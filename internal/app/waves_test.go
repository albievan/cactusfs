package app_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// appendKeys adds a row for the consumer for each key, in order, each made a
// second after the one before, and returns them.
func (f *fakeOutbox) appendKeys(to delivery.Consumer, keys ...string) []delivery.Item {
	made := f.append(len(keys), to)
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, key := range keys {
		made[i].Key = key
		f.items[len(f.items)-len(keys)+i].Key = key
	}
	return made
}

// waveDeliverer takes each item after a second, and records how many items
// were out at once, and of which objects.
type waveDeliverer struct {
	mu sync.Mutex
	// out are the keys of the items out now; most is the most there were.
	out  map[string]int
	most int
	// together are the keys of two items that were out at once.
	together []string
	// started are the rows, in the order they were handed over.
	started []uint64
	// deadlines are how long each attempt was given.
	deadlines []time.Duration
	// fail, when not nil, says whether the attempt-th attempt at a row
	// fails.
	fail  func(seq uint64, attempt int) bool
	tried map[uint64]int
	// instant takes each item at once, not after a second.
	instant bool
	// block, when it says so, keeps the attempt until its context ends.
	block func(target delivery.Target, item delivery.Item) bool
	// ended are the attempts that ended with their context: the endpoint
	// they were made to, and the row.
	ended []attemptAt
	// took are the attempts that were taken: the endpoint, and the row.
	took []attemptAt
}

// attemptAt is an attempt at a row, at an endpoint.
type attemptAt struct {
	endpoint string
	seq      uint64
}

func (d *waveDeliverer) Deliver(ctx context.Context, target delivery.Target, item delivery.Item) (object.ReplicationStatus, error) {
	d.mu.Lock()
	if d.out == nil {
		d.out, d.tried = map[string]int{}, map[uint64]int{}
	}
	if d.out[item.Key] > 0 {
		d.together = append(d.together, item.Key)
	}
	d.out[item.Key]++
	out := 0
	for _, n := range d.out {
		out += n
	}
	d.most = max(d.most, out)
	d.started = append(d.started, item.Seq.Index)
	d.tried[item.Seq.Index]++
	attempt := d.tried[item.Seq.Index]
	if deadline, ok := ctx.Deadline(); ok {
		d.deadlines = append(d.deadlines, time.Until(deadline))
	}
	fails := d.fail != nil && d.fail(item.Seq.Index, attempt)
	block := d.block != nil && d.block(target, item)
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.out[item.Key]--
		d.mu.Unlock()
	}()
	if block {
		<-ctx.Done()
		d.mu.Lock()
		d.ended = append(d.ended, attemptAt{endpoint: target.Endpoint, seq: item.Seq.Index})
		d.mu.Unlock()
		return "", delivery.Failed(delivery.ClassTimeout, ctx.Err())
	}
	if !d.instant {
		time.Sleep(time.Second)
	}
	if fails {
		return "", failing(delivery.ClassServer)
	}
	d.mu.Lock()
	d.took = append(d.took, attemptAt{endpoint: target.Endpoint, seq: item.Seq.Index})
	d.mu.Unlock()
	return object.ReplicationCompleted, nil
}

// attempts returns the attempts that ended with their context, and those
// that were taken.
func (d *waveDeliverer) attempts() (ended, took []attemptAt) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.ended), slices.Clone(d.took)
}

// handed returns the rows handed over so far.
func (d *waveDeliverer) handed() []uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.started)
}

// runWorker runs a worker until stop is called; stop waits for it to have
// returned.
func runWorker(t *testing.T, worker *app.DeliveryWorker) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// sameRows reports whether got is want with each of the spans in any order
// within it.
func sameRows(got, want []uint64, spans [][2]int) bool {
	if len(got) != len(want) {
		return false
	}
	for _, span := range spans {
		a, b := slices.Clone(got[span[0]:span[1]]), want[span[0]:span[1]]
		slices.Sort(a)
		if !slices.Equal(a, b) {
			return false
		}
	}
	return true
}

// TestDeliveryHandsOverInWaves: items of a kind handed over several at once
// go in waves - at most that many, never two of one object, the items of
// one object in the order of the outbox - and the cursor and the results
// follow the outbox as when they go one by one. An attempt has the deadline
// of its kind. Another kind, not named, goes one by one.
func TestDeliveryHandsOverInWaves(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		outbox := newFakeOutbox()
		outbox.define("site", delivery.KindS3)
		outbox.define("hook", delivery.KindWebhook)
		site := delivery.Consumer{Kind: delivery.ConsumerReplication, Target: "site"}
		items := outbox.appendKeys(site, "a", "b", "a", "c", "d", "e", "b")
		hooked := outbox.appendKeys(hookConsumer, "a", "b", "c")
		replicating, notifying := &waveDeliverer{}, &waveDeliverer{}
		stop := runWorker(t, app.NewDeliveryWorker(app.DeliveryWorkerConfig{
			Targets: outbox, Outbox: outbox, Clock: bubbleClock{},
			Deliverers: map[delivery.ConsumerKind]app.Deliverer{delivery.ConsumerReplication: replicating, delivery.ConsumerNotification: notifying},
			Parallel:   map[delivery.ConsumerKind]int{delivery.ConsumerReplication: 3, delivery.ConsumerNotification: 0},
			AttemptTimeouts: map[delivery.ConsumerKind]func(delivery.Item) time.Duration{
				delivery.ConsumerReplication:  func(delivery.Item) time.Duration { return 3 * time.Hour },
				delivery.ConsumerNotification: func(delivery.Item) time.Duration { return -1 },
			},
		}))
		time.Sleep(30 * time.Second)
		synctest.Wait()
		stop()

		// The waves: a b | a c d | e b.
		if want := []uint64{1, 2, 3, 4, 5, 6, 7}; !sameRows(replicating.handed(), want, [][2]int{{0, 2}, {2, 5}, {5, 7}}) {
			t.Errorf("handed over %v, want the waves [1 2] [3 4 5] [6 7]", replicating.handed())
		}
		if replicating.most != 3 || len(replicating.together) != 0 {
			t.Errorf("at most %d out at once, %v of one object together; want 3 and none", replicating.most, replicating.together)
		}
		if notifying.most != 1 || !slices.Equal(notifying.handed(), indexes(hooked)) {
			t.Errorf("another kind: at most %d at once, in the order %v", notifying.most, notifying.handed())
		}
		for _, d := range replicating.deadlines {
			if d != 3*time.Hour {
				t.Errorf("an attempt at replication was given %v", d)
			}
		}
		for _, d := range notifying.deadlines {
			if d != app.DefaultDeliveryAttemptTimeout {
				t.Errorf("an attempt at a notification was given %v", d)
			}
		}
		if got := outbox.cursor("site").Acked; items[len(items)-1].Seq.After(got) {
			t.Errorf("the cursor is at %v, want past %v", got, items[len(items)-1].Seq)
		}
		var results []uint64
		for _, ack := range outbox.acked() {
			for _, result := range ack.results {
				results = append(results, result.Seq.Index)
			}
		}
		if !slices.Equal(results, indexes(items)) {
			t.Errorf("results for %v, want one for each row in order: %v", results, indexes(items))
		}
	})
}

// TestDeliveryAWaveHoldsWhatIsBehindIt: an item of a wave that fails holds
// the target as one item does: it is tried again while the rest of its wave
// is taken, nothing behind the wave is handed over until it is taken, and
// the cursor does not pass it meanwhile.
func TestDeliveryAWaveHoldsWhatIsBehindIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		outbox := newFakeOutbox()
		outbox.define("site", delivery.KindS3)
		site := delivery.Consumer{Kind: delivery.ConsumerReplication, Target: "site"}
		items := outbox.appendKeys(site, "a", "b", "c", "d")
		deliverer := &waveDeliverer{fail: func(seq uint64, attempt int) bool { return seq == 2 && attempt <= 3 }}
		stop := runWorker(t, app.NewDeliveryWorker(app.DeliveryWorkerConfig{
			Targets: outbox, Outbox: outbox, Clock: bubbleClock{},
			Deliverers: map[delivery.ConsumerKind]app.Deliverer{delivery.ConsumerReplication: deliverer},
			Parallel:   map[delivery.ConsumerKind]int{delivery.ConsumerReplication: 2},
		}))
		// The first wave, the failure of row 2, and its first pause.
		time.Sleep(2500 * time.Millisecond)
		synctest.Wait()
		if early := deliverer.handed(); slices.Contains(early, 3) || slices.Contains(early, 4) {
			t.Errorf("handed over %v while row 2 of the wave before was not taken", early)
		}
		if got := outbox.cursor("site").Acked; got.After(delivery.Seq{}) {
			t.Errorf("the cursor passed row 1 while row 2 of its wave was not taken: %v", got)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		stop()
		if want := []uint64{1, 2, 2, 2, 2, 3, 4}; !sameRows(deliverer.handed(), want, [][2]int{{0, 2}, {2, 5}, {5, 7}}) {
			t.Errorf("handed over %v, want %v", deliverer.handed(), want)
		}
		if got := outbox.cursor("site").Acked; got != items[len(items)-1].Seq {
			t.Errorf("the cursor is at %v", got)
		}
	})
}
