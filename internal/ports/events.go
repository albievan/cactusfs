package ports

import (
	"context"
	"time"
)

// Event types. The contract is api/asyncapi/events.yaml: the names, and the
// payloads below, change only together with it.
const (
	EventBucketCreated   = "cactusfs.bucket.created.v1"
	EventBucketDeleted   = "cactusfs.bucket.deleted.v1"
	EventObjectCreated   = "cactusfs.object.created.v1"
	EventObjectDeleted   = "cactusfs.object.deleted.v1"
	EventRootKeySet      = "cactusfs.iam.root-credential-set.v1"
	EventTopologyChanged = "cactusfs.cluster.topology-changed.v1"
	EventNodeRegistered  = "cactusfs.cluster.node-registered.v1"
	EventNodeRemoved     = "cactusfs.cluster.node-removed.v1"
	EventAuditRecorded   = "cactusfs.audit.recorded.v1"
	// EventBucketVersioningChanged tells that a bucket began or stopped
	// keeping versions.
	EventBucketVersioningChanged = "cactusfs.bucket.versioning-changed.v1"
	// EventObjectDeleteMarkerCreated tells that an object in a bucket that
	// keeps versions was deleted: a delete marker is now its newest
	// version.
	EventObjectDeleteMarkerCreated = "cactusfs.object.delete-marker-created.v1"
)

// Events of Object Lock: the retention or the legal hold of a version was
// changed. The payload is an ObjectEvent that names the version.
const (
	EventObjectRetentionChanged = "cactusfs.object.retention-changed.v1"
	EventObjectLegalHoldChanged = "cactusfs.object.legal-hold-changed.v1"
)

// EventBucketLockChanged tells that the default retention of a bucket was
// set, changed or removed. The payload is a BucketEvent.
const EventBucketLockChanged = "cactusfs.bucket.object-lock-changed.v1"

// EventBucketConfigChanged tells that the policy, the CORS rules or the tags
// of a bucket were set, changed or removed.
const EventBucketConfigChanged = "cactusfs.bucket.configuration-changed.v1"

// EventObjectTagsChanged tells that the tags of an object version changed.
const EventObjectTagsChanged = "cactusfs.object.tags-changed.v1"

// Events of quotas, targets and replication (ADR-0023 to ADR-0026). Like
// every event they are derived from applied entries; a write that a quota
// refused changed nothing and is no event.
const (
	// EventBucketQuotaChanged tells that the quota of a bucket was set,
	// changed or removed. The payload is a BucketQuotaEvent.
	EventBucketQuotaChanged = "cactusfs.bucket.quota-changed.v1"
	// EventBucketReplicaModeChanged tells that a bucket became the replica
	// of another deployment's bucket, or was promoted. The payload is a
	// BucketReplicaModeEvent.
	EventBucketReplicaModeChanged = "cactusfs.bucket.replica-mode-changed.v1"
	// EventTargetChanged tells that a target the deployment sends to was
	// defined or changed. The payload is a TargetEvent.
	EventTargetChanged = "cactusfs.cluster.target-changed.v1"
	// EventTargetDeleted tells that a target was deleted. The payload is a
	// TargetEvent.
	EventTargetDeleted = "cactusfs.cluster.target-deleted.v1"
)

// The messages that are sent to targets outside the deployment and nowhere
// else: at least once, in order, without the envelope of an Event (ADR-0025).
// Their bodies are defined by api/asyncapi/events.yaml.
const (
	// MessageNotificationObject is a bucket notification: an object was
	// created or removed, told as an S3 event notification of one record.
	MessageNotificationObject = "cactusfs.notification.object.v1"
	// MessageAuditExported is one record of the audit trail, whole.
	MessageAuditExported = "cactusfs.audit.exported.v1"
)

// BucketQuotaEvent is the payload of EventBucketQuotaChanged.
type BucketQuotaEvent struct {
	Bucket string `json:"bucket"`
	// QuotaBytes is the most bytes the bucket may hold from now on; 0 when
	// the quota was removed.
	QuotaBytes int64 `json:"quota_bytes"`
}

// BucketReplicaModeEvent is the payload of EventBucketReplicaModeChanged.
type BucketReplicaModeEvent struct {
	Bucket string `json:"bucket"`
	// Source is the ID of the deployment the bucket now takes its versions
	// from; empty when it was promoted.
	Source string `json:"source"`
}

// TargetEvent is the payload of EventTargetChanged and EventTargetDeleted.
// It names the target and never carries its endpoint or anything of its
// credentials.
type TargetEvent struct {
	Name string `json:"name"`
	// Kind is "webhook", "nats" or "s3".
	Kind string `json:"kind,omitempty"`
}

// BucketVersioningEvent is the payload of EventBucketVersioningChanged.
type BucketVersioningEvent struct {
	Bucket string `json:"bucket"`
	// Status is "Enabled" or "Suspended".
	Status string `json:"status"`
}

// Event is a fact: a change of state that was committed through Raft. Events
// are derived from applied entries, so every node derives the same events
// with the same IDs, and a consumer that sees one twice can tell.
type Event struct {
	// ID is unique within a deployment and the same on every node.
	ID string `json:"id"`
	// Type is one of the Event constants.
	Type string `json:"type"`
	// Sequence is the index of the Raft entry the event was derived from.
	// Events are published in ascending order of it.
	Sequence uint64 `json:"sequence"`
	// Time is when the change was made, as recorded in the change itself.
	// It is the zero time when the change carries none.
	Time time.Time `json:"time,omitzero"`
	// Data is the payload: the struct of this package that belongs to Type.
	Data any `json:"data"`
}

// BucketEvent is the payload of EventBucketCreated and EventBucketDeleted.
type BucketEvent struct {
	Bucket string `json:"bucket"`
}

// ObjectEvent is the payload of EventObjectCreated and EventObjectDeleted.
// It describes the version that became, or stopped being, current.
type ObjectEvent struct {
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	VersionID string `json:"version_id"`
	Size      int64  `json:"size"`
	ETag      string `json:"etag"`
}

// RootCredentialEvent is the payload of EventRootKeySet. It never
// carries the secret.
type RootCredentialEvent struct {
	AccessKeyID string `json:"access_key_id"`
}

// TopologyEvent is the payload of EventTopologyChanged.
type TopologyEvent struct {
	DeploymentID string `json:"deployment_id"`
	Version      uint64 `json:"version"`
	ErasureSets  int    `json:"erasure_sets"`
	DrivesPerSet int    `json:"drives_per_set"`
}

// NodeEvent is the payload of EventNodeRegistered and EventNodeRemoved.
type NodeEvent struct {
	NodeID string   `json:"node_id"`
	Roles  []string `json:"roles,omitempty"`
}

// AuditEvent is the payload of EventAuditRecorded: that a record was added
// to the audit trail, and enough of it to follow the chain. The record
// itself is read from the audit log.
type AuditEvent struct {
	Sequence uint64 `json:"sequence"`
	Actor    string `json:"actor"`
	Action   string `json:"action"`
	Resource string `json:"resource,omitempty"`
	Outcome  string `json:"outcome"`
	Hash     string `json:"hash"`
}

// EventPublisher delivers events to whoever consumes them. Publish must not
// block for long and must not fail the change the event describes: that
// change is committed already.
type EventPublisher interface {
	Publish(ctx context.Context, e Event) error
}
