package iam_test

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

const publicRead = `{"Version":"2012-10-17","Statement":[
	{"Sid":"read","Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::site","arn:aws:s3:::site/*"]}]}`

func mustBucketPolicy(t *testing.T, document, bucket string) *iam.BucketPolicy {
	t.Helper()
	policy, err := iam.ParseBucketPolicy([]byte(document), bucket)
	if err != nil {
		t.Fatalf("ParseBucketPolicy: %v", err)
	}
	return &policy
}

func TestParseBucketPolicy(t *testing.T) {
	t.Parallel()
	statement := func(principal, rest string) string {
		return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow",` + principal + rest + `}]}`
	}
	about := `"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"`
	good := map[string]string{
		"anyone":             statement(`"Principal":"*",`, about),
		"anyone, as AWS":     statement(`"Principal":{"AWS":"*"},`, about),
		"a user":             statement(`"Principal":{"AWS":"arn:aws:iam:::user/alice"},`, about),
		"users":              statement(`"Principal":{"AWS":["arn:aws:iam:::user/alice","arn:aws:iam:::user/bob"]},`, about),
		"the bucket itself":  statement(`"Principal":"*",`, `"Action":"s3:ListBucket","Resource":"arn:aws:s3:::site"`),
		"with a condition":   statement(`"Principal":"*",`, about+`,"Condition":{"Bool":{"aws:SecureTransport":"true"}}`),
		"a deny for anyone":  `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::site/*"}]}`,
		"the public example": publicRead,
	}
	for name, document := range good {
		if _, err := iam.ParseBucketPolicy([]byte(document), "site"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]string{
		"no principal":                statement("", about),
		"a principal that is a name":  statement(`"Principal":"alice",`, about),
		"an empty principal":          statement(`"Principal":{"AWS":[]},`, about),
		"a principal of another kind": statement(`"Principal":{"Service":"s3.amazonaws.com"},`, about),
		"a user that is a pattern":    statement(`"Principal":{"AWS":"arn:aws:iam:::user/*"},`, about),
		"a user without a name":       statement(`"Principal":{"AWS":"arn:aws:iam:::user/"},`, about),
		"an account":                  statement(`"Principal":{"AWS":"arn:aws:iam::123456789012:root"},`, about),
		"NotPrincipal":                statement(`"NotPrincipal":"*",`, about),
		"the principal twice":         statement(`"Principal":"*","Principal":{"AWS":"arn:aws:iam:::user/alice"},`, about),
		"principal in another case":   statement(`"principal":"*",`, about),
		"another bucket":              statement(`"Principal":"*",`, `"Action":"s3:GetObject","Resource":"arn:aws:s3:::other/*"`),
		"a bucket with this prefix":   statement(`"Principal":"*",`, `"Action":"s3:GetObject","Resource":"arn:aws:s3:::site2/*"`),
		"every bucket":                statement(`"Principal":"*",`, `"Action":"s3:GetObject","Resource":"*"`),
		"buckets by pattern":          statement(`"Principal":"*",`, `"Action":"s3:GetObject","Resource":"arn:aws:s3:::*"`),
		"an administration action":    statement(`"Principal":"*",`, `"Action":"admin:ListUsers","Resource":"arn:aws:s3:::site/*"`),
		"every action of everything":  statement(`"Principal":"*",`, `"Action":"*","Resource":"arn:aws:s3:::site/*"`),
		"no statements":               `{"Version":"2012-10-17","Statement":[]}`,
		"not JSON":                    `Principal: *`,
		"data after the document":     publicRead + `{}`,
		"too large":                   statement(`"Principal":"*",`, about+`,"Sid":"`+strings.Repeat("x", iam.MaxPolicyBytes)+`"`),
	}
	for name, document := range bad {
		if _, err := iam.ParseBucketPolicy([]byte(document), "site"); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
}

// TestBucketPolicyDecisions: what a bucket policy can and cannot do for
// each kind of caller.
func TestBucketPolicyDecisions(t *testing.T) {
	t.Parallel()
	public := mustBucketPolicy(t, publicRead, "site")
	everything := mustBucketPolicy(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":["arn:aws:s3:::site","arn:aws:s3:::site/*"]}]}`, "site")
	forAlice := mustBucketPolicy(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/alice"},"Action":["s3:GetObject","s3:PutObject"],"Resource":"arn:aws:s3:::site/*"}]}`, "site")
	denyDrafts := mustBucketPolicy(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"},
		{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::site/drafts/*"}]}`, "site")
	tlsOnly := mustBucketPolicy(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*","Condition":{"Bool":{"aws:SecureTransport":"true"}}}]}`, "site")

	doc := func(actions ...string) iam.Document {
		return iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: actions, Resources: []string{"*"}}}}
	}
	nobody := iam.Principal{}
	overTLS := iam.Principal{Session: iam.Session{SecureTransport: true, SourceIP: netip.MustParseAddr("192.0.2.1")}}
	alice := iam.Principal{AccessKeyID: "A", User: "alice"}
	bob := iam.Principal{AccessKeyID: "B", User: "bob"}
	reader := iam.Principal{AccessKeyID: "R", User: "reader", Policies: []iam.Document{doc("s3:GetObject", "s3:PutObject")}}
	bounded := doc("s3:PutObject")
	aliceKey := iam.Principal{AccessKeyID: "K", User: "alice", Boundary: &bounded}
	root := iam.Principal{AccessKeyID: "ROOT", Root: true}

	page, draft := iam.Resource{Bucket: "site", Key: "index.html"}, iam.Resource{Bucket: "site", Key: "drafts/new.html"}
	tests := []struct {
		name      string
		principal iam.Principal
		policy    *iam.BucketPolicy
		action    iam.Action
		resource  iam.Resource
		allowed   bool
	}{
		{"nobody, no policy", nobody, nil, iam.ActionGetObject, page, false},
		{"nobody reads what is public", nobody, public, iam.ActionGetObject, page, true},
		{"nobody lists what is public", nobody, public, iam.ActionListBucket, iam.Resource{Bucket: "site"}, true},
		{"nobody reads another bucket", nobody, public, iam.ActionGetObject, iam.Resource{Bucket: "other", Key: "k"}, false},
		{"nobody writes, whatever the policy says", nobody, everything, iam.ActionPutObject, page, false},
		{"nobody deletes, whatever the policy says", nobody, everything, iam.ActionDeleteObject, page, false},
		{"nobody deletes a version", nobody, everything, iam.ActionDeleteObjectVersion, page, false},
		{"nobody changes the policy", nobody, everything, iam.ActionPutBucketPolicy, iam.Resource{Bucket: "site"}, false},
		{"nobody reads the policy", nobody, everything, iam.ActionGetBucketPolicy, iam.Resource{Bucket: "site"}, false},
		{"nobody lifts a retention", nobody, everything, iam.ActionOverrideGovernance, page, false},
		{"nobody aborts an upload", nobody, everything, iam.ActionAbortMultipartUpload, page, false},
		{"nobody is not a named user", nobody, forAlice, iam.ActionGetObject, page, false},
		{"nobody reads a denied part", nobody, denyDrafts, iam.ActionGetObject, draft, false},
		{"nobody reads beside the denied part", nobody, denyDrafts, iam.ActionGetObject, page, true},
		{"nobody without TLS, where TLS is the condition", nobody, tlsOnly, iam.ActionGetObject, page, false},
		{"nobody over TLS, where TLS is the condition", overTLS, tlsOnly, iam.ActionGetObject, page, true},

		{"a user with no policy of their own, named by the bucket", alice, forAlice, iam.ActionPutObject, page, true},
		{"another user, not named", bob, forAlice, iam.ActionPutObject, page, false},
		{"a user named for less than they ask", alice, forAlice, iam.ActionDeleteObject, page, false},
		{"any user, where anyone may", bob, everything, iam.ActionDeleteObject, page, true},
		{"a user allowed by their own policy, no bucket policy", reader, nil, iam.ActionGetObject, page, true},
		{"a user allowed by their own policy, denied by the bucket", reader, denyDrafts, iam.ActionGetObject, draft, false},
		{"a user allowed by their own policy, the bucket silent", reader, forAlice, iam.ActionPutObject, page, true},
		{"a key bounded to less than the bucket gives its owner", aliceKey, forAlice, iam.ActionGetObject, page, false},
		{"a key within its bound", aliceKey, forAlice, iam.ActionPutObject, page, true},
		{"the bypass, by a wildcard of the bucket", bob, everything, iam.ActionOverrideGovernance, page, false},

		{"root is bound by no bucket policy", root, denyDrafts, iam.ActionDeleteObject, draft, true},
	}
	for _, tt := range tests {
		err := iam.Decide(iam.Request{Principal: tt.principal, Action: tt.action, Resource: tt.resource, BucketPolicy: tt.policy})
		if (err == nil) != tt.allowed || (err != nil && !errors.Is(err, domain.ErrAccessDenied)) {
			t.Errorf("%s: %v, want allowed = %v", tt.name, err, tt.allowed)
		}
	}
	if !public.OpenToAnyone() || forAlice.OpenToAnyone() || !denyDrafts.OpenToAnyone() {
		t.Error("OpenToAnyone")
	}
	// The summary counts the statements and says the same of who is let in.
	for name, policy := range map[string]*iam.BucketPolicy{"public": public, "forAlice": forAlice, "denyDrafts": denyDrafts} {
		want := iam.BucketPolicySummary{Statements: len(policy.Document.Statements), OpenToAnyone: policy.OpenToAnyone()}
		if got := policy.Summary(); got != want || got.Statements == 0 {
			t.Errorf("%s: Summary = %+v, want %+v", name, got, want)
		}
	}
}

// FuzzBucketPolicy: whatever the document, parsing never panics; what
// parses has a principal for every statement and is about its bucket only;
// and no policy lets an anonymous caller do anything but read.
func FuzzBucketPolicy(f *testing.F) {
	f.Add([]byte(publicRead), "s3:PutObject", "index.html")
	f.Add([]byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":"s3:*","Resource":"arn:aws:s3:::site/*"}]}`), "s3:DeleteObject", "k")
	f.Add([]byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:G*","Resource":"arn:aws:s3:::site"}]}`), "s3:GetObject", "")
	f.Fuzz(func(t *testing.T, document []byte, action, key string) {
		policy, err := iam.ParseBucketPolicy(document, "site")
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("untyped error: %v", err)
			}
			return
		}
		if len(policy.Principals) != len(policy.Document.Statements) {
			t.Fatalf("%d statements, %d principals", len(policy.Document.Statements), len(policy.Principals))
		}
		for i, s := range policy.Document.Statements {
			if len(policy.Principals[i]) == 0 {
				t.Fatalf("statement %d is about nobody", i)
			}
			for _, resource := range s.Resources {
				if resource != "arn:aws:s3:::site" && !strings.HasPrefix(resource, "arn:aws:s3:::site/") {
					t.Fatalf("statement %d is about %q", i, resource)
				}
			}
		}
		request := iam.Request{Principal: iam.Principal{}, Action: iam.Action(action), Resource: iam.Resource{Bucket: "site", Key: key}, BucketPolicy: &policy}
		if iam.Decide(request) == nil {
			switch iam.Action(action) {
			case iam.ActionGetObject, iam.ActionGetObjectVersion, iam.ActionListBucket, iam.ActionListBucketVersions, iam.ActionGetBucketLocation:
			default:
				t.Fatalf("an anonymous caller was allowed %q by %s", action, document)
			}
		}
		// A deny for everyone is a deny for a user their own policy allows.
		user := iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{{Version: iam.PolicyVersion, Statements: []iam.Statement{{
			Effect: iam.Allow, Actions: []string{"s3:*"}, Resources: []string{"*"},
		}}}}}
		without := iam.Decide(iam.Request{Principal: user, Action: iam.Action(action), Resource: request.Resource})
		with := iam.Decide(iam.Request{Principal: user, Action: iam.Action(action), Resource: request.Resource, BucketPolicy: &policy})
		if without != nil && with == nil && !policyAllows(policy) {
			t.Fatalf("a bucket policy without an Allow allowed %q", action)
		}
	})
}

func policyAllows(p iam.BucketPolicy) bool {
	for _, s := range p.Document.Statements {
		if s.Effect == iam.Allow {
			return true
		}
	}
	return false
}

// TestServicePrincipal: the lifecycle service deletes and aborts, and does
// nothing else - above all, it lifts no retention. It is a service only when
// it is nothing else, and no policy binds or widens it.
func TestServicePrincipal(t *testing.T) {
	t.Parallel()
	service := iam.Principal{Service: iam.ServiceLifecycle}
	resource := iam.Resource{Bucket: "site", Key: "k"}
	deny := mustBucketPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::site/*"}]}`, "site")
	for action, want := range map[iam.Action]bool{
		iam.ActionDeleteObject: true, iam.ActionDeleteObjectVersion: true, iam.ActionAbortMultipartUpload: true,
		iam.ActionOverrideGovernance: false, iam.ActionGetObject: false, iam.ActionPutObject: false, iam.ActionDeleteBucket: false,
		iam.ActionPutBucketPolicy: false, iam.ActionPutLifecycle: false, iam.ActionPutObjectRetention: false, iam.ActionPutObjectLegalHold: false,
		iam.ActionListBucket: false, "admin:ListUsers": false, iam.ActionNodeRPC: false,
	} {
		for _, policy := range []*iam.BucketPolicy{nil, deny} {
			err := iam.Decide(iam.Request{Principal: service, Action: action, Resource: resource, BucketPolicy: policy})
			if (err == nil) != want {
				t.Errorf("the lifecycle service and %s: %v, want allowed = %v", action, err, want)
			}
		}
	}
	all := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"s3:*", "s3:BypassGovernanceRetention"}, Resources: []string{"*"}}}}
	for name, p := range map[string]iam.Principal{
		"another service":          {Service: "backup"},
		"a service that is root":   {Service: iam.ServiceLifecycle, Root: true, AccessKeyID: "ROOT"},
		"a service with a key":     {Service: iam.ServiceLifecycle, AccessKeyID: "K"},
		"a service that is a user": {Service: iam.ServiceLifecycle, User: "u", AccessKeyID: "K", Policies: []iam.Document{all}},
		"a service that is a node": {Service: iam.ServiceLifecycle, NodeID: "n1"},
	} {
		for _, action := range []iam.Action{iam.ActionDeleteObject, iam.ActionGetObject, iam.ActionOverrideGovernance, iam.ActionNodeRPC} {
			if err := iam.Decide(iam.Request{Principal: p, Action: action, Resource: resource}); !errors.Is(err, domain.ErrAccessDenied) {
				t.Errorf("%s and %s: %v", name, action, err)
			}
		}
	}
	if service.Anonymous() || service.Name() != "service/lifecycle" {
		t.Errorf("the service is called %q, anonymous = %v", service.Name(), service.Anonymous())
	}
}
