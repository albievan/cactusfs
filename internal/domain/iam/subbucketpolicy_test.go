package iam_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// subStatement is one statement of a policy of the sub-bucket "reports" of
// the bucket "site".
func subStatement(effect, principal, actions, resources string) string {
	return `{"Effect":"` + effect + `","Principal":` + principal + `,"Action":` + actions + `,"Resource":` + resources + `}`
}

func subDocument(statements ...string) string {
	return `{"Version":"2012-10-17","Statement":[` + strings.Join(statements, ",") + `]}`
}

const (
	aliceARN    = `{"AWS":"arn:aws:iam:::user/alice"}`
	reportKeys  = `"arn:aws:s3:::site/reports/*"`
	siteARN     = `"arn:aws:s3:::site"`
	readReports = `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/alice"},"Action":["s3:GetObject","s3:ListBucket"],
		 "Resource":["arn:aws:s3:::site","arn:aws:s3:::site/reports/*"]}]}`
)

func mustSubBucketPolicy(t testing.TB, document, bucketName, sub string) *iam.SubBucketPolicy {
	t.Helper()
	policy, err := iam.ParseSubBucketPolicy([]byte(document), bucketName, sub)
	if err != nil {
		t.Fatalf("ParseSubBucketPolicy: %v", err)
	}
	return &policy
}

// subBucketActions are the actions a sub-bucket policy may name (ADR-0017):
// those on objects, and the listings.
func subBucketActions() (objects, listings []iam.Action) {
	return []iam.Action{
			iam.ActionGetObject, iam.ActionPutObject, iam.ActionDeleteObject, iam.ActionGetObjectVersion, iam.ActionDeleteObjectVersion,
			iam.ActionGetObjectRetention, iam.ActionPutObjectRetention, iam.ActionGetObjectLegalHold, iam.ActionPutObjectLegalHold,
			iam.ActionGetObjectTagging, iam.ActionPutObjectTagging, iam.ActionDeleteObjectTagging,
			iam.ActionAbortMultipartUpload, iam.ActionListMultipartUploadParts,
		},
		[]iam.Action{iam.ActionListBucket, iam.ActionListBucketVersions, iam.ActionListBucketMultipartUploads}
}

func TestParseSubBucketPolicy(t *testing.T) {
	t.Parallel()
	get := `"s3:GetObject"`
	good := map[string]string{
		"read the keys and list them": readReports,
		"a narrower range of keys":    subDocument(subStatement("Allow", aliceARN, get, `"arn:aws:s3:::site/reports/2026/*"`)),
		"one key":                     subDocument(subStatement("Allow", aliceARN, get, `"arn:aws:s3:::site/reports/a.csv"`)),
		"several users":               subDocument(subStatement("Allow", `{"AWS":["arn:aws:iam:::user/alice","arn:aws:iam:::user/bob"]}`, get, reportKeys)),
		"action in another case":      subDocument(subStatement("Allow", aliceARN, `"S3:getobject"`, reportKeys)),
		"every object action": subDocument(subStatement("Allow", aliceARN, `["s3:GetObject","s3:PutObject","s3:DeleteObject",
			"s3:GetObjectVersion","s3:DeleteObjectVersion","s3:GetObjectRetention","s3:PutObjectRetention","s3:GetObjectLegalHold",
			"s3:PutObjectLegalHold","s3:GetObjectTagging","s3:PutObjectTagging","s3:DeleteObjectTagging","s3:AbortMultipartUpload",
			"s3:ListMultipartUploadParts"]`, reportKeys)),
		"every listing": subDocument(subStatement("Allow", aliceARN, `["s3:ListBucket","s3:ListBucketVersions","s3:ListBucketMultipartUploads"]`, siteARN)),
		"a deny for anyone of a write": subDocument(subStatement("Allow", aliceARN, get, reportKeys),
			subStatement("Deny", `"*"`, `["s3:PutObject","s3:DeleteObject"]`, reportKeys)),
		"a condition": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":` + aliceARN +
			`,"Action":"s3:GetObject","Resource":` + reportKeys + `,"Condition":{"Bool":{"aws:SecureTransport":"true"}}}]}`,
	}
	for name, document := range good {
		policy, err := iam.ParseSubBucketPolicy([]byte(document), "site", "reports")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if policy.Bucket() != "site" || policy.Name() != "reports" || len(policy.Policy().Document.Statements) == 0 {
			t.Errorf("%s: parsed as %+v", name, policy)
		}
	}

	bad := map[string]string{
		// What a bucket policy refuses, a sub-bucket's refuses.
		"not JSON":         `Principal: *`,
		"no principal":     `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":` + reportKeys + `}]}`,
		"another bucket":   subDocument(subStatement("Allow", aliceARN, get, `"arn:aws:s3:::other/reports/*"`)),
		"too large":        subDocument(subStatement("Allow", aliceARN, `["s3:GetObject"`+strings.Repeat(`,"s3:GetObject"`, iam.MaxPolicyBytes/15)+`]`, reportKeys)),
		"no statements":    `{"Version":"2012-10-17","Statement":[]}`,
		"lifts retentions": subDocument(subStatement("Allow", aliceARN, `"s3:BypassGovernanceRetention"`, reportKeys)),
		// Anonymous callers: never allowed.
		"anyone":                 subDocument(subStatement("Allow", `"*"`, get, reportKeys)),
		"anyone, as AWS":         subDocument(subStatement("Allow", `{"AWS":"*"}`, get, reportKeys)),
		"anyone among users":     subDocument(subStatement("Allow", `{"AWS":["arn:aws:iam:::user/alice","*"]}`, get, reportKeys)),
		"anyone may list":        subDocument(subStatement("Allow", `"*"`, `"s3:ListBucket"`, siteARN)),
		"named even in a deny":   subDocument(subStatement("Deny", aliceARN, `"s3:BypassGovernanceRetention"`, reportKeys)),
		"a replica":              subDocument(subStatement("Allow", aliceARN, `"s3:ReplicateObject"`, reportKeys)),
		"a replica, denied":      subDocument(subStatement("Deny", aliceARN, `"s3:ReplicateDelete"`, reportKeys)),
		"a wildcard of s3":       subDocument(subStatement("Allow", aliceARN, `"s3:*"`, reportKeys)),
		"a wildcard of reads":    subDocument(subStatement("Allow", aliceARN, `"s3:Get*"`, reportKeys)),
		"a wildcard of one":      subDocument(subStatement("Allow", aliceARN, `"s3:?etObject"`, reportKeys)),
		"a deny of a wildcard":   subDocument(subStatement("Deny", aliceARN, `"s3:*"`, reportKeys)),
		"the bucket's policy":    subDocument(subStatement("Allow", aliceARN, `"s3:PutBucketPolicy"`, siteARN)),
		"a sub-bucket":           subDocument(subStatement("Allow", aliceARN, `"s3:PutSubBucket"`, siteARN)),
		"its own policy":         subDocument(subStatement("Allow", aliceARN, `"s3:GetSubBucketPolicy"`, siteARN)),
		"deleting the bucket":    subDocument(subStatement("Allow", aliceARN, `"s3:DeleteBucket"`, siteARN)),
		"the bucket's location":  subDocument(subStatement("Allow", aliceARN, `"s3:GetBucketLocation"`, siteARN)),
		"versioning":             subDocument(subStatement("Allow", aliceARN, `"s3:PutBucketVersioning"`, siteARN)),
		"administration":         subDocument(subStatement("Allow", aliceARN, `"admin:PreviewObject"`, reportKeys)),
		"denying a listing":      subDocument(subStatement("Deny", aliceARN, `"s3:ListBucket"`, siteARN)),
		"denying versions":       subDocument(subStatement("Deny", `"*"`, `"s3:ListBucketVersions"`, siteARN)),
		"denying uploads":        subDocument(subStatement("Deny", aliceARN, `["s3:GetObject","s3:ListBucketMultipartUploads"]`, reportKeys)),
		"every key of the":       subDocument(subStatement("Allow", aliceARN, get, `"arn:aws:s3:::site/*"`)),
		"another sub-bucket":     subDocument(subStatement("Allow", aliceARN, get, `"arn:aws:s3:::site/drafts/*"`)),
		"a longer name":          subDocument(subStatement("Allow", aliceARN, get, `"arn:aws:s3:::site/reports-x/*"`)),
		"the name without slash": subDocument(subStatement("Allow", aliceARN, get, `"arn:aws:s3:::site/reports*"`)),
		"the name as a pattern":  subDocument(subStatement("Allow", aliceARN, get, `"arn:aws:s3:::site/report?/*"`)),
		"the name alone":         subDocument(subStatement("Allow", aliceARN, get, `"arn:aws:s3:::site/reports"`)),
		"every bucket":           subDocument(subStatement("Allow", aliceARN, get, `"*"`)),
		"buckets by pattern":     subDocument(subStatement("Allow", aliceARN, get, `"arn:aws:s3:::site*"`)),
		"one resource of two": subDocument(subStatement("Allow", aliceARN, get,
			`["arn:aws:s3:::site/reports/*","arn:aws:s3:::site/drafts/*"]`)),
		"one statement of two": subDocument(subStatement("Allow", aliceARN, get, reportKeys),
			subStatement("Allow", aliceARN, get, `"arn:aws:s3:::site/*"`)),
	}
	for name, document := range bad {
		_, err := iam.ParseSubBucketPolicy([]byte(document), "site", "reports")
		if !errors.Is(err, domain.ErrInvalidPolicy) || !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: %v, want %v", name, err, domain.ErrInvalidPolicy)
		}
	}

	// The error reads as what is wrong with the document, for the detail
	// of the answer: the reason, not the sentinels it wraps.
	for document, says := range map[string]string{
		bad["anyone"]:         `statement 0: an Allow names the users it allows, never "*"`,
		bad["another bucket"]: `resource "arn:aws:s3:::other/reports/*" is not this bucket or in it`,
	} {
		_, err := iam.ParseSubBucketPolicy([]byte(document), "site", "reports")
		if err == nil || !strings.Contains(err.Error(), says) || strings.Count(err.Error(), "invalid argument") != 1 {
			t.Errorf("the error %q does not say %q once", err, says)
		}
	}

	// A name is no fault of the document.
	for name, names := range map[string][2]string{
		"no sub-bucket":         {"site", ""},
		"a sub-bucket in caps":  {"site", "Reports"},
		"a nested sub-bucket":   {"site", "reports/2026"},
		"a sub-bucket pattern":  {"site", "rep*"},
		"a sub-bucket too long": {"site", strings.Repeat("r", 64)},
	} {
		_, err := iam.ParseSubBucketPolicy([]byte(readReports), names[0], names[1])
		if !errors.Is(err, domain.ErrInvalidArgument) || errors.Is(err, domain.ErrInvalidPolicy) {
			t.Errorf("%s: %v, want %v and not %v", name, err, domain.ErrInvalidArgument, domain.ErrInvalidPolicy)
		}
	}
	for _, name := range []string{"", "s*", "Site", "si/te", strings.Repeat("s", 64)} {
		_, err := iam.ParseSubBucketPolicy([]byte(readReports), name, "reports")
		if !errors.Is(err, domain.ErrInvalidBucketName) || errors.Is(err, domain.ErrInvalidPolicy) {
			t.Errorf("bucket %q: %v, want %v", name, err, domain.ErrInvalidBucketName)
		}
	}

	// Every action there is: one a sub-bucket policy may name is allowed by
	// it, and every other one refused, in an Allow and in a Deny alike -
	// but a listing, which only an Allow may name.
	objects, listings := subBucketActions()
	for _, action := range declaredActions(t) {
		for _, effect := range []string{"Allow", "Deny"} {
			document := subDocument(subStatement(effect, aliceARN, `"`+string(action)+`"`, `["arn:aws:s3:::site","arn:aws:s3:::site/reports/*"]`))
			_, err := iam.ParseSubBucketPolicy([]byte(document), "site", "reports")
			want := slices.Contains(objects, action) || (effect == "Allow" && slices.Contains(listings, action))
			if (err == nil) != want {
				t.Errorf("%s of %s: %v, accepted wanted: %v", effect, action, err, want)
			}
		}
	}
}

// TestSubBucketNamesAreTheBucketPackages: the names ParseSubBucketPolicy
// takes for a sub-bucket are those the bucket package does, and it takes
// every bucket name the bucket package does.
func TestSubBucketNamesAreTheBucketPackages(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"reports", "r", "a-b", "2026", strings.Repeat("r", 63), strings.Repeat("r", 64), "", "-a", "a-", "A", "a_b", "a.b", "a/b", "a b", "é",
	} {
		_, err := iam.ParseSubBucketPolicy([]byte(subDocument(subStatement("Allow", aliceARN, `"s3:GetObject"`, `"arn:aws:s3:::site/*"`))), "site", name)
		valid := bucket.ValidateSubBucketName(name) == nil
		if named := !errors.Is(err, domain.ErrInvalidArgument) || errors.Is(err, domain.ErrInvalidPolicy); named != valid {
			t.Errorf("sub-bucket %q: %v; the bucket package finds it valid: %v", name, err, valid)
		}
	}
	for _, name := range []string{"site", "my.site", "a-b", "123", strings.Repeat("b", 63)} {
		if bucket.ValidateName(name) != nil {
			t.Fatalf("%q is not a bucket name", name)
		}
		if _, err := iam.ParseSubBucketPolicy([]byte(subDocument(subStatement("Allow", aliceARN, `"s3:GetObject"`,
			`"arn:aws:s3:::`+name+`/reports/*"`))), name, "reports"); err != nil {
			t.Errorf("bucket %q: %v", name, err)
		}
	}
}

// FuzzSubBucketNames: whatever the name, ParseSubBucketPolicy and the
// bucket package agree whether a sub-bucket can be called that.
func FuzzSubBucketNames(f *testing.F) {
	for _, name := range []string{"reports", "a-", "-", "Reports", "a/b", "", strings.Repeat("x", 63)} {
		f.Add(name)
	}
	f.Fuzz(func(t *testing.T, name string) {
		_, err := iam.ParseSubBucketPolicy([]byte(readReports), "site", name)
		refusedForTheName := errors.Is(err, domain.ErrInvalidArgument) && !errors.Is(err, domain.ErrInvalidPolicy)
		if valid := bucket.ValidateSubBucketName(name) == nil; refusedForTheName == valid {
			t.Fatalf("sub-bucket %q: %v; the bucket package finds it valid: %v", name, err, valid)
		}
	})
}

// FuzzSubBucketPolicy: every document ParseSubBucketPolicy keeps is
// confined - it allows nobody anonymous, never names
// s3:BypassGovernanceRetention, never denies a listing and names no
// resource but the sub-bucket's keys and the bucket - and so is every
// decision made with it: no anonymous caller is let in by it, and it turns
// no Deny of the bucket's policy into an Allow.
func FuzzSubBucketPolicy(f *testing.F) {
	f.Add([]byte(readReports), "reports", "s3:GetObject", "reports/a", "")
	f.Add([]byte(readReports), "reports", "s3:ListBucket", "", "reports/")
	f.Add([]byte(readReports), "reports", "s3:ListBucket", "", "reports")
	f.Add([]byte(subDocument(subStatement("Allow", `"*"`, `"s3:GetObject"`, reportKeys))), "reports", "s3:GetObject", "reports/a", "")
	f.Add([]byte(subDocument(subStatement("Deny", `"*"`, `"s3:ListBucket"`, siteARN))), "reports", "s3:ListBucket", "", "reports/x")
	f.Add([]byte(subDocument(subStatement("Allow", aliceARN, `"s3:BypassGovernanceRetention"`, reportKeys))), "reports", "s3:BypassGovernanceRetention", "reports/a", "")
	f.Add([]byte(subDocument(subStatement("Allow", aliceARN, `"S3:PutObject"`, `"arn:aws:s3:::site/reports/*"`))), "reports", "s3:PutObject", "reports/x/y", "")
	f.Fuzz(func(t *testing.T, document []byte, sub, action, key, prefix string) {
		policy, err := iam.ParseSubBucketPolicy(document, "site", sub)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("untyped error: %v", err)
			}
			return
		}
		if bucket.ValidateSubBucketName(sub) != nil || policy.Bucket() != "site" || policy.Name() != sub {
			t.Fatalf("kept as the policy of %q/%q, asked for %q", policy.Bucket(), policy.Name(), sub)
		}
		parsed := policy.Policy()
		if len(parsed.Principals) != len(parsed.Document.Statements) {
			t.Fatalf("%d statements, %d principals", len(parsed.Document.Statements), len(parsed.Principals))
		}
		objects, listings := subBucketActions()
		for i, s := range parsed.Document.Statements {
			if s.Effect == iam.Allow && slices.Contains(parsed.Principals[i], iam.PrincipalAnyone) {
				t.Fatalf("statement %d allows anyone", i)
			}
			for _, a := range s.Actions {
				if strings.EqualFold(a, string(iam.ActionOverrideGovernance)) {
					t.Fatalf("statement %d names %s", i, a)
				}
				listing := slices.ContainsFunc(listings, func(l iam.Action) bool { return strings.EqualFold(a, string(l)) })
				object := slices.ContainsFunc(objects, func(o iam.Action) bool { return strings.EqualFold(a, string(o)) })
				if !listing && !object {
					t.Fatalf("statement %d names %q", i, a)
				}
				if listing && s.Effect == iam.Deny {
					t.Fatalf("statement %d denies %s", i, a)
				}
			}
			for _, resource := range s.Resources {
				if resource != "arn:aws:s3:::site" && !strings.HasPrefix(resource, "arn:aws:s3:::site/"+sub+"/") {
					t.Fatalf("statement %d is about %q", i, resource)
				}
			}
		}
		decideWithSubBucketPolicy(t, &policy, iam.Action(action), iam.Resource{Bucket: "site", Key: key}, prefix)
	})
}

// decideWithSubBucketPolicy holds the properties of a decision made with a
// sub-bucket policy, for one request, against every kind of caller and
// bucket policy.
func decideWithSubBucketPolicy(t *testing.T, policy *iam.SubBucketPolicy, action iam.Action, resource iam.Resource, prefix string) {
	t.Helper()
	public := mustBucketPolicy(t, publicRead, "site")
	everything := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"*"}, Resources: []string{"*"}}}}
	nothing := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"admin:GetMetrics"}, Resources: []string{"*"}}}}
	denyAll := mustBucketPolicy(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":["arn:aws:s3:::site","arn:aws:s3:::site/*"]}]}`, "site")
	for _, bucketPolicy := range []*iam.BucketPolicy{nil, public, denyAll} {
		// An anonymous caller is never let in by a sub-bucket policy; its
		// Deny may only refuse them what the bucket's policy allows.
		anonymous := iam.Request{Action: action, Resource: resource, BucketPolicy: bucketPolicy, ListPrefix: prefix, Now: policyNow}
		without := iam.Decide(anonymous)
		anonymous.SubBucketPolicy = policy
		if with := iam.Decide(anonymous); with == nil && without != nil {
			t.Fatalf("anonymous %s on %s (prefix %q): let in by the sub-bucket policy (%v without it)", action, resource, prefix, without)
		}
		for _, p := range []iam.Principal{
			{AccessKeyID: "A", User: "alice"},
			{AccessKeyID: "A", User: "alice", Policies: []iam.Document{everything}},
			{AccessKeyID: "K", User: "alice", Boundary: &nothing},
			{AccessKeyID: "B", User: "bob"},
		} {
			req := iam.Request{Principal: p, Action: action, Resource: resource, BucketPolicy: bucketPolicy, ListPrefix: prefix, SubBucketPolicy: policy, Now: policyNow}
			source, err := iam.Evaluate(req)
			if (iam.Decide(req) == nil) != (err == nil) {
				t.Fatal("Decide and Evaluate disagree")
			}
			// A Deny of the bucket's policy stays a Deny.
			if bucketPolicy == denyAll && err == nil {
				t.Fatalf("%s %s on %s (prefix %q): the bucket denies it, and %s allowed it", p.Name(), action, resource, prefix, source)
			}
			// The bound of an API key holds.
			if p.Boundary != nil && err == nil && action != iam.ActionGetMetrics {
				t.Fatalf("%s %s on %s: allowed past the key's bound by %s", p.Name(), action, resource, source)
			}
			// What the policy allows that nothing else does is no more
			// than what it may: an object action on a key of the
			// sub-bucket, or a listing of a prefix in it.
			req.SubBucketPolicy = nil
			if err == nil && source == iam.SourceSubBucket {
				if iam.Decide(req) == nil {
					t.Fatalf("the sub-bucket policy was named the source of what is allowed without it")
				}
				objects, listings := subBucketActions()
				inKeys := resource.Key != "" && strings.HasPrefix(resource.Key, policy.Name()+"/") && slices.Contains(objects, action)
				inListing := resource.Key == "" && strings.HasPrefix(prefix, policy.Name()+"/") && slices.Contains(listings, action)
				if !inKeys && !inListing {
					t.Fatalf("%s %s on %s (prefix %q) was allowed by the policy of %s", p.Name(), action, resource, prefix, policy.Name())
				}
			}
		}
	}
	// Root is bound by none of the policies.
	root := iam.Request{Principal: iam.Principal{AccessKeyID: "ROOT", Root: true}, Action: action, Resource: resource, BucketPolicy: denyAll, SubBucketPolicy: policy, ListPrefix: prefix}
	plain := root
	plain.BucketPolicy, plain.SubBucketPolicy = nil, nil
	if (iam.Decide(root) == nil) != (iam.Decide(plain) == nil) {
		t.Fatalf("root %s on %s is decided by the policies", action, resource)
	}
}

// TestDecideFromThreeSources: a request on a key of a sub-bucket, or a
// listing in one, is judged by the caller's own policies, the bucket's and
// the sub-bucket's: a Deny in any refuses, an Allow in any allows, the bound
// of an API key holds after, root is bound by none, an anonymous caller is
// never let in by a sub-bucket, and a listing is the sub-bucket's only when
// its prefix begins with the sub-bucket's name and a slash.
func TestDecideFromThreeSources(t *testing.T) {
	t.Parallel()
	reports := mustSubBucketPolicy(t, readReports, "site", "reports")
	writers := mustSubBucketPolicy(t, subDocument(
		subStatement("Allow", `{"AWS":["arn:aws:iam:::user/alice","arn:aws:iam:::user/bob"]}`, `["s3:PutObject","s3:GetObject"]`, reportKeys),
		subStatement("Deny", `{"AWS":"arn:aws:iam:::user/bob"}`, `"s3:DeleteObject"`, reportKeys),
	), "site", "reports")
	denyBob := mustSubBucketPolicy(t, subDocument(subStatement("Deny", `{"AWS":"arn:aws:iam:::user/bob"}`, `"s3:GetObject"`, reportKeys)), "site", "reports")
	privateReports := mustSubBucketPolicy(t, subDocument(subStatement("Deny", `"*"`, `"s3:GetObject"`, reportKeys)), "site", "reports")
	ofOtherBucket := mustSubBucketPolicy(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/alice"},"Action":["s3:GetObject","s3:ListBucket"],
		 "Resource":["arn:aws:s3:::other","arn:aws:s3:::other/reports/*"]}]}`, "other", "reports")
	ofDrafts := mustSubBucketPolicy(t, subDocument(subStatement("Allow", aliceARN, `"s3:GetObject"`, `"arn:aws:s3:::site/drafts/*"`)), "site", "drafts")
	var zero iam.SubBucketPolicy

	denyDrafts := mustBucketPolicy(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::site/reports/secret/*"}]}`, "site")
	public := mustBucketPolicy(t, publicRead, "site")
	bucketForBob := mustBucketPolicy(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`, "site")

	everything := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"s3:*"}, Resources: []string{"*"}}}}
	noReports := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Deny, Actions: []string{"s3:GetObject"}, Resources: []string{"arn:aws:s3:::site/reports/*"}}}}
	putOnly := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"s3:PutObject"}, Resources: []string{"*"}}}}
	alice := iam.Principal{AccessKeyID: "A", User: "alice"}
	aliceDenied := iam.Principal{AccessKeyID: "A", User: "alice", Policies: []iam.Document{noReports}}
	aliceKey := iam.Principal{AccessKeyID: "K", User: "alice", Boundary: &putOnly}
	bob := iam.Principal{AccessKeyID: "B", User: "bob"}
	bobAdmin := iam.Principal{AccessKeyID: "B", User: "bob", Policies: []iam.Document{everything}}
	carol := iam.Principal{AccessKeyID: "C", User: "carol"}
	root := iam.Principal{AccessKeyID: "ROOT", Root: true}
	nobody := iam.Principal{}

	key := iam.Resource{Bucket: "site", Key: "reports/q1.csv"}
	secret := iam.Resource{Bucket: "site", Key: "reports/secret/plan.txt"}
	top := iam.Resource{Bucket: "site", Key: "index.html"}
	lookalike := iam.Resource{Bucket: "site", Key: "reports-x/q1.csv"}
	bare := iam.Resource{Bucket: "site", Key: "reports"}
	site := iam.Resource{Bucket: "site"}

	tests := []struct {
		name      string
		principal iam.Principal
		sub       *iam.SubBucketPolicy
		bucket    *iam.BucketPolicy
		action    iam.Action
		resource  iam.Resource
		prefix    string
		source    iam.Source
		allowed   bool
	}{
		// An Allow in any source allows.
		{"the sub-bucket allows", alice, reports, nil, iam.ActionGetObject, key, "", iam.SourceSubBucket, true},
		{"its own policy allows", bobAdmin, reports, nil, iam.ActionGetObject, key, "", iam.SourceIdentity, true},
		{"the bucket allows", bob, reports, bucketForBob, iam.ActionGetObject, key, "", iam.SourceBucket, true},
		{
			"own policy named before the sub-bucket",
			iam.Principal{AccessKeyID: "A", User: "alice", Policies: []iam.Document{everything}},
			reports, nil, iam.ActionGetObject, key, "", iam.SourceIdentity, true,
		},
		{"silence everywhere", carol, reports, nil, iam.ActionGetObject, key, "", iam.SourceNone, false},
		{"what the sub-bucket does not allow", alice, reports, nil, iam.ActionPutObject, key, "", iam.SourceNone, false},
		{"a nested key", alice, reports, nil, iam.ActionGetObject, iam.Resource{Bucket: "site", Key: "reports/2026/q1.csv"}, "", iam.SourceSubBucket, true},
		// A Deny in any source refuses.
		{"the bucket denies", alice, reports, denyDrafts, iam.ActionGetObject, secret, "", iam.SourceBucket, false},
		{"its own policy denies", aliceDenied, reports, nil, iam.ActionGetObject, key, "", iam.SourceIdentity, false},
		{"the sub-bucket denies", bobAdmin, denyBob, nil, iam.ActionGetObject, key, "", iam.SourceSubBucket, false},
		{"the sub-bucket denies what the bucket allows", bob, denyBob, bucketForBob, iam.ActionGetObject, key, "", iam.SourceSubBucket, false},
		{"its deny is about whom it names", alice, writers, nil, iam.ActionPutObject, key, "", iam.SourceSubBucket, true},
		{"and not about others", bob, writers, nil, iam.ActionDeleteObject, key, "", iam.SourceSubBucket, false},
		// The bound of an API key holds.
		{"the key's bound refuses", aliceKey, reports, nil, iam.ActionGetObject, key, "", iam.SourceKeyBound, false},
		{"the key's bound allows", aliceKey, writers, nil, iam.ActionPutObject, key, "", iam.SourceSubBucket, true},
		// Root is bound by none.
		{"root, a sub-bucket denying", root, denyBob, denyDrafts, iam.ActionGetObject, secret, "", iam.SourceRule, true},
		// Anonymous callers: the sub-bucket's policy never lets them in...
		{"anonymous, the sub-bucket", nobody, reports, nil, iam.ActionGetObject, key, "", iam.SourceRule, false},
		{"anonymous lists", nobody, reports, nil, iam.ActionListBucket, site, "reports/", iam.SourceRule, false},
		{"anonymous, a public bucket", nobody, denyBob, public, iam.ActionGetObject, key, "", iam.SourceBucket, true},
		// ...but its Deny holds for them as for anybody: a private
		// sub-bucket of a public bucket stays private.
		{"anonymous, a private sub-bucket of a public bucket", nobody, privateReports, public, iam.ActionGetObject, key, "", iam.SourceSubBucket, false},
		{"anonymous, the public rest of that bucket", nobody, privateReports, public, iam.ActionGetObject, top, "", iam.SourceBucket, true},
		{"signed in, the same private sub-bucket", alice, privateReports, public, iam.ActionGetObject, key, "", iam.SourceSubBucket, false},
		// What the sub-bucket covers: keys that begin "reports/".
		{"a key at the top", alice, reports, nil, iam.ActionGetObject, top, "", iam.SourceNone, false},
		{"a key of a look-alike", alice, reports, nil, iam.ActionGetObject, lookalike, "", iam.SourceNone, false},
		{"a key that is the name", alice, reports, nil, iam.ActionGetObject, bare, "", iam.SourceNone, false},
		{"its deny on a look-alike", bobAdmin, denyBob, nil, iam.ActionGetObject, lookalike, "", iam.SourceIdentity, true},
		{"a policy of another bucket", alice, ofOtherBucket, nil, iam.ActionGetObject, key, "", iam.SourceNone, false},
		{"a policy of another sub-bucket", alice, ofDrafts, nil, iam.ActionGetObject, key, "", iam.SourceNone, false},
		{"the zero policy", alice, &zero, nil, iam.ActionGetObject, key, "", iam.SourceNone, false},
		// Listings: a prefix counts only if it begins "reports/".
		{"a listing of the sub-bucket", alice, reports, nil, iam.ActionListBucket, site, "reports/", iam.SourceSubBucket, true},
		{"a listing deeper in it", alice, reports, nil, iam.ActionListBucket, site, "reports/2026/", iam.SourceSubBucket, true},
		{"a listing of the bucket", alice, reports, nil, iam.ActionListBucket, site, "", iam.SourceNone, false},
		{"a prefix without the slash", alice, reports, nil, iam.ActionListBucket, site, "reports", iam.SourceNone, false},
		{"a prefix of a look-alike", alice, reports, nil, iam.ActionListBucket, site, "reports-x/", iam.SourceNone, false},
		{"a prefix that is shorter", alice, reports, nil, iam.ActionListBucket, site, "rep", iam.SourceNone, false},
		{"a prefix on an object request", alice, reports, nil, iam.ActionGetObject, top, "reports/", iam.SourceNone, false},
		{"a prefix on the bucket's policy", alice, reports, nil, iam.ActionGetBucketPolicy, site, "reports/", iam.SourceNone, false},
		{"a listing asked on a key", alice, reports, nil, iam.ActionListBucket, key, "reports/", iam.SourceNone, false},
		{"an object action asked on the bucket", alice, reports, nil, iam.ActionGetObject, site, "reports/", iam.SourceNone, false},
	}
	for _, tt := range tests {
		req := iam.Request{
			Principal: tt.principal, Action: tt.action, Resource: tt.resource, ListPrefix: tt.prefix,
			BucketPolicy: tt.bucket, SubBucketPolicy: tt.sub, Now: policyNow,
		}
		source, err := iam.Evaluate(req)
		if (err == nil) != tt.allowed || source != tt.source {
			t.Errorf("%s: %s, %v; want %s, allowed %v", tt.name, source, err, tt.source, tt.allowed)
		}
		if err != nil && !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("%s: %v is no denial", tt.name, err)
		}
		if decided := iam.Decide(req); (decided == nil) != (err == nil) {
			t.Errorf("%s: Decide says %v, Evaluate %v", tt.name, decided, err)
		}
	}
}

// TestEvaluateNamesTheSource: every kind of answer names where it came from,
// and the refusals read as they did before there were three sources.
func TestEvaluateNamesTheSource(t *testing.T) {
	t.Parallel()
	page := iam.Resource{Bucket: "site", Key: "index.html"}
	public := mustBucketPolicy(t, publicRead, "site")
	denyAll := mustBucketPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`, "site")
	reader := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"s3:GetObject"}, Resources: []string{"*"}}}}
	tests := []struct {
		name   string
		req    iam.Request
		source iam.Source
		err    string
	}{
		{
			"default deny",
			iam.Request{Principal: iam.Principal{AccessKeyID: "A", User: "alice"}, Action: iam.ActionGetObject, Resource: page},
			iam.SourceNone, "access denied: user/alice may not s3:GetObject on arn:aws:s3:::site/index.html",
		},
		{
			"own policy",
			iam.Request{Principal: iam.Principal{AccessKeyID: "A", User: "alice", Policies: []iam.Document{reader}}, Action: iam.ActionGetObject, Resource: page},
			iam.SourceIdentity, "",
		},
		{"anonymous, public", iam.Request{Action: iam.ActionGetObject, Resource: page, BucketPolicy: public}, iam.SourceBucket, ""},
		{
			"anonymous, denied",
			iam.Request{Action: iam.ActionGetObject, Resource: page, BucketPolicy: denyAll},
			iam.SourceBucket, "access denied: anonymous principal may not s3:GetObject on arn:aws:s3:::site/index.html",
		},
		{
			"anonymous, silence",
			iam.Request{Action: iam.ActionListBucket, Resource: iam.Resource{Bucket: "site", Key: "x"}, BucketPolicy: denyAll},
			iam.SourceNone, "access denied: anonymous principal may not s3:ListBucket on arn:aws:s3:::site/x",
		},
		{
			"anonymous, a write",
			iam.Request{Action: iam.ActionPutObject, Resource: page, BucketPolicy: public},
			iam.SourceRule, "access denied: anonymous principal may not s3:PutObject on arn:aws:s3:::site/index.html",
		},
		{
			"root",
			iam.Request{Principal: iam.Principal{AccessKeyID: "R", Root: true}, Action: iam.ActionDeleteBucket, Resource: iam.Resource{Bucket: "site"}},
			iam.SourceRule, "",
		},
		{"a node", iam.Request{Principal: iam.Principal{NodeID: "n1"}, Action: iam.ActionNodeRPC}, iam.SourceRule, ""},
		{
			"a service",
			iam.Request{Principal: iam.Principal{Service: iam.ServiceLifecycle}, Action: iam.ActionGetObject, Resource: page},
			iam.SourceRule, "access denied: service/lifecycle may not s3:GetObject on arn:aws:s3:::site/index.html",
		},
		{
			"a locked session",
			iam.Request{Principal: iam.Principal{AccessKeyID: "A", User: "alice", LockedSession: true}, Action: iam.ActionGetObject, Resource: page},
			iam.SourceRule, "the session is locked: user/alice may not s3:GetObject on arn:aws:s3:::site/index.html until it is unlocked",
		},
		{
			"the owner previews",
			iam.Request{Principal: iam.Principal{AccessKeyID: "A", User: "alice"}, Action: iam.ActionPreviewObject, Resource: page, ObjectCreator: "user/alice"},
			iam.SourceOwner, "",
		},
		{"the key's bound", iam.Request{
			Principal: iam.Principal{AccessKeyID: "A", User: "alice", Policies: []iam.Document{reader}, Boundary: &iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{
				{Effect: iam.Allow, Actions: []string{"s3:PutObject"}, Resources: []string{"*"}},
			}}}, Action: iam.ActionGetObject, Resource: page,
		}, iam.SourceKeyBound, "access denied: the API key's own policy does not allow s3:GetObject on arn:aws:s3:::site/index.html"},
	}
	for _, tt := range tests {
		source, err := iam.Evaluate(tt.req)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if source != tt.source || got != tt.err {
			t.Errorf("%s: %s, %q; want %s, %q", tt.name, source, got, tt.source, tt.err)
		}
	}
}

// TestSubBucketPolicyProperty: whatever a sub-bucket's policy says, adding
// it never turns a Deny of the bucket's policy - or of the caller's own -
// into an Allow, never lets an anonymous caller in, never takes an API key
// past its bound, and never allows anything outside the sub-bucket.
func TestSubBucketPolicyProperty(t *testing.T) {
	t.Parallel()
	objects, listings := subBucketActions()
	actions := append(append(slices.Clone(objects), listings...), iam.ActionGetBucketPolicy, iam.ActionPutSubBucket, iam.ActionDeleteBucket, iam.ActionGetBucketLocation)
	principals := []string{`"*"`, aliceARN, `{"AWS":"arn:aws:iam:::user/bob"}`, `{"AWS":["arn:aws:iam:::user/alice","arn:aws:iam:::user/bob"]}`}
	resources := []string{siteARN, reportKeys, `"arn:aws:s3:::site/reports/a*"`, `"arn:aws:s3:::site/reports/q?.csv"`}
	keys := []string{"reports/q1.csv", "reports/a/b", "reports", "reports-x/q1.csv", "index.html", ""}
	prefixes := []string{"", "reports/", "reports", "reports-x/", "reports/a", "r"}
	pick := func(n uint8, of int) int { return int(n) % of }
	document := func(picks []uint8) string {
		var statements []string
		for i := 0; i+3 < len(picks) && len(statements) < 4; i += 4 {
			effect := "Allow"
			if picks[i]%2 == 1 {
				effect = "Deny"
			}
			action, resource := `"`+string(actions[pick(picks[i+2], len(actions))])+`"`, resources[pick(picks[i+3], len(resources))]
			if effect == "Allow" && picks[i]%4 == 0 {
				// Now and then a listing too, which is allowed only.
				action = `["` + strings.Trim(action, `"`) + `","` + string(listings[pick(picks[i+2]/32, len(listings))]) + `"]`
				resource = `[` + resource + `,` + siteARN + `]`
			}
			statements = append(statements, subStatement(effect, principals[pick(picks[i+1], len(principals))], action, resource))
		}
		return subDocument(statements...)
	}
	kept := 0
	property := func(picks []uint8, ofBucket uint8, action, key, prefix, who uint8) bool {
		policy, err := iam.ParseSubBucketPolicy([]byte(document(picks)), "site", "reports")
		if err != nil {
			return errors.Is(err, domain.ErrInvalidPolicy)
		}
		kept++
		// The bucket's policy: nothing, a Deny for everyone of the
		// action, or a public read.
		var bucketPolicy *iam.BucketPolicy
		a := actions[pick(action, len(actions))]
		switch ofBucket % 3 {
		case 1:
			raw, err := json.Marshal(map[string]any{"Version": iam.PolicyVersion, "Statement": []any{map[string]any{
				"Effect": "Deny", "Principal": "*", "Action": string(a), "Resource": []string{"arn:aws:s3:::site", "arn:aws:s3:::site/*"},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := iam.ParseBucketPolicy(raw, "site")
			if err != nil {
				t.Fatal(err)
			}
			bucketPolicy = &parsed
		case 2:
			bucketPolicy = mustBucketPolicy(t, publicRead, "site")
		}
		everything := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"*"}, Resources: []string{"*"}}}}
		denyOwn := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Deny, Actions: []string{"*"}, Resources: []string{"*"}}}}
		bound := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"admin:GetMetrics"}, Resources: []string{"*"}}}}
		principal := []iam.Principal{
			{},
			{AccessKeyID: "A", User: "alice"},
			{AccessKeyID: "B", User: "bob", Policies: []iam.Document{everything}},
			{AccessKeyID: "A", User: "alice", Policies: []iam.Document{denyOwn}},
			{AccessKeyID: "K", User: "alice", Boundary: &bound},
		}[pick(who, 5)]
		resource := iam.Resource{Bucket: "site", Key: keys[pick(key, len(keys))]}
		if slices.Contains(listings, a) && key%4 != 0 {
			// A listing is mostly asked on the bucket, as it is made.
			resource.Key = ""
		}
		req := iam.Request{Principal: principal, Action: a, Resource: resource, BucketPolicy: bucketPolicy, ListPrefix: prefixes[pick(prefix, len(prefixes))], Now: policyNow}
		without := iam.Decide(req)
		req.SubBucketPolicy = &policy
		source, with := iam.Evaluate(req)
		switch {
		case with == nil && ofBucket%3 == 1:
			t.Logf("a Deny of the bucket became an Allow: %+v", req)
			return false
		case with == nil && principal.Anonymous() && without != nil:
			t.Logf("an anonymous caller was let in: %+v", req)
			return false
		case with == nil && (principal.Boundary != nil || pick(who, 5) == 3):
			t.Logf("a bound or a Deny of the caller's own was passed: %+v", req)
			return false
		case with == nil && without != nil:
			// What it adds is in the sub-bucket, and it says so.
			in := (resource.Key != "" && strings.HasPrefix(resource.Key, "reports/") && slices.Contains(objects, a)) ||
				(resource.Key == "" && strings.HasPrefix(req.ListPrefix, "reports/") && slices.Contains(listings, a))
			if !in || source != iam.SourceSubBucket {
				t.Logf("allowed outside the sub-bucket, by %s: %+v", source, req)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 8000}); err != nil {
		t.Error(err)
	}
	t.Logf("%d of the generated policies were kept", kept)
	if kept < 400 {
		t.Errorf("only %d of the generated policies were kept: the generator tests too little", kept)
	}
}

// TestSubBucketActionsOfTheBuiltInRoles: the actions of sub-buckets are
// held as the bucket's policy is - by the roles that hold "s3:*" - and by no
// role that reads or writes objects only, nor by the auditor.
func TestSubBucketActionsOfTheBuiltInRoles(t *testing.T) {
	t.Parallel()
	policies := iam.BuiltInPolicies()
	site := iam.Resource{Bucket: "site"}
	for _, action := range []iam.Action{iam.ActionListSubBuckets, iam.ActionPutSubBucket, iam.ActionDeleteSubBucket, iam.ActionGetSubBucketPolicy, iam.ActionGetBucketPolicy} {
		for role := range policies {
			err := decide(user("u", policies[role]), action, site)
			want := role == iam.RoleClusterAdmin || role == iam.RoleBucketAdmin
			if want != (err == nil) {
				t.Errorf("%s: %s allowed = %v, want %v", role, action, err == nil, want)
			}
		}
	}
	// A bucket policy may grant them to a user, as it may the bucket's
	// policy, and never to a caller who has not authenticated.
	grant := mustBucketPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*",
		"Action":["s3:ListSubBuckets","s3:PutSubBucket","s3:DeleteSubBucket","s3:GetSubBucketPolicy"],"Resource":"arn:aws:s3:::site"}]}`, "site")
	for _, action := range []iam.Action{iam.ActionListSubBuckets, iam.ActionPutSubBucket, iam.ActionDeleteSubBucket, iam.ActionGetSubBucketPolicy} {
		if err := iam.Decide(iam.Request{Principal: iam.Principal{AccessKeyID: "A", User: "alice"}, Action: action, Resource: site, BucketPolicy: grant}); err != nil {
			t.Errorf("a bucket policy does not grant %s: %v", action, err)
		}
		if err := iam.Decide(iam.Request{Action: action, Resource: site, BucketPolicy: grant}); err == nil {
			t.Errorf("an anonymous caller was allowed %s", action)
		}
	}
	// As the contract names them (x-cactusfs-action).
	for action, name := range map[iam.Action]string{
		iam.ActionListSubBuckets: "s3:ListSubBuckets", iam.ActionPutSubBucket: "s3:PutSubBucket",
		iam.ActionDeleteSubBucket: "s3:DeleteSubBucket", iam.ActionGetSubBucketPolicy: "s3:GetSubBucketPolicy",
	} {
		if string(action) != name {
			t.Errorf("%s is named %s in the contract", action, name)
		}
	}
}
