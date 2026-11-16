package app_test

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/delivery"
)

// namingDeliverer is a waveDeliverer that names the object of each item as
// names says; a key it does not name is named by nothing.
type namingDeliverer struct {
	*waveDeliverer
	names map[string]string
}

func (d namingDeliverer) ObjectOf(_ context.Context, _ delivery.Target, item delivery.Item) (string, bool) {
	name, ok := d.names[item.Key]
	return name, ok
}

// TestDeliveryWavesKeepApartWhatTheDelivererNamesOne (PLAN 8.1.4 review):
// two items the Deliverer names one object - two buckets here sent to one
// bucket there - are never in one wave, though their keys here differ; an
// item it names by nothing goes alone.
func TestDeliveryWavesKeepApartWhatTheDelivererNamesOne(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		outbox := newFakeOutbox()
		outbox.define("site", delivery.KindS3)
		outbox.appendKeys(siteConsumer, "a", "b", "c", "x", "d")
		d := namingDeliverer{waveDeliverer: &waveDeliverer{}, names: map[string]string{"a": "one", "b": "one", "c": "two", "d": "three"}}
		active := &atomic.Bool{}
		active.Store(true)
		stop := runWorker(t, app.NewDeliveryWorker(app.DeliveryWorkerConfig{
			Targets: outbox, Outbox: outbox, Clock: bubbleClock{}, Active: active.Load,
			Deliverers: map[delivery.ConsumerKind]app.Deliverer{delivery.ConsumerReplication: d},
			Parallel:   map[delivery.ConsumerKind]int{delivery.ConsumerReplication: 4},
		}))
		time.Sleep(30 * time.Second)
		synctest.Wait()
		stop()
		// The waves: a | b c | x | d.
		if got := d.handed(); !sameRows(got, []uint64{1, 2, 3, 4, 5}, [][2]int{{0, 1}, {1, 3}, {3, 4}, {4, 5}}) || d.most != 2 {
			t.Errorf("handed over %v, at most %d at once; want the waves [1] [2 3] [4] [5]", got, d.most)
		}
	})
}
