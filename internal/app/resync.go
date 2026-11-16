package app

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// The resync of a bucket (ADR-0026, "Resync"; PLAN 8.1.6): a walk of every
// version of the bucket, in the order of the version index, that asks the
// other deployment for each and sends it what it lacks - the versions that
// were there before the replication configuration, and those whose rows were
// dropped at the bound of the outbox. ReplicationService.Resync stores where
// it begins; the ResyncWorker walks it, on the node that leads, and stores
// how far it has come after every pass, so that the next leader goes on from
// there.

// Bounds of a resync.
const (
	// ResyncPass is how many versions one pass of a resync looks at. The
	// cursor and the statuses of the versions it dealt with are stored
	// after each pass.
	ResyncPass = 500
	// ResyncScanEvery is how often a round reads the record of every
	// bucket, each a consistent read of the metadata; the rounds between
	// read the records of the resyncs that are running only. A resync asked
	// for begins within it. No round reads every bucket while no bucket has
	// a replication configuration, which the targets tell (delivery.Target.
	// Refs of a target of kind s3).
	ResyncScanEvery = 10 * time.Second
	// resyncLook is how often Run asks whether this node leads.
	resyncLook = time.Second
	// resyncIdle is the longest pause of the walks after a round that moved
	// nothing: no resync running, or all of them waiting or held. A walk
	// that is due again sooner - one that left an object the outbox was
	// sending, or was held - shortens it to then.
	resyncIdle = 10 * time.Second
	// resyncBusyAgain is how long a walk leaves an object the outbox is
	// sending before it comes back to it; the walks pause no longer than
	// that when such a walk is the next one due.
	resyncBusyAgain = time.Second
	// ResyncPreemptions is how often the outbox ends a resync's send of one
	// version before it waits for that send instead - within its own
	// attempt's deadline - so that a key the outbox writes to again and
	// again does not keep a large old version of it from crossing.
	ResyncPreemptions = 3
)

// Why a resync that is running does not go on, as ResyncProgress says it.
const (
	// ResyncWaitingConfiguration is a bucket whose replication configuration
	// no longer names the resync's target, or cannot be read: the walk goes
	// on when it names it again, or starts again for the target it names
	// when a resync is asked again.
	ResyncWaitingConfiguration = "configuration"
	// ResyncWaitingTarget is a target that is gone, is no other deployment
	// or no longer admits the bucket.
	ResyncWaitingTarget = "target"
	// ResyncWaitingOthers is a walk that has been through its bucket while
	// the target needs a resync (delivery.Cursor.ResyncNeeded): it is done
	// once every bucket sent to the target has been through too, as the
	// mark is the target's and one "done" clears it for all of them.
	ResyncWaitingOthers = "others"
	// ResyncWaitingObject is a walk that came to an object the outbox is
	// sending to the other deployment: it leaves it and comes back to it
	// after resyncBusyAgain.
	ResyncWaitingObject = "object"
)

// What a ResyncSender answers that is no failure of the version or of the
// target; the walk stops at the version and stores what came before it.
var (
	// ErrResyncObjectBusy says that the object there is being sent by the
	// outbox: the walk comes back to it after resyncBusyAgain. A resync
	// never waits for the outbox; the outbox ends a resync's send rather
	// than wait for it, up to ResyncPreemptions times for one version.
	ErrResyncObjectBusy = errors.New("resync: the object is being sent by the outbox")
	// ErrResyncPreempted says that the outbox ended the resync's send of the
	// object, which it came to send itself. It is ErrResyncObjectBusy too.
	ErrResyncPreempted = fmt.Errorf("%w: the outbox ended the send", ErrResyncObjectBusy)
	// ErrResyncConfiguration says that the bucket's replication
	// configuration no longer names the target, or the target no longer
	// admits the bucket: the walk waits rather than pass what follows.
	ErrResyncConfiguration = errors.New("resync: the configuration no longer sends the bucket to the target")
)

// ResyncSender sends a version a resync walks; the Replicator is one
// (Replicator.Resend). It reports the version's status and whether anything
// was sent. ErrResyncObjectBusy and ErrResyncConfiguration stop the walk at
// the version; any other error is a failure of the class it carries, and one
// whose class does not pass holds the walk at the version.
type ResyncSender interface {
	Resend(ctx context.Context, target delivery.Target, v object.Version) (status object.ReplicationStatus, sent bool, err error)
}

// ResyncWorkerConfig lists what a ResyncWorker depends on.
type ResyncWorkerConfig struct {
	Buckets ports.BucketStore
	// Index is where the versions of a bucket are walked, and their
	// statuses read again before they are acknowledged.
	Index ports.ObjectIndex
	// Store holds the record of each resync.
	Store ports.ReplicationStore
	// Targets is where the target of a resync is read.
	Targets ports.TargetStore
	// Outbox takes the statuses of the versions a pass dealt with, and
	// says whether a target needs a resync and how many rows it lost.
	Outbox ports.Outbox
	// Sender sends a version: the Replicator that the delivery worker
	// sends the outbox through, so that both are held to one rate and
	// never send to one object at once.
	Sender ResyncSender
	// AttemptTimeout gives the deadline of one attempt at a version, as
	// the delivery worker gives it to a row of replication
	// (ReplicationAttemptTimeout); nil is DefaultDeliveryAttemptTimeout.
	AttemptTimeout func(delivery.Item) time.Duration
	// Active reports whether this node leads the metadata group: only that
	// node walks. Nil is a node that always leads.
	Active func() bool
	Clock  ports.Clock
	Logger *slog.Logger
	// Entropy, a cryptographically secure source, draws the jitter of the
	// pauses after a failure (delivery.Backoff). Nil draws none.
	Entropy io.Reader
	// Auditor, when not nil, records the resyncs the worker starts by
	// itself (ResyncWorker).
	Auditor *Auditor
}

// ResyncWorker walks the resyncs that are running, on the node that leads
// the metadata group and nowhere else; a lead that is lost ends what is in
// flight at once, as it ends the delivery worker's. Each round gives every
// running resync one pass: up to ResyncPass versions from its cursor, in the
// order of the version index (keys in order, the versions of a key from the
// newest), each sent through the Sender, which asks the other deployment for
// it first and sends it only when it is not there. Then the statuses of the
// versions the pass dealt with are read again and acknowledged by version
// (ADR-0023, "outbox.ack"), and then the cursor is stored
// (replication.resync.put): a node that comes to lead goes on from the last
// pass that was stored, and what it walks again it finds there and does not
// send again.
//
// A version is sent when a rule of the bucket's configuration matches it: a
// version with bytes always, a delete marker only when the deciding rule
// copies markers and the state gave it a status when it was made - a marker
// without one was made before the configuration or by the lifecycle service,
// and the two cannot be told apart, so neither crosses (a marker can hide a
// version there). A version that arrived as a replica is never sent on, and
// a version that is FAILED is neither sent nor given another status: a
// resync never lifts FAILED.
//
// A failure whose class passes (delivery.Class.Passes) is the version's: it
// is counted and the walk goes on. Any other holds the walk at the version,
// as it holds the target: what the pass dealt with before it is stored, and
// the version is tried again after delivery.Backoff. An object the outbox is
// sending, and a configuration that stops sending the bucket to the target,
// stop the walk at the version the same way, with no failure counted.
//
// The walk ends when a pass finds fewer than ResyncPass versions. It is done
// only when the target lost no row since the walk began (delivery.Cursor.
// Lost, as this node saw it when it began or took the walk over); otherwise
// it begins again, as a row lost behind its cursor would never be sent. When
// the target needs a resync (delivery.Cursor.ResyncNeeded) the mark is the
// target's and the first "done" of any bucket sent there clears it (ADR-0023,
// "Open", 6): so the worker starts the resync of every other bucket the
// target admits whose configuration names it, and stores "done" only once
// every one of them has been through, this one last.
type ResyncWorker struct {
	buckets ports.BucketStore
	index   ports.ObjectIndex
	store   ports.ReplicationStore
	targets ports.TargetStore
	outbox  ports.Outbox
	sender  ResyncSender
	timeout func(delivery.Item) time.Duration
	active  func() bool
	clock   ports.Clock
	logger  *slog.Logger
	auditor *Auditor

	entropyMu sync.Mutex
	entropy   io.Reader

	mu sync.Mutex
	// runs is what this node knows of each resync it walks, by bucket,
	// since it came to lead.
	runs map[string]*resyncRun
	// running are the buckets whose resync the last full scan, or the
	// worker itself, found running; scannedAt is when every bucket was
	// last read.
	running   map[string]bool
	scannedAt time.Time
	// warned are the conditions logged once, until they clear.
	warned map[string]bool
}

// resyncRun is a resync as the worker walks it.
type resyncRun struct {
	// record is the record as this worker last read or stored it; one that
	// is stored otherwise - a resync asked again - is a new run.
	record bucket.Resync
	// lost is the target's count of rows lost (delivery.Cursor.Lost) when
	// this node began the walk or took it over; lostKnown says whether it
	// was read.
	lost      uint64
	lostKnown bool
	// failures counts the failures in a row of the version the walk is
	// held at; lastClass is the class of the last one; retryAt is when it
	// is tried again.
	failures  int
	lastClass delivery.Class
	retryAt   time.Time
	// waiting says why the walk does not go on, or nothing.
	waiting string
	// through says that the walk has been through its bucket, at end, and
	// waits for the end of the round (settle) or for the other buckets of
	// its target; end is stored only when that decides.
	through bool
	end     bucket.Resync
	// restarts counts the times the walk began again because its target
	// lost rows; preempted the times the outbox ended its send of an
	// object - both since this node came to lead.
	restarts, preempted int
}

// ResyncProgress is how a resync stands on the node that leads: the record
// it stored, and what is kept in memory only, since the node came to lead.
type ResyncProgress struct {
	Record bucket.Resync
	// State is the state of the walk's own sending, as a target's is
	// shown: ports.DeliveryOK, ports.DeliveryRetrying or
	// ports.DeliveryStalled.
	State string
	// LastErrorClass is the class of the last failure, or none.
	LastErrorClass delivery.Class
	// Waiting says why the walk does not go on, or nothing:
	// ResyncWaitingConfiguration, ResyncWaitingTarget, ResyncWaitingOthers
	// or ResyncWaitingObject.
	Waiting string
	// Restarts counts the times the walk began again because its target
	// lost rows while it walked; a resync never ends while its target
	// keeps losing them.
	Restarts int
	// Preempted counts the times the outbox ended the walk's send of an
	// object it came to send (ErrResyncPreempted).
	Preempted int
}

// ResyncStats is what one round of the walks did.
type ResyncStats struct {
	// Passes counts the passes made.
	Passes int
	// Scanned counts the versions looked at, Sent those sent.
	Scanned, Sent int
	// Held counts the passes held at a version by a failure.
	Held int
	// Done counts the resyncs stored as done; Started those the worker
	// started by itself, for a target that needs a resync; Restarted the
	// walks begun again because their target lost rows meanwhile.
	Done, Started, Restarted int
}

// moved reports whether the round changed anything: a round that did not
// is followed by a pause.
func (s ResyncStats) moved() bool {
	return s.Scanned > 0 || s.Done > 0 || s.Started > 0 || s.Restarted > 0
}

// NewResyncWorker returns a ResyncWorker. Run starts it.
func NewResyncWorker(cfg ResyncWorkerConfig) *ResyncWorker {
	w := &ResyncWorker{
		buckets: cfg.Buckets, index: cfg.Index, store: cfg.Store, targets: cfg.Targets, outbox: cfg.Outbox, sender: cfg.Sender,
		timeout: cfg.AttemptTimeout, active: cfg.Active, clock: cfg.Clock, logger: cfg.Logger, auditor: cfg.Auditor,
		entropy: cfg.Entropy, runs: map[string]*resyncRun{}, running: map[string]bool{}, warned: map[string]bool{},
	}
	if w.logger == nil {
		w.logger = slog.New(slog.DiscardHandler)
	}
	return w
}

func (w *ResyncWorker) isActive() bool {
	return w.active == nil || w.active()
}

// jitter draws a number from 0 up to 1 for delivery.Backoff.
func (w *ResyncWorker) jitter() float64 {
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

// Run walks while this node leads, until ctx ends. It looks every second
// whether the node leads: the walks begin when it comes to lead, and end -
// what is in flight at once - when it no longer does; what was kept of them
// in memory is forgotten.
func (w *ResyncWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(resyncLook)
	defer ticker.Stop()
	var session *resyncSession
	defer func() { session.stop(w) }()
	for {
		active := w.isActive()
		switch {
		case active && session == nil:
			session = w.begin(ctx)
		case !active && session != nil:
			session.stop(w)
			session = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// resyncSession is the walks of one time this node leads.
type resyncSession struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// begin starts the walks of a time this node leads.
func (w *ResyncWorker) begin(ctx context.Context) *resyncSession {
	ctx, cancel := context.WithCancel(ctx)
	s := &resyncSession{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		w.walk(ctx)
	}()
	return s
}

// stop ends the walks of a session, what is in flight too, waits for them,
// and forgets what was kept of them in memory. A nil session is none.
func (s *resyncSession) stop(w *ResyncWorker) {
	if s == nil {
		return
	}
	s.cancel()
	<-s.done
	w.mu.Lock()
	w.runs, w.running, w.scannedAt, w.warned = map[string]*resyncRun{}, map[string]bool{}, time.Time{}, map[string]bool{}
	w.mu.Unlock()
}

// walk makes rounds until ctx ends, pausing after a round that moved
// nothing (or failed) until the next walk is due: resyncIdle at most.
func (w *ResyncWorker) walk(ctx context.Context) {
	for ctx.Err() == nil {
		stats, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil && !errors.Is(err, domain.ErrNotLeader) {
			w.logger.WarnContext(ctx, "a round of the resyncs failed; tried again in a while", slog.Any("error", err))
		}
		if err != nil || !stats.moved() {
			pause(ctx, w.idle())
		}
	}
}

// idle returns how long the walks pause after a round that moved nothing:
// resyncIdle, or less when a walk that left an object or was held is due
// again sooner. A time that has passed is no reason to hurry: a walk due
// then was given its round, and waits for something else.
func (w *ResyncWorker) idle() time.Duration {
	now := w.clock.Now()
	wait := resyncIdle
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, run := range w.runs {
		if due := run.retryAt.Sub(now); due > 0 && due < wait {
			wait = due
		}
	}
	return wait
}

// resyncRound is what one round reads once: the targets and the cursors, and
// - for a target that needs a resync - the buckets sent to it.
type resyncRound struct {
	targets map[string]delivery.Target
	// cursors are those read at the start of the round: where a walk this
	// node begins or takes over counts the rows its target lost from.
	cursors map[string]delivery.Cursor
	// records are the records of resyncs the round read, by bucket.
	records map[string]bucket.Resync
	// naming are the buckets sent to a target, once read; through the
	// walks that have been through their buckets, by target, which settle
	// decides on at the end of the round.
	naming  map[string][]string
	through map[string][]*resyncRun
	stats   ResyncStats
}

// RunOnce makes one round: every resync that is running and not waiting gets
// one pass. On a node that does not lead it does nothing, and the error
// wraps domain.ErrNotLeader.
func (w *ResyncWorker) RunOnce(ctx context.Context) (ResyncStats, error) {
	if !w.isActive() {
		return ResyncStats{}, fmt.Errorf("resync: %w: this node does not walk", domain.ErrNotLeader)
	}
	targets, err := w.targets.ListTargets(ctx)
	if err != nil {
		return ResyncStats{}, fmt.Errorf("resync: %w", err)
	}
	round := &resyncRound{
		targets: make(map[string]delivery.Target, len(targets)), records: map[string]bucket.Resync{},
		naming: map[string][]string{}, through: map[string][]*resyncRun{},
	}
	for _, target := range targets {
		round.targets[target.Name] = target
	}
	running, err := w.runningRecords(ctx, targets)
	if err != nil {
		return round.stats, fmt.Errorf("resync: %w", err)
	}
	for _, record := range running {
		round.records[record.Bucket] = record
	}
	if len(running) == 0 {
		return round.stats, nil
	}
	state, err := w.outbox.OutboxCursors(ctx)
	if err != nil {
		return round.stats, fmt.Errorf("resync: %w", err)
	}
	round.cursors = make(map[string]delivery.Cursor, len(state.Cursors))
	for _, cursor := range state.Cursors {
		round.cursors[cursor.Target] = cursor
	}
	for _, record := range running {
		if ctx.Err() != nil || !w.isActive() {
			return round.stats, fmt.Errorf("resync: %w: the lead was lost", domain.ErrNotLeader)
		}
		if err := w.advance(ctx, record, round); err != nil {
			return round.stats, fmt.Errorf("resync: bucket %s: %w", record.Bucket, err)
		}
	}
	if ctx.Err() != nil || !w.isActive() {
		return round.stats, fmt.Errorf("resync: %w: the lead was lost", domain.ErrNotLeader)
	}
	if err := w.settle(ctx, round); err != nil {
		return round.stats, fmt.Errorf("resync: %w", err)
	}
	return round.stats, nil
}

// replicating returns the targets a replication configuration names: each
// names a target of kind s3, which counts the configurations that name it
// (delivery.Target.Refs).
func replicating(targets []delivery.Target) []delivery.Target {
	var out []delivery.Target
	for _, target := range targets {
		if target.Kind.Serves(delivery.ConsumerReplication) && target.Refs > 0 {
			out = append(out, target)
		}
	}
	return out
}

// admitted reports whether one of the targets admits the bucket.
func admitted(targets []delivery.Target, bucketName string) bool {
	for _, target := range targets {
		if target.Admits(bucketName) {
			return true
		}
	}
	return false
}

// runningRecords returns the records of the resyncs that are running, and
// forgets what was kept of those that are not. Every ResyncScanEvery it
// reads the record of every bucket that a target named by a replication
// configuration admits - none while no bucket has a configuration, as no
// resync can go on then, and none of a bucket no such target admits, whose
// resync would only wait; between, those it found running.
func (w *ResyncWorker) runningRecords(ctx context.Context, targets []delivery.Target) ([]bucket.Resync, error) {
	now := w.clock.Now()
	w.mu.Lock()
	full := w.scannedAt.IsZero() || now.Sub(w.scannedAt) >= ResyncScanEvery || now.Before(w.scannedAt)
	names := make([]string, 0, len(w.running))
	for name := range w.running {
		names = append(names, name)
	}
	w.mu.Unlock()
	if full {
		names = nil
		if sending := replicating(targets); len(sending) > 0 {
			buckets, err := w.buckets.ListBuckets(ctx)
			if err != nil {
				return nil, err
			}
			for _, b := range buckets {
				if admitted(sending, b.Name) {
					names = append(names, b.Name)
				}
			}
		}
	}
	slices.Sort(names)
	var running []bucket.Resync
	present := map[string]bool{}
	for _, name := range names {
		record, err := w.store.GetResync(ctx, name)
		switch {
		case errors.Is(err, domain.ErrBucketNotFound):
			continue
		case err != nil:
			return nil, err
		case record.State == bucket.ResyncRunning:
			running = append(running, record)
			present[name] = true
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if full {
		w.scannedAt = now
	}
	w.running = present
	for name := range w.runs {
		if !present[name] {
			delete(w.runs, name)
		}
	}
	return running, nil
}

// runOf returns what is kept of a resync whose stored record is record: a
// new run when the record is not the one this worker last read or stored -
// the resync was asked again, or another leader went on with it.
func (w *ResyncWorker) runOf(record bucket.Resync) *resyncRun {
	w.mu.Lock()
	defer w.mu.Unlock()
	run := w.runs[record.Bucket]
	if run == nil || run.record != record {
		run = &resyncRun{record: record}
		w.runs[record.Bucket] = run
	}
	return run
}

// warn logs a condition once, until it clears (cleared).
func (w *ResyncWorker) warn(ctx context.Context, key, msg string, attrs ...any) {
	w.mu.Lock()
	logged := w.warned[key]
	w.warned[key] = true
	w.mu.Unlock()
	if !logged {
		w.logger.WarnContext(ctx, msg, attrs...)
	}
}

// cleared says that a condition logged by warn holds no more.
func (w *ResyncWorker) cleared(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.warned, key)
}

// advance gives one resync its pass, unless it waits.
func (w *ResyncWorker) advance(ctx context.Context, record bucket.Resync, round *resyncRound) error {
	run := w.runOf(record)
	cursor, known := round.cursors[record.Target]
	w.mu.Lock()
	if known && !run.lostKnown {
		// Where this node begins the walk, or takes it over: rows lost
		// after this are counted against it.
		run.lost, run.lostKnown = cursor.Lost, true
	}
	retryAt := run.retryAt
	w.mu.Unlock()
	if w.clock.Now().Before(retryAt) {
		return nil
	}
	config, err := w.configuration(ctx, record.Bucket)
	switch {
	case errors.Is(err, domain.ErrBucketNotFound):
		return nil
	case err != nil:
		w.warn(ctx, "configuration/"+record.Bucket, "the replication configuration of a bucket that is resynced cannot be read; the resync waits",
			slog.String("bucket", record.Bucket), slog.Any("error", err))
		w.wait(run, ResyncWaitingConfiguration)
		return nil
	case config.Target != record.Target:
		w.wait(run, ResyncWaitingConfiguration)
		return nil
	}
	w.cleared("configuration/" + record.Bucket)
	target, ok := round.targets[record.Target]
	if !ok || !target.Kind.Serves(delivery.ConsumerReplication) || !target.Admits(record.Bucket) {
		w.wait(run, ResyncWaitingTarget)
		return nil
	}
	w.mu.Lock()
	through := run.through
	if through {
		// Through already: the end of the round decides on it again, with
		// nothing read or stored for it here.
		round.through[target.Name] = append(round.through[target.Name], run)
	} else {
		run.waiting = ""
	}
	w.mu.Unlock()
	if through {
		return nil
	}
	return w.pass(ctx, run, record, config, target, round)
}

// wait records why a resync does not go on.
func (w *ResyncWorker) wait(run *resyncRun, reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	run.waiting = reason
}

// configuration reads the replication configuration of a bucket.
func (w *ResyncWorker) configuration(ctx context.Context, bucketName string) (bucket.ReplicationConfig, error) {
	raw, err := w.buckets.GetBucketConfig(ctx, bucketName, bucket.ConfigReplication)
	if err != nil {
		return bucket.ReplicationConfig{}, err
	}
	config, err := bucket.ParseReplicationConfig(raw)
	if err != nil {
		return bucket.ReplicationConfig{}, fmt.Errorf("%w: the stored replication configuration of a bucket: %s", domain.ErrIntegrity, err.Error())
	}
	return config, nil
}

// page returns the next ResyncPass versions after the cursor of a record. A
// cursor whose version is gone begins again at its key: what is walked again
// is found there and not sent again.
func (w *ResyncWorker) page(ctx context.Context, record bucket.Resync) ([]object.Version, error) {
	start := ports.VersionCursor{}
	if record.Key != "" {
		start.Key = record.Key
		ref, err := object.ParseVersionRef(record.Version)
		if err != nil {
			return nil, fmt.Errorf("%w: the cursor of a resync: %s", domain.ErrIntegrity, err.Error())
		}
		v, err := w.index.GetVersion(ctx, record.Bucket, record.Key, ref)
		switch {
		case err == nil:
			start.After, start.Seq = &ref, v.Seq
		case !errors.Is(err, domain.ErrVersionNotFound) && !errors.Is(err, domain.ErrObjectNotFound):
			return nil, err
		}
	}
	versions := make([]object.Version, 0, ResyncPass)
	err := w.index.ScanVersions(ctx, record.Bucket, start, func(v object.Version, _ bool) ports.ScanControl {
		v.Bucket = record.Bucket
		versions = append(versions, v)
		return ports.ScanControl{Stop: len(versions) == ResyncPass}
	})
	if err != nil {
		return nil, err
	}
	return versions, nil
}

// pass walks one page of a resync, acknowledges the statuses of the versions
// it dealt with, and stores how far it came.
func (w *ResyncWorker) pass(
	ctx context.Context, run *resyncRun, record bucket.Resync, config bucket.ReplicationConfig, target delivery.Target, round *resyncRound,
) error {
	versions, err := w.page(ctx, record)
	if err != nil {
		return err
	}
	round.stats.Passes++
	next := record
	var (
		results []delivery.Result
		held    error
		stopped error
	)
	for _, v := range versions {
		if ctx.Err() != nil || !w.isActive() {
			// Nothing of the pass is stored: the next leader walks it again,
			// and what it finds there it does not send.
			return fmt.Errorf("%w: the lead was lost", domain.ErrNotLeader)
		}
		status, sent, err := w.send(ctx, config, target, v)
		switch {
		case errors.Is(err, ErrResyncObjectBusy), errors.Is(err, ErrResyncConfiguration):
			stopped = err
		case err != nil && !delivery.ClassOf(err).Passes():
			held = err
		}
		if stopped != nil || held != nil {
			break
		}
		next.Key, next.Version = v.Key, v.Ref().String()
		next.Scanned++
		round.stats.Scanned++
		if sent {
			next.Sent++
			round.stats.Sent++
		}
		if status != "" {
			results = append(results, delivery.Result{Bucket: v.Bucket, Key: v.Key, Version: v.Ref(), Status: status})
		}
	}
	if ctx.Err() != nil || !w.isActive() {
		// What was in flight was ended by the loss of the lead, not by the
		// other deployment: nothing is stored or counted as a failure.
		return fmt.Errorf("%w: the lead was lost", domain.ErrNotLeader)
	}
	if err := w.acknowledge(ctx, record, target, results); err != nil {
		return err
	}
	switch {
	case held != nil:
		round.stats.Held++
		w.held(ctx, run, record, target, held)
	case errors.Is(stopped, ErrResyncObjectBusy):
		w.mu.Lock()
		run.retryAt, run.waiting = w.clock.Now().Add(resyncBusyAgain), ResyncWaitingObject
		if errors.Is(stopped, ErrResyncPreempted) {
			run.preempted++
		}
		w.mu.Unlock()
	case stopped != nil:
		w.wait(run, ResyncWaitingConfiguration)
	default:
		w.mu.Lock()
		run.failures, run.lastClass, run.retryAt = 0, delivery.ClassNone, time.Time{}
		w.mu.Unlock()
	}
	if held == nil && stopped == nil && len(versions) < ResyncPass {
		w.finish(run, next, target, round)
		return nil
	}
	return w.save(ctx, run, next)
}

// acknowledge reports the statuses a pass gave, by version (the statuses
// first: a cursor stored is a pass whose versions have their statuses). Each
// version is read again just before: one that became FAILED meanwhile - the
// outbox gave up on a change of it - or is gone, or is a replica, is left
// out, so that a resync never puts a status over a newer FAILED.
func (w *ResyncWorker) acknowledge(ctx context.Context, record bucket.Resync, target delivery.Target, results []delivery.Result) error {
	kept := results[:0]
	for _, result := range results {
		v, err := w.index.GetVersion(ctx, result.Bucket, result.Key, result.Version)
		switch {
		case errors.Is(err, domain.ErrVersionNotFound), errors.Is(err, domain.ErrObjectNotFound), errors.Is(err, domain.ErrBucketNotFound):
			continue
		case err != nil:
			return err
		case v.Replication == object.ReplicationFailed || v.Replication == object.ReplicationReplica:
			continue
		}
		kept = append(kept, result)
	}
	if len(kept) == 0 {
		return nil
	}
	unrelated, err := w.outbox.AckOutbox(ctx, target.Name, delivery.Seq{}, 0, kept)
	if err != nil {
		return err
	}
	if unrelated > 0 {
		w.logger.WarnContext(ctx, "a resync reported statuses the target was not to give; its bucket is sent elsewhere now",
			slog.String("bucket", record.Bucket), slog.String("target", target.Name), slog.Int("unrelated", unrelated))
	}
	return nil
}

// held records a failure that holds a resync at a version: it is tried again
// after delivery.Backoff. Nothing of the failure is logged but its class.
func (w *ResyncWorker) held(ctx context.Context, run *resyncRun, record bucket.Resync, target delivery.Target, err error) {
	class := delivery.ClassOf(err)
	w.mu.Lock()
	run.failures++
	run.lastClass = class
	wait := delivery.Backoff(run.failures, w.jitter())
	run.retryAt = w.clock.Now().Add(wait)
	failures := run.failures
	w.mu.Unlock()
	w.logger.WarnContext(ctx, "a resync is held at a version; it is tried again",
		slog.String("bucket", record.Bucket), slog.String("target", target.Name), slog.String("class", string(class)),
		slog.Int("failures", failures), slog.Duration("pause", wait))
}

// send sends one version, unless no rule sends it.
func (w *ResyncWorker) send(ctx context.Context, config bucket.ReplicationConfig, target delivery.Target, v object.Version) (object.ReplicationStatus, bool, error) {
	if !resyncs(config, v) {
		return "", false, nil
	}
	timeout := DefaultDeliveryAttemptTimeout
	if w.timeout != nil {
		if of := w.timeout(delivery.Item{Size: v.Size}); of > 0 {
			timeout = of
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return w.sender.Resend(ctx, target, v)
}

// resyncs reports whether a resync sends a version: one a rule of the
// configuration matches, but never a replica, never a version that is
// FAILED (a resync never lifts FAILED), and a delete marker only when the
// state gave it a status when it was made - one without was made before the
// configuration or by the lifecycle service, which cannot be told apart, and
// a marker that crosses can hide a version there.
func resyncs(config bucket.ReplicationConfig, v object.Version) bool {
	switch {
	case v.Replication == object.ReplicationReplica, v.Replication == object.ReplicationFailed:
		return false
	case v.DeleteMarker:
		return v.Replication != "" && len(config.Match(delivery.EventMarker, v.Key)) > 0
	}
	return len(config.Match(delivery.EventPut, v.Key)) > 0
}

// save stores how far a resync has come, unless its record was stored
// otherwise since this worker read it - a resync asked again begins again,
// and is not overwritten by the pass of the walk before - and unless it is
// what is stored already, which is not proposed again.
func (w *ResyncWorker) save(ctx context.Context, run *resyncRun, next bucket.Resync) error {
	w.mu.Lock()
	read := run.record
	w.mu.Unlock()
	if next == read {
		// Nothing to store: what the round read is what this worker last
		// stored or read.
		return nil
	}
	now, err := w.store.GetResync(ctx, next.Bucket)
	switch {
	case errors.Is(err, domain.ErrBucketNotFound):
		return nil
	case err != nil:
		return err
	case now != read:
		w.mu.Lock()
		delete(w.runs, next.Bucket)
		w.mu.Unlock()
		return nil
	case now != next:
		if err := w.store.PutResync(ctx, next); err != nil {
			if errors.Is(err, domain.ErrBucketNotFound) || errors.Is(err, domain.ErrEntityNotFound) {
				return nil
			}
			return err
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	run.record = next
	if next.State == bucket.ResyncDone && w.runs[next.Bucket] == run {
		// Nothing more is walked: nothing is shown of it but its record.
		delete(w.runs, next.Bucket)
	}
	return nil
}

// restart begins a walk that has been through its bucket again from the
// first version: its target lost rows since it began, which may have been
// behind its cursor.
//
// There is no bound on how often: a resync never ends while its target keeps
// losing rows. From the second time on it is logged as a warning.
func (w *ResyncWorker) restart(ctx context.Context, run *resyncRun, cursor delivery.Cursor, round *resyncRound) error {
	w.mu.Lock()
	record := run.record
	w.mu.Unlock()
	begin := bucket.Resync{Bucket: record.Bucket, Target: record.Target, State: bucket.ResyncRunning}
	if err := w.save(ctx, run, begin); err != nil {
		return err
	}
	w.mu.Lock()
	restarts := 0
	if w.runs[record.Bucket] == run {
		run.through, run.waiting, run.lost, run.lostKnown = false, "", cursor.Lost, true
		run.restarts++
		restarts = run.restarts
	}
	w.mu.Unlock()
	round.stats.Restarted++
	attrs := []any{slog.String("bucket", record.Bucket), slog.String("target", record.Target), slog.Int("restarts", restarts)}
	if restarts >= 2 {
		w.logger.WarnContext(ctx, "a resync begins again, once more: its target keeps losing rows, and it ends only when it loses none while a walk goes on", attrs...)
		return nil
	}
	w.logger.InfoContext(ctx, "a resync begins again: its target lost rows while it walked", attrs...)
	return nil
}

// lostSince reports whether the target lost rows since the run began, as
// this node saw it; a run that does not know is taken to have.
func (w *ResyncWorker) lostSince(run *resyncRun, cursor delivery.Cursor) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !run.lostKnown || cursor.Lost > run.lost
}

// finish records that a walk has been through its bucket, at next; what
// follows - and what is stored, once - is decided for its target at the end
// of the round (settle).
func (w *ResyncWorker) finish(run *resyncRun, next bucket.Resync, target delivery.Target, round *resyncRound) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.runs[next.Bucket] != run {
		return
	}
	run.through, run.end = true, next
	round.through[target.Name] = append(round.through[target.Name], run)
}

// keep stores where a walk that is through has come to, when it waits.
func (w *ResyncWorker) keep(ctx context.Context, run *resyncRun) error {
	w.mu.Lock()
	end := run.end
	w.mu.Unlock()
	return w.save(ctx, run, end)
}

// settle decides, once a round for each target, what becomes of the walks
// that have been through their buckets, reading the cursors once, after
// every pass of the round: rows lost while they walked count.
//
// A walk begins again when the target lost rows since it began. Otherwise
// it is done, when the target does not need a resync. When it does, the
// other buckets the target is sent are resynced too - those without a
// running resync of the target are started - and "done" is stored only once
// every one of them has been through and the target lost no row since the
// earliest of them began; otherwise every one of them begins again. A round
// reads the record of a bucket of the target only when it did not read it
// already, so it costs what the target's buckets are, once.
func (w *ResyncWorker) settle(ctx context.Context, round *resyncRound) error {
	if len(round.through) == 0 {
		return nil
	}
	state, err := w.outbox.OutboxCursors(ctx)
	if err != nil {
		return err
	}
	cursors := make(map[string]delivery.Cursor, len(state.Cursors))
	for _, c := range state.Cursors {
		cursors[c.Target] = c
	}
	names := make([]string, 0, len(round.through))
	for name := range round.through {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		cursor, known := cursors[name]
		if !known {
			// Whether the target lost rows is not known: where the walks
			// came to is kept, and the next round decides.
			for _, run := range round.through[name] {
				if err := w.keep(ctx, run); err != nil {
					return err
				}
			}
			continue
		}
		if err := w.settleTarget(ctx, round.targets[name], cursor, round.through[name], round); err != nil {
			return err
		}
	}
	return nil
}

// settleTarget is settle for the walks of one target that are through.
func (w *ResyncWorker) settleTarget(ctx context.Context, target delivery.Target, cursor delivery.Cursor, runs []*resyncRun, round *resyncRound) error {
	if !cursor.ResyncNeeded {
		for _, run := range runs {
			if err := w.end(ctx, []*resyncRun{run}, cursor, round); err != nil {
				return err
			}
		}
		return nil
	}
	for _, run := range runs {
		w.wait(run, ResyncWaitingOthers)
	}
	// Where each walk that waits came to is kept, so that a new leader does
	// not walk it again; one kept already is not proposed again.
	keepAll := func() error {
		for _, run := range runs {
			if err := w.keep(ctx, run); err != nil {
				return err
			}
		}
		return nil
	}
	naming, err := w.namingTarget(ctx, target, round)
	if err != nil {
		w.warn(ctx, "naming/"+target.Name, "the buckets sent to a target that needs a resync cannot be read; the resync waits",
			slog.String("target", target.Name), slog.Any("error", err))
		return keepAll()
	}
	w.cleared("naming/" + target.Name)
	through := make(map[string]*resyncRun, len(runs))
	for _, run := range runs {
		through[run.record.Bucket] = run
	}
	all := true
	// Every walk that is through ends with the others, also one whose
	// bucket the reading of the configurations just now did not find.
	walked := slices.Clone(runs)
	for _, name := range naming {
		if through[name] != nil {
			continue
		}
		all = false
		record, read := round.records[name]
		if !read {
			record, err = w.store.GetResync(ctx, name)
			switch {
			case errors.Is(err, domain.ErrBucketNotFound):
				continue
			case err != nil:
				return err
			}
			round.records[name] = record
		}
		if record.State == bucket.ResyncRunning && record.Target == target.Name {
			// Walking, or waiting: it is through later.
			w.mu.Lock()
			w.running[name] = true
			w.mu.Unlock()
			continue
		}
		if err := w.start(ctx, name, target.Name, round); err != nil {
			return err
		}
		round.records[name] = bucket.Resync{Bucket: name, Target: target.Name, State: bucket.ResyncRunning}
	}
	if !all {
		return keepAll()
	}
	return w.end(ctx, walked, cursor, round)
}

// end ends walks that have been through together: all of them begin again
// when the target lost rows since the earliest of them began; otherwise each
// is stored done.
func (w *ResyncWorker) end(ctx context.Context, runs []*resyncRun, cursor delivery.Cursor, round *resyncRound) error {
	for _, run := range runs {
		if w.lostSince(run, cursor) {
			for _, run := range runs {
				if err := w.restart(ctx, run, cursor, round); err != nil {
					return err
				}
			}
			return nil
		}
	}
	for _, run := range runs {
		w.mu.Lock()
		done := run.end
		w.mu.Unlock()
		done.State = bucket.ResyncDone
		if err := w.save(ctx, run, done); err != nil {
			return err
		}
		round.stats.Done++
	}
	return nil
}

// namingTarget returns the buckets that the target admits and whose
// replication configuration names it, once a round for each target. It
// reads no configuration of a bucket the target does not admit - one that
// named it could not be stored - and stops once it has found as many as the
// target counts (delivery.Target.Refs). A configuration that cannot be read
// could name it: that is an error, and nothing is concluded.
func (w *ResyncWorker) namingTarget(ctx context.Context, target delivery.Target, round *resyncRound) ([]string, error) {
	if names, ok := round.naming[target.Name]; ok {
		return names, nil
	}
	buckets, err := w.buckets.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, b := range buckets {
		if len(names) >= target.Refs {
			break
		}
		if !target.Admits(b.Name) {
			continue
		}
		config, err := w.configuration(ctx, b.Name)
		switch {
		case errors.Is(err, domain.ErrBucketNotFound):
			continue
		case err != nil:
			return nil, err
		case config.Target == target.Name:
			names = append(names, b.Name)
		}
	}
	round.naming[target.Name] = names
	return names, nil
}

// start starts the resync of a bucket the worker found sent to a target that
// needs one, and records that it did: in the log and, as something the
// deployment did by itself, in the audit trail.
func (w *ResyncWorker) start(ctx context.Context, bucketName, target string, round *resyncRound) error {
	record := bucket.Resync{Bucket: bucketName, Target: target, State: bucket.ResyncRunning}
	if err := w.store.PutResync(ctx, record); err != nil {
		if errors.Is(err, domain.ErrBucketNotFound) || errors.Is(err, domain.ErrEntityNotFound) {
			return nil
		}
		return err
	}
	w.mu.Lock()
	w.running[bucketName] = true
	w.mu.Unlock()
	round.stats.Started++
	w.logger.InfoContext(ctx, "a resync was started for a bucket sent to a target that needs one",
		slog.String("bucket", bucketName), slog.String("target", target))
	if w.auditor != nil {
		resource := iam.Resource{Bucket: bucketName}.String()
		w.auditor.Succeeded(ctx, w.auditor.System(string(iam.ActionResyncReplication), resource,
			map[string]string{"target": target, "resync": "started", "reason": "resync_needed"}))
	}
	return nil
}

// Progress returns how the resync of a bucket stands on this node, which
// walks it, and false on a node that does not lead or knows of no resync of
// the bucket running. The full status of a bucket's replication reads it
// (PLAN 8.1.7).
func (w *ResyncWorker) Progress(bucketName string) (ResyncProgress, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	run := w.runs[bucketName]
	if run == nil || !w.isActive() {
		return ResyncProgress{}, false
	}
	return ResyncProgress{
		Record: run.record, State: deliveryState(run.failures, run.lastClass), LastErrorClass: run.lastClass, Waiting: run.waiting,
		Restarts: run.restarts, Preempted: run.preempted,
	}, true
}
