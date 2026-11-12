package raftmeta

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// TestAnEntryThatCannotBeStoredIsRefusedBeforeItIsLogged (H35): an entry
// whose key the state database cannot take - none, or one longer than a
// bbolt key - would stop the state machine of every member that applies
// it, at every restart. Whoever proposes it, through the leader or on it, is
// refused definitely, nothing is logged, and the node goes on.
func TestAnEntryThatCannotBeStoredIsRefusedBeforeItIsLogged(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := openStore(t, t.TempDir())
	if err := store.PutUser(ctx, iam.User{Name: "alice", CreatedAt: time.Unix(1, 0)}, true, audit.Record{}); err != nil {
		t.Fatalf("PutUser: %v", err)
	}
	huge := strings.Repeat("a", 40_000)
	session := func(hash string) entry {
		return entry{Type: cmdPutSession, IAM: &iamCmd{Session: &sessionRecord{
			TokenHash: hash, User: "alice", CreatedAt: time.Unix(1, 0), ExpiresAt: time.Unix(1, 0).Add(time.Hour),
		}}}
	}
	idem := func(id string) entry {
		return entry{Type: cmdBeginIdem, IAM: &iamCmd{Idem: &idemRecord{ID: id, Fingerprint: "f", CreatedAt: time.Unix(1, 0)}}}
	}
	cases := []struct {
		name  string
		entry entry
		want  error
	}{
		{"a bucket of no name", entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: ""}}, domain.ErrInvalidBucketName},
		{"a bucket of a name too long for a key", entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: huge}}, domain.ErrInvalidBucketName},
		{"a bucket of a name longer than a bucket's", entry{Type: cmdCreateBucket, CreateBucket: &bucketRecord{Name: strings.Repeat("b", bucket.MaxNameLength+1)}}, domain.ErrInvalidBucketName},
		{"a session of no token hash", session(""), domain.ErrInvalidArgument},
		{"a session of a token hash too long for a key", session(huge), domain.ErrInvalidArgument},
		{"an idempotency record of an ID too long for a key", idem(huge), domain.ErrInvalidArgument},
	}
	for _, tc := range cases {
		data, err := encodeEntry(tc.entry, MetadataVersion)
		if err != nil {
			t.Fatal(err)
		}
		before := store.raft.LastIndex()
		_, err = store.ProposeLocal(ctx, data)
		if !errors.Is(err, tc.want) || errors.Is(err, domain.ErrOutcomeUnknown) {
			t.Errorf("%s: ProposeLocal = %v, want a definite %v", tc.name, err, tc.want)
		}
		if after := store.raft.LastIndex(); after != before {
			t.Errorf("%s: the log went from %d to %d entries, want nothing logged", tc.name, before, after)
		}
		if err := store.fsm.health(); err != nil {
			t.Fatalf("%s: the state machine stopped: %v", tc.name, err)
		}
	}

	// The node's own proposals pass the same gate.
	if _, err := store.propose(ctx, session(huge)); !errors.Is(err, domain.ErrInvalidArgument) || errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Errorf("propose of a session of a token hash too long = %v, want a definite %v", err, domain.ErrInvalidArgument)
	}
	// And what can be stored still is.
	if err := store.CreateBucket(ctx, bucket.Bucket{Name: "still-here", CreatedAt: time.Unix(1, 0)}); err != nil {
		t.Fatalf("CreateBucket after the refusals: %v", err)
	}
	if err := store.PutSession(ctx, ports.Session{
		TokenHash: strings.Repeat("0", 64), User: "alice", CreatedAt: time.Unix(1, 0), ExpiresAt: time.Unix(1, 0).Add(time.Hour),
	}, audit.Record{}); err != nil {
		t.Fatalf("PutSession after the refusals: %v", err)
	}
}
