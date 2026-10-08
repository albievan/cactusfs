package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// healQueueSize bounds the objects waiting to be healed. When the queue is
// full a request is dropped; the scrubber finds the object again.
const healQueueSize = 1024

// HealResult is the outcome of checking and repairing one object.
type HealResult struct {
	Bucket string
	Key    string
	// Shards is the number of shards the object has.
	Shards int
	// Damaged is the number that were missing, truncated or corrupt.
	Damaged int
	// Rebuilt is the number of those that were written again.
	Rebuilt int
	// Unrecoverable reports that more shards were damaged than parity can
	// rebuild: the object is lost.
	Unrecoverable bool
	// Verified is the number of stored bytes that were read and verified.
	Verified int64
	// Offline is the number of shards on drives that could not be reached.
	// Nothing is known about them; they are checked when the drives are
	// back.
	Offline int
}

// healTarget names what to heal: the current version of a key, or, with
// named set, the version ref names.
type healTarget struct {
	bucket string
	key    string
	named  bool
	ref    object.VersionRef
	// upload and part, when part is not zero, name a part of a multipart
	// upload in progress instead of a version.
	upload object.ID
	part   int
}

// Healer repairs objects whose shards are missing, truncated or corrupt, by
// rebuilding them from the remaining shards. Reads and writes that find an
// object degraded queue it; the scrubber checks every object in turn.
type Healer struct {
	index   ports.ObjectIndex
	uploads ports.UploadStore
	storage *shardStorage
	logger  *slog.Logger
	observe func(HealResult)

	queue chan healTarget
	mu    sync.Mutex
	// queued holds the targets in the queue, so that an object read a
	// thousand times while degraded is healed once.
	queued map[healTarget]bool
	// active holds the targets being healed, so that the queue and the
	// scrubber never rewrite the same shards at the same time.
	active map[healTarget]bool
}

func newHealer(index ports.ObjectIndex, uploads ports.UploadStore, storage *shardStorage, logger *slog.Logger, observe func(HealResult)) *Healer {
	if observe == nil {
		observe = func(HealResult) {}
	}
	return &Healer{
		index: index, uploads: uploads, storage: storage, logger: logger, observe: observe,
		queue: make(chan healTarget, healQueueSize), queued: map[healTarget]bool{}, active: map[healTarget]bool{},
	}
}

// Enqueue asks for a version of an object to be healed in the background. It
// never blocks.
func (h *Healer) Enqueue(v object.Version) {
	h.enqueue(healTarget{bucket: v.Bucket, key: v.Key, named: true, ref: v.Ref()})
}

// EnqueuePart asks for a part of a multipart upload in progress to be healed
// in the background. It never blocks.
func (h *Healer) EnqueuePart(bucketName, key string, uploadID object.ID, number int) {
	h.enqueue(healTarget{bucket: bucketName, key: key, upload: uploadID, part: number})
}

func (h *Healer) enqueue(target healTarget) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.queued[target] {
		return
	}
	select {
	case h.queue <- target:
		h.queued[target] = true
	default:
	}
}

// Run heals queued objects until ctx is cancelled.
func (h *Healer) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case target := <-h.queue:
			h.mu.Lock()
			delete(h.queued, target)
			h.mu.Unlock()
			if _, err := h.heal(ctx, target, nil); err != nil && ctx.Err() == nil {
				h.logger.ErrorContext(ctx, "heal failed", slog.String("bucket", target.bucket), slog.Any("error", err))
			}
		}
	}
}

// HealObject verifies every shard of the current version of an object and
// rebuilds the damaged ones. An object that does not exist any more, or that
// is being healed already, is not an error. The error wraps
// domain.ErrIntegrity when the object cannot be rebuilt.
func (h *Healer) HealObject(ctx context.Context, bucketName, key string) (HealResult, error) {
	return h.heal(ctx, healTarget{bucket: bucketName, key: key}, nil)
}

// HealVersion does what HealObject does for one named version of an object,
// current or not.
func (h *Healer) HealVersion(ctx context.Context, bucketName, key string, ref object.VersionRef) (HealResult, error) {
	return h.heal(ctx, healTarget{bucket: bucketName, key: key, named: true, ref: ref}, nil)
}

func (h *Healer) heal(ctx context.Context, target healTarget, slow throttle) (HealResult, error) {
	bucketName, key := target.bucket, target.key
	result := HealResult{Bucket: bucketName, Key: key}

	h.mu.Lock()
	if h.active[target] {
		h.mu.Unlock()
		return result, nil
	}
	h.active[target] = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.active, target)
		h.mu.Unlock()
	}()
	if target.part != 0 {
		return h.healPart(ctx, target, slow)
	}

	var version object.Version
	var err error
	if target.named {
		version, err = h.index.GetVersion(ctx, bucketName, key, target.ref)
	} else {
		version, err = h.index.GetObject(ctx, bucketName, key)
	}
	if errors.Is(err, domain.ErrObjectNotFound) || errors.Is(err, domain.ErrVersionNotFound) || errors.Is(err, domain.ErrBucketNotFound) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("heal.Object: %w", err)
	}
	if version.DeleteMarker {
		// A marker has nothing stored.
		return result, nil
	}

	// A version made of parts is healed part by part. Every part is
	// looked at even when one cannot be repaired: the first failure is
	// what the heal reports.
	type healed struct {
		extent  object.Extent
		rebuilt []int
	}
	var done []healed
	var healErr error
	walkErr := eachExtent(ctx, h.index, version, func(extent object.Extent) error {
		outcome, err := h.storage.heal(ctx, extent, slow)
		result.Shards += outcome.shards
		result.Damaged += outcome.damaged
		result.Rebuilt += len(outcome.rebuilt)
		result.Verified += outcome.verified
		result.Offline += outcome.offline
		if len(outcome.rebuilt) > 0 {
			done = append(done, healed{extent: extent, rebuilt: outcome.rebuilt})
		}
		if err != nil && healErr == nil {
			healErr = err
		}
		return ctx.Err()
	})
	if healErr == nil {
		healErr = walkErr
	}
	if healErr == nil && result.Damaged == 0 {
		h.observe(result)
		return result, nil
	}

	// Something was found or done. Before acting on it, see whether the
	// version is still there: it may have been replaced or deleted while
	// it was being checked, and its shards disappearing is then not
	// damage.
	if h.replaced(ctx, version) {
		// What was rebuilt belongs to nothing any more. Only that is
		// removed; a shard this heal did not write is never touched.
		for _, d := range done {
			h.storage.removeShards(ctx, d.extent.DataID, d.extent.Layout, d.rebuilt)
		}
		return HealResult{Bucket: bucketName, Key: key}, nil
	}
	if healErr != nil {
		if errors.Is(healErr, domain.ErrIntegrity) {
			result.Unrecoverable = true
			h.logger.ErrorContext(ctx, "object is damaged beyond repair",
				slog.String("bucket", bucketName), slog.String("data_id", version.DataID.String()),
				slog.Int("damaged_shards", result.Damaged), slog.Int("shards", result.Shards))
			h.observe(result)
		}
		return result, fmt.Errorf("heal.Object: %w", healErr)
	}
	h.logger.WarnContext(ctx, "object healed",
		slog.String("bucket", bucketName), slog.String("data_id", version.DataID.String()),
		slog.Int("damaged_shards", result.Damaged), slog.Int("rebuilt_shards", result.Rebuilt))
	h.observe(result)
	return result, nil
}

// healPart verifies and repairs one part of a multipart upload in progress.
// A part that is gone - replaced, or its upload completed or aborted - is
// not an error: what became of its data is the business of whatever has it.
func (h *Healer) healPart(ctx context.Context, target healTarget, slow throttle) (HealResult, error) {
	result := HealResult{Bucket: target.bucket, Key: target.key}
	if h.uploads == nil {
		return result, nil
	}
	parts, err := h.uploads.ListParts(ctx, target.bucket, target.key, target.upload, target.part-1, 1)
	if errors.Is(err, domain.ErrUploadNotFound) || errors.Is(err, domain.ErrBucketNotFound) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("heal.Part: %w", err)
	}
	if len(parts) == 0 || parts[0].Number != target.part {
		return result, nil
	}
	extent := parts[0].Extent()
	outcome, healErr := h.storage.heal(ctx, extent, slow)
	result.Shards, result.Damaged, result.Rebuilt, result.Verified, result.Offline = outcome.shards, outcome.damaged, len(outcome.rebuilt), outcome.verified, outcome.offline
	if healErr == nil && result.Damaged == 0 {
		return result, nil
	}
	// The part may have been sent again, or its upload aborted, while it
	// was being checked. Then nothing refers to its data, and what was
	// rebuilt is removed. When the index cannot say, it stays.
	if referenced, err := h.index.IsDataReferenced(context.WithoutCancel(ctx), extent.DataID); err == nil && !referenced {
		h.storage.removeShards(ctx, extent.DataID, extent.Layout, outcome.rebuilt)
		return HealResult{Bucket: target.bucket, Key: target.key}, nil
	}
	if healErr != nil {
		if errors.Is(healErr, domain.ErrIntegrity) {
			result.Unrecoverable = true
			h.logger.ErrorContext(ctx, "part of an upload is damaged beyond repair",
				slog.String("bucket", target.bucket), slog.String("data_id", extent.DataID.String()), slog.Int("part", target.part))
			h.observe(result)
		}
		return result, fmt.Errorf("heal.Part: %w", healErr)
	}
	h.logger.WarnContext(ctx, "part of an upload healed",
		slog.String("bucket", target.bucket), slog.String("data_id", extent.DataID.String()), slog.Int("rebuilt_shards", result.Rebuilt))
	h.observe(result)
	return result, nil
}

// eachExtent calls visit for every piece of stored data of a version: the
// version's own, or, for a version made of parts, that of each part in
// order. A version whose parts are gone has none.
func eachExtent(ctx context.Context, index ports.ObjectIndex, v object.Version, visit func(object.Extent) error) error {
	if !v.Multipart() {
		return visit(v.Extent())
	}
	next := 0
	var offset int64
	for next < v.Parts {
		parts, err := index.ObjectParts(ctx, v.Bucket, v.DataID, offset, partPage)
		if err != nil {
			return err
		}
		if len(parts) == 0 {
			// Its parts end before the version says they do. If the
			// version was removed meanwhile, the caller finds that out;
			// if not, the object cannot be read.
			return fmt.Errorf("%w: %s has %d of %d parts", domain.ErrIntegrity, v.DataID, next, v.Parts)
		}
		for _, part := range parts {
			if part.Index != next {
				return fmt.Errorf("%w: part %d of %s is where part %d should be", domain.ErrIntegrity, part.Index+1, v.DataID, next+1)
			}
			if err := visit(part.Extent()); err != nil {
				return err
			}
			next++
			offset = part.Offset + part.Size
		}
	}
	return nil
}

// replaced reports whether the version v is definitely gone: removed, or, for
// a null version, replaced by another. A version that is merely no longer
// current is not gone. It answers false whenever it cannot tell: a lookup
// that fails - because the node is shutting down, for one - is no evidence
// that the version is gone, and nothing may be removed on the strength of it.
func (h *Healer) replaced(ctx context.Context, v object.Version) bool {
	// Shutdown cancels ctx; the answer is still needed to decide whether
	// rebuilt shards are kept.
	stored, err := h.index.GetVersion(context.WithoutCancel(ctx), v.Bucket, v.Key, v.Ref())
	switch {
	case err == nil:
		return stored.DataID != v.DataID
	case errors.Is(err, domain.ErrVersionNotFound), errors.Is(err, domain.ErrObjectNotFound), errors.Is(err, domain.ErrBucketNotFound):
		return true
	default:
		return false
	}
}
