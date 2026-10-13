package raftmeta

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// Rules of the state machine of schema 5 that were ruled after its first
// review (PLAN, Decisions, 2026-10-13): a delete marker has an ID; a result
// by version is of a bucket that is sent to the target; a target goes on
// admitting the buckets whose documents name it; how many targets there are,
// and how many a document names; and that what is refused leaves nothing.

// zeroMarker is the copy of a delete marker of siteA whose ID is all zeros.
func zeroMarker(bucketName, key string, seq uint64, null bool) entry {
	e := deleteEntry(bucketName, key, "", 1)
	e.DeleteVersion.MarkerID = object.ID{}.String()
	return copied(e, siteA, seq, null)
}

// TestAMarkerWithoutAnIDIsNoCopy: the copy of a delete marker whose ID is all
// zeros is refused, whatever arrived before it and wherever its sequence
// would put it - it would share its place in "versions" with the null version
// of its key, and what the key holds would depend on which of the two came
// first. So the null version (n), the marker (m) and a newer version (h)
// leave the same bytes and the same usage in every order they arrive in, with
// the marker below the null version and at its very sequence.
func TestAMarkerWithoutAnIDIsNoCopy(t *testing.T) {
	t.Parallel()
	compared := [][]byte{objectsBucket(), versionsBucket(), versionIDsBucket(), bucketUsageBucket()}
	for _, seq := range []uint64{50, 100} {
		for _, null := range []bool{false, true} {
			var first map[string]string
			for _, order := range []string{"nmh", "nhm", "mnh", "hmn", "mhn"} {
				t.Run(fmt.Sprintf("m@%d null %t %s", seq, null, order), func(t *testing.T) {
					m := quick5(t)
					m.bucket5("dst", bucket.VersioningEnabled)
					m.ok5(replicaModeEntry("dst", siteA))
					for _, arrives := range order {
						switch arrives {
						case 'n':
							m.ok5(copied(putEntry("dst", "k", 5, 1), siteA, 100, true))
						case 'h':
							m.ok5(replicaPut("dst", "k", 7, 3, 200))
						case 'm':
							m.refused5("the copy of a marker without an ID", zeroMarker("dst", "k", seq, null), domain.ErrInvalidArgument)
						}
					}
					usage := checkUsage(t, m.f, "dst")
					if usage.Versions != 2 || usage.Objects != 1 || usage.StoredBytes != 12 || usage.CurrentBytes != 7 {
						t.Errorf("the bucket counts %+v, want the two versions that were taken", usage.UsageTotals)
					}
					if got := listed(t, m.f, "dst", "k"); got != "*"+sizedShort("dst", "k", 3)+"@200 null@100" {
						t.Errorf("the key holds %s", got)
					}
					state := rawState(t, m.f, compared...)
					if first == nil {
						first = state
					} else if !reflect.DeepEqual(state, first) {
						t.Errorf("in the order %s the state is another than in the first order", order)
					}
				})
			}
		}
	}

	// Into a bucket that holds nothing, too - and there it leaves nothing:
	// not the place of the bucket among the versions either.
	m := quick5(t)
	m.bucket5("dst", bucket.VersioningEnabled)
	m.ok5(replicaModeEntry("dst", siteA))
	m.refused5("the copy of a marker without an ID, into an empty bucket", zeroMarker("dst", "k", 9, false), domain.ErrInvalidArgument)
	// A copy with an ID is taken as it always was.
	if r := m.ok5(replicaMarker("dst", "k", 2, 9)); r.marker == nil || r.marker.Replication != string(object.ReplicationReplica) {
		t.Errorf("the copy of a marker with an ID: %+v", r.marker)
	}
}

// TestAMarkerWaitsExactlyWhenItsRowIsAppended: below schema 5 a delete whose
// marker has an ID of all zeros is applied as the release of schema 4 applies
// it. In a bucket that is replicated such a marker cannot be told of, so it is
// not stored as waiting to be sent on either: marking a version PENDING and
// appending the row that sends it on are one decision.
func TestAMarkerWaitsExactlyWhenItsRowIsAppended(t *testing.T) {
	t.Parallel()
	for _, schema := range []byte{entrySchemaTimed, entrySchema3, entrySchema4} {
		m := rowsScene(t)
		rows := len(storedRows(t, m.f))
		e := deleteEntry("photos", "k", "", 1)
		e.DeleteVersion.MarkerID = object.ID{}.String()
		r := m.as(schema, e)
		if r.err != nil || r.marker == nil {
			t.Fatalf("schema %d: the delete = %+v, %v", schema, r.marker, r.err)
		}
		usage := checkUsage(t, m.f, "photos")
		if r.marker.Replication != "" || usage.ReplicationPending != 0 || usage.Versions != 1 || len(storedRows(t, m.f)) != rows {
			t.Errorf("schema %d: the marker is %q, the bucket counts %d waiting, and %d rows were appended",
				schema, r.marker.Replication, usage.ReplicationPending, len(storedRows(t, m.f))-rows)
		}
		// One with an ID, written as the same schema, waits and has its row.
		r = m.as(schema, deleteEntry("photos", "k2", "", 2))
		usage = checkUsage(t, m.f, "photos")
		if r.err != nil || r.marker.Replication != string(object.ReplicationPending) || usage.ReplicationPending != 1 || len(storedRows(t, m.f)) != rows+1 {
			t.Errorf("schema %d: a marker with an ID is %q, %d waiting, %d rows more", schema, r.marker.Replication, usage.ReplicationPending, len(storedRows(t, m.f))-rows)
		}
	}
	// In an entry of schema 5 it is refused, replica or not, in a bucket that
	// keeps versions and in one that has paused them; and taken where no
	// marker is made at all.
	m := rowsScene(t)
	m.bucket5("paused", bucket.VersioningSuspended)
	for _, name := range []string{"photos", "paused"} {
		e := deleteEntry(name, "k", "", 1)
		e.DeleteVersion.MarkerID = object.ID{}.String()
		m.refused("a marker without an ID in "+name, entrySchema5, e, domain.ErrInvalidArgument)
	}
	m.ok5(putEntry("plain", "k", 5, 3))
	e := deleteEntry("plain", "k", "", 1)
	e.DeleteVersion.MarkerID = object.ID{}.String()
	if r := m.as(entrySchema5, e); r.err != nil || r.marker != nil || r.previous == nil {
		t.Errorf("a delete that makes no marker, without an ID for one: %+v, %v", r, r.err)
	}
	e = deleteEntry("photos", "gone", serialID(77).String(), 1)
	e.DeleteVersion.MarkerID = object.ID{}.String()
	if r := m.as(entrySchema5, e); r.err != nil {
		t.Errorf("the delete of one version, without an ID for a marker: %v", r.err)
	}
}

// TestAResultByVersionIsOfABucketThatIsSentToTheTarget: a result that names a
// version gives it its status only when the bucket is there and its stored
// replication configuration names the target that acknowledges. Any other is
// not the target's to give: it changes nothing - no status, no count of the
// usage of a bucket - and does not refuse the acknowledgement, whose cursor
// moves and whose other results are taken; the answer counts it. A result
// that is malformed refuses the acknowledgement whole.
func TestAResultByVersionIsOfABucketThatIsSentToTheTarget(t *testing.T) {
	t.Parallel()
	m := quick5(t)
	for _, name := range []string{"src", "src2", "other", "told"} {
		m.bucket5(name, bucket.VersioningEnabled)
	}
	for _, name := range []string{"site-b", "site-c", "site-z"} {
		m.ok5(targetEntry(name, delivery.KindS3))
	}
	m.ok5(targetEntry("hook", delivery.KindWebhook))
	all := bucket.ReplicationRule{ID: "all", Enabled: true}
	m.ok5(configEntry("src", bucket.ConfigReplication, replicateDoc(t, "site-b", all)))
	m.ok5(configEntry("src2", bucket.ConfigReplication, replicateDoc(t, "site-c", all)))
	m.ok5(configEntry("told", bucket.ConfigNotification, notifyDoc(t, bucket.NotificationRule{ID: "all", Target: "hook"})))
	versions := map[string]versionRecord{}
	places := map[string]delivery.Seq{}
	for i, name := range []string{"src", "src2", "other", "told"} {
		versions[name] = *m.ok5(putEntry(name, "k", 5, i+1)).stored
		places[name] = delivery.Seq{Index: m.index}
	}
	of := func(name string, status object.ReplicationStatus) delivery.Result {
		ref, err := object.ParseVersionRef(versions[name].VersionID)
		if err != nil {
			t.Fatal(err)
		}
		return delivery.Result{Bucket: name, Key: "k", Version: ref, Status: status}
	}
	status := func(name string) string {
		t.Helper()
		return statuses(t, m.f, name)["k@"+versions[name].VersionID]
	}
	counts := func(name string) [2]int64 {
		t.Helper()
		usage := checkUsage(t, m.f, name)
		return [2]int64{usage.ReplicationPending, usage.ReplicationFailed}
	}
	if status("src") != "PENDING" || status("src2") != "PENDING" || status("other") != "" || status("told") != "" {
		t.Fatalf("the scene: %q %q %q %q", status("src"), status("src2"), status("other"), status("told"))
	}

	for name, test := range map[string]struct {
		target string
		result delivery.Result
	}{
		"a target that no document names":                {"site-z", of("src", object.ReplicationCompleted)},
		"a target that no document names, failing":       {"site-z", of("src", object.ReplicationFailed)},
		"a bucket without a document":                    {"site-b", of("other", object.ReplicationFailed)},
		"a bucket that is told and not sent on":          {"site-b", of("told", object.ReplicationCompleted)},
		"the version of a bucket sent to another target": {"site-b", of("src2", object.ReplicationCompleted)},
		"the other way round":                            {"site-c", of("src", object.ReplicationFailed)},
		"a bucket that is not there":                     {"site-b", delivery.Result{Bucket: "gone", Key: "k", Version: object.VersionRef{Null: true}, Status: object.ReplicationFailed}},
		"the row of another target":                      {"site-b", delivery.Result{Seq: places["src2"], Status: object.ReplicationCompleted}},
		"the row of a notification":                      {"site-b", delivery.Result{Seq: places["told"], Status: object.ReplicationCompleted}},
	} {
		before := settled(t, m.f)
		acked := storedCursors(t, m.f)[test.target]
		r := m.ok5(ackEntry(test.target, delivery.Seq{}, test.result))
		if r.unrelated != 1 {
			t.Errorf("%s: %d results answered as not the target's, want 1", name, r.unrelated)
		}
		if after := settled(t, m.f); !reflect.DeepEqual(after, before) || storedCursors(t, m.f)[test.target] != acked {
			t.Errorf("%s changed the state", name)
		}
	}

	// Among results that are the target's, one that is not is passed over:
	// the cursor moves, the others are taken, and the answer counts the one.
	r := m.ok5(ackEntry("site-b", places["told"],
		of("other", object.ReplicationFailed), of("src", object.ReplicationFailed), of("src2", object.ReplicationCompleted),
		delivery.Result{Seq: delivery.Seq{Index: 1 << 40}, Status: object.ReplicationCompleted},
	))
	if r.err != nil || r.unrelated != 2 || status("src") != "FAILED" || status("src2") != "PENDING" || status("other") != "" {
		t.Errorf("a mixed acknowledgement: %d not the target's, src %q, src2 %q, other %q", r.unrelated, status("src"), status("src2"), status("other"))
	}
	if got := storedCursors(t, m.f)["site-b"].Acked; got != places["told"] {
		t.Errorf("the cursor of a mixed acknowledgement stands at %s, want %s", got, places["told"])
	}
	if counts("src") != [2]int64{0, 1} || counts("src2") != [2]int64{1, 0} || counts("other") != [2]int64{0, 0} {
		t.Errorf("the buckets count %v, %v and %v", counts("src"), counts("src2"), counts("other"))
	}
	// Its own target gives each its status, and the answer counts nothing.
	if r := m.ok5(ackEntry("site-c", delivery.Seq{}, of("src2", object.ReplicationCompleted))); r.unrelated != 0 || status("src2") != "COMPLETED" {
		t.Errorf("the result of the bucket's own target: %d not the target's, %q", r.unrelated, status("src2"))
	}
	// The answer travels: a node that proposed through the leader is told too.
	encoded, err := encodeResult(applyResult{unrelated: 3})
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := decodeResult(encoded); err != nil || decoded.unrelated != 3 || string(encoded) != `{"unrelated":3}` {
		t.Errorf("the answer is written as %s and read as %+v (%v)", encoded, decoded, err)
	}
	if encoded, _ := encodeResult(applyResult{}); string(encoded) != `{}` {
		t.Errorf("an answer that counts none is written as %s", encoded)
	}

	// A result that is malformed refuses the whole, whatever stands beside it.
	m.refused5("a malformed result beside one that is not the target's", entry{Type: cmdAckOutbox, AckOutbox: &ackOutboxCmd{
		Target: "site-b", Upto: places["told"].String(),
		Results: []ackResult{resultToRecord(of("other", object.ReplicationFailed)), {Bucket: "src", Key: "k", VersionRef: "null", Status: "DONE"}},
	}}, domain.ErrInvalidArgument)

	// The configuration is the stored one, as the entry finds it: once it is
	// removed, or names another target, the results of the old target are
	// not taken any more.
	m.ok5(configEntry("src", bucket.ConfigReplication, replicateDoc(t, "site-c", all)))
	if r := m.ok5(ackEntry("site-b", delivery.Seq{}, of("src", object.ReplicationCompleted))); r.unrelated != 1 || status("src") != "FAILED" {
		t.Errorf("a result of the target a configuration named before: %d not the target's, %q", r.unrelated, status("src"))
	}
	if r := m.ok5(ackEntry("site-c", delivery.Seq{}, of("src", object.ReplicationCompleted))); r.unrelated != 0 || status("src") != "COMPLETED" {
		t.Errorf("a result of the target the configuration names now: %d not the target's, %q", r.unrelated, status("src"))
	}
	m.ok5(configEntry("src", bucket.ConfigReplication, nil))
	if r := m.ok5(ackEntry("site-c", delivery.Seq{}, of("src", object.ReplicationFailed))); r.unrelated != 1 || status("src") != "COMPLETED" {
		t.Errorf("a result for a bucket whose configuration was removed: %d not the target's, %q", r.unrelated, status("src"))
	}
}

// references returns what "targetrefs" holds: the kinds of document, by
// "<target>/<bucket>".
func references(t *testing.T, f *fsm) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := f.view(func(tx *bbolt.Tx) error {
		refs := tx.Bucket(targetRefsBucket())
		if refs == nil {
			return nil
		}
		return refs.ForEachBucket(func(target []byte) error {
			return refs.Bucket(target).ForEach(func(name, kinds []byte) error {
				out[string(target)+"/"+string(name)] = string(kinds)
				return nil
			})
		})
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestATargetGoesOnAdmittingTheBucketsThatNameIt: a target that documents of
// buckets name is replaced only by one that admits every one of those
// buckets - a list that is narrowed past one of them is refused with
// ErrEntityInUse, like a change of kind; a list that is widened, and any
// other change, is taken; and once the document is gone, so is the hold. The
// state keeps which buckets name a target, beside how many documents do, and
// loses the record with the document, the bucket and the target.
func TestATargetGoesOnAdmittingTheBucketsThatNameIt(t *testing.T) {
	t.Parallel()
	m := quick5(t)
	for _, name := range []string{"photos", "logs-eu", "idle"} {
		m.bucket5(name, bucket.VersioningEnabled)
	}
	m.ok5(targetEntry("hook", delivery.KindWebhook, "photos", "logs-*"))
	m.ok5(targetEntry("site-b", delivery.KindS3, "photos"))
	if got := references(t, m.f); len(got) != 0 || len(rawState(t, m.f, targetRefsBucket())) != 0 {
		t.Fatalf("targets that nothing names have references: %v", got)
	}
	rule := bucket.NotificationRule{ID: "all", Target: "hook"}
	m.ok5(configEntry("photos", bucket.ConfigNotification, notifyDoc(t, rule)))
	m.ok5(configEntry("logs-eu", bucket.ConfigNotification, notifyDoc(t, rule)))
	m.ok5(configEntry("photos", bucket.ConfigReplication, replicateDoc(t, "site-b", bucket.ReplicationRule{ID: "all", Enabled: true})))
	want := map[string]string{"hook/photos": "n", "hook/logs-eu": "n", "site-b/photos": "r"}
	if got := references(t, m.f); !reflect.DeepEqual(got, want) {
		t.Fatalf("the references are %v, want %v", got, want)
	}
	// Pinned as bytes: a bucket of bbolt for each target, the bucket's name
	// the key, the letters of the kinds of document the value.
	raw := rawState(t, m.f, targetRefsBucket())
	if raw[fmt.Sprintf("targetrefs/hook/%x", "photos")] != "n" || raw[fmt.Sprintf("targetrefs/site-b/%x", "photos")] != "r" || len(raw) != 4 {
		t.Errorf("the stored references are %v", raw)
	}
	refs := func(name string) int { t.Helper(); return storedTargets(t, m.f)[name].Refs }
	if refs("hook") != 2 || refs("site-b") != 1 {
		t.Fatalf("hook is counted %d times and site-b %d", refs("hook"), refs("site-b"))
	}
	// The same document again counts nothing twice.
	m.ok5(configEntry("photos", bucket.ConfigNotification, notifyDoc(t, rule, bucket.NotificationRule{ID: "more", Target: "hook", Prefix: "a/"})))
	if got := references(t, m.f); !reflect.DeepEqual(got, want) || refs("hook") != 2 {
		t.Fatalf("after the document was replaced by one that names the same target: %v, %d", got, refs("hook"))
	}

	// Narrowed past a bucket that names it: refused, and nothing changed.
	for name, buckets := range map[string][]string{
		"to one of the two":            {"photos"},
		"to the other":                 {"logs-*"},
		"to a pattern that fits one":   {"p*"},
		"to buckets that do not exist": {"elsewhere", "idle"},
	} {
		m.refused5("a webhook narrowed "+name, targetEntry("hook", delivery.KindWebhook, buckets...), domain.ErrEntityInUse)
	}
	closed := targetToRecord(porttest.SampleTarget("hook", delivery.KindWebhook))
	closed.Buckets = nil
	m.refused5("a webhook closed to every bucket", entry{Type: cmdPutTarget, PutTarget: &targetCmd{Target: closed, Audit: trailOf("admin:PutTarget", "target/hook")}}, domain.ErrEntityInUse)
	m.refused5("another deployment narrowed", targetEntry("site-b", delivery.KindS3, "idle"), domain.ErrEntityInUse)
	// Its kind, while named: refused as before, and before the list is asked.
	m.refused5("a webhook made a NATS server", targetEntry("hook", delivery.KindNATS, "*"), domain.ErrEntityInUse)

	// Widened, written another way, or changed in anything else: taken.
	for name, buckets := range map[string][]string{
		"to every bucket":           {"*"},
		"by one more":               {"photos", "logs-*", "idle"},
		"to patterns that fit both": {"p*s", "l*"},
		"to the two names":          {"logs-eu", "photos"},
	} {
		m.ok5(targetEntry("hook", delivery.KindWebhook, buckets...))
		if got := storedTargets(t, m.f)["hook"]; !reflect.DeepEqual(got.Buckets, buckets) || got.Refs != 2 {
			t.Errorf("a webhook widened %s is stored open to %v with %d references", name, got.Buckets, got.Refs)
		}
	}
	other := porttest.SampleTarget("hook", delivery.KindWebhook)
	other.Endpoint, other.Buckets, other.Audit = "https://other.example.com", []string{"logs-eu", "photos"}, true
	m.ok5(entry{Type: cmdPutTarget, PutTarget: &targetCmd{Target: targetToRecord(other)}})

	// Once a document is removed its bucket holds the target no longer.
	m.ok5(configEntry("logs-eu", bucket.ConfigNotification, nil))
	m.refused5("a webhook narrowed past the bucket that is left", targetEntry("hook", delivery.KindWebhook, "logs-*"), domain.ErrEntityInUse)
	m.ok5(targetEntry("hook", delivery.KindWebhook, "photos"))
	if got := references(t, m.f); !reflect.DeepEqual(got, map[string]string{"hook/photos": "n", "site-b/photos": "r"}) || refs("hook") != 1 {
		t.Errorf("after a document was removed: %v, %d", got, refs("hook"))
	}
	// A document that comes to name another target moves its reference.
	m.ok5(targetEntry("bus", delivery.KindNATS))
	m.ok5(configEntry("photos", bucket.ConfigNotification, notifyDoc(t, bucket.NotificationRule{ID: "all", Target: "bus"})))
	if got := references(t, m.f); !reflect.DeepEqual(got, map[string]string{"bus/photos": "n", "site-b/photos": "r"}) || refs("hook") != 0 || refs("bus") != 1 {
		t.Errorf("after the document named another target: %v, hook %d, bus %d", got, refs("hook"), refs("bus"))
	}
	m.ok5(entry{Type: cmdPutTarget, PutTarget: &targetCmd{Target: closed}})
	m.ok5(targetEntry("hook", delivery.KindS3))

	// A bucket that is deleted takes its references with it; and a target
	// that is deleted leaves none for one defined under its name later.
	m.ok5(entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "photos"}})
	if got := references(t, m.f); len(got) != 0 || refs("bus") != 0 || refs("site-b") != 0 {
		t.Errorf("after the bucket was deleted: %v, bus %d, site-b %d", got, refs("bus"), refs("site-b"))
	}
	if raw := rawState(t, m.f, targetRefsBucket()); len(raw) != 1 {
		t.Errorf("targets that nothing names have a place among the references: %v", raw)
	}
	m.ok5(targetEntry("site-b", delivery.KindS3, "idle"))
	m.ok5(configEntry("idle", bucket.ConfigReplication, replicateDoc(t, "site-b", bucket.ReplicationRule{ID: "all", Enabled: true})))
	m.refused5("deleting a target that is named", entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: "site-b"}}, domain.ErrEntityInUse)
	m.ok5(configEntry("idle", bucket.ConfigReplication, nil))
	m.ok5(entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: "site-b"}})
	m.ok5(targetEntry("site-b", delivery.KindS3, "nothing"))
	if got := references(t, m.f); len(got) != 0 {
		t.Errorf("a target defined again has references: %v", got)
	}
}

// TestThereAreSoManyTargets: a deployment has at most delivery.MaxTargets
// targets - every acknowledgement reads the cursor of each - and a document
// names at most bucket.MaxDocumentTargets. One more is refused and leaves
// nothing; a target that exists is replaced at the bound, and a place that
// was freed is taken again.
func TestThereAreSoManyTargets(t *testing.T) {
	t.Parallel()
	m := quick5(t)
	m.bucket5("photos", bucket.VersioningEnabled)
	name := func(i int) string { return fmt.Sprintf("hook-%03d", i) }
	for i := range delivery.MaxTargets {
		m.ok5(targetEntry(name(i), delivery.KindWebhook))
	}
	if got := len(storedTargets(t, m.f)); got != delivery.MaxTargets || len(storedCursors(t, m.f)) != got {
		t.Fatalf("%d targets and %d cursors", got, len(storedCursors(t, m.f)))
	}
	m.refused5("one target more than a deployment has", targetEntry("one-more", delivery.KindWebhook), domain.ErrInvalidArgument)
	m.refused5("one more of another kind", targetEntry("site-b", delivery.KindS3), domain.ErrInvalidArgument)
	m.ok5(targetEntry(name(7), delivery.KindWebhook, "photos"))
	m.ok5(entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: name(0)}})
	m.ok5(targetEntry("one-more", delivery.KindWebhook))
	m.refused5("one more again", targetEntry("site-b", delivery.KindS3), domain.ErrInvalidArgument)

	// A document names so many targets and no more, whatever its rules.
	rules := make([]bucket.NotificationRule, bucket.MaxDocumentTargets+1)
	for i := range rules {
		rules[i] = bucket.NotificationRule{ID: fmt.Sprint("r", i), As: bucket.NotifyAsQueue, Target: name(i + 1), On: everyChange()}
	}
	if _, err := (bucket.NotificationConfig{Rules: rules}).Document(); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("a document of %d targets is written: %v", len(rules), err)
	}
	// As bytes, in an entry: written by hand, since the writer refuses it.
	document := notifyDoc(t, rules[:bucket.MaxDocumentTargets]...)
	last := rules[bucket.MaxDocumentTargets]
	forged := strings.Replace(string(document), `],"rules":[`, `,"`+last.Target+`"],"rules":[`, 1)
	forged = strings.TrimSuffix(forged, "]}") + `,{"id":"` + last.ID + `","as":"queue","target":"` + last.Target + `","on":["put","complete","marker","delete"]}]}`
	if !json.Valid([]byte(forged)) || strings.Count(forged, `"target":`) != bucket.MaxDocumentTargets+1 {
		t.Fatalf("the document made by hand: %s", forged)
	}
	m.refused5("a document that names one target too many", configEntry("photos", bucket.ConfigNotification, []byte(forged)), domain.ErrInvalidArgument)
	m.ok5(configEntry("photos", bucket.ConfigNotification, document))
	r := m.ok5(putEntry("photos", "k", 5, 1))
	if rows := storedRows(t, m.f); len(rows) != 1 || len(rows[0].To) != bucket.MaxDocumentTargets || r.err != nil {
		t.Errorf("a put that every rule matches appends %s", told(rows))
	}
}

// TestTheLargestRowOfTheOutbox: the size of a row is bounded by rules of
// bytes, and this is the bound ADR-0023 states ("The bound"): a key as long
// as a key can be, of characters that are written as six bytes each; a bucket
// name, an entity tag and the name of an actor at their bounds; as many
// notification targets as a document names and the one a bucket is sent on
// to, each of a name as long as a name can be; a size of nineteen digits; a
// time to the nanosecond. What an entry carries beyond those bounds is left
// out of the row or refused, so no row of a put is longer - and the row of a
// completion is longer by the five letters its kind of change has more.
func TestTheLargestRowOfTheOutbox(t *testing.T) {
	t.Parallel()
	// The figure of ADR-0023 is this and those five letters: 9 830 bytes.
	const largest = 9825
	if extra := len(delivery.EventComplete) - len(delivery.EventPut); extra != 5 {
		t.Fatalf("a completion is written with %d letters more than a put", extra)
	}
	m := quick5(t)
	name := strings.Repeat("b", bucket.MaxNameLength)
	m.ok5(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: name, CreatedAt: m.at, CreatedBy: "root"}})
	m.ok5(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: name, Status: string(bucket.VersioningEnabled)}})
	target := func(i int) string { return fmt.Sprintf("%02d", i) + strings.Repeat("t", 61) }
	rules := make([]bucket.NotificationRule, bucket.MaxDocumentTargets)
	for i := range rules {
		m.ok5(targetEntry(target(i), delivery.KindWebhook))
		rules[i] = bucket.NotificationRule{ID: fmt.Sprint(i), Target: target(i)}
	}
	m.ok5(targetEntry(target(99), delivery.KindS3))
	m.ok5(configEntry(name, bucket.ConfigNotification, notifyDoc(t, rules...)))
	m.ok5(configEntry(name, bucket.ConfigReplication, replicateDoc(t, target(99), bucket.ReplicationRule{ID: "all", Enabled: true})))

	put := putEntry(name, strings.Repeat("<", object.MaxKeyLength), 1<<62, 1)
	put.PutObject.ETag = strings.Repeat("&", delivery.MaxItemETag)
	put.PutObject.CreatedBy = strings.Repeat(">", 256)
	m.at = m.at.Add(123456789 * time.Nanosecond)
	if r := m.ok5(put); r.stored.Replication != string(object.ReplicationPending) {
		t.Fatalf("the put is stored as %q", r.stored.Replication)
	}
	rows := rawState(t, m.f, outboxBucket())
	delete(rows, "outbox")
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	for _, row := range rows {
		if len(row) != largest {
			t.Errorf("the largest row is %d bytes, and ADR-0023 says %d", len(row), largest)
		}
		if strings.Count(row, `"n:`) != bucket.MaxDocumentTargets || strings.Count(row, `"r:`) != 1 || !strings.Contains(row, `"by":"`) || !strings.Contains(row, `"et":"`) {
			t.Errorf("the row lacks something of what a row can carry: %s", row)
		}
	}

	// Beyond the bounds: an entity tag that is longer is left out of the
	// row, as a name of an actor that cannot be kept is, and the version is
	// sent on all the same; a bucket of a longer name has no document.
	longer := putEntry(name, "k", 5, 2)
	longer.PutObject.ETag = strings.Repeat("e", delivery.MaxItemETag+1)
	longer.PutObject.CreatedBy = strings.Repeat("u", 257)
	if r := m.ok5(longer); r.stored.Replication != string(object.ReplicationPending) || r.stored.ETag != longer.PutObject.ETag {
		t.Errorf("a version with an entity tag beyond the bound is stored as %q with a tag of %d bytes", r.stored.Replication, len(r.stored.ETag))
	}
	items := storedRows(t, m.f)
	if row := items[len(items)-1]; len(items) != 2 || row.ETag != "" || row.By != "" || row.Key != "k" || len(row.To) != bucket.MaxDocumentTargets+1 {
		t.Errorf("the row of a version with an entity tag beyond the bound: %+v", row)
	}
	long := strings.Repeat("b", bucket.MaxNameLength+1)
	m.ok5(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: long, CreatedAt: m.at, CreatedBy: "root"}})
	m.refused5("a document for a bucket of a name that is too long", configEntry(long, bucket.ConfigNotification, notifyDoc(t, rules[0])), domain.ErrInvalidArgument)
}

// TestADeleteThatIsRefusedLeavesNothing: in a state of schema 5 a delete that
// is refused, and one that finds nothing to delete, leaves the state as it
// was, byte for byte - the place of the bucket among the versions and in the
// index of them is made by the first delete that writes. For a bucket without
// a usage record, which is every bucket of a state below schema 5, the place
// is made by the first delete that looks, as the release of schema 4 makes it.
func TestADeleteThatIsRefusedLeavesNothing(t *testing.T) {
	t.Parallel()
	m := quick5(t)
	m.bucket5("dst", bucket.VersioningEnabled)
	m.bucket5("kept", bucket.VersioningEnabled)
	m.bucket5("plain", bucket.Unversioned)
	m.ok5(replicaModeEntry("dst", siteA))
	placed := func(name string) bool {
		t.Helper()
		state := wholeState(t, m.f)
		_, versions := state["versions/"+name+"/"]
		_, ids := state["versionids/"+name+"/"]
		if versions != ids {
			t.Fatalf("bucket %s has a place among the versions: %t; in the index: %t", name, versions, ids)
		}
		return versions
	}
	for _, name := range []string{"dst", "kept", "plain"} {
		if placed(name) {
			t.Fatalf("bucket %s has a place among the versions before anything was deleted", name)
		}
	}
	unreadable := replicaMarker("dst", "k", 1, 10)
	unreadable.DeleteVersion.MarkerID = "nonsense"
	expects := func(e entry) entry { e.DeleteVersion.Expect = serialID(9).String(); return e }
	plain := deleteEntry("kept", "k", "", 1)
	plain.DeleteVersion.MarkerID = "nonsense"
	for name, test := range map[string]struct {
		e    entry
		want error
	}{
		"the copy of a marker with an ID that cannot be read": {unreadable, domain.ErrInvalidArgument},
		"the copy of a marker without an ID":                  {zeroMarker("dst", "k", 10, false), domain.ErrInvalidArgument},
		"the copy of a marker decided for another version":    {expects(replicaMarker("dst", "k", 1, 10)), domain.ErrConflict},
		"the copy of a marker of another source":              {copied(deleteEntry("dst", "k", "", 1), siteC, 10, false), domain.ErrInvalidBucketState},
		"a delete of its own in a replica bucket":             {deleteEntry("dst", "k", "", 1), domain.ErrInvalidBucketState},
		"a marker with an ID that cannot be read":             {plain, domain.ErrInvalidArgument},
		"a marker decided for another version":                {expects(deleteEntry("kept", "k", "", 1)), domain.ErrConflict},
		"the delete of one version, decided for another":      {expects(deleteEntry("kept", "k", serialID(5).String(), 1)), domain.ErrConflict},
		"a delete where no versions are kept, decided so":     {expects(deleteEntry("plain", "k", "", 1)), domain.ErrConflict},
	} {
		m.refused5(name, test.e, test.want)
	}
	// What finds nothing to delete is no refusal, and leaves nothing either.
	for name, e := range map[string]entry{
		"the delete of a version that is not there": deleteEntry("kept", "k", serialID(5).String(), 1),
		"a delete of nothing, where none are kept":  deleteEntry("plain", "k", "", 1),
	} {
		before := settled(t, m.f)
		if r := m.ok5(e); r.previous != nil || r.marker != nil || len(r.events) != 0 {
			t.Errorf("%s answers %+v", name, r)
		}
		if after := settled(t, m.f); !reflect.DeepEqual(after, before) {
			t.Errorf("%s changed the state", name)
		}
	}
	// The first delete that writes makes the place.
	m.ok5(replicaMarker("dst", "k", 1, 10))
	m.ok5(deleteEntry("kept", "k", "", 2))
	if !placed("dst") || !placed("kept") || placed("plain") {
		t.Errorf("after markers were made: dst %t, kept %t, plain %t", placed("dst"), placed("kept"), placed("plain"))
	}
	// And one that is refused then leaves what is there.
	m.refused5("a marker that is there already", replicaMarker("dst", "k", 1, 10), domain.ErrConflict)

	// A bucket made below schema 5 has no usage record: its place is made
	// by the first delete that looks, refused or not, as it always was.
	old := newMachine5(t)
	old.f.db.NoSync = true
	old.as(entrySchema4, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "old", CreatedAt: old.at}})
	old.as(entrySchema4, entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "old", Status: string(bucket.VersioningEnabled)}})
	if r := old.as(entrySchema4, expects(deleteEntry("old", "k", "", 1))); !errors.Is(r.err, domain.ErrConflict) {
		t.Fatalf("a delete decided for another version = %v", r.err)
	}
	state := wholeState(t, old.f)
	if _, there := state["versions/old/"]; !there {
		t.Errorf("a refused delete of schema 4 left no place among the versions: the release of schema 4 leaves one")
	}
}

// TestABucketNameThatCannotBeStoredIsRefusedBeforeItIsProposed: the state
// machine does not hold a bucket.create to a name, as the release of schema 4
// does not, and an entry that names a bucket by nothing, or by more than a
// key of the state can have, stops every member that applies it. So the store
// proposes none: it refuses the name by itself, definitely, and logs nothing.
// (What a name may consist of is the use case's to judge.)
func TestABucketNameThatCannotBeStoredIsRefusedBeforeItIsProposed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openStore(t, t.TempDir())
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "kept"}); err != nil {
		t.Fatal(err)
	}
	index, err := store.AppliedIndex()
	if err != nil {
		t.Fatal(err)
	}
	for what, name := range map[string]string{
		"no name":                       "",
		"a name one byte too long":      strings.Repeat("n", bucket.MaxNameLength+1),
		"a name no key of bbolt can be": strings.Repeat("n", bbolt.MaxKeySize+1),
	} {
		err := store.CreateBucket(ctx, bucket.Bucket{Name: name})
		if !errors.Is(err, domain.ErrInvalidBucketName) || errors.Is(err, domain.ErrOutcomeUnknown) {
			t.Errorf("%s = %v, want a definite %v", what, err, domain.ErrInvalidBucketName)
		}
	}
	if after, err := store.AppliedIndex(); err != nil || after != index || store.fsm.health() != nil {
		t.Fatalf("a refused name was proposed: the state is at entry %d (%v), was at %d; health %v", after, err, index, store.fsm.health())
	}
	// At the bound a name is proposed, and the state machine goes on.
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: strings.Repeat("n", bucket.MaxNameLength)}); err != nil {
		t.Errorf("a name as long as a name can be: %v", err)
	}
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "b"}); err != nil {
		t.Errorf("a short name, which is the use case's to refuse: %v", err)
	}

	// What the guard is for: such an entry, applied, stops the state machine
	// - in this release and in the release of schema 4 alike - and it is not
	// this release's to refuse it there.
	for what, name := range map[string]string{"no name": "", "a name no key can be": strings.Repeat("n", bbolt.MaxKeySize+1)} {
		m := newMachine5(t)
		if r := m.raw(entrySchema4, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: name, CreatedAt: m.at}}); !r.fatal {
			t.Errorf("a bucket.create with %s is applied: %v", what, r.err)
		}
	}
}
