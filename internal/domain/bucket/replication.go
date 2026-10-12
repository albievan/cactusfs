package bucket

import (
	"bytes"
	"fmt"
	"math"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// ReplicationRule says which keys of a bucket are copied to another
// deployment, and whether their delete markers are.
type ReplicationRule struct {
	// ID names the rule; it is unique in the bucket.
	ID string
	// Enabled is false for a rule that is kept and matches nothing.
	Enabled bool
	// Priority decides among the rules that are about a key: the
	// greatest wins, and of two of the same priority the one that was
	// put first.
	Priority int
	// Prefix, when not empty, confines the rule to keys that begin with
	// it, byte for byte.
	Prefix string
	// Markers says that a delete marker made by a caller is copied too.
	Markers bool
}

// ReplicationConfig says where the versions of a bucket are copied to: one
// target, one bucket there, and the rules in the order they were put
// (ADR-0026).
type ReplicationConfig struct {
	// Target names the target, which is another deployment.
	Target string
	// Bucket is the bucket of the other deployment.
	Bucket string
	Rules  []ReplicationRule
}

func invalidReplication(format string, args ...any) error {
	return fmt.Errorf("%w: replication: %s", domain.ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// Validate reports whether the configuration can be kept. Whether the target
// is there and admits the bucket, and whether the bucket keeps versions, is
// for the store to say. The error wraps domain.ErrInvalidArgument.
func (c ReplicationConfig) Validate() error {
	if err := delivery.ValidateTargetName(c.Target); err != nil {
		return invalidReplication("%v", err)
	}
	if err := ValidateName(c.Bucket); err != nil {
		return invalidReplication("the bucket of the other deployment has no name a bucket can have")
	}
	if len(c.Rules) == 0 || len(c.Rules) > MaxReplicationRules {
		return invalidReplication("needs 1 to %d rules", MaxReplicationRules)
	}
	ids := map[string]bool{}
	for i, rule := range c.Rules {
		switch {
		case rule.ID == "" || !validRuleText(rule.ID, maxRuleID):
			return invalidReplication("rule %d: an ID of 1 to %d bytes", i, maxRuleID)
		case ids[rule.ID]:
			return invalidReplication("rule %d: its ID is used twice", i)
		case rule.Priority < 0 || rule.Priority > math.MaxInt32:
			return invalidReplication("rule %d: a priority from 0 to %d", i, math.MaxInt32)
		case !validRuleText(rule.Prefix, maxRuleAffix):
			return invalidReplication("rule %d: a prefix of more than %d bytes", i, maxRuleAffix)
		}
		ids[rule.ID] = true
	}
	return nil
}

// Empty reports whether there is no configuration.
func (c ReplicationConfig) Empty() bool {
	return c.Target == "" && c.Bucket == "" && len(c.Rules) == 0
}

// Targets returns the name of the target, as the one of a list: what a
// store counts the references of.
func (c ReplicationConfig) Targets() []string {
	if c.Target == "" {
		return nil
	}
	return []string{c.Target}
}

// Document returns the configuration as the document that is stored and
// that every member of a cluster matches by, in the canonical form
// (ADR-0023):
//
//	{"v":1,"target","bucket","rules":[{"id","enabled","priority","prefix","markers"}]}
//
// with "enabled" and "markers" left out when false and "prefix" when empty.
// The same configuration is always the same bytes, whatever release of Go
// wrote them.
func (c ReplicationConfig) Document() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	w := &canonicalWriter{}
	w.raw(`{"v":`)
	w.num(configVersion)
	w.raw(`,"target":`)
	w.str(c.Target)
	w.raw(`,"bucket":`)
	w.str(c.Bucket)
	w.raw(`,"rules":[`)
	for i, rule := range c.Rules {
		if i > 0 {
			w.raw(",")
		}
		w.raw(`{"id":`)
		w.str(rule.ID)
		if rule.Enabled {
			w.raw(`,"enabled":true`)
		}
		w.raw(`,"priority":`)
		w.num(int64(rule.Priority))
		if rule.Prefix != "" {
			w.raw(`,"prefix":`)
			w.str(rule.Prefix)
		}
		if rule.Markers {
			w.raw(`,"markers":true`)
		}
		w.raw("}")
	}
	w.raw("]}")
	if w.bad {
		return nil, invalidReplication("the document cannot be written")
	}
	if len(w.buf) > MaxConfigBytes {
		return nil, invalidReplication("the document is longer than %d bytes", MaxConfigBytes)
	}
	return w.buf, nil
}

// ParseReplicationConfig reads a stored document. An empty document is no
// configuration. Like ParseNotificationConfig it takes the bytes Document
// writes and no others. The error wraps domain.ErrInvalidArgument, or
// domain.ErrUnsupportedFormat for a document of a version this code does not
// know.
func ParseReplicationConfig(document []byte) (ReplicationConfig, error) {
	if len(document) == 0 {
		return ReplicationConfig{}, nil
	}
	if len(document) > MaxConfigBytes {
		return ReplicationConfig{}, invalidReplication("the document is longer than %d bytes", MaxConfigBytes)
	}
	r := &canonicalReader{data: document}
	r.lit(`{"v":`)
	version := r.small()
	if r.bad {
		return ReplicationConfig{}, invalidReplication("the document is not in its canonical form")
	}
	if version != configVersion {
		return ReplicationConfig{}, fmt.Errorf("%w: replication configuration of version %d", domain.ErrUnsupportedFormat, version)
	}
	var config ReplicationConfig
	r.lit(`,"target":`)
	config.Target = r.str()
	r.lit(`,"bucket":`)
	config.Bucket = r.str()
	r.lit(`,"rules":`)
	r.list(func() {
		var rule ReplicationRule
		r.lit(`{"id":`)
		rule.ID = r.str()
		rule.Enabled = r.has(`,"enabled":true`)
		r.lit(`,"priority":`)
		rule.Priority = r.small()
		if r.has(`,"prefix":`) {
			rule.Prefix = r.str()
		}
		rule.Markers = r.has(`,"markers":true`)
		r.lit("}")
		config.Rules = append(config.Rules, rule)
	})
	r.lit("}")
	if !r.end() {
		return ReplicationConfig{}, invalidReplication("the document is not in its canonical form")
	}
	again, err := config.Document()
	if err != nil {
		return ReplicationConfig{}, err
	}
	if !bytes.Equal(again, document) {
		return ReplicationConfig{}, invalidReplication("the document is not in its canonical form")
	}
	return config, nil
}

// Decides returns the rule that decides for a key: of the enabled rules
// whose prefix the key begins with, the one of the greatest priority, and of
// several of that priority the first.
func (c ReplicationConfig) Decides(key string) (ReplicationRule, bool) {
	var deciding ReplicationRule
	found := false
	for _, rule := range c.Rules {
		if !rule.Enabled || !strings.HasPrefix(key, rule.Prefix) {
			continue
		}
		if !found || rule.Priority > deciding.Priority {
			deciding, found = rule, true
		}
	}
	return deciding, found
}

// Match returns the consumer a change of the kind event to the key is for -
// the target, when a rule decides for the key - or none:
//
//   - a version that arrived (delivery.EventPut, delivery.EventComplete)
//     and a later change of its tags, retention or legal hold are for it;
//   - a delete marker is for it when the deciding rule copies markers;
//   - the removal of a version for good never is: a source that was taken
//     over must not be able to purge the other deployment.
//
// What Match cannot see is the caller's to leave out: a version that is
// itself a replica, a delete marker a lifecycle rule made (LifecycleActor),
// and a later change of a version that has no replication status.
//
// It is part of the replicated format: the state machine of every member
// appends the rows of the outbox by it.
func (c ReplicationConfig) Match(event delivery.Event, key string) []delivery.Consumer {
	rule, ok := c.Decides(key)
	switch {
	case !ok || !event.Valid() || event == delivery.EventDelete:
		return nil
	case event == delivery.EventMarker && !rule.Markers:
		return nil
	}
	return []delivery.Consumer{{Kind: delivery.ConsumerReplication, Target: c.Target}}
}

// LifecycleActor is what the audit trail calls the service that carries out
// lifecycle rules. A delete marker it made is not copied to another
// deployment: each expires by its own rules.
const LifecycleActor = "service/lifecycle"

// Replicates reports whether a change is one to send to another deployment,
// given what Match cannot see: by is who made it, replica says that the
// version arrived as a replica, and status is the replication status the
// version had before the change. A replica is never sent on; a delete marker
// of the lifecycle service is not sent; and a change of tags, retention or
// legal hold is sent only for a version that is being, or was, replicated.
//
// It is part of the replicated format, like Match.
func (c ReplicationConfig) Replicates(event delivery.Event, key, by string, replica bool, status object.ReplicationStatus) []delivery.Consumer {
	switch {
	case replica || status == object.ReplicationReplica:
		return nil
	case event == delivery.EventMarker && by == LifecycleActor:
		return nil
	case (event == delivery.EventTags || event == delivery.EventRetention || event == delivery.EventLegalHold) && status == "":
		return nil
	}
	return c.Match(event, key)
}

// ResyncState is how far the walk has come that sends another deployment
// what it lacks of a bucket.
type ResyncState string

// The states of a resync.
const (
	// ResyncRunning is a walk that goes on - also while it waits for a
	// target that cannot be reached.
	ResyncRunning ResyncState = "running"
	// ResyncDone is a walk that has been through every version.
	ResyncDone ResyncState = "done"
)

// Resync is the record of the walk that sends another deployment what it
// lacks of a bucket (ADR-0026). It is kept in the metadata, so that a node
// that comes to lead goes on where the last one was.
type Resync struct {
	// Bucket is the bucket that is walked, and Target where it is sent.
	Bucket string
	Target string
	State  ResyncState
	// Key and Version are the last version the walk has dealt with, as
	// object.VersionRef writes a version; both empty before the first.
	Key     string
	Version string
	// Scanned counts the versions looked at, Sent those the other
	// deployment did not have and was sent.
	Scanned uint64
	Sent    uint64
}

// Validate reports whether r is a record that can be kept. The zero value
// is a bucket that was never resynced, and is not valid as a record. The
// error wraps domain.ErrInvalidArgument.
func (r Resync) Validate() error {
	invalid := func(reason string) error {
		return fmt.Errorf("%w: resync: %s", domain.ErrInvalidArgument, reason)
	}
	if err := delivery.ValidateTargetName(r.Target); err != nil {
		return err
	}
	switch {
	case ValidateName(r.Bucket) != nil:
		return invalid("a bucket")
	case r.State != ResyncRunning && r.State != ResyncDone:
		return invalid("running or done")
	case r.Sent > r.Scanned:
		return invalid("more versions sent than looked at")
	case (r.Key == "") != (r.Version == ""):
		return invalid("a key and a version, or neither")
	case r.Key == "":
		return nil
	case object.ValidateKey(r.Key) != nil:
		return invalid("the key is none")
	}
	if _, err := object.ParseVersionRef(r.Version); err != nil {
		return invalid("the version is none")
	}
	return nil
}
