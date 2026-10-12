package object_test

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"testing"
	"testing/quick"
	"time"

	"github.com/albievan/cactusfs/internal/domain/object"
)

var accessDay = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func TestAccessAdd(t *testing.T) {
	t.Parallel()
	var a object.Access
	if !a.Empty() {
		t.Error("the zero record is not empty")
	}
	a = a.Add([]object.Read{
		{Who: "user/bob", Downloads: 2},
		{Who: "user/alice", Downloads: 1, Copies: 1},
		{Who: "user/alice", Presigned: true, Downloads: 3},
		{Who: "anonymous", Downloads: 1},
	}, accessDay)
	a = a.Add([]object.Read{{Who: "user/bob", Copies: 4}}, accessDay.Add(time.Hour))

	if a.Empty() || a.Downloads != 7 || a.Copies != 5 || !a.LastAt.Equal(accessDay.Add(time.Hour)) {
		t.Errorf("totals = %d downloads, %d copies, last %s", a.Downloads, a.Copies, a.LastAt)
	}
	want := []object.Reader{
		{Who: "anonymous", Downloads: 1, LastAt: accessDay},
		{Who: "user/alice", Downloads: 1, Copies: 1, LastAt: accessDay},
		{Who: "user/alice", Presigned: true, Downloads: 3, LastAt: accessDay},
		{Who: "user/bob", Downloads: 2, Copies: 4, LastAt: accessDay.Add(time.Hour)},
	}
	if !reflect.DeepEqual(a.Readers, want) {
		t.Errorf("readers = %+v\nwant      %+v", a.Readers, want)
	}
	// Adding does not change the record it was added to.
	before := slices.Clone(a.Readers)
	_ = a.Add([]object.Read{{Who: "user/bob", Downloads: 100}, {Who: "user/aaa", Downloads: 1}}, accessDay.Add(2*time.Hour))
	if !reflect.DeepEqual(a.Readers, before) {
		t.Error("Add changed the record it was called on")
	}
	// An earlier time does not move the last time back.
	if late := a.Add([]object.Read{{Who: "user/bob", Downloads: 1}}, accessDay.Add(-time.Hour)); !late.LastAt.Equal(a.LastAt) {
		t.Errorf("an earlier read moved the last time to %s", late.LastAt)
	}
}

func TestAccessLeavesOutWhatCannotBeRecorded(t *testing.T) {
	t.Parallel()
	for name, read := range map[string]object.Read{
		"nobody":           {Downloads: 1},
		"a name too long":  {Who: string(make([]byte, 257)), Downloads: 1},
		"a name of lines":  {Who: "user/a\nuser/b", Downloads: 1},
		"nothing read":     {Who: "user/a"},
		"a negative count": {Who: "user/a", Downloads: -1, Copies: 5},
		"another negative": {Who: "user/a", Downloads: 5, Copies: -1},
		// Two counts whose sum is no number: neither is one a node counts.
		"counts that wrap around": {Who: "user/a", Downloads: math.MaxInt64, Copies: math.MaxInt64},
		"too many downloads":      {Who: "user/a", Downloads: object.MaxReadCount + 1},
		"too many copies":         {Who: "user/a", Downloads: 1, Copies: object.MaxReadCount + 1},
	} {
		if read.Validate() == nil {
			t.Errorf("%s: Validate accepts %+v", name, read)
		}
		if got := (object.Access{}).Add([]object.Read{read}, accessDay); !got.Empty() || len(got.Readers) != 0 {
			t.Errorf("%s: recorded as %+v", name, got)
		}
	}
}

// TestAccessFoldsTheOldestReader: beyond the bound the reader who read
// longest ago is counted among the others, and nothing is lost of the totals.
func TestAccessFoldsTheOldestReader(t *testing.T) {
	t.Parallel()
	var a object.Access
	for i := range object.MaxReaders {
		a = a.Add([]object.Read{{Who: fmt.Sprintf("user/u%02d", i), Downloads: 1}}, accessDay.Add(time.Duration(i)*time.Minute))
	}
	if len(a.Readers) != object.MaxReaders || a.OtherReaders != 0 {
		t.Fatalf("%d readers, %d others", len(a.Readers), a.OtherReaders)
	}
	// u00 reads again: no longer the oldest.
	a = a.Add([]object.Read{{Who: "user/u00", Downloads: 1}}, accessDay.Add(time.Hour))
	a = a.Add([]object.Read{{Who: "user/new", Downloads: 5, Copies: 2}}, accessDay.Add(2*time.Hour))
	names := make([]string, len(a.Readers))
	for i, r := range a.Readers {
		names[i] = r.Who
	}
	if len(a.Readers) != object.MaxReaders || slices.Contains(names, "user/u01") || !slices.Contains(names, "user/u00") || !slices.Contains(names, "user/new") {
		t.Errorf("readers after the fold: %v", names)
	}
	if a.OtherReaders != 1 || a.OtherDownloads != 1 || a.OtherCopies != 0 {
		t.Errorf("others = %d readers, %d downloads, %d copies", a.OtherReaders, a.OtherDownloads, a.OtherCopies)
	}
	if !slices.IsSortedFunc(a.Readers, func(x, y object.Reader) int { return cmpReader(x, y) }) {
		t.Errorf("readers are not in order: %v", names)
	}
}

func cmpReader(x, y object.Reader) int {
	switch {
	case x.Who < y.Who:
		return -1
	case x.Who > y.Who:
		return 1
	case x.Presigned == y.Presigned:
		return 0
	case y.Presigned:
		return -1
	}
	return 1
}

// TestAccessProperties: whatever is added, in whatever batches, the totals
// are the sum of what was added, the readers kept by name and the others
// account for all of it, the bound holds, and the same reads give the same
// record.
func TestAccessProperties(t *testing.T) {
	t.Parallel()
	property := func(raw []uint16, split uint8) bool {
		var reads []object.Read
		for _, r := range raw {
			reads = append(reads, object.Read{
				Who: fmt.Sprintf("user/u%d", r%50), Presigned: r%7 == 0, Downloads: int64(r % 5), Copies: int64(r % 3),
			})
		}
		var downloads, copies int64
		for _, r := range reads {
			if r.Validate() == nil {
				downloads += r.Downloads
				copies += r.Copies
			}
		}
		run := func() object.Access {
			var a object.Access
			at := accessDay
			for start := 0; start < len(reads); {
				end := min(len(reads), start+1+int(split%5))
				a = a.Add(reads[start:end], at)
				at = at.Add(time.Second)
				start = end
			}
			return a
		}
		a := run()
		var named, namedCopies int64
		for _, r := range a.Readers {
			named += r.Downloads
			namedCopies += r.Copies
		}
		return a.Downloads == downloads && a.Copies == copies &&
			named+a.OtherDownloads == downloads && namedCopies+a.OtherCopies == copies &&
			len(a.Readers) <= object.MaxReaders &&
			slices.IsSortedFunc(a.Readers, cmpReader) &&
			reflect.DeepEqual(a, run())
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
		t.Error(err)
	}
}

// TestAccessCountsNeverWrapAround: a count is never negative and never goes
// down, whatever is added to whatever record. The largest read there can be
// is recorded whole; a total that has reached the largest number there is
// stays there, and so does what the readers folded into "others" had read.
func TestAccessCountsNeverWrapAround(t *testing.T) {
	t.Parallel()
	largest := object.Read{Who: "user/a", Downloads: object.MaxReadCount, Copies: object.MaxReadCount}
	if err := largest.Validate(); err != nil {
		t.Fatalf("the largest read there can be: %v", err)
	}
	if a := (object.Access{}).Add([]object.Read{largest, largest}, accessDay); a.Downloads != 2*object.MaxReadCount || a.Copies != 2*object.MaxReadCount {
		t.Errorf("two of the largest reads = %d downloads, %d copies", a.Downloads, a.Copies)
	}

	// A record whose counts are a step from the end, as none gets there by
	// counting: adding to it stops at the end.
	full := object.Access{
		Downloads: math.MaxInt64 - 1, Copies: math.MaxInt64, LastAt: accessDay,
		Readers:      []object.Reader{{Who: "user/a", Downloads: math.MaxInt64 - 1, Copies: math.MaxInt64, LastAt: accessDay}},
		OtherReaders: math.MaxInt64, OtherDownloads: math.MaxInt64 - 1, OtherCopies: math.MaxInt64,
	}
	got := full.Add([]object.Read{{Who: "user/a", Downloads: 5, Copies: 5}}, accessDay.Add(time.Second))
	if got.Downloads != math.MaxInt64 || got.Copies != math.MaxInt64 || got.Readers[0].Downloads != math.MaxInt64 || got.Readers[0].Copies != math.MaxInt64 {
		t.Errorf("counts at their end, added to = %+v", got)
	}
	// Enough new readers to fold the full one into the others.
	var reads []object.Read
	for i := range object.MaxReaders {
		reads = append(reads, object.Read{Who: fmt.Sprintf("user/n%02d", i), Downloads: 1})
	}
	got = got.Add(reads, accessDay.Add(time.Minute))
	if got.OtherReaders != math.MaxInt64 || got.OtherDownloads != math.MaxInt64 || got.OtherCopies != math.MaxInt64 {
		t.Errorf("others at their end, folded into = %d readers, %d downloads, %d copies", got.OtherReaders, got.OtherDownloads, got.OtherCopies)
	}

	property := func(start [6]int64, raw []int64) bool {
		for i := range start {
			start[i] &= math.MaxInt64
		}
		a := object.Access{
			Downloads: start[0], Copies: start[1], OtherReaders: start[2], OtherDownloads: start[3], OtherCopies: start[4],
			Readers: []object.Reader{{Who: "user/u0", Downloads: start[5], Copies: start[0]}},
		}
		for i, n := range raw {
			// Counts of every size, those beyond the bound and below
			// zero among them.
			read := object.Read{Who: fmt.Sprintf("user/u%d", i%40), Downloads: n, Copies: n >> 7}
			if i%3 == 0 {
				read.Downloads, read.Copies = n%object.MaxReadCount, object.MaxReadCount
			}
			before := a
			a = a.Add([]object.Read{read}, accessDay.Add(time.Duration(i)*time.Second))
			grew := a.Downloads >= before.Downloads && a.Copies >= before.Copies && a.OtherReaders >= before.OtherReaders &&
				a.OtherDownloads >= before.OtherDownloads && a.OtherCopies >= before.OtherCopies
			if !grew || (read.Validate() != nil && !reflect.DeepEqual(a, before)) {
				return false
			}
			for _, r := range a.Readers {
				if r.Downloads < 0 || r.Copies < 0 {
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 300}); err != nil {
		t.Error(err)
	}
}
