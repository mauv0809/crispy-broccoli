package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	"github.com/mauv0809/crispy-broccoli/internal/views"
)

type Handler struct {
	pool *pgxpool.Pool
}

// New constructs a Handler. pool may be nil — the Health handler
// reports unhealthy if the pool is nil or the ping fails.
func New(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

// BuildSHA and BuildTime are populated from main at startup.
// They mirror the ldflags-injected values; main calls SetBuildInfo.
var (
	BuildSHA  = "dev"
	BuildTime = "unknown"
)

// SetBuildInfo lets cmd/app/main wire build metadata into the handlers package.
func SetBuildInfo(sha, ts string) {
	BuildSHA = sha
	BuildTime = ts
}

// Health returns 200 when the database is reachable, 503 otherwise.
// @Summary Health check
// @Description Returns the health status of the application
// @Tags system
// @Produce json
// @Success 200 {object} map[string]any
// @Failure 503 {object} map[string]any
// @Router /health [get]
func (h *Handler) Health(c echo.Context) error {
	resp := map[string]any{
		"build_sha":  BuildSHA,
		"build_time": BuildTime,
	}
	if h.pool == nil {
		resp["status"] = "unhealthy"
		resp["error"] = "database pool not initialized"
		return c.JSON(http.StatusServiceUnavailable, resp)
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), 1*time.Second)
	defer cancel()
	if err := h.pool.Ping(ctx); err != nil {
		resp["status"] = "unhealthy"
		resp["error"] = "database unreachable"
		return c.JSON(http.StatusServiceUnavailable, resp)
	}
	resp["status"] = "ok"
	return c.JSON(http.StatusOK, resp)
}

func (h *Handler) Index(c echo.Context) error {
	return Render(c, http.StatusOK, views.Index())
}

func (h *Handler) Docs(c echo.Context) error {
	return Render(c, http.StatusOK, views.Docs())
}
