package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/ports"
)

// EnsureTopology returns the deployment's drive topology, bringing it in line
// with the drives that are attached.
//
// On a new deployment the attached drives are assigned to erasure sets, in
// the order given, and the assignment is recorded. From then on drives are
// known by their IDs, so the order in which they are attached does not
// matter. When one drive of the topology is gone and one empty drive has
// appeared, the empty drive takes over the slot; its contents are rebuilt by
// healing. Any other difference stops the node: it needs a person to decide.
func EnsureTopology(
	ctx context.Context, store ports.TopologyStore, attached []cluster.DriveState, logger *slog.Logger,
) (cluster.Topology, error) {
	stored, err := store.GetTopology(ctx)
	if errors.Is(err, domain.ErrNoTopology) {
		ids := make([]string, len(attached))
		for i, drive := range attached {
			ids[i] = drive.ID
		}
		created, err := cluster.NewTopology(ids)
		if err != nil {
			return cluster.Topology{}, fmt.Errorf("topology.Ensure: %w", err)
		}
		if err := store.SetTopology(ctx, created); err != nil {
			return cluster.Topology{}, fmt.Errorf("topology.Ensure: %w", err)
		}
		logger.InfoContext(ctx, "drive topology recorded",
			slog.Int("erasure_sets", len(created.Sets)), slog.Int("drives_per_set", created.SetSize()))
		return reread(ctx, store)
	}
	if err != nil {
		return cluster.Topology{}, fmt.Errorf("topology.Ensure: %w", err)
	}

	replacement, err := stored.Reconcile(attached)
	if err != nil {
		return cluster.Topology{}, fmt.Errorf("topology.Ensure: %w", err)
	}
	if replacement == nil {
		return stored, nil
	}
	if err := store.SetTopology(ctx, stored.Replace(*replacement)); err != nil {
		return cluster.Topology{}, fmt.Errorf("topology.Ensure: %w", err)
	}
	logger.WarnContext(ctx, "an empty drive has replaced a missing drive; its shards will be rebuilt by healing",
		slog.String("missing_drive", replacement.Old), slog.String("new_drive", replacement.New),
		slog.Int("erasure_set", replacement.Position.Set), slog.Int("slot", replacement.Position.Slot))
	return reread(ctx, store)
}

func reread(ctx context.Context, store ports.TopologyStore) (cluster.Topology, error) {
	t, err := store.GetTopology(ctx)
	if err != nil {
		return cluster.Topology{}, fmt.Errorf("topology.Ensure: %w", err)
	}
	return t, nil
}
