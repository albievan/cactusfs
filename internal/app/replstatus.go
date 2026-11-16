package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// The status of a bucket's replication (ADR-0026, "Status"; PLAN 8.1.7):
// what GET /buckets/{bucket}/replication/status answers on any node.

// ReplicationStatus is how far the replication of a bucket has come. Each
// part is read where it is true on whichever node answers:
//
//   - Pending and Failed are the bucket's, from its usage record in the
//     metadata, read after the consistent reads of the configuration, of
//     the resync and of the target's cursor, so never older than they are;
//   - Lost and ResyncNeeded are of the bucket's target, from its cursor in
//     the metadata (delivery.Cursor), a consistent read;
//   - TargetState, LastErrorClass and OldestPendingAt are of the bucket's
//     target, as the node that leads - which alone sends - says it; a node
//     that does not lead asks it. A resync held by a failure to the target
//     shows there too (DeliveryWorkerConfig.Resyncs);
//   - Resync is the record of the bucket's resync in the metadata, the same
//     on every node.
type ReplicationStatus struct {
	// Target is the target the bucket's replication configuration names.
	Target string
	// TargetState is ports.DeliveryOK, ports.DeliveryRetrying,
	// ports.DeliveryStalled, or ports.DeliveryUnsupported when the node
	// that leads is of a release that cannot be asked.
	TargetState string
	// LastErrorClass is the class of the target's last failure, or
	// delivery.ClassNone.
	LastErrorClass delivery.Class
	// OldestPendingAt is when the oldest row that waits for the target was
	// made, of any bucket sent there; zero when none waits.
	OldestPendingAt time.Time
	// Pending and Failed count the bucket's versions whose replication
	// status is PENDING and FAILED; 0 while its usage is being counted for
	// the first time.
	Pending, Failed int64
	// Lost counts the rows for the target dropped at the bound of the
	// outbox; ResyncNeeded says that some were and no resync has been
	// through since. Both are the cursor's, in the metadata.
	Lost         uint64
	ResyncNeeded bool
	// Resync is the record of the bucket's resync; nil when none was ever
	// asked for.
	Resync *bucket.Resync
}

// Status returns how far the replication of a bucket has come. It needs
// admin:GetReplicationStatus on the bucket - which cluster-admin and auditor
// hold, and a bucket's own administrator does not - and reads, in this
// order: the bucket's replication configuration, the record of its resync
// and the cursors of the targets, each a consistent read of the metadata;
// the bucket's usage record,
// from this node's copy, which the reads before brought up to date; and how
// delivery to the target stands, from the node that leads, asked within
// TargetStatusTimeout.
//
// The resync is the stored record and nothing else: what the node that
// leads keeps of a walk in memory (ResyncWorker.Progress) differs from one
// leader to the next, and a node that does not lead cannot read it; the
// contract says the resync is the same whichever node answers. A walk held
// by a failure shows in the target's state and class; its restarts and the
// sends the outbox ended are in the metrics of replication.
//
// The error wraps domain.ErrAccessDenied; domain.ErrBucketNotFound;
// domain.ErrEntityNotFound for a bucket without a replication configuration;
// domain.ErrIntegrity for a configuration that cannot be read; and
// domain.ErrUnavailable for one of a later release, on a node that cannot
// read the usage or the cursors or ask the leader, when there is no leader to ask or it
// did not answer in time, and when the leader does not know the target yet.
func (r *ReplicationService) Status(ctx context.Context, principal iam.Principal, bucketName string) (ReplicationStatus, error) {
	if err := r.authz.Authorize(ctx, bucketRequest(principal, iam.ActionGetReplicationStatus, bucketName)); err != nil {
		return ReplicationStatus{}, fmt.Errorf("replication.Status: %w", err)
	}
	status, err := r.status(ctx, bucketName)
	if err != nil {
		return ReplicationStatus{}, fmt.Errorf("replication.Status: %w", err)
	}
	return status, nil
}

// status reads the parts of a bucket's status.
func (r *ReplicationService) status(ctx context.Context, bucketName string) (ReplicationStatus, error) {
	if r.usage == nil || r.delivery == nil || r.outbox == nil {
		return ReplicationStatus{}, fmt.Errorf("%w: this node cannot say how replication stands", domain.ErrUnavailable)
	}
	config, err := r.storedConfiguration(ctx, bucketName)
	if err != nil {
		return ReplicationStatus{}, err
	}
	if config.Empty() {
		return ReplicationStatus{}, fmt.Errorf("%w: bucket %s has no replication configuration", domain.ErrEntityNotFound, bucketName)
	}
	out := ReplicationStatus{Target: config.Target}
	record, err := r.store.GetResync(ctx, bucketName)
	if err != nil {
		return ReplicationStatus{}, err
	}
	if record.Bucket != "" {
		out.Resync = &record
	}
	state, err := r.outbox.OutboxCursors(ctx)
	if err != nil {
		return ReplicationStatus{}, err
	}
	for _, cursor := range state.Cursors {
		if cursor.Target == config.Target {
			out.Lost, out.ResyncNeeded = cursor.Lost, cursor.ResyncNeeded
		}
	}
	usages, err := r.usage.BucketUsages(ctx, []string{bucketName})
	if err != nil {
		return ReplicationStatus{}, err
	}
	if len(usages) == 1 && usages[0].Usage.Exact {
		out.Pending, out.Failed = usages[0].Usage.ReplicationPending, usages[0].Usage.ReplicationFailed
	}
	target, err := r.targetStatus(ctx, config.Target)
	if err != nil {
		return ReplicationStatus{}, err
	}
	out.TargetState, out.LastErrorClass, out.OldestPendingAt = target.State, target.LastErrorClass, target.OldestPendingAt
	return out, nil
}

// storedConfiguration reads the replication configuration of a bucket
// without asking anyone: none is the empty configuration.
func (r *ReplicationService) storedConfiguration(ctx context.Context, bucketName string) (bucket.ReplicationConfig, error) {
	raw, err := r.buckets.GetBucketConfig(ctx, bucketName, bucket.ConfigReplication)
	if err != nil {
		return bucket.ReplicationConfig{}, err
	}
	config, err := bucket.ParseReplicationConfig(raw)
	switch {
	case errors.Is(err, domain.ErrUnsupportedFormat):
		return bucket.ReplicationConfig{}, fmt.Errorf("%w: the replication configuration of bucket %s is of a later release: %s",
			domain.ErrUnavailable, bucketName, err.Error())
	case err != nil:
		return bucket.ReplicationConfig{}, fmt.Errorf("%w: stored replication configuration of bucket %s: %s",
			domain.ErrIntegrity, bucketName, err.Error())
	}
	return config, nil
}

// targetStatus asks the node that leads how delivery to a target stands, as
// TargetService.Status does, and shows only words that are a state or a
// class (shownStatus).
func (r *ReplicationService) targetStatus(ctx context.Context, name string) (ports.TargetStatus, error) {
	askCtx, cancel := context.WithTimeout(ctx, TargetStatusTimeout)
	defer cancel()
	statuses, err := r.delivery.DeliveryStatus(askCtx, []string{name})
	switch {
	case errors.Is(err, domain.ErrNotSupported):
		return ports.TargetStatus{Name: name, State: ports.DeliveryUnsupported}, nil
	case err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		return ports.TargetStatus{}, fmt.Errorf("%w: the node that leads did not answer in time: %w", domain.ErrUnavailable, err)
	case err != nil:
		return ports.TargetStatus{}, err
	}
	for _, status := range statuses {
		if status.Name == name {
			return shownStatus(status), nil
		}
	}
	// The configuration names the target, so it is there (delivery.Target.
	// Refs); the node that leads has not read it yet.
	return ports.TargetStatus{}, fmt.Errorf("%w: the node that leads does not know target %s yet", domain.ErrUnavailable, name)
}
