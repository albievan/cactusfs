package telemetry_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// fixedDelivery says how delivery stands, or that it cannot say.
type fixedDelivery struct {
	state telemetry.DeliveryState
	known bool
}

func (d fixedDelivery) DeliveryState() (telemetry.DeliveryState, bool) { return d.state, d.known }

// The gauges of delivery, by name.
var deliveryGauges = []string{
	"cactusfs_delivery_targets", "cactusfs_delivery_oldest_pending_seconds", "cactusfs_outbox_rows", "cactusfs_outbox_dropped_total",
}

// TestDeliveryGauges: on the node that leads every kind and state is a
// series, 0 where nothing is counted, and a word this release does not know
// is "other"; on every other node, and with no source, the series are
// absent.
func TestDeliveryGauges(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil)
	absent := func(when string) {
		t.Helper()
		for name := range gauges(t, tel) {
			for _, gauge := range deliveryGauges {
				if strings.HasPrefix(name, gauge+"{") {
					t.Errorf("%s: %s is exposed", when, name)
				}
			}
		}
	}
	absent("without a source")
	tel.ObserveDelivery(fixedDelivery{known: false})
	absent("on a node that does not lead")

	tel.ObserveDelivery(fixedDelivery{known: true, state: telemetry.DeliveryState{
		Targets: map[string]map[string]int{
			"webhook": {"ok": 2, "stalled": 1}, "s3": {"retrying": 1}, "carrier-pigeon": {"flying": 3},
		},
		OldestPending: map[string]time.Duration{"webhook": 90 * time.Second, "carrier-pigeon": time.Hour},
		Rows:          1234, Dropped: 5,
	}})
	got := gauges(t, tel)
	for name, want := range map[string]float64{
		"cactusfs_delivery_targets{kind=webhook,state=ok}":       2,
		"cactusfs_delivery_targets{kind=webhook,state=stalled}":  1,
		"cactusfs_delivery_targets{kind=webhook,state=retrying}": 0,
		"cactusfs_delivery_targets{kind=s3,state=retrying}":      1,
		"cactusfs_delivery_targets{kind=nats,state=ok}":          0,
		"cactusfs_delivery_targets{kind=other,state=other}":      3,
		"cactusfs_delivery_oldest_pending_seconds{kind=webhook}": 90,
		"cactusfs_delivery_oldest_pending_seconds{kind=nats}":    0,
		"cactusfs_delivery_oldest_pending_seconds{kind=s3}":      0,
		"cactusfs_delivery_oldest_pending_seconds{kind=other}":   3600,
		"cactusfs_outbox_rows{}":                                 1234,
		"cactusfs_outbox_dropped_total{}":                        5,
	} {
		if value, ok := got[name]; !ok || value != want {
			t.Errorf("%s = %v (exposed %v), want %v", name, value, ok, want)
		}
	}
	if _, ok := got["cactusfs_delivery_targets{kind=other,state=ok}"]; ok {
		t.Error("a series of a word nobody counted is exposed")
	}

	tel.ObserveDelivery(nil)
	absent("once the source is detached")
}

// TestDeliveryAttemptsAreCounted: an attempt is counted by the kind of its
// target and its result, ok or the class of its failure; a word this release
// does not know is "other", so that the labels stay a set.
func TestDeliveryAttemptsAreCounted(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil)
	tel.ObserveDeliveryAttempt(delivery.KindWebhook, delivery.ClassNone)
	tel.ObserveDeliveryAttempt(delivery.KindWebhook, delivery.ClassNone)
	tel.ObserveDeliveryAttempt(delivery.KindNATS, delivery.ClassNoStream)
	tel.ObserveDeliveryAttempt(delivery.KindS3, delivery.Class("a-class-of-later"))
	tel.ObserveDeliveryAttempt(delivery.Kind("pigeon"), delivery.ClassTimeout)
	got := gauges(t, tel)
	for name, want := range map[string]float64{
		"cactusfs_delivery_attempts_total{kind=webhook,result=ok}":     2,
		"cactusfs_delivery_attempts_total{kind=nats,result=no_stream}": 1,
		"cactusfs_delivery_attempts_total{kind=s3,result=other}":       1,
		"cactusfs_delivery_attempts_total{kind=other,result=timeout}":  1,
	} {
		if got[name] != want {
			t.Errorf("%s = %v, want %v", name, got[name], want)
		}
	}
}

// TestOutboxAndDeliveryStatusAreCountedAndTraced: the outbox and the reader
// of the status of delivery keep their contracts through their decorators,
// every method is counted under its own name - and nothing of a target, an
// item or a status is in a metric or a span.
func TestOutboxAndDeliveryStatusAreCountedAndTraced(t *testing.T) {
	t.Parallel()
	recorder := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, recorder)
	ctx := context.Background()

	t.Run("contract", func(t *testing.T) {
		porttest.DeliveryStatusContract(t, func(_ *testing.T, script porttest.DeliveryStatusScript) ports.DeliveryStatusReader {
			return tel.InstrumentDeliveryStatus(porttest.ScriptedDeliveryStatusReader(script))
		})
	})

	mem := porttest.NewMemMetadata()
	mem.Audit = porttest.NewMemIAM()
	if err := mem.CreateBucket(ctx, bucket.Bucket{Name: "tenant-secret-bucket"}); err != nil {
		t.Fatal(err)
	}
	if err := mem.PutTarget(ctx, porttest.SampleTarget("hook-secret-name", delivery.KindWebhook), audit.Record{}); err != nil {
		t.Fatal(err)
	}
	outbox := tel.InstrumentOutbox(mem)
	if _, _, err := outbox.ReadOutbox(ctx, "hook-secret-name", delivery.Seq{}, 10); err != nil {
		t.Fatalf("ReadOutbox: %v", err)
	}
	if _, _, err := outbox.ReadOutbox(ctx, "missing-secret-name", delivery.Seq{}, 10); !errors.Is(err, domain.ErrEntityNotFound) {
		t.Fatalf("ReadOutbox of no target: %v", err)
	}
	if _, err := outbox.AckOutbox(ctx, "hook-secret-name", delivery.Seq{}, 0, nil); err != nil {
		t.Fatalf("AckOutbox: %v", err)
	}
	if _, err := outbox.OutboxCursors(ctx); err != nil {
		t.Fatalf("OutboxCursors: %v", err)
	}

	metrics := scrape(t, tel)
	wantEveryMethod(t, metrics, "outbox", methodsOf[ports.Outbox]())
	wantEveryMethod(t, metrics, "delivery_status", methodsOf[ports.DeliveryStatusReader]())
	for _, want := range []string{
		"cactusfs_port_calls_total method=ReadOutbox port=outbox result=error",
		"cactusfs_port_calls_total method=DeliveryStatus port=delivery_status result=ok",
		"cactusfs_port_calls_total method=DeliveryStatus port=delivery_status result=error",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	for _, span := range []string{"outbox.ReadOutbox", "outbox.AckOutbox", "delivery_status.DeliveryStatus"} {
		if !contains(spanNames(recorder), span) {
			t.Errorf("no span named %s among %v", span, spanNames(recorder))
		}
	}
	wantNothingOf(t, tel, recorder, "secret", "site-b", "MARK-7c1e", "hooks.example")
}

// egressAnswer is an egress client that answers as it is told.
type egressAnswer struct {
	err    error
	closed int
}

func (e *egressAnswer) Do(r *http.Request) (*http.Response, error) {
	if e.err != nil {
		return nil, e.err
	}
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: r}, nil
}

func (e *egressAnswer) CloseIdleConnections() { e.closed++ }

// TestEgressRefusalsAreCountedByClass: a request to a place outside the
// deployment that gets no answer is counted by the class of the failure, one
// that is answered is not, every class is a series from the start - and
// nothing of the request, its host, its path or the words of the error, is
// in a metric or a span.
func TestEgressRefusalsAreCountedByClass(t *testing.T) {
	t.Parallel()
	recorder := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, recorder)
	const target = "https://MARK-egress.example/hooks/token-MARK-path"

	before := gauges(t, tel)
	for _, class := range []string{"egress_refused", "redirect", "tls", "timeout", "connect", "internal"} {
		if value, ok := before["cactusfs_egress_refusals_total{class="+class+"}"]; !ok || value != 0 {
			t.Errorf("class %s before any request: %v, %v", class, value, ok)
		}
	}
	for _, failure := range []error{
		&url.Error{Op: "Post", URL: "https://MARK-egress.example", Err: delivery.Failed(delivery.ClassEgressRefused, errors.New("MARK-egress refused"))},
		delivery.Failed(delivery.ClassEgressRefused, errors.New("again")),
		delivery.Failed(delivery.ClassTLS, errors.New("x509: MARK-egress")),
		errors.New("no class at all: MARK-egress"),
		nil,
	} {
		client := &egressAnswer{err: failure}
		instrumented := tel.InstrumentEgress(client)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := instrumented.Do(req)
		status := 0
		if resp != nil {
			status = resp.StatusCode
			// The span covers the reading of the body: it ends when the
			// body is closed, and not before.
			ended := len(recorder.Ended())
			if len(recorder.Started()) != ended+1 {
				t.Error("the span of a request ended before the body of its answer was closed")
			}
			_ = resp.Body.Close()
			_ = resp.Body.Close()
			if len(recorder.Ended()) != ended+1 {
				t.Errorf("%d spans ended when the body was closed twice, want 1", len(recorder.Ended())-ended)
			}
		}
		switch {
		case failure == nil && (err != nil || status != http.StatusNoContent):
			t.Errorf("an answered request = %d, %v", status, err)
		case failure != nil && (!errors.Is(err, failure) || status != 0):
			t.Errorf("a failed request = %d, %v; want %v", status, err, failure)
		}
		instrumented.CloseIdleConnections()
		if client.closed != 1 {
			t.Error("CloseIdleConnections is not passed on")
		}
	}
	got := gauges(t, tel)
	for class, want := range map[string]float64{"egress_refused": 2, "tls": 1, "internal": 1, "timeout": 0} {
		if value := got["cactusfs_egress_refusals_total{class="+class+"}"]; value != want {
			t.Errorf("refusals of class %s = %v, want %v", class, value, want)
		}
	}
	if !contains(spanNames(recorder), "egress.Do") {
		t.Errorf("no span named egress.Do among %v", spanNames(recorder))
	}
	if _, ok := tel.InstrumentEgress(&egressAnswer{}).(telemetry.Instrumented); !ok {
		t.Error("the decorator of the egress client is not telemetry.Instrumented")
	}
	wantNothingOf(t, tel, recorder, "MARK", "hooks", "token")
}
