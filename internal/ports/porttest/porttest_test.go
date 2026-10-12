package porttest_test

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

func TestMemMetadataContract(t *testing.T) {
	t.Parallel()
	porttest.MetadataContract(t, func(*testing.T) porttest.Metadata { return porttest.NewMemMetadata() })
}

// memSchema3 is the in-memory stores as one: the metadata, and the trail
// that its audited changes are appended to.
type memSchema3 struct {
	*porttest.MemMetadata
	*porttest.MemIAM
}

func TestMemSchema3Contract(t *testing.T) {
	t.Parallel()
	porttest.Schema3Contract(t, func(*testing.T) porttest.Schema3 {
		store := memSchema3{MemMetadata: porttest.NewMemMetadata(), MemIAM: porttest.NewMemIAM()}
		store.Audit = store.MemIAM
		return store
	})
}

func TestMemSchema4Contract(t *testing.T) {
	t.Parallel()
	porttest.Schema4Contract(t, func(*testing.T) porttest.Schema4 { return porttest.NewMemMetadata() })
}

// memSchema5 is the in-memory stores as one: the metadata, and the trail
// that the changes of an administrator are appended to.
type memSchema5 struct {
	*porttest.MemMetadata
	*porttest.MemIAM
}

func openMemSchema5(bound int) porttest.Schema5 {
	store := memSchema5{MemMetadata: porttest.NewMemMetadata(), MemIAM: porttest.NewMemIAM()}
	store.Audit, store.MaxOutboxRows = store.MemIAM, bound
	return store
}

func TestMemSchema5Contract(t *testing.T) {
	t.Parallel()
	porttest.Schema5Contract(t, func(*testing.T) porttest.Schema5 { return openMemSchema5(0) })
}

func TestMemOutboxBoundContract(t *testing.T) {
	t.Parallel()
	for _, bound := range []int{4, 9, 40} {
		porttest.OutboxBoundContract(t, bound, func(*testing.T) porttest.Schema5 { return openMemSchema5(bound) })
	}
}

func TestMemSiteContract(t *testing.T) {
	t.Parallel()
	porttest.SiteClientContract(t, "5d3c1f0a-site-a", func(*testing.T) (ports.SiteClient, porttest.SiteStore) {
		store := porttest.NewMemMetadata()
		return porttest.NewMemSite(store), store
	})
}

// TestMemSite: what the in-memory client of another deployment adds to the
// contract for the tests that use it - its record of calls, the bytes it
// read, and the failures a test asks for.
func TestMemSite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := porttest.NewMemMetadata()
	site := porttest.NewMemSite(store)
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "replica"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBucketVersioning(ctx, "replica", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBucketReplica(ctx, "replica", "site-a", audit.Record{}); err != nil {
		t.Fatal(err)
	}
	v := porttest.SampleVersion("replica", "k", 1)
	v.Size, v.Replica = 10, &object.Replica{From: "site-a", Seq: 7}
	if err := site.PutReplica(ctx, v, strings.NewReader("0123456789")); err != nil {
		t.Fatalf("PutReplica: %v", err)
	}
	// A deployment that cannot be reached: every call fails with the class
	// the test gave, and nothing arrives.
	cause := errors.New("dial tcp: connection refused")
	site.Fail(delivery.Failed(delivery.ClassConnect, cause))
	later := porttest.SampleVersion("replica", "k2", 2)
	later.Size, later.Replica = 3, &object.Replica{From: "site-a", Seq: 8}
	checks := map[string]error{
		"PutReplica": site.PutReplica(ctx, later, strings.NewReader("abc")),
		"PutMarker":  site.PutMarker(ctx, object.Version{Bucket: "replica", Key: "k", DeleteMarker: true, Replica: later.Replica}),
		"PutTags":    site.PutTags(ctx, "replica", "k", v.Ref(), nil),
		"PutHold":    site.PutLegalHold(ctx, "replica", "k", v.Ref(), true),
		"PutRetain":  site.PutRetention(ctx, "replica", "k", v.Ref(), nil),
		"Abort":      site.AbortUpload(ctx, "replica", "k", "00"),
		"Complete":   site.CompleteReplica(ctx, later, "00", nil),
	}
	_, checks["Check"] = site.Check(ctx, "replica")
	_, checks["CreateUpload"] = site.CreateUpload(ctx, later)
	_, checks["PutPart"] = site.PutPart(ctx, "replica", "k", "00", 1, 3, strings.NewReader("abc"))
	_, checks["ListReplicaUploads"] = site.ListReplicaUploads(ctx, "replica", "k")
	_, _, checks["HeadVersion"] = site.HeadVersion(ctx, "replica", "k", v.Ref())
	for call, err := range checks {
		if delivery.ClassOf(err) != delivery.ClassConnect || !errors.Is(err, cause) {
			t.Errorf("%s at a deployment that cannot be reached = %v", call, err)
		}
	}
	if _, err := store.GetObject(ctx, "replica", "k2"); !errors.Is(err, domain.ErrObjectNotFound) {
		t.Errorf("a version arrived at a deployment that cannot be reached: %v", err)
	}
	// A failure that says no class is one of this side.
	site.Fail(errors.New("out of memory"))
	if _, err := site.Check(ctx, "replica"); delivery.ClassOf(err) != delivery.ClassInternal {
		t.Errorf("a failure without a class = %v", err)
	}
	site.Fail(nil)
	// What cannot be an upload or a marker is refused for itself.
	refused := map[string]error{
		"an upload ID that is none": site.AbortUpload(ctx, "replica", "k", "not-an-id"),
		"a completion of no upload": site.CompleteReplica(ctx, later, "not-an-id", nil),
		"a marker that is none":     site.PutMarker(ctx, later),
	}
	_, refused["a part of no upload"] = site.PutPart(ctx, "replica", "k", "not-an-id", 1, 0, strings.NewReader(""))
	_, refused["an upload of no source"] = site.CreateUpload(ctx, porttest.SampleVersion("replica", "k3", 3))
	_, refused["a part of an upload that is not there"] = site.PutPart(ctx, "replica", "k", later.VersionID.String(), 1, 0, strings.NewReader(""))
	for name, err := range refused {
		if delivery.ClassOf(err) != delivery.ClassRejected {
			t.Errorf("%s = %v, want the class %s", name, err, delivery.ClassRejected)
		}
	}
	if err := site.PutReplica(ctx, later, strings.NewReader("abc")); err != nil {
		t.Fatalf("PutReplica once the deployment is back: %v", err)
	}
	calls := site.Calls()
	if len(calls) != 21 || calls[0] != "PutReplica replica/k" || calls[len(calls)-1] != "PutReplica replica/k2" || !slices.Contains(calls, "Check replica") {
		t.Errorf("the calls that were made: %d, %v", len(calls), calls)
	}
	if site.BytesSent() != 13 {
		t.Errorf("BytesSent() = %d, want 13", site.BytesSent())
	}
}

func TestDeliveryStatusReaderContract(t *testing.T) {
	t.Parallel()
	porttest.DeliveryStatusContract(t, func(_ *testing.T, script porttest.DeliveryStatusScript) ports.DeliveryStatusReader {
		return porttest.ScriptedDeliveryStatusReader(script)
	})
}

func TestMemShardStoreContract(t *testing.T) {
	t.Parallel()
	porttest.ShardStoreContract(t, func(*testing.T) ports.ShardStore { return porttest.NewMemShardStore("mem-drive") })
}

func TestCodecContract(t *testing.T) {
	t.Parallel()
	porttest.ErasureCodecContract(t, porttest.NewCodec())
}

func TestKMS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatalf("NewKMS: %v", err)
	}
	if kms.KeyID() != "test:1" {
		t.Errorf("KeyID() = %q", kms.KeyID())
	}
	key, err := kms.GenerateDataKey(ctx, []byte("owner"))
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	if len(key.Plaintext) != 32 || bytes.Contains(key.Wrapped, key.Plaintext) {
		t.Fatal("data key is malformed or stored unwrapped")
	}
	got, err := kms.UnwrapDataKey(ctx, key.Wrapped, []byte("owner"))
	if err != nil || !bytes.Equal(got, key.Plaintext) {
		t.Errorf("UnwrapDataKey = %v", err)
	}
	if _, err := kms.UnwrapDataKey(ctx, key.Wrapped, []byte("someone else")); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("UnwrapDataKey with another owner: error = %v", err)
	}
	if _, err := kms.UnwrapDataKey(ctx, []byte("short"), nil); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Errorf("UnwrapDataKey of garbage: error = %v", err)
	}
}

func TestClockAuthorizerAndEntropy(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	clock := porttest.NewClock(start)
	clock.Advance(time.Minute)
	if !clock.Now().Equal(start.Add(time.Minute)) {
		t.Errorf("Now() = %v", clock.Now())
	}

	authz := &porttest.Authorizer{}
	if err := authz.Authorize(context.Background(), iam.Request{Principal: porttest.Root(), Action: iam.ActionGetObject}); err != nil {
		t.Errorf("root denied: %v", err)
	}
	if err := authz.Authorize(context.Background(), iam.Request{Action: iam.ActionGetObject}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("anonymous allowed: %v", err)
	}
	if len(authz.Requests()) != 2 {
		t.Errorf("recorded %d requests, want 2", len(authz.Requests()))
	}

	a, b := make([]byte, 64), make([]byte, 64)
	_, _ = porttest.Entropy().Read(a)
	_, _ = porttest.Entropy().Read(b)
	if !bytes.Equal(a, b) || bytes.Equal(a[:32], a[32:]) {
		t.Error("Entropy is not deterministic or repeats immediately")
	}
}

func TestMemIAMContract(t *testing.T) {
	t.Parallel()
	porttest.IAMContract(t, func(*testing.T) porttest.IAM { return porttest.NewMemIAM() })
	porttest.IdempotencyContract(t, func(*testing.T) ports.IdempotencyStore { return porttest.NewMemIAM() })
}

func TestNodeSettingsReaderContract(t *testing.T) {
	t.Parallel()
	porttest.NodeSettingsContract(t, func(_ *testing.T, script porttest.NodeSettingsScript) ports.NodeSettingsReader {
		return porttest.ScriptedNodeSettingsReader(script)
	})
}
