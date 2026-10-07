//go:build chaos && !windows

package chaos_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestScrubberFindsAndHealsCorruptionNobodyReads: bitrot on as many drives as
// there are parity shards, in data that no client touches. The scrubber alone
// finds it and rewrites the damaged shards; afterwards the same number of
// other drives can rot too and every object still reads back exactly.
func TestScrubberFindsAndHealsCorruptionNobodyReads(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.mustStart()
	n.createBucket()
	objects := n.populate(18)

	before := map[int]map[string][]byte{}
	for d := range parity {
		before[d] = n.snapshot(d)
		if n.corrupt(d) == 0 {
			t.Fatalf("drive %d has no shard to corrupt", d)
		}
	}
	// No object is read before the shards are back: a read would queue its
	// own repair, and this test is about the scrubber.
	for d := range parity {
		eventually(t, fmt.Sprintf("the scrubber to rewrite the shards on drive %d", d), func() bool { return n.restored(before[d]) })
	}
	// Redundancy is back: now the other three drives rot.
	for d := parity; d < drives; d++ {
		n.corrupt(d)
	}
	n.verify(objects, "after scrubbing, with corrupt shards on the other three drives")
	n.stop()
}

// TestReadsSurviveCorruptionAndDeletion: with shards corrupt on one drive and
// deleted on two more, every read is still exact - before anything has been
// repaired.
func TestReadsSurviveCorruptionAndDeletion(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	// No scrubbing: the reads have to cope by themselves.
	n.extra = []string{"--scrub-interval", "0"}
	n.mustStart()
	n.createBucket()
	objects := n.populate(18)

	n.corrupt(0)
	n.deleteShards(2)
	n.deleteShards(5)
	n.verify(objects, "with one drive corrupt and two drives emptied")
	// A read that had to work around damage queues the object for healing.
	before := len(n.shardFiles(2))
	eventually(t, "degraded reads to trigger healing", func() bool { return len(n.shardFiles(2)) > before })
	n.stop()
}

// TestDeletedShardsAreRebuiltByTheScrubber: shard files vanish from as many
// drives as there are parity shards, and nothing reads the objects.
func TestDeletedShardsAreRebuiltByTheScrubber(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.mustStart()
	n.createBucket()
	objects := n.populate(18)

	before := map[int]map[string][]byte{}
	for d := range parity {
		before[d] = n.snapshot(d)
		n.deleteShards(d)
	}
	for d := range parity {
		eventually(t, fmt.Sprintf("the scrubber to rebuild the shards on drive %d", d), func() bool { return n.restored(before[d]) })
	}
	for d := parity; d < drives; d++ {
		n.deleteShards(d)
	}
	n.verify(objects, "after scrubbing, with the shards of the other three drives deleted")
	n.stop()
}

// TestWipedDriveIsReplacedOnlyWhenConfirmedAndThenRefilled: a drive is
// replaced by an empty one while the node is down. An empty directory can
// also be a mount that is missing, so the node refuses to start until the
// operator confirms. Then it adopts the drive, the scrubber refills it without
// any object being read, and full redundancy is back. Two drives wiped at once
// are refused even when confirmed.
func TestWipedDriveIsReplacedOnlyWhenConfirmedAndThenRefilled(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.mustStart()
	n.createBucket()
	objects := n.populate(18)
	n.stop()

	for _, replaced := range []int{1, 4} {
		before := n.snapshot(replaced)
		n.wipe(replaced)

		n.extra = nil
		if err := n.start(); err == nil {
			n.kill()
			t.Fatalf("drive %d: the node started with an empty drive in place of a missing one, unconfirmed", replaced)
		} else if !strings.Contains(err.Error(), "confirm the replacement") {
			t.Fatalf("drive %d: unconfirmed start: %v", replaced, err)
		}

		n.extra = []string{"--allow-drive-replacement"}
		n.mustStart()
		if !n.logged("an empty drive has replaced a missing drive") {
			t.Errorf("drive %d: the replacement was not logged", replaced)
		}
		// Refilled by the scrubber alone, before any object is read, and
		// byte for byte what the lost drive held.
		eventually(t, fmt.Sprintf("drive %d to be refilled", replaced), func() bool {
			return len(n.shardFiles(replaced)) == len(before)
		})
		n.verify(objects, fmt.Sprintf("after drive %d was replaced and refilled", replaced))
		n.stop()

		// The confirmation was for that start only; the next needs none.
		n.extra = nil
		n.mustStart()
		n.stop()
	}

	// With both replacements refilled, three more drives can be lost.
	n.mustStart()
	for _, d := range []int{0, 2, 5} {
		n.deleteShards(d)
	}
	n.verify(objects, "after two replacements, with the shards of three other drives deleted")
	n.stop()

	// Two empty drives at once: which one replaces which cannot be known.
	n.wipe(0)
	n.wipe(3)
	n.extra = []string{"--allow-drive-replacement"}
	if err := n.start(); err == nil {
		n.kill()
		t.Fatal("the node started with two drives replaced at once")
	} else if !strings.Contains(err.Error(), "do not match the deployment") {
		t.Errorf("start with two wiped drives: %v", err)
	}
}

// TestShutdownDuringRefillLosesNothing: the node is stopped again and again
// while it is refilling a replaced drive. Healing that is interrupted must
// leave every object readable: whatever it had not finished, it must not have
// removed anything.
func TestShutdownDuringRefillLosesNothing(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.mustStart()
	n.createBucket()
	objects := map[string][]byte{}
	for i := range 300 {
		data := make([]byte, 200+i*37)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf("small-%03d", i)
		if err := n.put(key, data); err != nil {
			t.Fatalf("PutObject: %v", err)
		}
		objects[key] = data
	}
	n.stop()

	n.wipe(1)
	n.extra = []string{"--allow-drive-replacement"}
	for round := range 6 {
		n.mustStart()
		n.extra = nil
		// Stop at a different point of the refill each time.
		time.Sleep(time.Duration(150+round*120) * time.Millisecond)
		n.stop()
	}

	n.mustStart()
	n.verify(objects, "after six shutdowns during the refill of a replaced drive")
	eventually(t, "the replaced drive to be refilled", func() bool { return len(n.shardFiles(1)) == len(objects) })
	n.stop()
}

// TestEmptyMetadataDoesNotAdoptDrives: the metadata directory comes up empty -
// a mount that is missing, a wrong --meta-dir - while the drives hold objects.
// A node that started as a "new deployment" on them would see every shard as
// unreferenced, and its collector would delete all of them. It must refuse.
func TestEmptyMetadataDoesNotAdoptDrives(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.mustStart()
	n.createBucket()
	objects := n.populate(18)
	n.stop()

	// Every shard is old enough to be collected.
	old := time.Now().Add(-48 * time.Hour)
	files := 0
	for d := range drives {
		for _, path := range n.shardFiles(d) {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			files++
		}
	}
	metadata := filepath.Join(n.dir, "meta")
	if err := os.Rename(metadata, metadata+".away"); err != nil {
		t.Fatal(err)
	}

	if err := n.start(); err == nil {
		time.Sleep(3 * time.Second) // let the collector do its worst
		n.kill()
		t.Errorf("the node started on drives full of data with empty metadata")
	} else if !strings.Contains(err.Error(), "--meta-dir") {
		t.Errorf("start with empty metadata: %v", err)
	}
	remaining := 0
	for d := range drives {
		remaining += len(n.shardFiles(d))
	}
	if remaining != files {
		t.Fatalf("%d of %d shard files are left after a start with empty metadata", remaining, files)
	}

	// With the right metadata back, everything is as it was.
	if err := os.RemoveAll(metadata); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(metadata+".away", metadata); err != nil {
		t.Fatal(err)
	}
	n.mustStart()
	n.verify(objects, "after the metadata directory was restored")
	n.stop()
}

// TestMissingMountWithoutParityIsNeverReplaced: on a node without parity a
// drive's directory comes up empty - its filesystem is not mounted. Nothing on
// it could be rebuilt, so the empty directory must never take the drive's
// place, even with the confirmation: that would retire the only copy of every
// object. When the drive is back the node starts as before.
func TestMissingMountWithoutParityIsNeverReplaced(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.driveCount = 1
	n.mustStart()
	n.createBucket()
	objects := n.populate(9)
	n.stop()

	if err := os.Rename(n.drive(0), n.drive(0)+".unmounted"); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{nil, {"--allow-drive-replacement"}} {
		n.extra = extra
		if err := n.start(); err == nil {
			n.kill()
			t.Fatalf("the node started with an empty directory in place of its only drive (%v)", extra)
		} else if !strings.Contains(err.Error(), "no parity") {
			t.Errorf("start with the drive missing (%v): %v", extra, err)
		}
	}

	if err := os.RemoveAll(n.drive(0)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(n.drive(0)+".unmounted", n.drive(0)); err != nil {
		t.Fatal(err)
	}
	n.extra = nil
	n.mustStart()
	n.verify(objects, "after the drive was mounted again")
	n.stop()
}

// TestLossBeyondParityFailsCleanly: one object loses more shards than parity
// can rebuild. Reading it fails; it never returns other bytes; every other
// object is unaffected.
func TestLossBeyondParityFailsCleanly(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.mustStart()
	n.createBucket()
	objects := n.populate(9)

	victim := make([]byte, 2<<20)
	if _, err := rand.Read(victim); err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for d := range drives {
		for _, path := range n.shardFiles(d) {
			known[path] = true
		}
	}
	if err := n.put("victim", victim); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	// The victim's shards are the files that were not there before.
	damaged := 0
	for d := 0; d < drives && damaged < parity+1; d++ {
		for _, path := range n.shardFiles(d) {
			if known[path] {
				continue
			}
			data := readFile(t, path)
			data[len(data)/2] ^= 0x01
			writeFile(t, path, data)
			damaged++
		}
	}
	if damaged != parity+1 {
		t.Fatalf("damaged %d shards of the victim, want %d", damaged, parity+1)
	}

	for attempt := range 3 {
		got, err := n.get("victim")
		if err == nil && bytes.Equal(got, victim) {
			t.Fatalf("attempt %d: the object was read although %d of %d shards are corrupt", attempt, damaged, drives)
		}
		if err == nil {
			t.Fatalf("attempt %d: GetObject returned %d bytes that are not the object, without an error", attempt, len(got))
		}
		if len(got) > 0 && !bytes.HasPrefix(victim, got) {
			t.Fatalf("attempt %d: bytes were returned that are not part of the object", attempt)
		}
	}
	n.verify(objects, "next to an object damaged beyond repair")
	eventually(t, "the unrecoverable object to be reported", func() bool { return n.logged("damaged beyond repair") })
	n.stop()
}

// TestOrphanedShardsAreCollected: shards that no object refers to are removed
// once they are older than the grace period; young ones and everything in use
// stay.
func TestOrphanedShardsAreCollected(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.mustStart()
	n.createBucket()
	objects := n.populate(9)

	plant := func(age time.Duration) string {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		id := hex.EncodeToString(raw)
		dir := filepath.Join(n.drive(2), "shards", id[:2], id[2:4])
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, id+".0")
		writeFile(t, path, []byte("left behind by a write that never committed"))
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		return path
	}
	inUse := len(n.shardFiles(2))
	old, young := plant(3*time.Hour), plant(0)

	eventually(t, "the old orphan to be collected", func() bool {
		_, err := os.Stat(old)
		return os.IsNotExist(err)
	})
	if _, err := os.Stat(young); err != nil {
		t.Errorf("a shard younger than the grace period was removed: %v", err)
	}
	if got := len(n.shardFiles(2)); got != inUse+1 {
		t.Errorf("%d shard files on the drive, want the %d in use and the young orphan", got, inUse)
	}
	n.verify(objects, "after garbage collection")
	n.stop()
}

// TestKillDuringWritesLosesNothingAcknowledged: the process is killed while
// clients are writing. After a restart every upload that was acknowledged is
// there, whole; an upload that was not acknowledged is either absent or whole,
// never partial.
func TestKillDuringWritesLosesNothingAcknowledged(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.mustStart()
	n.createBucket()

	type upload struct {
		data         []byte
		acknowledged bool
	}
	var mu sync.Mutex
	uploads := map[string]*upload{}

	stop := make(chan struct{})
	var writers sync.WaitGroup
	for w := range 4 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				data := make([]byte, 1+(i*37_123+w*911)%(1<<20+5000))
				if _, err := rand.Read(data); err != nil {
					return
				}
				key := fmt.Sprintf("writer-%d/object-%04d", w, i)
				mu.Lock()
				uploads[key] = &upload{data: data}
				mu.Unlock()
				if err := n.put(key, data); err != nil {
					continue // the node is going down
				}
				mu.Lock()
				uploads[key].acknowledged = true
				mu.Unlock()
			}
		}()
	}

	time.Sleep(3 * time.Second)
	n.kill()
	close(stop)
	writers.Wait()

	n.mustStart()
	acknowledged := 0
	for key, u := range uploads {
		got, err := n.get(key)
		switch {
		case u.acknowledged && err != nil:
			t.Errorf("acknowledged object %s is gone after the crash: %v", key, err)
		case u.acknowledged && !bytes.Equal(got, u.data):
			t.Errorf("acknowledged object %s came back different after the crash", key)
		case !u.acknowledged && err == nil && !bytes.Equal(got, u.data):
			t.Errorf("unacknowledged object %s exists but is not whole: %d of %d bytes", key, len(got), len(u.data))
		}
		if u.acknowledged {
			acknowledged++
		}
	}
	if acknowledged < 10 {
		t.Fatalf("only %d uploads were acknowledged before the kill; the test did not exercise anything", acknowledged)
	}
	t.Logf("%d uploads acknowledged before the kill, %d attempted", acknowledged, len(uploads))

	// The node is fully usable after the crash.
	if err := n.put("after-crash", []byte("still works")); err != nil {
		t.Fatalf("PutObject after the crash: %v", err)
	}
	n.stop()
}
