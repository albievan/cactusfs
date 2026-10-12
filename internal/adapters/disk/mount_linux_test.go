//go:build linux

package disk_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/disk"
	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// checkFilesystem: on Linux every directory is on a mount of the table, and
// the answer is that mount - also when the directory is reached through a
// symbolic link.
func checkFilesystem(t *testing.T, dir string, fs cluster.Filesystem) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fs.Type == "" || fs.MountPoint == "" || !underMount(fs.MountPoint, resolved) {
		t.Errorf("FilesystemOf(%q) = %+v, want the mount that holds %q", dir, fs, resolved)
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if linked, err := disk.FilesystemOf(link); err != nil || linked != fs {
		t.Errorf("through a symbolic link: %+v, %v; want %+v", linked, err, fs)
	}
	// A directory that is not there has no links to resolve, and is looked
	// up as it is named.
	if absent, err := disk.FilesystemOf(filepath.Join(resolved, "absent", "deeper")); err != nil || absent != fs {
		t.Errorf("a directory that is not there: %+v, %v; want %+v", absent, err, fs)
	}
}
