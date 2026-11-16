package app_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// slowSite takes a version in one PUT at 128 KiB/s: a link far slower than
// the 1 MiB/s a version was once given time for.
type slowSite struct {
	*porttest.MemSite
}

func (s slowSite) PutReplica(ctx context.Context, _ object.Version, body io.Reader) error {
	buf := make([]byte, 64<<10)
	for {
		n, err := io.ReadFull(body, buf)
		if n > 0 {
			select {
			case <-ctx.Done():
				return delivery.Failed(delivery.ClassTimeout, ctx.Err())
			case <-time.After(500 * time.Millisecond):
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return delivery.Failed(delivery.ClassInternal, err)
		}
	}
}

// TestReplicatorAVersionInOnePutOnASlowLink (PLAN 8.1.4 re-review): a
// version sent in one PUT, which cannot go on from where it was cut off, is
// given time enough on a link far slower than 1 MiB/s: under the deadline
// the worker gives it, it arrives - where the deadline of before (the
// attempt's and the size at 1 MiB/s) would have ended it every time.
func TestReplicatorAVersionInOnePutOnASlowLink(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w := newReplicatorWorld(t, false)
		w.putIn(t, "big", randomBytes(t, 3<<20), object.SSE{})
		item := w.last(t)
		r := w.replicatorWith(slowSite{MemSite: w.site}, 0)
		const base = 10 * time.Second
		deadline := app.ReplicationAttemptTimeout(base, 0, 4)(item)
		before := base + (3<<20/(1<<20)+1)*time.Second
		ctx, cancel := context.WithTimeout(t.Context(), deadline)
		defer cancel()
		start := time.Now()
		status, err := r.Deliver(ctx, w.target, item)
		if err != nil || status != object.ReplicationCompleted {
			t.Fatalf("Deliver = %q, %v under a deadline of %v", status, err, deadline)
		}
		if took := time.Since(start); took <= before {
			t.Errorf("it took %v, within the deadline of before (%v): the link is not slow enough to tell", took, before)
		}
	})
}

// TestReplicatorChangesOnlyItsOwnVersion (PLAN 8.1.4 re-review): a later
// change is made only to the version this deployment sent - the version
// there is asked for first, and one with other bytes under its ID, or none,
// is FAILED with nothing sent; and a retention that ends within the margin
// of a copy is no change to carry.
func TestReplicatorChangesOnlyItsOwnVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := porttest.Root()
	w := newReplicatorWorld(t, false)
	v := w.putIn(t, "k", []byte("version"), object.SSE{})
	if _, err := w.replicator.Deliver(ctx, w.target, w.last(t)); err != nil {
		t.Fatal(err)
	}
	ref := v.Ref()
	in := app.LockInput{Bucket: "b", Key: "k", Version: &ref}

	// Within the margin: passed, nothing sent.
	setRetention(t, w, in, testNow.Add(10*time.Minute), false)
	calls := len(w.site.Calls())
	status, err := w.replicator.Deliver(ctx, w.target, w.last(t))
	if status != "" || delivery.ClassOf(err) != delivery.ClassRejected || len(w.site.Calls()) != calls {
		t.Errorf("a retention within the margin = %q, %v, calls %v", status, err, w.site.Calls()[calls:])
	}

	// Other bytes there under the version's ID.
	w.dest.Tamper("copies", "k", func(v *object.Version) { v.ETag = "0123456789abcdef0123456789abcdef" })
	if _, err := w.objects.SetLegalHold(ctx, root, in, true); err != nil {
		t.Fatal(err)
	}
	calls = len(w.site.Calls())
	status, err = w.replicator.Deliver(ctx, w.target, w.last(t))
	if status != object.ReplicationFailed || delivery.ClassOf(err) != delivery.ClassRejected {
		t.Errorf("a change of a version that is another there = %q, %v", status, err)
	}
	for _, call := range w.site.Calls()[calls:] {
		if strings.HasPrefix(call, "Put") {
			t.Errorf("sent %q to a version that is another there", call)
		}
	}
	if there, err := w.dest.GetVersion(ctx, "copies", "k", ref); err != nil || there.Lock.LegalHold {
		t.Errorf("the version there = %+v, %v; a legal hold was put on it", there.Lock, err)
	}
}

// TestReplicatorHoldsWhereNoLockIsKept (PLAN 8.1.4 re-review): a bucket
// there without Object Lock is not sent a version that carries a lock, nor
// a change that sets one: the target holds, with delivery.ClassObjectLock,
// which an operator can act on. A version without a lock goes, and a change
// that lifts a lock is nothing to carry there.
func TestReplicatorHoldsWhereNoLockIsKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := porttest.Root()
	w := newReplicatorWorldLocked(t, false, false)

	plain := w.putIn(t, "plain", []byte("version"), object.SSE{})
	if status, err := w.replicator.Deliver(ctx, w.target, w.last(t)); err != nil || status != object.ReplicationCompleted {
		t.Fatalf("a version without a lock = %q, %v", status, err)
	}
	ref := plain.Ref()
	in := app.LockInput{Bucket: "b", Key: "plain", Version: &ref}

	held := w.putIn(t, "held", []byte("version"), object.SSE{})
	put := w.last(t)
	heldRef := held.Ref()
	if _, err := w.objects.SetLegalHold(ctx, root, app.LockInput{Bucket: "b", Key: "held", Version: &heldRef}, true); err != nil {
		t.Fatal(err)
	}
	calls := len(w.site.Calls())
	if status, err := w.replicator.Deliver(ctx, w.target, put); status != "" || delivery.ClassOf(err) != delivery.ClassObjectLock {
		t.Errorf("a version with a legal hold = %q, %v; want held as object_lock", status, err)
	}
	if made := w.site.Calls()[calls:]; len(made) != 0 {
		t.Errorf("sent %v", made)
	}

	if _, err := w.objects.SetLegalHold(ctx, root, in, true); err != nil {
		t.Fatal(err)
	}
	on := w.last(t)
	if status, err := w.replicator.Deliver(ctx, w.target, on); status != "" || delivery.ClassOf(err) != delivery.ClassObjectLock {
		t.Errorf("a change that sets a legal hold = %q, %v; want held as object_lock", status, err)
	}
	if _, err := w.objects.SetLegalHold(ctx, root, in, false); err != nil {
		t.Fatal(err)
	}
	off := w.last(t)
	if status, err := w.replicator.Deliver(ctx, w.target, off); status != "" || delivery.ClassOf(err) != delivery.ClassRejected {
		t.Errorf("a change that lifts a legal hold = %q, %v; want passed", status, err)
	}
}

// TestReplicatorForgetsWhatAnotherConnectionKnew (PLAN 8.1.4 re-review):
// the probe of a bucket there and the upload of a version cut off are of a
// target's connection; once the connection changes, the bucket is probed
// again and nothing is resumed at the endpoint the target had.
func TestReplicatorForgetsWhatAnotherConnectionKnew(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newReplicatorWorld(t, false)
	w.putIn(t, "k", []byte("version"), object.SSE{})
	item := w.last(t)
	for range 2 {
		if _, err := w.replicator.Deliver(ctx, w.target, item); err != nil {
			t.Fatal(err)
		}
	}
	if n := w.count("Check"); n != 1 {
		t.Fatalf("probed %d times, want once", n)
	}
	moved := w.target
	moved.Endpoint = "https://site-b-elsewhere.example"
	if _, err := w.replicator.Deliver(ctx, moved, item); err != nil {
		t.Fatal(err)
	}
	if n := w.count("Check"); n != 2 {
		t.Errorf("probed %d times, want again once the connection changed", n)
	}

	// A version of parts cut off through the target as it was is not
	// resumed through the target as it is.
	w.threeParts(t, "movie")
	parts := w.last(t)
	puts := 0
	site := &scriptedSite{MemSite: w.site, part: func() error {
		if puts++; puts == 3 {
			return siteFailure(delivery.ClassConnect)
		}
		return nil
	}}
	r := w.replicatorWith(site, 0)
	if _, err := r.Deliver(ctx, w.target, parts); delivery.ClassOf(err) != delivery.ClassConnect {
		t.Fatalf("the first attempt: %v", err)
	}
	site.part = nil
	if status, err := r.Deliver(ctx, moved, parts); err != nil || status != object.ReplicationCompleted {
		t.Fatalf("through the target as it is: %q, %v", status, err)
	}
	if n := w.count("ListParts"); n != 0 {
		t.Errorf("the upload of the connection before was asked for %d times", n)
	}
}

// TestReplicatorBoundsWhatItRemembers (PLAN 8.1.4 re-review): beyond the
// uploads, or the parts of uploads, it remembers, the oldest upload is
// forgotten and aborted there, as a best effort.
func TestReplicatorBoundsWhatItRemembers(t *testing.T) {
	t.Parallel()
	for name, bounds := range map[string][2]int{"by uploads": {1, 100}, "by parts": {10, 3}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			w := newReplicatorWorld(t, false)
			puts := 0
			site := &scriptedSite{MemSite: w.site, part: func() error {
				if puts++; puts%3 == 0 {
					return siteFailure(delivery.ClassConnect)
				}
				return nil
			}}
			r := w.replicatorWith(site, 0).WithRememberedBounds(bounds[0], bounds[1])
			for _, key := range []string{"movie", "film"} {
				w.threeParts(t, key)
				if _, err := r.Deliver(ctx, w.target, w.last(t)); delivery.ClassOf(err) != delivery.ClassConnect {
					t.Fatalf("%s: %v", key, err)
				}
			}
			if n := w.count("AbortUpload"); n != 1 {
				t.Errorf("%d aborts, want the oldest upload's", n)
			}
			ids, err := w.site.ListReplicaUploads(ctx, "copies", "movie")
			if err != nil || len(ids) != 0 {
				t.Errorf("uploads of the oldest left there: %v, %v", ids, err)
			}
			if ids, err := w.site.ListReplicaUploads(ctx, "copies", "film"); err != nil || len(ids) != 1 {
				t.Errorf("uploads of the newest there: %v, %v", ids, err)
			}
		})
	}
}

// slowSite is a client of another deployment.
var _ ports.SiteClient = slowSite{}
