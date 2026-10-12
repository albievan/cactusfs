package ports

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/tagging"
)

// TagDefinitionStore is the replicated catalogue of tags (ADR-0018). Every
// change is committed through Raft before the call returns and carries its
// audit record in the same entry, as changes to identities do; a record
// without an actor is not appended. Reads are strongly consistent. Until
// every member of the cluster knows the catalogue a change is refused with
// domain.ErrUnavailable, and the catalogue is empty.
type TagDefinitionStore interface {
	// PutTagDefinition adds a definition, or replaces the one with its
	// key; that one keeps the time it was created. The error wraps
	// domain.ErrInvalidArgument for a definition that cannot be kept, or
	// when the catalogue has tagging.MaxDefinitions others.
	PutTagDefinition(ctx context.Context, d tagging.Definition, record audit.Record) error
	// DeleteTagDefinition removes a definition. The error wraps
	// domain.ErrEntityNotFound, and domain.ErrConflict while the tag rules
	// of any bucket require a tag with that key.
	DeleteTagDefinition(ctx context.Context, key string, record audit.Record) error
	// GetTagDefinition returns a definition. The error wraps
	// domain.ErrEntityNotFound.
	GetTagDefinition(ctx context.Context, key string) (tagging.Definition, error)
	// ListTagDefinitions returns the catalogue in key order.
	ListTagDefinitions(ctx context.Context) ([]tagging.Definition, error)
}
