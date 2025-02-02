package v1

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"

	storepb "github.com/yourselfhosted/slash/proto/gen/store"
	"github.com/yourselfhosted/slash/server/metric"
	"github.com/yourselfhosted/slash/store"
)

func (s *APIV1Service) registerRedirectorRoutes(g *echo.Group) {
	g.GET("/*", func(c echo.Context) error {
		ctx := c.Request().Context()
		if len(c.ParamValues()) == 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid shortcut name")
		}

		shortcutName := c.ParamValues()[0]
		shortcut, err := s.Store.GetShortcut(ctx, &store.FindShortcut{
			Name: &shortcutName,
		})
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to get shortcut, err: %s", err)).SetInternal(err)
		}
		if shortcut == nil {
			return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/404?shortcut=%s", shortcutName))
		}
		if shortcut.Visibility != storepb.Visibility_PUBLIC {
			userID, ok := c.Get(userIDContextKey).(int32)
			if !ok {
				return echo.NewHTTPError(http.StatusUnauthorized, "Unauthorized")
			}
			if shortcut.Visibility == storepb.Visibility_PRIVATE && shortcut.CreatorId != userID {
				return echo.NewHTTPError(http.StatusUnauthorized, "Unauthorized")
			}
		}

		if err := s.createShortcutViewActivity(c, shortcut); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to create activity, err: %s", err)).SetInternal(err)
		}

		metric.Enqueue("shortcut redirect")
		return redirectToShortcut(c, shortcut)
	})
}

func redirectToShortcut(c echo.Context, shortcut *storepb.Shortcut) error {
	isValidURL := isValidURLString(shortcut.Link)
	if shortcut.OgMetadata == nil || (shortcut.OgMetadata.Title == "" && shortcut.OgMetadata.Description == "" && shortcut.OgMetadata.Image == "") {
		if isValidURL {
			return c.Redirect(http.StatusSeeOther, shortcut.Link)
		}
		return c.String(http.StatusOK, shortcut.Link)
	}

	htmlTemplate := `<html><head>%s</head><body>%s</body></html>`
	metadataList := []string{
		fmt.Sprintf(`<title>%s</title>`, shortcut.OgMetadata.Title),
		fmt.Sprintf(`<meta name="description" content="%s" />`, shortcut.OgMetadata.Description),
		fmt.Sprintf(`<meta property="og:title" content="%s" />`, shortcut.OgMetadata.Title),
		fmt.Sprintf(`<meta property="og:description" content="%s" />`, shortcut.OgMetadata.Description),
		fmt.Sprintf(`<meta property="og:image" content="%s" />`, shortcut.OgMetadata.Image),
		`<meta property="og:type" content="website" />`,
		// Twitter related metadata.
		fmt.Sprintf(`<meta name="twitter:title" content="%s" />`, shortcut.OgMetadata.Title),
		fmt.Sprintf(`<meta name="twitter:description" content="%s" />`, shortcut.OgMetadata.Description),
		fmt.Sprintf(`<meta name="twitter:image" content="%s" />`, shortcut.OgMetadata.Image),
		`<meta name="twitter:card" content="summary_large_image" />`,
	}
	if isValidURL {
		metadataList = append(metadataList, fmt.Sprintf(`<meta property="og:url" content="%s" />`, shortcut.Link))
	}
	body := ""
	if isValidURL {
		body = fmt.Sprintf(`<script>window.location.href = "%s";</script>`, shortcut.Link)
	} else {
		body = html.EscapeString(shortcut.Link)
	}
	htmlString := fmt.Sprintf(htmlTemplate, strings.Join(metadataList, ""), body)
	return c.HTML(http.StatusOK, htmlString)
}

func (s *APIV1Service) createShortcutViewActivity(c echo.Context, shortcut *storepb.Shortcut) error {
	payload := &ActivityShorcutViewPayload{
		ShortcutID: shortcut.Id,
		IP:         c.RealIP(),
		Referer:    c.Request().Referer(),
		UserAgent:  c.Request().UserAgent(),
	}
	payloadStr, err := json.Marshal(payload)
	if err != nil {
		return errors.Wrap(err, "Failed to marshal activity payload")
	}
	activity := &store.Activity{
		CreatorID: BotID,
		Type:      store.ActivityShortcutView,
		Level:     store.ActivityInfo,
		Payload:   string(payloadStr),
	}
	_, err = s.Store.CreateActivity(c.Request().Context(), activity)
	if err != nil {
		return errors.Wrap(err, "Failed to create activity")
	}
	return nil
}

func isValidURLString(s string) bool {
	_, err := url.ParseRequestURI(s)
	return err == nil
}
