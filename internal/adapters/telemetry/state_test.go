package telemetry_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/telemetry"
)

// gauges returns every sample the registry has, as "name{label=value,...}"
// to its value.
func gauges(t *testing.T, tel *telemetry.Telemetry) map[string]float64 {
	t.Helper()
	families, err := tel.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
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
	return samples
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
			sources := map[string]*heldSource{"raft": {}, "drives": {}, "certificate": {}, "readiness": {}}
			tel.ObserveRaft(sources["raft"])
			tel.ObserveDrives(sources["drives"])
			tel.ObserveNodeCertificate(sources["certificate"])
			tel.ObserveReadiness(sources["readiness"])

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
