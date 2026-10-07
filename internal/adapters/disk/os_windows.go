//go:build windows

package disk

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// lockExclusive takes an exclusive, non-blocking lock on f. The lock is
// released when f is closed or the process exits.
func lockExclusive(f *os.File) error {
	var overlapped windows.Overlapped
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &overlapped); err != nil {
		return fmt.Errorf("LockFileEx: %w", err)
	}
	return nil
}

func unlock(f *os.File) error {
	var overlapped windows.Overlapped
	if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped); err != nil {
		return fmt.Errorf("UnlockFileEx: %w", err)
	}
	return nil
}

// syncDir does nothing on Windows: a directory handle cannot be flushed, and
// NTFS journals metadata changes such as renames. File contents are still
// flushed with FlushFileBuffers before a rename (see ADR-0001).
func syncDir(*os.Root, string) error {
	return nil
}
