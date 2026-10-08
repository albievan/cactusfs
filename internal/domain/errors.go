// Package domain is the root of the CactusFS domain model. It holds the typed
// sentinel errors shared by every aggregate. Inbound adapters, and only they,
// map these errors to S3 XML error codes or problem+json responses.
//
// Nothing under internal/domain imports anything outside the standard library.
package domain

import "errors"

// Lifecycle errors.
var (
	// ErrShuttingDown reports that the node has begun a graceful shutdown and
	// no longer accepts new work.
	ErrShuttingDown = errors.New("node is shutting down")
	// ErrNotLeader reports that this node cannot commit metadata changes
	// because it is not the Raft leader.
	ErrNotLeader = errors.New("node is not the metadata leader")
	// ErrOutcomeUnknown reports that a metadata change may or may not have
	// been committed: it reached the Raft log but its result was not
	// observed, so it can still take effect, for example after a restart.
	// Nothing that the change refers to may be cleaned up on this error.
	ErrOutcomeUnknown = errors.New("outcome of the metadata change is unknown")
	// ErrNoTopology reports that the deployment's drives have not been
	// assigned to erasure sets yet.
	ErrNoTopology = errors.New("deployment has no drive topology")
	// ErrConflict reports that a change was based on a state that has since
	// changed.
	ErrConflict = errors.New("state has changed")
	// ErrNotBootstrapped reports that the deployment has no root credentials
	// yet.
	ErrNotBootstrapped = errors.New("deployment is not bootstrapped")
	// ErrAlreadyBootstrapped reports an attempt to bootstrap a deployment a
	// second time.
	ErrAlreadyBootstrapped = errors.New("deployment is already bootstrapped")
)

// Request errors.
var (
	// ErrInvalidArgument reports a request parameter that is malformed or out
	// of range.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrAccessDenied reports that the authorizer refused the request.
	ErrAccessDenied = errors.New("access denied")
	// ErrCredentialNotFound reports an access key ID that does not exist or
	// is no longer valid.
	ErrCredentialNotFound = errors.New("credential not found")
	// ErrBadDigest reports that the payload did not match a digest the client
	// declared for it.
	ErrBadDigest = errors.New("payload does not match the declared digest")
	// ErrIncompleteBody reports that the payload ended before the declared
	// length.
	ErrIncompleteBody = errors.New("payload is shorter than declared")
	// ErrEntityTooLarge reports a payload larger than the maximum object size.
	ErrEntityTooLarge = errors.New("payload exceeds the maximum object size")
)

// Bucket errors.
var (
	// ErrInvalidBucketName reports a bucket name that breaks the naming rules.
	ErrInvalidBucketName = errors.New("invalid bucket name")
	// ErrBucketNotFound reports that the bucket does not exist.
	ErrBucketNotFound = errors.New("bucket not found")
	// ErrBucketAlreadyExists reports that a bucket of that name already
	// exists.
	ErrBucketAlreadyExists = errors.New("bucket already exists")
	// ErrInvalidBucketState means that the state of the bucket does not
	// allow the request.
	ErrInvalidBucketState = errors.New("the state of the bucket does not allow this")
	// ErrBucketNotEmpty reports an attempt to delete a bucket that still
	// holds objects.
	ErrBucketNotEmpty = errors.New("bucket is not empty")
)

// Object errors.
var (
	// ErrInvalidObjectKey reports an object key that breaks the key rules.
	ErrInvalidObjectKey = errors.New("invalid object key")
	// ErrObjectNotFound reports that the object does not exist.
	ErrObjectNotFound = errors.New("object not found")
	// ErrVersionNotFound means that an object has no version with the ID
	// that was named.
	ErrVersionNotFound = errors.New("version not found")
	// ErrObjectLocked means that a version is kept by Object Lock and may
	// not be removed, or its retention changed, as was asked.
	ErrObjectLocked = errors.New("the object version is locked")
	// ErrLockNotConfigured means that a bucket was not created with Object
	// Lock, or that a version has no retention to report.
	ErrLockNotConfigured = errors.New("object lock is not configured")
	// ErrMethodNotAllowed means that what was asked cannot be done with
	// what was named: reading a delete marker.
	ErrMethodNotAllowed = errors.New("method not allowed")
	// ErrInvalidRange reports a byte range that cannot be satisfied.
	ErrInvalidRange = errors.New("requested range is not satisfiable")
	// ErrInvalidID reports a malformed identifier.
	ErrInvalidID = errors.New("invalid identifier")
)

// Data-safety errors.
var (
	// ErrIntegrity reports stored bytes that failed checksum or
	// authentication. Such bytes are never returned to a client.
	ErrIntegrity = errors.New("stored data failed integrity verification")
	// ErrInsufficientDrives reports that too few drives are usable to make
	// a write durable on a write quorum.
	ErrInsufficientDrives = errors.New("not enough drives for a write quorum")
	// ErrShardNotFound reports that a shard is missing from a drive.
	ErrShardNotFound = errors.New("shard not found")
	// ErrKeyUnavailable reports that the KMS cannot unwrap a data key, for
	// example because the wrong master key is configured.
	ErrKeyUnavailable = errors.New("encryption key is unavailable")
	// ErrUnsupportedFormat reports on-disk data or a metadata entry written
	// in a format version this binary does not understand.
	ErrUnsupportedFormat = errors.New("unsupported format version")
)

// Errors of identity and access management.
var (
	// ErrEntityNotFound means that a user, group, role, policy, API key or
	// session does not exist.
	ErrEntityNotFound = errors.New("no such entity")
	// ErrEntityExists means that the name is taken.
	ErrEntityExists = errors.New("entity already exists")
	// ErrEntityInUse means that something still refers to what was to be
	// removed: a role a user holds, a policy a role contains.
	ErrEntityInUse = errors.New("entity is in use")
)

// Errors of the management API's request handling.
var (
	// ErrIdempotencyKeyReused means that an idempotency key came with a
	// request other than the one it was first used for.
	ErrIdempotencyKeyReused = errors.New("idempotency key was used for a different request")
	// ErrRequestInProgress means that the request an idempotency key
	// belongs to is still being carried out.
	ErrRequestInProgress = errors.New("the request with this idempotency key is still in progress")
	// ErrTooManyAttempts means that the caller must wait before trying
	// again.
	ErrTooManyAttempts = errors.New("too many attempts")
)

// Errors that describe the state of the cluster.
var (
	// ErrUnavailable means that something the request needs cannot be
	// reached right now: a node is down, the metadata group has no leader.
	// Nothing is wrong with the data; the request can be repeated.
	ErrUnavailable = errors.New("temporarily unavailable")
	// ErrDriveOffline means that a drive could not be reached. It says
	// nothing about what is stored on it.
	ErrDriveOffline = errors.New("drive is offline")
)

// wireCodes names the sentinels that cross a node boundary. The names are
// part of the node RPC contract: never change one, only add.
func wireCodes() []struct {
	code string
	err  error
} {
	return []struct {
		code string
		err  error
	}{
		{"shutting_down", ErrShuttingDown},
		{"not_leader", ErrNotLeader},
		{"outcome_unknown", ErrOutcomeUnknown},
		{"no_topology", ErrNoTopology},
		{"conflict", ErrConflict},
		{"not_bootstrapped", ErrNotBootstrapped},
		{"already_bootstrapped", ErrAlreadyBootstrapped},
		{"invalid_argument", ErrInvalidArgument},
		{"access_denied", ErrAccessDenied},
		{"invalid_bucket_name", ErrInvalidBucketName},
		{"bucket_not_found", ErrBucketNotFound},
		{"bucket_already_exists", ErrBucketAlreadyExists},
		{"bucket_not_empty", ErrBucketNotEmpty},
		{"invalid_object_key", ErrInvalidObjectKey},
		{"object_not_found", ErrObjectNotFound},
		{"invalid_id", ErrInvalidID},
		{"integrity", ErrIntegrity},
		{"insufficient_drives", ErrInsufficientDrives},
		{"shard_not_found", ErrShardNotFound},
		{"unsupported_format", ErrUnsupportedFormat},
		{"unavailable", ErrUnavailable},
		{"drive_offline", ErrDriveOffline},
		{"entity_not_found", ErrEntityNotFound},
		{"entity_exists", ErrEntityExists},
		{"entity_in_use", ErrEntityInUse},
		{"credential_not_found", ErrCredentialNotFound},
		{"version_not_found", ErrVersionNotFound},
		{"method_not_allowed", ErrMethodNotAllowed},
		{"invalid_bucket_state", ErrInvalidBucketState},
		{"object_locked", ErrObjectLocked},
		{"lock_not_configured", ErrLockNotConfigured},
	}
}

// Codes returns the wire names of the sentinels err wraps, for sending an
// error to another node. An error that wraps none has no codes.
func Codes(err error) []string {
	var codes []string
	for _, entry := range wireCodes() {
		if errors.Is(err, entry.err) {
			codes = append(codes, entry.code)
		}
	}
	return codes
}

// remoteError is an error that was received from another node.
type remoteError struct {
	sentinels []error
	message   string
}

func (e *remoteError) Error() string   { return e.message }
func (e *remoteError) Unwrap() []error { return e.sentinels }

// FromCodes rebuilds an error received from another node: it reads as
// message and wraps the sentinels the codes name. Codes this binary does not
// know are ignored, so the error is then an unclassified failure.
func FromCodes(codes []string, message string) error {
	e := &remoteError{message: message}
	for _, code := range codes {
		for _, entry := range wireCodes() {
			if entry.code == code {
				e.sentinels = append(e.sentinels, entry.err)
			}
		}
	}
	return e
}
