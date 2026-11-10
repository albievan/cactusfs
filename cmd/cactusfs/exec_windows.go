//go:build windows

package main

import "errors"

// reexecSupported says that a process cannot execute another program in its
// own place on Windows: a replaced binary runs once the service manager has
// restarted the node (ADR-0029).
const reexecSupported = false

// reexec is never called on Windows (reexecSupported), and fails if it is.
func reexec(string, []string, []string) error {
	return errors.New("a process cannot execute itself again on Windows")
}
