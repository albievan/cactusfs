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

// fileSystemUsage returns the room on the volume the directory behind root
// is on. It asks by the name the directory was opened under, not through the
// handle: GetDiskFreeSpaceEx, which is what says how much this process may
// still write under a quota, takes a name, and an os.Root does not give out
// its handle. The caller has just found the drive's file through the handle;
// if another directory now has the name, the room reported is that one's.
func fileSystemUsage(root *os.Root) (Usage, error) {
	name, err := windows.UTF16PtrFromString(root.Name())
	if err != nil {
		return Usage{}, fmt.Errorf("GetDiskFreeSpaceEx: %w", err)
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(name, &available, &total, &free); err != nil {
		return Usage{}, fmt.Errorf("GetDiskFreeSpaceEx: %w", err)
	}
	return Usage{FreeBytes: float64(available), TotalBytes: float64(total)}, nil
}
