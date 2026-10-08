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
