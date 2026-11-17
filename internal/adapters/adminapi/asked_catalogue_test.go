package adminapi

import (
	"testing"

	"github.com/albievan/cactusfs/internal/domain/iam"
)

// TestAskedFirstIsTheCatalogues (PLAN 9.5): every question the idempotency
// use case is given to ask before a key is reserved (askedFirstOf) is of an
// action of the catalogue, on a resource that fits one of the templates the
// catalogue gives that action - with the name of a request, or without one
// for an operation on its kind as a whole.
func TestAskedFirstIsTheCatalogues(t *testing.T) {
	t.Parallel()
	for pattern, asked := range askedFirstOf() {
		resource := iam.Resource{Admin: asked.kind + "/team"}
		if asked.whole {
			resource = iam.Resource{Admin: asked.kind}
		}
		entry, ok := iam.LookupAction(asked.action)
		if !ok {
			t.Errorf("%s asks %s, which is not in the catalogue", pattern, asked.action)
			continue
		}
		if !entry.AskedOn(resource) {
			t.Errorf("%s asks %s on %s, which fits none of its templates %v", pattern, asked.action, resource, entry.Resources)
		}
	}
}
