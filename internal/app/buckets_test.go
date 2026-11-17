package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

func TestBucketService(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := porttest.Root()
	boom := errors.New("raft unavailable")

	tests := []struct {
		name    string
		setup   func(t *testing.T, f *fixture)
		run     func(f *fixture) error
		wantErr error
		check   func(t *testing.T, f *fixture)
	}{
		{
			name: "create",
			run:  func(f *fixture) error { return f.buckets.Create(ctx, root, "photos") },
			check: func(t *testing.T, f *fixture) {
				b, err := f.meta.GetBucket(ctx, "photos")
				if err != nil || !b.CreatedAt.Equal(testNow) {
					t.Errorf("stored bucket = %+v, %v; want it created at the clock's time", b, err)
				}
			},
		},
		{
			name:    "create with an invalid name",
			run:     func(f *fixture) error { return f.buckets.Create(ctx, root, "Not_Valid") },
			wantErr: domain.ErrInvalidBucketName,
			check: func(t *testing.T, f *fixture) {
				if list, _ := f.meta.ListBuckets(ctx); len(list) != 0 {
					t.Errorf("an invalid bucket was stored: %+v", list)
				}
			},
		},
		{
			name:    "create a duplicate",
			setup:   func(t *testing.T, f *fixture) { f.createBucket(t, "photos") },
			run:     func(f *fixture) error { return f.buckets.Create(ctx, root, "photos") },
			wantErr: domain.ErrBucketAlreadyExists,
		},
		{
			name:    "create when the commit fails",
			setup:   func(_ *testing.T, f *fixture) { f.meta.FailWrites = boom },
			run:     func(f *fixture) error { return f.buckets.Create(ctx, root, "photos") },
			wantErr: boom,
		},
		{
			name:  "delete",
			setup: func(t *testing.T, f *fixture) { f.createBucket(t, "photos") },
			run:   func(f *fixture) error { return f.buckets.Delete(ctx, root, "photos") },
			check: func(t *testing.T, f *fixture) {
				if _, err := f.meta.GetBucket(ctx, "photos"); !errors.Is(err, domain.ErrBucketNotFound) {
					t.Errorf("bucket still exists: %v", err)
				}
			},
		},
		{
			name:    "delete a missing bucket",
			run:     func(f *fixture) error { return f.buckets.Delete(ctx, root, "photos") },
			wantErr: domain.ErrBucketNotFound,
		},
		{
			name: "delete a bucket that holds objects",
			setup: func(t *testing.T, f *fixture) {
				f.createBucket(t, "photos")
				f.put(t, "photos", "a.jpg", []byte("x"))
			},
			run:     func(f *fixture) error { return f.buckets.Delete(ctx, root, "photos") },
			wantErr: domain.ErrBucketNotEmpty,
		},
		{
			name:  "get",
			setup: func(t *testing.T, f *fixture) { f.createBucket(t, "photos") },
			run: func(f *fixture) error {
				b, err := f.buckets.Get(ctx, root, "photos")
				if err == nil && b.Name != "photos" {
					return errors.New("wrong bucket returned")
				}
				return err
			},
		},
		{
			name:    "get a missing bucket",
			run:     func(f *fixture) error { _, err := f.buckets.Get(ctx, root, "photos"); return err },
			wantErr: domain.ErrBucketNotFound,
		},
		{
			name:  "location",
			setup: func(t *testing.T, f *fixture) { f.createBucket(t, "photos") },
			run:   func(f *fixture) error { return f.buckets.Location(ctx, root, "photos") },
		},
		{
			name:    "location of a missing bucket",
			run:     func(f *fixture) error { return f.buckets.Location(ctx, root, "photos") },
			wantErr: domain.ErrBucketNotFound,
		},
		{
			name: "list in name order",
			setup: func(t *testing.T, f *fixture) {
				f.createBucket(t, "zebra")
				f.createBucket(t, "alpha")
			},
			run: func(f *fixture) error {
				list, err := f.buckets.List(ctx, root)
				if err == nil && (len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "zebra") {
					return errors.New("wrong listing")
				}
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			err := tt.run(f)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil && !strings.HasPrefix(err.Error(), "bucket.") {
				t.Errorf("error %q is not wrapped with the use case name", err)
			}
			if tt.check != nil {
				tt.check(t, f)
			}
		})
	}
}

// TestBucketServiceAuthorizesEveryCall proves that no bucket use case reaches
// the store for a principal the authorizer refuses, and that each asks about
// the right action and resource.
func TestBucketServiceAuthorizesEveryCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	calls := map[iam.Action]func(f *fixture, p iam.Principal) error{
		iam.ActionCreateBucket:      func(f *fixture, p iam.Principal) error { return f.buckets.Create(ctx, p, "photos") },
		iam.ActionDeleteBucket:      func(f *fixture, p iam.Principal) error { return f.buckets.Delete(ctx, p, "photos") },
		iam.ActionListBucket:        func(f *fixture, p iam.Principal) error { _, err := f.buckets.Get(ctx, p, "photos"); return err },
		iam.ActionGetBucketLocation: func(f *fixture, p iam.Principal) error { return f.buckets.Location(ctx, p, "photos") },
		iam.ActionListAllMyBuckets:  func(f *fixture, p iam.Principal) error { _, err := f.buckets.List(ctx, p); return err },
	}
	for action, call := range calls {
		t.Run(string(action), func(t *testing.T) {
			t.Parallel()
			for _, principal := range []iam.Principal{anonymous(), {AccessKeyID: "NOT-ROOT"}} {
				f := newFixture(t)
				f.createBucket(t, "existing")
				// Any store access would fail loudly.
				f.meta.FailWrites = errors.New("the store was reached")

				if err := call(f, principal); !errors.Is(err, domain.ErrAccessDenied) {
					t.Fatalf("%+v: error = %v, want %v", principal, err, domain.ErrAccessDenied)
				}
				requests := f.authz.Requests()
				if len(requests) != 1 || requests[0].Action != action || !reflect.DeepEqual(requests[0].Principal, principal) {
					t.Fatalf("authorizer saw %+v, want one request for %s", requests, action)
				}
				wantBucket := "photos"
				if action == iam.ActionListAllMyBuckets {
					wantBucket = ""
				}
				if requests[0].Resource.Bucket != wantBucket {
					t.Errorf("resource = %+v, want bucket %q", requests[0].Resource, wantBucket)
				}
			}
		})
	}
}

func TestAuthorizerLogsRefusalsWithoutSecrets(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	authz := app.NewAuthorizer(slog.New(slog.NewJSONHandler(&logs, nil)), porttest.NewClock(testNow), nil)
	resource := iam.Resource{Bucket: "photos", Key: "a.jpg"}

	if err := authz.Authorize(context.Background(), iam.Request{Principal: porttest.Root(), Action: iam.ActionGetObject, Resource: resource}); err != nil {
		t.Fatalf("root was refused: %v", err)
	}
	if logs.Len() != 0 {
		t.Errorf("an allowed request was logged as a refusal: %s", logs.String())
	}

	err := authz.Authorize(context.Background(), iam.Request{Principal: iam.Principal{AccessKeyID: "SOMEONE"}, Action: iam.ActionGetObject, Resource: resource})
	if !errors.Is(err, domain.ErrAccessDenied) {
		t.Fatalf("error = %v, want %v", err, domain.ErrAccessDenied)
	}
	for _, want := range []string{`"principal":"SOMEONE"`, `"action":"s3:GetObject"`, `"resource":"arn:aws:s3:::photos/a.jpg"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("refusal log lacks %s: %s", want, logs.String())
		}
	}
}

// failingPolicies cannot read the policy of any bucket.
type failingPolicies struct{}

func (failingPolicies) GetBucketConfig(context.Context, string, string) ([]byte, error) {
	return nil, domain.ErrUnavailable
}

// brokenPolicies hold a policy that does not parse.
type brokenPolicies struct{}

func (brokenPolicies) GetBucketConfig(context.Context, string, string) ([]byte, error) {
	return []byte("not a policy"), nil
}

// TestAuthorizerAndBucketPolicies: the authorizer takes the policy of the
// bucket into its decision, and refuses when it cannot read it - the policy
// might have denied the request.
func TestAuthorizerAndBucketPolicies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	meta := porttest.NewMemMetadata()
	clock := porttest.NewClock(testNow)
	if err := meta.CreateBucket(ctx, bucket.Bucket{Name: "site"}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	authz := app.NewAuthorizer(logger, clock, nil).WithBucketPolicies(meta)
	buckets := app.NewBucketService(meta, authz, clock)
	policy := []byte(`{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"},
		{"Effect":"Deny","Principal":"*","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::site/*"}]}`)
	read := iam.Request{Action: iam.ActionGetObject, Resource: iam.Resource{Bucket: "site", Key: "index.html"}}
	if err := authz.Authorize(ctx, read); !errors.Is(err, domain.ErrAccessDenied) {
		t.Fatalf("anonymous read before there is a policy = %v", err)
	}
	if err := buckets.SetPolicy(ctx, anonymous(), "site", policy); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("SetPolicy by nobody = %v", err)
	}
	if err := buckets.SetPolicy(ctx, porttest.Root(), "site", []byte(`{"Version":"2012-10-17"}`)); !errors.Is(err, domain.ErrInvalidArgument) || !errors.Is(err, domain.ErrInvalidPolicy) {
		t.Errorf("SetPolicy with a document that is none = %v", err)
	}
	if err := buckets.SetPolicy(ctx, porttest.Root(), "site", policy); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if got, err := buckets.Policy(ctx, porttest.Root(), "site"); err != nil || string(got) != string(policy) {
		t.Errorf("Policy = %s, %v", got, err)
	}
	if err := authz.Authorize(ctx, read); err != nil {
		t.Errorf("anonymous read of what the policy opens: %v", err)
	}
	write := read
	write.Action = iam.ActionPutObject
	if err := authz.Authorize(ctx, write); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("anonymous write = %v", err)
	}
	// A user their own policy allows everything is still denied what the
	// bucket denies; root is not.
	all := iam.Document{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"s3:*"}, Resources: []string{"*"}}}}
	user := iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{all}}
	remove := iam.Request{Principal: user, Action: iam.ActionDeleteObject, Resource: read.Resource}
	if err := authz.Authorize(ctx, remove); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("a delete the bucket denies, by a user who may otherwise = %v", err)
	}
	remove.Principal = porttest.Root()
	if err := authz.Authorize(ctx, remove); err != nil {
		t.Errorf("root, bound by a bucket policy: %v", err)
	}
	// What is not about a bucket asks for no policy.
	if err := authz.Authorize(ctx, iam.Request{Principal: user, Action: iam.ActionListAllMyBuckets}); err != nil {
		t.Errorf("ListAllMyBuckets: %v", err)
	}

	// A policy that cannot be read, or cannot be understood, refuses
	// everybody it could have bound - and nobody it could not.
	for name, source := range map[string]app.BucketPolicySource{"unavailable": failingPolicies{}, "damaged": brokenPolicies{}} {
		closed := app.NewAuthorizer(logger, clock, nil).WithBucketPolicies(source)
		request := iam.Request{Principal: user, Action: iam.ActionGetObject, Resource: read.Resource}
		if err := closed.Authorize(ctx, request); err == nil {
			t.Errorf("%s policy: a user was allowed", name)
		}
		if err := closed.Authorize(ctx, read); err == nil {
			t.Errorf("%s policy: an anonymous caller was allowed", name)
		}
		request.Principal = porttest.Root()
		if err := closed.Authorize(ctx, request); err != nil {
			t.Errorf("%s policy: root was refused: %v", name, err)
		}
	}

	// CORS rules and tags of the bucket.
	rules := &bucket.CORS{Rules: []bucket.CORSRule{{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}}}}
	if err := buckets.SetCORS(ctx, user, "site", rules); err != nil {
		t.Errorf("SetCORS by a user with s3:* = %v", err)
	}
	if err := buckets.SetCORS(ctx, user, "site", nil); err != nil {
		t.Errorf("removing the rules by a user with s3:* = %v", err)
	}
	if err := buckets.SetCORS(ctx, porttest.Root(), "site", &bucket.CORS{}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("SetCORS with no rules = %v", err)
	}
	if err := buckets.SetCORS(ctx, porttest.Root(), "site", rules); err != nil {
		t.Fatal(err)
	}
	if got, err := buckets.CORS(ctx, porttest.Root(), "site"); err != nil || got == nil || len(got.Rules) != 1 {
		t.Errorf("CORS = %+v, %v", got, err)
	}
	if got := buckets.CORSOf(ctx, "site"); got == nil || len(got.Rules) != 1 {
		t.Errorf("CORSOf = %+v", got)
	}
	if buckets.CORSOf(ctx, "missing") != nil {
		t.Error("CORSOf of a missing bucket")
	}
	if _, err := buckets.CORS(ctx, anonymous(), "site"); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("CORS by nobody = %v", err)
	}
	if err := buckets.SetCORS(ctx, porttest.Root(), "site", nil); err != nil || buckets.CORSOf(ctx, "site") != nil {
		t.Errorf("removing the rules: %v", err)
	}
	if got, err := buckets.CORS(ctx, porttest.Root(), "site"); err != nil || got != nil {
		t.Errorf("CORS after removal = %+v, %v", got, err)
	}
	if _, err := buckets.SetTags(ctx, porttest.Root(), "site", []object.Tag{{Key: "team", Value: "web"}}); err != nil {
		t.Fatal(err)
	}
	if got, err := buckets.Tags(ctx, porttest.Root(), "site"); err != nil || got["team"] != "web" {
		t.Errorf("Tags = %v, %v", got, err)
	}
	if _, err := buckets.SetTags(ctx, porttest.Root(), "site", []object.Tag{{Value: "x"}}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("SetTags with an empty key = %v", err)
	}
	if _, err := buckets.SetTags(ctx, anonymous(), "site", nil); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("SetTags by nobody = %v", err)
	}
	if _, err := buckets.SetTags(ctx, porttest.Root(), "site", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := buckets.Tags(ctx, porttest.Root(), "site"); err != nil || got != nil {
		t.Errorf("Tags after removal = %v, %v", got, err)
	}
	if err := buckets.SetPolicy(ctx, porttest.Root(), "site", nil); err != nil {
		t.Fatal(err)
	}
	if err := authz.Authorize(ctx, read); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("anonymous read after the policy was removed = %v", err)
	}
}

// countingActivity counts the questions about the latest use of buckets, and
// can fail or answer short.
type countingActivity struct {
	next ports.BucketActivityStore
	fail error
	// short makes it leave the last record out.
	short bool

	mu    sync.Mutex
	calls [][]string
}

func (c *countingActivity) BucketActivities(ctx context.Context, names []string) ([]bucket.Activity, error) {
	c.mu.Lock()
	c.calls = append(c.calls, slices.Clone(names))
	c.mu.Unlock()
	if c.fail != nil {
		return nil, c.fail
	}
	records, err := c.next.BucketActivities(ctx, names)
	if err == nil && c.short && len(records) > 0 {
		records = records[:len(records)-1]
	}
	return records, err
}

func (c *countingActivity) asked() [][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

// detailsFixture is a BucketService that lists with details, behind the
// authorizer a node has: one that reads bucket policies and audits.
type detailsFixture struct {
	*searchFixture
	activity *countingActivity
	buckets  *app.BucketService
}

func newDetailsFixture(ctx context.Context, t *testing.T) *detailsFixture {
	t.Helper()
	f := &detailsFixture{searchFixture: newSearchFixture(ctx, t, nil, nil)}
	f.base.meta.Now = f.base.clock.Now
	f.activity = &countingActivity{next: f.base.meta}
	f.buckets = app.NewBucketService(f.base.meta, f.authz, f.base.clock).WithDetails(f.policies, f.activity)
	return f
}

// The policies of the buckets of the details tests: beta lets anybody read
// its objects; delta names users only - viewer may read its policy, and
// scoped may not list it whatever a policy of their own says.
const (
	betaPolicy = `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::beta/*"},
		{"Effect":"Deny","Principal":"*","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::beta/*"}]}`
	deltaPolicy = `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/viewer"},"Action":"s3:GetBucketPolicy","Resource":"arn:aws:s3:::delta"},
		{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam:::user/viewer"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::delta/*"},
		{"Effect":"Deny","Principal":{"AWS":"arn:aws:iam:::user/scoped"},"Action":"s3:ListBucket","Resource":"arn:aws:s3:::delta"}]}`
)

// detailsWorld is what the buckets of the details tests are, told to a
// caller who may see all of it.
type detailsWorld struct {
	creators map[string]string
	activity map[string]bucket.Activity
	policies map[string]*iam.BucketPolicySummary
}

// roleUser is a user who holds one built-in role.
func roleUser(name, role string) iam.Principal {
	return iam.Principal{AccessKeyID: "K" + name, User: name, Policies: []iam.Document{iam.BuiltInPolicies()[role]}}
}

// populate makes four buckets, each another way, and uses two of them -
// alpha has three uploads, under two prefixes, and a download:
// alpha by a user through Create, beta by root through CreateWithObjectLock,
// gamma by another user, and delta as a bucket of before creators were kept.
func (f *detailsFixture) populate(ctx context.Context, t *testing.T) detailsWorld {
	t.Helper()
	root, alice, bob := porttest.Root(), roleUser("alice", iam.RoleClusterAdmin), roleUser("bob", iam.RoleBucketAdmin)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.buckets.Create(ctx, alice, "alpha"))
	must(f.buckets.CreateWithObjectLock(ctx, root, "beta"))
	must(f.buckets.Create(ctx, bob, "gamma"))
	must(f.base.meta.CreateBucket(ctx, bucket.Bucket{Name: "delta", CreatedAt: testNow}))
	must(f.buckets.SetPolicy(ctx, root, "beta", []byte(betaPolicy)))
	must(f.buckets.SetPolicy(ctx, root, "delta", []byte(deltaPolicy)))

	put := func(p iam.Principal, bucketName, name string) (object.Version, time.Time) {
		t.Helper()
		f.base.clock.Advance(time.Minute)
		v, err := f.base.objects.Put(ctx, p, app.PutInput{Bucket: bucketName, Key: name, Body: bytes.NewReader([]byte("data")), Size: 4})
		must(err)
		return v, f.base.clock.Now()
	}
	one, firstAt := put(alice, "alpha", "a/one.txt")
	_, secondAt := put(root, "alpha", "a/two.txt")
	_, laterAt := put(bob, "alpha", "b/three.txt")
	_, thirdAt := put(alice, "beta", "b/x.bin")
	f.base.clock.Advance(time.Minute)
	readAt := f.base.clock.Now()
	must(f.base.meta.AddObjectReads(ctx, ports.ReadBatch{Node: "node-a", Boot: 1, Seq: 1, Items: []ports.ObjectRead{{
		Bucket: "alpha", Key: one.Key, Version: one.Ref(), VersionID: one.VersionID,
		Read: object.Read{Who: "user/bob", Presigned: true, Downloads: 2},
	}}}))
	return detailsWorld{
		creators: map[string]string{"alpha": "user/alice", "beta": "root", "gamma": "user/bob", "delta": ""},
		activity: map[string]bucket.Activity{
			"alpha": {
				Uploads: []bucket.RecentUpload{
					{At: laterAt, By: "user/bob", Key: "b/three.txt"},
					{At: secondAt, By: "root", Key: "a/two.txt"},
					{At: firstAt, By: "user/alice", Key: "a/one.txt"},
				},
				Downloads: []bucket.RecentDownload{{At: readAt, By: "user/bob", Presigned: true, Key: "a/one.txt"}},
			},
			"beta": {Uploads: []bucket.RecentUpload{{At: thirdAt, By: "user/alice", Key: "b/x.bin"}}},
		},
		policies: map[string]*iam.BucketPolicySummary{
			"beta":  {Statements: 2, OpenToAnyone: true},
			"delta": {Statements: 3, OpenToAnyone: false},
		},
	}
}

// without returns the world as a caller is to be shown it from whom the rows
// of some objects are kept: hidden names, by bucket, the keys of those
// objects. The rows that stay keep their order.
func (w detailsWorld) without(hidden map[string][]string) detailsWorld {
	out := detailsWorld{creators: w.creators, policies: w.policies, activity: map[string]bucket.Activity{}}
	for name, a := range w.activity {
		a = a.Clone()
		a.Uploads = slices.DeleteFunc(a.Uploads, func(u bucket.RecentUpload) bool { return slices.Contains(hidden[name], u.Key) })
		a.Downloads = slices.DeleteFunc(a.Downloads, func(d bucket.RecentDownload) bool { return slices.Contains(hidden[name], d.Key) })
		out.activity[name] = a
	}
	return out
}

// part is what a caller is shown of one bucket.
type part struct{ access, keys, policy bool }

// withoutKeys is a record as who may not list the bucket is shown it.
func withoutKeys(a bucket.Activity) bucket.Activity {
	out := a.Clone()
	for i := range out.Uploads {
		out.Uploads[i].Key = ""
	}
	for i := range out.Downloads {
		out.Downloads[i].Key = ""
	}
	return out
}

// sameActivity compares two records row by row; times by the instant.
func sameActivity(a, b bucket.Activity) bool {
	return slices.EqualFunc(a.Uploads, b.Uploads, func(x, y bucket.RecentUpload) bool {
		return x.At.Equal(y.At) && x.By == y.By && x.Key == y.Key
	}) && slices.EqualFunc(a.Downloads, b.Downloads, func(x, y bucket.RecentDownload) bool {
		return x.At.Equal(y.At) && x.By == y.By && x.Key == y.Key && x.Presigned == y.Presigned
	})
}

// checkDetails holds a list against what the caller is to be shown of each
// bucket; a bucket that want does not name is shown as List shows it.
func checkDetails(t *testing.T, world detailsWorld, got []app.BucketDetail, want map[string]part) {
	t.Helper()
	names := make([]string, len(got))
	for i, d := range got {
		names[i] = d.Bucket.Name
	}
	if !slices.Equal(names, []string{"alpha", "beta", "delta", "gamma"}) {
		t.Fatalf("the list is of %v", names)
	}
	for _, d := range got {
		name, w := d.Bucket.Name, want[d.Bucket.Name]
		if (part{d.AccessIncluded, d.KeysIncluded, d.PolicyIncluded}) != w {
			t.Errorf("%s: access, keys, policy = %v, %v, %v; want %+v", name, d.AccessIncluded, d.KeysIncluded, d.PolicyIncluded, w)
			continue
		}
		creator, activity := "", bucket.Activity{}
		if w.access {
			creator, activity = world.creators[name], world.activity[name]
			if !w.keys {
				activity = withoutKeys(activity)
			}
		}
		if d.Bucket.CreatedBy != creator {
			t.Errorf("%s: created by %q, want %q", name, d.Bucket.CreatedBy, creator)
		}
		if !sameActivity(d.Activity, activity) {
			t.Errorf("%s: activity = %+v, want %+v", name, d.Activity, activity)
		}
		var policy *iam.BucketPolicySummary
		if w.policy {
			policy = world.policies[name]
		}
		if (d.Policy == nil) != (policy == nil) || (policy != nil && *d.Policy != *policy) {
			t.Errorf("%s: policy = %+v, want %+v", name, d.Policy, policy)
		}
		if d.Bucket.CreatedAt.IsZero() {
			t.Errorf("%s: no time of creation", name)
		}
	}
}

// everywhere is the same part of every bucket of the details tests.
func everywhere(p part) map[string]part {
	return map[string]part{"alpha": p, "beta": p, "delta": p, "gamma": p}
}

// TestBucketCreatorIsRecorded: both ways of making a bucket record who made
// it, as the audit trail names actors; a bucket of before has none.
func TestBucketCreatorIsRecorded(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newDetailsFixture(ctx, t)
	world := f.populate(ctx, t)
	for name, creator := range world.creators {
		stored, err := f.base.meta.GetBucket(ctx, name)
		if err != nil || stored.CreatedBy != creator {
			t.Errorf("%s: stored as created by %q, %v; want %q", name, stored.CreatedBy, err, creator)
		}
		if stored.ObjectLock != (name == "beta") {
			t.Errorf("%s: object lock = %v", name, stored.ObjectLock)
		}
	}
	// A bucket that could not be made records nobody and is not there.
	if err := f.buckets.Create(ctx, stranger(), "refused"); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("Create by who may not = %v", err)
	}
	if _, err := f.base.meta.GetBucket(ctx, "refused"); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("a refused bucket: %v", err)
	}
}

// TestListDetailed: the list is the list List gives, and each bucket carries
// what the caller may see of it - decided bucket by bucket, by the caller's
// own policies and by the bucket's, and row by row of the activity by what
// the caller may see of the object the row names - at the cost of one walk of
// the policies and one reading of the records, with nothing audited for what
// is left out.
func TestListDetailed(t *testing.T) {
	t.Parallel()
	statement := func(effect iam.Effect, action string, resources ...string) iam.Statement {
		return iam.Statement{Effect: effect, Actions: []string{action}, Resources: resources}
	}
	with := func(name string, statements ...iam.Statement) iam.Principal {
		return iam.Principal{AccessKeyID: "K" + name, User: name, Policies: []iam.Document{{Version: iam.PolicyVersion, Statements: statements}}}
	}
	listAll := statement(iam.Allow, "s3:ListAllMyBuckets", "*")
	access := string(iam.ActionGetObjectAccess)
	all := part{access: true, keys: true, policy: true}

	tests := []struct {
		name      string
		principal iam.Principal
		wantErr   error
		want      map[string]part
		// hidden names, by bucket, the objects whose rows of the activity
		// are left out for this caller.
		hidden map[string][]string
	}{
		{name: "root", principal: porttest.Root(), want: everywhere(all)},
		{name: "a cluster admin", principal: roleUser("carol", iam.RoleClusterAdmin), want: everywhere(all)},
		// All of S3 and nothing of administration: the policy, not who
		// uses the bucket.
		{name: "a bucket admin", principal: roleUser("dave", iam.RoleBucketAdmin), want: everywhere(part{policy: true})},
		{name: "a read-write user", principal: roleUser("erin", iam.RoleReadWrite), want: map[string]part{}},
		{name: "a read-only user", principal: roleUser("frank", iam.RoleReadOnly), want: map[string]part{}},
		{name: "who may list the buckets and no more", principal: lister(), want: map[string]part{}},
		{
			name:      "who may see who uses a bucket and may list none",
			principal: with("grace", listAll, statement(iam.Allow, access, "*")),
			want:      everywhere(part{access: true}),
		},
		{
			name:      "who may read the policies only",
			principal: with("heidi", listAll, statement(iam.Allow, "s3:GetBucketPolicy", "*")),
			want:      everywhere(part{policy: true}),
		},
		// The action on a bucket shows who created it; a row of its
		// activity is shown where the action is held on the object as well:
		// here on those under b/ and on no other.
		{
			name: "a policy of their own that names buckets",
			principal: with("ivan", listAll,
				statement(iam.Allow, access, "arn:aws:s3:::alpha", "arn:aws:s3:::alpha/b/*"), statement(iam.Allow, "s3:ListBucket", "arn:aws:s3:::alpha"),
				statement(iam.Allow, access, "arn:aws:s3:::gamma"), statement(iam.Allow, "s3:GetBucketPolicy", "arn:aws:s3:::beta")),
			want:   map[string]part{"alpha": {access: true, keys: true}, "beta": {policy: true}, "gamma": {access: true}},
			hidden: map[string][]string{"alpha": {"a/one.txt", "a/two.txt"}},
		},
		{
			name: "a policy of their own that denies one bucket",
			principal: with("judy", statement(iam.Allow, "s3:*", "*"), statement(iam.Allow, "admin:*", "*"),
				statement(iam.Deny, access, "arn:aws:s3:::beta"), statement(iam.Deny, "s3:GetBucketPolicy", "arn:aws:s3:::alpha")),
			want: map[string]part{"alpha": {access: true, keys: true}, "beta": {policy: true}, "delta": all, "gamma": all},
		},
		// The bucket's own policy has its say: delta denies scoped the
		// listing, so its keys are withheld and the rest is shown.
		{
			name:      "whom the policy of a bucket denies its listing",
			principal: roleUser("scoped", iam.RoleClusterAdmin),
			want:      map[string]part{"alpha": all, "beta": all, "delta": {access: true, policy: true}, "gamma": all},
		},
		// And it can give: delta lets viewer read its policy.
		{
			name:      "whom the policy of a bucket lets read it",
			principal: with("viewer", listAll),
			want:      map[string]part{"delta": {policy: true}},
		},
		// A row of the activity is of an object: what /object/access would
		// refuse of that object is not shown here either. The row goes
		// whole, the others stay in their order, newest first.
		{
			name: "a policy of their own that denies the objects of a prefix",
			principal: with("kim", statement(iam.Allow, "s3:*", "*"), statement(iam.Allow, "admin:*", "*"),
				statement(iam.Deny, access, "arn:aws:s3:::alpha/a/t*")),
			want:   everywhere(all),
			hidden: map[string][]string{"alpha": {"a/two.txt"}},
		},
		// The scenario the review found: the uploads and the download of
		// what is under a/ are not shown, the upload beside it is.
		{
			name: "a policy of their own that denies the objects under a prefix",
			principal: with("lena", statement(iam.Allow, "s3:*", "*"), statement(iam.Allow, "admin:*", "*"),
				statement(iam.Deny, access, "arn:aws:s3:::alpha/a/*")),
			want:   everywhere(all),
			hidden: map[string][]string{"alpha": {"a/one.txt", "a/two.txt"}},
		},
		// The bucket is theirs to see - who created it is shown - and none
		// of its objects: an activity without rows.
		{
			name: "a policy of their own that denies every object of a bucket",
			principal: with("mona", statement(iam.Allow, "s3:*", "*"), statement(iam.Allow, "admin:*", "*"),
				statement(iam.Deny, access, "arn:aws:s3:::alpha/*")),
			want:   everywhere(all),
			hidden: map[string][]string{"alpha": {"a/one.txt", "a/two.txt", "b/three.txt"}},
		},
		// Decided by the key the row has, though the caller is shown none:
		// the row of a denied object is not there without its key either.
		{
			name:      "who may list no bucket and is denied one object",
			principal: with("nina", listAll, statement(iam.Allow, access, "*"), statement(iam.Deny, access, "arn:aws:s3:::alpha/a/one.txt")),
			want:      everywhere(part{access: true}),
			hidden:    map[string][]string{"alpha": {"a/one.txt"}},
		},
		// The stricter reading: the action is needed on the bucket. Held on
		// objects only it shows nothing here, though /object/access answers
		// for those objects and a listing of the bucket says "included".
		{
			name: "who holds the action on the objects of a prefix and not on the bucket",
			principal: with("olga", listAll, statement(iam.Allow, "s3:ListBucket", "arn:aws:s3:::alpha"),
				statement(iam.Allow, access, "arn:aws:s3:::alpha/a/*")),
			want: map[string]part{},
		},
		{name: "who may not list the buckets", principal: stranger(), wantErr: domain.ErrAccessDenied},
		{name: "an auditor, who may not list the buckets", principal: roleUser("oscar", iam.RoleAuditor), wantErr: domain.ErrAccessDenied},
		{name: "nobody", principal: anonymous(), wantErr: domain.ErrAccessDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newDetailsFixture(ctx, t)
			world := f.populate(ctx, t).without(tt.hidden)
			f.policies.all.Store(0)
			f.policies.one.Store(0)

			got, err := f.buckets.ListDetailed(ctx, tt.principal)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ListDetailed = %v, want %v", err, tt.wantErr)
			}
			denied := f.denials(ctx, t)
			if err != nil {
				// The list itself is the one thing that is gated, and so
				// the one thing a refusal is recorded for - before
				// anything is read.
				if len(denied) != 1 || denied[0].Action != "s3:ListAllMyBuckets" {
					t.Errorf("the refusal left the audit records %+v", denied)
				}
				if got != nil || f.policies.all.Load() != 0 || f.policies.one.Load() != 0 || len(f.activity.asked()) != 0 {
					t.Errorf("a refusal came with %+v, after %d pages of policies and %d readings of records",
						got, f.policies.all.Load(), len(f.activity.asked()))
				}
				return
			}
			checkDetails(t, world, got, tt.want)
			// What is left out is what the answer about one object would
			// refuse: the two cannot disagree.
			for name, keys := range tt.hidden {
				for _, key := range keys {
					single := iam.Request{Principal: tt.principal, Action: iam.ActionGetObjectAccess, Resource: iam.Resource{Bucket: name, Key: key}}
					if allowed, err := f.authz.Allowed(ctx, single); err != nil || allowed {
						t.Errorf("%s/%s is left out of the list and its own answer is allowed: %v, %v", name, key, allowed, err)
					}
				}
			}
			// Nothing was asked for by name and refused: no record.
			if len(denied) != 0 {
				t.Errorf("parts that were left out are in the audit trail: %+v", denied)
			}
			// Four buckets, three a page: one walk is two pages, whoever
			// asks, and no bucket is asked about by itself.
			if pages, single := f.policies.all.Load(), f.policies.one.Load(); pages != 2 || single != 0 {
				t.Errorf("the policies were read in %d pages and %d times for one bucket; want one walk of 2 pages", pages, single)
			}
			// One reading of the records, of the buckets they are shown of.
			var shown []string
			for _, d := range got {
				if d.AccessIncluded {
					shown = append(shown, d.Bucket.Name)
				}
			}
			asked := f.activity.asked()
			if (len(shown) == 0 && len(asked) != 0) || (len(shown) > 0 && (len(asked) != 1 || !slices.Equal(asked[0], shown))) {
				t.Errorf("the records were asked for as %v, want once for %v", asked, shown)
			}
			// What the result holds is the caller's own: changing it
			// changes nothing of the next answer.
			for i := range got {
				for j := range got[i].Activity.Uploads {
					got[i].Activity.Uploads[j].By = "somebody else"
				}
			}
			again, err := f.buckets.ListDetailed(ctx, tt.principal)
			if err != nil {
				t.Fatalf("ListDetailed again: %v", err)
			}
			checkDetails(t, world, again, tt.want)
		})
	}
}

// TestListWithoutDetails: List reads neither policies nor records, and a
// service that was given neither says of no bucket more than List does.
func TestListWithoutDetails(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newDetailsFixture(ctx, t)
	world := f.populate(ctx, t)
	f.policies.all.Store(0)
	f.policies.one.Store(0)

	plain, err := f.buckets.List(ctx, porttest.Root())
	if err != nil || len(plain) != 4 {
		t.Fatalf("List = %+v, %v", plain, err)
	}
	bare := app.NewBucketService(f.base.meta, f.authz, f.base.clock)
	got, err := bare.ListDetailed(ctx, porttest.Root())
	if err != nil {
		t.Fatalf("ListDetailed: %v", err)
	}
	checkDetails(t, world, got, map[string]part{})
	if f.policies.all.Load() != 0 || f.policies.one.Load() != 0 || len(f.activity.asked()) != 0 {
		t.Errorf("a plain list read %d pages of policies, %d single policies and the records %v",
			f.policies.all.Load(), f.policies.one.Load(), f.activity.asked())
	}
}

// TestListDetailedWhenSomethingIsWrong: what may be shown and cannot be read
// fails the call - for who would be shown it; the damaged policy of one
// bucket leaves that bucket without what the policy has a say in, and the
// others as they are.
func TestListDetailedWhenSomethingIsWrong(t *testing.T) {
	t.Parallel()
	boom := errors.New("the store is away")
	all := part{access: true, keys: true, policy: true}
	admin := roleUser("carol", iam.RoleClusterAdmin)

	t.Run("the records cannot be read", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f := newDetailsFixture(ctx, t)
		world := f.populate(ctx, t)
		f.activity.fail = boom
		for _, p := range []iam.Principal{porttest.Root(), admin} {
			if got, err := f.buckets.ListDetailed(ctx, p); !errors.Is(err, boom) || got != nil {
				t.Errorf("%s: ListDetailed = %+v, %v; want %v", p.Name(), got, err, boom)
			}
		}
		// Who is shown none of them is not failed for them.
		got, err := f.buckets.ListDetailed(ctx, roleUser("dave", iam.RoleBucketAdmin))
		if err != nil {
			t.Fatalf("ListDetailed for who is shown no record: %v", err)
		}
		checkDetails(t, world, got, everywhere(part{policy: true}))
	})

	t.Run("the records are not those asked for", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f := newDetailsFixture(ctx, t)
		f.populate(ctx, t)
		f.activity.short = true
		if got, err := f.buckets.ListDetailed(ctx, admin); !errors.Is(err, domain.ErrIntegrity) || got != nil {
			t.Errorf("ListDetailed = %+v, %v; want %v", got, err, domain.ErrIntegrity)
		}
	})

	// No page of the policies can be read: no bucket can be decided, nor
	// summarized. A store that is damaged reads as one that is away - not as
	// the damaged policy of a bucket.
	for name, source := range map[string]ports.BucketPolicies{"away": failingPolicies{}, "damaged": unreadablePolicies{}} {
		t.Run("the policies cannot be read, the store is "+name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newDetailsFixture(ctx, t)
			f.populate(ctx, t)
			buckets := app.NewBucketService(f.base.meta, f.authz, f.base.clock).WithDetails(source, f.activity)
			for _, p := range []iam.Principal{porttest.Root(), admin, lister()} {
				got, err := buckets.ListDetailed(ctx, p)
				if !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrIntegrity) || got != nil {
					t.Errorf("%s: ListDetailed = %+v, %v; want %v", p.Name(), got, err, domain.ErrUnavailable)
				}
			}
			// Beside an authorizer that cannot say what it walked, the
			// service's own walk for the summaries fails the same way.
			own := app.NewBucketService(f.base.meta, plainAuthorizer{f.authz}, f.base.clock).WithDetails(source, f.activity)
			if got, err := own.ListDetailed(ctx, porttest.Root()); !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrIntegrity) || got != nil {
				t.Errorf("a plain authorizer: ListDetailed = %+v, %v; want %v", got, err, domain.ErrUnavailable)
			}
			if denied := f.denials(ctx, t); len(denied) != 0 {
				t.Errorf("a list that failed is in the audit trail as refused: %+v", denied)
			}
		})
	}

	t.Run("the policy of one bucket is damaged", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f := newDetailsFixture(ctx, t)
		world := f.populate(ctx, t)
		if err := f.base.meta.SetBucketConfig(ctx, "gamma", bucket.ConfigPolicy, []byte("not a policy")); err != nil {
			t.Fatal(err)
		}
		// Whom the policy binds is refused what it might have denied: no
		// keys, no summary.
		got, err := f.buckets.ListDetailed(ctx, admin)
		if err != nil {
			t.Fatalf("ListDetailed: %v", err)
		}
		checkDetails(t, world, got, map[string]part{"alpha": all, "beta": all, "delta": all, "gamma": {access: true}})
		// Root is bound by no bucket policy, and still there is no summary
		// of what cannot be read.
		got, err = f.buckets.ListDetailed(ctx, porttest.Root())
		if err != nil {
			t.Fatalf("ListDetailed for root: %v", err)
		}
		checkDetails(t, world, got, map[string]part{"alpha": all, "beta": all, "delta": all, "gamma": {access: true, keys: true}})
		// Nor beside an authorizer that cannot say what it walked.
		own := app.NewBucketService(f.base.meta, plainAuthorizer{f.authz}, f.base.clock).WithDetails(f.policies, f.activity)
		if got, err = own.ListDetailed(ctx, porttest.Root()); err != nil {
			t.Fatalf("ListDetailed for root, a plain authorizer: %v", err)
		}
		checkDetails(t, world, got, map[string]part{"alpha": all, "beta": all, "delta": all, "gamma": {access: true, keys: true}})
		if denied := f.denials(ctx, t); len(denied) != 0 {
			t.Errorf("a damaged policy is in the audit trail as a refusal: %+v", denied)
		}
	})

	t.Run("the caller has gone", func(t *testing.T) {
		t.Parallel()
		f := newDetailsFixture(t.Context(), t)
		f.populate(t.Context(), t)
		gone, cancel := context.WithCancel(t.Context())
		cancel()
		if got, err := f.buckets.ListDetailed(gone, admin); !errors.Is(err, context.Canceled) || got != nil {
			t.Errorf("ListDetailed = %+v, %v; want %v", got, err, context.Canceled)
		}
		if asked := f.activity.asked(); len(asked) != 0 {
			t.Errorf("the records were read for a caller who had gone: %v", asked)
		}
	})
}

// TestListDetailedOfManyBuckets: the records of more buckets than one call
// may name are asked for in as few calls as that allows, and the policies
// are still walked once.
func TestListDetailedOfManyBuckets(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newDetailsFixture(ctx, t)
	const buckets, page = ports.MaxBucketActivities + 1, 4096
	for i := range buckets {
		name := fmt.Sprintf("bucket-%05d", i)
		if err := f.base.meta.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: testNow, CreatedBy: "root"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.buckets.SetPolicy(ctx, porttest.Root(), "bucket-10000", []byte(strings.ReplaceAll(betaPolicy, "beta", "bucket-10000"))); err != nil {
		t.Fatal(err)
	}
	f.base.meta.PolicyPage = page
	for _, p := range []iam.Principal{porttest.Root(), roleUser("carol", iam.RoleClusterAdmin)} {
		f.policies.all.Store(0)
		f.policies.one.Store(0)
		before := len(f.activity.asked())
		got, err := f.buckets.ListDetailed(ctx, p)
		if err != nil || len(got) != buckets {
			t.Fatalf("%s: ListDetailed = %d buckets, %v", p.Name(), len(got), err)
		}
		last := got[buckets-1]
		if !last.AccessIncluded || last.Bucket.CreatedBy != "root" || last.Policy == nil || *last.Policy != (iam.BucketPolicySummary{Statements: 2, OpenToAnyone: true}) {
			t.Errorf("%s: the last bucket = %+v", p.Name(), last)
		}
		if first := got[0]; !first.PolicyIncluded || first.Policy != nil {
			t.Errorf("%s: the first bucket = %+v", p.Name(), first)
		}
		asked := f.activity.asked()[before:]
		if len(asked) != 2 || len(asked[0]) != ports.MaxBucketActivities || len(asked[1]) != 1 {
			t.Errorf("%s: the records were asked for in %d calls", p.Name(), len(asked))
		}
		if pages, single := f.policies.all.Load(), f.policies.one.Load(); pages != (buckets+page-1)/page || single != 0 {
			t.Errorf("%s: the policies were read in %d pages and %d times for one bucket", p.Name(), pages, single)
		}
	}
}

// plainAuthorizer is an authorizer that decides as a node's does and can do
// no more than ports.Authorizer says.
type plainAuthorizer struct{ ports.Authorizer }

// TestListDetailedSummaries: the summary of a policy is of the document the
// walk of this call read - a policy that was changed or removed between two
// calls is summarized as it is now, whatever the authorizer remembers of what
// it parsed - for who is bound by bucket policies and for root, who is not;
// and an authorizer that cannot say what it walked changes nothing of the
// answer.
func TestListDetailedSummaries(t *testing.T) {
	t.Parallel()
	all := part{access: true, keys: true, policy: true}
	changed := strings.ReplaceAll(deltaPolicy, "delta", "beta")
	for _, p := range []iam.Principal{porttest.Root(), roleUser("carol", iam.RoleClusterAdmin)} {
		for _, plain := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, a plain authorizer %v", p.Name(), plain), func(t *testing.T) {
				t.Parallel()
				ctx := t.Context()
				f := newDetailsFixture(ctx, t)
				world := f.populate(ctx, t)
				// Whoever walks, a call walks the policies once: by the
				// authorizer as it decides or, for whom none binds, for the
				// summaries. Only beside an authorizer that cannot say what
				// it walked is there a second walk for them.
				pages := int64(2)
				if plain {
					f.buckets = app.NewBucketService(f.base.meta, plainAuthorizer{f.authz}, f.base.clock).WithDetails(f.policies, f.activity)
					if !p.Root {
						pages = 4
					}
				}
				list := func(want map[string]*iam.BucketPolicySummary) {
					t.Helper()
					f.policies.all.Store(0)
					f.policies.one.Store(0)
					got, err := f.buckets.ListDetailed(ctx, p)
					if err != nil {
						t.Fatalf("ListDetailed: %v", err)
					}
					world.policies = want
					checkDetails(t, world, got, everywhere(all))
					if read, single := f.policies.all.Load(), f.policies.one.Load(); read != pages || single != 0 {
						t.Errorf("the policies were read in %d pages and %d times for one bucket; want %d pages", read, single, pages)
					}
				}
				// Twice as they are: the second answer is from what the
				// first left remembered, and says the same.
				list(world.policies)
				list(world.policies)
				if err := f.buckets.SetPolicy(ctx, porttest.Root(), "beta", []byte(changed)); err != nil {
					t.Fatal(err)
				}
				list(map[string]*iam.BucketPolicySummary{"beta": {Statements: 3}, "delta": {Statements: 3}})
				if err := f.buckets.SetPolicy(ctx, porttest.Root(), "beta", nil); err != nil {
					t.Fatal(err)
				}
				if err := f.buckets.SetPolicy(ctx, porttest.Root(), "alpha", []byte(strings.ReplaceAll(betaPolicy, "beta", "alpha"))); err != nil {
					t.Fatal(err)
				}
				list(map[string]*iam.BucketPolicySummary{"alpha": {Statements: 2, OpenToAnyone: true}, "delta": {Statements: 3}})
			})
		}
	}
}

// TestSetPolicyRefusesAPolicyThatCannotBeKept: every document the bucket
// policy parser refuses is a domain.ErrInvalidPolicy - which the management
// API answers 400 InvalidPolicy - and an argument error still; nothing is
// stored, and the policy the bucket had is kept.
func TestSetPolicyRefusesAPolicyThatCannotBeKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	meta := porttest.NewMemMetadata()
	clock := porttest.NewClock(testNow)
	if err := meta.CreateBucket(ctx, bucket.Bucket{Name: "site"}); err != nil {
		t.Fatal(err)
	}
	authz := app.NewAuthorizer(slog.New(slog.DiscardHandler), clock, nil).WithBucketPolicies(meta)
	buckets := app.NewBucketService(meta, authz, clock)
	kept := []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`)
	if err := buckets.SetPolicy(ctx, porttest.Root(), "site", kept); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		document string
	}{
		{"not JSON", `not a policy`},
		{"no statement", `{"Version":"2012-10-17"}`},
		{"no principal", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`},
		{"another bucket", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::other/*"}]}`},
		{"an action that is not S3's", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"admin:ListUsers","Resource":"arn:aws:s3:::site"}]}`},
		{"governance bypass", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:BypassGovernanceRetention","Resource":"arn:aws:s3:::site/*"}]}`},
		{"larger than allowed", `{"Version":"2012-10-17","Statement":[{"Sid":"` + strings.Repeat("x", iam.MaxPolicyBytes) + `","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`},
		// Stored as sent, it would be handed to S3 clients as a text that
		// is not JSON (PLAN 9.2a).
		{"a closing brace after the document", string(kept) + `}`},
		{"a closing bracket after the document", string(kept) + "\n]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := buckets.SetPolicy(ctx, porttest.Root(), "site", []byte(tt.document))
			if !errors.Is(err, domain.ErrInvalidPolicy) || !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("SetPolicy = %v, want an ErrInvalidPolicy", err)
			}
			if got, err := buckets.Policy(ctx, porttest.Root(), "site"); err != nil || string(got) != string(kept) {
				t.Errorf("the policy after a refused one = %s, %v", got, err)
			}
		})
	}
	// Errors that are not about the document are not ErrInvalidPolicy.
	elsewhere := bytes.ReplaceAll(kept, []byte(":::site/"), []byte(":::nowhere/"))
	if err := buckets.SetPolicy(ctx, porttest.Root(), "nowhere", elsewhere); !errors.Is(err, domain.ErrBucketNotFound) || errors.Is(err, domain.ErrInvalidPolicy) {
		t.Errorf("SetPolicy on a bucket that is not there = %v", err)
	}
}

// TestPolicyIsTheDocumentEnforced: the policy a bucket is read back with is
// the document the authorizer enforces. A record stored before PLAN 9.2a
// with a closing bracket after its document reads back as that document,
// which is JSON; every other record reads back byte for byte as stored -
// one that is JSON, however it is laid out, and one that is damaged.
func TestPolicyIsTheDocumentEnforced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	document := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::site/*"}]}`
	tests := []struct {
		name   string
		stored string
		want   string
	}{
		{"JSON", document, document},
		{"JSON laid out with white space", " \n" + strings.ReplaceAll(document, ",", ",\n\t") + "\r\n ", " \n" + strings.ReplaceAll(document, ",", ",\n\t") + "\r\n "},
		{"a closing brace after it", document + "}", document},
		{"a closing bracket and more after white space", document + " \n]x", document},
		{"a value after it", document + " x", document + " x"},
		{"not JSON", "not a policy", "not a policy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			meta := porttest.NewMemMetadata()
			clock := porttest.NewClock(testNow)
			if err := meta.CreateBucket(ctx, bucket.Bucket{Name: "site"}); err != nil {
				t.Fatal(err)
			}
			if err := meta.SetBucketConfig(ctx, "site", bucket.ConfigPolicy, []byte(tt.stored)); err != nil {
				t.Fatal(err)
			}
			authz := app.NewAuthorizer(slog.New(slog.DiscardHandler), clock, nil).WithBucketPolicies(meta)
			buckets := app.NewBucketService(meta, authz, clock)
			got, err := buckets.Policy(ctx, porttest.Root(), "site")
			if err != nil || string(got) != tt.want {
				t.Errorf("Policy = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// rulesFailingStore is a bucket store whose writes of tag rules fail, and
// that notes how buckets are deleted: what CreateLowercasing does when its
// second entry is not made.
type rulesFailingStore struct {
	*porttest.MemMetadata
	// failRules, when set, is returned by every write of tag rules; gap,
	// when set, is called first - what others do between the two entries -
	// and cancel, as if the caller had gone.
	failRules error
	gap       func(ctx context.Context, m *porttest.MemMetadata) error
	cancel    func()
	// committed makes the failing write of the rules write them first: an
	// outcome that was not known.
	committed bool
	// failDelete, when set, is returned by DeleteBucket - after deleting,
	// when deleteFirst is set.
	failDelete  error
	deleteFirst bool
	// seen, when set, changes the record GetBucket reads; configs are
	// documents GetBucketConfig reads in place of the store's - what
	// others did in the gap that the store's own changes cannot show.
	seen    func(b *bucket.Bucket)
	configs map[string][]byte

	mu        sync.Mutex
	deletes   int
	deleteCtx error
}

func (s *rulesFailingStore) SetBucketConfig(ctx context.Context, name, kind string, document []byte) error {
	if kind == bucket.ConfigTagRules && s.failRules != nil {
		if s.gap != nil {
			if err := s.gap(ctx, s.MemMetadata); err != nil {
				return err
			}
		}
		if s.committed {
			if err := s.MemMetadata.SetBucketConfig(ctx, name, kind, document); err != nil {
				return err
			}
		}
		if s.cancel != nil {
			s.cancel()
		}
		return s.failRules
	}
	return s.MemMetadata.SetBucketConfig(ctx, name, kind, document)
}

func (s *rulesFailingStore) GetBucket(ctx context.Context, name string) (bucket.Bucket, error) {
	b, err := s.MemMetadata.GetBucket(ctx, name)
	if err == nil && s.seen != nil {
		s.seen(&b)
	}
	return b, err
}

func (s *rulesFailingStore) GetBucketConfig(ctx context.Context, name, kind string) ([]byte, error) {
	if document, ok := s.configs[kind]; ok {
		return document, nil
	}
	return s.MemMetadata.GetBucketConfig(ctx, name, kind)
}

func (s *rulesFailingStore) DeleteBucket(ctx context.Context, name string) error {
	s.mu.Lock()
	s.deletes++
	s.deleteCtx = ctx.Err()
	s.mu.Unlock()
	if s.failDelete != nil {
		if s.deleteFirst {
			if err := s.MemMetadata.DeleteBucket(ctx, name); err != nil {
				return err
			}
		}
		return s.failDelete
	}
	return s.MemMetadata.DeleteBucket(ctx, name)
}

// lowercasingRow is a case of TestCreateLowercasing.
type lowercasingRow struct {
	name      string
	principal iam.Principal
	bucket    string
	// noUndo builds the service without WithUndo.
	noUndo  bool
	setup   func(t *testing.T, s *rulesFailingStore, cancel func())
	wantErr error
	notErr  error
	// exists is whether the bucket is there afterwards, and lowercase
	// whether it has the rule; deletes is how often a deletion was tried.
	exists, lowercase bool
	deletes           int
}

// TestCreateLowercasing: a bucket made as the admin API makes one has the
// tag rule lowercase from the start (ADR-0018). The bucket and its rules
// are two entries. When the second is not made the bucket is taken back -
// even when the caller has gone - and the caller is told why; unless it is
// no longer only what was made, or cannot be taken back: then it is left,
// and the creation is told as made.
func TestCreateLowercasing(t *testing.T) {
	t.Parallel()
	boom := errors.New("raft unavailable")
	gone := fmt.Errorf("%w: the rules entry", domain.ErrOutcomeUnknown)
	other := iam.Principal{AccessKeyID: "OTHER"}
	uploadID, err := object.ParseID(strings.Repeat("0", 31) + "1")
	if err != nil {
		t.Fatal(err)
	}
	// inTheGap is a row in which something is done to the bucket between
	// the two entries, after which it is left as it is.
	inTheGap := func(do func(ctx context.Context, m *porttest.MemMetadata) error) func(*testing.T, *rulesFailingStore, func()) {
		return func(_ *testing.T, s *rulesFailingStore, _ func()) { s.failRules, s.gap = boom, do }
	}
	tests := []lowercasingRow{
		{name: "made with the rule", principal: porttest.Root(), bucket: "photos", exists: true, lowercase: true},
		{name: "refused", principal: anonymous(), bucket: "photos", wantErr: domain.ErrAccessDenied},
		{name: "an invalid name", principal: porttest.Root(), bucket: "Not_Valid", wantErr: domain.ErrInvalidBucketName},
		{
			name: "a bucket that exists keeps its rules", principal: porttest.Root(), bucket: "photos",
			setup: func(t *testing.T, s *rulesFailingStore, _ func()) {
				t.Helper()
				if err := s.CreateBucket(t.Context(), bucket.Bucket{Name: "photos", CreatedAt: testNow}); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: domain.ErrBucketAlreadyExists, exists: true,
		},
		{
			name: "the rules are not made: the bucket is taken back", principal: porttest.Root(), bucket: "photos",
			setup:   func(_ *testing.T, s *rulesFailingStore, _ func()) { s.failRules = boom },
			wantErr: boom, deletes: 1,
		},
		{
			name: "the rules are not made and the caller has gone: still taken back", principal: porttest.Root(), bucket: "photos",
			setup: func(_ *testing.T, s *rulesFailingStore, cancel func()) {
				s.failRules, s.cancel = gone, cancel
			},
			wantErr: domain.ErrOutcomeUnknown, deletes: 1,
		},
		{
			name: "somebody deleted it in the gap: nothing to take back", principal: porttest.Root(), bucket: "photos",
			setup: inTheGap(func(ctx context.Context, m *porttest.MemMetadata) error {
				return m.DeleteBucket(ctx, "photos")
			}),
			wantErr: domain.ErrConflict, notErr: boom,
		},
		{
			name: "the rules entry finds no bucket: nothing to take back", principal: porttest.Root(), bucket: "photos",
			setup: func(_ *testing.T, s *rulesFailingStore, _ func()) {
				s.failRules = fmt.Errorf("%w: photos", domain.ErrBucketNotFound)
				s.gap = func(ctx context.Context, m *porttest.MemMetadata) error { return m.DeleteBucket(ctx, "photos") }
			},
			wantErr: domain.ErrConflict, notErr: domain.ErrBucketNotFound,
		},
		{
			name: "the rules were made after all: made", principal: porttest.Root(), bucket: "photos",
			setup: func(_ *testing.T, s *rulesFailingStore, _ func()) {
				s.failRules, s.committed = gone, true
			},
			exists: true, lowercase: true,
		},
		{
			name: "the deletion's outcome is not known, and it was deleted: taken back", principal: porttest.Root(), bucket: "photos",
			setup: func(_ *testing.T, s *rulesFailingStore, _ func()) {
				s.failRules, s.failDelete, s.deleteFirst = boom, gone, true
			},
			wantErr: boom, deletes: 1,
		},
		{
			name: "the deletion's outcome is not known, and it was not deleted: left", principal: porttest.Root(), bucket: "photos",
			setup: func(_ *testing.T, s *rulesFailingStore, _ func()) {
				s.failRules, s.failDelete = boom, gone
			},
			exists: true, deletes: 1,
		},
		{
			name: "the deletion fails: left, and told as made", principal: porttest.Root(), bucket: "photos",
			setup: func(_ *testing.T, s *rulesFailingStore, _ func()) {
				s.failRules, s.failDelete = boom, fmt.Errorf("%w: photos", domain.ErrUnavailable)
			},
			exists: true, deletes: 1,
		},
		{
			name: "what is in it cannot be read: left", principal: porttest.Root(), bucket: "photos", noUndo: true,
			setup:  func(_ *testing.T, s *rulesFailingStore, _ func()) { s.failRules = boom },
			exists: true,
		},
		{
			name: "a policy was set in the gap: left", principal: porttest.Root(), bucket: "photos",
			setup: inTheGap(func(ctx context.Context, m *porttest.MemMetadata) error {
				return m.SetBucketConfig(ctx, "photos", bucket.ConfigCORS, []byte(`{"rules":[]}`))
			}),
			exists: true,
		},
		{
			name: "a sub-bucket was made in the gap: left", principal: porttest.Root(), bucket: "photos",
			setup: inTheGap(func(ctx context.Context, m *porttest.MemMetadata) error {
				return m.PutSubBucket(ctx, "photos", bucket.SubBucket{Name: "reports", CreatedAt: testNow}, audit.Record{
					Time: testNow, Actor: "user/other", Action: string(iam.ActionPutSubBucket), Resource: "arn:aws:s3:::photos", Outcome: audit.Succeeded,
				})
			}),
			exists: true,
		},
		{
			name: "an upload began in the gap: left", principal: porttest.Root(), bucket: "photos",
			setup: inTheGap(func(ctx context.Context, m *porttest.MemMetadata) error {
				return m.CreateUpload(ctx, object.Upload{Bucket: "photos", Key: "k", ID: uploadID, InitiatedAt: testNow, Encryption: object.Encryption{KeyMode: object.KeyModePartHKDF}})
			}),
			exists: true,
		},
		{
			name: "versioning was turned on in the gap: left", principal: porttest.Root(), bucket: "photos",
			setup: inTheGap(func(ctx context.Context, m *porttest.MemMetadata) error {
				return m.SetBucketVersioning(ctx, "photos", bucket.VersioningEnabled)
			}),
			exists: true,
		},
		{
			name: "it was made again by somebody else in the gap: left", principal: porttest.Root(), bucket: "photos",
			setup: inTheGap(func(ctx context.Context, m *porttest.MemMetadata) error {
				made, err := m.GetBucket(ctx, "photos")
				if err != nil {
					return err
				}
				if err := m.DeleteBucket(ctx, "photos"); err != nil {
					return err
				}
				return m.CreateBucket(ctx, bucket.Bucket{Name: "photos", CreatedAt: made.CreatedAt, CreatedBy: "user/" + other.AccessKeyID})
			}),
			exists: true,
		},
		{
			name: "it was made again at another time in the gap: left", principal: porttest.Root(), bucket: "photos",
			setup: inTheGap(func(ctx context.Context, m *porttest.MemMetadata) error {
				made, err := m.GetBucket(ctx, "photos")
				if err != nil {
					return err
				}
				if err := m.DeleteBucket(ctx, "photos"); err != nil {
					return err
				}
				return m.CreateBucket(ctx, bucket.Bucket{Name: "photos", CreatedAt: testNow.Add(time.Second), CreatedBy: made.CreatedBy})
			}),
			exists: true,
		},
	}
	// What the record of the bucket says that only other changes give it,
	// and every kind of configuration document: each in the gap leaves it.
	// (A bucket with Object Lock keeps versions, and only such a bucket has
	// a default retention: the store cannot show either alone, so they are
	// seen as a record that says so.)
	for name, seen := range map[string]func(*bucket.Bucket){
		"a quota":             func(b *bucket.Bucket) { b.QuotaBytes = 1 << 30 },
		"a replica source":    func(b *bucket.Bucket) { b.ReplicaFrom = "site-a" },
		"object lock":         func(b *bucket.Bucket) { b.ObjectLock = true },
		"a default retention": func(b *bucket.Bucket) { b.DefaultRetention = &bucket.DefaultRetention{Days: 1} },
	} {
		tests = append(tests, lowercasingRow{
			name: name + " was given in the gap: left", principal: porttest.Root(), bucket: "photos", exists: true,
			setup: func(_ *testing.T, s *rulesFailingStore, _ func()) { s.failRules, s.seen = boom, seen },
		})
	}
	for _, kind := range []string{
		bucket.ConfigPolicy, bucket.ConfigCORS, bucket.ConfigTagging, bucket.ConfigLifecycle,
		bucket.ConfigNotification, bucket.ConfigReplication, bucket.ConfigTagRules,
	} {
		tests = append(tests, lowercasingRow{
			name: "a document " + kind + " was set in the gap: left", principal: porttest.Root(), bucket: "photos", exists: true,
			setup: func(_ *testing.T, s *rulesFailingStore, _ func()) {
				s.failRules, s.configs = boom, map[string][]byte{kind: []byte(`{"set":"by another"}`)}
			},
		})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			store := &rulesFailingStore{MemMetadata: f.meta}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.setup != nil {
				tt.setup(t, store, cancel)
			}
			buckets := app.NewBucketService(store, f.authz, f.clock)
			if !tt.noUndo {
				buckets = buckets.WithUndo(store, store, slog.New(slog.DiscardHandler))
			}
			made, err := buckets.CreateLowercasing(ctx, tt.principal, tt.bucket)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("CreateLowercasing = %v, want %v", err, tt.wantErr)
			}
			// The bucket answered is the one written, or none.
			if wantMade := (bucket.Bucket{Name: tt.bucket, CreatedAt: testNow, CreatedBy: "root"}); (err == nil) != (made == wantMade) {
				t.Errorf("CreateLowercasing answered %+v (%v), want %+v when it is made", made, err, wantMade)
			}
			if err != nil && !strings.HasPrefix(err.Error(), "bucket.CreateLowercasing: ") {
				t.Errorf("error %q is not wrapped with the use case name", err)
			}
			if tt.notErr != nil && errors.Is(err, tt.notErr) {
				t.Errorf("the error %v is told as %v", err, tt.notErr)
			}
			_, getErr := f.meta.GetBucket(t.Context(), tt.bucket)
			if exists := getErr == nil; exists != tt.exists {
				t.Errorf("the bucket is there: %v, want %v (%v)", exists, tt.exists, getErr)
			}
			if tt.exists {
				rules, err := f.buckets.TagRules(t.Context(), porttest.Root(), tt.bucket)
				if err != nil || rules.Lowercase != tt.lowercase || rules.CatalogueOnly || len(rules.Required) != 0 {
					t.Errorf("the rules = %+v, %v; want lowercase %v and nothing else", rules, err, tt.lowercase)
				}
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if store.deletes != tt.deletes || store.deleteCtx != nil {
				t.Errorf("%d deletions were tried, with the context %v; want %d with a live one", store.deletes, store.deleteCtx, tt.deletes)
			}
		})
	}
}

// TestCreateLowercasingIsAudited: what CreateLowercasing did is in the
// trail, in one record of s3:CreateBucket - made with its rule; made, and
// taken back (failed); or made without it, which the log says at ERROR as
// well. It asks s3:CreateBucket alone.
func TestCreateLowercasingIsAudited(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newIAMFixture(t)
	store := &rulesFailingStore{MemMetadata: f.meta}
	logs := &syncBuffer{}
	buckets := app.NewBucketService(store, f.authz, f.clock).WithAuditor(f.auditor).
		WithUndo(store, store, slog.New(slog.NewJSONHandler(logs, nil)))
	// A policy that may make buckets and do nothing else.
	maker := []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:CreateBucket","Resource":"*"}]}`)
	if err := f.iam.PutPolicy(ctx, f.root, "maker", maker, true); err != nil {
		t.Fatal(err)
	}
	if err := f.iam.PutRole(ctx, f.root, iam.Role{Name: "maker", Policies: []string{"maker"}}, true); err != nil {
		t.Fatal(err)
	}
	alice := f.as(t, f.user(t, "alice", "maker"))
	base := len(f.trail(t))
	if _, err := buckets.CreateLowercasing(ctx, alice, "site"); err != nil {
		t.Fatal(err)
	}
	store.failRules = errors.New("raft unavailable")
	if _, err := buckets.CreateLowercasing(ctx, alice, "undone"); err == nil {
		t.Fatal("a bucket whose rules were not made, and that was taken back, was told as made")
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("a bucket taken back is logged as an error: %s", logs.String())
	}
	store.gap = func(ctx context.Context, m *porttest.MemMetadata) error { return m.DeleteBucket(ctx, "vanished") }
	if _, err := buckets.CreateLowercasing(ctx, alice, "vanished"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a bucket deleted by another before its rules were made is told as %v", err)
	}
	store.gap = nil
	store.failDelete = errors.New("lost quorum")
	if _, err := buckets.CreateLowercasing(ctx, alice, "left"); err != nil {
		t.Fatalf("a bucket that was left is told as %v", err)
	}
	want := map[string]struct {
		outcome audit.Outcome
		details map[string]string
	}{
		"arn:aws:s3:::site":     {audit.Succeeded, map[string]string{"tag_rules": "lowercase"}},
		"arn:aws:s3:::undone":   {audit.Failed, map[string]string{"tag_rules": "failed", "undone": "true"}},
		"arn:aws:s3:::left":     {audit.Succeeded, map[string]string{"tag_rules": "not_set"}},
		"arn:aws:s3:::vanished": {audit.Failed, map[string]string{"tag_rules": "failed", "undone": "false", "removed_by_other": "true"}},
	}
	seen := 0
	for _, r := range f.trail(t)[base:] {
		if r.Action != string(iam.ActionCreateBucket) {
			continue
		}
		seen++
		w, ok := want[r.Resource]
		if !ok || r.Actor != "user/alice" || r.Outcome != w.outcome || !maps.Equal(r.Details, w.details) {
			t.Errorf("record %+v, want %+v", r, w)
		}
	}
	if seen != len(want) {
		t.Errorf("%d records of creations, want %d", seen, len(want))
	}
	if log := logs.String(); !strings.Contains(log, `"level":"ERROR"`) || !strings.Contains(log, `"bucket":"left"`) || !strings.Contains(log, "lowercase") {
		t.Errorf("the log of a bucket left without its rule: %s", log)
	}
}
