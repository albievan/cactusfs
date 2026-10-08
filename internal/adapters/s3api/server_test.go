package s3api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

var serverNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// testServer is the S3 handler over real use cases and in-memory stores.
type testServer struct {
	// iamStore, auditor and iam let a test make users and keys and read
	// the audit trail.
	iamStore *porttest.MemIAM
	auditor  *app.Auditor
	iam      *app.IAMService
	*httptest.Server
	t       *testing.T
	keyID   string
	key     []byte
	drive   *porttest.MemShardStore
	meta    *porttest.MemMetadata
	logs    *lockedBuffer
	seen    *observations
	objects *app.ObjectService
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type observations struct {
	mu   sync.Mutex
	list []Observation
}

func (o *observations) add(_ context.Context, obs Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.list = append(o.list, obs)
}

func (o *observations) last() Observation {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.list) == 0 {
		return Observation{}
	}
	return o.list[len(o.list)-1]
}

func newTestServer(t *testing.T, domainName string) *testServer {
	t.Helper()
	ctx := context.Background()

	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatalf("NewKMS: %v", err)
	}
	s := &testServer{
		t:     t,
		drive: porttest.NewMemShardStore("drive-a"),
		meta:  porttest.NewMemMetadata(),
		logs:  &lockedBuffer{},
		seen:  &observations{},
	}
	logger := slog.New(slog.NewJSONHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	clock := porttest.NewClock(serverNow)
	s.iamStore = porttest.NewMemIAM()
	s.auditor = app.NewAuditor(s.iamStore, clock, logger)
	authz := app.NewAuthorizer(logger, clock, s.auditor)
	s.iam = app.NewIAMService(app.IAMServiceConfig{
		Store: s.iamStore, Trail: s.iamStore, Authz: authz, Auditor: s.auditor, KMS: kms, Hasher: &porttest.Hasher{},
		Clock: clock, Entropy: rand.Reader,
	})

	identities := app.NewIdentityService(s.meta, kms, clock, rand.Reader).WithIAM(s.iamStore, s.auditor)
	generated, err := identities.EnsureRoot(ctx, nil)
	if err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	s.keyID, s.key = generated.AccessKeyID, []byte(generated.SecretKey)

	topology, err := cluster.NewTopology([]string{s.drive.DriveID()})
	if err != nil {
		t.Fatalf("NewTopology: %v", err)
	}
	drives, err := app.FixedDrives(topology, []ports.ShardStore{s.drive})
	if err != nil {
		t.Fatalf("FixedDrives: %v", err)
	}
	s.objects, err = app.NewObjectService(app.ObjectServiceConfig{
		Index: s.meta, Buckets: s.meta, Uploads: s.meta, Drives: drives, Codec: porttest.NewCodec(), KMS: kms,
		Authz: authz, Clock: clock, Entropy: rand.Reader, Logger: logger,
	})
	if err != nil {
		t.Fatalf("NewObjectService: %v", err)
	}
	handler := NewHandler(Config{
		Buckets: app.NewBucketService(s.meta, authz, clock), Objects: s.objects, Identities: identities,
		Clock: clock, Logger: logger, Domain: domainName, Observe: s.seen.add,
	})
	s.Server = httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return s
}

// call describes one request a test sends.
type call struct {
	method  string
	path    string // path and query, as on the request line
	host    string // Host header; the server's own when empty
	body    []byte
	headers map[string]string
	// payloadHash overrides x-amz-content-sha256; the SHA-256 of body when
	// empty.
	payloadHash string
	// unsigned sends the request without an Authorization header.
	unsigned bool
	// keyID, key and signedAt override the signing identity and time.
	keyID    string
	key      []byte
	signedAt time.Time
	// mutate changes the request after it has been signed.
	mutate func(r *http.Request)
}

type reply struct {
	status int
	header http.Header
	body   []byte
	// err is set when the transfer itself failed.
	err error
}

func (s *testServer) do(c call) reply {
	s.t.Helper()
	r, err := http.NewRequestWithContext(s.t.Context(), c.method, s.URL+c.path, bytes.NewReader(c.body))
	if err != nil {
		s.t.Fatalf("new request: %v", err)
	}
	r.Host = r.URL.Host
	if c.host != "" {
		r.Host = c.host
	}
	for name, value := range c.headers {
		r.Header.Set(name, value)
	}
	if !c.unsigned {
		keyID, key, signedAt := s.keyID, s.key, serverNow
		if c.keyID != "" {
			keyID = c.keyID
		}
		if c.key != nil {
			key = c.key
		}
		if !c.signedAt.IsZero() {
			signedAt = c.signedAt
		}
		payloadHash := c.payloadHash
		if payloadHash == "" {
			sum := sha256.Sum256(c.body)
			payloadHash = hex.EncodeToString(sum[:])
		}
		signRequest(r, keyID, key, signedAt, payloadHash)
	}
	if c.mutate != nil {
		c.mutate(r)
	}

	resp, err := s.Client().Do(r)
	if err != nil {
		return reply{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, header: resp.Header, body: body, err: err}
}

// signRequest signs r as an S3 client would: the host and every x-amz-* and
// content-md5 header are covered.
func signRequest(r *http.Request, keyID string, key []byte, signedAt time.Time, payloadHash string) {
	r.Header.Set(headerAmzDate, signedAt.UTC().Format(amzDateFormat))
	r.Header.Set(headerContentSHA, payloadHash)

	signed := []string{"host"}
	for name := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") || lower == "content-md5" || lower == "range" {
			signed = append(signed, lower)
		}
	}
	slices.Sort(signed)
	sig := signature{
		accessKeyID: keyID, scopeDate: signedAt.UTC().Format(scopeDateFormat), region: DefaultRegion,
		signedHeaders: signed, signedHeadersRaw: strings.Join(signed, ";"),
	}
	sig.signatureHex = expectedSignature(key, sig, r.Header.Get(headerAmzDate), canonicalRequest(r, sig, payloadHash))
	r.Header.Set("Authorization", sigV4Algorithm+" Credential="+keyID+"/"+sig.scopeDate+"/"+sig.region+
		"/s3/aws4_request, SignedHeaders="+sig.signedHeadersRaw+", Signature="+sig.signatureHex)
}

// errorCode extracts the S3 error code of an error response.
func (s *testServer) errorCode(r reply) string {
	s.t.Helper()
	var e errorResponse
	if err := xml.Unmarshal(r.body, &e); err != nil {
		s.t.Fatalf("response is not an S3 error document (status %d): %q", r.status, r.body)
	}
	if e.RequestID == "" || e.RequestID != r.header.Get("X-Amz-Request-Id") {
		s.t.Errorf("error document request ID %q does not match the header %q", e.RequestID, r.header.Get("X-Amz-Request-Id"))
	}
	return e.Code
}

func (s *testServer) expectError(r reply, status int, code string) {
	s.t.Helper()
	if r.err != nil {
		s.t.Fatalf("request failed: %v", r.err)
	}
	if got := s.errorCode(r); r.status != status || got != code {
		s.t.Errorf("response = %d %s, want %d %s", r.status, got, status, code)
	}
}

func (s *testServer) mustStatus(r reply, status int) reply {
	s.t.Helper()
	if r.err != nil {
		s.t.Fatalf("request failed: %v", r.err)
	}
	if r.status != status {
		s.t.Fatalf("status = %d, want %d; body: %s", r.status, status, r.body)
	}
	return r
}

func (s *testServer) createBucket(name string) {
	s.t.Helper()
	s.mustStatus(s.do(call{method: http.MethodPut, path: "/" + name}), http.StatusOK)
}

func (s *testServer) putObject(path string, body []byte) reply {
	s.t.Helper()
	return s.mustStatus(s.do(call{method: http.MethodPut, path: path, body: body}), http.StatusOK)
}

// principalOf is used by tests that call use cases directly.
func (s *testServer) principal() iam.Principal {
	return iam.Principal{AccessKeyID: s.keyID, Root: true}
}
