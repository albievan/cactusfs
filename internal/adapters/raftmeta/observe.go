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
		AppliedIndex: s.raft.AppliedIndex(), CommitIndex: s.raft.CommitIndex(),
		Supported: int(s.supported),
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
	_ = s.fsm.view(func(tx *bbolt.Tx) error {
		o.StateSchema = readStateSchema(tx)
		return nil
	})
	return o, true
}
