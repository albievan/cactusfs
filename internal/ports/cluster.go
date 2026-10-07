package ports

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// NodeRegistry is the replicated record of the nodes of the cluster: where
// they are reached and what they do.
type NodeRegistry interface {
	// SetNode records a node, replacing what was recorded under its ID.
	SetNode(ctx context.Context, n cluster.Node) error
	// RemoveNode forgets a node. Removing a node that is not recorded is
	// not an error.
	RemoveNode(ctx context.Context, id string) error
	// ListNodes returns every recorded node, ordered by ID.
	ListNodes(ctx context.Context) ([]cluster.Node, error)
}

// ClusterView is what this node knows about the cluster without asking
// anybody: its own copy of the replicated state, which may be a moment
// behind. It is for finding drives and nodes, where a stale answer costs a
// retry and a fresh one would cost a round trip on every request; nothing
// that must be exact is decided on it.
type ClusterView interface {
	// Topology returns the drive topology. The error wraps
	// domain.ErrNoTopology when none has been set.
	Topology(ctx context.Context) (cluster.Topology, error)
	// Nodes returns the recorded nodes, ordered by ID.
	Nodes(ctx context.Context) ([]cluster.Node, error)
}

// DriveSource finds the drives objects are stored on, wherever they are.
type DriveSource interface {
	// Topology returns the current drive topology, as this node knows it.
	Topology(ctx context.Context) (cluster.Topology, error)
	// Drive returns the store of the drive in a slot of the topology: a
	// drive of this node, or a connection to the node that has it. It
	// reports false when the drive is one of this node's and is not
	// attached.
	Drive(slot cluster.Slot) (ShardStore, bool)
}
