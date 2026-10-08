package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// DriveStatus is one drive of an erasure set, and whether it answers.
type DriveStatus struct {
	ID     string
	NodeID string
	Online bool
}

// ClusterStatus is the deployment as one node sees it.
type ClusterStatus struct {
	DeploymentID string
	// AnsweredBy is the node that was asked.
	AnsweredBy string
	// Leader is the node that leads the metadata group, or "".
	Leader string
	Nodes  []cluster.Node
	// Sets holds the drives of every erasure set, in order.
	Sets [][]DriveStatus
	// NodeFailuresTolerated is how many nodes can be lost without losing
	// access to data.
	NodeFailuresTolerated int
	DrivesOnline          int
	DrivesTotal           int
}

// MaintenanceStatus is what the background work of one node has done since
// the node started.
type MaintenanceStatus struct {
	NodeID    string
	StartedAt time.Time

	ScrubPasses int64
	LastScrubAt time.Time
	LastScrub   ScrubStats

	HealedObjects       int64
	HealedShards        int64
	UnrecoverableObject int64

	GCPasses int64
	LastGCAt time.Time
	LastGC   GCStats
}

// Maintenance remembers what scrubbing, healing and collection did. The
// background workers report to it; the status use case reads it.
type Maintenance struct {
	clock ports.Clock

	mu     sync.Mutex
	status MaintenanceStatus
}

// NewMaintenance returns a Maintenance for a node that starts now.
func NewMaintenance(nodeID string, clock ports.Clock) *Maintenance {
	return &Maintenance{clock: clock, status: MaintenanceStatus{NodeID: nodeID, StartedAt: clock.Now().UTC()}}
}

// ObserveScrub records a completed pass of the scrubber.
func (m *Maintenance) ObserveScrub(stats ScrubStats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.ScrubPasses++
	m.status.LastScrubAt, m.status.LastScrub = m.clock.Now().UTC(), stats
}

// ObserveHeal records the outcome of healing one object.
func (m *Maintenance) ObserveHeal(result HealResult) {
	if result.Damaged == 0 && !result.Unrecoverable {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.HealedObjects++
	m.status.HealedShards += int64(result.Rebuilt)
	if result.Unrecoverable {
		m.status.UnrecoverableObject++
	}
}

// ObserveGC records a completed pass of the collector.
func (m *Maintenance) ObserveGC(stats GCStats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.GCPasses++
	m.status.LastGCAt, m.status.LastGC = m.clock.Now().UTC(), stats
}

// Status returns what has been recorded.
func (m *Maintenance) Status() MaintenanceStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

// ClusterServiceConfig lists what a ClusterService depends on.
type ClusterServiceConfig struct {
	View  ports.ClusterView
	Authz ports.Authorizer
	// NodeID is the node the service runs on.
	NodeID string
	// Leader returns the node that leads the metadata group, or "".
	Leader func() string
	// Probe reports whether the drive in a slot answers.
	Probe func(ctx context.Context, slot cluster.Slot) bool
	// Maintenance is the record of this node's background work.
	Maintenance *Maintenance
}

// ClusterService answers what the deployment consists of and how it is.
type ClusterService struct {
	cfg ClusterServiceConfig
}

// NewClusterService returns a ClusterService.
func NewClusterService(cfg ClusterServiceConfig) *ClusterService {
	return &ClusterService{cfg: cfg}
}

// probeTimeout bounds how long one drive is waited for.
const probeTimeout = 3 * time.Second

func (s *ClusterService) allow(ctx context.Context, p iam.Principal) error {
	return s.cfg.Authz.Authorize(ctx, iam.Request{
		Principal: p, Action: iam.ActionGetClusterStatus, Resource: iam.Resource{Admin: "cluster"},
	})
}

// Status returns the nodes and drives of the deployment. Every drive is
// asked whether it answers, all at once.
func (s *ClusterService) Status(ctx context.Context, p iam.Principal) (ClusterStatus, error) {
	if err := s.allow(ctx, p); err != nil {
		return ClusterStatus{}, fmt.Errorf("cluster.Status: %w", err)
	}
	topology, err := s.cfg.View.Topology(ctx)
	if err != nil {
		return ClusterStatus{}, fmt.Errorf("cluster.Status: %w", err)
	}
	nodes, err := s.cfg.View.Nodes(ctx)
	if err != nil {
		return ClusterStatus{}, fmt.Errorf("cluster.Status: %w", err)
	}
	status := ClusterStatus{
		DeploymentID: topology.DeploymentID, AnsweredBy: s.cfg.NodeID, Leader: s.cfg.Leader(), Nodes: nodes,
		Sets: make([][]DriveStatus, len(topology.Sets)), NodeFailuresTolerated: topology.NodeFailuresTolerated(),
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for i, set := range topology.Sets {
		status.Sets[i] = make([]DriveStatus, len(set))
		for j, slot := range set {
			status.Sets[i][j] = DriveStatus{ID: slot.DriveID, NodeID: slot.NodeID}
			wg.Go(func() {
				status.Sets[i][j].Online = s.cfg.Probe(probeCtx, slot)
			})
		}
	}
	wg.Wait()
	for _, set := range status.Sets {
		for _, drive := range set {
			status.DrivesTotal++
			if drive.Online {
				status.DrivesOnline++
			}
		}
	}
	return status, nil
}

// Maintenance returns what this node's background work has done.
func (s *ClusterService) Maintenance(ctx context.Context, p iam.Principal) (MaintenanceStatus, error) {
	if err := s.allow(ctx, p); err != nil {
		return MaintenanceStatus{}, fmt.Errorf("cluster.Maintenance: %w", err)
	}
	return s.cfg.Maintenance.Status(), nil
}
