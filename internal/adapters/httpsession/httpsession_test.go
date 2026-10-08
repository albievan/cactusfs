package httpsession_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/httpsession"
)

// TestCookieIsLockedDown: the session cookie cannot be read by scripts, is
// never sent without TLS or from another site, and cannot be set for or by
// another host.
func TestCookieIsLockedDown(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	expires := time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)
	httpsession.Set(recorder, "the-token", expires)

	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("%d cookies set", len(cookies))
	}
	c := cookies[0]
	if c.Name != "__Host-cactusfs-session" || c.Value != "the-token" || !c.HttpOnly || !c.Secure ||
		c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" || !c.Expires.Equal(expires) {
		t.Errorf("cookie = %+v", c)
	}
	header := recorder.Header().Get("Set-Cookie")
	for _, attribute := range []string{"HttpOnly", "Secure", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(header, attribute) {
			t.Errorf("Set-Cookie %q lacks %s", header, attribute)
		}
	}

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://console.test/", nil)
	if httpsession.Token(request) != "" {
		t.Error("a request without the cookie has a token")
	}
	request.AddCookie(c)
	// A cookie of a similar name, as a page on a sibling host could plant.
	request.Header.Add("Cookie", "cactusfs-session=planted-by-a-subdomain")
	if got := httpsession.Token(request); got != "the-token" {
		t.Errorf("Token = %q", got)
	}

	cleared := httptest.NewRecorder()
	httpsession.Clear(cleared)
	gone := cleared.Result().Cookies()[0]
	if gone.Name != c.Name || gone.Value != "" || gone.MaxAge >= 0 || !gone.HttpOnly || !gone.Secure || gone.SameSite != http.SameSiteStrictMode {
		t.Errorf("clearing cookie = %+v", gone)
	}
}

func TestCSRFAndMutating(t *testing.T) {
	t.Parallel()
	for method, mutating := range map[string]bool{
		http.MethodGet: false, http.MethodHead: false, http.MethodOptions: false,
		http.MethodPost: true, http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true, "PROPFIND": true,
	} {
		request := httptest.NewRequestWithContext(t.Context(), method, "https://console.test/api/v1/users", nil)
		if got := httpsession.Mutating(request); got != mutating {
			t.Errorf("Mutating(%s) = %v, want %v", method, got, mutating)
		}
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://console.test/", nil)
	if httpsession.CSRFToken(request) != "" {
		t.Error("a request without the header has a CSRF token")
	}
	request.Header.Set("X-CSRF-Token", "csrf")
	if httpsession.CSRFToken(request) != "csrf" {
		t.Error("CSRFToken did not read the header")
	}
}
