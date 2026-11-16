package audit_test

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
)

// TestLineIsTheExportedFormat holds the bytes of a line: the fields in their
// order, the empty ones left out, the time as RFC 3339 with its fraction. It
// is what the node's own output has always written and what a target is
// sent as cactusfs.audit.exported.v1.
func TestLineIsTheExportedFormat(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		record audit.Record
		want   string
	}{
		"every field": {
			record: audit.Record{
				Sequence: 7, Time: time.Date(2026, 11, 16, 9, 30, 0, 123000000, time.UTC), Actor: "user/alice", AccessKeyID: "AKID",
				Action: "admin:PutUser", Resource: "arn:cactusfs:admin:::user/bob", Outcome: audit.Denied, SourceIP: "10.0.0.1",
				RequestID: "R1", Details: map[string]string{"b": "2", "a": "1"}, PreviousHash: "p", Hash: "h",
			},
			want: `{"stream":"audit","sequence":7,"time":"2026-11-16T09:30:00.123Z","actor":"user/alice","access_key_id":"AKID",` +
				`"action":"admin:PutUser","resource":"arn:cactusfs:admin:::user/bob","outcome":"denied","source_ip":"10.0.0.1",` +
				`"request_id":"R1","details":{"a":"1","b":"2"},"previous_hash":"p","hash":"h"}`,
		},
		"the first record, of the system": {
			record: audit.Record{
				Sequence: 1, Time: time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC), Actor: "system", Action: "cluster:Bootstrap",
				Outcome: audit.Succeeded, Hash: "h",
			},
			want: `{"stream":"audit","sequence":1,"time":"2026-11-16T09:30:00Z","actor":"system","action":"cluster:Bootstrap",` +
				`"outcome":"success","hash":"h"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.record.Line()
			if err != nil || string(got) != tt.want {
				t.Fatalf("Line() = %s, %v\nwant %s", got, err, tt.want)
			}
		})
	}
}

// TestLinesOfAChainVerify: every record of a chain, made a line and read
// back, is the record it was, and the chain of what was read verifies; a
// line that was changed on its way no longer does.
func TestLinesOfAChainVerify(t *testing.T) {
	t.Parallel()
	records := chain(20)
	// A record of a node whose clock is not in UTC hashes its time in UTC.
	records[3].Time = records[3].Time.In(time.FixedZone("SAST", 2*60*60))
	var read []audit.Record
	for _, record := range records {
		data, err := record.Line()
		if err != nil {
			t.Fatal(err)
		}
		back, err := audit.ParseLine(data)
		if err != nil {
			t.Fatalf("ParseLine(%s): %v", data, err)
		}
		if !back.Time.Equal(record.Time) {
			t.Fatalf("record %d read back at %v, was %v", record.Sequence, back.Time, record.Time)
		}
		back.Time, record.Time = back.Time.UTC(), record.Time.UTC()
		if !reflect.DeepEqual(back, record) {
			t.Fatalf("record %d read back as %+v, was %+v", record.Sequence, back, record)
		}
		read = append(read, back)
	}
	if err := audit.Verify(audit.Record{}, read); err != nil {
		t.Fatalf("the chain of the lines read back: %v", err)
	}

	changed, _ := records[5].Line()
	changed = bytes.Replace(changed, []byte(`"user/alice"`), []byte(`"user/mallory"`), 1)
	back, err := audit.ParseLine(changed)
	if err != nil {
		t.Fatal(err)
	}
	read[5] = back
	if err := audit.Verify(audit.Record{}, read); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("a changed line verifies: %v", err)
	}
}

// TestParseLineRefusesWhatIsNoLine: a receiver is told when what it holds is
// not a line of the trail.
func TestParseLineRefusesWhatIsNoLine(t *testing.T) {
	t.Parallel()
	good := `{"stream":"audit","sequence":1,"time":"2026-11-16T09:30:00Z","actor":"system","action":"a","outcome":"success","hash":"h"}`
	if _, err := audit.ParseLine([]byte(good)); err != nil {
		t.Fatalf("a good line: %v", err)
	}
	for name, data := range map[string]string{
		"empty":              ``,
		"no JSON":            `audit 1`,
		"an array":           `[]`,
		"another stream":     strings.Replace(good, `"stream":"audit"`, `"stream":"access"`, 1),
		"no sequence":        strings.Replace(good, `"sequence":1`, `"sequence":0`, 1),
		"a negative one":     strings.Replace(good, `"sequence":1`, `"sequence":-1`, 1),
		"another outcome":    strings.Replace(good, `"success"`, `"maybe"`, 1),
		"no hash":            strings.Replace(good, `,"hash":"h"`, ``, 1),
		"a field unknown":    strings.Replace(good, `"hash":"h"`, `"hash":"h","secret":"s"`, 1),
		"two lines in one":   good + good,
		"a bad time":         strings.Replace(good, `2026-11-16T09:30:00Z`, `yesterday`, 1),
		"no time":            strings.Replace(good, `"time":"2026-11-16T09:30:00Z",`, ``, 1),
		"the zero time":      strings.Replace(good, `2026-11-16T09:30:00Z`, `0001-01-01T00:00:00Z`, 1),
		"no actor":           strings.Replace(good, `"actor":"system",`, ``, 1),
		"an empty actor":     strings.Replace(good, `"actor":"system"`, `"actor":""`, 1),
		"no action":          strings.Replace(good, `"action":"a",`, ``, 1),
		"an empty action":    strings.Replace(good, `"action":"a"`, `"action":""`, 1),
		"a name twice":       strings.Replace(good, `"actor":"system"`, `"actor":"system","actor":"root"`, 1),
		"a hash twice":       strings.Replace(good, `"hash":"h"`, `"hash":"h","hash":"g"`, 1),
		"a detail twice":     strings.Replace(good, `"hash":"h"`, `"details":{"k":"1","k":"2"},"hash":"h"`, 1),
		"details no object":  strings.Replace(good, `"hash":"h"`, `"details":["k"],"hash":"h"`, 1),
		"a detail no string": strings.Replace(good, `"hash":"h"`, `"details":{"k":1},"hash":"h"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := audit.ParseLine([]byte(data)); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("ParseLine(%q) = %v", data, err)
			}
		})
	}
}

// FuzzParseLine: whatever arrives, ParseLine answers without panicking, and
// what it reads is written back as a line that reads back the same.
func FuzzParseLine(f *testing.F) {
	for _, record := range chain(3) {
		data, err := record.Line()
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add([]byte(`{"stream":"audit","sequence":1,"time":"2026-11-16T09:30:00Z","actor":"","action":"","outcome":"failed","hash":"x"}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		record, err := audit.ParseLine(data)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("ParseLine failed with %v", err)
			}
			return
		}
		again, err := record.Line()
		if err != nil {
			t.Fatalf("a record read is not written: %v", err)
		}
		back, err := audit.ParseLine(again)
		if err != nil {
			t.Fatalf("a line written is not read: %s: %v", again, err)
		}
		if !back.Time.Equal(record.Time) {
			t.Fatalf("time %v read back as %v", record.Time, back.Time)
		}
		back.Time, record.Time = time.Time{}, time.Time{}
		if !reflect.DeepEqual(back, record) {
			t.Fatalf("%+v read back as %+v", record, back)
		}
	})
}
