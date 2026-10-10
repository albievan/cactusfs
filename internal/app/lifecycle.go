package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// LifecycleStats summarises one pass of the lifecycle worker.
type LifecycleStats struct {
	// Buckets is the number of buckets that have rules.
	Buckets int
	// Expired is the number of current versions that were expired: removed,
	// or, where versions are kept, put under a delete marker.
	Expired int
	// Noncurrent is the number of older versions removed for good.
	Noncurrent int
	// Markers is the number of delete markers removed that were the only
	// version left of their key.
	Markers int
	// Aborted is the number of multipart uploads aborted.
	Aborted int
	// Kept is the number of removals a lock refused.
	Kept int
	// Skipped is the number of removals not made because what was to be
	// removed had been replaced since the pass looked at it.
	Skipped int
	// Failed is the number of removals that failed for another reason.
	Failed int
}

// LifecycleObjects are the object use cases the lifecycle worker acts
// through. *ObjectService implements it.
type LifecycleObjects interface {
	Delete(ctx context.Context, principal iam.Principal, in DeleteInput) (DeleteOutput, error)
	AbortUpload(ctx context.Context, principal iam.Principal, in UploadRef) error
}

// LifecycleWorker carries out the lifecycle rules of buckets: it expires
// objects, removes old versions and lone delete markers, and aborts
// multipart uploads that were never finished.
//
// It removes nothing by a way of its own. Every removal is the delete or the
// abort a client could have asked for, made as the lifecycle service: it
// passes the authorizer, which lets that service delete and abort and never
// lift a retention, and it is decided by the state machine, where Object
// Lock is enforced. A version that a lock keeps stays, whatever a rule says.
type LifecycleWorker struct {
	buckets ports.BucketStore
	index   ports.ObjectIndex
	uploads ports.UploadStore
	objects LifecycleObjects
	clock   ports.Clock
	logger  *slog.Logger
	onPass  func(LifecycleStats)
	active  func() bool
}

// LifecycleWorkerConfig lists what a LifecycleWorker depends on.
type LifecycleWorkerConfig struct {
	Buckets ports.BucketStore
	Index   ports.ObjectIndex
	Uploads ports.UploadStore
	Objects LifecycleObjects
	Clock   ports.Clock
	Logger  *slog.Logger
	// OnPass, when not nil, is told the result of every pass.
	OnPass func(LifecycleStats)
	// Active, when not nil, says whether this node is the one that carries
	// out rules now: in a cluster, the one that leads the metadata group.
	Active func() bool
}

// NewLifecycleWorker returns a LifecycleWorker.
func NewLifecycleWorker(cfg LifecycleWorkerConfig) *LifecycleWorker {
	return &LifecycleWorker{
		buckets: cfg.Buckets, index: cfg.Index, uploads: cfg.Uploads, objects: cfg.Objects,
		clock: cfg.Clock, logger: cfg.Logger, onPass: cfg.OnPass, active: cfg.Active,
	}
}

// lifecycleBatch is the number of versions or uploads looked at between two
// rounds of removals. Nothing is removed while a scan is reading the state.
const lifecycleBatch = 500

// Run carries out the rules, waits for the interval and starts again, until
// ctx is cancelled.
func (w *LifecycleWorker) Run(ctx context.Context, interval time.Duration) {
	for ctx.Err() == nil {
		if w.isActive() {
			stats, err := w.RunOnce(ctx)
			// What a pass did is said whether or not it came to its end:
			// this is the only record of what rules removed.
			logged := context.WithoutCancel(ctx)
			if w.onPass != nil {
				w.onPass(stats)
			}
			if stats.Expired+stats.Noncurrent+stats.Markers+stats.Aborted+stats.Failed+stats.Skipped > 0 {
				w.logger.InfoContext(logged, "lifecycle rules carried out",
					slog.Int("expired", stats.Expired), slog.Int("noncurrent_removed", stats.Noncurrent),
					slog.Int("markers_removed", stats.Markers), slog.Int("uploads_aborted", stats.Aborted),
					slog.Int("kept_by_lock", stats.Kept), slog.Int("changed_meanwhile", stats.Skipped), slog.Int("failed", stats.Failed))
			}
			if err != nil && ctx.Err() == nil && !errors.Is(err, errStandby) {
				w.logger.ErrorContext(ctx, "lifecycle pass failed", slog.Any("error", err))
			}
			if ctx.Err() != nil {
				return
			}
		}
		sleep(ctx, interval)
	}
}

func (w *LifecycleWorker) isActive() bool {
	return w.active == nil || w.active()
}

// RunOnce carries out the rules of every bucket once.
func (w *LifecycleWorker) RunOnce(ctx context.Context) (LifecycleStats, error) {
	var stats LifecycleStats
	buckets, err := w.buckets.ListBuckets(ctx)
	if err != nil {
		return stats, fmt.Errorf("lifecycle.RunOnce: %w", err)
	}
	for _, b := range buckets {
		raw, err := w.buckets.GetBucketConfig(ctx, b.Name, bucket.ConfigLifecycle)
		if errors.Is(err, domain.ErrBucketNotFound) || (err == nil && len(raw) == 0) {
			continue
		}
		if err != nil {
			return stats, fmt.Errorf("lifecycle.RunOnce: %w", err)
		}
		rules, err := decodeLifecycle(raw)
		if err == nil {
			err = rules.Validate()
		}
		if err != nil {
			// Rules that cannot be understood remove nothing.
			w.logger.ErrorContext(ctx, "lifecycle rules of a bucket cannot be read; nothing is removed", slog.String("bucket", b.Name), slog.Any("error", err))
			continue
		}
		stats.Buckets++
		if err := w.versions(ctx, b.Name, *rules, &stats); err != nil {
			return stats, fmt.Errorf("lifecycle.RunOnce: %w", err)
		}
		if err := w.incomplete(ctx, b.Name, *rules, &stats); err != nil {
			return stats, fmt.Errorf("lifecycle.RunOnce: %w", err)
		}
	}
	return stats, nil
}

// removal is one thing a pass decided to remove.
type removal struct {
	key     string
	version *object.VersionRef
	// judged is the version ID of the version the decision was made
	// about: the delete is carried out only if that is still the one.
	judged object.ID
	count  *int
}

// keyWalk follows the versions of a bucket as a scan yields them - key by
// key, the newest version of each first - and decides what the rules remove.
type keyWalk struct {
	rules bucket.Lifecycle
	now   time.Time
	stats *LifecycleStats
	todo  []removal

	// key is the key being walked; newer is when the version before the
	// present one in the walk was made, which is when the present one
	// stopped being current.
	key   string
	newer time.Time
	// lone is a delete marker that is the newest version of key and, so
	// far, the only one seen.
	lone *object.VersionRef
	// loneID is the version ID of the newest version of key.
	loneID object.ID
}

func (k *keyWalk) feed(v object.Version, latest bool) {
	if latest || v.Key != k.key {
		k.flush()
		k.key = v.Key
		ref := v.Ref()
		switch {
		case v.DeleteMarker:
			if k.rules.RemovesLoneMarker(v.Key) {
				k.lone = &ref
			}
		case k.rules.ExpiresCurrent(v.Key, v.ModifiedAt, k.now):
			// As a client deletes: without naming a version. Where
			// versions are kept that makes a marker and removes nothing.
			k.todo = append(k.todo, removal{key: v.Key, judged: v.VersionID, count: &k.stats.Expired})
		}
		k.newer = v.ModifiedAt
		k.loneID = v.VersionID
		return
	}
	// An older version: the key has more than its newest.
	k.lone = nil
	if k.rules.ExpiresNoncurrent(v.Key, k.newer, k.now) {
		ref := v.Ref()
		k.todo = append(k.todo, removal{key: v.Key, version: &ref, judged: v.VersionID, count: &k.stats.Noncurrent})
	}
	k.newer = v.ModifiedAt
}

// flush ends the walk of a key: a marker that turned out to be alone goes.
func (k *keyWalk) flush() {
	if k.lone != nil {
		k.todo = append(k.todo, removal{key: k.key, version: k.lone, judged: k.loneID, count: &k.stats.Markers})
		k.lone = nil
	}
}

func (w *LifecycleWorker) versions(ctx context.Context, bucketName string, rules bucket.Lifecycle, stats *LifecycleStats) error {
	walk := &keyWalk{rules: rules, now: w.clock.Now(), stats: stats}
	var start ports.VersionCursor
	// held are the removals decided on the batch before the present one.
	// They are carried out only after the next batch has been read: the
	// scan resumes after the last version it saw, and that version must
	// still be there to resume after.
	var held []removal
	for {
		var last *object.Version
		count, more := 0, false
		err := w.index.ScanVersions(ctx, bucketName, start, func(v object.Version, latest bool) ports.ScanControl {
			if count == lifecycleBatch {
				more = true
				return ports.ScanControl{Stop: true}
			}
			count++
			walk.feed(v, latest)
			last = &v
			return ports.ScanControl{}
		})
		if errors.Is(err, domain.ErrBucketNotFound) {
			return nil // deleted meanwhile
		}
		if err != nil {
			return err
		}
		if !more {
			walk.flush()
		}
		if err := w.remove(ctx, bucketName, held, stats); err != nil {
			return err
		}
		held, walk.todo = walk.todo, nil
		if !more || last == nil {
			return w.remove(ctx, bucketName, held, stats)
		}
		ref := last.Ref()
		start = ports.VersionCursor{Key: last.Key, After: &ref}
	}
}

// remove carries out removals, each on the condition that what was judged
// is still what is there: between the reading of the state and this moment a
// client may have written the key again, and what it wrote is not what the
// rule was about.
func (w *LifecycleWorker) remove(ctx context.Context, bucketName string, removals []removal, stats *LifecycleStats) error {
	service := iam.Principal{Service: iam.ServiceLifecycle}
	for _, r := range removals {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !w.isActive() {
			return errStandby
		}
		out, err := w.objects.Delete(ctx, service, DeleteInput{Bucket: bucketName, Key: r.key, Version: r.version, Expect: r.judged})
		if err == nil && out.Removed == nil && out.Marker == nil {
			// Gone already: nothing was done.
			continue
		}
		w.count(ctx, err, r.count, stats, bucketName)
	}
	return nil
}

// count records how a removal went. One that a lock refused, or that found
// its target gone or replaced, is no failure of the pass.
func (w *LifecycleWorker) count(ctx context.Context, err error, done *int, stats *LifecycleStats, bucketName string) {
	switch {
	case err == nil:
		*done++
	case errors.Is(err, domain.ErrObjectLocked):
		stats.Kept++
	case errors.Is(err, domain.ErrConflict):
		stats.Skipped++
	case errors.Is(err, domain.ErrObjectNotFound), errors.Is(err, domain.ErrVersionNotFound),
		errors.Is(err, domain.ErrUploadNotFound), errors.Is(err, domain.ErrBucketNotFound):
	default:
		stats.Failed++
		w.logger.WarnContext(ctx, "a lifecycle rule could not be carried out", slog.String("bucket", bucketName), slog.Any("error", err))
	}
}

func (w *LifecycleWorker) incomplete(ctx context.Context, bucketName string, rules bucket.Lifecycle, stats *LifecycleStats) error {
	service := iam.Principal{Service: iam.ServiceLifecycle}
	now := w.clock.Now()
	var start ports.UploadCursor
	for {
		var due []object.Upload
		var last *object.Upload
		count, more := 0, false
		err := w.uploads.ScanUploads(ctx, bucketName, start, func(u object.Upload) ports.ScanControl {
			if count == lifecycleBatch {
				more = true
				return ports.ScanControl{Stop: true}
			}
			count++
			last = &u
			if rules.AbortsUpload(u.Key, u.InitiatedAt, now) {
				due = append(due, u)
			}
			return ports.ScanControl{}
		})
		if errors.Is(err, domain.ErrBucketNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, u := range due {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !w.isActive() {
				return errStandby
			}
			err := w.objects.AbortUpload(ctx, service, UploadRef{Bucket: bucketName, Key: u.Key, UploadID: u.ID})
			w.count(ctx, err, &stats.Aborted, stats, bucketName)
		}
		if !more || last == nil {
			return nil
		}
		start = ports.UploadCursor{Key: last.Key, After: last.ID}
	}
}
