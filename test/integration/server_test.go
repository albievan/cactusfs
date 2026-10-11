//go:build integration && !windows

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const modulePath = "github.com/albievan/cactusfs"

// buildBinary compiles cmd/cactusfs exactly as a user would and returns the
// path of the executable.
func buildBinary(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "cactusfs")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", bin, modulePath+"/cmd/cactusfs")
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func TestVersionCommand(t *testing.T) {
	t.Parallel()

	out, err := exec.CommandContext(t.Context(), buildBinary(t), "version").Output()
	if err != nil {
		t.Fatalf("cactusfs version: %v", err)
	}
	if !strings.HasPrefix(string(out), "cactusfs ") {
		t.Errorf("output = %q, want it to start with %q", out, "cactusfs ")
	}
}

func TestServerRefusesToStartWithoutTLS(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), buildBinary(t), "server")
	cmd.Stderr = &stderr
	err := cmd.Run()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("cactusfs server: err = %v, want exit code 2", err)
	}
	if !strings.Contains(stderr.String(), "TLS is required") {
		t.Errorf("stderr = %q, want it to explain that TLS is required", stderr.String())
	}
}

// startServer runs "cactusfs server" for development against dir and returns
// the process, its JSON log and what it wrote to standard error once it has
// stopped.
type server struct {
	cmd    *exec.Cmd
	logs   *bufio.Scanner
	stderr *lockedBuffer
}

// lockedBuffer collects what a process writes while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func startServer(ctx context.Context, t *testing.T, bin, dir string, extraEnv ...string) *server {
	t.Helper()
	return startServerWith(ctx, t, bin, []string{
		"server",
		"--data", filepath.Join(dir, "drive1"), "--data", filepath.Join(dir, "drive2"), "--kms", "local",
		"--admin-addr", "127.0.0.1:0", "--s3-addr", "127.0.0.1:0",
	}, append([]string{"CACTUSFS_DEV_PLAINTEXT=true", "CACTUSFS_KMS_PASSPHRASE=integration test passphrase"}, extraEnv...)...)
}

// startServerWith runs the binary with the given arguments and environment.
func startServerWith(ctx context.Context, t *testing.T, bin string, args []string, env ...string) *server {
	t.Helper()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(cmd.Environ(), env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	s := &server{cmd: cmd, logs: bufio.NewScanner(stdout), stderr: &lockedBuffer{}}
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return s
}

// stop sends SIGTERM and waits for a clean exit.
func (s *server) stop(t *testing.T) {
	t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	var sawStopped bool
	for s.logs.Scan() {
		var entry struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal(s.logs.Bytes(), &entry); err != nil {
			t.Errorf("log line is not JSON: %q", s.logs.Text())
			continue
		}
		if entry.Msg == "stopped" {
			sawStopped = true
		}
	}
	if err := s.cmd.Wait(); err != nil {
		t.Fatalf("server did not exit cleanly on SIGTERM: %v\n%s", err, s.stderr.String())
	}
	if !sawStopped {
		t.Error("server exited without logging a clean stop")
	}
}

func TestServerProbesAndGracefulShutdown(t *testing.T) {
	t.Parallel()

	// Built before the clock of the test starts: on a cold cache the build
	// alone can take longer than the test may.
	bin, dir := buildBinary(t), t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	first := startServer(ctx, t, bin, dir)
	addrs := waitForListening(t, first.logs)

	client := &http.Client{Timeout: 5 * time.Second}
	for path, want := range map[string]int{
		"/healthz": http.StatusOK,
		"/readyz":  http.StatusOK,
		// Built with Go alone: no console, and a page that says so.
		"/":           http.StatusOK,
		"/index.html": http.StatusNotFound,
		"/docs":       http.StatusOK,
	} {
		if got := status(ctx, t, client, "http://"+addrs["admin"]+path); got != want {
			t.Errorf("GET %s: status = %d, want %d", path, got, want)
		}
	}
	// Metrics are served only to an authenticated caller.
	if got := status(ctx, t, client, "http://"+addrs["admin"]+"/metrics"); got != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /metrics: status = %d, want 401", got)
	}
	// The S3 listener answers, and refuses a request that is not signed.
	if got := status(ctx, t, client, "http://"+addrs["s3"]+"/"); got != http.StatusForbidden {
		t.Errorf("unsigned S3 request: status = %d, want 403", got)
	}
	first.stop(t)

	// A new deployment shows generated root credentials once, on standard
	// error; a restart shows nothing.
	shown := regexp.MustCompile(`Access key ID: (\S+)\s+Secret key: +(\S+)`).FindStringSubmatch(first.stderr.String())
	if shown == nil {
		t.Fatalf("first start did not show root credentials: %q", first.stderr.String())
	}
	second := startServer(ctx, t, bin, dir)
	addrs = waitForListening(t, second.logs)

	// The credentials shown at the first start open the metrics endpoint
	// after a restart, and the metrics are the node's own.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addrs["admin"]+"/metrics", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+shown[1]+":"+shown[2])
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK ||
		!strings.Contains(string(body), "cactusfs_build_info") || !strings.Contains(string(body), `cactusfs_port_calls_total{method="GetRootCredential"`) {
		t.Errorf("authenticated GET /metrics: status %d, %v, body %.300q", resp.StatusCode, err, body)
	}
	second.stop(t)
	if second.stderr.Len() != 0 {
		t.Errorf("a restart wrote to standard error: %q", second.stderr.String())
	}
}

func TestServerRefusesToStartWithoutAKMS(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(t.Context(), buildBinary(t), "server", "--dev-plaintext", "--data", t.TempDir())
	cmd.Stderr = &stderr
	err := cmd.Run()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("cactusfs server: err = %v, want exit code 2", err)
	}
	if !strings.Contains(stderr.String(), "no unencrypted mode") {
		t.Errorf("stderr = %q, want it to say there is no unencrypted mode", stderr.String())
	}
}

// waitForListening reads JSON log lines until both listeners have reported
// their addresses, and returns them by listener name.
func waitForListening(t *testing.T, logs *bufio.Scanner) map[string]string {
	t.Helper()

	addrs := map[string]string{}
	for logs.Scan() {
		var entry struct {
			Msg      string `json:"msg"`
			Listener string `json:"listener"`
			Addr     string `json:"addr"`
		}
		if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", logs.Text())
		}
		if entry.Msg == "listening" {
			addrs[entry.Listener] = entry.Addr
		}
		if addrs["admin"] != "" && addrs["s3"] != "" {
			return addrs
		}
	}
	t.Fatalf("server exited before listening: %v", logs.Err())
	return nil
}

func status(ctx context.Context, t *testing.T, client *http.Client, url string) int {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode
}
