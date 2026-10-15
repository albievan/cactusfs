package raftmeta

import (
	"context"
	"encoding/json"
	"fmt"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

const (
	cmdSetBucketConfig = "bucket.config.put"
	cmdSetTags         = "object.tags.put"
)

// applySetBucketConfig replaces or removes one configuration document of a
// bucket. What a document means is the business of whoever reads it; the
// state machine checks that it is of a kind there is and of a size it keeps.
//
// Which kinds there are depends on the schema of the entry, not on the
// binary: tag rules exist from schema 3 on, and the notification rules and
// the replication configuration from schema 5 on. In an entry of an older
// schema such a kind is refused, exactly as the binary of that schema refuses
// it, so that the two never disagree about an entry; bucket.ValidConfigKind
// stays the list of schema 2.
//
// The two kinds of schema 5 are the exception to "what a document means is
// the business of whoever reads it": the state machine reads them, to match
// every change of an object by (outbox.go), so it takes them only as the
// bytes their parsers write and judges the targets they name (targets.go).
func applySetBucketConfig(tx *bbolt.Tx, cmd bucketConfigCmd, now applied) (applyResult, error) {
	b, err := readBucket(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	switch {
	case b == nil:
		return refuse(domain.ErrBucketNotFound, "%s", cmd.Bucket)
	case !bucket.ValidConfigKind(cmd.Kind) && (cmd.Kind != bucket.ConfigTagRules || now.schema < entrySchema3) &&
		(!deliveredKind(cmd.Kind) || now.schema < entrySchema5):
		return refuse(domain.ErrInvalidArgument, "bucket configuration %q", cmd.Kind)
	case len(cmd.Document) > bucket.MaxConfigBytes:
		return refuse(domain.ErrInvalidArgument, "bucket configuration of %d bytes", len(cmd.Document))
	}
	if deliveredKind(cmd.Kind) {
		// The two documents the state machine matches changes by (schema
		// 5): taken in their canonical form only, and only when every
		// target they name is there, fits and admits the bucket. The
		// targets are counted by the documents that name them.
		names, refused, err := checkDelivered(tx, b, cmd.Kind, cmd.Document)
		if err != nil {
			return applyResult{}, err
		}
		if refused != nil {
			return *refused, nil
		}
		if err := replaceDelivered(tx, b, cmd.Kind, names); err != nil {
			return applyResult{}, err
		}
	}
	if len(cmd.Document) == 0 {
		delete(b.Config, cmd.Kind)
	} else {
		if b.Config == nil {
			b.Config = map[string][]byte{}
		}
		b.Config[cmd.Kind] = cmd.Document
	}
	data, err := json.Marshal(*b)
	if err != nil {
		return applyResult{}, err
	}
	if err := tx.Bucket(bucketsBucket()).Put([]byte(cmd.Bucket), data); err != nil {
		return applyResult{}, err
	}
	return applyResult{events: []ports.Event{
		{Type: ports.EventBucketConfigChanged, Data: ports.BucketEvent{Bucket: cmd.Bucket}},
	}}, nil
}

// applySetTags replaces the tags of a version. Tags are not part of what a
// lock keeps: a locked version can be tagged.
func applySetTags(tx *bbolt.Tx, cmd tagsCmd, now applied) (applyResult, error) {
	if err := object.ValidateTags(cmd.Tags, object.MaxObjectTags); err != nil {
		return applyResult{err: err}, nil
	}
	h, target, storedKey, refused, err := versionTarget(tx, cmd.Bucket, cmd.Key, cmd.VersionRef, false)
	if err != nil || refused != nil {
		if refused != nil {
			return *refused, nil
		}
		return applyResult{}, err
	}
	target.Tags = cmd.Tags
	if len(target.Tags) == 0 {
		target.Tags = nil
	}
	if err := h.replaceVersion(*target, storedKey); err != nil {
		return applyResult{}, err
	}
	stored := *target
	if err := tellChanged(tx, now, delivery.EventTags, stored); err != nil {
		return applyResult{}, err
	}
	return applyResult{stored: &stored, events: []ports.Event{
		{Type: ports.EventObjectTagsChanged, Time: now.at, Data: objectEvent(stored)},
	}}, nil
}

// SetBucketConfig implements ports.BucketStore.
func (s *Store) SetBucketConfig(ctx context.Context, name, kind string, document []byte) error {
	if kind == bucket.ConfigTagRules && len(document) > bucket.MaxConfigBytes {
		// Tag rules are an entry of schema 3, and the first of those a
		// state applies is where going back to the release before ends,
		// whether the entry took effect or not: what can be refused
		// without looking at the state is refused before it is proposed.
		return fmt.Errorf("raftmeta.SetBucketConfig: %w: bucket configuration of %d bytes, at most %d", domain.ErrInvalidArgument, len(document), bucket.MaxConfigBytes)
	}
	// So for the two kinds of schema 5 (ADR-0023): a document that is not
	// in its canonical form is refused by every member by its bytes alone,
	// and here before any of them sees it. A document of a version this
	// release does not know is told as that - domain.ErrUnsupportedFormat,
	// by the domain's own reader - which only the proposer may say: where
	// an entry is applied the same document is a refusal like any other
	// (unreadDocument), since "unsupported format" from a state machine
	// means that it stopped.
	if len(document) > 0 {
		var err error
		switch kind {
		case bucket.ConfigNotification:
			_, err = bucket.ParseNotificationConfig(document)
		case bucket.ConfigReplication:
			_, err = bucket.ParseReplicationConfig(document)
		}
		if err != nil {
			return fmt.Errorf("raftmeta.SetBucketConfig: %w", err)
		}
	}
	e := entry{Type: cmdSetBucketConfig, SetBucketConfig: &bucketConfigCmd{Bucket: name, Kind: kind, Document: document}}
	if _, err := s.propose(ctx, e); err != nil {
		return fmt.Errorf("raftmeta.SetBucketConfig: %w", err)
	}
	return nil
}

// LocalConfig reads the configuration of buckets from this node's own copy
// of the state, without confirming with the leader that it is current.
type LocalConfig struct {
	store *Store
}

// LocalConfig returns a reader of bucket configuration that never leaves
// this node.
func (s *Store) LocalConfig() LocalConfig {
	return LocalConfig{store: s}
}

// GetBucketConfig returns one configuration document of a bucket as this
// node has it, or nil.
func (l LocalConfig) GetBucketConfig(_ context.Context, name, kind string) ([]byte, error) {
	return l.store.bucketConfig(name, kind, l.store.peek)
}

// Bounds of one page of LocalConfig.BucketPolicies, which is one read
// transaction: it reads buckets until it has read policyPageBuckets of them
// or policyPageBytes of their stored records, whichever comes first, and at
// least one. A stored record holds a document in base64, a third longer than
// the document: a megabyte of records is some 45 buckets whose policies are
// 17 KiB each, and takes 1.4 ms to read and decode on the machine it was
// measured on (BenchmarkLocalBucketPolicies, Apple M3) - that is how long a
// page holds the read lock of the state, which a change waiting to be applied
// waits for.
const (
	policyPageBuckets = 256
	policyPageBytes   = 1 << 20
)

// BucketPolicies implements ports.BucketPolicies: one page of buckets, read
// in one read transaction on this node's copy of the state. Nothing is asked
// of the leader; a caller that has just made a consistent read finds the copy
// at least as new as that read, since the state a node has applied never goes
// back.
//
// The transaction, and with it the read lock of the state, ends before visit
// is first called: what visit does with a document - hashing it, parsing it -
// holds up no change. The documents visit is given are decoded copies on the
// heap, never memory of the database; the port still lends them for the call
// only.
func (l LocalConfig) BucketPolicies(ctx context.Context, from string, visit func(name string, document []byte) error) (string, error) {
	type found struct {
		name     string
		document []byte
	}
	var page []found
	next := ""
	err := l.store.peek(func(tx *bbolt.Tx) error {
		read, size := 0, 0
		cursor := tx.Bucket(bucketsBucket()).Cursor()
		for name, raw := cursor.Seek([]byte(from)); name != nil; name, raw = cursor.Next() {
			if read >= policyPageBuckets || size >= policyPageBytes {
				next = string(name)
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			read++
			size += len(raw)
			var record bucketRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return fmt.Errorf("%w: stored bucket: %w", domain.ErrIntegrity, err)
			}
			if document := record.Config[bucket.ConfigPolicy]; len(document) > 0 {
				page = append(page, found{name: string(name), document: document})
			}
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("raftmeta.BucketPolicies: %w", err)
	}
	for _, p := range page {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("raftmeta.BucketPolicies: %w", err)
		}
		if err := visit(p.name, p.document); err != nil {
			return "", fmt.Errorf("raftmeta.BucketPolicies: %w", err)
		}
	}
	return next, nil
}

var _ ports.BucketPolicies = LocalConfig{}

// GetBucketConfig implements ports.BucketStore.
func (s *Store) GetBucketConfig(ctx context.Context, name, kind string) ([]byte, error) {
	return s.bucketConfig(name, kind, func(fn func(tx *bbolt.Tx) error) error { return s.view(ctx, fn) })
}

func (s *Store) bucketConfig(name, kind string, read func(func(tx *bbolt.Tx) error) error) ([]byte, error) {
	var document []byte
	err := read(func(tx *bbolt.Tx) error {
		b, err := readBucket(tx, name)
		if err != nil {
			return err
		}
		if b == nil {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, name)
		}
		document = append([]byte(nil), b.Config[kind]...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.GetBucketConfig: %w", err)
	}
	if len(document) == 0 {
		return nil, nil
	}
	return document, nil
}

// SetTags implements ports.ObjectIndex.
func (s *Store) SetTags(ctx context.Context, bucketName, key string, ref object.VersionRef, tags map[string]string) (object.Version, error) {
	cmd := &tagsCmd{Bucket: bucketName, Key: key, VersionRef: ref.String(), Tags: tags}
	result, err := s.propose(ctx, entry{Type: cmdSetTags, SetTags: cmd})
	if err != nil {
		return object.Version{}, fmt.Errorf("raftmeta.SetTags: %w", err)
	}
	return storedVersion("SetTags", result)
}
