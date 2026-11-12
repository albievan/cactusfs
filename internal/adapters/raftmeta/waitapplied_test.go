package raftmeta

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// TestWaitApplied: a node that has applied the index answers at once, one
// that applies it while waiting answers then, and one that never does is
// refused as unavailable with what it applied - never with a count that says
// it had caught up.
func TestWaitApplied(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// seen is what the node has applied at the start; later, when not
		// zero, is what it applies after a few milliseconds.
		seen, later uint64
		wait        time.Duration
		wantErr     string
	}{
		{name: "already applied", seen: 7, wait: time.Hour},
		{name: "applied while waiting", seen: 3, later: 7, wait: time.Minute},
		{name: "never applied", seen: 3, wait: 30 * time.Millisecond, wantErr: "(applied 3, need 7)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := &Store{fsm: &fsm{}, leaderWait: tt.wait}
			s.fsm.seen.Store(tt.seen)
			if tt.later != 0 {
				go func() {
					time.Sleep(5 * time.Millisecond)
					s.fsm.seen.Store(tt.later)
				}()
			}
			err := s.waitApplied(context.Background(), 7)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("waitApplied = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("waitApplied = %v, want %v naming %q", err, domain.ErrUnavailable, tt.wantErr)
			}
		})
	}
}
