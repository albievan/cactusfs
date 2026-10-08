package porttest

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// MemIAM is an in-memory ports.IAMStore, ports.AuditLog and
// ports.SessionStore with the rules of the real one.
type MemIAM struct {
	mu       sync.Mutex
	users    map[string]iam.User
	groups   map[string]iam.Group
	roles    map[string]iam.Role
	policies map[string]iam.Policy
	keys     map[string]iam.APIKey
	sessions map[string]ports.Session
	trail    []audit.Record

	// FailWrites, when set, is returned by every change.
	FailWrites error
}

var (
	_ ports.IAMStore     = (*MemIAM)(nil)
	_ ports.AuditLog     = (*MemIAM)(nil)
	_ ports.SessionStore = (*MemIAM)(nil)
)

// NewMemIAM returns an empty MemIAM.
func NewMemIAM() *MemIAM {
	return &MemIAM{
		users: map[string]iam.User{}, groups: map[string]iam.Group{}, roles: map[string]iam.Role{},
		policies: map[string]iam.Policy{}, keys: map[string]iam.APIKey{}, sessions: map[string]ports.Session{},
	}
}

// change runs a change and, when it took effect, appends its audit record.
func (m *MemIAM) change(record audit.Record, fn func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites != nil {
		return m.FailWrites
	}
	if record.Actor != "" {
		if err := record.Validate(); err != nil {
			return err
		}
	}
	if err := fn(); err != nil {
		return err
	}
	if record.Actor != "" {
		var previous audit.Record
		if len(m.trail) > 0 {
			previous = m.trail[len(m.trail)-1]
		}
		m.trail = append(m.trail, record.Seal(previous))
	}
	return nil
}

func put[V any](entities map[string]V, name, what string, create bool, value V) error {
	_, present := entities[name]
	switch {
	case create && present:
		return fmt.Errorf("%w: %s %s", domain.ErrEntityExists, what, name)
	case !create && !present:
		return fmt.Errorf("%w: %s %s", domain.ErrEntityNotFound, what, name)
	}
	entities[name] = value
	return nil
}

func sorted[V any](entities map[string]V) []V {
	names := make([]string, 0, len(entities))
	for name := range entities {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]V, len(names))
	for i, name := range names {
		out[i] = entities[name]
	}
	return out
}

func (m *MemIAM) rolesExist(names []string) error {
	for _, name := range names {
		if _, ok := m.roles[name]; !ok && !iam.IsBuiltIn(name) {
			return fmt.Errorf("%w: role %s", domain.ErrEntityNotFound, name)
		}
	}
	return nil
}

// PutUser implements ports.IAMStore.
func (m *MemIAM) PutUser(_ context.Context, u iam.User, create bool, record audit.Record) error {
	if err := u.Validate(); err != nil {
		return err
	}
	return m.change(record, func() error {
		for _, name := range u.Groups {
			if _, ok := m.groups[name]; !ok {
				return fmt.Errorf("%w: group %s", domain.ErrEntityNotFound, name)
			}
		}
		if err := m.rolesExist(u.Roles); err != nil {
			return err
		}
		return put(m.users, u.Name, "user", create, u)
	})
}

// DeleteUser implements ports.IAMStore.
func (m *MemIAM) DeleteUser(_ context.Context, name string, record audit.Record) error {
	return m.change(record, func() error {
		if _, ok := m.users[name]; !ok {
			return fmt.Errorf("%w: user %s", domain.ErrEntityNotFound, name)
		}
		for id, key := range m.keys {
			if key.Owner == name {
				delete(m.keys, id)
			}
		}
		for hash, session := range m.sessions {
			if session.User == name {
				delete(m.sessions, hash)
			}
		}
		delete(m.users, name)
		return nil
	})
}

// GetUser implements ports.IAMStore.
func (m *MemIAM) GetUser(_ context.Context, name string) (iam.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.users[name]
	if !ok {
		return iam.User{}, fmt.Errorf("%w: user %s", domain.ErrEntityNotFound, name)
	}
	return user, nil
}

// ListUsers implements ports.IAMStore.
func (m *MemIAM) ListUsers(context.Context) ([]iam.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sorted(m.users), nil
}

// PutGroup implements ports.IAMStore.
func (m *MemIAM) PutGroup(_ context.Context, g iam.Group, create bool, record audit.Record) error {
	if err := g.Validate(); err != nil {
		return err
	}
	return m.change(record, func() error {
		if err := m.rolesExist(g.Roles); err != nil {
			return err
		}
		return put(m.groups, g.Name, "group", create, g)
	})
}

// DeleteGroup implements ports.IAMStore.
func (m *MemIAM) DeleteGroup(_ context.Context, name string, record audit.Record) error {
	return m.change(record, func() error {
		if _, ok := m.groups[name]; !ok {
			return fmt.Errorf("%w: group %s", domain.ErrEntityNotFound, name)
		}
		for _, user := range m.users {
			if slices.Contains(user.Groups, name) {
				return fmt.Errorf("%w: group %s has members", domain.ErrEntityInUse, name)
			}
		}
		delete(m.groups, name)
		return nil
	})
}

// GetGroup implements ports.IAMStore.
func (m *MemIAM) GetGroup(_ context.Context, name string) (iam.Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	group, ok := m.groups[name]
	if !ok {
		return iam.Group{}, fmt.Errorf("%w: group %s", domain.ErrEntityNotFound, name)
	}
	return group, nil
}

// ListGroups implements ports.IAMStore.
func (m *MemIAM) ListGroups(context.Context) ([]iam.Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sorted(m.groups), nil
}

// PutRole implements ports.IAMStore.
func (m *MemIAM) PutRole(_ context.Context, r iam.Role, create bool, record audit.Record) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if iam.IsBuiltIn(r.Name) {
		return fmt.Errorf("%w: %s is a built-in role", domain.ErrInvalidArgument, r.Name)
	}
	return m.change(record, func() error {
		for _, name := range r.Policies {
			if _, ok := m.policies[name]; !ok && !iam.IsBuiltIn(name) {
				return fmt.Errorf("%w: policy %s", domain.ErrEntityNotFound, name)
			}
		}
		return put(m.roles, r.Name, "role", create, r)
	})
}

// DeleteRole implements ports.IAMStore.
func (m *MemIAM) DeleteRole(_ context.Context, name string, record audit.Record) error {
	if iam.IsBuiltIn(name) {
		return fmt.Errorf("%w: %s is a built-in role", domain.ErrInvalidArgument, name)
	}
	return m.change(record, func() error {
		if _, ok := m.roles[name]; !ok {
			return fmt.Errorf("%w: role %s", domain.ErrEntityNotFound, name)
		}
		for _, user := range m.users {
			if slices.Contains(user.Roles, name) {
				return fmt.Errorf("%w: role %s is held by a user", domain.ErrEntityInUse, name)
			}
		}
		for _, group := range m.groups {
			if slices.Contains(group.Roles, name) {
				return fmt.Errorf("%w: role %s is held by a group", domain.ErrEntityInUse, name)
			}
		}
		delete(m.roles, name)
		return nil
	})
}

// GetRole implements ports.IAMStore.
func (m *MemIAM) GetRole(_ context.Context, name string) (iam.Role, error) {
	if role, ok := iam.BuiltInRoles()[name]; ok {
		return role, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	role, ok := m.roles[name]
	if !ok {
		return iam.Role{}, fmt.Errorf("%w: role %s", domain.ErrEntityNotFound, name)
	}
	return role, nil
}

// ListRoles implements ports.IAMStore.
func (m *MemIAM) ListRoles(context.Context) ([]iam.Role, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append(sorted(iam.BuiltInRoles()), sorted(m.roles)...), nil
}

// PutPolicy implements ports.IAMStore.
func (m *MemIAM) PutPolicy(_ context.Context, p iam.Policy, create bool, record audit.Record) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if iam.IsBuiltIn(p.Name) {
		return fmt.Errorf("%w: %s is a built-in policy", domain.ErrInvalidArgument, p.Name)
	}
	return m.change(record, func() error { return put(m.policies, p.Name, "policy", create, p) })
}

// DeletePolicy implements ports.IAMStore.
func (m *MemIAM) DeletePolicy(_ context.Context, name string, record audit.Record) error {
	if iam.IsBuiltIn(name) {
		return fmt.Errorf("%w: %s is a built-in policy", domain.ErrInvalidArgument, name)
	}
	return m.change(record, func() error {
		if _, ok := m.policies[name]; !ok {
			return fmt.Errorf("%w: policy %s", domain.ErrEntityNotFound, name)
		}
		for _, role := range m.roles {
			if slices.Contains(role.Policies, name) {
				return fmt.Errorf("%w: policy %s is part of a role", domain.ErrEntityInUse, name)
			}
		}
		delete(m.policies, name)
		return nil
	})
}

// GetPolicy implements ports.IAMStore.
func (m *MemIAM) GetPolicy(_ context.Context, name string) (iam.Policy, error) {
	if doc, ok := iam.BuiltInPolicies()[name]; ok {
		return iam.Policy{Name: name, Document: doc, BuiltIn: true}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	policy, ok := m.policies[name]
	if !ok {
		return iam.Policy{}, fmt.Errorf("%w: policy %s", domain.ErrEntityNotFound, name)
	}
	return policy, nil
}

// ListPolicies implements ports.IAMStore.
func (m *MemIAM) ListPolicies(context.Context) ([]iam.Policy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	builtIn := map[string]iam.Policy{}
	for name, doc := range iam.BuiltInPolicies() {
		builtIn[name] = iam.Policy{Name: name, Document: doc, BuiltIn: true}
	}
	return append(sorted(builtIn), sorted(m.policies)...), nil
}

// CreateAPIKey implements ports.IAMStore.
func (m *MemIAM) CreateAPIKey(_ context.Context, k iam.APIKey, record audit.Record) error {
	if err := k.Validate(); err != nil {
		return err
	}
	return m.change(record, func() error {
		if _, ok := m.users[k.Owner]; !ok {
			return fmt.Errorf("%w: user %s", domain.ErrEntityNotFound, k.Owner)
		}
		return put(m.keys, k.AccessKeyID, "API key", true, k)
	})
}

// DeleteAPIKey implements ports.IAMStore.
func (m *MemIAM) DeleteAPIKey(_ context.Context, accessKeyID string, record audit.Record) error {
	return m.change(record, func() error {
		if _, ok := m.keys[accessKeyID]; !ok {
			return fmt.Errorf("%w: API key %s", domain.ErrEntityNotFound, accessKeyID)
		}
		delete(m.keys, accessKeyID)
		return nil
	})
}

// ListAPIKeys implements ports.IAMStore.
func (m *MemIAM) ListAPIKeys(_ context.Context, owner string) ([]iam.APIKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []iam.APIKey
	for _, key := range sorted(m.keys) {
		if owner == "" || key.Owner == owner {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// TouchAPIKey implements ports.IAMStore.
func (m *MemIAM) TouchAPIKey(_ context.Context, accessKeyID string, usedAt time.Time) error {
	return m.change(audit.Record{}, func() error {
		if key, ok := m.keys[accessKeyID]; ok && usedAt.After(key.LastUsedAt) {
			key.LastUsedAt = usedAt.UTC()
			m.keys[accessKeyID] = key
		}
		return nil
	})
}

func (m *MemIAM) resolve(name string) (iam.User, []iam.Document, error) {
	user, ok := m.users[name]
	if !ok {
		return iam.User{}, nil, fmt.Errorf("%w: user %s", domain.ErrEntityNotFound, name)
	}
	var groups []iam.Group
	for _, groupName := range user.Groups {
		if group, ok := m.groups[groupName]; ok {
			groups = append(groups, group)
		}
	}
	var policyNames []string
	for _, roleName := range iam.RoleNames(user, groups) {
		if iam.IsBuiltIn(roleName) {
			policyNames = append(policyNames, roleName)
		} else if role, ok := m.roles[roleName]; ok {
			policyNames = append(policyNames, role.Policies...)
		}
	}
	slices.Sort(policyNames)
	var documents []iam.Document
	for _, policyName := range slices.Compact(policyNames) {
		if doc, ok := iam.BuiltInPolicies()[policyName]; ok {
			documents = append(documents, doc)
		} else if policy, ok := m.policies[policyName]; ok {
			documents = append(documents, policy.Document)
		}
	}
	return user, documents, nil
}

// ResolveIdentity implements ports.IAMStore.
func (m *MemIAM) ResolveIdentity(_ context.Context, accessKeyID string) (iam.Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.keys[accessKeyID]
	if !ok {
		return iam.Identity{}, domain.ErrCredentialNotFound
	}
	user, documents, err := m.resolve(key.Owner)
	if err != nil {
		return iam.Identity{}, err
	}
	return iam.Identity{Key: key, User: user, Policies: documents}, nil
}

// ResolveUser implements ports.IAMStore.
func (m *MemIAM) ResolveUser(_ context.Context, name string) (iam.Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	user, documents, err := m.resolve(name)
	if err != nil {
		return iam.Identity{}, err
	}
	return iam.Identity{User: user, Policies: documents}, nil
}

// AppendAudit implements ports.AuditLog.
func (m *MemIAM) AppendAudit(_ context.Context, record audit.Record) error {
	if record.Actor == "" {
		return fmt.Errorf("%w: an audit record needs an actor", domain.ErrInvalidArgument)
	}
	return m.change(record, func() error { return nil })
}

// ListAudit implements ports.AuditLog.
func (m *MemIAM) ListAudit(_ context.Context, after uint64, limit int) ([]audit.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	var records []audit.Record
	for _, record := range m.trail {
		if record.Sequence > after && len(records) < limit {
			records = append(records, record)
		}
	}
	return records, nil
}

// Tamper changes a stored audit record in place, as an attacker with access
// to the state would.
func (m *MemIAM) Tamper(sequence uint64, change func(*audit.Record)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.trail {
		if m.trail[i].Sequence == sequence {
			change(&m.trail[i])
		}
	}
}

// PutSession implements ports.SessionStore.
func (m *MemIAM) PutSession(_ context.Context, s ports.Session, record audit.Record) error {
	return m.change(record, func() error {
		if _, ok := m.users[s.User]; !ok {
			return fmt.Errorf("%w: user %s", domain.ErrEntityNotFound, s.User)
		}
		m.sessions[s.TokenHash] = s
		return nil
	})
}

// GetSession implements ports.SessionStore.
func (m *MemIAM) GetSession(_ context.Context, tokenHash string) (ports.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[tokenHash]
	if !ok {
		return ports.Session{}, fmt.Errorf("%w: session", domain.ErrEntityNotFound)
	}
	return session, nil
}

// DeleteSession implements ports.SessionStore.
func (m *MemIAM) DeleteSession(_ context.Context, tokenHash string, record audit.Record) error {
	return m.change(record, func() error {
		delete(m.sessions, tokenHash)
		return nil
	})
}

// DeleteExpiredSessions implements ports.SessionStore.
func (m *MemIAM) DeleteExpiredSessions(_ context.Context, now time.Time) (int, error) {
	removed := 0
	err := m.change(audit.Record{}, func() error {
		for hash, session := range m.sessions {
			if session.ExpiresAt.Before(now) {
				delete(m.sessions, hash)
				removed++
			}
		}
		return nil
	})
	return removed, err
}

// Sessions returns the number of stored sessions.
func (m *MemIAM) Sessions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}
