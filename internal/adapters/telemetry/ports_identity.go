package telemetry

import (
	"context"
	"time"

	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// The decorators of the ports of identities, sessions, repeated requests,
// the audit trail, events and passwords. As with every other port, the
// method label is the name of the Go method and the result is "error"
// whenever the port returns one - also when the error is the port's way of
// saying that there is no such user, session or record. No name of a user,
// no access key and no token is an attribute or a label.

// InstrumentIAM returns next wrapped so that every call is traced and
// counted.
func (t *Telemetry) InstrumentIAM(next ports.IAMStore) ports.IAMStore {
	return &iamStore{t: t, next: next}
}

type iamStore struct {
	t    *Telemetry
	next ports.IAMStore
}

const portIAM = "iam"

func (*iamStore) instrumented() {}

// PutUser implements ports.IAMStore.
func (d *iamStore) PutUser(ctx context.Context, u iam.User, create bool, record audit.Record) error {
	ctx, done := d.t.call(ctx, portIAM, "PutUser")
	err := d.next.PutUser(ctx, u, create, record)
	done(err)
	return err
}

// DeleteUser implements ports.IAMStore.
func (d *iamStore) DeleteUser(ctx context.Context, name string, record audit.Record) error {
	ctx, done := d.t.call(ctx, portIAM, "DeleteUser")
	err := d.next.DeleteUser(ctx, name, record)
	done(err)
	return err
}

// GetUser implements ports.IAMStore.
func (d *iamStore) GetUser(ctx context.Context, name string) (iam.User, error) {
	ctx, done := d.t.call(ctx, portIAM, "GetUser")
	result, err := d.next.GetUser(ctx, name)
	done(err)
	return result, err
}

// ListUsers implements ports.IAMStore.
func (d *iamStore) ListUsers(ctx context.Context) ([]iam.User, error) {
	ctx, done := d.t.call(ctx, portIAM, "ListUsers")
	result, err := d.next.ListUsers(ctx)
	done(err)
	return result, err
}

// PutGroup implements ports.IAMStore.
func (d *iamStore) PutGroup(ctx context.Context, g iam.Group, create bool, record audit.Record) error {
	ctx, done := d.t.call(ctx, portIAM, "PutGroup")
	err := d.next.PutGroup(ctx, g, create, record)
	done(err)
	return err
}

// DeleteGroup implements ports.IAMStore.
func (d *iamStore) DeleteGroup(ctx context.Context, name string, record audit.Record) error {
	ctx, done := d.t.call(ctx, portIAM, "DeleteGroup")
	err := d.next.DeleteGroup(ctx, name, record)
	done(err)
	return err
}

// GetGroup implements ports.IAMStore.
func (d *iamStore) GetGroup(ctx context.Context, name string) (iam.Group, error) {
	ctx, done := d.t.call(ctx, portIAM, "GetGroup")
	result, err := d.next.GetGroup(ctx, name)
	done(err)
	return result, err
}

// ListGroups implements ports.IAMStore.
func (d *iamStore) ListGroups(ctx context.Context) ([]iam.Group, error) {
	ctx, done := d.t.call(ctx, portIAM, "ListGroups")
	result, err := d.next.ListGroups(ctx)
	done(err)
	return result, err
}

// PutRole implements ports.IAMStore.
func (d *iamStore) PutRole(ctx context.Context, r iam.Role, create bool, record audit.Record) error {
	ctx, done := d.t.call(ctx, portIAM, "PutRole")
	err := d.next.PutRole(ctx, r, create, record)
	done(err)
	return err
}

// DeleteRole implements ports.IAMStore.
func (d *iamStore) DeleteRole(ctx context.Context, name string, record audit.Record) error {
	ctx, done := d.t.call(ctx, portIAM, "DeleteRole")
	err := d.next.DeleteRole(ctx, name, record)
	done(err)
	return err
}

// GetRole implements ports.IAMStore.
func (d *iamStore) GetRole(ctx context.Context, name string) (iam.Role, error) {
	ctx, done := d.t.call(ctx, portIAM, "GetRole")
	result, err := d.next.GetRole(ctx, name)
	done(err)
	return result, err
}

// ListRoles implements ports.IAMStore.
func (d *iamStore) ListRoles(ctx context.Context) ([]iam.Role, error) {
	ctx, done := d.t.call(ctx, portIAM, "ListRoles")
	result, err := d.next.ListRoles(ctx)
	done(err)
	return result, err
}

// PutPolicy implements ports.IAMStore.
func (d *iamStore) PutPolicy(ctx context.Context, p iam.Policy, create bool, record audit.Record) error {
	ctx, done := d.t.call(ctx, portIAM, "PutPolicy")
	err := d.next.PutPolicy(ctx, p, create, record)
	done(err)
	return err
}

// DeletePolicy implements ports.IAMStore.
func (d *iamStore) DeletePolicy(ctx context.Context, name string, record audit.Record) error {
	ctx, done := d.t.call(ctx, portIAM, "DeletePolicy")
	err := d.next.DeletePolicy(ctx, name, record)
	done(err)
	return err
}

// GetPolicy implements ports.IAMStore.
func (d *iamStore) GetPolicy(ctx context.Context, name string) (iam.Policy, error) {
	ctx, done := d.t.call(ctx, portIAM, "GetPolicy")
	result, err := d.next.GetPolicy(ctx, name)
	done(err)
	return result, err
}

// ListPolicies implements ports.IAMStore.
func (d *iamStore) ListPolicies(ctx context.Context) ([]iam.Policy, error) {
	ctx, done := d.t.call(ctx, portIAM, "ListPolicies")
	result, err := d.next.ListPolicies(ctx)
	done(err)
	return result, err
}

// CreateAPIKey implements ports.IAMStore.
func (d *iamStore) CreateAPIKey(ctx context.Context, k iam.APIKey, record audit.Record) error {
	ctx, done := d.t.call(ctx, portIAM, "CreateAPIKey")
	err := d.next.CreateAPIKey(ctx, k, record)
	done(err)
	return err
}

// DeleteAPIKey implements ports.IAMStore.
func (d *iamStore) DeleteAPIKey(ctx context.Context, accessKeyID string, record audit.Record) error {
	ctx, done := d.t.call(ctx, portIAM, "DeleteAPIKey")
	err := d.next.DeleteAPIKey(ctx, accessKeyID, record)
	done(err)
	return err
}

// ListAPIKeys implements ports.IAMStore.
func (d *iamStore) ListAPIKeys(ctx context.Context, owner string) ([]iam.APIKey, error) {
	ctx, done := d.t.call(ctx, portIAM, "ListAPIKeys")
	result, err := d.next.ListAPIKeys(ctx, owner)
	done(err)
	return result, err
}

// TouchAPIKey implements ports.IAMStore.
func (d *iamStore) TouchAPIKey(ctx context.Context, accessKeyID string, usedAt time.Time) error {
	ctx, done := d.t.call(ctx, portIAM, "TouchAPIKey")
	err := d.next.TouchAPIKey(ctx, accessKeyID, usedAt)
	done(err)
	return err
}

// ResolveIdentity implements ports.IAMStore.
func (d *iamStore) ResolveIdentity(ctx context.Context, accessKeyID string) (iam.Identity, error) {
	ctx, done := d.t.call(ctx, portIAM, "ResolveIdentity")
	result, err := d.next.ResolveIdentity(ctx, accessKeyID)
	done(err)
	return result, err
}

// ResolveUser implements ports.IAMStore.
func (d *iamStore) ResolveUser(ctx context.Context, name string) (iam.Identity, error) {
	ctx, done := d.t.call(ctx, portIAM, "ResolveUser")
	result, err := d.next.ResolveUser(ctx, name)
	done(err)
	return result, err
}

// InstrumentSessions returns next wrapped so that every call is traced and
// counted.
func (t *Telemetry) InstrumentSessions(next ports.SessionStore) ports.SessionStore {
	return &sessionStore{t: t, next: next}
}

type sessionStore struct {
	t    *Telemetry
	next ports.SessionStore
}

const portSessions = "sessions"

func (*sessionStore) instrumented() {}

// PutSession implements ports.SessionStore.
func (d *sessionStore) PutSession(ctx context.Context, s ports.Session, record audit.Record) error {
	ctx, done := d.t.call(ctx, portSessions, "PutSession")
	err := d.next.PutSession(ctx, s, record)
	done(err)
	return err
}

// GetSession implements ports.SessionStore.
func (d *sessionStore) GetSession(ctx context.Context, tokenHash string) (ports.Session, error) {
	ctx, done := d.t.call(ctx, portSessions, "GetSession")
	result, err := d.next.GetSession(ctx, tokenHash)
	done(err)
	return result, err
}

// DeleteSession implements ports.SessionStore.
func (d *sessionStore) DeleteSession(ctx context.Context, tokenHash string, record audit.Record) error {
	ctx, done := d.t.call(ctx, portSessions, "DeleteSession")
	err := d.next.DeleteSession(ctx, tokenHash, record)
	done(err)
	return err
}

// DeleteExpiredSessions implements ports.SessionStore.
func (d *sessionStore) DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error) {
	ctx, done := d.t.call(ctx, portSessions, "DeleteExpiredSessions")
	result, err := d.next.DeleteExpiredSessions(ctx, now)
	done(err)
	return result, err
}

// InstrumentIdempotency returns next wrapped so that every call is traced and
// counted.
func (t *Telemetry) InstrumentIdempotency(next ports.IdempotencyStore) ports.IdempotencyStore {
	return &idempotencyStore{t: t, next: next}
}

type idempotencyStore struct {
	t    *Telemetry
	next ports.IdempotencyStore
}

const portIdempotency = "idempotency"

func (*idempotencyStore) instrumented() {}

// BeginIdempotent implements ports.IdempotencyStore.
func (d *idempotencyStore) BeginIdempotent(ctx context.Context, r ports.IdempotencyRecord) error {
	ctx, done := d.t.call(ctx, portIdempotency, "BeginIdempotent")
	err := d.next.BeginIdempotent(ctx, r)
	done(err)
	return err
}

// GetIdempotent implements ports.IdempotencyStore.
func (d *idempotencyStore) GetIdempotent(ctx context.Context, id string) (ports.IdempotencyRecord, error) {
	ctx, done := d.t.call(ctx, portIdempotency, "GetIdempotent")
	result, err := d.next.GetIdempotent(ctx, id)
	done(err)
	return result, err
}

// CompleteIdempotent implements ports.IdempotencyStore.
func (d *idempotencyStore) CompleteIdempotent(ctx context.Context, r ports.IdempotencyRecord) error {
	ctx, done := d.t.call(ctx, portIdempotency, "CompleteIdempotent")
	err := d.next.CompleteIdempotent(ctx, r)
	done(err)
	return err
}

// DeleteIdempotent implements ports.IdempotencyStore.
func (d *idempotencyStore) DeleteIdempotent(ctx context.Context, id string, createdAt time.Time) error {
	ctx, done := d.t.call(ctx, portIdempotency, "DeleteIdempotent")
	err := d.next.DeleteIdempotent(ctx, id, createdAt)
	done(err)
	return err
}

// DeleteIdempotentBefore implements ports.IdempotencyStore.
func (d *idempotencyStore) DeleteIdempotentBefore(ctx context.Context, before time.Time) (int, error) {
	ctx, done := d.t.call(ctx, portIdempotency, "DeleteIdempotentBefore")
	result, err := d.next.DeleteIdempotentBefore(ctx, before)
	done(err)
	return result, err
}

// InstrumentAuditLog returns next wrapped so that every call is traced and
// counted.
func (t *Telemetry) InstrumentAuditLog(next ports.AuditLog) ports.AuditLog {
	return &auditLog{t: t, next: next}
}

type auditLog struct {
	t    *Telemetry
	next ports.AuditLog
}

const portAudit = "audit"

func (*auditLog) instrumented() {}

// AppendAudit implements ports.AuditLog.
func (a *auditLog) AppendAudit(ctx context.Context, record audit.Record) error {
	ctx, done := a.t.call(ctx, portAudit, "AppendAudit")
	err := a.next.AppendAudit(ctx, record)
	done(err)
	return err
}

// ListAudit implements ports.AuditLog.
func (a *auditLog) ListAudit(ctx context.Context, after uint64, limit int) ([]audit.Record, error) {
	ctx, done := a.t.call(ctx, portAudit, "ListAudit")
	result, err := a.next.ListAudit(ctx, after, limit)
	done(err)
	return result, err
}

// InstrumentEventPublisher returns next wrapped so that every call is traced and
// counted. An event that cannot be delivered is counted here; the change it
// tells of is committed all the same.
func (t *Telemetry) InstrumentEventPublisher(next ports.EventPublisher) ports.EventPublisher {
	return &eventPublisher{t: t, next: next}
}

type eventPublisher struct {
	t    *Telemetry
	next ports.EventPublisher
}

const portEvents = "events"

func (*eventPublisher) instrumented() {}

// Publish implements ports.EventPublisher.
func (p *eventPublisher) Publish(ctx context.Context, e ports.Event) error {
	ctx, done := p.t.call(ctx, portEvents, "Publish")
	err := p.next.Publish(ctx, e)
	done(err)
	return err
}

// InstrumentIdentityProvider returns next wrapped so that every call is traced and
// counted. Credentials that are not accepted are an error of the port and
// counted as one.
func (t *Telemetry) InstrumentIdentityProvider(next ports.IdentityProvider) ports.IdentityProvider {
	return &identityProvider{t: t, next: next}
}

type identityProvider struct {
	t    *Telemetry
	next ports.IdentityProvider
}

const portIdentityProvider = "identity_provider"

func (*identityProvider) instrumented() {}

// Authenticate implements ports.IdentityProvider.
func (p *identityProvider) Authenticate(ctx context.Context, username, password string) (string, error) {
	ctx, done := p.t.call(ctx, portIdentityProvider, "Authenticate")
	result, err := p.next.Authenticate(ctx, username, password)
	done(err)
	return result, err
}

// InstrumentClusterView returns next wrapped so that every call is traced and
// counted.
func (t *Telemetry) InstrumentClusterView(next ports.ClusterView) ports.ClusterView {
	return &clusterView{t: t, next: next}
}

type clusterView struct {
	t    *Telemetry
	next ports.ClusterView
}

const portClusterView = "cluster_view"

func (*clusterView) instrumented() {}

// Topology implements ports.ClusterView.
func (v *clusterView) Topology(ctx context.Context) (cluster.Topology, error) {
	ctx, done := v.t.call(ctx, portClusterView, "Topology")
	result, err := v.next.Topology(ctx)
	done(err)
	return result, err
}

// Nodes implements ports.ClusterView.
func (v *clusterView) Nodes(ctx context.Context) ([]cluster.Node, error) {
	ctx, done := v.t.call(ctx, portClusterView, "Nodes")
	result, err := v.next.Nodes(ctx)
	done(err)
	return result, err
}

// InstrumentPasswords returns next wrapped so that every call is counted and
// timed. The port takes no context, so a call has no span: it would be one
// that belongs to no trace. A password that is wrong is an answer, not an
// error.
func (t *Telemetry) InstrumentPasswords(next ports.PasswordHasher) ports.PasswordHasher {
	return &passwordHasher{t: t, next: next}
}

type passwordHasher struct {
	t    *Telemetry
	next ports.PasswordHasher
}

const portHasher = "passwords"

func (*passwordHasher) instrumented() {}

// Hash implements ports.PasswordHasher.
func (h *passwordHasher) Hash(password string) (string, error) {
	done := h.t.count(portHasher, "Hash")
	encoded, err := h.next.Hash(password)
	done(err)
	return encoded, err
}

// Verify implements ports.PasswordHasher.
func (h *passwordHasher) Verify(encoded, password string) (bool, error) {
	done := h.t.count(portHasher, "Verify")
	ok, err := h.next.Verify(encoded, password)
	done(err)
	return ok, err
}
