package console

import "testing"

// TestContentTypeOfEveryFileTheBuildEmits: the handler's own type for each
// extension the console's build emits - not the host's table of types, which
// differs between systems and which the handler's tests cannot tell apart
// from it on a host whose table agrees (a Mac's names .txt text/plain).
func TestContentTypeOfEveryFileTheBuildEmits(t *testing.T) {
	t.Parallel()
	for ext, want := range map[string]string{
		".html":  "text/html; charset=utf-8",
		".js":    "text/javascript; charset=utf-8",
		".mjs":   "text/javascript; charset=utf-8",
		".css":   "text/css; charset=utf-8",
		".svg":   "image/svg+xml",
		".woff2": "font/woff2",
		".woff":  "font/woff",
		// The notices of the packages the console is built with (PLAN X.5).
		".txt":  "text/plain; charset=utf-8",
		".wasm": "",
		"":      "",
	} {
		if got := contentTypeOf(ext); got != want {
			t.Errorf("contentTypeOf(%q) = %q, want %q", ext, got, want)
		}
	}
}
