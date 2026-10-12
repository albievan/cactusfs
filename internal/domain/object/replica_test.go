package object_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

func TestReplicationStatus(t *testing.T) {
	t.Parallel()
	for status, valid := range map[object.ReplicationStatus]bool{
		"": true, object.ReplicationPending: true, object.ReplicationCompleted: true, object.ReplicationFailed: true, object.ReplicationReplica: true,
		"pending": false, "COMPLETE": false, "REPLICA ": false, "DONE": false,
	} {
		if status.Valid() != valid {
			t.Errorf("%q valid = %v, want %v", status, status.Valid(), valid)
		}
	}
	// The words are those of x-amz-replication-status.
	if object.ReplicationPending != "PENDING" || object.ReplicationCompleted != "COMPLETED" || object.ReplicationFailed != "FAILED" || object.ReplicationReplica != "REPLICA" {
		t.Error("a replication status is not written as S3 writes it")
	}
}

func TestReplica(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		replica object.Replica
		valid   bool
	}{
		{"a source and a sequence", object.Replica{From: "4f0c2a9e-7b1d-4c58-9a36-0d2f5e8b7c11", Seq: 300}, true},
		{"the null version of the source", object.Replica{From: "site-a", Seq: 1, Null: true}, true},
		{"a source of one letter", object.Replica{From: "A", Seq: 1}, true},
		{"a source as long as one can be", object.Replica{From: strings.Repeat("a", object.MaxDeploymentIDLength), Seq: 1}, true},
		{"the greatest sequence there is", object.Replica{From: "site-a", Seq: object.MaxReplicaSeq}, true},
		{"one above the greatest sequence", object.Replica{From: "site-a", Seq: object.MaxReplicaSeq + 1}, false},
		{"the last number there is", object.Replica{From: "site-a", Seq: math.MaxUint64}, false},
		{"the null version above the greatest sequence", object.Replica{From: "site-a", Seq: 1 << 63, Null: true}, false},
		{"no source", object.Replica{Seq: 300}, false},
		{"no sequence", object.Replica{From: "site-a"}, false},
		{"a source that is too long", object.Replica{From: strings.Repeat("a", object.MaxDeploymentIDLength+1), Seq: 1}, false},
		{"a source with a space", object.Replica{From: "site a", Seq: 1}, false},
		{"a source with a line break", object.Replica{From: "site-a\n", Seq: 1}, false},
		{"a source with a slash", object.Replica{From: "site/a", Seq: 1}, false},
		{"a source that is not ASCII", object.Replica{From: "sité", Seq: 1}, false},
	}
	for _, tt := range tests {
		err := tt.replica.Validate()
		if (err == nil) != tt.valid || (err != nil && !errors.Is(err, domain.ErrInvalidArgument)) {
			t.Errorf("%s: Validate = %v, valid wanted: %v", tt.name, err, tt.valid)
		}
		if source := object.ValidateReplicaSource(tt.replica.From); tt.valid && source != nil {
			t.Errorf("%s: ValidateReplicaSource = %v", tt.name, source)
		}
	}
}

// TestMaxReplicaSeq pins the bound: it is in the replicated rule, and a
// change of it is a change of what every member refuses.
func TestMaxReplicaSeq(t *testing.T) {
	t.Parallel()
	if uint64(object.MaxReplicaSeq) != 4611686018427387904 {
		t.Errorf("MaxReplicaSeq = %d, want 2^62", uint64(object.MaxReplicaSeq))
	}
}
