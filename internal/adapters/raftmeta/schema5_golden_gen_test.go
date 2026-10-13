//go:build fixturegen

package raftmeta

import (
	"os"
	"testing"
)

// TestWriteSchema5Golden writes the golden files of schema 5 that this
// package pins - the entries (schema5EntryBytes), the stored usage records
// (usageGolden) and every other record of schema 5 (schema5RecordBytes) -
// into the directory $CACTUSFS_SCHEMA5_GOLDEN_OUT. It was run when the codec
// and the state machine of schema 5 were written (PLAN 8.0.6 to 8.0.9),
// before any binary announced schema 5: the entries were written again at
// PLAN 8.0.8, when the path of a target's endpoint was ruled to be sealed,
// and the records after the review of 8.0.8 and 8.0.9, when the state came
// to keep which buckets name a target ("targetrefs": three lines more, and
// every line that was there the same bytes).
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
	files := map[string][]byte{schema5EntriesFile: schema5EntryBytes(t), schema5RecordsFile: schema5RecordBytes(t)}
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
