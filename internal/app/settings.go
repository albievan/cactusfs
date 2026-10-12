package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/erasure"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// ErasureSetSettings is how one erasure set codes what is stored in it.
type ErasureSetSettings struct {
	Index int
	// Drives is the number of drives in the set.
	Drives int
	// Data and Parity are the shards of one object.
	Data   int
	Parity int
	// ReadQuorum and WriteQuorum are how many shards a read needs and how
	// many a write must have made durable.
	ReadQuorum  int
	WriteQuorum int
}

// ClusterSettings is what holds for the deployment as a whole, from this
// node's copy of the replicated record and its part of the metadata group.
type ClusterSettings struct {
	// DeploymentID is empty while the deployment has no drive topology.
	DeploymentID string
	// MetadataLevel is the format of metadata entry the deployment writes
	// now: the newest every member of the metadata group can apply, as the
	// metadata store of this node works it out. It is never 0.
	MetadataLevel int
	// Leader is the node that leads the metadata group, or "".
	Leader string
	// Nodes is how many nodes are recorded; Voters how many of them have
	// the meta role. A deployment that records no node is the node that
	// answers and nothing else - a cluster of one, which is the one member
	// of its metadata group: one node, one voter.
	Nodes       int
	Voters      int
	ErasureSets []ErasureSetSettings
}

// SettingsView is how the deployment is configured, as one node could gather
// it at one moment. Nothing of it is stored.
type SettingsView struct {
	// AnsweredBy is the node that was asked, and asked the others.
	AnsweredBy string
	Cluster    ClusterSettings
	// Nodes has every node, by ID: its answer, or why there is none.
	Nodes []cluster.NodeAnswer
	// Mismatches are the settings the nodes that answered should agree on
	// and do not (cluster.SettingMismatches).
	Mismatches []cluster.Mismatch
}

// The defaults of a SettingsService.
const (
	// DefaultSettingsNodeTimeout is how long one node is waited for. It is
	// longer than a node is waited for to be connected to (three seconds,
	// in the node RPC client), so that a node that cannot be reached is said
	// to be that - "unreachable" - before its time is up, and "timeout" is
	// left for a node that was reached and did not answer. With the two
	// equal, which was said was a race.
	DefaultSettingsNodeTimeout = 4 * time.Second
	// DefaultSettingsConcurrency is how many nodes are asked at one time.
	DefaultSettingsConcurrency = 8
)

// SettingsServiceConfig lists what a SettingsService depends on.
type SettingsServiceConfig struct {
	View  ports.ClusterView
	Authz ports.Authorizer
	// NodeID is the node the service runs on.
	NodeID string
	// Leader returns the node that leads the metadata group, or "".
	Leader func() string
	// Level returns the format of metadata entry the deployment writes now,
	// which is 1 or more, as the metadata store of this node knows it: the
	// number its metrics show. An error fails the call - a level is not
	// guessed.
	Level func(ctx context.Context) (int, error)
	// Local returns the settings of this node. This node is never asked
	// through Reader.
	Local ports.LocalSettings
	// Reader asks the other nodes. It may be nil on a node that has no way
	// to ask anybody; other nodes, should the record name any, are then
	// unreachable.
	Reader ports.NodeSettingsReader
	// NodeTimeout is how long one node is waited for, counted from when it
	// is asked; zero means DefaultSettingsNodeTimeout.
	NodeTimeout time.Duration
	// Concurrency is how many other nodes are asked at one time; zero
	// means DefaultSettingsConcurrency. With more nodes than that, a node
	// may wait for its turn behind others that are slow, by at most
	// NodeTimeout for every Concurrency nodes ahead of it.
	Concurrency int
	Logger      *slog.Logger
}

// SettingsService answers how the nodes of the deployment are configured. It
// reads what it answers from the nodes each time it is asked.
type SettingsService struct {
	cfg SettingsServiceConfig
}

// NewSettingsService returns a SettingsService. The error wraps
// domain.ErrInvalidArgument when a dependency other than Reader is missing.
func NewSettingsService(cfg SettingsServiceConfig) (*SettingsService, error) {
	if cfg.View == nil || cfg.Authz == nil || cfg.Leader == nil || cfg.Level == nil || cfg.Local == nil || cfg.Logger == nil || cfg.NodeID == "" ||
		cfg.NodeTimeout < 0 || cfg.Concurrency < 0 {
		return nil, fmt.Errorf("settings.New: %w: a dependency is missing", domain.ErrInvalidArgument)
	}
	if cfg.NodeTimeout == 0 {
		cfg.NodeTimeout = DefaultSettingsNodeTimeout
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = DefaultSettingsConcurrency
	}
	return &SettingsService{cfg: cfg}, nil
}

// Get returns the settings of every node and of the deployment. The caller
// must be allowed admin:GetSettings; a refusal is audited by the authorizer,
// a reading is not.
//
// Every other node is asked once, all at the same time as far as Concurrency
// allows, and none is waited for longer than NodeTimeout: a node that is
// down or slow costs its own time and nobody else's. A node that gave no
// answer is in the view with the reason and nothing else - what went wrong is
// logged here and not told to the caller. Every answer is normalized, which
// redacts it once more, whoever gave it.
//
// The error wraps domain.ErrAccessDenied for a caller who may not, the error
// of ctx when the caller went away, and domain.ErrUnavailable when the record
// or the metadata level cannot be read.
func (s *SettingsService) Get(ctx context.Context, p iam.Principal) (SettingsView, error) {
	if err := s.cfg.Authz.Authorize(ctx, iam.Request{
		Principal: p, Action: iam.ActionGetSettings, Resource: iam.Resource{Admin: "settings"},
	}); err != nil {
		return SettingsView{}, fmt.Errorf("settings.Get: %w", err)
	}
	nodes, err := s.cfg.View.Nodes(ctx)
	if err != nil {
		return SettingsView{}, fmt.Errorf("settings.Get: %w", err)
	}
	// A deployment that has not recorded its drives yet has nodes with
	// settings all the same - and that is when somebody wants to read them.
	topology, err := s.cfg.View.Topology(ctx)
	if err != nil && !errors.Is(err, domain.ErrNoTopology) {
		return SettingsView{}, fmt.Errorf("settings.Get: %w", err)
	}
	// The level is the store's to say and not the sum of the records: it
	// is what is written, also where no node is recorded.
	level, err := s.cfg.Level(ctx)
	if err != nil {
		return SettingsView{}, fmt.Errorf("settings.Get: metadata level: %w", err)
	}
	if level < 1 {
		return SettingsView{}, fmt.Errorf("settings.Get: %w: the metadata level is not known", domain.ErrUnavailable)
	}
	view := SettingsView{AnsweredBy: s.cfg.NodeID}
	if view.Cluster, err = clusterSettings(nodes, topology, s.cfg.Leader(), level); err != nil {
		return SettingsView{}, fmt.Errorf("settings.Get: %w", err)
	}

	// This node is part of the answer also when the record does not name it
	// (yet); a node that is named twice is asked once.
	ids := []string{s.cfg.NodeID}
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)

	view.Nodes = s.ask(ctx, ids)
	if err := ctx.Err(); err != nil {
		// The caller went away, or its time is up: what was gathered until
		// then is not an answer.
		return SettingsView{}, fmt.Errorf("settings.Get: %w", err)
	}
	for i := range view.Nodes {
		drives := view.Nodes[i].Settings.Drives
		for j := range drives {
			drives[j].ErasureSet = cluster.ErasureSetUnknown
			if position, ok := topology.Locate(drives[j].ID); ok {
				drives[j].ErasureSet = position.Set
			}
		}
	}
	view.Mismatches = cluster.SettingMismatches(view.Nodes)
	return view, nil
}

// clusterSettings returns what the replicated record says of the deployment,
// at the metadata level the store gave.
func clusterSettings(nodes []cluster.Node, topology cluster.Topology, leader string, level int) (ClusterSettings, error) {
	out := ClusterSettings{
		DeploymentID: topology.DeploymentID, MetadataLevel: level, Leader: leader, Nodes: len(nodes),
		ErasureSets: make([]ErasureSetSettings, len(topology.Sets)),
	}
	for _, node := range nodes {
		if node.Roles.Has(cluster.RoleMeta) {
			out.Voters++
		}
	}
	if len(nodes) == 0 {
		// A node on its own registers nobody, itself included, and is a
		// cluster of one all the same: the node that answers, which is the
		// single member of its metadata group. It is in the list of nodes
		// whatever the record says.
		out.Nodes, out.Voters = 1, 1
	}
	for i, set := range topology.Sets {
		params, err := erasure.ParamsFor(len(set))
		if err != nil {
			return ClusterSettings{}, fmt.Errorf("erasure set %d: %w", i, err)
		}
		out.ErasureSets[i] = ErasureSetSettings{
			Index: i, Drives: len(set), Data: params.Data, Parity: params.Parity,
			ReadQuorum: params.ReadQuorum(), WriteQuorum: params.WriteQuorum(),
		}
	}
	return out, nil
}

// ask returns the answer of every node named, in the order of ids. It
// returns when every node has answered or been given up, which is at once
// when ctx ends.
func (s *SettingsService) ask(ctx context.Context, ids []string) []cluster.NodeAnswer {
	answers := make([]cluster.NodeAnswer, len(ids))
	turn := make(chan struct{}, s.cfg.Concurrency)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() {
			// This node asks nobody and needs no turn.
			if id != s.cfg.NodeID {
				select {
				case turn <- struct{}{}:
					defer func() { <-turn }()
				case <-ctx.Done():
					answers[i] = cluster.NodeAnswer{NodeID: id, Reason: cluster.ReasonFailed}
					return
				}
			}
			answers[i] = s.askNode(ctx, id)
		})
	}
	wg.Wait()
	return answers
}

// errNoReader is why another node cannot be asked by a node that was given
// no way to.
var errNoReader = fmt.Errorf("%w: this node has no way to ask another", domain.ErrUnavailable)

// askNode asks one node and waits for it no longer than NodeTimeout,
// whatever the node - or what asks it - does with the context it is given.
func (s *SettingsService) askNode(ctx context.Context, id string) cluster.NodeAnswer {
	nodeCtx, cancel := context.WithTimeout(ctx, s.cfg.NodeTimeout)
	defer cancel()
	type result struct {
		settings cluster.NodeSettings
		err      error
	}
	// The channel has room for the one result, so that what was given up
	// can still finish and be forgotten.
	done := make(chan result, 1)
	go func() {
		var r result
		switch {
		case id == s.cfg.NodeID:
			r.settings, r.err = s.cfg.Local(nodeCtx)
		case s.cfg.Reader == nil:
			r.err = errNoReader
		default:
			r.settings, r.err = s.cfg.Reader.NodeSettings(nodeCtx, id)
		}
		done <- r
	}()
	var r result
	select {
	case r = <-done:
	case <-nodeCtx.Done():
		r.err = fmt.Errorf("gave up waiting: %w", nodeCtx.Err())
	}
	if r.err != nil {
		reason := settingsReason(r.err)
		if ctx.Err() == nil {
			// The cause stays here: the caller is told the reason and no
			// word of an error, which may name addresses and paths.
			s.cfg.Logger.WarnContext(ctx, "a node did not give its settings",
				slog.String("node", id), slog.String("reason", string(reason)), slog.Any("error", r.err))
		}
		return cluster.NodeAnswer{NodeID: id, Reason: reason}
	}
	// Whoever gave the answer, it is redacted and put in order again here,
	// and it is the answer of the node that was asked.
	settings := r.settings.Normalized()
	settings.NodeID = id
	return cluster.NodeAnswer{NodeID: id, Answered: true, Settings: settings}
}

// settingsReason sorts the error of asking a node into the reasons a caller
// is told (ports.NodeSettingsReader). A node that could not be reached in
// time is unreachable; one that was reached and did not answer in time timed
// out.
func settingsReason(err error) cluster.AnswerReason {
	switch {
	case errors.Is(err, domain.ErrNotSupported):
		return cluster.ReasonUnsupported
	case errors.Is(err, domain.ErrUnavailable):
		return cluster.ReasonUnreachable
	case errors.Is(err, context.DeadlineExceeded):
		return cluster.ReasonTimeout
	default:
		return cluster.ReasonFailed
	}
}
