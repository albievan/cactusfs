// Package audit exports the audit trail as it grows: one JSON object per
// record, on a stream of its own. The trail itself lives in the replicated
// state; this adapter is for shipping it to wherever logs are kept.
//
// It consumes the event that says a record was appended and reads the
// record from the audit log, so what it writes is what the trail holds,
// hash and all. Like every consumer of the in-process bus it can miss an
// event; a reader of the stream can tell from the sequence numbers, and the
// trail is the authority.
package audit

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"time"

	domainaudit "github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/ports"
)

// line is one exported record.
type line struct {
	Stream       string            `json:"stream"`
	Sequence     uint64            `json:"sequence"`
	Time         time.Time         `json:"time"`
	Actor        string            `json:"actor"`
	AccessKeyID  string            `json:"access_key_id,omitempty"`
	Action       string            `json:"action"`
	Resource     string            `json:"resource,omitempty"`
	Outcome      string            `json:"outcome"`
	SourceIP     string            `json:"source_ip,omitempty"`
	RequestID    string            `json:"request_id,omitempty"`
	Details      map[string]string `json:"details,omitempty"`
	PreviousHash string            `json:"previous_hash,omitempty"`
	Hash         string            `json:"hash"`
}

// Exporter writes audit records to a stream.
type Exporter struct {
	log    ports.AuditLog
	logger *slog.Logger

	mu  sync.Mutex
	out io.Writer
	// last is the sequence number of the last record written.
	last uint64
}

// NewExporter returns an Exporter that writes to out.
func NewExporter(log ports.AuditLog, out io.Writer, logger *slog.Logger) *Exporter {
	return &Exporter{log: log, out: out, logger: logger}
}

// readTimeout bounds one read of the audit log.
const readTimeout = 30 * time.Second

// Handle consumes an event of the in-process bus. On the event that a record
// was appended it writes every record it has not written yet, up to that
// one: a record whose event was missed is caught up with the next.
func (e *Exporter) Handle(ctx context.Context, event ports.Event) {
	recorded, ok := event.Data.(ports.AuditEvent)
	if event.Type != ports.EventAuditRecorded || !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if recorded.Sequence <= e.last {
		return
	}
	if e.last == 0 {
		// The first record seen in this process: start there, not at the
		// beginning of a trail that may be years long.
		e.last = recorded.Sequence - 1
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	for e.last < recorded.Sequence {
		records, err := e.log.ListAudit(ctx, e.last, int(min(recorded.Sequence-e.last, 500)))
		if err != nil || len(records) == 0 {
			e.logger.WarnContext(ctx, "audit records not exported", slog.Uint64("after", e.last), slog.Any("error", err))
			return
		}
		for _, record := range records {
			if err := e.write(record); err != nil {
				e.logger.WarnContext(ctx, "audit record not exported", slog.Uint64("sequence", record.Sequence), slog.Any("error", err))
				return
			}
			e.last = record.Sequence
		}
	}
}

func (e *Exporter) write(r domainaudit.Record) error {
	data, err := json.Marshal(line{
		Stream: "audit", Sequence: r.Sequence, Time: r.Time, Actor: r.Actor, AccessKeyID: r.AccessKeyID, Action: r.Action,
		Resource: r.Resource, Outcome: string(r.Outcome), SourceIP: r.SourceIP, RequestID: r.RequestID, Details: r.Details,
		PreviousHash: r.PreviousHash, Hash: r.Hash,
	})
	if err != nil {
		return err
	}
	_, err = e.out.Write(append(data, '\n'))
	return err
}
