package bucket

import (
	"fmt"
	"slices"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
)

// The kinds of configuration a bucket can carry besides what it is created
// with. Each is stored whole, as one document, and replaced whole.
const (
	// ConfigPolicy is the bucket policy, as the JSON the client sent.
	ConfigPolicy = "policy"
	// ConfigCORS is the cross-origin rules.
	ConfigCORS = "cors"
	// ConfigTagging is the tags of the bucket.
	ConfigTagging = "tagging"
)

// MaxConfigBytes bounds one configuration document of a bucket.
const MaxConfigBytes = 64 << 10

// ValidConfigKind reports whether kind is a kind of bucket configuration.
func ValidConfigKind(kind string) bool {
	return kind == ConfigPolicy || kind == ConfigCORS || kind == ConfigTagging || kind == ConfigLifecycle
}

// Limits of a CORS configuration, from the Amazon S3 API reference.
const (
	maxCORSRules  = 100
	maxCORSValues = 100
)

// CORSRule says which origins may make which cross-origin requests.
type CORSRule struct {
	ID string `json:"id,omitempty"`
	// AllowedOrigins are origins, each with at most one "*" wildcard.
	AllowedOrigins []string `json:"allowed_origins"`
	// AllowedMethods are HTTP methods: GET, PUT, POST, DELETE, HEAD.
	AllowedMethods []string `json:"allowed_methods"`
	// AllowedHeaders are request headers a preflight may ask for, each
	// with at most one "*" wildcard.
	AllowedHeaders []string `json:"allowed_headers,omitempty"`
	// ExposeHeaders are response headers a browser may show a script.
	ExposeHeaders []string `json:"expose_headers,omitempty"`
	// MaxAgeSeconds is how long a browser may remember a preflight.
	MaxAgeSeconds int `json:"max_age_seconds,omitempty"`
}

// CORS is the cross-origin configuration of a bucket.
type CORS struct {
	Rules []CORSRule `json:"rules"`
}

func invalidCORS(format string, args ...any) error {
	return fmt.Errorf("%w: CORS: %s", domain.ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// Validate reports whether the configuration is one that can be kept.
func (c CORS) Validate() error {
	if len(c.Rules) == 0 || len(c.Rules) > maxCORSRules {
		return invalidCORS("needs 1 to %d rules", maxCORSRules)
	}
	for i, rule := range c.Rules {
		if len(rule.AllowedOrigins) == 0 || len(rule.AllowedMethods) == 0 {
			return invalidCORS("rule %d: needs an origin and a method", i)
		}
		if len(rule.AllowedOrigins) > maxCORSValues || len(rule.AllowedHeaders) > maxCORSValues || len(rule.ExposeHeaders) > maxCORSValues || len(rule.ID) > 255 {
			return invalidCORS("rule %d: too many values", i)
		}
		for _, method := range rule.AllowedMethods {
			if !slices.Contains([]string{"GET", "PUT", "POST", "DELETE", "HEAD"}, method) {
				return invalidCORS("rule %d: method %q", i, method)
			}
		}
		for _, pattern := range slices.Concat(rule.AllowedOrigins, rule.AllowedHeaders) {
			if pattern == "" || strings.Count(pattern, "*") > 1 || strings.ContainsAny(pattern, "\r\n") {
				return invalidCORS("rule %d: %q has more than one wildcard, or none of anything", i, pattern)
			}
		}
		for _, header := range rule.ExposeHeaders {
			if !HeaderName(header) || strings.Contains(header, "*") {
				return invalidCORS("rule %d: exposed header %q", i, header)
			}
		}
		for _, header := range rule.AllowedHeaders {
			if !HeaderName(strings.Replace(header, "*", "x", 1)) {
				return invalidCORS("rule %d: allowed header %q", i, header)
			}
		}
		if rule.MaxAgeSeconds < 0 {
			return invalidCORS("rule %d: MaxAgeSeconds", i)
		}
	}
	return nil
}

// HeaderName reports whether s can be the name of an HTTP header: a token of
// RFC 9110.
func HeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !letter && !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}
	return true
}

// oneWildcard reports whether s matches a pattern with at most one "*".
func oneWildcard(pattern, s string, fold bool) bool {
	if fold {
		pattern, s = strings.ToLower(pattern), strings.ToLower(s)
	}
	prefix, suffix, wild := strings.Cut(pattern, "*")
	if !wild {
		return pattern == s
	}
	return len(s) >= len(prefix)+len(suffix) && strings.HasPrefix(s, prefix) && strings.HasSuffix(s, suffix)
}

// Match returns the first rule that allows a request with the given method
// from the given origin, asking for the given request headers. Origins are
// compared exactly, headers without regard to case.
func (c CORS) Match(origin, method string, headers []string) (CORSRule, bool) {
	for _, rule := range c.Rules {
		if !slices.Contains(rule.AllowedMethods, method) {
			continue
		}
		if !slices.ContainsFunc(rule.AllowedOrigins, func(p string) bool { return oneWildcard(p, origin, false) }) {
			continue
		}
		allowed := true
		for _, header := range headers {
			allowed = allowed && slices.ContainsFunc(rule.AllowedHeaders, func(p string) bool { return oneWildcard(p, header, true) })
		}
		if allowed {
			return rule, true
		}
	}
	return CORSRule{}, false
}

// AllowsAnyOrigin reports whether the rule matched because it names every
// origin, in which case the answer names "*" and not the origin asked for.
func (r CORSRule) AllowsAnyOrigin() bool {
	return slices.Contains(r.AllowedOrigins, "*")
}
