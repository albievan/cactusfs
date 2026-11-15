package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
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

// BucketQuota is the quota of a bucket and what counts against it
// (ADR-0024).
type BucketQuota struct {
	// QuotaBytes is the most bytes the bucket may hold; 0 when it has no
	// quota.
	QuotaBytes int64
	// UsedBytes is what counts against the quota: the stored versions and
	// the parts of unfinished uploads, added. 0 while Exact is false.
	UsedBytes int64
	// Exact says that the bucket's usage is counted. A quota can be set
	// only then.
	Exact bool
}

// QuotaService holds the use cases of the quota of a bucket (ADR-0024):
// reading, setting and removing it, each through the single authorizer
// with an admin: action on the bucket - so that a bucket's own
// administrator, who holds s3:*, cannot lift its limit - and each change
// with its audit record in the entry that makes it. It also holds the
// pre-check of the write path (Admit).
type QuotaService struct {
	buckets   ports.BucketStore
	usage     ports.BucketUsageStore
	authz     ports.Authorizer
	auditor   *Auditor
	level     func(ctx context.Context) (int, error)
	onRefusal func()
}

// QuotaServiceConfig lists what a QuotaService depends on.
type QuotaServiceConfig struct {
	Buckets ports.BucketStore
	Usage   ports.BucketUsageStore
	Authz   ports.Authorizer
	// Auditor builds the records of changes; without one every change is
	// refused.
	Auditor *Auditor
	// Level returns the metadata level the deployment writes now, as the
	// metadata store of this node knows it. Below ports.UsageLevel there
	// are no quotas, and Get answers domain.ErrUnavailable, as the changes
	// do; without Level, Get always does - a level is not guessed.
	Level func(ctx context.Context) (int, error)
	// OnRefusal, when not nil, is called for every write refused for a
	// quota, by the pre-check or by the store at commit
	// (cactusfs_quota_refusals_total). A refusal is no audit event.
	OnRefusal func()
}

// NewQuotaService returns a QuotaService.
func NewQuotaService(cfg QuotaServiceConfig) *QuotaService {
	return &QuotaService{
		buckets: cfg.Buckets, usage: cfg.Usage, authz: cfg.Authz, auditor: cfg.Auditor, level: cfg.Level, onRefusal: cfg.OnRefusal,
	}
}

// Get returns the quota of a bucket and what counts against it, from this
// node's copy of the state. It needs admin:GetBucketQuota on the bucket.
// The error wraps domain.ErrAccessDenied, domain.ErrBucketNotFound, and
// domain.ErrUnavailable until every member of the metadata group counts
// usage.
func (q *QuotaService) Get(ctx context.Context, principal iam.Principal, bucketName string) (BucketQuota, error) {
	if err := q.authz.Authorize(ctx, bucketRequest(principal, iam.ActionGetBucketQuota, bucketName)); err != nil {
		return BucketQuota{}, fmt.Errorf("quota.Get: %w", err)
	}
	if err := q.counts(ctx); err != nil {
		return BucketQuota{}, fmt.Errorf("quota.Get: %w", err)
	}
	got, err := q.read(ctx, bucketName)
	if err != nil {
		return BucketQuota{}, fmt.Errorf("quota.Get: %w", err)
	}
	return got, nil
}

// counts wraps domain.ErrUnavailable unless every member of the metadata
// group counts usage and keeps quotas. Below that level the store has no
// record, and a read would answer "no quota" for what is "not yet".
func (q *QuotaService) counts(ctx context.Context) error {
	if q.level == nil {
		return fmt.Errorf("%w: the metadata level is not known", domain.ErrUnavailable)
	}
	level, err := q.level(ctx)
	if err != nil {
		return err
	}
	if level < ports.UsageLevel {
		return fmt.Errorf("%w: the cluster is being upgraded: quotas need metadata version %d, and a node that only understands version %d is still a member",
			domain.ErrUnavailable, ports.UsageLevel, level)
	}
	return nil
}

// read returns the quota of a bucket and its usage, without asking anyone.
func (q *QuotaService) read(ctx context.Context, bucketName string) (BucketQuota, error) {
	// The record of a bucket that is not there is the zero record: the
	// bucket is read first, so that it is told apart from one not counted.
	if _, err := q.buckets.GetBucket(ctx, bucketName); err != nil {
		return BucketQuota{}, err
	}
	records, err := q.usage.BucketUsages(ctx, []string{bucketName})
	if err != nil {
		return BucketQuota{}, err
	}
	record := records[0]
	got := BucketQuota{QuotaBytes: record.QuotaBytes, Exact: record.Usage.Exact}
	if got.Exact {
		got.UsedBytes = record.Usage.Used()
	}
	return got, nil
}

// Set gives a bucket a quota of quotaBytes, from 1 to bucket.MaxQuotaBytes
// (2^62), in place of the one it had, and returns it with what counts
// against it. A quota below what the bucket holds is accepted: nothing is
// removed, and nothing more is taken until enough was deleted. It needs
// admin:PutBucketQuota on the bucket, and the change carries its audit
// record in its entry.
//
// The error wraps domain.ErrAccessDenied, domain.ErrInvalidArgument (a
// quota out of bounds; nothing is proposed), domain.ErrBucketNotFound,
// domain.ErrConflict while the bucket's usage is being counted for the
// first time, and domain.ErrUnavailable until every member of the metadata
// group counts usage.
func (q *QuotaService) Set(ctx context.Context, principal iam.Principal, bucketName string, quotaBytes int64) (BucketQuota, error) {
	if err := q.authz.Authorize(ctx, bucketRequest(principal, iam.ActionPutBucketQuota, bucketName)); err != nil {
		return BucketQuota{}, fmt.Errorf("quota.Set: %w", err)
	}
	// 0 removes a quota through the port; here it is out of bounds, as
	// removing is a permission of its own (Delete).
	if err := bucket.ValidateQuota(quotaBytes); err != nil {
		return BucketQuota{}, fmt.Errorf("quota.Set: %w", err)
	}
	if err := q.change(ctx, principal, iam.ActionPutBucketQuota, bucketName, quotaBytes); err != nil {
		return BucketQuota{}, fmt.Errorf("quota.Set: %w", err)
	}
	// What the bucket holds is read after the change, without a second
	// authorization: the caller may set the quota and need not read it.
	got, err := q.read(ctx, bucketName)
	if err != nil {
		// The quota was set; a repeat sets the same again.
		return BucketQuota{}, fmt.Errorf("quota.Set: the quota was set, but cannot be read: %w", err)
	}
	return got, nil
}

// Delete removes the quota of a bucket; a bucket without one is left as it
// is. It needs admin:DeleteBucketQuota on the bucket, and the change carries
// its audit record in its entry. The error wraps domain.ErrAccessDenied,
// domain.ErrBucketNotFound, domain.ErrConflict while the bucket's usage is
// being counted for the first time - when it can have no quota - and
// domain.ErrUnavailable until every member of the metadata group counts
// usage.
func (q *QuotaService) Delete(ctx context.Context, principal iam.Principal, bucketName string) error {
	if err := q.authz.Authorize(ctx, bucketRequest(principal, iam.ActionDeleteBucketQuota, bucketName)); err != nil {
		return fmt.Errorf("quota.Delete: %w", err)
	}
	if err := q.change(ctx, principal, iam.ActionDeleteBucketQuota, bucketName, 0); err != nil {
		return fmt.Errorf("quota.Delete: %w", err)
	}
	return nil
}

// change proposes a quota - 0 for none - with the audit record of the
// change that was allowed.
func (q *QuotaService) change(ctx context.Context, principal iam.Principal, action iam.Action, bucketName string, quotaBytes int64) error {
	if q.auditor == nil {
		return fmt.Errorf("%w: no audit trail to record the change in", domain.ErrUnavailable)
	}
	var details map[string]string
	if quotaBytes != 0 {
		details = map[string]string{"quota_bytes": strconv.FormatInt(quotaBytes, 10)}
	}
	record := q.auditor.Record(principal, action, iam.Resource{Bucket: bucketName}, audit.Succeeded, details)
	return q.usage.SetBucketQuota(ctx, bucketName, quotaBytes, record)
}

// Admit is the pre-check of the write path (ADR-0024): it says whether a
// write of declared bytes - -1 when the length is not known - may be made
// to a bucket, before any shard of it is written, so that a write that
// cannot succeed does not first write its shards. It is advice, answered
// from this node's copy of the state; what decides is the store, when the
// write is committed (domain.ErrQuotaExceeded). It authorizes nothing: the
// caller has authorized the write.
//
// A write is judged as the store judges it: by what it adds less what it
// frees. frees, when not nil, returns what the write would replace - the
// null version of a key where versions are not kept, the part of the same
// number - and is called only when the bucket has a quota that the write
// would pass without it. A length that is not known is judged as one byte,
// so that such a write is refused only when the bucket is full and the
// write frees nothing.
//
// A bucket without a quota and one whose usage is not exact admit
// everything. So does a deployment below ports.UsageLevel, without asking
// the level on every write: there no bucket has a record, let alone an
// exact one, and the store keeps no quota. The error wraps
// domain.ErrQuotaExceeded, or the error of a read.
func (q *QuotaService) Admit(ctx context.Context, bucketName string, declared int64, frees func(context.Context) (int64, error)) error {
	records, err := q.usage.BucketUsages(ctx, []string{bucketName})
	if err != nil {
		return fmt.Errorf("quota.Admit: %w", err)
	}
	record := records[0]
	if record.QuotaBytes == 0 || !record.Usage.Exact {
		return nil
	}
	adds := declared
	if declared < 0 {
		adds = 1
	}
	if record.Usage.Admits(record.QuotaBytes, bucket.UsageDelta{StoredBytes: adds}) {
		return nil
	}
	if frees != nil {
		freed, err := frees(ctx)
		if err != nil {
			return fmt.Errorf("quota.Admit: %w", err)
		}
		// Neither is below nothing, so the difference does not wrap.
		if record.Usage.Admits(record.QuotaBytes, bucket.UsageDelta{StoredBytes: adds - max(freed, 0)}) {
			return nil
		}
	}
	q.refused()
	return fmt.Errorf("quota.Admit: %w: bucket %s", domain.ErrQuotaExceeded, bucketName)
}

// refused counts a write refused for a quota. Admit counts its own; the
// write path counts a commit the store refused.
func (q *QuotaService) refused() {
	if q.onRefusal != nil {
		q.onRefusal()
	}
}
