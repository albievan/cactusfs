package iam

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// PolicyVersion is the one policy language version understood.
const PolicyVersion = "2012-10-17"

// Limits on a policy document. They bound the work of parsing and of every
// decision made with it.
const (
	MaxPolicyBytes      = 20 << 10
	MaxPolicyStatements = 64
	maxPolicyValues     = 128
)

// Effect is what a statement does when it matches.
type Effect string

// The two effects.
const (
	Allow Effect = "Allow"
	Deny  Effect = "Deny"
)

// Document is a policy: a list of statements in the IAM policy language.
//
//	{"Version": "2012-10-17", "Statement": [
//	  {"Effect": "Allow", "Action": ["s3:GetObject"], "Resource": ["arn:aws:s3:::photos/*"]}
//	]}
//
// Parsing is strict: a field, an operator or a condition key that this
// version does not know is an error, never ignored. A statement that was
// silently dropped could be the Deny somebody relied on.
type Document struct {
	Version    string
	Statements []Statement
}

// Statement is one rule of a policy.
type Statement struct {
	Sid       string
	Effect    Effect
	Actions   []string
	Resources []string
	// Conditions must all hold for the statement to match.
	Conditions []Condition
}

// Condition is one test on the context of a request: the value of Key is
// compared with Values by Operator, and the test holds when any of the
// values matches.
type Condition struct {
	Operator string
	Key      string
	Values   []string
}

// Condition operators.
const (
	OpStringEquals    = "StringEquals"
	OpStringNotEquals = "StringNotEquals"
	OpStringLike      = "StringLike"
	OpStringNotLike   = "StringNotLike"
	OpIPAddress       = "IpAddress"
	OpNotIPAddress    = "NotIpAddress"
	OpBool            = "Bool"
	OpDateGreaterThan = "DateGreaterThan"
	OpDateLessThan    = "DateLessThan"
)

// Condition keys.
const (
	// KeySourceIP is the address the request came from.
	KeySourceIP = "aws:SourceIp"
	// KeySecureTransport is "true" when the request arrived over TLS.
	KeySecureTransport = "aws:SecureTransport"
	// KeyCurrentTime is the time of the decision.
	KeyCurrentTime = "aws:CurrentTime"
	// KeyUsername is the name of the user the request acts as.
	KeyUsername = "aws:username"
)

// operatorsFor lists, per condition key, the operators that make sense.
func operatorsFor(key string) []string {
	switch key {
	case KeySourceIP:
		return []string{OpIPAddress, OpNotIPAddress}
	case KeySecureTransport:
		return []string{OpBool}
	case KeyCurrentTime:
		return []string{OpDateGreaterThan, OpDateLessThan}
	case KeyUsername:
		return []string{OpStringEquals, OpStringNotEquals, OpStringLike, OpStringNotLike}
	default:
		return nil
	}
}

// Context is what a condition can ask about a request.
type Context struct {
	// SourceIP is the address of the client; invalid when unknown.
	SourceIP netip.Addr
	// SecureTransport reports whether the request arrived over TLS.
	SecureTransport bool
	// Now is the time of the decision.
	Now time.Time
	// Username is the user the request acts as.
	Username string
}

// wire forms: fields that may be one string or a list of strings.
type oneOrMany []string

// UnmarshalJSON accepts a string or a list of strings.
func (o *oneOrMany) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*o = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("want a string or a list of strings")
	}
	*o = many
	return nil
}

type wireStatement struct {
	Sid       string                          `json:"Sid,omitempty"`
	Effect    string                          `json:"Effect"`
	Action    oneOrMany                       `json:"Action"`
	Resource  oneOrMany                       `json:"Resource"`
	Condition map[string]map[string]oneOrMany `json:"Condition,omitempty"`
}

type wireDocument struct {
	Version   string          `json:"Version"`
	Statement []wireStatement `json:"Statement"`
}

func invalidPolicy(format string, args ...any) error {
	return fmt.Errorf("%w: policy: %s", domain.ErrInvalidArgument, fmt.Sprintf(format, args...))
}

// ParsePolicy parses and validates a policy document. The error wraps
// domain.ErrInvalidArgument.
func ParsePolicy(data []byte) (Document, error) {
	if len(data) > MaxPolicyBytes {
		return Document{}, invalidPolicy("larger than %d bytes", MaxPolicyBytes)
	}
	// encoding/json is forgiving in ways a policy must not be: of two
	// fields with one name it keeps the last, and it matches names without
	// regard to case. Both would let a document mean something else than
	// it reads.
	if err := checkFields(data); err != nil {
		return Document{}, invalidPolicy("%v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire wireDocument
	if err := decoder.Decode(&wire); err != nil {
		return Document{}, invalidPolicy("%v", err)
	}
	if decoder.More() {
		return Document{}, invalidPolicy("data after the document")
	}
	doc := Document{Version: wire.Version}
	for i, ws := range wire.Statement {
		statement := Statement{Sid: ws.Sid, Effect: Effect(ws.Effect), Actions: ws.Action, Resources: ws.Resource}
		// A condition block that is there and tests nothing would make a
		// statement that was meant to be restricted apply always.
		if ws.Condition != nil && len(ws.Condition) == 0 {
			return Document{}, invalidPolicy("statement %d: Condition is empty", i)
		}
		for operator, tests := range ws.Condition {
			if len(tests) == 0 {
				return Document{}, invalidPolicy("statement %d: condition %s tests nothing", i, operator)
			}
		}
		// Conditions are kept in a fixed order, so that a document always
		// reads back the same.
		for _, operator := range []string{
			OpStringEquals, OpStringNotEquals, OpStringLike, OpStringNotLike, OpIPAddress, OpNotIPAddress,
			OpBool, OpDateGreaterThan, OpDateLessThan,
		} {
			tests, ok := ws.Condition[operator]
			if !ok {
				continue
			}
			delete(ws.Condition, operator)
			for _, key := range []string{KeySourceIP, KeySecureTransport, KeyCurrentTime, KeyUsername} {
				if values, ok := tests[key]; ok {
					statement.Conditions = append(statement.Conditions, Condition{Operator: operator, Key: key, Values: values})
					delete(tests, key)
				}
			}
			for key := range tests {
				return Document{}, invalidPolicy("statement %d: condition key %q is not supported", i, key)
			}
		}
		for operator := range ws.Condition {
			return Document{}, invalidPolicy("statement %d: condition operator %q is not supported", i, operator)
		}
		doc.Statements = append(doc.Statements, statement)
	}
	if err := doc.Validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// checkFields walks a document and refuses a field that appears twice in
// one object, and, where the fields are fixed, one that is not spelled
// exactly as the policy language spells it.
func checkFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := walkFields(decoder, ""); err != nil {
		return err
	}
	if _, err := decoder.Token(); err == nil {
		return fmt.Errorf("data after the document")
	}
	return nil
}

// fieldsAt lists the fields of the objects whose fields are fixed, by where
// the object is in the document.
func fieldsAt(where string) []string {
	switch where {
	case "":
		return []string{"Version", "Statement"}
	case "/Statement/[]":
		return []string{"Sid", "Effect", "Action", "Resource", "Condition"}
	default:
		return nil
	}
}

// maxPolicyDepth bounds how deeply a document may nest.
const maxPolicyDepth = 8

func walkFields(decoder *json.Decoder, where string) error {
	if strings.Count(where, "/") > maxPolicyDepth {
		return fmt.Errorf("nested too deeply")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			name, _ := token.(string)
			if seen[name] {
				return fmt.Errorf("field %q appears twice", name)
			}
			seen[name] = true
			if allowed := fieldsAt(where); allowed != nil {
				known := false
				for _, field := range allowed {
					known = known || field == name
				}
				if !known {
					return fmt.Errorf("unknown field %q", name)
				}
			}
			if err := walkFields(decoder, where+"/"+name); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := walkFields(decoder, where+"/[]"); err != nil {
				return err
			}
		}
	}
	// The closing bracket.
	_, err = decoder.Token()
	return err
}

// MarshalJSON writes the document in the policy language.
func (d Document) MarshalJSON() ([]byte, error) {
	wire := wireDocument{Version: d.Version, Statement: make([]wireStatement, len(d.Statements))}
	for i, s := range d.Statements {
		ws := wireStatement{Sid: s.Sid, Effect: string(s.Effect), Action: s.Actions, Resource: s.Resources}
		for _, c := range s.Conditions {
			if ws.Condition == nil {
				ws.Condition = map[string]map[string]oneOrMany{}
			}
			if ws.Condition[c.Operator] == nil {
				ws.Condition[c.Operator] = map[string]oneOrMany{}
			}
			ws.Condition[c.Operator][c.Key] = c.Values
		}
		wire.Statement[i] = ws
	}
	return json.Marshal(wire)
}

// UnmarshalJSON reads a document, validating it.
func (d *Document) UnmarshalJSON(data []byte) error {
	doc, err := ParsePolicy(data)
	if err != nil {
		return err
	}
	*d = doc
	return nil
}

// Validate reports whether the document is well formed. The error wraps
// domain.ErrInvalidArgument.
func (d Document) Validate() error {
	if d.Version != PolicyVersion {
		return invalidPolicy("Version must be %q", PolicyVersion)
	}
	if len(d.Statements) == 0 || len(d.Statements) > MaxPolicyStatements {
		return invalidPolicy("must have 1 to %d statements", MaxPolicyStatements)
	}
	for i, s := range d.Statements {
		if s.Effect != Allow && s.Effect != Deny {
			return invalidPolicy("statement %d: Effect must be Allow or Deny", i)
		}
		if len(s.Actions) == 0 || len(s.Actions) > maxPolicyValues || len(s.Resources) == 0 || len(s.Resources) > maxPolicyValues {
			return invalidPolicy("statement %d: needs 1 to %d actions and resources", i, maxPolicyValues)
		}
		for _, action := range s.Actions {
			if err := validateActionPattern(action); err != nil {
				return invalidPolicy("statement %d: %v", i, err)
			}
		}
		for _, resource := range s.Resources {
			if err := validateResourcePattern(resource); err != nil {
				return invalidPolicy("statement %d: %v", i, err)
			}
		}
		seen := map[string]bool{}
		for _, c := range s.Conditions {
			if err := c.validate(); err != nil {
				return invalidPolicy("statement %d: %v", i, err)
			}
			if seen[c.Operator+"\x00"+c.Key] {
				return invalidPolicy("statement %d: condition %s on %s is given twice", i, c.Operator, c.Key)
			}
			seen[c.Operator+"\x00"+c.Key] = true
		}
	}
	return nil
}

// validateActionPattern accepts "*" and "<service>:<name>" with wildcards in
// the name, for the services whose actions a policy can grant.
func validateActionPattern(pattern string) error {
	if pattern == "*" {
		return nil
	}
	service, name, ok := strings.Cut(pattern, ":")
	if !ok || name == "" || (!strings.EqualFold(service, "s3") && !strings.EqualFold(service, "admin")) {
		return fmt.Errorf("action %q: want \"*\", \"s3:<action>\" or \"admin:<action>\"", pattern)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '*' && c != '?' {
			return fmt.Errorf("action %q: letters, digits, '*' and '?' only", pattern)
		}
	}
	return nil
}

// validateResourcePattern accepts "*" and the two kinds of ARN there are.
func validateResourcePattern(pattern string) error {
	if pattern == "*" {
		return nil
	}
	if len(pattern) > 2048 || strings.ContainsAny(pattern, "\x00\n\r") {
		return fmt.Errorf("resource is too long or contains control characters")
	}
	if !strings.HasPrefix(pattern, s3ARNPrefix) && !strings.HasPrefix(pattern, adminARNPrefix) {
		return fmt.Errorf("resource %q: want \"*\" or an ARN beginning with %q or %q", pattern, s3ARNPrefix, adminARNPrefix)
	}
	return nil
}

func (c Condition) validate() error {
	allowed := operatorsFor(c.Key)
	if allowed == nil {
		return fmt.Errorf("condition key %q is not supported", c.Key)
	}
	ok := false
	for _, operator := range allowed {
		ok = ok || operator == c.Operator
	}
	if !ok {
		return fmt.Errorf("condition operator %q cannot be used with %s", c.Operator, c.Key)
	}
	if len(c.Values) == 0 || len(c.Values) > maxPolicyValues {
		return fmt.Errorf("condition %s on %s needs 1 to %d values", c.Operator, c.Key, maxPolicyValues)
	}
	for _, value := range c.Values {
		var err error
		switch c.Operator {
		case OpIPAddress, OpNotIPAddress:
			_, err = parsePrefix(value)
		case OpBool:
			if value != "true" && value != "false" {
				err = fmt.Errorf("want true or false")
			}
		case OpDateGreaterThan, OpDateLessThan:
			_, err = time.Parse(time.RFC3339, value)
		}
		if err != nil {
			return fmt.Errorf("condition %s on %s: value %q: %w", c.Operator, c.Key, value, err)
		}
	}
	return nil
}

// parsePrefix reads an address or a CIDR range.
func parsePrefix(value string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		addr, addrErr := netip.ParseAddr(value)
		if addrErr != nil || addr.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("want an IP address or a CIDR range, without a zone")
		}
		prefix = netip.PrefixFrom(addr, addr.BitLen())
	}
	// Addresses are compared in their plain form: an IPv4 address written
	// as IPv6 ("::ffff:10.0.0.1") would never match the IPv4 address a
	// request comes from, and a zone means nothing in a policy.
	if prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" {
		return netip.Prefix{}, fmt.Errorf("write IPv4 addresses as IPv4, and no zone")
	}
	return prefix.Masked(), nil
}

// holds reports whether the condition is true of the context, for a
// statement with the given effect.
//
// When the source address of a request is not known, a condition on it is
// decided against the request either way: it does not hold in an Allow, and
// it holds in a Deny. Nobody is let in, and nobody escapes being shut out,
// by not saying where they come from.
func (c Condition) holds(ctx Context, effect Effect) bool {
	switch c.Operator {
	case OpIPAddress, OpNotIPAddress:
		if !ctx.SourceIP.IsValid() {
			return effect == Deny
		}
		addr := ctx.SourceIP.Unmap().WithZone("")
		inside := false
		for _, value := range c.Values {
			if prefix, err := parsePrefix(value); err == nil && prefix.Contains(addr) {
				inside = true
			}
		}
		return inside == (c.Operator == OpIPAddress)
	case OpBool:
		for _, value := range c.Values {
			if (value == "true") == ctx.SecureTransport {
				return true
			}
		}
		return false
	case OpDateGreaterThan, OpDateLessThan:
		for _, value := range c.Values {
			when, err := time.Parse(time.RFC3339, value)
			if err != nil {
				continue
			}
			if (c.Operator == OpDateGreaterThan && ctx.Now.After(when)) || (c.Operator == OpDateLessThan && ctx.Now.Before(when)) {
				return true
			}
		}
		return false
	case OpStringEquals, OpStringNotEquals, OpStringLike, OpStringNotLike:
		matched := false
		for _, value := range c.Values {
			if c.Operator == OpStringEquals || c.Operator == OpStringNotEquals {
				matched = matched || value == ctx.Username
			} else {
				matched = matched || wildcardMatch(value, ctx.Username)
			}
		}
		return matched == (c.Operator == OpStringEquals || c.Operator == OpStringLike)
	default:
		return false
	}
}

// wildcardMatch reports whether s matches pattern, in which '*' stands for
// any run of characters and '?' for exactly one. It runs in time
// proportional to len(pattern) * len(s) at worst and never backtracks
// exponentially.
func wildcardMatch(pattern, s string) bool {
	p, i := 0, 0
	star, mark := -1, 0
	for i < len(s) {
		switch {
		// The star is looked at first: a '*' in the pattern is always the
		// wildcard, also when the text happens to have a '*' there.
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == s[i]):
			p++
			i++
		case star >= 0:
			p = star + 1
			mark++
			i = mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// matches reports whether the statement applies to the request.
func (s Statement) matches(action Action, resource string, ctx Context) bool {
	hit := false
	for _, pattern := range s.Actions {
		// Action names are matched without regard to case, as in IAM.
		if pattern == "*" || wildcardMatch(strings.ToLower(pattern), strings.ToLower(string(action))) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	hit = false
	for _, pattern := range s.Resources {
		if pattern == "*" || wildcardMatch(pattern, resource) {
			hit = true
			break
		}
	}
	if !hit {
		return false
	}
	for _, c := range s.Conditions {
		if !c.holds(ctx, s.Effect) {
			return false
		}
	}
	return true
}

// verdict is what a set of policies says about a request.
type verdict int

const (
	silent verdict = iota
	allowed
	denied
)

// judge evaluates documents: an explicit Deny wins over everything, an Allow
// is needed for anything, and silence is a refusal.
func judge(documents []Document, action Action, resource string, ctx Context) verdict {
	result := silent
	for _, doc := range documents {
		for _, s := range doc.Statements {
			if !s.matches(action, resource, ctx) {
				continue
			}
			if s.Effect == Deny {
				return denied
			}
			result = allowed
		}
	}
	return result
}
