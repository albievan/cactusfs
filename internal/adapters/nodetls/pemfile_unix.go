//go:build unix

package nodetls

import (
	"os"
	"syscall"
)

// openForRead opens a file without waiting for anything: opening a pipe for
// reading otherwise blocks until somebody opens it for writing. The caller
// asks the open file what it is before it reads.
func openForRead(from opener, name string) (*os.File, error) {
	return from.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
