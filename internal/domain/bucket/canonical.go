package bucket

import (
	"math"
	"strconv"
	"unicode/utf8"
)

// The canonical encoding of the records and documents every member of a
// cluster decides by: the usage record, the notification rules and the
// replication configuration (ADR-0023, "Canonical documents").
//
// They are JSON, and any reader of JSON reads them. But which bytes they are
// is decided here, by hand, and by nothing that a release of Go may change:
// a reader takes the bytes its writer writes and no others, so two members
// built with different releases must write the same bytes for ever.
//
//   - No white space. The fields of an object in one fixed order, each
//     written or left out by a rule of its own.
//   - A string is its UTF-8 bytes between quotation marks. Only three
//     things are escaped: the quotation mark as \", the backslash as \\,
//     and each of U+0000 to U+001F as \u00XX with lower-case hexadecimal
//     digits - never one of the short forms \b, \f, \n, \r, \t. Everything
//     else is written as it is: "<", ">" and "&", U+007F, U+2028 and
//     U+2029, and the characters beyond the basic plane. Text that is not
//     valid UTF-8 cannot be written.
//   - A number is a whole number that is not below nothing, in decimal
//     digits: no sign, no leading zero, no point, no exponent. Nothing of
//     these records can be negative, so the minus sign is legal nowhere.
//   - true and false, in those words.

const lowerHex = "0123456789abcdef"

// appendCanonicalString appends s as a canonical string. It is the one
// place that decides how a string of a canonical document is escaped.
// Text that is not valid UTF-8 is not written, and ok is false.
func appendCanonicalString(dst []byte, s string) (out []byte, ok bool) {
	if !utf8.ValidString(s) {
		return dst, false
	}
	dst = append(dst, '"')
	for i := range len(s) {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			dst = append(dst, '\\', c)
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', lowerHex[c>>4], lowerHex[c&0x0f])
		default:
			dst = append(dst, c)
		}
	}
	return append(dst, '"'), true
}

// canonicalWriter writes a canonical document. bad says that something
// could not be written: text that is not valid UTF-8, or a number below
// nothing.
type canonicalWriter struct {
	buf []byte
	bad bool
}

// raw writes punctuation and field names: text that is the same in every
// document.
func (w *canonicalWriter) raw(text string) {
	w.buf = append(w.buf, text...)
}

func (w *canonicalWriter) str(s string) {
	out, ok := appendCanonicalString(w.buf, s)
	w.buf, w.bad = out, w.bad || !ok
}

// num writes a number. There is no sign: a number below nothing cannot be
// written.
func (w *canonicalWriter) num(n int64) {
	if n < 0 {
		w.bad = true
		return
	}
	w.buf = strconv.AppendInt(w.buf, n, 10)
}

func (w *canonicalWriter) unum(n uint64) {
	w.buf = strconv.AppendUint(w.buf, n, 10)
}

func (w *canonicalWriter) boolean(b bool) {
	if b {
		w.raw("true")
		return
	}
	w.raw("false")
}

// canonicalReader reads a canonical document from its first byte to its
// last. The first thing that is not as the writer writes it stops it, and
// every read after that returns nothing.
type canonicalReader struct {
	data []byte
	at   int
	bad  bool
}

// lit reads text that must be there.
func (r *canonicalReader) lit(text string) {
	if !r.has(text) {
		r.bad = true
	}
}

// has reads text when it is there, and says whether it was.
func (r *canonicalReader) has(text string) bool {
	if r.bad || len(r.data)-r.at < len(text) || string(r.data[r.at:r.at+len(text)]) != text {
		return false
	}
	r.at += len(text)
	return true
}

func lowerHexValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

// str reads a canonical string: exactly what appendCanonicalString writes.
func (r *canonicalReader) str() string {
	r.lit(`"`)
	var out []byte
	for !r.bad {
		if r.at >= len(r.data) {
			r.bad = true
			break
		}
		c := r.data[r.at]
		switch {
		case c == '"':
			r.at++
			if !utf8.Valid(out) {
				r.bad = true
				return ""
			}
			return string(out)
		case c < 0x20:
			r.bad = true
		case c != '\\':
			out = append(out, c)
			r.at++
		case r.has(`\"`):
			out = append(out, '"')
		case r.has(`\\`):
			out = append(out, '\\')
		case r.has(`\u00`) && len(r.data)-r.at >= 2:
			high, okHigh := lowerHexValue(r.data[r.at])
			low, okLow := lowerHexValue(r.data[r.at+1])
			// Only what has no other form is written so: U+0020 and
			// above are themselves.
			if !okHigh || !okLow || high > 1 {
				r.bad = true
				break
			}
			out = append(out, high<<4|low)
			r.at += 2
		default:
			r.bad = true
		}
	}
	return ""
}

// canonicalNumber reads a number from 0 to limit: decimal digits, no sign,
// and no zero before another digit.
func canonicalNumber[T int | int64 | uint64](r *canonicalReader, limit T) T {
	if r.bad {
		return 0
	}
	start := r.at
	var n T
	for r.at < len(r.data) && r.data[r.at] >= '0' && r.data[r.at] <= '9' {
		digit := T(r.data[r.at] - '0')
		if digit > limit || n > (limit-digit)/10 {
			r.bad = true
			return 0
		}
		n = n*10 + digit
		r.at++
	}
	if r.at == start || (r.data[start] == '0' && r.at-start > 1) {
		r.bad = true
		return 0
	}
	return n
}

// num reads a number that an int64 holds.
func (r *canonicalReader) num() int64 {
	return canonicalNumber(r, int64(math.MaxInt64))
}

// unum reads a number that a uint64 holds.
func (r *canonicalReader) unum() uint64 {
	return canonicalNumber(r, uint64(math.MaxUint64))
}

// small reads a number that an int32 holds.
func (r *canonicalReader) small() int {
	return canonicalNumber(r, math.MaxInt32)
}

func (r *canonicalReader) boolean() bool {
	switch {
	case r.has("true"):
		return true
	case r.has("false"):
		return false
	}
	r.bad = true
	return false
}

// list reads a list: "[", the elements one reads each, "," between them,
// "]". The list may be empty.
func (r *canonicalReader) list(one func()) {
	r.lit("[")
	if r.has("]") {
		return
	}
	for !r.bad {
		one()
		if r.has("]") {
			return
		}
		r.lit(",")
	}
}

// end reports whether the whole of the data was read and all of it was as
// the writer writes it.
func (r *canonicalReader) end() bool {
	return !r.bad && r.at == len(r.data)
}
