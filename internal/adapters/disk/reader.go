package disk

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// shardFile is what a shard reader needs of an open shard file: an *os.File,
// or in tests a file that fails as a failing drive does.
type shardFile interface {
	io.Reader
	io.ReaderAt
	io.Closer
	Stat() (fs.FileInfo, error)
}

// mediaError classifies an error from reading the bytes of an open shard
// file. The file is open, so the drive was reached and the shard is there: a
// read that fails anyway - EIO, a bad sector, a CRC error - is the shard
// being unreadable, which is damage the shard can be rebuilt from parity for,
// and the error wraps domain.ErrIntegrity. A file this process has closed is
// its own mistake, not the drive's, and is returned as it is.
func mediaError(err error) error {
	if errors.Is(err, os.ErrClosed) {
		return err
	}
	return fmt.Errorf("%w: read failed: %w", domain.ErrIntegrity, err)
}

// shardReader reads a shard file and verifies every block before it returns
// any of the block's bytes.
type shardReader struct {
	file      shardFile
	id        object.ShardID
	index     uint32 // shard index as the checksum covers it
	blockSize int
	size      int64 // data bytes in the shard

	// mu guards the one-block cache, which makes sequential reads verify
	// each block once.
	mu     sync.Mutex
	cached int64 // number of the verified block in buf, or -1
	buf    []byte
}

func newShardReader(f shardFile, want object.ShardID) (*shardReader, error) {
	head := make([]byte, headerSize)
	if _, err := io.ReadFull(f, head); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%w: shard file is shorter than its header", domain.ErrIntegrity)
		}
		return nil, mediaError(err)
	}
	h, err := decodeHeader(head)
	if err != nil {
		return nil, err
	}
	if h.id != want {
		return nil, fmt.Errorf("%w: file belongs to shard %s", domain.ErrIntegrity, h.id)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size, err := logicalSize(info.Size(), h.blockSize)
	if err != nil {
		return nil, err
	}
	index, err := checksumIndex(h.id)
	if err != nil {
		return nil, err
	}
	return &shardReader{file: f, id: h.id, index: index, blockSize: h.blockSize, size: size, cached: -1}, nil
}

// Size implements ports.ShardReader.
func (r *shardReader) Size() int64 {
	return r.size
}

// Close implements ports.ShardReader.
func (r *shardReader) Close() error {
	if err := r.file.Close(); err != nil {
		return fmt.Errorf("disk.Close: %w", err)
	}
	return nil
}

// ReadAt implements ports.ShardReader. Bytes are copied to p only from blocks
// whose checksum matched.
func (r *shardReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("disk.ReadAt: %w: negative offset", domain.ErrInvalidArgument)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	read := 0
	for read < len(p) {
		pos := off + int64(read)
		if pos >= r.size {
			return read, io.EOF
		}
		block := pos / int64(r.blockSize)
		data, err := r.verifiedBlock(block)
		if err != nil {
			return read, fmt.Errorf("disk.ReadAt: shard %s: %w", r.id, err)
		}
		read += copy(p[read:], data[pos-block*int64(r.blockSize):])
	}
	return read, nil
}

// verifiedBlock returns the data of a block after checking its checksum.
func (r *shardReader) verifiedBlock(block int64) ([]byte, error) {
	if r.cached == block {
		return r.buf[checksumSize:], nil
	}
	r.cached = -1

	length := min(int64(r.blockSize), r.size-block*int64(r.blockSize))
	stored := checksumSize + int(length)
	if cap(r.buf) < stored {
		r.buf = make([]byte, checksumSize+r.blockSize)
	}
	r.buf = r.buf[:stored]

	position := headerSize + block*int64(checksumSize+r.blockSize)
	if _, err := r.file.ReadAt(r.buf, position); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: block %d is truncated", domain.ErrIntegrity, block)
		}
		return nil, mediaError(err)
	}
	if block < 0 {
		return nil, fmt.Errorf("%w: negative block number", domain.ErrInvalidArgument)
	}
	want := blockChecksum(r.id.DataID, r.index, uint64(block), r.buf[checksumSize:])
	if string(want[:]) != string(r.buf[:checksumSize]) {
		return nil, fmt.Errorf("%w: block %d checksum mismatch", domain.ErrIntegrity, block)
	}
	r.cached = block
	return r.buf[checksumSize:], nil
}
