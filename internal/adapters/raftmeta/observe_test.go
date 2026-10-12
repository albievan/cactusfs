package raftmeta_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/domain/bucket"
)

// TestObserve: a store on its own leads a group of one voter and writes the
// newest schema; its indexes move with what is committed; and a store that
// is closed says nothing - without waiting.
func TestObserve(t *testing.T) {
	t.Parallel()
	store := open(t, t.TempDir())

	before, ok := store.Observe()
	if !ok {
		t.Fatal("an open store could not be observed")
	}
	if !before.Leader || before.Voters != 1 || before.Term == 0 {
		t.Errorf("Observe = %+v, want the leader of a group of one in a term", before)
	}
	if before.Level != raftmeta.MetadataVersion || before.Supported != raftmeta.MetadataVersion {
		t.Errorf("level %d, supported %d, want both %d", before.Level, before.Supported, raftmeta.MetadataVersion)
	}
	if before.StateSchema == 0 {
		t.Error("the schema of the state is not told")
	}
	if schema := raftmeta.StateSchemaForTest(t, store); before.StateSchema != schema {
		t.Errorf("state schema %d, the state says %d", before.StateSchema, schema)
	}

	if err := store.CreateBucket(t.Context(), bucket.Bucket{Name: "bkt"}); err != nil {
		t.Fatal(err)
	}
	after, ok := store.Observe()
	if !ok || after.AppliedIndex <= before.AppliedIndex || after.CommitIndex < after.AppliedIndex {
		t.Errorf("after a change: applied %d (was %d), committed %d", after.AppliedIndex, before.AppliedIndex, after.CommitIndex)
	}
	applied, err := store.AppliedIndex()
	if err != nil || applied != after.AppliedIndex {
		t.Errorf("the state reflects entry %d (%v), Observe says %d", applied, err, after.AppliedIndex)
	}
	// Observing changes nothing.
	if again, _ := store.Observe(); again != after {
		t.Errorf("a second look differs: %+v, then %+v", after, again)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if o, ok := store.Observe(); ok || o != (raftmeta.Observation{}) {
		t.Errorf("a closed store was observed: %+v", o)
	}
}

// observed is a store as the gauges of the telemetry adapter read it: as the
// composition root attaches it.
type observed struct {
	store *raftmeta.Store
}

// RaftState implements telemetry.RaftSource.
func (o observed) RaftState() (telemetry.RaftState, bool) {
	seen, ok := o.store.Observe()
	return telemetry.RaftState{
		Leader: seen.Leader, Term: seen.Term, AppliedIndex: seen.AppliedIndex, CommitIndex: seen.CommitIndex, Voters: seen.Voters,
		SchemaLevel: seen.Level, SchemaSupported: seen.Supported, StateSchema: seen.StateSchema,
	}, ok
}

// Ready implements telemetry.ReadinessSource.
func (o observed) Ready(ctx context.Context) bool {
	return o.store.Check(ctx) == nil
}

// exposed returns the names of the series a node exposes that begin with
// one of the prefixes.
func exposed(t *testing.T, tel *telemetry.Telemetry, prefixes ...string) map[string]bool {
	t.Helper()
	families, err := tel.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	names := map[string]bool{}
	for _, family := range families {
		for _, prefix := range prefixes {
			if strings.HasPrefix(family.GetName(), prefix) && len(family.GetMetric()) > 0 {
				names[family.GetName()] = true
			}
		}
	}
	return names
}

// TestAScrapeIsNotHeldByTheStateMachine: Observe and Check read under the
// lock of the state machine, which a snapshot being restored holds for as
// long as it takes. A node that is scraped meanwhile answers in the time a
// scrape may wait, without its Raft gauges and without its readiness - not
// with zeros - and has them again when the lock is let go.
func TestAScrapeIsNotHeldByTheStateMachine(t *testing.T) {
	t.Parallel()
	store := open(t, t.TempDir())
	const timeout = 100 * time.Millisecond
	tel, err := telemetry.New(t.Context(), telemetry.Config{Version: "test", StateTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	tel.ObserveRaft(observed{store: store})
	tel.ObserveReadiness(observed{store: store})
	want := []string{
		"cactusfs_raft_is_leader", "cactusfs_raft_term", "cactusfs_raft_applied_index", "cactusfs_metadata_schema_level",
		"cactusfs_metadata_state_schema", "cactusfs_ready",
	}
	names := exposed(t, tel, "cactusfs_raft_", "cactusfs_metadata_", "cactusfs_ready")
	for _, name := range want {
		if !names[name] {
			t.Fatalf("%s is not exposed by a store that is open", name)
		}
	}

	release := raftmeta.HoldStateForTest(store)
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	for scrape := range 3 {
		started := time.Now()
		names = exposed(t, tel, "cactusfs_raft_", "cactusfs_metadata_", "cactusfs_ready")
		if waited := time.Since(started); waited > timeout+5*time.Second {
			t.Fatalf("scrape %d took %s while the state machine is held", scrape, waited)
		}
		if len(names) != 0 {
			t.Errorf("scrape %d: %v are exposed though the state could not be read", scrape, names)
		}
		if up := exposed(t, tel, "cactusfs_build_info"); len(up) != 1 {
			t.Errorf("scrape %d: what has nothing to do with the state is not exposed either", scrape)
		}
	}

	release()
	released = true
	deadline := time.Now().Add(10 * time.Second)
	for {
		names = exposed(t, tel, "cactusfs_raft_", "cactusfs_metadata_", "cactusfs_ready")
		missing := ""
		for _, name := range want {
			if !names[name] {
				missing = name
			}
		}
		if missing == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the state machine is free again and %s is still not exposed", missing)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
