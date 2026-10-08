//go:build !console

// Package web holds the console: the React application under src/, built by
// Vite into dist/. With the build tag "console" the built files are part of
// the binary; without it the binary knows that it has no console.
package web

import "io/fs"

// Dist reports that this binary was built without the console.
func Dist() (fs.FS, bool) {
	return nil, false
}
