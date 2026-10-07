//go:build !windows

package fsutil

import (
	"fmt"
	"os"
)

// SyncDir flushes the directory name inside root, which makes the creation,
// rename or removal of an entry in it durable.
func SyncDir(root *os.Root, name string) error {
	dir, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}
