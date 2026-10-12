package telemetry_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/telemetry"
)

// gauges returns every sample the registry has, as "name{label=value,...}"
// to its value.
func gauges(t *testing.T, tel *telemetry.Telemetry) map[string]float64 {
	t.Helper()
	samples, err := samplesOf(tel)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	return samples
}

// samplesOf is gauges for a goroutine that is not the test's own, which may
// fail a test and not stop it.
func samplesOf(tel *telemetry.Telemetry) (map[string]float64, error) {
	families, err := tel.Gatherer().Gather()
	if err != nil {
		return nil, err
	}
	samples := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			var labels []string
			for _, label := range metric.GetLabel() {
				labels = append(labels, label.GetName()+"="+label.GetValue())
			}
			name := family.GetName() + "{" + strings.Join(labels, ",") + "}"
			switch {
			case metric.GetGauge() != nil:
				samples[name] = metric.GetGauge().GetValue()
			case metric.GetCounter() != nil:
				samples[name] = metric.GetCounter().GetValue()
			}
		}
	}
	return samples, nil
}

type fixedRaft struct {
	state telemetry.RaftState
	known bool
}

func (r fixedRaft) RaftState() (telemetry.RaftState, bool) { return r.state, r.known }

type fixedDrives []telemetry.DriveState

func (d fixedDrives) DriveStates() []telemetry.DriveState { return d }

type fixedCertificate struct {
	notAfter time.Time
	present  bool
}

func (c fixedCertificate) NodeCertificateNotAfter() (time.Time, bool) { return c.notAfter, c.present }

// readyWhen is ready when it says so, and never when the scrape that asks
// has given up.
type readyWhen bool

func (r readyWhen) Ready(ctx context.Context) bool { return bool(r) && ctx.Err() == nil }

// The gauges that say how a node is, by name.
var stateGauges = []string{
	"cactusfs_raft_is_leader", "cactusfs_raft_term", "cactusfs_raft_applied_index", "cactusfs_raft_commit_index",
	"cactusfs_raft_peers", "cactusfs_metadata_schema_level", "cactusfs_metadata_schema_supported",
	"cactusfs_metadata_state_schema", "cactusfs_drive_online", "cactusfs_drive_free_bytes", "cactusfs_drive_total_bytes",
	"cactusfs_node_certificate_not_after_seconds", "cactusfs_ready",
	"cactusfs_tls_certificate_not_after_seconds", "cactusfs_tls_reload_pending", "cactusfs_tls_reload_refused_total",
}

func hasSeries(samples map[string]float64, name string) bool {
	for sample := range samples {
		if strings.HasPrefix(sample, name+"{") {
			return true
		}
	}
	return false
}

// TestStateGauges: what a node says of itself is read when it is scraped,
// from the sources that were attached; a gauge whose source is absent, or
// cannot say, is not exposed; a source that is detached is asked no more.
func TestStateGauges(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil)

	// A node that is starting has attached nothing.
	before := gauges(t, tel)
	for _, name := range stateGauges {
		if hasSeries(before, name) {
			t.Errorf("%s is exposed before anything is known of it", name)
		}
	}

	notAfter := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	tel.ObserveRaft(fixedRaft{known: true, state: telemetry.RaftState{
		Leader: true, Term: 7, AppliedIndex: 1234, CommitIndex: 1240, Voters: 3,
		SchemaLevel: 3, SchemaSupported: 4, StateSchema: 3,
	}})
	tel.ObserveDrives(fixedDrives{
		{ID: "drive-a", Online: true, FreeBytes: 250, TotalBytes: 1000},
		{ID: "drive-b"},
	})
	tel.ObserveNodeCertificate(fixedCertificate{notAfter: notAfter, present: true})
	tel.ObserveReadiness(readyWhen(true))

	samples := gauges(t, tel)
	for name, want := range map[string]float64{
		"cactusfs_raft_is_leader{}":                     1,
		"cactusfs_raft_term{}":                          7,
		"cactusfs_raft_applied_index{}":                 1234,
		"cactusfs_raft_commit_index{}":                  1240,
		"cactusfs_raft_peers{}":                         3,
		"cactusfs_metadata_schema_level{}":              3,
		"cactusfs_metadata_schema_supported{}":          4,
		"cactusfs_metadata_state_schema{}":              3,
		"cactusfs_drive_online{drive=drive-a}":          1,
		"cactusfs_drive_free_bytes{drive=drive-a}":      250,
		"cactusfs_drive_total_bytes{drive=drive-a}":     1000,
		"cactusfs_drive_online{drive=drive-b}":          0,
		"cactusfs_node_certificate_not_after_seconds{}": float64(notAfter.Unix()),
		"cactusfs_ready{}":                              1,
	} {
		if got, ok := samples[name]; !ok || got != want {
			t.Errorf("%s = %v (exposed: %v), want %v", name, got, ok, want)
		}
	}
	// Nothing is said of the room on a drive that cannot be reached.
	for _, name := range []string{"cactusfs_drive_free_bytes{drive=drive-b}", "cactusfs_drive_total_bytes{drive=drive-b}"} {
		if _, ok := samples[name]; ok {
			t.Errorf("%s is exposed for a drive that is offline", name)
		}
	}

	// A follower that cannot tell the level, a node without a certificate,
	// a node that is not ready.
	tel.ObserveRaft(fixedRaft{known: true, state: telemetry.RaftState{Term: 8, Voters: 3}})
	tel.ObserveNodeCertificate(fixedCertificate{})
	tel.ObserveReadiness(readyWhen(false))
	samples = gauges(t, tel)
	if got, ok := samples["cactusfs_raft_is_leader{}"]; !ok || got != 0 {
		t.Errorf("cactusfs_raft_is_leader = %v (exposed: %v), want 0", got, ok)
	}
	if got, ok := samples["cactusfs_ready{}"]; !ok || got != 0 {
		t.Errorf("cactusfs_ready = %v (exposed: %v), want 0", got, ok)
	}
	for _, name := range []string{
		"cactusfs_metadata_schema_level", "cactusfs_metadata_schema_supported", "cactusfs_metadata_state_schema",
		"cactusfs_node_certificate_not_after_seconds",
	} {
		if hasSeries(samples, name) {
			t.Errorf("%s is exposed though it is not known", name)
		}
	}

	// A store that is closing says nothing; detached sources are not asked.
	tel.ObserveRaft(fixedRaft{})
	if hasSeries(gauges(t, tel), "cactusfs_raft_term") {
		t.Error("the Raft gauges are exposed though the source could not say")
	}
	tel.ObserveRaft(nil)
	tel.ObserveDrives(nil)
	tel.ObserveNodeCertificate(nil)
	tel.ObserveReadiness(nil)
	after := gauges(t, tel)
	for _, name := range stateGauges {
		if hasSeries(after, name) {
			t.Errorf("%s is exposed after its source was detached", name)
		}
	}
}

// hangingDrives answers at once the first time and then not until released.
type hangingDrives struct {
	asked   chan struct{}
	release chan struct{}
}

func (d *hangingDrives) DriveStates() []telemetry.DriveState {
	select {
	case d.asked <- struct{}{}:
		// The first question.
	default:
		<-d.release
	}
	return []telemetry.DriveState{{ID: "drive-a", Online: true, FreeBytes: 1, TotalBytes: 2}}
}

// TestADriveThatHangsDoesNotHoldUpTheScrape: a scrape waits for the drives
// only so long; then every drive it knows is reported offline, and while the
// question is out no second one is asked. When the drives answer again they
// are online again.
func TestADriveThatHangsDoesNotHoldUpTheScrape(t *testing.T) {
	t.Parallel()
	tel, err := telemetry.New(t.Context(), telemetry.Config{Version: "test", StateTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	drives := &hangingDrives{asked: make(chan struct{}, 1), release: make(chan struct{})}
	tel.ObserveDrives(drives)

	if got := gauges(t, tel)["cactusfs_drive_online{drive=drive-a}"]; got != 1 {
		t.Fatalf("cactusfs_drive_online = %v at the first scrape, want 1", got)
	}
	for range 2 {
		started := time.Now()
		samples := gauges(t, tel)
		if waited := time.Since(started); waited > 5*time.Second {
			t.Fatalf("the scrape took %s", waited)
		}
		if got, ok := samples["cactusfs_drive_online{drive=drive-a}"]; !ok || got != 0 {
			t.Errorf("cactusfs_drive_online = %v (exposed: %v) while the drive hangs, want 0", got, ok)
		}
		if _, ok := samples["cactusfs_drive_free_bytes{drive=drive-a}"]; ok {
			t.Error("the room on a drive that hangs is exposed")
		}
	}
	close(drives.release)
	deadline := time.Now().Add(10 * time.Second)
	for gauges(t, tel)["cactusfs_drive_online{drive=drive-a}"] != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the drive answers again and is still reported offline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// heldSource is a source of every kind that answers at once until it is
// held, and then not until it is let go. It counts how often it was asked,
// and - like the metadata store's readiness check - does not look at the
// context it is given.
type heldSource struct {
	asked atomic.Int32
	held  atomic.Pointer[chan struct{}]
	// answered is how often it has said how many handshakes were refused,
	// which is what it says: a count that grows.
	answered atomic.Uint64
}

func (h *heldSource) answer() {
	h.asked.Add(1)
	if held := h.held.Load(); held != nil {
		<-*held
	}
}

// RaftState implements telemetry.RaftSource.
func (h *heldSource) RaftState() (telemetry.RaftState, bool) {
	h.answer()
	return telemetry.RaftState{Leader: true, Term: 7, AppliedIndex: 10, CommitIndex: 10, Voters: 3, SchemaLevel: 4, SchemaSupported: 4, StateSchema: 4}, true
}

// DriveStates implements telemetry.DriveSource.
func (h *heldSource) DriveStates() []telemetry.DriveState {
	h.answer()
	return []telemetry.DriveState{{ID: "drive-a", Online: true, FreeBytes: 1, TotalBytes: 2}}
}

// NodeCertificateNotAfter implements telemetry.CertificateSource.
func (h *heldSource) NodeCertificateNotAfter() (time.Time, bool) {
	h.answer()
	return time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC), true
}

// Ready implements telemetry.ReadinessSource.
func (h *heldSource) Ready(context.Context) bool {
	h.answer()
	return true
}

// TLSState implements telemetry.TLSSource.
func (h *heldSource) TLSState() telemetry.TLSState {
	h.answer()
	return telemetry.TLSState{NotAfter: time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC), Refused: map[string]uint64{"pair": 1}}
}

// HandshakesRefused implements telemetry.HandshakeSource.
func (h *heldSource) HandshakesRefused() map[string]uint64 {
	h.answer()
	return map[string]uint64{telemetry.HandshakeTimeout: h.answered.Add(1)}
}

// The series of each kind of source, as they are while it answers.
var sourceSeries = map[string][]string{
	"raft": {
		"cactusfs_raft_is_leader{}", "cactusfs_raft_term{}", "cactusfs_raft_applied_index{}", "cactusfs_raft_commit_index{}",
		"cactusfs_raft_peers{}", "cactusfs_metadata_schema_level{}", "cactusfs_metadata_schema_supported{}",
		"cactusfs_metadata_state_schema{}",
	},
	"drives":      {"cactusfs_drive_online{drive=drive-a}", "cactusfs_drive_free_bytes{drive=drive-a}", "cactusfs_drive_total_bytes{drive=drive-a}"},
	"certificate": {"cactusfs_node_certificate_not_after_seconds{}"},
	"readiness":   {"cactusfs_ready{}"},
	"tls-node": {
		"cactusfs_tls_certificate_not_after_seconds{use=node}", "cactusfs_tls_reload_pending{use=node}",
		"cactusfs_tls_reload_refused_total{reason=pair,use=node}", "cactusfs_tls_reload_refused_total{reason=usage,use=node}",
	},
	"tls-s3": {
		"cactusfs_tls_certificate_not_after_seconds{use=s3}", "cactusfs_tls_reload_pending{use=s3}",
		"cactusfs_tls_reload_refused_total{reason=pair,use=s3}", "cactusfs_tls_reload_refused_total{reason=usage,use=s3}",
	},
}

// TestASourceThatHangsDoesNotHoldUpTheScrape: whichever source does not
// answer, the scrape returns when its time is up with the series of every
// other source; those of the one that hangs are not exposed - but for
// cactusfs_drive_online, which says 0 of a drive that was known; however
// often the node is scraped the source is asked once; and when it answers
// its series are back.
func TestASourceThatHangsDoesNotHoldUpTheScrape(t *testing.T) {
	t.Parallel()
	const timeout = 100 * time.Millisecond
	for kind := range sourceSeries {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			tel, err := telemetry.New(t.Context(), telemetry.Config{Version: "test", StateTimeout: timeout})
			if err != nil {
				t.Fatal(err)
			}
			sources := map[string]*heldSource{"raft": {}, "drives": {}, "certificate": {}, "readiness": {}, "tls-node": {}, "tls-s3": {}}
			tel.ObserveRaft(sources["raft"])
			tel.ObserveDrives(sources["drives"])
			tel.ObserveNodeCertificate(sources["certificate"])
			tel.ObserveReadiness(sources["readiness"])
			tel.ObserveTLS(telemetry.TLSUseNode, sources["tls-node"])
			tel.ObserveTLS(telemetry.TLSUseS3, sources["tls-s3"])

			samples := gauges(t, tel)
			for _, names := range sourceSeries {
				for _, name := range names {
					if _, ok := samples[name]; !ok {
						t.Fatalf("%s is not exposed while every source answers", name)
					}
				}
			}

			hung := sources[kind]
			release := make(chan struct{})
			hung.held.Store(&release)
			before := hung.asked.Load()
			for scrape := range 3 {
				started := time.Now()
				samples = gauges(t, tel)
				// The time a scrape may wait, and room for a slow machine;
				// far from the forever the source would take.
				if waited := time.Since(started); waited > timeout+5*time.Second {
					t.Fatalf("scrape %d took %s", scrape, waited)
				}
				for other, names := range sourceSeries {
					for _, name := range names {
						_, ok := samples[name]
						switch {
						case other != kind && !ok:
							t.Errorf("scrape %d: %s is not exposed though its source answers", scrape, name)
						case other == kind && name == "cactusfs_drive_online{drive=drive-a}":
							if !ok || samples[name] != 0 {
								t.Errorf("scrape %d: %s = %v (exposed: %v) while the drives hang, want 0", scrape, name, samples[name], ok)
							}
						case other == kind && ok:
							t.Errorf("scrape %d: %s = %v is exposed though its source has not answered", scrape, name, samples[name])
						}
					}
				}
			}
			// Three scrapes, one question.
			if asked := hung.asked.Load() - before; asked != 1 {
				t.Errorf("the source that hangs was asked %d times by three scrapes, want once", asked)
			}
			for other, source := range sources {
				if other != kind && source.asked.Load() != 4 {
					t.Errorf("the %s source was asked %d times by four scrapes", other, source.asked.Load())
				}
			}

			hung.held.Store(nil)
			close(release)
			deadline := time.Now().Add(10 * time.Second)
			for {
				samples = gauges(t, tel)
				missing := ""
				for _, name := range sourceSeries[kind] {
					if _, ok := samples[name]; !ok || name == "cactusfs_drive_online{drive=drive-a}" && samples[name] != 1 {
						missing = name
					}
				}
				if missing == "" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the source answers again and %s is still not exposed as it was", missing)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// TestDrivesThatNeverAnsweredAreNotExposed: of a source that hangs the first
// time it is asked no drive is known, and none is called offline.
func TestDrivesThatNeverAnsweredAreNotExposed(t *testing.T) {
	t.Parallel()
	tel, err := telemetry.New(t.Context(), telemetry.Config{Version: "test", StateTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	drives, release := &heldSource{}, make(chan struct{})
	drives.held.Store(&release)
	defer close(release)
	tel.ObserveDrives(drives)
	for name := range gauges(t, tel) {
		if strings.HasPrefix(name, "cactusfs_drive_") {
			t.Errorf("%s is exposed though the drives have never answered", name)
		}
	}
}

// TestRefusedHandshakesAreCountedByAFixedReason: every reason is a series
// from the start, and a reason nobody defined is "other".
func TestRefusedHandshakesAreCountedByAFixedReason(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil)
	reasons := []string{
		telemetry.HandshakeCertificate, telemetry.HandshakeNotAdmitted, telemetry.HandshakeTimeout, telemetry.HandshakeOther,
	}
	samples := gauges(t, tel)
	for _, reason := range reasons {
		if got, ok := samples["cactusfs_node_handshakes_refused_total{reason="+reason+"}"]; !ok || got != 0 {
			t.Errorf("reason %s: %v (exposed: %v) before any refusal, want 0", reason, got, ok)
		}
	}
	for i, reason := range reasons {
		for range i + 1 {
			tel.ObserveHandshakeRefused(reason)
		}
	}
	tel.ObserveHandshakeRefused("node gateway-sentinel")
	tel.ObserveHandshakeRefused("")
	samples = gauges(t, tel)
	for i, reason := range reasons {
		want := float64(i + 1)
		if reason == telemetry.HandshakeOther {
			want += 2
		}
		if got := samples["cactusfs_node_handshakes_refused_total{reason="+reason+"}"]; got != want {
			t.Errorf("reason %s: %v, want %v", reason, got, want)
		}
	}
	count := 0
	for name := range samples {
		if strings.HasPrefix(name, "cactusfs_node_handshakes_refused_total{") {
			count++
		}
	}
	if count != len(reasons) {
		t.Errorf("%d series of refused handshakes, want %d", count, len(reasons))
	}
}

// fixedTLS is one of a node's certificates, as it is.
type fixedTLS telemetry.TLSState

func (c fixedTLS) TLSState() telemetry.TLSState { return telemetry.TLSState(c) }

// changingTLS is one source whose certificate changes between two scrapes.
type changingTLS struct {
	now atomic.Pointer[telemetry.TLSState]
}

func (c *changingTLS) TLSState() telemetry.TLSState { return *c.now.Load() }

// TestTLSGauges: each of the node's certificates says when it ends, whether
// what is on disk was refused, and how often for which reason. Every reason
// is a series, at 0, from the first scrape of a certificate; a reason nobody
// defined is no series; a use nobody defined attaches nothing; and a count
// never goes back, whatever source is attached after another.
func TestTLSGauges(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil)
	tlsSeries := func(samples map[string]float64) int {
		count := 0
		for name := range samples {
			if strings.HasPrefix(name, "cactusfs_tls_") {
				count++
			}
		}
		return count
	}
	tel.ObserveTLS("admin", fixedTLS{})
	if count := tlsSeries(gauges(t, tel)); count != 0 {
		t.Fatalf("%d series of certificates before one was attached", count)
	}

	nodeEnd, s3End := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	tel.ObserveTLS(telemetry.TLSUseNode, fixedTLS{NotAfter: nodeEnd})
	samples := gauges(t, tel)
	// The end, pending, and one series a reason; nothing of the other use.
	if count, want := tlsSeries(samples), 2+len(telemetry.TLSReloadReasons()); count != want {
		t.Errorf("%d series of the node's certificate, want %d", count, want)
	}
	for _, reason := range telemetry.TLSReloadReasons() {
		if got, ok := samples["cactusfs_tls_reload_refused_total{reason="+reason+",use=node}"]; !ok || got != 0 {
			t.Errorf("reason %s: %v (exposed: %v) before any refusal, want 0", reason, got, ok)
		}
	}

	served := &changingTLS{}
	served.now.Store(&telemetry.TLSState{NotAfter: s3End, Pending: true, Refused: map[string]uint64{
		"expired": 2, "usage": 1, "reason-sentinel": 9,
	}})
	tel.ObserveTLS(telemetry.TLSUseS3, served)
	want := map[string]float64{
		"cactusfs_tls_certificate_not_after_seconds{use=node}":       float64(nodeEnd.Unix()),
		"cactusfs_tls_certificate_not_after_seconds{use=s3}":         float64(s3End.Unix()),
		"cactusfs_tls_reload_pending{use=node}":                      0,
		"cactusfs_tls_reload_pending{use=s3}":                        1,
		"cactusfs_tls_reload_refused_total{reason=expired,use=s3}":   2,
		"cactusfs_tls_reload_refused_total{reason=usage,use=s3}":     1,
		"cactusfs_tls_reload_refused_total{reason=pair,use=s3}":      0,
		"cactusfs_tls_reload_refused_total{reason=expired,use=node}": 0,
	}
	check := func(when string) {
		t.Helper()
		samples := gauges(t, tel)
		for name, value := range want {
			if got, ok := samples[name]; !ok || got != value {
				t.Errorf("%s: %s = %v (exposed: %v), want %v", when, name, got, ok, value)
			}
		}
		if count, all := tlsSeries(samples), 2*(2+len(telemetry.TLSReloadReasons())); count != all {
			t.Errorf("%s: %d series of certificates, want %d", when, count, all)
		}
		for name := range samples {
			if strings.Contains(name, "sentinel") {
				t.Errorf("%s: %s has a label value nobody defined", when, name)
			}
		}
	}
	check("two certificates")

	// The certificate is replaced: its end is the new one's, nothing is
	// pending, and what was refused before stays counted.
	renewed := s3End.AddDate(1, 0, 0)
	served.now.Store(&telemetry.TLSState{NotAfter: renewed, Refused: map[string]uint64{"expired": 2, "usage": 1}})
	want["cactusfs_tls_certificate_not_after_seconds{use=s3}"], want["cactusfs_tls_reload_pending{use=s3}"] = float64(renewed.Unix()), 0
	check("after the replacement")

	// Another source, which has counted less: a count goes on from where
	// it was. 2 expired before, and 1 since; 1 usage before, none since.
	tel.ObserveTLS(telemetry.TLSUseS3, fixedTLS{NotAfter: renewed, Refused: map[string]uint64{"expired": 1}})
	want["cactusfs_tls_reload_refused_total{reason=expired,use=s3}"] = 3
	check("after another source was attached")
	check("scraped again")

	tel.ObserveTLS(telemetry.TLSUseS3, nil)
	tel.ObserveTLS(telemetry.TLSUseNode, nil)
	if count := tlsSeries(gauges(t, tel)); count != 0 {
		t.Errorf("%d series of certificates after their sources were detached", count)
	}
}

// countedHandshakes is a count of refused handshakes kept elsewhere.
type countedHandshakes map[string]uint64

func (c countedHandshakes) HandshakesRefused() map[string]uint64 { return c }

// TestHandshakesCountedElsewhereAreAddedOnce: what a source has counted is
// added to the counter when the node is scraped - what is new since the last
// scrape, once, beside what the node was told one by one; under the same
// fixed reasons; and the counter never goes back, when the source is detached
// and when another that starts at nothing takes its place.
func TestHandshakesCountedElsewhereAreAddedOnce(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil)
	refused := func() map[string]float64 {
		t.Helper()
		byReason := map[string]float64{}
		for name, value := range gauges(t, tel) {
			if reason, ok := strings.CutPrefix(name, "cactusfs_node_handshakes_refused_total{reason="); ok {
				byReason[strings.TrimSuffix(reason, "}")] = value
			}
		}
		return byReason
	}
	steps := []struct {
		name string
		do   func()
		// certificate, not_admitted, timeout, other.
		want [4]float64
	}{
		{name: "nothing yet", do: func() {}},
		{name: "told of one", do: func() { tel.ObserveHandshakeRefused(telemetry.HandshakeCertificate) }, want: [4]float64{1, 0, 0, 0}},
		{
			name: "a source that has counted",
			do: func() {
				tel.ObserveHandshakes(countedHandshakes{telemetry.HandshakeCertificate: 2, telemetry.HandshakeNotAdmitted: 3, "reason-sentinel": 4})
			},
			want: [4]float64{3, 3, 0, 4},
		},
		{name: "scraped again, nothing new", do: func() {}, want: [4]float64{3, 3, 0, 4}},
		{
			name: "the source has counted more",
			do: func() {
				tel.ObserveHandshakes(countedHandshakes{telemetry.HandshakeCertificate: 2, telemetry.HandshakeTimeout: 1})
			},
			// Another source: it starts at nothing.
			want: [4]float64{5, 3, 1, 4},
		},
		{name: "detached", do: func() { tel.ObserveHandshakes(nil) }, want: [4]float64{5, 3, 1, 4}},
		{
			name: "told of one more, and a source that starts again",
			do: func() {
				tel.ObserveHandshakeRefused(telemetry.HandshakeTimeout)
				tel.ObserveHandshakes(countedHandshakes{telemetry.HandshakeOther: 1})
			},
			want: [4]float64{5, 3, 2, 5},
		},
	}
	reasons := []string{telemetry.HandshakeCertificate, telemetry.HandshakeNotAdmitted, telemetry.HandshakeTimeout, telemetry.HandshakeOther}
	for _, step := range steps {
		step.do()
		got := refused()
		if len(got) != len(reasons) {
			t.Errorf("%s: %d series of refused handshakes, want %d: %v", step.name, len(got), len(reasons), got)
		}
		for i, reason := range reasons {
			if got[reason] != step.want[i] {
				t.Errorf("%s: reason %s = %v, want %v", step.name, reason, got[reason], step.want[i])
			}
		}
	}
}

// growing is one source whose count grows between two scrapes.
type growing struct{ counted atomic.Uint64 }

func (g *growing) HandshakesRefused() map[string]uint64 {
	return map[string]uint64{telemetry.HandshakeNotAdmitted: g.counted.Load()}
}

// TestHandshakesOfOneSourceAreDeltas: of one source that stays, each scrape
// adds what is new.
func TestHandshakesOfOneSourceAreDeltas(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil)
	source := &growing{}
	tel.ObserveHandshakes(source)
	const name = "cactusfs_node_handshakes_refused_total{reason=not_admitted}"
	for _, counted := range []uint64{0, 1, 1, 5, 6} {
		source.counted.Store(counted)
		if got := gauges(t, tel)[name]; got != float64(counted) {
			t.Errorf("the source has counted %d, the counter says %v", counted, got)
		}
	}
}

// TestHandshakeSourceThatHangsDoesNotHoldUpTheScrape: the counter is exposed
// with what it had, the scrape returns when its time is up, the source is
// asked once, and what it had counted is added when it has answered.
func TestHandshakeSourceThatHangsDoesNotHoldUpTheScrape(t *testing.T) {
	t.Parallel()
	const timeout = 100 * time.Millisecond
	tel, err := telemetry.New(t.Context(), telemetry.Config{Version: "test", StateTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	const name = "cactusfs_node_handshakes_refused_total{reason=timeout}"
	source := &heldSource{}
	tel.ObserveHandshakes(source)
	// It says how often it has answered: once.
	if got := gauges(t, tel)[name]; got != 1 {
		t.Fatalf("%s = %v after the first scrape, want 1", name, got)
	}
	release := make(chan struct{})
	source.held.Store(&release)
	for scrape := range 3 {
		started := time.Now()
		got, ok := gauges(t, tel)[name]
		if waited := time.Since(started); waited > timeout+5*time.Second {
			t.Fatalf("scrape %d took %s", scrape, waited)
		}
		if !ok || got != 1 {
			t.Errorf("scrape %d: %s = %v (exposed: %v) while the source hangs, want 1", scrape, name, got, ok)
		}
	}
	if asked := source.asked.Load(); asked != 2 {
		t.Errorf("the source was asked %d times, want twice: once before it hung and once since", asked)
	}
	source.held.Store(nil)
	close(release)
	// The answer that was held said 2; whatever is asked after says more.
	for deadline := time.Now().Add(10 * time.Second); gauges(t, tel)[name] < 2; {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not grow after the source answered", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// slowRaft answers after a while, as a store under load does.
type slowRaft time.Duration

func (r slowRaft) RaftState() (telemetry.RaftState, bool) {
	time.Sleep(time.Duration(r))
	return telemetry.RaftState{Leader: true, Term: 3, Voters: 3}, true
}

// countingTLS has refused one more content of its files each time it is
// asked.
type countingTLS struct{ refused atomic.Uint64 }

func (c *countingTLS) TLSState() telemetry.TLSState {
	return telemetry.TLSState{NotAfter: time.Unix(1_900_000_000, 0), Refused: map[string]uint64{"expired": c.refused.Add(1)}}
}

// TestConcurrentScrapesSeeTheSameState: two scrapers at the same moment - two
// Prometheus servers, or one and somebody with curl - are told the same of
// sources that answer. A scrape that comes while a question is out waits for
// that answer: no drive is reported offline and no series is left out because
// somebody else asked first, and a count read by two scrapes is counted once.
func TestConcurrentScrapesSeeTheSameState(t *testing.T) {
	t.Parallel()
	tel, err := telemetry.New(t.Context(), telemetry.Config{Version: "test", StateTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	certificate := &countingTLS{}
	// The metadata store takes its time, so that every scrape finds
	// questions out that another put.
	tel.ObserveRaft(slowRaft(2 * time.Millisecond))
	tel.ObserveDrives(fixedDrives{{ID: "drive-a", Online: true, FreeBytes: 1, TotalBytes: 2}, {ID: "drive-b", Online: true, FreeBytes: 3, TotalBytes: 4}})
	tel.ObserveNodeCertificate(fixedCertificate{notAfter: time.Unix(1_900_000_000, 0), present: true})
	tel.ObserveReadiness(readyWhen(true))
	tel.ObserveTLS(telemetry.TLSUseNode, certificate)

	want := map[string]float64{
		"cactusfs_raft_is_leader{}": 1, "cactusfs_raft_peers{}": 3, "cactusfs_ready{}": 1,
		"cactusfs_drive_online{drive=drive-a}": 1, "cactusfs_drive_total_bytes{drive=drive-a}": 2,
		"cactusfs_drive_online{drive=drive-b}": 1, "cactusfs_drive_free_bytes{drive=drive-b}": 3,
		"cactusfs_node_certificate_not_after_seconds{}":        1_900_000_000,
		"cactusfs_tls_certificate_not_after_seconds{use=node}": 1_900_000_000,
	}
	const refused = "cactusfs_tls_reload_refused_total{reason=expired,use=node}"
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				samples, err := samplesOf(tel)
				if err != nil {
					t.Errorf("Gather: %v", err)
					return
				}
				for name, value := range want {
					if got, ok := samples[name]; !ok || got != value {
						t.Errorf("%s = %v (exposed: %t) in a scrape beside others, want %v", name, got, ok, value)
						return
					}
				}
				// What was counted is never more than the source has said.
				if got, said := samples[refused], float64(certificate.refused.Load()); got < 1 || got > said {
					t.Errorf("%s = %v in a scrape beside others; the source has counted %v", refused, got, said)
					return
				}
			}
		})
	}
	wg.Wait()
	// Alone again, a scrape reads the count as the source has it.
	if got, said := gauges(t, tel)[refused], float64(certificate.refused.Load()); got != said {
		t.Errorf("%s = %v after the scrapes, the source has counted %v", refused, got, said)
	}
}
