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
	// Lifecycle rules.
	ActionGetLifecycle Action = "s3:GetLifecycleConfiguration"
	ActionPutLifecycle Action = "s3:PutLifecycleConfiguration"
	// Multipart uploads. Beginning one, uploading its parts and completing
	// it are s3:PutObject.
	ActionAbortMultipartUpload       Action = "s3:AbortMultipartUpload"
	ActionListMultipartUploadParts   Action = "s3:ListMultipartUploadParts"
	ActionListBucketMultipartUploads Action = "s3:ListBucketMultipartUploads"
	// ActionGetBucketNotification allows reading the notification rules of
	// a bucket (ADR-0025).
	ActionGetBucketNotification Action = "s3:GetBucketNotification"
	// ActionPutBucketNotification allows setting and removing the
	// notification rules of a bucket. A rule names a target; which targets
	// there are is not for this action to say.
	ActionPutBucketNotification Action = "s3:PutBucketNotification"
	// ActionGetReplication allows reading the replication configuration of
	// a bucket (ADR-0026).
	ActionGetReplication Action = "s3:GetReplicationConfiguration"
	// ActionPutReplication allows setting and removing the replication
	// configuration of a bucket: as in S3, removing has no action of its
	// own.
	ActionPutReplication Action = "s3:PutReplicationConfiguration"
	// ActionOverrideGovernance (s3:BypassGovernanceRetention) lifts a
	// governance retention. It is granted
	// only by a statement that names it: no wildcard includes it, so that
	// no role holds it by having "everything".
	ActionOverrideGovernance Action = "s3:BypassGovernanceRetention"
	// ActionReplicateObject allows committing a version as the copy of a
	// version of another deployment: with an ID, a time and a sequence of
	// the caller's choosing (ADR-0026). It also covers the questions the
	// source asks of the replica - whether the bucket is its replica,
	// whether a version is there - and the multipart uploads it makes. Like
	// ActionOverrideGovernance it is granted only by a statement that names
	// it, and by no bucket policy.
	ActionReplicateObject Action = "s3:ReplicateObject"
	// ActionReplicateDelete allows committing a delete marker as the copy
	// of one of another deployment. It is granted as ActionReplicateObject
	// is.
	ActionReplicateDelete Action = "s3:ReplicateDelete"
)

// byNameOnly reports whether the action is one that no wildcard grants: an
// Allow covers it only by naming it, so that no role holds it by having
// "everything", and no bucket policy can allow it at all - whoever may
// write a bucket's policy must not be able to give it to themselves. A Deny
// may still cover it with a wildcard: denying more is never wrong.
//
// These are the actions that undo what the store otherwise guarantees:
// lifting a retention, and making a version with an identity and a time the
// caller chose.
func (a Action) byNameOnly() bool {
	for _, named := range []Action{ActionOverrideGovernance, ActionReplicateObject, ActionReplicateDelete} {
		if strings.EqualFold(string(a), string(named)) {
			return true
		}
	}
	return false
}

// The admin actions implemented so far. Reading actions begin with Get or
// List, so that a policy can grant "admin:Get*" and "admin:List*".
const (
	// ActionGetObjectAccess allows reading who uploaded an object version
	// and who read it, how often and when. Reading the object does not
	// imply it.
	ActionGetObjectAccess Action = "admin:GetObjectAccess"
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
	// ActionGetSettings allows reading how every node of the deployment
	// was configured: its settings as they may be shown, its drives and
	// directories, the certificates it serves. No secret is part of it,
	// but the layout of a deployment is not for everybody either.
	ActionGetSettings Action = "admin:GetSettings"
)

// The admin actions of quotas, targets and replication (ADR-0023 to
// ADR-0026). None of them is an S3 action, so that "s3:*" on a bucket - the
// built-in bucket-admin - gives none: who administers a bucket does not lift
// its quota, define where the deployment connects to, or decide which
// deployment a bucket takes its versions from.
const (
	// ActionGetBucketQuota allows reading the quota of a bucket.
	ActionGetBucketQuota Action = "admin:GetBucketQuota"
	// ActionPutBucketQuota allows setting the quota of a bucket.
	ActionPutBucketQuota Action = "admin:PutBucketQuota"
	// ActionDeleteBucketQuota allows removing the quota of a bucket.
	ActionDeleteBucketQuota Action = "admin:DeleteBucketQuota"
	// ActionRecountBucketUsage allows having the usage of a bucket counted
	// again. It changes nothing a caller can see unless the count had
	// drifted, and it is work for every member of the metadata group: it
	// does not begin with Get.
	ActionRecountBucketUsage Action = "admin:RecountBucketUsage"
	// ActionListTargets allows listing the targets the deployment sends to.
	ActionListTargets Action = "admin:ListTargets"
	// ActionGetTarget allows reading a target, without its credentials:
	// nothing allows reading those.
	ActionGetTarget Action = "admin:GetTarget"
	// ActionPutTarget allows defining and changing a target: an address
	// outside the deployment that it will connect to, and the credential
	// it shows there.
	ActionPutTarget Action = "admin:PutTarget"
	// ActionDeleteTarget allows deleting a target.
	ActionDeleteTarget Action = "admin:DeleteTarget"
	// ActionGetTargetStatus allows reading whether a target is being
	// delivered to.
	ActionGetTargetStatus Action = "admin:GetTargetStatus"
	// ActionGetReplicationStatus allows reading how far the replication of
	// a bucket has come.
	ActionGetReplicationStatus Action = "admin:GetReplicationStatus"
	// ActionResyncReplication allows starting the walk that sends another
	// deployment what it lacks of a bucket.
	ActionResyncReplication Action = "admin:ResyncReplication"
	// ActionGetBucketReplicaMode allows reading whether a bucket is the
	// replica of another deployment's.
	ActionGetBucketReplicaMode Action = "admin:GetBucketReplicaMode"
	// ActionPutBucketReplicaMode allows making a bucket a replica and
	// promoting it.
	ActionPutBucketReplicaMode Action = "admin:PutBucketReplicaMode"
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
	// ActionReportReads hands in what a node counted of the reading of
	// objects (ADR-0016). A node reports its own counts only.
	ActionReportReads Action = "cluster:ReportReads"
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
	// Service is set, and nothing else is, when the node itself acts by a
	// standing instruction of the deployment and not for a caller: the
	// worker that carries out lifecycle rules is ServiceLifecycle. No
	// request can authenticate as a service.
	Service string
	// LockedSession marks the user of a console session that has reached
	// its timeout and is kept because the user is uploading objects
	// (ADR-0027). Such a principal may store objects where the user may,
	// and nothing else: Decide says so. Only the session use case sets it;
	// no API key, node or service is ever locked.
	LockedSession bool
}

// ServiceLifecycle is the service that carries out the lifecycle rules of
// buckets.
const ServiceLifecycle = "lifecycle"

// serviceMay reports what a service may do. The lifecycle worker removes
// what rules say has expired - and nothing a lock keeps: lifting a retention
// is not among its actions, so no rule ever removes a retained version.
func serviceMay(service string, action Action) bool {
	if service != ServiceLifecycle {
		return false
	}
	switch action {
	case ActionDeleteObject, ActionDeleteObjectVersion, ActionAbortMultipartUpload:
		return true
	}
	return false
}

// Session describes the connection a request arrived on.
type Session struct {
	// SourceIP is the address of the client; invalid when unknown.
	SourceIP netip.Addr
	// SecureTransport reports whether the request arrived over TLS.
	SecureTransport bool
	// RequestID identifies the request in logs and in the audit trail.
	RequestID string
	// Presigned says that the request was authenticated by a signature in
	// its URL: the principal is whoever signed the URL, and the caller is
	// whoever holds it.
	Presigned bool
}

// Name returns what the principal is called in logs.
func (p Principal) Name() string {
	if p.Service != "" {
		return "service/" + p.Service
	}
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
	return p.AccessKeyID == "" && p.NodeID == "" && p.User == "" && p.Service == ""
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
//     to set up and to recover a deployment; it has no policy to lose. That
//     includes the actions no wildcard grants (Action.byNameOnly): root
//     lifts a governance retention and commits a replica as it does
//     everything else, without a statement that names the action. What root
//     cannot do is what the state refuses to everybody - remove a version
//     under compliance, write a replica into a bucket that is none.
//   - A user may do what some policy of theirs allows and none denies. An
//     explicit Deny wins over every Allow.
//   - An API key with an inline policy may do what its owner may do AND the
//     inline policy allows: a key never exceeds its owner.
//   - A locked session (Principal.LockedSession) may store objects, and
//     nothing else, whatever its user is allowed: every other action is
//     refused before a policy is read. Storing an object is then decided as
//     for the same user with a session that is not locked.
//
// A refusal wraps domain.ErrAccessDenied - but the refusal of a locked
// session, which wraps domain.ErrSessionLocked and not ErrAccessDenied: it
// says nothing of what the user may do.
func Decide(req Request) error {
	if req.Principal.LockedSession && req.Action != ActionPutObject {
		// First of all: no policy, no kind of principal and no kind of
		// action has a say in it.
		return fmt.Errorf("%w: %s may not %s on %s until it is unlocked", domain.ErrSessionLocked, req.Principal.Name(), req.Action, req.Resource)
	}
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
	if service := req.Principal.Service; service != "" {
		// A service is that and nothing else, and is bound by no policy:
		// what it may do is fixed here.
		p := req.Principal
		if p.AccessKeyID == "" && !p.Root && p.NodeID == "" && p.User == "" && serviceMay(service, req.Action) {
			return nil
		}
		return refused
	}
	if node := req.Principal.NodeID; node != "" || req.Action.cluster() {
		// A node adds, records and removes itself only, and reports no
		// counts but its own.
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
