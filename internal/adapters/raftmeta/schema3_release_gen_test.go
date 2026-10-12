//go:build fixturegen

package raftmeta

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// TestWriteSchema3ReleaseEvidence writes the log of level 3 and the state the
// state machine of the tree it is run in makes of it, into the directory
// $CACTUSFS_SCHEMA3_RELEASE_OUT. It was run once, in an export of commit
// 0b7d825 - the release of schema 3 - into which this file and
// schema3_release_test.go had been copied; the command lines are in the
// comment at the head of that file. What it wrote is in testdata.
//
// It must not be run on a later tree to replace those files: they are what
// THAT code made, and TestThisReleaseAppliesALogOfLevel3AsTheSchema3ReleaseDoes
// holds this tree to them. It is kept so that it can be seen how they came to
// be, writes nowhere unless it is told where, and refuses to write over a
// file that exists.
func TestWriteSchema3ReleaseEvidence(t *testing.T) {
	dir := os.Getenv("CACTUSFS_SCHEMA3_RELEASE_OUT")
	if dir == "" {
		t.Fatal("CACTUSFS_SCHEMA3_RELEASE_OUT names no directory: the evidence is written once, by the release it is evidence of, and not into this tree")
	}
	if MetadataVersion != entrySchema3 {
		t.Fatalf("this tree is of metadata version %d: the evidence is what the release of schema %d makes", MetadataVersion, entrySchema3)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	log := schema3ReleaseLog(t)
	lines := make([]string, len(log))
	for i, data := range log {
		lines[i] = hex.EncodeToString(data)
	}
	for name, content := range map[string][]byte{
		schema3ReleaseLogFile:   []byte(strings.Join(lines, "\n") + "\n"),
		schema3ReleaseStateFile: schema3ReleaseState(t, log, 0),
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
