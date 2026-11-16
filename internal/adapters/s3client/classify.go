package s3client

import (
	"context"
	"errors"
	"net/http"

	"github.com/albievan/cactusfs/internal/domain/delivery"
)

// The S3 error codes the classes are told by. A code the list does not name
// is judged by its status alone.
const (
	codeQuotaExceeded      = "QuotaExceeded"
	codeNoSuchBucket       = "NoSuchBucket"
	codeNoSuchKey          = "NoSuchKey"
	codeNoSuchVersion      = "NoSuchVersion"
	codeNoSuchUpload       = "NoSuchUpload"
	codeInvalidBucketState = "InvalidBucketState"
	codeOperationAborted   = "OperationAborted"
	codeInvalidArgument    = "InvalidArgument"
	// codeObjectLocked is the other deployment's refusal, by Object Lock,
	// of a request of a source (ADR-0026): with the status 403 of any
	// refusal by Object Lock, and a code that is not the AccessDenied of
	// a refused key.
	codeObjectLocked = "ObjectLocked"
	// codeReplicaHeaderNotTaken is the other deployment's refusal of a
	// request that does not take the replica headers it carries
	// (ADR-0026): of the request as this release writes it, never of the
	// version, whatever the call.
	codeReplicaHeaderNotTaken = "ReplicaHeaderNotTaken"
)

// call kinds: what a request carries, which decides what some refusals say.
type callKind int

const (
	// kindCopy is a request that carries a version, or a probe: a put,
	// an upload and its parts, a completion, a marker, a check.
	kindCopy callKind = iota
	// kindChange is a later change of a version: its tags, its legal
	// hold.
	kindChange
	// kindRetention is a later change of the retention of a version.
	kindRetention
)

// authCodes are the codes of a refused credential that come with a status
// other than 401 and 403: the signature, not the version, is what the other
// deployment refuses.
func authCodes() map[string]bool {
	return map[string]bool{
		"AuthorizationHeaderMalformed":      true,
		"AuthorizationQueryParametersError": true,
		"InvalidAccessKeyId":                true,
		"SignatureDoesNotMatch":             true,
		"RequestTimeTooSkewed":              true,
		"ExpiredToken":                      true,
		"InvalidToken":                      true,
	}
}

// versionCodes are the codes that say the version itself cannot be kept
// there, however often it is sent: too large, a key too long, tags or
// metadata that the other deployment does not take.
func versionCodes() map[string]bool {
	return map[string]bool{
		"EntityTooLarge":   true,
		"KeyTooLongError":  true,
		"InvalidTag":       true,
		"MetadataTooLarge": true,
	}
}

// classOf returns the class of an answer of the other deployment to a call
// of kind, by its status and its S3 error code ("" when the answer has
// none: a HEAD, or a body that is no error document). A 2xx is no failure.
//
// ClassRejected - the version is passed as FAILED and never sent again - is
// given only to what is known to be of the version itself: a 413, the codes
// of versionCodes, a key or a version that is not there for a later change
// of it, a later change that Object Lock there refuses (ObjectLocked), and
// an InvalidArgument of a retention, which the other deployment judges by
// its own clock (ADR-0026, "Open", 7). Every other refusal - of
// the credential, the bucket, the deployment, the request as this release
// writes it, a fault or a load of the other side - holds the target and is
// tried again: holding loses nothing, and passing would mark a version
// FAILED for a fault that is not its own. The other cases ADR-0026 answers
// with InvalidArgument (a sequence out of range, a marker without an ID, a
// retention that has passed) are refused by the client before it sends, so
// an InvalidArgument of a copy is one of the request and not of the
// version.
func classOf(status int, code string, kind callKind) delivery.Class {
	switch {
	case status >= 200 && status <= 299:
		return delivery.ClassNone
	case status >= 300 && status <= 399:
		return delivery.ClassRedirect
	case code == codeReplicaHeaderNotTaken:
		// A deployment that does not take the replica headers on this
		// request: held, and never the version's, so that no change is
		// FAILED for what the pair of releases does not agree on.
		return delivery.ClassServer
	case code == codeQuotaExceeded:
		return delivery.ClassQuota
	case code == codeNoSuchBucket:
		return delivery.ClassNoSuchBucket
	case code == codeInvalidBucketState:
		return delivery.ClassNotReplica
	case code == codeObjectLocked && kind != kindCopy:
		// A later change that Object Lock there does not allow - a
		// retention shortened, say: it is never made, however often it
		// is sent, and nothing else of the target is wrong.
		return delivery.ClassRejected
	case code == codeObjectLocked:
		// A copy is never refused by Object Lock; should one be, it is
		// held and asked again rather than passed or taken for the key.
		return delivery.ClassServer
	case status == http.StatusUnauthorized || status == http.StatusForbidden || authCodes()[code]:
		return delivery.ClassAuth
	case status == http.StatusNotImplemented:
		// A deployment that does not know what is asked of it: one of a
		// release before replica mode, or another S3 service.
		return delivery.ClassNotReplica
	case status == http.StatusRequestEntityTooLarge || (status >= 400 && status <= 499 && versionCodes()[code]):
		return delivery.ClassRejected
	case kind != kindCopy && (code == codeNoSuchKey || code == codeNoSuchVersion):
		return delivery.ClassRejected
	case kind == kindRetention && status == http.StatusBadRequest && code == codeInvalidArgument:
		return delivery.ClassRejected
	}
	// Any other 4xx, a 5xx, and anything that is no status at all.
	return delivery.ClassServer
}

// localClass returns the class of a failure before anything was sent: a
// deadline that passed is ClassTimeout - the attempt ran out of time, as it
// does while a target is slow - and anything else, a cancelled context among
// it, is this side's: ClassInternal.
func localClass(err error) delivery.Class {
	if errors.Is(err, context.DeadlineExceeded) {
		return delivery.ClassTimeout
	}
	return delivery.ClassInternal
}
