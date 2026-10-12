package bucket_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// everyClass is text with a character of every class the canonical form
// treats in a way of its own (ADR-0023): the control characters, with those
// JSON has a short form for; a space; U+007F; the three characters a writer
// of JSON for HTML escapes; the quotation mark and the backslash; a slash;
// the two line separators; a character of two bytes and one beyond the basic
// plane.
const everyClass = "\x01\b\f\n\r\t\x1f \x7f<>&\"\\/\u2028\u2029\u00e9\U0001d11e"

// everyClassWritten is everyClass between the quotation marks of a canonical
// document, written out by hand.
const everyClassWritten = `"\u0001\u0008\u000c\u000a\u000d\u0009\u001f ` + "\x7f" + `<>&\"\\/` + "\u2028\u2029\u00e9\U0001d11e" + `"`

// asLongAsAKey is 1 024 bytes: as long as a key, a prefix or a suffix can
// be.
func asLongAsAKey() string {
	return strings.Repeat("long/", 204) + "1024"
}

// asLongAsAnID is 255 bytes: as long as the ID of a rule can be.
func asLongAsAnID() string {
	return "id-" + strings.Repeat("i", 252)
}

// TestCanonicalDocumentsOfEveryClassOfText pins, for the three things that
// are stored in the canonical form, the bytes of text of every class - in
// the cursor of a usage record and in the ID, the prefix and the suffix of a
// rule - and of values as long as they can be. The golden files were written
// from the rule of ADR-0023, not by this code.
func TestCanonicalDocumentsOfEveryClassOfText(t *testing.T) {
	t.Parallel()
	if len(asLongAsAKey()) != object.MaxKeyLength || len(asLongAsAnID()) != 255 {
		t.Fatal("the long values are not as long as they are said to be")
	}
	text := bucket.Usage{High: 5, Count: &bucket.UsageCount{Phase: bucket.UsagePhaseKeys, Cursor: everyClass}}
	bounds := bucket.Usage{
		Exact: true, UsageTotals: bucket.UsageTotals{Objects: 1<<63 - 1}, High: 1<<64 - 1,
		Count: &bucket.UsageCount{Phase: bucket.UsagePhaseUploads, Cursor: asLongAsAKey()},
	}
	for name, usage := range map[string]bucket.Usage{"usage-record-text.golden.json": text, "usage-record-bounds.golden.json": bounds} {
		record, err := usage.Record()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		golden(t, name, record)
		back, err := bucket.ParseUsage(record)
		if err != nil || !reflect.DeepEqual(back, usage) {
			t.Errorf("%s: ParseUsage = %+v, %v", name, back, err)
		}
		readByAnyone(t, name, record)
	}
	if record, err := text.Record(); err != nil || !bytes.Contains(record, []byte(`"cursor":`+everyClassWritten+`,`)) {
		t.Errorf("the cursor is not written as the rule says: %s, %v", record, err)
	}

	notification := bucket.NotificationConfig{Rules: []bucket.NotificationRule{
		{ID: everyClass, As: bucket.NotifyAsQueue, Target: "hook", On: []delivery.Event{delivery.EventPut}, Prefix: everyClass, Suffix: everyClass},
		{
			ID: asLongAsAnID(), As: bucket.NotifyAsTopic, Target: "hook",
			On:     []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventMarker, delivery.EventDelete},
			Prefix: asLongAsAKey(), Suffix: asLongAsAKey(),
		},
	}}
	document, err := notification.Document()
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "notification-text.golden.json", document)
	if back, err := bucket.ParseNotificationConfig(document); err != nil || !reflect.DeepEqual(back, notification) {
		t.Errorf("ParseNotificationConfig = %+v, %v", back, err)
	}
	readByAnyone(t, "notification-text.golden.json", document)
	for _, field := range []string{`"id":`, `"prefix":`, `"suffix":`} {
		if !bytes.Contains(document, []byte(field+everyClassWritten)) {
			t.Errorf("%s is not written as the rule says: %s", field, document)
		}
	}
	// A rule matches by the bytes of its prefix and suffix, whatever they
	// are.
	if got := notification.Match(delivery.EventPut, everyClass+"/key/"+everyClass); len(got) != 1 {
		t.Errorf("Match with text of every class = %v", got)
	}

	replication := bucket.ReplicationConfig{Target: "site-b", Bucket: "photos-copy", Rules: []bucket.ReplicationRule{
		{ID: everyClass, Enabled: true, Priority: 1<<31 - 1, Prefix: everyClass, Markers: true},
		{ID: asLongAsAnID(), Prefix: asLongAsAKey()},
	}}
	document, err = replication.Document()
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "replication-text.golden.json", document)
	if back, err := bucket.ParseReplicationConfig(document); err != nil || !reflect.DeepEqual(back, replication) {
		t.Errorf("ParseReplicationConfig = %+v, %v", back, err)
	}
	readByAnyone(t, "replication-text.golden.json", document)
	for _, field := range []string{`"id":`, `"prefix":`} {
		if !bytes.Contains(document, []byte(field+everyClassWritten)) {
			t.Errorf("%s is not written as the rule says: %s", field, document)
		}
	}
}

// readByAnyone checks that a canonical document is JSON to a reader that
// knows nothing of the canonical form, and holds no byte below a space.
func readByAnyone(t *testing.T, name string, document []byte) {
	t.Helper()
	var anything map[string]any
	if err := json.Unmarshal(document, &anything); err != nil {
		t.Errorf("%s is not JSON: %v", name, err)
	}
	if bytes.ContainsFunc(document, func(r rune) bool { return r < 0x20 }) || !utf8.Valid(document) {
		t.Errorf("%s holds a control character, or is not UTF-8", name)
	}
}

// TestCanonicalDocumentsHaveOneForm: the same record and the same rules,
// with their text escaped as another writer of JSON would escape it, are
// JSON of the same meaning - and are refused, each of them. What a member
// reads is what every member wrote, byte for byte, whatever release of Go
// built it.
func TestCanonicalDocumentsHaveOneForm(t *testing.T) {
	t.Parallel()
	usage := string(goldenFile(t, "usage-record-text.golden.json"))
	notification := string(goldenFile(t, "notification-text.golden.json"))
	replication := string(goldenFile(t, "replication-text.golden.json"))
	parsers := map[string]func([]byte) error{
		usage:        func(b []byte) error { _, err := bucket.ParseUsage(b); return err },
		notification: func(b []byte) error { _, err := bucket.ParseNotificationConfig(b); return err },
		replication:  func(b []byte) error { _, err := bucket.ParseReplicationConfig(b); return err },
	}
	changes := []struct{ name, from, to string }{
		{"the short form of a line feed", `\u000a`, `\n`},
		{"the short form of a tab", `\u0009`, `\t`},
		{"the short form of a backspace", `\u0008`, `\b`},
		{"the short form of a form feed", `\u000c`, `\f`},
		{"the short form of a carriage return", `\u000d`, `\r`},
		{"hexadecimal in capitals", `\u000c`, `\u000C`},
		{"an angle bracket escaped, as for HTML", `<`, `\u003c`},
		{"an ampersand escaped, as for HTML", `&`, `\u0026`},
		{"a slash escaped", `/`, `\/`},
		{"U+007F escaped", "\x7f", `\u007f`},
		{"a line separator escaped, as for JavaScript", "\u2028", `\u2028`},
		{"a character beyond the basic plane as a surrogate pair", "\U0001d11e", `\ud834\udd1e`},
		{"a character of two bytes escaped", "\u00e9", `\u00e9`},
		{"a control character as it is", `\u0001`, "\x01"},
		{"a space escaped", `\u001f `, `\u001f\u0020`},
	}
	for document, parse := range parsers {
		if err := parse([]byte(document)); err != nil {
			t.Fatalf("the canonical document: %v", err)
		}
		for _, change := range changes {
			changed := strings.Replace(document, change.from, change.to, 1)
			if changed == document {
				t.Fatalf("%s: the document holds nothing to change", change.name)
			}
			// It is still JSON, and - but for the raw control character,
			// which JSON forbids - of the same meaning.
			var before, after any
			errBefore, errAfter := json.Unmarshal([]byte(document), &before), json.Unmarshal([]byte(changed), &after)
			if change.name != "a control character as it is" && (errBefore != nil || errAfter != nil || !reflect.DeepEqual(before, after)) {
				t.Fatalf("%s: the change is no other writing of the same document: %v, %v", change.name, errBefore, errAfter)
			}
			if err := parse([]byte(changed)); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("%s: read with %v, want %v", change.name, err, domain.ErrInvalidArgument)
			}
		}
	}
	// Numbers have one form too.
	const record = `{"v":1,"exact":true,"objects":7,"current_bytes":0,"versions":0,"stored_bytes":0,"upload_bytes":0,"replication_pending":0,"replication_failed":0,"high":0}`
	if _, err := bucket.ParseUsage([]byte(record)); err != nil {
		t.Fatal(err)
	}
	for name, number := range map[string]string{
		"a zero first": "07", "an exponent": "7e0", "an exponent in capitals": "7E0", "a point": "7.0", "a plus sign": "+7", "a minus sign": "-7",
		"minus nothing": "-0", "beyond every count": "9223372036854775808", "hexadecimal": "0x7",
	} {
		if _, err := bucket.ParseUsage([]byte(strings.Replace(record, `"objects":7`, `"objects":`+number, 1))); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("a count written with %s: %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	for name, changed := range map[string]string{
		"a sequence beyond the last":  strings.Replace(record, `"high":0`, `"high":18446744073709551616`, 1),
		"a sequence below nothing":    strings.Replace(record, `"high":0`, `"high":-1`, 1),
		"a version with a zero first": strings.Replace(record, `"v":1`, `"v":01`, 1),
		"a version below nothing":     strings.Replace(record, `"v":1`, `"v":-1`, 1),
		"true in capitals":            strings.Replace(record, `true`, `TRUE`, 1),
		"true as a number":            strings.Replace(record, `true`, `1`, 1),
		"a phase with a zero first":   strings.Replace(record, `"high":0`, `"high":0,"count":{"phase":01,"cursor":"","objects":0,"current_bytes":0,"versions":0,"stored_bytes":0,"upload_bytes":0,"replication_pending":0,"replication_failed":0}`, 1),
		"a line break at the end":     record + "\n",
		"a byte order mark":           "\xef\xbb\xbf" + record,
	} {
		if _, err := bucket.ParseUsage([]byte(changed)); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	const rules = `{"v":1,"target":"site-b","bucket":"photos-copy","rules":[{"id":"r","priority":5}]}`
	if _, err := bucket.ParseReplicationConfig([]byte(rules)); err != nil {
		t.Fatal(err)
	}
	for name, number := range map[string]string{
		"a zero first": "05", "an exponent": "5e0", "a point": "5.0", "a minus sign": "-5", "minus nothing": "-0", "beyond every priority": "2147483648",
	} {
		if _, err := bucket.ParseReplicationConfig([]byte(strings.Replace(rules, `"priority":5`, `"priority":`+number, 1))); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("a priority written with %s: %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
}

// FuzzCanonicalDocumentText is the round trip from the other side: whatever
// text a record and the two documents are made with, they are written
// exactly when their text is valid UTF-8 within its bounds, what is written
// is JSON that any reader reads the same text from, and reading it gives
// what was written from.
func FuzzCanonicalDocumentText(f *testing.F) {
	f.Add("r", "logs/", ".gz", uint8(1))
	f.Add(everyClass, everyClass, everyClass, uint8(2))
	f.Add(asLongAsAnID(), asLongAsAKey(), asLongAsAKey(), uint8(3))
	f.Add("\xff", "a\xc3", "", uint8(0))
	f.Add("", "", "", uint8(9))
	f.Fuzz(func(t *testing.T, id, prefix, suffix string, phase uint8) {
		valid := func(s string, limit int) bool { return len(s) <= limit && utf8.ValidString(s) }

		usage := bucket.Usage{Exact: true, High: uint64(phase), Count: &bucket.UsageCount{Phase: bucket.UsagePhase(phase%2 + 1), Cursor: prefix}}
		record, err := usage.Record()
		if (err == nil) != valid(prefix, object.MaxKeyLength) {
			t.Fatalf("Record with the cursor %q: %v", prefix, err)
		}
		if err == nil {
			back, err := bucket.ParseUsage(record)
			if err != nil || !reflect.DeepEqual(back, usage) {
				t.Fatalf("%s is read as %+v, %v", record, back, err)
			}
			var read struct {
				Count struct {
					Cursor string `json:"cursor"`
				} `json:"count"`
			}
			if err := json.Unmarshal(record, &read); err != nil || read.Count.Cursor != prefix {
				t.Fatalf("a reader of JSON reads the cursor of %s as %q, %v", record, read.Count.Cursor, err)
			}
		}

		notification := bucket.NotificationConfig{Rules: []bucket.NotificationRule{
			{ID: id, As: bucket.NotifyAsQueue, Target: "hook", On: []delivery.Event{delivery.EventPut}, Prefix: prefix, Suffix: suffix},
		}}
		document, err := notification.Document()
		if (err == nil) != (id != "" && valid(id, 255) && valid(prefix, 1024) && valid(suffix, 1024)) {
			t.Fatalf("Document of the rule %q, %q, %q: %v", id, prefix, suffix, err)
		}
		if err == nil {
			back, err := bucket.ParseNotificationConfig(document)
			if err != nil || !reflect.DeepEqual(back, notification) {
				t.Fatalf("%s is read as %+v, %v", document, back, err)
			}
			var read struct {
				Rules []struct {
					ID     string `json:"id"`
					Prefix string `json:"prefix"`
					Suffix string `json:"suffix"`
				} `json:"rules"`
			}
			if err := json.Unmarshal(document, &read); err != nil || len(read.Rules) != 1 || read.Rules[0].ID != id || read.Rules[0].Prefix != prefix || read.Rules[0].Suffix != suffix {
				t.Fatalf("a reader of JSON reads %s as %+v, %v", document, read, err)
			}
		}

		replication := bucket.ReplicationConfig{Target: "site-b", Bucket: "photos-copy", Rules: []bucket.ReplicationRule{
			{ID: id, Enabled: phase%2 == 0, Priority: int(phase), Prefix: prefix, Markers: phase%3 == 0},
		}}
		document, err = replication.Document()
		if (err == nil) != (id != "" && valid(id, 255) && valid(prefix, 1024)) {
			t.Fatalf("Document of the rule %q, %q: %v", id, prefix, err)
		}
		if err == nil {
			back, err := bucket.ParseReplicationConfig(document)
			if err != nil || !reflect.DeepEqual(back, replication) {
				t.Fatalf("%s is read as %+v, %v", document, back, err)
			}
			var read struct {
				Rules []struct {
					ID     string `json:"id"`
					Prefix string `json:"prefix"`
				} `json:"rules"`
			}
			if err := json.Unmarshal(document, &read); err != nil || len(read.Rules) != 1 || read.Rules[0].ID != id || read.Rules[0].Prefix != prefix {
				t.Fatalf("a reader of JSON reads %s as %+v, %v", document, read, err)
			}
		}
	})
}
