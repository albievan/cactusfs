//go:build fixturegen

package raftmeta

import (
	"os"
	"testing"
)

// TestWriteSchema5Golden writes the golden files of schema 5 that this
// package pins - the entries (schema5EntryBytes) and the stored usage
// records (usageGolden) - into the directory $CACTUSFS_SCHEMA5_GOLDEN_OUT.
// It was run when the codec and the state machine of schema 5 were written
// (PLAN 8.0.6, 8.0.7), before any binary announced schema 5.
//
// Once schema 5 is announced (PLAN 8.0.10) it must not be run to replace
// those files: a change of them is then a change of what two releases write,
// and needs a rise of the entry schema. It is kept so that it can be seen how
// the files came to be, writes nowhere unless it is told where, and refuses
// to write over a file that exists.
func TestWriteSchema5Golden(t *testing.T) {
	dir := os.Getenv("CACTUSFS_SCHEMA5_GOLDEN_OUT")
	if dir == "" {
		t.Fatal("CACTUSFS_SCHEMA5_GOLDEN_OUT names no directory: the golden files are not written into this tree")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	files := map[string][]byte{schema5EntriesFile: schema5EntryBytes(t)}
	for name, record := range usageGolden(t) {
		files[name] = append(record, '\n')
	}
	for name, content := range files {
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := file.Write(content); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
