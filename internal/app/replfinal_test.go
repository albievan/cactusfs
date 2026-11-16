package app_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// TestReplicatorNeverAbortsWhatIsInFlight (PLAN 8.1.4 final verification):
// uploads that attempts are sending through are pinned. More of them in
// flight together than the bound of parts allows exceed it for as long as
// they last: none is forgotten or aborted, and every version arrives.
func TestReplicatorNeverAbortsWhatIsInFlight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newReplicatorWorld(t, false)
	keys := []string{"movie", "film", "clip"}
	items := make([]delivery.Item, 0, len(keys))
	for _, key := range keys {
		w.threeParts(t, key)
		items = append(items, w.last(t))
	}
	// Every attempt has begun its upload before any sends a part, so the
	// three are in flight together: nine parts against a bound of three.
	var created atomic.Int32
	all := make(chan struct{})
	var once sync.Once
	site := &scriptedSite{
		MemSite: w.site,
		created: func() error {
			if created.Add(1) == 3 { // the three keys
				once.Do(func() { close(all) })
			}
			return nil
		},
		part: func() error {
			select {
			case <-all:
				return nil
			case <-time.After(10 * time.Second):
				return errors.New("the attempts were not in flight together")
			}
		},
	}
	r := w.replicatorWith(site, 0).WithRememberedBounds(10, 3)
	statuses := make([]object.ReplicationStatus, len(items))
	errs := make([]error, len(items))
	var running sync.WaitGroup
	for i, item := range items {
		running.Go(func() { statuses[i], errs[i] = r.Deliver(ctx, w.target, item) })
	}
	running.Wait()
	for i := range items {
		if errs[i] != nil || statuses[i] != object.ReplicationCompleted {
			t.Errorf("%s = %q, %v; want COMPLETED", keys[i], statuses[i], errs[i])
		}
	}
	if n := w.count("AbortUpload"); n != 0 {
		t.Errorf("%d uploads in flight were aborted", n)
	}
	for _, key := range keys {
		if ids, err := w.site.ListReplicaUploads(ctx, "copies", key); err != nil || len(ids) != 0 {
			t.Errorf("uploads of %s left there: %v, %v", key, ids, err)
		}
	}
}

// TestReplicatorIgnoresAnOlderTarget (PLAN 8.1.4 final verification): an
// attempt made with an older state of a target - one that began before a
// change, or the loop of a target deleted since - changes nothing of what is
// known of the target as it is: the probe of the target as it is is not
// asked again. Targets that are gone are forgotten (KeepTargets).
func TestReplicatorIgnoresAnOlderTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newReplicatorWorld(t, false)
	w.putIn(t, "k", []byte("version"), object.SSE{})
	item := w.last(t)
	was := w.target
	was.UpdatedAt = testNow.Add(-time.Minute)
	is := was
	is.Endpoint, is.UpdatedAt = "https://site-b-elsewhere.example", testNow
	deleted := was
	deleted.CreatedAt = was.CreatedAt.Add(-24 * time.Hour)

	probes := func() int { return w.count("Check") }
	deliver := func(target delivery.Target) {
		t.Helper()
		if _, err := w.replicator.Deliver(ctx, target, item); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
	}
	deliver(is)
	if probes() != 1 {
		t.Fatalf("probed %d times", probes())
	}
	deliver(was)
	deliver(deleted)
	deliver(is)
	// The two older states were probed each for itself, and the target as
	// it is was not probed again.
	if probes() != 3 {
		t.Errorf("probed %d times, want 3: an older target changed what is known of the target as it is", probes())
	}
	w.replicator.KeepTargets([]string{"another"})
	deliver(is)
	if probes() != 4 {
		t.Errorf("probed %d times, want 4: a target that is gone was not forgotten", probes())
	}
	w.replicator.KeepTargets([]string{is.Name})
	deliver(is)
	if probes() != 4 {
		t.Errorf("probed %d times, want 4: a target that is there was forgotten", probes())
	}
}

// keepingDeliverer records the names of targets the worker tells it of.
type keepingDeliverer struct {
	*waveDeliverer
	mu    sync.Mutex
	names []string
}

func (d *keepingDeliverer) KeepTargets(names []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.names = slices.Clone(names)
}

func (d *keepingDeliverer) kept() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.names)
}

// TestDeliveryTellsWhichTargetsThereAre: at every refresh the worker tells a
// Deliverer that keeps something of each target (app.TargetKeeper) which
// targets there are, so that one deleted is forgotten.
func TestDeliveryTellsWhichTargetsThereAre(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		outbox := newFakeOutbox()
		outbox.define("site", delivery.KindS3)
		outbox.define("other", delivery.KindS3)
		d := &keepingDeliverer{waveDeliverer: &waveDeliverer{}}
		active := &atomic.Bool{}
		active.Store(true)
		stop := runWorker(t, app.NewDeliveryWorker(app.DeliveryWorkerConfig{
			Targets: outbox, Outbox: outbox, Clock: bubbleClock{}, Active: active.Load,
			Deliverers: map[delivery.ConsumerKind]app.Deliverer{delivery.ConsumerReplication: d},
		}))
		defer stop()
		time.Sleep(time.Second)
		synctest.Wait()
		if got := d.kept(); !sameNames(got, []string{"other", "site"}) {
			t.Errorf("told of %v, want both targets", got)
		}
		if err := outbox.DeleteTarget(ctxWithDeadline(t), "other", audit.Record{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(6 * time.Second)
		synctest.Wait()
		if got := d.kept(); !sameNames(got, []string{"site"}) {
			t.Errorf("told of %v after a target was deleted, want site alone", got)
		}
	})
}

// sameNames reports whether two lists hold the same names, in any order.
func sameNames(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
