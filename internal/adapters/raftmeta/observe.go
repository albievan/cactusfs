package raftmeta

import (
	"github.com/hashicorp/raft"
	"go.etcd.io/bbolt"
)

// Observation is what a node's metrics show of its part of the metadata
// group. It is read, never written: nothing here is part of the replicated
// state.
type Observation struct {
	// Leader reports whether this node leads the group.
	Leader bool
	// Term is the Raft term the node is in; AppliedIndex the last entry its
	// state reflects; CommitIndex the last entry it knows to be committed.
	Term, AppliedIndex, CommitIndex uint64
	// Voters is the number of voters of the group as this node knows it.
	Voters int
	// Level is the newest entry schema the cluster writes now - the oldest
	// of what its members understand - and zero when the node cannot tell.
	// Supported is the newest this binary applies.
	Level, Supported int
	// StateSchema is the schema the node's state says it has, and zero when
	// it could not be read.
	StateSchema uint64
	// UsageClamped is how many changes this node has applied, since it
	// started, that held a count of a bucket's usage record at one of its
	// bounds - each began a recount (ADR-0023). UsageDrift is how many
	// recounts it has applied that found other totals than were kept. Every
	// member counts for itself, as it applies, and neither is in the
	// replicated state.
	//
	// Neither is a metric, and neither is meant to be: a sum of either over
	// the members would be what happened times the members.
	// cactusfs_bucket_usage_drift_total counts app.UsageCounterConfig.OnDrift,
	// once on the node whose entry ended the recount (PLAN 8.3.3). A clamp is
	// counted by no metric (PLAN 8.3.6, ADR-0023 "Bucket usage"): the recount
	// it begins reports it as drift whenever it left a figure that was wrong.
	UsageClamped, UsageDrift uint64
}

// Observe returns the state of this node's part of the metadata group, for
// its metrics. It asks no other node and changes nothing: the numbers are
// those Raft keeps in memory, the schemas up to three short reads of this
// node's state. It reports false when the store is closed or about to be - a scrape
// never waits for Close.
//
// It can wait, though, and without bound: the reads take the lock of the
// state machine, which an entry holds while it is applied and a snapshot for
// as long as it is being restored. The level is worked out from the members'
// records and Raft's configuration, so that there is no number to keep
// beside the state that would spare the lock. Whoever reads Observe for a
// scrape must therefore not wait for it longer than the scrape may take; the
// telemetry adapter asks through a probe that does not. Check is the same.
func (s *Store) Observe() (Observation, bool) {
	if !s.mu.TryRLock() {
		return Observation{}, false
	}
	defer s.mu.RUnlock()
	if s.closed {
		return Observation{}, false
	}
	o := Observation{
		Leader: s.raft.State() == raft.Leader, Term: s.raft.CurrentTerm(),
		CommitIndex:  s.raft.CommitIndex(),
		Supported:    int(s.supported),
		UsageClamped: s.fsm.usageClamped.Load(), UsageDrift: s.fsm.usageDrift.Load(),
	}
	if future := s.raft.GetConfiguration(); future.Error() == nil {
		for _, server := range future.Configuration().Servers {
			if server.Suffrage == raft.Voter {
				o.Voters++
			}
		}
	}
	if level, err := s.entryVersion(); err == nil {
		o.Level = int(level)
	}
	// The applied index is the state's own, read with its schema: Raft's
	// counter of what it handed the state machine moves a moment after the
	// state does, and an observation must not be behind what a read sees.
	_ = s.fsm.view(func(tx *bbolt.Tx) error {
		o.StateSchema = readStateSchema(tx)
		o.AppliedIndex = readApplied(tx)
		return nil
	})
	return o, true
}
