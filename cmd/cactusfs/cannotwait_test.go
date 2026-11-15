package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
)

// TestCannotWait: the loops of the start wait on what a forming cluster
// cures, and end on what it does not - a metadata state machine that
// stopped above all, which requests meet as domain.ErrUnavailable: waiting
// on it, the node would be neither ready nor gone.
func TestCannotWait(t *testing.T) {
	t.Parallel()
	enospc := errors.New("no space left on device")
	stopped := func() error { return fmt.Errorf("the metadata state machine of this node stopped: %w", enospc) }
	running := func() error { return nil }
	unavailable := fmt.Errorf("%w: metadata state machine stopped at entry 9: write: no space left on device", domain.ErrUnavailable)
	fatal := errors.New("drive belongs to another deployment")
	cases := []struct {
		name    string
		stopped func() error
		err     error
		want    error // nil: wait
	}{
		{"store not open yet, transient", nil, domain.ErrNotLeader, nil},
		{"store not open yet, fatal", nil, fatal, fatal},
		{"running, transient", running, unavailable, nil},
		{"running, fatal", running, fatal, fatal},
		{"stopped, met as unavailable", stopped, unavailable, enospc},
		{"stopped, met as no leader", stopped, domain.ErrNotLeader, enospc},
		{"stopped, fatal", stopped, fatal, enospc},
	}
	for _, c := range cases {
		got := cannotWait(c.stopped, c.err)
		switch {
		case c.want == nil && got != nil:
			t.Errorf("%s: cannotWait = %v, want to wait", c.name, got)
		case c.want != nil && !errors.Is(got, c.want):
			t.Errorf("%s: cannotWait = %v, want %v", c.name, got, c.want)
		}
	}
}
