package adminapi_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
)

// TestAnUnknownOutcomeIsNotToBeRepeated: a change whose outcome is not
// known may have been committed. It is answered 500 without Retry-After -
// never the 503 that invites a caller to make it again, Idempotency-Key or
// not - also where its error says, besides, that something was unavailable.
func TestAnUnknownOutcomeIsNotToBeRepeated(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{
		domain.ErrOutcomeUnknown,
		fmt.Errorf("%w: %w", domain.ErrUnavailable, domain.ErrOutcomeUnknown),
		errors.Join(domain.ErrNotLeader, domain.ErrOutcomeUnknown),
	} {
		f := newFixture(t)
		f.meta.FailWrites = cause
		r := f.do(withBearer(call{method: "POST", path: "/buckets", body: map[string]any{"name": "maybe"}}, f.root))
		if p := r.problem(t); r.status != http.StatusInternalServerError || p["code"] != "InternalError" {
			t.Errorf("%v: a create = %d %v, want 500 InternalError", cause, r.status, p["code"])
		}
		if got := r.header.Get("Retry-After"); got != "" {
			t.Errorf("%v: Retry-After = %q, want none", cause, got)
		}
	}
}
