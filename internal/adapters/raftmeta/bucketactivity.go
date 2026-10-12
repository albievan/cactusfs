package raftmeta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// The latest uploads into and downloads from every bucket, in the state
// (ADR-0019).
//
//	bucketactivity   bucket name   -> activityRecord
//
// Nothing proposes this record. Every member keeps it by itself while it
// applies the entries that commit a version (object.put, upload.complete) and
// those that hand in reads (object.reads.add) - and only those among them
// that were written as schema 4. A snapshot is the state file, so what is
// derived from the log must be derived by every member alike, from the log
// alone: the schema byte of an entry is in the log, the level of the cluster
// at the moment a member happens to apply the entry is not. A member of the
// release before never applies an entry of schema 4, and so never has to keep
// what it does not know of.
//
// The time of a row is the time of its entry (ADR-0008), never a clock's.
//
// The bucket does not exist in a state until an entry of schema 4 writes to
// it: a state that was never written with schema 4 is byte for byte what the
// release before wrote. A record goes with its bucket (applyDeleteBucket),
// and with nothing else: a row outlives the object it names.

// activityRecordVersion is the version of a stored activityRecord.
const activityRecordVersion = 1

func bucketActivityBucket() []byte { return []byte("bucketactivity") }

// activityRecord is a bucket.Activity as it is stored. V is the version of
// the record's own format. The order of the fields, and of the rows, is the
// order they are stored in: every member stores the same bytes.
type activityRecord struct {
	V         int           `json:"v"`
	Uploads   []uploadRow   `json:"uploads,omitempty"`
	Downloads []downloadRow `json:"downloads,omitempty"`
}

type uploadRow struct {
	At  time.Time `json:"at"`
	By  string    `json:"by"`
	Key string    `json:"key"`
}

type downloadRow struct {
	At        time.Time `json:"at"`
	By        string    `json:"by"`
	Presigned bool      `json:"presigned,omitempty"`
	Key       string    `json:"key"`
}

func activityToRecord(a bucket.Activity) activityRecord {
	record := activityRecord{V: activityRecordVersion}
	for _, u := range a.Uploads {
		record.Uploads = append(record.Uploads, uploadRow{At: u.At.UTC(), By: u.By, Key: u.Key})
	}
	for _, d := range a.Downloads {
		record.Downloads = append(record.Downloads, downloadRow{At: d.At.UTC(), By: d.By, Presigned: d.Presigned, Key: d.Key})
	}
	return record
}

func (r activityRecord) toDomain() bucket.Activity {
	var a bucket.Activity
	for _, u := range r.Uploads {
		a.Uploads = append(a.Uploads, bucket.RecentUpload{At: u.At, By: u.By, Key: u.Key})
	}
	for _, d := range r.Downloads {
		a.Downloads = append(a.Downloads, bucket.RecentDownload{At: d.At, By: d.By, Presigned: d.Presigned, Key: d.Key})
	}
	return a
}

// readBucketActivity returns the record of a bucket and the bytes it is
// stored in; the zero record and nil when there is none. A record that cannot
// be read is an error, as a damaged record of reads is (readAccess): a member
// that went on with a record of its own making would hold other bytes than
// the others and say nothing.
func readBucketActivity(records *bbolt.Bucket, name string) (bucket.Activity, []byte, error) {
	if records == nil {
		return bucket.Activity{}, nil, nil
	}
	raw := records.Get([]byte(name))
	if raw == nil {
		return bucket.Activity{}, nil, nil
	}
	a, err := decodeBucketActivity(name, raw)
	return a, raw, err
}

// decodeBucketActivity reads a stored record. The error wraps
// domain.ErrIntegrity for bytes that are no record, and
// domain.ErrUnsupportedFormat for a record of a format this binary does not
// know.
func decodeBucketActivity(name string, raw []byte) (bucket.Activity, error) {
	var record activityRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return bucket.Activity{}, fmt.Errorf("%w: stored activity of bucket %s: %w", domain.ErrIntegrity, name, err)
	}
	if record.V != activityRecordVersion {
		return bucket.Activity{}, fmt.Errorf("%w: stored activity of bucket %s, of version %d", domain.ErrUnsupportedFormat, name, record.V)
	}
	return record.toDomain(), nil
}

// encodeBucketActivity writes a record as it is stored.
func encodeBucketActivity(name string, a bucket.Activity) ([]byte, error) {
	data, err := json.Marshal(activityToRecord(a))
	if err != nil {
		// Only a time no entry has can fail to be written: one that came
		// out of a stored record that was not this binary's.
		return nil, fmt.Errorf("%w: activity of bucket %s: %w", domain.ErrIntegrity, name, err)
	}
	return data, nil
}

// changeBucketActivity replaces the record of a bucket by what change makes
// of it. A record that would be empty, or the one that is stored already, is
// not written: the bucket of records exists only once it holds one.
func changeBucketActivity(tx *bbolt.Tx, name string, change func(bucket.Activity) bucket.Activity) error {
	records, err := lazyBucket(tx, bucketActivityBucket(), false)
	if err != nil {
		return err
	}
	current, raw, err := readBucketActivity(records, name)
	if err != nil {
		return err
	}
	next := change(current)
	if next.Empty() {
		return nil
	}
	data, err := encodeBucketActivity(name, next)
	if err != nil {
		return err
	}
	if bytes.Equal(raw, data) {
		return nil
	}
	if records, err = lazyBucket(tx, bucketActivityBucket(), true); err != nil {
		return err
	}
	return records.Put([]byte(name), data)
}

// noteUpload records that the entry being applied committed v: an upload
// into its bucket by whoever made the version, at the time of the entry. It
// does so for an entry of schema 4 only - see the top of this file. The cost
// is one read and one write of a small record for a version committed.
func noteUpload(tx *bbolt.Tx, v versionRecord, now applied) error {
	if now.schema < entrySchema4 {
		return nil
	}
	return changeBucketActivity(tx, v.Bucket, func(a bucket.Activity) bucket.Activity {
		return a.Uploaded(now.at, v.CreatedBy, v.Key)
	})
}

// noteDownloads records the downloads of a batch of reads, in the order of
// the batch and at the time of its entry, each as a download from its bucket.
// It does so for an entry of schema 4 only.
//
// An item counts when it is a read that can be recorded (object.Read), says
// that something was downloaded - the source of a copy was not - and names a
// bucket there is. Whether the version it read is still there is not asked:
// bytes left the bucket, and the row would outlive the object anyway. (The
// record of the version itself, in "access", is the version's or nobody's.)
func noteDownloads(tx *bbolt.Tx, items []readItem, now applied) error {
	if now.schema < entrySchema4 {
		return nil
	}
	// One read and one write a bucket, however many of the batch's items are
	// its: the buckets in the order the batch first names them.
	var names []string
	changed := map[string]bucket.Activity{}
	for _, item := range items {
		read := object.Read{Who: item.Who, Presigned: item.Presigned, Downloads: item.Downloads, Copies: item.Copies}
		if read.Validate() != nil || item.Downloads <= 0 || tx.Bucket(objectsBucket()).Bucket([]byte(item.Bucket)) == nil {
			continue
		}
		a, seen := changed[item.Bucket]
		if !seen {
			records, err := lazyBucket(tx, bucketActivityBucket(), false)
			if err != nil {
				return err
			}
			if a, _, err = readBucketActivity(records, item.Bucket); err != nil {
				return err
			}
			names = append(names, item.Bucket)
		}
		changed[item.Bucket] = a.Downloaded(now.at, item.Who, item.Presigned, item.Key)
	}
	for _, name := range names {
		next := changed[name]
		if err := changeBucketActivity(tx, name, func(bucket.Activity) bucket.Activity { return next }); err != nil {
			return err
		}
	}
	return nil
}

// dropBucketActivity removes the record of a bucket that is being deleted.
func dropBucketActivity(tx *bbolt.Tx, name string) error {
	records, err := lazyBucket(tx, bucketActivityBucket(), false)
	if err != nil || records == nil {
		return err
	}
	return records.Delete([]byte(name))
}

var _ ports.BucketActivityStore = (*Store)(nil)

// BucketActivities implements ports.BucketActivityStore. The records are
// read in one transaction, each by a point read: no walk, whatever the
// buckets hold.
func (s *Store) BucketActivities(ctx context.Context, names []string) ([]bucket.Activity, error) {
	if len(names) > ports.MaxBucketActivities {
		return nil, fmt.Errorf("raftmeta.BucketActivities: %w: %d buckets, at most %d", domain.ErrInvalidArgument, len(names), ports.MaxBucketActivities)
	}
	out := make([]bucket.Activity, len(names))
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		records, err := lazyBucket(tx, bucketActivityBucket(), false)
		if err != nil {
			return err
		}
		for i, name := range names {
			if out[i], _, err = readBucketActivity(records, name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.BucketActivities: %w", err)
	}
	return out, nil
}
