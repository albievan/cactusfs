// Package iam holds the domain model for identity and access: principals,
// actions, resources, credentials and the authorization decision.
package iam

import (
	"fmt"
	"net/netip"
	"strings"
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
	// Versions.
	ActionGetObjectVersion    Action = "s3:GetObjectVersion"
	ActionDeleteObjectVersion Action = "s3:DeleteObjectVersion"
	ActionListBucketVersions  Action = "s3:ListBucketVersions"
	ActionGetBucketVersioning Action = "s3:GetBucketVersioning"
	ActionPutBucketVersioning Action = "s3:PutBucketVersioning"
	// Object Lock.
	ActionPutObjectRetention  Action = "s3:PutObjectRetention"
	ActionGetObjectRetention  Action = "s3:GetObjectRetention"
	ActionPutObjectLegalHold  Action = "s3:PutObjectLegalHold"
	ActionGetObjectLegalHold  Action = "s3:GetObjectLegalHold"
	ActionGetBucketObjectLock Action = "s3:GetBucketObjectLockConfiguration"
	ActionPutBucketObjectLock Action = "s3:PutBucketObjectLockConfiguration"
	// Bucket policy, CORS and tags.
	ActionGetBucketPolicy     Action = "s3:GetBucketPolicy"
	ActionPutBucketPolicy     Action = "s3:PutBucketPolicy"
	ActionDeleteBucketPolicy  Action = "s3:DeleteBucketPolicy"
	ActionGetBucketCORS       Action = "s3:GetBucketCORS"
	ActionPutBucketCORS       Action = "s3:PutBucketCORS"
	ActionGetBucketTagging    Action = "s3:GetBucketTagging"
	ActionPutBucketTagging    Action = "s3:PutBucketTagging"
	ActionGetObjectTagging    Action = "s3:GetObjectTagging"
	ActionPutObjectTagging    Action = "s3:PutObjectTagging"
	ActionDeleteObjectTagging Action = "s3:DeleteObjectTagging"
	// Multipart uploads. Beginning one, uploading its parts and completing
	// it are s3:PutObject.
	ActionAbortMultipartUpload       Action = "s3:AbortMultipartUpload"
	ActionListMultipartUploadParts   Action = "s3:ListMultipartUploadParts"
	ActionListBucketMultipartUploads Action = "s3:ListBucketMultipartUploads"
	// ActionOverrideGovernance (s3:BypassGovernanceRetention) lifts a
	// governance retention. It is granted
	// only by a statement that names it: no wildcard includes it, so that
	// no role holds it by having "everything".
	ActionOverrideGovernance Action = "s3:BypassGovernanceRetention"
)

// The admin actions implemented so far. Reading actions begin with Get or
// List, so that a policy can grant "admin:Get*" and "admin:List*".
const (
	// ActionGetMetrics allows reading the node's metrics.
	ActionGetMetrics Action = "admin:GetMetrics"

	ActionGetUser      Action = "admin:GetUser"
	ActionListUsers    Action = "admin:ListUsers"
	ActionPutUser      Action = "admin:PutUser"
	ActionDeleteUser   Action = "admin:DeleteUser"
	ActionGetGroup     Action = "admin:GetGroup"
	ActionListGroups   Action = "admin:ListGroups"
	ActionPutGroup     Action = "admin:PutGroup"
	ActionDeleteGroup  Action = "admin:DeleteGroup"
	ActionGetRole      Action = "admin:GetRole"
	ActionListRoles    Action = "admin:ListRoles"
	ActionPutRole      Action = "admin:PutRole"
	ActionDeleteRole   Action = "admin:DeleteRole"
	ActionGetPolicy    Action = "admin:GetPolicy"
	ActionListPolicies Action = "admin:ListPolicies"
	ActionPutPolicy    Action = "admin:PutPolicy"
	ActionDeletePolicy Action = "admin:DeletePolicy"
	ActionListKeys     Action = "admin:ListAPIKeys"
	ActionCreateKey    Action = "admin:CreateAPIKey"
	ActionDeleteKey    Action = "admin:DeleteAPIKey"
	ActionGetAuditLog  Action = "admin:GetAuditLog"
	// ActionGetClusterStatus allows reading the nodes, drives and
	// background work of the deployment.
	ActionGetClusterStatus Action = "admin:GetClusterStatus"
)

// ARN prefixes of the two kinds of resource a policy can name.
const (
	s3ARNPrefix    = "arn:aws:s3:::"
	adminARNPrefix = "arn:cactusfs:admin:::"
)

// The cluster actions: what the nodes of a cluster do to each other over
// node RPC. No user or API key is ever allowed them.
const (
	// ActionNodeRPC covers the calls nodes make in the course of their
	// work: reaching shards, the metadata leader and each other's state.
	ActionNodeRPC Action = "cluster:NodeRPC"
	// ActionRegisterNode records a node: where it is reached and what it
	// does.
	ActionRegisterNode Action = "cluster:RegisterNode"
	// ActionJoinNode adds a node to the metadata group.
	ActionJoinNode Action = "cluster:JoinNode"
	// ActionRemoveNode takes a node out of the cluster.
	ActionRemoveNode Action = "cluster:RemoveNode"
)

// cluster reports whether the action is one of the cluster actions.
func (a Action) cluster() bool {
	return strings.HasPrefix(string(a), "cluster:")
}

// Resource names what an action applies to.
type Resource struct {
	// Node is the node a cluster action applies to, or empty.
	Node string
	// Admin names what an admin action applies to, as "<kind>/<name>":
	// "user/alice", "role/read-only", "audit", "metrics". Empty for
	// anything else.
	Admin string
	// Bucket is the bucket name, or empty for account-level actions.
	Bucket string
	// Key is the object key, or empty for bucket-level actions.
	Key string
}

// String returns the resource in ARN form.
func (r Resource) String() string {
	switch {
	case r.Node != "":
		return "arn:cactusfs:cluster:::node/" + r.Node
	case r.Admin != "":
		return adminARNPrefix + r.Admin
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
	// NodeID is set, and nothing else is, when the caller is a node of the
	// cluster: it proved possession of the node certificate that names it.
	NodeID string
	// User is the name of the user the key belongs to; empty for root and
	// for nodes.
	User string
	// Policies are the documents that say what the user may do: those of
	// every role held directly or through a group, resolved when the
	// request was authenticated.
	Policies []Document
	// Boundary, when set, is the inline policy of the API key: the request
	// must be allowed by it as well.
	Boundary *Document
	// Session is where and how the request arrived; conditions test it.
	Session Session
}

// Session describes the connection a request arrived on.
type Session struct {
	// SourceIP is the address of the client; invalid when unknown.
	SourceIP netip.Addr
	// SecureTransport reports whether the request arrived over TLS.
	SecureTransport bool
	// RequestID identifies the request in logs and in the audit trail.
	RequestID string
}

// Name returns what the principal is called in logs.
func (p Principal) Name() string {
	if p.NodeID != "" {
		return "node/" + p.NodeID
	}
	if p.User != "" {
		return "user/" + p.User
	}
	return p.AccessKeyID
}

// Anonymous reports whether the principal is unauthenticated.
func (p Principal) Anonymous() bool {
	return p.AccessKeyID == "" && p.NodeID == "" && p.User == ""
}

// Request is one authorization question: may Principal perform Action on
// Resource?
type Request struct {
	Principal Principal
	Action    Action
	Resource  Resource
	// Now is the time of the decision; conditions on time test it.
	Now time.Time
	// BucketPolicy is the policy of the bucket the resource is in, if it
	// has one.
	BucketPolicy *BucketPolicy
}

// Decide answers an authorization request. The rule is default deny.
//
//   - Nobody is allowed anything without authenticating.
//   - Nodes of the cluster and the cluster actions belong to each other and
//     to nothing else.
//   - The root identity may do everything a user could be allowed. It exists
//     to set up and to recover a deployment; it has no policy to lose.
//   - A user may do what some policy of theirs allows and none denies. An
//     explicit Deny wins over every Allow.
//   - An API key with an inline policy may do what its owner may do AND the
//     inline policy allows: a key never exceeds its owner.
//
// A refusal wraps domain.ErrAccessDenied.
func Decide(req Request) error {
	if req.Principal.Anonymous() {
		// Only the policy of a bucket lets anybody in unauthenticated,
		// and only to read.
		session := Context{SourceIP: req.Principal.Session.SourceIP, SecureTransport: req.Principal.Session.SecureTransport, Now: req.Now}
		if req.BucketPolicy != nil && anonymousReadable(req.Action) &&
			req.BucketPolicy.judge(req.Principal, req.Action, req.Resource.String(), session) == allowed {
			return nil
		}
		return fmt.Errorf("%w: anonymous principal may not %s on %s", domain.ErrAccessDenied, req.Action, req.Resource)
	}
	refused := fmt.Errorf("%w: %s may not %s on %s", domain.ErrAccessDenied, req.Principal.Name(), req.Action, req.Resource)
	if node := req.Principal.NodeID; node != "" || req.Action.cluster() {
		// A node adds, records and removes itself only.
		own := req.Action == ActionNodeRPC || req.Resource.Node == node
		if node != "" && req.Principal.AccessKeyID == "" && !req.Principal.Root && req.Principal.User == "" && req.Action.cluster() && own {
			return nil
		}
		return refused
	}
	if req.Principal.Root {
		if req.Principal.User != "" {
			return refused
		}
		return nil
	}
	if req.Principal.User == "" {
		return refused
	}
	ctx := Context{
		SourceIP: req.Principal.Session.SourceIP, SecureTransport: req.Principal.Session.SecureTransport,
		Now: req.Now, Username: req.Principal.User,
	}
	resource := req.Resource.String()
	// The user's own policies and the bucket's: either may allow, and a
	// Deny in either wins over every Allow.
	own, bucket := judge(req.Principal.Policies, req.Action, resource, ctx), silent
	if req.BucketPolicy != nil {
		bucket = req.BucketPolicy.judge(req.Principal, req.Action, resource, ctx)
	}
	if own == denied || bucket == denied || (own != allowed && bucket != allowed) {
		return refused
	}
	if boundary := req.Principal.Boundary; boundary != nil && judge([]Document{*boundary}, req.Action, resource, ctx) != allowed {
		return fmt.Errorf("%w: the API key's own policy does not allow %s on %s", domain.ErrAccessDenied, req.Action, req.Resource)
	}
	return nil
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
