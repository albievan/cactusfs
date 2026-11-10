package telemetry_test

import (
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// TestReleasesAreCountedAndTraced: the source of releases, the verifier and
// the upgrader keep their contracts through their decorators, every method
// that does I/O is counted under its port and is a span, and nothing of a
// release - its addresses, a version - is in a metric or a span.
func TestReleasesAreCountedAndTraced(t *testing.T) {
	t.Parallel()
	recorder := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, recorder)
	t.Run("contracts", func(t *testing.T) {
		porttest.ReleaseSourceContract(t, func(_ *testing.T, script porttest.ReleaseSourceScript) ports.ReleaseSource {
			return tel.InstrumentReleaseSource(porttest.ScriptedReleaseSource(script))
		})
		porttest.ReleaseVerifierContract(t, tel.InstrumentReleaseVerifier(&porttest.ReleaseVerifier{}),
			func(_ *testing.T, version release.Version, signed []byte) []byte {
				return porttest.FakeBundle(version, signed)
			})
		porttest.UpgraderContract(t, func(t *testing.T) (ports.Upgrader, release.Verified, func()) {
			u := porttest.NewUpgrader(porttest.UpgraderConfig{Mode: release.ModeBinary, Nodes: []string{"node-1"}})
			return tel.InstrumentUpgrader(u), porttest.FakeVerifiedBinary(t, "0.7.1"), u.Finish
		})
		t.Run("unavailable", func(t *testing.T) {
			t.Parallel()
			u := porttest.NewUpgrader(porttest.UpgraderConfig{Mode: release.ModeBinary, Unavailable: "a cluster of four"})
			porttest.UnavailableUpgraderContract(t, tel.InstrumentUpgrader(u), porttest.FakeVerifiedBinary(t, "0.7.1"))
		})
	})

	metrics := scrape(t, tel)
	wantEveryMethod(t, metrics, "release_source", methodsOf[ports.ReleaseSource]())
	wantEveryMethod(t, metrics, "release_verifier", methodsOf[ports.ReleaseVerifier]())
	// Mode tells what the upgrader is, from memory, and is no call.
	wantEveryMethod(t, metrics, "upgrader", []string{"Availability", "Start", "Progress"})
	for _, want := range []string{
		"cactusfs_port_calls_total method=Releases port=release_source result=error",
		"cactusfs_port_calls_total method=Verify port=release_verifier result=error",
		"cactusfs_port_calls_total method=Start port=upgrader result=error",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	for _, name := range []string{"release_source.Releases", "release_source.Fetch", "release_verifier.Verify", "upgrader.Start"} {
		if !contains(spanNames(recorder), name) {
			t.Errorf("no span named %s among %v", name, spanNames(recorder))
		}
	}
	wantNothingOf(t, tel, recorder, "https://", "0.7.1", "user/alice", "a cluster of four")
}

// TestUpgradeRunsAreCounted: a start the upgrader refuses is counted as
// refused, and a run that began once, by the state it ended in, however
// often it reports.
func TestUpgradeRunsAreCounted(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil)
	runs := func() map[string]float64 {
		t.Helper()
		return counts(t, tel, "cactusfs_upgrade_runs_total")
	}
	if got := runs(); len(got) != 4 || got["refused"] != 0 || got["done"] != 0 || got["failed"] != 0 || got["restart-required"] != 0 {
		t.Fatalf("before any run: %v", got)
	}

	refusing := tel.InstrumentUpgrader(porttest.NewUpgrader(porttest.UpgraderConfig{Mode: release.ModeOff}))
	if err := refusing.Start(t.Context(), porttest.FakeVerifiedBinary(t, "0.7.1"), "user/alice", nil); !errors.Is(err, domain.ErrUpgradeUnavailable) {
		t.Fatalf("Start = %v", err)
	}
	if got := runs(); got["refused"] != 1 {
		t.Errorf("after a refusal: %v", got)
	}

	for _, outcome := range []release.State{release.StateFailed, release.StateDone} {
		u := porttest.NewUpgrader(porttest.UpgraderConfig{Mode: release.ModeBinary, Outcome: outcome})
		ended := make(chan struct{})
		report := func(p release.Progress) {
			if p.State.Ended() {
				close(ended)
			}
		}
		if err := tel.InstrumentUpgrader(u).Start(t.Context(), porttest.FakeVerifiedBinary(t, "0.7.1"), "user/alice", report); err != nil {
			t.Fatal(err)
		}
		if got := runs(); got[string(outcome)] != 0 {
			t.Errorf("a run under way is counted: %v", got)
		}
		u.Finish()
		<-ended
		if got := runs(); got[string(outcome)] != 1 {
			t.Errorf("after a run that ended %s: %v", outcome, got)
		}
	}
}

// TestUpdateChecksAreCounted: every asking of the source is counted by its
// outcome - a class the contract does not know as unreachable - and the
// gauge says whether a release may be installed, by the last asking.
func TestUpdateChecksAreCounted(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil)
	if got := counts(t, tel, "cactusfs_update_checks_total"); len(got) != 6 {
		t.Fatalf("the outcomes from the start: %v", got)
	}
	for _, c := range []struct {
		class       string
		installable bool
	}{{"", true}, {"rate-limited", true}, {"something the source said", false}, {"unreachable", false}} {
		tel.ObserveUpdateCheck(c.class, c.installable)
		value := 0.0
		if c.installable {
			value = 1
		}
		if got := gaugeValue(t, tel, "cactusfs_update_available"); got != value {
			t.Errorf("after %q, %v: cactusfs_update_available = %v", c.class, c.installable, got)
		}
	}
	got := counts(t, tel, "cactusfs_update_checks_total")
	if got[telemetry.UpdateCheckOK] != 1 || got["rate-limited"] != 1 || got["unreachable"] != 2 || len(got) != 6 {
		t.Errorf("checks = %v", got)
	}
}

// counts returns the values of a counter by its one label.
func counts(t *testing.T, tel *telemetry.Telemetry, name string) map[string]float64 {
	t.Helper()
	families, err := tel.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			out[metric.GetLabel()[0].GetValue()] = metric.GetCounter().GetValue()
		}
	}
	return out
}

// gaugeValue returns the value of a gauge without labels.
func gaugeValue(t *testing.T, tel *telemetry.Telemetry, name string) float64 {
	t.Helper()
	families, err := tel.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name {
			return family.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("no %s", name)
	return 0
}
