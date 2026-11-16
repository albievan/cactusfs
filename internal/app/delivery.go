package app

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Deliverer hands one item of the outbox to a target: it tells a webhook or
// a NATS server of a change (ADR-0025), or sends a version to another
// deployment (ADR-0026). One is registered with the DeliveryWorker for each
// kind of consumer.
//
// An error is a failure of the class it carries (delivery.ClassOf; an error
// without one is delivery.ClassInternal). A failure whose class passes
// (delivery.Class.Passes) gives the item up: it is counted and never sent
// again. Any other is tried again, after a pause, and nothing behind the
// item is sent meanwhile. The error may name an endpoint: the worker shows
// and logs its class and nothing else of it.
//
// The status, when not empty, is what became of a version to be replicated,
// reported to the metadata with the acknowledgement: object.ReplicationCompleted
// or object.ReplicationFailed. It is taken from a target of kind s3 only, and
// with an error only when the error's class passes.
//
// ctx carries the deadline of the attempt; a Deliverer sends nothing after
// it has ended.
type Deliverer interface {
	Deliver(ctx context.Context, target delivery.Target, item delivery.Item) (object.ReplicationStatus, error)
}

// DelivererFunc is a function that is a Deliverer.
type DelivererFunc func(ctx context.Context, target delivery.Target, item delivery.Item) (object.ReplicationStatus, error)

// Deliver implements Deliverer.
func (f DelivererFunc) Deliver(ctx context.Context, target delivery.Target, item delivery.Item) (object.ReplicationStatus, error) {
	return f(ctx, target, item)
}

// The pace of delivery (ADR-0023, "One log, one cursor a target").
const (
	// DeliveryBatch is how many items are read from the outbox at once,
	// and how many delivered items an acknowledgement waits for at most.
	DeliveryBatch = 100
	// DeliveryAckInterval is how long a delivered item waits at most to
	// be acknowledged. With DeliveryBatch it bounds what a new leader
	// sends again: one batch, or what one second delivered.
	DeliveryAckInterval = time.Second
	// DefaultDeliveryAttemptTimeout is the deadline of one attempt at
	// one item when the configuration names none.
	DefaultDeliveryAttemptTimeout = time.Minute
	// DeliveryStallAfter is the number of failures in a row after which a
	// target that fails with a class that may pass by itself is shown as
	// stalled: by then the pause between attempts is at its longest.
	DeliveryStallAfter = 10
	// PendingLookReads bounds the reads of one count of what waits beyond
	// the batch in hand: a target further behind than PendingLookReads
	// reads of delivery.MaxOutboxRead items shows that many as pending.
	PendingLookReads = 10
)

// What is said of rows dropped at the bound of the outbox (ADR-0025,
// "Overflow"; PLAN 8.2.7).
const (
	// OutboxReportEvery is the least time between two audit records of one
	// node that say rows of the outbox were dropped at its bound, but for
	// the one that tells, when the node's lead ends, what it had counted
	// and not told yet.
	OutboxReportEvery = time.Minute
	// OutboxDroppedAction is the action of those records.
	OutboxDroppedAction = "system:OutboxDropped"
	// OutboxResource is their resource.
	OutboxResource = "system/outbox"
)

// Intervals of the worker, unexported: nothing outside needs to wait for
// them.
const (
	// deliveryLook is how often Run asks whether this node leads.
	deliveryLook = time.Second
	// deliveryRefresh is how often the targets and the cursors are read
	// again while this node leads.
	deliveryRefresh = 5 * time.Second
	// deliveryIdle is the pause of a target that has nothing to send, and
	// after a read of the outbox that failed.
	deliveryIdle = time.Second
	// deliveryMetaTimeout bounds every call the worker makes to the
	// metadata.
	deliveryMetaTimeout = 30 * time.Second
	// pendingLookEvery is how often what waits beyond the batch in hand is
	// counted again, while a target is behind.
	pendingLookEvery = 10 * time.Second
)

// errNoDeliverer is the cause of a failure of an item of a kind of consumer
// that nothing delivers on this node.
var errNoDeliverer = errors.New("nothing delivers this kind of consumer on this node")

// stalls reports whether a failure of the class needs somebody to act: the
// target refuses the credential, its certificate, its address, or what it is
// sent to, or it is full, and trying again will not change it.
func stalls(class delivery.Class) bool {
	switch class {
	case delivery.ClassAuth, delivery.ClassTLS, delivery.ClassRedirect, delivery.ClassEgressRefused,
		delivery.ClassNoStream, delivery.ClassNoSuchBucket, delivery.ClassVersioning, delivery.ClassNotReplica,
		delivery.ClassQuota:
		return true
	}
	return false
}

// DeliveryWorkerConfig lists what a DeliveryWorker depends on.
type DeliveryWorkerConfig struct {
	Targets ports.TargetStore
	Outbox  ports.Outbox
	// Deliverers hands items to targets, by the kind of consumer. An item
	// of a kind that has none fails with delivery.ClassInternal and holds
	// its target where it is.
	Deliverers map[delivery.ConsumerKind]Deliverer
	// Active reports whether this node leads the metadata group: only that
	// node delivers. Nil is a node that always leads.
	Active func() bool
	Clock  ports.Clock
	Logger *slog.Logger
	// AttemptTimeout is the deadline of one attempt at one item; zero is
	// DefaultDeliveryAttemptTimeout.
	AttemptTimeout time.Duration
	// Entropy, a cryptographically secure source, draws the jitter of the
	// pauses between attempts (delivery.Backoff). Nil draws none.
	Entropy io.Reader
	// OnAttempt, when not nil, is told of every attempt at an item: the
	// kind of the target, and the class of the failure or
	// delivery.ClassNone.
	OnAttempt func(kind delivery.Kind, class delivery.Class)
	// Auditor, when not nil, is given the audit record that says rows of
	// the outbox were dropped at its bound (OutboxResource), at most every
	// OutboxReportEvery, and once more when the node's lead ends. Its Run
	// must outlast the worker's for that last record to be written.
	Auditor *Auditor
}

// DeliveryWorker delivers the rows of the outbox to their targets, on the
// node that leads the metadata group and nowhere else (ADR-0023). For each
// target a loop reads the rows after the target's cursor, in order, hands
// each to the Deliverer of its kind of consumer, and acknowledges through
// Raft every DeliveryBatch items or every DeliveryAckInterval. Delivery is
// therefore at least once, and what a new leader sends again is at most one
// batch.
//
// A failure holds the target at the item that failed, which is tried again
// after delivery.Backoff, unless its class passes: then the item is counted
// and given up, and the target goes on. Nothing of a failure is shown or
// logged but its class: the words of an error may name an endpoint, and the
// path of an endpoint may be a credential.
//
// How each target stands is kept in memory since this node came to lead,
// and is what DeliveryStatus answers.
type DeliveryWorker struct {
	targets        ports.TargetStore
	outbox         ports.Outbox
	deliverers     map[delivery.ConsumerKind]Deliverer
	active         func() bool
	clock          ports.Clock
	logger         *slog.Logger
	attemptTimeout time.Duration
	onAttempt      func(delivery.Kind, delivery.Class)
	auditor        *Auditor

	entropyMu sync.Mutex
	entropy   io.Reader

	// reportMu orders the reports of rows dropped at the bound; reportedAt
	// is when this node last made one, whichever session it was in.
	reportMu   sync.Mutex
	reportedAt time.Time

	mu sync.Mutex
	// session is the delivery of this node's time as leader; nil when it
	// does not lead.
	session *deliverySession
}

// NewDeliveryWorker returns a DeliveryWorker. Run starts it.
func NewDeliveryWorker(cfg DeliveryWorkerConfig) *DeliveryWorker {
	w := &DeliveryWorker{
		targets: cfg.Targets, outbox: cfg.Outbox, deliverers: map[delivery.ConsumerKind]Deliverer{}, active: cfg.Active,
		clock: cfg.Clock, logger: cfg.Logger, attemptTimeout: cfg.AttemptTimeout, onAttempt: cfg.OnAttempt, entropy: cfg.Entropy,
		auditor: cfg.Auditor,
	}
	for kind, deliverer := range cfg.Deliverers {
		if deliverer != nil {
			w.deliverers[kind] = deliverer
		}
	}
	if w.attemptTimeout <= 0 {
		w.attemptTimeout = DefaultDeliveryAttemptTimeout
	}
	if w.logger == nil {
		w.logger = slog.New(slog.DiscardHandler)
	}
	return w
}

func (w *DeliveryWorker) isActive() bool {
	return w.active == nil || w.active()
}

// jitter draws a number from 0 up to 1 for delivery.Backoff.
func (w *DeliveryWorker) jitter() float64 {
	if w.entropy == nil {
		return 0
	}
	var raw [8]byte
	w.entropyMu.Lock()
	_, err := io.ReadFull(w.entropy, raw[:])
	w.entropyMu.Unlock()
	if err != nil {
		return 0
	}
	return float64(binary.BigEndian.Uint64(raw[:])>>11) / (1 << 53)
}

// metaContext bounds a call to the metadata: every request the worker sends
// has a deadline.
func metaContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, deliveryMetaTimeout)
}

// Run delivers while this node leads, until ctx ends. It looks every second
// whether the node leads: delivery begins when it comes to lead and stops
// when it no longer does, and what was kept of the targets is forgotten.
func (w *DeliveryWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(deliveryLook)
	defer ticker.Stop()
	looks := 0
	defer w.end(ctx)
	for {
		active := w.isActive()
		w.mu.Lock()
		s := w.session
		w.mu.Unlock()
		switch {
		case active && s == nil:
			s = w.begin(ctx)
			w.refresh(ctx, s)
			looks = 0
		case !active && s != nil:
			w.end(ctx)
		case active:
			looks++
			if looks%int(deliveryRefresh/deliveryLook) == 0 {
				w.refresh(ctx, s)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// deliverySession is delivery for as long as this node leads: the loops of
// the targets, and the state of the outbox as last read.
type deliverySession struct {
	ctx    context.Context
	cancel context.CancelFunc
	loops  sync.WaitGroup
	// refreshing holds one token: one refresh at a time, so that a slower
	// one does not apply an older list of the targets over a newer one.
	refreshing chan struct{}

	mu sync.Mutex
	// closed says that the session ended: no loop is started any more.
	closed bool
	runs   map[string]*targetRun
	// rows and dropped are the counts of the outbox; outboxKnown says
	// whether they were read.
	rows, dropped uint64
	outboxKnown   bool
	// told is how far the last report of rows dropped at the bound
	// counted, or the first read of the outbox in this session; seen is
	// the last read. toldKnown says whether there was one.
	told, seen outboxTold
	toldKnown  bool
}

// outboxTold is a count of rows dropped at the bound of the outbox: the
// outbox's own, each target's lost, the rows it held, and when it was read.
type outboxTold struct {
	dropped, rows uint64
	lost          map[string]uint64
	at            time.Time
}

// outboxReport is what was dropped at the bound since it was last told.
type outboxReport struct {
	// dropped is the rise of the outbox's count, total the count, rows
	// what the outbox holds.
	dropped, total, rows uint64
	// since is when what came before was told, or when this node's
	// session first read the outbox.
	since time.Time
	// targets is "name=n" for each target whose lost rose, by n: those
	// that lost most first, then by name, within outboxTargetsBytes;
	// more counts those left out.
	targets []string
	more    int
}

// outboxTargetsBytes bounds the detail that names the targets that lost rows,
// within what the auditor keeps of a detail whole (bound).
const outboxTargetsBytes = 512

// start runs the loop of a target for as long as the session lasts, or
// until the target's run is cancelled. A loop belongs to the session, not to
// the call that found its target: it keeps what ctx carries but not its end,
// and ends with the session.
func (s *deliverySession) start(ctx context.Context, run *targetRun, loop func(context.Context)) {
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	run.cancel = cancel
	stop := s.whenEnded(cancel)
	s.loops.Go(func() {
		defer stop()
		defer cancel()
		loop(runCtx)
	})
}

// whenEnded calls cancel once the session ends; the function it returns
// stops that.
func (s *deliverySession) whenEnded(cancel context.CancelFunc) func() bool {
	return context.AfterFunc(s.ctx, cancel)
}

// begin starts a session.
func (w *DeliveryWorker) begin(ctx context.Context) *deliverySession {
	sctx, cancel := context.WithCancel(ctx)
	s := &deliverySession{ctx: sctx, cancel: cancel, runs: map[string]*targetRun{}, refreshing: make(chan struct{}, 1)}
	w.mu.Lock()
	w.session = s
	w.mu.Unlock()
	w.logger.InfoContext(ctx, "this node leads the metadata group and delivers to the targets of the deployment")
	return s
}

// end stops the session, if one runs, and waits for its loops to return.
// Rows dropped at the bound that the session counted and has not told of yet
// are told now, whatever the pace: the next leader begins its counting where
// it first reads, so nobody else would. That is done when the node stops
// leading and when it stops: the caller's end does not end it.
func (w *DeliveryWorker) end(ctx context.Context) {
	w.mu.Lock()
	s := w.session
	w.session = nil
	w.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.loops.Wait()
	if report, ok := w.owed(s); ok {
		w.tell(context.WithoutCancel(ctx), report)
	}
}

// refresh reads the targets and the cursors: a target that is new gets its
// loop, one that is gone loses it, one that changed is delivered to as it
// now is. One refresh runs at a time; another waits for it.
func (w *DeliveryWorker) refresh(ctx context.Context, s *deliverySession) {
	// What the caller gives ends with the session too.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer s.whenEnded(cancel)()
	select {
	case s.refreshing <- struct{}{}:
		defer func() { <-s.refreshing }()
	case <-ctx.Done():
		return
	}
	if ctx.Err() != nil {
		return
	}
	listCtx, cancel := metaContext(ctx)
	targets, err := w.targets.ListTargets(listCtx)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			w.logger.WarnContext(ctx, "the targets could not be read for delivery; read again in a while", slog.Any("error", err))
		}
		return
	}
	stateCtx, cancel := metaContext(ctx)
	state, stateErr := w.outbox.OutboxCursors(stateCtx)
	cancel()
	cursors := map[string]delivery.Cursor{}
	if stateErr == nil {
		for _, cursor := range state.Cursors {
			cursors[cursor.Target] = cursor
		}
		if report, ok := w.overflow(s, state); ok {
			w.tell(ctx, report)
		}
	} else if ctx.Err() == nil {
		w.logger.WarnContext(ctx, "the cursors of the outbox could not be read; read again in a while", slog.Any("error", stateErr))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if stateErr == nil {
		s.rows, s.dropped, s.outboxKnown = state.Rows, state.Dropped, true
	}
	present := map[string]bool{}
	for _, target := range targets {
		present[target.Name] = true
		cursor, known := cursors[target.Name]
		if run, ok := s.runs[target.Name]; ok {
			run.update(target, cursor, known)
			continue
		}
		if !known && stateErr != nil {
			// Where to begin is not known yet: next time.
			continue
		}
		run := newTargetRun(target, cursor)
		s.runs[target.Name] = run
		s.start(ctx, run, func(runCtx context.Context) {
			if w.deliverTo(runCtx, run, cursor.Acked) {
				// The target was deleted, and may be made again under its
				// name: the next refresh starts a loop from its cursor.
				s.forget(target.Name, run)
			}
		})
	}
	for name, run := range s.runs {
		if !present[name] {
			run.cancel()
			delete(s.runs, name)
		}
	}
}

// overflow compares the state of the outbox with what was last told of rows
// dropped at its bound, and returns what is to be told now: rows were
// dropped since, and this node has told nothing for OutboxReportEvery. What
// is not told yet is told later, added up, once the minute has passed - or
// when the session ends (owed): at most once a minute, and once more when a
// lead ends.
//
// The first read of a session is where its counting begins: what was dropped
// before this node came to lead was the node before's to tell, and the total
// is in every record. A count lower than the one told before (a state
// restored from a snapshot older than it) begins the counting again.
func (w *DeliveryWorker) overflow(s *deliverySession, state ports.OutboxState) (outboxReport, bool) {
	w.reportMu.Lock()
	defer w.reportMu.Unlock()
	now := w.clock.Now()
	lost := make(map[string]uint64, len(state.Cursors))
	for _, cursor := range state.Cursors {
		lost[cursor.Target] = cursor.Lost
	}
	read := outboxTold{dropped: state.Dropped, rows: state.Rows, lost: lost, at: now}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return outboxReport{}, false
	}
	s.seen = read
	switch {
	case !s.toldKnown || read.dropped < s.told.dropped:
		s.told, s.toldKnown = read, true
		return outboxReport{}, false
	case read.dropped == s.told.dropped:
		return outboxReport{}, false
	case !w.reportedAt.IsZero() && now.Sub(w.reportedAt) < OutboxReportEvery:
		return outboxReport{}, false
	}
	w.reportedAt = now
	return s.reportLocked(), true
}

// owed returns what a session that ended had counted of rows dropped at the
// bound and not told yet, whatever the pace.
func (w *DeliveryWorker) owed(s *deliverySession) (outboxReport, bool) {
	w.reportMu.Lock()
	defer w.reportMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.toldKnown || s.seen.dropped <= s.told.dropped {
		return outboxReport{}, false
	}
	w.reportedAt = w.clock.Now()
	return s.reportLocked(), true
}

// reportLocked returns what was dropped from what was told to what was last
// read, and makes that what was told; s.mu is held.
func (s *deliverySession) reportLocked() outboxReport {
	told, seen := s.told, s.seen
	report := outboxReport{dropped: seen.dropped - told.dropped, total: seen.dropped, rows: seen.rows, since: told.at}
	type loss struct {
		name string
		n    uint64
	}
	var losses []loss
	for name, n := range seen.lost {
		if before := told.lost[name]; n > before {
			losses = append(losses, loss{name: name, n: n - before})
		}
	}
	// Those that lost most first: a detail that cannot name them all
	// names those.
	slices.SortFunc(losses, func(a, b loss) int {
		return cmp.Or(cmp.Compare(b.n, a.n), strings.Compare(a.name, b.name))
	})
	size := 0
	for i, l := range losses {
		entry := l.name + "=" + strconv.FormatUint(l.n, 10)
		if size+len(entry)+1 > outboxTargetsBytes {
			report.more = len(losses) - i
			break
		}
		size += len(entry) + 1
		report.targets = append(report.targets, entry)
	}
	s.told = seen
	return report
}

// tell says that rows were dropped at the bound of the outbox: in the log of
// the node, and in the audit trail. The record is written in the background,
// as every record of something the node did by itself is: nothing waits for
// it, a write least of all. It names the targets that lost rows and how
// many, never an endpoint.
func (w *DeliveryWorker) tell(ctx context.Context, report outboxReport) {
	targets := strings.Join(report.targets, ",")
	w.logger.WarnContext(ctx, "the outbox is at its bound: its oldest rows were dropped, lost to the targets that had not passed them",
		slog.Uint64("dropped", report.dropped), slog.Uint64("dropped_total", report.total), slog.Uint64("rows", report.rows),
		slog.String("targets", targets), slog.Int("targets_more", report.more))
	if w.auditor == nil {
		return
	}
	details := map[string]string{
		"dropped":       strconv.FormatUint(report.dropped, 10),
		"dropped_total": strconv.FormatUint(report.total, 10),
		"rows":          strconv.FormatUint(report.rows, 10),
		"since":         report.since.UTC().Format(time.RFC3339),
	}
	if targets != "" {
		details["targets"] = targets
	}
	if report.more > 0 {
		details["targets_more"] = strconv.Itoa(report.more)
	}
	record := w.auditor.System(OutboxDroppedAction, OutboxResource, details)
	// Rows the targets were owed were not delivered: a failure, not a
	// change somebody made.
	record.Outcome = audit.Failed
	w.auditor.Succeeded(ctx, record)
}

// forget removes the run of a target whose loop found it gone, unless the
// run of that name is another by now.
func (s *deliverySession) forget(name string, run *targetRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs[name] == run {
		delete(s.runs, name)
	}
}

// targetRun is one target, as its loop delivers to it and its status says.
type targetRun struct {
	cancel context.CancelFunc

	mu     sync.Mutex
	target delivery.Target
	// failures counts the failures in a row; lastClass is the class of
	// the last attempt's failure, or none.
	failures    int
	lastClass   delivery.Class
	lastSuccess time.Time
	rejected    uint64
	failed      uint64
	lost        uint64
	resync      bool
	// hand are the items read and not yet acknowledged, in order: their
	// places and when they were made.
	hand []handItem
	// ahead counts the items for the target beyond the batch in hand, up
	// to aheadUpto, as the last look found them; aheadAt is when the first
	// of them was made.
	ahead     uint64
	aheadUpto delivery.Seq
	aheadAt   time.Time
}

type handItem struct {
	seq delivery.Seq
	at  time.Time
}

func newTargetRun(target delivery.Target, cursor delivery.Cursor) *targetRun {
	return &targetRun{target: target.Clone(), lost: cursor.Lost, resync: cursor.ResyncNeeded}
}

func (r *targetRun) update(target delivery.Target, cursor delivery.Cursor, known bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.target = target.Clone()
	if known {
		r.lost, r.resync = cursor.Lost, cursor.ResyncNeeded
	}
}

func (r *targetRun) current() delivery.Target {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.target
}

// took records a batch that was read.
func (r *targetRun) took(items []delivery.Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range items {
		r.hand = append(r.hand, handItem{seq: item.Seq, at: item.At})
		if r.ahead > 0 && !item.Seq.After(r.aheadUpto) {
			r.ahead--
		}
	}
	if r.ahead == 0 {
		r.aheadAt = time.Time{}
	}
}

// acked records an acknowledgement up to upto.
func (r *targetRun) acked(upto delivery.Seq) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for n < len(r.hand) && !r.hand[n].seq.After(upto) {
		n++
	}
	r.hand = slices.Delete(r.hand, 0, n)
}

// caughtUp records that nothing waits beyond the batch in hand.
func (r *targetRun) caughtUp() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ahead, r.aheadAt = 0, time.Time{}
}

// looked records a count of what waits beyond the batch in hand.
func (r *targetRun) looked(count uint64, upto delivery.Seq, first time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ahead, r.aheadUpto, r.aheadAt = count, upto, first
}

// succeeded records an attempt that the target took, or that gave the item
// up with a class that passes.
func (r *targetRun) succeeded(class delivery.Class, at time.Time, status object.ReplicationStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures, r.lastClass = 0, class
	if class == delivery.ClassNone {
		r.lastSuccess = at
	}
	if class == delivery.ClassRejected {
		r.rejected++
	}
	if status == object.ReplicationFailed {
		r.failed++
	}
}

// failedWith records an attempt that failed and is tried again; it returns
// the number of failures in a row.
func (r *targetRun) failedWith(class delivery.Class) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures++
	r.lastClass = class
	return r.failures
}

// stateLocked returns the word of the target's state; r.mu is held.
func (r *targetRun) stateLocked() string {
	switch {
	case r.failures == 0:
		return ports.DeliveryOK
	case stalls(r.lastClass) || r.failures >= DeliveryStallAfter:
		return ports.DeliveryStalled
	}
	return ports.DeliveryRetrying
}

// status returns how the target stands.
func (r *targetRun) status() ports.TargetStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := ports.TargetStatus{
		Name: r.target.Name, Kind: r.target.Kind, State: r.stateLocked(), Pending: uint64(len(r.hand)) + r.ahead,
		LastSuccessAt: r.lastSuccess, LastErrorClass: r.lastClass, Lost: r.lost, Rejected: r.rejected, Failed: r.failed,
		ResyncNeeded: r.resync,
	}
	switch {
	case len(r.hand) > 0:
		st.OldestPendingAt = r.hand[0].at
	case r.ahead > 0:
		st.OldestPendingAt = r.aheadAt
	}
	return st
}

// targetLoop is the delivery to one target: where it has read to, what it
// has delivered and not acknowledged yet.
type targetLoop struct {
	w   *DeliveryWorker
	run *targetRun
	// name is the target's; it does not change.
	name string
	// read is where the next read begins; acked is where the cursor is,
	// as far as this loop knows; position is how far delivery has come.
	read, acked, position delivery.Seq
	// unacked counts the items delivered since the last acknowledgement;
	// results are what became of versions among them.
	unacked int
	results []delivery.Result
	// ackedAt is when the last acknowledgement was made, or the loop
	// began; lookedAt when what waits was last counted.
	ackedAt  time.Time
	lookedAt time.Time
	// gone says that the target is no more.
	gone bool
}

// deliverTo is the loop of one target, from the place its cursor is at. It
// returns when ctx ends, and when the target is no more: then it reports
// true.
//
// While this node does not lead, the loop sends nothing and looks again
// every deliveryLook: a lead lost for a moment, between two of Run's looks,
// leaves the loop in place; one lost for real ends the session, and ctx.
func (w *DeliveryWorker) deliverTo(ctx context.Context, run *targetRun, from delivery.Seq) bool {
	l := &targetLoop{w: w, run: run, name: run.current().Name, read: from, acked: from, position: from, ackedAt: w.clock.Now()}
	defer l.finish(ctx)
	for ctx.Err() == nil && !l.gone {
		if !w.isActive() {
			if !pause(ctx, deliveryLook) {
				break
			}
			continue
		}
		readCtx, cancel := metaContext(ctx)
		items, upto, err := w.outbox.ReadOutbox(readCtx, l.name, l.read, DeliveryBatch)
		cancel()
		switch {
		case errors.Is(err, domain.ErrEntityNotFound):
			// The target was deleted: its cursor went with it.
			l.gone = true
			continue
		case err != nil:
			if ctx.Err() == nil {
				w.logger.WarnContext(ctx, "the outbox could not be read for a target; read again in a while",
					slog.String("target", l.name), slog.Any("error", err))
			}
			pause(ctx, deliveryIdle)
			continue
		}
		if len(items) == 0 {
			if upto.After(l.read) {
				// Rows of other targets were passed over: they are behind
				// this one too.
				l.read, l.position = upto, upto
				l.ackIfDue(ctx)
				continue
			}
			if l.settle(ctx) {
				run.caughtUp()
				pause(ctx, deliveryIdle)
			}
			continue
		}
		run.took(items)
		if len(items) == DeliveryBatch {
			l.countAhead(ctx, upto)
		}
		if !l.deliverAll(ctx, items) {
			continue
		}
		l.read, l.position = upto, upto
		if l.ackIfDue(ctx) && len(items) < DeliveryBatch {
			run.caughtUp()
		}
	}
	return l.gone
}

// deliverAll delivers a batch in order, acknowledging as it goes; it reports
// false when the loop is to stop before the end of the batch.
func (l *targetLoop) deliverAll(ctx context.Context, items []delivery.Item) bool {
	for _, item := range items {
		if !l.deliver(ctx, item) {
			return false
		}
		l.position = item.Seq
		l.unacked++
		if !l.ackIfDue(ctx) {
			return false
		}
	}
	return true
}

// deliver hands one item to its target until the target takes it or its
// failure passes. It reports false when the loop is to stop: ctx ended, or
// the target is gone. While the node does not lead, nothing is sent: the
// item waits, and the lead is looked at again every deliveryLook.
func (l *targetLoop) deliver(ctx context.Context, item delivery.Item) bool {
	kind, _ := item.For(l.name)
	deliverer := l.w.deliverers[kind]
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
		status, err := l.attempt(ctx, deliverer, target, item)
		if ctx.Err() != nil {
			// The loop was stopped while the item was out: not a failure
			// of the target. It is sent again by whoever delivers next.
			return false
		}
		class := delivery.ClassOf(err)
		if l.w.onAttempt != nil {
			l.w.onAttempt(target.Kind, class)
		}
		if err == nil || class.Passes() {
			if target.Kind != delivery.KindS3 {
				status = ""
			}
			if status != "" {
				l.results = append(l.results, delivery.Result{Seq: item.Seq, Status: status})
			}
			l.run.succeeded(class, l.w.clock.Now(), status)
			if err != nil {
				l.w.logger.InfoContext(ctx, "a target refused one item, which is passed and not sent again",
					slog.String("target", target.Name), slog.String("kind", string(target.Kind)), slog.String("class", string(class)))
			}
			return true
		}
		failures := l.run.failedWith(class)
		wait := delivery.Backoff(failures, l.w.jitter())
		l.w.logger.WarnContext(ctx, "delivery to a target failed; the item is tried again",
			slog.String("target", target.Name), slog.String("kind", string(target.Kind)), slog.String("class", string(class)),
			slog.Int("failures", failures), slog.Duration("pause", wait))
		// What was delivered before is acknowledged while the target
		// waits: a target that stalls holds back nothing it took. An
		// acknowledgement that fails is tried again with the next attempt.
		if l.w.isActive() {
			l.ack(ctx)
		}
		if l.gone || !pause(ctx, wait) {
			return false
		}
	}
}

// attempt is one attempt at one item, under its deadline.
func (l *targetLoop) attempt(ctx context.Context, deliverer Deliverer, target delivery.Target, item delivery.Item) (object.ReplicationStatus, error) {
	if deliverer == nil {
		return "", delivery.Failed(delivery.ClassInternal, errNoDeliverer)
	}
	ctx, cancel := context.WithTimeout(ctx, l.w.attemptTimeout)
	defer cancel()
	return deliverer.Deliver(ctx, target, item.Clone())
}

// ackIfDue acknowledges when a batch was delivered, or a delivered item has
// waited for DeliveryAckInterval, through settle. It reports false when the
// loop is to stop.
func (l *targetLoop) ackIfDue(ctx context.Context) bool {
	if l.unacked >= DeliveryBatch || (l.position.After(l.acked) && l.w.clock.Now().Sub(l.ackedAt) >= DeliveryAckInterval) {
		return l.settle(ctx)
	}
	return true
}

// settle acknowledges, and holds the loop while that fails as a failed
// attempt holds it: the acknowledgement is tried again after
// delivery.Backoff, and nothing more is delivered meanwhile. So what a new
// leader sends again stays within one batch, and the results of one
// acknowledgement within DeliveryBatch. While the node does not lead it
// waits, sending nothing, and looks again every deliveryLook. It reports
// false when the loop is to stop: ctx ended, or the target is gone.
func (l *targetLoop) settle(ctx context.Context) bool {
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

// ack acknowledges everything up to the position, through Raft, and reports
// whether it did, or had nothing to acknowledge.
func (l *targetLoop) ack(ctx context.Context) bool {
	if !l.position.After(l.acked) && len(l.results) == 0 {
		return true
	}
	ackCtx, cancel := metaContext(ctx)
	unrelated, err := l.w.outbox.AckOutbox(ackCtx, l.name, l.position, 0, l.results)
	cancel()
	switch {
	case errors.Is(err, domain.ErrEntityNotFound):
		l.gone = true
		return false
	case err != nil:
		if ctx.Err() == nil {
			l.w.logger.WarnContext(ctx, "delivered items could not be acknowledged; nothing more is delivered to the target until they are",
				slog.String("target", l.name), slog.Any("error", err))
		}
		return false
	}
	if unrelated > 0 {
		l.w.logger.WarnContext(ctx, "an acknowledgement carried results that were not the target's to give",
			slog.String("target", l.name), slog.Int("unrelated", unrelated))
	}
	l.acked, l.unacked, l.results, l.ackedAt = l.position, 0, nil, l.w.clock.Now()
	l.run.acked(l.position)
	return true
}

// countAhead counts, at most every pendingLookEvery, what waits for the
// target after from, the end of the batch in hand: at most PendingLookReads
// reads.
func (l *targetLoop) countAhead(ctx context.Context, from delivery.Seq) {
	now := l.w.clock.Now()
	if !l.lookedAt.IsZero() && now.Sub(l.lookedAt) < pendingLookEvery {
		return
	}
	l.lookedAt = now
	var (
		count uint64
		first time.Time
	)
	for range PendingLookReads {
		readCtx, cancel := metaContext(ctx)
		items, upto, err := l.w.outbox.ReadOutbox(readCtx, l.name, from, delivery.MaxOutboxRead)
		cancel()
		if err != nil {
			return
		}
		if len(items) > 0 && first.IsZero() {
			first = items[0].At
		}
		count += uint64(len(items))
		if len(items) == 0 && !upto.After(from) {
			break
		}
		from = upto
	}
	l.run.looked(count, from, first)
}

// finish acknowledges what was delivered when the loop ends while this node
// still leads - it is stopping - so that the next leader sends it not again.
func (l *targetLoop) finish(ctx context.Context) {
	if l.gone || !l.w.isActive() {
		return
	}
	l.ack(context.WithoutCancel(ctx))
}

// pause waits for d or until ctx ends, and reports whether d passed.
func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// DeliveryStatus implements ports.DeliveryStatusReader on the node that
// leads: how delivery to the targets named stands, in name order - to every
// target when none is named. A name that is not known is looked for once
// among the targets; one that is no target's is left out. On a node that does
// not lead the error wraps domain.ErrNotLeader.
func (w *DeliveryWorker) DeliveryStatus(ctx context.Context, names []string) ([]ports.TargetStatus, error) {
	w.mu.Lock()
	s := w.session
	w.mu.Unlock()
	if s == nil || !w.isActive() {
		return nil, fmt.Errorf("delivery.Status: %w: this node does not deliver", domain.ErrNotLeader)
	}
	if out, missing := s.statuses(names); !missing {
		return out, nil
	}
	// A target defined a moment ago is looked for before it is said to be
	// none.
	w.refresh(ctx, s)
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("delivery.Status: %w", err)
	}
	out, _ := s.statuses(names)
	return out, nil
}

// statuses returns the status of the targets named, and whether one of them
// was not found.
func (s *deliverySession) statuses(names []string) (out []ports.TargetStatus, missing bool) {
	s.mu.Lock()
	runs := make([]*targetRun, 0, len(s.runs))
	if len(names) == 0 {
		for _, run := range s.runs {
			runs = append(runs, run)
		}
	}
	for _, name := range names {
		run, ok := s.runs[name]
		if !ok {
			missing = true
			continue
		}
		if !slices.Contains(runs, run) {
			runs = append(runs, run)
		}
	}
	s.mu.Unlock()
	out = make([]ports.TargetStatus, 0, len(runs))
	for _, run := range runs {
		out = append(out, run.status())
	}
	slices.SortFunc(out, func(a, b ports.TargetStatus) int { return strings.Compare(a.Name, b.Name) })
	return out, missing
}

// DeliverySummary is how delivery stands, as the metrics show it: counts,
// with no target named.
type DeliverySummary struct {
	// Targets counts the targets by kind and state.
	Targets map[delivery.Kind]map[string]int
	// OldestPending is, by kind of target, how long the oldest item that
	// waits has waited; a kind with targets and nothing waiting has 0.
	OldestPending map[delivery.Kind]time.Duration
	// Rows and Dropped are the counts of the outbox (ports.OutboxState).
	Rows, Dropped uint64
}

// Summary returns how delivery stands, and false on a node that does not
// deliver or before the outbox was read.
func (w *DeliveryWorker) Summary() (DeliverySummary, bool) {
	w.mu.Lock()
	s := w.session
	w.mu.Unlock()
	if s == nil || !w.isActive() {
		return DeliverySummary{}, false
	}
	s.mu.Lock()
	known, rows, dropped := s.outboxKnown, s.rows, s.dropped
	s.mu.Unlock()
	if !known {
		return DeliverySummary{}, false
	}
	out := DeliverySummary{Targets: map[delivery.Kind]map[string]int{}, OldestPending: map[delivery.Kind]time.Duration{}, Rows: rows, Dropped: dropped}
	statuses, _ := s.statuses(nil)
	now := w.clock.Now()
	for _, st := range statuses {
		if out.Targets[st.Kind] == nil {
			out.Targets[st.Kind] = map[string]int{}
		}
		out.Targets[st.Kind][st.State]++
		waited := time.Duration(0)
		if !st.OldestPendingAt.IsZero() {
			waited = max(now.Sub(st.OldestPendingAt), 0)
		}
		out.OldestPending[st.Kind] = max(out.OldestPending[st.Kind], waited)
	}
	return out, true
}
