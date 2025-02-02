package store

import (
	"context"
	"errors"
	"strings"
)

// Role is the type of a role.
type Role string

const (
	// RoleAdmin is the ADMIN role.
	RoleAdmin Role = "ADMIN"
	// RoleUser is the USER role.
	RoleUser Role = "USER"
)

type User struct {
	ID int32

	// Standard fields
	CreatedTs int64
	UpdatedTs int64
	RowStatus RowStatus

	// Domain specific fields
	Email        string
	Nickname     string
	PasswordHash string
	Role         Role
}

type UpdateUser struct {
	ID int32

	RowStatus    *RowStatus
	Email        *string
	Nickname     *string
	PasswordHash *string
	Role         *Role
}

type FindUser struct {
	ID        *int32
	RowStatus *RowStatus
	Email     *string
	Nickname  *string
	Role      *Role
}

type DeleteUser struct {
	ID int32
}

func (s *Store) CreateUser(ctx context.Context, create *User) (*User, error) {
	stmt := `
		INSERT INTO user (
			email,
			nickname,
			password_hash,
			role
		)
		VALUES (?, ?, ?, ?)
		RETURNING id, created_ts, updated_ts, row_status
	`
	if err := s.db.QueryRowContext(ctx, stmt,
		create.Email,
		create.Nickname,
		create.PasswordHash,
		create.Role,
	).Scan(
		&create.ID,
		&create.CreatedTs,
		&create.UpdatedTs,
		&create.RowStatus,
	); err != nil {
		return nil, err
	}

	user := create
	s.userCache.Store(user.ID, user)
	return user, nil
}

func (s *Store) UpdateUser(ctx context.Context, update *UpdateUser) (*User, error) {
	set, args := []string{}, []any{}
	if v := update.RowStatus; v != nil {
		set, args = append(set, "row_status = ?"), append(args, *v)
	}
	if v := update.Email; v != nil {
		set, args = append(set, "email = ?"), append(args, *v)
	}
	if v := update.Nickname; v != nil {
		set, args = append(set, "nickname = ?"), append(args, *v)
	}
	if v := update.PasswordHash; v != nil {
		set, args = append(set, "password_hash = ?"), append(args, *v)
	}
	if v := update.Role; v != nil {
		set, args = append(set, "role = ?"), append(args, *v)
	}

	if len(set) == 0 {
		return nil, errors.New("no fields to update")
	}

	stmt := `
		UPDATE user
		SET ` + strings.Join(set, ", ") + `
		WHERE id = ?
		RETURNING id, created_ts, updated_ts, row_status, email, nickname, password_hash, role
	`
	args = append(args, update.ID)
	user := &User{}
	if err := s.db.QueryRowContext(ctx, stmt, args...).Scan(
		&user.ID,
		&user.CreatedTs,
		&user.UpdatedTs,
		&user.RowStatus,
		&user.Email,
		&user.Nickname,
		&user.PasswordHash,
		&user.Role,
	); err != nil {
		return nil, err
	}

	s.userCache.Store(user.ID, user)
	return user, nil
}

func (s *Store) ListUsers(ctx context.Context, find *FindUser) ([]*User, error) {
	where, args := []string{"1 = 1"}, []any{}

	if v := find.ID; v != nil {
		where, args = append(where, "id = ?"), append(args, *v)
	}
	if v := find.RowStatus; v != nil {
		where, args = append(where, "row_status = ?"), append(args, v.String())
	}
	if v := find.Email; v != nil {
		where, args = append(where, "email = ?"), append(args, *v)
	}
	if v := find.Nickname; v != nil {
		where, args = append(where, "nickname = ?"), append(args, *v)
	}
	if v := find.Role; v != nil {
		where, args = append(where, "role = ?"), append(args, *v)
	}

	query := `
		SELECT 
			id,
			created_ts,
			updated_ts,
			row_status,
			email,
			nickname,
			password_hash,
			role
		FROM user
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY updated_ts DESC, created_ts DESC
	`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]*User, 0)
	for rows.Next() {
		user := &User{}
		if err := rows.Scan(
			&user.ID,
			&user.CreatedTs,
			&user.UpdatedTs,
			&user.RowStatus,
			&user.Email,
			&user.Nickname,
			&user.PasswordHash,
			&user.Role,
		); err != nil {
			return nil, err
		}
		list = append(list, user)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, user := range list {
		s.userCache.Store(user.ID, user)
	}

	return list, nil
}

func (s *Store) GetUser(ctx context.Context, find *FindUser) (*User, error) {
	if find.ID != nil {
		if cache, ok := s.userCache.Load(*find.ID); ok {
			return cache.(*User), nil
		}
	}

	list, err := s.ListUsers(ctx, find)
	if err != nil {
		return nil, err
	}

	if len(list) == 0 {
		return nil, nil
	}

	return list[0], nil
}

func (s *Store) DeleteUser(ctx context.Context, delete *DeleteUser) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM user WHERE id = ?
	`, delete.ID); err != nil {
		return err
	}

	if err := vacuumUserSetting(ctx, tx); err != nil {
		return err
	}

	if err := vacuumShortcut(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	s.userCache.Delete(delete.ID)

	return nil
}
