package raftmeta

import (
	"bytes"
	"context"
	"fmt"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// What entry schema 5 added to the store (ADR-0023 to ADR-0026): the usage of
// buckets and their quotas, targets, the outbox, replica buckets and the
// record of a resync. The state machine of each is in bucketusage.go,
// targets.go, outbox.go and replica.go; here are the calls that propose
// their entries and read what they left.
//
// Every change is one of the seven commands of schema 5, or - a notification
// or replication document, a replica put - an old command with what only
// schema 5 says (bucketcfg.go, versions.go, uploads.go). None has a form
// below schema 5, so while a member of the cluster has not registered that
// it applies schema 5 each is answered with domain.ErrUnavailable by
// Store.propose, which logs nothing: the state stays one the release of
// schema 4 opens.
//
// The first entry of schema 5 a state applies ends the way back to that
// release, whether the entry took effect or not (ADR-0023, "Rollback"). So
// what the state machine would refuse without looking at the state - a limit,
// a quota, a name, a target that cannot be, a result that names nothing - is
// refused here, before anything is proposed, as the store does for schema 3
// and schema 4.

var (
	_ ports.BucketUsageStore = (*Store)(nil)
	_ ports.TargetStore      = (*Store)(nil)
	_ ports.Outbox           = (*Store)(nil)
	_ ports.ReplicationStore = (*Store)(nil)
)

// outboxScan is how many rows one call of ReadOutbox looks at, whoever they
// are for: a target whose rows are few among many of others is not made to
// hold a read transaction open over the whole outbox. A call that has looked
// at so many returns what it found and how far it came.
const outboxScan = 4 * delivery.MaxOutboxRead

// knownBucket reports whether a name can be that of a bucket the state
// holds: one of a length the store lets a bucket be created with
// (Store.CreateBucket).
func knownBucket(name string) bool {
	return name != "" && len(name) <= bucket.MaxNameLength
}

// BucketUsages implements ports.BucketUsageStore. The records are read in
// one transaction, each by a point read, from this node's own copy of the
// state: the leader is not asked whether the copy is current.
func (s *Store) BucketUsages(ctx context.Context, names []string) ([]ports.BucketUsage, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("raftmeta.BucketUsages: %w", err)
	}
	if len(names) > ports.MaxBucketUsages {
		return nil, fmt.Errorf("raftmeta.BucketUsages: %w: %d buckets, at most %d", domain.ErrInvalidArgument, len(names), ports.MaxBucketUsages)
	}
	out := make([]ports.BucketUsage, len(names))
	err := s.peek(func(tx *bbolt.Tx) error {
		for i, name := range names {
			out[i] = ports.BucketUsage{}
			if !knownBucket(name) {
				continue
			}
			b, err := readBucket(tx, name)
			if err != nil {
				return err
			}
			if b == nil {
				// A record goes with its bucket (dropUsage): there is none.
				continue
			}
			usage, _, err := readUsage(tx, name)
			if err != nil {
				return err
			}
			out[i] = ports.BucketUsage{Usage: usage, QuotaBytes: b.QuotaBytes}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.BucketUsages: %w", err)
	}
	return out, nil
}

// WalkBucketUsage implements ports.BucketUsageStore. Every call is one
// entry, whatever the record says: whether there is something to count is
// decided where the entry is applied, from the record as it is then, and the
// answer is the record that entry left. A call for a record that is exact
// and not being counted therefore costs an entry that changes nothing; who
// counts reads BucketUsages first and asks for what is not exact.
func (s *Store) WalkBucketUsage(ctx context.Context, name string, limit int, restart bool) (ports.UsageWalk, error) {
	if limit < 1 || limit > ports.MaxUsageWalk {
		return ports.UsageWalk{}, fmt.Errorf("raftmeta.WalkBucketUsage: %w: a walk of %d records, from 1 to %d", domain.ErrInvalidArgument, limit, ports.MaxUsageWalk)
	}
	if !knownBucket(name) {
		return ports.UsageWalk{}, fmt.Errorf("raftmeta.WalkBucketUsage: %w: %s", domain.ErrBucketNotFound, name)
	}
	e := entry{Type: cmdWalkBucketUsage, WalkBucketUsage: &usageWalkCmd{Bucket: name, Limit: limit, Restart: restart}}
	result, err := s.propose(ctx, e)
	if err != nil {
		return ports.UsageWalk{}, fmt.Errorf("raftmeta.WalkBucketUsage: %w", err)
	}
	// From here on the entry is committed. An answer that cannot be read is
	// not a failure of the walk: the record says where it is.
	usage, err := bucket.ParseUsage(result.usage)
	if err != nil {
		return ports.UsageWalk{}, fmt.Errorf("raftmeta.WalkBucketUsage: %w: the record the entry left: %w", domain.ErrOutcomeUnknown, err)
	}
	return ports.UsageWalk{Usage: usage, Drift: result.drift}, nil
}

// SetBucketQuota implements ports.BucketUsageStore.
func (s *Store) SetBucketQuota(ctx context.Context, name string, quotaBytes int64, record audit.Record) error {
	if quotaBytes != 0 {
		if err := bucket.ValidateQuota(quotaBytes); err != nil {
			return fmt.Errorf("raftmeta.SetBucketQuota: %w", err)
		}
	}
	if !knownBucket(name) {
		return fmt.Errorf("raftmeta.SetBucketQuota: %w: %s", domain.ErrBucketNotFound, name)
	}
	trail, err := auditOf(record)
	if err != nil {
		return fmt.Errorf("raftmeta.SetBucketQuota: %w", err)
	}
	e := entry{Type: cmdSetBucketQuota, SetBucketQuota: &bucketQuotaCmd{Bucket: name, Bytes: quotaBytes, Audit: trail}}
	if _, err := s.propose(ctx, e); err != nil {
		return fmt.Errorf("raftmeta.SetBucketQuota: %w", err)
	}
	return nil
}

// PutTarget implements ports.TargetStore.
func (s *Store) PutTarget(ctx context.Context, t delivery.Target, record audit.Record) error {
	// The count of the documents that name it is the state's, and is not
	// taken from the caller: it is judged without.
	t.Refs = 0
	if err := t.Validate(); err != nil {
		return fmt.Errorf("raftmeta.PutTarget: %w", err)
	}
	trail, err := auditOf(record)
	if err != nil {
		return fmt.Errorf("raftmeta.PutTarget: %w", err)
	}
	e := entry{Type: cmdPutTarget, PutTarget: &targetCmd{Target: targetToRecord(t), Audit: trail}}
	if _, err := s.propose(ctx, e); err != nil {
		return fmt.Errorf("raftmeta.PutTarget: %w", err)
	}
	return nil
}

// DeleteTarget implements ports.TargetStore.
func (s *Store) DeleteTarget(ctx context.Context, name string, record audit.Record) error {
	if err := delivery.ValidateTargetName(name); err != nil {
		// A name no target can have is that of no target.
		return fmt.Errorf("raftmeta.DeleteTarget: %w: target %q", domain.ErrEntityNotFound, name)
	}
	trail, err := auditOf(record)
	if err != nil {
		return fmt.Errorf("raftmeta.DeleteTarget: %w", err)
	}
	e := entry{Type: cmdDeleteTarget, DeleteTarget: &targetRefCmd{Name: name, Audit: trail}}
	if _, err := s.propose(ctx, e); err != nil {
		return fmt.Errorf("raftmeta.DeleteTarget: %w", err)
	}
	return nil
}

// GetTarget implements ports.TargetStore.
func (s *Store) GetTarget(ctx context.Context, name string) (delivery.Target, error) {
	var found *targetRecord
	err := s.view(ctx, func(tx *bbolt.Tx) (err error) {
		found = nil
		if name == "" {
			return nil
		}
		found, err = readTarget(tx, name)
		return err
	})
	if err == nil && found == nil {
		err = fmt.Errorf("%w: target %s", domain.ErrEntityNotFound, name)
	}
	if err != nil {
		return delivery.Target{}, fmt.Errorf("raftmeta.GetTarget: %w", err)
	}
	return found.toDomain(), nil
}

// ListTargets implements ports.TargetStore.
func (s *Store) ListTargets(ctx context.Context) ([]delivery.Target, error) {
	var out []delivery.Target
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		out = nil
		targets := tx.Bucket(targetsBucket())
		if targets == nil {
			return nil
		}
		return targets.ForEach(func(name, raw []byte) error {
			record, err := decodeTarget(string(name), raw)
			if err != nil {
				return err
			}
			out = append(out, record.toDomain())
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.ListTargets: %w", err)
	}
	return out, nil
}

// ReadOutbox implements ports.Outbox. One call looks at no more than
// outboxScan rows, whoever they are for, and says how far it looked: a call
// that found nothing for the target and came further than after is repeated
// from there. A limit that cannot be is refused before the leader is asked
// whether this node's copy of the state is current.
func (s *Store) ReadOutbox(ctx context.Context, target string, after delivery.Seq, limit int) ([]delivery.Item, delivery.Seq, error) {
	if limit < 1 || limit > delivery.MaxOutboxRead {
		return nil, delivery.Seq{}, fmt.Errorf("raftmeta.ReadOutbox: %w: a read of %d rows, from 1 to %d", domain.ErrInvalidArgument, limit, delivery.MaxOutboxRead)
	}
	var items []delivery.Item
	upto := after
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		items, upto = nil, after
		var cursor delivery.Cursor
		defined := false
		if target != "" {
			var err error
			if cursor, defined, err = readCursor(tx, target); err != nil {
				return err
			}
		}
		if !defined {
			return fmt.Errorf("%w: target %s", domain.ErrEntityNotFound, target)
		}
		rows := tx.Bucket(outboxBucket())
		if rows == nil {
			return nil
		}
		// What was written before the target was defined is not the
		// target's, whatever name it was written for: the read begins
		// behind both places.
		from := after
		if cursor.From.After(from) {
			from = cursor.From
		}
		at := from.Key()
		c := rows.Cursor()
		key, raw := c.Seek(at[:])
		if key != nil && bytes.Equal(key, at[:]) {
			key, raw = c.Next()
		}
		for looked := 0; key != nil && len(items) < limit && looked < s.scanRows; key, raw = c.Next() {
			item, err := decodeItem(key, raw)
			if err != nil {
				return err
			}
			looked++
			upto = item.Seq
			if _, mine := item.For(target); mine {
				items = append(items, item)
			}
		}
		return nil
	})
	if err != nil {
		return nil, delivery.Seq{}, fmt.Errorf("raftmeta.ReadOutbox: %w", err)
	}
	return items, upto, nil
}

// AckOutbox implements ports.Outbox.
func (s *Store) AckOutbox(ctx context.Context, target string, upto delivery.Seq, auditUpto uint64, results []delivery.Result) (int, error) {
	if err := delivery.ValidateTargetName(target); err != nil {
		// A name no target can have is that of no target.
		return 0, fmt.Errorf("raftmeta.AckOutbox: %w: target %q", domain.ErrEntityNotFound, target)
	}
	if len(results) > delivery.MaxAckResults {
		return 0, fmt.Errorf("raftmeta.AckOutbox: %w: %d results, at most %d", domain.ErrInvalidArgument, len(results), delivery.MaxAckResults)
	}
	cmd := &ackOutboxCmd{Target: target, Upto: upto.String(), AuditUpto: auditUpto}
	for _, result := range results {
		if err := result.Validate(); err != nil {
			return 0, fmt.Errorf("raftmeta.AckOutbox: %w", err)
		}
		cmd.Results = append(cmd.Results, resultToRecord(result))
	}
	result, err := s.propose(ctx, entry{Type: cmdAckOutbox, AckOutbox: cmd})
	if err != nil {
		return 0, fmt.Errorf("raftmeta.AckOutbox: %w", err)
	}
	return result.unrelated, nil
}

// OutboxCursors implements ports.Outbox.
func (s *Store) OutboxCursors(ctx context.Context) (ports.OutboxState, error) {
	var state ports.OutboxState
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		state = ports.OutboxState{
			Cursors: []delivery.Cursor{},
			Rows:    readCounter(tx, outboxCountKey()), Dropped: readCounter(tx, outboxDroppedKey()),
		}
		cursors := tx.Bucket(outboxCursorBucket())
		if cursors == nil {
			return nil
		}
		return cursors.ForEach(func(name, raw []byte) error {
			cursor, err := decodeCursor(string(name), raw)
			if err != nil {
				return err
			}
			state.Cursors = append(state.Cursors, cursor)
			return nil
		})
	})
	if err != nil {
		return ports.OutboxState{}, fmt.Errorf("raftmeta.OutboxCursors: %w", err)
	}
	return state, nil
}

// SetBucketReplica implements ports.ReplicationStore.
func (s *Store) SetBucketReplica(ctx context.Context, name, source string, record audit.Record) error {
	if source != "" {
		if err := object.ValidateReplicaSource(source); err != nil {
			return fmt.Errorf("raftmeta.SetBucketReplica: %w", err)
		}
	}
	if !knownBucket(name) {
		return fmt.Errorf("raftmeta.SetBucketReplica: %w: %s", domain.ErrBucketNotFound, name)
	}
	trail, err := auditOf(record)
	if err != nil {
		return fmt.Errorf("raftmeta.SetBucketReplica: %w", err)
	}
	e := entry{Type: cmdSetBucketReplica, SetBucketReplica: &bucketReplicaCmd{Bucket: name, Source: source, Audit: trail}}
	if _, err := s.propose(ctx, e); err != nil {
		return fmt.Errorf("raftmeta.SetBucketReplica: %w", err)
	}
	return nil
}

// PutResync implements ports.ReplicationStore.
func (s *Store) PutResync(ctx context.Context, r bucket.Resync) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("raftmeta.PutResync: %w", err)
	}
	if _, err := s.propose(ctx, entry{Type: cmdPutResync, PutResync: resyncToRecord(r)}); err != nil {
		return fmt.Errorf("raftmeta.PutResync: %w", err)
	}
	return nil
}

// GetResync implements ports.ReplicationStore.
func (s *Store) GetResync(ctx context.Context, bucketName string) (bucket.Resync, error) {
	var record bucket.Resync
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		record = bucket.Resync{}
		var b *bucketRecord
		if knownBucket(bucketName) {
			var err error
			if b, err = readBucket(tx, bucketName); err != nil {
				return err
			}
		}
		if b == nil {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
		}
		var err error
		record, _, err = readResync(tx, bucketName)
		return err
	})
	if err != nil {
		return bucket.Resync{}, fmt.Errorf("raftmeta.GetResync: %w", err)
	}
	return record, nil
}
