package iam

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
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

// bucketShape is the shape of each field of a statement of a bucket policy
// in its wire form: all but the Sid are read later.
func bucketShape(field string) shape {
	if field == "Sid" {
		return shapeString
	}
	return shapeAny
}

// ParseBucketPolicy parses and validates the policy of the named bucket.
// Every statement needs a Principal - "*", {"AWS": "*"}, or users named as
// "arn:aws:iam:::user/<name>" - and may be about that bucket and its objects
// only, and about S3 actions only. The error wraps domain.ErrInvalidArgument;
// it is a *PolicyError, which reads as the first fault of the document and
// holds every fault found.
func ParseBucketPolicy(data []byte, bucketName string) (BucketPolicy, error) {
	policy, faults := parseBucketPolicy(data, bucketName)
	if err := faults.err(domain.ErrInvalidArgument); err != nil {
		return BucketPolicy{}, err
	}
	return policy, nil
}

// parseBucketPolicy parses a bucket policy, collecting its faults, located
// when it is refused (parseLocated).
func parseBucketPolicy(data []byte, bucketName string) (BucketPolicy, *faultList) {
	return parseLocated(func(faults *faultList) BucketPolicy { return parseBucketPolicyInto(faults, data, bucketName) })
}

// parseBucketPolicyInto parses a bucket policy, recording its faults in the
// order ParseBucketPolicy has always met them.
func parseBucketPolicyInto(faults *faultList, data []byte, bucketName string) BucketPolicy {
	if len(data) > MaxPolicyBytes {
		faults.add("", invalidPolicy("larger than %d bytes", MaxPolicyBytes))
		return BucketPolicy{}
	}
	tree, ok := readDocument(faults, data, bucketPolicyFieldsAt)
	if !ok {
		return BucketPolicy{}
	}
	faults.tree = tree
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire wireBucketPolicy
	if err := decoder.Decode(&wire); err != nil {
		addDecodeFaults(faults, err, typeFaults(tree, shapeAny, bucketShape))
		return BucketPolicy{}
	}
	if decoder.More() {
		faults.addAt("", decoder.InputOffset(), invalidPolicy("data after the document"))
	}
	nullConditions := false
	for i, ws := range wire.Statement {
		// A condition that is there and says nothing would make a
		// statement that was meant to be restricted apply always.
		if bytes.Equal(bytes.TrimSpace(ws.Condition), []byte("null")) {
			faults.add(pointerOf(statementPointer(i), "Condition"), invalidPolicy("statement %d: Condition is null", i))
			nullConditions = true
		}
	}
	policy := BucketPolicy{}
	// What is left when the principals are taken out is an identity
	// policy, and is read as one.
	rest := map[string]any{"Version": wire.Version}
	statements := make([]map[string]any, 0, len(wire.Statement))
	for i, ws := range wire.Statement {
		principals, refused := parsePrincipal(ws.Principal)
		for _, fault := range refused {
			pointer := pointerOf(statementPointer(i), "Principal") + fault.where
			if fault.item >= 0 {
				pointer = faults.item(pointer, fault.item)
			}
			faults.add(pointer, invalidPolicy("statement %d: %v", i, fault.err))
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
		faults.add("", invalidPolicy("%v", err))
		return BucketPolicy{}
	}
	// The identity policy has the statements, and the fields of each, of
	// the document as sent: its faults are where they are in it, located or
	// not as this document's are. A null Condition, which the step above
	// said of each statement in the words of a bucket policy, is not said
	// again in those of an identity policy; every other fault is kept, also
	// where another is at the same place.
	identityFaults := &faultList{locate: faults.locate}
	document := parsePolicyInto(identityFaults, identity)
	for _, fault := range identityFaults.faults {
		if nullConditions && fault.Reason == invalidPolicy("a Condition is null").Error() {
			continue
		}
		faults.add(faults.clamp(fault.Pointer), errString(fault.Reason))
	}
	faults.truncated = faults.truncated || identityFaults.truncated
	if identityFaults.failed() {
		return BucketPolicy{}
	}
	policy.Document = document
	own, objects := Resource{Bucket: bucketName}.String(), Resource{Bucket: bucketName, Key: "x"}.String()
	objects = strings.TrimSuffix(objects, "x")
	for i, s := range policy.Document.Statements {
		for j, action := range s.Actions {
			if !strings.HasPrefix(strings.ToLower(action), "s3:") {
				faults.add(faults.item(pointerOf(statementPointer(i), "Action"), j), invalidPolicy("statement %d: a bucket policy is about S3 actions, not %q", i, action))
				continue
			}
			// Lifting a governance retention and committing a replica
			// are given by a policy of the user, by name, and by nothing
			// else. Whoever may write a bucket's policy - by a wildcard,
			// perhaps - must not be able to give them to themselves.
			if s.Effect == Allow && Action(action).byNameOnly() {
				faults.add(faults.item(pointerOf(statementPointer(i), "Action"), j), invalidPolicy("statement %d: a bucket policy cannot allow %s", i, action))
			}
		}
		for j, resource := range s.Resources {
			if resource != own && !strings.HasPrefix(resource, objects) {
				faults.add(faults.item(pointerOf(statementPointer(i), "Resource"), j), invalidPolicy("statement %d: resource %q is not this bucket or in it", i, resource))
			}
		}
	}
	if faults.failed() {
		return BucketPolicy{}
	}
	return policy
}

// principalFault is a fault of a Principal: where in it - "" for the
// Principal itself, "/AWS" for its AWS - and, for a name of a list, which.
type principalFault struct {
	where string
	item  int
	err   error
}

// parsePrincipal reads the Principal of a statement: the names of its users,
// or PrincipalAnyone. It reports every name it refuses.
func parsePrincipal(raw json.RawMessage) ([]string, []principalFault) {
	if len(raw) == 0 {
		return nil, []principalFault{{"", -1, errString("Principal is required")}}
	}
	var names oneOrMany
	var anyone string
	if err := json.Unmarshal(raw, &anyone); err == nil {
		if anyone != PrincipalAnyone {
			return nil, []principalFault{{"", -1, errString(`a Principal that is a string must be "*"`)}}
		}
		return []string{PrincipalAnyone}, nil
	}
	var object struct {
		AWS oneOrMany `json:"AWS"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		where := ""
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
			where = "/AWS"
		}
		return nil, []principalFault{{where, -1, errString(`Principal must be "*" or {"AWS": ...}`)}}
	}
	if len(object.AWS) == 0 || len(object.AWS) > maxPolicyValues {
		return nil, []principalFault{{"/AWS", -1, errString("Principal names nobody, or too many")}}
	}
	var faults []principalFault
	for i, name := range object.AWS {
		switch user, isUser := strings.CutPrefix(name, userARNPrefix); {
		case name == PrincipalAnyone:
			names = append(names, PrincipalAnyone)
		case isUser && ValidateName(user) == nil:
			names = append(names, user)
		default:
			faults = append(faults, principalFault{"/AWS", i, errString(`a principal is "*" or "` + userARNPrefix + `<name>"`)})
		}
	}
	if len(faults) > 0 {
		return nil, faults
	}
	return names, nil
}

type errString string

func (e errString) Error() string { return string(e) }

// judge says what the policy says about a request by the principal, as the
// package-level judge does: with a nil collector it stops at the first Deny
// that matches; with one it reads every statement and tells the collector,
// as from source, those that had a say - a statement about somebody else,
// and an Allow of what only a user's own policy gives, have none.
func (p BucketPolicy) judge(principal Principal, action Action, resource string, ctx Context, c *collector, source Source) verdict {
	result := silent
	for i := range p.Document.Statements {
		s := &p.Document.Statements[i]
		about := false
		for _, name := range p.Principals[i] {
			// A user is named by name. The root identity and anonymous
			// callers have none, and are reached by "*" only.
			about = about || name == PrincipalAnyone || (principal.User != "" && name == principal.User)
		}
		if !about {
			continue
		}
		// What only a user's own policy gives, by name, no bucket policy
		// gives - not even one that names it, should one have been stored
		// before ParseBucketPolicy refused it.
		mute := s.Effect == Allow && action.byNameOnly()
		var unknown bool
		var decidedUnknown *bool
		if c != nil && !mute {
			decidedUnknown = &unknown
		}
		matched := s.matches(action, resource, ctx, decidedUnknown)
		if c != nil && !mute {
			c.cite(source, 0, i, s, matched, unknown)
		}
		if !matched || mute {
			continue
		}
		if s.Effect == Deny {
			if c == nil {
				return denied
			}
			result = denied
			continue
		}
		if result != denied {
			result = allowed
		}
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
