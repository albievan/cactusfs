package domain_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
)

// TestErrorsCrossNodesWithTheirMeaning: an error sent to another node arrives
// as an error that is every sentinel the original was, and reads the same.
func TestErrorsCrossNodesWithTheirMeaning(t *testing.T) {
	t.Parallel()

	sentinels := []error{
		domain.ErrShuttingDown, domain.ErrNotLeader, domain.ErrOutcomeUnknown, domain.ErrNoTopology, domain.ErrConflict,
		domain.ErrNotBootstrapped, domain.ErrAlreadyBootstrapped, domain.ErrInvalidArgument, domain.ErrAccessDenied,
		domain.ErrInvalidBucketName, domain.ErrBucketNotFound, domain.ErrBucketAlreadyExists, domain.ErrBucketNotEmpty,
		domain.ErrInvalidObjectKey, domain.ErrObjectNotFound, domain.ErrInvalidID, domain.ErrIntegrity,
		domain.ErrInsufficientDrives, domain.ErrShardNotFound, domain.ErrUnsupportedFormat, domain.ErrUnavailable,
		domain.ErrDriveOffline, domain.ErrEntityNotFound, domain.ErrEntityExists, domain.ErrEntityInUse,
		domain.ErrCredentialNotFound, domain.ErrSubBucketNotFound, domain.ErrTooManySubBuckets, domain.ErrSubBucketNotEmpty,
		domain.ErrTooManyRequests, domain.ErrTooManyBuckets,
	}
	seen := map[string]bool{}
	for i, sentinel := range sentinels {
		original := fmt.Errorf("drive.Open: %w: shard 7", sentinel)
		codes := domain.Codes(original)
		if len(codes) != 1 || seen[codes[0]] {
			t.Errorf("Codes(%v) = %v, want one code of its own", sentinel, codes)
			continue
		}
		seen[codes[0]] = true
		received := domain.FromCodes(codes, original.Error())
		if !errors.Is(received, sentinel) || received.Error() != original.Error() {
			t.Errorf("%v arrived as %v", original, received)
		}
		for j, other := range sentinels {
			if j != i && errors.Is(received, other) {
				t.Errorf("%v arrived as %v too", sentinel, other)
			}
		}
	}

	// An error can be several things at once, and stays all of them.
	both := fmt.Errorf("%w: %w: leadership lost", domain.ErrOutcomeUnknown, domain.ErrNotLeader)
	codes := domain.Codes(both)
	if !slices.Contains(codes, "outcome_unknown") || !slices.Contains(codes, "not_leader") || len(codes) != 2 {
		t.Errorf("Codes = %v", codes)
	}
	received := domain.FromCodes(codes, "x")
	if !errors.Is(received, domain.ErrOutcomeUnknown) || !errors.Is(received, domain.ErrNotLeader) {
		t.Errorf("FromCodes(%v) = %v", codes, received)
	}

	// What the domain has no name for has no code, and a code this binary
	// does not know - one from a later release - is ignored: the error is
	// then a failure of no particular kind, never a success.
	if codes := domain.Codes(errors.New("too many open files")); len(codes) != 0 {
		t.Errorf("Codes of an unnamed error = %v", codes)
	}
	if codes := domain.Codes(nil); len(codes) != 0 {
		t.Errorf("Codes(nil) = %v", codes)
	}
	unknown := domain.FromCodes([]string{"invented_in_a_later_release", "integrity"}, "node b: bad block")
	if unknown == nil || !errors.Is(unknown, domain.ErrIntegrity) || unknown.Error() != "node b: bad block" {
		t.Errorf("FromCodes with an unknown code = %v", unknown)
	}
	plain := domain.FromCodes(nil, "node b: too many open files")
	if plain == nil || errors.Is(plain, domain.ErrIntegrity) || errors.Is(plain, domain.ErrShardNotFound) {
		t.Errorf("FromCodes without codes = %v", plain)
	}
}
