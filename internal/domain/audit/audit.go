// Package audit holds the audit trail: what a record says, and the hash
// chain that makes the trail tamper-evident.
//
// Every record carries the hash of the one before it. Whoever holds the
// latest hash can tell whether any earlier record was changed, removed or
// inserted: the chain would no longer lead to that hash. Records are only
// ever appended; nothing in the product removes one.
package audit

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// Outcome is how the audited action ended.
type Outcome string

// The outcomes.
const (
	// Succeeded: the action was carried out.
	Succeeded Outcome = "success"
	// Denied: the authorizer refused it.
	Denied Outcome = "denied"
	// Failed: it was not carried out for another reason - for an
	// authentication attempt, that the credentials were not accepted.
	Failed Outcome = "failed"
)

// Limits on the free-form parts of a record.
const (
	maxFieldLength = 1024
	maxDetails     = 16
)

// Record is one entry of the audit trail. It never holds a secret, a key, a
// token or object contents.
type Record struct {
	// Sequence numbers the records from 1 without gaps.
	Sequence uint64
	// Time is when the action happened, by the clock of the node that
	// handled it.
	Time time.Time
	// Actor is who acted: "root", "user/<name>", "node/<id>", an access key
	// ID that could not be resolved, or "anonymous".
	Actor string
	// AccessKeyID is the key the request was made with, when there was one.
	AccessKeyID string
	// Action is what was attempted, such as "admin:PutUser" or
	// "auth:SignIn".
	Action string
	// Resource is what it was attempted on, in ARN form.
	Resource string
	Outcome  Outcome
	// SourceIP and RequestID tie the record to a request.
	SourceIP  string
	RequestID string
	// Details add what the action needs to be understood: the reason of a
	// refusal, how often it was repeated.
	Details map[string]string
	// PreviousHash is the Hash of the record before this one; empty for the
	// first. Hash is the hash of this record, PreviousHash included.
	PreviousHash string
	Hash         string
}

// Validate reports whether a record that is about to be appended is well
// formed. The error wraps domain.ErrInvalidArgument.
func (r Record) Validate() error {
	if r.Actor == "" || r.Action == "" {
		return fmt.Errorf("%w: an audit record needs an actor and an action", domain.ErrInvalidArgument)
	}
	if r.Outcome != Succeeded && r.Outcome != Denied && r.Outcome != Failed {
		return fmt.Errorf("%w: audit outcome %q", domain.ErrInvalidArgument, r.Outcome)
	}
	if len(r.Details) > maxDetails {
		return fmt.Errorf("%w: more than %d audit details", domain.ErrInvalidArgument, maxDetails)
	}
	fields := []string{r.Actor, r.AccessKeyID, r.Action, r.Resource, r.SourceIP, r.RequestID}
	for key, value := range r.Details {
		fields = append(fields, key, value)
	}
	for _, field := range fields {
		if len(field) > maxFieldLength {
			return fmt.Errorf("%w: audit field longer than %d bytes", domain.ErrInvalidArgument, maxFieldLength)
		}
	}
	return nil
}

// Seal returns the record as it is appended after previous: numbered, linked
// to it and hashed. previous is the zero Record for the first record.
func (r Record) Seal(previous Record) Record {
	r.Sequence = previous.Sequence + 1
	r.PreviousHash = previous.Hash
	r.Hash = r.digest()
	return r
}

// digest hashes every field but Hash. Each field is written with its length
// in front, so that no two different records produce the same input.
func (r Record) digest() string {
	h := sha256.New()
	write := func(s string) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(s)))
		h.Write(length[:])
		h.Write([]byte(s))
	}
	var sequence [8]byte
	binary.BigEndian.PutUint64(sequence[:], r.Sequence)
	h.Write(sequence[:])
	write(r.Time.UTC().Format(time.RFC3339Nano))
	for _, field := range []string{
		r.Actor, r.AccessKeyID, r.Action, r.Resource, string(r.Outcome), r.SourceIP, r.RequestID, r.PreviousHash,
	} {
		write(field)
	}
	keys := make([]string, 0, len(r.Details))
	for key := range r.Details {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(keys)))
	h.Write(count[:])
	for _, key := range keys {
		write(key)
		write(r.Details[key])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Verify checks a run of consecutive records: each must be numbered one
// after, and carry the hash of, the one before, and its own hash must be
// right. before is the record that precedes the run, or the zero Record when
// the run starts the trail. The error wraps domain.ErrIntegrity and names
// the first record that is wrong.
func Verify(before Record, records []Record) error {
	previous := before
	for _, r := range records {
		switch {
		case r.Sequence != previous.Sequence+1:
			return fmt.Errorf("%w: audit record %d follows record %d: one is missing or inserted",
				domain.ErrIntegrity, r.Sequence, previous.Sequence)
		case r.PreviousHash != previous.Hash:
			return fmt.Errorf("%w: audit record %d does not continue the chain", domain.ErrIntegrity, r.Sequence)
		case r.Hash != r.digest():
			return fmt.Errorf("%w: audit record %d has been altered", domain.ErrIntegrity, r.Sequence)
		}
		previous = r
	}
	return nil
}
