package app_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// TestResyncNeverLiftsFailed: a version that is FAILED is neither sent nor
// given another status by a resync, even when the other deployment holds
// it; and a version that becomes FAILED while a pass walks keeps FAILED -
// the pass reads each status again before it acknowledges.
func TestResyncNeverLiftsFailed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newResyncWorld(t, false)
	// There at the other deployment, and FAILED here.
	failed := w.putIn(t, "k/failed", []byte("failed"), object.SSE{})
	row := w.last(t)
	if _, err := w.replicator.Deliver(ctx, w.target, row); err != nil {
		t.Fatal(err)
	}
	if got := w.settle(t, row, object.ReplicationFailed); got != object.ReplicationFailed {
		t.Fatalf("the version is %q", got)
	}
	// Becomes FAILED while the pass walks: after it was sent.
	late := w.before(t, 1, 1)[0]
	becomes := &interrupting{ResyncSender: w.replicator, during: func() {
		result := delivery.Result{Bucket: late.Bucket, Key: late.Key, Version: late.Ref(), Status: object.ReplicationFailed}
		if _, err := w.meta.AckOutbox(ctx, w.target.Name, delivery.Seq{}, 0, []delivery.Result{result}); err != nil {
			t.Errorf("AckOutbox: %v", err)
		}
	}}
	heads, puts := w.count("HeadVersion"), w.count("PutReplica")
	w.startResync(t)
	if _, err := w.resyncWorker(becomes, nil, nil).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, v := range []object.Version{failed, late} {
		if got := w.statusOf(t, v); got != object.ReplicationFailed {
			t.Errorf("%s is %q after the resync, want FAILED", v.Key, got)
		}
	}
	// The FAILED version was not asked for; the other one was asked for and
	// sent once.
	if h, p := w.count("HeadVersion")-heads, w.count("PutReplica")-puts; h != 1 || p != 1 {
		t.Errorf("%d questions and %d sends, want one of each, for the version that was not FAILED yet", h, p)
	}
	if got := w.resyncOf(t, "photos"); got.State != bucket.ResyncDone || got.Scanned != 2 || got.Sent != 1 {
		t.Errorf("the record = %+v", got)
	}
}

// TestResyncBeginsAgainWhenRowsAreLostMeanwhile: rows the target loses
// while a walk goes on may be behind its cursor; the walk is then not done -
// it begins again from the first version, and is done once a walk lost
// nothing. The versions behind the first cursor arrive.
func TestResyncBeginsAgainWhenRowsAreLostMeanwhile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newResyncWorld(t, false)
	w.before(t, 3, 1)
	// The outbox keeps one row: the target loses the rows of what is put
	// during the walk.
	w.meta.MaxOutboxRows = 1
	var behind []object.Version
	lose := &interrupting{ResyncSender: w.replicator, during: func() {
		for i := range 2 {
			behind = append(behind, w.putIn(t, fmt.Sprintf("a/%d", i), []byte("behind the cursor"), object.SSE{}))
		}
	}}
	w.startResync(t)
	worker := w.resyncWorker(lose, nil, nil)
	stats, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Restarted != 1 || stats.Done != 0 {
		t.Errorf("the first round = %+v, want begun again", stats)
	}
	if got := w.resyncOf(t, "photos"); got != (bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning}) {
		t.Errorf("after rows were lost the record = %+v, want begun again", got)
	}
	if progress, ok := worker.Progress("photos"); !ok || progress.Restarts != 1 {
		t.Errorf("the progress = %+v, %v; want one restart counted", progress, ok)
	}
	stats, err = worker.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.resyncOf(t, "photos"); stats.Done != 1 || got.State != bucket.ResyncDone || got.Scanned != 5 {
		t.Errorf("the second round = %+v, the record %+v; want done, 5 looked at", stats, got)
	}
	for _, v := range behind {
		if _, err := w.dest.GetVersion(ctx, "copies", v.Key, v.Ref()); err != nil {
			t.Errorf("%s, put behind the cursor, is not there: %v", v.Key, err)
		}
	}
	state, err := w.meta.OutboxCursors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range state.Cursors {
		if c.Target == "site-b" && c.ResyncNeeded {
			t.Error("the target still needs a resync")
		}
	}
}

// TestResyncStopsWhereTheConfigurationChanges: a configuration removed while
// a pass walks stops the pass at the version it was at - what came before is
// stored, nothing after is passed over - and the walk waits; when the
// configuration is back the walk goes on from there and leaves no hole.
func TestResyncStopsWhereTheConfigurationChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newResyncWorld(t, false)
	versions := w.before(t, 3, 1)
	raw, err := w.meta.GetBucketConfig(ctx, "photos", bucket.ConfigReplication)
	if err != nil {
		t.Fatal(err)
	}
	config, err := bucket.ParseReplicationConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	removed := &interrupting{ResyncSender: w.replicator, during: func() { w.configure(t, bucket.ReplicationConfig{}) }}
	w.startResync(t)
	worker := w.resyncWorker(removed, nil, nil)
	stats, err := worker.RunOnce(ctx)
	if err != nil || stats.Held != 0 {
		t.Fatalf("the round = %+v, %v", stats, err)
	}
	if got := w.resyncOf(t, "photos"); got.Scanned != 1 || got.Key != versions[0].Key {
		t.Fatalf("the record = %+v, want the first version stored and nothing after", got)
	}
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if progress, ok := worker.Progress("photos"); !ok || progress.Waiting != app.ResyncWaitingConfiguration {
		t.Errorf("the progress = %+v, %v; want waiting for the configuration", progress, ok)
	}
	w.configure(t, config)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := w.resyncOf(t, "photos"); got.State != bucket.ResyncDone || got.Scanned != 3 {
		t.Errorf("the record = %+v, want done, 3 looked at", got)
	}
	for _, v := range versions {
		if _, err := w.dest.GetVersion(ctx, "copies", v.Key, v.Ref()); err != nil {
			t.Errorf("%s is not there: %v", v.Key, err)
		}
	}
}

// countingMeta counts the reads and proposals a resync worker makes.
type countingMeta struct {
	*porttest.MemMetadata
	mu     sync.Mutex
	counts map[string]int
}

func (c *countingMeta) add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[name]++
}

func (c *countingMeta) take() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.counts
	c.counts = map[string]int{}
	return out
}

func (c *countingMeta) ListBuckets(ctx context.Context) ([]bucket.Bucket, error) {
	c.add("ListBuckets")
	return c.MemMetadata.ListBuckets(ctx)
}

func (c *countingMeta) GetResync(ctx context.Context, name string) (bucket.Resync, error) {
	c.add("GetResync")
	return c.MemMetadata.GetResync(ctx, name)
}

func (c *countingMeta) PutResync(ctx context.Context, r bucket.Resync) error {
	c.add("PutResync")
	return c.MemMetadata.PutResync(ctx, r)
}

func (c *countingMeta) GetBucketConfig(ctx context.Context, name, kind string) ([]byte, error) {
	c.add("GetBucketConfig " + name)
	c.add("GetBucketConfig")
	return c.MemMetadata.GetBucketConfig(ctx, name, kind)
}

// counted is a resync worker of the world that reads and proposes through
// a countingMeta.
func (w *replicatorWorld) counted(sender app.ResyncSender) (*app.ResyncWorker, *countingMeta) {
	if sender == nil {
		sender = w.replicator
	}
	c := &countingMeta{MemMetadata: w.meta, counts: map[string]int{}}
	return app.NewResyncWorker(app.ResyncWorkerConfig{
		Buckets: c, Index: w.meta, Store: c, Targets: w.meta, Outbox: w.meta, Sender: sender, Clock: w.clock,
	}), c
}

// TestResyncReadsWhatItMust: what a round reads and proposes does not grow
// with the buckets there are. No bucket is read while none has a
// replication configuration; between full scans only the running resyncs
// are read again; a round where nothing moves proposes nothing.
func TestResyncReadsWhatItMust(t *testing.T) {
	t.Parallel()
	t.Run("no configuration anywhere", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		w := newResyncWorld(t, false)
		w.startResync(t)
		w.configure(t, bucket.ReplicationConfig{})
		for i := range 5 {
			w.createBucket(t, fmt.Sprintf("other-%d", i))
		}
		worker, counts := w.counted(nil)
		if _, err := worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if got := counts.take(); got["ListBuckets"] != 0 || got["GetResync"] != 0 {
			t.Errorf("a round with no configuration read %v", got)
		}
	})
	t.Run("between full scans", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		w := newResyncWorld(t, false)
		w.before(t, 300, 2)
		for i := range 5 {
			w.createBucket(t, fmt.Sprintf("other-%d", i))
		}
		w.startResync(t)
		worker, counts := w.counted(nil)
		if _, err := worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if got := counts.take(); got["ListBuckets"] != 1 || got["GetResync"] != 2 {
			t.Errorf("the first round, a full scan, read %v", got)
		}
		if _, err := worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		// The running resync, and its record again before it is stored.
		if got := counts.take(); got["ListBuckets"] != 0 || got["GetResync"] != 2 {
			t.Errorf("a round between full scans read %v", got)
		}
		w.clock.Advance(app.ResyncScanEvery)
		if _, err := worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if got := counts.take(); got["ListBuckets"] != 1 {
			t.Errorf("a round after %s read %v, want a full scan", app.ResyncScanEvery, got)
		}
	})
	t.Run("walks that wait for another", func(t *testing.T) {
		t.Parallel()
		testResyncWaitingRounds(t)
	})
}

// heldFor fails every version of one bucket with a class that holds.
type heldFor struct {
	app.ResyncSender
	bucket string
}

func (h heldFor) Resend(ctx context.Context, target delivery.Target, v object.Version) (object.ReplicationStatus, bool, error) {
	if v.Bucket == h.bucket {
		return "", false, siteFailure(delivery.ClassAuth)
	}
	return h.ResyncSender.Resend(ctx, target, v)
}

// testResyncWaitingRounds: three buckets sent to a target that needs a
// resync, one of them held: the two that are through wait for it, read the
// buckets sent to the target once a round between them, read no
// configuration of a bucket the target does not admit, and propose nothing;
// the round counts as one that moved nothing.
func testResyncWaitingRounds(t *testing.T) {
	ctx := context.Background()
	w := newResyncWorld(t, false)
	if err := w.meta.PutTarget(ctx, siteRecord("site-b", "photos", "cards", "maps"), audit.Record{}); err != nil {
		t.Fatal(err)
	}
	w.createBucket(t, "elsewhere")
	for _, name := range []string{"cards", "maps"} {
		if err := w.meta.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: testNow}); err != nil {
			t.Fatal(err)
		}
		if err := w.meta.SetBucketVersioning(ctx, name, bucket.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
		document, err := bucket.ReplicationConfig{Target: "site-b", Bucket: "copies-" + name, Rules: []bucket.ReplicationRule{{ID: "all", Enabled: true}}}.Document()
		if err != nil {
			t.Fatal(err)
		}
		if err := w.meta.SetBucketConfig(ctx, name, bucket.ConfigReplication, document); err != nil {
			t.Fatal(err)
		}
		if err := w.dest.CreateBucket(ctx, bucket.Bucket{Name: "copies-" + name, CreatedAt: testNow}); err != nil {
			t.Fatal(err)
		}
		if err := w.dest.SetBucketVersioning(ctx, "copies-"+name, bucket.VersioningEnabled); err != nil {
			t.Fatal(err)
		}
		if err := w.dest.SetBucketReplica(ctx, "copies-"+name, sendingSource, audit.Record{}); err != nil {
			t.Fatal(err)
		}
		w.put(t, name, "k", []byte(name))
	}
	// The target loses rows, and needs a resync.
	w.meta.MaxOutboxRows = 1
	for i := range 2 {
		w.putIn(t, fmt.Sprintf("k/%d", i), []byte("lost"), object.SSE{})
	}
	w.startResync(t)
	worker, counts := w.counted(heldFor{ResyncSender: w.replicator, bucket: "cards"})
	for range 2 {
		if _, err := worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for name, waiting := range map[string]string{"photos": app.ResyncWaitingOthers, "maps": app.ResyncWaitingOthers, "cards": ""} {
		if progress, ok := worker.Progress(name); !ok || progress.Waiting != waiting {
			t.Fatalf("%s: %+v, %v; want waiting for %q", name, progress, ok, waiting)
		}
	}
	counts.take()
	stats, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := counts.take()
	if stats.Scanned != 0 || stats.Done != 0 || stats.Started != 0 || stats.Restarted != 0 {
		t.Errorf("a round where nothing moves = %+v", stats)
	}
	if got["PutResync"] != 0 {
		t.Errorf("a round where nothing moves proposed %d records", got["PutResync"])
	}
	// One read of each running resync - photos, cards, maps - and none more
	// for the two that wait for the third: what they need of each other the
	// round read once.
	if got["GetResync"] != 3 {
		t.Errorf("the records were read %d times in a round, want 3", got["GetResync"])
	}
	// One read of the buckets, for the target both walks that wait share.
	if got["ListBuckets"] != 1 {
		t.Errorf("the buckets were listed %d times in a round, want once", got["ListBuckets"])
	}
	if got["GetBucketConfig elsewhere"] != 0 {
		t.Errorf("the configuration of a bucket the target does not admit was read %d times", got["GetBucketConfig elsewhere"])
	}
}
