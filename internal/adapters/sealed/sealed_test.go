package sealed_test

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/sealed"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// markedValue is what must never be in an error's text.
const markedValue = "MARKED-sealed-5d0a"

// failingKMS is a KMS whose unwrapping fails.
type failingKMS struct{ *porttest.KMS }

func (failingKMS) UnwrapDataKey(context.Context, []byte, []byte) ([]byte, error) {
	return nil, errors.New("the KMS is down")
}

func seal(t *testing.T, kms ports.KMS, value string, aad []byte) iam.SealedSecret {
	t.Helper()
	key, err := kms.GenerateDataKey(t.Context(), aad)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Plaintext)
	field, err := iam.Seal(key.Plaintext, rand.Reader, []byte(value), aad)
	if err != nil {
		t.Fatal(err)
	}
	field.KMSKeyID, field.WrappedKey = kms.KeyID(), key.Wrapped
	return field
}

// TestKMSOpens: a field opens with the KMS and the additional data it was
// sealed with, and with nothing else - and no failure tells the plaintext.
func TestKMSOpens(t *testing.T) {
	t.Parallel()
	kms, err := porttest.NewKMS("kms-1")
	if err != nil {
		t.Fatal(err)
	}
	aad := delivery.SecretAAD("events")
	field := seal(t, kms, markedValue, aad)

	plain, err := sealed.KMS{KMS: kms}.Open(t.Context(), field, aad)
	if err != nil || string(plain) != markedValue {
		t.Fatalf("Open = %q, %v", plain, err)
	}
	other := field
	other.KMSKeyID = "kms-2"
	tests := map[string]struct {
		opener sealed.KMS
		field  iam.SealedSecret
		aad    []byte
		is     error
	}{
		"no KMS":                      {opener: sealed.KMS{}, field: field, aad: aad, is: domain.ErrKeyUnavailable},
		"another KMS key":             {opener: sealed.KMS{KMS: kms}, field: other, aad: aad, is: domain.ErrKeyUnavailable},
		"the field of another target": {opener: sealed.KMS{KMS: kms}, field: field, aad: delivery.SecretAAD("other")},
		"the signing key's field":     {opener: sealed.KMS{KMS: kms}, field: field, aad: delivery.SignKeyAAD("events")},
		"the KMS fails":               {opener: sealed.KMS{KMS: failingKMS{kms}}, field: field, aad: aad},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plain, err := tc.opener.Open(t.Context(), tc.field, tc.aad)
			if err == nil || plain != nil {
				t.Fatalf("Open = %q, %v", plain, err)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("error %v, want one that wraps %v", err, tc.is)
			}
			for e := err; e != nil; e = errors.Unwrap(e) {
				if strings.Contains(e.Error(), markedValue) {
					t.Errorf("the error tells the plaintext: %v", e)
				}
			}
		})
	}
}

// TestKMSOpensACorruptedField: a ciphertext that was changed is an
// integrity failure.
func TestKMSOpensACorruptedField(t *testing.T) {
	t.Parallel()
	kms, err := porttest.NewKMS("kms-1")
	if err != nil {
		t.Fatal(err)
	}
	aad := delivery.PathAAD("hook")
	field := seal(t, kms, markedValue, aad)
	field.Ciphertext[0] ^= 1
	if _, err := (sealed.KMS{KMS: kms}).Open(t.Context(), field, aad); !errors.Is(err, domain.ErrIntegrity) {
		t.Errorf("Open of a changed field = %v, want %v", err, domain.ErrIntegrity)
	}
}
