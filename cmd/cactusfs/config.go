package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"time"
)

// Environment variables read by "cactusfs server". Each has a matching flag;
// a flag given on the command line wins over the variable.
const (
	envAdminAddr       = "CACTUSFS_ADMIN_ADDR"
	envTLSCert         = "CACTUSFS_TLS_CERT"
	envTLSKey          = "CACTUSFS_TLS_KEY"
	envDevPlaintext    = "CACTUSFS_DEV_PLAINTEXT"
	envLogLevel        = "CACTUSFS_LOG_LEVEL"
	envDrainDelay      = "CACTUSFS_DRAIN_DELAY"
	envShutdownTimeout = "CACTUSFS_SHUTDOWN_TIMEOUT"
)

const (
	defaultAdminAddr       = ":9001"
	defaultLogLevel        = "info"
	defaultShutdownTimeout = 30 * time.Second
	loopbackHost           = "127.0.0.1"
)

// errFlagParse marks a command line the flag package rejected and has already
// reported to the user.
var errFlagParse = errors.New("invalid command line")

// serverConfig is the validated configuration of "cactusfs server".
type serverConfig struct {
	// AdminAddr is the listen address of the admin listener, which serves the
	// probe endpoints.
	AdminAddr string
	// TLSCert and TLSKey are the paths of the PEM certificate chain and
	// private key of the listener.
	TLSCert string
	TLSKey  string
	// DevPlaintext serves HTTP without TLS, on loopback only.
	DevPlaintext bool
	// LogLevel is the minimum level written to the log.
	LogLevel slog.Level
	// DrainDelay is how long the node keeps serving after a shutdown signal,
	// with /readyz answering 503, before it closes its listener. Zero closes
	// the listener at once.
	DrainDelay time.Duration
	// ShutdownTimeout bounds the wait for in-flight requests once the
	// listener has closed.
	ShutdownTimeout time.Duration
}

// parseServerConfig builds the server configuration from, in order of
// precedence, command-line flags, CACTUSFS_* variables and defaults. Usage and
// flag errors are written to stderr.
func parseServerConfig(args []string, getenv func(string) string, stderr io.Writer) (serverConfig, error) {
	var cfg serverConfig

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
	fs.StringVar(&cfg.AdminAddr, "admin-addr", envString(getenv, envAdminAddr, defaultAdminAddr),
		"listen `address` of the admin listener ($"+envAdminAddr+")")
	fs.StringVar(&cfg.TLSCert, "tls-cert", envString(getenv, envTLSCert, ""),
		"`path` of the PEM certificate chain ($"+envTLSCert+")")
	fs.StringVar(&cfg.TLSKey, "tls-key", envString(getenv, envTLSKey, ""),
		"`path` of the PEM private key ($"+envTLSKey+")")
	fs.BoolVar(&cfg.DevPlaintext, "dev-plaintext", devPlaintext,
		"DEVELOPMENT ONLY: serve HTTP without TLS, on a loopback IP address only ($"+envDevPlaintext+")")
	fs.DurationVar(&cfg.DrainDelay, "drain-delay", drainDelay,
		"on shutdown, keep serving this long with /readyz answering 503 before closing the listener ($"+envDrainDelay+")")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", shutdownTimeout,
		"time allowed for in-flight requests to finish once the listener has closed ($"+envShutdownTimeout+")")
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
	if err := cfg.LogLevel.UnmarshalText([]byte(*logLevel)); err != nil {
		return cfg, fmt.Errorf("log level %q: want debug, info, warn or error", *logLevel)
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// validate rejects configurations that would break the encryption-in-transit
// rule and normalises the listen address.
func (c *serverConfig) validate() error {
	host, port, err := net.SplitHostPort(c.AdminAddr)
	if err != nil {
		return fmt.Errorf("admin address %q: %w", c.AdminAddr, err)
	}
	if c.DrainDelay < 0 {
		return fmt.Errorf("drain delay %s: must not be negative", c.DrainDelay)
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("shutdown timeout %s: must be positive", c.ShutdownTimeout)
	}

	if !c.DevPlaintext {
		if c.TLSCert == "" || c.TLSKey == "" {
			return errors.New("TLS is required: set --tls-cert and --tls-key " +
				"(--dev-plaintext serves HTTP on loopback for development only)")
		}
		return nil
	}

	if c.TLSCert != "" || c.TLSKey != "" {
		return errors.New("--dev-plaintext cannot be combined with --tls-cert or --tls-key")
	}
	if host == "" {
		host = loopbackHost
		c.AdminAddr = net.JoinHostPort(host, port)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("admin address %q: --dev-plaintext listens on a loopback IP address only "+
			"(127.0.0.1 or ::1)", c.AdminAddr)
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
