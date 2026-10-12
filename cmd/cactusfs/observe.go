package main

import (
	"context"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/disk"
	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/app"
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

// certificateGauge is the node's identity as the gauge of its certificate's
// end reads it.
//
// TODO(PLAN 7.5): nodetls has no accessor for the certificate it presents,
// so this reads it from the configuration the identity gives a listener.
// When certificates are replaced without a restart and nodetls can say
// which one it presents now, read it from there.
type certificateGauge struct {
	identity *nodetls.Identity
}

// NodeCertificateNotAfter implements telemetry.CertificateSource.
func (g certificateGauge) NodeCertificateNotAfter() (time.Time, bool) {
	certificates := g.identity.Server().Certificates
	if len(certificates) == 0 || certificates[0].Leaf == nil {
		return time.Time{}, false
	}
	return certificates[0].Leaf.NotAfter, true
}

// readyGauge is the health service as the readiness gauge reads it.
type readyGauge struct {
	health *app.HealthService
}

// Ready implements telemetry.ReadinessSource.
func (g readyGauge) Ready(ctx context.Context) bool {
	return g.health.Readiness(ctx).Ready()
}
