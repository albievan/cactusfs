package iam_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

func TestDecide(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		principal iam.Principal
		allowed   bool
	}{
		{"root is allowed", iam.Principal{AccessKeyID: "ROOT", Root: true}, true},
		{"anonymous is denied", iam.Principal{}, false},
		{"anonymous claiming root is denied", iam.Principal{Root: true}, false},
		{"any other key is denied until policies exist", iam.Principal{AccessKeyID: "SOMEONE"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, action := range []iam.Action{iam.ActionGetObject, iam.ActionPutObject, iam.ActionDeleteBucket, iam.ActionListAllMyBuckets} {
				err := iam.Decide(iam.Request{Principal: tt.principal, Action: action, Resource: iam.Resource{Bucket: "b", Key: "k"}})
				if tt.allowed && err != nil {
					t.Errorf("%s: Decide = %v, want nil", action, err)
				}
				if !tt.allowed && !errors.Is(err, domain.ErrAccessDenied) {
					t.Errorf("%s: Decide = %v, want %v", action, err, domain.ErrAccessDenied)
				}
			}
		})
	}
}

func TestResourceString(t *testing.T) {
	t.Parallel()

	tests := map[iam.Resource]string{
		{}:                                 "arn:aws:s3:::*",
		{Bucket: "photos"}:                 "arn:aws:s3:::photos",
		{Bucket: "photos", Key: "a/b.jpg"}: "arn:aws:s3:::photos/a/b.jpg",
	}
	for resource, want := range tests {
		if got := resource.String(); got != want {
			t.Errorf("%+v.String() = %q, want %q", resource, got, want)
		}
	}
}

func TestValidateAccessKeyID(t *testing.T) {
	t.Parallel()

	valid := []string{"abc", "AKIAIOSFODNN7EXAMPLE", "my-key_01", strings.Repeat("a", iam.MaxAccessKeyIDLength)}
	invalid := []string{"", "ab", strings.Repeat("a", iam.MaxAccessKeyIDLength+1), "has space", "slash/key", "comma,key", "eq=key", "ключ"}
	for _, id := range valid {
		if err := iam.ValidateAccessKeyID(id); err != nil {
			t.Errorf("ValidateAccessKeyID(%q) = %v, want nil", id, err)
		}
	}
	for _, id := range invalid {
		if err := iam.ValidateAccessKeyID(id); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ValidateAccessKeyID(%q) = %v, want %v", id, err, domain.ErrInvalidArgument)
		}
	}
}

func TestValidateSecret(t *testing.T) {
	t.Parallel()

	const good = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	if err := iam.ValidateSecret(good); err != nil {
		t.Errorf("ValidateSecret(valid) = %v", err)
	}
	for name, secret := range map[string]string{
		"short":   "fifteen-chars-x",
		"long":    strings.Repeat("s", iam.MaxSecretLength+1),
		"space":   "sixteen chars ok!",
		"control": "sixteen-chars-ok\n",
		"unicode": "sixteen-chars-ok-é",
	} {
		err := iam.ValidateSecret(secret)
		if !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: ValidateSecret = %v, want %v", name, err, domain.ErrInvalidArgument)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the error message contains the secret", name)
		}
	}
}

func TestGeneratedCredentialsAreValidAndRandom(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for range 50 {
		id, err := iam.GenerateAccessKeyID(rand.Reader)
		if err != nil {
			t.Fatalf("GenerateAccessKeyID: %v", err)
		}
		secret, err := iam.GenerateSecret(rand.Reader)
		if err != nil {
			t.Fatalf("GenerateSecret: %v", err)
		}
		if err := iam.ValidateAccessKeyID(id); err != nil {
			t.Errorf("generated access key ID %q is invalid: %v", id, err)
		}
		if err := iam.ValidateSecret(secret); err != nil {
			t.Errorf("generated secret is invalid: %v", err)
		}
		if len(id) != 20 || len(secret) != 40 {
			t.Errorf("lengths = %d, %d; want 20, 40", len(id), len(secret))
		}
		if seen[id] || seen[secret] {
			t.Fatal("generated the same credential twice")
		}
		seen[id], seen[secret] = true, true
	}

	boom := errors.New("no entropy")
	if _, err := iam.GenerateAccessKeyID(iotest.ErrReader(boom)); !errors.Is(err, boom) {
		t.Errorf("GenerateAccessKeyID error = %v, want %v", err, boom)
	}
	if _, err := iam.GenerateSecret(iotest.ErrReader(boom)); !errors.Is(err, boom) {
		t.Errorf("GenerateSecret error = %v, want %v", err, boom)
	}
}

func TestSecretsEqual(t *testing.T) {
	t.Parallel()

	if !iam.SecretsEqual([]byte("same"), []byte("same")) {
		t.Error("equal secrets compare as different")
	}
	for _, other := range []string{"", "sam", "samE", "same "} {
		if iam.SecretsEqual([]byte("same"), []byte(other)) {
			t.Errorf("%q compares equal to %q", "same", other)
		}
	}
}

func TestSealAndOpen(t *testing.T) {
	t.Parallel()

	key := bytes.Repeat([]byte{7}, 32)
	secret := []byte("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY")
	aad := []byte("AKIAIOSFODNN7EXAMPLE")

	sealed, err := iam.Seal(key, rand.Reader, secret, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed.Ciphertext, secret) {
		t.Fatal("ciphertext contains the secret")
	}
	again, err := iam.Seal(key, rand.Reader, secret, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Equal(sealed.Nonce, again.Nonce) || bytes.Equal(sealed.Ciphertext, again.Ciphertext) {
		t.Error("two seals of the same secret are identical: the nonce is not random")
	}

	got, err := sealed.Open(key, aad)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("Open = %q, %v", got, err)
	}

	tampered := sealed
	tampered.Ciphertext = bytes.Clone(sealed.Ciphertext)
	tampered.Ciphertext[0] ^= 1
	shortNonce := sealed
	shortNonce.Nonce = sealed.Nonce[:4]

	failures := map[string]func() ([]byte, error){
		"wrong key":         func() ([]byte, error) { return sealed.Open(bytes.Repeat([]byte{8}, 32), aad) },
		"wrong owner":       func() ([]byte, error) { return sealed.Open(key, []byte("SOMEONE-ELSE")) },
		"tampered":          func() ([]byte, error) { return tampered.Open(key, aad) },
		"malformed nonce":   func() ([]byte, error) { return shortNonce.Open(key, aad) },
		"empty sealed data": func() ([]byte, error) { return iam.SealedSecret{}.Open(key, aad) },
	}
	for name, open := range failures {
		if got, err := open(); !errors.Is(err, domain.ErrIntegrity) || got != nil {
			t.Errorf("%s: Open = %q, %v; want nil, %v", name, got, err, domain.ErrIntegrity)
		}
	}

	if _, err := iam.Seal([]byte("short"), rand.Reader, secret, aad); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Seal with a short key: error = %v", err)
	}
	if _, err := sealed.Open([]byte("short"), aad); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Open with a short key: error = %v", err)
	}
	boom := errors.New("no entropy")
	if _, err := iam.Seal(key, iotest.ErrReader(boom), secret, aad); !errors.Is(err, boom) {
		t.Errorf("Seal without entropy: error = %v", err)
	}
}
