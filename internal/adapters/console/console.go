// Package console is the inbound adapter that serves the web console: the
// files Vite built from web/, embedded in the binary. It serves files and
// nothing else - the console talks to the management API like any client.
package console

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// contentSecurityPolicy lets the console load its own scripts, styles and
// images and call its own origin, and nothing else: no inline script, no
// other host.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; font-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

const stubPage = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>CactusFS</title>
  </head>
  <body>
    <h1>CactusFS</h1>
    <p>The console was not built into this binary.</p>
    <p>Build with <code>make build</code>, or with <code>docker build .</code>, to include it.
       The management API is documented at <a href="/docs">/docs</a>.</p>
  </body>
</html>
`

// NewHandler serves the console from dist. With built false it serves a page
// that says the console was not built.
func NewHandler(dist fs.FS, built bool) http.Handler {
	var files http.Handler
	if built {
		files = http.FileServerFS(dist)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			header.Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !built {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			header.Set("Content-Type", "text/html; charset=utf-8")
			header.Set("Cache-Control", "no-cache")
			_, _ = w.Write([]byte(stubPage))
			return
		}
		// Vite names the files under assets/ after their content: they
		// never change. Everything else is looked at again every time.
		if strings.HasPrefix(path.Clean(r.URL.Path), "/assets/") {
			header.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			header.Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
