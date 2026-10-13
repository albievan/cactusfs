package raftmeta

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// The evidence that a member of this release and a member of the release of
// schema 3 hold the same state while a cluster is at level 3 (ADR-0019): a
// log of level 3, and the state the state machine of the release of schema 3
// made of it.
//
//	testdata/schema3-release.log.hex     the log, an entry a line, in hex
//	testdata/schema3-release.state.json  what every entry came to, and the
//	                                     whole state database afterwards
//
// Both files were written by the code of commit 0b7d825 - the last release
// whose MetadataVersion is 3 - and never by a later tree: this file and
// schema3_release_gen_test.go were copied into an export of that commit and
// the writer was run there.
//
//	git archive 0b7d825 | tar -x -C "$old"
//	cp internal/adapters/raftmeta/schema3_release_test.go \
//	   internal/adapters/raftmeta/schema3_release_gen_test.go "$old/internal/adapters/raftmeta/"
//	(cd "$old" && CACTUSFS_SCHEMA3_RELEASE_OUT="$out" \
//	   go test -tags fixturegen -run '^TestWriteSchema3ReleaseEvidence$' -count=1 ./internal/adapters/raftmeta/)
//	cp "$out"/schema3-release.* internal/adapters/raftmeta/testdata/
//
// So everything in this file compiles in both trees and names nothing that
// schema 4 brought. The files are written once. Should the log ever need
// another entry, it is that commit that must write both again - the state
// this tree makes of a log is what is under test, never the evidence.
const (
	schema3ReleaseLogFile   = "schema3-release.log.hex"
	schema3ReleaseStateFile = "schema3-release.state.json"
)

// schema3ReleaseLog is a log as a cluster at level 3 can hold it: every entry
// in the schema it needs and none above the third, stamped by a leader whose
// clock moves on by a second an entry. It is made of what schema 4 changed
// the handling of - buckets created, puts, a completed upload, batches of
// reads, a bucket deleted and created again - of what schema 3 brought, and
// of bytes no proposer of either release writes but a log may hold: a
// "created_by" on a bucket.create whose schema byte says 3, 2 or 1, which
// the release of schema 3 does not know and drops, and this one must drop
// alike.
func schema3ReleaseLog(t *testing.T) [][]byte {
	t.Helper()
	at := time.Date(2026, 10, 12, 9, 0, 0, 123456789, time.UTC)
	var log [][]byte
	raw := func(schema byte, e entry) []byte {
		t.Helper()
		at = at.Add(time.Second)
		if schema >= entrySchemaTimed {
			e.At = at
		}
		data, err := encodeEntry(e, schema)
		if err != nil {
			t.Fatal(err)
		}
		log = append(log, data)
		return data
	}
	add := func(e entry) {
		t.Helper()
		schema := max(versionOf(e), entrySchemaTimed)
		if schema > entrySchema3 {
			t.Fatalf("%s needs schema %d: it cannot be in a log of level 3", e.Type, schema)
		}
		raw(schema, e)
	}
	// createdBy creates a bucket with a "created_by" beside its name, in an
	// entry of the given schema. The field is written into the bytes, not
	// through the record: the release of schema 3 has no such field.
	createdBy := func(schema byte, record bucketRecord, by string) {
		t.Helper()
		data := raw(schema, entry{Type: cmdCreateBucket, CreateBucket: &record})
		field, err := json.Marshal(by)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasSuffix(data, []byte("}}")) {
			t.Fatalf("a bucket.create does not end with its record: %s", data[1:])
		}
		data = slices.Concat(data[:len(data)-2], []byte(`,"created_by":`), field, []byte("}}"))
		var check struct {
			CreateBucket map[string]any `json:"create_bucket"`
		}
		if err := json.Unmarshal(data[1:], &check); err != nil || check.CreateBucket["created_by"] == nil {
			t.Fatalf("the entry with a creator written into it: %v: %s", err, data[1:])
		}
		log[len(log)-1] = data
	}
	put := func(bucketName, key, by string, fill byte) versionRecord {
		t.Helper()
		v := porttest.SampleVersion(bucketName, key, fill)
		v.CreatedBy = by
		record := versionToRecord(v)
		add(entry{Type: cmdPutObject, PutObject: record})
		return *record
	}
	id := func(fill byte) string { return object.ID(bytes.Repeat([]byte{fill}, object.IDSize)).String() }

	add(entry{Type: cmdSetNode, SetNode: &nodeRecord{ID: "node-a", RPCAddr: "node-a:9002", Roles: []string{"meta", "storage"}, MetadataVersion: entrySchema3}})
	add(entry{Type: cmdSetNode, SetNode: &nodeRecord{ID: "node-b", RPCAddr: "node-b:9002", Roles: []string{"gateway"}, MetadataVersion: entrySchema3 + 1}})
	// Buckets as a cluster at level 3 creates them, whoever asks: without
	// a creator.
	add(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedAt: at}})
	add(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "lock", CreatedAt: at, ObjectLock: true}})
	add(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "gone", CreatedAt: at}})
	// And with a creator all the same, under every schema byte below 4;
	// one that is too long and one of two lines, which schema 4 refuses.
	createdBy(entrySchema3, bucketRecord{Name: "s3", CreatedAt: at}, "user/smuggled")
	createdBy(entrySchemaTimed, bucketRecord{Name: "s2", CreatedAt: at, ObjectLock: true}, "user/smuggled")
	createdBy(entrySchemaVersion, bucketRecord{Name: "s1", CreatedAt: at}, "user/smuggled")
	createdBy(entrySchema3, bucketRecord{Name: "s3long", CreatedAt: at}, strings.Repeat("u", 300))
	createdBy(entrySchema3, bucketRecord{Name: "s3lines", CreatedAt: at}, "a\nb")
	createdBy(entrySchemaTimed, bucketRecord{Name: "b", CreatedAt: at}, "user/again")

	// Puts: keys and makers at and beyond every bound, and bytes that are
	// no text.
	k := put("b", "k", "user/albie", 1)
	put("b", "k2", "", 2)
	put("b", "nul\x00key", "user/albie", 3)
	put("b", "line\nbreak\r", "user/albie", 4)
	put("b", strings.Repeat("k", 1024), strings.Repeat("u", 256), 5)
	put("b", strings.Repeat("k", 1025), "root", 6)
	put("b", "bad\xff\xfeutf8", "user/\xff", 7)
	put("b", "by-two-lines", "user/a\nuser/b", 8)
	put("b", "by-long", strings.Repeat("u", 257), 9)
	put("b", "<html>& ", "root", 10)
	put("none", "k", "root", 11)
	put("b", "bad\xff\xfeutf8", "user/\xff", 12)

	// An upload with tags, a part, and its completion by another than who
	// began it; one that is aborted.
	u := porttest.SampleUpload("b", "large", 20)
	u.Tags = map[string]string{"stage": "draft"}
	part := porttest.SamplePart(u, 1, 10, 20)
	add(entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)})
	add(entry{Type: cmdPutPart, PutPart: &putPartCmd{Bucket: "b", Key: "large", UploadID: u.ID.String(), Part: partToRecord(part)}})
	add(entry{Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{
		Bucket: "b", Key: "large", UploadID: u.ID.String(), VersionID: id(21), ETag: "etag", ModifiedAt: at, CreatedBy: "key/AKID",
		Parts: []listedPart{{Number: 1, ETag: part.ETag, DataID: part.DataID.String(), Seal: bytes.Repeat([]byte{1}, 32)}},
	}})
	aborted := porttest.SampleUpload("b", "aborted", 22)
	add(entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(aborted)})
	add(entry{Type: cmdAbortUpload, AbortUpload: &uploadRefCmd{Bucket: "b", Key: "aborted", UploadID: aborted.ID.String()}})

	// Batches of reads: what is recorded, and everything that is not.
	item := func(bucketName, key, who string, presigned bool, downloads, copies int64) readItem {
		return readItem{Bucket: bucketName, Key: key, Version: refOf(k), VersionID: k.VersionID, Who: who, Presigned: presigned, Downloads: downloads, Copies: copies}
	}
	add(entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: "node-a", Boot: 1, Seq: 1, Items: []readItem{
		item("b", "k", "anonymous", false, 1, 0), item("b", "k", "user/x", true, 2, 1), item("b", "k", "a\nb", false, 1, 0),
		item("b", "k", strings.Repeat("w", 257), false, 1, 0), item("none", "k", "user/x", false, 1, 0), item("b", "k", "user/copy", false, 0, 3),
		item("b", "", "user/x", false, 1, 0), item("", "k", "user/x", false, 1, 0), item("b", "never-existed", "user/\xff", false, 1, 0),
		item("b", strings.Repeat("z", 2000), "user/x", false, 1, 0), item("b", "k", "", false, 1, 0), item("b", "k", "user/neg", false, -1, 0),
		item("gone", "old-key", "user/old", false, 1, 0),
	}}})

	// Versions, a delete marker, a version removed for good, tags.
	add(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "lock", Status: string(bucket.VersioningEnabled)}})
	first := put("lock", "k", "root", 30)
	put("lock", "k", "", 31)
	add(entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "lock", Key: "k", MarkerID: id(32), At: at}})
	add(entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: "lock", Key: "k", VersionRef: refOf(first), Tags: map[string]string{"stage": "final"}}})
	add(entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "b", Key: "k2", MarkerID: id(33), At: at}})

	// A bucket deleted and created again; reads of the old one that arrive
	// late; a batch seen before; a batch of a node nobody registered.
	add(entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "gone"}})
	add(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "gone", CreatedAt: at}})
	add(entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: "node-a", Boot: 1, Seq: 2, Items: []readItem{item("gone", "old-key-2", "user/old", false, 1, 0)}}})
	add(entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: "node-a", Boot: 1, Seq: 2, Items: []readItem{item("b", "k", "user/dup", false, 1, 0)}}})
	add(entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: "unknown", Boot: 1, Seq: 1, Items: []readItem{item("b", "k", "user/unk", false, 1, 0)}}})
	add(entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: "node-b", Boot: 4, Seq: 9, Items: []readItem{item("b", "k", "user/x", false, 3, 0), item("b", "k", "root", true, 1, 0)}}})

	// Bucket configuration, and what else schema 3 brought: sub-buckets,
	// the catalogue of tags, tag rules - each with its audit record.
	add(entry{Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Bucket: "b", Kind: bucket.ConfigPolicy, Document: []byte(`{}`)}})
	trail := &auditRecord{Time: at, Actor: "user/albie", Action: "s3:PutSubBucket", Resource: "b/reports", Outcome: "succeeded"}
	add(entry{Type: cmdPutSubBucket, PutSubBucket: &subBucketCmd{Bucket: "b", SubBucket: subBucketRecord{Name: "reports", CreatedAt: at, Policy: []byte(`{"Statement":[]}`)}, Audit: trail}})
	add(entry{Type: cmdPutSubBucket, PutSubBucket: &subBucketCmd{Bucket: "b", SubBucket: subBucketRecord{Name: "empty", CreatedAt: at}}})
	add(entry{Type: cmdDeleteSubBucket, DeleteSubBucket: &subBucketRefCmd{Bucket: "b", Name: "empty", Audit: trail}})
	add(entry{Type: cmdPutSubBucket, PutSubBucket: &subBucketCmd{Bucket: "none", SubBucket: subBucketRecord{Name: "x", CreatedAt: at}}})
	add(entry{Type: cmdPutTagDefinition, PutTagDefinition: &tagDefinitionCmd{
		Definition: tagDefinitionRecord{Key: "stage", Description: "d", Values: []string{"draft", "final"}, CreatedAt: at}, Audit: trail,
	}})
	add(entry{Type: cmdPutTagDefinition, PutTagDefinition: &tagDefinitionCmd{Definition: tagDefinitionRecord{Key: "team", CreatedAt: at}}})
	add(entry{Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{
		Bucket: "b", Kind: bucket.ConfigTagRules, Document: []byte(`{"version":1,"lowercase":true,"catalogue_only":false,"required":[{"key":"stage"}]}`),
	}})
	add(entry{Type: cmdDeleteTagDefinition, DeleteTagDefinition: &tagDefinitionRefCmd{Key: "stage"}})
	add(entry{Type: cmdDeleteTagDefinition, DeleteTagDefinition: &tagDefinitionRefCmd{Key: "team", Audit: trail}})

	// Buckets that hold something, or nothing, deleted at the end.
	add(entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "s1"}})
	add(entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "b"}})
	add(entry{Type: cmdRemoveNode, RemoveNode: &removeNodeCmd{ID: "node-b"}})
	return log
}

// schema3ReleaseEvidence is what a state machine made of a log: what every
// entry came to, and everything the state database holds afterwards.
type schema3ReleaseEvidence struct {
	// Outcomes says of each entry, in order: its index, its schema byte,
	// its command, and whether it was applied, refused, or stopped the
	// state machine.
	Outcomes []string `json:"outcomes"`
	// State is the state database, byte for byte. A key is the path of
	// bbolt buckets and the key in the last of them, each in hex, joined by
	// "/"; the value is in hex. A bucket is in it by its own path with a
	// "/" at the end, whether it holds anything or not.
	State map[string]string `json:"state"`
}

// schema3ReleaseState applies a log to a new state machine of the tree it is
// compiled in - held to the entry schema supported when that is not zero -
// and returns the evidence as it is stored: JSON, indented, the keys in
// order.
func schema3ReleaseState(t *testing.T, log [][]byte, supported byte) []byte {
	t.Helper()
	f := newFSM(t)
	if supported != 0 {
		f.supported = supported
	}
	// The drive is not waited for: what the state holds does not depend on
	// when it reached the drive, and a log of some thousand entries would
	// wait for it as often.
	f.db.NoSync = true
	evidence := schema3ReleaseEvidence{State: map[string]string{}}
	var index uint64
	for _, data := range log {
		index++
		var command struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data[1:], &command); err != nil {
			t.Fatalf("entry %d: %v", index, err)
		}
		r, ok := f.Apply(&raft.Log{Index: index, Type: raft.LogCommand, Data: data}).(applyResult)
		if !ok {
			t.Fatalf("entry %d: Apply did not return an applyResult", index)
		}
		outcome := "applied"
		switch {
		case r.fatal:
			outcome = "stopped"
		case r.err != nil:
			outcome = "refused"
		}
		evidence.Outcomes = append(evidence.Outcomes, fmt.Sprintf("%d schema=%d %s %s", index, data[0], command.Type, outcome))
	}
	var walk func(path string, b *bbolt.Bucket) error
	walk = func(path string, b *bbolt.Bucket) error {
		evidence.State[path+"/"] = ""
		return b.ForEach(func(k, v []byte) error {
			if nested := b.Bucket(k); nested != nil {
				return walk(path+"/"+hex.EncodeToString(k), nested)
			}
			evidence.State[path+"/"+hex.EncodeToString(k)] = hex.EncodeToString(v)
			return nil
		})
	}
	if err := f.view(func(tx *bbolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bbolt.Bucket) error { return walk(hex.EncodeToString(name), b) })
	}); err != nil {
		t.Fatal(err)
	}
	out, err := json.MarshalIndent(evidence, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// schema3ReleasePath writes a key of the evidence as it can be read: every
// element of the path as the bytes it stands for.
func schema3ReleasePath(key string) string {
	parts := strings.Split(key, "/")
	for i, part := range parts {
		if raw, err := hex.DecodeString(part); err == nil && part != "" {
			parts[i] = fmt.Sprintf("%q", raw)
		}
	}
	return strings.Join(parts, "/")
}

// TestThisReleaseAppliesALogOfLevel3AsTheSchema3ReleaseDoes: while one
// member is of the release before, the cluster is at level 3 and members of
// both releases apply the same log - and must hold the same bytes, or a
// snapshot taken on one is another state than the log gives the other. The
// log and the state in testdata are those of the release of schema 3 itself,
// written by its code (see the files' comment above). This tree's state
// machine, given that log, must come to the same outcome for every entry and
// to the same state database, byte for byte: nothing of schema 4 - no
// creator, no record of a bucket's use, no rise of the state schema.
//
// The same is asked of this tree's state machine held to schema 3, which is
// what the tests of a cluster stand in for a member of that release with
// (asSchema3Release): for this log the stand-in is the release.
//
// And the log is what this tree writes for the same entries at level 3: the
// encoding of what both releases know has not changed.
func TestThisReleaseAppliesALogOfLevel3AsTheSchema3ReleaseDoes(t *testing.T) {
	t.Parallel()
	files := os.DirFS("testdata")
	rawLog, err := fs.ReadFile(files, schema3ReleaseLogFile)
	if err != nil {
		t.Fatal(err)
	}
	want, err := fs.ReadFile(files, schema3ReleaseStateFile)
	if err != nil {
		t.Fatal(err)
	}
	var log [][]byte
	for line := range strings.SplitSeq(strings.TrimSuffix(string(rawLog), "\n"), "\n") {
		data, err := hex.DecodeString(line)
		if err != nil || len(data) < 2 {
			t.Fatalf("entry %d of %s: %d bytes, %v", len(log)+1, schema3ReleaseLogFile, len(data), err)
		}
		// What a cluster at level 3 can hold, and nothing else.
		if data[0] < entrySchemaVersion || data[0] > entrySchema3 {
			t.Fatalf("entry %d of %s is of schema %d: no log of level 3 holds it", len(log)+1, schema3ReleaseLogFile, data[0])
		}
		log = append(log, data)
	}
	var old schema3ReleaseEvidence
	if err := json.Unmarshal(want, &old); err != nil || len(old.Outcomes) != len(log) || len(log) < 50 {
		t.Fatalf("%s: %d outcomes for %d entries, %v", schema3ReleaseStateFile, len(old.Outcomes), len(log), err)
	}

	// The evidence is of a state that was reached: the release of schema 3
	// stopped at nothing, refused some of it, and kept buckets, versions
	// and records of reads.
	var refused int
	for _, outcome := range old.Outcomes {
		if strings.HasSuffix(outcome, " stopped") {
			t.Errorf("the release of schema 3 stopped at an entry of its own level: %s", outcome)
		}
		if strings.HasSuffix(outcome, " refused") {
			refused++
		}
	}
	if refused == 0 || refused > len(log)/2 {
		t.Errorf("the release of schema 3 refused %d of %d entries; the log is meant to hold both kinds", refused, len(log))
	}
	for _, name := range [][]byte{bucketsBucket(), objectsBucket(), accessBucket(), metaBucket()} {
		if _, ok := old.State[hex.EncodeToString(name)+"/"]; !ok {
			t.Errorf("the state of the release of schema 3 has no bucket %q", name)
		}
	}
	// And it holds nothing of schema 4, which this tree must not add: the
	// state says 3, no record of a bucket's use, and no bucket record with
	// a creator, in whatever entry one came.
	if got := old.State[hex.EncodeToString(metaBucket())+"/"+hex.EncodeToString(schemaKey())]; got != fmt.Sprintf("%016x", stateSchema3) {
		t.Errorf("the state of the release of schema 3 says schema %s", got)
	}
	for key, value := range old.State {
		raw, err := hex.DecodeString(value)
		if err != nil {
			t.Fatalf("%s: %v", schema3ReleasePath(key), err)
		}
		if strings.HasPrefix(key, hex.EncodeToString(bucketsBucket())+"/") && bytes.Contains(raw, []byte("created_by")) {
			t.Errorf("the release of schema 3 stored a bucket with a creator: %s = %s", schema3ReleasePath(key), raw)
		}
		if strings.HasPrefix(key, hex.EncodeToString([]byte("bucketactivity"))+"/") {
			t.Errorf("the release of schema 3 holds %s", schema3ReleasePath(key))
		}
	}

	if written := schema3ReleaseLog(t); !slices.EqualFunc(written, log, bytes.Equal) {
		for i := range min(len(written), len(log)) {
			if !bytes.Equal(written[i], log[i]) {
				t.Errorf("entry %d as this tree writes it at level 3:\n%s\nas the release of schema 3 wrote it:\n%s", i+1, written[i][1:], log[i][1:])
			}
		}
		t.Errorf("this tree writes %d entries, the release of schema 3 wrote %d", len(written), len(log))
	}

	for name, supported := range map[string]byte{"this release": 0, "its stand-in for the release of schema 3": entrySchema3} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := schema3ReleaseState(t, log, supported)
			if bytes.Equal(got, want) {
				return
			}
			var ours schema3ReleaseEvidence
			if err := json.Unmarshal(got, &ours); err != nil {
				t.Fatal(err)
			}
			for i, outcome := range ours.Outcomes {
				if outcome != old.Outcomes[i] {
					t.Errorf("entry: %s; in the release of schema 3: %s", outcome, old.Outcomes[i])
				}
			}
			for key, value := range ours.State {
				theirs, held := old.State[key]
				raw, _ := hex.DecodeString(value)
				rawTheirs, _ := hex.DecodeString(theirs)
				switch {
				case !held:
					t.Errorf("only this tree holds %s = %q", schema3ReleasePath(key), raw)
				case theirs != value:
					t.Errorf("%s:\n this tree: %q\n schema 3:  %q", schema3ReleasePath(key), raw, rawTheirs)
				}
			}
			for key, value := range old.State {
				if _, held := ours.State[key]; !held {
					raw, _ := hex.DecodeString(value)
					t.Errorf("only the release of schema 3 holds %s = %q", schema3ReleasePath(key), raw)
				}
			}
			t.Fatalf("a member of this release and a member of the release of schema 3 part at level 3: the evidence differs (%d bytes, want %d)", len(got), len(want))
		})
	}
}
