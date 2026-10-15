package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain/cluster"
)

func noEnv(string) string { return "" }

func envMap(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

// storeArgs are the flags every server start needs beside its transport: a
// drive and an explicitly chosen KMS.
func storeArgs(t *testing.T, args ...string) []string {
	t.Helper()
	return append([]string{"--data", t.TempDir(), "--kms", "local", "--s3-addr", "127.0.0.1:0"}, args...)
}

// storeEnv is the environment of those starts: the KMS passphrase.
func storeEnv() func(string) string {
	return envMap(map[string]string{envKMSPhrase: "unit test passphrase"})
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       func(t *testing.T) []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no command", wantCode: exitUsage, wantStderr: "Usage:"},
		{name: "unknown command", args: fixed("frobnicate"), wantCode: exitUsage, wantStderr: `unknown command "frobnicate"`},
		{name: "help", args: fixed("help"), wantCode: exitOK, wantStdout: "Usage:"},
		{name: "help flag", args: fixed("--help"), wantCode: exitOK, wantStdout: "Usage:"},
		{name: "version", args: fixed("version"), wantCode: exitOK, wantStdout: "cactusfs " + buildinfo.Release() + " "},
		{name: "version names the toolchain and the platform", args: fixed("version"), wantCode: exitOK, wantStdout: runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH + ")"},
		{name: "server help", args: fixed("server", "--help"), wantCode: exitOK, wantStderr: "-admin-addr"},
		{name: "server unknown flag", args: fixed("server", "--nope"), wantCode: exitUsage, wantStderr: "flag provided but not defined"},
		{name: "server without TLS refuses to start", args: fixed("server"), wantCode: exitUsage, wantStderr: "TLS is required"},
		{name: "server without drives refuses to start", args: fixed("server", "--dev-plaintext"), wantCode: exitUsage, wantStderr: "at least one drive"},
		{
			name:     "server without a KMS refuses to start",
			args:     func(t *testing.T) []string { return []string{"server", "--dev-plaintext", "--data", t.TempDir()} },
			wantCode: exitUsage, wantStderr: "there is no unencrypted mode",
		},
		{
			name: "server with unreadable certificate fails",
			args: func(t *testing.T) []string {
				return append([]string{"server"}, storeArgs(t, "--tls-cert", "missing.pem", "--tls-key", "missing.key")...)
			},
			wantCode: exitError, wantStderr: "server.LoadTLS",
		},
		{
			name: "server with a plaintext trace collector refuses to start",
			args: func(t *testing.T) []string {
				return append([]string{"server"}, storeArgs(t, "--dev-plaintext", "--otlp-endpoint", "http://collector.example:4318")...)
			},
			wantCode: exitError, wantStderr: "must use https",
		},
		{
			// And is told so without the credential being repeated: see
			// TestARefusedCollectorIsNamedWithoutItsCredential.
			name: "server with a credential in the URL of the trace collector refuses to start",
			args: func(t *testing.T) []string {
				return append([]string{"server"}, storeArgs(t, "--dev-plaintext", "--otlp-endpoint", "https://tracer:pw@collector.example:4318")...)
			},
			wantCode: exitError, wantStderr: `OTLP endpoint "https://collector.example:4318" (shown without them) must not carry userinfo`,
		},
		{
			name: "server with unusable address fails",
			args: func(t *testing.T) []string {
				return append([]string{"server"}, storeArgs(t, "--dev-plaintext", "--admin-addr", "127.0.0.1:99999")...)
			},
			wantCode: exitError, wantStderr: "server.Listen",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var args []string
			if tt.args != nil {
				args = tt.args(t)
			}
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), args, &stdout, &stderr, storeEnv())

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func fixed(args ...string) func(*testing.T) []string {
	return func(*testing.T) []string { return args }
}

// cancelOnListen cancels a context as soon as the server logs that it is
// listening, which lets a test stop a server it started through run.
type cancelOnListen struct {
	cancel context.CancelFunc
}

func (c cancelOnListen) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"msg":"listening"`)) {
		c.cancel()
	}
	return len(p), nil
}

func TestRunServerStopsCleanlyWhenContextIsCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var stderr bytes.Buffer
	args := append([]string{"server"}, storeArgs(t, "--dev-plaintext", "--admin-addr", "127.0.0.1:0")...)
	code := run(ctx, args, cancelOnListen{cancel: cancel}, &stderr, storeEnv())

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}
	// A new deployment shows its generated root credentials once, here.
	if !strings.Contains(stderr.String(), "Access key ID:") || !strings.Contains(stderr.String(), "shown once") {
		t.Errorf("stderr does not show the generated root credentials: %q", stderr.String())
	}
}

func TestParseServerConfig(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	otherDir := t.TempDir()
	passphrase := map[string]string{envKMSPhrase: "unit test passphrase"}
	withPassphrase := func(extra map[string]string) map[string]string {
		env := map[string]string{envKMSPhrase: "unit test passphrase"}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}
	// base is what a minimal development start resolves to.
	base := func(change func(c *serverConfig)) serverConfig {
		c := serverConfig{
			AdminAddr: "127.0.0.1:9001", S3Addr: "127.0.0.1:9000", Data: []string{dataDir},
			MetaDir: filepath.Join(dataDir, "meta"), KMS: "local", KMSKeyFile: filepath.Join(dataDir, "meta", "kms-local.json"),
			DevPlaintext: true, LogLevel: slog.LevelInfo, ShutdownTimeout: 30 * time.Second,
			ScrubInterval: 24 * time.Hour, ScrubRateMiB: 64, GCInterval: time.Hour, GCGrace: 24 * time.Hour,
			LifecycleInterval: time.Hour, AccessFlushInterval: 30 * time.Second, TLSReloadInterval: time.Minute,
			SessionLifetime: 30 * time.Minute, SessionUploadGrace: 90 * time.Second, SessionLockMax: 12 * time.Hour,
			DeliveryTimeout: 30 * time.Second, ReplicationWorkers: 4,
			Roles: cluster.AllRoles(), RPCAddr: ":9002", RaftAddr: ":9003",
			secrets: secrets{kmsPassphrase: "unit test passphrase"},
		}
		if change != nil {
			change(&c)
		}
		return c
	}
	dev := func(args ...string) []string {
		return append([]string{"--dev-plaintext", "--data", dataDir, "--kms", "local"}, args...)
	}

	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    serverConfig
		wantErr string
	}{
		{name: "defaults need TLS", env: passphrase, wantErr: "TLS is required"},
		{name: "certificate without key", args: []string{"--tls-cert", "c.pem"}, env: passphrase, wantErr: "TLS is required"},
		{
			name: "TLS listens on all interfaces",
			args: []string{"--tls-cert", "c.pem", "--tls-key", "k.pem", "--data", dataDir, "--kms", "local"}, env: passphrase,
			want: base(func(c *serverConfig) {
				c.AdminAddr, c.S3Addr, c.TLSCert, c.TLSKey, c.DevPlaintext = ":9001", ":9000", "c.pem", "k.pem", false
			}),
		},
		{name: "plaintext defaults to loopback", args: dev(), env: passphrase, want: base(nil)},
		{
			name: "plaintext on IPv6 loopback", args: dev("--admin-addr", "[::1]:9001", "--s3-addr", "[::1]:9000"), env: passphrase,
			want: base(func(c *serverConfig) { c.AdminAddr, c.S3Addr = "[::1]:9001", "[::1]:9000" }),
		},
		{name: "plaintext admin on all interfaces", args: dev("--admin-addr", "0.0.0.0:9001"), env: passphrase, wantErr: "loopback IP address only"},
		{name: "plaintext S3 on all interfaces", args: dev("--s3-addr", "0.0.0.0:9000"), env: passphrase, wantErr: "loopback IP address only"},
		{name: "plaintext S3 on a routable address", args: dev("--s3-addr", "192.0.2.10:9000"), env: passphrase, wantErr: "loopback IP address only"},
		{name: "plaintext on a host name", args: dev("--admin-addr", "localhost:9001"), env: passphrase, wantErr: "loopback IP address only"},
		{name: "plaintext with certificate", args: dev("--tls-cert", "c.pem", "--tls-key", "k.pem"), env: passphrase, wantErr: "cannot be combined"},
		{name: "no drives", args: []string{"--dev-plaintext", "--kms", "local"}, env: passphrase, wantErr: "at least one drive"},
		{name: "the same drive twice", args: dev("--data", dataDir), env: passphrase, wantErr: "listed twice"},
		{name: "no KMS chosen", args: []string{"--dev-plaintext", "--data", dataDir}, env: passphrase, wantErr: "there is no unencrypted mode"},
		{name: "unknown KMS", args: []string{"--dev-plaintext", "--data", dataDir, "--kms", "none"}, env: passphrase, wantErr: "unknown key management service"},
		{name: "no passphrase", args: dev(), wantErr: "needs a passphrase"},
		{name: "passphrase twice", args: dev("--kms-passphrase-file", "p.txt"), env: passphrase, wantErr: "not both"},
		{
			name: "passphrase file", args: dev("--kms-passphrase-file", "p.txt"),
			want: base(func(c *serverConfig) { c.KMSPassphraseFile, c.secrets.kmsPassphrase = "p.txt", "" }),
		},
		{
			name: "several drives and explicit directories",
			args: []string{"--dev-plaintext", "--kms", "local", "--data", dataDir + "," + otherDir, "--meta-dir", "m", "--kms-key-file", "k.json", "--s3-domain", "s3.test", "--region", "eu-1"},
			env:  passphrase,
			want: base(func(c *serverConfig) {
				c.Data, c.MetaDir, c.KMSKeyFile, c.S3Domain, c.Region = []string{dataDir, otherDir}, "m", "k.json", "s3.test", "eu-1"
			}),
		},
		{
			name: "environment",
			env: withPassphrase(map[string]string{
				envAdminAddr: "127.0.0.1:19001", envS3Addr: "127.0.0.1:19000", envDevPlaintext: "true", envLogLevel: "debug",
				envShutdownTimeout: "5s", envDrainDelay: "3s", envData: dataDir + ", " + otherDir, envKMS: "local",
				envMetaDir: "m", envKMSKeyFile: "k.json", envS3Domain: "s3.test", envRegion: "eu-1",
				envRootKeyID: "ROOTKEY", envRootKeyValue: "root-secret-value", envOTLPEndpoint: "https://otel.test:4318",
				envScrubInterval: "6h", envScrubRate: "8", envGCInterval: "30m", envGCGrace: "48h",
			}),
			want: base(func(c *serverConfig) {
				c.AdminAddr, c.S3Addr, c.LogLevel = "127.0.0.1:19001", "127.0.0.1:19000", slog.LevelDebug
				c.DrainDelay, c.ShutdownTimeout = 3*time.Second, 5*time.Second
				c.Data, c.MetaDir, c.KMSKeyFile, c.S3Domain, c.Region = []string{dataDir, otherDir}, "m", "k.json", "s3.test", "eu-1"
				c.secrets.rootAccessKey, c.secrets.rootSecretKey = "ROOTKEY", "root-secret-value"
				c.OTLPEndpoint = "https://otel.test:4318"
				c.ScrubInterval, c.ScrubRateMiB, c.GCInterval, c.GCGrace = 6*time.Hour, 8, 30*time.Minute, 48*time.Hour
			}),
		},
		{
			name: "flag wins over environment",
			args: dev("--admin-addr", "127.0.0.1:2", "--log-level", "error", "--shutdown-timeout", "1s", "--dev-plaintext=true", "--drain-delay", "2s"),
			env: withPassphrase(map[string]string{
				envAdminAddr: "127.0.0.1:1", envLogLevel: "debug", envShutdownTimeout: "5s", envDevPlaintext: "false",
				envDrainDelay: "9s", envData: otherDir,
			}),
			want: base(func(c *serverConfig) {
				c.AdminAddr, c.LogLevel, c.DrainDelay, c.ShutdownTimeout = "127.0.0.1:2", slog.LevelError, 2*time.Second, time.Second
			}),
		},
		{name: "root access key without secret", args: dev(), env: withPassphrase(map[string]string{envRootKeyID: "ROOTKEY"}), wantErr: "or neither"},
		{name: "root secret without access key", args: dev(), env: withPassphrase(map[string]string{envRootKeyValue: "s"}), wantErr: "or neither"},
		{
			name: "background work switched off", args: dev("--scrub-interval", "0", "--gc-interval", "0", "--scrub-rate-mib", "0", "--lifecycle-interval", "0"), env: passphrase,
			want: base(func(c *serverConfig) { c.ScrubInterval, c.GCInterval, c.ScrubRateMiB, c.LifecycleInterval = 0, 0, 0, 0 }),
		},
		{name: "negative scrub interval", args: dev("--scrub-interval", "-1s"), env: passphrase, wantErr: "must not be negative"},
		{name: "negative lifecycle interval", args: dev("--lifecycle-interval", "-1s"), env: passphrase, wantErr: "must not be negative"},
		{name: "bad lifecycle interval variable", env: withPassphrase(map[string]string{envLifecycle: "daily"}), wantErr: envLifecycle},
		{
			name: "reads recorded more often, by flag over variable", args: dev("--access-flush-interval", "5s"),
			env:  withPassphrase(map[string]string{envAccessFlush: "1m"}),
			want: base(func(c *serverConfig) { c.AccessFlushInterval = 5 * time.Second }),
		},
		{
			name: "reads recorded less often, by variable", args: dev(), env: withPassphrase(map[string]string{envAccessFlush: "2m"}),
			want: base(func(c *serverConfig) { c.AccessFlushInterval = 2 * time.Minute }),
		},
		{name: "reads recorded too often", args: dev("--access-flush-interval", "4s"), env: passphrase, wantErr: "access flush interval"},
		{name: "reads never recorded", args: dev("--access-flush-interval", "0"), env: passphrase, wantErr: "access flush interval"},
		{name: "bad access flush interval variable", env: withPassphrase(map[string]string{envAccessFlush: "often"}), wantErr: envAccessFlush},
		{
			name: "console sessions, by flag over variable", args: dev("--session-lifetime", "10s", "--session-upload-grace", "5s", "--session-lock-max", "1m"),
			env: withPassphrase(map[string]string{envSessionLifetime: "1h", envSessionGrace: "2m", envSessionLockMax: "2h"}),
			want: base(func(c *serverConfig) {
				c.SessionLifetime, c.SessionUploadGrace, c.SessionLockMax = 10*time.Second, 5*time.Second, time.Minute
			}),
		},
		{
			name: "console sessions, by variable, at their longest", args: dev(),
			env: withPassphrase(map[string]string{envSessionLifetime: "12h", envSessionGrace: "10m", envSessionLockMax: "24h"}),
			want: base(func(c *serverConfig) {
				c.SessionLifetime, c.SessionUploadGrace, c.SessionLockMax = 12*time.Hour, 10*time.Minute, 24*time.Hour
			}),
		},
		{
			name: "console sessions that never lock", args: dev("--session-lock-max", "0"), env: passphrase,
			want: base(func(c *serverConfig) { c.SessionLockMax = 0 }),
		},
		{name: "a session too short", args: dev("--session-lifetime", "9s"), env: passphrase, wantErr: "session-lifetime 9s: must be between 10s and 12h"},
		{name: "a session too long", args: dev("--session-lifetime", "12h1m"), env: passphrase, wantErr: "session-lifetime"},
		{name: "a session of no time", args: dev("--session-lifetime", "0"), env: passphrase, wantErr: "session-lifetime"},
		{name: "an upload grace too short", args: dev("--session-upload-grace", "4s"), env: passphrase, wantErr: "session-upload-grace 4s: must be between 5s and 10m"},
		{name: "an upload grace too long", env: withPassphrase(map[string]string{envSessionGrace: "11m"}), args: dev(), wantErr: "session-upload-grace"},
		{name: "a session locked for too long", args: dev("--session-lock-max", "25h"), env: passphrase, wantErr: "session-lock-max 25h: must be between 0"},
		{name: "a session locked for less than no time", args: dev("--session-lock-max", "-1s"), env: passphrase, wantErr: "session-lock-max"},
		{name: "bad session lifetime variable", env: withPassphrase(map[string]string{envSessionLifetime: "long"}), wantErr: envSessionLifetime},
		{
			name: "delivery, by flag over variable",
			args: dev("--egress-allow", "127.0.0.1/32", "--egress-allow", "fe80::/10,::1/128", "--delivery-timeout", "1s",
				"--replication-workers", "1", "--replication-rate-mib", "0"),
			env: withPassphrase(map[string]string{
				envEgressAllow: "10.0.0.0/8", envDeliveryTimeout: "5m", envReplWorkers: "32", envReplRate: "1048576",
			}),
			want: base(func(c *serverConfig) {
				c.EgressAllow = []netip.Prefix{
					netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("::1/128"),
				}
				c.DeliveryTimeout, c.ReplicationWorkers = time.Second, 1
			}),
		},
		{
			name: "delivery, by variable, at its largest", args: dev(),
			env: withPassphrase(map[string]string{
				envEgressAllow: "169.254.170.2/32", envDeliveryTimeout: "5m", envReplWorkers: "32", envReplRate: "1048576",
			}),
			want: base(func(c *serverConfig) {
				c.EgressAllow = []netip.Prefix{netip.MustParsePrefix("169.254.170.2/32")}
				c.DeliveryTimeout, c.ReplicationWorkers, c.ReplicationRateMiB = 5*time.Minute, 32, 1<<20
			}),
		},
		{name: "a range that is none", args: dev("--egress-allow", "localhost"), env: passphrase, wantErr: `egress-allow "localhost": want ranges`},
		{name: "a range from inside it", args: dev("--egress-allow", "10.1.2.3/8"), env: passphrase, wantErr: "write the range from its first address, 10.0.0.0/8"},
		{name: "an IPv4 range as IPv6", args: dev("--egress-allow", "::ffff:127.0.0.0/104"), env: passphrase, wantErr: "write a range of IPv4 addresses as IPv4"},
		{name: "the metadata service allowed", env: withPassphrase(map[string]string{envEgressAllow: "169.254.169.254/32"}), args: dev(), wantErr: "never connected to"},
		{name: "the metadata service of Alibaba allowed", args: dev("--egress-allow", "100.100.100.200/32"), env: passphrase, wantErr: "never connected to"},
		{name: "every IPv4 address allowed", args: dev("--egress-allow", "0.0.0.0/0"), env: passphrase, wantErr: `egress-allow "0.0.0.0/0": a range of every address`},
		{name: "every IPv6 address allowed", env: withPassphrase(map[string]string{envEgressAllow: "127.0.0.1/32,::/0"}), args: dev(), wantErr: `egress-allow "::/0": a range of every address`},
		{name: "this network allowed", args: dev("--egress-allow", "0.0.0.0/8"), env: passphrase, wantErr: "never connected to"},
		{name: "multicast allowed", args: dev("--egress-allow", "224.0.0.0/4"), env: passphrase, wantErr: "never connected to"},
		{name: "multicast of IPv6 allowed", args: dev("--egress-allow", "ff00::/8"), env: passphrase, wantErr: "never connected to"},
		{name: "the well-known NAT64 prefix allowed", args: dev("--egress-allow", "64:ff9b::/96"), env: passphrase, wantErr: "never connected to"},
		{name: "a delivery timeout too short", args: dev("--delivery-timeout", "999ms"), env: passphrase, wantErr: "delivery-timeout 999ms: must be between 1s and 5m"},
		{name: "a delivery timeout too long", args: dev("--delivery-timeout", "5m1s"), env: passphrase, wantErr: "delivery-timeout"},
		{name: "no replication workers", args: dev("--replication-workers", "0"), env: passphrase, wantErr: "replication-workers 0: must be between 1 and 32"},
		{name: "too many replication workers", env: withPassphrase(map[string]string{envReplWorkers: "33"}), args: dev(), wantErr: "replication-workers 33"},
		{name: "a negative replication rate", args: dev("--replication-rate-mib", "-1"), env: passphrase, wantErr: "replication-rate-mib -1: must be between 0"},
		{name: "a replication rate too high", args: dev("--replication-rate-mib", "1048577"), env: passphrase, wantErr: "replication-rate-mib"},
		{name: "bad replication workers variable", env: withPassphrase(map[string]string{envReplWorkers: "many"}), wantErr: envReplWorkers},
		{
			name: "certificates read again more often, by flag over variable", args: dev("--tls-reload-interval", "5s"),
			env:  withPassphrase(map[string]string{envTLSReload: "10m"}),
			want: base(func(c *serverConfig) { c.TLSReloadInterval = 5 * time.Second }),
		},
		{
			name: "certificates read again less often, by variable", args: dev(), env: withPassphrase(map[string]string{envTLSReload: "10m"}),
			want: base(func(c *serverConfig) { c.TLSReloadInterval = 10 * time.Minute }),
		},
		{
			name: "certificates never read again", args: dev("--tls-reload-interval", "0"), env: passphrase,
			want: base(func(c *serverConfig) { c.TLSReloadInterval = 0 }),
		},
		{name: "certificates read again too often", args: dev("--tls-reload-interval", "4s"), env: passphrase, wantErr: "TLS reload interval"},
		{name: "negative TLS reload interval", args: dev("--tls-reload-interval", "-1m"), env: passphrase, wantErr: "TLS reload interval"},
		{name: "bad TLS reload interval variable", env: withPassphrase(map[string]string{envTLSReload: "often"}), wantErr: envTLSReload},
		{name: "negative scrub rate", args: dev("--scrub-rate-mib", "-1"), env: passphrase, wantErr: "must not be negative"},
		{name: "grace period too short to be safe", args: dev("--gc-grace", "59m"), env: passphrase, wantErr: "grace period"},
		{
			name: "drive replacement confirmed", args: dev("--allow-drive-replacement"), env: passphrase,
			want: base(func(c *serverConfig) { c.AllowDriveReplacement = true }),
		},
		{
			name: "drive replacement confirmed in the environment", args: dev(), env: withPassphrase(map[string]string{envAllowReplace: "true"}),
			want: base(func(c *serverConfig) { c.AllowDriveReplacement = true }),
		},
		{name: "bad replacement variable", env: withPassphrase(map[string]string{envAllowReplace: "perhaps"}), wantErr: envAllowReplace},
		{name: "bad scrub rate variable", env: withPassphrase(map[string]string{envScrubRate: "fast"}), wantErr: envScrubRate},
		{name: "bad scrub interval variable", env: withPassphrase(map[string]string{envScrubInterval: "often"}), wantErr: envScrubInterval},
		{name: "bad collection interval variable", env: withPassphrase(map[string]string{envGCInterval: "often"}), wantErr: envGCInterval},
		{name: "bad grace variable", env: withPassphrase(map[string]string{envGCGrace: "long"}), wantErr: envGCGrace},
		{name: "bad boolean variable", env: withPassphrase(map[string]string{envDevPlaintext: "maybe"}), wantErr: envDevPlaintext},
		{name: "bad duration variable", env: withPassphrase(map[string]string{envShutdownTimeout: "soon"}), wantErr: envShutdownTimeout},
		{name: "bad drain delay variable", env: withPassphrase(map[string]string{envDrainDelay: "soon"}), wantErr: envDrainDelay},
		{name: "negative drain delay", args: dev("--drain-delay", "-1s"), env: passphrase, wantErr: "must not be negative"},
		{name: "bad log level", args: dev("--log-level", "loud"), env: passphrase, wantErr: "log level"},
		{name: "bad admin address", args: dev("--admin-addr", "nonsense"), env: passphrase, wantErr: "admin address"},
		{name: "bad S3 address", args: dev("--s3-addr", "nonsense"), env: passphrase, wantErr: "S3 address"},
		{name: "non-positive shutdown timeout", args: dev("--shutdown-timeout", "0s"), env: passphrase, wantErr: "must be positive"},
		{name: "stray argument", args: dev("extra"), env: passphrase, wantErr: "unexpected argument"},
		{name: "unknown flag", args: []string{"--nope"}, env: passphrase, wantErr: errFlagParse.Error()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseServerConfig(tt.args, envMap(tt.env), io.Discard)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				// Whatever is wrong, a secret is never echoed back.
				for _, secret := range []string{"unit test passphrase", "root-secret-value"} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("the error message contains a secret: %q", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("config =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

func TestSecretsHaveNoFlags(t *testing.T) {
	t.Parallel()

	// A command line is visible to every user of the machine, so no secret
	// may be accepted on it.
	var usageText bytes.Buffer
	_, err := parseServerConfig([]string{"--help"}, noEnv, &usageText)
	if err == nil {
		t.Fatal("--help did not stop parsing")
	}
	for _, forbidden := range []string{"-kms-passphrase ", "-root-secret", "-root-access-key", "-secret"} {
		if strings.Contains(usageText.String(), forbidden) {
			t.Errorf("server has a flag that takes a secret: %q", forbidden)
		}
	}
	for _, flagName := range []string{"-data", "-meta-dir", "-kms", "-kms-key-file", "-kms-passphrase-file", "-s3-addr", "-s3-domain", "-region"} {
		if !strings.Contains(usageText.String(), flagName+" ") {
			t.Errorf("usage does not document %s", flagName)
		}
	}
}
