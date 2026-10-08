package audit_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
)

var now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func chain(n int) []audit.Record {
	var records []audit.Record
	var previous audit.Record
	for i := range n {
		record := audit.Record{
			Time: now.Add(time.Duration(i) * time.Second), Actor: "user/alice", AccessKeyID: "AKID", Action: "admin:PutUser",
			Resource: "arn:cactusfs:admin:::user/bob", Outcome: audit.Succeeded, SourceIP: "10.0.0.1", RequestID: "R1",
			Details: map[string]string{"change": "update", "n": strings.Repeat("x", i)},
		}.Seal(previous)
		records = append(records, record)
		previous = record
	}
	return records
}

// TestChainDetectsEveryKindOfTampering: a record changed in any field, a
// record removed, inserted or moved, a chain cut short at the front - each is
// found, and the first wrong record is named.
func TestChainDetectsEveryKindOfTampering(t *testing.T) {
	t.Parallel()
	records := chain(6)
	if err := audit.Verify(audit.Record{}, records); err != nil {
		t.Fatalf("Verify of an untouched chain: %v", err)
	}
	if err := audit.Verify(records[2], records[3:]); err != nil {
		t.Fatalf("Verify of the tail after record 3: %v", err)
	}
	if err := audit.Verify(audit.Record{}, nil); err != nil {
		t.Errorf("Verify of nothing: %v", err)
	}
	for i, r := range records {
		if r.Sequence != uint64(i+1) || len(r.Hash) != 64 || (i > 0 && r.PreviousHash != records[i-1].Hash) || (i == 0 && r.PreviousHash != "") {
			t.Fatalf("record %d is not sealed onto the chain: %+v", i, r)
		}
	}

	tamper := map[string]func(r []audit.Record) []audit.Record{
		"actor changed":      func(r []audit.Record) []audit.Record { r[2].Actor = "user/mallory"; return r },
		"action changed":     func(r []audit.Record) []audit.Record { r[2].Action = "admin:GetUser"; return r },
		"resource changed":   func(r []audit.Record) []audit.Record { r[2].Resource = "arn:cactusfs:admin:::user/eve"; return r },
		"outcome changed":    func(r []audit.Record) []audit.Record { r[2].Outcome = audit.Denied; return r },
		"time changed":       func(r []audit.Record) []audit.Record { r[2].Time = r[2].Time.Add(time.Nanosecond); return r },
		"source changed":     func(r []audit.Record) []audit.Record { r[2].SourceIP = "10.0.0.2"; return r },
		"request ID changed": func(r []audit.Record) []audit.Record { r[2].RequestID = "R2"; return r },
		"key changed":        func(r []audit.Record) []audit.Record { r[2].AccessKeyID = "OTHER"; return r },
		"detail changed": func(r []audit.Record) []audit.Record {
			r[2].Details = map[string]string{"change": "create", "n": "xx"}
			return r
		},
		"detail added": func(r []audit.Record) []audit.Record {
			r[2].Details = map[string]string{"change": "update", "n": "xx", "x": ""}
			return r
		},
		"detail removed":                     func(r []audit.Record) []audit.Record { r[2].Details = map[string]string{"change": "update"}; return r },
		"record removed":                     func(r []audit.Record) []audit.Record { return append(r[:2], r[3:]...) },
		"last record removed and renumbered": func(r []audit.Record) []audit.Record { r[5].Sequence = 5; return append(r[:4], r[5]) },
		"records swapped":                    func(r []audit.Record) []audit.Record { r[2], r[3] = r[3], r[2]; return r },
		"record duplicated":                  func(r []audit.Record) []audit.Record { return append(r[:3], r[2:]...) },
		"record forged with a fresh hash": func(r []audit.Record) []audit.Record {
			forged := r[2]
			forged.Actor = "user/mallory"
			r[2] = forged.Seal(r[1])
			return r
		},
		"hash blanked": func(r []audit.Record) []audit.Record { r[2].Hash = ""; return r },
	}
	for name, change := range tamper {
		changed := change(chain(6))
		if err := audit.Verify(audit.Record{}, changed); !errors.Is(err, domain.ErrIntegrity) {
			t.Errorf("%s: Verify = %v, want %v", name, err, domain.ErrIntegrity)
		}
	}
	// A forged record with a fresh hash is caught by the one after it; a
	// forged LAST record needs the holder of the latest hash to notice.
	// That is what publishing the hash (the audit event) is for.
	if err := audit.Verify(records[0], records[2:]); !errors.Is(err, domain.ErrIntegrity) {
		t.Errorf("a run that skips a record: %v", err)
	}

	// Fields cannot be shifted into one another without changing the hash.
	a := audit.Record{Time: now, Actor: "ab", Action: "c", Outcome: audit.Succeeded}.Seal(audit.Record{})
	b := audit.Record{Time: now, Actor: "a", Action: "bc", Outcome: audit.Succeeded}.Seal(audit.Record{})
	c := audit.Record{Time: now, Actor: "ab", Action: "c", Outcome: audit.Succeeded, Details: map[string]string{"k": "v"}}.Seal(audit.Record{})
	d := audit.Record{Time: now, Actor: "ab", Action: "c", Outcome: audit.Succeeded, Details: map[string]string{"kv": ""}}.Seal(audit.Record{})
	if a.Hash == b.Hash || c.Hash == d.Hash || a.Hash == c.Hash {
		t.Error("different records have the same hash")
	}
	// The same record always has the same hash, whatever order a map is
	// walked in.
	for range 20 {
		again := audit.Record{Time: now, Actor: "ab", Action: "c", Outcome: audit.Succeeded, Details: map[string]string{"a": "1", "b": "2", "c": "3"}}
		first := again.Seal(audit.Record{})
		if first.Hash != again.Seal(audit.Record{}).Hash {
			t.Fatal("hash depends on map order")
		}
	}
}

func TestRecordValidation(t *testing.T) {
	t.Parallel()
	valid := audit.Record{Actor: "root", Action: "admin:PutUser", Outcome: audit.Succeeded}
	if err := valid.Validate(); err != nil {
		t.Errorf("valid record: %v", err)
	}
	many := map[string]string{}
	for i := range 17 {
		many[strings.Repeat("k", i+1)] = "v"
	}
	invalid := map[string]audit.Record{
		"no actor":         {Action: "a", Outcome: audit.Failed},
		"no action":        {Actor: "a", Outcome: audit.Failed},
		"unknown outcome":  {Actor: "a", Action: "a", Outcome: "maybe"},
		"oversized field":  {Actor: "a", Action: "a", Outcome: audit.Denied, Resource: strings.Repeat("r", 1025)},
		"oversized detail": {Actor: "a", Action: "a", Outcome: audit.Denied, Details: map[string]string{"k": strings.Repeat("v", 1025)}},
		"too many details": {Actor: "a", Action: "a", Outcome: audit.Denied, Details: many},
	}
	for name, record := range invalid {
		if err := record.Validate(); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: Validate = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
}
