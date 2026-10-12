package iam

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

// A bucket policy is a policy attached to a bucket instead of to a user: its
// statements say who - Principal - may or may not do what to the bucket and
// its objects. It is the only thing that can let somebody in who has not
// authenticated, and it can do that for reading only.

// PrincipalAnyone is the principal that matches every caller, anonymous
// ones included.
const PrincipalAnyone = "*"

// userARNPrefix is how a bucket policy names a user of this deployment.
const userARNPrefix = "arn:aws:iam:::user/"

// BucketPolicy is a parsed bucket policy.
type BucketPolicy struct {
	// Document holds the statements as an identity policy has them.
	Document Document
	// Principals holds, for each statement, whom it is about: user names,
	// or PrincipalAnyone.
	Principals [][]string
}

type wireBucketStatement struct {
	Sid       string          `json:"Sid,omitempty"`
	Effect    json.RawMessage `json:"Effect"`
	Principal json.RawMessage `json:"Principal"`
	Action    json.RawMessage `json:"Action"`
	Resource  json.RawMessage `json:"Resource"`
	Condition json.RawMessage `json:"Condition,omitempty"`
}

type wireBucketPolicy struct {
	Version   json.RawMessage       `json:"Version"`
	Statement []wireBucketStatement `json:"Statement"`
}

func bucketPolicyFieldsAt(where string) []string {
	switch where {
	case "/Statement/[]":
		return []string{"Sid", "Effect", "Principal", "Action", "Resource", "Condition"}
	case "/Statement/[]/Principal":
		return []string{"AWS"}
	default:
		return fieldsAt(where)
	}
}

// ParseBucketPolicy parses and validates the policy of the named bucket.
// Every statement needs a Principal - "*", {"AWS": "*"}, or users named as
// "arn:aws:iam:::user/<name>" - and may be about that bucket and its objects
// only, and about S3 actions only. The error wraps domain.ErrInvalidArgument.
func ParseBucketPolicy(data []byte, bucketName string) (BucketPolicy, error) {
	if len(data) > MaxPolicyBytes {
		return BucketPolicy{}, invalidPolicy("larger than %d bytes", MaxPolicyBytes)
	}
	if err := checkFields(data, bucketPolicyFieldsAt); err != nil {
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
		// A condition that is there and says nothing would make a
		// statement that was meant to be restricted apply always.
		if bytes.Equal(bytes.TrimSpace(ws.Condition), []byte("null")) {
			return BucketPolicy{}, invalidPolicy("statement %d: Condition is null", i)
		}
	}
	policy := BucketPolicy{}
	// What is left when the principals are taken out is an identity
	// policy, and is read as one.
	rest := map[string]any{"Version": wire.Version}
	statements := make([]map[string]any, 0, len(wire.Statement))
	for i, ws := range wire.Statement {
		principals, err := parsePrincipal(ws.Principal)
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
	if policy.Document, err = ParsePolicy(identity); err != nil {
		return BucketPolicy{}, err
	}
	own, objects := Resource{Bucket: bucketName}.String(), Resource{Bucket: bucketName, Key: "x"}.String()
	objects = strings.TrimSuffix(objects, "x")
	for i, s := range policy.Document.Statements {
		for _, action := range s.Actions {
			if !strings.HasPrefix(strings.ToLower(action), "s3:") {
				return BucketPolicy{}, invalidPolicy("statement %d: a bucket policy is about S3 actions, not %q", i, action)
			}
			// Lifting a governance retention is given by a policy of
			// the user, by name, and by nothing else. Whoever may write
			// a bucket's policy - by a wildcard, perhaps - must not be
			// able to give it to themselves.
			if s.Effect == Allow && strings.EqualFold(action, string(ActionOverrideGovernance)) {
				return BucketPolicy{}, invalidPolicy("statement %d: a bucket policy cannot allow %s", i, ActionOverrideGovernance)
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

func parsePrincipal(raw json.RawMessage) ([]string, error) {
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

type errString string

func (e errString) Error() string { return string(e) }

// judge says what the policy says about a request by the principal.
func (p BucketPolicy) judge(principal Principal, action Action, resource string, ctx Context) verdict {
	result := silent
	for i, s := range p.Document.Statements {
		about := false
		for _, name := range p.Principals[i] {
			// A user is named by name. The root identity and anonymous
			// callers have none, and are reached by "*" only.
			about = about || name == PrincipalAnyone || (principal.User != "" && name == principal.User)
		}
		if !about || !s.matches(action, resource, ctx) {
			continue
		}
		if s.Effect == Deny {
			return denied
		}
		result = allowed
	}
	return result
}

// anonymousReadable are the actions a bucket policy can open to callers who
// have not authenticated: reading objects and listing them. Nothing that
// writes, deletes or configures is ever anonymous, whatever a policy says.
func anonymousReadable(action Action) bool {
	switch action {
	case ActionGetObject, ActionGetObjectVersion, ActionListBucket, ActionListBucketVersions, ActionGetBucketLocation:
		return true
	}
	return false
}

// OpenToAnyone reports whether some statement of the policy allows something
// to every caller: an Allow whose principal is "*". Conditions and Deny
// statements are not weighed, and it does not say that a caller who has not
// authenticated is let in - that is Decide's to say, and only for the
// actions of anonymousReadable.
func (p BucketPolicy) OpenToAnyone() bool {
	for i, s := range p.Document.Statements {
		if s.Effect == Allow && slices.Contains(p.Principals[i], PrincipalAnyone) {
			return true
		}
	}
	return false
}

// BucketPolicySummary says what a bucket's policy is without what it says:
// what a list of buckets shows of each to who may read the policy.
type BucketPolicySummary struct {
	// Statements is how many statements the policy has.
	Statements int
	// OpenToAnyone says that some statement allows something to every
	// caller, whatever its conditions and the other statements say
	// (BucketPolicy.OpenToAnyone).
	OpenToAnyone bool
}

// Summary returns the summary of the policy. It classifies the document and
// decides nothing: what a caller may do under the policy is Decide's to say.
func (p BucketPolicy) Summary() BucketPolicySummary {
	return BucketPolicySummary{Statements: len(p.Document.Statements), OpenToAnyone: p.OpenToAnyone()}
}
