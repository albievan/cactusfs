package app_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
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
