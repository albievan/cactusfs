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

func TestServerProbesAndGracefulShutdown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, buildBinary(t), "server", "--admin-addr", "127.0.0.1:0")
	cmd.Env = append(cmd.Environ(), "CACTUSFS_DEV_PLAINTEXT=true")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	logs := bufio.NewScanner(stdout)
	addr := waitForListening(t, logs)

	client := &http.Client{Timeout: 5 * time.Second}
	for path, want := range map[string]int{
		"/healthz": http.StatusOK,
		"/readyz":  http.StatusOK,
		"/":        http.StatusNotFound,
	} {
		if got := status(ctx, t, client, "http://"+addr+path); got != want {
			t.Errorf("GET %s: status = %d, want %d", path, got, want)
		}
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	var sawStopped bool
	for logs.Scan() {
		var entry struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
			t.Errorf("log line is not JSON: %q", logs.Text())
			continue
		}
		if entry.Msg == "stopped" {
			sawStopped = true
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("server did not exit cleanly on SIGTERM: %v", err)
	}
	if !sawStopped {
		t.Error("server exited without logging a clean stop")
	}
}

// waitForListening reads JSON log lines until the server reports its listen
// address.
func waitForListening(t *testing.T, logs *bufio.Scanner) string {
	t.Helper()

	for logs.Scan() {
		var entry struct {
			Msg  string `json:"msg"`
			Addr string `json:"addr"`
		}
		if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", logs.Text())
		}
		if entry.Msg == "listening" {
			return entry.Addr
		}
	}
	t.Fatalf("server exited before listening: %v", logs.Err())
	return ""
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
