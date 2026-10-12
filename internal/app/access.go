package app

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

const (
	// DefaultAccessFlushInterval is how often the reads a node counted are
	// handed to the metadata store, unless the deployment says otherwise.
	DefaultAccessFlushInterval = 30 * time.Second
	// MinAccessFlushInterval is the shortest interval accepted: every flush
	// is an entry of the metadata log.
	MinAccessFlushInterval = 5 * time.Second
	// MaxBufferedReads bounds how many aggregates - reads of one version by
	// one reader - a node holds that the store has not taken yet. Beyond it
	// a read that would need a new one is not counted.
	MaxBufferedReads = 10_000

	// accessFlushTimeout bounds one batch on its way to the store.
	accessFlushTimeout = 30 * time.Second
	// accessStopTimeout bounds the flush a node makes when it stops.
	accessStopTimeout = 10 * time.Second
)

// readKey names what is counted together: the reads of one version by one
// reader, those made with presigned URLs apart.
type readKey struct {
	bucket string
	key    string
	// ref is the version as clients name it, id the ID it has of its own:
	// "null" may name another version by the time the count is recorded.
	ref       object.VersionRef
	id        object.ID
	who       string
	presigned bool
}

// readCount is how often something was read.
type readCount struct {
	downloads int64
	copies    int64
}

// AccessRecorder counts who read which version and hands the counts to the
// metadata store now and then (ADR-0016).
//
// Counting is an update of a map in memory: nothing on the path of a read
// proposes an entry, waits for one, or can fail because of one. What a node
// counted between two flushes is lost when it crashes, and what it cannot
// hand over is dropped beyond a bound - the record is a count kept as well as
// can be, not evidence.
type AccessRecorder struct {
	store  ports.ObjectAccessStore
	node   string
	boot   uint64
	logger *slog.Logger
	onDrop func(reads int64)
	ready  func(context.Context) error
	tick   func(time.Duration) <-chan time.Time

	// flushing is held for a flush: one batch is on its way at a time.
	flushing sync.Mutex

	mu     sync.Mutex
	counts map[readKey]readCount
	// pending is the batch that was sent and not known to be recorded. It
	// is sent again exactly as it is, under its number, until the store
	// has taken it: the store records a batch it has seen as nothing.
	pending *ports.ReadBatch
	// seq numbers the batches since the node started.
	seq uint64
	// failing says that the last flush failed, so that a store that stays
	// away is reported once and not every interval.
	failing bool
}

// AccessRecorderConfig lists what an AccessRecorder depends on.
type AccessRecorderConfig struct {
	// Store takes the counts.
	Store ports.ObjectAccessStore
	// Node is the ID of this node: a batch of reads is its node's.
	Node string
	// Entropy is where the number that tells this start of the node from
	// every other is read from.
	Entropy io.Reader
	Logger  *slog.Logger
	// OnDrop, when not nil, is told how many reads were not counted.
	OnDrop func(reads int64)
	// Ready, when not nil, says whether the store can be given anything
	// yet. While it returns an error nothing is sent.
	Ready func(context.Context) error
	// Tick, when not nil, replaces the timer between two flushes.
	Tick func(time.Duration) <-chan time.Time
}

// NewAccessRecorder returns an AccessRecorder. Its Run method must be running
// for the counts to reach the store.
func NewAccessRecorder(cfg AccessRecorderConfig) (*AccessRecorder, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("app.NewAccessRecorder: %w: no store", domain.ErrInvalidArgument)
	}
	if err := cluster.ValidateNodeID(cfg.Node); err != nil {
		return nil, fmt.Errorf("app.NewAccessRecorder: %w", err)
	}
	if cfg.Entropy == nil {
		return nil, fmt.Errorf("app.NewAccessRecorder: %w: no source of randomness", domain.ErrInvalidArgument)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	var boot [8]byte
	if _, err := io.ReadFull(cfg.Entropy, boot[:]); err != nil {
		return nil, fmt.Errorf("app.NewAccessRecorder: read randomness: %w", err)
	}
	tick := cfg.Tick
	if tick == nil {
		tick = time.After
	}
	return &AccessRecorder{
		store: cfg.Store, node: cfg.Node, boot: binary.BigEndian.Uint64(boot[:]), logger: cfg.Logger,
		onDrop: cfg.OnDrop, ready: cfg.Ready, tick: tick, counts: map[readKey]readCount{},
	}, nil
}

// Download counts one download of a version by a principal. It never blocks
// on anything but the recorder's own lock and never fails.
func (r *AccessRecorder) Download(principal iam.Principal, version object.Version) {
	r.add(principal, version, readCount{downloads: 1})
}

// Copy counts one use of a version as the source of a copy.
func (r *AccessRecorder) Copy(principal iam.Principal, version object.Version) {
	r.add(principal, version, readCount{copies: 1})
}

func (r *AccessRecorder) add(principal iam.Principal, version object.Version, more readCount) {
	// What the store reads for itself is nobody's read: a node, or a
	// service of the deployment, is never a reader.
	if r == nil || principal.NodeID != "" || principal.Service != "" || version.DeleteMarker || version.VersionID.IsZero() {
		return
	}
	key := readKey{
		bucket: version.Bucket, key: version.Key, ref: version.Ref(), id: version.VersionID,
		who: actorOf(principal), presigned: principal.Session.Presigned && !principal.Anonymous(),
	}
	if !r.count(key, more) {
		if r.onDrop != nil {
			r.onDrop(more.downloads + more.copies)
		}
	}
}

// count adds to an aggregate and reports whether it could.
func (r *AccessRecorder) count(key readKey, more readCount) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	had, known := r.counts[key]
	sum := readCount{downloads: had.downloads + more.downloads, copies: had.copies + more.copies}
	held := len(r.counts)
	if r.pending != nil {
		held += len(r.pending.Items)
	}
	read := object.Read{Who: key.who, Presigned: key.presigned, Downloads: sum.downloads, Copies: sum.copies}
	// A new aggregate beyond the bound, and a count the store would refuse
	// whole, are left out.
	if (!known && held >= MaxBufferedReads) || read.Validate() != nil {
		return false
	}
	r.counts[key] = sum
	return true
}

// Buffered returns how many aggregates wait to be recorded.
func (r *AccessRecorder) Buffered() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	held := len(r.counts)
	if r.pending != nil {
		held += len(r.pending.Items)
	}
	return held
}

// next returns the batch to send: the one whose outcome is not known, or a
// new one of what was counted since.
func (r *AccessRecorder) next() (ports.ReadBatch, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending != nil {
		return *r.pending, true
	}
	if len(r.counts) == 0 {
		return ports.ReadBatch{}, false
	}
	r.seq++
	batch := ports.ReadBatch{Node: r.node, Boot: r.boot, Seq: r.seq, Items: make([]ports.ObjectRead, 0, min(len(r.counts), ports.MaxReadBatch))}
	for key, n := range r.counts {
		if len(batch.Items) == ports.MaxReadBatch {
			break
		}
		batch.Items = append(batch.Items, ports.ObjectRead{
			Bucket: key.bucket, Key: key.key, Version: key.ref, VersionID: key.id,
			Read: object.Read{Who: key.who, Presigned: key.presigned, Downloads: n.downloads, Copies: n.copies},
		})
		delete(r.counts, key)
	}
	r.pending = &batch
	return batch, true
}

// settle forgets the batch that was sent: the store has taken it, or refused
// it for good.
func (r *AccessRecorder) settle() {
	r.mu.Lock()
	r.pending = nil
	r.mu.Unlock()
}

// Flush hands what was counted to the store, a batch at a time. A batch
// that fails, or whose outcome is not known, is kept and sent again by the
// next flush under the same number, so that it is recorded once.
func (r *AccessRecorder) Flush(ctx context.Context) error {
	r.flushing.Lock()
	defer r.flushing.Unlock()
	if r.ready != nil {
		if err := r.ready(ctx); err != nil {
			return fmt.Errorf("access.Flush: %w", err)
		}
	}
	for {
		batch, more := r.next()
		if !more {
			return nil
		}
		sendCtx, cancel := context.WithTimeout(ctx, accessFlushTimeout)
		err := r.store.AddObjectReads(sendCtx, batch)
		cancel()
		switch {
		case err == nil:
			r.settle()
		case errors.Is(err, domain.ErrInvalidArgument) && !errors.Is(err, domain.ErrOutcomeUnknown):
			// Refused as what it is: sending it again would be refused
			// again, and would keep everything counted after it waiting.
			r.settle()
			var reads int64
			for _, item := range batch.Items {
				reads += item.Read.Downloads + item.Read.Copies
			}
			if r.onDrop != nil {
				r.onDrop(reads)
			}
			r.logger.ErrorContext(ctx, "a batch of object reads was refused and is dropped",
				slog.Int("aggregates", len(batch.Items)), slog.Any("error", err))
		default:
			return fmt.Errorf("access.Flush: %w", err)
		}
	}
}

// Run flushes every interval until ctx is cancelled, and then once more: a
// node that stops in good order hands over what it counted.
func (r *AccessRecorder) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultAccessFlushInterval
	}
	for {
		select {
		case <-ctx.Done():
			r.stop(ctx)
			return
		case <-r.tick(interval):
			r.flushed(ctx, r.Flush(ctx))
		}
	}
}

// stop is the flush of a node that stops. Its context has ended; the flush
// gets a short time of its own.
func (r *AccessRecorder) stop(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), accessStopTimeout)
	defer cancel()
	if err := r.Flush(ctx); err != nil && r.Buffered() > 0 {
		r.logger.WarnContext(ctx, "object reads counted on this node were not recorded before it stopped",
			slog.Int("aggregates", r.Buffered()), slog.Any("error", err))
	}
}

// flushed reports a flush that failed, once for as long as it keeps failing.
func (r *AccessRecorder) flushed(ctx context.Context, err error) {
	r.mu.Lock()
	was := r.failing
	r.failing = err != nil
	r.mu.Unlock()
	if err == nil || was || ctx.Err() != nil {
		return
	}
	// Not an error of the node: with no leader, or while a member of the
	// cluster does not know such records yet, the counts wait.
	r.logger.WarnContext(ctx, "object reads could not be recorded; they are kept and sent again",
		slog.Int("aggregates", r.Buffered()), slog.Any("error", err))
}

// countedBody is the body of a download: closing it counts the read, once.
// A transfer that was cut short counts as well, because bytes left the store.
type countedBody struct {
	io.ReadCloser
	once  sync.Once
	count func()
}

// Close implements io.Closer.
func (b *countedBody) Close() error {
	b.once.Do(b.count)
	return b.ReadCloser.Close()
}

// ObjectAccess is what is known of who made a version and who read it.
type ObjectAccess struct {
	// Version is the version asked about: its CreatedBy is who uploaded
	// it, empty when that was not recorded, and its ModifiedAt is when.
	Version object.Version
	// Access is how often it was read, by whom and when last.
	Access object.Access
}

// Access returns who uploaded a version and who read it: the version named
// as clients name one, or the current version for "". It takes a permission
// of its own, which reading the object does not give, and it is asked for
// before anything is said about the request or the object - whether the
// object exists included.
func (s *ObjectService) Access(ctx context.Context, principal iam.Principal, bucketName, key, versionID string) (ObjectAccess, error) {
	resource := iam.Resource{Bucket: bucketName, Key: key}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: principal, Action: iam.ActionGetObjectAccess, Resource: resource}); err != nil {
		return ObjectAccess{}, fmt.Errorf("object.Access: %w", err)
	}
	if s.accessStore == nil {
		return ObjectAccess{}, fmt.Errorf("object.Access: %w: this node keeps no record of reads", domain.ErrUnavailable)
	}
	version, err := s.accessTarget(ctx, bucketName, key, versionID)
	if err != nil {
		return ObjectAccess{}, fmt.Errorf("object.Access: %w", err)
	}
	access, err := s.accessStore.ObjectAccess(ctx, bucketName, key, version.Ref())
	if err != nil {
		return ObjectAccess{}, fmt.Errorf("object.Access: %w", err)
	}
	return ObjectAccess{Version: version, Access: access}, nil
}

// accessTarget finds the version a question about its readers is about.
func (s *ObjectService) accessTarget(ctx context.Context, bucketName, key, versionID string) (object.Version, error) {
	if err := object.ValidateKey(key); err != nil {
		return object.Version{}, err
	}
	if versionID == "" {
		return s.index.GetObject(ctx, bucketName, key)
	}
	ref, err := object.ParseVersionRef(versionID)
	if err != nil {
		return object.Version{}, err
	}
	version, err := s.index.GetVersion(ctx, bucketName, key, ref)
	if err != nil {
		return object.Version{}, err
	}
	if version.DeleteMarker {
		return object.Version{}, fmt.Errorf("%w: version %s of %s/%s is a delete marker", domain.ErrMethodNotAllowed, ref, bucketName, key)
	}
	return version, nil
}
