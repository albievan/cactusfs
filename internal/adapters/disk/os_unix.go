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

// fileSystemUsage returns the room on the file system the directory behind
// root is on. An os.Root does not give out its descriptor, so the directory
// is opened once more through it - relative to the descriptor, not by name -
// and that one is asked.
func fileSystemUsage(root *os.Root) (usage Usage, err error) {
	dir, err := root.Open(".")
	if err != nil {
		return Usage{}, err
	}
	defer func() {
		if closeErr := dir.Close(); err == nil && closeErr != nil {
			usage, err = Usage{}, closeErr
		}
	}()
	var stat syscall.Statfs_t
	if err := syscall.Fstatfs(int(dir.Fd()), &stat); err != nil {
		return Usage{}, fmt.Errorf("fstatfs: %w", err)
	}
	// The fields are integers of a width that differs from one system to
	// the next; as floating-point numbers they are the same everywhere.
	block := float64(stat.Bsize)
	return Usage{FreeBytes: float64(stat.Bavail) * block, TotalBytes: float64(stat.Blocks) * block}, nil
}
