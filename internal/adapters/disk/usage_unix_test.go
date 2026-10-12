//go:build !windows

package disk_test

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUsageIsOfTheDirectoryTheDriveWritesTo: the drive is asked through the
// handle it was opened with, not by its name. A drive whose directory was
// moved, with another put where it was, is still the one that answers; one
// whose directory is gone is an error. (Windows does not let a directory be
// moved or removed while a file in it is open, as the drive's lock is.)
func TestUsageIsOfTheDirectoryTheDriveWritesTo(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "drive")
	drive := openDrive(t, dir)

	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	// An empty directory under the old name: it has no drive file, and a
	// look by name would not find the drive.
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	usage, err := drive.Usage()
	if err != nil {
		t.Fatalf("Usage of a drive whose directory was moved: %v", err)
	}
	if usage.TotalBytes <= 0 {
		t.Errorf("Usage = %+v", usage)
	}

	if err := os.RemoveAll(moved); err != nil {
		t.Fatal(err)
	}
	if _, err := drive.Usage(); err == nil {
		t.Error("Usage of a drive that is gone succeeded")
	}
}
