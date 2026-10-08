// Package httpsession carries a console session between browser and server:
// the cookie that holds the session token, and the header that holds the
// CSRF token. It only moves the two tokens; what they mean is decided by the
// session use cases.
//
// The cookie is HttpOnly (scripts cannot read it), Secure (sent over TLS
// only), SameSite=Strict (not sent on requests that come from another site)
// and carries the "__Host-" prefix, which makes a browser refuse it unless it
// is Secure, has no Domain and has Path=/ - so that no other host and no
// plaintext page can set or overwrite it.
package httpsession

import (
	"net/http"
	"time"
)

const (
	// CookieName is the name of the session cookie.
	CookieName = "__Host-cactusfs-session"
	// CSRFHeader is the request header that carries the CSRF token.
	CSRFHeader = "X-CSRF-Token"
)

// Set stores a session token in the response. expires is when the session
// ends; the browser drops the cookie then.
func Set(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, Cookie(token, expires))
}

// Cookie returns the cookie that holds a session token until expires.
func Cookie(token string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name: CookieName, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	}
}

// Expired returns the cookie that makes a browser forget the session.
func Expired() *http.Cookie {
	return &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	}
}

// Clear makes the browser forget the session cookie.
func Clear(w http.ResponseWriter) {
	http.SetCookie(w, Expired())
}

// Token returns the session token of a request, or "" when it has none.
func Token(r *http.Request) string {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// CSRFToken returns the CSRF token a request sent, or "".
func CSRFToken(r *http.Request) string {
	return r.Header.Get(CSRFHeader)
}

// Mutating reports whether a request may change something, and must
// therefore carry the CSRF token: every method but the ones HTTP defines as
// safe.
func Mutating(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
