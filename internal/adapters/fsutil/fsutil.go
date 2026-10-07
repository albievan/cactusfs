// Package fsutil holds the few filesystem operations whose behaviour differs
// between operating systems and that more than one adapter needs. The
// OS-specific parts are in the *_unix.go and *_windows.go files.
package fsutil

import (
	"fmt"
	"os"
)

// WriteNewDurable creates the file name inside root, which must not exist,
// writes data, flushes it to stable storage and makes its directory entry
// durable. When it returns nil the file survives a power loss.
func WriteNewDurable(root *os.Root, name string, data []byte, mode os.FileMode) error {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	return SyncDir(root, ".")
}
