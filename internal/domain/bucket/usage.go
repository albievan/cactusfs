package bucket

import (
	"bytes"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// MaxQuotaBytes is the greatest quota a bucket can have: 2^62 bytes.
const MaxQuotaBytes = 1 << 62

// ValidateQuota reports whether a bucket can be given a quota of that many
// bytes: from 1 to MaxQuotaBytes. The error wraps domain.ErrInvalidArgument.
// That there is no quota is said with 0, which is no quota and not valid as
// one.
func ValidateQuota(quotaBytes int64) error {
	if quotaBytes < 1 || quotaBytes > MaxQuotaBytes {
		return fmt.Errorf("%w: a quota is from 1 to 2^62 bytes", domain.ErrInvalidArgument)
	}
	return nil
}

// usageRecordVersion is the version of the stored record.
const usageRecordVersion = 1

// UsageTotals is what is counted of a bucket (ADR-0023). Sizes are those of
// the objects as they were sent, not of what they take on the drives.
type UsageTotals struct {
	// Objects are the current versions that are not delete markers.
	Objects int64
	// CurrentBytes are their sizes, added.
	CurrentBytes int64
	// Versions are all the versions the bucket keeps, current or not,
	// delete markers among them.
	Versions int64
	// StoredBytes are the sizes of all of them, added.
	StoredBytes int64
	// UploadBytes are the sizes of the parts of multipart uploads that
	// are neither completed nor aborted.
	UploadBytes int64
	// ReplicationPending and ReplicationFailed are the versions whose
	// replication status is PENDING and FAILED (ADR-0026).
	ReplicationPending int64
	ReplicationFailed  int64
}

// Used returns what counts against a quota: the stored versions and the
// parts of unfinished uploads together (ADR-0024).
func (t UsageTotals) Used() int64 {
	used, _ := clampedSum(t.StoredBytes, t.UploadBytes)
	return used
}

// clampedSum adds a change to a count. A count is never below nothing and
// never wraps: a change that would take it there leaves it at the bound, and
// clamped says so. Every member of a cluster clamps alike, and a recount puts
// right what was clamped.
func clampedSum(count, change int64) (sum int64, clamped bool) {
	if count < 0 {
		count, clamped = 0, true
	}
	if change > 0 && count > math.MaxInt64-change {
		return math.MaxInt64, true
	}
	// A count that is not below nothing and a change that is cannot wrap.
	if count+change < 0 {
		return 0, true
	}
	return count + change, clamped
}

// plus adds d to t, field by field, and says whether any count was held at a
// bound.
func (t UsageTotals) plus(d UsageTotals) (sum UsageTotals, clamped bool) {
	add := func(count, change int64) int64 {
		next, held := clampedSum(count, change)
		clamped = clamped || held
		return next
	}
	return UsageTotals{
		Objects:            add(t.Objects, d.Objects),
		CurrentBytes:       add(t.CurrentBytes, d.CurrentBytes),
		Versions:           add(t.Versions, d.Versions),
		StoredBytes:        add(t.StoredBytes, d.StoredBytes),
		UploadBytes:        add(t.UploadBytes, d.UploadBytes),
		ReplicationPending: add(t.ReplicationPending, d.ReplicationPending),
		ReplicationFailed:  add(t.ReplicationFailed, d.ReplicationFailed),
	}, clamped
}

func (t UsageTotals) negative() bool {
	return t.Objects < 0 || t.CurrentBytes < 0 || t.Versions < 0 || t.StoredBytes < 0 || t.UploadBytes < 0 ||
		t.ReplicationPending < 0 || t.ReplicationFailed < 0
}

// UsagePhase is how far the count of a bucket has come.
type UsagePhase int

// The phases of a count, in their order.
const (
	// UsagePhaseKeys is the walk through the keys of the bucket: every
	// version of a key at once, key by key.
	UsagePhaseKeys UsagePhase = 1
	// UsagePhaseUploads is the walk through the parts of the multipart
	// uploads in progress, upload by upload. The keys are done.
	UsagePhaseUploads UsagePhase = 2
	// UsagePhaseDone is a walk that has been through everything and is
	// waiting to be finished.
	UsagePhaseDone UsagePhase = 3
)

// UsageCount is a count of a bucket that is under way.
type UsageCount struct {
	Phase UsagePhase
	// Cursor is the last key, or in UsagePhaseUploads the last upload,
	// the walk has been to; empty when it has been to none of its phase.
	Cursor string
	// UsageTotals is what the walk has counted so far, kept right by
	// every change at or before the cursor.
	UsageTotals
}

// Usage is the record a bucket has of what it holds (ADR-0023). Every change
// of the bucket keeps it; a bucket that was there before the record is
// counted by a walk that is spread over many entries of the log, and one
// that is exact can be counted again the same way.
//
// The zero value is a bucket that has no record: nothing is known, and
// nothing is kept.
//
// Usage, and everything it decides by, is part of the replicated format:
// the state machine of every member keeps the record through these methods,
// so changing what any of them does needs a rise of the entry schema.
type Usage struct {
	// Exact says that UsageTotals is the bucket's, kept right by every
	// change. While it is false the bucket is being counted for the first
	// time and the totals are zero.
	Exact bool
	// UsageTotals is what the bucket holds, when Exact.
	UsageTotals
	// High is the greatest sequence a version of the bucket was ever
	// committed at. A version made here is given a sequence above it, so
	// that it is the newest of its key whatever sequences a source of
	// replication had reached (ADR-0026).
	High uint64
	// Count is there while a count, or a recount, is under way.
	Count *UsageCount
}

// UsageDelta is what one change of a bucket does to its usage. A change of a
// key says the key; a change of the parts of an upload says the upload; the
// completion of an upload is both.
//
// A delta is of one key and of one upload, and is added with one Add: while
// a bucket is being counted, whether a change counts is decided by where its
// key and its upload are. An entry that changes several keys, or the parts of
// several uploads, is as many deltas - the changes of one key may be summed
// into one, those of two keys may not.
type UsageDelta struct {
	// Key is the key whose versions changed, when any did. While the
	// bucket is being counted it decides whether the change is the
	// walk's to count or already counted.
	Key string
	// Upload is the upload whose parts changed, when any did: its ID as
	// the walk orders uploads.
	Upload string
	// Seq is the sequence of a version the change committed, or 0.
	Seq uint64
	// Objects, CurrentBytes, Versions, StoredBytes, ReplicationPending
	// and ReplicationFailed are the changes of the key: what it has
	// after, less what it had before.
	Objects            int64
	CurrentBytes       int64
	Versions           int64
	StoredBytes        int64
	ReplicationPending int64
	ReplicationFailed  int64
	// UploadBytes is the change of the upload's parts.
	UploadBytes int64
}

func (d UsageDelta) ofKey() UsageTotals {
	return UsageTotals{
		Objects: d.Objects, CurrentBytes: d.CurrentBytes, Versions: d.Versions, StoredBytes: d.StoredBytes,
		ReplicationPending: d.ReplicationPending, ReplicationFailed: d.ReplicationFailed,
	}
}

// Net returns what the change adds to what counts against a quota, less
// what the same change frees.
func (d UsageDelta) Net() int64 {
	switch {
	case d.StoredBytes > 0 && d.UploadBytes > math.MaxInt64-d.StoredBytes:
		return math.MaxInt64
	case d.StoredBytes < 0 && d.UploadBytes < math.MinInt64-d.StoredBytes:
		return math.MinInt64
	}
	return d.StoredBytes + d.UploadBytes
}

// counted reports whether a change of a key, and of an upload, is one a
// count under way has been past: it then keeps the count, and is otherwise
// left to the walk, which will find what the change left.
func (c UsageCount) counted(key, upload string) (ofKey, ofUpload bool) {
	switch c.Phase {
	case UsagePhaseKeys:
		return key != "" && key <= c.Cursor, false
	case UsagePhaseUploads:
		return true, upload != "" && upload <= c.Cursor
	case UsagePhaseDone:
		return true, true
	}
	return false, false
}

// Walking reports whether a count, or a recount, is under way.
func (u Usage) Walking() bool {
	return u.Count != nil
}

// clone returns a copy that shares nothing with u.
func (u Usage) clone() Usage {
	if u.Count != nil {
		count := *u.Count
		u.Count = &count
	}
	return u
}

// Add returns the record after a change of the bucket. u is not changed.
//
// An exact record takes the whole change. A count under way takes what is
// at or before its cursor - keys compared byte for byte, uploads as the walk
// orders them - and leaves the rest to the walk. High follows Seq. One Add
// is one key and one upload (UsageDelta).
//
// No count goes below nothing or wraps: a change that would take one there
// leaves it at the bound, and clamped says so. That is no change of a real
// bucket - it is a record that was wrong before, and what it holds now is
// wrong too. So a clamp begins a recount, in the record itself: the result is
// the clamped record with a count begun as Counting begins one (a count that
// was under way begins again), which every member does alike, and which the
// walk then puts right; Finish reports it as drift. clamped is for a counter
// and is not stored.
//
// The zero record stays as it is but for High: a bucket without a record has
// none kept, and the caller does not make one by adding to it.
func (u Usage) Add(d UsageDelta) (next Usage, clamped bool) {
	next = u.clone()
	next.High = max(next.High, d.Seq)
	all := d.ofKey()
	all.UploadBytes = d.UploadBytes
	if next.Exact {
		next.UsageTotals, clamped = next.plus(all)
	}
	if c := next.Count; c != nil {
		ofKey, ofUpload := c.counted(d.Key, d.Upload)
		var counted UsageTotals
		if ofKey {
			counted = d.ofKey()
		}
		if ofUpload {
			counted.UploadBytes = d.UploadBytes
		}
		var held bool
		c.UsageTotals, held = c.plus(counted)
		clamped = clamped || held
	}
	if clamped {
		next = next.Counting()
	}
	return next, clamped
}

// NextSeq returns the sequence of a version that the entry of that index
// makes in the bucket: the index, or one above High when the bucket holds a
// version at or above the index - one that came from another deployment, at
// the sequence it had there (ADR-0026). It never wraps: at the last number
// there is it stays there. object.MaxReplicaSeq keeps a replica far below it.
//
// It is part of the replicated format.
func (u Usage) NextSeq(index uint64) uint64 {
	if u.High == math.MaxUint64 {
		return math.MaxUint64
	}
	return max(index, u.High+1)
}

// Admits reports whether a change may be made to a bucket with a quota of
// quotaBytes (ADR-0024): whether it frees at least what it adds, or leaves
// what counts within the quota. A quota of 0 is none, and admits everything;
// so does a change that adds nothing, however full the bucket is - a full
// bucket can always be emptied.
func (u Usage) Admits(quotaBytes int64, d UsageDelta) bool {
	net := d.Net()
	if quotaBytes <= 0 || net <= 0 {
		return true
	}
	used := u.Used()
	return used <= quotaBytes && net <= quotaBytes-used
}

// Counting returns the record with a count begun: of a bucket that has none
// yet, or - of one that is exact - a recount, during which the totals go on
// being kept and at whose end they are replaced (Finish). A count that was
// under way is thrown away and begun again.
func (u Usage) Counting() Usage {
	next := Usage{Exact: u.Exact, High: u.High, Count: &UsageCount{Phase: UsagePhaseKeys}}
	if u.Exact {
		next.UsageTotals = u.UsageTotals
	}
	return next
}

// UsageStep is what one entry of the walk found.
type UsageStep struct {
	// Cursor is the last key the step visited - or, in
	// UsagePhaseUploads, the last upload. Empty when it visited none.
	Cursor string
	// Counted is what it counted: of keys in UsagePhaseKeys, of the parts
	// of uploads in UsagePhaseUploads.
	Counted UsageTotals
	// High is the greatest sequence among the versions it visited.
	High uint64
	// End says that nothing is left of the phase: the walk goes on with
	// the next.
	End bool
}

func invalidUsage(format string, args ...any) error {
	return fmt.Errorf("%w: bucket usage: %s", domain.ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// Step returns the record after one entry of the walk. A step moves the
// cursor forward or not at all, visits whole keys, and counts of keys in the
// first phase and of uploads in the second; with End the walk goes on to
// the next phase, with the cursor before everything of it. u is not changed.
//
// The error wraps domain.ErrInvalidArgument for a step that the walk cannot
// have made: none is under way or it has ended, the cursor goes back, the
// step counts what its phase does not, or counts without having been
// anywhere.
func (u Usage) Step(s UsageStep) (Usage, error) {
	if u.Count == nil || u.Count.Phase == UsagePhaseDone {
		return Usage{}, invalidUsage("a step of a walk that is not under way")
	}
	next := u.clone()
	c := next.Count
	uploads := UsageTotals{UploadBytes: s.Counted.UploadBytes}
	switch {
	case s.Counted.negative():
		return Usage{}, invalidUsage("a step counts nothing below nothing")
	case s.Cursor == "" && s.Counted != (UsageTotals{}):
		return Usage{}, invalidUsage("a step that counted has been somewhere")
	case s.Cursor != "" && s.Cursor <= c.Cursor:
		return Usage{}, invalidUsage("the walk does not go back")
	case len(s.Cursor) > object.MaxKeyLength || !utf8.ValidString(s.Cursor):
		return Usage{}, invalidUsage("the cursor is a key or an upload")
	case c.Phase == UsagePhaseKeys && uploads != (UsageTotals{}):
		return Usage{}, invalidUsage("the walk through the keys counts no uploads")
	case c.Phase == UsagePhaseUploads && s.Counted != uploads:
		return Usage{}, invalidUsage("the walk through the uploads counts nothing else")
	}
	// What a walk counts of a real bucket is within the bounds; a count
	// that is held at one is found as drift by the next recount.
	c.UsageTotals, _ = c.plus(s.Counted)
	next.High = max(next.High, s.High)
	if s.Cursor != "" {
		c.Cursor = s.Cursor
	}
	if s.End {
		c.Phase, c.Cursor = c.Phase+1, ""
	}
	return next, nil
}

// Finish returns the record at the end of a walk that has been through
// everything: exact, with what the walk counted. drift says that the record
// was exact before and held other totals than the walk found - which is a
// fault of whoever kept it, and is for a counter. u is not changed.
//
// The error wraps domain.ErrInvalidArgument when no walk has come to its
// end.
func (u Usage) Finish() (next Usage, drift bool, err error) {
	if u.Count == nil || u.Count.Phase != UsagePhaseDone {
		return Usage{}, false, invalidUsage("no walk has come to its end")
	}
	next = Usage{Exact: true, UsageTotals: u.Count.UsageTotals, High: u.High}
	return next, u.Exact && u.UsageTotals != next.UsageTotals, nil
}

// Validate reports whether u is a record that can be kept: exact, being
// counted, or both; nothing counted below nothing; and a count in one of its
// phases with a cursor that can be one. The zero value is no record and is
// not valid as one. The error wraps domain.ErrInvalidArgument.
func (u Usage) Validate() error {
	switch {
	case !u.Exact && u.Count == nil:
		return invalidUsage("a record is exact or being counted")
	case u.negative():
		return invalidUsage("a count below nothing")
	}
	if c := u.Count; c != nil {
		switch {
		case c.Phase < UsagePhaseKeys || c.Phase > UsagePhaseDone:
			return invalidUsage("phase %d of a walk", c.Phase)
		case c.negative():
			return invalidUsage("a count below nothing")
		case len(c.Cursor) > object.MaxKeyLength || !utf8.ValidString(c.Cursor):
			return invalidUsage("the cursor is a key or an upload")
		case c.Phase == UsagePhaseDone && c.Cursor != "":
			return invalidUsage("a walk that has ended has no cursor")
		}
	}
	return nil
}

// writeUsageTotals writes the seven counts, in their order, each after a
// comma.
func writeUsageTotals(w *canonicalWriter, t UsageTotals) {
	w.raw(`"objects":`)
	w.num(t.Objects)
	w.raw(`,"current_bytes":`)
	w.num(t.CurrentBytes)
	w.raw(`,"versions":`)
	w.num(t.Versions)
	w.raw(`,"stored_bytes":`)
	w.num(t.StoredBytes)
	w.raw(`,"upload_bytes":`)
	w.num(t.UploadBytes)
	w.raw(`,"replication_pending":`)
	w.num(t.ReplicationPending)
	w.raw(`,"replication_failed":`)
	w.num(t.ReplicationFailed)
}

func readUsageTotals(r *canonicalReader) UsageTotals {
	var t UsageTotals
	r.lit(`"objects":`)
	t.Objects = r.num()
	r.lit(`,"current_bytes":`)
	t.CurrentBytes = r.num()
	r.lit(`,"versions":`)
	t.Versions = r.num()
	r.lit(`,"stored_bytes":`)
	t.StoredBytes = r.num()
	r.lit(`,"upload_bytes":`)
	t.UploadBytes = r.num()
	r.lit(`,"replication_pending":`)
	t.ReplicationPending = r.num()
	r.lit(`,"replication_failed":`)
	t.ReplicationFailed = r.num()
	return t
}

// Record returns the record as it is stored: one JSON object in the
// canonical form (ADR-0023), its fields in a fixed order, all of them always
// written but "count", which is there while a count is under way. The same
// record is always the same bytes, whatever release of Go wrote them.
func (u Usage) Record() ([]byte, error) {
	if err := u.Validate(); err != nil {
		return nil, err
	}
	w := &canonicalWriter{}
	w.raw(`{"v":`)
	w.num(usageRecordVersion)
	w.raw(`,"exact":`)
	w.boolean(u.Exact)
	w.raw(",")
	writeUsageTotals(w, u.UsageTotals)
	w.raw(`,"high":`)
	w.unum(u.High)
	if c := u.Count; c != nil {
		w.raw(`,"count":{"phase":`)
		w.num(int64(c.Phase))
		w.raw(`,"cursor":`)
		w.str(c.Cursor)
		w.raw(",")
		writeUsageTotals(w, c.UsageTotals)
		w.raw("}")
	}
	w.raw("}")
	if w.bad {
		return nil, invalidUsage("the record cannot be written")
	}
	return w.buf, nil
}

// ParseUsage reads a stored record. It takes the bytes Record writes and no
// others: a record with a field too many, a field twice, in another order,
// with white space or with a string escaped another way is refused, so that
// what is read is what every member wrote. The error wraps
// domain.ErrInvalidArgument, or domain.ErrUnsupportedFormat for a record of
// a version this code does not know.
func ParseUsage(record []byte) (Usage, error) {
	if len(record) > MaxConfigBytes {
		return Usage{}, invalidUsage("a record of more than %d bytes", MaxConfigBytes)
	}
	r := &canonicalReader{data: record}
	r.lit(`{"v":`)
	version := r.small()
	if r.bad {
		return Usage{}, invalidUsage("the record is not as it is written")
	}
	if version != usageRecordVersion {
		return Usage{}, fmt.Errorf("%w: bucket usage record of version %d", domain.ErrUnsupportedFormat, version)
	}
	var usage Usage
	r.lit(`,"exact":`)
	usage.Exact = r.boolean()
	r.lit(",")
	usage.UsageTotals = readUsageTotals(r)
	r.lit(`,"high":`)
	usage.High = r.unum()
	if r.has(`,"count":{"phase":`) {
		count := &UsageCount{Phase: UsagePhase(r.small())}
		r.lit(`,"cursor":`)
		count.Cursor = r.str()
		r.lit(",")
		count.UsageTotals = readUsageTotals(r)
		r.lit("}")
		usage.Count = count
	}
	r.lit("}")
	if !r.end() {
		return Usage{}, invalidUsage("the record is not as it is written")
	}
	// What was read is written again: whatever the reader took that the
	// writer would not write is refused here.
	again, err := usage.Record()
	if err != nil {
		return Usage{}, err
	}
	if !bytes.Equal(again, record) {
		return Usage{}, invalidUsage("the record is not as it is written")
	}
	return usage, nil
}
