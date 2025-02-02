package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/pkg/errors"

	"github.com/yourselfhosted/slash/server/profile"
	"github.com/yourselfhosted/slash/server/version"
)

//go:embed migration
var migrationFS embed.FS

//go:embed seed
var seedFS embed.FS

type DB struct {
	// sqlite db connection instance
	DBInstance *sql.DB
	profile    *profile.Profile
}

// NewDB returns a new instance of DB associated with the given datasource name.
func NewDB(profile *profile.Profile) *DB {
	db := &DB{
		profile: profile,
	}
	return db
}

func (db *DB) Open(ctx context.Context) (err error) {
	// Ensure a DSN is set before attempting to open the database.
	if db.profile.DSN == "" {
		return errors.New("dsn required")
	}

	// Connect to the database with some sane settings:
	// - No shared-cache: it's obsolete; WAL journal mode is a better solution.
	// - No foreign key constraints: it's currently disabled by default, but it's a
	// good practice to be explicit and prevent future surprises on SQLite upgrades.
	// - Journal mode set to WAL: it's the recommended journal mode for most applications
	// as it prevents locking issues.
	//
	// Notes:
	// - When using the `modernc.org/sqlite` driver, each pragma must be prefixed with `_pragma=`.
	//
	// References:
	// - https://pkg.go.dev/modernc.org/sqlite#Driver.Open
	// - https://www.sqlite.org/sharedcache.html
	// - https://www.sqlite.org/pragma.html
	sqliteDB, err := sql.Open("sqlite", db.profile.DSN+"?_pragma=foreign_keys(0)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return errors.Wrapf(err, "failed to open db with dsn: %s", db.profile.DSN)
	}
	db.DBInstance = sqliteDB
	currentVersion := version.GetCurrentVersion(db.profile.Mode)

	if db.profile.Mode == "prod" {
		_, err := os.Stat(db.profile.DSN)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return errors.Wrap(err, "failed to get db file stat")
			}

			// If db file not exists, we should create a new one with latest schema.
			err := db.applyLatestSchema(ctx)
			if err != nil {
				return errors.Wrap(err, "failed to apply latest schema")
			}
			_, err = db.UpsertMigrationHistory(ctx, &MigrationHistoryUpsert{
				Version: currentVersion,
			})
			if err != nil {
				return errors.Wrap(err, "failed to upsert migration history")
			}
			return nil
		}

		// If db file exists, we should check if we need to migrate the database.
		migrationHistoryList, err := db.FindMigrationHistoryList(ctx, &MigrationHistoryFind{})
		if err != nil {
			return errors.Wrap(err, "failed to find migration history")
		}
		if len(migrationHistoryList) == 0 {
			_, err := db.UpsertMigrationHistory(ctx, &MigrationHistoryUpsert{
				Version: currentVersion,
			})
			if err != nil {
				return errors.Wrap(err, "failed to upsert migration history")
			}
			return nil
		}

		migrationHistoryVersionList := []string{}
		for _, migrationHistory := range migrationHistoryList {
			migrationHistoryVersionList = append(migrationHistoryVersionList, migrationHistory.Version)
		}
		sort.Sort(version.SortVersion(migrationHistoryVersionList))
		latestMigrationHistoryVersion := migrationHistoryVersionList[len(migrationHistoryVersionList)-1]

		if version.IsVersionGreaterThan(version.GetSchemaVersion(currentVersion), latestMigrationHistoryVersion) {
			minorVersionList := getMinorVersionList()

			// backup the raw database file before migration
			rawBytes, err := os.ReadFile(db.profile.DSN)
			if err != nil {
				return errors.Wrap(err, "failed to read raw database file")
			}
			backupDBFilePath := fmt.Sprintf("%s/slash_%s_%d_backup.db", db.profile.Data, db.profile.Version, time.Now().Unix())
			if err := os.WriteFile(backupDBFilePath, rawBytes, 0644); err != nil {
				return errors.Wrap(err, "failed to write raw database file")
			}
			slog.Log(ctx, slog.LevelInfo, "succeed to copy a backup database file")

			slog.Log(ctx, slog.LevelInfo, "start migrate")
			for _, minorVersion := range minorVersionList {
				normalizedVersion := minorVersion + ".0"
				if version.IsVersionGreaterThan(normalizedVersion, latestMigrationHistoryVersion) && version.IsVersionGreaterOrEqualThan(currentVersion, normalizedVersion) {
					slog.Log(ctx, slog.LevelInfo, fmt.Sprintf("applying migration for %s", normalizedVersion))
					if err := db.applyMigrationForMinorVersion(ctx, minorVersion); err != nil {
						return errors.Wrap(err, "failed to apply minor version migration")
					}
				}
			}
			slog.Log(ctx, slog.LevelInfo, "end migrate")

			// remove the created backup db file after migrate succeed
			if err := os.Remove(backupDBFilePath); err != nil {
				slog.Log(ctx, slog.LevelError, fmt.Sprintf("Failed to remove temp database file, err %v", err))
			}
		}
	} else {
		// In non-prod mode, we should always migrate the database.
		if _, err := os.Stat(db.profile.DSN); errors.Is(err, os.ErrNotExist) {
			if err := db.applyLatestSchema(ctx); err != nil {
				return errors.Wrap(err, "failed to apply latest schema")
			}
			// In demo mode, we should seed the database.
			if db.profile.Mode == "demo" {
				if err := db.seed(ctx); err != nil {
					return errors.Wrap(err, "failed to seed")
				}
			}
		}
	}

	return nil
}

const (
	latestSchemaFileName = "LATEST__SCHEMA.sql"
)

func (db *DB) applyLatestSchema(ctx context.Context) error {
	schemaMode := "dev"
	if db.profile.Mode == "prod" {
		schemaMode = "prod"
	}
	latestSchemaPath := fmt.Sprintf("migration/%s/%s", schemaMode, latestSchemaFileName)
	buf, err := migrationFS.ReadFile(latestSchemaPath)
	if err != nil {
		return errors.Wrapf(err, "failed to read latest schema %q", latestSchemaPath)
	}
	stmt := string(buf)
	if err := db.execute(ctx, stmt); err != nil {
		return errors.Wrapf(err, "migrate error: statement %s", stmt)
	}
	return nil
}

func (db *DB) applyMigrationForMinorVersion(ctx context.Context, minorVersion string) error {
	filenames, err := fs.Glob(migrationFS, fmt.Sprintf("migration/prod/%s/*.sql", minorVersion))
	if err != nil {
		return errors.Wrap(err, "failed to read migrate files")
	}

	sort.Strings(filenames)
	migrationStmt := ""

	// Loop over all migration files and execute them in order.
	for _, filename := range filenames {
		buf, err := migrationFS.ReadFile(filename)
		if err != nil {
			return errors.Wrapf(err, "failed to read minor version migration file, filename %s", filename)
		}
		stmt := string(buf)
		migrationStmt += stmt
		if err := db.execute(ctx, stmt); err != nil {
			return errors.Wrapf(err, "migrate error: statement %s", stmt)
		}
	}

	// Upsert the newest version to migration_history.
	version := minorVersion + ".0"
	if _, err = db.UpsertMigrationHistory(ctx, &MigrationHistoryUpsert{
		Version: version,
	}); err != nil {
		return errors.Wrapf(err, "failed to upsert migration history with version %s", version)
	}

	return nil
}

func (db *DB) seed(ctx context.Context) error {
	filenames, err := fs.Glob(seedFS, "seed/*.sql")
	if err != nil {
		return errors.Wrap(err, "failed to read seed files")
	}

	sort.Strings(filenames)
	// Loop over all seed files and execute them in order.
	for _, filename := range filenames {
		buf, err := seedFS.ReadFile(filename)
		if err != nil {
			return errors.Wrapf(err, "failed to read seed file, filename %s", filename)
		}
		stmt := string(buf)
		if err := db.execute(ctx, stmt); err != nil {
			return errors.Wrapf(err, "seed error: statement %s", stmt)
		}
	}
	return nil
}

// execute runs a single SQL statement within a transaction.
func (db *DB) execute(ctx context.Context, stmt string) error {
	if _, err := db.DBInstance.ExecContext(ctx, stmt); err != nil {
		return errors.Wrap(err, "failed to execute statement")
	}

	return nil
}

// minorDirRegexp is a regular expression for minor version directory.
var minorDirRegexp = regexp.MustCompile(`^migration/prod/[0-9]+\.[0-9]+$`)

func getMinorVersionList() []string {
	minorVersionList := []string{}

	if err := fs.WalkDir(migrationFS, "migration", func(path string, file fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if file.IsDir() && minorDirRegexp.MatchString(path) {
			minorVersionList = append(minorVersionList, file.Name())
		}

		return nil
	}); err != nil {
		panic(err)
	}

	sort.Sort(version.SortVersion(minorVersionList))

	return minorVersionList
}
