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
// other host. Images may also come from blob: URLs, which the console makes
// of the previews it fetched itself, and workers - the readers of PDF and
// Office documents - from its own origin only (ADR-0028). No eval, no inline
// code, no WebAssembly.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; " +
	"connect-src 'self'; font-src 'self'; worker-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// contentTypeOf is the type of a file the console's build emits, by its
// extension, or "" for another: set by the handler, not taken from the
// host's table of types, which differs between systems (on Windows the
// registry may call a script text/plain, and with nosniff a browser then
// refuses to run it).
func contentTypeOf(ext string) string {
	switch ext {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	default:
		return ""
	}
}

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
		clean := path.Clean(r.URL.Path)
		if strings.HasPrefix(clean, "/assets/") {
			header.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			header.Set("Cache-Control", "no-cache")
		}
		// The page itself is index.html, served for "/".
		ext := path.Ext(clean)
		if strings.HasSuffix(clean, "/") {
			ext = ".html"
		}
		if contentType := contentTypeOf(ext); contentType != "" {
			header.Set("Content-Type", contentType)
		}
		files.ServeHTTP(w, r)
	})
}
