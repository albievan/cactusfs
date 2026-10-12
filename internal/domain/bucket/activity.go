package bucket

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// MaxRecentUploads and MaxRecentDownloads are how many uploads and how many
// downloads of a bucket are kept: the latest, and the two before it.
//
// They are part of the replicated format: the state machine of every member
// cuts the record by them, so changing either needs a rise of the entry
// schema.
const (
	MaxRecentUploads   = 3
	MaxRecentDownloads = 3
)

// maxActorName bounds the name of whoever created a bucket, uploaded or
// downloaded, in bytes: what object.Read allows a reader. Part of the
// replicated format, like MaxRecentUploads.
const maxActorName = 256

// RecentUpload is one upload into a bucket: a version committed by a put, a
// copy or the completion of a multipart upload.
type RecentUpload struct {
	// At is when the version was committed.
	At time.Time
	// By is who committed it, as the audit trail names actors; empty when
	// that was not recorded.
	By string
	// Key is the key of the object. It may have been deleted since.
	Key string
}

// RecentDownload is one reader's latest download of one object of a bucket.
type RecentDownload struct {
	// At is when the download was recorded: when the node that served it
	// handed its count over, not when the bytes left (ADR-0016).
	At time.Time
	// By is the reader, as object.Reader names one: an actor, or
	// "anonymous".
	By string
	// Presigned says that the download was made with a URL By signed:
	// whoever held the URL made it, not necessarily the signer.
	Presigned bool
	// Key is the key of the object. It may have been deleted since.
	Key string
}

// Activity is what is known of the latest use of a bucket (ADR-0019): its
// most recent uploads and downloads, newest first. Like object.Access it is
// kept as well as can be without making a read wait for it - not a trail of
// evidence. The zero value is a bucket of which nothing is known.
type Activity struct {
	// Uploads are the latest uploads, newest first, at most
	// MaxRecentUploads and one a key.
	Uploads []RecentUpload
	// Downloads are the latest downloads, newest first, at most
	// MaxRecentDownloads and one for a key, a reader and whether the reader
	// came with a presigned URL.
	Downloads []RecentDownload
}

// ValidateCreator reports whether by can be recorded as who created a
// bucket, uploaded or downloaded: at most 256 bytes on one line. The empty
// name is valid and says that nobody was recorded. The error wraps
// domain.ErrInvalidArgument.
//
// It is part of the replicated format: the state machine of every member
// refuses, or leaves out, what it refuses, and it looks at bytes only so that
// every build decides alike. Changing what it accepts needs a rise of the
// entry schema.
func ValidateCreator(by string) error {
	if len(by) > maxActorName || strings.ContainsAny(by, "\x00\n\r") {
		return fmt.Errorf("%w: an actor has a name of at most %d bytes on one line", domain.ErrInvalidArgument, maxActorName)
	}
	return nil
}

// keyFits reports whether a row may name key: a key of 1 to
// object.MaxKeyLength bytes. Only the length is asked, which no build decides
// differently; what a key may consist of is for whoever stores the object.
func keyFits(key string) bool {
	return key != "" && len(key) <= object.MaxKeyLength
}

// placed returns rows with row among them: before every row that is not
// newer than it, and without the row that same names, which row replaces. A
// row older than the one it would replace changes nothing. Of what is beyond
// limit the last go, whatever comes of row. rows is not changed, nor shared
// with the result.
func placed[T any](rows []T, row T, limit int, at func(T) time.Time, same func(T) bool) []T {
	out := make([]T, 0, len(rows)+1)
	for _, existing := range rows {
		if same(existing) {
			if at(existing).After(at(row)) {
				return slices.Clone(rows[:min(len(rows), limit)])
			}
			continue
		}
		out = append(out, existing)
	}
	i := 0
	for i < len(out) && at(out[i]).After(at(row)) {
		i++
	}
	out = slices.Insert(out, i, row)
	return slices.Clip(out[:min(len(out), limit)])
}

// Uploaded returns the record after an upload of key by by, as of the time
// given. The key has one row: a later upload of it takes the row's place, and
// an upload older than the row changes nothing. Of two uploads of the same
// time the one recorded later comes first. An upload that cannot be recorded
// - a key that is none, a name ValidateCreator refuses - is left out.
//
// The result depends on nothing but the record and what is given: every
// member of a cluster that applies the same uploads has the same record. The
// record it is called on is not changed.
//
// It is part of the replicated format, with everything it decides by:
// changing any of it needs a rise of the entry schema.
func (a Activity) Uploaded(at time.Time, by, key string) Activity {
	out := a.Clone()
	if !keyFits(key) || ValidateCreator(by) != nil {
		return out
	}
	out.Uploads = placed(a.Uploads, RecentUpload{At: at, By: by, Key: key}, MaxRecentUploads,
		func(u RecentUpload) time.Time { return u.At },
		func(u RecentUpload) bool { return u.Key == key })
	return out
}

// Downloaded returns the record after a download of key by the reader by,
// with a URL the reader signed when presigned is set, as of the time given.
// A key, a reader and whether the URL was presigned have one row together,
// as a reader has in object.Access; the rest is as Uploaded does it. A
// download by nobody is left out, as object.Read refuses a reader without a
// name.
//
// It is part of the replicated format, like Uploaded.
func (a Activity) Downloaded(at time.Time, by string, presigned bool, key string) Activity {
	out := a.Clone()
	if !keyFits(key) || by == "" || ValidateCreator(by) != nil {
		return out
	}
	out.Downloads = placed(a.Downloads, RecentDownload{At: at, By: by, Presigned: presigned, Key: key}, MaxRecentDownloads,
		func(d RecentDownload) time.Time { return d.At },
		func(d RecentDownload) bool { return d.Key == key && d.By == by && d.Presigned == presigned })
	return out
}

// Clone returns a copy that shares nothing with the record.
func (a Activity) Clone() Activity {
	return Activity{Uploads: slices.Clone(a.Uploads), Downloads: slices.Clone(a.Downloads)}
}

// Empty reports whether nothing is known of the bucket's use.
func (a Activity) Empty() bool {
	return len(a.Uploads) == 0 && len(a.Downloads) == 0
}
