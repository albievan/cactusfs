//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// SyncDir flushes the directory name inside root, which makes the creation,
// rename or removal of an entry in it durable.
//
// NTFS records a rename in its log, but the log is written lazily: until it
// is flushed, a power loss can undo a rename that has already returned. A
// flush of the directory forces it out. FlushFileBuffers needs a handle with
// write access, and os.Root opens directories for reading only, so the
// directory is first opened through root - which keeps the name confined to
// it - and that handle is then reopened for writing by a relative open with
// an empty name, which names the directory itself. No path is built outside
// root (see ADR-0001).
func SyncDir(root *os.Root, name string) (err error) {
	dir, err := root.Open(name)
	if err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	defer func() {
		if closeErr := dir.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("sync directory: %w", closeErr)
		}
	}()
	info, err := dir.Stat()
	if err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("sync directory: %s is not a directory", name)
	}

	attrs := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(dir.Fd()),
		ObjectName:    &windows.NTUnicodeString{},
	}
	attrs.Length = uint32(unsafe.Sizeof(*attrs))
	var handle windows.Handle
	if err := windows.NtCreateFile(
		&handle,
		windows.SYNCHRONIZE|windows.FILE_WRITE_DATA|windows.FILE_APPEND_DATA,
		attrs,
		&windows.IO_STATUS_BLOCK{},
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT,
		0,
		0,
	); err != nil {
		return fmt.Errorf("sync directory: reopen %s for writing: %w", name, err)
	}
	flushErr := windows.FlushFileBuffers(handle)
	closeErr := windows.CloseHandle(handle)
	if err := errors.Join(flushErr, closeErr); err != nil {
		return fmt.Errorf("sync directory: FlushFileBuffers %s: %w", name, err)
	}
	return nil
}
