//go:build tools

// Package tools tracks build-time CLI dependencies so their versions are
// locked in go.mod. They are not imported by the application; the build
// tag ensures they are excluded from compiled binaries.
//
// To install or update these tools locally, run `make tools`.
package tools

import (
	_ "github.com/a-h/templ/cmd/templ"
	_ "github.com/swaggo/swag/cmd/swag"
)
