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

	dirMode           = 0o700
	lockTimeout       = 2 * time.Second
	retainedSnapshots = 2
	nodeIDSize        = 8
	defaultApplyWait  = 10 * time.Second
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
}

// Timing holds the Raft timeouts a caller may override.
type Timing struct {
	Heartbeat   time.Duration
	Election    time.Duration
	LeaderLease time.Duration
	Commit      time.Duration
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
}

var (
	_ ports.BucketStore         = (*Store)(nil)
	_ ports.ObjectIndex         = (*Store)(nil)
	_ ports.RootCredentialStore = (*Store)(nil)
	_ ports.TopologyStore       = (*Store)(nil)
	_ ports.ReadinessCheck      = (*Store)(nil)
)

// Open opens the metadata store in cfg.Dir, creating a single-voter Raft
// group on first use, and waits until this node leads it or ctx ends. The
// caller must Close the store.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if err := os.MkdirAll(cfg.Dir, dirMode); err != nil {
		return nil, fmt.Errorf("raftmeta.Open: %w", err)
	}
	root, err := os.OpenRoot(cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("raftmeta.Open: %w", err)
	}
	s := &Store{root: root}
	if err := s.start(cfg); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("raftmeta.Open: %s: %w", cfg.Dir, err)
	}
	if err := s.waitForLeadership(ctx); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("raftmeta.Open: %w", err)
	}
	return s, nil
}

func (s *Store) start(cfg Config) error {
	var err error
	if s.nodeID, err = loadOrCreateNodeID(s.root, cfg.Entropy); err != nil {
		return err
	}
	if s.fsm, err = openFSM(s.root); err != nil {
		return err
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
	if t := cfg.Timing; t.Heartbeat > 0 {
		conf.HeartbeatTimeout, conf.ElectionTimeout = t.Heartbeat, t.Election
		conf.LeaderLeaseTimeout, conf.CommitTimeout = t.LeaderLease, t.Commit
	}

	// One voter needs no network. The address is fixed because Raft stores
	// it in its configuration; node RPC over mTLS replaces this transport
	// when clusters arrive (PLAN 3.3).
	address := raft.ServerAddress(s.nodeID)
	_, transport := raft.NewInmemTransport(address)

	existing, err := raft.HasExistingState(s.logs, s.logs, snapshots)
	if err != nil {
		return fmt.Errorf("inspect raft state: %w", err)
	}
	if !existing {
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

func loadOrCreateNodeID(root *os.Root, entropy io.Reader) (string, error) {
	data, err := root.ReadFile(nodeFile)
	if err == nil {
		var info nodeInfo
		if err := json.Unmarshal(data, &info); err != nil || info.Magic != nodeMagic || info.NodeID == "" {
			return "", fmt.Errorf("%w: %s is not a node file", domain.ErrIntegrity, nodeFile)
		}
		if info.FormatVersion != nodeVersion {
			return "", fmt.Errorf("%w: node file version %d", domain.ErrUnsupportedFormat, info.FormatVersion)
		}
		return info.NodeID, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	id := make([]byte, nodeIDSize)
	if _, err := io.ReadFull(entropy, id); err != nil {
		return "", fmt.Errorf("generate node ID: %w", err)
	}
	info := nodeInfo{Magic: nodeMagic, FormatVersion: nodeVersion, NodeID: "node-" + hex.EncodeToString(id)}
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
	if s.raft != nil {
		errs = append(errs, s.raft.Shutdown().Error())
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

// Check implements ports.ReadinessCheck: the node is ready when it leads the
// metadata group and its state machine is healthy.
func (s *Store) Check(context.Context) error {
	return s.use(func() error {
		if state := s.raft.State(); state != raft.Leader {
			return fmt.Errorf("%w: raft state is %s", domain.ErrNotLeader, state)
		}
		return s.fsm.health()
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

// view runs fn in a read transaction on the state.
func (s *Store) view(fn func(tx *bbolt.Tx) error) error {
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
		data, err := encodeEntry(e)
		if err != nil {
			return err
		}
		wait := defaultApplyWait
		if deadline, ok := ctx.Deadline(); ok {
			wait = max(time.Until(deadline), time.Millisecond)
		}
		future := s.raft.Apply(data, wait)
		if err := future.Error(); err != nil {
			return translateRaftError(err)
		}
		applied, ok := future.Response().(applyResult)
		if !ok {
			return fmt.Errorf("%w: unexpected state machine response", domain.ErrOutcomeUnknown)
		}
		if applied.fatal {
			// The entry is in the log but was not applied; it will be
			// applied when the cause is gone and the node restarts.
			return fmt.Errorf("%w: %w", domain.ErrOutcomeUnknown, applied.err)
		}
		result = applied
		return applied.err
	})
	return result, err
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
	err := s.view(func(tx *bbolt.Tx) error {
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
	err := s.view(func(tx *bbolt.Tx) error {
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
	err := s.view(func(tx *bbolt.Tx) error {
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
	err := s.view(func(tx *bbolt.Tx) error {
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
	err := s.view(func(tx *bbolt.Tx) error {
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
	var record topologyRecord
	err := s.view(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(clusterBucket()).Get(topologyKey())
		if raw == nil {
			return domain.ErrNoTopology
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return fmt.Errorf("%w: stored topology: %w", domain.ErrIntegrity, err)
		}
		return nil
	})
	if err != nil {
		return cluster.Topology{}, fmt.Errorf("raftmeta.GetTopology: %w", err)
	}
	return record.toDomain(), nil
}

// SetTopology implements ports.TopologyStore.
func (s *Store) SetTopology(ctx context.Context, t cluster.Topology) error {
	if _, err := s.propose(ctx, entry{Type: cmdSetTopology, SetTopology: topologyToRecord(t)}); err != nil {
		return fmt.Errorf("raftmeta.SetTopology: %w", err)
	}
	return nil
}
