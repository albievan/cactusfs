package s3api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/sigv4"
)

// printableASCII is every printable ASCII character, the space first.
const printableASCII = " !\"#$%&'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~"

// printable maps a string the generator made to printable ASCII with
// spaces, which is what a header value may hold.
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		i := int(r) % len(printableASCII)
		if i < 0 {
			i = -i
		}
		b.WriteByte(printableASCII[i])
	}
	return b.String()
}

// TestSignRequestVerifies is the property that ties the two sides of SigV4
// together: a request signed by sigv4.SignRequest - whatever its method,
// path, query, x-amz-* headers, body and secret - crosses a real connection
// and verifies in this package as a client's would, and stops verifying
// under another secret.
func TestSignRequestVerifies(t *testing.T) {
	t.Parallel()
	// The access key ID of AWS's example pair, kept as test data.
	keyID := loadDocVectors(t).AccessKeyID
	signedAt := time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC)

	var mu sync.Mutex
	var secret []byte
	results := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		key := secret
		mu.Unlock()
		sig, err := parseAuthorization(r.Header.Get("Authorization"))
		if err == nil {
			_, err = verifySignature(r, sig, key, signedAt)
		}
		results <- err
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	methods := []string{http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete, http.MethodHead}
	send := func(r *http.Request) error {
		t.Helper()
		resp, err := server.Client().Do(r)
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return <-results
	}
	property := func(method uint8, path string, query map[string]string, meta []string, body, key []byte) bool {
		if len(key) == 0 {
			key = []byte{0}
		}
		mu.Lock()
		secret = key
		mu.Unlock()
		target := *base
		target.Path = "/bucket/" + path
		values := url.Values{}
		for name, value := range query {
			values.Add(name, value)
		}
		target.RawQuery = values.Encode()
		r, err := http.NewRequestWithContext(t.Context(), methods[int(method)%len(methods)], target.String(), bytes.NewReader(body))
		if err != nil {
			t.Logf("new request: %v", err)
			return false
		}
		for i, value := range meta {
			r.Header.Add("X-Amz-Meta-M"+string(rune('a'+i%26)), printable(value))
		}
		r.Header.Set("Content-Type", printable(path))
		sum := sha256.Sum256(body)
		if err := sigv4.SignRequest(r, keyID, key, DefaultRegion, hex.EncodeToString(sum[:]), signedAt); err != nil {
			t.Logf("SignRequest: %v", err)
			return false
		}
		if err := send(r); err != nil {
			t.Logf("%s %s: %v", r.Method, r.URL, err)
			return false
		}
		// The same request under another secret is refused.
		mu.Lock()
		secret = append(bytes.Clone(key), 'x')
		mu.Unlock()
		again := r.Clone(t.Context())
		again.Body = io.NopCloser(bytes.NewReader(body))
		return send(again) != nil
	}
	// The sub-delimiters of a URL, which net/http would send unencoded.
	if !property(1, "!$&'()*+,;=:@ and more", map[string]string{"q": "!$&'()*+,;=:@"}, []string{"v"}, []byte("body"), []byte("key")) {
		t.Error("a path of sub-delimiters does not verify")
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 300}); err != nil {
		t.Error(err)
	}
}

// TestSignRequestAgainstTheServer: an object written and read back through
// the S3 API with requests sigv4.SignRequest signed, as one deployment
// replicates to another.
func TestSignRequestAgainstTheServer(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("replica")
	content := []byte("a version, replicated")
	sum := sha256.Sum256(content)

	put, err := http.NewRequestWithContext(t.Context(), http.MethodPut, s.URL+"/replica/a%20key/with+signs!$&'()*,;=:@", bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	put.Header.Set("X-Amz-Meta-Source", "site-a")
	if err := sigv4.SignRequest(put, s.keyID, s.key, DefaultRegion, hex.EncodeToString(sum[:]), serverNow); err != nil {
		t.Fatal(err)
	}
	resp, err := s.Client().Do(put)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}

	get, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL+"/replica/a%20key/with+signs!$&'()*,;=:@", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sigv4.SignRequest(get, s.keyID, s.key, DefaultRegion, sigv4.EmptyPayloadSHA256, serverNow); err != nil {
		t.Fatal(err)
	}
	resp, err = s.Client().Do(get)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(got, content) || resp.Header.Get("X-Amz-Meta-Source") != "site-a" {
		t.Fatalf("GET = %d, %q, %v", resp.StatusCode, got, err)
	}
}
