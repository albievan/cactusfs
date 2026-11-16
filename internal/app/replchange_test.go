package app_test

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// count returns how many calls of the name the site was given.
func (w *replicatorWorld) count(name string) int {
	n := 0
	for _, call := range w.called() {
		if call == name {
			n++
		}
	}
	return n
}

// TestReplicatorResumesParts: an attempt at a version of parts that is cut
// off keeps its upload there; the next asks which parts arrived and sends
// only the others - aborting the other uploads this deployment left of the
// key, not that one. An upload the other deployment no longer has is
// begun again; a part there with another entity tag than the one it was
// given is sent again.
func TestReplicatorResumesParts(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		between func(t *testing.T, w *replicatorWorld)
		// sent are the places of the parts the second attempt sends.
		sent []int
	}{
		"goes on from the parts that arrived": {sent: []int{2}},
		"the upload is gone there": {sent: []int{0, 1, 2}, between: func(t *testing.T, w *replicatorWorld) {
			for _, id := range w.uploadIDs(t) {
				if _, err := w.dest.AbortUpload(context.Background(), "copies", "movie", id); err != nil {
					t.Fatal(err)
				}
			}
		}},
		"a part there is not the one sent": {sent: []int{0, 2}, between: func(t *testing.T, w *replicatorWorld) {
			id := w.uploadIDs(t)[0]
			if _, err := w.dest.PutPart(context.Background(), "copies", "movie", id,
				object.Part{Number: 2, DataID: object.ID{0xee}, Size: object.MinPartSize, ETag: "another", ModifiedAt: testNow}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			w := newReplicatorWorld(t, false)
			v, parts, content := w.threeParts(t, "movie")
			item := w.last(t)
			puts := 0
			site := &scriptedSite{MemSite: w.site, part: func() error {
				if puts++; puts == 3 {
					return siteFailure(delivery.ClassConnect)
				}
				return nil
			}}
			r := w.replicatorWith(site, 0)
			if status, err := r.Deliver(ctx, w.target, item); status != "" || delivery.ClassOf(err) != delivery.ClassConnect {
				t.Fatalf("the first attempt = %q, %v; want held", status, err)
			}
			if left := w.uploadsThere(t); left != 1 {
				t.Fatalf("%d uploads there after the first attempt, want the one kept", left)
			}
			if tt.between != nil {
				tt.between(t, w)
			}
			// What another attempt, cut off before, left behind.
			stale := object.Upload{
				Bucket: "copies", Key: "movie", ID: object.ID{0x5a}, InitiatedAt: testNow, ReplicaFrom: sendingSource,
				Encryption: object.Encryption{KeyMode: object.KeyModePartHKDF},
			}
			if err := w.dest.CreateUpload(ctx, stale); err != nil {
				t.Fatal(err)
			}
			before, parted := w.site.BytesSent(), w.count("PutPart")
			site.part = nil
			status, err := r.Deliver(ctx, w.target, item)
			if err != nil || status != object.ReplicationCompleted {
				t.Fatalf("the second attempt = %q, %v", status, err)
			}
			if got := w.count("PutPart") - parted; got != len(tt.sent) {
				t.Errorf("the second attempt sent %d parts, want %d", got, len(tt.sent))
			}
			var want int64
			for _, i := range tt.sent {
				want += parts[i].Size
			}
			if got := w.site.BytesSent() - before; got != want {
				t.Errorf("the second attempt sent %d bytes, want %d", got, want)
			}
			got, err := w.dest.GetVersion(ctx, "copies", "movie", v.Ref())
			if err != nil || got.Size != int64(len(content)) || got.ETag != v.ETag {
				t.Fatalf("the copy is %+v, %v", got, err)
			}
			if left := w.uploadsThere(t); left != 0 {
				t.Errorf("%d uploads left there", left)
			}
		})
	}
}

// uploadIDs returns the IDs of the uploads in progress there.
func (w *replicatorWorld) uploadIDs(t *testing.T) []object.ID {
	t.Helper()
	ids, err := w.site.ListReplicaUploads(context.Background(), "copies", "movie")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]object.ID, 0, len(ids))
	for _, id := range ids {
		parsed, err := object.ParseID(id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, parsed)
	}
	return out
}

// TestReplicatorLaterChanges (PLAN 8.1.5): a change of the tags, the
// retention or the legal hold of a version is carried to the version there,
// as the version has them now. One the other deployment takes reports no
// status - the version stays as its own row left it; one it refuses for
// itself - Object Lock refusing a retention shortened, a version not there
// - is FAILED, and the target goes on; any other failure holds it. A
// retention that has ended is nothing to carry, and a change of a version
// of a customer's key, which never crossed, is passed.
func TestReplicatorLaterChanges(t *testing.T) {
	t.Parallel()
	root := porttest.Root()
	inOneDay := testNow.Add(24 * time.Hour)
	for name, tt := range map[string]struct {
		// change makes the change at the source.
		change func(t *testing.T, w *replicatorWorld, in app.LockInput)
		site   func(s *scriptedSite)
		sse    object.SSE
		// unsent leaves the version there unsent.
		unsent bool
		status object.ReplicationStatus
		class  delivery.Class
		// call is the call the change is made with, or none.
		call  string
		there func(t *testing.T, v object.Version)
		// after is the status the version has after the acknowledgement.
		after object.ReplicationStatus
	}{
		"tags": {
			change: func(t *testing.T, w *replicatorWorld, in app.LockInput) {
				if _, err := w.objects.SetTags(context.Background(), root, in, []object.Tag{{Key: "stage", Value: "final"}}); err != nil {
					t.Fatal(err)
				}
			},
			call:  "PutTags",
			there: func(t *testing.T, v object.Version) { wantTags(t, v, map[string]string{"stage": "final"}) },
			after: object.ReplicationCompleted,
		},
		"tags removed": {
			change: func(t *testing.T, w *replicatorWorld, in app.LockInput) {
				if _, err := w.objects.SetTags(context.Background(), root, in, nil); err != nil {
					t.Fatal(err)
				}
			},
			call:  "PutTags",
			there: func(t *testing.T, v object.Version) { wantTags(t, v, nil) },
			after: object.ReplicationCompleted,
		},
		"a retention": {
			change: func(t *testing.T, w *replicatorWorld, in app.LockInput) {
				setRetention(t, w, in, inOneDay, false)
			},
			call: "PutRetention",
			there: func(t *testing.T, v object.Version) {
				if r := v.Lock.Retention; r == nil || r.Mode != object.Governance || !r.Until.Equal(inOneDay) {
					t.Errorf("the retention there is %+v", r)
				}
			},
			after: object.ReplicationCompleted,
		},
		"a legal hold": {
			change: func(t *testing.T, w *replicatorWorld, in app.LockInput) {
				if _, err := w.objects.SetLegalHold(context.Background(), root, in, true); err != nil {
					t.Fatal(err)
				}
			},
			call: "PutLegalHold",
			there: func(t *testing.T, v object.Version) {
				if !v.Lock.LegalHold {
					t.Error("no legal hold there")
				}
			},
			after: object.ReplicationCompleted,
		},
		"a retention shortened, which Object Lock refuses there": {
			change: func(t *testing.T, w *replicatorWorld, in app.LockInput) {
				setRetention(t, w, in, inOneDay, false)
				if _, err := w.replicator.Deliver(context.Background(), w.target, w.last(t)); err != nil {
					t.Fatalf("the first retention: %v", err)
				}
				setRetention(t, w, in, testNow.Add(time.Hour), true)
			},
			call: "PutRetention", status: object.ReplicationFailed, class: delivery.ClassRejected,
			there: func(t *testing.T, v object.Version) {
				if r := v.Lock.Retention; r == nil || !r.Until.Equal(inOneDay) {
					t.Errorf("the retention there is %+v, want the longer one kept", r)
				}
			},
			after: object.ReplicationFailed,
		},
		"a version not there": {
			unsent: true,
			change: func(t *testing.T, w *replicatorWorld, in app.LockInput) {
				if _, err := w.objects.SetLegalHold(context.Background(), root, in, true); err != nil {
					t.Fatal(err)
				}
			},
			call: "HeadVersion", status: object.ReplicationFailed, class: delivery.ClassRejected, after: object.ReplicationFailed,
		},
		"a retention that has ended": {
			change: func(t *testing.T, w *replicatorWorld, in app.LockInput) {
				setRetention(t, w, in, testNow.Add(time.Hour), false)
				w.clock.Advance(2 * time.Hour)
			},
			class: delivery.ClassRejected, after: object.ReplicationCompleted,
		},
		"the other side fails": {
			change: func(t *testing.T, w *replicatorWorld, in app.LockInput) {
				if _, err := w.objects.SetTags(context.Background(), root, in, []object.Tag{{Key: "a", Value: "b"}}); err != nil {
					t.Fatal(err)
				}
			},
			site:  func(s *scriptedSite) { s.tags = func() error { return siteFailure(delivery.ClassServer) } },
			class: delivery.ClassServer, after: object.ReplicationCompleted,
		},
		"a version of a customer's key": {
			sse: object.SSE{Mode: object.SSEC, CustomerKey: bytes.Repeat([]byte{7}, 32)},
			change: func(t *testing.T, w *replicatorWorld, in app.LockInput) {
				if _, err := w.objects.SetTags(context.Background(), root, in, []object.Tag{{Key: "a", Value: "b"}}); err != nil {
					t.Fatal(err)
				}
			},
			class: delivery.ClassSSEC, after: object.ReplicationFailed,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			w := newReplicatorWorld(t, false)
			v := w.putIn(t, "k", []byte("version"), tt.sse)
			put := w.last(t)
			if !tt.unsent {
				status, err := w.replicator.Deliver(ctx, w.target, put)
				w.settle(t, put, status)
				if err != nil && tt.sse.Mode == "" {
					t.Fatalf("the version: %v", err)
				}
			}
			ref := v.Ref()
			tt.change(t, w, app.LockInput{Bucket: "b", Key: "k", Version: &ref})
			item := w.last(t)
			if item.Event != delivery.EventTags && item.Event != delivery.EventRetention && item.Event != delivery.EventLegalHold {
				t.Fatalf("the last row is of %q", item.Event)
			}
			site := &scriptedSite{MemSite: w.site}
			if tt.site != nil {
				tt.site(site)
			}
			calls := len(w.site.Calls())
			status, err := w.replicatorWith(site, 0).Deliver(ctx, w.target, item)
			if status != tt.status || delivery.ClassOf(err) != tt.class {
				t.Fatalf("Deliver = %q, %v; want %q, class %q", status, err, tt.status, tt.class)
			}
			made := w.called()[calls:]
			if tt.call == "" && slices.ContainsFunc(made, func(c string) bool { return strings.HasPrefix(c, "Put") }) {
				t.Errorf("sent %v", made)
			}
			if tt.call != "" && !slices.Contains(made, tt.call) {
				t.Errorf("calls %v, want %s", made, tt.call)
			}
			if tt.there != nil {
				got, err := w.dest.GetVersion(ctx, "copies", "k", ref)
				if err != nil {
					t.Fatalf("the version there: %v", err)
				}
				tt.there(t, got)
			}
			if got := w.settle(t, item, status); got != tt.after {
				t.Errorf("the version is %q after the acknowledgement, want %q", got, tt.after)
			}
		})
	}
}

// setRetention gives a version a governance retention until the time given.
func setRetention(t *testing.T, w *replicatorWorld, in app.LockInput, until time.Time, bypass bool) {
	t.Helper()
	if _, err := w.objects.SetRetention(context.Background(), porttest.Root(), in,
		&object.Retention{Mode: object.Governance, Until: until}, bypass); err != nil {
		t.Fatalf("SetRetention: %v", err)
	}
}

// wantTags holds a version's tags to want.
func wantTags(t *testing.T, v object.Version, want map[string]string) {
	t.Helper()
	if len(v.Tags) != len(want) || !maps.Equal(v.Tags, want) {
		t.Errorf("the tags there are %v, want %v", v.Tags, want)
	}
}

// TestReplicatorNamesTheObjectThere: the object of an item, as the delivery
// worker keeps two of one apart, is the target's, the bucket there that the
// configuration names, and the key - whichever bucket here the item is of;
// an item of a bucket whose configuration does not name the target is named
// by nothing, and goes alone.
func TestReplicatorNamesTheObjectThere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newReplicatorWorld(t, false)
	w.putIn(t, "k", []byte("version"), object.SSE{})
	item := w.last(t)
	if name, ok := w.replicator.ObjectOf(ctx, w.target, item); !ok || name != "site-b\x00copies\x00k" {
		t.Errorf("ObjectOf = %q, %v", name, ok)
	}
	other := item
	other.Bucket = "elsewhere"
	if _, ok := w.replicator.ObjectOf(ctx, w.target, other); ok {
		t.Error("an item of a bucket that is not sent to the target is named")
	}
	w.configure(t, bucket.ReplicationConfig{})
	if _, ok := w.replicator.ObjectOf(ctx, w.target, item); ok {
		t.Error("an item of a bucket whose configuration was removed is named")
	}
	if _, ok := app.NewReplicator(app.ReplicatorConfig{}).ObjectOf(ctx, w.target, item); ok {
		t.Error("a replicator with no store names an item")
	}
}

// TestReplicationAttemptTimeout (PLAN 8.1.4 re-review): an attempt at a
// version has a day, or the time its size takes at 64 KiB/s when that is
// longer - or at the rate shared by every version that may be sent at once,
// when that is slower still - besides the deadline of any attempt; a row
// without bytes has that alone, and no size makes the deadline wrap.
func TestReplicationAttemptTimeout(t *testing.T) {
	t.Parallel()
	const base = 70 * time.Second
	day := app.ReplicationLeastAttempt
	for name, tt := range map[string]struct {
		rate    int64
		workers int
		size    int64
		want    time.Duration
	}{
		"no bytes":               {size: 0, want: base},
		"1 GiB has a day":        {size: 1 << 30, want: base + day},
		"10 GiB at 64 KiB/s":     {size: 10 << 30, want: base + (10<<14+1)*time.Second},
		"a rate above the floor": {rate: 1 << 30, workers: 4, size: 10 << 30, want: base + (10<<14+1)*time.Second},
		"a rate shared below it": {rate: 1 << 20, workers: 4, size: 1 << 30, want: base + (1<<20+1)*time.Second},
		"a rate below one byte":  {rate: 3, workers: 8, size: 10, want: base + day},
		"no workers named":       {rate: 1 << 20, size: 1 << 30, want: base + (1<<18+1)*time.Second},
	} {
		if got := app.ReplicationAttemptTimeout(base, tt.rate, tt.workers)(delivery.Item{Size: tt.size}); got != tt.want {
			t.Errorf("%s: %v, want %v", name, got, tt.want)
		}
	}
	if got := app.ReplicationAttemptTimeout(base, 1, 32)(delivery.Item{Size: object.MaxSize}); got <= base+day {
		t.Errorf("5 TiB at a byte a second: %v", got)
	}
}
