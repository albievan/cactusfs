package raftmeta

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/ports"
)

// The targets a deployment sends to, in the state (ADR-0023, "Targets").
//
//	targets              target name -> targetRecord (JSON, "v":1)
//	targetrefs/<target>  bucket name -> the kinds of document of that bucket
//	                     that name the target: "n" for its notification
//	                     rules, "r" for its replication configuration,
//	                     "nr" for both
//
// A target is defined by target.put and by nothing else, and is kept as the
// entry gives it but for three things that are the state's: refs, which
// counts the notification and replication documents of buckets that name the
// target (bucketcfg.go keeps it, and the deletion of a bucket); created_at,
// which a target that exists keeps; and its cursor in the outbox, which is
// made with a new target and goes with one that is deleted (outbox.go).
//
// "targetrefs" says WHICH buckets those documents are of, which the count
// does not: a target that is replaced must go on admitting every bucket
// whose document names it (applyPutTarget). It is kept with the count, by
// the same function (refer), so refs is always the number of letters under
// the target; a target that nothing names has no place there, and the bucket
// itself is made when the first document names a target.
//
// What a target may be is delivery.Target.Validate, a rule of bytes. Its
// sealed fields - the secret, the signing key, the path of its endpoint - are
// judged by whether they are there and how long they are, and by nothing
// else: nothing here opens one, and the state never holds a path or a secret
// in the clear.
//
// The bucket is made when the first target is written. A state no target was
// ever defined in has none of this.

func targetsBucket() []byte    { return []byte("targets") }
func targetRefsBucket() []byte { return []byte("targetrefs") }

// The letter a kind of document is written as in "targetrefs", in the order
// they are written in.
const (
	refNotification = 'n'
	refReplication  = 'r'
)

func refLetter(kind string) byte {
	if kind == bucket.ConfigReplication {
		return refReplication
	}
	return refNotification
}

// readTarget returns the stored record of a target, or nil. A record that
// cannot be read stops the state machine, as every damaged record does.
func readTarget(tx *bbolt.Tx, name string) (*targetRecord, error) {
	targets := tx.Bucket(targetsBucket())
	if targets == nil {
		return nil, nil
	}
	raw := targets.Get([]byte(name))
	if raw == nil {
		return nil, nil
	}
	var record targetRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("%w: stored target %s: %w", domain.ErrIntegrity, name, err)
	}
	if record.V != targetRecordVersion {
		return nil, fmt.Errorf("%w: stored target %s of version %d", domain.ErrUnsupportedFormat, name, record.V)
	}
	return &record, nil
}

func writeTarget(tx *bbolt.Tx, record targetRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	targets, err := lazyBucket(tx, targetsBucket(), true)
	if err != nil {
		return err
	}
	return targets.Put([]byte(record.Name), data)
}

// countTargets returns how many targets there are, counting no further than
// limit.
func countTargets(tx *bbolt.Tx, limit int) int {
	targets := tx.Bucket(targetsBucket())
	if targets == nil {
		return 0
	}
	count := 0
	c := targets.Cursor()
	for key, _ := c.First(); key != nil && count < limit; key, _ = c.Next() {
		count++
	}
	return count
}

// namers returns the buckets whose documents name a target, in the order of
// their names.
func namers(tx *bbolt.Tx, target string) ([]string, error) {
	refs, err := lazyNested(tx, targetRefsBucket(), target, false)
	if err != nil || refs == nil {
		return nil, err
	}
	var names []string
	err = refs.ForEach(func(name, _ []byte) error {
		names = append(names, string(name))
		return nil
	})
	return names, err
}

// applyPutTarget defines a target, or replaces one of its name whole. The
// cursor of a new target begins at this entry: nothing that was written to
// the outbox before is the target's, whatever name it was written for.
//
// What it refuses by, in this order, is part of the replicated format:
// delivery.Target.Validate; for a new target, that there are
// delivery.MaxTargets already (domain.ErrInvalidArgument); and for one that
// documents of buckets name (domain.ErrEntityInUse), that it keeps its kind
// and goes on admitting every bucket whose document names it - what was
// matched for a webhook is not handed to another deployment, and a bucket
// the target is closed to is not sent on to it. A list that is widened, and
// any other change, is taken.
func applyPutTarget(tx *bbolt.Tx, cmd targetCmd, now applied) (applyResult, error) {
	record := cmd.Target
	if record.V != targetRecordVersion {
		return refuse(domain.ErrInvalidArgument, "a target record of version %d", record.V)
	}
	// The count of the documents that name it is never taken from an entry.
	record.Refs = 0
	if err := record.toDomain().Validate(); err != nil {
		return applyResult{err: err}, nil
	}
	record.CreatedAt, record.UpdatedAt = record.CreatedAt.UTC(), record.UpdatedAt.UTC()
	if record.Buckets == nil {
		// A list that admits no bucket is written as a list.
		record.Buckets = []string{}
	}
	existing, err := readTarget(tx, record.Name)
	if err != nil {
		return applyResult{}, err
	}
	if existing == nil && countTargets(tx, delivery.MaxTargets) >= delivery.MaxTargets {
		return refuse(domain.ErrInvalidArgument, "there are %d targets, and a deployment has no more", delivery.MaxTargets)
	}
	if existing != nil {
		// What was matched for a webhook is not to be handed to another
		// deployment.
		if existing.Refs > 0 && existing.Kind != record.Kind {
			return refuse(domain.ErrEntityInUse, "target %s is named by bucket configurations as a %s target", record.Name, existing.Kind)
		}
		// Nor is a bucket sent to a target that no longer admits it: the
		// document that names the target was judged against the list as it
		// was, and nothing judges it again.
		buckets, err := namers(tx, record.Name)
		if err != nil {
			return applyResult{}, err
		}
		replacement := record.toDomain()
		for _, name := range buckets {
			if !replacement.Admits(name) {
				return refuse(domain.ErrEntityInUse, "target %s is named by the configuration of bucket %s, which it would no longer admit", record.Name, name)
			}
		}
		record.Refs, record.CreatedAt = existing.Refs, existing.CreatedAt
	}
	if err := writeTarget(tx, record); err != nil {
		return applyResult{}, err
	}
	if existing == nil {
		here := delivery.Seq{Index: now.index}
		if err := writeCursor(tx, delivery.Cursor{Target: record.Name, From: here, Acked: here}); err != nil {
			return applyResult{}, err
		}
	}
	return applyResult{events: []ports.Event{
		{Type: ports.EventTargetChanged, Time: now.at, Data: ports.TargetEvent{Name: record.Name, Kind: record.Kind}},
	}}, nil
}

// applyDeleteTarget removes a target and its cursor. A target that a
// document of a bucket names is not removed.
//
// The rows that were for the target stay in the outbox: they are cut as all
// rows are, when every target that is left has passed them - and this entry
// cuts as an acknowledgement does, since the cursor that held rows back may
// have been this one. A target defined under the name later begins behind
// all of them.
func applyDeleteTarget(tx *bbolt.Tx, cmd targetRefCmd, now applied) (applyResult, error) {
	existing, err := readTarget(tx, cmd.Name)
	if err != nil {
		return applyResult{}, err
	}
	switch {
	case existing == nil:
		return refuse(domain.ErrEntityNotFound, "target %s", cmd.Name)
	case existing.Refs > 0:
		return refuse(domain.ErrEntityInUse, "target %s is named by %d bucket configurations", cmd.Name, existing.Refs)
	}
	if err := tx.Bucket(targetsBucket()).Delete([]byte(cmd.Name)); err != nil {
		return applyResult{}, err
	}
	if cursors := tx.Bucket(outboxCursorBucket()); cursors != nil {
		if err := cursors.Delete([]byte(cmd.Name)); err != nil {
			return applyResult{}, err
		}
	}
	// A target that nothing names has no place among the references; were
	// one left all the same, it would not be met by a target defined under
	// the name later.
	if refs := tx.Bucket(targetRefsBucket()); refs != nil && refs.Bucket([]byte(cmd.Name)) != nil {
		if err := refs.DeleteBucket([]byte(cmd.Name)); err != nil {
			return applyResult{}, err
		}
	}
	if err := trimOutbox(tx); err != nil {
		return applyResult{}, err
	}
	return applyResult{events: []ports.Event{
		{Type: ports.EventTargetDeleted, Time: now.at, Data: ports.TargetEvent{Name: cmd.Name, Kind: existing.Kind}},
	}}, nil
}

// refer says that the document of a kind of a bucket names each of the
// targets from now on (by is 1), or no longer (by is -1): the count of the
// target follows, and the place of the bucket among the references of the
// target. A count never goes below none; a name that is no target's - which
// the rules of this file do not let a stored document hold - is passed over,
// by every member alike. What is said twice is counted once: the count is of
// the letters, and a letter is there or is not.
func refer(tx *bbolt.Tx, names []string, bucketName, kind string, by int) error {
	letter := refLetter(kind)
	for _, name := range names {
		record, err := readTarget(tx, name)
		if err != nil {
			return err
		}
		if record == nil {
			continue
		}
		refs, err := lazyNested(tx, targetRefsBucket(), name, by > 0)
		if err != nil {
			return err
		}
		var kinds []byte
		if refs != nil {
			kinds = bytes.Clone(refs.Get([]byte(bucketName)))
		}
		at := bytes.IndexByte(kinds, letter)
		switch {
		case by > 0 && at < 0:
			// Notification before replication, whichever came first.
			kinds = append(kinds, letter)
			slices.Sort(kinds)
		case by < 0 && at >= 0:
			kinds = slices.Delete(kinds, at, at+1)
		default:
			continue
		}
		switch {
		case len(kinds) > 0:
			err = refs.Put([]byte(bucketName), kinds)
		default:
			if err = refs.Delete([]byte(bucketName)); err == nil {
				if key, _ := refs.Cursor().First(); key == nil {
					err = tx.Bucket(targetRefsBucket()).DeleteBucket([]byte(name))
				}
			}
		}
		if err != nil {
			return err
		}
		record.Refs = max(record.Refs+by, 0)
		if err := writeTarget(tx, *record); err != nil {
			return err
		}
	}
	return nil
}

// deliveredKind reports whether kind is one of the two kinds of bucket
// configuration that the state machine matches changes by: the notification
// rules of a bucket and its replication configuration (schema 5).
func deliveredKind(kind string) bool {
	return kind == bucket.ConfigNotification || kind == bucket.ConfigReplication
}

// delivered returns the document of one of those two kinds a bucket has, or
// nil. In a state no entry of schema 5 was applied to there is none,
// whatever the record of the bucket holds: such a document is made by a
// bucket.config.put of schema 5 and by nothing else, and what a bucket.create
// of an older schema carried under that name is bytes the release of that
// schema stored and never read (enterSchema5 removes them).
func delivered(tx *bbolt.Tx, b *bucketRecord, kind string) []byte {
	document := b.Config[kind]
	if len(document) == 0 || readStateSchema(tx) < stateSchema5 {
		return nil
	}
	return document
}

// damagedDocument is the error of a stored document that is not what
// applySetBucketConfig stored: it stops the state machine. A member that
// matched by a document of its own reading would append other rows than the
// others and say nothing.
func damagedDocument(bucketName, kind string, err error) error {
	return fmt.Errorf("%w: stored %s document of bucket %s: %s", domain.ErrIntegrity, kind, bucketName, err.Error())
}

// namedBy returns the targets the stored document of a kind names: those of
// a notification document sorted, each once, or the one of a replication
// configuration. No document names none.
func namedBy(bucketName, kind string, document []byte) ([]string, error) {
	if len(document) == 0 {
		return nil, nil
	}
	if kind == bucket.ConfigReplication {
		config, err := bucket.ParseReplicationConfig(document)
		if err != nil {
			return nil, damagedDocument(bucketName, kind, err)
		}
		return config.Targets(), nil
	}
	config, err := bucket.ParseNotificationConfig(document)
	if err != nil {
		return nil, damagedDocument(bucketName, kind, err)
	}
	return config.Targets(), nil
}

// unreadDocument is the refusal of a document an entry carries that is not in
// its canonical form. One of a version this release does not know is refused
// like any other that cannot be read: a refusal never says that a format is
// not supported, which is what stops a state machine.
func unreadDocument(kind string, err error) *applyResult {
	if errors.Is(err, domain.ErrUnsupportedFormat) {
		return refusal(domain.ErrInvalidArgument, "a %s document of a version that is not known", kind)
	}
	return &applyResult{err: err}
}

// checkDelivered reports whether a notification or replication document can
// become the bucket's, and returns the targets it names: it is in its
// canonical form (bucket.ParseNotificationConfig, ParseReplicationConfig);
// for replication the bucket is no replica and keeps versions; and every
// target it names is there, is of a kind that serves it, and admits the
// bucket. No document removes the configuration and is always allowed.
//
// All of it is part of the replicated format: the two parsers, the order of
// the refusals, delivery.Kind.Serves and delivery.Target.Admits.
func checkDelivered(tx *bbolt.Tx, b *bucketRecord, kind string, document []byte) ([]string, *applyResult, error) {
	if len(document) == 0 {
		return nil, nil, nil
	}
	// A bucket.create is not held to a name in apply (the release of schema
	// 4 is not), and a row of the outbox names its bucket: only a bucket of
	// a name as long as a name can be is matched by.
	if len(b.Name) > bucket.MaxNameLength {
		return nil, refusal(domain.ErrInvalidArgument, "a bucket of a name of more than %d bytes has no %s document", bucket.MaxNameLength, kind), nil
	}
	consumer := delivery.ConsumerNotification
	var names []string
	if kind == bucket.ConfigReplication {
		config, err := bucket.ParseReplicationConfig(document)
		if err != nil {
			return nil, unreadDocument(kind, err), nil
		}
		switch {
		case b.ReplicaFrom != "":
			return nil, refusal(domain.ErrInvalidBucketState, "bucket %s is a replica and is not replicated again", b.Name), nil
		case b.Versioning != string(bucket.VersioningEnabled):
			return nil, refusal(domain.ErrInvalidBucketState, "bucket %s does not keep versions", b.Name), nil
		}
		consumer, names = delivery.ConsumerReplication, config.Targets()
	} else {
		config, err := bucket.ParseNotificationConfig(document)
		if err != nil {
			return nil, unreadDocument(kind, err), nil
		}
		names = config.Targets()
	}
	for _, name := range names {
		record, err := readTarget(tx, name)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case record == nil:
			return nil, refusal(domain.ErrInvalidArgument, "there is no target %s", name), nil
		case !delivery.Kind(record.Kind).Serves(consumer):
			return nil, refusal(domain.ErrInvalidArgument, "target %s is a %s target", name, record.Kind), nil
		case !record.toDomain().Admits(b.Name):
			return nil, refusal(domain.ErrInvalidArgument, "target %s is not open to bucket %s", name, b.Name), nil
		}
	}
	return names, nil, nil
}

// replaceDelivered keeps the counts of the targets when the document of a
// kind is replaced: one fewer for each target only the old one names, one
// more for each only the new one names.
func replaceDelivered(tx *bbolt.Tx, b *bucketRecord, kind string, names []string) error {
	old, err := namedBy(b.Name, kind, delivered(tx, b, kind))
	if err != nil {
		return err
	}
	var gone, come []string
	for _, name := range old {
		if !slices.Contains(names, name) {
			gone = append(gone, name)
		}
	}
	for _, name := range names {
		if !slices.Contains(old, name) {
			come = append(come, name)
		}
	}
	if err := refer(tx, gone, b.Name, kind, -1); err != nil {
		return err
	}
	return refer(tx, come, b.Name, kind, 1)
}

// forgetDelivered takes a bucket that is being deleted out of the counts of
// the targets its two documents name. The rows it left in the outbox stay
// for their targets (ADR-0023): nothing of the outbox is walked for a bucket.
func forgetDelivered(tx *bbolt.Tx, b *bucketRecord) error {
	for _, kind := range []string{bucket.ConfigNotification, bucket.ConfigReplication} {
		names, err := namedBy(b.Name, kind, delivered(tx, b, kind))
		if err != nil {
			return err
		}
		if err := refer(tx, names, b.Name, kind, -1); err != nil {
			return err
		}
	}
	return nil
}

// enterSchema5 marks the state as holding what only schema 5 can express,
// when the first entry of schema 5 is applied to it. From here on the two
// kinds of bucket configuration of schema 5 mean something, so whatever a
// bucket record holds under their names goes first: no bucket.config.put
// below schema 5 stores them, but a bucket.create of schema 2 to 4 stores the
// configuration its entry carries, of any kind, and the release of schema 4
// applies such an entry alike. No proposer ever wrote one. A document that
// was not judged against the targets, and that no count of a target knows
// of, must not be matched by.
//
// It reads every bucket record once, in the one transaction that makes the
// state schema 5, and writes those that held such a kind - none, in a state
// that members wrote.
func enterSchema5(tx *bbolt.Tx) error {
	if readStateSchema(tx) >= stateSchema5 {
		return nil
	}
	buckets := tx.Bucket(bucketsBucket())
	cleaned := map[string][]byte{}
	err := buckets.ForEach(func(name, raw []byte) error {
		var b bucketRecord
		if err := json.Unmarshal(raw, &b); err != nil {
			return fmt.Errorf("%w: stored bucket: %w", domain.ErrIntegrity, err)
		}
		if !stripDelivered(&b) {
			return nil
		}
		data, err := json.Marshal(b)
		if err != nil {
			return err
		}
		cleaned[string(name)] = data
		return nil
	})
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(cleaned)) {
		if err := buckets.Put([]byte(name), cleaned[name]); err != nil {
			return err
		}
	}
	return raiseStateSchema(tx, stateSchema5)
}

// stripDelivered takes the two kinds of schema 5 out of the configuration of
// a bucket record, and reports whether there was one.
func stripDelivered(b *bucketRecord) bool {
	found := false
	for _, kind := range []string{bucket.ConfigNotification, bucket.ConfigReplication} {
		if _, there := b.Config[kind]; there {
			delete(b.Config, kind)
			found = true
		}
	}
	if found && len(b.Config) == 0 {
		b.Config = nil
	}
	return found
}
