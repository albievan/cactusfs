// Package search is the language of the search field: what somebody types,
// and whether a name answers to it.
//
// A query is one of three things. Plain text is found anywhere in a name,
// whatever its case. Text with * or ? in it is a pattern for the whole name:
// * stands for any run of characters, the separator of a key included, and ?
// for exactly one. Text that begins with "rx:" is a regular expression in
// the syntax of RE2, taken as written - case and all - and found anywhere in
// a name unless it anchors itself.
//
// Every query becomes one regular expression of RE2, whose matching takes
// time linear in the name however the pattern is written: there is nothing
// in it that backtracks.
package search

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode/utf8"

	"github.com/albievan/cactusfs/internal/domain"
)

// Kind says how a query is read.
type Kind string

// The kinds of query.
const (
	Substring Kind = "substring"
	Wildcard  Kind = "wildcard"
	Regex     Kind = "regex"
)

// RegexPrefix begins a query that is a regular expression.
const RegexPrefix = "rx:"

// Bounds of a query.
const (
	// MaxLength is the longest query, in bytes.
	MaxLength = 256
	// maxProgram bounds the compiled form of a regular expression: a short
	// pattern can ask for a large program with counted repetitions.
	maxProgram = 4096
)

// Query is a parsed query. The zero value matches nothing.
type Query struct {
	kind Kind
	text string
	re   *regexp.Regexp
}

// Parse reads what was typed into the search field. The error wraps
// domain.ErrInvalidArgument and says, for a regular expression, what the
// matter with it is.
func Parse(typed string) (Query, error) {
	text := strings.TrimSpace(typed)
	switch {
	case text == "":
		return Query{}, fmt.Errorf("%w: nothing to search for", domain.ErrInvalidArgument)
	case len(text) > MaxLength:
		return Query{}, fmt.Errorf("%w: a search is at most %d bytes", domain.ErrInvalidArgument, MaxLength)
	case !utf8.ValidString(text):
		return Query{}, fmt.Errorf("%w: a search is text", domain.ErrInvalidArgument)
	}
	if pattern, ok := strings.CutPrefix(text, RegexPrefix); ok {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return Query{}, fmt.Errorf("%w: %s is followed by a regular expression", domain.ErrInvalidArgument, RegexPrefix)
		}
		re, err := compile(pattern)
		if err != nil {
			return Query{}, err
		}
		return Query{kind: Regex, text: pattern, re: re}, nil
	}
	if strings.ContainsAny(text, "*?") {
		var pattern strings.Builder
		pattern.WriteString("(?is)^")
		for _, r := range text {
			switch r {
			case '*':
				pattern.WriteString(".*")
			case '?':
				pattern.WriteString(".")
			default:
				pattern.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		pattern.WriteString("$")
		re, err := compile(pattern.String())
		if err != nil {
			return Query{}, err
		}
		return Query{kind: Wildcard, text: text, re: re}, nil
	}
	re, err := compile("(?is)" + regexp.QuoteMeta(text))
	if err != nil {
		return Query{}, err
	}
	return Query{kind: Substring, text: text, re: re}, nil
}

// compile compiles a pattern, within the bound on its program.
func compile(pattern string) (*regexp.Regexp, error) {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil, fmt.Errorf("%w: regular expression: %s", domain.ErrInvalidArgument, err.Error())
	}
	program, err := syntax.Compile(parsed.Simplify())
	if err != nil {
		return nil, fmt.Errorf("%w: regular expression: %s", domain.ErrInvalidArgument, err.Error())
	}
	if len(program.Inst) > maxProgram {
		return nil, fmt.Errorf("%w: regular expression: it asks for too much repetition", domain.ErrInvalidArgument)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: regular expression: %s", domain.ErrInvalidArgument, err.Error())
	}
	return re, nil
}

// Kind returns how the query is read.
func (q Query) Kind() Kind { return q.kind }

// Text returns the query without what marked its kind.
func (q Query) Text() string { return q.text }

// Match reports whether a name - of a bucket, or the key of an object -
// answers to the query.
func (q Query) Match(name string) bool {
	return q.re != nil && q.re.MatchString(name)
}
