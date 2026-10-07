package ports

import (
	"context"
	"io"

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
	// Open opens a committed shard for reading. The error wraps
	// domain.ErrShardNotFound when it does not exist.
	Open(ctx context.Context, id object.ShardID) (ShardReader, error)
	// Delete removes a shard. Deleting a shard that does not exist is not an
	// error.
	Delete(ctx context.Context, id object.ShardID) error
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
