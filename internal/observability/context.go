package observability

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"

	"github.com/labstack/echo/v4"
)

const (
	contextKeyLogger    = "obs.logger"
	contextKeyRequestID = "obs.request_id"
	headerRequestID     = "X-Request-Id"
)

// LoggerFromContext returns the request-scoped logger placed by
// RequestLoggerMiddleware. Returns nil if none was set.
func LoggerFromContext(c echo.Context) *slog.Logger {
	v := c.Get(contextKeyLogger)
	if l, ok := v.(*slog.Logger); ok {
		return l
	}
	return nil
}

// RequestIDFromContext returns the request ID set by RequestIDMiddleware.
// Empty string if none was set.
func RequestIDFromContext(c echo.Context) string {
	v := c.Get(contextKeyRequestID)
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
