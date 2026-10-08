//go:build console

// Package web holds the console: the React application under src/, built by
// Vite into dist/. With the build tag "console" the built files are part of
// the binary; without it the binary knows that it has no console.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built console, and true.
func Dist() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, false
	}
	return sub, true
}
