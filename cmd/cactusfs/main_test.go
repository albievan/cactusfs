package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"
)

func noEnv(string) string { return "" }

func envMap(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "no command", args: nil, wantCode: exitUsage, wantStderr: "Usage:"},
		{name: "unknown command", args: []string{"frobnicate"}, wantCode: exitUsage, wantStderr: `unknown command "frobnicate"`},
		{name: "help", args: []string{"help"}, wantCode: exitOK, wantStdout: "Usage:"},
		{name: "help flag", args: []string{"--help"}, wantCode: exitOK, wantStdout: "Usage:"},
		{name: "version", args: []string{"version"}, wantCode: exitOK, wantStdout: "cactusfs " + version + " (" + runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH + ")"},
		{name: "server help", args: []string{"server", "--help"}, wantCode: exitOK, wantStderr: "-admin-addr"},
		{name: "server unknown flag", args: []string{"server", "--nope"}, wantCode: exitUsage, wantStderr: "flag provided but not defined"},
		{name: "server without TLS refuses to start", args: []string{"server"}, wantCode: exitUsage, wantStderr: "TLS is required"},
		{
			name: "server with unreadable certificate fails", args: []string{"server", "--tls-cert", "missing.pem", "--tls-key", "missing.key"},
			wantCode: exitError, wantStderr: "server.LoadTLS",
		},
		{
			name: "server with unusable address fails", args: []string{"server", "--dev-plaintext", "--admin-addr", "127.0.0.1:99999"},
			wantCode: exitError, wantStderr: "server.Listen",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			code := run(t.Context(), tt.args, &stdout, &stderr, noEnv)

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
	args := []string{"server", "--dev-plaintext", "--admin-addr", "127.0.0.1:0"}
	code := run(ctx, args, cancelOnListen{cancel: cancel}, &stderr, noEnv)

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}
}

func TestParseServerConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    serverConfig
		wantErr string
	}{
		{name: "defaults need TLS", wantErr: "TLS is required"},
		{name: "certificate without key", args: []string{"--tls-cert", "c.pem"}, wantErr: "TLS is required"},
		{
			name: "TLS with defaults",
			args: []string{"--tls-cert", "c.pem", "--tls-key", "k.pem"},
			want: serverConfig{AdminAddr: ":9001", TLSCert: "c.pem", TLSKey: "k.pem", LogLevel: slog.LevelInfo, ShutdownTimeout: 30 * time.Second},
		},
		{
			name: "plaintext defaults to loopback",
			args: []string{"--dev-plaintext"},
			want: serverConfig{AdminAddr: "127.0.0.1:9001", DevPlaintext: true, LogLevel: slog.LevelInfo, ShutdownTimeout: 30 * time.Second},
		},
		{
			name: "plaintext on IPv6 loopback",
			args: []string{"--dev-plaintext", "--admin-addr", "[::1]:9001"},
			want: serverConfig{AdminAddr: "[::1]:9001", DevPlaintext: true, LogLevel: slog.LevelInfo, ShutdownTimeout: 30 * time.Second},
		},
		{name: "plaintext on all interfaces", args: []string{"--dev-plaintext", "--admin-addr", "0.0.0.0:9001"}, wantErr: "loopback IP address only"},
		{name: "plaintext on a routable address", args: []string{"--dev-plaintext", "--admin-addr", "192.0.2.10:9001"}, wantErr: "loopback IP address only"},
		{name: "plaintext on a host name", args: []string{"--dev-plaintext", "--admin-addr", "localhost:9001"}, wantErr: "loopback IP address only"},
		{name: "plaintext with certificate", args: []string{"--dev-plaintext", "--tls-cert", "c.pem", "--tls-key", "k.pem"}, wantErr: "cannot be combined"},
		{
			name: "environment",
			env: map[string]string{
				envAdminAddr: "127.0.0.1:19001", envDevPlaintext: "true", envLogLevel: "debug", envShutdownTimeout: "5s",
				envDrainDelay: "3s",
			},
			want: serverConfig{
				AdminAddr: "127.0.0.1:19001", DevPlaintext: true, LogLevel: slog.LevelDebug,
				DrainDelay: 3 * time.Second, ShutdownTimeout: 5 * time.Second,
			},
		},
		{
			name: "environment TLS",
			env:  map[string]string{envTLSCert: "c.pem", envTLSKey: "k.pem"},
			want: serverConfig{AdminAddr: ":9001", TLSCert: "c.pem", TLSKey: "k.pem", LogLevel: slog.LevelInfo, ShutdownTimeout: 30 * time.Second},
		},
		{
			name: "flag wins over environment",
			args: []string{
				"--admin-addr", "127.0.0.1:2", "--log-level", "error", "--shutdown-timeout", "1s", "--dev-plaintext=true",
				"--drain-delay", "2s",
			},
			env: map[string]string{
				envAdminAddr: "127.0.0.1:1", envLogLevel: "debug", envShutdownTimeout: "5s", envDevPlaintext: "false",
				envDrainDelay: "9s",
			},
			want: serverConfig{
				AdminAddr: "127.0.0.1:2", DevPlaintext: true, LogLevel: slog.LevelError,
				DrainDelay: 2 * time.Second, ShutdownTimeout: time.Second,
			},
		},
		{name: "bad boolean variable", env: map[string]string{envDevPlaintext: "maybe"}, wantErr: envDevPlaintext},
		{name: "bad duration variable", env: map[string]string{envShutdownTimeout: "soon"}, wantErr: envShutdownTimeout},
		{name: "bad log level", args: []string{"--dev-plaintext", "--log-level", "loud"}, wantErr: "log level"},
		{name: "bad address", args: []string{"--dev-plaintext", "--admin-addr", "nonsense"}, wantErr: "admin address"},
		{name: "bad drain delay variable", env: map[string]string{envDrainDelay: "soon"}, wantErr: envDrainDelay},
		{name: "negative drain delay", args: []string{"--dev-plaintext", "--drain-delay", "-1s"}, wantErr: "must not be negative"},
		{name: "non-positive shutdown timeout", args: []string{"--dev-plaintext", "--shutdown-timeout", "0s"}, wantErr: "must be positive"},
		{name: "stray argument", args: []string{"--dev-plaintext", "extra"}, wantErr: "unexpected argument"},
		{name: "unknown flag", args: []string{"--nope"}, wantErr: errFlagParse.Error()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseServerConfig(tt.args, envMap(tt.env), io.Discard)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("config = %+v, want %+v", got, tt.want)
			}
		})
	}
}
