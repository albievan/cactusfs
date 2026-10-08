// Package swaggerui serves the interactive documentation of the management
// API from assets that are part of the binary. Nothing is fetched from a CDN
// or any other host, and the Content-Security-Policy of every answer makes a
// browser refuse to: the page works on a network that reaches nothing else.
package swaggerui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed index.html initializer.js dist/swagger-ui-bundle.js dist/swagger-ui.css dist/favicon-32x32.png
var assets embed.FS

// Path is where the documentation is served.
const Path = "/docs"

// contentSecurityPolicy allows the page its own scripts, styles and images
// and calls to this origin, and nothing else. Swagger UI draws its icons from
// data: URIs in its stylesheet.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; font-src 'self'; form-action 'none'; base-uri 'none'; frame-ancestors 'none'"

// Register adds the documentation to a mux, at Path.
func Register(mux *http.ServeMux) {
	files := map[string]string{
		"":                      "index.html",
		"/{$}":                  "index.html",
		"/initializer.js":       "initializer.js",
		"/swagger-ui-bundle.js": "dist/swagger-ui-bundle.js",
		"/swagger-ui.css":       "dist/swagger-ui.css",
		"/favicon-32x32.png":    "dist/favicon-32x32.png",
	}
	types := map[string]string{
		".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8",
		".css": "text/css; charset=utf-8", ".png": "image/png",
	}
	for suffix, name := range files {
		mux.HandleFunc("GET "+Path+suffix, func(w http.ResponseWriter, _ *http.Request) {
			data, err := fs.ReadFile(assets, name)
			if err != nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			header := w.Header()
			header.Set("Content-Type", types[name[strings.LastIndex(name, "."):]])
			header.Set("Content-Security-Policy", contentSecurityPolicy)
			header.Set("X-Content-Type-Options", "nosniff")
			header.Set("X-Frame-Options", "DENY")
			header.Set("Referrer-Policy", "no-referrer")
			header.Set("Cache-Control", "no-cache")
			_, _ = w.Write(data)
		})
	}
}
