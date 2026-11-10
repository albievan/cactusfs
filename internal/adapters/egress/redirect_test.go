package egress

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/albievan/cactusfs/internal/domain/delivery"
)

// TestNewClientRefusesBounds: a body bound or a count of redirects outside
// what ADR-0029 allows is refused; the bounds themselves are taken.
func TestNewClientRefusesBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		policy Policy
		ok     bool
	}{
		{name: "the defaults", policy: Policy{}, ok: true},
		{name: "the largest body", policy: Policy{MaxResponseBody: MaxResponseBodyLimit}, ok: true},
		{name: "one byte", policy: Policy{MaxResponseBody: 1}, ok: true},
		{name: "a body above the limit", policy: Policy{MaxResponseBody: MaxResponseBodyLimit + 1}},
		{name: "a negative body", policy: Policy{MaxResponseBody: -1}},
		{name: "the most redirects", policy: Policy{Redirects: MaxRedirects}, ok: true},
		{name: "one redirect too many", policy: Policy{Redirects: MaxRedirects + 1}},
		{name: "fewer than no redirects", policy: Policy{Redirects: -1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client, dial, err := NewClient(tt.policy)
			if tt.ok != (err == nil) || (err == nil) != (client != nil && dial != nil) {
				t.Errorf("NewClient = %v, %v; want ok %v", client, err, tt.ok)
			}
			if err != nil && !errors.Is(err, errPolicy) {
				t.Errorf("NewClient = %v, want errPolicy", err)
			}
		})
	}
}

// TestPolicyBodyBound: a policy's bound replaces 64 KiB: a body of it is
// read whole with its length kept, one byte more ends in
// ErrResponseTooLarge with no length, and Discard of a long body stops
// soon after 64 KiB whatever the bound.
func TestPolicyBodyBound(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	const bound = 1 << 20
	exact, over := bytes.Repeat([]byte("a"), bound), bytes.Repeat([]byte("b"), bound+1)
	server := serve(t, ca, nil, func(w http.ResponseWriter, r *http.Request) {
		body := exact
		if r.URL.Path == "/over" {
			body = over
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	})
	policy := allowLoopback(ca)
	policy.MaxResponseBody = bound
	client, _ := newClient(t, policy)
	base := "https://target.test:" + portOf(t, server.URL)

	got, err := get(t, client, base+"/exact")
	if err != nil || !bytes.Equal(got.body, exact) || got.contentLength != bound {
		t.Errorf("1 MiB under a bound of 1 MiB: %d bytes of %d, %v", len(got.body), got.contentLength, err)
	}
	got, err = get(t, client, base+"/over")
	if !errors.Is(err, ErrResponseTooLarge) || delivery.ClassOf(err) != delivery.ClassServer {
		t.Errorf("1 MiB and a byte: %v (class %q), want %v", err, delivery.ClassOf(err), ErrResponseTooLarge)
	}
	if len(got.body) != bound || got.contentLength != -1 {
		t.Errorf("1 MiB and a byte: read %d of a body said to be %d", len(got.body), got.contentLength)
	}

	body := &endReader{r: bytes.NewReader(make([]byte, bound))}
	if err := Discard(&http.Response{Body: &limitedBody{body: body, left: bound}}); err != nil || body.taken != MaxResponseBody+1 || body.ended {
		t.Errorf("Discard under a bound of 1 MiB: read %d, to the end %v, %v", body.taken, body.ended, err)
	}
}

// hops is a server of a test whose path /hop/<n> redirects n times on the
// same server - relative and absolute in turn - before it answers, and
// records the headers every request came with.
type hops struct {
	mu      sync.Mutex
	headers []http.Header
	base    string
}

func (h *hops) handle(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.headers = append(h.headers, r.Header.Clone())
	base := h.base
	h.mu.Unlock()
	n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
	if err != nil || n <= 0 {
		_, _ = io.WriteString(w, "arrived")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "hop", Value: strconv.Itoa(n), Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	next := "/hop/" + strconv.Itoa(n-1) + "?token=in-query"
	if n%2 == 0 {
		next = base + next
	}
	status := []int{http.StatusFound, http.StatusMovedPermanently, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect}[n%5]
	http.Redirect(w, r, next, status)
}

// TestRedirectsFollowed: a client with Redirects follows that many, to
// https on the same address check, and the next one is refused; every hop
// after the first comes with no Authorization, Cookie or Referer, the
// same host included.
func TestRedirectsFollowed(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	h := &hops{}
	server := serve(t, ca, nil, h.handle)
	base := "https://target.test:" + portOf(t, server.URL)
	h.mu.Lock()
	h.base = base
	h.mu.Unlock()
	policy := allowLoopback(ca)
	policy.Redirects = MaxRedirects
	client, _ := newClient(t, policy)

	send := func(path string) (answer, error) {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer a-token")
		r.Header.Set("Cookie", "session=a-cookie")
		r.Header.Set("Proxy-Authorization", "Basic eA==")
		return do(t, client, r)
	}

	got, err := send(fmt.Sprintf("/hop/%d", MaxRedirects))
	if err != nil || string(got.body) != "arrived" {
		t.Fatalf("%d redirects: %q, %v", MaxRedirects, got.body, err)
	}
	h.mu.Lock()
	seen := h.headers
	h.headers = nil
	h.mu.Unlock()
	if len(seen) != MaxRedirects+1 {
		t.Fatalf("%d requests, want %d", len(seen), MaxRedirects+1)
	}
	if seen[0].Get("Authorization") == "" {
		t.Error("the first request lost its Authorization")
	}
	for i, header := range seen[1:] {
		for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization", "Referer"} {
			if value := header.Get(name); value != "" {
				t.Errorf("hop %d carried %s: %q", i+1, name, value)
			}
		}
	}

	_, err = send(fmt.Sprintf("/hop/%d", MaxRedirects+1))
	if !errors.Is(err, ErrRedirect) || delivery.ClassOf(err) != delivery.ClassRedirect {
		t.Errorf("%d redirects: %v (class %q), want %v", MaxRedirects+1, err, delivery.ClassOf(err), ErrRedirect)
	}
	if err != nil && strings.Contains(err.Error(), "in-query") {
		t.Errorf("the error tells a query: %v", err)
	}
	h.mu.Lock()
	if len(h.headers) != MaxRedirects+1 {
		t.Errorf("the redirect past the count was asked: %d requests", len(h.headers))
	}
	h.mu.Unlock()
}

// TestRedirectsRefused: what a client that follows redirects still refuses
// - a redirect to http, to an address the policy does not admit, one that
// is not a redirect to a Location, a Location that does not parse - and
// that the place pointed to is never asked nor named.
func TestRedirectsRefused(t *testing.T) {
	t.Parallel()
	ca := newTestCA(t)
	var clear atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		clear.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(plain.Close)
	var elsewhere atomic.Int64
	other := serve(t, ca, nil, func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	mark := strings.Join([]string{"TOKEN", "77cd", "IN", "LOCATION"}, "-")
	locations := map[string]string{
		"/to-http":     plain.URL + "/" + mark,
		"/to-ipv6":     "https://[::1]:" + portOf(t, other.URL) + "/" + mark,
		"/to-metadata": "https://169.254.169.254:" + portOf(t, other.URL) + "/" + mark,
		"/malformed":   "https://target.test/" + mark + "/%zz",
		"/ftp":         "ftp://target.test/" + mark,
	}
	server := serve(t, ca, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/none":
			w.WriteHeader(http.StatusFound)
		case "/300":
			w.Header().Set("Location", "/"+mark)
			w.WriteHeader(http.StatusMultipleChoices)
		case "/304":
			w.WriteHeader(http.StatusNotModified)
		case "/307":
			w.Header().Set("Location", "https://target.test:"+portOf(t, other.URL)+"/"+mark)
			w.WriteHeader(http.StatusTemporaryRedirect)
		default:
			w.Header().Set("Location", locations[r.URL.Path])
			w.WriteHeader(http.StatusFound)
		}
	})
	policy := allowLoopback(ca)
	policy.Redirects = MaxRedirects
	client, _ := newClient(t, policy)
	base := "https://target.test:" + portOf(t, server.URL)

	tests := []struct {
		path  string
		want  error
		class delivery.Class
	}{
		{path: "/to-http", want: ErrNotTLS, class: delivery.ClassRedirect},
		{path: "/ftp", want: ErrNotTLS, class: delivery.ClassRedirect},
		{path: "/to-ipv6", want: ErrRefused, class: delivery.ClassEgressRefused},
		{path: "/to-metadata", want: ErrRefused, class: delivery.ClassEgressRefused},
		{path: "/malformed", want: ErrRedirect, class: delivery.ClassRedirect},
		{path: "/none", want: ErrRedirect, class: delivery.ClassRedirect},
		{path: "/300", want: ErrRedirect, class: delivery.ClassRedirect},
		{path: "/304", want: ErrRedirect, class: delivery.ClassRedirect},
		{path: "/307", want: ErrRedirect, class: delivery.ClassRedirect},
	}
	for _, tt := range tests {
		method, body := http.MethodGet, io.Reader(nil)
		if tt.path == "/307" {
			// A body that cannot be sent again: http.Client hands the
			// 307 back as an answer, which is refused all the same.
			method, body = http.MethodPost, struct{ io.Reader }{strings.NewReader("an event")}
		}
		r, err := http.NewRequestWithContext(t.Context(), method, base+tt.path, body)
		if err != nil {
			t.Fatal(err)
		}
		got, err := do(t, client, r)
		if got.status != 0 {
			t.Errorf("%s: a response, %d", tt.path, got.status)
		}
		if !errors.Is(err, tt.want) || delivery.ClassOf(err) != tt.class {
			t.Errorf("%s: %v (class %q), want %v of class %q", tt.path, err, delivery.ClassOf(err), tt.want, tt.class)
		}
		if err != nil && strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), mark) {
			t.Errorf("%s: the error tells the Location: %v", tt.path, err)
		}
	}
	if clear.Load() != 0 || elsewhere.Load() != 0 {
		t.Errorf("a refused redirect was followed: %d in the clear, %d elsewhere", clear.Load(), elsewhere.Load())
	}
}
