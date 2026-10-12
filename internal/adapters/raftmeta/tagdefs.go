package raftmeta

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"go.etcd.io/bbolt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/tagging"
	"github.com/albievan/cactusfs/internal/ports"
)

// The catalogue of tags in the state (ADR-0018).
//
//	tagdefs   tag key -> tagDefinitionRecord
//
// The bucket exists in the state only once a definition was written
// (schema 3).

const (
	cmdPutTagDefinition    = "tag.definition.put"
	cmdDeleteTagDefinition = "tag.definition.delete"
)

func tagDefsBucket() []byte { return []byte("tagdefs") }

// tagDefinitionRecord is a tag of the catalogue as it is stored and
// replicated.
type tagDefinitionRecord struct {
	Key         string    `json:"key"`
	Description string    `json:"description,omitempty"`
	Values      []string  `json:"values,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func (r tagDefinitionRecord) toDomain() tagging.Definition {
	return tagging.Definition{Key: r.Key, Description: r.Description, Values: slices.Clone(r.Values), CreatedAt: r.CreatedAt}
}

// tagDefinitionCmd adds a definition to the catalogue, or replaces one.
type tagDefinitionCmd struct {
	Definition tagDefinitionRecord `json:"definition"`
	// Audit is the record of the change, appended in the same transaction.
	Audit *auditRecord `json:"audit,omitempty"`
}

// tagDefinitionRefCmd names a definition: to delete it.
type tagDefinitionRefCmd struct {
	Key   string       `json:"key"`
	Audit *auditRecord `json:"audit,omitempty"`
}

// fitsCatalogue reports whether a definition is within the bounds of the
// catalogue. Only what is counted in bytes is checked here, where every
// release must decide alike: what "lower case" is belongs to the Unicode
// tables of a binary, and is the proposer's to check (tagging.Definition).
//
// It is part of the replicated format, with the bounds of package tagging it
// reads and with tagging.MaxDefinitions: changing what it accepts needs a
// rise of the entry schema.
func fitsCatalogue(d tagDefinitionRecord) bool {
	if d.Key == "" || len(d.Key) > tagging.MaxKeyBytes || len(d.Description) > tagging.MaxDescriptionBytes || len(d.Values) > tagging.MaxValues {
		return false
	}
	for i, value := range d.Values {
		if len(value) > tagging.MaxValueBytes || slices.Contains(d.Values[:i], value) {
			return false
		}
	}
	return true
}

// applyPutTagDefinition adds a definition or replaces the one with its key.
// How many the catalogue has is counted here.
func applyPutTagDefinition(tx *bbolt.Tx, cmd tagDefinitionCmd) (applyResult, error) {
	d := cmd.Definition
	if !fitsCatalogue(d) {
		return refuse(domain.ErrInvalidArgument, "tag definition beyond the bounds of the catalogue")
	}
	defs, err := lazyBucket(tx, tagDefsBucket(), true)
	if err != nil {
		return applyResult{}, err
	}
	if raw := defs.Get([]byte(d.Key)); raw != nil {
		// The tag is as old as it was; what is said of it is replaced.
		var stored tagDefinitionRecord
		if err := json.Unmarshal(raw, &stored); err != nil {
			return applyResult{}, fmt.Errorf("%w: stored tag definition: %w", domain.ErrIntegrity, err)
		}
		d.CreatedAt = stored.CreatedAt
	} else {
		count := 0
		c := defs.Cursor()
		for k, _ := c.First(); k != nil && count < tagging.MaxDefinitions; k, _ = c.Next() {
			count++
		}
		if count >= tagging.MaxDefinitions {
			return refuse(domain.ErrInvalidArgument, "the catalogue has %d tag definitions, as many as it can", tagging.MaxDefinitions)
		}
	}
	if len(d.Values) == 0 {
		d.Values = nil
	}
	data, err := json.Marshal(d)
	if err != nil {
		return applyResult{}, err
	}
	return applyResult{}, defs.Put([]byte(d.Key), data)
}

// requiredKeys is as much of a bucket's tag rules as the state machine
// reads: the keys of the tags they require. It reads them by itself rather
// than with bucket.ParseTagRules, whose judgement of a document may change
// from one release to the next; this one must not.
type requiredKeys struct {
	Required []struct {
		Key string `json:"key"`
	} `json:"required"`
}

// ruleNaming returns the name of a bucket whose tag rules require a tag with
// the key, or "". A document that cannot be read names nothing.
func ruleNaming(tx *bbolt.Tx, key string) (string, error) {
	var named string
	err := tx.Bucket(bucketsBucket()).ForEach(func(name, raw []byte) error {
		if named != "" {
			return nil
		}
		var b bucketRecord
		if err := json.Unmarshal(raw, &b); err != nil {
			return fmt.Errorf("%w: stored bucket: %w", domain.ErrIntegrity, err)
		}
		var rules requiredKeys
		if document := b.Config[bucket.ConfigTagRules]; len(document) == 0 || json.Unmarshal(document, &rules) != nil {
			return nil
		}
		for _, required := range rules.Required {
			if required.Key == key {
				named = string(name)
			}
		}
		return nil
	})
	return named, err
}

// applyDeleteTagDefinition removes a definition, unless the tag rules of a
// bucket require a tag with its key. Every bucket is looked at: there are
// few definitions and they are seldom deleted.
func applyDeleteTagDefinition(tx *bbolt.Tx, cmd tagDefinitionRefCmd) (applyResult, error) {
	defs, err := lazyBucket(tx, tagDefsBucket(), false)
	if err != nil {
		return applyResult{}, err
	}
	if defs == nil || defs.Get([]byte(cmd.Key)) == nil {
		return refuse(domain.ErrEntityNotFound, "tag definition %s", cmd.Key)
	}
	named, err := ruleNaming(tx, cmd.Key)
	if err != nil {
		return applyResult{}, err
	}
	if named != "" {
		return refuse(domain.ErrConflict, "the tag rules of bucket %s require the tag %s", named, cmd.Key)
	}
	return applyResult{}, defs.Delete([]byte(cmd.Key))
}

var _ ports.TagDefinitionStore = (*Store)(nil)

// PutTagDefinition implements ports.TagDefinitionStore.
func (s *Store) PutTagDefinition(ctx context.Context, d tagging.Definition, record audit.Record) error {
	if err := d.Validate(); err != nil {
		return fmt.Errorf("raftmeta.PutTagDefinition: %w", err)
	}
	trail, err := auditOf(record)
	if err != nil {
		return fmt.Errorf("raftmeta.PutTagDefinition: %w", err)
	}
	cmd := &tagDefinitionCmd{Audit: trail, Definition: tagDefinitionRecord{
		Key: d.Key, Description: d.Description, Values: d.Values, CreatedAt: d.CreatedAt.UTC(),
	}}
	if _, err := s.propose(ctx, entry{Type: cmdPutTagDefinition, PutTagDefinition: cmd}); err != nil {
		return fmt.Errorf("raftmeta.PutTagDefinition: %w", err)
	}
	return nil
}

// DeleteTagDefinition implements ports.TagDefinitionStore.
func (s *Store) DeleteTagDefinition(ctx context.Context, key string, record audit.Record) error {
	trail, err := auditOf(record)
	if err != nil {
		return fmt.Errorf("raftmeta.DeleteTagDefinition: %w", err)
	}
	cmd := &tagDefinitionRefCmd{Key: key, Audit: trail}
	if _, err := s.propose(ctx, entry{Type: cmdDeleteTagDefinition, DeleteTagDefinition: cmd}); err != nil {
		return fmt.Errorf("raftmeta.DeleteTagDefinition: %w", err)
	}
	return nil
}

// GetTagDefinition implements ports.TagDefinitionStore.
func (s *Store) GetTagDefinition(ctx context.Context, key string) (tagging.Definition, error) {
	var record tagDefinitionRecord
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		defs, err := lazyBucket(tx, tagDefsBucket(), false)
		if err != nil {
			return err
		}
		var raw []byte
		if defs != nil {
			raw = defs.Get([]byte(key))
		}
		if raw == nil {
			return fmt.Errorf("%w: tag definition %s", domain.ErrEntityNotFound, key)
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return fmt.Errorf("%w: stored tag definition: %w", domain.ErrIntegrity, err)
		}
		return nil
	})
	if err != nil {
		return tagging.Definition{}, fmt.Errorf("raftmeta.GetTagDefinition: %w", err)
	}
	return record.toDomain(), nil
}

// ListTagDefinitions implements ports.TagDefinitionStore.
func (s *Store) ListTagDefinitions(ctx context.Context) ([]tagging.Definition, error) {
	var out []tagging.Definition
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		out = nil
		defs, err := lazyBucket(tx, tagDefsBucket(), false)
		if err != nil || defs == nil {
			return err
		}
		return defs.ForEach(func(_, raw []byte) error {
			var record tagDefinitionRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return fmt.Errorf("%w: stored tag definition: %w", domain.ErrIntegrity, err)
			}
			out = append(out, record.toDomain())
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("raftmeta.ListTagDefinitions: %w", err)
	}
	return out, nil
}
