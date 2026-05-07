package observability

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"", slog.LevelInfo},
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"garbage", slog.LevelInfo},
	}
	for _, tc := range cases {
		if got := ParseLevel(tc.in); got != tc.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNewLogger_ProductionEmitsJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(Config{Env: "production", Level: slog.LevelInfo, Output: &buf})
	logger.Info("hello", "k", "v")

	line := strings.TrimSpace(buf.String())
	var fields map[string]any
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		t.Fatalf("expected JSON output, got %q: %v", line, err)
	}
	if fields["msg"] != "hello" {
		t.Errorf(`msg = %v, want "hello"`, fields["msg"])
	}
	if fields["k"] != "v" {
		t.Errorf(`k = %v, want "v"`, fields["k"])
	}
}

func TestNewLogger_DevelopmentEmitsText(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(Config{Env: "development", Level: slog.LevelInfo, Output: &buf})
	logger.Info("hello", "k", "v")

	out := buf.String()
	if !strings.Contains(out, "msg=hello") || !strings.Contains(out, "k=v") {
		t.Errorf("expected key=value text format, got %q", out)
	}
}
