package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

var testNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// fixture wires the use cases to in-memory fakes.
type fixture struct {
	meta    *porttest.MemMetadata
	drives  []*porttest.MemShardStore
	kms     *porttest.KMS
	authz   *porttest.Authorizer
	clock   *porttest.Clock
	logs    *syncBuffer
	buckets *app.BucketService
	objects *app.ObjectService
	events  *eventLog
	// topology is the drive topology the object service was built with.
	topology cluster.Topology
	heals    *healLog
}

// healLog records the outcome of every heal.
type healLog struct {
	mu      sync.Mutex
	results []app.HealResult
}

func (l *healLog) add(r app.HealResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.results = append(l.results, r)
}

func (l *healLog) list() []app.HealResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]app.HealResult(nil), l.results...)
}

// eventLog records the order of the steps of the write path.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// recordingDrive and recordingIndex note when a shard became durable and when
// a version became visible.
type recordingDrive struct {
	ports.ShardStore
	events *eventLog
}

func (d recordingDrive) Create(ctx context.Context, id object.ShardID) (ports.ShardWriter, error) {
	w, err := d.ShardStore.Create(ctx, id)
	if err != nil {
		return nil, err
	}
	return recordingWriter{ShardWriter: w, events: d.events}, nil
}

func (d recordingDrive) Delete(ctx context.Context, id object.ShardID) error {
	d.events.add("shard.delete")
	return d.ShardStore.Delete(ctx, id)
}

type recordingWriter struct {
	ports.ShardWriter
	events *eventLog
}

func (w recordingWriter) Commit(ctx context.Context) error {
	err := w.ShardWriter.Commit(ctx)
	if err == nil {
		w.events.add("shard.commit")
	}
	return err
}

func (w recordingWriter) Abort() error {
	w.events.add("shard.abort")
	return w.ShardWriter.Abort()
}

type recordingIndex struct {
	ports.ObjectIndex
	events *eventLog
}

func (i recordingIndex) PutVersion(ctx context.Context, v object.Version) (ports.PutResult, error) {
	replaced, err := i.ObjectIndex.PutVersion(ctx, v)
	if err == nil {
		i.events.add("index.put")
	}
	return replaced, err
}

// newFixture wires the use cases over two drives: too few for parity, so
// every object is one shard on one drive.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWithDrives(t, 2)
}

// newFixtureWithDrives wires the use cases over n drives. With four or more
// the objects are erasure coded.
func newFixtureWithDrives(t *testing.T, n int) *fixture {
	t.Helper()
	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatalf("NewKMS: %v", err)
	}
	drives := make([]*porttest.MemShardStore, n)
	ids := make([]string, n)
	for i := range drives {
		ids[i] = "drive-" + string(rune('a'+i))
		drives[i] = porttest.NewMemShardStore(ids[i])
	}
	topology, err := cluster.NewTopology(ids)
	if err != nil {
		t.Fatalf("NewTopology: %v", err)
	}
	f := &fixture{
		meta:     porttest.NewMemMetadata(),
		drives:   drives,
		topology: topology,
		heals:    &healLog{},
		kms:      kms,
		authz:    &porttest.Authorizer{},
		clock:    porttest.NewClock(testNow),
		logs:     &syncBuffer{},
		events:   &eventLog{},
	}
	f.buckets = app.NewBucketService(f.meta, f.authz, f.clock)
	f.objects = f.objectService(t, f.kms)
	return f
}

// objectService builds an ObjectService over the fixture's stores with the
// given KMS, as another node of the same deployment would.
func (f *fixture) objectService(t *testing.T, kms ports.KMS) *app.ObjectService {
	t.Helper()
	return f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) { cfg.KMS = kms })
}

// objectServiceWith builds an ObjectService over the fixture's stores, with
// what change makes different.
func (f *fixture) objectServiceWith(t *testing.T, change func(cfg *app.ObjectServiceConfig)) *app.ObjectService {
	t.Helper()
	drives := make([]ports.ShardStore, len(f.drives))
	for i, d := range f.drives {
		drives[i] = recordingDrive{ShardStore: d, events: f.events}
	}
	cfg := app.ObjectServiceConfig{
		Index:   recordingIndex{ObjectIndex: f.meta, events: f.events},
		Buckets: f.meta,
		Uploads: f.meta,
		Drives:  fixed(t, f.topology, drives),
		Codec:   porttest.NewCodec(),
		OnHeal:  f.heals.add,
		KMS:     f.kms,
		Authz:   f.authz,
		Clock:   f.clock,
		Entropy: rand.Reader,
		Logger:  slog.New(slog.NewJSONHandler(f.logs, nil)),
	}
	change(&cfg)
	svc, err := app.NewObjectService(cfg)
	if err != nil {
		t.Fatalf("NewObjectService: %v", err)
	}
	return svc
}

func (f *fixture) createBucket(t *testing.T, name string) {
	t.Helper()
	if err := f.meta.CreateBucket(context.Background(), bucket.Bucket{Name: name, CreatedAt: testNow}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
}

func (f *fixture) put(t *testing.T, bucketName, key string, data []byte) object.Version {
	t.Helper()
	v, err := f.objects.Put(context.Background(), porttest.Root(), app.PutInput{
		Bucket: bucketName, Key: key, Body: bytes.NewReader(data), Size: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("Put(%s/%s): %v", bucketName, key, err)
	}
	return v
}

func (f *fixture) get(bucketName, key string, spec *object.RangeSpec) ([]byte, app.GetOutput, error) {
	out, err := f.objects.Get(context.Background(), porttest.Root(), app.GetInput{Bucket: bucketName, Key: key, Range: spec})
	if err != nil {
		return nil, out, err
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	return data, out, err
}

// shardCount returns the number of shards stored on all drives.
func (f *fixture) shardCount() int {
	n := 0
	for _, d := range f.drives {
		n += len(d.IDs())
	}
	return n
}

// driveOf returns the drive holding a version's only shard.
func (f *fixture) driveOf(t *testing.T, v object.Version) *porttest.MemShardStore {
	t.Helper()
	for _, d := range f.drives {
		if d.DriveID() == v.Layout.Shards[0].DriveID {
			return d
		}
	}
	t.Fatalf("no drive %q", v.Layout.Shards[0].DriveID)
	return nil
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}

func anonymous() iam.Principal { return iam.Principal{} }

// fixed returns the DriveSource of a node on its own.
func fixed(t *testing.T, topology cluster.Topology, drives []ports.ShardStore) ports.DriveSource {
	t.Helper()
	source, err := app.FixedDrives(topology, drives)
	if err != nil {
		t.Fatalf("FixedDrives: %v", err)
	}
	return source
}
