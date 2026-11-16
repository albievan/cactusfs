package app_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// TestTheOutboxStopsEndingOneResyncSend: the outbox ends a resync's send of
// a version app.ResyncPreemptions times and then waits for it instead, so a
// key the outbox keeps writing to does not keep an old version from
// crossing; and the outbox never waits past its own deadline - a wait that
// outlasts it is a timeout, which holds, never FAILED.
func TestTheOutboxStopsEndingOneResyncSend(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		w := newResyncWorld(t, false)
		v := w.putIn(t, "k", []byte("one object"), object.SSE{})
		site := &gatedSite{MemSite: w.site, gate: make(chan struct{})}
		replicator := w.replicatorWith(site, 0)
		row := w.last(t)
		// outboxOnce is one attempt of the outbox at the row, under a
		// deadline of a second.
		outboxOnce := func() (object.ReplicationStatus, time.Duration, error) {
			attempt, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			began := time.Now()
			status, err := replicator.Deliver(attempt, w.target, row)
			return status, time.Since(began), err
		}
		for i := range app.ResyncPreemptions {
			var resync sync.WaitGroup
			var resyncErr error
			resync.Go(func() { _, _, resyncErr = replicator.Resend(ctx, w.target, v) })
			synctest.Wait()
			status, _, err := outboxOnce()
			resync.Wait()
			if !errors.Is(resyncErr, app.ErrResyncPreempted) {
				t.Fatalf("preemption %d: the resync's send = %v, want ended by the outbox", i+1, resyncErr)
			}
			// The outbox went on to send it, and the closed gate held it
			// past its deadline: a timeout, which holds.
			if status != "" || delivery.ClassOf(err) != delivery.ClassTimeout {
				t.Fatalf("preemption %d: the outbox's attempt = %q, %v", i+1, status, err)
			}
		}
		// Once more: the outbox waits for the resync's send - and not past
		// its deadline - and the send is not ended.
		var resync sync.WaitGroup
		var resyncStatus object.ReplicationStatus
		var resyncSent bool
		var resyncErr error
		var resyncDone atomic.Bool
		resync.Go(func() {
			resyncStatus, resyncSent, resyncErr = replicator.Resend(ctx, w.target, v)
			resyncDone.Store(true)
		})
		synctest.Wait()
		status, waited, err := outboxOnce()
		if status != "" || delivery.ClassOf(err) != delivery.ClassTimeout || delivery.ClassOf(err).Passes() || waited > time.Second {
			t.Errorf("the outbox's wait = %q, %v after %s; want a timeout that holds, within its deadline", status, err, waited)
		}
		synctest.Wait()
		if resyncDone.Load() {
			t.Fatalf("the resync's send was ended a fourth time: %v", resyncErr)
		}
		close(site.gate)
		resync.Wait()
		if resyncErr != nil || resyncStatus != object.ReplicationCompleted || !resyncSent {
			t.Errorf("the resync's send = %q, %v, %v; want it sent", resyncStatus, resyncSent, resyncErr)
		}
		// The outbox's next attempt finds it there.
		if status, _, err := outboxOnce(); err != nil || status != object.ReplicationCompleted {
			t.Errorf("the outbox after the resync = %q, %v", status, err)
		}
		if n := w.count("PutReplica"); n != 1 {
			t.Errorf("the object arrived %d times", n)
		}
	})
}

// TestResyncCountsWhatTheOutboxEnded: a walk whose send the outbox ended
// stops at the version, with no failure, waits for the object, and counts
// it.
func TestResyncCountsWhatTheOutboxEnded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		w := newResyncWorld(t, false)
		w.putIn(t, "k", []byte("one object"), object.SSE{})
		site := &gatedSite{MemSite: w.site, gate: make(chan struct{})}
		w.replicator = w.replicatorWith(site, 0)
		row := w.last(t)
		w.startResync(t)
		worker := w.resyncWorker(nil, nil, nil)
		var round sync.WaitGroup
		var stats app.ResyncStats
		var err error
		round.Go(func() { stats, err = worker.RunOnce(ctx) })
		synctest.Wait()
		attempt, cancel := context.WithTimeout(ctx, time.Second)
		_, _ = w.replicator.Deliver(attempt, w.target, row)
		cancel()
		round.Wait()
		if err != nil || stats.Held != 0 || stats.Scanned != 0 {
			t.Fatalf("the round = %+v, %v", stats, err)
		}
		progress, ok := worker.Progress("photos")
		if !ok || progress.Preempted != 1 || progress.Waiting != app.ResyncWaitingObject || progress.LastErrorClass != delivery.ClassNone {
			t.Errorf("the progress = %+v, %v; want one send ended, waiting for the object, no failure", progress, ok)
		}
		close(site.gate)
	})
}

// TestResyncComesBackToABusyObjectSoon: the walks pause no longer than until
// the next walk is due - one that left an object the outbox was sending
// comes back a second later, not after the pause of a round that moved
// nothing.
func TestResyncComesBackToABusyObjectSoon(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		w := newResyncWorld(t, false)
		w.putIn(t, "k", []byte("one object"), object.SSE{})
		site := &gatedSite{MemSite: w.site, gate: make(chan struct{})}
		w.replicator = w.replicatorWith(site, 0)
		row := w.last(t)
		var live sync.WaitGroup
		live.Go(func() { _, _ = w.replicator.Deliver(ctx, w.target, row) })
		synctest.Wait()
		w.startResync(t)
		worker := app.NewResyncWorker(app.ResyncWorkerConfig{
			Buckets: w.meta, Index: w.meta, Store: w.meta, Targets: w.meta, Outbox: w.meta, Sender: w.replicator, Clock: bubbleClock{},
		})
		runCtx, stop := context.WithCancel(ctx)
		var running sync.WaitGroup
		running.Go(func() { worker.Run(runCtx) })
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if progress, ok := worker.Progress("photos"); !ok || progress.Waiting != app.ResyncWaitingObject {
			t.Fatalf("the progress = %+v, %v; want waiting for the object", progress, ok)
		}
		close(site.gate)
		live.Wait()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := w.resyncOf(t, "photos"); got.State != bucket.ResyncDone {
			t.Errorf("2.5 s in, the record = %+v; want done, a second after the object was free", got)
		}
		stop()
		running.Wait()
	})
}
