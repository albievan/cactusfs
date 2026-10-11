package search_test

import (
	"errors"
	"strings"
	"testing"
	"testing/quick"
	"time"
	"unicode/utf8"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/search"
)

func TestParseAndMatch(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		query   string
		kind    search.Kind
		matches []string
		misses  []string
	}{
		"plain text, anywhere, any case": {
			query: "Report", kind: search.Substring,
			matches: []string{"report", "reports/2026/q1.txt", "Annual-REPORT.pdf", "x/y/quarterly_report"},
			misses:  []string{"repor", "rep/ort", ""},
		},
		"plain text is not a pattern": {
			query: "a.b+c(1)", kind: search.Substring,
			matches: []string{"A.B+C(1).txt"},
			misses:  []string{"aXb+c(1)", "a.bbc(1)"},
		},
		"spaces around are not searched for": {
			query: "  photos  ", kind: search.Substring,
			matches: []string{"my photos"},
		},
		"a star is any run, also across separators": {
			query: "reports/*.txt", kind: search.Wildcard,
			matches: []string{"reports/q1.txt", "Reports/2026/Q1.TXT", "reports/.txt"},
			misses:  []string{"old/reports/q1.txt", "reports/q1.txt.bak", "reports/q1.pdf"},
		},
		"a question mark is one character": {
			query: "log-?.gz", kind: search.Wildcard,
			matches: []string{"log-1.gz", "LOG-a.GZ", "log-é.gz"},
			misses:  []string{"log-10.gz", "log-.gz", "x/log-1.gz"},
		},
		"a wildcard is the whole name": {
			query: "*cactus*", kind: search.Wildcard,
			matches: []string{"cactus", "the/Cactus/file", "xcactusx"},
			misses:  []string{"cactu"},
		},
		"other signs in a wildcard stand for themselves": {
			query: "a.b[1]*", kind: search.Wildcard,
			matches: []string{"a.b[1]", "A.B[1]/x"},
			misses:  []string{"aXb[1]", "a.b1"},
		},
		"a regular expression, as written": {
			query: `rx:^reports/\d{4}/q[1-4]\.txt$`, kind: search.Regex,
			matches: []string{"reports/2026/q1.txt"},
			misses:  []string{"Reports/2026/q1.txt", "reports/2026/q5.txt", "x/reports/2026/q1.txt"},
		},
		"a regular expression is found anywhere unless it anchors": {
			query: "rx: q[12]", kind: search.Regex,
			matches: []string{"reports/q1.txt", "q2"},
			misses:  []string{"Q1", "q3"},
		},
		"a regular expression folds case when it says so": {
			query: "rx:(?i)readme", kind: search.Regex,
			matches: []string{"README.md", "docs/ReadMe"},
		},
		"only the prefix in lower case marks a regular expression": {
			query: "RX:abc", kind: search.Substring,
			matches: []string{"x/rx:abc"},
			misses:  []string{"abc"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, err := search.Parse(tt.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.query, err)
			}
			if q.Kind() != tt.kind {
				t.Errorf("Parse(%q) is a %s, want a %s", tt.query, q.Kind(), tt.kind)
			}
			for _, name := range tt.matches {
				if !q.Match(name) {
					t.Errorf("%q does not match %q", tt.query, name)
				}
			}
			for _, name := range tt.misses {
				if q.Match(name) {
					t.Errorf("%q matches %q", tt.query, name)
				}
			}
		})
	}
}

func TestParseRefuses(t *testing.T) {
	t.Parallel()
	for name, query := range map[string]string{
		"nothing":                       "",
		"only spaces":                   "   ",
		"too long":                      strings.Repeat("a", search.MaxLength+1),
		"not text":                      "\xff\xfe",
		"a prefix with nothing after":   "rx:",
		"a prefix with spaces after":    "rx:   ",
		"an expression that is not one": "rx:(unclosed",
		"a bad repetition":              "rx:a{5,2}",
		"too much repetition":           "rx:((a{100}){100}){100}",
		"a large program":               "rx:" + strings.Repeat("(a{1,500})", 10),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, err := search.Parse(query)
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("Parse(%q) = %v, want %v", query, err, domain.ErrInvalidArgument)
			}
			if q.Match("anything") || q.Match("") {
				t.Error("a query that was refused matches")
			}
		})
	}
	// The zero value matches nothing either.
	if (search.Query{}).Match("") {
		t.Error("the zero query matches")
	}
}

// TestMatchingIsLinear: a pattern that would take a backtracking matcher
// until the end of time takes this one no time to speak of.
func TestMatchingIsLinear(t *testing.T) {
	t.Parallel()
	q, err := search.Parse("rx:^(a+)+$")
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if q.Match(strings.Repeat("a", 1024) + "!") {
		t.Error("matched")
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("matching took %s", took)
	}
}

// glob is the wildcard written out by hand: * any run, ? one character,
// everything else itself, case folded, over the whole name.
func glob(pattern, name []rune) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			for i := 0; i <= len(name); i++ {
				if glob(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(name) == 0 {
				return false
			}
		default:
			if len(name) == 0 || !strings.EqualFold(string(pattern[0]), string(name[0])) {
				return false
			}
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}

// containsFold is plain text written out by hand: found anywhere, case folded.
func containsFold(name, text []rune) bool {
	for i := 0; i+len(text) <= len(name); i++ {
		if strings.EqualFold(string(name[i:i+len(text)]), string(text)) {
			return true
		}
	}
	return false
}

// letters turns arbitrary bytes into short text over a small alphabet, so
// that matches are common enough to be tested.
func letters(raw []byte, alphabet string, limit int) string {
	var out strings.Builder
	for i, b := range raw {
		if i == limit {
			break
		}
		out.WriteByte(alphabet[int(b)%len(alphabet)])
	}
	return out.String()
}

func TestWildcardAgreesWithAGlobWrittenByHand(t *testing.T) {
	t.Parallel()
	property := func(rawPattern, rawName []byte) bool {
		pattern := "*" + letters(rawPattern, "aAbB/.*?", 8)
		name := letters(rawName, "aAbB/.x", 12)
		q, err := search.Parse(pattern)
		if err != nil || q.Kind() != search.Wildcard {
			return false
		}
		return q.Match(name) == glob([]rune(pattern), []rune(name))
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

func TestPlainTextAgreesWithAFoldedContains(t *testing.T) {
	t.Parallel()
	property := func(rawText, rawName []byte) bool {
		text := "a" + letters(rawText, "aAbB/.+(", 4)
		name := letters(rawName, "aAbB/.+(x", 14)
		q, err := search.Parse(text)
		if err != nil || q.Kind() != search.Substring {
			return false
		}
		return q.Match(name) == containsFold([]rune(name), []rune(text))
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{"report", "*.txt", "rx:^a+$", "rx:(", "a?b", "rx:(?i)x{1,1000}", "\xff", "rx:\\p{Greek}+"} {
		f.Add(seed, "reports/2026/q1.txt")
	}
	f.Fuzz(func(t *testing.T, typed, name string) {
		q, err := search.Parse(typed)
		if err != nil {
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("Parse(%q): %v does not wrap ErrInvalidArgument", typed, err)
			}
			if q.Match(name) {
				t.Fatalf("a refused query matches")
			}
			return
		}
		if len(strings.TrimSpace(typed)) > search.MaxLength || !utf8.ValidString(strings.TrimSpace(typed)) {
			t.Fatalf("Parse accepted %q", typed)
		}
		// Whatever it is, matching ends and does not panic.
		_ = q.Match(name)
		if q.Kind() == search.Substring && !q.Match("x"+q.Text()+"y") {
			t.Fatalf("plain text %q is not found in a name that contains it", q.Text())
		}
	})
}
