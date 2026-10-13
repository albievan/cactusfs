package delivery

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// SeqSize is the size of the key of a row of the outbox, in bytes.
const SeqSize = 10

// Seq is the place of a row in the outbox: the index of the entry that made
// it, and the number of the row among those the entry made. Of two rows the
// one with the greater Seq was made later, in the whole deployment.
type Seq struct {
	// Index is the index of the Raft entry.
	Index uint64
	// Row numbers the rows of one entry, from 0.
	Row uint16
}

// IsZero reports whether s is the place before every row.
func (s Seq) IsZero() bool {
	return s == Seq{}
}

// Compare returns -1, 0 or 1 as s is before, at or after o.
func (s Seq) Compare(o Seq) int {
	switch {
	case s.Index < o.Index, s.Index == o.Index && s.Row < o.Row:
		return -1
	case s == o:
		return 0
	}
	return 1
}

// After reports whether s is a later place than o.
func (s Seq) After(o Seq) bool {
	return s.Compare(o) > 0
}

// Key returns the key of the row in the outbox: the index and then the row,
// both big-endian, so that keys sort as places do.
func (s Seq) Key() [SeqSize]byte {
	var key [SeqSize]byte
	binary.BigEndian.PutUint64(key[:8], s.Index)
	binary.BigEndian.PutUint16(key[8:], s.Row)
	return key
}

// SeqFromKey reads the key of a row. The error wraps
// domain.ErrInvalidArgument.
func SeqFromKey(key []byte) (Seq, error) {
	if len(key) != SeqSize {
		return Seq{}, fmt.Errorf("%w: the key of an outbox row has %d bytes", domain.ErrInvalidArgument, SeqSize)
	}
	return Seq{Index: binary.BigEndian.Uint64(key[:8]), Row: binary.BigEndian.Uint16(key[8:])}, nil
}

// String returns the place as 20 lower-case hexadecimal characters: the
// sequencer of a notification, and the ID of the message a row becomes. Of
// two of them the greater, compared as text, was made later.
func (s Seq) String() string {
	key := s.Key()
	return hex.EncodeToString(key[:])
}

// ParseSeq reads what String writes. The error wraps
// domain.ErrInvalidArgument.
func ParseSeq(text string) (Seq, error) {
	if len(text) != 2*SeqSize {
		return Seq{}, fmt.Errorf("%w: a place in the outbox is %d hexadecimal characters", domain.ErrInvalidArgument, 2*SeqSize)
	}
	for i := range len(text) {
		if c := text[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return Seq{}, fmt.Errorf("%w: a place in the outbox is lower-case hexadecimal", domain.ErrInvalidArgument)
		}
	}
	key, err := hex.DecodeString(text)
	if err != nil {
		return Seq{}, fmt.Errorf("%w: a place in the outbox is lower-case hexadecimal", domain.ErrInvalidArgument)
	}
	return SeqFromKey(key)
}

// maxActor bounds the name of whoever made a change, as bucket.ValidateCreator
// does.
const maxActor = 256

// MaxItemETag bounds the entity tag a row carries, in bytes. An entity tag as
// this deployment makes one is 32 hexadecimal characters, and a hyphen and
// the number of its parts for a version of parts; nothing holds an entry to
// that, so a row is: one that would be longer is appended without it
// (ADR-0023, "The bound"). The bound is part of the replicated format.
const MaxItemETag = 128

// Item is one row of the outbox: a change of one version of one object, and
// the consumers it is for. It holds what a receiver is told and what the
// replicator needs to find the version - never anything of the object's
// contents, its metadata or its tags.
type Item struct {
	// Seq is the place of the row.
	Seq Seq
	// Event is the kind of change.
	Event Event
	// Bucket and Key name the object.
	Bucket string
	Key    string
	// Version names the version that was made, changed or removed.
	Version object.VersionRef
	// Size and ETag are those of the version; zero and empty for a delete
	// marker.
	Size int64
	ETag string
	// At is the time of the entry that made the change, which is the
	// leader's.
	At time.Time
	// By is who made the change, as the audit trail names actors; empty
	// when that was not recorded.
	By string
	// To are the consumers the row is for, each once.
	To []Consumer
}

// ID returns the ID of the message the item becomes: the same every time it
// is sent, so that a receiver that takes it twice can tell.
func (i Item) ID() string {
	return i.Seq.String()
}

// For reports whether the item is for a consumer of the target named, and of
// which kind. A target is of one kind, so an item is for it once at most.
func (i Item) For(target string) (ConsumerKind, bool) {
	for _, consumer := range i.To {
		if consumer.Target == target {
			return consumer.Kind, true
		}
	}
	return "", false
}

// Clone returns a copy that shares nothing with the item.
func (i Item) Clone() Item {
	i.To = slices.Clone(i.To)
	return i
}

// Validate reports whether the item is a row that can be kept. The error
// wraps domain.ErrInvalidArgument and names no key.
func (i Item) Validate() error {
	invalid := func(reason string) error {
		return fmt.Errorf("%w: outbox item: %s", domain.ErrInvalidArgument, reason)
	}
	switch {
	case !i.Event.Valid():
		return invalid("no such kind of change")
	case i.Bucket == "" || object.ValidateKey(i.Key) != nil:
		return invalid("a bucket and a key")
	case !i.Version.Null && i.Version.ID.IsZero():
		return invalid("a version")
	case i.Size < 0:
		return invalid("a size below nothing")
	case len(i.ETag) > MaxItemETag:
		return invalid("an entity tag is too long")
	case len(i.By) > maxActor:
		return invalid("the name of an actor is too long")
	case len(i.To) == 0:
		return invalid("a row is for somebody")
	}
	for n, consumer := range i.To {
		if _, err := ParseConsumer(consumer.String()); err != nil {
			return err
		}
		// A target is of one kind: it is a consumer of a row once.
		for _, earlier := range i.To[:n] {
			if earlier.Target == consumer.Target {
				return invalid("a target is a consumer of a row once")
			}
		}
	}
	return nil
}

// Cursor is how far delivery to one target has come. It is made with the
// target and goes with it: a target defined under a name that was used
// before is sent nothing of before.
type Cursor struct {
	// Target names the target.
	Target string
	// From is the place the cursor began at: that of the entry that
	// defined the target. No row at or before it is the target's.
	From Seq
	// Acked is the last place the target was delivered to, or passed.
	// It never goes back.
	Acked Seq
	// AuditAcked is the sequence of the last record of the audit trail
	// the target was sent (ADR-0025). It never goes back.
	AuditAcked uint64
	// Lost counts the rows for the target that were dropped at the bound
	// of the outbox before they were sent.
	Lost uint64
	// ResyncNeeded says that rows for replication were among them: the
	// other deployment lacks versions until a resync has been through.
	ResyncNeeded bool
}

// Pending reports whether the target has yet to be delivered the row at seq.
func (c Cursor) Pending(seq Seq) bool {
	return seq.After(c.Acked)
}

// Advanced returns the cursor after an acknowledgement up to upto, and of
// the audit trail up to auditUpto. Neither goes back: an acknowledgement
// that is behind the cursor - one of a leader that was replaced, arriving
// late - changes nothing.
func (c Cursor) Advanced(upto Seq, auditUpto uint64) Cursor {
	if upto.After(c.Acked) {
		c.Acked = upto
	}
	c.AuditAcked = max(c.AuditAcked, auditUpto)
	return c
}

// Dropped returns the cursor after item was dropped at the bound of the
// outbox. A row the target had not passed and is a consumer of is lost to
// it, and counted; one for replication leaves the other deployment in need
// of a resync. Anything else changes nothing.
//
// It is part of the replicated format, like Advanced.
func (c Cursor) Dropped(item Item) Cursor {
	kind, ok := item.For(c.Target)
	if !ok || !c.Pending(item.Seq) {
		return c
	}
	c.Lost++
	if kind == ConsumerReplication {
		c.ResyncNeeded = true
	}
	return c
}

// Result is what became of one version that was to be replicated, as an
// acknowledgement reports it. It names the version by the row that told of
// it, or - for a version a resync sent, or one whose row was dropped - by
// itself.
type Result struct {
	// Seq, when not zero, is the row.
	Seq Seq
	// Bucket, Key and Version name the version when Seq is zero.
	Bucket  string
	Key     string
	Version object.VersionRef
	// Status is object.ReplicationCompleted or object.ReplicationFailed.
	Status object.ReplicationStatus
}

// ByRow reports whether the result names a row and not a version.
func (r Result) ByRow() bool {
	return !r.Seq.IsZero()
}

// Validate reports whether the result says something: it names a row or a
// version, never both, and the status is one an acknowledgement can give.
// The error wraps domain.ErrInvalidArgument.
func (r Result) Validate() error {
	invalid := func(reason string) error {
		return fmt.Errorf("%w: result of a delivery: %s", domain.ErrInvalidArgument, reason)
	}
	named := r.Bucket != "" || r.Key != "" || r.Version != (object.VersionRef{})
	switch {
	case r.Status != object.ReplicationCompleted && r.Status != object.ReplicationFailed:
		return invalid("the status is COMPLETED or FAILED")
	case r.ByRow() && named:
		return invalid("a row or a version, not both")
	case r.ByRow():
		return nil
	case r.Bucket == "" || object.ValidateKey(r.Key) != nil || (!r.Version.Null && r.Version.ID.IsZero()):
		return invalid("a row, or the bucket, the key and the version")
	}
	return nil
}
