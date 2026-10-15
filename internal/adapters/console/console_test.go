package console_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/albievan/cactusfs/internal/adapters/console"
)

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, nil))
	return rec
}

// consolePolicy is the console's Content Security Policy, word for word:
// its own scripts, styles, fonts and calls; images of its own, data: and the
// blob: URLs of the previews it fetched; workers of its own (ADR-0028). No
// eval, no inline code, no WebAssembly, no other host, no frame.
const consolePolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; " +
	"connect-src 'self'; font-src 'self'; worker-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// TestConsolePolicyLoosensNothing: whatever the policy is written as, it
// names none of the sources that would let code in.
func TestConsolePolicyLoosensNothing(t *testing.T) {
	t.Parallel()
	for _, page := range []*httptest.ResponseRecorder{
		get(t, console.NewHandler(fstest.MapFS{"index.html": {Data: []byte("<title>c</title>")}}, true), http.MethodGet, "/"),
		get(t, console.NewHandler(nil, false), http.MethodGet, "/"),
	} {
		csp := page.Header().Get("Content-Security-Policy")
		for _, loose := range []string{"unsafe-eval", "unsafe-inline", "wasm-unsafe-eval", "unsafe-hashes", "*", "http:", "https:", "strict-dynamic"} {
			if strings.Contains(csp, loose) {
				t.Errorf("the policy %q names %s", csp, loose)
			}
		}
		if csp != consolePolicy {
			t.Errorf("Content-Security-Policy = %q, want %q", csp, consolePolicy)
		}
	}
}

func TestConsoleFiles(t *testing.T) {
	t.Parallel()
	dist := fstest.MapFS{
		"index.html":          {Data: []byte("<!doctype html><title>console</title>")},
		"favicon.svg":         {Data: []byte("<svg/>")},
		"assets/index-abc.js": {Data: []byte("console.log(1)")},
		// pdf.js's worker keeps the name it is published with (ADR-0028).
		"assets/pdf.worker.min-abc.mjs":                 {Data: []byte("export {}")},
		"assets/pdfjs-6.4.299/jbig2_nowasm_fallback.js": {Data: []byte("export default 1")},
		"assets/index-abc.css":                          {Data: []byte("body{}")},
		"assets/mark-abc.svg":                           {Data: []byte("<svg/>")},
		"assets/icons-abc.woff2":                        {Data: []byte("wOF2")},
		"assets/icons-abc.woff":                         {Data: []byte("wOFF")},
	}
	h := console.NewHandler(dist, true)

	index := get(t, h, http.MethodGet, "/")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "<title>console</title>") || index.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("GET / = %d, Cache-Control %q", index.Code, index.Header().Get("Cache-Control"))
	}
	script := get(t, h, http.MethodGet, "/assets/index-abc.js")
	if script.Code != http.StatusOK || !strings.Contains(script.Header().Get("Cache-Control"), "immutable") ||
		!strings.Contains(script.Header().Get("Content-Type"), "javascript") {
		t.Errorf("GET of an asset = %d, %v", script.Code, script.Header())
	}
	// Every kind of file the build emits has its type from the handler,
	// exactly, not from the host's table of types: with nosniff a browser
	// refuses a script, a worker or a style sheet of another type.
	worker := get(t, h, http.MethodGet, "/assets/pdf.worker.min-abc.mjs")
	for path, want := range map[string]string{
		"/":                              "text/html; charset=utf-8",
		"/favicon.svg":                   "image/svg+xml",
		"/assets/index-abc.js":           "text/javascript; charset=utf-8",
		"/assets/pdf.worker.min-abc.mjs": "text/javascript; charset=utf-8",
		"/assets/pdfjs-6.4.299/jbig2_nowasm_fallback.js": "text/javascript; charset=utf-8",
		"/assets/index-abc.css":                          "text/css; charset=utf-8",
		"/assets/mark-abc.svg":                           "image/svg+xml",
		"/assets/icons-abc.woff2":                        "font/woff2",
		"/assets/icons-abc.woff":                         "font/woff",
	} {
		rec := get(t, h, http.MethodGet, path)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != want {
			t.Errorf("GET %s = %d, Content-Type %q, want %q", path, rec.Code, rec.Header().Get("Content-Type"), want)
		}
	}
	// A script that is not there is a plain 404, not JavaScript.
	if rec := get(t, h, http.MethodGet, "/assets/missing.js"); rec.Code != http.StatusNotFound || strings.Contains(rec.Header().Get("Content-Type"), "javascript") {
		t.Errorf("GET of a missing script = %d, Content-Type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, rec := range []*httptest.ResponseRecorder{index, script, worker, get(t, h, http.MethodGet, "/nothing-here")} {
		if csp := rec.Header().Get("Content-Security-Policy"); csp != consolePolicy {
			t.Errorf("Content-Security-Policy = %q, want %q", csp, consolePolicy)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("security headers = %v", rec.Header())
		}
	}
	if rec := get(t, h, http.MethodGet, "/nothing-here"); rec.Code != http.StatusNotFound {
		t.Errorf("GET of a missing file = %d", rec.Code)
	}
	if rec := get(t, h, http.MethodGet, "/assets/../../etc/passwd"); rec.Code == http.StatusOK {
		t.Errorf("a path out of the console = %d", rec.Code)
	}
	if rec := get(t, h, http.MethodPost, "/"); rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") == "" {
		t.Errorf("POST / = %d", rec.Code)
	}
	if rec := get(t, h, http.MethodHead, "/"); rec.Code != http.StatusOK {
		t.Errorf("HEAD / = %d", rec.Code)
	}
}

func TestConsoleThatWasNotBuilt(t *testing.T) {
	t.Parallel()
	h := console.NewHandler(nil, false)
	page := get(t, h, http.MethodGet, "/")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "console was not built") || page.Header().Get("Content-Security-Policy") == "" {
		t.Errorf("GET / = %d: %s", page.Code, page.Body.String())
	}
	if rec := get(t, h, http.MethodGet, "/assets/x.js"); rec.Code != http.StatusNotFound {
		t.Errorf("GET of a file of a console that was not built = %d", rec.Code)
	}
}
