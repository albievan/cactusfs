package telemetry

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// The telemetry of replication between sites (ADR-0026; PLAN 8.1.7). No
// metric names a bucket, a key, a version or a target: what is known of one
// bucket is read from GET /buckets/{bucket}/replication/status, and of one
// target from GET /targets/{name}/status. The words of a label are a set
// this file fixes.

// The results of cactusfs_replication_versions_total: the statuses the
// replicator gives a version.
const (
	replicationCompleted = "completed"
	replicationFailed    = "failed"
)

// resyncEvents are the values of the event label of
// cactusfs_replication_resync_events_total.
func resyncEvents() []string {
	return []string{app.ResyncEventHeld, app.ResyncEventPreempted, app.ResyncEventRestarted}
}

// resyncStates are the values of the state label of
// cactusfs_replication_resyncs.
func resyncStates() []string {
	return []string{app.ResyncWalking, app.ResyncHeld, app.ResyncWaiting}
}

// replicationMetrics are the counters of replication, every series there
// from the start, and the gauges of the node that leads.
type replicationMetrics struct {
	bytes    prometheus.Counter
	versions *prometheus.CounterVec
	resync   *prometheus.CounterVec
	gauges   *replicationGauges
}

func newReplicationMetrics(timeout time.Duration) *replicationMetrics {
	m := &replicationMetrics{
		bytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cactusfs_replication_bytes_total",
			Help: "Bytes of versions and parts this node read into requests to other deployments, whatever became of the request: the traffic of replication, which replication-rate-mib bounds.",
		}),
		versions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cactusfs_replication_versions_total",
			Help: "Versions this node, while it led the metadata group, found at or sent to another deployment (completed), or gave up on (failed: a version encrypted with a customer's key, or one the other deployment refuses for itself), by result.",
		}, []string{"result"}),
		resync: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cactusfs_replication_resync_events_total",
			Help: "What befell the resyncs this node walked while it led the metadata group, by event: held (a pass held at a version by a failure that holds the target), preempted (a send the outbox ended, to send that object itself), restarted (a walk begun again because its target lost rows meanwhile).",
		}, []string{"event"}),
		gauges: newReplicationGauges(timeout),
	}
	for _, result := range []string{replicationCompleted, replicationFailed} {
		m.versions.WithLabelValues(result)
	}
	for _, event := range resyncEvents() {
		m.resync.WithLabelValues(event)
	}
	return m
}

// collectors returns what is registered.
func (m *replicationMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.bytes, m.versions, m.resync, m.gauges}
}

// ObserveReplicatedVersion counts a status of replication the replicator
// gave a version (app.ReplicatorConfig.OnVersion): COMPLETED or FAILED. Any
// other status is not counted. Which version, and which target, is not said.
func (t *Telemetry) ObserveReplicatedVersion(status object.ReplicationStatus) {
	switch status {
	case object.ReplicationCompleted:
		t.replication.versions.WithLabelValues(replicationCompleted).Inc()
	case object.ReplicationFailed:
		t.replication.versions.WithLabelValues(replicationFailed).Inc()
	default:
	}
}

// ObserveResyncEvent counts an event of a resync (app.ResyncWorkerConfig.
// OnEvent). An event this release does not know is not counted.
func (t *Telemetry) ObserveResyncEvent(event string) {
	if word(event, resyncEvents()) == labelOther {
		return
	}
	t.replication.resync.WithLabelValues(event).Inc()
}

// replicatedBody is the body of a version or of a part sent to another
// deployment, counted into cactusfs_replication_bytes_total as the client
// reads it.
type replicatedBody struct {
	next    io.Reader
	counter prometheus.Counter
}

// Read implements io.Reader.
func (b replicatedBody) Read(p []byte) (int, error) {
	n, err := b.next.Read(p)
	if n > 0 {
		b.counter.Add(float64(n))
	}
	return n, err
}

// countedBody returns body counted as replication's; nil stays nil.
func (t *Telemetry) countedBody(body io.Reader) io.Reader {
	if body == nil {
		return nil
	}
	return replicatedBody{next: body, counter: t.replication.bytes}
}

// ReplicationState is how replication stands on the node that leads, as its
// gauges show it: counts, with no bucket or target named.
type ReplicationState struct {
	// Pending counts the rows of the outbox that wait for a target of kind
	// s3 - another deployment - all such targets together.
	Pending uint64
	// OldestPending is how long the oldest of them has waited; 0 when none
	// waits.
	OldestPending time.Duration
	// Resyncs counts the resyncs this node walks by state:
	// app.ResyncWalking, app.ResyncHeld, app.ResyncWaiting.
	Resyncs map[string]int
}

// ReplicationSource says how replication stands. It reports false when it
// cannot say: on every node but the one that leads the metadata group,
// which alone sends.
type ReplicationSource interface {
	ReplicationState() (ReplicationState, bool)
}

// replicationAnswer is what a replication source said, and whether it could
// say.
type replicationAnswer struct {
	state ReplicationState
	known bool
}

// replicationGauges is the collector of the gauges of replication, read at
// scrape time from the source the composition root attaches, through a
// probe as the gauges of delivery are: a scrape waits for it at most its
// timeout, and the series are absent when it does not answer or cannot say.
type replicationGauges struct {
	timeout time.Duration
	source  atomic.Pointer[ReplicationSource]
	probe   probe[replicationAnswer]

	entries, seconds, resyncs *prometheus.Desc
}

func newReplicationGauges(timeout time.Duration) *replicationGauges {
	if timeout <= 0 {
		timeout = stateTimeout
	}
	return &replicationGauges{
		timeout: timeout,
		entries: prometheus.NewDesc("cactusfs_replication_lag_entries",
			"Rows of the outbox that wait to be sent to another deployment, all such targets together, as the node that leads the metadata group has them; absent on every other node.",
			nil, nil),
		seconds: prometheus.NewDesc("cactusfs_replication_lag_seconds",
			"How long the oldest row of the outbox that waits to be sent to another deployment has waited; 0 when none waits. Absent on every node but the one that leads.",
			nil, nil),
		resyncs: prometheus.NewDesc("cactusfs_replication_resyncs",
			"Resyncs the node that leads the metadata group walks, by state: walking, held (by a failure, retrying or stalled), waiting (for a configuration, a target, an object the outbox sends, or the other buckets of the target); absent on every other node.",
			[]string{"state"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (g *replicationGauges) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{g.entries, g.seconds, g.resyncs} {
		ch <- d
	}
}

// Collect implements prometheus.Collector. Every state is a series when the
// source answers, 0 where it counts none; a state this release does not
// know is not shown.
func (g *replicationGauges) Collect(ch chan<- prometheus.Metric) {
	source := g.source.Load()
	if source == nil {
		return
	}
	asked := g.probe.ask(source, func() replicationAnswer {
		state, known := (*source).ReplicationState()
		return replicationAnswer{state: state, known: known}
	})
	expired, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()
	answer, ok := asked.wait(expired, source)
	if !ok || !answer.known {
		return
	}
	s := answer.state
	ch <- prometheus.MustNewConstMetric(g.entries, prometheus.GaugeValue, float64(s.Pending))
	ch <- prometheus.MustNewConstMetric(g.seconds, prometheus.GaugeValue, max(s.OldestPending, 0).Seconds())
	for _, state := range resyncStates() {
		ch <- prometheus.MustNewConstMetric(g.resyncs, prometheus.GaugeValue, float64(s.Resyncs[state]), state)
	}
}

// ObserveReplication attaches the source of the gauges of replication; nil
// detaches it.
func (t *Telemetry) ObserveReplication(source ReplicationSource) {
	if source == nil {
		t.replication.gauges.source.Store(nil)
		return
	}
	t.replication.gauges.source.Store(&source)
}
