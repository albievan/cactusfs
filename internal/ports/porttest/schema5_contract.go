package porttest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// Schema5 is everything the contract of entry schema 5 exercises: the
// metadata ports, what ADR-0023 to ADR-0026 added to them - the usage of
// buckets and their quotas, targets, the outbox, replica buckets and the
// record of a resync - and the audit trail that the changes of an
// administrator write to.
type Schema5 interface {
	Metadata
	ports.BucketUsageStore
	ports.TargetStore
	ports.Outbox
	ports.ReplicationStore
	ports.AuditLog
}

// SampleTarget returns a target of the kind given that a store keeps, open
// to every bucket. Its secret is sealed bytes made for the test, which open
// with nothing.
func SampleTarget(name string, kind delivery.Kind) delivery.Target {
	t := delivery.Target{Name: name, Kind: kind, Buckets: []string{"*"}, CreatedAt: contractTime, UpdatedAt: contractTime}
	sealed := iam.SealedSecret{KMSKeyID: "local:test", WrappedKey: []byte("wrapped-" + name), Nonce: []byte("nonce-of-12b"), Ciphertext: []byte("sealed-" + name)}
	switch kind {
	case delivery.KindNATS:
		t.Endpoint, t.Subject = "tls://nats.example.com:4222", "cactusfs.events"
	case delivery.KindS3:
		t.Endpoint, t.Region = "https://s3.site-b.example", "site-b"
		t.Auth, t.Secret = delivery.Auth{Mode: delivery.AuthSigV4, ID: "AKIDSITEB"}, sealed
	default:
		t.Endpoint = "https://hooks.example.com/in/" + name
		t.Auth, t.Secret, t.SignKey = delivery.Auth{Mode: delivery.AuthBearer}, sealed, sealed
	}
	return t
}

// sourceID stands for the ID of the deployment a replica bucket takes its
// versions from, and otherSourceID for another.
const (
	sourceID      = "5d3c1f0a-site-a"
	otherSourceID = "9b8e7a66-site-c"
)

// Schema5Contract is the behaviour every store of bucket usage and quotas,
// of targets and the outbox, and of replica buckets must have. open returns
// a new, empty store in which all of it can be written: every member of its
// cluster knows entry schema 5.
//
// What happens at the bound of the outbox is OutboxBoundContract's, for
// which a store has to be opened with the bound lowered.
func Schema5Contract(t *testing.T, open func(t *testing.T) Schema5) {
	t.Helper()
	ctx := context.Background()
	noAudit := audit.Record{}
	const big = object.MinPartSize

	withBucket := func(t *testing.T, names ...string) Schema5 {
		t.Helper()
		s := open(t)
		for _, name := range names {
			if err := s.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: contractTime}); err != nil {
				t.Fatalf("CreateBucket %s: %v", name, err)
			}
		}
		return s
	}
	versioned := func(t *testing.T, s Schema5, names ...string) {
		t.Helper()
		for _, name := range names {
			if err := s.SetBucketVersioning(ctx, name, bucket.VersioningEnabled); err != nil {
				t.Fatalf("SetBucketVersioning %s: %v", name, err)
			}
		}
	}
	// sized is a version of size bytes; fill keeps the samples apart.
	sized := func(bucketName, key string, fill byte, size int64) object.Version {
		v := SampleVersion(bucketName, key, fill)
		v.Size, v.CreatedBy = size, "user/alice"
		return v
	}
	put := func(t *testing.T, s Schema5, v object.Version) object.Version {
		t.Helper()
		result, err := s.PutVersion(ctx, v)
		if err != nil {
			t.Fatalf("PutVersion %s/%s: %v", v.Bucket, v.Key, err)
		}
		return result.Stored
	}
	usage := func(t *testing.T, s Schema5, name string) ports.BucketUsage {
		t.Helper()
		got, err := s.BucketUsages(ctx, []string{name})
		if err != nil || len(got) != 1 {
			t.Fatalf("BucketUsages %s = %+v, %v", name, got, err)
		}
		return got[0]
	}
	// holds asserts that the record of a bucket is exact and holds want.
	holds := func(t *testing.T, s Schema5, name, after string, want bucket.UsageTotals) {
		t.Helper()
		got := usage(t, s, name).Usage
		if !got.Exact || got.UsageTotals != want {
			t.Fatalf("after %s the usage of %s is exact = %v, %+v; want %+v", after, name, got.Exact, got.UsageTotals, want)
		}
	}
	target := func(t *testing.T, s Schema5, name string, kind delivery.Kind, buckets ...string) {
		t.Helper()
		tg := SampleTarget(name, kind)
		if len(buckets) > 0 {
			tg.Buckets = buckets
		}
		if err := s.PutTarget(ctx, tg, noAudit); err != nil {
			t.Fatalf("PutTarget %s: %v", name, err)
		}
	}
	notify := func(t *testing.T, s Schema5, bucketName string, rules ...bucket.NotificationRule) {
		t.Helper()
		document, err := bucket.NotificationConfig{Rules: rules}.Document()
		if err != nil {
			t.Fatalf("the notification document: %v", err)
		}
		if err := s.SetBucketConfig(ctx, bucketName, bucket.ConfigNotification, document); err != nil {
			t.Fatalf("SetBucketConfig %s notification: %v", bucketName, err)
		}
	}
	replicate := func(t *testing.T, s Schema5, bucketName, targetName string, rules ...bucket.ReplicationRule) {
		t.Helper()
		document, err := bucket.ReplicationConfig{Target: targetName, Bucket: "copy-of-" + bucketName, Rules: rules}.Document()
		if err != nil {
			t.Fatalf("the replication document: %v", err)
		}
		if err := s.SetBucketConfig(ctx, bucketName, bucket.ConfigReplication, document); err != nil {
			t.Fatalf("SetBucketConfig %s replication: %v", bucketName, err)
		}
	}
	every := []delivery.Event{delivery.EventPut, delivery.EventComplete, delivery.EventMarker, delivery.EventDelete}
	rule := func(id, targetName string, on ...delivery.Event) bucket.NotificationRule {
		if len(on) == 0 {
			on = every
		}
		return bucket.NotificationRule{ID: id, As: bucket.NotifyAsQueue, Target: targetName, On: on}
	}
	// read returns everything the outbox holds for a target, a page of
	// limit rows at a time.
	read := func(t *testing.T, s Schema5, targetName string, limit int) []delivery.Item {
		t.Helper()
		var all []delivery.Item
		after := delivery.Seq{}
		for range 10000 {
			items, upto, err := s.ReadOutbox(ctx, targetName, after, limit)
			if err != nil {
				t.Fatalf("ReadOutbox %s: %v", targetName, err)
			}
			if len(items) > limit || after.After(upto) || (len(items) > 0 && items[len(items)-1].Seq.After(upto)) {
				t.Fatalf("ReadOutbox %s after %s: %d items up to %s", targetName, after, len(items), upto)
			}
			all = append(all, items...)
			if upto == after {
				return all
			}
			after = upto
		}
		t.Fatalf("ReadOutbox %s does not come to an end", targetName)
		return nil
	}
	// told puts the rows of a target in words: the kind of change and the
	// key, in order.
	told := func(items []delivery.Item) string {
		rows := make([]string, 0, len(items))
		for _, item := range items {
			rows = append(rows, string(item.Event)+" "+item.Key)
		}
		return strings.Join(rows, "; ")
	}
	cursor := func(t *testing.T, s Schema5, name string) delivery.Cursor {
		t.Helper()
		state, err := s.OutboxCursors(ctx)
		if err != nil {
			t.Fatalf("OutboxCursors: %v", err)
		}
		for _, c := range state.Cursors {
			if c.Target == name {
				return c
			}
		}
		t.Fatalf("target %s has no cursor among %+v", name, state.Cursors)
		return delivery.Cursor{}
	}
	version := func(t *testing.T, s Schema5, bucketName, key string, ref object.VersionRef) object.Version {
		t.Helper()
		v, err := s.GetVersion(ctx, bucketName, key, ref)
		if err != nil {
			t.Fatalf("GetVersion %s/%s %s: %v", bucketName, key, ref, err)
		}
		return v
	}
	// replicaOf is the copy of a version of the source: with an ID, a time
	// and a sequence that are the source's.
	replicaOf := func(bucketName, key string, fill byte, seq uint64) object.Version {
		v := sized(bucketName, key, fill, int64(fill)*10)
		v.ModifiedAt = time.Date(2025, 3, int(fill%28)+1, 8, 0, 0, 0, time.UTC)
		v.Replica = &object.Replica{From: sourceID, Seq: seq}
		return v
	}
	replicaBucket := func(t *testing.T, names ...string) Schema5 {
		t.Helper()
		s := withBucket(t, names...)
		versioned(t, s, names...)
		for _, name := range names {
			if err := s.SetBucketReplica(ctx, name, sourceID, noAudit); err != nil {
				t.Fatalf("SetBucketReplica %s: %v", name, err)
			}
		}
		return s
	}

	t.Run("a new bucket is counted from the start", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "new")
		got, err := s.BucketUsages(ctx, []string{"new", "missing", "new"})
		if err != nil || len(got) != 3 {
			t.Fatalf("BucketUsages = %+v, %v", got, err)
		}
		for _, i := range []int{0, 2} {
			if u := got[i]; !u.Usage.Exact || u.Usage.Walking() || u.Usage.UsageTotals != (bucket.UsageTotals{}) || u.QuotaBytes != 0 {
				t.Errorf("the usage of a new bucket = %+v", u)
			}
		}
		// A bucket that is not there is no error: it has no record.
		if u := got[1]; u.Usage.Exact || u.Usage.Walking() || u.Usage.UsageTotals != (bucket.UsageTotals{}) || u.QuotaBytes != 0 {
			t.Errorf("the usage of a bucket that is not there = %+v", u)
		}
		if none, err := s.BucketUsages(ctx, nil); err != nil || len(none) != 0 {
			t.Errorf("BucketUsages of no buckets = %+v, %v", none, err)
		}
		if _, err := s.BucketUsages(ctx, make([]string, ports.MaxBucketUsages+1)); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("BucketUsages of too many buckets = %v", err)
		}
		// The record goes with its bucket, and a bucket made again begins
		// again.
		put(t, s, sized("new", "k", 1, 10))
		if _, err := s.DeleteObject(ctx, "new", "k", ports.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteBucket(ctx, "new"); err != nil {
			t.Fatal(err)
		}
		if u := usage(t, s, "new"); u.Usage.Exact {
			t.Errorf("the usage of a deleted bucket = %+v", u)
		}
		if err := s.CreateBucket(ctx, bucket.Bucket{Name: "new", CreatedAt: contractTime}); err != nil {
			t.Fatal(err)
		}
		holds(t, s, "new", "being made again", bucket.UsageTotals{})
	})

	t.Run("every change keeps the usage", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "counted", "other")
		holds(t, s, "counted", "nothing", bucket.UsageTotals{})

		put(t, s, sized("counted", "k1", 1, 1000))
		holds(t, s, "counted", "a put", bucket.UsageTotals{Objects: 1, CurrentBytes: 1000, Versions: 1, StoredBytes: 1000})
		// Where versions are not kept a put replaces.
		null := put(t, s, sized("counted", "k1", 2, 2000))
		holds(t, s, "counted", "an overwrite", bucket.UsageTotals{Objects: 1, CurrentBytes: 2000, Versions: 1, StoredBytes: 2000})

		versioned(t, s, "counted")
		third := put(t, s, sized("counted", "k1", 3, 3000))
		holds(t, s, "counted", "a second version", bucket.UsageTotals{Objects: 1, CurrentBytes: 3000, Versions: 2, StoredBytes: 5000})
		put(t, s, sized("counted", "k2", 4, 4000))
		holds(t, s, "counted", "a second key", bucket.UsageTotals{Objects: 2, CurrentBytes: 7000, Versions: 3, StoredBytes: 9000})

		// A delete marker is a version of no bytes, and its key no object.
		deleted, err := s.DeleteObject(ctx, "counted", "k1", ports.DeleteOptions{MarkerID: sampleVersionID(50), At: contractTime})
		if err != nil || deleted.Marker == nil {
			t.Fatalf("DeleteObject = %+v, %v", deleted, err)
		}
		holds(t, s, "counted", "a delete marker", bucket.UsageTotals{Objects: 1, CurrentBytes: 4000, Versions: 4, StoredBytes: 9000})
		// A version that is not the current one, removed for good.
		ref := null.Ref()
		if _, err := s.DeleteObject(ctx, "counted", "k1", ports.DeleteOptions{Version: &ref}); err != nil {
			t.Fatal(err)
		}
		holds(t, s, "counted", "a noncurrent version removed", bucket.UsageTotals{Objects: 1, CurrentBytes: 4000, Versions: 3, StoredBytes: 7000})
		// The marker removed: what it hid is the object again.
		ref = deleted.Marker.Ref()
		if _, err := s.DeleteObject(ctx, "counted", "k1", ports.DeleteOptions{Version: &ref}); err != nil {
			t.Fatal(err)
		}
		holds(t, s, "counted", "the marker removed", bucket.UsageTotals{Objects: 2, CurrentBytes: 7000, Versions: 2, StoredBytes: 7000})
		// Deleting what is not there, by version, changes nothing.
		ref = object.VersionRef{ID: sampleVersionID(99)}
		if _, err := s.DeleteObject(ctx, "counted", "k1", ports.DeleteOptions{Version: &ref}); err != nil {
			t.Fatal(err)
		}
		holds(t, s, "counted", "a delete of nothing", bucket.UsageTotals{Objects: 2, CurrentBytes: 7000, Versions: 2, StoredBytes: 7000})

		// The parts of uploads in progress are counted apart.
		upload := SampleUpload("counted", "k3", 5)
		if err := s.CreateUpload(ctx, upload); err != nil {
			t.Fatal(err)
		}
		first, second := SamplePart(upload, 1, big, 1), SamplePart(upload, 2, 100, 1)
		for _, part := range []object.Part{first, second} {
			if _, err := s.PutPart(ctx, "counted", "k3", upload.ID, part); err != nil {
				t.Fatal(err)
			}
		}
		holds(t, s, "counted", "two parts", bucket.UsageTotals{Objects: 2, CurrentBytes: 7000, Versions: 2, StoredBytes: 7000, UploadBytes: big + 100})
		second = SamplePart(upload, 2, 300, 2)
		if _, err := s.PutPart(ctx, "counted", "k3", upload.ID, second); err != nil {
			t.Fatal(err)
		}
		holds(t, s, "counted", "a part uploaded again", bucket.UsageTotals{Objects: 2, CurrentBytes: 7000, Versions: 2, StoredBytes: 7000, UploadBytes: big + 300})
		aborted := SampleUpload("counted", "k4", 6)
		if err := s.CreateUpload(ctx, aborted); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutPart(ctx, "counted", "k4", aborted.ID, SamplePart(aborted, 1, 50, 1)); err != nil {
			t.Fatal(err)
		}
		holds(t, s, "counted", "a second upload", bucket.UsageTotals{Objects: 2, CurrentBytes: 7000, Versions: 2, StoredBytes: 7000, UploadBytes: big + 350})
		if _, err := s.AbortUpload(ctx, "counted", "k4", aborted.ID); err != nil {
			t.Fatal(err)
		}
		holds(t, s, "counted", "an abort", bucket.UsageTotals{Objects: 2, CurrentBytes: 7000, Versions: 2, StoredBytes: 7000, UploadBytes: big + 300})
		// A completion moves the bytes from the upload to the version.
		if _, err := s.CompleteUpload(ctx, completionOf(upload, 60, first, second)); err != nil {
			t.Fatal(err)
		}
		whole := bucket.UsageTotals{Objects: 3, CurrentBytes: 7000 + big + 300, Versions: 3, StoredBytes: 7000 + big + 300}
		holds(t, s, "counted", "a completion", whole)
		// One that leaves a part unused gives its bytes back.
		partly := SampleUpload("counted", "k5", 7)
		if err := s.CreateUpload(ctx, partly); err != nil {
			t.Fatal(err)
		}
		used := SamplePart(partly, 1, 700, 1)
		for _, part := range []object.Part{used, SamplePart(partly, 2, 7, 1)} {
			if _, err := s.PutPart(ctx, "counted", "k5", partly.ID, part); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.CompleteUpload(ctx, completionOf(partly, 61, used)); err != nil {
			t.Fatal(err)
		}
		whole = bucket.UsageTotals{Objects: 4, CurrentBytes: whole.CurrentBytes + 700, Versions: 4, StoredBytes: whole.StoredBytes + 700}
		holds(t, s, "counted", "a completion that leaves a part unused", whole)

		// A change of tags, of a retention, of nothing that is counted.
		if _, err := s.SetTags(ctx, "counted", "k2", object.VersionRef{ID: sampleVersionID(4)}, map[string]string{"a": "b"}); err != nil {
			t.Fatal(err)
		}
		holds(t, s, "counted", "a change of tags", whole)
		// The greatest sequence is not behind any version of the bucket.
		if got := usage(t, s, "counted").Usage; got.High < third.Seq || got.High == 0 {
			t.Errorf("the greatest sequence is %d, a version has %d", got.High, third.Seq)
		}
		// What happens in one bucket is not counted in another.
		holds(t, s, "other", "changes of another bucket", bucket.UsageTotals{})
	})

	t.Run("a bucket is counted again by a walk", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "walked")
		versioned(t, s, "walked")
		want := bucket.UsageTotals{}
		fill := byte(0)
		for i := range 7 {
			for n := range 1 + i%3 {
				size := int64(100*i + n)
				fill++
				put(t, s, sized("walked", fmt.Sprintf("key-%d", i), fill, size))
				want.Versions++
				want.StoredBytes += size
				// Of a key's versions the newest is the current one.
				want.CurrentBytes += size
				if n > 0 {
					want.CurrentBytes -= int64(100*i + n - 1)
				}
			}
			want.Objects++
		}
		upload := SampleUpload("walked", "in-progress", 90)
		if err := s.CreateUpload(ctx, upload); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutPart(ctx, "walked", "in-progress", upload.ID, SamplePart(upload, 1, 77, 1)); err != nil {
			t.Fatal(err)
		}
		want.UploadBytes = 77
		holds(t, s, "walked", "being filled", want)

		// Without a restart there is nothing to do for a record that is
		// exact.
		idle, err := s.WalkBucketUsage(ctx, "walked", 10, false)
		if err != nil || idle.Usage.Walking() || !idle.Usage.Exact || idle.Usage.UsageTotals != want || idle.Drift {
			t.Fatalf("a walk nobody asked for = %+v, %v", idle, err)
		}

		// A recount goes on over as many entries as it takes, and the
		// totals are kept right while it does.
		walk, err := s.WalkBucketUsage(ctx, "walked", 2, true)
		if err != nil || !walk.Usage.Walking() || !walk.Usage.Exact || walk.Usage.UsageTotals != want {
			t.Fatalf("the first entry of a recount = %+v, %v", walk, err)
		}
		if got := usage(t, s, "walked").Usage; !got.Walking() || got.UsageTotals != want {
			t.Fatalf("the record during a recount = %+v", got)
		}
		entries := 1
		for walk.Usage.Walking() {
			// Changes arrive while the walk is under way: before its
			// cursor, beyond it, among the uploads.
			switch entries {
			case 1:
				put(t, s, sized("walked", "key-0", 200, 5))
				// The version it takes the place of had no bytes.
				want.Versions, want.StoredBytes, want.CurrentBytes = want.Versions+1, want.StoredBytes+5, want.CurrentBytes+5
			case 2:
				put(t, s, sized("walked", "zz-last", 201, 9))
				want.Objects, want.Versions, want.StoredBytes, want.CurrentBytes = want.Objects+1, want.Versions+1, want.StoredBytes+9, want.CurrentBytes+9
			case 3:
				if _, err := s.PutPart(ctx, "walked", "in-progress", upload.ID, SamplePart(upload, 2, 23, 1)); err != nil {
					t.Fatal(err)
				}
				want.UploadBytes += 23
			}
			if entries > 1000 {
				t.Fatal("the walk does not come to an end")
			}
			if walk, err = s.WalkBucketUsage(ctx, "walked", 2, false); err != nil {
				t.Fatalf("entry %d of the walk: %v", entries, err)
			}
			if !walk.Usage.Exact || walk.Usage.UsageTotals != want {
				t.Fatalf("after entry %d of the walk the totals are %+v, want %+v", entries, walk.Usage.UsageTotals, want)
			}
			entries++
		}
		// It took more than one entry, found what was kept, and is over.
		if entries < 4 || walk.Drift || walk.Usage.UsageTotals != want {
			t.Errorf("the recount took %d entries and ended with drift = %v, %+v; want %+v", entries, walk.Drift, walk.Usage.UsageTotals, want)
		}
		holds(t, s, "walked", "a recount", want)

		// One entry is enough for a bucket smaller than its limit - or two,
		// for a store that learns at the next entry that nothing is left.
		for n := 0; ; n++ {
			walk, err := s.WalkBucketUsage(ctx, "walked", ports.MaxUsageWalk, n == 0)
			if err != nil || walk.Drift || walk.Usage.UsageTotals != want {
				t.Fatalf("a recount with the greatest limit = %+v, %v", walk, err)
			}
			if !walk.Usage.Walking() {
				break
			}
			if n == 3 {
				t.Fatal("a recount of a small bucket with the greatest limit takes more than four entries")
			}
		}
		// A recount that is begun again begins again.
		if _, err := s.WalkBucketUsage(ctx, "walked", 1, true); err != nil {
			t.Fatal(err)
		}
		if _, err := s.WalkBucketUsage(ctx, "walked", 1, false); err != nil {
			t.Fatal(err)
		}
		again, err := s.WalkBucketUsage(ctx, "walked", 1, true)
		if err != nil || !again.Usage.Walking() || again.Usage.Count.Phase != bucket.UsagePhaseKeys {
			t.Errorf("a recount begun again = %+v, %v", again.Usage.Count, err)
		}

		for name, limit := range map[string]int{"no records": 0, "fewer than none": -1, "more than an entry may": ports.MaxUsageWalk + 1} {
			if _, err := s.WalkBucketUsage(ctx, "walked", limit, false); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("a walk of %s = %v", name, err)
			}
		}
		if _, err := s.WalkBucketUsage(ctx, "missing", 10, true); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("a walk of a bucket that is not there = %v", err)
		}
	})

	t.Run("a quota is kept to", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "limited")
		if err := s.SetBucketQuota(ctx, "limited", 10000, noAudit); err != nil {
			t.Fatalf("SetBucketQuota: %v", err)
		}
		if b, err := s.GetBucket(ctx, "limited"); err != nil || b.QuotaBytes != 10000 {
			t.Errorf("GetBucket = quota %d, %v", b.QuotaBytes, err)
		}
		if u := usage(t, s, "limited"); u.QuotaBytes != 10000 || !u.Usage.Exact {
			t.Errorf("BucketUsages = %+v", u)
		}
		first := put(t, s, sized("limited", "a", 1, 4000))
		put(t, s, sized("limited", "b", 2, 5000))
		holding := bucket.UsageTotals{Objects: 2, CurrentBytes: 9000, Versions: 2, StoredBytes: 9000}

		// One byte past the quota is refused, and nothing has changed.
		if _, err := s.PutVersion(ctx, sized("limited", "c", 3, 1001)); !errors.Is(err, domain.ErrQuotaExceeded) {
			t.Fatalf("a put past the quota = %v, want %v", err, domain.ErrQuotaExceeded)
		}
		if _, err := s.GetObject(ctx, "limited", "c"); !errors.Is(err, domain.ErrObjectNotFound) {
			t.Errorf("the object of a refused put: %v", err)
		}
		if referenced, err := s.IsDataReferenced(ctx, sized("limited", "c", 3, 1001).DataID); err != nil || referenced {
			t.Errorf("the data of a refused put is referred to: %v, %v", referenced, err)
		}
		holds(t, s, "limited", "a refused put", holding)
		// Exactly to the quota is taken.
		put(t, s, sized("limited", "c", 4, 1000))
		holding = bucket.UsageTotals{Objects: 3, CurrentBytes: 10000, Versions: 3, StoredBytes: 10000}
		holds(t, s, "limited", "a put to the quota", holding)

		// In a full bucket: an overwrite of the same size and of a smaller
		// one are taken, of a larger one is not - what counts is what the
		// change adds less what it frees.
		put(t, s, sized("limited", "a", 5, 4000))
		holds(t, s, "limited", "an overwrite of the same size", holding)
		if _, err := s.PutVersion(ctx, sized("limited", "a", 6, 4001)); !errors.Is(err, domain.ErrQuotaExceeded) {
			t.Errorf("an overwrite by something larger = %v", err)
		}
		if got, err := s.GetObject(ctx, "limited", "a"); err != nil || got.VersionID != sampleVersionID(5) || got.VersionID == first.VersionID {
			t.Errorf("the object after a refused overwrite = %s, %v", got.VersionID, err)
		}
		put(t, s, sized("limited", "a", 7, 1000))
		holding = bucket.UsageTotals{Objects: 3, CurrentBytes: 7000, Versions: 3, StoredBytes: 7000}
		holds(t, s, "limited", "an overwrite by something smaller", holding)

		// A quota below what the bucket holds is taken; nothing is removed
		// and nothing more comes in. A full bucket can always be emptied.
		if err := s.SetBucketQuota(ctx, "limited", 1, noAudit); err != nil {
			t.Fatalf("a quota below what the bucket holds: %v", err)
		}
		holds(t, s, "limited", "a quota below what it holds", holding)
		if _, err := s.PutVersion(ctx, sized("limited", "d", 8, 1)); !errors.Is(err, domain.ErrQuotaExceeded) {
			t.Errorf("a put into a bucket over its quota = %v", err)
		}
		// Nothing of no bytes adds anything.
		put(t, s, sized("limited", "empty", 9, 0))
		if _, err := s.DeleteObject(ctx, "limited", "b", ports.DeleteOptions{}); err != nil {
			t.Fatalf("a delete in a bucket over its quota: %v", err)
		}
		// Where versions are kept a delete leaves a marker, which costs
		// nothing and is never refused.
		versioned(t, s, "limited")
		if result, err := s.DeleteObject(ctx, "limited", "c", ports.DeleteOptions{MarkerID: sampleVersionID(70), At: contractTime}); err != nil || result.Marker == nil {
			t.Fatalf("a delete marker in a bucket over its quota = %+v, %v", result, err)
		}
		if _, err := s.SetTags(ctx, "limited", "a", object.VersionRef{Null: true}, map[string]string{"a": "b"}); err != nil {
			t.Errorf("tags in a bucket over its quota: %v", err)
		}

		// Without the quota everything is taken again.
		if err := s.SetBucketQuota(ctx, "limited", 0, noAudit); err != nil {
			t.Fatalf("removing the quota: %v", err)
		}
		if b, err := s.GetBucket(ctx, "limited"); err != nil || b.QuotaBytes != 0 {
			t.Errorf("GetBucket after the quota was removed = %d, %v", b.QuotaBytes, err)
		}
		put(t, s, sized("limited", "d", 8, 1<<40))

		for name, quota := range map[string]int64{"below nothing": -1, "beyond the greatest": bucket.MaxQuotaBytes + 1} {
			if err := s.SetBucketQuota(ctx, "limited", quota, noAudit); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("a quota %s = %v", name, err)
			}
		}
		if err := s.SetBucketQuota(ctx, "limited", bucket.MaxQuotaBytes, noAudit); err != nil {
			t.Errorf("the greatest quota: %v", err)
		}
		if err := s.SetBucketQuota(ctx, "missing", 10, noAudit); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("a quota of a bucket that is not there = %v", err)
		}
	})

	t.Run("a quota counts the parts of uploads", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "parts")
		if err := s.SetBucketQuota(ctx, "parts", big+100, noAudit); err != nil {
			t.Fatal(err)
		}
		upload := SampleUpload("parts", "large", 1)
		if err := s.CreateUpload(ctx, upload); err != nil {
			t.Fatal(err)
		}
		first := SamplePart(upload, 1, big, 1)
		if _, err := s.PutPart(ctx, "parts", "large", upload.ID, first); err != nil {
			t.Fatal(err)
		}
		// A part that would pass the quota is refused at the part.
		if _, err := s.PutPart(ctx, "parts", "large", upload.ID, SamplePart(upload, 2, 101, 1)); !errors.Is(err, domain.ErrQuotaExceeded) {
			t.Fatalf("a part past the quota = %v, want %v", err, domain.ErrQuotaExceeded)
		}
		if listed, err := s.ListParts(ctx, "parts", "large", upload.ID, 0, 10); err != nil || len(listed) != 1 {
			t.Errorf("the parts after a refused part = %d, %v", len(listed), err)
		}
		second := SamplePart(upload, 2, 100, 2)
		if _, err := s.PutPart(ctx, "parts", "large", upload.ID, second); err != nil {
			t.Fatalf("a part to the quota: %v", err)
		}
		// The same part again adds what it has less what it replaces.
		second = SamplePart(upload, 2, 100, 3)
		if _, err := s.PutPart(ctx, "parts", "large", upload.ID, second); err != nil {
			t.Fatalf("a part uploaded again, of the same size: %v", err)
		}
		if _, err := s.PutPart(ctx, "parts", "large", upload.ID, SamplePart(upload, 2, 101, 4)); !errors.Is(err, domain.ErrQuotaExceeded) {
			t.Errorf("a part uploaded again, one byte larger = %v", err)
		}
		if listed, err := s.ListParts(ctx, "parts", "large", upload.ID, 1, 10); err != nil || len(listed) != 1 || listed[0].DataID != second.DataID {
			t.Errorf("the part after its larger successor was refused: %+v, %v", listed, err)
		}
		// And a put of anything finds the bucket full.
		if _, err := s.PutVersion(ctx, sized("parts", "small", 9, 1)); !errors.Is(err, domain.ErrQuotaExceeded) {
			t.Errorf("a put beside an upload that fills the bucket = %v", err)
		}
		holds(t, s, "parts", "refused parts", bucket.UsageTotals{UploadBytes: big + 100})
		// A completion is never refused: it adds nothing.
		if _, err := s.CompleteUpload(ctx, completionOf(upload, 20, first, second)); err != nil {
			t.Fatalf("a completion in a full bucket: %v", err)
		}
		holds(t, s, "parts", "the completion", bucket.UsageTotals{Objects: 1, CurrentBytes: big + 100, Versions: 1, StoredBytes: big + 100})
	})

	t.Run("targets are kept", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t)
		if listed, err := s.ListTargets(ctx); err != nil || len(listed) != 0 {
			t.Fatalf("ListTargets of a new store = %+v, %v", listed, err)
		}
		hook := SampleTarget("hook", delivery.KindWebhook)
		hook.Buckets, hook.Refs = []string{"photos", "logs-*"}, 7
		for _, tg := range []delivery.Target{SampleTarget("site-b", delivery.KindS3), hook, SampleTarget("bus", delivery.KindNATS)} {
			if err := s.PutTarget(ctx, tg, noAudit); err != nil {
				t.Fatalf("PutTarget %s: %v", tg.Name, err)
			}
		}
		got, err := s.GetTarget(ctx, "hook")
		if err != nil {
			t.Fatalf("GetTarget: %v", err)
		}
		// Everything is kept as it was given - the sealed secrets too, for
		// the node that leads opens them - but the count of references,
		// which is the store's.
		if got.Kind != hook.Kind || got.Endpoint != hook.Endpoint || !slices.Equal(got.Buckets, hook.Buckets) || got.Auth != hook.Auth ||
			string(got.Secret.Ciphertext) != string(hook.Secret.Ciphertext) || string(got.SignKey.WrappedKey) != string(hook.SignKey.WrappedKey) ||
			got.Secret.KMSKeyID != hook.Secret.KMSKeyID || string(got.Secret.Nonce) != string(hook.Secret.Nonce) ||
			!got.CreatedAt.Equal(contractTime) || !got.UpdatedAt.Equal(contractTime) || got.Refs != 0 {
			t.Errorf("GetTarget = %#v, references %d", got.Endpoint, got.Refs)
		}
		listed, err := s.ListTargets(ctx)
		if err != nil || len(listed) != 3 || listed[0].Name != "bus" || listed[1].Name != "hook" || listed[2].Name != "site-b" {
			t.Fatalf("ListTargets = %v, %v", listed, err)
		}
		if listed[0].Subject != "cactusfs.events" || !listed[0].CreatedAt.Equal(contractTime) || listed[2].Region != "site-b" || listed[2].Auth.ID != "AKIDSITEB" {
			t.Errorf("ListTargets = %v", listed)
		}
		// What is returned is the caller's: changing it changes nothing.
		got.Buckets[0], got.Secret.Ciphertext[0] = "changed", 'X'
		if again, _ := s.GetTarget(ctx, "hook"); again.Buckets[0] != "photos" || again.Secret.Ciphertext[0] != hook.Secret.Ciphertext[0] {
			t.Error("a target that was read shares with the one that is stored")
		}

		// A target is replaced whole, and keeps when it was made.
		replaced := SampleTarget("hook", delivery.KindWebhook)
		replaced.Endpoint, replaced.Audit, replaced.Buckets = "https://other.example.com/in", true, nil
		replaced.CreatedAt, replaced.UpdatedAt = contractTime.Add(time.Hour), contractTime.Add(time.Hour)
		replaced.Auth, replaced.Secret = delivery.Auth{}, iam.SealedSecret{}
		before := cursor(t, s, "hook")
		if err := s.PutTarget(ctx, replaced, noAudit); err != nil {
			t.Fatalf("replacing a target: %v", err)
		}
		got, err = s.GetTarget(ctx, "hook")
		if err != nil || got.Endpoint != replaced.Endpoint || !got.Audit || len(got.Buckets) != 0 || got.SecretSet() || !got.SignKeySet() ||
			!got.CreatedAt.Equal(contractTime) || !got.UpdatedAt.Equal(contractTime.Add(time.Hour)) {
			t.Errorf("a target that was replaced = %#v created %s updated %s, %v", got.Endpoint, got.CreatedAt, got.UpdatedAt, err)
		}
		// So does its cursor.
		if after := cursor(t, s, "hook"); after != before {
			t.Errorf("the cursor of a target that was replaced: %+v, was %+v", after, before)
		}

		// A target that cannot be kept is not.
		for name, edit := range map[string]func(*delivery.Target){
			"no name":                  func(x *delivery.Target) { x.Name = "" },
			"a name that is an ARN":    func(x *delivery.Target) { x.Name = "arn:cactusfs:notify:::x" },
			"an endpoint in the clear": func(x *delivery.Target) { x.Endpoint = "http://hooks.example.com/in" },
			"an endpoint with a user":  func(x *delivery.Target) { x.Endpoint = "https://user@hooks.example.com/in" },
			"a kind there is not":      func(x *delivery.Target) { x.Kind = "kafka" },
			"a secret without a use":   func(x *delivery.Target) { x.Auth = delivery.Auth{} },
		} {
			bad := SampleTarget("bad", delivery.KindWebhook)
			edit(&bad)
			if err := s.PutTarget(ctx, bad, noAudit); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("%s: PutTarget = %v, want %v", name, err, domain.ErrInvalidArgument)
			}
		}
		if _, err := s.GetTarget(ctx, "bad"); !errors.Is(err, domain.ErrEntityNotFound) {
			t.Errorf("a target that was refused: %v", err)
		}
		if _, err := s.GetTarget(ctx, "missing"); !errors.Is(err, domain.ErrEntityNotFound) {
			t.Errorf("GetTarget of none = %v", err)
		}

		if err := s.DeleteTarget(ctx, "bus", noAudit); err != nil {
			t.Fatalf("DeleteTarget: %v", err)
		}
		if err := s.DeleteTarget(ctx, "bus", noAudit); !errors.Is(err, domain.ErrEntityNotFound) {
			t.Errorf("deleting a target twice = %v", err)
		}
		if _, err := s.GetTarget(ctx, "bus"); !errors.Is(err, domain.ErrEntityNotFound) {
			t.Errorf("a target that was deleted: %v", err)
		}
		// Its cursor went with it.
		state, err := s.OutboxCursors(ctx)
		if err != nil || len(state.Cursors) != 2 || state.Cursors[0].Target != "hook" || state.Cursors[1].Target != "site-b" {
			t.Errorf("OutboxCursors after a target was deleted = %+v, %v", state, err)
		}
	})

	t.Run("a document names targets that are there, fit and admit the bucket", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "photos", "logs", "private")
		versioned(t, s, "photos", "logs")
		target(t, s, "hook", delivery.KindWebhook, "photos", "logs*")
		target(t, s, "bus", delivery.KindNATS)
		target(t, s, "site-b", delivery.KindS3, "photos")
		refs := func(name string) int {
			t.Helper()
			got, err := s.GetTarget(ctx, name)
			if err != nil {
				t.Fatalf("GetTarget %s: %v", name, err)
			}
			return got.Refs
		}
		document := func(c interface{ Document() ([]byte, error) }) []byte {
			t.Helper()
			out, err := c.Document()
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		notification := func(targets ...string) []byte {
			var config bucket.NotificationConfig
			for i, name := range targets {
				config.Rules = append(config.Rules, rule(fmt.Sprint(i), name))
			}
			return document(config)
		}
		replication := func(targetName string) []byte {
			return document(bucket.ReplicationConfig{Target: targetName, Bucket: "copy", Rules: []bucket.ReplicationRule{{ID: "all", Enabled: true}}})
		}
		for name, tt := range map[string]struct {
			bucket, kind string
			document     []byte
			want         error
		}{
			"a target that is not there":             {"photos", bucket.ConfigNotification, notification("missing"), domain.ErrInvalidArgument},
			"one of two targets is not there":        {"photos", bucket.ConfigNotification, notification("hook", "missing"), domain.ErrInvalidArgument},
			"another deployment told of changes":     {"photos", bucket.ConfigNotification, notification("site-b"), domain.ErrInvalidArgument},
			"a target that does not admit it":        {"private", bucket.ConfigNotification, notification("hook"), domain.ErrInvalidArgument},
			"versions sent to a webhook":             {"photos", bucket.ConfigReplication, replication("hook"), domain.ErrInvalidArgument},
			"versions sent to a NATS server":         {"photos", bucket.ConfigReplication, replication("bus"), domain.ErrInvalidArgument},
			"versions sent to nowhere":               {"photos", bucket.ConfigReplication, replication("missing"), domain.ErrInvalidArgument},
			"replication, and it is not admitted":    {"logs", bucket.ConfigReplication, replication("site-b"), domain.ErrInvalidArgument},
			"replication without versions":           {"private", bucket.ConfigReplication, replication("site-b"), nil},
			"a document that is not canonical":       {"photos", bucket.ConfigNotification, append(notification("hook"), ' '), domain.ErrInvalidArgument},
			"a document of another kind":             {"photos", bucket.ConfigNotification, replication("site-b"), domain.ErrInvalidArgument},
			"a document that is no document":         {"photos", bucket.ConfigReplication, []byte(`{"v":1}`), domain.ErrInvalidArgument},
			"a document of a version nobody knows":   {"photos", bucket.ConfigNotification, []byte(strings.Replace(string(notification("hook")), `"v":1`, `"v":2`, 1)), domain.ErrUnsupportedFormat},
			"a notification of a bucket that is not": {"missing", bucket.ConfigNotification, notification("bus"), domain.ErrBucketNotFound},
		} {
			err := s.SetBucketConfig(ctx, tt.bucket, tt.kind, tt.document)
			if tt.want == nil {
				// Refused for the state of the bucket, or for the target
				// that does not admit it: either is true of it.
				if !errors.Is(err, domain.ErrInvalidBucketState) && !errors.Is(err, domain.ErrInvalidArgument) {
					t.Errorf("%s: SetBucketConfig = %v", name, err)
				}
			} else if !errors.Is(err, tt.want) {
				t.Errorf("%s: SetBucketConfig = %v, want %v", name, err, tt.want)
			}
		}
		for _, name := range []string{"photos", "logs", "private"} {
			for _, kind := range []string{bucket.ConfigNotification, bucket.ConfigReplication} {
				if stored, err := s.GetBucketConfig(ctx, name, kind); err != nil || stored != nil {
					t.Errorf("after refusals %s has the %s document %s, %v", name, kind, stored, err)
				}
			}
		}
		if refs("hook") != 0 || refs("bus") != 0 || refs("site-b") != 0 {
			t.Errorf("after refusals targets are referred to: %d, %d, %d", refs("hook"), refs("bus"), refs("site-b"))
		}

		// The count of references follows put, replace and removal, and
		// counts documents: two rules of one document are one.
		steps := []struct {
			name           string
			change         func() error
			hook, bus, sit int
		}{
			{"a document that names two", func() error {
				return s.SetBucketConfig(ctx, "photos", bucket.ConfigNotification, notification("hook", "bus", "hook"))
			}, 1, 1, 0},
			{"a document of another bucket", func() error { return s.SetBucketConfig(ctx, "logs", bucket.ConfigNotification, notification("hook")) }, 2, 1, 0},
			{"a replication document", func() error { return s.SetBucketConfig(ctx, "photos", bucket.ConfigReplication, replication("site-b")) }, 2, 1, 1},
			{"the same document again", func() error { return s.SetBucketConfig(ctx, "photos", bucket.ConfigReplication, replication("site-b")) }, 2, 1, 1},
			{"a document replaced", func() error { return s.SetBucketConfig(ctx, "photos", bucket.ConfigNotification, notification("bus")) }, 1, 1, 1},
			{"a document removed", func() error { return s.SetBucketConfig(ctx, "photos", bucket.ConfigNotification, nil) }, 1, 0, 1},
			{"a document that was not there removed", func() error { return s.SetBucketConfig(ctx, "photos", bucket.ConfigNotification, nil) }, 1, 0, 1},
			{"a refused document in place of one", func() error {
				if err := s.SetBucketConfig(ctx, "logs", bucket.ConfigNotification, notification("missing")); !errors.Is(err, domain.ErrInvalidArgument) {
					return fmt.Errorf("a document that names nothing = %w", err)
				}
				return nil
			}, 1, 0, 1},
		}
		for _, step := range steps {
			if err := step.change(); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
			if refs("hook") != step.hook || refs("bus") != step.bus || refs("site-b") != step.sit {
				t.Fatalf("after %s the references are %d, %d, %d; want %d, %d, %d", step.name, refs("hook"), refs("bus"), refs("site-b"), step.hook, step.bus, step.sit)
			}
		}
		if stored, err := s.GetBucketConfig(ctx, "logs", bucket.ConfigNotification); err != nil || string(stored) != string(notification("hook")) {
			t.Errorf("the document a refused one was to replace = %s, %v", stored, err)
		}

		// A target that is named cannot be deleted, nor become of another
		// kind; it can be changed.
		if err := s.DeleteTarget(ctx, "hook", noAudit); !errors.Is(err, domain.ErrEntityInUse) {
			t.Errorf("deleting a target that is named = %v, want %v", err, domain.ErrEntityInUse)
		}
		if err := s.PutTarget(ctx, SampleTarget("hook", delivery.KindS3), noAudit); !errors.Is(err, domain.ErrEntityInUse) {
			t.Errorf("a webhook that is named, made another deployment = %v, want %v", err, domain.ErrEntityInUse)
		}
		changed := SampleTarget("hook", delivery.KindWebhook)
		changed.Endpoint = "https://moved.example.com/in"
		if err := s.PutTarget(ctx, changed, noAudit); err != nil {
			t.Fatalf("changing a target that is named: %v", err)
		}
		if got, err := s.GetTarget(ctx, "hook"); err != nil || got.Refs != 1 || got.Endpoint != changed.Endpoint || got.Kind != delivery.KindWebhook {
			t.Errorf("a named target after a change and a refused one: %d references, kind %s, %v", got.Refs, got.Kind, err)
		}
		// A target nothing names can become of another kind.
		if err := s.PutTarget(ctx, SampleTarget("bus", delivery.KindWebhook), noAudit); err != nil {
			t.Errorf("a target nothing names, made of another kind: %v", err)
		}

		// A bucket that goes takes its documents with it.
		if err := s.DeleteBucket(ctx, "logs"); err != nil {
			t.Fatal(err)
		}
		if refs("hook") != 0 {
			t.Errorf("after its bucket was deleted the target has %d references", refs("hook"))
		}
		if err := s.DeleteTarget(ctx, "hook", noAudit); err != nil {
			t.Errorf("deleting a target nothing names any more: %v", err)
		}
		// The two documents are kinds of schema 5 and are stored like any
		// other.
		if stored, err := s.GetBucketConfig(ctx, "photos", bucket.ConfigReplication); err != nil || string(stored) != string(replication("site-b")) {
			t.Errorf("GetBucketConfig = %s, %v", stored, err)
		}
	})

	t.Run("rows are appended for exactly the rules that match", func(t *testing.T) {
		t.Parallel()
		since := time.Now()
		s := withBucket(t, "photos", "quiet")
		target(t, s, "hook", delivery.KindWebhook)
		target(t, s, "bus", delivery.KindNATS)
		target(t, s, "idle", delivery.KindWebhook)
		// Before there are rules nothing is told.
		put(t, s, sized("photos", "logs/before.gz", 1, 10))
		logs := rule("logs", "hook", delivery.EventPut, delivery.EventComplete, delivery.EventDelete)
		logs.Prefix, logs.Suffix = "logs/", ".gz"
		notify(t, s, "photos", logs, rule("markers", "bus", delivery.EventMarker), rule("all", "bus", delivery.EventPut))

		first := put(t, s, sized("photos", "logs/a.gz", 2, 20))
		put(t, s, sized("photos", "logs/a.txt", 3, 30))    // the suffix does not fit
		put(t, s, sized("photos", "other/a.gz", 4, 40))    // the prefix does not fit
		put(t, s, sized("quiet", "logs/a.gz", 5, 50))      // a bucket without rules
		put(t, s, sized("photos", "logs/a.gz", 6, 60))     // an overwrite, where versions are not kept
		upload := SampleUpload("photos", "logs/big.gz", 7) // the completion of an upload
		if err := s.CreateUpload(ctx, upload); err != nil {
			t.Fatal(err)
		}
		part := SamplePart(upload, 1, 70, 1)
		if _, err := s.PutPart(ctx, "photos", "logs/big.gz", upload.ID, part); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CompleteUpload(ctx, completionOf(upload, 8, part)); err != nil {
			t.Fatal(err)
		}
		// Where versions are not kept a delete removes for good.
		if _, err := s.DeleteObject(ctx, "photos", "logs/a.gz", ports.DeleteOptions{By: "user/bob"}); err != nil {
			t.Fatal(err)
		}
		// Deleting what is not there tells nothing.
		if _, err := s.DeleteObject(ctx, "photos", "logs/never.gz", ports.DeleteOptions{By: "user/bob"}); err != nil {
			t.Fatal(err)
		}
		// Where they are, it leaves a marker; removing a version tells of
		// the version.
		versioned(t, s, "photos")
		kept := put(t, s, sized("photos", "logs/v.gz", 9, 90))
		marked, err := s.DeleteObject(ctx, "photos", "logs/v.gz", ports.DeleteOptions{MarkerID: sampleVersionID(100), At: contractTime, By: "user/bob"})
		if err != nil || marked.Marker == nil {
			t.Fatalf("DeleteObject = %+v, %v", marked, err)
		}
		ref := kept.Ref()
		if _, err := s.DeleteObject(ctx, "photos", "logs/v.gz", ports.DeleteOptions{Version: &ref, By: "service/lifecycle"}); err != nil {
			t.Fatal(err)
		}
		// Tags, a retention and a legal hold are not told to anybody who
		// is notified.
		if _, err := s.SetTags(ctx, "photos", "logs/big.gz", object.VersionRef{Null: true}, map[string]string{"a": "b"}); err != nil {
			t.Fatal(err)
		}

		hook := read(t, s, "hook", 100)
		if got, want := told(hook), "put logs/a.gz; put logs/a.gz; complete logs/big.gz; delete logs/a.gz; put logs/v.gz; delete logs/v.gz"; got != want {
			t.Fatalf("the rows of the webhook:\n got %s\nwant %s", got, want)
		}
		bus := read(t, s, "bus", 100)
		if got, want := told(bus), "put logs/a.gz; put logs/a.txt; put other/a.gz; put logs/a.gz; put logs/v.gz; marker logs/v.gz"; got != want {
			t.Fatalf("the rows of the NATS server:\n got %s\nwant %s", got, want)
		}
		if idle := read(t, s, "idle", 100); len(idle) != 0 {
			t.Errorf("a target no rule names is told %s", told(idle))
		}

		// A row says what a receiver is told, and nothing of the object.
		row := hook[0]
		if row.Bucket != "photos" || row.Version != first.Ref() || row.Size != 20 || row.ETag != first.ETag || row.By != "user/alice" ||
			row.At.Before(since.Add(-time.Minute)) || row.At.After(time.Now().Add(time.Minute)) || row.ID() != row.Seq.String() {
			t.Errorf("the row of a put = %+v", row)
		}
		if err := row.Validate(); err != nil {
			t.Errorf("a row that was read: %v", err)
		}
		// One row for a change, for all its consumers: the row of the put
		// both targets were told of is one row.
		if kind, ok := row.For("bus"); !ok || kind != delivery.ConsumerNotification || bus[0].Seq != row.Seq || len(row.To) != 2 {
			t.Errorf("the row of a put that two targets are told of: %+v and %+v", row, bus[0])
		}
		if done := hook[2]; done.Event != delivery.EventComplete || done.Size != 70 || done.Version != (object.VersionRef{Null: true}) {
			t.Errorf("the row of a completion = %+v", done)
		}
		if gone := hook[3]; gone.By != "user/bob" || gone.Size != 60 || gone.Version != (object.VersionRef{Null: true}) {
			t.Errorf("the row of a delete = %+v", gone)
		}
		if marker := bus[5]; marker.By != "user/bob" || marker.Size != 0 || marker.ETag != "" || marker.Version != marked.Marker.Ref() {
			t.Errorf("the row of a delete marker = %+v", marker)
		}
		if removed := hook[5]; removed.By != "service/lifecycle" || removed.Version != kept.Ref() || removed.Size != 90 {
			t.Errorf("the row of a version removed for good = %+v", removed)
		}
		// Rows are in the order of the changes, each at a place of its own.
		for _, rows := range [][]delivery.Item{hook, bus} {
			for i := 1; i < len(rows); i++ {
				if !rows[i].Seq.After(rows[i-1].Seq) {
					t.Fatalf("rows out of order: %s then %s", rows[i-1].Seq, rows[i].Seq)
				}
			}
		}

		// Reading: a page at a time gives the same, and says how far it
		// looked.
		if paged := read(t, s, "hook", 1); told(paged) != told(hook) {
			t.Errorf("read a row at a time: %s", told(paged))
		}
		if paged := read(t, s, "bus", 4); told(paged) != told(bus) {
			t.Errorf("read four rows at a time: %s", told(paged))
		}
		tail, upto, err := s.ReadOutbox(ctx, "hook", hook[3].Seq, 100)
		if err != nil || told(tail) != "put logs/v.gz; delete logs/v.gz" || upto.Compare(tail[1].Seq) < 0 {
			t.Errorf("ReadOutbox after the fourth row = %s up to %s, %v", told(tail), upto, err)
		}
		// Beyond the last row there is nothing, and nothing was looked at.
		beyond := delivery.Seq{Index: ^uint64(0), Row: 1}
		if rest, upto, err := s.ReadOutbox(ctx, "hook", beyond, 100); err != nil || len(rest) != 0 || upto != beyond {
			t.Errorf("ReadOutbox beyond the end = %d rows up to %s, %v", len(rest), upto, err)
		}
		// What is read is the caller's.
		hook[0].To[0].Target = "changed"
		if again := read(t, s, "hook", 100); len(again) == 0 || again[0].To[0].Target == "changed" {
			t.Error("a row that was read shares with the one that is stored")
		}
		for name, limit := range map[string]int{"no rows": 0, "fewer than none": -1, "more than a read may": delivery.MaxOutboxRead + 1} {
			if _, _, err := s.ReadOutbox(ctx, "hook", delivery.Seq{}, limit); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("a read of %s = %v", name, err)
			}
		}
		if _, _, err := s.ReadOutbox(ctx, "missing", delivery.Seq{}, 10); !errors.Is(err, domain.ErrEntityNotFound) {
			t.Errorf("a read for a target that is not there = %v", err)
		}
		// Refused changes tell nothing.
		if err := s.SetBucketQuota(ctx, "photos", 1, noAudit); err != nil {
			t.Fatal(err)
		}
		if _, err := s.PutVersion(ctx, sized("photos", "logs/refused.gz", 11, 5)); !errors.Is(err, domain.ErrQuotaExceeded) {
			t.Fatalf("a put past the quota = %v", err)
		}
		if again := read(t, s, "hook", 100); len(again) != len(hook) {
			t.Errorf("a refused put is told: %s", told(again))
		}
	})

	t.Run("a cursor moves forward only and rows every target has passed go", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "photos")
		target(t, s, "fast", delivery.KindWebhook)
		target(t, s, "slow", delivery.KindWebhook)
		start := cursor(t, s, "fast")
		if start.From != start.Acked || start.Lost != 0 || start.ResyncNeeded || start.AuditAcked != 0 {
			t.Fatalf("the cursor of a new target = %+v", start)
		}
		if later := cursor(t, s, "slow"); !later.From.After(start.From) {
			t.Errorf("a target defined later begins at %s, the earlier one at %s", later.From, start.From)
		}
		notify(t, s, "photos", rule("fast", "fast"), rule("slow", "slow"))
		for i, fill := range []byte{1, 2, 3, 4, 5, 6} {
			put(t, s, sized("photos", fmt.Sprintf("k%d", i), fill, 1))
		}
		rows := read(t, s, "fast", 100)
		if len(rows) != 6 {
			t.Fatalf("six puts made %d rows", len(rows))
		}
		state, err := s.OutboxCursors(ctx)
		if err != nil || state.Rows != 6 || state.Dropped != 0 || len(state.Cursors) != 2 {
			t.Fatalf("OutboxCursors = %+v, %v", state, err)
		}

		if err := s.AckOutbox(ctx, "fast", rows[3].Seq, 0, nil); err != nil {
			t.Fatalf("AckOutbox: %v", err)
		}
		if got := cursor(t, s, "fast"); got.Acked != rows[3].Seq || got.From != start.From {
			t.Errorf("the cursor after an acknowledgement = %+v", got)
		}
		// An acknowledgement of a node that led before, arriving late:
		// nothing goes back, and it is no error.
		for _, behind := range []delivery.Seq{rows[1].Seq, {}, start.From} {
			if err := s.AckOutbox(ctx, "fast", behind, 0, nil); err != nil {
				t.Fatalf("an acknowledgement behind the cursor: %v", err)
			}
			if got := cursor(t, s, "fast"); got.Acked != rows[3].Seq {
				t.Fatalf("the cursor went back to %s", got.Acked)
			}
		}
		// Nothing is removed while one target has not passed it.
		if state, _ := s.OutboxCursors(ctx); state.Rows != 6 {
			t.Errorf("rows were removed that a target has not passed: %d left", state.Rows)
		}
		if waiting := read(t, s, "slow", 100); len(waiting) != 6 {
			t.Errorf("the slow target is left %d rows", len(waiting))
		}
		// What the slow one passes, both have passed.
		if err := s.AckOutbox(ctx, "slow", rows[2].Seq, 0, nil); err != nil {
			t.Fatal(err)
		}
		if state, _ := s.OutboxCursors(ctx); state.Rows != 3 || state.Dropped != 0 {
			t.Errorf("after both targets passed three rows %d are left, %d dropped", state.Rows, state.Dropped)
		}
		// What is left is read from where a target is.
		if rest, _, err := s.ReadOutbox(ctx, "fast", cursor(t, s, "fast").Acked, 100); err != nil || told(rest) != "put k4; put k5" {
			t.Errorf("the rows after the cursor = %s, %v", told(rest), err)
		}

		// An acknowledgement beyond the last row counts as the last row:
		// what is written afterwards is still the target's.
		if err := s.AckOutbox(ctx, "fast", delivery.Seq{Index: ^uint64(0), Row: 65535}, 0, nil); err != nil {
			t.Fatalf("an acknowledgement beyond the end: %v", err)
		}
		put(t, s, sized("photos", "after", 50, 1))
		if rest, _, err := s.ReadOutbox(ctx, "fast", cursor(t, s, "fast").Acked, 100); err != nil || told(rest) != "put after" {
			t.Errorf("after an acknowledgement beyond the end the target is left %q, %v", told(rest), err)
		}

		// The audit trail has a cursor of its own, for a target that is
		// sent it, and for no other.
		audited := SampleTarget("trail", delivery.KindWebhook)
		audited.Audit = true
		if err := s.PutTarget(ctx, audited, noAudit); err != nil {
			t.Fatal(err)
		}
		for _, upto := range []uint64{7, 3, 7, 9} {
			if err := s.AckOutbox(ctx, "trail", delivery.Seq{}, upto, nil); err != nil {
				t.Fatal(err)
			}
		}
		if got := cursor(t, s, "trail"); got.AuditAcked != 9 {
			t.Errorf("the cursor of the audit trail = %d, want 9", got.AuditAcked)
		}
		if err := s.AckOutbox(ctx, "slow", delivery.Seq{}, 5, nil); err != nil {
			t.Fatal(err)
		}
		if got := cursor(t, s, "slow"); got.AuditAcked != 0 {
			t.Errorf("a target that is not sent the trail has passed %d of it", got.AuditAcked)
		}

		if err := s.AckOutbox(ctx, "missing", rows[0].Seq, 0, nil); !errors.Is(err, domain.ErrEntityNotFound) {
			t.Errorf("an acknowledgement for a target that is not there = %v", err)
		}
		// Results are of replication: a webhook has none to give.
		result := delivery.Result{Seq: rows[5].Seq, Status: object.ReplicationCompleted}
		if err := s.AckOutbox(ctx, "slow", delivery.Seq{}, 0, []delivery.Result{result}); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("a result from a webhook = %v", err)
		}
	})

	t.Run("a target defined again is sent nothing of before", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "photos", "kept")
		target(t, s, "hook", delivery.KindWebhook)
		target(t, s, "other", delivery.KindWebhook)
		notify(t, s, "photos", rule("r", "hook"), rule("o", "other"))
		put(t, s, sized("photos", "old", 1, 1))
		if _, err := s.DeleteObject(ctx, "photos", "old", ports.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		// The bucket goes; its rows stay for their targets.
		if err := s.DeleteBucket(ctx, "photos"); err != nil {
			t.Fatal(err)
		}
		if rows := read(t, s, "hook", 100); told(rows) != "put old; delete old" {
			t.Fatalf("the rows of a bucket that was deleted = %q", told(rows))
		}
		// The target goes, and one of its name is defined: the rows that
		// are still there for the other target are not its own.
		if err := s.DeleteTarget(ctx, "hook", noAudit); err != nil {
			t.Fatal(err)
		}
		target(t, s, "hook", delivery.KindWebhook)
		if rows := read(t, s, "hook", 100); len(rows) != 0 {
			t.Errorf("a target defined again is sent %q", told(rows))
		}
		if rows := read(t, s, "other", 100); told(rows) != "put old; delete old" {
			t.Errorf("the other target is left %q", told(rows))
		}
		notify(t, s, "kept", rule("r", "hook"))
		put(t, s, sized("kept", "new", 2, 1))
		if rows := read(t, s, "hook", 100); told(rows) != "put new" {
			t.Errorf("a target defined again is sent %q of after", told(rows))
		}
	})

	t.Run("replication is told and its status is kept on the version", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "photos", "plain")
		target(t, s, "site-b", delivery.KindS3)
		target(t, s, "hook", delivery.KindWebhook)
		all := bucket.ReplicationRule{ID: "all", Enabled: true, Priority: 1}
		logs := bucket.ReplicationRule{ID: "logs", Enabled: true, Priority: 5, Prefix: "logs/", Markers: true}
		// A bucket that does not keep versions is not replicated.
		document, err := bucket.ReplicationConfig{Target: "site-b", Bucket: "copy", Rules: []bucket.ReplicationRule{all}}.Document()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetBucketConfig(ctx, "plain", bucket.ConfigReplication, document); !errors.Is(err, domain.ErrInvalidBucketState) {
			t.Errorf("replication of a bucket without versions = %v, want %v", err, domain.ErrInvalidBucketState)
		}
		versioned(t, s, "photos")
		before := put(t, s, sized("photos", "before", 1, 10))
		replicate(t, s, "photos", "site-b", all, logs)
		// And it goes on keeping them.
		if err := s.SetBucketVersioning(ctx, "photos", bucket.VersioningSuspended); !errors.Is(err, domain.ErrInvalidBucketState) {
			t.Errorf("suspending versioning under replication = %v, want %v", err, domain.ErrInvalidBucketState)
		}
		if err := s.SetBucketVersioning(ctx, "photos", bucket.VersioningEnabled); err != nil {
			t.Errorf("enabling what is enabled: %v", err)
		}

		// A version a rule matches is pending from its commit; one that
		// was there before has no status.
		one := put(t, s, sized("photos", "a", 2, 20))
		two := put(t, s, sized("photos", "logs/b", 3, 30))
		if one.Replication != object.ReplicationPending || two.Replication != object.ReplicationPending {
			t.Fatalf("PutVersion says %q and %q", one.Replication, two.Replication)
		}
		if got := version(t, s, "photos", "before", before.Ref()); got.Replication != "" {
			t.Errorf("a version of before the configuration says %q", got.Replication)
		}
		// Whatever a caller says of the status, the store says its own.
		claimed := sized("photos", "claimed", 4, 40)
		claimed.Replication = object.ReplicationCompleted
		if got := put(t, s, claimed); got.Replication != object.ReplicationPending {
			t.Errorf("a put that claims a status is stored as %q", got.Replication)
		}
		holds(t, s, "photos", "three pending versions", bucket.UsageTotals{Objects: 4, CurrentBytes: 100, Versions: 4, StoredBytes: 100, ReplicationPending: 3})

		// Markers go where the rule that decides says so, and never when
		// the lifecycle service made them. A version removed for good is
		// never told to the other deployment.
		noMarker, err := s.DeleteObject(ctx, "photos", "a", ports.DeleteOptions{MarkerID: sampleVersionID(60), At: contractTime, By: "user/bob"})
		if err != nil || noMarker.Marker == nil || noMarker.Marker.Replication != "" {
			t.Fatalf("a marker where markers are not copied = %+v, %v", noMarker.Marker, err)
		}
		marker, err := s.DeleteObject(ctx, "photos", "logs/b", ports.DeleteOptions{MarkerID: sampleVersionID(61), At: contractTime, By: "user/bob"})
		if err != nil || marker.Marker == nil || marker.Marker.Replication != object.ReplicationPending {
			t.Fatalf("a marker where markers are copied = %+v, %v", marker.Marker, err)
		}
		expired, err := s.DeleteObject(ctx, "photos", "logs/c", ports.DeleteOptions{MarkerID: sampleVersionID(62), At: contractTime, By: bucket.LifecycleActor})
		if err != nil || expired.Marker == nil || expired.Marker.Replication != "" {
			t.Fatalf("a marker of the lifecycle service = %+v, %v", expired.Marker, err)
		}
		ref := noMarker.Marker.Ref()
		if _, err := s.DeleteObject(ctx, "photos", "a", ports.DeleteOptions{Version: &ref, By: "user/bob"}); err != nil {
			t.Fatal(err)
		}
		// A later change of a version that is being replicated follows it;
		// of one no rule was about, nothing.
		if _, err := s.SetTags(ctx, "photos", "a", one.Ref(), map[string]string{"stage": "final"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetTags(ctx, "photos", "before", before.Ref(), map[string]string{"stage": "final"}); err != nil {
			t.Fatal(err)
		}
		rows := read(t, s, "site-b", 100)
		if got, want := told(rows), "put a; put logs/b; put claimed; marker logs/b; tags a"; got != want {
			t.Fatalf("the rows of the other deployment:\n got %s\nwant %s", got, want)
		}
		for _, row := range rows {
			if kind, ok := row.For("site-b"); !ok || kind != delivery.ConsumerReplication || len(row.To) != 1 {
				t.Errorf("the row %s %s is for %v", row.Event, row.Key, row.To)
			}
		}
		if hooked := read(t, s, "hook", 100); len(hooked) != 0 {
			t.Errorf("a target of no rule is told %s", told(hooked))
		}

		// An acknowledgement says what became of versions: by the row
		// that told of them, or by the version itself - which is how one
		// a resync sent, like the one of before, gets its status.
		results := []delivery.Result{
			{Seq: rows[0].Seq, Status: object.ReplicationCompleted},
			{Seq: rows[1].Seq, Status: object.ReplicationFailed},
			{Bucket: "photos", Key: "before", Version: before.Ref(), Status: object.ReplicationCompleted},
			// What is gone, or never was, changes nothing and is no error.
			{Bucket: "photos", Key: "a", Version: noMarker.Marker.Ref(), Status: object.ReplicationCompleted},
			{Bucket: "photos", Key: "never", Version: object.VersionRef{ID: sampleVersionID(77)}, Status: object.ReplicationFailed},
			{Bucket: "missing", Key: "k", Version: object.VersionRef{Null: true}, Status: object.ReplicationFailed},
			{Seq: delivery.Seq{Index: ^uint64(0)}, Status: object.ReplicationCompleted},
		}
		if err := s.AckOutbox(ctx, "site-b", rows[1].Seq, 0, results); err != nil {
			t.Fatalf("AckOutbox with results: %v", err)
		}
		for name, tt := range map[string]struct {
			key  string
			ref  object.VersionRef
			want object.ReplicationStatus
		}{
			"taken by the other deployment":   {"a", one.Ref(), object.ReplicationCompleted},
			"given up on":                     {"logs/b", two.Ref(), object.ReplicationFailed},
			"sent by a resync":                {"before", before.Ref(), object.ReplicationCompleted},
			"not acknowledged yet":            {"claimed", object.VersionRef{ID: sampleVersionID(4)}, object.ReplicationPending},
			"a marker not acknowledged yet":   {"logs/b", marker.Marker.Ref(), object.ReplicationPending},
			"a marker of the lifecycle rules": {"logs/c", expired.Marker.Ref(), ""},
		} {
			if got := version(t, s, "photos", tt.key, tt.ref); got.Replication != tt.want {
				t.Errorf("%s: the version says %q, want %q", name, got.Replication, tt.want)
			}
		}
		if got := usage(t, s, "photos").Usage; got.ReplicationPending != 2 || got.ReplicationFailed != 1 {
			t.Errorf("the bucket counts %d pending and %d failed, want 2 and 1", got.ReplicationPending, got.ReplicationFailed)
		}
		// A result that is none is refused, and with it the whole
		// acknowledgement.
		acked := cursor(t, s, "site-b").Acked
		bad := []delivery.Result{{Seq: rows[2].Seq, Status: object.ReplicationCompleted}, {Seq: rows[3].Seq, Status: object.ReplicationPending}}
		if err := s.AckOutbox(ctx, "site-b", rows[4].Seq, 0, bad); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("a result that says pending = %v", err)
		}
		if err := s.AckOutbox(ctx, "site-b", rows[4].Seq, 0, make([]delivery.Result, delivery.MaxAckResults+1)); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("more results than an acknowledgement carries = %v", err)
		}
		if got := cursor(t, s, "site-b"); got.Acked != acked {
			t.Errorf("a refused acknowledgement moved the cursor to %s", got.Acked)
		}
		if got := version(t, s, "photos", "claimed", object.VersionRef{ID: sampleVersionID(4)}); got.Replication != object.ReplicationPending {
			t.Errorf("a refused acknowledgement set a status: %q", got.Replication)
		}
		// A row of another target names nothing for this one.
		target(t, s, "site-c", delivery.KindS3)
		if err := s.AckOutbox(ctx, "site-c", delivery.Seq{}, 0, []delivery.Result{{Seq: rows[2].Seq, Status: object.ReplicationFailed}}); err != nil {
			t.Fatal(err)
		}
		if got := version(t, s, "photos", "claimed", object.VersionRef{ID: sampleVersionID(4)}); got.Replication != object.ReplicationPending {
			t.Errorf("a target set the status of a version by a row that was not its own: %q", got.Replication)
		}

		// Without the configuration nothing more is pending.
		if err := s.SetBucketConfig(ctx, "photos", bucket.ConfigReplication, nil); err != nil {
			t.Fatal(err)
		}
		if got := put(t, s, sized("photos", "later", 9, 1)); got.Replication != "" {
			t.Errorf("a version after the configuration was removed says %q", got.Replication)
		}
		if err := s.SetBucketVersioning(ctx, "photos", bucket.VersioningSuspended); err != nil {
			t.Errorf("suspending versioning once the configuration is gone: %v", err)
		}
	})

	t.Run("a replica bucket takes the versions of its source and no others", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "replica", "plain", "used", "sending")
		target(t, s, "site-b", delivery.KindS3)

		// Becoming a replica: a bucket that keeps versions, holds none
		// and is not replicated itself.
		if err := s.SetBucketReplica(ctx, "plain", sourceID, noAudit); !errors.Is(err, domain.ErrInvalidBucketState) {
			t.Errorf("a replica that keeps no versions = %v, want %v", err, domain.ErrInvalidBucketState)
		}
		versioned(t, s, "replica", "used", "sending")
		own := put(t, s, sized("used", "own", 1, 10))
		if err := s.SetBucketReplica(ctx, "used", sourceID, noAudit); !errors.Is(err, domain.ErrBucketNotEmpty) {
			t.Errorf("a replica that holds versions of its own = %v, want %v", err, domain.ErrBucketNotEmpty)
		}
		// Not even a delete marker, or a version that is not current.
		if _, err := s.DeleteObject(ctx, "used", "own", ports.DeleteOptions{MarkerID: sampleVersionID(40), At: contractTime}); err != nil {
			t.Fatal(err)
		}
		ref := own.Ref()
		if _, err := s.DeleteObject(ctx, "used", "own", ports.DeleteOptions{Version: &ref}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetBucketReplica(ctx, "used", sourceID, noAudit); !errors.Is(err, domain.ErrBucketNotEmpty) {
			t.Errorf("a replica that holds a delete marker of its own = %v, want %v", err, domain.ErrBucketNotEmpty)
		}
		replicate(t, s, "sending", "site-b", bucket.ReplicationRule{ID: "all", Enabled: true})
		if err := s.SetBucketReplica(ctx, "sending", sourceID, noAudit); !errors.Is(err, domain.ErrInvalidBucketState) {
			t.Errorf("a replica that is replicated itself = %v, want %v", err, domain.ErrInvalidBucketState)
		}
		for name, source := range map[string]string{"with a space": "site a", "that is too long": strings.Repeat("a", object.MaxDeploymentIDLength+1)} {
			if err := s.SetBucketReplica(ctx, "replica", source, noAudit); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("a source %s = %v", name, err)
			}
		}
		if err := s.SetBucketReplica(ctx, "missing", sourceID, noAudit); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("a replica that is not there = %v", err)
		}
		// Promoting what is no replica is nothing.
		if err := s.SetBucketReplica(ctx, "plain", "", noAudit); err != nil {
			t.Errorf("promoting a bucket that is no replica: %v", err)
		}
		if err := s.SetBucketReplica(ctx, "replica", sourceID, noAudit); err != nil {
			t.Fatalf("SetBucketReplica: %v", err)
		}
		if b, err := s.GetBucket(ctx, "replica"); err != nil || b.ReplicaFrom != sourceID {
			t.Errorf("GetBucket = replica of %q, %v", b.ReplicaFrom, err)
		}
		if listed, err := s.ListBuckets(ctx); err != nil || len(listed) != 4 || listed[1].Name != "replica" || listed[1].ReplicaFrom != sourceID || listed[0].ReplicaFrom != "" {
			t.Errorf("ListBuckets = %+v, %v", listed, err)
		}

		// A replica is not replicated again, and keeps versions.
		document, err := bucket.ReplicationConfig{Target: "site-b", Bucket: "copy", Rules: []bucket.ReplicationRule{{ID: "all", Enabled: true}}}.Document()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetBucketConfig(ctx, "replica", bucket.ConfigReplication, document); !errors.Is(err, domain.ErrInvalidBucketState) {
			t.Errorf("replication of a replica = %v, want %v", err, domain.ErrInvalidBucketState)
		}
		if err := s.SetBucketVersioning(ctx, "replica", bucket.VersioningSuspended); !errors.Is(err, domain.ErrInvalidBucketState) {
			t.Errorf("suspending versioning of a replica = %v, want %v", err, domain.ErrInvalidBucketState)
		}

		// Nothing that makes a version or a marker is taken from anybody
		// but the source - whoever asks.
		foreign := replicaOf("replica", "k", 9, 5)
		foreign.Replica.From = otherSourceID
		noSeq := replicaOf("replica", "k", 9, 0)
		refusals := map[string]struct {
			err  error
			want error
		}{}
		refuse := func(name string, want error, err error) {
			refusals[name] = struct{ err, want error }{err, want}
		}
		_, err = s.PutVersion(ctx, sized("replica", "k", 9, 1))
		refuse("an ordinary put", domain.ErrInvalidBucketState, err)
		_, err = s.PutVersion(ctx, foreign)
		refuse("a replica of another source", domain.ErrInvalidBucketState, err)
		_, err = s.PutVersion(ctx, noSeq)
		refuse("a replica without a sequence", domain.ErrInvalidArgument, err)
		refuse("an ordinary upload", domain.ErrInvalidBucketState, s.CreateUpload(ctx, SampleUpload("replica", "k", 9)))
		_, err = s.DeleteObject(ctx, "replica", "k", ports.DeleteOptions{MarkerID: sampleVersionID(41), At: contractTime})
		refuse("an ordinary delete, which would leave a marker", domain.ErrInvalidBucketState, err)
		_, err = s.DeleteObject(ctx, "replica", "k", ports.DeleteOptions{MarkerID: sampleVersionID(41), At: contractTime, Replica: &object.Replica{From: otherSourceID, Seq: 5}})
		refuse("a marker of another source", domain.ErrInvalidBucketState, err)
		// And no other bucket takes anything of a source.
		_, err = s.PutVersion(ctx, replicaOf("plain", "k", 9, 5))
		refuse("a replica put into a bucket that is none", domain.ErrInvalidBucketState, err)
		_, err = s.DeleteObject(ctx, "plain", "k", ports.DeleteOptions{MarkerID: sampleVersionID(41), At: contractTime, Replica: &object.Replica{From: sourceID, Seq: 5}})
		refuse("a replica marker in a bucket that is none", domain.ErrInvalidBucketState, err)
		fromSource := SampleUpload("plain", "k", 9)
		fromSource.ReplicaFrom = sourceID
		refuse("a replica upload in a bucket that is none", domain.ErrInvalidBucketState, s.CreateUpload(ctx, fromSource))
		for name, tt := range refusals {
			if !errors.Is(tt.err, tt.want) {
				t.Errorf("%s = %v, want %v", name, tt.err, tt.want)
			}
		}
		for _, name := range []string{"replica", "plain"} {
			holds(t, s, name, "refusals", bucket.UsageTotals{})
		}
	})

	t.Run("replica versions are in the order of their source whatever order they arrive in", func(t *testing.T) {
		t.Parallel()
		// The same versions and markers of one key, arriving in three
		// orders: a retry, the live stream and a resync interleave.
		type arrival struct {
			fill   byte
			seq    uint64
			marker bool
		}
		arrivals := []arrival{{1, 300, false}, {2, 100, false}, {3, 250, true}, {4, 5000, false}, {5, 200, false}}
		orders := [][]int{{0, 1, 2, 3, 4}, {4, 3, 2, 1, 0}, {2, 0, 4, 1, 3}}
		names := []string{"in-order", "reversed", "shuffled"}
		s := replicaBucket(t, names...)
		target(t, s, "hook", delivery.KindWebhook)
		notify(t, s, "shuffled", rule("all", "hook"))
		listed := make([][]string, len(names))
		for n, name := range names {
			for _, i := range orders[n] {
				a := arrivals[i]
				if a.marker {
					result, err := s.DeleteObject(ctx, name, "k", ports.DeleteOptions{
						MarkerID: sampleVersionID(a.fill), At: contractTime.Add(-time.Hour), By: "key/AKIDSITEA", Replica: &object.Replica{From: sourceID, Seq: a.seq},
					})
					if err != nil || result.Marker == nil || result.Marker.Seq != a.seq || result.Marker.Replication != object.ReplicationReplica ||
						result.Marker.VersionID != sampleVersionID(a.fill) || !result.Marker.ModifiedAt.Equal(contractTime.Add(-time.Hour)) {
						t.Fatalf("%s: a replica marker = %+v, %v", name, result.Marker, err)
					}
					continue
				}
				v := replicaOf(name, "k", a.fill, a.seq)
				stored := put(t, s, v)
				// The copy has the ID, the time and the sequence of its
				// source, says that it is a replica, and carries nothing
				// of how it was said.
				if stored.VersionID != v.VersionID || !stored.ModifiedAt.Equal(v.ModifiedAt) || stored.Seq != a.seq || !stored.Versioned ||
					stored.Replication != object.ReplicationReplica || stored.Replica != nil {
					t.Fatalf("%s: a replica version is stored as %+v", name, stored)
				}
			}
			err := s.ScanVersions(ctx, name, ports.VersionCursor{}, func(v object.Version, latest bool) ports.ScanControl {
				listed[n] = append(listed[n], fmt.Sprintf("%s seq=%d marker=%t latest=%t status=%s at=%s", v.VersionID, v.Seq, v.DeleteMarker, latest, v.Replication, v.ModifiedAt.Format(time.RFC3339)))
				return ports.ScanControl{}
			})
			if err != nil {
				t.Fatal(err)
			}
			// The current version is the one of the greatest sequence.
			if current, err := s.GetObject(ctx, name, "k"); err != nil || current.Seq != 5000 || current.VersionID != sampleVersionID(4) {
				t.Errorf("%s: the current version = %s at %d, %v", name, current.VersionID, current.Seq, err)
			}
			holds(t, s, name, "five arrivals", bucket.UsageTotals{Objects: 1, CurrentBytes: 40, Versions: 5, StoredBytes: 10 + 20 + 40 + 50})
		}
		for n := 1; n < len(names); n++ {
			if !slices.Equal(listed[n], listed[0]) {
				t.Errorf("%s lists\n%s\n and %s lists\n%s", names[n], strings.Join(listed[n], "\n"), names[0], strings.Join(listed[0], "\n"))
			}
		}
		if len(listed[0]) != 5 || !strings.Contains(listed[0][0], "seq=5000") || !strings.Contains(listed[0][4], "seq=100") {
			t.Errorf("the versions are listed as\n%s", strings.Join(listed[0], "\n"))
		}

		// A version that arrives a second time is there already: the
		// source reads the conflict as done, and nothing has changed.
		if _, err := s.PutVersion(ctx, replicaOf("in-order", "k", 1, 300)); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("a second arrival of a version = %v, want %v", err, domain.ErrConflict)
		}
		again := replicaOf("in-order", "k", 1, 9000)
		if _, err := s.PutVersion(ctx, again); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("a second arrival at another sequence = %v, want %v", err, domain.ErrConflict)
		}
		_, err := s.DeleteObject(ctx, "in-order", "k", ports.DeleteOptions{MarkerID: sampleVersionID(3), At: contractTime, Replica: &object.Replica{From: sourceID, Seq: 250}})
		if !errors.Is(err, domain.ErrConflict) {
			t.Errorf("a second arrival of a marker = %v, want %v", err, domain.ErrConflict)
		}
		holds(t, s, "in-order", "second arrivals", bucket.UsageTotals{Objects: 1, CurrentBytes: 40, Versions: 5, StoredBytes: 120})

		// A replica is told to who is notified, as the put or the marker
		// it is - and is never sent on.
		if rows := read(t, s, "hook", 100); len(rows) != 5 || rows[0].Event != delivery.EventMarker || rows[1].Version != (object.VersionRef{ID: sampleVersionID(1)}) {
			t.Errorf("the arrivals in a replica bucket are told as %s", told(rows))
		}

		// The null version of the source is the null version here.
		null := replicaOf("reversed", "old", 20, 7)
		null.Replica.Null = true
		if stored := put(t, s, null); stored.Versioned || stored.Seq != 7 || stored.Replication != object.ReplicationReplica {
			t.Errorf("the null version of the source is stored as %+v", stored)
		}
		if got := version(t, s, "reversed", "old", object.VersionRef{Null: true}); got.VersionID != null.VersionID {
			t.Errorf("the null version of a replica = %+v", got)
		}
		twice := replicaOf("reversed", "old", 21, 8)
		twice.Replica.Null = true
		if _, err := s.PutVersion(ctx, twice); !errors.Is(err, domain.ErrConflict) {
			t.Errorf("a second null version of a key = %v, want %v", err, domain.ErrConflict)
		}

		// Removing one version for good is judged as everywhere: the
		// replica's own rules expire what is no longer current.
		ref := object.VersionRef{ID: sampleVersionID(2)}
		if result, err := s.DeleteObject(ctx, "in-order", "k", ports.DeleteOptions{Version: &ref, By: bucket.LifecycleActor}); err != nil || result.Removed == nil {
			t.Errorf("removing a noncurrent version of a replica = %+v, %v", result, err)
		}
		// What arrived as a replica says so for good: no acknowledgement
		// of anybody's makes it anything else.
		target(t, s, "site-b", delivery.KindS3)
		newest := object.VersionRef{ID: sampleVersionID(4)}
		result := delivery.Result{Bucket: "in-order", Key: "k", Version: newest, Status: object.ReplicationCompleted}
		if err := s.AckOutbox(ctx, "site-b", delivery.Seq{}, 0, []delivery.Result{result}); err != nil {
			t.Fatalf("AckOutbox: %v", err)
		}
		if got := version(t, s, "in-order", "k", newest); got.Replication != object.ReplicationReplica {
			t.Errorf("after an acknowledgement a replica says %q", got.Replication)
		}
		if _, err := s.SetTags(ctx, "in-order", "k", newest, map[string]string{"a": "b"}); err != nil {
			t.Errorf("tags of a replica version: %v", err)
		}
	})

	t.Run("a multipart replica is completed as a replica", func(t *testing.T) {
		t.Parallel()
		s := replicaBucket(t, "replica")
		upload := SampleUpload("replica", "large", 1)
		upload.ReplicaFrom = sourceID
		if err := s.CreateUpload(ctx, upload); err != nil {
			t.Fatalf("a replica upload: %v", err)
		}
		if got, err := s.GetUpload(ctx, "replica", "large", upload.ID); err != nil || got.ReplicaFrom != sourceID {
			t.Errorf("GetUpload = from %q, %v", got.ReplicaFrom, err)
		}
		part := SamplePart(upload, 1, 500, 1)
		if _, err := s.PutPart(ctx, "replica", "large", upload.ID, part); err != nil {
			t.Fatal(err)
		}
		completion := completionOf(upload, 30, part)
		// Without saying which version of the source it is, and from
		// another source, it is not completed.
		if _, err := s.CompleteUpload(ctx, completion); !errors.Is(err, domain.ErrInvalidBucketState) {
			t.Errorf("a replica upload completed as an ordinary one = %v, want %v", err, domain.ErrInvalidBucketState)
		}
		completion.Replica = &object.Replica{From: otherSourceID, Seq: 44}
		if _, err := s.CompleteUpload(ctx, completion); !errors.Is(err, domain.ErrInvalidBucketState) {
			t.Errorf("a replica upload completed for another source = %v, want %v", err, domain.ErrInvalidBucketState)
		}
		completion.Replica = &object.Replica{From: sourceID, Seq: 44}
		result, err := s.CompleteUpload(ctx, completion)
		if err != nil {
			t.Fatalf("CompleteUpload as a replica: %v", err)
		}
		if stored := result.Stored; stored.Seq != 44 || stored.VersionID != completion.VersionID || !stored.ModifiedAt.Equal(completion.At) ||
			stored.Replication != object.ReplicationReplica || stored.Replica != nil || stored.Size != 500 || !stored.Multipart() {
			t.Errorf("a multipart replica is stored as %+v", stored)
		}
		holds(t, s, "replica", "a multipart replica", bucket.UsageTotals{Objects: 1, CurrentBytes: 500, Versions: 1, StoredBytes: 500})
	})

	t.Run("a promoted bucket takes writes again and they are the newest", func(t *testing.T) {
		t.Parallel()
		s := replicaBucket(t, "replica")
		// The source had reached sequences far beyond anything here.
		far := uint64(1) << 40
		put(t, s, replicaOf("replica", "k", 1, far))
		put(t, s, replicaOf("replica", "k", 2, far+7))
		if got := usage(t, s, "replica").Usage; got.High != far+7 {
			t.Errorf("the greatest sequence of the replica is %d, want %d", got.High, far+7)
		}
		// A quota of the replica is its own deployment's, and holds for
		// what its source sends.
		if err := s.SetBucketQuota(ctx, "replica", 100, noAudit); err != nil {
			t.Fatal(err)
		}
		over := replicaOf("replica", "big", 50, far+8) // 500 bytes
		if _, err := s.PutVersion(ctx, over); !errors.Is(err, domain.ErrQuotaExceeded) {
			t.Errorf("a replica past the quota of its bucket = %v, want %v", err, domain.ErrQuotaExceeded)
		}
		if err := s.SetBucketQuota(ctx, "replica", 0, noAudit); err != nil {
			t.Fatal(err)
		}
		// Naming the source it has changes nothing, also now that it
		// holds versions.
		if err := s.SetBucketReplica(ctx, "replica", sourceID, noAudit); err != nil {
			t.Errorf("naming the source a replica has: %v", err)
		}
		// Another source would find versions here.
		if err := s.SetBucketReplica(ctx, "replica", otherSourceID, noAudit); !errors.Is(err, domain.ErrBucketNotEmpty) {
			t.Errorf("another source for a replica that holds versions = %v, want %v", err, domain.ErrBucketNotEmpty)
		}

		// Promotion: replica mode off.
		if err := s.SetBucketReplica(ctx, "replica", "", noAudit); err != nil {
			t.Fatalf("promoting: %v", err)
		}
		if b, err := s.GetBucket(ctx, "replica"); err != nil || b.ReplicaFrom != "" || b.Versioning != bucket.VersioningEnabled {
			t.Errorf("GetBucket after promotion = %+v, %v", b, err)
		}
		// From now on nothing more is taken from the source.
		if _, err := s.PutVersion(ctx, replicaOf("replica", "k", 3, far+9)); !errors.Is(err, domain.ErrInvalidBucketState) {
			t.Errorf("a replica put after promotion = %v, want %v", err, domain.ErrInvalidBucketState)
		}
		// And a write made here is the newest version of its key, whatever
		// sequences the source had reached.
		local := put(t, s, sized("replica", "k", 4, 1))
		if local.Seq <= far+7 || local.Replication != "" {
			t.Errorf("a version made after promotion has the sequence %d and says %q", local.Seq, local.Replication)
		}
		if current, err := s.GetObject(ctx, "replica", "k"); err != nil || current.VersionID != local.VersionID {
			t.Errorf("the current version after promotion = %s, %v; want %s", current.VersionID, err, local.VersionID)
		}
		marker, err := s.DeleteObject(ctx, "replica", "k", ports.DeleteOptions{MarkerID: sampleVersionID(90), At: contractTime})
		if err != nil || marker.Marker == nil || marker.Marker.Seq <= local.Seq {
			t.Errorf("a delete after promotion = %+v, %v", marker.Marker, err)
		}
		if _, err := s.GetObject(ctx, "replica", "k"); !errors.Is(err, domain.ErrObjectNotFound) {
			t.Errorf("the object behind a marker made after promotion: %v", err)
		}
	})

	t.Run("no replica is at a sequence nothing can be above", func(t *testing.T) {
		t.Parallel()
		s := replicaBucket(t, "replica")
		// A sequence above the bound is refused, and nothing of it is
		// kept: a replica at the last number there is would leave no
		// sequence for a version made here.
		for _, seq := range []uint64{object.MaxReplicaSeq + 1, 1 << 63, math.MaxUint64} {
			if _, err := s.PutVersion(ctx, replicaOf("replica", "k", 1, seq)); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("a replica at the sequence %d = %v, want %v", seq, err, domain.ErrInvalidArgument)
			}
			_, err := s.DeleteObject(ctx, "replica", "k", ports.DeleteOptions{
				MarkerID: sampleVersionID(41), At: contractTime, Replica: &object.Replica{From: sourceID, Seq: seq},
			})
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("a replica marker at the sequence %d = %v, want %v", seq, err, domain.ErrInvalidArgument)
			}
		}
		if got := usage(t, s, "replica").Usage; got.High != 0 || got.Versions != 0 {
			t.Errorf("a refused replica left %+v", got)
		}
		// The greatest sequence a replica can have is taken, and a version
		// made here after promotion is above it.
		put(t, s, replicaOf("replica", "k", 2, object.MaxReplicaSeq))
		if err := s.SetBucketReplica(ctx, "replica", "", noAudit); err != nil {
			t.Fatalf("promoting: %v", err)
		}
		local := put(t, s, sized("replica", "k", 3, 1))
		if local.Seq != object.MaxReplicaSeq+1 {
			t.Errorf("a version made above the greatest replica has the sequence %d, want %d", local.Seq, uint64(object.MaxReplicaSeq+1))
		}
		if current, err := s.GetObject(ctx, "replica", "k"); err != nil || current.VersionID != local.VersionID {
			t.Errorf("the current version = %s, %v; want %s", current.VersionID, err, local.VersionID)
		}
	})

	t.Run("a locked replica version cannot be removed", func(t *testing.T) {
		t.Parallel()
		s := open(t)
		if err := s.CreateBucket(ctx, bucket.Bucket{Name: "vault", CreatedAt: contractTime, ObjectLock: true}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetBucketReplica(ctx, "vault", sourceID, noAudit); err != nil {
			t.Fatalf("SetBucketReplica: %v", err)
		}
		// The retention travels with the version, and is the replica's to
		// keep: nobody removes it, with a bypass or without.
		locked := replicaOf("vault", "ledger", 1, 10)
		locked.Lock = object.Lock{Retention: &object.Retention{Mode: object.Compliance, Until: time.Now().Add(24 * time.Hour).UTC()}}
		stored := put(t, s, locked)
		if stored.Lock.Retention == nil || stored.Lock.Retention.Mode != object.Compliance {
			t.Fatalf("the retention of a replica = %+v", stored.Lock)
		}
		ref := stored.Ref()
		for _, bypass := range []bool{false, true} {
			if _, err := s.DeleteObject(ctx, "vault", "ledger", ports.DeleteOptions{Version: &ref, BypassGovernance: bypass}); !errors.Is(err, domain.ErrObjectLocked) {
				t.Errorf("removing a compliance-locked replica (bypass %v) = %v, want %v", bypass, err, domain.ErrObjectLocked)
			}
		}
		// Not after promotion either.
		if err := s.SetBucketReplica(ctx, "vault", "", noAudit); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeleteObject(ctx, "vault", "ledger", ports.DeleteOptions{Version: &ref, BypassGovernance: true}); !errors.Is(err, domain.ErrObjectLocked) {
			t.Errorf("removing a compliance-locked version after promotion = %v, want %v", err, domain.ErrObjectLocked)
		}
		if got := version(t, s, "vault", "ledger", ref); got.VersionID != stored.VersionID {
			t.Errorf("the locked version = %+v", got)
		}
	})

	t.Run("the record of a resync is kept", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "photos", "other")
		target(t, s, "site-b", delivery.KindS3)
		if got, err := s.GetResync(ctx, "photos"); err != nil || got != (bucket.Resync{}) {
			t.Errorf("GetResync of a bucket that was never resynced = %+v, %v", got, err)
		}
		running := bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning}
		passes := []bucket.Resync{
			running,
			{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning, Key: "k/500", Version: object.VersionRef{ID: sampleVersionID(1)}.String(), Scanned: 500, Sent: 20},
			{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning, Key: "k/999", Version: object.NullVersion, Scanned: 1000, Sent: 21},
			{Bucket: "photos", Target: "site-b", State: bucket.ResyncDone, Key: "k/999", Version: object.NullVersion, Scanned: 1200, Sent: 21},
			// Started again: a record replaces the one before, whole.
			running,
		}
		for _, pass := range passes {
			if err := s.PutResync(ctx, pass); err != nil {
				t.Fatalf("PutResync %+v: %v", pass, err)
			}
			if got, err := s.GetResync(ctx, "photos"); err != nil || got != pass {
				t.Fatalf("GetResync = %+v, %v; want %+v", got, err, pass)
			}
		}
		if got, err := s.GetResync(ctx, "other"); err != nil || got != (bucket.Resync{}) {
			t.Errorf("the resync of another bucket = %+v, %v", got, err)
		}
		for name, tt := range map[string]struct {
			record bucket.Resync
			want   error
		}{
			"a bucket that is not there": {bucket.Resync{Bucket: "missing", Target: "site-b", State: bucket.ResyncRunning}, domain.ErrBucketNotFound},
			"a target that is not there": {bucket.Resync{Bucket: "photos", Target: "missing", State: bucket.ResyncRunning}, domain.ErrEntityNotFound},
			"a state there is not":       {bucket.Resync{Bucket: "photos", Target: "site-b", State: "paused"}, domain.ErrInvalidArgument},
			"more sent than looked at":   {bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning, Scanned: 1, Sent: 2}, domain.ErrInvalidArgument},
			"a key without its version":  {bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning, Key: "k"}, domain.ErrInvalidArgument},
			"no record at all":           {bucket.Resync{}, domain.ErrInvalidArgument},
		} {
			if err := s.PutResync(ctx, tt.record); !errors.Is(err, tt.want) {
				t.Errorf("%s: PutResync = %v, want %v", name, err, tt.want)
			}
		}
		if got, err := s.GetResync(ctx, "photos"); err != nil || got != running {
			t.Errorf("the record after refusals = %+v, %v", got, err)
		}
		if _, err := s.GetResync(ctx, "missing"); !errors.Is(err, domain.ErrBucketNotFound) {
			t.Errorf("GetResync of a bucket that is not there = %v", err)
		}
		// The record goes with its bucket.
		if err := s.DeleteBucket(ctx, "photos"); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateBucket(ctx, bucket.Bucket{Name: "photos", CreatedAt: contractTime}); err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetResync(ctx, "photos"); err != nil || got != (bucket.Resync{}) {
			t.Errorf("the resync of a bucket made again = %+v, %v", got, err)
		}
	})

	t.Run("changes of an administrator carry their audit record", func(t *testing.T) {
		t.Parallel()
		s := withBucket(t, "b")
		versioned(t, s, "b")
		trail := func() []audit.Record {
			t.Helper()
			records, err := s.ListAudit(ctx, 0, 1000)
			if err != nil {
				t.Fatalf("ListAudit: %v", err)
			}
			return records
		}
		hook := SampleTarget("hook", delivery.KindWebhook)
		steps := []struct {
			action, resource string
			change           func(record audit.Record) error
		}{
			{"admin:PutBucketQuota", "arn:aws:s3:::b", func(r audit.Record) error { return s.SetBucketQuota(ctx, "b", 100, r) }},
			{"admin:PutTarget", "target/hook", func(r audit.Record) error { return s.PutTarget(ctx, hook, r) }},
			{"admin:PutBucketReplicaMode", "arn:aws:s3:::b", func(r audit.Record) error { return s.SetBucketReplica(ctx, "b", sourceID, r) }},
			{"admin:DeleteTarget", "target/hook", func(r audit.Record) error { return s.DeleteTarget(ctx, "hook", r) }},
			{"admin:DeleteBucketQuota", "arn:aws:s3:::b", func(r audit.Record) error { return s.SetBucketQuota(ctx, "b", 0, r) }},
		}
		for i, step := range steps {
			if err := step.change(auditOf(step.action, step.resource)); err != nil {
				t.Fatalf("%s: %v", step.action, err)
			}
			records := trail()
			if len(records) != i+1 {
				t.Fatalf("after %s the trail has %d records, want %d", step.action, len(records), i+1)
			}
			last := records[i]
			if last.Action != step.action || last.Resource != step.resource || last.Actor != "root" || last.Outcome != audit.Succeeded || last.Hash == "" {
				t.Errorf("the record of %s = %+v", step.action, last)
			}
			if i > 0 && last.PreviousHash != records[i-1].Hash {
				t.Errorf("the record of %s is not chained to the one before", step.action)
			}
		}
		// A change that was refused leaves none, and neither does one
		// made without an actor.
		refused := map[string]error{
			"a quota of a missing bucket":   s.SetBucketQuota(ctx, "missing", 10, auditOf("admin:PutBucketQuota", "arn:aws:s3:::missing")),
			"a quota that is none":          s.SetBucketQuota(ctx, "b", -1, auditOf("admin:PutBucketQuota", "arn:aws:s3:::b")),
			"a target that cannot be kept":  s.PutTarget(ctx, delivery.Target{Name: "x"}, auditOf("admin:PutTarget", "target/x")),
			"deleting a target that is not": s.DeleteTarget(ctx, "hook", auditOf("admin:DeleteTarget", "target/hook")),
			"a source that is none":         s.SetBucketReplica(ctx, "b", "site a", auditOf("admin:PutBucketReplicaMode", "arn:aws:s3:::b")),
		}
		for name, err := range refused {
			if err == nil {
				t.Errorf("%s was not refused", name)
			}
		}
		if err := s.PutTarget(ctx, hook, noAudit); err != nil {
			t.Fatal(err)
		}
		if err := s.SetBucketQuota(ctx, "b", 5, noAudit); err != nil {
			t.Fatal(err)
		}
		if records := trail(); len(records) != len(steps) {
			t.Errorf("after refused and unaudited changes the trail has %d records, want %d", len(records), len(steps))
		}
		// A record that cannot be kept refuses the change it came with.
		bad := audit.Record{Actor: "root", Action: "admin:DeleteTarget", Outcome: "perhaps"}
		if err := s.DeleteTarget(ctx, "hook", bad); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("a change with an audit record that cannot be kept = %v, want %v", err, domain.ErrInvalidArgument)
		}
		if _, err := s.GetTarget(ctx, "hook"); err != nil {
			t.Errorf("the target after that refusal: %v", err)
		}
		if err := s.SetBucketReplica(ctx, "b", "", bad); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("a promotion with an audit record that cannot be kept = %v, want %v", err, domain.ErrInvalidArgument)
		}
		if b, err := s.GetBucket(ctx, "b"); err != nil || b.ReplicaFrom != sourceID {
			t.Errorf("the bucket after that refusal = replica of %q, %v", b.ReplicaFrom, err)
		}
	})
}

// OutboxBoundContract is what every store of the outbox must do at its
// bound (ADR-0023). open returns a new, empty store, as for Schema5Contract,
// whose outbox holds bound rows and no more - a store's own test lowers its
// bound to that; bound is at least 4.
func OutboxBoundContract(t *testing.T, bound int, open func(t *testing.T) Schema5) {
	t.Helper()
	ctx := context.Background()
	noAudit := audit.Record{}
	if bound < 4 {
		t.Fatalf("OutboxBoundContract needs a bound of at least 4, not %d", bound)
	}
	s := open(t)
	if err := s.CreateBucket(ctx, bucket.Bucket{Name: "photos", CreatedAt: contractTime}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBucketVersioning(ctx, "photos", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	for _, tg := range []delivery.Target{SampleTarget("dead", delivery.KindWebhook), SampleTarget("live", delivery.KindWebhook), SampleTarget("site-b", delivery.KindS3)} {
		if err := s.PutTarget(ctx, tg, noAudit); err != nil {
			t.Fatal(err)
		}
	}
	every := []delivery.Event{delivery.EventPut}
	notification, err := bucket.NotificationConfig{Rules: []bucket.NotificationRule{
		{ID: "dead", As: bucket.NotifyAsQueue, Target: "dead", On: every},
		{ID: "live", As: bucket.NotifyAsQueue, Target: "live", On: every, Prefix: "live/"},
	}}.Document()
	if err != nil {
		t.Fatal(err)
	}
	replication, err := bucket.ReplicationConfig{Target: "site-b", Bucket: "copy", Rules: []bucket.ReplicationRule{{ID: "r", Enabled: true, Prefix: "live/"}}}.Document()
	if err != nil {
		t.Fatal(err)
	}
	for kind, document := range map[string][]byte{bucket.ConfigNotification: notification, bucket.ConfigReplication: replication} {
		if err := s.SetBucketConfig(ctx, "photos", kind, document); err != nil {
			t.Fatalf("SetBucketConfig %s: %v", kind, err)
		}
	}
	state := func() (ports.OutboxState, map[string]delivery.Cursor) {
		t.Helper()
		got, err := s.OutboxCursors(ctx)
		if err != nil {
			t.Fatalf("OutboxCursors: %v", err)
		}
		cursors := map[string]delivery.Cursor{}
		for _, c := range got.Cursors {
			cursors[c.Target] = c
		}
		return got, cursors
	}
	// The bound as the store counts its rows.
	rows := uint64(0)
	for range bound {
		rows++
	}
	fill := byte(0)
	put := func(key string) {
		t.Helper()
		fill++
		v := SampleVersion("photos", key, fill)
		if _, err := s.PutVersion(ctx, v); err != nil {
			// A target that takes nothing never refuses a write.
			t.Fatalf("PutVersion %s with the outbox at %d rows: %v", key, bound, err)
		}
	}

	// Two rows that all three targets are consumers of, then rows for the
	// dead target alone until the outbox is full.
	put("live/1")
	put("live/2")
	for i := 2; i < bound; i++ {
		put(fmt.Sprintf("other/%d", i))
	}
	full, cursors := state()
	if full.Rows != rows || full.Dropped != 0 || cursors["dead"].Lost != 0 {
		t.Fatalf("a full outbox = %+v", full)
	}
	// The live target is delivered its first row and acknowledges it; the
	// other deployment and the dead target acknowledge nothing.
	mine, _, err := s.ReadOutbox(ctx, "live", delivery.Seq{}, 10)
	if err != nil || len(mine) != 2 {
		t.Fatalf("ReadOutbox = %d rows, %v", len(mine), err)
	}
	if err := s.AckOutbox(ctx, "live", mine[0].Seq, 0, nil); err != nil {
		t.Fatal(err)
	}

	// One row more: the oldest goes. The live target had passed it; the
	// dead one and the other deployment had not.
	put("other/more")
	after, cursors := state()
	if after.Rows != rows || after.Dropped != 1 {
		t.Fatalf("one row past the bound: %d rows, %d dropped; want %d and 1", after.Rows, after.Dropped, bound)
	}
	if c := cursors["live"]; c.Lost != 0 || c.ResyncNeeded {
		t.Errorf("a target that had passed the dropped row = %+v", c)
	}
	if c := cursors["dead"]; c.Lost != 1 || c.ResyncNeeded {
		t.Errorf("a notification target that had not passed the dropped row = %+v", c)
	}
	if c := cursors["site-b"]; c.Lost != 1 || !c.ResyncNeeded {
		t.Errorf("another deployment that had not passed the dropped row = %+v", c)
	}
	// The second: nobody had passed it. It was for all three.
	put("other/more-2")
	// The third was for the dead target alone: the others lose nothing.
	put("other/more-3")
	after, cursors = state()
	if after.Rows != rows || after.Dropped != 3 {
		t.Fatalf("three rows past the bound: %d rows, %d dropped", after.Rows, after.Dropped)
	}
	if cursors["live"].Lost != 1 || cursors["dead"].Lost != 3 || cursors["site-b"].Lost != 2 {
		t.Errorf("lost: live %d, dead %d, the other deployment %d; want 1, 3, 2", cursors["live"].Lost, cursors["dead"].Lost, cursors["site-b"].Lost)
	}
	// What was dropped is not read any more; what is left is, in order.
	left, _, err := s.ReadOutbox(ctx, "dead", delivery.Seq{}, delivery.MaxOutboxRead)
	if err != nil || len(left) != min(bound, delivery.MaxOutboxRead) {
		t.Fatalf("after rows were dropped the dead target is left %d rows, %v", len(left), err)
	}
	if first := left[0].Key; first == "live/1" || first == "live/2" || first == "other/2" {
		t.Errorf("after rows were dropped the first that is left is of %q", first)
	}
	// The versions themselves are all there: nothing but the telling was
	// lost.
	for _, key := range []string{"live/1", "live/2", "other/more-3"} {
		if _, err := s.GetObject(ctx, "photos", key); err != nil {
			t.Errorf("the object %s after its row was dropped: %v", key, err)
		}
	}

	// A resync that has been through everything makes the other
	// deployment whole again; what was lost stays counted.
	record := bucket.Resync{Bucket: "photos", Target: "site-b", State: bucket.ResyncRunning}
	if err := s.PutResync(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, cursors = state(); !cursors["site-b"].ResyncNeeded {
		t.Error("a resync that is still running cleared the need for one")
	}
	record.State = bucket.ResyncDone
	if err := s.PutResync(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, cursors = state(); cursors["site-b"].ResyncNeeded || cursors["site-b"].Lost != 2 {
		t.Errorf("after a resync the cursor of the other deployment = %+v", cursors["site-b"])
	}
}
