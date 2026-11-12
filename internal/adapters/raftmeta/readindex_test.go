package raftmeta

import (
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain/bucket"
)

// TestAReadOnTheLeaderSeesWhatAFollowerHasApplied (L38): a follower applies
// an entry once it learns that the entry is committed, which can be before
// the leader's own state machine has applied it. A read on the leader that
// begins after a read on the follower saw the entry must see it too: the
// leader's read index is its commit index, not what its state machine has
// applied.
func TestAReadOnTheLeaderSeesWhatAFollowerHasApplied(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	nodes := []string{"m1", "m2", "m3"}
	c.form(nodes, nil)
	leader := c.leader()
	var follower string
	for _, id := range nodes {
		if id != leader {
			follower = id
			break
		}
	}
	if err := c.store(leader).CreateBucket(ctx, bucket.Bucket{Name: "first"}); err != nil {
		t.Fatal(err)
	}
	// The leader has caught up in its term: no barrier stands in the way.
	if _, err := c.store(leader).GetBucket(ctx, "first"); err != nil {
		t.Fatal(err)
	}

	// The leader's state machine falls behind its log.
	behind := c.store(leader).fsm
	release := make(chan struct{})
	behind.mu.Lock()
	behind.beforeApply = func() { <-release }
	behind.mu.Unlock()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()

	written := make(chan error, 1)
	go func() { written <- c.store(follower).CreateBucket(ctx, bucket.Bucket{Name: "second"}) }()
	eventually(t, "the follower to apply the new bucket", func() bool {
		var there bool
		_ = c.store(follower).fsm.view(func(tx *bbolt.Tx) error {
			there = tx.Bucket(bucketsBucket()).Get([]byte("second")) != nil
			return nil
		})
		return there
	})
	// A read on the follower sees it.
	if _, err := c.store(follower).GetBucket(ctx, "second"); err != nil {
		t.Fatalf("GetBucket on the follower: %v", err)
	}

	read := make(chan error, 1)
	go func() {
		_, err := c.store(leader).GetBucket(ctx, "second")
		read <- err
	}()
	select {
	case err := <-read:
		// The leader's state machine has not applied the bucket: the
		// read cannot have seen it, and must not have answered.
		t.Fatalf("a read on the leader, after the follower's saw the bucket, answered %v before the leader's state machine applied it; want it to wait", err)
	case <-time.After(300 * time.Millisecond):
	}
	released = true
	close(release)
	if err := <-read; err != nil {
		t.Errorf("the read on the leader, once its state machine caught up = %v, want the bucket", err)
	}
	if err := <-written; err != nil {
		t.Errorf("CreateBucket through the follower: %v", err)
	}
}
