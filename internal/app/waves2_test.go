package app_test

import (
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/ports"
)

// siteConsumer is the replication consumer of the target "site".
var siteConsumer = delivery.Consumer{Kind: delivery.ConsumerReplication, Target: "site"}

// waveWorker is a worker of the fake outbox that hands replication over n
// at a time, through d, leading while active says so.
func waveWorker(outbox *fakeOutbox, d *waveDeliverer, n int, active *atomic.Bool) *app.DeliveryWorker {
	return app.NewDeliveryWorker(app.DeliveryWorkerConfig{
		Targets: outbox, Outbox: outbox, Clock: bubbleClock{}, Active: active.Load,
		Deliverers: map[delivery.ConsumerKind]app.Deliverer{delivery.ConsumerReplication: d},
		Parallel:   map[delivery.ConsumerKind]int{delivery.ConsumerReplication: n},
	})
}

// changeTarget changes the target "site" as a refresh will read it.
func (f *fakeOutbox) changeTarget(change func(*delivery.Target)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := f.targets["site"]
	change(&target)
	f.targets["site"] = target
}

// TestDeliveryATargetThatChangesEndsWhatIsInFlight (PLAN 8.1.4 review): an
// attempt in flight when its target's connection - its endpoint, its
// credentials - or the buckets it admits change ends at the next refresh,
// and the item is sent again, at once, to the target as it is; nothing is
// sent to the target as it was after that. A change that is no change of
// the connection - the audit flag - ends nothing.
func TestDeliveryATargetThatChangesEndsWhatIsInFlight(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		// buckets, when set, are the buckets the target admits first.
		buckets []string
		change  func(*delivery.Target)
		cut     bool
	}{
		"its endpoint":                   {change: func(t *delivery.Target) { t.Endpoint = markedEndpoint + ":8443" }, cut: true},
		"its credentials":                {change: func(t *delivery.Target) { t.Auth.ID = "AKIDANOTHER" }, cut: true},
		"the buckets it admits":          {change: func(t *delivery.Target) { t.Buckets = []string{"photos"} }, cut: true},
		"whether it is sent the trail":   {change: func(t *delivery.Target) { t.Audit = true }},
		"the buckets it admits, widened": {change: func(t *delivery.Target) { t.Buckets = append(t.Buckets, "logs-*") }},
		"the buckets it admits, in another order": {
			buckets: []string{"photos", "logs-*"},
			change:  func(t *delivery.Target) { t.Buckets = []string{"logs-*", "photos"} },
		},
		"a bucket it admitted, dropped from two": {
			buckets: []string{"photos", "logs-*"}, cut: true,
			change: func(t *delivery.Target) { t.Buckets = []string{"photos"} },
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				outbox := newFakeOutbox()
				outbox.define("site", delivery.KindS3)
				if tt.buckets != nil {
					outbox.changeTarget(func(t *delivery.Target) { t.Buckets = tt.buckets })
				}
				items := outbox.appendKeys(siteConsumer, "a")
				var before atomic.Pointer[delivery.Target]
				d := &waveDeliverer{instant: true, block: func(target delivery.Target, _ delivery.Item) bool {
					was := before.Load()
					return was != nil && target.SameConnection(*was) && slices.Equal(target.Buckets, was.Buckets)
				}}
				original := outbox.targets["site"]
				before.Store(&original)
				active := &atomic.Bool{}
				active.Store(true)
				stop := runWorker(t, waveWorker(outbox, d, 2, active))
				time.Sleep(2 * time.Second)
				outbox.changeTarget(tt.change)
				time.Sleep(10 * time.Second)
				synctest.Wait()
				ended, took := d.attempts()
				stop()

				if !tt.cut {
					if len(ended) != 0 || len(took) != 0 {
						t.Errorf("a change of no connection ended %v and sent %v", ended, took)
					}
					return
				}
				if len(ended) != 1 || ended[0].endpoint != original.Endpoint {
					t.Errorf("ended %v, want the one attempt to the target as it was", ended)
				}
				if len(took) != 1 {
					t.Fatalf("taken %v, want the item once, by the target as it is", took)
				}
				if name == "its endpoint" && took[0].endpoint == original.Endpoint {
					t.Errorf("the item was sent to the endpoint the target no longer has")
				}
				if got := outbox.cursor("site").Acked; items[0].Seq.After(got) {
					t.Errorf("the cursor is at %v", got)
				}
			})
		})
	}
}

// TestDeliveryStateIsOfTheItemThatFailsMost (PLAN 8.1.4 review): the items
// of a wave are paused each by its own failures, and the target is shown
// by the one that has failed most - an item of its wave that is taken does
// not show it ok while another is still tried again.
func TestDeliveryStateIsOfTheItemThatFailsMost(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		outbox := newFakeOutbox()
		outbox.define("site", delivery.KindS3)
		outbox.appendKeys(siteConsumer, "a", "b")
		d := &waveDeliverer{instant: true, fail: func(seq uint64, attempt int) bool { return seq == 2 || attempt <= 2 }}
		active := &atomic.Bool{}
		active.Store(true)
		worker := waveWorker(outbox, d, 2, active)
		stop := runWorker(t, worker)
		defer stop()
		time.Sleep(100 * time.Millisecond)
		for range 120 {
			time.Sleep(500 * time.Millisecond)
			statuses, err := worker.DeliveryStatus(t.Context(), []string{"site"})
			if err != nil || len(statuses) != 1 {
				t.Fatalf("DeliveryStatus = %+v, %v", statuses, err)
			}
			if statuses[0].State == ports.DeliveryOK {
				t.Fatalf("the target is shown ok while row 2 fails: %+v", statuses[0])
			}
		}
		// Row 2 failed on its own count: in a minute, a pause of 1, 2, 4,
		// 8, 16 and 32 seconds - six or seven attempts, not the fewer a
		// count shared with row 1 would give.
		d.mu.Lock()
		tried := d.tried[2]
		d.mu.Unlock()
		if tried < 6 {
			t.Errorf("row 2 was tried %d times in a minute", tried)
		}
	})
}

// TestDeliveryAWaveCutOff (PLAN 8.1.4 review): a wave that is out when the
// worker stops, or when the node stops leading, has nothing of it
// acknowledged - not even what it delivered - and whoever delivers next
// sends it again: the wave, and nothing that was acknowledged.
func TestDeliveryAWaveCutOff(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"the worker stops", "the lead is lost"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				outbox := newFakeOutbox()
				outbox.define("site", delivery.KindS3)
				items := outbox.appendKeys(siteConsumer, "a", "b", "c", "d", "e")
				var cut atomic.Bool
				cut.Store(true)
				// Rows 1 and 2 are taken; row 3 of the same wave is out
				// until the cut.
				d := &waveDeliverer{instant: true, block: func(_ delivery.Target, item delivery.Item) bool {
					return cut.Load() && item.Seq.Index == 3
				}}
				active := &atomic.Bool{}
				active.Store(true)
				stop := runWorker(t, waveWorker(outbox, d, 3, active))
				time.Sleep(2 * time.Second)
				synctest.Wait()
				if how == "the worker stops" {
					stop()
				} else {
					active.Store(false)
					time.Sleep(3 * time.Second)
					synctest.Wait()
				}
				for _, ack := range outbox.acked() {
					if ack.upto.After(delivery.Seq{}) || len(ack.results) > 0 {
						t.Errorf("an acknowledgement of a wave that was cut off: %+v", ack)
					}
				}
				cut.Store(false)
				if how == "the worker stops" {
					stop = runWorker(t, waveWorker(outbox, d, 3, active))
				} else {
					active.Store(true)
				}
				time.Sleep(20 * time.Second)
				synctest.Wait()
				stop()
				handed := d.handed()
				for _, row := range []uint64{1, 2, 3} {
					if n := countOf(handed, row); n != 2 {
						t.Errorf("row %d of the wave was handed over %d times, want twice: %v", row, n, handed)
					}
				}
				for _, row := range []uint64{4, 5} {
					if n := countOf(handed, row); n != 1 {
						t.Errorf("row %d behind the wave was handed over %d times, want once: %v", row, n, handed)
					}
				}
				if got := outbox.cursor("site").Acked; items[len(items)-1].Seq.After(got) {
					t.Errorf("the cursor is at %v", got)
				}
			})
		})
	}
}

// countOf counts row in rows.
func countOf(rows []uint64, row uint64) int {
	n := 0
	for _, r := range rows {
		if r == row {
			n++
		}
	}
	return n
}

// TestDeliveryAWaitingItemAcknowledgesTheWavesBefore: an item of a wave that
// waits between attempts acknowledges, from its own goroutine, what the
// waves before it delivered - their place and their results - and nothing
// of its own wave.
func TestDeliveryAWaitingItemAcknowledgesTheWavesBefore(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		outbox := newFakeOutbox()
		outbox.define("site", delivery.KindS3)
		items := outbox.appendKeys(siteConsumer, "a", "b", "c", "d")
		d := &waveDeliverer{instant: true, fail: func(seq uint64, attempt int) bool { return seq == 3 && attempt <= 3 }}
		active := &atomic.Bool{}
		active.Store(true)
		stop := runWorker(t, waveWorker(outbox, d, 2, active))
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		acks := outbox.acked()
		if len(acks) != 1 || acks[0].upto != items[1].Seq || len(acks[0].results) != 2 {
			t.Errorf("while row 3 waits: acknowledgements %+v, want one up to row 2 with its two results", acks)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		stop()
		if got := outbox.cursor("site").Acked; items[len(items)-1].Seq.After(got) {
			t.Errorf("the cursor is at %v", got)
		}
	})
}
