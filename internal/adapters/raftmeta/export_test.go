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
