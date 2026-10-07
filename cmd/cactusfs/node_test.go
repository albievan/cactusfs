package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

const nodePassphrase = "unit test passphrase"

// nodeConfig returns the configuration of a development node in dir.
func nodeConfig(dir string) serverConfig {
	meta := filepath.Join(dir, "d1", metaDirName)
	return serverConfig{
		Data: []string{filepath.Join(dir, "d1"), filepath.Join(dir, "d2")}, MetaDir: meta,
		KMS: kmsLocal, KMSKeyFile: filepath.Join(meta, kmsKeyFileName),
		secrets: secrets{kmsPassphrase: nodePassphrase},
		raftTiming: raftmeta.Timing{
			Heartbeat: 50 * time.Millisecond, Election: 50 * time.Millisecond,
			LeaderLease: 50 * time.Millisecond, Commit: 5 * time.Millisecond,
		},
	}
}

type started struct {
	node   *node
	notice string
	logs   string
}

func startNode(t *testing.T, cfg serverConfig) (started, error) {
	t.Helper()
	var notice, logs bytes.Buffer
	n, err := openNode(t.Context(), cfg, slog.New(slog.NewJSONHandler(&logs, nil)), &notice)
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
	if !strings.Contains(first.logs, "without parity") {
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
	if first.notice != "" {
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
	if _, err := openNode(ctx, nodeConfig(t.TempDir()), slog.New(slog.DiscardHandler), &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
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
