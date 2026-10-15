package domain_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
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
		domain.ErrTooManyRequests, domain.ErrTooManyBuckets, domain.ErrQuotaExceeded, domain.ErrNotPreviewable,
		domain.ErrPreviewTooLarge,
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
	// That a node's release cannot be asked something is the judgement of
	// the node that asked. No node says it of itself, so it has no code -
	// and a node cannot make another believe it is an older release.
	if codes := domain.Codes(fmt.Errorf("%w: node b", domain.ErrNotSupported)); len(codes) != 0 {
		t.Errorf("Codes of ErrNotSupported = %v", codes)
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

// TestSentinelsAreThoseWithAWireName: the list the inbound adapters hold
// their mappings against is every sentinel that crosses a node boundary,
// each once, each under a name of its own - and the names that are in
// releases already are still the names.
func TestSentinelsAreThoseWithAWireName(t *testing.T) {
	t.Parallel()
	sentinels := domain.Sentinels()
	names := map[string]error{}
	for _, sentinel := range sentinels {
		codes := domain.Codes(sentinel)
		if len(codes) != 1 {
			t.Errorf("%v has the wire names %v", sentinel, codes)
			continue
		}
		if other, taken := names[codes[0]]; taken {
			t.Errorf("%v and %v share the wire name %s", sentinel, other, codes[0])
		}
		names[codes[0]] = sentinel
		if back := domain.FromCodes(codes, "x"); !errors.Is(back, sentinel) {
			t.Errorf("%s does not arrive as %v", codes[0], sentinel)
		}
	}
	// Names are added, never changed: a node of another release reads them.
	for name, sentinel := range map[string]error{
		"conflict": domain.ErrConflict, "access_denied": domain.ErrAccessDenied, "invalid_bucket_state": domain.ErrInvalidBucketState,
		"entity_in_use": domain.ErrEntityInUse, "unavailable": domain.ErrUnavailable, "bucket_not_empty": domain.ErrBucketNotEmpty,
		"quota_exceeded": domain.ErrQuotaExceeded, "not_previewable": domain.ErrNotPreviewable,
		"preview_too_large": domain.ErrPreviewTooLarge,
	} {
		if !errors.Is(names[name], sentinel) {
			t.Errorf("the wire name %s is of %v, want %v", name, names[name], sentinel)
		}
	}
	// A refusal for the quota is no denial, and the other way round.
	quota := domain.FromCodes(domain.Codes(fmt.Errorf("object.Put: %w", domain.ErrQuotaExceeded)), "object.Put: quota")
	if !errors.Is(quota, domain.ErrQuotaExceeded) || errors.Is(quota, domain.ErrAccessDenied) {
		t.Errorf("a refusal for the quota arrives as %v", quota)
	}
	// What the list returns is the caller's.
	sentinels[0] = nil
	if domain.Sentinels()[0] == nil {
		t.Error("Sentinels returns a list it keeps")
	}
}

// TestEverySentinelIsInOneOfTheTwoLists reads the declarations themselves:
// every sentinel that errors.go declares is in Sentinels or in
// LocalSentinels, and in one of them only. The inbound adapters hold their
// answers against the two lists, so a sentinel that is declared and listed
// nowhere would be answered by default; this is what makes that impossible.
func TestEverySentinelIsInOneOfTheTwoLists(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "errors.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := 0
	for _, decl := range file.Decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok || general.Tok != token.VAR {
			continue
		}
		for _, spec := range general.Specs {
			values, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range values.Names {
				if strings.HasPrefix(name.Name, "Err") {
					declared++
				}
			}
		}
	}
	wired, local := domain.Sentinels(), domain.LocalSentinels()
	if declared == 0 || declared != len(wired)+len(local) {
		t.Errorf("errors.go declares %d sentinels; %d have a wire name and %d are listed as having none", declared, len(wired), len(local))
	}
	seen := map[error]bool{}
	for _, sentinel := range append(wired, local...) {
		if sentinel == nil || seen[sentinel] {
			t.Errorf("%v is listed twice, or is none", sentinel)
		}
		seen[sentinel] = true
	}
	// One without a wire name crosses a node boundary as what it wraps, if
	// it wraps anything: an invalid tag arrives as an invalid argument.
	for _, sentinel := range local {
		for _, code := range domain.Codes(sentinel) {
			if back := domain.FromCodes([]string{code}, "x"); errors.Is(back, sentinel) {
				t.Errorf("%v is listed as having no wire name and arrives by %s", sentinel, code)
			}
		}
	}
	// A locked session is no denial (ADR-0027): what is told of it must not
	// be taken for what a policy said, and no node tells another of it.
	locked := fmt.Errorf("authorize: %w: user/alice", domain.ErrSessionLocked)
	if errors.Is(locked, domain.ErrAccessDenied) || len(domain.Codes(locked)) != 0 {
		t.Errorf("a locked session reads as a denial, or has the wire names %v", domain.Codes(locked))
	}
	// What the list returns is the caller's.
	local[0] = nil
	if domain.LocalSentinels()[0] == nil {
		t.Error("LocalSentinels returns what it keeps")
	}
}
