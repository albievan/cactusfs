package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
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

// usageReadFails is a usage store whose records cannot be read.
type usageReadFails struct {
	ports.BucketUsageStore
	err   error
	reads int
}

func (u *usageReadFails) BucketUsages(context.Context, []string) ([]ports.BucketUsage, error) {
	u.reads++
	return nil, u.err
}

// TestUsageFromTheRecord: where the metadata store keeps an exact count of
// a bucket, that is the answer, with every figure and the quota; otherwise
// the index is walked, as without a count.
func TestUsageFromTheRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.createBucket(t, "b")
	if err := f.meta.SetBucketVersioning(ctx, "b", bucket.VersioningEnabled); err != nil {
		t.Fatalf("SetBucketVersioning: %v", err)
	}
	// Two versions of a.txt, c.txt under a delete marker, and a part of
	// an upload in progress.
	f.put(t, "b", "a.txt", []byte("123"))
	f.put(t, "b", "a.txt", []byte("12345"))
	f.put(t, "b", "c.txt", []byte("1"))
	if _, err := f.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "b", Key: "c.txt"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	f.uploadPart(t, f.createUpload(t, "big.mp4"), 1, []byte("1234567"))
	if err := f.meta.SetBucketQuota(ctx, "b", 1<<20, audit.Record{}); err != nil {
		t.Fatalf("SetBucketQuota: %v", err)
	}
	// A bucket that is older than the count.
	f.createBucket(t, "old")
	f.put(t, "old", "x", []byte("1234"))
	f.meta.ForgetUsage("old")

	exact := app.BucketUsage{
		Objects: 1, Bytes: 5, Complete: true, Exact: true, Versions: 4, StoredBytes: 3 + 5 + 1, UploadBytes: 7, QuotaBytes: 1 << 20,
	}
	walked := app.BucketUsage{Objects: 1, Bytes: 5, Complete: true}
	broken := errors.New("the state cannot be read")
	for name, tt := range map[string]struct {
		usage   ports.BucketUsageStore
		bucket  string
		want    app.BucketUsage
		wantErr error
	}{
		"an exact count":                       {usage: f.meta, bucket: "b", want: exact},
		"no count kept":                        {bucket: "b", want: walked},
		"a bucket not counted yet":             {usage: f.meta, bucket: "old", want: app.BucketUsage{Objects: 1, Bytes: 4, Complete: true}},
		"a deployment that does not count yet": {usage: &usageReadFails{BucketUsageStore: f.meta, err: fmtErr(domain.ErrUnavailable)}, bucket: "b", want: walked},
		"a count that cannot be read":          {usage: &usageReadFails{BucketUsageStore: f.meta, err: broken}, bucket: "b", wantErr: broken},
		"no such bucket":                       {usage: f.meta, bucket: "nowhere", wantErr: domain.ErrBucketNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			objects := f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) { cfg.Usage = tt.usage })
			got, err := objects.Usage(ctx, porttest.Root(), tt.bucket)
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil) != (err == nil) || got != tt.want {
				t.Errorf("Usage = %+v, %v; want %+v, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}

	// The count is no way around the authorizer: it is not read for who
	// may not list the bucket.
	unread := &usageReadFails{BucketUsageStore: f.meta, err: broken}
	objects := f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) { cfg.Usage = unread })
	stranger := iam.Principal{AccessKeyID: "AKIDSTRANGER00000001", User: "stranger"}
	if _, err := objects.Usage(ctx, stranger, "b"); !errors.Is(err, domain.ErrAccessDenied) || unread.reads != 0 {
		t.Errorf("Usage for somebody who may not list the bucket: error = %v after %d reads; want %v and none", err, unread.reads, domain.ErrAccessDenied)
	}
}
