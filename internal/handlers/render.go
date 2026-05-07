package handlers

import (
	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/mauv0809/crispy-broccoli/internal/views"
)

func Render(c echo.Context, statusCode int, t templ.Component) error {
	c.Response().Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	c.Response().WriteHeader(statusCode)

	ctx := c.Request().Context()
	if tok, ok := c.Get("csrf").(string); ok {
		ctx = views.WithCSRFToken(ctx, tok)
	}
	return t.Render(ctx, c.Response())
}
