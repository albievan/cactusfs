// Package inproc is the default event publisher: it delivers events to
// subscribers inside the process.
//
// Delivery is at most once. Events are queued without blocking the publisher
// - the publisher is the metadata state machine, which must never wait for a
// consumer - and an event that finds the queue full is dropped and counted.
// Nothing that must not be missed may depend on this bus alone; durable
// delivery to the outside (bucket notifications, audit export) reads the
// applied log through its own adapter.
package inproc

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/ports"
)

// queueSize is the number of events that can wait for delivery.
const queueSize = 4096

// Handler consumes one event. It is called from a single goroutine, in the
// order the events were published.
type Handler func(ctx context.Context, e ports.Event)

// Bus is an in-process event publisher.
type Bus struct {
	logger  *slog.Logger
	queue   chan ports.Event
	dropped atomic.Uint64

	mu          sync.RWMutex
	subscribers []subscriber
}

type subscriber struct {
	name    string
	handler Handler
}

var _ ports.EventPublisher = (*Bus)(nil)

// New returns a Bus. Events are delivered while Run is running.
func New(logger *slog.Logger) *Bus {
	return &Bus{logger: logger, queue: make(chan ports.Event, queueSize)}
}

// Subscribe adds a consumer. name identifies it in logs.
func (b *Bus) Subscribe(name string, handler Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers = append(b.subscribers, subscriber{name: name, handler: handler})
}

// Publish implements ports.EventPublisher. It never blocks. When the queue
// is full the event is dropped and the error wraps domain.ErrUnavailable.
func (b *Bus) Publish(_ context.Context, e ports.Event) error {
	select {
	case b.queue <- e:
		return nil
	default:
		b.dropped.Add(1)
		return fmt.Errorf("%w: the event queue is full", domain.ErrUnavailable)
	}
}

// Dropped returns the number of events that were dropped because the queue
// was full.
func (b *Bus) Dropped() uint64 {
	return b.dropped.Load()
}

// Run delivers events until ctx is cancelled.
func (b *Bus) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-b.queue:
			b.deliver(ctx, e)
		}
	}
}

func (b *Bus) deliver(ctx context.Context, e ports.Event) {
	b.mu.RLock()
	subscribers := b.subscribers
	b.mu.RUnlock()
	for _, s := range subscribers {
		b.call(ctx, s, e)
	}
}

// call runs one handler. A consumer that fails must not take the others, or
// the node, down with it.
func (b *Bus) call(ctx context.Context, s subscriber, e ports.Event) {
	defer func() {
		if r := recover(); r != nil {
			b.logger.ErrorContext(ctx, "event consumer failed",
				slog.String("consumer", s.name), slog.String("event_id", e.ID), slog.String("type", e.Type), slog.Any("panic", r))
		}
	}()
	s.handler(ctx, e)
}

// remembered is the number of event IDs a deduplicating consumer keeps.
const remembered = 1024

// Deduplicate wraps a handler so that it sees every event once, by event ID:
// consumers must be idempotent, and this is the simplest way to be. It
// remembers the most recent IDs; an event older than that which arrives
// again is not recognised.
func Deduplicate(next Handler) Handler {
	var mu sync.Mutex
	seen := make(map[string]struct{}, remembered)
	order := make([]string, 0, remembered)
	return func(ctx context.Context, e ports.Event) {
		mu.Lock()
		if _, dup := seen[e.ID]; dup {
			mu.Unlock()
			return
		}
		if len(order) == remembered {
			delete(seen, order[0])
			order = order[1:]
		}
		seen[e.ID] = struct{}{}
		order = append(order, e.ID)
		mu.Unlock()
		next(ctx, e)
	}
}
