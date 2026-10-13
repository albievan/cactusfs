package ports

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/delivery"
)

// OutboxState is where delivery stands as the metadata has it.
type OutboxState struct {
	// Cursors are the cursors of all targets, in name order.
	Cursors []delivery.Cursor
	// Rows is how many rows the outbox holds: at most
	// delivery.MaxOutboxRows.
	Rows uint64
	// Dropped counts the rows that were pushed out at that bound, since
	// the deployment began.
	Dropped uint64
}

// Outbox is the replicated log of what is still to be delivered to targets,
// and how far each target has come in it (ADR-0023). The store appends the
// rows by itself, when a change is committed that a rule of the bucket's
// notification or replication document matches (ObjectIndex, UploadStore):
// nothing appends through this port. One row is appended for a change, for
// all the consumers it has; a bucket that is deleted leaves its rows to
// their targets.
//
// Delivery is at least once: a row is acknowledged after its target took
// it, so what is sent twice is what one node had sent and not yet
// acknowledged when another came to lead.
type Outbox interface {
	// ReadOutbox returns the rows after the place after that are for the
	// target named, in ascending order, at most limit of them - and upto,
	// the place of the last row it looked at, which is that of the last
	// item or beyond it: rows that are for other targets are passed over,
	// and a target that acknowledges upto has them behind it. With
	// nothing to look at, upto is after.
	//
	// Rows at or before the place the target's cursor began at are not
	// the target's and are never returned: a target defined under a name
	// that was used before is sent nothing of before. How many rows one
	// call looks at is bounded by the implementation; a call that returns
	// no items and an upto beyond after is to be repeated from upto.
	//
	// limit is from 1 to delivery.MaxOutboxRead: domain.ErrInvalidArgument
	// otherwise. The error wraps domain.ErrEntityNotFound when there is
	// no such target.
	ReadOutbox(ctx context.Context, target string, after delivery.Seq, limit int) (items []delivery.Item, upto delivery.Seq, err error)
	// AckOutbox says that the target named has been delivered everything
	// up to upto, or was passed it, and of the audit trail everything up
	// to the sequence auditUpto (ADR-0025); and what became of versions
	// that were to be replicated (ADR-0026).
	//
	//   - A cursor moves forward only: an upto, or an auditUpto, behind
	//     the cursor changes nothing and is no error - it is the
	//     acknowledgement of a node that led before. An upto beyond the
	//     last row there is counts as that row: no acknowledgement passes
	//     what was not written yet. auditUpto moves the cursor of a target
	//     that is sent the audit trail, and of no other.
	//   - A result sets the replication status of the version it names -
	//     by the row that told of it, when that row is still there and
	//     was for the target as replication, or by itself, when its
	//     bucket is there and the replication configuration of that
	//     bucket names the target - when the version still exists and is
	//     no replica. A result that names what is gone changes nothing
	//     and is no error. Results are for a target that is another
	//     deployment: domain.ErrInvalidArgument for any other.
	//   - A result that is well formed and is not the target's to give -
	//     a row that was for another target or was no replication, a
	//     version of a bucket that is not sent to the target - changes
	//     nothing and does not refuse the acknowledgement: the cursor
	//     moves and the other results are taken. unrelated counts them,
	//     for the caller to log; a row that is gone is not counted.
	//   - The store then removes rows every target has passed, at most
	//     delivery.MaxOutboxTrim of them for one acknowledgement.
	//
	// More than delivery.MaxAckResults results, and a result that
	// delivery.Result.Validate refuses, refuse the acknowledgement whole
	// and wrap domain.ErrInvalidArgument. The error wraps
	// domain.ErrEntityNotFound when there is no such target.
	AckOutbox(ctx context.Context, target string, upto delivery.Seq, auditUpto uint64, results []delivery.Result) (unrelated int, err error)
	// OutboxCursors returns the cursor of every target, and the counts of
	// the outbox.
	OutboxCursors(ctx context.Context) (OutboxState, error)
}
