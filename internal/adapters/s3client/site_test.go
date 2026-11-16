package s3client_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/egress"
	"github.com/albievan/cactusfs/internal/adapters/s3api"
	"github.com/albievan/cactusfs/internal/adapters/s3client"
	"github.com/albievan/cactusfs/internal/adapters/sealed"
	"github.com/albievan/cactusfs/internal/adapters/sigv4"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// The other deployment's clock, which the client's is set to: what the
// server judges by the time of a request is the same whatever day the test
// runs on.
var siteNow = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

const (
	// testHost is the name the target gives the other deployment:
	// httptest's certificate is made for it, and the test's resolver
	// gives loopback for it.
	testHost = "example.com"
	// source is the ID of the deployment that sends.
	source = "site-a"
	// targetName names the target of every test.
	targetName = "site-b"
)

// names is a resolver of a test.
type names map[string][]netip.Addr

// LookupNetIP implements egress.Resolver.
func (n names) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if addrs, ok := n[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// seen is a request as it arrived at the other deployment.
type seen struct {
	method string
	path   string
	query  string
	header http.Header
}

// site is the other deployment: the real S3 handler over in-memory stores,
// served over TLS, behind a recorder. Its replica buckets are made so by
// its replica-mode use case, and every answer - the questions of a source
// among them - is the handler's own. The recorder only records, and may
// change a request before the handler sees it (tamper).
type site struct {
	t           *testing.T
	server      *httptest.Server
	real        http.Handler
	replication *app.ReplicationService
	keyID       string
	key         []byte

	mu       sync.Mutex
	requests []seen
	// scratch says that the bucket etagOf puts into was made.
	scratch bool
	tamper  func(*http.Request)
	// script, when set, answers instead of the handler: the signature
	// of the request is verified by verifySigned first.
	script http.HandlerFunc
}

// newSite starts the other deployment.
func newSite(t *testing.T) *site {
	t.Helper()
	ctx := context.Background()
	kms, err := porttest.NewKMS("site-b:1")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	clock := porttest.NewClock(siteNow)
	meta := porttest.NewMemMetadata()
	meta.Now = clock.Now
	iamStore := porttest.NewMemIAM()
	meta.Audit = iamStore
	auditor := app.NewAuditor(iamStore, clock, logger)
	authz := app.NewAuthorizer(logger, clock, auditor).WithBucketPolicies(meta).WithSubBuckets(meta)
	identities := app.NewIdentityService(meta, kms, clock, rand.Reader).WithIAM(iamStore, auditor)
	root, err := identities.EnsureRoot(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	drive := porttest.NewMemShardStore("drive-a")
	topology, err := cluster.NewTopology([]string{drive.DriveID()})
	if err != nil {
		t.Fatal(err)
	}
	drives, err := app.FixedDrives(topology, []ports.ShardStore{drive})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := app.NewObjectService(app.ObjectServiceConfig{
		Index: meta, Buckets: meta, Uploads: meta, Drives: drives, Codec: porttest.NewCodec(), KMS: kms,
		Authz: authz, Clock: clock, Entropy: rand.Reader, Logger: logger, Auditor: auditor,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &site{
		t: t, keyID: root.AccessKeyID, key: []byte(root.SecretKey),
		replication: app.NewReplicationService(app.ReplicationServiceConfig{Buckets: meta, Store: meta, Authz: authz, Auditor: auditor}),
		real: s3api.NewHandler(s3api.Config{
			Buckets: app.NewBucketService(meta, authz, clock), Objects: objects, Identities: identities,
			Clock: clock, Logger: logger,
		}),
	}
	s.server = httptest.NewUnstartedServer(s)
	// What a test makes fail on purpose - a body cut short, a client that
	// gave up - the server would log.
	s.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.server.StartTLS()
	t.Cleanup(s.server.Close)
	return s
}

// ServeHTTP records the request, changes it as the test says, and hands it
// to the handler.
func (s *site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, seen{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone()})
	tamper, script := s.tamper, s.script
	s.mu.Unlock()
	if tamper != nil {
		tamper(r)
	}
	if script != nil {
		if err := verifySigned(r, s.keyID, s.key); err != nil {
			s.t.Errorf("%s %s: %v", r.Method, r.URL.Path, err)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		script(w, r)
		return
	}
	s.real.ServeHTTP(w, r)
}

// seen returns the requests that arrived so far.
func (s *site) seen() []seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// set changes the recorder under its lock.
func (s *site) set(change func(s *site)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(s)
}

// endpoint returns the origin a target names the site by.
func (s *site) endpoint() string {
	_, port, err := net.SplitHostPort(strings.TrimPrefix(s.server.URL, "https://"))
	if err != nil {
		s.t.Fatal(err)
	}
	return "https://" + testHost + ":" + port
}

// egressClient returns an egress client that trusts the site's certificate,
// resolves testHost to loopback and admits loopback; timeout is its response
// timeout, zero for the default.
func (s *site) egressClient(timeout time.Duration) *egress.Client {
	s.t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(s.server.Certificate())
	client, _, err := egress.NewClient(egress.Policy{
		Allow:           []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
		RootCAs:         pool,
		ResponseTimeout: timeout,
		Resolver:        names{testHost: {netip.MustParseAddr("127.0.0.1")}},
	})
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(client.CloseIdleConnections)
	return client
}

// target returns the target of the site, its secret - secret, or the
// site's root key when that is nil - sealed through kms as the target use
// case seals it.
func (s *site) target(kms ports.KMS, secret []byte) delivery.Target {
	s.t.Helper()
	if secret == nil {
		secret = s.key
	}
	return delivery.Target{
		Name: targetName, Kind: delivery.KindS3, Endpoint: s.endpoint(), Region: s3api.DefaultRegion,
		Buckets: []string{"*"},
		Auth:    delivery.Auth{Mode: delivery.AuthSigV4, ID: s.keyID},
		Secret:  seal(s.t, kms, secret, delivery.SecretAAD(targetName)),
	}
}

// client returns a client of the site through an egress client of the
// timeout given (zero for the default).
func (s *site) client(timeout time.Duration) *s3client.Client {
	s.t.Helper()
	return s.clientAt(timeout, siteNow)
}

// clientAt is client with a clock that says now: the site's clock is
// siteNow, so the two differ by siteNow - now.
func (s *site) clientAt(timeout time.Duration, now time.Time) *s3client.Client {
	s.t.Helper()
	kms := newKMS(s.t)
	c, err := s3client.New(s.target(kms, nil), sealed.KMS{KMS: kms}, s.egressClient(timeout),
		s3client.Options{Source: source, Version: "1.2.3-test", Clock: porttest.NewClock(now)})
	if err != nil {
		s.t.Fatal(err)
	}
	return c
}

// rootReply is what the site answered its root: the status and the headers.
type rootReply struct {
	StatusCode int
	Header     http.Header
}

// root sends a request as the site's root, signed by sigv4.SignRequest, and
// returns the answer and its body. It goes through the recorder like any
// request.
func (s *site) root(method, path string, header http.Header, body []byte) (rootReply, []byte) {
	s.t.Helper()
	req, err := http.NewRequestWithContext(s.t.Context(), method, s.server.URL+path, bytes.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	for name, values := range header {
		req.Header[name] = values
	}
	sum := sha256.Sum256(body)
	if err := sigv4.SignRequest(req, s.keyID, s.key, s3api.DefaultRegion, hex.EncodeToString(sum[:]), siteNow); err != nil {
		s.t.Fatal(err)
	}
	resp, err := s.server.Client().Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	return rootReply{StatusCode: resp.StatusCode, Header: resp.Header}, data
}

// createBucket makes a bucket that keeps versions - with Object Lock when
// lock is set - and makes it the replica of source with the site's
// replica-mode use case.
func (s *site) createBucket(name string, lock bool) {
	s.t.Helper()
	s.createPlain(name, lock)
	if _, err := s.replication.SetReplicaMode(s.t.Context(), porttest.Root(), name, source); err != nil {
		s.t.Fatalf("make %s a replica: %v", name, err)
	}
}

// createPlain makes a bucket that keeps versions - with Object Lock when
// lock is set - and is no replica.
func (s *site) createPlain(name string, lock bool) {
	s.t.Helper()
	header := http.Header{}
	if lock {
		header.Set("X-Amz-Bucket-Object-Lock-Enabled", "true")
	}
	if resp, body := s.root(http.MethodPut, "/"+name, header, nil); resp.StatusCode != http.StatusOK {
		s.t.Fatalf("create %s = %d %s", name, resp.StatusCode, body)
	}
	if !lock {
		doc := []byte(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`)
		if resp, body := s.root(http.MethodPut, "/"+name+"?versioning", nil, doc); resp.StatusCode != http.StatusOK {
			s.t.Fatalf("versioning of %s = %d %s", name, resp.StatusCode, body)
		}
	}
}

// seal seals value through kms as the target use case does.
func seal(t *testing.T, kms ports.KMS, value, aad []byte) iam.SealedSecret {
	t.Helper()
	key, err := kms.GenerateDataKey(t.Context(), aad)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Plaintext)
	s, err := iam.Seal(key.Plaintext, rand.Reader, value, aad)
	if err != nil {
		t.Fatal(err)
	}
	s.KMSKeyID, s.WrappedKey = kms.KeyID(), key.Wrapped
	return s
}

func newKMS(t *testing.T) *porttest.KMS {
	t.Helper()
	kms, err := porttest.NewKMS("site-a:1")
	if err != nil {
		t.Fatal(err)
	}
	return kms
}

// verifySigned verifies the signature of a request with the primitives the
// server verifies with (package sigv4): every header the S3 API requires
// signed and every replica header among the signed ones, a signed payload,
// and the signature itself. It reads nothing of the body.
func verifySigned(r *http.Request, keyID string, secret []byte) error {
	authorization := r.Header.Get("Authorization")
	rest, ok := strings.CutPrefix(authorization, sigv4.Algorithm+" ")
	if !ok {
		return fmt.Errorf("not signed with %s", sigv4.Algorithm)
	}
	fields := map[string]string{}
	for part := range strings.SplitSeq(rest, ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		fields[name] = value
	}
	credential := strings.Split(fields["Credential"], "/")
	if len(credential) != 5 || credential[0] != keyID || credential[2] != s3api.DefaultRegion {
		return fmt.Errorf("credential %q", fields["Credential"])
	}
	signed := strings.Split(fields["SignedHeaders"], ";")
	if !slices.IsSorted(signed) || !slices.Contains(signed, "host") {
		return fmt.Errorf("signed headers %q", fields["SignedHeaders"])
	}
	for name := range r.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" || lower == "user-agent" || lower == "accept-encoding" || lower == "content-length" {
			continue
		}
		if !slices.Contains(signed, lower) {
			return fmt.Errorf("header %s is not signed", lower)
		}
	}
	payload := r.Header.Get(sigv4.HeaderContentSHA256)
	if payload == sigv4.UnsignedPayload || strings.HasPrefix(payload, "STREAMING-UNSIGNED") {
		return fmt.Errorf("the payload is not signed: %s", payload)
	}
	want := sigv4.Expected(secret, credential[1], credential[2], r.Header.Get(sigv4.HeaderAmzDate),
		sigv4.CanonicalRequest(r, signed, fields["SignedHeaders"], payload))
	if want != fields["Signature"] {
		return fmt.Errorf("the signature does not verify")
	}
	return nil
}

// signedHeaders returns the names a request's signature covers.
func signedHeaders(header http.Header) []string {
	_, rest, _ := strings.Cut(header.Get("Authorization"), "SignedHeaders=")
	names, _, _ := strings.Cut(rest, ",")
	return strings.Split(names, ";")
}
