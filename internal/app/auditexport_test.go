package app_test

import (
	"context"
	"encoding/json"
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
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// trailMark is in every record of the trail of these tests: the log of the
// worker never holds it.
const trailMark = "TRAIL-5c27"

// fakeReceiver is a target the audit trail is sent to: it records every
// attempt at a record and answers as answer says; a notification it takes
// and forgets. One without a deadline is recorded.
type fakeReceiver struct {
	mu sync.Mutex
	// attempts are the sequences of every attempt, in order; taken the
	// messages it took.
	attempts    []uint64
	taken       []ports.Event
	tried       map[uint64]int
	undeadlined int
	// answer says what the attempt-th attempt (from 1) at a record gives:
	// a record of the trail, or any message when all is set.
	answer func(sequence uint64, attempt int) error
	// onCall, when not nil, is called with the number of the call, from 1.
	onCall func(call int)
}

// Publish implements ports.EventPublisher.
func (r *fakeReceiver) Publish(ctx context.Context, e ports.Event) error {
	r.mu.Lock()
	if _, ok := ctx.Deadline(); !ok {
		r.undeadlined++
	}
	if r.tried == nil {
		r.tried = map[uint64]int{}
	}
	if e.Type != ports.MessageAuditExported {
		// A notification of the outbox, taken.
		r.mu.Unlock()
		return nil
	}
	r.tried[e.Sequence]++
	attempt := r.tried[e.Sequence]
	r.attempts = append(r.attempts, e.Sequence)
	calls := len(r.attempts)
	answer, onCall := r.answer, r.onCall
	r.mu.Unlock()
	if onCall != nil {
		onCall(calls)
	}
	if answer != nil {
		if err := answer(e.Sequence, attempt); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.taken = append(r.taken, e)
	return nil
}

func (r *fakeReceiver) sent() []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.attempts)
}

func (r *fakeReceiver) messages() []ports.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.taken)
}

// held returns the sequences of the messages taken, once each, in order,
// as a receiver that deduplicates by the ID holds them; and the records
// read from their bodies.
func (r *fakeReceiver) held(t *testing.T) ([]uint64, []audit.Record) {
	t.Helper()
	seen := map[string]bool{}
	var sequences []uint64
	var records []audit.Record
	for _, e := range r.messages() {
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		body, ok := e.Data.(json.RawMessage)
		if !ok {
			t.Fatalf("message %s carries %T", e.ID, e.Data)
		}
		record, err := audit.ParseLine(body)
		if err != nil {
			t.Fatalf("message %s: %v", e.ID, err)
		}
		if e.Type != ports.MessageAuditExported || e.ID != app.AuditExportID(record.Sequence) || e.Sequence != record.Sequence {
			t.Fatalf("message %s of type %s, sequence %d, carries record %d", e.ID, e.Type, e.Sequence, record.Sequence)
		}
		sequences = append(sequences, record.Sequence)
		records = append(records, record)
	}
	return sequences, records
}

// appendTrail adds n records to the trail, a second apart, and returns
// them as the trail holds them.
func appendTrail(t *testing.T, trail *porttest.MemIAM, n int) []audit.Record {
	t.Helper()
	before, err := trail.ListAudit(ctxWithDeadline(t), 0, ports.MaxAuditPage)
	if err != nil {
		t.Fatal(err)
	}
	last := uint64(len(before))
	for i := range n {
		record := audit.Record{
			Time: time.Now().UTC(), Actor: "user/alice", Action: "admin:PutUser", Resource: "arn:cactusfs:admin:::user/" + trailMark,
			Outcome: audit.Succeeded, SourceIP: "10.0.0.1", Details: map[string]string{"n": fmt.Sprint(i)},
		}
		if err := trail.AppendAudit(ctxWithDeadline(t), record); err != nil {
			t.Fatal(err)
		}
	}
	records, err := trail.ListAudit(ctxWithDeadline(t), last, n)
	if err != nil || len(records) != n {
		t.Fatalf("the trail holds %d new records, %v", len(records), err)
	}
	return records
}

// sequences returns 1 + from, ..., to.
func sequences(from, to uint64) []uint64 {
	var out []uint64
	for s := from + 1; s <= to; s++ {
		out = append(out, s)
	}
	return out
}

// exportWorld is a worker that sends the audit trail, and delivers the
// outbox, over fakes, in a synctest bubble.
type exportWorld struct {
	outbox   *fakeOutbox
	trail    ports.AuditLog
	receiver *fakeReceiver
	worker   *app.DeliveryWorker
	active   *atomic.Bool
	logs     *syncBuffer
	attempts *attemptLog
}

func newExportWorld(outbox *fakeOutbox, trail ports.AuditLog, receiver *fakeReceiver) *exportWorld {
	w := &exportWorld{outbox: outbox, trail: trail, receiver: receiver, active: &atomic.Bool{}, logs: &syncBuffer{}, attempts: &attemptLog{}}
	w.active.Store(true)
	publishers := app.PublisherSourceFunc(func(context.Context, delivery.Target) (ports.EventPublisher, error) { return receiver, nil })
	w.worker = app.NewDeliveryWorker(app.DeliveryWorkerConfig{
		Targets: outbox, Outbox: outbox, Active: w.active.Load, Clock: bubbleClock{},
		Deliverers:  map[delivery.ConsumerKind]app.Deliverer{delivery.ConsumerNotification: app.NewNotifier(app.NotifierConfig{Publishers: publishers})},
		Logger:      slog.New(slog.NewJSONHandler(w.logs, nil)),
		OnAttempt:   w.attempts.add,
		AuditExport: app.NewAuditExporter(app.AuditExporterConfig{Trail: trail, Publishers: publishers}),
	})
	return w
}

func (w *exportWorld) run(t *testing.T) (stop func()) {
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

func (w *exportWorld) status(t *testing.T, name string) ports.TargetStatus {
	t.Helper()
	statuses, err := w.worker.DeliveryStatus(t.Context(), []string{name})
	if err != nil || len(statuses) != 1 {
		t.Fatalf("DeliveryStatus(%s) = %+v, %v", name, statuses, err)
	}
	return statuses[0]
}

// audited defines a target and says whether it is sent the audit trail.
func (f *fakeOutbox) audited(name string, kind delivery.Kind, on bool) {
	f.define(name, kind)
	f.sendTrail(name, on)
}

// sendTrail says whether a target is sent the audit trail, as a change of
// the target does.
func (f *fakeOutbox) sendTrail(name string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := f.targets[name]
	target.Audit = on
	f.targets[name] = target
}

// auditAcks returns the places of the audit trail acknowledged for a target.
func (f *fakeOutbox) auditAcks(target string) []uint64 {
	var out []uint64
	for _, ack := range f.acked() {
		if ack.target == target && ack.audit > 0 {
			out = append(out, ack.audit)
		}
	}
	return out
}

// TestAuditExport: a target that is sent the audit trail is sent every
// record from the one after its cursor on, in sequence, as the record's
// line with the ID audit-<sequence>; a failure of any class holds the record
// and everything behind it - none is passed, a refusal neither - and the
// cursor is moved every batch or second and never past a record that was
// not taken.
func TestAuditExport(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		kind     delivery.Kind
		notSent  bool
		records  int
		acked    uint64
		answer   func(sequence uint64, attempt int) error
		want     []uint64
		wantAck  uint64
		state    string
		class    delivery.Class
		attempts map[string]int
	}{
		"every record, in sequence, from the first": {
			kind: delivery.KindWebhook, records: 250, want: sequences(0, 250), wantAck: 250, state: ports.DeliveryOK,
			attempts: map[string]int{"webhook/ok": 250},
		},
		"from where the cursor is": {
			kind: delivery.KindWebhook, records: 120, acked: 40, want: sequences(40, 120), wantAck: 120, state: ports.DeliveryOK,
		},
		"to a NATS server": {
			kind: delivery.KindNATS, records: 30, want: sequences(0, 30), wantAck: 30, state: ports.DeliveryOK,
			attempts: map[string]int{"nats/ok": 30},
		},
		"a failure holds the record and everything behind it": {
			kind: delivery.KindWebhook, records: 10,
			answer: func(sequence uint64, attempt int) error {
				if sequence == 5 && attempt <= 3 {
					return failing(delivery.ClassServer)
				}
				return nil
			},
			want: sequences(0, 10), wantAck: 10, state: ports.DeliveryOK,
			attempts: map[string]int{"webhook/server": 3, "webhook/ok": 10},
		},
		"a refusal is not passed: the target stalls": {
			kind: delivery.KindWebhook, records: 10,
			answer: func(sequence uint64, _ int) error {
				if sequence == 3 {
					return failing(delivery.ClassRejected)
				}
				return nil
			},
			want: sequences(0, 2), wantAck: 2, state: ports.DeliveryStalled, class: delivery.ClassRejected,
		},
		"a credential refused stalls at once": {
			kind: delivery.KindNATS, records: 4,
			answer: func(uint64, int) error { return failing(delivery.ClassAuth) },
			state:  ports.DeliveryStalled, class: delivery.ClassAuth,
		},
		"a target that is not sent the trail": {
			kind: delivery.KindWebhook, records: 20, notSent: true, state: ports.DeliveryOK,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				trail := porttest.NewMemIAM()
				records := appendTrail(t, trail, tt.records)
				outbox := newFakeOutbox()
				outbox.audited("trail", tt.kind, !tt.notSent)
				outbox.cursors["trail"] = delivery.Cursor{Target: "trail", AuditAcked: tt.acked}
				receiver := &fakeReceiver{answer: tt.answer}
				w := newExportWorld(outbox, trail, receiver)
				stop := w.run(t)
				time.Sleep(30 * time.Minute)
				synctest.Wait()
				st := w.status(t, "trail")
				stop()

				held, got := receiver.held(t)
				if !slices.Equal(held, tt.want) {
					t.Fatalf("the receiver holds %v, want %v", held, tt.want)
				}
				// What it holds is the trail's, record for record, and its
				// chain verifies from the record before the first.
				var before audit.Record
				if tt.acked > 0 {
					before = records[tt.acked-1]
				}
				if err := audit.Verify(before, got); err != nil {
					t.Errorf("the chain the receiver holds: %v", err)
				}
				bodies := map[string]string{}
				for _, m := range receiver.messages() {
					if body, ok := m.Data.(json.RawMessage); ok {
						bodies[m.ID] = string(body)
					}
				}
				for _, record := range got {
					want, _ := records[record.Sequence-1].Line()
					if body := bodies[app.AuditExportID(record.Sequence)]; body != string(want) {
						t.Errorf("record %d was sent as %s, want %s", record.Sequence, body, want)
					}
				}
				if sent := receiver.sent(); !slices.IsSorted(sent) {
					t.Errorf("sent out of sequence: %v", sent)
				}
				if cursor := outbox.cursor("trail"); cursor.AuditAcked != max(tt.wantAck, tt.acked) || !cursor.Acked.IsZero() {
					t.Errorf("the cursor is %+v, want the audit trail acknowledged to %d", cursor, max(tt.wantAck, tt.acked))
				}
				acks := outbox.auditAcks("trail")
				previous := tt.acked
				for _, ack := range acks {
					if ack <= previous || ack-previous > app.DeliveryBatch {
						t.Errorf("acknowledgements %v: more than a batch at once, or not forward", acks)
					}
					previous = ack
				}
				for _, ack := range outbox.acked() {
					if !ack.upto.IsZero() {
						t.Errorf("the export moved the cursor of the outbox: %+v", ack)
					}
				}
				if st.State != tt.state || st.LastErrorClass != tt.class {
					t.Errorf("status = %+v, want %s with class %q", st, tt.state, tt.class)
				}
				for key, n := range tt.attempts {
					if got := w.attempts.count(key); got != n {
						t.Errorf("%d attempts %s, want %d", got, key, n)
					}
				}
				if receiver.undeadlined > 0 || len(outbox.withoutDeadline()) > 0 {
					t.Errorf("calls without a deadline: %d attempts, %v", receiver.undeadlined, outbox.withoutDeadline())
				}
				if logs := w.logs.String(); strings.Contains(logs, trailMark) || strings.Contains(logs, deliveryMark) {
					t.Errorf("the log holds a record or an endpoint:\n%s", logs)
				}
			})
		})
	}
}

// TestAuditExportFollowsTheTarget: a target that comes to be sent the trail
// is sent it from its cursor at the next refresh; one that no longer is is
// sent nothing more, and is sent the rest when it is again; a target that is
// deleted is sent nothing more.
func TestAuditExportFollowsTheTarget(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		trail := porttest.NewMemIAM()
		appendTrail(t, trail, 5)
		outbox := newFakeOutbox()
		outbox.audited("trail", delivery.KindWebhook, false)
		receiver := &fakeReceiver{}
		w := newExportWorld(outbox, trail, receiver)
		stop := w.run(t)
		defer stop()

		time.Sleep(time.Minute)
		synctest.Wait()
		if sent := receiver.sent(); len(sent) != 0 {
			t.Fatalf("a target not sent the trail was sent %v", sent)
		}
		outbox.sendTrail("trail", true)
		time.Sleep(time.Minute)
		synctest.Wait()
		if held, _ := receiver.held(t); !slices.Equal(held, sequences(0, 5)) {
			t.Fatalf("after it was to be sent the trail it holds %v", held)
		}

		outbox.sendTrail("trail", false)
		time.Sleep(time.Minute)
		synctest.Wait()
		appendTrail(t, trail, 3)
		time.Sleep(time.Minute)
		synctest.Wait()
		if held, _ := receiver.held(t); len(held) != 5 {
			t.Fatalf("a target no longer sent the trail was sent %v", held)
		}
		outbox.sendTrail("trail", true)
		time.Sleep(time.Minute)
		synctest.Wait()
		if held, _ := receiver.held(t); !slices.Equal(held, sequences(0, 8)) {
			t.Fatalf("sent the trail again it holds %v", held)
		}

		if err := outbox.DeleteTarget(ctxWithDeadline(t), "trail", audit.Record{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		appendTrail(t, trail, 3)
		time.Sleep(time.Minute)
		synctest.Wait()
		if held, _ := receiver.held(t); len(held) != 8 {
			t.Fatalf("a target that was deleted was sent %v", held)
		}
	})
}

// TestAuditExportATargetMadeAgainUnderItsName: a target deleted and defined
// again under its name before the next refresh is another target, whose
// cursor of the trail begins at 0: the loop of the old one acknowledges
// nothing to it, and the new one is sent the trail from its first record.
func TestAuditExportATargetMadeAgainUnderItsName(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		trail := porttest.NewMemIAM()
		appendTrail(t, trail, 10)
		outbox := newFakeOutbox()
		outbox.audited("trail", delivery.KindWebhook, true)
		taking := &atomic.Bool{}
		receiver := &fakeReceiver{answer: func(sequence uint64, _ int) error {
			if sequence > 5 && !taking.Load() {
				return failing(delivery.ClassServer)
			}
			return nil
		}}
		w := newExportWorld(outbox, trail, receiver)
		stop := w.run(t)
		defer stop()
		// Records 1 to 5 are taken and acknowledged; 6 is retried.
		time.Sleep(2500 * time.Millisecond)
		synctest.Wait()
		if got := outbox.cursor("trail").AuditAcked; got != 5 {
			t.Fatalf("before the target is made again its cursor is at %d, want 5", got)
		}
		// Deleted and defined again, before the refresh at 5 s; from now on
		// the receiver takes everything.
		if err := outbox.DeleteTarget(ctxWithDeadline(t), "trail", audit.Record{}); err != nil {
			t.Fatal(err)
		}
		outbox.audited("trail", delivery.KindWebhook, true)
		outbox.mu.Lock()
		again := outbox.targets["trail"]
		again.CreatedAt = time.Now()
		outbox.targets["trail"] = again
		outbox.mu.Unlock()
		before := len(receiver.messages())
		taking.Store(true)
		time.Sleep(10 * time.Minute)
		synctest.Wait()

		var after []uint64
		for _, m := range receiver.messages()[before:] {
			after = append(after, m.Sequence)
		}
		// The old loop may have sent 6 once more before it found the target
		// was another; the new target is sent everything from 1, in order.
		from := slices.Index(after, 1)
		if from < 0 || !slices.Equal(after[from:], sequences(0, 10)) {
			t.Fatalf("after the target was made again it was sent %v, want 1 to 10 in order", after)
		}
		if got := outbox.cursor("trail").AuditAcked; got != 10 {
			t.Errorf("the cursor of the target made again is at %d, want 10", got)
		}
	})
}

// TestAuditExportARestartResendsAtMostOneBatch: what the next leader sends
// again is what the last one sent and had not acknowledged - at most one
// batch - and a receiver that deduplicates by the ID holds every record
// once, without a gap, and its chain verifies.
func TestAuditExportARestartResendsAtMostOneBatch(t *testing.T) {
	t.Parallel()
	for name, lostAt := range map[string]int{"early": 7, "in the second batch": 150, "at the end": 300} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				trail := porttest.NewMemIAM()
				appendTrail(t, trail, 300)
				outbox := newFakeOutbox()
				outbox.audited("trail", delivery.KindWebhook, true)
				receiver := &fakeReceiver{}
				one := newExportWorld(outbox, trail, receiver)
				receiver.onCall = func(call int) {
					if call == lostAt {
						one.active.Store(false)
					}
				}
				stopOne := one.run(t)
				time.Sleep(10 * time.Minute)
				synctest.Wait()
				stopOne()
				first := len(receiver.sent())
				receiver.onCall = nil

				two := newExportWorld(outbox, trail, receiver)
				stopTwo := two.run(t)
				time.Sleep(10 * time.Minute)
				synctest.Wait()
				stopTwo()

				sent := receiver.sent()
				again := 0
				for _, sequence := range sent[first:] {
					if slices.Contains(sent[:first], sequence) {
						again++
					}
				}
				if again > app.DeliveryBatch {
					t.Errorf("the second leader sent %d records again, want at most %d", again, app.DeliveryBatch)
				}
				if !slices.IsSorted(sent[first:]) {
					t.Error("the second leader sent out of sequence")
				}
				held, records := receiver.held(t)
				if !slices.Equal(held, sequences(0, 300)) {
					t.Errorf("the receiver holds %d records, want all 300 once", len(held))
				}
				if err := audit.Verify(audit.Record{}, records); err != nil {
					t.Errorf("the chain the receiver holds: %v", err)
				}
			})
		})
	}
}

// TestAuditExportStatus: the export of the trail is part of how its target
// stands - what it holds is pending, the oldest since the first, and a
// failure of the export shows though the outbox is delivered.
func TestAuditExportStatus(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		trail := porttest.NewMemIAM()
		appendTrail(t, trail, 3)
		// A trail of old records: what waits has waited since it was read
		// for the target, not since it was written, so a backfill does not
		// look stalled.
		time.Sleep(24 * time.Hour)
		start := time.Now()
		outbox := newFakeOutbox()
		outbox.audited("trail", delivery.KindWebhook, true)
		outbox.append(2, delivery.Consumer{Kind: delivery.ConsumerNotification, Target: "trail"})
		fail := &atomic.Bool{}
		fail.Store(true)
		receiver := &fakeReceiver{answer: func(uint64, int) error {
			if fail.Load() {
				return failing(delivery.ClassConnect)
			}
			return nil
		}}
		w := newExportWorld(outbox, trail, receiver)
		stop := w.run(t)
		defer stop()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		st := w.status(t, "trail")
		if st.State != ports.DeliveryRetrying || st.LastErrorClass != delivery.ClassConnect || st.Pending != 3 ||
			!st.OldestPendingAt.Equal(start) {
			t.Errorf("while the trail fails, status = %+v; want the oldest pending at %v, when it was read", st, start)
		}
		if summary, ok := w.worker.Summary(); !ok || summary.OldestPending[delivery.KindWebhook] > time.Minute {
			t.Errorf("summary = %+v, %v: a backfill shows as waiting since its records were written", summary, ok)
		}
		fail.Store(false)
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		st = w.status(t, "trail")
		if st.State != ports.DeliveryOK || st.Pending != 0 || !st.OldestPendingAt.IsZero() || st.LastSuccessAt.IsZero() {
			t.Errorf("once the trail is taken, status = %+v", st)
		}
	})
}

// failingTrail is an audit trail that cannot be read while fails is set.
type failingTrail struct {
	ports.AuditLog
	fails atomic.Bool
}

func (f *failingTrail) ListAudit(ctx context.Context, after uint64, limit int) ([]audit.Record, error) {
	if f.fails.Load() {
		return nil, fmt.Errorf("%w: the trail of %s", domain.ErrUnavailable, trailMark)
	}
	return f.AuditLog.ListAudit(ctx, after, limit)
}

// TestAuditExportWhenTheMetadataFails: a trail that cannot be read is read
// again, and an acknowledgement that fails holds the export - nothing more
// than one batch is sent meanwhile - and both go on once the metadata
// answers. The log says so, without the words of the record.
func TestAuditExportWhenTheMetadataFails(t *testing.T) {
	t.Parallel()
	for name, acks := range map[string]bool{"the trail cannot be read": false, "acknowledgements fail": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				mem := porttest.NewMemIAM()
				appendTrail(t, mem, 250)
				trail := &failingTrail{AuditLog: mem}
				outbox := newFakeOutbox()
				outbox.audited("trail", delivery.KindWebhook, true)
				if acks {
					outbox.ackFails = errors.New("no quorum")
				} else {
					trail.fails.Store(true)
				}
				receiver := &fakeReceiver{}
				w := newExportWorld(outbox, trail, receiver)
				stop := w.run(t)
				defer stop()
				time.Sleep(10 * time.Minute)
				synctest.Wait()
				if sent := receiver.sent(); len(sent) > app.DeliveryBatch || (!acks && len(sent) > 0) {
					t.Errorf("sent %d records while the metadata failed", len(sent))
				}
				want := "the audit trail could not be read"
				if acks {
					want = "could not be acknowledged"
				}
				if logs := w.logs.String(); !strings.Contains(logs, want) || strings.Contains(logs, "user/alice") {
					t.Errorf("the log does not say %q, or holds a record:\n%s", want, logs)
				}
				outbox.mu.Lock()
				outbox.ackFails = nil
				outbox.mu.Unlock()
				trail.fails.Store(false)
				time.Sleep(10 * time.Minute)
				synctest.Wait()
				if held, _ := receiver.held(t); !slices.Equal(held, sequences(0, 250)) {
					t.Errorf("the receiver holds %d records, want 250", len(held))
				}
				if got := outbox.cursor("trail").AuditAcked; got != 250 {
					t.Errorf("the cursor of the trail is at %d", got)
				}
			})
		})
	}
}

// TestAuditExportNothingIsSentAfterTheLeadIsLost: a node that no longer
// leads sends no record, and acknowledges nothing.
func TestAuditExportNothingIsSentAfterTheLeadIsLost(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		trail := porttest.NewMemIAM()
		appendTrail(t, trail, 50)
		outbox := newFakeOutbox()
		outbox.audited("trail", delivery.KindWebhook, true)
		receiver := &fakeReceiver{}
		w := newExportWorld(outbox, trail, receiver)
		receiver.onCall = func(call int) {
			if call == 10 {
				w.active.Store(false)
			}
		}
		stop := w.run(t)
		defer stop()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if sent := receiver.sent(); len(sent) != 10 {
			t.Errorf("sent %d records, want 10: none after the lead was lost", len(sent))
		}
		if acks := outbox.auditAcks("trail"); len(acks) != 0 {
			t.Errorf("acknowledged %v after the lead was lost", acks)
		}
	})
}

// TestExport: one record to one target, as a message, or a failure of the
// class the publisher gives - and never to a target that is not sent the
// trail.
func TestExport(t *testing.T) {
	t.Parallel()
	record := audit.Record{
		Time: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC), Actor: "root", Action: "admin:PutTarget", Outcome: audit.Succeeded,
	}.Seal(audit.Record{})
	line, err := record.Line()
	if err != nil {
		t.Fatal(err)
	}
	hook := delivery.Target{Name: "trail", Kind: delivery.KindWebhook, Audit: true}
	for name, tt := range map[string]struct {
		target     delivery.Target
		publishers app.PublisherSource
		publish    error
		class      delivery.Class
	}{
		"a webhook":                    {target: hook},
		"a NATS server":                {target: delivery.Target{Name: "bus", Kind: delivery.KindNATS, Audit: true}},
		"a target not sent the trail":  {target: delivery.Target{Name: "hook", Kind: delivery.KindWebhook}, class: delivery.ClassInternal},
		"another deployment":           {target: delivery.Target{Name: "site", Kind: delivery.KindS3, Audit: true}, class: delivery.ClassInternal},
		"no publishers":                {target: hook, publishers: nil, class: delivery.ClassInternal},
		"a publisher that is no class": {target: hook, publish: errors.New("broken"), class: delivery.ClassInternal},
		"a publisher's class":          {target: hook, publish: failing(delivery.ClassTLS), class: delivery.ClassTLS},
		"a deadline":                   {target: hook, publish: fmt.Errorf("post: %w", context.DeadlineExceeded), class: delivery.ClassTimeout},
		"a credential that does not open": {
			target: hook, class: delivery.ClassAuth,
			publishers: app.PublisherSourceFunc(func(context.Context, delivery.Target) (ports.EventPublisher, error) {
				return nil, failing(delivery.ClassAuth)
			}),
		},
		"a source that gives nothing": {
			target: hook, class: delivery.ClassInternal,
			publishers: app.PublisherSourceFunc(func(context.Context, delivery.Target) (ports.EventPublisher, error) { return nil, nil }),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			receiver := &fakeReceiver{answer: func(uint64, int) error { return tt.publish }}
			publishers := tt.publishers
			if publishers == nil && name != "no publishers" {
				publishers = app.PublisherSourceFunc(func(context.Context, delivery.Target) (ports.EventPublisher, error) { return receiver, nil })
			}
			exporter := app.NewAuditExporter(app.AuditExporterConfig{Trail: porttest.NewMemIAM(), Publishers: publishers})
			err := exporter.Export(ctxWithDeadline(t), tt.target, record)
			if got := delivery.ClassOf(err); got != tt.class {
				t.Fatalf("Export = %v, class %q, want %q", err, got, tt.class)
			}
			if err != nil {
				if err.Error() != "delivery failed: "+string(tt.class) {
					t.Errorf("the failure reads %q: more than its class", err.Error())
				}
				if len(receiver.messages()) != 0 {
					t.Error("a failure was taken")
				}
				return
			}
			messages := receiver.messages()
			if len(messages) != 1 {
				t.Fatalf("%d messages", len(messages))
			}
			m := messages[0]
			body, ok := m.Data.(json.RawMessage)
			if m.ID != "audit-1" || m.Type != "cactusfs.audit.exported.v1" || m.Sequence != 1 || !m.Time.Equal(record.Time) ||
				!ok || string(body) != string(line) {
				t.Errorf("message = %+v, want the line %s", m, line)
			}
		})
	}
}
