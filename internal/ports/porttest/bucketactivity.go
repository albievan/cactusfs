package porttest

import (
	"context"
	"fmt"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/ports"
)

// What entry schema 4 added to the metadata ports (ADR-0019), in memory and
// with the rules of the real store: the record is kept where versions are
// committed (commit) and where reads are handed in (AddObjectReads), and goes
// with its bucket (DeleteBucket).

var _ ports.BucketActivityStore = (*MemMetadata)(nil)

// BucketActivities implements ports.BucketActivityStore.
func (m *MemMetadata) BucketActivities(_ context.Context, names []string) ([]bucket.Activity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(names) > ports.MaxBucketActivities {
		return nil, fmt.Errorf("%w: %d buckets, at most %d", domain.ErrInvalidArgument, len(names), ports.MaxBucketActivities)
	}
	out := make([]bucket.Activity, len(names))
	for i, name := range names {
		out[i] = m.activity[name].Clone()
	}
	return out, nil
}
