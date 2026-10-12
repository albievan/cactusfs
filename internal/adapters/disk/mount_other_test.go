//go:build !linux

package disk_test

import (
	"testing"

	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// checkFilesystem: a system without a table of mounts that is read here
// answers nothing, and that is no error.
func checkFilesystem(t *testing.T, dir string, fs cluster.Filesystem) {
	t.Helper()
	if fs != (cluster.Filesystem{}) {
		t.Errorf("FilesystemOf(%q) = %+v, want nothing on this system", dir, fs)
	}
}
