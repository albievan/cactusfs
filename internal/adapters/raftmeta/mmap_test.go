package raftmeta

import (
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"go.etcd.io/bbolt"
)

// TestEntriesDoNotWaitForASnapshotBeingWritten (L39): a snapshot holds a
// read transaction of the state for as long as it is being written out.
// Entries that grow the state meanwhile - here by 1 MiB of records, and by
// more in the pages the open transaction keeps from reuse, from a state of a
// few pages - must not wait for it to end.
//
// The only thing in bbolt that waits for a read transaction is mapping the
// file anew, so the test says that: the state grew, its map did not move,
// and no entry was stuck behind the snapshot. It does not time the entries:
// each costs an fsync, which a loaded host makes as slow as it likes (the
// first version of this test bounded the whole run at 10 s and took 12 s
// under -race).
func TestEntriesDoNotWaitForASnapshotBeingWritten(t *testing.T) {
	t.Parallel()
	f := newFSM(t)
	if r := result(t, f.Apply(command(t, 1, createBucket("bkt")))); r.err != nil {
		t.Fatalf("Apply: %v", r.err)
	}
	pad := strings.Repeat("p", 8<<10)
	var logs []*raft.Log
	for i := range 128 {
		logs = append(logs, command(t, uint64(2+i), entry{Type: cmdAppendAudit, IAM: &iamCmd{Audit: &auditRecord{
			Time: time.Unix(int64(1+i), 0).UTC(), Actor: "tester", Action: "admin:Test", Outcome: "succeeded",
			Details: map[string]string{"pad": pad},
		}}}))
	}
	mapped := f.db.Info().Data
	sizeBefore := stateSize(t, f)

	// What Persist holds while it writes.
	held, err := f.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			held.Release()
		}
	}
	defer release()

	// Each entry reports when it is applied. One that waits for the
	// snapshot waits until it is released - for ever, here - so an entry
	// that makes no progress for a long while is one that waits.
	applied := make(chan applyResult, len(logs))
	go func() {
		for _, log := range logs {
			r, _ := f.Apply(log).(applyResult)
			applied <- r
			if r.err != nil {
				return
			}
		}
	}()
	const stuck = time.Minute
	for i := range logs {
		select {
		case r := <-applied:
			if r.err != nil {
				t.Fatalf("Apply of entry %d: %v", i, r.err)
			}
		case <-time.After(stuck):
			release()
			t.Fatalf("entry %d of %d waited %v for the snapshot being written", i, len(logs), stuck)
		}
	}

	// Every entry was applied while the snapshot was held: the state grew,
	// and in the map it had.
	if grown := stateSize(t, f); grown <= sizeBefore {
		t.Errorf("the state did not grow (%d bytes, then %d): the test proves nothing", sizeBefore, grown)
	}
	if moved := f.db.Info().Data; moved != mapped {
		t.Error("the state was mapped anew while a snapshot held a read transaction")
	}
}

// stateSize returns how much of the state database's file is in use: the
// file itself is as large as its map on Windows.
func stateSize(t *testing.T, f *fsm) int64 {
	t.Helper()
	var size int64
	if err := f.db.View(func(tx *bbolt.Tx) error {
		size = tx.Size()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return size
}
