package release_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/release"
)

func mustVersion(t testing.TB, s string) release.Version {
	t.Helper()
	v, err := release.ParseVersion(s)
	if err != nil {
		t.Fatalf("ParseVersion(%q): %v", s, err)
	}
	return v
}

func TestParseVersion(t *testing.T) {
	t.Parallel()
	good := []struct {
		in                  string
		major, minor, patch uint64
		pre, text, tagName  string
	}{
		{"0.7.0", 0, 7, 0, "", "0.7.0", "v0.7.0"},
		{"v0.7.1", 0, 7, 1, "", "0.7.1", "v0.7.1"},
		{"1.2.3-rc.1", 1, 2, 3, "rc.1", "1.2.3-rc.1", "v1.2.3-rc.1"},
		{"v10.20.30-alpha-1.0.x-y", 10, 20, 30, "alpha-1.0.x-y", "10.20.30-alpha-1.0.x-y", "v10.20.30-alpha-1.0.x-y"},
		{"0.0.0", 0, 0, 0, "", "0.0.0", "v0.0.0"},
		{"18446744073709551615.0.1", 18446744073709551615, 0, 1, "", "18446744073709551615.0.1", "v18446744073709551615.0.1"},
		{"1.0.0-0A.is.legal", 1, 0, 0, "0A.is.legal", "1.0.0-0A.is.legal", "v1.0.0-0A.is.legal"},
		{"1.0.0--", 1, 0, 0, "-", "1.0.0--", "v1.0.0--"},
	}
	for _, tt := range good {
		v, err := release.ParseVersion(tt.in)
		if err != nil {
			t.Errorf("ParseVersion(%q): %v", tt.in, err)
			continue
		}
		if v.Major() != tt.major || v.Minor() != tt.minor || v.Patch() != tt.patch || v.Prerelease() != tt.pre ||
			v.String() != tt.text || v.Tag() != tt.tagName || v.IsPrerelease() != (tt.pre != "") {
			t.Errorf("ParseVersion(%q) = %d.%d.%d-%q %s %s", tt.in, v.Major(), v.Minor(), v.Patch(), v.Prerelease(), v, v.Tag())
		}
	}
	bad := []string{
		"", "v", "1", "1.2", "1.2.3.4", "01.2.3", "1.02.3", "1.2.03", "1.2.3-", "1.2.3-01", "1.2.3-rc..1",
		"1.2.3-rc.", "1.2.3+build", "1.2.3-rc.1+build", "1.2.3+", "V1.2.3", "vv1.2.3", " 1.2.3", "1.2.3 ", "1.2.3\n",
		"-1.2.3", "1.-2.3", "1.2.x", "1.2.3-rc_1", "1.2.3-ré", "18446744073709551616.0.0", "1.2.3-a/b",
		"1.2.3-" + strings.Repeat("a", release.MaxVersionLength),
	}
	for _, in := range bad {
		v, err := release.ParseVersion(in)
		if !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("ParseVersion(%q) = %v, %v; want ErrInvalidArgument", in, v, err)
			continue
		}
		if in != "" && len(in) > 3 && strings.Contains(err.Error(), in) {
			t.Errorf("the refusal of %q repeats it: %v", in, err)
		}
	}
	var zero release.Version
	if !zero.IsZero() || zero.String() != "0.0.0" || mustVersion(t, "0.0.1").IsZero() {
		t.Errorf("zero version: %v %v", zero.IsZero(), zero)
	}
	if !mustVersion(t, "0.7.4").SameMinor(mustVersion(t, "v0.7.0-rc.1")) || mustVersion(t, "0.7.4").SameMinor(mustVersion(t, "1.7.4")) {
		t.Error("SameMinor")
	}
}

// TestCompareFollowsSemver: the order of precedence the specification
// gives as its example, and the cases it describes.
func TestCompareFollowsSemver(t *testing.T) {
	t.Parallel()
	ordered := []string{
		"0.9.9", "1.0.0-0", "1.0.0-1", "1.0.0-2", "1.0.0-10", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta",
		"1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "1.10.0", "2.0.0",
		"18446744073709551615.0.0",
	}
	for i, x := range ordered {
		for j, y := range ordered {
			a, b := mustVersion(t, x), mustVersion(t, y)
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := release.Compare(a, b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", a, b, got, want)
			}
		}
	}
	// A numeric identifier is compared by its value, whatever its length.
	big := mustVersion(t, "1.0.0-rc.99999999999999999999999")
	if big.Compare(mustVersion(t, "1.0.0-rc.100000000000000000000000")) != -1 || big.Compare(mustVersion(t, "1.0.0-rc.x")) != -1 {
		t.Error("large numeric identifiers")
	}
	// v is only a way of writing.
	if mustVersion(t, "v1.2.3") != mustVersion(t, "1.2.3") {
		t.Error("v1.2.3 is not 1.2.3")
	}
}

// versions is a set of versions in which every rule of precedence meets
// every other: numbers that tie and that do not, pre-releases of every
// shape.
func versions(t testing.TB) []release.Version {
	t.Helper()
	pres := []string{"", "-alpha", "-alpha.1", "-1", "-2", "-10", "-rc.1", "-rc.2", "-rc.11", "-rc", "-beta-x", "-rc.1.a"}
	var out []release.Version
	for _, core := range []string{"0.7.0", "0.7.1", "0.7.10", "0.8.0", "0.8.2", "0.9.0", "1.0.0", "1.1.0"} {
		for _, pre := range pres {
			out = append(out, mustVersion(t, core+pre))
		}
	}
	return out
}

// TestCompareIsATotalOrder: reflexive, antisymmetric, transitive, and in
// agreement with == and with the text a version is written as.
func TestCompareIsATotalOrder(t *testing.T) {
	t.Parallel()
	all := versions(t)
	for _, a := range all {
		back, err := release.ParseVersion(a.String())
		if err != nil || back != a {
			t.Errorf("%s does not read back: %v, %v", a, back, err)
		}
		for _, b := range all {
			ab, ba := a.Compare(b), b.Compare(a)
			if ab != -ba || (ab == 0) != (a == b) || (ab == 0) != (a.String() == b.String()) {
				t.Errorf("Compare(%s, %s) = %d, Compare(%s, %s) = %d", a, b, ab, b, a, ba)
			}
		}
	}
	// Transitivity, on a third of the set to keep it quick.
	some := all[:len(all)/3+1]
	for _, a := range some {
		for _, b := range some {
			for _, c := range some {
				if a.Compare(b) <= 0 && b.Compare(c) <= 0 && a.Compare(c) > 0 {
					t.Errorf("%s <= %s <= %s, and %s > %s", a, b, c, a, c)
				}
			}
		}
	}
	sorted := slices.Clone(all)
	slices.SortFunc(sorted, release.Compare)
	if !slices.IsSortedFunc(sorted, release.Compare) {
		t.Error("a sorted set is not sorted")
	}
}

// FuzzParseVersion: whatever is read is read back from its String as the
// same version, compares equal only to itself, and is never longer than the
// bound; nothing panics.
func FuzzParseVersion(f *testing.F) {
	for _, seed := range []string{"0.7.0", "v1.2.3-rc.1", "1.0.0-alpha.beta", "1.2.3+b", "01.2.3", "1.2.3-", "v", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := release.ParseVersion(s)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("ParseVersion(%q): %v is not ErrInvalidArgument", s, err)
			}
			return
		}
		if strings.Contains(s, "+") || len(s) > release.MaxVersionLength {
			t.Fatalf("ParseVersion(%q) took build metadata or too much", s)
		}
		back, err := release.ParseVersion(v.String())
		if err != nil || back != v || back.Compare(v) != 0 {
			t.Fatalf("%q read as %s, which reads back as %v, %v", s, v, back, err)
		}
		if strings.TrimPrefix(s, "v") != v.String() {
			t.Fatalf("%q read as %s", s, v)
		}
	})
}
