package adminapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
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
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// watchedStore is the in-memory store with what the tests of the session
// lock ask of it: how many idempotency records were begun, and when an
// upload lease has been written.
type watchedStore struct {
	*porttest.MemIAM
	idempotent atomic.Int64
	leases     chan struct{}
	// endAfterReading, when set, ends the next session that is read, once
	// it has been read: the session a request was authenticated with is
	// gone when its operation looks for it.
	endAfterReading atomic.Bool
	// kept are the records of repeated requests the store was given.
	keptMu sync.Mutex
	kept   []ports.IdempotencyRecord
}

// keep notes a record of a repeated request.
func (w *watchedStore) keep(r ports.IdempotencyRecord) {
	w.keptMu.Lock()
	defer w.keptMu.Unlock()
	w.kept = append(w.kept, r)
}

// Kept returns the records of repeated requests the store was given.
func (w *watchedStore) Kept() []ports.IdempotencyRecord {
	w.keptMu.Lock()
	defer w.keptMu.Unlock()
	return slices.Clone(w.kept)
}

// CompleteIdempotent implements ports.IdempotencyStore.
func (w *watchedStore) CompleteIdempotent(ctx context.Context, r ports.IdempotencyRecord) error {
	w.keep(r)
	return w.MemIAM.CompleteIdempotent(ctx, r)
}

func (w *watchedStore) GetSession(ctx context.Context, tokenHash string) (ports.Session, error) {
	stored, err := w.MemIAM.GetSession(ctx, tokenHash)
	if err == nil && !strings.Contains(tokenHash, ":") && w.endAfterReading.CompareAndSwap(true, false) {
		if err := w.DeleteSession(ctx, tokenHash, audit.Record{}); err != nil {
			return ports.Session{}, err
		}
	}
	return stored, err
}

func (w *watchedStore) BeginIdempotent(ctx context.Context, r ports.IdempotencyRecord) error {
	w.idempotent.Add(1)
	w.keep(r)
	return w.MemIAM.BeginIdempotent(ctx, r)
}

func (w *watchedStore) PutSession(ctx context.Context, s ports.Session, record audit.Record) error {
	err := w.MemIAM.PutSession(ctx, s, record)
	if err == nil && strings.HasPrefix(s.TokenHash, "upload-lease:") {
		select {
		case w.leases <- struct{}{}:
		default:
		}
	}
	return err
}

// consolePassword is the password of the console users of these tests. It
// is put together here so that it is no credential written into the source.
func consolePassword() string {
	return strings.Join([]string{"correct", "horse", "battery"}, " ")
}

// console is a browser that has signed in.
type console struct {
	f      *fixture
	client *http.Client
	csrf   string
	// cookie is the session cookie as "name=value", for a request made
	// without the jar.
	cookie string
}

// changing returns the headers of a request of this console that changes
// something, with an idempotency key when one is named.
func (c *console) changing(key string) map[string]string {
	headers := map[string]string{httpsession.CSRFHeader: c.csrf}
	if key != "" {
		headers["Idempotency-Key"] = key
	}
	return headers
}

// consoleOf signs a user in, whom it makes first when a role is named.
func consoleOf(t *testing.T, f *fixture, user, role string) *console {
	t.Helper()
	if role != "" {
		f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{
			"name": user, "roles": []string{role}, "password": consolePassword(),
		}})
	}
	client, csrf, r := signIn(t, f, user, consolePassword())
	if r.status != http.StatusOK || csrf == "" {
		t.Fatalf("sign-in of %s = %d %s", user, r.status, r.body)
	}
	return &console{f: f, client: client, csrf: csrf, cookie: strings.SplitN(r.header.Get("Set-Cookie"), ";", 2)[0]}
}

// keepLeases runs the keeper of upload leases of the fixture for as long as
// the test lasts, as a node does.
func keepLeases(t *testing.T, f *fixture) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Go(func() { f.sessions.Run(ctx) })
	t.Cleanup(func() {
		cancel()
		done.Wait()
	})
}

// slowUpload is an upload whose body the test sends when it likes.
type slowUpload struct {
	body  *io.PipeWriter
	reply chan reply
}

// finish sends the rest of the body and returns the answer.
func (u *slowUpload) finish(t *testing.T) reply {
	t.Helper()
	if _, err := u.body.Write([]byte("the rest")); err != nil {
		t.Fatalf("sending the rest of an upload: %v", err)
	}
	if err := u.body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-u.reply:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("the upload was not answered")
		return reply{}
	}
}

// upload begins an upload of this console and returns once the node has
// written the upload lease of its session. A caller who expects no lease -
// one has been written lately - says so.
func (c *console) upload(t *testing.T, bucketName, key string, lease bool) *slowUpload {
	t.Helper()
	f := c.f
	reader, writer := io.Pipe()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPut, f.URL+adminapi.BasePath+"/buckets/"+bucketName+"/object?key="+key, reader)
	if err != nil {
		t.Fatal(err)
	}
	r.ContentLength = int64(len("the rest"))
	r.Header.Set(httpsession.CSRFHeader, c.csrf)
	up := &slowUpload{body: writer, reply: make(chan reply, 1)}
	// What was written for uploads before this one is not what is waited
	// for.
	for drained := false; !drained; {
		select {
		case <-f.watched.leases:
		default:
			drained = true
		}
	}
	go func() {
		resp, err := c.client.Do(r)
		if err != nil {
			up.reply <- reply{}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(resp.Body)
		up.reply <- reply{status: resp.StatusCode, header: resp.Header, body: data}
	}()
	t.Cleanup(func() { _ = writer.Close() })
	if !lease {
		return up
	}
	select {
	case <-f.watched.leases:
	case <-time.After(30 * time.Second):
		t.Fatal("no upload lease was written for an upload that began")
	}
	return up
}

// lock takes the session of this console to its timeout with an upload in
// flight, and returns that upload. The lease lasts for 80 seconds more of
// the fixture's clock, which nobody moves.
func (c *console) lock(t *testing.T, bucketName string) *slowUpload {
	t.Helper()
	c.f.clock.Advance(app.DefaultSessionLifetime - 10*time.Second)
	up := c.upload(t, bucketName, "slow", true)
	c.f.clock.Advance(10 * time.Second)
	return up
}

// identity is what GET /session answers.
type identity struct {
	Kind       string     `json:"kind"`
	Via        string     `json:"via"`
	User       string     `json:"user"`
	Locked     *bool      `json:"locked"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LockEndsAt *time.Time `json:"lock_ends_at"`
}

func (c *console) identity(t *testing.T) identity {
	t.Helper()
	var who identity
	c.f.must(http.StatusOK, call{method: "GET", path: "/session", client: c.client}).decode(t, &who)
	return who
}

// lockDenials returns how many refusals of a locked session of a user the
// audit trail accounts for - each in a record of its own, in the count of a
// record that says how often one was repeated, or in the count of those that
// were more than the trail takes one by one - and the action of the record
// that names a request, "" when the refusal of that request has no record of
// its own.
func (f *fixture) lockDenials(user, requestID string) (accounted int, action string) {
	f.t.Helper()
	count := func(text string) int {
		n := 0
		if _, err := fmt.Sscanf(text, "%d", &n); err != nil {
			f.t.Fatalf("a count in the audit trail: %q", text)
		}
		return n
	}
	for _, r := range f.trail() {
		switch {
		case r.Action == "audit:Suppressed":
			accounted += count(r.Details["suppressed"])
		case r.Outcome != audit.Denied || r.Actor != "user/"+user || r.Details["reason"] != "the session is locked":
		case r.Details["repeated"] != "":
			accounted += count(r.Details["repeated"])
		default:
			accounted++
			if r.RequestID == requestID {
				action = r.Action
			}
		}
	}
	return accounted, action
}

// TestALockedSessionOnlyUploads walks the contract with a locked session
// (ADR-0027): a well-formed call of every operation that needs a credential
// answers 423 SessionLocked - never a 2xx, never the 401 by which a console
// takes its session for gone - and leaves a record in the audit trail. The
// exceptions are the four a locked session is for: storing an object,
// reading its own state, unlocking and signing out. An operation that is
// added to the contract is asked for here the day it is added.
func TestALockedSessionOnlyUploads(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	keepLeases(t, f)
	document := map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
		"Effect": "Allow", "Action": []string{"s3:GetObject"}, "Resource": []string{"arn:aws:s3:::some-bucket/*"},
	}}}
	// What the calls name is there, so that no call is refused for naming
	// what is not.
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/object?key=k", raw: []byte("content")})
	f.must(http.StatusCreated, call{method: "POST", path: "/policies", body: map[string]any{"name": "some-policy", "document": document}})
	f.must(http.StatusCreated, call{method: "POST", path: "/roles", body: map[string]any{"name": "some-role", "policies": []string{"some-policy"}}})
	f.must(http.StatusCreated, call{method: "POST", path: "/groups", body: map[string]any{"name": "some-group", "roles": []string{"some-role"}}})
	somebody := f.userKey("somebody", iam.RoleReadOnly)
	keyID, _, _ := strings.Cut(somebody, ":")

	// The user has every permission there is: nothing below is refused for
	// want of one.
	admin := consoleOf(t, f, "locksmith", iam.RoleClusterAdmin)
	if who := admin.identity(t); who.Locked == nil || *who.Locked || who.ExpiresAt == nil || !who.ExpiresAt.Equal(testNow.Add(app.DefaultSessionLifetime)) ||
		who.LockEndsAt == nil || !who.LockEndsAt.Equal(testNow.Add(app.DefaultSessionLifetime+app.DefaultSessionLockMax)) {
		t.Fatalf("GET /session of a session that is not locked = %+v", who)
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets", client: admin.client})
	// What the console did while the session was not locked, each with the
	// key that makes it safe to repeat: the answers are kept for a day, and
	// one of them holds the secret of an API key.
	type made struct {
		call call
		want int
	}
	before := []made{
		{call{method: "POST", path: "/users/locksmith/keys", body: map[string]any{"description": "made before the lock"}}, http.StatusCreated},
		{call{method: "POST", path: "/buckets", body: map[string]any{"name": "made-before"}}, http.StatusCreated},
		{call{method: "POST", path: "/users", body: map[string]any{"name": "made-before", "roles": []string{iam.RoleReadOnly}}}, http.StatusCreated},
		{call{method: "PUT", path: "/users/made-before", body: map[string]any{"roles": []string{iam.RoleReadWrite}, "groups": []string{}, "disabled": false}}, http.StatusOK},
		{call{method: "POST", path: "/policies", body: map[string]any{"name": "made-before", "document": document}}, http.StatusCreated},
		{call{method: "DELETE", path: "/policies/made-before"}, http.StatusNoContent},
		{call{method: "GET", path: "/users/locksmith/keys"}, http.StatusOK},
	}
	var secret struct {
		SecretKey string `json:"secret_key"`
	}
	for n := range before {
		c := &before[n].call
		c.client, c.headers = admin.client, admin.changing(fmt.Sprintf("before-%d", n))
		first := f.must(before[n].want, *c)
		if n == 0 {
			first.decode(t, &secret)
		}
		// Not locked, the repeat is given the answer the first was given.
		if c.method != http.MethodGet {
			if again := f.must(before[n].want, *c); again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(again.body, first.body) {
				t.Fatalf("the repeat of %s %s by a session that is not locked was not answered from the record", c.method, c.path)
			}
		}
	}
	if len(secret.SecretKey) < 20 {
		t.Fatalf("no secret key was made: %q", secret.SecretKey)
	}
	uploaded := call{
		method: "PUT", path: "/buckets/some-bucket/object?key=before", client: admin.client, raw: []byte("before the lock"), headers: admin.changing("upload-before"),
	}
	f.must(http.StatusOK, uploaded)

	slow := admin.lock(t, "some-bucket")
	who := admin.identity(t)
	if who.Kind != "user" || who.Via != "session" || who.User != "locksmith" || who.Locked == nil || !*who.Locked ||
		who.ExpiresAt == nil || !who.ExpiresAt.Equal(testNow.Add(app.DefaultSessionLifetime)) ||
		who.LockEndsAt == nil || !who.LockEndsAt.Equal(testNow.Add(app.DefaultSessionLifetime+app.DefaultSessionLockMax)) {
		t.Fatalf("GET /session of a locked session = %+v", who)
	}

	media := map[string]string{"Content-Type": "application/json"}
	// A well-formed call of every operation, by its name in the contract.
	calls := map[string]call{
		"listBuckets":                {path: "/buckets"},
		"createBucket":               {path: "/buckets", body: map[string]any{"name": "new-bucket"}},
		"getBucket":                  {path: "/buckets/some-bucket"},
		"deleteBucket":               {path: "/buckets/some-bucket"},
		"getBucketUsage":             {path: "/buckets/some-bucket/usage"},
		"recountBucketUsage":         {path: "/buckets/some-bucket/usage/recount", headers: media},
		"getBucketQuota":             {path: "/buckets/some-bucket/quota"},
		"putBucketQuota":             {path: "/buckets/some-bucket/quota", body: map[string]any{"quota_bytes": 1}},
		"deleteBucketQuota":          {path: "/buckets/some-bucket/quota"},
		"getBucketNotifications":     {path: "/buckets/some-bucket/notifications"},
		"putBucketNotifications":     {path: "/buckets/some-bucket/notifications", body: map[string]any{"rules": []any{}}},
		"deleteBucketNotifications":  {path: "/buckets/some-bucket/notifications"},
		"getBucketReplication":       {path: "/buckets/some-bucket/replication"},
		"putBucketReplication":       {path: "/buckets/some-bucket/replication", body: map[string]any{"target": "site-b", "destination_bucket": "copy", "rules": []any{}}},
		"deleteBucketReplication":    {path: "/buckets/some-bucket/replication"},
		"getBucketReplicationStatus": {path: "/buckets/some-bucket/replication/status"},
		"resyncBucketReplication":    {path: "/buckets/some-bucket/replication/resync", headers: media},
		"getBucketReplicaMode":       {path: "/buckets/some-bucket/replica"},
		"putBucketReplicaMode":       {path: "/buckets/some-bucket/replica", body: map[string]any{"source": ""}},
		"listObjects":                {path: "/buckets/some-bucket/objects"},
		"getObject":                  {path: "/buckets/some-bucket/object?key=k"},
		"deleteObject":               {path: "/buckets/some-bucket/object?key=k"},
		"getObjectMetadata":          {path: "/buckets/some-bucket/object/metadata?key=k"},
		"getObjectAccess":            {path: "/buckets/some-bucket/object/access?key=k"},
		"previewObject":              {path: "/buckets/some-bucket/object/preview?key=k"},
		"search":                     {path: "/search?q=k"},
		"listUsers":                  {path: "/users"},
		"createUser":                 {path: "/users", body: map[string]any{"name": "new-user", "roles": []string{iam.RoleClusterAdmin}}},
		"getUser":                    {path: "/users/somebody"},
		"updateUser":                 {path: "/users/somebody", body: map[string]any{"roles": []string{iam.RoleClusterAdmin}, "groups": []string{}, "disabled": false}},
		"deleteUser":                 {path: "/users/somebody"},
		"listAPIKeys":                {path: "/users/somebody/keys"},
		"createAPIKey":               {path: "/users/locksmith/keys", body: map[string]any{}},
		"deleteAPIKey":               {path: "/keys/" + keyID},
		"listGroups":                 {path: "/groups"},
		"createGroup":                {path: "/groups", body: map[string]any{"name": "new-group", "roles": []string{}}},
		"getGroup":                   {path: "/groups/some-group"},
		"updateGroup":                {path: "/groups/some-group", body: map[string]any{"roles": []string{}}},
		"deleteGroup":                {path: "/groups/some-group"},
		"listRoles":                  {path: "/roles"},
		"createRole":                 {path: "/roles", body: map[string]any{"name": "new-role", "policies": []string{}}},
		"getRole":                    {path: "/roles/some-role"},
		"updateRole":                 {path: "/roles/some-role", body: map[string]any{"policies": []string{}}},
		"deleteRole":                 {path: "/roles/some-role"},
		"listPolicies":               {path: "/policies"},
		"createPolicy":               {path: "/policies", body: map[string]any{"name": "new-policy", "document": document}},
		"getPolicy":                  {path: "/policies/some-policy"},
		"updatePolicy":               {path: "/policies/some-policy", body: map[string]any{"document": document}},
		"deletePolicy":               {path: "/policies/some-policy"},
		"getCluster":                 {path: "/cluster"},
		"getMaintenance":             {path: "/cluster/maintenance"},
		"getSettings":                {path: "/settings"},
		"listTargets":                {path: "/targets"},
		"getTarget":                  {path: "/targets/hook"},
		"putTarget":                  {path: "/targets/hook", body: map[string]any{"kind": "webhook", "endpoint": "https://hook.example", "buckets": []string{"*"}}},
		"deleteTarget":               {path: "/targets/hook"},
		"getTargetStatus":            {path: "/targets/hook/status"},
		"listAudit":                  {path: "/audit"},
	}
	// The same operations asked in the other ways they can be: a list that
	// is filtered must not answer "nothing" to a locked session.
	variants := map[string][]string{
		"listBuckets": {"/buckets?details=true"},
		"listObjects": {"/buckets/some-bucket/objects?access=true", "/buckets/some-bucket/objects?prefix=k&delimiter=/"},
		"search":      {"/search?q=k&bucket=some-bucket", "/search?q=some"},
		"getObject":   {"/buckets/no-such-bucket/object?key=k", "/buckets/some-bucket/object?key=no-such-key"},
		"previewObject": {
			"/buckets/some-bucket/object/preview?key=k&version_id=null", "/buckets/some-bucket/object/preview?key=no-such-key",
		},
		"getBucket": {"/buckets/no-such-bucket"},
		"getUser":   {"/users/nobody"},
		"listAudit": {"/audit?limit=1"},
	}
	// The operations that are not written yet (TestOperationsThatAreNotWrittenYet)
	// say so before anybody is asked what the caller may do. One that is
	// written is taken from this list, and must then answer 423.
	notWritten := []string{
		"getBucketQuota", "putBucketQuota", "deleteBucketQuota", "recountBucketUsage",
		"getTargetStatus",
		"getBucketNotifications", "putBucketNotifications", "deleteBucketNotifications",
		"getBucketReplication", "putBucketReplication", "deleteBucketReplication", "getBucketReplicationStatus", "resyncBucketReplication",
		"getBucketReplicaMode", "putBucketReplicaMode",
	}
	// The calls that are refused for another action than the contract
	// names for their operation: the first question the use case asks. A
	// search of all buckets asks first whether the caller may list them.
	// A preview asks first whether the caller may download the object.
	auditedAs := map[string]string{
		"/search?q=k": "s3:ListAllMyBuckets", "/search?q=some": "s3:ListAllMyBuckets",
		"/buckets/some-bucket/object/preview?key=k":                 "s3:GetObject",
		"/buckets/some-bucket/object/preview?key=no-such-key":       "s3:GetObject",
		"/buckets/some-bucket/object/preview?key=k&version_id=null": "s3:GetObjectVersion",
	}
	// What a locked session is for.
	allowed := []string{"getSession", "signOut", "unlockSession", "putObject"}

	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string            `json:"operationId"`
			Action      string            `json:"x-cactusfs-action"`
			Security    *[]map[string]any `json:"security"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openapi.AdminJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	refused, unwritten, accounted := 0, 0, 0
	records := f.watched.idempotent.Load()

	// First of all: what was done before the lock is not answered again.
	// A stored answer is given without the use case being asked - it must
	// not be given to a locked session, least of all the one with a secret.
	for n, m := range before {
		r := f.do(m.call)
		if m.call.method == http.MethodGet {
			// A reading has no key to it: refused by its use case.
			if r.status != http.StatusLocked {
				t.Errorf("%s %s by a locked session = %d, want 423", m.call.method, m.call.path, r.status)
			}
			continue
		}
		if r.status != http.StatusLocked || r.header.Get("Idempotent-Replayed") != "" || bytes.Contains(r.body, []byte(secret.SecretKey)) || bytes.Contains(r.body, []byte("secret_key")) {
			t.Fatalf("the repeat, by a locked session, of %s %s (made before the lock, key before-%d) = %d, replayed %q: %s",
				m.call.method, m.call.path, n, r.status, r.header.Get("Idempotent-Replayed"), r.body)
		}
		if p := r.problem(t); p["code"] != "SessionLocked" || p["detail"] != nil {
			t.Errorf("%s %s: problem = %s", m.call.method, m.call.path, r.body)
		}
		now, action := f.lockDenials("locksmith", r.header.Get("X-Request-Id"))
		if now <= accounted || (action != "" && action != string(app.ActionRepeatRequest)) {
			t.Errorf("%s %s: the audit trail accounts for %d refusals after it and %d before; the action recorded is %q", m.call.method, m.call.path, now, accounted, action)
		}
		accounted = now
	}
	if strings.Contains(fmt.Sprint(f.trail()), secret.SecretKey) || strings.Contains(f.logs.String(), secret.SecretKey) {
		t.Fatal("a secret key is in the audit trail or the log")
	}
	for path, operations := range doc.Paths {
		for method, operation := range operations {
			id := operation.OperationID
			if (operation.Security != nil && len(*operation.Security) == 0) || slices.Contains(allowed, id) {
				continue
			}
			c, known := calls[id]
			if !known {
				t.Errorf("%s (%s %s) is not asked for by a locked session: add a well-formed call of it to this test", id, method, path)
				continue
			}
			// Every way the operation can be asked - and, when it changes
			// something, each of them without and with the key that makes
			// it safe to repeat, as the console sends it: without, the use
			// case of the operation refuses; with, the lock is met where
			// the key is, before anything is reserved.
			type asked struct {
				path  string
				keyed bool
			}
			var all []asked
			for _, concrete := range append([]string{c.path}, variants[id]...) {
				all = append(all, asked{path: concrete})
				if httpMutates(strings.ToUpper(method)) {
					all = append(all, asked{path: concrete, keyed: true})
				}
			}
			for n, a := range all {
				concrete := a.path
				c.method, c.path, c.client = strings.ToUpper(method), concrete, admin.client
				headers := map[string]string{}
				for name, value := range c.headers {
					if name != "Idempotency-Key" {
						headers[name] = value
					}
				}
				c.headers = headers
				if httpMutates(c.method) {
					c.headers[httpsession.CSRFHeader] = admin.csrf
				}
				if a.keyed {
					c.headers["Idempotency-Key"] = fmt.Sprintf("locked-%s-%d", id, n)
				}
				r := f.do(c)
				if slices.Contains(notWritten, id) && !a.keyed {
					unwritten++
					if r.status != http.StatusNotImplemented {
						t.Errorf("%s %s, not written yet, by a locked session = %d, want 501: %s", c.method, concrete, r.status, r.body)
					}
					continue
				}
				if r.status != http.StatusLocked {
					t.Errorf("%s %s (keyed: %v) by a locked session = %d, want 423: %s", c.method, concrete, a.keyed, r.status, r.body)
					continue
				}
				refused++
				if p := r.problem(t); p["code"] != "SessionLocked" || p["detail"] != nil || p["title"] == "" {
					t.Errorf("%s %s: problem = %s", c.method, concrete, r.body)
				}
				if r.header.Get("WWW-Authenticate") != "" || r.header.Get("Set-Cookie") != "" || r.header.Get("Idempotent-Replayed") != "" {
					t.Errorf("%s %s: a locked session is answered WWW-Authenticate %q, Set-Cookie %q", c.method, concrete,
						r.header.Get("WWW-Authenticate"), r.header.Get("Set-Cookie"))
				}
				want := operation.Action
				if other, ok := auditedAs[concrete]; ok {
					want = other
				}
				if a.keyed {
					want = string(app.ActionRepeatRequest)
				}
				// The refusal is in the audit trail: in a record that names
				// this request and the action it was refused, or - when
				// more refusals were made than the trail takes one by one -
				// in a count.
				now, action := f.lockDenials("locksmith", r.header.Get("X-Request-Id"))
				if now <= accounted || (action != "" && action != want) {
					t.Errorf("%s %s: the audit trail accounts for %d refusals after it and %d before; the action recorded is %q, want %q",
						c.method, concrete, now, accounted, action, want)
				}
				accounted = now
			}
		}
	}
	// Not one of those requests reserved its key: a locked session costs
	// the replicated log nothing before it is refused.
	if n := f.watched.idempotent.Load(); n != records {
		t.Errorf("the requests of a locked session reserved %d idempotency keys", n-records)
	}
	if refused < 70 || unwritten != len(notWritten) {
		t.Errorf("%d calls were refused and %d were of operations not written yet", refused, unwritten)
	}
	for _, id := range notWritten {
		if _, ok := calls[id]; !ok {
			t.Errorf("%s is listed as not written and is not asked for", id)
		}
	}

	// Nothing was changed, and nothing was told.
	for path, want := range map[string]int{
		"/buckets/some-bucket": 200, "/buckets/new-bucket": 404, "/users/somebody": 200, "/users/new-user": 404,
		"/groups/some-group": 200, "/groups/new-group": 404, "/roles/some-role": 200, "/roles/new-role": 404,
		"/policies/some-policy": 200, "/policies/new-policy": 404, "/buckets/some-bucket/object/metadata?key=k": 200,
	} {
		f.must(want, call{method: "GET", path: path})
	}
	var keys []any
	f.must(http.StatusOK, call{method: "GET", path: "/users/locksmith/keys"}).decode(t, &keys)
	if len(keys) != 1 {
		t.Errorf("the user has %d API keys, want the one made before the lock", len(keys))
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets", bearer: somebody})

	// Storing an object is what it may do - and so is repeating an upload:
	// the one made before the lock, sent again with its key, is answered
	// from the record, as the retry of an upload must be.
	if again := f.must(http.StatusOK, uploaded); again.header.Get("Idempotent-Replayed") != "true" {
		t.Error("the repeat, by a locked session, of an upload made before the lock was not answered from the record")
	}
	// A second upload, begun while locked, and the first, which was in
	// flight when it locked.
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/object?key=second", client: admin.client, raw: []byte("while locked"), headers: admin.changing("")})
	if r := slow.finish(t); r.status != http.StatusOK {
		t.Fatalf("the upload that was in flight when the session locked = %d %s", r.status, r.body)
	}
	for _, key := range []string{"second", "slow"} {
		f.must(http.StatusOK, call{method: "GET", path: "/buckets/some-bucket/object/metadata?key=" + key})
	}
	// Not without the CSRF token, locked or not.
	if r := f.do(call{method: "PUT", path: "/buckets/some-bucket/object?key=third", client: admin.client, raw: []byte("x")}); r.status != http.StatusUnauthorized {
		t.Errorf("an upload by a locked session without the CSRF token = %d, want 401", r.status)
	}
	// And where the user may: into a bucket that is not there it is told
	// so, as it would be unlocked.
	if r := f.do(call{method: "PUT", path: "/buckets/no-such-bucket/object?key=k", client: admin.client, raw: []byte("x"), headers: admin.changing("")}); r.status != http.StatusNotFound {
		t.Errorf("an upload by a locked session into no bucket = %d, want 404", r.status)
	}

	// An API key is never locked: the key of the very user whose session
	// is, does what the user may.
	var created struct {
		Key struct {
			AccessKeyID string `json:"access_key_id"`
		} `json:"key"`
		SecretKey string `json:"secret_key"`
	}
	f.must(http.StatusCreated, call{method: "POST", path: "/users/locksmith/keys", body: map[string]any{}}).decode(t, &created)
	bearer := created.Key.AccessKeyID + ":" + created.SecretKey
	f.must(http.StatusOK, call{method: "GET", path: "/users", bearer: bearer})
	var viaKey identity
	f.must(http.StatusOK, call{method: "GET", path: "/session", bearer: bearer}).decode(t, &viaKey)
	if viaKey.Via != "api_key" || viaKey.Locked != nil || viaKey.ExpiresAt != nil || viaKey.LockEndsAt != nil {
		t.Errorf("GET /session with an API key = %+v", viaKey)
	}
	// With the key and the cookie of the locked session, it is the key's.
	f.must(http.StatusOK, call{method: "GET", path: "/users", bearer: bearer, client: admin.client})
	if who := admin.identity(t); who.Locked == nil || !*who.Locked {
		t.Errorf("after all that the session is %+v", who)
	}

	// Signing out is the last thing a locked session may do.
	out := f.do(call{method: "DELETE", path: "/session", client: admin.client, headers: admin.changing("")})
	if out.status != http.StatusNoContent || !strings.Contains(out.header.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("sign-out of a locked session = %d, Set-Cookie %q", out.status, out.header.Get("Set-Cookie"))
	}
	if r := f.do(call{method: "GET", path: "/session", headers: map[string]string{"Cookie": admin.cookie}}); r.status != http.StatusUnauthorized {
		t.Errorf("the session after sign-out = %d, want 401", r.status)
	}
}

// httpMutates reports whether a method is one that changes something.
func httpMutates(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// TestALockedSessionWithoutTheRight: storing an object is decided by the
// user's policies, locked or not - a locked session of a user who may not
// write is refused the upload as a denial, 403, and everything else as the
// lock, 423.
func TestALockedSessionWithoutTheRight(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	keepLeases(t, f)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "reports"}})
	reader := consoleOf(t, f, "reader", iam.RoleReadOnly)
	f.must(http.StatusOK, call{method: "GET", path: "/buckets/reports/objects", client: reader.client})
	// The upload is refused - and was one this node served for the
	// session: the session is locked at its timeout, for all the good it
	// does its user.
	if r := reader.lock(t, "reports").finish(t); r.status != http.StatusForbidden {
		t.Fatalf("an upload by a reader = %d, want 403", r.status)
	}
	if who := reader.identity(t); who.Locked == nil || !*who.Locked {
		t.Fatalf("the session is %+v, want locked", who)
	}
	r := f.do(call{method: "PUT", path: "/buckets/reports/object?key=a", client: reader.client, raw: []byte("x"), headers: reader.changing("")})
	if r.status != http.StatusForbidden || r.problem(t)["code"] != "AccessDenied" {
		t.Errorf("an upload by the locked session of a reader = %d %s, want 403 AccessDenied", r.status, r.body)
	}
	// What the user may do unlocked - read - the locked session may not.
	r = f.do(call{method: "GET", path: "/buckets/reports/objects", client: reader.client})
	if r.status != http.StatusLocked || r.problem(t)["code"] != "SessionLocked" {
		t.Errorf("a listing by the locked session of a reader = %d %s, want 423 SessionLocked", r.status, r.body)
	}
	f.must(http.StatusNotFound, call{method: "GET", path: "/buckets/reports/object/metadata?key=a"})
}

// sessionCookie reads the session cookie an answer sets.
func sessionCookie(t *testing.T, r reply) *http.Cookie {
	t.Helper()
	parsed, err := http.ParseSetCookie(r.header.Get("Set-Cookie"))
	if err != nil {
		t.Fatalf("Set-Cookie %q: %v", r.header.Get("Set-Cookie"), err)
	}
	return parsed
}

// TestUnlockSession: the password of the session's user unlocks it; the
// session has new tokens then, and the old ones are refused. Nothing of the
// request is kept, whatever Idempotency-Key it carries.
func TestUnlockSession(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	keepLeases(t, f)
	password := consolePassword()
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "uploads"}})
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{
		"name": "ursula", "roles": []string{iam.RoleBucketAdmin}, "password": password,
	}})
	client, csrf, signed := signIn(t, f, "ursula", password)
	if signed.status != http.StatusOK {
		t.Fatalf("sign-in = %d %s", signed.status, signed.body)
	}
	c := &console{f: f, client: client, csrf: csrf, cookie: strings.SplitN(signed.header.Get("Set-Cookie"), ";", 2)[0]}
	// The cookie of a session is kept for as long as the session can be
	// locked, and is the cookie it always was.
	first := sessionCookie(t, signed)
	if !first.Expires.Equal(testNow.Add(app.DefaultSessionLifetime+app.DefaultSessionLockMax)) || !first.HttpOnly || !first.Secure ||
		first.SameSite != http.SameSiteStrictMode || first.Path != "/" || first.Domain != "" || first.Name != httpsession.CookieName {
		t.Errorf("the cookie of sign-in = %+v", first)
	}

	slow := c.lock(t, "uploads")
	if r := f.do(call{method: "GET", path: "/buckets", client: c.client}); r.status != http.StatusLocked {
		t.Fatalf("a locked session listing buckets = %d", r.status)
	}
	// A change the console sent as the session locked: refused, and the
	// refusal is not what its repeat will be answered.
	late := call{method: "POST", path: "/buckets", client: c.client, body: map[string]any{"name": "made-late"}, headers: c.changing("made-late-1")}
	if r := f.do(late); r.status != http.StatusLocked {
		t.Fatalf("a change by a locked session = %d", r.status)
	}
	records := f.watched.idempotent.Load()
	unlock := func(with string, headers map[string]string) reply {
		return f.do(call{method: "POST", path: "/session/unlock", client: c.client, body: map[string]any{"password": with}, headers: headers})
	}

	// Not without the CSRF token: another site cannot make the browser
	// unlock, or guess.
	for name, headers := range map[string]map[string]string{"no": nil, "a wrong": {httpsession.CSRFHeader: "not-the-token"}} {
		if r := unlock(password, headers); r.status != http.StatusUnauthorized {
			t.Errorf("unlocking with %s CSRF token = %d, want 401", name, r.status)
		}
	}
	// Not with an API key, which has nothing to unlock - whatever cookie
	// comes along.
	if r := f.do(call{method: "POST", path: "/session/unlock", bearer: f.root, client: c.client, body: map[string]any{"password": password}}); r.status != http.StatusBadRequest {
		t.Errorf("unlocking with an API key = %d, want 400", r.status)
	}
	// Not without a password, or with a body that is no JSON.
	for name, bad := range map[string]call{
		"no password":     {body: map[string]any{}},
		"no body":         {headers: map[string]string{"Content-Type": "application/json"}},
		"a form":          {raw: []byte("password=" + password), headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}},
		"a long password": {body: map[string]any{"password": strings.Repeat("p", 2000)}},
	} {
		bad.method, bad.path, bad.client = "POST", "/session/unlock", c.client
		if bad.headers == nil {
			bad.headers = map[string]string{}
		}
		bad.headers[httpsession.CSRFHeader] = c.csrf
		if r := f.do(bad); r.status == http.StatusOK || r.status == http.StatusUnauthorized || r.status >= 500 {
			t.Errorf("unlocking with %s = %d", name, r.status)
		}
	}
	if who := c.identity(t); who.Locked == nil || !*who.Locked {
		t.Fatalf("after requests that unlock nothing the session is %+v", who)
	}

	// A wrong password: 403 - not the 401 by which a console takes its
	// session for gone - and the session stays locked. Sent twice with one
	// idempotency key it is carried out twice.
	keyed := c.changing("unlock-1")
	for range 2 {
		r := unlock("not her password", keyed)
		if r.status != http.StatusForbidden || r.header.Get("WWW-Authenticate") != "" || r.header.Get("Set-Cookie") != "" || r.header.Get("Idempotent-Replayed") != "" {
			t.Fatalf("a wrong password = %d %s, WWW-Authenticate %q", r.status, r.body, r.header.Get("WWW-Authenticate"))
		}
		if p := r.problem(t); p["code"] != "AccessDenied" || p["detail"] != nil {
			t.Errorf("a wrong password: problem = %s", r.body)
		}
	}
	failures := 0
	for _, r := range f.trail() {
		if r.Action == "auth:UnlockSession" && r.Outcome == audit.Failed {
			failures++
			if repeated := r.Details["repeated"]; repeated != "" {
				failures++
			}
		}
	}
	if failures == 0 {
		t.Error("wrong passwords at the unlock left nothing in the audit trail")
	}
	if who := c.identity(t); who.Locked == nil || !*who.Locked {
		t.Fatalf("after a wrong password the session is %+v", who)
	}

	// The right one - with the idempotency key the wrong ones had, which
	// is neither a reuse nor a repeat: nothing was kept.
	f.clock.Advance(5 * time.Second)
	at := f.clock.Now()
	r := unlock(password, keyed)
	if r.status != http.StatusOK || r.header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("unlock = %d %s", r.status, r.body)
	}
	var session struct {
		User      string    `json:"user"`
		CSRFToken string    `json:"csrf_token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	r.decode(t, &session)
	var server serverOf
	r.decode(t, &server)
	renewed := sessionCookie(t, r)
	if session.User != "ursula" || session.CSRFToken == "" || session.CSRFToken == c.csrf || !session.ExpiresAt.Equal(at.Add(app.DefaultSessionLifetime)) {
		t.Errorf("unlock answered %s", r.body)
	}
	server.check(t, "the answer of an unlock", testBuild)
	if renewed.Name != httpsession.CookieName || renewed.Value == "" || renewed.Name+"="+renewed.Value == c.cookie ||
		!renewed.Expires.Equal(at.Add(app.DefaultSessionLifetime+app.DefaultSessionLockMax)) || !renewed.HttpOnly || !renewed.Secure ||
		renewed.SameSite != http.SameSiteStrictMode || renewed.Path != "/" || renewed.Domain != "" {
		t.Errorf("the cookie of an unlock = %+v", renewed)
	}
	if strings.Contains(string(r.body), renewed.Value) {
		t.Error("the session token is in the body of the answer")
	}
	old := *c
	c.csrf, c.cookie = session.CSRFToken, renewed.Name+"="+renewed.Value

	// The old cookie is refused - with either CSRF token - and so is the
	// old CSRF token with the new cookie.
	for name, stale := range map[string]call{
		"the old cookie":                        {method: "GET", path: "/buckets", headers: map[string]string{"Cookie": old.cookie}},
		"the old cookie and its CSRF token":     {method: "POST", path: "/buckets", body: map[string]any{"name": "stale-a"}, headers: map[string]string{"Cookie": old.cookie, httpsession.CSRFHeader: old.csrf}},
		"the old cookie and the new CSRF token": {method: "POST", path: "/buckets", body: map[string]any{"name": "stale-b"}, headers: map[string]string{"Cookie": old.cookie, httpsession.CSRFHeader: c.csrf}},
		"the new cookie and the old CSRF token": {method: "POST", path: "/buckets", body: map[string]any{"name": "stale-c"}, headers: map[string]string{"Cookie": c.cookie, httpsession.CSRFHeader: old.csrf}},
		"unlocking with the old cookie":         {method: "POST", path: "/session/unlock", body: map[string]any{"password": password}, headers: map[string]string{"Cookie": old.cookie, httpsession.CSRFHeader: old.csrf}},
	} {
		r := f.do(stale)
		if r.status != http.StatusUnauthorized {
			t.Errorf("%s after the unlock = %d, want 401: %s", name, r.status, r.body)
		}
		// And the cookie is left alone. The unlock that comes with the old
		// cookie may be the very request that unlocked, sent twice - which
		// a console must not do, and a network may: by now the browser
		// holds the new cookie, and an answer that cleared it would sign
		// the user out of the session just unlocked.
		if set := r.header.Get("Set-Cookie"); set != "" {
			t.Errorf("%s after the unlock is answered Set-Cookie %q", name, set)
		}
	}
	// The browser - its jar took the new cookie - has full authority again.
	if who := c.identity(t); who.Locked == nil || *who.Locked || who.ExpiresAt == nil || !who.ExpiresAt.Equal(session.ExpiresAt) {
		t.Errorf("after the unlock the session is %+v", who)
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets", client: c.client})
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", client: c.client, body: map[string]any{"name": "after-unlock"}, headers: c.changing("")})
	// The change that was refused for the lock, repeated with the key it
	// had: carried out now, not answered "locked" from a record.
	late.headers = c.changing("made-late-1")
	if r := f.must(http.StatusCreated, late); r.header.Get("Idempotent-Replayed") != "" {
		t.Error("the repeat of a change that was refused for the lock was answered from a record")
	}
	records++ // that one is kept, as the answer of every change is
	// The upload that was in flight all along is stored.
	if r := slow.finish(t); r.status != http.StatusOK {
		t.Errorf("the upload that was in flight across the lock and the unlock = %d %s", r.status, r.body)
	}

	// Unlocking a session that is not locked does the same: sent again,
	// with the same idempotency key, it is carried out again.
	again := unlock(password, c.changing("unlock-1"))
	if again.status != http.StatusOK || again.header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("unlocking a session that is not locked = %d %s", again.status, again.body)
	}
	if third := sessionCookie(t, again); third.Value == renewed.Value {
		t.Error("the second unlock was answered with the session of the first")
	}
	// Three unlocks carried an idempotency key and two did not; no request
	// since the count was taken wrote an idempotency record.
	if n := f.watched.idempotent.Load(); n != records {
		t.Errorf("unlocking wrote %d idempotency records", n-records)
	}

	// The password went to the password check and nowhere else: not into
	// the log, not into the audit trail - and into no idempotency record,
	// of which none was written (above).
	if strings.Contains(f.logs.String(), password) || strings.Contains(fmt.Sprint(f.trail()), password) {
		t.Fatal("a password is in the log or the audit trail")
	}
	unlocked := 0
	for _, r := range f.trail() {
		if r.Action == "auth:UnlockSession" && r.Outcome == audit.Succeeded && r.Actor == "user/ursula" {
			unlocked++
		}
	}
	if unlocked != 2 {
		t.Errorf("the audit trail has %d unlocks, want 2", unlocked)
	}
}

// TestUnlockAttemptsAreLimited: the address that keeps failing waits, as at
// sign-in and by the same count; and the fifth wrong password for one
// session ends that session.
func TestUnlockAttemptsAreLimited(t *testing.T) {
	t.Parallel()
	t.Run("an address that keeps failing", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		c := consoleOf(t, f, "victor", iam.RoleReadOnly)
		// Eight wrong sign-ins and two wrong unlocks are ten failures.
		for i := range 8 {
			if _, _, r := signIn(t, f, "victor", fmt.Sprintf("guess number %d", i)); r.status != http.StatusUnauthorized {
				t.Fatalf("guess %d = %d", i, r.status)
			}
		}
		unlock := func(with string) reply {
			return f.do(call{method: "POST", path: "/session/unlock", client: c.client, body: map[string]any{"password": with}, headers: c.changing("")})
		}
		for i := range 2 {
			if r := unlock(fmt.Sprintf("unlock guess %d", i)); r.status != http.StatusForbidden {
				t.Fatalf("unlock guess %d = %d", i, r.status)
			}
		}
		r := unlock(consolePassword())
		if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" || r.problem(t)["code"] != "TooManyAttempts" {
			t.Fatalf("the right password after ten failures = %d %s", r.status, r.body)
		}
		// The session is kept: it is the address that waits.
		if who := c.identity(t); who.Locked == nil || *who.Locked {
			t.Errorf("the session of an address that waits is %+v", who)
		}
		f.clock.Advance(6 * time.Minute)
		if r := unlock(consolePassword()); r.status != http.StatusOK {
			t.Errorf("after the wait: %d %s", r.status, r.body)
		}
	})
	t.Run("the fifth wrong password for a session", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		c := consoleOf(t, f, "wanda", iam.RoleReadOnly)
		unlock := func(with string) reply {
			return f.do(call{method: "POST", path: "/session/unlock", client: c.client, body: map[string]any{"password": with}, headers: c.changing("")})
		}
		for i := range 4 {
			if r := unlock(fmt.Sprintf("guess number %d", i)); r.status != http.StatusForbidden {
				t.Fatalf("guess %d = %d %s", i, r.status, r.body)
			}
		}
		r := unlock("the fifth guess")
		if r.status != http.StatusUnauthorized || r.problem(t)["code"] != "Unauthenticated" || !strings.Contains(r.header.Get("Set-Cookie"), "Max-Age=0") {
			t.Fatalf("the fifth wrong password = %d %s, Set-Cookie %q; want 401 and the cookie cleared", r.status, r.body, r.header.Get("Set-Cookie"))
		}
		// The session is gone for whoever kept its cookie.
		for _, stale := range []call{
			{method: "GET", path: "/session", headers: map[string]string{"Cookie": c.cookie}},
			{method: "POST", path: "/session/unlock", body: map[string]any{"password": consolePassword()}, headers: map[string]string{"Cookie": c.cookie, httpsession.CSRFHeader: c.csrf}},
		} {
			if r := f.do(stale); r.status != http.StatusUnauthorized {
				t.Errorf("%s %s after the session was ended = %d, want 401", stale.method, stale.path, r.status)
			}
		}
		// The user signs in again.
		if _, _, r := signIn(t, f, "wanda", consolePassword()); r.status != http.StatusOK {
			t.Errorf("signing in again = %d %s", r.status, r.body)
		}
	})
	t.Run("an unlock that finds its session ended", func(t *testing.T) {
		t.Parallel()
		// The request is authenticated, and the session is gone when the
		// operation looks for it: as when the same unlock was sent twice
		// and the first has just rotated the tokens. 401 - and the cookie
		// is left alone, for it may be the new one by now. Only the unlock
		// that itself ends the session, above, clears it.
		f := newFixture(t)
		c := consoleOf(t, f, "xenia", iam.RoleReadOnly)
		f.watched.endAfterReading.Store(true)
		r := f.do(call{method: "POST", path: "/session/unlock", client: c.client, body: map[string]any{"password": consolePassword()}, headers: c.changing("")})
		if r.status != http.StatusUnauthorized || r.problem(t)["code"] != "Unauthenticated" {
			t.Fatalf("an unlock whose session ended under it = %d %s, want 401", r.status, r.body)
		}
		if set := r.header.Get("Set-Cookie"); set != "" {
			t.Errorf("it is answered Set-Cookie %q", set)
		}
		if f.watched.endAfterReading.Load() {
			t.Fatal("the session was never read")
		}
	})
}

// TestAPasswordChangeEndsEverySession: when the password of a user is
// changed - by an administrator here - every session of the user ends at
// once, the locked one with its upload in flight and the active one alike,
// and neither password unlocks either. The user signs in again with the new
// one.
func TestAPasswordChangeEndsEverySession(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	keepLeases(t, f)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "uploads"}})
	locked := consoleOf(t, f, "yolanda", iam.RoleBucketAdmin)
	slow := locked.lock(t, "uploads")
	active := consoleOf(t, f, "yolanda", "")
	if who := locked.identity(t); who.Locked == nil || !*who.Locked {
		t.Fatalf("the first session is %+v, want locked", who)
	}
	if who := active.identity(t); who.Locked == nil || *who.Locked {
		t.Fatalf("the second session is %+v, want active", who)
	}

	changed := strings.Join([]string{"another", "long", "password"}, " ")
	f.clock.Advance(time.Second)
	f.must(http.StatusOK, call{method: "PUT", path: "/users/yolanda", body: map[string]any{
		"roles": []string{iam.RoleBucketAdmin}, "groups": []string{}, "disabled": false, "password": changed,
	}})
	for name, c := range map[string]*console{"locked": locked, "active": active} {
		for what, request := range map[string]call{
			"GET /session":                    {method: "GET", path: "/session"},
			"an upload":                       {method: "PUT", path: "/buckets/uploads/object?key=after", raw: []byte("x"), headers: c.changing("")},
			"unlocking with the old password": {method: "POST", path: "/session/unlock", body: map[string]any{"password": consolePassword()}, headers: c.changing("")},
			"unlocking with the new password": {method: "POST", path: "/session/unlock", body: map[string]any{"password": changed}, headers: c.changing("")},
		} {
			request.client = c.client
			if r := f.do(request); r.status != http.StatusUnauthorized {
				t.Errorf("%s by the %s session after the password changed = %d, want 401: %s", what, name, r.status, r.body)
			}
		}
	}
	f.must(http.StatusNotFound, call{method: "GET", path: "/buckets/uploads/object/metadata?key=after"})
	// The upload that was in flight is finished by the server, as one is
	// whose session ends in any other way.
	if r := slow.finish(t); r.status != http.StatusOK {
		t.Errorf("the upload in flight when the password changed = %d %s", r.status, r.body)
	}
	if _, _, r := signIn(t, f, "yolanda", consolePassword()); r.status != http.StatusUnauthorized {
		t.Errorf("signing in with the old password = %d", r.status)
	}
	// A session begun within app.PasswordChangeMargin of the change counts
	// as one from before it - the two times may be of two clocks - and is
	// ended at once, though it was begun with the new password: the price
	// of the margin, and the user signs in again.
	f.clock.Advance(time.Second)
	client, csrf, r := signIn(t, f, "yolanda", changed)
	if r.status != http.StatusOK || csrf == "" {
		t.Fatalf("signing in with the new password a second after the change = %d %s", r.status, r.body)
	}
	f.must(http.StatusUnauthorized, call{method: "GET", path: "/buckets", client: client})
	f.clock.Advance(app.PasswordChangeMargin)
	client, csrf, r = signIn(t, f, "yolanda", changed)
	if r.status != http.StatusOK || csrf == "" {
		t.Fatalf("signing in with the new password = %d %s", r.status, r.body)
	}
	f.must(http.StatusOK, call{method: "GET", path: "/buckets", client: client})
	if strings.Contains(f.logs.String(), changed) || strings.Contains(fmt.Sprint(f.trail()), changed) {
		t.Fatal("a password is in the log or the audit trail")
	}
}
