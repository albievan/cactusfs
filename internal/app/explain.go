package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

// The kinds of subject an explanation is about.
const (
	// SubjectUser is a user, as a console session of theirs would be.
	SubjectUser = "user"
	// SubjectAccessKey is an API key: its owner, bounded by its inline
	// policy.
	SubjectAccessKey = "access_key"
	// SubjectAnonymous is a caller without credentials.
	SubjectAnonymous = "anonymous"
)

// Why a subject would not authenticate.
const (
	// NotAuthenticatedUserDisabled is a disabled user, or a key whose owner
	// is disabled.
	NotAuthenticatedUserDisabled = "user_disabled"
	// NotAuthenticatedKeyExpired is a key whose expiry has passed.
	NotAuthenticatedKeyExpired = "key_expired"
)

// NotConsideredObjectOwnership says that an answer leaves out who made the
// version asked about (ADR-0028): only admin:PreviewObject asks it.
const NotConsideredObjectOwnership = "object_ownership"

// maxExplainField bounds what an explanation is asked about: a key, a
// listing prefix, an administrative resource.
const maxExplainField = 1024

// maxExplainAccessKeyID bounds the access key ID an explanation is asked
// about.
const maxExplainAccessKeyID = 128

// reasonUnreadablePolicy is why an explanation that depends on a policy the
// caller may not read, or that cannot be read, is refused.
const reasonUnreadablePolicy = "may not read the policy the answer depends on"

// ExplainService answers "Who may do this?" (ADR-0032, Decision 3): what
// the authorizer decides for a subject - a user, an API key or a caller
// without credentials - an action and a resource, and why. The decision is
// the authorizer's own, through a ports.AccessExplainer, which this use
// case alone holds; it is never the gate of anything. Every answer is
// recorded in the audit trail before it is given, and none is given that
// could not be recorded.
type ExplainService struct {
	store     ports.IAMStore
	trail     ports.AuditLog
	authz     ports.Authorizer
	explainer ports.AccessExplainer
	auditor   *Auditor
	clock     ports.Clock
	logger    *slog.Logger
}

// ExplainServiceConfig lists what an ExplainService depends on.
type ExplainServiceConfig struct {
	// Store resolves the subject: a user and the documents of their
	// policies, or an API key with its owner's. Nothing is written to it.
	Store ports.IAMStore
	// Trail is where every answer is recorded, synchronously, before it
	// is given.
	Trail ports.AuditLog
	// Authz is the single authorizer: it gates the explanation, and says
	// what the caller may read of the policies the answer depends on.
	Authz ports.Authorizer
	// Explainer is the authorizer's decision and why
	// (app.NewExplainer). Only this use case holds one.
	Explainer ports.AccessExplainer
	// Auditor builds the records, and records refusals.
	Auditor *Auditor
	// Clock is the time of this node, at which every answer is decided.
	Clock ports.Clock
	// Logger is where a policy that cannot be read is told, with the
	// names of where it is and never with what it says.
	Logger *slog.Logger
}

// NewExplainService returns an ExplainService.
func NewExplainService(cfg ExplainServiceConfig) *ExplainService {
	return &ExplainService{
		store: cfg.Store, trail: cfg.Trail, authz: cfg.Authz, explainer: cfg.Explainer, auditor: cfg.Auditor,
		clock: cfg.Clock, logger: cfg.Logger,
	}
}

// ExplainRequest is what an explanation is asked about: one subject, an
// action and a resource, and the circumstances of the request.
type ExplainRequest struct {
	// User, AccessKeyID and Anonymous name the subject; exactly one is
	// given.
	User        string
	AccessKeyID string
	Anonymous   bool
	// Action is an action of the catalogue, without a wildcard.
	Action iam.Action
	// Bucket and Key name an S3 resource: the bucket, and the key for an
	// action on objects; neither for s3:ListAllMyBuckets.
	Bucket string
	Key    string
	// AdminResource names an administrative resource, "<kind>" or
	// "<kind>/<name>", as the catalogue's templates; not with Bucket.
	AdminResource string
	// ListPrefix is the prefix of a listing, for the three listing
	// actions only: it decides whether a sub-bucket's policy has a say.
	ListPrefix string
	// SourceIP is the address the request would come from, without a
	// zone; empty, it is unknown, and conditions on it are decided against
	// the request.
	SourceIP string
	// SecureTransport says whether the request would arrive over TLS;
	// nil is taken as true, and the answer says what was assumed.
	SecureTransport *bool
}

// ExplainSubject is whom an explanation is about.
type ExplainSubject struct {
	// Kind is SubjectUser, SubjectAccessKey or SubjectAnonymous.
	Kind string
	// User is the user, or the owner of the key once it is resolved.
	User string
	// AccessKeyID is the key asked about.
	AccessKeyID string
}

// Citation is a statement that had a say in a decision, as far as the
// caller may read where it is.
type Citation struct {
	// Source and Effect are always given.
	Source iam.Source
	Effect iam.Effect
	// Readable says that the caller may read the policy the statement is
	// in. When it is false the rest is empty: "a policy you may not read".
	Readable bool
	// Policy is the name of a named policy; empty for a bucket's, a
	// sub-bucket's or a key's.
	Policy string
	// Document and Statement are the indexes of the document in its
	// source and of the statement in its document, from 0.
	Document  int
	Statement int
	// Sid is the statement's Sid, or empty.
	Sid string
}

// KeyPolicyCitation is what the inline policy of an API key said.
type KeyPolicyCitation struct {
	// Allowed reports whether the key's own policy allowed the request.
	Allowed bool
	// Statements are its statements that matched.
	Statements []Citation
}

// Assumptions are what a decision was made with.
type Assumptions struct {
	// Time is this node's: there is no simulation at another time.
	Time time.Time
	// SourceIP is the address asked about; not valid when it is unknown.
	SourceIP netip.Addr
	// SecureTransport is whether the request was taken to arrive over TLS.
	SecureTransport bool
}

// AccessExplanation is the answer to "Who may do this?".
type AccessExplanation struct {
	// Subject is whom it is about.
	Subject ExplainSubject
	// Action, Resource (the ARN) and ListPrefix are what was decided.
	Action     iam.Action
	Resource   string
	ListPrefix string
	// Authenticates is false when the subject would not authenticate:
	// NotAuthenticated says why, and nothing below is given.
	Authenticates    bool
	NotAuthenticated string
	// Allowed is the decision, and Source where it came from; Rule is the
	// rule that answered when Source is iam.SourceRule.
	Allowed bool
	Source  iam.Source
	Rule    iam.Rule
	// Statements are the statements that matched, of every source.
	Statements []Citation
	// KeyPolicy is what a key's inline policy said; nil for a subject
	// without one, or when the decision did not reach the policies.
	KeyPolicy *KeyPolicyCitation
	// UnknownAddress are the statements whose condition on the source
	// address was decided without an address.
	UnknownAddress []Citation
	// Assumed is what the decision was made with.
	Assumed Assumptions
	// NotConsidered is what the answer leaves out:
	// NotConsideredObjectOwnership for admin:PreviewObject, nothing
	// otherwise. Never nil.
	NotConsidered []string
}

// asked is an explanation request read: the request it is about, without
// its principal, and what it assumes.
type asked struct {
	request iam.Request
	assumed Assumptions
}

// Explain answers what the authorizer decides for a subject, an action and
// a resource, and why. In this order:
//
//  1. The caller must hold admin:ExplainAccess on the subject: user/<name>
//     for a user; user/<owner> for an API key, resolved first, or user when
//     there is no such key, so that a refusal says nothing of the key;
//     anonymous for a caller without credentials. A refusal is audited.
//  2. The request is read: an action of the catalogue without a wildcard,
//     a resource of one of its templates, a listing prefix for a listing
//     only, an address. One that is not one is an error wrapping
//     domain.ErrInvalidArgument, recorded.
//  3. An answer about an s3: action on a bucket tells what the bucket's
//     policy says, and one whose key or listing prefix could lie in a
//     sub-bucket what that sub-bucket's says: the caller must also be
//     allowed s3:GetBucketPolicy, and then s3:GetSubBucketPolicy, on the
//     bucket. Otherwise - and when such a policy cannot be read - the
//     explanation is refused as a whole, audited as a refusal of
//     admin:ExplainAccess.
//  4. The subject is resolved: one that is not there is an error wrapping
//     domain.ErrEntityNotFound, recorded; a disabled user, a key whose
//     owner is disabled and an expired key would not authenticate, which
//     is the answer, recorded, without a decision. A subject one of whose
//     policies cannot be read - a role's, or a key's inline policy - is
//     refused as any refusal is, audited; for a key that is so before the
//     caller is asked about it, since whose it is cannot be known.
//  5. The explainer decides, at this node's time.
//  6. A cited statement is named only where the caller may read the
//     policy it is in.
//  7. The answer is recorded through the audit trail, synchronously; if
//     the record cannot be appended no answer is given: the error wraps
//     domain.ErrUnavailable, or domain.ErrOutcomeUnknown when the store
//     cannot say whether it was.
//
// No error says who the subject is, what was asked or what a policy says:
// their texts are fixed, every refusal's the same, and nothing a store or a
// parser said is returned, logged or recorded.
func (s *ExplainService) Explain(ctx context.Context, p iam.Principal, in ExplainRequest) (AccessExplanation, error) {
	// One reading of the policies for every decision of the call.
	ctx = withScope(ctx)
	answer := AccessExplanation{Subject: ExplainSubject{Kind: subjectKind(in)}, NotConsidered: []string{}}
	if err := checkSubject(in); err != nil {
		return AccessExplanation{}, fmt.Errorf("explain.Explain: %w", err)
	}

	// 1. The subject, and the caller's right to ask about it.
	gate := iam.Request{Principal: p, Action: iam.ActionExplainAccess, Resource: iam.Resource{Admin: "anonymous"}}
	var identity iam.Identity
	var resolveErr error
	switch answer.Subject.Kind {
	case SubjectUser:
		answer.Subject.User = in.User
		gate.Resource.Admin = "user/" + in.User
	case SubjectAccessKey:
		answer.Subject.AccessKeyID = in.AccessKeyID
		identity, resolveErr = s.store.ResolveIdentity(ctx, in.AccessKeyID)
		resolveErr = resolution(resolveErr)
		switch {
		case resolveErr == nil:
			answer.Subject.User = identity.User.Name
			gate.Resource.Admin = "user/" + identity.User.Name
		case errors.Is(resolveErr, domain.ErrEntityNotFound):
			// Whether the key is there is not told to somebody who may
			// not ask: the authorizer is asked about a key of nobody.
			gate.Resource.Admin = "user"
		case errors.Is(resolveErr, domain.ErrIntegrity):
			// The key's policy, or its owner's, cannot be read, and so
			// whose it is is not known: the gate is not lowered to a key
			// of nobody. Refused, as any refusal.
			gate.Resource.Admin = "user"
			return AccessExplanation{}, s.unreadableSubject(ctx, gate, answer.Subject.Kind)
		default:
			return AccessExplanation{}, resolveErr
		}
	}
	if err := s.authz.Authorize(ctx, gate); err != nil {
		return AccessExplanation{}, fmt.Errorf("explain.Explain: %w", refusal(err))
	}
	details := map[string]string{"subject": subjectName(answer.Subject)}
	if answer.Subject.AccessKeyID != "" {
		details["access_key_id"] = answer.Subject.AccessKeyID
	}

	// 2. The request. The action is recorded only as a name of the
	// catalogue, never as the text the caller sent.
	if _, ok := iam.LookupAction(in.Action); ok {
		details["action"] = string(in.Action)
	}
	read, err := readExplainRequest(in, s.clock.Now().UTC())
	if err != nil {
		details["answer"] = "invalid_argument"
		if recordErr := s.record(ctx, p, gate, audit.Failed, details); recordErr != nil {
			return AccessExplanation{}, recordErr
		}
		return AccessExplanation{}, fmt.Errorf("explain.Explain: %w", err)
	}
	answer.Action, answer.Resource, answer.ListPrefix, answer.Assumed = read.request.Action, read.request.Resource.String(), read.request.ListPrefix, read.assumed
	if read.request.Action == iam.ActionPreviewObject {
		answer.NotConsidered = append(answer.NotConsidered, NotConsideredObjectOwnership)
	}
	details["resource"] = answer.Resource
	if answer.ListPrefix != "" {
		details["list_prefix"] = answer.ListPrefix
	}

	// 3. The policies the answer depends on.
	if err := s.policiesReadable(ctx, p, gate, read.request); err != nil {
		return AccessExplanation{}, err
	}

	// 4. The subject, as authentication would make it.
	principal, notAuthenticated, err := s.subject(ctx, answer.Subject, in, identity, resolveErr, read)
	switch {
	case errors.Is(err, domain.ErrEntityNotFound):
		details["answer"] = "no_such_entity"
		if recordErr := s.record(ctx, p, gate, audit.Failed, details); recordErr != nil {
			return AccessExplanation{}, recordErr
		}
		return AccessExplanation{}, fmt.Errorf("explain.Explain: %w: the subject is not there", domain.ErrEntityNotFound)
	case errors.Is(err, domain.ErrIntegrity):
		return AccessExplanation{}, s.unreadableSubject(ctx, gate, answer.Subject.Kind)
	case err != nil:
		return AccessExplanation{}, err
	case notAuthenticated != "":
		details["answer"], details["not_authenticated"] = "not_authenticated", notAuthenticated
		if recordErr := s.record(ctx, p, gate, audit.Succeeded, details); recordErr != nil {
			return AccessExplanation{}, recordErr
		}
		answer.NotAuthenticated = notAuthenticated
		return answer, nil
	}

	// 5. The decision.
	request := read.request
	request.Principal = principal
	explanation, err := s.explainer.Explain(ctx, request)
	if err != nil {
		return AccessExplanation{}, s.undecided(ctx, gate, request, err)
	}

	// 6. What the caller may read of it.
	answer.Authenticates, answer.Allowed, answer.Source, answer.Rule = true, explanation.Allowed, explanation.Source, explanation.Rule
	readable := citationReader{authz: s.authz, caller: p, bucket: request.Resource.Bucket, owner: principal.User, memo: map[string]bool{}}
	answer.Statements = readable.cite(ctx, explanation.Statements)
	answer.UnknownAddress = readable.cite(ctx, explanation.UnknownAddress)
	if explanation.KeyPolicy != nil {
		answer.KeyPolicy = &KeyPolicyCitation{Allowed: explanation.KeyPolicy.Allowed, Statements: readable.cite(ctx, explanation.KeyPolicy.Statements)}
	}

	// 7. The record, before the answer.
	details["answer"], details["allowed"], details["source"] = "decision", strconv.FormatBool(answer.Allowed), string(answer.Source)
	if answer.Rule != "" {
		details["rule"] = string(answer.Rule)
	}
	if err := s.record(ctx, p, gate, audit.Succeeded, details); err != nil {
		return AccessExplanation{}, err
	}
	return answer, nil
}

// subject resolves the subject of an explanation as authentication would
// make it: a user with the documents of their policies and their names, a
// key with its owner's and its inline policy as the boundary, or a caller
// without credentials. It returns why the subject would not authenticate,
// or an error wrapping domain.ErrEntityNotFound for one that is not there.
func (s *ExplainService) subject(
	ctx context.Context, subject ExplainSubject, in ExplainRequest, identity iam.Identity, resolveErr error, read asked,
) (iam.Principal, string, error) {
	session := iam.Session{SourceIP: read.assumed.SourceIP, SecureTransport: read.assumed.SecureTransport}
	switch subject.Kind {
	case SubjectUser:
		resolved, err := s.store.ResolveUser(ctx, in.User)
		if err != nil {
			return iam.Principal{}, "", resolution(err)
		}
		identity = resolved
	case SubjectAccessKey:
		if resolveErr != nil {
			return iam.Principal{}, "", resolveErr
		}
		if identity.Key.Expired(read.assumed.Time) {
			return iam.Principal{}, NotAuthenticatedKeyExpired, nil
		}
	default:
		return iam.Principal{Session: session}, "", nil
	}
	if identity.User.Disabled {
		return iam.Principal{}, NotAuthenticatedUserDisabled, nil
	}
	principal := iam.Principal{
		User: identity.User.Name, UserCreatedAt: identity.User.CreatedAt, Policies: identity.Policies,
		PolicyNames: identity.PolicyNames, Session: session,
	}
	if subject.Kind == SubjectAccessKey {
		principal.AccessKeyID, principal.Boundary = identity.Key.AccessKeyID, identity.Key.InlinePolicy
	}
	return principal, "", nil
}

// resolution reads the failure to resolve a subject into one of three
// sentinels, and none of the store's text: a stored policy that cannot be
// read is reported by the store with the parser's reason, which can quote
// the document, and a key's with the key's ID.
//
//   - domain.ErrEntityNotFound: the subject is not there - a user, a key, or
//     a key whose owner was removed;
//   - domain.ErrIntegrity: a policy of the subject - one of its roles', or
//     a key's inline policy - cannot be read (unreadableSubject);
//   - domain.ErrUnavailable, with a fixed text: anything else.
func resolution(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrIntegrity):
		return domain.ErrIntegrity
	case errors.Is(err, domain.ErrEntityNotFound) || errors.Is(err, domain.ErrCredentialNotFound):
		return domain.ErrEntityNotFound
	default:
		return fmt.Errorf("explain.Explain: %w: the subject cannot be resolved now", domain.ErrUnavailable)
	}
}

// refused is the one error of every refusal of an explanation: by the
// authorizer, for a policy the caller may not read, and for one that cannot
// be read. Its text is fixed, so that none is told from another.
func refused() error {
	return fmt.Errorf("explain.Explain: %w", domain.ErrAccessDenied)
}

// unreadableSubject refuses an explanation whose subject has a policy that
// cannot be read - one of its roles', or a key's inline policy - as a
// damaged policy of a bucket is refused (undecided): audited as a refusal of
// admin:ExplainAccess on gate, and logged with the kind of the subject
// alone. What the store or the parser said is told nowhere: not to the
// caller, not in the log, not in the trail, whose record of the refusal
// names the gate's resource as every refusal does.
func (s *ExplainService) unreadableSubject(ctx context.Context, gate iam.Request, kind string) error {
	s.logger.ErrorContext(ctx, "a policy of the subject of an explanation cannot be read; the explanation is refused",
		slog.String("subject_kind", kind))
	s.auditor.Denied(ctx, gate, reasonUnreadablePolicy)
	return refused()
}

// policiesReadable refuses an explanation whose answer depends on a policy
// the caller may not read, or that cannot be read (ADR-0032, Decision 3,
// step 2): the bucket's of an s3: action on a bucket, and the sub-bucket's
// when the key or the listing prefix could lie in one - asked whether or
// not such a sub-bucket is registered, since a requirement that depended
// on it would tell it. The refusal is audited as one of admin:ExplainAccess.
func (s *ExplainService) policiesReadable(ctx context.Context, p iam.Principal, gate, request iam.Request) error {
	if !strings.HasPrefix(string(request.Action), "s3:") || request.Resource.Bucket == "" {
		return nil
	}
	needed := []iam.Action{iam.ActionGetBucketPolicy}
	if _, in := subBucketOf(request); in {
		needed = append(needed, iam.ActionGetSubBucketPolicy)
	}
	for _, action := range needed {
		allowed, err := s.authz.Allowed(ctx, bucketRequest(p, action, request.Resource.Bucket))
		switch {
		case errors.Is(err, domain.ErrIntegrity):
			s.damaged(ctx, request)
		case errors.Is(err, domain.ErrSessionLocked):
			return fmt.Errorf("explain.Explain: %w", domain.ErrSessionLocked)
		case err != nil:
			return fmt.Errorf("explain.Explain: %w: whether the caller may read the policies the answer depends on cannot be decided", domain.ErrUnavailable)
		case allowed:
			continue
		}
		s.auditor.Denied(ctx, gate, reasonUnreadablePolicy)
		return refused()
	}
	return nil
}

// undecided answers an explanation the explainer could make no decision
// for. A policy that cannot be read is answered as a refusal of the
// explanation, audited, and logged with the names of the bucket and the
// sub-bucket only: a damaged policy is told from a readable one in the
// node's log alone. Nothing of the explainer's error is returned or logged:
// its text can quote the document.
func (s *ExplainService) undecided(ctx context.Context, gate, request iam.Request, err error) error {
	if errors.Is(err, domain.ErrIntegrity) {
		s.damaged(ctx, request)
		s.auditor.Denied(ctx, gate, reasonUnreadablePolicy)
		return refused()
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("explain.Explain: %w", ctxErr)
	}
	s.logger.WarnContext(ctx, "the policies an explanation depends on cannot be read now; no answer is given",
		slog.String("bucket", request.Resource.Bucket))
	return fmt.Errorf("explain.Explain: %w: the policies the answer depends on cannot be read now", domain.ErrUnavailable)
}

// damaged logs that a policy an explanation depends on cannot be read,
// naming the bucket and the sub-bucket the request could be in, and never
// what the policy says.
func (s *ExplainService) damaged(ctx context.Context, request iam.Request) {
	name, _ := subBucketOf(request)
	s.logger.ErrorContext(ctx, "a policy an explanation depends on cannot be read; the explanation is refused",
		slog.String("bucket", request.Resource.Bucket), slog.String("sub_bucket", name))
}

// record appends the record of an answer to the audit trail, synchronously:
// an answer whose record is not in the trail is not given. The question was
// authorized and answered, so its record is written even when the caller
// has gone - a closed connection ends the request's context - within the
// bound every audit write has.
func (s *ExplainService) record(ctx context.Context, p iam.Principal, gate iam.Request, outcome audit.Outcome, details map[string]string) error {
	record := s.auditor.Record(p, iam.ActionExplainAccess, gate.Resource, outcome, details)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()
	err := s.trail.AppendAudit(ctx, record)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrOutcomeUnknown):
		return fmt.Errorf("explain.Explain: %w: whether the answer was recorded is not known, and it is not given", domain.ErrOutcomeUnknown)
	default:
		return fmt.Errorf("explain.Explain: %w: the answer could not be recorded, and it is not given", domain.ErrUnavailable)
	}
}

// refusal is what an explanation refused by the authorizer returns: the
// sentinel, and nothing of the text, which names the subject.
func refusal(err error) error {
	switch {
	case errors.Is(err, domain.ErrSessionLocked):
		return domain.ErrSessionLocked
	case errors.Is(err, domain.ErrAccessDenied):
		return domain.ErrAccessDenied
	default:
		return fmt.Errorf("%w: the explanation cannot be authorized now", domain.ErrUnavailable)
	}
}

// subjectKind says which subject a request names; empty when it names
// none or more than one.
func subjectKind(in ExplainRequest) string {
	kinds := make([]string, 0, 3)
	if in.User != "" {
		kinds = append(kinds, SubjectUser)
	}
	if in.AccessKeyID != "" {
		kinds = append(kinds, SubjectAccessKey)
	}
	if in.Anonymous {
		kinds = append(kinds, SubjectAnonymous)
	}
	if len(kinds) != 1 {
		return ""
	}
	return kinds[0]
}

// checkSubject refuses a request that does not name one subject that could
// be: these are refused before the authorizer is asked, since there is no
// subject to ask about.
func checkSubject(in ExplainRequest) error {
	switch subjectKind(in) {
	case "":
		return fmt.Errorf("%w: name one subject: a user, an access key or anonymous", domain.ErrInvalidArgument)
	case SubjectUser:
		return iam.ValidateName(in.User)
	case SubjectAccessKey:
		if len(in.AccessKeyID) > maxExplainAccessKeyID {
			return fmt.Errorf("%w: an access key ID is 1 to %d characters", domain.ErrInvalidArgument, maxExplainAccessKeyID)
		}
	}
	return nil
}

// subjectName names a subject in the audit trail.
func subjectName(subject ExplainSubject) string {
	switch subject.Kind {
	case SubjectUser:
		return "user/" + subject.User
	case SubjectAccessKey:
		return "key/" + subject.AccessKeyID
	default:
		return SubjectAnonymous
	}
}

// listingAction reports whether a listing prefix belongs to an action.
func listingAction(action iam.Action) bool {
	return action == iam.ActionListBucket || action == iam.ActionListBucketVersions || action == iam.ActionListBucketMultipartUploads
}

// readExplainRequest reads what an explanation is asked about. Its errors
// wrap domain.ErrInvalidArgument and name nothing that was sent.
func readExplainRequest(in ExplainRequest, now time.Time) (asked, error) {
	entry, ok := iam.LookupAction(in.Action)
	if !ok {
		return asked{}, fmt.Errorf("%w: the action is not one of the catalogue, without a wildcard", domain.ErrInvalidArgument)
	}
	var resource iam.Resource
	switch {
	case in.AdminResource != "" && (in.Bucket != "" || in.Key != ""):
		return asked{}, fmt.Errorf("%w: a resource is administrative or of S3, not both", domain.ErrInvalidArgument)
	case in.AdminResource != "":
		if !boundedText(in.AdminResource) {
			return asked{}, fmt.Errorf("%w: an administrative resource is 1 to %d characters of UTF-8", domain.ErrInvalidArgument, maxExplainField)
		}
		if err := checkAdminName(in.AdminResource); err != nil {
			return asked{}, err
		}
		resource.Admin = in.AdminResource
	default:
		if in.Bucket != "" {
			if err := bucket.ValidateName(in.Bucket); err != nil {
				return asked{}, fmt.Errorf("%w: the bucket is no bucket name", domain.ErrInvalidArgument)
			}
		}
		if in.Key != "" && !boundedText(in.Key) {
			return asked{}, fmt.Errorf("%w: a key is 1 to %d characters of UTF-8", domain.ErrInvalidArgument, maxExplainField)
		}
		if in.Key != "" && in.Bucket == "" {
			return asked{}, fmt.Errorf("%w: a key is in a bucket", domain.ErrInvalidArgument)
		}
		if in.Bucket == "" && !entry.AskedOn(iam.Resource{}) {
			// Only an action asked on every bucket as a whole
			// (s3:ListAllMyBuckets) is asked without a bucket.
			return asked{}, fmt.Errorf("%w: the action is asked on a bucket, and none is named", domain.ErrInvalidArgument)
		}
		resource.Bucket, resource.Key = in.Bucket, in.Key
	}
	if !entry.AskedOn(resource) {
		return asked{}, fmt.Errorf("%w: the resource is not of a template the action is asked on", domain.ErrInvalidArgument)
	}
	if in.ListPrefix != "" && (!listingAction(entry.Name) || !boundedText(in.ListPrefix)) {
		return asked{}, fmt.Errorf("%w: a listing prefix is for a listing, and 1 to %d characters of UTF-8", domain.ErrInvalidArgument, maxExplainField)
	}
	assumed := Assumptions{Time: now, SecureTransport: true}
	if in.SecureTransport != nil {
		assumed.SecureTransport = *in.SecureTransport
	}
	if in.SourceIP != "" {
		addr, err := netip.ParseAddr(in.SourceIP)
		if err != nil || addr.Zone() != "" {
			return asked{}, fmt.Errorf("%w: the source address is no IP address without a zone", domain.ErrInvalidArgument)
		}
		assumed.SourceIP = addr.Unmap()
	}
	return asked{
		request: iam.Request{Action: entry.Name, Resource: resource, ListPrefix: in.ListPrefix, Now: now},
		assumed: assumed,
	}, nil
}

// checkAdminName refuses an administrative resource whose name is none its
// kind can have: a user's, group's, role's or policy's (iam.ValidateName), a
// target's (delivery.ValidateTargetName), a tag key (object.ValidateTagKey).
// A kind without a name is left to the templates of the action. A tag key
// may hold a '/' (the template's {tag_key}), so a definition's name is all
// that follows the first '/' of the resource.
func checkAdminName(resource string) error {
	kind, name, named := strings.Cut(resource, "/")
	if !named {
		return nil
	}
	var err error
	switch kind {
	case "user", "group", "role", "policy":
		err = iam.ValidateName(name)
	case "target":
		err = delivery.ValidateTargetName(name)
	case "tag-definition":
		err = object.ValidateTagKey(name)
	default:
		err = domain.ErrInvalidArgument
	}
	if err != nil {
		return fmt.Errorf("%w: the name of the administrative resource is none its kind can have", domain.ErrInvalidArgument)
	}
	return nil
}

// boundedText reports whether s is UTF-8 of 1 to maxExplainField bytes.
func boundedText(s string) bool {
	return s != "" && len(s) <= maxExplainField && utf8.ValidString(s)
}

// citationReader names a cited statement only where the caller may read
// the policy it is in, asked of the authorizer with Allowed - which audits
// nothing - once a question.
type citationReader struct {
	authz  ports.Authorizer
	caller iam.Principal
	// bucket is the bucket of the request, owner the owner of the key.
	bucket, owner string
	memo          map[string]bool
}

// cite returns the citations of statements, each named or not; never nil.
func (r citationReader) cite(ctx context.Context, statements []iam.CitedStatement) []Citation {
	citations := make([]Citation, 0, len(statements))
	for _, statement := range statements {
		citation := Citation{Source: statement.Source, Effect: statement.Effect}
		if r.mayRead(ctx, statement) {
			citation.Readable, citation.Policy, citation.Document, citation.Statement, citation.Sid = true, statement.Policy, statement.Document, statement.Statement, statement.Sid
		}
		citations = append(citations, citation)
	}
	return citations
}

// mayRead reports whether the caller may read the policy a statement is
// in: admin:GetPolicy on a named policy, s3:GetBucketPolicy or
// s3:GetSubBucketPolicy on the bucket, admin:ListAPIKeys on the key's
// owner. A policy of the caller's own without a name, and one that cannot
// be asked about, is not read.
func (r citationReader) mayRead(ctx context.Context, statement iam.CitedStatement) bool {
	request := iam.Request{Principal: r.caller}
	switch statement.Source {
	case iam.SourceIdentity:
		if statement.Policy == "" {
			return false
		}
		request.Action, request.Resource = iam.ActionGetPolicy, iam.Resource{Admin: "policy/" + statement.Policy}
	case iam.SourceBucket:
		request.Action, request.Resource = iam.ActionGetBucketPolicy, iam.Resource{Bucket: r.bucket}
	case iam.SourceSubBucket:
		request.Action, request.Resource = iam.ActionGetSubBucketPolicy, iam.Resource{Bucket: r.bucket}
	case iam.SourceKeyBound:
		request.Action, request.Resource = iam.ActionListKeys, iam.Resource{Admin: "user/" + r.owner}
	default:
		return false
	}
	if request.Resource.Bucket == "" && request.Resource.Admin == "" {
		return false
	}
	asked := string(request.Action) + "\x00" + request.Resource.String()
	if allowed, ok := r.memo[asked]; ok {
		return allowed
	}
	allowed, err := r.authz.Allowed(ctx, request)
	allowed = allowed && err == nil
	r.memo[asked] = allowed
	return allowed
}
