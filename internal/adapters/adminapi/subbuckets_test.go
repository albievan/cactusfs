package adminapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/api/openapi"
	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/adapters/httpsession"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// The operations of bucket policies and sub-buckets (ADR-0017, PLAN C.6).
// The handlers translate; the use cases authorize and audit
// (BucketService.SetPolicy, SubBucketService), so these tests ask what a
// caller is told, and that a change is recorded once - also when its
// request is repeated with the same Idempotency-Key.

// policyOf is a bucket policy of some-bucket - which its sub-bucket
// "reports" could have too, being confined to it - that allows the user
// dora to read the keys of reports.
func policyOf(bucketName string) map[string]any {
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect": "Allow", "Principal": map[string]any{"AWS": "arn:aws:iam:::user/dora"},
			"Action": "s3:GetObject", "Resource": "arn:aws:s3:::" + bucketName + "/reports/*",
		}},
	}
}

// aDocument is policyOf("some-bucket").
var aDocument = policyOf("some-bucket")

// listingByDora is a policy of a sub-bucket of some-bucket that lets the
// user dora list it, and nothing else.
func listingByDora() map[string]any {
	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect": "Allow", "Principal": map[string]any{"AWS": "arn:aws:iam:::user/dora"},
			"Action": "s3:ListBucket", "Resource": "arn:aws:s3:::some-bucket",
		}},
	}
}

// succeeded counts the records of the trail from n on of action on the
// bucket that succeeded.
func succeeded(f *fixture, from int, action, bucketName string) int {
	f.t.Helper()
	count := 0
	for _, r := range f.trail()[from:] {
		if r.Action == action && r.Resource == "arn:aws:s3:::"+bucketName && r.Outcome == audit.Succeeded {
			count++
		}
	}
	return count
}

// TestBucketPolicyAndSubBucketOperationsAreDeclared: each operation is
// declared in the contract with the action its use case asks, and each that
// changes something with Idempotency-Key and X-CSRF-Token, as every other.
func TestBucketPolicyAndSubBucketOperationsAreDeclared(t *testing.T) {
	t.Parallel()
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Action      string `json:"x-cactusfs-action"`
			Parameters  []struct {
				Ref string `json:"$ref"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openapi.AdminJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ id, action string }{
		{"getBucketPolicy", "s3:GetBucketPolicy"},
		{"putBucketPolicy", "s3:PutBucketPolicy"},
		{"deleteBucketPolicy", "s3:DeleteBucketPolicy"},
		{"listSubBuckets", "s3:ListSubBuckets"},
		{"getSubBucket", "s3:GetSubBucketPolicy"},
		{"putSubBucket", "s3:PutSubBucket"},
		{"deleteSubBucket", "s3:DeleteSubBucket"},
	}
	for _, tt := range tests {
		found := false
		for path, operations := range doc.Paths {
			for method, operation := range operations {
				if operation.OperationID != tt.id {
					continue
				}
				found = true
				if operation.Action != tt.action {
					t.Errorf("%s (%s %s) needs %q, want %q", tt.id, method, path, operation.Action, tt.action)
				}
				var refs []string
				for _, p := range operation.Parameters {
					refs = append(refs, p.Ref)
				}
				for _, p := range []string{"#/components/parameters/IdempotencyKey", "#/components/parameters/CSRFToken"} {
					if mutating := method != "get"; mutating != slices.Contains(refs, p) {
						t.Errorf("%s (%s %s): parameters %v, %s declared: %v", tt.id, method, path, refs, p, !mutating)
					}
				}
			}
		}
		if !found {
			t.Errorf("the contract has no operation %s", tt.id)
		}
	}
}

// TestBucketPolicy: GET, PUT and DELETE /buckets/{bucket}/policy, the
// document S3's GetBucketPolicy and PutBucketPolicy read and write.
func TestBucketPolicy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	path := "/buckets/some-bucket/policy"
	start := len(f.trail())

	// None yet: 200, and no document.
	if r := f.must(http.StatusOK, call{method: "GET", path: path}); strings.TrimSpace(string(r.body)) != "{}" {
		t.Errorf("the policy of a bucket without one = %s", r.body)
	}

	// Set, with a key that makes it safe to repeat: the repeat is the
	// answer that was given, and the change is recorded once.
	put := call{method: "PUT", path: path, body: map[string]any{"document": aDocument}, headers: map[string]string{"Idempotency-Key": "policy-1"}}
	first := f.must(http.StatusOK, put)
	var got struct {
		Document map[string]any `json:"document"`
	}
	first.decode(t, &got)
	want, _ := json.Marshal(aDocument)
	if have, _ := json.Marshal(got.Document); !bytes.Equal(have, want) {
		t.Errorf("PUT answered %s, want the document %s", first.body, want)
	}
	again := f.must(http.StatusOK, put)
	if again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(again.body, first.body) || first.header.Get("Idempotent-Replayed") != "" {
		t.Errorf("the repeat of a PUT: replayed %q, body %s", again.header.Get("Idempotent-Replayed"), again.body)
	}
	if n := succeeded(f, start, "s3:PutBucketPolicy", "some-bucket"); n != 1 {
		t.Errorf("a PUT sent twice with one key was recorded %d times", n)
	}
	read := f.must(http.StatusOK, call{method: "GET", path: path})
	read.decode(t, &got)
	if have, _ := json.Marshal(got.Document); !bytes.Equal(have, want) {
		t.Errorf("GET after PUT = %s", read.body)
	}

	// Documents a bucket cannot keep: 400 InvalidPolicy, and the policy it
	// had is kept.
	allowBypass := policyOf("some-bucket")
	firstStatement(t, allowBypass)["Action"] = "s3:BypassGovernanceRetention"
	refused := map[string]any{
		"empty":               map[string]any{},
		"another bucket":      policyOf("other-bucket"),
		"a governance bypass": allowBypass,
		"larger than 20 KiB": map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
			"Sid": strings.Repeat("x", 21<<10), "Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::some-bucket/*",
		}}},
	}
	for name, document := range refused {
		r := f.must(http.StatusBadRequest, call{method: "PUT", path: path, body: map[string]any{"document": document}})
		if p := r.problem(t); p["code"] != "InvalidPolicy" || p["detail"] == nil {
			t.Errorf("%s: problem = %s", name, r.body)
		}
	}
	// A body without the document the contract requires is a request
	// that is wrong, as for every operation, not a policy.
	for _, body := range []map[string]any{{}, {"document": nil}} {
		r := f.must(http.StatusBadRequest, call{method: "PUT", path: path, body: body})
		if p := r.problem(t); p["code"] != "InvalidArgument" {
			t.Errorf("a body %v: problem = %s", body, r.body)
		}
	}
	if r := f.must(http.StatusOK, call{method: "GET", path: path}); !bytes.Equal(r.body, read.body) {
		t.Errorf("the policy after refused ones = %s, want %s", r.body, read.body)
	}

	// The policies of identities are not bucket policies: one that does not
	// parse is still InvalidArgument.
	r := f.must(http.StatusBadRequest, call{method: "POST", path: "/policies", body: map[string]any{"name": "nothing", "document": map[string]any{}}})
	if p := r.problem(t); p["code"] != "InvalidArgument" {
		t.Errorf("an identity policy that does not parse: problem = %s", r.body)
	}
	r = f.must(http.StatusBadRequest, call{method: "PUT", path: "/policies/" + iam.RoleReadOnly, body: map[string]any{"document": map[string]any{}}})
	if p := r.problem(t); p["code"] != "InvalidArgument" {
		t.Errorf("an identity policy that does not parse, put: problem = %s", r.body)
	}

	// Removed, with a key: the repeat is the answer that was given.
	del := call{method: "DELETE", path: path, headers: map[string]string{"Idempotency-Key": "policy-2"}}
	f.must(http.StatusNoContent, del)
	if again := f.must(http.StatusNoContent, del); again.header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("the repeat of a DELETE was not replayed")
	}
	if n := succeeded(f, start, "s3:DeleteBucketPolicy", "some-bucket"); n != 1 {
		t.Errorf("a DELETE sent twice with one key was recorded %d times", n)
	}
	if r := f.must(http.StatusOK, call{method: "GET", path: path}); strings.TrimSpace(string(r.body)) != "{}" {
		t.Errorf("the policy after DELETE = %s", r.body)
	}

	// No such bucket: 404 NoSuchBucket, to a caller who may ask.
	for _, c := range []call{
		{method: "GET", path: "/buckets/no-such-bucket/policy"},
		{method: "PUT", path: "/buckets/no-such-bucket/policy", body: map[string]any{"document": policyOf("no-such-bucket")}},
		{method: "DELETE", path: "/buckets/no-such-bucket/policy"},
	} {
		if p := f.must(http.StatusNotFound, c).problem(t); p["code"] != "NoSuchBucket" {
			t.Errorf("%s %s: problem = %v", c.method, c.path, p)
		}
	}
}

// TestBucketPolicyIsAuthorized: 401 without a credential, 403 to a caller
// without the action - read-only holds none of the policy's - and the
// policy is neither shown nor changed.
func TestBucketPolicyIsAuthorized(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/policy", body: map[string]any{"document": aDocument}})
	reader := f.userKey("reader", iam.RoleReadOnly)
	for _, c := range []call{
		{method: "GET", path: "/buckets/some-bucket/policy"},
		{method: "PUT", path: "/buckets/some-bucket/policy", body: map[string]any{"document": aDocument}},
		{method: "DELETE", path: "/buckets/some-bucket/policy"},
	} {
		for name, bearer := range map[string]string{"no credential": "", "a credential that is none": "AKIDNOBODY:not-a-secret"} {
			asked := c
			asked.bearer = bearer
			if r := f.do(asked); r.status != http.StatusUnauthorized || r.problem(t)["code"] != "Unauthenticated" {
				t.Errorf("%s %s with %s = %d: %s", c.method, c.path, name, r.status, r.body)
			}
		}
		asked := c
		asked.bearer = reader
		r := f.do(asked)
		if r.status != http.StatusForbidden {
			t.Errorf("%s %s by read-only = %d: %s", c.method, c.path, r.status, r.body)
			continue
		}
		if p := r.problem(t); p["code"] != "AccessDenied" || p["detail"] != nil || bytes.Contains(r.body, []byte("dora")) {
			t.Errorf("%s %s by read-only: problem = %s", c.method, c.path, r.body)
		}
	}
	if r := f.must(http.StatusOK, call{method: "GET", path: "/buckets/some-bucket/policy"}); !bytes.Contains(r.body, []byte("user/dora")) {
		t.Errorf("the policy after refused changes = %s", r.body)
	}
}

// TestSubBuckets: PUT, GET, list and DELETE of the sub-buckets of a
// bucket, and the problems the contract names for each.
func TestSubBuckets(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	start := len(f.trail())
	path := "/buckets/some-bucket/subbuckets/reports"

	// Made with a policy, with a key that makes it safe to repeat.
	put := call{method: "PUT", path: path, body: map[string]any{"policy": aDocument}, headers: map[string]string{"Idempotency-Key": "sub-1"}}
	first := f.must(http.StatusOK, put)
	var made struct {
		Name      string         `json:"name"`
		CreatedAt time.Time      `json:"created_at"`
		Policy    map[string]any `json:"policy"`
	}
	first.decode(t, &made)
	want, _ := json.Marshal(aDocument)
	if have, _ := json.Marshal(made.Policy); made.Name != "reports" || !made.CreatedAt.Equal(testNow) || !bytes.Equal(have, want) {
		t.Errorf("PUT answered %s", first.body)
	}
	f.clock.Advance(time.Hour)
	again := f.must(http.StatusOK, put)
	if again.header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(again.body, first.body) {
		t.Errorf("the repeat of a PUT: replayed %q, body %s", again.header.Get("Idempotent-Replayed"), again.body)
	}
	if n := succeeded(f, start, "s3:PutSubBucket", "some-bucket"); n != 1 {
		t.Errorf("a PUT sent twice with one key was recorded %d times", n)
	}
	if r := f.must(http.StatusOK, call{method: "GET", path: path}); !bytes.Equal(r.body, first.body) {
		t.Errorf("GET = %s, want %s", r.body, first.body)
	}

	// Put again without a policy: it keeps its time, and has none.
	r := f.must(http.StatusOK, call{method: "PUT", path: path, body: map[string]any{}})
	var plain map[string]any
	r.decode(t, &plain)
	if _, has := plain["policy"]; has || plain["created_at"] != testNow.Format(time.RFC3339) {
		t.Errorf("PUT without a policy = %s", r.body)
	}
	f.must(http.StatusOK, call{method: "PUT", path: path, body: map[string]any{"policy": aDocument}})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/subbuckets/archive", body: map[string]any{}})

	// The list names them and their ages, and never a policy.
	list := f.must(http.StatusOK, call{method: "GET", path: "/buckets/some-bucket/subbuckets"})
	var listed []map[string]any
	list.decode(t, &listed)
	if len(listed) != 2 || listed[0]["name"] != "archive" || listed[1]["name"] != "reports" || bytes.Contains(list.body, []byte("policy")) ||
		bytes.Contains(list.body, []byte("Statement")) {
		t.Errorf("the list = %s", list.body)
	}
	for _, s := range listed {
		if len(s) != 2 || s["created_at"] == nil {
			t.Errorf("a sub-bucket in the list = %v", s)
		}
	}

	// Policies a sub-bucket cannot have: 400 InvalidPolicy, and nothing
	// changed. A name a sub-bucket cannot have: 400 InvalidArgument.
	anybody := policyOf("some-bucket")
	firstStatement(t, anybody)["Principal"] = "*"
	for name, document := range map[string]any{
		"another sub-bucket's keys": map[string]any{"Version": "2012-10-17", "Statement": []any{map[string]any{
			"Effect": "Allow", "Principal": map[string]any{"AWS": "arn:aws:iam:::user/dora"},
			"Action": "s3:GetObject", "Resource": "arn:aws:s3:::some-bucket/archive/*",
		}}},
		"allowed to anybody": anybody,
		"empty":              map[string]any{},
		"no statement":       map[string]any{"Version": "2012-10-17"},
	} {
		r := f.must(http.StatusBadRequest, call{method: "PUT", path: path, body: map[string]any{"policy": document}})
		if p := r.problem(t); p["code"] != "InvalidPolicy" || p["detail"] == nil {
			t.Errorf("%s: problem = %s", name, r.body)
		}
	}
	if r := f.must(http.StatusOK, call{method: "GET", path: path}); !bytes.Contains(r.body, []byte("reports/*")) {
		t.Errorf("the sub-bucket after refused policies = %s", r.body)
	}
	for _, body := range []map[string]any{{}, {"policy": aDocument}} {
		r := f.must(http.StatusBadRequest, call{method: "PUT", path: "/buckets/some-bucket/subbuckets/Reports", body: body})
		if p := r.problem(t); p["code"] != "InvalidArgument" {
			t.Errorf("a name a sub-bucket cannot have (%v): problem = %s", body, r.body)
		}
	}

	// Not there: 404 NoSuchBucket for the bucket, NoSuchSubBucket for the
	// sub-bucket.
	for _, c := range []call{
		{method: "GET", path: "/buckets/no-such-bucket/subbuckets"},
		{method: "GET", path: "/buckets/no-such-bucket/subbuckets/reports"},
		{method: "PUT", path: "/buckets/no-such-bucket/subbuckets/reports", body: map[string]any{}},
		{method: "DELETE", path: "/buckets/no-such-bucket/subbuckets/reports"},
	} {
		if p := f.must(http.StatusNotFound, c).problem(t); p["code"] != "NoSuchBucket" {
			t.Errorf("%s %s: problem = %v", c.method, c.path, p)
		}
	}
	for _, c := range []call{
		{method: "GET", path: "/buckets/some-bucket/subbuckets/missing"},
		{method: "DELETE", path: "/buckets/some-bucket/subbuckets/missing"},
	} {
		if p := f.must(http.StatusNotFound, c).problem(t); p["code"] != "NoSuchSubBucket" || p["title"] == "" {
			t.Errorf("%s %s: problem = %v", c.method, c.path, p)
		}
	}

	// A sub-bucket that holds a key is not deleted: 409 SubBucketNotEmpty.
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/object?key=reports/2026.csv", raw: []byte("a,b\n")})
	if p := f.must(http.StatusConflict, call{method: "DELETE", path: path}).problem(t); p["code"] != "SubBucketNotEmpty" {
		t.Errorf("deleting a sub-bucket that holds a key: problem = %v", p)
	}
	f.must(http.StatusNoContent, call{method: "DELETE", path: "/buckets/some-bucket/object?key=reports/2026.csv"})

	// Empty, it is deleted, and the repeat is the answer that was given.
	del := call{method: "DELETE", path: path, headers: map[string]string{"Idempotency-Key": "sub-2"}}
	f.must(http.StatusNoContent, del)
	if again := f.must(http.StatusNoContent, del); again.header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("the repeat of a DELETE was not replayed")
	}
	if n := succeeded(f, start, "s3:DeleteSubBucket", "some-bucket"); n != 1 {
		t.Errorf("a DELETE sent twice with one key was recorded %d times", n)
	}
	if p := f.must(http.StatusNotFound, call{method: "GET", path: path}).problem(t); p["code"] != "NoSuchSubBucket" {
		t.Errorf("GET of a deleted sub-bucket: problem = %v", p)
	}

	// No audit record holds a policy document.
	if strings.Contains(fmt.Sprint(f.trail()), "user/dora") {
		t.Errorf("the audit trail holds a policy document")
	}
}

// TestTheEleventhSubBucketIsRefused: 409 TooManySubBuckets, and a sub-bucket
// that is there can still be put again.
func TestTheEleventhSubBucketIsRefused(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	for i := range 10 {
		f.must(http.StatusOK, call{method: "PUT", path: fmt.Sprintf("/buckets/some-bucket/subbuckets/s%d", i), body: map[string]any{}})
	}
	r := f.must(http.StatusConflict, call{method: "PUT", path: "/buckets/some-bucket/subbuckets/s10", body: map[string]any{}})
	if p := r.problem(t); p["code"] != "TooManySubBuckets" || p["title"] == "" {
		t.Errorf("the eleventh: problem = %s", r.body)
	}
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/subbuckets/s3", body: map[string]any{}})
	f.must(http.StatusNotFound, call{method: "GET", path: "/buckets/some-bucket/subbuckets/s10"})
}

// TestSubBucketsAreAuthorized: 401 without a credential; 403 to a caller
// without the action; a listing filtered to what the caller may list, and
// refused when nothing of the bucket is visible to them.
func TestSubBucketsAreAuthorized(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/subbuckets/reports", body: map[string]any{"policy": listingByDora()}})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/subbuckets/archive", body: map[string]any{}})
	reader := f.userKey("reader", iam.RoleReadOnly)
	auditor := f.userKey("watcher", iam.RoleAuditor)
	// dora holds no role that names an action of S3: what she may list of
	// the bucket is what the policy of reports gives her.
	dora := f.userKey("dora", iam.RoleAuditor)

	calls := []call{
		{method: "GET", path: "/buckets/some-bucket/subbuckets"},
		{method: "GET", path: "/buckets/some-bucket/subbuckets/reports"},
		{method: "PUT", path: "/buckets/some-bucket/subbuckets/reports", body: map[string]any{}},
		{method: "DELETE", path: "/buckets/some-bucket/subbuckets/archive"},
	}
	for _, c := range calls {
		for name, bearer := range map[string]string{"no credential": "", "a credential that is none": "AKIDNOBODY:not-a-secret"} {
			asked := c
			asked.bearer = bearer
			if r := f.do(asked); r.status != http.StatusUnauthorized || r.problem(t)["code"] != "Unauthenticated" {
				t.Errorf("%s %s with %s = %d: %s", c.method, c.path, name, r.status, r.body)
			}
		}
	}
	// read-only may list the bucket, and so is shown every sub-bucket; it
	// may neither read a policy nor change anything.
	asked := calls[0]
	asked.bearer = reader
	if r := f.do(asked); r.status != http.StatusOK || !bytes.Contains(r.body, []byte("archive")) || !bytes.Contains(r.body, []byte("reports")) {
		t.Errorf("the list for read-only = %d %s", r.status, r.body)
	}
	for _, c := range calls[1:] {
		c.bearer = reader
		r := f.do(c)
		if r.status != http.StatusForbidden || r.problem(t)["code"] != "AccessDenied" {
			t.Errorf("%s %s by read-only = %d: %s", c.method, c.path, r.status, r.body)
		}
	}
	// dora is shown the one sub-bucket she may list, without its policy.
	asked.bearer = dora
	r := f.do(asked)
	var listed []map[string]any
	if r.status == http.StatusOK {
		r.decode(t, &listed)
	}
	if r.status != http.StatusOK || len(listed) != 1 || listed[0]["name"] != "reports" || bytes.Contains(r.body, []byte("policy")) {
		t.Errorf("the list for dora = %d %s", r.status, r.body)
	}
	// The auditor sees nothing of the bucket: 403, as for a bucket that is
	// not there - which they are not told.
	for _, bucketName := range []string{"some-bucket", "no-such-bucket"} {
		c := call{method: "GET", path: "/buckets/" + bucketName + "/subbuckets", bearer: auditor}
		r := f.do(c)
		if r.status != http.StatusForbidden || r.problem(t)["code"] != "AccessDenied" {
			t.Errorf("the list of %s for an auditor = %d: %s", bucketName, r.status, r.body)
		}
	}
}

// TestSubBucketChangesNeedTheCSRFTokenOfASession: a console session's
// change without its CSRF token is refused 403 before anything else, and
// with it is made.
func TestSubBucketChangesNeedTheCSRFTokenOfASession(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	const password = "correct horse battery"
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{
		"name": "admin2", "roles": []string{iam.RoleClusterAdmin}, "password": password,
	}})
	client, csrf, signedIn := signIn(t, f, "admin2", password)
	if signedIn.status != http.StatusOK || csrf == "" {
		t.Fatalf("signing in = %d", signedIn.status)
	}
	tests := []struct {
		c    call
		want int
	}{
		{call{method: "PUT", path: "/buckets/some-bucket/policy", body: map[string]any{"document": aDocument}}, http.StatusOK},
		{call{method: "DELETE", path: "/buckets/some-bucket/policy"}, http.StatusNoContent},
		{call{method: "PUT", path: "/buckets/some-bucket/subbuckets/reports", body: map[string]any{"policy": aDocument}}, http.StatusOK},
		{call{method: "DELETE", path: "/buckets/some-bucket/subbuckets/reports"}, http.StatusNoContent},
	}
	for n, tt := range tests {
		c := tt.c
		c.client = client
		for _, keyed := range []bool{false, true} {
			c.headers = map[string]string{}
			if keyed {
				c.headers["Idempotency-Key"] = fmt.Sprintf("csrf-%d", n)
			}
			r := f.do(c)
			if r.status != http.StatusForbidden || r.problem(t)["code"] != "CSRFTokenInvalid" {
				t.Errorf("%s %s (keyed: %v) without the CSRF token = %d: %s", c.method, c.path, keyed, r.status, r.body)
			}
		}
		c.headers[httpsession.CSRFHeader] = csrf
		if r := f.do(c); r.status != tt.want {
			t.Errorf("%s %s with the CSRF token = %d, want %d: %s", c.method, c.path, r.status, tt.want, r.body)
		}
	}
}

// TestSubBucketsOfANodeWithoutTheUseCase: a node that was not given the use
// case of sub-buckets answers 501 to a caller it has authenticated, and 401
// to one it has not.
func TestSubBucketsOfANodeWithoutTheUseCase(t *testing.T) {
	t.Parallel()
	f := newFixtureWith(t, func(c *adminapi.Config) { c.SubBuckets = nil })
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	for _, c := range []call{
		{method: "GET", path: "/buckets/some-bucket/subbuckets"},
		{method: "GET", path: "/buckets/some-bucket/subbuckets/reports"},
		{method: "PUT", path: "/buckets/some-bucket/subbuckets/reports", body: map[string]any{}},
		{method: "DELETE", path: "/buckets/some-bucket/subbuckets/reports"},
	} {
		if r := f.do(c); r.status != http.StatusUnauthorized {
			t.Errorf("%s %s without a credential = %d", c.method, c.path, r.status)
		}
		if p := f.must(http.StatusNotImplemented, c).problem(t); p["code"] != "NotImplemented" || p["detail"] != nil {
			t.Errorf("%s %s: problem = %v", c.method, c.path, p)
		}
	}
}

// firstStatement returns the first statement of a policy document made by
// policyOf, to be changed in place.
func firstStatement(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	statements, ok := document["Statement"].([]any)
	if !ok || len(statements) == 0 {
		t.Fatalf("the document has no statements: %v", document)
	}
	statement, ok := statements[0].(map[string]any)
	if !ok {
		t.Fatalf("the first statement is a %T", statements[0])
	}
	return statement
}

// TestPolicyFieldsAreTheContracts: the operations whose body is checked for
// a policy field given twice are exactly those whose request body carries a
// PolicyDocument, and the field is the one that holds it.
func TestPolicyFieldsAreTheContracts(t *testing.T) {
	t.Parallel()
	var doc struct {
		Paths map[string]map[string]struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Ref string `json:"$ref"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openapi.AdminJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for path, operations := range doc.Paths {
		for method, operation := range operations {
			ref := operation.RequestBody.Content["application/json"].Schema.Ref
			schema := doc.Components.Schemas[strings.TrimPrefix(ref, "#/components/schemas/")]
			for field, property := range schema.Properties {
				if property.Ref == "#/components/schemas/PolicyDocument" {
					want[strings.ToUpper(method)+" "+adminapi.BasePath+path] = field
				}
			}
		}
	}
	got := adminapi.PolicyFields()
	if len(want) == 0 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("policy fields = %v, the contract's = %v", got, want)
	}
}

// TestPolicyDocumentsAreReadAsSent: a policy reaches its parser as the
// client sent it. A key given twice in it - an Effect that says Deny and
// then Allow, a condition key twice - is refused on every route that takes a
// policy, as S3's PutBucketPolicy refuses it, and not judged by the last
// value; so is a body that gives the policy field itself twice, in two
// cases; the 20 KiB limit is that of the bytes sent; and a bucket's policy
// is answered with its keys in the order they were set.
func TestPolicyDocumentsAreReadAsSent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{"name": "dora", "roles": []string{iam.RoleReadOnly}}})
	f.must(http.StatusCreated, call{method: "POST", path: "/policies", body: map[string]any{"name": "existing", "document": map[string]any{
		"Version": "2012-10-17", "Statement": []any{map[string]any{"Effect": "Allow", "Action": "s3:GetObject", "Resource": "*"}},
	}}})
	media := map[string]string{"Content-Type": "application/json"}

	// Each route, the field it carries the policy in, and the policy
	// with the key given twice.
	const principal = `"Principal":{"AWS":"arn:aws:iam:::user/dora"},`
	effectTwice := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny",` + principal +
		`"Action":"s3:GetObject","Resource":"arn:aws:s3:::some-bucket/reports/*","Effect":"Allow"}]}`
	conditionTwice := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` + principal +
		`"Action":"s3:GetObject","Resource":"arn:aws:s3:::some-bucket/reports/*",` +
		`"Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"},"IpAddress":{"aws:SourceIp":"0.0.0.0/0"}}}]}`
	identity := func(doc string) string { return strings.ReplaceAll(doc, principal, "") }
	routes := []struct {
		method, path, field, code string
		identity                  bool
	}{
		{"PUT", "/buckets/some-bucket/policy", "document", "InvalidPolicy", false},
		{"PUT", "/buckets/some-bucket/subbuckets/reports", "policy", "InvalidPolicy", false},
		{"POST", "/policies", "document", "InvalidArgument", true},
		{"PUT", "/policies/existing", "document", "InvalidArgument", true},
		{"POST", "/users/dora/keys", "inline_policy", "InvalidArgument", true},
	}
	for _, route := range routes {
		extra := ""
		if route.path == "/policies" {
			extra = `"name":"twice",`
		}
		for name, document := range map[string]string{"Effect twice": effectTwice, "a condition key twice": conditionTwice} {
			if route.identity {
				document = identity(document)
			}
			body := `{` + extra + `"` + route.field + `":` + document + `}`
			r := f.must(http.StatusBadRequest, call{method: route.method, path: route.path, raw: []byte(body), headers: media})
			if p := r.problem(t); p["code"] != route.code {
				t.Errorf("%s %s with %s: problem = %s", route.method, route.path, name, r.body)
			}
		}
		// The field twice, in two cases: the decoder would keep the last.
		document := identity(string(mustJSON(t, aDocument)))
		if !route.identity {
			document = string(mustJSON(t, aDocument))
		}
		body := `{` + extra + `"` + route.field + `":{"Version":"2012-10-17","Statement":[]},"` + strings.ToUpper(route.field) + `":` + document + `}`
		r := f.must(http.StatusBadRequest, call{method: route.method, path: route.path, raw: []byte(body), headers: media})
		if p := r.problem(t); p["code"] != "InvalidArgument" {
			t.Errorf("%s %s with the field twice: problem = %s", route.method, route.path, r.body)
		}
	}
	// Nothing of it was kept.
	if r := f.must(http.StatusOK, call{method: "GET", path: "/buckets/some-bucket/policy"}); strings.TrimSpace(string(r.body)) != "{}" {
		t.Errorf("the bucket's policy after refused ones = %s", r.body)
	}
	f.must(http.StatusNotFound, call{method: "GET", path: "/buckets/some-bucket/subbuckets/reports"})
	f.must(http.StatusNotFound, call{method: "GET", path: "/policies/twice"})

	// More than 20 KiB as sent, if not once the spaces are taken out: the
	// size is the client's.
	spaced, err := json.MarshalIndent(aDocument, "", strings.Repeat(" ", 2000))
	if err != nil {
		t.Fatal(err)
	}
	if compact := mustJSON(t, aDocument); len(spaced) <= iam.MaxPolicyBytes || len(compact) > iam.MaxPolicyBytes {
		t.Fatalf("the document is %d bytes as sent and %d without spaces", len(spaced), len(compact))
	}
	for _, route := range routes[:2] {
		body := `{"` + route.field + `":` + string(spaced) + `}`
		r := f.must(http.StatusBadRequest, call{method: route.method, path: route.path, raw: []byte(body), headers: media})
		if p := r.problem(t); p["code"] != "InvalidPolicy" {
			t.Errorf("%s %s with %d bytes: problem = %s", route.method, route.path, len(spaced), r.body)
		}
	}

	// As it was set: the keys in the order the client gave them.
	ordered := `{"Statement":[{"Resource":"arn:aws:s3:::some-bucket/reports/*","Action":"s3:GetObject",` + principal + `"Effect":"Allow"}],"Version":"2012-10-17"}`
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/some-bucket/policy", raw: []byte(`{"document": ` + ordered + `}`), headers: media})
	if r := f.must(http.StatusOK, call{method: "GET", path: "/buckets/some-bucket/policy"}); strings.TrimSpace(string(r.body)) != `{"document":`+ordered+`}` {
		t.Errorf("the policy as it is read = %s, want it as it was set: %s", r.body, ordered)
	}
}

// mustJSON is v encoded as JSON.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
