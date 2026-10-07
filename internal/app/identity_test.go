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
