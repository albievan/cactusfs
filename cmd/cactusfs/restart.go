package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// restarter carries a request of the node to restart into its executable:
// the binary upgrader (PLAN P.7) replaces the executable and asks; the
// server then stops as it does on SIGTERM, and main, once the server has
// returned cleanly - every listener, Raft and bbolt closed - executes the
// executable again with the same arguments and environment (exec_unix.go).
// Windows cannot: there the request is never offered (reexecSupported).
type restarter struct {
	// executable is the path of the running executable, symbolic links
	// resolved, read when the process started - before an upgrade could
	// move the file it was started from aside; empty when it could not be
	// read.
	executable string

	mu        sync.Mutex
	requested bool
	cancel    context.CancelFunc
}

// newRestarter returns a restarter of the executable at path.
func newRestarter(executable string) *restarter {
	return &restarter{executable: executable}
}

// executablePath returns the path of the running executable with symbolic
// links resolved, or "" when it cannot be read.
func executablePath() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return resolved
}

// bind returns a context of ctx that a request ends. The returned function
// releases it.
func (r *restarter) bind(ctx context.Context) (context.Context, context.CancelFunc) {
	bound, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()
	return bound, cancel
}

// attach gives the node's configuration the executable and, where a
// restart can be honoured, the request; it returns the context the node
// runs under, which a request ends as SIGTERM does, and the function that
// releases it. A nil restarter gives nothing, and ctx is returned as it is.
func (r *restarter) attach(ctx context.Context, cfg *serverConfig) (context.Context, context.CancelFunc) {
	if r == nil {
		return ctx, func() {}
	}
	cfg.executable = r.executable
	if r.available() {
		cfg.restart = r.Request
	}
	return r.bind(ctx)
}

// available reports whether a request can be honoured: the platform can
// execute a process again in place and the executable's path is known.
func (r *restarter) available() bool {
	return r != nil && reexecSupported && r.executable != ""
}

// Request asks the node to stop gracefully and restart into its executable.
// It returns at once; a second request is the first.
func (r *restarter) Request() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requested = true
	if r.cancel != nil {
		r.cancel()
	}
}

// Requested reports whether a restart was asked for.
func (r *restarter) Requested() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requested
}

// afterRun decides how the process ends once the command has returned code:
// it executes the executable again when a restart was asked for, the
// command ended cleanly and the process was not told to stop (signalled) -
// an operator's SIGTERM wins over a restart. exec replaces the process and
// returns only when it could not; the code is then exitError.
func afterRun(code int, signalled bool, r *restarter, args, env []string, exec func(path string, argv, env []string) error, stderr io.Writer) int {
	if code != exitOK || signalled || !r.Requested() || !r.available() {
		return code
	}
	err := exec(r.executable, args, env)
	if err == nil {
		return exitOK
	}
	_, _ = fmt.Fprintf(stderr, "cactusfs: the node stopped to restart, and could not: %v\n", err)
	return exitError
}
