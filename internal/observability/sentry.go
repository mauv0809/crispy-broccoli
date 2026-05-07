package observability

import (
	"context"
	"log/slog"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/labstack/echo/v4"
)

// InitSentry initializes the global Sentry client when dsn is non-empty.
// Returns a cleanup function that should be deferred from main; the
// cleanup is a no-op when Sentry was not initialized.
func InitSentry(dsn, env, release string) (cleanup func(), enabled bool) {
	if dsn == "" {
		return func() {}, false
	}
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Environment:      env,
		Release:          release,
		TracesSampleRate: 0,
	})
	if err != nil {
		slog.Error("sentry init failed; continuing without error tracking", "error", err)
		return func() {}, false
	}
	slog.Info("sentry initialized", "environment", env, "release", release)
	return func() {
		sentry.Flush(2 * time.Second)
	}, true
}

// SentryErrorMiddleware reports handler errors to Sentry when it is
// enabled. Safe to add unconditionally — when Sentry is not initialized,
// CaptureException is a no-op.
//
// Must run before any other middleware that swallows errors.
func SentryErrorMiddleware(enabled bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			if err == nil || !enabled {
				return err
			}
			hub := sentry.CurrentHub().Clone()
			hub.Scope().SetTag("request_id", RequestIDFromContext(c))
			hub.Scope().SetTag("path", c.Request().URL.Path)
			hub.Scope().SetTag("method", c.Request().Method)
			hub.CaptureException(err)
			return err
		}
	}
}

// CaptureContextError reports an error from background code that is
// not in an HTTP handler (e.g. a long-running job). No-op when Sentry
// is not initialized.
func CaptureContextError(_ context.Context, err error) {
	if err == nil {
		return
	}
	sentry.CaptureException(err)
}
