//go:build windows

package fsutil

import "os"

// SyncDir does nothing on Windows: a directory handle cannot be flushed, and
// NTFS journals metadata changes such as creations and renames. File contents
// are still flushed with FlushFileBuffers before they are relied on (see
// ADR-0001).
func SyncDir(*os.Root, string) error {
	return nil
}
