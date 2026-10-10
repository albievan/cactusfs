package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/passwords"
	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// Environment variables read by "cactusfs server". Each has a matching flag,
// and a flag given on the command line wins over the variable.
const (
	envAdminAddr       = "CACTUSFS_ADMIN_ADDR"
	envS3Addr          = "CACTUSFS_S3_ADDR"
	envS3Domain        = "CACTUSFS_S3_DOMAIN"
	envRegion          = "CACTUSFS_REGION"
	envData            = "CACTUSFS_DATA"
	envMetaDir         = "CACTUSFS_META_DIR"
	envKMS             = "CACTUSFS_KMS"
	envKMSKeyFile      = "CACTUSFS_KMS_KEY_FILE"
	envKMSPhraseFile   = "CACTUSFS_KMS_PASSPHRASE_FILE"
	envVaultAddr       = "CACTUSFS_VAULT_ADDR"
	envVaultMount      = "CACTUSFS_VAULT_TRANSIT_MOUNT"
	envVaultKey        = "CACTUSFS_VAULT_KEY"
	envVaultAuthFile   = "CACTUSFS_VAULT_TOKEN_FILE"
	envVaultCACert     = "CACTUSFS_VAULT_CA_CERT"
	envVaultServerName = "CACTUSFS_VAULT_SERVER_NAME"
	envVaultNamespace  = "CACTUSFS_VAULT_NAMESPACE"
	envAuditExport     = "CACTUSFS_AUDIT_EXPORT"
	envTLSCert         = "CACTUSFS_TLS_CERT"
	envTLSKey          = "CACTUSFS_TLS_KEY"
	envDevPlaintext    = "CACTUSFS_DEV_PLAINTEXT"
	envLogLevel        = "CACTUSFS_LOG_LEVEL"
	envOTLPEndpoint    = "CACTUSFS_OTLP_ENDPOINT"
	envScrubInterval   = "CACTUSFS_SCRUB_INTERVAL"
	envScrubRate       = "CACTUSFS_SCRUB_RATE_MIB"
	envGCInterval      = "CACTUSFS_GC_INTERVAL"
	envLifecycle       = "CACTUSFS_LIFECYCLE_INTERVAL"
	envGCGrace         = "CACTUSFS_GC_GRACE"
	envAllowReplace    = "CACTUSFS_ALLOW_DRIVE_REPLACEMENT"
	envNodeID          = "CACTUSFS_NODE_ID"
	envRoles           = "CACTUSFS_ROLES"
	envPeers           = "CACTUSFS_PEERS"
	envRPCAddr         = "CACTUSFS_RPC_ADDR"
	envRaftAddr        = "CACTUSFS_RAFT_ADDR"
	envAdvertiseRPC    = "CACTUSFS_ADVERTISE_RPC"
	envAdvertiseRaft   = "CACTUSFS_ADVERTISE_RAFT"
	envNodeTLSCert     = "CACTUSFS_NODE_TLS_CERT"
	envNodeTLSKey      = "CACTUSFS_NODE_TLS_KEY"
	envNodeTLSCA       = "CACTUSFS_NODE_TLS_CA"
	envTopologyRegion  = "CACTUSFS_TOPOLOGY_REGION"
	envTopologyZone    = "CACTUSFS_TOPOLOGY_ZONE"
	envTopologyRack    = "CACTUSFS_TOPOLOGY_RACK"
	envTopologyHost    = "CACTUSFS_TOPOLOGY_HOST"
	envDrainDelay      = "CACTUSFS_DRAIN_DELAY"
	envShutdownTimeout = "CACTUSFS_SHUTDOWN_TIMEOUT"
)

// Secrets are read from the environment only. They have no flag, because a
// command line is visible to every user of the machine.
const (
	envKMSPhrase    = "CACTUSFS_KMS_PASSPHRASE"
	envRootKeyID    = "CACTUSFS_ROOT_ACCESS_KEY"
	envRootKeyValue = "CACTUSFS_ROOT_SECRET_KEY"
)

const (
	defaultAdminAddr       = ":9001"
	defaultS3Addr          = ":9000"
	defaultRPCAddr         = ":9002"
	defaultRaftAddr        = ":9003"
	defaultLogLevel        = "info"
	defaultShutdownTimeout = 30 * time.Second
	defaultScrubInterval   = 24 * time.Hour
	defaultScrubRateMiB    = 64
	defaultGCInterval      = time.Hour
	defaultLifecycle       = time.Hour
	defaultGCGrace         = 24 * time.Hour
	// minGCGrace is the shortest grace period accepted. Writes in progress
	// are protected from the collector by other means; the grace period is
	// what remains when those do not apply, so it is kept generous.
	minGCGrace     = time.Hour
	loopbackHost   = "127.0.0.1"
	metaDirName    = "meta"
	kmsKeyFileName = "kms-local.json"
	kmsLocal       = "local"
	kmsVault       = "vault"
)

// errFlagParse marks a command line the flag package rejected and has already
// reported to the user.
var errFlagParse = errors.New("invalid command line")

// serverConfig is the validated configuration of "cactusfs server".
type serverConfig struct {
	// AdminAddr is the listen address of the admin listener: probes and
	// metrics.
	AdminAddr string
	// S3Addr is the listen address of the S3 API.
	S3Addr string
	// S3Domain enables virtual-hosted-style requests under that domain.
	S3Domain string
	// Region is the region name reported to S3 clients.
	Region string
	// Data lists the drive directories.
	Data []string
	// MetaDir is the metadata directory.
	MetaDir string
	// KMS names the key management service; KMSKeyFile and
	// KMSPassphraseFile configure the local one.
	KMS               string
	KMSKeyFile        string
	KMSPassphraseFile string
	// Vault configures the Vault Transit KMS.
	VaultAddr       string
	VaultMount      string
	VaultKey        string
	VaultTokenFile  string
	VaultCACert     string
	VaultServerName string
	VaultNamespace  string
	// AuditExport writes every audit record to standard output as a JSON
	// line, next to the log.
	AuditExport bool
	// TLSCert and TLSKey are the paths of the PEM certificate chain and
	// private key of the listeners.
	TLSCert string
	TLSKey  string
	// DevPlaintext serves HTTP without TLS, on loopback only.
	DevPlaintext bool
	// LogLevel is the minimum level written to the log.
	LogLevel slog.Level
	// ScrubInterval is the pause between two passes of the scrubber; zero
	// disables scrubbing. ScrubRateMiB limits how fast it reads, in MiB per
	// second; zero is no limit.
	ScrubInterval time.Duration
	ScrubRateMiB  int
	// LifecycleInterval is the pause between two passes of the worker
	// that carries out lifecycle rules; zero disables it.
	LifecycleInterval time.Duration
	// GCInterval is the pause between two passes of the garbage collector;
	// zero disables it. GCGrace is the minimum age of an unreferenced shard
	// before it is removed.
	GCInterval time.Duration
	GCGrace    time.Duration
	// AllowDriveReplacement confirms that one empty drive may take the place
	// of one missing drive at this start.
	AllowDriveReplacement bool
	// OTLPEndpoint is the URL of an OTLP/HTTP trace collector. Empty, the
	// default, exports nothing.
	OTLPEndpoint string
	// DrainDelay is how long the node keeps serving after a shutdown signal,
	// with /readyz answering 503, before it closes its listeners. Zero closes
	// them at once.
	DrainDelay time.Duration
	// ShutdownTimeout bounds the wait for in-flight requests once the
	// listeners have closed.
	ShutdownTimeout time.Duration

	// NodeID names the node. A node of a cluster must be given one; a node
	// on its own generates one.
	NodeID string
	// Roles are the jobs this node does.
	Roles cluster.Roles
	// Peers lists every node of the cluster, this one included, by ID with
	// the address of its node RPC. It is what makes a node a member of a
	// cluster; empty, the node runs on its own.
	Peers map[string]string
	// RPCAddr and RaftAddr are the listen addresses of node RPC and Raft;
	// AdvertiseRPC and AdvertiseRaft are the addresses at which the other
	// nodes reach them.
	RPCAddr       string
	RaftAddr      string
	AdvertiseRPC  string
	AdvertiseRaft string
	// NodeTLSCert, NodeTLSKey and NodeTLSCA are the node's certificate and
	// key and the CA of the cluster's nodes, for mutual TLS between nodes.
	NodeTLSCert string
	NodeTLSKey  string
	NodeTLSCA   string
	// Labels say where the node is, for placement.
	Labels cluster.Labels
	// secrets come from the environment and are never logged or printed.
	secrets secrets
	// auditOut is where exported audit records go. It has no flag: the
	// server sets it to its log stream when AuditExport is on.
	auditOut io.Writer
	// passwordParams overrides the cost of console password hashes. It has
	// no flag: tests lower it, production keeps the default.
	passwordParams passwords.Params
	// raftTiming overrides Raft's timeouts. It has no flag: tests shorten
	// the timeouts, production keeps Raft's defaults.
	raftTiming raftmeta.Timing
}

// secrets holds the secret inputs of a node.
type secrets struct {
	kmsPassphrase string
	rootAccessKey string
	rootSecretKey string
}

// stringList is a flag that may be given several times or as a
// comma-separated list.
type stringList []string

// String implements flag.Value.
func (l *stringList) String() string { return strings.Join(*l, ",") }

// Set implements flag.Value.
func (l *stringList) Set(value string) error {
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			*l = append(*l, item)
		}
	}
	return nil
}

// parseServerConfig builds the server configuration from, in order of
// precedence, command-line flags, CACTUSFS_* variables and defaults. Usage and
// flag errors are written to stderr.
func parseServerConfig(args []string, getenv func(string) string, stderr io.Writer) (serverConfig, error) {
	cfg := serverConfig{secrets: secrets{
		kmsPassphrase: getenv(envKMSPhrase),
		rootAccessKey: getenv(envRootKeyID),
		rootSecretKey: getenv(envRootKeyValue),
	}}

	devPlaintext, err := envBool(getenv, envDevPlaintext, false)
	if err != nil {
		return cfg, err
	}
	drainDelay, err := envDuration(getenv, envDrainDelay, 0)
	if err != nil {
		return cfg, err
	}
	shutdownTimeout, err := envDuration(getenv, envShutdownTimeout, defaultShutdownTimeout)
	if err != nil {
		return cfg, err
	}

	scrubInterval, err := envDuration(getenv, envScrubInterval, defaultScrubInterval)
	if err != nil {
		return cfg, err
	}
	lifecycleInterval, err := envDuration(getenv, envLifecycle, defaultLifecycle)
	if err != nil {
		return cfg, err
	}
	gcInterval, err := envDuration(getenv, envGCInterval, defaultGCInterval)
	if err != nil {
		return cfg, err
	}
	gcGrace, err := envDuration(getenv, envGCGrace, defaultGCGrace)
	if err != nil {
		return cfg, err
	}
	scrubRate, err := envInt(getenv, envScrubRate, defaultScrubRateMiB)
	if err != nil {
		return cfg, err
	}

	allowReplace, err := envBool(getenv, envAllowReplace, false)
	if err != nil {
		return cfg, err
	}

	auditExport, err := envBool(getenv, envAuditExport, false)
	if err != nil {
		return cfg, err
	}

	fs := flag.NewFlagSet("cactusfs server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var data stringList
	fs.Var(&data, "data", "drive `directory`; repeat the flag or separate directories with commas ($"+envData+")")
	fs.StringVar(&cfg.MetaDir, "meta-dir", getenv(envMetaDir),
		"metadata `directory`; default <first data directory>/"+metaDirName+" ($"+envMetaDir+")")
	fs.StringVar(&cfg.KMS, "kms", getenv(envKMS),
		"key management `service`: \""+kmsVault+"\" for HashiCorp Vault Transit, \""+kmsLocal+
			"\" for a sealed local key file; there is no default ($"+envKMS+")")
	fs.StringVar(&cfg.VaultAddr, "vault-addr", getenv(envVaultAddr), "`URL` of Vault, https://host:port ($"+envVaultAddr+")")
	fs.StringVar(&cfg.VaultMount, "vault-transit-mount", getenv(envVaultMount),
		"`path` of Vault's Transit engine; default transit ($"+envVaultMount+")")
	fs.StringVar(&cfg.VaultKey, "vault-key", getenv(envVaultKey),
		"`name` of the Transit key, of an AEAD type such as aes256-gcm96 ($"+envVaultKey+")")
	fs.StringVar(&cfg.VaultTokenFile, "vault-token-file", getenv(envVaultAuthFile),
		"`path` of a file holding the Vault token; read at every use, so it can be renewed in place ($"+envVaultAuthFile+")")
	fs.StringVar(&cfg.VaultCACert, "vault-ca-cert", getenv(envVaultCACert),
		"`path` of the CA certificate that issued Vault's certificate; default the system's CAs ($"+envVaultCACert+")")
	fs.StringVar(&cfg.VaultServerName, "vault-server-name", getenv(envVaultServerName),
		"`name` expected in Vault's certificate, when Vault is reached under another ($"+envVaultServerName+")")
	fs.StringVar(&cfg.VaultNamespace, "vault-namespace", getenv(envVaultNamespace),
		"Vault Enterprise `namespace` ($"+envVaultNamespace+")")
	fs.BoolVar(&cfg.AuditExport, "audit-export", auditExport,
		"write every audit record to standard output as a JSON line with \"stream\":\"audit\" ($"+envAuditExport+")")
	fs.StringVar(&cfg.KMSKeyFile, "kms-key-file", getenv(envKMSKeyFile),
		"`path` of the local KMS key file; default <metadata directory>/"+kmsKeyFileName+" ($"+envKMSKeyFile+")")
	fs.StringVar(&cfg.KMSPassphraseFile, "kms-passphrase-file", getenv(envKMSPhraseFile),
		"`path` of a file holding the local KMS passphrase, instead of $"+envKMSPhrase+" ($"+envKMSPhraseFile+")")
	fs.StringVar(&cfg.S3Addr, "s3-addr", envString(getenv, envS3Addr, defaultS3Addr),
		"listen `address` of the S3 API ($"+envS3Addr+")")
	fs.StringVar(&cfg.S3Domain, "s3-domain", getenv(envS3Domain),
		"`domain` for virtual-hosted-style requests, <bucket>.<domain> ($"+envS3Domain+")")
	fs.StringVar(&cfg.Region, "region", getenv(envRegion),
		"`region` name reported to S3 clients; default us-east-1 ($"+envRegion+")")
	fs.StringVar(&cfg.AdminAddr, "admin-addr", envString(getenv, envAdminAddr, defaultAdminAddr),
		"listen `address` of the admin listener ($"+envAdminAddr+")")
	fs.StringVar(&cfg.TLSCert, "tls-cert", getenv(envTLSCert),
		"`path` of the PEM certificate chain ($"+envTLSCert+")")
	fs.StringVar(&cfg.TLSKey, "tls-key", getenv(envTLSKey),
		"`path` of the PEM private key ($"+envTLSKey+")")
	fs.BoolVar(&cfg.DevPlaintext, "dev-plaintext", devPlaintext,
		"DEVELOPMENT ONLY: serve HTTP without TLS, on a loopback IP address only ($"+envDevPlaintext+")")
	fs.DurationVar(&cfg.DrainDelay, "drain-delay", drainDelay,
		"on shutdown, keep serving this long with /readyz answering 503 before closing the listeners ($"+envDrainDelay+")")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", shutdownTimeout,
		"time allowed for in-flight requests to finish once the listeners have closed ($"+envShutdownTimeout+")")
	fs.DurationVar(&cfg.ScrubInterval, "scrub-interval", scrubInterval,
		"pause between two passes of the scrubber, which verifies and heals every object; 0 disables it ($"+envScrubInterval+")")
	fs.DurationVar(&cfg.LifecycleInterval, "lifecycle-interval", lifecycleInterval,
		"pause between two passes of the worker that carries out the lifecycle rules of buckets; 0 disables it ($"+envLifecycle+")")
	fs.IntVar(&cfg.ScrubRateMiB, "scrub-rate-mib", scrubRate,
		"limit on how fast the scrubber reads, in MiB per second; 0 is no limit ($"+envScrubRate+")")
	fs.DurationVar(&cfg.GCInterval, "gc-interval", gcInterval,
		"pause between two passes of the garbage collector for unreferenced shards; 0 disables it ($"+envGCInterval+")")
	fs.DurationVar(&cfg.GCGrace, "gc-grace", gcGrace,
		"minimum age of an unreferenced shard before it is removed ($"+envGCGrace+")")
	fs.BoolVar(&cfg.AllowDriveReplacement, "allow-drive-replacement", allowReplace,
		"confirm that one empty drive replaces one missing drive at this start; permanent: the missing drive is "+
			"refused afterwards ($"+envAllowReplace+")")
	fs.StringVar(&cfg.NodeID, "node-id", getenv(envNodeID),
		"`name` of this node: lowercase letters, digits and hyphens; required in a cluster, where it must be "+
			"the name in the node certificate ($"+envNodeID+")")
	roles := fs.String("roles", getenv(envRoles),
		"`roles` of this node: gateway, meta, storage, console; default all ($"+envRoles+")")
	peers := fs.String("peers", getenv(envPeers),
		"every node of the cluster, this one included, as `id=host:port` of its node RPC, separated by commas; "+
			"makes the node a member of a cluster ($"+envPeers+")")
	fs.StringVar(&cfg.RPCAddr, "rpc-addr", envString(getenv, envRPCAddr, defaultRPCAddr),
		"listen `address` of node RPC ($"+envRPCAddr+")")
	fs.StringVar(&cfg.RaftAddr, "raft-addr", envString(getenv, envRaftAddr, defaultRaftAddr),
		"listen `address` of the Raft transport ($"+envRaftAddr+")")
	fs.StringVar(&cfg.AdvertiseRPC, "advertise-rpc", getenv(envAdvertiseRPC),
		"`host:port` at which other nodes reach this node's RPC; default its entry in --peers ($"+envAdvertiseRPC+")")
	fs.StringVar(&cfg.AdvertiseRaft, "advertise-raft", getenv(envAdvertiseRaft),
		"`host:port` at which other nodes reach this node's Raft transport; default the host of "+
			"--advertise-rpc with the port of --raft-addr ($"+envAdvertiseRaft+")")
	fs.StringVar(&cfg.NodeTLSCert, "node-tls-cert", getenv(envNodeTLSCert),
		"`path` of the node certificate for mutual TLS between nodes ($"+envNodeTLSCert+")")
	fs.StringVar(&cfg.NodeTLSKey, "node-tls-key", getenv(envNodeTLSKey),
		"`path` of the node certificate's private key ($"+envNodeTLSKey+")")
	fs.StringVar(&cfg.NodeTLSCA, "node-tls-ca", getenv(envNodeTLSCA),
		"`path` of the CA certificate that issues the cluster's node certificates, and nothing else ($"+envNodeTLSCA+")")
	fs.StringVar(&cfg.Labels.Region, "topology-region", getenv(envTopologyRegion),
		"`region` this node is in, for placement ($"+envTopologyRegion+")")
	fs.StringVar(&cfg.Labels.Zone, "topology-zone", getenv(envTopologyZone),
		"`zone` this node is in, for placement ($"+envTopologyZone+")")
	fs.StringVar(&cfg.Labels.Rack, "topology-rack", getenv(envTopologyRack),
		"`rack` this node is in, for placement ($"+envTopologyRack+")")
	fs.StringVar(&cfg.Labels.Host, "topology-host", getenv(envTopologyHost),
		"`host` this node runs on, for placement ($"+envTopologyHost+")")
	fs.StringVar(&cfg.OTLPEndpoint, "otlp-endpoint", getenv(envOTLPEndpoint),
		"`URL` of an OTLP/HTTP trace collector; traces are not exported when empty ($"+envOTLPEndpoint+")")
	logLevel := fs.String("log-level", envString(getenv, envLogLevel, defaultLogLevel),
		"minimum log `level`: debug, info, warn or error ($"+envLogLevel+")")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return cfg, err
		}
		return cfg, errFlagParse
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if len(data) == 0 {
		// The flag was not given: fall back to the variable.
		if err := data.Set(getenv(envData)); err != nil {
			return cfg, err
		}
	}
	cfg.Data = data
	if cfg.Roles, err = cluster.ParseRoles(*roles); err != nil {
		return cfg, err
	}
	if cfg.Peers, err = parsePeers(*peers); err != nil {
		return cfg, err
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(*logLevel)); err != nil {
		return cfg, fmt.Errorf("log level %q: want debug, info, warn or error", *logLevel)
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// validate rejects configurations that would break a data-security rule and
// fills in the settings that derive from others.
func (c *serverConfig) validate() error {
	if c.DrainDelay < 0 {
		return fmt.Errorf("drain delay %s: must not be negative", c.DrainDelay)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("shutdown timeout %s: must be positive", c.ShutdownTimeout)
	}
	if c.ScrubInterval < 0 || c.GCInterval < 0 || c.ScrubRateMiB < 0 || c.LifecycleInterval < 0 {
		return errors.New("scrub interval, scrub rate, lifecycle interval and garbage collection interval must not be negative")
	}
	if c.GCGrace < minGCGrace {
		return fmt.Errorf("garbage collection grace period %s: must be at least %s", c.GCGrace, minGCGrace)
	}
	if err := c.validateTransport(); err != nil {
		return err
	}
	if err := c.validateCluster(); err != nil {
		return err
	}
	if err := c.validateStorage(); err != nil {
		return err
	}
	return c.validateSecrets()
}

// parsePeers parses the --peers list.
func parsePeers(list string) (map[string]string, error) {
	if strings.TrimSpace(list) == "" {
		return nil, nil
	}
	peers := map[string]string{}
	for item := range strings.SplitSeq(list, ",") {
		id, address, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok {
			return nil, fmt.Errorf("peer %q: want id=host:port", item)
		}
		if err := cluster.ValidateNodeID(id); err != nil {
			return nil, fmt.Errorf("peer %q: %w", item, err)
		}
		if host, _, err := net.SplitHostPort(address); err != nil || host == "" {
			return nil, fmt.Errorf("peer %q: address %q: want host:port", item, address)
		}
		if _, dup := peers[id]; dup {
			return nil, fmt.Errorf("peer %s is listed twice", id)
		}
		peers[id] = address
	}
	return peers, nil
}

// clustered reports whether the node is a member of a cluster.
func (c *serverConfig) clustered() bool {
	return len(c.Peers) > 0
}

// validateCluster checks what a member of a cluster needs: a name, its place
// in the list of peers, and a certificate - there is no node RPC and no Raft
// transport without mutual TLS, in development mode either.
func (c *serverConfig) validateCluster() error {
	if c.NodeID != "" {
		if err := cluster.ValidateNodeID(c.NodeID); err != nil {
			return err
		}
	}
	if !c.clustered() {
		if c.NodeTLSCert != "" || c.NodeTLSKey != "" || c.NodeTLSCA != "" || c.AdvertiseRPC != "" || c.AdvertiseRaft != "" {
			return errors.New("node certificates and advertised addresses are for members of a cluster: set --peers")
		}
		if !slices.Equal(c.Roles, cluster.AllRoles()) {
			return errors.New("a node on its own has every role: --roles is for members of a cluster (set --peers)")
		}
		return nil
	}
	if c.NodeID == "" {
		return errors.New("a member of a cluster needs a name: set --node-id")
	}
	own, listed := c.Peers[c.NodeID]
	if !listed {
		return fmt.Errorf("--peers must list every node of the cluster, this one (%s) included", c.NodeID)
	}
	if c.NodeTLSCert == "" || c.NodeTLSKey == "" || c.NodeTLSCA == "" {
		return errors.New("nodes of a cluster talk to each other over mutual TLS only: " +
			"set --node-tls-cert, --node-tls-key and --node-tls-ca")
	}
	if c.AdvertiseRPC == "" {
		c.AdvertiseRPC = own
	}
	host, _, err := net.SplitHostPort(c.AdvertiseRPC)
	if err != nil || host == "" {
		return fmt.Errorf("advertised RPC address %q: want host:port", c.AdvertiseRPC)
	}
	for name, addr := range map[string]string{"RPC": c.RPCAddr, "Raft": c.RaftAddr} {
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return fmt.Errorf("%s address %q: %w", name, addr, err)
		}
	}
	if c.AdvertiseRaft == "" {
		_, port, _ := net.SplitHostPort(c.RaftAddr)
		c.AdvertiseRaft = net.JoinHostPort(host, port)
	}
	if h, _, err := net.SplitHostPort(c.AdvertiseRaft); err != nil || h == "" {
		return fmt.Errorf("advertised Raft address %q: want host:port", c.AdvertiseRaft)
	}
	return nil
}

// validateTransport enforces encryption in transit: TLS, or development
// plaintext on a loopback address only.
func (c *serverConfig) validateTransport() error {
	if !c.DevPlaintext && (c.TLSCert == "" || c.TLSKey == "") {
		return errors.New("TLS is required: set --tls-cert and --tls-key " +
			"(--dev-plaintext serves HTTP on loopback for development only)")
	}
	if c.DevPlaintext && (c.TLSCert != "" || c.TLSKey != "") {
		return errors.New("--dev-plaintext cannot be combined with --tls-cert or --tls-key")
	}
	for name, addr := range map[string]*string{"admin": &c.AdminAddr, "S3": &c.S3Addr} {
		host, port, err := net.SplitHostPort(*addr)
		if err != nil {
			return fmt.Errorf("%s address %q: %w", name, *addr, err)
		}
		if !c.DevPlaintext {
			continue
		}
		if host == "" {
			host = loopbackHost
			*addr = net.JoinHostPort(host, port)
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("%s address %q: --dev-plaintext listens on a loopback IP address only "+
				"(127.0.0.1 or ::1)", name, *addr)
		}
	}
	return nil
}

// validateStorage requires drives and an explicitly chosen KMS: there is no
// mode that stores data unencrypted.
func (c *serverConfig) validateStorage() error {
	switch storage := c.Roles.Has(cluster.RoleStorage); {
	case storage && len(c.Data) == 0:
		return errors.New("at least one drive is required: set --data <directory>")
	case !storage && len(c.Data) > 0:
		return errors.New("--data is for nodes with the storage role")
	case !storage && c.MetaDir == "":
		return errors.New("a node without drives needs a metadata directory: set --meta-dir")
	}
	seen := map[string]bool{}
	for i, dir := range c.Data {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("data directory %q: %w", dir, err)
		}
		if seen[abs] {
			return fmt.Errorf("data directory %q is listed twice", dir)
		}
		seen[abs] = true
		c.Data[i] = abs
	}
	if c.MetaDir == "" {
		c.MetaDir = filepath.Join(c.Data[0], metaDirName)
	}
	switch c.KMS {
	case kmsLocal:
		if c.VaultAddr != "" || c.VaultKey != "" || c.VaultTokenFile != "" {
			return errors.New("the --vault-* settings are for --kms " + kmsVault)
		}
	case kmsVault:
		if c.VaultAddr == "" || c.VaultKey == "" || c.VaultTokenFile == "" {
			return errors.New("--kms " + kmsVault + " needs --vault-addr, --vault-key and --vault-token-file")
		}
		if c.KMSKeyFile != "" || c.KMSPassphraseFile != "" || c.secrets.kmsPassphrase != "" {
			return errors.New("the key file and passphrase settings are for --kms " + kmsLocal)
		}
		return nil
	case "":
		return errors.New("a key management service must be chosen: set --kms " + kmsVault + " for HashiCorp Vault Transit or --kms " +
			kmsLocal + " for a sealed local key file (there is no unencrypted mode)")
	default:
		return fmt.Errorf("unknown key management service %q: want %q or %q", c.KMS, kmsVault, kmsLocal)
	}
	if c.KMSKeyFile == "" {
		if c.clustered() {
			// Every node must open the same key; one made here would be
			// this node's alone.
			return errors.New("the nodes of a cluster share one KMS key: set --kms-key-file to the key file " +
				"they all have (\"cactusfs dev-init\" writes one for development)")
		}
		c.KMSKeyFile = filepath.Join(c.MetaDir, kmsKeyFileName)
	}
	return nil
}

func (c *serverConfig) validateSecrets() error {
	if (c.secrets.rootAccessKey == "") != (c.secrets.rootSecretKey == "") {
		return errors.New("set both $" + envRootKeyID + " and $" + envRootKeyValue + ", or neither")
	}
	if c.KMS == kmsVault {
		return nil
	}
	if c.secrets.kmsPassphrase != "" && c.KMSPassphraseFile != "" {
		return errors.New("set either $" + envKMSPhrase + " or --kms-passphrase-file, not both")
	}
	if c.secrets.kmsPassphrase == "" && c.KMSPassphraseFile == "" {
		return errors.New("the local KMS needs a passphrase: set $" + envKMSPhrase + " or --kms-passphrase-file")
	}
	if (c.secrets.rootAccessKey == "") != (c.secrets.rootSecretKey == "") {
		return errors.New("set both $" + envRootKeyID + " and $" + envRootKeyValue + ", or neither")
	}
	return nil
}

func envString(getenv func(string) string, key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(getenv func(string) string, key string, fallback bool) (bool, error) {
	v := getenv(key)
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s=%q: want true or false", key, v)
	}
	return b, nil
}

func envDuration(getenv func(string) string, key string, fallback time.Duration) (time.Duration, error) {
	v := getenv(key)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: want a duration such as 30s", key, v)
	}
	return d, nil
}

func envInt(getenv func(string) string, key string, fallback int) (int, error) {
	v := getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: want a whole number", key, v)
	}
	return n, nil
}
