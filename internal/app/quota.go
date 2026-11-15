package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// The pace of the walk the counter proposes (ADR-0023): entries of
// UsageWalkLimit records, one every UsageWalkInterval. An entry is applied
// by every member in one transaction, in which nothing else changes; this
// many records hold changes up for a few milliseconds.
const (
	// UsageWalkLimit is how many records one entry of the walk visits.
	UsageWalkLimit = 10_000
	// UsageWalkInterval is the time between two entries of the walk.
	UsageWalkInterval = 100 * time.Millisecond
)

// usageLookEvery is how many intervals pass between two looks at every
// bucket while nothing is being counted: ten seconds. A look reads the
// record of every bucket from this node's copy of the state; it finds the
// buckets that are older than the record, a recount asked for through
// another node, and the recount a clamp began (ADR-0024).
const usageLookEvery = 100

// UsageCounter has the usage of buckets counted (ADR-0023, ADR-0024). The
// metadata store keeps the record of a bucket by itself, from every change,
// once it has one that is exact; a bucket that is older than the record,
// and a record that is being counted again, are brought there by a walk
// that is spread over many entries of the log. The counter proposes those
// entries, one every UsageWalkInterval, on the node that leads the metadata
// group only, for the buckets in turn - one entry for each, round and round
// - so that a very large bucket holds up neither the others nor a recount:
// the walk is in the record, so a new leader goes on from where the last
// one was.
//
// It also holds the use case that asks for a recount (Recount).
type UsageCounter struct {
	buckets ports.BucketStore
	usage   ports.BucketUsageStore
	authz   ports.Authorizer
	auditor *Auditor
	logger  *slog.Logger
	active  func() bool
	onDrift func()

	// mu guards what follows: queue are the buckets still to be walked,
	// the first walked next; idle counts the intervals since the last look
	// at every bucket.
	mu    sync.Mutex
	queue []string
	idle  int
}

// UsageCounterConfig lists what a UsageCounter depends on.
type UsageCounterConfig struct {
	Buckets ports.BucketStore
	Usage   ports.BucketUsageStore
	Authz   ports.Authorizer
	// Auditor records every recount asked for.
	Auditor *Auditor
	Logger  *slog.Logger
	// Active, when not nil, says whether this node is the one that counts
	// now: in a cluster, the one that leads the metadata group.
	Active func() bool
	// OnDrift, when not nil, is called for every recount that ended with
	// other totals than the record held: a fault of whoever kept the
	// record (cactusfs_bucket_usage_drift_total).
	OnDrift func()
}

// NewUsageCounter returns a UsageCounter.
func NewUsageCounter(cfg UsageCounterConfig) *UsageCounter {
	return &UsageCounter{
		buckets: cfg.Buckets, usage: cfg.Usage, authz: cfg.Authz, auditor: cfg.Auditor, logger: cfg.Logger,
		active: cfg.Active, onDrift: cfg.OnDrift,
		// The first interval looks at every bucket.
		idle: usageLookEvery,
	}
}

// Run proposes the entries of the walk, one every UsageWalkInterval, until
// ctx is cancelled.
func (c *UsageCounter) Run(ctx context.Context) {
	ticker := time.NewTicker(UsageWalkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.step(ctx)
		}
	}
}

func (c *UsageCounter) isActive() bool {
	return c.active == nil || c.active()
}

// step is one interval of the counter: on the node that leads, at most one
// entry of the walk, for the first bucket that needs one.
func (c *UsageCounter) step(ctx context.Context) {
	if !c.isActive() {
		// Another node counts. What was to be counted here is looked up
		// again when this node leads: the leader may have finished it.
		c.mu.Lock()
		c.queue, c.idle = nil, usageLookEvery
		c.mu.Unlock()
		return
	}
	name, ok := c.next(ctx)
	if !ok {
		return
	}
	walk, err := c.usage.WalkBucketUsage(ctx, name, UsageWalkLimit, false)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return
	case errors.Is(err, domain.ErrBucketNotFound):
		c.done(name)
		return
	case errors.Is(err, domain.ErrUnavailable), errors.Is(err, domain.ErrNotLeader):
		// Below level 5 nothing is counted yet, and a node that no
		// longer leads proposes nothing: look again in a while.
		c.rest()
		return
	default:
		// The bucket is looked at again with every other: a fault of one
		// does not hold up the rest, nor fill the log every interval.
		c.logger.WarnContext(ctx, "an entry of the walk that counts a bucket's usage failed", slog.String("bucket", name), slog.Any("error", err))
		c.done(name)
		return
	}
	c.drifted(ctx, name, walk)
	if walk.Usage.Walking() {
		c.rotate(name)
		return
	}
	c.done(name)
}

// next returns the bucket to walk now. When the time for a look has come,
// it looks at every bucket first, and queues those that are not queued yet
// behind those that are: a look finds what a walk under way does not - a
// recount asked for through another node, one a clamp began.
func (c *UsageCounter) next(ctx context.Context) (string, bool) {
	c.mu.Lock()
	c.idle++
	look := c.idle >= usageLookEvery
	c.mu.Unlock()
	if look {
		names, err := c.uncounted(ctx)
		c.mu.Lock()
		c.idle = 0
		c.mu.Unlock()
		switch {
		case err != nil && ctx.Err() == nil:
			c.logger.WarnContext(ctx, "the usage of buckets could not be read", slog.Any("error", err))
		case err == nil:
			for _, name := range names {
				c.enqueue(name)
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
		return "", false
	}
	return c.queue[0], true
}

// uncounted returns, in name order, the buckets whose record is not exact
// or is being counted, from this node's copy of the state.
func (c *UsageCounter) uncounted(ctx context.Context) ([]string, error) {
	list, err := c.buckets.ListBuckets(ctx)
	if err != nil {
		return nil, fmt.Errorf("usage.uncounted: %w", err)
	}
	names := make([]string, len(list))
	for i, b := range list {
		names[i] = b.Name
	}
	var out []string
	for chunk := range slices.Chunk(names, ports.MaxBucketUsages) {
		records, err := c.usage.BucketUsages(ctx, chunk)
		if err != nil {
			return nil, fmt.Errorf("usage.uncounted: %w", err)
		}
		for i, record := range records {
			if counting(record.Usage) {
				out = append(out, chunk[i])
			}
		}
	}
	return out, nil
}

// counting reports whether a record needs the walk: it is not exact - the
// bucket has none yet, or is being counted for the first time - or a
// recount is under way.
func counting(u bucket.Usage) bool {
	return !u.Exact || u.Walking()
}

// done takes a bucket out of the queue.
func (c *UsageCounter) done(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queue = slices.DeleteFunc(c.queue, func(n string) bool { return n == name })
}

// rotate puts a bucket whose walk goes on behind the others.
func (c *UsageCounter) rotate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queue = append(slices.DeleteFunc(c.queue, func(n string) bool { return n == name }), name)
}

// rest empties the queue until the next look.
func (c *UsageCounter) rest() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queue, c.idle = nil, 0
}

// enqueue has a bucket walked after those before it.
func (c *UsageCounter) enqueue(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Contains(c.queue, name) {
		c.queue = append(c.queue, name)
	}
}

// drifted tells of a recount that ended with other totals than the record
// held.
func (c *UsageCounter) drifted(ctx context.Context, name string, walk ports.UsageWalk) {
	if !walk.Drift {
		return
	}
	if c.onDrift != nil {
		c.onDrift()
	}
	c.logger.WarnContext(ctx, "the usage kept of a bucket was not what it holds; a recount put it right", slog.String("bucket", name))
}

// Recount has the usage of a bucket counted again, as it was counted the
// first time: it proposes the first entry of the walk, which begins the
// recount - a recount under way begins again - and the counter of the node
// that leads proposes the rest. The record stays exact meanwhile, and the
// totals go on being kept until the result takes their place. It needs
// admin:RecountBucketUsage on the bucket, and returns once the recount was
// asked for; a recount whose result differs from the totals is drift
// (UsageCounterConfig.OnDrift).
//
// The error wraps domain.ErrAccessDenied, domain.ErrBucketNotFound, and
// domain.ErrUnavailable until every member of the metadata group counts
// usage.
func (c *UsageCounter) Recount(ctx context.Context, principal iam.Principal, bucketName string) error {
	resource := iam.Resource{Bucket: bucketName}
	if err := c.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionRecountBucketUsage, Resource: resource}); err != nil {
		return fmt.Errorf("usage.Recount: %w", err)
	}
	// The entry carries no audit record (its schema is frozen): the
	// recount is audited here, in the background, as a change that does
	// not carry its record is.
	walk, err := c.usage.WalkBucketUsage(ctx, bucketName, UsageWalkLimit, true)
	if errors.Is(err, domain.ErrOutcomeUnknown) && c.auditor != nil {
		// The entry may have been committed, and the recount begun.
		c.auditor.Succeeded(ctx, c.auditor.Record(principal, iam.ActionRecountBucketUsage, resource, audit.Failed,
			map[string]string{"outcome": "unknown"}))
	}
	if err != nil {
		return fmt.Errorf("usage.Recount: %w", err)
	}
	if c.auditor != nil {
		c.auditor.Succeeded(ctx, c.auditor.Record(principal, iam.ActionRecountBucketUsage, resource, audit.Succeeded, nil))
	}
	// A small bucket is counted again by that one entry.
	c.drifted(ctx, bucketName, walk)
	if walk.Usage.Walking() {
		c.enqueue(bucketName)
	}
	return nil
}
