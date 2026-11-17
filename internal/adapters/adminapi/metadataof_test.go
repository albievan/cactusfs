package adminapi

import (
	"testing"

	"github.com/albievan/cactusfs/internal/domain/object"
)

// TestMetadataOfReplicationStatus: every status a version can carry is
// given as the contract names it - REPLICA among them, which only the S3
// API's replica requests make - and a status the contract does not name,
// as a newer release might store, is left out rather than sent outside the
// enum (PLAN 8.1.10).
func TestMetadataOfReplicationStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		stored object.ReplicationStatus
		want   string // "": absent
	}{
		{stored: ""},
		{stored: object.ReplicationPending, want: "PENDING"},
		{stored: object.ReplicationCompleted, want: "COMPLETED"},
		{stored: object.ReplicationFailed, want: "FAILED"},
		{stored: object.ReplicationReplica, want: "REPLICA"},
		{stored: "QUEUED"},
	} {
		got := metadataOf(object.Version{Bucket: "b", Key: "k", Replication: tc.stored}).ReplicationStatus
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%q: replication_status = %q, want it absent", tc.stored, *got)
		case tc.want != "" && (got == nil || string(*got) != tc.want):
			t.Errorf("%q: replication_status = %v, want %q", tc.stored, got, tc.want)
		}
	}
}
