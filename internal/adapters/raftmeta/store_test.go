package raftmeta_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

func fastTiming() raftmeta.Timing {
	return raftmeta.Timing{
		Heartbeat: 50 * time.Millisecond, Election: 50 * time.Millisecond,
		LeaderLease: 50 * time.Millisecond, Commit: 5 * time.Millisecond,
	}
}

func config(dir string) raftmeta.Config {
	return raftmeta.Config{Dir: dir, Logger: slog.New(slog.DiscardHandler), Entropy: rand.Reader, Timing: fastTiming()}
}

func open(t *testing.T, dir string) *raftmeta.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := raftmeta.Open(ctx, config(dir))
	if err != nil {
		t.Fatalf("raftmeta.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestStoreContract(t *testing.T) {
	t.Parallel()
	porttest.MetadataContract(t, func(t *testing.T) porttest.Metadata { return open(t, t.TempDir()) })
}

// dump captures everything a store holds, for comparing states.
func dump(t *testing.T, store *raftmeta.Store) map[string]any {
	t.Helper()
	ctx := context.Background()
	out := map[string]any{}

	buckets, err := store.ListBuckets(ctx)
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	for _, b := range buckets {
		out["bucket/"+b.Name] = b.CreatedAt.UTC().String()
		err := store.ScanObjects(ctx, b.Name, "", func(v object.Version) ports.ScanControl {
			v.ModifiedAt = v.ModifiedAt.UTC()
			out["object/"+b.Name+"/"+v.Key] = v
			return ports.ScanControl{}
		})
		if err != nil {
			t.Fatalf("ScanObjects: %v", err)
		}
	}
	root, err := store.GetRootCredential(ctx)
	if err == nil {
		out["root"] = root.AccessKeyID
	}
	return out
}

func populate(t *testing.T, store *raftmeta.Store) {
	t.Helper()
	ctx := context.Background()
	created := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

	for _, name := range []string{"alpha", "beta", "gone"} {
		if err := store.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: created}); err != nil {
			t.Fatalf("CreateBucket: %v", err)
		}
	}
	for i, key := range []string{"a", "b/1", "b/2", "c"} {
		if _, err := store.PutVersion(ctx, porttest.SampleVersion("alpha", key, byte(i+1))); err != nil {
			t.Fatalf("PutVersion: %v", err)
		}
	}
	if _, err := store.PutVersion(ctx, porttest.SampleVersion("alpha", "a", 50)); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if _, err := store.DeleteObject(ctx, "alpha", "c"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if err := store.DeleteBucket(ctx, "gone"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	// Refused commands are entries too and must replay as refusals.
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "alpha", CreatedAt: created.Add(time.Hour)}); !errors.Is(err, domain.ErrBucketAlreadyExists) {
		t.Fatalf("duplicate CreateBucket: %v", err)
	}
	if err := store.SetRootCredential(ctx, iam.RootCredential{AccessKeyID: "ROOT", CreatedAt: created}); err != nil {
		t.Fatalf("SetRootCredential: %v", err)
	}
}

func TestStateSurvivesRestartAndEntriesApplyOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := open(t, dir)
	nodeID := first.NodeID()
	populate(t, first)
	want := dump(t, first)
	applied, err := first.AppliedIndex()
	if err != nil || applied == 0 {
		t.Fatalf("AppliedIndex = %d, %v", applied, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for restart := 1; restart <= 3; restart++ {
		store := open(t, dir)
		if store.NodeID() != nodeID {
			t.Fatalf("restart %d: node ID changed from %q to %q", restart, nodeID, store.NodeID())
		}
		if got := dump(t, store); !reflect.DeepEqual(got, want) {
			t.Fatalf("restart %d: state differs after replay\n got %v\nwant %v", restart, got, want)
		}
		// Replay must not re-apply: an overwrite replayed twice would
		// report the wrong previous version, and the applied index only
		// moves for the entries Raft itself adds on becoming leader.
		replaced, err := store.PutVersion(context.Background(), porttest.SampleVersion("beta", "probe", byte(restart)))
		if err != nil {
			t.Fatalf("restart %d: PutVersion: %v", restart, err)
		}
		if restart == 1 && replaced != nil {
			t.Fatalf("restart %d: first probe replaced %+v", restart, replaced)
		}
		if restart > 1 && (replaced == nil || replaced.VersionID != porttest.SampleVersion("beta", "probe", byte(restart-1)).VersionID) {
			t.Fatalf("restart %d: probe replaced %+v, want the previous restart's probe", restart, replaced)
		}
		want = dump(t, store)
		if err := store.Close(); err != nil {
			t.Fatalf("restart %d: Close: %v", restart, err)
		}
	}
}

func TestStateSurvivesSnapshotAndRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := open(t, dir)
	populate(t, store)
	if err := store.Snapshot(); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Changes made after the snapshot must not be lost when the node
	// restarts with a snapshot on disk.
	if _, err := store.PutVersion(context.Background(), porttest.SampleVersion("beta", "after-snapshot", 9)); err != nil {
		t.Fatalf("PutVersion: %v", err)
	}
	if _, err := store.DeleteObject(context.Background(), "alpha", "a"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	want := dump(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := open(t, dir)
	if got := dump(t, reopened); !reflect.DeepEqual(got, want) {
		t.Fatalf("state differs after snapshot and restart\n got %v\nwant %v", got, want)
	}
	if err := reopened.Snapshot(); err != nil {
		t.Fatalf("second Snapshot: %v", err)
	}
}

func TestASecondProcessCannotOpenTheSameDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	open(t, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if second, err := raftmeta.Open(ctx, config(dir)); err == nil {
		_ = second.Close()
		t.Fatal("a second Open of a metadata directory in use succeeded")
	}
}

func TestReadinessAndShutdown(t *testing.T) {
	t.Parallel()

	store := open(t, t.TempDir())
	if store.Name() == "" {
		t.Error("Name() is empty")
	}
	if err := store.Check(context.Background()); err != nil {
		t.Errorf("Check on a leading node = %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.CreateBucket(cancelled, bucket.Bucket{Name: "nope"}); !errors.Is(err, context.Canceled) {
		t.Errorf("CreateBucket with a cancelled context: %v", err)
	}
	if _, err := store.GetBucket(cancelled, "nope"); !errors.Is(err, context.Canceled) {
		t.Errorf("GetBucket with a cancelled context: %v", err)
	}
	if _, err := store.GetBucket(context.Background(), "nope"); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("the cancelled CreateBucket took effect: %v", err)
	}
}

func TestOpenFailures(t *testing.T) {
	t.Parallel()

	t.Run("context ends before leadership", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		cfg := config(t.TempDir())
		cfg.Timing = raftmeta.Timing{} // Raft's defaults need over a second to elect.
		if _, err := raftmeta.Open(ctx, cfg); !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want %v", err, context.Canceled)
		}
	})
	t.Run("no entropy for a new node", func(t *testing.T) {
		t.Parallel()
		cfg := config(t.TempDir())
		cfg.Entropy = bytes.NewReader(nil)
		if _, err := raftmeta.Open(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "node ID") {
			t.Errorf("error = %v, want a node ID failure", err)
		}
	})
}

// TestLostStateDatabaseIsRebuiltFromTheSnapshot: Raft does not restore
// snapshots at start, because the state database is durable by itself. If
// that database is gone or stale while a snapshot exists, starting on the log
// alone would silently drop everything the snapshot covers.
func TestLostStateDatabaseIsRebuiltFromTheSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	created := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

	dir := t.TempDir()
	store := open(t, dir)
	populate(t, store)
	if err := store.Snapshot(); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "after-snapshot", CreatedAt: created}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	want := dump(t, store)
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Remove("state.db"); err != nil {
		t.Fatalf("remove state.db: %v", err)
	}
	_ = root.Close()

	reopened := open(t, dir)
	if got := dump(t, reopened); !reflect.DeepEqual(got, want) {
		t.Fatalf("state after losing state.db\n got %v\nwant %v", got, want)
	}
	// The root credential in particular must not have vanished: a node
	// that believed itself new would generate another one.
	if _, err := reopened.GetRootCredential(ctx); err != nil {
		t.Errorf("root credential lost: %v", err)
	}
}

func TestStoreRefusesUseAfterClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	store := open(t, t.TempDir())
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	calls := map[string]func() error{
		"CreateBucket": func() error { return store.CreateBucket(ctx, bucket.Bucket{Name: "late"}) },
		"GetBucket":    func() error { _, err := store.GetBucket(ctx, "late"); return err },
		"ListBuckets":  func() error { _, err := store.ListBuckets(ctx); return err },
		"PutVersion":   func() error { _, err := store.PutVersion(ctx, porttest.SampleVersion("b", "k", 1)); return err },
		"GetObject":    func() error { _, err := store.GetObject(ctx, "b", "k"); return err },
		"DeleteObject": func() error { _, err := store.DeleteObject(ctx, "b", "k"); return err },
		"ScanObjects": func() error {
			return store.ScanObjects(ctx, "b", "", func(object.Version) ports.ScanControl { return ports.ScanControl{} })
		},
		"GetRootCredential": func() error { _, err := store.GetRootCredential(ctx); return err },
		"Check":             func() error { return store.Check(ctx) },
		"Snapshot":          store.Snapshot,
		"AppliedIndex":      func() error { _, err := store.AppliedIndex(); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, domain.ErrShuttingDown) {
			t.Errorf("%s after Close: error = %v, want %v", name, err, domain.ErrShuttingDown)
		}
	}
}

func TestIAMContract(t *testing.T) {
	t.Parallel()
	porttest.IAMContract(t, func(t *testing.T) porttest.IAM { return open(t, t.TempDir()) })
}
