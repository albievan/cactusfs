package telemetry

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// The gauges of this file say how the node is, not what it did: they are
// read when the metrics are scraped, from sources the composition root
// attaches once the things they describe exist. A gauge whose source is not
// attached - the node is starting, or has no such thing - is not exposed: an
// absent series, not a zero, is what "not known" looks like.
//
// A source is meant to answer from memory or from this node's own drives,
// and none is trusted to: the metadata store reads its state under a lock
// that a snapshot being restored holds for as long as it takes, a drive may
// hang. So every source is asked through a probe (see probe), and what is
// guaranteed is this, whatever a source does:
//
//   - a scrape waits stateTimeout for all the sources together, not for
//     each, and then goes on without those that have not answered;
//   - the series of a source that has not answered are not exposed in that
//     scrape - absent, not zero: a node that cannot say whether it leads has
//     not said that it does not. The one exception is cactusfs_drive_online,
//     which is 0 for every drive the last answer named: a drive that does not
//     answer cannot be reached, and that is what the gauge says;
//   - one question is out to a source at a time. A source that hangs holds
//     one goroutine, however often the node is scraped, and is not asked
//     again until it has answered.
const stateTimeout = 2 * time.Second

// Reasons a node refused the handshake of another. They are the only values
// of the reason label.
const (
	// HandshakeCertificate: no certificate was shown, or not one of the
	// node CA, or not one that names a node.
	HandshakeCertificate = "certificate"
	// HandshakeNotAdmitted: the certificate is a node's, and that node may
	// not connect here - the Raft listener admits only voters.
	HandshakeNotAdmitted = "not_admitted"
	// HandshakeTimeout: the peer connected and did not finish in time.
	HandshakeTimeout = "timeout"
	// HandshakeOther is everything else: a connection that was closed
	// half-way, a peer that does not speak TLS 1.3.
	HandshakeOther = "other"
)

// RaftState is this node's part of the metadata group, as its gauges show
// it.
type RaftState struct {
	// Leader reports whether this node leads the group.
	Leader bool
	// Term, AppliedIndex and CommitIndex are Raft's own numbers.
	Term, AppliedIndex, CommitIndex uint64
	// Voters is the number of voters the node knows of.
	Voters int
	// SchemaLevel is the newest metadata entry schema the cluster writes
	// now, SchemaSupported the newest this binary applies, StateSchema the
	// schema this node's state has. Zero means not known, and the gauge is
	// left out.
	SchemaLevel, SchemaSupported int
	StateSchema                  uint64
}

// RaftSource says how this node's part of the metadata group is. It reports
// false when it cannot say.
type RaftSource interface {
	RaftState() (RaftState, bool)
}

// DriveState is one of this node's drives, as its gauges show it.
type DriveState struct {
	// ID is the drive's generated ID - never the directory it is in.
	ID string
	// Online reports whether the drive can be reached.
	Online bool
	// FreeBytes and TotalBytes are the room on the drive's file system;
	// they are exposed for a drive that is online.
	FreeBytes, TotalBytes float64
}

// DriveSource says how this node's drives are.
type DriveSource interface {
	DriveStates() []DriveState
}

// CertificateSource says until when the certificate this node shows the
// other nodes is valid. It reports false when the node has none.
type CertificateSource interface {
	NodeCertificateNotAfter() (time.Time, bool)
}

// The certificates a node shows, as the use label names them.
const (
	// TLSUseNode is the certificate the node shows the other nodes, on node
	// RPC and on Raft.
	TLSUseNode = "node"
	// TLSUseS3 is the certificate of the listeners clients reach: the S3
	// API, and the management API and console with it.
	TLSUseS3 = "s3"
)

// tlsUses and tlsReloadReasons are the only values of the use and reason
// labels of the cactusfs_tls_* metrics.
func tlsUses() []string { return []string{TLSUseNode, TLSUseS3} }

// TLSReloadReasons returns every reason a certificate found on disk is
// refused for, as cactusfs_tls_reload_refused_total labels them. They are
// the reasons of the adapter that reads the files; the composition root has
// a test that holds the two sets to each other.
func TLSReloadReasons() []string {
	return []string{"unreadable", "pair", "not_yet_valid", "expired", "identity", "untrusted", "usage"}
}

// TLSState is one of the node's certificates, and what became of the files
// it is read from again, as their gauges show it.
type TLSState struct {
	// NotAfter is when the certificate in use ends.
	NotAfter time.Time
	// Pending reports that what is on disk is not what is in use: it was
	// refused, and the certificate from before is still shown.
	Pending bool
	// Refused is how many distinct contents of the files were refused since
	// the node started, by reason: one of TLSReloadReasons. Any other
	// reason is left out.
	Refused map[string]uint64
}

// TLSSource says how one of the node's certificates is.
type TLSSource interface {
	TLSState() TLSState
}

// HandshakeSource says how many handshakes with other nodes were refused
// where gRPC does not do them - on the Raft port and on the connections Raft
// dials - since the node started, by reason: one of the Handshake constants.
// Any other reason is counted as HandshakeOther. The counts only grow.
type HandshakeSource interface {
	HandshakesRefused() map[string]uint64
}

// ReadinessSource says what /readyz would say.
type ReadinessSource interface {
	Ready(ctx context.Context) bool
}

// state is the collector of the gauges read at scrape time.
type state struct {
	raft        atomic.Pointer[RaftSource]
	drives      atomic.Pointer[DriveSource]
	certificate atomic.Pointer[CertificateSource]
	readiness   atomic.Pointer[ReadinessSource]
	// tls holds, by use, the source of a certificate's gauges; the uses are
	// those of tlsUses, and the map is not written after newState.
	tls     map[string]*tlsObserved
	timeout time.Duration

	// One probe a source.
	raftProbe        probe[raftAnswer]
	drivesProbe      probe[[]DriveState]
	certificateProbe probe[certificateAnswer]
	readinessProbe   probe[bool]

	mu sync.Mutex
	// known are the drives of the last answer of knownOf, the source that
	// gave it.
	known   []string
	knownOf *DriveSource

	isLeader, term, applied, commit, peers *prometheus.Desc
	schemaLevel, schemaSupported, schema   *prometheus.Desc
	driveOnline, driveFree, driveTotal     *prometheus.Desc
	certificateNotAfter, ready             *prometheus.Desc
	tlsNotAfter, tlsPending, tlsRefused    *prometheus.Desc
}

// tlsObserved is the source of one certificate's gauges, the probe it is
// asked through, and what it counted.
type tlsObserved struct {
	source atomic.Pointer[TLSSource]
	probe  probe[TLSState]
	// refused holds, by reason, what the sources of this use have counted,
	// and read is the last question whose answer was counted. Both are
	// guarded by the mutex of the state.
	refused map[string]*tally
	read    uint64
}

// tally makes one count that only grows of the readings of a count that is
// kept elsewhere, by a source that may be exchanged for another that starts
// at nothing again: a counter that is exposed never goes back.
type tally struct {
	// total is what is exposed; last is the last reading of the source of.
	total, last uint64
	of          any
}

// read takes a reading of a source and returns what is new in it.
func (c *tally) read(source any, reading uint64) uint64 {
	if c.of != source || reading < c.last {
		c.of, c.last = source, 0
	}
	grown := reading - c.last
	c.total, c.last = c.total+grown, reading
	return grown
}

func newState(timeout time.Duration) *state {
	if timeout <= 0 {
		timeout = stateTimeout
	}
	desc := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(name, help, labels, nil)
	}
	observed := map[string]*tlsObserved{}
	for _, use := range tlsUses() {
		observed[use] = &tlsObserved{refused: map[string]*tally{}}
		for _, reason := range TLSReloadReasons() {
			observed[use].refused[reason] = &tally{}
		}
	}
	return &state{
		timeout: timeout, tls: observed,
		isLeader: desc("cactusfs_raft_is_leader", "Whether this node leads the metadata group: 1 or 0."),
		term:     desc("cactusfs_raft_term", "The Raft term this node is in. It grows with every election."),
		applied:  desc("cactusfs_raft_applied_index", "Index of the last metadata entry this node's state reflects."),
		commit:   desc("cactusfs_raft_commit_index", "Index of the last metadata entry this node knows to be committed."),
		peers:    desc("cactusfs_raft_peers", "Voters of the metadata group, as this node knows it."),
		schemaLevel: desc("cactusfs_metadata_schema_level",
			"Newest metadata entry schema the cluster writes now: the oldest of what its members understand."),
		schemaSupported: desc("cactusfs_metadata_schema_supported", "Newest metadata entry schema this binary applies."),
		schema:          desc("cactusfs_metadata_state_schema", "Schema of this node's metadata state."),
		driveOnline:     desc("cactusfs_drive_online", "Whether a drive of this node can be reached: 1 or 0.", "drive"),
		driveFree:       desc("cactusfs_drive_free_bytes", "Bytes this node may still write to the file system of a drive.", "drive"),
		driveTotal:      desc("cactusfs_drive_total_bytes", "Size in bytes of the file system of a drive.", "drive"),
		certificateNotAfter: desc("cactusfs_node_certificate_not_after_seconds",
			"When the certificate this node shows the other nodes ends, in seconds since 1970."),
		ready: desc("cactusfs_ready", "Whether this node is ready to serve, as /readyz says: 1 or 0."),
		tlsNotAfter: desc("cactusfs_tls_certificate_not_after_seconds",
			"When a certificate this node shows ends, in seconds since 1970, by use: node (to the other nodes) or s3 (to clients of the S3 API, the management API and the console).",
			"use"),
		tlsPending: desc("cactusfs_tls_reload_pending",
			"Whether the certificate files on disk hold something other than the certificate in use, because it was refused: 1 or 0, by use.",
			"use"),
		tlsRefused: desc("cactusfs_tls_reload_refused_total",
			"Contents of the certificate files that were refused as a replacement, by use and reason; one that stays on disk is counted once.",
			"use", "reason"),
	}
}

// Describe implements prometheus.Collector.
func (s *state) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		s.isLeader, s.term, s.applied, s.commit, s.peers, s.schemaLevel, s.schemaSupported, s.schema,
		s.driveOnline, s.driveFree, s.driveTotal, s.certificateNotAfter, s.ready,
		s.tlsNotAfter, s.tlsPending, s.tlsRefused,
	} {
		ch <- d
	}
}

func flag(on bool) float64 {
	if on {
		return 1
	}
	return 0
}

// raftAnswer and certificateAnswer are what a source said, and whether it
// could say.
type raftAnswer struct {
	state RaftState
	known bool
}

type certificateAnswer struct {
	notAfter time.Time
	present  bool
}

// Collect implements prometheus.Collector. Every source that is attached is
// asked first, and then they are waited for together: the scrape is held for
// s.timeout at most, however many of them hang.
func (s *state) Collect(ch chan<- prometheus.Metric) {
	gauge := func(d *prometheus.Desc, value float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, value, labels...)
	}
	raft, drives, certificate, readiness := s.raft.Load(), s.drives.Load(), s.certificate.Load(), s.readiness.Load()
	var (
		raftAsked        *question[raftAnswer]
		drivesAsked      *question[[]DriveState]
		certificateAsked *question[certificateAnswer]
		readinessAsked   *question[bool]
	)
	if raft != nil {
		raftAsked = s.raftProbe.ask(raft, func() raftAnswer {
			r, known := (*raft).RaftState()
			return raftAnswer{state: r, known: known}
		})
	}
	if drives != nil {
		drivesAsked = s.drivesProbe.ask(drives, (*drives).DriveStates)
	}
	if certificate != nil {
		certificateAsked = s.certificateProbe.ask(certificate, func() certificateAnswer {
			notAfter, present := (*certificate).NodeCertificateNotAfter()
			return certificateAnswer{notAfter: notAfter, present: present}
		})
	}
	if readiness != nil {
		readinessAsked = s.readinessProbe.ask(readiness, func() bool {
			// A scrape has no context of its own to pass on. The source is
			// given as long as the scrape waits for it.
			ctx, done := context.WithTimeout(context.Background(), s.timeout)
			defer done()
			return (*readiness).Ready(ctx)
		})
	}
	tlsSources, tlsAsked := map[string]*TLSSource{}, map[string]*question[TLSState]{}
	for _, use := range tlsUses() {
		if source := s.tls[use].source.Load(); source != nil {
			tlsSources[use], tlsAsked[use] = source, s.tls[use].probe.ask(source, (*source).TLSState)
		}
	}
	// One deadline for all of them.
	expired, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	if answer, ok := raftAsked.wait(expired, raft); ok && answer.known {
		r := answer.state
		gauge(s.isLeader, flag(r.Leader))
		gauge(s.term, float64(r.Term))
		gauge(s.applied, float64(r.AppliedIndex))
		gauge(s.commit, float64(r.CommitIndex))
		gauge(s.peers, float64(r.Voters))
		for d, level := range map[*prometheus.Desc]float64{
			s.schemaLevel: float64(r.SchemaLevel), s.schemaSupported: float64(r.SchemaSupported), s.schema: float64(r.StateSchema),
		} {
			if level > 0 {
				gauge(d, level)
			}
		}
	}
	if drives != nil {
		answer, ok := drivesAsked.wait(expired, drives)
		for _, drive := range s.driveStates(drives, answer, ok) {
			gauge(s.driveOnline, flag(drive.Online), drive.ID)
			if drive.Online {
				gauge(s.driveFree, drive.FreeBytes, drive.ID)
				gauge(s.driveTotal, drive.TotalBytes, drive.ID)
			}
		}
	}
	if answer, ok := certificateAsked.wait(expired, certificate); ok && answer.present {
		gauge(s.certificateNotAfter, float64(answer.notAfter.Unix()))
	}
	if ready, ok := readinessAsked.wait(expired, readiness); ok {
		gauge(s.ready, flag(ready))
	}
	for _, use := range tlsUses() {
		source := tlsSources[use]
		answer, ok := tlsAsked[use].wait(expired, source)
		if !ok {
			continue
		}
		gauge(s.tlsNotAfter, float64(answer.NotAfter.Unix()), use)
		gauge(s.tlsPending, flag(answer.Pending), use)
		// Every reason is a series from the first answer on, so that the
		// first refusal is an increase from nothing.
		for reason, total := range s.tlsRefusals(use, source, tlsAsked[use].seq, answer.Refused) {
			ch <- prometheus.MustNewConstMetric(s.tlsRefused, prometheus.CounterValue, float64(total), use, reason)
		}
	}
}

// tlsRefusals takes the reading of a source of the given use, which is the
// answer to the question asked, and returns what to expose: by reason,
// everything the sources of that use have counted.
//
// An answer is counted once, and never after a later one: two scrapes at the
// same moment share an answer, or hold two and come here in either order,
// and a reading that is older than the last would look like a source that
// began again at nothing - and be counted a second time.
func (s *state) tlsRefusals(use string, source *TLSSource, asked uint64, reading map[string]uint64) map[string]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	observed := s.tls[use]
	fresh := asked > observed.read
	if fresh {
		observed.read = asked
	}
	totals := map[string]uint64{}
	for reason, counted := range observed.refused {
		if fresh {
			counted.read(source, reading[reason])
		}
		totals[reason] = counted.total
	}
	return totals
}

// driveStates returns the drives to expose: those of the answer, or - when
// the source did not answer in time - every drive its last answer named, as
// offline and with nothing said of its room. Before a source has answered
// once nothing is known of its drives, and nothing is exposed.
func (s *state) driveStates(source *DriveSource, answer []DriveState, answered bool) []DriveState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if answered {
		s.known, s.knownOf = make([]string, len(answer)), source
		for i, drive := range answer {
			s.known[i] = drive.ID
		}
		return answer
	}
	if s.knownOf != source {
		return nil
	}
	offline := make([]DriveState, len(s.known))
	for i, id := range s.known {
		offline[i] = DriveState{ID: id}
	}
	return offline
}

// probe asks one source how it is without letting a source that hangs hold
// up a scrape, or cost more than one goroutine: one question is out at a
// time, every scrape that comes while it is out waits for that one, and none
// waits longer than it may.
type probe[T any] struct {
	mu sync.Mutex
	// out is the question that is out; nil when none is.
	out *question[T]
	// asked counts the questions put.
	asked uint64
}

// question is one asking of a source.
type question[T any] struct {
	// of is the source that was asked, as it was attached.
	of any
	// seq numbers the questions of a probe from 1, in the order they were
	// put: an answer with a higher number is the later one.
	seq uint64
	// answered is closed when the source has answered; answer is written
	// before that and never after.
	answered chan struct{}
	answer   T
}

// ask puts fn to the source unless a question is out already, and returns
// the question to wait for. It does not wait.
func (p *probe[T]) ask(source any, fn func() T) *question[T] {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.out != nil {
		return p.out
	}
	p.asked++
	q := &question[T]{of: source, seq: p.asked, answered: make(chan struct{})}
	p.out = q
	go func() {
		q.answer = fn()
		p.mu.Lock()
		p.out = nil
		p.mu.Unlock()
		close(q.answered)
	}()
	return q
}

// wait returns the answer to q, and false when there is none for this
// scrape: nothing was asked, the time was up first, or the question that was
// out was put to a source that has been detached since.
func (q *question[T]) wait(expired context.Context, source any) (T, bool) {
	var none T
	if q == nil {
		return none, false
	}
	select {
	case <-q.answered:
	case <-expired.Done():
		// An answer that came at the same moment counts.
		select {
		case <-q.answered:
		default:
			return none, false
		}
	}
	if q.of != source {
		return none, false
	}
	return q.answer, true
}

// ObserveRaft attaches the source of the Raft gauges; nil detaches it.
func (t *Telemetry) ObserveRaft(source RaftSource) {
	if source == nil {
		t.state.raft.Store(nil)
		return
	}
	t.state.raft.Store(&source)
}

// ObserveDrives attaches the source of the drive gauges; nil detaches it.
func (t *Telemetry) ObserveDrives(source DriveSource) {
	if source == nil {
		t.state.drives.Store(nil)
		return
	}
	t.state.drives.Store(&source)
}

// ObserveNodeCertificate attaches the source of the gauge of the node
// certificate's end; nil detaches it.
func (t *Telemetry) ObserveNodeCertificate(source CertificateSource) {
	if source == nil {
		t.state.certificate.Store(nil)
		return
	}
	t.state.certificate.Store(&source)
}

// ObserveReadiness attaches the source of the readiness gauge; nil detaches
// it.
func (t *Telemetry) ObserveReadiness(source ReadinessSource) {
	if source == nil {
		t.state.readiness.Store(nil)
		return
	}
	t.state.readiness.Store(&source)
}

// ObserveTLS attaches the source of the gauges of one of the node's
// certificates; nil detaches it. use is TLSUseNode or TLSUseS3, and anything
// else attaches nothing: the label takes no other value.
func (t *Telemetry) ObserveTLS(use string, source TLSSource) {
	observed, ok := t.state.tls[use]
	if !ok {
		return
	}
	if source == nil {
		observed.source.Store(nil)
		return
	}
	observed.source.Store(&source)
}

// ObserveHandshakes attaches a source of refused handshakes that are counted
// where they happen and read here; nil detaches it. What it has counted is
// added to cactusfs_node_handshakes_refused_total when the node is scraped,
// beside what ObserveHandshakeRefused is told; a handshake must reach the
// metric by one of the two ways only.
func (t *Telemetry) ObserveHandshakes(source HandshakeSource) {
	if source == nil {
		t.handshakes.source.Store(nil)
		return
	}
	t.handshakes.source.Store(&source)
}

// handshakes is cactusfs_node_handshakes_refused_total as it is scraped:
// the counter, to which what the attached source has counted since the last
// scrape is added first. The source is asked through a probe, as the sources
// of the gauges are: a scrape waits for it no longer than timeout, and what
// it has counted is then added by a later scrape.
type handshakes struct {
	counter *prometheus.CounterVec
	source  atomic.Pointer[HandshakeSource]
	probe   probe[struct{}]
	timeout time.Duration

	mu sync.Mutex
	// read holds, by reason of the source, what was read of it.
	read map[string]*tally
}

func newHandshakes(counter *prometheus.CounterVec, timeout time.Duration) *handshakes {
	if timeout <= 0 {
		timeout = stateTimeout
	}
	return &handshakes{counter: counter, timeout: timeout, read: map[string]*tally{}}
}

// Describe implements prometheus.Collector.
func (h *handshakes) Describe(ch chan<- *prometheus.Desc) {
	h.counter.Describe(ch)
}

// Collect implements prometheus.Collector.
func (h *handshakes) Collect(ch chan<- prometheus.Metric) {
	if source := h.source.Load(); source != nil {
		asked := h.probe.ask(source, func() struct{} {
			h.add(source, (*source).HandshakesRefused())
			return struct{}{}
		})
		expired, cancel := context.WithTimeout(context.Background(), h.timeout)
		asked.wait(expired, source)
		cancel()
	}
	h.counter.Collect(ch)
}

// add adds what is new in a reading of a source to the counter.
func (h *handshakes) add(source *HandshakeSource, reading map[string]uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for reason, counted := range reading {
		if h.read[reason] == nil {
			h.read[reason] = &tally{}
		}
		if grown := h.read[reason].read(source, counted); grown > 0 {
			h.counter.WithLabelValues(handshakeReason(reason)).Add(float64(grown))
		}
	}
}

// handshakeReason returns reason when it is one of the Handshake constants,
// and HandshakeOther for anything else.
func handshakeReason(reason string) string {
	switch reason {
	case HandshakeCertificate, HandshakeNotAdmitted, HandshakeTimeout:
		return reason
	default:
		return HandshakeOther
	}
}

// ObserveHandshakeRefused counts a handshake of another node that this node
// refused. reason is one of the Handshake constants; anything else is
// counted as HandshakeOther.
func (t *Telemetry) ObserveHandshakeRefused(reason string) {
	t.handshakesRefused.WithLabelValues(handshakeReason(reason)).Inc()
}
