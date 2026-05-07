package observability

import (
	"log/slog"
	"time"

	"github.com/labstack/echo/v4"
)

// RequestIDMiddleware reads the X-Request-Id incoming header (if present)
// or generates a fresh one, stashes it on the echo.Context, and echoes
// it back on the response. Other middlewares and handlers can retrieve
// it via RequestIDFromContext.
func RequestIDMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			rid := c.Request().Header.Get(headerRequestID)
			if rid == "" {
				rid = newRequestID()
			}
			c.Set(contextKeyRequestID, rid)
			c.Response().Header().Set(headerRequestID, rid)
			return next(c)
		}
	}
}

// RequestLoggerMiddleware attaches a request-scoped *slog.Logger
// (already enriched with request_id) to echo.Context and emits an
// access log line after the handler returns. Must run after
// RequestIDMiddleware so the request ID is available.
func RequestLoggerMiddleware(base *slog.Logger) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			rid := RequestIDFromContext(c)
			scoped := base.With("request_id", rid)
			c.Set(contextKeyLogger, scoped)

			start := time.Now()
			err := next(c)
			latencyMs := time.Since(start).Milliseconds()

			status := c.Response().Status

			fields := []any{
				"method", c.Request().Method,
				"path", c.Request().URL.Path,
				"status", status,
				"latency_ms", latencyMs,
				"request_id", rid,
			}
			if err != nil {
				fields = append(fields, "error", err.Error())
				base.Error("http_request", fields...)
			} else if status >= 500 {
				base.Error("http_request", fields...)
			} else if status >= 400 {
				base.Warn("http_request", fields...)
			} else {
				base.Info("http_request", fields...)
			}
			return err
		}
	}
}
