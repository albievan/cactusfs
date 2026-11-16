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

// instrumentedReplication is the store of schema 5 with replica mode and
// the records of resyncs reached through the decorator.
type instrumentedReplication struct {
	*porttest.MemMetadata
	*porttest.MemIAM
	replication ports.ReplicationStore
}

// SetBucketReplica implements ports.ReplicationStore through the decorator.
func (s instrumentedReplication) SetBucketReplica(ctx context.Context, name, source string, record audit.Record) error {
	return s.replication.SetBucketReplica(ctx, name, source, record)
}

// PutResync implements ports.ReplicationStore through the decorator.
func (s instrumentedReplication) PutResync(ctx context.Context, r bucket.Resync) error {
	return s.replication.PutResync(ctx, r)
}

// GetResync implements ports.ReplicationStore through the decorator.
func (s instrumentedReplication) GetResync(ctx context.Context, bucketName string) (bucket.Resync, error) {
	return s.replication.GetResync(ctx, bucketName)
}

// TestReplicationIsCountedAndTraced: the store of replica mode and resyncs
// keeps the contract of schema 5 through its decorator, every method is
// counted under its own name, a refusal is counted as an error - and
// neither a bucket's name nor the ID of a deployment is in a metric or a
// span.
func TestReplicationIsCountedAndTraced(t *testing.T) {
	t.Parallel()
	recorder := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, recorder)
	t.Run("contract", func(t *testing.T) {
		porttest.Schema5Contract(t, func(*testing.T) porttest.Schema5 {
			mem := porttest.NewMemMetadata()
			trail := porttest.NewMemIAM()
			mem.Audit = trail
			return instrumentedReplication{MemMetadata: mem, MemIAM: trail, replication: tel.InstrumentReplication(mem)}
		})
	})

	ctx := context.Background()
	const name, source = "tenant-secret-bucket", "site-secret-deployment"
	mem := porttest.NewMemMetadata()
	if err := mem.CreateBucket(ctx, bucket.Bucket{Name: name}); err != nil {
		t.Fatal(err)
	}
	replication := tel.InstrumentReplication(mem)
	// A bucket that keeps no versions is no replica: refused, and counted
	// as an error.
	if err := replication.SetBucketReplica(ctx, name, source, audit.Record{}); !errors.Is(err, domain.ErrInvalidBucketState) {
		t.Errorf("a replica that keeps no versions = %v", err)
	}
	if err := replication.SetBucketReplica(ctx, name, "", audit.Record{}); err != nil {
		t.Errorf("promoting a bucket that is no replica = %v", err)
	}
	if got, err := replication.GetResync(ctx, name); err != nil || got.Bucket != "" {
		t.Errorf("GetResync = %+v, %v", got, err)
	}
	if err := replication.PutResync(ctx, bucket.Resync{Bucket: name + "-missing"}); err == nil {
		t.Error("the resync of a bucket that is not there was stored")
	}

	metrics := scrape(t, tel)
	wantEveryMethod(t, metrics, "replication", methodsOf[ports.ReplicationStore]())
	if want := "cactusfs_port_calls_total method=SetBucketReplica port=replication result=error"; !strings.Contains(metrics, want) {
		t.Errorf("metrics lack %q", want)
	}
	if !contains(spanNames(recorder), "replication.GetResync") {
		t.Errorf("no span named replication.GetResync among %v", spanNames(recorder))
	}
	wantNothingOf(t, tel, recorder, name)
	wantNothingOf(t, tel, recorder, source)
}
