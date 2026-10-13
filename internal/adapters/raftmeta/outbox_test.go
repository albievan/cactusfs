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

	"github.com/hashicorp/raft"
	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// Targets, the outbox and its cursors in the state machine (ADR-0023, "The
// outbox", "Targets"; PLAN 8.0.8): target.put and target.delete, the two
// kinds of bucket configuration that name targets, the rows every change of
// an object appends, outbox.ack in the order the ADR gives, and the bound.
// Replica buckets are in replica_test.go, and long random histories against
// the model of the ports in delivery_property_test.go.

// quick5 is a state machine that applies schema 5 and does not wait for the
// drive: for the tests that apply hundreds of entries.
func quick5(t *testing.T) *machine {
	t.Helper()
	return &machine{t: t, f: fastFSM(t), at: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
}

// targetEntry defines a sample target of a kind, open to the buckets named,
// or to every bucket.
func targetEntry(name string, kind delivery.Kind, buckets ...string) entry {
	target := porttest.SampleTarget(name, kind)
	if len(buckets) > 0 {
		target.Buckets = buckets
	}
	return entry{Type: cmdPutTarget, PutTarget: &targetCmd{Target: targetToRecord(target)}}
}

// everyChange are the kinds of change a notification rule can ask for.
func everyChange() []delivery.Event {
	return []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventMarker, delivery.EventDelete}
}

// notifyDoc is the canonical document of notification rules, each for every
// kind of change unless it says otherwise.
func notifyDoc(t *testing.T, rules ...bucket.NotificationRule) []byte {
	t.Helper()
	for i := range rules {
		if rules[i].As == "" {
			rules[i].As = bucket.NotifyAsQueue
		}
		if len(rules[i].On) == 0 {
			rules[i].On = everyChange()
		}
	}
	document, err := bucket.NotificationConfig{Rules: rules}.Document()
	if err != nil {
		t.Fatal(err)
	}
	return document
}

// replicateDoc is the canonical document of a replication configuration to
// the bucket "copy" of a target.
func replicateDoc(t *testing.T, target string, rules ...bucket.ReplicationRule) []byte {
	t.Helper()
	document, err := bucket.ReplicationConfig{Target: target, Bucket: "copy", Rules: rules}.Document()
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func configEntry(bucketName, kind string, document []byte) entry {
	return entry{Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Bucket: bucketName, Kind: kind, Document: document}}
}

func ackEntry(target string, upto delivery.Seq, results ...delivery.Result) entry {
	cmd := &ackOutboxCmd{Target: target, Upto: upto.String()}
	for _, r := range results {
		cmd.Results = append(cmd.Results, resultToRecord(r))
	}
	return entry{Type: cmdAckOutbox, AckOutbox: cmd}
}

// storedRows returns the rows of the outbox, oldest first.
func storedRows(t *testing.T, f *fsm) []delivery.Item {
	t.Helper()
	var rows []delivery.Item
	if err := f.view(func(tx *bbolt.Tx) error {
		outbox := tx.Bucket(outboxBucket())
		if outbox == nil {
			return nil
		}
		return outbox.ForEach(func(key, raw []byte) error {
			item, err := decodeItem(key, raw)
			if err != nil {
				return err
			}
			// A row is stored as the bytes its record writes.
			again, err := json.Marshal(itemToRecord(item))
			if err != nil {
				return err
			}
			if !bytes.Equal(again, raw) {
				return fmt.Errorf("a row is stored as %s and written again as %s", raw, again)
			}
			rows = append(rows, item)
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

// storedCursors returns the cursor of every target, by name.
func storedCursors(t *testing.T, f *fsm) map[string]delivery.Cursor {
	t.Helper()
	cursors := map[string]delivery.Cursor{}
	if err := f.view(func(tx *bbolt.Tx) error {
		stored := tx.Bucket(outboxCursorBucket())
		if stored == nil {
			return nil
		}
		return stored.ForEach(func(name, raw []byte) error {
			cursor, err := decodeCursor(string(name), raw)
			cursors[string(name)] = cursor
			return err
		})
	}); err != nil {
		t.Fatal(err)
	}
	return cursors
}

// storedTargets returns the record of every target, by name.
func storedTargets(t *testing.T, f *fsm) map[string]targetRecord {
	t.Helper()
	targets := map[string]targetRecord{}
	if err := f.view(func(tx *bbolt.Tx) error {
		stored := tx.Bucket(targetsBucket())
		if stored == nil {
			return nil
		}
		return stored.ForEach(func(name, _ []byte) error {
			record, err := readTarget(tx, string(name))
			if err != nil {
				return err
			}
			targets[string(name)] = *record
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return targets
}

// outboxCounts returns the two counts of the outbox as the state keeps them.
func outboxCounts(t *testing.T, f *fsm) (rows, dropped uint64) {
	t.Helper()
	if err := f.view(func(tx *bbolt.Tx) error {
		rows, dropped = readCounter(tx, outboxCountKey()), readCounter(tx, outboxDroppedKey())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return rows, dropped
}

// number is n as a count: how many there are of something.
func number(n int) uint64 {
	var count uint64
	for range n {
		count++
	}
	return count
}

// told writes rows as "event key>consumer,consumer", in their order.
func told(rows []delivery.Item) string {
	parts := make([]string, len(rows))
	for i, row := range rows {
		to := make([]string, len(row.To))
		for n, consumer := range row.To {
			to[n] = consumer.String()
		}
		parts[i] = fmt.Sprintf("%s %s>%s", row.Event, row.Key, strings.Join(to, ","))
	}
	return strings.Join(parts, "; ")
}

// settled is the state without what every entry leaves behind: the index of
// the last entry and its time.
func settled(t *testing.T, f *fsm) map[string]string {
	t.Helper()
	state := wholeState(t, f)
	for _, key := range [][]byte{appliedKey(), timeKey()} {
		delete(state, fmt.Sprintf("meta/%x", key))
	}
	return state
}

// refused applies an entry written in a schema, which must be refused with
// want, tell nobody and leave the state as it was, byte for byte - no bucket
// made, no count moved.
func (m *machine) refused(what string, schema byte, e entry, want error) {
	m.t.Helper()
	before := settled(m.t, m.f)
	r := m.as(schema, e)
	if !errors.Is(r.err, want) {
		m.t.Fatalf("%s = %v, want %v", what, r.err, want)
	}
	if len(r.events) != 0 || r.stored != nil || r.marker != nil || r.previous != nil {
		m.t.Errorf("%s was refused and answers %+v", what, r)
	}
	if after := settled(m.t, m.f); !reflect.DeepEqual(after, before) {
		for key, value := range after {
			if before[key] != value {
				m.t.Errorf("%s: %s is now %q, was %q", what, key, value, before[key])
			}
		}
		m.t.Fatalf("%s was refused and changed the state", what)
	}
}

// refused5 is refused for an entry as a cluster at level 5 writes it.
func (m *machine) refused5(what string, e entry, want error) {
	m.t.Helper()
	held, schema := written(e, entrySchema5)
	m.refused(what, schema, held, want)
}

// statuses returns the replication status of every version of a bucket, by
// key and version.
func statuses(t *testing.T, f *fsm, bucketName string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, v := range everyVersion(t, f, bucketName) {
		out[v.Key+"@"+refOf(v)] = v.Replication
	}
	return out
}

// trailOf is an audit record as an administrative change carries it.
func trailOf(action, resource string) *auditRecord {
	return auditToRecord(audit.Record{
		Time: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), Actor: "user/albie", Action: action, Resource: resource, Outcome: audit.Succeeded,
	})
}

// setStatus writes the replication status of the current version of a key
// by hand, as no entry does: for a test that needs a version back where it
// was. The usage record is put right with it.
func setStatus(t *testing.T, f *fsm, bucketName, key, status string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.db.Update(func(tx *bbolt.Tx) error {
		index := tx.Bucket(objectsBucket()).Bucket([]byte(bucketName))
		v, err := readVersion(index, key)
		if err != nil {
			return err
		}
		usage, _, err := readUsage(tx, bucketName)
		if err != nil {
			return err
		}
		for was, count := range map[string]*int64{"PENDING": &usage.ReplicationPending, "FAILED": &usage.ReplicationFailed} {
			if v.Replication == was {
				*count--
			}
			if status == was {
				*count++
			}
		}
		v.Replication = status
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err := index.Put([]byte(key), data); err != nil {
			return err
		}
		_, err = writeUsage(tx, bucketName, usage)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func auditCount(t *testing.T, f *fsm) int {
	t.Helper()
	return len(rawStateOf(rawState(t, f, auditBucket()), auditBucket()))
}

// TestATargetIsDefinedReplacedAndDeleted: target.put keeps a target as the
// entry gives it but for what is the state's - the count of the documents
// that name it, when it was made, and its cursor, which begins at the entry
// that made it; target.delete removes it with its cursor. Both carry their
// audit record in the entry, and tell the name and the kind and nothing
// else.
func TestATargetIsDefinedReplacedAndDeleted(t *testing.T) {
	t.Parallel()
	m := quick5(t)
	m.bucket5("photos", bucket.VersioningEnabled)

	hook := porttest.SampleTarget("hook", delivery.KindWebhook)
	hook.Buckets, hook.Refs = []string{"photos", "logs-*"}, 7
	hook.CreatedAt = time.Date(2026, 10, 12, 11, 0, 0, 0, time.FixedZone("", 7200))
	trail := auditCount(t, m.f)
	r := m.ok5(entry{Type: cmdPutTarget, PutTarget: &targetCmd{Target: targetToRecord(hook), Audit: trailOf("admin:PutTarget", "target/hook")}})
	made := m.index
	if len(r.events) != 2 || r.events[0].Type != ports.EventTargetChanged || r.events[0].Data != (ports.TargetEvent{Name: "hook", Kind: "webhook"}) ||
		r.events[1].Type != ports.EventAuditRecorded {
		t.Errorf("target.put tells %+v", r.events)
	}
	if got := auditCount(t, m.f); got != trail+1 {
		t.Errorf("%d audit records after a target.put with one, there were %d", got, trail)
	}
	stored := storedTargets(t, m.f)["hook"]
	want := targetToRecord(hook)
	want.Refs, want.CreatedAt = 0, hook.CreatedAt.UTC()
	if !reflect.DeepEqual(stored, want) {
		t.Errorf("the stored target is\n%+v\nwant\n%+v", stored, want)
	}
	if got := stored.toDomain(); !got.PathSet() || got.Endpoint != "https://hooks.example.com" || string(got.SealedPath.Ciphertext) != "sealed-path-hook" {
		t.Errorf("the endpoint of the stored target: %q, a sealed path: %t", got.Endpoint, got.PathSet())
	}
	here := delivery.Seq{Index: made}
	if got := storedCursors(t, m.f); !reflect.DeepEqual(got, map[string]delivery.Cursor{"hook": {Target: "hook", From: here, Acked: here}}) {
		t.Errorf("the cursor of a new target = %+v, want it to begin at entry %d", got, made)
	}

	// A document names it: the count is the state's, whatever an entry says.
	m.ok5(configEntry("photos", bucket.ConfigNotification, notifyDoc(t, bucket.NotificationRule{ID: "r", Target: "hook"})))
	m.ok5(putEntry("photos", "a", 5, 1))
	m.ok5(ackEntry("hook", delivery.Seq{Index: m.index}))
	before := storedCursors(t, m.f)["hook"]
	if before.Acked != (delivery.Seq{Index: m.index - 1}) {
		t.Fatalf("the cursor after an acknowledgement = %+v", before)
	}

	// Replaced whole: no path, no secret, another origin - and it keeps its
	// count, when it was made, and its cursor.
	replaced := porttest.SampleTarget("hook", delivery.KindWebhook)
	replaced.Endpoint, replaced.SealedPath, replaced.Audit, replaced.Buckets = "https://other.example.com:8443", iam.SealedSecret{}, true, []string{"ph*"}
	replaced.Auth, replaced.Secret, replaced.Refs = delivery.Auth{Mode: delivery.AuthNone}, iam.SealedSecret{}, 99
	replaced.CreatedAt, replaced.UpdatedAt = hook.CreatedAt.Add(time.Hour), hook.UpdatedAt.Add(time.Hour)
	record := targetToRecord(replaced)
	m.ok5(entry{Type: cmdPutTarget, PutTarget: &targetCmd{Target: record}})
	stored = storedTargets(t, m.f)["hook"]
	want = targetToRecord(replaced)
	want.Refs, want.CreatedAt = 1, hook.CreatedAt.UTC()
	if !reflect.DeepEqual(stored, want) {
		t.Errorf("the replaced target is\n%+v\nwant\n%+v", stored, want)
	}
	if got := storedCursors(t, m.f)["hook"]; got != before {
		t.Errorf("the cursor of a target that was replaced = %+v, was %+v", got, before)
	}
	raw := rawState(t, m.f, targetsBucket())[fmt.Sprintf("targets/%x", "hook")]
	if strings.Contains(raw, "endpoint_path") || strings.Contains(raw, `"secret"`) || !strings.Contains(raw, `"buckets":["ph*"]`) || !strings.Contains(raw, `"refs":1`) {
		t.Errorf("the record of a target without a path and without a secret: %s", raw)
	}

	// While a document names it, it keeps its kind and cannot be deleted.
	m.refused5("a webhook that is named, made another deployment", targetEntry("hook", delivery.KindS3), domain.ErrEntityInUse)
	m.refused5("a webhook that is named, made a NATS server", targetEntry("hook", delivery.KindNATS), domain.ErrEntityInUse)
	m.refused5("deleting a target that is named", entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: "hook", Audit: trailOf("admin:DeleteTarget", "target/hook")}}, domain.ErrEntityInUse)
	m.refused5("deleting a target that is not", entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: "gone"}}, domain.ErrEntityNotFound)

	// What cannot be kept is refused by bytes, and leaves nothing: not the
	// bucket of targets either, in a state that had none.
	fresh := quick5(t)
	fresh.bucket5("photos", bucket.Unversioned)
	for name, edit := range map[string]func(*targetRecord){
		"a record of another version":   func(r *targetRecord) { r.V = 2 },
		"a record of no version":        func(r *targetRecord) { r.V = 0 },
		"no name":                       func(r *targetRecord) { r.Name = "" },
		"a kind there is not":           func(r *targetRecord) { r.Kind = "kafka" },
		"an endpoint in the clear":      func(r *targetRecord) { r.Endpoint = "http://hooks.example.com" },
		"an endpoint with its path":     func(r *targetRecord) { r.Endpoint = "https://hooks.example.com/services/T0/B0/token" },
		"an endpoint that ends a slash": func(r *targetRecord) { r.Endpoint = "https://hooks.example.com/" },
		"a sealed path that is too long": func(r *targetRecord) {
			r.EndpointPath.Ciphertext = make([]byte, delivery.MaxSealedBytes+1)
		},
		"a sealed secret that is too long": func(r *targetRecord) { r.Secret.Ciphertext = make([]byte, delivery.MaxSealedBytes+1) },
		"a signing key with a long key":    func(r *targetRecord) { r.SignKey.WrappedKey = make([]byte, delivery.MaxSealedKeyBytes+1) },
		"a secret without a use":           func(r *targetRecord) { r.Auth.Mode = "none" },
		"a bearer without a secret":        func(r *targetRecord) { r.Secret = nil },
		"a bucket twice":                   func(r *targetRecord) { r.Buckets = []string{"a", "a"} },
		"certificates that are a key": func(r *targetRecord) {
			r.CAPEM = []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")
		},
	} {
		bad := targetToRecord(porttest.SampleTarget("bad", delivery.KindWebhook))
		edit(&bad)
		fresh.refused5(name, entry{Type: cmdPutTarget, PutTarget: &targetCmd{Target: bad, Audit: trailOf("admin:PutTarget", "target/bad")}}, domain.ErrInvalidArgument)
	}
	for name, edit := range map[string]func(*targetRecord){
		"a NATS server with a path":      func(r *targetRecord) { r.EndpointPath = &sealedRecord{Ciphertext: []byte("x")} },
		"a NATS server without subject":  func(r *targetRecord) { r.Subject = "" },
		"a NATS server with a sign key":  func(r *targetRecord) { r.SignKey = &sealedRecord{Ciphertext: []byte("x")} },
		"a NATS server over HTTPS":       func(r *targetRecord) { r.Endpoint = "https://nats.example.com" },
		"a NATS endpoint with something": func(r *targetRecord) { r.Endpoint = "tls://nats.example.com:4222/x" },
	} {
		bad := targetToRecord(porttest.SampleTarget("bad", delivery.KindNATS))
		edit(&bad)
		fresh.refused5(name, entry{Type: cmdPutTarget, PutTarget: &targetCmd{Target: bad}}, domain.ErrInvalidArgument)
	}
	for name, edit := range map[string]func(*targetRecord){
		"another deployment with a path":      func(r *targetRecord) { r.EndpointPath = &sealedRecord{Ciphertext: []byte("x")} },
		"another deployment sent the trail":   func(r *targetRecord) { r.Audit = true },
		"another deployment without a region": func(r *targetRecord) { r.Region = "" },
	} {
		bad := targetToRecord(porttest.SampleTarget("bad", delivery.KindS3))
		edit(&bad)
		fresh.refused5(name, entry{Type: cmdPutTarget, PutTarget: &targetCmd{Target: bad}}, domain.ErrInvalidArgument)
	}
	if got := rawState(t, fresh.f, targetsBucket(), outboxCursorBucket(), outboxBucket()); len(got) != 0 {
		t.Errorf("targets that were refused left %v in the state", got)
	}
	// A target.put in an entry that claims an older schema is no entry.
	if r := fresh.raw(entrySchema4, targetEntry("hook", delivery.KindWebhook)); !r.fatal || !errors.Is(r.err, domain.ErrUnsupportedFormat) {
		t.Errorf("a target.put of schema 4 = %+v", r.err)
	}

	// Once nothing names it, it can become of another kind, and be deleted:
	// its cursor goes with it, and one of its name defined later begins
	// behind every row there is.
	m.ok5(configEntry("photos", bucket.ConfigNotification, nil))
	if got := storedTargets(t, m.f)["hook"].Refs; got != 0 {
		t.Fatalf("%d references after the document was removed", got)
	}
	// And admit no bucket at all: a list that admits none is written as a
	// list.
	closed := targetToRecord(porttest.SampleTarget("hook", delivery.KindWebhook))
	closed.Buckets = nil
	m.ok5(entry{Type: cmdPutTarget, PutTarget: &targetCmd{Target: closed}})
	if raw := rawState(t, m.f, targetsBucket())[fmt.Sprintf("targets/%x", "hook")]; !strings.Contains(raw, `"buckets":[]`) || strings.Contains(raw, `"refs"`) {
		t.Errorf("the record of a target that admits no bucket: %s", raw)
	}
	m.ok5(targetEntry("hook", delivery.KindNATS))
	if got := storedTargets(t, m.f)["hook"]; got.Kind != "nats" || !got.CreatedAt.Equal(hook.CreatedAt) {
		t.Errorf("a target nothing names, made of another kind: %+v", got)
	}
	trail = auditCount(t, m.f)
	r = m.ok5(entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: "hook", Audit: trailOf("admin:DeleteTarget", "target/hook")}})
	if len(r.events) != 2 || r.events[0].Type != ports.EventTargetDeleted || r.events[0].Data != (ports.TargetEvent{Name: "hook", Kind: "nats"}) {
		t.Errorf("target.delete tells %+v", r.events)
	}
	if got := auditCount(t, m.f); got != trail+1 {
		t.Errorf("%d audit records after a target.delete with one, there were %d", got, trail)
	}
	if len(storedTargets(t, m.f)) != 0 || len(storedCursors(t, m.f)) != 0 {
		t.Errorf("a deleted target left %v and %v", storedTargets(t, m.f), storedCursors(t, m.f))
	}
	m.ok5(targetEntry("hook", delivery.KindWebhook))
	if got := storedCursors(t, m.f)["hook"]; got.From != (delivery.Seq{Index: m.index}) || got.Acked != got.From {
		t.Errorf("the cursor of a target defined again = %+v, want it to begin at entry %d", got, m.index)
	}
	// No event and no audit record of a target holds anything but its name
	// and its kind.
	trailText := fmt.Sprint(rawState(t, m.f, auditBucket()))
	for _, secret := range []string{"hooks.example.com", "sealed-", "wrapped-", "other.example.com"} {
		if strings.Contains(trailText, secret) {
			t.Errorf("the audit trail holds %q of a target", secret)
		}
	}
}

// TestADocumentNamesTargetsThatAreThereFitAndAdmitTheBucket: the two kinds
// of bucket configuration of schema 5 are taken only as the bytes their
// parsers write, and only when every target they name exists, is of a kind
// that serves them and admits the bucket; a replication configuration only by
// a bucket that keeps versions. The count of every target follows put,
// replace, removal and the deletion of the bucket.
func TestADocumentNamesTargetsThatAreThereFitAndAdmitTheBucket(t *testing.T) {
	t.Parallel()
	m := quick5(t)
	m.bucket5("photos", bucket.VersioningEnabled)
	m.bucket5("plain", bucket.Unversioned)
	m.bucket5("closed", bucket.VersioningEnabled)
	m.ok5(targetEntry("hook", delivery.KindWebhook, "photos", "pl*"))
	m.ok5(targetEntry("bus", delivery.KindNATS))
	m.ok5(targetEntry("site-b", delivery.KindS3, "photos", "plain"))
	refs := func() map[string]int {
		t.Helper()
		out := map[string]int{}
		for name, record := range storedTargets(t, m.f) {
			out[name] = record.Refs
		}
		return out
	}
	rule := func(id, target string) bucket.NotificationRule {
		return bucket.NotificationRule{ID: id, Target: target}
	}
	enabled := bucket.ReplicationRule{ID: "all", Enabled: true}
	valid := notifyDoc(t, rule("r", "hook"))
	spaced := bytes.Replace(valid, []byte(`,"rules"`), []byte(`, "rules"`), 1)
	newer := bytes.Replace(valid, []byte(`{"v":1`), []byte(`{"v":2`), 1)
	refusals := []struct {
		name     string
		bucket   string
		kind     string
		document []byte
		want     error
	}{
		{"rules that are not canonical", "photos", bucket.ConfigNotification, spaced, domain.ErrInvalidArgument},
		{"rules of a version that is not known", "photos", bucket.ConfigNotification, newer, domain.ErrInvalidArgument},
		{"rules that are no JSON", "photos", bucket.ConfigNotification, []byte("{}"), domain.ErrInvalidArgument},
		{"rules for a target that is not there", "photos", bucket.ConfigNotification, notifyDoc(t, rule("r", "hook"), rule("s", "gone")), domain.ErrInvalidArgument},
		{"rules for another deployment", "photos", bucket.ConfigNotification, notifyDoc(t, rule("r", "site-b")), domain.ErrInvalidArgument},
		{"rules for a target that does not admit the bucket", "closed", bucket.ConfigNotification, notifyDoc(t, rule("r", "bus"), rule("s", "hook")), domain.ErrInvalidArgument},
		{"rules that are too long", "photos", bucket.ConfigNotification, bytes.Repeat([]byte("x"), bucket.MaxConfigBytes+1), domain.ErrInvalidArgument},
		{"rules for a bucket that is not there", "gone", bucket.ConfigNotification, valid, domain.ErrBucketNotFound},
		{"a configuration that is not canonical", "photos", bucket.ConfigReplication, append(replicateDoc(t, "site-b", enabled), '\n'), domain.ErrInvalidArgument},
		{"a configuration of a version that is not known", "photos", bucket.ConfigReplication, bytes.Replace(replicateDoc(t, "site-b", enabled), []byte(`{"v":1`), []byte(`{"v":3`), 1), domain.ErrInvalidArgument},
		{"replication to a target that is not there", "photos", bucket.ConfigReplication, replicateDoc(t, "site-c", enabled), domain.ErrInvalidArgument},
		{"replication to a webhook", "photos", bucket.ConfigReplication, replicateDoc(t, "hook", enabled), domain.ErrInvalidArgument},
		{"replication to a NATS server", "photos", bucket.ConfigReplication, replicateDoc(t, "bus", enabled), domain.ErrInvalidArgument},
		{"replication to a target that does not admit the bucket", "closed", bucket.ConfigReplication, replicateDoc(t, "site-b", enabled), domain.ErrInvalidArgument},
		{"replication of a bucket that keeps no versions", "plain", bucket.ConfigReplication, replicateDoc(t, "site-b", enabled), domain.ErrInvalidBucketState},
	}
	for _, test := range refusals {
		m.refused5(test.name, configEntry(test.bucket, test.kind, test.document), test.want)
	}
	// In an entry that claims an older schema the two kinds are kinds there
	// are not, however good the document and whatever the state holds:
	// refused as the release of that schema refuses them.
	for _, schema := range []byte{entrySchemaTimed, entrySchema3, entrySchema4} {
		for name, e := range map[string]entry{
			"rules":                      configEntry("photos", bucket.ConfigNotification, valid),
			"a replication":              configEntry("photos", bucket.ConfigReplication, replicateDoc(t, "site-b", enabled)),
			"the removal of rules":       configEntry("photos", bucket.ConfigNotification, nil),
			"the removal of replication": configEntry("photos", bucket.ConfigReplication, nil),
		} {
			m.refused(fmt.Sprintf("%s in an entry of schema %d", name, schema), schema, e, domain.ErrInvalidArgument)
		}
	}
	// A refusal never says that a format is not supported: that stops a
	// state machine.
	if r := m.at5(configEntry("photos", bucket.ConfigNotification, newer)); errors.Is(r.err, domain.ErrUnsupportedFormat) || r.fatal {
		t.Errorf("a document of another version = %v", r.err)
	}
	if got := refs(); !reflect.DeepEqual(got, map[string]int{"hook": 0, "bus": 0, "site-b": 0}) {
		t.Fatalf("references after refusals only: %v", got)
	}

	steps := []struct {
		name     string
		bucket   string
		kind     string
		document []byte
		want     map[string]int
	}{
		{"rules are put", "photos", bucket.ConfigNotification, notifyDoc(t, rule("r", "hook"), rule("s", "hook"), rule("t", "bus")), map[string]int{"hook": 1, "bus": 1, "site-b": 0}},
		{"the same rules again", "photos", bucket.ConfigNotification, notifyDoc(t, rule("r", "hook"), rule("s", "hook"), rule("t", "bus")), map[string]int{"hook": 1, "bus": 1, "site-b": 0}},
		{"another bucket names one of them", "plain", bucket.ConfigNotification, notifyDoc(t, rule("r", "hook")), map[string]int{"hook": 2, "bus": 1, "site-b": 0}},
		{"rules replaced by rules for one target", "photos", bucket.ConfigNotification, notifyDoc(t, rule("t", "bus")), map[string]int{"hook": 1, "bus": 1, "site-b": 0}},
		{"replication is configured", "photos", bucket.ConfigReplication, replicateDoc(t, "site-b", enabled), map[string]int{"hook": 1, "bus": 1, "site-b": 1}},
		{"replication is configured again", "photos", bucket.ConfigReplication, replicateDoc(t, "site-b", bucket.ReplicationRule{ID: "logs", Enabled: true, Prefix: "logs/"}), map[string]int{"hook": 1, "bus": 1, "site-b": 1}},
		{"rules removed", "photos", bucket.ConfigNotification, nil, map[string]int{"hook": 1, "bus": 0, "site-b": 1}},
		{"rules removed that are not there", "photos", bucket.ConfigNotification, nil, map[string]int{"hook": 1, "bus": 0, "site-b": 1}},
		{"replication removed", "photos", bucket.ConfigReplication, nil, map[string]int{"hook": 1, "bus": 0, "site-b": 0}},
		{"both again", "photos", bucket.ConfigReplication, replicateDoc(t, "site-b", enabled), map[string]int{"hook": 1, "bus": 0, "site-b": 1}},
		{"and rules", "photos", bucket.ConfigNotification, notifyDoc(t, rule("r", "hook"), rule("t", "bus")), map[string]int{"hook": 2, "bus": 1, "site-b": 1}},
	}
	for _, step := range steps {
		r := m.ok5(configEntry(step.bucket, step.kind, step.document))
		if len(r.events) != 1 || r.events[0].Type != ports.EventBucketConfigChanged {
			t.Errorf("%s tells %+v", step.name, r.events)
		}
		if got := refs(); !reflect.DeepEqual(got, step.want) {
			t.Fatalf("%s: references %v, want %v", step.name, got, step.want)
		}
		var b bucketRecord
		if err := json.Unmarshal(storedBucket(t, m.f, step.bucket), &b); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b.Config[step.kind], step.document) {
			t.Fatalf("%s: the bucket holds %s", step.name, b.Config[step.kind])
		}
	}
	// A bucket that sends its versions on keeps them.
	m.refused5("suspending the versions of a bucket that is replicated",
		entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "photos", Status: string(bucket.VersioningSuspended)}}, domain.ErrInvalidBucketState)
	m.refused("suspending them in an entry of schema 2", entrySchemaTimed,
		entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "photos", Status: string(bucket.VersioningSuspended)}}, domain.ErrInvalidBucketState)
	m.ok5(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "photos", Status: string(bucket.VersioningEnabled)}})

	// The deletion of a bucket takes it out of the counts; a target that
	// nothing names any more can go.
	m.refused5("deleting a target a bucket names", entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: "site-b"}}, domain.ErrEntityInUse)
	m.ok5(entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "photos"}})
	if got := refs(); !reflect.DeepEqual(got, map[string]int{"hook": 1, "bus": 0, "site-b": 0}) {
		t.Fatalf("references after the bucket was deleted: %v", got)
	}
	m.ok5(entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: "site-b"}})
	m.ok5(entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "plain"}})
	if got := refs(); !reflect.DeepEqual(got, map[string]int{"hook": 0, "bus": 0}) {
		t.Fatalf("references after every bucket was deleted: %v", got)
	}
}

// TestABucketCreateCarriesNoDocumentThatNamesATarget: a bucket.create of
// schema 2 and above stores the configuration its entry carries, and no
// proposer writes one. One of the two kinds of schema 5 is never matched by
// and never counted: in a state of schema 5 it is not stored, and what a
// state of schema 4 took - byte for byte as the release of schema 4 takes it -
// appends no row and goes when the state becomes schema 5.
func TestABucketCreateCarriesNoDocumentThatNamesATarget(t *testing.T) {
	t.Parallel()
	planted := func(name string) entry {
		return entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: name, CreatedAt: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), Config: map[string][]byte{
			bucket.ConfigNotification: notifyDoc(t, bucket.NotificationRule{ID: "r", Target: "hook"}),
			bucket.ConfigReplication:  replicateDoc(t, "site-b", bucket.ReplicationRule{ID: "all", Enabled: true}),
			bucket.ConfigPolicy:       []byte(`{"Version":"2012-10-17"}`),
		}}}
	}
	config := func(m *machine, name string) map[string][]byte {
		t.Helper()
		var b bucketRecord
		if err := json.Unmarshal(storedBucket(t, m.f, name), &b); err != nil {
			t.Fatal(err)
		}
		return b.Config
	}
	for _, schema := range []byte{entrySchemaTimed, entrySchema3, entrySchema4} {
		m := quick5(t)
		if r := m.as(schema, planted("old")); r.err != nil {
			t.Fatal(r.err)
		}
		m.as(schema, entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "clean", CreatedAt: m.at, Config: map[string][]byte{bucket.ConfigPolicy: []byte("{}")}}})
		if got := config(m, "old"); len(got) != 3 {
			t.Fatalf("a bucket.create of schema %d stores %d documents; the release of that schema stores all three", schema, len(got))
		}
		clean := storedBucket(t, m.f, "clean")
		// While the state is below schema 5 the documents are bytes nobody
		// reads: a put appends no row, and versioning can be suspended.
		m.as(schema, entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "old", Status: string(bucket.VersioningEnabled)}})
		if r := m.as(schema, putEntry("old", "k", 5, 1)); r.err != nil || r.stored.Replication != "" {
			t.Fatalf("a put of schema %d into the bucket = %v, status %q", schema, r.err, r.stored.Replication)
		}
		if r := m.as(schema, entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "old", Status: string(bucket.VersioningSuspended)}}); r.err != nil {
			t.Fatalf("suspending the versions of the bucket in a state of schema %d = %v", schema, r.err)
		}
		if got := rawState(t, m.f, outboxBucket(), targetsBucket(), outboxCursorBucket()); len(got) != 0 || stateSchema(t, m.f) >= stateSchema5 {
			t.Fatalf("a state of schema %d holds %v", stateSchema(t, m.f), got)
		}
		// The first entry of schema 5 - refused, here - makes the state
		// schema 5, and the two documents go with it; nothing else does.
		if r := m.as(entrySchema5, entry{Type: cmdWalkBucketUsage, WalkBucketUsage: &usageWalkCmd{Bucket: "gone", Limit: 1}}); !errors.Is(r.err, domain.ErrBucketNotFound) {
			t.Fatalf("a walk of a bucket there is not = %v", r.err)
		}
		if got := config(m, "old"); len(got) != 1 || got[bucket.ConfigPolicy] == nil {
			t.Fatalf("after the first entry of schema 5 the bucket holds %v", got)
		}
		if again := storedBucket(t, m.f, "clean"); !bytes.Equal(again, clean) {
			t.Errorf("a bucket without such a document was written again: %s, was %s", again, clean)
		}
	}

	// In a state of schema 5, whatever schema the entry claims.
	for _, schema := range []byte{entrySchemaTimed, entrySchema4, entrySchema5} {
		m := quick5(t)
		m.bucket5("first", bucket.Unversioned)
		m.ok5(targetEntry("hook", delivery.KindWebhook))
		m.ok5(targetEntry("site-b", delivery.KindS3))
		if r := m.as(schema, planted("new")); r.err != nil {
			t.Fatal(r.err)
		}
		if got := config(m, "new"); len(got) != 1 || got[bucket.ConfigPolicy] == nil {
			t.Fatalf("a bucket.create of schema %d in a state of schema 5 stores %v", schema, got)
		}
		m.ok5(putEntry("new", "k", 5, 1))
		m.ok5(deleteEntry("new", "k", "", 2))
		m.ok5(entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "new"}})
		if rows := storedRows(t, m.f); len(rows) != 0 {
			t.Errorf("a document a bucket.create carried was matched by: %s", told(rows))
		}
		for name, record := range storedTargets(t, m.f) {
			if record.Refs != 0 {
				t.Errorf("target %s is counted %d times", name, record.Refs)
			}
		}
	}
}

// rowsScene is a state with two buckets that tell and send on: "photos"
// keeps versions, tells the webhook of everything, the NATS server of what
// arrives under "logs/", and sends everything on to another deployment, with
// delete markers; "plain" keeps none and tells the webhook.
func rowsScene(t *testing.T) *machine {
	t.Helper()
	m := quick5(t)
	m.bucket5("photos", bucket.VersioningEnabled)
	m.bucket5("plain", bucket.Unversioned)
	m.bucket5("quiet", bucket.VersioningEnabled)
	m.ok5(targetEntry("hook", delivery.KindWebhook))
	m.ok5(targetEntry("bus", delivery.KindNATS))
	m.ok5(targetEntry("site-b", delivery.KindS3))
	m.ok5(configEntry("photos", bucket.ConfigNotification, notifyDoc(t,
		bucket.NotificationRule{ID: "all", Target: "hook"},
		bucket.NotificationRule{ID: "logs", Target: "bus", Prefix: "logs/", Suffix: ".gz", On: []delivery.Event{delivery.EventPut, delivery.EventComplete}},
	)))
	m.ok5(configEntry("photos", bucket.ConfigReplication, replicateDoc(t, "site-b",
		bucket.ReplicationRule{ID: "all", Enabled: true, Markers: true},
		bucket.ReplicationRule{ID: "private", Enabled: true, Priority: 5, Prefix: "private/"},
		bucket.ReplicationRule{ID: "off", Priority: 9, Prefix: "logs/"},
	)))
	m.ok5(configEntry("plain", bucket.ConfigNotification, notifyDoc(t, bucket.NotificationRule{ID: "all", Target: "hook"})))
	return m
}

// TestEveryChangeOfAnObjectAppendsItsRow: each function of the state machine
// that changes an object appends one row, in the transaction of the change,
// for exactly the consumers the documents of the bucket give - whatever the
// schema byte of the entry - and a version that is sent on is stored as
// waiting to be, and counted so. What is refused, and what changes no
// object, appends nothing.
func TestEveryChangeOfAnObjectAppendsItsRow(t *testing.T) {
	t.Parallel()
	m := rowsScene(t)
	// next applies an entry written at a level and returns the rows it
	// appended.
	next := func(level byte, e entry) (applyResult, []delivery.Item) {
		t.Helper()
		before := len(storedRows(t, m.f))
		held := heldTo(e, level)
		r := m.as(max(writtenAt(held, versionOf(held), level), entrySchemaTimed), held)
		if r.err != nil {
			t.Fatalf("%s: %v", e.Type, r.err)
		}
		rows := storedRows(t, m.f)
		if count, _ := outboxCounts(t, m.f); count != uint64(len(rows)) {
			t.Fatalf("the outbox counts %d rows and holds %d", count, len(rows))
		}
		for _, name := range []string{"photos", "plain", "quiet"} {
			checkUsage(t, m.f, name)
		}
		return r, rows[before:]
	}
	one := func(what string, level byte, e entry, event delivery.Event, by string, to ...string) (applyResult, delivery.Item) {
		t.Helper()
		r, rows := next(level, e)
		if len(rows) != 1 {
			t.Fatalf("%s appended %d rows (%s), want one", what, len(rows), told(rows))
		}
		row := rows[0]
		var got []string
		for _, consumer := range row.To {
			got = append(got, consumer.String())
		}
		if row.Seq != (delivery.Seq{Index: m.index}) || row.Event != event || row.By != by || !slices.Equal(got, to) || !row.At.Equal(m.at) {
			t.Fatalf("%s: the row is %+v at %s, want %s by %q for %v at entry %d, %s", what, row, row.At, event, by, to, m.index, m.at)
		}
		return r, row
	}
	none := func(what string, level byte, e entry) applyResult {
		t.Helper()
		r, rows := next(level, e)
		if len(rows) != 0 {
			t.Fatalf("%s appended %s", what, told(rows))
		}
		return r
	}
	pending := func(want int64, failed int64) {
		t.Helper()
		if usage, _, _ := usageOf(t, m.f, "photos"); usage.ReplicationPending != want || usage.ReplicationFailed != failed {
			t.Fatalf("photos counts %d versions waiting and %d failed, want %d and %d", usage.ReplicationPending, usage.ReplicationFailed, want, failed)
		}
	}
	withBy := func(e entry, by string) entry {
		if e.PutObject != nil {
			e.PutObject.CreatedBy = by
		}
		if e.DeleteVersion != nil {
			e.DeleteVersion.By = by
		}
		return e
	}

	// A put: told to the webhook, sent on, and waiting.
	r, row := one("a put", entrySchema5, withBy(putEntry("photos", "a.jpg", 1024, 1), "user/albie"), delivery.EventPut, "user/albie", "n:hook", "r:site-b")
	first := *r.stored
	if row.Bucket != "photos" || row.Key != "a.jpg" || row.Version.String() != first.VersionID || row.Size != 1024 || row.ETag != first.ETag {
		t.Errorf("the row of a put: %+v, the version %+v", row, first)
	}
	if first.Replication != string(object.ReplicationPending) || statuses(t, m.f, "photos")["a.jpg@"+first.VersionID] != "PENDING" {
		t.Errorf("a version that is sent on is stored as %q", first.Replication)
	}
	pending(1, 0)
	// The row is its record and nothing else: no metadata, no tags, no
	// contents.
	raw := rawState(t, m.f, outboxBucket())[fmt.Sprintf("outbox/%x", row.Seq.Key())]
	wantRaw := fmt.Sprintf(`{"v":1,"e":"put","b":"photos","k":"a.jpg","vid":%q,"sz":1024,"et":%q,"at":%q,"by":"user/albie","to":["n:hook","r:site-b"]}`,
		first.VersionID, first.ETag, m.at.Format(time.RFC3339Nano))
	if raw != wantRaw {
		t.Errorf("the stored row is\n%s\nwant\n%s", raw, wantRaw)
	}

	// Rules that match by prefix and suffix, byte for byte; the rule of the
	// greatest priority decides what is sent on, and one that is not
	// enabled decides nothing.
	one("a put under logs/", entrySchema5, putEntry("photos", "logs/a.gz", 10, 2), delivery.EventPut, "", "n:bus", "n:hook", "r:site-b")
	one("a put under logs/ with another ending", entrySchema5, putEntry("photos", "logs/a.txt", 10, 3), delivery.EventPut, "", "n:hook", "r:site-b")
	pending(3, 0)

	// Whatever the schema byte of the entry: a proposer that learnt of the
	// level late writes a put as schema 4, and one of schema 2 says nobody.
	one("a put of schema 4", entrySchema4, withBy(putEntry("photos", "b", 7, 4), "user/albie"), delivery.EventPut, "user/albie", "n:hook", "r:site-b")
	one("a put of schema 2", entrySchemaTimed, withBy(putEntry("photos", "b", 8, 5), "user/albie"), delivery.EventPut, "", "n:hook", "r:site-b")
	pending(5, 0)

	// A size below nothing is told as nothing, as it is counted.
	_, row = one("a put of a size below nothing", entrySchema5, putEntry("photos", "odd", -600, 6), delivery.EventPut, "", "n:hook", "r:site-b")
	if row.Size != 0 {
		t.Errorf("a version of -600 bytes is told as %d", row.Size)
	}
	pending(6, 0)

	// The completion of an upload: its own kind of change, by who completed.
	upload, parts, entries := uploadEntries("photos", "logs/big.gz", 10, object.MinPartSize, 300)
	for _, e := range entries {
		none("the creation of an upload and its parts", entrySchema5, e)
	}
	r, row = one("a completion", entrySchema5, completeEntry(upload, 11, parts...), delivery.EventComplete, "root", "n:bus", "n:hook", "r:site-b")
	if row.Size != object.MinPartSize+300 || row.Version.String() != r.stored.VersionID || r.stored.Replication != "PENDING" {
		t.Errorf("the row of a completion: %+v", row)
	}
	pending(7, 0)
	other, _, entries := uploadEntries("photos", "aborted", 12, 100)
	for _, e := range entries {
		none("an upload", entrySchema5, e)
	}
	none("the abort of an upload", entrySchema5, abortEntry(other))

	// A delete that leaves a marker: told, and sent on where the deciding
	// rule sends markers - but never the marker of the lifecycle service.
	r, row = one("a delete marker", entrySchema5, withBy(deleteEntry("photos", "a.jpg", "", 20), "user/albie"), delivery.EventMarker, "user/albie", "n:hook", "r:site-b")
	if row.Version.String() != r.marker.VersionID || row.Size != 0 || row.ETag != "" || r.marker.Replication != "PENDING" {
		t.Errorf("the row of a marker: %+v, the marker %+v", row, r.marker)
	}
	pending(8, 0)
	r, _ = one("a marker of the lifecycle service", entrySchema5, withBy(deleteEntry("photos", "b", "", 21), bucket.LifecycleActor), delivery.EventMarker, bucket.LifecycleActor, "n:hook")
	if r.marker.Replication != "" {
		t.Errorf("a marker of the lifecycle service is stored as %q", r.marker.Replication)
	}
	one("a put where the rule sends no markers", entrySchema5, putEntry("photos", "private/x", 3, 22), delivery.EventPut, "", "n:hook", "r:site-b")
	r, _ = one("a marker where the rule sends none", entrySchema5, deleteEntry("photos", "private/x", "", 23), delivery.EventMarker, "", "n:hook")
	if r.marker.Replication != "" {
		t.Errorf("a marker the rule does not send is stored as %q", r.marker.Replication)
	}
	// A marker whose ID is all zeros - which no proposer writes - is refused
	// in an entry of schema 5: it would share its place with the null
	// version of its key. Below schema 5 the delete is applied as the
	// release of that schema applies it, and its marker cannot be named in a
	// row: none is appended, by every member alike, and the marker does not
	// wait to be sent on by a row that is not there.
	zero := func() entry {
		e := deleteEntry("photos", "odd/marker", "", 26)
		e.DeleteVersion.MarkerID = object.ID{}.String()
		return e
	}
	m.refused5("a marker without an ID, by somebody", withBy(zero(), "user/albie"), domain.ErrInvalidArgument)
	m.refused("a marker without an ID, in an entry of schema 5", entrySchema5, zero(), domain.ErrInvalidArgument)
	if r := none("a marker without an ID, below schema 5", entrySchema5, zero()); r.marker == nil || r.marker.Replication != "" {
		t.Fatalf("the marker of an entry below schema 5: %+v", r.marker)
	}
	pending(9, 0)
	// A delete of schema 4 says nobody: who deleted is left out below 5.
	one("a marker of schema 4", entrySchema4, withBy(deleteEntry("photos", "odd", "", 24), "user/albie"), delivery.EventMarker, "", "n:hook", "r:site-b")
	pending(10, 0)

	// Later changes of a version that is being sent on are sent after it,
	// by nobody; of a version without a status, and of any version to a
	// notification target, they are not told.
	ref := first.VersionID
	one("tags", entrySchema5, entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: "photos", Key: "a.jpg", VersionRef: ref, Tags: map[string]string{"a": "b"}}},
		delivery.EventTags, "", "r:site-b")
	one("tags of schema 2", entrySchemaTimed, entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: "photos", Key: "a.jpg", VersionRef: ref}},
		delivery.EventTags, "", "r:site-b")
	one("a put into a bucket that sends nothing on", entrySchema5, putEntry("plain", "tagged", 5, 25), delivery.EventPut, "", "n:hook")
	none("the tags of a version that is not sent on", entrySchema5,
		entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: "plain", Key: "tagged", VersionRef: object.NullVersion, Tags: map[string]string{"a": "b"}}})
	pending(10, 0)

	// A version removed for good is told and never sent on; one that was
	// waiting is no longer counted as waiting.
	_, row = one("a version removed for good", entrySchema5, withBy(deleteEntry("photos", "a.jpg", ref, 30), "user/albie"), delivery.EventDelete, "user/albie", "n:hook")
	if row.Version.String() != ref || row.Size != 1024 {
		t.Errorf("the row of a removal: %+v", row)
	}
	pending(9, 0)
	_, row = one("a marker removed for good", entrySchema5, deleteEntry("photos", "odd", serialID(24).String(), 31), delivery.EventDelete, "", "n:hook")
	if row.Size != 0 || row.ETag != "" {
		t.Errorf("the row of a marker that was removed: %+v", row)
	}
	pending(8, 0)
	none("a delete of a version that is not there", entrySchema5, deleteEntry("photos", "a.jpg", ref, 32))

	// A bucket that keeps no versions: a put that replaces is one row, a
	// delete is a removal, and so is the delete of schema 1.
	_, row = one("a put into a bucket without versions", entrySchema5, putEntry("plain", "k", 5, 40), delivery.EventPut, "", "n:hook")
	if row.Version != (object.VersionRef{Null: true}) {
		t.Errorf("the row of a bucket that keeps no versions names %s", row.Version)
	}
	one("a put that replaces", entrySchema5, putEntry("plain", "k", 6, 41), delivery.EventPut, "", "n:hook")
	one("a delete", entrySchema5, withBy(deleteEntry("plain", "k", "", 42), "user/albie"), delivery.EventDelete, "user/albie", "n:hook")
	none("a delete of nothing", entrySchema5, deleteEntry("plain", "k", "", 43))
	one("a put", entrySchema5, putEntry("plain", "old", 5, 44), delivery.EventPut, "", "n:hook")
	before := len(storedRows(t, m.f))
	at := m.at
	if r := m.as(entrySchemaVersion, entry{Type: cmdDeleteObject, DeleteObject: &deleteObjectCmd{Bucket: "plain", Key: "old"}}); r.err != nil {
		t.Fatal(r.err)
	}
	rows := storedRows(t, m.f)[before:]
	// An entry of the first schema has no time: its row has that of the
	// newest entry that had one.
	if len(rows) != 1 || rows[0].Event != delivery.EventDelete || rows[0].Key != "old" || rows[0].By != "" || !rows[0].At.Equal(at) || rows[0].Seq.Index != m.index {
		t.Errorf("the delete of schema 1 appended %+v", rows)
	}

	// A bucket without a document, and what is refused, append nothing.
	none("a put into a bucket nobody is told of", entrySchema5, putEntry("quiet", "k", 5, 50))
	none("a marker there", entrySchema5, deleteEntry("quiet", "k", "", 51))
	m.refused5("a put of data that is taken", putEntry("photos", "logs/a.gz", 10, 2), domain.ErrConflict)
	m.refused5("a put into a bucket there is not", putEntry("gone", "k", 5, 52), domain.ErrBucketNotFound)

	// A bucket that pauses its versions cannot be replicated; one without
	// replication replaces its null version with one row.
	m.ok5(configEntry("photos", bucket.ConfigReplication, nil))
	m.ok5(entry{Type: cmdSetVersioning, SetVersioning: &versioningCmd{Bucket: "photos", Status: string(bucket.VersioningSuspended)}})
	one("a put while versions are paused", entrySchema5, putEntry("photos", "paused", 5, 60), delivery.EventPut, "", "n:hook")
	one("a put that replaces the null version", entrySchema5, putEntry("photos", "paused", 6, 61), delivery.EventPut, "", "n:hook")
	one("a marker that replaces the null version", entrySchema5, deleteEntry("photos", "paused", "", 62), delivery.EventMarker, "", "n:hook")
	// What was waiting stays waiting: the status is stored, not derived.
	pending(8, 0)

	// A name of an actor that cannot be kept is left out of the row.
	one("a put by a name of two lines", entrySchema5, withBy(putEntry("plain", "nobody", 1, 70), "a\nb"), delivery.EventPut, "", "n:hook")
}

// TestAnAcknowledgementIsAppliedInItsOrder: outbox.ack as ADR-0023 lists it -
// refused whole; an acknowledgement beyond the newest row counts as that row;
// the cursor moves forward only; the audit trail is acknowledged for a target
// that is sent it; a result gives its status to the version it names, by row
// or by version, and moves the counts of the bucket; then the rows every
// target has passed go.
func TestAnAcknowledgementIsAppliedInItsOrder(t *testing.T) {
	t.Parallel()
	m := rowsScene(t)
	m.ok5(entry{Type: cmdPutTarget, PutTarget: &targetCmd{Target: func() targetRecord {
		target := porttest.SampleTarget("trail", delivery.KindWebhook)
		target.Audit = true
		return targetToRecord(target)
	}()}})
	puts := map[string]versionRecord{}
	place := map[string]delivery.Seq{}
	for i, key := range []string{"a", "b", "c", "d", "e"} {
		puts[key] = *m.ok5(putEntry("photos", key, int64(100+i), i+1)).stored
		place[key] = delivery.Seq{Index: m.index}
	}
	plain := *m.ok5(putEntry("plain", "p", 5, 9)).stored
	place["p"] = delivery.Seq{Index: m.index}
	quiet := *m.ok5(putEntry("quiet", "q", 5, 10)).stored
	last := place["p"]
	usage := func() (pending, failed int64) {
		t.Helper()
		u := checkUsage(t, m.f, "photos")
		return u.ReplicationPending, u.ReplicationFailed
	}
	if pending, failed := usage(); pending != 5 || failed != 0 || len(storedRows(t, m.f)) != 6 {
		t.Fatalf("the scene: %d waiting, %d failed, %d rows", pending, failed, len(storedRows(t, m.f)))
	}
	completed := func(seq delivery.Seq) delivery.Result {
		return delivery.Result{Seq: seq, Status: object.ReplicationCompleted}
	}
	byVersion := func(bucketName string, v versionRecord, status object.ReplicationStatus) delivery.Result {
		ref, err := object.ParseVersionRef(refOf(v))
		if err != nil {
			t.Fatal(err)
		}
		return delivery.Result{Bucket: bucketName, Key: v.Key, Version: ref, Status: status}
	}

	// Refused whole, with nothing changed.
	many := make([]delivery.Result, delivery.MaxAckResults+1)
	for i := range many {
		many[i] = completed(place["a"])
	}
	raw := func(target, upto string, results ...ackResult) entry {
		return entry{Type: cmdAckOutbox, AckOutbox: &ackOutboxCmd{Target: target, Upto: upto, AuditUpto: 9, Results: results}}
	}
	for name, test := range map[string]struct {
		e    entry
		want error
	}{
		"a target that is not there":            {ackEntry("gone", last), domain.ErrEntityNotFound},
		"a target that is not there, with more": {ackEntry("gone", last, many...), domain.ErrEntityNotFound},
		"more results than one entry carries":   {ackEntry("site-b", last, many...), domain.ErrInvalidArgument},
		"a result from a webhook":               {ackEntry("hook", last, completed(place["a"])), domain.ErrInvalidArgument},
		"a result by version from a NATS server": {
			ackEntry("bus", last, byVersion("photos", puts["a"], object.ReplicationCompleted)), domain.ErrInvalidArgument,
		},
		"a place that is none":            {raw("site-b", "zz"), domain.ErrInvalidArgument},
		"a place in capitals":             {raw("site-b", strings.ToUpper(delivery.Seq{Index: 0xab}.String())), domain.ErrInvalidArgument},
		"no place at all":                 {raw("hook", ""), domain.ErrInvalidArgument},
		"a result that names a row badly": {raw("site-b", last.String(), ackResult{Seq: "0c", Status: "COMPLETED"}), domain.ErrInvalidArgument},
		"a result that names the place before every row": {
			raw("site-b", last.String(), ackResult{Seq: delivery.Seq{}.String(), Status: "COMPLETED"}), domain.ErrInvalidArgument,
		},
		"a result that names a row and a version": {
			raw("site-b", last.String(), ackResult{Seq: place["a"].String(), Bucket: "photos", Key: "a", VersionRef: "null", Status: "COMPLETED"}), domain.ErrInvalidArgument,
		},
		"a result that names nothing":       {raw("site-b", last.String(), ackResult{Status: "COMPLETED"}), domain.ErrInvalidArgument},
		"a result of a status there is not": {raw("site-b", last.String(), ackResult{Seq: place["a"].String(), Status: "PENDING"}), domain.ErrInvalidArgument},
		"a result that says REPLICA":        {raw("site-b", last.String(), ackResult{Bucket: "photos", Key: "a", VersionRef: "null", Status: "REPLICA"}), domain.ErrInvalidArgument},
		"a result of a version that is none": {
			raw("site-b", last.String(), ackResult{Bucket: "photos", Key: "a", VersionRef: "latest", Status: "FAILED"}), domain.ErrInvalidArgument,
		},
		"one result that is refused among good ones": {
			raw("site-b", last.String(), resultToRecord(completed(place["a"])), ackResult{Seq: place["b"].String(), Status: "DONE"}), domain.ErrInvalidArgument,
		},
	} {
		m.refused5(name, test.e, test.want)
	}

	// As many results as one entry carries are taken: here they all say
	// what is so already, or name nothing.
	before := settled(t, m.f)
	m.ok5(ackEntry("site-b", delivery.Seq{}, many[:delivery.MaxAckResults]...))
	if after := settled(t, m.f); !reflect.DeepEqual(after, before) {
		for key := range after {
			if after[key] != before[key] && !strings.HasPrefix(key, "objects/photos/") && !strings.HasPrefix(key, "bucketusage/") {
				t.Errorf("%d results for one version changed %s", delivery.MaxAckResults, key)
			}
		}
	}
	if got := statuses(t, m.f, "photos")["a@"+puts["a"].VersionID]; got != "COMPLETED" {
		t.Fatalf("after %d results that say so, the version is %q", delivery.MaxAckResults, got)
	}
	// Back to waiting, for what follows: by the hand of the test.
	m.ok5(ackEntry("site-b", delivery.Seq{}, delivery.Result{Seq: place["a"], Status: object.ReplicationFailed}))
	setStatus(t, m.f, "photos", "a", string(object.ReplicationPending))

	// The cursor moves forward only, and never past what was written.
	cursor := func(name string) delivery.Cursor { t.Helper(); return storedCursors(t, m.f)[name] }
	m.ok5(ackEntry("site-b", place["b"]))
	if got := cursor("site-b"); got.Acked != place["b"] || got.AuditAcked != 0 {
		t.Fatalf("after an acknowledgement up to b: %+v", got)
	}
	before = settled(t, m.f)
	m.ok5(ackEntry("site-b", place["a"]))
	m.ok5(ackEntry("site-b", delivery.Seq{}))
	if after := settled(t, m.f); !reflect.DeepEqual(after, before) {
		t.Errorf("an acknowledgement behind the cursor changed the state")
	}
	m.ok5(ackEntry("site-b", delivery.Seq{Index: 1 << 50, Row: 7}))
	if got := cursor("site-b"); got.Acked != last {
		t.Errorf("an acknowledgement beyond the newest row moved the cursor to %s, the newest row is %s", got.Acked, last)
	}
	// Between two rows it stands where it was said.
	between := delivery.Seq{Index: place["c"].Index, Row: 9}
	m.ok5(ackEntry("bus", between))
	if got := cursor("bus"); got.Acked != between {
		t.Errorf("an acknowledgement between two rows = %s, want %s", got.Acked, between)
	}

	// The audit trail is acknowledged for a target that is sent it, and for
	// no other; and it never goes back.
	audited := func(target string, upto uint64) {
		t.Helper()
		m.ok5(entry{Type: cmdAckOutbox, AckOutbox: &ackOutboxCmd{Target: target, Upto: delivery.Seq{}.String(), AuditUpto: upto}})
	}
	audited("hook", 40)
	audited("trail", 40)
	audited("trail", 12)
	if hook, trail := cursor("hook"), cursor("trail"); hook.AuditAcked != 0 || trail.AuditAcked != 40 || hook.Acked != hook.From || trail.Acked != trail.From {
		t.Errorf("the audit trail acknowledged: hook %+v, trail %+v", hook, trail)
	}

	// Results by row: of a row that is still there and was for this target
	// as replication.
	status := func(v versionRecord) string { t.Helper(); return statuses(t, m.f, v.Bucket)[v.Key+"@"+refOf(v)] }
	m.ok5(ackEntry("site-b", last, completed(place["a"]), delivery.Result{Seq: place["b"], Status: object.ReplicationFailed}))
	if status(puts["a"]) != "COMPLETED" || status(puts["b"]) != "FAILED" || status(puts["c"]) != "PENDING" {
		t.Fatalf("after results by row: %v", statuses(t, m.f, "photos"))
	}
	if pending, failed := usage(); pending != 3 || failed != 1 {
		t.Fatalf("after one completed and one failed: %d waiting, %d failed", pending, failed)
	}
	// A row that was for a notification target only, a place where no row
	// is, and a version that is gone change nothing.
	m.ok5(deleteEntry("photos", "e", puts["e"].VersionID, 20))
	before = settled(t, m.f)
	r := m.ok5(ackEntry("site-b", last,
		completed(place["p"]), completed(delivery.Seq{Index: place["c"].Index, Row: 3}), completed(delivery.Seq{Index: 1 << 40}),
		completed(place["e"]), byVersion("photos", puts["e"], object.ReplicationFailed),
		byVersion("gone", puts["c"], object.ReplicationFailed), delivery.Result{Bucket: "photos", Key: "never", Version: object.VersionRef{Null: true}, Status: object.ReplicationFailed},
		// A result that says what the version says already.
		completed(place["a"]), byVersion("photos", puts["b"], object.ReplicationFailed),
	))
	if after := settled(t, m.f); !reflect.DeepEqual(after, before) {
		t.Errorf("results that name what is gone changed the state")
	}
	// Two of them were not the target's to give - the row of a notification,
	// and the version of a bucket that is not there - and the answer says so;
	// what is gone is not counted.
	if r.unrelated != 2 {
		t.Errorf("%d results are answered as not the target's, want 2", r.unrelated)
	}
	if pending, failed := usage(); pending != 2 || failed != 1 {
		t.Fatalf("after a waiting version was removed: %d waiting, %d failed", pending, failed)
	}

	// Results by version: of a version of a bucket whose replication
	// configuration names the target - also one no row tells of, which is
	// what a resync sent. Every move of a status moves the counts. The
	// versions of buckets that are not sent to the target are not its to
	// speak of: they keep what they had, the acknowledgement is taken all
	// the same, and the answer counts them.
	r = m.ok5(ackEntry("site-b", last,
		byVersion("photos", puts["b"], object.ReplicationCompleted), // FAILED -> COMPLETED
		byVersion("photos", puts["a"], object.ReplicationFailed),    // COMPLETED -> FAILED
		byVersion("photos", puts["c"], object.ReplicationFailed),    // PENDING -> FAILED
		byVersion("photos", puts["c"], object.ReplicationCompleted), // and on, in one entry
		byVersion("quiet", quiet, object.ReplicationCompleted),      // a bucket without a document
		byVersion("plain", plain, object.ReplicationFailed),         // one that is told, and not sent on
	))
	if status(puts["a"]) != "FAILED" || status(puts["b"]) != "COMPLETED" || status(puts["c"]) != "COMPLETED" || status(puts["d"]) != "PENDING" ||
		status(quiet) != "" || status(plain) != "" || r.unrelated != 2 {
		t.Fatalf("after results by version: %v %v %v, %d not the target's", statuses(t, m.f, "photos"), statuses(t, m.f, "quiet"), statuses(t, m.f, "plain"), r.unrelated)
	}
	if pending, failed := usage(); pending != 1 || failed != 1 {
		t.Fatalf("after results by version: %d waiting, %d failed", pending, failed)
	}
	if u := checkUsage(t, m.f, "plain"); u.ReplicationFailed != 0 || u.ReplicationPending != 0 {
		t.Errorf("plain counts %+v", u.UsageTotals)
	}
	if u := checkUsage(t, m.f, "quiet"); u.ReplicationFailed != 0 || u.ReplicationPending != 0 {
		t.Errorf("quiet counts %+v", u.UsageTotals)
	}
	// A later change of a version that was sent on is sent after it, and
	// of one that failed too.
	if rows := len(storedRows(t, m.f)); rows != 7 {
		t.Fatalf("%d rows", rows)
	}
	m.ok5(entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: "photos", Key: "b", VersionRef: puts["b"].VersionID, Tags: map[string]string{"x": "y"}}})
	if rows := storedRows(t, m.f); len(rows) != 8 || told(rows[7:]) != "tags b>r:site-b" {
		t.Fatalf("the tags of a version that was sent on: %s", told(rows))
	}

	// Rows every target has passed go: nothing while one target stands at
	// its beginning, then all at or below the lowest cursor.
	if rows, _ := outboxCounts(t, m.f); rows != 8 {
		t.Fatalf("the outbox counts %d rows", rows)
	}
	m.ok5(ackEntry("hook", place["c"]))
	m.ok5(ackEntry("trail", place["d"]))
	m.ok5(ackEntry("bus", place["d"]))
	m.ok5(ackEntry("site-b", delivery.Seq{Index: 1 << 50}))
	// The lowest cursor is the webhook's, at c: a, b and c go.
	if rows := storedRows(t, m.f); told(rows) != "put d>n:hook,r:site-b; put e>n:hook,r:site-b; put p>n:hook; delete e>n:hook; tags b>r:site-b" {
		t.Fatalf("after every target passed c: %s", told(rows))
	}
	if rows, dropped := outboxCounts(t, m.f); rows != 5 || dropped != 0 {
		t.Errorf("the outbox counts %d rows and %d dropped", rows, dropped)
	}
	// A result by a row that was trimmed names nothing any more.
	m.ok5(ackEntry("site-b", last, delivery.Result{Seq: place["a"], Status: object.ReplicationCompleted}))
	if status(puts["a"]) != "FAILED" {
		t.Errorf("a result by a row that is gone moved a status to %s", status(puts["a"]))
	}
	for _, name := range []string{"hook", "trail", "bus", "site-b"} {
		m.ok5(ackEntry(name, delivery.Seq{Index: 1 << 50}))
	}
	if rows, _ := outboxCounts(t, m.f); rows != 0 || len(storedRows(t, m.f)) != 0 {
		t.Errorf("after every target passed everything the outbox holds %d rows", rows)
	}
	// An acknowledgement of an empty outbox counts as the cursor's own
	// place.
	before = settled(t, m.f)
	m.ok5(ackEntry("hook", delivery.Seq{Index: 1 << 60}))
	if after := settled(t, m.f); !reflect.DeepEqual(after, before) {
		t.Errorf("an acknowledgement of an empty outbox changed the state")
	}
}

// TestAnAcknowledgementTrimsSoManyRows: one acknowledgement removes at most
// delivery.MaxOutboxTrim rows, the oldest, and the next goes on; a status is
// kept while the counts of a bucket are being counted.
func TestAnAcknowledgementTrimsSoManyRows(t *testing.T) {
	t.Parallel()
	m := quick5(t)
	m.bucket5("plain", bucket.Unversioned)
	m.ok5(targetEntry("hook", delivery.KindWebhook))
	m.ok5(configEntry("plain", bucket.ConfigNotification, notifyDoc(t, bucket.NotificationRule{ID: "all", Target: "hook"})))
	const rows = delivery.MaxOutboxTrim + 150
	for i := range rows {
		m.ok5(putEntry("plain", fmt.Sprintf("k%04d", i), 1, i))
	}
	beyond := delivery.Seq{Index: 1 << 50}
	m.ok5(ackEntry("hook", beyond))
	left := storedRows(t, m.f)
	if count, _ := outboxCounts(t, m.f); count != 150 || len(left) != 150 || left[0].Key != fmt.Sprintf("k%04d", delivery.MaxOutboxTrim) {
		t.Fatalf("after one acknowledgement of %d rows: %d left, the outbox counts %d", rows, len(left), count)
	}
	m.ok5(ackEntry("hook", beyond))
	if count, _ := outboxCounts(t, m.f); count != 0 || len(storedRows(t, m.f)) != 0 {
		t.Errorf("after the second: the outbox counts %d rows", count)
	}
}

// TestTheOutboxIsBounded: at the bound the oldest row goes, the count of
// dropped rows rises, and the row is lost to exactly the consumers that had
// not passed it - with a resync needed for one that was to send it on. A
// target that takes nothing never refuses a write and never grows the state.
func TestTheOutboxIsBounded(t *testing.T) {
	t.Parallel()
	const bound = 6
	m := quick5(t)
	setOutboxBound(m.f, bound)
	m.bucket5("photos", bucket.VersioningEnabled)
	for _, name := range []string{"dead", "live", "gone", "left"} {
		m.ok5(targetEntry(name, delivery.KindWebhook))
	}
	m.ok5(targetEntry("site-b", delivery.KindS3))
	m.ok5(configEntry("photos", bucket.ConfigNotification, notifyDoc(t,
		bucket.NotificationRule{ID: "dead", Target: "dead"},
		bucket.NotificationRule{ID: "gone", Target: "gone", Prefix: "live/"},
		bucket.NotificationRule{ID: "left", Target: "left", Prefix: "live/"},
		bucket.NotificationRule{ID: "live", Target: "live", Prefix: "live/"},
	)))
	m.ok5(configEntry("photos", bucket.ConfigReplication, replicateDoc(t, "site-b", bucket.ReplicationRule{ID: "r", Enabled: true, Prefix: "live/"})))
	serial := 0
	put := func(key string) delivery.Seq {
		t.Helper()
		serial++
		m.ok5(putEntry("photos", key, 1, serial))
		return delivery.Seq{Index: m.index}
	}
	type lost struct {
		lost   uint64
		resync bool
	}
	check := func(what string, rows, dropped uint64, want map[string]lost) {
		t.Helper()
		gotRows, gotDropped := outboxCounts(t, m.f)
		if gotRows != rows || gotDropped != dropped || uint64(len(storedRows(t, m.f))) != rows {
			t.Fatalf("%s: the outbox counts %d rows and %d dropped and holds %d; want %d and %d", what, gotRows, gotDropped, len(storedRows(t, m.f)), rows, dropped)
		}
		cursors := storedCursors(t, m.f)
		if len(cursors) != 4 || len(storedTargets(t, m.f)) != 4 {
			t.Fatalf("%s: %d cursors and %d targets, want those of dead, live, gone and site-b", what, len(cursors), len(storedTargets(t, m.f)))
		}
		for name, cursor := range cursors {
			if got := (lost{cursor.Lost, cursor.ResyncNeeded}); got != want[name] {
				t.Fatalf("%s: target %s has lost %d rows, resync needed %t; want %+v", what, name, cursor.Lost, cursor.ResyncNeeded, want[name])
			}
		}
	}
	first, second := put("live/1"), put("live/2")
	for i := 2; i < bound; i++ {
		put(fmt.Sprintf("other/%d", i))
	}
	if rows := storedRows(t, m.f); told(rows[:1]) != "put live/1>n:dead,n:gone,n:left,n:live,r:site-b" {
		t.Fatalf("the first row: %s", told(rows[:1]))
	}
	// The live target has passed the first row, the other deployment both;
	// "gone" is deleted and defined again: nothing of before is its own. And
	// "left" is deleted for good: a row that is dropped is lost to nobody
	// that is not there, and makes no cursor.
	m.ok5(ackEntry("live", first))
	m.ok5(ackEntry("site-b", second))
	m.ok5(configEntry("photos", bucket.ConfigNotification, notifyDoc(t,
		bucket.NotificationRule{ID: "dead", Target: "dead"}, bucket.NotificationRule{ID: "live", Target: "live", Prefix: "live/"},
	)))
	m.ok5(entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: "gone"}})
	m.ok5(entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: "left"}})
	m.ok5(targetEntry("gone", delivery.KindWebhook))
	check("before the first drop", bound, 0, nil)

	// One row more: the first goes. It is lost to the dead target alone -
	// the live one and the other deployment had passed it.
	put("other/x")
	check("after one drop", bound, 1, map[string]lost{"dead": {1, false}})
	// The second: lost to the dead target and to the live one, which had
	// not passed it; the other deployment had.
	put("other/y")
	check("after two drops", bound, 2, map[string]lost{"dead": {2, false}, "live": {1, false}})
	// A row for the other deployment that it has not passed: a resync is
	// needed.
	third := put("live/3")
	for range bound - 1 {
		put("other/z")
	}
	check("before the row for the other deployment goes", bound, uint64(3+bound-1), map[string]lost{"dead": {uint64(2 + bound), false}, "live": {1, false}})
	put("other/last")
	check("after it went", bound, uint64(3+bound), map[string]lost{"dead": {uint64(3 + bound), false}, "live": {2, false}, "site-b": {1, true}})
	if rows := storedRows(t, m.f); rows[0].Seq.Compare(third) <= 0 {
		t.Errorf("the oldest row is %s, the one that went was %s", rows[0].Seq, third)
	}
	// The version whose row was lost is still waiting, and a result that
	// names it by itself gives it its status: that is what a resync does.
	var waiting versionRecord
	for _, v := range everyVersion(t, m.f, "photos") {
		if v.Key == "live/3" {
			waiting = v
		}
	}
	if waiting.Replication != "PENDING" {
		t.Fatalf("the version whose row was dropped is %q", waiting.Replication)
	}
	ref, _ := object.ParseVersionRef(waiting.VersionID)
	m.ok5(ackEntry("site-b", delivery.Seq{}, delivery.Result{Seq: third, Status: object.ReplicationCompleted}))
	if statuses(t, m.f, "photos")["live/3@"+waiting.VersionID] != "PENDING" {
		t.Errorf("a result by a row that was dropped moved the status")
	}
	m.ok5(ackEntry("site-b", delivery.Seq{}, delivery.Result{Bucket: "photos", Key: "live/3", Version: ref, Status: object.ReplicationCompleted}))
	if statuses(t, m.f, "photos")["live/3@"+waiting.VersionID] != "COMPLETED" {
		t.Errorf("a result by version did not move the status of a version whose row was dropped")
	}
	// A resync that is done clears the mark of its target, and no other
	// count.
	m.ok5(entry{Type: cmdPutResync, PutResync: resyncToRecord(bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning})})
	check("while a resync runs", bound, uint64(3+bound), map[string]lost{"dead": {uint64(3 + bound), false}, "live": {2, false}, "site-b": {1, true}})
	m.ok5(entry{Type: cmdPutResync, PutResync: resyncToRecord(bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncDone})})
	check("after a resync", bound, uint64(3+bound), map[string]lost{"dead": {uint64(3 + bound), false}, "live": {2, false}, "site-b": {1, false}})
	checkUsage(t, m.f, "photos")
}

// TestWhatABucketAndATargetLeaveBehind: a bucket that is deleted leaves its
// rows to their targets and costs its deletion nothing of the outbox; a
// target that is deleted leaves its rows to be cut as all rows are - and its
// deletion cuts as an acknowledgement does, at most delivery.MaxOutboxTrim
// rows, since the cursor that held them back may have been its own.
func TestWhatABucketAndATargetLeaveBehind(t *testing.T) {
	t.Parallel()
	m := quick5(t)
	m.bucket5("photos", bucket.Unversioned)
	m.bucket5("kept", bucket.Unversioned)
	m.ok5(targetEntry("hook", delivery.KindWebhook))
	m.ok5(targetEntry("other", delivery.KindWebhook))
	rule := func(id, target string) bucket.NotificationRule {
		return bucket.NotificationRule{ID: id, Target: target}
	}
	m.ok5(configEntry("photos", bucket.ConfigNotification, notifyDoc(t, rule("r", "hook"), rule("o", "other"))))
	m.ok5(putEntry("photos", "old", 1, 1))
	m.ok5(deleteEntry("photos", "old", "", 2))
	m.ok5(entry{Type: cmdPutResync, PutResync: resyncToRecord(bucket.Resync{Bucket: "photos", Target: "hook", State: bucket.ResyncRunning})})
	m.ok5(entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "photos"}})
	if rows := storedRows(t, m.f); told(rows) != "put old>n:hook,n:other; delete old>n:hook,n:other" {
		t.Fatalf("the rows of a bucket that was deleted: %s", told(rows))
	}
	if got := rawState(t, m.f, resyncBucket()); len(rawStateOf(got, resyncBucket())) != 0 {
		t.Errorf("the record of a resync outlived its bucket: %v", got)
	}
	// The other target passes everything; the webhook holds the rows back.
	m.ok5(ackEntry("other", delivery.Seq{Index: 1 << 50}))
	if rows, _ := outboxCounts(t, m.f); rows != 2 {
		t.Fatalf("%d rows while one target has not passed them", rows)
	}
	// The webhook goes: nothing holds them back any more.
	m.ok5(entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: "hook"}})
	if rows, _ := outboxCounts(t, m.f); rows != 0 || len(storedRows(t, m.f)) != 0 {
		t.Errorf("%d rows after the target that held them back was deleted", rows)
	}

	// With no target left every row is behind every target; the deletion of
	// the last target cuts so many, and no more.
	m.ok5(targetEntry("hook", delivery.KindWebhook))
	m.ok5(configEntry("kept", bucket.ConfigNotification, notifyDoc(t, rule("r", "hook"))))
	const rows = delivery.MaxOutboxTrim + 20
	for i := range rows {
		m.ok5(putEntry("kept", fmt.Sprintf("k%04d", i), 1, 100+i))
	}
	m.ok5(entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: "other"}})
	if count, _ := outboxCounts(t, m.f); count != rows {
		t.Fatalf("%d rows after a target was deleted that had passed them, while another had not", count)
	}
	m.ok5(configEntry("kept", bucket.ConfigNotification, nil))
	m.ok5(entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: "hook"}})
	if count, _ := outboxCounts(t, m.f); count != 20 || len(storedRows(t, m.f)) != 20 {
		t.Fatalf("%d rows after the last target was deleted, want the %d that one entry does not cut", count, 20)
	}
	// A target defined now begins behind them, and its first
	// acknowledgement cuts the rest.
	m.ok5(targetEntry("hook", delivery.KindWebhook))
	m.ok5(ackEntry("hook", delivery.Seq{}))
	if count, _ := outboxCounts(t, m.f); count != 0 {
		t.Errorf("%d rows after the acknowledgement of a target defined later", count)
	}
}

// TestADamagedRecordOfDeliveryStopsTheStateMachine: a stored target, cursor,
// row or document that is not what this code wrote stops the state machine,
// like every damaged record: a member that went on by a reading of its own
// would hold other bytes than the others and say nothing.
func TestADamagedRecordOfDeliveryStopsTheStateMachine(t *testing.T) {
	t.Parallel()
	plant := func(m *machine, top []byte, nested string, key, value []byte) {
		t.Helper()
		m.f.mu.Lock()
		defer m.f.mu.Unlock()
		if err := m.f.db.Update(func(tx *bbolt.Tx) error {
			b := tx.Bucket(top)
			if nested != "" {
				b = b.Bucket([]byte(nested))
			}
			return b.Put(key, value)
		}); err != nil {
			t.Fatal(err)
		}
	}
	scene := func() (*machine, delivery.Seq) {
		m := rowsScene(t)
		m.ok5(putEntry("photos", "a", 1, 1))
		return m, delivery.Seq{Index: m.index}
	}
	tests := map[string]struct {
		damage func(m *machine, row delivery.Seq)
		then   entry
		want   error
	}{
		"a target that is no JSON": {
			func(m *machine, _ delivery.Seq) { plant(m, targetsBucket(), "", []byte("hook"), []byte("{")) },
			ackEntry("hook", delivery.Seq{}), domain.ErrIntegrity,
		},
		"a target of another version": {
			func(m *machine, _ delivery.Seq) {
				plant(m, targetsBucket(), "", []byte("hook"), []byte(`{"v":2,"name":"hook"}`))
			},
			targetEntry("hook", delivery.KindWebhook), domain.ErrUnsupportedFormat,
		},
		"a cursor that is no JSON": {
			func(m *machine, _ delivery.Seq) { plant(m, outboxCursorBucket(), "", []byte("hook"), []byte("[]")) },
			ackEntry("hook", delivery.Seq{}), domain.ErrIntegrity,
		},
		"a cursor of another version": {
			func(m *machine, _ delivery.Seq) {
				plant(m, outboxCursorBucket(), "", []byte("bus"), []byte(`{"v":9,"from":"00000000000000000000","acked":"00000000000000000000"}`))
			},
			ackEntry("hook", delivery.Seq{}), domain.ErrUnsupportedFormat,
		},
		"a cursor at a place that is none": {
			func(m *machine, _ delivery.Seq) {
				plant(m, outboxCursorBucket(), "", []byte("hook"), []byte(`{"v":1,"from":"00","acked":"00000000000000000000"}`))
			},
			ackEntry("hook", delivery.Seq{}), domain.ErrIntegrity,
		},
		// Read by a result that names it (below).
		"a row that is no row": {
			damage: func(m *machine, row delivery.Seq) {
				key := row.Key()
				plant(m, outboxBucket(), "", key[:], []byte(`{"v":1,"e":"put","b":"photos","k":"a","vid":"latest","at":"2026-10-12T09:00:00Z","to":["n:hook"]}`))
			},
		},
		"a row of another version": {
			damage: func(m *machine, row delivery.Seq) {
				key := row.Key()
				plant(m, outboxBucket(), "", key[:], []byte(`{"v":2}`))
			},
			want: domain.ErrUnsupportedFormat,
		},
		"a row for nobody": {
			damage: func(m *machine, row delivery.Seq) {
				key := row.Key()
				plant(m, outboxBucket(), "", key[:], []byte(`{"v":1,"e":"put","b":"photos","k":"a","vid":"null","at":"2026-10-12T09:00:00Z","to":[]}`))
			},
		},
		"a row for a consumer that is none": {
			damage: func(m *machine, row delivery.Seq) {
				key := row.Key()
				plant(m, outboxBucket(), "", key[:], []byte(`{"v":1,"e":"put","b":"photos","k":"a","vid":"null","at":"2026-10-12T09:00:00Z","to":["x:hook"]}`))
			},
		},
		"rules that are not canonical": {
			func(m *machine, _ delivery.Seq) {
				var b bucketRecord
				if err := json.Unmarshal(storedBucket(t, m.f, "photos"), &b); err != nil {
					t.Fatal(err)
				}
				b.Config[bucket.ConfigNotification] = []byte(`{"v":1, "targets":[]}`)
				data, _ := json.Marshal(b)
				plant(m, bucketsBucket(), "", []byte("photos"), data)
			},
			putEntry("photos", "b", 1, 2), domain.ErrIntegrity,
		},
		"a replication configuration that is not canonical": {
			func(m *machine, _ delivery.Seq) {
				var b bucketRecord
				if err := json.Unmarshal(storedBucket(t, m.f, "photos"), &b); err != nil {
					t.Fatal(err)
				}
				b.Config[bucket.ConfigReplication] = []byte(`{}`)
				data, _ := json.Marshal(b)
				plant(m, bucketsBucket(), "", []byte("photos"), data)
			},
			entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "photos"}},
			domain.ErrIntegrity,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, row := scene()
			test.damage(m, row)
			then, want := test.then, test.want
			if then.Type == "" {
				// The row is read by a result that names it.
				then = ackEntry("site-b", delivery.Seq{}, delivery.Result{Seq: row, Status: object.ReplicationCompleted})
			}
			if want == nil {
				want = domain.ErrIntegrity
			}
			if then.DeleteBucket != nil {
				// Emptied first, so that the deletion comes to the documents.
				for _, v := range everyVersion(t, m.f, "photos") {
					m.f.mu.Lock()
					err := m.f.db.Update(func(tx *bbolt.Tx) error {
						return tx.Bucket(objectsBucket()).Bucket([]byte("photos")).Delete([]byte(v.Key))
					})
					m.f.mu.Unlock()
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			before := wholeState(t, m.f)
			held, schema := written(then, entrySchema5)
			if r := m.raw(schema, held); !r.fatal || !errors.Is(r.err, want) {
				t.Fatalf("%s over a damaged record = %+v (stopped: %t), want the state machine to stop with %v", then.Type, r.err, r.fatal, want)
			}
			if after := wholeState(t, m.f); !reflect.DeepEqual(after, before) {
				t.Errorf("the entry it stopped at changed the state")
			}
		})
	}
}

// TestAResultByRowIsOfARowThatWasForReplication: a target keeps its cursor
// when it is replaced, so one that was a webhook and - once no document named
// it - became another deployment still has rows that were for it as a
// notification target. A result that names such a row names no version: what
// was told is not what was sent on.
func TestAResultByRowIsOfARowThatWasForReplication(t *testing.T) {
	t.Parallel()
	m := quick5(t)
	m.bucket5("photos", bucket.VersioningEnabled)
	m.ok5(targetEntry("x", delivery.KindWebhook))
	m.ok5(targetEntry("keeps", delivery.KindWebhook))
	m.ok5(configEntry("photos", bucket.ConfigNotification, notifyDoc(t, bucket.NotificationRule{ID: "x", Target: "x"}, bucket.NotificationRule{ID: "k", Target: "keeps"})))
	v := *m.ok5(putEntry("photos", "a", 5, 1)).stored
	row := delivery.Seq{Index: m.index}
	m.ok5(configEntry("photos", bucket.ConfigNotification, notifyDoc(t, bucket.NotificationRule{ID: "k", Target: "keeps"})))
	m.ok5(targetEntry("x", delivery.KindS3))
	if rows := storedRows(t, m.f); told(rows) != "put a>n:keeps,n:x" {
		t.Fatalf("the rows: %s", told(rows))
	}
	before := settled(t, m.f)
	m.ok5(ackEntry("x", delivery.Seq{}, delivery.Result{Seq: row, Status: object.ReplicationCompleted}))
	if after := settled(t, m.f); !reflect.DeepEqual(after, before) || statuses(t, m.f, "photos")["a@"+v.VersionID] != "" {
		t.Errorf("a result by a row that was for the target as a notification moved the status to %q", statuses(t, m.f, "photos")["a@"+v.VersionID])
	}
	// By the version itself it is no more the target's while the bucket is
	// not sent to it - and is taken once the replication configuration of
	// the bucket names the target: that is what a resync reports.
	ref, _ := object.ParseVersionRef(v.VersionID)
	byVersion := ackEntry("x", delivery.Seq{}, delivery.Result{Bucket: "photos", Key: "a", Version: ref, Status: object.ReplicationCompleted})
	if r := m.ok5(byVersion); r.unrelated != 1 || !reflect.DeepEqual(settled(t, m.f), before) {
		t.Errorf("a result by version of a bucket that is not sent to the target: %d not the target's, status %q", r.unrelated, statuses(t, m.f, "photos")["a@"+v.VersionID])
	}
	m.ok5(configEntry("photos", bucket.ConfigReplication, replicateDoc(t, "x", bucket.ReplicationRule{ID: "all", Enabled: true})))
	if r := m.ok5(byVersion); r.unrelated != 0 {
		t.Errorf("%d results not the target's, of a bucket that is sent to it", r.unrelated)
	}
	if got := statuses(t, m.f, "photos")["a@"+v.VersionID]; got != "COMPLETED" {
		t.Errorf("a result by version = %q", got)
	}
}

// TestALaterChangeOfALockIsSentAfterItsVersion: a change of the retention or
// of the legal hold of a version that is being, or was, sent to another
// deployment appends a row for that deployment and for nobody else; of a
// version that was never sent it appends none.
func TestALaterChangeOfALockIsSentAfterItsVersion(t *testing.T) {
	t.Parallel()
	m := quick5(t)
	m.ok5(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "vault", CreatedAt: m.at, ObjectLock: true}})
	m.ok5(targetEntry("site-b", delivery.KindS3))
	m.ok5(targetEntry("hook", delivery.KindWebhook))
	m.ok5(configEntry("vault", bucket.ConfigNotification, notifyDoc(t, bucket.NotificationRule{ID: "all", Target: "hook"})))
	before := *m.ok5(putEntry("vault", "k", 5, 1)).stored
	m.ok5(configEntry("vault", bucket.ConfigReplication, replicateDoc(t, "site-b", bucket.ReplicationRule{ID: "all", Enabled: true})))
	sent := *m.ok5(putEntry("vault", "k", 6, 2)).stored
	if before.Replication != "" || sent.Replication != "PENDING" {
		t.Fatalf("the statuses: %q, %q", before.Replication, sent.Replication)
	}
	retention := func(v versionRecord, days int) entry {
		return entry{Type: cmdSetRetention, SetRetention: &retentionCmd{
			Bucket: "vault", Key: "k", VersionRef: v.VersionID, Mode: string(object.Governance), RetainUntil: m.at.Add(time.Duration(days) * 24 * time.Hour),
		}}
	}
	hold := func(v versionRecord, on bool) entry {
		return entry{Type: cmdSetLegalHold, SetLegalHold: &legalHoldCmd{Bucket: "vault", Key: "k", VersionRef: v.VersionID, On: on}}
	}
	rows := len(storedRows(t, m.f))
	steps := []struct {
		name string
		e    entry
		want string
	}{
		{"the retention of a version that waits", retention(sent, 2), "retention k>r:site-b"},
		{"its legal hold", hold(sent, true), "legalhold k>r:site-b"},
		{"the retention of a version that was never sent", retention(before, 2), ""},
		{"its legal hold", hold(before, true), ""},
		{"the hold taken off", hold(sent, false), "legalhold k>r:site-b"},
	}
	for _, step := range steps {
		r := m.ok5(step.e)
		got := storedRows(t, m.f)[rows:]
		rows += len(got)
		if told(got) != step.want {
			t.Fatalf("%s appended %q, want %q", step.name, told(got), step.want)
		}
		if step.want != "" && (got[0].Version.String() != sent.VersionID || got[0].By != "" || got[0].Seq != (delivery.Seq{Index: m.index}) || got[0].Size != 6) {
			t.Errorf("%s: the row is %+v", step.name, got[0])
		}
		if r.stored.Replication != map[bool]string{true: "PENDING", false: ""}[step.want != ""] {
			t.Errorf("%s changed the status to %q", step.name, r.stored.Replication)
		}
	}
	// A refused change appends nothing: a retention that would end sooner,
	// without the word that lifts governance.
	m.refused5("a retention made shorter", retention(sent, 1), domain.ErrObjectLocked)
	// Once the other deployment has it, a change is still sent after it.
	ref, _ := object.ParseVersionRef(sent.VersionID)
	m.ok5(ackEntry("site-b", delivery.Seq{}, delivery.Result{Bucket: "vault", Key: "k", Version: ref, Status: object.ReplicationCompleted}))
	m.ok5(hold(sent, true))
	if got := storedRows(t, m.f)[rows:]; told(got) != "legalhold k>r:site-b" {
		t.Errorf("the hold of a version that was sent appended %q", told(got))
	}
	checkUsage(t, m.f, "vault")
}

// BenchmarkApplyPutWithARow is BenchmarkApplyPutWithAUsageRecord into a
// bucket whose notification rules match every put - one rule, and a hundred
// of which the last matches: what parsing the document, matching and
// appending the row cost a put (ADR-0023, "Consequences").
func BenchmarkApplyPutWithARow(b *testing.B) {
	for _, rules := range []int{1, bucket.MaxNotificationRules} {
		b.Run(fmt.Sprintf("%d rules, without waiting for the drive", rules), func(b *testing.B) {
			f := benchFSM(b, false)
			f.supported = entrySchema5
			config := bucket.NotificationConfig{}
			for i := range rules {
				rule := bucket.NotificationRule{ID: fmt.Sprint("r", i), As: bucket.NotifyAsQueue, Target: "hook", On: everyChange(), Prefix: "no such prefix/"}
				if i == rules-1 {
					rule.Prefix = ""
				}
				config.Rules = append(config.Rules, rule)
			}
			document, err := config.Document()
			if err != nil {
				b.Fatal(err)
			}
			index := uint64(0)
			for _, e := range []entry{
				{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedBy: "root"}},
				targetEntry("hook", delivery.KindWebhook),
				configEntry("b", bucket.ConfigNotification, document),
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
				if rows := readCounter(tx, outboxCountKey()); rows != number(b.N) {
					return fmt.Errorf("after %d puts the outbox counts %d rows", b.N, rows)
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
		})
	}
}
