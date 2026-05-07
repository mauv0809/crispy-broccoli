package handlers

import (
	"net/http"

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

// Health returns application health status
// @Summary Health check
// @Description Returns the health status of the application
// @Tags system
// @Produce json
// @Success 200 {object} map[string]string
// @Router /health [get]
func (h *Handler) Health(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{
		"status": "ok",
	})
}

func (h *Handler) Index(c echo.Context) error {
	return Render(c, http.StatusOK, views.Index())
}

func (h *Handler) Docs(c echo.Context) error {
	return Render(c, http.StatusOK, views.Docs())
}
