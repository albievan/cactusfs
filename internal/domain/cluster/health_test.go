package cluster_test

import (
	"errors"
	"testing"

	"github.com/albievan/cactusfs/internal/domain/cluster"
)

func TestReadiness(t *testing.T) {
	t.Parallel()

	errDisk := errors.New("disk offline")
	errRaft := errors.New("no leader")

	tests := []struct {
		name       string
		checks     []cluster.CheckResult
		wantReady  bool
		wantFailed []string
	}{
		{name: "no checks is ready", checks: nil, wantReady: true},
		{
			name:      "all passing",
			checks:    []cluster.CheckResult{{Name: "disk"}, {Name: "raft"}},
			wantReady: true,
		},
		{
			name:       "one failing",
			checks:     []cluster.CheckResult{{Name: "disk", Err: errDisk}, {Name: "raft"}},
			wantFailed: []string{"disk"},
		},
		{
			name:       "failures keep their order",
			checks:     []cluster.CheckResult{{Name: "raft", Err: errRaft}, {Name: "kms"}, {Name: "disk", Err: errDisk}},
			wantFailed: []string{"raft", "disk"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := cluster.Readiness{Checks: tt.checks}
			if got := r.Ready(); got != tt.wantReady {
				t.Errorf("Ready() = %v, want %v", got, tt.wantReady)
			}

			failed := r.Failed()
			if len(failed) != len(tt.wantFailed) {
				t.Fatalf("Failed() returned %d checks, want %d", len(failed), len(tt.wantFailed))
			}
			for i, name := range tt.wantFailed {
				if failed[i].Name != name {
					t.Errorf("Failed()[%d].Name = %q, want %q", i, failed[i].Name, name)
				}
				if failed[i].OK() {
					t.Errorf("Failed()[%d].OK() = true, want false", i)
				}
			}
		})
	}
}
