package views

import "context"

type csrfKey struct{}

// WithCSRFToken stashes the token on a context so templ templates can read it.
func WithCSRFToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, csrfKey{}, token)
}

// CSRFFromContext reads a token previously stashed by WithCSRFToken.
// Returns empty string when none is present.
func CSRFFromContext(ctx context.Context) string {
	if s, ok := ctx.Value(csrfKey{}).(string); ok {
		return s
	}
	return ""
}
