package raftmeta

import (
	"context"
	"encoding/json"
	"fmt"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
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
// binary: tag rules exist from schema 3 on. In an entry of an older schema
// that kind is refused, exactly as the binary of that schema refuses it, so
// that the two never disagree about an entry; bucket.ValidConfigKind stays
// the list of schema 2.
func applySetBucketConfig(tx *bbolt.Tx, cmd bucketConfigCmd, now applied) (applyResult, error) {
	b, err := readBucket(tx, cmd.Bucket)
	if err != nil {
		return applyResult{}, err
	}
	switch {
	case b == nil:
		return refuse(domain.ErrBucketNotFound, "%s", cmd.Bucket)
	case !bucket.ValidConfigKind(cmd.Kind) && (cmd.Kind != bucket.ConfigTagRules || now.schema < entrySchema3):
		return refuse(domain.ErrInvalidArgument, "bucket configuration %q", cmd.Kind)
	case len(cmd.Document) > bucket.MaxConfigBytes:
		return refuse(domain.ErrInvalidArgument, "bucket configuration of %d bytes", len(cmd.Document))
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
