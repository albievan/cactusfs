package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/tagging"
	"github.com/albievan/cactusfs/internal/ports"
)

// tagDefinitionsResource is what the catalogue of tags is, to the
// authorizer: arn:cactusfs:admin:::tag-definitions. One definition is
// tag-definition/<key>.
const tagDefinitionsResource = "tag-definitions"

func tagDefinitionResource(key string) string { return "tag-definition/" + key }

// TagDefinitionService holds the use cases of the deployment's catalogue of
// tags (ADR-0018): the tags an administrator defines for users to pick, and
// that a bucket's tag rules may hold its objects to. Every one of them
// passes the authorizer first; a change carries its audit record in the
// entry that makes it, as changes to identities do.
type TagDefinitionService struct {
	store   ports.TagDefinitionStore
	authz   ports.Authorizer
	auditor *Auditor
	clock   ports.Clock
	// keys orders the changes of one key made through this service.
	keys nameLocks
}

// TagDefinitionServiceConfig lists what a TagDefinitionService depends on.
type TagDefinitionServiceConfig struct {
	Store ports.TagDefinitionStore
	Authz ports.Authorizer
	// Auditor builds the records of changes.
	Auditor *Auditor
	Clock   ports.Clock
}

// NewTagDefinitionService returns a TagDefinitionService.
func NewTagDefinitionService(cfg TagDefinitionServiceConfig) *TagDefinitionService {
	return &TagDefinitionService{store: cfg.Store, authz: cfg.Authz, auditor: cfg.Auditor, clock: cfg.Clock}
}

// TagDefinitionInput is what is said of a tag when it is defined: its key,
// what it is for, and the only values it may have - none lets it have any.
type TagDefinitionInput struct {
	Key         string
	Description string
	Values      []string
}

// allow asks the authorizer and, when it agrees, returns the audit record of
// the action as carried out, with details.
func (s *TagDefinitionService) allow(
	ctx context.Context, p iam.Principal, action iam.Action, resource string, details map[string]string,
) (audit.Record, error) {
	r := iam.Resource{Admin: resource}
	if err := s.authz.Authorize(ctx, iam.Request{Principal: p, Action: action, Resource: r}); err != nil {
		return audit.Record{}, err
	}
	return s.auditor.Record(p, action, r, audit.Succeeded, details), nil
}

// List returns the catalogue in key order.
func (s *TagDefinitionService) List(ctx context.Context, p iam.Principal) ([]tagging.Definition, error) {
	if _, err := s.allow(ctx, p, iam.ActionListTagDefinitions, tagDefinitionsResource, nil); err != nil {
		return nil, fmt.Errorf("tagdefs.List: %w", err)
	}
	definitions, err := s.store.ListTagDefinitions(ctx)
	if err != nil {
		return nil, fmt.Errorf("tagdefs.List: %w", err)
	}
	return definitions, nil
}

// Get returns one definition. The error wraps domain.ErrEntityNotFound.
func (s *TagDefinitionService) Get(ctx context.Context, p iam.Principal, key string) (tagging.Definition, error) {
	if _, err := s.allow(ctx, p, iam.ActionGetTagDefinition, tagDefinitionResource(key), nil); err != nil {
		return tagging.Definition{}, fmt.Errorf("tagdefs.Get: %w", err)
	}
	d, err := s.store.GetTagDefinition(ctx, key)
	if err != nil {
		return tagging.Definition{}, fmt.Errorf("tagdefs.Get: %w", err)
	}
	return d, nil
}

// Create adds a definition of a key the catalogue does not define, and
// returns it as it is kept. A key that is defined already is refused with
// domain.ErrEntityExists; Put changes it.
//
// Whether the key is defined is read before the definition is proposed, not
// decided with it: the store has no "create only" (its entry adds or
// replaces). Two creates of one key through this service are carried out
// one at a time, so the second is refused; two through different gateways
// at the same moment may both succeed, and the definition is then the one
// stored last - both callers held admin:PutTagDefinition, which may replace
// it anyway. The contract says so (createTagDefinition).
func (s *TagDefinitionService) Create(ctx context.Context, p iam.Principal, in TagDefinitionInput) (tagging.Definition, error) {
	d, err := s.change(ctx, p, in, true)
	if err != nil {
		return tagging.Definition{}, fmt.Errorf("tagdefs.Create: %w", err)
	}
	return d, nil
}

// Put defines a key, or changes its definition, and returns it as it is
// kept: a definition that is changed keeps the time it was first defined.
// Changing a definition changes no tag already kept and no tag rule.
func (s *TagDefinitionService) Put(ctx context.Context, p iam.Principal, in TagDefinitionInput) (tagging.Definition, error) {
	d, err := s.change(ctx, p, in, false)
	if err != nil {
		return tagging.Definition{}, fmt.Errorf("tagdefs.Put: %w", err)
	}
	return d, nil
}

// change carries out Create (only) and Put. The definition is judged by
// tagging.Definition.Validate (domain.ErrInvalidArgument) after the caller
// is authorized and before anything is read. The store refuses the 201st
// definition (domain.ErrInvalidArgument), and every change while a member
// of the metadata group does not know the catalogue (domain.ErrUnavailable).
func (s *TagDefinitionService) change(ctx context.Context, p iam.Principal, in TagDefinitionInput, only bool) (tagging.Definition, error) {
	details := map[string]string{"values": strconv.Itoa(len(in.Values))}
	if only {
		details["created"] = "true"
	}
	record, err := s.allow(ctx, p, iam.ActionPutTagDefinition, tagDefinitionResource(in.Key), details)
	if err != nil {
		return tagging.Definition{}, err
	}
	d := tagging.Definition{Key: in.Key, Description: in.Description, Values: in.Values, CreatedAt: s.clock.Now().UTC()}
	if err := d.Validate(); err != nil {
		return tagging.Definition{}, err
	}
	d = d.Clone()
	if len(d.Values) == 0 {
		d.Values = nil
	}
	unlock, err := s.keys.lock(ctx, d.Key)
	if err != nil {
		return tagging.Definition{}, err
	}
	defer unlock()
	existing, err := s.store.GetTagDefinition(ctx, d.Key)
	switch {
	case err == nil && only:
		return tagging.Definition{}, fmt.Errorf("%w: the tag %q is defined already", domain.ErrEntityExists, d.Key)
	case err == nil:
		// The store keeps the time it was first defined; so does the answer.
		d.CreatedAt = existing.CreatedAt
	case !errors.Is(err, domain.ErrEntityNotFound):
		return tagging.Definition{}, err
	}
	if err := s.store.PutTagDefinition(ctx, d, record); err != nil {
		return tagging.Definition{}, err
	}
	return d, nil
}

// Delete removes a definition. Tags already kept are not changed. The error
// wraps domain.ErrEntityNotFound, and domain.ErrEntityInUse while the tag
// rules of any bucket require a tag with the key - which bucket is not
// said: the caller may not be allowed to know of it.
func (s *TagDefinitionService) Delete(ctx context.Context, p iam.Principal, key string) error {
	record, err := s.allow(ctx, p, iam.ActionDeleteTagDefinition, tagDefinitionResource(key), nil)
	if err != nil {
		return fmt.Errorf("tagdefs.Delete: %w", err)
	}
	unlock, err := s.keys.lock(ctx, key)
	if err != nil {
		return fmt.Errorf("tagdefs.Delete: %w", err)
	}
	defer unlock()
	err = s.store.DeleteTagDefinition(ctx, key, record)
	switch {
	case errors.Is(err, domain.ErrConflict):
		// The store's refusal names the bucket; this one does not.
		return fmt.Errorf("tagdefs.Delete: %w: the tag rules of a bucket require the tag %q", domain.ErrEntityInUse, key)
	case err != nil:
		return fmt.Errorf("tagdefs.Delete: %w", err)
	}
	return nil
}
