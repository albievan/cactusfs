package ports

import (
	"context"
	"time"

	"github.com/albievan/cactusfs/internal/domain/iam"
)

// Clock tells the time. Use cases never call time.Now directly.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
}

// Authorizer is the single decision point every inbound request passes
// through after authentication. The rule is default deny.
type Authorizer interface {
	// Authorize returns nil when the request is allowed and an error
	// wrapping domain.ErrAccessDenied when it is not - or wrapping
	// domain.ErrSessionLocked, and not ErrAccessDenied, when it is refused
	// because the caller's console session is locked.
	Authorize(ctx context.Context, req iam.Request) error
	// Allowed reports whether Authorize would allow the request: the same
	// decision, from the same reading of the policies, but a refusal is
	// neither written to the audit trail nor logged. It exists only to
	// filter a set of results down to what the caller may see - a search
	// over a hundred buckets must not write a hundred denials - and is
	// never to be used as the gate of an operation: whatever a caller asks
	// for by name goes through Authorize. The error is not nil only when
	// the decision could not be made, and then nothing is allowed - and
	// for a caller whose console session is locked, when it wraps
	// domain.ErrSessionLocked: such a caller is never answered "no", which
	// would leave a list empty where it is to be refused. That refusal
	// ends the operation that asked, and is recorded as one of Authorize.
	Allowed(ctx context.Context, req iam.Request) (bool, error)
}
