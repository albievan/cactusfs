package raftmeta

import (
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// What a put costs the state machine (ADR-0023, "Consequences": one usage
// record read and written in the transaction of every put). This file names
// nothing that schema 5 brought, so that it compiles and runs in an export of
// commit ddbfe43 - the tree before the usage record - and the same benchmark
// gives the figure of that tree:
//
//	git archive ddbfe43 | tar -x -C "$old"
//	cp internal/adapters/raftmeta/put_bench_test.go "$old/internal/adapters/raftmeta/"
//	(cd "$old" && go test -run '^$' -bench 'BenchmarkApplyPutAtLevel4' -benchtime 3x -count 1 ./internal/adapters/raftmeta/)
//
// BenchmarkApplyPutWithAUsageRecord (bucketusage_test.go) is the same put
// into a bucket that has a record.

// benchPuts applies b.N puts to bucket "b" of a state machine, as a cluster
// at level 4 writes them: over a thousand keys, so that most of them replace
// a version. index is the index of the last entry the state machine applied.
func benchPuts(b *testing.B, f *fsm, index uint64) {
	b.Helper()
	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	entries := make([][]byte, b.N)
	for i := range entries {
		v := porttest.SampleVersion("b", fmt.Sprintf("key-%04d", i%1000), 1)
		sum := sha256.Sum256(fmt.Appendf(nil, "bench/%d", i))
		v.VersionID, v.DataID, v.Size, v.CreatedBy = object.ID(sum[:object.IDSize]), object.ID(sum[object.IDSize:]), int64(i%4096), "user/albie"
		e := entry{Type: cmdPutObject, At: at.Add(time.Duration(i) * time.Millisecond), PutObject: versionToRecord(v)}
		data, err := encodeEntry(e, entrySchema4)
		if err != nil {
			b.Fatal(err)
		}
		entries[i] = data
	}
	b.ReportAllocs()
	b.ResetTimer()
	for _, data := range entries {
		index++
		r, ok := f.Apply(&raft.Log{Index: index, Type: raft.LogCommand, Data: data}).(applyResult)
		if !ok || r.err != nil {
			b.Fatalf("put at entry %d: %v", index, r.err)
		}
	}
}

// benchFSM opens a state machine for a benchmark. Without sync its database
// does not wait for the drive, so that what is measured is the work of
// applying an entry and not the drive's.
func benchFSM(b *testing.B, sync bool) *fsm {
	b.Helper()
	root, err := os.OpenRoot(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	f, err := openFSM(root)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = f.close(); _ = root.Close() })
	f.db.NoSync = !sync
	return f
}

// BenchmarkApplyPutAtLevel4 is a put into a bucket that has no usage record:
// every put of a cluster at level 4, and - run in the tree before the record
// - what a put cost then.
func BenchmarkApplyPutAtLevel4(b *testing.B) {
	for name, sync := range map[string]bool{"without waiting for the drive": false, "durable": true} {
		b.Run(name, func(b *testing.B) {
			f := benchFSM(b, sync)
			data, err := encodeEntry(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedBy: "root"}}, entrySchema4)
			if err != nil {
				b.Fatal(err)
			}
			if r, ok := f.Apply(&raft.Log{Index: 1, Type: raft.LogCommand, Data: data}).(applyResult); !ok || r.err != nil {
				b.Fatalf("bucket.create: %v", r.err)
			}
			benchPuts(b, f, 1)
		})
	}
}
