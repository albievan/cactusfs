package raftmeta

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// FuzzDecodeOutboxRow: whatever bytes are stored as a row of the outbox, they
// are read as a row that can be kept or refused as damaged - never a panic,
// never a row for nobody. What is read is written again as a record that is
// read as the same row.
func FuzzDecodeOutboxRow(f *testing.F) {
	key := delivery.Seq{Index: 47, Row: 2}.Key()
	f.Add(key[:], []byte(`{"v":1,"e":"put","b":"photos","k":"a.jpg","vid":"07070707070707070707070707070707","sz":1024,"et":"etag","at":"2026-10-12T09:00:17Z","by":"root","to":["n:hook","r:site-b"]}`))
	f.Add(key[:], []byte(`{"v":1,"e":"marker","b":"photos","k":"zoë","vid":"null","at":"2026-10-12T09:00:17.5Z","to":["n:hook"]}`))
	f.Add(key[:], []byte(`{"v":2,"e":"put"}`))
	f.Add(key[:], []byte(`{"v":1,"e":"put","b":"b","k":"k","vid":"latest","at":"2026-10-12T09:00:17Z","to":["x:hook"]}`))
	f.Add(key[:], []byte(`{"v":1,"e":"read","b":"b","k":"k","vid":"null","sz":-1,"at":"2026-10-12T09:00:17Z","to":["n:hook","r:hook"]}`))
	f.Add(key[:9], []byte(`{}`))
	f.Add([]byte{}, []byte(`[]`))
	f.Fuzz(func(t *testing.T, key, raw []byte) {
		item, err := decodeItem(key, raw)
		if err != nil {
			if !errors.Is(err, domain.ErrIntegrity) && !errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Fatalf("untyped error: %v", err)
			}
			return
		}
		if err := item.Validate(); err != nil || len(item.To) == 0 || len(key) != delivery.SeqSize || len(item.ETag) > delivery.MaxItemETag {
			t.Fatalf("a row was read that cannot be kept: %+v, %v", item, err)
		}
		again, err := json.Marshal(itemToRecord(item))
		if err != nil {
			t.Fatal(err)
		}
		at := item.Seq.Key()
		twice, err := decodeItem(at[:], again)
		if err != nil || !bytes.Equal(at[:], key) {
			t.Fatalf("a row that was read is written as %s, which is read as %v", again, err)
		}
		// The time is compared as a time: it is written in UTC.
		if !twice.At.Equal(item.At) {
			t.Fatalf("the time of a row: %s, then %s", item.At, twice.At)
		}
		twice.At = item.At
		if !reflect.DeepEqual(twice, item) {
			t.Fatalf("a row is read as %+v, written as %s and read as %+v", item, again, twice)
		}
		if third, err := json.Marshal(itemToRecord(twice)); err != nil || !bytes.Equal(third, again) {
			t.Fatalf("a row is written as %s and then as %s (%v)", again, third, err)
		}
	})
}

// FuzzDecodeCursor: whatever bytes are stored as a cursor, they are read as
// one or refused as damaged, and what is read is written as bytes that are
// read as the same cursor.
func FuzzDecodeCursor(f *testing.F) {
	f.Add("hook", []byte(`{"v":1,"from":"000000000000000f0000","acked":"00000000000000290000","audit_acked":77,"lost":2,"resync_needed":true}`))
	f.Add("site-b", []byte(`{"v":1,"from":"00000000000000000000","acked":"00000000000000000000","audit_acked":0,"lost":0,"resync_needed":false}`))
	f.Add("x", []byte(`{"v":1,"from":"0F","acked":""}`))
	f.Add("x", []byte(`{"v":3}`))
	f.Add("", []byte(`null`))
	f.Fuzz(func(t *testing.T, target string, raw []byte) {
		cursor, err := decodeCursor(target, raw)
		if err != nil {
			if !errors.Is(err, domain.ErrIntegrity) && !errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Fatalf("untyped error: %v", err)
			}
			return
		}
		if cursor.Target != target {
			t.Fatalf("the cursor of %q is read as that of %q", target, cursor.Target)
		}
		again, err := json.Marshal(cursorRecord{
			V: cursorRecordVersion, From: cursor.From.String(), Acked: cursor.Acked.String(), AuditAcked: cursor.AuditAcked, Lost: cursor.Lost, ResyncNeeded: cursor.ResyncNeeded,
		})
		if err != nil {
			t.Fatal(err)
		}
		if twice, err := decodeCursor(target, again); err != nil || twice != cursor {
			t.Fatalf("a cursor is read as %+v, written as %s and read as %+v, %v", cursor, again, twice, err)
		}
	})
}

// FuzzTargetRecord: whatever a target.put carries as its target, what
// delivery.Target.Validate takes of it - which is what the state machine
// keeps - is an origin without a path, with sealed fields of a size that is
// kept, and is written and read again as the same record. Nothing of it is
// printed.
func FuzzTargetRecord(f *testing.F) {
	for _, name := range []string{"target.put of a webhook", "target.put of a NATS server", "target.put of a deployment"} {
		data, err := json.Marshal(schema5Commands(f)[name].PutTarget.Target)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add([]byte(`{"v":1,"name":"hook","kind":"webhook","endpoint":"https://h.example/in/token","buckets":["*"],"auth":{"mode":"none"}}`))
	f.Add([]byte(`{"v":1,"name":"hook","kind":"webhook","endpoint":"https://h.example","endpoint_path":{"ciphertext":"AA=="},"buckets":null,"auth":{"mode":""}}`))
	f.Add([]byte(`{"v":1,"name":"bus","kind":"nats","endpoint":"tls://n.example:4222","endpoint_path":{"nonce":"AA=="},"subject":"s","auth":{"mode":"none"}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var record targetRecord
		if json.Unmarshal(data, &record) != nil {
			return
		}
		target := record.toDomain()
		err := target.Validate()
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("untyped error: %v", err)
			}
			// A refusal repeats nothing of where the target is.
			if _, after, found := strings.Cut(record.Endpoint, "://"); found && len(after) > 8 && strings.Contains(err.Error(), after) {
				t.Fatalf("the refusal repeats the endpoint: %v", err)
			}
			return
		}
		origin, path := delivery.SplitEndpoint(target.Endpoint)
		if origin != target.Endpoint || path != "" || target.Origin() != target.Endpoint {
			t.Fatalf("a target is kept with something behind its origin")
		}
		for _, sealed := range []*sealedRecord{record.Secret, record.SignKey, record.EndpointPath} {
			if sealed != nil && (len(sealed.Ciphertext) > delivery.MaxSealedBytes || len(sealed.WrappedKey) > delivery.MaxSealedKeyBytes || len(sealed.KMSKeyID) > delivery.MaxSealedKeyBytes) {
				t.Fatalf("a target is kept with a sealed field of %d bytes", len(sealed.Ciphertext))
			}
		}
		if target.PathSet() && target.Kind != delivery.KindWebhook {
			t.Fatalf("a %s target is kept with a path", target.Kind)
		}
		again, err := json.Marshal(targetToRecord(target))
		if err != nil {
			t.Fatal(err)
		}
		var twice targetRecord
		if err := json.Unmarshal(again, &twice); err != nil {
			t.Fatal(err)
		}
		if third, err := json.Marshal(targetToRecord(twice.toDomain())); err != nil || !bytes.Equal(third, again) {
			t.Fatalf("a target is written as %s and then as %s (%v)", again, third, err)
		}
		// However it is printed, it is its name and its kind.
		if shown := target.String(); shown != "target "+target.Name+" ("+string(target.Kind)+")" {
			t.Fatalf("a target is printed as %q", shown)
		}
	})
}

// FuzzApplyOfSchema5: whatever an entry of schema 5 carries for the commands
// of targets, the outbox and replicas, the state machine applies it or
// refuses it - it never stops, since every member would stop with it - and a
// refusal leaves the state as it was, byte for byte (but for the empty place
// of a bucket among the versions, which three commands that were there before
// schema 5 may leave as the release of schema 4 leaves it). The state the entries
// meet has targets of every kind, a bucket that tells and sends on, a
// replica bucket, and rows in the outbox.
func FuzzApplyOfSchema5(f *testing.F) {
	commands := schema5Commands(f)
	for _, e := range commands {
		data, err := json.Marshal(e)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add([]byte(`{"type":"outbox.ack","ack_outbox":{"target":"site-b","upto":"ffffffffffffffffffff","audit_upto":18446744073709551615,"results":[{"b":"photos","k":"a","vid":"null","status":"FAILED"},{"seq":"00000000000000010000","status":"COMPLETED"}]}}`))
	f.Add([]byte(`{"type":"object.put","put_object":{"bucket":"copy","key":"k","version_id":"07070707070707070707070707070707","data_id":"08080808080808080808080808080808","size":-5,"replica":{"from":"5d3c1f0a-site-a","seq":4611686018427387904,"null":true}}}`))
	f.Add([]byte(`{"type":"object.version.delete","delete_version":{"bucket":"copy","key":"k","marker_id":"00000000000000000000000000000000","replica":{"from":"5d3c1f0a-site-a","seq":3},"by":"a\nb"}}`))
	f.Add([]byte(`{"type":"bucket.config.put","set_bucket_config":{"bucket":"photos","kind":"replication","document":"eyJ2IjoxfQ=="}}`))
	f.Add([]byte(`{"type":"replication.resync.put","put_resync":{"bucket":"photos","target":"site-b","state":"done","key":"k","version":"null","scanned":18446744073709551615,"sent":1}}`))
	f.Add([]byte(`{"type":"bucket.create","create_bucket":{"name":"new","config":{"notification":"e30=","replication":"e30="}}}`))
	f.Add([]byte(`{"type":"object.version.delete","delete_version":{"bucket":"photos","key":"b.jpg","marker_id":"00000000000000000000000000000000","by":"root"}}`))
	f.Add([]byte(`{"type":"object.version.delete","delete_version":{"bucket":"copy","key":"k","marker_id":"nonsense","expect":"07070707070707070707070707070707","replica":{"from":"5d3c1f0a-site-a","seq":3,"null":true}}}`))
	f.Add([]byte(`{"type":"outbox.ack","ack_outbox":{"target":"hook","upto":"00000000000000000000","results":[]}}`))
	f.Add([]byte(`{"type":"outbox.ack","ack_outbox":{"target":"site-b","upto":"00000000000000000000","results":[{"b":"copy","k":"k","vid":"null","status":"FAILED"}]}}`))
	f.Add([]byte(`{"type":"replication.resync.put","put_resync":{"bucket":"copy","target":"site-b","state":"done","key":"","version":"","scanned":0,"sent":0}}`))
	if closed, err := json.Marshal(targetEntry("hook", delivery.KindWebhook, "elsewhere")); err == nil {
		f.Add(closed)
	}

	// One state machine for all inputs of a process: what an entry leaves is
	// what the next one meets.
	root, err := os.OpenRoot(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}
	machine, err := openFSMOf(root, entrySchema5)
	if err != nil {
		f.Fatal(err)
	}
	machine.db.NoSync = true
	f.Cleanup(func() { _ = machine.close(); _ = root.Close() })
	index := uint64(0)
	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	apply := func(e entry) applyResult {
		index++
		at = at.Add(time.Second)
		e.At = at
		data, err := encodeEntry(e, entrySchema5)
		if err != nil {
			f.Fatal(err)
		}
		r, _ := machine.Apply(&raft.Log{Index: index, Type: raft.LogCommand, Data: data}).(applyResult)
		return r
	}
	scene := []entry{
		{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "photos", CreatedAt: at}},
		{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "photos", Status: string(bucket.VersioningEnabled)}},
		{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "copy", CreatedAt: at}},
		{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "copy", Status: string(bucket.VersioningEnabled)}},
		targetEntry("hook", delivery.KindWebhook), targetEntry("bus", delivery.KindNATS), targetEntry("site-b", delivery.KindS3),
		commands["bucket.config.put of notification rules"], commands["bucket.config.put of a replication configuration"],
		replicaModeEntry("copy", siteA),
		putEntry("photos", "logs/a.gz", 10, 1), putEntry("photos", "b.jpg", 10, 2), deleteEntry("photos", "b.jpg", "", 3),
		replicaPut("copy", "k", 5, 4, 40), replicaMarker("copy", "k", 5, 50),
	}
	for _, e := range scene {
		if r := apply(e); r.err != nil {
			f.Fatalf("%s: %v", e.Type, r.err)
		}
	}
	setOutboxBound(machine, 8)

	f.Fuzz(func(t *testing.T, data []byte) {
		e, err := decodeEntry(append([]byte{entrySchema5}, data...), entrySchema5)
		if err != nil {
			return
		}
		switch e.Type {
		case cmdPutTarget, cmdDeleteTarget, cmdAckOutbox, cmdSetBucketReplica, cmdPutResync, cmdSetBucketConfig, cmdPutObject, cmdDeleteVersion,
			cmdCreateUpload, cmdCompleteUpload, cmdCreateBucket, cmdSetVersioning, cmdSetTags:
		default:
			// What was there before schema 5 has its own tests; and an
			// IAM command may append to the audit trail what this test does
			// not look at.
			return
		}
		if e.CreateBucket != nil && bucket.ValidateName(e.CreateBucket.Name) != nil {
			// Found by this test, and older than schema 5: a bucket.create
			// is not held to a name in apply, and one without a name - which
			// no proposer writes - cannot be stored and stops the state
			// machine, in the release of schema 4 alike. Apply stays as that
			// release has it; the store refuses such a name before it
			// proposes (Store.CreateBucket, and its test in
			// delivery_rules_test.go).
			return
		}
		before := settled(t, machine)
		r := apply(e)
		if r.fatal {
			t.Fatalf("%s stopped the state machine: %v", e.Type, r.err)
		}
		if errors.Is(r.err, domain.ErrUnavailable) && e.Type != cmdPutObject && e.Type != cmdCompleteUpload {
			// Only Object Lock answers so, for an entry without a time.
			t.Fatalf("%s is answered as unavailable: %v", e.Type, r.err)
		}
		after := settled(t, machine)
		if r.err != nil && !reflect.DeepEqual(after, before) {
			// Three commands the release of schema 4 knew may leave the empty
			// place of a bucket among the versions behind when they are
			// refused, as that release does: a put and a completion, at the
			// refusals that were there before schema 5 (commitVersion), and
			// the tags of a version (versionTarget). Nothing else leaves
			// anything - no delete, and no command of schema 5.
			leaves := e.Type == cmdPutObject || e.Type == cmdCompleteUpload || e.Type == cmdSetTags
			for key, value := range after {
				if before[key] != value && (!leaves || value != "" || (!strings.HasPrefix(key, "versions/") && !strings.HasPrefix(key, "versionids/"))) {
					t.Fatalf("%s was refused (%v) and changed %s from %q to %q", e.Type, r.err, key, before[key], value)
				}
			}
			for key := range before {
				if _, there := after[key]; !there {
					t.Fatalf("%s was refused (%v) and removed %s", e.Type, r.err, key)
				}
			}
		}
		// What is kept stays what can be read: every row, every cursor,
		// every target, and the counts.
		rows := storedRows(t, machine)
		if count, _ := outboxCounts(t, machine); count != uint64(len(rows)) || count > 8 {
			t.Fatalf("after %s the outbox counts %d rows and holds %d", e.Type, count, len(rows))
		}
		cursors, targets := storedCursors(t, machine), storedTargets(t, machine)
		if len(cursors) != len(targets) {
			t.Fatalf("after %s there are %d targets and %d cursors", e.Type, len(targets), len(cursors))
		}
		for name, record := range targets {
			if err := record.toDomain().Validate(); err != nil || record.Refs < 0 {
				t.Fatalf("after %s the stored target %s cannot be kept: %v", e.Type, name, err)
			}
			if _, there := cursors[name]; !there {
				t.Fatalf("after %s target %s has no cursor", e.Type, name)
			}
		}
		if len(targets) > delivery.MaxTargets {
			t.Fatalf("after %s there are %d targets", e.Type, len(targets))
		}
		// Which buckets name a target is what their documents say, the count
		// of every target is of those, and every target admits them.
		checkReferences(t, machine)
		// An acknowledgement answers how many of its results were not its
		// target's, and nothing else answers so.
		if r.unrelated < 0 || (r.unrelated > 0 && (e.AckOutbox == nil || r.unrelated > len(e.AckOutbox.Results) || r.err != nil)) {
			t.Fatalf("%s answers %d results that were not its target's", e.Type, r.unrelated)
		}
		// No entry of schema 5 makes a delete marker without an ID.
		if r.marker != nil && r.marker.VersionID == (object.ID{}).String() {
			t.Fatalf("a delete made a marker without an ID: %+v", r.marker)
		}
		for _, name := range []string{"photos", "copy"} {
			if storedBucket(t, machine, name) != nil {
				checkUsage(t, machine, name)
			}
		}
		// A copy that was taken is where its sequence puts it: "objects"
		// holds the newest version of a key exactly when it is no marker.
		if r.err == nil && (e.PutObject != nil || e.DeleteVersion != nil) {
			name := ""
			if e.PutObject != nil {
				name = e.PutObject.Bucket
			} else {
				name = e.DeleteVersion.Bucket
			}
			if storedBucket(t, machine, name) == nil {
				return
			}
			for key, versions := range versionsOf(t, machine, name) {
				for i := 1; i < len(versions); i++ {
					above, err := outranks(versions[i], versions[i-1])
					if err != nil || above {
						t.Fatalf("after %s the versions of %s/%s are out of their order at %d (%v)", e.Type, name, key, i, err)
					}
				}
			}
		}
	})
}
