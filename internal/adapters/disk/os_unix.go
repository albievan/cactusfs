//go:build !windows

package disk

import (
	"fmt"
	"os"
	"syscall"
)

// lockExclusive takes an exclusive, non-blocking advisory lock on f. The lock
// is released when f is closed or the process exits.
func lockExclusive(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	return nil
}

func unlock(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("flock: %w", err)
	}
	return nil
}
