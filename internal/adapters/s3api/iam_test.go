package s3api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// TestRequestsSignedWithAUsersKey: the S3 API with API keys of users instead
// of the root credential. What a request may do is what the user's policies
// say, enforced by the one authorizer; a key that is revoked stops with the
// next request; refusals and failed signatures are in the audit trail.
func TestRequestsSignedWithAUsersKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestServer(t, "")
	root := s.principal()
	s.createBucket("photos")
	s.createBucket("finance")
	s.mustStatus(s.putObject("/photos/a.jpg", []byte("picture")), http.StatusOK)
	s.mustStatus(s.putObject("/finance/q3.csv", []byte("numbers")), http.StatusOK)

	policy := `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:ListBucket"],"Resource":["arn:aws:s3:::photos","arn:aws:s3:::photos/*"]},
		{"Effect":"Deny","Action":"s3:*","Resource":"arn:aws:s3:::photos/private/*"}]}`
	if err := s.iam.PutPolicy(ctx, root, "photos-rw", []byte(policy), true); err != nil {
		t.Fatal(err)
	}
	if err := s.iam.PutRole(ctx, root, iam.Role{Name: "photographer", Policies: []string{"photos-rw"}}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.iam.CreateUser(ctx, root, app.UserInput{Name: "alice", Roles: []string{"photographer"}}); err != nil {
		t.Fatal(err)
	}
	creds, _, err := s.iam.CreateAPIKey(ctx, root, app.APIKeyInput{Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	as := func(c call) reply {
		c.keyID, c.key = creds.AccessKeyID, []byte(creds.SecretKey)
		return s.do(c)
	}

	// What the policy allows.
	if r := as(call{method: http.MethodGet, path: "/photos/a.jpg"}); r.status != http.StatusOK || string(r.body) != "picture" {
		t.Fatalf("alice reading her bucket: %d %s", r.status, r.body)
	}
	s.mustStatus(as(call{method: http.MethodPut, path: "/photos/b.jpg", body: []byte("another")}), http.StatusOK)
	s.mustStatus(as(call{method: http.MethodGet, path: "/photos?list-type=2"}), http.StatusOK)

	// What it does not: answered 403 AccessDenied, with nothing about the
	// object - not even whether it exists.
	denied := map[string]call{
		"another bucket":              {method: http.MethodGet, path: "/finance/q3.csv"},
		"an object that is not there": {method: http.MethodGet, path: "/finance/nothing"},
		"a bucket that is not there":  {method: http.MethodGet, path: "/nowhere/x"},
		"explicitly denied prefix":    {method: http.MethodPut, path: "/photos/private/x", body: []byte("no")},
		"deleting":                    {method: http.MethodDelete, path: "/photos/a.jpg"},
		"listing every bucket":        {method: http.MethodGet, path: "/"},
		"making a bucket":             {method: http.MethodPut, path: "/mine"},
		"deleting a bucket":           {method: http.MethodDelete, path: "/photos"},
		"listing another bucket":      {method: http.MethodGet, path: "/finance?list-type=2"},
		"head of another's object":    {method: http.MethodHead, path: "/finance/q3.csv"},
	}
	for name, c := range denied {
		r := as(c)
		if r.status != http.StatusForbidden || (c.method != http.MethodHead && s.errorCode(r) != "AccessDenied") {
			t.Errorf("alice %s: %d %s, want 403 AccessDenied", name, r.status, r.body)
		}
		if strings.Contains(string(r.body), "numbers") {
			t.Fatalf("alice %s: the response carries the object", name)
		}
	}
	// Nothing she was refused happened.
	s.mustStatus(s.do(call{method: http.MethodGet, path: "/photos/a.jpg"}), http.StatusOK)
	s.expectError(s.do(call{method: http.MethodGet, path: "/photos/private/x"}), http.StatusNotFound, "NoSuchKey")

	// A wrong secret for a real key, and a key that does not exist: both
	// are refused the same way as before there were users.
	s.expectError(s.do(call{method: http.MethodGet, path: "/photos/a.jpg", keyID: creds.AccessKeyID, key: []byte("not-the-secret-of-this-key-0000")}),
		http.StatusForbidden, "SignatureDoesNotMatch")
	s.expectError(s.do(call{method: http.MethodGet, path: "/photos/a.jpg", keyID: "AKIDNOSUCHKEY0000000", key: []byte(creds.SecretKey)}),
		http.StatusForbidden, "InvalidAccessKeyId")

	// Her roles change: the next request has the new ones.
	if _, err := s.iam.UpdateUser(ctx, root, app.UserInput{Name: "alice", Roles: []string{"photographer", iam.RoleReadOnly}}); err != nil {
		t.Fatal(err)
	}
	s.mustStatus(as(call{method: http.MethodGet, path: "/finance/q3.csv"}), http.StatusOK)
	// The explicit deny still wins over the new allow.
	s.expectError(as(call{method: http.MethodGet, path: "/photos/private/x"}), http.StatusForbidden, "AccessDenied")

	// The key is revoked: the very next request is refused.
	if err := s.iam.DeleteAPIKey(ctx, root, creds.AccessKeyID); err != nil {
		t.Fatal(err)
	}
	s.expectError(as(call{method: http.MethodGet, path: "/photos/a.jpg"}), http.StatusForbidden, "InvalidAccessKeyId")

	// The trail has her refusals and the failed authentications, with her
	// address, and no secret.
	s.auditor.Flush(ctx)
	records, err := s.iamStore.ListAudit(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := audit.Verify(audit.Record{}, records); err != nil {
		t.Fatalf("the audit trail does not verify: %v", err)
	}
	// One record per kind of refusal, and a count of its repeats.
	count := func(r audit.Record) int {
		if repeated := r.Details["repeated"]; repeated != "" {
			n, _ := strconv.Atoi(repeated)
			return n
		}
		return 1
	}
	var denials, authFailures int
	for _, r := range records {
		switch {
		case r.Outcome == audit.Denied && r.Actor == "user/alice":
			denials += count(r)
			if r.SourceIP != "127.0.0.1" || r.AccessKeyID != creds.AccessKeyID || !strings.HasPrefix(r.Action, "s3:") {
				t.Errorf("denial record = %+v", r)
			}
		case r.Action == "auth:Authenticate" && r.Outcome == audit.Failed:
			authFailures += count(r)
			if r.SourceIP != "127.0.0.1" || r.Details["reason"] == "" {
				t.Errorf("authentication failure record = %+v", r)
			}
		}
		for _, value := range r.Details {
			if strings.Contains(value, creds.SecretKey) {
				t.Fatal("a secret is in the audit trail")
			}
		}
	}
	if denials != 11 || authFailures != 3 {
		t.Errorf("%d denials and %d authentication failures accounted for in the trail; want 11 and 3", denials, authFailures)
	}
	if strings.Contains(s.logs.String(), creds.SecretKey) {
		t.Error("a secret is in the log")
	}
}

// TestPolicyConditionsSeeTheRequest: a policy that allows only from some
// addresses, or only over TLS, is decided on how the request really arrived.
func TestPolicyConditionsSeeTheRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestServer(t, "")
	root := s.principal()
	s.createBucket("photos")
	s.mustStatus(s.putObject("/photos/a.jpg", []byte("picture")), http.StatusOK)

	user := func(name, condition string) app.Credentials {
		policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":` + condition + `}]}`
		if err := s.iam.PutPolicy(ctx, root, name, []byte(policy), true); err != nil {
			t.Fatal(err)
		}
		if err := s.iam.PutRole(ctx, root, iam.Role{Name: name, Policies: []string{name}}, true); err != nil {
			t.Fatal(err)
		}
		if _, err := s.iam.CreateUser(ctx, root, app.UserInput{Name: name, Roles: []string{name}}); err != nil {
			t.Fatal(err)
		}
		creds, _, err := s.iam.CreateAPIKey(ctx, root, app.APIKeyInput{Owner: name})
		if err != nil {
			t.Fatal(err)
		}
		return creds
	}
	get := func(creds app.Credentials) int {
		return s.do(call{method: http.MethodGet, path: "/photos/a.jpg", keyID: creds.AccessKeyID, key: []byte(creds.SecretKey)}).status
	}
	// The test server is reached over loopback, without TLS.
	if status := get(user("from-loopback", `{"IpAddress":{"aws:SourceIp":"127.0.0.0/8"}}`)); status != http.StatusOK {
		t.Errorf("allowed from loopback: status %d", status)
	}
	if status := get(user("from-office", `{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}`)); status != http.StatusForbidden {
		t.Errorf("allowed from 10.0.0.0/8 only, asked from loopback: status %d", status)
	}
	if status := get(user("tls-only", `{"Bool":{"aws:SecureTransport":"true"}}`)); status != http.StatusForbidden {
		t.Errorf("allowed over TLS only, asked without: status %d", status)
	}
	if status := get(user("plain-only", `{"Bool":{"aws:SecureTransport":"false"}}`)); status != http.StatusOK {
		t.Errorf("allowed without TLS: status %d", status)
	}
	if status := get(user("until-2020", `{"DateLessThan":{"aws:CurrentTime":"2020-01-01T00:00:00Z"}}`)); status != http.StatusForbidden {
		t.Errorf("allowed until 2020: status %d", status)
	}
}
