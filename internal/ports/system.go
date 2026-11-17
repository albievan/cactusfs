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
	// over a hundred buckets must not write a hundred denials - and to
	// choose which action Authorize is then asked for, never instead of
	// it: the parts, the abort and the list of the parts of a multipart
	// upload ask it whether the caller replicates, so as to ask Authorize
	// for s3:ReplicateObject or the ordinary action before the upload is
	// read (app.ObjectService's authorizeUpload, ADR-0026). It is never to
	// be used as the gate of an operation: whatever a caller asks for by
	// name goes through Authorize. The error is not nil only when
	// the decision could not be made, and then nothing is allowed - and
	// for a caller whose console session is locked, when it wraps
	// domain.ErrSessionLocked: such a caller is never answered "no", which
	// would leave a list empty where it is to be refused. That refusal
	// ends the operation that asked, and is recorded as one of Authorize.
	Allowed(ctx context.Context, req iam.Request) (bool, error)
}

// AccessExplainer reports the decision the Authorizer would make and why.
// It is not an Authorizer and is never to be used as the gate of an
// operation: whatever a caller asks for goes through Authorizer.Authorize.
// The explanation use case alone is to hold one (ADR-0032, Decision 3). The
// authorizer of the use cases implements Authorizer and not this, so a
// type assertion of an Authorizer to it fails; the one implementation is
// made by app.NewExplainer. A test of the source of internal/app,
// internal/adapters and cmd/cactusfs (TestOnlyTheExplainUseCaseHoldsAnExplainer)
// refuses this type, app.NewExplainer and a call of Explain anywhere there
// but in the explanation use case, the one place that wires it, and the
// file of app.NewExplainer. Tests are not held to it.
type AccessExplainer interface {
	// Explain reports the decision Authorize would make for req and why -
	// the same decision, from the same reading of the policies of the
	// bucket and of the sub-bucket. Nothing is logged or audited, a
	// refusal included. A refusal is no error: it is the explanation's
	// Err. The error is not nil only when no decision could be made - a
	// policy could not be read, and for a stored policy that is damaged
	// it wraps domain.ErrIntegrity - and then the explanation is empty.
	Explain(ctx context.Context, req iam.Request) (iam.Explanation, error)
}
