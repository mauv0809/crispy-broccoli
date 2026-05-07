package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/alexedwards/scs/v2"
	"github.com/gorilla/sessions"
	"github.com/labstack/echo/v4"
	"github.com/markbates/goth"
	"github.com/markbates/goth/gothic"
	"github.com/markbates/goth/providers/google"

	"github.com/mauv0809/crispy-broccoli/internal/users"
)

// GoogleConfig wires the goth provider. Caller (main) reads env.
type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	BaseURL      string // e.g. https://deepvalue.example.com or http://localhost:8080
}

// RegisterGoogle installs the goth Google provider. Must be called once
// at startup before any login attempt. The redirect URI is derived from
// BaseURL: <BaseURL>/auth/google/callback.
func RegisterGoogle(cfg GoogleConfig) error {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return errors.New("google oauth: client id and secret required")
	}
	if cfg.BaseURL == "" {
		return errors.New("google oauth: BASE_URL required")
	}
	goth.UseProviders(google.New(
		cfg.ClientID,
		cfg.ClientSecret,
		fmt.Sprintf("%s/auth/google/callback", cfg.BaseURL),
		"email", "profile",
	))
	return nil
}

// NewGothicStore returns a gorilla CookieStore suitable for gothic's
// short-lived OAuth state cookie. Caller assigns it to gothic.Store.
func NewGothicStore(sessionKey []byte, secureCookie bool) *sessions.CookieStore {
	store := sessions.NewCookieStore(sessionKey)
	store.Options.HttpOnly = true
	store.Options.Secure = secureCookie
	store.Options.SameSite = http.SameSiteLaxMode
	store.Options.MaxAge = 60 * 10 // OAuth state cookie — 10 min is plenty.
	store.Options.Path = "/"
	return store
}

// GoogleHandler bundles the three OAuth endpoints.
type GoogleHandler struct {
	sm    *scs.SessionManager
	users *users.Repository
}

func NewGoogleHandler(sm *scs.SessionManager, repo *users.Repository) *GoogleHandler {
	return &GoogleHandler{sm: sm, users: repo}
}

// Login initiates the OAuth dance. goth/gothic stashes the OAuth state
// in a short-lived cookie and redirects to Google's consent screen.
func (h *GoogleHandler) Login(c echo.Context) error {
	// gothic reads `provider` from the request query string.
	q := c.Request().URL.Query()
	q.Set("provider", "google")
	c.Request().URL.RawQuery = q.Encode()

	// If the user is already authenticated upstream, just redirect home.
	if u, _ := gothic.CompleteUserAuth(c.Response().Writer, c.Request()); u.Email != "" {
		return c.Redirect(http.StatusSeeOther, "/")
	}
	gothic.BeginAuthHandler(c.Response().Writer, c.Request())
	return nil
}

// Callback finalizes the OAuth dance.
func (h *GoogleHandler) Callback(c echo.Context) error {
	q := c.Request().URL.Query()
	q.Set("provider", "google")
	c.Request().URL.RawQuery = q.Encode()

	gothUser, err := gothic.CompleteUserAuth(c.Response().Writer, c.Request())
	if err != nil {
		slog.Warn("oauth callback failed", "error", err)
		return c.String(http.StatusBadRequest, "OAuth callback failed.")
	}

	ctx := c.Request().Context()
	u, err := h.users.EnsureIdentity(ctx, "google", gothUser.UserID, gothUser.Email)
	if errors.Is(err, users.ErrNotFound) {
		return c.String(http.StatusForbidden,
			"This Google account is not authorized. Contact the administrator.")
	}
	if err != nil {
		slog.Error("ensure identity failed", "error", err)
		return err
	}
	if !u.IsActive {
		return c.String(http.StatusForbidden,
			"Your account is disabled. Contact the administrator.")
	}

	if err := h.users.TouchLastLogin(ctx, u.ID); err != nil {
		// Non-fatal — log and continue.
		slog.Warn("touch last_login_at failed", "user_id", u.ID, "error", err)
	}

	if err := h.sm.RenewToken(ctx); err != nil {
		slog.Error("session renew failed", "error", err)
		return err
	}
	PutUserID(h.sm, c, u.ID)

	return c.Redirect(http.StatusSeeOther, "/")
}

// Logout clears the session and redirects home.
func (h *GoogleHandler) Logout(c echo.Context) error {
	if err := h.sm.Destroy(c.Request().Context()); err != nil {
		slog.Error("session destroy failed", "error", err)
		return err
	}
	return c.Redirect(http.StatusSeeOther, "/")
}

// Mount registers the auth routes onto the Echo instance. These must NOT
// be wrapped in RequireAuth.
func (h *GoogleHandler) Mount(e *echo.Echo) {
	e.GET("/auth/google/login", h.Login)
	e.GET("/auth/google/callback", h.Callback)
	e.POST("/auth/logout", h.Logout)
}
