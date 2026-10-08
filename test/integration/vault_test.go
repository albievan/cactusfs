//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/albievan/cactusfs/internal/adapters/kms/vault"
	"github.com/albievan/cactusfs/internal/domain"
)

// vaultImage is HashiCorp Vault, pinned by digest. It is an external service
// the test talks to over its HTTP API; nothing of it is linked or shipped.
const vaultImage = "hashicorp/vault:1.20@sha256:268bb80aa9c6d13d65fcfa05c0c268caca068952240a8087291a6ce0b66e3a10"

func randomToken(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

func writeFileIn(t *testing.T, path string, data []byte) {
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

// vaultServer is a Vault in development mode with TLS, a Transit engine, an
// AEAD key, and a token that may do nothing but encrypt and decrypt with it.
type vaultServer struct {
	address   string
	caFile    string
	tokenFile string
	rootToken string
	client    *http.Client
}

func startVault(ctx context.Context, t *testing.T) *vaultServer {
	t.Helper()
	rootToken := randomToken(t)
	ctr, err := testcontainers.Run(ctx, vaultImage,
		testcontainers.WithCmd("server", "-dev", "-dev-tls", "-dev-tls-cert-dir=/tmp",
			"-dev-listen-address=0.0.0.0:8200", "-dev-root-token-id="+rootToken),
		testcontainers.WithExposedPorts("8200/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("8200/tcp").WithStartupTimeout(2*time.Minute)),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start Vault: %v", err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := ctr.MappedPort(ctx, "8200/tcp")
	if err != nil {
		t.Fatal(err)
	}
	var caPEM []byte
	for attempt := 0; ; attempt++ {
		reader, err := ctr.CopyFileFromContainer(ctx, "/tmp/vault-ca.pem")
		if err == nil {
			caPEM, err = io.ReadAll(reader)
			_ = reader.Close()
		}
		if err == nil && len(caPEM) > 0 {
			break
		}
		if attempt > 50 {
			t.Fatalf("read Vault's CA certificate: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	dir := t.TempDir()
	v := &vaultServer{
		address: "https://" + net.JoinHostPort(host, port.Port()), rootToken: rootToken,
		caFile: filepath.Join(dir, "vault-ca.pem"), tokenFile: filepath.Join(dir, "token"),
	}
	writeFileIn(t, v.caFile, caPEM)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("Vault's CA file holds no certificate")
	}
	// The development certificate names localhost, whatever address the
	// container is reached at.
	v.client = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12},
	}}

	v.api(ctx, t, rootToken, http.MethodPost, "sys/mounts/transit", map[string]any{"type": "transit"}, nil)
	v.api(ctx, t, rootToken, http.MethodPost, "transit/keys/cactusfs", map[string]any{"type": "aes256-gcm96"}, nil)
	// A key type that cannot bind associated data, to be refused.
	v.api(ctx, t, rootToken, http.MethodPost, "transit/keys/nobinding", map[string]any{"type": "rsa-2048"}, nil)
	policy := `path "transit/encrypt/cactusfs" { capabilities = ["update"] }
path "transit/decrypt/cactusfs" { capabilities = ["update"] }
path "transit/encrypt/nobinding" { capabilities = ["update"] }
path "transit/decrypt/nobinding" { capabilities = ["update"] }`
	v.api(ctx, t, rootToken, http.MethodPut, "sys/policies/acl/cactusfs", map[string]any{"policy": policy}, nil)
	var created struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	v.api(ctx, t, rootToken, http.MethodPost, "auth/token/create", map[string]any{"policies": []string{"cactusfs"}, "no_default_policy": true}, &created)
	if created.Auth.ClientToken == "" {
		t.Fatal("Vault issued no token")
	}
	writeFileIn(t, v.tokenFile, []byte(created.Auth.ClientToken))
	return v
}

func (v *vaultServer) api(ctx context.Context, t *testing.T, token, method, path string, in, out any) {
	t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, method, v.address+"/v1/"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Vault-Token", token)
	resp, err := v.client.Do(req)
	if err != nil {
		t.Fatalf("Vault %s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("Vault %s %s: status %d: %s", method, path, resp.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("Vault %s %s: %v", method, path, err)
		}
	}
}

func (v *vaultServer) config(key string) vault.Config {
	return vault.Config{
		Address: v.address, Key: key, TokenFile: v.tokenFile, CACertFile: v.caFile, ServerName: "localhost", Entropy: rand.Reader,
	}
}

// TestVaultTransit: the KMS adapter against a real Vault - with a token that
// may only encrypt and decrypt with the one key - and then the product
// itself, started with --kms vault, storing and reading an object across a
// restart.
func TestVaultTransit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	v := startVault(ctx, t)

	kms, err := vault.Open(ctx, v.config("cactusfs"))
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	aad := []byte("bucket/object/version")
	key, err := kms.GenerateDataKey(ctx, aad)
	if err != nil || len(key.Plaintext) != 32 {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	back, err := kms.UnwrapDataKey(ctx, key.Wrapped, aad)
	if err != nil || !bytes.Equal(back, key.Plaintext) {
		t.Fatalf("UnwrapDataKey: %v", err)
	}
	if _, err := kms.UnwrapDataKey(ctx, key.Wrapped, []byte("another object")); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("Vault unwrapped a key for another owner than it was wrapped for: %v", err)
	}
	// A key that cannot bind associated data, a key that does not exist, a
	// token without the permission: refused at start.
	if _, err := vault.Open(ctx, v.config("nobinding")); err == nil {
		t.Error("a Transit key that does not bind associated data was accepted")
	}
	if _, err := vault.Open(ctx, v.config("absent")); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("a missing Transit key = %v", err)
	}

	// The product with its keys in Vault.
	bin, dir := buildBinary(t), t.TempDir()
	start := func() (*server, map[string]string) {
		cmd := startServerWith(ctx, t, bin, []string{
			"server", "--dev-plaintext", "--data", filepath.Join(dir, "d1"), "--data", filepath.Join(dir, "d2"),
			"--kms", "vault", "--vault-addr", v.address, "--vault-key", "cactusfs", "--vault-token-file", v.tokenFile,
			"--vault-ca-cert", v.caFile, "--vault-server-name", "localhost",
			"--admin-addr", "127.0.0.1:0", "--s3-addr", "127.0.0.1:0",
		})
		return cmd, waitForListening(t, cmd.logs)
	}
	first, addrs := start()
	shown := regexp.MustCompile(`Access key ID: (\S+)\s+Secret key: +(\S+)`).FindStringSubmatch(waitForCredentials(t, first))
	if shown == nil {
		t.Fatalf("no root credentials were shown: %q", first.stderr.String())
	}
	client := func(addr string) *s3.Client {
		return s3.NewFromConfig(aws.Config{
			Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider(shown[1], shown[2], ""),
		}, func(o *s3.Options) {
			o.BaseEndpoint = aws.String("http://" + addr)
			o.UsePathStyle = true
		})
	}
	data := make([]byte, 300_000)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	c := client(addrs["s3"])
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("vaulted")}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("vaulted"), Key: aws.String("k"), Body: bytes.NewReader(data)}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	first.stop(t)

	// Nothing on the drives or in the metadata is the object, or a key file.
	if matches, _ := filepath.Glob(filepath.Join(dir, "d1", "meta", "kms-local.json")); len(matches) != 0 {
		t.Error("a local key file was created although the keys are in Vault")
	}

	second, addrs := start()
	out, err := client(addrs["s3"]).GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("vaulted"), Key: aws.String("k")})
	if err != nil {
		t.Fatalf("GetObject after a restart: %v", err)
	}
	got, _ := io.ReadAll(out.Body)
	_ = out.Body.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("the object read back after a restart differs")
	}
	second.stop(t)
	if strings.Contains(first.stderr.String()+second.stderr.String(), v.rootToken) {
		t.Error("a Vault token was printed")
	}

	// With Vault unreachable the node does not start: there is no
	// fallback to anything else.
	failing := startServerWith(ctx, t, bin, []string{
		"server", "--dev-plaintext", "--data", filepath.Join(dir, "d1"), "--data", filepath.Join(dir, "d2"),
		"--kms", "vault", "--vault-addr", "https://127.0.0.1:1", "--vault-key", "cactusfs", "--vault-token-file", v.tokenFile,
		"--admin-addr", "127.0.0.1:0", "--s3-addr", "127.0.0.1:0",
	})
	if err := failing.cmd.Wait(); err == nil {
		t.Error("the node started without its KMS")
	}
	if !strings.Contains(failing.stderr.String(), "Vault cannot be reached") {
		t.Errorf("stderr = %q", failing.stderr.String())
	}
}

// waitForCredentials returns what the server has written to standard error
// once the generated root credentials are in it.
func waitForCredentials(t *testing.T, s *server) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if text := s.stderr.String(); strings.Contains(text, "Secret key:") {
			return text
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Sprint(s.stderr.String())
}
