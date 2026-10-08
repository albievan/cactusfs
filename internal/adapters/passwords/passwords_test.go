package passwords_test

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/passwords"
	"github.com/albievan/cactusfs/internal/domain"
)

// light keeps the tests fast; the format and the logic do not depend on the
// cost.
func light() passwords.Params {
	return passwords.Params{Time: 1, MemoryKiB: 8 << 10, Threads: 1}
}

func TestHashAndVerify(t *testing.T) {
	t.Parallel()
	hasher, err := passwords.New(light(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const password = "correct horse battery staple"
	encoded, err := hasher.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=8192,t=1,p=1$") || strings.Contains(encoded, password) {
		t.Errorf("encoded hash = %q", encoded)
	}
	if ok, err := hasher.Verify(encoded, password); err != nil || !ok {
		t.Errorf("Verify with the right password = %v, %v", ok, err)
	}
	for _, wrong := range []string{"", "Correct horse battery staple", password + " ", password[:len(password)-1]} {
		if ok, err := hasher.Verify(encoded, wrong); err != nil || ok {
			t.Errorf("Verify(%q) = %v, %v; want false", wrong, ok, err)
		}
	}
	// Every hash has its own salt.
	again, _ := hasher.Hash(password)
	if again == encoded {
		t.Error("two hashes of one password are identical")
	}
	// A hash made with other parameters is verified with those: raising
	// the cost later does not invalidate the hashes that exist.
	stronger, err := passwords.New(passwords.Params{Time: 2, MemoryKiB: 16 << 10, Threads: 2}, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := stronger.Verify(encoded, password); err != nil || !ok {
		t.Errorf("Verify of a hash with older parameters = %v, %v", ok, err)
	}
	if d := passwords.DefaultParams(); d.Time != 3 || d.MemoryKiB != 64<<10 || d.Threads != 4 {
		t.Errorf("DefaultParams = %+v", d)
	}
}

// TestStoredHashesAreNotTrusted: a hash that cannot be read is damage, not a
// wrong password, and one whose parameters were weakened to make guessing
// cheap, or raised to exhaust the machine, is refused before any work.
func TestStoredHashesAreNotTrusted(t *testing.T) {
	t.Parallel()
	hasher, err := passwords.New(light(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	good, _ := hasher.Hash("a password of some length")
	parts := strings.Split(good, "$")
	with := func(i int, value string) string {
		changed := append([]string{}, parts...)
		changed[i] = value
		return strings.Join(changed, "$")
	}
	bad := map[string]string{
		"empty":               "",
		"not a hash":          "password",
		"another algorithm":   with(1, "argon2i"),
		"another version":     with(2, "v=16"),
		"weakened memory":     with(3, "m=8,t=1,p=1"),
		"exhausting memory":   with(3, "m=4294967295,t=1,p=1"),
		"exhausting time":     with(3, "m=8192,t=1000000,p=1"),
		"zero time":           with(3, "m=8192,t=0,p=1"),
		"too many threads":    with(3, "m=8192,t=1,p=255"),
		"unknown parameter":   with(3, "m=8192,t=1,p=1,x=1"),
		"malformed parameter": with(3, "m=8192,t=one,p=1"),
		"bad salt":            with(4, "!!!"),
		"short salt":          with(4, "AAAA"),
		"bad hash":            with(5, "!!!"),
		"short hash":          with(5, "AAAA"),
		"extra field":         good + "$extra",
	}
	for name, encoded := range bad {
		if ok, err := hasher.Verify(encoded, "a password of some length"); !errors.Is(err, domain.ErrIntegrity) || ok {
			t.Errorf("%s: Verify = %v, %v; want %v", name, ok, err, domain.ErrIntegrity)
		}
	}
	for name, params := range map[string]passwords.Params{
		"too little memory": {Time: 1, MemoryKiB: 1024, Threads: 1},
		"no passes":         {Time: 0, MemoryKiB: 8 << 10, Threads: 1},
		"no threads":        {Time: 1, MemoryKiB: 8 << 10, Threads: 0},
	} {
		if _, err := passwords.New(params, rand.Reader); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("New with %s = %v", name, err)
		}
	}
	failing, _ := passwords.New(light(), strings.NewReader(""))
	if _, err := failing.Hash("x"); err == nil {
		t.Error("Hash without entropy succeeded")
	}
}
