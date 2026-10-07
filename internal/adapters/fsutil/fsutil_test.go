package fsutil_test

import (
	"errors"
	"os"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/fsutil"
)

func TestWriteNewDurable(t *testing.T) {
	t.Parallel()

	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	if err := fsutil.WriteNewDurable(root, "key.json", []byte("content"), 0o600); err != nil {
		t.Fatalf("WriteNewDurable: %v", err)
	}
	got, err := root.ReadFile("key.json")
	if err != nil || string(got) != "content" {
		t.Errorf("read back %q, %v", got, err)
	}
	// It never replaces a file.
	if err := fsutil.WriteNewDurable(root, "key.json", []byte("other"), 0o600); !errors.Is(err, os.ErrExist) {
		t.Errorf("second write: error = %v, want %v", err, os.ErrExist)
	}
	if got, _ := root.ReadFile("key.json"); string(got) != "content" {
		t.Errorf("the existing file was changed to %q", got)
	}
	if err := fsutil.WriteNewDurable(root, "missing-dir/key.json", nil, 0o600); err == nil {
		t.Error("writing into a missing directory succeeded")
	}
}

func TestSyncDir(t *testing.T) {
	t.Parallel()

	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir("sub", 0o700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{".", "sub"} {
		if err := fsutil.SyncDir(root, dir); err != nil {
			t.Errorf("SyncDir(%q): %v", dir, err)
		}
	}
}
