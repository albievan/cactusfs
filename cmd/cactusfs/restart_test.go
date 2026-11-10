package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRestarter(t *testing.T) {
	t.Parallel()

	var none *restarter
	if none.Requested() || none.available() {
		t.Error("a nil restarter was asked, or can restart")
	}

	r := newRestarter("/opt/cactusfs/cactusfs")
	if r.available() != reexecSupported {
		t.Errorf("available = %v, want %v", r.available(), reexecSupported)
	}
	if newRestarter("").available() {
		t.Error("a restarter of no executable can restart")
	}
	// A request before the node runs is kept.
	r.Request()
	if !r.Requested() {
		t.Error("Requested = false after a request")
	}

	r = newRestarter("/opt/cactusfs/cactusfs")
	ctx, release := r.bind(t.Context())
	defer release()
	if ctx.Err() != nil || r.Requested() {
		t.Fatal("bound before any request, and already ended")
	}
	r.Request()
	r.Request()
	if !errors.Is(ctx.Err(), context.Canceled) || !r.Requested() {
		t.Errorf("after a request: ctx %v, requested %v", ctx.Err(), r.Requested())
	}
	if t.Context().Err() != nil {
		t.Error("a request ended the parent context")
	}
}

func TestExecutablePath(t *testing.T) {
	t.Parallel()
	path := executablePath()
	if path == "" || !filepath.IsAbs(path) {
		t.Errorf("executablePath = %q, want an absolute path", path)
	}
	if resolved, err := filepath.EvalSymlinks(path); err != nil || resolved != path {
		t.Errorf("executablePath = %q is not resolved (%q, %v)", path, resolved, err)
	}
}

func TestAfterRun(t *testing.T) {
	t.Parallel()

	args, env := []string{"cactusfs", "server", "--data", "/d"}, []string{"CACTUSFS_X=1"}
	requested := func(path string) *restarter {
		r := newRestarter(path)
		r.Request()
		return r
	}
	tests := []struct {
		name      string
		code      int
		signalled bool
		restart   *restarter
		execErr   error
		wantExec  bool
		wantCode  int
	}{
		{name: "no restarter", code: exitOK, wantCode: exitOK},
		{name: "no request", code: exitOK, restart: newRestarter("/bin/cactusfs"), wantCode: exitOK},
		{name: "a request, and the node failed", code: exitError, restart: requested("/bin/cactusfs"), wantCode: exitError},
		{name: "a request, and the operator's SIGTERM", code: exitOK, signalled: true, restart: requested("/bin/cactusfs"), wantCode: exitOK},
		{name: "a request, and no known executable", code: exitOK, restart: requested(""), wantCode: exitOK},
		{name: "a request, after a clean stop", code: exitOK, restart: requested("/bin/cactusfs"), wantExec: reexecSupported, wantCode: exitOK},
		{
			name: "a request that cannot be honoured", code: exitOK, restart: requested("/bin/cactusfs"), execErr: errors.New("exec format error"),
			wantExec: reexecSupported, wantCode: map[bool]int{true: exitError, false: exitOK}[reexecSupported],
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			executed := false
			exec := func(path string, argv, environ []string) error {
				executed = true
				if path != "/bin/cactusfs" || !slices.Equal(argv, args) || !slices.Equal(environ, env) {
					t.Errorf("exec(%q, %q, %q)", path, argv, environ)
				}
				return tt.execErr
			}
			var stderr bytes.Buffer
			if got := afterRun(tt.code, tt.signalled, tt.restart, args, env, exec, &stderr); got != tt.wantCode {
				t.Errorf("afterRun = %d, want %d", got, tt.wantCode)
			}
			if executed != tt.wantExec {
				t.Errorf("executed = %v, want %v", executed, tt.wantExec)
			}
			if tt.execErr != nil && tt.wantExec && !strings.Contains(stderr.String(), "could not") {
				t.Errorf("stderr = %q", stderr.String())
			}
		})
	}
}

// requestOnListen asks for a restart as soon as the server logs that it is
// listening, as the binary upgrader does once the binary is replaced.
type requestOnListen struct {
	restart *restarter
}

func (r requestOnListen) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"msg":"listening"`)) {
		r.restart.Request()
	}
	return len(p), nil
}

// A request to restart stops the node as SIGTERM does, and cleanly: the
// command returns exitOK, and main may then execute the binary again.
func TestARestartStopsTheServerCleanly(t *testing.T) {
	t.Parallel()

	restart := newRestarter("/opt/cactusfs/cactusfs")
	var stderr bytes.Buffer
	args := append([]string{"server"}, storeArgs(t, "--dev-plaintext", "--admin-addr", "127.0.0.1:0")...)
	code := runCommand(t.Context(), args, requestOnListen{restart: restart}, &stderr, storeEnv(), restart)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}
	if !restart.Requested() {
		t.Error("the restart was not recorded")
	}
	if t.Context().Err() != nil {
		t.Error("the restart ended the process's context")
	}
}

// The executable and the request reach the node's configuration.
func TestAttach(t *testing.T) {
	t.Parallel()

	var none *restarter
	cfg := serverConfig{}
	ctx, release := none.attach(t.Context(), &cfg)
	release()
	if ctx != t.Context() || cfg.executable != "" || cfg.restart != nil {
		t.Error("a nil restarter gave the node something")
	}

	restart := newRestarter("/opt/cactusfs/cactusfs")
	ctx, release = restart.attach(t.Context(), &cfg)
	defer release()
	if cfg.executable != "/opt/cactusfs/cactusfs" {
		t.Errorf("executable = %q", cfg.executable)
	}
	if (cfg.restart != nil) != reexecSupported {
		t.Errorf("restart offered = %v, want %v", cfg.restart != nil, reexecSupported)
	}
	if cfg.restart != nil {
		cfg.restart()
		if ctx.Err() == nil || !restart.Requested() {
			t.Error("the request did not stop the node")
		}
	}

	cfg = serverConfig{}
	_, releaseNone := newRestarter("").attach(t.Context(), &cfg)
	releaseNone()
	if cfg.restart != nil {
		t.Error("a restart was offered with no executable to execute")
	}
}
