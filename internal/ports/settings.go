package ports

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// NodeSettingsReader asks another node of the cluster how it was configured.
// Nothing is kept of the answer: every call asks the node again.
type NodeSettingsReader interface {
	// NodeSettings returns what the node says of itself, in the form of
	// cluster.NodeSettings.Normalized: an implementation redacts what it
	// received, whatever the node sent, and bounds it. The drives carry
	// cluster.ErasureSetUnknown: which set a drive is in is for whoever
	// holds the topology to say.
	//
	// An error says why there is no answer, and wraps
	//
	//   - domain.ErrNotSupported when the node is of a release that cannot
	//     be asked;
	//   - context.DeadlineExceeded when the node was reached and did not
	//     answer in the time ctx gives;
	//   - domain.ErrUnavailable when the node could not be reached;
	//
	// and none of these when the node was reached and could not answer.
	// The call returns when ctx ends, whatever the node does.
	NodeSettings(ctx context.Context, nodeID string) (cluster.NodeSettings, error)
}

// LocalSettings returns how this node was configured: its settings as
// cluster.NewSetting made them, its drives, its certificates and its build.
// It asks nobody - it is what the node answers a NodeSettingsReader of
// another node with, and what it shows of itself.
type LocalSettings func(ctx context.Context) (cluster.NodeSettings, error)
