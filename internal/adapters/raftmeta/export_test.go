package raftmeta

import (
	"encoding/binary"
	"testing"

	"go.etcd.io/bbolt"
)

// StateSchemaForTest returns the schema the state database of a store says
// it has.
func StateSchemaForTest(t *testing.T, s *Store) uint64 {
	t.Helper()
	var schema uint64
	if err := s.fsm.view(func(tx *bbolt.Tx) error {
		schema = binary.BigEndian.Uint64(tx.Bucket(metaBucket()).Get(schemaKey()))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return schema
}

// HoldStateForTest takes the lock of the state machine as an entry that is
// being applied or a snapshot that is being restored holds it, and returns
// the function that lets it go.
func HoldStateForTest(s *Store) (release func()) {
	s.fsm.mu.Lock()
	return s.fsm.mu.Unlock
}

// PolicyPageBuckets is the most buckets one page of
// LocalConfig.BucketPolicies reads.
const PolicyPageBuckets = policyPageBuckets

// SetOutboxBoundForTest makes the outbox of a store hold so many rows in
// place of delivery.MaxOutboxRows. The bound is part of the replicated
// format: every member of a cluster under test is given the same.
func SetOutboxBoundForTest(s *Store, rows uint64) {
	setOutboxBound(s.fsm, rows)
}

// SetOutboxScanForTest makes one call of ReadOutbox on a store look at so
// many rows in place of outboxScan. It is no part of the replicated format:
// it bounds a read of this node, and is set on the node that is asked.
func SetOutboxScanForTest(s *Store, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanRows = rows
}

func setOutboxBound(f *fsm, rows uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outboxRows = rows
}
