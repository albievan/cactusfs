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
	"strings"
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
	stderr *bytes.Buffer
}

func startServer(ctx context.Context, t *testing.T, bin, dir string, extraEnv ...string) *server {
	t.Helper()

	cmd := exec.CommandContext(ctx, bin, "server",
		"--data", filepath.Join(dir, "drive1"), "--data", filepath.Join(dir, "drive2"), "--kms", "local",
		"--admin-addr", "127.0.0.1:0", "--s3-addr", "127.0.0.1:0")
	cmd.Env = append(cmd.Environ(), "CACTUSFS_DEV_PLAINTEXT=true", "CACTUSFS_KMS_PASSPHRASE=integration test passphrase")
	cmd.Env = append(cmd.Env, extraEnv...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	s := &server{cmd: cmd, logs: bufio.NewScanner(stdout), stderr: &bytes.Buffer{}}
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

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	bin, dir := buildBinary(t), t.TempDir()

	first := startServer(ctx, t, bin, dir)
	addrs := waitForListening(t, first.logs)

	client := &http.Client{Timeout: 5 * time.Second}
	for path, want := range map[string]int{
		"/healthz": http.StatusOK,
		"/readyz":  http.StatusOK,
		"/":        http.StatusNotFound,
	} {
		if got := status(ctx, t, client, "http://"+addrs["admin"]+path); got != want {
			t.Errorf("GET %s: status = %d, want %d", path, got, want)
		}
	}
	// The S3 listener answers, and refuses a request that is not signed.
	if got := status(ctx, t, client, "http://"+addrs["s3"]+"/"); got != http.StatusForbidden {
		t.Errorf("unsigned S3 request: status = %d, want 403", got)
	}
	first.stop(t)

	// A new deployment shows generated root credentials once, on standard
	// error; a restart shows nothing.
	if !strings.Contains(first.stderr.String(), "Access key ID:") {
		t.Errorf("first start did not show root credentials: %q", first.stderr.String())
	}
	second := startServer(ctx, t, bin, dir)
	waitForListening(t, second.logs)
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
