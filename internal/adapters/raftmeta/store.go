// Package raftmeta is the outbound adapter for replicated metadata: buckets,
// the object index and the root credential, replicated with hashicorp/raft
// and stored in bbolt. No external database is involved.
//
// Every change is proposed to Raft and takes effect only when the entry has
// been committed and applied; that commit is the single point at which a
// written object becomes visible. A standalone node runs the same code as a
// cluster, with a Raft group of one voter.
package raftmeta

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/adapters/fsutil"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

const (
	nodeFile     = "node.json"
	nodeMagic    = "cactusfs-meta-node"
	nodeVersion  = 1
	raftLogFile  = "raft.db"
	stateFile    = "state.db"
	restoreFile  = "state.db.restore"
	snapshotsDir = "snapshots"

	dirMode            = 0o700
	lockTimeout        = 2 * time.Second
	retainedSnapshots  = 2
	nodeIDSize         = 8
	defaultApplyWait   = 10 * time.Second
	defaultLeaderWait  = 10 * time.Second
	defaultConfirmWait = 2 * time.Second

	clusterCommitTimeout = 10 * time.Millisecond
	raftConnectionPool   = 3
	raftIOTimeout        = 10 * time.Second
)

// Config configures a Store.
type Config struct {
	// Dir is the metadata directory. It is created when missing.
	Dir string
	// Logger receives the adapter's and Raft's log messages.
	Logger *slog.Logger
	// Entropy names a new node. It must be cryptographically secure.
	Entropy io.Reader
	// Timing overrides Raft's timeouts; the zero value keeps the defaults.
	// Tests shorten them.
	Timing Timing
	// NodeID names this node. It is fixed at the first start: when empty a
	// name is generated then, and a later start with another name is
	// refused.
	NodeID string
	// Cluster makes the store a member of a Raft group of several nodes.
	// When nil the node is the only voter of its own group.
	Cluster *Cluster
	// Events, when not nil, receives the events of every applied entry.
	Events ports.EventPublisher
}

// Timing holds the Raft timeouts a caller may override.
type Timing struct {
	Heartbeat   time.Duration
	Election    time.Duration
	LeaderLease time.Duration
	Commit      time.Duration
	// LeaderWait is how long an operation waits for the group to have a
	// leader before it fails as unavailable. The default is ten seconds.
	LeaderWait time.Duration
	// TrailingLogs is how many entries stay in the log behind a snapshot;
	// zero keeps Raft's default. A node further behind than that is sent
	// the snapshot instead.
	TrailingLogs uint64
}

// nodeInfo is the content of node.json: the stable identity of this metadata
// node, which Raft's configuration refers to.
type nodeInfo struct {
	Magic         string `json:"magic"`
	FormatVersion int    `json:"format_version"`
	NodeID        string `json:"node_id"`
}

// Store is the replicated metadata store. It implements ports.BucketStore,
// ports.ObjectIndex, ports.RootCredentialStore and ports.ReadinessCheck.
type Store struct {
	nodeID string

	// mu guards the fields below against Close: every operation holds it
	// for reading, so Close waits for operations in flight and later ones
	// are refused instead of meeting a store that is half torn down.
	mu     sync.RWMutex
	closed bool
	root   *os.Root
	fsm    *fsm
	logs   *raftboltdb.BoltStore
	raft   *raft.Raft

	logger *slog.Logger
	// leader reaches the leader of the group from a node that is not it;
	// nil when the node is alone.
	leader     LeaderClient
	transport  *raft.NetworkTransport
	leaderWait time.Duration
	// confirmWait bounds one round of asking the voters for their term.
	confirmWait time.Duration
	verifier    verifier
	// readyTerm is the term in which this node, as leader, has made sure
	// its state machine holds everything from earlier terms.
	readyMu   sync.Mutex
	readyTerm uint64
	// supported is the newest entry schema this binary applies; versionOf
	// names the schema an entry needs. Tests replace both to stand in for
	// the binary of a later release.
	supported byte
	versionOf func(entry) byte
	// stopped is closed when the state machine has stopped.
	stopped  chan struct{}
	stopOnce sync.Once
}

var (
	_ ports.BucketStore         = (*Store)(nil)
	_ ports.ObjectIndex         = (*Store)(nil)
	_ ports.RootCredentialStore = (*Store)(nil)
	_ ports.TopologyStore       = (*Store)(nil)
	_ ports.NodeRegistry        = (*Store)(nil)
	_ ports.ReadinessCheck      = (*Store)(nil)
)

// Open opens the metadata store in cfg.Dir. The caller must Close the store.
//
// A node on its own creates a single-voter Raft group on first use, and Open
// waits until the node leads it or ctx ends. A member of a cluster starts
// without a group and returns at once: the group is formed with Bootstrap, or
// the node is added to an existing one by its leader.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if err := os.MkdirAll(cfg.Dir, dirMode); err != nil {
		return nil, fmt.Errorf("raftmeta.Open: %w", err)
	}
	root, err := os.OpenRoot(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("raftmeta.Open: %w", err)
	}
	s := &Store{
		root: root, logger: cfg.Logger, leaderWait: cfg.Timing.LeaderWait,
		supported: entrySchemaVersion, versionOf: versionOf, stopped: make(chan struct{}),
	}
	if s.leaderWait <= 0 {
		s.leaderWait = defaultLeaderWait
	}
	s.confirmWait = min(s.leaderWait, defaultConfirmWait)
	if err := s.start(ctx, cfg); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("raftmeta.Open: %s: %w", cfg.Dir, err)
	}
	go s.leaveWhenStopped()
	if cfg.Cluster != nil {
		return s, nil
	}
	if err := s.waitForLeadership(ctx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("raftmeta.Open: %w", err)
	}
	return s, nil
}

// leaveWhenStopped takes the node out of the Raft group when its state
// machine can no longer apply entries. A leader in that condition would hold
// the whole cluster up; this way the others elect a new one. The node stays
// out until it is restarted.
func (s *Store) leaveWhenStopped() {
	<-s.stopped
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.closed {
		_ = s.raft.Shutdown().Error()
	}
}

func (s *Store) start(ctx context.Context, cfg Config) error {
	var err error
	if s.nodeID, err = loadOrCreateNodeID(s.root, cfg.Entropy, cfg.NodeID); err != nil {
		return err
	}
	if s.fsm, err = openFSM(s.root); err != nil {
		return err
	}
	s.fsm.supported = s.supported
	if events := cfg.Events; events != nil {
		// Events are published for as long as the store lives, not for as
		// long as opening it took.
		ctx := context.WithoutCancel(ctx)
		s.fsm.publish = func(e ports.Event) {
			if err := events.Publish(ctx, e); err != nil {
				// The publisher counts what it drops; a line per event
				// would flood the log of a node that replays a long log.
				cfg.Logger.Debug("event not delivered", slog.String("event_id", e.ID), slog.String("type", e.Type), slog.Any("error", err))
			}
		}
	}
	s.fsm.onFail = func(err error) {
		cfg.Logger.Error("metadata state machine stopped; this node leaves the metadata group until it is restarted", slog.Any("error", err))
		s.stopOnce.Do(func() { close(s.stopped) })
	}
	dir := s.root.Name()
	s.logs, err = raftboltdb.New(raftboltdb.Options{
		Path:        filepath.Join(dir, raftLogFile),
		BoltOptions: &bbolt.Options{Timeout: lockTimeout},
	})
	if err != nil {
		return fmt.Errorf("open raft log: %w", err)
	}
	logger := newHCLogger(cfg.Logger.With(slog.String("component", "raft")))
	snapshots, err := raft.NewFileSnapshotStoreWithLogger(dir, retainedSnapshots, logger)
	if err != nil {
		return fmt.Errorf("open snapshot store: %w", err)
	}

	conf := raft.DefaultConfig()
	conf.LocalID = raft.ServerID(s.nodeID)
	conf.Logger = logger
	// The state database is durable on its own. Restoring the latest
	// snapshot over it at every start would move it back in time.
	conf.NoSnapshotRestoreOnStart = true
	if cfg.Cluster != nil {
		// A follower learns that an entry is committed with the leader's
		// next message, at the latest after this long. Every read on a
		// node that does not lead waits for that, so it is kept short;
		// the price is more frequent heartbeats on an idle cluster.
		conf.CommitTimeout = clusterCommitTimeout
	}
	if cfg.Timing.TrailingLogs > 0 {
		conf.TrailingLogs = cfg.Timing.TrailingLogs
	}
	if t := cfg.Timing; t.Heartbeat > 0 {
		conf.HeartbeatTimeout, conf.ElectionTimeout = t.Heartbeat, t.Election
		conf.LeaderLeaseTimeout, conf.CommitTimeout = t.LeaderLease, t.Commit
	}

	var transport raft.Transport
	address := raft.ServerAddress(s.nodeID)
	if c := cfg.Cluster; c != nil {
		// Members of a cluster talk Raft over connections the caller has
		// secured with mutual TLS.
		s.leader = c.Leader
		address = raft.ServerAddress(c.Advertise)
		s.transport = raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
			Stream:                &streamLayer{Listener: c.Listener, dial: c.Dial},
			MaxPool:               raftConnectionPool,
			Timeout:               raftIOTimeout,
			Logger:                logger,
			ServerAddressProvider: addressProvider{advertise: address, local: conf.LocalID},
		})
		transport = s.transport
	} else {
		// One voter needs no network. The address is fixed because Raft
		// stores it in its configuration.
		_, transport = raft.NewInmemTransport(address)
	}

	existing, err := raft.HasExistingState(s.logs, s.logs, snapshots)
	if err != nil {
		return fmt.Errorf("inspect raft state: %w", err)
	}
	if !existing && cfg.Cluster == nil {
		servers := raft.Configuration{Servers: []raft.Server{{Suffrage: raft.Voter, ID: conf.LocalID, Address: address}}}
		if err := raft.BootstrapCluster(conf, s.logs, s.logs, snapshots, transport, servers); err != nil {
			return fmt.Errorf("bootstrap raft: %w", err)
		}
	}
	if err := s.catchUpToSnapshot(snapshots); err != nil {
		return err
	}
	if s.raft, err = raft.NewRaft(conf, s.fsm, s.logs, s.logs, snapshots, transport); err != nil {
		return fmt.Errorf("start raft: %w", err)
	}
	return nil
}

// catchUpToSnapshot makes sure the state database is at least as new as the
// newest Raft snapshot. Raft is configured not to restore a snapshot at
// start, because the state database is durable on its own; but then Raft only
// replays the log that follows the snapshot. A state database that is missing
// or older than the snapshot - deleted, or restored from an old copy - would
// come up looking healthy with part of the metadata gone. In that case the
// snapshot is restored first.
func (s *Store) catchUpToSnapshot(snapshots raft.SnapshotStore) error {
	list, err := snapshots.List()
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	if len(list) == 0 {
		return nil
	}
	newest := list[0]
	applied, err := s.fsm.appliedIndex()
	if err != nil {
		return err
	}
	if applied >= newest.Index {
		return nil
	}
	_, source, err := snapshots.Open(newest.ID)
	if err != nil {
		return fmt.Errorf("open snapshot %s: %w", newest.ID, err)
	}
	if err := s.fsm.Restore(source); err != nil {
		return fmt.Errorf("state database is behind snapshot %s and could not be restored from it: %w", newest.ID, err)
	}
	return nil
}

func (s *Store) waitForLeadership(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for s.raft.State() != raft.Leader {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting to lead the metadata group: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	// A barrier makes sure every entry committed before this start has been
	// applied before the first read is served.
	if err := s.raft.Barrier(defaultApplyWait).Error(); err != nil {
		return fmt.Errorf("applying committed metadata: %w", translateRaftError(err))
	}
	return s.fsm.health()
}

// loadOrCreateNodeID returns the node's identity. The first start fixes it:
// as want when that is given, generated otherwise. A later start that asks
// for another name is refused, because Raft and the drive topology know the
// node by the first.
func loadOrCreateNodeID(root *os.Root, entropy io.Reader, want string) (string, error) {
	data, err := root.ReadFile(nodeFile)
	if err == nil {
		var info nodeInfo
		if err := json.Unmarshal(data, &info); err != nil || info.Magic != nodeMagic || info.NodeID == "" {
			return "", fmt.Errorf("%w: %s is not a node file", domain.ErrIntegrity, nodeFile)
		}
		if info.FormatVersion != nodeVersion {
			return "", fmt.Errorf("%w: node file version %d", domain.ErrUnsupportedFormat, info.FormatVersion)
		}
		if want != "" && want != info.NodeID {
			return "", fmt.Errorf("%w: this metadata directory belongs to node %q, not %q; a node keeps its name",
				domain.ErrInvalidArgument, info.NodeID, want)
		}
		return info.NodeID, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	info := nodeInfo{Magic: nodeMagic, FormatVersion: nodeVersion, NodeID: want}
	if want == "" {
		id := make([]byte, nodeIDSize)
		if _, err := io.ReadFull(entropy, id); err != nil {
			return "", fmt.Errorf("generate node ID: %w", err)
		}
		info.NodeID = "node-" + hex.EncodeToString(id)
	}
	if err := cluster.ValidateNodeID(info.NodeID); err != nil {
		return "", err
	}
	data, err = json.MarshalIndent(info, "", "  ")
	if err != nil {
		return "", err
	}
	if err := fsutil.WriteNewDurable(root, nodeFile, append(data, '\n'), stateFileMode); err != nil {
		return "", err
	}
	return info.NodeID, nil
}

// Close stops Raft and closes the databases. It waits for operations in
// flight; operations that start afterwards fail with domain.ErrShuttingDown.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true

	var errs []error
	s.stopOnce.Do(func() { close(s.stopped) })
	if s.raft != nil {
		errs = append(errs, s.raft.Shutdown().Error())
	}
	if s.transport != nil {
		errs = append(errs, s.transport.Close())
	}
	if s.logs != nil {
		errs = append(errs, s.logs.Close())
	}
	if s.fsm != nil {
		errs = append(errs, s.fsm.close())
	}
	if s.root != nil {
		errs = append(errs, s.root.Close())
	}
	return errors.Join(errs...)
}

// use runs fn while holding the store open. It fails with
// domain.ErrShuttingDown once the store has been closed.
func (s *Store) use(fn func() error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return domain.ErrShuttingDown
	}
	return fn()
}

// NodeID returns the stable identity of this metadata node.
func (s *Store) NodeID() string {
	return s.nodeID
}

// Name implements ports.ReadinessCheck.
func (s *Store) Name() string {
	return "metadata"
}

// Check implements ports.ReadinessCheck: the node is ready when its state
// machine is healthy and the metadata group has a leader it knows of.
func (s *Store) Check(context.Context) error {
	return s.use(func() error {
		if err := s.fsm.health(); err != nil {
			return err
		}
		if address, _ := s.raft.LeaderWithID(); address == "" {
			return fmt.Errorf("%w: the metadata group has no leader (raft state is %s)", domain.ErrNotLeader, s.raft.State())
		}
		return nil
	})
}

// Snapshot forces a Raft snapshot, which lets the log be truncated.
func (s *Store) Snapshot() error {
	return s.use(func() error {
		if err := s.raft.Snapshot().Error(); err != nil {
			return fmt.Errorf("raftmeta.Snapshot: %w", translateRaftError(err))
		}
		return nil
	})
}

// AppliedIndex returns the index of the last Raft entry reflected in the
// state.
func (s *Store) AppliedIndex() (uint64, error) {
	var index uint64
	err := s.use(func() (err error) {
		index, err = s.fsm.appliedIndex()
		return err
	})
	return index, err
}

// view runs fn in a read transaction on the state, once the state reflects
// everything that was committed before the call: reads are linearizable on
// every node, whether it leads or not.
func (s *Store) view(ctx context.Context, fn func(tx *bbolt.Tx) error) error {
	return s.use(func() error {
		if err := s.consistent(ctx); err != nil {
			return err
		}
		return s.fsm.view(fn)
	})
}

// peek runs fn in a read transaction on the state as it is on this node now.
func (s *Store) peek(fn func(tx *bbolt.Tx) error) error {
	return s.use(func() error { return s.fsm.view(fn) })
}

// propose commits one entry through Raft and returns the state machine's
// result for it.
//
// An error means one of two things, and callers must tell them apart. A plain
// error is a definite "not committed": the entry was refused by the state
// machine or never reached the log. An error wrapping domain.ErrOutcomeUnknown
// means the entry may be in the log and may still take effect, now or after a
// restart; nothing it refers to may be cleaned up.
func (s *Store) propose(ctx context.Context, e entry) (applyResult, error) {
	var result applyResult
	err := s.use(func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Fail-stop: once the state machine cannot apply an entry, no
		// further entry is logged. Entries logged behind one that failed
		// would be reported as errors now and silently applied at the next
		// start.
		if err := s.fsm.health(); err != nil {
			return err
		}
		version := s.versionOf(e)
		if level, err := s.entryVersion(); err != nil {
			return err
		} else if version > level {
			return fmt.Errorf("%w: the cluster is being upgraded: this change needs metadata version %d, "+
				"and a node that only understands version %d is still a member", domain.ErrUnavailable, version, level)
		}
		data, err := encodeEntry(e, version)
		if err != nil {
			return err
		}
		return s.onLeader(ctx,
			func() (err error) {
				result, err = s.apply(ctx, data)
				return err
			},
			func(leaderID string) error {
				raw, err := s.leader.Propose(ctx, leaderID, data)
				if err != nil {
					return err
				}
				if result, err = decodeResult(raw); err != nil {
					// The leader applied the entry; what it answered is lost.
					return fmt.Errorf("%w: %w", domain.ErrOutcomeUnknown, err)
				}
				return result.err
			})
	})
	return result, err
}

// apply commits an encoded entry on this node, which must be the leader.
func (s *Store) apply(ctx context.Context, data []byte) (applyResult, error) {
	wait := defaultApplyWait
	if deadline, ok := ctx.Deadline(); ok {
		wait = max(time.Until(deadline), time.Millisecond)
	}
	future := s.raft.Apply(data, wait)
	if err := future.Error(); err != nil {
		return applyResult{}, translateRaftError(err)
	}
	applied, ok := future.Response().(applyResult)
	if !ok {
		return applyResult{}, fmt.Errorf("%w: unexpected state machine response", domain.ErrOutcomeUnknown)
	}
	if applied.fatal {
		// The entry is in the log but was not applied; it will be
		// applied when the cause is gone and the node restarts.
		return applyResult{}, fmt.Errorf("%w: %w", domain.ErrOutcomeUnknown, applied.err)
	}
	return applied, applied.err
}

// translateRaftError maps a Raft error to a domain error. Only the errors
// Raft returns before an entry is written to the log are definite; for every
// other error the entry may be logged, so the outcome is unknown.
func translateRaftError(err error) error {
	switch {
	case errors.Is(err, raft.ErrNotLeader), errors.Is(err, raft.ErrLeadershipTransferInProgress):
		return fmt.Errorf("%w: %w", domain.ErrNotLeader, err)
	case errors.Is(err, raft.ErrEnqueueTimeout):
		return fmt.Errorf("%w: metadata group is overloaded: %w", domain.ErrNotLeader, err)
	case errors.Is(err, raft.ErrRaftShutdown):
		return fmt.Errorf("%w: %w: %w", domain.ErrOutcomeUnknown, domain.ErrShuttingDown, err)
	case errors.Is(err, raft.ErrLeadershipLost):
		return fmt.Errorf("%w: %w: %w", domain.ErrOutcomeUnknown, domain.ErrNotLeader, err)
	default:
		return fmt.Errorf("%w: %w", domain.ErrOutcomeUnknown, err)
	}
}

// CreateBucket implements ports.BucketStore.
func (s *Store) CreateBucket(ctx context.Context, b bucket.Bucket) error {
	if _, err := s.propose(ctx, entry{Type: cmdCreateBucket, CreateBucket: bucketToRecord(b)}); err != nil {
		return fmt.Errorf("raftmeta.CreateBucket: %w", err)
	}
	return nil
}

// DeleteBucket implements ports.BucketStore.
func (s *Store) DeleteBucket(ctx context.Context, name string) error {
	if _, err := s.propose(ctx, entry{Type: cmdDeleteBucket, DeleteBucket: &deleteBucketCmd{Name: name}}); err != nil {
		return fmt.Errorf("raftmeta.DeleteBucket: %w", err)
	}
	return nil
}

// GetBucket implements ports.BucketStore.
func (s *Store) GetBucket(ctx context.Context, name string) (bucket.Bucket, error) {
	if err := ctx.Err(); err != nil {
		return bucket.Bucket{}, fmt.Errorf("raftmeta.GetBucket: %w", err)
	}
	var record bucketRecord
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bucketsBucket()).Get([]byte(name))
		if raw == nil {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, name)
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return fmt.Errorf("%w: stored bucket: %w", domain.ErrIntegrity, err)
		}
		return nil
	})
	if err != nil {
		return bucket.Bucket{}, fmt.Errorf("raftmeta.GetBucket: %w", err)
	}
	return record.toDomain(), nil
}

// ListBuckets implements ports.BucketStore.
func (s *Store) ListBuckets(ctx context.Context) ([]bucket.Bucket, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("raftmeta.ListBuckets: %w", err)
	}
	var out []bucket.Bucket
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketsBucket()).ForEach(func(_, raw []byte) error {
			var record bucketRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return fmt.Errorf("%w: stored bucket: %w", domain.ErrIntegrity, err)
			}
			out = append(out, record.toDomain())
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.ListBuckets: %w", err)
	}
	return out, nil
}

// PutVersion implements ports.ObjectIndex.
func (s *Store) PutVersion(ctx context.Context, v object.Version) (*object.Version, error) {
	result, err := s.propose(ctx, entry{Type: cmdPutObject, PutObject: versionToRecord(v)})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.PutVersion: %w", err)
	}
	return previousVersion(result)
}

// DeleteObject implements ports.ObjectIndex.
func (s *Store) DeleteObject(ctx context.Context, bucketName, key string) (*object.Version, error) {
	cmd := &deleteObjectCmd{Bucket: bucketName, Key: key}
	result, err := s.propose(ctx, entry{Type: cmdDeleteObject, DeleteObject: cmd})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.DeleteObject: %w", err)
	}
	return previousVersion(result)
}

func previousVersion(result applyResult) (*object.Version, error) {
	if result.previous == nil {
		return nil, nil
	}
	v, err := result.previous.toDomain()
	if err != nil {
		return nil, fmt.Errorf("%w: stored object version: %w", domain.ErrIntegrity, err)
	}
	return &v, nil
}

// GetObject implements ports.ObjectIndex.
func (s *Store) GetObject(ctx context.Context, bucketName, key string) (object.Version, error) {
	if err := ctx.Err(); err != nil {
		return object.Version{}, fmt.Errorf("raftmeta.GetObject: %w", err)
	}
	var version object.Version
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		index := tx.Bucket(objectsBucket()).Bucket([]byte(bucketName))
		if index == nil {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
		}
		record, err := readVersion(index, key)
		if err != nil {
			return err
		}
		if record == nil {
			return fmt.Errorf("%w: %s/%s", domain.ErrObjectNotFound, bucketName, key)
		}
		version, err = record.toDomain()
		if err != nil {
			return fmt.Errorf("%w: stored object version: %w", domain.ErrIntegrity, err)
		}
		return nil
	})
	if err != nil {
		return object.Version{}, fmt.Errorf("raftmeta.GetObject: %w", err)
	}
	return version, nil
}

// ScanObjects implements ports.ObjectIndex. The whole scan sees one
// consistent state.
func (s *Store) ScanObjects(
	ctx context.Context, bucketName, start string, visit func(object.Version) ports.ScanControl,
) error {
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		index := tx.Bucket(objectsBucket()).Bucket([]byte(bucketName))
		if index == nil {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, bucketName)
		}
		cursor := index.Cursor()
		key, raw := cursor.Seek([]byte(start))
		for key != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			var record versionRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return fmt.Errorf("%w: stored object version: %w", domain.ErrIntegrity, err)
			}
			version, err := record.toDomain()
			if err != nil {
				return fmt.Errorf("%w: stored object version: %w", domain.ErrIntegrity, err)
			}
			control := visit(version)
			switch {
			case control.Stop:
				return nil
			case control.SeekTo > string(key):
				key, raw = cursor.Seek([]byte(control.SeekTo))
			default:
				key, raw = cursor.Next()
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("raftmeta.ScanObjects: %w", err)
	}
	return nil
}

// GetRootCredential implements ports.RootCredentialStore.
func (s *Store) GetRootCredential(ctx context.Context) (iam.RootCredential, error) {
	if err := ctx.Err(); err != nil {
		return iam.RootCredential{}, fmt.Errorf("raftmeta.GetRootCredential: %w", err)
	}
	var record rootRecord
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		raw := tx.Bucket(iamBucket()).Get(rootKey())
		if raw == nil {
			return domain.ErrNotBootstrapped
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return fmt.Errorf("%w: stored root credential: %w", domain.ErrIntegrity, err)
		}
		return nil
	})
	if err != nil {
		return iam.RootCredential{}, fmt.Errorf("raftmeta.GetRootCredential: %w", err)
	}
	return record.toDomain(), nil
}

// SetRootCredential implements ports.RootCredentialStore.
func (s *Store) SetRootCredential(ctx context.Context, c iam.RootCredential) error {
	if _, err := s.propose(ctx, entry{Type: cmdSetRoot, SetRoot: rootToRecord(c)}); err != nil {
		return fmt.Errorf("raftmeta.SetRootCredential: %w", err)
	}
	return nil
}

// IsDataReferenced implements ports.ObjectIndex.
func (s *Store) IsDataReferenced(ctx context.Context, dataID object.ID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("raftmeta.IsDataReferenced: %w", err)
	}
	var referenced bool
	err := s.use(func() error {
		// A stopped state machine has entries in the log that it has not
		// applied. Their data looks unreferenced now and will be referenced
		// after a restart, so no answer is given at all.
		if err := s.fsm.health(); err != nil {
			return err
		}
		// The answer must include every version committed before the
		// question was asked, on whichever node committed it.
		if err := s.consistent(ctx); err != nil {
			return err
		}
		return s.fsm.view(func(tx *bbolt.Tx) error {
			referenced = tx.Bucket(dataBucket()).Get(dataID[:]) != nil
			return nil
		})
	})
	if err != nil {
		return false, fmt.Errorf("raftmeta.IsDataReferenced: %w", err)
	}
	return referenced, nil
}

// GetTopology implements ports.TopologyStore.
func (s *Store) GetTopology(ctx context.Context) (cluster.Topology, error) {
	if err := ctx.Err(); err != nil {
		return cluster.Topology{}, fmt.Errorf("raftmeta.GetTopology: %w", err)
	}
	var topology cluster.Topology
	err := s.view(ctx, func(tx *bbolt.Tx) (err error) {
		topology, err = readTopology(tx)
		return err
	})
	if err != nil {
		return cluster.Topology{}, fmt.Errorf("raftmeta.GetTopology: %w", err)
	}
	return topology, nil
}

// SetTopology implements ports.TopologyStore.
func (s *Store) SetTopology(ctx context.Context, t cluster.Topology) error {
	if _, err := s.propose(ctx, entry{Type: cmdSetTopology, SetTopology: topologyToRecord(t)}); err != nil {
		return fmt.Errorf("raftmeta.SetTopology: %w", err)
	}
	return nil
}
