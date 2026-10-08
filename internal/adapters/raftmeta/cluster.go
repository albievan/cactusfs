package raftmeta

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/ports"
)

// Cluster connects a Store to the other nodes of a cluster.
type Cluster struct {
	// Listener accepts the Raft connections of the other nodes. The caller
	// secures it: it must only hand out connections from authenticated
	// nodes (mutual TLS).
	Listener net.Listener
	// Dial connects to the Raft listener of another node, authenticating
	// both ends.
	Dial func(ctx context.Context, address string) (net.Conn, error)
	// Advertise is the address at which the other nodes reach Listener.
	Advertise string
	// Leader reaches the node that leads the group.
	Leader LeaderClient
}

// LeaderClient carries what a node that does not lead the metadata group
// must ask of the one that does. The node RPC adapter implements it.
type LeaderClient interface {
	// Propose has the leader commit an encoded entry and returns its
	// encoded result (see Store.ProposeLocal).
	//
	// The error must say what is known about the entry: it wraps
	// domain.ErrNotLeader or domain.ErrUnavailable only when the entry was
	// certainly not logged - the node refused it, or was never reached -
	// and domain.ErrOutcomeUnknown whenever it may have been.
	Propose(ctx context.Context, leaderID string, entry []byte) ([]byte, error)
	// ReadIndex returns the leader's read index (see Store.ReadIndexLocal).
	ReadIndex(ctx context.Context, leaderID string) (uint64, error)
	// Term returns the Raft term a node is in (see Store.Term).
	Term(ctx context.Context, nodeID string) (uint64, error)
}

// Member is a node in the configuration of the Raft group.
type Member struct {
	ID       string
	RaftAddr string
	// Voter reports whether the node votes: the nodes with the meta role
	// do; every other node receives the log without a vote.
	Voter bool
}

// streamLayer adapts a listener and a dialer to Raft's network transport.
type streamLayer struct {
	net.Listener
	dial func(ctx context.Context, address string) (net.Conn, error)
	// advertise is where the other nodes reach this one.
	advertise raft.ServerAddress
}

// Dial implements raft.StreamLayer.
func (l *streamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return l.dial(ctx, string(address))
}

// Addr implements raft.StreamLayer. Raft tells its peers that this is where
// the node is, so it is the address the node advertises, not the one the
// listener is bound to.
func (l *streamLayer) Addr() net.Addr {
	return advertised(l.advertise)
}

// advertised is the address a node tells its peers to reach it at.
type advertised raft.ServerAddress

// Network implements net.Addr.
func (advertised) Network() string { return "tcp" }

// String implements net.Addr.
func (a advertised) String() string { return string(a) }

// Formed reports whether this node knows of a Raft group: it bootstrapped
// one, or the leader of one has sent it the configuration.
func (s *Store) Formed() (bool, error) {
	var formed bool
	err := s.use(func() error {
		future := s.raft.GetConfiguration()
		if err := future.Error(); err != nil {
			return translateRaftError(err)
		}
		formed = len(future.Configuration().Servers) > 0
		return nil
	})
	return formed, err
}

// Bootstrap forms a new Raft group from members. Every voter among them
// must call it with the same list; a node that knows of a group already is
// left as it is.
func (s *Store) Bootstrap(members []Member) error {
	return s.use(func() error {
		configuration := raft.Configuration{}
		for _, m := range members {
			suffrage := raft.Nonvoter
			if m.Voter {
				suffrage = raft.Voter
			}
			configuration.Servers = append(configuration.Servers, raft.Server{
				Suffrage: suffrage, ID: raft.ServerID(m.ID), Address: raft.ServerAddress(m.RaftAddr),
			})
		}
		err := s.raft.BootstrapCluster(configuration).Error()
		if err != nil && !errors.Is(err, raft.ErrCantBootstrap) {
			return fmt.Errorf("raftmeta.Bootstrap: %w", err)
		}
		return nil
	})
}

// Members returns the configuration of the Raft group as this node knows it.
func (s *Store) Members() ([]Member, error) {
	var members []Member
	err := s.use(func() error {
		future := s.raft.GetConfiguration()
		if err := future.Error(); err != nil {
			return translateRaftError(err)
		}
		for _, server := range future.Configuration().Servers {
			members = append(members, Member{
				ID: string(server.ID), RaftAddr: string(server.Address), Voter: server.Suffrage == raft.Voter,
			})
		}
		return nil
	})
	return members, err
}

// AddMember adds a node to the Raft group, or changes its address or its
// vote. Only the leader can; elsewhere the error wraps domain.ErrNotLeader.
func (s *Store) AddMember(ctx context.Context, m Member) error {
	if err := cluster.ValidateNodeID(m.ID); err != nil {
		return fmt.Errorf("raftmeta.AddMember: %w", err)
	}
	return s.use(func() error {
		var future raft.IndexFuture
		if m.Voter {
			future = s.raft.AddVoter(raft.ServerID(m.ID), raft.ServerAddress(m.RaftAddr), 0, futureWait(ctx))
		} else {
			future = s.raft.AddNonvoter(raft.ServerID(m.ID), raft.ServerAddress(m.RaftAddr), 0, futureWait(ctx))
		}
		if err := future.Error(); err != nil {
			return fmt.Errorf("raftmeta.AddMember: %w", translateRaftError(err))
		}
		return nil
	})
}

// RemoveMember takes a node out of the Raft group. Only the leader can;
// elsewhere the error wraps domain.ErrNotLeader.
func (s *Store) RemoveMember(ctx context.Context, id string) error {
	return s.use(func() error {
		if err := s.raft.RemoveServer(raft.ServerID(id), 0, futureWait(ctx)).Error(); err != nil {
			return fmt.Errorf("raftmeta.RemoveMember: %w", translateRaftError(err))
		}
		return nil
	})
}

func futureWait(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		return max(time.Until(deadline), time.Millisecond)
	}
	return defaultApplyWait
}

// Leader returns the ID of the node that leads the group, or "" when this
// node knows of none.
func (s *Store) Leader() string {
	var id raft.ServerID
	_ = s.use(func() error {
		_, id = s.raft.LeaderWithID()
		return nil
	})
	return string(id)
}

// IsLeader reports whether this node leads the group.
func (s *Store) IsLeader() bool {
	leader := false
	_ = s.use(func() error {
		leader = s.raft.State() == raft.Leader
		return nil
	})
	return leader
}

// HandOver passes the leadership of the group to another voter, if this node
// has it. A node does so before it stops, so that the others need not wait
// for an election timeout. It is best effort: when there is nobody to hand
// over to, the node just stops.
func (s *Store) HandOver() {
	_ = s.use(func() error {
		if s.transport != nil && s.raft.State() == raft.Leader {
			if err := s.raft.LeadershipTransfer().Error(); err != nil {
				s.logger.Info("metadata leadership was not handed over", slog.Any("error", err))
			}
		}
		return nil
	})
}

// WaitReady waits until this node can read the replicated state: the group
// has a leader and the node has caught up with it.
func (s *Store) WaitReady(ctx context.Context) error {
	for {
		err := s.use(func() error { return s.consistent(ctx) })
		if err == nil || ctx.Err() != nil || !errors.Is(err, domain.ErrUnavailable) {
			return err
		}
	}
}

// ProposeLocal commits an entry that another node proposed, and returns the
// state machine's result in encoded form. The node must lead the group; the
// error wraps domain.ErrNotLeader when it does not, and
// domain.ErrOutcomeUnknown whenever the entry may have been logged.
func (s *Store) ProposeLocal(ctx context.Context, data []byte) ([]byte, error) {
	var encoded []byte
	err := s.use(func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.fsm.health(); err != nil {
			return err
		}
		// Never log what some member could not apply: its state machine
		// would stop.
		if _, err := decodeEntry(data, s.supported); err != nil {
			return err
		}
		if level, err := s.entryVersion(); err != nil {
			return err
		} else if data[0] > level {
			return fmt.Errorf("%w: metadata entry version %d, the cluster is at %d", domain.ErrUnsupportedFormat, data[0], level)
		}
		if s.raft.State() != raft.Leader {
			return domain.ErrNotLeader
		}
		result, err := s.apply(ctx, data)
		if err != nil && result.err == nil {
			return err
		}
		// A refusal is an answer, not a failure of the call.
		if encoded, err = encodeResult(result); err != nil {
			return fmt.Errorf("%w: %w", domain.ErrOutcomeUnknown, err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.ProposeLocal: %w", err)
	}
	return encoded, nil
}

// Subject says what an encoded entry is about, for whoever must decide
// whether its proposer may propose it: the node whose record the entry sets
// or removes. Both are empty for every other entry.
func (s *Store) Subject(data []byte) (registers, removes string, err error) {
	e, err := decodeEntry(data, s.supported)
	if err != nil {
		return "", "", fmt.Errorf("raftmeta.Subject: %w", err)
	}
	switch e.Type {
	case cmdSetNode:
		return e.SetNode.ID, "", nil
	case cmdRemoveNode:
		return "", e.RemoveNode.ID, nil
	default:
		return "", "", nil
	}
}

// entryVersion returns the newest entry schema that may be written now: the
// oldest of what the members of the group understand. A member that has not
// registered yet is assumed to understand only the first.
func (s *Store) entryVersion() (byte, error) {
	if s.supported == entrySchemaVersion {
		// Nothing older exists, so nothing can hold the cluster back.
		return entrySchemaVersion, nil
	}
	if !s.clustered {
		// A node on its own is every member there is.
		return s.supported, nil
	}
	// A state that holds what only schema 2 can express has no member of
	// the previous release: such a member stopped at the first of those
	// entries. A member that has merely not registered yet - one that was
	// just added - does not take the cluster back.
	floor := byte(entrySchemaVersion)
	if err := s.fsm.view(func(tx *bbolt.Tx) error {
		if raw := tx.Bucket(metaBucket()).Get(schemaKey()); len(raw) == 8 && binary.BigEndian.Uint64(raw) >= stateSchemaTimed {
			floor = entrySchemaTimed
		}
		return nil
	}); err != nil {
		return 0, err
	}
	future := s.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		return 0, translateRaftError(err)
	}
	var nodes []cluster.Node
	if err := s.fsm.view(func(tx *bbolt.Tx) (err error) {
		nodes, err = readNodes(tx)
		return err
	}); err != nil {
		return 0, err
	}
	known := map[string]int{}
	for _, node := range nodes {
		known[node.ID] = node.MetadataVersion
	}
	level := s.supported
	for _, server := range future.Configuration().Servers {
		version, registered := known[string(server.ID)]
		if !registered || version < entrySchemaVersion {
			return floor, nil
		}
		for int(level) > version {
			level--
		}
	}
	return max(level, floor), nil
}

// Term returns the Raft term this node is in. A leader asks the voters for
// it to prove that it still leads (see ReadIndexLocal).
func (s *Store) Term() (uint64, error) {
	var term uint64
	err := s.use(func() error {
		term = s.raft.CurrentTerm()
		return nil
	})
	return term, err
}

// ReadIndexLocal returns an index such that a state machine that has applied
// it reflects every change acknowledged before the call. The node must lead
// the group, and proves that it still does by asking a majority of the
// voters; elsewhere the error wraps domain.ErrNotLeader.
func (s *Store) ReadIndexLocal(ctx context.Context) (uint64, error) {
	var index uint64
	err := s.use(func() (err error) {
		index, err = s.readIndexLocal(ctx)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("raftmeta.ReadIndexLocal: %w", err)
	}
	return index, nil
}

// readIndexLocal is the read-index protocol of Raft, in terms of what the
// state machine has applied rather than what Raft has handed to it.
//
// A change is acknowledged only after the leader's state machine applied it.
// So once this node's state machine holds everything from earlier terms -
// established once per term, with a barrier - the index it has applied covers
// every change acknowledged so far, provided no other leader has been
// acknowledging changes meanwhile. That is what the round of confirmations
// rules out: a majority of the voters say, after the read began, that they
// are still in this node's term, so no later leader can have been elected
// before the read began.
func (s *Store) readIndexLocal(ctx context.Context) (uint64, error) {
	if s.raft.State() != raft.Leader {
		return 0, domain.ErrNotLeader
	}
	term := s.raft.CurrentTerm()
	if err := s.caughtUp(ctx, term); err != nil {
		return 0, err
	}
	index := s.fsm.seen.Load()
	confirmed, err := s.verifier.verify(ctx, s)
	if err != nil {
		return 0, err
	}
	if confirmed != term || s.raft.CurrentTerm() != term || s.raft.State() != raft.Leader {
		return 0, fmt.Errorf("%w: leadership changed", domain.ErrNotLeader)
	}
	return index, nil
}

// caughtUp makes sure, once per term this node leads, that its state machine
// has applied everything committed in earlier terms.
func (s *Store) caughtUp(ctx context.Context, term uint64) error {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if s.readyTerm == term {
		return nil
	}
	if err := s.raft.Barrier(futureWait(ctx)).Error(); err != nil {
		// Nothing was proposed; whatever the barrier's error wraps, this
		// is no more than "not the leader now".
		return fmt.Errorf("%w: %s", domain.ErrNotLeader, err.Error())
	}
	if s.raft.CurrentTerm() != term {
		return fmt.Errorf("%w: leadership changed", domain.ErrNotLeader)
	}
	s.readyTerm = term
	return nil
}

// confirmLeadership asks the voters which term they are in and returns this
// node's term once a majority, the node included, is in it.
//
// Raft's own VerifyLeader is not used: it counts the answer of any server it
// replicates to, including those without a vote, so a leader cut off from
// the other voters but not from a storage node would pass.
func (s *Store) confirmLeadership(ctx context.Context) (uint64, error) {
	if s.raft.State() != raft.Leader {
		return 0, domain.ErrNotLeader
	}
	term := s.raft.CurrentTerm()
	future := s.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		return 0, fmt.Errorf("%w: %s", domain.ErrNotLeader, err.Error())
	}
	var others []string
	voters := 0
	for _, server := range future.Configuration().Servers {
		if server.Suffrage != raft.Voter {
			continue
		}
		voters++
		if string(server.ID) != s.nodeID {
			others = append(others, string(server.ID))
		}
	}
	need := voters/2 + 1
	have := voters - len(others) // this node, when it votes
	if have >= need {
		return term, nil
	}
	if s.leader == nil {
		return 0, fmt.Errorf("%w: the other voters cannot be asked", domain.ErrNotLeader)
	}

	ctx, cancel := context.WithTimeout(ctx, s.confirmWait)
	defer cancel()
	answers := make(chan bool, len(others))
	for _, id := range others {
		go func() {
			theirs, err := s.leader.Term(ctx, id)
			answers <- err == nil && theirs == term
		}()
	}
	for range others {
		if <-answers {
			have++
		}
		if have >= need {
			return term, nil
		}
	}
	return 0, fmt.Errorf("%w: only %d of %d voters confirm this node as leader", domain.ErrNotLeader, have, voters)
}

// consistent returns once the local state reflects everything that was
// committed before the call.
func (s *Store) consistent(ctx context.Context) error {
	if err := s.fsm.health(); err != nil {
		return err
	}
	var index uint64
	err := s.onLeader(ctx,
		func() (err error) {
			index, err = s.readIndexLocal(ctx)
			return err
		},
		func(leaderID string) (err error) {
			if index, err = s.leader.ReadIndex(ctx, leaderID); err != nil && ctx.Err() == nil &&
				!errors.Is(err, domain.ErrNotLeader) {
				// Asking again is always safe for a read.
				return fmt.Errorf("%w: %w", domain.ErrUnavailable, err)
			}
			return err
		})
	if err != nil {
		return err
	}
	return s.waitApplied(ctx, index)
}

// waitApplied waits until the local state machine has applied index.
func (s *Store) waitApplied(ctx context.Context, index uint64) error {
	if s.fsm.seen.Load() >= index {
		return nil
	}
	deadline := time.NewTimer(s.leaderWait)
	defer deadline.Stop()
	pause := 200 * time.Microsecond
	for s.fsm.seen.Load() < index {
		if err := s.fsm.health(); err != nil {
			return err
		}
		wait := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			wait.Stop()
			return ctx.Err()
		case <-deadline.C:
			wait.Stop()
			return fmt.Errorf("%w: this node has not caught up with the metadata leader (applied %d, need %d)",
				domain.ErrUnavailable, s.fsm.seen.Load(), index)
		case <-wait.C:
		}
		pause = min(pause*2, 10*time.Millisecond)
	}
	return nil
}

// onLeader runs local when this node leads the group and remote, with the
// leader's ID, when another node does. While the group has no leader, or the
// leader cannot be reached, it tries again - but only after a failure that
// is certain to have changed nothing - until the leader wait has passed; the
// error then wraps domain.ErrUnavailable.
func (s *Store) onLeader(ctx context.Context, local func() error, remote func(leaderID string) error) error {
	deadline := time.Now().Add(s.leaderWait)
	pause := 2 * time.Millisecond
	for {
		var err error
		switch state := s.raft.State(); state {
		case raft.Leader:
			err = local()
		case raft.Shutdown:
			return fmt.Errorf("%w: %w", domain.ErrShuttingDown, domain.ErrNotLeader)
		default:
			_, id := s.raft.LeaderWithID()
			switch {
			case id == "" || string(id) == s.nodeID:
				err = fmt.Errorf("%w: the metadata group has no leader", domain.ErrNotLeader)
			case s.leader == nil:
				return fmt.Errorf("%w: raft state is %s", domain.ErrNotLeader, state)
			default:
				err = remote(string(id))
			}
		}
		definite := (errors.Is(err, domain.ErrNotLeader) || errors.Is(err, domain.ErrUnavailable)) &&
			!errors.Is(err, domain.ErrOutcomeUnknown)
		if err == nil || !definite {
			return err
		}
		if s.leader == nil && s.raft.State() != raft.Candidate && s.raft.State() != raft.Follower {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w: the metadata leader could not be reached: %w", domain.ErrUnavailable, err)
		}
		wait := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			wait.Stop()
			return ctx.Err()
		case <-wait.C:
		}
		pause = min(pause*2, 100*time.Millisecond)
	}
}

// verifier batches leadership checks. A check costs a round trip to the
// voters; every read needs one, and reads that arrive while a round is under
// way share the next.
type verifier struct {
	mu      sync.Mutex
	waiting []chan confirmation
	running bool
}

type confirmation struct {
	term uint64
	err  error
}

// verify returns the term in which a round of confirmations that began after
// the call found this node to be the leader.
func (v *verifier) verify(ctx context.Context, s *Store) (uint64, error) {
	done := make(chan confirmation, 1)
	v.mu.Lock()
	v.waiting = append(v.waiting, done)
	if !v.running {
		v.running = true
		// A round serves every caller that waits for it, so it must not
		// end because the first of them gave up.
		go v.run(context.WithoutCancel(ctx), s)
	}
	v.mu.Unlock()

	select {
	case c := <-done:
		return c.term, c.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (v *verifier) run(ctx context.Context, s *Store) {
	for {
		v.mu.Lock()
		batch := v.waiting
		v.waiting = nil
		if len(batch) == 0 {
			v.running = false
			v.mu.Unlock()
			return
		}
		v.mu.Unlock()

		term, err := s.confirmLeadership(ctx)
		for _, done := range batch {
			done <- confirmation{term: term, err: err}
		}
	}
}

// SetNode implements ports.NodeRegistry.
func (s *Store) SetNode(ctx context.Context, n cluster.Node) error {
	if err := n.Validate(); err != nil {
		return fmt.Errorf("raftmeta.SetNode: %w", err)
	}
	if _, err := s.propose(ctx, entry{Type: cmdSetNode, SetNode: nodeToRecord(n)}); err != nil {
		return fmt.Errorf("raftmeta.SetNode: %w", err)
	}
	return nil
}

// RemoveNode implements ports.NodeRegistry.
func (s *Store) RemoveNode(ctx context.Context, id string) error {
	if _, err := s.propose(ctx, entry{Type: cmdRemoveNode, RemoveNode: &removeNodeCmd{ID: id}}); err != nil {
		return fmt.Errorf("raftmeta.RemoveNode: %w", err)
	}
	return nil
}

// ListNodes implements ports.NodeRegistry.
func (s *Store) ListNodes(ctx context.Context) ([]cluster.Node, error) {
	var nodes []cluster.Node
	err := s.view(ctx, func(tx *bbolt.Tx) (err error) {
		nodes, err = readNodes(tx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.ListNodes: %w", err)
	}
	return nodes, nil
}

// Local returns this node's own copy of the cluster's layout, which may be a
// moment behind the leader's.
func (s *Store) Local() ports.ClusterView {
	return localView{store: s}
}

type localView struct {
	store *Store
}

// Topology implements ports.ClusterView.
func (v localView) Topology(context.Context) (cluster.Topology, error) {
	var topology cluster.Topology
	err := v.store.peek(func(tx *bbolt.Tx) (err error) {
		topology, err = readTopology(tx)
		return err
	})
	return topology, err
}

// Nodes implements ports.ClusterView.
func (v localView) Nodes(context.Context) ([]cluster.Node, error) {
	var nodes []cluster.Node
	err := v.store.peek(func(tx *bbolt.Tx) (err error) {
		nodes, err = readNodes(tx)
		return err
	})
	return nodes, err
}
