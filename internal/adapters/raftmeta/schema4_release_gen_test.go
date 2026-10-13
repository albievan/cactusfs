//go:build fixturegen

package raftmeta

import (
	"os"
	"testing"
)

// TestWriteSchema4ReleaseEvidence writes the sums of the log of level 4 and
// of the state the state machine of the tree it is run in makes of it, and
// the bytes of every entry of schema 4 and below, into
// the directory $CACTUSFS_SCHEMA4_RELEASE_OUT. It was run in an export of
// commit ddbfe43 - the last tree whose state machine knows nothing of schema
// 5 - into which this file and schema4_release_test.go had been copied; the
// command lines are in the comment at the head of that file. What it wrote
// is in testdata.
//
// It must not be run on a later tree to replace those files: they are what
// THAT code made, and TestThisReleaseAppliesALogOfLevel4AsTheSchema4ReleaseDoes
// holds this tree to it. It is kept so that it can be seen how the file came
// to be, writes nowhere unless it is told where, and refuses to write over a
// file that exists.
func TestWriteSchema4ReleaseEvidence(t *testing.T) {
	dir := os.Getenv("CACTUSFS_SCHEMA4_RELEASE_OUT")
	if dir == "" {
		t.Fatal("CACTUSFS_SCHEMA4_RELEASE_OUT names no directory: the evidence is written once, by the release it is evidence of, and not into this tree")
	}
	if MetadataVersion != entrySchema4 {
		t.Fatalf("this tree is of metadata version %d: the evidence is what the release of schema %d makes", MetadataVersion, entrySchema4)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for name, content := range map[string][]byte{
		schema4ReleaseFile: schema4ReleaseSums(t, schema4ReleaseLog(t), 0),
		schema4EntriesFile: schema4EntryBytes(t),
	} {
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
