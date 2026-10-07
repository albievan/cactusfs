// Package app contains the CactusFS use cases. It depends on the domain model
// and on the interfaces in internal/ports, never on an adapter.
package app

import (
	"context"
	"fmt"
	"slices"

	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/ports"
)

// HealthService answers whether this node is able to serve traffic.
type HealthService struct {
	checks []ports.ReadinessCheck
}

// NewHealthService returns a HealthService that evaluates checks in the order
// given.
func NewHealthService(checks ...ports.ReadinessCheck) *HealthService {
	return &HealthService{checks: slices.Clone(checks)}
}

// Readiness runs every check and returns the aggregated result. A failing
// check does not stop the remaining checks from running.
func (s *HealthService) Readiness(ctx context.Context) cluster.Readiness {
	results := make([]cluster.CheckResult, 0, len(s.checks))
	for _, check := range s.checks {
		result := cluster.CheckResult{Name: check.Name()}
		if err := check.Check(ctx); err != nil {
			result.Err = fmt.Errorf("health.Readiness: check %q: %w", check.Name(), err)
		}
		results = append(results, result)
	}
	return cluster.Readiness{Checks: results}
}
