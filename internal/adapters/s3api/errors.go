package s3api

import (
	"errors"
	"net/http"

	"github.com/albievan/cactusfs/internal/domain"
)

// apiError is an S3 error response: an HTTP status, an error code from the S3
// API reference and a message that is safe to show to a client.
type apiError struct {
	status  int
	code    string
	message string
}

func errInternal() apiError {
	return apiError{http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again."}
}

func errNotImplemented() apiError {
	return apiError{http.StatusNotImplemented, "NotImplemented", "A header or query you provided implies functionality that is not implemented."}
}

func errMethodNotAllowed() apiError {
	return apiError{http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource."}
}

// mapping pairs an error with its response. The first entry whose error
// matches wins, so more specific errors come first.
type mapping struct {
	err error
	api apiError
}

func errorMappings() []mapping {
	return []mapping{
		// Authentication.
		{errAuthMissing, apiError{http.StatusForbidden, "AccessDenied", "Access Denied."}},
		{errAuthUnsupported, apiError{http.StatusBadRequest, "InvalidRequest", "Only AWS Signature Version 4 with the Authorization header is supported."}},
		{errAuthMissingContent, apiError{http.StatusBadRequest, "InvalidRequest", "Missing required header for this request: x-amz-content-sha256."}},
		{errAuthExpired, apiError{http.StatusForbidden, "AccessDenied", "Request has expired."}},
		{errAuthQuery, apiError{http.StatusBadRequest, "AuthorizationQueryParametersError", "The query-string authentication parameters that you provided are not valid."}},
		{errAuthMalformed, apiError{http.StatusBadRequest, "AuthorizationHeaderMalformed", "The authorization header that you provided is not valid."}},
		{errAuthSkew, apiError{http.StatusForbidden, "RequestTimeTooSkewed", "The difference between the request time and the server's time is too large."}},
		{errAuthSignature, apiError{http.StatusForbidden, "SignatureDoesNotMatch", "The request signature that the server calculated does not match the signature that you provided."}},
		{domain.ErrCredentialNotFound, apiError{http.StatusForbidden, "InvalidAccessKeyId", "The access key ID that you provided does not exist in our records."}},
		{domain.ErrAccessDenied, apiError{http.StatusForbidden, "AccessDenied", "Access Denied."}},

		// Payload.
		{errUnsupportedPayload, errNotImplemented()},
		{errUnsupportedChecksum, apiError{http.StatusBadRequest, "InvalidRequest", "The checksum algorithm you specified is not supported."}},
		{errChunkFraming, apiError{http.StatusBadRequest, "IncompleteBody", "The request body is not valid aws-chunked content."}},
		{errSHA256Mismatch, apiError{http.StatusBadRequest, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed."}},
		{errInvalidDigest, apiError{http.StatusBadRequest, "InvalidDigest", "The Content-MD5 or checksum value that you specified is not valid."}},
		{domain.ErrBadDigest, apiError{http.StatusBadRequest, "BadDigest", "The Content-MD5 or checksum value that you specified did not match what the server received."}},
		{domain.ErrIncompleteBody, apiError{http.StatusBadRequest, "IncompleteBody", "You did not provide the number of bytes specified by the Content-Length HTTP header."}},
		{domain.ErrEntityTooLarge, apiError{http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size."}},

		// Buckets and objects.
		{domain.ErrInvalidBucketName, apiError{http.StatusBadRequest, "InvalidBucketName", "The specified bucket is not valid."}},
		{domain.ErrBucketNotFound, apiError{http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist."}},
		{domain.ErrBucketAlreadyExists, apiError{http.StatusConflict, "BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it."}},
		{domain.ErrBucketNotEmpty, apiError{http.StatusConflict, "BucketNotEmpty", "The bucket that you tried to delete is not empty."}},
		{domain.ErrInvalidObjectKey, apiError{http.StatusBadRequest, "KeyTooLongError", "Your key is too long or not valid."}},
		{domain.ErrObjectNotFound, apiError{http.StatusNotFound, "NoSuchKey", "The specified key does not exist."}},
		{domain.ErrInvalidRange, apiError{http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable."}},
		{domain.ErrInvalidArgument, apiError{http.StatusBadRequest, "InvalidArgument", "Invalid argument."}},

		// Availability.
		{domain.ErrInsufficientDrives, apiError{http.StatusServiceUnavailable, "ServiceUnavailable", "Service is unable to handle request."}},
		{domain.ErrNotLeader, apiError{http.StatusServiceUnavailable, "ServiceUnavailable", "Service is unable to handle request."}},
		{domain.ErrShuttingDown, apiError{http.StatusServiceUnavailable, "ServiceUnavailable", "Service is unable to handle request."}},
		// A node or the metadata leader cannot be reached: nothing is wrong
		// with the data, and the request can be repeated.
		{domain.ErrUnavailable, apiError{http.StatusServiceUnavailable, "ServiceUnavailable", "Service is unable to handle request."}},
		{domain.ErrDriveOffline, apiError{http.StatusServiceUnavailable, "ServiceUnavailable", "Service is unable to handle request."}},
	}
}

// toAPIError maps an error to its S3 response. Anything without a mapping —
// integrity failures, unavailable keys, storage errors — is an InternalError:
// the client is told nothing about the inside of the system, and the caller
// logs the cause.
func toAPIError(err error) apiError {
	var api apiError
	if errors.As(err, &api) {
		return api
	}
	for _, m := range errorMappings() {
		if errors.Is(err, m.err) {
			return m.api
		}
	}
	return errInternal()
}

// Error implements error, so that handlers can return a ready-made response.
func (e apiError) Error() string {
	return e.code + ": " + e.message
}
