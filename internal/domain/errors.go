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
	// ErrShardNotFound reports that a shard is missing from a drive.
	ErrShardNotFound = errors.New("shard not found")
	// ErrKeyUnavailable reports that the KMS cannot unwrap a data key, for
	// example because the wrong master key is configured.
	ErrKeyUnavailable = errors.New("encryption key is unavailable")
	// ErrUnsupportedFormat reports on-disk data or a metadata entry written
	// in a format version this binary does not understand.
	ErrUnsupportedFormat = errors.New("unsupported format version")
)
