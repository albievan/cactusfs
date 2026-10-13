package raftmeta

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// Entry schema 5 in the codec (ADR-0023, "Entry schema 5"; PLAN 8.0.6): the
// seven commands, what six old commands can carry, which schema each entry
// needs and is written in, and what a member below level 5 makes of it. The
// state machine of schema 5 is tested in bucketusage_test.go.

// schema5EntriesFile pins the bytes of every entry of schema 5.
const schema5EntriesFile = "schema5-entries.golden.txt"

// newMachine5 is the state machine of a binary whose newest entry schema is
// the fifth: what this one becomes when it announces schema 5 (PLAN 8.0.10).
func newMachine5(t *testing.T) *machine {
	t.Helper()
	m := newMachine(t)
	m.f.supported = entrySchema5
	return m
}

// written returns the schema a leader writes an entry in while the cluster
// is at level: what writtenAt gives what heldTo left of it, and never below
// the schema that carries the leader's time.
func written(e entry, level byte) (entry, byte) {
	held := heldTo(e, level)
	return held, max(writtenAt(held, versionOf(held), level), entrySchemaTimed)
}

// at5 applies an entry as the leader of a cluster at level 5 writes it, and
// returns whatever came of it but a stop.
func (m *machine) at5(e entry) applyResult {
	m.t.Helper()
	held, schema := written(e, entrySchema5)
	return m.as(schema, held)
}

// ok5 applies an entry as a cluster at level 5 writes it, which must take
// effect.
func (m *machine) ok5(e entry) applyResult {
	m.t.Helper()
	r := m.at5(e)
	if r.err != nil {
		m.t.Fatalf("%s: %v", e.Type, r.err)
	}
	return r
}

func domainGolden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := fs.ReadFile(os.DirFS("../../domain/bucket/testdata"), name)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(raw, []byte("\n"))
}

// schema5Commands is one entry of every command schema 5 brought, in each
// of its forms, and of every old command that carries something of schema 5,
// by name. Each needs schema 5 and is written in it.
func schema5Commands(t *testing.T) map[string]entry {
	t.Helper()
	at := time.Date(2026, 10, 12, 9, 0, 0, 123456789, time.UTC)
	record := auditToRecord(audit.Record{
		Time: at, Actor: "user/albie", Action: "admin:PutBucketQuota", Resource: "arn:aws:s3:::photos", Outcome: audit.Succeeded,
		RequestID: "req-1",
	})
	plain := porttest.SampleVersion("photos", "a/b.jpg", 1)
	replica, null := plain, plain
	replica.Replica = &object.Replica{From: "5d3c1f0a-site-a", Seq: 41}
	null.Replica = &object.Replica{From: "5d3c1f0a-site-a", Seq: object.MaxReplicaSeq, Null: true}
	upload := porttest.SampleUpload("photos", "a/b.jpg", 1)
	upload.ReplicaFrom = "5d3c1f0a-site-a"
	target := func(kind delivery.Kind) targetRecord {
		target := porttest.SampleTarget("site-b", kind)
		target.CreatedAt, target.UpdatedAt = at, at.Add(time.Hour)
		if target.Auth.Mode == "" {
			target.Auth.Mode = delivery.AuthNone
		}
		return targetToRecord(target)
	}
	audited := target(delivery.KindWebhook)
	audited.Audit, audited.CAPEM, audited.Buckets = true, []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), []string{"photos", "logs-*"}
	id := object.ID(bytes.Repeat([]byte{7}, object.IDSize))
	return map[string]entry{
		"bucket.quota.put":                {Type: cmdSetBucketQuota, SetBucketQuota: &bucketQuotaCmd{Bucket: "photos", Bytes: bucket.MaxQuotaBytes, Audit: record}},
		"bucket.quota.put that removes":   {Type: cmdSetBucketQuota, SetBucketQuota: &bucketQuotaCmd{Bucket: "photos"}},
		"bucket.usage.walk":               {Type: cmdWalkBucketUsage, WalkBucketUsage: &usageWalkCmd{Bucket: "photos", Limit: 10000}},
		"bucket.usage.walk that restarts": {Type: cmdWalkBucketUsage, WalkBucketUsage: &usageWalkCmd{Bucket: "photos", Limit: 1, Restart: true}},
		"target.put of a webhook":         {Type: cmdPutTarget, PutTarget: &targetCmd{Target: audited, Audit: record}},
		"target.put of a NATS server":     {Type: cmdPutTarget, PutTarget: &targetCmd{Target: target(delivery.KindNATS)}},
		"target.put of a deployment":      {Type: cmdPutTarget, PutTarget: &targetCmd{Target: target(delivery.KindS3)}},
		"target.delete":                   {Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: "site-b", Audit: record}},
		"outbox.ack with nothing to say":  {Type: cmdAckOutbox, AckOutbox: &ackOutboxCmd{Target: "hook", Upto: delivery.Seq{}.String()}},
		"outbox.ack of rows": {Type: cmdAckOutbox, AckOutbox: &ackOutboxCmd{
			Target: "site-b", Upto: delivery.Seq{Index: 1 << 40, Row: 65535}.String(), AuditUpto: 77,
			Results: []ackResult{
				resultToRecord(delivery.Result{Seq: delivery.Seq{Index: 12, Row: 3}, Status: object.ReplicationCompleted}),
				resultToRecord(delivery.Result{Seq: delivery.Seq{Index: 13}, Status: object.ReplicationFailed}),
			},
		}},
		"outbox.ack of versions": {Type: cmdAckOutbox, AckOutbox: &ackOutboxCmd{
			Target: "site-b", Upto: delivery.Seq{Index: 14}.String(),
			Results: []ackResult{
				resultToRecord(delivery.Result{Bucket: "photos", Key: "a/b.jpg", Version: object.VersionRef{ID: id}, Status: object.ReplicationCompleted}),
				resultToRecord(delivery.Result{Bucket: "photos", Key: "zoë \"π\"", Version: object.VersionRef{Null: true}, Status: object.ReplicationFailed}),
			},
		}},
		"bucket.replica.put":           {Type: cmdSetBucketReplica, SetBucketReplica: &bucketReplicaCmd{Bucket: "photos", Source: "5d3c1f0a-site-a", Audit: record}},
		"bucket.replica.put that ends": {Type: cmdSetBucketReplica, SetBucketReplica: &bucketReplicaCmd{Bucket: "photos"}},
		"replication.resync.put": {Type: cmdPutResync, PutResync: resyncToRecord(bucket.Resync{
			Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning, Key: "a/b.jpg", Version: id.String(), Scanned: 1200, Sent: 7,
		})},
		"replication.resync.put before the first version": {Type: cmdPutResync, PutResync: resyncToRecord(bucket.Resync{
			Bucket: "photos", Target: "site-b", State: bucket.ResyncDone,
		})},
		"object.put of a replica":                   {Type: cmdPutObject, PutObject: versionToRecord(replica)},
		"object.put of a replica of a null version": {Type: cmdPutObject, PutObject: versionToRecord(null)},
		"upload.create of a replica":                {Type: cmdCreateUpload, CreateUpload: uploadToRecord(upload)},
		"upload.complete of a replica": {Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{
			Bucket: "photos", Key: "a/b.jpg", UploadID: upload.ID.String(), VersionID: id.String(), ETag: "etag-1", ModifiedAt: at,
			Parts:   []listedPart{{Number: 1, ETag: "etag", DataID: id.String(), Seal: bytes.Repeat([]byte{1}, 32)}},
			Replica: &replicaRecord{From: "5d3c1f0a-site-a", Seq: 1},
		}},
		"object.version.delete of a replica": {Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{
			Bucket: "photos", Key: "a/b.jpg", MarkerID: id.String(), At: at, Replica: &replicaRecord{From: "5d3c1f0a-site-a", Seq: 42, Null: true},
		}},
		"object.version.delete that says who": {Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{
			Bucket: "photos", Key: "a/b.jpg", VersionRef: object.NullVersion, MarkerID: id.String(), At: at, By: "service/lifecycle",
		}},
		"bucket.config.put of notification rules": {Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{
			Bucket: "photos", Kind: bucket.ConfigNotification, Document: domainGolden(t, "notification.golden.json"),
		}},
		"bucket.config.put of a replication configuration": {Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{
			Bucket: "photos", Kind: bucket.ConfigReplication, Document: domainGolden(t, "replication.golden.json"),
		}},
		"bucket.config.put that removes notification rules": {Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Bucket: "photos", Kind: bucket.ConfigNotification}},
	}
}

// schema5EntryBytes writes every entry of schema5Commands as it is logged,
// a line each, in the order of the names: the name, the schema byte, and the
// document.
func schema5EntryBytes(t *testing.T) []byte {
	t.Helper()
	commands := schema5Commands(t)
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	slices.Sort(names)
	var out []byte
	for _, name := range names {
		e := commands[name]
		data, err := encodeEntry(e, versionOf(e))
		if err != nil {
			t.Fatal(err)
		}
		out = fmt.Appendf(out, "%s\tschema %d\t%s\n", name, data[0], data[1:])
	}
	return out
}

// TestTheEntriesOfSchema5ArePinned: every command of schema 5, and every
// old command with what schema 5 lets it carry, is the bytes of the golden
// file - both forms of a result of outbox.ack, a replica with "null" and
// without it. A place in the outbox is 20 hexadecimal characters, a version
// "null" or 32 of them, and what is optional is left out. A change that
// makes this test fail makes two releases write other entries: it needs a
// rise of the entry schema once schema 5 is announced, not a new file.
func TestTheEntriesOfSchema5ArePinned(t *testing.T) {
	t.Parallel()
	want, err := fs.ReadFile(os.DirFS("testdata"), schema5EntriesFile)
	if err != nil {
		t.Fatal(err)
	}
	got := schema5EntryBytes(t)
	gotLines, wantLines := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := range min(len(gotLines), len(wantLines)) {
		if gotLines[i] != wantLines[i] {
			t.Errorf("an entry of schema 5 is not written as it is pinned:\n got %s\nwant %s", gotLines[i], wantLines[i])
		}
	}
	if len(gotLines) != len(wantLines) {
		t.Errorf("%d entries, %d are pinned", len(gotLines)-1, len(wantLines)-1)
	}
	// What is pinned is read as the entry it was written from, and written
	// again as the same bytes.
	commands := schema5Commands(t)
	for _, line := range wantLines {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 || fields[1] != "schema 5" {
			t.Fatalf("a line of the golden file: %q", line)
		}
		data := append([]byte{entrySchema5}, fields[2]...)
		e, err := decodeEntry(data, entrySchema5)
		if err != nil {
			t.Errorf("%s does not decode: %v", fields[0], err)
			continue
		}
		if want := commands[fields[0]]; !reflect.DeepEqual(e, want) {
			t.Errorf("%s decodes as\n%+v\nwant\n%+v", fields[0], e, want)
		}
		if again, err := encodeEntry(e, entrySchema5); err != nil || !bytes.Equal(again, data) {
			t.Errorf("%s is written again as %s, %v", fields[0], again, err)
		}
	}
	for _, text := range []string{
		`"seq":"000000000000000c0003"`, `"upto":"0000010000000000ffff"`, `"vid":"null"`, `"vid":"07070707070707070707070707070707"`,
		`"replica":{"from":"5d3c1f0a-site-a","seq":41}`, `"replica":{"from":"5d3c1f0a-site-a","seq":4611686018427387904,"null":true}`,
		`"replica_from":"5d3c1f0a-site-a"`, `"by":"service/lifecycle"`, `"walk_bucket_usage":{"bucket":"photos","limit":10000}`,
		`"set_bucket_quota":{"bucket":"photos","bytes":0}`,
	} {
		if !bytes.Contains(want, []byte(text)) {
			t.Errorf("no pinned entry says %s", text)
		}
	}
}

// TestSchema5OfEveryCommand: for every command there is and every shape of
// it - which schema it needs (versionOf), what of it is written at level 4
// and at level 5 (heldTo) and in which schema (writtenAt). Schema 5 is four
// things and no more: its seven commands need it; an old command that says
// a version is a replica, an upload that says where it comes from, a delete
// that says who, and the two new kinds of bucket configuration need it; a
// bucket.create is written in it once the level is 5, and below as it
// always was; and everything else is written exactly as before, at either
// level. Below level 5 only who deleted is left out, and nothing else.
func TestSchema5OfEveryCommand(t *testing.T) {
	t.Parallel()
	type expectation struct {
		// needs is what versionOf gives the entry; at4 and at5 the schema it
		// is written in at the two levels. unavailable says that it cannot
		// be written at level 4 at all.
		needs, at4, at5 byte
		unavailable     bool
	}
	want := map[string]expectation{}
	// What was there before: as TestSchema4OfEveryCommand has it at level
	// 4, and the same at level 5 - but for a bucket.create.
	for name, e := range everyCommand() {
		at4 := writtenAt(e, versionOf(e), entrySchema4)
		expect := expectation{needs: versionOf(e), at4: at4, at5: at4}
		if e.CreateBucket != nil {
			expect.at5 = entrySchema5
		}
		if expect.needs > entrySchema4 || expect.at4 > entrySchema4 {
			t.Fatalf("%s needs schema %d and is written as %d at level 4: nothing that was there before does", name, expect.needs, expect.at4)
		}
		want[name] = expect
	}
	commands := everyCommand()
	for name, e := range schema5Commands(t) {
		commands[name] = e
		want[name] = expectation{needs: entrySchema5, at4: entrySchema5, at5: entrySchema5, unavailable: true}
	}
	// Who deleted is the one thing of schema 5 a write goes without.
	want["object.version.delete that says who"] = expectation{needs: entrySchema5, at4: entrySchemaTimed, at5: entrySchema5}
	// And each shape by itself, so that none of them hides behind another.
	replica := &replicaRecord{From: "site-a", Seq: 1}
	for name, e := range map[string]entry{
		"a put with a replica and who":        {Type: cmdPutObject, PutObject: &versionRecord{Bucket: "b", Key: "k", CreatedBy: "root", Replica: replica}},
		"a completion with a replica":         {Type: cmdCompleteUpload, CompleteUpload: &completeUploadCmd{Bucket: "b", Key: "k", Replica: replica}},
		"an upload of a source with tags":     {Type: cmdCreateUpload, CreateUpload: &uploadRecord{Bucket: "b", Key: "k", ReplicaFrom: "site-a", Tags: map[string]string{"a": "b"}}},
		"a delete marker that is a replica":   {Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "b", Key: "k", Replica: replica}},
		"a replica delete that also says who": {Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "b", Key: "k", Replica: replica, By: "root"}},
	} {
		commands[name] = e
		want[name] = expectation{needs: entrySchema5, at4: entrySchema5, at5: entrySchema5, unavailable: true}
	}
	if len(commands) != len(want) {
		t.Fatalf("%d commands and %d expectations", len(commands), len(want))
	}
	for name, e := range commands {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			expect := want[name]
			if got := versionOf(e); got != expect.needs {
				t.Errorf("versionOf = %d, want %d", got, expect.needs)
			}
			before, _ := json.Marshal(e)
			for level, schema := range map[byte]byte{entrySchema4: expect.at4, entrySchema5: expect.at5} {
				held := heldTo(e, level)
				got := writtenAt(held, versionOf(held), level)
				if got != schema {
					t.Errorf("at level %d written as schema %d, want %d", level, got, schema)
				}
				if unavailable := got > level; unavailable != (expect.unavailable && level < entrySchema5) {
					t.Errorf("at level %d written as schema %d: unavailable = %t", level, got, unavailable)
				}
				// Who deleted is the one thing an entry loses below level
				// 5, and it loses nothing at level 5.
				after, _ := json.Marshal(held)
				lost := !bytes.Equal(before, after)
				if wantLost := level < entrySchema5 && e.DeleteVersion != nil && e.DeleteVersion.By != ""; lost != wantLost {
					t.Errorf("at level %d the entry is written as %s, was %s", level, after, before)
				}
				if held.DeleteVersion != nil && level < entrySchema5 && held.DeleteVersion.By != "" {
					t.Errorf("at level %d a delete still says who: %q", level, held.DeleteVersion.By)
				}
				if got > level {
					continue
				}
				// It is written and read there by a binary of schema 5,
				// and never read by the release before when it is of
				// schema 5: that one stops at it.
				data, err := encodeEntry(held, got)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := decodeEntry(data, entrySchema5); err != nil {
					t.Errorf("at level %d the entry does not decode in its own schema: %v", level, err)
				}
				if _, err := decodeEntry(data, entrySchema4); (err == nil) != (got < entrySchema5) {
					t.Errorf("at level %d, written as schema %d, read by a binary of schema 4 = %v", level, got, err)
				} else if err != nil && !errors.Is(err, domain.ErrUnsupportedFormat) {
					t.Errorf("a binary of schema 4 refuses it with %v", err)
				}
			}
			if after, _ := json.Marshal(e); !bytes.Equal(before, after) {
				t.Errorf("heldTo changed the caller's entry:\n%s\n%s", before, after)
			}
			// Below level 4 nothing changed: what is written is what was
			// written before there was a schema 5.
			for _, level := range []byte{entrySchemaVersion, entrySchemaTimed, entrySchema3} {
				held := heldTo(e, level)
				if got := writtenAt(held, versionOf(held), level); got != versionOf(held) {
					t.Errorf("at level %d written as schema %d, the entry needs %d", level, got, versionOf(held))
				}
			}
		})
	}
}

// TestTheCommandsOfSchema5AreRefusedBelowIt: each of the seven commands in
// an entry that claims an older schema is no entry at all - the binary of
// that schema does not know the command, and stops; this one must stop
// alike. And their payloads, beside the payload of an older command in an
// entry of an older schema, say nothing: the binary of that schema does not
// see them, and applies the command it knows.
func TestTheCommandsOfSchema5AreRefusedBelowIt(t *testing.T) {
	t.Parallel()
	seven := map[string]bool{}
	for name, e := range schema5Commands(t) {
		switch e.Type {
		case cmdSetBucketQuota, cmdWalkBucketUsage, cmdPutTarget, cmdDeleteTarget, cmdAckOutbox, cmdSetBucketReplica, cmdPutResync:
		default:
			continue
		}
		seven[e.Type] = true
		for schema := byte(entrySchemaVersion); schema <= entrySchema5; schema++ {
			data, err := encodeEntry(e, schema)
			if err != nil {
				t.Fatal(err)
			}
			_, err = decodeEntry(data, entrySchema5)
			if (err == nil) != (schema == entrySchema5) {
				t.Errorf("%s in an entry of schema %d = %v", name, schema, err)
			}
			if err != nil && !errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Errorf("%s in an entry of schema %d is refused with %v", name, schema, err)
			}
			// A state machine that applies schema 5 stops at it, as the
			// release of that schema does.
			if schema < entrySchema5 {
				m := newMachine5(t)
				if r := m.raw(schema, e); !r.fatal || !errors.Is(r.err, domain.ErrUnsupportedFormat) {
					t.Errorf("%s in an entry of schema %d = %+v, want the state machine to stop", name, schema, r.err)
				}
			}
		}
		// Beside a command of schema 4, in an entry of schema 4.
		body, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		payload := body[bytes.IndexByte(body, ',')+1 : len(body)-1]
		data := slices.Concat([]byte{entrySchema4}, []byte(`{"type":"bucket.delete","delete_bucket":{"name":"b"},`), payload, []byte("}"))
		decoded, err := decodeEntry(data, entrySchema5)
		if err != nil {
			t.Errorf("a bucket.delete of schema 4 with the payload of %s beside it = %v", name, err)
			continue
		}
		if decoded.SetBucketQuota != nil || decoded.WalkBucketUsage != nil || decoded.PutTarget != nil || decoded.DeleteTarget != nil ||
			decoded.AckOutbox != nil || decoded.SetBucketReplica != nil || decoded.PutResync != nil {
			t.Errorf("the payload of %s was read from an entry of schema 4", name)
		}
		// In an entry of schema 5 two payloads are one too many.
		data[0] = entrySchema5
		if _, err := decodeEntry(data, entrySchema5); !errors.Is(err, domain.ErrUnsupportedFormat) {
			t.Errorf("two payloads in an entry of schema 5 = %v", err)
		}
	}
	if len(seven) != 7 {
		t.Errorf("%d commands of schema 5 were tried, there are seven: %v", len(seven), seven)
	}
}

// TestAnEntryBelowSchema5SaysNothingOfSchema5: entries are decoded
// leniently, so a put that brings "replica" in an entry that claims schema 4
// is read by the binary of that schema without it. A state machine that
// applies schema 5 stores exactly what that one stores: the bytes it stores
// for the same entry without the field. So for "replica_from" on an upload,
// "replica" and "by" on a delete, and "quota_bytes" and "replica_from" on a
// bucket.create, which no entry of any schema may say.
func TestAnEntryBelowSchema5SaysNothingOfSchema5(t *testing.T) {
	t.Parallel()
	replica := &replicaRecord{From: "5d3c1f0a-site-a", Seq: 9000}
	plain := versionToRecord(porttest.SampleVersion("ver", "k", 2))
	with := *plain
	with.Replica, with.Replication = replica, string(object.ReplicationReplica)
	upload := uploadToRecord(porttest.SampleUpload("ver", "u", 3))
	from := *upload
	from.ReplicaFrom = "5d3c1f0a-site-a"
	marker := object.ID(bytes.Repeat([]byte{9}, object.IDSize)).String()
	tests := map[string]struct{ without, with entry }{
		"a put": {
			without: entry{Type: cmdPutObject, PutObject: plain},
			with:    entry{Type: cmdPutObject, PutObject: &with},
		},
		"an upload": {
			without: entry{Type: cmdCreateUpload, CreateUpload: upload},
			with:    entry{Type: cmdCreateUpload, CreateUpload: &from},
		},
		"a delete": {
			without: entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "ver", Key: "first", MarkerID: marker}},
			with:    entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "ver", Key: "first", MarkerID: marker, Replica: replica, By: "user/albie"}},
		},
		"a bucket": {
			without: entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "new", CreatedBy: "root"}},
			with:    entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "new", CreatedBy: "root", QuotaBytes: 5, ReplicaFrom: "5d3c1f0a-site-a"}},
		},
	}
	for name, test := range tests {
		for schema := byte(entrySchemaTimed); schema <= entrySchema4; schema++ {
			t.Run(fmt.Sprintf("%s in an entry of schema %d", name, schema), func(t *testing.T) {
				t.Parallel()
				states := map[string]map[string]string{}
				for form, e := range map[string]entry{"without": test.without, "with": test.with} {
					m := newMachine5(t)
					m.bucket("ver", bucket.VersioningEnabled)
					m.put("ver", "first", 1)
					if r := m.as(schema, e); r.err != nil {
						t.Fatalf("%s: %v", form, r.err)
					}
					states[form] = rawState(t, m.f, everyBucket(t, m.f)...)
					if got := stateSchema(t, m.f); got > uint64(schema) {
						t.Errorf("%s: the state says schema %d after an entry of schema %d", form, got, schema)
					}
				}
				if !reflect.DeepEqual(states["with"], states["without"]) {
					t.Errorf("an entry of schema %d that carries what schema 5 added is not applied as the same entry without it", schema)
				}
				if _, there := states["with"][string(bucketUsageBucket())]; there {
					t.Errorf("an entry of schema %d made the bucket of usage records", schema)
				}
			})
		}
	}
	// The two kinds of bucket configuration of schema 5 are, in an entry
	// below it, kinds there are not: refused as the release of schema 4
	// refuses them, and nothing is stored.
	for _, kind := range []string{bucket.ConfigNotification, bucket.ConfigReplication} {
		m := newMachine5(t)
		m.bucket("b", bucket.Unversioned)
		before := storedBucket(t, m.f, "b")
		r := m.as(entrySchema4, entry{Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Bucket: "b", Kind: kind, Document: []byte("{}")}})
		if !errors.Is(r.err, domain.ErrInvalidArgument) {
			t.Errorf("a %s document in an entry of schema 4 = %v, want %v", kind, r.err, domain.ErrInvalidArgument)
		}
		if after := storedBucket(t, m.f, "b"); !bytes.Equal(after, before) {
			t.Errorf("a %s document in an entry of schema 4 changed the bucket: %s", kind, after)
		}
	}
}

// ports5Delete is a delete that makes a marker, with what schema 5 lets a
// delete say.
func ports5Delete(marker object.ID, by string, replica *object.Replica) ports.DeleteOptions {
	return ports.DeleteOptions{MarkerID: marker, At: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC), By: by, Replica: replica}
}

// rawStateOf returns the keys of a rawState that are inside one top-level
// bucket.
func rawStateOf(state map[string]string, name []byte) map[string]string {
	out := map[string]string{}
	for key, value := range state {
		if strings.HasPrefix(key, string(name)+"/") {
			out[key] = value
		}
	}
	return out
}

// TestStateBecomesSchema5WithItsFirstEntryOfSchema5: the state is marked as
// schema 5 by the first entry of schema 5 that is applied to it - whether it
// took effect, was refused, or was applied as nothing - because the release
// of schema 4 would stop at that entry whatever became of it. Until then the
// state says 4, and holds nothing schema 5 keeps.
func TestStateBecomesSchema5WithItsFirstEntryOfSchema5(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		first   func(m *machine) entry
		refused error
		// kept names the buckets that have a usage record afterwards.
		kept []string
	}{
		"a bucket": {first: func(m *machine) entry {
			return entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "second", CreatedAt: m.at, CreatedBy: "root"}}
		}, kept: []string{"second"}},
		"a bucket that exists": {first: func(m *machine) entry {
			return entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedAt: m.at}}
		}, refused: domain.ErrBucketAlreadyExists},
		"a walk": {first: func(*machine) entry {
			return entry{Type: cmdWalkBucketUsage, WalkBucketUsage: &usageWalkCmd{Bucket: "b", Limit: 1}}
		}, kept: []string{"b"}},
		"a walk of a bucket there is not": {first: func(*machine) entry {
			return entry{Type: cmdWalkBucketUsage, WalkBucketUsage: &usageWalkCmd{Bucket: "missing", Limit: 1}}
		}, refused: domain.ErrBucketNotFound},
		"a quota for a bucket that was not counted": {first: func(*machine) entry {
			return entry{Type: cmdSetBucketQuota, SetBucketQuota: &bucketQuotaCmd{Bucket: "b", Bytes: 10}}
		}, refused: domain.ErrConflict},
		"a delete that says who": {first: func(m *machine) entry {
			return entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "b", Key: "k", At: m.at, By: "user/albie"}}
		}},
		"a delete of nothing": {first: func(m *machine) entry {
			return entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: "b", Key: "never", At: m.at, By: "user/albie"}}
		}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMachine5(t)
			// Everything of schema 4, as a cluster at level 4 writes it.
			populateSchema4(m)
			m.at4(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedAt: m.at, CreatedBy: "root"}})
			m.put4("b", "k", "root", 30)
			if got := stateSchema(t, m.f); got != stateSchema4 {
				t.Fatalf("after entries of schema 4: state schema %d", got)
			}
			if got := rawState(t, m.f, bucketUsageBucket()); len(got) != 0 {
				t.Fatalf("a state of schema 4 holds %v", got)
			}
			before := rawState(t, m.f, everyBucket(t, m.f)...)

			r := m.as(entrySchema5, test.first(m))
			if !errors.Is(r.err, test.refused) {
				t.Fatalf("the first entry of schema 5 = %v, want %v", r.err, test.refused)
			}
			if got := stateSchema(t, m.f); got != stateSchema5 {
				t.Fatalf("after an entry of schema 5: state schema %d", got)
			}
			var kept []string
			for key := range rawStateOf(rawState(t, m.f, bucketUsageBucket()), bucketUsageBucket()) {
				kept = append(kept, key)
			}
			slices.Sort(kept)
			var want []string
			for _, name := range test.kept {
				want = append(want, fmt.Sprintf("%s/%x", bucketUsageBucket(), name))
			}
			if !slices.Equal(kept, want) {
				t.Errorf("usage records afterwards: %v, want %v", kept, want)
			}
			if test.refused != nil {
				// Nothing but the mark: a refusal changes nothing else.
				after := rawState(t, m.f, everyBucket(t, m.f)...)
				for _, key := range [][]byte{schemaKey(), appliedKey(), timeKey()} {
					delete(before, fmt.Sprintf("meta/%x", key))
					delete(after, fmt.Sprintf("meta/%x", key))
				}
				if !reflect.DeepEqual(after, before) {
					t.Errorf("a refused entry of schema 5 changed the state")
				}
			}
		})
	}
}

// TestAStateOfSchema5IsOpenedOnlyByABinaryOfSchema5: this binary announces
// schema 4, and refuses a state of schema 5 and a snapshot of one as the
// release of schema 4 does - there is no way back once an entry of schema 5
// was applied. The binary it becomes at PLAN 8.0.10 opens both, and goes on.
func TestAStateOfSchema5IsOpenedOnlyByABinaryOfSchema5(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	f, err := openFSMOf(root, entrySchema5)
	if err != nil {
		t.Fatal(err)
	}
	m := &machine{t: t, f: f, at: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	m.ok5(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedAt: m.at}})
	m.ok5(entry{Type: cmdPutObject, PutObject: versionToRecord(porttest.SampleVersion("b", "k", 3))})
	want := rawState(t, f, everyBucket(t, f)...)
	snap, err := f.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatal(err)
	}
	snap.Release()
	if err := f.close(); err != nil {
		t.Fatal(err)
	}

	if f, err := openFSM(root); !errors.Is(err, domain.ErrUnsupportedFormat) {
		if f != nil {
			_ = f.close()
		}
		t.Fatalf("this binary opens a state of schema 5: %v", err)
	}
	if f, err = openFSMOf(root, entrySchema5); err != nil {
		t.Fatalf("a binary of schema 5 does not open its own state: %v", err)
	}
	defer func() { _ = f.close() }()
	if got := rawState(t, f, everyBucket(t, f)...); !reflect.DeepEqual(got, want) {
		t.Errorf("the state of schema 5 was changed by being opened")
	}

	// A snapshot of it: refused by this binary, which keeps the state it
	// has; taken by one of schema 5.
	old := newMachine(t)
	old.bucket("mine", bucket.Unversioned)
	mine := rawState(t, old.f, everyBucket(t, old.f)...)
	if err := old.f.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Fatalf("this binary restores a snapshot of schema 5: %v", err)
	}
	if got := rawState(t, old.f, everyBucket(t, old.f)...); !reflect.DeepEqual(got, mine) {
		t.Errorf("a snapshot that was refused changed the state")
	}
	next := newMachine5(t)
	if err := next.f.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("a binary of schema 5 does not restore a snapshot of schema 5: %v", err)
	}
	if got := rawState(t, next.f, everyBucket(t, next.f)...); !reflect.DeepEqual(got, want) {
		t.Errorf("the restored state is not the one the snapshot was taken of")
	}
}

// TestThisBinaryStopsAtAnEntryOfSchema5: the binary that announces schema 4
// does not apply an entry of schema 5 in some other way - not a put it could
// read every field of, either. It stops, with its state as it was.
func TestThisBinaryStopsAtAnEntryOfSchema5(t *testing.T) {
	t.Parallel()
	commands := schema5Commands(t)
	commands["a put that says nothing new"] = entry{Type: cmdPutObject, PutObject: versionToRecord(porttest.SampleVersion("b", "k", 2))}
	commands["a bucket"] = entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "c"}}
	for name, e := range commands {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMachine(t)
			m.bucket("b", bucket.Unversioned)
			before := rawState(t, m.f, everyBucket(t, m.f)...)
			if r := m.raw(entrySchema5, e); !r.fatal || !errors.Is(r.err, domain.ErrUnsupportedFormat) {
				t.Fatalf("an entry of schema 5 on this binary = %+v, want it to stop with %v", r.err, domain.ErrUnsupportedFormat)
			}
			if after := rawState(t, m.f, everyBucket(t, m.f)...); !reflect.DeepEqual(after, before) {
				t.Errorf("the entry it stopped at changed the state")
			}
		})
	}
}

// TestWhatSchema5DoesNotApplyYet: the entries of schema 5 whose state
// machine is not written yet are refused as unavailable by one that applies
// schema 5, and change nothing but the mark of the state. PLAN 8.0.8 takes
// targets, the outbox and the two kinds of bucket configuration out of this
// list, PLAN 8.0.9 replica buckets, replicas and resyncs; when it is empty
// the test goes, with notYet.
func TestWhatSchema5DoesNotApplyYet(t *testing.T) {
	t.Parallel()
	applied := map[string]bool{
		"bucket.quota.put": true, "bucket.quota.put that removes": true, "bucket.usage.walk": true, "bucket.usage.walk that restarts": true,
		"object.version.delete that says who": true,
		// A document that is removed names nothing, but it is of a kind
		// whose rules are not written.
		"bucket.config.put that removes notification rules": false,
	}
	for name, e := range schema5Commands(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := newMachine5(t)
			m.ok5(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "photos", CreatedAt: m.at}})
			m.ok5(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "photos", Status: string(bucket.VersioningEnabled)}})
			before := rawState(t, m.f, everyBucket(t, m.f)...)
			r := m.as(entrySchema5, e)
			if got := errors.Is(r.err, domain.ErrUnavailable); got == applied[name] {
				t.Fatalf("%s = %v; applied by this release: %t", name, r.err, applied[name])
			}
			if applied[name] {
				return
			}
			after := rawState(t, m.f, everyBucket(t, m.f)...)
			for _, key := range [][]byte{appliedKey(), timeKey()} {
				delete(before, fmt.Sprintf("meta/%x", key))
				delete(after, fmt.Sprintf("meta/%x", key))
			}
			if !reflect.DeepEqual(after, before) {
				t.Errorf("an entry that is not applied yet changed the state")
			}
		})
	}
}

// TestAResultOfAnAcknowledgementIsReadAsItIsWritten: a result names a row by
// its place or a version by itself, and is read back as the result it was
// written from; what is no place and no version is refused, and what names
// both is left to delivery.Result.Validate, which refuses it.
func TestAResultOfAnAcknowledgementIsReadAsItIsWritten(t *testing.T) {
	t.Parallel()
	id := object.ID(bytes.Repeat([]byte{7}, object.IDSize))
	for _, want := range []delivery.Result{
		{Seq: delivery.Seq{Index: 12, Row: 3}, Status: object.ReplicationCompleted},
		{Seq: delivery.Seq{Index: 1<<64 - 1, Row: 65535}, Status: object.ReplicationFailed},
		{Bucket: "b", Key: "k", Version: object.VersionRef{ID: id}, Status: object.ReplicationCompleted},
		{Bucket: "b", Key: "k", Version: object.VersionRef{Null: true}, Status: object.ReplicationFailed},
	} {
		record := resultToRecord(want)
		got, err := record.toDomain()
		if err != nil || got != want {
			t.Errorf("%+v is read as %+v, %v", record, got, err)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("%+v: %v", record, err)
		}
		if (record.Seq == "") == (record.VersionRef == "") {
			t.Errorf("%+v names a row and a version, or neither", record)
		}
	}
	for name, record := range map[string]ackResult{
		"a place that is too short":  {Seq: "0c", Status: "COMPLETED"},
		"a place in upper case":      {Seq: "000000000000000C0003", Status: "COMPLETED"},
		"the place before every row": {Seq: delivery.Seq{}.String(), Status: "COMPLETED"},
		"a version that is none":     {Bucket: "b", Key: "k", VersionRef: "latest", Status: "COMPLETED"},
		"the version of all zeros":   {Bucket: "b", Key: "k", VersionRef: object.ID{}.String(), Status: "COMPLETED"},
	} {
		if _, err := record.toDomain(); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s = %v, want %v", name, err, domain.ErrInvalidArgument)
		}
	}
	both, err := ackResult{Seq: delivery.Seq{Index: 1}.String(), Bucket: "b", Key: "k", VersionRef: "null", Status: "COMPLETED"}.toDomain()
	if err != nil || both.Validate() == nil {
		t.Errorf("a result that names a row and a version = %+v, %v; it is for Validate to refuse", both, err)
	}
	// The records of a target and of a resync, there and back.
	for _, kind := range []delivery.Kind{delivery.KindWebhook, delivery.KindNATS, delivery.KindS3} {
		target := porttest.SampleTarget("site-b", kind)
		target.Refs = 2
		if got := targetToRecord(target).toDomain(); !reflect.DeepEqual(got, target) {
			t.Errorf("a %s target is read back as %#v", kind, got.Shown())
		}
	}
	resync := bucket.Resync{Bucket: "b", Target: "site-b", State: bucket.ResyncRunning, Key: "k", Version: "null", Scanned: 9, Sent: 2}
	if got := resyncToRecord(resync).toDomain(); got != resync {
		t.Errorf("a resync is read back as %+v", got)
	}
	replica := &object.Replica{From: "site-a", Seq: 5, Null: true}
	if got := replicaToRecord(replica).toDomain(); *got != *replica || replicaToRecord(nil) != nil || (*replicaRecord)(nil).toDomain() != nil {
		t.Errorf("a replica is read back as %+v", got)
	}
}

// TestThisReleaseAppliesALogOfLevel4AsTheSchema4ReleaseDoesWhenItKnowsSchema5:
// the state machine that applies schema 5 - what this binary is from PLAN
// 8.0.10 on - makes of a log of level 4 the outcomes and the state database
// of the release of schema 4, byte for byte. Nothing of schema 5 is in a
// state no entry of schema 5 was applied to: not the bucket of usage
// records, and not a byte of any record that was there before.
func TestThisReleaseAppliesALogOfLevel4AsTheSchema4ReleaseDoesWhenItKnowsSchema5(t *testing.T) {
	t.Parallel()
	want, err := fs.ReadFile(os.DirFS("testdata"), schema4ReleaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := schema4ReleaseSums(t, schema4ReleaseLog(t), entrySchema5); !bytes.Equal(got, want) {
		t.Errorf("the log of level 4 and what a state machine of schema 5 makes of it are not those of the release of schema 4:\n got %swant %s", got, want)
	}
}

// TestAStoreHeldToSchema4WritesNothingOfSchema5: this binary announces
// schema 4, so its store is one of a cluster at level 4. A put, a delete that
// says who and a bucket are written and applied exactly as before - who
// deleted is left out - and what has no older form is refused as
// unavailable, with nothing logged: a replica, an upload of a source, a
// replica delete.
func TestAStoreHeldToSchema4WritesNothingOfSchema5(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openStore(t, t.TempDir())
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b", CreatedBy: "user/albie"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBucketVersioning(ctx, "b", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	v := porttest.SampleVersion("b", "k", 1)
	if _, err := store.PutVersion(ctx, v); err != nil {
		t.Fatal(err)
	}
	marker := object.ID(bytes.Repeat([]byte{5}, object.IDSize))
	result, err := store.DeleteObject(ctx, "b", "k", ports5Delete(marker, "user/albie", nil))
	if err != nil || result.Marker == nil {
		t.Fatalf("a delete that says who, at level 4 = %+v, %v", result, err)
	}
	if got := stateSchema(t, store.fsm); got != stateSchema4 {
		t.Errorf("state schema %d after a put, a delete and a bucket at level 4", got)
	}
	if got := rawState(t, store.fsm, bucketUsageBucket()); len(got) != 0 {
		t.Errorf("a store at level 4 keeps usage records: %v", got)
	}
	applied, err := store.fsm.appliedIndex()
	if err != nil {
		t.Fatal(err)
	}

	replica := &object.Replica{From: "5d3c1f0a-site-a", Seq: 7}
	copied := porttest.SampleVersion("b", "copied", 2)
	copied.Replica = replica
	if _, err := store.PutVersion(ctx, copied); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("a replica put at level 4 = %v, want %v", err, domain.ErrUnavailable)
	}
	if _, err := store.DeleteObject(ctx, "b", "k", ports5Delete(marker, "", replica)); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("a replica delete at level 4 = %v, want %v", err, domain.ErrUnavailable)
	}
	upload := porttest.SampleUpload("b", "u", 3)
	upload.ReplicaFrom = replica.From
	if err := store.CreateUpload(ctx, upload); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("an upload of a source at level 4 = %v, want %v", err, domain.ErrUnavailable)
	}
	if after, err := store.fsm.appliedIndex(); err != nil || after != applied {
		t.Errorf("what is unavailable at level 4 was logged: applied index %d, was %d (%v)", after, applied, err)
	}
	// And the entries it did write say nothing a member of schema 4 does
	// not know.
	if err := store.fsm.view(func(tx *bbolt.Tx) error {
		if tx.Bucket(bucketUsageBucket()) != nil {
			t.Errorf("the state has a bucket of usage records")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// FuzzDecodeEntryOfSchema5: whatever bytes a log holds, a binary that
// applies schema 5 decodes them into exactly one well-formed command or
// refuses them with ErrUnsupportedFormat - never a panic, never two
// payloads. What decodes is written again and decodes as the same entry; a
// command of schema 5 comes from an entry of schema 5 only; and the binary of
// schema 4 reads an entry exactly when its byte is below 5.
func FuzzDecodeEntryOfSchema5(f *testing.F) {
	f.Add([]byte{5})
	f.Add(append([]byte{5}, `{}`...))
	f.Add(append([]byte{5}, `{"type":"bucket.quota.put","set_bucket_quota":{"bucket":"b","bytes":4611686018427387904,"audit":{"actor":"root"}}}`...))
	f.Add(append([]byte{5}, `{"type":"bucket.usage.walk","walk_bucket_usage":{"bucket":"b","limit":10000,"restart":true}}`...))
	f.Add(append([]byte{4}, `{"type":"bucket.usage.walk","walk_bucket_usage":{"bucket":"b","limit":1}}`...))
	f.Add(append([]byte{5}, `{"type":"target.put","put_target":{"target":{"v":1,"name":"hook","kind":"webhook","endpoint":"https://h.example/in","buckets":["*"],"auth":{"mode":"bearer"},"secret":{"kms_key_id":"k","wrapped_key":"AA==","nonce":"AA==","ciphertext":"AA=="},"created_at":"2026-10-12T09:00:00Z","updated_at":"2026-10-12T09:00:00Z"}}}`...))
	f.Add(append([]byte{5}, `{"type":"target.delete","delete_target":{"name":"hook"}}`...))
	f.Add(append([]byte{5}, `{"type":"outbox.ack","ack_outbox":{"target":"site-b","upto":"000000000000000e0000","audit_upto":7,"results":[{"seq":"000000000000000c0003","status":"COMPLETED"},{"b":"b","k":"k","vid":"null","status":"FAILED"}]}}`...))
	f.Add(append([]byte{5}, `{"type":"outbox.ack","ack_outbox":{"target":"site-b","upto":"zz","results":[{"seq":"1","b":"b","vid":"x"}]},"put_resync":{}}`...))
	f.Add(append([]byte{5}, `{"type":"bucket.replica.put","set_bucket_replica":{"bucket":"b","source":"site-a"}}`...))
	f.Add(append([]byte{5}, `{"type":"replication.resync.put","put_resync":{"bucket":"b","target":"t","state":"running","key":"","version":"","scanned":0,"sent":0}}`...))
	f.Add(append([]byte{5}, `{"type":"object.put","put_object":{"bucket":"b","key":"k","replica":{"from":"site-a","seq":18446744073709551615,"null":true},"replication":"REPLICA"}}`...))
	f.Add(append([]byte{4}, `{"type":"object.put","put_object":{"bucket":"b","key":"k","replica":{"from":"site-a","seq":1}},"set_bucket_quota":{"bucket":"b","bytes":1}}`...))
	f.Add(append([]byte{5}, `{"type":"object.version.delete","delete_version":{"bucket":"b","key":"k","marker_id":"00","at":"2026-10-12T09:00:00Z","replica":{"from":"a","seq":1},"by":"root"}}`...))
	f.Add(append([]byte{5}, `{"type":"upload.create","create_upload":{"bucket":"b","key":"k","replica_from":"site-a"}}`...))
	f.Add(append([]byte{5}, `{"type":"bucket.config.put","set_bucket_config":{"bucket":"b","kind":"notification","document":"e30="}}`...))
	f.Add(append([]byte{5}, `{"type":"bucket.create","create_bucket":{"name":"b","quota_bytes":1,"replica_from":"a"}}`...))
	f.Add(append([]byte{6}, `{"type":"bucket.create","create_bucket":{"name":"b"}}`...))
	f.Fuzz(func(t *testing.T, data []byte) {
		e, err := decodeEntry(data, entrySchema5)
		if err != nil {
			if !errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Fatalf("untyped error: %v", err)
			}
			// What a binary of schema 5 refuses, the one of schema 4 does
			// not read either - but for nothing: an entry of an older
			// schema is read by both alike.
			if _, older := decodeEntry(data, entrySchema4); older == nil {
				t.Fatalf("a binary of schema 4 read what one of schema 5 refuses")
			}
			return
		}
		if data[0] > entrySchema5 {
			t.Fatalf("an entry of schema %d was decoded", data[0])
		}
		payloads := 0
		for _, set := range []bool{
			e.CreateBucket != nil, e.DeleteBucket != nil, e.PutObject != nil, e.DeleteObject != nil, e.SetRoot != nil, e.SetTopology != nil,
			e.SetNode != nil, e.RemoveNode != nil, e.IAM != nil, e.SetVersioning != nil, e.DeleteVersion != nil, e.SetBucketLock != nil,
			e.SetRetention != nil, e.SetLegalHold != nil, e.CreateUpload != nil, e.PutPart != nil, e.CompleteUpload != nil, e.AbortUpload != nil,
			e.SetBucketConfig != nil, e.SetTags != nil,
			e.AddReads != nil, e.PutSubBucket != nil, e.DeleteSubBucket != nil, e.PutTagDefinition != nil, e.DeleteTagDefinition != nil,
			e.SetBucketQuota != nil, e.WalkBucketUsage != nil, e.PutTarget != nil, e.DeleteTarget != nil, e.AckOutbox != nil,
			e.SetBucketReplica != nil, e.PutResync != nil,
		} {
			if set {
				payloads++
			}
		}
		if payloads != 1 {
			t.Fatalf("decoded entry %q has %d payloads", e.Type, payloads)
		}
		fifth := e.SetBucketQuota != nil || e.WalkBucketUsage != nil || e.PutTarget != nil || e.DeleteTarget != nil || e.AckOutbox != nil ||
			e.SetBucketReplica != nil || e.PutResync != nil
		if fifth != (versionOf(entry{Type: e.Type}) == entrySchema5) {
			t.Fatalf("decoded entry %q has a payload of schema 5: %v", e.Type, fifth)
		}
		if fifth && data[0] < entrySchema5 {
			t.Fatalf("a command of schema 5, %q, was decoded from an entry of schema %d", e.Type, data[0])
		}
		if needs := versionOf(e); needs > entrySchema5 || needs < entrySchemaVersion {
			t.Fatalf("decoded entry %q needs schema %d", e.Type, needs)
		}
		// What decodes can be written again in the schema it came in, and
		// then has one form: it decodes as the same entry and is written
		// as the same bytes.
		again, err := encodeEntry(e, data[0])
		if err != nil {
			t.Fatalf("a decoded entry cannot be encoded: %v", err)
		}
		twice, err := decodeEntry(again, entrySchema5)
		if err != nil || twice.Type != e.Type {
			t.Fatalf("a decoded entry does not decode again: %v", err)
		}
		if third, err := encodeEntry(twice, data[0]); err != nil || !bytes.Equal(third, again) {
			t.Fatalf("an entry is written as %s and then as %s (%v)", again, third, err)
		}
		// The binary of schema 4 reads it exactly when its byte is below 5.
		if _, err := decodeEntry(data, entrySchema4); (err == nil) != (data[0] < entrySchema5) {
			t.Fatalf("a binary of schema 4 and %q in an entry of schema %d: %v", e.Type, data[0], err)
		}
		// What it carries of a result is a result or refused, never a
		// panic.
		if e.AckOutbox != nil {
			for _, r := range e.AckOutbox.Results {
				if result, err := r.toDomain(); err != nil && !errors.Is(err, domain.ErrInvalidArgument) && !errors.Is(err, domain.ErrInvalidID) {
					t.Fatalf("a result %+v = %+v, %v", r, result, err)
				}
			}
			if _, err := delivery.ParseSeq(e.AckOutbox.Upto); err != nil && !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("upto %q = %v", e.AckOutbox.Upto, err)
			}
		}
	})
}
