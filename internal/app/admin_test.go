package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// TestIdempotency is the life of an idempotency key.
func TestIdempotency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, clock := porttest.NewMemIAM(), porttest.NewClock(testNow)
	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}
	s := app.NewIdempotencyService(store, kms, clock, rand.Reader)
	request := app.IdempotentRequest{Scope: "key:AKID", Key: "k-1", Fingerprint: "POST /users {alice}"}
	answer := app.IdempotentAnswer{Status: 201, ContentType: "application/json", Body: []byte(`{"secret_key":"shown once"}`)}

	tests := []struct {
		name string
		req  app.IdempotentRequest
		want error
	}{
		{"no key", app.IdempotentRequest{Scope: "key:AKID", Fingerprint: "x"}, domain.ErrInvalidArgument},
		{"no scope", app.IdempotentRequest{Key: "k", Fingerprint: "x"}, domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		if _, _, err := s.Begin(ctx, tt.req); !errors.Is(err, tt.want) {
			t.Errorf("%s: Begin = %v, want %v", tt.name, err, tt.want)
		}
	}

	replay, reservation, err := s.Begin(ctx, request)
	if err != nil || replay != nil {
		t.Fatalf("first Begin = %v, %v", replay, err)
	}
	// While the first is being carried out, a repeat waits.
	if _, _, err := s.Begin(ctx, request); !errors.Is(err, domain.ErrRequestInProgress) {
		t.Fatalf("Begin during the first request = %v, want %v", err, domain.ErrRequestInProgress)
	}
	if err := s.Complete(ctx, reservation, answer); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	replay, _, err = s.Begin(ctx, request)
	if err != nil || replay == nil || replay.Status != 201 || replay.ContentType != "application/json" || !bytes.Equal(replay.Body, answer.Body) {
		t.Fatalf("Begin after the answer = %+v, %v", replay, err)
	}
	// The answer is stored sealed, and bound to its record.
	stored, err := store.GetIdempotent(ctx, hashOfIdempotent(t, store))
	if err != nil || bytes.Contains(stored.Body.Ciphertext, []byte("shown once")) || stored.Body.KMSKeyID != "test:1" || len(stored.Body.WrappedKey) == 0 {
		t.Fatalf("stored record = %+v, %v", stored, err)
	}
	// The same key for another request, or from another caller.
	other := request
	other.Fingerprint = "POST /users {bob}"
	if _, _, err := s.Begin(ctx, other); !errors.Is(err, domain.ErrIdempotencyKeyReused) {
		t.Errorf("another request with the key = %v, want %v", err, domain.ErrIdempotencyKeyReused)
	}
	stranger := request
	stranger.Scope = "key:OTHER"
	if replay, _, err := s.Begin(ctx, stranger); err != nil || replay != nil {
		t.Errorf("another caller with the key was given an answer: %+v, %v", replay, err)
	}

	// A failure of the server, and an answer too large to keep, free the key.
	for name, failed := range map[string]app.IdempotentAnswer{
		"server error": {Status: 503},
		"too large":    {Status: 200, Body: make([]byte, app.MaxIdempotentAnswer+1)},
	} {
		req := app.IdempotentRequest{Scope: "key:AKID", Key: name, Fingerprint: "f"}
		_, res, err := s.Begin(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Complete(ctx, res, failed); err != nil {
			t.Fatalf("%s: Complete: %v", name, err)
		}
		if replay, _, err := s.Begin(ctx, req); err != nil || replay != nil {
			t.Errorf("%s: the key was not freed: %+v, %v", name, replay, err)
		}
	}
	// A reservation nobody completed goes stale: the node stopped.
	abandoned := app.IdempotentRequest{Scope: "key:AKID", Key: "crashed", Fingerprint: "f"}
	if _, _, err := s.Begin(ctx, abandoned); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if _, _, err := s.Begin(ctx, abandoned); !errors.Is(err, domain.ErrRequestInProgress) {
		t.Errorf("a fresh reservation = %v", err)
	}
	clock.Advance(15 * time.Minute)
	_, late, err := s.Begin(ctx, abandoned)
	if err != nil {
		t.Fatalf("a stale reservation was not taken over: %v", err)
	}
	if err := s.Abandon(ctx, late); err != nil {
		t.Fatal(err)
	}

	// A request that outlives its reservation does not undo the repeat
	// that took the key over: whatever the slow one does at its end, the
	// repeat's reservation and answer stand.
	slow := app.IdempotentRequest{Scope: "key:AKID", Key: "slow", Fingerprint: "f"}
	_, original, err := s.Begin(ctx, slow)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(16 * time.Minute)
	_, takeover, err := s.Begin(ctx, slow)
	if err != nil {
		t.Fatalf("takeover of a stale reservation: %v", err)
	}
	if err := s.Complete(ctx, original, app.IdempotentAnswer{Status: 503}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Begin(ctx, slow); !errors.Is(err, domain.ErrRequestInProgress) {
		t.Fatalf("after the slow original gave up, a third request = %v; want %v: the repeat is still running", err, domain.ErrRequestInProgress)
	}
	if err := s.Complete(ctx, original, app.IdempotentAnswer{Status: 200, Body: []byte("from the original")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, takeover, app.IdempotentAnswer{Status: 201, Body: []byte("from the repeat")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Abandon(ctx, original); err != nil {
		t.Fatal(err)
	}
	if replay, _, err := s.Begin(ctx, slow); err != nil || replay == nil || string(replay.Body) != "from the repeat" {
		t.Fatalf("the answer after a takeover = %+v, %v; want the repeat's", replay, err)
	}
	if err := s.Abandon(ctx, app.Reservation{}); err != nil {
		t.Errorf("abandoning nothing: %v", err)
	}
	if err := s.Complete(ctx, app.Reservation{}, answer); err != nil {
		t.Errorf("completing nothing: %v", err)
	}

	// After a day the answer is forgotten, with or without a sweep.
	clock.Advance(app.IdempotencyTTL + time.Minute)
	if replay, _, err := s.Begin(ctx, request); err != nil || replay != nil {
		t.Errorf("an answer older than the TTL = %+v, %v", replay, err)
	}
	clock.Advance(app.IdempotencyTTL + time.Minute)
	if n, err := s.Sweep(ctx); err != nil || n == 0 {
		t.Errorf("Sweep = %d, %v", n, err)
	}
	// A store that fails is an error, never a second execution.
	store.FailWrites = errBoom
	if _, _, err := s.Begin(ctx, app.IdempotentRequest{Scope: "s", Key: "k", Fingerprint: "f"}); !errors.Is(err, errBoom) {
		t.Errorf("Begin with a failing store = %v", err)
	}
	if _, err := s.Sweep(ctx); !errors.Is(err, errBoom) {
		t.Errorf("Sweep with a failing store = %v", err)
	}
}

// hashOfIdempotent finds the one completed record of the store.
func hashOfIdempotent(t *testing.T, store *porttest.MemIAM) string {
	t.Helper()
	ids := store.IdempotentIDs()
	for _, id := range ids {
		if r, err := store.GetIdempotent(context.Background(), id); err == nil && r.Done {
			return id
		}
	}
	t.Fatal("no completed idempotency record")
	return ""
}

type staticView struct {
	topology cluster.Topology
	err      error
}

func (v staticView) Topology(context.Context) (cluster.Topology, error) { return v.topology, v.err }

func (v staticView) Nodes(context.Context) ([]cluster.Node, error) {
	return []cluster.Node{{ID: "n1", Roles: cluster.AllRoles()}, {ID: "n2", Roles: cluster.AllRoles()}}, v.err
}

func TestClusterStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := porttest.NewClock(testNow)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	trail := porttest.NewMemIAM()
	authz := app.NewAuthorizer(logger, clock, app.NewAuditor(trail, clock, logger))
	topology, err := cluster.NewTopology([]string{"d1", "d2", "d3", "d4"})
	if err != nil {
		t.Fatal(err)
	}
	maintenance := app.NewMaintenance("n1", clock)
	newService := func(view ports.ClusterView) *app.ClusterService {
		return app.NewClusterService(app.ClusterServiceConfig{
			View: view, Authz: authz, NodeID: "n1", Leader: func() string { return "n2" }, Maintenance: maintenance,
			Probe: func(_ context.Context, slot cluster.Slot) bool { return slot.DriveID != "d3" },
		})
	}
	s := newService(staticView{topology: topology})
	root := iam.Principal{AccessKeyID: "ROOT", Root: true}
	allowed, err := iam.ParsePolicy([]byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"admin:GetClusterStatus","Resource":"*"}]}`))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		principal iam.Principal
		want      error
	}{
		{"root", root, nil},
		{"a user who may", iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{allowed}}, nil},
		{"a user with no policy", iam.Principal{AccessKeyID: "K", User: "u"}, domain.ErrAccessDenied},
		{"nobody", iam.Principal{}, domain.ErrAccessDenied},
	}
	for _, tt := range tests {
		status, err := s.Status(ctx, tt.principal)
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: Status = %v, want %v", tt.name, err, tt.want)
		}
		if _, err := s.Maintenance(ctx, tt.principal); !errors.Is(err, tt.want) {
			t.Errorf("%s: Maintenance = %v, want %v", tt.name, err, tt.want)
		}
		if tt.want != nil {
			if status.DrivesTotal != 0 || status.Nodes != nil {
				t.Errorf("%s: a refusal came with a status: %+v", tt.name, status)
			}
			continue
		}
		if status.AnsweredBy != "n1" || status.Leader != "n2" || len(status.Nodes) != 2 || status.DrivesTotal != 4 || status.DrivesOnline != 3 ||
			status.DeploymentID != topology.DeploymentID {
			t.Errorf("%s: status = %+v", tt.name, status)
		}
		for _, set := range status.Sets {
			for _, d := range set {
				if d.Online == (d.ID == "d3") {
					t.Errorf("drive %s online = %v", d.ID, d.Online)
				}
			}
		}
	}
	if _, err := newService(staticView{err: domain.ErrNoTopology}).Status(ctx, root); !errors.Is(err, domain.ErrNoTopology) {
		t.Errorf("Status without a topology = %v", err)
	}

	maintenance.ObserveScrub(app.ScrubStats{Objects: 3})
	clock.Advance(time.Minute)
	maintenance.ObserveScrub(app.ScrubStats{Objects: 5, Damaged: 1})
	maintenance.ObserveGC(app.GCStats{Shards: 9, Removed: 2})
	maintenance.ObserveHeal(app.HealResult{})
	maintenance.ObserveHeal(app.HealResult{Damaged: 2, Rebuilt: 2})
	maintenance.ObserveHeal(app.HealResult{Damaged: 3, Unrecoverable: true})
	m, err := s.Maintenance(ctx, root)
	if err != nil || m.NodeID != "n1" || !m.StartedAt.Equal(testNow) || m.ScrubPasses != 2 || m.LastScrub.Objects != 5 ||
		!m.LastScrubAt.Equal(testNow.Add(time.Minute)) || m.GCPasses != 1 || m.LastGC.Removed != 2 ||
		m.HealedObjects != 2 || m.HealedShards != 2 || m.UnrecoverableObject != 1 {
		t.Errorf("maintenance = %+v, %v", m, err)
	}
}

// gatedHasher is a password hasher that takes as long as the test says.
type gatedHasher struct {
	porttest.Hasher
	checks  atomic.Int32
	release chan struct{}
}

func (g *gatedHasher) Verify(encoded, password string) (bool, error) {
	g.checks.Add(1)
	<-g.release
	return g.Hasher.Verify(encoded, password)
}

// TestGuessesAtTheSameMomentAreCounted: an address gets its ten attempts
// whether it makes them one by one or all at once. Counting only after the
// slow check would let every parallel guess through.
func TestGuessesAtTheSameMomentAreCounted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, clock := porttest.NewMemIAM(), porttest.NewClock(testNow)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hasher := &gatedHasher{release: make(chan struct{})}
	slow, err := app.NewLocalIdentityProvider(store, hasher, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sessions := app.NewSessionService(app.SessionServiceConfig{
		Sessions: store, Users: store, Provider: slow, Auditor: app.NewAuditor(store, clock, logger), Clock: clock, Entropy: rand.Reader,
	})
	from := iam.Session{SourceIP: netip.MustParseAddr("203.0.113.77")}

	const guesses = 60
	results := make(chan error, guesses)
	for i := range guesses {
		go func() {
			_, err := sessions.SignIn(ctx, "nobody", fmt.Sprintf("guess %d", i), from)
			results <- err
		}()
	}
	// Those that were let through are waiting in the hasher; the others
	// have been refused already.
	refused := 0
	for refused < guesses-10 {
		select {
		case err := <-results:
			if !errors.Is(err, domain.ErrTooManyAttempts) {
				t.Fatalf("a guess ended with %v before any password was checked", err)
			}
			refused++
		case <-time.After(10 * time.Second):
			t.Fatalf("%d guesses were refused and %d reached the password check; want %d and 10", refused, hasher.checks.Load(), guesses-10)
		}
	}
	close(hasher.release)
	for range 10 {
		if err := <-results; !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("a guess that was checked = %v", err)
		}
	}
	if got := hasher.checks.Load(); got != 10 {
		t.Errorf("%d passwords were checked, want 10", got)
	}
	// Another address is not affected, and a sign-in that fails for
	// another reason than a wrong guess does not count.
	if _, err := sessions.SignIn(ctx, "nobody", "x", iam.Session{SourceIP: netip.MustParseAddr("203.0.113.78")}); !errors.Is(err, domain.ErrAccessDenied) {
		t.Errorf("another address = %v", err)
	}
}

// countingIdempotency counts what the idempotency use case asks of its
// store.
type countingIdempotency struct {
	ports.IdempotencyStore
	begun, read atomic.Int64
}

func (c *countingIdempotency) BeginIdempotent(ctx context.Context, r ports.IdempotencyRecord) error {
	c.begun.Add(1)
	return c.IdempotencyStore.BeginIdempotent(ctx, r)
}

func (c *countingIdempotency) GetIdempotent(ctx context.Context, id string) (ports.IdempotencyRecord, error) {
	c.read.Add(1)
	return c.IdempotencyStore.GetIdempotent(ctx, id)
}

// TestIdempotencyOfALockedSession: a stored answer is given without the use
// case being asked again - and that is no way round the lock of a console
// session (ADR-0027). A locked session is given no stored answer and reserves
// no key: it is refused, the refusal is in the audit trail, and the store is
// not even asked. The one request it may make, an upload, it may repeat.
func TestIdempotencyOfALockedSession(t *testing.T) {
	t.Parallel()
	alice := iam.Principal{User: "alice", Session: session("198.51.100.7")}
	locked := alice
	locked.LockedSession = true
	answer := app.IdempotentAnswer{Status: 201, ContentType: "application/json", Body: []byte(`{"secret_key":"shown once"}`)}
	tests := map[string]struct {
		// stored says that the request was carried out before, while the
		// session was not locked.
		stored bool
		upload bool
		// bare is a service that was given no authorizer.
		bare bool
	}{
		"the answer of a change made before":     {stored: true},
		"a change made now":                      {},
		"without an authorizer, a stored answer": {stored: true, bare: true},
		"without an authorizer, a change":        {bare: true},
		"the answer of an upload made before":    {stored: true, upload: true},
		"an upload made now":                     {upload: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			mem, clock, logs := porttest.NewMemIAM(), porttest.NewClock(testNow), &syncBuffer{}
			kms, err := porttest.NewKMS("test:1")
			if err != nil {
				t.Fatal(err)
			}
			logger := slog.New(slog.NewJSONHandler(logs, nil))
			auditor := app.NewAuditor(mem, clock, logger)
			store := &countingIdempotency{IdempotencyStore: mem}
			s := app.NewIdempotencyService(store, kms, clock, rand.Reader)
			if !tt.bare {
				s = s.WithAuthorizer(app.NewAuthorizer(logger, clock, auditor))
			}
			request := app.IdempotentRequest{Scope: "session:alice", Key: "k-1", Fingerprint: "POST /users/alice/keys {}", Principal: alice, Upload: tt.upload}
			if tt.stored {
				_, reservation, err := s.Begin(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Complete(ctx, reservation, answer); err != nil {
					t.Fatal(err)
				}
				// Not locked, the repeat is given the answer.
				if replay, _, err := s.Begin(ctx, request); err != nil || replay == nil || !bytes.Equal(replay.Body, answer.Body) {
					t.Fatalf("the repeat by a session that is not locked = %+v, %v", replay, err)
				}
			}
			begun, read := store.begun.Load(), store.read.Load()

			request.Principal = locked
			replay, reservation, err := s.Begin(ctx, request)
			auditor.Flush(ctx)
			trail, listErr := mem.ListAudit(ctx, 0, 100)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if tt.upload {
				// Decided by the use case behind the key, as ever.
				if err != nil || (replay != nil) != tt.stored || len(trail) != 0 {
					t.Fatalf("Begin for the upload of a locked session = %+v, %v; %d audit records", replay, err, len(trail))
				}
				if tt.stored && !bytes.Equal(replay.Body, answer.Body) {
					t.Errorf("the repeat of an upload is answered %s", replay.Body)
				}
				if !tt.stored {
					if err := s.Abandon(ctx, reservation); err != nil {
						t.Fatal(err)
					}
				}
				return
			}
			if !errors.Is(err, domain.ErrSessionLocked) || errors.Is(err, domain.ErrAccessDenied) || replay != nil {
				t.Fatalf("Begin for a locked session = %+v, %v; want the session is locked", replay, err)
			}
			if err := s.Complete(ctx, reservation, answer); err != nil || store.begun.Load() != begun || store.read.Load() != read {
				t.Errorf("a locked session reserved, or read, a key: %d reservations, %d readings more (%v)",
					store.begun.Load()-begun, store.read.Load()-read, err)
			}
			if tt.bare {
				return
			}
			if len(trail) != 1 || trail[0].Outcome != audit.Denied || trail[0].Actor != "user/alice" || trail[0].Action != string(app.ActionRepeatRequest) ||
				trail[0].Details["reason"] != "the session is locked" || trail[0].SourceIP != "198.51.100.7" {
				t.Errorf("the audit trail = %+v", trail)
			}
			if !strings.Contains(logs.String(), "the session is locked") || strings.Contains(logs.String(), "shown once") {
				t.Errorf("the log: %s", logs.String())
			}
		})
	}
}

// TestIdempotencyConcealsSecrets: the values of a request that may be
// secrets are recognised again - the same request with the same key is
// answered as before, the same key with another secret is refused - and are
// kept neither as they are nor as a plain hash.
func TestIdempotencyConcealsSecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, clock := porttest.NewMemIAM(), porttest.NewClock(testNow)
	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}
	const concealed = "a-password-of-the-request"
	request := app.IdempotentRequest{
		Scope: "key:AKID", Key: "k-1", Operation: "PUT /api/v1/targets/hook",
		Fingerprint: "PUT /api/v1/targets/hook {redacted}", Concealed: []string{"auth.secret=" + concealed},
	}

	// When the secrets cannot be concealed the request is refused, says
	// why, and reserves nothing: the same request is taken afterwards.
	for name, tt := range map[string]struct {
		hasher ports.PasswordHasher
		want   error
	}{
		"no hasher":            {nil, nil},
		"a hasher unavailable": {&failingHasher{err: domain.ErrUnavailable}, domain.ErrUnavailable},
		"a hasher overloaded":  {&failingHasher{err: domain.ErrTooManyRequests}, domain.ErrTooManyRequests},
	} {
		refusing := app.NewIdempotencyService(store, kms, clock, rand.Reader)
		if tt.hasher != nil {
			refusing.WithHasher(tt.hasher)
		}
		_, _, err := refusing.Begin(ctx, request)
		if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) || !strings.Contains(err.Error(), "idempotency.fingerprint") {
			t.Errorf("%s: Begin = %v, want %v from idempotency.fingerprint", name, err, tt.want)
		}
	}

	s := app.NewIdempotencyService(store, kms, clock, rand.Reader).WithHasher(&porttest.Hasher{})
	_, reservation, err := s.Begin(ctx, request)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := s.Complete(ctx, reservation, app.IdempotentAnswer{Status: 200, Body: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if replay, _, err := s.Begin(ctx, request); err != nil || replay == nil {
		t.Fatalf("the same request again = %v, %v; want the answer", replay, err)
	}
	for name, change := range map[string]func(r *app.IdempotentRequest){
		"another secret":    func(r *app.IdempotentRequest) { r.Concealed = []string{"auth.secret=another"} },
		"no secret":         func(r *app.IdempotentRequest) { r.Concealed = nil },
		"another operation": func(r *app.IdempotentRequest) { r.Operation = "PUT /api/v1/targets/other" },
	} {
		other := request
		change(&other)
		if _, _, err := s.Begin(ctx, other); !errors.Is(err, domain.ErrIdempotencyKeyReused) {
			t.Errorf("%s with the same key = %v, want ErrIdempotencyKeyReused", name, err)
		}
	}

	plain := sha256.Sum256([]byte(concealed))
	plainField := sha256.Sum256([]byte("auth.secret=" + concealed))
	dump := fmt.Sprintf("%+v", store)
	for name, mark := range map[string]string{
		"the secret": concealed, "its plain SHA-256": hex.EncodeToString(plain[:]), "the plain SHA-256 of the field": hex.EncodeToString(plainField[:]),
	} {
		if strings.Contains(dump, mark) {
			t.Errorf("the stored record holds %s", name)
		}
	}
}

// failingHasher conceals nothing: it answers err.
type failingHasher struct {
	porttest.Hasher
	err error
}

// Conceal implements ports.PasswordHasher.
func (h *failingHasher) Conceal(context.Context, []byte, []byte) ([]byte, error) {
	return nil, h.err
}

// blockingHasher conceals as the fake does, but holds every value that
// holds "hold" until it is let go.
type blockingHasher struct {
	porttest.Hasher
	entered chan struct{}
	release chan struct{}
}

// Conceal implements ports.PasswordHasher.
func (h *blockingHasher) Conceal(ctx context.Context, value, salt []byte) ([]byte, error) {
	if bytes.Contains(value, []byte("hold")) {
		h.entered <- struct{}{}
		<-h.release
	}
	return h.Hasher.Conceal(ctx, value, salt)
}

// TestOneConcealedRequestAtATimeForACaller: while a request of a caller is
// being concealed, a second of the same caller is refused at once as too
// many and reserves nothing; another caller is not held up by it; once the
// first is done, the caller is taken again.
func TestOneConcealedRequestAtATimeForACaller(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, clock := porttest.NewMemIAM(), porttest.NewClock(testNow)
	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}
	hasher := &blockingHasher{entered: make(chan struct{}), release: make(chan struct{})}
	s := app.NewIdempotencyService(store, kms, clock, rand.Reader).WithHasher(hasher)
	request := func(scope, key, body string) app.IdempotentRequest {
		return app.IdempotentRequest{Scope: scope, Key: key, Operation: "PUT /api/v1/targets/hook", Fingerprint: "f", Concealed: []string{"body=" + body}}
	}

	first := make(chan error, 1)
	go func() {
		_, _, err := s.Begin(ctx, request("key:AKID", "k-1", "hold"))
		first <- err
	}()
	<-hasher.entered

	begun := time.Now()
	if _, _, err := s.Begin(ctx, request("key:AKID", "k-2", "another")); !errors.Is(err, domain.ErrTooManyRequests) {
		t.Errorf("a second concealed request of the caller = %v, want ErrTooManyRequests", err)
	}
	if waited := time.Since(begun); waited > time.Second {
		t.Errorf("the refusal took %v: it is to be at once", waited)
	}
	if _, reservation, err := s.Begin(ctx, request("key:OTHER", "k-2", "another")); err != nil || reservation == (app.Reservation{}) {
		t.Errorf("another caller, while the first is concealed = %v", err)
	}

	hasher.release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatalf("the first request: %v", err)
	}
	// Nothing was reserved by the refusal: the key is taken now.
	if _, reservation, err := s.Begin(ctx, request("key:AKID", "k-2", "another")); err != nil || reservation == (app.Reservation{}) {
		t.Errorf("the refused request, sent again = %v", err)
	}
}
