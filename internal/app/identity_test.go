package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// testCredentials builds a credential at run time, so that no key material is
// written in the source.
func testCredentials(t *testing.T) *app.Credentials {
	t.Helper()
	id, err := iam.GenerateAccessKeyID(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateAccessKeyID: %v", err)
	}
	secret, err := iam.GenerateSecret(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	return &app.Credentials{AccessKeyID: id, SecretKey: secret}
}

func newIdentity(t *testing.T) (*app.IdentityService, *porttest.MemMetadata, *porttest.KMS) {
	t.Helper()
	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatalf("NewKMS: %v", err)
	}
	meta := porttest.NewMemMetadata()
	return app.NewIdentityService(meta, kms, porttest.NewClock(testNow), rand.Reader), meta, kms
}

func TestEnsureRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("generates a credential once on a new deployment", func(t *testing.T) {
		t.Parallel()
		svc, meta, _ := newIdentity(t)

		generated, err := svc.EnsureRoot(ctx, nil)
		if err != nil || generated == nil {
			t.Fatalf("EnsureRoot = %v, %v; want generated credentials", generated, err)
		}
		if err := iam.ValidateAccessKeyID(generated.AccessKeyID); err != nil {
			t.Errorf("generated access key ID: %v", err)
		}
		if err := iam.ValidateSecret(generated.SecretKey); err != nil {
			t.Errorf("generated secret: %v", err)
		}

		// Stored sealed: neither the record nor anything in it holds the secret.
		stored, err := meta.GetRootCredential(ctx)
		if err != nil {
			t.Fatalf("GetRootCredential: %v", err)
		}
		if stored.AccessKeyID != generated.AccessKeyID || !stored.CreatedAt.Equal(testNow) || stored.Secret.KMSKeyID != "test:1" {
			t.Errorf("stored root = %+v", stored)
		}
		for name, field := range map[string][]byte{
			"ciphertext": stored.Secret.Ciphertext, "wrapped key": stored.Secret.WrappedKey, "nonce": stored.Secret.Nonce,
		} {
			if bytes.Contains(field, []byte(generated.SecretKey)) {
				t.Errorf("the stored %s contains the secret in the clear", name)
			}
		}

		// A second start shows nothing: the secret cannot be shown again.
		again, err := svc.EnsureRoot(ctx, nil)
		if err != nil || again != nil {
			t.Errorf("second EnsureRoot = %v, %v; want nil, nil", again, err)
		}
		secret, principal, err := svc.SecretFor(ctx, generated.AccessKeyID)
		if err != nil || string(secret) != generated.SecretKey || !principal.Root || principal.AccessKeyID != generated.AccessKeyID {
			t.Errorf("SecretFor = %v, %+v; want the generated secret for the root principal", err, principal)
		}
	})

	t.Run("two deployments get different credentials", func(t *testing.T) {
		t.Parallel()
		a, _, _ := newIdentity(t)
		b, _, _ := newIdentity(t)
		first, err1 := a.EnsureRoot(ctx, nil)
		second, err2 := b.EnsureRoot(ctx, nil)
		if err1 != nil || err2 != nil {
			t.Fatalf("EnsureRoot: %v, %v", err1, err2)
		}
		if first.AccessKeyID == second.AccessKeyID || first.SecretKey == second.SecretKey {
			t.Error("two deployments generated the same root credential: there must be no default")
		}
	})

	t.Run("stores supplied credentials and returns nothing to show", func(t *testing.T) {
		t.Parallel()
		svc, _, _ := newIdentity(t)
		creds := testCredentials(t)

		generated, err := svc.EnsureRoot(ctx, creds)
		if err != nil || generated != nil {
			t.Fatalf("EnsureRoot = %v, %v; want nil, nil", generated, err)
		}
		// Supplying the same credentials on later starts is accepted.
		if _, err := svc.EnsureRoot(ctx, creds); err != nil {
			t.Errorf("EnsureRoot with the same credentials = %v", err)
		}
		secret, _, err := svc.SecretFor(ctx, creds.AccessKeyID)
		if err != nil || string(secret) != creds.SecretKey {
			t.Errorf("SecretFor: %v", err)
		}
	})

	t.Run("refuses supplied credentials that differ from the stored ones", func(t *testing.T) {
		t.Parallel()
		svc, _, _ := newIdentity(t)
		original := testCredentials(t)
		if _, err := svc.EnsureRoot(ctx, original); err != nil {
			t.Fatalf("EnsureRoot: %v", err)
		}
		other := testCredentials(t)
		for name, creds := range map[string]*app.Credentials{
			"another key and secret": other,
			"another secret":         {AccessKeyID: original.AccessKeyID, SecretKey: other.SecretKey},
			"another access key ID":  {AccessKeyID: other.AccessKeyID, SecretKey: original.SecretKey},
		} {
			_, err := svc.EnsureRoot(ctx, creds)
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("%s: error = %v, want %v", name, err, domain.ErrInvalidArgument)
				continue
			}
			if strings.Contains(err.Error(), creds.SecretKey) || strings.Contains(err.Error(), original.SecretKey) {
				t.Errorf("%s: the error message contains a secret", name)
			}
		}
		// The original still works.
		if _, _, err := svc.SecretFor(ctx, original.AccessKeyID); err != nil {
			t.Errorf("SecretFor(original): %v", err)
		}
	})

	t.Run("refuses invalid supplied credentials", func(t *testing.T) {
		t.Parallel()
		good := testCredentials(t)
		for name, creds := range map[string]*app.Credentials{
			"short access key ID":        {AccessKeyID: "ab", SecretKey: good.SecretKey},
			"access key ID with a slash": {AccessKeyID: "bad/key", SecretKey: good.SecretKey},
			"short secret":               {AccessKeyID: good.AccessKeyID, SecretKey: "too-short"},
			"empty":                      {},
		} {
			svc, meta, _ := newIdentity(t)
			if _, err := svc.EnsureRoot(ctx, creds); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("%s: error = %v, want %v", name, err, domain.ErrInvalidArgument)
			}
			if _, err := meta.GetRootCredential(ctx); !errors.Is(err, domain.ErrNotBootstrapped) {
				t.Errorf("%s: an invalid credential was stored", name)
			}
		}
	})

	t.Run("detects the wrong KMS key on an existing deployment", func(t *testing.T) {
		t.Parallel()
		svc, meta, _ := newIdentity(t)
		if _, err := svc.EnsureRoot(ctx, nil); err != nil {
			t.Fatalf("EnsureRoot: %v", err)
		}
		for _, keyID := range []string{"test:1", "test:2"} {
			wrongKMS, err := porttest.NewKMS(keyID)
			if err != nil {
				t.Fatalf("NewKMS: %v", err)
			}
			other := app.NewIdentityService(meta, wrongKMS, porttest.NewClock(testNow), rand.Reader)
			if generated, err := other.EnsureRoot(ctx, nil); !errors.Is(err, domain.ErrKeyUnavailable) || generated != nil {
				t.Errorf("KMS %s: EnsureRoot = %v, %v; want %v and nothing generated", keyID, generated, err, domain.ErrKeyUnavailable)
			}
		}
	})

	t.Run("reports failures", func(t *testing.T) {
		t.Parallel()

		svc, meta, _ := newIdentity(t)
		meta.FailWrites = errBoom
		if _, err := svc.EnsureRoot(ctx, nil); !errors.Is(err, errBoom) {
			t.Errorf("store failure: error = %v", err)
		}

		svc, _, kms := newIdentity(t)
		kms.FailGenerate = errBoom
		if _, err := svc.EnsureRoot(ctx, nil); !errors.Is(err, errBoom) {
			t.Errorf("KMS failure: error = %v", err)
		}

		kms2, err := porttest.NewKMS("test:1")
		if err != nil {
			t.Fatalf("NewKMS: %v", err)
		}
		noEntropy := app.NewIdentityService(porttest.NewMemMetadata(), kms2, porttest.NewClock(testNow), iotest.ErrReader(errBoom))
		if _, err := noEntropy.EnsureRoot(ctx, nil); !errors.Is(err, errBoom) {
			t.Errorf("entropy failure while generating: error = %v", err)
		}
		if _, err := noEntropy.EnsureRoot(ctx, testCredentials(t)); !errors.Is(err, errBoom) {
			t.Errorf("entropy failure while sealing: error = %v", err)
		}
	})
}

func TestSecretFor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	svc, meta, kms := newIdentity(t)
	if _, _, err := svc.SecretFor(ctx, "ANYONE"); !errors.Is(err, domain.ErrNotBootstrapped) {
		t.Errorf("before bootstrap: error = %v, want %v", err, domain.ErrNotBootstrapped)
	}

	creds := testCredentials(t)
	if _, err := svc.EnsureRoot(ctx, creds); err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}

	for _, unknown := range []string{"", "SOMEONE-ELSE", creds.AccessKeyID + "x", strings.ToLower(creds.AccessKeyID)} {
		secret, principal, err := svc.SecretFor(ctx, unknown)
		if !errors.Is(err, domain.ErrCredentialNotFound) || secret != nil || !principal.Anonymous() {
			t.Errorf("SecretFor(%q) = %v, %+v, %v; want no secret and %v", unknown, secret, principal, err, domain.ErrCredentialNotFound)
		}
	}

	kms.FailUnwrap = errBoom
	if _, _, err := svc.SecretFor(ctx, creds.AccessKeyID); !errors.Is(err, errBoom) {
		t.Errorf("KMS failure: error = %v", err)
	}
	kms.FailUnwrap = nil

	// A sealed secret moved under another access key ID does not open.
	stored, err := meta.GetRootCredential(ctx)
	if err != nil {
		t.Fatalf("GetRootCredential: %v", err)
	}
	moved := porttest.NewMemMetadata()
	stored.AccessKeyID = "ATTACKER"
	if err := moved.SetRootCredential(ctx, stored); err != nil {
		t.Fatalf("SetRootCredential: %v", err)
	}
	other := app.NewIdentityService(moved, kms, porttest.NewClock(testNow), rand.Reader)
	if secret, _, err := other.SecretFor(ctx, "ATTACKER"); !errors.Is(err, domain.ErrKeyUnavailable) || secret != nil {
		t.Errorf("a sealed secret opened under another access key ID: %v", err)
	}
}

// lateStore answers the first question about the root credential as if the
// deployment were new, which is what a node sees when another node of the
// cluster is bootstrapping at the same moment.
type lateStore struct {
	ports.RootCredentialStore
	asked bool
}

func (l *lateStore) GetRootCredential(ctx context.Context) (iam.RootCredential, error) {
	if !l.asked {
		l.asked = true
		return iam.RootCredential{}, domain.ErrNotBootstrapped
	}
	return l.RootCredentialStore.GetRootCredential(ctx)
}

// TestEnsureRootWhenAnotherNodeIsFirst: the nodes of a new cluster all try to
// bootstrap it. One wins. The others must end up with the winner's
// credential - not fail, not show a credential that was never stored, and not
// accept a supplied credential that differs from the stored one.
func TestEnsureRootWhenAnotherNodeIsFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	supplied, other := testCredentials(t), testCredentials(t)
	tests := map[string]struct {
		winner, loser *app.Credentials
		wantErr       error
	}{
		"both generate":               {},
		"both supply the same":        {winner: supplied, loser: supplied},
		"winner supplies, loser none": {winner: supplied},
		"they supply different ones":  {winner: supplied, loser: other, wantErr: domain.ErrInvalidArgument},
	}
	for name, tt := range tests {
		meta := porttest.NewMemMetadata()
		kms, err := porttest.NewKMS("test:1")
		if err != nil {
			t.Fatal(err)
		}
		clock := porttest.NewClock(testNow)
		first := app.NewIdentityService(meta, kms, clock, porttest.Entropy())
		shown, err := first.EnsureRoot(ctx, tt.winner)
		if err != nil {
			t.Fatalf("%s: winner: %v", name, err)
		}
		winning := tt.winner
		if winning == nil {
			winning = shown
		}

		second := app.NewIdentityService(&lateStore{RootCredentialStore: meta}, kms, clock, rand.Reader)
		generated, err := second.EnsureRoot(ctx, tt.loser)
		if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
			t.Errorf("%s: loser: error = %v, want %v", name, err, tt.wantErr)
		}
		if generated != nil {
			t.Errorf("%s: the loser returned credentials to show, though they were never stored", name)
		}
		// The deployment has one credential: the winner's, on both.
		secret, principal, err := second.SecretFor(ctx, winning.AccessKeyID)
		if err != nil || !principal.Root || string(secret) != winning.SecretKey {
			t.Errorf("%s: the loser does not know the winner's credential: %v", name, err)
		}
	}
}

// unsureStore stores the root credential and then reports that it does not
// know whether it did.
type unsureStore struct {
	ports.RootCredentialStore
	store bool
}

func (u *unsureStore) SetRootCredential(ctx context.Context, c iam.RootCredential) error {
	if u.store {
		if err := u.RootCredentialStore.SetRootCredential(ctx, c); err != nil {
			return err
		}
	}
	return domain.ErrOutcomeUnknown
}

// TestGeneratedRootCredentialsAreShownEvenWhenTheCommitIsUncertain: the node
// generates credentials, proposes them, and gets no answer. If they were
// stored after all they must be shown now - they are sealed, and nobody can
// ever show them again. If they were not, nothing is shown and the node
// fails, to try again.
func TestGeneratedRootCredentialsAreShownEvenWhenTheCommitIsUncertain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}

	meta := porttest.NewMemMetadata()
	svc := app.NewIdentityService(&unsureStore{RootCredentialStore: meta, store: true}, kms, porttest.NewClock(testNow), rand.Reader)
	generated, err := svc.EnsureRoot(ctx, nil)
	if err != nil || generated == nil {
		t.Fatalf("EnsureRoot when the credential was stored after all = %v, %v; want the credentials", generated, err)
	}
	if secret, principal, err := svc.SecretFor(ctx, generated.AccessKeyID); err != nil || !principal.Root || string(secret) != generated.SecretKey {
		t.Errorf("the credentials that were shown are not the stored ones: %v", err)
	}

	empty := porttest.NewMemMetadata()
	svc = app.NewIdentityService(&unsureStore{RootCredentialStore: empty}, kms, porttest.NewClock(testNow), rand.Reader)
	generated, err = svc.EnsureRoot(ctx, nil)
	if !errors.Is(err, domain.ErrOutcomeUnknown) || generated != nil {
		t.Errorf("EnsureRoot when nothing was stored = %v, %v; want the error and nothing shown", generated, err)
	}
}
