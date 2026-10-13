package bucket_test

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// goldenFiles are the files of testdata. They pin what is stored: a change
// that makes two releases write other bytes for the same record needs a rise
// of the entry schema (ADR-0023).
//
//go:embed testdata/*.golden.json
var goldenFiles embed.FS

// goldenFile returns a file of testdata, without the line break at its end.
func goldenFile(tb testing.TB, name string) []byte {
	tb.Helper()
	content, err := goldenFiles.ReadFile("testdata/" + name)
	if err != nil {
		tb.Fatal(err)
	}
	return bytes.TrimSpace(content)
}

// golden compares bytes with a file of testdata.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	if want := goldenFile(t, name); !bytes.Equal(want, got) {
		t.Errorf("%s:\n got %s\nwant %s", name, got, want)
	}
}

// dice is chance for the properties below, made from the numbers
// testing/quick draws: the same numbers, the same changes.
type dice struct {
	state int
}

// roll returns a number from 0 up to n.
func (d *dice) roll(n int) int {
	d.state = (d.state*1103515245 + 12345) & 0x7fffffff
	return (d.state >> 8) % n
}

func newDice(seed1, seed2 uint16) *dice {
	return &dice{state: int(seed1)<<15 ^ int(seed2)}
}

func TestValidateQuota(t *testing.T) {
	t.Parallel()
	for quota, valid := range map[int64]bool{
		1: true, 10 << 20: true, bucket.MaxQuotaBytes: true,
		0: false, -1: false, bucket.MaxQuotaBytes + 1: false, math.MaxInt64: false, math.MinInt64: false,
	} {
		err := bucket.ValidateQuota(quota)
		if (err == nil) != valid || (err != nil && !errors.Is(err, domain.ErrInvalidArgument)) {
			t.Errorf("ValidateQuota(%d) = %v, valid wanted: %v", quota, err, valid)
		}
	}
}

func exact(totals bucket.UsageTotals) bucket.Usage {
	return bucket.Usage{Exact: true, UsageTotals: totals}
}

func TestUsageAdd(t *testing.T) {
	t.Parallel()
	counting := func(phase bucket.UsagePhase, cursor string) bucket.Usage {
		return bucket.Usage{Count: &bucket.UsageCount{Phase: phase, Cursor: cursor}}
	}
	put := bucket.UsageDelta{Key: "m", Seq: 7, Objects: 1, CurrentBytes: 10, Versions: 1, StoredBytes: 10, ReplicationPending: 1}
	part := bucket.UsageDelta{Upload: "m", UploadBytes: 5}
	// A completion is of a key and of an upload at once.
	complete := bucket.UsageDelta{Key: "m", Upload: "m", Seq: 9, Objects: 1, CurrentBytes: 8, Versions: 1, StoredBytes: 8, UploadBytes: -8}
	ofPut := bucket.UsageTotals{Objects: 1, CurrentBytes: 10, Versions: 1, StoredBytes: 10, ReplicationPending: 1}
	tests := []struct {
		name  string
		start bucket.Usage
		delta bucket.UsageDelta
		live  bucket.UsageTotals
		count bucket.UsageTotals
		high  uint64
	}{
		{"an exact record takes the change", exact(bucket.UsageTotals{}), put, ofPut, bucket.UsageTotals{}, 7},
		{"an exact record takes a part", exact(bucket.UsageTotals{}), part, bucket.UsageTotals{UploadBytes: 5}, bucket.UsageTotals{}, 0},
		{
			"a completion moves bytes from the uploads to the versions",
			exact(bucket.UsageTotals{UploadBytes: 8}), complete,
			bucket.UsageTotals{Objects: 1, CurrentBytes: 8, Versions: 1, StoredBytes: 8},
			bucket.UsageTotals{},
			9,
		},
		{"the walk has not been to the key", counting(bucket.UsagePhaseKeys, "a"), put, bucket.UsageTotals{}, bucket.UsageTotals{}, 7},
		{"the walk has not been anywhere", counting(bucket.UsagePhaseKeys, ""), put, bucket.UsageTotals{}, bucket.UsageTotals{}, 7},
		{"the walk is at the key", counting(bucket.UsagePhaseKeys, "m"), put, bucket.UsageTotals{}, ofPut, 7},
		{"the walk is past the key", counting(bucket.UsagePhaseKeys, "z"), put, bucket.UsageTotals{}, ofPut, 7},
		{"uploads are not counted while the keys are", counting(bucket.UsagePhaseKeys, "z"), part, bucket.UsageTotals{}, bucket.UsageTotals{}, 0},
		{"every key is counted once the keys are done", counting(bucket.UsagePhaseUploads, ""), put, bucket.UsageTotals{}, ofPut, 7},
		{"the walk has not been to the upload", counting(bucket.UsagePhaseUploads, "a"), part, bucket.UsageTotals{}, bucket.UsageTotals{}, 0},
		{"the walk is at the upload", counting(bucket.UsagePhaseUploads, "m"), part, bucket.UsageTotals{}, bucket.UsageTotals{UploadBytes: 5}, 0},
		{"a walk that has ended counts everything", counting(bucket.UsagePhaseDone, ""), part, bucket.UsageTotals{}, bucket.UsageTotals{UploadBytes: 5}, 0},
		{
			"a completion while the keys are walked: the key, not the upload",
			counting(bucket.UsagePhaseKeys, "z"), complete,
			bucket.UsageTotals{},
			bucket.UsageTotals{Objects: 1, CurrentBytes: 8, Versions: 1, StoredBytes: 8},
			9,
		},
		{
			"a completion of an upload the walk has not been to: its parts were never counted",
			counting(bucket.UsagePhaseUploads, "a"), complete,
			bucket.UsageTotals{},
			bucket.UsageTotals{Objects: 1, CurrentBytes: 8, Versions: 1, StoredBytes: 8},
			9,
		},
		{"a change without a key is left to the walk", counting(bucket.UsagePhaseKeys, "z"), bucket.UsageDelta{Versions: 1}, bucket.UsageTotals{}, bucket.UsageTotals{}, 0},
		{"a phase there is not counts nothing", counting(9, "z"), put, bucket.UsageTotals{}, bucket.UsageTotals{}, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			before := tt.start
			if before.Count != nil {
				copied := *before.Count
				before.Count = &copied
			}
			got, clamped := before.Add(tt.delta)
			if clamped {
				t.Error("a change of a real bucket was held at a bound")
			}
			var count bucket.UsageTotals
			if got.Count != nil {
				count = got.Count.UsageTotals
			}
			if got.UsageTotals != tt.live || count != tt.count || got.High != tt.high {
				t.Errorf("Add = %+v, count %+v, high %d; want %+v, count %+v, high %d", got.UsageTotals, count, got.High, tt.live, tt.count, tt.high)
			}
			// The record that was added to is as it was.
			if before.UsageTotals != tt.start.UsageTotals || (before.Count != nil && *before.Count != *tt.start.Count) {
				t.Errorf("Add changed the record it was called on: %+v", before)
			}
		})
	}

	// A recount: the totals go on being kept while the walk counts again.
	recounting := exact(bucket.UsageTotals{Versions: 3, StoredBytes: 30}).Counting()
	if !recounting.Exact || !recounting.Walking() || recounting.Versions != 3 {
		t.Fatalf("Counting of an exact record = %+v", recounting)
	}
	after, _ := recounting.Add(bucket.UsageDelta{Key: "z", Versions: 1, StoredBytes: 1})
	if after.Versions != 4 || after.StoredBytes != 31 || after.Count.Versions != 0 {
		t.Errorf("a change beyond the cursor of a recount: %+v, count %+v", after.UsageTotals, after.Count)
	}
	// The sequence only ever rises.
	if got, _ := (bucket.Usage{Exact: true, High: 50}).Add(bucket.UsageDelta{Seq: 20}); got.High != 50 {
		t.Errorf("High went back to %d", got.High)
	}
}

func TestUsageCountsNeitherWrapNorGoBelowNothing(t *testing.T) {
	t.Parallel()
	full := exact(bucket.UsageTotals{Versions: math.MaxInt64 - 1, StoredBytes: math.MaxInt64, UploadBytes: 5})
	got, clamped := full.Add(bucket.UsageDelta{Versions: 10, StoredBytes: 10, UploadBytes: -20, Objects: math.MinInt64})
	if got.Versions != math.MaxInt64 || got.StoredBytes != math.MaxInt64 || got.UploadBytes != 0 || got.Objects != 0 {
		t.Errorf("Add at the bounds = %+v", got.UsageTotals)
	}
	// A count that was held at a bound is said, and the record is counted
	// again: exact as before, with a walk begun.
	if !clamped || !got.Exact || !got.Walking() || got.Count.Phase != bucket.UsagePhaseKeys || got.Count.Cursor != "" || got.Count.UsageTotals != (bucket.UsageTotals{}) {
		t.Errorf("Add at the bounds: clamped %v, record %+v, count %+v", clamped, got, got.Count)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("a record that was clamped cannot be kept: %v", err)
	}
	// A count under way that is held at a bound begins again, and a first
	// count stays a first count.
	first := bucket.Usage{Count: &bucket.UsageCount{Phase: bucket.UsagePhaseUploads, Cursor: "0a", UsageTotals: bucket.UsageTotals{Versions: 3, UploadBytes: 4}}}
	again, clamped := first.Add(bucket.UsageDelta{Upload: "09", UploadBytes: -5})
	if !clamped || again.Exact || again.Count.Phase != bucket.UsagePhaseKeys || again.Count.UsageTotals != (bucket.UsageTotals{}) || again.UsageTotals != (bucket.UsageTotals{}) {
		t.Errorf("a first count held at a bound: clamped %v, record %+v, count %+v", clamped, again, again.Count)
	}
	// What the walk has not been to is not counted, and so not clamped.
	if beyond, clamped := first.Add(bucket.UsageDelta{Upload: "0b", UploadBytes: -5}); clamped || *beyond.Count != *first.Count {
		t.Errorf("a change beyond the cursor: clamped %v, count %+v", clamped, beyond.Count)
	}
	// The record without a bucket's record takes nothing and clamps nothing.
	if none, clamped := (bucket.Usage{}).Add(bucket.UsageDelta{Key: "k", Versions: -1, Seq: 4}); clamped || none.Exact || none.Walking() || none.High != 4 {
		t.Errorf("no record: clamped %v, %+v", clamped, none)
	}
	if got.Used() != math.MaxInt64 {
		t.Errorf("Used at the bounds = %d", got.Used())
	}
	property := func(start, changes [7]int64, key string, phase uint8, cursor string) bool {
		totals := func(v [7]int64) bucket.UsageTotals {
			return bucket.UsageTotals{
				Objects: v[0] & math.MaxInt64, CurrentBytes: v[1] & math.MaxInt64, Versions: v[2] & math.MaxInt64, StoredBytes: v[3] & math.MaxInt64,
				UploadBytes: v[4] & math.MaxInt64, ReplicationPending: v[5] & math.MaxInt64, ReplicationFailed: v[6] & math.MaxInt64,
			}
		}
		u := bucket.Usage{Exact: true, UsageTotals: totals(start), Count: &bucket.UsageCount{Phase: bucket.UsagePhase(phase%4 + 1), Cursor: cursor, UsageTotals: totals(start)}}
		next, _ := u.Add(bucket.UsageDelta{
			Key: key, Upload: key, Objects: changes[0], CurrentBytes: changes[1], Versions: changes[2], StoredBytes: changes[3],
			UploadBytes: changes[4], ReplicationPending: changes[5], ReplicationFailed: changes[6],
		})
		for _, all := range []bucket.UsageTotals{next.UsageTotals, next.Count.UsageTotals} {
			for _, count := range []int64{all.Objects, all.CurrentBytes, all.Versions, all.StoredBytes, all.UploadBytes, all.ReplicationPending, all.ReplicationFailed, all.Used()} {
				if count < 0 {
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 20000}); err != nil {
		t.Error(err)
	}
}

func TestUsageAdmits(t *testing.T) {
	t.Parallel()
	holding := exact(bucket.UsageTotals{StoredBytes: 60, UploadBytes: 30})
	tests := []struct {
		name  string
		usage bucket.Usage
		quota int64
		delta bucket.UsageDelta
		want  bool
	}{
		{"no quota", holding, 0, bucket.UsageDelta{StoredBytes: 1 << 40}, true},
		{"within the quota", holding, 100, bucket.UsageDelta{StoredBytes: 9}, true},
		{"exactly to the quota", holding, 100, bucket.UsageDelta{StoredBytes: 10}, true},
		{"one byte past it", holding, 100, bucket.UsageDelta{StoredBytes: 11}, false},
		{"a part past it", holding, 100, bucket.UsageDelta{UploadBytes: 11}, false},
		{"a part within it", holding, 100, bucket.UsageDelta{UploadBytes: 10}, true},
		{"an overwrite of the same size", holding, 90, bucket.UsageDelta{StoredBytes: 0}, true},
		{"an overwrite by something smaller in a bucket that is over", holding, 50, bucket.UsageDelta{StoredBytes: -5}, true},
		{"an overwrite by something larger in a bucket that is over", holding, 50, bucket.UsageDelta{StoredBytes: 1}, false},
		{"a delete marker in a full bucket", holding, 90, bucket.UsageDelta{Versions: 1}, true},
		{"a delete in a bucket that is over", holding, 10, bucket.UsageDelta{Versions: -1, StoredBytes: -60}, true},
		{"a completion, which moves bytes and adds none", holding, 90, bucket.UsageDelta{StoredBytes: 30, UploadBytes: -30}, true},
		{"a completion that leaves parts unused", holding, 90, bucket.UsageDelta{StoredBytes: 20, UploadBytes: -30}, true},
		{"a quota below what is held, something added", holding, 10, bucket.UsageDelta{StoredBytes: 1}, false},
		{"the greatest quota", holding, bucket.MaxQuotaBytes, bucket.UsageDelta{StoredBytes: bucket.MaxQuotaBytes - 90}, true},
		{"the greatest quota, one byte past it", holding, bucket.MaxQuotaBytes, bucket.UsageDelta{StoredBytes: bucket.MaxQuotaBytes - 89}, false},
		{"a change beyond every number", holding, 100, bucket.UsageDelta{StoredBytes: math.MaxInt64, UploadBytes: math.MaxInt64}, false},
		{"a change below every number", holding, 100, bucket.UsageDelta{StoredBytes: math.MinInt64, UploadBytes: math.MinInt64}, true},
		{"an empty bucket, an object of the quota", exact(bucket.UsageTotals{}), 100, bucket.UsageDelta{StoredBytes: 100}, true},
		{"an empty bucket, an object past the quota", exact(bucket.UsageTotals{}), 100, bucket.UsageDelta{StoredBytes: 101}, false},
	}
	for _, tt := range tests {
		if got := tt.usage.Admits(tt.quota, tt.delta); got != tt.want {
			t.Errorf("%s: Admits(%d, %+v) = %v, want %v", tt.name, tt.quota, tt.delta, got, tt.want)
		}
	}
	if got := (bucket.UsageDelta{StoredBytes: 20, UploadBytes: -30}).Net(); got != -10 {
		t.Errorf("Net = %d", got)
	}
}

func TestUsageWalk(t *testing.T) {
	t.Parallel()
	// A bucket that was there before the record: counted from nothing.
	u := bucket.Usage{High: 3}.Counting()
	if u.Exact || !u.Walking() || u.Count.Phase != bucket.UsagePhaseKeys || u.Count.Cursor != "" || u.High != 3 {
		t.Fatalf("Counting = %+v, count %+v", u, u.Count)
	}
	if err := u.Validate(); err != nil {
		t.Fatalf("a record that is being counted: %v", err)
	}
	step := func(u bucket.Usage, s bucket.UsageStep) bucket.Usage {
		t.Helper()
		next, err := u.Step(s)
		if err != nil {
			t.Fatalf("Step(%+v): %v", s, err)
		}
		return next
	}
	u = step(u, bucket.UsageStep{Cursor: "b", Counted: bucket.UsageTotals{Objects: 2, CurrentBytes: 20, Versions: 3, StoredBytes: 25}, High: 40})
	u = step(u, bucket.UsageStep{}) // an entry that found nothing to do
	u = step(u, bucket.UsageStep{Cursor: "k", Counted: bucket.UsageTotals{Objects: 1, CurrentBytes: 1, Versions: 1, StoredBytes: 1, ReplicationFailed: 1}, High: 12, End: true})
	if u.Count.Phase != bucket.UsagePhaseUploads || u.Count.Cursor != "" || u.High != 40 || u.Exact {
		t.Fatalf("after the keys: %+v, count %+v", u, u.Count)
	}
	// The cursor of the uploads begins before every upload, whatever the
	// last key was.
	u = step(u, bucket.UsageStep{Cursor: "0a", Counted: bucket.UsageTotals{UploadBytes: 7}})
	u = step(u, bucket.UsageStep{End: true})
	if u.Count.Phase != bucket.UsagePhaseDone || u.Exact {
		t.Fatalf("after the uploads: %+v, count %+v", u, u.Count)
	}
	done, drift, err := u.Finish()
	want := bucket.UsageTotals{Objects: 3, CurrentBytes: 21, Versions: 4, StoredBytes: 26, UploadBytes: 7, ReplicationFailed: 1}
	if err != nil || drift || !done.Exact || done.Walking() || done.UsageTotals != want || done.High != 40 {
		t.Fatalf("Finish = %+v, drift %v, %v; want %+v", done, drift, err, want)
	}

	// A recount that finds what the record holds is no drift; one that
	// finds anything else is, and its result is what stays.
	for name, tt := range map[string]struct {
		found bucket.UsageTotals
		drift bool
	}{
		"the same":       {want, false},
		"one byte other": {bucket.UsageTotals{Objects: 3, CurrentBytes: 21, Versions: 4, StoredBytes: 27, UploadBytes: 7, ReplicationFailed: 1}, true},
	} {
		again := done.Counting()
		again = step(again, bucket.UsageStep{Cursor: "k", Counted: bucket.UsageTotals{
			Objects: tt.found.Objects, CurrentBytes: tt.found.CurrentBytes, Versions: tt.found.Versions, StoredBytes: tt.found.StoredBytes,
			ReplicationFailed: tt.found.ReplicationFailed,
		}, End: true})
		again = step(again, bucket.UsageStep{Cursor: "0a", Counted: bucket.UsageTotals{UploadBytes: tt.found.UploadBytes}, End: true})
		if !again.Exact || again.UsageTotals != want {
			t.Errorf("%s: the totals are not kept while the bucket is counted again: %+v", name, again)
		}
		result, drift, err := again.Finish()
		if err != nil || drift != tt.drift || result.UsageTotals != tt.found || !result.Exact || result.Walking() {
			t.Errorf("%s: Finish = %+v, drift %v, %v", name, result, drift, err)
		}
	}

	// A step that ends its phase may have counted and name no cursor: keys
	// that can be none (a state written before keys were validated) are
	// behind the end of the phase like every other.
	for name, begun := range map[string]bucket.Usage{
		"from the start":  bucket.Usage{}.Counting(),
		"behind a cursor": step(bucket.Usage{}.Counting(), bucket.UsageStep{Cursor: "b", Counted: bucket.UsageTotals{Versions: 1, StoredBytes: 2}}),
	} {
		ended := step(begun, bucket.UsageStep{Counted: bucket.UsageTotals{Objects: 1, CurrentBytes: 5, Versions: 1, StoredBytes: 5}, High: 9, End: true})
		wantCount := begun.Count.UsageTotals
		wantCount.Objects, wantCount.CurrentBytes, wantCount.Versions, wantCount.StoredBytes = wantCount.Objects+1, wantCount.CurrentBytes+5, wantCount.Versions+1, wantCount.StoredBytes+5
		if ended.Count.Phase != bucket.UsagePhaseUploads || ended.Count.Cursor != "" || ended.Count.UsageTotals != wantCount || ended.High != 9 {
			t.Errorf("%s: a step that counts, names no cursor and ends the keys = %+v, count %+v", name, ended, ended.Count)
		}
		if err := ended.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Beginning again throws away what a walk had counted.
	if again := u.Counting(); again.Count.Phase != bucket.UsagePhaseKeys || again.Count.UsageTotals != (bucket.UsageTotals{}) || again.High != 40 {
		t.Errorf("Counting during a count = %+v", again.Count)
	}
}

func TestUsageWalkRefused(t *testing.T) {
	t.Parallel()
	keys := bucket.Usage{Count: &bucket.UsageCount{Phase: bucket.UsagePhaseKeys, Cursor: "k"}}
	uploads := bucket.Usage{Count: &bucket.UsageCount{Phase: bucket.UsagePhaseUploads, Cursor: "k"}}
	ended := bucket.Usage{Count: &bucket.UsageCount{Phase: bucket.UsagePhaseDone}}
	for name, tt := range map[string]struct {
		usage bucket.Usage
		step  bucket.UsageStep
	}{
		"no walk under way":                 {exact(bucket.UsageTotals{}), bucket.UsageStep{Cursor: "a"}},
		"no record":                         {bucket.Usage{}, bucket.UsageStep{Cursor: "a"}},
		"a walk that has ended":             {ended, bucket.UsageStep{Cursor: "a"}},
		"the cursor goes back":              {keys, bucket.UsageStep{Cursor: "a"}},
		"the cursor stays where it is":      {keys, bucket.UsageStep{Cursor: "k"}},
		"counted without going anywhere":    {keys, bucket.UsageStep{Counted: bucket.UsageTotals{Versions: 1}}},
		"a count below nothing":             {keys, bucket.UsageStep{Cursor: "z", Counted: bucket.UsageTotals{Versions: -1}}},
		"uploads counted among the keys":    {keys, bucket.UsageStep{Cursor: "z", Counted: bucket.UsageTotals{UploadBytes: 1}}},
		"versions counted among uploads":    {uploads, bucket.UsageStep{Cursor: "z", Counted: bucket.UsageTotals{Versions: 1}}},
		"a cursor that is no key":           {keys, bucket.UsageStep{Cursor: "z\xff"}},
		"a cursor longer than a key can be": {keys, bucket.UsageStep{Cursor: "z" + strings.Repeat("a", object.MaxKeyLength)}},
	} {
		if got, err := tt.usage.Step(tt.step); !errors.Is(err, domain.ErrInvalidArgument) || got.Walking() || got.Exact {
			t.Errorf("%s: Step = %+v, %v", name, got, err)
		}
	}
	for name, usage := range map[string]bucket.Usage{
		"no record": {}, "an exact record": exact(bucket.UsageTotals{}), "the keys are being walked": keys, "the uploads are being walked": uploads,
	} {
		if got, drift, err := usage.Finish(); !errors.Is(err, domain.ErrInvalidArgument) || drift || got.Exact {
			t.Errorf("%s: Finish = %+v, %v, %v", name, got, drift, err)
		}
	}
}

func TestUsageRecord(t *testing.T) {
	t.Parallel()
	plain := bucket.Usage{
		Exact:       true,
		UsageTotals: bucket.UsageTotals{Objects: 3, CurrentBytes: 21, Versions: 4, StoredBytes: 26, UploadBytes: 7, ReplicationPending: 2, ReplicationFailed: 1},
		High:        40,
	}
	recounted := plain.Counting()
	recounted.Count.Cursor = "reports/<q1> & \"more\".txt"
	recounted.Count.UsageTotals = bucket.UsageTotals{Objects: 1, CurrentBytes: 9, Versions: 2, StoredBytes: 14}
	first := bucket.Usage{High: 5, Count: &bucket.UsageCount{Phase: bucket.UsagePhaseUploads, Cursor: "0a0b"}}
	for name, usage := range map[string]bucket.Usage{
		"usage-record.golden.json": plain, "usage-record-recount.golden.json": recounted, "usage-record-counting.golden.json": first,
	} {
		record, err := usage.Record()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		golden(t, name, record)
		back, err := bucket.ParseUsage(record)
		if err != nil || back.UsageTotals != usage.UsageTotals || back.Exact != usage.Exact || back.High != usage.High ||
			back.Walking() != usage.Walking() || (back.Walking() && *back.Count != *usage.Count) {
			t.Errorf("%s: ParseUsage = %+v, %v", name, back, err)
		}
	}

	for name, usage := range map[string]bucket.Usage{
		"no record":                  {},
		"neither exact nor counted":  {High: 9},
		"a count below nothing":      {Exact: true, UsageTotals: bucket.UsageTotals{UploadBytes: -1}},
		"a phase before the first":   {Count: &bucket.UsageCount{}},
		"a phase after the last":     {Count: &bucket.UsageCount{Phase: 4}},
		"a walk below nothing":       {Count: &bucket.UsageCount{Phase: 1, UsageTotals: bucket.UsageTotals{Objects: -1}}},
		"a cursor that is no key":    {Count: &bucket.UsageCount{Phase: 1, Cursor: "\xff"}},
		"a cursor that is too long":  {Count: &bucket.UsageCount{Phase: 1, Cursor: strings.Repeat("k", object.MaxKeyLength+1)}},
		"a cursor after the walk":    {Count: &bucket.UsageCount{Phase: 3, Cursor: "k"}},
		"exact, and a walk of none":  {Exact: true, Count: &bucket.UsageCount{Phase: 0}},
		"a failed count below none":  {Exact: true, UsageTotals: bucket.UsageTotals{ReplicationFailed: -1}},
		"a pending count below none": {Exact: true, UsageTotals: bucket.UsageTotals{ReplicationPending: -1}},
	} {
		if err := usage.Validate(); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: Validate = %v", name, err)
		}
		if _, err := usage.Record(); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: Record = %v", name, err)
		}
	}

	const record = `{"v":1,"exact":true,"objects":0,"current_bytes":0,"versions":0,"stored_bytes":0,"upload_bytes":0,"replication_pending":0,"replication_failed":0,"high":0}`
	if _, err := bucket.ParseUsage([]byte(record)); err != nil {
		t.Fatalf("the record of a new bucket: %v", err)
	}
	for name, document := range map[string]string{
		"nothing":                   "",
		"not JSON":                  "{",
		"a field too many":          strings.Replace(record, `"high":0`, `"high":0,"more":1`, 1),
		"a field twice":             strings.Replace(record, `"high":0`, `"high":0,"high":0`, 1),
		"a field missing":           strings.Replace(record, `"upload_bytes":0,`, "", 1),
		"another order":             strings.Replace(record, `"objects":0,"current_bytes":0`, `"current_bytes":0,"objects":0`, 1),
		"a field in capitals":       strings.Replace(record, `"high"`, `"High"`, 1),
		"white space":               strings.Replace(record, `,"high"`, `, "high"`, 1),
		"something after":           record + " {}",
		"a number as text":          strings.Replace(record, `"objects":0`, `"objects":"0"`, 1),
		"a number with a point":     strings.Replace(record, `"objects":0`, `"objects":0.0`, 1),
		"a count below nothing":     strings.Replace(record, `"objects":0`, `"objects":-1`, 1),
		"neither exact nor counted": strings.Replace(record, `"exact":true`, `"exact":false`, 1),
		"a count that is null":      strings.Replace(record, `"high":0`, `"high":0,"count":null`, 1),
		"too long":                  `{"v":1,"exact":true,"x":"` + strings.Repeat("a", bucket.MaxConfigBytes) + `"}`,
	} {
		if _, err := bucket.ParseUsage([]byte(document)); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: ParseUsage = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	for _, version := range []string{`"v":2`, `"v":0`} {
		if _, err := bucket.ParseUsage([]byte(strings.Replace(record, `"v":1`, version, 1))); !errors.Is(err, domain.ErrUnsupportedFormat) {
			t.Errorf("a record with %s: %v, want %v", version, err, domain.ErrUnsupportedFormat)
		}
	}
}

func FuzzParseUsage(f *testing.F) {
	for _, name := range []string{
		"usage-record.golden.json", "usage-record-recount.golden.json", "usage-record-counting.golden.json",
		"usage-record-text.golden.json", "usage-record-bounds.golden.json",
	} {
		f.Add(goldenFile(f, name), "k", int64(5), uint8(1))
	}
	f.Add([]byte(`{"v":2}`), "", int64(-1), uint8(0))
	f.Add([]byte(`{`), "a", int64(0), uint8(3))
	f.Fuzz(func(t *testing.T, record []byte, key string, change int64, steps uint8) {
		usage, err := bucket.ParseUsage(record)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) && !errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Fatalf("ParseUsage: %v", err)
			}
			return
		}
		// What is read is exactly what would be written.
		again, err := usage.Record()
		if err != nil || !bytes.Equal(again, record) {
			t.Fatalf("a record that was read is written as %s, %v", again, err)
		}
		// Whatever is done with a record that was read, it stays one that
		// can be written, and no count goes below nothing.
		check := func(u bucket.Usage) {
			if _, err := u.Record(); err != nil {
				t.Fatalf("a record that was kept cannot be written: %+v: %v", u, err)
			}
		}
		next, _ := usage.Add(bucket.UsageDelta{
			Key: key, Upload: key, Seq: uint64(steps), Objects: change, CurrentBytes: change, Versions: change, StoredBytes: change,
			UploadBytes: -change, ReplicationPending: change, ReplicationFailed: -change,
		})
		check(next)
		_ = next.Admits(change, bucket.UsageDelta{StoredBytes: change})
		walk := next.Counting()
		check(walk)
		for range steps % 4 {
			stepped, err := walk.Step(bucket.UsageStep{Cursor: key, End: true})
			if err != nil {
				if !errors.Is(err, domain.ErrInvalidArgument) {
					t.Fatalf("Step: %v", err)
				}
				break
			}
			walk = stepped
			check(walk)
		}
		if done, _, err := walk.Finish(); err == nil {
			check(done)
			if !done.Exact || done.Walking() {
				t.Fatalf("Finish = %+v", done)
			}
		}
	})
}

// A model of a bucket for the properties below: its keys with their
// versions, oldest first, and its uploads in progress with the bytes of
// their parts. It counts by looking, which is what a recount does.
type modelVersion struct {
	size   int64
	marker bool
	status object.ReplicationStatus
}

type model struct {
	keys    map[string][]modelVersion
	uploads map[string]int64
	seq     uint64
}

func (m *model) ofKey(key string) bucket.UsageTotals {
	var totals bucket.UsageTotals
	versions := m.keys[key]
	for i, v := range versions {
		totals.Versions++
		totals.StoredBytes += v.size
		if i == len(versions)-1 && !v.marker {
			totals.Objects, totals.CurrentBytes = 1, v.size
		}
		switch v.status {
		case object.ReplicationPending:
			totals.ReplicationPending++
		case object.ReplicationFailed:
			totals.ReplicationFailed++
		}
	}
	return totals
}

func sum(a, b bucket.UsageTotals) bucket.UsageTotals {
	return bucket.UsageTotals{
		Objects: a.Objects + b.Objects, CurrentBytes: a.CurrentBytes + b.CurrentBytes, Versions: a.Versions + b.Versions,
		StoredBytes: a.StoredBytes + b.StoredBytes, UploadBytes: a.UploadBytes + b.UploadBytes,
		ReplicationPending: a.ReplicationPending + b.ReplicationPending, ReplicationFailed: a.ReplicationFailed + b.ReplicationFailed,
	}
}

// recount counts the bucket by looking at all of it - or, with a count under
// way, at what the walk has been to.
func (m *model) recount(count *bucket.UsageCount) bucket.UsageTotals {
	var totals bucket.UsageTotals
	for key := range m.keys {
		if count == nil || count.Phase > bucket.UsagePhaseKeys || key <= count.Cursor {
			totals = sum(totals, m.ofKey(key))
		}
	}
	for upload, size := range m.uploads {
		if count == nil || count.Phase > bucket.UsagePhaseUploads || (count.Phase == bucket.UsagePhaseUploads && upload <= count.Cursor) {
			totals.UploadBytes += size
		}
	}
	return totals
}

// change makes one change of the bucket, as chance has it, and returns what
// it did to the usage.
func (m *model) change(r *dice) bucket.UsageDelta {
	key := fmt.Sprintf("k%02d", r.roll(12))
	upload := fmt.Sprintf("%02x", r.roll(6))
	before := m.ofKey(key)
	delta := bucket.UsageDelta{Key: key}
	versions := m.keys[key]
	status := []object.ReplicationStatus{"", object.ReplicationPending, object.ReplicationCompleted, object.ReplicationFailed, object.ReplicationReplica}
	switch op := r.roll(10); {
	case op < 3: // a put where versions are kept
		m.seq++
		delta.Seq = m.seq
		m.keys[key] = append(versions, modelVersion{size: int64(r.roll(1000)), status: status[r.roll(len(status))]})
	case op == 3 && len(versions) > 0: // a put where they are not: the newest is replaced
		versions[len(versions)-1] = modelVersion{size: int64(r.roll(1000))}
	case op == 4: // a delete that leaves a marker
		m.seq++
		delta.Seq = m.seq
		m.keys[key] = append(versions, modelVersion{marker: true})
	case op == 5 && len(versions) > 0: // a version removed for good
		i := r.roll(len(versions))
		m.keys[key] = slices.Delete(versions, i, i+1)
		if len(m.keys[key]) == 0 {
			delete(m.keys, key)
		}
	case op == 6 && len(versions) > 0: // an acknowledgement sets a status
		versions[r.roll(len(versions))].status = status[r.roll(len(status))]
	case op == 7: // a part
		size := int64(r.roll(500))
		m.uploads[upload] += size
		return bucket.UsageDelta{Upload: upload, UploadBytes: size}
	case op == 8: // an abort
		size, ok := m.uploads[upload]
		delete(m.uploads, upload)
		if !ok {
			return bucket.UsageDelta{}
		}
		return bucket.UsageDelta{Upload: upload, UploadBytes: -size}
	default: // a completion, which may leave parts unused
		size, ok := m.uploads[upload]
		if !ok {
			return bucket.UsageDelta{}
		}
		delete(m.uploads, upload)
		m.seq++
		delta.Seq, delta.Upload, delta.UploadBytes = m.seq, upload, -size
		m.keys[key] = append(versions, modelVersion{size: size - size*int64(r.roll(4))/4})
	}
	after := m.ofKey(key)
	delta.Objects, delta.CurrentBytes = after.Objects-before.Objects, after.CurrentBytes-before.CurrentBytes
	delta.Versions, delta.StoredBytes = after.Versions-before.Versions, after.StoredBytes-before.StoredBytes
	delta.ReplicationPending = after.ReplicationPending - before.ReplicationPending
	delta.ReplicationFailed = after.ReplicationFailed - before.ReplicationFailed
	return delta
}

// step makes one entry of the walk: up to limit keys - or uploads - after
// the cursor, in order, whole.
func (m *model) step(count bucket.UsageCount, limit int) bucket.UsageStep {
	var step bucket.UsageStep
	if count.Phase == bucket.UsagePhaseKeys {
		keys := slices.Sorted(maps.Keys(m.keys))
		at, _ := slices.BinarySearch(keys, count.Cursor+"\x00")
		rest := keys[at:]
		for _, key := range rest[:min(limit, len(rest))] {
			step.Cursor, step.Counted = key, sum(step.Counted, m.ofKey(key))
		}
		step.End, step.High = len(rest) <= limit, m.seq
		return step
	}
	uploads := slices.Sorted(maps.Keys(m.uploads))
	at, _ := slices.BinarySearch(uploads, count.Cursor+"\x00")
	rest := uploads[at:]
	for _, upload := range rest[:min(limit, len(rest))] {
		step.Cursor = upload
		step.Counted.UploadBytes += m.uploads[upload]
	}
	step.End = len(rest) <= limit
	return step
}

// TestUsageEqualsARecount is the property the record stands on (ADR-0023):
// with changes and entries of the walk in any order,
//
//   - an exact record holds what a recount of the bucket finds, always;
//   - what a walk has counted is what a recount of the part it has been to
//     finds, always;
//   - a first count that ends is exact and holds what the bucket holds, and
//     a recount that ends found no drift;
//   - High is the greatest sequence there was.
func TestUsageEqualsARecount(t *testing.T) {
	t.Parallel()
	property := func(seed1, seed2 uint16, recount bool) bool {
		r := newDice(seed1, seed2)
		m := &model{keys: map[string][]modelVersion{}, uploads: map[string]int64{}}
		usage := bucket.Usage{}
		if recount {
			// A bucket of schema 5: exact from the start, with something
			// in it before it is counted again.
			usage.Exact = true
		}
		for range r.roll(30) {
			delta := m.change(r)
			if recount {
				usage, _ = usage.Add(delta)
			}
		}
		check := func() bool {
			if usage.Exact && usage.UsageTotals != m.recount(nil) {
				t.Logf("exact record %+v, the bucket holds %+v", usage.UsageTotals, m.recount(nil))
				return false
			}
			if usage.Count != nil && usage.Count.UsageTotals != m.recount(usage.Count) {
				t.Logf("the walk at %+v counted %+v, there is %+v", usage.Count, usage.Count.UsageTotals, m.recount(usage.Count))
				return false
			}
			return true
		}
		if !check() {
			return false
		}
		usage = usage.Counting()
		for rounds := 0; usage.Walking(); rounds++ {
			if rounds > 10000 {
				t.Log("the walk does not end")
				return false
			}
			var err error
			switch {
			case usage.Count.Phase == bucket.UsagePhaseDone:
				var drift bool
				if usage, drift, err = usage.Finish(); err != nil || drift {
					t.Logf("Finish: drift %v, %v", drift, err)
					return false
				}
			case r.roll(3) == 0:
				usage, err = usage.Step(m.step(*usage.Count, 1+r.roll(4)))
				if err != nil {
					t.Logf("Step: %v", err)
					return false
				}
			default:
				usage, _ = usage.Add(m.change(r))
			}
			if !check() {
				return false
			}
		}
		// From here on every change keeps it.
		for range 20 {
			usage, _ = usage.Add(m.change(r))
		}
		return usage.Exact && usage.UsageTotals == m.recount(nil) && usage.High == m.seq
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// TestUsageIsTheSameHoweverChangesAreGrouped: the same changes, added one by
// one or several at once as one change of their sum, give the same record -
// what makes it safe for one entry of the log to change several things.
func TestUsageIsTheSameHoweverChangesAreGrouped(t *testing.T) {
	t.Parallel()
	property := func(seed1, seed2 uint16) bool {
		r := newDice(seed1, seed2)
		m := &model{keys: map[string][]modelVersion{}, uploads: map[string]int64{}}
		deltas := make([]bucket.UsageDelta, 40+r.roll(40))
		for i := range deltas {
			deltas[i] = m.change(r)
		}
		oneByOne := bucket.Usage{Exact: true}
		clamped := false
		add := func(u bucket.Usage, delta bucket.UsageDelta) bucket.Usage {
			next, held := u.Add(delta)
			clamped = clamped || held
			return next
		}
		for _, delta := range deltas {
			oneByOne = add(oneByOne, delta)
		}
		grouped := bucket.Usage{Exact: true}
		for rest := deltas; len(rest) > 0; {
			n := 1 + r.roll(min(5, len(rest)))
			var group bucket.UsageDelta
			for _, delta := range rest[:n] {
				group.Seq = max(group.Seq, delta.Seq)
				group.Objects += delta.Objects
				group.CurrentBytes += delta.CurrentBytes
				group.Versions += delta.Versions
				group.StoredBytes += delta.StoredBytes
				group.UploadBytes += delta.UploadBytes
				group.ReplicationPending += delta.ReplicationPending
				group.ReplicationFailed += delta.ReplicationFailed
			}
			grouped, rest = add(grouped, group), rest[n:]
		}
		return !clamped && grouped.UsageTotals == oneByOne.UsageTotals && grouped.High == oneByOne.High && oneByOne.UsageTotals == m.recount(nil)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// TestUsageIsTheSameHoweverChangesAreGroupedDuringAWalk is the same for a
// bucket that is being counted, where a change counts or not by where its
// key and its upload are: the changes added one by one, or summed into one
// Add for each key and one for each upload, give the same record. Summed
// across keys they would not, which is why a delta is of one key and of one
// upload.
func TestUsageIsTheSameHoweverChangesAreGroupedDuringAWalk(t *testing.T) {
	t.Parallel()
	property := func(seed1, seed2 uint16, recount bool) bool {
		r := newDice(seed1, seed2)
		m := &model{keys: map[string][]modelVersion{}, uploads: map[string]int64{}}
		clamped := false
		add := func(u bucket.Usage, delta bucket.UsageDelta) bucket.Usage {
			next, held := u.Add(delta)
			clamped = clamped || held
			return next
		}
		start := bucket.Usage{Exact: recount}
		for range r.roll(30) {
			start = add(start, m.change(r))
		}
		// A walk that has come some way: among the keys, among the
		// uploads, or to its end.
		start = start.Counting()
		for range r.roll(8) {
			if start.Count.Phase == bucket.UsagePhaseDone {
				break
			}
			next, err := start.Step(m.step(*start.Count, 1+r.roll(4)))
			if err != nil {
				t.Logf("Step: %v", err)
				return false
			}
			start = next
		}
		deltas := make([]bucket.UsageDelta, 20+r.roll(40))
		for i := range deltas {
			deltas[i] = m.change(r)
		}
		oneByOne := start
		for _, delta := range deltas {
			oneByOne = add(oneByOne, delta)
		}
		// One Add a key, one Add an upload - in the order of their names,
		// which is not the order the changes were made in.
		ofKeys, ofUploads := map[string]bucket.UsageDelta{}, map[string]bucket.UsageDelta{}
		for _, delta := range deltas {
			if delta.Key != "" {
				group := ofKeys[delta.Key]
				group.Key, group.Seq = delta.Key, max(group.Seq, delta.Seq)
				group.Objects += delta.Objects
				group.CurrentBytes += delta.CurrentBytes
				group.Versions += delta.Versions
				group.StoredBytes += delta.StoredBytes
				group.ReplicationPending += delta.ReplicationPending
				group.ReplicationFailed += delta.ReplicationFailed
				ofKeys[delta.Key] = group
			}
			if delta.Upload != "" {
				group := ofUploads[delta.Upload]
				group.Upload = delta.Upload
				group.UploadBytes += delta.UploadBytes
				ofUploads[delta.Upload] = group
			}
		}
		grouped := start
		for _, key := range slices.Sorted(maps.Keys(ofKeys)) {
			grouped = add(grouped, ofKeys[key])
		}
		for _, upload := range slices.Sorted(maps.Keys(ofUploads)) {
			grouped = add(grouped, ofUploads[upload])
		}
		if clamped || grouped.UsageTotals != oneByOne.UsageTotals || grouped.High != oneByOne.High || grouped.Exact != oneByOne.Exact ||
			*grouped.Count != *oneByOne.Count {
			t.Logf("one by one %+v, count %+v; grouped %+v, count %+v; clamped %v", oneByOne, oneByOne.Count, grouped, grouped.Count, clamped)
			return false
		}
		// And both are what a recount of the part the walk has been to
		// finds.
		return oneByOne.Count.UsageTotals == m.recount(oneByOne.Count) && (!recount || oneByOne.UsageTotals == m.recount(nil))
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

func TestUsageNextSeq(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		high, index uint64
		want        uint64
	}{
		{"a bucket that never took a replica: the index", 40, 41, 41},
		{"an index far above the greatest sequence", 40, 5000, 5000},
		{"a version at the index already", 41, 41, 42},
		{"a replica far above the index", 1 << 40, 41, 1<<40 + 1},
		{"the greatest replica there can be", object.MaxReplicaSeq, 41, object.MaxReplicaSeq + 1},
		{"one below the last number", math.MaxUint64 - 1, 41, math.MaxUint64},
		{"the last number: it does not wrap", math.MaxUint64, 41, math.MaxUint64},
		{"the last number as the index", 7, math.MaxUint64, math.MaxUint64},
		{"nothing yet", 0, 0, 1},
	} {
		if got := (bucket.Usage{Exact: true, High: tt.high}).NextSeq(tt.index); got != tt.want {
			t.Errorf("%s: NextSeq(%d) with High %d = %d, want %d", tt.name, tt.index, tt.high, got, tt.want)
		}
	}
	// It is never below the index or the greatest sequence, and above the
	// greatest wherever a number above it exists.
	property := func(high, index uint64) bool {
		next := (bucket.Usage{High: high}).NextSeq(index)
		return next >= index && next >= high && (high == math.MaxUint64 || next > high)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 5000}); err != nil {
		t.Error(err)
	}
}
