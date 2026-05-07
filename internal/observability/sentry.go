package observability

import (
	"context"
	"errors"
	"fmt"
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

// SentryErrorMiddleware reports server failures to Sentry when it is
// enabled. Safe to add unconditionally — when Sentry is not initialized,
// the capture calls are no-ops.
//
// It captures in two cases:
//
//  1. The handler returns a non-nil error (typical Echo idiom; covers
//     panics surfaced through middleware.Recover and Echo's default 4xx
//     responses for unmatched routes).
//
//  2. The response status is >= 500 even though the handler returned nil.
//     This happens often in this codebase: handlers log the underlying
//     error via slog and then write c.JSON(500, ...) and return nil. Without
//     this fallback, those failures would never reach Sentry.
//
// Case 2 captures a synthetic "handler returned 5xx with no error" event
// tagged with method/path/request_id. The actual error message is in the
// preceding slog log line in Coolify, correlatable by request_id. For
// richer Sentry context (real exception type, stack trace, grouping),
// handlers should call CaptureHandlerError directly with the underlying
// error before writing the JSON response.
func SentryErrorMiddleware(enabled bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			if !enabled {
				return err
			}
			if err != nil {
				captureWithRequestTags(c, err)
				return err
			}
			if status := c.Response().Status; status >= 500 {
				captureWithRequestTags(c, fmt.Errorf("handler returned %d with no error", status))
			}
			return err
		}
	}
}

// CaptureHandlerError reports err to Sentry with the request's metadata
// as tags. Handlers that turn errors into JSON responses (the
// `slog.Error(...); return c.JSON(500, ...)` pattern) should call this
// before returning so Sentry sees the real exception instead of the
// middleware's synthetic fallback. Safe to call unconditionally — no-op
// when Sentry is not initialized or err is nil.
func CaptureHandlerError(c echo.Context, err error) {
	if err == nil {
		return
	}
	captureWithRequestTags(c, err)
}

// CaptureContextError reports an error from background code that is
// not in an HTTP handler (e.g. a long-running job). No-op when Sentry
// is not initialized or err is nil.
func CaptureContextError(_ context.Context, err error) {
	if err == nil {
		return
	}
	sentry.CaptureException(err)
}

func captureWithRequestTags(c echo.Context, err error) {
	if err == nil {
		err = errors.New("unspecified error")
	}
	hub := sentry.CurrentHub().Clone()
	hub.Scope().SetTag("request_id", RequestIDFromContext(c))
	hub.Scope().SetTag("path", c.Request().URL.Path)
	hub.Scope().SetTag("method", c.Request().Method)
	hub.CaptureException(err)
}
