package iam

import (
	"fmt"
	"slices"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
)

// A sub-bucket policy is the policy of a sub-bucket: a named first-level
// prefix of a bucket (ADR-0017). It is written in the language of bucket
// policies and confined to the sub-bucket's keys, so that it can grant what
// the bucket's policy is silent about there and nothing more.

// subBucketObjectActions are the actions on objects a sub-bucket policy may
// name: each is asked on a key, and counts only for a key of the sub-bucket.
func subBucketObjectActions() []Action {
	return []Action{
		ActionGetObject, ActionPutObject, ActionDeleteObject,
		ActionGetObjectVersion, ActionDeleteObjectVersion,
		ActionGetObjectRetention, ActionPutObjectRetention,
		ActionGetObjectLegalHold, ActionPutObjectLegalHold,
		ActionGetObjectTagging, ActionPutObjectTagging, ActionDeleteObjectTagging,
		ActionAbortMultipartUpload, ActionListMultipartUploadParts,
	}
}

// subBucketListActions are the listings a sub-bucket policy may allow. Each
// is asked on the bucket, and counts only for a listing whose prefix begins
// with the sub-bucket's name and a slash. A sub-bucket policy cannot deny
// one: a listing of the whole bucket would still show the keys.
func subBucketListActions() []Action {
	return []Action{ActionListBucket, ActionListBucketVersions, ActionListBucketMultipartUploads}
}

// subBucketAction reports whether the action is one a sub-bucket policy may
// name, matched without regard to case as actions are.
func subBucketAction(action string) (Action, bool) {
	for _, known := range append(subBucketObjectActions(), subBucketListActions()...) {
		if strings.EqualFold(action, string(known)) {
			return known, true
		}
	}
	return "", false
}

// SubBucketPolicy is the parsed, confined policy of one sub-bucket. It can be
// had only from ParseSubBucketPolicy; the zero value is the policy of no
// sub-bucket, and counts for no request.
type SubBucketPolicy struct {
	bucket string
	name   string
	policy BucketPolicy
}

// Bucket returns the name of the bucket the sub-bucket is in.
func (p SubBucketPolicy) Bucket() string { return p.bucket }

// Name returns the name of the sub-bucket.
func (p SubBucketPolicy) Name() string { return p.name }

// Policy returns the policy as a bucket policy: its statements and whom
// each is about.
func (p SubBucketPolicy) Policy() BucketPolicy { return p.policy }

// policyError is a policy a sub-bucket cannot keep: it reads as the reason,
// and is a domain.ErrInvalidPolicy.
type policyError struct{ err error }

// Error returns the reason the policy cannot be kept.
func (e policyError) Error() string { return e.err.Error() }

// Unwrap returns domain.ErrInvalidPolicy and the reason.
func (e policyError) Unwrap() []error { return []error{domain.ErrInvalidPolicy, e.err} }

func invalidSubBucketPolicy(format string, args ...any) error {
	return policyError{invalidPolicy("sub-bucket policy: %s", fmt.Sprintf(format, args...))}
}

// ParseSubBucketPolicy parses and validates the policy of the sub-bucket sub
// of the bucket bucketName (ADR-0017). It is a bucket policy (see
// ParseBucketPolicy), and more confined than one:
//
//   - every resource is a key of the sub-bucket - it begins
//     "arn:aws:s3:::<bucket>/<sub>/" - or the bucket itself, for listing;
//   - every action is named, without a wildcard, and is one on objects or a
//     listing (s3:ListBucket, s3:ListBucketVersions,
//     s3:ListBucketMultipartUploads): no action of the bucket, of
//     sub-buckets or of administration, and neither
//     s3:BypassGovernanceRetention nor a replication action, not even in a
//     Deny;
//   - no Allow is about "*": the policy of a sub-bucket allows users it
//     names, and never a caller who has not authenticated;
//   - no Deny names a listing: a listing of the whole bucket would still
//     show the keys, so the denial would promise what it cannot keep.
//
// A document larger than MaxPolicyBytes is refused as well. An error about
// the document wraps domain.ErrInvalidPolicy, which is a
// domain.ErrInvalidArgument. A name that cannot be the bucket's wraps
// domain.ErrInvalidBucketName, and one that cannot be the sub-bucket's
// domain.ErrInvalidArgument without domain.ErrInvalidPolicy: they are errors
// about the names, not the document.
func ParseSubBucketPolicy(doc []byte, bucketName, sub string) (SubBucketPolicy, error) {
	if err := checkNames(bucketName, sub); err != nil {
		return SubBucketPolicy{}, fmt.Errorf("iam.ParseSubBucketPolicy: %w", err)
	}
	if len(doc) > MaxPolicyBytes {
		return SubBucketPolicy{}, invalidSubBucketPolicy("larger than %d bytes", MaxPolicyBytes)
	}
	parsed, err := ParseBucketPolicy(doc, bucketName)
	if err != nil {
		return SubBucketPolicy{}, policyError{err}
	}
	own := Resource{Bucket: bucketName}.String()
	keys := Resource{Bucket: bucketName, Key: sub + "/"}.String()
	for i, s := range parsed.Document.Statements {
		if s.Effect == Allow && slices.Contains(parsed.Principals[i], PrincipalAnyone) {
			return SubBucketPolicy{}, invalidSubBucketPolicy("statement %d: an Allow names the users it allows, never \"*\"", i)
		}
		for _, name := range s.Actions {
			action, known := subBucketAction(name)
			if !known {
				return SubBucketPolicy{}, invalidSubBucketPolicy("statement %d: %q is not an action on objects or a listing, named without a wildcard", i, name)
			}
			if s.Effect == Deny && slices.Contains(subBucketListActions(), action) {
				return SubBucketPolicy{}, invalidSubBucketPolicy("statement %d: a sub-bucket policy cannot deny %s", i, action)
			}
		}
		for _, resource := range s.Resources {
			if resource != own && !strings.HasPrefix(resource, keys) {
				return SubBucketPolicy{}, invalidSubBucketPolicy("statement %d: resource %q is neither a key of the sub-bucket (%s*) nor the bucket", i, resource, keys)
			}
		}
	}
	return SubBucketPolicy{bucket: bucketName, name: sub, policy: parsed}, nil
}

// checkNames refuses names a bucket or a sub-bucket cannot have, as far as
// confining a policy needs: each is then a literal of the ARNs it begins -
// no wildcard, no slash - so that "arn:aws:s3:::<bucket>/<sub>/" begins the
// keys of that sub-bucket and of no other. The sub-bucket's rule is
// bucket.ValidateSubBucketName's, which this package cannot import (the
// bucket package depends on it, through delivery); a test holds the two
// alike. The bucket's is
// looser than bucket.ValidateName, which the bucket was made by.
func checkNames(bucketName, sub string) error {
	valid := len(bucketName) >= 3 && len(bucketName) <= 63
	for i := 0; valid && i < len(bucketName); i++ {
		c := bucketName[i]
		valid = (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-'
	}
	if !valid {
		return fmt.Errorf("%w: %q", domain.ErrInvalidBucketName, bucketName)
	}
	valid = len(sub) >= 1 && len(sub) <= 63
	for i := 0; valid && i < len(sub); i++ {
		c := sub[i]
		letterOrDigit := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		valid = letterOrDigit || (c == '-' && i > 0 && i < len(sub)-1)
	}
	if !valid {
		return fmt.Errorf("%w: a sub-bucket is named with 1 to 63 lower-case letters, digits and hyphens", domain.ErrInvalidArgument)
	}
	return nil
}

// covers reports whether the policy has a say in the request: it is about a
// key of the sub-bucket and an action on objects, or about a listing of the
// bucket whose prefix begins with the sub-bucket's name and a slash ("sub/",
// not "sub", not "sub-x/"). Nothing else - not the bucket's configuration,
// not another bucket, not a listing of the whole bucket - is the
// sub-bucket's to decide.
func (p SubBucketPolicy) covers(req Request) bool {
	r := req.Resource
	if p.bucket == "" || p.name == "" || r.Node != "" || r.Admin != "" || r.Bucket != p.bucket {
		return false
	}
	prefix := p.name + "/"
	if r.Key != "" {
		return strings.HasPrefix(r.Key, prefix) && slices.Contains(subBucketObjectActions(), req.Action)
	}
	return strings.HasPrefix(req.ListPrefix, prefix) && slices.Contains(subBucketListActions(), req.Action)
}

// judge says what the policy says about the request, and is silent about
// every request it does not cover.
func (p SubBucketPolicy) judge(req Request, resource string, ctx Context) verdict {
	if !p.covers(req) {
		return silent
	}
	return p.policy.judge(req.Principal, req.Action, resource, ctx)
}
