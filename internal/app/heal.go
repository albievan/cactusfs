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

type healTarget struct {
	bucket string
	key    string
}

// Healer repairs objects whose shards are missing, truncated or corrupt, by
// rebuilding them from the remaining shards. Reads and writes that find an
// object degraded queue it; the scrubber checks every object in turn.
type Healer struct {
	index   ports.ObjectIndex
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

func newHealer(index ports.ObjectIndex, storage *shardStorage, logger *slog.Logger, observe func(HealResult)) *Healer {
	if observe == nil {
		observe = func(HealResult) {}
	}
	return &Healer{
		index: index, storage: storage, logger: logger, observe: observe,
		queue: make(chan healTarget, healQueueSize), queued: map[healTarget]bool{}, active: map[healTarget]bool{},
	}
}

// Enqueue asks for an object to be healed in the background. It never blocks.
func (h *Healer) Enqueue(bucketName, key string) {
	target := healTarget{bucket: bucketName, key: key}
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
			if _, err := h.HealObject(ctx, target.bucket, target.key); err != nil && ctx.Err() == nil {
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
	return h.heal(ctx, bucketName, key, nil)
}

func (h *Healer) heal(ctx context.Context, bucketName, key string, slow throttle) (HealResult, error) {
	target := healTarget{bucket: bucketName, key: key}
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

	version, err := h.index.GetObject(ctx, bucketName, key)
	if errors.Is(err, domain.ErrObjectNotFound) || errors.Is(err, domain.ErrBucketNotFound) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("heal.Object: %w", err)
	}

	outcome, healErr := h.storage.heal(ctx, version, slow)
	result.Shards, result.Damaged, result.Rebuilt, result.Verified = outcome.shards, outcome.damaged, len(outcome.rebuilt), outcome.verified
	result.Offline = outcome.offline
	if healErr == nil && result.Damaged == 0 {
		h.observe(result)
		return result, nil
	}

	// Something was found or done. Before acting on it, see whether the
	// version is still the object's current one: it may have been replaced
	// or deleted while it was being checked, and its shards disappearing is
	// then not damage.
	if h.replaced(ctx, version) {
		// What was rebuilt belongs to nothing any more. Only that is
		// removed; a shard this heal did not write is never touched.
		h.storage.removeShards(ctx, version.DataID, version.Layout, outcome.rebuilt)
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

// replaced reports whether the object definitely has another current version
// than v, or none. It answers false whenever it cannot tell: a lookup that
// fails - because the node is shutting down, for one - is no evidence that
// the version is gone, and nothing may be removed on the strength of it.
func (h *Healer) replaced(ctx context.Context, v object.Version) bool {
	// Shutdown cancels ctx; the answer is still needed to decide whether
	// rebuilt shards are kept.
	current, err := h.index.GetObject(context.WithoutCancel(ctx), v.Bucket, v.Key)
	switch {
	case err == nil:
		return current.DataID != v.DataID
	case errors.Is(err, domain.ErrObjectNotFound), errors.Is(err, domain.ErrBucketNotFound):
		return true
	default:
		return false
	}
}
