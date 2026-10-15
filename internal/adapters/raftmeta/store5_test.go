package raftmeta

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
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

// outboxBoundForTest is the bound the outbox of a store is lowered to for
// the contract of what happens at it.
const outboxBoundForTest = 8

// TestStoreSchema5Contract: what schema 5 added - the usage of buckets and
// their quotas, targets, the outbox, replica buckets and the record of a
// resync - asked of a store on its own, which nothing holds below level 5.
func TestStoreSchema5Contract(t *testing.T) {
	t.Parallel()
	porttest.Schema5Contract(t, func(t *testing.T) porttest.Schema5 { return openStore(t, t.TempDir()) })
}

// TestStoreOutboxBoundContract: what a store on its own does when its outbox
// is full.
func TestStoreOutboxBoundContract(t *testing.T) {
	t.Parallel()
	porttest.OutboxBoundContract(t, outboxBoundForTest, func(t *testing.T) porttest.Schema5 {
		store := openStore(t, t.TempDir())
		setOutboxBound(store.fsm, outboxBoundForTest)
		return store
	})
}

// TestWhatSchema5RefusesIsRefusedBeforeItIsProposed: the first entry of
// schema 5 a state applies ends the way back to the release of schema 4,
// whether it took effect or not (ADR-0023, "Rollback"). So what the state
// machine would refuse without looking at the state - a limit, a quota, a
// name, a target that cannot be, a result that names nothing, a document
// that is not canonical, an audit record that cannot be kept - the store
// refuses by itself and proposes nothing: the state stays one that release
// opens. And an entry that IS proposed and refused where it is applied marks
// the state all the same.
func TestWhatSchema5RefusesIsRefusedBeforeItIsProposed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openStore(t, t.TempDir())
	index, err := store.AppliedIndex()
	if err != nil {
		t.Fatal(err)
	}
	notification, err := bucket.NotificationConfig{Rules: []bucket.NotificationRule{
		{ID: "all", As: bucket.NotifyAsQueue, Target: "hook", On: []delivery.Event{delivery.EventPut}},
	}}.Document()
	if err != nil {
		t.Fatal(err)
	}
	bad := audit.Record{Actor: "root", Action: "admin:PutBucketQuota", Outcome: "perhaps"}
	nameless := porttest.SampleTarget("hook", delivery.KindWebhook)
	nameless.Name = ""
	kindless := porttest.SampleTarget("hook", delivery.KindWebhook)
	kindless.Kind = "ftp"
	long := strings.Repeat("b", bucket.MaxNameLength+1)
	tests := map[string]struct {
		call func() error
		want error
	}{
		"a walk of no records": {func() error { _, err := store.WalkBucketUsage(ctx, "photos", 0, false); return err }, domain.ErrInvalidArgument},
		"a walk of more than an entry may": {func() error {
			_, err := store.WalkBucketUsage(ctx, "photos", ports.MaxUsageWalk+1, true)
			return err
		}, domain.ErrInvalidArgument},
		"a walk of a bucket without a name":   {func() error { _, err := store.WalkBucketUsage(ctx, "", 10, false); return err }, domain.ErrBucketNotFound},
		"a walk of a name no bucket can have": {func() error { _, err := store.WalkBucketUsage(ctx, long, 10, false); return err }, domain.ErrBucketNotFound},
		"a quota below nothing":               {func() error { return store.SetBucketQuota(ctx, "photos", -1, audit.Record{}) }, domain.ErrInvalidArgument},
		"a quota above the greatest": {func() error {
			return store.SetBucketQuota(ctx, "photos", bucket.MaxQuotaBytes+1, audit.Record{})
		}, domain.ErrInvalidArgument},
		"a quota of a bucket without a name":               {func() error { return store.SetBucketQuota(ctx, "", 10, audit.Record{}) }, domain.ErrBucketNotFound},
		"a quota with an audit record that cannot be kept": {func() error { return store.SetBucketQuota(ctx, "photos", 10, bad) }, domain.ErrInvalidArgument},
		"a target without a name":                          {func() error { return store.PutTarget(ctx, nameless, audit.Record{}) }, domain.ErrInvalidArgument},
		"a target of a kind there is not":                  {func() error { return store.PutTarget(ctx, kindless, audit.Record{}) }, domain.ErrInvalidArgument},
		"a target with an audit record that cannot be kept": {func() error {
			return store.PutTarget(ctx, porttest.SampleTarget("hook", delivery.KindWebhook), bad)
		}, domain.ErrInvalidArgument},
		"the deletion of a name no target can have":           {func() error { return store.DeleteTarget(ctx, "", audit.Record{}) }, domain.ErrEntityNotFound},
		"a deletion with an audit record that cannot be kept": {func() error { return store.DeleteTarget(ctx, "hook", bad) }, domain.ErrInvalidArgument},
		"more results than an acknowledgement carries": {func() error {
			_, err := store.AckOutbox(ctx, "site-b", delivery.Seq{Index: 1}, 0, make([]delivery.Result, delivery.MaxAckResults+1))
			return err
		}, domain.ErrInvalidArgument},
		"a result that names nothing": {func() error {
			_, err := store.AckOutbox(ctx, "site-b", delivery.Seq{Index: 1}, 0, []delivery.Result{{Status: object.ReplicationCompleted}})
			return err
		}, domain.ErrInvalidArgument},
		"an acknowledgement of a name no target can have": {func() error {
			_, err := store.AckOutbox(ctx, "", delivery.Seq{Index: 1}, 0, nil)
			return err
		}, domain.ErrEntityNotFound},
		"replica mode of a bucket without a name": {func() error { return store.SetBucketReplica(ctx, "", "5d3c1f0a-site-a", audit.Record{}) }, domain.ErrBucketNotFound},
		"a source that is no deployment":          {func() error { return store.SetBucketReplica(ctx, "photos", "not a deployment!", audit.Record{}) }, domain.ErrInvalidArgument},
		"replica mode with an audit record that cannot be kept": {func() error {
			return store.SetBucketReplica(ctx, "photos", "5d3c1f0a-site-a", bad)
		}, domain.ErrInvalidArgument},
		"a resync that is no record": {func() error { return store.PutResync(ctx, bucket.Resync{}) }, domain.ErrInvalidArgument},
		"notification rules that are not canonical": {func() error {
			return store.SetBucketConfig(ctx, "photos", bucket.ConfigNotification, append(notification, ' '))
		}, domain.ErrInvalidArgument},
		"notification rules of a version nobody knows": {func() error {
			return store.SetBucketConfig(ctx, "photos", bucket.ConfigNotification, []byte(strings.Replace(string(notification), `"v":1`, `"v":2`, 1)))
		}, domain.ErrUnsupportedFormat},
		"a replication configuration that is no document": {func() error {
			return store.SetBucketConfig(ctx, "photos", bucket.ConfigReplication, []byte(`{"v":1}`))
		}, domain.ErrInvalidArgument},
	}
	for name, test := range tests {
		if err := test.call(); !errors.Is(err, test.want) || errors.Is(err, domain.ErrOutcomeUnknown) || errors.Is(err, domain.ErrUnavailable) {
			t.Errorf("%s = %v, want a definite %v", name, err, test.want)
		}
	}
	// Questions that cannot be answered are refused too, and are no entry.
	if _, err := store.BucketUsages(ctx, make([]string, ports.MaxBucketUsages+1)); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("BucketUsages of too many buckets = %v", err)
	}
	if _, _, err := store.ReadOutbox(ctx, "nobody", delivery.Seq{}, 10); !errors.Is(err, domain.ErrEntityNotFound) {
		t.Errorf("ReadOutbox of a target there is not = %v", err)
	}
	// A read of a number of rows that cannot be is refused before the leader
	// is asked whether the state is current - here, with a context that has
	// ended, where every read that asks fails for that.
	ended, cancel := context.WithCancel(ctx)
	cancel()
	for _, limit := range []int{0, -1, delivery.MaxOutboxRead + 1} {
		if _, _, err := store.ReadOutbox(ended, "nobody", delivery.Seq{}, limit); !errors.Is(err, domain.ErrInvalidArgument) || errors.Is(err, context.Canceled) {
			t.Errorf("ReadOutbox of %d rows, with nobody to ask = %v, want %v", limit, err, domain.ErrInvalidArgument)
		}
	}
	if _, _, err := store.ReadOutbox(ended, "nobody", delivery.Seq{}, 10); err == nil || errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("ReadOutbox with a context that has ended = %v: the test proves less than it says", err)
	}
	if _, err := store.GetTarget(ctx, ""); !errors.Is(err, domain.ErrEntityNotFound) {
		t.Errorf("GetTarget without a name = %v", err)
	}
	if _, err := store.GetResync(ctx, ""); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("GetResync without a bucket = %v", err)
	}
	if after, err := store.AppliedIndex(); err != nil || after != index {
		t.Fatalf("a refused call was proposed: the state is at entry %d (%v), was at %d", after, err, index)
	}
	if got := stateSchema(t, store.fsm); got >= stateSchema5 {
		t.Errorf("refused calls left the state at schema %d", got)
	}
	if raw := rawState(t, store.fsm, schema5Buckets()...); len(raw) != 0 {
		t.Errorf("refused calls left %v", raw)
	}

	// What is well formed is proposed, and marks the state though the state
	// machine refuses it: a walk through a bucket that is not there.
	if _, err := store.WalkBucketUsage(ctx, "missing", 10, false); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Fatalf("a walk through a bucket that is not there = %v, want %v", err, domain.ErrBucketNotFound)
	}
	if after, err := store.AppliedIndex(); err != nil || after <= index {
		t.Errorf("the walk was an entry: the state is at entry %d (%v), was at %d", after, err, index)
	}
	if got := stateSchema(t, store.fsm); got != stateSchema5 {
		t.Errorf("after an entry of schema 5 that was refused the state is at schema %d, want %d", got, stateSchema5)
	}
}

// The two targets of the tests of a read of the outbox, and the bucket whose
// puts each is told of.
const (
	outboxFirst, outboxSecond             = "first-hook", "second-hook"
	outboxFirstBucket, outboxSecondBucket = "told-first", "told-second"
)

// twoTargets defines two targets on a store and a bucket for each, whose
// every put the target is told of, and returns the function that puts an
// object into a bucket: one row of the outbox, for one of the two.
func twoTargets(t *testing.T, s *Store) (put func(bucketName string)) {
	t.Helper()
	ctx := t.Context()
	for target, name := range map[string]string{outboxFirst: outboxFirstBucket, outboxSecond: outboxSecondBucket} {
		if err := s.PutTarget(ctx, porttest.SampleTarget(target, delivery.KindWebhook), audit.Record{}); err != nil {
			t.Fatalf("PutTarget %s: %v", target, err)
		}
		if err := s.CreateBucket(ctx, bucket.Bucket{Name: name}); err != nil {
			t.Fatalf("CreateBucket %s: %v", name, err)
		}
		document, err := bucket.NotificationConfig{Rules: []bucket.NotificationRule{
			{ID: "all", As: bucket.NotifyAsQueue, Target: target, On: []delivery.Event{delivery.EventPut}},
		}}.Document()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetBucketConfig(ctx, name, bucket.ConfigNotification, document); err != nil {
			t.Fatalf("SetBucketConfig %s: %v", name, err)
		}
	}
	var serial byte
	return func(bucketName string) {
		t.Helper()
		serial++
		if _, err := s.PutVersion(ctx, porttest.SampleVersion(bucketName, "k-"+string('a'+rune(serial)), serial)); err != nil {
			t.Fatalf("PutVersion into %s: %v", bucketName, err)
		}
	}
}

// outboxPassesOtherTargets is what a read of the outbox makes of rows that
// are for other targets (ports.Outbox.ReadOutbox), asked of one node. They
// are passed over, and the read says so: upto is the place of the last row
// it looked at, though the target has none - a target that acknowledges upto
// has them behind it. And it looks at no more rows than its bound, whoever
// they are for: with the bound at two rows, a target whose row lies behind
// four of another finds it by repeating the read from where the last came
// to. A read that said "nothing was looked at" there would never reach it.
func outboxPassesOtherTargets(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	put := twoTargets(t, s)
	for range 4 {
		put(outboxFirstBucket)
	}
	others, last, err := s.ReadOutbox(ctx, outboxFirst, delivery.Seq{}, 10)
	if err != nil || len(others) != 4 || last != others[3].Seq {
		t.Fatalf("the rows of %s = %d up to %s, %v; want four, up to the last", outboxFirst, len(others), last, err)
	}
	// Rows of another target only: none is returned, and all are passed.
	items, upto, err := s.ReadOutbox(ctx, outboxSecond, delivery.Seq{}, 10)
	if err != nil || len(items) != 0 || upto != last {
		t.Fatalf("a read for %s over the rows of %s = %d rows up to %s, %v; want none, up to the last row %s", outboxSecond, outboxFirst, len(items), upto, err, last)
	}
	// From there nothing is left to look at.
	if items, upto, err = s.ReadOutbox(ctx, outboxSecond, last, 10); err != nil || len(items) != 0 || upto != last {
		t.Errorf("a read from the last row = %d rows up to %s, %v; want none, and %s still", len(items), upto, err, last)
	}

	// The bound: two rows a call, whoever they are for.
	SetOutboxScanForTest(s, 2)
	put(outboxSecondBucket)
	if items, upto, err = s.ReadOutbox(ctx, outboxFirst, delivery.Seq{}, 10); err != nil || len(items) != 2 || upto != others[1].Seq {
		t.Errorf("a read for %s that may look at two rows = %d rows up to %s, %v; want two, up to %s", outboxFirst, len(items), upto, err, others[1].Seq)
	}
	after := delivery.Seq{}
	for call := 1; ; call++ {
		if items, upto, err = s.ReadOutbox(ctx, outboxSecond, after, 10); err != nil {
			t.Fatalf("read %d for %s: %v", call, outboxSecond, err)
		}
		if len(items) != 0 {
			// The fifth row, in the third call: behind two calls of two
			// rows of the other target.
			if call != 3 || len(items) != 1 || items[0].Bucket != outboxSecondBucket || upto != items[0].Seq || !upto.After(last) {
				t.Errorf("read %d for %s = %d rows, the first in %s, up to %s; want its one row in the third read", call, outboxSecond, len(items), items[0].Bucket, upto)
			}
			break
		}
		if call > 2 || upto != others[2*call-1].Seq {
			t.Fatalf("read %d for %s, of two rows of %s, came from %s to %s; want %s", call, outboxSecond, outboxFirst, after, upto, others[min(2*call, len(others))-1].Seq)
		}
		after = upto
	}
}

// TestAReadOfTheOutboxPassesTheRowsOfOtherTargets: asked of a store on its
// own, and of a node of a cluster that neither leads nor votes - whose reads
// wait for the leader's index, and whose bound is its own.
func TestAReadOfTheOutboxPassesTheRowsOfOtherTargets(t *testing.T) {
	t.Parallel()
	tests := map[string]func(t *testing.T) *Store{
		"a store on its own":                  func(t *testing.T) *Store { return openStore(t, t.TempDir()) },
		"a node that neither leads nor votes": func(t *testing.T) *Store { return clusterAtLevel5(t, nil).store("s4") },
	}
	for name, open := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			outboxPassesOtherTargets(t, open(t))
		})
	}
}

// TestAReadOfTheOutboxLooksAtNoMoreThanItsBound: the bound a store is opened
// with. A target whose rows are few among many of others must not hold a
// read transaction open over the whole outbox: one call looks at four reads'
// worth of rows - outboxScan - and returns how far it came. The rows are
// written straight into the state, as copies of one the store made: four
// thousand puts would say no more.
func TestAReadOfTheOutboxLooksAtNoMoreThanItsBound(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openStore(t, t.TempDir())
	put := twoTargets(t, store)
	put(outboxFirstBucket)
	made, first, err := store.ReadOutbox(ctx, outboxFirst, delivery.Seq{}, delivery.MaxOutboxRead)
	if err != nil || len(made) != 1 || first != made[0].Seq {
		t.Fatalf("the row of the put = %d rows up to %s, %v", len(made), first, err)
	}
	// One row more than a call looks at, all for the first target: the one
	// the put made, and so many copies of it behind it, at the places of
	// further rows of the same entry.
	const bound = 4 * delivery.MaxOutboxRead
	atBound, last := first, first
	if err := store.fsm.db.Update(func(tx *bbolt.Tx) error {
		rows := tx.Bucket(outboxBucket())
		at := first.Key()
		raw := bytes.Clone(rows.Get(at[:]))
		for n := range bound {
			last.Row++
			key := last.Key()
			if err := rows.Put(key[:], raw); err != nil {
				return err
			}
			if n == bound-2 {
				// With the row of the put, the row the bound is at.
				atBound = last
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	items, upto, err := store.ReadOutbox(ctx, outboxSecond, delivery.Seq{}, delivery.MaxOutboxRead)
	if err != nil || len(items) != 0 || upto != atBound {
		t.Fatalf("a read over %d rows of another target = %d rows up to %s, %v; want none, up to row %d of them, %s", bound+1, len(items), upto, err, bound, atBound)
	}
	if items, upto, err = store.ReadOutbox(ctx, outboxSecond, upto, delivery.MaxOutboxRead); err != nil || len(items) != 0 || upto != last {
		t.Errorf("the read repeated from there = %d rows up to %s, %v; want none, up to the last row %s", len(items), upto, err, last)
	}
	// The bound is of the rows looked at, not of those returned: a target
	// that has them all is given a read's worth.
	if items, upto, err = store.ReadOutbox(ctx, outboxFirst, delivery.Seq{}, delivery.MaxOutboxRead); err != nil || len(items) != delivery.MaxOutboxRead || upto != items[len(items)-1].Seq {
		t.Errorf("a read for the target of every row = %d rows up to %s, %v; want %d", len(items), upto, err, delivery.MaxOutboxRead)
	}
}

// The bytes of schema 5 are frozen (PLAN 8.0.10): since this release
// announces schema 5, members of two releases write and read its entries and
// its records, and what one writes the other must read. These are the SHA-256
// of the two golden files as they stood when schema 5 was announced.
const (
	schema5EntriesFrozen = "4f4222da822e819a4421f590aefc260141948ef2b86b6303f296a8704e952107"
	schema5RecordsFrozen = "a5a082a02a100b8313b35234594281bc9c00f4a93a27911ab531088c385f6c2d"
)

// TestTheGoldenFilesOfSchema5AreFrozen: the two files that pin the entries
// and the records of schema 5 are the bytes they were when schema 5 was
// announced. TestTheEntriesOfSchema5ArePinned and
// TestTheRecordsOfSchema5ArePinned hold the code to the files; this holds
// the files to themselves, so that a change of the codec cannot pass by
// being written into them. A change of either is a change of what two
// releases write: it needs a schema 6, with its ADR, its migration and its
// upgrade test - not new sums here.
func TestTheGoldenFilesOfSchema5AreFrozen(t *testing.T) {
	t.Parallel()
	for name, frozen := range map[string]string{schema5EntriesFile: schema5EntriesFrozen, schema5RecordsFile: schema5RecordsFrozen} {
		content, err := fs.ReadFile(os.DirFS("testdata"), name)
		if err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256(content); hex.EncodeToString(sum[:]) != frozen {
			t.Errorf("testdata/%s was changed (SHA-256 %x): the bytes of schema 5 are frozen since it was announced, and a change of them needs a schema 6", name, sum)
		}
	}
	if MetadataVersion != entrySchema5 {
		t.Errorf("MetadataVersion = %d: when a schema above 5 is announced, its golden files are frozen beside these", MetadataVersion)
	}
}
