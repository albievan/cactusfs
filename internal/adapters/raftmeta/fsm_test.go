package raftmeta

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
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
	data, err := encodeEntry(e, entrySchemaVersion)
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
		"newer schema version":         append([]byte{MetadataVersion + 1}, `{"type":"bucket.create","create_bucket":{"name":"x"}}`...),
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
	data, err := encodeEntry(entry{Type: cmdPutObject, PutObject: versionToRecord(version)}, entrySchemaVersion)
	if err != nil {
		t.Fatalf("encodeEntry: %v", err)
	}
	if data[0] != entrySchemaVersion {
		t.Errorf("first byte = %d, want the schema version %d", data[0], entrySchemaVersion)
	}
	decoded, err := decodeEntry(data, entrySchemaVersion)
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
	f.Add(append([]byte{2}, `{"type":"upload.abort","abort_upload":{"bucket":"b","key":"k","upload_id":"00"}}`...))
	f.Add(append([]byte{2}, `{"type":"upload.complete","complete_upload":{"bucket":"b","parts":[{"number":1}]},"put_part":{}}`...))
	f.Add(append([]byte{2}, `{"type":"object.put","put_object":{"bucket":"b","key":"k","created_by":"user/x"},"add_reads":{"node":"n"}}`...))
	f.Add(append([]byte{2}, `{"type":"bucket.config.put","set_bucket_config":{"bucket":"b","kind":"tagrules","document":"e30="}}`...))
	f.Add(append([]byte{2}, `{"type":"subbucket.put","put_sub_bucket":{"bucket":"b","sub_bucket":{"name":"reports"}}}`...))
	f.Add(append([]byte{3}, `{"type":"object.reads.add","add_reads":{"node":"n","boot":18446744073709551615,"seq":1,"items":[{"bucket":"b","key":"k","version":"null","version_id":"0102030405060708090a0b0c0d0e0f10","who":"root","downloads":1}]}}`...))
	f.Add(append([]byte{3}, `{"type":"subbucket.put","put_sub_bucket":{"bucket":"b","sub_bucket":{"name":"reports","policy":"e30="},"audit":{"actor":"root"}}}`...))
	f.Add(append([]byte{3}, `{"type":"subbucket.delete","delete_sub_bucket":{"bucket":"b","name":"reports"},"put_sub_bucket":{}}`...))
	f.Add(append([]byte{3}, `{"type":"tag.definition.put","put_tag_definition":{"definition":{"key":"stage","values":["a","b"]}}}`...))
	f.Add(append([]byte{3}, `{"type":"tag.definition.delete","delete_tag_definition":{"key":"stage"}}`...))
	f.Add(append([]byte{3}, `{"type":"upload.create","create_upload":{"bucket":"b","key":"k","tags":{"a":"b"}}}`...))
	f.Add(append([]byte{4}, `{"type":"tag.definition.delete","delete_tag_definition":{"key":"stage"}}`...))
	// Schema 4 (ADR-0019): a bucket with its creator, in an entry of its own
	// schema and in one that claims an older; and the three entries that are
	// written as schema 4 without saying anything new.
	f.Add(append([]byte{4}, `{"type":"bucket.create","at":"2026-10-12T09:00:00Z","create_bucket":{"name":"b","created_at":"2026-10-12T09:00:00Z","created_by":"user/albie"}}`...))
	f.Add(append([]byte{3}, `{"type":"bucket.create","create_bucket":{"name":"b","created_at":"2026-10-12T09:00:00Z","created_by":"user/albie"}}`...))
	f.Add(append([]byte{1}, `{"type":"bucket.create","create_bucket":{"name":"b","created_by":"root"},"created_by":"root"}`...))
	f.Add(append([]byte{4}, `{"type":"object.put","put_object":{"bucket":"b","key":"k","created_by":"user/x"}}`...))
	f.Add(append([]byte{4}, `{"type":"upload.complete","complete_upload":{"bucket":"b","key":"k","created_by":"key/AKID","parts":[{"number":1}]}}`...))
	f.Add(append([]byte{4}, `{"type":"object.reads.add","add_reads":{"node":"n","boot":1,"seq":1,"items":[{"bucket":"b","key":"k","version":"null","version_id":"0102030405060708090a0b0c0d0e0f10","who":"anonymous","presigned":true,"downloads":1}]}}`...))
	f.Add(append([]byte{5}, `{"type":"bucket.create","create_bucket":{"name":"b","created_by":"user/albie"}}`...))
	f.Fuzz(func(t *testing.T, data []byte) {
		// A binary of the newest schema: what it decodes has one payload,
		// the one its type names.
		if e, err := decodeEntry(data, MetadataVersion); err == nil {
			if data[0] > MetadataVersion {
				t.Fatalf("an entry of schema %d was decoded: this binary applies %d", data[0], MetadataVersion)
			}
			payloads := 0
			for _, set := range []bool{
				e.CreateBucket != nil, e.DeleteBucket != nil, e.PutObject != nil, e.DeleteObject != nil, e.SetRoot != nil, e.SetTopology != nil,
				e.SetNode != nil, e.RemoveNode != nil, e.IAM != nil, e.SetVersioning != nil, e.DeleteVersion != nil, e.SetBucketLock != nil,
				e.SetRetention != nil, e.SetLegalHold != nil, e.CreateUpload != nil, e.PutPart != nil, e.CompleteUpload != nil, e.AbortUpload != nil,
				e.SetBucketConfig != nil, e.SetTags != nil,
				e.AddReads != nil, e.PutSubBucket != nil, e.DeleteSubBucket != nil, e.PutTagDefinition != nil, e.DeleteTagDefinition != nil,
			} {
				if set {
					payloads++
				}
			}
			if payloads != 1 {
				t.Fatalf("decoded entry %q has %d payloads", e.Type, payloads)
			}
			// What schema 3 added is in entries of schema 3 and in no
			// other: neither its commands nor their payloads.
			third := e.AddReads != nil || e.PutSubBucket != nil || e.DeleteSubBucket != nil || e.PutTagDefinition != nil || e.DeleteTagDefinition != nil
			if third != (versionOf(entry{Type: e.Type}) == entrySchema3) {
				t.Fatalf("decoded entry %q has a payload of schema 3: %v", e.Type, third)
			}
			if third && data[0] < entrySchema3 {
				t.Fatalf("a command of schema 3, %q, was decoded from an entry of schema %d", e.Type, data[0])
			}
			// What decodes can be written again in the schema it came
			// in, and decodes again as the same command.
			again, err := encodeEntry(e, data[0])
			if err != nil {
				t.Fatalf("a decoded entry cannot be encoded: %v", err)
			}
			if twice, err := decodeEntry(again, MetadataVersion); err != nil || twice.Type != e.Type {
				t.Fatalf("a decoded entry does not decode again: %v", err)
			}
			// A binary of the schema before never accepts what only this
			// one can apply.
			if _, err := decodeEntry(data, entrySchemaTimed); err == nil && (data[0] >= entrySchema3 || third) {
				t.Fatalf("a binary of schema 2 decoded %q from an entry of schema %d", e.Type, data[0])
			}
			// Nor does a binary of schema 3 accept an entry of schema 4,
			// whatever it says: it stops at it.
			if _, err := decodeEntry(data, entrySchema3); (err == nil) != (data[0] < entrySchema4) {
				t.Fatalf("a binary of schema 3 and %q in an entry of schema %d: %v", e.Type, data[0], err)
			}
		} else if !errors.Is(err, domain.ErrUnsupportedFormat) {
			t.Fatalf("untyped error: %v", err)
		}
		e, err := decodeEntry(data, entrySchemaVersion)
		if err != nil {
			if !errors.Is(err, domain.ErrUnsupportedFormat) {
				t.Fatalf("untyped error: %v", err)
			}
			return
		}
		payloads := 0
		for _, set := range []bool{
			e.CreateBucket != nil, e.DeleteBucket != nil, e.PutObject != nil, e.DeleteObject != nil, e.SetRoot != nil, e.SetTopology != nil,
		} {
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

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := Open(ctx, Config{
		Dir: dir, Logger: slog.New(slog.DiscardHandler), Entropy: rand.Reader,
		Timing: Timing{Heartbeat: 50 * time.Millisecond, Election: 50 * time.Millisecond, LeaderLease: 50 * time.Millisecond, Commit: 5 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestStoppedStateMachineIsFailStop: once an entry could not be applied, no
// further entry may be written to the log. Otherwise writes would be reported
// as failed now and silently take effect at the next start - after the caller
// has already cleaned up what they referred to.
func TestStoppedStateMachineIsFailStop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dir := t.TempDir()
	store := openStore(t, dir)
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "bkt"}); err != nil {
		t.Fatal(err)
	}
	first := porttest.SampleVersion("bkt", "k", 1)
	if _, err := store.PutVersion(ctx, first); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("disk full")
	store.fsm.mu.Lock()
	store.fsm.failed = boom
	store.fsm.mu.Unlock()

	if err := store.Check(ctx); !errors.Is(err, boom) {
		t.Errorf("Check = %v, want the node to be unready", err)
	}
	// A stopped state machine has entries it has not applied; whether
	// their data is referenced cannot be answered, and the collector must
	// not be told "no".
	if referenced, err := store.IsDataReferenced(ctx, first.DataID); !errors.Is(err, boom) || referenced {
		t.Errorf("IsDataReferenced on a stopped state machine = %v, %v; want an error", referenced, err)
	}
	_, err := store.PutVersion(ctx, porttest.SampleVersion("bkt", "k", 2))
	if !errors.Is(err, boom) {
		t.Fatalf("PutVersion on a stopped state machine = %v, want %v", err, boom)
	}
	// Refused before it was logged: a definite "not committed".
	if errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Errorf("the refusal is reported as an unknown outcome: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After a restart the refused write has not appeared.
	reopened := openStore(t, dir)
	got, err := reopened.GetObject(ctx, "bkt", "k")
	if err != nil || got.VersionID != first.VersionID {
		t.Errorf("after restart the object is %s (%v), want the version from before the failure", got.VersionID, err)
	}
}

// TestRaftErrorsThatMayHaveBeenLogged: only errors Raft returns before
// writing to its log are definite. For the others the entry can still take
// effect, so nothing it refers to may be cleaned up.
func TestRaftErrorsThatMayHaveBeenLogged(t *testing.T) {
	t.Parallel()

	definite := []error{raft.ErrNotLeader, raft.ErrEnqueueTimeout, raft.ErrLeadershipTransferInProgress}
	for _, err := range definite {
		got := translateRaftError(err)
		if errors.Is(got, domain.ErrOutcomeUnknown) || !errors.Is(got, domain.ErrNotLeader) {
			t.Errorf("translateRaftError(%v) = %v, want a definite not-leader error", err, got)
		}
	}
	unknown := []error{raft.ErrLeadershipLost, raft.ErrRaftShutdown, raft.ErrAbortedByRestore, errors.New("anything else")}
	for _, err := range unknown {
		if got := translateRaftError(err); !errors.Is(got, domain.ErrOutcomeUnknown) || !errors.Is(got, err) {
			t.Errorf("translateRaftError(%v) = %v, want it to wrap %v", err, got, domain.ErrOutcomeUnknown)
		}
	}
}

func TestApplyResultsOfAStoppedStateMachineAreFatal(t *testing.T) {
	t.Parallel()

	f := newFSM(t)
	bad := result(t, f.Apply(&raft.Log{Index: 1, Type: raft.LogCommand, Data: []byte{9}}))
	if !bad.fatal || bad.err == nil {
		t.Errorf("an entry that cannot be applied: %+v, want a fatal result", bad)
	}
	later := result(t, f.Apply(command(t, 2, createBucket("x"))))
	if !later.fatal {
		t.Errorf("an entry after the stop: %+v, want a fatal result", later)
	}
	// A refusal is not fatal: the entry was applied, with no effect.
	g := newFSM(t)
	_ = g.Apply(command(t, 1, createBucket("x")))
	if refused := result(t, g.Apply(command(t, 2, createBucket("x")))); refused.fatal || refused.err == nil {
		t.Errorf("a refusal: %+v, want an error that is not fatal", refused)
	}
}

// TestDataIndexIsRebuiltForAnOlderState: the data-reference index is derived
// from the object index. A state written before it existed gets it on open.
func TestDataIndexIsRebuiltForAnOlderState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dir := t.TempDir()
	store := openStore(t, dir)
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "bkt"}); err != nil {
		t.Fatal(err)
	}
	kept, other := porttest.SampleVersion("bkt", "a", 1), porttest.SampleVersion("bkt", "b", 5)
	for _, v := range []object.Version{kept, other} {
		if _, err := store.PutVersion(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	// Make the state look as it did before the index existed.
	if err := store.fsm.db.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket(dataBucket()) }); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openStore(t, dir)
	for _, v := range []object.Version{kept, other} {
		if ok, err := reopened.IsDataReferenced(ctx, v.DataID); err != nil || !ok {
			t.Errorf("data %s is not referenced after the index was rebuilt: %v", v.DataID, err)
		}
	}
	// The rebuilt index is the one puts are checked against: data that
	// belongs to a version is not given to another.
	twin := porttest.SampleVersion("bkt", "c", 9)
	twin.DataID = other.DataID
	if _, err := reopened.PutVersion(ctx, twin); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("a put that names another version's data, after the rebuild = %v, want %v", err, domain.ErrConflict)
	}
	if _, err := reopened.DeleteObject(ctx, "bkt", "b", ports.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := reopened.IsDataReferenced(ctx, other.DataID); ok {
		t.Error("data is still referenced after its object was deleted")
	}
	if ok, _ := reopened.IsDataReferenced(ctx, kept.DataID); !ok {
		t.Error("deleting one object released the data of another")
	}
}

func TestTopologyCommandsAreValidatedByTheStateMachine(t *testing.T) {
	t.Parallel()

	f := newFSM(t)
	set := func(index uint64, record topologyRecord) applyResult {
		return result(t, f.Apply(command(t, index, entry{Type: cmdSetTopology, SetTopology: &record})))
	}
	good := topologyRecord{Sets: [][]slotRecord{{{DriveID: "a"}, {DriveID: "b"}}}}

	if r := set(1, topologyRecord{}); !errors.Is(r.err, domain.ErrInvalidArgument) || r.fatal {
		t.Errorf("empty topology: %+v, want a refusal", r)
	}
	if r := set(2, topologyRecord{Sets: [][]slotRecord{{{DriveID: "a"}, {DriveID: "a"}}}}); !errors.Is(r.err, domain.ErrInvalidArgument) {
		t.Errorf("duplicate drive: %+v, want a refusal", r)
	}
	if r := set(3, good); r.err != nil {
		t.Fatalf("first topology: %v", r.err)
	}
	if r := set(4, good); !errors.Is(r.err, domain.ErrConflict) || r.fatal {
		t.Errorf("stale change: %+v, want a conflict", r)
	}
	next := good
	next.Version = 1
	if r := set(5, next); r.err != nil {
		t.Errorf("change based on the stored version: %v", r.err)
	}
}

// stateSchema reads the schema the state database says it has.
func stateSchema(t *testing.T, f *fsm) uint64 {
	t.Helper()
	var schema uint64
	if err := f.view(func(tx *bbolt.Tx) error {
		schema = binary.BigEndian.Uint64(tx.Bucket(metaBucket()).Get(schemaKey()))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return schema
}

// TestTimedEntries: an entry of schema 2 carries the leader's time. The
// state machine takes its time from there and from nowhere else, never lets
// it go back, and marks the state as one the previous release must not open.
func TestTimedEntries(t *testing.T) {
	t.Parallel()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	f, err := openFSM(root)
	if err != nil {
		t.Fatal(err)
	}
	stateTime := func() time.Time {
		var at time.Time
		if err := f.view(func(tx *bbolt.Tx) error { at = readTime(tx); return nil }); err != nil {
			t.Fatal(err)
		}
		return at
	}
	apply := func(index uint64, name string, at time.Time, version byte) applyResult {
		data, err := encodeEntry(entry{Type: cmdCreateBucket, At: at, CreateBucket: &bucketRecord{Name: name, CreatedAt: at}}, version)
		if err != nil {
			t.Fatal(err)
		}
		return result(t, f.Apply(&raft.Log{Index: index, Type: raft.LogCommand, Data: data}))
	}
	noon := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	// Entries of schema 1 leave the state as the previous release wrote it.
	if r := apply(1, "plain", time.Time{}, entrySchemaVersion); r.err != nil {
		t.Fatal(r.err)
	}
	if stateSchema(t, f) != stateSchemaVersion || !stateTime().IsZero() {
		t.Fatalf("after a schema 1 entry: state schema %d, time %v", stateSchema(t, f), stateTime())
	}
	if r := apply(2, "timed", noon, entrySchemaTimed); r.err != nil {
		t.Fatal(r.err)
	}
	if stateSchema(t, f) != stateSchemaTimed || !stateTime().Equal(noon) {
		t.Fatalf("after a schema 2 entry: state schema %d, time %v", stateSchema(t, f), stateTime())
	}
	// A leader whose clock is behind does not turn time back.
	if r := apply(3, "from-a-slow-clock", noon.Add(-time.Hour), entrySchemaTimed); r.err != nil {
		t.Fatal(r.err)
	}
	if !stateTime().Equal(noon) {
		t.Errorf("time went back to %v", stateTime())
	}
	if r := apply(4, "later", noon.Add(time.Minute), entrySchemaTimed); r.err != nil {
		t.Fatal(r.err)
	}
	// A replayed entry changes nothing, the time included.
	if r := apply(2, "timed", noon.Add(24*time.Hour), entrySchemaTimed); r.err != nil {
		t.Fatal(r.err)
	}
	if !stateTime().Equal(noon.Add(time.Minute)) {
		t.Errorf("time after a replay = %v", stateTime())
	}
	// The state reopens with its schema and its time.
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	if f, err = openFSM(root); err != nil {
		t.Fatalf("reopening a schema 2 state: %v", err)
	}
	if stateSchema(t, f) != stateSchemaTimed || !stateTime().Equal(noon.Add(time.Minute)) {
		t.Errorf("after reopening: state schema %d, time %v", stateSchema(t, f), stateTime())
	}
	// A state of a schema this binary does not know is refused, as the
	// previous release refuses this one.
	if err := f.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(metaBucket()).Put(schemaKey(), binary.BigEndian.AppendUint64(nil, stateSchema5+1))
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openFSM(root); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Errorf("opening a state of a newer schema = %v, want %v", err, domain.ErrUnsupportedFormat)
	}
}
