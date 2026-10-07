package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/ports"
)

type stubCheck struct {
	name  string
	err   error
	calls *int
}

func (s stubCheck) Name() string { return s.name }

func (s stubCheck) Check(ctx context.Context) error {
	if s.calls != nil {
		*s.calls++
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.err
}

func TestHealthServiceReadiness(t *testing.T) {
	t.Parallel()

	errDisk := errors.New("disk offline")

	tests := []struct {
		name      string
		checks    []ports.ReadinessCheck
		cancelled bool
		wantReady bool
		wantErrs  map[string]error
	}{
		{name: "no checks", wantReady: true},
		{
			name:      "all pass",
			checks:    []ports.ReadinessCheck{stubCheck{name: "disk"}, stubCheck{name: "raft"}},
			wantReady: true,
		},
		{
			name:     "failure is wrapped and named",
			checks:   []ports.ReadinessCheck{stubCheck{name: "disk", err: errDisk}, stubCheck{name: "raft"}},
			wantErrs: map[string]error{"disk": errDisk},
		},
		{
			name:     "domain sentinel survives wrapping",
			checks:   []ports.ReadinessCheck{stubCheck{name: "shutdown", err: domain.ErrShuttingDown}},
			wantErrs: map[string]error{"shutdown": domain.ErrShuttingDown},
		},
		{
			name:      "context is propagated to checks",
			checks:    []ports.ReadinessCheck{stubCheck{name: "disk"}},
			cancelled: true,
			wantErrs:  map[string]error{"disk": context.Canceled},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancelled {
				cancel()
			}

			got := app.NewHealthService(tt.checks...).Readiness(ctx)

			if got.Ready() != tt.wantReady {
				t.Errorf("Ready() = %v, want %v", got.Ready(), tt.wantReady)
			}
			if len(got.Checks) != len(tt.checks) {
				t.Fatalf("got %d results, want %d", len(got.Checks), len(tt.checks))
			}
			for i, result := range got.Checks {
				if want := tt.checks[i].Name(); result.Name != want {
					t.Errorf("result %d name = %q, want %q", i, result.Name, want)
				}
				wantErr := tt.wantErrs[result.Name]
				if !errors.Is(result.Err, wantErr) {
					t.Errorf("check %q error = %v, want %v", result.Name, result.Err, wantErr)
				}
				if wantErr == nil {
					continue
				}
				// The error says which use case and which check failed.
				for _, part := range []string{"health.Readiness", `"` + result.Name + `"`} {
					if !strings.Contains(result.Err.Error(), part) {
						t.Errorf("check %q error = %q, want it to contain %q", result.Name, result.Err, part)
					}
				}
			}
		})
	}
}

func TestHealthServiceRunsEveryCheck(t *testing.T) {
	t.Parallel()

	var first, second int
	svc := app.NewHealthService(
		stubCheck{name: "first", err: errors.New("boom"), calls: &first},
		stubCheck{name: "second", calls: &second},
	)

	svc.Readiness(context.Background())

	if first != 1 || second != 1 {
		t.Errorf("calls = (%d, %d), want (1, 1): a failing check must not stop the rest", first, second)
	}
}

func TestNewHealthServiceCopiesChecks(t *testing.T) {
	t.Parallel()

	checks := []ports.ReadinessCheck{stubCheck{name: "disk"}}
	svc := app.NewHealthService(checks...)
	checks[0] = stubCheck{name: "mutated", err: errors.New("boom")}

	if got := svc.Readiness(context.Background()); !got.Ready() || got.Checks[0].Name != "disk" {
		t.Errorf("service observed a later mutation of the caller's slice: %+v", got.Checks)
	}
}
