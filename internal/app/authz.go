package app

import (
	"context"
	"log/slog"

	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// Authorizer is the single authorization decision point. It applies the
// domain's rules - default deny, explicit deny wins, a key never exceeds its
// owner - to the principal a request was authenticated as, and records every
// refusal in the audit trail and in the log. Neither ever contains a secret.
type Authorizer struct {
	logger  *slog.Logger
	clock   ports.Clock
	auditor *Auditor
}

var _ ports.Authorizer = (*Authorizer)(nil)

// NewAuthorizer returns an Authorizer. The clock gives the time that
// conditions on time are tested against. auditor may be nil, in which case
// refusals are logged only.
func NewAuthorizer(logger *slog.Logger, clock ports.Clock, auditor *Auditor) *Authorizer {
	return &Authorizer{logger: logger, clock: clock, auditor: auditor}
}

// Authorize implements ports.Authorizer.
func (a *Authorizer) Authorize(ctx context.Context, req iam.Request) error {
	// The time of the decision is this node's, never the caller's.
	req.Now = a.clock.Now()
	err := iam.Decide(req)
	if err != nil {
		a.logger.WarnContext(ctx, "authorization denied",
			slog.String("principal", req.Principal.Name()),
			slog.String("action", string(req.Action)),
			slog.String("resource", req.Resource.String()),
		)
		if a.auditor != nil {
			a.auditor.Denied(ctx, req, "not allowed by policy")
		}
	}
	return err
}
