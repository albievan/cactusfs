package disk_test

import (
	"path/filepath"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/disk"
)

// TestUsage: a drive says how much room its file system has, for as long as
// it is open; a drive that is closed cannot be reached, and says so.
func TestUsage(t *testing.T) {
	t.Parallel()
	drive := openDrive(t, filepath.Join(t.TempDir(), "drive"))

	usage, err := drive.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage.TotalBytes <= 0 || usage.FreeBytes < 0 || usage.FreeBytes > usage.TotalBytes {
		t.Errorf("Usage = %+v, want room within the size of a file system", usage)
	}

	// A gauge may ask at any time.
	if err := drive.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := drive.Usage(); err == nil {
		t.Error("Usage of a drive that is closed succeeded")
	}
}

// TestUsageOf: a directory that is no drive says how much room its file
// system has; one that is not there says so.
func TestUsageOf(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	usage, err := disk.UsageOf(dir)
	if err != nil {
		t.Fatalf("UsageOf: %v", err)
	}
	if usage.TotalBytes <= 0 || usage.FreeBytes < 0 || usage.FreeBytes > usage.TotalBytes {
		t.Errorf("UsageOf = %+v, want room within the size of a file system", usage)
	}
	if _, err := disk.UsageOf(filepath.Join(dir, "absent")); err == nil {
		t.Error("UsageOf a directory that is not there succeeded")
	}
}
