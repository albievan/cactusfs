package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeTestFile writes a file through an os.Root, like the code under test
// reads it.
func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Dir(path), err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(filepath.Base(path), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fileExists reports whether path exists.
func fileExists(path string) (bool, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	_, err = root.Lstat(filepath.Base(path))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
