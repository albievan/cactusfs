package raftmeta

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"

	"github.com/albievan/cactusfs/internal/domain/bucket"
)

// TestAStoppedStateMachineTakesNoSnapshot (M36): after an entry stopped the
// state machine, Raft goes on handing it entries and would label a snapshot
// with the index of the last of them. The snapshot would hold the state from
// before the entry that failed, and a restart would skip every entry up to
// its label.
func TestAStoppedStateMachineTakesNoSnapshot(t *testing.T) {
	t.Parallel()
	f := newFSM(t)
	if r := result(t, f.Apply(command(t, 1, createBucket("bkt")))); r.err != nil {
		t.Fatalf("Apply: %v", r.err)
	}
	if r := result(t, f.Apply(command(t, 2, createBucket("")))); !r.fatal {
		t.Fatalf("Apply of a bucket of no name = %+v, want the state machine stopped", r)
	}
	if r := result(t, f.Apply(command(t, 3, createBucket("later")))); !r.fatal {
		t.Fatalf("Apply after the state machine stopped = %+v, want it refused as fatal", r)
	}
	snapshot, err := f.Snapshot()
	if err == nil {
		snapshot.Release()
		t.Fatal("Snapshot of a stopped state machine succeeded; want it refused")
	}
	if !errors.Is(err, f.health()) {
		t.Errorf("Snapshot = %v, want the error that stopped the state machine", err)
	}
}

// TestASnapshotThatClaimsMoreThanItHoldsIsMadeUpFromTheLog (M36): a snapshot
// taken by a stopped state machine before the fix holds less than its index
// claims. At a start the state is brought up to the snapshot, and the
// commands up to its index that the log still holds are applied: none is
// skipped.
func TestASnapshotThatClaimsMoreThanItHoldsIsMadeUpFromTheLog(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()
	store := openStore(t, dir)
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "one", CreatedAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	// The state as a stopped state machine would have kept it.
	held, err := store.fsm.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var old bytes.Buffer
	if _, err := held.(*snapshot).tx.WriteTo(&old); err != nil {
		t.Fatal(err)
	}
	held.Release()
	for _, name := range []string{"two", "three"} {
		if err := store.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: time.Unix(1, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	index, term := store.raft.LastIndex(), store.raft.CurrentTerm()
	future := store.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		t.Fatal(err)
	}
	configuration := future.Configuration()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// A snapshot labelled with the last index that holds the old state,
	// and the state database as old as it.
	snapshots, err := raft.NewFileSnapshotStoreWithLogger(dir, retainedSnapshots, hclog.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}
	_, transport := raft.NewInmemTransport("")
	sink, err := snapshots.Create(raft.SnapshotVersionMax, index, term, configuration, 1, transport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write(old.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFile), old.Bytes(), stateFileMode); err != nil {
		t.Fatal(err)
	}

	reopened := openStore(t, dir)
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	buckets, err := reopened.ListBuckets(readCtx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range buckets {
		names = append(names, b.Name)
	}
	if len(names) != 3 {
		t.Errorf("buckets after the start = %v, want one, three and two: no entry the snapshot claimed may be skipped", names)
	}
}

// TestARestoreThatFailsAfterTheStateWasClosedStopsTheStateMachine (L37): once
// Restore has closed the state, a failure leaves the state machine without
// one. It must stop - be unhealthy and tell onFail - not stay half alive and
// be counted ready.
func TestARestoreThatFailsAfterTheStateWasClosedStopsTheStateMachine(t *testing.T) {
	t.Parallel()
	source := newFSM(t)
	if r := result(t, source.Apply(command(t, 1, createBucket("bkt")))); r.err != nil {
		t.Fatalf("Apply: %v", r.err)
	}
	taken, err := source.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var image bytes.Buffer
	if _, err := taken.(*snapshot).tx.WriteTo(&image); err != nil {
		t.Fatal(err)
	}
	taken.Release()

	cases := map[string]func(f *fsm) func(){
		"the snapshot cannot take the place of the state": func(f *fsm) func() {
			return func() { _ = f.root.Remove(restoreFile) }
		},
		"the snapshot in place of the state cannot be opened": func(f *fsm) func() {
			return func() { _ = f.root.WriteFile(restoreFile, []byte("not a database"), stateFileMode) }
		},
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFSM(t)
			var told []error
			f.mu.Lock()
			f.onFail = func(err error) { told = append(told, err) }
			f.beforeSwap = fail(f)
			f.mu.Unlock()
			if err := f.Restore(io.NopCloser(bytes.NewReader(image.Bytes()))); err == nil {
				t.Fatal("Restore succeeded; want it to fail")
			}
			if f.health() == nil {
				t.Error("the state machine is healthy without a state; want it stopped")
			}
			if len(told) != 1 {
				t.Errorf("onFail was told %d times, want once", len(told))
			}
		})
	}
}
