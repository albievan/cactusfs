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
	// wrapping domain.ErrAccessDenied when it is not.
	Authorize(ctx context.Context, req iam.Request) error
}
