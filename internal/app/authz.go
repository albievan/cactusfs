package app

import (
	"context"
	"log/slog"

	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// Authorizer is the single authorization decision point. It applies the
// domain's default-deny rule and records every refusal.
//
// Until the hash-chained audit log exists (PLAN 4.6) a refusal is written to
// the log; the entry names the principal, the action and the resource and
// never contains a secret.
type Authorizer struct {
	logger *slog.Logger
}

var _ ports.Authorizer = (*Authorizer)(nil)

// NewAuthorizer returns an Authorizer that logs refusals to logger.
func NewAuthorizer(logger *slog.Logger) *Authorizer {
	return &Authorizer{logger: logger}
}

// Authorize implements ports.Authorizer.
func (a *Authorizer) Authorize(ctx context.Context, req iam.Request) error {
	err := iam.Decide(req)
	if err != nil {
		a.logger.WarnContext(ctx, "authorization denied",
			slog.String("principal", req.Principal.Name()),
			slog.String("action", string(req.Action)),
			slog.String("resource", req.Resource.String()),
		)
	}
	return err
}
