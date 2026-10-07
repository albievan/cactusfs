package noderpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	nodev1 "github.com/albievan/cactusfs/internal/adapters/noderpc/nodev1/cactusfs/node/v1"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// remoteDrive is a drive attached to another node.
type remoteDrive struct {
	client  *Client
	nodeID  string
	driveID string
}

var _ ports.ShardStore = (*remoteDrive)(nil)

// Drive returns the drive with the given ID on another node as a
// ports.ShardStore. Nothing is contacted until the store is used.
//
// A failure to reach the node wraps domain.ErrDriveOffline: it says nothing
// about the shards on the drive, and nobody may conclude from it that one is
// missing or damaged.
func (c *Client) Drive(nodeID, driveID string) ports.ShardStore {
	return &remoteDrive{client: c, nodeID: nodeID, driveID: driveID}
}

// DriveID implements ports.ShardStore.
func (d *remoteDrive) DriveID() string { return d.driveID }

// offline marks a failure to reach the node as saying nothing about the
// drive's content.
func (d *remoteDrive) offline(err error) error {
	return fmt.Errorf("%w: drive %s: %w", domain.ErrDriveOffline, d.driveID, err)
}

// failed sorts out a call's result: unreachable node, or the node's answer.
//
// A call that failed because its caller gave up is neither: it is the
// caller's own cancellation, and says nothing about the node.
func (d *remoteDrive) failed(ctx context.Context, rpcErr error, reported *nodev1.Error) error {
	if rpcErr != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		return d.offline(unreachable(d.nodeID, rpcErr))
	}
	return fromWire(d.nodeID, reported)
}

func (d *remoteDrive) storage(ctx context.Context) (nodev1.StorageServiceClient, error) {
	conn, err := d.client.ready(ctx, d.nodeID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, d.offline(err)
	}
	return nodev1.NewStorageServiceClient(conn), nil
}

// Create implements ports.ShardStore.
func (d *remoteDrive) Create(ctx context.Context, id object.ShardID) (ports.ShardWriter, error) {
	return d.write(ctx, id, false)
}

// Rewrite implements ports.ShardStore.
func (d *remoteDrive) Rewrite(ctx context.Context, id object.ShardID) (ports.ShardWriter, error) {
	return d.write(ctx, id, true)
}

func (d *remoteDrive) write(ctx context.Context, id object.ShardID, rewrite bool) (ports.ShardWriter, error) {
	storage, err := d.storage(ctx)
	if err != nil {
		return nil, err
	}
	// The stream lives as long as the writer, not as long as this call;
	// cancelling it is how the writer aborts.
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	stream, err := storage.WriteShard(streamCtx)
	if err == nil {
		err = stream.Send(&nodev1.WriteShardRequest{Step: &nodev1.WriteShardRequest_Header{
			Header: &nodev1.WriteShardHeader{DriveId: d.driveID, Shard: shardToWire(id), Rewrite: rewrite},
		}})
	}
	var accepted *nodev1.WriteShardResponse
	if err == nil {
		accepted, err = stream.Recv()
	}
	if err := d.failed(ctx, err, accepted.GetError()); err != nil {
		stop()
		cancel()
		return nil, err
	}
	return &remoteWriter{drive: d, stream: stream, cancel: cancel, stop: stop}, nil
}

// remoteWriter writes one shard on another node.
type remoteWriter struct {
	drive  *remoteDrive
	stream nodev1.StorageService_WriteShardClient
	cancel context.CancelFunc
	stop   func() bool
	done   bool
}

// Write implements io.Writer.
func (w *remoteWriter) Write(p []byte) (int, error) {
	if w.done {
		return 0, fmt.Errorf("%w: write to a finished shard", domain.ErrInvalidArgument)
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxChunk)
		err := w.stream.Send(&nodev1.WriteShardRequest{Step: &nodev1.WriteShardRequest_Chunk{Chunk: p[:n]}})
		if err != nil {
			// The node ended the stream; what it said is waiting to be read.
			return written, w.broken(err)
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// broken explains a stream that failed while sending.
func (w *remoteWriter) broken(sendErr error) error {
	if resp, err := w.stream.Recv(); err == nil && resp.GetError() != nil {
		return fromWire(w.drive.nodeID, resp.GetError())
	}
	return w.drive.offline(unreachable(w.drive.nodeID, sendErr))
}

// Commit implements ports.ShardWriter. It returns nil only when the other
// node has answered that the shard is on stable storage.
func (w *remoteWriter) Commit(ctx context.Context) error {
	if w.done {
		return fmt.Errorf("%w: shard already finished", domain.ErrInvalidArgument)
	}
	w.done = true
	defer w.release()
	if err := w.stream.Send(&nodev1.WriteShardRequest{Step: &nodev1.WriteShardRequest_Commit{Commit: &nodev1.WriteShardCommit{}}}); err != nil {
		return w.broken(err)
	}
	resp, err := w.stream.Recv()
	return w.drive.failed(ctx, err, resp.GetError())
}

// Abort implements ports.ShardWriter.
func (w *remoteWriter) Abort() error {
	if w.done {
		return nil
	}
	w.done = true
	w.release()
	return nil
}

func (w *remoteWriter) release() {
	w.stop()
	w.cancel()
}

// Open implements ports.ShardStore.
func (d *remoteDrive) Open(ctx context.Context, id object.ShardID) (ports.ShardReader, error) {
	storage, err := d.storage(ctx)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := storage.StatShard(callCtx, &nodev1.StatShardRequest{DriveId: d.driveID, Shard: shardToWire(id)})
	if err := d.failed(ctx, err, resp.GetError()); err != nil {
		return nil, err
	}
	// io.ReaderAt has no context; reads go on under the one the shard was
	// opened with, as they do on a local drive.
	return &remoteReader{drive: d, ctx: ctx, id: id, size: resp.GetSize()}, nil
}

// remoteReader reads one shard on another node. The bytes were verified by
// the node that stores them, and travel under TLS.
type remoteReader struct {
	drive *remoteDrive
	ctx   context.Context
	id    object.ShardID
	size  int64
}

// Size implements ports.ShardReader.
func (r *remoteReader) Size() int64 { return r.size }

// Close implements io.Closer.
func (r *remoteReader) Close() error { return nil }

// ReadAt implements io.ReaderAt.
func (r *remoteReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("%w: negative offset", domain.ErrInvalidArgument)
	}
	read := 0
	for read < len(p) {
		if off+int64(read) >= r.size {
			return read, io.EOF
		}
		storage, err := r.drive.storage(r.ctx)
		if err != nil {
			return read, err
		}
		want := min(len(p)-read, maxReadLength)
		ctx, cancel := withTimeout(r.ctx)
		resp, err := storage.ReadShard(ctx, &nodev1.ReadShardRequest{
			DriveId: r.drive.driveID, Shard: shardToWire(r.id), Offset: off + int64(read), Length: uint32(want),
		})
		cancel()
		if err := r.drive.failed(r.ctx, err, resp.GetError()); err != nil {
			return read, err
		}
		if resp.GetSize() != r.size {
			// The shard was replaced by one of another size since it was opened.
			return read, fmt.Errorf("%w: shard %s changed size while it was read", domain.ErrIntegrity, r.id)
		}
		n := copy(p[read:], resp.GetData())
		if n == 0 {
			return read, io.ErrUnexpectedEOF
		}
		read += n
	}
	return read, nil
}

// Delete implements ports.ShardStore.
func (d *remoteDrive) Delete(ctx context.Context, id object.ShardID) error {
	storage, err := d.storage(ctx)
	if err != nil {
		return err
	}
	callCtx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := storage.DeleteShard(callCtx, &nodev1.DeleteShardRequest{DriveId: d.driveID, Shard: shardToWire(id)})
	return d.failed(ctx, err, resp.GetError())
}

// List implements ports.ShardStore. ModifiedAt is given by this node's
// clock: the other node reports how old each shard is, so that the two
// clocks need not agree.
func (d *remoteDrive) List(ctx context.Context, visit func(ports.ShardInfo) error) error {
	storage, err := d.storage(ctx)
	if err != nil {
		return err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := storage.ListShards(streamCtx, &nodev1.ListShardsRequest{DriveId: d.driveID})
	if err != nil {
		return d.failed(ctx, err, nil)
	}
	now := d.client.clock.Now()
	for {
		batch, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err := d.failed(ctx, err, batch.GetError()); err != nil {
			return err
		}
		for _, shard := range batch.GetShards() {
			id, err := shardFromWire(shard.GetShard())
			if err != nil {
				return err
			}
			info := ports.ShardInfo{ID: id, ModifiedAt: now.Add(-time.Duration(shard.GetAgeNanos()))}
			if err := visit(info); err != nil {
				return err
			}
		}
	}
}
