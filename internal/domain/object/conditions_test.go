package object_test

import (
	"errors"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
)

func TestConditions(t *testing.T) {
	t.Parallel()
	modified := time.Date(2026, 10, 7, 12, 0, 0, 500_000_000, time.UTC)
	second := modified.Truncate(time.Second)
	const tag = "9e107d9d372bb6826bd81d3542a419d6"
	tests := []struct {
		name string
		c    object.Conditions
		// read and copy are what a reader and the source of a copy get.
		read, copy error
	}{
		{"none", object.Conditions{}, nil, nil},
		{"If-Match, the tag", object.Conditions{IfMatch: []string{"x", tag}}, nil, nil},
		{"If-Match, any", object.Conditions{IfMatch: []string{"*"}}, nil, nil},
		{"If-Match, another", object.Conditions{IfMatch: []string{"x"}}, domain.ErrPreconditionFailed, domain.ErrPreconditionFailed},
		{"If-None-Match, the tag", object.Conditions{IfNoneMatch: []string{tag}}, domain.ErrNotModified, domain.ErrPreconditionFailed},
		{"If-None-Match, any", object.Conditions{IfNoneMatch: []string{"*"}}, domain.ErrNotModified, domain.ErrPreconditionFailed},
		{"If-None-Match, another", object.Conditions{IfNoneMatch: []string{"x"}}, nil, nil},
		// Dates are compared to the second: the half second of the
		// version is not something an HTTP date can say.
		{"If-Modified-Since, its own second", object.Conditions{IfModifiedSince: second}, domain.ErrNotModified, domain.ErrPreconditionFailed},
		{"If-Modified-Since, later", object.Conditions{IfModifiedSince: second.Add(time.Hour)}, domain.ErrNotModified, domain.ErrPreconditionFailed},
		{"If-Modified-Since, earlier", object.Conditions{IfModifiedSince: second.Add(-time.Second)}, nil, nil},
		{"If-Unmodified-Since, its own second", object.Conditions{IfUnmodifiedSince: second}, nil, nil},
		{"If-Unmodified-Since, earlier", object.Conditions{IfUnmodifiedSince: second.Add(-time.Second)}, domain.ErrPreconditionFailed, domain.ErrPreconditionFailed},
		// Where a tag is given, the date beside it is not asked.
		{"If-Match holds, If-Unmodified-Since would not", object.Conditions{IfMatch: []string{tag}, IfUnmodifiedSince: second.Add(-time.Hour)}, nil, nil},
		{"If-None-Match passes, If-Modified-Since would not", object.Conditions{IfNoneMatch: []string{"x"}, IfModifiedSince: second.Add(time.Hour)}, nil, nil},
		// A failed precondition comes before "not modified".
		{"If-Match fails, If-None-Match matches", object.Conditions{IfMatch: []string{"x"}, IfNoneMatch: []string{tag}}, domain.ErrPreconditionFailed, domain.ErrPreconditionFailed},
		{"If-Match holds, If-None-Match matches", object.Conditions{IfMatch: []string{tag}, IfNoneMatch: []string{tag}}, domain.ErrNotModified, domain.ErrPreconditionFailed},
	}
	for _, tt := range tests {
		if err := tt.c.Check(tag, modified, true); !errors.Is(err, tt.read) || (tt.read == nil && err != nil) {
			t.Errorf("%s, for a reader: %v, want %v", tt.name, err, tt.read)
		}
		if err := tt.c.Check(tag, modified, false); !errors.Is(err, tt.copy) || (tt.copy == nil && err != nil) {
			t.Errorf("%s, for the source of a copy: %v, want %v", tt.name, err, tt.copy)
		}
		if tt.c.IsZero() != (tt.name == "none") {
			t.Errorf("%s: IsZero", tt.name)
		}
	}
}
