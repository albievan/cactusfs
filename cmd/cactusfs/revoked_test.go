package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// TestANodeThatLeftIsRefusedEverywhere: a node that leaves the cluster is
// revoked (ADR-0030). Its certificate of the node CA stays valid, and with
// it, afterwards, no member serves its node RPC, the leader does not let it
// join again, and the Raft port does not admit it; the nodes that stay are
// served as before.
func TestANodeThatLeftIsRefusedEverywhere(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	roles := fourNodes()
	roles["g5"] = "gateway"
	c := newTestCluster(t, roles)
	c.start(c.all()...)
	leaving := c.node("g5")

	// It leaves through the leader, as itself.
	deadline := time.Now().Add(time.Minute)
	for left := false; !left; {
		for _, id := range []string{"n1", "n2", "n3"} {
			err := leaving.rpc.Leave(ctx, id, "g5")
			if err == nil {
				left = true
				break
			}
			if !errors.Is(err, domain.ErrNotLeader) && !errors.Is(err, domain.ErrUnavailable) {
				t.Fatalf("Leave through %s: %v", id, err)
			}
		}
		if !left && time.Now().After(deadline) {
			t.Fatal("g5 could not leave the cluster")
		}
	}

	stayed := []string{"n1", "n2", "n3", "n4"}
	for _, id := range stayed {
		n := c.node(id)
		for {
			revoked, err := n.revoked("g5")
			if err == nil && revoked {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s does not know that g5 left: %v, %v", id, revoked, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if revoked, err := n.revoked("n4"); err != nil || revoked {
			t.Errorf("%s says n4 was revoked: %v, %v", id, revoked, err)
		}
		if !n.everyMemberReadsWrapVersion2() {
			t.Errorf("%s: a cluster of this release keeps wrapping keys in version 1", id)
		}
		if admitted, err := n.mayReplicate(c.configs[id], "g5"); err != nil || admitted {
			t.Errorf("the Raft port of %s admits g5: %v, %v", id, admitted, err)
		}
	}
	for _, id := range stayed {
		// (The client tells a call that was not served as a node out of
		// reach; the server said why.)
		if _, err := leaving.rpc.Term(ctx, id); err == nil || !strings.Contains(err.Error(), "node/g5 may not cluster:NodeRPC") {
			t.Errorf("node RPC of g5 to %s = %v, want it refused by the authorizer", id, err)
		}
		if err := leaving.rpc.Join(ctx, id, "g5", c.node("g5").self(c.configs["g5"]).RaftAddr, false); err == nil {
			t.Errorf("g5 joined again through %s", id)
		}
	}
	// The leader itself refuses to add it, whoever asks.
	for _, id := range []string{"n1", "n2", "n3"} {
		if !c.node(id).meta.IsLeader() {
			continue
		}
		err := metaRPC{node: c.node(id)}.Join(ctx, "g5", c.node("g5").self(c.configs["g5"]).RaftAddr, false)
		if !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("the leader adds g5 again: %v", err)
		}
	}
	// The others are served as before.
	if _, err := c.node("n4").rpc.Term(ctx, "n1"); err != nil {
		t.Errorf("node RPC of n4 after g5 left: %v", err)
	}
	if members, err := c.node("n1").meta.Members(); err != nil {
		t.Fatal(err)
	} else {
		for _, member := range members {
			if member.ID == "g5" {
				t.Errorf("g5 is still a member of the metadata group: %+v", members)
			}
		}
	}
}
