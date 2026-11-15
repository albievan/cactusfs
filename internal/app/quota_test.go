package app_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
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
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// watchedUsage is the metadata fake with what is asked of its usage counted,
// and faults put in. The fake counts by looking, so it never drifts: with
// drift, a walk that ends a recount says that it did.
type watchedUsage struct {
	*porttest.MemMetadata
	walks, reads, lists atomic.Int64
	// limits holds every limit a walk was asked for other than the
	// counter's.
	mu     sync.Mutex
	limits []int
	fail   error
	// failOn, when not empty, is the one bucket whose walks fail.
	failOn string
	drift  bool
	// walked are the buckets walked, in order.
	walked []string
}

func newWatchedUsage() *watchedUsage {
	return &watchedUsage{MemMetadata: porttest.NewMemMetadata()}
}

func (w *watchedUsage) ListBuckets(ctx context.Context) ([]bucket.Bucket, error) {
	w.lists.Add(1)
	return w.MemMetadata.ListBuckets(ctx)
}

func (w *watchedUsage) BucketUsages(ctx context.Context, names []string) ([]ports.BucketUsage, error) {
	w.reads.Add(1)
	return w.MemMetadata.BucketUsages(ctx, names)
}

func (w *watchedUsage) WalkBucketUsage(ctx context.Context, name string, limit int, restart bool) (ports.UsageWalk, error) {
	w.walks.Add(1)
	w.mu.Lock()
	if limit != app.UsageWalkLimit {
		w.limits = append(w.limits, limit)
	}
	fail, drift := w.fail, w.drift
	if w.failOn != "" && w.failOn != name {
		fail = nil
	}
	w.walked = append(w.walked, name)
	w.mu.Unlock()
	if fail != nil {
		return ports.UsageWalk{}, fail
	}
	before, err := w.MemMetadata.BucketUsages(ctx, []string{name})
	if err != nil {
		return ports.UsageWalk{}, err
	}
	walk, err := w.MemMetadata.WalkBucketUsage(ctx, name, limit, restart)
	was := before[0].Usage
	if err == nil && drift && was.Exact && (restart || was.Walking()) && !walk.Usage.Walking() {
		walk.Drift = true
	}
	return walk, err
}

func (w *watchedUsage) set(fail error, drift bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fail, w.drift = fail, drift
}

func (w *watchedUsage) failOnly(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failOn = name
}

func (w *watchedUsage) walkedNames() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.walked...)
}

func (w *watchedUsage) otherLimits() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]int(nil), w.limits...)
}

// record returns the usage record of a bucket.
func (w *watchedUsage) record(t *testing.T, name string) bucket.Usage {
	t.Helper()
	records, err := w.MemMetadata.BucketUsages(context.Background(), []string{name})
	if err != nil {
		t.Fatalf("BucketUsages(%s): %v", name, err)
	}
	return records[0].Usage
}

// bucketOf makes a bucket of n keys of one byte. An old one has no usage
// record, as a bucket made before the record has none.
func (w *watchedUsage) bucketOf(t *testing.T, name string, n int, old bool) {
	t.Helper()
	if err := w.CreateBucket(context.Background(), bucket.Bucket{Name: name, CreatedAt: testNow}); err != nil {
		t.Fatalf("CreateBucket(%s): %v", name, err)
	}
	if err := w.Fill(name, "key-%07d", n, 1); err != nil {
		t.Fatalf("Fill(%s): %v", name, err)
	}
	if old {
		w.ForgetUsage(name)
	}
}

// counterOf returns a counter over the store, active while active says so,
// and its log.
func counterOf(w *watchedUsage, active *atomic.Bool, onDrift func()) (*app.UsageCounter, *syncBuffer) {
	logs := &syncBuffer{}
	return app.NewUsageCounter(app.UsageCounterConfig{
		Buckets: w, Usage: w, Authz: &porttest.Authorizer{}, Logger: slog.New(slog.NewJSONHandler(logs, nil)),
		Active: active.Load, OnDrift: onDrift,
	}), logs
}

// running runs a counter in the bubble of the test until stop is called;
// stop waits for it to have returned.
func running(t *testing.T, c *app.UsageCounter) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// after lets the counter run for d of the bubble's time, and for the
// interval that ends then to be done with. d is not a whole number of
// intervals, so that no tick falls at the moment it ends.
func after(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

// TestUsageCounter: the node that leads proposes an entry of the walk every
// interval for the buckets that are not exact or are being counted again,
// one bucket after the other, until each is exact; a node that does not lead
// does nothing; a fault of the store is let go until the next look.
func TestUsageCounter(t *testing.T) {
	t.Parallel()
	type made struct {
		name string
		keys int
		old  bool
		// recounting begins a recount of one record before the counter
		// runs.
		recounting bool
	}
	for name, tt := range map[string]struct {
		buckets []made
		standby bool
		fail    error
		failOn  string
		run     time.Duration
		// wantWalks is how many entries were proposed; wantExact and
		// wantCounting are the buckets exact and done, and those not.
		wantWalks    int64
		wantExact    []string
		wantCounting []string
		wantLogged   string
	}{
		"nothing to count": {
			buckets: []made{{name: "a", keys: 3}}, run: 2050 * time.Millisecond,
			wantExact: []string{"a"},
		},
		"a bucket older than the record is counted": {
			buckets: []made{{name: "a", keys: 3, old: true}}, run: 2050 * time.Millisecond,
			wantWalks: 1, wantExact: []string{"a"},
		},
		"one bucket after the other": {
			// 25 000 keys are three entries: 10 000, 20 000, then the
			// rest with the uploads; three keys are one.
			buckets:   []made{{name: "a", keys: 25_000, old: true}, {name: "b", keys: 3, old: true}, {name: "c", keys: 2}},
			run:       2050 * time.Millisecond,
			wantWalks: 4, wantExact: []string{"a", "b", "c"},
		},
		"a recount under way goes on": {
			buckets: []made{{name: "a", keys: 25_000, recounting: true}}, run: 2050 * time.Millisecond,
			wantWalks: 3, wantExact: []string{"a"},
		},
		"standby does nothing": {
			buckets: []made{{name: "a", keys: 3, old: true}, {name: "b", keys: 25_000, recounting: true}},
			standby: true, run: 20050 * time.Millisecond,
			wantCounting: []string{"a", "b"},
		},
		"below level 5 the counter waits for the next look": {
			buckets: []made{{name: "a", keys: 3, old: true}}, fail: fmtErr(domain.ErrUnavailable), run: 10150 * time.Millisecond,
			// The first interval, and the 101st: a hundred after the
			// look.
			wantWalks: 2, wantCounting: []string{"a"},
		},
		// A bucket let go is let go alone: the next is walked in the next
		// interval, not at the next look.
		"a bucket deleted meanwhile is let go": {
			buckets: []made{{name: "a", keys: 3, old: true}, {name: "b", keys: 3, old: true}},
			fail:    fmtErr(domain.ErrBucketNotFound), failOn: "a", run: 2050 * time.Millisecond,
			wantWalks: 2, wantExact: []string{"b"}, wantCounting: []string{"a"},
		},
		"a fault is logged and holds up no other bucket": {
			buckets: []made{{name: "a", keys: 3, old: true}, {name: "b", keys: 3, old: true}},
			fail:    errors.New("the disk is on fire"), failOn: "a", run: 2050 * time.Millisecond,
			wantWalks: 2, wantExact: []string{"b"}, wantCounting: []string{"a"}, wantLogged: "the disk is on fire",
		},
		"a faulty bucket is looked at again with every other": {
			buckets: []made{{name: "a", keys: 3, old: true}}, fail: errors.New("the disk is on fire"), run: 10150 * time.Millisecond,
			// The first interval, and the 101st.
			wantWalks: 2, wantCounting: []string{"a"}, wantLogged: "the disk is on fire",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				w := newWatchedUsage()
				for _, b := range tt.buckets {
					w.bucketOf(t, b.name, b.keys, b.old)
					if b.recounting {
						if _, err := w.MemMetadata.WalkBucketUsage(t.Context(), b.name, 1, true); err != nil {
							t.Fatalf("WalkBucketUsage: %v", err)
						}
					}
				}
				w.set(tt.fail, false)
				w.failOnly(tt.failOn)
				var active atomic.Bool
				active.Store(!tt.standby)
				counter, logs := counterOf(w, &active, nil)
				stop := running(t, counter)
				after(tt.run)
				stop()

				if got := w.walks.Load(); got != tt.wantWalks {
					t.Errorf("entries of the walk = %d, want %d", got, tt.wantWalks)
				}
				if limits := w.otherLimits(); len(limits) > 0 {
					t.Errorf("walks of other limits than %d: %v", app.UsageWalkLimit, limits)
				}
				w.set(nil, false)
				for _, name := range tt.wantExact {
					if u := w.record(t, name); !u.Exact || u.Walking() {
						t.Errorf("record of %s = %+v, want exact and done", name, u)
					}
				}
				for _, name := range tt.wantCounting {
					if u := w.record(t, name); u.Exact && !u.Walking() {
						t.Errorf("record of %s = %+v, want it still to be counted", name, u)
					}
				}
				if tt.standby && w.reads.Load()+w.lists.Load() > 0 {
					t.Errorf("a node that does not lead read the store: %d lists, %d reads", w.lists.Load(), w.reads.Load())
				}
				logged := logs.String()
				if tt.wantLogged != "" && !strings.Contains(logged, tt.wantLogged) {
					t.Errorf("log = %q, want it to say %q", logged, tt.wantLogged)
				}
				if tt.wantLogged == "" && logged != "" {
					t.Errorf("log = %q, want nothing", logged)
				}
			})
		})
	}
}

func fmtErr(sentinel error) error {
	return errors.Join(sentinel, errors.New("as the store says it"))
}

// TestUsageCounterTakesTurns: the buckets being counted are walked an entry
// each in turn, so that a large bucket holds up neither the others nor a
// recount; recounts asked for through another node are found by the next
// look.
func TestUsageCounterTakesTurns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w := newWatchedUsage()
		w.bucketOf(t, "a", 25_000, true)
		w.bucketOf(t, "b", 25_000, true)
		w.bucketOf(t, "c", 40_000, false)
		w.bucketOf(t, "d", 25_000, false)
		var active atomic.Bool
		active.Store(true)
		counter, _ := counterOf(w, &active, nil)
		stop := running(t, counter)
		defer stop()
		after(app.UsageWalkInterval + app.UsageWalkInterval/2)
		// Recounts through another node: the records walk, and this
		// node's queue does not know them until its next look.
		for _, name := range []string{"c", "d"} {
			if _, err := w.MemMetadata.WalkBucketUsage(t.Context(), name, 1, true); err != nil {
				t.Fatalf("WalkBucketUsage(%s): %v", name, err)
			}
		}
		after(11 * time.Second)
		// a and b in turn, three entries each; c and d are found by the
		// look of the 101st interval and walked in turn, four entries
		// and three.
		want := []string{"a", "b", "a", "b", "a", "b", "c", "d", "c", "d", "c", "d", "c"}
		if got := w.walkedNames(); !slices.Equal(got, want) {
			t.Errorf("walked %v, want %v", got, want)
		}
		for _, name := range []string{"a", "b", "c", "d"} {
			if u := w.record(t, name); !u.Exact || u.Walking() {
				t.Errorf("record of %s = %+v, want exact and done", name, u)
			}
		}
	})
}

// TestUsageCounterCadence: one entry every interval, and not one more.
func TestUsageCounterCadence(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w := newWatchedUsage()
		w.bucketOf(t, "a", 100_000, true)
		var active atomic.Bool
		active.Store(true)
		counter, _ := counterOf(w, &active, nil)
		stop := running(t, counter)
		defer stop()
		for i := range int64(5) {
			if got := w.walks.Load(); got != i {
				t.Fatalf("entries after %d intervals = %d, want %d", i, got, i)
			}
			after(app.UsageWalkInterval)
		}
	})
}

// TestUsageCounterChangeOfLeader: the node that comes to lead goes on with
// the walk from where the record says it is, and the one that led stops.
func TestUsageCounterChangeOfLeader(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w := newWatchedUsage()
		w.bucketOf(t, "a", 25_000, true)
		var first, second atomic.Bool
		first.Store(true)
		one, _ := counterOf(w, &first, nil)
		two, _ := counterOf(w, &second, nil)
		stopOne, stopTwo := running(t, one), running(t, two)
		defer stopOne()
		defer stopTwo()

		after(150 * time.Millisecond)
		if got := w.walks.Load(); got != 1 {
			t.Fatalf("entries under the first leader = %d, want 1", got)
		}
		first.Store(false)
		second.Store(true)
		after(2 * time.Second)
		if got, u := w.walks.Load(), w.record(t, "a"); got != 3 || !u.Exact || u.Walking() || u.Objects != 25_000 {
			t.Errorf("after the change of leader: %d entries, record %+v; want 3 in all and exact", got, u)
		}
	})
}

// TestUsageOfABucketOlderThanTheRecord: a bucket of 250 000 keys is walked
// in 25 entries of 10 000 records and is exact with the next, which walks
// its uploads; from then on its usage is no longer bounded.
func TestUsageOfABucketOlderThanTheRecord(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		w := newWatchedUsage()
		w.MemMetadata = f.meta
		w.bucketOf(t, "big", 250_000, true)
		objects := f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) { cfg.Usage = w })
		usage := func() app.BucketUsage {
			t.Helper()
			got, err := objects.Usage(t.Context(), porttest.Root(), "big")
			if err != nil {
				t.Fatalf("Usage: %v", err)
			}
			return got
		}
		bounded := app.BucketUsage{Objects: app.MaxUsageScan, Bytes: app.MaxUsageScan}
		if got := usage(); got != bounded {
			t.Fatalf("Usage before the count = %+v, want %+v", got, bounded)
		}

		var active atomic.Bool
		active.Store(true)
		counter, _ := counterOf(w, &active, nil)
		stop := running(t, counter)
		defer stop()
		after(25*app.UsageWalkInterval + app.UsageWalkInterval/2)
		if got, u := w.walks.Load(), w.record(t, "big"); got != 25 || u.Exact || u.Count == nil || u.Count.Phase != bucket.UsagePhaseUploads {
			t.Fatalf("after 25 intervals: %d entries, record %+v; want 25, every key walked", got, u)
		}
		after(app.UsageWalkInterval)
		want := app.BucketUsage{
			Objects: 250_000, Bytes: 250_000, Complete: true, Exact: true, Versions: 250_000, StoredBytes: 250_000,
		}
		if got, u := w.walks.Load(), w.record(t, "big"); got != 26 || !u.Exact || u.Walking() {
			t.Fatalf("after 26 intervals: %d entries, record %+v; want 26 and exact", got, u)
		}
		if got := usage(); got != want {
			t.Errorf("Usage once counted = %+v, want %+v", got, want)
		}
		// Nothing is left to count: no more entries.
		after(15 * time.Second)
		if got := w.walks.Load(); got != 26 {
			t.Errorf("entries once everything was counted = %d, want 26", got)
		}
	})
}

// TestRecount: a recount is asked for with its first entry; the counter
// finishes it; one whose result differs from what was kept is drift.
func TestRecount(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		bucket string
		keys   int
		drift  bool
		fail   error
		// wantWalks are the entries of Recount and of the counter.
		wantWalks int64
		wantDrift int64
		wantErr   error
	}{
		"a small bucket is counted again by one entry": {bucket: "a", keys: 5, wantWalks: 1},
		"a large one by the counter":                   {bucket: "a", keys: 25_000, wantWalks: 3},
		"drift found by the one entry":                 {bucket: "a", keys: 5, drift: true, wantWalks: 1, wantDrift: 1},
		"drift found by the counter":                   {bucket: "a", keys: 25_000, drift: true, wantWalks: 3, wantDrift: 1},
		"no such bucket":                               {bucket: "nowhere", wantWalks: 1, wantErr: domain.ErrBucketNotFound},
		"a deployment that does not count yet": {
			bucket: "a", keys: 5, fail: fmtErr(domain.ErrUnavailable), wantWalks: 1, wantErr: domain.ErrUnavailable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				w := newWatchedUsage()
				w.bucketOf(t, "a", tt.keys, false)
				w.set(tt.fail, tt.drift)
				var active atomic.Bool
				active.Store(true)
				var drifts atomic.Int64
				counter, logs := counterOf(w, &active, func() { drifts.Add(1) })
				// The counter has looked at every bucket before the recount
				// is asked for: what it walks after is the recount's.
				stop := running(t, counter)
				defer stop()
				after(app.UsageWalkInterval + app.UsageWalkInterval/2)

				err := counter.Recount(t.Context(), porttest.Root(), tt.bucket)
				if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil) != (err == nil) {
					t.Fatalf("Recount = %v, want %v", err, tt.wantErr)
				}
				after(time.Second)
				if got := w.walks.Load(); got != tt.wantWalks {
					t.Errorf("entries = %d, want %d", got, tt.wantWalks)
				}
				if got := drifts.Load(); got != tt.wantDrift {
					t.Errorf("drift counted %d times, want %d", got, tt.wantDrift)
				}
				if logged := strings.Contains(logs.String(), "a recount put it right"); logged != (tt.wantDrift > 0) {
					t.Errorf("drift logged = %v, want %v: %s", logged, tt.wantDrift > 0, logs.String())
				}
				if tt.wantErr == nil {
					if u := w.record(t, "a"); !u.Exact || u.Walking() || u.Objects != int64(tt.keys) {
						t.Errorf("record after the recount = %+v, want exact, %d objects", u, tt.keys)
					}
				}
			})
		})
	}
}

// TestRecountIsAuthorized: a recount needs admin:RecountBucketUsage on the
// bucket, which of the built-in roles cluster-admin alone holds; a refusal
// proposes nothing and is audited, and a recount is audited too.
func TestRecountIsAuthorized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newIAMFixture(t)
	w := newWatchedUsage()
	w.MemMetadata = f.meta
	w.bucketOf(t, "photos", 3, false)
	counter := app.NewUsageCounter(app.UsageCounterConfig{
		Buckets: w, Usage: w, Authz: f.authz, Auditor: f.auditor, Logger: slog.New(slog.DiscardHandler),
	})
	for _, tt := range []struct {
		role    string
		user    string
		wantErr error
	}{
		{iam.RoleClusterAdmin, "admin-user", nil},
		{iam.RoleBucketAdmin, "bucket-user", domain.ErrAccessDenied},
		{iam.RoleAuditor, "auditor-user", domain.ErrAccessDenied},
		{iam.RoleReadOnly, "reader-user", domain.ErrAccessDenied},
		{iam.RoleReadWrite, "writer-user", domain.ErrAccessDenied},
	} {
		p := f.as(t, f.user(t, tt.user, tt.role))
		walks := w.walks.Load()
		err := counter.Recount(ctx, p, "photos")
		if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil) != (err == nil) {
			t.Errorf("Recount as %s = %v, want %v", tt.role, err, tt.wantErr)
			continue
		}
		wantWalks := walks
		wantOutcome := audit.Denied
		if tt.wantErr == nil {
			wantWalks, wantOutcome = walks+1, audit.Succeeded
		}
		if got := w.walks.Load(); got != wantWalks {
			t.Errorf("entries = %d, want %d", got, wantWalks)
		}
		var found bool
		for _, r := range f.trail(t) {
			if r.Actor == "user/"+tt.user && r.Action == string(iam.ActionRecountBucketUsage) &&
				r.Resource == "arn:aws:s3:::photos" && r.Outcome == wantOutcome {
				found = true
			}
		}
		if !found {
			t.Errorf("no %s record of the recount by %s in the trail", wantOutcome, tt.user)
		}
	}
}

// TestRecountOfUnknownOutcome: a recount whose entry may or may not have
// been committed is audited as failed, with the outcome unknown.
func TestRecountOfUnknownOutcome(t *testing.T) {
	t.Parallel()
	f := newIAMFixture(t)
	w := newWatchedUsage()
	w.MemMetadata = f.meta
	w.bucketOf(t, "photos", 3, false)
	w.set(fmtErr(domain.ErrOutcomeUnknown), false)
	counter := app.NewUsageCounter(app.UsageCounterConfig{
		Buckets: w, Usage: w, Authz: f.authz, Auditor: f.auditor, Logger: slog.New(slog.DiscardHandler),
	})
	if err := counter.Recount(context.Background(), f.root, "photos"); !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Fatalf("Recount = %v, want %v", err, domain.ErrOutcomeUnknown)
	}
	var records []audit.Record
	for _, r := range f.trail(t) {
		if r.Action == string(iam.ActionRecountBucketUsage) {
			records = append(records, r)
		}
	}
	if len(records) != 1 || records[0].Outcome != audit.Failed || records[0].Details["outcome"] != "unknown" ||
		records[0].Resource != "arn:aws:s3:::photos" {
		t.Errorf("records of the recount = %+v, want one, failed, of an unknown outcome", records)
	}
}
