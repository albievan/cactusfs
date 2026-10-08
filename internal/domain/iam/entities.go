package iam

import (
	"fmt"
	"slices"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// maxNameLength bounds the name of a user, group, role or policy.
const maxNameLength = 64

// maxAttachments bounds the groups, roles and policies of one entity.
const maxAttachments = 64

// ValidateName reports whether name may name a user, group, role or policy:
// 1 to 64 letters, digits and "+=,.@_-", as in IAM. Names are case
// sensitive. The error wraps domain.ErrInvalidArgument.
func ValidateName(name string) error {
	if name == "" || len(name) > maxNameLength {
		return fmt.Errorf("%w: name must be 1 to %d characters", domain.ErrInvalidArgument, maxNameLength)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !letter && c != '+' && c != '=' && c != ',' && c != '.' && c != '@' && c != '_' && c != '-' {
			return fmt.Errorf("%w: name %q: letters, digits and +=,.@_- only", domain.ErrInvalidArgument, name)
		}
	}
	return nil
}

func validateNames(kind string, names []string) error {
	if len(names) > maxAttachments {
		return fmt.Errorf("%w: more than %d %s", domain.ErrInvalidArgument, maxAttachments, kind)
	}
	seen := map[string]bool{}
	for _, name := range names {
		if err := ValidateName(name); err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("%w: %s %q is listed twice", domain.ErrInvalidArgument, kind, name)
		}
		seen[name] = true
	}
	return nil
}

// User is somebody, or something, that acts: the owner of API keys and, with
// a password, a console account. What a user may do comes from the roles the
// user holds directly and through groups.
type User struct {
	Name string
	// Groups are the groups the user belongs to; Roles the roles held
	// directly.
	Groups []string
	Roles  []string
	// Disabled suspends the user: every key and session stops working, and
	// nothing is deleted.
	Disabled bool
	// PasswordHash is the encoded hash of the console password, or empty
	// for a user who cannot sign in to the console.
	PasswordHash string
	CreatedAt    time.Time
}

// Validate reports whether the user is well formed.
func (u User) Validate() error {
	if err := ValidateName(u.Name); err != nil {
		return err
	}
	if err := validateNames("groups", u.Groups); err != nil {
		return err
	}
	return validateNames("roles", u.Roles)
}

// Group is a set of users that hold roles together.
type Group struct {
	Name      string
	Roles     []string
	CreatedAt time.Time
}

// Validate reports whether the group is well formed.
func (g Group) Validate() error {
	if err := ValidateName(g.Name); err != nil {
		return err
	}
	return validateNames("roles", g.Roles)
}

// Role is a named set of policies.
type Role struct {
	Name     string
	Policies []string
	// BuiltIn marks a role that ships with the product; it cannot be
	// changed or removed.
	BuiltIn   bool
	CreatedAt time.Time
}

// Validate reports whether the role is well formed.
func (r Role) Validate() error {
	if err := ValidateName(r.Name); err != nil {
		return err
	}
	return validateNames("policies", r.Policies)
}

// Policy is a named policy document.
type Policy struct {
	Name     string
	Document Document
	// BuiltIn marks a policy that ships with the product.
	BuiltIn   bool
	CreatedAt time.Time
}

// Validate reports whether the policy is well formed.
func (p Policy) Validate() error {
	if err := ValidateName(p.Name); err != nil {
		return err
	}
	return p.Document.Validate()
}

// APIKey is a credential of a user: an access key ID and a secret. The
// secret is stored sealed through the KMS and shown once, when the key is
// made.
type APIKey struct {
	AccessKeyID string
	// Owner is the user the key acts as. A key can never do more than its
	// owner.
	Owner string
	// Secret is the sealed secret half.
	Secret SealedSecret
	// Description says what the key is for.
	Description string
	// InlinePolicy, when set, narrows what the key may do: a request must
	// be allowed by the owner's policies and by this one.
	InlinePolicy *Document
	// ExpiresAt, when not zero, is when the key stops working.
	ExpiresAt time.Time
	CreatedAt time.Time
	// LastUsedAt is when the key last authenticated a request, to within a
	// few minutes; zero when it never did.
	LastUsedAt time.Time
}

// maxDescriptionLength bounds the description of a key.
const maxDescriptionLength = 256

// Validate reports whether the key is well formed.
func (k APIKey) Validate() error {
	if err := ValidateAccessKeyID(k.AccessKeyID); err != nil {
		return err
	}
	if err := ValidateName(k.Owner); err != nil {
		return err
	}
	if len(k.Description) > maxDescriptionLength {
		return fmt.Errorf("%w: description longer than %d bytes", domain.ErrInvalidArgument, maxDescriptionLength)
	}
	if k.InlinePolicy != nil {
		return k.InlinePolicy.Validate()
	}
	return nil
}

// Expired reports whether the key has stopped working at the given time.
func (k APIKey) Expired(now time.Time) bool {
	return !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt)
}

// Identity is everything that decides what a request made with an API key
// may do, resolved at one moment: the key, its owner, and every policy that
// reaches the owner through roles and groups.
type Identity struct {
	Key  APIKey
	User User
	// Policies are the documents of every policy of every role the user
	// holds directly or through a group.
	Policies []Document
}

// RoleNames returns the roles a user holds, directly and through the given
// groups, without duplicates, sorted.
func RoleNames(user User, groups []Group) []string {
	names := slices.Clone(user.Roles)
	for _, group := range groups {
		names = append(names, group.Roles...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}
