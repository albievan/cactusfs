package raftmeta

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// FuzzEncodeKey: the encoding of object keys in the version index keeps
// their order, lets no encoded key be the beginning of another, and comes
// back as it went in - for any bytes, zeros included.
func FuzzEncodeKey(f *testing.F) {
	f.Add("a", "ab")
	f.Add("a", "a\x00")
	f.Add("a\x00", "a\x00\x00")
	f.Add("a\x00\x01", "a\x01")
	f.Add("", "\x00")
	f.Add("a\x00\xff", "a\x00\x01")
	f.Fuzz(func(t *testing.T, a, b string) {
		ea, eb := encodeKey(a), encodeKey(b)
		for key, encoded := range map[string][]byte{a: ea, b: eb} {
			// What follows the key in the index does not disturb it.
			decoded, rest, ok := decodeKey(append(bytes.Clone(encoded), 0xde, 0xad, 0x00, 0x01))
			if !ok || decoded != key || !bytes.Equal(rest, []byte{0xde, 0xad, 0x00, 0x01}) {
				t.Fatalf("decodeKey(encodeKey(%q)) = %q, %x, %v", key, decoded, rest, ok)
			}
		}
		if got, want := bytes.Compare(ea, eb), bytes.Compare([]byte(a), []byte(b)); got != want {
			t.Fatalf("order of %q and %q is %d, of their encodings %d", a, b, want, got)
		}
		if a != b && (bytes.HasPrefix(ea, eb) || bytes.HasPrefix(eb, ea)) {
			t.Fatalf("the encoding of %q or %q begins with the other's", a, b)
		}
	})
}

// FuzzDecodeKey: bytes that are not an encoded key are refused, never
// misread, and never panic.
func FuzzDecodeKey(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0})
	f.Add([]byte{'a', 0, 2})
	f.Add(encodeKey("a\x00b"))
	f.Fuzz(func(t *testing.T, stored []byte) {
		key, rest, ok := decodeKey(stored)
		if !ok {
			return
		}
		if again := append(encodeKey(key), rest...); !bytes.Equal(again, stored) {
			t.Fatalf("decodeKey accepted %x as %q + %x, which encodes to %x", stored, key, rest, again)
		}
	})
}

// recount computes the data-reference index from scratch: what the index
// must be if it is right.
func recount(t *testing.T, s *Store) map[string]uint64 {
	t.Helper()
	counts := map[string]uint64{}
	if err := s.fsm.view(func(tx *bbolt.Tx) error {
		for _, parent := range [][]byte{objectsBucket(), versionsBucket()} {
			holder := tx.Bucket(parent)
			if err := holder.ForEachBucket(func(name []byte) error {
				return holder.Bucket(name).ForEach(func(_, raw []byte) error {
					v, err := decodeVersion(raw)
					if err != nil {
						return err
					}
					if !v.DeleteMarker {
						counts[v.DataID]++
					}
					return nil
				})
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return counts
}

func storedCounts(t *testing.T, s *Store) map[string]uint64 {
	t.Helper()
	counts := map[string]uint64{}
	if err := s.fsm.view(func(tx *bbolt.Tx) error {
		return tx.Bucket(dataBucket()).ForEach(func(id, raw []byte) error {
			counts[object.ID(id).String()] = binary.BigEndian.Uint64(raw)
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return counts
}

// checkInvariant verifies, for every key of a bucket, the rule the version
// index is built on: the current version is in "objects" exactly when the
// newest version is not a delete marker, and it is the newest.
func checkInvariant(t *testing.T, s *Store, bucketName string) {
	t.Helper()
	if err := s.fsm.view(func(tx *bbolt.Tx) error {
		h, err := openHistory(tx, bucketName, false)
		if err != nil || h.current == nil {
			return err
		}
		keys := map[string]bool{}
		if err := h.current.ForEach(func(k, _ []byte) error { keys[string(k)] = true; return nil }); err != nil {
			return err
		}
		if h.older != nil {
			if err := h.older.ForEach(func(k, _ []byte) error {
				key, rest, ok := decodeKey(k)
				if !ok || len(rest) != versionSuffixSize {
					t.Errorf("a key of the version index is malformed: %x", k)
				}
				keys[key] = true
				return nil
			}); err != nil {
				return err
			}
		}
		for key := range keys {
			current, err := readVersion(h.current, key)
			if err != nil {
				return err
			}
			var older []*versionRecord
			if err := h.each(key, func(_ []byte, v *versionRecord) (bool, error) { older = append(older, v); return true, nil }); err != nil {
				return err
			}
			for i := 1; i < len(older); i++ {
				if older[i-1].Seq < older[i].Seq {
					t.Errorf("versions of %q are not newest first", key)
				}
			}
			nulls := 0
			for _, v := range append(older, current) {
				if v != nil && !v.Versioned {
					nulls++
				}
			}
			if nulls > 1 {
				t.Errorf("key %q has %d null versions", key, nulls)
			}
			switch {
			case current != nil && current.DeleteMarker:
				t.Errorf("a delete marker is the current version of %q", key)
			case current != nil && len(older) > 0 && older[0].Seq > current.Seq:
				t.Errorf("the current version of %q is not its newest", key)
			case current == nil && len(older) > 0 && !older[0].DeleteMarker:
				t.Errorf("key %q has no current version although its newest version is not a marker", key)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// choices is a small repeatable sequence of choices. The test needs
// variety that is the same on every run, not unpredictability.
type choices struct{ state uint64 }

// IntN returns the next choice among n.
func (c *choices) IntN(n int) int {
	c.state ^= c.state << 13
	c.state ^= c.state >> 7
	c.state ^= c.state << 17
	// Counted, not converted: n is a handful.
	choice := 0
	for range c.state % uint64(max(n, 1)) {
		choice++
	}
	return choice
}

func describe(v *object.Version) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%s@%s marker=%v versioned=%v data=%s", v.Key, v.Ref(), v.DeleteMarker, v.Versioned, v.DataID.String()[:4])
}

func snapshotOf(t *testing.T, store porttest.Metadata, bucketName string) []string {
	t.Helper()
	var out []string
	if err := store.ScanVersions(context.Background(), bucketName, "", func(v object.Version, latest bool) ports.ScanControl {
		out = append(out, fmt.Sprintf("%s latest=%v", describe(&v), latest))
		return ports.ScanControl{}
	}); err != nil {
		t.Fatal(err)
	}
	out = append(out, "--- current ---")
	if err := store.ScanObjects(context.Background(), bucketName, "", func(v object.Version) ports.ScanControl {
		out = append(out, describe(&v))
		return ports.ScanControl{}
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestVersionsAgainstAModel drives the state machine and a plain in-memory
// model with the same random sequence of puts, deletes, version deletes and
// versioning changes, and requires them to agree after every step: on what
// each change returned, on every version of every key, on what is current,
// and on which stored data is still referred to. The invariant of the index
// and the reference counts are checked from the raw state as well.
func TestVersionsAgainstAModel(t *testing.T) {
	t.Parallel()
	for seed := range uint64(6) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			random := &choices{state: seed*2654435761 + 99}
			real, model := openStore(t, t.TempDir()), porttest.NewMemMetadata()
			stores := []porttest.Metadata{real, model}
			for _, s := range stores {
				if err := s.CreateBucket(ctx, bucket.Bucket{Name: "b"}); err != nil {
					t.Fatal(err)
				}
			}
			keys := []string{"a", "a\x00", "a\x00b", "ab", "b/c", "\x00"}
			var refs []object.VersionRef
			nextFill := byte(1)
			at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

			for step := range 300 {
				key := keys[random.IntN(len(keys))]
				var got [2]string
				switch op := random.IntN(20); {
				case op < 9: // put
					v := porttest.SampleVersion("b", key, nextFill)
					refs = append(refs, object.VersionRef{ID: v.VersionID})
					nextFill += 2
					for i, s := range stores {
						r, err := s.PutVersion(ctx, v)
						if err != nil {
							t.Fatalf("step %d: PutVersion: %v", step, err)
						}
						got[i] = "put " + describe(&r.Stored) + " removed " + describe(r.Removed)
					}
				case op < 13: // delete the object
					marker := object.ID(bytes.Repeat([]byte{nextFill}, object.IDSize))
					refs = append(refs, object.VersionRef{ID: marker})
					nextFill += 2
					for i, s := range stores {
						r, err := s.DeleteObject(ctx, "b", key, ports.DeleteOptions{MarkerID: marker, At: at})
						if err != nil {
							t.Fatalf("step %d: DeleteObject: %v", step, err)
						}
						got[i] = "delete marker " + describe(r.Marker) + " removed " + describe(r.Removed)
					}
				case op < 18: // remove one version for good
					ref := object.VersionRef{Null: true}
					if len(refs) > 0 && random.IntN(4) > 0 {
						ref = refs[random.IntN(len(refs))]
					}
					for i, s := range stores {
						r, err := s.DeleteObject(ctx, "b", key, ports.DeleteOptions{Version: &ref, At: at})
						if err != nil {
							t.Fatalf("step %d: DeleteObject(version): %v", step, err)
						}
						got[i] = "remove " + ref.String()[:4] + ": " + describe(r.Removed) + " marker " + describe(r.Marker)
					}
				default: // change the versioning state
					state := bucket.VersioningEnabled
					if random.IntN(2) == 0 {
						state = bucket.VersioningSuspended
					}
					for i, s := range stores {
						if err := s.SetBucketVersioning(ctx, "b", state); err != nil {
							t.Fatalf("step %d: SetBucketVersioning: %v", step, err)
						}
						got[i] = "versioning " + string(state)
					}
				}
				if got[0] != got[1] {
					t.Fatalf("step %d: the state machine answered\n  %s\nthe model\n  %s", step, got[0], got[1])
				}
				if a, b := snapshotOf(t, real, "b"), snapshotOf(t, model, "b"); !slices.Equal(a, b) {
					t.Fatalf("step %d (%s): the state machine holds\n  %v\nthe model\n  %v", step, got[0], a, b)
				}
				checkInvariant(t, real, "b")
				counts := recount(t, real)
				if stored := storedCounts(t, real); fmt.Sprint(stored) != fmt.Sprint(counts) {
					t.Fatalf("step %d (%s): the reference index says %v, a recount %v", step, got[0], stored, counts)
				}
				for fill := byte(1); fill < nextFill; fill += 2 {
					id := porttest.SampleVersion("b", "x", fill).DataID
					a, errA := real.IsDataReferenced(ctx, id)
					b, errB := model.IsDataReferenced(ctx, id)
					if errA != nil || errB != nil || a != b {
						t.Fatalf("step %d: data %s referenced: state machine %v (%v), model %v (%v)", step, id, a, errA, b, errB)
					}
				}
			}
		})
	}
}

// TestVersionsSurviveARestartAndARebuiltIndex: versions that are no longer
// current are in the state after a restart, and a reference index that is
// rebuilt counts them. If it did not, the collector would delete the data of
// every old version.
func TestVersionsSurviveARestartAndARebuiltIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	store := openStore(t, dir)
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBucketVersioning(ctx, "b", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	old, newer := porttest.SampleVersion("b", "k", 1), porttest.SampleVersion("b", "k", 3)
	for _, v := range []object.Version{old, newer} {
		if _, err := store.PutVersion(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	marker := object.ID(bytes.Repeat([]byte{9}, object.IDSize))
	if _, err := store.DeleteObject(ctx, "b", "k", ports.DeleteOptions{MarkerID: marker, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	before := snapshotOf(t, store, "b")
	if err := store.fsm.db.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket(dataBucket()) }); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openStore(t, dir)
	if after := snapshotOf(t, reopened, "b"); !slices.Equal(before, after) {
		t.Fatalf("versions after a restart:\n  %v\nbefore:\n  %v", after, before)
	}
	for _, v := range []object.Version{old, newer} {
		if ok, err := reopened.IsDataReferenced(ctx, v.DataID); err != nil || !ok {
			t.Errorf("the data of a version that is not current is not referenced after the index was rebuilt (%v)", err)
		}
	}
	if stored, counts := storedCounts(t, reopened), recount(t, reopened); fmt.Sprint(stored) != fmt.Sprint(counts) || len(counts) != 2 {
		t.Errorf("rebuilt index %v, recount %v", stored, counts)
	}
	if b, err := reopened.GetBucket(ctx, "b"); err != nil || b.Versioning != bucket.VersioningEnabled {
		t.Errorf("bucket after a restart = %+v, %v", b, err)
	}
}
