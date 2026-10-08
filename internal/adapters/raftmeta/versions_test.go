package raftmeta

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/hashicorp/raft"

	"github.com/albievan/cactusfs/internal/domain"
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
		// The index of versions by name holds exactly the versions that
		// are in "versions", and leads to each.
		if h.older != nil {
			indexed := 0
			if err := h.ids.ForEach(func(at, inverted []byte) error {
				indexed++
				split := len(at) - object.IDSize
				if h.older.Get(append(append(bytes.Clone(at[:split]), inverted...), at[split:]...)) == nil {
					t.Errorf("the index of versions names one that is not there: %x", at)
				}
				return nil
			}); err != nil {
				return err
			}
			stored := 0
			if err := h.older.ForEach(func(k, raw []byte) error {
				stored++
				split := len(k) - versionSuffixSize
				if h.ids.Get(append(bytes.Clone(k[:split]), k[split+8:]...)) == nil {
					v, _ := decodeVersion(raw)
					t.Errorf("a stored version is not in the index of versions: %q %s marker=%v versioned=%v seq=%d", v.Key, v.VersionID[:4], v.DeleteMarker, v.Versioned, v.Seq)
				}
				return nil
			}); err != nil {
				return err
			}
			if stored != indexed {
				t.Errorf("%d versions are stored and %d are in the index of them", stored, indexed)
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
	if err := store.ScanVersions(context.Background(), bucketName, ports.VersionCursor{}, func(v object.Version, latest bool) ports.ScanControl {
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
			var data []object.ID
			serial := uint32(0)
			// fresh returns a version with an ID and data of its own.
			fresh := func(key string) object.Version {
				serial++
				v := porttest.SampleVersion("b", key, 1)
				binary.BigEndian.PutUint32(v.VersionID[4:8], serial)
				binary.BigEndian.PutUint32(v.DataID[4:8], serial)
				return v
			}
			at := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

			for step := range 300 {
				key := keys[random.IntN(len(keys))]
				var got [2]string
				switch op := random.IntN(20); {
				case op < 9: // put
					v := fresh(key)
					refs, data = append(refs, object.VersionRef{ID: v.VersionID}), append(data, v.DataID)
					for i, s := range stores {
						r, err := s.PutVersion(ctx, v)
						if err != nil {
							t.Fatalf("step %d: PutVersion: %v", step, err)
						}
						got[i] = "put " + describe(&r.Stored) + " removed " + describe(r.Removed)
					}
				case op < 13: // delete the object
					marker := fresh(key).VersionID
					refs = append(refs, object.VersionRef{ID: marker})
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
				for _, id := range data {
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
	if err := store.fsm.db.Update(func(tx *bbolt.Tx) error {
		if err := tx.DeleteBucket(versionIDsBucket()); err != nil {
			return err
		}
		return tx.DeleteBucket(dataBucket())
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openStore(t, dir)
	checkInvariant(t, reopened, "b")
	for _, ref := range []object.VersionRef{old.Ref(), {ID: old.VersionID}, {ID: marker}} {
		if ref.Null {
			continue
		}
		if _, err := reopened.GetVersion(ctx, "b", "k", ref); err != nil {
			t.Errorf("version %s after the index of versions was rebuilt: %v", ref, err)
		}
	}
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

// TestAKeyWithManyVersions: a key may have any number of versions. Finding
// one of them by name, finding the null version among them, and reading the
// first of them do not walk the others - which matters most where it happens
// inside the state machine, with every other change waiting.
func TestAKeyWithManyVersions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openStore(t, t.TempDir())
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	null := porttest.SampleVersion("b", "k", 1)
	if _, err := store.PutVersion(ctx, null); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBucketVersioning(ctx, "b", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	const many = 600
	var middle object.Version
	for i := range many {
		v := porttest.SampleVersion("b", "k", 3)
		binary.BigEndian.PutUint32(v.VersionID[:4], uint32(i)+1000)
		binary.BigEndian.PutUint32(v.DataID[:4], uint32(i)+1000)
		put, err := store.PutVersion(ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		if i == many/2 {
			// As it was stored: with its sequence and its name.
			middle = put.Stored
		}
	}
	// How many versions a lookup decodes, counted in the state itself.
	walked := func(fn func(h history)) int {
		t.Helper()
		n := 0
		if err := store.fsm.view(func(tx *bbolt.Tx) error {
			h, err := openHistory(tx, "b", false)
			if err != nil {
				return err
			}
			stats := tx.Stats()
			before := stats.GetCursorCount()
			fn(h)
			stats = tx.Stats()
			n = int(stats.GetCursorCount() - before)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for name, ref := range map[string]string{"the null version": object.NullVersion, "a version in the middle": middle.VersionID.String()} {
		cursors := walked(func(h history) {
			found, _, err := h.find("k", ref)
			if err != nil || found == nil || refOf(*found) != ref {
				t.Errorf("%s: %+v, %v", name, found, err)
			}
		})
		if cursors > 4 {
			t.Errorf("finding %s among %d versions opened %d cursors: it walks", name, many, cursors)
		}
	}
	// A scan that looks at the first version only does not read the rest.
	visited := 0
	if err := store.ScanVersions(ctx, "b", ports.VersionCursor{}, func(object.Version, bool) ports.ScanControl {
		visited++
		return ports.ScanControl{Stop: true}
	}); err != nil || visited != 1 {
		t.Fatalf("ScanVersions: %d visits, %v", visited, err)
	}
	// A scan resumed after a version in the middle goes on with the next.
	var next []object.Version
	after := middle.Ref()
	if err := store.ScanVersions(ctx, "b", ports.VersionCursor{Key: "k", After: &after}, func(v object.Version, latest bool) ports.ScanControl {
		if latest {
			t.Error("a version in the middle of a key is called its latest")
		}
		next = append(next, v)
		return ports.ScanControl{Stop: len(next) == 2}
	}); err != nil || len(next) != 2 || next[0].Seq >= middle.Seq || next[1].Seq >= next[0].Seq {
		t.Fatalf("resuming after a version in the middle: %d versions, %v", len(next), err)
	}
	// Resuming after the oldest version of the last key ends the scan;
	// after one that is gone, goes on with the next key.
	if _, err := store.PutVersion(ctx, porttest.SampleVersion("b", "z", 9)); err != nil {
		t.Fatal(err)
	}
	var keys []string
	gone := object.VersionRef{ID: object.ID{0xEE}}
	if err := store.ScanVersions(ctx, "b", ports.VersionCursor{Key: "k", After: &gone}, func(v object.Version, _ bool) ports.ScanControl {
		keys = append(keys, v.Key)
		return ports.ScanControl{}
	}); err != nil || !slices.Equal(keys, []string{"z"}) {
		t.Errorf("resuming after a version that is gone visits %v, %v", keys, err)
	}
	// Under Suspended every put replaces the null version: found, not searched for.
	if err := store.SetBucketVersioning(ctx, "b", bucket.VersioningSuspended); err != nil {
		t.Fatal(err)
	}
	replaced, err := store.PutVersion(ctx, porttest.SampleVersion("b", "k", 5))
	if err != nil || replaced.Removed == nil || replaced.Removed.DataID != null.DataID {
		t.Fatalf("a put over a null version under %d others = %+v, %v", many, replaced.Removed, err)
	}
	checkInvariant(t, store, "b")
}

// TestNoEntryRemovesAComplianceLockedVersion feeds the state machine
// arbitrary entries - retention changes of every mode and date, version
// deletes, object deletes, legal holds, default retentions, every one of
// them with the proposer claiming the right to bypass - at times before the
// retention ends. The version is there after each of them, in compliance
// mode, retained at least as long as it was. Only the time of an entry
// reaching the date changes that; the state machine reads no clock.
func TestNoEntryRemovesAComplianceLockedVersion(t *testing.T) {
	t.Parallel()
	for seed := range uint64(4) {
		f := newFSM(t)
		random := &choices{state: seed*7919 + 1}
		start := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
		until := start.Add(1000 * time.Hour)
		index := uint64(0)
		apply := func(at time.Time, e entry) applyResult {
			t.Helper()
			index++
			e.At = at
			data, err := encodeEntry(e, entrySchemaTimed)
			if err != nil {
				t.Fatal(err)
			}
			r := result(t, f.Apply(&raft.Log{Index: index, Type: raft.LogCommand, Data: data}))
			if r.fatal {
				t.Fatalf("entry %d (%s) stopped the state machine: %v", index, e.Type, r.err)
			}
			return r
		}
		locked := porttest.SampleVersion("vault", "ledger", 1)
		locked.Lock = object.Lock{Retention: &object.Retention{Mode: object.Compliance, Until: until}}
		if r := apply(start, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "vault", ObjectLock: true}}); r.err != nil {
			t.Fatal(r.err)
		}
		if r := apply(start, entry{Type: cmdPutObject, PutObject: versionToRecord(locked)}); r.err != nil {
			t.Fatal(r.err)
		}
		ref := locked.VersionID.String()
		check := func(what string) time.Time {
			t.Helper()
			var record *versionRecord
			if err := f.view(func(tx *bbolt.Tx) error {
				h, err := openHistory(tx, "vault", false)
				if err != nil {
					return err
				}
				record, _, err = h.find("ledger", ref)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if record == nil {
				t.Fatalf("seed %d: after %s the compliance-locked version is gone", seed, what)
			}
			lock := record.Lock.toDomain()
			if lock.Retention == nil || lock.Retention.Mode != object.Compliance || lock.Retention.Until.Before(until) {
				t.Fatalf("seed %d: after %s the version is retained as %+v, was compliance until %v", seed, what, lock.Retention, until)
			}
			return lock.Retention.Until
		}

		at := start
		modes := []string{"", string(object.Governance), string(object.Compliance), "NONE", "compliance"}
		for step := range 400 {
			// Time passes, never as far as the date. Now and then an
			// entry is stamped earlier than the one before it.
			at = at.Add(time.Duration(random.IntN(120)) * time.Minute)
			if at.After(until.Add(-time.Hour)) {
				at = until.Add(-time.Hour)
			}
			stamp := at
			if random.IntN(10) == 0 {
				stamp = at.Add(-time.Duration(random.IntN(5000)) * time.Hour)
			}
			var e entry
			switch random.IntN(9) {
			case 0, 1:
				e = entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{
					Bucket: "vault", Key: "ledger", VersionRef: ref, Bypass: random.IntN(2) == 0, MarkerID: object.ID{9}.String(), At: stamp,
				}}
			case 2, 3, 4:
				e = entry{Type: cmdSetRetention, SetRetention: &retentionCmd{
					Bucket: "vault", Key: "ledger", VersionRef: ref, Bypass: random.IntN(2) == 0,
					Mode:        modes[random.IntN(len(modes))],
					RetainUntil: start.Add(time.Duration(random.IntN(3000)-500) * time.Hour),
				}}
			case 5:
				e = entry{Type: cmdSetLegalHold, SetLegalHold: &legalHoldCmd{Bucket: "vault", Key: "ledger", VersionRef: ref, On: random.IntN(2) == 0}}
			case 6:
				marker := object.ID{}
				binary.BigEndian.PutUint64(marker[:8], uint64(step)+1)
				e = entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "vault", Key: "ledger", MarkerID: marker.String(), At: stamp, Bypass: true}}
			case 7:
				other := porttest.SampleVersion("vault", "ledger", 3)
				binary.BigEndian.PutUint64(other.VersionID[:8], uint64(step)+1)
				binary.BigEndian.PutUint64(other.DataID[:8], uint64(step)+1)
				e = entry{Type: cmdPutObject, PutObject: versionToRecord(other)}
			default:
				e = entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "vault"}}
				if random.IntN(2) == 0 {
					e = entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "vault", Status: string(bucket.VersioningSuspended)}}
				}
			}
			apply(stamp, e)
			// An extension that was accepted is the new floor.
			until = check(fmt.Sprintf("step %d (%s)", step, e.Type))
		}
		// The schema 1 delete does not reach it either.
		data, err := encodeEntry(entry{Type: cmdDeleteObject, DeleteObject: &deleteObjectCmd{Bucket: "vault", Key: "ledger"}}, entrySchemaVersion)
		if err != nil {
			t.Fatal(err)
		}
		index++
		_ = f.Apply(&raft.Log{Index: index, Type: raft.LogCommand, Data: data})
		check("a schema 1 delete")
		// Nor does an entry without a time.
		untimed, err := encodeEntry(entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{
			Bucket: "vault", Key: "ledger", VersionRef: ref, Bypass: true, MarkerID: object.ID{9}.String(),
		}}, entrySchemaTimed)
		if err != nil {
			t.Fatal(err)
		}
		index++
		_ = f.Apply(&raft.Log{Index: index, Type: raft.LogCommand, Data: untimed})
		check("a delete without a time")

		// When the time of an entry reaches the date - and only then -
		// the version can be removed. Legal hold comes off first.
		apply(until.Add(-time.Nanosecond), entry{Type: cmdSetLegalHold, SetLegalHold: &legalHoldCmd{Bucket: "vault", Key: "ledger", VersionRef: ref}})
		remove := entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "vault", Key: "ledger", VersionRef: ref, MarkerID: object.ID{9}.String()}}
		if r := apply(until.Add(-time.Nanosecond), remove); !errors.Is(r.err, domain.ErrObjectLocked) {
			t.Fatalf("seed %d: a nanosecond before the date the delete = %v, want %v", seed, r.err, domain.ErrObjectLocked)
		}
		check("a delete a nanosecond early")
		// An entry stamped before the last one does not turn time back,
		// and one stamped at the date is on time.
		if r := apply(until, remove); r.err != nil || r.previous == nil {
			t.Fatalf("seed %d: at the date the delete = %+v, %v", seed, r.previous, r.err)
		}
	}
}
