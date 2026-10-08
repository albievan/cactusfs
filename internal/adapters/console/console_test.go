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

func TestConsoleFiles(t *testing.T) {
	t.Parallel()
	dist := fstest.MapFS{
		"index.html":          {Data: []byte("<!doctype html><title>console</title>")},
		"favicon.svg":         {Data: []byte("<svg/>")},
		"assets/index-abc.js": {Data: []byte("console.log(1)")},
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
	for _, rec := range []*httptest.ResponseRecorder{index, script, get(t, h, http.MethodGet, "/nothing-here")} {
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") ||
			!strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("Content-Security-Policy = %q", csp)
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
