package audit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	auditexport "github.com/albievan/cactusfs/internal/adapters/audit"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

func appendRecords(t *testing.T, store *porttest.MemIAM, n int) {
	t.Helper()
	for range n {
		record := audit.Record{
			Time: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), Actor: "root", Action: "admin:PutUser",
			Resource: "arn:cactusfs:admin:::user/x", Outcome: audit.Succeeded, Details: map[string]string{"change": "create"},
		}
		if err := store.AppendAudit(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
}

func event(sequence uint64) ports.Event {
	return ports.Event{Type: ports.EventAuditRecorded, Data: ports.AuditEvent{Sequence: sequence}}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// TestExporterWritesTheTrailAsItGrows: one JSON line per record, in order,
// with the hashes; a record whose event was missed is caught up with the
// next; nothing is written twice.
func TestExporterWritesTheTrailAsItGrows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := porttest.NewMemIAM()
	var out bytes.Buffer
	exporter := auditexport.NewExporter(store, &out, slog.New(slog.DiscardHandler))

	lines := func() []map[string]any {
		var parsed []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			if line == "" {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Fatalf("exported line is not JSON: %q", line)
			}
			parsed = append(parsed, record)
		}
		return parsed
	}

	// The trail already has history when the node starts; the exporter
	// begins with the first record it is told of.
	appendRecords(t, store, 3)
	appendRecords(t, store, 1)
	exporter.Handle(ctx, event(4))
	if got := lines(); len(got) != 1 || got[0]["sequence"] != float64(4) || got[0]["stream"] != "audit" ||
		got[0]["actor"] != "root" || got[0]["outcome"] != "success" || got[0]["hash"] == "" || got[0]["previous_hash"] == "" {
		t.Fatalf("exported = %v", got)
	}
	// Events 5 and 6 are missed; 7 arrives.
	appendRecords(t, store, 3)
	exporter.Handle(ctx, event(7))
	got := lines()
	if len(got) != 4 {
		t.Fatalf("%d lines after a gap in the events, want 4", len(got))
	}
	for i, line := range got {
		if line["sequence"] != float64(4+i) {
			t.Errorf("line %d has sequence %v", i, line["sequence"])
		}
	}
	// The same event again, an older one, and events of other kinds.
	exporter.Handle(ctx, event(7))
	exporter.Handle(ctx, event(5))
	exporter.Handle(ctx, ports.Event{Type: ports.EventBucketCreated, Data: ports.BucketEvent{Bucket: "b"}})
	exporter.Handle(ctx, ports.Event{Type: ports.EventAuditRecorded, Data: "not an audit event"})
	if len(lines()) != 4 {
		t.Errorf("%d lines after repeated and foreign events, want 4", len(lines()))
	}
	// What is exported is what the trail holds: the chain verifies.
	records, _ := store.ListAudit(ctx, 3, 10)
	for i, line := range lines() {
		if line["hash"] != records[i].Hash || line["previous_hash"] != records[i].PreviousHash {
			t.Errorf("line %d does not carry the record's hashes", i)
		}
	}

	// A stream that cannot be written, and an event for a record that is
	// not there: nothing breaks, and the next event catches up.
	broken := auditexport.NewExporter(store, failingWriter{}, slog.New(slog.DiscardHandler))
	broken.Handle(ctx, event(7))
	exporter.Handle(ctx, event(99))
	appendRecords(t, store, 1)
	exporter.Handle(ctx, event(8))
	if len(lines()) != 5 {
		t.Errorf("%d lines at the end, want 5", len(lines()))
	}
}
