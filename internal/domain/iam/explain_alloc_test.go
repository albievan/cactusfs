//go:build !race

package iam_test

import (
	"testing"

	"github.com/albievan/cactusfs/internal/domain/iam"
)

// TestDecideAllocations holds Decide to the allocations it made before it
// became a projection of Explain - what Explain collects costs Decide
// nothing - for each request of BenchmarkDecide. The ceiling is not a
// number written here: it is testing.AllocsPerRun of the Decide of bd35f29,
// frozen in decideoracle_test.go, measured in the same run, so a toolchain
// that allocates less lowers it.
//
// It is built without the race detector only. Under -race the race runtime
// allocates for its own bookkeeping, and how much depends on the shape of
// the code: on linux/amd64 the Decide of this change made one allocation a
// call more than the frozen one for some requests (bucket-deny 22 against
// 21, identity-allowed 16 against 15), which say nothing of the cost of the
// binary that ships - built without it, the two are equal. The unit jobs of
// CI run the tests without the race detector on Linux, macOS and Windows;
// make check runs them with it. Recorded at bd35f29 without it (Apple M3,
// Go 1.27.4; PLAN 9.3): anonymous-public 2, bucket-deny 21,
// identity-allowed 15, identity-refused 37, sub-bucket 9. To measure them
// again, check out the base commit and run this AllocsPerRun of iam.Decide
// there.
func TestDecideAllocations(t *testing.T) {
	for name, req := range decideRequests(t) {
		before := testing.AllocsPerRun(100, func() { _ = iam.OldDecide(req) })
		if got := testing.AllocsPerRun(100, func() { _ = iam.Decide(req) }); got > before {
			t.Errorf("Decide(%s) makes %v allocations, %v at bd35f29", name, got, before)
		}
	}
}
