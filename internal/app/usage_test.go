package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// TestUsage: a bucket's objects are counted and their sizes added up, for
// whoever may list the bucket, and the count ends at its bound.
func TestUsage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := listFixture(t)
	f.createBucket(t, "empty")
	var bytes int64
	for _, key := range listingKeys() {
		bytes += int64(len(key))
	}

	for name, tt := range map[string]struct {
		bucket  string
		limit   int
		want    app.BucketUsage
		wantErr error
	}{
		"every object":      {bucket: "b", limit: app.MaxUsageScan, want: app.BucketUsage{Objects: int64(len(listingKeys())), Bytes: bytes, Complete: true}},
		"an empty bucket":   {bucket: "empty", limit: app.MaxUsageScan, want: app.BucketUsage{Complete: true}},
		"more than counted": {bucket: "b", limit: 2, want: app.BucketUsage{Objects: 2, Bytes: int64(len("a.txt") + len("b.txt")), Complete: false}},
		"exactly the bound": {bucket: "b", limit: len(listingKeys()), want: app.BucketUsage{Objects: int64(len(listingKeys())), Bytes: bytes, Complete: true}},
		"no such bucket":    {bucket: "nowhere", limit: app.MaxUsageScan, wantErr: domain.ErrBucketNotFound},
	} {
		// Not in parallel: the bucket gains an object further down.
		t.Run(name, func(t *testing.T) {
			got, err := f.objects.CountObjects(ctx, tt.bucket, tt.limit)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Errorf("Usage = %+v, %v; want %+v, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}

	// Who may not list the bucket is not told how much is in it - nor
	// whether it is there.
	stranger := iam.Principal{AccessKeyID: "AKIDSTRANGER00000001", User: "stranger"}
	for _, bucket := range []string{"b", "nowhere"} {
		if _, err := f.objects.Usage(ctx, stranger, bucket); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("Usage of %s for somebody who may not list it: error = %v, want %v", bucket, err, domain.ErrAccessDenied)
		}
	}

	// Through the use case: the figure, kept for half a minute - one walk
	// however many ask - and counted again after that.
	want := app.BucketUsage{Objects: int64(len(listingKeys())), Bytes: bytes, Complete: true}
	if got, err := f.objects.Usage(ctx, porttest.Root(), "b"); err != nil || got != want {
		t.Fatalf("Usage = %+v, %v; want %+v", got, err, want)
	}
	f.put(t, "b", "zz-new.txt", []byte("12345"))
	if got, err := f.objects.Usage(ctx, porttest.Root(), "b"); err != nil || got != want {
		t.Errorf("Usage a moment later = %+v, %v; want the figure of a moment ago, %+v", got, err, want)
	}
	// The figure that is kept is no way around the authorizer.
	if _, err := f.objects.Usage(ctx, stranger, "b"); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("Usage of a kept figure for somebody who may not list the bucket: error = %v", err)
	}
	f.clock.Advance(31 * time.Second)
	want.Objects, want.Bytes = want.Objects+1, want.Bytes+5
	if got, err := f.objects.Usage(ctx, porttest.Root(), "b"); err != nil || got != want {
		t.Errorf("Usage half a minute later = %+v, %v; want %+v", got, err, want)
	}
	if _, err := f.objects.Usage(ctx, porttest.Root(), "nowhere"); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("Usage of a bucket that is not there: error = %v", err)
	}
}
