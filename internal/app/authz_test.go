package app_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// TestAllowedIsAuthorizeWithoutItsRecord: Allowed and Authorize make the
// same decision of every request, bucket policy included; a refusal by
// Authorize is in the audit trail and in the log, one by Allowed in neither.
func TestAllowedIsAuthorizeWithoutItsRecord(t *testing.T) {
	t.Parallel()
	allow := func(action, resource string) iam.Document {
		return iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{
			Effect: iam.Allow, Actions: []string{action}, Resources: []string{resource},
		}}}
	}
	lister := iam.Principal{AccessKeyID: "L", User: "lister", Policies: []iam.Document{allow("s3:ListBucket", "arn:aws:s3:::site")}}
	everything := iam.Principal{AccessKeyID: "E", User: "everything", Policies: []iam.Document{allow("s3:*", "*")}}
	bounded := allow("s3:GetObject", "*")
	key := iam.Principal{AccessKeyID: "K", User: "everything", Policies: everything.Policies, Boundary: &bounded}
	alice := iam.Principal{AccessKeyID: "A", User: "alice"}
	site, other := iam.Resource{Bucket: "site"}, iam.Resource{Bucket: "other"}
	page := iam.Resource{Bucket: "site", Key: "index.html"}

	tests := map[string]struct {
		request iam.Request
		allowed bool
	}{
		"root":                                   {iam.Request{Principal: porttest.Root(), Action: iam.ActionDeleteBucket, Resource: site}, true},
		"a user, by their own policy":            {iam.Request{Principal: lister, Action: iam.ActionListBucket, Resource: site}, true},
		"a user, another bucket":                 {iam.Request{Principal: lister, Action: iam.ActionListBucket, Resource: other}, false},
		"a user, another action":                 {iam.Request{Principal: lister, Action: iam.ActionListAllMyBuckets}, false},
		"a user with no policy":                  {iam.Request{Principal: iam.Principal{AccessKeyID: "N", User: "nobody"}, Action: iam.ActionListBucket, Resource: site}, false},
		"a user, by the bucket's policy":         {iam.Request{Principal: alice, Action: iam.ActionListBucket, Resource: site}, true},
		"a user, where the bucket's policy ends": {iam.Request{Principal: alice, Action: iam.ActionPutObject, Resource: page}, false},
		"a user the bucket denies":               {iam.Request{Principal: everything, Action: iam.ActionDeleteObject, Resource: page}, false},
		"a key within its bound":                 {iam.Request{Principal: key, Action: iam.ActionGetObject, Resource: page}, true},
		"a key beyond its bound":                 {iam.Request{Principal: key, Action: iam.ActionListBucket, Resource: site}, false},
		"nobody, what is public":                 {iam.Request{Action: iam.ActionGetObject, Resource: page}, true},
		"nobody, what is not":                    {iam.Request{Action: iam.ActionListBucket, Resource: site}, false},
		"nobody, the buckets":                    {iam.Request{Action: iam.ActionListAllMyBuckets}, false},
		"a node, what is a user's":               {iam.Request{Principal: iam.Principal{NodeID: "n1"}, Action: iam.ActionListBucket, Resource: site}, false},
		"an administration action":               {iam.Request{Principal: lister, Action: "admin:ListUsers", Resource: iam.Resource{Admin: "users"}}, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			meta, trail, clock, logs := porttest.NewMemMetadata(), porttest.NewMemIAM(), porttest.NewClock(testNow), &syncBuffer{}
			if err := meta.CreateBucket(ctx, bucket.Bucket{Name: "site", CreatedAt: testNow}); err != nil {
				t.Fatal(err)
			}
			policy := `{"Version":"2012-10-17","Statement":[
				{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"},
				{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/alice"},"Action":"s3:ListBucket","Resource":"arn:aws:s3:::site"},
				{"Effect":"Deny","Principal":"*","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::site/*"}]}`
			if err := meta.SetBucketConfig(ctx, "site", bucket.ConfigPolicy, []byte(policy)); err != nil {
				t.Fatal(err)
			}
			logger := slog.New(slog.NewJSONHandler(logs, nil))
			auditor := app.NewAuditor(trail, clock, logger)
			authz := app.NewAuthorizer(logger, clock, auditor).WithBucketPolicies(meta)
			denials := func() int {
				auditor.Flush(ctx)
				records, err := trail.ListAudit(ctx, 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				n := 0
				for _, r := range records {
					if r.Outcome == audit.Denied {
						n++
					}
				}
				return n
			}

			allowed, err := authz.Allowed(ctx, tt.request)
			if err != nil || allowed != tt.allowed {
				t.Fatalf("Allowed = %v, %v; want %v", allowed, err, tt.allowed)
			}
			if n := denials(); n != 0 || strings.Contains(logs.String(), "authorization denied") {
				t.Fatalf("Allowed left %d audit records and this in the log: %s", n, logs.String())
			}
			err = authz.Authorize(ctx, tt.request)
			if (err == nil) != allowed || (err != nil && !errors.Is(err, domain.ErrAccessDenied)) {
				t.Fatalf("Authorize = %v where Allowed = %v", err, allowed)
			}
			want := 1
			if allowed {
				want = 0
			}
			if n := denials(); n != want || strings.Contains(logs.String(), "authorization denied") == allowed {
				t.Errorf("Authorize left %d audit records, want %d; the log: %s", n, want, logs.String())
			}
		})
	}
}

// TestAllowedWhenNoDecisionCanBeMade: a policy that cannot be read allows
// nothing, and that is an error and no refusal - for Allowed as for
// Authorize.
func TestAllowedWhenNoDecisionCanBeMade(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	user := iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{{
		Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"s3:*"}, Resources: []string{"*"}}},
	}}}
	request := iam.Request{Principal: user, Action: iam.ActionListBucket, Resource: iam.Resource{Bucket: "site"}}
	for name, tt := range map[string]struct {
		source app.BucketPolicySource
		want   error
	}{
		"unavailable": {failingPolicies{}, domain.ErrUnavailable},
		"damaged":     {brokenPolicies{}, domain.ErrIntegrity},
	} {
		authz := app.NewAuthorizer(slog.New(slog.DiscardHandler), porttest.NewClock(testNow), nil).WithBucketPolicies(tt.source)
		allowed, err := authz.Allowed(ctx, request)
		if allowed || !errors.Is(err, tt.want) || errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("%s: Allowed = %v, %v; want false and %v", name, allowed, err, tt.want)
		}
		if err := authz.Authorize(ctx, request); !errors.Is(err, tt.want) {
			t.Errorf("%s: Authorize = %v, want %v", name, err, tt.want)
		}
		// Whoever no bucket policy binds is asked nothing about one.
		request := request
		request.Principal = porttest.Root()
		if allowed, err := authz.Allowed(ctx, request); !allowed || err != nil {
			t.Errorf("%s: Allowed for root = %v, %v", name, allowed, err)
		}
	}
}

// TestRequestScopeReadsForWhoIsBound: within one request scope the policy of
// a bucket is read once, for the first request it binds - and what was
// decided before for somebody it does not bind, the root identity, leaves
// nothing behind that a later request of the scope would be decided from.
func TestRequestScopeReadsForWhoIsBound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	meta, clock := porttest.NewMemMetadata(), porttest.NewClock(testNow)
	if err := meta.CreateBucket(ctx, bucket.Bucket{Name: "site", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	deny := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:ListBucket","Resource":"arn:aws:s3:::site"}]}`
	if err := meta.SetBucketConfig(ctx, "site", bucket.ConfigPolicy, []byte(deny)); err != nil {
		t.Fatal(err)
	}
	source := &countingPolicies{meta: meta, clock: clock}
	authz := app.NewAuthorizer(slog.New(slog.DiscardHandler), clock, nil).WithBucketPolicies(source)
	user := iam.Principal{AccessKeyID: "E", User: "everything", Policies: []iam.Document{{
		Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"s3:*"}, Resources: []string{"*"}}},
	}}}
	list := func(principal iam.Principal) iam.Request {
		return iam.Request{Principal: principal, Action: iam.ActionListBucket, Resource: iam.Resource{Bucket: "site"}}
	}

	scope := app.WithRequestScope(ctx)
	if allowed, err := authz.Allowed(scope, list(porttest.Root())); !allowed || err != nil {
		t.Fatalf("root = %v, %v", allowed, err)
	}
	if one := source.one.Load(); one != 0 {
		t.Errorf("a request by root read %d policies", one)
	}
	// The same scope, the same bucket, somebody the policy binds: decided
	// by the bucket's Deny, which is read now, once.
	for range 3 {
		if allowed, err := authz.Allowed(scope, list(user)); allowed || err != nil {
			t.Errorf("a user the bucket denies, after root in the same scope = %v, %v", allowed, err)
		}
		if err := authz.Authorize(scope, list(user)); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("Authorize for a user the bucket denies, after root in the same scope = %v", err)
		}
	}
	if allowed, err := authz.Allowed(scope, list(porttest.Root())); !allowed || err != nil {
		t.Errorf("root again = %v, %v", allowed, err)
	}
	if one, all := source.one.Load(), source.all.Load(); one != 1 || all != 0 {
		t.Errorf("the scope read %d policies of one bucket and %d pages of all, want 1 and none", one, all)
	}
}

// TestPrimedScopeWalksInAnyOrder: a scope that was told where the policies
// of all buckets are answers about any bucket, in whatever order it is
// asked - in name order at the price of one walk, and about a bucket it has
// left behind by beginning again there. No bucket is read on its own.
func TestPrimedScopeWalksInAnyOrder(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	meta, clock := porttest.NewMemMetadata(), porttest.NewClock(testNow)
	// Buckets b00 to b19; every third opens itself to alice, every seventh
	// has a policy that is damaged.
	want := map[string]error{}
	var names []string
	for i := range 20 {
		name := fmt.Sprintf("b%02d", i)
		names = append(names, name)
		if err := meta.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: testNow}); err != nil {
			t.Fatal(err)
		}
		want[name] = domain.ErrAccessDenied
		document := ""
		switch {
		case i%7 == 6:
			document, want[name] = `{"Version":`, domain.ErrIntegrity
		case i%3 == 0:
			document = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/alice"},` +
				`"Action":"s3:ListBucket","Resource":"arn:aws:s3:::` + name + `"}]}`
			want[name] = nil
		}
		if err := meta.SetBucketConfig(ctx, name, bucket.ConfigPolicy, []byte(document)); err != nil {
			t.Fatal(err)
		}
	}
	// A bucket that is not there has no policy.
	want["zzz-not-there"], want["a-before-all"] = domain.ErrAccessDenied, domain.ErrAccessDenied
	source := &countingPolicies{meta: meta, clock: clock}
	authz := app.NewAuthorizer(slog.New(slog.DiscardHandler), clock, nil).WithBucketPolicies(source)
	alice := iam.Principal{AccessKeyID: "A", User: "alice"}
	ask := func(scope context.Context, name string) {
		t.Helper()
		allowed, err := authz.Allowed(scope, iam.Request{Principal: alice, Action: iam.ActionListBucket, Resource: iam.Resource{Bucket: name}})
		switch {
		case errors.Is(want[name], domain.ErrIntegrity):
			if allowed || !errors.Is(err, domain.ErrIntegrity) {
				t.Errorf("%s, whose policy is damaged = %v, %v", name, allowed, err)
			}
		case err != nil || allowed != (want[name] == nil):
			t.Errorf("%s = %v, %v; want allowed: %v", name, allowed, err, want[name] == nil)
		}
	}
	primed := func() context.Context {
		scope := app.WithRequestScope(ctx)
		app.PrimeBucketPolicies(scope, source)
		return scope
	}

	// In name order: one walk, seven pages of three buckets.
	scope := primed()
	for _, name := range names {
		ask(scope, name)
	}
	ask(scope, "zzz-not-there")
	if one, all := source.one.Load(), source.all.Load(); one != 0 || all != 7 {
		t.Errorf("in name order: %d policies of one bucket and %d pages of all were read, want none and 7", one, all)
	}
	// In any order, and the same bucket again and again.
	scope = primed()
	for _, i := range []int{19, 0, 0, 7, 6, 6, 18, 3, 13, 12, 1, 19, 9} {
		ask(scope, names[i])
	}
	ask(scope, "a-before-all")
	if one := source.one.Load(); one != 0 {
		t.Errorf("out of order: %d policies of one bucket were read", one)
	}
	// What the walks parsed is remembered; what does not parse is not.
	if entries, _ := authz.ParsedPolicies(); entries != 6 {
		t.Errorf("the authorizer remembers %d policies, want the 6 that parse", entries)
	}
	// A request that is gone is not decided, and that is what it says.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	scope = app.WithRequestScope(gone)
	app.PrimeBucketPolicies(scope, source)
	_, err := authz.Allowed(scope, iam.Request{Principal: alice, Action: iam.ActionListBucket, Resource: iam.Resource{Bucket: "b00"}})
	if !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Allowed for a request that is gone = %v", err)
	}
	// A store whose pages do not go forward is not walked for ever.
	scope = app.WithRequestScope(ctx)
	app.PrimeBucketPolicies(scope, stuckPolicies{})
	_, err = authz.Allowed(scope, iam.Request{Principal: alice, Action: iam.ActionListBucket, Resource: iam.Resource{Bucket: "b19"}})
	if !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Allowed over pages that do not go forward = %v", err)
	}
}

// stuckPolicies is a store of policies whose every page says to go on where
// the first page ends.
type stuckPolicies struct{}

func (stuckPolicies) BucketPolicies(context.Context, string, func(string, []byte) error) (string, error) {
	return "b05", nil
}

// TestAuthorizerOnALockedSession: a locked session (ADR-0027) is refused
// everything but storing an object - by Authorize with a record in the audit
// trail that says why and a line in the log, by Allowed with the same error
// and never with a plain "no", which would leave a list empty. That refusal
// of Allowed ends the operation that asked, so it is recorded too: once, the
// repeat being counted. Storing an object is decided by the policies as
// ever. No policy of a bucket is read to refuse a locked session: not one,
// and not a page of a walk of them all.
func TestAuthorizerOnALockedSession(t *testing.T) {
	t.Parallel()
	everything := []iam.Document{{
		Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"*"}, Resources: []string{"*"}}},
	}}
	locked := iam.Principal{User: "alice", Policies: everything, LockedSession: true}
	reader := iam.Principal{User: "bob", Policies: []iam.Document{iam.BuiltInPolicies()[iam.RoleReadOnly]}, LockedSession: true}
	site, page := iam.Resource{Bucket: "site"}, iam.Resource{Bucket: "site", Key: "index.html"}

	tests := map[string]struct {
		request iam.Request
		// primed makes the request scope one of a search: the policies of
		// all buckets are walked.
		primed bool
		// want is nil when the request is allowed.
		want error
	}{
		"listing the buckets":                   {request: iam.Request{Principal: locked, Action: iam.ActionListAllMyBuckets}, want: domain.ErrSessionLocked},
		"listing a bucket":                      {request: iam.Request{Principal: locked, Action: iam.ActionListBucket, Resource: site}, want: domain.ErrSessionLocked},
		"listing a bucket, in a search":         {request: iam.Request{Principal: locked, Action: iam.ActionListBucket, Resource: site}, primed: true, want: domain.ErrSessionLocked},
		"reading an object":                     {request: iam.Request{Principal: locked, Action: iam.ActionGetObject, Resource: page}, want: domain.ErrSessionLocked},
		"what the bucket's policy makes public": {request: iam.Request{Principal: reader, Action: iam.ActionGetObject, Resource: page}, want: domain.ErrSessionLocked},
		"deleting an object":                    {request: iam.Request{Principal: locked, Action: iam.ActionDeleteObject, Resource: page}, want: domain.ErrSessionLocked},
		"administering users":                   {request: iam.Request{Principal: locked, Action: iam.ActionPutUser, Resource: iam.Resource{Admin: "user/eve"}}, want: domain.ErrSessionLocked},
		"reading the audit trail":               {request: iam.Request{Principal: locked, Action: iam.ActionGetAuditLog, Resource: iam.Resource{Admin: "audit"}}, want: domain.ErrSessionLocked},
		"storing an object":                     {request: iam.Request{Principal: locked, Action: iam.ActionPutObject, Resource: page}},
		"storing an object, in a search":        {request: iam.Request{Principal: locked, Action: iam.ActionPutObject, Resource: page}, primed: true},
		"storing an object without the right":   {request: iam.Request{Principal: reader, Action: iam.ActionPutObject, Resource: page}, want: domain.ErrAccessDenied},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := app.WithRequestScope(t.Context())
			meta, trail, clock, logs := porttest.NewMemMetadata(), porttest.NewMemIAM(), porttest.NewClock(testNow), &syncBuffer{}
			if err := meta.CreateBucket(ctx, bucket.Bucket{Name: "site", CreatedAt: testNow}); err != nil {
				t.Fatal(err)
			}
			public := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`
			if err := meta.SetBucketConfig(ctx, "site", bucket.ConfigPolicy, []byte(public)); err != nil {
				t.Fatal(err)
			}
			source := &countingPolicies{meta: meta, clock: clock}
			if tt.primed {
				app.PrimeBucketPolicies(ctx, source)
			}
			logger := slog.New(slog.NewJSONHandler(logs, nil))
			auditor := app.NewAuditor(trail, clock, logger)
			authz := app.NewAuthorizer(logger, clock, auditor).WithBucketPolicies(source)
			records := func() []audit.Record {
				auditor.Flush(ctx)
				all, err := trail.ListAudit(ctx, 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				return all
			}
			isLock := errors.Is(tt.want, domain.ErrSessionLocked)

			allowed, err := authz.Allowed(ctx, tt.request)
			switch {
			case isLock:
				// Never (false, nil): a filter would take that for "leave
				// it out" and answer an empty list.
				if allowed || !errors.Is(err, domain.ErrSessionLocked) || errors.Is(err, domain.ErrAccessDenied) {
					t.Fatalf("Allowed = %v, %v; want false and the session is locked", allowed, err)
				}
			case err != nil || allowed != (tt.want == nil):
				t.Fatalf("Allowed = %v, %v; want %v and no error", allowed, err, tt.want == nil)
			}
			wantLines := 0
			if isLock {
				wantLines = 1
			}
			if n := len(records()); n != wantLines || strings.Count(logs.String(), `"msg"`) != wantLines {
				t.Fatalf("Allowed left %d audit records, want %d, and this in the log: %s", n, wantLines, logs.String())
			}

			err = authz.Authorize(ctx, tt.request)
			if tt.want == nil {
				if err != nil || len(records()) != 0 {
					t.Fatalf("Authorize = %v, with %d audit records", err, len(records()))
				}
				return
			}
			if !errors.Is(err, tt.want) || errors.Is(err, domain.ErrSessionLocked) != isLock || (isLock && errors.Is(err, domain.ErrAccessDenied)) {
				t.Fatalf("Authorize = %v, want %v", err, tt.want)
			}
			all := records()
			reason, line := "not allowed by policy", "authorization denied"
			if isLock {
				reason, line = "the session is locked", "refused: the session is locked"
			}
			// A record for the refusal of Authorize - and, of a locked
			// session, the one Allowed left before; a line in the log for
			// each. (Asking for the trail in between made the auditor
			// forget that the second is a repeat of the first.)
			if len(all) != wantLines+1 || strings.Count(logs.String(), `"msg"`) != wantLines+1 || strings.Count(logs.String(), line) != wantLines+1 {
				t.Fatalf("Authorize left %+v; the log: %s", all, logs.String())
			}
			for _, r := range all {
				if r.Outcome != audit.Denied || r.Action != string(tt.request.Action) || r.Actor != "user/"+tt.request.Principal.User || r.Details["reason"] != reason {
					t.Errorf("the record of the refusal = %+v", r)
				}
			}
			if isLock && (source.one.Load() != 0 || source.all.Load() != 0) {
				t.Errorf("refusing a locked session read %d policies and %d pages of them", source.one.Load(), source.all.Load())
			}
		})
	}
}

// subBucketSite is a bucket "site" that anybody may read (its policy), with
// sub-buckets: "reports", whose policy lets bob and erin in and denies carol
// what she would otherwise be allowed; "private", whose policy denies every
// reader; "open" and "reports-x", without a policy; and "broken", whose
// stored policy cannot be a sub-bucket's - a resource outside it, which the
// parser's reason would quote. "other" is a bucket without sub-buckets.
func subBucketSite(t *testing.T) *porttest.MemMetadata {
	t.Helper()
	ctx := t.Context()
	meta := porttest.NewMemMetadata()
	for _, name := range []string{"site", "other"} {
		if err := meta.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: testNow}); err != nil {
			t.Fatal(err)
		}
	}
	public := `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"},
		{"Effect":"Deny","Principal":{"AWS":"arn:aws:iam:::user/mallory"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"}]}`
	if err := meta.SetBucketConfig(ctx, "site", bucket.ConfigPolicy, []byte(public)); err != nil {
		t.Fatal(err)
	}
	reports := `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":["arn:aws:iam:::user/bob","arn:aws:iam:::user/erin"]},"Action":["s3:PutObject","s3:GetObject"],"Resource":"arn:aws:s3:::site/reports/*"},
		{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":["s3:ListBucket","s3:ListBucketVersions","s3:ListBucketMultipartUploads"],"Resource":"arn:aws:s3:::site"},
		{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/mallory"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/reports/*"},
		{"Effect":"Deny","Principal":{"AWS":"arn:aws:iam:::user/carol"},"Action":"s3:DeleteObject","Resource":"arn:aws:s3:::site/reports/*"}]}`
	private := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/private/*"}]}`
	broken := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::site/elsewhere-sentinel/*"}]}`
	for name, policy := range map[string]string{"reports": reports, "private": private, "broken": broken, "open": "", "reports-x": ""} {
		if err := meta.PutSubBucket(ctx, "site", bucket.SubBucket{Name: name, Policy: []byte(policy), CreatedAt: testNow}, audit.Record{}); err != nil {
			t.Fatal(err)
		}
	}
	return meta
}

// TestSubBucketPolicies: the authorizer decides a request in a sub-bucket
// from three sources (ADR-0017) - the caller's own policies, the bucket's and
// the sub-bucket's: a Deny in any refuses, an Allow in any allows, the bound
// of an API key holds after. The sub-bucket is the first segment of the key,
// or of the prefix of a listing, and only one that is registered: neither
// "reports-x/…" nor "reports" is in "reports". A stored sub-bucket policy that
// does not parse refuses what is in that sub-bucket and nothing else, and is
// logged without what it says. An anonymous caller is refused by a
// sub-bucket's Deny, under a bucket that is public. Every refusal names its
// source in the log and in the audit trail.
func TestSubBucketPolicies(t *testing.T) {
	t.Parallel()
	allow := func(effect iam.Effect, action string) []iam.Document {
		return []iam.Document{{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: effect, Actions: []string{action}, Resources: []string{"*"}}}}}
	}
	bob := iam.Principal{AccessKeyID: "B", User: "bob"}
	readOnly := allow(iam.Allow, "s3:GetObject")[0]
	bobsKey := iam.Principal{AccessKeyID: "BK", User: "bob", Boundary: &readOnly}
	carol := iam.Principal{AccessKeyID: "C", User: "carol", Policies: allow(iam.Allow, "s3:*")}
	dave := iam.Principal{AccessKeyID: "D", User: "dave", Policies: allow(iam.Allow, "s3:PutObject")}
	erin := iam.Principal{AccessKeyID: "E", User: "erin", Policies: allow(iam.Deny, "s3:PutObject")}
	mallory := iam.Principal{AccessKeyID: "M", User: "mallory"}
	anonymous := iam.Principal{}
	key := func(k string) iam.Resource { return iam.Resource{Bucket: "site", Key: k} }
	site := iam.Resource{Bucket: "site"}

	tests := map[string]struct {
		request iam.Request
		// want is nil when the request is allowed; source is that of a
		// refusal.
		want   error
		source iam.Source
	}{
		"allow in the sub-bucket's policy":       {request: iam.Request{Principal: bob, Action: iam.ActionPutObject, Resource: key("reports/q1.csv")}},
		"allow in the caller's own policy":       {request: iam.Request{Principal: dave, Action: iam.ActionPutObject, Resource: key("reports/q1.csv")}},
		"allow in the bucket's policy":           {request: iam.Request{Principal: anonymous, Action: iam.ActionGetObject, Resource: key("index.html")}},
		"allow in the bucket's, in a sub-bucket": {request: iam.Request{Principal: anonymous, Action: iam.ActionGetObject, Resource: key("reports/q1.csv")}},
		"the bucket's deny wins over the sub-bucket's allow": {
			request: iam.Request{Principal: mallory, Action: iam.ActionGetObject, Resource: key("reports/q1.csv")}, want: domain.ErrAccessDenied, source: iam.SourceBucket,
		},
		"the sub-bucket's deny wins over the caller's allow": {
			request: iam.Request{Principal: carol, Action: iam.ActionDeleteObject, Resource: key("reports/q1.csv")}, want: domain.ErrAccessDenied, source: iam.SourceSubBucket,
		},
		"the caller's deny wins over the sub-bucket's allow": {
			request: iam.Request{Principal: erin, Action: iam.ActionPutObject, Resource: key("reports/q1.csv")}, want: domain.ErrAccessDenied, source: iam.SourceIdentity,
		},
		"the sub-bucket's deny is of its own keys only": {request: iam.Request{Principal: carol, Action: iam.ActionDeleteObject, Resource: key("open/q1.csv")}},
		"the key's bound holds": {
			request: iam.Request{Principal: bobsKey, Action: iam.ActionPutObject, Resource: key("reports/q1.csv")}, want: domain.ErrAccessDenied, source: iam.SourceKeyBound,
		},
		"within the key's bound": {request: iam.Request{Principal: bobsKey, Action: iam.ActionGetObject, Resource: key("reports/q1.csv")}},
		"reports-x/ is not in reports": {
			request: iam.Request{Principal: bob, Action: iam.ActionPutObject, Resource: key("reports-x/q1.csv")}, want: domain.ErrAccessDenied, source: iam.SourceNone,
		},
		"the key reports is not in reports": {
			request: iam.Request{Principal: bob, Action: iam.ActionPutObject, Resource: key("reports")}, want: domain.ErrAccessDenied, source: iam.SourceNone,
		},
		"a key of the same name in another bucket": {
			request: iam.Request{Principal: bob, Action: iam.ActionPutObject, Resource: iam.Resource{Bucket: "other", Key: "reports/q1.csv"}}, want: domain.ErrAccessDenied, source: iam.SourceNone,
		},
		"a listing of reports/":     {request: iam.Request{Principal: bob, Action: iam.ActionListBucket, Resource: site, ListPrefix: "reports/"}},
		"a listing of reports/2026": {request: iam.Request{Principal: bob, Action: iam.ActionListBucketVersions, Resource: site, ListPrefix: "reports/2026"}},
		"a listing of reports": {
			request: iam.Request{Principal: bob, Action: iam.ActionListBucket, Resource: site, ListPrefix: "reports"}, want: domain.ErrAccessDenied, source: iam.SourceNone,
		},
		"a listing of reports-x/": {
			request: iam.Request{Principal: bob, Action: iam.ActionListBucket, Resource: site, ListPrefix: "reports-x/"}, want: domain.ErrAccessDenied, source: iam.SourceNone,
		},
		"a listing of the whole bucket": {
			request: iam.Request{Principal: bob, Action: iam.ActionListBucket, Resource: site}, want: domain.ErrAccessDenied, source: iam.SourceNone,
		},
		"a prefix is a listing's, not a key's": {
			request: iam.Request{Principal: bob, Action: iam.ActionPutObject, Resource: key("top.txt"), ListPrefix: "reports/"}, want: domain.ErrAccessDenied, source: iam.SourceNone,
		},
		"anonymous, a private sub-bucket of a public bucket": {
			request: iam.Request{Principal: anonymous, Action: iam.ActionGetObject, Resource: key("private/salaries.csv")}, want: domain.ErrAccessDenied, source: iam.SourceSubBucket,
		},
		"a user, a private sub-bucket of a public bucket": {
			request: iam.Request{Principal: carol, Action: iam.ActionGetObject, Resource: key("private/salaries.csv")}, want: domain.ErrAccessDenied, source: iam.SourceSubBucket,
		},
		"anonymous, what the sub-bucket allows users": {
			request: iam.Request{Principal: anonymous, Action: iam.ActionPutObject, Resource: key("reports/q1.csv")}, want: domain.ErrAccessDenied, source: iam.SourceRule,
		},
		"a policy that does not parse refuses": {
			request: iam.Request{Principal: dave, Action: iam.ActionPutObject, Resource: key("broken/q1.csv")}, want: domain.ErrIntegrity,
		},
		"a policy that does not parse, read anonymously": {
			request: iam.Request{Principal: anonymous, Action: iam.ActionGetObject, Resource: key("broken/q1.csv")}, want: domain.ErrIntegrity,
		},
		"a policy that does not parse, outside it": {request: iam.Request{Principal: dave, Action: iam.ActionPutObject, Resource: key("open/q1.csv")}},
		"root, where the policy does not parse":    {request: iam.Request{Principal: porttest.Root(), Action: iam.ActionDeleteObject, Resource: key("broken/q1.csv")}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			meta, trail, clock, logs := subBucketSite(t), porttest.NewMemIAM(), porttest.NewClock(testNow), &syncBuffer{}
			logger := slog.New(slog.NewJSONHandler(logs, nil))
			auditor := app.NewAuditor(trail, clock, logger)
			authz := app.NewAuthorizer(logger, clock, auditor).WithBucketPolicies(meta).WithSubBuckets(meta)

			allowed, err := authz.Allowed(ctx, tt.request)
			switch {
			case errors.Is(tt.want, domain.ErrIntegrity):
				if allowed || !errors.Is(err, domain.ErrIntegrity) {
					t.Fatalf("Allowed = %v, %v; want false and %v", allowed, err, tt.want)
				}
			case err != nil || allowed != (tt.want == nil):
				t.Fatalf("Allowed = %v, %v; want %v", allowed, err, tt.want == nil)
			}
			err = authz.Authorize(app.WithRequestScope(ctx), tt.request)
			if (tt.want == nil) != (err == nil) || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("Authorize = %v, want %v", err, tt.want)
			}
			auditor.Flush(ctx)
			records, err := trail.ListAudit(ctx, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			if errors.Is(tt.want, domain.ErrIntegrity) {
				// No decision, so no refusal: what is logged is that the
				// policy cannot be read, and never what it says.
				if len(records) != 0 || !strings.Contains(logs.String(), "the policy of a sub-bucket cannot be read") ||
					!strings.Contains(logs.String(), `"sub_bucket":"broken"`) || strings.Contains(logs.String(), "elsewhere-sentinel") {
					t.Errorf("records %+v; the log: %s", records, logs.String())
				}
				return
			}
			if tt.want == nil {
				if len(records) != 0 {
					t.Errorf("an allowed request left %+v", records)
				}
				return
			}
			if len(records) != 1 || records[0].Outcome != audit.Denied || records[0].Details["source"] != string(tt.source) {
				t.Errorf("the record of the refusal = %+v, want the source %s", records, tt.source)
			}
			if !strings.Contains(logs.String(), `"source":"`+string(tt.source)+`"`) {
				t.Errorf("the log does not name the source %s: %s", tt.source, logs.String())
			}
		})
	}
}

// countingReads stands for a consistent store: it counts every read the
// authorizer makes of it - of the policy of a bucket alone, or of the policy
// and the sub-buckets together - each of which is a question to the leader,
// and can fail them.
type countingReads struct {
	meta  *porttest.MemMetadata
	reads atomic.Int64
	err   error
}

func (c *countingReads) GetBucketConfig(ctx context.Context, name, kind string) ([]byte, error) {
	c.reads.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	return c.meta.GetBucketConfig(ctx, name, kind)
}

func (c *countingReads) GetBucketAccess(ctx context.Context, bucketName string) (ports.BucketAccess, error) {
	c.reads.Add(1)
	if c.err != nil {
		return ports.BucketAccess{}, c.err
	}
	return c.meta.GetBucketAccess(ctx, bucketName)
}

// TestSubBucketPolicyIsCachedAndFollowsChanges: within a request scope the
// sub-buckets of a bucket are read once, however many keys are decided; from
// one request to the next a policy is parsed once for as long as its document
// is the same - and a policy that was changed is read as it is now, at the
// next request: the cache is asked only with the hash of the document just
// read. A sub-bucket that was deleted is forgotten, and no longer decides.
func TestSubBucketPolicyIsCachedAndFollowsChanges(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	meta := subBucketSite(t)
	source := &countingReads{meta: meta}
	clock := porttest.NewClock(testNow)
	authz := app.NewAuthorizer(slog.New(slog.DiscardHandler), clock, nil).WithBucketPolicies(source).WithSubBuckets(source)
	bob := iam.Principal{AccessKeyID: "B", User: "bob"}
	put := func(ctx context.Context, key string) error {
		return authz.Authorize(ctx, iam.Request{Principal: bob, Action: iam.ActionPutObject, Resource: iam.Resource{Bucket: "site", Key: key}})
	}

	scope := app.WithRequestScope(ctx)
	for i := range 50 {
		if err := put(scope, fmt.Sprintf("reports/%02d.csv", i)); err != nil {
			t.Fatalf("a key of reports in one scope: %v", err)
		}
	}
	if reads := source.reads.Load(); reads != 1 {
		t.Errorf("one scope, fifty keys: the sub-buckets were read %d times, want once", reads)
	}
	parsed := authz.ParsedSubBucketPolicy("site", "reports")
	if parsed == nil {
		t.Fatal("the policy of reports is not remembered")
	}
	if err := put(app.WithRequestScope(ctx), "reports/again.csv"); err != nil {
		t.Fatal(err)
	}
	if again := authz.ParsedSubBucketPolicy("site", "reports"); again != parsed || source.reads.Load() != 2 {
		t.Errorf("a second request parsed the same document again (%p, %p) or did not read the sub-buckets (%d)", again, parsed, source.reads.Load())
	}

	// bob's Allow is taken away and a Deny put in its place.
	denied := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:PutObject","Resource":"arn:aws:s3:::site/reports/*"}]}`
	if err := meta.PutSubBucket(ctx, "site", bucket.SubBucket{Name: "reports", Policy: []byte(denied)}, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	if err := put(app.WithRequestScope(ctx), "reports/q1.csv"); !errors.Is(err, domain.ErrAccessDenied) {
		t.Fatalf("after the policy was changed to deny = %v", err)
	}
	if changed := authz.ParsedSubBucketPolicy("site", "reports"); changed == nil || changed == parsed {
		t.Errorf("the changed policy was not parsed anew: %p, before %p", changed, parsed)
	}
	// Without a scope it is read every time, and decides the same.
	if err := put(ctx, "reports/q1.csv"); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("without a scope = %v", err)
	}

	// The sub-bucket is deleted: its policy no longer decides, and is
	// forgotten when the sub-buckets are next read.
	if err := meta.DeleteSubBucket(ctx, "site", "reports", audit.Record{}); err != nil {
		t.Fatal(err)
	}
	if err := put(app.WithRequestScope(ctx), "reports/q1.csv"); !errors.Is(err, domain.ErrAccessDenied) {
		t.Fatalf("after the sub-bucket was deleted = %v", err)
	}
	if gone := authz.ParsedSubBucketPolicy("site", "reports"); gone != nil {
		t.Errorf("the policy of a deleted sub-bucket is still remembered")
	}
	allow := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/bob"},"Action":"s3:PutObject","Resource":"arn:aws:s3:::site/reports/*"}]}`
	if err := meta.PutSubBucket(ctx, "site", bucket.SubBucket{Name: "reports", Policy: []byte(allow)}, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	if err := put(app.WithRequestScope(ctx), "reports/q1.csv"); err != nil {
		t.Errorf("after the sub-bucket was registered again with an Allow = %v", err)
	}
	if authz.ParsedSubBucketPolicy("site", "reports") == nil {
		t.Fatal("the policy registered again is not remembered")
	}
	// A walk of the policies of all buckets, from the first to the last,
	// during which nobody asked about the sub-bucket: its policy is dropped
	// when the walk ends.
	walk := app.WithRequestScope(ctx)
	app.PrimeBucketPolicies(walk, meta)
	for _, name := range []string{"other", "site"} {
		if _, err := authz.Allowed(walk, iam.Request{Principal: bob, Action: iam.ActionListBucket, Resource: iam.Resource{Bucket: name}}); err != nil {
			t.Fatal(err)
		}
	}
	if authz.ParsedSubBucketPolicy("site", "reports") != nil {
		t.Error("a policy nobody asked about during a whole walk is still remembered")
	}
}

// TestSubBucketsThatCannotBeRead: when the policy and the sub-buckets of a
// bucket cannot be read - they are read together - every request about it
// that a policy binds is refused, in a sub-bucket or not: either policy might
// have denied it. It is refused with that error and no refusal. The root
// identity and whoever is bound by no policy are decided without reading.
func TestSubBucketsThatCannotBeRead(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	meta := subBucketSite(t)
	source := &countingReads{meta: meta, err: fmt.Errorf("%w: no leader", domain.ErrUnavailable)}
	authz := app.NewAuthorizer(slog.New(slog.DiscardHandler), porttest.NewClock(testNow), nil).WithBucketPolicies(meta).WithSubBuckets(source)
	dave := iam.Principal{AccessKeyID: "D", User: "dave", Policies: []iam.Document{{
		Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"s3:*"}, Resources: []string{"*"}}},
	}}}
	in := iam.Request{Principal: dave, Action: iam.ActionPutObject, Resource: iam.Resource{Bucket: "site", Key: "open/q1.csv"}}
	if allowed, err := authz.Allowed(ctx, in); allowed || !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("Allowed in a sub-bucket = %v, %v", allowed, err)
	}
	if err := authz.Authorize(app.WithRequestScope(ctx), in); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Authorize in a sub-bucket = %v", err)
	}
	for name, req := range map[string]iam.Request{
		"a key at the top":        {Principal: dave, Action: iam.ActionPutObject, Resource: iam.Resource{Bucket: "site", Key: "top.txt"}},
		"a key no name begins":    {Principal: dave, Action: iam.ActionPutObject, Resource: iam.Resource{Bucket: "site", Key: "Upper/top.txt"}},
		"a listing of the bucket": {Principal: dave, Action: iam.ActionListBucket, Resource: iam.Resource{Bucket: "site"}},
		"anonymous":               {Action: iam.ActionGetObject, Resource: iam.Resource{Bucket: "site", Key: "top.txt"}},
	} {
		if err := authz.Authorize(ctx, req); !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("%s = %v, want it refused for want of the policies", name, err)
		}
	}
	// In a walk of the policies of all buckets, those of buckets come from
	// elsewhere; the sub-buckets, which the search did not tell, cannot be
	// read, and a request in one is refused all the same.
	walk := app.WithRequestScope(ctx)
	app.PrimeBucketPolicies(walk, meta)
	if allowed, err := authz.Allowed(walk, in); allowed || !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Allowed in a sub-bucket, in a walk = %v, %v", allowed, err)
	}
	reads := source.reads.Load()
	top := iam.Request{Principal: dave, Action: iam.ActionPutObject, Resource: iam.Resource{Bucket: "site", Key: "top.txt"}}
	if allowed, err := authz.Allowed(walk, top); !allowed || err != nil {
		t.Errorf("Allowed at the top, in a walk = %v, %v; want it decided without reading", allowed, err)
	}
	for name, req := range map[string]iam.Request{
		"root":   {Principal: porttest.Root(), Action: iam.ActionPutObject, Resource: iam.Resource{Bucket: "site", Key: "open/q1.csv"}},
		"a node": {Principal: iam.Principal{NodeID: "n1"}, Action: iam.ActionNodeRPC},
	} {
		// A node asking what is no node's is refused, but not for want of
		// the policies.
		if err := authz.Authorize(ctx, req); errors.Is(err, domain.ErrUnavailable) || (err != nil && name != "a node") {
			t.Errorf("%s = %v", name, err)
		}
	}
	if more := source.reads.Load() - reads; more != 0 {
		t.Errorf("requests no policy binds read the store %d times", more)
	}
}

// TestOneReadPerScopeAndBucket: the authorizer reads the policy of a bucket
// and its sub-buckets in one consistent read, once per request scope and
// bucket - whether the bucket has sub-buckets or not, whether the request is
// in one or not, however many keys of how many sub-buckets a request decides,
// and for a caller who has not authenticated as for one who has. A decision
// outside a request scope is a scope of its own: one read. Nothing reads the
// policy of a bucket alone once the authorizer has the reader of both.
func TestOneReadPerScopeAndBucket(t *testing.T) {
	t.Parallel()
	bob := iam.Principal{AccessKeyID: "B", User: "bob"}
	anonymous := iam.Principal{}
	get := func(who iam.Principal, bucketName, key string) iam.Request {
		return iam.Request{Principal: who, Action: iam.ActionGetObject, Resource: iam.Resource{Bucket: bucketName, Key: key}}
	}
	tests := map[string]struct {
		// scopes are the requests of each request scope; a scope of nil
		// is a request decided without one.
		scopes [][]iam.Request
		// alone is decided without a request scope, each.
		alone []iam.Request
		// walk primes each scope with a walk of the policies of all
		// buckets, as a search does: the policies of buckets are then
		// read from this node's state, and only the sub-buckets that
		// the search did not tell the scope are read consistently.
		walk bool
		want int64
	}{
		"a key at the top of a bucket with sub-buckets": {scopes: [][]iam.Request{{get(bob, "site", "top.txt")}}, want: 1},
		"a key in a sub-bucket":                         {scopes: [][]iam.Request{{get(bob, "site", "reports/q1.csv")}}, want: 1},
		"a bucket without sub-buckets":                  {scopes: [][]iam.Request{{get(bob, "other", "reports/q1.csv")}}, want: 1},
		"anonymous, in a sub-bucket":                    {scopes: [][]iam.Request{{get(anonymous, "site", "private/a"), get(anonymous, "site", "private/b")}}, want: 1},
		"anonymous, refused by the sub-bucket, again and again": {
			alone: []iam.Request{get(anonymous, "site", "private/a"), get(anonymous, "site", "private/b"), get(anonymous, "site", "private/c")}, want: 3,
		},
		"keys of every sub-bucket and of none, in one scope": {scopes: [][]iam.Request{{
			get(bob, "site", "top.txt"), get(bob, "site", "reports/a"), get(bob, "site", "private/a"), get(bob, "site", "open/a"),
			get(bob, "site", "reports-x/a"), get(bob, "site", "broken/a"), get(bob, "site", "reports/b"),
		}}, want: 1},
		"two buckets in one scope": {scopes: [][]iam.Request{{
			get(bob, "site", "reports/a"), get(bob, "other", "reports/a"), get(bob, "site", "top.txt"), get(bob, "other", "top.txt"),
		}}, want: 2},
		"a bucket that is not there": {scopes: [][]iam.Request{{get(bob, "nowhere", "reports/a"), get(bob, "nowhere", "a")}}, want: 1},
		"two scopes":                 {scopes: [][]iam.Request{{get(bob, "site", "reports/a")}, {get(bob, "site", "reports/a")}}, want: 2},
		"root reads nothing":         {scopes: [][]iam.Request{{get(porttest.Root(), "site", "reports/a")}}, alone: []iam.Request{get(porttest.Root(), "site", "private/a")}},
		"in a walk, keys in no sub-bucket": {
			walk: true, scopes: [][]iam.Request{{get(bob, "site", "top.txt"), get(bob, "other", "top.txt")}},
		},
		"in a walk, keys in sub-buckets": {
			walk: true, scopes: [][]iam.Request{{get(anonymous, "site", "private/a"), get(bob, "site", "reports/a"), get(bob, "site", "open/a")}}, want: 1,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			meta := subBucketSite(t)
			source := &countingReads{meta: meta}
			authz := app.NewAuthorizer(slog.New(slog.DiscardHandler), porttest.NewClock(testNow), nil).WithBucketPolicies(source).WithSubBuckets(source)
			decide := func(ctx context.Context, req iam.Request) {
				// Allowed and refused alike; what is counted is the
				// reading. Only the damaged policy of "broken" says that
				// no decision could be made.
				if _, err := authz.Allowed(ctx, req); err != nil && !errors.Is(err, domain.ErrIntegrity) {
					t.Fatalf("Allowed(%+v) = %v", req.Resource, err)
				}
			}
			for _, scope := range tt.scopes {
				scoped := app.WithRequestScope(ctx)
				if tt.walk {
					app.PrimeBucketPolicies(scoped, meta)
				}
				for _, req := range scope {
					decide(scoped, req)
				}
			}
			for _, req := range tt.alone {
				decide(ctx, req)
			}
			if got := source.reads.Load(); got != tt.want {
				t.Errorf("the store was read %d times, want %d", got, tt.want)
			}
		})
	}
}

// TestSubBucketPolicyFindsNoRoom: when the authorizer's memory of parsed
// policies is full - here with the policy of a bucket - the policy of a
// sub-bucket is not remembered, and still decides: its Deny refuses, and
// its Allow allows, parsed anew each time.
func TestSubBucketPolicyFindsNoRoom(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	meta := subBucketSite(t)
	trail, clock := porttest.NewMemIAM(), porttest.NewClock(testNow)
	logger := slog.New(slog.DiscardHandler)
	auditor := app.NewAuditor(trail, clock, logger)
	authz := app.NewAuthorizer(logger, clock, auditor).WithBucketPolicies(meta).WithSubBuckets(meta).WithParsedPolicyBounds(1, 1<<20)
	// A walk of the policies of all buckets remembers that of "site", and
	// with it the one entry there is room for.
	walk := app.WithRequestScope(ctx)
	app.PrimeBucketPolicies(walk, meta)
	if _, err := authz.Allowed(walk, iam.Request{Principal: iam.Principal{AccessKeyID: "B", User: "bob"}, Action: iam.ActionListBucket, Resource: iam.Resource{Bucket: "site"}}); err != nil {
		t.Fatal(err)
	}
	if authz.ParsedPolicy("site") == nil {
		t.Fatal("the policy of the bucket is not remembered")
	}
	for range 2 {
		err := authz.Authorize(app.WithRequestScope(ctx), iam.Request{Action: iam.ActionGetObject, Resource: iam.Resource{Bucket: "site", Key: "private/salaries.csv"}})
		if !errors.Is(err, domain.ErrAccessDenied) {
			t.Fatalf("anonymous in the private sub-bucket = %v, want it refused by its Deny", err)
		}
		bob := iam.Principal{AccessKeyID: "B", User: "bob"}
		if err := authz.Authorize(app.WithRequestScope(ctx), iam.Request{Principal: bob, Action: iam.ActionPutObject, Resource: iam.Resource{Bucket: "site", Key: "reports/q1.csv"}}); err != nil {
			t.Fatalf("bob in reports = %v, want it allowed by its Allow", err)
		}
	}
	if authz.ParsedSubBucketPolicy("site", "private") != nil || authz.ParsedSubBucketPolicy("site", "reports") != nil {
		t.Error("a sub-bucket policy is remembered in a memory that is full")
	}
	auditor.Flush(ctx)
	records, err := trail.ListAudit(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	// The auditor counts the repeat of the same refusal in a second record.
	if len(records) == 0 || records[0].Details["source"] != string(iam.SourceSubBucket) {
		t.Errorf("the refusals = %+v, want them by the sub-bucket's policy", records)
	}
	for _, record := range records {
		if record.Actor != "anonymous" || record.Action != string(iam.ActionGetObject) {
			t.Errorf("a refusal of what was allowed: %+v", record)
		}
	}
}

// TestDamagedBucketPolicyWithSubBuckets: the policy of a bucket read together
// with its sub-buckets is a policy like any other: one that does not parse
// refuses every request a policy binds - in a sub-bucket or not - with
// domain.ErrIntegrity and no refusal, and is logged without what it says.
func TestDamagedBucketPolicyWithSubBuckets(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	meta := subBucketSite(t)
	if err := meta.SetBucketConfig(ctx, "site", bucket.ConfigPolicy, []byte(`{"Version":"damaged-sentinel`)); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	authz := app.NewAuthorizer(slog.New(slog.NewJSONHandler(logs, nil)), porttest.NewClock(testNow), nil).WithBucketPolicies(meta).WithSubBuckets(meta)
	bob := iam.Principal{AccessKeyID: "B", User: "bob"}
	for _, key := range []string{"reports/q1.csv", "top.txt"} {
		req := iam.Request{Principal: bob, Action: iam.ActionPutObject, Resource: iam.Resource{Bucket: "site", Key: key}}
		if err := authz.Authorize(app.WithRequestScope(ctx), req); !errors.Is(err, domain.ErrIntegrity) || errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("%s = %v, want %v", key, err, domain.ErrIntegrity)
		}
	}
	if !strings.Contains(logs.String(), "the policy of a bucket cannot be read") || strings.Contains(logs.String(), "damaged-sentinel") {
		t.Errorf("the log: %s", logs.String())
	}
}
