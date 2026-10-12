package bucket

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCanonicalString pins the one rule of how a string of a canonical
// document is written (ADR-0023): every class of character, by hand.
func TestCanonicalString(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, text, want string
	}{
		{"nothing", "", `""`},
		{"letters", "logs/2026", `"logs/2026"`},
		{"a quotation mark", `a"b`, `"a\"b"`},
		{"a backslash", `a\b`, `"a\\b"`},
		{"a slash is itself", "a/b", `"a/b"`},
		{"the first character there is", "\x00", `"\u0000"`},
		{"U+0001", "\x01", `"\u0001"`},
		{"a backspace has no short form", "\b", `"\u0008"`},
		{"a form feed has no short form", "\f", `"\u000c"`},
		{"a line feed has no short form", "\n", `"\u000a"`},
		{"a carriage return has no short form", "\r", `"\u000d"`},
		{"a tab has no short form", "\t", `"\u0009"`},
		{"the last control character that is escaped", "\x1f", `"\u001f"`},
		{"a space is itself", " ", `" "`},
		{"U+007F is itself", "\x7f", "\"\x7f\""},
		{"angle brackets and an ampersand are themselves", "<>&", `"<>&"`},
		{"the line separators are themselves", "\u2028\u2029", "\"\u2028\u2029\""},
		{"two bytes", "\u00e9", "\"\u00e9\""},
		{"beyond the basic plane", "\U0001d11e", "\"\U0001d11e\""},
		{"the replacement character, which is valid", "�", "\"�\""},
	} {
		got, ok := appendCanonicalString(nil, tt.text)
		if !ok || string(got) != tt.want {
			t.Errorf("%s: %q is written as %s (%v), want %s", tt.name, tt.text, got, ok, tt.want)
			continue
		}
		r := &canonicalReader{data: got}
		if back := r.str(); !r.end() || back != tt.text {
			t.Errorf("%s: %s is read as %q (to the end: %v)", tt.name, got, back, r.end())
		}
		// It is JSON: any reader of JSON reads the same text.
		var read string
		if err := json.Unmarshal(got, &read); err != nil || read != tt.text {
			t.Errorf("%s: a reader of JSON reads %s as %q, %v", tt.name, got, read, err)
		}
	}
	for name, text := range map[string]string{
		"a byte that is no UTF-8":      "a\xffb",
		"a sequence cut short":         "a\xc3",
		"half of a surrogate pair":     "\xed\xa0\x80",
		"a character written too long": "\xc0\xaf",
	} {
		if got, ok := appendCanonicalString([]byte("kept"), text); ok || string(got) != "kept" {
			t.Errorf("%s: written as %q", name, got)
		}
	}
}

// TestCanonicalStringRefused: a string that is JSON, and is not as the
// writer writes it, is not read.
func TestCanonicalStringRefused(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"nothing":                           ``,
		"no end":                            `"abc`,
		"no beginning":                      `abc"`,
		"a short form: line feed":           `"\n"`,
		"a short form: tab":                 `"\t"`,
		"a short form: backspace":           `"\b"`,
		"a short form: form feed":           `"\f"`,
		"a short form: carriage return":     `"\r"`,
		"a slash escaped":                   `"\/"`,
		"hexadecimal in capitals":           `"\u000A"`,
		"the escape in capitals":            `"\U000a"`,
		"an angle bracket escaped":          `"\u003c"`,
		"an ampersand escaped":              `"\u0026"`,
		"a letter escaped":                  `"\u0041"`,
		"a space escaped":                   `"\u0020"`,
		"U+007F escaped":                    `"\u007f"`,
		"a line separator escaped":          `"\u2028"`,
		"a surrogate pair escaped":          `"\ud834\udd1e"`,
		"an escape cut short":               `"\u00"`,
		"an escape of three digits":         `"\u001"`,
		"an escape that is no hexadecimal":  `"\u00zz"`,
		"a backslash at the end":            `"\`,
		"a backslash before the end":        `"\"`,
		"an escape there is not":            `"\x01"`,
		"a control character as it is":      "\"a\x01b\"",
		"a line feed as it is":              "\"a\nb\"",
		"the first character as it is":      "\"\x00\"",
		"a byte that is no UTF-8":           "\"a\xffb\"",
		"a character written too long":      "\"\xc0\xaf\"",
		"half of a surrogate pair in UTF-8": "\"\xed\xa0\x80\"",
		"single quotation marks":            `'abc'`,
	} {
		r := &canonicalReader{data: []byte(text)}
		if got := r.str(); r.end() {
			t.Errorf("%s: %s is read as %q", name, text, got)
		}
	}
}

func TestCanonicalNumber(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]uint64{
		"0": 0, "1": 1, "10": 10, "9223372036854775807": math.MaxInt64, "18446744073709551615": math.MaxUint64,
	} {
		r := &canonicalReader{data: []byte(text)}
		if got := r.unum(); !r.end() || got != want {
			t.Errorf("%s is read as %d (to the end: %v)", text, got, r.end())
		}
		w := &canonicalWriter{}
		w.unum(want)
		if string(w.buf) != text || w.bad {
			t.Errorf("%d is written as %s", want, w.buf)
		}
	}
	for name, text := range map[string]string{
		"nothing": "", "a minus sign": "-1", "minus nothing": "-0", "a plus sign": "+1", "a zero first": "01", "two zeros": "00",
		"a point": "1.0", "an exponent": "1e2", "an exponent in capitals": "1E2", "one beyond the last": "18446744073709551616",
		"far beyond the last": "99999999999999999999999", "hexadecimal": "0x10", "a space first": " 1", "a space after": "1 ", "text": `"1"`,
		"digits that are not ASCII": "١",
	} {
		r := &canonicalReader{data: []byte(text)}
		if got := r.unum(); r.end() {
			t.Errorf("%s: %q is read as %d", name, text, got)
		}
	}
	// A number is read into what holds it, and no further.
	for text, fits := range map[string]bool{"9223372036854775807": true, "9223372036854775808": false} {
		r := &canonicalReader{data: []byte(text)}
		if got := r.num(); r.end() != fits {
			t.Errorf("%s as an int64 = %d, read: %v", text, got, r.end())
		}
	}
	for text, fits := range map[string]bool{"2147483647": true, "2147483648": false} {
		r := &canonicalReader{data: []byte(text)}
		if got := r.small(); r.end() != fits {
			t.Errorf("%s as a small number = %d, read: %v", text, got, r.end())
		}
	}
	// A number below nothing cannot be written.
	w := &canonicalWriter{}
	w.num(-1)
	if !w.bad || len(w.buf) != 0 {
		t.Errorf("a number below nothing is written as %s", w.buf)
	}
	// After the first thing that is wrong, nothing more is read.
	r := &canonicalReader{data: []byte(`x"a"1true`)}
	r.lit("y")
	if r.str() != "" || r.unum() != 0 || r.boolean() || r.has("x") || r.end() {
		t.Error("a reader that has stopped reads on")
	}
	for _, text := range []string{"tru", "TRUE", "1", ""} {
		r := &canonicalReader{data: []byte(text)}
		if r.boolean() || r.end() {
			t.Errorf("%q is read as true or false", text)
		}
	}
}

// FuzzCanonicalString: whatever text is written is read back as it was, is
// JSON that any reader reads as the same text, and holds no byte below a
// space; and whatever bytes are read are exactly what the writer writes for
// the text they were read as - a string has one form.
func FuzzCanonicalString(f *testing.F) {
	for _, seed := range []string{
		"", "logs/", "\x01\b\f\n\r\t\x1f \x7f<>&\"\\/\u2028\u2029\u00e9\U0001d11e", `"\u000a"`, `"\n"`, `"\u003c"`, "\"\xff\"", `"a\\b"`,
		strings.Repeat("long/", 204) + "1024",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		written, ok := appendCanonicalString(nil, string(data))
		if ok != utf8.Valid(data) {
			t.Fatalf("%q: written %v, valid UTF-8 %v", data, ok, utf8.Valid(data))
		}
		if ok {
			if bytes.ContainsFunc(written, func(r rune) bool { return r < 0x20 }) {
				t.Fatalf("%q is written with a control character: %q", data, written)
			}
			r := &canonicalReader{data: written}
			if back := r.str(); !r.end() || back != string(data) {
				t.Fatalf("%q is written as %s and read as %q", data, written, back)
			}
			var read string
			if err := json.Unmarshal(written, &read); err != nil || read != string(data) {
				t.Fatalf("a reader of JSON reads %s as %q, %v", written, read, err)
			}
		}
		r := &canonicalReader{data: data}
		text := r.str()
		if !r.end() {
			return
		}
		again, ok := appendCanonicalString(nil, text)
		if !ok || !bytes.Equal(again, data) {
			t.Fatalf("%q is read as %q, which is written as %q", data, text, again)
		}
	})
}
