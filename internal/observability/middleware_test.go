package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestRequestLogger_EmitsStructuredLine(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(Config{Env: "production", Level: slog.LevelInfo, Output: &buf})

	e := echo.New()
	e.Use(RequestIDMiddleware())
	e.Use(RequestLoggerMiddleware(logger))
	e.GET("/x", func(c echo.Context) error {
		l := LoggerFromContext(c)
		if l == nil {
			t.Fatal("LoggerFromContext returned nil")
		}
		l.Info("inside handler")
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected >=2 log lines, got %d: %q", len(lines), buf.String())
	}

	var inside, access map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &inside); err != nil {
		t.Fatalf("inside line not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &access); err != nil {
		t.Fatalf("access line not JSON: %v", err)
	}

	rid, ok := inside["request_id"].(string)
	if !ok || rid == "" {
		t.Errorf("inside log missing non-empty request_id: %v", inside["request_id"])
	}
	if access["request_id"] != rid {
		t.Errorf("access log request_id = %v, want %v", access["request_id"], rid)
	}
	if access["method"] != "GET" {
		t.Errorf("access log method = %v, want GET", access["method"])
	}
	if access["path"] != "/x" {
		t.Errorf("access log path = %v, want /x", access["path"])
	}
	if got, ok := access["status"].(float64); !ok || int(got) != 200 {
		t.Errorf("access log status = %v, want 200", access["status"])
	}
}

func TestRequestIDMiddleware_RespectsIncomingHeader(t *testing.T) {
	e := echo.New()
	e.Use(RequestIDMiddleware())
	e.GET("/x", func(c echo.Context) error {
		return c.String(http.StatusOK, RequestIDFromContext(c))
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-Id", "abc-123")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Body.String() != "abc-123" {
		t.Errorf("request id: got %q, want %q", rec.Body.String(), "abc-123")
	}
	if got := rec.Header().Get("X-Request-Id"); got != "abc-123" {
		t.Errorf("response X-Request-Id: got %q, want %q", got, "abc-123")
	}
}
