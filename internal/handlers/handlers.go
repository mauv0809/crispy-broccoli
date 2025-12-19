package handlers

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/mauv0809/crispy-broccoli/internal/views"
)

type Handler struct {
	// Add dependencies here (e.g., db pool, services)
}

func New() *Handler {
	return &Handler{}
}

func (h *Handler) Health(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{
		"status": "ok",
	})
}

func (h *Handler) Index(c echo.Context) error {
	return Render(c, http.StatusOK, views.Index())
}
