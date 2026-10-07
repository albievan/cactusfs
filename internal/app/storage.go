package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// stripeSize is the number of stream bytes per erasure stripe recorded in new
// layouts.
const stripeSize = 1 << 20

// shardStorage places the encrypted stream of an object version on drives and
// reads it back. Until erasure coding (milestone 2) every version is stored
// as one data shard and no parity: the shard is the stream itself.
type shardStorage struct {
	drives []ports.ShardStore
	byID   map[string]ports.ShardStore
	logger *slog.Logger
}

func newShardStorage(drives []ports.ShardStore, logger *slog.Logger) (*shardStorage, error) {
	if len(drives) == 0 {
		return nil, fmt.Errorf("%w: at least one drive is required", domain.ErrInvalidArgument)
	}
	byID := make(map[string]ports.ShardStore, len(drives))
	for _, drive := range drives {
		if _, dup := byID[drive.DriveID()]; dup || drive.DriveID() == "" {
			return nil, fmt.Errorf("%w: drive ID %q is empty or used twice", domain.ErrInvalidArgument, drive.DriveID())
		}
		byID[drive.DriveID()] = drive
	}
	return &shardStorage{drives: drives, byID: byID, logger: logger}, nil
}

// streamWriter writes the encrypted stream of one object version.
type streamWriter struct {
	drive   ports.ShardStore
	id      object.ShardID
	writer  ports.ShardWriter
	written int64
}

// create starts writing the stream of dataID. The drive is chosen by the
// data ID, which is random, so objects spread evenly.
func (s *shardStorage) create(ctx context.Context, dataID object.ID) (*streamWriter, error) {
	drive := s.drives[int(dataID[0])%len(s.drives)]
	id := object.ShardID{DataID: dataID, Index: 0}
	writer, err := drive.Create(ctx, id)
	if err != nil {
		return nil, err
	}
	return &streamWriter{drive: drive, id: id, writer: writer}, nil
}

// Write implements io.Writer.
func (w *streamWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.written += int64(n)
	return n, err
}

// commit makes the stream durable on its drive and returns where it is. Only
// after commit returns may the object version be proposed to Raft.
func (w *streamWriter) commit(ctx context.Context) (object.Layout, error) {
	if err := w.writer.Commit(ctx); err != nil {
		return object.Layout{}, err
	}
	return object.Layout{
		DataShards:   1,
		ParityShards: 0,
		StripeSize:   stripeSize,
		StoredSize:   w.written,
		Shards:       []object.ShardLocation{{DriveID: w.drive.DriveID(), Size: w.written}},
	}, nil
}

// abort discards a stream that was not committed.
func (w *streamWriter) abort() error {
	return w.writer.Abort()
}

// open returns a reader over the encrypted stream of a version, after
// checking that the stored shard has the recorded size.
func (s *shardStorage) open(ctx context.Context, v object.Version) (ports.ShardReader, error) {
	layout := v.Layout
	if layout.DataShards != 1 || layout.ParityShards != 0 || len(layout.Shards) != 1 {
		return nil, fmt.Errorf("%w: layout with %d data and %d parity shards",
			domain.ErrUnsupportedFormat, layout.DataShards, layout.ParityShards)
	}
	location := layout.Shards[0]
	drive, ok := s.byID[location.DriveID]
	if !ok {
		return nil, fmt.Errorf("%w: drive %s is not attached", domain.ErrShardNotFound, location.DriveID)
	}
	reader, err := drive.Open(ctx, object.ShardID{DataID: v.DataID, Index: 0})
	if err != nil {
		return nil, err
	}
	if reader.Size() != location.Size || location.Size != layout.StoredSize {
		_ = reader.Close()
		return nil, fmt.Errorf("%w: shard is %d bytes, recorded %d", domain.ErrIntegrity, reader.Size(), location.Size)
	}
	return reader, nil
}

// remove deletes the shards of a version that is no longer, or never became,
// visible. It is best effort: a shard that cannot be deleted now is an orphan
// for the collector (PLAN 2.7), never a visible object, so the failure is
// logged and not returned.
func (s *shardStorage) remove(ctx context.Context, dataID object.ID, layout object.Layout) {
	// The request that led here may already be cancelled; the cleanup is
	// still owed.
	ctx = context.WithoutCancel(ctx)
	for index, location := range layout.Shards {
		drive, ok := s.byID[location.DriveID]
		if !ok {
			continue
		}
		id := object.ShardID{DataID: dataID, Index: index}
		if err := drive.Delete(ctx, id); err != nil && !errors.Is(err, domain.ErrShardNotFound) {
			s.logger.WarnContext(ctx, "shard left behind for garbage collection",
				slog.String("shard", id.String()), slog.String("drive", location.DriveID), slog.Any("error", err))
		}
	}
}

var _ io.Writer = (*streamWriter)(nil)
