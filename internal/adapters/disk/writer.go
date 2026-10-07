package disk

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// shardWriter writes a shard to a temporary file, block by block, and
// publishes it on Commit.
type shardWriter struct {
	drive  *Drive
	id     object.ShardID
	file   *os.File
	tmp    string
	block  []byte // data of the block being filled
	index  uint32 // shard index as the checksum covers it
	blocks uint64 // blocks already written
	done   bool
	// replace allows the shard to exist: the commit replaces it.
	replace bool
}

// Write implements ports.ShardWriter.
func (w *shardWriter) Write(p []byte) (int, error) {
	if w.done {
		return 0, fmt.Errorf("disk.Write: %w: shard %s is closed", domain.ErrInvalidArgument, w.id)
	}
	written := 0
	for len(p) > 0 {
		n := copy(w.block[len(w.block):cap(w.block)], p)
		w.block = w.block[:len(w.block)+n]
		p = p[n:]
		written += n
		if len(w.block) == cap(w.block) {
			if err := w.flush(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// flush writes the current block with its checksum.
func (w *shardWriter) flush() error {
	sum := blockChecksum(w.id.DataID, w.index, w.blocks, w.block)
	if _, err := w.file.Write(sum[:]); err != nil {
		return fmt.Errorf("disk.Write: %w", err)
	}
	if _, err := w.file.Write(w.block); err != nil {
		return fmt.Errorf("disk.Write: %w", err)
	}
	w.blocks++
	w.block = w.block[:0]
	return nil
}

// Commit implements ports.ShardWriter: it flushes the data to stable storage,
// then makes the shard visible under its identifier.
func (w *shardWriter) Commit(ctx context.Context) error {
	if w.done {
		return fmt.Errorf("disk.Commit: %w: shard %s is closed", domain.ErrInvalidArgument, w.id)
	}
	err := w.finish(ctx)
	w.done = true
	if err != nil {
		_ = w.file.Close()
		_ = w.drive.root.Remove(w.tmp)
		return fmt.Errorf("disk.Commit: shard %s: %w", w.id, err)
	}
	return nil
}

func (w *shardWriter) finish(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(w.block) > 0 {
		if err := w.flush(); err != nil {
			return err
		}
	}
	if err := w.file.Sync(); err != nil {
		return err
	}
	if err := w.file.Close(); err != nil {
		return err
	}
	return w.drive.publish(w.tmp, w.id, w.replace)
}

// Abort implements ports.ShardWriter.
func (w *shardWriter) Abort() error {
	if w.done {
		return nil
	}
	w.done = true
	closeErr := w.file.Close()
	if err := w.drive.root.Remove(w.tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("disk.Abort: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("disk.Abort: %w", closeErr)
	}
	return nil
}
