package vault_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/kms/vault"
	"github.com/albievan/cactusfs/internal/domain"
)

// transit is a stand-in for Vault's Transit engine: an AEAD key behind the
// two endpoints the adapter uses.
type transit struct {
	t     *testing.T
	token string
	aead  cipher.AEAD
	// ignoreAAD makes it behave like a key type that does not bind
	// associated data.
	ignoreAAD bool

	mu       sync.Mutex
	requests []*http.Request
	fail     int
}

func newTransit(t *testing.T) *transit {
	t.Helper()
	key := make([]byte, 32)
	token := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return &transit{t: t, token: base64.RawURLEncoding.EncodeToString(token), aead: aead}
}

func (v *transit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	v.requests = append(v.requests, r)
	fail := v.fail
	v.mu.Unlock()
	reply := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	if fail != 0 {
		reply(fail, map[string]any{"errors": []string{"vault is sealed"}})
		return
	}
	if r.Header.Get("X-Vault-Token") != v.token {
		reply(http.StatusForbidden, map[string]any{"errors": []string{"permission denied"}})
		return
	}
	var in map[string]string
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		reply(http.StatusBadRequest, map[string]any{"errors": []string{"bad request"}})
		return
	}
	aad, _ := base64.StdEncoding.DecodeString(in["associated_data"])
	if v.ignoreAAD {
		aad = nil
	}
	switch r.URL.Path {
	case "/v1/transit/encrypt/cactusfs", "/v1/kms/prod/encrypt/cactusfs":
		plaintext, err := base64.StdEncoding.DecodeString(in["plaintext"])
		if err != nil {
			reply(http.StatusBadRequest, map[string]any{"errors": []string{"bad plaintext"}})
			return
		}
		nonce := make([]byte, v.aead.NonceSize())
		_, _ = rand.Read(nonce)
		sealed := v.aead.Seal(nonce, nonce, plaintext, aad)
		reply(http.StatusOK, map[string]any{"data": map[string]string{"ciphertext": "vault:v1:" + base64.StdEncoding.EncodeToString(sealed)}})
	case "/v1/transit/decrypt/cactusfs", "/v1/kms/prod/decrypt/cactusfs":
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(in["ciphertext"], "vault:v1:"))
		if err != nil || len(raw) < v.aead.NonceSize() {
			reply(http.StatusBadRequest, map[string]any{"errors": []string{"invalid ciphertext"}})
			return
		}
		plaintext, err := v.aead.Open(nil, raw[:v.aead.NonceSize()], raw[v.aead.NonceSize():], aad)
		if err != nil {
			reply(http.StatusBadRequest, map[string]any{"errors": []string{"cipher: message authentication failed"}})
			return
		}
		reply(http.StatusOK, map[string]any{"data": map[string]string{"plaintext": base64.StdEncoding.EncodeToString(plaintext)}})
	default:
		reply(http.StatusNotFound, map[string]any{"errors": []string{"no handler for route"}})
	}
}

// serve starts the stand-in over TLS and returns a configuration that
// reaches it.
func serve(t *testing.T, v *transit) vault.Config {
	t.Helper()
	server := httptest.NewTLSServer(v)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	writeFile(t, ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	token := filepath.Join(dir, "token")
	writeFile(t, token, []byte(v.token+"\n"))
	return vault.Config{Address: server.URL, Key: "cactusfs", TokenFile: token, CACertFile: ca, Entropy: rand.Reader}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(filepath.Base(path), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWrapAndUnwrap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	v := newTransit(t)
	cfg := serve(t, v)
	kms, err := vault.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if kms.KeyID() != "vault:transit/cactusfs" {
		t.Errorf("KeyID = %q", kms.KeyID())
	}

	aad := []byte("object 42")
	key, err := kms.GenerateDataKey(ctx, aad)
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	if len(key.Plaintext) != 32 || bytes.Contains(key.Wrapped, key.Plaintext) || !bytes.HasPrefix(key.Wrapped, []byte("vault:v1:")) {
		t.Fatalf("data key: %d bytes, wrapped %q", len(key.Plaintext), key.Wrapped)
	}
	other, _ := kms.GenerateDataKey(ctx, aad)
	if bytes.Equal(other.Plaintext, key.Plaintext) {
		t.Error("two data keys are identical")
	}
	back, err := kms.UnwrapDataKey(ctx, key.Wrapped, aad)
	if err != nil || !bytes.Equal(back, key.Plaintext) {
		t.Fatalf("UnwrapDataKey: %v", err)
	}
	// A wrapped key cannot be moved to another owner, or altered.
	if _, err := kms.UnwrapDataKey(ctx, key.Wrapped, []byte("object 43")); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("unwrap with other associated data = %v, want %v", err, domain.ErrKeyUnavailable)
	}
	tampered := bytes.Clone(key.Wrapped)
	tampered[len(tampered)-3] ^= 1
	if _, err := kms.UnwrapDataKey(ctx, tampered, aad); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("unwrap of a tampered key = %v", err)
	}
	if _, err := kms.UnwrapDataKey(ctx, []byte("garbage"), aad); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("unwrap of garbage = %v", err)
	}

	// The token is sent in its header and nowhere else, and re-read at
	// every use: a renewed token works without a restart.
	v.mu.Lock()
	for _, r := range v.requests {
		if r.Header.Get("X-Vault-Token") != v.token || strings.Contains(r.URL.String(), v.token) {
			t.Error("the token was not sent as a header, or leaked into the URL")
		}
	}
	renewed := v.token + "-renewed"
	v.token = renewed
	v.mu.Unlock()
	if _, err := kms.GenerateDataKey(ctx, aad); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("with an old token = %v, want %v", err, domain.ErrKeyUnavailable)
	}
	writeFile(t, cfg.TokenFile, []byte(renewed))
	if _, err := kms.GenerateDataKey(ctx, aad); err != nil {
		t.Errorf("after the token file was renewed: %v", err)
	}

	// Vault's failures are key-unavailable errors that say what Vault said
	// and never contain the token.
	v.mu.Lock()
	v.fail = http.StatusServiceUnavailable
	v.mu.Unlock()
	_, err = kms.UnwrapDataKey(ctx, key.Wrapped, aad)
	if !errors.Is(err, domain.ErrKeyUnavailable) || !strings.Contains(err.Error(), "sealed") || strings.Contains(err.Error(), renewed) {
		t.Errorf("with Vault failing = %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := kms.GenerateDataKey(cancelled, aad); !errors.Is(err, context.Canceled) {
		t.Errorf("with a cancelled context = %v", err)
	}
}

// TestOpenRefusesWhatWouldNotBeSafe: plain HTTP, a wrong token, a key that
// does not exist, a key type that does not bind associated data, a
// certificate that is not trusted - each stops the node at start.
func TestOpenRefusesWhatWouldNotBeSafe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	v := newTransit(t)
	good := serve(t, v)

	change := func(fn func(c *vault.Config)) vault.Config {
		c := good
		fn(&c)
		return c
	}
	wrongToken := filepath.Join(t.TempDir(), "token")
	writeFile(t, wrongToken, []byte("not the token"))
	emptyCA := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, emptyCA, []byte("no certificate here"))

	invalid := map[string]vault.Config{
		"plain http":             change(func(c *vault.Config) { c.Address = "http://vault.internal:8200" }),
		"plain http to loopback": change(func(c *vault.Config) { c.Address = "http://127.0.0.1:8200" }),
		"plain http to a name, allowed": change(func(c *vault.Config) {
			c.Address, c.AllowPlaintextLoopback = "http://localhost:8200", true
		}),
		"no scheme":                     change(func(c *vault.Config) { c.Address = "vault:8200" }),
		"another scheme":                change(func(c *vault.Config) { c.Address = "ftp://vault:8200" }),
		"no key":                        change(func(c *vault.Config) { c.Key = "" }),
		"key with a path":               change(func(c *vault.Config) { c.Key = "../sys/seal" }),
		"no token file":                 change(func(c *vault.Config) { c.TokenFile = "" }),
		"CA file without a certificate": change(func(c *vault.Config) { c.CACertFile = emptyCA }),
		"missing CA file":               change(func(c *vault.Config) { c.CACertFile = filepath.Join(t.TempDir(), "absent.pem") }),
	}
	for name, cfg := range invalid {
		if _, err := vault.Open(ctx, cfg); err == nil || errors.Is(err, domain.ErrKeyUnavailable) {
			t.Errorf("%s: Open = %v, want a configuration error", name, err)
		}
	}
	unavailable := map[string]vault.Config{
		"wrong token":           change(func(c *vault.Config) { c.TokenFile = wrongToken }),
		"missing token file":    change(func(c *vault.Config) { c.TokenFile = filepath.Join(t.TempDir(), "absent") }),
		"unknown key":           change(func(c *vault.Config) { c.Key = "nosuchkey" }),
		"unknown mount":         change(func(c *vault.Config) { c.Mount = "elsewhere" }),
		"untrusted certificate": change(func(c *vault.Config) { c.CACertFile = "" }),
		"wrong server name":     change(func(c *vault.Config) { c.ServerName = "another.example" }),
		"nothing listening":     change(func(c *vault.Config) { c.Address = "https://127.0.0.1:1" }),
	}
	for name, cfg := range unavailable {
		if _, err := vault.Open(ctx, cfg); !errors.Is(err, domain.ErrKeyUnavailable) {
			t.Errorf("%s: Open = %v, want %v", name, err, domain.ErrKeyUnavailable)
		}
	}

	// A Transit key that ignores associated data would let a wrapped key
	// be moved from one object to another.
	loose := newTransit(t)
	loose.ignoreAAD = true
	if _, err := vault.Open(ctx, serve(t, loose)); !errors.Is(err, domain.ErrInvalidArgument) || !strings.Contains(err.Error(), "associated data") {
		t.Errorf("Open with a key that does not bind associated data = %v", err)
	}

	// Another mount, and a namespace, are passed on.
	custom := newTransit(t)
	cfg := serve(t, custom)
	cfg.Mount, cfg.Namespace = "/kms/prod/", "team-a"
	kms, err := vault.Open(ctx, cfg)
	if err != nil || kms.KeyID() != "vault:team-a:kms/prod/cactusfs" {
		t.Fatalf("Open with another mount: %v", err)
	}
	custom.mu.Lock()
	defer custom.mu.Unlock()
	if r := custom.requests[0]; r.Header.Get("X-Vault-Namespace") != "team-a" || !strings.HasPrefix(r.URL.Path, "/v1/kms/prod/") {
		t.Errorf("request went to %s with namespace %q", r.URL.Path, r.Header.Get("X-Vault-Namespace"))
	}
}

// TestTokenGoesNowhereElse: Vault's address is where the token is sent, and
// nowhere else - not to where a redirect points, and not through a proxy
// named in the environment.
func TestTokenGoesNowhereElse(t *testing.T) {
	ctx := context.Background()
	v := newTransit(t)
	cfg := serve(t, v)

	// Somebody else's server, which would see the token if it were followed
	// or used as a proxy.
	var leaked sync.Map
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("X-Vault-Token"), true)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"ciphertext":"vault:v1:AAAA","plaintext":"AAAA"}}`))
	}))
	t.Cleanup(elsewhere.Close)
	t.Setenv("HTTPS_PROXY", elsewhere.URL)
	t.Setenv("HTTP_PROXY", elsewhere.URL)
	t.Setenv("NO_PROXY", "")

	kms, err := vault.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open with a proxy in the environment: %v", err)
	}
	// Vault now answers every request with a redirect to the other server.
	redirecting := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/transit/encrypt/cactusfs", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirecting.Close)
	redirected := cfg
	redirected.Address = redirecting.URL
	redirected.CACertFile = filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, redirected.CACertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: redirecting.Certificate().Raw}))
	if _, err := vault.Open(ctx, redirected); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("Open against a Vault that redirects = %v, want %v", err, domain.ErrKeyUnavailable)
	}
	if _, err := kms.GenerateDataKey(ctx, []byte("x")); err != nil {
		t.Errorf("GenerateDataKey with a proxy in the environment: %v", err)
	}
	leaked.Range(func(token, _ any) bool {
		t.Errorf("a request reached another server, with token header %q", token)
		return true
	})
}

// TestOnlyARefusalProvesTheBinding: at start the adapter checks that a key
// wrapped for one owner cannot be unwrapped for another. Vault being
// unreachable for that check is not the proof - only Vault saying no is.
func TestOnlyARefusalProvesTheBinding(t *testing.T) {
	t.Parallel()
	v := newTransit(t)
	v.ignoreAAD = true
	cfg := serve(t, v)
	// The third request - the unwrap for another owner - meets a Vault
	// that has just gone away.
	var calls int
	var mu sync.Mutex
	gate := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n >= 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"errors":["vault is sealed"]}`))
			return
		}
		v.ServeHTTP(w, r)
	}))
	t.Cleanup(gate.Close)
	cfg.Address = gate.URL
	cfg.CACertFile = filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, cfg.CACertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gate.Certificate().Raw}))
	if _, err := vault.Open(context.Background(), cfg); err == nil {
		t.Fatal("Open accepted a key whose binding of associated data it could not check")
	}
}
