package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

const (
	// auditQueueSize bounds the denials and authentication failures that
	// wait to be written.
	auditQueueSize = 1024
	// auditWindow is how long repeats of one refusal are counted instead of
	// written one by one.
	auditWindow = 10 * time.Second
	// auditTracked bounds how many distinct refusals are counted at once.
	auditTracked = 4096
	// auditBudget is how many kinds of refusal are written, one record
	// each, per window. Beyond it they are counted into one record. It
	// bounds what refusals can add to the metadata log to a few records a
	// second, whatever is thrown at the node.
	auditBudget = 64
	// auditWriteTimeout bounds one write to the audit log.
	auditWriteTimeout = 30 * time.Second
)

// Auditor writes the audit trail.
//
// Changes to identities are audited by the store, in the same Raft entry as
// the change: Auditor only builds their records. Refusals - authorization
// denials and failed authentications - are written by Auditor itself, in the
// background, because the request that was refused must not wait for a Raft
// commit and because anybody can cause them: an unauthenticated client must
// not be able to fill the metadata log one request at a time. The first
// refusal of a kind is written at once; repeats of it within a short window
// are counted and written as one record saying how many there were. If even
// that outruns the log, records are dropped, counted and reported in the
// node's log: availability of the store wins over completeness of the count
// of refusals, never over the record of a change.
type Auditor struct {
	log    ports.AuditLog
	clock  ports.Clock
	logger *slog.Logger

	queue chan audit.Record

	mu      sync.Mutex
	repeats map[string]*repeated
	dropped uint64
	// windowStart, written and suppressed are the budget of the current
	// window.
	windowStart time.Time
	written     int
	suppressed  uint64
}

type repeated struct {
	record audit.Record
	since  time.Time
	count  int
}

// NewAuditor returns an Auditor. Its Run method must be running for refusals
// to be written.
func NewAuditor(log ports.AuditLog, clock ports.Clock, logger *slog.Logger) *Auditor {
	return &Auditor{
		log: log, clock: clock, logger: logger,
		queue: make(chan audit.Record, auditQueueSize), repeats: map[string]*repeated{},
	}
}

// actorOf names a principal in the audit trail.
func actorOf(p iam.Principal) string {
	switch {
	case p.Service != "":
		return "service/" + p.Service
	case p.NodeID != "":
		return "node/" + p.NodeID
	case p.Root:
		return "root"
	case p.User != "":
		return "user/" + p.User
	case p.AccessKeyID != "":
		return "key/" + p.AccessKeyID
	default:
		return "anonymous"
	}
}

// bound keeps a string that a client controls within what a record may
// hold: a long one is cut and given the hash of the whole, so that the record
// can always be written and two different long strings stay different.
func bound(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return s[:limit-80] + "...sha256:" + hex.EncodeToString(sum[:])
}

func boundAll(details map[string]string) map[string]string {
	if len(details) == 0 {
		return nil
	}
	out := make(map[string]string, len(details))
	for key, value := range details {
		out[bound(key)] = bound(value)
	}
	return out
}

// Record builds the audit record of something a principal did or tried.
func (a *Auditor) Record(p iam.Principal, action iam.Action, resource iam.Resource, outcome audit.Outcome, details map[string]string) audit.Record {
	record := audit.Record{
		Time: a.clock.Now().UTC(), Actor: bound(actorOf(p)), AccessKeyID: bound(p.AccessKeyID), Action: bound(string(action)),
		Resource: bound(resource.String()), Outcome: outcome, RequestID: bound(p.Session.RequestID), Details: boundAll(details),
	}
	if p.Session.SourceIP.IsValid() {
		record.SourceIP = p.Session.SourceIP.String()
	}
	return record
}

// System builds the record of something the node did by itself: forming a
// deployment, recording its topology, registering.
func (a *Auditor) System(action, resource string, details map[string]string) audit.Record {
	return audit.Record{
		Time: a.clock.Now().UTC(), Actor: "system", Action: bound(action), Resource: bound(resource),
		Outcome: audit.Succeeded, Details: boundAll(details),
	}
}

// Succeeded records, in the background, an administrative change that the
// store does not audit in the same entry as the change itself: a bucket made
// or removed, a deployment bootstrapped, a topology recorded. It is written
// right after the change; a node that stops in between leaves the change
// without its record, which changes to identities never are.
func (a *Auditor) Succeeded(ctx context.Context, record audit.Record) {
	a.enqueue(ctx, record)
}

// Denied records that the authorizer refused a request.
func (a *Auditor) Denied(ctx context.Context, req iam.Request, reason string) {
	record := a.Record(req.Principal, req.Action, req.Resource, audit.Denied, map[string]string{"reason": reason})
	// Repeats are counted per actor, action and address - not per
	// resource, or somebody refused once could write a record for every
	// name they can think of.
	a.refusal(ctx, record, record.Actor+"\x00"+record.Action+"\x00"+record.SourceIP)
}

// AuthenticationFailed records that somebody presented credentials that
// were not accepted. accessKeyID is what they claimed to be; it may name no
// key at all.
func (a *Auditor) AuthenticationFailed(ctx context.Context, action, accessKeyID string, session iam.Session, reason string) {
	record := audit.Record{
		Time: a.clock.Now().UTC(), Actor: "anonymous", AccessKeyID: bound(accessKeyID), Action: bound(action),
		Outcome: audit.Failed, RequestID: bound(session.RequestID), Details: map[string]string{"reason": bound(reason)},
	}
	if session.SourceIP.IsValid() {
		record.SourceIP = session.SourceIP.String()
	}
	// Counted per address and kind of failure only: what the caller
	// claims to be is the caller's to invent.
	a.refusal(ctx, record, record.Action+"\x00"+record.SourceIP)
}

// refusal writes a record, or counts it as a repeat of one written lately,
// or - when more kinds of refusal arrive than the budget of a window allows -
// counts it as suppressed. Nothing is lost count of: every refusal is in a
// record, in a repeat count, in a suppressed count or in the dropped count.
func (a *Auditor) refusal(ctx context.Context, record audit.Record, key string) {
	var due []audit.Record
	write := false

	a.mu.Lock()
	seen, ok := a.repeats[key]
	if ok && record.Time.Sub(seen.since) >= auditWindow {
		// Its window has ended and nobody summarised it yet: do that
		// now, before the entry is used again.
		if summary, any := seen.summary(record.Time); any {
			due = append(due, summary)
		}
		delete(a.repeats, key)
		ok = false
	}
	switch {
	case ok:
		seen.count++
	case record.Time.Sub(a.windowStart) >= auditWindow:
		// A new budget window.
		if a.suppressed > 0 {
			due = append(due, a.suppressedRecord(record.Time))
		}
		a.windowStart, a.written, a.suppressed = record.Time, 1, 0
		a.repeats[key] = &repeated{record: record, since: record.Time}
		write = true
	case a.written < auditBudget && len(a.repeats) < auditTracked:
		a.written++
		a.repeats[key] = &repeated{record: record, since: record.Time}
		write = true
	default:
		a.suppressed++
	}
	a.mu.Unlock()

	for _, summary := range due {
		a.enqueue(ctx, summary)
	}
	if write {
		a.enqueue(ctx, record)
	}
}

// summary returns the record that says how often a refusal was repeated.
func (r *repeated) summary(now time.Time) (audit.Record, bool) {
	if r.count == 0 {
		return audit.Record{}, false
	}
	record := r.record
	record.Time, record.RequestID = now.UTC(), ""
	record.Details = map[string]string{"repeated": strconv.Itoa(r.count), "since": r.since.UTC().Format(time.RFC3339)}
	if reason, ok := r.record.Details["reason"]; ok {
		record.Details["reason"] = reason
	}
	return record, true
}

// suppressedRecord says how many refusals of further kinds were not written
// one by one in the window that just ended. The caller holds the lock.
func (a *Auditor) suppressedRecord(now time.Time) audit.Record {
	return audit.Record{
		Time: now.UTC(), Actor: "system", Action: "audit:Suppressed", Outcome: audit.Failed,
		Details: map[string]string{
			"suppressed": strconv.FormatUint(a.suppressed, 10), "since": a.windowStart.UTC().Format(time.RFC3339),
			"reason": "more kinds of refusal than are written per window",
		},
	}
}

func (a *Auditor) enqueue(ctx context.Context, record audit.Record) {
	select {
	case a.queue <- record:
	default:
		a.mu.Lock()
		a.dropped++
		dropped := a.dropped
		a.mu.Unlock()
		a.logger.ErrorContext(ctx, "audit record dropped: the audit queue is full",
			slog.String("actor", record.Actor), slog.String("action", record.Action), slog.String("outcome", string(record.Outcome)),
			slog.Uint64("dropped_total", dropped))
	}
}

// Dropped returns how many refusals could not be queued.
func (a *Auditor) Dropped() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dropped
}

// Run writes queued records and, as their windows end, the counts of
// repeats, until ctx is cancelled. What is queued then is written before it
// returns.
func (a *Auditor) Run(ctx context.Context) {
	ticker := time.NewTicker(auditWindow / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			a.Flush(context.WithoutCancel(ctx))
			return
		case record := <-a.queue:
			a.write(ctx, record)
		case <-ticker.C:
			a.summarise(ctx, false)
		}
	}
}

// Flush writes everything that is queued or counted, now.
func (a *Auditor) Flush(ctx context.Context) {
	a.summarise(ctx, true)
	for {
		select {
		case record := <-a.queue:
			a.write(ctx, record)
		default:
			return
		}
	}
}

// summarise turns the counts of windows that have ended into records.
func (a *Auditor) summarise(ctx context.Context, all bool) {
	now := a.clock.Now()
	var due []audit.Record
	a.mu.Lock()
	for key, seen := range a.repeats {
		if !all && now.Sub(seen.since) < auditWindow {
			continue
		}
		if summary, any := seen.summary(now); any {
			due = append(due, summary)
		}
		delete(a.repeats, key)
	}
	if a.suppressed > 0 && (all || now.Sub(a.windowStart) >= auditWindow) {
		due = append(due, a.suppressedRecord(now))
		a.suppressed = 0
	}
	a.mu.Unlock()
	for _, record := range due {
		a.enqueue(ctx, record)
	}
}

func (a *Auditor) write(ctx context.Context, record audit.Record) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	if err := a.log.AppendAudit(ctx, record); err != nil {
		// The trail could not take it. The node's log is the last resort,
		// so that the refusal is at least somewhere.
		a.logger.ErrorContext(ctx, "audit record could not be written",
			slog.String("actor", record.Actor), slog.String("action", record.Action), slog.String("resource", record.Resource),
			slog.String("outcome", string(record.Outcome)), slog.Any("error", err))
	}
}

// ReadAudit returns up to limit audit records after the given sequence
// number, having verified that they continue the hash chain from the record
// before them. The error wraps domain.ErrIntegrity when they do not.
func ReadAudit(ctx context.Context, log ports.AuditLog, after uint64, limit int) ([]audit.Record, error) {
	var before audit.Record
	if after > 0 {
		previous, err := log.ListAudit(ctx, after-1, 1)
		if err != nil {
			return nil, fmt.Errorf("audit.Read: %w", err)
		}
		if len(previous) == 1 {
			before = previous[0]
		}
	}
	records, err := log.ListAudit(ctx, after, limit)
	if err != nil {
		return nil, fmt.Errorf("audit.Read: %w", err)
	}
	if err := audit.Verify(before, records); err != nil {
		return nil, fmt.Errorf("audit.Read: %w", err)
	}
	return records, nil
}
