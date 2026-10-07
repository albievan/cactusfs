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
)

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

// EventPublisher delivers events to whoever consumes them. Publish must not
// block for long and must not fail the change the event describes: that
// change is committed already.
type EventPublisher interface {
	Publish(ctx context.Context, e Event) error
}
