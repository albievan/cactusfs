package adminapi

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
		domain.ErrOutcomeUnknown:        {503, "ServiceUnavailable"},
		domain.ErrNoTopology:            {503, "ServiceUnavailable"},
		domain.ErrConflict:              {409, "Conflict"},
		domain.ErrNotBootstrapped:       {500, "InternalError"},
		domain.ErrAlreadyBootstrapped:   {500, "InternalError"},
		domain.ErrInvalidArgument:       {400, "InvalidArgument"},
		domain.ErrAccessDenied:          {403, "AccessDenied"},
		domain.ErrInvalidBucketName:     {400, "InvalidBucketName"},
		domain.ErrBucketNotFound:        {404, "NoSuchBucket"},
		domain.ErrBucketAlreadyExists:   {409, "BucketAlreadyExists"},
		domain.ErrBucketNotEmpty:        {409, "BucketNotEmpty"},
		domain.ErrInvalidObjectKey:      {400, "InvalidObjectKey"},
		domain.ErrObjectNotFound:        {404, "NoSuchObject"},
		domain.ErrInvalidID:             {500, "InternalError"},
		domain.ErrIntegrity:             {500, "InternalError"},
		domain.ErrInsufficientDrives:    {503, "ServiceUnavailable"},
		domain.ErrShardNotFound:         {500, "InternalError"},
		domain.ErrUnsupportedFormat:     {500, "InternalError"},
		domain.ErrUnavailable:           {503, "ServiceUnavailable"},
		domain.ErrDriveOffline:          {503, "ServiceUnavailable"},
		domain.ErrEntityNotFound:        {404, "NoSuchEntity"},
		domain.ErrEntityExists:          {409, "EntityAlreadyExists"},
		domain.ErrEntityInUse:           {409, "EntityInUse"},
		domain.ErrCredentialNotFound:    {401, "Unauthenticated"},
		domain.ErrVersionNotFound:       {404, "NoSuchVersion"},
		domain.ErrMethodNotAllowed:      {405, "MethodNotAllowed"},
		domain.ErrInvalidBucketState:    {500, "InternalError"},
		domain.ErrObjectLocked:          {500, "InternalError"},
		domain.ErrEntityTooLarge:        {413, "EntityTooLarge"},
		domain.ErrCustomerKeyRequired:   {400, "InvalidArgument"},
		domain.ErrCustomerKeyMismatch:   {500, "InternalError"},
		domain.ErrCustomerKeyUnexpected: {500, "InternalError"},
		domain.ErrPreconditionFailed:    {500, "InternalError"},
		domain.ErrNotModified:           {500, "InternalError"},
		domain.ErrUploadNotFound:        {500, "InternalError"},
		domain.ErrPartNotFound:          {500, "InternalError"},
		domain.ErrPartOrder:             {500, "InternalError"},
		domain.ErrPartTooSmall:          {500, "InternalError"},
		domain.ErrLockNotConfigured:     {500, "InternalError"},
		domain.ErrSubBucketNotFound:     {500, "InternalError"},
		domain.ErrTooManySubBuckets:     {500, "InternalError"},
		domain.ErrSubBucketNotEmpty:     {500, "InternalError"},
		domain.ErrTooManyRequests:       {429, "TooManyRequests"},
		domain.ErrTooManyBuckets:        {500, "InternalError"},
		domain.ErrQuotaExceeded:         {403, "QuotaExceeded"},
		domain.ErrNotPreviewable:        {415, "NotPreviewable"},
		domain.ErrPreviewTooLarge:       {413, "PreviewTooLarge"},
	}
	// The sentinels without a wire name, as they are answered today.
	local := map[error]answer{
		domain.ErrBadDigest:            {400, "InvalidArgument"},
		domain.ErrIncompleteBody:       {400, "InvalidArgument"},
		domain.ErrInvalidRange:         {400, "InvalidArgument"},
		domain.ErrKeyUnavailable:       {503, "ServiceUnavailable"},
		domain.ErrInvalidTag:           {400, "InvalidArgument"},
		domain.ErrTagRule:              {400, "InvalidArgument"},
		domain.ErrIdempotencyKeyReused: {422, "IdempotencyKeyReused"},
		domain.ErrRequestInProgress:    {409, "RequestInProgress"},
		domain.ErrTooManyAttempts:      {429, "TooManyAttempts"},
		domain.ErrNotSupported:         {500, "InternalError"},
		domain.ErrSessionLocked:        {423, "SessionLocked"},
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
			t.Errorf("%v (%v) has no answer in the management API: decide one, here and in the mapping", sentinel, domain.Codes(sentinel))
			continue
		}
		// As a use case returns it: wrapped with context.
		got := problemOf(fmt.Errorf("object.Put: %w", sentinel))
		if got.status != want.status || got.code != want.code {
			t.Errorf("problemOf(%v) = %d %s, want %d %s", sentinel, got.status, got.code, want.status, want.code)
		}
	}

	// The refusal of a quota (ADR-0024): a 403 with a code of its own, by
	// which alone it is told apart from a denial.
	quota := problemOf(fmt.Errorf("quota.Admit: %w: bucket photos", domain.ErrQuotaExceeded))
	if quota.status != 403 || quota.code != "QuotaExceeded" || quota.title == "" || quota.detail != "" {
		t.Errorf("the refusal of a quota = %+v", quota)
	}
	if denied := problemOf(fmt.Errorf("%w: %w", domain.ErrQuotaExceeded, domain.ErrAccessDenied)); denied.code != "AccessDenied" {
		t.Errorf("a denial that also names the quota is told as %s", denied.code)
	}

	// The refusal of a locked session (ADR-0027): 423, never the 401 by
	// which a console takes its session for gone, and told as the lock even
	// when the error names a denial as well.
	locked := problemOf(fmt.Errorf("bucket.List: %w: user/alice", domain.ErrSessionLocked))
	if locked.status != 423 || locked.code != "SessionLocked" || locked.title == "" || locked.detail != "" {
		t.Errorf("the refusal of a locked session = %+v", locked)
	}
	if both := problemOf(fmt.Errorf("%w: %w", domain.ErrAccessDenied, domain.ErrSessionLocked)); both.code != "SessionLocked" {
		t.Errorf("a locked session that is also denied is told as %s", both.code)
	}
}
