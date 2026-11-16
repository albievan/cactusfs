package telemetry

import (
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/ports"
)

// The telemetry of delivery to targets (ADR-0023). No metric names a target
// or a bucket: delivery is counted by the kind of the target and the result,
// and what is known of one target is read from GET /targets/{name}/status.
// The words of a label are a set this file fixes; anything else is "other".

// labelOther stands, in a label of delivery, for a word this release does
// not know.
const labelOther = "other"

// deliveryKinds are the values of the kind label.
func deliveryKinds() []string {
	return []string{string(delivery.KindWebhook), string(delivery.KindNATS), string(delivery.KindS3)}
}

// deliveryStates are the values of the state label of
// cactusfs_delivery_targets.
func deliveryStates() []string {
	return []string{ports.DeliveryOK, ports.DeliveryRetrying, ports.DeliveryStalled}
}

// deliveryClasses are the classes of failure a label takes; every other is
// "other".
func deliveryClasses() []string {
	return []string{
		string(delivery.ClassRejected), string(delivery.ClassSSEC), string(delivery.ClassTimeout), string(delivery.ClassConnect),
		string(delivery.ClassTLS), string(delivery.ClassServer), string(delivery.ClassRedirect), string(delivery.ClassEgressRefused),
		string(delivery.ClassAuth), string(delivery.ClassNoStream), string(delivery.ClassNoSuchBucket), string(delivery.ClassVersioning),
		string(delivery.ClassNotReplica), string(delivery.ClassQuota), string(delivery.ClassInternal),
	}
}

// egressClasses are the classes a request that the egress client sends can
// fail with: those of the connection, never of the answer.
func egressClasses() []string {
	return []string{
		string(delivery.ClassEgressRefused), string(delivery.ClassRedirect), string(delivery.ClassTLS), string(delivery.ClassTimeout),
		string(delivery.ClassConnect), string(delivery.ClassInternal),
	}
}

// word returns w when it is one of known, and "other" otherwise.
func word(w string, known []string) string {
	for _, k := range known {
		if w == k {
			return w
		}
	}
	return labelOther
}

func newDeliveryAttempts() *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cactusfs_delivery_attempts_total",
		Help: "Attempts this node made, while it led the metadata group, to hand an item of the outbox to a target, by kind of target and result: ok, or the class of the failure.",
	}, []string{"kind", "result"})
}

func newEgressRefusals() *prometheus.CounterVec {
	refusals := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cactusfs_egress_refusals_total",
		Help: "Requests to a place outside the deployment that got no answer, and connections to one that could not be made, by class: egress_refused (an address the policy refuses), redirect, tls, timeout, connect, internal.",
	}, []string{"class"})
	// The first refusal is the one that matters: every class is a series
	// from the start.
	for _, class := range egressClasses() {
		refusals.WithLabelValues(class)
	}
	return refusals
}

// ObserveDeliveryAttempt counts one attempt at one item
// (app.DeliveryWorkerConfig.OnAttempt): the kind of the target, and the
// class of the failure or delivery.ClassNone. Which target is not said.
func (t *Telemetry) ObserveDeliveryAttempt(kind delivery.Kind, class delivery.Class) {
	result := resultOK
	if class != delivery.ClassNone {
		result = word(string(class), deliveryClasses())
	}
	t.deliveryAttempts.WithLabelValues(word(string(kind), deliveryKinds()), result).Inc()
}

// DeliveryState is how delivery stands, as its gauges show it: counts, with
// no target named.
type DeliveryState struct {
	// Targets counts the targets by kind and state.
	Targets map[string]map[string]int
	// OldestPending is, by kind of target, how long the oldest item that
	// waits has waited.
	OldestPending map[string]time.Duration
	// Rows is how many rows the outbox holds; Dropped how many were pushed
	// out at its bound since the deployment began.
	Rows, Dropped uint64
}

// DeliverySource says how delivery stands. It reports false when it cannot
// say: on every node but the one that leads the metadata group, which alone
// delivers.
type DeliverySource interface {
	DeliveryState() (DeliveryState, bool)
}

// deliveryAnswer is what a delivery source said, and whether it could say.
type deliveryAnswer struct {
	state DeliveryState
	known bool
}

// deliveryGauges is the collector of the gauges of delivery, read at scrape
// time from the source the composition root attaches, through a probe as the
// gauges of state.go are: a scrape waits for it at most its timeout, and the
// series are absent when it does not answer or cannot say.
type deliveryGauges struct {
	timeout time.Duration
	source  atomic.Pointer[DeliverySource]
	probe   probe[deliveryAnswer]

	targets, oldest, rows, dropped *prometheus.Desc
}

func newDeliveryGauges(timeout time.Duration) *deliveryGauges {
	if timeout <= 0 {
		timeout = stateTimeout
	}
	return &deliveryGauges{
		timeout: timeout,
		targets: prometheus.NewDesc("cactusfs_delivery_targets",
			"Targets by kind and state of delivery (ok, retrying, stalled), as the node that leads the metadata group has them; absent on every other node.",
			[]string{"kind", "state"}, nil),
		oldest: prometheus.NewDesc("cactusfs_delivery_oldest_pending_seconds",
			"How long the oldest item of the outbox that waits for a target of the kind has waited; 0 when none waits. Absent on every node but the one that leads.",
			[]string{"kind"}, nil),
		rows: prometheus.NewDesc("cactusfs_outbox_rows",
			"Rows the outbox holds, as the node that leads read them last; absent on every other node.", nil, nil),
		dropped: prometheus.NewDesc("cactusfs_outbox_dropped_total",
			"Rows pushed out of the outbox at its bound since the deployment began, as the metadata counts them; absent on every node but the one that leads.",
			nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (g *deliveryGauges) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{g.targets, g.oldest, g.rows, g.dropped} {
		ch <- d
	}
}

// Collect implements prometheus.Collector. Every kind and state is a series
// when the source answers, 0 where it counts none.
func (g *deliveryGauges) Collect(ch chan<- prometheus.Metric) {
	source := g.source.Load()
	if source == nil {
		return
	}
	asked := g.probe.ask(source, func() deliveryAnswer {
		state, known := (*source).DeliveryState()
		return deliveryAnswer{state: state, known: known}
	})
	expired, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()
	answer, ok := asked.wait(expired, source)
	if !ok || !answer.known {
		return
	}
	s := answer.state
	counts := map[string]map[string]int{}
	oldest := map[string]time.Duration{}
	for kind, states := range s.Targets {
		kind = word(kind, deliveryKinds())
		if counts[kind] == nil {
			counts[kind] = map[string]int{}
		}
		for state, n := range states {
			counts[kind][word(state, deliveryStates())] += n
		}
	}
	for kind, waited := range s.OldestPending {
		kind = word(kind, deliveryKinds())
		oldest[kind] = max(oldest[kind], waited)
	}
	// Every kind and state this release knows is a series; a word it does
	// not know is one more, as "other", when it is counted.
	for _, kind := range append(deliveryKinds(), labelOther) {
		for _, state := range append(deliveryStates(), labelOther) {
			n, counted := counts[kind][state]
			if (kind == labelOther || state == labelOther) && !counted {
				continue
			}
			ch <- prometheus.MustNewConstMetric(g.targets, prometheus.GaugeValue, float64(n), kind, state)
		}
		if waited, counted := oldest[kind]; kind != labelOther || counted {
			ch <- prometheus.MustNewConstMetric(g.oldest, prometheus.GaugeValue, waited.Seconds(), kind)
		}
	}
	ch <- prometheus.MustNewConstMetric(g.rows, prometheus.GaugeValue, float64(s.Rows))
	ch <- prometheus.MustNewConstMetric(g.dropped, prometheus.CounterValue, float64(s.Dropped))
}

// ObserveDelivery attaches the source of the gauges of delivery; nil
// detaches it.
func (t *Telemetry) ObserveDelivery(source DeliverySource) {
	if source == nil {
		t.delivery.source.Store(nil)
		return
	}
	t.delivery.source.Store(&source)
}

// InstrumentOutbox returns next wrapped so that every call is traced and
// counted. Neither a target nor a bucket is a label or an attribute.
func (t *Telemetry) InstrumentOutbox(next ports.Outbox) ports.Outbox {
	return &outbox{t: t, next: next}
}

type outbox struct {
	t    *Telemetry
	next ports.Outbox
}

const portOutbox = "outbox"

func (*outbox) instrumented() {}

// ReadOutbox implements ports.Outbox.
func (o *outbox) ReadOutbox(ctx context.Context, target string, after delivery.Seq, limit int) ([]delivery.Item, delivery.Seq, error) {
	ctx, done := o.t.call(ctx, portOutbox, "ReadOutbox")
	items, upto, err := o.next.ReadOutbox(ctx, target, after, limit)
	done(err)
	return items, upto, err
}

// AckOutbox implements ports.Outbox.
func (o *outbox) AckOutbox(ctx context.Context, target string, upto delivery.Seq, auditUpto uint64, results []delivery.Result) (int, error) {
	ctx, done := o.t.call(ctx, portOutbox, "AckOutbox")
	unrelated, err := o.next.AckOutbox(ctx, target, upto, auditUpto, results)
	done(err)
	return unrelated, err
}

// OutboxCursors implements ports.Outbox.
func (o *outbox) OutboxCursors(ctx context.Context) (ports.OutboxState, error) {
	ctx, done := o.t.call(ctx, portOutbox, "OutboxCursors")
	state, err := o.next.OutboxCursors(ctx)
	done(err)
	return state, err
}

// InstrumentDeliveryStatus returns next wrapped so that every question to
// the node that leads is traced and counted. Neither the targets asked about
// nor anything of the answer is a label or an attribute.
func (t *Telemetry) InstrumentDeliveryStatus(next ports.DeliveryStatusReader) ports.DeliveryStatusReader {
	return &deliveryStatus{t: t, next: next}
}

type deliveryStatus struct {
	t    *Telemetry
	next ports.DeliveryStatusReader
}

const portDeliveryStatus = "delivery_status"

func (*deliveryStatus) instrumented() {}

// DeliveryStatus implements ports.DeliveryStatusReader.
func (d *deliveryStatus) DeliveryStatus(ctx context.Context, targets []string) ([]ports.TargetStatus, error) {
	ctx, done := d.t.call(ctx, portDeliveryStatus, "DeliveryStatus")
	statuses, err := d.next.DeliveryStatus(ctx, targets)
	done(err)
	return statuses, err
}

// ObserveEgress is the observer of the egress client (egress.Observer,
// given with egress.Client.Observe): every request is a span "egress.Do",
// from when it is sent to when the body of its answer is closed, and every
// request that gets no answer is counted by its class
// (cactusfs_egress_refusals_total). Nothing of the request - its URL, its
// host, the target it is for - is a label or an attribute: a path may hold
// a token, and a target's name is chosen by somebody.
func (t *Telemetry) ObserveEgress(ctx context.Context) (context.Context, func(error)) {
	ctx, span := t.tracer.Start(ctx, "egress.Do", trace.WithSpanKind(trace.SpanKindClient))
	return ctx, func(err error) {
		if err != nil {
			class := word(string(delivery.ClassOf(err)), egressClasses())
			t.egressRefusals.WithLabelValues(class).Inc()
			span.SetStatus(codes.Error, class)
		}
		span.End()
	}
}

// InstrumentEgressDial returns next wrapped so that every connection it
// makes is a span and every one it cannot make is counted by its class
// (cactusfs_egress_refusals_total), as ObserveEgress does for requests:
// next is the dial function of the egress client, which a client of another
// protocol than HTTP - NATS - connects with. The span covers the dial
// alone; what the connection carries afterwards is the client's. Nothing
// of the address is a label or an attribute. A nil next is returned as it
// is.
func (t *Telemetry) InstrumentEgressDial(
	next func(ctx context.Context, network, address string) (net.Conn, error),
) func(ctx context.Context, network, address string) (net.Conn, error) {
	if next == nil {
		return nil
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		ctx, span := t.tracer.Start(ctx, "egress.Dial", trace.WithSpanKind(trace.SpanKindClient))
		defer span.End()
		conn, err := next(ctx, network, address)
		if err != nil {
			class := word(string(delivery.ClassOf(err)), egressClasses())
			t.egressRefusals.WithLabelValues(class).Inc()
			span.SetStatus(codes.Error, class)
			return nil, err
		}
		return conn, nil
	}
}
