package raftmeta

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/domain/tagging"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// Entry and state schema 4 (ADR-0019): who created a bucket, and the record
// of a bucket's latest uploads and downloads that every member keeps from
// entries written as schema 4.

// at4 applies an entry as the leader of a cluster at level 4 writes it: in
// the schema writtenAt gives it there. (apply is the leader of a cluster at
// level 3: every entry in the oldest schema that can say what it says.)
func (m *machine) at4(e entry) applyResult {
	m.t.Helper()
	r := m.as(max(writtenAt(e, versionOf(e), entrySchema4), entrySchemaTimed), e)
	if r.err != nil {
		m.t.Fatalf("%s: %v", e.Type, r.err)
	}
	return r
}

// put4 commits a version by somebody, as a cluster at level 4 does.
func (m *machine) put4(bucketName, key, by string, fill byte) versionRecord {
	m.t.Helper()
	v := porttest.SampleVersion(bucketName, key, fill)
	v.CreatedBy = by
	return *m.at4(entry{Type: cmdPutObject, PutObject: versionToRecord(v)}).stored
}

// download is one item of a batch of reads: so many downloads of a version
// by a reader.
func download(v versionRecord, who string, presigned bool, downloads, copies int64) readItem {
	return readItem{
		Bucket: v.Bucket, Key: v.Key, Version: refOf(v), VersionID: v.VersionID, Who: who, Presigned: presigned, Downloads: downloads, Copies: copies,
	}
}

// reads4 applies a batch of reads as a cluster at level 4 writes it.
func (m *machine) reads4(node string, seq uint64, items ...readItem) {
	m.t.Helper()
	m.at4(entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: node, Boot: 1, Seq: seq, Items: items}})
}

// upload4 makes a version of key by a multipart upload of one part, each
// entry as a cluster at level writes it, and returns the version.
func (m *machine) upload4(bucketName, key, by string, fill byte, level byte) versionRecord {
	m.t.Helper()
	u := porttest.SampleUpload(bucketName, key, fill)
	part := porttest.SamplePart(u, 1, 10, fill)
	var stored *versionRecord
	for _, e := range []entry{
		{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)},
		{Type: cmdPutPart, PutPart: &putPartCmd{Bucket: bucketName, Key: key, UploadID: u.ID.String(), Part: partToRecord(part)}},
		{Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{
			Bucket: bucketName, Key: key, UploadID: u.ID.String(), VersionID: object.ID(bytes.Repeat([]byte{fill}, object.IDSize)).String(),
			ETag: "etag", CreatedBy: by,
			Parts: []listedPart{{Number: 1, ETag: part.ETag, DataID: part.DataID.String(), Seal: bytes.Repeat([]byte{1}, 32)}},
		}},
	} {
		held := heldTo(e, level)
		r := m.as(max(writtenAt(held, versionOf(held), level), entrySchemaTimed), held)
		if r.err != nil {
			m.t.Fatalf("%s: %v", e.Type, r.err)
		}
		stored = r.stored
	}
	return *stored
}

// activityBytes returns the record of a bucket's latest use as it is stored;
// nil when there is none.
func activityBytes(t *testing.T, f *fsm, name string) []byte {
	t.Helper()
	var raw []byte
	if err := f.view(func(tx *bbolt.Tx) error {
		if records := tx.Bucket(bucketActivityBucket()); records != nil {
			raw = bytes.Clone(records.Get([]byte(name)))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return raw
}

// activityOf reads the record of a bucket's latest use from the state, and
// puts it in words: the keys and who, newest first.
func activityOf(t *testing.T, f *fsm, name string) (uploads, downloads string) {
	t.Helper()
	var a bucket.Activity
	if err := f.view(func(tx *bbolt.Tx) (err error) {
		a, _, err = readBucketActivity(tx.Bucket(bucketActivityBucket()), name)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return activityWords(a)
}

func activityWords(a bucket.Activity) (uploads, downloads string) {
	var up, down []string
	for _, u := range a.Uploads {
		up = append(up, u.Key+" by "+u.By)
	}
	for _, d := range a.Downloads {
		down = append(down, fmt.Sprintf("%s by %s presigned=%t", d.Key, d.By, d.Presigned))
	}
	return strings.Join(up, "; "), strings.Join(down, "; ")
}

// storedBucket returns the record of a bucket as it is stored.
func storedBucket(t *testing.T, f *fsm, name string) []byte {
	t.Helper()
	var raw []byte
	if err := f.view(func(tx *bbolt.Tx) error {
		raw = bytes.Clone(tx.Bucket(bucketsBucket()).Get([]byte(name)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return raw
}

// everyCommand is one entry of every command there is, and of every old
// command in each of the forms that needs a newer schema, by name.
func everyCommand() map[string]entry {
	plain := porttest.SampleVersion("b", "k", 1)
	by := plain
	by.CreatedBy = "user/albie"
	upload := porttest.SampleUpload("b", "k", 1)
	tagged := upload
	tagged.Tags = map[string]string{"stage": "draft"}
	commands := map[string]entry{
		"bucket.create":                  {Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b"}},
		"bucket.create with Object Lock": {Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", ObjectLock: true}},
		"bucket.create that says who":    {Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedBy: "user/albie"}},
		"bucket.create with Object Lock that says who": {
			Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", ObjectLock: true, CreatedBy: "root"},
		},
		"bucket.delete":                             {Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "b"}},
		"object.put":                                {Type: cmdPutObject, PutObject: versionToRecord(plain)},
		"object.put that says who":                  {Type: cmdPutObject, PutObject: versionToRecord(by)},
		"object.delete":                             {Type: cmdDeleteObject, DeleteObject: &deleteObjectCmd{Bucket: "b", Key: "k"}},
		"root.set":                                  {Type: cmdSetRoot, SetRoot: &rootRecord{AccessKeyID: "ROOT"}},
		"topology.set":                              {Type: cmdSetTopology, SetTopology: &topologyRecord{}},
		"node.set":                                  {Type: cmdSetNode, SetNode: &nodeRecord{ID: "n1", MetadataVersion: entrySchema4}},
		"node.remove":                               {Type: cmdRemoveNode, RemoveNode: &removeNodeCmd{ID: "n1"}},
		"iam.user.put":                              {Type: cmdPutUser, IAM: &iamCmd{User: &userRecord{Name: "albie"}}},
		"audit.append":                              {Type: cmdAppendAudit, IAM: &iamCmd{Audit: &auditRecord{Actor: "root"}}},
		"bucket.versioning.put":                     {Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "b", Status: "Enabled"}},
		"object.version.delete":                     {Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "b", Key: "k"}},
		"bucket.lock.put":                           {Type: cmdSetBucketLock, SetBucketLock: &bucketLockCmd{Bucket: "b"}},
		"object.retention.put":                      {Type: cmdSetRetention, SetRetention: &retentionCmd{Bucket: "b", Key: "k"}},
		"object.legalhold.put":                      {Type: cmdSetLegalHold, SetLegalHold: &legalHoldCmd{Bucket: "b", Key: "k"}},
		"object.tags.put":                           {Type: cmdSetTags, SetTags: &tagsCmd{Bucket: "b", Key: "k", Tags: map[string]string{"a": "b"}}},
		"upload.create":                             {Type: cmdCreateUpload, CreateUpload: uploadToRecord(upload)},
		"upload.create with tags":                   {Type: cmdCreateUpload, CreateUpload: uploadToRecord(tagged)},
		"upload.part.put":                           {Type: cmdPutPart, PutPart: &putPartCmd{Bucket: "b", Key: "k"}},
		"upload.complete":                           {Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{Bucket: "b", Key: "k"}},
		"upload.complete that says who":             {Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{Bucket: "b", Key: "k", CreatedBy: "key/AKID"}},
		"upload.abort":                              {Type: cmdAbortUpload, AbortUpload: &uploadRefCmd{Bucket: "b", Key: "k"}},
		"bucket.config.put of a policy":             {Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Bucket: "b", Kind: bucket.ConfigPolicy, Document: []byte("{}")}},
		"bucket.config.put of tag rules":            {Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Bucket: "b", Kind: bucket.ConfigTagRules, Document: []byte("{}")}},
		"object.reads.add":                          {Type: cmdAddReads, AddReads: &addReadsCmd{Node: "n1"}},
		"subbucket.put":                             {Type: cmdPutSubBucket, PutSubBucket: &subBucketCmd{Bucket: "b"}},
		"subbucket.delete":                          {Type: cmdDeleteSubBucket, DeleteSubBucket: &subBucketRefCmd{Bucket: "b"}},
		"tag.definition.put":                        {Type: cmdPutTagDefinition, PutTagDefinition: &tagDefinitionCmd{}},
		"tag.definition.delete":                     {Type: cmdDeleteTagDefinition, DeleteTagDefinition: &tagDefinitionRefCmd{Key: "k"}},
		"a command of the first schema with a time": {Type: cmdDeleteBucket, At: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), DeleteBucket: &deleteBucketCmd{Name: "b"}},
	}
	return commands
}

// TestSchema4OfEveryCommand: for every command there is, at the level of
// the release before and at this one - which schema it needs (versionOf),
// what of it is written at the level (heldTo) and in which schema
// (writtenAt). Schema 4 is three things and no more: a bucket that says who
// created it needs it, and at level 3 goes without; a put, a completion and a
// batch of reads are written in it once the level is 4, and below as they
// always were; and everything else is written exactly as before, at either
// level.
func TestSchema4OfEveryCommand(t *testing.T) {
	t.Parallel()
	// needs is what versionOf gives the entry; at3 and at4 the schema it is
	// written in at the two levels.
	want := map[string]struct{ needs, at3, at4 byte }{
		"bucket.create":                                {entrySchemaVersion, entrySchemaVersion, entrySchemaVersion},
		"bucket.create with Object Lock":               {entrySchemaTimed, entrySchemaTimed, entrySchemaTimed},
		"bucket.create that says who":                  {entrySchema4, entrySchemaVersion, entrySchema4},
		"bucket.create with Object Lock that says who": {entrySchema4, entrySchemaTimed, entrySchema4},
		"bucket.delete":                                {entrySchemaVersion, entrySchemaVersion, entrySchemaVersion},
		"object.put":                                   {entrySchemaVersion, entrySchemaVersion, entrySchema4},
		"object.put that says who":                     {entrySchema3, entrySchema3, entrySchema4},
		"object.delete":                                {entrySchemaVersion, entrySchemaVersion, entrySchemaVersion},
		"root.set":                                     {entrySchemaVersion, entrySchemaVersion, entrySchemaVersion},
		"topology.set":                                 {entrySchemaVersion, entrySchemaVersion, entrySchemaVersion},
		"node.set":                                     {entrySchemaVersion, entrySchemaVersion, entrySchemaVersion},
		"node.remove":                                  {entrySchemaVersion, entrySchemaVersion, entrySchemaVersion},
		"iam.user.put":                                 {entrySchemaVersion, entrySchemaVersion, entrySchemaVersion},
		"audit.append":                                 {entrySchemaVersion, entrySchemaVersion, entrySchemaVersion},
		"bucket.versioning.put":                        {entrySchemaTimed, entrySchemaTimed, entrySchemaTimed},
		"object.version.delete":                        {entrySchemaTimed, entrySchemaTimed, entrySchemaTimed},
		"bucket.lock.put":                              {entrySchemaTimed, entrySchemaTimed, entrySchemaTimed},
		"object.retention.put":                         {entrySchemaTimed, entrySchemaTimed, entrySchemaTimed},
		"object.legalhold.put":                         {entrySchemaTimed, entrySchemaTimed, entrySchemaTimed},
		"object.tags.put":                              {entrySchemaTimed, entrySchemaTimed, entrySchemaTimed},
		"upload.create":                                {entrySchemaTimed, entrySchemaTimed, entrySchemaTimed},
		"upload.create with tags":                      {entrySchema3, entrySchema3, entrySchema3},
		"upload.part.put":                              {entrySchemaTimed, entrySchemaTimed, entrySchemaTimed},
		"upload.complete":                              {entrySchemaTimed, entrySchemaTimed, entrySchema4},
		"upload.complete that says who":                {entrySchema3, entrySchema3, entrySchema4},
		"upload.abort":                                 {entrySchemaTimed, entrySchemaTimed, entrySchemaTimed},
		"bucket.config.put of a policy":                {entrySchemaTimed, entrySchemaTimed, entrySchemaTimed},
		"bucket.config.put of tag rules":               {entrySchema3, entrySchema3, entrySchema3},
		"object.reads.add":                             {entrySchema3, entrySchema3, entrySchema4},
		"subbucket.put":                                {entrySchema3, entrySchema3, entrySchema3},
		"subbucket.delete":                             {entrySchema3, entrySchema3, entrySchema3},
		"tag.definition.put":                           {entrySchema3, entrySchema3, entrySchema3},
		"tag.definition.delete":                        {entrySchema3, entrySchema3, entrySchema3},
		"a command of the first schema with a time":    {entrySchemaTimed, entrySchemaTimed, entrySchemaTimed},
	}
	commands := everyCommand()
	if len(commands) != len(want) {
		t.Fatalf("%d commands and %d expectations", len(commands), len(want))
	}
	for name, e := range commands {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			expect, ok := want[name]
			if !ok {
				t.Fatal("nothing is expected of this command")
			}
			if got := versionOf(e); got != expect.needs {
				t.Errorf("versionOf = %d, want %d", got, expect.needs)
			}
			before, _ := json.Marshal(e)
			for level, written := range map[byte]byte{entrySchema3: expect.at3, entrySchema4: expect.at4} {
				held := heldTo(e, level)
				got := writtenAt(held, versionOf(held), level)
				if got != written {
					t.Errorf("at level %d written as schema %d, want %d", level, got, written)
				}
				if got > level {
					t.Errorf("at level %d written as schema %d, which a member does not know", level, got)
				}
				// Who created a bucket is the one thing an entry loses
				// below level 4, and it loses nothing at level 4.
				after, _ := json.Marshal(held)
				lost := !bytes.Equal(before, after)
				if wantLost := level < entrySchema4 && e.CreateBucket != nil && e.CreateBucket.CreatedBy != ""; lost != wantLost {
					t.Errorf("at level %d the entry is written as %s, was %s", level, after, before)
				}
				if held.CreateBucket != nil && level < entrySchema4 && held.CreateBucket.CreatedBy != "" {
					t.Errorf("at level %d a bucket is still created by %q", level, held.CreateBucket.CreatedBy)
				}
				// And it is written and read there by this binary, and
				// never read by the release before when it is of schema 4.
				data, err := encodeEntry(held, got)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := decodeEntry(data, MetadataVersion); err != nil {
					t.Errorf("at level %d the entry does not decode in its own schema: %v", level, err)
				}
				if _, err := decodeEntry(data, entrySchema3); (err == nil) != (got < entrySchema4) {
					t.Errorf("at level %d, written as schema %d, read by a binary of schema 3 = %v", level, got, err)
				}
			}
			if after, _ := json.Marshal(e); !bytes.Equal(before, after) {
				t.Errorf("heldTo changed the caller's entry:\n%s\n%s", before, after)
			}
			// Below schema 3 nothing changed: what writtenAt gives is what
			// versionOf gave before there was a schema 4.
			for _, level := range []byte{entrySchemaVersion, entrySchemaTimed} {
				held := heldTo(e, level)
				if got := writtenAt(held, versionOf(held), level); got != versionOf(held) || got == entrySchema4 {
					t.Errorf("at level %d written as schema %d, the entry needs %d", level, got, versionOf(held))
				}
			}
		})
	}
	// An entry of a schema this binary does not know is refused, as before.
	for name, e := range commands {
		data, err := encodeEntry(e, MetadataVersion+1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeEntry(data, MetadataVersion); !errors.Is(err, domain.ErrUnsupportedFormat) {
			t.Errorf("%s in an entry of schema %d = %v, want %v", name, MetadataVersion+1, err, domain.ErrUnsupportedFormat)
		}
	}
}

// TestAnEntryBelowSchema4SaysNothingOfWhoCreatedABucket: entries are decoded
// leniently, so a bucket.create that brings created_by in an entry that
// claims schema 3 - or 2, or 1 - is read by the binary of that schema without
// it. This binary stores exactly the bytes that one stores: the bucket record
// as the release of schema 3 (commit d2d6861) writes it, which are pinned
// here. Only an entry of schema 4 says who.
func TestAnEntryBelowSchema4SaysNothingOfWhoCreatedABucket(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	// What the release of schema 3 stores for these buckets, byte for byte.
	const (
		plain  = `{"name":"b","created_at":"2026-10-12T08:00:00Z"}`
		locked = `{"name":"b","created_at":"2026-10-12T08:00:00Z","versioning":"Enabled","object_lock":true}`
	)
	tests := map[string]struct {
		schema byte
		lock   bool
		// supported is the newest schema the state machine applies: this
		// binary's, or that of a store held to schema 3.
		supported byte
		want      string
	}{
		"schema 1":                       {entrySchemaVersion, false, MetadataVersion, plain},
		"schema 2":                       {entrySchemaTimed, false, MetadataVersion, plain},
		"schema 2, with Object Lock":     {entrySchemaTimed, true, MetadataVersion, locked},
		"schema 3":                       {entrySchema3, false, MetadataVersion, plain},
		"schema 3, with Object Lock":     {entrySchema3, true, MetadataVersion, locked},
		"schema 3, on a store held to 3": {entrySchema3, false, entrySchema3, plain},
		"schema 2, on a store held to 3": {entrySchemaTimed, true, entrySchema3, locked},
		"schema 4":                       {entrySchema4, false, MetadataVersion, `{"name":"b","created_at":"2026-10-12T08:00:00Z","created_by":"user/albie"}`},
		"schema 4, with Object Lock":     {entrySchema4, true, MetadataVersion, `{"name":"b","created_at":"2026-10-12T08:00:00Z","versioning":"Enabled","object_lock":true,"created_by":"user/albie"}`},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stored [2][]byte
			for i, by := range []string{"user/albie", ""} {
				m := newMachine(t)
				m.f.supported = test.supported
				r := m.as(test.schema, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedAt: created, ObjectLock: test.lock, CreatedBy: by}})
				if r.err != nil {
					t.Fatal(r.err)
				}
				stored[i] = storedBucket(t, m.f, "b")
				wantState := uint64(min(max(test.schema, stateSchemaVersion), stateSchema4))
				if got := stateSchema(t, m.f); got != wantState {
					t.Errorf("created by %q: state schema %d, want %d", by, got, wantState)
				}
			}
			if string(stored[0]) != test.want {
				t.Errorf("the bucket of an entry that says who is stored as\n%s\nwant\n%s", stored[0], test.want)
			}
			if below := test.schema < entrySchema4; below != bytes.Equal(stored[0], stored[1]) {
				t.Errorf("with a creator: %s\nwithout:        %s", stored[0], stored[1])
			}
		})
	}

	// What follows keeps what was stored: a bucket of schema 4 is still its
	// creator's after its record was rewritten, and one without stays
	// without.
	m := newMachine(t)
	m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "with", CreatedAt: created, CreatedBy: "key/AKID"}})
	m.as(entrySchema3, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "without", CreatedAt: created, CreatedBy: "key/AKID"}})
	for _, name := range []string{"with", "without"} {
		m.ok(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: name, Status: string(bucket.VersioningEnabled)}})
		m.ok(entry{Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Bucket: name, Kind: bucket.ConfigPolicy, Document: []byte(`{}`)}})
	}
	for name, want := range map[string]string{"with": "key/AKID", "without": ""} {
		var b bucketRecord
		if err := json.Unmarshal(storedBucket(t, m.f, name), &b); err != nil || b.CreatedBy != want || b.Versioning != string(bucket.VersioningEnabled) || len(b.Config) != 1 {
			t.Errorf("bucket %s after changes = %+v, %v; want it created by %q", name, b, err, want)
		}
	}
	// A creator that cannot be kept is a refusal, never a stop: the entry
	// is in the log of every member, and all must go on alike.
	for what, by := range map[string]string{"too long": strings.Repeat("u", 257), "of two lines": "user/a\nuser/b"} {
		r := m.as(entrySchema4, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "refused", CreatedAt: created, CreatedBy: by}})
		if !errors.Is(r.err, domain.ErrInvalidArgument) || len(r.events) != 0 || storedBucket(t, m.f, "refused") != nil {
			t.Errorf("a creator %s = %v, events %d", what, r.err, len(r.events))
		}
	}
}

// TestStateBecomesSchema4WithItsFirstEntryOfSchema4: until an entry of
// schema 4 is applied the state stays what the release of schema 3 wrote -
// whatever this binary applies of the older schemas - and holds no record of
// any bucket's use. The first entry of schema 4 marks it, refused or not: the
// release of schema 3 would have stopped at it. Older entries that follow do
// not take that back, and it is there after a restart.
func TestStateBecomesSchema4WithItsFirstEntryOfSchema4(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		// first is the first entry of schema 4, and refused the refusal it
		// meets, if any.
		first   func(m *machine) entry
		refused error
	}{
		"a put": {first: func(*machine) entry {
			return entry{Type: cmdPutObject, PutObject: versionToRecord(porttest.SampleVersion("b", "first", 9))}
		}},
		"a put that is refused": {first: func(*machine) entry {
			return entry{Type: cmdPutObject, PutObject: versionToRecord(porttest.SampleVersion("missing", "first", 9))}
		}, refused: domain.ErrBucketNotFound},
		"a bucket with its creator": {first: func(m *machine) entry {
			return entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "second", CreatedAt: m.at, CreatedBy: "root"}}
		}},
		"a bucket that exists": {first: func(m *machine) entry {
			return entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedAt: m.at, CreatedBy: "root"}}
		}, refused: domain.ErrBucketAlreadyExists},
		"a batch of reads of a node that is not registered": {first: func(m *machine) entry {
			m.ok(entry{Type: cmdSetNode, SetNode: &nodeRecord{ID: "known", MetadataVersion: MetadataVersion}})
			return entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: "unknown", Boot: 1, Seq: 1}}
		}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMachine(t)
			// Everything of schema 3, as a cluster at level 3 writes it.
			versions := populateSchema3(m)
			m.bucket("b", bucket.Unversioned)
			m.put("b", "k", 30)
			m.upload4("b", "uploaded", "key/AKID", 31, entrySchema3)
			m.reads("node-a", 9, 1, "user/albie", versions[0])
			if got := stateSchema(t, m.f); got != stateSchema3 {
				t.Fatalf("after entries of schema 3: state schema %d", got)
			}
			if got := rawState(t, m.f, bucketActivityBucket()); len(got) != 0 {
				t.Fatalf("a state of schema 3 holds %v", got)
			}
			before := rawState(t, m.f, everyBucket(t, m.f)...)

			first := test.first(m)
			r := m.as(entrySchema4, first)
			if !errors.Is(r.err, test.refused) {
				t.Fatalf("the first entry of schema 4 = %v, want %v", r.err, test.refused)
			}
			if got := stateSchema(t, m.f); got != stateSchema4 {
				t.Fatalf("after an entry of schema 4: state schema %d", got)
			}
			if test.refused != nil {
				// Nothing but the mark: a refusal changes nothing else.
				after := rawState(t, m.f, everyBucket(t, m.f)...)
				delete(before, "meta/"+fmt.Sprintf("%x", schemaKey()))
				delete(before, "meta/"+fmt.Sprintf("%x", appliedKey()))
				delete(before, "meta/"+fmt.Sprintf("%x", timeKey()))
				for key, value := range before {
					if after[key] != value {
						t.Errorf("a refused entry of schema 4 changed %s", key)
					}
				}
				if got := rawState(t, m.f, bucketActivityBucket()); len(got) != 0 {
					t.Errorf("a refused entry of schema 4 left %v", got)
				}
			}
			// Entries of older schemas that follow do not take it back, and
			// write no record.
			m.as(entrySchemaVersion, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "one"}})
			m.as(entrySchemaTimed, entry{Type: cmdPutObject, PutObject: versionToRecord(porttest.SampleVersion("one", "k", 40))})
			m.as(entrySchema3, entry{Type: cmdPutObject, PutObject: versionToRecord(porttest.SampleVersion("one", "k3", 41))})
			if got := stateSchema(t, m.f); got != stateSchema4 {
				t.Errorf("after older entries the state schema went back to %d", got)
			}
			if got := activityBytes(t, m.f, "one"); got != nil {
				t.Errorf("entries of older schemas wrote the record %s", got)
			}
		})
	}
}

// TestAStateOfSchema4IsReopenedAndANewerOneRefused: the release of schema 4
// opens what it wrote; a state of a schema it does not know it refuses, as
// the release of schema 3 refuses that one. (The store is held to schema 4:
// on a node of this release on its own the bucket below is an entry of
// schema 5, ADR-0023. What this release opens and refuses is
// TestAStateOfSchema5IsOpenedOnlyByABinaryOfSchema5.)
func TestAStateOfSchema4IsReopenedAndANewerOneRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := t.Context()
	store := openStore(t, dir)
	asSchema4Release(store)
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b", CreatedBy: "user/albie"}); err != nil {
		t.Fatal(err)
	}
	v := porttest.SampleVersion("b", "k", 1)
	v.CreatedBy = "user/albie"
	if _, err := store.PutVersion(ctx, v); err != nil {
		t.Fatal(err)
	}
	want, err := store.BucketActivities(ctx, []string{"b"})
	if err != nil || len(want[0].Uploads) != 1 {
		t.Fatalf("BucketActivities = %+v, %v", want, err)
	}
	if got := stateSchema(t, store.fsm); got != stateSchema4 {
		t.Fatalf("state schema %d after a put on a node on its own, want %d", got, stateSchema4)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// The log is replayed over the state at the next start, and changes
	// nothing in it: the record is not written a second time.
	store = openStore(t, dir)
	asSchema4Release(store)
	got, err := store.BucketActivities(ctx, []string{"b"})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("after a restart = %+v, %v; want %+v", got, err, want)
	}
	if b, err := store.GetBucket(ctx, "b"); err != nil || b.CreatedBy != "user/albie" {
		t.Errorf("after a restart the bucket is created by %q, %v", b.CreatedBy, err)
	}
	if got := stateSchema(t, store.fsm); got != stateSchema4 {
		t.Errorf("state schema %d after a restart", got)
	}
	if err := store.fsm.db.Update(func(tx *bbolt.Tx) error { return raiseStateSchema(tx, stateSchema4+1) }); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if f, err := openFSMOf(root, entrySchema4); !errors.Is(err, domain.ErrUnsupportedFormat) {
		if f != nil {
			_ = f.close()
		}
		t.Errorf("opening a state of schema %d = %v, want %v", stateSchema4+1, err, domain.ErrUnsupportedFormat)
	}
}

// TestAStateMachineHeldToSchema3StopsAtAnEntryOfSchema4: the release of
// schema 3 does not apply an entry of schema 4 in some other way - not a put
// it could read every field of, either. It stops, with its state as it was,
// and stays stopped: what a put of schema 4 means, it does not know.
func TestAStateMachineHeldToSchema3StopsAtAnEntryOfSchema4(t *testing.T) {
	t.Parallel()
	v := porttest.SampleVersion("b", "of-schema-4", 2)
	v.CreatedBy = "user/albie"
	for name, e := range map[string]entry{
		"a put":                     {Type: cmdPutObject, PutObject: versionToRecord(v)},
		"a completion":              {Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{Bucket: "b", Key: "k", CreatedBy: "root"}},
		"a batch of reads":          {Type: cmdAddReads, AddReads: &addReadsCmd{Node: "n1", Boot: 1, Seq: 1}},
		"a bucket with its creator": {Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "c", CreatedBy: "root"}},
		"a bucket without":          {Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "c"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMachine(t)
			m.f.supported = entrySchema3
			m.bucket("b", bucket.Unversioned)
			m.put("b", "k", 1)
			before := rawState(t, m.f, everyBucket(t, m.f)...)
			if r := m.raw(entrySchema4, e); !r.fatal || !errors.Is(r.err, domain.ErrUnsupportedFormat) {
				t.Fatalf("an entry of schema 4 on a state machine of schema 3 = %+v, want it to stop with %v", r.err, domain.ErrUnsupportedFormat)
			}
			if !errors.Is(m.f.health(), domain.ErrUnsupportedFormat) {
				t.Errorf("health = %v", m.f.health())
			}
			if after := rawState(t, m.f, everyBucket(t, m.f)...); !reflect.DeepEqual(after, before) {
				t.Errorf("the entry it stopped at changed the state:\n%v\n%v", after, before)
			}
			if got := stateSchema(t, m.f); got != stateSchemaTimed {
				t.Errorf("state schema %d", got)
			}
			// And nothing after it is applied.
			if r := m.raw(entrySchemaTimed, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "later"}}); !r.fatal {
				t.Errorf("an entry after the one it stopped at = %+v", r)
			}
		})
	}
	// An entry of a schema this binary does not know stops it in the same
	// way.
	m := newMachine(t)
	if r := m.raw(MetadataVersion+1, entry{Type: cmdPutObject, PutObject: versionToRecord(v)}); !r.fatal || !errors.Is(r.err, domain.ErrUnsupportedFormat) {
		t.Errorf("an entry of schema %d = %+v, want the state machine to stop", MetadataVersion+1, r.err)
	}
}

// populateSchema4 writes what schema 4 keeps, as a cluster at level 4 does:
// buckets with their creators, uploads by puts and by a completion, and
// downloads by several readers from several nodes - more of each than a
// record holds. It returns the versions that were read.
func populateSchema4(m *machine) []versionRecord {
	m.t.Helper()
	m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "plain", CreatedAt: m.at, CreatedBy: "user/albie"}})
	m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "ver", CreatedAt: m.at, CreatedBy: "root", ObjectLock: true}})
	m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "quiet", CreatedAt: m.at}})
	versions := []versionRecord{
		m.put4("plain", "a", "user/albie", 1), m.put4("plain", "reports/q3", "key/AKID", 2),
		m.put4("ver", "k", "root", 3), m.put4("ver", "k", "", 4), m.put4("plain", "c", "user/bob", 5), m.put4("plain", "a", "user/carol", 6),
	}
	versions = append(versions, m.upload4("plain", "large", "key/AKID", 7, entrySchema4))
	for i := range 5 {
		m.reads4("node-a", uint64(i+1),
			download(versions[1], fmt.Sprintf("user/%02d", i), false, 1, 0),
			download(versions[2], "anonymous", false, 2, 0),
			download(versions[3], "key/AKID", i%2 == 0, 1, 1),
			// The source of a copy, and a read of nothing there is.
			download(versions[4], "user/copier", false, 0, 1),
			readItem{Bucket: "none", Key: "k", Version: object.NullVersion, VersionID: versions[0].VersionID, Who: "user/x", Downloads: 1},
		)
	}
	m.reads4("node-b", 1, download(versions[5], "root", false, 3, 0), download(versions[6], "root", true, 1, 0))
	return versions
}

// TestReplicasKeepIdenticalBucketActivity: the record of a bucket's latest
// use is computed from the entries alone - which were committed, in which
// order, in which schema, at which time of the leader - so two replicas that
// apply the same log hold the same bytes, and a third that is restored from a
// snapshot of one of them holds them too and goes on like the others. That
// includes a leader whose clock went back, and a batch that came twice.
func TestReplicasKeepIdenticalBucketActivity(t *testing.T) {
	t.Parallel()
	a, b := newMachine(t), newMachine(t)
	var newest time.Time
	for _, m := range []*machine{a, b} {
		versions := populateSchema4(m)
		// A leader with a slow clock: the state's time does not go back,
		// and so neither does the time of an upload or a download.
		newest = m.at
		m.at = m.at.Add(-time.Hour)
		m.put4("ver", "late", "user/late", 20)
		m.reads4("node-c", 1, download(versions[0], "user/late", false, 1, 0))
		m.reads4("node-c", 1, download(versions[0], "user/twice", false, 1, 0))
		m.at = m.at.Add(2 * time.Hour)
		m.reads4("node-c", 2, download(versions[2], "user/00", false, 1, 0))
	}
	names := [][]byte{bucketActivityBucket(), bucketsBucket()}
	want, got := rawState(t, a.f, names...), rawState(t, b.f, names...)
	if len(want) != 2+2+3 {
		t.Fatalf("the replicas hold %d places, want two records and three buckets: %v", len(want), want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("two replicas that applied the same entries hold different records:\n%v\n%v", want, got)
	}
	if all, other := rawState(t, a.f, everyBucket(t, a.f)...), rawState(t, b.f, everyBucket(t, b.f)...); !reflect.DeepEqual(all, other) {
		t.Fatal("the two replicas differ somewhere else in their state")
	}
	// And what they hold is what was uploaded and downloaded.
	if uploads, downloads := activityOf(t, a.f, "plain"); uploads != "large by key/AKID; a by user/carol; c by user/bob" ||
		downloads != "a by user/late presigned=false; large by root presigned=true; a by root presigned=false" {
		t.Errorf("the record of plain = %q and %q", uploads, downloads)
	}
	if uploads, downloads := activityOf(t, a.f, "ver"); uploads != "late by user/late; k by " ||
		downloads != "k by user/00 presigned=false; k by key/AKID presigned=true; k by anonymous presigned=false" {
		t.Errorf("the record of ver = %q and %q", uploads, downloads)
	}
	for _, name := range []string{"quiet", "none"} {
		if raw := activityBytes(t, a.f, name); raw != nil {
			t.Errorf("the record of %s = %s, want none", name, raw)
		}
	}
	// The upload stamped an hour early has the time of the entry before it.
	var record activityRecord
	if err := json.Unmarshal(activityBytes(t, a.f, "ver"), &record); err != nil || !record.Uploads[0].At.Equal(newest) {
		t.Errorf("an upload stamped by a slow clock was recorded at %v (%v), want the time of the entry before it, %v", record.Uploads[0].At, err, newest)
	}

	// A third replica is restored from a snapshot of the first.
	snap, err := a.f.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatal(err)
	}
	snap.Release()
	c := newMachine(t)
	c.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "discarded", CreatedAt: c.at, CreatedBy: "root"}})
	c.put4("discarded", "k", "root", 50)
	if err := c.f.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	c.index, c.at = a.index, a.at
	if restored := rawState(t, c.f, everyBucket(t, c.f)...); !reflect.DeepEqual(restored, rawState(t, a.f, everyBucket(t, a.f)...)) {
		t.Fatalf("the restored state differs from the one the snapshot was taken of")
	}
	if got := rawState(t, c.f, names...); !reflect.DeepEqual(got, want) {
		t.Fatalf("the restored replica holds other records:\n%v\n%v", got, want)
	}
	if schema := stateSchema(t, c.f); schema != stateSchema4 {
		t.Errorf("the restored state says schema %d", schema)
	}
	// All three go on alike.
	for _, m := range []*machine{a, b, c} {
		m.put4("quiet", "first", "user/albie", 60)
		m.put4("plain", "c", "root", 61)
		v := m.put4("ver", "k", "root", 62)
		m.reads4("node-a", 100, download(v, "user/new", false, 1, 0))
		// A bucket that is emptied and deleted takes its record with it.
		m.ok(entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "quiet", Key: "first", At: m.at}})
		m.ok(entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "quiet"}})
		if raw := activityBytes(t, m.f, "quiet"); raw != nil {
			t.Errorf("the record of a deleted bucket = %s", raw)
		}
	}
	want = rawState(t, a.f, everyBucket(t, a.f)...)
	for name, m := range map[string]*machine{"the second": b, "the restored": c} {
		if got := rawState(t, m.f, everyBucket(t, m.f)...); !reflect.DeepEqual(got, want) {
			t.Errorf("%s replica parted from the first after the snapshot", name)
		}
	}
	if uploads, _ := activityOf(t, c.f, "plain"); uploads != "c by root; large by key/AKID; a by user/carol" {
		t.Errorf("the restored replica's record of plain = %q", uploads)
	}
	// A replica that applied fewer entries does not hold the same: the
	// comparison shows something.
	d := newMachine(t)
	populateSchema4(d)
	if reflect.DeepEqual(rawState(t, d.f, bucketActivityBucket()), rawState(t, a.f, bucketActivityBucket())) {
		t.Error("a replica that applied fewer entries holds the same records")
	}
}

// TestBucketActivityIsKeptFromEntriesOfSchema4Only: the same puts,
// completions and batches of reads leave no record when they are written as
// a cluster at level 3 writes them, and the record when a cluster at level 4
// does. A member cannot know the level its leader wrote at; it reads the
// schema of the entry, which every member reads alike.
func TestBucketActivityIsKeptFromEntriesOfSchema4Only(t *testing.T) {
	t.Parallel()
	for _, level := range []byte{entrySchemaTimed, entrySchema3, entrySchema4} {
		t.Run(fmt.Sprintf("level %d", level), func(t *testing.T) {
			t.Parallel()
			m := newMachine(t)
			write := func(e entry) applyResult {
				t.Helper()
				held := heldTo(e, level)
				r := m.as(max(writtenAt(held, versionOf(held), level), entrySchemaTimed), held)
				if r.err != nil {
					t.Fatalf("%s: %v", e.Type, r.err)
				}
				return r
			}
			write(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedAt: m.at, CreatedBy: "user/albie"}})
			v := porttest.SampleVersion("b", "k", 1)
			v.CreatedBy = "user/albie"
			put := *write(entry{Type: cmdPutObject, PutObject: versionToRecord(v)}).stored
			m.upload4("b", "large", "key/AKID", 2, level)
			if level >= entrySchema3 {
				write(entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: "node-a", Boot: 1, Seq: 1, Items: []readItem{download(put, "anonymous", false, 1, 0)}}})
			}
			var b bucketRecord
			if err := json.Unmarshal(storedBucket(t, m.f, "b"), &b); err != nil {
				t.Fatal(err)
			}
			uploads, downloads := activityOf(t, m.f, "b")
			if level < entrySchema4 {
				if uploads != "" || downloads != "" || b.CreatedBy != "" {
					t.Errorf("at level %d: created by %q, uploads %q, downloads %q; want none of it", level, b.CreatedBy, uploads, downloads)
				}
				if got := rawState(t, m.f, bucketActivityBucket()); len(got) != 0 {
					t.Errorf("at level %d the state holds %v", level, got)
				}
				if got := stateSchema(t, m.f); got != uint64(level) {
					t.Errorf("at level %d the state says schema %d", level, got)
				}
				return
			}
			if b.CreatedBy != "user/albie" || uploads != "large by key/AKID; k by user/albie" || downloads != "k by anonymous presigned=false" {
				t.Errorf("at level 4: created by %q, uploads %q, downloads %q", b.CreatedBy, uploads, downloads)
			}
			if got := stateSchema(t, m.f); got != stateSchema4 {
				t.Errorf("at level 4 the state says schema %d", got)
			}
		})
	}
}

// TestWhatIsNoUploadLeavesTheRecordOfABucket: an upload is a version that is
// committed. The creation of a multipart upload, its parts and its abortion,
// a delete marker, the removal of a version, tags, retention, a legal hold
// and the configuration of the bucket change nothing - in whatever schema
// they are written - and a row outlives the object it names. The record goes
// with its bucket, and with nothing else.
func TestWhatIsNoUploadLeavesTheRecordOfABucket(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "ver", CreatedAt: m.at, CreatedBy: "root", ObjectLock: true}})
	m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "other", CreatedAt: m.at}})
	kept := m.put4("ver", "k", "user/albie", 1)
	m.put4("other", "k", "root", 2)
	m.reads4("node-a", 1, download(kept, "anonymous", false, 1, 0))
	want := activityBytes(t, m.f, "ver")
	if want == nil {
		t.Fatal("no record to compare with")
	}
	same := func(what string) {
		t.Helper()
		if got := activityBytes(t, m.f, "ver"); !bytes.Equal(got, want) {
			t.Fatalf("after %s the record is\n%s\nwas\n%s", what, got, want)
		}
	}
	u := porttest.SampleUpload("ver", "large", 3)
	part := porttest.SamplePart(u, 1, 10, 0)
	marker := object.ID(bytes.Repeat([]byte{8}, object.IDSize)).String()
	for _, step := range []struct {
		what string
		e    entry
	}{
		{"the creation of an upload", entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)}},
		{"a part", entry{Type: cmdPutPart, PutPart: &putPartCmd{Bucket: "ver", Key: u.Key, UploadID: u.ID.String(), Part: partToRecord(part)}}},
		{"tags", entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: "ver", Key: "k", VersionRef: refOf(kept), Tags: map[string]string{"a": "b"}}}},
		{"a legal hold", entry{Type: cmdSetLegalHold, SetLegalHold: &legalHoldCmd{Bucket: "ver", Key: "k", VersionRef: refOf(kept), On: true}}},
		{"a configuration", entry{Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Bucket: "ver", Kind: bucket.ConfigPolicy, Document: []byte(`{}`)}}},
		{"a default retention", entry{Type: cmdSetBucketLock, SetBucketLock: &bucketLockCmd{Bucket: "ver"}}},
	} {
		// In its own schema, where it must take effect, and again in the
		// newest, where it may be refused as a repetition: only the three
		// entries of writtenAt keep the record, whatever the schema of
		// another says.
		if r := m.as(max(versionOf(step.e), entrySchemaTimed), step.e); r.err != nil {
			t.Fatalf("%s: %v", step.what, r.err)
		}
		same(step.what)
		m.as(entrySchema4, step.e)
		same(step.what + ", written as schema 4")
	}
	m.ok(entry{Type: cmdAbortUpload, AbortUpload: &uploadRefCmd{Bucket: "ver", Key: u.Key, UploadID: u.ID.String()}})
	same("the abortion of an upload")
	m.ok(entry{Type: cmdSetLegalHold, SetLegalHold: &legalHoldCmd{Bucket: "ver", Key: "k", VersionRef: refOf(kept)}})
	m.as(entrySchema4, entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "ver", Key: "k", MarkerID: marker, At: m.at}})
	same("a delete marker")
	// A put that is refused is no upload, in an entry of schema 4 either.
	refused := porttest.SampleVersion("ver", "", 9)
	if r := m.as(entrySchema4, entry{Type: cmdPutObject, PutObject: versionToRecord(refused)}); r.err == nil {
		t.Fatal("a put without a key was accepted")
	}
	same("a put that was refused")
	// Both versions go, for good: the rows stay.
	for _, ref := range []string{marker, refOf(kept)} {
		if r := m.as(entrySchema4, entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "ver", Key: "k", VersionRef: ref, At: m.at}}); r.err != nil {
			t.Fatalf("removing version %s: %v", ref, r.err)
		}
	}
	same("the removal of the object")
	// The bucket goes, and its record with it, and no other. An entry of
	// the first schema does that as well: a record is never left behind
	// for a bucket created under the name later.
	other := activityBytes(t, m.f, "other")
	if r := m.as(entrySchemaVersion, entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "ver"}}); r.err != nil {
		t.Fatalf("deleting the bucket: %v", r.err)
	}
	if got := activityBytes(t, m.f, "ver"); got != nil {
		t.Errorf("the record of the deleted bucket = %s", got)
	}
	if got := activityBytes(t, m.f, "other"); other == nil || !bytes.Equal(got, other) {
		t.Errorf("another bucket's record after the deletion = %s, was %s", got, other)
	}
	m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "ver", CreatedAt: m.at}})
	if got := activityBytes(t, m.f, "ver"); got != nil {
		t.Errorf("a bucket created again has the record %s", got)
	}
}

// TestADamagedRecordOfABucketsUseStopsTheStateMachine: a record this binary
// cannot read - damaged, or of a format it does not know - is not replaced by
// a new one, which the other members would not hold. The state machine stops,
// as it does at a damaged record of reads (TestTheRecordOfReadsHasAVersion)
// and at every other stored record it cannot read; and a reader is told.
func TestADamagedRecordOfABucketsUseStopsTheStateMachine(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		damage func(raw []byte) []byte
		want   error
	}{
		"a format it does not know": {func(raw []byte) []byte { return bytes.Replace(raw, []byte(`"v":1`), []byte(`"v":2`), 1) }, domain.ErrUnsupportedFormat},
		"no JSON":                   {func(raw []byte) []byte { return raw[:len(raw)/2] }, domain.ErrIntegrity},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store := openStore(t, t.TempDir())
			if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b"}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.PutVersion(ctx, porttest.SampleVersion("b", "k", 1)); err != nil {
				t.Fatal(err)
			}
			raw := activityBytes(t, store.fsm, "b")
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil || string(fields["v"]) != "1" {
				t.Fatalf("the stored record = %s, want a JSON document with \"v\":1", raw)
			}
			if err := store.fsm.db.Update(func(tx *bbolt.Tx) error {
				return tx.Bucket(bucketActivityBucket()).Put([]byte("b"), test.damage(raw))
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.BucketActivities(ctx, []string{"b"}); !errors.Is(err, test.want) {
				t.Errorf("BucketActivities over the damaged record = %v, want %v", err, test.want)
			}
			if _, err := store.PutVersion(ctx, porttest.SampleVersion("b", "next", 2)); !errors.Is(err, test.want) || !errors.Is(err, domain.ErrOutcomeUnknown) {
				t.Errorf("a put over the damaged record = %v, want %v and an unknown outcome", err, test.want)
			}
			if err := store.fsm.health(); !errors.Is(err, test.want) {
				t.Errorf("health = %v, want the state machine stopped", err)
			}
		})
	}
}

// TestAStoreHeldToSchema3WritesNothingOfSchema4: the release of schema 3 at
// work, as this binary stands in for it - and so, also, a node of this
// release whose cluster is at level 3. A bucket is created without its
// creator, and every put, completion and batch of reads succeeds as before
// and is written as before: no record of any bucket's use, no entry of
// schema 4, a state the release of schema 3 opens.
func TestAStoreHeldToSchema3WritesNothingOfSchema4(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openStore(t, t.TempDir())
	asSchema3Release(store)
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b", CreatedAt: time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC), CreatedBy: "user/albie"}); err != nil {
		t.Fatalf("a bucket that says who, at level 3: %v", err)
	}
	if got := string(storedBucket(t, store.fsm, "b")); got != `{"name":"b","created_at":"2026-10-12T08:00:00Z"}` {
		t.Errorf("the bucket is stored as %s", got)
	}
	if b, err := store.GetBucket(ctx, "b"); err != nil || b.CreatedBy != "" {
		t.Errorf("GetBucket = created by %q, %v; want nobody recorded", b.CreatedBy, err)
	}
	v := porttest.SampleVersion("b", "k", 1)
	v.CreatedBy = "user/albie"
	put, err := store.PutVersion(ctx, v)
	if err != nil || put.Stored.CreatedBy != "user/albie" {
		t.Fatalf("a put at level 3 = made by %q, %v", put.Stored.CreatedBy, err)
	}
	u := porttest.SampleUpload("b", "large", 2)
	if err := store.CreateUpload(ctx, u); err != nil {
		t.Fatal(err)
	}
	part := porttest.SamplePart(u, 1, 10, 0)
	if _, err := store.PutPart(ctx, "b", u.Key, u.ID, part); err != nil {
		t.Fatal(err)
	}
	part.Seal = bytes.Repeat([]byte{1}, 32)
	if _, err := store.CompleteUpload(ctx, ports.Completion{
		Bucket: "b", Key: u.Key, ID: u.ID, VersionID: object.ID(bytes.Repeat([]byte{9}, object.IDSize)), ETag: "etag",
		At: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), CreatedBy: "key/AKID", Parts: []object.Part{part},
	}); err != nil {
		t.Fatalf("a completion at level 3: %v", err)
	}
	batch := ports.ReadBatch{Node: store.NodeID(), Boot: 1, Seq: 1, Items: []ports.ObjectRead{
		{Bucket: "b", Key: "k", Version: put.Stored.Ref(), VersionID: put.Stored.VersionID, Read: object.Read{Who: "anonymous", Downloads: 1}},
	}}
	if err := store.AddObjectReads(ctx, batch); err != nil {
		t.Fatalf("a batch of reads at level 3: %v", err)
	}
	if access, err := store.ObjectAccess(ctx, "b", "k", put.Stored.Ref()); err != nil || access.Downloads != 1 {
		t.Errorf("the record of reads at level 3 = %+v, %v", access, err)
	}
	got, err := store.BucketActivities(ctx, []string{"b"})
	if err != nil || len(got) != 1 || !got[0].Empty() {
		t.Errorf("BucketActivities at level 3 = %+v, %v; want nothing known", got, err)
	}
	if raw := rawState(t, store.fsm, bucketActivityBucket()); len(raw) != 0 {
		t.Errorf("at level 3 the state holds %v", raw)
	}
	if schema := stateSchema(t, store.fsm); schema != stateSchema3 {
		t.Errorf("at level 3 the state says schema %d", schema)
	}
	// And what it is handed of schema 4 it does not log.
	data, err := encodeEntry(entry{Type: cmdPutObject, PutObject: versionToRecord(porttest.SampleVersion("b", "smuggled", 5))}, entrySchema4)
	if err != nil {
		t.Fatal(err)
	}
	index, err := store.AppliedIndex()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProposeLocal(ctx, data); !errors.Is(err, domain.ErrUnsupportedFormat) || errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Errorf("ProposeLocal of an entry of schema 4 = %v, want a definite %v", err, domain.ErrUnsupportedFormat)
	}
	if after, err := store.AppliedIndex(); err != nil || after != index || store.fsm.health() != nil {
		t.Errorf("the refused entry was logged: the state is at entry %d (%v), was at %d; health %v", after, err, index, store.fsm.health())
	}
}

// TestWhatSchema4RefusesIsRefusedBeforeItIsProposed: the first entry of
// schema 4 ends the way back to the release before, whether it took effect or
// not. So a creator that the state machine would refuse - by its bytes, not
// by the state - the store refuses by itself, and proposes nothing; and a
// question about too many buckets is refused before the store is asked.
//
// The store is held to schema 4: at level 5 the first bucket is itself an
// entry of schema 5 (ADR-0023), and the state would say 5 before any of
// these calls. What schema 5 refuses before it proposes is asked in
// store5_test.go.
func TestWhatSchema4RefusesIsRefusedBeforeItIsProposed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openStore(t, t.TempDir())
	asSchema4Release(store)
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	index, err := store.AppliedIndex()
	if err != nil {
		t.Fatal(err)
	}
	for what, by := range map[string]string{"too long": strings.Repeat("u", 257), "of two lines": "user/a\nuser/b", "with a NUL": "user/\x00"} {
		err := store.CreateBucket(ctx, bucket.Bucket{Name: "refused", CreatedBy: by})
		if !errors.Is(err, domain.ErrInvalidArgument) || errors.Is(err, domain.ErrOutcomeUnknown) {
			t.Errorf("a creator %s = %v, want a definite %v", what, err, domain.ErrInvalidArgument)
		}
	}
	if _, err := store.BucketActivities(ctx, make([]string, ports.MaxBucketActivities+1)); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("BucketActivities of too many buckets = %v", err)
	}
	if after, err := store.AppliedIndex(); err != nil || after != index {
		t.Fatalf("a refused call was proposed: the state is at entry %d (%v), was at %d", after, err, index)
	}
	if got := stateSchema(t, store.fsm); got != stateSchemaTimed {
		t.Errorf("refused calls left the state at schema %d, want %d", got, stateSchemaTimed)
	}
	// What can be, at the bound, is proposed and kept - and from then on
	// the state is of schema 4.
	longest := strings.Repeat("u", 256)
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "kept", CreatedBy: longest}); err != nil {
		t.Fatal(err)
	}
	if b, err := store.GetBucket(ctx, "kept"); err != nil || b.CreatedBy != longest {
		t.Errorf("the longest creator there can be = %d bytes, %v", len(b.CreatedBy), err)
	}
	if got := stateSchema(t, store.fsm); got != stateSchema4 {
		t.Errorf("after the first bucket with its creator the state is at schema %d, want %d", got, stateSchema4)
	}
}

// TestTheStoredBucketActivityIsPinned: the record of a bucket's latest use is
// computed by every member from the entries, so the rule it is computed by is
// part of the replicated format - how many rows are kept, which goes, what a
// row is the only one of, where a row of the same time is put - and so are
// the bytes it is stored in: the order of the fields, the form of a time,
// what is left out when it is empty. This is the record after a fixed
// sequence of entries, byte for byte. A change that makes this test fail
// makes two releases disagree: it needs a rise of the entry schema, not a
// new file.
func TestTheStoredBucketActivityIsPinned(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	// A time with a fraction of a second: it is stored to the nanosecond.
	m.at = time.Date(2026, 10, 12, 9, 0, 0, 123456789, time.UTC)
	m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedAt: m.at, CreatedBy: "user/albie"}})
	a := m.put4("b", "reports/2026/q3.pdf", "user/albie", 1)
	// A key that is not ASCII; and one that JSON does not write as it is,
	// by somebody who was not recorded.
	c := m.put4("b", "zoë/π.txt", "key/AKIDEXAMPLE", 2)
	m.put4("b", "a key with \"quotes\", <angles> & a\ttab", "", 3)
	m.upload4("b", "large.bin", "root", 4, entrySchema4)
	// The first key again: its row moves to the front, and the oldest of
	// four goes.
	a = m.put4("b", a.Key, "user/bob", 5)
	// One batch, one time: four downloads, of which three are kept, the
	// later of the batch first; a copy, which is none; the same reader
	// twice, who has one row.
	m.reads4("node-a", 1,
		download(c, "anonymous", false, 1, 0),
		download(a, "user/albie", false, 2, 0),
		download(c, "user/copier", false, 0, 5),
		download(a, "key/AKIDEXAMPLE", true, 1, 1),
		download(a, "user/albie", false, 1, 0),
	)
	// A leader with a slow clock: the time of a row does not go back.
	m.at = m.at.Add(-time.Hour)
	m.reads4("node-b", 1, download(c, "key/AKIDEXAMPLE", false, 1, 0))
	// The same batch again, which is nothing.
	m.reads4("node-b", 1, download(a, "user/never", false, 9, 0))

	got := activityBytes(t, m.f, "b")
	want, err := os.ReadFile("testdata/bucket-activity-record.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(got, '\n'), want) {
		t.Errorf("the stored record of the bucket's use is not the one this release stores:\n got %s\nwant %s", got, want)
	}
	// A record of uploads alone has no place for downloads, and the other
	// way about: what is empty is left out.
	m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "up", CreatedAt: m.at}})
	m.at = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	only := m.put4("up", "k", "root", 9)
	if got := string(activityBytes(t, m.f, "up")); got != `{"v":1,"uploads":[{"at":"2026-10-12T12:00:01Z","by":"root","key":"k"}]}` {
		t.Errorf("a record of one upload = %s", got)
	}
	m.reads4("node-a", 2, download(only, "anonymous", true, 1, 0))
	if got := string(activityBytes(t, m.f, "up")); got != `{"v":1,"uploads":[{"at":"2026-10-12T12:00:01Z","by":"root","key":"k"}],`+
		`"downloads":[{"at":"2026-10-12T12:00:02Z","by":"anonymous","presigned":true,"key":"k"}]}` {
		t.Errorf("a record of one upload and one download = %s", got)
	}
}

// FuzzBucketActivityRecord: whatever bytes are stored as the record of a
// bucket's use are read as a record of the format this binary knows or
// refused with a domain error - never a panic. What is read stays within its
// bounds when it is added to, and what is then stored is read again as the
// same record: the encoding has one form.
func FuzzBucketActivityRecord(f *testing.F) {
	f.Add([]byte(`{"v":1}`))
	f.Add([]byte(`{"v":1,"uploads":[{"at":"2026-10-12T09:00:01Z","by":"root","key":"k"}]}`))
	f.Add([]byte(`{"v":1,"uploads":[{"at":"2026-10-12T09:00:01.123456789Z","by":"","key":"a"},{"at":"2026-10-12T09:00:00Z","by":"user/b","key":"b"}],` +
		`"downloads":[{"at":"2026-10-12T09:00:02Z","by":"anonymous","presigned":true,"key":"k"}]}`))
	f.Add([]byte(`{"v":2,"uploads":[]}`))
	f.Add([]byte(`{"v":1,"uploads":[{},{},{},{},{},{},{}],"downloads":null}`))
	f.Add([]byte(`{"v":1,"uploads":[{"at":"not a time"}]}`))
	f.Add([]byte(`{"v":1,"downloads":[{"at":"2026-10-12T09:00:02+02:00","by":"x","key":"k","presigned":1}]}`))
	f.Add([]byte(`[]`))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		a, err := decodeBucketActivity("b", raw)
		if err != nil {
			if !errors.Is(err, domain.ErrIntegrity) && !errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Fatalf("untyped error: %v", err)
			}
			return
		}
		at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		next := a.Uploaded(at, "root", "fuzz").Downloaded(at, "root", false, "fuzz")
		if len(next.Uploads) > bucket.MaxRecentUploads || len(next.Downloads) > bucket.MaxRecentDownloads || len(next.Uploads) == 0 || len(next.Downloads) == 0 {
			t.Fatalf("a record of %d uploads and %d downloads, added to, has %d and %d", len(a.Uploads), len(a.Downloads), len(next.Uploads), len(next.Downloads))
		}
		stored, err := encodeBucketActivity("b", next)
		if err != nil {
			// A time that can be read and not written - one before the
			// year 0 once it is in UTC - is a damaged record too.
			if !errors.Is(err, domain.ErrIntegrity) {
				t.Fatalf("storing a record that was read and added to: %v", err)
			}
			return
		}
		read, err := decodeBucketActivity("b", stored)
		if err != nil {
			t.Fatalf("the record that was just stored: %v", err)
		}
		again, err := encodeBucketActivity("b", read)
		if err != nil || !bytes.Equal(again, stored) {
			t.Fatalf("the stored record %s is encoded again as %s (%v)", stored, again, err)
		}
	})
}

// TestRollingUpgradeToSchema4: a cluster of the release before this one -
// entry schema 3 - is upgraded node by node. While any member is of that
// release, or has not said that it is not, nothing of schema 4 is logged: a
// bucket is created without its creator and is created all the same; an
// upload and a batch of reads succeed as they did; no member keeps a record
// of any bucket's use; and every state is one the release of schema 3 opens.
// Nothing is refused, on any node, at any step. When the last member has
// registered, the next bucket has its creator and the next upload and the
// next batch of reads write the record - the same bytes on every member, and
// still after the leader has changed. A bucket that is deleted takes its
// record with it.
//
// The release the nodes come back with is the one of schema 4: with this
// one, of schema 5, the first bucket after the last registration would be an
// entry of schema 5, and the state would say 5 before schema 4 was looked at.
// That upgrade is TestRollingUpgradeToSchema5.
func TestRollingUpgradeToSchema4(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	nodes := []string{"m1", "m2", "m3", "s4"}
	for _, id := range nodes {
		c.schema3[id] = true
	}
	c.form(nodes[:3], nodes[3:])
	c.register(nodes...)
	for _, id := range nodes {
		eventually(t, "node "+id+" to see every member on schema 3", func() bool {
			level, err := c.store(id).entryVersion()
			return err == nil && level == entrySchema3
		})
	}
	created := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	var serial byte
	seq := map[string]uint64{}
	// use is everything a bucket's use consists of, through one node: a
	// bucket created by somebody, a put and a completed upload into it,
	// and a batch of reads of what was put. It returns the bucket.
	use := func(s *Store, name string) bucket.Bucket {
		t.Helper()
		serial += 2
		if err := s.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: created, CreatedBy: "user/albie"}); err != nil {
			t.Fatalf("CreateBucket %s through %s: %v", name, s.NodeID(), err)
		}
		v := porttest.SampleVersion(name, "put", serial)
		v.CreatedBy = "user/albie"
		put, err := s.PutVersion(ctx, v)
		if err != nil || put.Stored.CreatedBy != "user/albie" {
			t.Fatalf("PutVersion into %s through %s = made by %q, %v", name, s.NodeID(), put.Stored.CreatedBy, err)
		}
		u := porttest.SampleUpload(name, "upload", serial+1)
		if err := s.CreateUpload(ctx, u); err != nil {
			t.Fatalf("CreateUpload: %v", err)
		}
		part := porttest.SamplePart(u, 1, 10, 0)
		if _, err := s.PutPart(ctx, name, u.Key, u.ID, part); err != nil {
			t.Fatalf("PutPart: %v", err)
		}
		part.Seal = bytes.Repeat([]byte{1}, 32)
		if _, err := s.CompleteUpload(ctx, ports.Completion{
			Bucket: name, Key: u.Key, ID: u.ID, VersionID: object.ID(bytes.Repeat([]byte{serial + 100}, object.IDSize)),
			ETag: "etag", At: created, CreatedBy: "key/AKID", Parts: []object.Part{part},
		}); err != nil {
			t.Fatalf("CompleteUpload into %s through %s: %v", name, s.NodeID(), err)
		}
		seq[s.NodeID()]++
		if err := s.AddObjectReads(ctx, ports.ReadBatch{Node: s.NodeID(), Boot: 1, Seq: seq[s.NodeID()], Items: []ports.ObjectRead{
			{Bucket: name, Key: "put", Version: put.Stored.Ref(), VersionID: put.Stored.VersionID, Read: object.Read{Who: "anonymous", Downloads: 1}},
		}}); err != nil {
			t.Fatalf("AddObjectReads through %s: %v", s.NodeID(), err)
		}
		// The reads were taken either way.
		if access, err := s.ObjectAccess(ctx, name, "put", put.Stored.Ref()); err != nil || access.Downloads != 1 {
			t.Errorf("the record of reads of %s/put = %+v, %v", name, access, err)
		}
		b, err := s.GetBucket(ctx, name)
		if err != nil {
			t.Fatalf("GetBucket %s: %v", name, err)
		}
		return b
	}
	// nothingOfSchema4 says of every node that it holds nothing schema 4
	// keeps and is in a state the release of schema 3 opens.
	nothingOfSchema4 := func(step string, names ...string) {
		t.Helper()
		for _, id := range nodes {
			s := c.store(id)
			got, err := s.BucketActivities(ctx, names)
			if err != nil {
				t.Fatalf("%s: BucketActivities on %s: %v", step, id, err)
			}
			for i, a := range got {
				if !a.Empty() {
					t.Errorf("%s: node %s keeps a record of %s: %+v", step, id, names[i], a)
				}
			}
			if raw := rawState(t, s.fsm, bucketActivityBucket()); len(raw) != 0 {
				t.Errorf("%s: node %s holds %v", step, id, raw)
			}
			if schema := stateSchema(t, s.fsm); schema != stateSchema3 {
				t.Errorf("%s: node %s: state schema %d, want %d", step, id, schema, stateSchema3)
			}
			if err := s.Check(ctx); err != nil {
				t.Errorf("%s: node %s is not healthy: %v", step, id, err)
			}
		}
	}

	// The release before at work.
	var names []string
	names = append(names, "before")
	if b := use(c.store("s4"), "before"); b.CreatedBy != "" {
		t.Fatalf("the release of schema 3 recorded that %q created a bucket", b.CreatedBy)
	}
	nothingOfSchema4("before the upgrade", names...)

	for i, id := range nodes {
		// The node is stopped and comes back with the release of schema 4.
		c.stop(id)
		c.mu.Lock()
		c.schema3[id], c.schema4[id] = false, true
		c.mu.Unlock()
		store := c.start(id, id != "s4")
		c.ready(id)
		// Twice: before the node has registered what it now applies, and
		// after, while members of the release before remain.
		for _, registered := range []bool{false, true} {
			if registered {
				c.register(id)
				if i == len(nodes)-1 {
					break
				}
			}
			name := fmt.Sprintf("during-%d-%t", i, registered)
			names = append(names, name)
			if b := use(store, name); b.CreatedBy != "" {
				t.Fatalf("node %s on schema 4 (registered: %v), members on schema 3: the bucket was created by %q", id, registered, b.CreatedBy)
			}
			// Through every other node as well, of either release.
			for _, other := range nodes {
				if _, err := c.store(other).PutVersion(ctx, porttest.SampleVersion(name, "k-"+other, 1)); err != nil {
					t.Fatalf("upgrade step %d: PutVersion on %s: %v", i, other, err)
				}
			}
			nothingOfSchema4(fmt.Sprintf("upgrade step %d, registered %v", i, registered), names...)
		}
	}

	// All four run that release and have said so. Nothing of schema 4 was
	// written yet: the upgrade could be undone.
	for _, id := range nodes {
		eventually(t, "node "+id+" to see every member on schema 4", func() bool {
			level, err := c.store(id).entryVersion()
			return err == nil && level == entrySchema4
		})
	}
	nothingOfSchema4("after the last registration", names...)

	// Now it is in use, through every node.
	var after []string
	for _, id := range nodes {
		name := "after-" + id
		after = append(after, name)
		if b := use(c.store(id), name); b.CreatedBy != "user/albie" {
			t.Errorf("after the upgrade, through %s: the bucket was created by %q", id, b.CreatedBy)
		}
	}
	// same says that every running node holds the same buckets and the
	// same records, byte for byte, and returns what they hold.
	same := func(step string, ids ...string) map[string]string {
		t.Helper()
		var want map[string]string
		for _, id := range ids {
			s := c.store(id)
			// A read through the node waits until it has applied everything.
			got, err := s.BucketActivities(ctx, slices.Concat(after, names))
			if err != nil {
				t.Fatalf("%s: BucketActivities on %s: %v", step, id, err)
			}
			for i, a := range got {
				uploads, downloads := activityWords(a)
				switch {
				case i < len(after) && (!strings.Contains(uploads, "upload by key/AKID; put by user/albie") || downloads != "put by anonymous presigned=false"):
					t.Errorf("%s: node %s: the record of %s = %q and %q", step, id, after[i], uploads, downloads)
				case i >= len(after) && !a.Empty():
					t.Errorf("%s: node %s keeps a record of %s, which was used before the cluster was at schema 4: %+v", step, id, names[i-len(after)], a)
				}
			}
			if schema := stateSchema(t, s.fsm); schema != stateSchema4 {
				t.Errorf("%s: node %s: state schema %d after schema 4 was used", step, id, schema)
			}
			raw := rawState(t, s.fsm, bucketActivityBucket(), bucketsBucket())
			if want == nil {
				want = raw
			}
			if !reflect.DeepEqual(raw, want) {
				t.Errorf("%s: node %s holds other records than %s:\n%v\n%v", step, id, ids[0], raw, want)
			}
		}
		return want
	}
	held := same("after the upgrade", nodes...)
	if len(held) != 2+len(after)+len(names)+len(after) {
		t.Errorf("the nodes hold %d places, want %d records and %d buckets: %v", len(held), len(after), len(names)+len(after), held)
	}

	// The leader goes. The others elect a new one, which keeps the record
	// as the old one did; the old one comes back and catches up.
	old := c.leader()
	c.stop(old)
	var rest []string
	for _, id := range nodes {
		if id != old {
			rest = append(rest, id)
		}
	}
	through := rest[len(rest)-1]
	eventually(t, "a new leader to take a write", func() bool {
		_, err := c.store(through).PutVersion(ctx, porttest.SampleVersion(after[0], "after-the-leader", 250))
		return err == nil || errors.Is(err, domain.ErrConflict)
	})
	latest, err := c.store(through).BucketActivities(ctx, after[:1])
	if err != nil {
		t.Fatal(err)
	}
	if uploads, _ := activityWords(latest[0]); !strings.HasPrefix(uploads, "after-the-leader by ; upload by key/AKID; ") {
		t.Errorf("after the leader changed, the record of %s = %q", after[0], uploads)
	}
	c.start(old, old != "s4")
	c.ready(old)
	changed := same("after the leader changed", nodes...)
	if reflect.DeepEqual(changed, held) {
		t.Error("the upload after the leader changed is in no record")
	}

	// A bucket that is emptied and deleted takes its record with it, on
	// every node.
	gone := after[len(after)-1]
	s := c.store(through)
	for _, key := range []string{"put", "upload"} {
		if _, err := s.DeleteObject(ctx, gone, key, ports.DeleteOptions{At: created}); err != nil {
			t.Fatalf("DeleteObject %s/%s: %v", gone, key, err)
		}
	}
	if err := s.DeleteBucket(ctx, gone); err != nil {
		t.Fatalf("DeleteBucket %s: %v", gone, err)
	}
	for _, id := range nodes {
		got, err := c.store(id).BucketActivities(ctx, []string{gone, after[0]})
		if err != nil || !got[0].Empty() || got[1].Empty() {
			t.Errorf("node %s after the bucket was deleted: %+v, %v", id, got, err)
		}
		if raw := activityBytes(t, c.store(id).fsm, gone); raw != nil {
			t.Errorf("node %s still holds the record of the deleted bucket: %s", id, raw)
		}
	}
}

// TestANodeOfSchema3StopsAtAnEntryOfSchema4: if an entry of schema 4 does
// reach a node of the release before - here because it was wrongly said to
// apply schema 4 - that node stops and leaves the group; it does not apply
// the put as the put it could read. The rest of the cluster goes on.
func TestANodeOfSchema3StopsAtAnEntryOfSchema4(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	c.schema3["s4"] = true
	c.form([]string{"m1", "m2", "m3"}, []string{"s4"})
	// s4 is a member the others do not know the version of: the cluster
	// stays below schema 4, and everything works without it.
	c.register("m1", "m2", "m3")
	if err := c.store("m1").CreateBucket(ctx, bucket.Bucket{Name: "b", CreatedBy: "user/albie"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store("m1").PutVersion(ctx, porttest.SampleVersion("b", "before", 1)); err != nil {
		t.Fatal(err)
	}
	if b, err := c.store("s4").GetBucket(ctx, "b"); err != nil || b.CreatedBy != "" {
		t.Fatalf("with an unregistered member the bucket was created by %q, %v", b.CreatedBy, err)
	}
	if got, err := c.store("s4").BucketActivities(ctx, []string{"b"}); err != nil || !got[0].Empty() {
		t.Fatalf("with an unregistered member a record was kept: %+v, %v", got, err)
	}

	// Force the situation the rules exist to prevent.
	lying := cluster.Node{ID: "s4", RPCAddr: "s4:9002", Roles: cluster.AllRoles(), MetadataVersion: entrySchema4}
	if err := c.store("m1").SetNode(ctx, lying); err != nil {
		t.Fatal(err)
	}
	eventually(t, "m1 to believe every member on schema 4", func() bool {
		level, err := c.store("m1").entryVersion()
		return err == nil && level == entrySchema4
	})
	if _, err := c.store("m1").PutVersion(ctx, porttest.SampleVersion("b", "too-new", 2)); err != nil {
		t.Fatalf("the put of schema 4: %v", err)
	}
	eventually(t, "the node of schema 3 to stop", func() bool {
		return errors.Is(c.store("s4").Check(ctx), domain.ErrUnsupportedFormat)
	})
	if _, err := c.store("s4").GetObject(ctx, "b", "before"); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Errorf("read on the stopped node = %v, want %v", err, domain.ErrUnsupportedFormat)
	}
	// It did not apply the entry in some other way: the version is not in
	// its state, nothing of schema 4 is, and the state says what it said.
	stopped := c.store("s4").fsm
	if err := stopped.view(func(tx *bbolt.Tx) error {
		if v, err := readVersion(tx.Bucket(objectsBucket()).Bucket([]byte("b")), "too-new"); err != nil || v != nil {
			t.Errorf("the stopped node holds the version of the entry it stopped at: %+v, %v", v, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if raw := rawState(t, stopped, bucketActivityBucket()); len(raw) != 0 {
		t.Errorf("the stopped node holds %v", raw)
	}
	if schema := stateSchema(t, stopped); schema >= stateSchema4 {
		t.Errorf("the stopped node's state says schema %d", schema)
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		got, err := c.store(id).BucketActivities(ctx, []string{"b"})
		if uploads, _ := activityWords(got[0]); err != nil || uploads != "too-new by " {
			t.Errorf("node %s: the record of b = %q, %v", id, uploads, err)
		}
	}
}

// TestSchema4WhenAMemberHasNotRegistered: before anything of schema 4 is in
// the state, a member that has not said which schema it applies holds the
// cluster below it - it might be of the release before - and what schema 4
// would add is left out, not refused. Once the state holds what only schema 4
// expresses, no member of that release can be in the cluster: it stopped at
// the first such entry. So a member whose record is gone no longer takes the
// cluster back, and the record of a bucket's use goes on being kept.
//
// The members run the release of schema 4: a bucket of this one is an entry
// of schema 5 once all have registered (ADR-0023), and the state would hold
// what only schema 5 expresses - which TestSchema5WhenAMemberHasNotRegistered
// is about.
func TestSchema4WhenAMemberHasNotRegistered(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	c.allOfSchema4("m1", "m2", "m3", "s4")
	c.form([]string{"m1", "m2", "m3"}, []string{"s4"})
	c.register("m1", "m2", "m3", "s4")
	store := c.store("m1")
	eventually(t, "m1 to see every member on schema 4", func() bool {
		level, err := store.entryVersion()
		return err == nil && level == entrySchema4
	})
	if err := store.RemoveNode(ctx, "s4"); err != nil {
		t.Fatal(err)
	}
	// (Whether m1 leads or not, and so whether it knows of the removal
	// yet: one that does not is told by the leader, and writes below.)
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "held-back", CreatedBy: "user/albie"}); err != nil {
		t.Fatalf("a bucket that says who, with an unregistered member = %v, want it created without", err)
	}
	if _, err := store.PutVersion(ctx, porttest.SampleVersion("held-back", "k", 1)); err != nil {
		t.Fatalf("a put with an unregistered member: %v", err)
	}
	if b, err := store.GetBucket(ctx, "held-back"); err != nil || b.CreatedBy != "" {
		t.Errorf("with an unregistered member the bucket was created by %q, %v", b.CreatedBy, err)
	}
	if got, err := store.BucketActivities(ctx, []string{"held-back"}); err != nil || !got[0].Empty() {
		t.Errorf("with an unregistered member a record was kept: %+v, %v", got, err)
	}
	for _, id := range []string{"m1", "m2", "m3", "s4"} {
		c.ready(id)
		if schema := stateSchema(t, c.store(id).fsm); schema >= stateSchema4 {
			t.Errorf("node %s: state schema %d before schema 4 was used", id, schema)
		}
	}

	c.register("s4")
	eventually(t, "schema 4 to be used", func() bool {
		level, err := store.entryVersion()
		return err == nil && level == entrySchema4
	})
	if _, err := store.PutVersion(ctx, porttest.SampleVersion("held-back", "first", 2)); err != nil {
		t.Fatal(err)
	}
	if got, err := store.BucketActivities(ctx, []string{"held-back"}); err != nil || len(got[0].Uploads) != 1 {
		t.Fatalf("the first upload at schema 4 = %+v, %v", got, err)
	}

	// From now on it cannot be held back.
	if err := store.RemoveNode(ctx, "s4"); err != nil {
		t.Fatal(err)
	}
	for id, fill := range map[string]byte{"m1": 10, "s4": 11} {
		name := "through-" + id
		if err := c.store(id).CreateBucket(ctx, bucket.Bucket{Name: name, CreatedBy: "key/AKID"}); err != nil {
			t.Fatalf("a bucket through %s with an unregistered member, after schema 4 was used: %v", id, err)
		}
		if _, err := c.store(id).PutVersion(ctx, porttest.SampleVersion(name, "k", fill)); err != nil {
			t.Fatalf("a put through %s: %v", id, err)
		}
		if b, err := c.store(id).GetBucket(ctx, name); err != nil || b.CreatedBy != "key/AKID" {
			t.Errorf("through %s the bucket was created by %q, %v", id, b.CreatedBy, err)
		}
		if got, err := c.store(id).BucketActivities(ctx, []string{name}); err != nil || len(got[0].Uploads) != 1 {
			t.Errorf("through %s the record = %+v, %v", id, got, err)
		}
	}
	// And what needs schema 3 is written too, as it could not be at a
	// level below.
	if err := store.PutTagDefinition(ctx, tagging.Definition{Key: "stage", CreatedAt: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}, audit.Record{}); err != nil {
		t.Errorf("a tag definition with an unregistered member, after schema 4 was used: %v", err)
	}
}

// TestALeaderRefusesAnEntryOfSchema4BelowLevel4: what a node that knows the
// cluster a moment late proposes as schema 4 is refused by the leader -
// nothing is logged - and written again as the cluster takes it: the put
// succeeds, and no record is kept. (The members run the release of schema
// 4; the same of schema 5 is TestALeaderRefusesAnEntryOfSchema5BelowLevel5.)
func TestALeaderRefusesAnEntryOfSchema4BelowLevel4(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	c.allOfSchema4("m1", "m2", "m3", "s4")
	c.form([]string{"m1", "m2", "m3"}, []string{"s4"})
	c.register("m1", "m2", "m3", "s4")
	store := c.store("s4")
	eventually(t, "s4 to see every member on schema 4", func() bool {
		level, err := store.entryVersion()
		return err == nil && level == entrySchema4
	})
	// From here on s4 believes the cluster to be at schema 4, and its
	// leader says 3. (s4 has no vote and never leads.)
	behind := &behindLeader{LeaderClient: store.leader, level: entrySchema3}
	store.leader = behind
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b", CreatedBy: "user/albie"}); err != nil {
		t.Fatalf("a bucket that says who, through a node behind its leader: %v", err)
	}
	if got := behind.proposed(); len(got) != 2 || got[0] != entrySchema4 || got[1] >= entrySchema3 {
		t.Errorf("the bucket was proposed in the schemas %v, want 4 and then one below 3", got)
	}
	v := porttest.SampleVersion("b", "k", 1)
	v.CreatedBy = "user/albie"
	put, err := store.PutVersion(ctx, v)
	if err != nil || put.Stored.CreatedBy != "user/albie" {
		t.Fatalf("a put through a node behind its leader = made by %q, %v", put.Stored.CreatedBy, err)
	}
	if got := behind.proposed(); !bytes.Equal(got, []byte{entrySchema4, entrySchema3}) {
		t.Errorf("the put was proposed in the schemas %v, want 4 and then 3", got)
	}
	if err := store.AddObjectReads(ctx, ports.ReadBatch{Node: "s4", Boot: 1, Seq: 1, Items: []ports.ObjectRead{
		{Bucket: "b", Key: "k", Version: put.Stored.Ref(), VersionID: put.Stored.VersionID, Read: object.Read{Who: "anonymous", Downloads: 1}},
	}}); err != nil {
		t.Fatalf("a batch of reads through a node behind its leader: %v", err)
	}
	if got := behind.proposed(); !bytes.Equal(got, []byte{entrySchema4, entrySchema3}) {
		t.Errorf("the reads were proposed in the schemas %v, want 4 and then 3", got)
	}
	if b, err := store.GetBucket(ctx, "b"); err != nil || b.CreatedBy != "" {
		t.Errorf("the bucket was created by %q, %v; want nobody recorded", b.CreatedBy, err)
	}
	for _, id := range []string{"m1", "m2", "m3", "s4"} {
		c.ready(id)
		if raw := rawState(t, c.store(id).fsm, bucketActivityBucket()); len(raw) != 0 {
			t.Errorf("node %s holds %v", id, raw)
		}
		if schema := stateSchema(t, c.store(id).fsm); schema != stateSchema3 {
			t.Errorf("node %s: state schema %d, want %d", id, schema, stateSchema3)
		}
	}
}
