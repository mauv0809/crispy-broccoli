package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/mauv0809/crispy-broccoli/internal/users"
)

const contextKeyUser = "auth.user"

// Session abstracts the bits of scs the middleware actually needs, so
// tests don't have to construct a real scs.SessionManager.
type Session interface {
	UserID(c echo.Context) int64
}

// UserLoader fetches a user by ID. *users.Repository satisfies it.
type UserLoader interface {
	LoadUser(ctx context.Context, id int64) (*users.User, error)
}

// repoLoader adapts *users.Repository to UserLoader without changing its API.
type repoLoader struct{ R *users.Repository }

func (l repoLoader) LoadUser(ctx context.Context, id int64) (*users.User, error) {
	return l.R.GetByID(ctx, id)
}

// NewLoader is the production wiring for UserLoader.
func NewLoader(r *users.Repository) UserLoader { return repoLoader{R: r} }

// sessionLike captures only the scs.SessionManager methods the adapter needs.
type sessionLike interface {
	GetInt64(ctx context.Context, key string) int64
}

// scsSession adapts *scs.SessionManager to the Session interface.
type scsSession struct{ M sessionLike }

func (s scsSession) UserID(c echo.Context) int64 {
	return s.M.GetInt64(c.Request().Context(), sessionUserIDKey)
}

// NewSession is the production wiring for Session. Pass *scs.SessionManager.
func NewSession(m sessionLike) Session { return scsSession{M: m} }

// UserFromContext returns the *users.User attached by RequireAuth, or nil.
func UserFromContext(c echo.Context) *users.User {
	if v, ok := c.Get(contextKeyUser).(*users.User); ok {
		return v
	}
	return nil
}

// SetUserOnContext attaches a user — used by RequireAuth and by tests.
func SetUserOnContext(c echo.Context, u *users.User) {
	c.Set(contextKeyUser, u)
}

// RequireAuth ensures a session-bound, active user exists. HTML requests
// without a session redirect to the login route; HTMX/JSON requests get a
// 401 instead so the client can handle it without a page reload.
func RequireAuth(sess Session, loader UserLoader) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			id := sess.UserID(c)
			if id == 0 {
				return rejectUnauthenticated(c)
			}
			u, err := loader.LoadUser(c.Request().Context(), id)
			if errors.Is(err, users.ErrNotFound) {
				return rejectUnauthenticated(c)
			}
			if err != nil {
				slog.Error("auth: load user failed", "user_id", id, "error", err)
				return err
			}
			if !u.IsActive {
				return rejectUnauthenticated(c)
			}
			SetUserOnContext(c, u)
			return next(c)
		}
	}
}

// RequireAdmin must run after RequireAuth. Returns 403 when the user is
// missing or non-admin.
func RequireAdmin() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			u := UserFromContext(c)
			if u == nil || !u.IsAdmin {
				return c.String(http.StatusForbidden, "forbidden")
			}
			return next(c)
		}
	}
}

func rejectUnauthenticated(c echo.Context) error {
	if isAPI(c) {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}
	return c.Redirect(http.StatusSeeOther, "/auth/google/login")
}

func isAPI(c echo.Context) bool {
	if c.Request().Header.Get("HX-Request") == "true" {
		return true
	}
	accept := c.Request().Header.Get("Accept")
	if accept == "" {
		return false
	}
	if strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html") {
		return true
	}
	if strings.HasPrefix(c.Request().URL.Path, "/api/") {
		return true
	}
	return false
}
