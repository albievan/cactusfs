package noderpc_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/noderpc"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// revocations is the record of revoked nodes a server is given.
type revocations struct {
	mu      sync.Mutex
	revoked map[string]bool
	err     error
}

func (r *revocations) Revoked(nodeID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revoked[nodeID], r.err
}

func (r *revocations) revoke(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revoked[nodeID] = true
}

// TestARevokedNodeIsServedNothing: a node taken out of the cluster keeps a
// certificate of the node CA (ADR-0030). From the moment the record of
// revoked nodes names it, nothing it asks is served - no proposal, no shard,
// nothing about the node, and no Join to come back - while the other nodes are served
// as before. A record that cannot be read refuses the call.
func TestARevokedNodeIsServedNothing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p := newPKI(t)
	record := &revocations{revoked: map[string]bool{}}
	node := serveWith(t, p, "meta-1", func(c *noderpc.ServerConfig) { c.Revoked = record.Revoked }, porttest.NewMemShardStore("d1"))
	addresses := map[string]string{"meta-1": node.address}
	leaving := clientFor(t, p.identity("gateway-1"), addresses)
	staying := clientFor(t, p.identity("gateway-2"), addresses)

	// Before: served.
	if _, err := leaving.Propose(ctx, "meta-1", []byte("register gateway-1")); err != nil {
		t.Fatalf("Propose before the node was revoked: %v", err)
	}
	record.revoke("gateway-1")

	calls := map[string]func() error{
		"Propose": func() error {
			_, err := leaving.Propose(ctx, "meta-1", []byte("register gateway-1"))
			return err
		},
		"Term": func() error {
			_, err := leaving.Term(ctx, "meta-1")
			return err
		},
		"Join": func() error { return leaving.Join(ctx, "meta-1", "gateway-1", "gateway-1@g:9003", false) },
		"Leave": func() error {
			return leaving.Leave(ctx, "meta-1", "gateway-1")
		},
		"Info": func() error {
			_, err := leaving.Info(ctx, "meta-1", node.address)
			return err
		},
		"InFlight": func() error {
			_, err := leaving.InFlight(ctx, "meta-1", shard(1, 0).DataID)
			return err
		},
		"WriteShard": func() error {
			_, err := leaving.Drive("meta-1", "d1").Create(ctx, shard(1, 0))
			return err
		},
		"ReadShard": func() error {
			_, err := leaving.Drive("meta-1", "d1").Open(ctx, shard(1, 0))
			return err
		},
		"DeleteShard": func() error { return leaving.Drive("meta-1", "d1").Delete(ctx, shard(1, 0)) },
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s of a revoked node was served", name)
		}
	}
	if len(node.meta.proposed) != 1 || len(node.meta.joined) != 0 || len(node.meta.left) != 0 {
		t.Errorf("a revoked node reached the metadata store: proposed %q, joined %v, left %v",
			node.meta.proposed, node.meta.joined, node.meta.left)
	}

	// Another node is served as before.
	if _, err := staying.Term(ctx, "meta-1"); err != nil {
		t.Errorf("a node that was not revoked: %v", err)
	}
	if _, err := staying.Info(ctx, "meta-1", node.address); err != nil {
		t.Errorf("a node that was not revoked asks for Info: %v", err)
	}

	// A record that cannot be read is no reason to serve anybody.
	record.mu.Lock()
	record.err = errors.New("the state cannot be read")
	record.mu.Unlock()
	if _, err := staying.Term(ctx, "meta-1"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Term while the record of revoked nodes cannot be read = %v, want it refused as unavailable", err)
	}
}
