package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/labstack/echo/v4"

	"github.com/mauv0809/crispy-broccoli/internal/email"
	"github.com/mauv0809/crispy-broccoli/internal/users"
	"github.com/mauv0809/crispy-broccoli/internal/views"
)

const (
	magicTokenLifetime = 15 * time.Minute
	magicRateWindow    = 15 * time.Minute
	magicRateMax       = 3 // requests per user per window
)

// userLookup is the slice of *users.Repository that MagicHandler needs.
// Defined as an interface so tests inject a stub without a real DB.
type userLookup interface {
	GetByEmail(ctx context.Context, email string) (*users.User, error)
	TouchLastLogin(ctx context.Context, id int64) error
}

// MagicHandler implements email-magic-link login. Coexists with Google
// OAuth — both produce the same scs session via PutUserID.
type MagicHandler struct {
	sm     *scs.SessionManager
	users  userLookup
	tokens MagicTokenStore
	sender email.Sender
	from   string
}

func NewMagicHandler(sm *scs.SessionManager, users userLookup, tokens MagicTokenStore, sender email.Sender, fromAddress string) *MagicHandler {
	return &MagicHandler{sm: sm, users: users, tokens: tokens, sender: sender, from: fromAddress}
}

// Mount registers the public auth routes. None require RequireAuth.
func (h *MagicHandler) Mount(e *echo.Echo, googleEnabled bool) {
	e.GET("/auth/login", h.LoginPage(googleEnabled))
	e.POST("/auth/magic/request", h.Request)
	e.GET("/auth/magic/verify", h.Verify)
}

// LoginPage returns a closure that captures whether Google OAuth is
// configured, so the template can hide the button when it isn't.
func (h *MagicHandler) LoginPage(googleEnabled bool) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
		return views.LoginPage(googleEnabled).Render(c.Request().Context(), c.Response())
	}
}

// Request accepts an email, looks up the user, and emails them a link
// if eligible. Always returns the same MagicSent page on the happy path
// to avoid email enumeration; only structurally-broken submissions
// (missing/invalid email) get a different response.
func (h *MagicHandler) Request(c echo.Context) error {
	email := strings.TrimSpace(c.FormValue("email"))
	if email == "" {
		return c.Redirect(http.StatusSeeOther, "/auth/login")
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return c.Redirect(http.StatusSeeOther, "/auth/login")
	}

	// Always render the same response after this point — the user must not
	// be able to tell, from the response, whether the address is registered.
	defer func() {
		// no-op; the renderInbox call below is the actual return path
	}()

	go h.tryDeliver(linkBase(c), email) //nolint:contextcheck // background send: detach from request ctx so the response isn't blocked

	return renderInbox(c)
}

// tryDeliver runs in its own goroutine — keeps the user-facing response
// fast and constant-time regardless of whether a real send happens.
// Errors are logged and swallowed; the user sees the inbox page either way.
func (h *MagicHandler) tryDeliver(linkBase, addr string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	u, err := h.users.GetByEmail(ctx, addr)
	if errors.Is(err, users.ErrNotFound) {
		// silent — enumeration mitigation
		return
	}
	if err != nil {
		slog.Error("magic: lookup failed", "error", err)
		return
	}
	if !u.IsActive {
		return
	}

	count, err := h.tokens.RecentCount(ctx, u.ID, time.Now().Add(-magicRateWindow))
	if err != nil {
		slog.Error("magic: rate-limit check failed", "user_id", u.ID, "error", err)
		return
	}
	if count >= magicRateMax {
		slog.Warn("magic: rate limit hit", "user_id", u.ID, "count", count)
		return
	}

	raw, hash, err := generateMagicToken()
	if err != nil {
		slog.Error("magic: token gen failed", "error", err)
		return
	}
	if err := h.tokens.Insert(ctx, hash, u.ID, time.Now().Add(magicTokenLifetime)); err != nil {
		slog.Error("magic: token insert failed", "user_id", u.ID, "error", err)
		return
	}

	link := linkBase + "/auth/magic/verify?token=" + raw
	msg := buildMagicEmail(u.Email, link)
	if err := h.sender.Send(ctx, msg); err != nil {
		slog.Error("magic: send failed", "user_id", u.ID, "error", err)
		return
	}
}

// Verify consumes a token, creates a session, and redirects home.
// Returns the MagicError page on any failure so the user sees a
// consistent surface — we don't want to leak which check tripped.
func (h *MagicHandler) Verify(c echo.Context) error {
	raw := c.QueryParam("token")
	if raw == "" {
		return renderMagicError(c, "This link is missing its token.")
	}

	ctx := c.Request().Context()
	userID, err := h.tokens.Consume(ctx, hashToken(raw))
	if errors.Is(err, ErrTokenInvalid) {
		return renderMagicError(c, "This link has expired or already been used.")
	}
	if err != nil {
		slog.Error("magic: consume failed", "error", err)
		return err
	}

	if err := h.sm.RenewToken(ctx); err != nil {
		slog.Error("magic: session renew failed", "error", err)
		return err
	}
	PutUserID(h.sm, c, userID)

	if err := h.users.TouchLastLogin(ctx, userID); err != nil {
		// Non-fatal — same trade-off as the OAuth path.
		slog.Warn("magic: touch last_login_at failed", "user_id", userID, "error", err)
	}

	return c.Redirect(http.StatusSeeOther, "/")
}

// linkBase derives the absolute URL prefix for magic links from the
// incoming request (X-Forwarded-Proto + Host). Works in prod and in any
// Coolify preview deploy without per-environment BASE_URL config.
//
// Safe even though Host is client-controllable: nginx only proxies
// requests whose Host matches a configured server_name, so the app
// only ever sees real prod or preview hostnames.
func linkBase(c echo.Context) string {
	proto := c.Request().Header.Get("X-Forwarded-Proto")
	if proto == "" {
		if c.Request().TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	return proto + "://" + c.Request().Host
}

func buildMagicEmail(to, link string) email.Message {
	text := fmt.Sprintf(`Sign in to DeepValue:

%s

This link expires in 15 minutes and can only be used once.
If you did not request it, you can safely ignore this email.
`, link)

	html := fmt.Sprintf(`<!DOCTYPE html>
<html><body style="font-family:-apple-system,Segoe UI,Roboto,sans-serif;color:#111;line-height:1.5">
<p>Click the link below to sign in to DeepValue:</p>
<p><a href="%s" style="background:#3b82f6;color:#fff;padding:10px 16px;border-radius:6px;text-decoration:none">Sign in</a></p>
<p style="color:#555;font-size:13px">Or copy this URL: %s</p>
<p style="color:#888;font-size:12px">This link expires in 15 minutes and can only be used once. If you did not request it, ignore this email.</p>
</body></html>
`, link, link)

	return email.Message{
		To:       to,
		Subject:  "Sign in to DeepValue",
		HTMLBody: html,
		TextBody: text,
	}
}

func renderInbox(c echo.Context) error {
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	c.Response().WriteHeader(http.StatusOK)
	return views.MagicSent().Render(c.Request().Context(), c.Response())
}

func renderMagicError(c echo.Context, reason string) error {
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	c.Response().WriteHeader(http.StatusBadRequest)
	return views.MagicError(reason).Render(c.Request().Context(), c.Response())
}
