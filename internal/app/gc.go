package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// GCStats summarises one pass of the collector.
type GCStats struct {
	// Shards is the number of shards looked at.
	Shards int
	// Removed is the number of unreferenced shards deleted.
	Removed int
	// Young is the number of unreferenced shards left alone because they are
	// not older than the grace period.
	Young int
}

// Collector removes shards that no object version refers to: what a failed or
// interrupted write, an overwrite or a delete left behind when its own
// cleanup did not happen.
//
// A shard is written and made durable before the version that refers to it is
// committed, so for a moment every new shard is unreferenced. The grace
// period protects that window: only shards older than it are removed. It must
// be much longer than any write can take between making its shards durable
// and committing.
type Collector struct {
	drives []ports.ShardStore
	index  ports.ObjectIndex
	clock  ports.Clock
	grace  time.Duration
	logger *slog.Logger
	onPass func(GCStats)
}

// CollectorConfig lists what a Collector depends on.
type CollectorConfig struct {
	Drives []ports.ShardStore
	Index  ports.ObjectIndex
	Clock  ports.Clock
	// Grace is the minimum age of a shard before it may be removed.
	Grace  time.Duration
	Logger *slog.Logger
	// OnPass, when not nil, is called after every completed pass.
	OnPass func(GCStats)
}

// NewCollector returns a Collector.
func NewCollector(cfg CollectorConfig) *Collector {
	return &Collector{
		drives: cfg.Drives, index: cfg.Index, clock: cfg.Clock, grace: cfg.Grace, logger: cfg.Logger, onPass: cfg.OnPass,
	}
}

// Run collects, waits for the interval and starts again, until ctx is
// cancelled.
func (c *Collector) Run(ctx context.Context, interval time.Duration) {
	for ctx.Err() == nil {
		stats, err := c.RunOnce(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			c.logger.ErrorContext(ctx, "garbage collection failed", slog.Any("error", err))
		default:
			if c.onPass != nil {
				c.onPass(stats)
			}
			if stats.Removed > 0 {
				c.logger.InfoContext(ctx, "unreferenced shards removed",
					slog.Int("removed", stats.Removed), slog.Int("shards", stats.Shards), slog.Int("young", stats.Young))
			}
		}
		sleep(ctx, interval)
	}
}

// RunOnce looks at every shard on every drive once.
func (c *Collector) RunOnce(ctx context.Context) (GCStats, error) {
	var stats GCStats
	cutoff := c.clock.Now().Add(-c.grace)
	for _, drive := range c.drives {
		// Shards of one object sit on several drives; remember the answer
		// for the data ID seen last on this drive.
		var lastID object.ID
		var lastReferenced, haveLast bool

		err := drive.List(ctx, func(info ports.ShardInfo) error {
			stats.Shards++
			if !haveLast || info.ID.DataID != lastID {
				referenced, err := c.index.IsDataReferenced(ctx, info.ID.DataID)
				if err != nil {
					return err
				}
				lastID, lastReferenced, haveLast = info.ID.DataID, referenced, true
			}
			switch {
			case lastReferenced:
			case info.CommittedAt.After(cutoff):
				stats.Young++
			default:
				if err := drive.Delete(ctx, info.ID); err != nil {
					c.logger.WarnContext(ctx, "unreferenced shard cannot be removed",
						slog.String("shard", info.ID.String()), slog.String("drive", drive.DriveID()), slog.Any("error", err))
					return nil
				}
				stats.Removed++
			}
			return nil
		})
		if err != nil {
			return stats, fmt.Errorf("gc.RunOnce: drive %s: %w", drive.DriveID(), err)
		}
	}
	return stats, nil
}
