package raftmeta

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// The evidence that a member of this release and a member of the release of
// schema 4 hold the same state while a cluster is at level 4 (ADR-0023): a
// long log as a cluster at level 4 can hold it, and what the state machine of
// the release of schema 4 made of it.
//
//	testdata/schema4-release.sha256   how many entries the log has, the
//	                                  SHA-256 of the log, and the SHA-256 of
//	                                  the evidence: what every entry came to
//	                                  and the whole state database afterwards
//	                                  (schema3ReleaseState)
//
//	testdata/schema4-entries.golden.txt   every command there was before
//	                                      schema 5, and every old command in
//	                                      each form that needs a newer schema
//	                                      (everyCommand), as it is written at
//	                                      level 3 and at level 4: its name,
//	                                      the level, the schema byte, and the
//	                                      entry in hex: an entry that says
//	                                      nothing of schema 5 is the bytes it
//	                                      always was
//
// The log is not kept, as that of schema 3 is: it is some megabytes. It is
// made by schema4ReleaseLog from a generator whose sequence is fixed
// (dice, below), and its sum is in the file, so that a change of the
// generator, or of how an entry of schema 4 or below is encoded, is seen as
// such and not as a change of the state.
//
// Both files were written by the code of commit ddbfe43 - the last tree
// without anything of schema 5 in the codec or the state machine - and never
// by a later one: this file and schema4_release_gen_test.go were copied into
// an export of that commit and the writer was run there.
//
//	git archive ddbfe43 | tar -x -C "$old"
//	cp internal/adapters/raftmeta/schema4_release_test.go \
//	   internal/adapters/raftmeta/schema4_release_gen_test.go "$old/internal/adapters/raftmeta/"
//	(cd "$old" && CACTUSFS_SCHEMA4_RELEASE_OUT="$out" \
//	   go test -tags fixturegen -run '^TestWriteSchema4ReleaseEvidence$' -count=1 ./internal/adapters/raftmeta/)
//	cp "$out"/schema4-* internal/adapters/raftmeta/testdata/
//
// So everything in this file compiles in both trees and names nothing that
// schema 5 brought.
const (
	schema4ReleaseFile = "schema4-release.sha256"
	schema4EntriesFile = "schema4-entries.golden.txt"
	// schema4ReleaseEntries is how many entries of the log are made by
	// chance; a few that are fixed follow them.
	schema4ReleaseEntries = 2500
)

// schema4EntryBytes writes every command of everyCommand as a cluster at
// level 3 and one at level 4 write it, a line each, in the order of the
// names.
func schema4EntryBytes(t *testing.T) []byte {
	t.Helper()
	commands := everyCommand()
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	slices.Sort(names)
	var out []byte
	for _, name := range names {
		for _, level := range []byte{entrySchema3, entrySchema4} {
			held := heldTo(commands[name], level)
			schema := writtenAt(held, versionOf(held), level)
			data, err := encodeEntry(held, schema)
			if err != nil {
				t.Fatal(err)
			}
			out = fmt.Appendf(out, "%s\tlevel %d\tschema %d\t%x\n", name, level, schema, data)
		}
	}
	return out
}

// TestEntriesOfSchema4AndBelowAreTheBytesTheyWere: every command that was
// there before schema 5, written without anything schema 5 added, is byte
// for byte what the release of schema 4 writes - at level 3 and at level 4.
// A member of that release reads what this one writes while the cluster is
// mixed, and the other way about.
func TestEntriesOfSchema4AndBelowAreTheBytesTheyWere(t *testing.T) {
	t.Parallel()
	want, err := fs.ReadFile(os.DirFS("testdata"), schema4EntriesFile)
	if err != nil {
		t.Fatal(err)
	}
	got := schema4EntryBytes(t)
	if bytes.Equal(got, want) {
		return
	}
	gotLines, wantLines := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := range min(len(gotLines), len(wantLines)) {
		if gotLines[i] != wantLines[i] {
			t.Errorf("an entry is not written as the release of schema 4 writes it:\n got %s\nwant %s", gotLines[i], wantLines[i])
		}
	}
	if len(gotLines) != len(wantLines) {
		t.Errorf("%d lines, the release of schema 4 wrote %d", len(gotLines), len(wantLines))
	}
}

// dice is chance for the histories of these tests: the same seed, the same
// numbers, on every platform and in every release of Go.
type dice struct {
	state int
}

func newDice(seed int) *dice {
	d := &dice{state: seed & 0x7fffffff}
	// The first numbers of seeds that are close are close: roll past them.
	for range 8 {
		d.roll(2)
	}
	return d
}

// roll returns a number from 0 up to n.
func (d *dice) roll(n int) int {
	d.state = (d.state*1103515245 + 12345) & 0x7fffffff
	return (d.state >> 8) % n
}

// perm returns the numbers from 0 up to n in an order of chance.
func (d *dice) perm(n int) []int {
	out := make([]int, n)
	for i := range out {
		j := d.roll(i + 1)
		out[i], out[j] = out[j], i
	}
	return out
}

// schema4Upload is an upload the generator of the log began, and the parts
// it gave it.
type schema4Upload struct {
	bucket, key string
	upload      object.Upload
	parts       map[int]partRecord
}

// schema4ReleaseLog is a log as a cluster at level 4 holds it: mostly
// entries as a proposer that knows the level writes them, and some as one
// that learnt it late writes them - in the schema of level 3, 2 or 1. It is
// made of everything that changes the versions of a key or the parts of an
// upload, in buckets of every versioning state, with keys that meet each
// other often: puts and overwrites, deletes of each kind, the delete of
// schema 1, uploads with parts put again, completed with all or some of
// them, and aborted, a bucket deleted and created again, tags, legal holds,
// and entries that are refused; and at its end entries that carry what only
// schema 5 gives a meaning, and one that stops the state machine. The same
// log every time it is called.
func schema4ReleaseLog(t *testing.T) [][]byte {
	t.Helper()
	rng := newDice(4)
	at := time.Date(2026, 10, 12, 9, 0, 0, 123456789, time.UTC)
	var log [][]byte
	serial := 0
	id := func() object.ID {
		serial++
		sum := sha256.Sum256(fmt.Appendf(nil, "schema4-release/%d", serial))
		return object.ID(sum[:object.IDSize])
	}
	// add writes an entry as a proposer that takes the cluster to be at
	// level writes it, and the leader of a cluster at level 4 stamps it.
	add := func(level byte, e entry) {
		t.Helper()
		held := heldTo(e, level)
		schema := writtenAt(held, versionOf(held), level)
		at = at.Add(time.Second)
		if level >= entrySchemaTimed || schema >= entrySchemaTimed {
			schema = max(schema, entrySchemaTimed)
			held.At = at
		}
		if schema > entrySchema4 {
			t.Fatalf("%s needs schema %d: it cannot be in a log of level 4", e.Type, schema)
		}
		data, err := encodeEntry(held, schema)
		if err != nil {
			t.Fatal(err)
		}
		log = append(log, data)
	}
	level := func() byte {
		switch n := rng.roll(100); {
		case n < 80:
			return entrySchema4
		case n < 90:
			return entrySchema3
		case n < 95:
			return entrySchemaTimed
		}
		return entrySchemaVersion
	}
	buckets := []string{"plain", "kept", "paused", "locked", "late"}
	keys := []string{
		"a", "a/b", "a\x00b", "a\x01", "b", "zoë/π.txt", "k with \"quotes\" & <angles>", strings.Repeat("l", 300), "\U0001F335",
	}
	pick := func(from []string) string { return from[rng.roll(len(from))] }
	who := func() string { return pick([]string{"", "user/albie", "key/AKIDEXAMPLE", "root"}) }
	versions := map[string][]string{}
	var uploads []*schema4Upload
	ref := func(bucketName, key string) string {
		known := versions[bucketName+"\x00"+key]
		switch n := rng.roll(10); {
		case n < 3 || len(known) == 0 && n < 9:
			return object.NullVersion
		case n < 9:
			return known[rng.roll(len(known))]
		}
		return id().String()
	}

	add(entrySchema4, entry{Type: cmdSetNode, SetNode: &nodeRecord{ID: "node-a", RPCAddr: "node-a:9002", Roles: []string{"meta"}, MetadataVersion: entrySchema4}})
	add(entrySchema4, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "plain", CreatedAt: at, CreatedBy: "user/albie"}})
	add(entrySchema3, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "kept", CreatedAt: at, CreatedBy: "user/albie"}})
	add(entrySchema4, entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "kept", Status: string(bucket.VersioningEnabled)}})
	add(entrySchemaVersion, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "paused", CreatedAt: at}})
	add(entrySchema4, entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "paused", Status: string(bucket.VersioningEnabled)}})
	add(entrySchema4, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "locked", CreatedAt: at, ObjectLock: true, CreatedBy: "root"}})

	for len(log) < schema4ReleaseEntries {
		bucketName, key := pick(buckets), pick(keys)
		switch n := rng.roll(100); {
		case n < 38:
			v := porttest.SampleVersion(bucketName, key, 1)
			v.VersionID, v.DataID, v.Size, v.CreatedBy = id(), id(), int64(rng.roll(5000)), who()
			if rng.roll(10) == 0 {
				v.Tags = map[string]string{"stage": "draft"}
			}
			versions[bucketName+"\x00"+key] = append(versions[bucketName+"\x00"+key], v.VersionID.String())
			add(level(), entry{Type: cmdPutObject, PutObject: versionToRecord(v)})
		case n < 50:
			add(level(), entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: bucketName, Key: key, MarkerID: id().String(), At: at}})
		case n < 60:
			add(level(), entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{
				Bucket: bucketName, Key: key, VersionRef: ref(bucketName, key), MarkerID: id().String(), At: at, Bypass: rng.roll(2) == 0,
			}})
		case n < 63:
			add(entrySchemaVersion, entry{Type: cmdDeleteObject, DeleteObject: &deleteObjectCmd{Bucket: bucketName, Key: key}})
		case n < 66:
			status := pick([]string{string(bucket.VersioningEnabled), string(bucket.VersioningSuspended)})
			add(level(), entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: pick([]string{"paused", "paused", "late"}), Status: status}})
		case n < 72:
			u := porttest.SampleUpload(bucketName, key, 1)
			u.ID = id()
			if rng.roll(5) == 0 {
				u.Tags = map[string]string{"stage": "draft"}
			}
			uploads = append(uploads, &schema4Upload{bucket: bucketName, key: key, upload: u, parts: map[int]partRecord{}})
			add(level(), entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)})
		case n < 84 && len(uploads) > 0:
			u := uploads[rng.roll(len(uploads))]
			size := int64(object.MinPartSize)
			if rng.roll(3) == 0 {
				size = int64(rng.roll(4000))
			}
			part := porttest.SamplePart(u.upload, 1+rng.roll(3), size, 1)
			part.DataID = id()
			u.parts[part.Number] = partToRecord(part)
			add(level(), entry{Type: cmdPutPart, PutPart: &putPartCmd{Bucket: u.bucket, Key: u.key, UploadID: u.upload.ID.String(), Part: partToRecord(part)}})
		case n < 89 && len(uploads) > 0:
			at := rng.roll(len(uploads))
			u := uploads[at]
			cmd := &completeUploadCmd{
				Bucket: u.bucket, Key: u.key, UploadID: u.upload.ID.String(), VersionID: id().String(), ETag: "etag-" + fmt.Sprint(len(u.parts)), CreatedBy: who(),
			}
			numbers := make([]int, 0, len(u.parts))
			for number := range u.parts {
				numbers = append(numbers, number)
			}
			slices.Sort(numbers)
			for _, number := range numbers {
				if rng.roll(4) == 0 {
					// A part that is not listed goes with the upload.
					continue
				}
				part := u.parts[number]
				if rng.roll(12) == 0 {
					// The part as it was before it was put again.
					part.DataID = id().String()
				}
				cmd.Parts = append(cmd.Parts, listedPart{Number: number, ETag: part.ETag, Checksums: part.Checksums, DataID: part.DataID, Seal: bytes.Repeat([]byte{"\x00\x01\x02\x03"[number]}, sha256.Size)})
			}
			versions[u.bucket+"\x00"+u.key] = append(versions[u.bucket+"\x00"+u.key], cmd.VersionID)
			if rng.roll(5) != 0 {
				// Mostly forgotten here whatever comes of it: some are
				// then completed or aborted twice, or put to once gone.
				uploads = slices.Delete(uploads, at, at+1)
			}
			add(level(), entry{Type: cmdCompleteUpload, CompleteUpload: cmd})
		case n < 92 && len(uploads) > 0:
			at := rng.roll(len(uploads))
			u := uploads[at]
			if rng.roll(5) != 0 {
				uploads = slices.Delete(uploads, at, at+1)
			}
			add(level(), entry{Type: cmdAbortUpload, AbortUpload: &uploadRefCmd{Bucket: u.bucket, Key: u.key, UploadID: u.upload.ID.String()}})
		case n < 95:
			if rng.roll(2) == 0 {
				add(level(), entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "late", CreatedAt: at, CreatedBy: who()}})
			} else {
				add(level(), entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "late"}})
			}
		case n < 97:
			add(level(), entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: bucketName, Key: key, VersionRef: ref(bucketName, key), Tags: map[string]string{"n": fmt.Sprint(n)}}})
		case n < 98:
			add(level(), entry{Type: cmdSetLegalHold, SetLegalHold: &legalHoldCmd{Bucket: "locked", Key: key, VersionRef: ref("locked", key), On: rng.roll(3) == 0}})
		case n < 99:
			cmd := &bucketConfigCmd{Bucket: bucketName, Kind: bucket.ConfigPolicy}
			if rng.roll(2) == 0 {
				cmd.Document = []byte(`{"Version":"2012-10-17","Statement":[]}`)
			}
			add(level(), entry{Type: cmdSetBucketConfig, SetBucketConfig: cmd})
		}
	}
	// And what chance may leave out: a bucket that is deleted, and a version
	// that a legal hold keeps from being removed.
	add(entrySchema4, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "brief", CreatedAt: at, CreatedBy: "root"}})
	add(entrySchema4, entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "brief"}})
	held := porttest.SampleVersion("locked", "held", 1)
	held.VersionID, held.DataID = id(), id()
	add(entrySchema4, entry{Type: cmdPutObject, PutObject: versionToRecord(held)})
	add(entrySchema4, entry{Type: cmdSetLegalHold, SetLegalHold: &legalHoldCmd{Bucket: "locked", Key: "held", VersionRef: held.VersionID.String(), On: true}})
	add(entrySchema4, entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{
		Bucket: "locked", Key: "held", VersionRef: held.VersionID.String(), MarkerID: id().String(), At: at, Bypass: true,
	}})

	// And what no proposer of level 4 writes and a log of level 4 may hold
	// all the same, since nothing but the codec keeps it out: entries of
	// schema 2, 3 and 4 that carry, as JSON, the names that only schema 5
	// gives a meaning - and a size below nothing. The release of schema 4
	// reads past every name it does not know; a later one must come to the
	// same state, which is to say: must take none of them for what they say.
	// They are written here as bytes, by putting them into an entry that
	// was encoded without them, so that this file names nothing that schema
	// 5 brought.
	raw := func(schema byte, e entry, old, with string) {
		t.Helper()
		at = at.Add(time.Second)
		if schema >= entrySchemaTimed {
			e.At = at
		}
		data, err := encodeEntry(e, schema)
		if err != nil {
			t.Fatal(err)
		}
		if old != "" {
			if !bytes.Contains(data, []byte(old)) {
				t.Fatalf("%s of schema %d has no %s to write over: %s", e.Type, schema, old, data)
			}
			data = bytes.Replace(data, []byte(old), []byte(with), 1)
		}
		log = append(log, data)
	}
	const replica = `"replica":{"from":"5d3c1f0a-site-a","seq":9000},`
	for _, schema := range []byte{entrySchemaTimed, entrySchema3, entrySchema4} {
		name := fmt.Sprintf("raw%d", schema)
		raw(schema, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: name, CreatedAt: at, CreatedBy: "root"}},
			`"create_bucket":{`, `"create_bucket":{"quota_bytes":77,"replica_from":"5d3c1f0a-site-a",`)
		raw(schema, entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: name, Status: string(bucket.VersioningEnabled)}}, "", "")
		for range 2 {
			v := porttest.SampleVersion(name, "k", 1)
			v.VersionID, v.DataID, v.Size = id(), id(), 100
			raw(schema, entry{Type: cmdPutObject, PutObject: versionToRecord(v)},
				`"put_object":{`, `"put_object":{"replication":"PENDING",`+replica)
		}
		raw(schema, entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: name, Key: "k", MarkerID: id().String(), At: at}},
			`"delete_version":{`, `"delete_version":{"by":"user/albie",`+replica)
		u := porttest.SampleUpload(name, "up", 1)
		u.ID = id()
		raw(schema, entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)},
			`"create_upload":{`, `"create_upload":{"replica_from":"5d3c1f0a-site-a",`)
		part := porttest.SamplePart(u, 1, 10, 1)
		part.DataID = id()
		stored := partToRecord(part)
		raw(schema, entry{Type: cmdPutPart, PutPart: &putPartCmd{Bucket: name, Key: "up", UploadID: u.ID.String(), Part: stored}}, "", "")
		raw(schema, entry{Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{
			Bucket: name, Key: "up", UploadID: u.ID.String(), VersionID: id().String(), ETag: "etag-1", ModifiedAt: at,
			Parts: []listedPart{{Number: 1, ETag: stored.ETag, Checksums: stored.Checksums, DataID: stored.DataID, Seal: bytes.Repeat([]byte{1}, sha256.Size)}},
		}}, `"complete_upload":{`, `"complete_upload":{`+replica)
		// The payloads of two commands of schema 5 beside a command that
		// is not of it: they are not what the entry is.
		v := porttest.SampleVersion(name, "beside", 1)
		v.VersionID, v.DataID, v.Size = id(), id(), 5
		raw(schema, entry{Type: cmdPutObject, PutObject: versionToRecord(v)}, `"put_object":{`,
			`"set_bucket_quota":{"bucket":"`+name+`","bytes":5},"walk_bucket_usage":{"bucket":"`+name+`","limit":5},"put_object":{`)
		// A size below nothing, which apply takes from an entry.
		v = porttest.SampleVersion(name, "below", 1)
		v.VersionID, v.DataID, v.Size = id(), id(), 7
		raw(schema, entry{Type: cmdPutObject, PutObject: versionToRecord(v)}, `"size":7`, `"size":-7`)
		// The two kinds of bucket configuration that came with schema 5.
		for _, kind := range []string{"notification", "replication"} {
			raw(schema, entry{Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Bucket: name, Kind: bucket.ConfigPolicy, Document: []byte("{}")}},
				`"kind":"policy"`, `"kind":"`+kind+`"`)
		}
		raw(schema, entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: name, Key: "below", VersionRef: object.NullVersion, MarkerID: id().String(), At: at}}, "", "")
	}
	// The last entry of the log, and it must be the last: a command of
	// schema 5 in an entry whose byte says 4. The release of schema 4 does
	// not know the command and stops there; so must every later one that
	// reads the byte, whatever it knows (schema4LastEntry).
	raw(entrySchema4, entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "raw4", Status: string(bucket.VersioningEnabled)}},
		`"type":"bucket.versioning.put"`, `"type":"bucket.quota.put","set_bucket_quota":{"bucket":"raw4","bytes":5}`)
	return log
}

// schema4LastEntry is what the last entry of the log of level 4 comes to:
// the one entry of it that stops the state machine.
const schema4LastEntry = "schema=4 bucket.quota.put stopped"

// schema4RawNames are what the entries at the end of the log of level 4
// carry that only schema 5 gives a meaning (schema4ReleaseLog).
func schema4RawNames() []string {
	return []string{
		`"create_bucket":{"quota_bytes":77,"replica_from":"5d3c1f0a-site-a",`,
		`"put_object":{"replication":"PENDING","replica":{`,
		`"delete_version":{"by":"user/albie","replica":{`,
		`"create_upload":{"replica_from":"5d3c1f0a-site-a",`,
		`"complete_upload":{"replica":{`,
		`"set_bucket_quota":{"bucket":"raw4","bytes":5},"walk_bucket_usage":{"bucket":"raw4","limit":5},"put_object":{`,
		`"size":-7`,
		`"kind":"notification"`,
		`"kind":"replication"`,
	}
}

// schema4ReleaseSums returns the evidence as the file holds it: the number
// of entries of the log, its sum, and the sum of what the state machine of
// the tree it is compiled in - held to the entry schema supported when that
// is not zero - made of it.
func schema4ReleaseSums(t *testing.T, log [][]byte, supported byte) []byte {
	t.Helper()
	logSum := sha256.New()
	for _, data := range log {
		logSum.Write([]byte(hex.EncodeToString(data) + "\n"))
	}
	state := schema3ReleaseState(t, log, supported)
	if last := fmt.Sprintf("%d %s", len(log), schema4LastEntry); strings.Count(string(state), "stopped") != 1 || !strings.Contains(string(state), last) {
		t.Fatalf("the state machine is to stop at the last entry of the log and at no other (%q)", last)
	}
	stateSum := sha256.Sum256(state)
	return fmt.Appendf(nil, "entries %d\nlog %x\nstate %x\n", len(log), logSum.Sum(nil), stateSum)
}

// TestThisReleaseAppliesALogOfLevel4AsTheSchema4ReleaseDoes: a long log of a
// cluster at level 4 comes to the same outcomes and the same state database,
// byte for byte, as in the release of schema 4. Nothing that schema 5 keeps -
// the usage of a bucket above all, which every put, delete, part and
// completion would keep - exists in a state no entry of schema 5 was applied
// to, and no entry of schema 4 or below is encoded or applied otherwise than
// it was.
func TestThisReleaseAppliesALogOfLevel4AsTheSchema4ReleaseDoes(t *testing.T) {
	t.Parallel()
	want, err := fs.ReadFile(os.DirFS("testdata"), schema4ReleaseFile)
	if err != nil {
		t.Fatal(err)
	}
	log := schema4ReleaseLog(t)
	if again := schema4ReleaseLog(t); !slices.EqualFunc(log, again, bytes.Equal) {
		t.Fatal("the generator of the log does not make the same log twice")
	}
	// The two are one configuration while this binary announces schema 4,
	// and two from PLAN 8.0.10 on, when "this release" applies schema 5.
	for name, supported := range map[string]byte{"this release": 0, "held to schema 4": entrySchema4} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := schema4ReleaseSums(t, log, supported); !bytes.Equal(got, want) {
				t.Errorf("the log of level 4 and what this tree makes of it are not those of the release of schema 4 (the first line that differs says which):\n got %swant %s", got, want)
			}
		})
	}
}

// TestTheLogOfLevel4IsWorthItsName: the log holds what the test above is
// for - every kind of change of a key and of an upload, taking effect and
// refused, in entries of every schema up to the fourth.
func TestTheLogOfLevel4IsWorthItsName(t *testing.T) {
	t.Parallel()
	log := schema4ReleaseLog(t)
	state := string(schema3ReleaseState(t, log, 0))
	// What only schema 5 gives a meaning, in entries of each schema from the
	// second to the fourth.
	for _, schema := range []byte{entrySchemaTimed, entrySchema3, entrySchema4} {
		for _, name := range schema4RawNames() {
			name = strings.ReplaceAll(name, "raw4", fmt.Sprintf("raw%d", schema))
			if !slices.ContainsFunc(log, func(data []byte) bool { return data[0] == schema && bytes.Contains(data, []byte(name)) }) {
				t.Errorf("the log of level 4 has no entry of schema %d that carries %s", schema, name)
			}
		}
	}
	for _, want := range []string{
		"schema=1 object.put applied", "schema=2 object.put applied", "schema=3 object.put applied", "schema=4 object.put applied",
		"object.version.delete applied", "object.version.delete refused", "object.delete applied",
		"upload.create applied", "upload.part.put applied", "upload.part.put refused",
		"schema=4 upload.complete applied", "upload.complete refused", "upload.abort applied", "upload.abort refused",
		"bucket.delete applied", "bucket.delete refused", "schema=4 bucket.create applied", "bucket.versioning.put applied",
		"object.tags.put applied", "object.legalhold.put applied", "bucket.config.put applied",
		"schema=2 bucket.config.put refused", "schema=3 bucket.config.put refused", "schema=4 bucket.config.put refused", schema4LastEntry,
	} {
		if !strings.Contains(state, want) {
			t.Errorf("the log of level 4 has no entry that says %q", want)
		}
	}
}
