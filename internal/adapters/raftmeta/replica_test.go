package raftmeta

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// Replica buckets in the state machine (ADR-0026; PLAN 8.0.9):
// bucket.replica.put, what a replica bucket takes and refuses, where the copy
// of a version of another deployment is put among the versions of its key,
// what it counts for, and the record of a resync.

// siteA stands for the deployment a replica bucket takes its versions from,
// and siteC for another.
const (
	siteA = "5d3c1f0a-site-a"
	siteC = "9b8e7a66-site-c"
)

func replicaModeEntry(bucketName, source string) entry {
	return entry{Type: cmdSetBucketReplica, SetBucketReplica: &bucketReplicaCmd{Bucket: bucketName, Source: source}}
}

// copied makes an entry the copy of what it makes at a deployment: at the
// sequence it has there, and as the null version of its key or not.
func copied(e entry, from string, seq uint64, null bool) entry {
	replica := &replicaRecord{From: from, Seq: seq, Null: null}
	switch {
	case e.PutObject != nil:
		e.PutObject.Replica = replica
	case e.DeleteVersion != nil:
		e.DeleteVersion.Replica = replica
	case e.CompleteUpload != nil:
		e.CompleteUpload.Replica = replica
	}
	return e
}

// replicaPut is the copy of a version of siteA, of so many bytes.
func replicaPut(bucketName, key string, size int64, serial int, seq uint64) entry {
	return copied(putEntry(bucketName, key, size, serial), siteA, seq, false)
}

// replicaMarker is the copy of a delete marker of siteA.
func replicaMarker(bucketName, key string, serial int, seq uint64) entry {
	return copied(deleteEntry(bucketName, key, "", serial), siteA, seq, false)
}

// listed returns the versions of a key as a listing has them - the current
// version first, then the others, newest first - each as "ref@seq", with a
// "!" before a delete marker and a "*" before the current version.
func listed(t *testing.T, f *fsm, bucketName, key string) string {
	t.Helper()
	var out []string
	if err := f.view(func(tx *bbolt.Tx) error {
		h, err := openHistory(tx, bucketName, false)
		if err != nil {
			return err
		}
		name := func(v *versionRecord) string {
			ref := refOf(*v)
			if len(ref) > 4 {
				ref = ref[:4]
			}
			mark := ""
			if v.DeleteMarker {
				mark = "!"
			}
			return fmt.Sprintf("%s%s@%d", mark, ref, v.Seq)
		}
		current, err := readVersion(h.current, key)
		if err != nil {
			return err
		}
		if current != nil {
			out = append(out, "*"+name(current))
		}
		return h.each(key, func(_ []byte, v *versionRecord) (bool, error) {
			out = append(out, name(v))
			return true, nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, " ")
}

// short is the beginning of the ID of serial, as listed writes a version.
func short(serial int) string {
	return serialID(serial).String()[:4]
}

// sizedShort is the beginning of the ID of the version sized makes.
func sizedShort(bucketName, key string, serial int) string {
	return sized(bucketName, key, 0, serial).VersionID[:4]
}

// TestABucketBecomesAReplicaOnlyEmptyAndCounted: bucket.replica.put answers
// in the order ADR-0026 gives - the bucket is not there; the source is no
// deployment ID; nothing changed when it is the source the bucket has; the
// bucket keeps no versions, or sends its own on; it is not counted yet; it
// holds a version. A refusal changes nothing.
func TestABucketBecomesAReplicaOnlyEmptyAndCounted(t *testing.T) {
	t.Parallel()
	m := quick5(t)
	old := func(e entry) {
		t.Helper()
		if r := m.at4(e); r.err != nil {
			t.Fatal(r.err)
		}
	}
	enable := func(name string) entry {
		return entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: name, Status: string(bucket.VersioningEnabled)}}
	}
	m.bucket5("dst", bucket.VersioningEnabled)
	m.bucket5("plain", bucket.Unversioned)
	m.bucket5("paused", bucket.VersioningSuspended)
	m.bucket5("sender", bucket.VersioningEnabled)
	m.bucket5("held", bucket.VersioningEnabled)
	m.bucket5("marked", bucket.VersioningEnabled)
	m.bucket5("recount", bucket.VersioningEnabled)
	m.bucket5("uploading", bucket.VersioningEnabled)
	m.ok5(targetEntry("site-b", delivery.KindS3))
	m.ok5(targetEntry("hook", delivery.KindWebhook))
	m.ok5(configEntry("sender", bucket.ConfigReplication, replicateDoc(t, "site-b", bucket.ReplicationRule{ID: "all", Enabled: true})))
	m.ok5(putEntry("held", "k", 1, 1))
	m.ok5(deleteEntry("marked", "k", "", 2))
	m.walk("recount", 1, true)
	// Buckets of before schema 5: without a record, and being counted.
	for _, name := range []string{"old", "oldplain", "counting"} {
		old(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: name, CreatedAt: m.at}})
	}
	old(enable("old"))
	old(enable("counting"))
	for i := range 3 {
		old(putEntry("counting", fmt.Sprintf("k%d", i), 1, 10+i))
	}
	m.walk("counting", 1, false)
	if usage, kept, _ := usageOf(t, m.f, "counting"); !kept || usage.Exact || !usage.Walking() {
		t.Fatalf("the bucket that is being counted: %+v", usage)
	}
	if usage, _, _ := usageOf(t, m.f, "recount"); !usage.Exact {
		t.Fatalf("the bucket that is being counted again: %+v", usage)
	}
	_, _, entries := uploadEntries("uploading", "big", 20, 100)
	for _, e := range entries {
		m.ok5(e)
	}

	refusals := []struct {
		name, bucket, source string
		want                 error
	}{
		{"a bucket that is not there", "gone", siteA, domain.ErrBucketNotFound},
		{"a bucket that is not there, and a source that is none", "gone", "site a", domain.ErrBucketNotFound},
		{"a source with a space", "dst", "site a", domain.ErrInvalidArgument},
		{"a source that is too long", "dst", strings.Repeat("a", object.MaxDeploymentIDLength+1), domain.ErrInvalidArgument},
		{"a source that is none, for a bucket that keeps no versions", "plain", "site/a", domain.ErrInvalidArgument},
		{"a bucket that keeps no versions", "plain", siteA, domain.ErrInvalidBucketState},
		{"a bucket whose versions are paused", "paused", siteA, domain.ErrInvalidBucketState},
		{"a bucket that sends its own versions on", "sender", siteA, domain.ErrInvalidBucketState},
		{"a bucket of before that keeps no versions", "oldplain", siteA, domain.ErrInvalidBucketState},
		{"a bucket that was never counted", "old", siteA, domain.ErrConflict},
		{"a bucket that is being counted", "counting", siteA, domain.ErrConflict},
		{"a bucket that holds a version", "held", siteA, domain.ErrBucketNotEmpty},
		{"a bucket that holds a delete marker", "marked", siteA, domain.ErrBucketNotEmpty},
	}
	for _, test := range refusals {
		m.refused5(test.name, replicaModeEntry(test.bucket, test.source), test.want)
		// The audit record of a change that was refused is the proposer's
		// to write, not the entry's.
		e := replicaModeEntry(test.bucket, test.source)
		e.SetBucketReplica.Audit = trailOf("admin:PutBucketReplicaMode", "arn:aws:s3:::"+test.bucket)
		m.refused5(test.name+", with its audit record", e, test.want)
	}
	// A bucket.replica.put in an entry that claims an older schema is no
	// entry: tried on a state machine of its own, which stops.
	if stopped := quick5(t); !stopped.raw(entrySchema4, replicaModeEntry("dst", siteA)).fatal {
		t.Errorf("a bucket.replica.put of schema 4 did not stop the state machine")
	}

	// It becomes one: the record says of whom, and nothing else changed.
	plainRecord := storedBucket(t, m.f, "dst")
	trail := auditCount(t, m.f)
	e := replicaModeEntry("dst", siteA)
	e.SetBucketReplica.Audit = trailOf("admin:PutBucketReplicaMode", "arn:aws:s3:::dst")
	r := m.ok5(e)
	if len(r.events) != 2 || r.events[0].Type != ports.EventBucketReplicaModeChanged || r.events[0].Data != (ports.BucketReplicaModeEvent{Bucket: "dst", Source: siteA}) {
		t.Errorf("bucket.replica.put tells %+v", r.events)
	}
	if got := auditCount(t, m.f); got != trail+1 {
		t.Errorf("%d audit records, there were %d", got, trail)
	}
	want := bytes.Replace(plainRecord, []byte("}"), []byte(`,"replica_from":"`+siteA+`"}`), 1)
	if got := storedBucket(t, m.f, "dst"); !bytes.Equal(got, want) || !bytes.HasSuffix(got, []byte(`,"replica_from":"`+siteA+`"}`)) {
		t.Errorf("the record of a replica bucket is %s, was %s", got, plainRecord)
	}
	// The source it has, again: nothing changed, nobody told.
	before := settled(t, m.f)
	if r := m.ok5(replicaModeEntry("dst", siteA)); len(r.events) != 0 {
		t.Errorf("naming the source a bucket has tells %+v", r.events)
	}
	if after := settled(t, m.f); !reflect.DeepEqual(after, before) {
		t.Errorf("naming the source a bucket has changed the state")
	}
	// While it holds nothing it can be given another source.
	m.ok5(replicaModeEntry("dst", siteC))
	m.ok5(replicaModeEntry("dst", siteA))
	// A recount under way, and uploads in progress, do not stand in the way:
	// the record is exact and says no version.
	m.ok5(replicaModeEntry("recount", siteA))
	m.ok5(replicaModeEntry("uploading", siteA))

	// Once it holds a version it cannot be given another source - but it
	// can be promoted, and then it is a bucket like any other.
	m.ok5(replicaPut("dst", "k", 5, 30, 300))
	m.refused5("another source for a replica bucket that holds a version", replicaModeEntry("dst", siteC), domain.ErrBucketNotEmpty)
	r = m.ok5(replicaModeEntry("dst", ""))
	if len(r.events) != 1 || r.events[0].Data != (ports.BucketReplicaModeEvent{Bucket: "dst"}) {
		t.Errorf("promoting tells %+v", r.events)
	}
	if got := storedBucket(t, m.f, "dst"); !bytes.Equal(got, plainRecord) {
		t.Errorf("the record of a promoted bucket is %s, was %s before it was a replica", got, plainRecord)
	}
	m.refused5("making a promoted bucket a replica again", replicaModeEntry("dst", siteA), domain.ErrBucketNotEmpty)
	// Promoting a bucket that is no replica changes nothing.
	before = settled(t, m.f)
	if r := m.ok5(replicaModeEntry("plain", "")); len(r.events) != 0 {
		t.Errorf("promoting a bucket that is no replica tells %+v", r.events)
	}
	if after := settled(t, m.f); !reflect.DeepEqual(after, before) {
		t.Errorf("promoting a bucket that is no replica changed the state")
	}
}

// replicaScene is a state with a replica bucket of siteA, "dst", that tells a
// webhook of everything; one created with Object Lock, "locked"; and a bucket
// that is no replica, "photos".
func replicaScene(t *testing.T) *machine {
	t.Helper()
	m := quick5(t)
	m.bucket5("dst", bucket.VersioningEnabled)
	m.bucket5("photos", bucket.VersioningEnabled)
	m.ok5(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "locked", CreatedAt: m.at, ObjectLock: true}})
	m.ok5(targetEntry("hook", delivery.KindWebhook))
	m.ok5(targetEntry("site-b", delivery.KindS3))
	m.ok5(configEntry("dst", bucket.ConfigNotification, notifyDoc(t, bucket.NotificationRule{ID: "all", Target: "hook"})))
	m.ok5(replicaModeEntry("dst", siteA))
	m.ok5(replicaModeEntry("locked", siteA))
	return m
}

// TestAReplicaBucketTakesTheVersionsOfItsSourceAndNoOthers: in a replica
// bucket every entry that would make a version or a delete marker is refused
// unless it says that what it makes is the copy of one of the bucket's
// source - whoever wrote the entry, and in whatever schema: the state
// decides. No other bucket takes a copy. A copy keeps its ID and its
// sequence, is never sent on, and is told to who asked to be told.
func TestAReplicaBucketTakesTheVersionsOfItsSourceAndNoOthers(t *testing.T) {
	t.Parallel()
	m := replicaScene(t)
	// An upload that was begun before the bucket became a replica.
	stale := quick5(t)
	stale.bucket5("dst", bucket.VersioningEnabled)
	begun, begunParts, entries := uploadEntries("dst", "begun", 1, 100)
	for _, e := range entries {
		stale.ok5(e)
	}
	stale.ok5(replicaModeEntry("dst", siteA))
	stale.refused5("completing an upload of before as a version of this deployment", completeEntry(begun, 2, begunParts...), domain.ErrInvalidBucketState)
	stale.refused5("completing an upload of before as a copy", copied(completeEntry(begun, 2, begunParts...), siteA, 5, false), domain.ErrInvalidBucketState)
	// Its parts are no versions: one more is taken, and it can be given up.
	stale.ok5(partEntry(begun, porttest.SamplePart(begun, 2, 50, 9)))
	stale.ok5(abortEntry(begun))

	ordinary, _, _ := uploadEntries("dst", "big", 3, 100)
	fromC, _, _ := uploadEntries("dst", "big", 4, 100)
	fromC.ReplicaFrom = siteC
	odd, _, _ := uploadEntries("dst", "big", 5, 100)
	odd.ReplicaFrom = "site a"
	intoPhotos, _, _ := uploadEntries("photos", "big", 6, 100)
	intoPhotos.ReplicaFrom = siteA
	suspend := entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "dst", Status: string(bucket.VersioningSuspended)}}
	refusals := []struct {
		name string
		e    entry
		want error
	}{
		{"a put", putEntry("dst", "k", 5, 10), domain.ErrInvalidBucketState},
		{"a delete that would leave a marker", deleteEntry("dst", "k", "", 11), domain.ErrInvalidBucketState},
		{"an upload", entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(ordinary)}, domain.ErrInvalidBucketState},
		{"the copy of a version of another deployment", copied(putEntry("dst", "k", 5, 12), siteC, 7, false), domain.ErrInvalidBucketState},
		{"the copy of a marker of another deployment", copied(deleteEntry("dst", "k", "", 13), siteC, 7, false), domain.ErrInvalidBucketState},
		{"an upload of another deployment", entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(fromC)}, domain.ErrInvalidBucketState},
		{"an upload of a source that is none", entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(odd)}, domain.ErrInvalidArgument},
		{"a copy at no sequence", replicaPut("dst", "k", 5, 14, 0), domain.ErrInvalidArgument},
		{"a copy beyond the last sequence a copy can have", replicaPut("dst", "k", 5, 15, object.MaxReplicaSeq+1), domain.ErrInvalidArgument},
		{"a copy at the end of the numbers", replicaPut("dst", "k", 5, 16, 1<<64-1), domain.ErrInvalidArgument},
		{"a copy of a source that is none", copied(putEntry("dst", "k", 5, 17), "site a", 7, false), domain.ErrInvalidArgument},
		{"a marker at no sequence", replicaMarker("dst", "k", 18, 0), domain.ErrInvalidArgument},
		{"a marker beyond the last sequence", replicaMarker("dst", "k", 19, object.MaxReplicaSeq+1), domain.ErrInvalidArgument},
		{"the copy of a delete that names a version", copied(deleteEntry("dst", "k", object.NullVersion, 20), siteA, 7, false), domain.ErrInvalidArgument},
		{"a copy into a bucket that is no replica", replicaPut("photos", "k", 5, 21, 7), domain.ErrInvalidBucketState},
		{"the copy of a marker into a bucket that is no replica", replicaMarker("photos", "k", 22, 7), domain.ErrInvalidBucketState},
		{"an upload of a source into a bucket that is no replica", entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(intoPhotos)}, domain.ErrInvalidBucketState},
		{"pausing the versions of a replica bucket", suspend, domain.ErrInvalidBucketState},
		{"a replication configuration for a replica bucket", configEntry("dst", bucket.ConfigReplication, replicateDoc(t, "site-b", bucket.ReplicationRule{ID: "r", Enabled: true})), domain.ErrInvalidBucketState},
	}
	for _, test := range refusals {
		m.refused5(test.name, test.e, test.want)
	}
	// Whoever wrote the entry, and in whatever schema: a proposer that
	// takes the cluster to be at level 4, or 2, is refused alike.
	for _, level := range []byte{entrySchema4, entrySchema3, entrySchemaTimed} {
		for name, e := range map[string]entry{
			"a put":     putEntry("dst", "k", 5, 30),
			"a delete":  deleteEntry("dst", "k", "", 31),
			"an upload": {Type: cmdCreateUpload, CreateUpload: uploadToRecord(ordinary)},
			"pausing":   suspend,
		} {
			held := heldTo(e, level)
			m.refused(fmt.Sprintf("%s written at level %d", name, level), max(writtenAt(held, versionOf(held), level), entrySchemaTimed), held, domain.ErrInvalidBucketState)
		}
		// And what an entry of an older schema says of a copy is not read:
		// it is an ordinary put, and refused as one.
		m.refused(fmt.Sprintf("a copy in an entry of schema %d", level), level, replicaPut("dst", "k", 5, 32, 7), domain.ErrInvalidBucketState)
	}
	if got := rawState(t, m.f, outboxBucket()); len(got) != 0 {
		t.Fatalf("refusals appended rows: %v", got)
	}

	// The copy of a version: under its ID, at the sequence of its source,
	// with the time it was given, marked as a replica - and told, not sent
	// on.
	at := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	e := replicaPut("dst", "k", 500, 40, 7000)
	e.PutObject.ModifiedAt, e.PutObject.CreatedBy = at, "user/replicator"
	// What an entry says of where the version stands is not taken.
	e.PutObject.Replication = string(object.ReplicationCompleted)
	r := m.ok5(e)
	if v := r.stored; v.VersionID != sized("dst", "k", 500, 40).VersionID || v.Seq != 7000 || !v.Versioned || v.Replication != "REPLICA" || !v.ModifiedAt.Equal(at) || v.Replica != nil {
		t.Fatalf("the copy is stored as %+v", v)
	}
	if strings.Contains(rawState(t, m.f, objectsBucket())[fmt.Sprintf("objects/dst/%x", "k")], `"replica":`) {
		t.Errorf("the stored version says more of its source than its sequence and its status")
	}
	if usage := checkUsage(t, m.f, "dst"); usage.Versions != 1 || usage.Objects != 1 || usage.StoredBytes != 500 || usage.High != 7000 || usage.ReplicationPending != 0 {
		t.Errorf("the usage after one copy: %+v", usage)
	}
	if rows := storedRows(t, m.f); told(rows) != "put k>n:hook" || rows[0].By != "user/replicator" {
		t.Errorf("the rows after one copy: %s", told(rows))
	}
	// A second arrival is refused, and the source reads that as done.
	again := replicaPut("dst", "k", 500, 40, 7000)
	again.PutObject.DataID = serialID(900).String()
	m.refused5("a copy that arrives a second time", again, domain.ErrConflict)
	elsewhere := replicaPut("dst", "k", 500, 40, 9000)
	elsewhere.PutObject.DataID = serialID(901).String()
	m.refused5("the same version at another sequence", elsewhere, domain.ErrConflict)

	// The copy of a marker, and of the null version of a key.
	r = m.ok5(replicaMarker("dst", "k", 41, 7100))
	if mk := r.marker; mk.VersionID != serialID(41).String() || mk.Seq != 7100 || !mk.Versioned || mk.Replication != "REPLICA" || !mk.DeleteMarker {
		t.Fatalf("the copy of a marker is stored as %+v", mk)
	}
	m.refused5("a marker that arrives a second time", replicaMarker("dst", "k", 41, 7100), domain.ErrConflict)
	r = m.ok5(copied(putEntry("dst", "k", 30, 42), siteA, 50, true))
	if v := r.stored; v.Versioned || v.Seq != 50 || v.Replication != "REPLICA" || refOf(*v) != object.NullVersion {
		t.Fatalf("the copy of a null version is stored as %+v", v)
	}
	m.refused5("a second null version of the key", copied(putEntry("dst", "k", 31, 43), siteA, 60, true), domain.ErrConflict)
	m.refused5("a null marker where the null version is", copied(deleteEntry("dst", "k", "", 44), siteA, 61, true), domain.ErrConflict)
	if got, want := listed(t, m.f, "dst", "k"), fmt.Sprintf("!%s@7100 %s@7000 null@50", short(41), sizedShort("dst", "k", 40)); got != want {
		t.Fatalf("the versions of the key: %s, want %s", got, want)
	}
	if rows := storedRows(t, m.f); told(rows) != "put k>n:hook; marker k>n:hook; put k>n:hook" {
		t.Errorf("the rows: %s", told(rows))
	}
	checkUsage(t, m.f, "dst")

	// A version of parts: begun as an upload of the source, completed as a
	// copy.
	upload, parts, entries := uploadEntries("dst", "big", 50, object.MinPartSize, 7)
	upload.ReplicaFrom = siteA
	entries[0] = entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(upload)}
	for _, e := range entries {
		m.ok5(e)
	}
	m.refused5("completing an upload of the source as a version of this deployment", completeEntry(upload, 51, parts...), domain.ErrInvalidBucketState)
	m.refused5("completing it as the copy of another deployment's", copied(completeEntry(upload, 51, parts...), siteC, 8000, false), domain.ErrInvalidBucketState)
	m.refused5("completing it at no sequence", copied(completeEntry(upload, 51, parts...), siteA, 0, false), domain.ErrInvalidArgument)
	r = m.ok5(copied(completeEntry(upload, 51, parts...), siteA, 8000, false))
	if v := r.stored; v.VersionID != serialID(51).String() || v.Seq != 8000 || v.Replication != "REPLICA" || v.Parts != 2 || v.Size != object.MinPartSize+7 {
		t.Fatalf("the copy of a version of parts is stored as %+v", v)
	}
	if rows := storedRows(t, m.f); rows[len(rows)-1].Event != delivery.EventComplete || rows[len(rows)-1].Key != "big" {
		t.Errorf("the row of a completed copy: %+v", rows[len(rows)-1])
	}
	usage := checkUsage(t, m.f, "dst")
	if usage.UploadBytes != 0 || usage.High != 8000 || usage.Versions != 4 {
		t.Errorf("the usage: %+v", usage)
	}

	// The status of a copy never changes, and a later change of it is not
	// sent anywhere.
	ref, _ := object.ParseVersionRef(sized("dst", "k", 500, 40).VersionID)
	m.ok5(ackEntry("site-b", delivery.Seq{}, delivery.Result{Bucket: "dst", Key: "k", Version: ref, Status: object.ReplicationFailed}))
	rows := len(storedRows(t, m.f))
	m.ok5(entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: "dst", Key: "k", VersionRef: ref.String(), Tags: map[string]string{"a": "b"}}})
	if got := statuses(t, m.f, "dst")["k@"+ref.String()]; got != "REPLICA" || len(storedRows(t, m.f)) != rows {
		t.Errorf("a copy after a result and a change of its tags: status %s, %d rows more", got, len(storedRows(t, m.f))-rows)
	}
	if usage := checkUsage(t, m.f, "dst"); usage.ReplicationFailed != 0 || usage.ReplicationPending != 0 {
		t.Errorf("the counts of a replica bucket: %+v", usage.UsageTotals)
	}

	// The quota of the destination is held to: that is what it is for.
	m.ok5(entry{Type: cmdSetBucketQuota, SetBucketQuota: &bucketQuotaCmd{Bucket: "dst", Bytes: usage.Used() + 10}})
	m.refused5("a copy that would take the bucket past its quota", replicaPut("dst", "full", 11, 60, 9000), domain.ErrQuotaExceeded)
	m.refused5("an older copy that would", replicaPut("dst", "k", 11, 61, 10), domain.ErrQuotaExceeded)
	m.ok5(replicaPut("dst", "k", 10, 62, 10))
	checkUsage(t, m.f, "dst")

	// Removing a version for good is judged as everywhere: allowed here,
	// told, and kept from by a lock.
	m.ok5(deleteEntry("dst", "k", ref.String(), 70))
	if rows := storedRows(t, m.f); told(rows[len(rows)-1:]) != "delete k>n:hook" {
		t.Errorf("the removal of a copy: %s", told(rows[len(rows)-1:]))
	}
	checkUsage(t, m.f, "dst")
	kept := replicaPut("locked", "k", 5, 80, 100)
	kept.PutObject.Lock = &lockRecord{Mode: string(object.Compliance), RetainUntil: m.at.Add(24 * time.Hour)}
	r = m.ok5(kept)
	for name, e := range map[string]entry{
		"removing a copy under compliance retention": deleteEntry("locked", "k", r.stored.VersionID, 81),
		"removing it with the word that lifts governance": func() entry {
			e := deleteEntry("locked", "k", r.stored.VersionID, 82)
			e.DeleteVersion.Bypass = true
			return e
		}(),
	} {
		m.refused5(name, e, domain.ErrObjectLocked)
	}
}

// TestACopyIsWhereItsSequencePutsIt: a copy is stored at the sequence it has
// at its source and is the current version of its key exactly when no version
// of the key outranks it - so a copy that arrives late does not become
// current over a newer one, a marker hides only what is older than it, and
// what the key counts for among the objects of the bucket is right after
// every arrival.
func TestACopyIsWhereItsSequencePutsIt(t *testing.T) {
	t.Parallel()
	m := quick5(t)
	m.bucket5("dst", bucket.VersioningEnabled)
	m.ok5(replicaModeEntry("dst", siteA))
	id := func(serial int) string { return sizedShort("dst", "k", serial) }
	steps := []struct {
		name    string
		e       entry
		listed  string
		objects int64
		current int64
		high    uint64
	}{
		{"the first copy", replicaPut("dst", "k", 300, 1, 300), "*" + id(1) + "@300", 1, 300, 300},
		{"an older one, late", replicaPut("dst", "k", 100, 2, 100), "*" + id(1) + "@300 " + id(2) + "@100", 1, 300, 300},
		{"an older marker, late", replicaMarker("dst", "k", 3, 200), "*" + id(1) + "@300 !" + short(3) + "@200 " + id(2) + "@100", 1, 300, 300},
		{"a newer one", replicaPut("dst", "k", 400, 4, 400), "*" + id(4) + "@400 " + id(1) + "@300 !" + short(3) + "@200 " + id(2) + "@100", 1, 400, 400},
		{"a marker that is the newest", replicaMarker("dst", "k", 5, 500), "!" + short(5) + "@500 " + id(4) + "@400 " + id(1) + "@300 !" + short(3) + "@200 " + id(2) + "@100", 0, 0, 500},
		{"a version under the marker", replicaPut("dst", "k", 450, 6, 450), "!" + short(5) + "@500 " + id(6) + "@450 " + id(4) + "@400 " + id(1) + "@300 !" + short(3) + "@200 " + id(2) + "@100", 0, 0, 500},
		{"the null version, the oldest", copied(putEntry("dst", "k", 50, 7), siteA, 50, true), "!" + short(5) + "@500 " + id(6) + "@450 " + id(4) + "@400 " + id(1) + "@300 !" + short(3) + "@200 " + id(2) + "@100 null@50", 0, 0, 500},
		{"a version over the marker", replicaPut("dst", "k", 600, 8, 600), "*" + id(8) + "@600 !" + short(5) + "@500 " + id(6) + "@450 " + id(4) + "@400 " + id(1) + "@300 !" + short(3) + "@200 " + id(2) + "@100 null@50", 1, 600, 600},
		{"at the last sequence a copy can have", replicaPut("dst", "k", 9, 9, object.MaxReplicaSeq), "*" + id(9) + fmt.Sprintf("@%d ", uint64(object.MaxReplicaSeq)) + id(8) + "@600 !" + short(5) + "@500 " + id(6) + "@450 " + id(4) + "@400 " + id(1) + "@300 !" + short(3) + "@200 " + id(2) + "@100 null@50", 1, 9, object.MaxReplicaSeq},
	}
	var versions, stored int64
	for _, step := range steps {
		r := m.ok5(step.e)
		versions++
		if r.stored != nil {
			stored += r.stored.Size
		}
		if got := listed(t, m.f, "dst", "k"); got != step.listed {
			t.Fatalf("%s: the versions of the key are\n%s\nwant\n%s", step.name, got, step.listed)
		}
		usage := checkUsage(t, m.f, "dst")
		want := bucket.UsageTotals{Objects: step.objects, CurrentBytes: step.current, Versions: versions, StoredBytes: stored}
		if usage.UsageTotals != want || usage.High != step.high || !usage.Exact {
			t.Fatalf("%s: the usage is %+v, greatest sequence %d; want %+v and %d", step.name, usage.UsageTotals, usage.High, want, step.high)
		}
	}
	// "objects" holds the newest version of a key exactly when it is no
	// marker, and nothing of a key twice.
	if all := everyVersion(t, m.f, "dst"); len(all) != len(steps) {
		t.Errorf("%d versions stored, %d arrived", len(all), len(steps))
	}

	// Promoted: what is written here now is the newest of its key, above
	// every sequence the source ever had.
	m.ok5(replicaModeEntry("dst", ""))
	r := m.ok5(putEntry("dst", "k", 77, 20))
	if r.stored.Seq != object.MaxReplicaSeq+1 || r.stored.Replication != "" {
		t.Fatalf("a put after promotion is stored at %d as %q", r.stored.Seq, r.stored.Replication)
	}
	if got := listed(t, m.f, "dst", "k"); !strings.HasPrefix(got, fmt.Sprintf("*%s@%d %s@%d ", id(20), uint64(object.MaxReplicaSeq+1), id(9), uint64(object.MaxReplicaSeq))) {
		t.Fatalf("after a put into the promoted bucket: %s", got)
	}
	r = m.ok5(deleteEntry("dst", "k", "", 21))
	if r.marker.Seq != object.MaxReplicaSeq+2 {
		t.Errorf("a marker after promotion is at %d", r.marker.Seq)
	}
	if got := listed(t, m.f, "dst", "k"); !strings.HasPrefix(got, fmt.Sprintf("!%s@%d %s@%d ", short(21), uint64(object.MaxReplicaSeq+2), id(20), uint64(object.MaxReplicaSeq+1))) {
		t.Fatalf("after a delete in the promoted bucket: %s", got)
	}
	// The promoted bucket may send its versions on, like any other: what
	// is written now is sent, and what arrived as a copy never is - not its
	// later changes either.
	m.ok5(targetEntry("site-b", delivery.KindS3))
	m.ok5(configEntry("dst", bucket.ConfigReplication, replicateDoc(t, "site-b", bucket.ReplicationRule{ID: "all", Enabled: true, Markers: true})))
	m.ok5(entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: "dst", Key: "k", VersionRef: sized("dst", "k", 0, 9).VersionID, Tags: map[string]string{"a": "b"}}})
	m.ok5(entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: "dst", Key: "k", VersionRef: sized("dst", "k", 0, 20).VersionID, Tags: map[string]string{"a": "b"}}})
	if rows := storedRows(t, m.f); len(rows) != 0 {
		t.Errorf("a copy, and a version of before the configuration, were sent on: %s", told(rows))
	}
	if r := m.ok5(putEntry("dst", "k", 3, 23)); r.stored.Replication != "PENDING" || told(storedRows(t, m.f)) != "put k>r:site-b" {
		t.Errorf("a put into a promoted bucket that sends its versions on: %q, rows %s", r.stored.Replication, told(storedRows(t, m.f)))
	}
	if got := statuses(t, m.f, "dst")["k@"+sized("dst", "k", 0, 9).VersionID]; got != "REPLICA" {
		t.Errorf("the copy is now %q", got)
	}
	m.ok5(configEntry("dst", bucket.ConfigReplication, nil))
	// A key the source never wrote begins above its sequences too.
	if r := m.ok5(putEntry("dst", "new", 1, 22)); r.stored.Seq != object.MaxReplicaSeq+4 {
		t.Errorf("a put of a new key after promotion is at %d", r.stored.Seq)
	}
	usage := checkUsage(t, m.f, "dst")
	if usage.High != object.MaxReplicaSeq+4 || usage.Objects != 2 || usage.ReplicationPending != 1 {
		t.Errorf("the usage of the promoted bucket: %+v", usage)
	}
}

// replicaSet is a set of copies as a source sends them of four keys:
// versions, markers, the null version of some keys, and two versions that
// the source - at fault - has at one sequence. No two are of one version.
// Among them are copies of markers whose ID is all zeros, which no source
// sends and every arrival of which is refused: at the sequence of the null
// version of the key, where one would share its place, and at others.
func replicaSet(rng *dice) []entry {
	var set []entry
	serial := 0
	for _, key := range []string{"a", "a/b", "zoë", "k \"q\""} {
		seqs := rng.perm(40)
		for i := range 9 + rng.roll(6) {
			serial++
			seq := number(seqs[i]+1) * 10
			switch n := rng.roll(10); {
			case n < 6:
				set = append(set, replicaPut("dst", key, int64(rng.roll(900)), serial, seq))
			case n < 9:
				set = append(set, replicaMarker("dst", key, serial, seq))
			default:
				// Two versions at one sequence.
				set = append(set, replicaPut("dst", key, int64(rng.roll(900)), serial, seq))
				serial++
				set = append(set, replicaPut("dst", key, int64(rng.roll(900)), serial, seq))
			}
		}
		null := number(seqs[39]+1)*10 + number(rng.roll(5))
		if rng.roll(3) > 0 {
			serial++
			e := putEntry("dst", key, int64(rng.roll(900)), serial)
			if rng.roll(2) == 0 {
				e = deleteEntry("dst", key, "", serial)
			}
			set = append(set, copied(e, siteA, null, true))
		}
		set = append(set, zeroMarker("dst", key, null, false), zeroMarker("dst", key, number(seqs[rng.roll(40)]+1)*10, rng.roll(2) == 0))
	}
	return set
}

// withoutID reports whether an entry is the copy of a marker whose ID is all
// zeros.
func withoutID(e entry) bool {
	return e.DeleteVersion != nil && e.DeleteVersion.MarkerID == object.ID{}.String()
}

// TestAnyOrderOfArrivalGivesTheSameVersions: one set of copies, applied in
// any order and with any of them arriving again and again, gives the same
// "objects", "versions" and "versionids", byte for byte, the same index of
// data and the same usage record - and after promotion a put is the newest
// version of its key on every one of them. The copies of markers without an
// ID that are among them are refused wherever they arrive, and change
// nothing.
func TestAnyOrderOfArrivalGivesTheSameVersions(t *testing.T) {
	t.Parallel()
	for _, seed := range []int{1, 2, 3, 2026} {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()
			rng := newDice(seed)
			set := replicaSet(rng)
			compared := [][]byte{objectsBucket(), versionsBucket(), versionIDsBucket(), dataBucket(), bucketUsageBucket()}
			var want map[string]string
			var wantUsage bucket.Usage
			for order := range 6 {
				m := quick5(t)
				m.bucket5("dst", bucket.VersioningEnabled)
				m.ok5(replicaModeEntry("dst", siteA))
				arrivals := make([]entry, 0, 2*len(set))
				for _, i := range rng.perm(len(set)) {
					arrivals = append(arrivals, set[i])
				}
				if order == 0 {
					arrivals = slices.Clone(set)
				}
				// Some arrive again, anywhere after their first arrival.
				for i := 0; i < len(arrivals) && order > 0; i += 3 {
					at := i + 1 + rng.roll(len(arrivals)-i)
					arrivals = slices.Insert(arrivals, at, arrivals[i])
				}
				seen := map[string]bool{}
				for n, e := range arrivals {
					name := ""
					if e.PutObject != nil {
						name = e.PutObject.VersionID
					} else {
						name = e.DeleteVersion.MarkerID
					}
					// An entry is applied as it was written: the copy is the
					// test's to keep.
					data, err := json.Marshal(e)
					if err != nil {
						t.Fatal(err)
					}
					var again entry
					if err := json.Unmarshal(data, &again); err != nil {
						t.Fatal(err)
					}
					switch {
					case withoutID(again):
						m.refused5(fmt.Sprintf("arrival %d, a marker without an ID", n), again, domain.ErrInvalidArgument)
					case !seen[name]:
						seen[name] = true
						m.ok5(again)
					default:
						m.refused5(fmt.Sprintf("arrival %d, a second one", n), again, domain.ErrConflict)
					}
					if n%7 == 0 {
						checkUsage(t, m.f, "dst")
					}
				}
				usage := checkUsage(t, m.f, "dst")
				got := rawState(t, m.f, compared...)
				if order == 0 {
					want, wantUsage = got, usage
					taken := 0
					for _, e := range set {
						if !withoutID(e) {
							taken++
						}
					}
					if usage.Versions != int64(taken) || taken > len(set)-8 || !usage.Exact || usage.Walking() {
						t.Fatalf("the usage after %d copies of which %d could be taken: %+v", len(set), taken, usage)
					}
					continue
				}
				if !reflect.DeepEqual(got, want) {
					for key, value := range want {
						if got[key] != value {
							t.Errorf("order %d: %s is %s, in the order of the source %s", order, key, got[key], value)
						}
					}
					t.Fatalf("order %d of arrival gives another state than the order of the source", order)
				}
				if !reflect.DeepEqual(usage, wantUsage) {
					t.Fatalf("order %d: the usage is %+v, in the order of the source %+v", order, usage, wantUsage)
				}
				// Promoted, a put and a delete are the newest of their keys.
				m.ok5(replicaModeEntry("dst", ""))
				for i, key := range []string{"a", "a/b", "zoë", "k \"q\"", "new"} {
					r := m.ok5(putEntry("dst", key, 5, 5000+i))
					if got := listed(t, m.f, "dst", key); !strings.HasPrefix(got, fmt.Sprintf("*%s@%d", r.stored.VersionID[:4], r.stored.Seq)) || r.stored.Seq <= wantUsage.High {
						t.Fatalf("order %d: after a put into the promoted bucket, %s: %s (the source had reached %d)", order, key, got, wantUsage.High)
					}
				}
				r := m.ok5(deleteEntry("dst", "a", "", 6000))
				if got := listed(t, m.f, "dst", "a"); !strings.HasPrefix(got, fmt.Sprintf("!%s@%d", r.marker.VersionID[:4], r.marker.Seq)) {
					t.Fatalf("order %d: after a delete in the promoted bucket: %s", order, got)
				}
				checkUsage(t, m.f, "dst")
			}
		})
	}
}

// TestTheRecordOfAResyncIsKept: replication.resync.put stores how far the
// walk has come, in place of what was stored, as the payload with its
// version; a record that says the walk is done clears the mark of its target
// and of no other; and the record goes with its bucket.
func TestTheRecordOfAResyncIsKept(t *testing.T) {
	t.Parallel()
	m := quick5(t)
	m.bucket5("photos", bucket.VersioningEnabled)
	m.ok5(targetEntry("site-b", delivery.KindS3))
	m.ok5(targetEntry("site-c", delivery.KindS3))
	resync := func(r bucket.Resync) entry { return entry{Type: cmdPutResync, PutResync: resyncToRecord(r)} }
	stored := func() (bucket.Resync, bool) {
		t.Helper()
		var r bucket.Resync
		var kept bool
		if err := m.f.view(func(tx *bbolt.Tx) (err error) {
			r, kept, err = readResync(tx, "photos")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return r, kept
	}
	running := bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning}
	for name, test := range map[string]struct {
		edit func(*bucket.Resync)
		want error
	}{
		"a state there is not":              {func(r *bucket.Resync) { r.State = "paused" }, domain.ErrInvalidArgument},
		"no state":                          {func(r *bucket.Resync) { r.State = "" }, domain.ErrInvalidArgument},
		"more sent than looked at":          {func(r *bucket.Resync) { r.Scanned, r.Sent = 1, 2 }, domain.ErrInvalidArgument},
		"a key without a version":           {func(r *bucket.Resync) { r.Key = "k" }, domain.ErrInvalidArgument},
		"a version without a key":           {func(r *bucket.Resync) { r.Version = "null" }, domain.ErrInvalidArgument},
		"a version that is none":            {func(r *bucket.Resync) { r.Key, r.Version = "k", "latest" }, domain.ErrInvalidArgument},
		"a target that has no name":         {func(r *bucket.Resync) { r.Target = "" }, domain.ErrInvalidArgument},
		"a target named as no target is":    {func(r *bucket.Resync) { r.Target = "Site_B" }, domain.ErrInvalidArgument},
		"a bucket named as no bucket is":    {func(r *bucket.Resync) { r.Bucket = "P" }, domain.ErrInvalidArgument},
		"a bucket that is not there":        {func(r *bucket.Resync) { r.Bucket = "gone" }, domain.ErrBucketNotFound},
		"a target that is not there":        {func(r *bucket.Resync) { r.Target = "site-d" }, domain.ErrEntityNotFound},
		"neither the bucket nor the target": {func(r *bucket.Resync) { r.Bucket, r.Target = "gone", "site-d" }, domain.ErrBucketNotFound},
	} {
		r := running
		test.edit(&r)
		m.refused5(name, resync(r), test.want)
	}
	if got := rawState(t, m.f, resyncBucket()); len(got) != 0 {
		t.Fatalf("refusals left %v", got)
	}
	if _, kept := stored(); kept {
		t.Fatal("a bucket that was never resynced has a record")
	}

	if r := m.ok5(resync(running)); len(r.events) != 0 {
		t.Errorf("a record of a resync tells %+v", r.events)
	}
	raw := rawState(t, m.f, resyncBucket())[fmt.Sprintf("resync/%x", "photos")]
	if want := `{"v":1,"bucket":"photos","target":"site-b","state":"running","key":"","version":"","scanned":0,"sent":0}`; raw != want {
		t.Errorf("the stored record before the first version is\n%s\nwant\n%s", raw, want)
	}
	next := bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning, Key: "a/b \"c\"", Version: serialID(7).String(), Scanned: 500, Sent: 12}
	m.ok5(resync(next))
	if got, kept := stored(); !kept || got != next {
		t.Errorf("the stored record = %+v, want %+v", got, next)
	}
	raw = rawState(t, m.f, resyncBucket())[fmt.Sprintf("resync/%x", "photos")]
	if want := fmt.Sprintf(`{"v":1,"bucket":"photos","target":"site-b","state":"running","key":"a/b \"c\"","version":%q,"scanned":500,"sent":12}`, serialID(7).String()); raw != want {
		t.Errorf("the stored record is\n%s\nwant\n%s", raw, want)
	}

	// Both targets lost rows of replication. A walk that goes on clears
	// nothing; one that is done clears the mark of its target, and leaves
	// what was lost counted.
	mark := func(name string) {
		t.Helper()
		m.f.mu.Lock()
		defer m.f.mu.Unlock()
		if err := m.f.db.Update(func(tx *bbolt.Tx) error {
			cursor, _, err := readCursor(tx, name)
			if err != nil {
				return err
			}
			cursor.Lost, cursor.ResyncNeeded = 3, true
			return writeCursor(tx, cursor)
		}); err != nil {
			t.Fatal(err)
		}
	}
	mark("site-b")
	mark("site-c")
	m.ok5(resync(next))
	if cursors := storedCursors(t, m.f); !cursors["site-b"].ResyncNeeded || !cursors["site-c"].ResyncNeeded {
		t.Fatalf("a resync that goes on cleared a mark: %+v", cursors)
	}
	done := next
	done.State, done.Scanned = bucket.ResyncDone, 1200
	// A walk that is done clears the mark of a target its bucket is sent
	// to, and of no other: not while the bucket has no replication
	// configuration, not for a target the configuration does not name, and
	// not by a bucket that is sent elsewhere. The record is stored each time.
	m.bucket5("other", bucket.VersioningEnabled)
	m.ok5(configEntry("other", bucket.ConfigReplication, replicateDoc(t, "site-c", bucket.ReplicationRule{ID: "all", Enabled: true})))
	elsewhere := bucket.Resync{Bucket: "other", Target: "site-b", State: bucket.ResyncDone}
	for name, r := range map[string]bucket.Resync{"a bucket without a replication configuration": done, "a bucket that is sent to another target": elsewhere} {
		m.ok5(resync(r))
		if cursors := storedCursors(t, m.f); !cursors["site-b"].ResyncNeeded || !cursors["site-c"].ResyncNeeded {
			t.Fatalf("%s, done, cleared a mark: %+v", name, cursors)
		}
	}
	if got, _ := stored(); got != done {
		t.Errorf("the record of a resync that cleared nothing = %+v, want %+v", got, done)
	}
	m.ok5(configEntry("photos", bucket.ConfigReplication, replicateDoc(t, "site-b", bucket.ReplicationRule{ID: "all", Enabled: true})))
	other := done
	other.Target = "site-c"
	m.ok5(resync(other))
	if cursors := storedCursors(t, m.f); !cursors["site-b"].ResyncNeeded || !cursors["site-c"].ResyncNeeded {
		t.Fatalf("a resync that is done for a target the configuration does not name cleared a mark: %+v", cursors)
	}
	m.ok5(resync(done))
	cursors := storedCursors(t, m.f)
	if cursors["site-b"].ResyncNeeded || cursors["site-b"].Lost != 3 || !cursors["site-c"].ResyncNeeded {
		t.Errorf("after a resync that is done: %+v", cursors)
	}
	if got, _ := stored(); got != done {
		t.Errorf("the stored record = %+v, want %+v", got, done)
	}
	// Done again: nothing but the record is written.
	before := settled(t, m.f)
	m.ok5(resync(done))
	if after := settled(t, m.f); !reflect.DeepEqual(after, before) {
		t.Errorf("the same record again changed the state")
	}

	// The mark is of the target: a bucket that is done clears it for every
	// bucket that is sent there (ADR-0026, "Resync").
	m.ok5(configEntry("other", bucket.ConfigReplication, replicateDoc(t, "site-b", bucket.ReplicationRule{ID: "all", Enabled: true})))
	mark("site-b")
	m.ok5(resync(elsewhere))
	if cursors := storedCursors(t, m.f); cursors["site-b"].ResyncNeeded {
		t.Errorf("a resync that is done of one bucket of a target left its mark: %+v", cursors["site-b"])
	}

	// The record goes with the bucket; one made under the name later has
	// none.
	m.ok5(entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "photos"}})
	if got := rawStateOf(rawState(t, m.f, resyncBucket()), resyncBucket()); len(got) != 1 || got[fmt.Sprintf("resync/%x", "other")] == "" {
		t.Errorf("the record of a resync outlived its bucket, or another went with it: %v", got)
	}
	m.bucket5("photos", bucket.VersioningEnabled)
	if _, kept := stored(); kept {
		t.Error("a bucket made under the name of one that was resynced has its record")
	}
	// A record that cannot be read stops the state machine when it is read.
	for name, test := range map[string]struct {
		raw  string
		want error
	}{
		"no JSON":         {"{", domain.ErrIntegrity},
		"another version": {`{"v":2,"bucket":"photos"}`, domain.ErrUnsupportedFormat},
	} {
		m.ok5(resync(running))
		m.f.mu.Lock()
		err := m.f.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(resyncBucket()).Put([]byte("photos"), []byte(test.raw)) })
		m.f.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if err := m.f.view(func(tx *bbolt.Tx) error {
			_, _, err := readResync(tx, "photos")
			return err
		}); !errors.Is(err, test.want) {
			t.Errorf("a stored record that is %s is read as %v, want %v", name, err, test.want)
		}
	}
}
