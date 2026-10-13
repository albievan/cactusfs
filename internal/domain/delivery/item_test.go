package delivery_test

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
)

func TestSeq(t *testing.T) {
	t.Parallel()
	ordered := []delivery.Seq{
		{},
		{Index: 0, Row: 1},
		{Index: 1, Row: 0},
		{Index: 1, Row: 1},
		{Index: 1, Row: 65535},
		{Index: 2, Row: 0},
		{Index: 1234, Row: 0},
		{Index: 1 << 40, Row: 7},
		{Index: ^uint64(0), Row: 65535},
	}
	for i, a := range ordered {
		key := a.Key()
		if back, err := delivery.SeqFromKey(key[:]); err != nil || back != a {
			t.Errorf("SeqFromKey(Key(%+v)) = %+v, %v", a, back, err)
		}
		if back, err := delivery.ParseSeq(a.String()); err != nil || back != a || len(a.String()) != 20 {
			t.Errorf("ParseSeq(%q) = %+v, %v", a.String(), back, err)
		}
		if a.IsZero() != (i == 0) {
			t.Errorf("%+v is zero = %v", a, a.IsZero())
		}
		for j, b := range ordered {
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			keyB := b.Key()
			if a.Compare(b) != want || a.After(b) != (want > 0) {
				t.Errorf("Compare(%+v, %+v) = %d, want %d", a, b, a.Compare(b), want)
			}
			// Keys and texts sort as places do.
			if bytes.Compare(key[:], keyB[:]) != want || strings.Compare(a.String(), b.String()) != want {
				t.Errorf("the key or the text of %+v and %+v sorts otherwise", a, b)
			}
		}
	}
	// The sequencer of the ADR: entry 1234, row 0.
	if got := (delivery.Seq{Index: 1234}).String(); got != "00000000000004d20000" {
		t.Errorf("String() = %q", got)
	}
	for _, text := range []string{"", "00", "00000000000004d2000", "00000000000004d200000", "00000000000004D20000", "0000000000000 d20000", "zz000000000004d20000"} {
		if _, err := delivery.ParseSeq(text); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ParseSeq(%q) = %v", text, err)
		}
	}
	for _, key := range [][]byte{nil, make([]byte, 9), make([]byte, 11)} {
		if _, err := delivery.SeqFromKey(key); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("SeqFromKey of %d bytes = %v", len(key), err)
		}
	}
}

func FuzzParseSeq(f *testing.F) {
	f.Add("00000000000004d20000")
	f.Add("ffffffffffffffffffff")
	f.Add("00000000000004D20000")
	f.Add("")
	f.Fuzz(func(t *testing.T, text string) {
		seq, err := delivery.ParseSeq(text)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("ParseSeq: %v", err)
			}
			return
		}
		if seq.String() != text {
			t.Fatalf("ParseSeq(%q) = %+v, which is written %q", text, seq, seq.String())
		}
	})
}

func sampleItem() delivery.Item {
	return delivery.Item{
		Seq: delivery.Seq{Index: 1234, Row: 1}, Event: delivery.EventPut, Bucket: "photos", Key: "2026/a+b.jpg",
		Version: object.VersionRef{ID: object.ID{1, 2, 3}}, Size: 1024, ETag: "9b2c", At: time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC),
		By: "user/alice",
		To: []delivery.Consumer{{Kind: delivery.ConsumerNotification, Target: "hook"}, {Kind: delivery.ConsumerReplication, Target: "site-b"}},
	}
}

func TestItem(t *testing.T) {
	t.Parallel()
	item := sampleItem()
	if err := item.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if item.ID() != "00000000000004d20001" {
		t.Errorf("ID() = %q", item.ID())
	}
	if kind, ok := item.For("hook"); !ok || kind != delivery.ConsumerNotification {
		t.Errorf("For(hook) = %q, %v", kind, ok)
	}
	if kind, ok := item.For("site-b"); !ok || kind != delivery.ConsumerReplication {
		t.Errorf("For(site-b) = %q, %v", kind, ok)
	}
	if _, ok := item.For("other"); ok {
		t.Error("an item is for a target it does not name")
	}
	clone := item.Clone()
	clone.To[0].Target = "changed"
	if item.To[0].Target != "hook" {
		t.Error("a copy of an item shares with it")
	}
	null := item.Clone()
	null.Version = object.VersionRef{Null: true}
	marker := item.Clone()
	marker.Event, marker.Size, marker.ETag, marker.By = delivery.EventMarker, 0, "", ""
	longest := item.Clone()
	longest.ETag = strings.Repeat("e", delivery.MaxItemETag)
	for name, valid := range map[string]delivery.Item{"the null version": null, "a marker by nobody": marker, "the longest entity tag": longest} {
		if err := valid.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	change := func(edit func(*delivery.Item)) delivery.Item {
		out := item.Clone()
		edit(&out)
		return out
	}
	for name, bad := range map[string]delivery.Item{
		"no kind of change":       change(func(i *delivery.Item) { i.Event = "" }),
		"a kind there is not":     change(func(i *delivery.Item) { i.Event = "copy" }),
		"no bucket":               change(func(i *delivery.Item) { i.Bucket = "" }),
		"no key":                  change(func(i *delivery.Item) { i.Key = "" }),
		"a key that is none":      change(func(i *delivery.Item) { i.Key = "\xff" }),
		"no version":              change(func(i *delivery.Item) { i.Version = object.VersionRef{} }),
		"a size below nothing":    change(func(i *delivery.Item) { i.Size = -1 }),
		"an entity tag too long":  change(func(i *delivery.Item) { i.ETag = strings.Repeat("e", delivery.MaxItemETag+1) }),
		"an actor without end":    change(func(i *delivery.Item) { i.By = strings.Repeat("a", 257) }),
		"for nobody":              change(func(i *delivery.Item) { i.To = nil }),
		"a consumer of no kind":   change(func(i *delivery.Item) { i.To[0].Kind = "x" }),
		"a consumer of no target": change(func(i *delivery.Item) { i.To[0].Target = "" }),
		"a target twice":          change(func(i *delivery.Item) { i.To[1].Target = "hook" }),
	} {
		if err := bad.Validate(); !errors.Is(err, domain.ErrInvalidArgument) || strings.Contains(err.Error(), "a+b") {
			t.Errorf("%s: Validate = %v", name, err)
		}
	}
}

func TestCursor(t *testing.T) {
	t.Parallel()
	at := func(index uint64) delivery.Seq { return delivery.Seq{Index: index} }
	cursor := delivery.Cursor{Target: "hook", From: at(10), Acked: at(10)}
	if cursor.Pending(at(10)) || cursor.Pending(at(9)) || !cursor.Pending(delivery.Seq{Index: 10, Row: 1}) || !cursor.Pending(at(11)) {
		t.Error("Pending")
	}
	steps := []struct {
		upto      delivery.Seq
		auditUpto uint64
		acked     delivery.Seq
		audit     uint64
	}{
		{at(12), 0, at(12), 0},
		{at(11), 5, at(12), 5}, // an acknowledgement that arrives late moves nothing back
		{at(12), 3, at(12), 5},
		{delivery.Seq{Index: 12, Row: 2}, 5, delivery.Seq{Index: 12, Row: 2}, 5},
		{delivery.Seq{}, 0, delivery.Seq{Index: 12, Row: 2}, 5},
		{at(40), 41, at(40), 41},
	}
	for i, step := range steps {
		cursor = cursor.Advanced(step.upto, step.auditUpto)
		if cursor.Acked != step.acked || cursor.AuditAcked != step.audit || cursor.From != at(10) {
			t.Fatalf("step %d: %+v", i, cursor)
		}
	}

	// What is dropped at the bound is lost to exactly who had not passed it
	// and was to get it.
	item := sampleItem() // at entry 1234, for n:hook and r:site-b
	behind := delivery.Cursor{Target: "hook", Acked: at(1000)}
	past := delivery.Cursor{Target: "hook", Acked: at(2000)}
	exactly := delivery.Cursor{Target: "hook", Acked: item.Seq}
	other := delivery.Cursor{Target: "other", Acked: at(1000)}
	replica := delivery.Cursor{Target: "site-b", Acked: at(1000)}
	for name, tt := range map[string]struct {
		cursor delivery.Cursor
		lost   uint64
		resync bool
	}{
		"had not passed it": {behind, 1, false}, "had passed it": {past, 0, false}, "was at it": {exactly, 0, false},
		"is no consumer of it": {other, 0, false}, "replication had not passed it": {replica, 1, true},
	} {
		got := tt.cursor.Dropped(item)
		if got.Lost != tt.lost || got.ResyncNeeded != tt.resync || got.Acked != tt.cursor.Acked {
			t.Errorf("%s: %+v", name, got)
		}
	}
	twice := replica.Dropped(item).Dropped(item)
	if twice.Lost != 2 || !twice.ResyncNeeded {
		t.Errorf("two rows dropped: %+v", twice)
	}
}

func TestResult(t *testing.T) {
	t.Parallel()
	version := object.VersionRef{ID: object.ID{9}}
	valid := []delivery.Result{
		{Seq: delivery.Seq{Index: 5}, Status: object.ReplicationCompleted},
		{Seq: delivery.Seq{Row: 1}, Status: object.ReplicationFailed},
		{Bucket: "photos", Key: "a", Version: version, Status: object.ReplicationCompleted},
		{Bucket: "photos", Key: "a", Version: object.VersionRef{Null: true}, Status: object.ReplicationFailed},
	}
	for i, r := range valid {
		if err := r.Validate(); err != nil || r.ByRow() != (i < 2) {
			t.Errorf("%+v: %v, by row = %v", r, err, r.ByRow())
		}
	}
	for name, r := range map[string]delivery.Result{
		"nothing":                 {},
		"no status":               {Seq: delivery.Seq{Index: 5}},
		"pending":                 {Seq: delivery.Seq{Index: 5}, Status: object.ReplicationPending},
		"replica":                 {Seq: delivery.Seq{Index: 5}, Status: object.ReplicationReplica},
		"a row and a version":     {Seq: delivery.Seq{Index: 5}, Bucket: "photos", Key: "a", Version: version, Status: object.ReplicationCompleted},
		"a row and a bucket":      {Seq: delivery.Seq{Index: 5}, Bucket: "photos", Status: object.ReplicationCompleted},
		"a version of no bucket":  {Key: "a", Version: version, Status: object.ReplicationCompleted},
		"a version of no key":     {Bucket: "photos", Version: version, Status: object.ReplicationCompleted},
		"a key and no version":    {Bucket: "photos", Key: "a", Status: object.ReplicationCompleted},
		"neither row nor version": {Status: object.ReplicationCompleted},
	} {
		if err := r.Validate(); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: Validate = %v", name, err)
		}
	}
}

// TestCursorsNeverGoBack: whatever acknowledgements arrive, in whatever
// order, a cursor ends at the greatest of them and is never behind where it
// was.
func TestCursorsNeverGoBack(t *testing.T) {
	t.Parallel()
	acks := []delivery.Seq{{Index: 5}, {Index: 3, Row: 9}, {Index: 5, Row: 1}, {Index: 4}, {Index: 9}, {Index: 1}}
	greatest := slices.MaxFunc(acks, delivery.Seq.Compare)
	for shift := range acks {
		order := slices.Concat(acks[shift:], acks[:shift])
		cursor := delivery.Cursor{Target: "hook"}
		for _, ack := range order {
			next := cursor.Advanced(ack, ack.Index)
			if cursor.Acked.After(next.Acked) || next.AuditAcked < cursor.AuditAcked {
				t.Fatalf("the cursor went back: %+v, then %+v", cursor, next)
			}
			cursor = next
		}
		if cursor.Acked != greatest || cursor.AuditAcked != 9 {
			t.Errorf("order %v: %+v", order, cursor)
		}
	}
}
