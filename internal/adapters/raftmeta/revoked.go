package raftmeta

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"

	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// The record of the nodes that were taken out of the cluster (ADR-0030,
// entry schema 6). A node that leaves is revoked first: from then on no
// member answers its node RPC, admits it to the Raft port or adds it to the
// group again, whatever certificate of the node CA it shows. Nothing takes a
// node off the record; a node that is to come back comes back under a new
// node ID, or the node CA is replaced.
//
// The bucket is made by the first node.revoke that is applied, and the state
// is marked as schema 6 by the same entry: before it, a state of schema 5 is
// the state the release of schema 5 wrote, and after it, that release
// refuses the state rather than misread it.
//
//	revokednodes <node id> -> revokedRecord (JSON)

func revokedBucket() []byte { return []byte("revokednodes") }

// stateSchema6 is the state of ADR-0030: one that may hold revoked nodes.
const stateSchema6 = 6

// revokeNodeCmd revokes a node (schema 6).
type revokeNodeCmd struct {
	ID string `json:"id"`
}

// revokedRecord is a revoked node as it is stored. At is the time of the
// entry that revoked it, as the leader gave it.
type revokedRecord struct {
	ID string    `json:"id"`
	At time.Time `json:"at,omitzero"`
}

// applyRevokeNode records that a node was revoked. Revoking a node that is
// revoked already changes nothing.
func applyRevokeNode(tx *bbolt.Tx, cmd revokeNodeCmd, now applied) (applyResult, error) {
	if err := cluster.ValidateNodeID(cmd.ID); err != nil {
		return applyResult{err: err}, nil
	}
	revoked, err := tx.CreateBucketIfNotExists(revokedBucket())
	if err != nil {
		return applyResult{}, err
	}
	if revoked.Get([]byte(cmd.ID)) != nil {
		return applyResult{}, nil
	}
	data, err := json.Marshal(revokedRecord{ID: cmd.ID, At: now.at})
	if err != nil {
		return applyResult{}, err
	}
	return applyResult{}, revoked.Put([]byte(cmd.ID), data)
}

// isRevoked reports whether the state records the node as revoked.
func isRevoked(tx *bbolt.Tx, id string) bool {
	revoked := tx.Bucket(revokedBucket())
	return revoked != nil && revoked.Get([]byte(id)) != nil
}

// RevokeNode records that a node was taken out of the cluster for good: no
// member answers it again (ADR-0030). It is an entry of schema 6, and while
// a member of an older release remains the error wraps
// domain.ErrUnavailable and nothing is logged.
func (s *Store) RevokeNode(ctx context.Context, id string) error {
	if err := cluster.ValidateNodeID(id); err != nil {
		return fmt.Errorf("raftmeta.RevokeNode: %w", err)
	}
	if _, err := s.propose(ctx, entry{Type: cmdRevokeNode, RevokeNode: &revokeNodeCmd{ID: id}}); err != nil {
		return fmt.Errorf("raftmeta.RevokeNode: %w", err)
	}
	return nil
}

// CanRevoke reports whether a node can be revoked now, as this node knows
// the cluster: the error wraps domain.ErrUnavailable while a member of a
// release before schema 6 remains. Whoever must not take a node out of the
// cluster without revoking it asks first.
func (s *Store) CanRevoke() error {
	return s.use(func() error {
		level, err := s.entryVersion()
		if err != nil {
			return fmt.Errorf("raftmeta.CanRevoke: %w", err)
		}
		if level < entrySchema6 {
			return fmt.Errorf("raftmeta.CanRevoke: %w: the cluster is being upgraded: revoking a node needs metadata version %d, "+
				"and a node that only understands version %d is still a member", domain.ErrUnavailable, entrySchema6, level)
		}
		return nil
	})
}

// NodeRevoked reports whether this node's copy of the state records the node
// as revoked. It asks nobody: it is read for every call of another node, and
// a revocation reaches this node as soon as this node has applied it.
func (s *Store) NodeRevoked(id string) (bool, error) {
	revoked := false
	err := s.use(func() error {
		return s.fsm.view(func(tx *bbolt.Tx) error {
			revoked = isRevoked(tx, id)
			return nil
		})
	})
	if err != nil {
		return false, fmt.Errorf("raftmeta.NodeRevoked: %w", err)
	}
	return revoked, nil
}
