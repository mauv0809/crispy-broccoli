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
