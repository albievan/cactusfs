package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// LineStream is the value of "stream" in every exported line.
const LineStream = "audit"

// line is a record as it is exported: one JSON object, the fields in this
// order. It is the body of the message cactusfs.audit.exported.v1 (ADR-0025)
// and a line of the node's own output, and is defined once, here, so that
// both are the same bytes.
type line struct {
	Stream       string            `json:"stream"`
	Sequence     uint64            `json:"sequence"`
	Time         time.Time         `json:"time"`
	Actor        string            `json:"actor"`
	AccessKeyID  string            `json:"access_key_id,omitempty"`
	Action       string            `json:"action"`
	Resource     string            `json:"resource,omitempty"`
	Outcome      string            `json:"outcome"`
	SourceIP     string            `json:"source_ip,omitempty"`
	RequestID    string            `json:"request_id,omitempty"`
	Details      map[string]string `json:"details,omitempty"`
	PreviousHash string            `json:"previous_hash,omitempty"`
	Hash         string            `json:"hash"`
}

// lineField reports whether name is that of a field of a line.
func lineField(name string) bool {
	switch name {
	case "stream", "sequence", "time", "actor", "access_key_id", "action", "resource", "outcome", "source_ip", "request_id",
		"details", "previous_hash", "hash":
		return true
	}
	return false
}

// Line returns the record as it is exported: compact JSON without a line
// break, every field of the record, the hashes included, so that whoever
// holds every line can verify the chain (ParseLine, Verify).
func (r Record) Line() ([]byte, error) {
	data, err := json.Marshal(line{
		Stream: LineStream, Sequence: r.Sequence, Time: r.Time, Actor: r.Actor, AccessKeyID: r.AccessKeyID, Action: r.Action,
		Resource: r.Resource, Outcome: string(r.Outcome), SourceIP: r.SourceIP, RequestID: r.RequestID, Details: r.Details,
		PreviousHash: r.PreviousHash, Hash: r.Hash,
	})
	if err != nil {
		return nil, fmt.Errorf("audit.Line: %w", err)
	}
	return data, nil
}

// ParseLine reads an exported line back into the record it was made from:
// the reverse of Record.Line, for a receiver that verifies the chain. It is
// strict - one JSON object of the fields of a line and no other, no name
// twice (in the details neither), "stream" "audit", a sequence from 1, a
// time, an actor, an action, an outcome of the three, a hash - and the error
// wraps domain.ErrInvalidArgument. It does not judge the hash: Verify does.
func ParseLine(data []byte) (Record, error) {
	fields, err := objectFields(data)
	if err != nil {
		return Record{}, fmt.Errorf("audit.ParseLine: %w: an audit line that is no JSON object of its fields: %s", domain.ErrInvalidArgument, err.Error())
	}
	// encoding/json matches names regardless of case: the names are held
	// to those of a line, exactly, before the fields are read.
	for name := range fields {
		if !lineField(name) {
			return Record{}, fmt.Errorf("audit.ParseLine: %w: an audit line with a field %q", domain.ErrInvalidArgument, name)
		}
	}
	if details, ok := fields["details"]; ok && !bytes.Equal(bytes.TrimSpace(details), []byte("null")) {
		if _, err := objectFields(details); err != nil {
			return Record{}, fmt.Errorf("audit.ParseLine: %w: the details of an audit line: %s", domain.ErrInvalidArgument, err.Error())
		}
	}
	var l line
	if err := json.Unmarshal(data, &l); err != nil {
		return Record{}, fmt.Errorf("audit.ParseLine: %w: an audit line that is not one: %s", domain.ErrInvalidArgument, err.Error())
	}
	if len(l.Details) == 0 {
		// No details and an empty object of them are one record, and hash
		// alike.
		l.Details = nil
	}
	outcome := Outcome(l.Outcome)
	switch {
	case l.Stream != LineStream:
		return Record{}, fmt.Errorf("audit.ParseLine: %w: an audit line of stream %q", domain.ErrInvalidArgument, l.Stream)
	case l.Sequence == 0:
		return Record{}, fmt.Errorf("audit.ParseLine: %w: an audit line without a sequence", domain.ErrInvalidArgument)
	case l.Time.IsZero():
		return Record{}, fmt.Errorf("audit.ParseLine: %w: an audit line without a time", domain.ErrInvalidArgument)
	case l.Actor == "" || l.Action == "":
		return Record{}, fmt.Errorf("audit.ParseLine: %w: an audit line without an actor or an action", domain.ErrInvalidArgument)
	case outcome != Succeeded && outcome != Denied && outcome != Failed:
		return Record{}, fmt.Errorf("audit.ParseLine: %w: audit outcome %q", domain.ErrInvalidArgument, l.Outcome)
	case l.Hash == "":
		return Record{}, fmt.Errorf("audit.ParseLine: %w: an audit line without a hash", domain.ErrInvalidArgument)
	}
	return Record{
		Sequence: l.Sequence, Time: l.Time, Actor: l.Actor, AccessKeyID: l.AccessKeyID, Action: l.Action, Resource: l.Resource,
		Outcome: outcome, SourceIP: l.SourceIP, RequestID: l.RequestID, Details: l.Details, PreviousHash: l.PreviousHash, Hash: l.Hash,
	}, nil
}

// errDuplicateName is a JSON object that names a field twice: encoding/json
// would keep the last, and two readers of one line could read two records.
var errDuplicateName = errors.New("a name given twice")

// objectFields reads one JSON object and nothing after it, its fields by
// name, refusing a name given twice.
func objectFields(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	open, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if open != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, errors.New("not an object")
		}
		if _, seen := fields[name]; seen {
			return nil, fmt.Errorf("%w: %q", errDuplicateName, name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more after the object")
	}
	return fields, nil
}
