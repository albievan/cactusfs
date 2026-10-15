package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/passwords"
	"github.com/albievan/cactusfs/internal/buildinfo"

	"github.com/albievan/cactusfs/internal/domain/cluster"

	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

const nodePassphrase = "unit test passphrase"

// nodeConfig returns the configuration of a development node in dir.
func nodeConfig(dir string) serverConfig {
	meta := filepath.Join(dir, "d1", metaDirName)
	return serverConfig{
		Data: []string{filepath.Join(dir, "d1"), filepath.Join(dir, "d2")}, MetaDir: meta,
		KMS: kmsLocal, KMSKeyFile: filepath.Join(meta, kmsKeyFileName), Roles: cluster.AllRoles(),
		secrets: secrets{kmsPassphrase: nodePassphrase}, passwordParams: passwords.Params{Time: 1, MemoryKiB: 8 << 10, Threads: 1},
		raftTiming: raftmeta.Timing{
			Heartbeat: 50 * time.Millisecond, Election: 50 * time.Millisecond,
			LeaderLease: 50 * time.Millisecond, Commit: 5 * time.Millisecond,
		},
	}
}

func newTelemetry(t *testing.T) *telemetry.Telemetry {
	t.Helper()
	tel, err := telemetry.New(t.Context(), telemetry.Config{Version: buildinfo.Get().Tag()})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	return tel
}

type started struct {
	node   *node
	notice string
	logs   string
}

func startNode(t *testing.T, cfg serverConfig) (started, error) {
	t.Helper()
	var notice, logs bytes.Buffer
	n, err := openNode(t.Context(), cfg, newTelemetry(t), slog.New(slog.NewJSONHandler(&logs, nil)), &notice)
	if err != nil {
		return started{notice: notice.String(), logs: logs.String()}, err
	}
	t.Cleanup(func() { _ = n.Close() })
	return started{node: n, notice: notice.String(), logs: logs.String()}, nil
}

func mustStart(t *testing.T, cfg serverConfig) started {
	t.Helper()
	s, err := startNode(t, cfg)
	if err != nil {
		t.Fatalf("openNode: %v", err)
	}
	return s
}

func shownCredentials(t *testing.T, notice string) app.Credentials {
	t.Helper()
	id := regexp.MustCompile(`Access key ID: (\S+)`).FindStringSubmatch(notice)
	secret := regexp.MustCompile(`Secret key: +(\S+)`).FindStringSubmatch(notice)
	if id == nil || secret == nil {
		t.Fatalf("no credentials in the notice: %q", notice)
	}
	return app.Credentials{AccessKeyID: id[1], SecretKey: secret[1]}
}

func TestNewDeploymentGeneratesRootCredentialsOnce(t *testing.T) {
	t.Parallel()
	cfg := nodeConfig(t.TempDir())

	first := mustStart(t, cfg)
	creds := shownCredentials(t, first.notice)
	if iam.ValidateAccessKeyID(creds.AccessKeyID) != nil || iam.ValidateSecret(creds.SecretKey) != nil {
		t.Errorf("generated credentials are not valid: %q", creds.AccessKeyID)
	}
	// Shown on the terminal, never written to the log.
	if strings.Contains(first.logs, creds.SecretKey) {
		t.Fatal("the log contains the generated secret key")
	}
	if strings.Contains(first.logs, nodePassphrase) {
		t.Fatal("the log contains the KMS passphrase")
	}
	if !strings.Contains(first.logs, "WITHOUT PARITY") {
		t.Error("the log does not say that objects are stored without parity")
	}
	secret, principal, err := first.node.identities.SecretFor(t.Context(), creds.AccessKeyID)
	if err != nil || string(secret) != creds.SecretKey || !principal.Root {
		t.Fatalf("the shown credentials do not authenticate: %v", err)
	}
	if err := first.node.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Later starts show nothing: the secret is sealed and cannot be shown.
	second := mustStart(t, cfg)
	if second.notice != "" {
		t.Errorf("a restart wrote to the terminal: %q", second.notice)
	}
	if _, _, err := second.node.identities.SecretFor(t.Context(), creds.AccessKeyID); err != nil {
		t.Errorf("the credentials stopped working after a restart: %v", err)
	}
	if len(second.node.drives) != 2 || second.node.describe() == "" {
		t.Errorf("drives = %d", len(second.node.drives))
	}

	// Two deployments never share a credential: there is no default.
	other := mustStart(t, nodeConfig(t.TempDir()))
	if otherCreds := shownCredentials(t, other.notice); otherCreds == creds {
		t.Error("two deployments generated the same root credentials")
	}
}

func TestSuppliedRootCredentials(t *testing.T) {
	t.Parallel()
	cfg := nodeConfig(t.TempDir())
	id, err := iam.GenerateAccessKeyID(strings.NewReader(strings.Repeat("k", 64)))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := iam.GenerateSecret(strings.NewReader(strings.Repeat("s", 64)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.secrets.rootAccessKey, cfg.secrets.rootSecretKey = id, secret

	first := mustStart(t, cfg)
	// The notice speaks of the default console user (ADR-0014), and of
	// nothing that was supplied.
	if strings.Contains(first.notice, id) || strings.Contains(first.notice, secret) || strings.Contains(first.notice, "Access key ID") {
		t.Errorf("supplied credentials were printed: %q", first.notice)
	}
	if strings.Contains(first.logs, secret) {
		t.Fatal("the log contains the supplied secret key")
	}
	if got, _, err := first.node.identities.SecretFor(t.Context(), id); err != nil || string(got) != secret {
		t.Fatalf("the supplied credentials do not authenticate: %v", err)
	}
	if err := first.node.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The same credentials again are fine; different ones are refused, not
	// silently ignored and not silently adopted.
	again := mustStart(t, cfg)
	if err := again.node.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cfg.secrets.rootSecretKey = strings.Repeat("x", 40)
	if _, err := startNode(t, cfg); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("start with different root credentials: error = %v, want %v", err, domain.ErrInvalidArgument)
	}
	cfg.secrets.rootAccessKey, cfg.secrets.rootSecretKey = "", ""
	mustStart(t, cfg)
}

func TestKMSMistakesStopTheNode(t *testing.T) {
	t.Parallel()

	t.Run("wrong passphrase", func(t *testing.T) {
		t.Parallel()
		cfg := nodeConfig(t.TempDir())
		first := mustStart(t, cfg)
		if err := first.node.Close(); err != nil {
			t.Fatal(err)
		}
		cfg.secrets.kmsPassphrase = "another passphrase"
		if _, err := startNode(t, cfg); !errors.Is(err, domain.ErrKeyUnavailable) {
			t.Errorf("error = %v, want %v", err, domain.ErrKeyUnavailable)
		}
	})

	t.Run("key file missing on an existing deployment", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		cfg := nodeConfig(dir)
		first := mustStart(t, cfg)
		if err := first.node.Close(); err != nil {
			t.Fatal(err)
		}
		// Pointing at a path with no key file must not create a new key:
		// every stored object would become unreadable.
		cfg.KMSKeyFile = filepath.Join(dir, "elsewhere", "key.json")
		_, err := startNode(t, cfg)
		if err == nil || !strings.Contains(err.Error(), "already has metadata") {
			t.Fatalf("error = %v, want a refusal to create a new key", err)
		}
		if exists, _ := fileExists(cfg.KMSKeyFile); exists {
			t.Error("a new key file was created for an existing deployment")
		}
	})

	t.Run("another key file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		cfg := nodeConfig(dir)
		first := mustStart(t, cfg)
		if err := first.node.Close(); err != nil {
			t.Fatal(err)
		}
		// A key file from another deployment opens with its passphrase but
		// cannot unwrap this deployment's root secret.
		foreign := nodeConfig(t.TempDir())
		other := mustStart(t, foreign)
		if err := other.node.Close(); err != nil {
			t.Fatal(err)
		}
		cfg.KMSKeyFile = foreign.KMSKeyFile
		if _, err := startNode(t, cfg); !errors.Is(err, domain.ErrKeyUnavailable) {
			t.Errorf("error = %v, want %v", err, domain.ErrKeyUnavailable)
		}
	})

	t.Run("short passphrase", func(t *testing.T) {
		t.Parallel()
		cfg := nodeConfig(t.TempDir())
		cfg.secrets.kmsPassphrase = "short"
		if _, err := startNode(t, cfg); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("error = %v, want %v", err, domain.ErrInvalidArgument)
		}
	})
}

func TestPassphraseFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "passphrase.txt")
	writeTestFile(t, file, "  "+nodePassphrase+"\n")

	cfg := nodeConfig(dir)
	cfg.secrets.kmsPassphrase, cfg.KMSPassphraseFile = "", file
	first := mustStart(t, cfg)
	if err := first.node.Close(); err != nil {
		t.Fatal(err)
	}
	// The file and the variable are the same passphrase: whitespace around
	// the file's content is not part of it.
	cfg.secrets.kmsPassphrase, cfg.KMSPassphraseFile = nodePassphrase, ""
	mustStart(t, cfg)

	for name, content := range map[string]string{"empty": " \n", "huge": strings.Repeat("p", maxPassphraseFile+1)} {
		bad := filepath.Join(dir, name+".txt")
		writeTestFile(t, bad, content)
		if _, err := readPassphrase(serverConfig{KMSPassphraseFile: bad}); err == nil {
			t.Errorf("%s passphrase file was accepted", name)
		}
	}
	if _, err := readPassphrase(serverConfig{KMSPassphraseFile: filepath.Join(dir, "absent.txt")}); err == nil {
		t.Error("a missing passphrase file was accepted")
	}
	if _, err := readPassphrase(serverConfig{KMSPassphraseFile: filepath.Join(dir, "no", "dir", "p.txt")}); err == nil {
		t.Error("a passphrase file in a missing directory was accepted")
	}
}

func TestOpenNodeReleasesEverythingOnFailure(t *testing.T) {
	t.Parallel()
	cfg := nodeConfig(t.TempDir())
	cfg.secrets.kmsPassphrase = "short"
	if _, err := startNode(t, cfg); err == nil {
		t.Fatal("openNode succeeded with a short passphrase")
	}
	// The drives were unlocked again: a corrected start works.
	cfg.secrets.kmsPassphrase = nodePassphrase
	mustStart(t, cfg)
}

func TestOpenNodeHonoursCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := openNode(ctx, nodeConfig(t.TempDir()), newTelemetry(t), slog.New(slog.DiscardHandler), &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want %v", err, context.Canceled)
	}
}

func TestSystemClock(t *testing.T) {
	t.Parallel()
	before := time.Now()
	if got := (systemClock{}).Now(); got.Before(before) || got.After(before.Add(time.Minute)) {
		t.Errorf("Now() = %v", got)
	}
}

// TestDefaultConsoleUser: a new deployment can be signed in to as admin with
// the password admin (ADR-0014), is told so loudly, and a deployment whose
// operator deleted that user does not get it back.
func TestDefaultConsoleUser(t *testing.T) {
	t.Parallel()
	cfg := nodeConfig(t.TempDir())
	first := mustStart(t, cfg)
	if !strings.Contains(first.notice, `console user "admin"`) {
		t.Errorf("nothing on standard error about the default console user: %q", first.notice)
	}
	if !strings.Contains(first.logs, "default password") {
		t.Errorf("nothing in the log about the default password:\n%s", first.logs)
	}
	visitor := iam.Session{SourceIP: netip.MustParseAddr("127.0.0.1"), SecureTransport: true, RequestID: "REQ-1"}
	if _, err := first.node.sessions.SignIn(t.Context(), app.DefaultAdminName, app.DefaultAdminPassword, visitor); err != nil {
		t.Fatalf("sign-in with the default user: %v", err)
	}
	root := iam.Principal{AccessKeyID: "ROOT", Root: true, Session: visitor}
	if err := first.node.iam.DeleteUser(t.Context(), root, app.DefaultAdminName); err != nil {
		t.Fatal(err)
	}
	if err := first.node.Close(); err != nil {
		t.Fatal(err)
	}
	again := mustStart(t, cfg)
	if _, err := again.node.sessions.SignIn(t.Context(), app.DefaultAdminName, app.DefaultAdminPassword, visitor); err == nil {
		t.Error("the deleted default user is back after a restart")
	}
	if strings.Contains(again.logs, "default password") {
		t.Errorf("a second start speaks of the default password:\n%s", again.logs)
	}
}

// TestANodeRecordsWhoReadWhat: a node as it is wired counts a download
// under its own name, hands the count to its metadata store when it stops,
// and has it again when it starts (ADR-0016).
func TestANodeRecordsWhoReadWhat(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cfg := nodeConfig(t.TempDir())
	first := mustStart(t, cfg)
	n := first.node
	n.startBackground(ctx, cfg)
	root := iam.Principal{AccessKeyID: "ROOT", Root: true}
	if err := n.buckets.Create(ctx, root, "photos"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	content := []byte("a picture")
	stored, err := n.objects.Put(ctx, root, app.PutInput{Bucket: "photos", Key: "cat.jpg", Body: bytes.NewReader(content), Size: int64(len(content))})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if stored.CreatedBy != "root" {
		t.Errorf("the version was made by %q, want root", stored.CreatedBy)
	}
	for range 2 {
		out, err := n.objects.Get(ctx, root, app.GetInput{Bucket: "photos", Key: "cat.jpg"})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if err := out.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Counted, and not handed over yet: the interval has not passed.
	if got, err := n.objects.Access(ctx, root, "photos", "cat.jpg", ""); err != nil || got.Access.Downloads != 0 || n.access.Buffered() != 1 {
		t.Fatalf("Access before a flush = %+v, %v; %d aggregates wait", got.Access, err, n.access.Buffered())
	}
	// Stopping in good order hands it over.
	if err := n.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again := mustStart(t, cfg).node
	got, err := again.objects.Access(ctx, root, "photos", "cat.jpg", "")
	if err != nil {
		t.Fatalf("Access: %v", err)
	}
	if got.Version.CreatedBy != "root" || got.Access.Downloads != 2 || len(got.Access.Readers) != 1 || got.Access.Readers[0].Who != "root" {
		t.Errorf("after a restart: uploaded by %q, access %+v", got.Version.CreatedBy, got.Access)
	}
	// The node counts again from one, under another number of its start:
	// the batch is not taken for one that was seen.
	out, err := again.objects.Get(ctx, root, app.GetInput{Bucket: "photos", Key: "cat.jpg"})
	if err != nil {
		t.Fatal(err)
	}
	_ = out.Body.Close()
	if err := again.access.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got, err := again.objects.Access(ctx, root, "photos", "cat.jpg", ""); err != nil || got.Access.Downloads != 3 {
		t.Errorf("after another download: %+v, %v", got.Access, err)
	}
	if scraped := scrapeCounter(t, again.telemetry, "cactusfs_object_access_dropped_total"); scraped != 0 {
		t.Errorf("%v reads were dropped", scraped)
	}
}

// scrapeCounter returns the value of a counter without labels.
func scrapeCounter(t *testing.T, tel *telemetry.Telemetry, name string) float64 {
	t.Helper()
	families, err := tel.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name {
			return family.GetMetric()[0].GetCounter().GetValue()
		}
	}
	t.Fatalf("no metric %s", name)
	return 0
}

// TestANodeWiresWhatEndsAndRefusesSessions holds the composition root to two
// wirings of ADR-0027 that no use case can see from inside: the IAM use case
// is given the store of sessions - without it a change of password ends no
// session - and the idempotency use case the authorizer - without it a
// locked session that repeats a request is refused and not audited.
func TestANodeWiresWhatEndsAndRefusesSessions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	n := mustStart(t, nodeConfig(t.TempDir())).node
	visitor := iam.Session{SourceIP: netip.MustParseAddr("127.0.0.1"), SecureTransport: true, RequestID: "REQ-1"}
	root := iam.Principal{AccessKeyID: "ROOT", Root: true, Session: visitor}

	t.Run("a change of password ends the sessions", func(t *testing.T) {
		signed, err := n.sessions.SignIn(ctx, app.DefaultAdminName, app.DefaultAdminPassword, visitor)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := n.sessions.Authenticate(ctx, signed.Token, "", false, visitor); err != nil {
			t.Fatalf("the session before the change: %v", err)
		}
		changed := strings.Join([]string{"a", "password", "changed", "by", "root"}, " ")
		if _, err := n.iam.UpdateUser(ctx, root, app.UserInput{Name: app.DefaultAdminName, Roles: []string{iam.RoleClusterAdmin}, Password: changed}); err != nil {
			t.Fatal(err)
		}
		if _, err := n.sessions.Authenticate(ctx, signed.Token, "", false, visitor); !errors.Is(err, domain.ErrAccessDenied) {
			t.Errorf("the session after its user's password changed = %v, want refused", err)
		}
	})

	t.Run("a locked session that repeats a request is refused, and audited", func(t *testing.T) {
		locked := iam.Principal{User: app.DefaultAdminName, LockedSession: true, Session: visitor}
		_, _, err := n.idempotency.Begin(ctx, app.IdempotentRequest{
			Scope: "user/" + app.DefaultAdminName, Key: "a-key-of-the-test", Fingerprint: "DELETE /buckets/photos", Principal: locked,
		})
		if !errors.Is(err, domain.ErrSessionLocked) {
			t.Fatalf("Begin for a locked session = %v", err)
		}
		n.auditor.Flush(ctx)
		records, err := n.meta.ListAudit(ctx, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range records {
			if r.Action == string(app.ActionRepeatRequest) && r.Actor == "user/"+app.DefaultAdminName && r.Outcome == audit.Denied {
				return
			}
		}
		t.Errorf("no audit record of the refusal among %d records", len(records))
	})
}
