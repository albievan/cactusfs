package bucket_test

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
)

var activityDay = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// minutes is a time that many minutes into the day of the tests.
func minutes(n int) time.Time { return activityDay.Add(time.Duration(n) * time.Minute) }

func TestActivityZeroValue(t *testing.T) {
	t.Parallel()
	var a bucket.Activity
	if !a.Empty() {
		t.Error("the zero record is not empty")
	}
	if c := a.Clone(); !c.Empty() || c.Uploads != nil || c.Downloads != nil {
		t.Errorf("a copy of the zero record = %+v", c)
	}
	up := a.Uploaded(minutes(1), "user/albie", "k")
	down := a.Downloaded(minutes(2), "anonymous", false, "k")
	if !reflect.DeepEqual(up, bucket.Activity{Uploads: []bucket.RecentUpload{{At: minutes(1), By: "user/albie", Key: "k"}}}) || up.Empty() {
		t.Errorf("the first upload = %+v", up)
	}
	if !reflect.DeepEqual(down, bucket.Activity{Downloads: []bucket.RecentDownload{{At: minutes(2), By: "anonymous", Key: "k"}}}) || down.Empty() {
		t.Errorf("the first download = %+v", down)
	}
	if !a.Empty() {
		t.Errorf("the zero record was changed: %+v", a)
	}
}

func TestActivityUploaded(t *testing.T) {
	t.Parallel()
	type step struct {
		minute  int
		by, key string
	}
	tests := map[string]struct {
		steps []step
		want  []bucket.RecentUpload
	}{
		"newest first": {
			steps: []step{{1, "root", "a"}, {2, "user/b", "b"}},
			want:  []bucket.RecentUpload{{At: minutes(2), By: "user/b", Key: "b"}, {At: minutes(1), By: "root", Key: "a"}},
		},
		"at most three, the oldest goes": {
			steps: []step{{1, "root", "a"}, {2, "root", "b"}, {3, "root", "c"}, {4, "key/AKID", "d"}},
			want: []bucket.RecentUpload{
				{At: minutes(4), By: "key/AKID", Key: "d"}, {At: minutes(3), By: "root", Key: "c"}, {At: minutes(2), By: "root", Key: "b"},
			},
		},
		"a key has one row, and a later upload of it moves to the front": {
			steps: []step{{1, "root", "a"}, {2, "root", "b"}, {3, "user/c", "a"}},
			want:  []bucket.RecentUpload{{At: minutes(3), By: "user/c", Key: "a"}, {At: minutes(2), By: "root", Key: "b"}},
		},
		"an upload of a key older than its row changes nothing": {
			steps: []step{{5, "root", "a"}, {2, "user/late", "a"}},
			want:  []bucket.RecentUpload{{At: minutes(5), By: "root", Key: "a"}},
		},
		"of two of the same time the later recorded comes first": {
			steps: []step{{1, "root", "a"}, {1, "root", "b"}, {1, "root", "c"}, {1, "root", "d"}},
			want: []bucket.RecentUpload{
				{At: minutes(1), By: "root", Key: "d"}, {At: minutes(1), By: "root", Key: "c"}, {At: minutes(1), By: "root", Key: "b"},
			},
		},
		"the same key at the same time is the later one": {
			steps: []step{{1, "root", "a"}, {1, "user/b", "a"}},
			want:  []bucket.RecentUpload{{At: minutes(1), By: "user/b", Key: "a"}},
		},
		"an earlier upload goes where its time puts it": {
			steps: []step{{5, "root", "a"}, {1, "root", "b"}, {3, "root", "c"}},
			want: []bucket.RecentUpload{
				{At: minutes(5), By: "root", Key: "a"}, {At: minutes(3), By: "root", Key: "c"}, {At: minutes(1), By: "root", Key: "b"},
			},
		},
		"an upload older than three kept ones is not kept": {
			steps: []step{{5, "root", "a"}, {6, "root", "b"}, {7, "root", "c"}, {1, "root", "d"}},
			want: []bucket.RecentUpload{
				{At: minutes(7), By: "root", Key: "c"}, {At: minutes(6), By: "root", Key: "b"}, {At: minutes(5), By: "root", Key: "a"},
			},
		},
		"an uploader who was not recorded": {
			steps: []step{{1, "", "a"}},
			want:  []bucket.RecentUpload{{At: minutes(1), Key: "a"}},
		},
		"what cannot be recorded is left out": {
			steps: []step{
				{1, "root", "a"},
				{2, "root", ""},
				{3, "root", strings.Repeat("k", object.MaxKeyLength+1)},
				{4, strings.Repeat("u", 257), "b"},
				{5, "user/a\nuser/b", "c"},
				{6, "user/a\x00", "d"},
			},
			want: []bucket.RecentUpload{{At: minutes(1), By: "root", Key: "a"}},
		},
		"the longest key and the longest name are kept whole": {
			steps: []step{{1, strings.Repeat("u", 256), strings.Repeat("k", object.MaxKeyLength)}},
			want:  []bucket.RecentUpload{{At: minutes(1), By: strings.Repeat("u", 256), Key: strings.Repeat("k", object.MaxKeyLength)}},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var a bucket.Activity
			for _, s := range test.steps {
				before := a.Clone()
				next := a.Uploaded(minutes(s.minute), s.by, s.key)
				if !reflect.DeepEqual(a, before) {
					t.Fatalf("Uploaded changed the record it was called on: %+v, was %+v", a, before)
				}
				a = next
			}
			if !reflect.DeepEqual(a.Uploads, test.want) || len(a.Downloads) != 0 {
				t.Errorf("uploads = %+v\nwant      %+v (downloads %v)", a.Uploads, test.want, a.Downloads)
			}
		})
	}
}

func TestActivityDownloaded(t *testing.T) {
	t.Parallel()
	type step struct {
		minute    int
		by        string
		presigned bool
		key       string
	}
	tests := map[string]struct {
		steps []step
		want  []bucket.RecentDownload
	}{
		"newest first, at most three": {
			steps: []step{{1, "root", false, "a"}, {2, "root", false, "b"}, {3, "root", false, "c"}, {4, "anonymous", false, "d"}},
			want: []bucket.RecentDownload{
				{At: minutes(4), By: "anonymous", Key: "d"}, {At: minutes(3), By: "root", Key: "c"}, {At: minutes(2), By: "root", Key: "b"},
			},
		},
		"a key, a reader and the way they came have one row": {
			steps: []step{{1, "root", false, "a"}, {2, "user/b", false, "a"}, {3, "root", false, "a"}},
			want:  []bucket.RecentDownload{{At: minutes(3), By: "root", Key: "a"}, {At: minutes(2), By: "user/b", Key: "a"}},
		},
		"a presigned download is not the signer's own": {
			steps: []step{{1, "key/AKID", false, "a"}, {2, "key/AKID", true, "a"}, {3, "key/AKID", true, "a"}},
			want: []bucket.RecentDownload{
				{At: minutes(3), By: "key/AKID", Presigned: true, Key: "a"}, {At: minutes(1), By: "key/AKID", Key: "a"},
			},
		},
		"the same reader of another key is another row": {
			steps: []step{{1, "root", false, "a"}, {2, "root", false, "b"}},
			want:  []bucket.RecentDownload{{At: minutes(2), By: "root", Key: "b"}, {At: minutes(1), By: "root", Key: "a"}},
		},
		"a download older than its row changes nothing": {
			steps: []step{{5, "root", false, "a"}, {2, "root", false, "a"}},
			want:  []bucket.RecentDownload{{At: minutes(5), By: "root", Key: "a"}},
		},
		"of two of the same time the later recorded comes first": {
			steps: []step{{1, "user/a", false, "k"}, {1, "user/b", false, "k"}, {1, "user/c", false, "k"}, {1, "user/d", false, "k"}},
			want: []bucket.RecentDownload{
				{At: minutes(1), By: "user/d", Key: "k"}, {At: minutes(1), By: "user/c", Key: "k"}, {At: minutes(1), By: "user/b", Key: "k"},
			},
		},
		"what cannot be recorded is left out": {
			steps: []step{
				{1, "root", false, "a"},
				{2, "", false, "b"},
				{3, "root", true, ""},
				{4, strings.Repeat("u", 257), false, "c"},
				{5, "user/a\ruser/b", false, "d"},
				{6, "root", false, strings.Repeat("k", object.MaxKeyLength+1)},
			},
			want: []bucket.RecentDownload{{At: minutes(1), By: "root", Key: "a"}},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var a bucket.Activity
			for _, s := range test.steps {
				before := a.Clone()
				next := a.Downloaded(minutes(s.minute), s.by, s.presigned, s.key)
				if !reflect.DeepEqual(a, before) {
					t.Fatalf("Downloaded changed the record it was called on: %+v, was %+v", a, before)
				}
				a = next
			}
			if !reflect.DeepEqual(a.Downloads, test.want) || len(a.Uploads) != 0 {
				t.Errorf("downloads = %+v\nwant        %+v (uploads %v)", a.Downloads, test.want, a.Uploads)
			}
		})
	}
}

// TestActivitySharesNothing: a record made from another has rows of its own.
// Two records that grew from one - as the state of two replicas does - must
// not write into each other.
func TestActivitySharesNothing(t *testing.T) {
	t.Parallel()
	base := bucket.Activity{}.Uploaded(minutes(1), "root", "a").Downloaded(minutes(1), "root", false, "a")
	left := base.Uploaded(minutes(2), "user/l", "left").Downloaded(minutes(2), "user/l", false, "left")
	right := base.Uploaded(minutes(2), "user/r", "right").Downloaded(minutes(2), "user/r", true, "right")
	if left.Uploads[0].Key != "left" || right.Uploads[0].Key != "right" || left.Downloads[0].By != "user/l" || right.Downloads[0].By != "user/r" {
		t.Errorf("two records grown from one: %+v and %+v", left, right)
	}
	if len(base.Uploads) != 1 || len(base.Downloads) != 1 {
		t.Errorf("the record they grew from = %+v", base)
	}
	// Nor does what is left out, or the copy, hand the rows themselves on.
	for name, other := range map[string]bucket.Activity{
		"a copy":               base.Clone(),
		"an upload left out":   base.Uploaded(minutes(3), "root", ""),
		"a download left out":  base.Downloaded(minutes(3), "", false, "a"),
		"an upload too old":    base.Uploaded(minutes(0), "root", "a"),
		"a download too old":   base.Downloaded(minutes(0), "root", false, "a"),
		"the other kind added": base.Uploaded(minutes(3), "root", "b"),
	} {
		other.Uploads[0].Key, other.Downloads[0].Key = "changed", "changed"
		if base.Uploads[0].Key != "a" || base.Downloads[0].Key != "a" {
			t.Fatalf("%s shares its rows with the record it came from", name)
		}
	}
}

// TestActivityCutsARecordThatIsTooLong: a record with more rows than the
// bound - which none gets by Uploaded and Downloaded - is back within it
// after the next row.
func TestActivityCutsARecordThatIsTooLong(t *testing.T) {
	t.Parallel()
	var long bucket.Activity
	for i := range 10 {
		long.Uploads = append(long.Uploads, bucket.RecentUpload{At: minutes(20 - i), Key: fmt.Sprint("u", i)})
		long.Downloads = append(long.Downloads, bucket.RecentDownload{At: minutes(20 - i), By: "root", Key: fmt.Sprint("d", i)})
	}
	got := long.Uploaded(minutes(30), "root", "new").Downloaded(minutes(30), "root", false, "new")
	if len(got.Uploads) != bucket.MaxRecentUploads || len(got.Downloads) != bucket.MaxRecentDownloads || got.Uploads[0].Key != "new" || got.Downloads[0].Key != "new" {
		t.Errorf("a record of ten rows, added to = %+v", got)
	}
}

func TestValidateCreator(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		by    string
		valid bool
	}{
		"nobody recorded":   {"", true},
		"root":              {"root", true},
		"a user":            {"user/albie", true},
		"the longest":       {strings.Repeat("u", 256), true},
		"not ASCII":         {"user/zoë", true},
		"too long":          {strings.Repeat("u", 257), false},
		"two lines":         {"user/a\nuser/b", false},
		"a carriage back":   {"user/a\r", false},
		"a NUL":             {"user/a\x00", false},
		"too long by a lot": {strings.Repeat("u", 1<<16), false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := bucket.ValidateCreator(test.by)
			if (err == nil) != test.valid {
				t.Fatalf("ValidateCreator = %v, want valid: %t", err, test.valid)
			}
			if err != nil && !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("the error %v does not wrap %v", err, domain.ErrInvalidArgument)
			}
		})
	}
}

// activityEvent is one upload or download of a random sequence.
type activityEvent struct {
	upload    bool
	minute    int
	by        string
	presigned bool
	key       string
}

// activityEvents turns random numbers into a sequence of uploads and
// downloads over a few keys and readers, some of them at the same time, some
// earlier than the one before, and some that cannot be recorded.
func activityEvents(raw []uint16) []activityEvent {
	events := make([]activityEvent, 0, len(raw))
	for i, r := range raw {
		e := activityEvent{
			upload: r%2 == 0, minute: i/2 + int(r%3), by: fmt.Sprintf("user/u%d", r%5), presigned: r%7 == 0, key: fmt.Sprintf("k%d", r%6),
		}
		switch r % 23 {
		case 0:
			e.key = ""
		case 1:
			e.by = strings.Repeat("u", 300)
		case 2:
			e.by = ""
		case 3:
			e.minute = 0
		}
		events = append(events, e)
	}
	return events
}

// TestActivityProperties: whatever is uploaded and downloaded, in whatever
// order, the bounds hold, the rows are newest first, no key has two rows of
// uploads and no key, reader and way of coming two of downloads, the newest
// that could be recorded is at the front, and the same sequence gives the
// same record.
func TestActivityProperties(t *testing.T) {
	t.Parallel()
	property := func(raw []uint16) bool {
		events := activityEvents(raw)
		run := func() bucket.Activity {
			var a bucket.Activity
			for _, e := range events {
				if e.upload {
					a = a.Uploaded(minutes(e.minute), e.by, e.key)
				} else {
					a = a.Downloaded(minutes(e.minute), e.by, e.presigned, e.key)
				}
			}
			return a
		}
		a := run()
		if len(a.Uploads) > bucket.MaxRecentUploads || len(a.Downloads) > bucket.MaxRecentDownloads {
			return false
		}
		if !slices.IsSortedFunc(a.Uploads, func(x, y bucket.RecentUpload) int { return y.At.Compare(x.At) }) ||
			!slices.IsSortedFunc(a.Downloads, func(x, y bucket.RecentDownload) int { return y.At.Compare(x.At) }) {
			return false
		}
		keys, readers := map[string]bool{}, map[string]bool{}
		for _, u := range a.Uploads {
			if keys[u.Key] || u.Key == "" || bucket.ValidateCreator(u.By) != nil {
				return false
			}
			keys[u.Key] = true
		}
		for _, d := range a.Downloads {
			name := fmt.Sprintf("%s\x00%s\x00%t", d.Key, d.By, d.Presigned)
			if readers[name] || d.Key == "" || d.By == "" || bucket.ValidateCreator(d.By) != nil {
				return false
			}
			readers[name] = true
		}
		// The newest upload, and the newest download, that could be
		// recorded: its time is the time of the first row.
		var lastUpload, lastDownload time.Time
		for _, e := range events {
			recordable := e.key != "" && bucket.ValidateCreator(e.by) == nil
			switch at := minutes(e.minute); {
			case e.upload && recordable && at.After(lastUpload):
				lastUpload = at
			case !e.upload && recordable && e.by != "" && at.After(lastDownload):
				lastDownload = at
			}
		}
		if (len(a.Uploads) == 0) != lastUpload.IsZero() || (len(a.Uploads) > 0 && !a.Uploads[0].At.Equal(lastUpload)) {
			return false
		}
		if (len(a.Downloads) == 0) != lastDownload.IsZero() || (len(a.Downloads) > 0 && !a.Downloads[0].At.Equal(lastDownload)) {
			return false
		}
		return reflect.DeepEqual(a, run())
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 1000}); err != nil {
		t.Error(err)
	}
}

// TestActivityInTheOrderOfALog: when time never goes back - as the time of
// entries never does (ADR-0008) - the record is the last three keys that were
// uploaded and the last three readings, in the order of the log, whatever
// times are equal.
func TestActivityInTheOrderOfALog(t *testing.T) {
	t.Parallel()
	property := func(raw []uint16) bool {
		var a bucket.Activity
		var uploads, downloads []string
		at := 0
		for _, r := range raw {
			at += int(r % 2) // The same time as the entry before, or later.
			key, by := fmt.Sprintf("k%d", r%7), fmt.Sprintf("user/u%d", r%3)
			if r%5 < 2 {
				a = a.Uploaded(minutes(at), by, key)
				uploads = append(slices.DeleteFunc(uploads, func(k string) bool { return k == key }), key)
			} else {
				a = a.Downloaded(minutes(at), by, false, key)
				name := key + " " + by
				downloads = append(slices.DeleteFunc(downloads, func(n string) bool { return n == name }), name)
			}
		}
		slices.Reverse(uploads)
		slices.Reverse(downloads)
		var gotUploads, gotDownloads []string
		for _, u := range a.Uploads {
			gotUploads = append(gotUploads, u.Key)
		}
		for _, d := range a.Downloads {
			gotDownloads = append(gotDownloads, d.Key+" "+d.By)
		}
		return slices.Equal(gotUploads, uploads[:min(len(uploads), bucket.MaxRecentUploads)]) &&
			slices.Equal(gotDownloads, downloads[:min(len(downloads), bucket.MaxRecentDownloads)])
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 1000}); err != nil {
		t.Error(err)
	}
}
