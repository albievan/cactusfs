package raftmeta

import (
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// TestTheLargestAuditPageIsGivenInFull: a page of the largest limit is given
// whole, so that the management API, which asks for one record more than its
// largest page, can tell that another follows. The trail is written straight
// into the state, in one transaction: a thousand entries through Raft would
// prove nothing more and take a minute.
func TestTheLargestAuditPageIsGivenInFull(t *testing.T) {
	t.Parallel()
	store := openStore(t, t.TempDir())
	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	err := store.fsm.db.Update(func(tx *bbolt.Tx) error {
		for range ports.MaxAuditPage + 1 {
			record := audit.Record{Time: at, Actor: "user/alice", Action: "s3:GetObject", Resource: "arn:aws:s3:::b/k", Outcome: audit.Denied}
			if _, err := appendAudit(tx, *auditToRecord(record)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	porttest.AuditPageContract(t, store)
}
