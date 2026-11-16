package app_test

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
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
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// deliveryMark is in the words of every failure a deliverer of these tests
// gives, and in the endpoint of every target: neither may be found in a
// status or in the log.
const deliveryMark = "MARK-9d41"

// markedEndpoint is the origin of every target of these tests.
const markedEndpoint = "https://" + deliveryMark + ".example"

// bubbleClock is the time of the synctest bubble a test runs in.
type bubbleClock struct{}

func (bubbleClock) Now() time.Time { return time.Now() }

// ackCall is one acknowledgement the fake outbox was given.
type ackCall struct {
	target  string
	upto    delivery.Seq
	audit   uint64
	results []delivery.Result
}

// fakeOutbox is the targets, the outbox and its cursors of a deployment, in
// memory, as the ports say they behave. Every call must carry a deadline;
// one that does not is recorded.
type fakeOutbox struct {
	mu      sync.Mutex
	targets map[string]delivery.Target
	items   []delivery.Item
	cursors map[string]delivery.Cursor
	acks    []ackCall
	dropped uint64
	// undeadlined names the calls that came without a deadline.
	undeadlined []string
	// readFails, while set, fails every read of the outbox; ackFails every
	// acknowledgement.
	readFails error
	ackFails  error
	// bound, when not zero, is the most rows the outbox holds: past it the
	// oldest goes, counted in dropped and lost to the targets that had
	// not passed it, as the store does (delivery.Cursor.Dropped).
	bound int
}

func newFakeOutbox() *fakeOutbox {
	return &fakeOutbox{targets: map[string]delivery.Target{}, cursors: map[string]delivery.Cursor{}}
}

// deadline records a call without one.
func (f *fakeOutbox) deadline(ctx context.Context, call string) {
	if _, ok := ctx.Deadline(); !ok {
		f.undeadlined = append(f.undeadlined, call)
	}
}

// define defines a target, whose cursor begins after every row appended
// so far, as that of a target the entry that defines it begins.
func (f *fakeOutbox) define(name string, kind delivery.Kind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets[name] = delivery.Target{Name: name, Kind: kind, Endpoint: markedEndpoint, Buckets: []string{"*"}}
	cursor := delivery.Cursor{Target: name}
	if len(f.items) > 0 {
		cursor.From = f.items[len(f.items)-1].Seq
	}
	f.cursors[name] = cursor
}

// append adds n rows for the consumer, each made a second after the one
// before, and returns them.
func (f *fakeOutbox) append(n int, to ...delivery.Consumer) []delivery.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	made := make([]delivery.Item, n)
	for i := range n {
		index := uint64(len(f.items) + 1)
		item := delivery.Item{
			Seq: delivery.Seq{Index: index}, Event: delivery.EventPut, Bucket: "photos", Key: fmt.Sprintf("k%04d", index),
			Version: object.VersionRef{Null: true}, Size: 1, At: time.Unix(0, 0).Add(time.Duration(len(f.items)+1) * time.Second).UTC(), To: to,
		}
		f.items = append(f.items, item)
		made[i] = item
		for f.bound > 0 && len(f.items) > f.bound {
			oldest := f.items[0]
			f.items = slices.Delete(f.items, 0, 1)
			f.dropped++
			for name, cursor := range f.cursors {
				f.cursors[name] = cursor.Dropped(oldest)
			}
		}
	}
	return made
}

// restore sets the count of rows dropped, as a state restored from an
// older snapshot has it.
func (f *fakeOutbox) restore(dropped uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = dropped
}

// lose counts n rows dropped and lost to a target, as rows of it alone that
// were dropped at the bound would be.
func (f *fakeOutbox) lose(target string, n uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cursor := f.cursors[target]
	cursor.Lost += n
	f.cursors[target] = cursor
	f.dropped += n
}

// PutTarget implements ports.TargetStore.
func (f *fakeOutbox) PutTarget(ctx context.Context, t delivery.Target, _ audit.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadline(ctx, "PutTarget")
	f.targets[t.Name] = t
	return nil
}

// DeleteTarget implements ports.TargetStore: the cursor goes with the
// target.
func (f *fakeOutbox) DeleteTarget(ctx context.Context, name string, _ audit.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadline(ctx, "DeleteTarget")
	if _, ok := f.targets[name]; !ok {
		return domain.ErrEntityNotFound
	}
	delete(f.targets, name)
	delete(f.cursors, name)
	return nil
}

// GetTarget implements ports.TargetStore.
func (f *fakeOutbox) GetTarget(ctx context.Context, name string) (delivery.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadline(ctx, "GetTarget")
	t, ok := f.targets[name]
	if !ok {
		return delivery.Target{}, fmt.Errorf("%w: target %s", domain.ErrEntityNotFound, name)
	}
	return t, nil
}

// ListTargets implements ports.TargetStore.
func (f *fakeOutbox) ListTargets(ctx context.Context) ([]delivery.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadline(ctx, "ListTargets")
	out := make([]delivery.Target, 0, len(f.targets))
	for _, t := range f.targets {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b delivery.Target) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ReadOutbox implements ports.Outbox, looking at 4 x limit rows at most.
func (f *fakeOutbox) ReadOutbox(ctx context.Context, target string, after delivery.Seq, limit int) ([]delivery.Item, delivery.Seq, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadline(ctx, "ReadOutbox")
	if f.readFails != nil {
		return nil, delivery.Seq{}, f.readFails
	}
	cursor, ok := f.cursors[target]
	if !ok {
		return nil, delivery.Seq{}, fmt.Errorf("%w: target %s", domain.ErrEntityNotFound, target)
	}
	from := after
	if cursor.From.After(from) {
		from = cursor.From
	}
	var items []delivery.Item
	upto, looked := after, 0
	for _, item := range f.items {
		if !item.Seq.After(from) {
			continue
		}
		if len(items) == limit || looked == 4*limit {
			break
		}
		looked++
		upto = item.Seq
		if _, mine := item.For(target); mine {
			items = append(items, item.Clone())
		}
	}
	return items, upto, nil
}

// AckOutbox implements ports.Outbox.
func (f *fakeOutbox) AckOutbox(ctx context.Context, target string, upto delivery.Seq, auditUpto uint64, results []delivery.Result) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadline(ctx, "AckOutbox")
	if f.ackFails != nil {
		return 0, f.ackFails
	}
	cursor, ok := f.cursors[target]
	if !ok {
		return 0, fmt.Errorf("%w: target %s", domain.ErrEntityNotFound, target)
	}
	if len(results) > delivery.MaxAckResults {
		return 0, fmt.Errorf("%w: %d results in one acknowledgement", domain.ErrInvalidArgument, len(results))
	}
	if len(results) > 0 && f.targets[target].Kind != delivery.KindS3 {
		return 0, fmt.Errorf("%w: results for a target of kind %s", domain.ErrInvalidArgument, f.targets[target].Kind)
	}
	if len(f.items) > 0 {
		if last := f.items[len(f.items)-1].Seq; upto.After(last) {
			upto = last
		}
	}
	// The cursor of the audit trail moves for a target that is sent it,
	// and for no other.
	if !f.targets[target].Audit {
		auditUpto = 0
	}
	f.cursors[target] = cursor.Advanced(upto, auditUpto)
	f.acks = append(f.acks, ackCall{target: target, upto: upto, audit: auditUpto, results: slices.Clone(results)})
	return 0, nil
}

// OutboxCursors implements ports.Outbox.
func (f *fakeOutbox) OutboxCursors(ctx context.Context) (ports.OutboxState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadline(ctx, "OutboxCursors")
	state := ports.OutboxState{Rows: uint64(len(f.items)), Dropped: f.dropped}
	for _, cursor := range f.cursors {
		state.Cursors = append(state.Cursors, cursor)
	}
	slices.SortFunc(state.Cursors, func(a, b delivery.Cursor) int { return strings.Compare(a.Target, b.Target) })
	return state, nil
}

// cursor returns where the cursor of a target is.
func (f *fakeOutbox) cursor(target string) delivery.Cursor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursors[target]
}

// acked returns the acknowledgements so far.
func (f *fakeOutbox) acked() []ackCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.acks)
}

func (f *fakeOutbox) withoutDeadline() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.undeadlined)
}

// delivered is one attempt a deliverer was given.
type delivered struct {
	target string
	seq    uint64
}

// fakeDeliverer records every attempt and answers as answer says; one
// without a deadline is recorded.
type fakeDeliverer struct {
	mu       sync.Mutex
	attempts []delivered
	// answer says what the attempt-th attempt (from 1) at an item gives.
	answer      func(item delivery.Item, attempt int) (object.ReplicationStatus, error)
	tried       map[uint64]int
	undeadlined int
	// onCall, when not nil, is called with the number of the call, from 1.
	onCall func(call int)
}

func (d *fakeDeliverer) Deliver(ctx context.Context, target delivery.Target, item delivery.Item) (object.ReplicationStatus, error) {
	d.mu.Lock()
	if _, ok := ctx.Deadline(); !ok {
		d.undeadlined++
	}
	if d.tried == nil {
		d.tried = map[uint64]int{}
	}
	d.tried[item.Seq.Index]++
	attempt := d.tried[item.Seq.Index]
	d.attempts = append(d.attempts, delivered{target: target.Name, seq: item.Seq.Index})
	calls := len(d.attempts)
	answer, onCall := d.answer, d.onCall
	d.mu.Unlock()
	if onCall != nil {
		onCall(calls)
	}
	if answer == nil {
		return "", nil
	}
	return answer(item, attempt)
}

func (d *fakeDeliverer) calls() []delivered {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.attempts)
}

// seqs returns the rows the deliverer was given for a target, in order,
// each attempt once.
func (d *fakeDeliverer) seqs(target string) []uint64 {
	var out []uint64
	for _, call := range d.calls() {
		if call.target == target {
			out = append(out, call.seq)
		}
	}
	return out
}

// failing is a failure of the class whose words name an endpoint.
func failing(class delivery.Class) error {
	return delivery.Failed(class, fmt.Errorf("post %s/hooks/token-%s: connection refused", markedEndpoint, deliveryMark))
}

// deliveryWorld is a worker over a fake outbox and deliverer, in a synctest
// bubble.
type deliveryWorld struct {
	outbox    *fakeOutbox
	deliverer *fakeDeliverer
	worker    *app.DeliveryWorker
	active    *atomic.Bool
	logs      *syncBuffer
	attempts  *attemptLog
}

// attemptLog counts the attempts as OnAttempt is told of them.
type attemptLog struct {
	mu   sync.Mutex
	seen map[string]int
}

func (a *attemptLog) add(kind delivery.Kind, class delivery.Class) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen == nil {
		a.seen = map[string]int{}
	}
	result := string(class)
	if class == delivery.ClassNone {
		result = "ok"
	}
	a.seen[string(kind)+"/"+result]++
}

func (a *attemptLog) count(key string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen[key]
}

func newDeliveryWorld(outbox *fakeOutbox, deliverer *fakeDeliverer, kinds ...delivery.ConsumerKind) *deliveryWorld {
	w := &deliveryWorld{outbox: outbox, deliverer: deliverer, active: &atomic.Bool{}, logs: &syncBuffer{}, attempts: &attemptLog{}}
	w.active.Store(true)
	deliverers := map[delivery.ConsumerKind]app.Deliverer{}
	for _, kind := range kinds {
		deliverers[kind] = deliverer
	}
	w.worker = app.NewDeliveryWorker(app.DeliveryWorkerConfig{
		Targets: outbox, Outbox: outbox, Deliverers: deliverers, Active: w.active.Load, Clock: bubbleClock{},
		Logger: slog.New(slog.NewJSONHandler(w.logs, nil)), OnAttempt: w.attempts.add,
	})
	return w
}

// run runs the worker in the bubble until stop is called; stop waits for it
// to have returned.
func (w *deliveryWorld) run(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.worker.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// status returns the status of one target, as the worker has it.
func (w *deliveryWorld) status(t *testing.T, name string) ports.TargetStatus {
	t.Helper()
	statuses, err := w.worker.DeliveryStatus(t.Context(), []string{name})
	if err != nil || len(statuses) != 1 {
		t.Fatalf("DeliveryStatus(%s) = %+v, %v", name, statuses, err)
	}
	return statuses[0]
}

func indexes(items []delivery.Item) []uint64 {
	out := make([]uint64, len(items))
	for i, item := range items {
		out[i] = item.Seq.Index
	}
	return out
}

var hookConsumer = delivery.Consumer{Kind: delivery.ConsumerNotification, Target: "hook"}

// TestDeliveryWorker: the node that leads hands every row of a target to the
// deliverer of its kind, in order; a failure is tried again and holds
// everything behind it; a refusal of one item passes it and is counted; the
// cursor is moved forward every batch or second and never past an item that
// was not taken; how each target stands is kept.
func TestDeliveryWorker(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		rows   int
		kind   delivery.Kind
		answer func(item delivery.Item, attempt int) (object.ReplicationStatus, error)
		// other rows, for another target, are interleaved with the
		// target's.
		other bool
		// noDeliverer registers nothing for the target's kind.
		noDeliverer bool
		run         time.Duration
		check       func(t *testing.T, w *deliveryWorld, items []delivery.Item, st ports.TargetStatus)
	}{
		"order kept": {
			rows: 250, kind: delivery.KindWebhook, run: 30 * time.Second,
			check: func(t *testing.T, w *deliveryWorld, items []delivery.Item, st ports.TargetStatus) {
				if got := w.deliverer.seqs("hook"); !slices.Equal(got, indexes(items)) {
					t.Errorf("delivered %v, want every row once in order", got)
				}
				if c := w.outbox.cursor("hook"); c.Acked != items[len(items)-1].Seq {
					t.Errorf("cursor at %v, want the last row", c.Acked)
				}
				acks := w.outbox.acked()
				for i := 1; i < len(acks); i++ {
					if !acks[i].upto.After(acks[i-1].upto) && acks[i].upto != acks[i-1].upto {
						t.Errorf("acknowledgement %d went back: %v after %v", i, acks[i].upto, acks[i-1].upto)
					}
				}
				if len(acks) < 3 {
					t.Errorf("%d acknowledgements of 250 rows, want one at least every 100", len(acks))
				}
				if st.State != ports.DeliveryOK || st.Pending != 0 || !st.OldestPendingAt.IsZero() || st.LastSuccessAt.IsZero() ||
					st.LastErrorClass != delivery.ClassNone || st.Rejected != 0 {
					t.Errorf("status = %+v", st)
				}
				if got := w.attempts.count("webhook/ok"); got != 250 {
					t.Errorf("attempts counted ok: %d, want 250", got)
				}
			},
		},
		"a failure is retried and nothing passed": {
			rows: 10, kind: delivery.KindWebhook, run: 30 * time.Second,
			answer: func(item delivery.Item, attempt int) (object.ReplicationStatus, error) {
				if item.Seq.Index == 3 && attempt <= 3 {
					return "", failing(delivery.ClassServer)
				}
				return "", nil
			},
			check: func(t *testing.T, w *deliveryWorld, items []delivery.Item, st ports.TargetStatus) {
				want := []uint64{1, 2, 3, 3, 3, 3, 4, 5, 6, 7, 8, 9, 10}
				if got := w.deliverer.seqs("hook"); !slices.Equal(got, want) {
					t.Errorf("delivered %v, want %v: the failed row again and nothing behind it meanwhile", got, want)
				}
				if c := w.outbox.cursor("hook"); c.Acked != items[len(items)-1].Seq {
					t.Errorf("cursor at %v, want the last row", c.Acked)
				}
				if st.State != ports.DeliveryOK || st.Pending != 0 || st.LastErrorClass != delivery.ClassNone {
					t.Errorf("status after the target came back = %+v", st)
				}
				if got := w.attempts.count("webhook/server"); got != 3 {
					t.Errorf("attempts counted as server: %d, want 3", got)
				}
			},
		},
		"a rejected item is passed and counted": {
			rows: 5, kind: delivery.KindWebhook, run: 10 * time.Second,
			answer: func(item delivery.Item, _ int) (object.ReplicationStatus, error) {
				if item.Seq.Index == 2 {
					return "", failing(delivery.ClassRejected)
				}
				return "", nil
			},
			check: func(t *testing.T, w *deliveryWorld, items []delivery.Item, st ports.TargetStatus) {
				if got := w.deliverer.seqs("hook"); !slices.Equal(got, indexes(items)) {
					t.Errorf("delivered %v, want every row once: a refusal of one is not tried again", got)
				}
				if c := w.outbox.cursor("hook"); c.Acked != items[len(items)-1].Seq {
					t.Errorf("cursor at %v, want past the refused row", c.Acked)
				}
				if st.Rejected != 1 || st.State != ports.DeliveryOK || st.Pending != 0 {
					t.Errorf("status = %+v, want one rejected, ok", st)
				}
			},
		},
		"a credential refused stalls the target": {
			rows: 4, kind: delivery.KindWebhook, run: 20 * time.Second,
			answer: func(delivery.Item, int) (object.ReplicationStatus, error) {
				return "", failing(delivery.ClassAuth)
			},
			check: func(t *testing.T, w *deliveryWorld, items []delivery.Item, st ports.TargetStatus) {
				for _, seq := range w.deliverer.seqs("hook") {
					if seq != 1 {
						t.Errorf("row %d was sent while row 1 was refused", seq)
					}
				}
				if c := w.outbox.cursor("hook"); !c.Acked.IsZero() {
					t.Errorf("cursor at %v, want where it was", c.Acked)
				}
				if st.State != ports.DeliveryStalled || st.LastErrorClass != delivery.ClassAuth || st.Pending != 4 ||
					!st.OldestPendingAt.Equal(items[0].At) || !st.LastSuccessAt.IsZero() {
					t.Errorf("status = %+v", st)
				}
			},
		},
		"a destination that is full stalls the target": {
			rows: 2, kind: delivery.KindS3, run: 5 * time.Second,
			answer: func(delivery.Item, int) (object.ReplicationStatus, error) {
				return "", failing(delivery.ClassQuota)
			},
			check: func(t *testing.T, w *deliveryWorld, _ []delivery.Item, st ports.TargetStatus) {
				if st.State != ports.DeliveryStalled || st.LastErrorClass != delivery.ClassQuota || st.Pending != 2 {
					t.Errorf("status = %+v", st)
				}
				if !w.outbox.cursor("hook").Acked.IsZero() {
					t.Errorf("cursor moved past an item the destination had no room for")
				}
			},
		},
		"a target that does not answer is retried, then stalled": {
			rows: 2, kind: delivery.KindNATS, run: 20 * time.Second,
			answer: func(delivery.Item, int) (object.ReplicationStatus, error) {
				return "", failing(delivery.ClassTimeout)
			},
			check: func(t *testing.T, _ *deliveryWorld, _ []delivery.Item, st ports.TargetStatus) {
				// At 0, 1, 3, 7 and 15 s: five failures in 20 s, fewer
				// than DeliveryStallAfter.
				if st.State != ports.DeliveryRetrying || st.LastErrorClass != delivery.ClassTimeout || st.Pending != 2 {
					t.Errorf("status = %+v", st)
				}
			},
		},
		"rows of others are passed over": {
			rows: 3, kind: delivery.KindWebhook, other: true, run: 10 * time.Second,
			check: func(t *testing.T, w *deliveryWorld, items []delivery.Item, _ ports.TargetStatus) {
				if got := w.deliverer.seqs("hook"); !slices.Equal(got, indexes(items)) {
					t.Errorf("delivered %v, want the target's rows only", got)
				}
				if c := w.outbox.cursor("hook"); c.Acked.Index != uint64(2*len(items)) {
					t.Errorf("cursor at %v, want past the rows of the other target too", c.Acked)
				}
			},
		},
		"replication reports what became of each version": {
			rows: 4, kind: delivery.KindS3, run: 10 * time.Second,
			answer: func(item delivery.Item, _ int) (object.ReplicationStatus, error) {
				if item.Seq.Index == 2 {
					return object.ReplicationFailed, failing(delivery.ClassSSEC)
				}
				return object.ReplicationCompleted, nil
			},
			check: func(t *testing.T, w *deliveryWorld, _ []delivery.Item, st ports.TargetStatus) {
				var results []delivery.Result
				for _, ack := range w.outbox.acked() {
					results = append(results, ack.results...)
				}
				want := []delivery.Result{
					{Seq: delivery.Seq{Index: 1}, Status: object.ReplicationCompleted},
					{Seq: delivery.Seq{Index: 2}, Status: object.ReplicationFailed},
					{Seq: delivery.Seq{Index: 3}, Status: object.ReplicationCompleted},
					{Seq: delivery.Seq{Index: 4}, Status: object.ReplicationCompleted},
				}
				if !slices.Equal(results, want) {
					t.Errorf("results acknowledged = %+v, want %+v", results, want)
				}
				if st.Failed != 1 || st.Rejected != 0 || st.State != ports.DeliveryOK {
					t.Errorf("status = %+v", st)
				}
			},
		},
		"a kind nothing delivers holds its target": {
			rows: 2, kind: delivery.KindWebhook, noDeliverer: true, run: 5 * time.Second,
			check: func(t *testing.T, w *deliveryWorld, _ []delivery.Item, st ports.TargetStatus) {
				if len(w.deliverer.calls()) != 0 || !w.outbox.cursor("hook").Acked.IsZero() {
					t.Errorf("something was delivered or acknowledged")
				}
				if st.State != ports.DeliveryRetrying || st.LastErrorClass != delivery.ClassInternal || st.Pending != 2 {
					t.Errorf("status = %+v", st)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				outbox := newFakeOutbox()
				outbox.define("hook", tt.kind)
				consumer := hookConsumer
				if tt.kind == delivery.KindS3 {
					consumer.Kind = delivery.ConsumerReplication
				}
				var items []delivery.Item
				if tt.other {
					outbox.define("other", delivery.KindWebhook)
					for range tt.rows {
						items = append(items, outbox.append(1, consumer)...)
						outbox.append(1, delivery.Consumer{Kind: delivery.ConsumerNotification, Target: "other"})
					}
				} else {
					items = outbox.append(tt.rows, consumer)
				}
				deliverer := &fakeDeliverer{answer: tt.answer}
				kinds := []delivery.ConsumerKind{delivery.ConsumerNotification, delivery.ConsumerReplication}
				if tt.noDeliverer {
					kinds = nil
				}
				w := newDeliveryWorld(outbox, deliverer, kinds...)
				stop := w.run(t)
				time.Sleep(tt.run)
				synctest.Wait()
				st := w.status(t, "hook")
				stop()
				tt.check(t, w, items, st)
				if missing := outbox.withoutDeadline(); len(missing) > 0 {
					t.Errorf("calls without a deadline: %v", missing)
				}
				if deliverer.undeadlined > 0 {
					t.Errorf("%d attempts without a deadline", deliverer.undeadlined)
				}
				if logs := w.logs.String(); strings.Contains(logs, deliveryMark) || strings.Contains(logs, "connection refused") {
					t.Errorf("the log holds the words of a failure or an endpoint:\n%s", logs)
				}
			})
		})
	}
}

// TestDeliveryNothingIsSentAfterTheLeadIsLost: a node that no longer leads
// sends nothing more - not the next item, not the next attempt - and says
// that it does not deliver; one that leads again begins anew.
func TestDeliveryNothingIsSentAfterTheLeadIsLost(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func(delivery.Item, int) (object.ReplicationStatus, error){
		"while items are taken": nil,
		"while an item is retried": func(delivery.Item, int) (object.ReplicationStatus, error) {
			return "", failing(delivery.ClassServer)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				outbox := newFakeOutbox()
				outbox.define("hook", delivery.KindWebhook)
				outbox.append(50, hookConsumer)
				deliverer := &fakeDeliverer{answer: answer}
				w := newDeliveryWorld(outbox, deliverer, delivery.ConsumerNotification)
				deliverer.onCall = func(call int) {
					if call == 3 {
						// The lead is lost while the third attempt is out.
						w.active.Store(false)
					}
				}
				stop := w.run(t)
				defer stop()
				time.Sleep(time.Minute)
				synctest.Wait()
				if calls := deliverer.calls(); len(calls) != 3 {
					t.Errorf("%d attempts, want 3: nothing after the lead was lost", len(calls))
				}
				if _, err := w.worker.DeliveryStatus(t.Context(), nil); !errors.Is(err, domain.ErrNotLeader) {
					t.Errorf("DeliveryStatus on a node that does not lead = %v, want %v", err, domain.ErrNotLeader)
				}
				if _, ok := w.worker.Summary(); ok {
					t.Error("a node that does not lead has a summary")
				}
				if acks := outbox.acked(); answer != nil && len(acks) != 0 && acks[len(acks)-1].upto.Index > 0 {
					t.Errorf("acknowledged %v past an item that failed", acks)
				}
			})
		})
	}
}

// TestDeliveryARestartResendsAtMostOneBatch: what the next leader sends again
// is what the last one delivered and had not acknowledged - at most one
// batch, or what one second delivered - and every row is delivered.
func TestDeliveryARestartResendsAtMostOneBatch(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		// takes is how long the target takes for an item.
		takes time.Duration
		// lostAt is the attempt during which the first leader loses its
		// lead.
		lostAt int
		most   int
	}{
		"a fast target":         {takes: 0, lostAt: 250, most: app.DeliveryBatch},
		"a target that is slow": {takes: 300 * time.Millisecond, lostAt: 25, most: 5},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				outbox := newFakeOutbox()
				outbox.define("hook", delivery.KindWebhook)
				items := outbox.append(400, hookConsumer)
				slow := func(delivery.Item, int) (object.ReplicationStatus, error) {
					time.Sleep(tt.takes)
					return "", nil
				}
				first := &fakeDeliverer{answer: slow}
				one := newDeliveryWorld(outbox, first, delivery.ConsumerNotification)
				first.onCall = func(call int) {
					if call == tt.lostAt {
						one.active.Store(false)
					}
				}
				stopOne := one.run(t)
				time.Sleep(10 * time.Minute)
				synctest.Wait()
				stopOne()

				second := &fakeDeliverer{answer: slow}
				two := newDeliveryWorld(outbox, second, delivery.ConsumerNotification)
				stopTwo := two.run(t)
				time.Sleep(10 * time.Minute)
				synctest.Wait()
				stopTwo()

				sentBefore := first.seqs("hook")
				sentAfter := second.seqs("hook")
				again := 0
				for _, seq := range sentAfter {
					if slices.Contains(sentBefore, seq) {
						again++
					}
				}
				if again > tt.most {
					t.Errorf("the second leader sent %d rows again, want at most %d", again, tt.most)
				}
				all := append(slices.Clone(sentBefore), sentAfter...)
				slices.Sort(all)
				all = slices.Compact(all)
				if !slices.Equal(all, indexes(items)) {
					t.Errorf("%d distinct rows delivered, want all %d", len(all), len(items))
				}
				if !slices.IsSorted(sentAfter) {
					t.Error("the second leader delivered out of order")
				}
			})
		})
	}
}

// TestDeliveryTellsNoErrorOrEndpoint is the marked URL test: the endpoint of
// a target and the words of a failure, both holding a mark, are in no status
// and no line of the log; the class of the failure is.
func TestDeliveryTellsNoErrorOrEndpoint(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		outbox := newFakeOutbox()
		outbox.define("hook", delivery.KindWebhook)
		outbox.append(3, hookConsumer)
		deliverer := &fakeDeliverer{answer: func(item delivery.Item, _ int) (object.ReplicationStatus, error) {
			if item.Seq.Index == 1 {
				return "", failing(delivery.ClassRejected)
			}
			return "", failing(delivery.ClassConnect)
		}}
		w := newDeliveryWorld(outbox, deliverer, delivery.ConsumerNotification)
		stop := w.run(t)
		time.Sleep(20 * time.Second)
		synctest.Wait()
		statuses, err := w.worker.DeliveryStatus(t.Context(), nil)
		summary, ok := w.worker.Summary()
		stop()
		if err != nil || len(statuses) != 1 || !ok {
			t.Fatalf("DeliveryStatus = %+v, %v; summary %v", statuses, err, ok)
		}
		for name, text := range map[string]string{
			"status": fmt.Sprintf("%+v %#v", statuses, statuses), "summary": fmt.Sprintf("%#v", summary), "log": w.logs.String(),
		} {
			if strings.Contains(text, deliveryMark) || strings.Contains(text, "connection refused") || strings.Contains(text, "hooks/token") {
				t.Errorf("the %s holds an endpoint or the words of a failure:\n%s", name, text)
			}
		}
		if !strings.Contains(w.logs.String(), `"class":"connect"`) || !strings.Contains(w.logs.String(), `"class":"rejected"`) {
			t.Errorf("the log does not name the classes of the failures:\n%s", w.logs.String())
		}
		if statuses[0].LastErrorClass != delivery.ClassConnect || statuses[0].Rejected != 1 {
			t.Errorf("status = %+v", statuses[0])
		}
	})
}

// TestDeliveryStatusAndSummary: the status names the targets asked for, in
// name order, finds one defined a moment ago, and leaves out what is no
// target; the summary counts targets by kind and state and says how long
// the oldest item has waited, with the counts of the outbox.
func TestDeliveryStatusAndSummary(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		outbox := newFakeOutbox()
		outbox.define("hook", delivery.KindWebhook)
		outbox.define("bus", delivery.KindNATS)
		outbox.append(2, delivery.Consumer{Kind: delivery.ConsumerNotification, Target: "bus"})
		outbox.dropped = 7
		deliverer := &fakeDeliverer{answer: func(delivery.Item, int) (object.ReplicationStatus, error) {
			return "", failing(delivery.ClassNoStream)
		}}
		w := newDeliveryWorld(outbox, deliverer, delivery.ConsumerNotification)
		stop := w.run(t)
		defer stop()
		time.Sleep(30 * time.Second)
		synctest.Wait()

		all, err := w.worker.DeliveryStatus(t.Context(), nil)
		if err != nil || len(all) != 2 || all[0].Name != "bus" || all[1].Name != "hook" {
			t.Fatalf("DeliveryStatus of all = %+v, %v", all, err)
		}
		if all[0].State != ports.DeliveryStalled || all[0].Kind != delivery.KindNATS || all[1].State != ports.DeliveryOK {
			t.Errorf("states = %+v", all)
		}
		some, err := w.worker.DeliveryStatus(t.Context(), []string{"hook", "none", "hook"})
		if err != nil || len(some) != 1 || some[0].Name != "hook" {
			t.Errorf("DeliveryStatus of a target, one that is none and the first again = %+v, %v", some, err)
		}
		// A target defined a moment ago is found when it is asked for.
		outbox.define("new", delivery.KindWebhook)
		if got, err := w.worker.DeliveryStatus(t.Context(), []string{"new"}); err != nil || len(got) != 1 || got[0].State != ports.DeliveryOK {
			t.Errorf("DeliveryStatus of a new target = %+v, %v", got, err)
		}

		summary, ok := w.worker.Summary()
		if !ok {
			t.Fatal("the node that leads has no summary")
		}
		if summary.Rows != 2 || summary.Dropped != 7 || summary.Targets[delivery.KindNATS][ports.DeliveryStalled] != 1 ||
			summary.Targets[delivery.KindWebhook][ports.DeliveryOK] != 2 {
			t.Errorf("summary = %+v", summary)
		}
		// The first row of bus was made at 1 s past the epoch of the fake.
		if waited := summary.OldestPending[delivery.KindNATS]; waited != time.Since(time.Unix(1, 0)) {
			t.Errorf("the oldest item of nats waited %s", waited)
		}
		if summary.OldestPending[delivery.KindWebhook] != 0 {
			t.Errorf("nothing of a webhook waits, the summary says %s", summary.OldestPending[delivery.KindWebhook])
		}
	})
}

// TestDeliveryCountsWhatWaitsBeyondTheBatch: a target further behind than one
// batch is shown with what waits for it beyond the batch in hand, up to the
// bound of the count; and a target that is deleted loses its loop.
func TestDeliveryCountsWhatWaitsBeyondTheBatch(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		outbox := newFakeOutbox()
		outbox.define("hook", delivery.KindWebhook)
		outbox.append(2500, hookConsumer)
		deliverer := &fakeDeliverer{answer: func(delivery.Item, int) (object.ReplicationStatus, error) {
			return "", failing(delivery.ClassServer)
		}}
		w := newDeliveryWorld(outbox, deliverer, delivery.ConsumerNotification)
		stop := w.run(t)
		defer stop()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if st := w.status(t, "hook"); st.Pending != 2500 || !st.OldestPendingAt.Equal(time.Unix(1, 0).UTC()) {
			t.Errorf("status of a target 2 500 rows behind = %+v", st)
		}

		if err := outbox.DeleteTarget(context.Background(), "hook", audit.Record{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if got, err := w.worker.DeliveryStatus(t.Context(), nil); err != nil || len(got) != 0 {
			t.Errorf("DeliveryStatus after the target was deleted = %+v, %v", got, err)
		}
	})
}

// TestDeliveryWhenTheOutboxCannotBeRead: a read that fails is tried again a
// second later, and logged without holding up the other targets.
func TestDeliveryWhenTheOutboxCannotBeRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		outbox := newFakeOutbox()
		outbox.define("hook", delivery.KindWebhook)
		outbox.append(3, hookConsumer)
		outbox.readFails = fmt.Errorf("%w: no leader", domain.ErrUnavailable)
		deliverer := &fakeDeliverer{}
		w := newDeliveryWorld(outbox, deliverer, delivery.ConsumerNotification)
		stop := w.run(t)
		defer stop()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if len(deliverer.calls()) != 0 || !strings.Contains(w.logs.String(), "the outbox could not be read") {
			t.Errorf("calls %v, log:\n%s", deliverer.calls(), w.logs.String())
		}
		outbox.mu.Lock()
		outbox.readFails = nil
		outbox.mu.Unlock()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if got := deliverer.seqs("hook"); !slices.Equal(got, []uint64{1, 2, 3}) {
			t.Errorf("delivered %v once the outbox could be read", got)
		}
	})
}

// TestDeliveryWhenAnAcknowledgementFails: what was delivered is acknowledged
// with the next acknowledgement, and nothing is sent again meanwhile; the
// pauses between attempts are drawn with jitter from the entropy given,
// within what delivery.Backoff allows.
func TestDeliveryWhenAnAcknowledgementFails(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		outbox := newFakeOutbox()
		outbox.define("hook", delivery.KindWebhook)
		items := outbox.append(3, hookConsumer)
		outbox.ackFails = fmt.Errorf("%w: no leader", domain.ErrUnavailable)
		var (
			mu       sync.Mutex
			attempts []time.Time
		)
		deliverer := app.DelivererFunc(func(_ context.Context, _ delivery.Target, item delivery.Item) (object.ReplicationStatus, error) {
			mu.Lock()
			defer mu.Unlock()
			if item.Seq.Index == 3 && len(attempts) < 4 {
				attempts = append(attempts, time.Now())
				return "", failing(delivery.ClassServer)
			}
			return "", nil
		})
		logs := &syncBuffer{}
		worker := app.NewDeliveryWorker(app.DeliveryWorkerConfig{
			Targets: outbox, Outbox: outbox, Deliverers: map[delivery.ConsumerKind]app.Deliverer{delivery.ConsumerNotification: deliverer},
			Clock: bubbleClock{}, Logger: slog.New(slog.NewJSONHandler(logs, nil)), Entropy: rand.Reader,
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			worker.Run(ctx)
		}()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if !outbox.cursor("hook").Acked.IsZero() || !strings.Contains(logs.String(), "could not be acknowledged") {
			t.Errorf("cursor %v while acknowledgements fail; log:\n%s", outbox.cursor("hook").Acked, logs.String())
		}
		outbox.mu.Lock()
		outbox.ackFails = nil
		outbox.mu.Unlock()
		time.Sleep(time.Minute)
		synctest.Wait()
		cancel()
		<-done
		if c := outbox.cursor("hook"); c.Acked != items[len(items)-1].Seq {
			t.Errorf("cursor at %v once acknowledgements are taken, want the last row", c.Acked)
		}
		mu.Lock()
		defer mu.Unlock()
		for i := 1; i < len(attempts); i++ {
			pause, longest := attempts[i].Sub(attempts[i-1]), delivery.Backoff(i, 0)
			if pause > longest || pause < longest*3/4 {
				t.Errorf("pause %d was %s, want from %s to %s", i, pause, longest*3/4, longest)
			}
		}
	})
}

// TestDeliveryALeadLostForAMoment: a lead lost between two of Run's looks
// and found again stops nothing for good - the loop sends nothing while the
// node does not lead, then goes on - and a target deleted and made again
// under its name within one refresh is delivered to from its new cursor.
func TestDeliveryALeadLostForAMoment(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		outbox := newFakeOutbox()
		outbox.define("hook", delivery.KindWebhook)
		items := outbox.append(50, hookConsumer)
		deliverer := &fakeDeliverer{}
		w := newDeliveryWorld(outbox, deliverer, delivery.ConsumerNotification)
		var sentWhileLost atomic.Int32
		deliverer.onCall = func(call int) {
			if !w.active.Load() {
				sentWhileLost.Add(1)
			}
			if call == 3 {
				w.active.Store(false)
				go func() {
					time.Sleep(100 * time.Millisecond)
					w.active.Store(true)
				}()
			}
		}
		stop := w.run(t)
		defer stop()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if got := deliverer.seqs("hook"); !slices.Equal(got, indexes(items)) {
			t.Errorf("delivered %v after a lead lost for a moment, want every row once in order", got)
		}
		if n := sentWhileLost.Load(); n != 0 {
			t.Errorf("%d attempts while the node did not lead", n)
		}
		if st := w.status(t, "hook"); st.State != ports.DeliveryOK || st.Pending != 0 {
			t.Errorf("status = %+v", st)
		}
		if c := outbox.cursor("hook"); c.Acked != items[len(items)-1].Seq {
			t.Errorf("cursor at %v, want the last row", c.Acked)
		}
	})
}

// TestDeliveryATargetMadeAgainUnderItsName: a target deleted, whose loop
// found it gone, and made again before the next refresh, gets a new loop
// from its new cursor.
func TestDeliveryATargetMadeAgainUnderItsName(t *testing.T) {
	t.Parallel()
	for name, ackFindsIt := range map[string]bool{
		// The loop is idle and finds the target gone at its next read.
		"found gone by a read": false,
		// The target is deleted while an item is out, which takes a
		// second: the loop finds it gone when it acknowledges the item.
		"found gone by an acknowledgement": true,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				outbox := newFakeOutbox()
				outbox.define("hook", delivery.KindWebhook)
				deliverer := &fakeDeliverer{}
				if ackFindsIt {
					outbox.append(1, hookConsumer)
					deliverer.answer = func(item delivery.Item, _ int) (object.ReplicationStatus, error) {
						if item.Seq.Index == 1 {
							if err := outbox.DeleteTarget(ctxWithDeadline(t), "hook", audit.Record{}); err != nil {
								t.Error(err)
							}
							time.Sleep(app.DeliveryAckInterval)
						}
						return "", nil
					}
				}
				w := newDeliveryWorld(outbox, deliverer, delivery.ConsumerNotification)
				stop := w.run(t)
				defer stop()
				time.Sleep(2500 * time.Millisecond)
				synctest.Wait()
				if !ackFindsIt {
					if err := outbox.DeleteTarget(ctxWithDeadline(t), "hook", audit.Record{}); err != nil {
						t.Fatal(err)
					}
				}
				// Before the refresh at 5 s, the loop finds the target gone
				// and the target is made again.
				time.Sleep(1500 * time.Millisecond)
				synctest.Wait()
				outbox.define("hook", delivery.KindWebhook)
				before := len(deliverer.calls())
				items := outbox.append(5, hookConsumer)
				time.Sleep(10 * time.Minute)
				synctest.Wait()
				if got := deliverer.seqs("hook")[before:]; !slices.Equal(got, indexes(items)) {
					t.Errorf("delivered %v to the target made again, want %v", got, indexes(items))
				}
				if st := w.status(t, "hook"); st.State != ports.DeliveryOK || st.Pending != 0 {
					t.Errorf("status = %+v", st)
				}
				if acks := outbox.acked(); len(acks) == 0 || acks[len(acks)-1].upto != items[len(items)-1].Seq {
					t.Errorf("acknowledgements %+v, want the last at the last row", acks)
				}
			})
		})
	}
}

// TestDeliveryHoldsWhileAcknowledgementsFail: an acknowledgement that fails
// holds the target as a failed attempt does - nothing beyond one batch is
// delivered meanwhile, and no acknowledgement carries more results than the
// store takes - and delivery goes on once acknowledgements are taken again.
func TestDeliveryHoldsWhileAcknowledgementsFail(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		outbox := newFakeOutbox()
		outbox.define("dst", delivery.KindS3)
		items := outbox.append(1500, delivery.Consumer{Kind: delivery.ConsumerReplication, Target: "dst"})
		outbox.ackFails = fmt.Errorf("%w: no quorum", domain.ErrUnavailable)
		deliverer := &fakeDeliverer{answer: func(delivery.Item, int) (object.ReplicationStatus, error) {
			return object.ReplicationCompleted, nil
		}}
		w := newDeliveryWorld(outbox, deliverer, delivery.ConsumerReplication)
		stop := w.run(t)
		defer stop()
		time.Sleep(time.Minute)
		synctest.Wait()
		if n := len(deliverer.calls()); n > app.DeliveryBatch {
			t.Errorf("%d items delivered while acknowledgements failed, want at most one batch of %d", n, app.DeliveryBatch)
		}
		if !strings.Contains(w.logs.String(), "could not be acknowledged") {
			t.Errorf("the failed acknowledgements are not logged:\n%s", w.logs.String())
		}
		outbox.mu.Lock()
		outbox.ackFails = nil
		outbox.mu.Unlock()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if got := deliverer.seqs("dst"); !slices.Equal(got, indexes(items)) {
			t.Errorf("%d attempts once acknowledgements were taken, want every row once in order", len(got))
		}
		results := 0
		for _, ack := range outbox.acked() {
			if len(ack.results) > app.DeliveryBatch {
				t.Errorf("an acknowledgement carried %d results, want at most %d", len(ack.results), app.DeliveryBatch)
			}
			results += len(ack.results)
		}
		if results != len(items) {
			t.Errorf("%d results acknowledged, want %d", results, len(items))
		}
		if c := outbox.cursor("dst"); c.Acked != items[len(items)-1].Seq {
			t.Errorf("cursor at %v, want the last row", c.Acked)
		}
	})
}

// ctxWithDeadline is a context with a deadline, as the fake outbox wants.
func ctxWithDeadline(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// neverAnswers is a target that takes a request and never answers it: each
// attempt ends with its deadline.
var neverAnswers = app.DelivererFunc(func(ctx context.Context, _ delivery.Target, _ delivery.Item) (object.ReplicationStatus, error) {
	<-ctx.Done()
	return "", delivery.Failed(delivery.ClassTimeout, ctx.Err())
})

// overflowWorld is a worker with an auditor, over a fake outbox and a target
// that never answers, in a synctest bubble.
type overflowWorld struct {
	outbox  *fakeOutbox
	worker  *app.DeliveryWorker
	auditor *app.Auditor
	trail   *porttest.MemIAM
	active  *atomic.Bool
	logs    *syncBuffer
}

func newOverflowWorld(outbox *fakeOutbox) *overflowWorld {
	w := &overflowWorld{outbox: outbox, trail: porttest.NewMemIAM(), active: &atomic.Bool{}, logs: &syncBuffer{}}
	w.active.Store(true)
	logger := slog.New(slog.NewJSONHandler(w.logs, nil))
	w.auditor = app.NewAuditor(w.trail, bubbleClock{}, logger)
	w.worker = app.NewDeliveryWorker(app.DeliveryWorkerConfig{
		Targets: outbox, Outbox: outbox, Active: w.active.Load, Clock: bubbleClock{}, Logger: logger,
		Deliverers: map[delivery.ConsumerKind]app.Deliverer{delivery.ConsumerNotification: neverAnswers},
		Auditor:    w.auditor,
	})
	return w
}

// run runs the worker and its auditor until stop is called. stop stops the
// worker first and then the auditor, as a node closes, and waits for what the
// auditor holds to be written.
func (w *overflowWorld) run(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	auditCtx, stopAuditing := context.WithCancel(t.Context())
	var working, auditing sync.WaitGroup
	working.Go(func() { w.worker.Run(ctx) })
	auditing.Go(func() { w.auditor.Run(auditCtx) })
	return func() {
		cancel()
		working.Wait()
		stopAuditing()
		auditing.Wait()
	}
}

// told returns the records that say rows of the outbox were dropped.
func (w *overflowWorld) told(t *testing.T) []audit.Record {
	t.Helper()
	records, err := w.trail.ListAudit(t.Context(), 0, ports.MaxAuditPage)
	if err != nil {
		t.Fatal(err)
	}
	var out []audit.Record
	for _, record := range records {
		if record.Resource == app.OutboxResource {
			out = append(out, record)
		}
	}
	return out
}

// TestDeliveryTellsOfRowsDroppedAtTheBound: an outbox at its bound loses its
// oldest rows to the targets that had not passed them (ADR-0025,
// "Overflow"). The node that leads says so in its log and in an audit record
// system/outbox, once a minute at most: what is dropped meanwhile is told,
// added up, once the minute has passed - or when its lead ends, or it stops,
// before the minute: what it counted is told then, since no other node will.
// What was dropped before the node came to lead is not told again, and
// nothing is told while nothing is dropped. No endpoint is named.
func TestDeliveryTellsOfRowsDroppedAtTheBound(t *testing.T) {
	t.Parallel()
	// bound is the most rows the outbox holds.
	const bound = 10
	// A step is done at a time of the bubble, from its start.
	type step struct {
		at time.Duration
		do func(w *overflowWorld)
	}
	appendRows := func(n int) func(w *overflowWorld) {
		return func(w *overflowWorld) { w.outbox.append(n, hookConsumer) }
	}
	lead := func(leads bool) func(w *overflowWorld) {
		return func(w *overflowWorld) { w.active.Store(leads) }
	}
	tests := map[string]struct {
		// dropped is what the outbox had dropped, and the target lost,
		// before the worker began.
		dropped uint64
		steps   []step
		// lost is what the target has lost at the end.
		lost uint64
		// runFor is how long the worker runs after the last step; zero is
		// five minutes. Then the node stops.
		runFor time.Duration
		// want are the details of the records, in order.
		want []map[string]string
		// ending are the records told when a lead ended: they do not wait
		// for the minute.
		ending map[int]bool
	}{
		"what was counted and not told is told when the lead ends": {
			steps: []step{
				{2 * time.Second, appendRows(15)},
				{22 * time.Second, appendRows(3)},
				{30 * time.Second, lead(false)},
			},
			lost: 8,
			want: []map[string]string{
				{"dropped": "5", "dropped_total": "5", "rows": "10", "targets": "hook=5"},
				{"dropped": "3", "dropped_total": "8", "rows": "10", "targets": "hook=3"},
			},
			ending: map[int]bool{1: true},
		},
		"what was counted and not told is told when the node stops": {
			steps: []step{
				{2 * time.Second, appendRows(15)},
				{22 * time.Second, appendRows(3)},
			},
			runFor: 10 * time.Second,
			lost:   8,
			want: []map[string]string{
				{"dropped": "5", "dropped_total": "5", "rows": "10", "targets": "hook=5"},
				{"dropped": "3", "dropped_total": "8", "rows": "10", "targets": "hook=3"},
			},
			ending: map[int]bool{1: true},
		},
		"nothing dropped, nothing told": {
			steps: []step{{2 * time.Second, appendRows(10)}},
		},
		"told at once, then once a minute at most": {
			steps: []step{
				{2 * time.Second, appendRows(15)},
				{10 * time.Second, appendRows(3)},
				{20 * time.Second, appendRows(3)},
				{30 * time.Second, appendRows(3)},
				{40 * time.Second, appendRows(3)},
				{50 * time.Second, appendRows(3)},
			},
			lost: 20,
			want: []map[string]string{
				{"dropped": "5", "dropped_total": "5", "rows": "10", "targets": "hook=5"},
				{"dropped": "15", "dropped_total": "20", "rows": "10", "targets": "hook=15"},
			},
		},
		"what was dropped before the node led is not told again": {
			dropped: 7,
			steps:   []step{{2 * time.Second, appendRows(10)}, {3 * time.Minute, appendRows(2)}},
			lost:    9,
			want:    []map[string]string{{"dropped": "2", "dropped_total": "9", "rows": "10", "targets": "hook=2"}},
		},
		"a lead lost and taken again tells no more than once a minute": {
			steps: []step{
				{2 * time.Second, appendRows(15)},
				{10 * time.Second, lead(false)},
				{12 * time.Second, appendRows(4)},
				{15 * time.Second, lead(true)},
				{20 * time.Second, appendRows(2)},
			},
			lost: 11,
			want: []map[string]string{
				{"dropped": "5", "dropped_total": "5", "rows": "10", "targets": "hook=5"},
				// The four dropped while the node did not lead were the
				// leader's to tell.
				{"dropped": "2", "dropped_total": "11", "rows": "10", "targets": "hook=2"},
			},
		},
		"a count that falls begins the counting again": {
			steps: []step{
				{2 * time.Second, appendRows(15)},
				{20 * time.Second, func(w *overflowWorld) { w.outbox.restore(1) }},
				{30 * time.Second, appendRows(3)},
			},
			lost: 8,
			want: []map[string]string{
				{"dropped": "5", "dropped_total": "5", "rows": "10", "targets": "hook=5"},
				{"dropped": "3", "dropped_total": "4", "rows": "10", "targets": "hook=3"},
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				outbox := newFakeOutbox()
				outbox.define("hook", delivery.KindWebhook)
				outbox.define("other", delivery.KindWebhook)
				outbox.bound, outbox.dropped = bound, tt.dropped
				outbox.cursors["hook"] = delivery.Cursor{Target: "hook", Lost: tt.dropped}
				w := newOverflowWorld(outbox)
				stop := w.run(t)
				for _, s := range tt.steps {
					time.Sleep(time.Until(start.Add(s.at)))
					s.do(w)
				}
				time.Sleep(cmp.Or(tt.runFor, 5*time.Minute))
				synctest.Wait()

				// The outbox stays at its bound; the target lost what it
				// dropped, the other target nothing.
				state, err := outbox.OutboxCursors(ctxWithDeadline(t))
				if err != nil || state.Rows > bound {
					t.Errorf("the outbox holds %d rows, %v; at most %d", state.Rows, err, bound)
				}
				if hook, other := outbox.cursor("hook"), outbox.cursor("other"); hook.Lost != tt.lost || other.Lost != 0 {
					t.Errorf("lost: hook %d, other %d; want %d and 0", hook.Lost, other.Lost, tt.lost)
				}
				stop()

				records := w.told(t)
				if len(records) != len(tt.want) {
					t.Fatalf("%d records of rows dropped, want %d: %+v", len(records), len(tt.want), records)
				}
				for i, record := range records {
					if record.Actor != "system" || record.Action != app.OutboxDroppedAction || record.Outcome != audit.Failed {
						t.Errorf("record %d = %+v", i, record)
					}
					for key, want := range tt.want[i] {
						if got := record.Details[key]; got != want {
							t.Errorf("record %d: %s = %q, want %q", i, key, got, want)
						}
					}
					if _, err := time.Parse(time.RFC3339, record.Details["since"]); err != nil {
						t.Errorf("record %d: since = %q: %v", i, record.Details["since"], err)
					}
					if i == 0 || tt.ending[i] {
						continue
					}
					if gap := record.Time.Sub(records[i-1].Time); gap < app.OutboxReportEvery {
						t.Errorf("record %d came %s after the one before", i, gap)
					}
				}
				logs := w.logs.String()
				if got := strings.Count(logs, "the outbox is at its bound"); got != len(tt.want) {
					t.Errorf("the log says %d times that rows were dropped, want %d", got, len(tt.want))
				}
				if strings.Contains(logs, deliveryMark) {
					t.Errorf("the log names the endpoint of a target:\n%s", logs)
				}
			})
		})
	}
}

// TestDeliveryNamesTheTargetsThatLostMostFirst: the detail that names the
// targets that lost rows is kept whole: it names those that lost most first,
// as many as fit in 512 bytes, and says how many it left out.
func TestDeliveryNamesTheTargetsThatLostMostFirst(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		// targets lost 1, 2, ... rows: target i lost i + 1.
		targets  int
		names    int
		more     string
		greatest string
	}{
		// Each "name=n" is 63 bytes and a comma: eight fit.
		"more than fit":   {targets: 20, names: 8, more: "12", greatest: "=20"},
		"all of them fit": {targets: 3, names: 3, greatest: "=3"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				outbox := newFakeOutbox()
				long := func(i int) string { return fmt.Sprintf("target-%02d-%s", i, strings.Repeat("x", 50)) }
				for i := range tt.targets {
					outbox.define(long(i), delivery.KindWebhook)
				}
				w := newOverflowWorld(outbox)
				stop := w.run(t)
				time.Sleep(2 * time.Second)
				for i := range tt.targets {
					outbox.lose(long(i), uint64(i)+1)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				stop()

				records := w.told(t)
				if len(records) != 1 {
					t.Fatalf("%d records, want one: %+v", len(records), records)
				}
				details := records[0].Details
				named := strings.Split(details["targets"], ",")
				if len(named) != tt.names || len(details["targets"]) > 512 || details["targets_more"] != tt.more {
					t.Errorf("targets %q (%d bytes), targets_more %q; want %d named and %q more",
						details["targets"], len(details["targets"]), details["targets_more"], tt.names, tt.more)
				}
				if !strings.HasPrefix(named[0], long(tt.targets-1)) || !strings.HasSuffix(named[0], tt.greatest) {
					t.Errorf("the first named is %q, want the one that lost most", named[0])
				}
				for i := 1; i < len(named); i++ {
					if !strings.HasPrefix(named[i], long(tt.targets-1-i)+"=") {
						t.Errorf("named %d is %q, want %s: the most first", i, named[i], long(tt.targets-1-i))
					}
				}
			})
		})
	}
}
