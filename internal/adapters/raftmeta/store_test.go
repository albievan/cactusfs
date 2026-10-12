package raftmeta_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
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

func TestStoreSchema3Contract(t *testing.T) {
	t.Parallel()
	porttest.Schema3Contract(t, func(t *testing.T) porttest.Schema3 { return open(t, t.TempDir()) })
}

func TestStoreSchema4Contract(t *testing.T) {
	t.Parallel()
	porttest.Schema4Contract(t, func(t *testing.T) porttest.Schema4 { return open(t, t.TempDir()) })
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
	if _, err := store.DeleteObject(ctx, "alpha", "c", ports.DeleteOptions{}); err != nil {
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
		put, err := store.PutVersion(context.Background(), porttest.SampleVersion("beta", "probe", byte(restart)))
		replaced := put.Removed
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
	if _, err := store.DeleteObject(context.Background(), "alpha", "a", ports.DeleteOptions{}); err != nil {
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
		"DeleteObject": func() error { _, err := store.DeleteObject(ctx, "b", "k", ports.DeleteOptions{}); return err },
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
	porttest.IdempotencyContract(t, func(t *testing.T) ports.IdempotencyStore { return open(t, t.TempDir()) })
}

// TestANodeOnItsOwnUsesTheNewestSchema: nothing can hold a single node back,
// so its entries carry the time from the start.
func TestANodeOnItsOwnUsesTheNewestSchema(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := open(t, dir)
	if err := store.CreateBucket(context.Background(), bucket.Bucket{Name: "timed", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got := raftmeta.StateSchemaForTest(t, store); got != 2 {
		t.Errorf("state schema of a node on its own = %d, want 2", got)
	}
}

// walkPolicies reads the policies of all buckets page by page, as a search
// does, and returns them with the number of pages it took.
func walkPolicies(ctx context.Context, t *testing.T, local ports.BucketPolicies) (map[string]string, int) {
	t.Helper()
	got, pages, last := map[string]string{}, 0, ""
	for from := ""; ; {
		next, err := local.BucketPolicies(ctx, from, func(name string, document []byte) error {
			if name <= last || name < from {
				t.Errorf("a page from %q visited %q after %q", from, name, last)
			}
			last = name
			got[name] = string(document)
			return nil
		})
		if err != nil {
			t.Fatalf("BucketPolicies(%q): %v", from, err)
		}
		pages++
		if next == "" {
			return got, pages
		}
		if next <= from || next <= last {
			t.Fatalf("the page from %q, which visited up to %q, says to go on at %q", from, last, next)
		}
		from = next
	}
}

// TestLocalBucketPolicies: the policies of all buckets come from this node's
// copy of the state page by page - of the buckets that have one, each the
// document that was set, in name order - and follow what is set, removed and
// deleted.
func TestLocalBucketPolicies(t *testing.T) {
	t.Parallel()
	store := open(t, t.TempDir())
	var local ports.BucketPolicies = store.LocalConfig()
	policies := func() map[string]string {
		t.Helper()
		got, _ := walkPolicies(t.Context(), t, local)
		return got
	}
	if got := policies(); len(got) != 0 {
		t.Errorf("the policies of no bucket = %v", got)
	}
	for _, name := range []string{"aaa", "bbb", "ccc"} {
		if err := store.CreateBucket(t.Context(), bucket.Bucket{Name: name, CreatedAt: time.Unix(1, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	set := func(name, kind, document string) {
		t.Helper()
		if err := store.SetBucketConfig(t.Context(), name, kind, []byte(document)); err != nil {
			t.Fatalf("SetBucketConfig(%s, %s): %v", name, kind, err)
		}
	}
	set("aaa", bucket.ConfigPolicy, `{"a":1}`)
	set("bbb", bucket.ConfigCORS, `{"rules":[]}`)
	set("ccc", bucket.ConfigPolicy, `{"c":3}`)
	if got, want := policies(), map[string]string{"aaa": `{"a":1}`, "ccc": `{"c":3}`}; !reflect.DeepEqual(got, want) {
		t.Errorf("BucketPolicies = %v, want %v", got, want)
	}
	// Each is what the consistent read of that bucket says.
	for name, document := range policies() {
		if got, err := store.GetBucketConfig(t.Context(), name, bucket.ConfigPolicy); err != nil || string(got) != document {
			t.Errorf("GetBucketConfig(%s) = %q, %v; all at once said %q", name, got, err, document)
		}
	}
	// A page begins with the first bucket that is not before the name it is
	// given, be that the name of a bucket or not.
	for from, want := range map[string][]string{"aaa": {"aaa", "ccc"}, "aab": {"ccc"}, "ccc": {"ccc"}, "ccd": {}, "zzz": {}} {
		got := []string{}
		next, err := local.BucketPolicies(t.Context(), from, func(name string, _ []byte) error {
			got = append(got, name)
			return nil
		})
		if err != nil || next != "" || !slices.Equal(got, want) {
			t.Errorf("BucketPolicies(%q) visited %v and goes on at %q, %v; want %v and the end", from, got, next, err, want)
		}
	}
	// What a visitor does to a document, or keeps of it, changes nothing of
	// what the store holds.
	if _, err := local.BucketPolicies(t.Context(), "", func(_ string, document []byte) error {
		clear(document)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := policies(), map[string]string{"aaa": `{"a":1}`, "ccc": `{"c":3}`}; !reflect.DeepEqual(got, want) {
		t.Errorf("BucketPolicies after a visitor wrote into its documents = %v, want %v", got, want)
	}
	// The error of a visitor ends the page, and is what the page failed of.
	refusal := errors.New("the visitor has seen enough")
	visited := 0
	if _, err := local.BucketPolicies(t.Context(), "", func(string, []byte) error {
		visited++
		return refusal
	}); !errors.Is(err, refusal) || visited != 1 {
		t.Errorf("BucketPolicies with a visitor that refuses = %v after %d visits", err, visited)
	}
	set("aaa", bucket.ConfigPolicy, "")
	if err := store.DeleteBucket(t.Context(), "ccc"); err != nil {
		t.Fatal(err)
	}
	set("bbb", bucket.ConfigPolicy, `{"b":2}`)
	if got, want := policies(), map[string]string{"bbb": `{"b":2}`}; !reflect.DeepEqual(got, want) {
		t.Errorf("BucketPolicies after changes = %v, want %v", got, want)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := local.BucketPolicies(cancelled, "", func(string, []byte) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("BucketPolicies of a request that is gone = %v", err)
	}
}

// TestLocalBucketPoliciesArePaged: a page reads a bounded number of buckets
// and a bounded number of bytes, so that the read lock of the state is let
// go between pages; together the pages are every policy there is, each once.
// A bucket that is made, and a policy that is changed, while the walk is
// under way are seen by the pages that come after.
func TestLocalBucketPoliciesArePaged(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := open(t, t.TempDir())
	var local ports.BucketPolicies = store.LocalConfig()
	name := func(i int) string { return fmt.Sprintf("bucket-%04d", i) }
	want := map[string]string{}
	const buckets = raftmeta.PolicyPageBuckets + 20
	for i := range buckets {
		if err := store.CreateBucket(ctx, bucket.Bucket{Name: name(i), CreatedAt: time.Unix(1, 0)}); err != nil {
			t.Fatal(err)
		}
		if i%3 == 1 {
			continue
		}
		want[name(i)] = fmt.Sprintf(`{"of":%d}`, i)
		if err := store.SetBucketConfig(ctx, name(i), bucket.ConfigPolicy, []byte(want[name(i)])); err != nil {
			t.Fatal(err)
		}
	}
	got, pages := walkPolicies(ctx, t, local)
	if !reflect.DeepEqual(got, want) || pages != 2 {
		t.Errorf("%d buckets were read in %d pages, want 2; %d policies, want %d", buckets, pages, len(got), len(want))
	}
	// The first page ends where the bound in buckets says, whether those
	// buckets have a policy or not.
	next, err := local.BucketPolicies(ctx, "", func(string, []byte) error { return nil })
	if err != nil || next != name(raftmeta.PolicyPageBuckets) {
		t.Fatalf("the first page goes on at %q, %v; want %q", next, err, name(raftmeta.PolicyPageBuckets))
	}
	// Between two pages the state moves on: the next page shows it.
	last := name(buckets - 1)
	if err := store.SetBucketConfig(ctx, last, bucket.ConfigPolicy, []byte(`{"changed":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "zzz-new", CreatedAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBucketConfig(ctx, "zzz-new", bucket.ConfigPolicy, []byte(`{"new":true}`)); err != nil {
		t.Fatal(err)
	}
	rest := map[string]string{}
	for next != "" {
		if next, err = local.BucketPolicies(ctx, next, func(bucketName string, document []byte) error {
			rest[bucketName] = string(document)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if rest[last] != `{"changed":true}` || rest["zzz-new"] != `{"new":true}` || rest[name(0)] != "" {
		t.Errorf("the pages after the changes say %q of %s and %q of the new bucket", rest[last], last, rest["zzz-new"])
	}

	// Large policies: a page ends at its bound in bytes, long before the
	// bound in buckets.
	large := open(t, t.TempDir())
	document := []byte(`{"padding":"` + strings.Repeat("p", 60<<10) + `"}`)
	for i := range 16 {
		if err := large.CreateBucket(ctx, bucket.Bucket{Name: name(i), CreatedAt: time.Unix(1, 0)}); err != nil {
			t.Fatal(err)
		}
		if err := large.SetBucketConfig(ctx, name(i), bucket.ConfigPolicy, document); err != nil {
			t.Fatal(err)
		}
	}
	visited := 0
	next, err = large.LocalConfig().BucketPolicies(ctx, "", func(_ string, got []byte) error {
		visited++
		if !bytes.Equal(got, document) {
			t.Errorf("a large policy was read as %d bytes", len(got))
		}
		return nil
	})
	// A stored record holds the document in base64: 82 KiB each, so the
	// megabyte of a page is reached with the thirteenth.
	if err != nil || visited != 13 || next != name(13) {
		t.Errorf("a page of large policies visited %d and goes on at %q, %v; want 13 and %q", visited, next, err, name(13))
	}
	if all, pages := walkPolicies(ctx, t, large.LocalConfig()); len(all) != 16 || pages != 2 {
		t.Errorf("16 large policies were read as %d in %d pages, want 2", len(all), pages)
	}
}

// BenchmarkLocalBucketPolicies measures one page of the walk of all bucket
// policies, where every policy is large and where every policy is of a usual
// size: one read transaction, which is how long a page holds the read lock of
// the state, and - with a visitor that does nothing - little else.
func BenchmarkLocalBucketPolicies(b *testing.B) {
	for _, size := range []struct {
		name            string
		buckets, policy int
	}{{"17KiB", 100, 17 << 10}, {"1.2KiB", raftmeta.PolicyPageBuckets + 10, 1200}} {
		b.Run(size.name, func(b *testing.B) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			store, err := raftmeta.Open(ctx, config(b.TempDir()))
			cancel()
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = store.Close() })
			document := []byte(`{"padding":"` + strings.Repeat("p", size.policy) + `"}`)
			for i := range size.buckets {
				name := fmt.Sprintf("bucket-%04d", i)
				if err := store.CreateBucket(b.Context(), bucket.Bucket{Name: name, CreatedAt: time.Unix(1, 0)}); err != nil {
					b.Fatal(err)
				}
				if err := store.SetBucketConfig(b.Context(), name, bucket.ConfigPolicy, document); err != nil {
					b.Fatal(err)
				}
			}
			local := store.LocalConfig()
			visited := 0
			b.ReportAllocs()
			for b.Loop() {
				visited = 0
				next, err := local.BucketPolicies(b.Context(), "", func(string, []byte) error {
					visited++
					return nil
				})
				if err != nil || next == "" {
					b.Fatalf("a page = %q, %v", next, err)
				}
			}
			b.ReportMetric(float64(visited), "buckets/page")
		})
	}
}
