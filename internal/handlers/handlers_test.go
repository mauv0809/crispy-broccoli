package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
)

func TestHealth_NilPool_Returns503(t *testing.T) {
	e := echo.New()
	h := New(nil)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.Health(c); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	if body["status"] != "unhealthy" {
		t.Errorf(`body["status"] = %v, want "unhealthy"`, body["status"])
	}
	if _, ok := body["build_sha"]; !ok {
		t.Errorf("body missing build_sha")
	}
}

func TestHealth_ClosedPool_Returns503(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	pool.Close()

	e := echo.New()
	h := New(pool)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := h.Health(c); err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
