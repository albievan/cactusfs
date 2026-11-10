package github

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// testVersion is the version of the build the tests name.
const testVersion = "0.7.0"

// serve starts a TLS server on 127.0.0.1 with the handler.
func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	// A connection a test makes fail is logged by the server; it is meant.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// caOf returns the certificate of a test server, as PEM.
func caOf(server *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
}

// loopbackAllowed is the egress-allow of a test: its servers are on
// loopback, which is refused otherwise.
func loopbackAllowed() []netip.Prefix {
	return []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
}

// newSource returns a source of the releases JSON at path on server.
func newSource(t *testing.T, server *httptest.Server, path string, timeout time.Duration) *Source {
	t.Helper()
	source, err := New(Config{URL: server.URL + path, CA: caOf(server), Allow: loopbackAllowed(), Version: testVersion, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

// checkHeaders reports what a request of the source must not carry, or
// must: no credential, no cookie, its User-Agent.
func checkHeaders(r *http.Request) error {
	for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization", "Referer"} {
		if value := r.Header.Get(name); value != "" {
			return fmt.Errorf("%s %s carried %s: %q", r.Method, r.URL.Path, name, value)
		}
	}
	if got := r.Header.Get("User-Agent"); got != "cactusfs/"+testVersion {
		return fmt.Errorf("%s %s: User-Agent %q", r.Method, r.URL.Path, got)
	}
	return nil
}

// mirrorOf returns a handler that answers as GitHub does for the script:
// the releases JSON at /repos/o/r/releases, every file at
// /download/<tag>/<name> redirected - with a cookie - to /storage/<n>,
// which serves it with its length. Every request is checked by
// checkHeaders.
func mirrorOf(t *testing.T, script porttest.ReleaseSourceScript, base func() string) http.HandlerFunc {
	t.Helper()
	index := map[string]int{}
	var contents [][]byte
	for _, rel := range script.Releases {
		for _, a := range rel.Assets {
			index["/download/"+rel.Tag+"/"+a.Name] = len(contents)
			contents = append(contents, a.Content)
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if err := checkHeaders(r); err != nil {
			t.Error(err)
		}
		switch {
		case r.URL.Path == "/repos/o/r/releases":
			if r.URL.RawQuery != "per_page=30" || r.Header.Get("Accept") != "application/vnd.github+json" {
				t.Errorf("the releases asked with ?%s, Accept %q", r.URL.RawQuery, r.Header.Get("Accept"))
			}
			var list []map[string]any
			for _, rel := range script.Releases {
				var assets []map[string]any
				for _, a := range rel.Assets {
					address := base() + "/download/" + rel.Tag + "/" + a.Name
					if a.Insecure {
						address = strings.Replace(address, "https://", "http://", 1)
					}
					assets = append(assets, map[string]any{"name": a.Name, "browser_download_url": address, "size": len(a.Content)})
				}
				list = append(list, map[string]any{
					"tag_name": rel.Tag, "draft": false, "prerelease": rel.Prerelease,
					"published_at": "2026-11-01T08:21:07Z", "html_url": base() + "/notes/" + rel.Tag, "assets": assets,
				})
			}
			// A draft of every release is left out whatever it says.
			list = append(list, map[string]any{"tag_name": "v9.9.9", "draft": true, "published_at": nil})
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(list)
		case strings.HasPrefix(r.URL.Path, "/download/"):
			http.SetCookie(w, &http.Cookie{Name: "logged_in", Value: "no", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			n, ok := index[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			http.Redirect(w, r, "/storage/"+strconv.Itoa(n)+"?X-Amz-Signature=abc", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/storage/"):
			n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/storage/"))
			if err != nil || n < 0 || n >= len(contents) {
				http.NotFound(w, r)
				return
			}
			content := contents[n]
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			_, _ = w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}
}

// TestContract: the source keeps the promises of ports.ReleaseSource,
// against a mirror that answers as GitHub does, its downloads redirected.
func TestContract(t *testing.T) {
	t.Parallel()
	porttest.ReleaseSourceContract(t, func(t *testing.T, script porttest.ReleaseSourceScript) ports.ReleaseSource {
		t.Helper()
		var base atomic.Pointer[string]
		server := serve(t, mirrorOf(t, script, func() string { return *base.Load() }))
		base.Store(&server.URL)
		return newSource(t, server, "/repos/o/r/releases", 0)
	})
}

// TestFixture: GitHub's releases JSON, trimmed: the draft, the tag that is
// no version and the tag with build metadata are left out; the pre-release
// is one; the asset over http is dropped, the release kept.
func TestFixture(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/releases.json")
	if err != nil {
		t.Fatal(err)
	}
	server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(data)
	})
	got, err := newSource(t, server, "/repos/albievan/cactusfs/releases", 0).Releases(t.Context())
	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	var versions []string
	for _, r := range got {
		versions = append(versions, r.Version.String())
	}
	if want := "0.8.0-rc.1 0.7.2 0.7.1"; strings.Join(versions, " ") != want {
		t.Fatalf("Releases = %v, want %s", versions, want)
	}
	rc, patch := got[0], got[1]
	if !rc.Prerelease || patch.Prerelease {
		t.Errorf("pre-release: %v, %v", rc.Prerelease, patch.Prerelease)
	}
	if want := time.Date(2026, 11, 1, 8, 21, 7, 0, time.UTC); !patch.Published.Equal(want) {
		t.Errorf("published %v, want %v", patch.Published, want)
	}
	if patch.NotesURL != "https://github.com/albievan/cactusfs/releases/tag/v0.7.2" {
		t.Errorf("notes %q", patch.NotesURL)
	}
	var names []string
	for _, a := range patch.Assets {
		names = append(names, a.Name)
	}
	if want := "cactusfs-linux-amd64 SHA256SUMS SHA256SUMS.sigstore.json"; strings.Join(names, " ") != want {
		t.Errorf("assets %v, want %s", names, want)
	}
	if sums, ok := patch.Asset(release.SumsAsset); !ok || sums.Size != 534 ||
		sums.URL != "https://github.com/albievan/cactusfs/releases/download/v0.7.2/SHA256SUMS" {
		t.Errorf("SHA256SUMS: %+v, %v", sums, ok)
	}
}

// TestReleasesAnswers: what the source answers, by status, headers and
// body, and the class and sentinel of each failure.
func TestReleasesAnswers(t *testing.T) {
	t.Parallel()
	big := append([]byte("["), bytes.Repeat([]byte(" "), MaxReleasesBody)...)
	big = append(big, ']')
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = io.WriteString(w, `[{"tag_name":"v0.7.1","assets":[]},{"tag_name":"v0.7.2","assets":"not a list"}]`)
		case "/limited":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
		case "/retry-after":
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusForbidden)
		case "/429":
			w.WriteHeader(http.StatusTooManyRequests)
		case "/403":
			w.Header().Set("X-RateLimit-Remaining", "59")
			w.WriteHeader(http.StatusForbidden)
		case "/404":
			http.NotFound(w, r)
		case "/500":
			w.WriteHeader(http.StatusBadGateway)
		case "/204":
			w.WriteHeader(http.StatusNoContent)
		case "/not-json":
			_, _ = io.WriteString(w, "<html>a portal</html>")
		case "/object":
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		case "/big-declared":
			w.Header().Set("Content-Length", strconv.Itoa(len(big)))
			_, _ = w.Write(big)
		case "/big-chunked":
			_ = http.NewResponseController(w).Flush()
			_, _ = w.Write(big)
		}
	})
	tests := []struct {
		path   string
		class  Class
		sentry error
	}{
		{path: "/ok", class: ClassNone},
		{path: "/limited", class: ClassRateLimited, sentry: domain.ErrUnavailable},
		{path: "/retry-after", class: ClassRateLimited, sentry: domain.ErrUnavailable},
		{path: "/429", class: ClassRateLimited, sentry: domain.ErrUnavailable},
		{path: "/403", class: ClassRefused},
		{path: "/404", class: ClassRefused},
		{path: "/500", class: ClassUnreachable, sentry: domain.ErrUnavailable},
		{path: "/204", class: ClassInvalidAnswer},
		{path: "/not-json", class: ClassInvalidAnswer},
		{path: "/object", class: ClassInvalidAnswer},
		{path: "/big-declared", class: ClassInvalidAnswer},
		{path: "/big-chunked", class: ClassInvalidAnswer},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			got, err := newSource(t, server, tt.path, 0).Releases(t.Context())
			if ClassOf(err) != tt.class {
				t.Fatalf("Releases = %v, %v (class %q), want class %q", got, err, ClassOf(err), tt.class)
			}
			if tt.class == ClassNone {
				if len(got) != 1 || got[0].Version.String() != "0.7.1" {
					t.Errorf("Releases = %v", got)
				}
				return
			}
			if tt.sentry != nil && !errors.Is(err, tt.sentry) {
				t.Errorf("Releases = %v, want %v", err, tt.sentry)
			}
			if tt.sentry == nil && errors.Is(err, domain.ErrUnavailable) {
				t.Errorf("Releases = %v: not a failure asking later mends", err)
			}
		})
	}
}

// TestStall: a source that sends no headers, or stops in the middle of a
// body, fails as a timeout within the response timeout.
func TestStall(t *testing.T) {
	t.Parallel()
	done := make(chan struct{})
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/body" {
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "[  ")
			_ = http.NewResponseController(w).Flush()
		}
		select {
		case <-r.Context().Done():
		case <-done:
		}
	})
	t.Cleanup(func() { close(done) })
	for _, path := range []string{"/headers", "/body"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			source := newSource(t, server, path, 200*time.Millisecond)
			started := time.Now()
			_, err := source.Releases(t.Context())
			if ClassOf(err) != ClassTimeout || !errors.Is(err, domain.ErrUnavailable) {
				t.Errorf("Releases = %v (class %q), want a timeout", err, ClassOf(err))
			}
			var buf bytes.Buffer
			err = source.Fetch(t.Context(), release.Asset{Name: "x", URL: server.URL + path}, 1000, &buf)
			if ClassOf(err) != ClassTimeout || !errors.Is(err, domain.ErrUnavailable) {
				t.Errorf("Fetch = %v (class %q), want a timeout", err, ClassOf(err))
			}
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Errorf("took %s", elapsed)
			}
		})
	}
	t.Run("the caller cancelled", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, err := newSource(t, server, "/headers", 0).Releases(ctx)
		if !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrUnavailable) {
			t.Errorf("Releases = %v, want the cancellation alone", err)
		}
	})
	t.Run("the caller's deadline", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		_, err := newSource(t, server, "/headers", 0).Releases(ctx)
		if ClassOf(err) != ClassTimeout || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Releases = %v (class %q), want the deadline", err, ClassOf(err))
		}
	})
}

// TestFetchBounds: a declared length over the limit is refused before the
// body is read - one over the egress bound too - and a body longer than the
// limit whatever it declared fails once the limit is written.
func TestFetchBounds(t *testing.T) {
	t.Parallel()
	const limit = 1000
	done := make(chan struct{})
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/declared", "/declared-huge":
			// The length is declared and the body never comes: a client
			// that reads it waits until the test ends.
			length := strconv.Itoa(limit + 1)
			if r.URL.Path == "/declared-huge" {
				length = strconv.FormatInt(1<<40, 10)
			}
			w.Header().Set("Content-Length", length)
			w.WriteHeader(http.StatusOK)
			_ = http.NewResponseController(w).Flush()
			select {
			case <-r.Context().Done():
			case <-done:
			}
		case "/lying":
			// A small length, and chunks that override it (RFC 9112):
			// the body is far longer than it says.
			conn, rw, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = conn.Close() }()
			chunk := strings.Repeat("L", 2*limit)
			_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Length: 4\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", len(chunk), chunk)
			_ = rw.Flush()
		case "/chunked-over", "/chunked-exact":
			size := limit
			if r.URL.Path == "/chunked-over" {
				size++
			}
			_ = http.NewResponseController(w).Flush()
			_, _ = w.Write(bytes.Repeat([]byte("c"), size))
		case "/short":
			w.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(w, "ten bytes.")
		}
	})
	t.Cleanup(func() { close(done) })
	source := newSource(t, server, "/", 30*time.Second)
	tests := []struct {
		path    string
		limit   int64
		want    error
		written int
	}{
		{path: "/declared", limit: limit, want: domain.ErrEntityTooLarge},
		{path: "/declared-huge", limit: MaxDownload, want: domain.ErrEntityTooLarge},
		{path: "/lying", limit: limit, want: domain.ErrEntityTooLarge, written: limit},
		{path: "/chunked-over", limit: limit, want: domain.ErrEntityTooLarge, written: limit},
		{path: "/chunked-exact", limit: limit, written: limit},
		{path: "/short", limit: limit, want: domain.ErrUnavailable, written: 10},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			started := time.Now()
			err := source.Fetch(t.Context(), release.Asset{Name: "x", URL: server.URL + tt.path}, tt.limit, &buf)
			if (tt.want == nil) != (err == nil) || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("Fetch = %v, want %v", err, tt.want)
			}
			if buf.Len() != tt.written {
				t.Errorf("wrote %d bytes, want %d", buf.Len(), tt.written)
			}
			if errors.Is(tt.want, domain.ErrEntityTooLarge) && ClassOf(err) != ClassInvalidAnswer {
				t.Errorf("class %q, want %q", ClassOf(err), ClassInvalidAnswer)
			}
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Errorf("took %s: the body was waited for", elapsed)
			}
		})
	}
}

// TestFetchRedirects: three redirects are followed and the fourth is
// refused; a redirect to http, to loopback that egress-allow does not
// admit, and to the metadata service is refused, and the place is never
// asked; no cookie set on a hop is sent on the next; the error names the
// origin and no path or query.
func TestFetchRedirects(t *testing.T) {
	t.Parallel()
	var clear atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		clear.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(plain.Close)
	mark := "secret-path-77"
	var port atomic.Pointer[string]
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if err := checkHeaders(r); err != nil {
			t.Error(err)
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/hops/"):
			n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hops/"))
			if n == 0 {
				_, _ = io.WriteString(w, "arrived")
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "hop", Value: strconv.Itoa(n), Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/hops/"+strconv.Itoa(n-1)+"?"+mark, http.StatusFound)
		case r.URL.Path == "/to-http":
			http.Redirect(w, r, plain.URL+"/"+mark, http.StatusFound)
		case r.URL.Path == "/to-ipv6-loopback":
			http.Redirect(w, r, "https://[::1]:"+*port.Load()+"/"+mark, http.StatusFound)
		case r.URL.Path == "/to-metadata":
			http.Redirect(w, r, "https://169.254.169.254/"+mark, http.StatusFound)
		}
	})
	_, p, _ := strings.Cut(strings.TrimPrefix(server.URL, "https://"), ":")
	port.Store(&p)
	// Only 127.0.0.1 is admitted: [::1] is loopback that egress-allow does
	// not name.
	source, err := New(Config{
		URL: server.URL + "/r", CA: caOf(server), Version: testVersion,
		Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := source.Fetch(t.Context(), release.Asset{Name: "x", URL: server.URL + "/hops/3?" + mark}, 100, &buf); err != nil || buf.String() != "arrived" {
		t.Fatalf("three redirects: %q, %v", buf.String(), err)
	}
	for _, path := range []string{"/hops/4", "/to-http", "/to-ipv6-loopback", "/to-metadata"} {
		buf.Reset()
		err := source.Fetch(t.Context(), release.Asset{Name: "x", URL: server.URL + path + "?" + mark}, 100, &buf)
		if ClassOf(err) != ClassRefused || errors.Is(err, domain.ErrUnavailable) || buf.Len() != 0 {
			t.Errorf("%s: %v (class %q), %d bytes; want refused", path, err, ClassOf(err), buf.Len())
		}
		if err == nil {
			continue
		}
		if text := fmt.Sprintf("%v %+v", err, err); strings.Contains(text, mark) || strings.Contains(text, path) || !strings.Contains(text, server.URL) {
			t.Errorf("%s: the error does not name the origin alone: %s", path, text)
		}
	}
	if clear.Load() != 0 {
		t.Error("a redirect to http was followed")
	}
}

// TestRefusedBeforeAsking: an asset address that is not https, with user
// information or none at all, and a negative limit are refused without a
// request; the writer's failure is the writer's.
func TestRefusedBeforeAsking(t *testing.T) {
	t.Parallel()
	var asked atomic.Int64
	server := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		_, _ = io.WriteString(w, "content")
	})
	source := newSource(t, server, "/", 0)
	for _, address := range []string{
		strings.Replace(server.URL, "https://", "http://", 1) + "/x",
		"https://user:pass@" + strings.TrimPrefix(server.URL, "https://") + "/x",
		"https:///x", "https:opaque", "%zz", "",
	} {
		var buf bytes.Buffer
		err := source.Fetch(t.Context(), release.Asset{Name: "x", URL: address}, 100, &buf)
		if !errors.Is(err, domain.ErrInvalidArgument) || ClassOf(err) != ClassRefused {
			t.Errorf("Fetch(%q) = %v", address, err)
		}
		if err != nil && strings.Contains(err.Error(), "pass") {
			t.Errorf("Fetch(%q): the error tells the address: %v", address, err)
		}
	}
	err := source.Fetch(t.Context(), release.Asset{Name: "x", URL: server.URL + "/x"}, -1, io.Discard)
	if !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a negative limit: %v", err)
	}
	if asked.Load() != 0 {
		t.Errorf("the source was asked %d times", asked.Load())
	}
	broken := errors.New("disk full")
	err = source.Fetch(t.Context(), release.Asset{Name: "x", URL: server.URL + "/x"}, 100, failingWriter{err: broken})
	if !errors.Is(err, broken) || errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("a writer that fails: %v", err)
	}
}

// TestFetchOnlyFromTheSourcesHost: an asset whose address names another
// host or port than the source's is refused and never asked, whatever the
// releases JSON says; a redirect from the source's host is followed.
func TestFetchOnlyFromTheSourcesHost(t *testing.T) {
	t.Parallel()
	var asked, other atomic.Int64
	elsewhere := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		other.Add(1)
		_, _ = io.WriteString(w, "elsewhere")
	})
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if r.URL.Path == "/redirected" {
			http.Redirect(w, r, elsewhere.URL+"/storage", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "content")
	})
	// Both servers' certificates are httptest's one: the CA of the one is
	// the CA of the other.
	source := newSource(t, server, "/repos/o/r/releases", 0)
	_, port, _ := strings.Cut(strings.TrimPrefix(server.URL, "https://"), ":")
	for _, address := range []string{
		elsewhere.URL + "/x",                // another port of the same address
		"https://localhost:" + port + "/x",  // the same server by another name
		"https://github.com/o/r/releases/x", // GitHub, of a source that is not GitHub
		"https://127.0.0.1/x",               // the default port
		"https://169.254.169.254/latest/x",  // the metadata service
		"https://api.github.com:8443/o/r/x", // a name GitHub uses, another port
	} {
		var buf bytes.Buffer
		err := source.Fetch(t.Context(), release.Asset{Name: "x", URL: address}, 100, &buf)
		if !errors.Is(err, domain.ErrInvalidArgument) || ClassOf(err) != ClassRefused || buf.Len() != 0 {
			t.Errorf("Fetch(%q) = %v (class %q), %d bytes; want refused", address, err, ClassOf(err), buf.Len())
		}
	}
	if asked.Load() != 0 || other.Load() != 0 {
		t.Errorf("asked %d and %d times", asked.Load(), other.Load())
	}
	var buf bytes.Buffer
	if err := source.Fetch(t.Context(), release.Asset{Name: "x", URL: strings.ToUpper(server.URL[:8]) + server.URL[8:] + "/x"}, 100, &buf); err != nil || buf.String() != "content" {
		t.Errorf("the source's own host: %q, %v", buf.String(), err)
	}
	buf.Reset()
	if err := source.Fetch(t.Context(), release.Asset{Name: "x", URL: server.URL + "/redirected"}, 100, &buf); err != nil || buf.String() != "elsewhere" {
		t.Errorf("a redirect from the source's host: %q, %v", buf.String(), err)
	}
}

// TestTheAssetHostsOfGitHub: a source that is GitHub's API takes the files
// GitHub names at github.com:443, and no other host; a mirror takes its own
// host only.
func TestTheAssetHostsOfGitHub(t *testing.T) {
	t.Parallel()
	tests := []struct {
		source string
		asset  string
		want   bool
	}{
		{"https://api.github.com/repos/albievan/cactusfs/releases", "https://github.com/albievan/cactusfs/releases/download/v0.7.1/SHA256SUMS", true},
		{"https://api.github.com/repos/albievan/cactusfs/releases", "https://GitHub.com:443/albievan/cactusfs/releases/download/v0.7.1/SHA256SUMS", true},
		{"https://api.github.com/repos/albievan/cactusfs/releases", "https://api.github.com/repos/albievan/cactusfs/releases/assets/1", true},
		{"https://api.github.com/repos/albievan/cactusfs/releases", "https://release-assets.githubusercontent.com/x", false},
		{"https://api.github.com/repos/albievan/cactusfs/releases", "https://objects.github.com/x", false},
		{"https://api.github.com/repos/albievan/cactusfs/releases", "https://github.com:8443/x", false},
		{"https://api.github.com:8443/repos/o/r/releases", "https://github.com/x", false},
		{"https://mirror.example/releases", "https://github.com/x", false},
		{"https://mirror.example/releases", "https://mirror.example:443/v0.7.1/x", true},
		{"https://mirror.example:8443/releases", "https://mirror.example/x", false},
	}
	for _, tt := range tests {
		source, err := New(Config{URL: tt.source, Version: testVersion})
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(tt.asset)
		if err != nil {
			t.Fatal(err)
		}
		if got := source.assetOrigins[originKey(u)]; got != tt.want {
			t.Errorf("source %s, asset %s: allowed %v, want %v", tt.source, tt.asset, got, tt.want)
		}
	}
}

// failingWriter is a writer that fails.
type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

// TestNewRefuses: a configuration that cannot be used is refused, and the
// error does not repeat the address.
func TestNewRefuses(t *testing.T) {
	t.Parallel()
	good := Config{URL: "https://api.github.com/repos/albievan/cactusfs/releases", Version: testVersion}
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"http", func(c *Config) { c.URL = "http://api.github.com/repos/o/r/releases" }},
		{"no host", func(c *Config) { c.URL = "https:///repos/o/r/releases" }},
		{"opaque", func(c *Config) { c.URL = "https:api.github.com" }},
		{"user information", func(c *Config) { c.URL = "https://tok3n:x@api.github.com/repos/o/r/releases" }},
		{"a query", func(c *Config) { c.URL = "https://api.github.com/repos/o/r/releases?access_token=tok3n" }},
		{"an empty query", func(c *Config) { c.URL = "https://api.github.com/repos/o/r/releases?" }},
		{"a fragment", func(c *Config) { c.URL = "https://api.github.com/repos/o/r/releases#tok3n" }},
		{"not an address", func(c *Config) { c.URL = "https://api.github.com/%zz/tok3n" }},
		{"none", func(c *Config) { c.URL = "" }},
		{"a CA that is no PEM", func(c *Config) { c.CA = []byte("tok3n") }},
		{"no version", func(c *Config) { c.Version = "" }},
		{"a version with a space", func(c *Config) { c.Version = "0.7.0 evil" }},
		{"a version with a newline", func(c *Config) { c.Version = "0.7.0\r\nX: y" }},
		{"a negative timeout", func(c *Config) { c.Timeout = -time.Second }},
		{"an invalid range", func(c *Config) { c.Allow = []netip.Prefix{{}} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := good
			tt.change(&cfg)
			source, err := New(cfg)
			if source != nil || !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("New = %v, %v", source, err)
			}
			if strings.Contains(err.Error(), "tok3n") {
				t.Errorf("the error repeats the address: %v", err)
			}
			// The composition root refuses the same addresses before the
			// node starts.
			if cfg.URL != good.URL {
				err := CheckSourceURL(cfg.URL)
				if !errors.Is(err, domain.ErrInvalidArgument) || strings.Contains(err.Error(), "tok3n") {
					t.Errorf("CheckSourceURL = %v", err)
				}
			}
		})
	}
	if _, err := New(good); err != nil {
		t.Errorf("New(the default source) = %v", err)
	}
	if err := CheckSourceURL(good.URL); err != nil {
		t.Errorf("CheckSourceURL(the default source) = %v", err)
	}
}

// TestErrorsNameTheOriginOnly: a failure names the scheme, host and port
// of the source, never its path.
func TestErrorsNameTheOriginOnly(t *testing.T) {
	t.Parallel()
	server := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	_, err := newSource(t, server, "/repos/secret-owner/releases", 0).Releases(t.Context())
	if err == nil {
		t.Fatal("Releases = nil")
	}
	text := fmt.Sprintf("%v %+v", err, err)
	if strings.Contains(text, "secret-owner") || !strings.Contains(text, server.URL) || !strings.Contains(text, "503") {
		t.Errorf("the error: %s", text)
	}
	var failure *Error
	if !errors.As(err, &failure) || failure.Status != http.StatusServiceUnavailable || failure.Origin != server.URL {
		t.Errorf("the error: %#v", failure)
	}
}

// TestClassOf: the class of an error that is not this package's.
func TestClassOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		want Class
	}{
		{nil, ClassNone},
		{errors.New("other"), ClassUnreachable},
		{fmt.Errorf("x: %w", context.DeadlineExceeded), ClassTimeout},
		{fmt.Errorf("x: %w", &Error{Class: ClassRateLimited}), ClassRateLimited},
	}
	for _, tt := range tests {
		if got := ClassOf(tt.err); got != tt.want {
			t.Errorf("ClassOf(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
	if got := (&Error{Op: "github.Fetch", Class: ClassRefused}).Error(); got != "github.Fetch: refused" {
		t.Errorf("Error() = %q", got)
	}
}

// FuzzParseReleases: whatever the source sends, parsing does not panic,
// and what it keeps is a release of a version, with no address but https.
func FuzzParseReleases(f *testing.F) {
	fixture, err := os.ReadFile("testdata/releases.json")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(fixture)
	f.Add([]byte(`[]`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`[{"tag_name":"v1.2.3","assets":[{"name":"a","browser_download_url":"https://x/a","size":-1}]}]`))
	f.Add([]byte(`[{"tag_name":"v1.2.3","published_at":"yesterday","html_url":"javascript:alert(1)"}]`))
	f.Add([]byte(`[1, "x", {"tag_name": 7}, {"tag_name":"0.1.0","draft":true}]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := parseReleases(data)
		if err != nil {
			if got != nil {
				t.Errorf("releases and an error: %v", got)
			}
			return
		}
		var entries []json.RawMessage
		if json.Unmarshal(data, &entries) != nil || len(got) > len(entries) {
			t.Fatalf("%d releases of %d entries", len(got), len(entries))
		}
		for _, r := range got {
			if v, err := release.ParseVersion(r.Version.String()); err != nil || v != r.Version {
				t.Errorf("a release of version %q: %v", r.Version, err)
			}
			if r.NotesURL != "" && !strings.HasPrefix(r.NotesURL, "https://") {
				t.Errorf("notes at %q", r.NotesURL)
			}
			for _, a := range r.Assets {
				if !strings.HasPrefix(a.URL, "https://") || a.Size < 0 {
					t.Errorf("an asset %+v", a)
				}
			}
		}
	})
}
