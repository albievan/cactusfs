package app

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/ports"
)

const deploymentIDSize = 16

// AttachedDrive is what the topology needs to know about, and do to, a drive
// that is attached to the node.
type AttachedDrive interface {
	// DriveID returns the drive's generated identifier.
	DriveID() string
	// Empty reports whether the drive holds no shard.
	Empty() bool
	// DeploymentID returns the deployment the drive is claimed for, or "".
	DeploymentID() string
	// Claim durably records that the drive belongs to a deployment.
	Claim(ctx context.Context, deploymentID string) error
}

// TopologyOptions configures EnsureTopology.
type TopologyOptions struct {
	// AllowReplacement confirms that one empty drive may take the place of
	// one missing drive. It is a deliberate act of the operator.
	AllowReplacement bool
	// NodeID is the node the drives are attached to.
	NodeID string
	// Labels say where this node is; they steer where a new deployment
	// puts its erasure sets.
	Labels cluster.Labels
	// Survey is set on a node of a cluster. It returns the drives of every
	// other node, with where those nodes are, when this node is the one to
	// record the topology of a new deployment; otherwise, or while some
	// node cannot be asked, its error wraps domain.ErrUnavailable and the
	// caller tries again later. Nil means the node is on its own.
	Survey func(ctx context.Context) (drives []cluster.DriveState, labels map[string]cluster.Labels, err error)
	// Entropy names a new deployment. It must be cryptographically secure.
	Entropy io.Reader
	Logger  *slog.Logger
}

// EnsureTopology returns the deployment's drive topology, bringing it in line
// with the drives that are attached, and claims every attached drive for the
// deployment.
//
// A new deployment is created only from drives that are all empty and
// unclaimed - on every node, in a cluster: they are assigned to erasure sets,
// spread across nodes, racks and zones, and the assignment is recorded. From then on drives are known by their IDs, so the
// order in which they are attached does not matter, and each carries the
// deployment's ID, so drives and metadata that do not belong together are
// never combined.
//
// When one drive of the topology is gone and one empty drive has appeared,
// the empty drive takes over the slot - if there is parity to rebuild from
// and the operator has confirmed it. Any other difference stops the node.
//
// A node answers for its own drives only. Whether another node's drives are
// there is that node's business when it starts, and a matter of reaching
// them or not while it runs.
func EnsureTopology(
	ctx context.Context, store ports.TopologyStore, drives []AttachedDrive, opts TopologyOptions,
) (cluster.Topology, error) {
	topology, err := ensureTopology(ctx, store, drives, opts)
	if err != nil {
		return cluster.Topology{}, fmt.Errorf("topology.Ensure: %w", err)
	}
	// Claim after the topology is committed: a drive that carries the claim
	// is then always one the metadata knows. A start interrupted here claims
	// the rest next time.
	for _, drive := range drives {
		if err := drive.Claim(ctx, topology.DeploymentID); err != nil {
			return cluster.Topology{}, fmt.Errorf("topology.Ensure: %w", err)
		}
	}
	return topology, nil
}

func ensureTopology(
	ctx context.Context, store ports.TopologyStore, drives []AttachedDrive, opts TopologyOptions,
) (cluster.Topology, error) {
	attached := make([]cluster.DriveState, len(drives))
	for i, drive := range drives {
		attached[i] = cluster.DriveState{
			ID: drive.DriveID(), NodeID: opts.NodeID, Empty: drive.Empty(), DeploymentID: drive.DeploymentID(),
		}
	}

	stored, err := store.GetTopology(ctx)
	if errors.Is(err, domain.ErrNoTopology) {
		all, labels := attached, map[string]cluster.Labels{opts.NodeID: opts.Labels}
		if opts.Survey != nil {
			others, theirs, err := opts.Survey(ctx)
			if err != nil {
				return cluster.Topology{}, err
			}
			all = append(slices.Clone(attached), others...)
			for node, where := range theirs {
				labels[node] = where
			}
		}
		if err := cluster.CheckNewDeployment(all); err != nil {
			return cluster.Topology{}, err
		}
		created, err := cluster.PlanTopology(all, labels)
		if err != nil {
			return cluster.Topology{}, err
		}
		id := make([]byte, deploymentIDSize)
		if _, err := io.ReadFull(opts.Entropy, id); err != nil {
			return cluster.Topology{}, fmt.Errorf("generate deployment ID: %w", err)
		}
		created.DeploymentID = hex.EncodeToString(id)
		if err := store.SetTopology(ctx, created); err != nil {
			return cluster.Topology{}, err
		}
		opts.Logger.InfoContext(ctx, "new deployment: drive topology recorded",
			slog.String("deployment_id", created.DeploymentID),
			slog.Int("erasure_sets", len(created.Sets)), slog.Int("drives_per_set", created.SetSize()),
			slog.Int("nodes", len(created.NodeIDs())), slog.Int("node_failures_tolerated", created.NodeFailuresTolerated()))
		return store.GetTopology(ctx)
	}
	if err != nil {
		return cluster.Topology{}, err
	}
	if stored.DeploymentID == "" {
		return cluster.Topology{}, fmt.Errorf("%w: stored topology has no deployment ID", domain.ErrUnsupportedFormat)
	}

	if stored.Unassigned() && opts.NodeID != "" {
		// Recorded by a release in which a node was always alone and slots
		// did not name their node. The drives are this node's.
		if opts.Survey != nil {
			return cluster.Topology{}, fmt.Errorf("%w: this deployment was created by a node on its own; "+
				"a node on its own cannot be turned into a member of a cluster", domain.ErrInvalidArgument)
		}
		if err := store.SetTopology(ctx, stored.AssignedTo(opts.NodeID)); err != nil {
			return cluster.Topology{}, err
		}
		opts.Logger.InfoContext(ctx, "drive topology migrated: its slots now name this node", slog.String("node_id", opts.NodeID))
		if stored, err = store.GetTopology(ctx); err != nil {
			return cluster.Topology{}, err
		}
	}

	replacement, err := stored.Reconcile(opts.NodeID, attached, opts.AllowReplacement)
	if err != nil {
		return cluster.Topology{}, err
	}
	if replacement == nil {
		return stored, nil
	}
	if err := store.SetTopology(ctx, stored.Replace(*replacement)); err != nil {
		return cluster.Topology{}, err
	}
	opts.Logger.WarnContext(ctx, "an empty drive has replaced a missing drive; its shards will be rebuilt by healing",
		slog.String("missing_drive", replacement.Old), slog.String("new_drive", replacement.New),
		slog.Int("erasure_set", replacement.Position.Set), slog.Int("slot", replacement.Position.Slot))
	return store.GetTopology(ctx)
}
