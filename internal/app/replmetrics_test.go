package app_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// told records what a hook of the replicator or of the resync worker was
// told, in order.
type told struct {
	mu   sync.Mutex
	what []string
}

func (t *told) tell(what string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.what = append(t.what, what)
}

func (t *told) all() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.what...)
}

// TestReplicatorTellsTheStatusesItGives: OnVersion is told of every
// COMPLETED and FAILED the replicator gives a version - sent, found there,
// or one that can never be sent - and of nothing else: not of a version that
// is passed with no status, nor of a later change that is taken.
func TestReplicatorTellsTheStatusesItGives(t *testing.T) {
	t.Parallel()
	customerKey := make([]byte, 32)
	for name, tt := range map[string]struct {
		// deliver sends what the row is; it returns the rows to deliver.
		rows func(t *testing.T, w *replicatorWorld) []delivery.Item
		want []string
	}{
		"a version sent, then found there": {
			rows: func(t *testing.T, w *replicatorWorld) []delivery.Item {
				w.putIn(t, "k", []byte("version"), object.SSE{})
				return []delivery.Item{w.last(t), w.last(t)}
			},
			want: []string{"COMPLETED", "COMPLETED"},
		},
		"a version of a customer's key": {
			rows: func(t *testing.T, w *replicatorWorld) []delivery.Item {
				w.putIn(t, "k", []byte("version"), object.SSE{Mode: object.SSEC, CustomerKey: customerKey})
				return []delivery.Item{w.last(t)}
			},
			want: []string{"FAILED"},
		},
		"a version that is gone": {
			rows: func(t *testing.T, w *replicatorWorld) []delivery.Item {
				w.putIn(t, "k", []byte("version"), object.SSE{})
				row := w.last(t)
				if _, err := w.objects.Delete(t.Context(), porttest.Root(), app.DeleteInput{Bucket: "b", Key: row.Key, Version: &row.Version}); err != nil {
					t.Fatal(err)
				}
				return []delivery.Item{row}
			},
		},
		"a later change taken": {
			rows: func(t *testing.T, w *replicatorWorld) []delivery.Item {
				v := w.putIn(t, "k", []byte("version"), object.SSE{})
				put := w.last(t)
				ref := v.Ref()
				in := app.LockInput{Bucket: v.Bucket, Key: v.Key, Version: &ref}
				if _, err := w.objects.SetTags(t.Context(), porttest.Root(), in, []object.Tag{{Key: "a", Value: "b"}}); err != nil {
					t.Fatal(err)
				}
				return []delivery.Item{put, w.last(t)}
			},
			want: []string{"COMPLETED"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newReplicatorWorld(t, false)
			statuses := &told{}
			replicator := app.NewReplicator(app.ReplicatorConfig{
				Objects: w.objects, Buckets: w.meta, Source: sendingSource, Clock: w.clock,
				Sites:     app.SiteSourceFunc(func(context.Context, delivery.Target) (ports.SiteClient, error) { return w.site, nil }),
				OnVersion: func(status object.ReplicationStatus) { statuses.tell(string(status)) },
			})
			for _, row := range tt.rows(t, w) {
				_, _ = replicator.Deliver(t.Context(), w.target, row)
			}
			if got := statuses.all(); !slices.Equal(got, tt.want) {
				t.Errorf("told %v, want %v", got, tt.want)
			}
		})
	}
}

// eventfulWorker returns the resync worker of the world, sending through
// sender (the world's replicator when nil), whose events are told to events.
func (w *replicatorWorld) eventfulWorker(sender app.ResyncSender, events *told, clock ports.Clock) *app.ResyncWorker {
	if sender == nil {
		sender = w.replicator
	}
	if clock == nil {
		clock = w.clock
	}
	return app.NewResyncWorker(app.ResyncWorkerConfig{
		Buckets: w.meta, Index: w.meta, Store: w.meta, Targets: w.meta, Outbox: w.meta, Sender: sender, Clock: clock,
		AttemptTimeout: app.ReplicationAttemptTimeout(time.Minute, 0, 4), OnEvent: events.tell,
	})
}

// TestResyncTellsWhatBefellIt: OnEvent is told of a pass held at a version,
// of a walk begun again because its target lost rows, and of a send the
// outbox ended - and of nothing else; Summary counts the walks by how they
// stand, a held one as held.
func TestResyncTellsWhatBefellIt(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		// run makes the world so and walks once.
		run         func(t *testing.T, w *replicatorWorld, events *told) *app.ResyncWorker
		wantEvents  []string
		wantSummary map[string]int
	}{
		"a walk that goes through": {
			run: func(t *testing.T, w *replicatorWorld, events *told) *app.ResyncWorker {
				w.before(t, 3, 1)
				w.startResync(t)
				worker := w.eventfulWorker(nil, events, nil)
				if _, err := worker.RunOnce(t.Context()); err != nil {
					t.Fatal(err)
				}
				return worker
			},
			wantSummary: map[string]int{app.ResyncWalking: 0, app.ResyncHeld: 0, app.ResyncWaiting: 0},
		},
		"a pass held by a failure of the network": {
			run: func(t *testing.T, w *replicatorWorld, events *told) *app.ResyncWorker {
				w.before(t, 3, 1)
				site := &failingSite{MemSite: w.site}
				w.replicator = w.replicatorWith(site, 0)
				w.startResync(t)
				worker := w.eventfulWorker(&classAfter{ResyncSender: w.replicator, site: site, after: 1, class: delivery.ClassConnect}, events, nil)
				if _, err := worker.RunOnce(t.Context()); err != nil {
					t.Fatal(err)
				}
				return worker
			},
			wantEvents:  []string{app.ResyncEventHeld},
			wantSummary: map[string]int{app.ResyncWalking: 0, app.ResyncHeld: 1, app.ResyncWaiting: 0},
		},
		"a walk begun again": {
			run: func(t *testing.T, w *replicatorWorld, events *told) *app.ResyncWorker {
				w.before(t, 3, 1)
				w.meta.MaxOutboxRows = 1
				lose := &interrupting{ResyncSender: w.replicator, during: func() {
					for i := range 2 {
						w.putIn(t, fmt.Sprintf("a/%d", i), []byte("behind the cursor"), object.SSE{})
					}
				}}
				w.startResync(t)
				worker := w.eventfulWorker(lose, events, nil)
				if _, err := worker.RunOnce(t.Context()); err != nil {
					t.Fatal(err)
				}
				return worker
			},
			wantEvents:  []string{app.ResyncEventRestarted},
			wantSummary: map[string]int{app.ResyncWalking: 1, app.ResyncHeld: 0, app.ResyncWaiting: 0},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newResyncWorld(t, false)
			events := &told{}
			worker := tt.run(t, w, events)
			if got := events.all(); !slices.Equal(got, tt.wantEvents) {
				t.Errorf("told %v, want %v", got, tt.wantEvents)
			}
			if got, ok := worker.Summary(); !ok || !maps.Equal(got, tt.wantSummary) {
				t.Errorf("Summary = %v, %v; want %v", got, ok, tt.wantSummary)
			}
		})
	}
}

// TestResyncTellsOfASendTheOutboxEnded: a send the outbox ended is told as
// preempted, and the walk counts as waiting - for the object - not as held.
func TestResyncTellsOfASendTheOutboxEnded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		w := newResyncWorld(t, false)
		w.putIn(t, "k", []byte("one object"), object.SSE{})
		site := &gatedSite{MemSite: w.site, gate: make(chan struct{})}
		w.replicator = w.replicatorWith(site, 0)
		row := w.last(t)
		w.startResync(t)
		events := &told{}
		worker := w.eventfulWorker(nil, events, nil)
		var round sync.WaitGroup
		round.Go(func() { _, _ = worker.RunOnce(ctx) })
		synctest.Wait()
		attempt, cancel := context.WithTimeout(ctx, time.Second)
		_, _ = w.replicator.Deliver(attempt, w.target, row)
		cancel()
		round.Wait()
		if got, want := events.all(), []string{app.ResyncEventPreempted}; !slices.Equal(got, want) {
			t.Errorf("told %v, want %v", got, want)
		}
		if got, ok := worker.Summary(); !ok || got[app.ResyncWaiting] != 1 || got[app.ResyncHeld] != 0 {
			t.Errorf("Summary = %v, %v; want one waiting", got, ok)
		}
		close(site.gate)
	})
}

// TestResyncSummaryOfAStandby: a node that does not lead cannot say.
func TestResyncSummaryOfAStandby(t *testing.T) {
	t.Parallel()
	w := newResyncWorld(t, false)
	standby := w.resyncWorker(nil, nil, func() bool { return false })
	if got, ok := standby.Summary(); ok || got != nil {
		t.Errorf("Summary of a standby = %v, %v", got, ok)
	}
}

// TestAHeldResyncShowsInItsTargetsStatus: a resync held at a version
// because the other deployment refuses its key shows in the status of the
// target - stalled, class auth - although the outbox has nothing for it, in
// DeliveryStatus (which both status endpoints read) and in the summary
// (PLAN 8.1.7 review). The target is ok again once the resync's next
// attempt succeeds - and once the bucket's configuration names another
// target, for which the walk waits: a walk that waits for its configuration
// is held by no failure of a target, and counts as waiting, not held
// (verification of PLAN 8.1.7).
func TestAHeldResyncShowsInItsTargetsStatus(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		// after makes the world so after the resync was held.
		after func(t *testing.T, w *replicatorWorld, sender *classAfter)
		// wantScanned says that the round after the pause goes on.
		wantScanned bool
		wantSummary map[string]int
	}{
		"the next attempt succeeds": {
			after:       func(_ *testing.T, _ *replicatorWorld, sender *classAfter) { sender.stop = true },
			wantScanned: true,
			wantSummary: map[string]int{app.ResyncWalking: 0, app.ResyncHeld: 0, app.ResyncWaiting: 0},
		},
		"the configuration names another target": {
			after: func(t *testing.T, w *replicatorWorld, _ *classAfter) {
				if err := w.meta.PutTarget(t.Context(), siteRecord("site-c", "photos"), audit.Record{}); err != nil {
					t.Fatalf("PutTarget: %v", err)
				}
				w.configure(t, bucket.ReplicationConfig{Target: "site-c", Bucket: "copies", Rules: []bucket.ReplicationRule{{ID: "all", Enabled: true}}})
			},
			wantSummary: map[string]int{app.ResyncWalking: 0, app.ResyncHeld: 0, app.ResyncWaiting: 1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				w := newResyncWorld(t, false)
				w.before(t, 2, 1)
				site := &failingSite{MemSite: w.site}
				w.replicator = w.replicatorWith(site, 0)
				sender := &classAfter{ResyncSender: w.replicator, site: site, class: delivery.ClassAuth}
				resync := w.eventfulWorker(sender, &told{}, bubbleClock{})
				worker := app.NewDeliveryWorker(app.DeliveryWorkerConfig{
					Targets: w.meta, Outbox: w.meta, Clock: bubbleClock{}, Resyncs: resync,
					Deliverers: map[delivery.ConsumerKind]app.Deliverer{delivery.ConsumerReplication: w.replicator},
				})
				stop := runWorker(t, worker)
				defer stop()
				time.Sleep(time.Second)
				synctest.Wait()
				status := func(when, state string, class delivery.Class) {
					t.Helper()
					got, err := worker.DeliveryStatus(ctx, []string{"site-b"})
					if err != nil || len(got) != 1 || got[0].State != state || got[0].LastErrorClass != class || got[0].Pending != 0 {
						t.Errorf("%s: DeliveryStatus = %+v, %v; want %s, class %q, nothing pending", when, got, err, state, class)
					}
					stalled := 0
					if state == ports.DeliveryStalled {
						stalled = 1
					}
					if summary, ok := worker.Summary(); !ok || summary.Targets[delivery.KindS3][ports.DeliveryStalled] != stalled {
						t.Errorf("%s: summary = %+v, %v; want %d target stalled", when, summary, ok, stalled)
					}
				}
				status("before the resync", ports.DeliveryOK, delivery.ClassNone)

				w.startResync(t)
				if stats, err := resync.RunOnce(ctx); err != nil || stats.Held != 1 {
					t.Fatalf("the first round = %+v, %v; want held", stats, err)
				}
				status("held on a key refused", ports.DeliveryStalled, delivery.ClassAuth)
				if got, ok := resync.Summary(); !ok || got[app.ResyncHeld] != 1 {
					t.Errorf("Summary while held = %v, %v", got, ok)
				}

				tt.after(t, w, sender)
				time.Sleep(10 * time.Minute)
				stats, err := resync.RunOnce(ctx)
				if err != nil || stats.Held != 0 || (stats.Scanned > 0) != tt.wantScanned {
					t.Fatalf("the round after the pause = %+v, %v; want it to go on: %v", stats, err, tt.wantScanned)
				}
				status("after", ports.DeliveryOK, delivery.ClassNone)
				if got, ok := resync.Summary(); !ok || !maps.Equal(got, tt.wantSummary) {
					t.Errorf("Summary after = %v, %v; want %v", got, ok, tt.wantSummary)
				}
			})
		})
	}
}
