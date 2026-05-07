package auth

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/alexedwards/scs/postgresstore"
	"github.com/alexedwards/scs/v2"
	"github.com/labstack/echo/v4"
)

const sessionUserIDKey = "user_id"

// NewSessionManager returns an scs manager backed by a Postgres store.
// The schema (table `sessions`) is created by migration 014.
//
// db must be a *database/sql connection pool — scs's postgresstore wants
// the stdlib sql interface even though the rest of the app uses pgx.
// Open it via sql.Open("pgx", databaseURL).
func NewSessionManager(db *sql.DB) *scs.SessionManager {
	sm := scs.New()
	sm.Store = postgresstore.New(db)
	sm.Lifetime = 30 * 24 * time.Hour
	sm.Cookie.Name = "deepvalue_session"
	sm.Cookie.HttpOnly = true
	sm.Cookie.SameSite = http.SameSiteLaxMode
	sm.Cookie.Persist = true
	sm.Cookie.Secure = true // overridden by main when ENV != "production"
	return sm
}

// SessionMiddleware adapts scs's standard net/http middleware to Echo's
// pipeline so any handler downstream can read/write session data via
// sm.Get/Put on the request context.
func SessionMiddleware(sm *scs.SessionManager) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			var handlerErr error
			h := sm.LoadAndSave(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c.SetRequest(r)
				c.Response().Writer = w
				handlerErr = next(c)
			}))
			h.ServeHTTP(c.Response().Writer, c.Request())
			return handlerErr
		}
	}
}

// PutUserID writes the authenticated user's ID into the session.
func PutUserID(sm *scs.SessionManager, c echo.Context, id int64) {
	sm.Put(c.Request().Context(), sessionUserIDKey, id)
}

// UserID reads the authenticated user's ID from the session, or 0 if absent.
func UserID(sm *scs.SessionManager, c echo.Context) int64 {
	return sm.GetInt64(c.Request().Context(), sessionUserIDKey)
}

// Destroy clears the session — call on logout.
func Destroy(sm *scs.SessionManager, c echo.Context) error {
	return sm.Destroy(c.Request().Context())
}
