package disk

import (
	"io"
	"io/fs"

	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// ShardFile is the file a shard reader reads, for tests that make it fail.
type ShardFile interface {
	io.Reader
	io.ReaderAt
	io.Closer
	Stat() (fs.FileInfo, error)
}

// NewShardReader returns a reader over a shard file, as Drive.Open does.
func NewShardReader(f ShardFile, id object.ShardID) (ports.ShardReader, error) {
	return newShardReader(f, id)
}
