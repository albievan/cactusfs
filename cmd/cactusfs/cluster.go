package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/noderpc"
	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

const (
	// rpcStopTimeout is how long node RPC calls in progress get to finish
	// when the node stops.
	rpcStopTimeout = 10 * time.Second
	// peerTimeout bounds one question to one peer while a cluster forms.
	peerTimeout = 5 * time.Second
	// formationPause is the wait between two attempts to form or join.
	formationPause = 500 * time.Millisecond
	// progressInterval is how often a node that is waiting says what for.
	progressInterval = 10 * time.Second
	// nodeCacheTime is how long the record of nodes is used before it is
	// read again.
	nodeCacheTime = time.Second

	minStorageNodes = 4
)

// progress paces a node that is waiting for the rest of its cluster and has
// it say, now and then, what it is waiting for.
type progress struct {
	logger *slog.Logger
	what   string
	last   time.Time
}

func newProgress(logger *slog.Logger, what string) *progress {
	return &progress{logger: logger, what: what}
}

// pause waits for a moment, or returns the context's error.
func (p *progress) pause(ctx context.Context, reason error) error {
	if time.Since(p.last) >= progressInterval {
		p.logger.InfoContext(ctx, p.what, slog.Any("reason", reason))
		p.last = time.Now()
	}
	timer := time.NewTimer(formationPause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("%s: %w (last: %w)", p.what, ctx.Err(), reason)
	case <-timer.C:
		return nil
	}
}

// transient reports whether an error may go away while a cluster forms.
func transient(err error) bool {
	return errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrNotLeader) ||
		errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNoTopology)
}

// openCluster loads the node's certificate, starts node RPC and returns what
// the metadata store needs to reach the other nodes. Node RPC is up before
// the metadata store, because that is how the nodes of a new cluster find
// each other.
func (n *node) openCluster(ctx context.Context, cfg serverConfig, logger *slog.Logger) (*raftmeta.Cluster, error) {
	identity, err := nodetls.Load(cfg.NodeTLSCert, cfg.NodeTLSKey, cfg.NodeTLSCA)
	if err != nil {
		return nil, err
	}
	if identity.NodeID() != cfg.NodeID {
		return nil, fmt.Errorf("%w: the node certificate names %q, this node is %q",
			domain.ErrInvalidArgument, identity.NodeID(), cfg.NodeID)
	}
	n.identity = identity
	n.rpc = noderpc.NewClient(identity.Client, n.resolver(cfg), n.clock)

	// Raft trusts what its messages say about who sent them, so the
	// listener admits only the nodes that may send any: the voters.
	raftListener, err := identity.Listen(ctx, cfg.RaftAddr, func(nodeID string) bool { return n.mayReplicate(cfg, nodeID) })
	if err != nil {
		return nil, err
	}
	rpcListener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.RPCAddr)
	if err != nil {
		_ = raftListener.Close()
		return nil, fmt.Errorf("node RPC: %w", err)
	}
	n.rpcServer = noderpc.NewServer(noderpc.ServerConfig{
		TLS:        identity.Server(),
		PeerNodeID: nodetls.PeerNodeID,
		Authz:      n.authorizer,
		Info:       func(ctx context.Context) (noderpc.NodeInfo, error) { return n.info(ctx, cfg) },
		InFlight: func(id object.ID) bool {
			objects := n.inFlight.Load()
			return objects != nil && objects.InFlight(id)
		},
		Meta:   metaRPC{node: n},
		Drive:  n.localDrive,
		Clock:  n.clock,
		Logger: logger,
	})
	n.rpcDone = make(chan error, 1)
	go func() { n.rpcDone <- n.rpcServer.Serve(rpcListener) }()
	logger.InfoContext(ctx, "listening", slog.String("listener", "node-rpc"), slog.String("addr", rpcListener.Addr().String()),
		slog.Bool("tls", true))
	logger.InfoContext(ctx, "listening", slog.String("listener", "raft"), slog.String("addr", raftListener.Addr().String()),
		slog.Bool("tls", true))

	return &raftmeta.Cluster{
		Listener:  raftListener,
		Dial:      identity.Dial,
		Advertise: nodetls.Address(cfg.NodeID, cfg.AdvertiseRaft),
		Leader:    n.rpc,
	}, nil
}

// localDrive returns one of this node's drives for node RPC.
func (n *node) localDrive(driveID string) (ports.ShardStore, bool) {
	for _, drive := range n.drives {
		if drive.DriveID() == driveID {
			return n.telemetry.InstrumentShardStore(drive), true
		}
	}
	return nil, false
}

// info describes this node to its peers.
func (n *node) info(ctx context.Context, cfg serverConfig) (noderpc.NodeInfo, error) {
	info := noderpc.NodeInfo{Node: n.self(cfg)}
	meta := n.started.Load()
	if meta == nil {
		// Asked in the instant between the start of node RPC and of the
		// metadata store.
		return info, fmt.Errorf("%w: node %s is starting", domain.ErrUnavailable, cfg.NodeID)
	}
	formed, err := meta.Formed()
	if err != nil {
		return info, err
	}
	info.Formed = formed
	if topology, err := meta.Local().Topology(ctx); err == nil {
		info.DeploymentID = topology.DeploymentID
	} else if !errors.Is(err, domain.ErrNoTopology) {
		return info, err
	}
	for _, drive := range n.drives {
		info.Drives = append(info.Drives, cluster.DriveState{
			ID: drive.DriveID(), NodeID: cfg.NodeID, Empty: drive.Empty(), DeploymentID: drive.DeploymentID(),
		})
	}
	return info, nil
}

// self is this node as the cluster records it.
func (n *node) self(cfg serverConfig) cluster.Node {
	return cluster.Node{
		ID: cfg.NodeID, RPCAddr: cfg.AdvertiseRPC, RaftAddr: nodetls.Address(cfg.NodeID, cfg.AdvertiseRaft),
		Roles: cfg.Roles, Labels: cfg.Labels, MetadataVersion: raftmeta.MetadataVersion,
	}
}

// resolver finds the RPC address of a node: in the cluster's record of its
// nodes, and for a node that is not recorded yet, in the list of peers this
// node was started with.
func (n *node) resolver(cfg serverConfig) func(ctx context.Context, nodeID string) (string, bool) {
	var mu sync.Mutex
	var cached map[string]string
	var read time.Time
	return func(ctx context.Context, nodeID string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if meta := n.started.Load(); meta != nil && time.Since(read) > nodeCacheTime {
			if nodes, err := meta.Local().Nodes(ctx); err == nil {
				cached = make(map[string]string, len(nodes))
				for _, node := range nodes {
					cached[node.ID] = node.RPCAddr
				}
				read = time.Now()
			}
		}
		if address, ok := cached[nodeID]; ok && address != "" {
			return address, true
		}
		address, ok := cfg.Peers[nodeID]
		return address, ok
	}
}

// metaRPC is the metadata store as node RPC sees it.
type metaRPC struct {
	node *node
}

func (m metaRPC) store() (*raftmeta.Store, error) {
	meta := m.node.started.Load()
	if meta == nil {
		return nil, fmt.Errorf("%w: the node is starting", domain.ErrUnavailable)
	}
	return meta, nil
}

func (m metaRPC) ProposeLocal(ctx context.Context, entry []byte) ([]byte, error) {
	store, err := m.store()
	if err != nil {
		return nil, err
	}
	return store.ProposeLocal(ctx, entry)
}

func (m metaRPC) ReadIndexLocal(ctx context.Context) (uint64, error) {
	store, err := m.store()
	if err != nil {
		return 0, err
	}
	return store.ReadIndexLocal(ctx)
}

func (m metaRPC) Term() (uint64, error) {
	store, err := m.store()
	if err != nil {
		return 0, err
	}
	return store.Term()
}

func (m metaRPC) Subject(entry []byte) (registers, removes string, err error) {
	store, err := m.store()
	if err != nil {
		return "", "", err
	}
	return store.Subject(entry)
}

// Join adds a node to the metadata group. The set of voters is fixed when
// the cluster forms: a node gets a vote here only if it has one already and
// is being added again after starting late. Anything else joins without.
func (m metaRPC) Join(ctx context.Context, nodeID, raftAddr string, voter bool) error {
	store, err := m.store()
	if err != nil {
		return err
	}
	// The Raft address names the node whose certificate must answer there.
	if named, _, err := nodetls.SplitAddress(raftAddr); err != nil {
		return err
	} else if named != nodeID {
		return fmt.Errorf("%w: Raft address %q does not name node %s", domain.ErrInvalidArgument, raftAddr, nodeID)
	}
	members, err := store.Members()
	if err != nil {
		return err
	}
	votes := false
	for _, member := range members {
		if member.ID == nodeID {
			votes = member.Voter
		}
	}
	if voter && !votes {
		return fmt.Errorf("%w: node %s asks for a vote in the metadata group; the voters of a cluster are fixed "+
			"when it forms, and adding one is not supported yet", domain.ErrInvalidArgument, nodeID)
	}
	return store.AddMember(ctx, raftmeta.Member{ID: nodeID, RaftAddr: raftAddr, Voter: votes})
}

func (m metaRPC) Leave(ctx context.Context, nodeID string) error {
	store, err := m.store()
	if err != nil {
		return err
	}
	if err := store.RemoveMember(ctx, nodeID); err != nil {
		return err
	}
	return store.RemoveNode(ctx, nodeID)
}

// mayReplicate reports whether a node may open a Raft connection to this
// one: a voter of the metadata group as this node knows it, or - while this
// node knows no group yet - a node of the list it was started with.
func (n *node) mayReplicate(cfg serverConfig, nodeID string) bool {
	meta := n.started.Load()
	if meta == nil {
		return false
	}
	members, err := meta.Members()
	if err != nil {
		return false
	}
	if len(members) == 0 {
		_, listed := cfg.Peers[nodeID]
		return listed
	}
	for _, member := range members {
		if member.ID == nodeID {
			return member.Voter
		}
	}
	return false
}

// peerInfos asks every other node of the --peers list to describe itself.
// It returns what the ones that answered said, and the ones that did not.
func (n *node) peerInfos(ctx context.Context, cfg serverConfig) (map[string]noderpc.NodeInfo, map[string]error) {
	type answer struct {
		id   string
		info noderpc.NodeInfo
		err  error
	}
	answers := make(chan answer, len(cfg.Peers))
	asked := 0
	for id, address := range cfg.Peers {
		if id == cfg.NodeID {
			continue
		}
		asked++
		go func() {
			ctx, cancel := context.WithTimeout(ctx, peerTimeout)
			defer cancel()
			info, err := n.rpc.Info(ctx, id, address)
			answers <- answer{id: id, info: info, err: err}
		}()
	}
	infos, missing := map[string]noderpc.NodeInfo{}, map[string]error{}
	for range asked {
		a := <-answers
		if a.err != nil {
			missing[a.id] = a.err
			continue
		}
		infos[a.id] = a.info
	}
	return infos, missing
}

// joinCluster makes this node part of the metadata group and records it as a
// node of the cluster.
//
// A new cluster is formed by its voters, each from the same list: the nodes
// of --peers, as they describe themselves. So nothing is formed until every
// node has answered. Once any node knows of a deployment the cluster exists,
// and a node that is not part of the group asks its leader to be added
// instead - it never forms a second group.
func (n *node) joinCluster(ctx context.Context, cfg serverConfig, logger *slog.Logger) error {
	waiting := newProgress(logger, "waiting to form or join the cluster")
	for {
		formed, err := n.meta.Formed()
		if err != nil {
			return err
		}
		if formed {
			break
		}
		reason, err := n.formOrJoin(ctx, cfg, logger)
		if err != nil {
			return err
		}
		if err := waiting.pause(ctx, reason); err != nil {
			return err
		}
	}
	waiting = newProgress(logger, "waiting for the metadata group to have a leader")
	for {
		waitCtx, cancel := context.WithTimeout(ctx, progressInterval)
		err := n.meta.WaitReady(waitCtx)
		cancel()
		if err == nil {
			break
		}
		if ctx.Err() != nil || (!transient(err) && !errors.Is(err, context.DeadlineExceeded)) {
			return err
		}
		if err := waiting.pause(ctx, err); err != nil {
			return err
		}
	}
	self := n.self(cfg)
	for {
		err := n.meta.SetNode(ctx, self)
		if err == nil {
			break
		}
		if !transient(err) {
			return err
		}
		if err := waiting.pause(ctx, err); err != nil {
			return err
		}
	}
	logger.InfoContext(ctx, "joined the cluster", slog.String("node_id", cfg.NodeID), slog.String("roles", cfg.Roles.String()),
		slog.String("leader", n.meta.Leader()))
	return nil
}

// formOrJoin takes one step towards being part of the metadata group. It
// returns why the node is not there yet, or an error that waiting cannot
// cure.
func (n *node) formOrJoin(ctx context.Context, cfg serverConfig, logger *slog.Logger) (reason, fatal error) {
	infos, missing := n.peerInfos(ctx, cfg)
	voter := cfg.Roles.Has(cluster.RoleMeta)

	exists := false
	for _, info := range infos {
		exists = exists || info.DeploymentID != ""
	}
	if exists {
		// The cluster is there. Its leader adds this node - again, if the
		// node was part of it all along and merely started late.
		self := n.self(cfg)
		var last error
		for id := range infos {
			err := n.rpc.Join(ctx, id, self.ID, self.RaftAddr, voter)
			if err == nil {
				logger.InfoContext(ctx, "added to the metadata group", slog.String("by", id), slog.Bool("voter", voter))
				return errors.New("waiting to hear from the leader"), nil
			}
			if !transient(err) {
				return nil, fmt.Errorf("join the cluster through %s: %w", id, err)
			}
			last = err
		}
		return fmt.Errorf("no node that answered leads the metadata group: %w", last), nil
	}
	if len(missing) > 0 {
		ids := slices.Sorted(maps.Keys(missing))
		return fmt.Errorf("%w: a new cluster is formed when all its nodes are up; no answer from %v (%w)",
			domain.ErrUnavailable, ids, missing[ids[0]]), nil
	}

	// A new cluster, and every node has answered.
	infos[cfg.NodeID] = noderpc.NodeInfo{Node: n.self(cfg)}
	var members []raftmeta.Member
	voters, storage := 0, 0
	for _, id := range slices.Sorted(maps.Keys(infos)) {
		node := infos[id].Node
		member := raftmeta.Member{ID: id, RaftAddr: node.RaftAddr, Voter: node.Roles.Has(cluster.RoleMeta)}
		if member.Voter {
			voters++
		}
		if node.Roles.Has(cluster.RoleStorage) {
			storage++
		}
		members = append(members, member)
	}
	if voters != 3 && voters != 5 {
		return nil, fmt.Errorf("%w: a cluster has 3 or 5 nodes with the meta role, this one would have %d",
			domain.ErrInvalidArgument, voters)
	}
	if storage < minStorageNodes {
		return nil, fmt.Errorf("%w: a cluster has at least %d nodes with the storage role, this one would have %d",
			domain.ErrInvalidArgument, minStorageNodes, storage)
	}
	if !voter {
		return errors.New("waiting for the nodes with the meta role to form the metadata group"), nil
	}
	if err := n.meta.Bootstrap(members); err != nil {
		return nil, err
	}
	logger.InfoContext(ctx, "metadata group formed", slog.Int("voters", voters), slog.Int("nodes", len(members)))
	return errors.New("waiting for the election of a leader"), nil
}

// surveyDrives returns the drives of every other storage node of a new
// cluster. Only the node that leads the metadata group records the topology,
// and only when every node has answered.
func (n *node) surveyDrives(ctx context.Context, cfg serverConfig) ([]cluster.DriveState, map[string]cluster.Labels, error) {
	if !n.meta.IsLeader() {
		return nil, nil, fmt.Errorf("%w: the leader of the metadata group records the topology", domain.ErrUnavailable)
	}
	infos, missing := n.peerInfos(ctx, cfg)
	if len(missing) > 0 {
		ids := slices.Sorted(maps.Keys(missing))
		return nil, nil, fmt.Errorf("%w: the topology is recorded when every node has reported its drives; no answer from %v (%w)",
			domain.ErrUnavailable, ids, missing[ids[0]])
	}
	var drives []cluster.DriveState
	labels := map[string]cluster.Labels{}
	for _, id := range slices.Sorted(maps.Keys(infos)) {
		labels[id] = infos[id].Node.Labels
		drives = append(drives, infos[id].Drives...)
	}
	return drives, labels, nil
}

// inFlightAnywhere reports whether any node has an unresolved write that
// uses the stored data. A node that cannot be asked makes it an error, which
// the collector takes for a yes.
func (n *node) inFlightAnywhere(ctx context.Context, dataID object.ID) (bool, error) {
	if objects := n.inFlight.Load(); objects != nil && objects.InFlight(dataID) {
		return true, nil
	}
	if n.rpc == nil {
		return false, nil
	}
	// Every recorded node is asked, whatever its roles: any node can be
	// writing shards, for a client or to heal.
	nodes, err := n.meta.ListNodes(ctx)
	if err != nil {
		return false, err
	}
	for _, peer := range nodes {
		if peer.ID == n.id {
			continue
		}
		busy, err := n.rpc.InFlight(ctx, peer.ID, dataID)
		if err != nil {
			return false, err
		}
		if busy {
			return true, nil
		}
	}
	return false, nil
}
