package telemetry_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// fixedReplication says how replication stands, or that it cannot say.
type fixedReplication struct {
	state telemetry.ReplicationState
	known bool
}

func (r fixedReplication) ReplicationState() (telemetry.ReplicationState, bool) {
	return r.state, r.known
}

// The gauges of replication, by name.
var replicationGauges = []string{"cactusfs_replication_lag_entries", "cactusfs_replication_lag_seconds", "cactusfs_replication_resyncs"}

// TestReplicationGauges: on the node that leads the lag of replication and
// every state of the resyncs are series, 0 where nothing is counted, and a
// state this release does not know is not shown; on every other node, and
// with no source, the series are absent.
func TestReplicationGauges(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		source telemetry.ReplicationSource
		want   map[string]float64
	}{
		"no source":                 {},
		"a node that does not lead": {source: fixedReplication{known: false}},
		"a leader with nothing waiting": {
			source: fixedReplication{known: true},
			want: map[string]float64{
				"cactusfs_replication_lag_entries{}": 0, "cactusfs_replication_lag_seconds{}": 0,
				"cactusfs_replication_resyncs{state=walking}": 0, "cactusfs_replication_resyncs{state=held}": 0,
				"cactusfs_replication_resyncs{state=waiting}": 0,
			},
		},
		"a leader behind, with resyncs": {
			source: fixedReplication{known: true, state: telemetry.ReplicationState{
				Pending: 1234, OldestPending: 90 * time.Second,
				Resyncs: map[string]int{app.ResyncWalking: 2, app.ResyncHeld: 1, "dreaming": 5},
			}},
			want: map[string]float64{
				"cactusfs_replication_lag_entries{}": 1234, "cactusfs_replication_lag_seconds{}": 90,
				"cactusfs_replication_resyncs{state=walking}": 2, "cactusfs_replication_resyncs{state=held}": 1,
				"cactusfs_replication_resyncs{state=waiting}": 0,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tel := newTelemetry(t, nil)
			if tt.source != nil {
				tel.ObserveReplication(tt.source)
			}
			got := map[string]float64{}
			for series, value := range gauges(t, tel) {
				for _, gauge := range replicationGauges {
					if strings.HasPrefix(series, gauge+"{") {
						got[series] = value
					}
				}
			}
			if len(got) != len(tt.want) {
				t.Errorf("exposed %v, want %v", got, tt.want)
			}
			for series, want := range tt.want {
				if value, ok := got[series]; !ok || value != want {
					t.Errorf("%s = %v (exposed %v), want %v", series, value, ok, want)
				}
			}
			tel.ObserveReplication(nil)
			for series := range gauges(t, tel) {
				for _, gauge := range replicationGauges {
					if strings.HasPrefix(series, gauge+"{") {
						t.Errorf("once the source is detached %s is exposed", series)
					}
				}
			}
		})
	}
}

// TestReplicationIsCounted: the versions the replicator gave a status are
// counted by result, COMPLETED and FAILED and nothing else; the events of
// resyncs by event, and one this release does not know not at all. Every
// series is there from the start.
func TestReplicationIsCounted(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil)
	before := gauges(t, tel)
	for _, series := range []string{
		"cactusfs_replication_versions_total{result=completed}", "cactusfs_replication_versions_total{result=failed}",
		"cactusfs_replication_resync_events_total{event=held}", "cactusfs_replication_resync_events_total{event=preempted}",
		"cactusfs_replication_resync_events_total{event=restarted}", "cactusfs_replication_bytes_total{}",
	} {
		if value, ok := before[series]; !ok || value != 0 {
			t.Errorf("before anything happened %s = %v (exposed %v), want 0", series, value, ok)
		}
	}
	for _, status := range []object.ReplicationStatus{
		object.ReplicationCompleted, object.ReplicationCompleted, object.ReplicationFailed,
		object.ReplicationPending, object.ReplicationReplica, "",
	} {
		tel.ObserveReplicatedVersion(status)
	}
	for _, event := range []string{app.ResyncEventHeld, app.ResyncEventHeld, app.ResyncEventPreempted, app.ResyncEventRestarted, "napped"} {
		tel.ObserveResyncEvent(event)
	}
	got := gauges(t, tel)
	for series, want := range map[string]float64{
		"cactusfs_replication_versions_total{result=completed}":     2,
		"cactusfs_replication_versions_total{result=failed}":        1,
		"cactusfs_replication_resync_events_total{event=held}":      2,
		"cactusfs_replication_resync_events_total{event=preempted}": 1,
		"cactusfs_replication_resync_events_total{event=restarted}": 1,
	} {
		if got[series] != want {
			t.Errorf("%s = %v, want %v", series, got[series], want)
		}
	}
	for series := range got {
		if !strings.HasPrefix(series, "cactusfs_replication_") {
			continue
		}
		if strings.Contains(series, "other") || strings.Contains(series, "napped") || strings.Contains(series, "PENDING") {
			t.Errorf("%s is exposed", series)
		}
	}
}

// readingSite is another deployment that reads every body it is sent, as
// far as it is told to, and then answers what it is told to.
type readingSite struct {
	ports.SiteClient
	limit int64
	err   error
}

func (s readingSite) read(body io.Reader) error {
	if _, err := io.Copy(io.Discard, io.LimitReader(body, s.limit)); err != nil {
		return err
	}
	return s.err
}

func (s readingSite) PutReplica(_ context.Context, _ object.Version, body io.Reader) error {
	return s.read(body)
}

func (s readingSite) PutPart(_ context.Context, _, _, _ string, _ int, _ int64, body io.Reader) (string, error) {
	return "etag", s.read(body)
}

// TestReplicationBytesAreCounted: the bytes of every version and part sent
// through the client of another deployment are counted as the client reads
// them - those of a request that then failed too, and only what was read.
func TestReplicationBytesAreCounted(t *testing.T) {
	t.Parallel()
	refused := errors.New("refused")
	for name, tt := range map[string]struct {
		limit int64
		err   error
		want  float64
	}{
		"every byte taken":         {limit: 1 << 20, want: 300 + 40},
		"a request that failed":    {limit: 1 << 20, err: refused, want: 300 + 40},
		"a client that read a bit": {limit: 10, err: refused, want: 10 + 10},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			tel := newTelemetry(t, nil)
			site := tel.InstrumentSiteClient(readingSite{limit: tt.limit, err: tt.err})
			if err := site.PutReplica(ctx, object.Version{}, strings.NewReader(strings.Repeat("v", 300))); !errors.Is(err, tt.err) {
				t.Errorf("PutReplica = %v, want %v", err, tt.err)
			}
			if _, err := site.PutPart(ctx, "tenant-secret-bucket", "tenant-secret-key", "upload", 1, 40, strings.NewReader(strings.Repeat("p", 40))); !errors.Is(err, tt.err) {
				t.Errorf("PutPart = %v, want %v", err, tt.err)
			}
			if got := gauges(t, tel)["cactusfs_replication_bytes_total{}"]; got != tt.want {
				t.Errorf("cactusfs_replication_bytes_total = %v, want %v", got, tt.want)
			}
			wantNothingOf(t, tel, nil, "tenant-secret-bucket", "tenant-secret-key")
		})
	}
}
