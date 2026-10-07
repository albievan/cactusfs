package ports

import (
	"context"
	"io"
	"time"

	"github.com/albievan/cactusfs/internal/domain/object"
)

// ShardStore stores shards on one drive. Shards are addressed by generated
// identifiers only; nothing a client names reaches the drive.
type ShardStore interface {
	// DriveID returns the generated, stable identifier of the drive.
	DriveID() string
	// Create starts writing a new shard. The shard does not exist for
	// readers until the writer's Commit returns. The error wraps
	// domain.ErrInvalidArgument when the shard already exists: shards are
	// immutable.
	Create(ctx context.Context, id object.ShardID) (ShardWriter, error)
	// Rewrite starts writing a shard that may already exist. When the
	// writer's Commit returns, the new content has replaced the old in one
	// step; until then, and if the writer is aborted, the old content stays
	// as it was. Healing uses it to replace a damaged shard without ever
	// leaving the object with fewer shards than before.
	Rewrite(ctx context.Context, id object.ShardID) (ShardWriter, error)
	// Open opens a committed shard for reading. The error wraps
	// domain.ErrShardNotFound when it does not exist.
	Open(ctx context.Context, id object.ShardID) (ShardReader, error)
	// Delete removes a shard. Deleting a shard that does not exist is not an
	// error.
	Delete(ctx context.Context, id object.ShardID) error
	// List visits every committed shard on the drive, in no particular
	// order. visit may delete the shard it is shown. A visit error stops the
	// listing and is returned.
	List(ctx context.Context, visit func(ShardInfo) error) error
}

// ShardInfo describes a committed shard without opening it.
type ShardInfo struct {
	ID object.ShardID
	// ModifiedAt is when the shard's content was last written, which is at
	// or shortly before the moment it was made durable.
	ModifiedAt time.Time
}

// ShardWriter writes one shard. Exactly one of Commit or Abort must be
// called.
type ShardWriter interface {
	io.Writer
	// Commit makes the shard durable and visible: when it returns nil the
	// bytes have been flushed to stable storage.
	Commit(ctx context.Context) error
	// Abort discards the shard. It is safe to call after Commit, where it
	// does nothing.
	Abort() error
}

// ShardReader reads one committed shard. Every byte it returns has passed
// checksum verification; a read that hits damaged data returns an error
// wrapping domain.ErrIntegrity and no bytes from the damaged block.
type ShardReader interface {
	io.ReaderAt
	io.Closer
	// Size returns the number of bytes that were written to the shard.
	Size() int64
}
