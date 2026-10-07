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
	// InFlight is the number left alone because they belong to a write that
	// has not been resolved yet.
	InFlight int
}

// Collector removes shards that no object version refers to: what a failed or
// interrupted write, an overwrite or a delete left behind when its own
// cleanup did not happen.
//
// A shard is written and made durable before the version that refers to it is
// committed, so for a while every new shard is unreferenced. Three things keep
// the collector away from it:
//
//   - a write in this process pins its data ID until its version is committed
//     or definitely refused, however long that takes; a write whose outcome is
//     unknown stays pinned until the process ends;
//   - the collector does nothing while the metadata is not ready, because a
//     state machine that has stopped has entries it has not applied, and
//     their data looks unreferenced;
//   - only shards older than the grace period are removed, which covers
//     everything else, such as a clock that is slightly off.
type Collector struct {
	drives   []ports.ShardStore
	index    ports.ObjectIndex
	clock    ports.Clock
	grace    time.Duration
	logger   *slog.Logger
	onPass   func(GCStats)
	ready    func(ctx context.Context) error
	inFlight func(ctx context.Context, dataID object.ID) (bool, error)
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
	// Ready reports whether the metadata can be trusted to answer which data
	// is referenced. A pass is skipped while it returns an error.
	Ready func(ctx context.Context) error
	// InFlight reports whether a data ID belongs to a write that is not
	// resolved yet, on any node. An error means that some node could not
	// be asked, which the collector takes for a yes.
	InFlight func(ctx context.Context, dataID object.ID) (bool, error)
}

// NewCollector returns a Collector.
func NewCollector(cfg CollectorConfig) *Collector {
	c := &Collector{
		drives: cfg.Drives, index: cfg.Index, clock: cfg.Clock, grace: cfg.Grace, logger: cfg.Logger, onPass: cfg.OnPass,
		ready: cfg.Ready, inFlight: cfg.InFlight,
	}
	if c.ready == nil {
		c.ready = func(context.Context) error { return nil }
	}
	if c.inFlight == nil {
		c.inFlight = func(context.Context, object.ID) (bool, error) { return false, nil }
	}
	return c
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
	if err := c.ready(ctx); err != nil {
		return stats, fmt.Errorf("gc.RunOnce: metadata is not ready, nothing is collected: %w", err)
	}
	cutoff := c.clock.Now().Add(-c.grace)
	for _, drive := range c.drives {
		// Shards of one object can sit on several drives of a node;
		// remember the verdict for the data ID seen last on this drive.
		var lastID object.ID
		var last verdict
		var haveLast bool

		err := drive.List(ctx, func(info ports.ShardInfo) error {
			stats.Shards++
			if !haveLast || info.ID.DataID != lastID {
				v, err := c.judge(ctx, info.ID.DataID)
				if err != nil {
					return err
				}
				lastID, last, haveLast = info.ID.DataID, v, true
			}
			switch {
			case last == referenced:
			case last == inFlight:
				stats.InFlight++
			case info.ModifiedAt.After(cutoff):
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

// verdict is what the collector found out about stored data.
type verdict int

const (
	// unreferenced: no version refers to the data and no write is using it.
	unreferenced verdict = iota
	referenced
	inFlight
)

// judge decides whether stored data is in use. The order of the questions
// is what makes the answer safe.
//
// A write pins its data, makes its shards durable, commits its version and
// only then lets go of the pin. So if the data is found not pinned, the
// write is either finished - and a look at the index made afterwards sees
// its version - or was never begun. Looking at the index first and at the
// pins second would leave a gap: a write could commit and let go in between,
// and be found neither referenced nor pinned.
func (c *Collector) judge(ctx context.Context, dataID object.ID) (verdict, error) {
	// Most shards are referenced; that is the cheap question, asked first
	// to spare the expensive one. A "no" here decides nothing yet.
	if used, err := c.index.IsDataReferenced(ctx, dataID); err != nil || used {
		return referenced, err
	}
	busy, err := c.inFlight(ctx, dataID)
	if err != nil {
		c.logger.DebugContext(ctx, "a node could not be asked whether a write is in progress; the data is kept",
			slog.String("data_id", dataID.String()), slog.Any("error", err))
		return inFlight, nil
	}
	if busy {
		return inFlight, nil
	}
	used, err := c.index.IsDataReferenced(ctx, dataID)
	if err != nil || used {
		return referenced, err
	}
	return unreferenced, nil
}
