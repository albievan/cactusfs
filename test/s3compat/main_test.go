//go:build s3compat

package s3compat_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

const (
	modulePath = "github.com/albievan/cactusfs"
	region     = "us-east-1"
	// domain is the S3 domain the server is started with, for
	// virtual-hosted-style requests. The test client resolves every name
	// under it to the server.
	domain = "s3.cactus.test"
)

// suite is the running server and how to reach it.
type suite struct {
	endpoint string // https://127.0.0.1:<port>
	s3Addr   string
	// adminAddr is where the management API listens; certFile is the
	// certificate authority clients are to trust, as a file, for tools
	// that are told by a path.
	adminAddr string
	certFile  string
	accessKey string
	secretKey string
	pool      *x509.CertPool
}

var target suite

// TestMain builds the cactusfs binary, starts it over TLS exactly as an
// operator would, runs the suite against it with the AWS SDK's default
// options, and stops it.
func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "s3compat:", err)
		code = 1
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dir, err := os.MkdirTemp("", "cactusfs-s3compat-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	bin := filepath.Join(dir, "cactusfs")
	build := exec.CommandContext(ctx, "go", "build", "-o", bin, modulePath+"/cmd/cactusfs")
	build.Env = append(build.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("go build: %w\n%s", err, out)
	}

	certFile, keyFile, pool, err := writeCertificate(dir)
	if err != nil {
		return 0, err
	}
	target.pool, target.certFile = pool, certFile
	if target.accessKey, target.secretKey, err = randomCredentials(); err != nil {
		return 0, err
	}

	// The time limit above is for building and starting. The server then
	// runs for as long as the suite does, whose own limit is that of
	// "go test".
	serving, stop := context.WithCancel(context.Background())
	defer stop()
	server := exec.CommandContext(serving, bin, "server",
		// Four drives: objects are erasure-coded, as in a deployment.
		"--data", filepath.Join(dir, "drive1"), "--data", filepath.Join(dir, "drive2"),
		"--data", filepath.Join(dir, "drive3"), "--data", filepath.Join(dir, "drive4"), "--kms", "local",
		"--tls-cert", certFile, "--tls-key", keyFile,
		"--admin-addr", "127.0.0.1:0", "--s3-addr", "127.0.0.1:0", "--s3-domain", domain)
	server.Env = append(server.Environ(),
		"CACTUSFS_KMS_PASSPHRASE=s3 compatibility suite",
		"CACTUSFS_ROOT_ACCESS_KEY="+target.accessKey,
		"CACTUSFS_ROOT_SECRET_KEY="+target.secretKey,
	)
	var stderr bytes.Buffer
	server.Stderr = &stderr
	stdout, err := server.StdoutPipe()
	if err != nil {
		return 0, err
	}
	if err := server.Start(); err != nil {
		return 0, err
	}
	defer func() { _ = server.Process.Kill() }()

	logs := bufio.NewScanner(stdout)
	for logs.Scan() {
		var entry struct {
			Msg      string `json:"msg"`
			Listener string `json:"listener"`
			Addr     string `json:"addr"`
		}
		if json.Unmarshal(logs.Bytes(), &entry) != nil || entry.Msg != "listening" {
			continue
		}
		switch entry.Listener {
		case "s3":
			target.s3Addr = entry.Addr
		case "admin":
			target.adminAddr = entry.Addr
		}
		if target.s3Addr != "" && target.adminAddr != "" {
			break
		}
	}
	if target.s3Addr == "" || target.adminAddr == "" {
		return 0, fmt.Errorf("server did not start: %s", stderr.String())
	}
	target.endpoint = "https://" + target.s3Addr
	go func() {
		for logs.Scan() {
		}
	}()

	code := m.Run()

	if err := server.Process.Signal(syscall.SIGTERM); err != nil {
		return code, err
	}
	if err := server.Wait(); err != nil {
		return code, fmt.Errorf("server did not stop cleanly: %w\n%s", err, stderr.String())
	}
	return code, nil
}

// randomCredentials makes up root credentials for this run.
func randomCredentials() (accessKey, secretKey string, err error) {
	raw := make([]byte, 30)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	return "COMPAT" + strings.ToUpper(hex.EncodeToString(raw[:7])), hex.EncodeToString(raw[7:]), nil
}

// writeCertificate creates a throw-away certificate authority and a server
// certificate for the loopback address and the suite's S3 domain.
func writeCertificate(dir string) (certFile, keyFile string, pool *x509.CertPool, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cactusfs-s3compat"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{domain, "*." + domain},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", "", nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", "", nil, err
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile("cert.pem", certPEM, 0o600); err != nil {
		return "", "", nil, err
	}
	if err := root.WriteFile("key.pem", keyPEM, 0o600); err != nil {
		return "", "", nil, err
	}
	pool = x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		return "", "", nil, errors.New("certificate not added to pool")
	}
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), pool, nil
}

// httpClient trusts the suite's certificate authority and resolves every
// host name to the server, which is how virtual-hosted-style names reach it
// without DNS.
func httpClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout: 2 * time.Minute,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: target.pool, MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, target.s3Addr)
			},
		},
	}
}

// clientOptions vary how a client addresses and authenticates.
type clientOptions struct {
	endpoint  string
	pathStyle bool
	secretKey string
	anonymous bool
}

// newClient returns an S3 client that behaves as one built with the SDK's
// config loader does by default: flexible checksums are calculated and
// validated whenever an operation supports them, which over TLS means
// aws-chunked uploads with a trailing CRC32. The configuration is spelled out
// instead of loaded so that the suite never reads the developer's own AWS
// configuration files or environment.
func newClient(opts clientOptions) *s3.Client {
	cfg := aws.Config{
		Region:                     region,
		HTTPClient:                 httpClient(),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenSupported,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenSupported,
	}
	switch {
	case opts.anonymous:
		cfg.Credentials = aws.AnonymousCredentials{}
	case opts.secretKey != "":
		cfg.Credentials = credentials.NewStaticCredentialsProvider(target.accessKey, opts.secretKey, "")
	default:
		cfg.Credentials = credentials.NewStaticCredentialsProvider(target.accessKey, target.secretKey, "")
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(opts.endpoint)
		o.UsePathStyle = opts.pathStyle
	})
}

// client is the default client: path style against the loopback endpoint.
func client() *s3.Client {
	return newClient(clientOptions{endpoint: target.endpoint, pathStyle: true})
}

// errorCode returns the S3 error code of err, or "" when it is not an API
// error.
func errorCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// newBucket creates a bucket named after the test and removes it, with its
// objects, when the test ends.
func newBucket(t *testing.T) string {
	t.Helper()
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "compat-" + hex.EncodeToString(suffix)
	c := client()
	if _, err := c.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(name)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		// Every version and every delete marker goes: a bucket that
		// kept versions is empty only then.
		pages := s3.NewListObjectVersionsPaginator(c, &s3.ListObjectVersionsInput{Bucket: aws.String(name)})
		for pages.HasMorePages() {
			page, err := pages.NextPage(ctx)
			if err != nil {
				return
			}
			for _, version := range page.Versions {
				_, _ = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(name), Key: version.Key, VersionId: version.VersionId})
			}
			for _, marker := range page.DeleteMarkers {
				_, _ = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(name), Key: marker.Key, VersionId: marker.VersionId})
			}
		}
		_, _ = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(name)})
	})
	return name
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}
