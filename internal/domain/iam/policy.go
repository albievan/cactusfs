package iam

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

// errWantStrings is what oneOrMany says of a value that is neither.
const errWantStrings = errString("want a string or a list of strings")

// UnmarshalJSON accepts a string or a list of strings.
func (o *oneOrMany) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*o = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return errWantStrings
	}
	*o = many
	return nil
}

// nullCondition reports whether some statement of a raw document has a
// Condition that is null: there, and saying nothing.
func nullCondition(data []byte) bool {
	var probe struct {
		Statement []struct {
			Condition json.RawMessage `json:"Condition"`
		} `json:"Statement"`
	}
	if json.Unmarshal(data, &probe) != nil {
		return false
	}
	for _, s := range probe.Statement {
		if bytes.Equal(bytes.TrimSpace(s.Condition), []byte("null")) {
			return true
		}
	}
	return false
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

// conditionOperators are the operators of the language, in the order a
// statement keeps its conditions.
func conditionOperators() []string {
	return []string{
		OpStringEquals, OpStringNotEquals, OpStringLike, OpStringNotLike, OpIPAddress, OpNotIPAddress,
		OpBool, OpDateGreaterThan, OpDateLessThan,
	}
}

// conditionKeys are the condition keys of the language, in the order a
// statement keeps its conditions.
func conditionKeys() []string {
	return []string{KeySourceIP, KeySecureTransport, KeyCurrentTime, KeyUsername}
}

// identityShape is the shape of each field of a statement of an identity
// policy in its wire form.
func identityShape(field string) shape {
	switch field {
	case "Action", "Resource":
		return shapeStrings
	case "Condition":
		return shapeConditions
	default:
		return shapeString
	}
}

// ParsePolicy parses and validates a policy document. The error wraps
// domain.ErrInvalidArgument; it is a *PolicyError, which reads as the first
// fault of the document and holds every fault found.
func ParsePolicy(data []byte) (Document, error) {
	doc, faults := parsePolicy(data)
	if err := faults.err(domain.ErrInvalidArgument); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// parsePolicy parses a policy document, collecting its faults, located when
// it is refused (parseLocated).
func parsePolicy(data []byte) (Document, *faultList) {
	return parseLocated(func(faults *faultList) Document { return parsePolicyInto(faults, data) })
}

// parsePolicyInto parses a policy document, recording its faults. It makes
// the checks it has always made, in the order it has always made them, so
// that the first fault is the one it has always reported; it goes on after
// a fault for as long as the document can still be read.
func parsePolicyInto(faults *faultList, data []byte) Document {
	if len(data) > MaxPolicyBytes {
		faults.add("", invalidPolicy("larger than %d bytes", MaxPolicyBytes))
		return Document{}
	}
	// encoding/json is forgiving in ways a policy must not be: of two
	// fields with one name it keeps the last, and it matches names without
	// regard to case. Both would let a document mean something else than
	// it reads.
	tree, ok := readDocument(faults, data, fieldsAt)
	if !ok {
		return Document{}
	}
	faults.tree = tree
	if nullCondition(data) {
		located := false
		for i, statement := range tree.statements() {
			if condition := statement.child("Condition"); condition != nil && condition.kind == kindNull {
				faults.add(statementPointer(i)+"/Condition", invalidPolicy("a Condition is null"))
				located = true
			}
		}
		if !located {
			faults.add("", invalidPolicy("a Condition is null"))
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire wireDocument
	if err := decoder.Decode(&wire); err != nil {
		addDecodeFaults(faults, err, typeFaults(tree, shapeString, identityShape))
		return Document{}
	}
	if decoder.More() {
		faults.addAt("", decoder.InputOffset(), invalidPolicy("data after the document"))
	}
	doc := Document{Version: wire.Version}
	statements := tree.statements()
	for i, ws := range wire.Statement {
		var condition *jsonNode
		if i < len(statements) {
			condition = statements[i].child("Condition")
		}
		doc.Statements = append(doc.Statements, readStatement(faults, i, ws, condition))
	}
	doc.validate(faults)
	if faults.failed() {
		return Document{}
	}
	return doc
}

// readStatement makes a statement of its wire form, recording the faults of
// its Condition. condition is the Condition as written, for the order of its
// operators and keys; without it - when the faults are not located - they
// are taken in the order of the map, and a fault is a fault all the same.
func readStatement(faults *faultList, i int, ws wireStatement, condition *jsonNode) Statement {
	statement := Statement{Sid: ws.Sid, Effect: Effect(ws.Effect), Actions: ws.Action, Resources: ws.Resource}
	if ws.Condition == nil {
		return statement
	}
	// A condition block that is there and tests nothing would make a
	// statement that was meant to be restricted apply always.
	if len(ws.Condition) == 0 {
		faults.add(pointerOf(statementPointer(i), "Condition"), invalidPolicy("statement %d: Condition is empty", i))
	}
	// Each check is made of every operator, or key, in the order written
	// when the faults are located and in the order of the map otherwise:
	// ranging over the map itself, as the parser always has, allocates
	// nothing.
	var testsNothing map[string]bool
	emptyOperator := func(operator string) {
		if len(ws.Condition[operator]) == 0 {
			faults.add(pointerOf(statementPointer(i), "Condition", operator), invalidPolicy("statement %d: condition %s tests nothing", i, operator))
			if testsNothing == nil {
				testsNothing = map[string]bool{}
			}
			testsNothing[operator] = true
		}
	}
	unknownOperator := func(operator string) {
		if !slices.Contains(conditionOperators(), operator) && !testsNothing[operator] {
			faults.add(pointerOf(statementPointer(i), "Condition", operator), invalidPolicy("statement %d: condition operator %q is not supported", i, operator))
		}
	}
	if condition != nil {
		for _, operator := range condition.fieldNames() {
			emptyOperator(operator)
		}
	} else {
		for operator := range ws.Condition {
			emptyOperator(operator)
		}
	}
	// Conditions are kept in a fixed order, so that a document always
	// reads back the same.
	for _, operator := range conditionOperators() {
		tests, ok := ws.Condition[operator]
		if !ok {
			continue
		}
		for _, key := range conditionKeys() {
			if values, ok := tests[key]; ok {
				statement.Conditions = append(statement.Conditions, Condition{Operator: operator, Key: key, Values: values})
			}
		}
		unknownKey := func(key string) {
			if !slices.Contains(conditionKeys(), key) {
				faults.add(pointerOf(statementPointer(i), "Condition", operator, key), invalidPolicy("statement %d: condition key %q is not supported", i, key))
			}
		}
		if condition != nil {
			for _, key := range condition.child(operator).fieldNames() {
				unknownKey(key)
			}
		} else {
			for key := range tests {
				unknownKey(key)
			}
		}
	}
	if condition != nil {
		for _, operator := range condition.fieldNames() {
			unknownOperator(operator)
		}
	} else {
		for operator := range ws.Condition {
			unknownOperator(operator)
		}
	}
	return statement
}

// statementPointer is the JSON pointer of the i-th statement.
func statementPointer(i int) string { return "/Statement/" + strconv.Itoa(i) }

// pointerOf is the JSON pointer of the value at the field names under
// pointer. It is made when a fault is recorded, never on the way to a
// document that is accepted.
func pointerOf(pointer string, names ...string) string {
	for _, name := range names {
		pointer += "/" + escapePointer(name)
	}
	return pointer
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
// domain.ErrInvalidArgument; it is a *PolicyError, whose pointers count every
// field that may be a list as one, as MarshalJSON writes them.
func (d Document) Validate() error {
	faults := &faultList{}
	d.validate(faults)
	return faults.err(domain.ErrInvalidArgument)
}

// validate records every fault of a document; the first is the one
// Validate has always reported.
func (d Document) validate(faults *faultList) {
	if d.Version != PolicyVersion {
		faults.add("/Version", invalidPolicy("Version must be %q", PolicyVersion))
	}
	if len(d.Statements) == 0 || len(d.Statements) > MaxPolicyStatements {
		faults.add("/Statement", invalidPolicy("must have 1 to %d statements", MaxPolicyStatements))
	}
	for i, s := range d.Statements {
		s.validate(faults, i)
	}
}

// validate records every fault of the i-th statement of a document.
func (s Statement) validate(faults *faultList, i int) {
	if s.Effect != Allow && s.Effect != Deny {
		faults.add(pointerOf(statementPointer(i), "Effect"), invalidPolicy("statement %d: Effect must be Allow or Deny", i))
	}
	// One reason for both, as it has always read; each field that has too
	// few or too many is pointed at.
	if len(s.Actions) == 0 || len(s.Actions) > maxPolicyValues {
		faults.add(pointerOf(statementPointer(i), "Action"), invalidPolicy("statement %d: needs 1 to %d actions and resources", i, maxPolicyValues))
	}
	if len(s.Resources) == 0 || len(s.Resources) > maxPolicyValues {
		faults.add(pointerOf(statementPointer(i), "Resource"), invalidPolicy("statement %d: needs 1 to %d actions and resources", i, maxPolicyValues))
	}
	for j, action := range s.Actions {
		if err := validateActionPattern(action); err != nil {
			faults.add(faults.item(pointerOf(statementPointer(i), "Action"), j), invalidPolicy("statement %d: %v", i, err))
		}
	}
	for j, resource := range s.Resources {
		if err := validateResourcePattern(resource); err != nil {
			faults.add(faults.item(pointerOf(statementPointer(i), "Resource"), j), invalidPolicy("statement %d: %v", i, err))
		}
	}
	seen := map[string]bool{}
	for _, c := range s.Conditions {
		for _, fault := range c.faults() {
			pointer := pointerOf(statementPointer(i), "Condition", c.Operator, c.Key)
			if fault.value >= 0 {
				pointer = faults.item(pointer, fault.value)
			}
			faults.add(pointer, invalidPolicy("statement %d: %v", i, fault.err))
		}
		if seen[c.Operator+"\x00"+c.Key] {
			faults.add(pointerOf(statementPointer(i), "Condition", c.Operator, c.Key), invalidPolicy("statement %d: condition %s on %s is given twice", i, c.Operator, c.Key))
		}
		seen[c.Operator+"\x00"+c.Key] = true
	}
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

// conditionFault is a fault of a condition: of one of its values, by index,
// or of the whole condition when value is negative.
type conditionFault struct {
	value int
	err   error
}

// faults lists what is wrong with a condition: a key, an operator or a
// number of values it cannot have - after which nothing more is said of it -
// or else each value its operator cannot read.
func (c Condition) faults() []conditionFault {
	allowed := operatorsFor(c.Key)
	if allowed == nil {
		return []conditionFault{{-1, fmt.Errorf("condition key %q is not supported", c.Key)}}
	}
	if !slices.Contains(allowed, c.Operator) {
		return []conditionFault{{-1, fmt.Errorf("condition operator %q cannot be used with %s", c.Operator, c.Key)}}
	}
	if len(c.Values) == 0 || len(c.Values) > maxPolicyValues {
		return []conditionFault{{-1, fmt.Errorf("condition %s on %s needs 1 to %d values", c.Operator, c.Key, maxPolicyValues)}}
	}
	var faults []conditionFault
	for j, value := range c.Values {
		var err error
		switch c.Operator {
		case OpIPAddress, OpNotIPAddress:
			_, err = parsePrefix(value)
		case OpBool:
			if value != "true" && value != "false" {
				err = errors.New("want true or false")
			}
		case OpDateGreaterThan, OpDateLessThan:
			_, err = time.Parse(time.RFC3339, value)
		}
		if err != nil {
			faults = append(faults, conditionFault{j, fmt.Errorf("condition %s on %s: value %q: %w", c.Operator, c.Key, value, err)})
		}
	}
	return faults
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
// any run of characters and '?' for exactly one. A character is a UTF-8
// encoded code point, as in IAM: '?' matches "é" whole, never half of it.
// A byte that is not part of valid UTF-8 counts as one character of its
// own. It runs in time proportional to len(pattern) * len(s) at worst and
// never backtracks exponentially.
func wildcardMatch(pattern, s string) bool {
	p, i := 0, 0
	star, mark := -1, 0
	for i < len(s) {
		_, width := utf8.DecodeRuneInString(s[i:])
		switch {
		// The star is looked at first: a '*' in the pattern is always the
		// wildcard, also when the text happens to have a '*' there.
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case p < len(pattern) && pattern[p] == '?':
			p++
			i += width
		case p < len(pattern) && strings.HasPrefix(pattern[p:], s[i:i+width]) && sameCharacter(pattern[p:], width):
			p += width
			i += width
		case star >= 0:
			// The star takes one more character of the text, and the rest
			// of the pattern is tried from the one after it.
			p = star + 1
			_, taken := utf8.DecodeRuneInString(s[mark:])
			mark += taken
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

// sameCharacter reports whether the character pattern begins with is width
// bytes long: a literal of the pattern matches a character of the text only
// whole, so that an invalid byte of the text never matches the first byte
// of a valid character of the pattern.
func sameCharacter(pattern string, width int) bool {
	_, w := utf8.DecodeRuneInString(pattern)
	return w == width
}

// patternCovers reports whether an action pattern of a statement with the
// given effect covers the action.
func patternCovers(effect Effect, pattern string, action Action) bool {
	// Lifting a retention, and committing a replica, are allowed only by
	// their names (Action.byNameOnly). A Deny may still cover them with a
	// wildcard: denying more is never wrong.
	if action.byNameOnly() && effect == Allow {
		return strings.EqualFold(pattern, string(action))
	}
	// Action names are matched without regard to case, as in IAM.
	return pattern == "*" || wildcardMatch(strings.ToLower(pattern), strings.ToLower(string(action)))
}

// matches reports whether the statement applies to the request.
func (s Statement) matches(action Action, resource string, ctx Context) bool {
	hit := false
	for _, pattern := range s.Actions {
		if patternCovers(s.Effect, pattern, action) {
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
