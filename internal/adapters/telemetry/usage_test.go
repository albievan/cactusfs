package telemetry_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// instrumentedUsage is the store of schema 5 with its usage records and
// quotas reached through the decorator.
type instrumentedUsage struct {
	*porttest.MemMetadata
	*porttest.MemIAM
	usage ports.BucketUsageStore
}

// BucketUsages implements ports.BucketUsageStore through the decorator.
func (s instrumentedUsage) BucketUsages(ctx context.Context, names []string) ([]ports.BucketUsage, error) {
	return s.usage.BucketUsages(ctx, names)
}

// WalkBucketUsage implements ports.BucketUsageStore through the decorator.
func (s instrumentedUsage) WalkBucketUsage(ctx context.Context, name string, limit int, restart bool) (ports.UsageWalk, error) {
	return s.usage.WalkBucketUsage(ctx, name, limit, restart)
}

// SetBucketQuota implements ports.BucketUsageStore through the decorator.
func (s instrumentedUsage) SetBucketQuota(ctx context.Context, name string, quotaBytes int64, record audit.Record) error {
	return s.usage.SetBucketQuota(ctx, name, quotaBytes, record)
}

// TestBucketUsageIsCountedAndTraced: the store of usage records and quotas
// keeps the contract of schema 5 through its decorator, every method is
// counted under its own name, a refusal is counted as an error - and no
// bucket's name is in a metric or a span.
func TestBucketUsageIsCountedAndTraced(t *testing.T) {
	t.Parallel()
	recorder := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, recorder)
	t.Run("contract", func(t *testing.T) {
		porttest.Schema5Contract(t, func(*testing.T) porttest.Schema5 {
			mem := porttest.NewMemMetadata()
			trail := porttest.NewMemIAM()
			mem.Audit = trail
			return instrumentedUsage{MemMetadata: mem, MemIAM: trail, usage: tel.InstrumentBucketUsage(mem)}
		})
	})

	ctx := context.Background()
	const name = "tenant-secret-bucket"
	mem := porttest.NewMemMetadata()
	if err := mem.CreateBucket(ctx, bucket.Bucket{Name: name}); err != nil {
		t.Fatal(err)
	}
	usage := tel.InstrumentBucketUsage(mem)
	if err := usage.SetBucketQuota(ctx, name, 10, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	if got, err := usage.BucketUsages(ctx, []string{name}); err != nil || len(got) != 1 || got[0].QuotaBytes != 10 {
		t.Errorf("BucketUsages = %+v, %v", got, err)
	}
	if _, err := usage.WalkBucketUsage(ctx, name, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := usage.SetBucketQuota(ctx, name+"-missing", 10, audit.Record{}); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Errorf("a quota of a bucket that is not there = %v", err)
	}

	metrics := scrape(t, tel)
	wantEveryMethod(t, metrics, "bucket_usage", methodsOf[ports.BucketUsageStore]())
	if want := "cactusfs_port_calls_total method=SetBucketQuota port=bucket_usage result=error"; !strings.Contains(metrics, want) {
		t.Errorf("metrics lack %q", want)
	}
	if !contains(spanNames(recorder), "bucket_usage.WalkBucketUsage") {
		t.Errorf("no span named bucket_usage.WalkBucketUsage among %v", spanNames(recorder))
	}
	wantNothingOf(t, tel, recorder, name)
}

// TestQuotaCountersCountWithoutNames: a refused write and a recount that
// drifted are each one more of their counter, which has no label - so no
// bucket can be named by one.
func TestQuotaCountersCountWithoutNames(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil)
	samples := gauges(t, tel)
	for _, name := range []string{"cactusfs_quota_refusals_total{}", "cactusfs_bucket_usage_drift_total{}"} {
		if got, ok := samples[name]; !ok || got != 0 {
			t.Errorf("%s = %v (exposed: %v) before anything happened, want 0", name, got, ok)
		}
	}
	for range 3 {
		tel.ObserveQuotaRefusal()
	}
	tel.ObserveUsageDrift()
	samples = gauges(t, tel)
	for name, want := range map[string]float64{"cactusfs_quota_refusals_total{}": 3, "cactusfs_bucket_usage_drift_total{}": 1} {
		if got := samples[name]; got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}
