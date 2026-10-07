package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/erasure"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// Six drives: three data and three parity shards, write quorum four.
const (
	sixDrives   = 6
	sixData     = 3
	sixParity   = 3
	multiStripe = 2*erasure.DefaultStripeSize + 12345
)

func erasureFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixtureWithDrives(t, sixDrives)
	f.createBucket(t, "b")
	return f
}

// shardsOf returns, for each shard of a version, the drive that holds it.
func (f *fixture) shardsOf(t *testing.T, v object.Version) []*porttest.MemShardStore {
	t.Helper()
	out := make([]*porttest.MemShardStore, len(v.Layout.Shards))
	for i, location := range v.Layout.Shards {
		for _, d := range f.drives {
			if d.DriveID() == location.DriveID {
				out[i] = d
			}
		}
		if out[i] == nil {
			t.Fatalf("shard %d is on unknown drive %q", i, location.DriveID)
		}
	}
	return out
}

func shardID(v object.Version, index int) object.ShardID {
	return object.ShardID{DataID: v.DataID, Index: index}
}

// snapshot returns the stored bytes of every shard of a version.
func (f *fixture) snapshot(t *testing.T, v object.Version) [][]byte {
	t.Helper()
	drives := f.shardsOf(t, v)
	out := make([][]byte, len(drives))
	for i, d := range drives {
		out[i], _ = d.Bytes(shardID(v, i))
	}
	return out
}

func TestErasureCodedRoundTrip(t *testing.T) {
	t.Parallel()

	for _, size := range []int{0, 1, 2, 3, 4, 1000, erasure.DefaultStripeSize - 16, erasure.DefaultStripeSize, multiStripe} {
		f := erasureFixture(t)
		data := randomBytes(t, size)
		version := f.put(t, "b", "k", data)

		layout := version.Layout
		if layout.DataShards != sixData || layout.ParityShards != sixParity || len(layout.Shards) != sixDrives || layout.StripeSize != erasure.DefaultStripeSize {
			t.Fatalf("size %d: layout = %+v", size, layout)
		}
		// Shards of one object never share a drive.
		seen := map[string]bool{}
		geometry := erasure.Geometry{Params: erasure.Params{Data: sixData, Parity: sixParity}, StripeSize: layout.StripeSize}
		for i, location := range layout.Shards {
			if seen[location.DriveID] {
				t.Fatalf("size %d: two shards on drive %s", size, location.DriveID)
			}
			seen[location.DriveID] = true
			if location.Size != geometry.ShardSize(layout.StoredSize) {
				t.Errorf("size %d: shard %d recorded as %d bytes, want %d", size, i, location.Size, geometry.ShardSize(layout.StoredSize))
			}
		}
		for i, stored := range f.snapshot(t, version) {
			if int64(len(stored)) != layout.Shards[i].Size {
				t.Errorf("size %d: shard %d holds %d bytes, layout says %d", size, i, len(stored), layout.Shards[i].Size)
			}
			if size >= 64 && bytes.Contains(stored, data[:32]) {
				t.Errorf("size %d: shard %d holds plaintext", size, i)
			}
		}
		if f.shardCount() != sixDrives {
			t.Errorf("size %d: %d shards stored, want %d", size, f.shardCount(), sixDrives)
		}

		got, _, err := f.get("b", "k", nil)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("size %d: Get: %v", size, err)
		}
		if size > 100 {
			spec := object.RangeSpec{First: int64(size) - 50, Last: -1}
			if tail, _, err := f.get("b", "k", &spec); err != nil || !bytes.Equal(tail, data[size-50:]) {
				t.Errorf("size %d: ranged Get: %v", size, err)
			}
		}
	}
}

// TestReadSurvivesAnyLossUpToParity is what erasure coding is for: whichever
// shards are lost, up to the parity count, every read returns the object; one
// more, and the read fails rather than return anything else.
func TestReadSurvivesAnyLossUpToParity(t *testing.T) {
	t.Parallel()

	// Every combination is tried on an object of one stripe; the worst
	// combinations, which lose exactly the parity count, also on one that
	// spans several stripes.
	small, large := randomBytes(t, 50_000), randomBytes(t, multiStripe)
	type damage func(d *porttest.MemShardStore, id object.ShardID)
	kinds := map[string]damage{
		"missing":   func(d *porttest.MemShardStore, id object.ShardID) { _ = d.Delete(context.Background(), id) },
		"corrupt":   func(d *porttest.MemShardStore, id object.ShardID) { d.Damage(id) },
		"truncated": func(d *porttest.MemShardStore, id object.ShardID) { b, _ := d.Bytes(id); d.Corrupt(id, b[:len(b)/2]) },
	}

	for mask := 0; mask < 1<<sixDrives; mask++ {
		lost := 0
		for i := range sixDrives {
			if mask&(1<<i) != 0 {
				lost++
			}
		}
		if lost == 0 || lost > sixParity+1 {
			continue
		}
		for name, apply := range kinds {
			data := small
			if lost == sixParity && name == "missing" {
				data = large
			}
			f := erasureFixture(t)
			version := f.put(t, "b", "k", data)
			drives := f.shardsOf(t, version)
			for i := range sixDrives {
				if mask&(1<<i) != 0 {
					apply(drives[i], shardID(version, i))
				}
			}

			got, _, err := f.get("b", "k", nil)
			if lost > sixParity {
				if !errors.Is(err, domain.ErrIntegrity) && !errors.Is(err, domain.ErrShardNotFound) {
					t.Fatalf("%s, shards %06b lost: error = %v, want the read to fail", name, mask, err)
				}
				if len(got) != 0 && !bytes.HasPrefix(data, got) {
					t.Fatalf("%s, shards %06b lost: bytes were returned that are not the object", name, mask)
				}
				continue
			}
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("%s, shards %06b lost: Get: %v", name, mask, err)
			}
			if len(data) > erasure.DefaultStripeSize {
				spec := object.RangeSpec{First: erasure.DefaultStripeSize - 100, Last: erasure.DefaultStripeSize + 99}
				if part, _, err := f.get("b", "k", &spec); err != nil || !bytes.Equal(part, data[spec.First:spec.Last+1]) {
					t.Fatalf("%s, shards %06b lost: ranged Get across a stripe boundary: %v", name, mask, err)
				}
			}
		}
	}
}

// TestWriteQuorum: a write succeeds when it is durable on a write quorum of
// drives and fails, leaving nothing behind, when it is not. With three data
// and three parity shards the quorum is four.
func TestWriteQuorum(t *testing.T) {
	t.Parallel()
	boom := errors.New("drive failure")

	stages := map[string]func(d *porttest.MemShardStore){
		"create": func(d *porttest.MemShardStore) { d.FailCreate = boom },
		"write":  func(d *porttest.MemShardStore) { d.FailWrite = boom },
		"commit": func(d *porttest.MemShardStore) { d.FailCommit = boom },
	}
	for stage, fail := range stages {
		for failing := 1; failing <= sixDrives; failing++ {
			f := erasureFixture(t)
			for _, d := range f.drives[:failing] {
				fail(d)
			}
			size := 200_000
			if failing == 2 {
				size = multiStripe
			}
			data := randomBytes(t, size)
			version, err := f.objects.Put(context.Background(), porttest.Root(), app.PutInput{
				Bucket: "b", Key: "k", Body: bytes.NewReader(data), Size: int64(len(data)),
			})

			if failing > sixDrives-4 {
				if !errors.Is(err, domain.ErrInsufficientDrives) {
					t.Fatalf("%s fails on %d drives: error = %v, want %v", stage, failing, err, domain.ErrInsufficientDrives)
				}
				if _, getErr := f.meta.GetObject(context.Background(), "b", "k"); !errors.Is(getErr, domain.ErrObjectNotFound) {
					t.Errorf("%s fails on %d drives: the object is visible: %v", stage, failing, getErr)
				}
				if n := f.shardCount(); n != 0 {
					t.Errorf("%s fails on %d drives: %d shards left behind", stage, failing, n)
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s fails on %d drives: Put: %v", stage, failing, err)
			}
			if n := f.shardCount(); n != sixDrives-failing {
				t.Errorf("%s fails on %d drives: %d shards stored, want %d", stage, failing, n, sixDrives-failing)
			}
			// The layout still names every shard, so the missing ones can
			// be rebuilt where they belong.
			if len(version.Layout.Shards) != sixDrives {
				t.Errorf("%s fails on %d drives: layout has %d locations", stage, failing, len(version.Layout.Shards))
			}
			for _, d := range f.drives {
				d.FailCreate, d.FailWrite, d.FailCommit = nil, nil, nil
			}
			if got, _, err := f.get("b", "k", nil); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("%s fails on %d drives: Get: %v", stage, failing, err)
			}
			// A degraded write is queued for healing, which completes it.
			f.runHealer(t, func() bool { return f.shardCount() == sixDrives })
		}
	}
}

// runHealer runs the background healer until done reports true.
func (f *fixture) runHealer(t *testing.T, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		f.objects.Healer().Run(ctx)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !done() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-finished
	if !done() {
		t.Fatal("the background healer did not repair the object")
	}
}

func TestHealRebuildsDamagedShards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for damaged := 0; damaged <= sixParity; damaged++ {
		f := erasureFixture(t)
		data := randomBytes(t, multiStripe)
		version := f.put(t, "b", "k", data)
		before := f.snapshot(t, version)
		drives := f.shardsOf(t, version)

		// One of each kind of damage, on the last shards: parity first.
		kinds := []func(i int){
			func(i int) { _ = drives[i].Delete(ctx, shardID(version, i)) },
			func(i int) { drives[i].Damage(shardID(version, i)) },
			func(i int) { drives[i].Corrupt(shardID(version, i), before[i][:len(before[i])-1]) },
		}
		for n := range damaged {
			kinds[n](sixDrives - 1 - n)
		}

		result, err := f.objects.Healer().HealObject(ctx, "b", "k")
		if err != nil {
			t.Fatalf("%d damaged: HealObject: %v", damaged, err)
		}
		want := app.HealResult{Bucket: "b", Key: "k", Shards: sixDrives, Damaged: damaged, Rebuilt: damaged}
		if result != want {
			t.Errorf("%d damaged: result = %+v, want %+v", damaged, result, want)
		}
		// Every shard is back, byte for byte.
		after := f.snapshot(t, version)
		for i := range before {
			if !bytes.Equal(after[i], before[i]) {
				t.Errorf("%d damaged: shard %d differs after healing", damaged, i)
			}
		}
		// And the object can lose a full parity count of other shards again.
		for i := range sixParity {
			_ = drives[i].Delete(ctx, shardID(version, i))
		}
		if got, _, err := f.get("b", "k", nil); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%d damaged: Get after healing and losing the data shards: %v", damaged, err)
		}
		if results := f.heals.list(); len(results) != 1 || results[0] != want {
			t.Errorf("%d damaged: heal observations = %+v", damaged, results)
		}
	}
}

func TestHealReportsWhatCannotBeRebuilt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := erasureFixture(t)
	version := f.put(t, "b", "k", randomBytes(t, 5000))
	drives := f.shardsOf(t, version)
	for i := range sixParity + 1 {
		drives[i].Damage(shardID(version, i))
	}

	result, err := f.objects.Healer().HealObject(ctx, "b", "k")
	if !errors.Is(err, domain.ErrIntegrity) || !result.Unrecoverable || result.Damaged != sixParity+1 || result.Rebuilt != 0 {
		t.Fatalf("HealObject = %+v, %v; want an unrecoverable object", result, err)
	}
	if !strings.Contains(f.logs.String(), "damaged beyond repair") {
		t.Error("an unrecoverable object is not logged")
	}
	// Nothing was made worse: the shards that were intact still are.
	for i := sixParity + 1; i < sixDrives; i++ {
		if _, ok := drives[i].Bytes(shardID(version, i)); !ok {
			t.Errorf("intact shard %d was removed", i)
		}
	}

	// Objects that are gone are not an error.
	if result, err := f.objects.Healer().HealObject(ctx, "b", "absent"); err != nil || result.Shards != 0 {
		t.Errorf("HealObject(absent) = %+v, %v", result, err)
	}
	if _, err := f.objects.Healer().HealObject(ctx, "no-such-bucket", "k"); err != nil {
		t.Errorf("HealObject(missing bucket) = %v", err)
	}
}

func TestDegradedReadQueuesHealing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := erasureFixture(t)
	data := randomBytes(t, multiStripe)
	version := f.put(t, "b", "k", data)
	drives := f.shardsOf(t, version)
	_ = drives[0].Delete(ctx, shardID(version, 0))
	drives[1].Damage(shardID(version, 1))

	// The same degraded object read many times is queued once.
	for range 5 {
		if got, _, err := f.get("b", "k", nil); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("degraded Get: %v", err)
		}
	}
	f.runHealer(t, func() bool { return len(f.heals.list()) > 0 })

	if results := f.heals.list(); len(results) != 1 || results[0].Damaged != 2 || results[0].Rebuilt != 2 {
		t.Errorf("heal observations = %+v, want one heal of two shards", results)
	}
	if f.shardCount() != sixDrives {
		t.Errorf("%d shards after healing, want %d", f.shardCount(), sixDrives)
	}
	// A healthy read queues nothing.
	if _, _, err := f.get("b", "k", nil); err != nil {
		t.Fatal(err)
	}
	ctxShort, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	f.objects.Healer().Run(ctxShort)
	if len(f.heals.list()) != 1 {
		t.Errorf("a healthy read was queued for healing: %+v", f.heals.list())
	}
}

func TestHealOfAnObjectDeletedMeanwhile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := erasureFixture(t)
	version := f.put(t, "b", "k", randomBytes(t, 5000))
	drives := f.shardsOf(t, version)
	_ = drives[0].Delete(ctx, shardID(version, 0))

	// The object is deleted between the moment healing reads its metadata
	// and the moment it finishes: simulated by healing a stale version
	// through an index that still returns it once.
	stale := &staleIndex{ObjectIndex: f.meta, version: version}
	svc, err := app.NewObjectService(app.ObjectServiceConfig{
		Index: stale, Buckets: f.meta, Topology: f.topology, Drives: asStores(f.drives), Codec: porttest.NewCodec(),
		KMS: f.kms, Authz: f.authz, Clock: f.clock, Entropy: porttest.Entropy(), Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.objects.Delete(ctx, porttest.Root(), "b", "k"); err != nil {
		t.Fatal(err)
	}
	if f.shardCount() != 0 {
		t.Fatalf("%d shards after delete", f.shardCount())
	}

	result, err := svc.Healer().HealObject(ctx, "b", "k")
	if err != nil || result.Rebuilt != 0 {
		t.Errorf("HealObject of a deleted object = %+v, %v", result, err)
	}
	if n := f.shardCount(); n != 0 {
		t.Errorf("healing a deleted object left %d shards behind", n)
	}
}

// staleIndex returns one fixed version for the first lookup and the real
// state afterwards.
type staleIndex struct {
	ports.ObjectIndex
	version object.Version
	served  bool
}

func (s *staleIndex) GetObject(ctx context.Context, bucketName, key string) (object.Version, error) {
	if !s.served {
		s.served = true
		return s.version, nil
	}
	return s.ObjectIndex.GetObject(ctx, bucketName, key)
}

func asStores(drives []*porttest.MemShardStore) []ports.ShardStore {
	out := make([]ports.ShardStore, len(drives))
	for i, d := range drives {
		out[i] = d
	}
	return out
}

func TestScrubber(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := erasureFixture(t)
	f.createBucket(t, "second")
	var versions []object.Version
	for i := range 12 {
		versions = append(versions, f.put(t, "b", fmt.Sprintf("key-%02d", i), randomBytes(t, 3000+i)))
	}
	versions = append(versions, f.put(t, "second", "other", randomBytes(t, 50_000)))

	// Damage three objects within parity, and one beyond it.
	_ = f.shardsOf(t, versions[2])[0].Delete(ctx, shardID(versions[2], 0))
	f.shardsOf(t, versions[5])[4].Damage(shardID(versions[5], 4))
	f.shardsOf(t, versions[5])[5].Damage(shardID(versions[5], 5))
	_ = f.shardsOf(t, versions[12])[1].Delete(ctx, shardID(versions[12], 1))
	for i := range sixParity + 1 {
		f.shardsOf(t, versions[9])[i].Damage(shardID(versions[9], i))
	}

	var paused time.Duration
	scrubber := app.NewScrubber(app.ScrubberConfig{
		Buckets: f.meta, Index: f.meta, Healer: f.objects.Healer(), Logger: slog.New(slog.DiscardHandler),
		BytesPerSecond: 1 << 20,
		Pause:          func(_ context.Context, d time.Duration) { paused += d },
	})
	stats, err := scrubber.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Objects != 13 || stats.Damaged != 4 || stats.ShardsRebuilt != 4 || stats.Unrecoverable != 1 || stats.Bytes <= 0 {
		t.Errorf("stats = %+v, want 13 objects, 4 damaged, 4 shards rebuilt, 1 unrecoverable", stats)
	}
	if paused <= 0 {
		t.Error("the rate limit never paused the scrubber")
	}
	for _, i := range []int{2, 5, 12} {
		if got, _, err := f.get(versions[i].Bucket, versions[i].Key, nil); err != nil || int64(len(got)) != versions[i].Size {
			t.Errorf("object %d after scrub: %v", i, err)
		}
	}

	// A second pass finds the healed objects healthy.
	again, err := scrubber.RunOnce(ctx)
	if err != nil || again.Damaged != 1 || again.ShardsRebuilt != 0 || again.Unrecoverable != 1 {
		t.Errorf("second pass = %+v, %v; want only the unrecoverable object", again, err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := scrubber.RunOnce(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("RunOnce with a cancelled context: %v", err)
	}
	f.meta.FailWrites = nil
}

func TestScrubberWalksLargeBucketsInBatches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t)
	f.createBucket(t, "b")
	const objects = 600 // more than two batches
	for i := range objects {
		// The index is filled directly; their data does not exist, so
		// every one of them is reported as lost exactly once.
		v := porttest.SampleVersion("b", fmt.Sprintf("key-%04d", i), 1)
		v.Layout.Shards[0].DriveID = "drive-a"
		if _, err := f.meta.PutVersion(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	scrubber := app.NewScrubber(app.ScrubberConfig{
		Buckets: f.meta, Index: f.meta, Healer: f.objects.Healer(), Logger: slog.New(slog.DiscardHandler),
	})
	stats, err := scrubber.RunOnce(ctx)
	if err != nil || stats.Objects != objects || stats.Unrecoverable != objects {
		t.Errorf("stats = %+v, %v; want every one of %d objects visited once", stats, err, objects)
	}
}

func TestScrubberRunStopsWithItsContext(t *testing.T) {
	t.Parallel()

	f := erasureFixture(t)
	f.put(t, "b", "k", []byte("data"))
	passes := 0
	ctx, cancel := context.WithCancel(context.Background())
	scrubber := app.NewScrubber(app.ScrubberConfig{
		Buckets: f.meta, Index: f.meta, Healer: f.objects.Healer(), Logger: slog.New(slog.DiscardHandler),
		Pause: func(context.Context, time.Duration) {
			passes++
			if passes == 3 {
				cancel()
			}
		},
	})
	done := make(chan struct{})
	go func() { defer close(done); scrubber.Run(ctx, time.Hour) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop when its context was cancelled")
	}
	if passes != 3 {
		t.Errorf("%d passes, want 3", passes)
	}
}

func TestCollector(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := erasureFixture(t)
	now := testNow
	for _, d := range f.drives {
		d.Now = func() time.Time { return now.Add(-48 * time.Hour) }
	}
	kept := f.put(t, "b", "kept", randomBytes(t, 5000))

	// Left behind by writes that never committed: old ones and a fresh one,
	// which may belong to a write that is about to commit.
	orphan := func(fill byte) object.ID { return object.ID(bytes.Repeat([]byte{fill}, object.IDSize)) }
	for i, d := range f.drives[:4] {
		d.Put(object.ShardID{DataID: orphan(0xAA), Index: i}, []byte("old orphan"), now.Add(-25*time.Hour))
	}
	f.drives[0].Put(object.ShardID{DataID: orphan(0xBB), Index: 0}, []byte("another old orphan"), now.Add(-72*time.Hour))
	f.drives[1].Put(object.ShardID{DataID: orphan(0xCC), Index: 0}, []byte("young orphan"), now.Add(-time.Hour))

	collector := app.NewCollector(app.CollectorConfig{
		Drives: asStores(f.drives), Index: f.meta, Clock: f.clock, Grace: 24 * time.Hour, Logger: slog.New(slog.DiscardHandler),
	})
	stats, err := collector.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Shards != sixDrives+6 || stats.Removed != 5 || stats.Young != 1 {
		t.Errorf("stats = %+v, want %d shards seen, 5 removed, 1 young", stats, sixDrives+6)
	}
	// The object in use is untouched, although its shards are old.
	if f.shardCount() != sixDrives+1 {
		t.Errorf("%d shards remain, want the object's %d and the young orphan", f.shardCount(), sixDrives)
	}
	if got, _, err := f.get("b", "kept", nil); err != nil || int64(len(got)) != kept.Size {
		t.Errorf("the object in use was damaged by collection: %v", err)
	}

	// Once the grace period has passed the young orphan goes too, and a
	// deleted object's shards that its own cleanup missed.
	for _, d := range f.drives {
		d.FailDelete = errors.New("busy")
	}
	if err := f.objects.Delete(ctx, porttest.Root(), "b", "kept"); err != nil {
		t.Fatal(err)
	}
	for _, d := range f.drives {
		d.FailDelete = nil
	}
	f.clock.Advance(24 * time.Hour)
	stats, err = collector.RunOnce(ctx)
	if err != nil || stats.Removed != sixDrives+1 || f.shardCount() != 0 {
		t.Errorf("second pass = %+v, %v; %d shards remain, want none", stats, err, f.shardCount())
	}
}

func TestCollectorFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := erasureFixture(t)
	old := testNow.Add(-48 * time.Hour)
	f.drives[0].Put(object.ShardID{DataID: object.ID{1}}, []byte("orphan"), old)
	f.drives[1].Put(object.ShardID{DataID: object.ID{2}}, []byte("orphan"), old)
	collector := app.NewCollector(app.CollectorConfig{
		Drives: asStores(f.drives), Index: f.meta, Clock: f.clock, Grace: time.Hour, Logger: slog.New(slog.NewJSONHandler(f.logs, nil)),
	})

	// A shard that cannot be deleted is logged and the pass goes on.
	f.drives[0].FailDelete = errors.New("read-only drive")
	stats, err := collector.RunOnce(ctx)
	if err != nil || stats.Removed != 1 || f.shardCount() != 1 {
		t.Errorf("stats = %+v, %v with %d shards left; want one removed, one kept", stats, err, f.shardCount())
	}
	if !strings.Contains(f.logs.String(), "unreferenced shard cannot be removed") {
		t.Error("the failed removal is not logged")
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := collector.RunOnce(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("RunOnce with a cancelled context: %v", err)
	}

	// Run stops with its context.
	runCtx, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	done := make(chan struct{})
	go func() { defer close(done); collector.Run(runCtx, time.Millisecond) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop when its context ended")
	}
}

func TestEnsureTopology(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	states := func(ids ...string) []cluster.DriveState {
		out := make([]cluster.DriveState, len(ids))
		for i, id := range ids {
			out[i] = cluster.DriveState{ID: id}
		}
		return out
	}

	meta := porttest.NewMemMetadata()
	created, err := app.EnsureTopology(ctx, meta, states("d0", "d1", "d2", "d3"), logger)
	if err != nil || created.Version != 1 || !slices.Equal(created.DriveIDs(), []string{"d0", "d1", "d2", "d3"}) {
		t.Fatalf("first start: %+v, %v", created, err)
	}

	// Later starts find the drives whatever order they are given in.
	again, err := app.EnsureTopology(ctx, meta, states("d3", "d1", "d0", "d2"), logger)
	if err != nil || again.Version != 1 || !slices.Equal(again.DriveIDs(), created.DriveIDs()) {
		t.Errorf("restart with drives reordered: %+v, %v", again, err)
	}

	// One empty drive takes over the slot of the one missing drive.
	withNew := append(states("d0", "d1", "d3"), cluster.DriveState{ID: "new", Fresh: true})
	replaced, err := app.EnsureTopology(ctx, meta, withNew, logger)
	if err != nil || replaced.Version != 2 {
		t.Fatalf("replacement: %+v, %v", replaced, err)
	}
	if current, ok := replaced.Current("d2"); !ok || current != "new" {
		t.Errorf("Current(d2) = %q, %v; want the new drive", current, ok)
	}

	for name, attached := range map[string][]cluster.DriveState{
		"a drive missing":            states("d0", "d1", "d3"),
		"the replaced drive is back": states("d0", "d1", "d2", "d3"),
		"an unknown drive with data": states("d0", "d1", "d3", "foreign"),
		"an extra drive":             append(states("d0", "d1", "new", "d3"), cluster.DriveState{ID: "extra", Fresh: true}),
	} {
		if _, err := app.EnsureTopology(ctx, meta, attached, logger); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: error = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	if stored, _ := meta.GetTopology(ctx); stored.Version != 2 {
		t.Errorf("a refused start changed the topology: version %d", stored.Version)
	}

	// A new deployment whose drives cannot form equal sets is refused.
	seventeen := make([]string, 17)
	for i := range seventeen {
		seventeen[i] = fmt.Sprintf("d%02d", i)
	}
	if _, err := app.EnsureTopology(ctx, porttest.NewMemMetadata(), states(seventeen...), logger); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("17 drives: error = %v, want %v", err, domain.ErrInvalidArgument)
	}
	failing := porttest.NewMemMetadata()
	failing.FailWrites = errBoom
	if _, err := app.EnsureTopology(ctx, failing, states("d0"), logger); !errors.Is(err, errBoom) {
		t.Errorf("store failure: error = %v", err)
	}
}

// TestReplacedDriveIsRefilled: a drive dies and an empty one takes its place.
// Objects stay readable, healing rebuilds the lost shards on the new drive,
// and the layouts - which still name the old drive - keep working.
func TestReplacedDriveIsRefilled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := erasureFixture(t)
	if err := f.meta.SetTopology(ctx, f.topology); err != nil {
		t.Fatal(err)
	}
	payloads := map[string][]byte{}
	for i := range 8 {
		key := fmt.Sprintf("key-%d", i)
		payloads[key] = randomBytes(t, 100_000+i)
		f.put(t, "b", key, payloads[key])
	}

	// drive-c is replaced by an empty drive.
	const lost = 2
	replacement := porttest.NewMemShardStore("drive-new")
	attached := make([]cluster.DriveState, 0, sixDrives)
	for i, d := range f.drives {
		if i == lost {
			attached = append(attached, cluster.DriveState{ID: replacement.DriveID(), Fresh: true})
			continue
		}
		attached = append(attached, cluster.DriveState{ID: d.DriveID()})
	}
	topology, err := app.EnsureTopology(ctx, f.meta, attached, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("EnsureTopology: %v", err)
	}
	f.drives[lost] = replacement
	f.topology = topology
	f.objects = f.objectService(t, f.kms)

	for key, want := range payloads {
		if got, _, err := f.get("b", key, nil); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("Get(%s) with a replaced drive: %v", key, err)
		}
	}
	if n := len(replacement.IDs()); n != 0 {
		t.Fatalf("the new drive holds %d shards before healing", n)
	}

	scrubber := app.NewScrubber(app.ScrubberConfig{
		Buckets: f.meta, Index: f.meta, Healer: f.objects.Healer(), Logger: slog.New(slog.DiscardHandler),
	})
	stats, err := scrubber.RunOnce(ctx)
	if err != nil || stats.Damaged != 8 || stats.ShardsRebuilt != 8 || stats.Unrecoverable != 0 {
		t.Fatalf("scrub after replacement = %+v, %v; want every object rebuilt on the new drive", stats, err)
	}
	if n := len(replacement.IDs()); n != 8 {
		t.Errorf("the new drive holds %d shards after healing, want 8", n)
	}
	// Full redundancy is back: lose three other drives and still read.
	for _, d := range []*porttest.MemShardStore{f.drives[0], f.drives[1], f.drives[3]} {
		for _, id := range d.IDs() {
			_ = d.Delete(ctx, id)
		}
	}
	for key, want := range payloads {
		if got, _, err := f.get("b", key, nil); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("Get(%s) after refilling and losing three drives: %v", key, err)
		}
	}
	// New writes go to the new drive like to any other.
	f.put(t, "b", "after", randomBytes(t, 1000))
	if n := len(replacement.IDs()); n != 9 {
		t.Errorf("a new object did not use the new drive: %d shards", n)
	}
}
