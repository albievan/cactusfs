package adminapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/api/openapi"
	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/adapters/httpsession"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// testNow is the time of the test clock. It is far ahead of any day the
// tests will run on: the cookie jar of the HTTP client judges a session
// cookie's expiry by the real clock, and drops one that expired "yesterday".
var testNow = time.Date(2099, 10, 8, 9, 0, 0, 0, time.UTC)

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

// view is the cluster as the status use case is shown it.
type view struct {
	topology cluster.Topology
}

func (v view) Topology(context.Context) (cluster.Topology, error) { return v.topology, nil }

func (v view) Nodes(context.Context) ([]cluster.Node, error) {
	return []cluster.Node{{ID: "node-a", Roles: cluster.AllRoles(), Labels: cluster.Labels{Zone: "z1"}, MetadataVersion: 1}}, nil
}

// fixture is the management API over real use cases and in-memory stores.
type fixture struct {
	t *testing.T
	*httptest.Server
	store       *porttest.MemIAM
	clock       *porttest.Clock
	auditor     *app.Auditor
	iam         *app.IAMService
	identities  *app.IdentityService
	maintenance *app.Maintenance
	// meta is the metadata store, and access what counts the reads on
	// their way to it.
	meta   *porttest.MemMetadata
	access *app.AccessRecorder
	// index is what a search walks: the metadata store, which a test can
	// hold up; policies where it reads the policies of the buckets, which a
	// test can make slow.
	index    *heldIndex
	policies *slowPolicies
	logs     *lockedBuffer
	root     string // bearer credential of root
	seen     []adminapi.Observation
	seenMu   sync.Mutex
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	f := &fixture{t: t, store: porttest.NewMemIAM(), clock: porttest.NewClock(testNow), logs: &lockedBuffer{}}
	logger := slog.New(slog.NewJSONHandler(f.logs, nil))
	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}
	meta := porttest.NewMemMetadata()
	f.auditor = app.NewAuditor(f.store, f.clock, logger)
	authz := app.NewAuthorizer(logger, f.clock, f.auditor).WithBucketPolicies(meta)
	f.iam = app.NewIAMService(app.IAMServiceConfig{
		Store: f.store, Trail: f.store, Authz: authz, Auditor: f.auditor, KMS: kms, Hasher: &porttest.Hasher{},
		Clock: f.clock, Entropy: rand.Reader,
	})
	f.identities = app.NewIdentityService(meta, kms, f.clock, rand.Reader).WithIAM(f.store, f.auditor)
	generated, err := f.identities.EnsureRoot(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.root = generated.AccessKeyID + ":" + generated.SecretKey
	provider, err := app.NewLocalIdentityProvider(f.store, &porttest.Hasher{}, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sessions := app.NewSessionService(app.SessionServiceConfig{
		Sessions: f.store, Users: f.store, Provider: provider, Auditor: f.auditor, Clock: f.clock, Entropy: rand.Reader,
	})

	drive := porttest.NewMemShardStore("drive-a")
	topology, err := cluster.NewTopology([]string{drive.DriveID(), "drive-b"})
	if err != nil {
		t.Fatal(err)
	}
	single, err := cluster.NewTopology([]string{drive.DriveID()})
	if err != nil {
		t.Fatal(err)
	}
	drives, err := app.FixedDrives(single, []ports.ShardStore{drive})
	if err != nil {
		t.Fatal(err)
	}
	f.meta = meta
	f.access, err = app.NewAccessRecorder(app.AccessRecorderConfig{Store: meta, Node: "node-a", Entropy: rand.Reader, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := app.NewObjectService(app.ObjectServiceConfig{
		Index: meta, Buckets: meta, Uploads: meta, Drives: drives, Codec: porttest.NewCodec(), KMS: kms,
		Authz: authz, Clock: f.clock, Entropy: rand.Reader, Logger: logger,
		Access: f.access, AccessStore: meta,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.index = &heldIndex{ObjectIndex: meta}
	f.policies = &slowPolicies{next: meta, clock: f.clock}
	search, err := app.NewSearchService(app.SearchServiceConfig{Buckets: meta, Index: f.index, Policies: f.policies, Authz: authz, Clock: f.clock, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	f.maintenance = app.NewMaintenance("node-a", f.clock)
	clusterService := app.NewClusterService(app.ClusterServiceConfig{
		View: view{topology: topology}, Authz: authz, NodeID: "node-a", Leader: func() string { return "node-a" },
		Maintenance: f.maintenance,
		Probe:       func(_ context.Context, slot cluster.Slot) bool { return slot.DriveID == drive.DriveID() },
	})
	handler := adminapi.NewHandler(adminapi.Config{
		Buckets: app.NewBucketService(meta, authz, f.clock).WithAuditor(f.auditor), Objects: objects, Search: search, IAM: f.iam,
		Sessions: sessions, Cluster: clusterService, Idempotency: app.NewIdempotencyService(f.store, kms, f.clock, rand.Reader),
		Identities: f.identities, Clock: f.clock, Logger: logger,
		SpecYAML: openapi.AdminYAML(), SpecJSON: openapi.AdminJSON(),
		Observe: func(_ context.Context, o adminapi.Observation) {
			f.seenMu.Lock()
			defer f.seenMu.Unlock()
			f.seen = append(f.seen, o)
		},
	})
	f.Server = httptest.NewTLSServer(handler)
	t.Cleanup(f.Close)
	return f
}

// heldIndex is an index whose walks a test can hold up: while hold is set,
// a walk says on entered that it has begun and waits for release.
type heldIndex struct {
	ports.ObjectIndex
	hold atomic.Pointer[indexHold]
}

type indexHold struct {
	entered chan struct{}
	release chan struct{}
}

func (h *heldIndex) ScanObjects(ctx context.Context, bucketName, start string, visit func(object.Version) ports.ScanControl) error {
	if hold := h.hold.Load(); hold != nil {
		select {
		case hold.entered <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-hold.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return h.ObjectIndex.ScanObjects(ctx, bucketName, start, visit)
}

// slowPolicies reads the policies of the buckets as the metadata store does,
// and takes as long as a test says - by the clock of the test - for a page.
type slowPolicies struct {
	next  ports.BucketPolicies
	clock *porttest.Clock
	takes atomic.Int64
}

func (s *slowPolicies) BucketPolicies(ctx context.Context, from string, visit func(string, []byte) error) (string, error) {
	s.clock.Advance(time.Duration(s.takes.Load()))
	return s.next.BucketPolicies(ctx, from, visit)
}

// call is one request of a test.
type call struct {
	method string
	path   string
	// bearer is the API key sent; "" sends none.
	bearer string
	// body is sent as JSON unless raw is set.
	body    any
	raw     []byte
	headers map[string]string
	client  *http.Client
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

func (r reply) problem(t *testing.T) map[string]any {
	t.Helper()
	if got := r.header.Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("status %d has Content-Type %q, want application/problem+json; body %s", r.status, got, r.body)
	}
	var p map[string]any
	if err := json.Unmarshal(r.body, &p); err != nil {
		t.Fatalf("problem body: %v: %s", err, r.body)
	}
	for _, field := range []string{"type", "title", "status", "code", "request_id"} {
		if _, ok := p[field]; !ok {
			t.Errorf("problem has no %q: %s", field, r.body)
		}
	}
	if status, _ := p["status"].(float64); int(status) != r.status || p["request_id"] != r.header.Get("X-Request-Id") {
		t.Errorf("problem does not match its response: %s", r.body)
	}
	return p
}

func (r reply) decode(t *testing.T, into any) {
	t.Helper()
	if err := json.Unmarshal(r.body, into); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (f *fixture) do(c call) reply {
	f.t.Helper()
	var body io.Reader
	contentType := ""
	switch {
	case c.raw != nil:
		body = bytes.NewReader(c.raw)
	case c.body != nil:
		data, err := json.Marshal(c.body)
		if err != nil {
			f.t.Fatal(err)
		}
		body, contentType = bytes.NewReader(data), "application/json"
	}
	r, err := http.NewRequestWithContext(f.t.Context(), c.method, f.URL+adminapi.BasePath+c.path, body)
	if err != nil {
		f.t.Fatal(err)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if c.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	for name, value := range c.headers {
		r.Header.Set(name, value)
	}
	client := c.client
	if client == nil {
		client = f.Client()
	}
	resp, err := client.Do(r)
	if err != nil {
		f.t.Fatalf("%s %s: %v", c.method, c.path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatalf("%s %s: reading the answer: %v", c.method, c.path, err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: data}
}

// must sends a request as root and fails the test unless it answers want.
func (f *fixture) must(want int, c call) reply {
	f.t.Helper()
	if c.bearer == "" && c.client == nil {
		c.bearer = f.root
	}
	r := f.do(c)
	if r.status != want {
		f.t.Fatalf("%s %s = %d, want %d; body: %s", c.method, c.path, r.status, want, r.body)
	}
	return r
}

func (f *fixture) trail() []audit.Record {
	f.t.Helper()
	f.auditor.Flush(context.Background())
	records, err := f.store.ListAudit(context.Background(), 0, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return records
}

// userKey makes a user with a role and returns a bearer credential of theirs.
func (f *fixture) userKey(name, role string) string {
	f.t.Helper()
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{"name": name, "roles": []string{role}}})
	var created struct {
		Key struct {
			AccessKeyID string `json:"access_key_id"`
		} `json:"key"`
		SecretKey string `json:"secret_key"`
	}
	f.must(http.StatusCreated, call{method: "POST", path: "/users/" + name + "/keys", body: map[string]any{}}).decode(f.t, &created)
	return created.Key.AccessKeyID + ":" + created.SecretKey
}

// TestTheContractIsServed: the document that defines the API is part of it,
// needs no credential, and is the committed file.
func TestTheContractIsServed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	asYAML := f.do(call{method: "GET", path: "/openapi.yaml"})
	if asYAML.status != http.StatusOK || !bytes.Equal(asYAML.body, openapi.AdminYAML()) || asYAML.header.Get("Content-Type") != "application/yaml" {
		t.Errorf("openapi.yaml: status %d, type %q", asYAML.status, asYAML.header.Get("Content-Type"))
	}
	asJSON := f.do(call{method: "GET", path: "/openapi.json"})
	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	asJSON.decode(t, &doc)
	if asJSON.status != http.StatusOK || doc.OpenAPI != "3.0.3" || len(doc.Paths) < 20 {
		t.Errorf("openapi.json: status %d, version %q, %d paths", asJSON.status, doc.OpenAPI, len(doc.Paths))
	}
	// Every operation that changes something accepts an idempotency key.
	for path, operations := range doc.Paths {
		for method, raw := range operations {
			if method != "post" && method != "put" && method != "delete" {
				continue
			}
			encoded, _ := json.Marshal(raw)
			if !strings.Contains(string(encoded), "#/components/parameters/IdempotencyKey") {
				t.Errorf("%s %s does not accept Idempotency-Key", method, path)
			}
		}
	}
	missing := f.do(call{method: "GET", path: "/no-such-thing", bearer: f.root})
	if missing.status != http.StatusNotFound || missing.problem(t)["code"] != "NotFound" {
		t.Errorf("unknown endpoint = %d %s", missing.status, missing.body)
	}
}

// TestEveryRequestIsAuthenticated: without a credential that checks out,
// nothing but signing in and the contract answers, and every refusal is in
// the audit trail.
func TestEveryRequestIsAuthenticated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	alice := f.userKey("alice", iam.RoleReadOnly)
	id, secret, _ := strings.Cut(alice, ":")
	base := len(f.trail())

	// Every operation of the contract but the public ones.
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string            `json:"operationId"`
			Security    *[]map[string]any `json:"security"`
		} `json:"paths"`
	}
	f.do(call{method: "GET", path: "/openapi.json"}).decode(t, &doc)
	checked := 0
	for path, operations := range doc.Paths {
		for method, operation := range operations {
			if operation.Security != nil && len(*operation.Security) == 0 {
				continue
			}
			concrete := strings.NewReplacer("{bucket}", "some-bucket", "{name}", "somebody", "{access_key_id}", "AKID").Replace(path)
			if strings.Contains(path, "/object") && !strings.HasSuffix(path, "/objects") {
				concrete += "?key=k"
			}
			if path == "/search" {
				// As for a key: a request without what the contract
				// requires is turned away before anybody asks who sent it.
				concrete += "?q=k"
			}
			c := call{method: strings.ToUpper(method), path: concrete}
			if method == "post" || method == "put" {
				c.body = map[string]any{}
			}
			r := f.do(c)
			if r.status != http.StatusUnauthorized {
				t.Errorf("%s %s without a credential = %d, want 401", method, path, r.status)
				continue
			}
			if r.problem(t)["code"] != "Unauthenticated" || !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Bearer") {
				t.Errorf("%s %s: answer %s, WWW-Authenticate %q", method, path, r.body, r.header.Get("WWW-Authenticate"))
			}
			checked++
		}
	}
	if checked < 35 {
		t.Errorf("only %d operations were checked", checked)
	}

	for name, bearer := range map[string]string{
		"wrong secret":   id + ":" + strings.Repeat("x", len(secret)),
		"unknown key":    "AKIDNOSUCHKEY000000:" + secret,
		"no secret":      id,
		"empty secret":   id + ":",
		"secret of root": id + ":" + strings.SplitN(f.root, ":", 2)[1],
	} {
		if r := f.do(call{method: "GET", path: "/buckets", bearer: bearer}); r.status != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", name, r.status)
		}
	}
	if r := f.do(call{method: "GET", path: "/buckets", headers: map[string]string{"Authorization": "Basic " + alice}}); r.status != http.StatusUnauthorized {
		t.Errorf("Basic scheme = %d, want 401", r.status)
	}
	// The key works, and stops working the moment it is revoked.
	f.must(http.StatusOK, call{method: "GET", path: "/buckets", bearer: alice})
	f.must(http.StatusNoContent, call{method: "DELETE", path: "/keys/" + id})
	if r := f.do(call{method: "GET", path: "/buckets", bearer: alice}); r.status != http.StatusUnauthorized {
		t.Errorf("a revoked key = %d, want 401", r.status)
	}

	failures := 0
	for _, r := range f.trail()[base:] {
		if r.Action == "auth:Authenticate" && r.Outcome == audit.Failed {
			failures++
			if strings.Contains(fmt.Sprint(r), secret) {
				t.Fatal("a secret key is in the audit trail")
			}
		}
	}
	if failures == 0 {
		t.Error("failed authentications left nothing in the audit trail")
	}
	if strings.Contains(f.logs.String(), secret) {
		t.Fatal("a secret key is in the log")
	}
}

// TestAuthorizationIsTheUseCases: what a caller may do is decided by their
// policies, and a refusal is a problem that says nothing more.
func TestAuthorizationIsTheUseCases(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reader := f.userKey("reader", iam.RoleReadOnly)
	auditor := f.userKey("auditor", iam.RoleAuditor)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "reports"}})

	denied := []call{
		{method: "POST", path: "/buckets", body: map[string]any{"name": "mine"}},
		{method: "DELETE", path: "/buckets/reports"},
		{method: "PUT", path: "/buckets/reports/object?key=a", raw: []byte("x")},
		{method: "GET", path: "/users"},
		{method: "POST", path: "/users", body: map[string]any{"name": "eve", "roles": []string{iam.RoleClusterAdmin}}},
		{method: "PUT", path: "/users/reader", body: map[string]any{"roles": []string{iam.RoleClusterAdmin}, "groups": []string{}, "disabled": false}},
		{method: "POST", path: "/users/reader/keys", body: map[string]any{}},
		{method: "POST", path: "/policies", body: map[string]any{"name": "p", "document": map[string]any{}}},
		{method: "GET", path: "/audit"},
		{method: "GET", path: "/cluster"},
	}
	for _, c := range denied {
		c.bearer = reader
		r := f.do(c)
		if r.status != http.StatusForbidden {
			t.Errorf("%s %s as a read-only user = %d, want 403: %s", c.method, c.path, r.status, r.body)
			continue
		}
		if p := r.problem(t); p["code"] != "AccessDenied" || p["detail"] != nil {
			t.Errorf("%s %s: refusal = %s", c.method, c.path, r.body)
		}
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/reports/objects", bearer: reader})
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/reports/usage", bearer: reader})
	// An auditor reads everything about the deployment and changes nothing.
	for _, path := range []string{"/users", "/groups", "/roles", "/policies", "/audit", "/cluster", "/cluster/maintenance", "/users/reader/keys"} {
		f.must(http.StatusOK, call{method: "GET", path: path, bearer: auditor})
	}
	if r := f.do(call{method: "DELETE", path: "/users/reader", bearer: auditor}); r.status != http.StatusForbidden {
		t.Errorf("an auditor deleting a user = %d", r.status)
	}
}

func signIn(t *testing.T, f *fixture, user, password string) (*http.Client, string, reply) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := *f.Client()
	client.Jar = jar
	r := f.do(call{method: "POST", path: "/session", client: &client, body: map[string]any{"username": user, "password": password}})
	var session struct {
		CSRFToken string `json:"csrf_token"`
		User      string `json:"user"`
	}
	if r.status == http.StatusOK {
		r.decode(t, &session)
	}
	return &client, session.CSRFToken, r
}

// TestConsoleSession: signing in sets a cookie scripts cannot read, reading
// needs the cookie, changing something needs the CSRF token too, and signing
// out ends it.
func TestConsoleSession(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const password = "correct horse battery"
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{
		"name": "carol", "roles": []string{iam.RoleBucketAdmin}, "password": password,
	}})

	client, csrf, r := signIn(t, f, "carol", password)
	if r.status != http.StatusOK || csrf == "" {
		t.Fatalf("sign-in = %d %s", r.status, r.body)
	}
	cookie := r.header.Get("Set-Cookie")
	for _, attribute := range []string{httpsession.CookieName + "=", "HttpOnly", "Secure", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(cookie, attribute) {
			t.Errorf("session cookie %q lacks %s", cookie, attribute)
		}
	}
	if strings.Contains(cookie, "Domain=") || strings.Contains(string(r.body), strings.SplitN(strings.SplitN(cookie, "=", 2)[1], ";", 2)[0]) {
		t.Errorf("the cookie has a Domain, or the session token is in the body: %q", cookie)
	}

	var who struct{ Kind, Via, User string }
	f.must(http.StatusOK, call{method: "GET", path: "/session", client: client}).decode(t, &who)
	if who.Kind != "user" || who.Via != "session" || who.User != "carol" {
		t.Errorf("GET /session = %+v", who)
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets", client: client})
	// The cookie alone changes nothing: that is what another site could
	// make the browser send.
	create := call{method: "POST", path: "/buckets", client: client, body: map[string]any{"name": "carols"}}
	if r := f.do(create); r.status != http.StatusUnauthorized {
		t.Fatalf("a change without the CSRF token = %d, want 401", r.status)
	}
	create.headers = map[string]string{httpsession.CSRFHeader: "not-the-token"}
	if r := f.do(create); r.status != http.StatusUnauthorized {
		t.Fatalf("a change with a wrong CSRF token = %d, want 401", r.status)
	}
	create.headers = map[string]string{httpsession.CSRFHeader: csrf}
	f.must(http.StatusCreated, create)
	// Carol is a bucket administrator, not an administrator of users.
	if r := f.do(call{method: "GET", path: "/users", client: client}); r.status != http.StatusForbidden {
		t.Errorf("GET /users with carol's session = %d, want 403", r.status)
	}

	out := f.do(call{method: "DELETE", path: "/session", client: client, headers: map[string]string{httpsession.CSRFHeader: csrf}})
	if out.status != http.StatusNoContent || !strings.Contains(out.header.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("sign-out = %d, Set-Cookie %q", out.status, out.header.Get("Set-Cookie"))
	}
	// Even a client that kept the cookie is signed out.
	stale := call{method: "GET", path: "/buckets", headers: map[string]string{"Cookie": strings.SplitN(cookie, ";", 2)[0]}}
	if r := f.do(stale); r.status != http.StatusUnauthorized {
		t.Errorf("a request with the cookie of an ended session = %d, want 401", r.status)
	}

	// Wrong password, unknown user, disabled user: one answer.
	f.must(http.StatusOK, call{method: "PUT", path: "/users/carol", body: map[string]any{"roles": []string{}, "groups": []string{}, "disabled": true}})
	var answers []string
	for _, attempt := range [][2]string{{"carol", "wrong password here"}, {"nobody", password}, {"carol", password}} {
		_, _, r := signIn(t, f, attempt[0], attempt[1])
		if r.status != http.StatusUnauthorized {
			t.Fatalf("sign-in as %s = %d, want 401", attempt[0], r.status)
		}
		p := r.problem(t)
		delete(p, "request_id")
		encoded, _ := json.Marshal(p)
		answers = append(answers, string(encoded))
	}
	if answers[0] != answers[1] || answers[1] != answers[2] {
		t.Errorf("refused sign-ins can be told apart: %v", answers)
	}
	if strings.Contains(f.logs.String(), password) || strings.Contains(fmt.Sprint(f.trail()), password) {
		t.Fatal("a password is in the log or the audit trail")
	}
}

// TestSignInAttemptsAreLimited: an address that keeps failing has to wait,
// and is not even told whether its next guess was right.
func TestSignInAttemptsAreLimited(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const password = "correct horse battery"
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{"name": "dave", "password": password}})
	for i := range 10 {
		if _, _, r := signIn(t, f, "dave", fmt.Sprintf("guess number %d", i)); r.status != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d", i, r.status)
		}
	}
	_, _, r := signIn(t, f, "dave", password)
	if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" || r.problem(t)["code"] != "TooManyAttempts" {
		t.Fatalf("the right password after ten wrong ones = %d %s", r.status, r.body)
	}
	f.clock.Advance(6 * time.Minute)
	if _, csrf, r := signIn(t, f, "dave", password); r.status != http.StatusOK || csrf == "" {
		t.Fatalf("after the wait: %d %s", r.status, r.body)
	}
}

// TestIdentityManagement walks through users, groups, roles, policies and
// keys the way the console does.
func TestIdentityManagement(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	document := map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect": "Allow", "Action": []any{"s3:GetObject", "s3:ListBucket"}, "Resource": []any{"arn:aws:s3:::photos", "arn:aws:s3:::photos/*"},
		}},
	}
	var policy struct {
		Name     string         `json:"name"`
		Document map[string]any `json:"document"`
		BuiltIn  bool           `json:"built_in"`
	}
	f.must(http.StatusCreated, call{method: "POST", path: "/policies", body: map[string]any{"name": "photos-read", "document": document}}).decode(t, &policy)
	if policy.Name != "photos-read" || policy.BuiltIn || policy.Document["Version"] != "2012-10-17" {
		t.Errorf("created policy = %+v", policy)
	}
	// A policy that is not understood is refused, and says why.
	bad := f.do(call{method: "POST", path: "/policies", bearer: f.root, body: map[string]any{"name": "bad", "document": map[string]any{
		"Version": "2012-10-17", "Statement": []any{map[string]any{"Effect": "Allow", "NotAction": "s3:*", "Resource": "*"}},
	}}})
	if bad.status != http.StatusBadRequest || !strings.Contains(fmt.Sprint(bad.problem(t)["detail"]), "NotAction") {
		t.Errorf("a policy with NotAction = %d %s", bad.status, bad.body)
	}
	f.must(http.StatusConflict, call{method: "POST", path: "/policies", body: map[string]any{"name": "photos-read", "document": document}})

	f.must(http.StatusCreated, call{method: "POST", path: "/roles", body: map[string]any{"name": "photo-viewers", "policies": []string{"photos-read"}}})
	f.must(http.StatusCreated, call{method: "POST", path: "/groups", body: map[string]any{"name": "studio", "roles": []string{"photo-viewers"}}})
	f.must(http.StatusNotFound, call{method: "POST", path: "/roles", body: map[string]any{"name": "broken", "policies": []string{"no-such-policy"}}})
	var user struct {
		Name        string   `json:"name"`
		Groups      []string `json:"groups"`
		Roles       []string `json:"roles"`
		HasPassword bool     `json:"has_password"`
		Disabled    bool     `json:"disabled"`
	}
	created := f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{
		"name": "erin", "groups": []string{"studio"}, "password": "a long enough password",
	}})
	created.decode(t, &user)
	if user.Name != "erin" || len(user.Groups) != 1 || user.Roles == nil || !user.HasPassword || strings.Contains(string(created.body), "argon") ||
		strings.Contains(string(created.body), "a long enough password") {
		t.Errorf("created user = %s", created.body)
	}
	f.must(http.StatusBadRequest, call{method: "POST", path: "/users", body: map[string]any{"name": "short", "password": "short"}})
	f.must(http.StatusBadRequest, call{method: "POST", path: "/users", body: map[string]any{"name": "not a name"}})

	// The built-in roles are there and cannot be changed.
	var roles []struct {
		Name    string `json:"name"`
		BuiltIn bool   `json:"built_in"`
	}
	f.must(http.StatusOK, call{method: "GET", path: "/roles"}).decode(t, &roles)
	builtIn := 0
	for _, r := range roles {
		if r.BuiltIn {
			builtIn++
		}
	}
	if builtIn != 5 || len(roles) != 6 {
		t.Errorf("%d roles, %d built in; want 6 and 5", len(roles), builtIn)
	}
	if r := f.do(call{method: "PUT", path: "/roles/" + iam.RoleReadOnly, bearer: f.root, body: map[string]any{"policies": []string{"photos-read"}}}); r.status < 400 {
		t.Errorf("changing a built-in role = %d", r.status)
	}

	// A key: the secret is in the answer to its creation and nowhere else.
	expires := testNow.Add(time.Hour).Format(time.RFC3339)
	var key struct {
		Key struct {
			AccessKeyID string     `json:"access_key_id"`
			Owner       string     `json:"owner"`
			Description string     `json:"description"`
			ExpiresAt   *time.Time `json:"expires_at"`
		} `json:"key"`
		SecretKey string `json:"secret_key"`
	}
	f.must(http.StatusCreated, call{method: "POST", path: "/users/erin/keys", body: map[string]any{
		"description": "laptop", "expires_at": expires,
	}}).decode(t, &key)
	if key.SecretKey == "" || key.Key.Owner != "erin" || key.Key.Description != "laptop" || key.Key.ExpiresAt == nil {
		t.Fatalf("created key = %+v", key)
	}
	listed := f.must(http.StatusOK, call{method: "GET", path: "/users/erin/keys"})
	if !strings.Contains(string(listed.body), key.Key.AccessKeyID) || strings.Contains(string(listed.body), key.SecretKey) ||
		strings.Contains(string(listed.body), "ciphertext") || strings.Contains(string(listed.body), "wrapped") {
		t.Errorf("listing of keys = %s", listed.body)
	}
	erin := key.Key.AccessKeyID + ":" + key.SecretKey
	var who struct {
		Kind        string `json:"kind"`
		Via         string `json:"via"`
		User        string `json:"user"`
		AccessKeyID string `json:"access_key_id"`
	}
	f.must(http.StatusOK, call{method: "GET", path: "/session", bearer: erin}).decode(t, &who)
	if who.Kind != "user" || who.Via != "api_key" || who.User != "erin" || who.AccessKeyID != key.Key.AccessKeyID {
		t.Errorf("GET /session with erin's key = %+v", who)
	}
	// The key expires.
	f.clock.Advance(2 * time.Hour)
	if r := f.do(call{method: "GET", path: "/session", bearer: erin}); r.status != http.StatusUnauthorized {
		t.Errorf("an expired key = %d, want 401", r.status)
	}

	// What is referred to cannot be deleted; what is not, can.
	f.must(http.StatusConflict, call{method: "DELETE", path: "/policies/photos-read"})
	f.must(http.StatusConflict, call{method: "DELETE", path: "/roles/photo-viewers"})
	f.must(http.StatusNoContent, call{method: "DELETE", path: "/users/erin"})
	f.must(http.StatusNotFound, call{method: "GET", path: "/users/erin"})
	f.must(http.StatusNoContent, call{method: "DELETE", path: "/groups/studio"})
	f.must(http.StatusNoContent, call{method: "DELETE", path: "/roles/photo-viewers"})
	f.must(http.StatusNoContent, call{method: "DELETE", path: "/policies/photos-read"})
	f.must(http.StatusNotFound, call{method: "DELETE", path: "/policies/photos-read"})
	if root := f.must(http.StatusOK, call{method: "GET", path: "/session"}); !strings.Contains(string(root.body), `"kind":"root"`) {
		t.Errorf("GET /session as root = %s", root.body)
	}
}

// TestBucketsAndObjects: the object browser's requests.
func TestBucketsAndObjects(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "photos"}})
	f.must(http.StatusConflict, call{method: "POST", path: "/buckets", body: map[string]any{"name": "photos"}})
	f.must(http.StatusBadRequest, call{method: "POST", path: "/buckets", body: map[string]any{"name": "Not_A_Bucket"}})
	f.must(http.StatusNotFound, call{method: "GET", path: "/buckets/no-such-bucket"})

	content := bytes.Repeat([]byte("<script>alert(1)</script>"), 4000)
	var stored struct {
		Key         string `json:"key"`
		Size        int64  `json:"size"`
		ETag        string `json:"etag"`
		ContentType string `json:"content_type"`
		Encryption  string `json:"encryption"`
	}
	for _, key := range []string{"2026/a.html", "2026/b.html", "2025/c.html", "top.html"} {
		f.must(http.StatusOK, call{
			method: "PUT", path: "/buckets/photos/object?key=" + key, raw: content, headers: map[string]string{"Content-Type": "text/html"},
		}).decode(t, &stored)
	}
	if stored.Key != "top.html" || stored.Size != int64(len(content)) || stored.ContentType != "text/html" || stored.Encryption == "" ||
		!strings.HasSuffix(stored.ETag, `-1"`) {
		t.Errorf("stored object = %+v", stored)
	}

	var page struct {
		Objects []struct {
			Key  string `json:"key"`
			Size int64  `json:"size"`
		} `json:"objects"`
		CommonPrefixes []string `json:"common_prefixes"`
		Truncated      bool     `json:"truncated"`
		Next           string   `json:"next_continuation_token"`
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/objects?delimiter=/"}).decode(t, &page)
	if len(page.Objects) != 1 || page.Objects[0].Key != "top.html" || fmt.Sprint(page.CommonPrefixes) != "[2025/ 2026/]" || page.Truncated {
		t.Errorf("top level = %+v", page)
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/objects?prefix=2026/&delimiter=/&limit=1"}).decode(t, &page)
	if len(page.Objects) != 1 || page.Objects[0].Key != "2026/a.html" || !page.Truncated || page.Next == "" {
		t.Fatalf("first page of 2026/ = %+v", page)
	}
	next := page.Next
	page.Next = ""
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/objects?prefix=2026/&delimiter=/&limit=1&continuation_token=" + next}).decode(t, &page)
	if len(page.Objects) != 1 || page.Objects[0].Key != "2026/b.html" || page.Truncated {
		t.Errorf("second page of 2026/ = %+v", page)
	}
	f.must(http.StatusBadRequest, call{method: "GET", path: "/buckets/photos/objects?limit=100000"})

	// How much the bucket holds: its objects counted, their sizes added up.
	var usage struct {
		ObjectCount int64 `json:"object_count"`
		TotalSize   int64 `json:"total_size"`
		Complete    bool  `json:"complete"`
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/usage"}).decode(t, &usage)
	if usage.ObjectCount != 4 || usage.TotalSize != 4*int64(len(content)) || !usage.Complete {
		t.Errorf("usage of the bucket = %+v", usage)
	}
	f.must(http.StatusNotFound, call{method: "GET", path: "/buckets/no-such-bucket/usage"})

	// A download is the bytes, as a file to save: whatever was uploaded,
	// the browser does not render it on the console's origin.
	got := f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object?key=2026/a.html"})
	if !bytes.Equal(got.body, content) {
		t.Fatal("the downloaded bytes are not the uploaded ones")
	}
	if got.header.Get("Content-Type") != "application/octet-stream" || got.header.Get("Content-Disposition") != "attachment; filename=a.html" ||
		got.header.Get("X-Content-Type-Options") != "nosniff" || got.header.Get("ETag") == "" {
		t.Errorf("download headers = %v", got.header)
	}
	var metadata struct {
		Bucket string `json:"bucket"`
		Size   int64  `json:"size"`
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object/metadata?key=2026/a.html"}).decode(t, &metadata)
	if metadata.Bucket != "photos" || metadata.Size != int64(len(content)) {
		t.Errorf("metadata = %+v", metadata)
	}
	f.must(http.StatusNotFound, call{method: "GET", path: "/buckets/photos/object?key=nope"})
	f.must(http.StatusBadRequest, call{method: "GET", path: "/buckets/photos/object"})

	f.must(http.StatusConflict, call{method: "DELETE", path: "/buckets/photos"})
	for _, key := range []string{"2026/a.html", "2026/b.html", "2025/c.html", "top.html"} {
		f.must(http.StatusNoContent, call{method: "DELETE", path: "/buckets/photos/object?key=" + key})
	}
	f.must(http.StatusNoContent, call{method: "DELETE", path: "/buckets/photos"})
	var buckets []any
	f.must(http.StatusOK, call{method: "GET", path: "/buckets"}).decode(t, &buckets)
	if len(buckets) != 0 {
		t.Errorf("buckets after deleting the only one: %v", buckets)
	}
}

// TestRequestsAreSafeToRepeat: with an idempotency key, a repeated request
// gets the first answer and changes nothing again.
func TestRequestsAreSafeToRepeat(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{"name": "frank"}})
	keyed := func(key string) map[string]string { return map[string]string{"Idempotency-Key": key} }

	// Creating a key twice with one idempotency key makes one key, and the
	// repeat is given the same secret - the only way to have it after a
	// lost answer.
	create := call{method: "POST", path: "/users/frank/keys", body: map[string]any{"description": "ci"}, headers: keyed("create-key-1")}
	first := f.must(http.StatusCreated, create)
	second := f.must(http.StatusCreated, create)
	if !bytes.Equal(first.body, second.body) || second.header.Get("Idempotent-Replayed") != "true" || first.header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("the repeat differs from the original, or is not marked:\n%s\n%s", first.body, second.body)
	}
	var keys []any
	f.must(http.StatusOK, call{method: "GET", path: "/users/frank/keys"}).decode(t, &keys)
	if len(keys) != 1 {
		t.Fatalf("%d keys after one creation and its repeat", len(keys))
	}
	var created struct {
		SecretKey string `json:"secret_key"`
	}
	first.decode(t, &created)
	// The stored answer holds a secret: it is kept sealed.
	if dump := fmt.Sprintf("%+v", f.store); strings.Contains(dump, created.SecretKey) {
		t.Fatal("the secret key of a stored answer is in the store in the clear")
	}

	// The same key with another request is a mistake of the client.
	other := create
	other.body = map[string]any{"description": "something else"}
	if r := f.do(withBearer(other, f.root)); r.status != http.StatusUnprocessableEntity || r.problem(t)["code"] != "IdempotencyKeyReused" {
		t.Errorf("a key reused for another request = %d %s", r.status, r.body)
	}
	// Keys of different callers never meet, even when they are the same.
	admin := f.userKey("grace", iam.RoleClusterAdmin)
	mine := create
	mine.bearer = admin
	if r := f.do(mine); r.status != http.StatusCreated || bytes.Equal(r.body, first.body) || r.header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("another caller with the same idempotency key was given the first caller's answer: %d", r.status)
	}

	// A refusal is an answer too: the repeat of a conflict is the conflict.
	conflict := call{method: "POST", path: "/users", body: map[string]any{"name": "frank"}, headers: keyed("create-user-1")}
	a, b := f.must(http.StatusConflict, conflict), f.must(http.StatusConflict, conflict)
	if !bytes.Equal(a.body, b.body) || b.header.Get("Content-Type") != "application/problem+json" {
		t.Errorf("the repeat of a refusal differs: %s / %s", a.body, b.body)
	}
	// Deleting twice: the second answer is the first, not "no such user".
	remove := call{method: "DELETE", path: "/users/frank", headers: keyed("delete-user-1")}
	f.must(http.StatusNoContent, remove)
	f.must(http.StatusNoContent, remove)
	f.must(http.StatusNotFound, call{method: "DELETE", path: "/users/frank"})

	// Uploads: recognised by where they go and how long they are.
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "docs"}, headers: keyed("bucket-1")})
	upload := call{method: "PUT", path: "/buckets/docs/object?key=report", raw: []byte("version one"), headers: keyed("upload-1")}
	one := f.must(http.StatusOK, upload)
	two := f.must(http.StatusOK, upload)
	if !bytes.Equal(one.body, two.body) || two.header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("the repeat of an upload differs")
	}
	longer := upload
	longer.raw = []byte("a different, longer version")
	if r := f.do(withBearer(longer, f.root)); r.status != http.StatusUnprocessableEntity {
		t.Errorf("an upload of another length with the same key = %d, want 422", r.status)
	}

	for _, bad := range []string{strings.Repeat("k", 129), "has space", "tab\there", "ünïcode"} {
		r := f.do(call{method: "POST", path: "/buckets", bearer: f.root, body: map[string]any{"name": "x-bucket"}, headers: keyed(bad)})
		if r.status != http.StatusBadRequest {
			t.Errorf("idempotency key %q = %d, want 400", bad, r.status)
		}
	}
	f.must(http.StatusNotFound, call{method: "GET", path: "/buckets/x-bucket"})

	// After a day the key is free again.
	f.clock.Advance(25 * time.Hour)
	if r := f.must(http.StatusCreated, withBearer(call{method: "POST", path: "/users", body: map[string]any{"name": "frank"}, headers: keyed("create-user-1")}, f.root)); r.header.Get("Idempotent-Replayed") != "" {
		t.Error("an answer older than a day was replayed")
	}
}

func withBearer(c call, bearer string) call {
	c.bearer = bearer
	return c
}

// TestBodiesAreBounded: what is not JSON is not read as JSON, and what is
// too large is not read to the end.
func TestBodiesAreBounded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	form := f.do(call{
		method: "POST", path: "/buckets", bearer: f.root, raw: []byte(`{"name":"from-a-form"}`),
		headers: map[string]string{"Content-Type": "text/plain"},
	})
	if form.status != http.StatusUnsupportedMediaType || form.problem(t)["code"] != "UnsupportedMediaType" {
		t.Errorf("a JSON body sent as text/plain = %d", form.status)
	}
	f.must(http.StatusNotFound, call{method: "GET", path: "/buckets/from-a-form"})

	huge := []byte(`{"name":"` + strings.Repeat("a", 2<<20) + `"}`)
	for _, headers := range []map[string]string{
		{"Content-Type": "application/json"},
		{"Content-Type": "application/json", "Idempotency-Key": "huge-1"},
	} {
		r := f.do(call{method: "POST", path: "/users", bearer: f.root, raw: huge, headers: headers})
		if r.status != http.StatusRequestEntityTooLarge && r.status != http.StatusBadRequest {
			t.Errorf("a 2 MiB JSON body = %d, want 413 or 400", r.status)
		}
	}
	notJSON := f.do(call{method: "POST", path: "/users", bearer: f.root, raw: []byte(`{"name":`), headers: map[string]string{"Content-Type": "application/json"}})
	if notJSON.status != http.StatusBadRequest {
		t.Errorf("a body that is not JSON = %d, want 400", notJSON.status)
	}
	notJSON.problem(t)
}

// TestClusterAndAudit: the status screens and the audit log.
func TestClusterAndAudit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var status struct {
		AnsweredBy  string `json:"answered_by"`
		Leader      string `json:"leader"`
		DrivesTotal int    `json:"drives_total"`
		Online      int    `json:"drives_online"`
		Nodes       []struct {
			ID     string   `json:"id"`
			Leader bool     `json:"leader"`
			Roles  []string `json:"roles"`
			Labels struct {
				Zone string `json:"zone"`
			} `json:"labels"`
		} `json:"nodes"`
		Sets []struct {
			Drives []struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"drives"`
		} `json:"erasure_sets"`
	}
	f.must(http.StatusOK, call{method: "GET", path: "/cluster"}).decode(t, &status)
	if status.AnsweredBy != "node-a" || status.Leader != "node-a" || status.DrivesTotal != 2 || status.Online != 1 ||
		len(status.Nodes) != 1 || !status.Nodes[0].Leader || status.Nodes[0].Labels.Zone != "z1" || len(status.Nodes[0].Roles) != 4 {
		t.Errorf("cluster status = %+v", status)
	}
	states := map[string]string{}
	for _, set := range status.Sets {
		for _, d := range set.Drives {
			states[d.ID] = d.State
		}
	}
	if states["drive-a"] != "online" || states["drive-b"] != "offline" {
		t.Errorf("drive states = %v", states)
	}

	f.maintenance.ObserveScrub(app.ScrubStats{Objects: 7, Damaged: 1, ShardsRebuilt: 2, Bytes: 4096})
	f.maintenance.ObserveHeal(app.HealResult{Damaged: 1, Rebuilt: 1})
	f.maintenance.ObserveHeal(app.HealResult{})
	f.maintenance.ObserveGC(app.GCStats{Shards: 30, Removed: 3})
	var m struct {
		NodeID string `json:"node_id"`
		Scrub  struct {
			Passes     int64      `json:"passes"`
			Objects    int64      `json:"objects"`
			LastPassAt *time.Time `json:"last_pass_at"`
		} `json:"scrub"`
		Heal struct {
			Objects int64 `json:"objects"`
		} `json:"heal"`
		Collection struct {
			Removed int64 `json:"removed"`
		} `json:"collection"`
	}
	f.must(http.StatusOK, call{method: "GET", path: "/cluster/maintenance"}).decode(t, &m)
	if m.NodeID != "node-a" || m.Scrub.Passes != 1 || m.Scrub.Objects != 7 || m.Scrub.LastPassAt == nil || m.Heal.Objects != 1 || m.Collection.Removed != 3 {
		t.Errorf("maintenance = %+v", m)
	}

	for i := range 5 {
		f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{"name": fmt.Sprintf("user-%d", i)}})
	}
	f.auditor.Flush(context.Background())
	var page struct {
		Records []struct {
			Sequence int64             `json:"sequence"`
			Action   string            `json:"action"`
			Actor    string            `json:"actor"`
			Hash     string            `json:"hash"`
			Details  map[string]string `json:"details"`
		} `json:"records"`
		NextAfter *int64 `json:"next_after"`
	}
	f.must(http.StatusOK, call{method: "GET", path: "/audit?limit=2"}).decode(t, &page)
	if len(page.Records) != 2 || page.Records[0].Sequence != 1 || page.NextAfter == nil || *page.NextAfter != 2 || page.Records[0].Hash == "" {
		t.Fatalf("first page of the audit log = %+v", page)
	}
	seen := 2
	for page.NextAfter != nil {
		after := *page.NextAfter
		page.NextAfter = nil
		f.must(http.StatusOK, call{method: "GET", path: fmt.Sprintf("/audit?limit=2&after=%d", after)}).decode(t, &page)
		if len(page.Records) == 0 || page.Records[0].Sequence != after+1 {
			t.Fatalf("page after %d = %+v", after, page)
		}
		seen += len(page.Records)
	}
	if seen != len(f.trail()) || seen < 6 {
		t.Errorf("paging saw %d records, the trail has %d", seen, len(f.trail()))
	}
	f.must(http.StatusBadRequest, call{method: "GET", path: "/audit?limit=0"})
	f.must(http.StatusBadRequest, call{method: "GET", path: "/audit?after=-1"})
	// A trail that was tampered with is not served.
	f.store.Tamper(2, func(r *audit.Record) { r.Actor = "user/somebody-else" })
	if r := f.do(call{method: "GET", path: "/audit", bearer: f.root}); r.status != http.StatusInternalServerError {
		t.Errorf("a tampered trail = %d, want 500", r.status)
	} else if p := r.problem(t); p["detail"] != nil {
		t.Errorf("an internal error says what went wrong inside: %s", r.body)
	}

	f.seenMu.Lock()
	defer f.seenMu.Unlock()
	routes := map[string]bool{}
	for _, o := range f.seen {
		routes[o.Operation] = true
		if strings.Contains(o.Operation, "user-") {
			t.Errorf("an observation names a user: %q", o.Operation)
		}
	}
	if !routes["POST /api/v1/users"] || !routes["GET /api/v1/audit"] {
		t.Errorf("observed routes = %v", routes)
	}
}

// TestEveryDomainErrorHasAnAnswer: the mapping from errors to problems.
func TestEveryDomainErrorHasAnAnswer(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// An error the use cases wrap with context still maps.
	r := f.do(call{method: "GET", path: "/users/" + strings.Repeat("n", 64), bearer: f.root})
	if r.status != http.StatusNotFound || r.problem(t)["code"] != "NoSuchEntity" {
		t.Errorf("GET of an unknown user = %d %s", r.status, r.body)
	}
	if !errors.Is(errors.Join(io.EOF), io.EOF) {
		t.Fatal("errors.Join")
	}
	for _, header := range []string{"X-Request-Id", "X-Content-Type-Options", "Cache-Control"} {
		if r.header.Get(header) == "" {
			t.Errorf("answer has no %s", header)
		}
	}
	if r.header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", r.header.Get("Cache-Control"))
	}
}

// TestAnEntityNamedObject: which requests carry an object is decided by the
// operation, not by what the path looks like. A user named "object" is a
// user: its body is JSON, bounded, and compared whole when a request is
// repeated.
func TestAnEntityNamedObject(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{"name": "object"}})
	update := func(disabled string) []byte {
		return []byte(`{"roles":[],"groups":[],"disabled":` + disabled + `}`)
	}
	asText := f.do(call{method: "PUT", path: "/users/object", bearer: f.root, raw: update("true"), headers: map[string]string{"Content-Type": "text/plain"}})
	if asText.status != http.StatusUnsupportedMediaType {
		t.Errorf("a text/plain body for a user named object = %d, want 415", asText.status)
	}
	huge := []byte(`{"roles":[],"groups":[],"disabled":false,"password":"` + strings.Repeat("p", 3<<20) + `"}`)
	if r := f.do(call{method: "PUT", path: "/users/object", bearer: f.root, raw: huge, headers: map[string]string{"Content-Type": "application/json"}}); r.status != http.StatusRequestEntityTooLarge && r.status != http.StatusBadRequest {
		t.Errorf("a 3 MiB body for a user named object = %d, want 413 or 400", r.status)
	}
	// Two bodies of the same length under one idempotency key are two
	// requests.
	keyed := map[string]string{"Content-Type": "application/json", "Idempotency-Key": "object-1"}
	f.must(http.StatusOK, call{method: "PUT", path: "/users/object", raw: update("false"), headers: keyed})
	other := f.do(call{method: "PUT", path: "/users/object", bearer: f.root, raw: update("true "), headers: keyed})
	if other.status != http.StatusUnprocessableEntity {
		t.Errorf("another body of the same length with the same key = %d, want 422", other.status)
	}
	for _, kind := range []string{"groups", "roles", "policies"} {
		r := f.do(call{method: "PUT", path: "/" + kind + "/object", bearer: f.root, raw: []byte(`{}`), headers: map[string]string{"Content-Type": "text/plain"}})
		if r.status != http.StatusUnsupportedMediaType {
			t.Errorf("PUT /%s/object as text/plain = %d, want 415", kind, r.status)
		}
	}
	// HEAD does not fetch an object to throw it away.
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "docs"}})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/docs/object?key=a", raw: []byte("content")})
	if r := f.do(call{method: "HEAD", path: "/buckets/docs/object?key=a", bearer: f.root}); r.status != http.StatusMethodNotAllowed {
		t.Errorf("HEAD of an object = %d, want 405", r.status)
	}
	if r := f.do(call{method: "HEAD", path: "/buckets", bearer: f.root}); r.status != http.StatusOK {
		t.Errorf("HEAD /buckets as root = %d", r.status)
	}
	if r := f.do(call{method: "HEAD", path: "/buckets"}); r.status != http.StatusUnauthorized {
		t.Errorf("HEAD /buckets without a credential = %d, want 401", r.status)
	}
}

// TestWhatIsPublic: the contract decides which operations need no
// credential, so the contract is held to exactly these. An operation added
// with "security: []", or a contract that lost its global security, fails
// here.
func TestWhatIsPublic(t *testing.T) {
	t.Parallel()
	var doc struct {
		Security []map[string]any `json:"security"`
		Paths    map[string]map[string]struct {
			OperationID string            `json:"operationId"`
			Security    *[]map[string]any `json:"security"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openapi.AdminJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Security) != 2 {
		t.Fatalf("the contract's global security = %v, want the API key and the session", doc.Security)
	}
	public := map[string]bool{}
	for _, operations := range doc.Paths {
		for _, operation := range operations {
			if operation.Security != nil {
				if len(*operation.Security) != 0 {
					t.Errorf("%s has a security requirement of its own", operation.OperationID)
				}
				public[operation.OperationID] = true
			}
		}
	}
	want := map[string]bool{"signIn": true, "getOpenAPIJSON": true, "getOpenAPIYAML": true}
	if fmt.Sprint(public) != fmt.Sprint(want) {
		t.Errorf("public operations = %v, want %v", public, want)
	}
}

// TestBearerAndCookieTogether: a request with both is the API key's, and a
// repeat made with a session never meets one made with a key.
func TestBearerAndCookieTogether(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const password = "correct horse battery"
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{"name": "ivy", "roles": []string{iam.RoleClusterAdmin}, "password": password}})
	client, csrf, r := signIn(t, f, "ivy", password)
	if r.status != http.StatusOK {
		t.Fatal(r.status)
	}
	var who struct{ Kind, Via string }
	f.must(http.StatusOK, call{method: "GET", path: "/session", client: client, bearer: f.root}).decode(t, &who)
	if who.Kind != "root" || who.Via != "api_key" {
		t.Errorf("a request with a cookie and a bearer credential is %+v", who)
	}
	// A wrong bearer credential is not rescued by a good cookie.
	if r := f.do(call{method: "GET", path: "/session", client: client, bearer: strings.Repeat("N", 16) + ":" + strings.Repeat("n", 16)}); r.status != http.StatusUnauthorized {
		t.Errorf("a wrong key with a good cookie = %d, want 401", r.status)
	}
	keyed := map[string]string{"Idempotency-Key": "same-key", httpsession.CSRFHeader: csrf}
	viaSession := f.must(http.StatusCreated, call{method: "POST", path: "/users/ivy/keys", client: client, body: map[string]any{}, headers: keyed})
	viaKey := f.must(http.StatusCreated, call{method: "POST", path: "/users/ivy/keys", body: map[string]any{}, headers: map[string]string{"Idempotency-Key": "same-key"}})
	if bytes.Equal(viaSession.body, viaKey.body) || viaKey.header.Get("Idempotent-Replayed") != "" {
		t.Error("an answer given to a session was replayed to an API key")
	}
	if strings.Contains(f.logs.String(), csrf) || strings.Contains(fmt.Sprint(f.trail()), csrf) {
		t.Fatal("a CSRF token is in the log or the audit trail")
	}
}

// TestObjectAccess: who uploaded an object and who downloaded it is served
// to those who hold the permission of its own - reading the object does not
// give it - and nothing is said of the object to anybody else, not even
// whether it exists.
func TestObjectAccess(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.meta.Now = f.clock.Now
	alice := f.userKey("alice", iam.RoleBucketAdmin)
	bob := f.userKey("bob", iam.RoleReadWrite)
	auditor := f.userKey("carol", iam.RoleAuditor)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "photos"}})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/photos/object?key=a.txt", raw: []byte("hello"), bearer: alice})

	type access struct {
		UploadedBy       *string    `json:"uploaded_by"`
		UploadedAt       time.Time  `json:"uploaded_at"`
		DownloadCount    int64      `json:"download_count"`
		CopyCount        int64      `json:"copy_count"`
		LastDownloadedAt *time.Time `json:"last_downloaded_at"`
		Readers          []struct {
			Who       string    `json:"who"`
			Presigned bool      `json:"presigned"`
			Downloads int64     `json:"downloads"`
			Copies    int64     `json:"copies"`
			LastAt    time.Time `json:"last_at"`
		} `json:"readers"`
		OtherReaders   int64 `json:"other_readers"`
		OtherDownloads int64 `json:"other_downloads"`
		OtherCopies    int64 `json:"other_copies"`
	}
	const path = "/buckets/photos/object/access?key=a.txt"

	// Uploaded, never read: the uploader and the time, and nothing else.
	var fresh access
	r := f.must(http.StatusOK, call{method: "GET", path: path})
	r.decode(t, &fresh)
	if fresh.UploadedBy == nil || *fresh.UploadedBy != "user/alice" || !fresh.UploadedAt.Equal(testNow) || fresh.DownloadCount != 0 ||
		fresh.LastDownloadedAt != nil || fresh.Readers == nil || len(fresh.Readers) != 0 {
		t.Errorf("access of an object nobody read = %s", r.body)
	}
	if strings.Contains(string(r.body), "last_downloaded_at") || !strings.Contains(string(r.body), `"readers":[]`) {
		t.Errorf("an object nobody read: %s", r.body)
	}

	// Two downloads by root, one by bob; a look at the metadata is none.
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object?key=a.txt"})
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object?key=a.txt"})
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object?key=a.txt", bearer: bob})
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object/metadata?key=a.txt", bearer: bob})
	f.clock.Advance(time.Minute)
	if err := f.access.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	var got access
	r = f.must(http.StatusOK, call{method: "GET", path: path})
	r.decode(t, &got)
	if got.DownloadCount != 3 || got.CopyCount != 0 || got.LastDownloadedAt == nil || !got.LastDownloadedAt.Equal(testNow.Add(time.Minute)) ||
		got.OtherReaders != 0 || got.OtherDownloads != 0 || got.OtherCopies != 0 {
		t.Errorf("access = %s", r.body)
	}
	if len(got.Readers) != 2 || got.Readers[0].Who != "root" || got.Readers[0].Downloads != 2 || got.Readers[0].Presigned ||
		got.Readers[1].Who != "user/bob" || got.Readers[1].Downloads != 1 || !got.Readers[1].LastAt.Equal(testNow.Add(time.Minute)) {
		t.Errorf("readers = %s", r.body)
	}
	// The version can be named.
	f.must(http.StatusOK, call{method: "GET", path: path + "&version_id=null"}).decode(t, &got)
	if got.DownloadCount != 3 {
		t.Errorf("access of the null version = %+v", got)
	}

	for name, tt := range map[string]struct {
		bearer string
		path   string
		status int
		code   string
	}{
		"an auditor": {auditor, path, http.StatusOK, ""},
		"a bucket admin, who can read the object": {alice, path, http.StatusForbidden, "AccessDenied"},
		"a read-write user, who can read it too":  {bob, path, http.StatusForbidden, "AccessDenied"},
		"who may not see it, of a missing object": {bob, "/buckets/photos/object/access?key=nope", http.StatusForbidden, "AccessDenied"},
		"who may not see it, of a missing bucket": {bob, "/buckets/no-such-bucket/object/access?key=a.txt", http.StatusForbidden, "AccessDenied"},
		"who may not see it, with a bad version":  {bob, path + "&version_id=latest", http.StatusForbidden, "AccessDenied"},
		"a missing object":                        {f.root, "/buckets/photos/object/access?key=nope", http.StatusNotFound, "NoSuchObject"},
		"a missing bucket":                        {f.root, "/buckets/no-such-bucket/object/access?key=a.txt", http.StatusNotFound, "NoSuchBucket"},
		"a missing version":                       {f.root, path + "&version_id=" + strings.Repeat("0a", 16), http.StatusNotFound, "NoSuchVersion"},
		"a version that is no version":            {f.root, path + "&version_id=latest", http.StatusBadRequest, "InvalidArgument"},
		"no key":                                  {f.root, "/buckets/photos/object/access", http.StatusBadRequest, "InvalidArgument"},
		"nobody":                                  {"", path, http.StatusUnauthorized, "Unauthenticated"},
	} {
		r := f.do(call{method: "GET", path: tt.path, bearer: tt.bearer})
		if r.status != tt.status {
			t.Errorf("%s: GET %s = %d, want %d: %s", name, tt.path, r.status, tt.status, r.body)
			continue
		}
		if tt.code != "" && r.problem(t)["code"] != tt.code {
			t.Errorf("%s: problem = %s, want code %s", name, r.body, tt.code)
		}
	}
	// Reading the object is what both were allowed.
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object?key=a.txt", bearer: alice})
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object?key=a.txt", bearer: bob})

	// A version written before uploaders were recorded says nothing of one.
	old := porttest.SampleVersion("photos", "old.txt", 1)
	old.CreatedBy = ""
	if _, err := f.meta.PutVersion(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	r = f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object/access?key=old.txt"})
	if strings.Contains(string(r.body), "uploaded_by") {
		t.Errorf("an uploader that was not recorded: %s", r.body)
	}
}

// TestListObjectsWithAccess: asked to, the listing says of each object who
// uploaded it and how often it was read - to those who may see that. Who may
// not is given the same listing without it, and no denial is written for
// what nobody was refused.
func TestListObjectsWithAccess(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.meta.Now = f.clock.Now
	alice := f.userKey("alice", iam.RoleBucketAdmin)
	stranger := f.userKey("stranger", iam.RoleAuditor)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "photos"}})
	for _, key := range []string{"a.txt", "b.txt", "dir/c.txt"} {
		f.must(http.StatusOK, call{method: "PUT", path: "/buckets/photos/object?key=" + key, raw: []byte("hello"), bearer: alice})
	}
	old := porttest.SampleVersion("photos", "old.txt", 1)
	old.CreatedBy = ""
	if _, err := f.meta.PutVersion(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object?key=a.txt"})
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object?key=a.txt", bearer: alice})
	f.clock.Advance(time.Minute)
	if err := f.access.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	type listing struct {
		Objects []struct {
			Key    string `json:"key"`
			Access *struct {
				UploadedBy       *string    `json:"uploaded_by"`
				UploadedAt       *time.Time `json:"uploaded_at"`
				DownloadCount    *int64     `json:"download_count"`
				CopyCount        *int64     `json:"copy_count"`
				LastDownloadedAt *time.Time `json:"last_downloaded_at"`
			} `json:"access"`
		} `json:"objects"`
		CommonPrefixes []string `json:"common_prefixes"`
		AccessIncluded *bool    `json:"access_included"`
	}
	const path = "/buckets/photos/objects?delimiter=/"
	before := len(f.trail())

	// Root may see it: every object says, a folder nothing.
	var full listing
	r := f.must(http.StatusOK, call{method: "GET", path: path + "&access=true"})
	r.decode(t, &full)
	if full.AccessIncluded == nil || !*full.AccessIncluded || len(full.Objects) != 3 || fmt.Sprint(full.CommonPrefixes) != "[dir/]" {
		t.Fatalf("the listing with access = %s", r.body)
	}
	read, unread, unsigned := full.Objects[0].Access, full.Objects[1].Access, full.Objects[2].Access
	if read == nil || unread == nil || unsigned == nil {
		t.Fatalf("an object without what is known of its access: %s", r.body)
	}
	if read.UploadedBy == nil || *read.UploadedBy != "user/alice" || read.UploadedAt == nil || !read.UploadedAt.Equal(testNow) ||
		read.DownloadCount == nil || *read.DownloadCount != 2 || read.CopyCount == nil || *read.CopyCount != 0 ||
		read.LastDownloadedAt == nil || !read.LastDownloadedAt.Equal(testNow.Add(time.Minute)) {
		t.Errorf("an object that was read twice: %s", r.body)
	}
	if unread.DownloadCount == nil || *unread.DownloadCount != 0 || unread.CopyCount == nil || unread.LastDownloadedAt != nil {
		t.Errorf("an object nobody read: %s", r.body)
	}
	if unsigned.UploadedBy != nil || unsigned.UploadedAt == nil || !unsigned.UploadedAt.Equal(old.ModifiedAt) {
		t.Errorf("an object whose uploader was not recorded: %s", r.body)
	}
	// The same figures as the question about the one object - and never
	// who read it.
	var one struct {
		DownloadCount int64 `json:"download_count"`
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/photos/object/access?key=a.txt"}).decode(t, &one)
	if one.DownloadCount != *read.DownloadCount {
		t.Errorf("the listing says %d downloads, the object's own answer %d", *read.DownloadCount, one.DownloadCount)
	}
	for _, never := range []string{"readers", "who", "presigned", `"root"`} {
		if strings.Contains(string(r.body), never) {
			t.Errorf("the listing has %q in it: %s", never, r.body)
		}
	}

	for name, tt := range map[string]struct {
		bearer string
		query  string
	}{
		"not asked for":                   {f.root, ""},
		"asked not to":                    {f.root, "&access=false"},
		"a bucket admin, who may list":    {alice, "&access=true"},
		"a bucket admin, who did not ask": {alice, ""},
	} {
		var plain listing
		r := f.must(http.StatusOK, call{method: "GET", path: path + tt.query, bearer: tt.bearer})
		r.decode(t, &plain)
		if plain.AccessIncluded == nil || *plain.AccessIncluded || len(plain.Objects) != 3 || fmt.Sprint(plain.CommonPrefixes) != "[dir/]" ||
			strings.Contains(string(r.body), `"access":`) || strings.Contains(string(r.body), "uploaded_by") {
			t.Errorf("%s: the listing = %s", name, r.body)
		}
	}
	// Nobody was refused anything, and the trail says nothing.
	if n := len(f.trail()) - before; n != 0 {
		t.Errorf("listings left %d audit records: %+v", n, f.trail()[before:])
	}

	// Who may not list is refused, whatever else they may see, and that is
	// in the trail - once.
	r = f.do(call{method: "GET", path: path + "&access=true", bearer: stranger})
	if p := r.problem(t); r.status != http.StatusForbidden || p["code"] != "AccessDenied" {
		t.Errorf("a listing by who may not list = %d %s", r.status, r.body)
	}
	trail := f.trail()[before:]
	if len(trail) != 1 || trail[0].Outcome != audit.Denied || trail[0].Action != "s3:ListBucket" || trail[0].Actor != "user/stranger" {
		t.Errorf("the refusal in the trail: %+v", trail)
	}
	f.must(http.StatusBadRequest, call{method: "GET", path: path + "&access=perhaps"})
}

// searchPage is the answer of a search, as a client reads it.
type searchPage struct {
	Results []struct {
		Kind         string     `json:"kind"`
		Bucket       string     `json:"bucket"`
		Key          *string    `json:"key"`
		Size         *int64     `json:"size"`
		LastModified *time.Time `json:"last_modified"`
		ETag         *string    `json:"etag"`
	} `json:"results"`
	Match            string  `json:"match"`
	Examined         int     `json:"examined"`
	Complete         bool    `json:"complete"`
	NextContinuation *string `json:"next_continuation"`
}

// names lists what a page found: "bucket" for a bucket, "bucket/key" for
// an object.
func (p searchPage) names() []string {
	found := []string{}
	for _, r := range p.Results {
		if r.Key != nil {
			found = append(found, r.Bucket+"/"+*r.Key)
		} else {
			found = append(found, r.Bucket)
		}
	}
	return found
}

// TestSearch: the search endpoint - what it answers, what it refuses and
// how, and that a page goes on where the one before stopped.
func TestSearch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reader := f.userKey("reader", iam.RoleReadOnly)
	// A user with no role may do nothing.
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{"name": "stranger", "roles": []string{}}})
	var created struct {
		Key struct {
			AccessKeyID string `json:"access_key_id"`
		} `json:"key"`
		SecretKey string `json:"secret_key"`
	}
	f.must(http.StatusCreated, call{method: "POST", path: "/users/stranger/keys", body: map[string]any{}}).decode(t, &created)
	stranger := created.Key.AccessKeyID + ":" + created.SecretKey

	for bucketName, keys := range map[string][]string{
		"photos":  {"annual/cover.jpg"},
		"reports": {"2025/annual.pdf", "2026/annual.pdf", "notes.txt"},
	} {
		f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": bucketName}})
		for _, key := range keys {
			f.must(http.StatusOK, call{method: "PUT", path: "/buckets/" + bucketName + "/object?key=" + url.QueryEscape(key), raw: []byte("hello")})
		}
	}
	search := func(query string, more ...string) string {
		return "/search?q=" + url.QueryEscape(query) + strings.Join(append([]string{""}, more...), "&")
	}
	denials := len(f.trail())

	// The shape of an answer.
	var page searchPage
	r := f.must(http.StatusOK, call{method: "GET", path: search("ANNUAL"), bearer: reader})
	r.decode(t, &page)
	if got := strings.Join(page.names(), " "); got != "photos/annual/cover.jpg reports/2025/annual.pdf reports/2026/annual.pdf" {
		t.Errorf("found %q: %s", got, r.body)
	}
	if page.Match != "substring" || page.Examined != 4 || !page.Complete || strings.Contains(string(r.body), "next_continuation") {
		t.Errorf("the answer of a search = %s", r.body)
	}
	if first := page.Results[0]; first.Kind != "object" || first.Size == nil || *first.Size != 5 || first.LastModified == nil ||
		!first.LastModified.Equal(testNow) || first.ETag == nil || *first.ETag == "" {
		t.Errorf("an object in the answer = %s", r.body)
	}
	if got := r.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	// A bucket is found by its name, and has a name and nothing else.
	r = f.must(http.StatusOK, call{method: "GET", path: search("REP*")})
	r.decode(t, &page)
	if page.Match != "wildcard" || !strings.Contains(string(r.body), `"results":[{"bucket":"reports","kind":"bucket"}]`) {
		t.Errorf("a bucket in the answer = %s", r.body)
	}
	r = f.must(http.StatusOK, call{method: "GET", path: search(`rx:^\d{4}/`, "bucket=reports", "limit=500")})
	r.decode(t, &page)
	if page.Match != "regex" || strings.Join(page.names(), " ") != "reports/2025/annual.pdf reports/2026/annual.pdf" || page.Examined != 3 {
		t.Errorf("a regular expression in one bucket = %s", r.body)
	}
	// Nothing found is an answer, with a list in it.
	r = f.must(http.StatusOK, call{method: "GET", path: search("nothing-like-this")})
	if !strings.Contains(string(r.body), `"results":[]`) {
		t.Errorf("nothing found = %s", r.body)
	}

	// What is no search is a problem that says why.
	for name, tt := range map[string]struct {
		path string
		says string
	}{
		"a regular expression that is none": {search("rx:(unclosed"), "regular expression: error parsing regexp: missing closing )"},
		"rx: and nothing":                   {search("rx:"), "regular expression"},
		"nothing":                           {search(" "), "nothing to search for"},
		"no query":                          {"/search", "q"},
		"a query too long":                  {search(strings.Repeat("a", 257)), "at most 256"},
		"limit zero":                        {search("a", "limit=0"), "limit must be 1 to 500"},
		"limit too large":                   {search("a", "limit=501"), "limit must be 1 to 500"},
		"limit that is no number":           {search("a", "limit=many"), "limit"},
		"a continuation that is none":       {search("a", "continuation=made-up"), "continuation"},
	} {
		r := f.do(call{method: "GET", path: tt.path, bearer: f.root})
		if r.status != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400: %s", name, r.status, r.body)
			continue
		}
		if p := r.problem(t); p["code"] != "InvalidArgument" || p["title"] != "Invalid request" || !strings.Contains(fmt.Sprint(p["detail"]), tt.says) {
			t.Errorf("%s: problem = %s, want a detail that says %q", name, r.body, tt.says)
		}
	}
	if n := len(f.trail()); n != denials {
		t.Errorf("searches left %d audit records", n-denials)
	}

	// Who may list nothing is refused, with nothing said, and that is in
	// the trail - once; so is a bucket somebody names and may not list.
	r = f.do(call{method: "GET", path: search("annual"), bearer: stranger})
	if p := r.problem(t); r.status != http.StatusForbidden || p["code"] != "AccessDenied" || p["detail"] != nil {
		t.Errorf("a search by who may list nothing = %d %s", r.status, r.body)
	}
	r = f.do(call{method: "GET", path: search("annual", "bucket=reports"), bearer: stranger})
	if p := r.problem(t); r.status != http.StatusForbidden || p["code"] != "AccessDenied" {
		t.Errorf("a search of a bucket by who may not list it = %d %s", r.status, r.body)
	}
	trail := f.trail()[denials:]
	if len(trail) != 2 || trail[0].Outcome != audit.Denied || trail[0].Action != "s3:ListAllMyBuckets" || trail[0].Actor != "user/stranger" ||
		trail[1].Outcome != audit.Denied || trail[1].Action != "s3:ListBucket" {
		t.Errorf("the refusals in the trail: %+v", trail)
	}
	if r := f.do(call{method: "GET", path: search("annual")}); r.status != http.StatusUnauthorized {
		t.Errorf("a search without a credential = %d", r.status)
	}
	f.must(http.StatusNotFound, call{method: "GET", path: search("annual", "bucket=no-such-bucket")})

	// Page by page: each goes on where the one before stopped, and a
	// token is of its search only.
	found, continuation, pages := []string{}, "", 0
	for {
		path := search("annual", "limit=1")
		if continuation != "" {
			path += "&continuation=" + url.QueryEscape(continuation)
		}
		var page searchPage
		r := f.must(http.StatusOK, call{method: "GET", path: path, bearer: reader})
		r.decode(t, &page)
		found = append(found, page.names()...)
		if pages++; page.Complete {
			if page.NextContinuation != nil {
				t.Errorf("the last page has a continuation: %s", r.body)
			}
			break
		}
		if page.NextContinuation == nil || *page.NextContinuation == continuation || len(page.Results) > 1 || pages > 10 {
			t.Fatalf("page %d = %s", pages, r.body)
		}
		continuation = *page.NextContinuation
	}
	if got := strings.Join(found, " "); got != "photos/annual/cover.jpg reports/2025/annual.pdf reports/2026/annual.pdf" || pages < 3 {
		t.Errorf("page by page: %q in %d pages", got, pages)
	}
	r = f.do(call{method: "GET", path: search("notes", "continuation="+url.QueryEscape(continuation)), bearer: reader})
	if p := r.problem(t); r.status != http.StatusBadRequest || !strings.Contains(fmt.Sprint(p["detail"]), "another search") {
		t.Errorf("a continuation with another search = %d %s", r.status, r.body)
	}

	// One search at a time for a caller: the second is told to come again
	// in a moment, and can.
	hold := &indexHold{entered: make(chan struct{}), release: make(chan struct{})}
	f.index.hold.Store(hold)
	type answer struct {
		status int
		err    error
	}
	first := make(chan answer, 1)
	go func() {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.URL+adminapi.BasePath+search("annual"), nil)
		if err != nil {
			first <- answer{err: err}
			return
		}
		request.Header.Set("Authorization", "Bearer "+reader)
		response, err := f.Client().Do(request)
		if err != nil {
			first <- answer{err: err}
			return
		}
		defer func() { _ = response.Body.Close() }()
		_, err = io.Copy(io.Discard, response.Body)
		first <- answer{status: response.StatusCode, err: err}
	}()
	select {
	case <-hold.entered:
	case a := <-first:
		t.Fatalf("the search ended before it walked the index: %+v", a)
	case <-time.After(10 * time.Second):
		t.Fatal("the search did not reach the index")
	}
	r = f.do(call{method: "GET", path: search("annual"), bearer: reader})
	if p := r.problem(t); r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") != "1" || p["code"] != "TooManyRequests" ||
		p["title"] != "Too many requests at once" || !strings.Contains(fmt.Sprint(p["detail"]), "still running") {
		t.Errorf("a second search at once = %d, Retry-After %q, %s", r.status, r.header.Get("Retry-After"), r.body)
	}
	f.index.hold.Store(nil)
	close(hold.release)
	select {
	case a := <-first:
		if a.err != nil || a.status != http.StatusOK {
			t.Errorf("the search that was held up = %+v", a)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the search that was held up did not end")
	}
	f.must(http.StatusOK, call{method: "GET", path: search("annual"), bearer: reader})
}

// TestSearchSaysWhenItCannotDecideInTime: when deciding which buckets are
// the caller's takes all the time a search has, the answer is a 503 that
// says what it is - maybe a limit, and not for root - and nothing of what is
// inside. The same search by root, who is bound by no bucket policy, is
// answered.
func TestSearchSaysWhenItCannotDecideInTime(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reader := f.userKey("reader", iam.RoleReadOnly)
	for _, name := range []string{"photos", "reports"} {
		f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": name}})
	}
	f.policies.takes.Store(int64(2 * app.MaxSearchTime))
	r := f.do(call{method: "GET", path: "/search?q=annual", bearer: reader})
	p := r.problem(t)
	detail := fmt.Sprint(p["detail"])
	if r.status != http.StatusServiceUnavailable || r.header.Get("Retry-After") != "1" || p["code"] != "ServiceUnavailable" {
		t.Fatalf("a search that could not decide in time = %d, Retry-After %q, %s", r.status, r.header.Get("Retry-After"), r.body)
	}
	for _, want := range []string{"a limit of the deployment's size, not an outage", "started not long ago", "root identity"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the detail does not say %q: %s", want, detail)
		}
	}
	// What the use case said - its own words, the names of its parts - is
	// for the log.
	if strings.Contains(detail, "search.Search") || strings.Contains(detail, "temporarily unavailable") {
		t.Errorf("the detail says what is inside: %s", detail)
	}
	if logs := f.logs.String(); !strings.Contains(logs, "too many buckets to decide in time") {
		t.Errorf("the log does not say why: %s", logs)
	}
	// Any other 503 says nothing: only this one has words for the caller.
	f.must(http.StatusOK, call{method: "GET", path: "/search?q=annual"})
	f.policies.takes.Store(0)
	f.must(http.StatusOK, call{method: "GET", path: "/search?q=annual", bearer: reader})
}

// giveUpASearch asks for a search as a client does and gives the request up
// once the search has begun, which it says on begun.
func giveUpASearch(ctx context.Context, t *testing.T, client *http.Client, address, bearer string, begun <-chan struct{}) {
	t.Helper()
	asked, giveUp := context.WithCancel(ctx)
	defer giveUp()
	gone := make(chan error, 1)
	go func() {
		request, err := http.NewRequestWithContext(asked, http.MethodGet, address, nil)
		if err != nil {
			gone <- err
			return
		}
		request.Header.Set("Authorization", "Bearer "+bearer)
		response, err := client.Do(request)
		if err != nil {
			gone <- err
			return
		}
		defer func() { _ = response.Body.Close() }()
		gone <- nil
	}()
	select {
	case <-begun:
	case err := <-gone:
		t.Fatalf("the search ended before it walked the index: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the search did not reach the index")
	}
	giveUp()
	if err := <-gone; !errors.Is(err, context.Canceled) {
		t.Fatalf("the client that gave up was answered: %v", err)
	}
}

// TestARequestTheClientGaveUpIsNoErrorOfTheNode: the console gives up a
// search when the user asks for another. The node says so in its log, with
// the request and what it was doing, and not as an error.
func TestARequestTheClientGaveUpIsNoErrorOfTheNode(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "reports"}})
	hold := &indexHold{entered: make(chan struct{}), release: make(chan struct{})}
	f.index.hold.Store(hold)
	giveUpASearch(t.Context(), t, f.Client(), f.URL+adminapi.BasePath+"/search?q=annual", f.root, hold.entered)
	// The node learns of it when the connection closes, a moment later.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(f.logs.String(), "request cancelled by the client") {
		if time.Now().After(deadline) {
			t.Fatalf("the node did not say that the request was given up: %s", f.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	for line := range strings.SplitSeq(f.logs.String(), "\n") {
		switch {
		case strings.Contains(line, "request cancelled by the client"):
			if !strings.Contains(line, `"level":"INFO"`) || !strings.Contains(line, `"request_id":"`) || !strings.Contains(line, "context canceled") {
				t.Errorf("a request that was given up is logged as: %s", line)
			}
		case strings.Contains(line, `"level":"ERROR"`):
			t.Errorf("a request that was given up left an error in the log: %s", line)
		}
	}
}
