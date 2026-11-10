package telemetry

import (
	"context"
	"io"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
)

// The outcomes of a check for releases (cactusfs_update_checks_total): the
// source answered, or the class of why it did not, as the management API
// names it (app.CheckError*).
const (
	UpdateCheckOK            = "ok"
	updateCheckUnreachable   = "unreachable"
	updateCheckRateLimited   = "rate-limited"
	updateCheckRefused       = "refused"
	updateCheckInvalidAnswer = "invalid-answer"
	updateCheckTimeout       = "timeout"
)

// The outcomes of an upgrade (cactusfs_upgrade_runs_total): refused before
// anything was begun, or the state it ended in.
const (
	UpgradeRefused = "refused"
)

// updateMetrics are the metrics of checking for releases and installing
// one (ADR-0029).
type updateMetrics struct {
	checks    *prometheus.CounterVec
	available prometheus.Gauge
	runs      *prometheus.CounterVec
}

// newUpdateMetrics makes the metrics, every outcome a series from the
// start.
func newUpdateMetrics() *updateMetrics {
	m := &updateMetrics{
		checks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cactusfs_update_checks_total",
			Help: "Askings of the update source by this node, asked for or periodic, by outcome: ok, or the class of why it could not be read.",
		}, []string{"outcome"}),
		available: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cactusfs_update_available",
			Help: "1 when the releases the update source last listed hold one that may be installed from the running one, else 0.",
		}),
		runs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cactusfs_upgrade_runs_total",
			Help: "Upgrades started from this node, by outcome: refused by the upgrader, or the state the run ended in (done, failed, restart-required).",
		}, []string{"outcome"}),
	}
	for _, outcome := range []string{
		UpdateCheckOK, updateCheckUnreachable, updateCheckRateLimited, updateCheckRefused, updateCheckInvalidAnswer, updateCheckTimeout,
	} {
		m.checks.WithLabelValues(outcome)
	}
	for _, outcome := range []string{
		UpgradeRefused, string(release.StateDone), string(release.StateFailed), string(release.StateRestartRequired),
	} {
		m.runs.WithLabelValues(outcome)
	}
	return m
}

// collectors returns what is registered.
func (m *updateMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.checks, m.available, m.runs}
}

// ObserveUpdateCheck records one asking of the update source: the class of
// why it could not be read, "" when it answered, and whether a release may
// be installed now. It is app.UpdateServiceConfig.OnCheck. A class that is
// not one of the contract's is counted as unreachable, as the use case
// reads it.
func (t *Telemetry) ObserveUpdateCheck(class string, installable bool) {
	outcome := UpdateCheckOK
	switch class {
	case "":
	case updateCheckUnreachable, updateCheckRateLimited, updateCheckRefused, updateCheckInvalidAnswer, updateCheckTimeout:
		outcome = class
	default:
		outcome = updateCheckUnreachable
	}
	t.updates.checks.WithLabelValues(outcome).Inc()
	value := 0.0
	if installable {
		value = 1
	}
	t.updates.available.Set(value)
}

// InstrumentReleaseSource returns next wrapped so that every call is traced
// and counted. Nothing of the source - its address, an asset's name or
// address - is a label or an attribute: the address is the operator's, and
// may name a host they keep to themselves.
func (t *Telemetry) InstrumentReleaseSource(next ports.ReleaseSource) ports.ReleaseSource {
	return &releaseSource{t: t, next: next}
}

type releaseSource struct {
	t    *Telemetry
	next ports.ReleaseSource
}

const portReleaseSource = "release_source"

func (*releaseSource) instrumented() {}

// Releases implements ports.ReleaseSource.
func (s *releaseSource) Releases(ctx context.Context) ([]release.Release, error) {
	ctx, done := s.t.call(ctx, portReleaseSource, "Releases")
	releases, err := s.next.Releases(ctx)
	done(err)
	return releases, err
}

// Fetch implements ports.ReleaseSource.
func (s *releaseSource) Fetch(ctx context.Context, asset release.Asset, limit int64, w io.Writer) error {
	ctx, done := s.t.call(ctx, portReleaseSource, "Fetch")
	err := s.next.Fetch(ctx, asset, limit, w)
	done(err)
	return err
}

// InstrumentReleaseVerifier returns next wrapped so that every verification
// is traced and counted. The version is not a label: a label of every
// version ever offered grows without bound.
func (t *Telemetry) InstrumentReleaseVerifier(next ports.ReleaseVerifier) ports.ReleaseVerifier {
	return &releaseVerifier{t: t, next: next}
}

type releaseVerifier struct {
	t    *Telemetry
	next ports.ReleaseVerifier
}

const portReleaseVerifier = "release_verifier"

func (*releaseVerifier) instrumented() {}

// Verify implements ports.ReleaseVerifier.
func (v *releaseVerifier) Verify(ctx context.Context, version release.Version, signed, bundle []byte) error {
	ctx, done := v.t.call(ctx, portReleaseVerifier, "Verify")
	err := v.next.Verify(ctx, version, signed, bundle)
	done(err)
	return err
}

// InstrumentUpgrader returns next wrapped so that every call is traced and
// counted, and every upgrade it is asked to start is counted in
// cactusfs_upgrade_runs_total: as refused when Start fails, else once, by
// the state it ends in, when it reports that it has ended.
func (t *Telemetry) InstrumentUpgrader(next ports.Upgrader) ports.Upgrader {
	return &upgrader{t: t, next: next}
}

type upgrader struct {
	t    *Telemetry
	next ports.Upgrader
}

const portUpgrader = "upgrader"

func (*upgrader) instrumented() {}

// Mode implements ports.Upgrader.
func (u *upgrader) Mode() release.Mode { return u.next.Mode() }

// Availability implements ports.Upgrader.
func (u *upgrader) Availability(ctx context.Context) (ok bool, reason string, instructions []string) {
	ctx, done := u.t.call(ctx, portUpgrader, "Availability")
	ok, reason, instructions = u.next.Availability(ctx)
	done(nil)
	return ok, reason, instructions
}

// Start implements ports.Upgrader.
func (u *upgrader) Start(ctx context.Context, target release.Verified, startedBy string, report func(release.Progress)) error {
	ctx, done := u.t.call(ctx, portUpgrader, "Start")
	var once sync.Once
	counted := func(p release.Progress) {
		if p.State.Ended() {
			once.Do(func() { u.t.updates.runs.WithLabelValues(string(p.State)).Inc() })
		}
		if report != nil {
			report(p)
		}
	}
	err := u.next.Start(ctx, target, startedBy, counted)
	done(err)
	if err != nil {
		u.t.updates.runs.WithLabelValues(UpgradeRefused).Inc()
	}
	return err
}

// Progress implements ports.Upgrader.
func (u *upgrader) Progress(ctx context.Context) (release.Progress, error) {
	ctx, done := u.t.call(ctx, portUpgrader, "Progress")
	progress, err := u.next.Progress(ctx)
	done(err)
	return progress, err
}
