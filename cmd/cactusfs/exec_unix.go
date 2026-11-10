//go:build !windows

package main

import (
	"fmt"
	"syscall"
)

// reexecSupported says that a process can execute another program in its
// own place here.
const reexecSupported = true

// reexec replaces the process with the program at path, given argv and env.
// It returns only when it could not.
func reexec(path string, argv, env []string) error {
	if err := syscall.Exec(path, argv, env); err != nil {
		return fmt.Errorf("exec %s: %w", path, err)
	}
	return nil
}
