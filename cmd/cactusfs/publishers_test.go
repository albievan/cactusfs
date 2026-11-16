package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/events/nats"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// sealField seals value through kms as the target use case does.
func sealField(t *testing.T, kms ports.KMS, value string, aad []byte) iam.SealedSecret {
	t.Helper()
	key, err := kms.GenerateDataKey(t.Context(), aad)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Plaintext)
	field, err := iam.Seal(key.Plaintext, rand.Reader, []byte(value), aad)
	if err != nil {
		t.Fatal(err)
	}
	field.KMSKeyID, field.WrappedKey = kms.KeyID(), key.Wrapped
	return field
}

// closedPort returns a loopback port nothing listens on: one freeAddr found
// free, of the range no system hands out by itself.
func closedPort(t *testing.T) string {
	t.Helper()
	_, port, err := net.SplitHostPort(freeAddr(t))
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// testHook is a webhook target with a path, a bearer secret and a signing
// key, sealed through kms.
func testHook(t *testing.T, kms ports.KMS, name, endpoint string) delivery.Target {
	t.Helper()
	return delivery.Target{
		Name: name, Kind: delivery.KindWebhook, Endpoint: endpoint, Buckets: []string{"photos"},
		SealedPath: sealField(t, kms, "/hooks/in", delivery.PathAAD(name)),
		Auth:       delivery.Auth{Mode: delivery.AuthBearer},
		Secret:     sealField(t, kms, "bearer-secret", delivery.SecretAAD(name)),
		SignKey:    sealField(t, kms, "signing-key", delivery.SignKeyAAD(name)),
	}
}

// testNATS is a NATS target that proves itself with a token sealed through
// kms.
func testNATS(t *testing.T, kms ports.KMS, name, endpoint string) delivery.Target {
	t.Helper()
	return delivery.Target{
		Name: name, Kind: delivery.KindNATS, Endpoint: endpoint, Subject: "cactusfs.events", Buckets: []string{"photos"},
		Auth: delivery.Auth{Mode: delivery.AuthToken}, Secret: sealField(t, kms, "nats-token", delivery.SecretAAD(name)),
	}
}

// testMessage is a message a publisher is handed.
func testMessage() ports.Event {
	return ports.Event{ID: "event-1", Type: ports.MessageNotificationObject, Sequence: 1, Time: time.Unix(0, 0), Data: json.RawMessage(`{}`)}
}

// testPublishers returns publishers over a KMS and a store of targets of a
// test, and the telemetry they report to.
func testPublishers(t *testing.T, active func() bool) (*publishers, *porttest.KMS, *porttest.MemMetadata, *telemetry.Telemetry) {
	t.Helper()
	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}
	meta := porttest.NewMemMetadata()
	tel := newTelemetry(t)
	p := newPublishers(publishersConfig{
		KMS: tel.InstrumentKMS(kms), Targets: tel.InstrumentTargets(meta), Telemetry: tel,
		Version: "1.2.3", Clock: systemClock{}, Active: active,
	})
	t.Cleanup(p.closeAll)
	return p, kms, meta, tel
}

// egressRefusals sums cactusfs_egress_refusals_total over its classes.
func egressRefusals(t *testing.T, tel *telemetry.Telemetry) float64 {
	t.Helper()
	sum := 0.0
	for name, value := range samples(t, tel) {
		if strings.HasPrefix(name, "cactusfs_egress_refusals_total{") {
			sum += value
		}
	}
	return sum
}

// testCAPEM returns certificates in PEM that parse.
func testCAPEM(t *testing.T) []byte {
	t.Helper()
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
}

// TestPublishersAreMadeOnFirstUse: the publisher of a webhook and of a NATS
// server is made the first time it is asked for - decorated, without a
// connection or an address looked at - and is the same one the next time.
func TestPublishersAreMadeOnFirstUse(t *testing.T) {
	t.Parallel()
	p, kms, _, tel := testPublishers(t, nil)
	port := closedPort(t)
	for _, target := range []delivery.Target{
		testHook(t, kms, "hook", "https://127.0.0.1:"+port),
		testNATS(t, kms, "stream", "tls://127.0.0.1:"+port),
	} {
		first, err := p.Publisher(t.Context(), target)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if _, ok := first.(telemetry.Instrumented); !ok {
			t.Errorf("%s: the publisher %T is not decorated", target, first)
		}
		again, err := p.Publisher(t.Context(), target.Clone())
		if err != nil || again != first {
			t.Errorf("%s: asked again, a publisher %p (%v), want the first, %p", target, again, err, first)
		}
	}
	if got := egressRefusals(t, tel); got != 0 {
		t.Errorf("making publishers was %v failed connections; it connects to nothing", got)
	}
}

// TestPublishersRefuse: a target nothing publishes to, or whose
// certificates cannot be read, has no publisher.
func TestPublishersRefuse(t *testing.T) {
	t.Parallel()
	p, kms, _, _ := testPublishers(t, nil)
	for _, tt := range []struct {
		name   string
		target delivery.Target
	}{
		{name: "another deployment", target: delivery.Target{
			Name: "site-b", Kind: delivery.KindS3, Endpoint: "https://site-b.example", Region: "eu-1",
			Auth: delivery.Auth{Mode: delivery.AuthSigV4, ID: "AKIAEXAMPLE000000000"}, Secret: sealField(t, kms, "s", delivery.SecretAAD("site-b")),
		}},
		{name: "certificates that cannot be read", target: func() delivery.Target {
			hook := testHook(t, kms, "hook", "https://hooks.example.com")
			hook.CAPEM = []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
			return hook
		}()},
		{name: "a webhook without a signing key", target: func() delivery.Target {
			hook := testHook(t, kms, "hook", "https://hooks.example.com")
			hook.SignKey = iam.SealedSecret{}
			return hook
		}()},
	} {
		if publisher, err := p.Publisher(t.Context(), tt.target); err == nil || publisher != nil {
			t.Errorf("%s: Publisher = %v, %v; want a refusal", tt.name, publisher, err)
		} else if !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: %v, want %v", tt.name, err, domain.ErrInvalidArgument)
		}
	}
	if len(p.made) != 0 {
		t.Errorf("%d publishers kept of targets that have none", len(p.made))
	}
}

// TestPublishersFollowTheirTarget: a target whose endpoint, certificates,
// way of proving itself or sealed fields changed gets a new publisher and
// the one it replaces is closed; one whose buckets, audit flag, counts or
// times changed keeps its publisher.
func TestPublishersFollowTheirTarget(t *testing.T) {
	t.Parallel()
	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}
	ca := testCAPEM(t)
	tests := []struct {
		name   string
		kind   delivery.Kind
		change func(*delivery.Target)
		same   bool
	}{
		{name: "the buckets", kind: delivery.KindWebhook, same: true, change: func(t *delivery.Target) { t.Buckets = []string{"photos", "logs-*"} }},
		{name: "the audit trail", kind: delivery.KindNATS, same: true, change: func(t *delivery.Target) { t.Audit = true }},
		{name: "counts and times", kind: delivery.KindNATS, same: true, change: func(t *delivery.Target) {
			t.Refs, t.UpdatedAt = 3, time.Unix(1_700_000_000, 0)
		}},
		{name: "the endpoint of a webhook", kind: delivery.KindWebhook, change: func(t *delivery.Target) { t.Endpoint = "https://other.example" }},
		{name: "the path", kind: delivery.KindWebhook, change: func(tg *delivery.Target) {
			tg.SealedPath = sealField(t, kms, "/hooks/other", delivery.PathAAD(tg.Name))
		}},
		{name: "the signing key", kind: delivery.KindWebhook, change: func(tg *delivery.Target) {
			tg.SignKey = sealField(t, kms, "signing-key", delivery.SignKeyAAD(tg.Name))
		}},
		{name: "the certificates", kind: delivery.KindWebhook, change: func(t *delivery.Target) { t.CAPEM = ca }},
		{name: "the endpoint of a NATS server", kind: delivery.KindNATS, change: func(t *delivery.Target) { t.Endpoint = "tls://other.example:4222" }},
		{name: "the subject", kind: delivery.KindNATS, change: func(t *delivery.Target) { t.Subject = "cactusfs.other" }},
		{name: "the token, sealed again", kind: delivery.KindNATS, change: func(tg *delivery.Target) {
			tg.Secret = sealField(t, kms, "nats-token", delivery.SecretAAD(tg.Name))
		}},
		{name: "the way it proves itself", kind: delivery.KindNATS, change: func(t *delivery.Target) { t.Auth = delivery.Auth{Mode: delivery.AuthBasic, ID: "cactusfs"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, _, _, _ := testPublishers(t, nil)
			p.kms = kms
			target := testHook(t, kms, "target", "https://hooks.example.com")
			if tt.kind == delivery.KindNATS {
				target = testNATS(t, kms, "target", "tls://nats.example:4222")
			}
			first, err := p.Publisher(t.Context(), target)
			if err != nil {
				t.Fatal(err)
			}
			changed := target.Clone()
			tt.change(&changed)
			second, err := p.Publisher(t.Context(), changed)
			if err != nil {
				t.Fatal(err)
			}
			if (second == first) != tt.same {
				t.Fatalf("the same publisher after a change of %s: %v, want %v", tt.name, second == first, tt.same)
			}
			if tt.same || tt.kind != delivery.KindNATS {
				return
			}
			// The NATS publisher replaced is closed: it publishes nothing
			// more, and connects to nothing to say so.
			if err := first.Publish(t.Context(), testMessage()); !errors.Is(err, nats.ErrClosed) {
				t.Errorf("the replaced publisher publishes: %v, want %v", err, nats.ErrClosed)
			}
		})
	}
}

// TestPublishersAreTidied: the publisher of a target that is gone is
// closed, those of targets that are there are kept, and every one is closed
// when the node does not lead.
func TestPublishersAreTidied(t *testing.T) {
	t.Parallel()
	leads := true
	p, kms, meta, _ := testPublishers(t, func() bool { return leads })
	kept, gone := testNATS(t, kms, "kept", "tls://nats.example:4222"), testNATS(t, kms, "gone", "tls://nats.example:4222")
	if err := meta.PutTarget(t.Context(), kept, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	keptPublisher, err := p.Publisher(t.Context(), kept)
	if err != nil {
		t.Fatal(err)
	}
	gonePublisher, err := p.Publisher(t.Context(), gone)
	if err != nil {
		t.Fatal(err)
	}

	p.tidy(t.Context())
	if err := gonePublisher.Publish(t.Context(), testMessage()); !errors.Is(err, nats.ErrClosed) {
		t.Errorf("the publisher of a target that is gone publishes: %v", err)
	}
	if again, err := p.Publisher(t.Context(), kept); err != nil || again != keptPublisher {
		t.Errorf("the publisher of a target that is there was not kept: %v", err)
	}

	leads = false
	p.tidy(t.Context())
	if err := keptPublisher.Publish(t.Context(), testMessage()); !errors.Is(err, nats.ErrClosed) {
		t.Errorf("a publisher is open on a node that does not lead: %v", err)
	}
	if len(p.made) != 0 {
		t.Errorf("%d publishers kept on a node that does not lead", len(p.made))
	}
}

// TestPublishersRunUntilTheNodeStops: Run closes every publisher when it
// ends.
func TestPublishersRunUntilTheNodeStops(t *testing.T) {
	t.Parallel()
	p, kms, _, _ := testPublishers(t, nil)
	publisher, err := p.Publisher(t.Context(), testNATS(t, kms, "stream", "tls://nats.example:4222"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Run(ctx)
	}()
	cancel()
	<-done
	if err := publisher.Publish(t.Context(), testMessage()); !errors.Is(err, nats.ErrClosed) {
		t.Errorf("a publisher is open after Run ended: %v", err)
	}
}
