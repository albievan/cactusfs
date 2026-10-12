package raftmeta

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/domain/tagging"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// machine drives a state machine as a leader of the newest schema would:
// every entry is written in the schema it needs, never below the second, and
// stamped with a time that moves on by a second an entry.
type machine struct {
	t     *testing.T
	f     *fsm
	index uint64
	at    time.Time
}

func newMachine(t *testing.T) *machine {
	t.Helper()
	return &machine{t: t, f: newFSM(t), at: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
}

// as applies an entry written in the given schema. An entry that stops the
// state machine fails the test.
func (m *machine) as(schema byte, e entry) applyResult {
	m.t.Helper()
	r := m.raw(schema, e)
	if r.fatal {
		m.t.Fatalf("%s stopped the state machine: %v", e.Type, r.err)
	}
	return r
}

// raw applies an entry and returns whatever came of it.
func (m *machine) raw(schema byte, e entry) applyResult {
	m.t.Helper()
	m.index++
	if schema >= entrySchemaTimed {
		m.at = m.at.Add(time.Second)
		e.At = m.at
	}
	data, err := encodeEntry(e, schema)
	if err != nil {
		m.t.Fatal(err)
	}
	return result(m.t, m.f.Apply(&raft.Log{Index: m.index, Type: raft.LogCommand, Data: data}))
}

func (m *machine) apply(e entry) applyResult {
	m.t.Helper()
	return m.as(max(versionOf(e), entrySchemaTimed), e)
}

// ok applies an entry that must take effect.
func (m *machine) ok(e entry) applyResult {
	m.t.Helper()
	r := m.apply(e)
	if r.err != nil {
		m.t.Fatalf("%s: %v", e.Type, r.err)
	}
	return r
}

func (m *machine) bucket(name string, versioning bucket.Versioning) {
	m.t.Helper()
	m.ok(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: name, CreatedAt: m.at}})
	if versioning != bucket.Unversioned {
		m.ok(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: name, Status: string(versioning)}})
	}
}

func (m *machine) put(bucketName, key string, fill byte) versionRecord {
	m.t.Helper()
	return *m.ok(entry{Type: cmdPutObject, PutObject: versionToRecord(porttest.SampleVersion(bucketName, key, fill))}).stored
}

// reads applies a batch of one download each of the versions, by one reader.
func (m *machine) reads(node string, boot, seq uint64, who string, versions ...versionRecord) applyResult {
	m.t.Helper()
	cmd := &addReadsCmd{Node: node, Boot: boot, Seq: seq}
	for _, v := range versions {
		cmd.Items = append(cmd.Items, readItem{Bucket: v.Bucket, Key: v.Key, Version: refOf(v), VersionID: v.VersionID, Who: who, Downloads: 1})
	}
	return m.apply(entry{Type: cmdAddReads, AddReads: cmd})
}

// rawState returns what the named top-level buckets of a state hold, byte
// for byte: "<bucket>/<nested>/<key in hex>" -> value. A bucket that exists
// is in it by its own name, whether it holds anything or not.
func rawState(t *testing.T, f *fsm, names ...[]byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	var walk func(prefix string, b *bbolt.Bucket) error
	walk = func(prefix string, b *bbolt.Bucket) error {
		return b.ForEach(func(k, v []byte) error {
			if nested := b.Bucket(k); nested != nil {
				return walk(prefix+string(k)+"/", nested)
			}
			out[prefix+fmt.Sprintf("%x", k)] = string(v)
			return nil
		})
	}
	if err := f.view(func(tx *bbolt.Tx) error {
		for _, name := range names {
			b := tx.Bucket(name)
			if b == nil {
				continue
			}
			out[string(name)] = ""
			if err := walk(string(name)+"/", b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// nestedBuckets names the buckets nested directly in the named top-level
// buckets of a state, "<bucket>/<nested>", in order - whether they hold
// anything or not, which rawState does not show.
func nestedBuckets(t *testing.T, f *fsm, names ...[]byte) []string {
	t.Helper()
	var out []string
	if err := f.view(func(tx *bbolt.Tx) error {
		for _, name := range names {
			holder := tx.Bucket(name)
			if holder == nil {
				continue
			}
			if err := holder.ForEachBucket(func(nested []byte) error {
				out = append(out, string(name)+"/"+string(nested))
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// schema3Buckets are the buckets of the state that exist from schema 3 on.
func schema3Buckets() [][]byte {
	return [][]byte{accessBucket(), accessFlushBucket(), subBucketsBucket(), tagDefsBucket()}
}

// accessOf reads the record of who read a version straight from the state.
func accessOf(t *testing.T, f *fsm, v versionRecord) (access object.Access, found bool) {
	t.Helper()
	if err := f.view(func(tx *bbolt.Tx) error {
		records, err := lazyNested(tx, accessBucket(), v.Bucket, false)
		if err != nil || records == nil {
			return err
		}
		place, _ := locator(v.Key, refOf(v))
		found = records.Get(place) != nil
		access, err = readAccess(records, place)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return access, found
}

// TestVersionOf: every entry is written in the oldest schema that can say
// what it says - the new commands and the old commands that carry something
// new in the third, and everything else exactly where it was before, so that
// a cluster with a member of an older release goes on working.
func TestVersionOf(t *testing.T) {
	t.Parallel()
	plain := porttest.SampleVersion("b", "k", 1)
	with := func(change func(v *object.Version)) *versionRecord {
		v := plain
		change(&v)
		return versionToRecord(v)
	}
	upload := porttest.SampleUpload("b", "k", 1)
	tagged := upload
	tagged.Tags = map[string]string{"stage": "draft"}
	emptyTags := upload
	emptyTags.Tags = map[string]string{}
	stamp := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	config := func(kind string, document string) *bucketConfigCmd {
		return &bucketConfigCmd{Bucket: "b", Kind: kind, Document: []byte(document)}
	}

	tests := map[string]struct {
		e    entry
		want byte
	}{
		// Schema 1: what the first release wrote.
		"bucket.create":            {entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b"}}, entrySchemaVersion},
		"bucket.delete":            {entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "b"}}, entrySchemaVersion},
		"object.put":               {entry{Type: cmdPutObject, PutObject: versionToRecord(plain)}, entrySchemaVersion},
		"object.delete":            {entry{Type: cmdDeleteObject, DeleteObject: &deleteObjectCmd{Bucket: "b", Key: "k"}}, entrySchemaVersion},
		"root.set":                 {entry{Type: cmdSetRoot, SetRoot: &rootRecord{AccessKeyID: "ROOT"}}, entrySchemaVersion},
		"topology.set":             {entry{Type: cmdSetTopology, SetTopology: &topologyRecord{}}, entrySchemaVersion},
		"node.set":                 {entry{Type: cmdSetNode, SetNode: &nodeRecord{ID: "n1", MetadataVersion: 3}}, entrySchemaVersion},
		"node.remove":              {entry{Type: cmdRemoveNode, RemoveNode: &removeNodeCmd{ID: "n1"}}, entrySchemaVersion},
		"iam.user.put":             {entry{Type: cmdPutUser, IAM: &iamCmd{User: &userRecord{Name: "albie"}}}, entrySchemaVersion},
		"iam.policy.delete":        {entry{Type: cmdDeletePolicy, IAM: &iamCmd{Name: "p"}}, entrySchemaVersion},
		"audit.append":             {entry{Type: cmdAppendAudit, IAM: &iamCmd{Audit: &auditRecord{Actor: "root"}}}, entrySchemaVersion},
		"session.put":              {entry{Type: cmdPutSession, IAM: &iamCmd{Session: &sessionRecord{User: "albie"}}}, entrySchemaVersion},
		"idempotency.begin":        {entry{Type: cmdBeginIdem, IAM: &iamCmd{Idem: &idemRecord{ID: "x"}}}, entrySchemaVersion},
		"iam with an audit record": {entry{Type: cmdDeleteUser, IAM: &iamCmd{Name: "albie", Audit: &auditRecord{Actor: "root"}}}, entrySchemaVersion},

		// Schema 2: the leader's time, and what milestone 6 added.
		"any entry with a time":                {entry{Type: cmdDeleteBucket, At: stamp, DeleteBucket: &deleteBucketCmd{Name: "b"}}, entrySchemaTimed},
		"bucket.create with versions":          {entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", Versioning: "Enabled"}}, entrySchemaTimed},
		"bucket.create with Object Lock":       {entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", ObjectLock: true}}, entrySchemaTimed},
		"object.put with a lock":               {entry{Type: cmdPutObject, PutObject: with(func(v *object.Version) { v.Lock.LegalHold = true })}, entrySchemaTimed},
		"object.put with an encryption mode":   {entry{Type: cmdPutObject, PutObject: with(func(v *object.Version) { v.Encryption.Mode = object.SSEKMS })}, entrySchemaTimed},
		"object.put with tags":                 {entry{Type: cmdPutObject, PutObject: with(func(v *object.Version) { v.Tags = map[string]string{"a": "b"} })}, entrySchemaTimed},
		"bucket.versioning.put":                {entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "b", Status: "Enabled"}}, entrySchemaTimed},
		"object.version.delete":                {entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "b", Key: "k"}}, entrySchemaTimed},
		"bucket.lock.put":                      {entry{Type: cmdSetBucketLock, SetBucketLock: &bucketLockCmd{Bucket: "b"}}, entrySchemaTimed},
		"object.retention.put":                 {entry{Type: cmdSetRetention, SetRetention: &retentionCmd{Bucket: "b", Key: "k"}}, entrySchemaTimed},
		"object.legalhold.put":                 {entry{Type: cmdSetLegalHold, SetLegalHold: &legalHoldCmd{Bucket: "b", Key: "k"}}, entrySchemaTimed},
		"object.tags.put":                      {entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: "b", Key: "k", Tags: map[string]string{"a": "b"}}}, entrySchemaTimed},
		"upload.create":                        {entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(upload)}, entrySchemaTimed},
		"upload.create with an empty tag set":  {entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(emptyTags)}, entrySchemaTimed},
		"upload.part.put":                      {entry{Type: cmdPutPart, PutPart: &putPartCmd{Bucket: "b", Key: "k"}}, entrySchemaTimed},
		"upload.complete":                      {entry{Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{Bucket: "b", Key: "k"}}, entrySchemaTimed},
		"upload.abort":                         {entry{Type: cmdAbortUpload, AbortUpload: &uploadRefCmd{Bucket: "b", Key: "k"}}, entrySchemaTimed},
		"bucket.config.put of a policy":        {entry{Type: cmdSetBucketConfig, SetBucketConfig: config(bucket.ConfigPolicy, "{}")}, entrySchemaTimed},
		"bucket.config.put of CORS rules":      {entry{Type: cmdSetBucketConfig, SetBucketConfig: config(bucket.ConfigCORS, "{}")}, entrySchemaTimed},
		"bucket.config.put of bucket tags":     {entry{Type: cmdSetBucketConfig, SetBucketConfig: config(bucket.ConfigTagging, "{}")}, entrySchemaTimed},
		"bucket.config.put of a lifecycle":     {entry{Type: cmdSetBucketConfig, SetBucketConfig: config(bucket.ConfigLifecycle, "{}")}, entrySchemaTimed},
		"bucket.config.put of an unknown kind": {entry{Type: cmdSetBucketConfig, SetBucketConfig: config("acl", "{}")}, entrySchemaTimed},

		// Schema 3: the new commands...
		"object.reads.add":      {entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: "n1"}}, entrySchema3},
		"subbucket.put":         {entry{Type: cmdPutSubBucket, PutSubBucket: &subBucketCmd{Bucket: "b"}}, entrySchema3},
		"subbucket.delete":      {entry{Type: cmdDeleteSubBucket, DeleteSubBucket: &subBucketRefCmd{Bucket: "b"}}, entrySchema3},
		"tag.definition.put":    {entry{Type: cmdPutTagDefinition, PutTagDefinition: &tagDefinitionCmd{}}, entrySchema3},
		"tag.definition.delete": {entry{Type: cmdDeleteTagDefinition, DeleteTagDefinition: &tagDefinitionRefCmd{Key: "k"}}, entrySchema3},
		"a new command with a time": {
			entry{Type: cmdAddReads, At: stamp, AddReads: &addReadsCmd{Node: "n1"}}, entrySchema3,
		},
		// ...and the old ones that say something new.
		"object.put that says who": {
			entry{Type: cmdPutObject, PutObject: with(func(v *object.Version) { v.CreatedBy = "user/albie" })}, entrySchema3,
		},
		"object.put that says who, with tags and a time": {
			entry{Type: cmdPutObject, At: stamp, PutObject: with(func(v *object.Version) { v.CreatedBy, v.Tags = "root", map[string]string{"a": "b"} })}, entrySchema3,
		},
		"upload.complete that says who": {
			entry{Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{Bucket: "b", Key: "k", CreatedBy: "key/AKID"}}, entrySchema3,
		},
		"upload.create with tags": {entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(tagged)}, entrySchema3},
		"bucket.config.put of tag rules": {
			entry{Type: cmdSetBucketConfig, SetBucketConfig: config(bucket.ConfigTagRules, `{"version":1}`)}, entrySchema3,
		},
		"bucket.config.put that removes tag rules": {
			entry{Type: cmdSetBucketConfig, SetBucketConfig: config(bucket.ConfigTagRules, "")}, entrySchema3,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := versionOf(test.e); got != test.want {
				t.Errorf("versionOf = %d, want %d", got, test.want)
			}
			// Whatever schema an entry needs, this binary writes and
			// reads it there.
			data, err := encodeEntry(test.e, test.want)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeEntry(data, MetadataVersion); err != nil {
				t.Errorf("the entry does not decode in its own schema: %v", err)
			}
		})
	}
	if MetadataVersion != entrySchema3 {
		t.Errorf("MetadataVersion = %d: a node must announce that it applies schema %d", MetadataVersion, entrySchema3)
	}
}

// schema3Commands is one entry of every command schema 3 added, with every
// field set.
func schema3Commands() []entry {
	trail := &auditRecord{
		Time: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), Actor: "user/albie", Action: "s3:PutSubBucket", Resource: "b/reports",
		Outcome: "succeeded", Details: map[string]string{"k": "v"},
	}
	created := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	return []entry{
		{Type: cmdAddReads, AddReads: &addReadsCmd{Node: "node-a", Boot: 1 << 63, Seq: 42, Items: []readItem{
			{Bucket: "b", Key: "k", Version: object.NullVersion, VersionID: "0f0e0d0c0b0a09080706050403020100", Who: "user/albie", Downloads: 3},
			{
				Bucket: "b", Key: "k\x00odd", Version: "000102030405060708090a0b0c0d0e0f", VersionID: "000102030405060708090a0b0c0d0e0f",
				Who: "key/AKID", Presigned: true, Copies: 2,
			},
		}}},
		{Type: cmdPutSubBucket, PutSubBucket: &subBucketCmd{
			Bucket: "b", SubBucket: subBucketRecord{Name: "reports", CreatedAt: created, Policy: []byte(`{"Statement":[]}`)}, Audit: trail,
		}},
		{Type: cmdDeleteSubBucket, DeleteSubBucket: &subBucketRefCmd{Bucket: "b", Name: "reports", Audit: trail}},
		{Type: cmdPutTagDefinition, PutTagDefinition: &tagDefinitionCmd{
			Definition: tagDefinitionRecord{Key: "stage", Description: "Where the work is.", Values: []string{"draft", "final"}, CreatedAt: created},
			Audit:      trail,
		}},
		{Type: cmdDeleteTagDefinition, DeleteTagDefinition: &tagDefinitionRefCmd{Key: "stage", Audit: trail}},
	}
}

// TestCommandsOfSchema3: the new commands are written in the third schema
// and come back as they went in. A binary of the second refuses to apply
// them - it stops - and in an entry that claims an older schema they are no
// commands at all, as they are none to the binary of that schema.
func TestCommandsOfSchema3(t *testing.T) {
	t.Parallel()
	for _, e := range schema3Commands() {
		t.Run(e.Type, func(t *testing.T) {
			t.Parallel()
			if got := versionOf(e); got != entrySchema3 {
				t.Fatalf("written in schema %d, want %d", got, entrySchema3)
			}
			current, err := encodeEntry(e, entrySchema3)
			if err != nil {
				t.Fatal(err)
			}
			if current[0] != entrySchema3 {
				t.Errorf("first byte = %d, want the schema %d", current[0], entrySchema3)
			}
			decoded, err := decodeEntry(current, entrySchema3)
			if err != nil || !reflect.DeepEqual(decoded, e) {
				t.Errorf("round trip:\n got %+v, %v\nwant %+v", decoded, err, e)
			}
			for _, older := range []byte{entrySchemaVersion, entrySchemaTimed} {
				if _, err := decodeEntry(current, older); !errors.Is(err, domain.ErrUnsupportedFormat) {
					t.Errorf("read by a binary of schema %d = %v, want %v", older, err, domain.ErrUnsupportedFormat)
				}
				claimed, err := encodeEntry(e, older)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := decodeEntry(claimed, entrySchema3); !errors.Is(err, domain.ErrUnsupportedFormat) {
					t.Errorf("in an entry of schema %d = %v, want %v", older, err, domain.ErrUnsupportedFormat)
				}
			}
			// A command without its payload, or with another's as well,
			// is no command.
			bare, _ := encodeEntry(entry{Type: e.Type}, entrySchema3)
			if _, err := decodeEntry(bare, entrySchema3); !errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Errorf("without a payload = %v, want %v", err, domain.ErrUnsupportedFormat)
			}
			two := e
			two.DeleteBucket = &deleteBucketCmd{Name: "b"}
			double, _ := encodeEntry(two, entrySchema3)
			if _, err := decodeEntry(double, entrySchema3); !errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Errorf("with a second payload = %v, want %v", err, domain.ErrUnsupportedFormat)
			}
			other := entry{
				Type: cmdDeleteBucket, AddReads: e.AddReads, PutSubBucket: e.PutSubBucket, DeleteSubBucket: e.DeleteSubBucket,
				PutTagDefinition: e.PutTagDefinition, DeleteTagDefinition: e.DeleteTagDefinition,
			}
			wrong, _ := encodeEntry(other, entrySchema3)
			if _, err := decodeEntry(wrong, entrySchema3); !errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Errorf("as the payload of another command = %v, want %v", err, domain.ErrUnsupportedFormat)
			}
		})
	}

	// The old commands that carry something new round-trip with it.
	v := porttest.SampleVersion("b", "k", 1)
	v.CreatedBy = "user/albie"
	u := porttest.SampleUpload("b", "k", 1)
	u.Tags = map[string]string{"stage": "draft"}
	for _, e := range []entry{
		{Type: cmdPutObject, PutObject: versionToRecord(v)},
		{Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{Bucket: "b", Key: "k", UploadID: u.ID.String(), CreatedBy: "key/AKID"}},
		{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)},
		{Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Bucket: "b", Kind: bucket.ConfigTagRules, Document: []byte(`{"version":1}`)}},
	} {
		data, err := encodeEntry(e, versionOf(e))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeEntry(data, entrySchema3)
		if err != nil {
			t.Fatalf("%s: %v", e.Type, err)
		}
		switch {
		case decoded.PutObject != nil && decoded.PutObject.CreatedBy != "user/albie",
			decoded.CompleteUpload != nil && decoded.CompleteUpload.CreatedBy != "key/AKID",
			decoded.CreateUpload != nil && decoded.CreateUpload.Tags["stage"] != "draft",
			decoded.SetBucketConfig != nil && decoded.SetBucketConfig.Kind != bucket.ConfigTagRules:
			t.Errorf("%s lost what it carried: %+v", e.Type, decoded)
		}
		if _, err := decodeEntry(data, entrySchemaTimed); !errors.Is(err, domain.ErrUnsupportedFormat) {
			t.Errorf("%s of schema 3 read by a binary of schema 2 = %v, want %v", e.Type, err, domain.ErrUnsupportedFormat)
		}
	}
}

// TestHeldTo: below schema 3 a put and a completion go without who made the
// version, and nothing else of an entry changes - the caller's entry least of
// all.
func TestHeldTo(t *testing.T) {
	t.Parallel()
	v := porttest.SampleVersion("b", "k", 1)
	v.CreatedBy = "user/albie"
	v.Tags = map[string]string{"a": "b"}
	u := porttest.SampleUpload("b", "k", 1)
	u.Tags = map[string]string{"stage": "draft"}
	tests := map[string]struct {
		e     entry
		level byte
		want  byte // the schema the entry needs once it is held to the level
	}{
		"a put at level 3 keeps who":            {entry{Type: cmdPutObject, PutObject: versionToRecord(v)}, entrySchema3, entrySchema3},
		"a put at level 2 goes without":         {entry{Type: cmdPutObject, PutObject: versionToRecord(v)}, entrySchemaTimed, entrySchemaTimed},
		"a put at level 1 goes without":         {entry{Type: cmdPutObject, PutObject: versionToRecord(v)}, entrySchemaVersion, entrySchemaTimed},
		"a completion at level 3 keeps who":     {entry{Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{CreatedBy: "root"}}, entrySchema3, entrySchema3},
		"a completion at level 2 goes without":  {entry{Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{CreatedBy: "root"}}, entrySchemaTimed, entrySchemaTimed},
		"an upload with tags is not cut down":   {entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)}, entrySchemaTimed, entrySchema3},
		"tag rules are not cut down":            {entry{Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Kind: bucket.ConfigTagRules}}, entrySchemaTimed, entrySchema3},
		"a new command is not cut down":         {entry{Type: cmdPutSubBucket, PutSubBucket: &subBucketCmd{Bucket: "b"}}, entrySchemaTimed, entrySchema3},
		"an entry of the first schema is as is": {entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "b"}}, entrySchemaVersion, entrySchemaVersion},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			before, _ := json.Marshal(test.e)
			held := heldTo(test.e, test.level)
			if got := versionOf(held); got != test.want {
				t.Errorf("held to level %d the entry needs schema %d, want %d", test.level, got, test.want)
			}
			if after, _ := json.Marshal(test.e); !bytes.Equal(before, after) {
				t.Errorf("heldTo changed the caller's entry:\n%s\n%s", before, after)
			}
			if test.level < entrySchema3 && held.PutObject != nil && (held.PutObject.CreatedBy != "" || held.PutObject.Tags["a"] != "b" || held.PutObject.DataID != test.e.PutObject.DataID) {
				t.Errorf("a put held to level %d = %+v", test.level, held.PutObject)
			}
		})
	}
}

// TestAnEntryOfTheSecondSchemaSaysNothingOfTheThird: who made a version,
// the tags of an upload, the tag rules of a bucket and the commands of
// schema 3 exist in entries of schema 3 only. An entry that claims the
// second and brings them anyway is applied as the binary of the second
// applies it - without - and leaves a state that binary still opens.
func TestAnEntryOfTheSecondSchemaSaysNothingOfTheThird(t *testing.T) {
	t.Parallel()
	u := porttest.SampleUpload("b", "upload", 1)
	u.Tags = map[string]string{"stage": "draft"}
	part := porttest.SamplePart(u, 1, 10, 0)
	v := porttest.SampleVersion("b", "put", 2)
	v.CreatedBy = "user/mallory"
	completed := object.ID(bytes.Repeat([]byte{9}, object.IDSize)).String()
	entries := []entry{
		{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b"}},
		{Type: cmdPutObject, PutObject: versionToRecord(v)},
		{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)},
		{Type: cmdPutPart, PutPart: &putPartCmd{Bucket: "b", Key: u.Key, UploadID: u.ID.String(), Part: partToRecord(part)}},
		{Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{
			Bucket: "b", Key: u.Key, UploadID: u.ID.String(), VersionID: completed, ETag: "etag", CreatedBy: "user/mallory",
			Parts: []listedPart{{Number: 1, ETag: part.ETag, DataID: part.DataID.String(), Seal: bytes.Repeat([]byte{1}, 32)}},
		}},
	}
	rules := entry{Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Bucket: "b", Kind: bucket.ConfigTagRules, Document: []byte(`{"version":1,"lowercase":true,"catalogue_only":false,"required":[]}`)}}
	stored := func(m *machine, key string) *versionRecord {
		t.Helper()
		var found *versionRecord
		if err := m.f.view(func(tx *bbolt.Tx) (err error) {
			found, err = readVersion(tx.Bucket(objectsBucket()).Bucket([]byte("b")), key)
			return err
		}); err != nil || found == nil {
			t.Fatalf("version of %s = %v, %v", key, found, err)
		}
		return found
	}
	tagRules := func(m *machine) []byte {
		t.Helper()
		var document []byte
		if err := m.f.view(func(tx *bbolt.Tx) error {
			b, err := readBucket(tx, "b")
			if err == nil && b != nil {
				document = b.Config[bucket.ConfigTagRules]
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return document
	}

	t.Run("in entries of schema 2", func(t *testing.T) {
		t.Parallel()
		m := newMachine(t)
		for _, e := range entries {
			r := m.as(entrySchemaTimed, e)
			if r.err != nil {
				t.Fatalf("%s: %v", e.Type, r.err)
			}
			// What the leader answers is what was stored.
			if r.stored != nil && r.stored.CreatedBy != "" {
				t.Errorf("%s answered that the version was made by %q", e.Type, r.stored.CreatedBy)
			}
		}
		if got := stored(m, "put"); got.CreatedBy != "" {
			t.Errorf("a put of schema 2 recorded who made the version: %q", got.CreatedBy)
		}
		if got := stored(m, "upload"); got.CreatedBy != "" || len(got.Tags) != 0 {
			t.Errorf("an upload of schema 2 made a version by %q with tags %v", got.CreatedBy, got.Tags)
		}
		// Tag rules are refused as a kind there is not: a refusal, like
		// the one a binary of schema 2 gives, and no stop.
		if r := m.as(entrySchemaTimed, rules); !errors.Is(r.err, domain.ErrInvalidArgument) || len(r.events) != 0 {
			t.Errorf("tag rules in an entry of schema 2 = %v, events %d; want %v", r.err, len(r.events), domain.ErrInvalidArgument)
		}
		if got := tagRules(m); got != nil {
			t.Errorf("tag rules from an entry of schema 2 were stored: %s", got)
		}
		// A payload of schema 3 beside a command of schema 2 is not seen,
		// as that binary does not see it.
		beside := append([]byte{entrySchemaTimed}, `{"type":"bucket.create","at":"2026-10-12T10:00:00Z","create_bucket":{"name":"c","created_at":"2026-10-12T10:00:00Z"},`+
			`"put_tag_definition":{"definition":{"key":"smuggled"}},"add_reads":{"node":"n1","boot":1,"seq":1,"items":[{"bucket":"b","key":"put","version":"null","who":"x","downloads":9}]}}`...)
		m.index++
		if r := result(t, m.f.Apply(&raft.Log{Index: m.index, Type: raft.LogCommand, Data: beside})); r.err != nil || r.fatal {
			t.Fatalf("a command of schema 2 with payloads of schema 3 beside it: %v", r.err)
		}
		if err := m.f.view(func(tx *bbolt.Tx) error {
			if b, err := readBucket(tx, "c"); err != nil || b == nil {
				t.Errorf("the command itself was not applied: %v, %v", b, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		// The state is one the previous release opens: marked as schema
		// 2, and with none of what schema 3 keeps.
		if got := stateSchema(t, m.f); got != stateSchemaTimed {
			t.Errorf("state schema = %d after entries of schema 2 only, want %d", got, stateSchemaTimed)
		}
		if got := rawState(t, m.f, schema3Buckets()...); len(got) != 0 {
			t.Errorf("entries of schema 2 made buckets of schema 3 in the state: %v", got)
		}
	})

	t.Run("in entries of schema 3", func(t *testing.T) {
		t.Parallel()
		m := newMachine(t)
		for _, e := range append(entries, rules) {
			if r := m.as(entrySchema3, e); r.err != nil {
				t.Fatalf("%s: %v", e.Type, r.err)
			}
		}
		if got := stored(m, "put"); got.CreatedBy != "user/mallory" {
			t.Errorf("a put of schema 3 made by %q", got.CreatedBy)
		}
		if got := stored(m, "upload"); got.CreatedBy != "user/mallory" || got.Tags["stage"] != "draft" {
			t.Errorf("an upload of schema 3 made a version by %q with tags %v", got.CreatedBy, got.Tags)
		}
		if got := tagRules(m); !bytes.Equal(got, rules.SetBucketConfig.Document) {
			t.Errorf("tag rules from an entry of schema 3 = %s", got)
		}
		if got := stateSchema(t, m.f); got != stateSchema3 {
			t.Errorf("state schema = %d after entries of schema 3, want %d", got, stateSchema3)
		}
	})

	t.Run("a command of schema 3 in an entry of schema 2 stops the state machine", func(t *testing.T) {
		t.Parallel()
		// As it stops the binary of schema 2, which does not know the
		// command: both stop, neither guesses.
		for _, e := range schema3Commands() {
			m := newMachine(t)
			if r := m.raw(entrySchemaTimed, e); !r.fatal || !errors.Is(r.err, domain.ErrUnsupportedFormat) {
				t.Errorf("%s in an entry of schema 2 = %+v, want the state machine to stop", e.Type, r.err)
			}
		}
	})
}

// TestStateBecomesSchema3WithItsFirstEntryOfSchema3: until an entry of
// schema 3 is applied the state stays what the previous release wrote, and
// can go back to it. From then on it says 3, whatever comes after, and this
// binary opens it; a state of a schema it does not know it refuses.
func TestStateBecomesSchema3WithItsFirstEntryOfSchema3(t *testing.T) {
	t.Parallel()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	f, err := openFSM(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.close() }()
	m := &machine{t: t, f: f, at: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}

	m.as(entrySchemaVersion, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "one"}})
	if got := stateSchema(t, m.f); got != stateSchemaVersion {
		t.Fatalf("after an entry of schema 1: state schema %d", got)
	}
	m.as(entrySchemaTimed, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "two"}})
	m.put("two", "k", 1)
	if got := stateSchema(t, m.f); got != stateSchemaTimed {
		t.Fatalf("after entries of schema 2: state schema %d", got)
	}
	if got := rawState(t, m.f, schema3Buckets()...); len(got) != 0 {
		t.Fatalf("a state of schema 2 holds %v", got)
	}
	// The first entry of schema 3 marks the state, even one that is
	// refused: a binary of schema 2 would have stopped at it.
	refused := m.as(entrySchema3, entry{Type: cmdPutSubBucket, PutSubBucket: &subBucketCmd{Bucket: "missing", SubBucket: subBucketRecord{Name: "s"}}})
	if !errors.Is(refused.err, domain.ErrBucketNotFound) {
		t.Fatalf("a sub-bucket of a missing bucket = %v", refused.err)
	}
	if got := stateSchema(t, m.f); got != stateSchema3 {
		t.Fatalf("after an entry of schema 3: state schema %d", got)
	}
	// Entries of older schemas that follow do not take it back.
	m.as(entrySchemaTimed, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "three"}})
	m.as(entrySchemaVersion, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "four"}})
	if got := stateSchema(t, m.f); got != stateSchema3 {
		t.Errorf("after older entries the state schema went back to %d", got)
	}
	// A replayed entry of schema 3 on a state that has it changes nothing.
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	if f, err = openFSM(root); err != nil {
		t.Fatalf("reopening a state of schema 3: %v", err)
	}
	if got := stateSchema(t, f); got != stateSchema3 {
		t.Errorf("after reopening: state schema %d", got)
	}
	if err := f.db.Update(func(tx *bbolt.Tx) error { return raiseStateSchema(tx, stateSchema3+1) }); err != nil {
		t.Fatal(err)
	}
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openFSM(root); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Errorf("opening a state of schema %d = %v, want %v", stateSchema3+1, err, domain.ErrUnsupportedFormat)
	}
}

// populateSchema3 writes something into everything schema 3 keeps, and
// returns the versions that were read.
func populateSchema3(m *machine) []versionRecord {
	m.t.Helper()
	m.bucket("plain", bucket.Unversioned)
	m.bucket("ver", bucket.VersioningEnabled)
	versions := []versionRecord{m.put("plain", "a", 1), m.put("plain", "reports/q3", 2), m.put("ver", "k", 3), m.put("ver", "k", 4)}
	// More readers than a record names, so that folding is in it; reads
	// of several nodes; a copy; a presigned read.
	for i := range object.MaxReaders + 5 {
		if r := m.reads("node-a", 7, uint64(i+1), fmt.Sprintf("user/%02d", i), versions[0], versions[2]); r.err != nil {
			m.t.Fatal(r.err)
		}
	}
	m.ok(entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: "node-b", Boot: 1, Seq: 1, Items: []readItem{
		{Bucket: "ver", Key: "k", Version: refOf(versions[3]), VersionID: versions[3].VersionID, Who: "key/AKID", Presigned: true, Downloads: 2},
		{Bucket: "plain", Key: "reports/q3", Version: object.NullVersion, VersionID: versions[1].VersionID, Who: "anonymous", Copies: 1},
		{Bucket: "plain", Key: "gone", Version: object.NullVersion, VersionID: versions[1].VersionID, Who: "anonymous", Downloads: 1},
	}}})
	trail := &auditRecord{Time: m.at, Actor: "root", Action: "s3:PutSubBucket", Outcome: "succeeded"}
	for _, name := range []string{"reports", "archive"} {
		m.ok(entry{Type: cmdPutSubBucket, PutSubBucket: &subBucketCmd{
			Bucket: "plain", SubBucket: subBucketRecord{Name: name, CreatedAt: m.at, Policy: []byte(`{"Statement":[]}`)}, Audit: trail,
		}})
	}
	m.ok(entry{Type: cmdPutSubBucket, PutSubBucket: &subBucketCmd{Bucket: "ver", SubBucket: subBucketRecord{Name: "x", CreatedAt: m.at}}})
	for _, key := range []string{"stage", "team"} {
		m.ok(entry{Type: cmdPutTagDefinition, PutTagDefinition: &tagDefinitionCmd{
			Definition: tagDefinitionRecord{Key: key, Description: "d", Values: []string{"a", "b"}, CreatedAt: m.at}, Audit: trail,
		}})
	}
	m.ok(entry{Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{
		Bucket: "plain", Kind: bucket.ConfigTagRules, Document: []byte(`{"version":1,"lowercase":true,"catalogue_only":false,"required":[{"key":"stage"}]}`),
	}})
	u := porttest.SampleUpload("ver", "reports/upload", 5)
	u.Tags = map[string]string{"stage": "a"}
	m.ok(entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)})
	by := porttest.SampleVersion("ver", "by", 6)
	by.CreatedBy = "user/albie"
	m.ok(entry{Type: cmdPutObject, PutObject: versionToRecord(by)})
	return versions
}

// everyBucket names every top-level bucket of a state.
func everyBucket(t *testing.T, f *fsm) [][]byte {
	t.Helper()
	var names [][]byte
	if err := f.view(func(tx *bbolt.Tx) error {
		return tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
			names = append(names, bytes.Clone(name))
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return names
}

// TestSnapshotAndRestoreOfSchema3: a snapshot carries everything schema 3
// keeps - records of reads, the last batch of every node, sub-buckets, tag
// rules, the catalogue, who made a version, the tags of an upload - and a
// replica restored from it is, byte for byte, the replica it was taken on,
// and goes on like it.
func TestSnapshotAndRestoreOfSchema3(t *testing.T) {
	t.Parallel()
	source := newMachine(t)
	versions := populateSchema3(source)
	for _, name := range schema3Buckets() {
		if got := rawState(t, source.f, name); len(got) < 2 {
			t.Fatalf("the state to snapshot has nothing in %q: %v", name, got)
		}
	}

	snap, err := source.f.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatal(err)
	}
	snap.Release()
	target := newMachine(t)
	target.bucket("discarded", bucket.Unversioned)
	if err := target.f.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	target.index, target.at = source.index, source.at

	want, got := rawState(t, source.f, everyBucket(t, source.f)...), rawState(t, target.f, everyBucket(t, target.f)...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the restored state differs from the one the snapshot was taken of:\n got %v\nwant %v", got, want)
	}
	if schema := stateSchema(t, target.f); schema != stateSchema3 {
		t.Errorf("the restored state says schema %d", schema)
	}
	// Both go on alike: a batch that was applied before the snapshot is
	// known to the restored replica too, and a new one counts on both.
	for _, m := range []*machine{source, target} {
		before, _ := accessOf(t, m.f, versions[0])
		m.reads("node-a", 7, 3, "user/again", versions[0])
		if after, _ := accessOf(t, m.f, versions[0]); after.Downloads != before.Downloads {
			t.Errorf("a batch from before the snapshot was applied again: %d downloads, were %d", after.Downloads, before.Downloads)
		}
		m.reads("node-a", 7, 1000, "user/new", versions[0])
		if after, _ := accessOf(t, m.f, versions[0]); after.Downloads != before.Downloads+1 {
			t.Errorf("a new batch after the snapshot: %d downloads, were %d", after.Downloads, before.Downloads)
		}
		if r := m.apply(entry{Type: cmdDeleteSubBucket, DeleteSubBucket: &subBucketRefCmd{Bucket: "plain", Name: "reports"}}); !errors.Is(r.err, domain.ErrSubBucketNotEmpty) {
			t.Errorf("deleting a sub-bucket that holds an object = %v", r.err)
		}
		if r := m.apply(entry{Type: cmdDeleteTagDefinition, DeleteTagDefinition: &tagDefinitionRefCmd{Key: "stage"}}); !errors.Is(r.err, domain.ErrConflict) {
			t.Errorf("deleting a definition a rule names = %v", r.err)
		}
	}
	want, got = rawState(t, source.f, everyBucket(t, source.f)...), rawState(t, target.f, everyBucket(t, target.f)...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the two replicas parted after the snapshot")
	}
}

// TestReplicasKeepIdenticalRecordsOfReads: the record of who read a version
// is computed from the entries alone - the reads in their order and the
// leader's time of each entry - so two replicas that apply the same log hold
// the same bytes. That includes which readers were folded into "others", a
// leader whose clock went back, and a batch that came twice.
func TestReplicasKeepIdenticalRecordsOfReads(t *testing.T) {
	t.Parallel()
	a, b := newMachine(t), newMachine(t)
	var versions []versionRecord
	var newest time.Time
	for _, m := range []*machine{a, b} {
		versions = populateSchema3(m)
		// A leader with a slow clock: the state's time does not go back,
		// and so neither does the time of a read.
		newest = m.at
		m.at = m.at.Add(-time.Hour)
		m.reads("node-c", 1, 1, "user/late", versions...)
		m.reads("node-c", 1, 1, "user/late", versions...)
		m.at = m.at.Add(2 * time.Hour)
		m.reads("node-c", 1, 2, "user/00", versions[0])
	}
	want, got := rawState(t, a.f, accessBucket(), accessFlushBucket()), rawState(t, b.f, accessBucket(), accessFlushBucket())
	if len(want) < 6 {
		t.Fatalf("the replicas hold too little to compare: %v", want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("two replicas that applied the same entries hold different records:\n%v\n%v", want, got)
	}
	// And what they hold is what the reads add up to.
	first, found := accessOf(t, a.f, versions[0])
	if !found || first.Downloads != object.MaxReaders+5+2 || len(first.Readers) != object.MaxReaders || first.OtherReaders < 5 {
		t.Errorf("the record of the version read most = %+v", first)
	}
	var late *object.Reader
	for i := range first.Readers {
		if first.Readers[i].Who == "user/late" {
			late = &first.Readers[i]
		}
	}
	if late == nil || late.Downloads != 1 {
		t.Fatalf("the reader of the batch that came twice = %+v", late)
	}
	// The read stamped an hour early has the time of the entry before it.
	if !late.LastAt.Equal(newest) {
		t.Errorf("a read stamped by a slow clock was recorded at %v, want the time of the entry before it, %v", late.LastAt, newest)
	}
	// A third replica that applies them in another order is another log,
	// and need not agree; one that skips an entry does not.
	c := newMachine(t)
	populateSchema3(c)
	if reflect.DeepEqual(rawState(t, c.f, accessBucket()), want) {
		t.Error("a replica that applied fewer entries holds the same records: the comparison shows nothing")
	}
}

// TestABatchOfReadsIsAppliedOnce: a node sends a batch again when it does
// not know what became of it. The state remembers the last of every node and
// applies a repeated one as nothing - not a byte of any record changes.
func TestABatchOfReadsIsAppliedOnce(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.bucket("b", bucket.Unversioned)
	v := m.put("b", "k", 1)

	if r := m.reads("node-a", 7, 1, "user/albie", v); r.err != nil || len(r.events) != 0 {
		t.Fatalf("the first batch: %v, %d events; want none of either", r.err, len(r.events))
	}
	once := rawState(t, m.f, accessBucket(), accessFlushBucket())
	if got := once["accessflush/"+fmt.Sprintf("%x", "node-a")]; got != `{"boot":7,"seq":1}` {
		t.Errorf("the last batch of the node is recorded as %s", got)
	}
	for i := range 3 {
		if r := m.reads("node-a", 7, 1, "user/albie", v); r.err != nil {
			t.Fatalf("repeat %d: %v", i, r.err)
		}
		if got := rawState(t, m.f, accessBucket(), accessFlushBucket()); !reflect.DeepEqual(got, once) {
			t.Fatalf("repeat %d changed the state:\n%v\n%v", i, once, got)
		}
	}
	if access, _ := accessOf(t, m.f, v); access.Downloads != 1 {
		t.Errorf("after four deliveries of one batch: %d downloads", access.Downloads)
	}
	// A batch with other reads under the numbers of one that was applied
	// is the same batch, as far as the state can tell.
	if r := m.reads("node-a", 7, 1, "user/other", v); r.err != nil {
		t.Fatal(r.err)
	}
	if access, _ := accessOf(t, m.f, v); access.Downloads != 1 || len(access.Readers) != 1 {
		t.Errorf("a batch under used numbers was applied: %+v", access)
	}
	// The next one counts, and a node that restarted begins again.
	m.reads("node-a", 7, 2, "user/albie", v)
	m.reads("node-a", 9, 1, "user/albie", v)
	if access, _ := accessOf(t, m.f, v); access.Downloads != 3 {
		t.Errorf("after two new batches: %d downloads, want 3", access.Downloads)
	}
	// An empty batch is a batch: it moves the node on.
	m.reads("node-a", 9, 5, "user/albie")
	m.reads("node-a", 9, 4, "user/albie", v)
	if access, _ := accessOf(t, m.f, v); access.Downloads != 3 {
		t.Errorf("a batch numbered before an empty one that was applied: %d downloads, want 3", access.Downloads)
	}
}

// TestRemovingANodeForgetsItsBatches: what the state remembers of a node's
// batches goes when the node is removed from the cluster, registered or not.
func TestRemovingANodeForgetsItsBatches(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.bucket("b", bucket.Unversioned)
	v := m.put("b", "k", 1)
	// The batches come first: once a node has registered, the state takes
	// batches from registered nodes only.
	for _, node := range []string{"node-a", "node-b", "node-c"} {
		m.reads(node, 1, 1, "user/albie", v)
	}
	m.ok(entry{Type: cmdSetNode, SetNode: &nodeRecord{ID: "node-a", Roles: []string{"gateway"}, MetadataVersion: 3}})
	flushes := func() map[string]string { return rawState(t, m.f, accessFlushBucket()) }
	if got := flushes(); len(got) != 4 {
		t.Fatalf("batches of three nodes are remembered as %v", got)
	}
	// node-a had registered, node-b had not.
	for _, node := range []string{"node-a", "node-b"} {
		if r := m.as(entrySchemaTimed, entry{Type: cmdRemoveNode, RemoveNode: &removeNodeCmd{ID: node}}); r.err != nil {
			t.Fatal(r.err)
		}
	}
	got := flushes()
	if _, kept := got["accessflush/"+fmt.Sprintf("%x", "node-c")]; len(got) != 2 || !kept {
		t.Errorf("after two nodes were removed the state remembers %v, want node-c alone", got)
	}
	// The records of reads are not the node's: they stay.
	if access, _ := accessOf(t, m.f, v); access.Downloads != 3 {
		t.Errorf("removing nodes changed a record of reads: %+v", access)
	}
	// A state that never had a batch has nothing to forget, and does not
	// get a bucket for it.
	empty := newMachine(t)
	if r := empty.as(entrySchemaTimed, entry{Type: cmdRemoveNode, RemoveNode: &removeNodeCmd{ID: "node-a"}}); r.err != nil {
		t.Fatal(r.err)
	}
	if got := rawState(t, empty.f, schema3Buckets()...); len(got) != 0 {
		t.Errorf("removing a node made %v", got)
	}
}

// TestTheRecordOfReadsGoesWithItsVersion: every path that removes a version
// for good removes the record of who read it, in the same entry; a path that
// keeps the version keeps the record.
func TestTheRecordOfReadsGoesWithItsVersion(t *testing.T) {
	t.Parallel()
	remove := func(m *machine, v versionRecord) applyResult {
		return m.ok(entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: v.Bucket, Key: v.Key, VersionRef: refOf(v), MarkerID: v.VersionID, At: m.at}})
	}
	deleteObject := func(m *machine, key string, fill byte) applyResult {
		id := object.ID(bytes.Repeat([]byte{fill}, object.IDSize)).String()
		return m.ok(entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "b", Key: key, MarkerID: id, At: m.at}})
	}
	tests := map[string]struct {
		versioning bucket.Versioning
		// act removes, or does not remove, the version that was read,
		// and reports whether its record must be gone.
		act func(m *machine, read versionRecord) (gone bool)
	}{
		"a put over it, no versions kept": {bucket.Unversioned, func(m *machine, _ versionRecord) bool {
			m.put("b", "k", 2)
			return true
		}},
		"a delete, no versions kept": {bucket.Unversioned, func(m *machine, _ versionRecord) bool {
			deleteObject(m, "k", 9)
			return true
		}},
		"a delete of schema 1": {bucket.Unversioned, func(m *machine, _ versionRecord) bool {
			if r := m.as(entrySchemaVersion, entry{Type: cmdDeleteObject, DeleteObject: &deleteObjectCmd{Bucket: "b", Key: "k"}}); r.err != nil || r.previous == nil {
				m.t.Fatalf("object.delete = %+v, %v", r.previous, r.err)
			}
			return true
		}},
		"the version removed by name": {bucket.VersioningEnabled, func(m *machine, read versionRecord) bool {
			remove(m, read)
			return true
		}},
		"the version removed by name, no longer current": {bucket.VersioningEnabled, func(m *machine, read versionRecord) bool {
			m.put("b", "k", 2)
			remove(m, read)
			return true
		}},
		"a put over the null version, versioning suspended": {bucket.VersioningSuspended, func(m *machine, _ versionRecord) bool {
			m.put("b", "k", 2)
			return true
		}},
		"a delete over the null version, versioning suspended": {bucket.VersioningSuspended, func(m *machine, _ versionRecord) bool {
			if r := deleteObject(m, "k", 9); r.marker == nil || r.previous == nil {
				m.t.Fatalf("a delete under suspended versioning = %+v", r)
			}
			return true
		}},
		"an upload completed over it, no versions kept": {bucket.Unversioned, func(m *machine, _ versionRecord) bool {
			u := porttest.SampleUpload("b", "k", 3)
			part := porttest.SamplePart(u, 1, 10, 0)
			m.ok(entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)})
			m.ok(entry{Type: cmdPutPart, PutPart: &putPartCmd{Bucket: "b", Key: "k", UploadID: u.ID.String(), Part: partToRecord(part)}})
			r := m.ok(entry{Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{
				Bucket: "b", Key: "k", UploadID: u.ID.String(), VersionID: object.ID(bytes.Repeat([]byte{4}, object.IDSize)).String(), ETag: "e",
				Parts: []listedPart{{Number: 1, ETag: part.ETag, DataID: part.DataID.String(), Seal: bytes.Repeat([]byte{1}, 32)}},
			}})
			if r.previous == nil {
				m.t.Fatal("the completion removed nothing")
			}
			return true
		}},

		"a put over it, versions kept": {bucket.VersioningEnabled, func(m *machine, _ versionRecord) bool {
			m.put("b", "k", 2)
			return false
		}},
		"a delete marker over it": {bucket.VersioningEnabled, func(m *machine, _ versionRecord) bool {
			deleteObject(m, "k", 9)
			return false
		}},
		"a newer version removed, and it is current again": {bucket.VersioningEnabled, func(m *machine, _ versionRecord) bool {
			remove(m, m.put("b", "k", 2))
			return false
		}},
		"new tags": {bucket.Unversioned, func(m *machine, _ versionRecord) bool {
			m.ok(entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: "b", Key: "k", VersionRef: object.NullVersion, Tags: map[string]string{"a": "b"}}})
			return false
		}},
		"another key put and deleted": {bucket.Unversioned, func(m *machine, _ versionRecord) bool {
			m.put("b", "k2", 2)
			deleteObject(m, "k2", 9)
			return false
		}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMachine(t)
			m.bucket("b", bucket.Unversioned)
			m.bucket("other", bucket.Unversioned)
			var read versionRecord
			if test.versioning == bucket.VersioningSuspended {
				// The null version of a bucket that kept versions once.
				m.ok(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "b", Status: string(bucket.VersioningSuspended)}})
				read = m.put("b", "k", 1)
			} else {
				if test.versioning == bucket.VersioningEnabled {
					m.ok(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "b", Status: string(test.versioning)}})
				}
				read = m.put("b", "k", 1)
			}
			elsewhere := m.put("other", "k", 1)
			m.reads("node-a", 1, 1, "user/albie", read, elsewhere)
			if _, found := accessOf(t, m.f, read); !found {
				t.Fatal("the read was not recorded")
			}

			gone := test.act(m, read)
			access, found := accessOf(t, m.f, read)
			if gone && found {
				t.Errorf("the version was removed and its record of reads was not: %+v", access)
			}
			if !gone && (!found || access.Downloads != 1) {
				t.Errorf("the version was kept and its record of reads was not: %+v, found %v", access, found)
			}
			if other, found := accessOf(t, m.f, elsewhere); !found || other.Downloads != 1 {
				t.Errorf("the record of a version in another bucket: %+v, found %v", other, found)
			}
		})
	}

	t.Run("a bucket that is deleted", func(t *testing.T) {
		t.Parallel()
		m := newMachine(t)
		m.bucket("b", bucket.VersioningEnabled)
		m.bucket("other", bucket.Unversioned)
		read, elsewhere := m.put("b", "k", 1), m.put("other", "k", 1)
		m.reads("node-a", 1, 1, "user/albie", read, elsewhere)
		m.ok(entry{Type: cmdPutSubBucket, PutSubBucket: &subBucketCmd{Bucket: "b", SubBucket: subBucketRecord{Name: "reports"}}})
		m.ok(entry{Type: cmdPutSubBucket, PutSubBucket: &subBucketCmd{Bucket: "other", SubBucket: subBucketRecord{Name: "reports"}}})
		remove(m, read)

		// Before the deletion both buckets have their place in "access" and
		// in "subbuckets" - that of "b" in "access" is empty, its version
		// having gone - and after it only the other has: nothing is left
		// for a bucket of the same name to inherit.
		before := nestedBuckets(t, m.f, accessBucket(), subBucketsBucket())
		if want := []string{"access/b", "access/other", "subbuckets/b", "subbuckets/other"}; !slices.Equal(before, want) {
			t.Fatalf("before the deletion the state has %v, want %v", before, want)
		}
		m.ok(entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "b"}})
		if after, want := nestedBuckets(t, m.f, accessBucket(), subBucketsBucket()), []string{"access/other", "subbuckets/other"}; !slices.Equal(after, want) {
			t.Errorf("after the deletion the state has %v, want %v", after, want)
		}

		got := rawState(t, m.f, accessBucket(), subBucketsBucket())
		for path := range got {
			if strings.HasPrefix(path, "access/b/") || strings.HasPrefix(path, "subbuckets/b/") {
				t.Errorf("the deleted bucket left %s behind", path)
			}
		}
		if len(got) != 4 {
			t.Errorf("after the deletion the state holds %v, want the record and the sub-bucket of the other bucket", got)
		}
	})
}

// TestEntriesOfSchema3ThatAskForWhatCannotBe: an entry that cannot be
// carried out is refused, like any other that asks for what cannot be. It
// never stops the state machine - every replica would stop with it - and it
// leaves nothing behind.
func TestEntriesOfSchema3ThatAskForWhatCannotBe(t *testing.T) {
	t.Parallel()
	tooMany := make([]readItem, ports.MaxReadBatch+1)
	fifty := make([]string, tagging.MaxValues+1)
	for i := range fifty {
		fifty[i] = fmt.Sprint(i)
	}
	sub := func(bucketName string, record subBucketRecord) entry {
		return entry{Type: cmdPutSubBucket, PutSubBucket: &subBucketCmd{Bucket: bucketName, SubBucket: record, Audit: &auditRecord{Actor: "root", Action: "x", Outcome: "succeeded"}}}
	}
	definition := func(record tagDefinitionRecord) entry {
		return entry{Type: cmdPutTagDefinition, PutTagDefinition: &tagDefinitionCmd{Definition: record, Audit: &auditRecord{Actor: "root", Action: "x", Outcome: "succeeded"}}}
	}
	tests := map[string]struct {
		e    entry
		want error
	}{
		"reads of no node":                {entry{Type: cmdAddReads, AddReads: &addReadsCmd{}}, domain.ErrInvalidArgument},
		"reads of a node with a bad name": {entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: "../etc"}}, domain.ErrInvalidArgument},
		"more reads than a batch holds":   {entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: "n1", Items: tooMany}}, domain.ErrInvalidArgument},

		"a sub-bucket of a missing bucket":      {sub("missing", subBucketRecord{Name: "reports"}), domain.ErrBucketNotFound},
		"a sub-bucket without a name":           {sub("b", subBucketRecord{}), domain.ErrInvalidArgument},
		"a sub-bucket with a slash":             {sub("b", subBucketRecord{Name: "a/b"}), domain.ErrInvalidArgument},
		"a sub-bucket named like a path upward": {sub("b", subBucketRecord{Name: ".."}), domain.ErrInvalidArgument},
		"a sub-bucket with a policy too large":  {sub("b", subBucketRecord{Name: "big", Policy: make([]byte, iam.MaxPolicyBytes+1)}), domain.ErrInvalidArgument},
		"deleting a sub-bucket that is not": {
			entry{Type: cmdDeleteSubBucket, DeleteSubBucket: &subBucketRefCmd{Bucket: "b", Name: "never"}}, domain.ErrSubBucketNotFound,
		},
		"deleting a sub-bucket of a missing bucket": {
			entry{Type: cmdDeleteSubBucket, DeleteSubBucket: &subBucketRefCmd{Bucket: "missing", Name: "reports"}}, domain.ErrBucketNotFound,
		},

		"a definition without a key":          {definition(tagDefinitionRecord{}), domain.ErrInvalidArgument},
		"a definition with a key too long":    {definition(tagDefinitionRecord{Key: strings.Repeat("k", tagging.MaxKeyBytes+1)}), domain.ErrInvalidArgument},
		"a definition with too much text":     {definition(tagDefinitionRecord{Key: "k", Description: strings.Repeat("d", tagging.MaxDescriptionBytes+1)}), domain.ErrInvalidArgument},
		"a definition with too many values":   {definition(tagDefinitionRecord{Key: "k", Values: fifty}), domain.ErrInvalidArgument},
		"a definition with a value too long":  {definition(tagDefinitionRecord{Key: "k", Values: []string{strings.Repeat("v", tagging.MaxValueBytes+1)}}), domain.ErrInvalidArgument},
		"a definition with a value twice":     {definition(tagDefinitionRecord{Key: "k", Values: []string{"a", "b", "a"}}), domain.ErrInvalidArgument},
		"deleting a definition that is not":   {entry{Type: cmdDeleteTagDefinition, DeleteTagDefinition: &tagDefinitionRefCmd{Key: "never"}}, domain.ErrEntityNotFound},
		"tag rules larger than a document is": {entry{Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Bucket: "b", Kind: bucket.ConfigTagRules, Document: make([]byte, bucket.MaxConfigBytes+1)}}, domain.ErrInvalidArgument},
		"tag rules of a missing bucket":       {entry{Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Bucket: "missing", Kind: bucket.ConfigTagRules, Document: []byte("{}")}}, domain.ErrBucketNotFound},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMachine(t)
			m.bucket("b", bucket.Unversioned)
			r := m.as(entrySchema3, test.e)
			if !errors.Is(r.err, test.want) || len(r.events) != 0 {
				t.Errorf("= %v with %d events, want %v and none", r.err, len(r.events), test.want)
			}
			// Refused means nothing of it is in the state: no record,
			// and no audit record of a change that did not happen.
			got := rawState(t, m.f, append(schema3Buckets(), auditBucket())...)
			delete(got, "audit")
			for path := range got {
				if strings.Contains(path, "/") {
					t.Errorf("the refused entry left %s in the state", path)
				}
			}
			// And the state machine goes on.
			if next := m.as(entrySchema3, definition(tagDefinitionRecord{Key: "after"})); next.err != nil || len(next.events) != 1 {
				t.Errorf("the entry after it: %v, %d events; want it applied and audited", next.err, len(next.events))
			}
		})
	}

	// Items of a batch that name nothing are not a reason to refuse it.
	m := newMachine(t)
	m.bucket("b", bucket.VersioningEnabled)
	v := m.put("b", "k", 1)
	r := m.ok(entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: "n1", Boot: 1, Seq: 1, Items: []readItem{
		{Bucket: "", Key: "", Version: "", Who: ""},
		{Bucket: "b", Key: "k", Version: "not-a-version", VersionID: v.VersionID, Who: "x", Downloads: 1},
		{Bucket: "b", Key: "k", Version: object.NullVersion, VersionID: v.VersionID, Who: "x", Downloads: 1},
		{Bucket: "b", Key: strings.Repeat("k", 2000), Version: refOf(v), VersionID: v.VersionID, Who: "x", Downloads: 1},
		{Bucket: "b", Key: "k", Version: refOf(v), VersionID: v.VersionID, Who: strings.Repeat("w", 1000), Downloads: 1},
		{Bucket: "b", Key: "k", Version: refOf(v), VersionID: v.VersionID, Who: "x", Downloads: -1, Copies: 2},
		{Bucket: "b", Key: "k", Version: refOf(v), VersionID: v.VersionID, Who: "x", Downloads: math.MaxInt64, Copies: math.MaxInt64},
		{Bucket: "b", Key: "k", Version: refOf(v), VersionID: v.VersionID, Who: "x", Downloads: object.MaxReadCount + 1},
		// The version is named, and not said to be the one that was read.
		{Bucket: "b", Key: "k", Version: refOf(v), Who: "x", Downloads: 1},
		{Bucket: "b", Key: "k", Version: refOf(v), VersionID: "not-an-id", Who: "x", Downloads: 1},
		{Bucket: "b", Key: "k", Version: refOf(v), VersionID: strings.Repeat("0", 2*object.IDSize), Who: "x", Downloads: 1},
		{Bucket: "b", Key: "k", Version: refOf(v), VersionID: strings.Repeat("7", 2*object.IDSize), Who: "x", Downloads: 1},
		{Bucket: "b", Key: "k", Version: refOf(v), VersionID: v.VersionID, Who: "x", Copies: 1},
	}}})
	if access, found := accessOf(t, m.f, v); r.err != nil || !found || access.Copies != 1 || access.Downloads != 0 || len(access.Readers) != 1 {
		t.Errorf("a batch with one item that can be recorded: %v, %+v", r.err, access)
	}
	if got := rawState(t, m.f, accessBucket()); len(got) != 2 {
		t.Errorf("the items that name nothing left records: %v", got)
	}
}

// TestTheRecordOfReadsHasAVersion: the stored record says which format it is
// in. One of a format this binary does not know stops the state machine
// rather than being read as something else.
func TestTheRecordOfReadsHasAVersion(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.bucket("b", bucket.Unversioned)
	v := m.put("b", "k", 1)
	m.reads("node-a", 1, 1, "user/albie", v)

	place, _ := locator("k", object.NullVersion)
	var raw []byte
	if err := m.f.view(func(tx *bbolt.Tx) error {
		raw = bytes.Clone(tx.Bucket(accessBucket()).Bucket([]byte("b")).Get(place))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || string(fields["v"]) != "1" {
		t.Fatalf("the stored record = %s, want a JSON document with \"v\":1", raw)
	}
	for _, field := range []string{"downloads", "copies", "last_at", "readers"} {
		if _, ok := fields[field]; !ok {
			t.Errorf("the stored record has no %q: %s", field, raw)
		}
	}
	if err := m.f.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(accessBucket()).Bucket([]byte("b")).Put(place, bytes.Replace(raw, []byte(`"v":1`), []byte(`"v":2`), 1))
	}); err != nil {
		t.Fatal(err)
	}
	if r := m.raw(entrySchema3, entry{Type: cmdAddReads, AddReads: &addReadsCmd{
		Node: "node-a", Boot: 1, Seq: 2, Items: []readItem{{Bucket: "b", Key: "k", Version: object.NullVersion, VersionID: v.VersionID, Who: "x", Downloads: 1}},
	}}); !r.fatal || !errors.Is(r.err, domain.ErrUnsupportedFormat) {
		t.Errorf("adding to a record of another format = %+v, want the state machine to stop with %v", r.err, domain.ErrUnsupportedFormat)
	}
}

// TestAReadIsOfTheVersionThatWasRead: a node counts for half a minute before
// it says what it counted, and "null" names whatever was put last. A read is
// recorded for the version that was read - the one with the ID the item
// carries - or for none: never for the version that took its name meanwhile.
func TestAReadIsOfTheVersionThatWasRead(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.bucket("b", bucket.Unversioned)
	first := m.put("b", "k", 1)
	second := m.put("b", "k", 2)
	if refOf(first) != refOf(second) || first.VersionID == second.VersionID {
		t.Fatalf("two puts of one key where no versions are kept: named %s and %s, IDs %s and %s; want one name and two IDs",
			refOf(first), refOf(second), first.VersionID, second.VersionID)
	}

	// The batch names the first object, which was read and is gone.
	if r := m.reads("node-a", 1, 1, "user/reader-of-the-first", first); r.err != nil {
		t.Fatal(r.err)
	}
	if access, found := accessOf(t, m.f, second); found || !access.Empty() {
		t.Fatalf("the record of the second object shows a reader who fetched the first: %+v", access)
	}
	if got := rawState(t, m.f, accessBucket()); len(got) != 0 {
		t.Errorf("a read of a version that is gone left %v", got)
	}
	// The batch was a batch all the same: it is remembered, and not
	// applied again.
	if got := rawState(t, m.f, accessFlushBucket())["accessflush/"+fmt.Sprintf("%x", "node-a")]; got != `{"boot":1,"seq":1}` {
		t.Errorf("the batch is remembered as %q", got)
	}
	// Late reads of the first and reads of the second, in one batch.
	if r := m.reads("node-a", 1, 2, "user/reader", first, second, first); r.err != nil {
		t.Fatal(r.err)
	}
	access, found := accessOf(t, m.f, second)
	if !found || access.Downloads != 1 || len(access.Readers) != 1 || access.Readers[0].Who != "user/reader" {
		t.Errorf("the record of the second object = %+v, want its own one download", access)
	}

	// Where versions are kept the name is the ID: an item whose two say
	// different things is of no version.
	m.bucket("ver", bucket.VersioningEnabled)
	// IDs with letters in them, so that their case can be got wrong.
	older, current := m.put("ver", "k", 0xab), m.put("ver", "k", 0xcd)
	m.ok(entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: "node-a", Boot: 1, Seq: 3, Items: []readItem{
		{Bucket: "ver", Key: "k", Version: refOf(current), VersionID: older.VersionID, Who: "x", Downloads: 1},
		{Bucket: "ver", Key: "k", Version: refOf(older), VersionID: current.VersionID, Who: "x", Downloads: 1},
		{Bucket: "ver", Key: "k", Version: refOf(older), VersionID: strings.ToUpper(older.VersionID), Who: "x", Downloads: 7},
		{Bucket: "ver", Key: "k", Version: refOf(older), VersionID: older.VersionID, Who: "x", Copies: 1},
	}}})
	if access, found := accessOf(t, m.f, current); found {
		t.Errorf("the current version has a record: %+v", access)
	}
	// An ID is written in lower case: in capitals it is no ID, and names
	// no version.
	if strings.ToUpper(older.VersionID) == older.VersionID {
		t.Fatal("the ID of the test has no letter to get wrong")
	}
	if access, _ := accessOf(t, m.f, older); access.Downloads != 0 || access.Copies != 1 {
		t.Errorf("the older version = %+v, want the one copy that named it by its own ID", access)
	}
}

// TestABatchOfReadsIsItsNodes: a cluster that knows its nodes takes what a
// node counted from a node it knows. A batch under the name of any other is
// applied as nothing - it is not refused, which would be an error to a node
// that is merely not registered yet - and is not remembered: when the node is
// known, its numbers are unused.
func TestABatchOfReadsIsItsNodes(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.bucket("b", bucket.Unversioned)
	v := m.put("b", "k", 1)
	m.ok(entry{Type: cmdSetNode, SetNode: &nodeRecord{ID: "n1", Roles: []string{"gateway"}, MetadataVersion: 3}})

	if r := m.reads("n2", 1, 1, "user/forged", v); r.err != nil || r.fatal || len(r.events) != 0 {
		t.Fatalf("a batch of a node that is not registered = %+v; want it applied as nothing", r)
	}
	if got := rawState(t, m.f, accessBucket(), accessFlushBucket()); len(got) != 0 {
		t.Fatalf("a batch of a node that is not registered left %v", got)
	}
	if r := m.reads("n1", 1, 1, "user/albie", v); r.err != nil {
		t.Fatal(r.err)
	}
	if access, _ := accessOf(t, m.f, v); access.Downloads != 1 || len(access.Readers) != 1 || access.Readers[0].Who != "user/albie" {
		t.Errorf("after the batch of a registered node: %+v", access)
	}
	// The node registers; the numbers it used in vain are still its own.
	m.ok(entry{Type: cmdSetNode, SetNode: &nodeRecord{ID: "n2", Roles: []string{"gateway"}, MetadataVersion: 3}})
	m.reads("n2", 1, 1, "user/bob", v)
	if access, _ := accessOf(t, m.f, v); access.Downloads != 2 || len(access.Readers) != 2 {
		t.Errorf("after the node registered and sent its batch: %+v", access)
	}
	// And a node that was removed is no node of the cluster.
	m.ok(entry{Type: cmdRemoveNode, RemoveNode: &removeNodeCmd{ID: "n2"}})
	m.reads("n2", 1, 2, "user/bob", v)
	if access, _ := accessOf(t, m.f, v); access.Downloads != 2 {
		t.Errorf("a batch of a node that was removed was counted: %+v", access)
	}
	if got := rawState(t, m.f, accessFlushBucket()); len(got) != 2 {
		t.Errorf("the state remembers batches as %v, want that of n1 alone", got)
	}
	// A node on its own registers nobody, and takes its own batches: the
	// other tests of this file do that.
}

// TestTheTagsOfAnUploadAreBoundedInBytes: whether an entry is refused must
// not depend on the build that applies it. What a tag may consist of is a
// matter of Unicode tables, which differ between releases of Go; so the state
// machine counts tags and bytes and nothing else, and an upload whose tags
// only the proposer's check would refuse is applied - identically - while
// the store refuses it before it is proposed.
func TestTheTagsOfAnUploadAreBoundedInBytes(t *testing.T) {
	t.Parallel()
	upload := func(fill byte, tags map[string]string) entry {
		u := porttest.SampleUpload("b", fmt.Sprintf("k%d", fill), fill)
		u.Tags = tags
		return entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)}
	}
	// Within the bounds, and not tags: each is refused by ValidateTags.
	applied := []map[string]string{
		{"sta!ge": "draft"},
		{"stage": "dra#ft"},
		{"control\x01": "v"},
		{"not\xffutf8": "\xfe"},
		{strings.Repeat("k", maxUploadTagKeyBytes): strings.Repeat("v", maxUploadTagValueBytes)},
		// A letter of one release of Unicode, not yet one of another.
		{"\U0001E030": "\U00031350"},
	}
	a, b := newMachine(t), newMachine(t)
	for _, m := range []*machine{a, b} {
		m.bucket("b", bucket.Unversioned)
		for i, tags := range applied {
			if i < 4 && object.ValidateTags(tags, object.MaxObjectTags) == nil {
				t.Fatalf("%q are tags the proposer accepts: the case shows nothing", tags)
			}
			if r := m.as(entrySchema3, upload(byte(i+1), tags)); r.err != nil {
				t.Fatalf("an upload with the tags %q, within the bounds = %v, want it applied", tags, r.err)
			}
		}
	}
	want, got := rawState(t, a.f, uploadsBucket()), rawState(t, b.f, uploadsBucket())
	if len(want) != len(applied)+1 || !reflect.DeepEqual(want, got) {
		t.Fatalf("two replicas hold different uploads, or not all of them:\n%v\n%v", want, got)
	}
	stored := 0
	for path, raw := range want {
		if !strings.Contains(path, "/b/") {
			continue
		}
		var record uploadRecord
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			t.Fatal(err)
		}
		for _, tags := range applied {
			// As an entry carries them: JSON has no word for bytes that
			// are not UTF-8, and writes U+FFFD in their place - before
			// the entry is logged, so alike for every member.
			var carried map[string]string
			if encoded, err := json.Marshal(tags); err != nil || json.Unmarshal(encoded, &carried) != nil {
				t.Fatal(err)
			}
			if reflect.DeepEqual(record.Tags, carried) {
				stored++
			}
		}
	}
	if stored != len(applied) {
		t.Errorf("%d of %d uploads were stored with the tags of their entry", stored, len(applied))
	}

	// Beyond the bounds an entry is refused, by every build alike.
	eleven := map[string]string{}
	for i := range maxUploadTags + 1 {
		eleven[fmt.Sprint(i)] = "v"
	}
	for name, tags := range map[string]map[string]string{
		"more tags than a version has": eleven,
		"a key of no bytes":            {"": "v"},
		"a key too long":               {strings.Repeat("k", maxUploadTagKeyBytes+1): "v"},
		"a value too long":             {"k": strings.Repeat("v", maxUploadTagValueBytes+1)},
	} {
		before := rawState(t, a.f, uploadsBucket(), uploadKeysBucket(), dataBucket())
		if r := a.as(entrySchema3, upload(100, tags)); !errors.Is(r.err, domain.ErrInvalidTag) {
			t.Errorf("%s = %v, want %v", name, r.err, domain.ErrInvalidTag)
		}
		if after := rawState(t, a.f, uploadsBucket(), uploadKeysBucket(), dataBucket()); !reflect.DeepEqual(before, after) {
			t.Errorf("%s: the refused upload changed the state", name)
		}
	}
	// The bounds admit whatever the proposer's check admits.
	if maxUploadTags < object.MaxObjectTags {
		t.Errorf("the state machine keeps %d tags of an upload, a version has %d", maxUploadTags, object.MaxObjectTags)
	}
	widest := map[string]string{strings.Repeat("\U00010348", 128): strings.Repeat("\U00010348", 256)}
	if err := object.ValidateTags(widest, object.MaxObjectTags); err != nil || !uploadTagsFit(widest) {
		t.Errorf("the longest tag of the widest letters: the proposer says %v, the state machine keeps it: %v", err, uploadTagsFit(widest))
	}

	// The store refuses what is no tag before it proposes anything: the
	// state is not touched, and stays one the release before opens.
	store := openStore(t, t.TempDir())
	ctx := t.Context()
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	index, err := store.AppliedIndex()
	if err != nil {
		t.Fatal(err)
	}
	for i, tags := range applied[:4] {
		u := porttest.SampleUpload("b", "k", byte(i+1))
		u.Tags = tags
		if err := store.CreateUpload(ctx, u); !errors.Is(err, domain.ErrInvalidTag) {
			t.Errorf("CreateUpload with the tags %q = %v, want %v", tags, err, domain.ErrInvalidTag)
		}
		if _, err := store.GetUpload(ctx, "b", u.Key, u.ID); !errors.Is(err, domain.ErrUploadNotFound) {
			t.Errorf("the upload with the tags %q: %v, want %v", tags, err, domain.ErrUploadNotFound)
		}
	}
	if after, err := store.AppliedIndex(); err != nil || after != index {
		t.Errorf("refused uploads were proposed: the state is at entry %d (%v), was at %d", after, err, index)
	}
	if got := stateSchema(t, store.fsm); got != stateSchemaTimed {
		t.Errorf("refused uploads left the state at schema %d, want %d", got, stateSchemaTimed)
	}
}

// TestWhatCannotBeIsRefusedBeforeItIsProposed: the first entry of schema 3
// a state applies ends the way back to the release before, whether the entry
// took effect or not. So what the state machine would refuse without looking
// at the state - a name, a size, a number - the store refuses by itself, and
// proposes nothing: the state stays at schema 2.
func TestWhatCannotBeIsRefusedBeforeItIsProposed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openStore(t, t.TempDir())
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	v := porttest.SampleVersion("b", "k", 1)
	if _, err := store.PutVersion(ctx, v); err != nil {
		t.Fatal(err)
	}
	tagged := func(change func(u *object.Upload)) object.Upload {
		u := porttest.SampleUpload("b", "upload", 2)
		u.Tags = map[string]string{"stage": "draft"}
		change(&u)
		return u
	}
	tooMany := make([]ports.ObjectRead, ports.MaxReadBatch+1)
	noAudit := audit.Record{}
	tests := map[string]func() error{
		"a sub-bucket with a policy too large": func() error {
			return store.PutSubBucket(ctx, "b", bucket.SubBucket{Name: "big", Policy: make([]byte, iam.MaxPolicyBytes+1)}, noAudit)
		},
		"a sub-bucket with a name that is none": func() error {
			return store.PutSubBucket(ctx, "b", bucket.SubBucket{Name: "Not/A/Name"}, noAudit)
		},
		"a sub-bucket with an audit record that cannot be kept": func() error {
			return store.PutSubBucket(ctx, "b", bucket.SubBucket{Name: "reports"}, audit.Record{Actor: "root"})
		},
		"a tag definition beyond its bounds": func() error {
			return store.PutTagDefinition(ctx, tagging.Definition{Key: "k", Description: strings.Repeat("d", tagging.MaxDescriptionBytes+1)}, noAudit)
		},
		"tag rules larger than a configuration is": func() error {
			return store.SetBucketConfig(ctx, "b", bucket.ConfigTagRules, make([]byte, bucket.MaxConfigBytes+1))
		},
		"a batch of reads with too many items": func() error {
			return store.AddObjectReads(ctx, ports.ReadBatch{Node: "n1", Boot: 1, Seq: 1, Items: tooMany})
		},
		"a batch of reads of no node": func() error {
			return store.AddObjectReads(ctx, ports.ReadBatch{Boot: 1, Seq: 1})
		},
		"an upload with more tags than a version has": func() error {
			return store.CreateUpload(ctx, tagged(func(u *object.Upload) {
				for i := range object.MaxObjectTags + 1 {
					u.Tags[fmt.Sprint(i)] = "v"
				}
			}))
		},
		"an upload with tags and a key that is none": func() error {
			return store.CreateUpload(ctx, tagged(func(u *object.Upload) { u.Key = "" }))
		},
		"an upload with tags and no ID": func() error {
			return store.CreateUpload(ctx, tagged(func(u *object.Upload) { u.ID = object.ID{} }))
		},
		"an upload with tags and the key mode of a put": func() error {
			return store.CreateUpload(ctx, tagged(func(u *object.Upload) { u.Encryption.KeyMode = "" }))
		},
	}
	index, err := store.AppliedIndex()
	if err != nil {
		t.Fatal(err)
	}
	for name, attempt := range tests {
		// (A key that is none has an error of its own.)
		if err := attempt(); (!errors.Is(err, domain.ErrInvalidArgument) && !errors.Is(err, domain.ErrInvalidObjectKey)) || errors.Is(err, domain.ErrOutcomeUnknown) {
			t.Errorf("%s = %v, want a definite %v", name, err, domain.ErrInvalidArgument)
		}
		if after, err := store.AppliedIndex(); err != nil || after != index {
			t.Fatalf("%s was proposed: the state is at entry %d (%v), was at %d", name, after, err, index)
		}
		if got := stateSchema(t, store.fsm); got != stateSchemaTimed {
			t.Fatalf("%s: the refused call left the state at schema %d, want %d", name, got, stateSchemaTimed)
		}
	}
	if got := rawState(t, store.fsm, schema3Buckets()...); len(got) != 0 {
		t.Errorf("refused calls left %v", got)
	}
	// What can be, at the bounds, is proposed and kept - and from then on
	// the state is of schema 3.
	if err := store.PutSubBucket(ctx, "b", bucket.SubBucket{Name: "big", Policy: bytes.Repeat([]byte{'x'}, iam.MaxPolicyBytes)}, noAudit); err != nil {
		t.Fatalf("a sub-bucket with the largest policy there can be: %v", err)
	}
	if got := stateSchema(t, store.fsm); got != stateSchema3 {
		t.Errorf("after the first sub-bucket the state is at schema %d, want %d", got, stateSchema3)
	}
}

// TestTheStoredRecordOfReadsIsPinned: the record of who read a version is
// computed by every member from the entries, so the rule it is computed by
// is part of the replicated format - which readers are kept by name, which is
// folded into "others" when several read at the same time, in which order
// they are stored, how a presigned read is told from the signer's own - and
// so are the bytes it is stored in. This is the record after a fixed sequence
// of batches, byte for byte. A change that makes this test fail makes two
// releases disagree: it needs a rise of the entry schema, not a new file.
func TestTheStoredRecordOfReadsIsPinned(t *testing.T) {
	t.Parallel()
	m := newMachine(t)
	m.bucket("b", bucket.Unversioned)
	v := m.put("b", "k", 1)
	item := func(who string, presigned bool, downloads, copies int64) readItem {
		return readItem{Bucket: "b", Key: "k", Version: refOf(v), VersionID: v.VersionID, Who: who, Presigned: presigned, Downloads: downloads, Copies: copies}
	}
	batch := func(node string, seq uint64, items ...readItem) {
		t.Helper()
		m.ok(entry{Type: cmdAddReads, AddReads: &addReadsCmd{Node: node, Boot: 1, Seq: seq, Items: items}})
	}
	// One batch, one time: more readers than a record names, so that the
	// fold decides between readers who all read at the same moment. They
	// come in an order that is not the order they are stored in.
	var crowd []readItem
	for i := range object.MaxReaders + 3 {
		crowd = append(crowd, item(fmt.Sprintf("user/%02d", (i*11)%(object.MaxReaders+3)), false, int64(i+1), 0))
	}
	batch("node-a", 1, crowd...)
	// A signer's own reads and those made with URLs it signed; a reader
	// without credentials; a copy.
	batch("node-a", 2, item("key/AKID", true, 2, 0), item("key/AKID", false, 0, 1), item("anonymous", false, 1, 0), item("key/AKID", true, 1, 1))
	// Readers who return are no longer the oldest; one who was folded
	// returns and is counted again.
	batch("node-b", 1, item("user/03", false, 1, 0), item("user/34", false, 5, 0), item("root", false, 0, 2))
	// A leader with a slow clock: the time of a read does not go back.
	m.at = m.at.Add(-time.Hour)
	batch("node-a", 3, item("user/zz", true, 1, 0), item("user/10", false, 0, 4))
	m.at = m.at.Add(2 * time.Hour)
	// The same batch again, which is nothing, and a last one.
	batch("node-a", 3, item("user/zz", true, 100, 100))
	batch("node-b", 2, item("anonymous", false, 3, 0), item("user/new", false, object.MaxReadCount, object.MaxReadCount))

	place, _ := locator("k", refOf(v))
	var got []byte
	if err := m.f.view(func(tx *bbolt.Tx) error {
		got = bytes.Clone(tx.Bucket(accessBucket()).Bucket([]byte("b")).Get(place))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/access-record.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(got, '\n'), want) {
		t.Errorf("the stored record of reads is not the one this release stores:\n got %s\nwant %s", got, want)
	}
}
