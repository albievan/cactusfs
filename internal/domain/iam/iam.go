// Package iam holds the domain model for identity and access: principals,
// actions, resources, credentials and the authorization decision.
package iam

import (
	"fmt"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// Action names an operation a principal may be allowed to perform, in the
// IAM style "s3:GetObject".
type Action string

// The S3 actions implemented so far.
const (
	ActionListAllMyBuckets  Action = "s3:ListAllMyBuckets"
	ActionCreateBucket      Action = "s3:CreateBucket"
	ActionDeleteBucket      Action = "s3:DeleteBucket"
	ActionListBucket        Action = "s3:ListBucket"
	ActionGetBucketLocation Action = "s3:GetBucketLocation"
	ActionGetObject         Action = "s3:GetObject"
	ActionPutObject         Action = "s3:PutObject"
	ActionDeleteObject      Action = "s3:DeleteObject"
)

// Resource names what an action applies to.
type Resource struct {
	// Bucket is the bucket name, or empty for account-level actions.
	Bucket string
	// Key is the object key, or empty for bucket-level actions.
	Key string
}

// String returns the resource in ARN form.
func (r Resource) String() string {
	switch {
	case r.Bucket == "":
		return "arn:aws:s3:::*"
	case r.Key == "":
		return "arn:aws:s3:::" + r.Bucket
	default:
		return "arn:aws:s3:::" + r.Bucket + "/" + r.Key
	}
}

// Principal is an authenticated identity. The zero value is the anonymous
// principal, which has authenticated as nobody.
type Principal struct {
	// AccessKeyID is the key the request was signed with.
	AccessKeyID string
	// Root marks the deployment's root identity.
	Root bool
}

// Anonymous reports whether the principal is unauthenticated.
func (p Principal) Anonymous() bool {
	return p.AccessKeyID == ""
}

// Request is one authorization question: may Principal perform Action on
// Resource?
type Request struct {
	Principal Principal
	Action    Action
	Resource  Resource
}

// Decide answers an authorization request. The rule is default deny: until
// users and policies exist (milestone 4) only the root principal is allowed,
// and an anonymous principal is never allowed.
//
// A refusal wraps domain.ErrAccessDenied.
func Decide(req Request) error {
	if req.Principal.Anonymous() {
		return fmt.Errorf("%w: anonymous principal may not %s on %s", domain.ErrAccessDenied, req.Action, req.Resource)
	}
	if req.Principal.Root {
		return nil
	}
	return fmt.Errorf("%w: %s may not %s on %s", domain.ErrAccessDenied, req.Principal.AccessKeyID, req.Action, req.Resource)
}

// RootCredential is the deployment's root API key as it is stored: the secret
// is sealed and can be opened only with the KMS.
type RootCredential struct {
	// AccessKeyID is the public half of the key.
	AccessKeyID string
	// Secret is the sealed secret half.
	Secret SealedSecret
	// CreatedAt is when the deployment was bootstrapped.
	CreatedAt time.Time
}
