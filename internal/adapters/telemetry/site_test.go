package telemetry_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// TestSiteClientIsCountedAndTraced: the client of another deployment keeps
// the contract of a site client through its decorator, every method is
// counted under its own name with the port "site", a refusal is counted as
// an error - and neither a bucket, a key nor the source is in a metric or a
// span.
func TestSiteClientIsCountedAndTraced(t *testing.T) {
	t.Parallel()
	const source = "site-secret-deployment"
	recorder := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, recorder)
	t.Run("contract", func(t *testing.T) {
		porttest.SiteClientContract(t, source, func(*testing.T) (ports.SiteClient, porttest.SiteStore) {
			store := porttest.NewMemMetadata()
			return tel.InstrumentSiteClient(porttest.NewMemSite(store)), store
		})
	})

	ctx := context.Background()
	const name, key = "tenant-secret-bucket", "tenant-secret-key"
	store := porttest.NewMemMetadata()
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: name, CreatedAt: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBucketVersioning(ctx, name, bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBucketReplica(ctx, name, source, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	site := tel.InstrumentSiteClient(porttest.NewMemSite(store))
	v := porttest.SampleVersion(name, key, 1)
	v.Versioned, v.Seq, v.Replica = true, 7, &object.Replica{From: source, Seq: 7}
	ref := v.Ref()
	if answer, err := site.Check(ctx, name); err != nil || answer.ReplicaOf != source {
		t.Errorf("Check = %+v, %v", answer, err)
	}
	if err := site.PutReplica(ctx, v, strings.NewReader(strings.Repeat("x", int(v.Size)))); err != nil {
		t.Errorf("PutReplica: %v", err)
	}
	if etag, found, err := site.HeadVersion(ctx, name, key, ref); err != nil || !found || etag != v.ETag {
		t.Errorf("HeadVersion = %q, %v, %v", etag, found, err)
	}
	if err := site.PutTags(ctx, name, key, ref, map[string]string{"a": "b"}); err != nil {
		t.Errorf("PutTags: %v", err)
	}
	// A retention or a legal hold of a bucket without Object Lock is
	// refused: counted as errors.
	if err := site.PutRetention(ctx, name, key, ref, nil); err == nil {
		t.Error("a retention in a bucket without Object Lock was taken")
	}
	if err := site.PutLegalHold(ctx, name, key, ref, true); err == nil {
		t.Error("a legal hold in a bucket without Object Lock was taken")
	}
	parts := v
	parts.Key, parts.VersionID = key+"-parts", object.ID{9}
	id, err := site.CreateUpload(ctx, parts)
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	etag, err := site.PutPart(ctx, name, parts.Key, id, 1, 3, strings.NewReader("abc"))
	if err != nil {
		t.Fatalf("PutPart: %v", err)
	}
	if listed, found, err := site.ListParts(ctx, name, parts.Key, id); err != nil || !found || len(listed) != 1 || listed[0].ETag != etag {
		t.Errorf("ListParts = %+v, %v, %v", listed, found, err)
	}
	if ids, err := site.ListReplicaUploads(ctx, name, parts.Key); err != nil || len(ids) != 1 {
		t.Errorf("ListReplicaUploads = %v, %v", ids, err)
	}
	if err := site.CompleteReplica(ctx, parts, id, []ports.SitePart{{Number: 1, ETag: etag}}); err != nil {
		t.Errorf("CompleteReplica: %v", err)
	}
	if err := site.AbortUpload(ctx, name, parts.Key, id); err != nil {
		t.Errorf("AbortUpload of an upload completed: %v", err)
	}
	marker := v
	marker.Key, marker.VersionID, marker.DeleteMarker, marker.Replica = key, object.ID{8}, true, &object.Replica{From: source, Seq: 8}
	if err := site.PutMarker(ctx, marker); err != nil {
		t.Errorf("PutMarker: %v", err)
	}
	if _, err := site.Check(ctx, name+"-missing"); delivery.ClassOf(err) != delivery.ClassNoSuchBucket {
		t.Errorf("Check of a bucket that is not there = %v", err)
	}

	metrics := scrape(t, tel)
	wantEveryMethod(t, metrics, "site", methodsOf[ports.SiteClient]())
	if want := "cactusfs_port_calls_total method=PutRetention port=site result=error"; !strings.Contains(metrics, want) {
		t.Errorf("metrics lack %q", want)
	}
	if !contains(spanNames(recorder), "site.PutReplica") {
		t.Errorf("no span named site.PutReplica among %v", spanNames(recorder))
	}
	wantNothingOf(t, tel, recorder, name)
	wantNothingOf(t, tel, recorder, key)
	wantNothingOf(t, tel, recorder, source)
}
