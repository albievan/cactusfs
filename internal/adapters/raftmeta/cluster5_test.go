package raftmeta

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// Entry schema 5 in a cluster (ADR-0023, "Level rule and state schema";
// PLAN 8.0.10): the contracts of its ports asked of a node that neither
// leads nor votes, the rolling upgrade from the release of schema 4, and
// what a member that has not registered, a proposer behind its leader and a
// node of the release before make of it.

// caughtUp is a node of a cluster as the contract of a port asks it: what
// the port says is answered from the node's own copy of the state, without
// asking the leader (ports.BucketUsageStore.BucketUsages), is asked once the
// node has applied everything that was committed before the question. The
// contract is written for one store, in which a change is read back at
// once; a node that does not lead applies what it proposed a moment after
// the leader answered. The read itself stays the node's own.
type caughtUp struct {
	*Store
}

// BucketUsages implements ports.BucketUsageStore.
func (c caughtUp) BucketUsages(ctx context.Context, names []string) ([]ports.BucketUsage, error) {
	if err := c.WaitReady(ctx); err != nil {
		return nil, err
	}
	return c.Store.BucketUsages(ctx, names)
}

// clusterAtLevel5 forms a cluster of three voters and a node without a
// vote, all of this release, has every member register, and returns it when
// the node without a vote has seen that the cluster writes schema 5. prepare,
// when it is given, is run on every store before anything is proposed.
func clusterAtLevel5(t *testing.T, prepare func(*Store)) *testCluster {
	t.Helper()
	c := newTestCluster(t)
	nodes := []string{"m1", "m2", "m3", "s4"}
	c.form(nodes[:3], nodes[3:])
	if prepare != nil {
		for _, id := range nodes {
			prepare(c.store(id))
		}
	}
	c.register(nodes...)
	eventually(t, "the node to see every member on schema 5", func() bool {
		level, err := c.store("s4").entryVersion()
		return err == nil && level == entrySchema5
	})
	return c
}

// TestEveryNodeOfAClusterHonoursTheContractOfSchema5: what schema 5 added -
// the usage of buckets and their quotas, targets, the outbox, replica
// buckets and the record of a resync - asked of a node that neither leads
// nor votes, in a cluster whose members have all said that they apply it.
// Every change is forwarded to the leader, with the answer of the state
// machine carried back (the record a walk left, the results of an
// acknowledgement that were not its target's), and every read but the usage
// of buckets waits for the leader's index.
func TestEveryNodeOfAClusterHonoursTheContractOfSchema5(t *testing.T) {
	t.Parallel()
	porttest.Schema5Contract(t, func(t *testing.T) porttest.Schema5 {
		return caughtUp{clusterAtLevel5(t, nil).store("s4")}
	})
}

// TestEveryNodeOfAClusterHonoursTheContractOfTheOutboxBound: what happens at
// the bound of the outbox, asked of the same node. The bound is part of the
// replicated format, so every member is given the same.
func TestEveryNodeOfAClusterHonoursTheContractOfTheOutboxBound(t *testing.T) {
	t.Parallel()
	porttest.OutboxBoundContract(t, outboxBoundForTest, func(t *testing.T) porttest.Schema5 {
		c := clusterAtLevel5(t, func(s *Store) { setOutboxBound(s.fsm, outboxBoundForTest) })
		return caughtUp{c.store("s4")}
	})
}

// schema5Buckets are the buckets of bbolt that only schema 5 makes.
func schema5Buckets() [][]byte {
	return [][]byte{bucketUsageBucket(), targetsBucket(), targetRefsBucket(), outboxBucket(), outboxCursorBucket(), resyncBucket()}
}

// siteA5 stands for the ID of another deployment.
const siteA5 = "5d3c1f0a-site-a"

// schema5Proposers is every change that has no form below schema 5, each on
// the bucket named: the seven commands, the two kinds of bucket
// configuration, and the three old commands with what makes a version, a
// delete marker or an upload the copy of one of another deployment. Every
// one is well formed - what the store refuses by itself is not among them -
// so that below level 5 the only thing that can refuse it is the level.
func schema5Proposers(t *testing.T, s *Store, bucketName string) map[string]func(ctx context.Context) error {
	t.Helper()
	notification, err := bucket.NotificationConfig{Rules: []bucket.NotificationRule{
		{ID: "all", As: bucket.NotifyAsQueue, Target: "hook", On: []delivery.Event{delivery.EventPut}},
	}}.Document()
	if err != nil {
		t.Fatal(err)
	}
	replication, err := bucket.ReplicationConfig{Target: "site-b", Bucket: "copy", Rules: []bucket.ReplicationRule{{ID: "r", Enabled: true}}}.Document()
	if err != nil {
		t.Fatal(err)
	}
	replica := &object.Replica{From: siteA5, Seq: 7}
	id := object.ID(bytes.Repeat([]byte{0x5a}, object.IDSize))
	return map[string]func(ctx context.Context) error{
		"a quota":         func(ctx context.Context) error { return s.SetBucketQuota(ctx, bucketName, 1<<20, audit.Record{}) },
		"a quota removed": func(ctx context.Context) error { return s.SetBucketQuota(ctx, bucketName, 0, audit.Record{}) },
		"a walk": func(ctx context.Context) error {
			_, err := s.WalkBucketUsage(ctx, bucketName, 50, false)
			return err
		},
		"a recount": func(ctx context.Context) error {
			_, err := s.WalkBucketUsage(ctx, bucketName, 50, true)
			return err
		},
		"a target": func(ctx context.Context) error {
			return s.PutTarget(ctx, porttest.SampleTarget("hook", delivery.KindWebhook), audit.Record{})
		},
		"a target deleted": func(ctx context.Context) error { return s.DeleteTarget(ctx, "hook", audit.Record{}) },
		"an acknowledgement": func(ctx context.Context) error {
			_, err := s.AckOutbox(ctx, "hook", delivery.Seq{Index: 1}, 0, nil)
			return err
		},
		"replica mode": func(ctx context.Context) error { return s.SetBucketReplica(ctx, bucketName, siteA5, audit.Record{}) },
		"a promotion":  func(ctx context.Context) error { return s.SetBucketReplica(ctx, bucketName, "", audit.Record{}) },
		"a resync": func(ctx context.Context) error {
			return s.PutResync(ctx, bucket.Resync{Bucket: bucketName, Target: "site-b", State: bucket.ResyncRunning})
		},
		"notification rules": func(ctx context.Context) error {
			return s.SetBucketConfig(ctx, bucketName, bucket.ConfigNotification, notification)
		},
		"notification rules removed": func(ctx context.Context) error {
			return s.SetBucketConfig(ctx, bucketName, bucket.ConfigNotification, nil)
		},
		"a replication configuration": func(ctx context.Context) error {
			return s.SetBucketConfig(ctx, bucketName, bucket.ConfigReplication, replication)
		},
		"a replica put": func(ctx context.Context) error {
			v := porttest.SampleVersion(bucketName, "copied", 0x5a)
			v.Replica = replica
			_, err := s.PutVersion(ctx, v)
			return err
		},
		"a replica delete": func(ctx context.Context) error {
			_, err := s.DeleteObject(ctx, bucketName, "copied", ports.DeleteOptions{MarkerID: id, At: time.Unix(1, 0), Replica: replica})
			return err
		},
		"an upload of a source": func(ctx context.Context) error {
			u := porttest.SampleUpload(bucketName, "copied-upload", 0x5a)
			u.ReplicaFrom = siteA5
			return s.CreateUpload(ctx, u)
		},
		"a completion as a replica": func(ctx context.Context) error {
			_, err := s.CompleteUpload(ctx, ports.Completion{
				Bucket: bucketName, Key: "copied-upload", ID: id, VersionID: id, ETag: "etag", At: time.Unix(1, 0), Replica: replica,
				Parts: []object.Part{{Number: 1, ETag: "e", Seal: bytes.Repeat([]byte{1}, 32)}},
			})
			return err
		},
	}
}

// TestRollingUpgradeToSchema5: a cluster of the release before this one -
// entry schema 4 - is upgraded node by node (ADR-0023, "Level rule and state
// schema", "Migration", "Rollback"). While any member is of that release, or
// has not said that it is not, nothing of schema 5 is logged: a bucket is
// created as it always was and is created all the same; a put, a delete that
// says who, a multipart upload and an abort succeed as they did; no member
// keeps a usage record, a target or a row; every change that has no older
// form is answered "unavailable", definitely and with nothing logged, through
// a node of either release; and every state is one the release of schema 4
// opens. Nothing that worked before is refused, on any node, at any step.
//
// When the last member has registered, the level is 5 and STILL nothing of
// schema 5 is in any state: registering proposes nothing of it, and this
// build has no worker that counts without being asked (PLAN 8.3.1). The
// state becomes schema 5 with the first entry of schema 5 - here, as in the
// product today, the first bucket that is created - and that is where the
// way back to the release of schema 4 ends. The new bucket is counted from
// the start; a bucket that was there is counted by walk entries, a few
// records at a time, through any node, to exactly what it holds; a quota is
// held; and the usage records are the same bytes on every member, and still
// after the leader has changed.
func TestRollingUpgradeToSchema5(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	nodes := []string{"m1", "m2", "m3", "s4"}
	c.allOfSchema4(nodes...)
	c.form(nodes[:3], nodes[3:])
	c.register(nodes...)
	for _, id := range nodes {
		eventually(t, "node "+id+" to see every member on schema 4", func() bool {
			level, err := c.store(id).entryVersion()
			return err == nil && level == entrySchema4
		})
	}
	created := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	var serial byte
	next := func() byte {
		serial++
		return serial
	}
	// use is everything that worked before schema 5, through one node, in
	// a bucket of its own that keeps versions or does not: a bucket created
	// by somebody, puts, a delete that says who, a multipart upload that is
	// completed, one that is aborted and one left in progress. It returns
	// what the bucket then holds.
	use := func(s *Store, name string, versioned bool) bucket.UsageTotals {
		t.Helper()
		through := "through " + s.NodeID()
		if err := s.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: created, CreatedBy: "user/albie"}); err != nil {
			t.Fatalf("CreateBucket %s %s: %v", name, through, err)
		}
		if versioned {
			if err := s.SetBucketVersioning(ctx, name, bucket.VersioningEnabled); err != nil {
				t.Fatalf("SetBucketVersioning %s %s: %v", name, through, err)
			}
		}
		put := func(key string, size int64) {
			t.Helper()
			v := porttest.SampleVersion(name, key, next())
			v.Size, v.CreatedBy = size, "user/albie"
			if _, err := s.PutVersion(ctx, v); err != nil {
				t.Fatalf("PutVersion %s/%s %s: %v", name, key, through, err)
			}
		}
		put("kept", 1000)
		put("kept", 300)
		put("gone", 70)
		marker := object.ID(bytes.Repeat([]byte{next()}, object.IDSize))
		if _, err := s.DeleteObject(ctx, name, "gone", ports.DeleteOptions{MarkerID: marker, At: created, By: "user/albie"}); err != nil {
			t.Fatalf("a delete that says who, in %s %s: %v", name, through, err)
		}
		upload := func(key string, sizes ...int64) (object.Upload, []object.Part) {
			t.Helper()
			u := porttest.SampleUpload(name, key, next())
			if err := s.CreateUpload(ctx, u); err != nil {
				t.Fatalf("CreateUpload %s/%s %s: %v", name, key, through, err)
			}
			var parts []object.Part
			for i, size := range sizes {
				part := porttest.SamplePart(u, i+1, size, 0)
				if _, err := s.PutPart(ctx, name, key, u.ID, part); err != nil {
					t.Fatalf("PutPart %s/%s %s: %v", name, key, through, err)
				}
				part.Seal = bytes.Repeat([]byte{1}, 32)
				parts = append(parts, part)
			}
			return u, parts
		}
		u, parts := upload("upload", 40)
		if _, err := s.CompleteUpload(ctx, ports.Completion{
			Bucket: name, Key: u.Key, ID: u.ID, VersionID: object.ID(bytes.Repeat([]byte{next()}, object.IDSize)),
			ETag: "etag", At: created, CreatedBy: "key/AKID", Parts: parts,
		}); err != nil {
			t.Fatalf("CompleteUpload %s/%s %s: %v", name, u.Key, through, err)
		}
		u, _ = upload("aborted", 500, 600)
		if _, err := s.AbortUpload(ctx, name, u.Key, u.ID); err != nil {
			t.Fatalf("AbortUpload %s/%s %s: %v", name, u.Key, through, err)
		}
		upload("pending", 77, 23)
		if b, err := s.GetBucket(ctx, name); err != nil || b.CreatedBy != "user/albie" {
			t.Fatalf("the bucket %s %s was created by %q, %v: who created a bucket is of schema 4", name, through, b.CreatedBy, err)
		}
		if versioned {
			// Every version is kept, and the delete made a marker, which
			// is a version of no bytes.
			return bucket.UsageTotals{Objects: 2, CurrentBytes: 340, Versions: 5, StoredBytes: 1410, UploadBytes: 100}
		}
		return bucket.UsageTotals{Objects: 2, CurrentBytes: 340, Versions: 2, StoredBytes: 340, UploadBytes: 100}
	}
	// held is what every bucket made so far holds, by name.
	held := map[string]bucket.UsageTotals{}
	var names []string
	work := func(s *Store, name string) {
		t.Helper()
		for i, versioned := range []bool{false, true} {
			full := fmt.Sprintf("%s-%d", name, i)
			held[full] = use(s, full, versioned)
			names = append(names, full)
		}
	}
	// nothingOfSchema5 says of every node that it holds nothing schema 5
	// keeps and is in a state the release of schema 4 opens.
	nothingOfSchema5 := func(step string) {
		t.Helper()
		for _, id := range nodes {
			s := c.store(id)
			if err := s.WaitReady(ctx); err != nil {
				t.Fatalf("%s: node %s: %v", step, id, err)
			}
			got, err := s.BucketUsages(ctx, names)
			if err != nil {
				t.Fatalf("%s: BucketUsages on %s: %v", step, id, err)
			}
			for i, u := range got {
				if u.Usage != (bucket.Usage{}) || u.QuotaBytes != 0 {
					t.Errorf("%s: node %s keeps a usage record of %s: %+v", step, id, names[i], u)
				}
			}
			if targets, err := s.ListTargets(ctx); err != nil || len(targets) != 0 {
				t.Errorf("%s: node %s has the targets %v, %v", step, id, targets, err)
			}
			if raw := rawState(t, s.fsm, schema5Buckets()...); len(raw) != 0 {
				t.Errorf("%s: node %s holds %v", step, id, raw)
			}
			if schema := stateSchema(t, s.fsm); schema != stateSchema4 {
				t.Errorf("%s: node %s: state schema %d, want %d", step, id, schema, stateSchema4)
			}
			if err := s.Check(ctx); err != nil {
				t.Errorf("%s: node %s is not healthy: %v", step, id, err)
			}
		}
	}
	// unavailable asks a node for every change that has no form below
	// schema 5: each is answered "unavailable", definitely, and the leader
	// has logged nothing for it.
	unavailable := func(step string, s *Store) {
		t.Helper()
		leader := c.store(c.leader())
		before, err := leader.fsm.appliedIndex()
		if err != nil {
			t.Fatal(err)
		}
		proposers := schema5Proposers(t, s, names[0])
		for what, propose := range proposers {
			err := propose(ctx)
			if !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrOutcomeUnknown) || errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Errorf("%s: %s through %s = %v, want a definite %v and nothing else", step, what, s.NodeID(), err, domain.ErrUnavailable)
			}
		}
		if len(proposers) < 17 {
			t.Errorf("%d changes of schema 5 were asked for: the test proves less than it says", len(proposers))
		}
		if after, err := leader.fsm.appliedIndex(); err != nil || after != before {
			t.Errorf("%s: what is unavailable through %s was logged: the leader is at entry %d (%v), was at %d", step, s.NodeID(), after, err, before)
		}
	}

	// The release before at work.
	work(c.store("s4"), "before")
	nothingOfSchema5("before the upgrade")

	for i, id := range nodes {
		// The node is stopped and comes back with this release.
		c.stop(id)
		c.mu.Lock()
		c.schema4[id] = false
		c.mu.Unlock()
		store := c.start(id, id != "s4")
		c.ready(id)
		if store.supported != entrySchema5 || store.fsm.supported != entrySchema5 {
			t.Fatalf("node %s came back applying schema %d", id, store.supported)
		}
		// Twice: before the node has registered what it now applies, and
		// after, while members of the release before remain.
		for _, registered := range []bool{false, true} {
			if registered {
				c.register(id)
				if i == len(nodes)-1 {
					break
				}
			}
			step := fmt.Sprintf("upgrade step %d, registered %v", i, registered)
			work(store, fmt.Sprintf("during-%d-%t", i, registered))
			unavailable(step, store)
			// Through every other node as well, of either release.
			for _, other := range nodes {
				v := porttest.SampleVersion(names[len(names)-1], "k-"+other, next())
				v.Size = 0
				if _, err := c.store(other).PutVersion(ctx, v); err != nil {
					t.Fatalf("%s: PutVersion on %s: %v", step, other, err)
				}
				if _, err := c.store(other).DeleteObject(ctx, v.Bucket, v.Key, ports.DeleteOptions{Version: &object.VersionRef{ID: v.VersionID}, By: "root"}); err != nil {
					t.Fatalf("%s: DeleteObject on %s: %v", step, other, err)
				}
			}
			if last := nodes[len(nodes)-1]; last != id {
				unavailable(step, c.store(last))
			}
			nothingOfSchema5(step)
		}
	}

	// All four run this release and have said so: the cluster is at level
	// 5. Nothing of schema 5 was written - the registration is no entry of
	// it, and nothing counts without being asked - so every state is still
	// one the release of schema 4 opens: the upgrade could be undone.
	for _, id := range nodes {
		eventually(t, "node "+id+" to see every member on schema 5", func() bool {
			level, err := c.store(id).entryVersion()
			return err == nil && level == entrySchema5
		})
	}
	nothingOfSchema5("after the last registration")
	if len(names) < 14 {
		t.Fatalf("%d buckets were made before schema 5 was used: the test proves less than it says", len(names))
	}

	// The first bucket that is created is the first entry of schema 5:
	// from here on there is no way back. It is counted from the start, and
	// so is every bucket after it, through every node.
	old := append([]string(nil), names...)
	for n, id := range nodes {
		work(c.store(id), "after-"+id)
		if n > 0 {
			continue
		}
		for _, at := range nodes {
			if err := c.store(at).WaitReady(ctx); err != nil {
				t.Fatalf("node %s: %v", at, err)
			}
			if schema := stateSchema(t, c.store(at).fsm); schema != stateSchema5 {
				t.Errorf("after the first bucket of schema 5, node %s: state schema %d", at, schema)
			}
		}
	}
	fresh := names[len(old):]
	usages := func(s *Store, of []string) []ports.BucketUsage {
		t.Helper()
		got, err := caughtUp{s}.BucketUsages(ctx, of)
		if err != nil {
			t.Fatalf("BucketUsages on %s: %v", s.NodeID(), err)
		}
		return got
	}
	for _, id := range nodes {
		for i, u := range usages(c.store(id), fresh) {
			if !u.Usage.Exact || u.Usage.Walking() || u.Usage.UsageTotals != held[fresh[i]] {
				t.Errorf("node %s: the bucket %s, made at level 5, holds exact = %v, %+v; want %+v", id, fresh[i], u.Usage.Exact, u.Usage.UsageTotals, held[fresh[i]])
			}
		}
		// A bucket that was there has no record until it is counted.
		for i, u := range usages(c.store(id), old) {
			if u.Usage != (bucket.Usage{}) {
				t.Errorf("node %s: the bucket %s, made before schema 5, has the record %+v before it was counted", id, old[i], u.Usage)
			}
		}
	}

	// The buckets that were there are counted, two records an entry, each
	// entry through another node: whichever proposes it, the walk goes on
	// from where the record says it is.
	longest := 0
	for n, name := range old {
		if err := c.store(nodes[n%len(nodes)]).SetBucketQuota(ctx, name, 1<<30, audit.Record{}); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("a quota for %s, which was not counted yet = %v, want %v", name, err, domain.ErrConflict)
		}
		steps := 0
		for {
			walk, err := c.store(nodes[(n+steps)%len(nodes)]).WalkBucketUsage(ctx, name, 2, false)
			if err != nil {
				t.Fatalf("entry %d of the walk through %s: %v", steps, name, err)
			}
			steps++
			if !walk.Usage.Walking() {
				if walk.Drift || !walk.Usage.Exact || walk.Usage.UsageTotals != held[name] {
					t.Errorf("the walk through %s ended with drift = %v, exact = %v, %+v; want %+v", name, walk.Drift, walk.Usage.Exact, walk.Usage.UsageTotals, held[name])
				}
				break
			}
			// While a bucket is counted for the first time nothing may
			// be read from its totals, and it is not exact.
			if walk.Usage.Exact || walk.Usage.UsageTotals != (bucket.UsageTotals{}) {
				t.Errorf("entry %d of the first count of %s = exact %v, %+v", steps, name, walk.Usage.Exact, walk.Usage.UsageTotals)
			}
			if steps > 100 {
				t.Fatalf("the walk through %s does not come to an end", name)
			}
		}
		longest = max(longest, steps)
		// And a recount finds what the first count found.
		for restart := true; ; restart = false {
			walk, err := c.store(nodes[n%len(nodes)]).WalkBucketUsage(ctx, name, ports.MaxUsageWalk, restart)
			if err != nil || walk.Drift || walk.Usage.UsageTotals != held[name] {
				t.Fatalf("a recount of %s = %+v, %v; want %+v and no drift", name, walk, err, held[name])
			}
			if !walk.Usage.Walking() {
				break
			}
		}
	}
	if longest < 3 {
		t.Errorf("the longest walk took %d entries: the test proves less than it says", longest)
	}

	// same says that every running node holds the same usage records and
	// the same buckets, byte for byte, and returns what they hold.
	same := func(step string, ids ...string) map[string]string {
		t.Helper()
		var want map[string]string
		for _, id := range ids {
			s := c.store(id)
			if err := s.WaitReady(ctx); err != nil {
				t.Fatalf("%s: node %s: %v", step, id, err)
			}
			for i, u := range usages(s, names) {
				if !u.Usage.Exact || u.Usage.Walking() || u.Usage.UsageTotals != held[names[i]] {
					t.Errorf("%s: node %s: %s holds exact = %v, %+v; want %+v", step, id, names[i], u.Usage.Exact, u.Usage.UsageTotals, held[names[i]])
				}
			}
			if schema := stateSchema(t, s.fsm); schema != stateSchema5 {
				t.Errorf("%s: node %s: state schema %d after schema 5 was used", step, id, schema)
			}
			raw := rawState(t, s.fsm, bucketUsageBucket(), bucketsBucket())
			if want == nil {
				want = raw
			}
			if !reflect.DeepEqual(raw, want) {
				t.Errorf("%s: node %s holds other records than %s:\n%v\n%v", step, id, ids[0], raw, want)
			}
		}
		return want
	}
	records := same("after the upgrade", nodes...)
	if len(records) != 2+2*len(names) {
		t.Errorf("the nodes hold %d places, want a usage record and a bucket record for each of %d buckets: %v", len(records), len(names), records)
	}

	// A quota is held, through a node that does not lead: on a bucket that
	// was there before the upgrade, now that it is counted.
	limited := old[0]
	if err := c.store("s4").SetBucketQuota(ctx, limited, held[limited].StoredBytes+held[limited].UploadBytes+10, audit.Record{}); err != nil {
		t.Fatalf("a quota for %s: %v", limited, err)
	}
	over := porttest.SampleVersion(limited, "over", next())
	over.Size = 11
	if _, err := c.store("s4").PutVersion(ctx, over); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Errorf("a put of 11 bytes into a bucket with room for 10 = %v, want %v", err, domain.ErrQuotaExceeded)
	}
	over.Size = 10
	if _, err := c.store("s4").PutVersion(ctx, over); err != nil {
		t.Errorf("a put of 10 bytes into a bucket with room for 10: %v", err)
	}
	total := held[limited]
	total.Objects, total.Versions, total.CurrentBytes, total.StoredBytes = total.Objects+1, total.Versions+1, total.CurrentBytes+10, total.StoredBytes+10
	held[limited] = total

	// The leader goes. The others elect a new one, which keeps the records
	// as the old one did; the old one comes back and catches up.
	gone := c.leader()
	c.stop(gone)
	var rest []string
	for _, id := range nodes {
		if id != gone {
			rest = append(rest, id)
		}
	}
	through := rest[len(rest)-1]
	counted := fresh[0]
	late := porttest.SampleVersion(counted, "after-the-leader", next())
	late.Size = 9
	eventually(t, "a new leader to take a write", func() bool {
		_, err := c.store(through).PutVersion(ctx, late)
		return err == nil
	})
	total = held[counted]
	total.Objects, total.Versions, total.CurrentBytes, total.StoredBytes = total.Objects+1, total.Versions+1, total.CurrentBytes+9, total.StoredBytes+9
	held[counted] = total
	same("while the old leader is away", rest...)
	c.start(gone, gone != "s4")
	c.ready(gone)
	if changed := same("after the leader changed", nodes...); reflect.DeepEqual(changed, records) {
		t.Error("the put after the leader changed is in no record")
	}

	// A bucket that is deleted takes its record with it, on every node.
	if err := c.store(through).CreateBucket(ctx, bucket.Bucket{Name: "passing", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	if u := usages(c.store(through), []string{"passing"}); !u[0].Usage.Exact {
		t.Errorf("the usage of a new bucket = %+v", u[0])
	}
	if err := c.store(through).DeleteBucket(ctx, "passing"); err != nil {
		t.Fatal(err)
	}
	for _, id := range nodes {
		if u := usages(c.store(id), []string{"passing"}); u[0].Usage != (bucket.Usage{}) {
			t.Errorf("node %s still has a usage record of the deleted bucket: %+v", id, u[0])
		}
		if raw := usageBytes(t, c.store(id).fsm, "passing"); raw != nil {
			t.Errorf("node %s still holds the usage record of the deleted bucket: %s", id, raw)
		}
	}
}

// TestSchema5WhenAMemberHasNotRegistered: before anything of schema 5 is in
// the state, a member that has not said which schema it applies holds the
// cluster below it - it might be of the release before - and what has an
// older form is written in it, not refused: a bucket is created, and has no
// usage record. What has none is unavailable. Once the state holds what only
// schema 5 expresses, no member of that release can be in the cluster: it
// stopped at the first such entry. So a member whose record is gone no
// longer takes the cluster back (Store.entryVersion): buckets go on being
// counted from the start, and a quota can be set.
func TestSchema5WhenAMemberHasNotRegistered(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := clusterAtLevel5(t, nil)
	nodes := []string{"m1", "m2", "m3", "s4"}
	store := c.store("m1")
	if err := store.RemoveNode(ctx, "s4"); err != nil {
		t.Fatal(err)
	}
	// (Whether m1 leads or not, and so whether it knows of the removal
	// yet: one that does not is told by the leader, and writes below.)
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "held-back", CreatedBy: "user/albie"}); err != nil {
		t.Fatalf("a bucket with an unregistered member = %v, want it created", err)
	}
	if _, err := store.PutVersion(ctx, porttest.SampleVersion("held-back", "k", 1)); err != nil {
		t.Fatalf("a put with an unregistered member: %v", err)
	}
	if err := store.SetBucketQuota(ctx, "held-back", 1<<20, audit.Record{}); !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Errorf("a quota with an unregistered member = %v, want a definite %v", err, domain.ErrUnavailable)
	}
	if _, err := store.WalkBucketUsage(ctx, "held-back", 10, false); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("a walk with an unregistered member = %v, want %v", err, domain.ErrUnavailable)
	}
	for _, id := range nodes {
		c.ready(id)
		if schema := stateSchema(t, c.store(id).fsm); schema >= stateSchema5 {
			t.Errorf("node %s: state schema %d before schema 5 was used", id, schema)
		}
		if raw := rawState(t, c.store(id).fsm, schema5Buckets()...); len(raw) != 0 {
			t.Errorf("node %s holds %v", id, raw)
		}
	}

	c.register("s4")
	eventually(t, "schema 5 to be used", func() bool {
		level, err := store.entryVersion()
		return err == nil && level == entrySchema5
	})
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "first"}); err != nil {
		t.Fatal(err)
	}
	exact := func(s *Store, name string) bool {
		t.Helper()
		got, err := caughtUp{s}.BucketUsages(ctx, []string{name})
		if err != nil {
			t.Fatalf("BucketUsages on %s: %v", s.NodeID(), err)
		}
		return got[0].Usage.Exact
	}
	if !exact(store, "first") || exact(store, "held-back") {
		t.Fatalf("the first bucket at schema 5 is counted: %v; the one made below it: %v", exact(store, "first"), exact(store, "held-back"))
	}

	// From now on it cannot be held back.
	if err := store.RemoveNode(ctx, "s4"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"m1", "s4"} {
		name := "through-" + id
		// The node writes the bucket in the schema it believes the cluster
		// takes: it must have applied the first entry of schema 5, or it
		// might still hold the cluster below it and write the bucket so.
		c.ready(id)
		if err := c.store(id).CreateBucket(ctx, bucket.Bucket{Name: name}); err != nil {
			t.Fatalf("a bucket through %s with an unregistered member, after schema 5 was used: %v", id, err)
		}
		if !exact(c.store(id), name) {
			t.Errorf("through %s, with an unregistered member, the bucket has no usage record", id)
		}
		if err := c.store(id).SetBucketQuota(ctx, name, 1<<20, audit.Record{}); err != nil {
			t.Errorf("a quota through %s with an unregistered member, after schema 5 was used: %v", id, err)
		}
	}
	// And the bucket that was made below is counted when it is asked for.
	for steps := 0; ; steps++ {
		walk, err := c.store("s4").WalkBucketUsage(ctx, "held-back", 10, false)
		if err != nil || steps > 4 {
			t.Fatalf("entry %d of the walk through the bucket made below schema 5: %v", steps, err)
		}
		if !walk.Usage.Walking() {
			if want := (bucket.UsageTotals{Objects: 1, CurrentBytes: 1000, Versions: 1, StoredBytes: 1000}); walk.Usage.UsageTotals != want {
				t.Errorf("the bucket made below schema 5 holds %+v, want %+v", walk.Usage.UsageTotals, want)
			}
			break
		}
	}

	// Nor by a member that says it applies less. Every member is registered
	// again, and one of them as of the release of schema 4 - which cannot be
	// in the cluster: it stopped at the first entry of schema 5. The state
	// holds what only schema 5 expresses, so the cluster goes on writing it.
	behind := cluster.Node{ID: "s4", RPCAddr: "s4:9002", RaftAddr: c.members["s4"].RaftAddr, Roles: cluster.AllRoles(), MetadataVersion: entrySchema4}
	if err := store.SetNode(ctx, behind); err != nil {
		t.Fatal(err)
	}
	for _, id := range nodes {
		// Each has applied the registration, and judges by it.
		c.ready(id)
		recorded, err := c.store(id).ListNodes(ctx)
		if err != nil || !slices.ContainsFunc(recorded, func(n cluster.Node) bool { return n.ID == "s4" && n.MetadataVersion == entrySchema4 }) {
			t.Fatalf("node %s knows the nodes %+v, %v; want s4 registered at schema 4", id, recorded, err)
		}
		if level, err := c.store(id).entryVersion(); err != nil || level != entrySchema5 {
			t.Errorf("node %s, with every member registered and one of them at schema 4, after schema 5 was used: level %d, %v; want %d", id, level, err, entrySchema5)
		}
	}
	for _, id := range []string{"m1", "s4"} {
		name := "registered-below-through-" + id
		if err := c.store(id).CreateBucket(ctx, bucket.Bucket{Name: name}); err != nil {
			t.Fatalf("a bucket through %s with a member registered at schema 4, after schema 5 was used: %v", id, err)
		}
		if !exact(c.store(id), name) {
			t.Errorf("through %s, with a member registered at schema 4, the bucket has no usage record", id)
		}
	}
}

// TestALeaderRefusesAnEntryOfSchema5BelowLevel5: what a node that knows the
// cluster a moment late proposes as schema 5 is refused by the leader -
// nothing is logged - and written again as the cluster takes it, where it
// has an older form: a bucket is created, with its creator and without a
// usage record; a delete goes without who deleted. What has none is answered
// "unavailable" - never as a format nobody understands, which nobody tries
// again - and is sent once.
func TestALeaderRefusesAnEntryOfSchema5BelowLevel5(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := clusterAtLevel5(t, nil)
	nodes := []string{"m1", "m2", "m3", "s4"}
	store := c.store("s4")
	// From here on s4 believes the cluster to be at schema 5, and its
	// leader says 4. (s4 has no vote and never leads: every proposal of
	// its own goes through this client.)
	behind := &behindLeader{LeaderClient: store.leader, level: entrySchema4}
	store.leader = behind

	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "behind", CreatedBy: "user/albie"}); err != nil {
		t.Fatalf("a bucket through a node behind its leader: %v", err)
	}
	if got := behind.proposed(); !bytes.Equal(got, []byte{entrySchema5, entrySchema4}) {
		t.Errorf("the bucket was proposed in the schemas %v, want 5 and then 4", got)
	}
	if err := store.SetBucketVersioning(ctx, "behind", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutVersion(ctx, porttest.SampleVersion("behind", "k", 1)); err != nil {
		t.Fatal(err)
	}
	behind.proposed()
	marker := object.ID(bytes.Repeat([]byte{7}, object.IDSize))
	deleted, err := store.DeleteObject(ctx, "behind", "k", ports.DeleteOptions{MarkerID: marker, At: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC), By: "user/albie"})
	if err != nil || deleted.Marker == nil {
		t.Fatalf("a delete that says who, through a node behind its leader = %+v, %v", deleted, err)
	}
	if got := behind.proposed(); !bytes.Equal(got, []byte{entrySchema5, entrySchemaTimed}) {
		t.Errorf("the delete was proposed in the schemas %v, want 5 and then 2, without who deleted", got)
	}
	proposers := schema5Proposers(t, store, "behind")
	for what, propose := range proposers {
		behind.proposed()
		err := propose(ctx)
		if !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrUnsupportedFormat) || errors.Is(err, domain.ErrOutcomeUnknown) {
			t.Errorf("%s through a node behind its leader = %v, want a definite %v and nothing else", what, err, domain.ErrUnavailable)
		}
		// Sent once: when the leader has said that the cluster is not at
		// schema 5, the same entry is not sent again.
		if got := behind.proposed(); !bytes.Equal(got, []byte{entrySchema5}) {
			t.Errorf("%s was proposed in the schemas %v, want 5 and no more", what, got)
		}
	}
	// Nothing of schema 5 was logged: every state is still one the release
	// of schema 4 opens, and the bucket is the one that release makes.
	for _, id := range nodes {
		c.ready(id)
		if raw := rawState(t, c.store(id).fsm, schema5Buckets()...); len(raw) != 0 {
			t.Errorf("node %s holds %v", id, raw)
		}
		if schema := stateSchema(t, c.store(id).fsm); schema != stateSchema4 {
			t.Errorf("node %s: state schema %d, want %d", id, schema, stateSchema4)
		}
	}
	if b, err := store.GetBucket(ctx, "behind"); err != nil || b.CreatedBy != "user/albie" {
		t.Errorf("the bucket was created by %q, %v", b.CreatedBy, err)
	}
}

// TestAPutOfSchema4IsCountedWhereThereIsARecord: a node that learns a moment
// late that the cluster is at level 5 writes a put, a part and a delete as it
// wrote them at level 4. The bucket has a usage record, and every one of them
// is counted all the same: the switch of a counter is its record, not the
// schema byte of the entry (ADR-0023, "The switch - and why it is not the
// schema byte").
func TestAPutOfSchema4IsCountedWhereThereIsARecord(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := clusterAtLevel5(t, nil)
	store := c.store("s4")
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "counted"}); err != nil {
		t.Fatal(err)
	}
	// From here on s4 believes the cluster at level 4, and is of a release
	// that writes nothing above it: its entries arrive as schema 4 and
	// below at a leader that takes them, the cluster being at level 5.
	recording := &behindLeader{LeaderClient: store.leader, level: entrySchema5}
	store.leader = recording
	store.supported = entrySchema4
	t.Cleanup(func() { store.supported = entrySchema5 })

	v := porttest.SampleVersion("counted", "k", 1)
	v.Size, v.CreatedBy = 600, "user/albie"
	if _, err := store.PutVersion(ctx, v); err != nil {
		t.Fatal(err)
	}
	gone := porttest.SampleVersion("counted", "gone", 2)
	gone.Size = 50
	if _, err := store.PutVersion(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteObject(ctx, "counted", "gone", ports.DeleteOptions{By: "user/albie"}); err != nil {
		t.Fatal(err)
	}
	u := porttest.SampleUpload("counted", "upload", 3)
	if err := store.CreateUpload(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPart(ctx, "counted", u.Key, u.ID, porttest.SamplePart(u, 1, 33, 0)); err != nil {
		t.Fatal(err)
	}
	if got := recording.proposed(); len(got) != 5 || slices.ContainsFunc(got, func(schema byte) bool { return schema >= entrySchema5 }) {
		t.Fatalf("the changes were proposed in the schemas %v, want five entries below schema 5", got)
	}
	want := bucket.UsageTotals{Objects: 1, CurrentBytes: 600, Versions: 1, StoredBytes: 600, UploadBytes: 33}
	for _, id := range []string{"m1", "m2", "m3", "s4"} {
		got, err := caughtUp{c.store(id)}.BucketUsages(ctx, []string{"counted"})
		if err != nil || !got[0].Usage.Exact || got[0].Usage.UsageTotals != want {
			t.Errorf("node %s: the bucket holds %+v, %v; want %+v, exact", id, got, err, want)
		}
	}
}

// takenBack stops a node of a cluster and starts it again as the release of
// schema 4, under the name it had. It does not register: until it does, its
// record says what the release it was taken back from registered.
func (c *testCluster) takenBack(id string, voter bool) *Store {
	c.t.Helper()
	c.stop(id)
	c.mu.Lock()
	c.schema4[id] = true
	c.mu.Unlock()
	back := c.start(id, voter)
	c.ready(id)
	if back.supported != entrySchema4 || back.fsm.supported != entrySchema4 {
		c.t.Fatalf("node %s came back applying schema %d", id, back.supported)
	}
	return back
}

// untouched says of every node that it is healthy and that its state is one
// the release of schema 4 opens: of a schema below the fifth, with nothing
// only schema 5 makes.
func (c *testCluster) untouched(when string, ids ...string) {
	c.t.Helper()
	for _, id := range ids {
		c.ready(id)
		s := c.store(id)
		if err := s.Check(c.t.Context()); err != nil {
			c.t.Errorf("%s: node %s is not healthy: %v", when, id, err)
		}
		if schema := stateSchema(c.t, s.fsm); schema >= stateSchema5 {
			c.t.Errorf("%s: node %s: state schema %d, want one below %d", when, id, schema, stateSchema5)
		}
		if raw := rawState(c.t, s.fsm, schema5Buckets()...); len(raw) != 0 {
			c.t.Errorf("%s: node %s holds %v", when, id, raw)
		}
	}
}

// TestARollingRollbackFromLevel5EndsAtTheFirstBucket pins what ADR-0023
// ("Rollback") warns of. Every member has registered that it applies schema
// 5, and nothing of schema 5 was written: every state is still one the
// release of schema 4 opens, and that release, started on it, runs. But the
// way back is not walked a node at a time. A node that is taken back under
// its name has a record that says 5 until it registers again, so the leader
// goes on saying that the cluster is at level 5 - and the first bucket that
// is created in that window is an entry of schema 5. The cluster takes it.
// The node that was taken back cannot apply it and stops there, guessing
// nothing: its state is the one it had, and it says why it stopped. From
// then on the other members' states are of schema 5, and the release of
// schema 4 no longer opens them: the way back is over for the whole cluster.
// The node that stopped loses nothing: started on this release again, it
// applies the entry and goes on.
//
// So the way back from level 5 is with every node stopped first, and then
// all of them started on the release of schema 4 - and this test is what
// happens otherwise.
func TestARollingRollbackFromLevel5EndsAtTheFirstBucket(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := clusterAtLevel5(t, nil)
	nodes := []string{"m1", "m2", "m3", "s4"}
	c.untouched("at level 5, before anything of schema 5", nodes...)

	// One node is taken back; it has not registered again yet.
	back := c.takenBack("s4", false)
	c.untouched("with one node taken back", nodes...)
	before := stateSchema(t, back.fsm)
	var lead *Store
	eventually(t, "a leader that has applied every registration", func() bool {
		lead = c.store(c.leader())
		level, err := lead.entryVersion()
		return err == nil && level == entrySchema5
	})
	recorded, err := lead.ListNodes(ctx)
	if err != nil || !slices.ContainsFunc(recorded, func(n cluster.Node) bool { return n.ID == "s4" && n.MetadataVersion == entrySchema5 }) {
		t.Fatalf("the nodes the leader knows = %+v, %v; want the record of s4 to say schema 5 still", recorded, err)
	}

	// The window: one bucket.
	const name = "in-the-window"
	if err := lead.CreateBucket(ctx, bucket.Bucket{Name: name}); err != nil {
		t.Fatalf("a bucket through the leader, with a node taken back that has not registered: %v", err)
	}
	if schema := stateSchema(t, lead.fsm); schema != stateSchema5 {
		t.Fatalf("the bucket was written below schema 5 (state schema %d): the test proves less than it says", schema)
	}
	eventually(t, "the node that was taken back to stop", func() bool {
		return errors.Is(back.Check(ctx), domain.ErrUnsupportedFormat)
	})
	// What its operator is shown.
	if stopped := back.Check(ctx); !strings.Contains(stopped.Error(), "metadata state machine stopped at entry") ||
		!strings.Contains(stopped.Error(), "metadata entry schema version 5") {
		t.Errorf("the node that stopped says %q", stopped)
	}
	if err := back.CreateBucket(ctx, bucket.Bucket{Name: "after-it-stopped"}); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Errorf("a write on the node that stopped = %v, want %v", err, domain.ErrUnsupportedFormat)
	}
	// It applied nothing of the entry: its state is the one it had.
	if schema := stateSchema(t, back.fsm); schema != before {
		t.Errorf("the node that stopped: state schema %d, want %d still", schema, before)
	}
	if raw := rawState(t, back.fsm, schema5Buckets()...); len(raw) != 0 {
		t.Errorf("the node that stopped holds %v", raw)
	}
	for place := range rawState(t, back.fsm, bucketsBucket()) {
		if strings.Contains(place, name) {
			t.Errorf("the node that stopped holds %s: it applied the entry in some other way", place)
		}
	}
	// The others went on, at schema 5: no release of schema 4 opens them now.
	for _, id := range nodes[:3] {
		c.ready(id)
		if err := c.store(id).Check(ctx); err != nil {
			t.Errorf("node %s is not healthy: %v", id, err)
		}
		if schema := stateSchema(t, c.store(id).fsm); schema != stateSchema5 {
			t.Errorf("node %s: state schema %d after the bucket, want %d", id, schema, stateSchema5)
		}
	}

	// The way out is forward: the node is started on this release again,
	// applies the entry it stopped at and holds what the others hold.
	c.stop("s4")
	c.mu.Lock()
	c.schema4["s4"] = false
	c.mu.Unlock()
	again := c.start("s4", false)
	c.ready("s4")
	if err := again.Check(ctx); err != nil {
		t.Fatalf("the node, started on this release again, is not healthy: %v", err)
	}
	got, err := caughtUp{again}.BucketUsages(ctx, []string{name})
	if err != nil || !got[0].Usage.Exact || stateSchema(t, again.fsm) != stateSchema5 {
		t.Errorf("the node, started on this release again: the bucket's usage %+v, %v, state schema %d; want it counted, in a state of schema 5",
			got, err, stateSchema(t, again.fsm))
	}
	if _, err := again.GetBucket(ctx, name); err != nil {
		t.Errorf("the bucket on the node that had stopped: %v", err)
	}
}

// routedTo is the leader client of a node whose every proposal is handed to
// one node, whoever leads.
type routedTo struct {
	LeaderClient
	node string

	mu      sync.Mutex
	schemas []byte
}

// Propose implements LeaderClient.
func (r *routedTo) Propose(ctx context.Context, _ string, data []byte) ([]byte, error) {
	r.mu.Lock()
	r.schemas = append(r.schemas, data[0])
	r.mu.Unlock()
	return r.LeaderClient.Propose(ctx, r.node, data)
}

// TestANodeTakenBackAtLevel5RefusesAnEntryOfSchema5: the other half of the
// window (ADR-0023, "Rollback"). A node of the release of schema 4 that is
// handed an entry of schema 5 - as the leader is, by a member that believes
// the cluster at level 5, the node's own record saying so - does not log it:
// it cannot read it. What it answers is that of the release of schema 4, and
// is not ours to change: "a format this node does not understand", and not
// "unavailable" - it reads the entry before it asks itself whether it leads
// or what the cluster writes, so the answer is the same wherever it stands.
// The proposer does not take that for a leader behind the cluster, and does
// not write the entry again below: the bucket is not created, by an error
// nobody tries again, and nothing is logged. Every state stays one the
// release of schema 4 opens.
//
// When the node has registered what it applies, the cluster is at level 4
// for every member that has seen so: buckets are created as that release
// creates them, and the way back is open still.
func TestANodeTakenBackAtLevel5RefusesAnEntryOfSchema5(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := clusterAtLevel5(t, nil)
	nodes := []string{"m1", "m2", "m3", "s4"}
	// A voter: one that may come to lead.
	back := c.takenBack("m1", true)
	c.untouched("with a voter taken back", nodes...)

	data, err := encodeEntry(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "refused"}}, entrySchema5)
	if err != nil {
		t.Fatal(err)
	}
	definite := func(err error) bool {
		return errors.Is(err, domain.ErrUnsupportedFormat) && !errors.Is(err, domain.ErrUnavailable) && !errors.Is(err, domain.ErrOutcomeUnknown) &&
			!errors.Is(err, domain.ErrNotLeader)
	}
	if _, err := back.ProposeLocal(ctx, data); !definite(err) || isAboveLevel(err) {
		t.Fatalf("an entry of schema 5 handed to a node of the release of schema 4 = %v, want a definite %v and nothing else", err, domain.ErrUnsupportedFormat)
	}
	if _, _, _, err := back.Subject(data); !definite(err) {
		t.Errorf("the subject of an entry of schema 5, asked of that node = %v, want %v", err, domain.ErrUnsupportedFormat)
	}

	// Through a member of this release, which believes the cluster at level
	// 5 and whose proposals come to that node.
	store := c.store("s4")
	eventually(t, "s4 to see every member on schema 5", func() bool {
		level, err := store.entryVersion()
		return err == nil && level == entrySchema5
	})
	direct := store.leader
	routed := &routedTo{LeaderClient: direct, node: "m1"}
	store.leader = routed
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "refused"}); !definite(err) {
		t.Errorf("a bucket through a member of this release, handed to the node taken back = %v, want a definite %v and nothing else", err, domain.ErrUnsupportedFormat)
	}
	if !bytes.Equal(routed.schemas, []byte{entrySchema5}) {
		t.Errorf("the bucket was proposed in the schemas %v, want 5 and no more: it is not written again below", routed.schemas)
	}
	store.leader = direct
	c.untouched("after the refusal", nodes...)
	for _, id := range nodes {
		if _, err := c.store(id).GetBucket(ctx, "refused"); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("the refused bucket on %s: %v", id, err)
		}
	}

	// The node registers what it applies: level 4, and buckets again.
	c.register("m1")
	for _, id := range nodes {
		eventually(t, "node "+id+" to see the cluster at schema 4", func() bool {
			level, err := c.store(id).entryVersion()
			return err == nil && level == entrySchema4
		})
		if err := c.store(id).CreateBucket(ctx, bucket.Bucket{Name: "back-through-" + id, CreatedBy: "user/albie"}); err != nil {
			t.Errorf("a bucket through %s, once the node taken back has registered: %v", id, err)
		}
	}
	c.untouched("after the node registered schema 4", nodes...)
}
