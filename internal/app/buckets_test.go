package app_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
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
	if err := buckets.SetPolicy(ctx, porttest.Root(), "site", []byte(`{"Version":"2012-10-17"}`)); !errors.Is(err, domain.ErrInvalidArgument) {
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
	if err := buckets.SetTags(ctx, porttest.Root(), "site", map[string]string{"team": "web"}); err != nil {
		t.Fatal(err)
	}
	if got, err := buckets.Tags(ctx, porttest.Root(), "site"); err != nil || got["team"] != "web" {
		t.Errorf("Tags = %v, %v", got, err)
	}
	if err := buckets.SetTags(ctx, porttest.Root(), "site", map[string]string{"": "x"}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("SetTags with an empty key = %v", err)
	}
	if err := buckets.SetTags(ctx, anonymous(), "site", nil); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("SetTags by nobody = %v", err)
	}
	if err := buckets.SetTags(ctx, porttest.Root(), "site", nil); err != nil {
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
