package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// rulesRefusingStore is the metadata store of a node whose writes of tag
// rules fail - after writing them, when committed is set, as an outcome
// that was not known - and that lets gap do what another request does
// first.
type rulesRefusingStore struct {
	*raftmeta.Store
	err       error
	committed bool
	gap       func(ctx context.Context, s *raftmeta.Store) error
}

func (s *rulesRefusingStore) SetBucketConfig(ctx context.Context, name, kind string, document []byte) error {
	if kind != bucket.ConfigTagRules {
		return s.Store.SetBucketConfig(ctx, name, kind, document)
	}
	if s.gap != nil {
		if err := s.gap(ctx, s.Store); err != nil {
			return err
		}
	}
	if s.committed {
		if err := s.Store.SetBucketConfig(ctx, name, kind, document); err != nil {
			return err
		}
	}
	return s.err
}

// TestABucketWhoseRulesFailOnTheRealStore holds the clean-up of
// BucketService.CreateLowercasing to the replicated store a node runs, not
// only to the store of the tests: a bucket the creation made, whose rules
// entry fails, reads as nothing but what was made - every field and
// document as the store defaults them - and is deleted again; one that
// something was done to in the gap is left; rules that were committed
// after all are kept.
func TestABucketWhoseRulesFailOnTheRealStore(t *testing.T) {
	t.Parallel()
	n := mustStart(t, nodeConfig(t.TempDir())).node
	root := iam.Principal{AccessKeyID: "ROOT", Root: true}
	boom := fmt.Errorf("%w: the rules entry", domain.ErrUnavailable)
	unknown := fmt.Errorf("%w: the rules entry", domain.ErrOutcomeUnknown)
	cors := []byte(`{"rules":[{"allowed_origins":["https://example.test"],"allowed_methods":["GET"]}]}`)

	tests := []struct {
		name, bucket string
		store        *rulesRefusingStore
		// wantErr is what the creation answers; exists whether the bucket
		// is there, and lowercase whether it has the rule.
		wantErr           error
		exists, lowercase bool
	}{
		{name: "taken back", bucket: "taken-back", store: &rulesRefusingStore{err: boom}, wantErr: domain.ErrUnavailable},
		{name: "the rules committed after all", bucket: "committed-after-all", store: &rulesRefusingStore{err: unknown, committed: true}, exists: true, lowercase: true},
		{name: "a document set in the gap: left", bucket: "cors-in-the-gap", store: &rulesRefusingStore{err: boom, gap: func(ctx context.Context, s *raftmeta.Store) error {
			return s.SetBucketConfig(ctx, "cors-in-the-gap", bucket.ConfigCORS, cors)
		}}, exists: true},
	}
	for _, tt := range tests {
		name := tt.bucket
		tt.store.Store = n.meta
		buckets := app.NewBucketService(tt.store, n.authorizer, n.clock).WithUndo(n.meta, n.meta, slog.New(slog.DiscardHandler))
		_, err := buckets.CreateLowercasing(t.Context(), root, name)
		if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
			t.Errorf("%s: CreateLowercasing = %v, want %v", tt.name, err, tt.wantErr)
			continue
		}
		_, getErr := n.meta.GetBucket(t.Context(), name)
		if exists := getErr == nil; exists != tt.exists {
			t.Errorf("%s: the bucket is there: %v, want %v (%v)", tt.name, exists, tt.exists, getErr)
			continue
		}
		if !tt.exists {
			if !errors.Is(getErr, domain.ErrBucketNotFound) {
				t.Errorf("%s: reading the bucket taken back: %v", tt.name, getErr)
			}
			continue
		}
		rules, err := n.buckets.TagRules(t.Context(), root, name)
		if err != nil || rules.Lowercase != tt.lowercase {
			t.Errorf("%s: the rules = %+v, %v; want lowercase %v", tt.name, rules, err, tt.lowercase)
		}
	}
}
