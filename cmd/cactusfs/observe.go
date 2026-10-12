package main

import (
	"context"
	"fmt"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/disk"
	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
)

// The sources of the gauges that say how the node is. Each stands between
// an adapter, which has an accessor, and the telemetry adapter, which has
// the interface it reads at a scrape; neither knows the other.

// raftGauges is the metadata store as the Raft gauges read it.
type raftGauges struct {
	store *raftmeta.Store
}

// RaftState implements telemetry.RaftSource.
func (g raftGauges) RaftState() (telemetry.RaftState, bool) {
	o, ok := g.store.Observe()
	if !ok {
		return telemetry.RaftState{}, false
	}
	return telemetry.RaftState{
		Leader: o.Leader, Term: o.Term, AppliedIndex: o.AppliedIndex, CommitIndex: o.CommitIndex, Voters: o.Voters,
		SchemaLevel: o.Level, SchemaSupported: o.Supported, StateSchema: o.StateSchema,
	}, true
}

// metadataLevel returns what tells the entry schema the deployment writes
// now, for the settings: the level the gauge cactusfs_metadata_schema_level
// shows, from the same reading of the store. It is 1 or more. The error
// wraps domain.ErrUnavailable when the store is closed or cannot tell, and
// the error of ctx when the reading - which waits for the lock of the state
// machine (raftmeta.Store.Observe) - took longer than the caller had.
func metadataLevel(store *raftmeta.Store) func(ctx context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		type reading struct {
			observation raftmeta.Observation
			ok          bool
		}
		// Room for the one reading, so that one nobody waits for any more
		// can still finish and be forgotten.
		done := make(chan reading, 1)
		go func() {
			var r reading
			r.observation, r.ok = store.Observe()
			done <- r
		}()
		select {
		case r := <-done:
			if !r.ok {
				return 0, fmt.Errorf("metadata level: %w: the metadata store is closed", domain.ErrUnavailable)
			}
			if r.observation.Level < 1 {
				return 0, fmt.Errorf("metadata level: %w: the metadata store cannot tell", domain.ErrUnavailable)
			}
			return r.observation.Level, nil
		case <-ctx.Done():
			return 0, fmt.Errorf("metadata level: %w", ctx.Err())
		}
	}
}

// driveGauges is this node's drives as the drive gauges read them: by their
// generated IDs, never by where they are.
type driveGauges []*disk.Drive

// DriveStates implements telemetry.DriveSource.
func (g driveGauges) DriveStates() []telemetry.DriveState {
	states := make([]telemetry.DriveState, len(g))
	for i, drive := range g {
		states[i] = telemetry.DriveState{ID: drive.DriveID()}
		if usage, err := drive.Usage(); err == nil {
			states[i].Online, states[i].FreeBytes, states[i].TotalBytes = true, usage.FreeBytes, usage.TotalBytes
		}
	}
	return states
}

// certificateGauge is what replaces the node's certificate as the gauge of
// that certificate's end reads it: the one in use at the scrape, which is
// another after a replacement (ADR-0020).
type certificateGauge struct {
	reloader *nodetls.Reloader
}

// NodeCertificateNotAfter implements telemetry.CertificateSource.
func (g certificateGauge) NodeCertificateNotAfter() (time.Time, bool) {
	return g.reloader.NotAfter(), true
}

// tlsGauges is what replaces one of the node's certificates - the one the
// other nodes are shown, or the one clients are - as the gauges of that
// certificate read it. Nothing it asks waits for a file.
type tlsGauges struct {
	reloader *nodetls.Reloader
}

// TLSState implements telemetry.TLSSource.
func (g tlsGauges) TLSState() telemetry.TLSState {
	state := telemetry.TLSState{
		NotAfter: g.reloader.NotAfter(), Pending: g.reloader.Pending() != "", Refused: map[string]uint64{},
	}
	for _, reason := range nodetls.ReloadRefusals() {
		state.Refused[string(reason)] = g.reloader.Refused(reason)
	}
	return state
}

// handshakeCounts is the node's identity as the count of refused handshakes
// reads it: what it refused on the Raft port and on the connections Raft
// dials. Node RPC's handshakes are gRPC's, and are not in it.
type handshakeCounts struct {
	identity *nodetls.Identity
}

// HandshakesRefused implements telemetry.HandshakeSource.
func (c handshakeCounts) HandshakesRefused() map[string]uint64 {
	counts := map[string]uint64{}
	for _, reason := range nodetls.PeerRefusals() {
		counts[handshakeReason(reason)] += c.identity.PeersRefused(reason)
	}
	return counts
}

// handshakeReason names a refusal of nodetls as the metric does, which has
// the reasons node RPC tells: whichever port a peer was refused on, and
// whoever did the handshake, the same thing has the same name.
func handshakeReason(reason nodetls.PeerRefusal) string {
	switch reason {
	case nodetls.PeerRefusedNoCertificate, nodetls.PeerRefusedUntrusted, nodetls.PeerRefusedIdentity:
		return telemetry.HandshakeCertificate
	case nodetls.PeerRefusedNotAdmitted:
		return telemetry.HandshakeNotAdmitted
	case nodetls.PeerRefusedTimeout:
		return telemetry.HandshakeTimeout
	default:
		return telemetry.HandshakeOther
	}
}

// readyGauge is the health service as the readiness gauge reads it.
type readyGauge struct {
	health *app.HealthService
}

// Ready implements telemetry.ReadinessSource.
func (g readyGauge) Ready(ctx context.Context) bool {
	return g.health.Readiness(ctx).Ready()
}
