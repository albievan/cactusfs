package app_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// namedUsage answers the record a test gave a bucket by its name, the zero
// record for any other, and counts the calls and the names asked.
type namedUsage struct {
	ports.BucketUsageStore
	records map[string]ports.BucketUsage
	calls   int
	asked   int
	err     error
	// onCall, when set, is called on every call.
	onCall func()
}

func (u *namedUsage) BucketUsages(_ context.Context, names []string) ([]ports.BucketUsage, error) {
	u.calls++
	if u.onCall != nil {
		u.onCall()
	}
	u.asked += len(names)
	if u.err != nil {
		return nil, u.err
	}
	if len(names) > ports.MaxBucketUsages {
		return nil, fmt.Errorf("%w: %d buckets", domain.ErrInvalidArgument, len(names))
	}
	out := make([]ports.BucketUsage, len(names))
	for i, name := range names {
		out[i] = u.records[name]
	}
	return out, nil
}

// unlistedBuckets is a bucket store whose buckets cannot be listed.
type unlistedBuckets struct {
	ports.BucketStore
	err error
}

func (u unlistedBuckets) ListBuckets(context.Context) ([]bucket.Bucket, error) { return nil, u.err }

// TestQuotaSummary: how the buckets stand against their quotas, counted by
// the rule of the store - used is what is stored and what is uploaded, a
// bucket is full when one more byte would be refused - and with each count
// of a quota inside the one before it.
func TestQuotaSummary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exact := func(stored, uploads, quota int64) ports.BucketUsage {
		return ports.BucketUsage{QuotaBytes: quota, Usage: bucket.Usage{Exact: true, UsageTotals: bucket.UsageTotals{StoredBytes: stored, UploadBytes: uploads}}}
	}
	recounting := func(stored, quota int64) ports.BucketUsage {
		r := exact(stored, 0, quota)
		r.Usage.Count = &bucket.UsageCount{Phase: bucket.UsagePhaseKeys}
		return r
	}
	for _, tt := range []struct {
		name    string
		records map[string]ports.BucketUsage
		want    app.QuotaSummary
	}{
		{name: "no buckets"},
		{name: "no quota", records: map[string]ports.BucketUsage{"a": exact(1<<40, 0, 0)}},
		{name: "with room", records: map[string]ports.BucketUsage{"a": exact(89, 0, 100)}, want: app.QuotaSummary{Set: 1}},
		{name: "at 90 %", records: map[string]ports.BucketUsage{"a": exact(90, 0, 100)}, want: app.QuotaSummary{Set: 1, Over90: 1}},
		{name: "90 % rounded up", records: map[string]ports.BucketUsage{"a": exact(13, 0, 15), "b": exact(14, 0, 15)}, want: app.QuotaSummary{Set: 2, Over90: 1}},
		{name: "uploads count", records: map[string]ports.BucketUsage{"a": exact(50, 50, 100)}, want: app.QuotaSummary{Set: 1, Over90: 1, Full: 1}},
		{name: "one byte short of full", records: map[string]ports.BucketUsage{"a": exact(99, 0, 100)}, want: app.QuotaSummary{Set: 1, Over90: 1}},
		{name: "past its quota", records: map[string]ports.BucketUsage{"a": exact(500, 0, 100)}, want: app.QuotaSummary{Set: 1, Over90: 1, Full: 1}},
		{name: "the greatest quota, held at the bound", records: map[string]ports.BucketUsage{"a": exact(math.MaxInt64, 0, bucket.MaxQuotaBytes)}, want: app.QuotaSummary{Set: 1, Over90: 1, Full: 1}},
		{name: "a quota of one byte", records: map[string]ports.BucketUsage{"a": exact(0, 0, 1), "b": exact(1, 0, 1)}, want: app.QuotaSummary{Set: 2, Over90: 1, Full: 1}},
		{name: "counted for the first time", records: map[string]ports.BucketUsage{"a": {}}, want: app.QuotaSummary{Counting: 1}},
		{name: "recounted, full by the totals kept", records: map[string]ports.BucketUsage{"a": recounting(100, 100)}, want: app.QuotaSummary{Set: 1, Over90: 1, Full: 1, Counting: 1}},
		{name: "a quota not judged while not exact", records: map[string]ports.BucketUsage{"a": {QuotaBytes: 1}}, want: app.QuotaSummary{Set: 1, Counting: 1}},
		{name: "every kind", records: map[string]ports.BucketUsage{
			"a": exact(1, 0, 0), "b": exact(10, 0, 100), "c": exact(95, 0, 100), "d": exact(100, 0, 100), "e": {},
		}, want: app.QuotaSummary{Set: 3, Over90: 2, Full: 1, Counting: 1}},
	} {
		meta := porttest.NewMemMetadata()
		for name := range tt.records {
			if err := meta.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: testNow}); err != nil {
				t.Fatal(err)
			}
		}
		usage := &namedUsage{records: tt.records}
		quotas := app.NewQuotaService(app.QuotaServiceConfig{Buckets: meta, Usage: usage, Level: levelOf(meta)})
		got, err := quotas.Summary(ctx)
		if err != nil || got != tt.want {
			t.Errorf("%s: Summary = %+v, %v; want %+v", tt.name, got, err, tt.want)
		}
	}
}

// TestQuotaSummaryOfManyBuckets: the records are read in calls of at most
// ports.MaxBucketUsages buckets, and every bucket is counted.
func TestQuotaSummaryOfManyBuckets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	meta := porttest.NewMemMetadata()
	const n = ports.MaxBucketUsages + 5
	records := map[string]ports.BucketUsage{}
	for i := range n {
		name := fmt.Sprintf("bucket-%05d", i)
		if err := meta.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: testNow}); err != nil {
			t.Fatal(err)
		}
		records[name] = ports.BucketUsage{QuotaBytes: 10, Usage: bucket.Usage{Exact: true, UsageTotals: bucket.UsageTotals{StoredBytes: 10}}}
	}
	usage := &namedUsage{records: records}
	quotas := app.NewQuotaService(app.QuotaServiceConfig{Buckets: meta, Usage: usage, Level: levelOf(meta)})
	got, err := quotas.Summary(ctx)
	if want := (app.QuotaSummary{Set: n, Over90: n, Full: n}); err != nil || got != want {
		t.Errorf("Summary = %+v, %v; want %+v", got, err, want)
	}
	if usage.calls != 2 || usage.asked != n {
		t.Errorf("the records were read in %d calls for %d buckets, want 2 for %d", usage.calls, usage.asked, n)
	}

	// A caller that gives up during the first read is not read for again.
	cancelled, cancel := context.WithCancel(ctx)
	defer cancel()
	usage = &namedUsage{records: records, onCall: cancel}
	quotas = app.NewQuotaService(app.QuotaServiceConfig{Buckets: meta, Usage: usage, Level: levelOf(meta)})
	if got, err := quotas.Summary(cancelled); !errors.Is(err, context.Canceled) || got != (app.QuotaSummary{}) {
		t.Errorf("Summary of a caller that gave up = %+v, %v", got, err)
	}
	if usage.calls != 1 {
		t.Errorf("the records were read in %d calls after the caller gave up in the first, want 1", usage.calls)
	}
}

// TestQuotaSummaryFaults: below metadata level 5, without a level, and
// when the buckets or their records cannot be read, there is no summary -
// and below level 5 the records are not read at all.
func TestQuotaSummaryFaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	broken := errors.New("the state cannot be read")
	old := porttest.NewMemMetadata()
	old.Level = ports.UsageLevel - 1
	for _, tt := range []struct {
		name string
		meta *porttest.MemMetadata
		// unlisted, when set, is what listing the buckets fails with.
		unlisted error
		level    func(context.Context) (int, error)
		usage    *namedUsage
		wantErr  error
		// wantRead says whether the records are read.
		wantRead bool
	}{
		{name: "below level 5", meta: old, level: levelOf(old), usage: &namedUsage{}, wantErr: domain.ErrUnavailable},
		{name: "no level", meta: porttest.NewMemMetadata(), usage: &namedUsage{}, wantErr: domain.ErrUnavailable},
		{name: "a level that cannot be read", meta: porttest.NewMemMetadata(), level: func(context.Context) (int, error) { return 0, broken }, usage: &namedUsage{}, wantErr: broken},
		{name: "buckets that cannot be listed", meta: porttest.NewMemMetadata(), unlisted: broken, usage: &namedUsage{}, wantErr: broken},
		{name: "records that cannot be read", meta: porttest.NewMemMetadata(), usage: &namedUsage{err: broken}, wantErr: broken, wantRead: true},
	} {
		if tt.level == nil && tt.name != "no level" {
			tt.level = levelOf(tt.meta)
		}
		if err := tt.meta.CreateBucket(ctx, bucket.Bucket{Name: "photos", CreatedAt: testNow}); err != nil {
			t.Fatal(err)
		}
		var buckets ports.BucketStore = tt.meta
		if tt.unlisted != nil {
			buckets = unlistedBuckets{BucketStore: tt.meta, err: tt.unlisted}
		}
		quotas := app.NewQuotaService(app.QuotaServiceConfig{Buckets: buckets, Usage: tt.usage, Level: tt.level})
		got, err := quotas.Summary(ctx)
		if !errors.Is(err, tt.wantErr) || got != (app.QuotaSummary{}) {
			t.Errorf("%s: Summary = %+v, %v; want %v", tt.name, got, err, tt.wantErr)
		}
		if read := tt.usage.calls > 0; read != tt.wantRead {
			t.Errorf("%s: the records were read: %t, want %t", tt.name, read, tt.wantRead)
		}
	}
}
