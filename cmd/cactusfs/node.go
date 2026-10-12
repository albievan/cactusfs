package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	auditexport "github.com/albievan/cactusfs/internal/adapters/audit"
	"github.com/albievan/cactusfs/internal/adapters/disk"
	"github.com/albievan/cactusfs/internal/adapters/erasurecodec"
	"github.com/albievan/cactusfs/internal/adapters/events/inproc"
	"github.com/albievan/cactusfs/internal/adapters/kms/local"
	"github.com/albievan/cactusfs/internal/adapters/kms/vault"
	"github.com/albievan/cactusfs/internal/adapters/noderpc"
	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/internal/adapters/passwords"
	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/erasure"
	"github.com/albievan/cactusfs/internal/ports"
)

const (
	metadataStartTimeout = 30 * time.Second
	maxPassphraseFile    = 4 << 10
)

// systemClock is the real clock.
type systemClock struct{}

// Now implements ports.Clock.
func (systemClock) Now() time.Time { return time.Now() }

// node holds the adapters and use cases of a running node. It is assembled
// here, in the composition root, and nowhere else.
type node struct {
	drives []*disk.Drive
	meta   *raftmeta.Store
	// id is the node's name; roles are its jobs.
	id    string
	roles cluster.Roles
	// The fields below are set on a member of a cluster only.
	identity  *nodetls.Identity
	rpc       *noderpc.Client
	rpcServer *noderpc.Server
	rpcDone   chan error
	events    *inproc.Bus
	// publisher is the bus as the metadata store publishes to it: through
	// the decorator that counts what could not be delivered.
	publisher ports.EventPublisher
	// auditOut is where exported audit records are written; nil when they
	// are not exported.
	auditOut io.Writer
	// inFlight answers other nodes' collectors; it is set once the object
	// service exists. started is the metadata store as node RPC sees it:
	// node RPC serves before the store is open, and from other goroutines.
	inFlight atomic.Pointer[app.ObjectService]
	started  atomic.Pointer[raftmeta.Store]

	buckets    *app.BucketService
	objects    *app.ObjectService
	search     *app.SearchService
	identities *app.IdentityService
	authorizer *app.Authorizer
	auditor    *app.Auditor
	iam        *app.IAMService
	sessions   *app.SessionService
	// cluster, idempotency and maintenance serve the management API.
	cluster     *app.ClusterService
	idempotency *app.IdempotencyService
	maintenance *app.Maintenance
	clock       ports.Clock
	telemetry   *telemetry.Telemetry

	// access counts who reads which object, on its way to the metadata
	// store.
	access    *app.AccessRecorder
	scrubber  *app.Scrubber
	lifecycle *app.LifecycleWorker
	collector *app.Collector
	// stopBackground cancels the background tasks; background is done when
	// they have returned.
	stopBackground context.CancelFunc
	background     sync.WaitGroup
}

// openNode opens the drives, the KMS and the metadata store, wires the use
// cases and makes sure the deployment has a root credential. Credentials
// generated for a new deployment are shown once on notice, which is the
// operator's terminal and never the log.
func openNode(
	ctx context.Context, cfg serverConfig, tel *telemetry.Telemetry, logger *slog.Logger, notice io.Writer,
) (*node, error) {
	n := &node{clock: systemClock{}, telemetry: tel}
	if err := n.open(ctx, cfg, logger, notice); err != nil {
		return nil, errors.Join(err, n.Close())
	}
	return n, nil
}

func (n *node) open(ctx context.Context, cfg serverConfig, logger *slog.Logger, notice io.Writer) error {
	n.roles = cfg.Roles
	n.auditOut = cfg.auditOut
	// The one authorization decision point. Node RPC needs it first.
	// Refusals are audited from the first request on; the trail they are
	// written to becomes reachable when the metadata store is open.
	n.auditor = app.NewAuditor(n.telemetry.InstrumentAuditLog(lateAuditLog{node: n}), n.clock, logger)
	n.authorizer = app.NewAuthorizer(logger, n.clock, n.auditor)
	stores := make([]ports.ShardStore, 0, len(cfg.Data))
	attached := make([]app.AttachedDrive, 0, len(cfg.Data))
	for _, dir := range cfg.Data {
		drive, err := disk.Open(ctx, dir, rand.Reader, n.clock.Now())
		if err != nil {
			return err
		}
		n.drives = append(n.drives, drive)
		stores = append(stores, n.telemetry.InstrumentShardStore(drive))
		attached = append(attached, drive)
		logger.InfoContext(ctx, "drive opened",
			slog.String("drive_id", drive.DriveID()), slog.String("dir", dir), slog.Bool("empty", drive.Empty()))
	}

	opened, err := openKMS(ctx, cfg, logger)
	if err != nil {
		return err
	}
	kms := n.telemetry.InstrumentKMS(opened)

	// Events are derived from applied metadata entries and delivered inside
	// the process.
	n.events = inproc.New(logger)
	n.events.Subscribe("telemetry", inproc.Deduplicate(n.telemetry.ObserveEvent))
	n.publisher = n.telemetry.InstrumentEventPublisher(n.events)

	if err := n.openMetadata(ctx, cfg, logger); err != nil {
		return err
	}
	n.id = n.meta.NodeID()
	// Every outbound port is reached through a decorator that traces and
	// counts the call: a use case is never handed n.meta itself. A test
	// walks the node and fails on a port that is (see wiring_test.go).
	meta := n.telemetry.InstrumentMetadata(n.meta)
	identities := n.telemetry.InstrumentIAM(n.meta)
	trail := n.telemetry.InstrumentAuditLog(n.meta)
	view := n.telemetry.InstrumentClusterView(n.meta.Local())
	// How the node is, read when its metrics are scraped.
	n.telemetry.ObserveRaft(raftGauges{store: n.meta})
	n.telemetry.ObserveDrives(driveGauges(n.drives))
	n.authorizer.WithBucketPolicies(meta)

	if cfg.clustered() {
		if err := n.joinCluster(ctx, cfg, logger); err != nil {
			return err
		}
	}
	topology, err := n.ensureTopology(ctx, cfg, meta, attached, logger)
	if err != nil {
		return err
	}
	params, err := erasure.ParamsFor(topology.SetSize())
	if err != nil {
		return err
	}
	if params.Parity == 0 {
		logger.WarnContext(ctx, "fewer than four drives: objects are stored encrypted and checksummed on one drive each, "+
			"WITHOUT PARITY; the loss of a drive loses the objects on it", slog.Int("drives", len(stores)))
	} else {
		logger.InfoContext(ctx, "erasure coding enabled",
			slog.Int("erasure_sets", len(topology.Sets)), slog.Int("data_shards", params.Data),
			slog.Int("parity_shards", params.Parity), slog.Int("write_quorum", params.WriteQuorum()),
			slog.Int("nodes", len(topology.NodeIDs())), slog.Int("node_failures_tolerated", topology.NodeFailuresTolerated()))
	}
	if cfg.clustered() && topology.NodeFailuresTolerated() == 0 {
		logger.WarnContext(ctx, "the drives of this cluster are spread so that the loss of ONE NODE can make objects unreadable")
	}

	n.maintenance = app.NewMaintenance(n.id, n.clock)
	drives := n.driveSource(view, stores)
	// The list of buckets with details decides every bucket from one walk
	// of the policies in this node's state, as a search does, and reads the
	// latest use of the buckets in one call (ADR-0019).
	bucketPolicies := n.telemetry.InstrumentBucketPolicies(n.meta.LocalConfig())
	n.buckets = app.NewBucketService(meta, n.authorizer, n.clock).WithAuditor(n.auditor).WithLocalConfig(n.telemetry.InstrumentLocalConfig(n.meta.LocalConfig())).
		WithDetails(bucketPolicies, n.telemetry.InstrumentBucketActivity(n.meta))
	// Who read what: counted in memory and handed to the metadata store in
	// the background, by this node under its own name (ADR-0016).
	accessStore := n.telemetry.InstrumentObjectAccess(n.meta)
	n.access, err = app.NewAccessRecorder(app.AccessRecorderConfig{
		Store: accessStore, Node: n.id, Entropy: rand.Reader, Logger: logger,
		OnDrop: n.telemetry.ObserveAccessDropped,
		// Nothing is handed over while the metadata group has no leader.
		Ready: n.meta.Check,
	})
	if err != nil {
		return err
	}
	n.objects, err = app.NewObjectService(app.ObjectServiceConfig{
		Index: meta, Buckets: meta, Uploads: meta, Drives: drives, Codec: erasurecodec.New(), KMS: kms,
		OnHeal: func(r app.HealResult) {
			n.telemetry.ObserveHeal(r)
			n.maintenance.ObserveHeal(r)
		},
		Authz: n.authorizer, Clock: n.clock, Entropy: rand.Reader, Logger: logger,
		Auditor: n.auditor, Access: n.access, AccessStore: accessStore,
	})
	if err != nil {
		return err
	}
	n.inFlight.Store(n.objects)
	// Search reads the same instrumented stores as a listing, behind the
	// same authorizer. The policies of the buckets it filters by are read
	// from this node's state page by page, after the consistent listing of
	// the buckets: one question to the leader a page of a search, not one a
	// bucket. The authorizer remembers what they say from one search to the
	// next.
	n.search, err = app.NewSearchService(app.SearchServiceConfig{
		Buckets: meta, Index: meta, Policies: bucketPolicies,
		Authz: n.authorizer, Clock: n.clock, Logger: logger,
	})
	if err != nil {
		return err
	}
	n.identities = app.NewIdentityService(meta, kms, n.clock, rand.Reader).WithIAM(identities, n.auditor)
	cost := cfg.passwordParams
	if cost == (passwords.Params{}) {
		cost = passwords.DefaultParams()
	}
	argon, err := passwords.New(cost, rand.Reader)
	if err != nil {
		return err
	}
	hasher := n.telemetry.InstrumentPasswords(argon)
	n.iam = app.NewIAMService(app.IAMServiceConfig{
		Store: identities, Trail: trail, Authz: n.authorizer, Auditor: n.auditor, KMS: kms, Hasher: hasher,
		Clock: n.clock, Entropy: rand.Reader,
	})
	provider, err := app.NewLocalIdentityProvider(identities, hasher, rand.Reader)
	if err != nil {
		return err
	}
	n.sessions = app.NewSessionService(app.SessionServiceConfig{
		Sessions: n.telemetry.InstrumentSessions(n.meta), Users: identities,
		Provider: n.telemetry.InstrumentIdentityProvider(provider), Auditor: n.auditor, Clock: n.clock, Entropy: rand.Reader,
	})
	n.idempotency = app.NewIdempotencyService(n.telemetry.InstrumentIdempotency(n.meta), kms, n.clock, rand.Reader)
	n.cluster = app.NewClusterService(app.ClusterServiceConfig{
		View: view, Authz: n.authorizer, NodeID: n.id, Leader: n.meta.Leader, Maintenance: n.maintenance,
		Probe: func(ctx context.Context, slot cluster.Slot) bool { return driveAnswers(ctx, drives, slot) },
	})
	if cfg.AuditExport && n.auditOut != nil {
		n.events.Subscribe("audit-export", inproc.Deduplicate(auditexport.NewExporter(trail, n.auditOut, logger).Handle))
	}
	n.scrubber = app.NewScrubber(app.ScrubberConfig{
		Buckets: meta, Index: meta, Uploads: meta, Healer: n.objects.Healer(), Logger: logger,
		BytesPerSecond: int64(cfg.ScrubRateMiB) << 20,
		OnPass: func(s app.ScrubStats) {
			n.telemetry.ObserveScrub(s)
			n.maintenance.ObserveScrub(s)
		},
		// One node scrubs at a time: the one that leads the metadata group.
		Active: n.meta.IsLeader,
	})
	n.lifecycle = app.NewLifecycleWorker(app.LifecycleWorkerConfig{
		Buckets: meta, Index: meta, Uploads: meta, Objects: n.objects, Clock: n.clock, Logger: logger,
		OnPass: n.telemetry.ObserveLifecycle,
		// One node carries out rules at a time, as one node scrubs.
		Active: n.meta.IsLeader,
	})
	n.collector = app.NewCollector(app.CollectorConfig{
		// Every node collects on its own drives.
		Drives: stores, Index: meta, Clock: n.clock, Grace: cfg.GCGrace, Logger: logger,
		OnPass: func(s app.GCStats) {
			n.telemetry.ObserveGC(s)
			n.maintenance.ObserveGC(s)
		},
		// Nothing is collected while the metadata cannot be trusted, nor
		// what a write in progress on any node has put on the drives.
		Ready: n.meta.Check, InFlight: n.inFlightAnywhere,
	})

	var provided *app.Credentials
	if cfg.secrets.rootAccessKey != "" {
		provided = &app.Credentials{AccessKeyID: cfg.secrets.rootAccessKey, SecretKey: cfg.secrets.rootSecretKey}
	}
	generated, err := n.identities.EnsureRoot(ctx, provided)
	if err != nil {
		return err
	}
	if generated != nil {
		logger.InfoContext(ctx, "root credentials generated for a new deployment; they are shown once on standard error")
		showCredentials(notice, *generated)
	}
	// A new deployment begins with a console user whose name and password
	// are public (ADR-0014). Said loudly, once.
	if n.identities.CreatedRoot() {
		created, err := n.iam.EnsureDefaultAdmin(ctx)
		if err != nil {
			return err
		}
		if created {
			logger.WarnContext(ctx, "console user created with the default password; delete the user or change the password before the console can be reached by anybody else",
				"user", app.DefaultAdminName)
			_, _ = fmt.Fprintf(notice, "\nThis new deployment has the console user %q with the default password (ADR-0014).\nDelete that user, or change its password, before anybody else can reach the admin port.\n\n", app.DefaultAdminName)
		}
	}
	return nil
}

// openMetadata opens the metadata store: a Raft group of this node alone,
// or, for a member of a cluster, a store that waits to be part of one.
func (n *node) openMetadata(ctx context.Context, cfg serverConfig, logger *slog.Logger) (err error) {
	config := raftmeta.Config{
		Dir: cfg.MetaDir, Logger: logger, Entropy: rand.Reader, Timing: cfg.raftTiming, NodeID: cfg.NodeID, Events: n.publisher,
		Clock: n.clock,
	}
	if cfg.clustered() {
		if config.Cluster, err = n.openCluster(ctx, cfg, logger); err != nil {
			return err
		}
	}
	startCtx, cancel := context.WithTimeout(ctx, metadataStartTimeout)
	defer cancel()
	if n.meta, err = raftmeta.Open(startCtx, config); err != nil {
		return err
	}
	n.started.Store(n.meta)
	return nil
}

// ensureTopology returns the deployment's topology, once this node's drives
// agree with it. On a member of a new cluster that means waiting until the
// leader has recorded the topology, which it can only when it has heard from
// every node.
func (n *node) ensureTopology(
	ctx context.Context, cfg serverConfig, meta ports.TopologyStore, attached []app.AttachedDrive, logger *slog.Logger,
) (cluster.Topology, error) {
	opts := app.TopologyOptions{
		AllowReplacement: cfg.AllowDriveReplacement, NodeID: n.id, Labels: cfg.Labels, Entropy: rand.Reader, Logger: logger,
		Auditor: n.auditor,
	}
	if !cfg.clustered() {
		return app.EnsureTopology(ctx, meta, attached, opts)
	}
	opts.Survey = func(ctx context.Context) ([]cluster.DriveState, map[string]cluster.Labels, error) {
		return n.surveyDrives(ctx, cfg)
	}
	waiting := newProgress(logger, "waiting for the drive topology of the cluster")
	for {
		topology, err := app.EnsureTopology(ctx, meta, attached, opts)
		if err == nil || !transient(err) {
			return topology, err
		}
		if err := waiting.pause(ctx, err); err != nil {
			return cluster.Topology{}, err
		}
	}
}

// lateAuditLog is the audit trail as the auditor sees it from the first
// moment of the node: unavailable until the metadata store is open.
type lateAuditLog struct {
	node *node
}

func (l lateAuditLog) store() (*raftmeta.Store, error) {
	meta := l.node.started.Load()
	if meta == nil {
		return nil, fmt.Errorf("%w: the node is starting", domain.ErrUnavailable)
	}
	return meta, nil
}

// AppendAudit implements ports.AuditLog.
func (l lateAuditLog) AppendAudit(ctx context.Context, record audit.Record) error {
	meta, err := l.store()
	if err != nil {
		return err
	}
	return meta.AppendAudit(ctx, record)
}

// ListAudit implements ports.AuditLog.
func (l lateAuditLog) ListAudit(ctx context.Context, after uint64, limit int) ([]audit.Record, error) {
	meta, err := l.store()
	if err != nil {
		return nil, err
	}
	return meta.ListAudit(ctx, after, limit)
}

// sessionSweepInterval is how often ended console sessions are removed.
const sessionSweepInterval = 10 * time.Minute

// errProbed ends the listing a drive is probed with at its first entry.
var errProbed = errors.New("drive answered")

// driveAnswers reports whether the drive in a slot can be reached: it is
// asked for the first shard it holds.
func driveAnswers(ctx context.Context, drives ports.DriveSource, slot cluster.Slot) bool {
	drive, ok := drives.Drive(slot)
	if !ok {
		return false
	}
	err := drive.List(ctx, func(ports.ShardInfo) error { return errProbed })
	return err == nil || errors.Is(err, errProbed)
}

// sweepSessions removes ended sessions, and the answers kept for repeated
// requests that are old, now and then, on the node that leads the metadata
// group.
func (n *node) sweepSessions(ctx context.Context) {
	ticker := time.NewTicker(sessionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n.meta.IsLeader() {
				_, _ = n.sessions.Sweep(ctx)
				_, _ = n.idempotency.Sweep(ctx)
			}
		}
	}
}

// driveSource finds drives for the object service: this node's by their ID,
// those of other nodes through node RPC.
func (n *node) driveSource(view ports.ClusterView, local []ports.ShardStore) ports.DriveSource {
	source := &driveSource{node: n, view: view, local: map[string]ports.ShardStore{}, remote: map[string]ports.ShardStore{}}
	for _, drive := range local {
		source.local[drive.DriveID()] = drive
	}
	return source
}

type driveSource struct {
	node  *node
	view  ports.ClusterView
	local map[string]ports.ShardStore

	mu     sync.Mutex
	remote map[string]ports.ShardStore
}

// Topology implements ports.DriveSource.
func (d *driveSource) Topology(ctx context.Context) (cluster.Topology, error) {
	return d.view.Topology(ctx)
}

// Drive implements ports.DriveSource.
func (d *driveSource) Drive(slot cluster.Slot) (ports.ShardStore, bool) {
	if slot.NodeID == d.node.id {
		drive, ok := d.local[slot.DriveID]
		return drive, ok
	}
	if d.node.rpc == nil {
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	key := slot.NodeID + "/" + slot.DriveID
	drive, ok := d.remote[key]
	if !ok {
		drive = d.node.telemetry.InstrumentShardStore(d.node.rpc.Drive(slot.NodeID, slot.DriveID))
		d.remote[key] = drive
	}
	return drive, true
}

// showCredentials prints generated root credentials. This is the only place
// a secret is ever written, and it is written once.
func showCredentials(w io.Writer, creds app.Credentials) {
	_, _ = fmt.Fprintf(w, `
CactusFS generated root credentials for this new deployment.
They are shown once and cannot be recovered. Store them now.

  Access key ID: %s
  Secret key:    %s

To choose the credentials yourself, set %s and %s
before the first start.

`, creds.AccessKeyID, creds.SecretKey, envRootKeyID, envRootKeyValue)
}

// openKMS opens the configured KMS. The key file of the local KMS is created
// only for a deployment that has no metadata yet: once objects exist, a
// missing key file means the wrong path was given, and creating a new key
// there would leave every object unreadable while looking healthy.
func openKMS(ctx context.Context, cfg serverConfig, logger *slog.Logger) (ports.KMS, error) {
	if cfg.KMS == kmsVault {
		kms, err := vault.Open(ctx, vault.Config{
			Address: cfg.VaultAddr, Mount: cfg.VaultMount, Key: cfg.VaultKey, TokenFile: cfg.VaultTokenFile,
			Namespace: cfg.VaultNamespace, CACertFile: cfg.VaultCACert, ServerName: cfg.VaultServerName,
			AllowPlaintextLoopback: cfg.DevPlaintext, Entropy: rand.Reader,
		})
		if err != nil {
			return nil, err
		}
		logger.InfoContext(ctx, "KMS opened", slog.String("kms", kmsVault), slog.String("key_id", kms.KeyID()))
		return kms, nil
	}
	if cfg.clustered() {
		logger.WarnContext(ctx, "this cluster shares a sealed local KMS key file: a development arrangement; "+
			"production clusters keep the key in Vault (--kms vault)")
	}
	return openLocalKMS(ctx, cfg, logger)
}

func openLocalKMS(ctx context.Context, cfg serverConfig, logger *slog.Logger) (ports.KMS, error) {
	passphrase, err := readPassphrase(cfg)
	if err != nil {
		return nil, err
	}
	defer clear(passphrase)

	exists, err := local.Exists(cfg.KMSKeyFile)
	if err != nil {
		return nil, err
	}
	if !exists {
		started, err := metadataExists(cfg.MetaDir)
		if err != nil {
			return nil, err
		}
		if started {
			return nil, fmt.Errorf("KMS key file %s is missing, but this deployment already has metadata in %s: "+
				"point --kms-key-file at the original key file", cfg.KMSKeyFile, cfg.MetaDir)
		}
		if err := local.Create(cfg.KMSKeyFile, passphrase, local.DefaultParams(), rand.Reader); err != nil {
			return nil, err
		}
		logger.WarnContext(ctx, "created a new local KMS key file; back it up together with its passphrase: "+
			"without both, stored data cannot be decrypted", slog.String("key_file", cfg.KMSKeyFile))
	}
	kms, err := local.Open(cfg.KMSKeyFile, passphrase)
	if err != nil {
		return nil, err
	}
	logger.InfoContext(ctx, "KMS opened", slog.String("kms", kmsLocal), slog.String("key_id", kms.KeyID()))
	return kms, nil
}

// metadataExists reports whether the metadata directory has been used.
func metadataExists(dir string) (bool, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect metadata directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	switch _, err := root.Lstat("node.json"); {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("inspect metadata directory: %w", err)
	}
}

// readPassphrase returns the KMS passphrase from the environment or from the
// passphrase file. Surrounding whitespace of a file is not part of it.
func readPassphrase(cfg serverConfig) ([]byte, error) {
	if cfg.KMSPassphraseFile == "" {
		return []byte(cfg.secrets.kmsPassphrase), nil
	}
	root, err := os.OpenRoot(filepath.Dir(cfg.KMSPassphraseFile))
	if err != nil {
		return nil, fmt.Errorf("read KMS passphrase file: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(cfg.KMSPassphraseFile))
	if err != nil {
		return nil, fmt.Errorf("read KMS passphrase file: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxPassphraseFile+1))
	if err != nil {
		return nil, fmt.Errorf("read KMS passphrase file: %w", err)
	}
	if len(data) > maxPassphraseFile {
		return nil, errors.New("read KMS passphrase file: file is too large to be a passphrase")
	}
	passphrase := bytes.TrimSpace(data)
	if len(passphrase) == 0 {
		return nil, errors.New("read KMS passphrase file: file is empty")
	}
	return passphrase, nil
}

// startBackground starts healing, scrubbing, garbage collection and the
// recording of reads. They run until Close.
func (n *node) startBackground(ctx context.Context, cfg serverConfig) {
	ctx, n.stopBackground = context.WithCancel(context.WithoutCancel(ctx))
	n.background.Go(func() { n.objects.Healer().Run(ctx) })
	if cfg.ScrubInterval > 0 {
		n.background.Go(func() { n.scrubber.Run(ctx, cfg.ScrubInterval) })
	}
	if cfg.LifecycleInterval > 0 {
		n.background.Go(func() { n.lifecycle.Run(ctx, cfg.LifecycleInterval) })
	}
	if cfg.GCInterval > 0 && len(n.drives) > 0 {
		n.background.Go(func() { n.collector.Run(ctx, cfg.GCInterval) })
	}
	// Runs on every node that serves reads, and once more when it stops.
	n.background.Go(func() { n.access.Run(ctx, cfg.AccessFlushInterval) })
	n.background.Go(func() { n.events.Run(ctx) })
	n.background.Go(func() { n.auditor.Run(ctx) })
	n.background.Go(func() { n.identities.Run(ctx) })
	n.background.Go(func() { n.sweepSessions(ctx) })
}

// Close stops the background tasks, then the metadata store, and releases
// the drives.
func (n *node) Close() error {
	if n.stopBackground != nil {
		n.stopBackground()
		n.background.Wait()
		n.stopBackground = nil
	}
	// The gauges stop describing what is about to be closed.
	n.telemetry.ObserveRaft(nil)
	n.telemetry.ObserveDrives(nil)
	n.telemetry.ObserveNodeCertificate(nil)
	var errs []error
	if n.meta != nil {
		// Let the others carry on without waiting for an election timeout.
		n.meta.HandOver()
	}
	if n.rpcServer != nil {
		n.rpcServer.Stop(rpcStopTimeout)
		<-n.rpcDone
		n.rpcServer = nil
	}
	n.started.Store(nil)
	if n.meta != nil {
		errs = append(errs, n.meta.Close())
		n.meta = nil
	}
	if n.rpc != nil {
		errs = append(errs, n.rpc.Close())
		n.rpc = nil
	}
	for _, drive := range n.drives {
		errs = append(errs, drive.Close())
	}
	n.drives = nil
	return errors.Join(errs...)
}

// describe returns the drive IDs for the startup log.
func (n *node) describe() string {
	ids := make([]string, len(n.drives))
	for i, drive := range n.drives {
		ids[i] = drive.DriveID()
	}
	return strings.Join(ids, ",")
}
