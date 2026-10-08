package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/erasure"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// shardStorage places the encrypted stream of an object version on drives as
// erasure-coded shards and reads it back.
//
// The stream is cut into stripes; each stripe is split into data chunks and
// extended with parity chunks, and chunk i of every stripe is appended to
// shard i. Shards of one version go to different drives of one erasure set.
// With fewer than four drives there is no parity: a version is one shard on
// one drive.
type shardStorage struct {
	// drives finds the topology and the drives, on this node or another.
	drives ports.DriveSource
	codec  ports.ErasureCodec
	logger *slog.Logger

	// pinned holds the data IDs of writes whose version is not known to be
	// committed or refused yet. Their shards are unreferenced and must not
	// be collected, however long the write takes.
	pinMu  sync.Mutex
	pinned map[object.ID]struct{}
}

// fixedDrives is a DriveSource over a topology that does not change and
// drives that are all attached to this node.
type fixedDrives struct {
	topology cluster.Topology
	drives   map[string]ports.ShardStore
}

// FixedDrives returns a DriveSource for a topology whose drives are all
// given: the arrangement of a node on its own, between two starts. The error
// wraps domain.ErrInvalidArgument when a drive of the topology is missing.
func FixedDrives(topology cluster.Topology, drives []ports.ShardStore) (ports.DriveSource, error) {
	if err := topology.Validate(); err != nil {
		return nil, err
	}
	byID := make(map[string]ports.ShardStore, len(drives))
	for _, drive := range drives {
		if _, dup := byID[drive.DriveID()]; dup || drive.DriveID() == "" {
			return nil, fmt.Errorf("%w: drive ID %q is empty or used twice", domain.ErrInvalidArgument, drive.DriveID())
		}
		byID[drive.DriveID()] = drive
	}
	for _, id := range topology.DriveIDs() {
		if _, ok := byID[id]; !ok {
			return nil, fmt.Errorf("%w: drive %s of the topology is not attached", domain.ErrInvalidArgument, id)
		}
	}
	return fixedDrives{topology: topology, drives: byID}, nil
}

// Topology implements ports.DriveSource.
func (f fixedDrives) Topology(context.Context) (cluster.Topology, error) { return f.topology, nil }

// Drive implements ports.DriveSource.
func (f fixedDrives) Drive(slot cluster.Slot) (ports.ShardStore, bool) {
	drive, ok := f.drives[slot.DriveID]
	return drive, ok
}

// pin protects the shards of a write in progress from the collector.
func (s *shardStorage) pin(dataID object.ID) {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	s.pinned[dataID] = struct{}{}
}

// unpin ends the protection, once the write is committed or definitely not.
func (s *shardStorage) unpin(dataID object.ID) {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	delete(s.pinned, dataID)
}

// isPinned reports whether a write that uses the data ID is unresolved.
func (s *shardStorage) isPinned(dataID object.ID) bool {
	s.pinMu.Lock()
	defer s.pinMu.Unlock()
	_, ok := s.pinned[dataID]
	return ok
}

func newShardStorage(drives ports.DriveSource, codec ports.ErasureCodec, logger *slog.Logger) (*shardStorage, error) {
	if drives == nil {
		return nil, fmt.Errorf("%w: no drives", domain.ErrInvalidArgument)
	}
	return &shardStorage{drives: drives, codec: codec, logger: logger, pinned: map[object.ID]struct{}{}}, nil
}

// located finds the drives that shards are recorded on, in the topology as
// it is now.
type located struct {
	storage  *shardStorage
	topology cluster.Topology
}

func (s *shardStorage) locate(ctx context.Context) (located, error) {
	topology, err := s.drives.Topology(ctx)
	if err != nil {
		return located{}, err
	}
	return located{storage: s, topology: topology}, nil
}

// drive returns the drive that now holds what was recorded under driveID:
// the drive itself, or the one that replaced it in its slot.
//
// A drive the topology has never heard of holds nothing that can be found:
// the error wraps domain.ErrShardNotFound. A drive that is part of the
// topology and not attached is offline, which says nothing about the shards
// on it: the error wraps domain.ErrDriveOffline.
func (l located) drive(driveID string) (ports.ShardStore, error) {
	position, ok := l.topology.Locate(driveID)
	if !ok {
		return nil, fmt.Errorf("%w: drive %s is not part of the topology", domain.ErrShardNotFound, driveID)
	}
	drive, ok := l.storage.drives.Drive(l.topology.Sets[position.Set][position.Slot])
	if !ok {
		return nil, fmt.Errorf("%w: drive %s is not attached", domain.ErrDriveOffline, driveID)
	}
	return drive, nil
}

// geometryOf returns the stripe geometry recorded in a layout. The error
// wraps domain.ErrUnsupportedFormat when this binary cannot read it.
func geometryOf(layout object.Layout) (erasure.Geometry, error) {
	g := erasure.Geometry{
		Params:     erasure.Params{Data: layout.DataShards, Parity: layout.ParityShards},
		StripeSize: layout.StripeSize,
	}
	if err := g.Validate(); err != nil || len(layout.Shards) != g.Total() {
		return erasure.Geometry{}, fmt.Errorf("%w: layout with %d data and %d parity shards in %d locations",
			domain.ErrUnsupportedFormat, layout.DataShards, layout.ParityShards, len(layout.Shards))
	}
	return g, nil
}

// streamWriter writes the encrypted stream of one object version as shards.
type streamWriter struct {
	storage  *shardStorage
	geometry erasure.Geometry
	dataID   object.ID
	// ctx is the context of the request that writes; io.Writer has none.
	ctx context.Context
	// driveIDs and writers are indexed by shard. A nil writer is a shard
	// that could not be written; the write goes on while a write quorum
	// remains.
	driveIDs []string
	writers  []ports.ShardWriter
	stripe   []byte
	written  int64
}

// create starts writing the stream of dataID. The erasure set and the drives
// are chosen by the data ID, which is random, so objects spread evenly.
func (s *shardStorage) create(ctx context.Context, dataID object.ID) (*streamWriter, error) {
	topology, err := s.drives.Topology(ctx)
	if err != nil {
		return nil, err
	}
	params, err := erasure.ParamsFor(topology.SetSize())
	if err != nil {
		return nil, err
	}
	set, positions, err := erasure.Placement(dataID, len(topology.Sets), topology.SetSize(), params.Total())
	if err != nil {
		return nil, err
	}
	w := &streamWriter{
		storage:  s,
		geometry: erasure.Geometry{Params: params, StripeSize: erasure.DefaultStripeSize},
		dataID:   dataID,
		ctx:      ctx,
		driveIDs: make([]string, len(positions)),
		writers:  make([]ports.ShardWriter, len(positions)),
		stripe:   make([]byte, 0, erasure.DefaultStripeSize),
	}
	for i, position := range positions {
		slot := topology.Sets[set][position]
		w.driveIDs[i] = slot.DriveID
		drive, ok := s.drives.Drive(slot)
		if !ok {
			w.lose(ctx, i, "create", fmt.Errorf("%w: drive %s is not attached", domain.ErrDriveOffline, slot.DriveID))
			continue
		}
		writer, err := drive.Create(ctx, object.ShardID{DataID: dataID, Index: i})
		if err != nil {
			w.lose(ctx, i, "create", err)
			continue
		}
		w.writers[i] = writer
	}
	if err := w.quorum(); err != nil {
		w.abort()
		return nil, err
	}
	return w, nil
}

// lose gives up on one shard of the write in progress.
func (w *streamWriter) lose(ctx context.Context, index int, step string, err error) {
	if w.writers[index] != nil {
		_ = w.writers[index].Abort()
		w.writers[index] = nil
	}
	w.storage.logger.WarnContext(ctx, "shard write failed; the object will be stored degraded if a write quorum remains",
		slog.String("shard", object.ShardID{DataID: w.dataID, Index: index}.String()),
		slog.String("drive", w.driveIDs[index]), slog.String("step", step), slog.Any("error", err))
}

func (w *streamWriter) alive() int {
	n := 0
	for _, writer := range w.writers {
		if writer != nil {
			n++
		}
	}
	return n
}

// quorum reports an error when too few shards are still being written for
// the write to be allowed to commit.
func (w *streamWriter) quorum() error {
	if alive, need := w.alive(), w.geometry.WriteQuorum(); alive < need {
		return fmt.Errorf("%w: only %d of %d shards can be written, the write quorum is %d",
			domain.ErrInsufficientDrives, alive, w.geometry.Total(), need)
	}
	return nil
}

// Write implements io.Writer.
func (w *streamWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := copy(w.stripe[len(w.stripe):cap(w.stripe)], p)
		w.stripe = w.stripe[:len(w.stripe)+n]
		p = p[n:]
		written += n
		if len(w.stripe) == cap(w.stripe) {
			if err := w.flush(w.ctx); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// flush encodes the buffered stripe and appends its chunks to the shards.
func (w *streamWriter) flush(ctx context.Context) error {
	if len(w.stripe) == 0 {
		return nil
	}
	chunks, err := w.storage.codec.Encode(w.geometry.Params, w.stripe)
	if err != nil {
		return err
	}
	for i, writer := range w.writers {
		if writer == nil {
			continue
		}
		if _, err := writer.Write(chunks[i]); err != nil {
			w.lose(ctx, i, "write", err)
		}
	}
	w.written += int64(len(w.stripe))
	w.stripe = w.stripe[:0]
	return w.quorum()
}

// commit makes the shards durable and returns where they are. It succeeds
// when at least a write quorum of shards is durable; only then may the object
// version be proposed to Raft. degraded reports that some shard is missing
// and must be rebuilt.
func (w *streamWriter) commit(ctx context.Context) (layout object.Layout, degraded bool, err error) {
	if err := w.flush(ctx); err != nil {
		w.abort()
		return object.Layout{}, false, err
	}
	for i, writer := range w.writers {
		if writer == nil {
			continue
		}
		if err := writer.Commit(ctx); err != nil {
			w.lose(ctx, i, "commit", err)
		}
	}
	layout = object.Layout{
		DataShards:   w.geometry.Data,
		ParityShards: w.geometry.Parity,
		StripeSize:   w.geometry.StripeSize,
		StoredSize:   w.written,
		Shards:       make([]object.ShardLocation, len(w.driveIDs)),
	}
	shardSize := w.geometry.ShardSize(w.written)
	for i, driveID := range w.driveIDs {
		layout.Shards[i] = object.ShardLocation{DriveID: driveID, Size: shardSize}
	}
	if err := w.quorum(); err != nil {
		// Committed shards of a write that failed as a whole are removed.
		w.storage.remove(ctx, w.dataID, layout)
		w.writers = make([]ports.ShardWriter, len(w.writers))
		return object.Layout{}, false, err
	}
	degraded = w.alive() < w.geometry.Total()
	w.writers = make([]ports.ShardWriter, len(w.writers))
	return layout, degraded, nil
}

// abort discards a stream that was not committed.
func (w *streamWriter) abort() {
	for i, writer := range w.writers {
		if writer != nil {
			_ = writer.Abort()
			w.writers[i] = nil
		}
	}
}

// streamReader reads the encrypted stream of a version from its shards. It
// needs any Data of them: when a data shard is missing or fails verification
// the stripe is rebuilt from parity.
type streamReader struct {
	storage  *shardStorage
	geometry erasure.Geometry
	extent   object.Extent

	mu sync.Mutex
	// readers is indexed by shard; nil is a shard that cannot be used.
	readers  []ports.ShardReader
	degraded bool
	// offline counts the shards that could not be reached, as opposed to
	// shards that are missing or bad.
	offline int
	// The stripe decoded last, for sequential reads.
	cached int64
	stripe []byte
}

// open returns a reader over the encrypted stream of stored data.
//
// When no shard can be found at all the error wraps domain.ErrShardNotFound,
// which is what a version looks like just after it was replaced or deleted.
// When too few are usable only because drives cannot be reached, it wraps
// domain.ErrUnavailable: nothing is known to be wrong with the object. When
// shards are missing or bad beyond what parity covers it wraps
// domain.ErrIntegrity: the object cannot be read.
func (s *shardStorage) open(ctx context.Context, v object.Extent) (*streamReader, error) {
	geometry, err := geometryOf(v.Layout)
	if err != nil {
		return nil, err
	}
	where, err := s.locate(ctx)
	if err != nil {
		return nil, err
	}
	shardSize := geometry.ShardSize(v.Layout.StoredSize)
	r := &streamReader{
		storage: s, geometry: geometry, extent: v,
		readers: make([]ports.ShardReader, geometry.Total()), cached: -1,
	}
	usable, found := 0, 0
	var lastErr error
	for i, location := range v.Layout.Shards {
		reader, err := where.openShard(ctx, v.DataID, i, location, shardSize)
		if err != nil {
			switch {
			case errors.Is(err, domain.ErrDriveOffline):
				r.offline++
			case !errors.Is(err, domain.ErrShardNotFound):
				found++
			}
			lastErr = err
			r.degraded = true
			continue
		}
		r.readers[i] = reader
		usable++
		found++
	}
	if usable >= geometry.ReadQuorum() {
		return r, nil
	}
	_ = r.Close()
	switch {
	case usable+r.offline >= geometry.ReadQuorum():
		return nil, fmt.Errorf("%w: %d of %d shards of %s are on drives that cannot be reached, %d are needed (last error: %w)",
			domain.ErrUnavailable, r.offline, geometry.Total(), v.DataID, geometry.ReadQuorum(), lastErr)
	case found == 0 && r.offline == 0:
		return nil, fmt.Errorf("%w: no shard of %s is present", domain.ErrShardNotFound, v.DataID)
	default:
		return nil, fmt.Errorf("%w: %d of %d shards of %s are usable, %d are needed (last error: %w)",
			domain.ErrIntegrity, usable, geometry.Total(), v.DataID, geometry.ReadQuorum(), lastErr)
	}
}

// openShard opens one shard and checks that it has the size the layout
// records.
func (l located) openShard(
	ctx context.Context, dataID object.ID, index int, location object.ShardLocation, shardSize int64,
) (ports.ShardReader, error) {
	drive, err := l.drive(location.DriveID)
	if err != nil {
		return nil, err
	}
	reader, err := drive.Open(ctx, object.ShardID{DataID: dataID, Index: index})
	if err != nil {
		return nil, err
	}
	if reader.Size() != shardSize || location.Size != shardSize {
		_ = reader.Close()
		return nil, fmt.Errorf("%w: shard %d is %d bytes, expected %d", domain.ErrIntegrity, index, reader.Size(), shardSize)
	}
	return reader, nil
}

// Degraded reports whether any shard was missing or failed verification
// while reading. A degraded version should be healed.
func (r *streamReader) Degraded() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.degraded
}

// Close releases the shards.
func (r *streamReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	for i, reader := range r.readers {
		if reader != nil {
			errs = append(errs, reader.Close())
			r.readers[i] = nil
		}
	}
	return errors.Join(errs...)
}

// ReadAt implements io.ReaderAt over the encrypted stream.
func (r *streamReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("%w: negative offset", domain.ErrInvalidArgument)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	size := r.extent.Layout.StoredSize
	read := 0
	for read < len(p) {
		pos := off + int64(read)
		if pos >= size {
			return read, io.EOF
		}
		stripe := pos / int64(r.geometry.StripeSize)
		data, err := r.stripeData(stripe)
		if err != nil {
			return read, err
		}
		read += copy(p[read:], data[pos-stripe*int64(r.geometry.StripeSize):])
	}
	return read, nil
}

// stripeData returns the stream bytes of one stripe.
func (r *streamReader) stripeData(stripe int64) ([]byte, error) {
	if r.cached == stripe {
		return r.stripe, nil
	}
	r.cached = -1
	chunks, err := r.readStripe(stripe, false)
	if err != nil {
		return nil, err
	}
	length := r.geometry.StripeLen(r.extent.Layout.StoredSize, stripe)
	r.stripe = r.stripe[:0]
	for i := 0; i < r.geometry.Data; i++ {
		r.stripe = append(r.stripe, chunks[i]...)
	}
	r.stripe = r.stripe[:length]
	r.cached = stripe
	return r.stripe, nil
}

// readStripe reads the chunks of one stripe from the shards and rebuilds the
// ones that are missing: the data chunks, or with all set every chunk.
// Chunks come back verified, because every shard reader verifies what it
// returns; a shard that fails is dropped for the rest of the read.
func (r *streamReader) readStripe(stripe int64, all bool) ([][]byte, error) {
	g := r.geometry
	chunk := g.Chunk(g.StripeLen(r.extent.Layout.StoredSize, stripe))
	offset := g.ShardOffset(stripe)
	chunks := make([][]byte, g.Total())

	read := func(i int) bool {
		if r.readers[i] == nil {
			return false
		}
		buf := make([]byte, chunk)
		if _, err := r.readers[i].ReadAt(buf, offset); err != nil {
			// Missing, truncated or damaged: use the other shards.
			r.storage.logger.Warn("shard read failed; reading from the remaining shards",
				slog.String("shard", object.ShardID{DataID: r.extent.DataID, Index: i}.String()), slog.Any("error", err))
			_ = r.readers[i].Close()
			r.readers[i] = nil
			r.degraded = true
			if errors.Is(err, domain.ErrDriveOffline) {
				r.offline++
			}
			return false
		}
		chunks[i] = buf
		return true
	}

	// The data shards alone are enough when they are all intact.
	have := 0
	for i := 0; i < g.Data; i++ {
		if read(i) {
			have++
		}
	}
	if have == g.Data && !all {
		return chunks, nil
	}
	for i := g.Data; i < g.Total() && (all || have < g.Data); i++ {
		if read(i) {
			have++
		}
	}
	if err := r.storage.codec.Reconstruct(g.Params, chunks, !all); err != nil {
		usable := 0
		for _, chunk := range chunks {
			if chunk != nil {
				usable++
			}
		}
		if usable+r.offline >= g.ReadQuorum() {
			// Enough shards would be there if their drives could be reached.
			return nil, fmt.Errorf("%w: stripe %d of %s: %d shards are on drives that cannot be reached",
				domain.ErrUnavailable, stripe, r.extent.DataID, r.offline)
		}
		return nil, fmt.Errorf("%w: stripe %d of %s cannot be rebuilt: %w", domain.ErrIntegrity, stripe, r.extent.DataID, err)
	}
	return chunks, nil
}

// remove deletes the shards of a version that is no longer, or never became,
// visible. It is best effort: a shard that cannot be deleted now is left for
// the collector and is never a visible object, so the failure is logged and
// not returned.
func (s *shardStorage) remove(ctx context.Context, dataID object.ID, layout object.Layout) {
	// The request that led here may already be cancelled; the cleanup is
	// still owed.
	ctx = context.WithoutCancel(ctx)
	where, err := s.locate(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "shards left behind for garbage collection", slog.String("data_id", dataID.String()), slog.Any("error", err))
		return
	}
	for index, location := range layout.Shards {
		drive, err := where.drive(location.DriveID)
		if err != nil {
			continue
		}
		id := object.ShardID{DataID: dataID, Index: index}
		if err := drive.Delete(ctx, id); err != nil && !errors.Is(err, domain.ErrShardNotFound) {
			s.logger.WarnContext(ctx, "shard left behind for garbage collection",
				slog.String("shard", id.String()), slog.String("drive", location.DriveID), slog.Any("error", err))
		}
	}
}

var (
	_ io.Writer   = (*streamWriter)(nil)
	_ io.ReaderAt = (*streamReader)(nil)
)

// throttle is told how many stored bytes were just read, and may wait.
type throttle func(ctx context.Context, bytes int64)

// verifyShard reads a shard from beginning to end, which makes the drive
// verify every block of it. It returns the number of bytes verified.
func verifyShard(ctx context.Context, reader ports.ShardReader, slow throttle) (int64, error) {
	buf := make([]byte, 1<<20)
	off := int64(0)
	for off < reader.Size() {
		if err := ctx.Err(); err != nil {
			return off, err
		}
		n, err := reader.ReadAt(buf[:min(int64(len(buf)), reader.Size()-off)], off)
		off += int64(n)
		if err != nil && (!errors.Is(err, io.EOF) || off != reader.Size()) {
			return off, err
		}
		if slow != nil {
			slow(ctx, int64(n))
		}
	}
	return off, nil
}

// isDamage reports whether an error from opening or reading a shard means
// that the shard itself is missing or bad. Anything else - too many open
// files, an I/O error, a permission problem - says nothing about the shard,
// and a shard is never rewritten on such an error.
func isDamage(err error) bool {
	return errors.Is(err, domain.ErrShardNotFound) || errors.Is(err, domain.ErrIntegrity)
}

// repair is the outcome of healing one version.
type repair struct {
	// shards is the number of shards the version has.
	shards int
	// damaged is the number that were missing, truncated or corrupt.
	damaged int
	// offline is the number that could not be reached: their state is not
	// known, and they were neither used nor rebuilt.
	offline int
	// rebuilt lists the shards that were written again.
	rebuilt []int
	// verified is the number of stored bytes that were read and verified.
	verified int64
}

// heal verifies every shard of a version and rebuilds the ones that are
// missing, truncated or corrupt from the others.
//
// It never makes an object worse. A shard is rebuilt only when it is
// definitely damaged; an error that says nothing about the shard stops the
// heal with nothing changed. A shard on a drive that cannot be reached is
// left out: it is not used, not rebuilt and not counted as damaged, and the
// rest of the object is healed without it. A rebuilt shard is written in full beside the
// damaged one and replaces it in one step, so a rebuild that fails leaves
// every shard as it was.
//
// It returns an error wrapping domain.ErrIntegrity when too few shards are
// left to rebuild from, and one wrapping domain.ErrUnavailable when enough
// might be left but cannot be reached now.
func (s *shardStorage) heal(ctx context.Context, v object.Extent, slow throttle) (repair, error) {
	geometry, err := geometryOf(v.Layout)
	if err != nil {
		return repair{}, err
	}
	where, err := s.locate(ctx)
	if err != nil {
		return repair{}, err
	}
	shardSize := geometry.ShardSize(v.Layout.StoredSize)
	result := repair{shards: geometry.Total()}

	reader := &streamReader{
		storage: s, geometry: geometry, extent: v,
		readers: make([]ports.ShardReader, geometry.Total()), cached: -1,
	}
	defer func() { _ = reader.Close() }()

	var damaged []int
	for i, location := range v.Layout.Shards {
		shard, err := where.openShard(ctx, v.DataID, i, location, shardSize)
		if err == nil {
			var verified int64
			verified, err = verifyShard(ctx, shard, slow)
			result.verified += verified
			if err != nil {
				_ = shard.Close()
			}
		}
		switch {
		case err == nil:
			reader.readers[i] = shard
		case isDamage(err):
			damaged = append(damaged, i)
		case errors.Is(err, domain.ErrDriveOffline) && ctx.Err() == nil:
			result.offline++
		default:
			return result, fmt.Errorf("shard %d of %s could not be checked: %w", i, v.DataID, err)
		}
	}
	result.damaged = len(damaged)
	if len(damaged) == 0 {
		return result, nil
	}
	if sources := geometry.Total() - len(damaged) - result.offline; sources < geometry.ReadQuorum() {
		if sources+result.offline >= geometry.ReadQuorum() {
			return result, fmt.Errorf("%w: %d shards of %s are damaged and %d are on drives that cannot be reached; "+
				"too few are left to rebuild from for now", domain.ErrUnavailable, len(damaged), v.DataID, result.offline)
		}
		return result, fmt.Errorf("%w: %d of %d shards of %s are damaged, at most %d can be rebuilt",
			domain.ErrIntegrity, len(damaged), geometry.Total(), v.DataID, geometry.Parity)
	}

	writers := map[int]ports.ShardWriter{}
	defer func() {
		for _, writer := range writers {
			_ = writer.Abort()
		}
	}()
	for _, i := range damaged {
		drive, err := where.drive(v.Layout.Shards[i].DriveID)
		if err != nil {
			continue
		}
		id := object.ShardID{DataID: v.DataID, Index: i}
		writer, err := drive.Rewrite(ctx, id)
		if err != nil {
			s.logger.WarnContext(ctx, "shard cannot be rebuilt", slog.String("shard", id.String()), slog.Any("error", err))
			continue
		}
		writers[i] = writer
	}
	for stripe := int64(0); stripe < geometry.Stripes(v.Layout.StoredSize) && len(writers) > 0; stripe++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		chunks, err := reader.readStripe(stripe, true)
		if err != nil {
			return result, err
		}
		for i, writer := range writers {
			if _, err := writer.Write(chunks[i]); err != nil {
				_ = writer.Abort()
				delete(writers, i)
			}
		}
	}
	for i, writer := range writers {
		if err := writer.Commit(ctx); err != nil {
			s.logger.WarnContext(ctx, "rebuilt shard cannot be committed",
				slog.String("shard", object.ShardID{DataID: v.DataID, Index: i}.String()), slog.Any("error", err))
			continue
		}
		result.rebuilt = append(result.rebuilt, i)
	}
	return result, nil
}

// removeShards deletes the listed shards of a version, best effort.
func (s *shardStorage) removeShards(ctx context.Context, dataID object.ID, layout object.Layout, indexes []int) {
	ctx = context.WithoutCancel(ctx)
	where, err := s.locate(ctx)
	if err != nil {
		return
	}
	for _, index := range indexes {
		drive, err := where.drive(layout.Shards[index].DriveID)
		if err != nil {
			continue
		}
		id := object.ShardID{DataID: dataID, Index: index}
		if err := drive.Delete(ctx, id); err != nil {
			s.logger.WarnContext(ctx, "shard left behind for garbage collection",
				slog.String("shard", id.String()), slog.Any("error", err))
		}
	}
}
