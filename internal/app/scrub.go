package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// scrubBatch is how many objects are taken from the index at a time, so that
// no index transaction stays open while objects are being verified.
const scrubBatch = 256

// ScrubStats summarises one pass of the scrubber.
type ScrubStats struct {
	// Objects is the number of objects checked.
	Objects int
	// Damaged is the number that had at least one bad shard.
	Damaged int
	// ShardsRebuilt is the number of shards written again.
	ShardsRebuilt int
	// Unrecoverable is the number of objects that could not be rebuilt.
	Unrecoverable int
	// Bytes is the amount of stored data that was verified.
	Bytes int64
}

// Scrubber walks every object, verifies every block of every shard and heals
// what is damaged. It finds bitrot in data nobody reads.
type Scrubber struct {
	buckets ports.BucketStore
	index   ports.ObjectIndex
	healer  *Healer
	logger  *slog.Logger
	// bytesPerSecond limits how fast stored data is read; zero is no limit.
	bytesPerSecond int64
	// pause waits, or returns early when ctx ends.
	pause  func(ctx context.Context, d time.Duration)
	onPass func(ScrubStats)
	active func() bool
}

// ScrubberConfig lists what a Scrubber depends on.
type ScrubberConfig struct {
	Buckets ports.BucketStore
	Index   ports.ObjectIndex
	Healer  *Healer
	Logger  *slog.Logger
	// BytesPerSecond limits how fast stored data is read, so that scrubbing
	// does not starve client traffic. Zero is no limit.
	BytesPerSecond int64
	// OnPass, when not nil, is called after every completed pass.
	OnPass func(ScrubStats)
	// Pause waits for a duration or until the context ends. Nil uses the
	// real clock; tests replace it.
	Pause func(ctx context.Context, d time.Duration)
	// Active, when not nil, says whether this node is the one that scrubs
	// now. In a cluster one node scrubs at a time - the one that leads the
	// metadata group - so that every object is not verified by every node.
	Active func() bool
}

// NewScrubber returns a Scrubber.
func NewScrubber(cfg ScrubberConfig) *Scrubber {
	pause := cfg.Pause
	if pause == nil {
		pause = sleep
	}
	return &Scrubber{
		buckets: cfg.Buckets, index: cfg.Index, healer: cfg.Healer, logger: cfg.Logger,
		bytesPerSecond: cfg.BytesPerSecond, pause: pause, onPass: cfg.OnPass, active: cfg.Active,
	}
}

// sleep waits for d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// Run scrubs everything, waits for the interval and starts again, until ctx
// is cancelled.
func (s *Scrubber) Run(ctx context.Context, interval time.Duration) {
	for ctx.Err() == nil {
		if !s.isActive() {
			// Another node scrubs. Look again soon: this node may be next.
			s.pause(ctx, min(interval, standbyCheck))
			continue
		}
		stats, err := s.RunOnce(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, errStandby):
			s.logger.InfoContext(ctx, "scrub pass handed over: this node no longer leads", slog.Int("objects", stats.Objects))
			continue
		case err != nil:
			s.logger.ErrorContext(ctx, "scrub pass failed", slog.Any("error", err))
		default:
			if s.onPass != nil {
				s.onPass(stats)
			}
			s.logger.InfoContext(ctx, "scrub pass finished",
				slog.Int("objects", stats.Objects), slog.Int("damaged", stats.Damaged),
				slog.Int("shards_rebuilt", stats.ShardsRebuilt), slog.Int("unrecoverable", stats.Unrecoverable),
				slog.Int64("bytes", stats.Bytes))
		}
		s.pause(ctx, interval)
	}
}

// standbyCheck is how often a node that does not scrub looks whether it
// should.
const standbyCheck = 30 * time.Second

var errStandby = errors.New("this node is not the one that scrubs")

func (s *Scrubber) isActive() bool {
	return s.active == nil || s.active()
}

// RunOnce checks every object once.
func (s *Scrubber) RunOnce(ctx context.Context) (ScrubStats, error) {
	var stats ScrubStats
	buckets, err := s.buckets.ListBuckets(ctx)
	if err != nil {
		return stats, fmt.Errorf("scrub.RunOnce: %w", err)
	}
	for _, b := range buckets {
		if err := s.scrubBucket(ctx, b.Name, &stats); err != nil {
			return stats, fmt.Errorf("scrub.RunOnce: %w", err)
		}
	}
	return stats, nil
}

func (s *Scrubber) scrubBucket(ctx context.Context, bucketName string, stats *ScrubStats) error {
	start, skip := "", ""
	for {
		var batch []object.Version
		err := s.index.ScanObjects(ctx, bucketName, start, func(v object.Version) ports.ScanControl {
			if v.Key == skip {
				return ports.ScanControl{}
			}
			batch = append(batch, v)
			return ports.ScanControl{Stop: len(batch) == scrubBatch}
		})
		if errors.Is(err, domain.ErrBucketNotFound) {
			return nil // deleted while scrubbing
		}
		if err != nil {
			return err
		}
		for _, version := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !s.isActive() {
				return errStandby
			}
			s.scrubObject(ctx, version, stats)
		}
		if len(batch) < scrubBatch {
			return nil
		}
		start, skip = batch[len(batch)-1].Key, batch[len(batch)-1].Key
	}
}

func (s *Scrubber) scrubObject(ctx context.Context, version object.Version, stats *ScrubStats) {
	result, err := s.healer.heal(ctx, version.Bucket, version.Key, s.throttle)
	stats.Objects++
	stats.ShardsRebuilt += result.Rebuilt
	stats.Bytes += result.Verified
	if result.Damaged > 0 {
		stats.Damaged++
	}
	if result.Unrecoverable {
		stats.Unrecoverable++
	}
	if err != nil && !result.Unrecoverable && ctx.Err() == nil {
		s.logger.ErrorContext(ctx, "scrub could not check an object", slog.String("bucket", version.Bucket), slog.Any("error", err))
	}
}

// throttle keeps the scrubber to its byte rate. It is called after every
// piece read, so that a large object is slowed down as it is read, not after.
func (s *Scrubber) throttle(ctx context.Context, bytes int64) {
	if s.bytesPerSecond > 0 && bytes > 0 {
		s.pause(ctx, time.Duration(float64(bytes)/float64(s.bytesPerSecond)*float64(time.Second)))
	}
}
