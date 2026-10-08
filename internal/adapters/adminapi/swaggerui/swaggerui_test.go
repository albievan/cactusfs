package swaggerui_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/adminapi/swaggerui"
)

// TestDocumentationIsSelfContained: the page, its script and its stylesheet
// come from the binary, and nothing in what we wrote points anywhere else.
func TestDocumentationIsSelfContained(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	swaggerui.Register(mux)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		return rec
	}
	page := get("/docs")
	if page.Code != http.StatusOK || !strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET /docs = %d %q", page.Code, page.Header().Get("Content-Type"))
	}
	if get("/docs/").Body.String() != page.Body.String() {
		t.Error("/docs/ is not /docs")
	}
	// Everything the page refers to is served, from here.
	refs := regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(page.Body.String(), -1)
	if len(refs) < 4 {
		t.Fatalf("the page refers to %d files", len(refs))
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref[1], "/docs/") {
			t.Errorf("the page loads %q, which is not ours", ref[1])
			continue
		}
		if rec := get(ref[1]); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("GET %s = %d", ref[1], rec.Code)
		}
	}
	if strings.Contains(page.Body.String(), "<script>") || strings.Contains(page.Body.String(), "style=") {
		t.Error("the page has inline script or style, which its own policy forbids")
	}
	initializer := get("/docs/initializer.js").Body.String()
	if !strings.Contains(initializer, "validatorUrl: null") || !strings.Contains(initializer, "'/api/v1/openapi.json'") ||
		strings.Contains(initializer, "http://") || strings.Contains(initializer, "https://") {
		t.Errorf("initializer = %s", initializer)
	}
	for _, path := range []string{"/docs", "/docs/swagger-ui-bundle.js", "/docs/swagger-ui.css"} {
		rec := get(path)
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "connect-src 'self'") || strings.Contains(csp, "unsafe") ||
			rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: headers %v", path, rec.Header())
		}
	}
	if rec := get("/docs/dist/LICENSE"); rec.Code == http.StatusOK {
		t.Error("files that were not meant to be served are served")
	}
	if rec := get("/docs/nothing"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /docs/nothing = %d", rec.Code)
	}
}
