package raftmeta

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

func newFSM(t *testing.T) *fsm {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	f, err := openFSM(root)
	if err != nil {
		t.Fatalf("openFSM: %v", err)
	}
	t.Cleanup(func() { _ = f.close(); _ = root.Close() })
	return f
}

func command(t *testing.T, index uint64, e entry) *raft.Log {
	t.Helper()
	data, err := encodeEntry(e)
	if err != nil {
		t.Fatalf("encodeEntry: %v", err)
	}
	return &raft.Log{Index: index, Type: raft.LogCommand, Data: data}
}

func createBucket(name string) entry {
	return entry{Type: cmdCreateBucket, CreateBucket: bucketToRecord(bucket.Bucket{Name: name, CreatedAt: time.Unix(1, 0)})}
}

func result(t *testing.T, response any) applyResult {
	t.Helper()
	r, ok := response.(applyResult)
	if !ok {
		t.Fatalf("Apply returned %T, want applyResult", response)
	}
	return r
}

func TestApplyIsIdempotentPerIndex(t *testing.T) {
	t.Parallel()

	f := newFSM(t)
	if r := result(t, f.Apply(command(t, 5, createBucket("one")))); r.err != nil {
		t.Fatalf("first apply: %v", r.err)
	}
	// The same entry delivered again after a restart is skipped, not
	// refused as a duplicate bucket.
	if r := result(t, f.Apply(command(t, 5, createBucket("one")))); r.err != nil {
		t.Errorf("replayed entry returned %v, want it skipped", r.err)
	}
	if r := result(t, f.Apply(command(t, 3, createBucket("older")))); r.err != nil {
		t.Errorf("older entry returned %v, want it skipped", r.err)
	}
	// A new entry with the same content is a real duplicate.
	if r := result(t, f.Apply(command(t, 6, createBucket("one")))); !errors.Is(r.err, domain.ErrBucketAlreadyExists) {
		t.Errorf("new duplicate entry returned %v, want %v", r.err, domain.ErrBucketAlreadyExists)
	}
	if index, err := f.appliedIndex(); err != nil || index != 6 {
		t.Errorf("appliedIndex = %d, %v; want 6: refused entries count as applied", index, err)
	}
}

func TestNonCommandEntriesAreIgnored(t *testing.T) {
	t.Parallel()

	f := newFSM(t)
	if r := result(t, f.Apply(&raft.Log{Index: 1, Type: raft.LogNoop})); r.err != nil {
		t.Errorf("noop entry returned %v", r.err)
	}
	if index, _ := f.appliedIndex(); index != 0 {
		t.Errorf("appliedIndex = %d after a noop, want 0", index)
	}
}

// TestUnknownEntryStopsTheStateMachine pins the rolling-upgrade rule: an
// entry this binary does not understand is never skipped, because skipping
// would silently diverge this replica from the others.
func TestUnknownEntryStopsTheStateMachine(t *testing.T) {
	t.Parallel()

	tests := map[string][]byte{
		"newer schema version":         append([]byte{2}, `{"type":"bucket.create","create_bucket":{"name":"x"}}`...),
		"unknown command":              append([]byte{1}, `{"type":"bucket.rename"}`...),
		"command without its payload":  append([]byte{1}, `{"type":"bucket.create"}`...),
		"command with another payload": append([]byte{1}, `{"type":"bucket.create","delete_bucket":{"name":"x"}}`...),
		"two payloads":                 append([]byte{1}, `{"type":"bucket.create","create_bucket":{"name":"x"},"delete_bucket":{"name":"x"}}`...),
		"not JSON":                     append([]byte{1}, `{{{`...),
		"empty":                        nil,
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFSM(t)
			if r := result(t, f.Apply(command(t, 1, createBucket("before")))); r.err != nil {
				t.Fatalf("setup: %v", r.err)
			}

			r := result(t, f.Apply(&raft.Log{Index: 2, Type: raft.LogCommand, Data: data}))
			if !errors.Is(r.err, domain.ErrUnsupportedFormat) {
				t.Fatalf("Apply error = %v, want %v", r.err, domain.ErrUnsupportedFormat)
			}
			if index, _ := f.appliedIndex(); index != 1 {
				t.Errorf("appliedIndex = %d, want 1: the bad entry must not count as applied", index)
			}
			if f.health() == nil {
				t.Error("health() = nil after an entry that could not be applied")
			}
			// Later entries are refused too: order must be preserved.
			if r := result(t, f.Apply(command(t, 3, createBucket("after")))); r.err == nil {
				t.Error("an entry was applied after the state machine stopped")
			}
		})
	}
}

func TestSnapshotAndRestore(t *testing.T) {
	t.Parallel()

	source := newFSM(t)
	for i, e := range []entry{
		createBucket("one"),
		createBucket("two"),
		{Type: cmdPutObject, PutObject: versionToRecord(porttest.SampleVersion("one", "k", 3))},
	} {
		if r := result(t, source.Apply(command(t, uint64(i+1), e))); r.err != nil {
			t.Fatalf("apply %d: %v", i, r.err)
		}
	}
	snap, err := source.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// An entry applied after Snapshot but before Persist is not in it.
	if r := result(t, source.Apply(command(t, 4, createBucket("late")))); r.err != nil {
		t.Fatalf("late apply: %v", r.err)
	}
	sink := &memorySink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	snap.Release()
	if !sink.closed || sink.cancelled {
		t.Errorf("sink closed=%v cancelled=%v, want closed and not cancelled", sink.closed, sink.cancelled)
	}

	target := newFSM(t)
	if r := result(t, target.Apply(command(t, 1, createBucket("discarded")))); r.err != nil {
		t.Fatalf("target apply: %v", r.err)
	}
	if err := target.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if index, err := target.appliedIndex(); err != nil || index != 3 {
		t.Errorf("appliedIndex after restore = %d, %v; want 3", index, err)
	}
	// The restored state has "one" and "two", not "discarded" or "late".
	index := uint64(10)
	for name, wantExists := range map[string]bool{"one": true, "two": true, "discarded": false, "late": false} {
		index++
		r := result(t, target.Apply(command(t, index, createBucket(name))))
		exists := errors.Is(r.err, domain.ErrBucketAlreadyExists)
		if exists != wantExists {
			t.Errorf("bucket %q exists = %v after restore, want %v", name, exists, wantExists)
		}
	}
}

func TestRestoreRefusesAnUnreadableSnapshotAndKeepsTheState(t *testing.T) {
	t.Parallel()

	f := newFSM(t)
	if r := result(t, f.Apply(command(t, 1, createBucket("kept")))); r.err != nil {
		t.Fatalf("apply: %v", r.err)
	}
	if err := f.Restore(io.NopCloser(strings.NewReader("this is not a bbolt database"))); err == nil {
		t.Fatal("Restore of garbage returned nil")
	}
	if r := result(t, f.Apply(command(t, 2, createBucket("kept")))); !errors.Is(r.err, domain.ErrBucketAlreadyExists) {
		t.Errorf("state was lost by a failed restore: %v", r.err)
	}
}

func TestPersistCancelsTheSinkOnFailure(t *testing.T) {
	t.Parallel()

	f := newFSM(t)
	snap, err := f.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	defer snap.Release()
	sink := &memorySink{failWrite: errors.New("disk full")}
	if err := snap.Persist(sink); err == nil || !sink.cancelled {
		t.Errorf("Persist = %v, cancelled = %v; want an error and a cancelled sink", err, sink.cancelled)
	}
}

func TestClosedStateMachine(t *testing.T) {
	t.Parallel()

	f := newFSM(t)
	if err := f.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := f.close(); err != nil {
		t.Errorf("second close: %v", err)
	}
	if r := result(t, f.Apply(command(t, 1, createBucket("x")))); !errors.Is(r.err, domain.ErrShuttingDown) {
		t.Errorf("Apply after close = %v, want %v", r.err, domain.ErrShuttingDown)
	}
	if _, err := f.Snapshot(); !errors.Is(err, domain.ErrShuttingDown) {
		t.Errorf("Snapshot after close = %v, want %v", err, domain.ErrShuttingDown)
	}
	if _, err := f.appliedIndex(); !errors.Is(err, domain.ErrShuttingDown) {
		t.Errorf("appliedIndex after close = %v, want %v", err, domain.ErrShuttingDown)
	}
}

type memorySink struct {
	bytes.Buffer
	failWrite error
	closed    bool
	cancelled bool
}

func (s *memorySink) Write(p []byte) (int, error) {
	if s.failWrite != nil {
		return 0, s.failWrite
	}
	return s.Buffer.Write(p)
}
func (s *memorySink) ID() string    { return "test" }
func (s *memorySink) Close() error  { s.closed = true; return nil }
func (s *memorySink) Cancel() error { s.cancelled = true; return nil }

func TestEntryRoundTrip(t *testing.T) {
	t.Parallel()

	version := porttest.SampleVersion("b", "k", 7)
	data, err := encodeEntry(entry{Type: cmdPutObject, PutObject: versionToRecord(version)})
	if err != nil {
		t.Fatalf("encodeEntry: %v", err)
	}
	if data[0] != entrySchemaVersion {
		t.Errorf("first byte = %d, want the schema version %d", data[0], entrySchemaVersion)
	}
	decoded, err := decodeEntry(data)
	if err != nil {
		t.Fatalf("decodeEntry: %v", err)
	}
	got, err := decoded.PutObject.toDomain()
	if err != nil {
		t.Fatalf("toDomain: %v", err)
	}
	if got.DataID != version.DataID || got.Key != version.Key || !bytes.Equal(got.Encryption.WrappedKey, version.Encryption.WrappedKey) ||
		got.Layout.Shards[0] != version.Layout.Shards[0] || !got.ModifiedAt.Equal(version.ModifiedAt) {
		t.Errorf("round trip changed the version:\n got %+v\nwant %+v", got, version)
	}

	bad := *decoded.PutObject
	bad.DataID = "../../etc/passwd"
	if _, err := bad.toDomain(); !errors.Is(err, domain.ErrInvalidID) {
		t.Errorf("a stored version with a malformed data ID: error = %v, want %v", err, domain.ErrInvalidID)
	}
	bad = *decoded.PutObject
	bad.VersionID = "zz"
	if _, err := bad.toDomain(); !errors.Is(err, domain.ErrInvalidID) {
		t.Errorf("a stored version with a malformed version ID: error = %v, want %v", err, domain.ErrInvalidID)
	}
}

// FuzzDecodeEntry: arbitrary log bytes either decode into exactly one
// well-formed command or fail with ErrUnsupportedFormat; they never panic and
// never decode ambiguously.
func FuzzDecodeEntry(f *testing.F) {
	f.Add([]byte{1})
	f.Add(append([]byte{1}, `{"type":"bucket.delete","delete_bucket":{"name":"x"}}`...))
	f.Add(append([]byte{2}, `{}`...))
	f.Fuzz(func(t *testing.T, data []byte) {
		e, err := decodeEntry(data)
		if err != nil {
			if !errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Fatalf("untyped error: %v", err)
			}
			return
		}
		payloads := 0
		for _, set := range []bool{e.CreateBucket != nil, e.DeleteBucket != nil, e.PutObject != nil, e.DeleteObject != nil, e.SetRoot != nil} {
			if set {
				payloads++
			}
		}
		if payloads != 1 {
			t.Fatalf("decoded entry %q has %d payloads", e.Type, payloads)
		}
	})
}

func TestSlogBridge(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger := newHCLogger(base).Named("raft").With("node", "n1")

	logger.Debug("hidden")
	logger.Trace("hidden")
	logger.Info("elected", "term", 3)
	logger.Warn("slow")
	logger.Error("failed")
	logger.Log(hclog.Off, "never")
	logger.Named("snapshot").Info("nested")
	logger.StandardLogger(nil).Print("from the standard logger")

	text := out.String()
	for _, want := range []string{
		`"msg":"elected"`, `"term":3`, `"node":"n1"`, `"component":"raft"`, `"level":"WARN"`, `"level":"ERROR"`,
		`"component":"raft.snapshot"`, `"msg":"from the standard logger"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("log output lacks %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, "hidden") || strings.Contains(text, "never") {
		t.Errorf("messages below the level were written:\n%s", text)
	}
	if logger.IsDebug() || logger.IsTrace() || !logger.IsInfo() || !logger.IsWarn() || !logger.IsError() {
		t.Error("level guards do not follow the slog handler")
	}
	if logger.GetLevel() != hclog.Info {
		t.Errorf("GetLevel() = %v, want info", logger.GetLevel())
	}
	if logger.Name() != "raft" || len(logger.ImpliedArgs()) != 2 {
		t.Errorf("Name() = %q, ImpliedArgs() = %v", logger.Name(), logger.ImpliedArgs())
	}
	logger.SetLevel(hclog.Trace)
	if logger.IsDebug() {
		t.Error("SetLevel changed the level, which the slog handler owns")
	}
	if got := newHCLogger(slog.New(slog.DiscardHandler)).GetLevel(); got != hclog.Error {
		t.Errorf("GetLevel() of a discarding logger = %v, want error", got)
	}
}
