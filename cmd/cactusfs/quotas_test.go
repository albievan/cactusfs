package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// TestANodeWiresQuotas holds the composition root to the wirings of
// ADR-0023 and ADR-0024 that no use case can see from inside: the object
// service is given the usage records - its usage is exact - and the quota
// use case, whose pre-check refuses a write past a quota and counts it in
// cactusfs_quota_refusals_total; the quota use case is given the level of
// the store, without which it answers unavailable; the gauges of quotas
// read it when the node is scraped; and the recount is the counter's.
func TestANodeWiresQuotas(t *testing.T) {
	t.Parallel()
	n := mustStart(t, nodeConfig(t.TempDir())).node
	ctx := t.Context()
	root := iam.Principal{AccessKeyID: "ROOT", Root: true}
	for _, name := range []string{"photos", "logs"} {
		if err := n.buckets.Create(ctx, root, name); err != nil {
			t.Fatal(err)
		}
	}
	put := func(key, content string) error {
		_, err := n.objects.Put(ctx, root, app.PutInput{Bucket: "photos", Key: key, Body: bytes.NewReader([]byte(content)), Size: int64(len(content))})
		return err
	}

	got, err := n.quotas.Set(ctx, root, "photos", 10)
	if err != nil || got != (app.BucketQuota{QuotaBytes: 10, Exact: true}) {
		t.Fatalf("Set = %+v, %v", got, err)
	}
	if err := put("a", "123456789"); err != nil {
		t.Fatal(err)
	}
	if err := put("b", "12"); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("a put past the quota = %v", err)
	}
	usage, err := n.objects.Usage(ctx, root, "photos")
	if err != nil || !usage.Exact || usage.StoredBytes != 9 || usage.QuotaBytes != 10 {
		t.Errorf("Usage = %+v, %v; want the exact record", usage, err)
	}
	if err := put("c", "1"); err != nil {
		t.Fatal(err)
	}

	// One scrape: the gauges of quotas are shown again for a while once
	// read (quotaGaugesReuse).
	metrics := samples(t, n.telemetry)
	for name, want := range map[string]float64{
		`cactusfs_quota_refusals_total{}`:       1,
		`cactusfs_buckets_quota{state=set}`:     1,
		`cactusfs_buckets_quota{state=over_90}`: 1,
		`cactusfs_buckets_quota{state=full}`:    1,
		`cactusfs_bucket_usage_counting{}`:      0,
		`cactusfs_bucket_usage_drift_total{}`:   0,
	} {
		if got, ok := metrics[name]; !ok || got != want {
			t.Errorf("%s = %v (exposed: %v), want %v", name, got, ok, want)
		}
	}

	if err := n.counter.Recount(ctx, root, "logs"); err != nil {
		t.Errorf("Recount = %v", err)
	}
	if err := n.quotas.Delete(ctx, root, "photos"); err != nil {
		t.Fatal(err)
	}
	if err := put("d", "123"); err != nil {
		t.Errorf("a put once the quota is removed = %v", err)
	}
}

// TestANodeCountsUsageInTheBackground holds the composition root to the
// counter it runs (ADR-0023): a bucket whose record is being counted again
// - one entry of a recount, of one record, was committed and no more - is
// counted to the end by the node's background work alone, on the node that
// leads, and cactusfs_bucket_usage_counting comes back to 0. Without the
// counter running, the record stays as it was.
func TestANodeCountsUsageInTheBackground(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cfg := nodeConfig(t.TempDir())
	n := mustStart(t, cfg).node
	root := iam.Principal{AccessKeyID: "ROOT", Root: true}
	if err := n.buckets.Create(ctx, root, "photos"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a", "b", "c"} {
		if _, err := n.objects.Put(ctx, root, app.PutInput{Bucket: "photos", Key: key, Body: bytes.NewReader([]byte("1234")), Size: 4}); err != nil {
			t.Fatal(err)
		}
	}
	walk, err := n.meta.WalkBucketUsage(ctx, "photos", 1, true)
	if err != nil || !walk.Usage.Walking() {
		t.Fatalf("the first entry of a recount = %+v, %v; want a walk under way", walk, err)
	}
	if s, err := n.quotas.Summary(ctx); err != nil || s.Counting != 1 {
		t.Fatalf("Summary while counted = %+v, %v", s, err)
	}

	n.startBackground(ctx, cfg)
	deadline := time.Now().Add(15 * time.Second)
	for {
		records, err := n.meta.BucketUsages(ctx, []string{"photos"})
		if err != nil {
			t.Fatal(err)
		}
		if u := records[0].Usage; u.Exact && !u.Walking() {
			if u.StoredBytes != 12 {
				t.Errorf("the count holds %d bytes, want 12", u.StoredBytes)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the bucket is still counted after 15 s: %+v", records[0].Usage)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, ok := samples(t, n.telemetry)["cactusfs_bucket_usage_counting{}"]; !ok || got != 0 {
		t.Errorf("cactusfs_bucket_usage_counting = %v (exposed: %v) once the count is done, want 0", got, ok)
	}
}

// countedSummaries is a usage store that counts the calls that read the
// records, and fails them when told to.
type countedSummaries struct {
	ports.BucketUsageStore
	reads atomic.Int32
	fail  atomic.Bool
}

func (c *countedSummaries) BucketUsages(ctx context.Context, names []string) ([]ports.BucketUsage, error) {
	c.reads.Add(1)
	if c.fail.Load() {
		return nil, errors.New("the state cannot be read")
	}
	return c.BucketUsageStore.BucketUsages(ctx, names)
}

// TestQuotaGaugesAreReused: the gauges of quotas show a summary again for
// quotaGaugesReuse, by the node's clock, before the buckets are read anew;
// a summary that cannot be made is not shown, and an older one does not
// take its place.
func TestQuotaGaugesAreReused(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	meta := porttest.NewMemMetadata()
	for _, name := range []string{"a", "b"} {
		if err := meta.CreateBucket(ctx, bucket.Bucket{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := meta.SetBucketQuota(ctx, "a", 10, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	usage := &countedSummaries{BucketUsageStore: meta}
	clock := porttest.NewClock(time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))
	gauges := &quotaGauges{
		quotas: app.NewQuotaService(app.QuotaServiceConfig{
			Buckets: meta, Usage: usage, Level: func(context.Context) (int, error) { return meta.MetadataLevel() },
		}),
		clock: clock,
	}
	ask := func(step string, want telemetry.QuotaState, known bool, reads int32) {
		t.Helper()
		got, ok := gauges.QuotaState(ctx)
		if got != want || ok != known || usage.reads.Load() != reads {
			t.Errorf("%s: QuotaState = %+v, %t after %d reads; want %+v, %t after %d", step, got, ok, usage.reads.Load(), want, known, reads)
		}
	}
	one := telemetry.QuotaState{Set: 1}
	ask("the first", one, true, 1)
	if err := meta.SetBucketQuota(ctx, "b", 10, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	ask("at once", one, true, 1)
	clock.Advance(quotaGaugesReuse - time.Nanosecond)
	ask("just before the end of the reuse", one, true, 1)
	clock.Advance(time.Nanosecond)
	two := telemetry.QuotaState{Set: 2}
	ask("at the end of the reuse", two, true, 2)
	clock.Advance(quotaGaugesReuse)
	usage.fail.Store(true)
	ask("a summary that cannot be made", telemetry.QuotaState{}, false, 3)
	ask("and again", telemetry.QuotaState{}, false, 4)
	usage.fail.Store(false)
	ask("made again", two, true, 5)
	clock.Advance(-time.Second)
	ask("a clock that went back", two, true, 6)
}

// BenchmarkQuotaSummary measures a summary of the quotas over the metadata
// store a node runs, with many buckets - a tenth of them with a quota - so
// that what a scrape waits for, and how often a node does it, is known
// (PLAN 8.3.3). The number of buckets is CACTUSFS_BENCH_BUCKETS, 10 000
// unless that says otherwise.
func BenchmarkQuotaSummary(b *testing.B) {
	buckets := 10_000
	if v := os.Getenv("CACTUSFS_BENCH_BUCKETS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			b.Fatalf("CACTUSFS_BENCH_BUCKETS=%q", v)
		}
		buckets = n
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	store, err := raftmeta.Open(ctx, raftmeta.Config{
		Dir: b.TempDir(), Logger: slog.New(slog.DiscardHandler), Entropy: rand.Reader, Timing: nodeConfig("").raftTiming,
	})
	cancel()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	// Made by many at once, which Raft commits in batches. A worker that
	// fails goes on taking names, so that the sender is never held.
	var (
		workers sync.WaitGroup
		mu      sync.Mutex
		failed  error
	)
	names := make(chan int)
	for range 64 {
		workers.Go(func() {
			for i := range names {
				name := fmt.Sprintf("bucket-%07d", i)
				err := store.CreateBucket(b.Context(), bucket.Bucket{Name: name, CreatedAt: time.Unix(1, 0)})
				if err == nil && i%10 == 0 {
					err = store.SetBucketQuota(b.Context(), name, 1<<30, audit.Record{})
				}
				if err != nil {
					mu.Lock()
					failed = errors.Join(failed, err)
					mu.Unlock()
				}
			}
		})
	}
	for i := range buckets {
		names <- i
	}
	close(names)
	workers.Wait()
	if failed != nil {
		b.Fatal(failed)
	}
	quotas := app.NewQuotaService(app.QuotaServiceConfig{Buckets: store, Usage: store, Level: metadataLevel(store)})
	b.ReportAllocs()
	for b.Loop() {
		s, err := quotas.Summary(b.Context())
		if err != nil || s.Set != (buckets+9)/10 {
			b.Fatalf("Summary = %+v, %v", s, err)
		}
	}
	b.ReportMetric(float64(buckets), "buckets")
}
