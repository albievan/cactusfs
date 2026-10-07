package disk_test

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

// headerChecksum reproduces the header checksum of the documented format, so
// that a test can forge a well-formed header of another version.
func headerChecksum(header []byte) []byte {
	sum := sha256.Sum256(header)
	return sum[:16]
}

// readFile and writeFile reach test files through an os.Root, like the code
// under test does.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Dir(path), err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Dir(path), err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(filepath.Base(path), data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
