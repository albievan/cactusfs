package app_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// sendingSource is the ID of the deployment the tests of the replicator send
// from.
const sendingSource = "source-0001"

// replicatorWorld is a deployment with the bucket "b" (it keeps versions)
// sent to the target "site-b", whose bucket "copies" is a replica of this
// deployment at another one, in memory (porttest.MemSite).
type replicatorWorld struct {
	*fixture
	target     delivery.Target
	dest       *porttest.MemMetadata
	site       *porttest.MemSite
	replicator *app.Replicator
}

// siteRecord is the target of another deployment, open to the buckets named.
func siteRecord(name string, buckets ...string) delivery.Target {
	return delivery.Target{
		Name: name, Kind: delivery.KindS3, Endpoint: "https://" + name + ".example", Region: "eu-1", Buckets: buckets,
		Auth:   delivery.Auth{Mode: delivery.AuthSigV4, ID: "AKIDSITEBEXAMPLE0001"},
		Secret: iam.SealedSecret{KMSKeyID: "test:1", WrappedKey: []byte("wrapped"), Nonce: make([]byte, 12), Ciphertext: []byte("sealed")},
		// A time the fixture's clock is past.
		CreatedAt: testNow.Add(-time.Hour),
	}
}

// newReplicatorWorld makes the world, with the one rule given; markers says
// whether it copies delete markers.
func newReplicatorWorld(t *testing.T, markers bool) *replicatorWorld {
	t.Helper()
	return newReplicatorWorldLocked(t, markers, true)
}

// newReplicatorWorldLocked makes the world; destLock says whether the bucket
// there has Object Lock.
func newReplicatorWorldLocked(t *testing.T, markers, destLock bool) *replicatorWorld {
	t.Helper()
	ctx := context.Background()
	w := &replicatorWorld{fixture: newFixture(t), dest: porttest.NewMemMetadata()}
	// Both stores judge retentions by the fixture's clock.
	w.meta.Now, w.dest.Now = w.clock.Now, w.clock.Now
	// Both buckets have Object Lock, so that retentions and legal holds
	// travel.
	if err := w.meta.CreateBucket(ctx, bucket.Bucket{Name: "b", CreatedAt: testNow, ObjectLock: true}); err != nil {
		t.Fatal(err)
	}
	if err := w.meta.SetBucketVersioning(ctx, "b", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if err := w.meta.PutTarget(ctx, siteRecord("site-b", "b"), audit.Record{}); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	target, err := w.meta.GetTarget(ctx, "site-b")
	if err != nil {
		t.Fatal(err)
	}
	w.target = target
	w.configure(t, bucket.ReplicationConfig{Target: "site-b", Bucket: "copies", Rules: []bucket.ReplicationRule{{ID: "all", Enabled: true, Markers: markers}}})
	if err := w.dest.CreateBucket(ctx, bucket.Bucket{Name: "copies", CreatedAt: testNow, ObjectLock: destLock}); err != nil {
		t.Fatal(err)
	}
	if err := w.dest.SetBucketVersioning(ctx, "copies", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if err := w.dest.SetBucketReplica(ctx, "copies", sendingSource, audit.Record{}); err != nil {
		t.Fatalf("SetBucketReplica: %v", err)
	}
	w.site = porttest.NewMemSite(w.dest)
	w.replicator = w.replicatorWith(nil, 0)
	return w
}

// replicatorWith returns a replicator of the world that sends through site
// (the world's own when nil), at rate bytes a second (0: no bound).
func (w *replicatorWorld) replicatorWith(site ports.SiteClient, rate int64) *app.Replicator {
	if site == nil {
		site = w.site
	}
	return app.NewReplicator(app.ReplicatorConfig{
		Objects: w.objects, Buckets: w.meta, Source: sendingSource, Clock: w.clock, RateBytes: rate,
		Sites: app.SiteSourceFunc(func(context.Context, delivery.Target) (ports.SiteClient, error) { return site, nil }),
	})
}

// configure stores the replication configuration of "b".
func (w *replicatorWorld) configure(t *testing.T, config bucket.ReplicationConfig) {
	t.Helper()
	var document []byte
	if !config.Empty() {
		var err error
		if document, err = config.Document(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.meta.SetBucketConfig(context.Background(), "b", bucket.ConfigReplication, document); err != nil {
		t.Fatalf("SetBucketConfig: %v", err)
	}
}

// rows returns the rows of the outbox for the target, oldest first.
func (w *replicatorWorld) rows() []delivery.Item {
	var out []delivery.Item
	for _, item := range w.meta.OutboxRows() {
		if _, ok := item.For(w.target.Name); ok {
			out = append(out, item)
		}
	}
	return out
}

// last returns the last row of the outbox for the target.
func (w *replicatorWorld) last(t *testing.T) delivery.Item {
	t.Helper()
	rows := w.rows()
	if len(rows) == 0 {
		t.Fatal("no row for the target")
	}
	return rows[len(rows)-1]
}

// settle acknowledges an item with the status it gave, as the delivery
// worker does, and returns the version's status afterwards.
func (w *replicatorWorld) settle(t *testing.T, item delivery.Item, status object.ReplicationStatus) object.ReplicationStatus {
	t.Helper()
	ctx := context.Background()
	var results []delivery.Result
	if status != "" {
		results = []delivery.Result{{Seq: item.Seq, Status: status}}
	}
	if _, err := w.meta.AckOutbox(ctx, w.target.Name, item.Seq, 0, results); err != nil {
		t.Fatalf("AckOutbox: %v", err)
	}
	v, err := w.meta.GetVersion(ctx, item.Bucket, item.Key, item.Version)
	if err != nil {
		t.Fatalf("the version after the acknowledgement: %v", err)
	}
	return v.Replication
}

// putIn puts an object into "b" as the root identity.
func (w *replicatorWorld) putIn(t *testing.T, key string, data []byte, sse object.SSE) object.Version {
	t.Helper()
	v, err := w.objects.Put(context.Background(), porttest.Root(), app.PutInput{
		Bucket: "b", Key: key, Body: bytes.NewReader(data), Size: int64(len(data)), SSE: sse, ContentType: "text/plain",
		UserMetadata: map[string]string{"owner": "albie"},
	})
	if err != nil {
		t.Fatalf("Put %s: %v", key, err)
	}
	return v
}

// called returns the calls the site was given, by name only.
func (w *replicatorWorld) called() []string {
	var out []string
	for _, call := range w.site.Calls() {
		name, _, _ := strings.Cut(call, " ")
		out = append(out, name)
	}
	return out
}

// TestReplicatorSendsAVersion: a version is read as the replication service
// and sent in one PUT, with its ID, its time, its entity tag and its
// sequence; COMPLETED is reported, and the acknowledgement makes the
// version COMPLETED. Sent again - a new leader sends the last batch again -
// it is found there and not sent: COMPLETED again, no byte sent twice.
func TestReplicatorSendsAVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newReplicatorWorld(t, false)
	data := randomBytes(t, 70_000)
	v := w.putIn(t, "docs/a.txt", data, object.SSE{})
	if v.Replication != object.ReplicationPending {
		t.Fatalf("a version a rule matched is %q, want PENDING", v.Replication)
	}
	item := w.last(t)
	status, err := w.replicator.Deliver(ctx, w.target, item)
	if err != nil || status != object.ReplicationCompleted {
		t.Fatalf("Deliver = %q, %v; want COMPLETED", status, err)
	}
	got, err := w.dest.GetVersion(ctx, "copies", v.Key, v.Ref())
	if err != nil {
		t.Fatalf("the copy: %v", err)
	}
	if got.VersionID != v.VersionID || !got.ModifiedAt.Equal(v.ModifiedAt) || got.ETag != v.ETag || got.Size != v.Size ||
		got.Seq != v.Seq || got.Replication != object.ReplicationReplica || got.ContentType != "text/plain" || got.UserMetadata["owner"] != "albie" {
		t.Errorf("the copy is %+v; the version is %+v", got, v)
	}
	if w.site.BytesSent() != int64(len(data)) {
		t.Errorf("%d bytes sent, want %d", w.site.BytesSent(), len(data))
	}
	if want := []string{"Check", "HeadVersion", "PutReplica"}; !slices.Equal(w.called(), want) {
		t.Errorf("calls = %v, want %v", w.called(), want)
	}
	if got := w.settle(t, item, status); got != object.ReplicationCompleted {
		t.Errorf("the version after the acknowledgement is %q", got)
	}
	// Again: the probe is not asked again within the minute, the version
	// is found, and nothing is sent.
	status, err = w.replicator.Deliver(ctx, w.target, item)
	if err != nil || status != object.ReplicationCompleted {
		t.Errorf("Deliver again = %q, %v", status, err)
	}
	if want := []string{"Check", "HeadVersion", "PutReplica", "HeadVersion"}; !slices.Equal(w.called(), want) {
		t.Errorf("calls = %v, want %v", w.called(), want)
	}
	if w.site.BytesSent() != int64(len(data)) {
		t.Errorf("%d bytes sent in all, want %d: the second send sent the version again", w.site.BytesSent(), len(data))
	}
	// The probe is asked again once its answer is a minute old.
	w.clock.Advance(time.Minute)
	if _, err := w.replicator.Deliver(ctx, w.target, item); err != nil {
		t.Fatal(err)
	}
	if calls := w.called(); calls[len(calls)-2] != "Check" {
		t.Errorf("calls after a minute = %v, want the probe asked again", calls)
	}
}

// TestReplicatorSendsTheSameParts: a version of parts is sent as an upload
// of the same parts - each with its number and its size, in order - after
// the uploads this deployment left of the key there were aborted, and
// completed as the version; COMPLETED.
func TestReplicatorSendsTheSameParts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newReplicatorWorld(t, false)
	v, parts, content := w.threeParts(t, "movie")
	item := w.last(t)
	if item.Event != delivery.EventComplete || item.Version != v.Ref() {
		t.Fatalf("the row is %+v", item)
	}
	// What two attempts that were cut off left behind there.
	for i := range 2 {
		upload := object.Upload{
			Bucket: "copies", Key: "movie", ID: object.ID{byte(i + 1)}, InitiatedAt: testNow, ReplicaFrom: sendingSource,
			Encryption: object.Encryption{KeyMode: object.KeyModePartHKDF},
		}
		if err := w.dest.CreateUpload(ctx, upload); err != nil {
			t.Fatalf("CreateUpload: %v", err)
		}
	}
	status, err := w.replicator.Deliver(ctx, w.target, item)
	if err != nil || status != object.ReplicationCompleted {
		t.Fatalf("Deliver = %q, %v", status, err)
	}
	want := []string{"Check", "HeadVersion", "ListReplicaUploads", "AbortUpload", "AbortUpload", "CreateUpload", "PutPart", "PutPart", "PutPart", "CompleteReplica"}
	if !slices.Equal(w.called(), want) {
		t.Errorf("calls = %v, want %v", w.called(), want)
	}
	if w.site.BytesSent() != int64(len(content)) {
		t.Errorf("%d bytes sent, want %d", w.site.BytesSent(), len(content))
	}
	got, err := w.dest.GetVersion(ctx, "copies", "movie", v.Ref())
	if err != nil || got.ETag != v.ETag || got.Seq != v.Seq || got.Size != v.Size {
		t.Fatalf("the copy is %+v, %v; the version %+v", got, err, v)
	}
	sent, err := w.dest.ObjectParts(ctx, "copies", got.DataID, 0, 10)
	if err != nil || len(sent) != len(parts) {
		t.Fatalf("the parts there = %+v, %v", sent, err)
	}
	for i, part := range sent {
		if part.Number != parts[i].Number || part.Size != parts[i].Size {
			t.Errorf("part %d there is number %d of %d bytes, want number %d of %d", i, part.Number, part.Size, parts[i].Number, parts[i].Size)
		}
	}
	uploads := 0
	if err := w.dest.ScanUploads(ctx, "copies", ports.UploadCursor{}, func(object.Upload) ports.ScanControl {
		uploads++
		return ports.ScanControl{}
	}); err != nil {
		t.Fatal(err)
	}
	if uploads != 0 {
		t.Errorf("%d uploads left there", uploads)
	}
}

// TestReplicatorMarkers: a delete marker is in the outbox for the target, and
// sent, only when the deciding rule copies markers and a caller made it -
// never when the lifecycle service did, which now says so (DeleteOptions.By).
// One whose rule stopped copying markers since is passed and not sent.
func TestReplicatorMarkers(t *testing.T) {
	t.Parallel()
	lifecycle := iam.Principal{Service: iam.ServiceLifecycle}
	for name, tt := range map[string]struct {
		markers bool
		by      iam.Principal
		row     bool
	}{
		"a caller's, the rule copies markers":     {markers: true, by: porttest.Root(), row: true},
		"a caller's, the rule copies none":        {markers: false, by: porttest.Root()},
		"the lifecycle service's, copies markers": {markers: true, by: lifecycle},
		"the lifecycle service's, copies none":    {markers: false, by: lifecycle},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			w := newReplicatorWorld(t, tt.markers)
			v := w.putIn(t, "k", []byte("version"), object.SSE{})
			if status, err := w.replicator.Deliver(ctx, w.target, w.last(t)); err != nil || status != object.ReplicationCompleted {
				t.Fatalf("the version: %q, %v", status, err)
			}
			before := len(w.rows())
			out, err := w.objects.Delete(ctx, tt.by, app.DeleteInput{Bucket: "b", Key: "k"})
			if err != nil || out.Marker == nil {
				t.Fatalf("Delete = %+v, %v", out, err)
			}
			rows := w.rows()
			if got := len(rows) > before; got != tt.row {
				t.Fatalf("a row for the target of the marker = %v, want %v", got, tt.row)
			}
			if !tt.row {
				return
			}
			item := rows[len(rows)-1]
			if item.Event != delivery.EventMarker || item.By != "root" {
				t.Fatalf("the row of the marker is %+v", item)
			}
			status, err := w.replicator.Deliver(ctx, w.target, item)
			if err != nil || status != object.ReplicationCompleted {
				t.Fatalf("Deliver of the marker = %q, %v", status, err)
			}
			copied, err := w.dest.GetVersion(ctx, "copies", "k", out.Marker.Ref())
			if err != nil || !copied.DeleteMarker || copied.Seq != out.Marker.Seq || copied.Replication != object.ReplicationReplica {
				t.Errorf("the marker there is %+v, %v; want the marker %+v", copied, err, out.Marker)
			}
			if _, err := w.dest.GetObject(ctx, "copies", "k"); !errors.Is(err, domain.ErrObjectNotFound) {
				t.Errorf("the object there under the marker: %v", err)
			}
			if _, err := w.dest.GetVersion(ctx, "copies", "k", v.Ref()); err != nil {
				t.Errorf("the version under the marker there: %v", err)
			}
			// The rule stops copying markers: a marker's row that waited is
			// passed, and not sent.
			w.configure(t, bucket.ReplicationConfig{Target: "site-b", Bucket: "copies", Rules: []bucket.ReplicationRule{{ID: "all", Enabled: true}}})
			calls := len(w.site.Calls())
			status, err = w.replicator.Deliver(ctx, w.target, item)
			if status != "" || delivery.ClassOf(err) != delivery.ClassRejected {
				t.Errorf("a marker no rule copies now = %q, %v; want passed", status, err)
			}
			if len(w.site.Calls()) != calls {
				t.Errorf("a marker no rule copies was sent: %v", w.site.Calls()[calls:])
			}
		})
	}
}

// TestALifecycleMarkerIsToldApart: the lifecycle worker's delete says who
// deleted - the lifecycle service - and a caller's says the caller, as the
// put says who put: what a receiver is told, and what keeps the lifecycle
// service's markers from crossing.
func TestALifecycleMarkerIsToldApart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.createBucket(t, "b")
	if err := f.meta.SetBucketVersioning(ctx, "b", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	var seen []string
	index := byIndex{ObjectIndex: f.meta, seen: &seen}
	objects := f.objectServiceWith(t, func(cfg *app.ObjectServiceConfig) { cfg.Index = index })
	for _, principal := range []iam.Principal{porttest.Root(), {Service: iam.ServiceLifecycle}, {AccessKeyID: "AKIDUSER", User: "alice", Policies: everyS3Action()}} {
		f.put(t, "b", "k", []byte("x"))
		if _, err := objects.Delete(ctx, principal, app.DeleteInput{Bucket: "b", Key: "k"}); err != nil {
			t.Fatalf("Delete as %s: %v", principal.Name(), err)
		}
	}
	if want := []string{"root", bucket.LifecycleActor, "user/alice"}; !slices.Equal(seen, want) {
		t.Errorf("the deletes said they were by %v, want %v", seen, want)
	}
}

// byIndex records who each delete says it is by.
type byIndex struct {
	ports.ObjectIndex
	seen *[]string
}

func (i byIndex) DeleteObject(ctx context.Context, bucketName, key string, o ports.DeleteOptions) (ports.DeleteResult, error) {
	*i.seen = append(*i.seen, o.By)
	return i.ObjectIndex.DeleteObject(ctx, bucketName, key, o)
}

// everyS3Action is a policy that allows every s3: action.
func everyS3Action() []iam.Document {
	return []iam.Document{{Version: iam.PolicyVersion, Statements: []iam.Statement{{Effect: iam.Allow, Actions: []string{"s3:*"}, Resources: []string{"*"}}}}}
}

// scriptedSite is the world's site, but for the calls a test makes answer
// otherwise.
type scriptedSite struct {
	*porttest.MemSite
	check   func() (ports.SiteBucket, error)
	head    func() (string, bool, error)
	put     func() error
	marker  func() error
	created func() error
	listed  func() error
	aborted func() error
	part    func() error
	done    func() error
	tags    func() error
}

func (s *scriptedSite) PutTags(ctx context.Context, bucketName, key string, ref object.VersionRef, tags map[string]string) error {
	if s.tags != nil {
		if err := s.tags(); err != nil {
			return err
		}
	}
	return s.MemSite.PutTags(ctx, bucketName, key, ref, tags)
}

func (s *scriptedSite) ListReplicaUploads(ctx context.Context, bucketName, key string) ([]string, error) {
	if s.listed != nil {
		if err := s.listed(); err != nil {
			return nil, err
		}
	}
	return s.MemSite.ListReplicaUploads(ctx, bucketName, key)
}

func (s *scriptedSite) AbortUpload(ctx context.Context, bucketName, key, uploadID string) error {
	if s.aborted != nil {
		if err := s.aborted(); err != nil {
			return err
		}
	}
	return s.MemSite.AbortUpload(ctx, bucketName, key, uploadID)
}

func (s *scriptedSite) PutPart(ctx context.Context, bucketName, key, uploadID string, number int, size int64, body io.Reader) (string, error) {
	if s.part != nil {
		if err := s.part(); err != nil {
			return "", err
		}
	}
	return s.MemSite.PutPart(ctx, bucketName, key, uploadID, number, size, body)
}

func (s *scriptedSite) CompleteReplica(ctx context.Context, v object.Version, uploadID string, parts []ports.SitePart) error {
	if s.done != nil {
		if err := s.done(); err != nil {
			return err
		}
	}
	return s.MemSite.CompleteReplica(ctx, v, uploadID, parts)
}

func (s *scriptedSite) Check(ctx context.Context, bucketName string) (ports.SiteBucket, error) {
	if s.check != nil {
		return s.check()
	}
	return s.MemSite.Check(ctx, bucketName)
}

func (s *scriptedSite) HeadVersion(ctx context.Context, bucketName, key string, ref object.VersionRef) (string, bool, error) {
	if s.head != nil {
		return s.head()
	}
	return s.MemSite.HeadVersion(ctx, bucketName, key, ref)
}

func (s *scriptedSite) PutReplica(ctx context.Context, v object.Version, body io.Reader) error {
	if s.put != nil {
		_, _ = io.Copy(io.Discard, body)
		return s.put()
	}
	return s.MemSite.PutReplica(ctx, v, body)
}

func (s *scriptedSite) PutMarker(ctx context.Context, v object.Version) error {
	if s.marker != nil {
		return s.marker()
	}
	return s.MemSite.PutMarker(ctx, v)
}

func (s *scriptedSite) CreateUpload(ctx context.Context, v object.Version) (string, error) {
	if s.created != nil {
		if err := s.created(); err != nil {
			return "", err
		}
	}
	return s.MemSite.CreateUpload(ctx, v)
}

// siteFailure is a failure of the class given, as the client of another
// deployment gives one.
func siteFailure(class delivery.Class) error {
	return delivery.Failed(class, errors.New("s3client: the other deployment did not take the request"))
}

// TestReplicatorClasses: what the other deployment answers decides the
// status. Only what is of the version itself - a refusal of the version (a
// 400 or a 413, rejected), a customer's key, other bytes there under its ID
// - is FAILED and passes; every other answer holds the target and reports
// no status: a key that is refused, a bucket that is not there, keeps no
// versions or is no replica of this deployment, a destination that is full,
// and every failure of the network or of either side.
func TestReplicatorClasses(t *testing.T) {
	t.Parallel()
	failing := func(class delivery.Class) func() error { return func() error { return siteFailure(class) } }
	tests := map[string]struct {
		site      func(s *scriptedSite)
		multipart bool
		status    object.ReplicationStatus
		class     delivery.Class
	}{
		"taken":                         {status: object.ReplicationCompleted},
		"refused for itself (400, 413)": {site: func(s *scriptedSite) { s.put = failing(delivery.ClassRejected) }, status: object.ReplicationFailed, class: delivery.ClassRejected},
		"the key is refused":            {site: func(s *scriptedSite) { s.put = failing(delivery.ClassAuth) }, class: delivery.ClassAuth},
		"no such bucket":                {site: func(s *scriptedSite) { s.put = failing(delivery.ClassNoSuchBucket) }, class: delivery.ClassNoSuchBucket},
		"no versions there":             {site: func(s *scriptedSite) { s.put = failing(delivery.ClassVersioning) }, class: delivery.ClassVersioning},
		"no replica there":              {site: func(s *scriptedSite) { s.put = failing(delivery.ClassNotReplica) }, class: delivery.ClassNotReplica},
		"full":                          {site: func(s *scriptedSite) { s.put = failing(delivery.ClassQuota) }, class: delivery.ClassQuota},
		"no connection":                 {site: func(s *scriptedSite) { s.put = failing(delivery.ClassConnect) }, class: delivery.ClassConnect},
		"a handshake that fails":        {site: func(s *scriptedSite) { s.put = failing(delivery.ClassTLS) }, class: delivery.ClassTLS},
		"too slow":                      {site: func(s *scriptedSite) { s.put = failing(delivery.ClassTimeout) }, class: delivery.ClassTimeout},
		"an address not connected to":   {site: func(s *scriptedSite) { s.put = failing(delivery.ClassEgressRefused) }, class: delivery.ClassEgressRefused},
		"a redirect":                    {site: func(s *scriptedSite) { s.put = failing(delivery.ClassRedirect) }, class: delivery.ClassRedirect},
		"the other side fails":          {site: func(s *scriptedSite) { s.put = failing(delivery.ClassServer) }, class: delivery.ClassServer},
		"this side fails":               {site: func(s *scriptedSite) { s.put = failing(delivery.ClassInternal) }, class: delivery.ClassInternal},
		"an error of no class":          {site: func(s *scriptedSite) { s.put = func() error { return errors.New("boom") } }, class: delivery.ClassInternal},
		"the probe: versions suspended": {site: func(s *scriptedSite) {
			s.check = func() (ports.SiteBucket, error) {
				return ports.SiteBucket{Versioning: bucket.VersioningSuspended, ReplicaOf: sendingSource}, nil
			}
		}, class: delivery.ClassVersioning},
		"the probe: a replica of another": {site: func(s *scriptedSite) {
			s.check = func() (ports.SiteBucket, error) {
				return ports.SiteBucket{Versioning: bucket.VersioningEnabled, ReplicaOf: "someone-else"}, nil
			}
		}, class: delivery.ClassNotReplica},
		"the probe: no replica at all": {site: func(s *scriptedSite) {
			s.check = func() (ports.SiteBucket, error) { return ports.SiteBucket{Versioning: bucket.VersioningEnabled}, nil }
		}, class: delivery.ClassNotReplica},
		"the probe: the key is refused": {site: func(s *scriptedSite) {
			s.check = func() (ports.SiteBucket, error) { return ports.SiteBucket{}, siteFailure(delivery.ClassAuth) }
		}, class: delivery.ClassAuth},
		"the probe: no such bucket": {site: func(s *scriptedSite) {
			s.check = func() (ports.SiteBucket, error) { return ports.SiteBucket{}, siteFailure(delivery.ClassNoSuchBucket) }
		}, class: delivery.ClassNoSuchBucket},
		"there already, the same": {site: func(s *scriptedSite) {
			s.head = func() (string, bool, error) { return "", true, nil }
			s.put = failing(delivery.ClassInternal)
		}, status: object.ReplicationCompleted},
		"there already, other bytes": {site: func(s *scriptedSite) {
			s.head = func() (string, bool, error) { return "0123456789abcdef0123456789abcdef", true, nil }
		}, status: object.ReplicationFailed, class: delivery.ClassRejected},
		"the question fails": {site: func(s *scriptedSite) {
			s.head = func() (string, bool, error) { return "", false, siteFailure(delivery.ClassServer) }
		}, class: delivery.ClassServer},
		"parts: refused for itself": {
			multipart: true, site: func(s *scriptedSite) { s.created = failing(delivery.ClassRejected) },
			status: object.ReplicationFailed, class: delivery.ClassRejected,
		},
		"parts: the key is refused": {multipart: true, site: func(s *scriptedSite) { s.created = failing(delivery.ClassAuth) }, class: delivery.ClassAuth},
		"parts: taken":              {multipart: true, status: object.ReplicationCompleted},
		"parts: the uploads cannot be listed": {
			multipart: true, site: func(s *scriptedSite) { s.listed = failing(delivery.ClassServer) },
			class: delivery.ClassServer,
		},
		"parts: a part is refused for itself": {
			multipart: true, site: func(s *scriptedSite) { s.part = failing(delivery.ClassRejected) },
			status: object.ReplicationFailed, class: delivery.ClassRejected,
		},
		"parts: a part does not arrive": {
			multipart: true, site: func(s *scriptedSite) { s.part = failing(delivery.ClassConnect) },
			class: delivery.ClassConnect,
		},
		"parts: the completion fails": {
			multipart: true, site: func(s *scriptedSite) { s.done = failing(delivery.ClassServer) },
			class: delivery.ClassServer,
		},
		"parts: the completion fails, and so does the abort": {multipart: true, site: func(s *scriptedSite) {
			s.done, s.aborted = failing(delivery.ClassServer), failing(delivery.ClassServer)
		}, class: delivery.ClassServer},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			w := newReplicatorWorld(t, false)
			var v object.Version
			if tt.multipart {
				v, _, _ = w.threeParts(t, "movie")
			} else {
				v = w.putIn(t, "k", []byte("version"), object.SSE{})
			}
			site := &scriptedSite{MemSite: w.site}
			if tt.site != nil {
				tt.site(site)
			}
			if name == "there already, the same" {
				etag := v.ETag
				site.head = func() (string, bool, error) { return etag, true, nil }
			}
			item := w.last(t)
			status, err := w.replicatorWith(site, 0).Deliver(ctx, w.target, item)
			if status != tt.status || delivery.ClassOf(err) != tt.class {
				t.Fatalf("Deliver = %q, %v; want %q, class %q", status, err, tt.status, tt.class)
			}
			var failure *delivery.Error
			if err != nil && !errors.As(err, &failure) {
				t.Errorf("a failure of no class: %v", err)
			}
			if err != nil && tt.status == "" && delivery.ClassOf(err).Passes() {
				t.Errorf("a failure that holds the target passes")
			}
			// What the acknowledgement makes of the version.
			want := object.ReplicationPending
			if tt.status != "" {
				want = tt.status
			}
			if got := w.settle(t, item, status); got != want {
				t.Errorf("the version is %q after the acknowledgement, want %q", got, want)
			}
			// An upload refused for good is aborted; one held is kept,
			// for the next attempt to go on with.
			switch left := w.uploadsThere(t); {
			case tt.status == object.ReplicationFailed && left != 0 && site.aborted == nil:
				t.Errorf("%d uploads left there by a version refused for good", left)
			case left > 1:
				t.Errorf("%d uploads left there", left)
			}
		})
	}
}

// uploadsThere counts the uploads in progress at the other deployment.
func (w *replicatorWorld) uploadsThere(t *testing.T) int {
	t.Helper()
	n := 0
	if err := w.dest.ScanUploads(context.Background(), "copies", ports.UploadCursor{}, func(object.Upload) ports.ScanControl {
		n++
		return ports.ScanControl{}
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestReplicatorPartsThatDisagree: a version whose parts are not as its
// record says is held - delivery.ClassInternal, the fault is this side's -
// and the upload begun for it is kept, for an attempt after a repair.
func TestReplicatorPartsThatDisagree(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func([]object.Part) []object.Part{
		"a part missing":      func(parts []object.Part) []object.Part { return parts[:2] },
		"a part out of place": func(parts []object.Part) []object.Part { parts[1].Index = 5; return parts },
		"no part at all":      func([]object.Part) []object.Part { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			w := newReplicatorWorld(t, false)
			v, _, _ := w.threeParts(t, "movie")
			w.meta.TamperParts(v.DataID, change)
			status, err := w.replicator.Deliver(ctx, w.target, w.last(t))
			if status != "" || delivery.ClassOf(err) != delivery.ClassInternal || !errors.Is(err, domain.ErrIntegrity) {
				t.Errorf("Deliver = %q, %v; want held as internal", status, err)
			}
			if left := w.uploadsThere(t); left != 1 {
				t.Errorf("%d uploads there, want the one kept", left)
			}
		})
	}
}

// TestReplicatorHoldsOrPasses: what is not to be sent. An item that is not
// the target's as replication, of a target that is no other deployment, or
// of a later change of a version (not carried until PLAN 8.1.5) is held -
// delivery.ClassInternal - as an item nothing delivers is; an item of a
// version that is gone, that arrived as a replica, of a removal for good,
// of a bucket the target no longer admits or whose configuration no longer
// names the target is passed (delivery.ClassRejected) with no status. A
// version of a customer's key is FAILED, without a byte read or sent.
func TestReplicatorHoldsOrPasses(t *testing.T) {
	t.Parallel()
	customerKey := bytes.Repeat([]byte{7}, 32)
	for name, tt := range map[string]struct {
		change func(t *testing.T, w *replicatorWorld, item *delivery.Item, target *delivery.Target)
		sse    object.SSE
		status object.ReplicationStatus
		class  delivery.Class
	}{
		"for another target": {change: func(_ *testing.T, _ *replicatorWorld, item *delivery.Item, _ *delivery.Target) {
			item.To = []delivery.Consumer{{Kind: delivery.ConsumerReplication, Target: "site-c"}}
		}, class: delivery.ClassInternal},
		"for the target as a notification": {change: func(_ *testing.T, _ *replicatorWorld, item *delivery.Item, _ *delivery.Target) {
			item.To = []delivery.Consumer{{Kind: delivery.ConsumerNotification, Target: "site-b"}}
		}, class: delivery.ClassInternal},
		"a target that is a webhook": {change: func(_ *testing.T, _ *replicatorWorld, _ *delivery.Item, target *delivery.Target) {
			target.Kind = delivery.KindWebhook
		}, class: delivery.ClassInternal},
		"a removal for good": {change: event(delivery.EventDelete), class: delivery.ClassRejected},
		"a bucket the target no longer admits": {change: func(_ *testing.T, _ *replicatorWorld, _ *delivery.Item, target *delivery.Target) {
			target.Buckets = []string{"logs"}
		}, class: delivery.ClassRejected},
		"a configuration removed since": {change: func(t *testing.T, w *replicatorWorld, _ *delivery.Item, _ *delivery.Target) {
			w.configure(t, bucket.ReplicationConfig{})
		}, class: delivery.ClassRejected},
		"a configuration that names another target since": {change: func(t *testing.T, w *replicatorWorld, _ *delivery.Item, _ *delivery.Target) {
			if err := w.meta.PutTarget(context.Background(), siteRecord("site-c", "b"), audit.Record{}); err != nil {
				t.Fatal(err)
			}
			w.configure(t, bucket.ReplicationConfig{Target: "site-c", Bucket: "copies", Rules: []bucket.ReplicationRule{{ID: "all", Enabled: true}}})
		}, class: delivery.ClassRejected},
		"a version that is gone": {change: func(t *testing.T, w *replicatorWorld, item *delivery.Item, _ *delivery.Target) {
			if _, err := w.objects.Delete(context.Background(), porttest.Root(), app.DeleteInput{Bucket: "b", Key: item.Key, Version: &item.Version}); err != nil {
				t.Fatal(err)
			}
		}, class: delivery.ClassRejected},
		"a version that arrived as a replica": {change: func(_ *testing.T, w *replicatorWorld, item *delivery.Item, _ *delivery.Target) {
			w.meta.Tamper(item.Bucket, item.Key, func(v *object.Version) { v.Replication = object.ReplicationReplica })
		}, class: delivery.ClassRejected},
		"a row and a version that disagree": {change: event(delivery.EventMarker), class: delivery.ClassInternal},
		"a customer's key": {
			sse:    object.SSE{Mode: object.SSEC, CustomerKey: customerKey},
			status: object.ReplicationFailed, class: delivery.ClassSSEC,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			w := newReplicatorWorld(t, true)
			w.putIn(t, "named-object", []byte("version"), tt.sse)
			item, target := w.last(t), w.target
			if tt.change != nil {
				tt.change(t, w, &item, &target)
			}
			status, err := w.replicator.Deliver(ctx, target, item)
			if status != tt.status || delivery.ClassOf(err) != tt.class {
				t.Fatalf("Deliver = %q, %v; want %q, class %q", status, err, tt.status, tt.class)
			}
			if calls := w.site.Calls(); len(calls) != 0 {
				t.Errorf("sent to the other deployment: %v", calls)
			}
			if strings.Contains(err.Error(), item.Key) {
				t.Errorf("the failure names the key: %v", err)
			}
		})
	}
}

// event returns a change that makes the item one of another kind of change.
func event(e delivery.Event) func(*testing.T, *replicatorWorld, *delivery.Item, *delivery.Target) {
	return func(_ *testing.T, _ *replicatorWorld, item *delivery.Item, _ *delivery.Target) { item.Event = e }
}

// TestReplicatorWithoutWhatItNeeds: a replicator given nothing to read or
// send through, or no ID of this deployment, holds every item; so does one
// whose client of the other deployment cannot be made.
func TestReplicatorWithoutWhatItNeeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newReplicatorWorld(t, false)
	w.putIn(t, "k", []byte("version"), object.SSE{})
	item := w.last(t)
	sites := app.SiteSourceFunc(func(context.Context, delivery.Target) (ports.SiteClient, error) { return w.site, nil })
	for name, cfg := range map[string]app.ReplicatorConfig{
		"no reader":    {Buckets: w.meta, Sites: sites, Source: sendingSource, Clock: w.clock},
		"no store":     {Objects: w.objects, Sites: sites, Source: sendingSource, Clock: w.clock},
		"no sites":     {Objects: w.objects, Buckets: w.meta, Source: sendingSource, Clock: w.clock},
		"no source":    {Objects: w.objects, Buckets: w.meta, Sites: sites, Clock: w.clock},
		"a bad source": {Objects: w.objects, Buckets: w.meta, Sites: sites, Source: "not/an/id", Clock: w.clock},
		"no client": {
			Objects: w.objects, Buckets: w.meta, Source: sendingSource, Clock: w.clock,
			Sites: app.SiteSourceFunc(func(context.Context, delivery.Target) (ports.SiteClient, error) {
				return nil, errors.New("the secret does not open")
			}),
		},
	} {
		status, err := app.NewReplicator(cfg).Deliver(ctx, w.target, item)
		if status != "" || delivery.ClassOf(err) != delivery.ClassInternal {
			t.Errorf("%s: Deliver = %q, %v; want held as internal", name, status, err)
		}
	}
	// A store that cannot be read holds; one whose configuration is no
	// longer one this release reads holds too.
	failing := app.NewReplicator(app.ReplicatorConfig{Objects: w.objects, Buckets: failingConfig{BucketStore: w.meta}, Sites: sites, Source: sendingSource, Clock: w.clock})
	if status, err := failing.Deliver(ctx, w.target, item); status != "" || delivery.ClassOf(err) != delivery.ClassInternal {
		t.Errorf("a configuration that cannot be read: %q, %v", status, err)
	}
	garbled := app.NewReplicator(app.ReplicatorConfig{Objects: w.objects, Buckets: failingConfig{BucketStore: w.meta, document: []byte("{}")}, Sites: sites, Source: sendingSource, Clock: w.clock})
	if status, err := garbled.Deliver(ctx, w.target, item); status != "" || !errors.Is(err, domain.ErrIntegrity) {
		t.Errorf("a configuration that cannot be parsed: %q, %v", status, err)
	}
	if calls := w.site.Calls(); len(calls) != 0 {
		t.Errorf("sent: %v", calls)
	}
}

// failingConfig is a store whose replication configurations cannot be read,
// or read as document.
type failingConfig struct {
	ports.BucketStore
	document []byte
}

func (f failingConfig) GetBucketConfig(context.Context, string, string) ([]byte, error) {
	if f.document != nil {
		return f.document, nil
	}
	return nil, fmt.Errorf("%w: the metadata cannot be read", domain.ErrUnavailable)
}

// TestReplicationReads: the replicator reads as the replication service,
// which reads a version by its ID - a delete marker too - and nothing else:
// a caller without s3:GetObjectVersion is refused, the lifecycle service
// too; a marker has no bytes, a span beyond the version is no span, and a
// version that is not of parts has none.
func TestReplicationReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newReplicatorWorld(t, false)
	data := []byte("the bytes of the version")
	v := w.putIn(t, "k", data, object.SSE{})
	service := iam.Principal{Service: iam.ServiceReplication}
	for name, principal := range map[string]iam.Principal{
		"anonymous": {}, "the lifecycle service": {Service: iam.ServiceLifecycle}, "a user of no policy": {AccessKeyID: "AKIDNONE", User: "nobody"},
	} {
		if _, err := w.objects.ReplicationVersion(ctx, principal, "b", "k", v.Ref()); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("%s reads a version: %v", name, err)
		}
		if _, err := w.objects.OpenReplicationSpan(ctx, principal, "b", "k", v.Ref(), object.Range{Length: v.Size}); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("%s opens a version: %v", name, err)
		}
		if _, err := w.objects.ReplicationParts(ctx, principal, "b", "k", v.Ref(), 0, 1); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("%s reads parts: %v", name, err)
		}
	}
	got, err := w.objects.ReplicationVersion(ctx, service, "b", "k", v.Ref())
	if err != nil || got.VersionID != v.VersionID {
		t.Fatalf("ReplicationVersion = %+v, %v", got, err)
	}
	for _, span := range []object.Range{{Length: v.Size}, {Offset: 4, Length: 5}, {Offset: v.Size}} {
		body, err := w.objects.OpenReplicationSpan(ctx, service, "b", "k", got.Ref(), span)
		if err != nil {
			t.Fatalf("OpenReplicationSpan(%+v): %v", span, err)
		}
		read, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil || !bytes.Equal(read, data[span.Offset:span.Offset+span.Length]) {
			t.Errorf("span %+v read %q, %v", span, read, err)
		}
	}
	for _, span := range []object.Range{{Offset: -1, Length: 1}, {Length: v.Size + 1}, {Offset: v.Size, Length: 1}, {Offset: 1, Length: -1}} {
		if _, err := w.objects.OpenReplicationSpan(ctx, service, "b", "k", got.Ref(), span); !errors.Is(err, domain.ErrInvalidRange) {
			t.Errorf("span %+v: %v", span, err)
		}
	}
	if _, err := w.objects.ReplicationParts(ctx, service, "b", "k", got.Ref(), 0, 1); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("the parts of a version of none: %v", err)
	}
	if _, err := w.objects.ReplicationVersion(ctx, service, "b", "", v.Ref()); !errors.Is(err, domain.ErrInvalidObjectKey) {
		t.Errorf("a key that is none: %v", err)
	}
	out, err := w.objects.Delete(ctx, porttest.Root(), app.DeleteInput{Bucket: "b", Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	marker, err := w.objects.ReplicationVersion(ctx, service, "b", "k", out.Marker.Ref())
	if err != nil || !marker.DeleteMarker {
		t.Fatalf("the marker = %+v, %v", marker, err)
	}
	if _, err := w.objects.OpenReplicationSpan(ctx, service, "b", "k", marker.Ref(), object.Range{}); !errors.Is(err, domain.ErrMethodNotAllowed) {
		t.Errorf("the bytes of a marker: %v", err)
	}
	if _, err := w.objects.ReplicationVersion(ctx, service, "b", "k", object.VersionRef{ID: object.ID{9}}); !errors.Is(err, domain.ErrVersionNotFound) {
		t.Errorf("a version that is not there: %v", err)
	}
	// Every question the replicator asked was asked as the service, for a
	// version by its ID.
	if _, err := w.replicator.Deliver(ctx, w.target, w.rows()[0]); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	for _, req := range w.authz.Requests() {
		if req.Principal.Service == iam.ServiceReplication && req.Action != iam.ActionGetObjectVersion {
			t.Errorf("the replication service asked for %s", req.Action)
		}
	}
}

// rateSite records when each read of a body it is sent was made, and how
// many bytes it was.
type rateSite struct {
	*porttest.MemSite
	mu    sync.Mutex
	reads []timedRead
}

type timedRead struct {
	at time.Time
	n  int
}

func (s *rateSite) PutReplica(_ context.Context, _ object.Version, body io.Reader) error {
	buf := make([]byte, 1<<20)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.reads = append(s.reads, timedRead{at: time.Now(), n: n})
			s.mu.Unlock()
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return siteFailure(delivery.ClassInternal)
		}
	}
}

func (s *rateSite) HeadVersion(context.Context, string, string, object.VersionRef) (string, bool, error) {
	return "", false, nil
}

// TestReplicatorKeepsToItsRate: the bytes sent are held to the rate,
// averaged over ten seconds: in any ten seconds at most ten seconds' worth
// and one read more, and a version of twelve seconds' worth takes about
// twelve seconds.
func TestReplicatorKeepsToItsRate(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const rate = 256 << 10
		w := newReplicatorWorld(t, false)
		data := randomBytes(t, 12*rate)
		w.putIn(t, "big", data, object.SSE{})
		site := &rateSite{MemSite: w.site}
		r := app.NewReplicator(app.ReplicatorConfig{
			Objects: w.objects, Buckets: w.meta, Source: sendingSource, Clock: bubbleClock{}, RateBytes: rate,
			Sites: app.SiteSourceFunc(func(context.Context, delivery.Target) (ports.SiteClient, error) { return site, nil }),
		})
		start := time.Now()
		if status, err := r.Deliver(t.Context(), w.target, w.last(t)); err != nil || status != object.ReplicationCompleted {
			t.Fatalf("Deliver = %q, %v", status, err)
		}
		took := time.Since(start)
		if took < 11*time.Second || took > 13*time.Second {
			t.Errorf("twelve seconds' worth took %v", took)
		}
		total := 0
		for i, read := range site.reads {
			total += read.n
			if read.n > 64<<10 {
				t.Errorf("a read of %d bytes", read.n)
			}
			window := 0
			for _, other := range site.reads[i:] {
				if other.at.Sub(read.at) < app.ReplicationRateWindow {
					window += other.n
				}
			}
			if window > 10*rate+64<<10 {
				t.Errorf("%d bytes in the ten seconds from %v", window, read.at.Sub(start))
			}
		}
		if total != len(data) {
			t.Errorf("%d bytes sent, want %d", total, len(data))
		}
	})
	if got := app.ReplicationRateBytes(3); got != 3<<20 {
		t.Errorf("ReplicationRateBytes(3) = %d", got)
	}
	if got := app.ReplicationRateBytes(0); got != 0 {
		t.Errorf("ReplicationRateBytes(0) = %d", got)
	}
}
