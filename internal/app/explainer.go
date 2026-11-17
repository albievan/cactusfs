package app

import (
	"context"
	"fmt"

	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// NewExplainer returns the ports.AccessExplainer of an authorizer: its
// decision and why, from its own decide and its own reading of the
// policies of the bucket and the sub-bucket, with nothing logged or
// audited (ADR-0032, Decision 3). The Authorizer itself is no
// AccessExplainer - it has no method Explain - so nothing that holds it,
// or holds a ports.Authorizer, can come by one by a type assertion; this is
// the only way to make one. TestOnlyTheExplainUseCaseHoldsAnExplainer
// (cmd/cactusfs) lets this function, the port and its method Explain be
// named, in internal/app, internal/adapters and cmd/cactusfs, only by the
// explanation use case (ExplainService, NewExplainService,
// ExplainServiceConfig), by the one place of cmd/cactusfs that wires it,
// and in this file.
func NewExplainer(a *Authorizer) ports.AccessExplainer {
	return explainer{authz: a}
}

// explainer is the AccessExplainer of an Authorizer.
type explainer struct {
	authz *Authorizer
}

// Explain implements ports.AccessExplainer: the decision of Authorize and
// why it was made, within the request scope of ctx when it has one.
// Nothing is logged or audited - not a refusal, not that a policy cannot be
// read, not the refusal of a locked session.
//
// The error says that no decision could be made, and then the explanation
// is empty: a policy could not be read, and for a stored policy that is
// damaged it wraps domain.ErrIntegrity. Its text is the store's or the
// parser's, which may quote the document: it is for the node's log at
// most, never for a caller.
func (e explainer) Explain(ctx context.Context, req iam.Request) (iam.Explanation, error) {
	var explanation iam.Explanation
	if _, _, _, err := e.authz.decide(ctx, req, &explanation); err != nil {
		return iam.Explanation{}, fmt.Errorf("explain: %w", err)
	}
	return explanation, nil
}
