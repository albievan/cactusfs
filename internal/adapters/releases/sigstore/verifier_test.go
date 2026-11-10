package sigstore_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/albievan/cactusfs/internal/adapters/releases/sigstore"
	"github.com/albievan/cactusfs/internal/adapters/releases/sigstore/sigstoretest"
	"github.com/albievan/cactusfs/internal/domain"
)

// sums is a file of a release, as SHA256SUMS looks.
var sums = []byte("0f343b0931126a20f133d67c2b018a3b5e1ce6b8b0ab4e1a1d5d5c4e4d3f2a1b  cactusfs-linux-amd64\n")

// fixture is a Sigstore of the tests and a second one that no root of the
// tests believes; made once, as a VirtualSigstore takes a while.
type fixture struct {
	signer *sigstoretest.Sigstore
	other  *sigstoretest.Sigstore
	root   []byte
}

var shared = sync.OnceValues(func() (*fixture, error) {
	signer, err := sigstoretest.New()
	if err != nil {
		return nil, err
	}
	other, err := sigstoretest.New()
	if err != nil {
		return nil, err
	}
	trusted, err := signer.TrustedRoot()
	if err != nil {
		return nil, err
	}
	return &fixture{signer: signer, other: other, root: trusted}, nil
})

func newFixture(t testing.TB) *fixture {
	t.Helper()
	f, err := shared()
	if err != nil {
		t.Fatalf("the Sigstore of the tests: %v", err)
	}
	return f
}

func newVerifier(t testing.TB, trustedRoot []byte, opts ...sigstore.Option) *sigstore.Verifier {
	t.Helper()
	v, err := sigstore.NewVerifier(trustedRoot, opts...)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func sign(t testing.TB, s *sigstoretest.Sigstore, identity, issuer string, artifact []byte) []byte {
	t.Helper()
	b, err := s.Sign(identity, issuer, artifact)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return b
}

// identity is the identity of the release workflow at the tag v<version>.
func identity(version string) string { return sigstore.DefaultIdentityPrefix + version }

// TestVerify: a bundle is accepted only for the release workflow of
// albievan/cactusfs at the tag of the version asked about, issued by
// GitHub, over exactly the bytes handed in, from a Sigstore the root knows.
func TestVerify(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	v := newVerifier(t, f.root)

	changed := bytes.Clone(sums)
	changed[0] ^= 0x01

	tests := []struct {
		name     string
		version  string
		signed   []byte
		bundle   func(t *testing.T) []byte
		accepted bool
		reason   string
	}{
		{
			name: "the release workflow at the tag", version: "0.7.1", signed: sums, accepted: true,
			bundle: func(t *testing.T) []byte { return sign(t, f.signer, identity("0.7.1"), sigstore.DefaultIssuer, sums) },
		},
		{
			name: "a prerelease at its tag", version: "0.8.0-rc.1", signed: sums, accepted: true,
			bundle: func(t *testing.T) []byte {
				return sign(t, f.signer, identity("0.8.0-rc.1"), sigstore.DefaultIssuer, sums)
			},
		},
		{
			name: "another tag", version: "0.7.1", signed: sums, reason: "SAN",
			bundle: func(t *testing.T) []byte { return sign(t, f.signer, identity("0.7.0"), sigstore.DefaultIssuer, sums) },
		},
		{
			name: "a tag the version is a prefix of", version: "0.7.1", signed: sums, reason: "SAN",
			bundle: func(t *testing.T) []byte { return sign(t, f.signer, identity("0.7.10"), sigstore.DefaultIssuer, sums) },
		},
		{
			name: "a branch", version: "0.7.1", signed: sums, reason: "SAN",
			bundle: func(t *testing.T) []byte {
				return sign(t, f.signer, "https://github.com/albievan/cactusfs/.github/workflows/release.yml@refs/heads/main", sigstore.DefaultIssuer, sums)
			},
		},
		{
			name: "another repository", version: "0.7.1", signed: sums, reason: "SAN",
			bundle: func(t *testing.T) []byte {
				return sign(t, f.signer, "https://github.com/someone/cactusfs/.github/workflows/release.yml@refs/tags/v0.7.1", sigstore.DefaultIssuer, sums)
			},
		},
		{
			name: "another workflow file", version: "0.7.1", signed: sums, reason: "SAN",
			bundle: func(t *testing.T) []byte {
				return sign(t, f.signer, "https://github.com/albievan/cactusfs/.github/workflows/ci.yml@refs/tags/v0.7.1", sigstore.DefaultIssuer, sums)
			},
		},
		{
			name: "another issuer", version: "0.7.1", signed: sums, reason: "issuer",
			bundle: func(t *testing.T) []byte {
				return sign(t, f.signer, identity("0.7.1"), "https://accounts.google.com", sums)
			},
		},
		{
			name: "a changed byte of the file", version: "0.7.1", signed: changed,
			bundle: func(t *testing.T) []byte { return sign(t, f.signer, identity("0.7.1"), sigstore.DefaultIssuer, sums) },
		},
		{
			name: "the bundle of another file", version: "0.7.1", signed: sums,
			bundle: func(t *testing.T) []byte {
				return sign(t, f.signer, identity("0.7.1"), sigstore.DefaultIssuer, []byte("cactusfs:0.7.1 sha256:0000\n"))
			},
		},
		{
			name: "a Sigstore the root does not know", version: "0.7.1", signed: sums,
			bundle: func(t *testing.T) []byte { return sign(t, f.other, identity("0.7.1"), sigstore.DefaultIssuer, sums) },
		},
		{
			name: "a version with a v", version: "v0.7.1", signed: sums, reason: "version",
			bundle: func(t *testing.T) []byte { return sign(t, f.signer, identity("v0.7.1"), sigstore.DefaultIssuer, sums) },
		},
		{
			name: "no version", version: "", signed: sums, reason: "version",
			bundle: func(t *testing.T) []byte { return sign(t, f.signer, identity(""), sigstore.DefaultIssuer, sums) },
		},
		{
			name: "a version that is a path", version: "0.7.1/../0.7.2", signed: sums, reason: "version",
			bundle: func(t *testing.T) []byte {
				return sign(t, f.signer, identity("0.7.1/../0.7.2"), sigstore.DefaultIssuer, sums)
			},
		},
		{
			name: "an attestation, not a signature", version: "0.7.1", signed: sums, reason: "no message signature",
			bundle: func(t *testing.T) []byte {
				return asAttestation(t, sign(t, f.signer, identity("0.7.1"), sigstore.DefaultIssuer, sums))
			},
		},
		{
			name: "a bundle without its transparency log entry", version: "0.7.1", signed: sums,
			bundle: func(t *testing.T) []byte {
				return without(t, sign(t, f.signer, identity("0.7.1"), sigstore.DefaultIssuer, sums), "tlogEntries")
			},
		},
		{name: "not JSON", version: "0.7.1", signed: sums, reason: "not a Sigstore bundle", bundle: constant([]byte("not json"))},
		{name: "an empty object", version: "0.7.1", signed: sums, reason: "not a Sigstore bundle", bundle: constant([]byte("{}"))},
		{name: "nothing", version: "0.7.1", signed: sums, reason: "not a Sigstore bundle", bundle: constant(nil)},
		{
			name: "a truncated bundle", version: "0.7.1", signed: sums, reason: "not a Sigstore bundle",
			bundle: func(t *testing.T) []byte {
				b := sign(t, f.signer, identity("0.7.1"), sigstore.DefaultIssuer, sums)
				return b[:len(b)/2]
			},
		},
		{
			name: "a bundle too large", version: "0.7.1", signed: sums, reason: "larger than",
			bundle: constant(bytes.Repeat([]byte(" "), sigstore.MaxBundle+1)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := v.Verify(t.Context(), tt.version, tt.signed, tt.bundle(t))
			if tt.accepted {
				if err != nil {
					t.Fatalf("Verify = %v, want it accepted", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Verify accepted it")
			}
			if !errors.Is(err, domain.ErrReleaseNotVerified) {
				t.Errorf("Verify = %v, want it to wrap domain.ErrReleaseNotVerified", err)
			}
			if !strings.Contains(err.Error(), tt.reason) {
				t.Errorf("Verify = %v, want it to say %q", err, tt.reason)
			}
			if strings.Contains(err.Error(), string(tt.signed)) {
				t.Errorf("Verify = %v: it repeats the file", err)
			}
		})
	}
}

// TestVerifyCancelled: a cancelled context is not verified.
func TestVerifyCancelled(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	v := newVerifier(t, f.root)
	b := sign(t, f.signer, identity("0.7.1"), sigstore.DefaultIssuer, sums)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := v.Verify(ctx, "0.7.1", sums, b); !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify = %v, want context.Canceled", err)
	}
}

// TestOptions: the issuer and the identity can be another's; then the
// release workflow of CactusFS is no longer believed.
func TestOptions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	const prefix = "https://git.example.test/mirror/cactusfs/.github/workflows/release.yml@refs/tags/v"
	const issuer = "https://issuer.example.test"
	v := newVerifier(t, f.root, sigstore.WithIssuer(issuer), sigstore.WithIdentityPrefix(prefix))
	if got := v.Identity("0.7.1"); got != prefix+"0.7.1" {
		t.Errorf("Identity = %q", got)
	}
	if err := v.Verify(t.Context(), "0.7.1", sums, sign(t, f.signer, prefix+"0.7.1", issuer, sums)); err != nil {
		t.Errorf("Verify of the identity of the options = %v", err)
	}
	if err := v.Verify(t.Context(), "0.7.1", sums, sign(t, f.signer, identity("0.7.1"), sigstore.DefaultIssuer, sums)); err == nil {
		t.Error("Verify accepted the default identity")
	}

	for name, opt := range map[string]sigstore.Option{
		"no issuer":   sigstore.WithIssuer(""),
		"no identity": sigstore.WithIdentityPrefix(""),
	} {
		if _, err := sigstore.NewVerifier(f.root, opt); err == nil {
			t.Errorf("NewVerifier with %s: no error", name)
		}
	}
}

// TestNewVerifierRefusesARoot: what is not a trusted root is refused.
func TestNewVerifierRefusesARoot(t *testing.T) {
	t.Parallel()
	for _, trusted := range [][]byte{nil, []byte("{}"), []byte("not json"), []byte(`{"mediaType":"application/vnd.dev.sigstore.trustedroot+json;version=0.9"}`)} {
		if _, err := sigstore.NewVerifier(trusted); err == nil {
			t.Errorf("NewVerifier(%q): no error", trusted)
		}
	}
}

// TestDefault: the embedded root is Sigstore's public-good instance, and it
// parses; Default believes the release workflow of CactusFS.
func TestDefault(t *testing.T) {
	t.Parallel()
	v, err := sigstore.Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	if got := v.Identity("0.7.1"); got != "https://github.com/albievan/cactusfs/.github/workflows/release.yml@refs/tags/v0.7.1" {
		t.Errorf("Identity = %q", got)
	}

	content := sigstore.PublicGoodTrustedRoot()
	trusted, err := root.NewTrustedRootFromJSON(content)
	if err != nil {
		t.Fatalf("the embedded trusted root: %v", err)
	}
	if len(trusted.FulcioCertificateAuthorities()) == 0 || len(trusted.RekorLogs()) == 0 ||
		len(trusted.CTLogs()) == 0 || len(trusted.TimestampingAuthorities()) == 0 {
		t.Error("the embedded trusted root lacks a CA, a transparency log, a CT log or a TSA")
	}
	for _, want := range []string{"https://fulcio.sigstore.dev", "https://rekor.sigstore.dev", "https://ctfe.sigstore.dev"} {
		if !bytes.Contains(content, []byte(want)) {
			t.Errorf("the embedded trusted root does not name %s", want)
		}
	}

	// A release of the public-good instance is not taken from the test one,
	// nor the other way round.
	f := newFixture(t)
	if err := v.Verify(t.Context(), "0.7.1", sums, sign(t, f.signer, identity("0.7.1"), sigstore.DefaultIssuer, sums)); err == nil {
		t.Error("the public-good root believed the Sigstore of the tests")
	}

	// The caller gets a copy.
	content[0] ^= 0xff
	if again := sigstore.PublicGoodTrustedRoot(); again[0] == content[0] {
		t.Error("PublicGoodTrustedRoot hands out the embedded bytes themselves")
	}
}

// FuzzVerifyBundle: whatever the bundle, Verify never panics, and it never
// accepts a file that was never signed, nor a version that was not.
func FuzzVerifyBundle(f *testing.F) {
	fx := newFixture(f)
	v := newVerifier(f, fx.root)
	valid := sign(f, fx.signer, identity("0.7.1"), sigstore.DefaultIssuer, sums)
	f.Add(valid)
	f.Add(asAttestation(f, valid))
	f.Add(without(f, valid, "tlogEntries"))
	f.Add(without(f, valid, "timestampVerificationData"))
	f.Add(valid[:len(valid)/2])
	f.Add([]byte("{}"))
	f.Add([]byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`))
	f.Add([]byte(""))
	unsigned := []byte("never signed\n")
	f.Fuzz(func(t *testing.T, bundleJSON []byte) {
		_ = v.Verify(t.Context(), "0.7.1", sums, bundleJSON)
		if err := v.Verify(t.Context(), "0.7.1", unsigned, bundleJSON); err == nil {
			t.Fatal("a file that was never signed was accepted")
		}
		if err := v.Verify(t.Context(), "0.7.2", sums, bundleJSON); err == nil {
			t.Fatal("a version that was never signed was accepted")
		}
	})
}

func constant(b []byte) func(*testing.T) []byte {
	return func(*testing.T) []byte { return b }
}

// edit decodes a bundle, changes it and encodes it again.
func edit(t testing.TB, bundleJSON []byte, change func(map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(bundleJSON, &m); err != nil {
		t.Fatalf("the bundle: %v", err)
	}
	change(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("the bundle: %v", err)
	}
	return out
}

// without returns the bundle without a field of its verification material.
func without(t testing.TB, bundleJSON []byte, field string) []byte {
	t.Helper()
	return edit(t, bundleJSON, func(m map[string]any) {
		material, ok := m["verificationMaterial"].(map[string]any)
		if !ok {
			t.Fatal("the bundle has no verification material")
		}
		delete(material, field)
	})
}

// asAttestation returns the bundle with a DSSE envelope where its message
// signature was.
func asAttestation(t testing.TB, bundleJSON []byte) []byte {
	t.Helper()
	return edit(t, bundleJSON, func(m map[string]any) {
		delete(m, "messageSignature")
		m["dsseEnvelope"] = map[string]any{
			"payload":     base64.StdEncoding.EncodeToString([]byte(`{"_type":"https://in-toto.io/Statement/v1"}`)),
			"payloadType": "application/vnd.in-toto+json",
			"signatures":  []any{map[string]any{"sig": base64.StdEncoding.EncodeToString([]byte("signature"))}},
		}
	})
}
