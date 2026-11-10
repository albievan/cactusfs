//go:build windows

package binary

// syncDir does nothing on Windows: a directory cannot be opened for
// FlushFileBuffers, and NTFS journals the names of its entries itself.
func syncDir(string) error { return nil }
