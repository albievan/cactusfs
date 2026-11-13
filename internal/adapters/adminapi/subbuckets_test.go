package adminapi_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/albievan/cactusfs/api/openapi"
	"github.com/albievan/cactusfs/internal/adapters/httpsession"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// The operations of bucket policies and sub-buckets (PLAN C.0, ADR-0017)
// are in the contract ahead of their use cases. Until PLAN C.6 writes them,
// each answers 501 as a problem to a caller it has authenticated - before
// the authorizer is asked, as the other operations that are not written yet
// do (TestOperationsThatAreNotWrittenYet) - and 401 to one it has not. Each
// that changes something is declared as every other such operation is:
// with Idempotency-Key and X-CSRF-Token, and a session without its CSRF
// token is refused before it is told anything else.

// aDocument is a policy that the bucket and its sub-bucket "reports" could
// both have (ADR-0011, ADR-0017), for the bodies of these requests.
var aDocument = map[string]any{
	"Version": "2012-10-17",
	"Statement": []any{map[string]any{
		"Effect": "Allow", "Principal": map[string]any{"AWS": "arn:aws:iam:::user/dora"},
		"Action": "s3:GetObject", "Resource": "arn:aws:s3:::some-bucket/reports/*",
	}},
}

// notWrittenYet asks the operation the contract names id, as c, of every
// kind of caller, and checks the contract's declaration of it against
// action.
func notWrittenYet(t *testing.T, id, action string, c call) {
	t.Helper()

	// The contract declares it as its kind of operation is declared.
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
	found := false
	for path, operations := range doc.Paths {
		for method, operation := range operations {
			if operation.OperationID != id {
				continue
			}
			found = true
			if operation.Action != action {
				t.Errorf("%s (%s %s) needs %q, want %q", id, method, path, operation.Action, action)
			}
			var refs []string
			for _, p := range operation.Parameters {
				refs = append(refs, p.Ref)
			}
			for _, p := range []string{"#/components/parameters/IdempotencyKey", "#/components/parameters/CSRFToken"} {
				if mutating := method != "get"; mutating != slices.Contains(refs, p) {
					t.Errorf("%s (%s %s): parameters %v, %s declared: %v", id, method, path, refs, p, !mutating)
				}
			}
		}
	}
	if !found {
		t.Fatalf("the contract has no operation %s", id)
	}

	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "some-bucket"}})

	// Without a credential, and with one nobody has: 401.
	for name, bearer := range map[string]string{"no credential": "", "a credential that is none": "AKIDNOBODY:not-a-secret"} {
		asked := c
		asked.bearer = bearer
		r := f.do(asked)
		if r.status != http.StatusUnauthorized {
			t.Errorf("%s %s with %s = %d, want 401: %s", c.method, c.path, name, r.status, r.body)
			continue
		}
		if p := r.problem(t); p["code"] != "Unauthenticated" {
			t.Errorf("%s %s with %s: problem = %s", c.method, c.path, name, r.body)
		}
	}

	// With an API key, and again with a key that makes it safe to repeat:
	// 501, and nothing more is said.
	tries := []call{c}
	if c.method != http.MethodGet {
		keyed := c
		keyed.headers = map[string]string{"Idempotency-Key": "c0-" + id}
		tries = append(tries, keyed, keyed)
	}
	for n, asked := range tries {
		asked.bearer = f.root
		r := f.do(asked)
		if r.status != http.StatusNotImplemented {
			t.Errorf("%s %s (try %d) = %d, want 501: %s", c.method, c.path, n, r.status, r.body)
			continue
		}
		if p := r.problem(t); p["code"] != "NotImplemented" || p["detail"] != nil || p["status"] != float64(http.StatusNotImplemented) {
			t.Errorf("%s %s (try %d): problem = %s", c.method, c.path, n, r.body)
		}
	}

	// A console session: a change without the session's CSRF token is
	// refused 403 before it is told anything else; with it, 501.
	const password = "correct horse battery"
	f.must(http.StatusCreated, call{method: "POST", path: "/users", body: map[string]any{
		"name": "dora", "roles": []string{iam.RoleClusterAdmin}, "password": password,
	}})
	client, csrf, signedIn := signIn(t, f, "dora", password)
	if signedIn.status != http.StatusOK || csrf == "" {
		t.Fatalf("signing in = %d", signedIn.status)
	}
	asked := c
	asked.client = client
	if c.method != http.MethodGet {
		r := f.do(asked)
		if r.status != http.StatusForbidden {
			t.Errorf("%s %s by a session without its CSRF token = %d, want 403: %s", c.method, c.path, r.status, r.body)
		} else if p := r.problem(t); p["code"] != "CSRFTokenInvalid" {
			t.Errorf("%s %s by a session without its CSRF token: problem = %s", c.method, c.path, r.body)
		}
		asked.headers = map[string]string{httpsession.CSRFHeader: csrf}
	}
	if r := f.do(asked); r.status != http.StatusNotImplemented {
		t.Errorf("%s %s by a session = %d, want 501: %s", c.method, c.path, r.status, r.body)
	} else if p := r.problem(t); p["code"] != "NotImplemented" || p["detail"] != nil {
		t.Errorf("%s %s by a session: problem = %s", c.method, c.path, r.body)
	}
}

// TestGetBucketPolicyIsNotWrittenYet: GET /buckets/{bucket}/policy.
func TestGetBucketPolicyIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "getBucketPolicy", "s3:GetBucketPolicy", call{method: "GET", path: "/buckets/some-bucket/policy"})
}

// TestPutBucketPolicyIsNotWrittenYet: PUT /buckets/{bucket}/policy.
func TestPutBucketPolicyIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "putBucketPolicy", "s3:PutBucketPolicy", call{
		method: "PUT", path: "/buckets/some-bucket/policy", body: map[string]any{"document": aDocument},
	})
	// What the contract requires of the body is there before anything
	// else is said, as for every operation.
	f := newFixture(t)
	r := f.must(http.StatusBadRequest, call{method: "PUT", path: "/buckets/some-bucket/policy", body: map[string]any{}})
	if p := r.problem(t); p["code"] != "InvalidArgument" {
		t.Errorf("a body without document: problem = %s", r.body)
	}
}

// TestDeleteBucketPolicyIsNotWrittenYet: DELETE /buckets/{bucket}/policy.
func TestDeleteBucketPolicyIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "deleteBucketPolicy", "s3:DeleteBucketPolicy", call{method: "DELETE", path: "/buckets/some-bucket/policy"})
}

// TestListSubBucketsIsNotWrittenYet: GET /buckets/{bucket}/subbuckets.
func TestListSubBucketsIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "listSubBuckets", "s3:ListSubBuckets", call{method: "GET", path: "/buckets/some-bucket/subbuckets"})
}

// TestGetSubBucketIsNotWrittenYet: GET /buckets/{bucket}/subbuckets/{name}.
func TestGetSubBucketIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "getSubBucket", "s3:GetSubBucketPolicy", call{method: "GET", path: "/buckets/some-bucket/subbuckets/reports"})
}

// TestPutSubBucketIsNotWrittenYet: PUT /buckets/{bucket}/subbuckets/{name},
// with a policy in the body and without one.
func TestPutSubBucketIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]map[string]any{"with a policy": {"policy": aDocument}, "without one": {}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			notWrittenYet(t, "putSubBucket", "s3:PutSubBucket", call{
				method: "PUT", path: "/buckets/some-bucket/subbuckets/reports", body: body,
			})
		})
	}
}

// TestDeleteSubBucketIsNotWrittenYet: DELETE /buckets/{bucket}/subbuckets/{name}.
func TestDeleteSubBucketIsNotWrittenYet(t *testing.T) {
	t.Parallel()
	notWrittenYet(t, "deleteSubBucket", "s3:DeleteSubBucket", call{method: "DELETE", path: "/buckets/some-bucket/subbuckets/reports"})
}
