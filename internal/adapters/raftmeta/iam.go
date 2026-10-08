package raftmeta

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// Identity and access management in the replicated state. See
// docs/adr/0006-iam.md.
//
//	users     <name> -> userRecord
//	groups    <name> -> groupRecord
//	roles     <name> -> roleRecord
//	policies  <name> -> policyRecord
//	keys      <access key ID> -> keyRecord
//	sessions  <token hash> -> sessionRecord
//	idempotency <record ID> -> idemRecord
//	audit     <sequence, 8 bytes big endian> -> auditRecord
//
// Everything the state machine decides here it decides from the entry and
// the state alone, by rules that do not depend on the release: which roles
// and policies are built in is a property of a binary, so the proposer names
// the ones it vouches for in the entry (Known) and the state machine takes
// its word. Replicas of two releases therefore always agree.

func usersBucket() []byte    { return []byte("users") }
func groupsBucket() []byte   { return []byte("groups") }
func rolesBucket() []byte    { return []byte("roles") }
func policiesBucket() []byte { return []byte("policies") }
func keysBucket() []byte     { return []byte("keys") }
func sessionsBucket() []byte { return []byte("sessions") }

func idempotencyBucket() []byte { return []byte("idempotency") }
func auditBucket() []byte       { return []byte("audit") }

func iamBuckets() [][]byte {
	return [][]byte{usersBucket(), groupsBucket(), rolesBucket(), policiesBucket(), keysBucket(), sessionsBucket(), auditBucket(), idempotencyBucket()}
}

// IAM command types.
const (
	cmdPutUser        = "iam.user.put"
	cmdDeleteUser     = "iam.user.delete"
	cmdPutGroup       = "iam.group.put"
	cmdDeleteGroup    = "iam.group.delete"
	cmdPutRole        = "iam.role.put"
	cmdDeleteRole     = "iam.role.delete"
	cmdPutPolicy      = "iam.policy.put"
	cmdDeletePolicy   = "iam.policy.delete"
	cmdCreateKey      = "iam.key.create"
	cmdDeleteKey      = "iam.key.delete"
	cmdTouchKey       = "iam.key.touch"
	cmdPutSession     = "session.put"
	cmdDeleteSession  = "session.delete"
	cmdExpireSessions = "session.expire"
	cmdAppendAudit    = "audit.append"
	cmdBeginIdem      = "idempotency.begin"
	cmdCompleteIdem   = "idempotency.complete"
	cmdDeleteIdem     = "idempotency.delete"
	cmdExpireIdem     = "idempotency.expire"
)

func isIAMCommand(kind string) bool {
	switch kind {
	case cmdPutUser, cmdDeleteUser, cmdPutGroup, cmdDeleteGroup, cmdPutRole, cmdDeleteRole, cmdPutPolicy, cmdDeletePolicy,
		cmdCreateKey, cmdDeleteKey, cmdTouchKey, cmdPutSession, cmdDeleteSession, cmdExpireSessions, cmdAppendAudit,
		cmdBeginIdem, cmdCompleteIdem, cmdDeleteIdem, cmdExpireIdem:
		return true
	default:
		return false
	}
}

// iamCmd is the payload of every IAM command. Which fields are set depends
// on the command type.
type iamCmd struct {
	// Create distinguishes adding from replacing in a put.
	Create bool `json:"create,omitempty"`
	// Name is the entity a delete, or a touch, is about.
	Name string `json:"name,omitempty"`
	// Known lists the names among the entity's references that the
	// proposer vouches for as built in.
	Known   []string       `json:"known,omitempty"`
	User    *userRecord    `json:"user,omitempty"`
	Group   *groupRecord   `json:"group,omitempty"`
	Role    *roleRecord    `json:"role,omitempty"`
	Policy  *policyRecord  `json:"policy,omitempty"`
	Key     *keyRecord     `json:"key,omitempty"`
	Session *sessionRecord `json:"session,omitempty"`
	Idem    *idemRecord    `json:"idempotency,omitempty"`
	// At is the time of a touch or of an expiry sweep.
	At time.Time `json:"at,omitzero"`
	// Audit is the record of the change, appended in the same transaction.
	Audit *auditRecord `json:"audit,omitempty"`
}

type userRecord struct {
	Name         string    `json:"name"`
	Groups       []string  `json:"groups,omitempty"`
	Roles        []string  `json:"roles,omitempty"`
	Disabled     bool      `json:"disabled,omitempty"`
	PasswordHash string    `json:"password_hash,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type groupRecord struct {
	Name      string    `json:"name"`
	Roles     []string  `json:"roles,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type roleRecord struct {
	Name      string    `json:"name"`
	Policies  []string  `json:"policies,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type policyRecord struct {
	Name      string          `json:"name"`
	Document  json.RawMessage `json:"document"`
	CreatedAt time.Time       `json:"created_at"`
}

type keyRecord struct {
	AccessKeyID  string          `json:"access_key_id"`
	Owner        string          `json:"owner"`
	KMSKeyID     string          `json:"kms_key_id"`
	WrappedKey   []byte          `json:"wrapped_key"`
	Nonce        []byte          `json:"nonce"`
	Ciphertext   []byte          `json:"ciphertext"`
	Description  string          `json:"description,omitempty"`
	InlinePolicy json.RawMessage `json:"inline_policy,omitempty"`
	ExpiresAt    time.Time       `json:"expires_at,omitzero"`
	CreatedAt    time.Time       `json:"created_at"`
	LastUsedAt   time.Time       `json:"last_used_at,omitzero"`
}

// idemRecord is a request that carried an idempotency key, and its answer.
type idemRecord struct {
	ID          string    `json:"id"`
	Fingerprint string    `json:"fingerprint"`
	Done        bool      `json:"done,omitempty"`
	Status      int       `json:"status,omitempty"`
	ContentType string    `json:"content_type,omitempty"`
	KMSKeyID    string    `json:"kms_key_id,omitempty"`
	WrappedKey  []byte    `json:"wrapped_key,omitempty"`
	Nonce       []byte    `json:"nonce,omitempty"`
	Ciphertext  []byte    `json:"ciphertext,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func idemToRecord(r ports.IdempotencyRecord) *idemRecord {
	return &idemRecord{
		ID: r.ID, Fingerprint: r.Fingerprint, Done: r.Done, Status: r.Status, ContentType: r.ContentType,
		KMSKeyID: r.Body.KMSKeyID, WrappedKey: r.Body.WrappedKey, Nonce: r.Body.Nonce, Ciphertext: r.Body.Ciphertext,
		CreatedAt: r.CreatedAt.UTC(),
	}
}

func (r idemRecord) toPort() ports.IdempotencyRecord {
	return ports.IdempotencyRecord{
		ID: r.ID, Fingerprint: r.Fingerprint, Done: r.Done, Status: r.Status, ContentType: r.ContentType,
		Body:      iam.SealedSecret{KMSKeyID: r.KMSKeyID, WrappedKey: r.WrappedKey, Nonce: r.Nonce, Ciphertext: r.Ciphertext},
		CreatedAt: r.CreatedAt,
	}
}

type sessionRecord struct {
	TokenHash string    `json:"token_hash"`
	User      string    `json:"user"`
	CSRFHash  string    `json:"csrf_hash"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type auditRecord struct {
	Sequence     uint64            `json:"sequence,omitempty"`
	Time         time.Time         `json:"time"`
	Actor        string            `json:"actor"`
	AccessKeyID  string            `json:"access_key_id,omitempty"`
	Action       string            `json:"action"`
	Resource     string            `json:"resource,omitempty"`
	Outcome      string            `json:"outcome"`
	SourceIP     string            `json:"source_ip,omitempty"`
	RequestID    string            `json:"request_id,omitempty"`
	Details      map[string]string `json:"details,omitempty"`
	PreviousHash string            `json:"previous_hash,omitempty"`
	Hash         string            `json:"hash,omitempty"`
}

func auditToRecord(r audit.Record) *auditRecord {
	return &auditRecord{
		Sequence: r.Sequence, Time: r.Time.UTC(), Actor: r.Actor, AccessKeyID: r.AccessKeyID, Action: r.Action,
		Resource: r.Resource, Outcome: string(r.Outcome), SourceIP: r.SourceIP, RequestID: r.RequestID, Details: r.Details,
		PreviousHash: r.PreviousHash, Hash: r.Hash,
	}
}

func (r auditRecord) toDomain() audit.Record {
	return audit.Record{
		Sequence: r.Sequence, Time: r.Time, Actor: r.Actor, AccessKeyID: r.AccessKeyID, Action: r.Action,
		Resource: r.Resource, Outcome: audit.Outcome(r.Outcome), SourceIP: r.SourceIP, RequestID: r.RequestID, Details: r.Details,
		PreviousHash: r.PreviousHash, Hash: r.Hash,
	}
}

func userToRecord(u iam.User) *userRecord {
	return &userRecord{
		Name: u.Name, Groups: u.Groups, Roles: u.Roles, Disabled: u.Disabled, PasswordHash: u.PasswordHash, CreatedAt: u.CreatedAt.UTC(),
	}
}

func (r userRecord) toDomain() iam.User {
	return iam.User{
		Name: r.Name, Groups: r.Groups, Roles: r.Roles, Disabled: r.Disabled, PasswordHash: r.PasswordHash, CreatedAt: r.CreatedAt,
	}
}

func keyToRecord(k iam.APIKey) (*keyRecord, error) {
	record := &keyRecord{
		AccessKeyID: k.AccessKeyID, Owner: k.Owner, KMSKeyID: k.Secret.KMSKeyID, WrappedKey: k.Secret.WrappedKey,
		Nonce: k.Secret.Nonce, Ciphertext: k.Secret.Ciphertext, Description: k.Description,
		ExpiresAt: k.ExpiresAt.UTC(), CreatedAt: k.CreatedAt.UTC(), LastUsedAt: k.LastUsedAt.UTC(),
	}
	if k.ExpiresAt.IsZero() {
		record.ExpiresAt = time.Time{}
	}
	if k.LastUsedAt.IsZero() {
		record.LastUsedAt = time.Time{}
	}
	if k.InlinePolicy != nil {
		raw, err := json.Marshal(*k.InlinePolicy)
		if err != nil {
			return nil, err
		}
		record.InlinePolicy = raw
	}
	return record, nil
}

func (r keyRecord) toDomain() (iam.APIKey, error) {
	key := iam.APIKey{
		AccessKeyID: r.AccessKeyID, Owner: r.Owner,
		Secret:      iam.SealedSecret{KMSKeyID: r.KMSKeyID, WrappedKey: r.WrappedKey, Nonce: r.Nonce, Ciphertext: r.Ciphertext},
		Description: r.Description, ExpiresAt: r.ExpiresAt, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt,
	}
	if len(r.InlinePolicy) > 0 {
		doc, err := iam.ParsePolicy(r.InlinePolicy)
		if err != nil {
			return iam.APIKey{}, fmt.Errorf("%w: stored inline policy of key %s: %w", domain.ErrIntegrity, r.AccessKeyID, err)
		}
		key.InlinePolicy = &doc
	}
	return key, nil
}

// --- state machine ---

func getJSON(tx *bbolt.Tx, bucket []byte, name string, into any) (bool, error) {
	raw := tx.Bucket(bucket).Get([]byte(name))
	if raw == nil {
		return false, nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return false, fmt.Errorf("%w: stored %s %q: %w", domain.ErrIntegrity, bucket, name, err)
	}
	return true, nil
}

func putJSON(tx *bbolt.Tx, bucket []byte, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return tx.Bucket(bucket).Put([]byte(name), data)
}

func exists(tx *bbolt.Tx, bucket []byte, name string) bool {
	return tx.Bucket(bucket).Get([]byte(name)) != nil
}

// missing returns the first of names that neither exists in bucket nor is
// vouched for.
func missing(tx *bbolt.Tx, bucket []byte, names, known []string) string {
	for _, name := range names {
		if !exists(tx, bucket, name) && !slices.Contains(known, name) {
			return name
		}
	}
	return ""
}

// lastAudit returns the newest audit record, or the zero record.
func lastAudit(tx *bbolt.Tx) (audit.Record, error) {
	_, raw := tx.Bucket(auditBucket()).Cursor().Last()
	if raw == nil {
		return audit.Record{}, nil
	}
	var record auditRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return audit.Record{}, fmt.Errorf("%w: stored audit record: %w", domain.ErrIntegrity, err)
	}
	return record.toDomain(), nil
}

// appendAudit seals a record onto the chain and stores it.
func appendAudit(tx *bbolt.Tx, record auditRecord) (ports.Event, error) {
	previous, err := lastAudit(tx)
	if err != nil {
		return ports.Event{}, err
	}
	sealed := record.toDomain().Seal(previous)
	data, err := json.Marshal(auditToRecord(sealed))
	if err != nil {
		return ports.Event{}, err
	}
	if err := tx.Bucket(auditBucket()).Put(binary.BigEndian.AppendUint64(nil, sealed.Sequence), data); err != nil {
		return ports.Event{}, err
	}
	return ports.Event{Type: ports.EventAuditRecorded, Time: sealed.Time, Data: ports.AuditEvent{
		Sequence: sealed.Sequence, Actor: sealed.Actor, Action: sealed.Action, Resource: sealed.Resource,
		Outcome: string(sealed.Outcome), Hash: sealed.Hash,
	}}, nil
}

// applyIAM performs one IAM command. A change that is refused is not
// audited here: the refusal is the proposer's to record.
func applyIAM(tx *bbolt.Tx, kind string, c iamCmd) (applyResult, error) {
	result, err := applyIAMChange(tx, kind, c)
	if err != nil || result.err != nil || c.Audit == nil {
		return result, err
	}
	event, err := appendAudit(tx, *c.Audit)
	if err != nil {
		return applyResult{}, err
	}
	result.events = append(result.events, event)
	return result, nil
}

func applyIAMChange(tx *bbolt.Tx, kind string, c iamCmd) (applyResult, error) {
	switch kind {
	case cmdPutUser:
		u := *c.User
		if err := iam.ValidateName(u.Name); err != nil {
			return applyResult{err: err}, nil
		}
		if name := missing(tx, groupsBucket(), u.Groups, nil); name != "" {
			return refuse(domain.ErrEntityNotFound, "group %s", name)
		}
		if name := missing(tx, rolesBucket(), u.Roles, c.Known); name != "" {
			return refuse(domain.ErrEntityNotFound, "role %s", name)
		}
		return putEntity(tx, usersBucket(), u.Name, "user", c.Create, u)
	case cmdDeleteUser:
		if !exists(tx, usersBucket(), c.Name) {
			return refuse(domain.ErrEntityNotFound, "user %s", c.Name)
		}
		// A user's keys and sessions go with the user.
		if err := deleteWhere(tx, keysBucket(), func(raw []byte) bool {
			var k keyRecord
			return json.Unmarshal(raw, &k) == nil && k.Owner == c.Name
		}); err != nil {
			return applyResult{}, err
		}
		if err := deleteWhere(tx, sessionsBucket(), func(raw []byte) bool {
			var s sessionRecord
			return json.Unmarshal(raw, &s) == nil && s.User == c.Name
		}); err != nil {
			return applyResult{}, err
		}
		return applyResult{}, tx.Bucket(usersBucket()).Delete([]byte(c.Name))
	case cmdPutGroup:
		g := *c.Group
		if err := iam.ValidateName(g.Name); err != nil {
			return applyResult{err: err}, nil
		}
		if name := missing(tx, rolesBucket(), g.Roles, c.Known); name != "" {
			return refuse(domain.ErrEntityNotFound, "role %s", name)
		}
		return putEntity(tx, groupsBucket(), g.Name, "group", c.Create, g)
	case cmdDeleteGroup:
		return deleteEntity(tx, groupsBucket(), c.Name, "group", usersBucket(), func(raw []byte) bool {
			var u userRecord
			return json.Unmarshal(raw, &u) == nil && slices.Contains(u.Groups, c.Name)
		})
	case cmdPutRole:
		r := *c.Role
		if err := iam.ValidateName(r.Name); err != nil {
			return applyResult{err: err}, nil
		}
		if name := missing(tx, policiesBucket(), r.Policies, c.Known); name != "" {
			return refuse(domain.ErrEntityNotFound, "policy %s", name)
		}
		return putEntity(tx, rolesBucket(), r.Name, "role", c.Create, r)
	case cmdDeleteRole:
		used := func(raw []byte) bool {
			var holder struct {
				Roles []string `json:"roles"`
			}
			return json.Unmarshal(raw, &holder) == nil && slices.Contains(holder.Roles, c.Name)
		}
		if inUse, err := anyWhere(tx, groupsBucket(), used); err != nil || inUse {
			if err != nil {
				return applyResult{}, err
			}
			return refuse(domain.ErrEntityInUse, "role %s is held by a group", c.Name)
		}
		return deleteEntity(tx, rolesBucket(), c.Name, "role", usersBucket(), used)
	case cmdPutPolicy:
		p := *c.Policy
		if err := iam.ValidateName(p.Name); err != nil {
			return applyResult{err: err}, nil
		}
		return putEntity(tx, policiesBucket(), p.Name, "policy", c.Create, p)
	case cmdDeletePolicy:
		return deleteEntity(tx, policiesBucket(), c.Name, "policy", rolesBucket(), func(raw []byte) bool {
			var r roleRecord
			return json.Unmarshal(raw, &r) == nil && slices.Contains(r.Policies, c.Name)
		})
	case cmdCreateKey:
		k := *c.Key
		if err := iam.ValidateAccessKeyID(k.AccessKeyID); err != nil {
			return applyResult{err: err}, nil
		}
		if !exists(tx, usersBucket(), k.Owner) {
			return refuse(domain.ErrEntityNotFound, "user %s", k.Owner)
		}
		return putEntity(tx, keysBucket(), k.AccessKeyID, "API key", true, k)
	case cmdDeleteKey:
		if !exists(tx, keysBucket(), c.Name) {
			return refuse(domain.ErrEntityNotFound, "API key %s", c.Name)
		}
		return applyResult{}, tx.Bucket(keysBucket()).Delete([]byte(c.Name))
	case cmdTouchKey:
		var k keyRecord
		if found, err := getJSON(tx, keysBucket(), c.Name, &k); err != nil || !found {
			return applyResult{}, err
		}
		if !c.At.After(k.LastUsedAt) {
			return applyResult{}, nil
		}
		k.LastUsedAt = c.At.UTC()
		return applyResult{}, putJSON(tx, keysBucket(), c.Name, k)
	case cmdPutSession:
		s := *c.Session
		if !exists(tx, usersBucket(), s.User) {
			return refuse(domain.ErrEntityNotFound, "user %s", s.User)
		}
		return applyResult{}, putJSON(tx, sessionsBucket(), s.TokenHash, s)
	case cmdDeleteSession:
		return applyResult{}, tx.Bucket(sessionsBucket()).Delete([]byte(c.Name))
	case cmdExpireSessions:
		return applyResult{}, deleteWhere(tx, sessionsBucket(), func(raw []byte) bool {
			var s sessionRecord
			return json.Unmarshal(raw, &s) == nil && s.ExpiresAt.Before(c.At)
		})
	case cmdAppendAudit:
		// The record is the command; applyIAM appends it.
		return applyResult{}, nil
	case cmdBeginIdem:
		if exists(tx, idempotencyBucket(), c.Idem.ID) {
			return refuse(domain.ErrEntityExists, "idempotency record")
		}
		return applyResult{}, putJSON(tx, idempotencyBucket(), c.Idem.ID, *c.Idem)
	case cmdCompleteIdem:
		var current idemRecord
		found, err := getJSON(tx, idempotencyBucket(), c.Idem.ID, &current)
		// Only the request that began the record completes it: one that
		// was forgotten and begun again belongs to somebody else.
		if err != nil || !found || current.Done || !current.CreatedAt.Equal(c.Idem.CreatedAt) {
			return applyResult{}, err
		}
		return applyResult{}, putJSON(tx, idempotencyBucket(), c.Idem.ID, *c.Idem)
	case cmdDeleteIdem:
		var current idemRecord
		found, err := getJSON(tx, idempotencyBucket(), c.Name, &current)
		// Only the record that was meant: one begun since is another
		// request's reservation.
		if err != nil || !found || !current.CreatedAt.Equal(c.At) {
			return applyResult{}, err
		}
		return applyResult{}, tx.Bucket(idempotencyBucket()).Delete([]byte(c.Name))
	case cmdExpireIdem:
		return applyResult{}, deleteWhere(tx, idempotencyBucket(), func(raw []byte) bool {
			var r idemRecord
			return json.Unmarshal(raw, &r) == nil && r.CreatedAt.Before(c.At)
		})
	default:
		return applyResult{}, fmt.Errorf("%w: metadata command %q", domain.ErrUnsupportedFormat, kind)
	}
}

func putEntity(tx *bbolt.Tx, bucket []byte, name, what string, create bool, value any) (applyResult, error) {
	switch present := exists(tx, bucket, name); {
	case create && present:
		return refuse(domain.ErrEntityExists, "%s %s", what, name)
	case !create && !present:
		return refuse(domain.ErrEntityNotFound, "%s %s", what, name)
	}
	return applyResult{}, putJSON(tx, bucket, name, value)
}

func deleteEntity(tx *bbolt.Tx, bucket []byte, name, what string, holders []byte, holds func(raw []byte) bool) (applyResult, error) {
	if !exists(tx, bucket, name) {
		return refuse(domain.ErrEntityNotFound, "%s %s", what, name)
	}
	inUse, err := anyWhere(tx, holders, holds)
	if err != nil {
		return applyResult{}, err
	}
	if inUse {
		return refuse(domain.ErrEntityInUse, "%s %s is still referred to", what, name)
	}
	return applyResult{}, tx.Bucket(bucket).Delete([]byte(name))
}

func anyWhere(tx *bbolt.Tx, bucket []byte, match func(raw []byte) bool) (bool, error) {
	found := false
	err := tx.Bucket(bucket).ForEach(func(_, raw []byte) error {
		found = found || match(raw)
		return nil
	})
	return found, err
}

func deleteWhere(tx *bbolt.Tx, bucket []byte, match func(raw []byte) bool) error {
	var keys [][]byte
	b := tx.Bucket(bucket)
	if err := b.ForEach(func(key, raw []byte) error {
		if match(raw) {
			keys = append(keys, bytes.Clone(key))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, key := range keys {
		if err := b.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

// --- store ---

var (
	_ ports.IAMStore     = (*Store)(nil)
	_ ports.AuditLog     = (*Store)(nil)
	_ ports.SessionStore = (*Store)(nil)
)

// proposeIAM commits an IAM command. The audit record, when it has an
// actor, is appended in the same entry.
func (s *Store) proposeIAM(ctx context.Context, op, kind string, c iamCmd, record audit.Record) error {
	if record.Actor != "" {
		if err := record.Validate(); err != nil {
			return fmt.Errorf("raftmeta.%s: %w", op, err)
		}
		c.Audit = auditToRecord(record)
	}
	if _, err := s.propose(ctx, entry{Type: kind, IAM: &c}); err != nil {
		return fmt.Errorf("raftmeta.%s: %w", op, err)
	}
	return nil
}

// known returns the names among refs that are built in.
func known(refs []string) []string {
	var names []string
	for _, name := range refs {
		if iam.IsBuiltIn(name) {
			names = append(names, name)
		}
	}
	return names
}

// PutUser implements ports.IAMStore.
func (s *Store) PutUser(ctx context.Context, u iam.User, create bool, record audit.Record) error {
	if err := u.Validate(); err != nil {
		return fmt.Errorf("raftmeta.PutUser: %w", err)
	}
	return s.proposeIAM(ctx, "PutUser", cmdPutUser, iamCmd{Create: create, User: userToRecord(u), Known: known(u.Roles)}, record)
}

// DeleteUser implements ports.IAMStore.
func (s *Store) DeleteUser(ctx context.Context, name string, record audit.Record) error {
	return s.proposeIAM(ctx, "DeleteUser", cmdDeleteUser, iamCmd{Name: name}, record)
}

// GetUser implements ports.IAMStore.
func (s *Store) GetUser(ctx context.Context, name string) (iam.User, error) {
	var record userRecord
	if err := s.getEntity(ctx, "GetUser", usersBucket(), name, "user", &record); err != nil {
		return iam.User{}, err
	}
	return record.toDomain(), nil
}

// ListUsers implements ports.IAMStore.
func (s *Store) ListUsers(ctx context.Context) ([]iam.User, error) {
	var users []iam.User
	err := listEntities(ctx, s, "ListUsers", usersBucket(), func(r userRecord) error {
		users = append(users, r.toDomain())
		return nil
	})
	return users, err
}

// PutGroup implements ports.IAMStore.
func (s *Store) PutGroup(ctx context.Context, g iam.Group, create bool, record audit.Record) error {
	if err := g.Validate(); err != nil {
		return fmt.Errorf("raftmeta.PutGroup: %w", err)
	}
	c := iamCmd{Create: create, Group: &groupRecord{Name: g.Name, Roles: g.Roles, CreatedAt: g.CreatedAt.UTC()}, Known: known(g.Roles)}
	return s.proposeIAM(ctx, "PutGroup", cmdPutGroup, c, record)
}

// DeleteGroup implements ports.IAMStore.
func (s *Store) DeleteGroup(ctx context.Context, name string, record audit.Record) error {
	return s.proposeIAM(ctx, "DeleteGroup", cmdDeleteGroup, iamCmd{Name: name}, record)
}

// GetGroup implements ports.IAMStore.
func (s *Store) GetGroup(ctx context.Context, name string) (iam.Group, error) {
	var record groupRecord
	if err := s.getEntity(ctx, "GetGroup", groupsBucket(), name, "group", &record); err != nil {
		return iam.Group{}, err
	}
	return iam.Group{Name: record.Name, Roles: record.Roles, CreatedAt: record.CreatedAt}, nil
}

// ListGroups implements ports.IAMStore.
func (s *Store) ListGroups(ctx context.Context) ([]iam.Group, error) {
	var groups []iam.Group
	err := listEntities(ctx, s, "ListGroups", groupsBucket(), func(r groupRecord) error {
		groups = append(groups, iam.Group{Name: r.Name, Roles: r.Roles, CreatedAt: r.CreatedAt})
		return nil
	})
	return groups, err
}

// PutRole implements ports.IAMStore.
func (s *Store) PutRole(ctx context.Context, r iam.Role, create bool, record audit.Record) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("raftmeta.PutRole: %w", err)
	}
	if iam.IsBuiltIn(r.Name) {
		return fmt.Errorf("raftmeta.PutRole: %w: %s is a built-in role", domain.ErrInvalidArgument, r.Name)
	}
	c := iamCmd{Create: create, Role: &roleRecord{Name: r.Name, Policies: r.Policies, CreatedAt: r.CreatedAt.UTC()}, Known: known(r.Policies)}
	return s.proposeIAM(ctx, "PutRole", cmdPutRole, c, record)
}

// DeleteRole implements ports.IAMStore.
func (s *Store) DeleteRole(ctx context.Context, name string, record audit.Record) error {
	if iam.IsBuiltIn(name) {
		return fmt.Errorf("raftmeta.DeleteRole: %w: %s is a built-in role", domain.ErrInvalidArgument, name)
	}
	return s.proposeIAM(ctx, "DeleteRole", cmdDeleteRole, iamCmd{Name: name}, record)
}

// GetRole implements ports.IAMStore. Built-in roles are found too.
func (s *Store) GetRole(ctx context.Context, name string) (iam.Role, error) {
	if role, ok := iam.BuiltInRoles()[name]; ok {
		return role, nil
	}
	var record roleRecord
	if err := s.getEntity(ctx, "GetRole", rolesBucket(), name, "role", &record); err != nil {
		return iam.Role{}, err
	}
	return iam.Role{Name: record.Name, Policies: record.Policies, CreatedAt: record.CreatedAt}, nil
}

// ListRoles implements ports.IAMStore: the built-in roles, then the others,
// each by name.
func (s *Store) ListRoles(ctx context.Context) ([]iam.Role, error) {
	var roles []iam.Role
	builtIn := iam.BuiltInRoles()
	for _, name := range slices.Sorted(mapKeys(builtIn)) {
		roles = append(roles, builtIn[name])
	}
	err := listEntities(ctx, s, "ListRoles", rolesBucket(), func(r roleRecord) error {
		if !iam.IsBuiltIn(r.Name) {
			roles = append(roles, iam.Role{Name: r.Name, Policies: r.Policies, CreatedAt: r.CreatedAt})
		}
		return nil
	})
	return roles, err
}

func mapKeys[V any](m map[string]V) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}

// PutPolicy implements ports.IAMStore.
func (s *Store) PutPolicy(ctx context.Context, p iam.Policy, create bool, record audit.Record) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("raftmeta.PutPolicy: %w", err)
	}
	if iam.IsBuiltIn(p.Name) {
		return fmt.Errorf("raftmeta.PutPolicy: %w: %s is a built-in policy", domain.ErrInvalidArgument, p.Name)
	}
	document, err := json.Marshal(p.Document)
	if err != nil {
		return fmt.Errorf("raftmeta.PutPolicy: %w", err)
	}
	c := iamCmd{Create: create, Policy: &policyRecord{Name: p.Name, Document: document, CreatedAt: p.CreatedAt.UTC()}}
	return s.proposeIAM(ctx, "PutPolicy", cmdPutPolicy, c, record)
}

// DeletePolicy implements ports.IAMStore.
func (s *Store) DeletePolicy(ctx context.Context, name string, record audit.Record) error {
	if iam.IsBuiltIn(name) {
		return fmt.Errorf("raftmeta.DeletePolicy: %w: %s is a built-in policy", domain.ErrInvalidArgument, name)
	}
	return s.proposeIAM(ctx, "DeletePolicy", cmdDeletePolicy, iamCmd{Name: name}, record)
}

func policyFromRecord(r policyRecord) (iam.Policy, error) {
	doc, err := iam.ParsePolicy(r.Document)
	if err != nil {
		return iam.Policy{}, fmt.Errorf("%w: stored policy %s: %w", domain.ErrIntegrity, r.Name, err)
	}
	return iam.Policy{Name: r.Name, Document: doc, CreatedAt: r.CreatedAt}, nil
}

// GetPolicy implements ports.IAMStore. Built-in policies are found too.
func (s *Store) GetPolicy(ctx context.Context, name string) (iam.Policy, error) {
	if doc, ok := iam.BuiltInPolicies()[name]; ok {
		return iam.Policy{Name: name, Document: doc, BuiltIn: true}, nil
	}
	var record policyRecord
	if err := s.getEntity(ctx, "GetPolicy", policiesBucket(), name, "policy", &record); err != nil {
		return iam.Policy{}, err
	}
	policy, err := policyFromRecord(record)
	if err != nil {
		return iam.Policy{}, fmt.Errorf("raftmeta.GetPolicy: %w", err)
	}
	return policy, nil
}

// ListPolicies implements ports.IAMStore.
func (s *Store) ListPolicies(ctx context.Context) ([]iam.Policy, error) {
	var policies []iam.Policy
	builtIn := iam.BuiltInPolicies()
	for _, name := range slices.Sorted(mapKeys(builtIn)) {
		policies = append(policies, iam.Policy{Name: name, Document: builtIn[name], BuiltIn: true})
	}
	err := listEntities(ctx, s, "ListPolicies", policiesBucket(), func(r policyRecord) error {
		if iam.IsBuiltIn(r.Name) {
			return nil
		}
		policy, err := policyFromRecord(r)
		if err != nil {
			return err
		}
		policies = append(policies, policy)
		return nil
	})
	return policies, err
}

// CreateAPIKey implements ports.IAMStore.
func (s *Store) CreateAPIKey(ctx context.Context, k iam.APIKey, record audit.Record) error {
	if err := k.Validate(); err != nil {
		return fmt.Errorf("raftmeta.CreateAPIKey: %w", err)
	}
	key, err := keyToRecord(k)
	if err != nil {
		return fmt.Errorf("raftmeta.CreateAPIKey: %w", err)
	}
	return s.proposeIAM(ctx, "CreateAPIKey", cmdCreateKey, iamCmd{Create: true, Key: key}, record)
}

// DeleteAPIKey implements ports.IAMStore.
func (s *Store) DeleteAPIKey(ctx context.Context, accessKeyID string, record audit.Record) error {
	return s.proposeIAM(ctx, "DeleteAPIKey", cmdDeleteKey, iamCmd{Name: accessKeyID}, record)
}

// ListAPIKeys implements ports.IAMStore.
func (s *Store) ListAPIKeys(ctx context.Context, owner string) ([]iam.APIKey, error) {
	var keys []iam.APIKey
	err := listEntities(ctx, s, "ListAPIKeys", keysBucket(), func(r keyRecord) error {
		if owner != "" && r.Owner != owner {
			return nil
		}
		key, err := r.toDomain()
		if err != nil {
			return err
		}
		keys = append(keys, key)
		return nil
	})
	return keys, err
}

// TouchAPIKey implements ports.IAMStore.
func (s *Store) TouchAPIKey(ctx context.Context, accessKeyID string, usedAt time.Time) error {
	return s.proposeIAM(ctx, "TouchAPIKey", cmdTouchKey, iamCmd{Name: accessKeyID, At: usedAt.UTC()}, audit.Record{})
}

// ResolveIdentity implements ports.IAMStore.
func (s *Store) ResolveIdentity(ctx context.Context, accessKeyID string) (iam.Identity, error) {
	var identity iam.Identity
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		var key keyRecord
		found, err := getJSON(tx, keysBucket(), accessKeyID, &key)
		if err != nil {
			return err
		}
		if !found {
			return domain.ErrCredentialNotFound
		}
		if identity.Key, err = key.toDomain(); err != nil {
			return err
		}
		identity.User, identity.Policies, err = resolveUser(tx, key.Owner)
		return err
	})
	if err != nil {
		return iam.Identity{}, fmt.Errorf("raftmeta.ResolveIdentity: %w", err)
	}
	return identity, nil
}

// ResolveUser implements ports.IAMStore.
func (s *Store) ResolveUser(ctx context.Context, name string) (iam.Identity, error) {
	var identity iam.Identity
	err := s.view(ctx, func(tx *bbolt.Tx) (err error) {
		identity.User, identity.Policies, err = resolveUser(tx, name)
		return err
	})
	if err != nil {
		return iam.Identity{}, fmt.Errorf("raftmeta.ResolveUser: %w", err)
	}
	return identity, nil
}

// resolveUser reads a user and the documents of every policy of every role
// the user holds, directly or through a group. A role or policy this binary
// cannot find contributes nothing: what is unknown allows nothing.
func resolveUser(tx *bbolt.Tx, name string) (iam.User, []iam.Document, error) {
	var record userRecord
	found, err := getJSON(tx, usersBucket(), name, &record)
	if err != nil {
		return iam.User{}, nil, err
	}
	if !found {
		return iam.User{}, nil, fmt.Errorf("%w: user %s", domain.ErrEntityNotFound, name)
	}
	user := record.toDomain()
	var groups []iam.Group
	for _, groupName := range user.Groups {
		var g groupRecord
		if found, err := getJSON(tx, groupsBucket(), groupName, &g); err != nil {
			return iam.User{}, nil, err
		} else if found {
			groups = append(groups, iam.Group{Name: g.Name, Roles: g.Roles})
		}
	}
	builtIn := iam.BuiltInPolicies()
	var policyNames []string
	for _, roleName := range iam.RoleNames(user, groups) {
		if iam.IsBuiltIn(roleName) {
			policyNames = append(policyNames, roleName)
			continue
		}
		var r roleRecord
		if found, err := getJSON(tx, rolesBucket(), roleName, &r); err != nil {
			return iam.User{}, nil, err
		} else if found {
			policyNames = append(policyNames, r.Policies...)
		}
	}
	slices.Sort(policyNames)
	var documents []iam.Document
	for _, policyName := range slices.Compact(policyNames) {
		if doc, ok := builtIn[policyName]; ok {
			documents = append(documents, doc)
			continue
		}
		var p policyRecord
		found, err := getJSON(tx, policiesBucket(), policyName, &p)
		if err != nil {
			return iam.User{}, nil, err
		}
		if !found {
			continue
		}
		policy, err := policyFromRecord(p)
		if err != nil {
			return iam.User{}, nil, err
		}
		documents = append(documents, policy.Document)
	}
	return user, documents, nil
}

func (s *Store) getEntity(ctx context.Context, op string, bucket []byte, name, what string, into any) error {
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		found, err := getJSON(tx, bucket, name, into)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: %s %s", domain.ErrEntityNotFound, what, name)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("raftmeta.%s: %w", op, err)
	}
	return nil
}

func listEntities[R any](ctx context.Context, s *Store, op string, bucket []byte, visit func(R) error) error {
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return tx.Bucket(bucket).ForEach(func(key, raw []byte) error {
			var record R
			if err := json.Unmarshal(raw, &record); err != nil {
				return fmt.Errorf("%w: stored %s %q: %w", domain.ErrIntegrity, bucket, key, err)
			}
			return visit(record)
		})
	})
	if err != nil {
		return fmt.Errorf("raftmeta.%s: %w", op, err)
	}
	return nil
}

// AppendAudit implements ports.AuditLog.
func (s *Store) AppendAudit(ctx context.Context, record audit.Record) error {
	if record.Actor == "" {
		return fmt.Errorf("raftmeta.AppendAudit: %w: an audit record needs an actor", domain.ErrInvalidArgument)
	}
	return s.proposeIAM(ctx, "AppendAudit", cmdAppendAudit, iamCmd{}, record)
}

// maxAuditPage bounds one ListAudit.
const maxAuditPage = 1000

// ListAudit implements ports.AuditLog.
func (s *Store) ListAudit(ctx context.Context, after uint64, limit int) ([]audit.Record, error) {
	if limit <= 0 || limit > maxAuditPage {
		limit = maxAuditPage
	}
	var records []audit.Record
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		cursor := tx.Bucket(auditBucket()).Cursor()
		if after == ^uint64(0) {
			return nil
		}
		for key, raw := cursor.Seek(binary.BigEndian.AppendUint64(nil, after+1)); key != nil && len(records) < limit; key, raw = cursor.Next() {
			var record auditRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return fmt.Errorf("%w: stored audit record: %w", domain.ErrIntegrity, err)
			}
			records = append(records, record.toDomain())
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.ListAudit: %w", err)
	}
	return records, nil
}

// PutSession implements ports.SessionStore.
func (s *Store) PutSession(ctx context.Context, session ports.Session, record audit.Record) error {
	c := iamCmd{Session: &sessionRecord{
		TokenHash: session.TokenHash, User: session.User, CSRFHash: session.CSRFHash,
		CreatedAt: session.CreatedAt.UTC(), ExpiresAt: session.ExpiresAt.UTC(),
	}}
	return s.proposeIAM(ctx, "PutSession", cmdPutSession, c, record)
}

// GetSession implements ports.SessionStore.
func (s *Store) GetSession(ctx context.Context, tokenHash string) (ports.Session, error) {
	var record sessionRecord
	if err := s.getEntity(ctx, "GetSession", sessionsBucket(), tokenHash, "session", &record); err != nil {
		return ports.Session{}, err
	}
	return ports.Session{
		TokenHash: record.TokenHash, User: record.User, CSRFHash: record.CSRFHash, CreatedAt: record.CreatedAt, ExpiresAt: record.ExpiresAt,
	}, nil
}

// DeleteSession implements ports.SessionStore.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string, record audit.Record) error {
	return s.proposeIAM(ctx, "DeleteSession", cmdDeleteSession, iamCmd{Name: tokenHash}, record)
}

// DeleteExpiredSessions implements ports.SessionStore.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int, error) {
	expired := 0
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return tx.Bucket(sessionsBucket()).ForEach(func(_, raw []byte) error {
			var record sessionRecord
			if json.Unmarshal(raw, &record) == nil && record.ExpiresAt.Before(now) {
				expired++
			}
			return nil
		})
	})
	if err != nil {
		return 0, fmt.Errorf("raftmeta.DeleteExpiredSessions: %w", err)
	}
	if expired == 0 {
		return 0, nil
	}
	return expired, s.proposeIAM(ctx, "DeleteExpiredSessions", cmdExpireSessions, iamCmd{At: now.UTC()}, audit.Record{})
}

var _ ports.IdempotencyStore = (*Store)(nil)

// maxIdempotentBody bounds the sealed answer an idempotency record carries.
const maxIdempotentBody = 128 << 10

func checkIdempotent(r ports.IdempotencyRecord) error {
	if r.ID == "" || len(r.ID) > 128 || r.Fingerprint == "" || len(r.Fingerprint) > 128 || r.CreatedAt.IsZero() ||
		len(r.Body.Ciphertext) > maxIdempotentBody || len(r.ContentType) > 255 {
		return fmt.Errorf("%w: idempotency record", domain.ErrInvalidArgument)
	}
	return nil
}

// BeginIdempotent implements ports.IdempotencyStore.
func (s *Store) BeginIdempotent(ctx context.Context, r ports.IdempotencyRecord) error {
	if err := checkIdempotent(r); err != nil {
		return fmt.Errorf("raftmeta.BeginIdempotent: %w", err)
	}
	return s.proposeIAM(ctx, "BeginIdempotent", cmdBeginIdem, iamCmd{Idem: idemToRecord(r)}, audit.Record{})
}

// GetIdempotent implements ports.IdempotencyStore.
func (s *Store) GetIdempotent(ctx context.Context, id string) (ports.IdempotencyRecord, error) {
	var record idemRecord
	if err := s.getEntity(ctx, "GetIdempotent", idempotencyBucket(), id, "idempotency record", &record); err != nil {
		return ports.IdempotencyRecord{}, err
	}
	return record.toPort(), nil
}

// CompleteIdempotent implements ports.IdempotencyStore.
func (s *Store) CompleteIdempotent(ctx context.Context, r ports.IdempotencyRecord) error {
	if err := checkIdempotent(r); err != nil {
		return fmt.Errorf("raftmeta.CompleteIdempotent: %w", err)
	}
	r.Done = true
	return s.proposeIAM(ctx, "CompleteIdempotent", cmdCompleteIdem, iamCmd{Idem: idemToRecord(r)}, audit.Record{})
}

// DeleteIdempotent implements ports.IdempotencyStore.
func (s *Store) DeleteIdempotent(ctx context.Context, id string, createdAt time.Time) error {
	return s.proposeIAM(ctx, "DeleteIdempotent", cmdDeleteIdem, iamCmd{Name: id, At: createdAt.UTC()}, audit.Record{})
}

// DeleteIdempotentBefore implements ports.IdempotencyStore.
func (s *Store) DeleteIdempotentBefore(ctx context.Context, before time.Time) (int, error) {
	old := 0
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return tx.Bucket(idempotencyBucket()).ForEach(func(_, raw []byte) error {
			var record idemRecord
			if json.Unmarshal(raw, &record) == nil && record.CreatedAt.Before(before) {
				old++
			}
			return nil
		})
	})
	if err != nil {
		return 0, fmt.Errorf("raftmeta.DeleteIdempotentBefore: %w", err)
	}
	if old == 0 {
		return 0, nil
	}
	return old, s.proposeIAM(ctx, "DeleteIdempotentBefore", cmdExpireIdem, iamCmd{At: before.UTC()}, audit.Record{})
}
