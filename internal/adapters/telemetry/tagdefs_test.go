package telemetry_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/tagging"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// TestTagDefinitionsAreCountedAndTraced: every method of the catalogue's
// store is counted under its own name through the decorator, a refusal is
// counted as an error, what the store answers is passed on - and no key of
// the catalogue is in a metric or a span.
func TestTagDefinitionsAreCountedAndTraced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	recorder := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, recorder)
	store := tel.InstrumentTagDefinitions(porttest.NewMemMetadata())

	const key = "secret-project-key"
	if err := store.PutTagDefinition(ctx, tagging.Definition{Key: key, Values: []string{"a"}}, audit.Record{}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutTagDefinition(ctx, tagging.Definition{Key: "Upper"}, audit.Record{}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a definition the store refuses = %v", err)
	}
	if got, err := store.GetTagDefinition(ctx, key); err != nil || got.Key != key {
		t.Errorf("GetTagDefinition = %+v, %v", got, err)
	}
	if got, err := store.ListTagDefinitions(ctx); err != nil || len(got) != 1 {
		t.Errorf("ListTagDefinitions = %+v, %v", got, err)
	}
	if err := store.DeleteTagDefinition(ctx, key, audit.Record{}); err != nil {
		t.Fatal(err)
	}

	metrics := scrape(t, tel)
	wantEveryMethod(t, metrics, "tag_definitions", methodsOf[ports.TagDefinitionStore]())
	if want := "cactusfs_port_calls_total method=PutTagDefinition port=tag_definitions result=error"; !strings.Contains(metrics, want) {
		t.Errorf("metrics lack %q", want)
	}
	if !contains(spanNames(recorder), "tag_definitions.ListTagDefinitions") {
		t.Errorf("no span named tag_definitions.ListTagDefinitions among %v", spanNames(recorder))
	}
	wantNothingOf(t, tel, recorder, key)
}
