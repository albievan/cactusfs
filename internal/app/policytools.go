package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// PolicyToolsService holds the use cases of the policy builder (ADR-0032,
// Decisions 1 and 2): the catalogue of the policy language, and the
// validation of a policy document before it is put. Neither stores
// anything, and validation reads nothing beyond what the authorization of
// the put it stands for reads: it is authorized by the action that would
// store the document, so whoever may save may check and nobody else may,
// and it says nothing a put would not.
type PolicyToolsService struct {
	authz   ports.Authorizer
	auditor *Auditor
}

// NewPolicyToolsService returns a PolicyToolsService. The auditor records
// the refusal of a validation with a key owner to a principal bound by its
// key's inline policy, as CreateAPIKey records it.
func NewPolicyToolsService(authz ports.Authorizer, auditor *Auditor) *PolicyToolsService {
	return &PolicyToolsService{authz: authz, auditor: auditor}
}

// PolicyCatalogue is what a policy can say, as this binary understands it:
// the answer of the node asked, which in a cluster being upgraded may differ
// from another node's.
type PolicyCatalogue struct {
	// Version is the one Version a document may have.
	Version string
	// Actions are every action a policy can grant (iam.Catalogue).
	Actions []iam.CatalogueAction
	// ConditionKeys are the condition keys and their operators.
	ConditionKeys []iam.ConditionKey
	// Resources are the kinds of resource and their templates.
	Resources []iam.ResourceKind
	// Limits are the bounds every parser enforces.
	Limits iam.Limits
	// BucketPolicyPrincipals are the forms of a bucket policy's Principal.
	BucketPolicyPrincipals iam.PrincipalForms
}

// Catalogue returns the catalogue of the policy language. The caller must
// hold admin:GetPolicyCatalogue.
func (s *PolicyToolsService) Catalogue(ctx context.Context, p iam.Principal) (PolicyCatalogue, error) {
	if err := s.authz.Authorize(ctx, iam.Request{Principal: p, Action: iam.ActionGetPolicyCatalogue, Resource: iam.Resource{Admin: "policy-catalogue"}}); err != nil {
		return PolicyCatalogue{}, fmt.Errorf("policytools.Catalogue: %w", gateError(err))
	}
	return PolicyCatalogue{
		Version: iam.PolicyVersion, Actions: iam.Catalogue(), ConditionKeys: iam.ConditionKeys(), Resources: iam.ResourceKinds(),
		Limits: iam.PolicyLimits(), BucketPolicyPrincipals: iam.BucketPolicyPrincipals(),
	}, nil
}

// PolicyValidation is what validation says of a document: whether a put
// would take it - as a document; a put may still refuse what is not in it,
// a name that is built in, a bucket that is not there - every fault found,
// and what each statement does.
type PolicyValidation struct {
	// Valid is true exactly when Faults is empty.
	Valid bool
	// Faults are the faults of the document, the first first, at most
	// iam.MaxPolicyFaults; never nil.
	Faults []iam.PolicyFault
	// Truncated says that the document had more faults than are given.
	Truncated bool
	// Statements say what each statement of a valid document does, in
	// order; empty, never nil, for a document that is not valid.
	Statements []iam.StatementAnalysis
}

// IdentityPolicyValidation is a named policy, or the inline policy of a new
// API key, to validate.
type IdentityPolicyValidation struct {
	// Document is the policy as it would be sent to the put.
	Document []byte
	// Name is the name it would be stored under; empty when none is
	// given. It decides the resource of the authorization.
	Name string
	// KeyOwner, when set, makes the document the inline policy of a new
	// API key of that user; not with Name.
	KeyOwner string
}

// ValidateIdentity validates a named policy or the inline policy of a new
// API key. It is authorized as the put would be: admin:PutPolicy on
// policy/<name>, or on policy when no name is given; with a key owner,
// admin:CreateAPIKey on user/<owner> - refused, and the refusal audited, to
// a principal bound by its key's inline policy, exactly as CreateAPIKey
// refuses it. A request that is not one - no document, both a name and a
// key owner, a name that is not one - is an error wrapping
// domain.ErrInvalidArgument; a document that is refused is no error, but a
// validation with its faults.
func (s *PolicyToolsService) ValidateIdentity(ctx context.Context, p iam.Principal, in IdentityPolicyValidation) (PolicyValidation, error) {
	switch {
	case len(in.Document) == 0:
		return PolicyValidation{}, fmt.Errorf("policytools.ValidateIdentity: %w: no document to validate", domain.ErrInvalidArgument)
	case in.Name != "" && in.KeyOwner != "":
		return PolicyValidation{}, fmt.Errorf("policytools.ValidateIdentity: %w: a policy is named or is a key's, not both", domain.ErrInvalidArgument)
	}
	var request iam.Request
	switch {
	case in.KeyOwner != "":
		request = iam.Request{Principal: p, Action: iam.ActionCreateKey, Resource: iam.Resource{Admin: "user/" + in.KeyOwner}}
		if p.Boundary != nil {
			// Whoever may not make the key may not check its policy either,
			// refused first, as IAMService.CreateAPIKey refuses it.
			const reason = "an API key with a policy of its own cannot make keys"
			s.auditor.Denied(ctx, request, reason)
			return PolicyValidation{}, fmt.Errorf("policytools.ValidateIdentity: %w: %s", domain.ErrAccessDenied, reason)
		}
		if err := iam.ValidateName(in.KeyOwner); err != nil {
			return PolicyValidation{}, fmt.Errorf("policytools.ValidateIdentity: %w", err)
		}
	case in.Name != "":
		if err := iam.ValidateName(in.Name); err != nil {
			return PolicyValidation{}, fmt.Errorf("policytools.ValidateIdentity: %w", err)
		}
		request = iam.Request{Principal: p, Action: iam.ActionPutPolicy, Resource: iam.Resource{Admin: "policy/" + in.Name}}
	default:
		request = iam.Request{Principal: p, Action: iam.ActionPutPolicy, Resource: iam.Resource{Admin: "policy"}}
	}
	if err := s.authz.Authorize(ctx, request); err != nil {
		return PolicyValidation{}, fmt.Errorf("policytools.ValidateIdentity: %w", gateError(err))
	}
	doc, err := iam.ParsePolicy(in.Document)
	validation, err := validated(err, func() []iam.StatementAnalysis { return iam.AnalysePolicy(doc) })
	if err != nil {
		return PolicyValidation{}, fmt.Errorf("policytools.ValidateIdentity: %w", err)
	}
	return validation, nil
}

// ValidateBucket validates the policy of a bucket. It is authorized as the
// put is, s3:PutBucketPolicy on the bucket, and the bucket is not looked
// up: its name only confines the document, as the parser has it, so a
// bucket that is not there is no error. A name no bucket can have, or no
// document, is an error wrapping domain.ErrInvalidArgument.
func (s *PolicyToolsService) ValidateBucket(ctx context.Context, p iam.Principal, bucketName string, document []byte) (PolicyValidation, error) {
	if bucket.ValidateName(bucketName) != nil {
		return PolicyValidation{}, fmt.Errorf("policytools.ValidateBucket: %w: the bucket is no bucket name", domain.ErrInvalidArgument)
	}
	if len(document) == 0 {
		return PolicyValidation{}, fmt.Errorf("policytools.ValidateBucket: %w: no document to validate", domain.ErrInvalidArgument)
	}
	if err := s.authz.Authorize(ctx, bucketRequest(p, iam.ActionPutBucketPolicy, bucketName)); err != nil {
		return PolicyValidation{}, fmt.Errorf("policytools.ValidateBucket: %w", gateError(err))
	}
	policy, err := iam.ParseBucketPolicy(document, bucketName)
	validation, err := validated(err, func() []iam.StatementAnalysis { return iam.AnalyseBucketPolicy(policy) })
	if err != nil {
		return PolicyValidation{}, fmt.Errorf("policytools.ValidateBucket: %w", err)
	}
	return validation, nil
}

// ValidateSubBucket validates the policy of a sub-bucket. It is authorized
// as the put is, s3:PutSubBucket on the bucket, and neither the bucket nor
// the sub-bucket is looked up. A name no bucket or sub-bucket can have, or
// no document - a sub-bucket without a policy has nothing to validate - is
// an error wrapping domain.ErrInvalidArgument.
func (s *PolicyToolsService) ValidateSubBucket(ctx context.Context, p iam.Principal, bucketName, name string, document []byte) (PolicyValidation, error) {
	if bucket.ValidateName(bucketName) != nil {
		return PolicyValidation{}, fmt.Errorf("policytools.ValidateSubBucket: %w: the bucket is no bucket name", domain.ErrInvalidArgument)
	}
	if err := bucket.ValidateSubBucketName(name); err != nil {
		return PolicyValidation{}, fmt.Errorf("policytools.ValidateSubBucket: %w", err)
	}
	if len(document) == 0 {
		return PolicyValidation{}, fmt.Errorf("policytools.ValidateSubBucket: %w: no document to validate", domain.ErrInvalidArgument)
	}
	if err := s.authz.Authorize(ctx, bucketRequest(p, iam.ActionPutSubBucket, bucketName)); err != nil {
		return PolicyValidation{}, fmt.Errorf("policytools.ValidateSubBucket: %w", gateError(err))
	}
	policy, err := iam.ParseSubBucketPolicy(document, bucketName, name)
	validation, err := validated(err, func() []iam.StatementAnalysis { return iam.AnalyseSubBucketPolicy(policy) })
	if err != nil {
		return PolicyValidation{}, fmt.Errorf("policytools.ValidateSubBucket: %w", err)
	}
	return validation, nil
}

// gateError is what a validation refused or undecided by the authorizer
// returns: the sentinel, and none of the authorizer's text. A decision that
// could not be made because a stored policy - the bucket's - does not parse
// would otherwise carry the parser's reason, which can quote a document the
// caller may not read.
func gateError(err error) error {
	switch {
	case errors.Is(err, domain.ErrSessionLocked):
		return domain.ErrSessionLocked
	case errors.Is(err, domain.ErrAccessDenied):
		return domain.ErrAccessDenied
	case errors.Is(err, domain.ErrIntegrity):
		return fmt.Errorf("%w: a stored policy the decision needs cannot be read", domain.ErrIntegrity)
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return fmt.Errorf("%w: the validation cannot be authorized now", domain.ErrUnavailable)
	}
}

// validated makes the validation of a document from the error of its parse:
// the analysis when it was accepted, its faults when it was refused. Any
// other error is returned.
func validated(parseErr error, analyse func() []iam.StatementAnalysis) (PolicyValidation, error) {
	if parseErr == nil {
		statements := analyse()
		if statements == nil {
			statements = []iam.StatementAnalysis{}
		}
		return PolicyValidation{Valid: true, Faults: []iam.PolicyFault{}, Statements: statements}, nil
	}
	refused, ok := PolicyFaults(parseErr)
	if !ok {
		return PolicyValidation{}, parseErr
	}
	return PolicyValidation{Faults: refused.Faults(), Truncated: refused.Truncated(), Statements: []iam.StatementAnalysis{}}, nil
}

// PolicyFaults returns the located faults of a policy document a put or a
// validation refused: the *iam.PolicyError err wraps. It finds none in an
// error that also wraps domain.ErrIntegrity - the parse of a document
// already stored, which the caller may not be allowed to read - so that an
// inbound adapter can give a caller the faults of the document it sent, and
// never those of one it did not.
func PolicyFaults(err error) (*iam.PolicyError, bool) {
	if errors.Is(err, domain.ErrIntegrity) {
		return nil, false
	}
	var refused *iam.PolicyError
	if !errors.As(err, &refused) {
		return nil, false
	}
	return refused, true
}
