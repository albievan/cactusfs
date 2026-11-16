package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/ports"
)

// AuditExportIDPrefix begins the ID of every exported record: the ID is
// "audit-<sequence>", the same every time the record is sent, so that a
// receiver deduplicates by it (ADR-0025).
const AuditExportIDPrefix = "audit-"

// AuditExportID returns the ID a record of the audit trail is sent with.
func AuditExportID(sequence uint64) string {
	return AuditExportIDPrefix + strconv.FormatUint(sequence, 10)
}

// AuditExporterConfig lists what an AuditExporter depends on.
type AuditExporterConfig struct {
	// Trail is the audit trail the records are read from.
	Trail ports.AuditLog
	// Publishers gives the publisher of each target: the same source the
	// Notifier publishes through.
	Publishers PublisherSource
}

// AuditExporter sends the audit trail to the targets that are to be sent it
// (ADR-0025, "The audit trail through a target"): every record, from the
// one after the target's cursor (delivery.Cursor.AuditAcked) on, as the
// message ports.MessageAuditExported, whose body is the record's line
// (audit.Record.Line) - the bytes the node's own exporter writes - and
// whose ID is AuditExportID.
//
// The DeliveryWorker runs the export, beside the delivery of the outbox, on
// the node that leads and nowhere else; the AuditExporter reads the trail
// and sends one record. The worker sends the records of a target in
// sequence and moves its cursor through Raft every DeliveryBatch records or
// every DeliveryAckInterval, so the export is at least once and what a new
// leader sends again is at most one batch.
//
// No record is ever passed: a failure of any class - a receiver's 4xx
// included, which a notification passes - holds the export at the record
// and is tried again after delivery.Backoff, so that a receiver never holds
// a trail with a gap in it that it was not told of. Its target is shown
// retrying, then stalled.
type AuditExporter struct {
	trail      ports.AuditLog
	publishers PublisherSource
}

// NewAuditExporter returns an AuditExporter.
func NewAuditExporter(cfg AuditExporterConfig) *AuditExporter {
	return &AuditExporter{trail: cfg.Trail, publishers: cfg.Publishers}
}

// errNotAudited is the cause of an export to a target that is not to be sent
// the audit trail.
var errNotAudited = errors.New("the target is not sent the audit trail")

// ExportsAudit reports whether a target is to be sent the audit trail: it was
// defined so, and is a webhook or a NATS server.
func ExportsAudit(target delivery.Target) bool {
	return target.Audit && target.Kind.Serves(delivery.ConsumerNotification)
}

// Export sends one record of the audit trail to target. An error is a
// failure of the class it carries (delivery.ClassOf): the publisher's, or
// delivery.ClassInternal when it carries none, delivery.ClassTimeout when
// the attempt's deadline ended it. Its text is the class and nothing else:
// the words of the cause may name an endpoint.
func (a *AuditExporter) Export(ctx context.Context, target delivery.Target, record audit.Record) error {
	if !ExportsAudit(target) {
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("auditexport.Export: %w: %w", domain.ErrInvalidArgument, errNotAudited))
	}
	body, err := record.Line()
	if err != nil {
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("auditexport.Export: %w", err))
	}
	if a.publishers == nil {
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("auditexport.Export: %w", errNoPublishers))
	}
	publisher, err := a.publishers.Publisher(ctx, target)
	switch {
	case err != nil:
		return exportClassed(err)
	case publisher == nil:
		return delivery.Failed(delivery.ClassInternal, fmt.Errorf("auditexport.Export: %w", errNoPublishers))
	}
	event := ports.Event{
		ID: AuditExportID(record.Sequence), Type: ports.MessageAuditExported, Sequence: record.Sequence, Time: record.Time,
		Data: json.RawMessage(body),
	}
	if err := publisher.Publish(ctx, event); err != nil {
		return exportClassed(err)
	}
	return nil
}

// exportClassed returns a failure of a publisher as a delivery.Error of its
// class, as classed does for a notification.
func exportClassed(err error) error {
	class := delivery.ClassOf(err)
	if class == delivery.ClassInternal && errors.Is(err, context.DeadlineExceeded) {
		class = delivery.ClassTimeout
	}
	return delivery.Failed(class, fmt.Errorf("auditexport.Export: %w", err))
}

// auditExport is the export of the audit trail to one target, as its status
// shows it; the fields are guarded by the mutex of the target's run.
type auditExport struct {
	cancel context.CancelFunc
	// failures counts the failures in a row; lastClass is the class of the
	// last one, or none.
	failures    int
	lastClass   delivery.Class
	lastSuccess time.Time
	// hand are the records read and not yet acknowledged: their sequences
	// and times.
	hand []exportedRecord
}

type exportedRecord struct {
	sequence uint64
	at       time.Time
}

// exportAudit starts the export of the audit trail to a target that is to
// be sent it and has none running, from the cursor read, and stops the one
// of a target that is no longer to be sent it; the log says either. It is
// called by refresh with the session's lock held, after the run's loop was
// started. The export keeps what ctx carries but not its end: it belongs
// to the run, as the run's loop does, and ends with it (and so with the
// session), or when it is stopped here.
func (w *DeliveryWorker) exportAudit(ctx context.Context, s *deliverySession, run *targetRun, target delivery.Target, cursor delivery.Cursor, known bool) {
	if w.auditExport == nil || run.ctx == nil {
		return
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	switch running := run.export != nil; {
	case ExportsAudit(target) && !running && known:
		exportCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		stop := run.whenEnded(cancel)
		export := &auditExport{cancel: cancel}
		run.export = export
		w.logger.InfoContext(ctx, "the audit trail is sent to a target",
			slog.String("target", target.Name), slog.Uint64("after", cursor.AuditAcked))
		s.loops.Go(func() {
			defer stop()
			defer cancel()
			w.exportTo(exportCtx, run, export, cursor.AuditAcked)
			run.exportEnded(export)
		})
	case !ExportsAudit(target) && running:
		run.export.cancel()
		run.export = nil
		w.logger.InfoContext(ctx, "the audit trail is no longer sent to a target", slog.String("target", target.Name))
	}
}

// whenEnded calls cancel once the run ends - it is cancelled, or its
// session ends; the function it returns stops that.
func (r *targetRun) whenEnded(cancel context.CancelFunc) func() bool {
	return context.AfterFunc(r.ctx, cancel)
}

// exportEnded forgets the export that ended, unless another runs by now, so
// that the next refresh starts one when the target is to be sent the trail.
func (r *targetRun) exportEnded(export *auditExport) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.export == export {
		r.export = nil
	}
}

// exportStatusLocked adds the export of the audit trail to the status of
// its target; r.mu is held. What waits is added up; the oldest of either
// is the oldest; the state is the worse of the two, and the class shown is
// that of the worse.
func (r *targetRun) exportStatusLocked(st *ports.TargetStatus) {
	e := r.export
	if e == nil {
		return
	}
	st.Pending += uint64(len(e.hand))
	if len(e.hand) > 0 && (st.OldestPendingAt.IsZero() || e.hand[0].at.Before(st.OldestPendingAt)) {
		st.OldestPendingAt = e.hand[0].at
	}
	if e.lastSuccess.After(st.LastSuccessAt) {
		st.LastSuccessAt = e.lastSuccess
	}
	if state := deliveryState(e.failures, e.lastClass); stateRank(state) > stateRank(st.State) {
		st.State, st.LastErrorClass = state, e.lastClass
	}
}

// stateRank orders the words of a state from the best.
func stateRank(state string) int {
	switch state {
	case ports.DeliveryStalled:
		return 2
	case ports.DeliveryRetrying:
		return 1
	}
	return 0
}

// exportTook records records that were read, at the time they were read:
// what waits for a target is how long it has waited to be sent, not how old
// the record is - a target that is sent a trail of years from its start is
// not behind by years.
func (r *targetRun) exportTook(e *auditExport, records []audit.Record, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range records {
		e.hand = append(e.hand, exportedRecord{sequence: record.Sequence, at: at})
	}
}

// exportAcked records an acknowledgement up to the sequence upto.
func (r *targetRun) exportAcked(e *auditExport, upto uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for n < len(e.hand) && e.hand[n].sequence <= upto {
		n++
	}
	e.hand = e.hand[n:]
}

// exportSucceeded records a record the target took.
func (r *targetRun) exportSucceeded(e *auditExport, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.failures, e.lastClass, e.lastSuccess = 0, delivery.ClassNone, at
}

// exportFailed records an attempt that failed; it returns the number of
// failures in a row.
func (r *targetRun) exportFailed(e *auditExport, class delivery.Class) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.failures++
	e.lastClass = class
	return e.failures
}

// auditLoop is the export of the audit trail to one target: where it has
// read to, what it has sent and not acknowledged yet.
type auditLoop struct {
	w      *DeliveryWorker
	run    *targetRun
	export *auditExport
	name   string
	// createdAt is when the target the loop sends to was defined: a target
	// deleted and defined again under its name is another, whose cursor
	// begins anew, and is not this loop's.
	createdAt time.Time
	// read is the sequence the next read begins after; acked is where the
	// cursor is, as far as this loop knows; position is the last record
	// the target took.
	read, acked, position uint64
	// unacked counts the records sent since the last acknowledgement.
	unacked int
	ackedAt time.Time
	// gone says that the target is no more, or is another under its name.
	gone bool
}

// replaced reports whether the target under the loop's name is no longer
// the one the loop began with, as the run last read it; the loop is then
// gone.
func (l *auditLoop) replaced() bool {
	if !l.run.current().CreatedAt.Equal(l.createdAt) {
		l.gone = true
	}
	return l.gone
}

// exportTo sends the audit trail to a target from the record after from, in
// sequence, until ctx ends, the target is gone or is no longer to be sent
// it. While this node does not lead nothing is sent, and the lead is looked
// at again every deliveryLook.
func (w *DeliveryWorker) exportTo(ctx context.Context, run *targetRun, export *auditExport, from uint64) {
	target := run.current()
	l := &auditLoop{
		w: w, run: run, export: export, name: target.Name, createdAt: target.CreatedAt, read: from, acked: from, position: from,
		ackedAt: w.clock.Now(),
	}
	defer l.finish(ctx)
	for ctx.Err() == nil && !l.replaced() {
		if !w.isActive() {
			if !pause(ctx, deliveryLook) {
				return
			}
			continue
		}
		readCtx, cancel := metaContext(ctx)
		records, err := w.auditExport.trail.ListAudit(readCtx, l.read, DeliveryBatch)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				w.logger.WarnContext(ctx, "the audit trail could not be read for a target; read again in a while",
					slog.String("target", l.name), slog.Any("error", err))
			}
			pause(ctx, deliveryIdle)
			continue
		}
		if len(records) == 0 {
			if l.settle(ctx) {
				pause(ctx, deliveryIdle)
			}
			continue
		}
		run.exportTook(export, records, w.clock.Now())
		if !l.sendAll(ctx, records) {
			return
		}
		l.read = l.position
		l.ackIfDue(ctx)
	}
}

// sendAll sends a batch of records in sequence, acknowledging as it goes; it
// reports false when the loop is to stop before the end of the batch.
func (l *auditLoop) sendAll(ctx context.Context, records []audit.Record) bool {
	for _, record := range records {
		if !l.send(ctx, record) {
			return false
		}
		l.position = record.Sequence
		l.unacked++
		if !l.ackIfDue(ctx) {
			return false
		}
	}
	return true
}

// send sends one record until the target takes it. Nothing passes: every
// failure is tried again after delivery.Backoff. It reports false when the
// loop is to stop: ctx ended, the target is gone, or it is no longer to be
// sent the trail.
func (l *auditLoop) send(ctx context.Context, record audit.Record) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		if !l.w.isActive() {
			if !pause(ctx, deliveryLook) {
				return false
			}
			continue
		}
		target := l.run.current()
		if !ExportsAudit(target) || l.replaced() {
			return false
		}
		attemptCtx, cancel := context.WithTimeout(ctx, l.w.attemptTimeout)
		err := l.w.auditExport.Export(attemptCtx, target, record)
		cancel()
		if ctx.Err() != nil {
			// Stopped while the record was out: sent again by whoever
			// exports next.
			return false
		}
		class := delivery.ClassOf(err)
		if l.w.onAttempt != nil {
			l.w.onAttempt(target.Kind, class)
		}
		if err == nil {
			l.run.exportSucceeded(l.export, l.w.clock.Now())
			return true
		}
		failures := l.run.exportFailed(l.export, class)
		wait := delivery.Backoff(failures, l.w.jitter())
		l.w.logger.WarnContext(ctx, "the export of the audit trail to a target failed; the record is tried again",
			slog.String("target", target.Name), slog.String("kind", string(target.Kind)), slog.String("class", string(class)),
			slog.Uint64("sequence", record.Sequence), slog.Int("failures", failures), slog.Duration("pause", wait))
		if l.w.isActive() {
			l.ack(ctx)
		}
		if l.gone || !pause(ctx, wait) {
			return false
		}
	}
}

// ackIfDue acknowledges when a batch was sent, or a record sent has waited
// for DeliveryAckInterval, through settle. It reports false when the loop is
// to stop.
func (l *auditLoop) ackIfDue(ctx context.Context) bool {
	if l.unacked >= DeliveryBatch || (l.position > l.acked && l.w.clock.Now().Sub(l.ackedAt) >= DeliveryAckInterval) {
		return l.settle(ctx)
	}
	return true
}

// settle acknowledges, and holds the loop while that fails, as the delivery
// of the outbox does (targetLoop.settle): nothing more is sent until the
// acknowledgement is taken, so what a new leader sends again stays within
// one batch. It reports false when the loop is to stop.
func (l *auditLoop) settle(ctx context.Context) bool {
	failures := 0
	for ctx.Err() == nil {
		wait := deliveryLook
		if l.w.isActive() {
			if l.ack(ctx) {
				return true
			}
			if l.gone {
				return false
			}
			failures++
			wait = delivery.Backoff(failures, l.w.jitter())
		}
		if !pause(ctx, wait) {
			return false
		}
	}
	return false
}

// ack moves the target's cursor of the audit trail to the last record it
// took, through Raft, and reports whether it did or had nothing to move.
// The cursor of the outbox is left where it is: an upto of the zero place is
// behind every cursor.
//
// The target is read first: one deleted and defined again under the name
// since the loop began has a cursor of its own, from the first record, and
// an acknowledgement of this loop would move it past records it was never
// sent (the outbox's cursors are safe from that by their places; the trail's
// is a count). What is left is the moment between the read and the commit.
func (l *auditLoop) ack(ctx context.Context) bool {
	if l.position <= l.acked {
		return true
	}
	readCtx, cancel := metaContext(ctx)
	target, err := l.w.targets.GetTarget(readCtx, l.name)
	cancel()
	if err == nil && !target.CreatedAt.Equal(l.createdAt) {
		err = fmt.Errorf("%w: target %s was defined again", domain.ErrEntityNotFound, l.name)
	}
	if err == nil {
		ackCtx, cancel := metaContext(ctx)
		_, err = l.w.outbox.AckOutbox(ackCtx, l.name, delivery.Seq{}, l.position, nil)
		cancel()
	}
	switch {
	case errors.Is(err, domain.ErrEntityNotFound):
		l.gone = true
		return false
	case err != nil:
		if ctx.Err() == nil {
			l.w.logger.WarnContext(ctx, "exported records of the audit trail could not be acknowledged; nothing more is sent to the target until they are",
				slog.String("target", l.name), slog.Any("error", err))
		}
		return false
	}
	l.acked, l.unacked, l.ackedAt = l.position, 0, l.w.clock.Now()
	l.run.exportAcked(l.export, l.position)
	return true
}

// finish acknowledges what was sent when the loop ends while this node still
// leads, so that the next leader sends it not again.
func (l *auditLoop) finish(ctx context.Context) {
	if l.gone || !l.w.isActive() {
		return
	}
	l.ack(context.WithoutCancel(ctx))
}
