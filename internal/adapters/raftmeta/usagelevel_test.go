package raftmeta

import (
	"testing"

	"github.com/albievan/cactusfs/internal/ports"
)

// TestUsageLevelIsSchema5: the level from which the use cases take quotas to
// exist (ports.UsageLevel) is the entry schema that brought the usage record
// and the quota (ADR-0023), which MetadataLevel reports.
func TestUsageLevelIsSchema5(t *testing.T) {
	t.Parallel()
	if ports.UsageLevel != entrySchema5 {
		t.Errorf("ports.UsageLevel = %d, the schema of usage and quotas is %d", ports.UsageLevel, entrySchema5)
	}
}
