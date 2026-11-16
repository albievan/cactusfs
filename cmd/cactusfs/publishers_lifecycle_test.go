package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/events/nats"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/ports"
)

// listingStore is a store of targets that does something while it lists
// them.
type listingStore struct {
	ports.TargetStore
	during func()
}

func (s listingStore) ListTargets(ctx context.Context) ([]delivery.Target, error) {
	targets, err := s.TargetStore.ListTargets(ctx)
	s.during()
	return targets, err
}

// TestTidyKeepsWhatWasMadeWhileItListed: a publisher made after tidy read
// the targets - of a target the list it read cannot hold - is not closed by
// it; the next tidy judges it.
func TestTidyKeepsWhatWasMadeWhileItListed(t *testing.T) {
	t.Parallel()
	p, kms, meta, _ := testPublishers(t, nil)
	fresh := testNATS(t, kms, "fresh", "tls://nats.example:4222")
	var made ports.EventPublisher
	p.targets = listingStore{TargetStore: meta, during: func() {
		var err error
		if made, err = p.Publisher(t.Context(), fresh); err != nil {
			t.Error(err)
		}
	}}
	p.tidy(t.Context())
	if again, err := p.Publisher(t.Context(), fresh); err != nil || again != made {
		t.Fatalf("a publisher made while tidy listed was closed: %v", err)
	}

	p.targets = meta
	p.tidy(t.Context())
	if err := made.Publish(t.Context(), testMessage()); !errors.Is(err, nats.ErrClosed) {
		t.Errorf("the next tidy left the publisher of a target that is not there open: %v", err)
	}
}

// TestPublishersRefuseAnOlderTarget: a target deleted and made again under
// its name keeps its publisher when the loop of the one deleted still asks
// - that is refused, as internal - and a newer one replaces it.
func TestPublishersRefuseAnOlderTarget(t *testing.T) {
	t.Parallel()
	p, kms, _, _ := testPublishers(t, nil)
	older := testNATS(t, kms, "stream", "tls://nats.example:4222")
	older.CreatedAt = time.Unix(1_700_000_000, 0)
	newer := testNATS(t, kms, "stream", "tls://other.example:4222")
	newer.CreatedAt = older.CreatedAt.Add(time.Minute)

	current, err := p.Publisher(t.Context(), newer)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := p.Publisher(t.Context(), older)
	if stale != nil || delivery.ClassOf(err) != delivery.ClassInternal || !errors.Is(err, errTargetReplaced) {
		t.Fatalf("the publisher of a target older than the one held = %v, %v; want a refusal of class internal", stale, err)
	}
	if again, err := p.Publisher(t.Context(), newer); err != nil || again != current {
		t.Errorf("the publisher of the newer target was replaced: %v", err)
	}

	newest := testNATS(t, kms, "stream", "tls://third.example:4222")
	newest.CreatedAt = newer.CreatedAt.Add(time.Minute)
	if replaced, err := p.Publisher(t.Context(), newest); err != nil || replaced == current {
		t.Errorf("a newer target did not get a publisher of its own: %v", err)
	}
	if err := current.Publish(t.Context(), testMessage()); !errors.Is(err, nats.ErrClosed) {
		t.Errorf("the publisher a newer target replaced is open: %v", err)
	}
}

// TestPublishersAfterTheNodeCloses: close closes every publisher, and no
// publisher is made afterwards - the request fails as internal.
func TestPublishersAfterTheNodeCloses(t *testing.T) {
	t.Parallel()
	p, kms, _, _ := testPublishers(t, nil)
	target := testNATS(t, kms, "stream", "tls://nats.example:4222")
	publisher, err := p.Publisher(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	p.close()
	if err := publisher.Publish(t.Context(), testMessage()); !errors.Is(err, nats.ErrClosed) {
		t.Errorf("a publisher is open after the node closed: %v", err)
	}
	again, err := p.Publisher(t.Context(), target)
	if again != nil || delivery.ClassOf(err) != delivery.ClassInternal || !errors.Is(err, errPublishersClosed) {
		t.Errorf("a publisher asked for after the node closed = %v, %v; want a refusal of class internal", again, err)
	}
}

// TestUserAgentVersion: a version a User-Agent cannot name falls back to
// the release, with a warning; one it can name is kept, without one.
func TestUserAgentVersion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		version, want string
		warned        bool
	}{
		{version: "1.2.3+build.7", want: "1.2.3+build.7"},
		{version: "1.2.3 (dirty)", want: buildinfo.Release(), warned: true},
	} {
		var logs bytes.Buffer
		got := userAgentVersion(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)), tt.version)
		if got != tt.want {
			t.Errorf("userAgentVersion(%q) = %q, want %q", tt.version, got, tt.want)
		}
		if warned := strings.Contains(logs.String(), `"level":"WARN"`); warned != tt.warned {
			t.Errorf("userAgentVersion(%q) warned %v, want %v: %s", tt.version, warned, tt.warned, logs.String())
		}
	}
}
