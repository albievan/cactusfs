package raftmeta

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// Entry schema 6 (ADR-0030): the record of the nodes that were taken out of
// the cluster. What it is written as, what a member below level 6 makes of
// it, and the rolling upgrade from the release of schema 5.

// schema6File pins the bytes of the entry and of the record of schema 6.
const schema6File = "schema6.golden.txt"

// newMachine6 is the state machine of a binary whose newest entry schema is
// the sixth: this one (MetadataVersion).
func newMachine6(t *testing.T) *machine {
	t.Helper()
	m := newMachine(t)
	m.f.supported = entrySchema6
	return m
}

// schema6Golden reads the pinned lines, by what they pin.
func schema6Golden(t *testing.T) map[string]string {
	t.Helper()
	raw, err := fs.ReadFile(os.DirFS("testdata"), schema6File)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for line := range strings.Lines(string(raw)) {
		fields := strings.SplitN(strings.TrimSuffix(line, "\n"), "\t", 3)
		if len(fields) != 3 {
			t.Fatalf("a line of the golden file: %q", line)
		}
		out[fields[0]+" "+fields[1]] = fields[2]
	}
	return out
}

// TestTheEntryOfSchema6IsPinned: node.revoke is of schema 6 and of no other,
// is written as the golden file says, and is read back from it as the entry
// it was written from. A member of the release of schema 5 refuses it: as an
// entry of a schema it does not know, and as a command it does not know in
// an entry that claims schema 5.
func TestTheEntryOfSchema6IsPinned(t *testing.T) {
	t.Parallel()
	golden := schema6Golden(t)
	e := entry{Type: cmdRevokeNode, RevokeNode: &revokeNodeCmd{ID: "gateway-1"}}
	if got := versionOf(e); got != entrySchema6 {
		t.Errorf("versionOf(node.revoke) = %d, want %d", got, entrySchema6)
	}
	if got := writtenAt(e, versionOf(e), entrySchema6); got != entrySchema6 {
		t.Errorf("node.revoke is written at level 6 as schema %d", got)
	}
	data, err := encodeEntry(e, entrySchema6)
	if err != nil {
		t.Fatal(err)
	}
	want := golden["entry node.revoke"]
	if string(data[1:]) != want {
		t.Errorf("node.revoke is written as\n %s\nwant\n %s", data[1:], want)
	}
	back, err := decodeEntry(append([]byte{entrySchema6}, want...), entrySchema6)
	if err != nil || !reflect.DeepEqual(back, e) {
		t.Errorf("the pinned entry decodes as %+v, %v", back, err)
	}

	if _, err := decodeEntry(data, entrySchema5); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Errorf("the release of schema 5 reads an entry of schema 6: %v", err)
	}
	claimed := append([]byte{entrySchema5}, want...)
	for _, supported := range []byte{entrySchema5, entrySchema6} {
		if _, err := decodeEntry(claimed, supported); !errors.Is(err, domain.ErrUnsupportedFormat) {
			t.Errorf("a binary of schema %d reads node.revoke in an entry of schema 5: %v", supported, err)
		}
	}
	// The node it is about is what decides who may propose it: the node
	// itself, as for its removal.
	s := &Store{supported: MetadataVersion}
	if registers, removes, counts, err := s.Subject(data); err != nil || registers != "" || removes != "gateway-1" || counts != "" {
		t.Errorf("Subject(node.revoke) = %q, %q, %q, %v", registers, removes, counts, err)
	}
	if MetadataVersion != entrySchema6 {
		t.Errorf("MetadataVersion = %d: this release announces schema 6", MetadataVersion)
	}
}

// TestRevokingANode: the first node.revoke marks the state as schema 6 -
// whether it took effect or was refused - because the release of schema 5
// stops at it whatever became of it; the record is the bytes of the golden
// file; revoking a node twice changes nothing; and a state of schema 5 that
// was never revoked from holds nothing of schema 6.
func TestRevokingANode(t *testing.T) {
	t.Parallel()
	golden := schema6Golden(t)
	revoke := func(id string) entry { return entry{Type: cmdRevokeNode, RevokeNode: &revokeNodeCmd{ID: id}} }

	t.Run("revoked", func(t *testing.T) {
		t.Parallel()
		m := newMachine6(t)
		m.at = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
		// What a cluster at level 5 wrote.
		m.ok5(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedAt: m.at}})
		m.at = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
		if got := stateSchema(t, m.f); got != stateSchema5 {
			t.Fatalf("after entries of schema 5: state schema %d", got)
		}
		if got := rawState(t, m.f, revokedBucket()); len(got) != 0 {
			t.Fatalf("a state of schema 5 holds %v", got)
		}

		if r := m.as(entrySchema6, revoke("gateway-1")); r.err != nil {
			t.Fatalf("node.revoke: %v", r.err)
		}
		if got := stateSchema(t, m.f); got != stateSchema6 {
			t.Fatalf("after node.revoke: state schema %d", got)
		}
		record := rawStateOf(rawState(t, m.f, revokedBucket()), revokedBucket())
		key := fmt.Sprintf("%s/%x", revokedBucket(), "gateway-1")
		if want := golden["record revokednodes/gateway-1"]; len(record) != 1 || record[key] != want {
			t.Errorf("the record of revoked nodes = %v, want %s: %s", record, key, want)
		}
		before := rawState(t, m.f, everyBucket(t, m.f)...)
		if r := m.as(entrySchema6, revoke("gateway-1")); r.err != nil {
			t.Fatalf("node.revoke again: %v", r.err)
		}
		after := rawState(t, m.f, everyBucket(t, m.f)...)
		for _, state := range []map[string]string{before, after} {
			for _, k := range [][]byte{appliedKey(), timeKey()} {
				delete(state, fmt.Sprintf("meta/%x", k))
			}
		}
		if !reflect.DeepEqual(after, before) {
			t.Error("revoking a node twice changed the state")
		}
		if err := m.f.view(func(tx *bbolt.Tx) error {
			if !isRevoked(tx, "gateway-1") || isRevoked(tx, "gateway-2") {
				t.Error("isRevoked does not say what was revoked")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("refused", func(t *testing.T) {
		t.Parallel()
		m := newMachine6(t)
		m.ok5(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedAt: m.at}})
		if r := m.as(entrySchema6, revoke("not a node ID")); !errors.Is(r.err, domain.ErrInvalidArgument) {
			t.Fatalf("node.revoke of no node = %v, want %v", r.err, domain.ErrInvalidArgument)
		}
		if got := stateSchema(t, m.f); got != stateSchema6 {
			t.Errorf("after a refused node.revoke: state schema %d", got)
		}
		if got := rawStateOf(rawState(t, m.f, revokedBucket()), revokedBucket()); len(got) != 0 {
			t.Errorf("a refused node.revoke recorded %v", got)
		}
	})

	t.Run("the release of schema 5", func(t *testing.T) {
		t.Parallel()
		m := newMachine5(t)
		m.ok5(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedAt: m.at}})
		before := rawState(t, m.f, everyBucket(t, m.f)...)
		if r := m.raw(entrySchema6, revoke("gateway-1")); !r.fatal || !errors.Is(r.err, domain.ErrUnsupportedFormat) {
			t.Fatalf("the release of schema 5 applied node.revoke: %+v", r)
		}
		if after := rawState(t, m.f, everyBucket(t, m.f)...); !reflect.DeepEqual(after, before) {
			t.Error("the release of schema 5 changed its state at an entry of schema 6")
		}
	})
}

// TestAStateOfSchema6IsOpenedOnlyByABinaryOfSchema6: once a node was
// revoked there is no way back to the release of schema 5, which refuses
// the state; this binary opens it and goes on.
func TestAStateOfSchema6IsOpenedOnlyByABinaryOfSchema6(t *testing.T) {
	t.Parallel()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	f, err := openFSMOf(root, entrySchema6)
	if err != nil {
		t.Fatal(err)
	}
	m := &machine{t: t, f: f, at: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	m.ok5(entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: "b", CreatedAt: m.at}})
	m.ok5(entry{Type: cmdPutObject, PutObject: versionToRecord(porttest.SampleVersion("b", "k", 3))})
	m.as(entrySchema6, entry{Type: cmdRevokeNode, RevokeNode: &revokeNodeCmd{ID: "gateway-1"}})
	want := rawState(t, f, everyBucket(t, f)...)
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	if f, err := openFSMOf(root, entrySchema5); !errors.Is(err, domain.ErrUnsupportedFormat) {
		if f != nil {
			_ = f.close()
		}
		t.Fatalf("the release of schema 5 opens a state of schema 6: %v", err)
	}
	if f, err = openFSM(root); err != nil {
		t.Fatalf("this binary does not open its own state: %v", err)
	}
	defer func() { _ = f.close() }()
	if got := rawState(t, f, everyBucket(t, f)...); !reflect.DeepEqual(got, want) {
		t.Error("the state of schema 6 was changed by being opened")
	}
}

// TestRollingUpgradeToSchema6: a cluster of the release of schema 5 is
// upgraded to this release one node at a time. While any member is of the
// release before, a node cannot be revoked - nothing is logged, and the
// state stays one that release opens; once every member has registered
// schema 6, a node is revoked through a node that does not lead, and every
// member knows it.
func TestRollingUpgradeToSchema6(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t)
	nodes := []string{"m1", "m2", "m3", "s4"}
	for _, id := range nodes {
		c.schema5[id] = true
	}
	c.form(nodes[:3], nodes[3:])
	c.register(nodes...)
	for _, id := range nodes {
		eventually(t, "node "+id+" to see every member on schema 5", func() bool {
			level, err := c.store(id).entryVersion()
			return err == nil && level == entrySchema5
		})
	}
	// untouched says of every node that it holds nothing of schema 6.
	untouched := func(step string) {
		t.Helper()
		for _, id := range nodes {
			c.ready(id)
			s := c.store(id)
			if schema := stateSchema(t, s.fsm); schema >= stateSchema6 {
				t.Errorf("%s: node %s: state schema %d", step, id, schema)
			}
			if raw := rawState(t, s.fsm, revokedBucket()); len(raw) != 0 {
				t.Errorf("%s: node %s holds %v", step, id, raw)
			}
		}
	}
	refused := func(step string) {
		t.Helper()
		for _, id := range nodes {
			if err := c.store(id).CanRevoke(); !errors.Is(err, domain.ErrUnavailable) {
				t.Errorf("%s: CanRevoke on %s = %v, want %v", step, id, err, domain.ErrUnavailable)
			}
			// The local KMS keeps wrapping keys a node of schema 5 opens
			// (ADR-0031).
			if level, err := c.store(id).MetadataLevel(); err != nil || level != entrySchema5 {
				t.Errorf("%s: MetadataLevel on %s = %d, %v; want %d", step, id, level, err, entrySchema5)
			}
		}
		err := c.store("m2").RevokeNode(ctx, "s4")
		if !errors.Is(err, domain.ErrUnavailable) || errors.Is(err, domain.ErrOutcomeUnknown) {
			t.Fatalf("%s: RevokeNode = %v, want a definite %v", step, err, domain.ErrUnavailable)
		}
		untouched(step)
	}
	refused("every node of schema 5")

	for i, id := range nodes {
		c.stop(id)
		c.mu.Lock()
		c.schema5[id] = false
		c.mu.Unlock()
		back := c.start(id, i < 3)
		c.ready(id)
		if back.supported != MetadataVersion {
			t.Fatalf("node %s came back applying schema %d", id, back.supported)
		}
		c.register(id)
		if i < len(nodes)-1 {
			refused(fmt.Sprintf("%d of %d nodes upgraded", i+1, len(nodes)))
		}
	}
	for _, id := range nodes {
		eventually(t, "node "+id+" to see every member on schema 6", func() bool {
			level, err := c.store(id).entryVersion()
			return err == nil && level == entrySchema6
		})
	}

	if err := c.store(c.leader()).CanRevoke(); err != nil {
		t.Fatalf("CanRevoke at level 6: %v", err)
	}
	for _, id := range nodes {
		if level, err := c.store(id).MetadataLevel(); err != nil || level != entrySchema6 {
			t.Errorf("MetadataLevel on %s = %d, %v; want %d", id, level, err, entrySchema6)
		}
	}
	if err := c.store("m2").RevokeNode(ctx, "s4"); err != nil {
		t.Fatalf("RevokeNode at level 6: %v", err)
	}
	for _, id := range nodes {
		c.ready(id)
		s := c.store(id)
		eventually(t, "node "+id+" to know that s4 was revoked", func() bool {
			revoked, err := s.NodeRevoked("s4")
			return err == nil && revoked
		})
		if revoked, err := s.NodeRevoked("m1"); err != nil || revoked {
			t.Errorf("node %s says m1 was revoked: %v, %v", id, revoked, err)
		}
		if schema := stateSchema(t, s.fsm); schema != stateSchema6 {
			t.Errorf("node %s: state schema %d after a node was revoked", id, schema)
		}
	}
	// The floor: a state of schema 6 has no member of the release before,
	// whatever a member that has not registered says.
	if level, err := c.store("m1").entryVersion(); err != nil || level != entrySchema6 {
		t.Errorf("level after the revocation = %d, %v", level, err)
	}
}

// schema6Frozen is the SHA-256 of testdata/schema6.golden.txt. Schema 6 is
// announced with the file (MetadataVersion), and two releases write and read
// what it pins from then on: a change of it needs a schema 7, never a new
// sum here.
const schema6Frozen = "a9f9d10e1f85d9acf5730e64cdf7cce7a92578df3e9a3be81805aad5925cdadf"

// TestTheGoldenFileOfSchema6IsFrozen holds the golden file of schema 6 to
// the bytes it had when schema 6 was announced.
func TestTheGoldenFileOfSchema6IsFrozen(t *testing.T) {
	t.Parallel()
	content, err := fs.ReadFile(os.DirFS("testdata"), schema6File)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(content); hex.EncodeToString(sum[:]) != schema6Frozen {
		t.Errorf("testdata/%s was changed (SHA-256 %x): the bytes of schema 6 are frozen since it was announced, and a change of them needs a schema 7",
			schema6File, sum)
	}
}
