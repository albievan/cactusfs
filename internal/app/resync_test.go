package app_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// newResyncWorld is the world of the replicator with its bucket named
// "photos": a resync is stored only of a bucket with a valid name.
func newResyncWorld(t *testing.T, markers bool) *replicatorWorld {
	t.Helper()
	return newReplicatorWorldOf(t, markers, true, "photos")
}

// resyncStore counts the records of resyncs stored, as passes.
type resyncStore struct {
	ports.ReplicationStore
	mu     sync.Mutex
	stored []bucket.Resync
}

func (s *resyncStore) PutResync(ctx context.Context, r bucket.Resync) error {
	if err := s.ReplicationStore.PutResync(ctx, r); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stored = append(s.stored, r)
	return nil
}

func (s *resyncStore) records() []bucket.Resync {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bucket.Resync(nil), s.stored...)
}

// resyncWorker returns a worker of the world that sends through sender (the
// world's replicator when nil) and stores through store (the world's
// metadata when nil), on a node that leads while active says so (always
// when nil).
func (w *replicatorWorld) resyncWorker(sender app.ResyncSender, store ports.ReplicationStore, active func() bool) *app.ResyncWorker {
	if sender == nil {
		sender = w.replicator
	}
	if store == nil {
		store = w.meta
	}
	return app.NewResyncWorker(app.ResyncWorkerConfig{
		Buckets: w.meta, Index: w.meta, Store: store, Targets: w.meta, Outbox: w.meta, Sender: sender,
		Active: active, Clock: w.clock,
		AttemptTimeout: app.ReplicationAttemptTimeout(time.Minute, 0, 4),
	})
}

// before puts versions while "photos" has no replication configuration - so with
// no status and no row of the outbox, as versions that were there before
// the configuration - and puts the configuration back. Each key is given
// perKey versions; it returns them all.
func (w *replicatorWorld) before(t *testing.T, keys, perKey int) []object.Version {
	t.Helper()
	ctx := context.Background()
	raw, err := w.meta.GetBucketConfig(ctx, "photos", bucket.ConfigReplication)
	if err != nil {
		t.Fatal(err)
	}
	config, err := bucket.ParseReplicationConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	w.configure(t, bucket.ReplicationConfig{})
	var out []object.Version
	for k := range keys {
		for n := range perKey {
			out = append(out, w.putIn(t, fmt.Sprintf("k/%04d", k), fmt.Appendf(nil, "version %d of %d", n, k), object.SSE{}))
		}
	}
	w.configure(t, config)
	return out
}

// startResync stores the start of a resync of "photos", as the use case does.
func (w *replicatorWorld) startResync(t *testing.T) {
	t.Helper()
	if err := w.meta.PutResync(context.Background(), bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning}); err != nil {
		t.Fatalf("PutResync: %v", err)
	}
}

// resyncOf returns the stored record of the resync of a bucket.
func (w *replicatorWorld) resyncOf(t *testing.T, name string) bucket.Resync {
	t.Helper()
	r, err := w.meta.GetResync(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// statusOf returns the replication status of a version here.
func (w *replicatorWorld) statusOf(t *testing.T, v object.Version) object.ReplicationStatus {
	t.Helper()
	got, err := w.meta.GetVersion(context.Background(), "photos", v.Key, v.Ref())
	if err != nil {
		t.Fatal(err)
	}
	return got.Replication
}

// TestResyncWalksInPasses: a bucket of 1 200 versions needs three passes of
// 500, 500 and 200; the cursor is stored after each, the last as done. Each
// version is sent once, arrives with its ID, and is COMPLETED here; the
// counts say what was looked at and sent.
func TestResyncWalksInPasses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newResyncWorld(t, false)
	versions := w.before(t, 400, 3)
	if len(versions) != 1200 {
		t.Fatalf("%d versions", len(versions))
	}
	w.startResync(t)
	store := &resyncStore{ReplicationStore: w.meta}
	worker := w.resyncWorker(nil, store, nil)
	wantScanned := []uint64{500, 1000, 1200}
	for pass, want := range wantScanned {
		stats, err := worker.RunOnce(ctx)
		if err != nil {
			t.Fatalf("pass %d: %v", pass+1, err)
		}
		if stats.Passes != 1 {
			t.Errorf("pass %d: %+v", pass+1, stats)
		}
		got := w.resyncOf(t, "photos")
		wantState := bucket.ResyncRunning
		if pass == 2 {
			wantState = bucket.ResyncDone
		}
		if got.State != wantState || got.Scanned != want || got.Sent != want {
			t.Errorf("after pass %d the record = %+v, want %s, %d looked at and sent", pass+1, got, wantState, want)
		}
	}
	if records := store.records(); len(records) != 3 {
		t.Errorf("%d records stored, want one a pass: %+v", len(records), records)
	}
	if stats, err := worker.RunOnce(ctx); err != nil || stats.Passes != 0 {
		t.Errorf("a round after the end = %+v, %v; want nothing", stats, err)
	}
	if n := w.count("PutReplica"); n != 1200 {
		t.Errorf("%d versions sent, want 1200", n)
	}
	for _, v := range versions {
		if got := w.statusOf(t, v); got != object.ReplicationCompleted {
			t.Fatalf("%s %s is %q, want COMPLETED", v.Key, v.VersionID, got)
		}
		if _, err := w.dest.GetVersion(ctx, "copies", v.Key, v.Ref()); err != nil {
			t.Fatalf("%s %s is not there: %v", v.Key, v.VersionID, err)
		}
	}
}

// interrupting is a sender that loses the node its lead after a number of
// versions sent, and calls along the way.
type interrupting struct {
	app.ResyncSender
	lead  *atomic.Bool
	after int
	n     atomic.Int64
	// during, when not nil, is called once, after the first version.
	during func()
	once   sync.Once
}

func (s *interrupting) Resend(ctx context.Context, target delivery.Target, v object.Version) (object.ReplicationStatus, bool, error) {
	status, sent, err := s.ResyncSender.Resend(ctx, target, v)
	if s.during != nil {
		s.once.Do(s.during)
	}
	if s.after > 0 && s.n.Add(1) == int64(s.after) {
		s.lead.Store(false)
	}
	return status, sent, err
}

// TestResyncGoesOnFromItsCursor: a node that comes to lead goes on from the
// cursor stored by the last pass, and sends nothing twice that was
// acknowledged. A pass cut off by the loss of the lead stores nothing: the
// next leader walks it again, asks for each version, finds those that
// arrived and sends only the others - so no version is sent twice at all.
func TestResyncGoesOnFromItsCursor(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		// cutAfter is how many versions of the second pass are sent before
		// the lead is lost; 0 loses none.
		cutAfter int
		wantSent uint64
	}{
		"between two passes":         {cutAfter: 0, wantSent: 1200},
		"in the middle of a pass":    {cutAfter: 200, wantSent: 1000},
		"after the last of its pass": {cutAfter: 500, wantSent: 700},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			w := newResyncWorld(t, false)
			versions := w.before(t, 600, 2)
			w.startResync(t)
			// The first leader makes one pass and stores it.
			if _, err := w.resyncWorker(nil, nil, nil).RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if got := w.resyncOf(t, "photos"); got.Scanned != 500 || got.State != bucket.ResyncRunning {
				t.Fatalf("after the first pass: %+v", got)
			}
			if tt.cutAfter > 0 {
				// The second leader loses its lead within its pass.
				var lead atomic.Bool
				lead.Store(true)
				cut := &interrupting{ResyncSender: w.replicator, lead: &lead, after: tt.cutAfter}
				_, err := w.resyncWorker(cut, nil, lead.Load).RunOnce(ctx)
				if !errors.Is(err, domain.ErrNotLeader) {
					t.Fatalf("a pass whose lead was lost = %v", err)
				}
				if got := w.resyncOf(t, "photos"); got.Scanned != 500 {
					t.Errorf("a pass whose lead was lost stored %+v", got)
				}
			}
			// The next leader, which knows nothing of the others, ends it.
			next := w.resyncWorker(nil, nil, nil)
			for range 3 {
				if _, err := next.RunOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}
			got := w.resyncOf(t, "photos")
			if got.State != bucket.ResyncDone || got.Scanned != 1200 || got.Sent != tt.wantSent {
				t.Errorf("the record = %+v, want done, 1200 looked at, %d counted as sent", got, tt.wantSent)
			}
			if n := w.count("PutReplica"); n != 1200 {
				t.Errorf("%d versions sent, want each of the 1200 once", n)
			}
			for _, v := range versions {
				if got := w.statusOf(t, v); got != object.ReplicationCompleted {
					t.Fatalf("%s is %q", v.Key, got)
				}
			}
		})
	}
}

// TestResyncAskedAgainBeginsAgain: a resync asked again while one is walked
// begins at the first version, also when it is asked in the middle of a
// pass: that pass stores nothing over it. What it walks again is there and
// is not sent again.
func TestResyncAskedAgainBeginsAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newResyncWorld(t, false)
	w.before(t, 350, 2)
	w.startResync(t)
	worker := w.resyncWorker(nil, nil, nil)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	// Asked again within the second pass.
	again := &interrupting{ResyncSender: w.replicator, during: func() { w.startResync(t) }}
	worker = w.resyncWorker(again, nil, nil)
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := w.resyncOf(t, "photos"); got != (bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning}) {
		t.Fatalf("the pass stored over a resync asked again: %+v", got)
	}
	for range 2 {
		if _, err := worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.resyncOf(t, "photos"); got.State != bucket.ResyncDone || got.Scanned != 700 || got.Sent != 0 {
		t.Errorf("the resync asked again = %+v, want done, 700 looked at, none sent", got)
	}
	if n := w.count("PutReplica"); n != 700 {
		t.Errorf("%d versions sent, want 700", n)
	}
}

// TestResyncSendsWhatIsMissing: what a resync sends and what it says of each
// version, by the rules of the configuration (one rule, docs/, that copies
// markers): a version from before the configuration is sent and COMPLETED;
// one there already is not sent and COMPLETED; one of a customer's key is
// FAILED and not sent; one no rule matches is neither sent nor given a
// status; a marker made under the configuration is sent; a marker without a
// status - made before the configuration, or by the lifecycle service - is
// not.
func TestResyncSendsWhatIsMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newResyncWorld(t, true)
	docs := bucket.ReplicationConfig{Target: "site-b", Bucket: "copies", Rules: []bucket.ReplicationRule{{ID: "docs", Enabled: true, Prefix: "docs/", Markers: true}}}
	w.configure(t, docs)
	type version struct {
		key  string
		make func() object.Version
		// sent is whether the resync sends it; status what it is after.
		sent   bool
		status object.ReplicationStatus
	}
	del := func(key string) object.Version {
		out, err := w.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "photos", Key: key})
		if err != nil {
			t.Fatal(err)
		}
		if out.Marker == nil {
			t.Fatalf("delete of %s made no marker", key)
		}
		v, err := w.meta.GetVersion(ctx, "photos", key, out.Marker.Ref())
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	without := func(make func() object.Version) func() object.Version {
		return func() object.Version {
			w.configure(t, bucket.ReplicationConfig{})
			defer w.configure(t, docs)
			return make()
		}
	}
	rows := []version{
		{
			key: "docs/before", sent: true, status: object.ReplicationCompleted,
			make: without(func() object.Version { return w.putIn(t, "docs/before", []byte("before"), object.SSE{}) }),
		},
		{key: "docs/there", sent: false, status: object.ReplicationCompleted, make: func() object.Version {
			v := w.putIn(t, "docs/there", []byte("there"), object.SSE{})
			// The outbox sent it, and its acknowledgement was lost.
			if _, err := w.replicator.Deliver(context.Background(), w.target, w.last(t)); err != nil {
				t.Fatal(err)
			}
			return v
		}},
		{key: "docs/secret", sent: false, status: object.ReplicationFailed, make: without(func() object.Version {
			return w.putIn(t, "docs/secret", []byte("secret"), object.SSE{Mode: object.SSEC, CustomerKey: customerKey(3)})
		})},
		{
			key: "other/x", sent: false, status: "",
			make: without(func() object.Version { return w.putIn(t, "other/x", []byte("x"), object.SSE{}) }),
		},
		{key: "docs/marked", sent: true, status: object.ReplicationCompleted, make: func() object.Version {
			w.putIn(t, "docs/marked", []byte("marked"), object.SSE{})
			return del("docs/marked")
		}},
		{key: "docs/old-marker", sent: false, status: "", make: without(func() object.Version {
			w.putIn(t, "docs/old-marker", []byte("old"), object.SSE{})
			return del("docs/old-marker")
		})},
	}
	made := make([]object.Version, len(rows))
	for i, row := range rows {
		made[i] = row.make()
	}
	before := map[string]int{"PutReplica": w.count("PutReplica"), "PutMarker": w.count("PutMarker")}
	w.startResync(t)
	if _, err := w.resyncWorker(nil, nil, nil).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sentNow := map[string]int{"PutReplica": w.count("PutReplica") - before["PutReplica"], "PutMarker": w.count("PutMarker") - before["PutMarker"]}
	// The version from before, and the versions under the two markers - a
	// rule matches each, and neither had crossed - and the one marker.
	if sentNow["PutReplica"] != 3 || sentNow["PutMarker"] != 1 {
		t.Errorf("sent %v, want three versions and one marker", sentNow)
	}
	for i, row := range rows {
		if got := w.statusOf(t, made[i]); got != row.status {
			t.Errorf("%s: status %q, want %q", row.key, got, row.status)
		}
		_, err := w.dest.GetVersion(ctx, "copies", row.key, made[i].Ref())
		there := err == nil
		if wantThere := row.sent || row.key == "docs/there"; there != wantThere {
			t.Errorf("%s: there = %v (%v), want %v", row.key, there, err, wantThere)
		}
	}
	got := w.resyncOf(t, "photos")
	// Every version of the bucket is looked at: the six made and the
	// versions under the two markers.
	if got.State != bucket.ResyncDone || got.Scanned != 8 || got.Sent != 4 {
		t.Errorf("the record = %+v, want done, 8 looked at, 4 sent", got)
	}
	// Asked again: everything is there, the marker too - nothing is sent.
	w.startResync(t)
	if _, err := w.resyncWorker(nil, nil, nil).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := w.resyncOf(t, "photos"); got.State != bucket.ResyncDone || got.Scanned != 8 || got.Sent != 0 {
		t.Errorf("the resync asked again = %+v, want done, 8 looked at, none sent", got)
	}
	if again := w.count("PutReplica") + w.count("PutMarker"); again != before["PutReplica"]+before["PutMarker"]+4 {
		t.Errorf("the resync asked again sent %d more", again-before["PutReplica"]-before["PutMarker"]-4)
	}
}

// openedTo reads every target as open to the buckets named only.
type openedTo struct {
	ports.TargetStore
	buckets []string
}

func (o openedTo) ListTargets(ctx context.Context) ([]delivery.Target, error) {
	targets, err := o.TargetStore.ListTargets(ctx)
	for i := range targets {
		targets[i].Buckets = o.buckets
	}
	return targets, err
}

// TestResyncWaits: a resync whose bucket's configuration no longer names its
// target, or whose target no longer admits the bucket, waits - nothing is
// sent, the record stays as it is, and the leader shows why. While no bucket
// has a configuration at all, or no target named by one admits the bucket,
// its record is not read and nothing is shown.
func TestResyncWaits(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		change func(t *testing.T, w *replicatorWorld) ports.TargetStore
		// want is why the leader shows it waits; empty: it shows nothing.
		want string
	}{
		"the configuration removed, the only one": {change: func(t *testing.T, w *replicatorWorld) ports.TargetStore {
			w.configure(t, bucket.ReplicationConfig{})
			return w.meta
		}},
		"the configuration names another target": {want: app.ResyncWaitingConfiguration, change: func(t *testing.T, w *replicatorWorld) ports.TargetStore {
			if err := w.meta.PutTarget(context.Background(), siteRecord("site-c", "photos"), audit.Record{}); err != nil {
				t.Fatal(err)
			}
			w.configure(t, bucket.ReplicationConfig{Target: "site-c", Bucket: "copies", Rules: []bucket.ReplicationRule{{ID: "all", Enabled: true}}})
			return w.meta
		}},
		"the target read as no longer open to the bucket, the only one": {change: func(_ *testing.T, w *replicatorWorld) ports.TargetStore {
			return openedTo{TargetStore: w.meta, buckets: []string{"elsewhere"}}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			w := newResyncWorld(t, false)
			w.before(t, 3, 1)
			w.startResync(t)
			targets := tt.change(t, w)
			worker := app.NewResyncWorker(app.ResyncWorkerConfig{
				Buckets: w.meta, Index: w.meta, Store: w.meta, Targets: targets, Outbox: w.meta, Sender: w.replicator, Clock: w.clock,
			})
			stats, err := worker.RunOnce(ctx)
			if err != nil || stats.Passes != 0 {
				t.Fatalf("a round of a resync that waits = %+v, %v", stats, err)
			}
			if progress, ok := worker.Progress("photos"); ok != (tt.want != "") || progress.Waiting != tt.want {
				t.Errorf("the progress = %+v, %v; want waiting for %q", progress, ok, tt.want)
			}
			if n := len(w.site.Calls()); n != 0 {
				t.Errorf("a resync that waits made %d calls", n)
			}
			if got := w.resyncOf(t, "photos"); got != (bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning}) {
				t.Errorf("a resync that waits stored %+v", got)
			}
		})
	}
}

// failingSite is a site whose PutReplica fails with a class while fail is
// set.
type failingSite struct {
	*porttest.MemSite
	fail atomic.Pointer[delivery.Class]
}

func (s *failingSite) PutReplica(ctx context.Context, v object.Version, body io.Reader) error {
	if class := s.fail.Load(); class != nil {
		_, _ = io.Copy(io.Discard, body)
		return siteFailure(*class)
	}
	return s.MemSite.PutReplica(ctx, v, body)
}

// TestResyncIsHeldWhereTheTargetIsHeld: a failure that holds the target
// holds the resync at the version: what the pass dealt with before it is
// stored, the walk shows the class and the state of the target, it is not
// tried again before its pause, and then goes on from there. A failure of
// the version itself (a refusal, rejected) passes it: FAILED, and the walk
// goes on.
func TestResyncIsHeldWhereTheTargetIsHeld(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		class     delivery.Class
		wantState string
		held      bool
	}{
		"a key the other deployment refuses": {class: delivery.ClassAuth, wantState: ports.DeliveryStalled, held: true},
		"the other deployment is full":       {class: delivery.ClassQuota, wantState: ports.DeliveryStalled, held: true},
		"a failure of the network":           {class: delivery.ClassConnect, wantState: ports.DeliveryRetrying, held: true},
		"a refusal of the version":           {class: delivery.ClassRejected},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			w := newResyncWorld(t, false)
			versions := w.before(t, 5, 1)
			site := &failingSite{MemSite: w.site}
			w.replicator = w.replicatorWith(site, 0)
			w.startResync(t)
			// The first two are taken, the third fails.
			stats, err := w.resyncWorker(&classAfter{ResyncSender: w.replicator, site: site, after: 2, class: tt.class}, nil, nil).RunOnce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !tt.held {
				got := w.resyncOf(t, "photos")
				if stats.Held != 0 || got.State != bucket.ResyncDone || got.Scanned != 5 || got.Sent != 4 {
					t.Errorf("stats %+v, record %+v; want done, 5 looked at, 4 sent", stats, got)
				}
				for i, v := range versions {
					want := object.ReplicationCompleted
					if i == 2 {
						want = object.ReplicationFailed
					}
					if got := w.statusOf(t, v); got != want {
						t.Errorf("version %d is %q, want %q", i, got, want)
					}
				}
				return
			}
			got := w.resyncOf(t, "photos")
			if stats.Held != 1 || got.State != bucket.ResyncRunning || got.Scanned != 2 || got.Key != versions[1].Key {
				t.Fatalf("stats %+v, record %+v; want held at the third version, two stored", stats, got)
			}
			for i, v := range versions[:2] {
				if got := w.statusOf(t, v); got != object.ReplicationCompleted {
					t.Errorf("version %d before the failure is %q", i, got)
				}
			}
			// The class of the failure is shown while the walk is held.
			failing := &classAfter{ResyncSender: w.replicator, site: site, after: 0, class: tt.class}
			worker := w.resyncWorker(failing, nil, nil)
			for range 2 {
				if _, err := worker.RunOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}
			progress, ok := worker.Progress("photos")
			if !ok || progress.LastErrorClass != tt.class || progress.State != tt.wantState || progress.Record.Scanned != 2 {
				t.Errorf("the progress = %+v, %v; want %s and %s", progress, ok, tt.class, tt.wantState)
			}
			// Not tried again within its pause.
			calls := w.count("PutReplica")
			if stats, err := worker.RunOnce(ctx); err != nil || stats.Passes != 0 {
				t.Errorf("a round within the pause = %+v, %v", stats, err)
			}
			if w.count("PutReplica") != calls {
				t.Error("the version was tried again within its pause")
			}
			// The other deployment takes it again: the walk goes on.
			failing.stop = true
			w.clock.Advance(delivery.BackoffMax + time.Second)
			if _, err := worker.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if got := w.resyncOf(t, "photos"); got.State != bucket.ResyncDone || got.Scanned != 5 || got.Sent != 5 {
				t.Errorf("after the pause the record = %+v", got)
			}
			if progress, ok := worker.Progress("photos"); ok {
				t.Errorf("a resync that is done still shows %+v", progress)
			}
		})
	}
}

// classAfter makes the site fail with a class from the version after the
// first after - only that one when the class passes - until stop is set.
type classAfter struct {
	app.ResyncSender
	site  *failingSite
	after int
	class delivery.Class
	n     int
	stop  bool
}

func (s *classAfter) Resend(ctx context.Context, target delivery.Target, v object.Version) (object.ReplicationStatus, bool, error) {
	s.n++
	class := s.class
	if !s.stop && s.n > s.after && (!class.Passes() || s.n == s.after+1) {
		s.site.fail.Store(&class)
	}
	defer s.site.fail.Store(nil)
	return s.ResyncSender.Resend(ctx, target, v)
}

// TestResyncStandbyDoesNothing: a node that does not lead walks, sends and
// stores nothing, and shows no progress.
func TestResyncStandbyDoesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newResyncWorld(t, false)
	w.before(t, 3, 1)
	w.startResync(t)
	standby := w.resyncWorker(nil, nil, func() bool { return false })
	if _, err := standby.RunOnce(ctx); !errors.Is(err, domain.ErrNotLeader) {
		t.Errorf("RunOnce on a standby = %v, want ErrNotLeader", err)
	}
	if _, ok := standby.Progress("photos"); ok {
		t.Error("a standby shows progress")
	}
	if n := len(w.site.Calls()); n != 0 {
		t.Errorf("a standby made %d calls", n)
	}
	if got := w.resyncOf(t, "photos"); got.Scanned != 0 {
		t.Errorf("a standby stored %+v", got)
	}
}

// gatedSite holds every PutReplica until the gate is opened.
type gatedSite struct {
	*porttest.MemSite
	gate    chan struct{}
	waiting atomic.Int64
}

func (s *gatedSite) PutReplica(ctx context.Context, v object.Version, body io.Reader) error {
	s.waiting.Add(1)
	select {
	case <-s.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.MemSite.PutReplica(ctx, v, body)
}

// TestResyncRunsOnTheLeaderOnly: Run walks while the node leads, and not
// before; a lead lost ends the version in flight at once, and nothing of
// its pass is stored; the next time the node leads it goes on and ends.
func TestResyncRunsOnTheLeaderOnly(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w := newResyncWorld(t, false)
		w.before(t, 3, 1)
		w.startResync(t)
		site := &gatedSite{MemSite: w.site, gate: make(chan struct{})}
		w.replicator = w.replicatorWith(site, 0)
		var lead atomic.Bool
		worker := w.resyncWorker(nil, nil, lead.Load)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			worker.Run(ctx)
		}()
		time.Sleep(time.Minute)
		synctest.Wait()
		if n := len(w.site.Calls()); n != 0 {
			t.Errorf("a node that does not lead made %d calls", n)
		}
		lead.Store(true)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if site.waiting.Load() != 1 {
			t.Fatalf("%d versions in flight, want the first", site.waiting.Load())
		}
		if _, ok := worker.Progress("photos"); !ok {
			t.Error("the leader shows no progress")
		}
		// The lead is lost while the first version is in flight: it ends.
		lead.Store(false)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if _, ok := worker.Progress("photos"); ok {
			t.Error("a node that lost its lead still shows progress")
		}
		if got := w.resyncOf(t, "photos"); got.Scanned != 0 || got.State != bucket.ResyncRunning {
			t.Errorf("a pass whose lead was lost stored %+v", got)
		}
		close(site.gate)
		lead.Store(true)
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := w.resyncOf(t, "photos"); got.State != bucket.ResyncDone || got.Scanned != 3 || got.Sent != 3 {
			t.Errorf("after the lead came back the record = %+v", got)
		}
		cancel()
		<-done
	})
}

// TestResyncLeavesAnObjectTheOutboxSends: a resync that comes to an object
// the outbox is sending neither waits nor asks: it stops at the version with
// no failure counted, stores nothing over it, shows that it waits for the
// object, and comes back to it a moment later - to find it there.
func TestResyncLeavesAnObjectTheOutboxSends(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		w := newResyncWorld(t, false)
		v := w.putIn(t, "k", []byte("one object"), object.SSE{})
		site := &gatedSite{MemSite: w.site, gate: make(chan struct{})}
		w.replicator = w.replicatorWith(site, 0)
		row := w.last(t)
		var live sync.WaitGroup
		live.Go(func() {
			if status, err := w.replicator.Deliver(ctx, w.target, row); err != nil || status != object.ReplicationCompleted {
				t.Errorf("Deliver = %q, %v", status, err)
			}
		})
		synctest.Wait()
		w.startResync(t)
		worker := w.resyncWorker(nil, nil, nil)
		stats, err := worker.RunOnce(ctx)
		if err != nil || stats.Held != 0 || stats.Scanned != 0 {
			t.Fatalf("a round at an object the outbox sends = %+v, %v", stats, err)
		}
		if heads := w.count("HeadVersion"); heads != 1 {
			t.Errorf("%d questions asked, want the outbox's one", heads)
		}
		if progress, ok := worker.Progress("photos"); !ok || progress.Waiting != app.ResyncWaitingObject || progress.LastErrorClass != delivery.ClassNone {
			t.Errorf("the progress = %+v, %v", progress, ok)
		}
		if got := w.resyncOf(t, "photos"); got.Scanned != 0 {
			t.Errorf("stored %+v", got)
		}
		close(site.gate)
		live.Wait()
		w.clock.Advance(2 * time.Second)
		if _, err := worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if got := w.resyncOf(t, "photos"); got.State != bucket.ResyncDone || got.Scanned != 1 || got.Sent != 0 {
			t.Errorf("after the outbox's send the record = %+v", got)
		}
		// The outbox's row is not acknowledged here; the resync found the
		// version there and said so by version.
		if got := w.statusOf(t, v); got != object.ReplicationCompleted {
			t.Errorf("the version is %q, want COMPLETED", got)
		}
		if n := w.count("PutReplica"); n != 1 {
			t.Errorf("the object was sent %d times", n)
		}
	})
}

// TestTheOutboxEndsAResyncSend: the outbox never waits for a resync - it
// ends the resync's send to the object, which comes back to it later
// (ErrResyncObjectBusy, nothing sent, no status), and sends the version
// itself.
func TestTheOutboxEndsAResyncSend(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		w := newResyncWorld(t, false)
		v := w.putIn(t, "k", []byte("one object"), object.SSE{})
		site := &gatedSite{MemSite: w.site, gate: make(chan struct{})}
		replicator := w.replicatorWith(site, 0)
		row := w.last(t)
		var resync, live sync.WaitGroup
		var resyncErr error
		var resyncStatus object.ReplicationStatus
		var resyncSent bool
		resync.Go(func() { resyncStatus, resyncSent, resyncErr = replicator.Resend(ctx, w.target, v) })
		synctest.Wait()
		if site.waiting.Load() != 1 {
			t.Fatalf("%d sends in flight, want the resync's", site.waiting.Load())
		}
		live.Go(func() {
			if status, err := replicator.Deliver(ctx, w.target, row); err != nil || status != object.ReplicationCompleted {
				t.Errorf("Deliver = %q, %v", status, err)
			}
		})
		resync.Wait()
		if !errors.Is(resyncErr, app.ErrResyncObjectBusy) || resyncStatus != "" || resyncSent {
			t.Errorf("the resync's send = %q, %v, %v; want ended, busy", resyncStatus, resyncSent, resyncErr)
		}
		close(site.gate)
		live.Wait()
		if n := w.count("PutReplica"); n != 1 {
			t.Errorf("the object arrived %d times", n)
		}
	})
}

// TestTheOutboxWaitingForAnObjectHolds: a row that waits for another send of
// its object beyond its deadline fails as a timeout - which holds the target
// and is tried again - and never FAILED.
func TestTheOutboxWaitingForAnObjectHolds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		w := newResyncWorld(t, false)
		w.putIn(t, "k", []byte("one object"), object.SSE{})
		site := &gatedSite{MemSite: w.site, gate: make(chan struct{})}
		replicator := w.replicatorWith(site, 0)
		row := w.last(t)
		var first sync.WaitGroup
		first.Go(func() { _, _ = replicator.Deliver(ctx, w.target, row) })
		synctest.Wait()
		waiting, cancel := context.WithTimeout(ctx, time.Second)
		status, err := replicator.Deliver(waiting, w.target, row)
		cancel()
		if status != "" || delivery.ClassOf(err) != delivery.ClassTimeout || delivery.ClassOf(err).Passes() {
			t.Errorf("a row that waited beyond its deadline = %q, %v (%s); want a timeout that holds", status, err, delivery.ClassOf(err))
		}
		close(site.gate)
		first.Wait()
	})
}

// TestResyncOfATargetThatNeedsOne: the mark of a target that lost rows is
// the target's, and one "done" clears it for every bucket sent there: so a
// resync of one bucket starts the resync of every other bucket sent to the
// target, and is done - with the others - only once all have been through;
// then the mark is cleared. A start of the worker's is audited.
func TestResyncOfATargetThatNeedsOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newResyncWorld(t, false)
	// "cards" is sent to the same target, into a bucket of its own there.
	if err := w.meta.CreateBucket(ctx, bucket.Bucket{Name: "cards", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	if err := w.meta.SetBucketVersioning(ctx, "cards", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if err := w.meta.PutTarget(ctx, siteRecord("site-b", "photos", "cards"), audit.Record{}); err != nil {
		t.Fatal(err)
	}
	target, err := w.meta.GetTarget(ctx, "site-b")
	if err != nil {
		t.Fatal(err)
	}
	w.target = target
	cConfig, err := bucket.ReplicationConfig{Target: "site-b", Bucket: "copies-c", Rules: []bucket.ReplicationRule{{ID: "all", Enabled: true}}}.Document()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.meta.SetBucketConfig(ctx, "cards", bucket.ConfigReplication, cConfig); err != nil {
		t.Fatal(err)
	}
	if err := w.dest.CreateBucket(ctx, bucket.Bucket{Name: "copies-c", CreatedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	if err := w.dest.SetBucketVersioning(ctx, "copies-c", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if err := w.dest.SetBucketReplica(ctx, "copies-c", sendingSource, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	// The outbox keeps one row: what the target had not passed is lost.
	w.meta.MaxOutboxRows = 1
	var lost []object.Version
	for i := range 3 {
		lost = append(lost, w.putIn(t, fmt.Sprintf("b/%d", i), []byte("photos"), object.SSE{}))
	}
	for i := range 2 {
		v, err := w.objects.Put(ctx, porttest.Root(), app.PutInput{Bucket: "cards", Key: fmt.Sprintf("c/%d", i), Body: strings.NewReader("cards"), Size: 5})
		if err != nil {
			t.Fatal(err)
		}
		lost = append(lost, v)
	}
	marked := func() bool {
		t.Helper()
		state, err := w.meta.OutboxCursors(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range state.Cursors {
			if c.Target == "site-b" {
				return c.ResyncNeeded
			}
		}
		t.Fatal("no cursor of site-b")
		return false
	}
	if !marked() {
		t.Fatal("the target lost rows and needs no resync")
	}
	trail := porttest.NewMemIAM()
	auditor := app.NewAuditor(trail, w.clock, slog.New(slog.DiscardHandler))
	worker := app.NewResyncWorker(app.ResyncWorkerConfig{
		Buckets: w.meta, Index: w.meta, Store: w.meta, Targets: w.meta, Outbox: w.meta, Sender: w.replicator,
		Clock: w.clock, Auditor: auditor,
	})
	w.startResync(t)
	stats, err := worker.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Started != 1 || stats.Done != 0 {
		t.Errorf("the first round = %+v; want c started, nothing done", stats)
	}
	if b, c := w.resyncOf(t, "photos"), w.resyncOf(t, "cards"); b.State != bucket.ResyncRunning || b.Scanned != 3 ||
		c != (bucket.Resync{Bucket: "cards", Target: "site-b", State: bucket.ResyncRunning}) {
		t.Errorf("after the first round b = %+v, c = %+v", b, c)
	}
	if progress, ok := worker.Progress("photos"); !ok || progress.Waiting != app.ResyncWaitingOthers {
		t.Errorf("b through = %+v, %v; want waiting for the others", progress, ok)
	}
	if !marked() {
		t.Error("the mark was cleared before every bucket of the target was through")
	}
	if _, err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if b, c := w.resyncOf(t, "photos"), w.resyncOf(t, "cards"); b.State != bucket.ResyncDone || c.State != bucket.ResyncDone || c.Scanned != 2 {
		t.Errorf("after the second round b = %+v, c = %+v; want both done", b, c)
	}
	if marked() {
		t.Error("the mark stayed after every bucket of the target was through")
	}
	for _, v := range lost {
		got, err := w.meta.GetVersion(ctx, v.Bucket, v.Key, v.Ref())
		if err != nil || got.Replication != object.ReplicationCompleted {
			t.Errorf("%s/%s is %q (%v)", v.Bucket, v.Key, got.Replication, err)
		}
	}
	auditor.Flush(ctx)
	records, err := trail.ListAudit(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range records {
		found = found || (r.Actor == "system" && r.Action == string(iam.ActionResyncReplication) && r.Resource == "arn:aws:s3:::cards" && r.Details["reason"] == "resync_needed")
	}
	if !found {
		t.Error("the start of c's resync is not in the audit trail")
	}
}
