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

// TestCorruptedShardsAreDetectedAndHealed: bitrot on as many drives as there
// are parity shards. Reads stay correct, the scrubber rewrites the damaged
// shards, and afterwards the same number of other drives can rot too.
func TestCorruptedShardsAreDetectedAndHealed(t *testing.T) {
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
	n.verify(objects, "with corrupt shards on three drives")

	for d := range parity {
		eventually(t, fmt.Sprintf("the shards on drive %d to be rewritten", d), func() bool { return n.restored(before[d]) })
	}
	// Redundancy is back: now the other three drives rot.
	for d := parity; d < drives; d++ {
		n.corrupt(d)
	}
	n.verify(objects, "after healing, with corrupt shards on the other three drives")
	n.stop()
}

// TestDeletedShardsAreRebuilt: shard files vanish from as many drives as
// there are parity shards.
func TestDeletedShardsAreRebuilt(t *testing.T) {
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
	n.verify(objects, "with the shards of three drives deleted")

	for d := range parity {
		eventually(t, fmt.Sprintf("the shards on drive %d to be rebuilt", d), func() bool { return n.restored(before[d]) })
	}
	for d := parity; d < drives; d++ {
		n.deleteShards(d)
	}
	n.verify(objects, "after healing, with the shards of the other three drives deleted")
	n.stop()
}

// TestWipedDriveIsReplacedAndRefilled: a drive is replaced by an empty one
// while the node is down. The node adopts it, objects stay readable, and the
// drive is refilled. Two drives wiped at once are refused: that needs a
// person.
func TestWipedDriveIsReplacedAndRefilled(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.mustStart()
	n.createBucket()
	objects := n.populate(18)
	n.stop()

	for _, replaced := range []int{1, 4} {
		files := len(n.shardFiles(replaced))
		n.wipe(replaced)
		n.mustStart()
		if !n.logged("an empty drive has replaced a missing drive") {
			t.Errorf("drive %d: the replacement was not logged", replaced)
		}
		n.verify(objects, fmt.Sprintf("with drive %d replaced by an empty one", replaced))
		eventually(t, fmt.Sprintf("drive %d to be refilled", replaced), func() bool { return len(n.shardFiles(replaced)) == files })
		n.verify(objects, fmt.Sprintf("after drive %d was refilled", replaced))
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
	if err := n.start(); err == nil {
		n.kill()
		t.Fatal("the node started with two drives replaced at once")
	} else if !strings.Contains(err.Error(), "do not match the deployment") {
		t.Errorf("start with two wiped drives: %v", err)
	}
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
	old, young := plant(2*time.Hour), plant(0)

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
