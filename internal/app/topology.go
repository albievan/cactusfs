package app

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"

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
	// Entropy names a new deployment. It must be cryptographically secure.
	Entropy io.Reader
	Logger  *slog.Logger
}

// EnsureTopology returns the deployment's drive topology, bringing it in line
// with the drives that are attached, and claims every attached drive for the
// deployment.
//
// A new deployment is created only from drives that are all empty and
// unclaimed: they are assigned to erasure sets in the order given and the
// assignment is recorded. From then on drives are known by their IDs, so the
// order in which they are attached does not matter, and each carries the
// deployment's ID, so drives and metadata that do not belong together are
// never combined.
//
// When one drive of the topology is gone and one empty drive has appeared,
// the empty drive takes over the slot - if there is parity to rebuild from
// and the operator has confirmed it. Any other difference stops the node.
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
	ids := make([]string, len(drives))
	for i, drive := range drives {
		attached[i] = cluster.DriveState{ID: drive.DriveID(), Empty: drive.Empty(), DeploymentID: drive.DeploymentID()}
		ids[i] = drive.DriveID()
	}

	stored, err := store.GetTopology(ctx)
	if errors.Is(err, domain.ErrNoTopology) {
		if err := cluster.CheckNewDeployment(attached); err != nil {
			return cluster.Topology{}, err
		}
		created, err := cluster.NewTopology(ids)
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
			slog.Int("erasure_sets", len(created.Sets)), slog.Int("drives_per_set", created.SetSize()))
		return store.GetTopology(ctx)
	}
	if err != nil {
		return cluster.Topology{}, err
	}
	if stored.DeploymentID == "" {
		return cluster.Topology{}, fmt.Errorf("%w: stored topology has no deployment ID", domain.ErrUnsupportedFormat)
	}

	replacement, err := stored.Reconcile(attached, opts.AllowReplacement)
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
