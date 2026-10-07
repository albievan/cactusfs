package object

import "strings"

// MaxListKeys is the most entries one listing page may hold.
const MaxListKeys = 1000

// ListOptions selects a page of a bucket listing.
type ListOptions struct {
	// Prefix limits the listing to keys that begin with it.
	Prefix string
	// Delimiter groups keys: the part of a key up to and including the
	// first delimiter after the prefix is rolled up into a common prefix.
	Delimiter string
	// StartAfter lists only keys strictly greater than it. It is ignored
	// when Continuation is set.
	StartAfter string
	// Continuation continues a truncated listing. It is the NextContinuation of the
	// previous page and must be used with the same Prefix and Delimiter.
	Continuation string
	// MaxKeys bounds the number of objects plus common prefixes returned.
	MaxKeys int
}

// ListPage is one page of a bucket listing, in key order.
type ListPage struct {
	// Objects are the listed object versions.
	Objects []Version
	// CommonPrefixes are the rolled-up key groups.
	CommonPrefixes []string
	// Truncated reports that more entries follow this page.
	Truncated bool
	// NextContinuation continues the listing. It is set only when Truncated is
	// true and holds the last entry of this page: a key or a common prefix.
	NextContinuation string
}

// StartKey returns where a scan for these options begins: the first key to
// visit, and a key to skip if it is met (the entry the listing resumes after).
// The boolean is false when nothing can follow the resume point.
func (o ListOptions) StartKey() (seek, skip string, ok bool) {
	switch {
	case o.Continuation != "":
		// A token that is a common prefix stands for every key inside it,
		// all of which the previous page already accounted for.
		if group, isGroup := CommonPrefix(o.Continuation, o.Prefix, o.Delimiter); isGroup && group == o.Continuation {
			end, more := PrefixEnd(group)
			if !more {
				return "", "", false
			}
			seek = end
		} else {
			seek, skip = o.Continuation, o.Continuation
		}
	case o.StartAfter != "":
		seek, skip = o.StartAfter, o.StartAfter
	}
	if seek < o.Prefix {
		return o.Prefix, "", true
	}
	return seek, skip, true
}

// CommonPrefix returns the common prefix that key rolls up into for the given
// listing prefix and delimiter, and whether there is one. key must begin with
// prefix.
func CommonPrefix(key, prefix, delimiter string) (string, bool) {
	if delimiter == "" || !strings.HasPrefix(key, prefix) {
		return "", false
	}
	i := strings.Index(key[len(prefix):], delimiter)
	if i < 0 {
		return "", false
	}
	return key[:len(prefix)+i+len(delimiter)], true
}

// PrefixEnd returns the smallest string that is greater than every string
// beginning with prefix, and false when no such string exists (prefix is empty
// or consists only of 0xFF bytes). A listing uses it to jump past a common
// prefix without visiting the keys inside it.
func PrefixEnd(prefix string) (string, bool) {
	end := []byte(prefix)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xFF {
			end[i]++
			return string(end[:i+1]), true
		}
	}
	return "", false
}
