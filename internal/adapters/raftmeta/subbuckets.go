package raftmeta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// Sub-buckets in the state (ADR-0017).
//
//	subbuckets/<bucket>   sub-bucket name -> subBucketRecord
//
// A sub-bucket is a name registered with its bucket, and a policy. Its
// objects are keys of the bucket like any other: nothing in the object index
// knows of it. The bucket exists in the state only once a sub-bucket was
// registered (schema 3).

const (
	cmdPutSubBucket    = "subbucket.put"
	cmdDeleteSubBucket = "subbucket.delete"
)

func subBucketsBucket() []byte { return []byte("subbuckets") }

// subBucketRecord is a sub-bucket as it is stored and replicated.
type subBucketRecord struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// Policy is the policy document as it was sent; none when empty.
	Policy []byte `json:"policy,omitempty"`
}

func (r subBucketRecord) toDomain() bucket.SubBucket {
	return bucket.SubBucket{Name: r.Name, CreatedAt: r.CreatedAt, Policy: bytes.Clone(r.Policy)}
}

// subBucketCmd registers a sub-bucket, or replaces the policy of one.
type subBucketCmd struct {
	Bucket    string          `json:"bucket"`
	SubBucket subBucketRecord `json:"sub_bucket"`
	// Audit is the record of the change, appended in the same transaction.
	Audit *auditRecord `json:"audit,omitempty"`
}

// subBucketRefCmd names a sub-bucket: to delete it.
type subBucketRefCmd struct {
	Bucket string       `json:"bucket"`
	Name   string       `json:"name"`
	Audit  *auditRecord `json:"audit,omitempty"`
}

// audited returns what turns the outcome of a change into the outcome of
// the entry: when the change took effect and came with an audit record, the
// record is appended in the same transaction, as applyIAM does it. A change
// that was refused is not audited here: the refusal is the proposer's to
// record.
func audited(tx *bbolt.Tx, record *auditRecord) func(applyResult, error) (applyResult, error) {
	return func(result applyResult, err error) (applyResult, error) {
		if err != nil || result.err != nil || record == nil {
			return result, err
		}
		event, err := appendAudit(tx, *record)
		if err != nil {
			return applyResult{}, err
		}
		result.events = append(result.events, event)
		return result, nil
	}
}

// applyPutSubBucket registers a sub-bucket or replaces the policy of one
// that is registered. How many a bucket has is counted here, where every
// replica counts the same. What the policy means is the business of whoever
// reads it; the state machine checks that it is of a size it keeps.
//
// What it refuses by - bucket.ValidateSubBucketName, iam.MaxPolicyBytes,
// bucket.MaxSubBuckets - is part of the replicated format: changing any of
// it needs a rise of the entry schema.
func applyPutSubBucket(tx *bbolt.Tx, cmd subBucketCmd) (applyResult, error) {
	b, err := readBucket(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	if b == nil {
		return refuse(domain.ErrBucketNotFound, "%s", cmd.Bucket)
	}
	sub := cmd.SubBucket
	if err := bucket.ValidateSubBucketName(sub.Name); err != nil {
		return applyResult{err: err}, nil
	}
	if len(sub.Policy) > iam.MaxPolicyBytes {
		return refuse(domain.ErrInvalidArgument, "sub-bucket policy of %d bytes", len(sub.Policy))
	}
	subs, err := lazyNested(tx, subBucketsBucket(), cmd.Bucket, true)
	if err != nil {
		return applyResult{}, err
	}
	if raw := subs.Get([]byte(sub.Name)); raw != nil {
		// It is the policy that is replaced: the sub-bucket is as old as
		// it was.
		var stored subBucketRecord
		if err := json.Unmarshal(raw, &stored); err != nil {
			return applyResult{}, fmt.Errorf("%w: stored sub-bucket: %w", domain.ErrIntegrity, err)
		}
		sub.CreatedAt = stored.CreatedAt
	} else {
		count := 0
		c := subs.Cursor()
		for k, _ := c.First(); k != nil && count < bucket.MaxSubBuckets; k, _ = c.Next() {
			count++
		}
		if count >= bucket.MaxSubBuckets {
			return refuse(domain.ErrTooManySubBuckets, "bucket %s has %d", cmd.Bucket, bucket.MaxSubBuckets)
		}
	}
	if len(sub.Policy) == 0 {
		sub.Policy = nil
	}
	data, err := json.Marshal(sub)
	if err != nil {
		return applyResult{}, err
	}
	return applyResult{}, subs.Put([]byte(sub.Name), data)
}

// holdsKeyUnder reports whether an index whose keys begin with object keys -
// plain, or encoded as encodeKey does - has one under prefix. The prefix of
// a sub-bucket has no zero byte, so both encodings of it are the prefix
// itself.
func holdsKeyUnder(index *bbolt.Bucket, prefix []byte) bool {
	if index == nil {
		return false
	}
	k, _ := index.Cursor().Seek(prefix)
	return k != nil && bytes.HasPrefix(k, prefix)
}

// applyDeleteSubBucket removes a sub-bucket that holds nothing. Whether it
// does is decided here, against the state as this entry finds it, as it is
// for a bucket: a put that was committed a moment before is seen.
func applyDeleteSubBucket(tx *bbolt.Tx, cmd subBucketRefCmd) (applyResult, error) {
	b, err := readBucket(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	if b == nil {
		return refuse(domain.ErrBucketNotFound, "%s", cmd.Bucket)
	}
	subs, err := lazyNested(tx, subBucketsBucket(), cmd.Bucket, false)
	if err != nil {
		return applyResult{}, err
	}
	if subs == nil || subs.Get([]byte(cmd.Name)) == nil {
		return refuse(domain.ErrSubBucketNotFound, "%s/%s", cmd.Bucket, cmd.Name)
	}
	// Empty means: no object, no version that is no longer current, no
	// delete marker, no upload in progress. The first is in "objects", the
	// next two in "versions", the last in the listing order of uploads.
	prefix := []byte(bucket.SubBucket{Name: cmd.Name}.Prefix())
	for _, parent := range [][]byte{objectsBucket(), versionsBucket(), uploadKeysBucket()} {
		if holder := tx.Bucket(parent); holder != nil && holdsKeyUnder(holder.Bucket([]byte(cmd.Bucket)), prefix) {
			return refuse(domain.ErrSubBucketNotEmpty, "%s/%s", cmd.Bucket, cmd.Name)
		}
	}
	return applyResult{}, subs.Delete([]byte(cmd.Name))
}

var _ ports.SubBucketStore = (*Store)(nil)

// auditOf returns the audit record a change carries in its entry: none when
// the record has no actor.
func auditOf(record audit.Record) (*auditRecord, error) {
	if record.Actor == "" {
		return nil, nil
	}
	if err := record.Validate(); err != nil {
		return nil, err
	}
	return auditToRecord(record), nil
}

// PutSubBucket implements ports.SubBucketStore.
func (s *Store) PutSubBucket(ctx context.Context, bucketName string, sub bucket.SubBucket, record audit.Record) error {
	// What can be refused without looking at the state is refused before
	// anything is proposed: the first entry of schema 3 a state applies is
	// where going back to the release before ends, whether the entry took
	// effect or not.
	if err := bucket.ValidateSubBucketName(sub.Name); err != nil {
		return fmt.Errorf("raftmeta.PutSubBucket: %w", err)
	}
	if len(sub.Policy) > iam.MaxPolicyBytes {
		return fmt.Errorf("raftmeta.PutSubBucket: %w: sub-bucket policy of %d bytes, at most %d", domain.ErrInvalidArgument, len(sub.Policy), iam.MaxPolicyBytes)
	}
	trail, err := auditOf(record)
	if err != nil {
		return fmt.Errorf("raftmeta.PutSubBucket: %w", err)
	}
	cmd := &subBucketCmd{
		Bucket: bucketName, Audit: trail,
		SubBucket: subBucketRecord{Name: sub.Name, CreatedAt: sub.CreatedAt.UTC(), Policy: sub.Policy},
	}
	if _, err := s.propose(ctx, entry{Type: cmdPutSubBucket, PutSubBucket: cmd}); err != nil {
		return fmt.Errorf("raftmeta.PutSubBucket: %w", err)
	}
	return nil
}

// DeleteSubBucket implements ports.SubBucketStore.
func (s *Store) DeleteSubBucket(ctx context.Context, bucketName, name string, record audit.Record) error {
	trail, err := auditOf(record)
	if err != nil {
		return fmt.Errorf("raftmeta.DeleteSubBucket: %w", err)
	}
	cmd := &subBucketRefCmd{Bucket: bucketName, Name: name, Audit: trail}
	if _, err := s.propose(ctx, entry{Type: cmdDeleteSubBucket, DeleteSubBucket: cmd}); err != nil {
		return fmt.Errorf("raftmeta.DeleteSubBucket: %w", err)
	}
	return nil
}

// readSubBuckets visits the sub-buckets of a bucket in name order. The error
// wraps domain.ErrBucketNotFound.
func readSubBuckets(tx *bbolt.Tx, bucketName string, visit func(subBucketRecord)) error {
	if b, err := readBucket(tx, bucketName); err != nil {
		return err
	} else if b == nil {
		return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
	}
	subs, err := lazyNested(tx, subBucketsBucket(), bucketName, false)
	if err != nil || subs == nil {
		return err
	}
	return subs.ForEach(func(_, raw []byte) error {
		var record subBucketRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return fmt.Errorf("%w: stored sub-bucket: %w", domain.ErrIntegrity, err)
		}
		visit(record)
		return nil
	})
}

// GetSubBucket implements ports.SubBucketStore.
func (s *Store) GetSubBucket(ctx context.Context, bucketName, name string) (bucket.SubBucket, error) {
	var found *bucket.SubBucket
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		found = nil
		return readSubBuckets(tx, bucketName, func(record subBucketRecord) {
			if record.Name == name {
				sub := record.toDomain()
				found = &sub
			}
		})
	})
	if err == nil && found == nil {
		err = fmt.Errorf("%w: %s/%s", domain.ErrSubBucketNotFound, bucketName, name)
	}
	if err != nil {
		return bucket.SubBucket{}, fmt.Errorf("raftmeta.GetSubBucket: %w", err)
	}
	return *found, nil
}

// ListSubBuckets implements ports.SubBucketStore.
func (s *Store) ListSubBuckets(ctx context.Context, bucketName string) ([]bucket.SubBucket, error) {
	var out []bucket.SubBucket
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		out = nil
		return readSubBuckets(tx, bucketName, func(record subBucketRecord) { out = append(out, record.toDomain()) })
	})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.ListSubBuckets: %w", err)
	}
	return out, nil
}
