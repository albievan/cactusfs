package bucket_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
)

func TestCORSValidate(t *testing.T) {
	t.Parallel()
	rule := bucket.CORSRule{AllowedOrigins: []string{"https://app.example"}, AllowedMethods: []string{"GET"}}
	with := func(change func(r *bucket.CORSRule)) bucket.CORS {
		r := rule
		change(&r)
		return bucket.CORS{Rules: []bucket.CORSRule{r}}
	}
	many := bucket.CORS{}
	for range 101 {
		many.Rules = append(many.Rules, rule)
	}
	tests := []struct {
		name string
		cors bucket.CORS
		ok   bool
	}{
		{"one rule", bucket.CORS{Rules: []bucket.CORSRule{rule}}, true},
		{"every origin", with(func(r *bucket.CORSRule) { r.AllowedOrigins = []string{"*"} }), true},
		{"a wildcard origin", with(func(r *bucket.CORSRule) { r.AllowedOrigins = []string{"https://*.example"} }), true},
		{"headers and an age", with(func(r *bucket.CORSRule) {
			r.AllowedHeaders, r.ExposeHeaders, r.MaxAgeSeconds = []string{"x-amz-*", "Content-Type"}, []string{"ETag"}, 600
		}), true},
		{"no rules", bucket.CORS{}, false},
		{"too many rules", many, false},
		{"no origin", with(func(r *bucket.CORSRule) { r.AllowedOrigins = nil }), false},
		{"no method", with(func(r *bucket.CORSRule) { r.AllowedMethods = nil }), false},
		{"a method that is none", with(func(r *bucket.CORSRule) { r.AllowedMethods = []string{"PATCH"} }), false},
		{"a method in lower case", with(func(r *bucket.CORSRule) { r.AllowedMethods = []string{"get"} }), false},
		{"two wildcards", with(func(r *bucket.CORSRule) { r.AllowedOrigins = []string{"https://*.*.example"} }), false},
		{"an empty origin", with(func(r *bucket.CORSRule) { r.AllowedOrigins = []string{""} }), false},
		{"a line break in an origin", with(func(r *bucket.CORSRule) { r.AllowedOrigins = []string{"https://a\r\nSet-Cookie: x"} }), false},
		{"a line break in an exposed header", with(func(r *bucket.CORSRule) { r.ExposeHeaders = []string{"ETag\r\nX: y"} }), false},
		{"a wildcard exposed header", with(func(r *bucket.CORSRule) { r.ExposeHeaders = []string{"*"} }), false},
		{"a negative age", with(func(r *bucket.CORSRule) { r.MaxAgeSeconds = -1 }), false},
		{"a long ID", with(func(r *bucket.CORSRule) { r.ID = strings.Repeat("x", 256) }), false},
	}
	for _, tt := range tests {
		err := tt.cors.Validate()
		if (err == nil) != tt.ok || (err != nil && !errors.Is(err, domain.ErrInvalidArgument)) {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
	for kind, want := range map[string]bool{bucket.ConfigPolicy: true, bucket.ConfigCORS: true, bucket.ConfigTagging: true, "acl": false, "": false} {
		if bucket.ValidConfigKind(kind) != want {
			t.Errorf("ValidConfigKind(%q)", kind)
		}
	}
}

func TestCORSMatch(t *testing.T) {
	t.Parallel()
	cors := bucket.CORS{Rules: []bucket.CORSRule{
		{ID: "app", AllowedOrigins: []string{"https://app.example"}, AllowedMethods: []string{"GET", "PUT"}, AllowedHeaders: []string{"content-type", "x-amz-*"}},
		{ID: "tenants", AllowedOrigins: []string{"https://*.tenants.example"}, AllowedMethods: []string{"GET"}},
		{ID: "anyone", AllowedOrigins: []string{"*"}, AllowedMethods: []string{"HEAD"}},
	}}
	tests := []struct {
		origin, method string
		headers        []string
		want           string
	}{
		{"https://app.example", "GET", nil, "app"},
		{"https://app.example", "PUT", []string{"Content-Type", "X-Amz-Date"}, "app"},
		{"https://app.example", "PUT", []string{"Authorization"}, ""},
		{"https://app.example", "DELETE", nil, ""},
		{"https://APP.example", "GET", nil, ""},
		{"https://app.example.evil.test", "GET", nil, ""},
		{"https://a.tenants.example", "GET", nil, "tenants"},
		{"https://.tenants.example", "GET", nil, "tenants"},
		{"https://tenants.example", "GET", nil, ""},
		{"https://a.tenants.example", "GET", []string{"x-any"}, ""},
		{"https://evil.test", "HEAD", nil, "anyone"},
		{"https://evil.test", "GET", nil, ""},
		{"null", "HEAD", nil, "anyone"},
	}
	for _, tt := range tests {
		rule, ok := cors.Match(tt.origin, tt.method, tt.headers)
		if ok != (tt.want != "") || rule.ID != tt.want {
			t.Errorf("Match(%q, %s, %v) = %q, %v; want %q", tt.origin, tt.method, tt.headers, rule.ID, ok, tt.want)
		}
	}
	if !cors.Rules[2].AllowsAnyOrigin() || cors.Rules[1].AllowsAnyOrigin() {
		t.Error("AllowsAnyOrigin")
	}
}

// FuzzCORSMatch: a rule that is returned really allows the method, the
// origin and every header that was asked for.
func FuzzCORSMatch(f *testing.F) {
	f.Add("https://*.example", "x-amz-*", "https://a.example", "GET", "X-Amz-Date")
	f.Add("*", "*", "null", "PUT", "anything")
	f.Add("https://a*a", "", "https://a", "GET", "")
	f.Fuzz(func(t *testing.T, originPattern, headerPattern, origin, method, header string) {
		rule := bucket.CORSRule{AllowedOrigins: []string{originPattern}, AllowedMethods: []string{"GET", "PUT"}}
		if headerPattern != "" {
			rule.AllowedHeaders = []string{headerPattern}
		}
		cors := bucket.CORS{Rules: []bucket.CORSRule{rule}}
		if cors.Validate() != nil {
			return
		}
		var asked []string
		if header != "" {
			asked = []string{header}
		}
		_, ok := cors.Match(origin, method, asked)
		// A reference: at most one wildcard, which stands for any run of
		// characters, the empty one included.
		reference := func(pattern, s string) bool {
			prefix, suffix, wild := strings.Cut(pattern, "*")
			if !wild {
				return pattern == s
			}
			for i := len(prefix); i <= len(s)-len(suffix); i++ {
				if s[:len(prefix)] == prefix && s[i:] == suffix {
					return true
				}
			}
			return false
		}
		want := (method == "GET" || method == "PUT") && len(origin) >= 0 && reference(originPattern, origin) &&
			(header == "" || (headerPattern != "" && reference(strings.ToLower(headerPattern), strings.ToLower(header))))
		if len(origin) < len(strings.ReplaceAll(originPattern, "*", "")) {
			want = false
		}
		if ok != want {
			t.Fatalf("Match(%q against %q, %s, %q against %q) = %v, want %v", origin, originPattern, method, header, headerPattern, ok, want)
		}
	})
}
