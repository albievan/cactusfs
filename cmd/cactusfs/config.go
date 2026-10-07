package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
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
	envTLSCert         = "CACTUSFS_TLS_CERT"
	envTLSKey          = "CACTUSFS_TLS_KEY"
	envDevPlaintext    = "CACTUSFS_DEV_PLAINTEXT"
	envLogLevel        = "CACTUSFS_LOG_LEVEL"
	envOTLPEndpoint    = "CACTUSFS_OTLP_ENDPOINT"
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
	defaultLogLevel        = "info"
	defaultShutdownTimeout = 30 * time.Second
	loopbackHost           = "127.0.0.1"
	metaDirName            = "meta"
	kmsKeyFileName         = "kms-local.json"
	kmsLocal               = "local"
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
	// TLSCert and TLSKey are the paths of the PEM certificate chain and
	// private key of the listeners.
	TLSCert string
	TLSKey  string
	// DevPlaintext serves HTTP without TLS, on loopback only.
	DevPlaintext bool
	// LogLevel is the minimum level written to the log.
	LogLevel slog.Level
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

	// secrets come from the environment and are never logged or printed.
	secrets secrets
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

	fs := flag.NewFlagSet("cactusfs server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var data stringList
	fs.Var(&data, "data", "drive `directory`; repeat the flag or separate directories with commas ($"+envData+")")
	fs.StringVar(&cfg.MetaDir, "meta-dir", getenv(envMetaDir),
		"metadata `directory`; default <first data directory>/"+metaDirName+" ($"+envMetaDir+")")
	fs.StringVar(&cfg.KMS, "kms", getenv(envKMS),
		"key management `service`: \""+kmsLocal+"\" for a sealed local key file; there is no default ($"+envKMS+")")
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
	if err := c.validateTransport(); err != nil {
		return err
	}
	if err := c.validateStorage(); err != nil {
		return err
	}
	return c.validateSecrets()
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
	if len(c.Data) == 0 {
		return errors.New("at least one drive is required: set --data <directory>")
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
	case "":
		return errors.New("a key management service must be chosen: set --kms " + kmsLocal +
			" for a sealed local key file (there is no unencrypted mode)")
	default:
		return fmt.Errorf("unknown key management service %q: want %q", c.KMS, kmsLocal)
	}
	if c.KMSKeyFile == "" {
		c.KMSKeyFile = filepath.Join(c.MetaDir, kmsKeyFileName)
	}
	return nil
}

func (c *serverConfig) validateSecrets() error {
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
