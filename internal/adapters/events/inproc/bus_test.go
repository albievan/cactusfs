package inproc_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/events/inproc"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/ports"
)

type collected struct {
	mu  sync.Mutex
	ids []string
}

func (c *collected) add(_ context.Context, e ports.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, e.ID)
}

func (c *collected) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ids...)
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBusDeliversInOrderToEveryConsumer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	bus := inproc.New(slog.New(slog.DiscardHandler))
	var first, second collected
	bus.Subscribe("first", first.add)
	// A consumer that fails does not stop the others or the bus.
	bus.Subscribe("broken", func(context.Context, ports.Event) { panic("consumer bug") })
	bus.Subscribe("second", second.add)

	// Events published before the bus runs wait for it.
	for i := range 100 {
		if err := bus.Publish(ctx, ports.Event{ID: fmt.Sprint(i), Type: ports.EventBucketCreated}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	done := make(chan struct{})
	go func() { bus.Run(ctx); close(done) }()
	eventually(t, func() bool { return len(second.get()) == 100 })
	for i, id := range first.get() {
		if id != fmt.Sprint(i) {
			t.Fatalf("event %d delivered as number %d", i, i)
		}
	}
	if len(first.get()) != 100 {
		t.Errorf("first consumer saw %d events", len(first.get()))
	}
	cancel()
	<-done
}

func TestPublishNeverBlocks(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	bus := inproc.New(slog.New(slog.DiscardHandler))
	// Nobody is delivering. The publisher - the metadata state machine -
	// must not be held up; what does not fit is dropped and counted.
	var dropped uint64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 10_000 {
			if err := bus.Publish(ctx, ports.Event{ID: fmt.Sprint(i)}); err != nil {
				if !errors.Is(err, domain.ErrUnavailable) {
					t.Errorf("Publish = %v", err)
				}
				dropped++
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Publish blocked")
	}
	if dropped == 0 || bus.Dropped() != dropped {
		t.Errorf("dropped %d, counted %d", dropped, bus.Dropped())
	}
}

func TestDeduplicate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	var seen collected
	handler := inproc.Deduplicate(seen.add)
	for _, id := range []string{"a", "b", "a", "c", "b", "a"} {
		handler(ctx, ports.Event{ID: id})
	}
	if got := seen.get(); len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("delivered %v, want a b c", got)
	}
	// It remembers a bounded number of IDs; within that, nothing repeats.
	for round := range 2 {
		for i := range 500 {
			handler(ctx, ports.Event{ID: fmt.Sprintf("n%d", i)})
		}
		if got := len(seen.get()); got != 503 {
			t.Errorf("round %d: %d events delivered, want 503", round, got)
		}
	}
}
