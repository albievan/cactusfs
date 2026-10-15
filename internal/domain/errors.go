// Package domain is the root of the CactusFS domain model. It holds the typed
// sentinel errors shared by every aggregate. Inbound adapters, and only they,
// map these errors to S3 XML error codes or problem+json responses.
//
// Nothing under internal/domain imports anything outside the standard library.
package domain

import (
	"errors"
	"fmt"
)

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
	// ErrInvalidTag reports tags that cannot be kept: too many, too long,
	// given twice, or of characters a tag may not have. It is an
	// ErrInvalidArgument.
	ErrInvalidTag = fmt.Errorf("%w: invalid tag", ErrInvalidArgument)
	// ErrTagRule reports tags that a rule of the bucket does not allow: a
	// required tag is missing, or a tag is not one the bucket takes. It is
	// an ErrInvalidTag.
	ErrTagRule = fmt.Errorf("%w: against the bucket's tag rules", ErrInvalidTag)
	// ErrAccessDenied reports that the authorizer refused the request.
	ErrAccessDenied = errors.New("access denied")
	// ErrCredentialNotFound reports an access key ID that does not exist or
	// is no longer valid - or a console session that has ended, where the
	// caller is to be told that and not that a password was wrong.
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
	// ErrSubBucketNotFound reports that the bucket has no sub-bucket of
	// that name.
	ErrSubBucketNotFound = errors.New("sub-bucket not found")
	// ErrTooManySubBuckets reports an attempt to give a bucket more
	// sub-buckets than a bucket can have.
	ErrTooManySubBuckets = errors.New("the bucket has as many sub-buckets as a bucket can have")
	// ErrSubBucketNotEmpty reports an attempt to delete a sub-bucket that
	// still holds an object, a version of one, a delete marker or an
	// upload in progress.
	ErrSubBucketNotEmpty = errors.New("sub-bucket is not empty")
	// ErrQuotaExceeded reports a write that would take a bucket past its
	// quota (ADR-0024). Nothing was stored. It is a limit and no denial:
	// whoever may write the bucket is told so.
	ErrQuotaExceeded = errors.New("the bucket quota has been reached")
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
	// ErrUploadNotFound means that there is no multipart upload with the
	// ID that was named: it never was, or it was completed or aborted.
	ErrUploadNotFound = errors.New("no such multipart upload")
	// ErrPartNotFound means that a completion lists a part that was not
	// uploaded, or with another entity tag than the stored part has.
	ErrPartNotFound = errors.New("part not found, or its entity tag does not match")
	// ErrPartOrder means that a completion does not list its parts in
	// ascending order of number.
	ErrPartOrder = errors.New("parts are not listed in ascending order")
	// ErrPartTooSmall means that a part other than the last is smaller
	// than the minimum.
	ErrPartTooSmall = errors.New("part is smaller than the minimum")
	// ErrCustomerKeyRequired means that an object encrypted with a key the
	// client holds (SSE-C) was asked for without that key.
	ErrCustomerKeyRequired = errors.New("the object is encrypted with a customer key, which the request must provide")
	// ErrCustomerKeyMismatch means that the customer key of the request is
	// not the one the object was encrypted with.
	ErrCustomerKeyMismatch = errors.New("the customer key is not the one the object was encrypted with")
	// ErrCustomerKeyUnexpected means that a customer key came with a
	// request for an object that is not encrypted with one.
	ErrCustomerKeyUnexpected = errors.New("the object is not encrypted with a customer key")
	// ErrPreconditionFailed means that a version is not what a condition
	// of the request says it must be.
	ErrPreconditionFailed = errors.New("a precondition of the request does not hold")
	// ErrNotModified means that the caller already has the version it
	// asked for, by the conditions it sent. It is an answer, not a fault.
	ErrNotModified = errors.New("not modified")
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
	// ErrSessionLocked means that a console session has reached its timeout
	// and is kept only because its user is uploading objects (ADR-0027):
	// it may store objects, and nothing else, until the user's password
	// unlocks it. It is no ErrAccessDenied: nothing a policy says is behind
	// it, and whoever is told it must not take it for "not allowed" - or
	// for "not there", or for an empty list.
	ErrSessionLocked = errors.New("the session is locked")
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
	// ErrTooManyRequests means that the node is already doing as much of
	// this kind of work as it does at one time - for this caller, or for
	// everybody. Nothing is wrong with the request; it can be repeated in
	// a moment.
	ErrTooManyRequests = errors.New("too many requests at once")
	// ErrTooManyBuckets means that the deployment has more buckets than a
	// request that must decide each of them gets through in the time it
	// has. It comes together with ErrUnavailable, and differs from it:
	// repeating the request helps only as far as the node remembers work
	// it has done for the request before; past that, it is a limit.
	ErrTooManyBuckets = errors.New("too many buckets to decide in time")
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
	// ErrNotSupported means that a node was asked something its release
	// cannot answer: during a rolling upgrade the nodes are not all of one
	// release, and a newer one may ask what an older one has never heard
	// of. It is the judgement of the node that asked, made from how the
	// call was refused; no node reports it of itself, so it has no wire
	// code.
	ErrNotSupported = errors.New("not supported by the release of the node that was asked")
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
		{"entity_too_large", ErrEntityTooLarge},
		{"customer_key_required", ErrCustomerKeyRequired},
		{"customer_key_mismatch", ErrCustomerKeyMismatch},
		{"customer_key_unexpected", ErrCustomerKeyUnexpected},
		{"precondition_failed", ErrPreconditionFailed},
		{"not_modified", ErrNotModified},
		{"upload_not_found", ErrUploadNotFound},
		{"part_not_found", ErrPartNotFound},
		{"part_order", ErrPartOrder},
		{"part_too_small", ErrPartTooSmall},
		{"lock_not_configured", ErrLockNotConfigured},
		{"sub_bucket_not_found", ErrSubBucketNotFound},
		{"too_many_sub_buckets", ErrTooManySubBuckets},
		{"sub_bucket_not_empty", ErrSubBucketNotEmpty},
		{"too_many_requests", ErrTooManyRequests},
		{"too_many_buckets", ErrTooManyBuckets},
		{"quota_exceeded", ErrQuotaExceeded},
	}
}

// Sentinels returns every sentinel that has a wire name, in the order of
// their names' first release. It is for the tests of the inbound adapters:
// each of the two holds its mapping against this list, so that a sentinel
// that is added is given an answer in both APIs by decision and not by
// default.
func Sentinels() []error {
	entries := wireCodes()
	sentinels := make([]error, len(entries))
	for i, entry := range entries {
		sentinels[i] = entry.err
	}
	return sentinels
}

// LocalSentinels returns every sentinel that has no wire name: one that is
// made and answered on the node that met it, and that no node reports to
// another. With Sentinels it is every sentinel there is - a test of this
// package holds the two lists against the declarations - so that the tests
// of the inbound adapters can ask what a caller is told of each, whichever
// kind it is.
func LocalSentinels() []error {
	return []error{
		ErrBadDigest,
		ErrIncompleteBody,
		ErrInvalidRange,
		ErrKeyUnavailable,
		ErrInvalidTag,
		ErrTagRule,
		ErrIdempotencyKeyReused,
		ErrRequestInProgress,
		ErrTooManyAttempts,
		ErrNotSupported,
		ErrSessionLocked,
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
