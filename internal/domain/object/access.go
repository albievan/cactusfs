package object

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// MaxReaders is how many readers of a version are kept by name. Beyond it
// the reader who read longest ago is counted among "others".
//
// It is part of the replicated format: the state machine of every member
// folds by it, so changing it needs a rise of the entry schema.
const MaxReaders = 32

// MaxReadCount bounds each count of one Read: the downloads, and the copies,
// that one reader made of one version between two batches. No node counts
// that many in a flush interval; the bound is there so that a count that is
// wrong - or forged - cannot wrap a total around. Part of the replicated
// format, like MaxReaders.
const MaxReadCount = 1 << 32

// maxReaderName bounds the name of a reader, in bytes. Part of the
// replicated format, like MaxReaders.
const maxReaderName = 256

// Reader is one reader of a version and how often they read it.
type Reader struct {
	// Who is the reader, as the audit trail names actors: "root",
	// "user/<name>", "key/<id>", or "anonymous".
	Who string
	// Presigned says that these reads were made with URLs this reader
	// signed: whoever held the URL made them, not necessarily the signer.
	Presigned bool
	// Downloads is how often the version was read.
	Downloads int64
	// Copies is how often it was the source of a copy.
	Copies int64
	// LastAt is when the last of either was recorded.
	LastAt time.Time
}

// Access is what is known of the reading of one version: how often, by
// whom, when last. It is a count, kept as well as can be without making a
// read wait for it (ADR-0016) - not a trail of evidence.
type Access struct {
	// Downloads and Copies are totals, readers counted among others
	// included.
	Downloads int64
	Copies    int64
	// LastAt is when the last read of either kind was recorded.
	LastAt time.Time
	// Readers are the readers kept by name, ordered by name; those who
	// read with presigned URLs after those who did not.
	Readers []Reader
	// OtherReaders is how many readers were folded into "others", and
	// OtherDownloads and OtherCopies what they read. A reader who returns
	// after being folded is counted again.
	OtherReaders   int64
	OtherDownloads int64
	OtherCopies    int64
}

// Read is a number of reads of a version by one reader.
type Read struct {
	Who       string
	Presigned bool
	Downloads int64
	Copies    int64
}

// Validate reports whether the read can be recorded: a reader with a name
// of 1 to 256 bytes on one line, and at least one download or copy, neither
// count below zero nor above MaxReadCount. The error wraps
// domain.ErrInvalidArgument.
//
// It is part of the replicated format: the state machine of every member
// leaves out the reads it refuses, and looks at bytes and numbers only so
// that every build decides alike. Changing what it accepts needs a rise of
// the entry schema.
func (r Read) Validate() error {
	switch {
	case r.Who == "" || len(r.Who) > maxReaderName || strings.ContainsAny(r.Who, "\x00\n\r"):
		return fmt.Errorf("%w: a reader has a name of 1 to %d bytes", domain.ErrInvalidArgument, maxReaderName)
	case r.Downloads < 0 || r.Copies < 0 || (r.Downloads == 0 && r.Copies == 0):
		return fmt.Errorf("%w: a read counts at least one download or copy", domain.ErrInvalidArgument)
	case r.Downloads > MaxReadCount || r.Copies > MaxReadCount:
		return fmt.Errorf("%w: a read counts at most %d downloads and as many copies", domain.ErrInvalidArgument, int64(MaxReadCount))
	}
	return nil
}

// addCount adds to a count and stops at the largest there is: a count never
// wraps around to below zero.
func addCount(count, more int64) int64 {
	if more > 0 && count > math.MaxInt64-more {
		return math.MaxInt64
	}
	return count + more
}

func readerOrder(a, b Reader) int {
	if c := strings.Compare(a.Who, b.Who); c != 0 {
		return c
	}
	switch {
	case a.Presigned == b.Presigned:
		return 0
	case b.Presigned:
		return -1
	}
	return 1
}

// Add returns the record with the reads added, as of the time given. The
// result depends on nothing but the record, the reads in their order and the
// time: every member of a cluster that applies the same reads has the same
// record. A read that cannot be recorded is left out. A count that has
// reached the largest number there is stays there.
//
// It is part of the replicated format, with everything it decides by -
// Read.Validate, the order of readers, MaxReaders and which reader is folded
// first: a member that added differently would hold another record. Changing
// any of it needs a rise of the entry schema.
func (a Access) Add(reads []Read, at time.Time) Access {
	out := a
	out.Readers = slices.Clone(a.Readers)
	for _, read := range reads {
		if read.Validate() != nil {
			continue
		}
		out.Downloads = addCount(out.Downloads, read.Downloads)
		out.Copies = addCount(out.Copies, read.Copies)
		if at.After(out.LastAt) {
			out.LastAt = at
		}
		entry := Reader{Who: read.Who, Presigned: read.Presigned}
		i, found := slices.BinarySearchFunc(out.Readers, entry, readerOrder)
		if !found {
			out.Readers = slices.Insert(out.Readers, i, entry)
		}
		out.Readers[i].Downloads = addCount(out.Readers[i].Downloads, read.Downloads)
		out.Readers[i].Copies = addCount(out.Readers[i].Copies, read.Copies)
		if at.After(out.Readers[i].LastAt) {
			out.Readers[i].LastAt = at
		}
		for len(out.Readers) > MaxReaders {
			out.foldOldest()
		}
	}
	return out
}

// foldOldest counts the reader who read longest ago among the others. Of
// two who read at the same time, the later in order goes.
//
// It is part of the replicated format: which reader goes decides what every
// member stores, so changing the rule needs a rise of the entry schema.
func (a *Access) foldOldest() {
	oldest := 0
	for i, r := range a.Readers {
		if !r.LastAt.After(a.Readers[oldest].LastAt) {
			oldest = i
		}
	}
	gone := a.Readers[oldest]
	a.OtherReaders = addCount(a.OtherReaders, 1)
	a.OtherDownloads = addCount(a.OtherDownloads, gone.Downloads)
	a.OtherCopies = addCount(a.OtherCopies, gone.Copies)
	a.Readers = slices.Delete(a.Readers, oldest, oldest+1)
}

// Empty reports whether nothing was ever read.
func (a Access) Empty() bool {
	return a.Downloads == 0 && a.Copies == 0
}
