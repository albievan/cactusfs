package local_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/iotest"

	"github.com/albievan/cactusfs/internal/adapters/kms/local"
	"github.com/albievan/cactusfs/internal/domain"
)

// testParams is the cheapest parameter set the adapter accepts.
func testParams() local.Params { return local.Params{Time: 1, MemoryKiB: 8 << 10, Threads: 1} }

func testPassphrase() []byte { return []byte("correct horse battery staple") }

func createKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys", "master.json")
	if err := local.Create(path, testPassphrase(), testParams(), rand.Reader); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return path
}

func TestCreateAndOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	path := createKey(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 && os.PathSeparator == '/' {
		t.Errorf("key file mode = %o, want no access for group or others", perm)
	}

	kms, err := local.Open(path, testPassphrase())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if id := kms.KeyID(); len(id) != len("local:")+16 || id[:6] != "local:" {
		t.Errorf("KeyID() = %q", id)
	}

	key, err := kms.GenerateDataKey(ctx, []byte("data-id"))
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	if len(key.Plaintext) != 32 {
		t.Fatalf("data key is %d bytes, want 32", len(key.Plaintext))
	}
	if bytes.Contains(key.Wrapped, key.Plaintext) {
		t.Fatal("wrapped key contains the plaintext key")
	}
	other, err := kms.GenerateDataKey(ctx, []byte("data-id"))
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	if bytes.Equal(key.Plaintext, other.Plaintext) || bytes.Equal(key.Wrapped, other.Wrapped) {
		t.Fatal("two generated data keys are identical")
	}

	// A second process with the same file and passphrase unwraps the key.
	again, err := local.Open(path, testPassphrase())
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if again.KeyID() != kms.KeyID() {
		t.Errorf("KeyID differs between opens: %q, %q", kms.KeyID(), again.KeyID())
	}
	got, err := again.UnwrapDataKey(ctx, key.Wrapped, []byte("data-id"))
	if err != nil || !bytes.Equal(got, key.Plaintext) {
		t.Errorf("UnwrapDataKey after reopen: %v", err)
	}
}

func TestKeyFileNeverHoldsTheMasterKeyInTheClear(t *testing.T) {
	t.Parallel()

	// With a known entropy stream the master key is the first 32 bytes.
	entropy := bytes.Repeat([]byte{0xA5}, 256)
	path := filepath.Join(t.TempDir(), "master.json")
	if err := local.Create(path, testPassphrase(), testParams(), bytes.NewReader(entropy)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	data := readFile(t, path)
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("key file is not JSON: %v", err)
	}
	// 0xA5 × 32 in base64 is "paWlpaWl…"; the sealed form must not contain it.
	if bytes.Contains(data, []byte("paWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaWlpaU")) || bytes.Contains(data, entropy[:32]) {
		t.Error("key file contains the master key unencrypted")
	}
	if bytes.Contains(data, testPassphrase()) {
		t.Error("key file contains the passphrase")
	}
}

func TestOpenFailures(t *testing.T) {
	t.Parallel()

	edit := func(t *testing.T, change func(file map[string]any)) string {
		t.Helper()
		path := createKey(t)
		data := readFile(t, path)
		var file map[string]any
		if err := json.Unmarshal(data, &file); err != nil {
			t.Fatal(err)
		}
		change(file)
		out, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, out)
		return path
	}
	kdf := func(t *testing.T, file map[string]any) map[string]any {
		t.Helper()
		section, ok := file["kdf"].(map[string]any)
		if !ok {
			t.Fatal("key file has no kdf section")
		}
		return section
	}

	tests := []struct {
		name       string
		path       func(t *testing.T) string
		passphrase []byte
		want       error
	}{
		{"wrong passphrase", createKey, []byte("incorrect horse battery staple"), domain.ErrKeyUnavailable},
		{"empty passphrase", createKey, nil, domain.ErrKeyUnavailable},
		{"modified ciphertext", func(t *testing.T) string {
			return edit(t, func(f map[string]any) {
				f["ciphertext"] = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			})
		}, testPassphrase(), domain.ErrKeyUnavailable},
		{"modified key ID", func(t *testing.T) string {
			return edit(t, func(f map[string]any) { f["key_id"] = "local:0000000000000000" })
		}, testPassphrase(), domain.ErrKeyUnavailable},
		{"modified salt", func(t *testing.T) string {
			return edit(t, func(f map[string]any) { kdf(t, f)["salt"] = "AAAAAAAAAAAAAAAAAAAAAA==" })
		}, testPassphrase(), domain.ErrKeyUnavailable},
		{"weakened parameters", func(t *testing.T) string {
			return edit(t, func(f map[string]any) { kdf(t, f)["memory_kib"] = 8 })
		}, testPassphrase(), domain.ErrKeyUnavailable},
		{"exhausting parameters", func(t *testing.T) string {
			return edit(t, func(f map[string]any) { kdf(t, f)["memory_kib"] = 4294967295 })
		}, testPassphrase(), domain.ErrKeyUnavailable},
		{"changed parameters", func(t *testing.T) string {
			return edit(t, func(f map[string]any) { kdf(t, f)["time"] = 2 })
		}, testPassphrase(), domain.ErrKeyUnavailable},
		{"malformed nonce", func(t *testing.T) string {
			return edit(t, func(f map[string]any) { f["nonce"] = "AAAA" })
		}, testPassphrase(), domain.ErrKeyUnavailable},
		{"newer format", func(t *testing.T) string {
			return edit(t, func(f map[string]any) { f["format_version"] = 2 })
		}, testPassphrase(), domain.ErrUnsupportedFormat},
		{"another KDF", func(t *testing.T) string {
			return edit(t, func(f map[string]any) { kdf(t, f)["algorithm"] = "pbkdf2" })
		}, testPassphrase(), domain.ErrUnsupportedFormat},
		{"not a key file", func(t *testing.T) string {
			return edit(t, func(f map[string]any) { f["magic"] = "something-else" })
		}, testPassphrase(), domain.ErrKeyUnavailable},
		{"missing file", func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.json") }, testPassphrase(), os.ErrNotExist},
		{"missing directory", func(t *testing.T) string { return filepath.Join(t.TempDir(), "no", "such", "key.json") }, testPassphrase(), os.ErrNotExist},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kms, err := local.Open(tt.path(t), tt.passphrase)
			if !errors.Is(err, tt.want) || kms != nil {
				t.Errorf("Open = %v, %v; want nil, %v", kms, err, tt.want)
			}
		})
	}
}

func TestCreateFailures(t *testing.T) {
	t.Parallel()

	t.Run("does not overwrite", func(t *testing.T) {
		t.Parallel()
		path := createKey(t)
		before := readFile(t, path)
		if err := local.Create(path, testPassphrase(), testParams(), rand.Reader); !errors.Is(err, os.ErrExist) {
			t.Errorf("Create over an existing file: error = %v, want %v", err, os.ErrExist)
		}
		if after := readFile(t, path); !bytes.Equal(before, after) {
			t.Error("the existing key file was changed")
		}
	})
	t.Run("short passphrase", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "k.json")
		if err := local.Create(path, []byte("too-short"), testParams(), rand.Reader); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("error = %v, want %v", err, domain.ErrInvalidArgument)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Error("a key file was written despite the error")
		}
	})
	t.Run("weak parameters", func(t *testing.T) {
		t.Parallel()
		weak := local.Params{Time: 1, MemoryKiB: 64, Threads: 1}
		if err := local.Create(filepath.Join(t.TempDir(), "k.json"), testPassphrase(), weak, rand.Reader); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("error = %v, want %v", err, domain.ErrInvalidArgument)
		}
	})
	t.Run("no entropy", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("no entropy")
		if err := local.Create(filepath.Join(t.TempDir(), "k.json"), testPassphrase(), testParams(), iotest.ErrReader(boom)); !errors.Is(err, boom) {
			t.Errorf("error = %v, want %v", err, boom)
		}
	})
}

func TestUnwrapFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	kms, err := local.Open(createKey(t), testPassphrase())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	otherKMS, err := local.Open(createKey(t), testPassphrase())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	key, err := kms.GenerateDataKey(ctx, []byte("owner"))
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	flipped := bytes.Clone(key.Wrapped)
	flipped[len(flipped)-1] ^= 1
	badVersion := bytes.Clone(key.Wrapped)
	badVersion[0] = 9

	tests := map[string]func() ([]byte, error){
		"another owner":      func() ([]byte, error) { return kms.UnwrapDataKey(ctx, key.Wrapped, []byte("intruder")) },
		"another master key": func() ([]byte, error) { return otherKMS.UnwrapDataKey(ctx, key.Wrapped, []byte("owner")) },
		"tampered":           func() ([]byte, error) { return kms.UnwrapDataKey(ctx, flipped, []byte("owner")) },
		"unknown version":    func() ([]byte, error) { return kms.UnwrapDataKey(ctx, badVersion, []byte("owner")) },
		"truncated":          func() ([]byte, error) { return kms.UnwrapDataKey(ctx, key.Wrapped[:5], []byte("owner")) },
		"empty":              func() ([]byte, error) { return kms.UnwrapDataKey(ctx, nil, []byte("owner")) },
	}
	for name, unwrap := range tests {
		if got, err := unwrap(); !errors.Is(err, domain.ErrKeyUnavailable) || got != nil {
			t.Errorf("%s: UnwrapDataKey = %v, %v; want nil, %v", name, got, err, domain.ErrKeyUnavailable)
		}
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := kms.GenerateDataKey(cancelled, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("GenerateDataKey with a cancelled context: %v", err)
	}
	if _, err := kms.UnwrapDataKey(cancelled, key.Wrapped, []byte("owner")); !errors.Is(err, context.Canceled) {
		t.Errorf("UnwrapDataKey with a cancelled context: %v", err)
	}
}

func TestExists(t *testing.T) {
	t.Parallel()

	path := createKey(t)
	if ok, err := local.Exists(path); err != nil || !ok {
		t.Errorf("Exists(existing) = %v, %v", ok, err)
	}
	if ok, err := local.Exists(filepath.Join(filepath.Dir(path), "absent.json")); err != nil || ok {
		t.Errorf("Exists(absent) = %v, %v", ok, err)
	}
	if ok, err := local.Exists(filepath.Join(t.TempDir(), "no", "dir", "k.json")); err != nil || ok {
		t.Errorf("Exists(absent directory) = %v, %v", ok, err)
	}
}

func TestDefaultParamsAreAccepted(t *testing.T) {
	t.Parallel()

	p := local.DefaultParams()
	if p.Time < 3 || p.MemoryKiB < 64<<10 || p.Threads < 1 {
		t.Errorf("DefaultParams() = %+v, weaker than RFC 9106's second recommendation", p)
	}
}

// readFile and writeFile reach test files through an os.Root, like the code
// under test does.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Dir(path), err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Dir(path), err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(filepath.Base(path), data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
