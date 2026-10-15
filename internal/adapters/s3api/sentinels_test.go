package s3api

import (
	"fmt"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
)

// TestEverySentinelHasAnAnswer holds the mapping of this adapter against
// every sentinel of the domain: each has a row here, and is answered as its
// row says. A sentinel that is added to the domain fails this test - and the
// one of the other inbound adapter - until somebody has decided what a
// caller is told. InternalError is a decision too: it is what a caller is
// told of something that is none of their business, or that this API never
// meets.
//
// The first table holds the sentinels that have a wire name
// (domain.Sentinels): those that cross a node boundary, so that a use case
// may return one that another node made. The second holds those without one
// (domain.LocalSentinels), which are made on the node that answers. The
// domain's own test holds the two lists against the declarations, so that a
// new sentinel of either kind is in one of them.
func TestEverySentinelHasAnAnswer(t *testing.T) {
	t.Parallel()
	type answer struct {
		status int
		code   string
	}
	answers := map[error]answer{
		domain.ErrShuttingDown:          {503, "ServiceUnavailable"},
		domain.ErrNotLeader:             {503, "ServiceUnavailable"},
		domain.ErrOutcomeUnknown:        {500, "InternalError"},
		domain.ErrNoTopology:            {500, "InternalError"},
		domain.ErrConflict:              {409, "OperationAborted"},
		domain.ErrNotBootstrapped:       {500, "InternalError"},
		domain.ErrAlreadyBootstrapped:   {500, "InternalError"},
		domain.ErrInvalidArgument:       {400, "InvalidArgument"},
		domain.ErrAccessDenied:          {403, "AccessDenied"},
		domain.ErrInvalidBucketName:     {400, "InvalidBucketName"},
		domain.ErrBucketNotFound:        {404, "NoSuchBucket"},
		domain.ErrBucketAlreadyExists:   {409, "BucketAlreadyOwnedByYou"},
		domain.ErrBucketNotEmpty:        {409, "BucketNotEmpty"},
		domain.ErrInvalidObjectKey:      {400, "KeyTooLongError"},
		domain.ErrObjectNotFound:        {404, "NoSuchKey"},
		domain.ErrInvalidID:             {500, "InternalError"},
		domain.ErrIntegrity:             {500, "InternalError"},
		domain.ErrInsufficientDrives:    {503, "ServiceUnavailable"},
		domain.ErrShardNotFound:         {500, "InternalError"},
		domain.ErrUnsupportedFormat:     {500, "InternalError"},
		domain.ErrUnavailable:           {503, "ServiceUnavailable"},
		domain.ErrDriveOffline:          {503, "ServiceUnavailable"},
		domain.ErrEntityNotFound:        {500, "InternalError"},
		domain.ErrEntityExists:          {500, "InternalError"},
		domain.ErrEntityInUse:           {500, "InternalError"},
		domain.ErrCredentialNotFound:    {403, "InvalidAccessKeyId"},
		domain.ErrVersionNotFound:       {404, "NoSuchVersion"},
		domain.ErrMethodNotAllowed:      {405, "MethodNotAllowed"},
		domain.ErrInvalidBucketState:    {409, "InvalidBucketState"},
		domain.ErrObjectLocked:          {403, "AccessDenied"},
		domain.ErrEntityTooLarge:        {400, "EntityTooLarge"},
		domain.ErrCustomerKeyRequired:   {400, "InvalidRequest"},
		domain.ErrCustomerKeyMismatch:   {403, "AccessDenied"},
		domain.ErrCustomerKeyUnexpected: {400, "InvalidRequest"},
		domain.ErrPreconditionFailed:    {412, "PreconditionFailed"},
		domain.ErrNotModified:           {500, "InternalError"},
		domain.ErrUploadNotFound:        {404, "NoSuchUpload"},
		domain.ErrPartNotFound:          {400, "InvalidPart"},
		domain.ErrPartOrder:             {400, "InvalidPartOrder"},
		domain.ErrPartTooSmall:          {400, "EntityTooSmall"},
		domain.ErrLockNotConfigured:     {400, "InvalidRequest"},
		domain.ErrSubBucketNotFound:     {500, "InternalError"},
		domain.ErrTooManySubBuckets:     {500, "InternalError"},
		domain.ErrSubBucketNotEmpty:     {500, "InternalError"},
		domain.ErrTooManyRequests:       {500, "InternalError"},
		domain.ErrTooManyBuckets:        {500, "InternalError"},
		domain.ErrQuotaExceeded:         {403, "QuotaExceeded"},
	}
	// The sentinels without a wire name, as they are answered today.
	local := map[error]answer{
		domain.ErrBadDigest:            {400, "BadDigest"},
		domain.ErrIncompleteBody:       {400, "IncompleteBody"},
		domain.ErrInvalidRange:         {416, "InvalidRange"},
		domain.ErrKeyUnavailable:       {500, "InternalError"},
		domain.ErrInvalidTag:           {400, "InvalidTag"},
		domain.ErrTagRule:              {400, "InvalidTag"},
		domain.ErrIdempotencyKeyReused: {500, "InternalError"},
		domain.ErrRequestInProgress:    {500, "InternalError"},
		domain.ErrTooManyAttempts:      {500, "InternalError"},
		domain.ErrNotSupported:         {500, "InternalError"},
		domain.ErrSessionLocked:        {403, "AccessDenied"},
	}
	sentinels := domain.Sentinels()
	if len(sentinels) != len(answers) {
		t.Errorf("%d sentinels with a wire name, %d answers", len(sentinels), len(answers))
	}
	localSentinels := domain.LocalSentinels()
	if len(localSentinels) != len(local) {
		t.Errorf("%d sentinels without a wire name, %d answers", len(localSentinels), len(local))
	}
	for sentinel, answer := range local {
		if _, twice := answers[sentinel]; twice {
			t.Errorf("%v is in both tables", sentinel)
		}
		answers[sentinel] = answer
	}
	for _, sentinel := range append(sentinels, localSentinels...) {
		want, decided := answers[sentinel]
		if !decided {
			t.Errorf("%v (%v) has no answer in the S3 API: decide one, here and in the mapping", sentinel, domain.Codes(sentinel))
			continue
		}
		// As a use case returns it: wrapped with context.
		got := toAPIError(fmt.Errorf("object.Put: %w", sentinel))
		if got.status != want.status || got.code != want.code {
			t.Errorf("toAPIError(%v) = %d %s, want %d %s", sentinel, got.status, got.code, want.status, want.code)
		}
	}

	// The refusal of a quota, in so many words (ADR-0024): a 403, so that
	// an SDK tells its caller once and does not try again, with a code of
	// its own, so that it is told apart from a denial - also when both are
	// wrapped by one error.
	quota := toAPIError(fmt.Errorf("object.Put: %w: bucket photos", domain.ErrQuotaExceeded))
	if quota.status != 403 || quota.code != "QuotaExceeded" || quota.message != "The bucket quota has been reached." {
		t.Errorf("the refusal of a quota = %+v", quota)
	}
	if denied := toAPIError(fmt.Errorf("%w: %w", domain.ErrAccessDenied, domain.ErrQuotaExceeded)); denied.code != "AccessDenied" {
		t.Errorf("a denial that also names the quota is told as %s", denied.code)
	}
}
