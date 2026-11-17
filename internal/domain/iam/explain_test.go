package iam_test

import (
	"errors"
	"net/netip"
	"reflect"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// The policies TestExplain cites, each statement with a Sid.
const (
	siteReadWrite = `{"Version":"2012-10-17","Statement":[
		{"Sid":"read","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"},
		{"Sid":"write","Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::site/*"}]}`
	noDeleting = `{"Version":"2012-10-17","Statement":[
		{"Sid":"list","Effect":"Allow","Action":"s3:ListBucket","Resource":"*"},
		{"Sid":"no-delete","Effect":"Deny","Action":"s3:Delete*","Resource":"*"},
		{"Sid":"no-delete-site","Effect":"Deny","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::site/*"}]}`
	fromTheOffice = `{"Version":"2012-10-17","Statement":[
		{"Sid":"office","Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}]}`
	notOutside = `{"Version":"2012-10-17","Statement":[
		{"Sid":"everything","Effect":"Allow","Action":"s3:*","Resource":"*"},
		{"Sid":"outside","Effect":"Deny","Action":"s3:*","Resource":"*","Condition":{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}]}`
	onlyReading = `{"Version":"2012-10-17","Statement":[
		{"Sid":"only-read","Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`
	noPreview = `{"Version":"2012-10-17","Statement":[
		{"Sid":"no-preview","Effect":"Deny","Action":"admin:PreviewObject","Resource":"*"}]}`
	keepSite = `{"Version":"2012-10-17","Statement":[
		{"Sid":"keep","Effect":"Deny","Principal":"*","Action":"s3:Delete*","Resource":"arn:aws:s3:::site/*"}]}`
	bobReads = `{"Version":"2012-10-17","Statement":[
		{"Sid":"bob","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`
	carolReads = `{"Version":"2012-10-17","Statement":[
		{"Sid":"carol","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/carol"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`
	// A matching Deny before a matching Allow, which an explanation reads
	// on past: the Allow after it must not turn the verdict.
	denyThenAllow = `{"Version":"2012-10-17","Statement":[
		{"Sid":"deny","Effect":"Deny","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"},
		{"Sid":"allow","Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`
	denyGet = `{"Version":"2012-10-17","Statement":[
		{"Sid":"deny-get","Effect":"Deny","Action":"s3:GetObject","Resource":"*"}]}`
	bucketDenyThenAllow = `{"Version":"2012-10-17","Statement":[
		{"Sid":"deny","Effect":"Deny","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"},
		{"Sid":"allow","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`
	anyoneDenyThenAllow = `{"Version":"2012-10-17","Statement":[
		{"Sid":"deny","Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"},
		{"Sid":"allow","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`
	subDenyThenAllow = `{"Version":"2012-10-17","Statement":[
		{"Sid":"deny","Effect":"Deny","Principal":{"AWS":"arn:aws:iam:::user/alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"},
		{"Sid":"allow","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"}]}`
)

// named returns the principal of the user with the policies, named.
func named(name string, policies map[string]iam.Document, order ...string) iam.Principal {
	p := user(name)
	for _, policy := range order {
		p.Policies = append(p.Policies, policies[policy])
		p.PolicyNames = append(p.PolicyNames, policy)
	}
	return p
}

// cite is a citation for TestExplain.
func cite(source iam.Source, policy string, document, statement int, sid string, effect iam.Effect) iam.CitedStatement {
	return iam.CitedStatement{Source: source, Policy: policy, Document: document, Statement: statement, Sid: sid, Effect: effect}
}

func TestExplain(t *testing.T) {
	t.Parallel()
	docs := map[string]iam.Document{
		"site-rw":     mustPolicy(t, siteReadWrite),
		"no-deleting": mustPolicy(t, noDeleting),
		"office":      mustPolicy(t, fromTheOffice),
		"not-outside": mustPolicy(t, notOutside),
		"no-preview":  mustPolicy(t, noPreview),
		"deny-first":  mustPolicy(t, denyThenAllow),
		"deny-get":    mustPolicy(t, denyGet),
	}
	reading := mustPolicy(t, onlyReading)
	public := mustBucketPolicy(t, publicRead, "site")
	keep := mustBucketPolicy(t, keepSite, "site")
	bob := mustBucketPolicy(t, bobReads, "site")
	carol := mustBucketPolicy(t, carolReads, "site")
	sub := mustSubBucketPolicy(t, readReports, "site", "reports")
	private := mustSubBucketPolicy(t, subDocument(subStatement("Deny", `"*"`, `"s3:GetObject"`, reportKeys)), "site", "reports")
	object := iam.Resource{Bucket: "site", Key: "reports/a.csv"}
	nowhere := iam.Session{SecureTransport: true}

	bound := named("bob", docs, "site-rw")
	bound.Boundary = &reading
	locked := named("alice", docs, "site-rw")
	locked.LockedSession = true
	unnamed := named("bob", docs, "site-rw", "no-deleting")
	unnamed.PolicyNames = unnamed.PolicyNames[:1]
	away := named("dan", docs, "office")
	away.Session = nowhere
	outside := named("dan", docs, "not-outside")
	outside.Session = nowhere

	tests := []struct {
		name     string
		req      iam.Request
		allowed  bool
		source   iam.Source
		rule     iam.Rule
		cited    []iam.CitedStatement
		key      *iam.KeyPolicyVerdict
		unknown  []iam.CitedStatement
		sentinel error
	}{
		{
			name:    "root",
			req:     iam.Request{Principal: iam.Principal{AccessKeyID: "ROOT", Root: true}, Action: iam.ActionDeleteBucket, Resource: iam.Resource{Bucket: "site"}, BucketPolicy: keep},
			allowed: true, source: iam.SourceRule, rule: iam.RuleRoot,
		},
		{
			name:   "root acting as a user",
			req:    iam.Request{Principal: iam.Principal{AccessKeyID: "ROOT", Root: true, User: "alice"}, Action: iam.ActionGetObject, Resource: object},
			source: iam.SourceRule, rule: iam.RuleRoot, sentinel: domain.ErrAccessDenied,
		},
		{
			name:    "anonymous on a public bucket",
			req:     iam.Request{Action: iam.ActionGetObject, Resource: object, BucketPolicy: public},
			allowed: true, source: iam.SourceBucket,
			cited: []iam.CitedStatement{cite(iam.SourceBucket, "", 0, 0, "read", iam.Allow)},
		},
		{
			name:   "anonymous on a private bucket",
			req:    iam.Request{Action: iam.ActionGetObject, Resource: object},
			source: iam.SourceRule, rule: iam.RuleAnonymous, sentinel: domain.ErrAccessDenied,
		},
		{
			name:   "anonymous writing to a public bucket",
			req:    iam.Request{Action: iam.ActionPutObject, Resource: object, BucketPolicy: public},
			source: iam.SourceRule, rule: iam.RuleAnonymous, sentinel: domain.ErrAccessDenied,
		},
		{
			name:   "anonymous on a bucket whose policy is about somebody else",
			req:    iam.Request{Action: iam.ActionGetObject, Resource: object, BucketPolicy: bob},
			source: iam.SourceNone, sentinel: domain.ErrAccessDenied,
		},
		{
			name:   "anonymous in a private sub-bucket of a public bucket",
			req:    iam.Request{Action: iam.ActionGetObject, Resource: object, BucketPolicy: public, SubBucketPolicy: private},
			source: iam.SourceSubBucket, sentinel: domain.ErrAccessDenied,
			cited: []iam.CitedStatement{cite(iam.SourceSubBucket, "", 0, 0, "", iam.Deny), cite(iam.SourceBucket, "", 0, 0, "read", iam.Allow)},
		},
		{
			name:    "service",
			req:     iam.Request{Principal: iam.Principal{Service: iam.ServiceLifecycle}, Action: iam.ActionDeleteObject, Resource: object, BucketPolicy: keep},
			allowed: true, source: iam.SourceRule, rule: iam.RuleService,
		},
		{
			name:   "service beyond its actions",
			req:    iam.Request{Principal: iam.Principal{Service: iam.ServiceReplication}, Action: iam.ActionGetObject, Resource: object},
			source: iam.SourceRule, rule: iam.RuleService, sentinel: domain.ErrAccessDenied,
		},
		{
			name:    "node",
			req:     iam.Request{Principal: iam.Principal{NodeID: "n1"}, Action: iam.ActionNodeRPC},
			allowed: true, source: iam.SourceRule, rule: iam.RuleNode,
		},
		{
			name:   "a user asking a cluster action",
			req:    iam.Request{Principal: named("alice", docs, "site-rw"), Action: iam.ActionJoinNode, Resource: iam.Resource{Node: "n1"}},
			source: iam.SourceRule, rule: iam.RuleNode, sentinel: domain.ErrAccessDenied,
		},
		{
			name:   "locked session reading",
			req:    iam.Request{Principal: locked, Action: iam.ActionGetObject, Resource: object},
			source: iam.SourceRule, rule: iam.RuleLockedSession, sentinel: domain.ErrSessionLocked,
		},
		{
			name:    "locked session storing",
			req:     iam.Request{Principal: locked, Action: iam.ActionPutObject, Resource: object},
			allowed: true, source: iam.SourceIdentity,
			cited: []iam.CitedStatement{cite(iam.SourceIdentity, "site-rw", 0, 1, "write", iam.Allow)},
		},
		{
			name:   "no user",
			req:    iam.Request{Principal: iam.Principal{AccessKeyID: "K"}, Action: iam.ActionGetObject, Resource: object},
			source: iam.SourceRule, rule: iam.RuleNoUser, sentinel: domain.ErrAccessDenied,
		},
		{
			name:    "identity",
			req:     iam.Request{Principal: named("alice", docs, "no-deleting", "site-rw"), Action: iam.ActionGetObject, Resource: object},
			allowed: true, source: iam.SourceIdentity,
			cited: []iam.CitedStatement{cite(iam.SourceIdentity, "site-rw", 1, 0, "read", iam.Allow)},
		},
		{
			name:   "identity Deny, every one cited",
			req:    iam.Request{Principal: named("alice", docs, "no-deleting", "site-rw"), Action: iam.ActionDeleteObject, Resource: object},
			source: iam.SourceIdentity, sentinel: domain.ErrAccessDenied,
			cited: []iam.CitedStatement{
				cite(iam.SourceIdentity, "no-deleting", 0, 1, "no-delete", iam.Deny),
				cite(iam.SourceIdentity, "no-deleting", 0, 2, "no-delete-site", iam.Deny),
			},
		},
		{
			name:   "default deny",
			req:    iam.Request{Principal: named("alice", docs, "site-rw"), Action: iam.ActionPutBucketPolicy, Resource: iam.Resource{Bucket: "site"}},
			source: iam.SourceNone, sentinel: domain.ErrAccessDenied,
		},
		{
			name:   "policies not named one for one",
			req:    iam.Request{Principal: unnamed, Action: iam.ActionDeleteObject, Resource: object},
			source: iam.SourceIdentity, sentinel: domain.ErrAccessDenied,
			cited: []iam.CitedStatement{
				cite(iam.SourceIdentity, "", 1, 1, "no-delete", iam.Deny),
				cite(iam.SourceIdentity, "", 1, 2, "no-delete-site", iam.Deny),
			},
		},
		{
			name:    "bucket",
			req:     iam.Request{Principal: named("bob", docs), Action: iam.ActionGetObject, Resource: object, BucketPolicy: bob},
			allowed: true, source: iam.SourceBucket,
			cited: []iam.CitedStatement{cite(iam.SourceBucket, "", 0, 0, "bob", iam.Allow)},
		},
		{
			name:   "bucket about somebody else",
			req:    iam.Request{Principal: named("bob", docs), Action: iam.ActionGetObject, Resource: object, BucketPolicy: carol},
			source: iam.SourceNone, sentinel: domain.ErrAccessDenied,
		},
		{
			name:   "bucket Deny over an identity Allow",
			req:    iam.Request{Principal: named("bob", docs, "site-rw"), Action: iam.ActionDeleteObject, Resource: object, BucketPolicy: keep},
			source: iam.SourceBucket, sentinel: domain.ErrAccessDenied,
			cited: []iam.CitedStatement{cite(iam.SourceBucket, "", 0, 0, "keep", iam.Deny)},
		},
		{
			name:    "sub-bucket",
			req:     iam.Request{Principal: named("alice", docs), Action: iam.ActionGetObject, Resource: object, SubBucketPolicy: sub},
			allowed: true, source: iam.SourceSubBucket,
			cited: []iam.CitedStatement{cite(iam.SourceSubBucket, "", 0, 0, "", iam.Allow)},
		},
		{
			name:    "sub-bucket listing",
			req:     iam.Request{Principal: named("alice", docs), Action: iam.ActionListBucket, Resource: iam.Resource{Bucket: "site"}, ListPrefix: "reports/", SubBucketPolicy: sub},
			allowed: true, source: iam.SourceSubBucket,
			cited: []iam.CitedStatement{cite(iam.SourceSubBucket, "", 0, 0, "", iam.Allow)},
		},
		{
			name:   "sub-bucket not covering the request",
			req:    iam.Request{Principal: named("alice", docs), Action: iam.ActionGetObject, Resource: iam.Resource{Bucket: "site", Key: "other/a.csv"}, SubBucketPolicy: sub},
			source: iam.SourceNone, sentinel: domain.ErrAccessDenied,
		},
		{
			name:    "every source",
			req:     iam.Request{Principal: named("bob", docs, "site-rw"), Action: iam.ActionGetObject, Resource: object, BucketPolicy: bob, SubBucketPolicy: mustSubBucketPolicy(t, `{"Version":"2012-10-17","Statement":[{"Sid":"sub","Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"}]}`, "site", "reports")},
			allowed: true, source: iam.SourceIdentity,
			cited: []iam.CitedStatement{
				cite(iam.SourceIdentity, "site-rw", 0, 0, "read", iam.Allow),
				cite(iam.SourceBucket, "", 0, 0, "bob", iam.Allow),
				cite(iam.SourceSubBucket, "", 0, 0, "sub", iam.Allow),
			},
		},
		{
			name:    "key bound, allowed",
			req:     iam.Request{Principal: bound, Action: iam.ActionGetObject, Resource: object},
			allowed: true, source: iam.SourceIdentity,
			cited: []iam.CitedStatement{cite(iam.SourceIdentity, "site-rw", 0, 0, "read", iam.Allow)},
			key:   &iam.KeyPolicyVerdict{Allowed: true, Statements: []iam.CitedStatement{cite(iam.SourceKeyBound, "", 0, 0, "only-read", iam.Allow)}},
		},
		{
			name:   "key bound, refused by the key",
			req:    iam.Request{Principal: bound, Action: iam.ActionPutObject, Resource: object},
			source: iam.SourceKeyBound, sentinel: domain.ErrAccessDenied,
			cited: []iam.CitedStatement{cite(iam.SourceIdentity, "site-rw", 0, 1, "write", iam.Allow)},
			key:   &iam.KeyPolicyVerdict{},
		},
		{
			name:   "key bound, refused by the owner's policies, the key's told all the same",
			req:    iam.Request{Principal: bound, Action: iam.ActionGetObject, Resource: iam.Resource{Bucket: "other", Key: "a"}},
			source: iam.SourceNone, sentinel: domain.ErrAccessDenied,
			key: &iam.KeyPolicyVerdict{Allowed: true, Statements: []iam.CitedStatement{cite(iam.SourceKeyBound, "", 0, 0, "only-read", iam.Allow)}},
		},
		{
			name:    "owner preview",
			req:     iam.Request{Principal: named("alice", docs), Action: iam.ActionPreviewObject, Resource: object, ObjectCreator: "user/alice"},
			allowed: true, source: iam.SourceOwner,
		},
		{
			name:   "owner preview denied",
			req:    iam.Request{Principal: named("alice", docs, "no-preview"), Action: iam.ActionPreviewObject, Resource: object, ObjectCreator: "user/alice"},
			source: iam.SourceIdentity, sentinel: domain.ErrAccessDenied,
			cited: []iam.CitedStatement{cite(iam.SourceIdentity, "no-preview", 0, 0, "no-preview", iam.Deny)},
		},
		{
			name:   "an Allow on an unknown address",
			req:    iam.Request{Principal: away, Action: iam.ActionGetObject, Resource: object},
			source: iam.SourceNone, sentinel: domain.ErrAccessDenied,
			unknown: []iam.CitedStatement{cite(iam.SourceIdentity, "office", 0, 0, "office", iam.Allow)},
		},
		{
			name:   "a Deny before an Allow in one identity document",
			req:    iam.Request{Principal: named("alice", docs, "deny-first"), Action: iam.ActionGetObject, Resource: object},
			source: iam.SourceIdentity, sentinel: domain.ErrAccessDenied,
			cited: []iam.CitedStatement{
				cite(iam.SourceIdentity, "deny-first", 0, 0, "deny", iam.Deny),
				cite(iam.SourceIdentity, "deny-first", 0, 1, "allow", iam.Allow),
			},
		},
		{
			name:   "a Deny before an Allow across two identity documents",
			req:    iam.Request{Principal: named("alice", docs, "deny-get", "site-rw"), Action: iam.ActionGetObject, Resource: object},
			source: iam.SourceIdentity, sentinel: domain.ErrAccessDenied,
			cited: []iam.CitedStatement{
				cite(iam.SourceIdentity, "deny-get", 0, 0, "deny-get", iam.Deny),
				cite(iam.SourceIdentity, "site-rw", 1, 0, "read", iam.Allow),
			},
		},
		{
			name:   "a Deny before an Allow in a bucket policy",
			req:    iam.Request{Principal: named("bob", docs), Action: iam.ActionGetObject, Resource: object, BucketPolicy: mustBucketPolicy(t, bucketDenyThenAllow, "site")},
			source: iam.SourceBucket, sentinel: domain.ErrAccessDenied,
			cited: []iam.CitedStatement{
				cite(iam.SourceBucket, "", 0, 0, "deny", iam.Deny),
				cite(iam.SourceBucket, "", 0, 1, "allow", iam.Allow),
			},
		},
		{
			name:   "a Deny before an Allow in a sub-bucket policy",
			req:    iam.Request{Principal: named("alice", docs), Action: iam.ActionGetObject, Resource: object, SubBucketPolicy: mustSubBucketPolicy(t, subDenyThenAllow, "site", "reports")},
			source: iam.SourceSubBucket, sentinel: domain.ErrAccessDenied,
			cited: []iam.CitedStatement{
				cite(iam.SourceSubBucket, "", 0, 0, "deny", iam.Deny),
				cite(iam.SourceSubBucket, "", 0, 1, "allow", iam.Allow),
			},
		},
		{
			name:   "anonymous refused by a bucket's Deny before its Allow",
			req:    iam.Request{Action: iam.ActionGetObject, Resource: object, BucketPolicy: mustBucketPolicy(t, anyoneDenyThenAllow, "site")},
			source: iam.SourceBucket, sentinel: domain.ErrAccessDenied,
			cited: []iam.CitedStatement{
				cite(iam.SourceBucket, "", 0, 0, "deny", iam.Deny),
				cite(iam.SourceBucket, "", 0, 1, "allow", iam.Allow),
			},
		},
		{
			name:   "a Deny on an unknown address",
			req:    iam.Request{Principal: outside, Action: iam.ActionGetObject, Resource: object},
			source: iam.SourceIdentity, sentinel: domain.ErrAccessDenied,
			cited: []iam.CitedStatement{
				cite(iam.SourceIdentity, "not-outside", 0, 0, "everything", iam.Allow),
				cite(iam.SourceIdentity, "not-outside", 0, 1, "outside", iam.Deny),
			},
			unknown: []iam.CitedStatement{cite(iam.SourceIdentity, "not-outside", 0, 1, "outside", iam.Deny)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.req.Now = policyNow
			got := iam.Explain(tt.req)
			if got.Allowed != tt.allowed || got.Source != tt.source || got.Rule != tt.rule {
				t.Fatalf("Explain = allowed %v, source %q, rule %q; want %v, %q, %q (%v)", got.Allowed, got.Source, got.Rule, tt.allowed, tt.source, tt.rule, got.Err())
			}
			if !reflect.DeepEqual(got.Statements, tt.cited) {
				t.Errorf("statements = %+v, want %+v", got.Statements, tt.cited)
			}
			if !reflect.DeepEqual(got.KeyPolicy, tt.key) {
				t.Errorf("key policy = %+v, want %+v", got.KeyPolicy, tt.key)
			}
			if !reflect.DeepEqual(got.UnknownAddress, tt.unknown) {
				t.Errorf("unknown address = %+v, want %+v", got.UnknownAddress, tt.unknown)
			}
			source, err := iam.Evaluate(tt.req)
			if source != got.Source || !sameError(err, got.Err()) || !sameError(iam.Decide(tt.req), got.Err()) {
				t.Errorf("Evaluate = %q, %v; Decide = %v; Explain = %q, %v", source, err, iam.Decide(tt.req), got.Source, got.Err())
			}
			if tt.sentinel == nil {
				if got.Err() != nil {
					t.Fatalf("refused: %v", got.Err())
				}
				return
			}
			if !errors.Is(got.Err(), tt.sentinel) {
				t.Fatalf("Err = %v, want %v", got.Err(), tt.sentinel)
			}
			if errors.Is(tt.sentinel, domain.ErrSessionLocked) && errors.Is(got.Err(), domain.ErrAccessDenied) {
				t.Fatalf("a locked session refused as denied: %v", got.Err())
			}
		})
	}
}

// sameError reports whether two errors are both nil, or say the same.
func sameError(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Error() == b.Error()
}

// TestExplainSessionIsAssumed checks that an explanation is made with the
// address and transport of the request, as a decision is.
func TestExplainSessionIsAssumed(t *testing.T) {
	t.Parallel()
	office := mustPolicy(t, fromTheOffice)
	tests := []struct {
		name    string
		addr    string
		allowed bool
	}{
		{"inside", "10.9.8.7", true},
		{"outside", "192.0.2.1", false},
		{"mapped", "::ffff:10.0.0.1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := user("dan", office)
			p.Session.SourceIP = netip.MustParseAddr(tt.addr)
			req := iam.Request{Principal: p, Action: iam.ActionGetObject, Resource: iam.Resource{Bucket: "site", Key: "a"}, Now: policyNow}
			got := iam.Explain(req)
			if got.Allowed != tt.allowed || got.Allowed != (iam.Decide(req) == nil) || len(got.UnknownAddress) != 0 {
				t.Fatalf("Explain = %+v", got)
			}
		})
	}
}
