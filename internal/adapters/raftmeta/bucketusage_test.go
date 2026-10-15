package raftmeta

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hashicorp/raft"
	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// The usage record of every bucket, the walk that counts a bucket, and the
// quota that is held in apply (ADR-0023, "Bucket usage"; ADR-0024; PLAN
// 8.0.7). The state machines here apply schema 5, as this binary does since
// PLAN 8.0.10.

// usageBytes returns the usage record of a bucket as it is stored; nil when
// it has none.
func usageBytes(t *testing.T, f *fsm, name string) []byte {
	t.Helper()
	var raw []byte
	if err := f.view(func(tx *bbolt.Tx) error {
		if records := tx.Bucket(bucketUsageBucket()); records != nil {
			raw = bytes.Clone(records.Get([]byte(name)))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return raw
}

// usageOf reads the usage record of a bucket and its quota from the state.
func usageOf(t *testing.T, f *fsm, name string) (usage bucket.Usage, kept bool, quota int64) {
	t.Helper()
	if err := f.view(func(tx *bbolt.Tx) (err error) {
		if usage, kept, err = readUsage(tx, name); err != nil {
			return err
		}
		b, err := readBucket(tx, name)
		if b != nil {
			quota = b.QuotaBytes
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return usage, kept, quota
}

// setUsage writes a usage record into the state behind the back of the
// state machine: a record that is wrong, as only a fault could leave it.
func setUsage(t *testing.T, f *fsm, name string, usage bucket.Usage) {
	t.Helper()
	if err := f.db.Update(func(tx *bbolt.Tx) error {
		_, err := writeUsage(tx, name, usage)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// counted is what a bucket holds, counted by looking at every record of it
// in the state - by other code than the walk's: all of it, or, with a count,
// what a walk at that place has been past. high is the greatest sequence of
// all its versions. A size below nothing counts as nothing (ADR-0023).
func counted(t *testing.T, f *fsm, name string, count *bucket.UsageCount) (totals bucket.UsageTotals, high uint64) {
	t.Helper()
	ofKey := func(key string) bool {
		return count == nil || count.Phase != bucket.UsagePhaseKeys || strings.Compare(key, count.Cursor) <= 0
	}
	ofUpload := func(id string) bool {
		return count == nil || count.Phase == bucket.UsagePhaseDone || (count.Phase == bucket.UsagePhaseUploads && id <= count.Cursor)
	}
	add := func(raw []byte, key string, current bool) error {
		var v versionRecord
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		// A key that is no UTF-8 cannot be said in the record it is stored
		// under (planted, below).
		if v.Key != key && utf8.ValidString(key) {
			return fmt.Errorf("a version of %q is stored under %q", v.Key, key)
		}
		high = max(high, v.Seq)
		if !ofKey(key) {
			return nil
		}
		totals.Versions++
		totals.StoredBytes += max(v.Size, 0)
		if current {
			totals.Objects++
			totals.CurrentBytes += max(v.Size, 0)
		}
		switch object.ReplicationStatus(v.Replication) {
		case object.ReplicationPending:
			totals.ReplicationPending++
		case object.ReplicationFailed:
			totals.ReplicationFailed++
		}
		return nil
	}
	if err := f.view(func(tx *bbolt.Tx) error {
		if index := tx.Bucket(objectsBucket()).Bucket([]byte(name)); index != nil {
			if err := index.ForEach(func(k, raw []byte) error { return add(raw, string(k), true) }); err != nil {
				return err
			}
		}
		if older := tx.Bucket(versionsBucket()).Bucket([]byte(name)); older != nil {
			if err := older.ForEach(func(k, raw []byte) error {
				key, _, ok := decodeKey(k)
				if !ok {
					return fmt.Errorf("a key of versions: %x", k)
				}
				return add(raw, key, false)
			}); err != nil {
				return err
			}
		}
		if parts := tx.Bucket(partsBucket()).Bucket([]byte(name)); parts != nil {
			return parts.ForEach(func(k, raw []byte) error {
				var p partRecord
				if err := json.Unmarshal(raw, &p); err != nil {
					return err
				}
				if ofUpload(object.ID(k[:object.IDSize]).String()) {
					totals.UploadBytes += max(p.Size, 0)
				}
				return nil
			})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return totals, high
}

// everyVersion returns every version a bucket holds, current or not.
func everyVersion(t *testing.T, f *fsm, name string) []versionRecord {
	t.Helper()
	var out []versionRecord
	if err := f.view(func(tx *bbolt.Tx) error {
		for _, parent := range [][]byte{objectsBucket(), versionsBucket()} {
			index := tx.Bucket(parent).Bucket([]byte(name))
			if index == nil {
				continue
			}
			if err := index.ForEach(func(_, raw []byte) error {
				var v versionRecord
				if err := json.Unmarshal(raw, &v); err != nil {
					return err
				}
				out = append(out, v)
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// checkUsage holds the usage record of a bucket to what the state holds:
// when it is exact its totals are a recount, what a walk has counted is a
// recount of what it has been past, a first count shows nothing, the greatest
// sequence is not below that of any version an exact record covers, and the
// stored bytes are the domain's own.
func checkUsage(t *testing.T, f *fsm, name string) bucket.Usage {
	t.Helper()
	usage, kept, _ := usageOf(t, f, name)
	if !kept {
		return usage
	}
	raw := usageBytes(t, f, name)
	if again, err := usage.Record(); err != nil || !bytes.Equal(again, raw) {
		t.Fatalf("the stored usage of %s is not the canonical record: %s, %v", name, raw, err)
	}
	all, high := counted(t, f, name, nil)
	switch {
	case usage.Exact && usage.UsageTotals != all:
		t.Fatalf("the usage of %s is %+v, a recount finds %+v", name, usage.UsageTotals, all)
	case !usage.Exact && usage.UsageTotals != (bucket.UsageTotals{}):
		t.Fatalf("the usage of %s shows %+v while it is counted for the first time", name, usage.UsageTotals)
	case usage.Exact && usage.High < high:
		t.Fatalf("the greatest sequence of %s is %d, it holds a version at %d", name, usage.High, high)
	}
	if usage.Count != nil {
		if past, _ := counted(t, f, name, usage.Count); usage.Count.UsageTotals != past {
			t.Fatalf("the walk through %s, in phase %d at %q, has counted %+v; behind it lies %+v",
				name, usage.Count.Phase, usage.Count.Cursor, usage.Count.UsageTotals, past)
		}
	}
	return usage
}

// walk applies one entry of the walk through a bucket, which must not be
// refused, and returns the record after it and whether it found drift.
func (m *machine) walk(name string, limit int, restart bool) (bucket.Usage, bool) {
	m.t.Helper()
	r := m.ok5(entry{Type: cmdWalkBucketUsage, WalkBucketUsage: &usageWalkCmd{Bucket: name, Limit: limit, Restart: restart}})
	usage, err := bucket.ParseUsage(r.usage)
	if err != nil {
		m.t.Fatalf("the answer of a walk: %s: %v", r.usage, err)
	}
	if stored := usageBytes(m.t, m.f, name); !bytes.Equal(stored, r.usage) {
		m.t.Fatalf("a walk answers %s and stores %s", r.usage, stored)
	}
	return usage, r.drift
}

// sized is a version of so many bytes, told from every other by serial.
func sized(bucketName, key string, size int64, serial int) *versionRecord {
	v := porttest.SampleVersion(bucketName, key, 1)
	sum := sha256.Sum256(fmt.Appendf(nil, "sized/%s/%s/%d", bucketName, key, serial))
	v.VersionID, v.DataID, v.Size = object.ID(sum[:object.IDSize]), object.ID(sum[object.IDSize:]), size
	return versionToRecord(v)
}

func putEntry(bucketName, key string, size int64, serial int) entry {
	return entry{Type: cmdPutObject, PutObject: sized(bucketName, key, size, serial)}
}

func serialID(serial int) object.ID {
	sum := sha256.Sum256(fmt.Appendf(nil, "serial/%d", serial))
	return object.ID(sum[:object.IDSize])
}

func deleteEntry(bucketName, key, ref string, serial int) entry {
	return entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{
		Bucket: bucketName, Key: key, VersionRef: ref, MarkerID: serialID(serial).String(), At: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC),
	}}
}

// partsOfUpload are entries that begin an upload and put parts of the
// given sizes, numbered from 1.
func uploadEntries(bucketName, key string, serial int, sizes ...int64) (object.Upload, []object.Part, []entry) {
	u := porttest.SampleUpload(bucketName, key, 1)
	u.ID = serialID(serial)
	entries := []entry{{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)}}
	parts := make([]object.Part, len(sizes))
	for i, size := range sizes {
		parts[i] = porttest.SamplePart(u, i+1, size, serialID(serial)[0])
		entries = append(entries, partEntry(u, parts[i]))
	}
	return u, parts, entries
}

func partEntry(u object.Upload, p object.Part) entry {
	return entry{Type: cmdPutPart, PutPart: &putPartCmd{Bucket: u.Bucket, Key: u.Key, UploadID: u.ID.String(), Part: partToRecord(p)}}
}

func completeEntry(u object.Upload, serial int, parts ...object.Part) entry {
	cmd := &completeUploadCmd{
		Bucket: u.Bucket, Key: u.Key, UploadID: u.ID.String(), VersionID: serialID(serial).String(), ETag: "etag", CreatedBy: "root",
	}
	for _, p := range parts {
		cmd.Parts = append(cmd.Parts, listedPart{Number: p.Number, ETag: p.ETag, Checksums: p.Checksums, DataID: p.DataID.String(), Seal: bytes.Repeat([]byte{1}, sha256.Size)})
	}
	return entry{Type: cmdCompleteUpload, CompleteUpload: cmd}
}

func abortEntry(u object.Upload) entry {
	return entry{Type: cmdAbortUpload, AbortUpload: &uploadRefCmd{Bucket: u.Bucket, Key: u.Key, UploadID: u.ID.String()}}
}

// bucket5 makes a bucket as a cluster at level 5 does: with a usage record,
// exact and empty.
func (m *machine) bucket5(name string, versioning bucket.Versioning) {
	m.t.Helper()
	m.ok5(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: name, CreatedAt: m.at, CreatedBy: "root"}})
	if versioning != bucket.Unversioned {
		m.ok5(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: name, Status: string(bucket.VersioningEnabled)}})
	}
	if versioning == bucket.VersioningSuspended {
		m.ok5(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: name, Status: string(versioning)}})
	}
}

// usageGolden makes the three stored records the golden files pin, by fixed
// sequences of entries: of a bucket that is exact, of one that is counted
// for the first time, and of one that is counted again.
func usageGolden(t *testing.T) map[string][]byte {
	t.Helper()
	m := newMachine5(t)
	m.bucket5("photos", bucket.VersioningEnabled)
	m.ok5(putEntry("photos", "a", 1000, 1))
	m.ok5(putEntry("photos", "a", 2000, 2))
	m.ok5(putEntry("photos", "zoë/π.txt", 3000, 3))
	m.ok5(deleteEntry("photos", "a", "", 4))
	_, _, upload := uploadEntries("photos", "large.bin", 5, 10, 7)
	for _, e := range upload {
		m.ok5(e)
	}
	out := map[string][]byte{"bucket-usage-record.golden.json": usageBytes(t, m.f, "photos")}

	// A bucket of before schema 5, and one entry of the walk through it: a
	// cursor that JSON does not write as it is.
	m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "old", CreatedAt: m.at}})
	for i, key := range []string{"a \"quoted\"\tkey <&>", "b", "c"} {
		held, schema := written(putEntry("old", key, int64(100*(i+1)), 10+i), entrySchema4)
		if r := m.as(schema, held); r.err != nil {
			t.Fatal(r.err)
		}
	}
	m.walk("old", 1, false)
	out["bucket-usage-record-counting.golden.json"] = usageBytes(t, m.f, "old")

	// The first bucket counted again: one step, which ends behind a key of
	// three versions.
	m.walk("photos", 3, true)
	out["bucket-usage-record-recount.golden.json"] = usageBytes(t, m.f, "photos")
	return out
}

// TestTheStoredUsageRecordIsPinned: the record of a bucket's usage after a
// fixed sequence of entries, byte for byte - exact, being counted for the
// first time, and being counted again. The bytes are the domain's
// (bucket.Usage.Record): what is stored is read by bucket.ParseUsage, which
// takes no others, and written again as the same bytes. A change that makes
// this test fail makes two releases store other bytes: once schema 5 is
// announced it needs a rise of the entry schema, not a new file.
func TestTheStoredUsageRecordIsPinned(t *testing.T) {
	t.Parallel()
	got := usageGolden(t)
	if len(got) != 3 {
		t.Fatalf("%d records", len(got))
	}
	for name, record := range got {
		want, err := fs.ReadFile(os.DirFS("testdata"), name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(append(bytes.Clone(record), '\n'), want) {
			t.Errorf("%s: the stored record is not the one that is pinned:\n got %s\nwant %s", name, record, want)
		}
		usage, err := bucket.ParseUsage(record)
		if err != nil {
			t.Errorf("%s: the stored record is not one the domain reads: %v", name, err)
			continue
		}
		if again, err := usage.Record(); err != nil || !bytes.Equal(again, record) {
			t.Errorf("%s: the domain writes the record as %s, %v", name, again, err)
		}
	}
	// In words, so that the files can be read against what made them.
	exact, _ := bucket.ParseUsage(got["bucket-usage-record.golden.json"])
	if want := (bucket.UsageTotals{Objects: 1, CurrentBytes: 3000, Versions: 4, StoredBytes: 6000, UploadBytes: 17}); !exact.Exact || exact.Walking() || exact.UsageTotals != want {
		t.Errorf("the exact record is %+v, want %+v", exact, want)
	}
	counting, _ := bucket.ParseUsage(got["bucket-usage-record-counting.golden.json"])
	if counting.Exact || counting.UsageTotals != (bucket.UsageTotals{}) || counting.Count == nil || counting.Count.Phase != bucket.UsagePhaseKeys ||
		counting.Count.Cursor != "a \"quoted\"\tkey <&>" || counting.Count.UsageTotals != (bucket.UsageTotals{Objects: 1, CurrentBytes: 100, Versions: 1, StoredBytes: 100}) {
		t.Errorf("the record of a first count is %+v, count %+v", counting, counting.Count)
	}
	recount, _ := bucket.ParseUsage(got["bucket-usage-record-recount.golden.json"])
	if !recount.Exact || recount.UsageTotals != exact.UsageTotals || recount.Count == nil || recount.Count.Cursor != "a" ||
		recount.Count.UsageTotals != (bucket.UsageTotals{Versions: 3, StoredBytes: 3000}) {
		t.Errorf("the record of a recount is %+v, count %+v", recount, recount.Count)
	}
}

// TestEveryChangeOfABucketKeepsItsUsage: every function of the state
// machine that changes the versions of a key or the parts of an upload keeps
// the usage record of the bucket - in a bucket without versions, one that
// keeps them and one where they are suspended; whatever the schema byte of
// the entry; and what changes neither a size nor a count, or is refused,
// leaves the record as it was. After every entry the record is what a recount
// of the state finds.
func TestEveryChangeOfABucketKeepsItsUsage(t *testing.T) {
	t.Parallel()
	type totals = bucket.UsageTotals
	type step struct {
		name string
		// schema is the schema the entry is written in; 0 for the one a
		// cluster at level 5 writes it in.
		schema  byte
		entry   func() entry
		refused error
		want    totals
	}
	five := int64(object.MinPartSize)
	tests := map[string]struct {
		versioning bucket.Versioning
		steps      func(u1, u2 object.Upload, p1, p2 []object.Part, ids map[string]string) []step
	}{
		"a bucket without versions": {versioning: bucket.Unversioned, steps: func(u1, u2 object.Upload, p1, p2 []object.Part, _ map[string]string) []step {
			again := porttest.SamplePart(u1, 1, 25, 99)
			return []step{
				{name: "a put", entry: func() entry { return putEntry("b", "k", 1000, 1) }, want: totals{Objects: 1, CurrentBytes: 1000, Versions: 1, StoredBytes: 1000}},
				{name: "an overwrite, which is its net", entry: func() entry { return putEntry("b", "k", 3000, 2) }, want: totals{Objects: 1, CurrentBytes: 3000, Versions: 1, StoredBytes: 3000}},
				{name: "an upload begun, which holds nothing", entry: func() entry { return entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u1)} }, want: totals{Objects: 1, CurrentBytes: 3000, Versions: 1, StoredBytes: 3000}},
				{name: "a part", entry: func() entry { return partEntry(u1, p1[0]) }, want: totals{Objects: 1, CurrentBytes: 3000, Versions: 1, StoredBytes: 3000, UploadBytes: 10}},
				{name: "the part put again", entry: func() entry { return partEntry(u1, again) }, want: totals{Objects: 1, CurrentBytes: 3000, Versions: 1, StoredBytes: 3000, UploadBytes: 25}},
				{name: "another part, by a proposer of level 4", schema: entrySchemaTimed, entry: func() entry { return partEntry(u1, p1[1]) }, want: totals{Objects: 1, CurrentBytes: 3000, Versions: 1, StoredBytes: 3000, UploadBytes: 32}},
				{name: "a completion that lists one part and replaces the version", entry: func() entry { return completeEntry(u1, 3, p1[1]) }, want: totals{Objects: 1, CurrentBytes: 7, Versions: 1, StoredBytes: 7}},
				{name: "a second upload with a part", entry: func() entry { return entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u2)} }, want: totals{Objects: 1, CurrentBytes: 7, Versions: 1, StoredBytes: 7}},
				{name: "its part", entry: func() entry { return partEntry(u2, p2[0]) }, want: totals{Objects: 1, CurrentBytes: 7, Versions: 1, StoredBytes: 7, UploadBytes: 5}},
				{name: "its abort", entry: func() entry { return abortEntry(u2) }, want: totals{Objects: 1, CurrentBytes: 7, Versions: 1, StoredBytes: 7}},
				{name: "an abort of an upload that is gone", entry: func() entry { return abortEntry(u2) }, refused: domain.ErrUploadNotFound, want: totals{Objects: 1, CurrentBytes: 7, Versions: 1, StoredBytes: 7}},
				{name: "tags", entry: func() entry {
					return entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: "b", Key: "k", VersionRef: object.NullVersion, Tags: map[string]string{"a": "b"}}}
				}, want: totals{Objects: 1, CurrentBytes: 7, Versions: 1, StoredBytes: 7}},
				{name: "a delete", entry: func() entry { return deleteEntry("b", "k", "", 4) }, want: totals{}},
				{name: "a delete of nothing", entry: func() entry { return deleteEntry("b", "k", "", 5) }, want: totals{}},
				{name: "a put of the first schema", schema: entrySchemaVersion, entry: func() entry { return putEntry("b", "j", 2000, 6) }, want: totals{Objects: 1, CurrentBytes: 2000, Versions: 1, StoredBytes: 2000}},
				{name: "a put of schema 4", schema: entrySchema4, entry: func() entry { return putEntry("b", "i", 1, 7) }, want: totals{Objects: 2, CurrentBytes: 2001, Versions: 2, StoredBytes: 2001}},
				{name: "the delete of the first schema", schema: entrySchemaVersion, entry: func() entry {
					return entry{Type: cmdDeleteObject, DeleteObject: &deleteObjectCmd{Bucket: "b", Key: "j"}}
				}, want: totals{Objects: 1, CurrentBytes: 1, Versions: 1, StoredBytes: 1}},
				{name: "the null version removed by its name", entry: func() entry { return deleteEntry("b", "i", object.NullVersion, 8) }, want: totals{}},
			}
		}},
		"a bucket that keeps versions": {versioning: bucket.VersioningEnabled, steps: func(u1, _ object.Upload, p1, _ []object.Part, ids map[string]string) []step {
			first, second := sized("b", "a", 1000, 1), sized("b", "a", 2000, 2)
			ids["marker"] = serialID(3).String()
			return []step{
				{name: "a put", entry: func() entry { return entry{Type: cmdPutObject, PutObject: first} }, want: totals{Objects: 1, CurrentBytes: 1000, Versions: 1, StoredBytes: 1000}},
				{name: "a second version", entry: func() entry { return entry{Type: cmdPutObject, PutObject: second} }, want: totals{Objects: 1, CurrentBytes: 2000, Versions: 2, StoredBytes: 3000}},
				{name: "a delete, which is a marker", entry: func() entry { return deleteEntry("b", "a", "", 3) }, want: totals{Versions: 3, StoredBytes: 3000}},
				{name: "the marker removed: the version before it is current again", entry: func() entry { return deleteEntry("b", "a", ids["marker"], 4) }, want: totals{Objects: 1, CurrentBytes: 2000, Versions: 2, StoredBytes: 3000}},
				{name: "a version that is not there", entry: func() entry { return deleteEntry("b", "a", serialID(77).String(), 5) }, want: totals{Objects: 1, CurrentBytes: 2000, Versions: 2, StoredBytes: 3000}},
				{name: "the current version removed: the older one is current", entry: func() entry { return deleteEntry("b", "a", second.VersionID, 6) }, want: totals{Objects: 1, CurrentBytes: 1000, Versions: 1, StoredBytes: 1000}},
				{name: "an upload of two parts", entry: func() entry { return entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u1)} }, want: totals{Objects: 1, CurrentBytes: 1000, Versions: 1, StoredBytes: 1000}},
				{name: "its first part", entry: func() entry { return partEntry(u1, p1[0]) }, want: totals{Objects: 1, CurrentBytes: 1000, Versions: 1, StoredBytes: 1000, UploadBytes: five}},
				{name: "its second", entry: func() entry { return partEntry(u1, p1[1]) }, want: totals{Objects: 1, CurrentBytes: 1000, Versions: 1, StoredBytes: 1000, UploadBytes: five + 7}},
				{name: "its completion: a new version of another key", entry: func() entry { return completeEntry(u1, 7, p1...) }, want: totals{Objects: 2, CurrentBytes: 1000 + five + 7, Versions: 2, StoredBytes: 1000 + five + 7}},
				{name: "a legal hold where there is no Object Lock", entry: func() entry {
					return entry{Type: cmdSetLegalHold, SetLegalHold: &legalHoldCmd{Bucket: "b", Key: "a", VersionRef: first.VersionID, On: true}}
				}, refused: domain.ErrLockNotConfigured, want: totals{Objects: 2, CurrentBytes: 1000 + five + 7, Versions: 2, StoredBytes: 1000 + five + 7}},
				{name: "the version of parts removed", entry: func() entry { return deleteEntry("b", u1.Key, serialID(7).String(), 8) }, want: totals{Objects: 1, CurrentBytes: 1000, Versions: 1, StoredBytes: 1000}},
				{name: "the last version removed", entry: func() entry { return deleteEntry("b", "a", first.VersionID, 9) }, want: totals{}},
			}
		}},
		"a bucket whose versions are suspended": {versioning: bucket.VersioningSuspended, steps: func(_, _ object.Upload, _, _ []object.Part, _ map[string]string) []step {
			return []step{
				{name: "a put, which is the null version", entry: func() entry { return putEntry("b", "a", 500, 1) }, want: totals{Objects: 1, CurrentBytes: 500, Versions: 1, StoredBytes: 500}},
				{name: "versions kept again", entry: func() entry {
					return entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "b", Status: string(bucket.VersioningEnabled)}}
				}, want: totals{Objects: 1, CurrentBytes: 500, Versions: 1, StoredBytes: 500}},
				{name: "a version beside the null version", entry: func() entry { return putEntry("b", "a", 1000, 2) }, want: totals{Objects: 1, CurrentBytes: 1000, Versions: 2, StoredBytes: 1500}},
				{name: "suspended again", entry: func() entry {
					return entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "b", Status: string(bucket.VersioningSuspended)}}
				}, want: totals{Objects: 1, CurrentBytes: 1000, Versions: 2, StoredBytes: 1500}},
				{name: "a put that replaces the null version, which was not current", entry: func() entry { return putEntry("b", "a", 700, 3) }, want: totals{Objects: 1, CurrentBytes: 700, Versions: 2, StoredBytes: 1700}},
				{name: "a delete: a null marker in place of the null version", entry: func() entry { return deleteEntry("b", "a", "", 4) }, want: totals{Versions: 2, StoredBytes: 1000}},
				{name: "a delete again: a marker in place of the marker", entry: func() entry { return deleteEntry("b", "a", "", 5) }, want: totals{Versions: 2, StoredBytes: 1000}},
				{name: "a put over the marker", entry: func() entry { return putEntry("b", "a", 9, 6) }, want: totals{Objects: 1, CurrentBytes: 9, Versions: 2, StoredBytes: 1009}},
			}
		}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMachine5(t)
			m.bucket5("b", test.versioning)
			u1, p1, _ := uploadEntries("b", "k", 50, 10, 7)
			if test.versioning == bucket.VersioningEnabled {
				u1, p1, _ = uploadEntries("b", "large", 50, int64(object.MinPartSize), 7)
			}
			u2, p2, _ := uploadEntries("b", "k", 51, 5)
			for _, s := range test.steps(u1, u2, p1, p2, map[string]string{}) {
				held, schema := written(s.entry(), entrySchema5)
				if s.schema != 0 {
					held, schema = heldTo(s.entry(), s.schema), s.schema
				}
				before := usageBytes(t, m.f, "b")
				r := m.as(schema, held)
				if !errors.Is(r.err, s.refused) {
					t.Fatalf("%s = %v, want %v", s.name, r.err, s.refused)
				}
				usage := checkUsage(t, m.f, "b")
				if !usage.Exact || usage.Walking() || usage.UsageTotals != s.want {
					t.Fatalf("after %s the bucket holds %+v (walking %t), want %+v", s.name, usage.UsageTotals, usage.Walking(), s.want)
				}
				if s.refused != nil && !bytes.Equal(usageBytes(t, m.f, "b"), before) {
					t.Errorf("%s was refused and changed the record", s.name)
				}
			}
			if m.f.usageClamped.Load() != 0 || m.f.usageDrift.Load() != 0 {
				t.Errorf("changes of a real bucket were held at a bound %d times, and drift was found %d times", m.f.usageClamped.Load(), m.f.usageDrift.Load())
			}
			// The record goes with the bucket, and a bucket made under the
			// name by a proposer of level 4 has none.
			for serial, v := range everyVersion(t, m.f, "b") {
				m.ok5(deleteEntry("b", v.Key, refOf(v), 200+serial))
			}
			if usage := checkUsage(t, m.f, "b"); usage.UsageTotals != (bucket.UsageTotals{}) {
				t.Fatalf("the emptied bucket holds %+v", usage.UsageTotals)
			}
			m.ok5(entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "b"}})
			if raw := usageBytes(t, m.f, "b"); raw != nil {
				t.Errorf("the usage record outlives its bucket: %s", raw)
			}
			m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedAt: m.at}})
			held, schema := written(putEntry("b", "k", 10, 100), entrySchema4)
			if r := m.as(schema, held); r.err != nil {
				t.Fatal(r.err)
			}
			if raw := usageBytes(t, m.f, "b"); raw != nil {
				t.Errorf("a bucket made by an entry of schema 4 has a usage record: %s", raw)
			}
		})
	}
}

// TestTheWalkCountsABucketThatWasThere: a bucket of before schema 5 has no
// record until the first entry of the walk, which makes one in counting
// state. The walk visits whole keys until it has visited as many records as
// its limit, then the uploads; while the bucket is counted for the first time
// its totals are zero; changes behind the walk are counted and those before
// it are left to it; and the entry that ends the uploads makes the record
// exact, with what a recount of the state finds. An exact record is left
// alone by a walk without restart, counted again with it, and a recount that
// finds what was kept is no drift.
func TestTheWalkCountsABucketThatWasThere(t *testing.T) {
	t.Parallel()
	m := newMachine5(t)
	old := func(e entry) applyResult {
		t.Helper()
		held, schema := written(e, entrySchema4)
		r := m.as(schema, held)
		if r.err != nil {
			t.Fatalf("%s: %v", e.Type, r.err)
		}
		return r
	}
	old(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "old", CreatedAt: m.at}})
	old(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "old", Status: string(bucket.VersioningEnabled)}})
	// a: three versions; b: a version under a marker; c: one version;
	// d: nothing but a marker; e: one version. Two uploads, one without a
	// part.
	old(putEntry("old", "a", 100, 1))
	old(putEntry("old", "a", 200, 2))
	old(putEntry("old", "a", 300, 3))
	old(putEntry("old", "b", 1000, 4))
	old(deleteEntry("old", "b", "", 5))
	old(putEntry("old", "c", 10, 6))
	old(deleteEntry("old", "d", "", 7))
	old(putEntry("old", "e", 1, 8))
	u1, _, entries := uploadEntries("old", "up", 20, 40, 2)
	for _, e := range entries {
		old(e)
	}
	u2, _, entries := uploadEntries("old", "up", 21)
	for _, e := range entries {
		old(e)
	}
	if raw := usageBytes(t, m.f, "old"); raw != nil || stateSchema(t, m.f) != stateSchema4 {
		t.Fatalf("a bucket of before schema 5 has the record %s in a state of schema %d", raw, stateSchema(t, m.f))
	}
	first := min(u1.ID.String(), u2.ID.String())
	whole := bucket.UsageTotals{Objects: 3, CurrentBytes: 311, Versions: 8, StoredBytes: 1611, UploadBytes: 42}
	bytesOf := map[string]int64{u1.ID.String(): 42, u2.ID.String(): 0}

	// What is refused, and makes no record.
	for name, test := range map[string]struct {
		cmd  usageWalkCmd
		want error
	}{
		"a limit of nothing":         {usageWalkCmd{Bucket: "old", Limit: 0}, domain.ErrInvalidArgument},
		"a limit below nothing":      {usageWalkCmd{Bucket: "old", Limit: -1}, domain.ErrInvalidArgument},
		"a limit beyond the bound":   {usageWalkCmd{Bucket: "old", Limit: ports.MaxUsageWalk + 1, Restart: true}, domain.ErrInvalidArgument},
		"a bucket that is not there": {usageWalkCmd{Bucket: "missing", Limit: 1}, domain.ErrBucketNotFound},
	} {
		if r := m.at5(entry{Type: cmdWalkBucketUsage, WalkBucketUsage: &test.cmd}); !errors.Is(r.err, test.want) || r.usage != nil {
			t.Errorf("%s = %v, %s; want %v", name, r.err, r.usage, test.want)
		}
	}
	if raw := usageBytes(t, m.f, "old"); raw != nil {
		t.Fatalf("a walk that was refused made the record %s", raw)
	}

	type place struct {
		phase  bucket.UsagePhase
		cursor string
		count  bucket.UsageTotals
	}
	steps := []struct {
		limit int
		want  place
	}{
		// One record is asked for and the key has three: the whole key.
		{1, place{bucket.UsagePhaseKeys, "a", bucket.UsageTotals{Objects: 1, CurrentBytes: 300, Versions: 3, StoredBytes: 600}}},
		// Three records: b has two, and c is begun because one is left.
		{3, place{bucket.UsagePhaseKeys, "c", bucket.UsageTotals{Objects: 2, CurrentBytes: 310, Versions: 6, StoredBytes: 1610}}},
		// The keys end with the limit not used up: the uploads are begun,
		// and the first of them is its two parts.
		{3, place{bucket.UsagePhaseUploads, first, bucket.UsageTotals{Objects: 3, CurrentBytes: 311, Versions: 8, StoredBytes: 1611, UploadBytes: bytesOf[first]}}},
	}
	for i, step := range steps {
		usage, drift := m.walk("old", step.limit, false)
		if drift || usage.Exact || usage.UsageTotals != (bucket.UsageTotals{}) || usage.Count == nil {
			t.Fatalf("step %d: %+v, drift %t", i, usage, drift)
		}
		if got := (place{usage.Count.Phase, usage.Count.Cursor, usage.Count.UsageTotals}); got != step.want {
			t.Fatalf("step %d: the walk is at %+v, want %+v", i, got, step.want)
		}
		checkUsage(t, m.f, "old")
	}
	if got := stateSchema(t, m.f); got != stateSchema5 {
		t.Errorf("state schema %d after a walk", got)
	}
	// A quota cannot be held to a count that is not known.
	if r := m.at5(entry{Type: cmdSetBucketQuota, SetBucketQuota: &bucketQuotaCmd{Bucket: "old", Bytes: 1}}); !errors.Is(r.err, domain.ErrConflict) {
		t.Errorf("a quota for a bucket that is being counted = %v", r.err)
	}
	// Changes while it is counted: of a key (all keys are behind the walk
	// now) and of the two uploads, one behind it and one before.
	m.ok5(putEntry("old", "c", 25, 30))
	newest := m.index
	whole.Objects, whole.CurrentBytes, whole.Versions, whole.StoredBytes = 3, 326, 9, 1636
	for _, u := range []object.Upload{u1, u2} {
		m.ok5(partEntry(u, porttest.SamplePart(u, 9, 5, 31)))
		bytesOf[u.ID.String()] += 5
	}
	whole.UploadBytes = 52
	usage := checkUsage(t, m.f, "old")
	if want := (bucket.UsageTotals{Objects: 3, CurrentBytes: 326, Versions: 9, StoredBytes: 1636, UploadBytes: bytesOf[first]}); usage.Exact || usage.Count.UsageTotals != want {
		t.Fatalf("after changes behind and before the walk it has counted %+v, want %+v", usage.Count.UsageTotals, want)
	}
	// The last entry: the second upload, and the end.
	usage, drift := m.walk("old", 1, false)
	if drift || !usage.Exact || usage.Walking() || usage.UsageTotals != whole {
		t.Fatalf("at the end of the first count: %+v, drift %t; want %+v", usage, drift, whole)
	}
	if usage.High != newest {
		t.Errorf("the greatest sequence is %d, the newest version was made by entry %d", usage.High, newest)
	}
	checkUsage(t, m.f, "old")

	// Exact and not being counted: an entry without restart changes
	// nothing, and says what the record is.
	before := wholeState(t, m.f)
	if again, drift := m.walk("old", 100, false); drift || !reflect.DeepEqual(again, usage) {
		t.Errorf("a walk of an exact record = %+v, drift %t", again, drift)
	}
	after := wholeState(t, m.f)
	for _, key := range [][]byte{appliedKey(), timeKey()} {
		delete(before, fmt.Sprintf("meta/%x", key))
		delete(after, fmt.Sprintf("meta/%x", key))
	}
	if !reflect.DeepEqual(after, before) {
		t.Errorf("a walk of an exact record changed the state")
	}

	// A recount: the totals go on being kept, a walk under way is begun
	// again by another restart, and the end finds what was kept.
	recount, _ := m.walk("old", 4, true)
	if !recount.Exact || recount.UsageTotals != whole || recount.Count == nil || recount.Count.Cursor != "b" {
		t.Fatalf("the first entry of a recount: %+v, count %+v", recount, recount.Count)
	}
	m.ok5(deleteEntry("old", "a", "", 40))
	m.ok5(putEntry("old", "z", 7, 41))
	whole.Objects, whole.CurrentBytes, whole.Versions, whole.StoredBytes = 3, 33, 11, 1643
	if got := checkUsage(t, m.f, "old"); got.UsageTotals != whole || got.Count.Versions != 6 {
		t.Fatalf("during a recount the bucket holds %+v and the walk has counted %+v", got.UsageTotals, got.Count.UsageTotals)
	}
	again, _ := m.walk("old", 1, true)
	if again.Count == nil || again.Count.Cursor != "a" || again.Count.Versions != 4 || again.UsageTotals != whole {
		t.Fatalf("a recount begun again: %+v, count %+v", again, again.Count)
	}
	for range 20 {
		usage, drift = m.walk("old", 2, false)
		checkUsage(t, m.f, "old")
		if !usage.Walking() {
			break
		}
	}
	if drift || usage.Walking() || !usage.Exact || usage.UsageTotals != whole {
		t.Fatalf("at the end of a recount: %+v, drift %t; want %+v", usage, drift, whole)
	}
	if m.f.usageDrift.Load() != 0 || m.f.usageClamped.Load() != 0 {
		t.Errorf("drift %d, clamped %d", m.f.usageDrift.Load(), m.f.usageClamped.Load())
	}

	// An empty bucket of before is counted by one entry.
	old(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "empty", CreatedAt: m.at}})
	if usage, drift := m.walk("empty", 1, false); drift || !usage.Exact || usage.Walking() || usage.UsageTotals != (bucket.UsageTotals{}) {
		t.Errorf("an empty bucket after one entry: %+v", usage)
	}
}

// TestARecountThatFindsOtherTotalsIsDrift: a record that does not hold what
// the bucket holds - which only a fault of this code can leave - is put
// right by a recount, and the entry that ends it says so. A first count
// cannot drift: there was nothing to differ from.
func TestARecountThatFindsOtherTotalsIsDrift(t *testing.T) {
	t.Parallel()
	m := newMachine5(t)
	m.bucket5("b", bucket.Unversioned)
	m.ok5(putEntry("b", "k", 1000, 1))
	wrong := checkUsage(t, m.f, "b")
	right := wrong.UsageTotals
	wrong.StoredBytes, wrong.Objects = 5, 9
	setUsage(t, m.f, "b", wrong)
	usage, drift := m.walk("b", 100, true)
	if !drift || usage.Walking() || !usage.Exact || usage.UsageTotals != right {
		t.Fatalf("a recount of a record that was wrong: %+v, drift %t; want %+v", usage, drift, right)
	}
	if got := m.f.usageDrift.Load(); got != 1 {
		t.Errorf("drift was counted %d times", got)
	}
	// The answer travels to the node that proposed.
	encoded, err := encodeResult(applyResult{usage: usageBytes(t, m.f, "b"), drift: true})
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := decodeResult(encoded); err != nil || !decoded.drift || !bytes.Equal(decoded.usage, usageBytes(t, m.f, "b")) {
		t.Errorf("the answer of a walk on the far side = %+v, %v", decoded, err)
	}
	if usage, drift := m.walk("b", 100, true); drift || usage.UsageTotals != right {
		t.Errorf("a second recount: %+v, drift %t", usage, drift)
	}
	// An entry applied twice - replayed after a restart - is counted once.
	if r := result(t, m.f.Apply(&raft.Log{Index: m.index, Type: raft.LogCommand, Data: []byte{entrySchema5}})); r.err != nil || m.f.usageDrift.Load() != 1 {
		t.Errorf("an entry that was applied before = %v; drift counted %d times", r.err, m.f.usageDrift.Load())
	}
}

// TestAClampBeginsARecount: a change that would take a count below nothing
// leaves it at nothing, on every member alike - and because no change of a
// real bucket does that, the record was wrong before and is wrong now: the
// same entry begins a recount, the record stays exact meanwhile, the clamp
// is counted, and the walk puts the totals right and reports the drift.
func TestAClampBeginsARecount(t *testing.T) {
	t.Parallel()
	m := newMachine5(t)
	m.bucket5("b", bucket.Unversioned)
	m.ok5(putEntry("b", "gone", 1000, 1))
	m.ok5(putEntry("b", "stays", 50, 2))
	wrong := checkUsage(t, m.f, "b")
	wrong.StoredBytes, wrong.CurrentBytes = 10, 10
	setUsage(t, m.f, "b", wrong)
	m.ok5(deleteEntry("b", "gone", "", 3))
	if got := m.f.usageClamped.Load(); got != 1 {
		t.Fatalf("the clamp was counted %d times", got)
	}
	usage, _, _ := usageOf(t, m.f, "b")
	if !usage.Exact || usage.StoredBytes != 0 || usage.CurrentBytes != 0 || usage.Objects != 1 || usage.Count == nil ||
		usage.Count.Phase != bucket.UsagePhaseKeys || usage.Count.Cursor != "" || usage.Count.UsageTotals != (bucket.UsageTotals{}) {
		t.Fatalf("after a clamp the record is %+v, count %+v", usage, usage.Count)
	}
	// A quota goes on being judged by the clamped figure until the walk
	// ends: here it admits more than it should.
	m.ok5(entry{Type: cmdSetBucketQuota, SetBucketQuota: &bucketQuotaCmd{Bucket: "b", Bytes: 60}})
	m.ok5(putEntry("b", "more", 60, 4))
	usage, drift := m.walk("b", 100, false)
	if !drift || usage.Walking() || usage.StoredBytes != 110 || usage.Objects != 2 {
		t.Fatalf("the recount a clamp began: %+v, drift %t", usage, drift)
	}
	checkUsage(t, m.f, "b")
	if r := m.at5(putEntry("b", "over", 1, 5)); !errors.Is(r.err, domain.ErrQuotaExceeded) {
		t.Errorf("a put into a bucket over its quota, once it is counted = %v", r.err)
	}
	if m.f.usageClamped.Load() != 1 || m.f.usageDrift.Load() != 1 {
		t.Errorf("clamped %d, drift %d", m.f.usageClamped.Load(), m.f.usageDrift.Load())
	}
}

// TestAQuotaIsHeldInApply: where entries are ordered there is one answer. A
// quota is set only on a bucket whose record is exact; an entry whose net
// would take the bucket past it is refused with ErrQuotaExceeded and changes
// nothing, whatever its schema byte; a change that adds nothing is admitted
// however full the bucket is; and nothing that removes, no marker and no
// completion is ever refused.
func TestAQuotaIsHeldInApply(t *testing.T) {
	t.Parallel()
	m := newMachine5(t)
	m.bucket5("b", bucket.Unversioned)
	m.bucket5("ver", bucket.VersioningEnabled)
	at := m.at
	record := auditToRecord(audit.Record{Time: at, Actor: "user/albie", Action: "admin:PutBucketQuota", Resource: "arn:aws:s3:::b", Outcome: audit.Succeeded})
	quota := func(name string, quotaBytes int64, with *auditRecord) applyResult {
		return m.at5(entry{Type: cmdSetBucketQuota, SetBucketQuota: &bucketQuotaCmd{Bucket: name, Bytes: quotaBytes, Audit: with}})
	}
	audits := func() int {
		n := 0
		_ = m.f.view(func(tx *bbolt.Tx) error {
			n = tx.Bucket(auditBucket()).Stats().KeyN
			return nil
		})
		return n
	}
	for name, test := range map[string]struct {
		bucket string
		bytes  int64
		want   error
	}{
		"a bucket that is not there": {"missing", 10, domain.ErrBucketNotFound},
		"below nothing":              {"b", -1, domain.ErrInvalidArgument},
		"beyond the greatest":        {"b", bucket.MaxQuotaBytes + 1, domain.ErrInvalidArgument},
	} {
		if r := quota(test.bucket, test.bytes, record); !errors.Is(r.err, test.want) || len(r.events) != 0 {
			t.Errorf("%s = %v, want %v", name, r.err, test.want)
		}
	}
	if audits() != 0 {
		t.Errorf("a quota that was refused left %d audit records", audits())
	}
	r := quota("b", 1000, record)
	if r.err != nil || len(r.events) != 2 || r.events[0].Type != ports.EventBucketQuotaChanged || r.events[1].Type != ports.EventAuditRecorded {
		t.Fatalf("a quota = %v, events %+v", r.err, r.events)
	}
	if event, ok := r.events[0].Data.(ports.BucketQuotaEvent); !ok || event != (ports.BucketQuotaEvent{Bucket: "b", QuotaBytes: 1000}) {
		t.Errorf("the event of a quota is %+v", r.events[0].Data)
	}
	if _, _, got := usageOf(t, m.f, "b"); got != 1000 || audits() != 1 {
		t.Errorf("the quota is %d, with %d audit records", got, audits())
	}
	if got := string(storedBucket(t, m.f, "b")); !strings.HasSuffix(got, `"created_by":"root","quota_bytes":1000}`) {
		t.Errorf("the stored bucket is %s", got)
	}
	if r := quota("ver", bucket.MaxQuotaBytes, nil); r.err != nil || audits() != 1 {
		t.Errorf("the greatest quota, without an audit record = %v; %d records", r.err, audits())
	}

	// unchanged applies an entry that must be refused by the quota and
	// have changed nothing but what every entry leaves behind.
	unchanged := func(name string, schema byte, e entry) {
		t.Helper()
		before := wholeState(t, m.f)
		held, written := written(e, entrySchema5)
		if schema != 0 {
			held, written = heldTo(e, schema), schema
		}
		r := m.as(written, held)
		if !errors.Is(r.err, domain.ErrQuotaExceeded) || len(r.events) != 0 || r.stored != nil {
			t.Fatalf("%s = %v, want %v", name, r.err, domain.ErrQuotaExceeded)
		}
		// The refusal is a wire code on the far side of a node boundary.
		encoded, err := encodeResult(r)
		if err != nil {
			t.Fatal(err)
		}
		if decoded, err := decodeResult(encoded); err != nil || !errors.Is(decoded.err, domain.ErrQuotaExceeded) || !bytes.Contains(encoded, []byte(`"quota_exceeded"`)) {
			t.Errorf("%s on the far side = %v, %v: %s", name, decoded.err, err, encoded)
		}
		after := wholeState(t, m.f)
		for _, key := range [][]byte{appliedKey(), timeKey()} {
			delete(before, fmt.Sprintf("meta/%x", key))
			delete(after, fmt.Sprintf("meta/%x", key))
		}
		if !reflect.DeepEqual(after, before) {
			t.Errorf("%s was refused and changed the state", name)
		}
	}
	admitted := func(name string, e entry, want bucket.UsageTotals) {
		t.Helper()
		if r := m.at5(e); r.err != nil {
			t.Fatalf("%s = %v", name, r.err)
		}
		if usage := checkUsage(t, m.f, e.bucketName()); usage.UsageTotals != want {
			t.Fatalf("after %s the bucket holds %+v, want %+v", name, usage.UsageTotals, want)
		}
	}
	type totals = bucket.UsageTotals
	admitted("a put that fills the bucket to the byte", putEntry("b", "k", 1000, 1), totals{Objects: 1, CurrentBytes: 1000, Versions: 1, StoredBytes: 1000})
	unchanged("a put of one byte into a full bucket", 0, putEntry("b", "one", 1, 2))
	unchanged("a put of nothing more than fits, by a proposer of level 4", entrySchema4, putEntry("b", "one", 1, 3))
	unchanged("the same in an entry of the first schema", entrySchemaVersion, putEntry("b", "one", 1, 4))
	admitted("a put of no bytes into a full bucket", putEntry("b", "empty", 0, 5), totals{Objects: 2, CurrentBytes: 1000, Versions: 2, StoredBytes: 1000})
	admitted("an overwrite of the same size", putEntry("b", "k", 1000, 6), totals{Objects: 2, CurrentBytes: 1000, Versions: 2, StoredBytes: 1000})
	unchanged("an overwrite that is a byte larger", 0, putEntry("b", "k", 1001, 7))
	admitted("an overwrite that is smaller", putEntry("b", "k", 400, 8), totals{Objects: 2, CurrentBytes: 400, Versions: 2, StoredBytes: 400})
	// Parts count against it, each for its net.
	u, parts, entries := uploadEntries("b", "up", 20, 500)
	admitted("an upload begun", entries[0], totals{Objects: 2, CurrentBytes: 400, Versions: 2, StoredBytes: 400})
	admitted("a part within what is left", entries[1], totals{Objects: 2, CurrentBytes: 400, Versions: 2, StoredBytes: 400, UploadBytes: 500})
	unchanged("a second part past the quota", 0, partEntry(u, porttest.SamplePart(u, 2, 101, 21)))
	unchanged("the first part put again a hundred and one bytes larger", entrySchemaTimed, partEntry(u, porttest.SamplePart(u, 1, 601, 22)))
	admitted("the first part put again a hundred bytes larger", partEntry(u, porttest.SamplePart(u, 1, 600, 23)), totals{Objects: 2, CurrentBytes: 400, Versions: 2, StoredBytes: 400, UploadBytes: 600})
	parts[0] = porttest.SamplePart(u, 1, 600, 23)
	// A quota below what the bucket holds is accepted; nothing is removed,
	// and nothing more is taken.
	if r := quota("b", 100, nil); r.err != nil {
		t.Fatalf("a quota below what the bucket holds = %v", r.err)
	}
	unchanged("a put into a bucket over its quota", 0, putEntry("b", "one", 1, 9))
	unchanged("an overwrite that frees less than it adds", 0, putEntry("b", "empty", 1, 10))
	admitted("an overwrite that frees more than it adds, in a bucket over its quota", putEntry("b", "k", 399, 11), totals{Objects: 2, CurrentBytes: 399, Versions: 2, StoredBytes: 399, UploadBytes: 600})
	admitted("a completion in a bucket over its quota: it moves bytes that were counted", completeEntry(u, 12, parts...),
		totals{Objects: 3, CurrentBytes: 999, Versions: 3, StoredBytes: 999})
	admitted("a delete in a bucket over its quota", deleteEntry("b", "k", "", 13), totals{Objects: 2, CurrentBytes: 600, Versions: 2, StoredBytes: 600})
	u2, _, entries := uploadEntries("b", "up2", 24)
	admitted("an upload begun in a bucket over its quota", entries[0], totals{Objects: 2, CurrentBytes: 600, Versions: 2, StoredBytes: 600})
	unchanged("its first part", 0, partEntry(u2, porttest.SamplePart(u2, 1, 1, 25)))
	admitted("its abort", abortEntry(u2), totals{Objects: 2, CurrentBytes: 600, Versions: 2, StoredBytes: 600})
	// Removed, the quota holds nothing back.
	if r := quota("b", 0, record); r.err != nil || audits() != 2 {
		t.Fatalf("removing a quota = %v; %d audit records", r.err, audits())
	}
	if got := string(storedBucket(t, m.f, "b")); strings.Contains(got, "quota_bytes") {
		t.Errorf("a bucket without a quota is stored as %s", got)
	}
	admitted("a put once the quota is gone", putEntry("b", "one", 5000, 14), totals{Objects: 3, CurrentBytes: 5600, Versions: 3, StoredBytes: 5600})

	// Where versions are kept every put adds its size, and a delete that
	// makes a marker costs nothing.
	if r := quota("ver", 1500, nil); r.err != nil {
		t.Fatal(r.err)
	}
	admitted("a version", putEntry("ver", "a", 1000, 30), totals{Objects: 1, CurrentBytes: 1000, Versions: 1, StoredBytes: 1000})
	unchanged("a second version of the same key that does not fit", 0, putEntry("ver", "a", 501, 31))
	admitted("a marker in a bucket that is nearly full", deleteEntry("ver", "a", "", 32), totals{Versions: 2, StoredBytes: 1000})
	admitted("a second version that fits to the byte", putEntry("ver", "a", 500, 33), totals{Objects: 1, CurrentBytes: 500, Versions: 3, StoredBytes: 1500})
	unchanged("a version of one byte into a full bucket", 0, putEntry("ver", "b", 1, 34))
	admitted("a marker in a full bucket", deleteEntry("ver", "b", "", 35), totals{Objects: 1, CurrentBytes: 500, Versions: 4, StoredBytes: 1500})
	if m.f.usageClamped.Load() != 0 {
		t.Errorf("clamped %d times", m.f.usageClamped.Load())
	}

	// A refusal by the quota leaves nothing at all: not the place where the
	// parts of a bucket are kept, when the first part it is ever sent is
	// refused, and not the place of the versions of a bucket that keeps
	// them, when its first put is.
	m.bucket5("fresh", bucket.Unversioned)
	m.bucket5("freshkept", bucket.VersioningEnabled)
	m.bucket5("freshpaused", bucket.VersioningSuspended)
	for _, name := range []string{"fresh", "freshkept", "freshpaused"} {
		if r := quota(name, 10, nil); r.err != nil {
			t.Fatal(r.err)
		}
	}
	u3, _, entries := uploadEntries("fresh", "up", 40)
	m.ok5(entries[0])
	places := func() []string {
		return nestedBuckets(t, m.f, partsBucket(), versionsBucket(), versionIDsBucket())
	}
	for _, place := range places() {
		if strings.HasSuffix(place, "/fresh") || strings.HasSuffix(place, "/freshkept") || strings.HasSuffix(place, "/freshpaused") {
			t.Fatalf("%s is there before anything was stored in it", place)
		}
	}
	before := places()
	unchanged("the first part of a bucket", 0, partEntry(u3, porttest.SamplePart(u3, 1, 11, 41)))
	unchanged("the first part of a bucket, by a proposer of level 4", entrySchema4, partEntry(u3, porttest.SamplePart(u3, 1, 11, 42)))
	unchanged("the first put into a bucket that keeps versions", 0, putEntry("freshkept", "a", 11, 43))
	unchanged("the first put into a bucket that kept versions", entrySchemaTimed, putEntry("freshpaused", "a", 11, 44))
	if after := places(); !slices.Equal(after, before) {
		t.Errorf("refused by the quota, the first part and the first puts of their buckets left %v where there was %v", after, before)
	}
	admitted("the first part of a bucket that fits", partEntry(u3, porttest.SamplePart(u3, 1, 10, 45)), totals{UploadBytes: 10})
	admitted("the first put into a bucket that keeps versions that fits", putEntry("freshkept", "a", 10, 46), totals{Objects: 1, CurrentBytes: 10, Versions: 1, StoredBytes: 10})
	admitted("the first put into a bucket that kept versions that fits", putEntry("freshpaused", "a", 10, 47), totals{Objects: 1, CurrentBytes: 10, Versions: 1, StoredBytes: 10})
	if after := places(); !slices.Contains(after, "parts/fresh") || !slices.Contains(after, "versions/freshkept") || !slices.Contains(after, "versions/freshpaused") {
		t.Errorf("admitted, they have their places: %v", after)
	}

	// A bucket without a record, and one that is counted for the first
	// time, enforces nothing: there is no figure to hold a quota to. (No
	// entry can give either a quota; the rule is the keeper's all the same.)
	big := bucket.UsageDelta{StoredBytes: math.MaxInt64}
	for name, keeper := range map[string]usageKeeper{
		"no record":     {bucket: "x"},
		"a first count": {bucket: "x", kept: true, usage: bucket.Usage{}.Counting()},
	} {
		if !keeper.admits(1, big) {
			t.Errorf("%s refuses a change by a quota", name)
		}
	}
	full := usageKeeper{bucket: "x", kept: true, usage: bucket.Usage{Exact: true, UsageTotals: totals{StoredBytes: 10}}.Counting()}
	if full.admits(10, bucket.UsageDelta{StoredBytes: 1}) || !full.admits(10, bucket.UsageDelta{StoredBytes: 1, UploadBytes: -1}) || !full.admits(0, big) {
		t.Errorf("a bucket that is counted again holds its quota by what it kept")
	}
}

// bucketName names the bucket an entry of these tests is about.
func (e entry) bucketName() string {
	switch {
	case e.PutObject != nil:
		return e.PutObject.Bucket
	case e.DeleteVersion != nil:
		return e.DeleteVersion.Bucket
	case e.CreateUpload != nil:
		return e.CreateUpload.Bucket
	case e.PutPart != nil:
		return e.PutPart.Bucket
	case e.CompleteUpload != nil:
		return e.CompleteUpload.Bucket
	case e.AbortUpload != nil:
		return e.AbortUpload.Bucket
	}
	return ""
}

// TestTheSequenceOfAVersionIsAboveTheGreatestOfItsBucket: a version made
// here is given the index of its entry - or, in a bucket that holds a
// version at a sequence at or above it, one above the greatest there is
// (bucket.Usage.NextSeq), so that it is the newest of its key. A marker
// likewise. A bucket without versions keeps the sequence 0 it always had,
// and a bucket without a record the index.
func TestTheSequenceOfAVersionIsAboveTheGreatestOfItsBucket(t *testing.T) {
	t.Parallel()
	m := newMachine5(t)
	m.bucket5("ver", bucket.VersioningEnabled)
	m.bucket5("plain", bucket.Unversioned)
	m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "old", CreatedAt: m.at}})
	m.ok(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "old", Status: string(bucket.VersioningEnabled)}})

	stored := m.ok5(putEntry("ver", "a", 1, 1)).stored
	if stored.Seq != m.index {
		t.Fatalf("a version is at %d, its entry is %d", stored.Seq, m.index)
	}
	if usage := checkUsage(t, m.f, "ver"); usage.High != m.index {
		t.Errorf("the greatest sequence is %d after the entry %d", usage.High, m.index)
	}
	// As a bucket that was a replica holds them: a version far above the
	// indexes of this log.
	usage := checkUsage(t, m.f, "ver")
	usage.High = 1 << 40
	setUsage(t, m.f, "ver", usage)
	if stored = m.ok5(putEntry("ver", "a", 1, 2)).stored; stored.Seq != 1<<40+1 {
		t.Errorf("a version in a bucket that holds one at 2^40 is at %d", stored.Seq)
	}
	marker := m.ok5(deleteEntry("ver", "a", "", 3)).marker
	if marker == nil || marker.Seq != 1<<40+2 {
		t.Errorf("a marker after it is %+v", marker)
	}
	m.ok5(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "ver", Status: string(bucket.VersioningSuspended)}})
	if stored = m.ok5(putEntry("ver", "a", 1, 4)).stored; stored.Seq != 1<<40+3 || stored.Versioned {
		t.Errorf("a null version after it is at %d", stored.Seq)
	}
	if usage = checkUsage(t, m.f, "ver"); usage.High != 1<<40+3 {
		t.Errorf("the greatest sequence is %d", usage.High)
	}
	// The newest of its key, by the order the versions are kept in.
	if current := m.ok5(deleteEntry("ver", "a", "", 5)).marker; current == nil || current.Seq != 1<<40+4 {
		t.Errorf("a null marker in its place is %+v", current)
	}
	// At the last number there is it stays there, and wraps to nothing.
	usage = checkUsage(t, m.f, "ver")
	usage.High = math.MaxUint64
	setUsage(t, m.f, "ver", usage)
	m.ok5(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "ver", Status: string(bucket.VersioningEnabled)}})
	if stored = m.ok5(putEntry("ver", "b", 1, 6)).stored; stored.Seq != math.MaxUint64 {
		t.Errorf("a version in a bucket at the last sequence is at %d", stored.Seq)
	}

	if stored = m.ok5(putEntry("plain", "a", 1, 7)).stored; stored.Seq != 0 {
		t.Errorf("a version of a bucket without versions is at %d", stored.Seq)
	}
	if usage = checkUsage(t, m.f, "plain"); usage.High != 0 {
		t.Errorf("the greatest sequence of a bucket without versions is %d", usage.High)
	}
	if stored = m.ok5(putEntry("old", "a", 1, 8)).stored; stored.Seq != m.index {
		t.Errorf("a version of a bucket without a record is at %d, its entry is %d", stored.Seq, m.index)
	}
}

// orderedKeys are keys that would be walked in another order than they are
// compared in if the encoding of a key in "versions" did not keep the order
// of the keys: a zero byte and what stands beside it, the separator, the
// byte before and after it, a key that is the beginning of another, the last
// characters there are, and keys of the greatest length.
func orderedKeys() []string {
	long := strings.Repeat("l", object.MaxKeyLength-1)
	return []string{
		"\x00", "\x00\x00", "\x00\x01", "\x00\xc3\xbf", "\x01", "\x01\x00", "\x01\x01", "\x02",
		"a", "a\x00", "a\x00\x00", "a\x00\x01", "a\x00b", "a\x01", "a\x01\x00", "a\x02", "a.", "a/", "a/\x00", "a/b", "a0", "aa", "a\u007f", "a\u0080",
		"aÿ", "aĀ", "a￿", "a\U00010000", "a\U0010ffff", "b",
		long, long + "\x00", long + "\x01", long + "l", strings.Repeat("l", object.MaxKeyLength-2) + "ÿ",
		"zoë/π.txt", "ÿ", "￿", "\U0010ffff", "\U0010ffff\x00",
	}
}

// TestTheEncodingOfAKeyKeepsTheOrderOfTheKeys: the keys of "versions" sort
// as the object keys they encode do, compared byte for byte - which is the
// order of "objects", and the order bucket.Usage compares a key with the
// cursor of the walk in. And the place behind the versions of a key is behind
// all of them and before the first version of every key that follows.
func TestTheEncodingOfAKeyKeepsTheOrderOfTheKeys(t *testing.T) {
	t.Parallel()
	keys := orderedKeys()
	// And what no key can be but the encoding must still order: bytes
	// that are no UTF-8.
	keys = append(keys, "\xff", "\xff\x00", "a\xff", "a\xff\x00", "a\x00\xff", "a\x00\xff\x00", "\xfe\xff", "")
	for _, a := range keys {
		for _, b := range keys {
			if got, want := bytes.Compare(encodeKey(a), encodeKey(b)), strings.Compare(a, b); got != want {
				t.Errorf("enc(%q) against enc(%q) = %d, the keys compare as %d", a, b, got, want)
			}
			if a == b {
				continue
			}
			if bytes.HasPrefix(encodeKey(b), encodeKey(a)) {
				t.Errorf("enc(%q) begins enc(%q)", a, b)
			}
			// Every version of a, whatever its sequence and its ID, is
			// before afterKey(a); every version of a later key is behind it.
			for _, suffix := range [][]byte{bytes.Repeat([]byte{0}, versionSuffixSize), bytes.Repeat([]byte{0xff}, versionSuffixSize)} {
				mine, theirs := append(encodeKey(a), suffix...), append(encodeKey(b), suffix...)
				if bytes.Compare(mine, afterKey(a)) >= 0 {
					t.Errorf("a version of %q is not before the place behind its versions", a)
				}
				if behind := bytes.Compare(theirs, afterKey(a)) > 0; behind != (b > a) {
					t.Errorf("a version of %q is behind the versions of %q: %t", b, a, behind)
				}
			}
		}
		if decoded, rest, ok := decodeKey(append(encodeKey(a), 1, 2, 3)); !ok || decoded != a || !bytes.Equal(rest, []byte{1, 2, 3}) {
			t.Errorf("enc(%q) decodes as %q, %x, %t", a, decoded, rest, ok)
		}
	}
}

// FuzzTheOrderOfEncodedKeys: for any two keys, of any bytes, the encoded
// keys compare as the keys do, neither is the beginning of the other, and
// each decodes as itself.
func FuzzTheOrderOfEncodedKeys(f *testing.F) {
	for _, pair := range [][2]string{{"a", "a\x00"}, {"a\x00", "a\x01"}, {"a\x00\xff", "a\x01"}, {"", "\x00"}, {"a/b", "a0"}, {"\xff", "\xff\xff"}, {"zoë", "zoë/π"}} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		encA, encB := encodeKey(a), encodeKey(b)
		if got, want := bytes.Compare(encA, encB), strings.Compare(a, b); got != want {
			t.Fatalf("enc(%q) against enc(%q) = %d, the keys compare as %d", a, b, got, want)
		}
		if a != b && (bytes.HasPrefix(encA, encB) || bytes.HasPrefix(encB, encA)) {
			t.Fatalf("one of enc(%q) and enc(%q) begins the other", a, b)
		}
		if decoded, rest, ok := decodeKey(append(encA, encB...)); !ok || decoded != a || !bytes.Equal(rest, encB) {
			t.Fatalf("enc(%q) decodes as %q, %t", a, decoded, ok)
		}
		if behind := bytes.Compare(append(encB, 0xff), afterKey(a)) > 0; behind != (b > a) {
			t.Fatalf("a version of %q is behind the versions of %q: %t", b, a, behind)
		}
	})
}

// TestTheWalkGoesInTheOrderTheRecordComparesBy: the walk visits the keys of
// a bucket in the order bucket.Usage compares a key with its cursor in -
// byte for byte, as Go compares strings. Were the two orders not one, a
// change of a key the walk has been past would be taken for one it has yet
// to visit, and be counted twice or not at all. The keys here are those that
// would tell: some held in "objects" only, some in "versions" only (nothing
// but a marker), some in both; and a change between any two steps, behind
// and before the walk, leaves the count a recount of what lies behind it.
func TestTheWalkGoesInTheOrderTheRecordComparesBy(t *testing.T) {
	t.Parallel()
	for name, limit := range map[string]int{"a key a step": 1, "a few records a step": 4} {
		seed := 23 + limit
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMachine5(t)
			m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedAt: m.at}})
			m.ok(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "b", Status: string(bucket.VersioningEnabled)}})
			keys := orderedKeys()
			rng := newDice(seed)
			serial := 0
			change := func(key string) {
				serial++
				held, schema := written(putEntry("b", key, int64(1+serial%7), serial), entrySchema4)
				if rng.roll(3) == 0 {
					held, schema = written(deleteEntry("b", key, "", serial), entrySchema4)
				}
				if r := m.as(schema, held); r.err != nil {
					t.Fatal(r.err)
				}
			}
			// In another order than theirs; one version, or a version and
			// a marker, or a marker alone.
			for _, at := range rng.perm(len(keys)) {
				switch at % 3 {
				case 0:
					change(keys[at])
				case 1:
					change(keys[at])
					change(keys[at])
				default:
					serial++
					if r := m.as(entrySchemaTimed, deleteEntry("b", keys[at], "", serial)); r.err != nil {
						t.Fatal(r.err)
					}
				}
			}
			want := slices.Clone(keys)
			slices.Sort(want)
			last := ""
			steps := 0
			for {
				usage, drift := m.walk("b", limit, false)
				checkUsage(t, m.f, "b")
				if drift {
					t.Fatal("a first count found drift")
				}
				if !usage.Walking() || usage.Count.Phase != bucket.UsagePhaseKeys {
					break
				}
				cursor := usage.Count.Cursor
				if cursor <= last {
					t.Fatalf("the walk went from %q to %q", last, cursor)
				}
				at := slices.Index(want, cursor)
				if at < 0 {
					t.Fatalf("the walk is at %q, which is no key of the bucket", cursor)
				}
				// A step that asks for one record visits one key: the next
				// in the order of the keys, whatever index it is held in.
				if limit == 1 && at != steps {
					t.Fatalf("step %d of the walk is at %q, the key at that place is %q", steps, cursor, want[steps])
				}
				last = cursor
				if steps++; steps > len(want) {
					t.Fatalf("the walk through %d keys has taken %d steps", len(want), steps)
				}
				// Changes of keys behind the walk and before it, and at the
				// cursor itself.
				change(cursor)
				change(want[rng.roll(at+1)])
				change(want[at+rng.roll(len(want)-at)])
				checkUsage(t, m.f, "b")
			}
			if limit == 1 && steps != len(want)-1 {
				// The step that visits the last key ends the keys, and has
				// no cursor to show.
				t.Errorf("the walk showed %d keys before it ended, the bucket has %d", steps, len(want))
			}
			usage := checkUsage(t, m.f, "b")
			for usage.Walking() {
				m.walk("b", limit, false)
				usage = checkUsage(t, m.f, "b")
			}
			if !usage.Exact {
				t.Fatalf("the walk did not end: %+v", usage)
			}
			// And a recount, through both indexes merged again, comes to
			// what was kept.
			usage, _ = m.walk("b", limit, true)
			for usage.Walking() {
				var drift bool
				if usage, drift = m.walk("b", limit, false); drift {
					t.Fatal("a recount of a bucket that was kept found drift")
				}
				checkUsage(t, m.f, "b")
			}
			if m.f.usageDrift.Load() != 0 || m.f.usageClamped.Load() != 0 {
				t.Errorf("drift %d, clamped %d", m.f.usageDrift.Load(), m.f.usageClamped.Load())
			}
		})
	}
}

// TestADamagedUsageRecordStopsTheStateMachine: a record that is not what
// the domain writes is not counted on from, and not replaced by one of this
// member's making: the others hold other bytes. The state machine stops, at
// the entry that would have kept the record, with its state as it was.
func TestADamagedUsageRecordStopsTheStateMachine(t *testing.T) {
	t.Parallel()
	exact := `{"v":1,"exact":true,"objects":0,"current_bytes":0,"versions":0,"stored_bytes":0,"upload_bytes":0,"replication_pending":0,"replication_failed":0,"high":0}`
	for name, test := range map[string]struct {
		record string
		want   error
	}{
		"no JSON":                    {"usage", domain.ErrIntegrity},
		"white space":                {strings.Replace(exact, ",", ", ", 1), domain.ErrIntegrity},
		"a field too many":           {strings.Replace(exact, "}", `,"more":1}`, 1), domain.ErrIntegrity},
		"a version it does not know": {strings.Replace(exact, `"v":1`, `"v":2`, 1), domain.ErrUnsupportedFormat},
		"neither exact nor counted":  {strings.Replace(exact, "true", "false", 1), domain.ErrIntegrity},
	} {
		for what, e := range map[string]entry{
			"a put":    putEntry("b", "k", 1, 1),
			"a delete": deleteEntry("b", "k", "", 2),
			"a walk":   {Type: cmdWalkBucketUsage, WalkBucketUsage: &usageWalkCmd{Bucket: "b", Limit: 1}},
			"a quota":  {Type: cmdSetBucketQuota, SetBucketQuota: &bucketQuotaCmd{Bucket: "b", Bytes: 1}},
		} {
			t.Run(name+" and "+what, func(t *testing.T) {
				t.Parallel()
				m := newMachine5(t)
				m.bucket5("b", bucket.Unversioned)
				m.ok5(putEntry("b", "k", 5, 9))
				if err := m.f.db.Update(func(tx *bbolt.Tx) error {
					return tx.Bucket(bucketUsageBucket()).Put([]byte("b"), []byte(test.record))
				}); err != nil {
					t.Fatal(err)
				}
				before := wholeState(t, m.f)
				held, schema := written(e, entrySchema5)
				if r := m.raw(schema, held); !r.fatal || !errors.Is(r.err, test.want) {
					t.Fatalf("%s over a damaged record = %+v, want the state machine to stop with %v", what, r.err, test.want)
				}
				if after := wholeState(t, m.f); !reflect.DeepEqual(after, before) {
					t.Errorf("the entry it stopped at changed the state")
				}
			})
		}
	}
}

// usagePair drives state machines and the in-memory model of the ports
// (porttest.MemMetadata) through the same history. The model does not add up
// changes: it counts a bucket by looking, every time it is asked - so what it
// says is, by definition, what a recount finds, and it decides a quota by
// that. The others are replicas, fed the same log.
//
// The outcome of every entry is held to the model's - taken or refused, and
// refused by a quota or not - and so is the answer of every entry of the
// walk. The whole of what the first state machine keeps is held to the model,
// and recounted from the state itself, where it can have parted: at once
// after every entry that was refused (which must have changed nothing), and
// otherwise every lookEvery entries and at the end. A record is the sum of
// every change before it, so one that has parted stays parted until it is
// looked at, and what parted it is among the few entries since the last look.
type usagePair struct {
	t     *testing.T
	rng   *dice
	model *porttest.MemMetadata
	fsms  []*fsm
	index uint64
	at    time.Time
	// serial tells one ID from the next.
	serial int
	// versioning is what the driver takes each bucket's state to be, and
	// absent a bucket that is not there.
	versioning map[string]bucket.Versioning
	versions   map[string][]object.ID
	uploads    []*pairUpload
	// refusals counts the entries a quota refused, and walks those of the
	// walk that took a step.
	refusals, walks, drifts int
	// negative counts the puts of a size below nothing.
	negative int
	// look says that the entry just applied is one after which the whole is
	// held to the model at once (due), and looks counts how often it was.
	look  bool
	looks int
}

// lookEvery is how many entries pass, at most, between two times the whole of
// what a state machine keeps is held to the model.
const lookEvery = 25

// due reports whether the whole is to be held to the model after entry i of
// a history: after an entry that was refused or marked otherwise, and every
// lookEvery entries.
func (p *usagePair) due(i int) bool {
	due := p.look || i%lookEvery == lookEvery-1
	p.look = false
	if due {
		p.looks++
	}
	return due
}

type pairUpload struct {
	upload object.Upload
	parts  map[int]object.Part
}

func fastFSM(t *testing.T) *fsm {
	t.Helper()
	f := newFSM(t)
	f.supported = entrySchema5
	// The drive is not what is under test, and there are thousands of
	// entries.
	f.db.NoSync = true
	return f
}

func newUsagePair(t *testing.T, seed, replicas int) *usagePair {
	t.Helper()
	p := &usagePair{
		t: t, rng: newDice(seed), model: porttest.NewMemMetadata(), at: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC),
		versioning: map[string]bucket.Versioning{}, versions: map[string][]object.ID{},
	}
	p.model.Now = func() time.Time { return p.at }
	for range replicas {
		p.fsms = append(p.fsms, fastFSM(t))
	}
	return p
}

func (p *usagePair) id() object.ID {
	p.serial++
	return serialID(p.serial)
}

// apply writes an entry as a proposer that takes the cluster to be at level
// writes it, applies it to every state machine, and holds them to one
// answer.
func (p *usagePair) apply(level byte, e entry) applyResult {
	p.t.Helper()
	held := heldTo(e, level)
	schema := writtenAt(held, versionOf(held), level)
	p.index++
	p.at = p.at.Add(time.Second)
	if level >= entrySchemaTimed {
		schema = max(schema, entrySchemaTimed)
		held.At = p.at
	}
	data, err := encodeEntry(held, schema)
	if err != nil {
		p.t.Fatal(err)
	}
	var first applyResult
	var answer []byte
	for i, f := range p.fsms {
		r := result(p.t, f.Apply(&raft.Log{Index: p.index, Type: raft.LogCommand, Data: data}))
		if r.fatal {
			p.t.Fatalf("entry %d, %s, stopped state machine %d: %v", p.index, e.Type, i, r.err)
		}
		encoded, err := encodeResult(r)
		if err != nil {
			p.t.Fatal(err)
		}
		if i == 0 {
			first, answer = r, encoded
		} else if !bytes.Equal(encoded, answer) {
			p.t.Fatalf("entry %d, %s: state machine %d answers %s, the first %s", p.index, e.Type, i, encoded, answer)
		}
	}
	return first
}

// agree holds the outcome of an entry to that of the same change in the
// model: both took effect or both were refused, and by a quota both or
// neither.
func (p *usagePair) agree(what string, r applyResult, modelErr error) bool {
	p.t.Helper()
	if (r.err != nil) != (modelErr != nil) {
		p.t.Fatalf("entry %d, %s: the state machine says %v, the model %v", p.index, what, r.err, modelErr)
	}
	if r.err != nil {
		// What was refused changed nothing: looked at at once.
		p.look = true
	}
	if got, want := errors.Is(r.err, domain.ErrQuotaExceeded), errors.Is(modelErr, domain.ErrQuotaExceeded); got != want {
		p.t.Fatalf("entry %d, %s: refused by the quota in the state machine: %t (%v); in the model: %t (%v)", p.index, what, got, r.err, want, modelErr)
	} else if got {
		p.refusals++
	}
	return r.err == nil
}

// level is the level a proposer takes the cluster to be at: mostly the
// fifth, sometimes - having learnt of it late - the fourth, and now and then
// the second.
func (p *usagePair) level() byte {
	switch n := p.rng.roll(20); {
	case n < 15:
		return entrySchema5
	case n < 19:
		return entrySchema4
	}
	return entrySchemaTimed
}

func (p *usagePair) create(name string, level byte) {
	p.t.Helper()
	ctx := p.t.Context()
	b := bucket.Bucket{Name: name, CreatedAt: p.at, CreatedBy: "root"}
	modelErr := p.model.CreateBucket(ctx, b)
	if modelErr == nil && level < entrySchema5 {
		// The model makes every bucket as a cluster at level 5 does.
		p.model.ForgetUsage(name)
	}
	if p.agree("bucket.create", p.apply(level, entry{Type: cmdCreateBucket, CreateBucket: bucketToRecord(b)}), modelErr) {
		p.versioning[name] = bucket.Unversioned
	}
}

func (p *usagePair) setVersioning(name string, state bucket.Versioning) {
	p.t.Helper()
	modelErr := p.model.SetBucketVersioning(p.t.Context(), name, state)
	e := entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: name, Status: string(state)}}
	if p.agree("bucket.versioning.put", p.apply(p.level(), e), modelErr) {
		p.versioning[name] = state
	}
}

// walkOnce is one entry of the walk through a bucket, in both.
func (p *usagePair) walkOnce(name string, limit int, restart bool) {
	p.t.Helper()
	walked, modelErr := p.model.WalkBucketUsage(p.t.Context(), name, limit, restart)
	r := p.apply(entrySchema5, entry{Type: cmdWalkBucketUsage, WalkBucketUsage: &usageWalkCmd{Bucket: name, Limit: limit, Restart: restart}})
	if !p.agree("bucket.usage.walk", r, modelErr) {
		return
	}
	p.walks++
	if r.drift {
		p.drifts++
	}
	usage, err := bucket.ParseUsage(r.usage)
	if err != nil {
		p.t.Fatalf("entry %d: the answer of a walk: %v", p.index, err)
	}
	p.same(name, usage, walked.Usage)
}

// same holds a record of the state machine to the model's, in everything
// but the greatest sequence: the model numbers its changes by itself.
func (p *usagePair) same(name string, got, want bucket.Usage) {
	p.t.Helper()
	if got.Exact != want.Exact || got.UsageTotals != want.UsageTotals || (got.Count == nil) != (want.Count == nil) {
		p.t.Fatalf("entry %d: the usage of %s is %+v (count %+v), the model says %+v (count %+v)", p.index, name, got, got.Count, want, want.Count)
	}
	if got.Count != nil && *got.Count != *want.Count {
		p.t.Fatalf("entry %d: the walk through %s is at %+v, the model's at %+v", p.index, name, *got.Count, *want.Count)
	}
}

// check holds every bucket's record in the first state machine to the model
// and to the state itself.
func (p *usagePair) check(names []string) {
	p.t.Helper()
	p.counts(names)
	for _, name := range names {
		checkUsage(p.t, p.fsms[0], name)
	}
}

// counts holds every bucket's record in the first state machine, and its
// quota, to the model: to what the model counts by looking at the bucket -
// all of it, or what lies behind a walk that is under way.
func (p *usagePair) counts(names []string) {
	p.t.Helper()
	want, err := p.model.BucketUsages(p.t.Context(), names)
	if err != nil {
		p.t.Fatal(err)
	}
	for i, name := range names {
		got, kept, quota := usageOf(p.t, p.fsms[0], name)
		if kept != (want[i].Usage.Exact || want[i].Usage.Count != nil) {
			p.t.Fatalf("entry %d: %s has a record: %t; the model says %+v", p.index, name, kept, want[i].Usage)
		}
		p.same(name, got, want[i].Usage)
		if quota != want[i].QuotaBytes {
			p.t.Fatalf("entry %d: the quota of %s is %d, the model says %d", p.index, name, quota, want[i].QuotaBytes)
		}
	}
}

// identical holds every replica to the first, byte for byte.
func (p *usagePair) identical() {
	p.t.Helper()
	want := wholeState(p.t, p.fsms[0])
	for i, f := range p.fsms[1:] {
		if got := wholeState(p.t, f); !reflect.DeepEqual(got, want) {
			for key, value := range want {
				if got[key] != value {
					p.t.Errorf("replica %d: %s is %s, the first has %s", i+1, key, got[key], value)
				}
			}
			p.t.Fatalf("entry %d: replica %d holds another state than the first", p.index, i+1)
		}
	}
}

// restored adds a replica that begins from a snapshot of the first.
func (p *usagePair) restored() {
	p.t.Helper()
	snap, err := p.fsms[0].Snapshot()
	if err != nil {
		p.t.Fatal(err)
	}
	sink := &memorySink{}
	if err := snap.Persist(sink); err != nil {
		p.t.Fatal(err)
	}
	snap.Release()
	f := fastFSM(p.t)
	if err := f.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		p.t.Fatalf("Restore: %v", err)
	}
	f.db.NoSync = true
	p.fsms = append(p.fsms, f)
}

// step is one random change of the history, in the model and in the state
// machines.
func (p *usagePair) step(names, keys []string) {
	p.t.Helper()
	ctx := p.t.Context()
	name, key := names[p.rng.roll(len(names))], keys[p.rng.roll(len(keys))]
	at := name + "\x00" + key
	ref := func() object.VersionRef {
		known := p.versions[at]
		if n := p.rng.roll(10); n < 3 || len(known) == 0 {
			return object.VersionRef{Null: true}
		}
		return object.VersionRef{ID: known[p.rng.roll(len(known))]}
	}
	switch n := p.rng.roll(100); {
	case n < 28:
		v := porttest.SampleVersion(name, key, 1)
		v.VersionID, v.DataID, v.Size, v.CreatedBy = p.id(), p.id(), int64(p.rng.roll(5000)), "user/albie"
		if p.rng.roll(12) == 0 {
			// A size below nothing, which no proposer writes and apply
			// takes from an entry all the same: it counts as nothing, in
			// the record, in the walk and under a quota.
			v.Size = -1 - int64(p.rng.roll(3000))
			p.negative++
		}
		_, modelErr := p.model.PutVersion(ctx, v)
		if p.agree("object.put", p.apply(p.level(), entry{Type: cmdPutObject, PutObject: versionToRecord(v)}), modelErr) {
			p.versions[at] = append(p.versions[at], v.VersionID)
		}
	case n < 40:
		o := ports.DeleteOptions{MarkerID: p.id(), At: p.at}
		_, modelErr := p.model.DeleteObject(ctx, name, key, o)
		e := entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: name, Key: key, MarkerID: o.MarkerID.String(), At: o.At, By: "user/albie"}}
		if p.agree("object.version.delete", p.apply(p.level(), e), modelErr) {
			p.versions[at] = append(p.versions[at], o.MarkerID)
		}
	case n < 50:
		version := ref()
		o := ports.DeleteOptions{Version: &version, MarkerID: p.id(), At: p.at}
		_, modelErr := p.model.DeleteObject(ctx, name, key, o)
		e := entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: name, Key: key, VersionRef: version.String(), MarkerID: o.MarkerID.String(), At: o.At}}
		p.agree("object.version.delete of a version", p.apply(p.level(), e), modelErr)
	case n < 52:
		if state, there := p.versioning[name]; there && state != bucket.Unversioned {
			return
		}
		// The delete of the first schema, which a log may still hold.
		_, modelErr := p.model.DeleteObject(ctx, name, key, ports.DeleteOptions{MarkerID: p.id(), At: p.at})
		p.agree("object.delete", p.apply(entrySchemaVersion, entry{Type: cmdDeleteObject, DeleteObject: &deleteObjectCmd{Bucket: name, Key: key}}), modelErr)
	case n < 55:
		if name == "plain" || name == "old" {
			return
		}
		state := bucket.VersioningEnabled
		if p.rng.roll(2) == 0 {
			state = bucket.VersioningSuspended
		}
		p.setVersioning(name, state)
	case n < 60:
		u := porttest.SampleUpload(name, key, 1)
		u.ID = p.id()
		modelErr := p.model.CreateUpload(ctx, u)
		if p.agree("upload.create", p.apply(p.level(), entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)}), modelErr) {
			p.uploads = append(p.uploads, &pairUpload{upload: u, parts: map[int]object.Part{}})
		}
	case n < 70 && len(p.uploads) > 0:
		u := p.uploads[p.rng.roll(len(p.uploads))]
		size := int64(object.MinPartSize)
		if p.rng.roll(3) == 0 {
			size = int64(p.rng.roll(4000))
		}
		part := porttest.SamplePart(u.upload, 1+p.rng.roll(3), size, 1)
		part.DataID = p.id()
		_, modelErr := p.model.PutPart(ctx, u.upload.Bucket, u.upload.Key, u.upload.ID, part)
		if p.agree("upload.part.put", p.apply(p.level(), partEntry(u.upload, part)), modelErr) {
			u.parts[part.Number] = part
		}
	case n < 74 && len(p.uploads) > 0:
		i := p.rng.roll(len(p.uploads))
		u := p.uploads[i]
		c := ports.Completion{Bucket: u.upload.Bucket, Key: u.upload.Key, ID: u.upload.ID, VersionID: p.id(), ETag: "etag", At: p.at, CreatedBy: "root"}
		numbers := make([]int, 0, len(u.parts))
		for number := range u.parts {
			numbers = append(numbers, number)
		}
		slices.Sort(numbers)
		for _, number := range numbers {
			if p.rng.roll(4) == 0 {
				continue
			}
			part := u.parts[number]
			part.Seal = bytes.Repeat([]byte{1}, sha256.Size)
			c.Parts = append(c.Parts, part)
		}
		_, modelErr := p.model.CompleteUpload(ctx, c)
		cmd := &completeUploadCmd{
			Bucket: c.Bucket, Key: c.Key, UploadID: c.ID.String(), VersionID: c.VersionID.String(), ETag: c.ETag, ModifiedAt: c.At.UTC(), CreatedBy: c.CreatedBy,
			Parts: make([]listedPart, len(c.Parts)),
		}
		for j, part := range c.Parts {
			cmd.Parts[j] = listedPart{Number: part.Number, ETag: part.ETag, Checksums: part.Checksums, DataID: part.DataID.String(), Seal: part.Seal}
		}
		if p.agree("upload.complete", p.apply(p.level(), entry{Type: cmdCompleteUpload, CompleteUpload: cmd}), modelErr) {
			p.versions[c.Bucket+"\x00"+c.Key] = append(p.versions[c.Bucket+"\x00"+c.Key], c.VersionID)
			p.uploads = slices.Delete(p.uploads, i, i+1)
		}
	case n < 77 && len(p.uploads) > 0:
		i := p.rng.roll(len(p.uploads))
		u := p.uploads[i]
		_, modelErr := p.model.AbortUpload(ctx, u.upload.Bucket, u.upload.Key, u.upload.ID)
		p.agree("upload.abort", p.apply(p.level(), abortEntry(u.upload)), modelErr)
		if p.rng.roll(4) != 0 {
			p.uploads = slices.Delete(p.uploads, i, i+1)
		}
	case n < 89:
		// A walk with a small limit; now and then one that begins again -
		// also while one is under way.
		p.walkOnce(name, 1+p.rng.roll(6), p.rng.roll(7) == 0)
	case n < 93:
		usages, err := p.model.BucketUsages(ctx, []string{name})
		if err != nil {
			p.t.Fatal(err)
		}
		var quota int64
		if p.rng.roll(5) != 0 {
			quota = max(1, usages[0].Usage.Used()+int64(p.rng.roll(12000))-4000)
		}
		modelErr := p.model.SetBucketQuota(ctx, name, quota, audit.Record{})
		p.agree("bucket.quota.put", p.apply(entrySchema5, entry{Type: cmdSetBucketQuota, SetBucketQuota: &bucketQuotaCmd{Bucket: name, Bytes: quota}}), modelErr)
	case n < 96:
		// A bucket that comes and goes: made by a proposer of level 5 or
		// one of level 4, and deleted when it is empty.
		if _, there := p.versioning["late"]; !there {
			level := byte(entrySchema5)
			if p.rng.roll(2) == 0 {
				level = entrySchema4
			}
			p.create("late", level)
			return
		}
		modelErr := p.model.DeleteBucket(ctx, "late")
		if p.agree("bucket.delete", p.apply(p.level(), entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "late"}}), modelErr) {
			delete(p.versioning, "late")
		}
	default:
		version := ref()
		tags := map[string]string{"n": fmt.Sprint(n)}
		_, modelErr := p.model.SetTags(ctx, name, key, version, tags)
		e := entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: name, Key: key, VersionRef: version.String(), Tags: tags}}
		p.agree("object.tags.put", p.apply(p.level(), e), modelErr)
	}
}

// TestUsageIsARecountThroughAnyHistory: through long random histories -
// puts and overwrites, versions and markers, deletes of every kind, uploads
// with parts put again, completed and aborted, buckets of before schema 5
// and of it, one that is deleted and made again, quotas set and removed,
// entries written by proposers of level 5, 4 and 2, and entries of the walk
// with small limits anywhere among them, beginning again while under way -
// the record of every bucket is what the model of the ports says: a recount
// when it is exact, a recount of what lies behind the walk while it is
// counted, and nothing while it is counted for the first time. It is held to
// that after every entry that was refused, every lookEvery entries and at the
// end (usagePair); the answer of every entry of the walk is held to it at
// once. Some puts are of a size below nothing, which counts as nothing
// everywhere. A quota refuses in the state machine exactly when it refuses in
// the model. No recount finds drift and no change is held at a bound. A replica
// fed the same log holds the same bytes, and so does one that begins from a
// snapshot taken while a walk was under way.
func TestUsageIsARecountThroughAnyHistory(t *testing.T) {
	t.Parallel()
	const entries = 1800
	for _, seed := range []int{1, 2, 3, 2026} {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()
			p := newUsagePair(t, seed, 2)
			names := []string{"plain", "kept", "paused", "old", "oldkept", "late"}
			keys := []string{"a", "a/b", "a\x00b", "a\x01", "b", "zoë/π.txt", "k \"quoted\"", strings.Repeat("l", 200)}
			p.create("plain", entrySchema5)
			p.create("kept", entrySchema5)
			p.create("paused", entrySchema5)
			p.create("old", entrySchema4)
			p.create("oldkept", entrySchemaTimed)
			p.setVersioning("kept", bucket.VersioningEnabled)
			p.setVersioning("paused", bucket.VersioningEnabled)
			p.setVersioning("oldkept", bucket.VersioningEnabled)
			p.check(names)
			snapshots := 0
			for i := range entries {
				p.step(names, keys)
				if p.due(i) {
					p.check(names)
				}
				// A replica from a snapshot taken in the middle of a walk:
				// twice, where the history has one under way.
				if i > entries/3 && snapshots < 2 && i%97 == 0 {
					for _, name := range []string{"kept", "oldkept", "paused"} {
						p.walkOnce(name, 1, true)
						if usage, _, _ := usageOf(t, p.fsms[0], name); usage.Walking() {
							p.restored()
							snapshots++
							break
						}
					}
					p.check(names)
				}
				if i%200 == 0 {
					p.identical()
				}
			}
			if snapshots == 0 {
				t.Fatal("no snapshot was taken while a walk was under way")
			}
			// Every walk to its end, and every bucket that was never
			// counted: all are exact, and say what a recount finds.
			for _, name := range names {
				if _, there := p.versioning[name]; !there {
					continue
				}
				for range 10000 {
					if usage, kept, _ := usageOf(t, p.fsms[0], name); kept && !usage.Walking() {
						break
					}
					p.walkOnce(name, 3, false)
				}
				p.walkOnce(name, 5, true)
				for range 10000 {
					if usage, _, _ := usageOf(t, p.fsms[0], name); !usage.Walking() {
						break
					}
					p.walkOnce(name, 5, false)
				}
				if usage := checkUsage(t, p.fsms[0], name); !usage.Exact || usage.Walking() {
					t.Errorf("%s at the end: %+v", name, usage)
				}
			}
			p.check(names)
			p.identical()
			if len(p.fsms) != 2+snapshots {
				t.Errorf("%d state machines", len(p.fsms))
			}
			for i, f := range p.fsms {
				if f.usageClamped.Load() != 0 || f.usageDrift.Load() != 0 {
					t.Errorf("state machine %d: %d changes held at a bound, %d recounts that found drift", i, f.usageClamped.Load(), f.usageDrift.Load())
				}
			}
			if p.drifts != 0 {
				t.Errorf("%d entries of the walk reported drift", p.drifts)
			}
			if p.refusals < 5 || p.walks < 100 || p.negative < 20 || p.looks < entries/lookEvery+p.refusals {
				t.Errorf("the history had %d refusals by a quota, %d entries of the walk and %d puts of a size below nothing, and was held to the model %d times: it tried too little",
					p.refusals, p.walks, p.negative, p.looks)
			}
			if got := stateSchema(t, p.fsms[0]); got != stateSchema5 {
				t.Errorf("state schema %d", got)
			}
		})
	}
}

// TestAStoreShowsWhatItsStateMachineCounted: the clamps and the drift a
// node's state machine has counted are what its metrics read
// (cactusfs_bucket_usage_clamped_total, cactusfs_bucket_usage_drift_total).
func TestAStoreShowsWhatItsStateMachineCounted(t *testing.T) {
	t.Parallel()
	store := openStore(t, t.TempDir())
	if o, ok := store.Observe(); !ok || o.UsageClamped != 0 || o.UsageDrift != 0 {
		t.Fatalf("a new store shows %+v, %t", o, ok)
	}
	store.fsm.usageClamped.Add(2)
	store.fsm.usageDrift.Add(1)
	if o, ok := store.Observe(); !ok || o.UsageClamped != 2 || o.UsageDrift != 1 {
		t.Errorf("after two clamps and a drift the store shows %+v, %t", o, ok)
	}
}

// BenchmarkApplyPutWithAUsageRecord is BenchmarkApplyPutAtLevel4 into a
// bucket that has a usage record - one made by a cluster at level 5 - and
// into one that has a quota besides: what the record costs a put (ADR-0023,
// "Consequences").
func BenchmarkApplyPutWithAUsageRecord(b *testing.B) {
	for _, quota := range []int64{0, bucket.MaxQuotaBytes} {
		for name, sync := range map[string]bool{"without waiting for the drive": false, "durable": true} {
			if quota != 0 {
				name += " and a quota"
			}
			b.Run(name, func(b *testing.B) {
				f := benchFSM(b, sync)
				f.supported = entrySchema5
				index := uint64(0)
				for _, e := range []entry{
					{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedBy: "root"}},
					{Type: cmdSetBucketQuota, SetBucketQuota: &bucketQuotaCmd{Bucket: "b", Bytes: quota}},
				} {
					data, err := encodeEntry(e, entrySchema5)
					if err != nil {
						b.Fatal(err)
					}
					index++
					if r, ok := f.Apply(&raft.Log{Index: index, Type: raft.LogCommand, Data: data}).(applyResult); !ok || r.err != nil {
						b.Fatalf("%s: %v", e.Type, r.err)
					}
				}
				benchPuts(b, f, index)
				b.StopTimer()
				if err := f.view(func(tx *bbolt.Tx) error {
					usage, kept, err := readUsage(tx, "b")
					if err != nil {
						return err
					}
					if !kept || !usage.Exact || usage.Versions != int64(min(b.N, 1000)) {
						return fmt.Errorf("after %d puts the record is %+v, %t", b.N, usage, kept)
					}
					return nil
				}); err != nil {
					b.Fatal(err)
				}
			})
		}
	}
}

// TestASizeBelowNothingCountsAsNothing: an object.put is not refused for a
// size below nothing - the release of schema 4 applies it - and such a
// version counts for no bytes everywhere: in what its put adds, in what its
// overwrite and its delete take away, in what the walk finds, and under a
// quota. Counted as it is by the changes and as nothing by the walk, it made
// room for bytes that nobody freed: a bucket of quota 1000 that holds 1000
// took 600 more after a put of -600.
func TestASizeBelowNothingCountsAsNothing(t *testing.T) {
	t.Parallel()
	type totals = bucket.UsageTotals
	for name, versioning := range map[string]bucket.Versioning{
		"no versions": bucket.Unversioned, "versions kept": bucket.VersioningEnabled, "versions suspended": bucket.VersioningSuspended,
	} {
		for _, level := range []byte{entrySchema5, entrySchema4, entrySchemaVersion} {
			t.Run(fmt.Sprintf("%s, a proposer of level %d", name, level), func(t *testing.T) {
				t.Parallel()
				m := newMachine5(t)
				m.bucket5("b", versioning)
				kept := versioning == bucket.VersioningEnabled
				// Every put is written as a proposer of the level writes it.
				put := func(key string, size int64, serial int) applyResult {
					e := putEntry("b", key, size, serial)
					if level == entrySchemaVersion {
						return m.as(level, heldTo(e, level))
					}
					held, schema := written(e, level)
					return m.as(schema, held)
				}
				var negative string
				for _, step := range []struct {
					what   string
					do     func() applyResult
					want   error
					totals *totals
				}{
					{what: "a put of 1000", do: func() applyResult { return put("k1", 1000, 1) }, totals: &totals{Objects: 1, CurrentBytes: 1000, Versions: 1, StoredBytes: 1000}},
					{what: "a quota of 1000", do: func() applyResult {
						return m.at5(entry{Type: cmdSetBucketQuota, SetBucketQuota: &bucketQuotaCmd{Bucket: "b", Bytes: 1000}})
					}},
					{what: "a byte into the full bucket", do: func() applyResult { return put("over", 1, 2) }, want: domain.ErrQuotaExceeded},
					{what: "a put of -600", do: func() applyResult {
						r := put("neg", -600, 3)
						if r.stored == nil || r.stored.Size != -600 {
							t.Fatalf("the version of -600 bytes is stored as %+v", r.stored)
						}
						negative = r.stored.VersionID
						return r
					}, totals: &totals{Objects: 2, CurrentBytes: 1000, Versions: 2, StoredBytes: 1000}},
					{what: "600 more into the bucket that holds its 1000", do: func() applyResult { return put("more", 600, 4) }, want: domain.ErrQuotaExceeded},
					{what: "a byte more", do: func() applyResult { return put("more", 1, 5) }, want: domain.ErrQuotaExceeded},
					{what: "a put of nothing", do: func() applyResult { return put("more", 0, 6) }, totals: &totals{Objects: 3, CurrentBytes: 1000, Versions: 3, StoredBytes: 1000}},
					// Over the version of -600: it frees nothing.
					{what: "a byte over the version of -600", do: func() applyResult { return put("neg", 1, 7) }, want: domain.ErrQuotaExceeded},
					{what: "the walk, begun again and to its end", do: func() applyResult {
						usage, drift := m.walk("b", 1, true)
						for usage.Walking() && !drift {
							usage, drift = m.walk("b", 1, false)
						}
						if drift || !usage.Exact {
							t.Fatalf("a recount of a bucket that holds a version of -600 bytes: drift %t, %+v", drift, usage)
						}
						return applyResult{}
					}, totals: &totals{Objects: 3, CurrentBytes: 1000, Versions: 3, StoredBytes: 1000}},
					{what: "the delete of the version of -600", do: func() applyResult {
						// For good: where versions are kept, by its ID; where
						// they were, as the null version it is.
						ref := ""
						switch versioning {
						case bucket.VersioningEnabled:
							ref = negative
						case bucket.VersioningSuspended:
							ref = object.NullVersion
						}
						return m.at5(deleteEntry("b", "neg", ref, 8))
					}, totals: &totals{Objects: 2, CurrentBytes: 1000, Versions: 2, StoredBytes: 1000}},
					{what: "a byte, when it is gone", do: func() applyResult { return put("over", 1, 9) }, want: domain.ErrQuotaExceeded},
				} {
					r := step.do()
					if !errors.Is(r.err, step.want) || (step.want == nil && r.err != nil) {
						t.Fatalf("%s = %v, want %v", step.what, r.err, step.want)
					}
					usage := checkUsage(t, m.f, "b")
					if step.totals != nil && usage.UsageTotals != *step.totals {
						t.Fatalf("after %s the bucket holds %+v, want %+v", step.what, usage.UsageTotals, *step.totals)
					}
				}
				// A version of a size below nothing over one of 1000 bytes:
				// where versions are not kept the 1000 are freed, and where
				// they are, nothing is.
				if r := put("k1", -5, 10); r.err != nil {
					t.Fatal(r.err)
				}
				want := totals{Objects: 2, Versions: 2}
				if kept {
					want = totals{Objects: 2, Versions: 3, StoredBytes: 1000}
				}
				if usage := checkUsage(t, m.f, "b"); usage.UsageTotals != want {
					t.Fatalf("after a version of -5 bytes over one of 1000 the bucket holds %+v, want %+v", usage.UsageTotals, want)
				}
				if r := put("fill", 1000, 11); kept != errors.Is(r.err, domain.ErrQuotaExceeded) || (!kept && r.err != nil) {
					t.Errorf("1000 bytes into a bucket that holds %d = %v", want.StoredBytes, r.err)
				}
				checkUsage(t, m.f, "b")
				if usage, drift := m.walk("b", 100, true); drift || usage.Walking() {
					t.Errorf("the last recount: drift %t, %+v", drift, usage)
				}
				if m.f.usageClamped.Load() != 0 || m.f.usageDrift.Load() != 0 {
					t.Errorf("%d changes held at a bound, %d recounts that found drift", m.f.usageClamped.Load(), m.f.usageDrift.Load())
				}
			})
		}
	}
}

// TestAStoredPartOfASizeBelowNothingCountsAsNothing: apply refuses a part of
// a size below nothing, so a state holds one only if a release from before
// that did not. It counts for nothing in the walk, and for nothing when it is
// replaced and when its upload is aborted.
func TestAStoredPartOfASizeBelowNothingCountsAsNothing(t *testing.T) {
	t.Parallel()
	m := newMachine5(t)
	m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "old", CreatedAt: m.at}})
	u, parts, entries := uploadEntries("old", "up", 1, 100, 200)
	for _, e := range entries {
		if r := m.at4(e); r.err != nil {
			t.Fatal(r.err)
		}
	}
	if r := m.at5(partEntry(u, porttest.SamplePart(u, 3, -1, 2))); !errors.Is(r.err, domain.ErrEntityTooLarge) {
		t.Fatalf("a part of -1 bytes = %v", r.err)
	}
	if err := m.f.db.Update(func(tx *bbolt.Tx) error {
		stored := partToRecord(parts[0])
		stored.Size = -50
		data, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		return tx.Bucket(partsBucket()).Bucket([]byte("old")).Put(partKey(u.ID, 1), data)
	}); err != nil {
		t.Fatal(err)
	}
	usage, _ := m.walk("old", 100, false)
	if !usage.Exact || usage.UsageTotals != (bucket.UsageTotals{UploadBytes: 200}) {
		t.Fatalf("the walk counted %+v", usage)
	}
	m.ok5(partEntry(u, porttest.SamplePart(u, 1, 10, 3)))
	if usage := checkUsage(t, m.f, "old"); usage.UploadBytes != 210 {
		t.Fatalf("a part of 10 bytes in the place of one of -50: %+v", usage.UsageTotals)
	}
	if err := m.f.db.Update(func(tx *bbolt.Tx) error {
		stored := partToRecord(parts[1])
		stored.Size = -50
		data, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		return tx.Bucket(partsBucket()).Bucket([]byte("old")).Put(partKey(u.ID, 2), data)
	}); err != nil {
		t.Fatal(err)
	}
	// The record still says 210: the 200 of the part that was changed behind
	// its back are a fault, which the recount finds; the abort after it
	// takes away what the parts count for, and leaves nothing.
	if usage, drift := m.walk("old", 100, true); !drift || usage.UploadBytes != 10 {
		t.Fatalf("the recount: drift %t, %+v", drift, usage.UsageTotals)
	}
	m.ok5(abortEntry(u))
	if usage := checkUsage(t, m.f, "old"); usage.UsageTotals != (bucket.UsageTotals{}) || m.f.usageClamped.Load() != 0 {
		t.Errorf("after the abort: %+v, %d changes held at a bound", usage.UsageTotals, m.f.usageClamped.Load())
	}
}

// plantVersion stores a version under a key behind the back of the state
// machine - as the current version of the key, or among its older ones - with
// its reference to its data: what no entry can do for a key that apply does
// not accept, and a release from before keys were validated there could.
func plantVersion(t *testing.T, f *fsm, bucketName, key string, size int64, serial int, older bool) {
	t.Helper()
	v := *sized(bucketName, key, size, serial)
	if err := f.db.Update(func(tx *bbolt.Tx) error {
		if err := addDataReference(tx.Bucket(dataBucket()), v.DataID, 1); err != nil {
			return err
		}
		h, err := openHistory(tx, bucketName, older)
		if err != nil {
			return err
		}
		if older {
			v.Versioned, v.Seq = true, 1
			return h.putOlder(v)
		}
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return h.current.Put([]byte(key), data)
	}); err != nil {
		t.Fatal(err)
	}
}

// TestNoStoredKeyStopsTheWalk: a state may hold a key that can be no cursor
// of the walk - no UTF-8, or longer than a key can be - stored by a release
// from before keys were validated in apply. The walk is not stopped by one:
// a step never ends on such a key but goes on to the next that can be a
// cursor, and ends the keys when none follows. Such keys at the start, in
// the middle and at the end of a bucket, alone in one, among the older
// versions and with nothing but older versions; steps of one record; and
// between the steps deletes of such keys behind the walk and before it, and
// of the key that was to be its next cursor. After every step the count is a
// recount of what lies, byte for byte, at or before the cursor; two replicas
// hold the same bytes; and the end is exact, and a recount finds no drift.
func TestNoStoredKeyStopsTheWalk(t *testing.T) {
	t.Parallel()
	long := func(first string) string { return strings.Repeat(first, object.MaxKeyLength+76) }
	such := []string{"\x01\xff", long("a"), "c\xff", long("c"), "x\xff", "z\xff", long("z"), "\xff"}
	for _, key := range append([]string{"b", "m", "y", strings.Repeat("l", object.MaxKeyLength)}, such...) {
		// What the walk takes for a cursor is what the domain takes for one.
		_, err := bucket.Usage{}.Counting().Step(bucket.UsageStep{Cursor: key})
		if cursorKey(key) != (err == nil) || cursorKey(key) == slices.Contains(such, key) {
			t.Fatalf("%q can be a cursor: %t; a step that names it = %v", key, cursorKey(key), err)
		}
	}
	// deleteOld is the delete of schema 1, which names a key without asking
	// whether it is one.
	deleteOld := func(key string) entry {
		return entry{Type: cmdDeleteObject, DeleteObject: &deleteObjectCmd{Bucket: "old", Key: key}}
	}
	// asWritten applies an entry as a proposer of level 4 writes it, and the
	// delete of schema 1 in its own schema.
	asWritten := func(m *machine, e entry) applyResult {
		if e.DeleteObject != nil {
			return m.as(entrySchemaVersion, e)
		}
		held, schema := written(e, entrySchema4)
		return m.as(schema, held)
	}
	for name, test := range map[string]struct {
		kept  bool
		valid []string
		// cursors are where the walk must stand after each step that
		// leaves it among the keys, and between is what is applied after
		// the step of that number.
		cursors []string
		between map[int][]entry
		// gone are the keys the entries between the steps delete.
		gone []string
	}{
		"at the start, in the middle and at the end": {
			valid: []string{"b", "m", "y"}, cursors: []string{"b", "m", "y"},
		},
		"deleted behind the walk and before it": {
			valid: []string{"b", "m", "y"}, cursors: []string{"b", "m", "y"},
			between: map[int][]entry{
				1: {deleteOld(long("a")), deleteOld(long("z"))},
				2: {deleteOld(long("c"))},
			},
			gone: []string{long("a"), long("z"), long("c")},
		},
		"the last key that could be a cursor deleted before the walk came to it": {
			valid: []string{"b", "m", "y"}, cursors: []string{"b", "m"},
			between: map[int][]entry{2: {deleteOld("y"), deleteOld(long("z"))}},
			gone:    []string{"y", long("z")},
		},
		"nothing but such keys": {},
		"where versions are kept": {
			kept: true, valid: []string{"b", "m", "y"}, cursors: []string{"b", "m", "n", "y"},
			between: map[int][]entry{
				1: {putEntry("old", "b", 7, 900), deleteEntry("old", "y", "", 901)},
				2: {putEntry("old", "a", 9, 902), putEntry("old", "n", 11, 903)},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			replicas := []*machine{newMachine5(t), newMachine5(t)}
			both := func(do func(m *machine)) {
				t.Helper()
				for _, m := range replicas {
					do(m)
				}
				if got, want := wholeState(t, replicas[1].f), wholeState(t, replicas[0].f); !reflect.DeepEqual(got, want) {
					t.Fatal("the two replicas hold different states")
				}
			}
			versions := 0
			both(func(m *machine) {
				m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "old", CreatedAt: m.at}})
				if test.kept {
					m.ok(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "old", Status: string(bucket.VersioningEnabled)}})
				}
				serial := 0
				for _, key := range test.valid {
					serial++
					if r := m.at4(putEntry("old", key, int64(10*serial), serial)); r.err != nil {
						t.Fatal(r.err)
					}
				}
				for i, key := range such {
					serial++
					// Where versions are kept: some keys with a current
					// version, some with older ones besides, some with
					// nothing but older ones.
					if !test.kept || i%3 != 2 {
						plantVersion(t, m.f, "old", key, int64(100+serial), serial, false)
					}
					if test.kept && i%3 != 0 {
						plantVersion(t, m.f, "old", key, int64(200+serial), 100+serial, true)
						plantVersion(t, m.f, "old", key, int64(300+serial), 200+serial, true)
					}
				}
				versions = len(everyVersion(t, m.f, "old"))
			})
			if want := len(test.valid) + len(such); !test.kept && versions != want {
				t.Fatalf("the bucket holds %d versions, %d were stored", versions, want)
			}
			// No entry of schema 2 and above names a key that is not one,
			// and none of any schema can say bytes that are no UTF-8: an
			// entry is JSON, and names another key in their place. So the
			// only change such a key can see is the delete of schema 1 of
			// one that is too long, in a bucket without versions.
			if data, err := encodeEntry(deleteOld("c\xff"), entrySchemaVersion); err != nil || bytes.Contains(data, []byte("\xff")) || !bytes.Contains(data, []byte("\"key\":\"c\ufffd\"")) {
				t.Fatalf("an entry that names bytes that are no UTF-8 is written as %s, %v", data, err)
			}
			both(func(m *machine) {
				before := wholeState(t, m.f)
				for what, e := range map[string]entry{
					"a delete of a key that is too long":            deleteEntry("old", long("c"), "", 800),
					"a put of a key that is too long":               putEntry("old", long("c"), 1, 801),
					"a delete of schema 1 of what no entry can say": deleteOld("c\xff"),
					"a delete of schema 1 of what is not held":      deleteOld("nothing"),
				} {
					if test.kept && e.DeleteObject != nil {
						continue
					}
					if r := asWritten(m, e); r.previous != nil || r.stored != nil || r.marker != nil {
						t.Fatalf("%s took effect: %+v", what, r)
					}
				}
				after := wholeState(t, m.f)
				// But for what every entry leaves: where the log is, its
				// time, and the schema of the state.
				for _, state := range []map[string]string{before, after} {
					maps.DeleteFunc(state, func(key, _ string) bool { return strings.HasPrefix(key, "meta/") })
				}
				if !reflect.DeepEqual(after, before) {
					t.Fatal("entries that name what is no key changed the state")
				}
			})
			var cursors []string
			for step := 1; ; step++ {
				var usage bucket.Usage
				both(func(m *machine) {
					usage, _ = m.walk("old", 1, false)
					checkUsage(t, m.f, "old")
				})
				if !usage.Walking() || usage.Count.Phase != bucket.UsagePhaseKeys {
					break
				}
				cursors = append(cursors, usage.Count.Cursor)
				if step > len(test.cursors) {
					t.Fatalf("the walk was to stand at %q and has taken %d steps: %q", test.cursors, step, cursors)
				}
				both(func(m *machine) {
					for _, e := range test.between[step] {
						r := asWritten(m, e)
						if r.err != nil || (e.DeleteObject != nil && r.previous == nil) {
							t.Fatalf("%s between the steps = %v, removed %+v", e.Type, r.err, r.previous)
						}
						checkUsage(t, m.f, "old")
					}
				})
			}
			// The step that visits the last key that can be a cursor ends
			// the keys only when nothing follows it; here something does,
			// so every such key is shown as a cursor once - but for the one
			// that was deleted before the walk came to it.
			if !slices.Equal(cursors, test.cursors) {
				t.Errorf("the walk stood at %q, want %q", cursors, test.cursors)
			}
			var end bucket.Usage
			both(func(m *machine) {
				for end = checkUsage(t, m.f, "old"); end.Walking(); end = checkUsage(t, m.f, "old") {
					m.walk("old", 1, false)
				}
			})
			if !end.Exact {
				t.Fatalf("the walk did not end: %+v", end)
			}
			if !test.kept {
				if want := int64(len(test.valid) + len(such) - len(test.gone)); end.Versions != want || end.Objects != want {
					t.Errorf("the bucket holds %d versions of %d objects, want %d", end.Versions, end.Objects, want)
				}
			}
			both(func(m *machine) {
				usage, drift := m.walk("old", 1, true)
				for usage.Walking() && !drift {
					usage, drift = m.walk("old", 1, false)
					checkUsage(t, m.f, "old")
				}
				if drift || usage.UsageTotals != end.UsageTotals {
					t.Errorf("a recount: drift %t, %+v; the walk had come to %+v", drift, usage.UsageTotals, end.UsageTotals)
				}
				if m.f.usageClamped.Load() != 0 || m.f.usageDrift.Load() != 0 {
					t.Errorf("%d changes held at a bound, %d recounts that found drift", m.f.usageClamped.Load(), m.f.usageDrift.Load())
				}
			})
		})
	}
}
