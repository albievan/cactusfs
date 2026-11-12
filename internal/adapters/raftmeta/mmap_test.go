package raftmeta

import (
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// TestEntriesDoNotWaitForASnapshotBeingWritten (L39): a snapshot holds a
// read transaction of the state for as long as it is being written out.
// Entries that grow the state meanwhile - here by 1 MiB of records, and by
// more in the pages the open transaction keeps from reuse, from a state of a
// few pages - must not wait for it to end.
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

	done := make(chan applyResult, 1)
	go func() {
		var last applyResult
		for _, log := range logs {
			if r, ok := f.Apply(log).(applyResult); ok {
				last = r
				if r.err != nil {
					break
				}
			}
		}
		done <- last
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Apply: %v", r.err)
		}
	case <-time.After(10 * time.Second):
		release()
		<-done
		t.Fatal("entries that grew the state waited for the snapshot being written")
	}
}
