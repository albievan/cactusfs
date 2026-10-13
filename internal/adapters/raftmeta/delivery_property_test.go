package raftmeta

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// sitePair drives state machines and the in-memory model of the ports
// through the same history of everything schema 5 keeps (usagePair, which it
// is built on, does so for the usage of buckets alone): targets, the two
// documents, the rows of the outbox, acknowledgements by row and by version,
// the bound, replica buckets and the copies they take, promotion, resyncs.
// The first state machine is held to the model - row for row, cursor for
// cursor, version for version - and the others to the first.
//
// After EVERY entry: its outcome is the model's, taken or refused and with
// the same sentinel; it appended a row exactly when the model did, at its own
// index (track); and an acknowledgement answers the model's count of results
// that were not its target's. The whole of what is kept is held to the model
// (held, and counts of usagePair: every row, cursor, target and reference,
// every document, every version of every key with its status, every usage
// record and quota) where it can have parted without that being seen at once:
// after every entry that was refused, every acknowledgement and every entry
// that is, or makes, the copy of something of another deployment or changes
// whose copies a bucket takes - and otherwise every lookEvery entries, before
// every snapshot, and at the end. What an entry of any other kind changes is
// cumulative - a version, a status, a count, a cursor stays what it was made
// until an entry changes it - so a state that has parted is found at the next
// look, among the few entries since the last.
//
// Every lookEvery entries, before every snapshot and at the end the usage
// record of every bucket is also recounted from the state itself (check of
// usagePair). Between those looks it is held to the model's count, and the
// versions the model counts are held to the state's one by one, so a record
// that is no recount is seen there too; the recount from the state is the
// second, independent witness of it.
//
// The model numbers its changes by itself, so a place in its outbox is not
// the place the state machine has: the two are held together by the order
// the rows were appended in. everFSM and everModel are the places of every
// row that ever was, in that order, and a place is compared by how many rows
// are at or before it.
type sitePair struct {
	*usagePair
	bound              uint64
	everFSM, everModel []delivery.Seq
	// replicaOf is the source the driver takes each bucket to be the
	// replica of.
	replicaOf map[string]string
	// What the history tried.
	appended, results, moved, copies, late, conflicts, promotions, snapshots int
	targetsPut, documents, resyncs, acks, refusedInReplica, unrelated        int
	// withoutID counts the markers without an ID that were refused, inUse
	// the targets that were not replaced because a bucket named them, and
	// cleared the marks of a resync that a record cleared.
	withoutID, inUse, cleared int
	// recounts counts the times every usage record was recounted from the
	// state.
	recounts int
}

// statusesOfModel returns the replication status of every version of the
// buckets named, as the model has them, by bucket, key and version.
func (p *sitePair) statusesOfModel(names ...string) map[string]object.ReplicationStatus {
	p.t.Helper()
	out := map[string]object.ReplicationStatus{}
	for _, name := range names {
		err := p.model.ScanVersions(p.t.Context(), name, ports.VersionCursor{}, func(v object.Version, _ bool) ports.ScanControl {
			out[name+"/"+v.Key+"@"+v.Ref().String()] = v.Replication
			return ports.ScanControl{}
		})
		if err != nil && !errors.Is(err, domain.ErrBucketNotFound) {
			p.t.Fatal(err)
		}
	}
	return out
}

// sentOn returns the target the versions of a bucket are sent on to, as the
// model has it; empty when they are sent nowhere.
func (p *sitePair) sentOn(name string) string {
	p.t.Helper()
	document, err := p.model.GetBucketConfig(p.t.Context(), name, bucket.ConfigReplication)
	if err != nil || len(document) == 0 {
		return ""
	}
	config, err := bucket.ParseReplicationConfig(document)
	if err != nil {
		p.t.Fatal(err)
	}
	return config.Target
}

// marked reports whether the cursor of a target says, in the model, that the
// other deployment needs a resync.
func (p *sitePair) marked(target string) bool {
	p.t.Helper()
	state, err := p.model.OutboxCursors(p.t.Context())
	if err != nil {
		p.t.Fatal(err)
	}
	for _, cursor := range state.Cursors {
		if cursor.Target == target {
			return cursor.ResyncNeeded
		}
	}
	return false
}

// sentinels are the refusals the two are held to agree on by name, where the
// order of refusals is part of what is specified.
func sentinels() []error {
	return []error{
		domain.ErrInvalidArgument, domain.ErrInvalidBucketState, domain.ErrConflict, domain.ErrEntityInUse, domain.ErrEntityNotFound,
		domain.ErrBucketNotFound, domain.ErrBucketNotEmpty, domain.ErrQuotaExceeded,
	}
}

// agreed holds the outcome of an entry to that of the same change in the
// model: both took effect, or both were refused with the same sentinel.
func (p *sitePair) agreed(what string, r applyResult, modelErr error) bool {
	p.t.Helper()
	ok := p.agree(what, r, modelErr)
	for _, sentinel := range sentinels() {
		if errors.Is(r.err, sentinel) != errors.Is(modelErr, sentinel) {
			p.t.Fatalf("entry %d, %s: the state machine says %v, the model %v", p.index, what, r.err, modelErr)
		}
	}
	return ok
}

// rank is how many rows that ever were are at or before a place.
func rank(ever []delivery.Seq, at delivery.Seq) int {
	n := 0
	for _, seq := range ever {
		if !seq.After(at) {
			n++
		}
	}
	return n
}

// track holds the row the entry just applied appended to the model's: both
// appended one or neither did, and the state machine's is at the index of the
// entry. It keeps the places of every row that ever was (everFSM, everModel),
// and is called after every entry: a row that was appended and pushed out at
// the bound before anybody looked would otherwise never have been seen.
func (p *sitePair) track() {
	p.t.Helper()
	var lastFSM, lastModel delivery.Seq
	if n := len(p.everFSM); n > 0 {
		lastFSM, lastModel = p.everFSM[n-1], p.everModel[n-1]
	}
	var newest delivery.Seq
	if err := p.fsms[0].view(func(tx *bbolt.Tx) (err error) {
		newest, _, err = lastRow(tx)
		return err
	}); err != nil {
		p.t.Fatal(err)
	}
	var come []delivery.Seq
	for _, row := range p.model.OutboxRows() {
		if row.Seq.After(lastModel) {
			come = append(come, row.Seq)
		}
	}
	appended := newest.After(lastFSM)
	if appended != (len(come) == 1) || len(come) > 1 {
		p.t.Fatalf("entry %d: the state machine appended a row: %t (its newest is at %s); the model appended %d", p.index, appended, newest, len(come))
	}
	if !appended {
		return
	}
	if newest != (delivery.Seq{Index: p.index}) {
		p.t.Fatalf("entry %d appended a row at %s", p.index, newest)
	}
	p.everFSM, p.everModel = append(p.everFSM, newest), append(p.everModel, come[0])
	p.appended++
}

// newestSeq returns the sequence of the newest version of a key, and whether
// it has a version.
func newestSeq(t *testing.T, f *fsm, name, key string) (seq uint64, there bool) {
	t.Helper()
	if err := f.view(func(tx *bbolt.Tx) error {
		if tx.Bucket(objectsBucket()).Bucket([]byte(name)) == nil {
			return nil
		}
		h, err := openHistory(tx, name, false)
		if err != nil {
			return err
		}
		v, err := h.newest(key)
		if err != nil || v == nil {
			return err
		}
		seq, there = v.Seq, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return seq, there
}

// checkReferences holds what the state keeps of which buckets name a target
// ("targetrefs") to the documents of the buckets themselves, and the count of
// every target to it: a bucket is under a target, with the letter of a kind
// of document, exactly when its stored document of that kind names the
// target; the count of a target is the number of those letters; a target
// admits every bucket that names it; and a target that nothing names has no
// place there.
func checkReferences(t *testing.T, f *fsm) {
	t.Helper()
	want := map[string]string{}
	if err := f.view(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketsBucket()).ForEach(func(name, _ []byte) error {
			b, err := readBucket(tx, string(name))
			if err != nil {
				return err
			}
			for _, kind := range []string{bucket.ConfigNotification, bucket.ConfigReplication} {
				names, err := namedBy(b.Name, kind, delivered(tx, b, kind))
				if err != nil {
					return err
				}
				for _, target := range names {
					want[target+"/"+b.Name] += string(refLetter(kind))
				}
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	got := references(t, f)
	if len(got) != len(want) {
		t.Fatalf("the state keeps the references %v, the documents of the buckets name %v", got, want)
	}
	targets := storedTargets(t, f)
	counts, named := map[string]int{}, map[string]bool{}
	for at, kinds := range want {
		if got[at] != kinds {
			t.Fatalf("the reference %s is kept as %q, the documents say %q", at, got[at], kinds)
		}
		target, bucketName, _ := strings.Cut(at, "/")
		counts[target] += len(kinds)
		named[target] = true
		if record, there := targets[target]; !there || !record.toDomain().Admits(bucketName) {
			t.Fatalf("bucket %s names target %s, which is there: %t, and does not admit it", bucketName, target, there)
		}
	}
	for name, record := range targets {
		if record.Refs != counts[name] {
			t.Fatalf("target %s is counted %d times, %d documents name it", name, record.Refs, counts[name])
		}
	}
	if err := f.view(func(tx *bbolt.Tx) error {
		refs := tx.Bucket(targetRefsBucket())
		if refs == nil {
			return nil
		}
		return refs.ForEachBucket(func(target []byte) error {
			if !named[string(target)] {
				return fmt.Errorf("target %s, which nothing names, has a place among the references", target)
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
}

// versionsOf returns the versions of every key of a bucket as a listing has
// them: the current version first, then the others, newest first.
func versionsOf(t *testing.T, f *fsm, name string) map[string][]versionRecord {
	t.Helper()
	out := map[string][]versionRecord{}
	if err := f.view(func(tx *bbolt.Tx) error {
		if index := tx.Bucket(objectsBucket()).Bucket([]byte(name)); index != nil {
			if err := index.ForEach(func(k, raw []byte) error {
				var v versionRecord
				if err := json.Unmarshal(raw, &v); err != nil {
					return err
				}
				out[string(k)] = append(out[string(k)], v)
				return nil
			}); err != nil {
				return err
			}
		}
		older := tx.Bucket(versionsBucket()).Bucket([]byte(name))
		if older == nil {
			return nil
		}
		return older.ForEach(func(k, raw []byte) error {
			key, _, ok := decodeKey(k)
			if !ok {
				return fmt.Errorf("a key of versions: %x", k)
			}
			var v versionRecord
			if err := json.Unmarshal(raw, &v); err != nil {
				return err
			}
			out[key] = append(out[key], v)
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// held holds everything schema 5 keeps in the first state machine to the
// model.
func (p *sitePair) held(names []string) {
	p.t.Helper()
	ctx := p.t.Context()
	f := p.fsms[0]

	// The rows, in their order; those that are new were seen when they were
	// appended (track).
	p.track()
	rows, want := storedRows(p.t, f), p.model.OutboxRows()
	if len(p.everFSM) != len(p.everModel) || len(rows) != len(want) {
		p.t.Fatalf("entry %d: the outbox holds %d rows (%d ever), the model's %d (%d ever):\n%s\n%s",
			p.index, len(rows), len(p.everFSM), len(want), len(p.everModel), told(rows), told(want))
	}
	for i, row := range rows {
		m := want[i]
		if row.Event != m.Event || row.Bucket != m.Bucket || row.Key != m.Key || row.Version != m.Version || row.Size != m.Size || row.ETag != m.ETag ||
			row.By != m.By || !slices.Equal(row.To, m.To) || rank(p.everFSM, row.Seq) != rank(p.everModel, m.Seq) {
			p.t.Fatalf("entry %d: row %d is %+v, the model's %+v", p.index, i, row, m)
		}
		if row.Seq.Index > p.index || row.Seq.Row != 0 {
			p.t.Fatalf("entry %d: a row at %s", p.index, row.Seq)
		}
	}
	state, err := p.model.OutboxCursors(ctx)
	if err != nil {
		p.t.Fatal(err)
	}
	if count, dropped := outboxCounts(p.t, f); count != state.Rows || dropped != state.Dropped || count != uint64(len(rows)) || count > p.bound {
		p.t.Fatalf("entry %d: the outbox counts %d rows and %d dropped; the model %d and %d; it holds %d and its bound is %d",
			p.index, count, dropped, state.Rows, state.Dropped, len(rows), p.bound)
	}

	// The cursors: of the same targets, at the same places.
	cursors := storedCursors(p.t, f)
	if len(cursors) != len(state.Cursors) {
		p.t.Fatalf("entry %d: %d cursors, the model has %d", p.index, len(cursors), len(state.Cursors))
	}
	for _, m := range state.Cursors {
		got, there := cursors[m.Target]
		if !there || got.Lost != m.Lost || got.ResyncNeeded != m.ResyncNeeded || got.AuditAcked != m.AuditAcked ||
			rank(p.everFSM, got.Acked) != rank(p.everModel, m.Acked) || rank(p.everFSM, got.From) != rank(p.everModel, m.From) || got.From.After(got.Acked) {
			p.t.Fatalf("entry %d: the cursor of %s is %+v (there: %t), the model's %+v", p.index, m.Target, got, there, m)
		}
	}

	// The targets, with the count of the documents that name each - and
	// which buckets those documents are of, held to the documents themselves.
	checkReferences(p.t, f)
	targets, err := p.model.ListTargets(ctx)
	if err != nil {
		p.t.Fatal(err)
	}
	stored := storedTargets(p.t, f)
	if len(stored) != len(targets) || len(stored) != len(cursors) {
		p.t.Fatalf("entry %d: %d targets and %d cursors, the model has %d targets", p.index, len(stored), len(cursors), len(targets))
	}
	for _, m := range targets {
		got := stored[m.Name].toDomain()
		// The model keeps a target as it was given; the record writes its
		// list of buckets as a list.
		if len(m.Buckets) == 0 {
			m.Buckets, got.Buckets = nil, nil
		}
		if !reflect.DeepEqual(got, m) {
			p.t.Fatalf("entry %d: target %s is %#v with %d references, the model's has %d", p.index, m.Name, got.Shown(), got.Refs, m.Refs)
		}
	}

	// The buckets: whose replica each is, its two documents, the record of
	// its resync, and every version of every key in its order and with its
	// status.
	for _, name := range names {
		m, err := p.model.GetBucket(ctx, name)
		raw := storedBucket(p.t, f, name)
		if (err != nil) != (raw == nil) {
			p.t.Fatalf("entry %d: bucket %s is there: %t; the model says %v", p.index, name, raw != nil, err)
		}
		if raw == nil {
			if _, known := p.versioning[name]; known {
				p.t.Fatalf("entry %d: bucket %s is gone and the driver does not know", p.index, name)
			}
			continue
		}
		var b bucketRecord
		if err := json.Unmarshal(raw, &b); err != nil {
			p.t.Fatal(err)
		}
		if b.ReplicaFrom != m.ReplicaFrom || b.Versioning != string(m.Versioning) || b.ReplicaFrom != p.replicaOf[name] {
			p.t.Fatalf("entry %d: bucket %s is the replica of %q and keeps versions %q; the model says %q and %q, the driver %q",
				p.index, name, b.ReplicaFrom, b.Versioning, m.ReplicaFrom, m.Versioning, p.replicaOf[name])
		}
		for _, kind := range []string{bucket.ConfigNotification, bucket.ConfigReplication} {
			document, err := p.model.GetBucketConfig(ctx, name, kind)
			if err != nil || !bytes.Equal(document, b.Config[kind]) {
				p.t.Fatalf("entry %d: the %s document of %s is %s, the model's %s (%v)", p.index, kind, name, b.Config[kind], document, err)
			}
		}
		var resync bucket.Resync
		if err := f.view(func(tx *bbolt.Tx) (err error) {
			resync, _, err = readResync(tx, name)
			return err
		}); err != nil {
			p.t.Fatal(err)
		}
		if m, err := p.model.GetResync(ctx, name); err != nil || m != resync {
			p.t.Fatalf("entry %d: the resync of %s is %+v, the model's %+v (%v)", p.index, name, resync, m, err)
		}
		got := versionsOf(p.t, f, name)
		seen := map[string]int{}
		err = p.model.ScanVersions(ctx, name, ports.VersionCursor{}, func(m object.Version, latest bool) ports.ScanControl {
			at := seen[m.Key]
			seen[m.Key]++
			if at >= len(got[m.Key]) {
				p.t.Fatalf("entry %d: %s/%s has %d versions, the model more", p.index, name, m.Key, len(got[m.Key]))
			}
			v := got[m.Key][at]
			if refOf(v) != m.Ref().String() || v.DeleteMarker != m.DeleteMarker || v.Replication != string(m.Replication) || v.Size != m.Size ||
				(m.Replication == object.ReplicationReplica && v.Seq != m.Seq) || latest != (at == 0) {
				p.t.Fatalf("entry %d: version %d of %s/%s is %s at %d (marker %t, %q, %d bytes); the model's is %s at %d (marker %t, %q, %d bytes)",
					p.index, at, name, m.Key, refOf(v), v.Seq, v.DeleteMarker, v.Replication, v.Size, m.Ref(), m.Seq, m.DeleteMarker, m.Replication, m.Size)
			}
			return ports.ScanControl{}
		})
		if err != nil {
			p.t.Fatal(err)
		}
		for key, versions := range got {
			if seen[key] != len(versions) {
				p.t.Fatalf("entry %d: %s/%s has %d versions, the model %d", p.index, name, key, len(versions), seen[key])
			}
		}
	}
}

// snapshot adds a replica that begins from a snapshot of the first.
func (p *sitePair) snapshot() {
	p.t.Helper()
	p.restored()
	setOutboxBound(p.fsms[len(p.fsms)-1], p.bound)
	p.snapshots++
}

// pool are the names a target is defined under, and a document may name: one
// of them is never defined.
func pool() []string {
	return []string{"hook", "bus", "site-b", "site-c", "never"}
}

// document makes a document of a kind: mostly for targets that are there and
// are of a kind that serves it, now and then for any name of the pool.
func (p *sitePair) document(kind string) []byte {
	p.t.Helper()
	names := pool()
	prefixes := []string{"", "", "a", "b", "zo"}
	var serving []string
	for name, record := range storedTargets(p.t, p.fsms[0]) {
		if (record.Kind == string(delivery.KindS3)) == (kind == bucket.ConfigReplication) {
			serving = append(serving, name)
		}
	}
	slices.Sort(serving)
	if len(serving) > 0 && p.rng.roll(5) != 0 {
		names = slices.Repeat(serving, 5)
	}
	if kind == bucket.ConfigReplication {
		rules := make([]bucket.ReplicationRule, 1+p.rng.roll(2))
		for i := range rules {
			rules[i] = bucket.ReplicationRule{
				ID: fmt.Sprint("r", i), Enabled: p.rng.roll(5) != 0, Priority: p.rng.roll(3), Prefix: prefixes[p.rng.roll(len(prefixes))], Markers: p.rng.roll(2) == 0,
			}
		}
		return replicateDoc(p.t, names[(2+p.rng.roll(3))%len(names)], rules...)
	}
	rules := make([]bucket.NotificationRule, 1+p.rng.roll(3))
	for i := range rules {
		var on []delivery.Event
		for _, event := range everyChange() {
			if p.rng.roll(3) != 0 {
				on = append(on, event)
			}
		}
		rules[i] = bucket.NotificationRule{ID: fmt.Sprint("n", i), Target: names[p.rng.roll(len(names))], On: on, Prefix: prefixes[p.rng.roll(len(prefixes))]}
		if p.rng.roll(4) == 0 {
			rules[i].Suffix = "b"
		}
	}
	return notifyDoc(p.t, rules...)
}

// move is one random change of the history, in the model and in the state
// machines.
func (p *sitePair) move(names, keys []string) {
	p.t.Helper()
	ctx := p.t.Context()
	name, key := names[p.rng.roll(len(names))], keys[p.rng.roll(len(keys))]
	if p.rng.roll(3) == 0 {
		// The copies of a source go where a replica bucket is, mostly.
		name = []string{"dst", "dst2"}[p.rng.roll(2)]
	}
	at := name + "\x00" + key
	known := func() object.VersionRef {
		ids := p.versions[at]
		if n := p.rng.roll(10); n < 2 || len(ids) == 0 {
			return object.VersionRef{Null: true}
		}
		return object.VersionRef{ID: ids[p.rng.roll(len(ids))]}
	}
	// replica is a copy as a source says it: mostly of the source the
	// bucket has, at a sequence that others have too.
	replica := func() *object.Replica {
		from := p.replicaOf[name]
		switch n := p.rng.roll(12); {
		case n == 0:
			from = siteC
		case n == 1 || from == "":
			from = siteA
		}
		r := &object.Replica{From: from, Seq: number(1 + p.rng.roll(60)), Null: p.rng.roll(9) == 0}
		if p.rng.roll(40) == 0 {
			r.Seq = []uint64{0, object.MaxReplicaSeq + 1, object.MaxReplicaSeq}[p.rng.roll(3)]
		}
		return r
	}
	inReplica := p.replicaOf[name] != ""
	switch n := p.rng.roll(100); {
	case n < 16:
		level := p.level()
		v := porttest.SampleVersion(name, key, 1)
		v.VersionID, v.DataID, v.Size, v.CreatedBy = p.id(), p.id(), int64(p.rng.roll(5000)), "user/albie"
		if p.rng.roll(15) == 0 {
			v.Size = -1 - int64(p.rng.roll(3000))
		}
		e := entry{Type: cmdPutObject, PutObject: versionToRecord(v)}
		if level < entrySchema3 {
			// Who made a version is not said below schema 3.
			v.CreatedBy = ""
		}
		_, modelErr := p.model.PutVersion(ctx, v)
		if p.agreed("object.put", p.apply(level, e), modelErr) {
			p.versions[at] = append(p.versions[at], v.VersionID)
		} else if inReplica && errors.Is(modelErr, domain.ErrInvalidBucketState) {
			p.refusedInReplica++
		}
	case n < 34:
		// The copy of a version - now and then of one that arrived before.
		v := porttest.SampleVersion(name, key, 1)
		v.VersionID, v.DataID, v.Size, v.CreatedBy = p.id(), p.id(), int64(p.rng.roll(5000)), "user/replicator"
		v.Replica = replica()
		if ids := p.versions[at]; len(ids) > 0 && p.rng.roll(5) == 0 {
			v.VersionID = ids[p.rng.roll(len(ids))]
		}
		newest, there := newestSeq(p.t, p.fsms[0], name, key)
		_, modelErr := p.model.PutVersion(ctx, v)
		r := p.apply(entrySchema5, entry{Type: cmdPutObject, PutObject: versionToRecord(v)})
		p.look = true
		switch {
		case p.agreed("object.put of a replica", r, modelErr):
			p.versions[at] = append(p.versions[at], v.VersionID)
			p.copies++
			if there && newest > v.Replica.Seq {
				p.late++
			}
		case errors.Is(modelErr, domain.ErrConflict):
			p.conflicts++
		}
	case n < 42:
		level := p.level()
		o := ports.DeleteOptions{MarkerID: p.id(), At: p.at, By: []string{"user/albie", "user/albie", bucket.LifecycleActor}[p.rng.roll(3)]}
		if level == entrySchema5 && p.rng.roll(20) == 0 {
			// A marker without an ID, which an entry of schema 5 is refused
			// for wherever it would make one. (Below schema 5 it is applied
			// as the release of schema 4 applies it, and is not in this
			// history: there the model and that release part.)
			o.MarkerID = object.ID{}
		}
		e := entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: name, Key: key, MarkerID: o.MarkerID.String(), At: o.At, By: o.By}}
		if level < entrySchema5 {
			// Who deleted is left out below level 5.
			o.By = ""
		}
		_, modelErr := p.model.DeleteObject(ctx, name, key, o)
		if p.agreed("object.version.delete", p.apply(level, e), modelErr) {
			if o.MarkerID.IsZero() && p.versioning[name] != bucket.Unversioned {
				p.t.Fatalf("entry %d: a marker without an ID was made in a bucket whose versions are %q", p.index, p.versioning[name])
			}
			p.versions[at] = append(p.versions[at], o.MarkerID)
		} else if inReplica && errors.Is(modelErr, domain.ErrInvalidBucketState) {
			p.refusedInReplica++
		} else if o.MarkerID.IsZero() && errors.Is(modelErr, domain.ErrInvalidArgument) {
			p.withoutID++
		}
	case n < 48:
		// The copy of a delete marker.
		o := ports.DeleteOptions{MarkerID: p.id(), At: p.at, Replica: replica()}
		if p.rng.roll(6) == 0 {
			// The copy of a marker without an ID: refused, wherever its
			// sequence would put it and whatever the key holds.
			o.MarkerID = object.ID{}
		}
		_, modelErr := p.model.DeleteObject(ctx, name, key, o)
		e := entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: name, Key: key, MarkerID: o.MarkerID.String(), At: o.At, Replica: replicaToRecord(o.Replica)}}
		p.look = true
		switch {
		case p.agreed("object.version.delete of a replica", p.apply(entrySchema5, e), modelErr):
			if o.MarkerID.IsZero() {
				p.t.Fatalf("entry %d: the copy of a marker without an ID was taken", p.index)
			}
			p.versions[at] = append(p.versions[at], o.MarkerID)
			p.copies++
		case errors.Is(modelErr, domain.ErrConflict):
			p.conflicts++
		case o.MarkerID.IsZero() && errors.Is(modelErr, domain.ErrInvalidArgument):
			p.withoutID++
		}
	case n < 54:
		level := p.level()
		version := known()
		o := ports.DeleteOptions{Version: &version, MarkerID: p.id(), At: p.at, By: "user/albie"}
		e := entry{Type: cmdDeleteVersion, DeleteVersion: &deleteVersionCmd{Bucket: name, Key: key, VersionRef: version.String(), MarkerID: o.MarkerID.String(), At: o.At, By: o.By}}
		if level < entrySchema5 {
			o.By = ""
		}
		if p.rng.roll(12) == 0 {
			// Removing a version is never the copy of anything.
			o.Replica, level = replica(), entrySchema5
			e.DeleteVersion.Replica = replicaToRecord(o.Replica)
			p.look = true
		}
		_, modelErr := p.model.DeleteObject(ctx, name, key, o)
		p.agreed("object.version.delete of a version", p.apply(level, e), modelErr)
	case n < 56:
		if state, there := p.versioning[name]; there && state != bucket.Unversioned {
			return
		}
		_, modelErr := p.model.DeleteObject(ctx, name, key, ports.DeleteOptions{MarkerID: p.id(), At: p.at})
		p.agree("object.delete", p.apply(entrySchemaVersion, entry{Type: cmdDeleteObject, DeleteObject: &deleteObjectCmd{Bucket: name, Key: key}}), modelErr)
	case n < 59:
		version := known()
		tags := map[string]string{"n": fmt.Sprint(n)}
		_, modelErr := p.model.SetTags(ctx, name, key, version, tags)
		e := entry{Type: cmdSetTags, SetTags: &tagsCmd{Bucket: name, Key: key, VersionRef: version.String(), Tags: tags}}
		p.agree("object.tags.put", p.apply(p.level(), e), modelErr)
	case n < 61:
		if name == "plain" {
			return
		}
		p.setVersioning(name, []bucket.Versioning{bucket.VersioningEnabled, bucket.VersioningEnabled, bucket.VersioningSuspended}[p.rng.roll(3)])
	case n < 64:
		u := porttest.SampleUpload(name, key, 1)
		u.ID = p.id()
		level := p.level()
		if p.rng.roll(2) == 0 {
			u.ReplicaFrom, level = replica().From, entrySchema5
			p.look = true
		}
		modelErr := p.model.CreateUpload(ctx, u)
		if p.agree("upload.create", p.apply(level, entry{Type: cmdCreateUpload, CreateUpload: uploadToRecord(u)}), modelErr) {
			p.uploads = append(p.uploads, &pairUpload{upload: u, parts: map[int]object.Part{}})
		}
	case n < 69 && len(p.uploads) > 0:
		u := p.uploads[p.rng.roll(len(p.uploads))]
		part := porttest.SamplePart(u.upload, 1+p.rng.roll(2), int64(object.MinPartSize), 1)
		part.DataID = p.id()
		_, modelErr := p.model.PutPart(ctx, u.upload.Bucket, u.upload.Key, u.upload.ID, part)
		if p.agree("upload.part.put", p.apply(p.level(), partEntry(u.upload, part)), modelErr) {
			u.parts[part.Number] = part
		}
	case n < 73 && len(p.uploads) > 0:
		i := p.rng.roll(len(p.uploads))
		u := p.uploads[i]
		level := p.level()
		c := ports.Completion{Bucket: u.upload.Bucket, Key: u.upload.Key, ID: u.upload.ID, VersionID: p.id(), ETag: "etag", At: p.at, CreatedBy: "root"}
		if (u.upload.ReplicaFrom != "") != (p.rng.roll(8) == 0) {
			c.Replica, level = replica(), entrySchema5
			if p.rng.roll(6) != 0 {
				c.Replica.From = u.upload.ReplicaFrom
			}
		}
		if c.Replica != nil || u.upload.ReplicaFrom != "" {
			p.look = true
		}
		numbers := make([]int, 0, len(u.parts))
		for number := range u.parts {
			numbers = append(numbers, number)
		}
		slices.Sort(numbers)
		for _, number := range numbers {
			part := u.parts[number]
			part.Seal = bytes.Repeat([]byte{1}, sha256.Size)
			c.Parts = append(c.Parts, part)
		}
		cmd := &completeUploadCmd{
			Bucket: c.Bucket, Key: c.Key, UploadID: c.ID.String(), VersionID: c.VersionID.String(), ETag: c.ETag, ModifiedAt: c.At.UTC(), CreatedBy: c.CreatedBy,
			Parts: make([]listedPart, len(c.Parts)), Replica: replicaToRecord(c.Replica),
		}
		for j, part := range c.Parts {
			cmd.Parts[j] = listedPart{Number: part.Number, ETag: part.ETag, Checksums: part.Checksums, DataID: part.DataID.String(), Seal: part.Seal}
		}
		if level < entrySchema3 {
			c.CreatedBy = ""
		}
		_, modelErr := p.model.CompleteUpload(ctx, c)
		if p.agree("upload.complete", p.apply(level, entry{Type: cmdCompleteUpload, CompleteUpload: cmd}), modelErr) {
			p.versions[c.Bucket+"\x00"+c.Key] = append(p.versions[c.Bucket+"\x00"+c.Key], c.VersionID)
			p.uploads = slices.Delete(p.uploads, i, i+1)
			if c.Replica != nil {
				p.copies++
			}
		}
	case n < 74 && len(p.uploads) > 0:
		i := p.rng.roll(len(p.uploads))
		u := p.uploads[i]
		_, modelErr := p.model.AbortUpload(ctx, u.upload.Bucket, u.upload.Key, u.upload.ID)
		p.agree("upload.abort", p.apply(p.level(), abortEntry(u.upload)), modelErr)
		p.uploads = slices.Delete(p.uploads, i, i+1)
	case n < 77:
		p.walkOnce(name, 1+p.rng.roll(6), p.rng.roll(7) == 0)
	case n < 78:
		usages, err := p.model.BucketUsages(ctx, []string{name})
		if err != nil {
			p.t.Fatal(err)
		}
		quota := max(1, usages[0].Usage.Used()+int64(p.rng.roll(30000))-3000)
		if p.rng.roll(3) == 0 {
			quota = 0
		}
		modelErr := p.model.SetBucketQuota(ctx, name, quota, audit.Record{})
		p.agreed("bucket.quota.put", p.apply(entrySchema5, entry{Type: cmdSetBucketQuota, SetBucketQuota: &bucketQuotaCmd{Bucket: name, Bytes: quota}}), modelErr)
	case n < 82:
		// A target is defined, or replaced - also as one of another kind.
		names := pool()
		usual := map[string]delivery.Kind{"hook": delivery.KindWebhook, "bus": delivery.KindNATS, "site-b": delivery.KindS3, "site-c": delivery.KindS3}
		target := porttest.SampleTarget(names[p.rng.roll(len(names)-1)], []delivery.Kind{delivery.KindWebhook, delivery.KindNATS, delivery.KindS3}[p.rng.roll(3)])
		if p.rng.roll(5) != 0 {
			target = porttest.SampleTarget(target.Name, usual[target.Name])
		}
		target.Buckets = [][]string{{"*"}, {"*"}, {"*"}, {"*"}, {"src", "plain", "old", "dst"}, {"d*", "late", "src"}, nil}[p.rng.roll(7)]
		target.Audit = target.Kind != delivery.KindS3 && p.rng.roll(2) == 0
		target.CreatedAt, target.UpdatedAt = p.at, p.at
		if p.rng.roll(15) == 0 {
			// One that cannot be kept: its path in the clear.
			target.Endpoint += "/in/token"
		}
		modelErr := p.model.PutTarget(ctx, target, audit.Record{})
		if p.agreed("target.put", p.apply(entrySchema5, entry{Type: cmdPutTarget, PutTarget: &targetCmd{Target: targetToRecord(target)}}), modelErr) {
			p.targetsPut++
		} else if errors.Is(modelErr, domain.ErrEntityInUse) {
			// Of another kind, or closed to a bucket that names it.
			p.inUse++
		}
	case n < 83:
		if p.rng.roll(3) != 0 {
			return
		}
		names := pool()
		target := names[p.rng.roll(len(names))]
		modelErr := p.model.DeleteTarget(ctx, target, audit.Record{})
		p.agreed("target.delete", p.apply(entrySchema5, entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: target}}), modelErr)
	case n < 88:
		kind := []string{bucket.ConfigNotification, bucket.ConfigNotification, bucket.ConfigReplication}[p.rng.roll(3)]
		var document []byte
		if p.rng.roll(6) != 0 {
			document = p.document(kind)
		}
		modelErr := p.model.SetBucketConfig(ctx, name, kind, document)
		if p.agreed("bucket.config.put", p.apply(entrySchema5, configEntry(name, kind, document)), modelErr) && document != nil {
			p.documents++
		}
	case n < 95:
		// An acknowledgement: up to a place among the rows that ever were,
		// or beyond them, with results by row and by version.
		names := pool()
		target := names[p.rng.roll(len(names))]
		if sent := p.sentOn("src"); sent != "" && p.rng.roll(2) == 0 {
			// Mostly as the node that leads acknowledges: for the target a
			// bucket is sent on to. Any other is given results too, which
			// are not its to give.
			target = sent
		}
		p.look = true
		var uptoFSM, uptoModel delivery.Seq
		switch at := p.rng.roll(len(p.everFSM) + 2); {
		case at == len(p.everFSM)+1:
			uptoFSM, uptoModel = delivery.Seq{Index: p.index + 1000, Row: 3}, delivery.Seq{Index: 1 << 60}
		case at > 0:
			uptoFSM, uptoModel = p.everFSM[at-1], p.everModel[at-1]
		}
		cmd := &ackOutboxCmd{Target: target, Upto: uptoFSM.String(), AuditUpto: number(p.rng.roll(50))}
		var results []delivery.Result
		for range p.rng.roll(4) {
			status := []object.ReplicationStatus{object.ReplicationCompleted, object.ReplicationCompleted, object.ReplicationFailed}[p.rng.roll(3)]
			if len(p.everFSM) > 0 && p.rng.roll(2) == 0 {
				at := p.rng.roll(len(p.everFSM))
				cmd.Results = append(cmd.Results, resultToRecord(delivery.Result{Seq: p.everFSM[at], Status: status}))
				results = append(results, delivery.Result{Seq: p.everModel[at], Status: status})
				continue
			}
			k := keys[p.rng.roll(len(keys))]
			bucketName := []string{"src", "src", "old", "plain", "dst"}[p.rng.roll(5)]
			ref := object.VersionRef{Null: true}
			if ids := p.versions[bucketName+"\x00"+k]; len(ids) > 0 && p.rng.roll(8) != 0 {
				ref = object.VersionRef{ID: ids[p.rng.roll(len(ids))]}
			}
			result := delivery.Result{Bucket: bucketName, Key: k, Version: ref, Status: status}
			cmd.Results = append(cmd.Results, resultToRecord(result))
			results = append(results, result)
		}
		// What an acknowledgement moves is counted in the model: the state
		// machine is held to it, version for version, right after.
		before := p.statusesOfModel("src", "old", "plain", "dst")
		unrelated, modelErr := p.model.AckOutbox(ctx, target, uptoModel, cmd.AuditUpto, results)
		r := p.apply(entrySchema5, entry{Type: cmdAckOutbox, AckOutbox: cmd})
		if p.agreed("outbox.ack", r, modelErr) {
			if r.unrelated != unrelated {
				p.t.Fatalf("entry %d: %d results of the acknowledgement were not the target's to give, the model says %d", p.index, r.unrelated, unrelated)
			}
			p.unrelated += unrelated
			p.acks++
			p.results += len(results)
			for at, status := range p.statusesOfModel("src", "old", "plain", "dst") {
				if before[at] != status {
					p.moved++
				}
			}
		}
	case n < 97:
		// A bucket is made the replica of a deployment, or promoted.
		source := []string{siteA, siteA, siteA, siteC, ""}[p.rng.roll(5)]
		if name != "dst" && name != "dst2" && name != "late" && p.rng.roll(3) != 0 {
			return
		}
		// One replica bucket stays one, so that copies arrive to the end;
		// the other is promoted late in the history, by the test.
		if source == "" && (name == "dst2" || name == "dst") {
			source = siteA
		}
		modelErr := p.model.SetBucketReplica(ctx, name, source, audit.Record{})
		p.look = true
		was := p.replicaOf[name]
		if p.agreed("bucket.replica.put", p.apply(entrySchema5, replicaModeEntry(name, source)), modelErr) {
			if p.replicaOf[name] = source; source == "" {
				delete(p.replicaOf, name)
				if was != "" {
					p.promotions++
				}
			}
		}
	case n < 98:
		names := pool()
		r := bucket.Resync{Bucket: name, Target: names[p.rng.roll(len(names))], State: []bucket.ResyncState{bucket.ResyncRunning, bucket.ResyncDone}[p.rng.roll(2)], Scanned: 9, Sent: 2}
		if sent := p.sentOn("src"); sent != "" && p.rng.roll(2) == 0 {
			// The resync of a bucket that is sent on, for its target: the
			// one that clears the mark of the target when it is done.
			r.Bucket, r.Target = "src", sent
		}
		if p.rng.roll(2) == 0 {
			r.Key, r.Version = key, object.NullVersion
		}
		was := p.marked(r.Target)
		modelErr := p.model.PutResync(ctx, r)
		p.look = true
		if p.agreed("replication.resync.put", p.apply(entrySchema5, entry{Type: cmdPutResync, PutResync: resyncToRecord(r)}), modelErr) {
			p.resyncs++
			if was && !p.marked(r.Target) {
				if r.State != bucket.ResyncDone || p.sentOn(r.Bucket) != r.Target {
					p.t.Fatalf("entry %d: the mark of %s was cleared by %+v", p.index, r.Target, r)
				}
				p.cleared++
			}
		}
	default:
		// A bucket that comes and goes.
		if _, there := p.versioning["late"]; !there {
			p.create("late", []byte{entrySchema5, entrySchema5, entrySchema4}[p.rng.roll(3)])
			return
		}
		modelErr := p.model.DeleteBucket(ctx, "late")
		if p.agree("bucket.delete", p.apply(p.level(), entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: "late"}}), modelErr) {
			delete(p.versioning, "late")
			delete(p.replicaOf, "late")
		}
	}
}

// TestDeliveryAndReplicasAreTheModelsThroughAnyHistory: through long random
// histories of everything schema 5 keeps - puts, markers and deletes of every
// kind, uploads, walks and quotas; targets defined, replaced and deleted;
// notification rules and replication configurations put, replaced and
// removed, also for targets that are not there or do not fit; the rows every
// change appends, at a bound low enough that rows are dropped all the time;
// acknowledgements up to any place, with results by row and by version;
// buckets made replicas and promoted; the copies of versions and markers of a
// source in any order, at sequences that others have, arriving again, from
// another source, into buckets that are no replicas; resyncs; a bucket that
// is deleted and made again; entries written by proposers of level 5, 4 and
// 2 - the first state machine holds what the model of the ports holds, looked
// at as sitePair says - after every entry for its outcome and its row, and in
// full after every refusal, acknowledgement and copy and every lookEvery
// entries: the same rows in the same order for the same consumers, the
// same counts of rows and of dropped rows, the same cursors at the same
// places with the same losses, the same targets with the same counts of
// references, the same documents and replica sources, every version of every
// key in the same order with the same status, and the usage of every bucket
// with its two counts of replication. Every entry is refused in the state
// machine exactly when it is in the model, and - for what schema 5 added -
// with the same sentinel. A replica fed the same log holds the same bytes,
// and so does one that begins from a snapshot taken in the middle.
func TestDeliveryAndReplicasAreTheModelsThroughAnyHistory(t *testing.T) {
	t.Parallel()
	const entries = 2500
	for _, seed := range []int{1, 2, 3, 2026} {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()
			const bound = 12
			p := &sitePair{usagePair: newUsagePair(t, seed, 2), bound: bound, replicaOf: map[string]string{}}
			p.model.MaxOutboxRows = bound
			for _, f := range p.fsms {
				setOutboxBound(f, p.bound)
			}
			names := []string{"src", "plain", "dst", "dst2", "old", "late"}
			keys := []string{"a", "a/b", "b", "zoë/π.txt", "k \"quoted\" <&>"}
			for _, name := range []string{"src", "plain", "dst", "dst2"} {
				p.create(name, entrySchema5)
			}
			p.create("old", entrySchema4)
			for _, name := range []string{"src", "dst", "dst2", "old"} {
				p.setVersioning(name, bucket.VersioningEnabled)
			}
			// A beginning that the rest can build on: three targets, a
			// bucket that tells and sends on, two that are replicas.
			ctx := t.Context()
			for _, target := range []delivery.Target{porttest.SampleTarget("hook", delivery.KindWebhook), porttest.SampleTarget("site-b", delivery.KindS3)} {
				if err := p.model.PutTarget(ctx, target, audit.Record{}); err != nil {
					t.Fatal(err)
				}
				p.agreed("target.put", p.apply(entrySchema5, entry{Type: cmdPutTarget, PutTarget: &targetCmd{Target: targetToRecord(target)}}), nil)
				p.held(names)
			}
			for _, kind := range []string{bucket.ConfigNotification, bucket.ConfigReplication} {
				document := notifyDoc(t, bucket.NotificationRule{ID: "all", Target: "hook"})
				if kind == bucket.ConfigReplication {
					document = replicateDoc(t, "site-b", bucket.ReplicationRule{ID: "all", Enabled: true, Markers: true})
				}
				if err := p.model.SetBucketConfig(ctx, "src", kind, document); err != nil {
					t.Fatal(err)
				}
				p.agreed("bucket.config.put", p.apply(entrySchema5, configEntry("src", kind, document)), nil)
			}
			for _, name := range []string{"dst", "dst2"} {
				if err := p.model.SetBucketReplica(ctx, name, siteA, audit.Record{}); err != nil {
					t.Fatal(err)
				}
				p.agreed("bucket.replica.put", p.apply(entrySchema5, replicaModeEntry(name, siteA)), nil)
				p.replicaOf[name] = siteA
			}
			// The copies that arrive are told, and so is what a bucket
			// without versions takes.
			for _, name := range []string{"dst", "dst2", "plain"} {
				document := notifyDoc(t, bucket.NotificationRule{ID: "all", Target: "hook"})
				if err := p.model.SetBucketConfig(ctx, name, bucket.ConfigNotification, document); err != nil {
					t.Fatal(err)
				}
				p.agreed("bucket.config.put", p.apply(entrySchema5, configEntry(name, bucket.ConfigNotification, document)), nil)
			}
			p.check(names)
			p.held(names)
			for i := range entries {
				p.move(names, keys)
				p.track()
				snapshot := i == entries/3 || i == 2*entries/3
				whole := i%lookEvery == lookEvery-1 || snapshot || i == entries-1
				if p.due(i) || whole {
					p.counts(names)
					p.held(names)
				}
				if whole {
					// And recounted from the state itself.
					p.check(names)
					p.recounts++
				}
				if snapshot {
					p.snapshot()
				}
				if i == 4*entries/5 && p.replicaOf["dst"] != "" {
					// One replica bucket is promoted for the rest of the
					// history: what is written there now is the newest of
					// its key, above every sequence its source had.
					if err := p.model.SetBucketReplica(ctx, "dst", "", audit.Record{}); err != nil {
						t.Fatal(err)
					}
					p.agreed("bucket.replica.put", p.apply(entrySchema5, replicaModeEntry("dst", "")), nil)
					delete(p.replicaOf, "dst")
					p.promotions++
					p.track()
					p.check(names)
					p.held(names)
				}
				if i%150 == 0 {
					p.identical()
				}
			}
			p.identical()
			if len(p.fsms) != 4 {
				t.Errorf("%d state machines", len(p.fsms))
			}
			for i, f := range p.fsms {
				if f.usageClamped.Load() != 0 || f.usageDrift.Load() != 0 {
					t.Errorf("state machine %d: %d changes held at a bound, %d recounts that found drift", i, f.usageClamped.Load(), f.usageDrift.Load())
				}
			}
			_, dropped := outboxCounts(t, p.fsms[0])
			var lost uint64
			resync := false
			for _, cursor := range storedCursors(t, p.fsms[0]) {
				lost += cursor.Lost
				resync = resync || cursor.ResyncNeeded
			}
			tried := fmt.Sprintf("%d rows appended, %d dropped, %d lost to targets that are left (a resync needed: %t), %d acknowledgements with %d results that moved %d statuses "+
				"and of which %d were not the target's, %d copies of which %d arrived late and %d a second time, %d markers without an ID refused, "+
				"%d ordinary writes refused by a replica bucket, %d promotions, %d targets put and %d held by what names them, %d documents, "+
				"%d resyncs of which %d cleared a mark, %d snapshots, %d times the whole was held to the model and %d times recounted from the state",
				p.appended, dropped, lost, resync, p.acks, p.results, p.moved, p.unrelated, p.copies, p.late, p.conflicts, p.withoutID, p.refusedInReplica, p.promotions,
				p.targetsPut, p.inUse, p.documents, p.resyncs, p.cleared, p.snapshots, p.looks, p.recounts)
			if p.appended < 150 || dropped < 100 || lost == 0 || p.promotions == 0 || p.acks < 20 || p.moved < 5 || p.unrelated < 20 || p.copies < 80 || p.late < 15 ||
				p.conflicts < 5 || p.withoutID < 3 || p.refusedInReplica < 5 || p.targetsPut < 10 || p.inUse < 10 || p.documents < 10 || p.resyncs < 2 || p.cleared < 2 ||
				p.snapshots != 2 || p.looks < entries/lookEvery+p.acks+p.copies+p.conflicts || p.recounts < entries/lookEvery {
				t.Errorf("the history tried too little: %s", tried)
			}
			t.Log(strings.ReplaceAll(tried, ", ", "\n"))
		})
	}
}
