package ports

import (
	"context"
	"time"

	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/delivery"
)

// TargetStore is the replicated store of the targets a deployment sends to
// (ADR-0023). Every change is committed through Raft before the call
// returns and carries its audit record in the same entry, as SubBucketStore's
// do; a record without an actor is not appended. Reads are strongly
// consistent. Until every member of the cluster knows entry schema 5 a
// change wraps domain.ErrUnavailable, and there are no targets to read.
//
// A target as the store returns it carries its sealed secrets and the sealed
// path of its endpoint: the node that leads opens them when it connects. The
// store never holds a path in the clear - delivery.Target.Endpoint is the
// origin, and the use case seals the path before it proposes the target.
// Whoever shows a target shows delivery.Target.Shown.
type TargetStore interface {
	// PutTarget defines a target, or replaces one of that name whole. Of
	// what it is given the store keeps everything but three things that
	// are its own: Refs, which it counts; CreatedAt, which a target that
	// exists keeps; and the cursor of the target in the outbox, which is
	// made with a new target, at the entry that defines it, and stays
	// with one that is replaced.
	//
	// The error wraps domain.ErrInvalidArgument for a target that
	// delivery.Target.Validate refuses, and for a new target where there
	// are delivery.MaxTargets already; and domain.ErrEntityInUse for a
	// target that bucket configurations name when the change is of its
	// kind - what was matched for a webhook is not to be handed to
	// another deployment - or leaves it admitting no longer a bucket
	// whose configuration names it. A list of buckets that is widened,
	// and one that is narrowed once the configurations that needed it are
	// gone, is taken.
	PutTarget(ctx context.Context, t delivery.Target, record audit.Record) error
	// DeleteTarget removes a target and its cursor. The error wraps
	// domain.ErrEntityNotFound, and domain.ErrEntityInUse while a bucket
	// configuration names the target.
	DeleteTarget(ctx context.Context, name string, record audit.Record) error
	// GetTarget returns a target. The error wraps
	// domain.ErrEntityNotFound.
	GetTarget(ctx context.Context, name string) (delivery.Target, error)
	// ListTargets returns every target in name order.
	ListTargets(ctx context.Context) ([]delivery.Target, error)
}

// The states of delivery to a target, as a status tells them. A newer
// release may add to them: a state that is not known is shown as it is.
const (
	// DeliveryOK is a target whose last attempt succeeded, or for which
	// nothing waits.
	DeliveryOK = "ok"
	// DeliveryRetrying is a target after a failure that is tried again.
	DeliveryRetrying = "retrying"
	// DeliveryStalled is a target that refuses everything: somebody has
	// to act.
	DeliveryStalled = "stalled"
	// DeliveryUnsupported is what is shown of a target when the node that
	// leads is of a release that cannot say.
	DeliveryUnsupported = "unsupported"
)

// TargetStatus is how delivery to one target stands, as the node that leads
// the metadata group has it in memory since it came to lead, with the counts
// the metadata keeps. Neither an endpoint nor the words of an error are
// part of it.
type TargetStatus struct {
	Name string
	Kind delivery.Kind
	// State is one of the Delivery... constants, or a word of a newer
	// release.
	State string
	// Pending counts the rows of the outbox matched for the target and
	// not yet acknowledged.
	Pending uint64
	// OldestPendingAt is when the oldest of them was made; zero when
	// nothing waits.
	OldestPendingAt time.Time
	// LastSuccessAt is when the leader last delivered something; zero
	// when it has not since it came to lead.
	LastSuccessAt time.Time
	// LastErrorClass is the class of the last failure, or
	// delivery.ClassNone when the last attempt succeeded.
	LastErrorClass delivery.Class
	// Lost counts the rows that were dropped before they were sent, as
	// the metadata counts them (delivery.Cursor.Lost).
	Lost uint64
	// Rejected counts the messages the target refused one by one, which
	// are not sent again, since the leader came to lead.
	Rejected uint64
	// Failed counts the versions that cannot be replicated and were
	// given up on, since the leader came to lead.
	Failed uint64
	// ResyncNeeded says that rows for replication were lost.
	ResyncNeeded bool
}

// DeliveryStatusReader asks the node that leads the metadata group how
// delivery to targets stands. Only that node delivers, so only it can say;
// nothing is kept of the answer.
type DeliveryStatusReader interface {
	// DeliveryStatus returns the status of the targets named, in name
	// order - of every target when none is named. A name that is no
	// target's is left out of the answer.
	//
	// An error says why there is no answer, and wraps
	//
	//   - domain.ErrNotSupported when the node that leads is of a release
	//     that cannot be asked;
	//   - context.DeadlineExceeded when it was reached and did not answer
	//     in the time ctx gives;
	//   - domain.ErrUnavailable when there is no leader, or it could not
	//     be reached;
	//
	// and none of these when it was reached and could not answer. The
	// call returns when ctx ends, whatever the leader does.
	DeliveryStatus(ctx context.Context, targets []string) ([]TargetStatus, error)
}
