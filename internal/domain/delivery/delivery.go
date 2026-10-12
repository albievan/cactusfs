// Package delivery holds the domain model of what a deployment sends to
// places outside itself (ADR-0023): the targets it sends to, the rows of the
// outbox and the cursor each target has in it, what a failure to deliver is
// called, and the rules by which an endpoint and an address are judged.
//
// Nothing here connects to anything. And nothing here puts an endpoint, a
// credential or the words of somebody else's error into an error or a
// printed form of its own: what delivery says of a target is its name, its
// kind and the class of a failure.
package delivery

import (
	"fmt"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
)

// MaxTargetNameLength is the longest name a target can have.
const MaxTargetNameLength = 63

// Bounds of the outbox. They are part of the replicated format: the state
// machine of every member cuts the outbox by them, so changing any needs a
// rise of the entry schema.
const (
	// MaxOutboxRows is how many rows the outbox holds. A row that would be
	// one more pushes the oldest out, and that is counted (Cursor.Dropped).
	MaxOutboxRows = 1_000_000
	// MaxOutboxTrim is how many rows one acknowledgement removes at most
	// of those every target has passed.
	MaxOutboxTrim = 1000
	// MaxAckResults is how many results one acknowledgement carries.
	MaxAckResults = 1000
	// MaxOutboxRead is how many items one read of the outbox returns.
	MaxOutboxRead = 1000
)

// ValidateTargetName reports whether name can name a target: 1 to 63
// lower-case letters, digits and hyphens, beginning with a letter or digit.
// The name is part of the ARNs of the S3 API and of the consumers in the
// outbox. The error wraps domain.ErrInvalidArgument.
//
// It is part of the replicated format: the state machine of every member
// refuses a target, and a document that names one, by it. It looks at bytes
// only, so that every build decides alike.
func ValidateTargetName(name string) error {
	valid := len(name) >= 1 && len(name) <= MaxTargetNameLength && name[0] != '-'
	for i := 0; valid && i < len(name); i++ {
		c := name[i]
		valid = (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-'
	}
	if !valid {
		return fmt.Errorf("%w: a target is named with 1 to %d lower-case letters, digits and hyphens, and not a hyphen first",
			domain.ErrInvalidArgument, MaxTargetNameLength)
	}
	return nil
}

// Event is the kind of change a row of the outbox tells of.
type Event string

// The kinds of change. The first four are told to notification targets
// (ADR-0025); replication is sent all of them but EventDelete (ADR-0026).
const (
	// EventPut is a version committed by a put or a copy.
	EventPut Event = "put"
	// EventComplete is a version committed by the completion of a
	// multipart upload.
	EventComplete Event = "complete"
	// EventMarker is a delete that left a delete marker.
	EventMarker Event = "marker"
	// EventDelete is a version removed for good.
	EventDelete Event = "delete"
	// EventTags is a change of the tags of a version.
	EventTags Event = "tags"
	// EventRetention is a change of the retention of a version.
	EventRetention Event = "retention"
	// EventLegalHold is a change of the legal hold of a version.
	EventLegalHold Event = "legalhold"
)

// Valid reports whether e is a kind of change there is.
func (e Event) Valid() bool {
	switch e {
	case EventPut, EventComplete, EventMarker, EventDelete, EventTags, EventRetention, EventLegalHold:
		return true
	}
	return false
}

// Notified reports whether a notification rule can ask for e: a version that
// arrived, or one that went.
func (e Event) Notified() bool {
	return e == EventPut || e == EventComplete || e == EventMarker || e == EventDelete
}

// ConsumerKind says what a consumer of a row does with it.
type ConsumerKind string

// The kinds of consumer, as they are written in a row.
const (
	// ConsumerNotification tells a target of the change (ADR-0025).
	ConsumerNotification ConsumerKind = "n"
	// ConsumerReplication sends the version to another deployment
	// (ADR-0026).
	ConsumerReplication ConsumerKind = "r"
)

// Consumer is one use a row of the outbox is for: a kind, and the target it
// goes to.
type Consumer struct {
	Kind   ConsumerKind
	Target string
}

// String returns the consumer as a row writes it: "n:<target>" or
// "r:<target>".
func (c Consumer) String() string {
	return string(c.Kind) + ":" + c.Target
}

// ParseConsumer reads a consumer as a row writes it. The error wraps
// domain.ErrInvalidArgument.
func ParseConsumer(s string) (Consumer, error) {
	kind, target, found := strings.Cut(s, ":")
	c := Consumer{Kind: ConsumerKind(kind), Target: target}
	if !found || (c.Kind != ConsumerNotification && c.Kind != ConsumerReplication) || ValidateTargetName(target) != nil {
		return Consumer{}, fmt.Errorf("%w: a consumer is n:<target> or r:<target>", domain.ErrInvalidArgument)
	}
	return c, nil
}

// The ARNs by which the S3 API names a target: a notification rule names
// its queue or topic, a replication configuration its role.
const (
	notificationARNPrefix = "arn:cactusfs:notify:::"
	replicationARNPrefix  = "arn:cactusfs:replication:::"
)

// ARN returns the consumer as the S3 API names it:
// "arn:cactusfs:notify:::<target>" in a notification configuration,
// "arn:cactusfs:replication:::<target>" as the role of a replication
// configuration.
func (c Consumer) ARN() string {
	if c.Kind == ConsumerReplication {
		return replicationARNPrefix + c.Target
	}
	return notificationARNPrefix + c.Target
}

// ParseARN reads an ARN of one of the two forms Consumer.ARN writes. Nothing
// is passed over: another partition, a region, an account or a name that is
// no target's is refused. The error wraps domain.ErrInvalidArgument and does
// not repeat what it was given.
func ParseARN(arn string) (Consumer, error) {
	var c Consumer
	switch {
	case strings.HasPrefix(arn, notificationARNPrefix):
		c = Consumer{Kind: ConsumerNotification, Target: strings.TrimPrefix(arn, notificationARNPrefix)}
	case strings.HasPrefix(arn, replicationARNPrefix):
		c = Consumer{Kind: ConsumerReplication, Target: strings.TrimPrefix(arn, replicationARNPrefix)}
	default:
		return Consumer{}, fmt.Errorf("%w: a target is named arn:cactusfs:notify:::<name> or arn:cactusfs:replication:::<name>", domain.ErrInvalidArgument)
	}
	if err := ValidateTargetName(c.Target); err != nil {
		return Consumer{}, err
	}
	return c, nil
}
