package porttest_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

func TestMemMetadataContract(t *testing.T) {
	t.Parallel()
	porttest.MetadataContract(t, func(*testing.T) porttest.Metadata { return porttest.NewMemMetadata() })
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
