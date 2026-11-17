package iam

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// The parsers as they were before their faults were located (commit
// 3e620ef), frozen: FuzzPolicyFaults holds the parsers to them - the same
// documents accepted, to the same result; the same refused, with the same
// text and the same sentinels. They use the wire types of the package, whose
// names encoding/json quotes in its errors, and its helpers that did not
// change. Nothing but the fuzz target and TestFaultsAreLocated calls them.

func oracleParsePolicy(data []byte) (Document, error) {
	if len(data) > MaxPolicyBytes {
		return Document{}, invalidPolicy("larger than %d bytes", MaxPolicyBytes)
	}
	if err := oracleCheckFields(data, fieldsAt); err != nil {
		return Document{}, invalidPolicy("%v", err)
	}
	if nullCondition(data) {
		return Document{}, invalidPolicy("a Condition is null")
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
		if ws.Condition != nil && len(ws.Condition) == 0 {
			return Document{}, invalidPolicy("statement %d: Condition is empty", i)
		}
		for operator, tests := range ws.Condition {
			if len(tests) == 0 {
				return Document{}, invalidPolicy("statement %d: condition %s tests nothing", i, operator)
			}
		}
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
	if err := oracleValidate(doc); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func oracleCheckFields(data []byte, fields func(where string) []string) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := oracleWalkFields(decoder, "", fields); err != nil {
		return err
	}
	if _, err := decoder.Token(); err == nil {
		return errors.New("data after the document")
	}
	return nil
}

func oracleWalkFields(decoder *json.Decoder, where string, fields func(where string) []string) error {
	if strings.Count(where, "/") > maxPolicyDepth {
		return errors.New("nested too deeply")
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
			if allowed := fields(where); allowed != nil {
				known := false
				for _, field := range allowed {
					known = known || field == name
				}
				if !known {
					return fmt.Errorf("unknown field %q", name)
				}
			}
			if err := oracleWalkFields(decoder, where+"/"+name, fields); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := oracleWalkFields(decoder, where+"/[]", fields); err != nil {
				return err
			}
		}
	}
	_, err = decoder.Token()
	return err
}

func oracleValidate(d Document) error {
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
			if err := oracleConditionValidate(c); err != nil {
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

func oracleConditionValidate(c Condition) error {
	allowed := operatorsFor(c.Key)
	if allowed == nil {
		return fmt.Errorf("condition key %q is not supported", c.Key)
	}
	if !slices.Contains(allowed, c.Operator) {
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
				err = errors.New("want true or false")
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

func oracleParseBucketPolicy(data []byte, bucketName string) (BucketPolicy, error) {
	if len(data) > MaxPolicyBytes {
		return BucketPolicy{}, invalidPolicy("larger than %d bytes", MaxPolicyBytes)
	}
	if err := oracleCheckFields(data, bucketPolicyFieldsAt); err != nil {
		return BucketPolicy{}, invalidPolicy("%v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire wireBucketPolicy
	if err := decoder.Decode(&wire); err != nil {
		return BucketPolicy{}, invalidPolicy("%v", err)
	}
	if decoder.More() {
		return BucketPolicy{}, invalidPolicy("data after the document")
	}
	for i, ws := range wire.Statement {
		if bytes.Equal(bytes.TrimSpace(ws.Condition), []byte("null")) {
			return BucketPolicy{}, invalidPolicy("statement %d: Condition is null", i)
		}
	}
	policy := BucketPolicy{}
	rest := map[string]any{"Version": wire.Version}
	statements := make([]map[string]any, 0, len(wire.Statement))
	for i, ws := range wire.Statement {
		principals, err := oracleParsePrincipal(ws.Principal)
		if err != nil {
			return BucketPolicy{}, invalidPolicy("statement %d: %v", i, err)
		}
		policy.Principals = append(policy.Principals, principals)
		statement := map[string]any{"Effect": ws.Effect, "Action": ws.Action, "Resource": ws.Resource}
		if ws.Sid != "" {
			statement["Sid"] = ws.Sid
		}
		if ws.Condition != nil {
			statement["Condition"] = ws.Condition
		}
		statements = append(statements, statement)
	}
	rest["Statement"] = statements
	identity, err := json.Marshal(rest)
	if err != nil {
		return BucketPolicy{}, invalidPolicy("%v", err)
	}
	if policy.Document, err = oracleParsePolicy(identity); err != nil {
		return BucketPolicy{}, err
	}
	own, objects := Resource{Bucket: bucketName}.String(), Resource{Bucket: bucketName, Key: "x"}.String()
	objects = strings.TrimSuffix(objects, "x")
	for i, s := range policy.Document.Statements {
		for _, action := range s.Actions {
			if !strings.HasPrefix(strings.ToLower(action), "s3:") {
				return BucketPolicy{}, invalidPolicy("statement %d: a bucket policy is about S3 actions, not %q", i, action)
			}
			if s.Effect == Allow && Action(action).byNameOnly() {
				return BucketPolicy{}, invalidPolicy("statement %d: a bucket policy cannot allow %s", i, action)
			}
		}
		for _, resource := range s.Resources {
			if resource != own && !strings.HasPrefix(resource, objects) {
				return BucketPolicy{}, invalidPolicy("statement %d: resource %q is not this bucket or in it", i, resource)
			}
		}
	}
	return policy, nil
}

func oracleParsePrincipal(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, errString("Principal is required")
	}
	var names oneOrMany
	var anyone string
	if err := json.Unmarshal(raw, &anyone); err == nil {
		if anyone != PrincipalAnyone {
			return nil, errString(`a Principal that is a string must be "*"`)
		}
		return []string{PrincipalAnyone}, nil
	}
	var object struct {
		AWS oneOrMany `json:"AWS"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, errString(`Principal must be "*" or {"AWS": ...}`)
	}
	if len(object.AWS) == 0 || len(object.AWS) > maxPolicyValues {
		return nil, errString("Principal names nobody, or too many")
	}
	for _, name := range object.AWS {
		switch user, isUser := strings.CutPrefix(name, userARNPrefix); {
		case name == PrincipalAnyone:
			names = append(names, PrincipalAnyone)
		case isUser && ValidateName(user) == nil:
			names = append(names, user)
		default:
			return nil, errString(`a principal is "*" or "` + userARNPrefix + `<name>"`)
		}
	}
	return names, nil
}

// oraclePolicyError is the error of a sub-bucket's policy as it was.
type oraclePolicyError struct{ err error }

// Error returns the reason.
func (e oraclePolicyError) Error() string { return e.err.Error() }

// Unwrap returns domain.ErrInvalidPolicy and the reason.
func (e oraclePolicyError) Unwrap() []error { return []error{domain.ErrInvalidPolicy, e.err} }

func oracleInvalidSubBucketPolicy(format string, args ...any) error {
	return oraclePolicyError{invalidPolicy("sub-bucket policy: %s", fmt.Sprintf(format, args...))}
}

func oracleParseSubBucketPolicy(doc []byte, bucketName, sub string) (SubBucketPolicy, error) {
	if err := checkNames(bucketName, sub); err != nil {
		return SubBucketPolicy{}, fmt.Errorf("iam.ParseSubBucketPolicy: %w", err)
	}
	if len(doc) > MaxPolicyBytes {
		return SubBucketPolicy{}, oracleInvalidSubBucketPolicy("larger than %d bytes", MaxPolicyBytes)
	}
	parsed, err := oracleParseBucketPolicy(doc, bucketName)
	if err != nil {
		return SubBucketPolicy{}, oraclePolicyError{err}
	}
	own := Resource{Bucket: bucketName}.String()
	keys := Resource{Bucket: bucketName, Key: sub + "/"}.String()
	for i, s := range parsed.Document.Statements {
		if s.Effect == Allow && slices.Contains(parsed.Principals[i], PrincipalAnyone) {
			return SubBucketPolicy{}, oracleInvalidSubBucketPolicy("statement %d: an Allow names the users it allows, never \"*\"", i)
		}
		for _, name := range s.Actions {
			action, known := subBucketAction(name)
			if !known {
				return SubBucketPolicy{}, oracleInvalidSubBucketPolicy("statement %d: %q is not an action on objects or a listing, named without a wildcard", i, name)
			}
			if s.Effect == Deny && slices.Contains(subBucketListActions(), action) {
				return SubBucketPolicy{}, oracleInvalidSubBucketPolicy("statement %d: a sub-bucket policy cannot deny %s", i, action)
			}
		}
		for _, resource := range s.Resources {
			if resource != own && !strings.HasPrefix(resource, keys) {
				return SubBucketPolicy{}, oracleInvalidSubBucketPolicy("statement %d: resource %q is neither a key of the sub-bucket (%s*) nor the bucket", i, resource, keys)
			}
		}
	}
	return SubBucketPolicy{bucket: bucketName, name: sub, policy: parsed}, nil
}
