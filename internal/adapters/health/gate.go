package health

import (
	"context"
	"sync/atomic"

	"github.com/albievan/cactusfs/internal/domain"
)

// Gate is a readiness check that passes until Close is called. The composition
// root closes it when shutdown begins so that load balancers and orchestrators
// stop routing to the node before its listeners close.
type Gate struct {
	closed atomic.Bool
}

// NewGate returns an open Gate.
func NewGate() *Gate {
	return &Gate{}
}

// Name implements ports.ReadinessCheck.
func (g *Gate) Name() string {
	return "shutdown"
}

// Check implements ports.ReadinessCheck. It returns domain.ErrShuttingDown
// once the gate is closed.
func (g *Gate) Check(context.Context) error {
	if g.closed.Load() {
		return domain.ErrShuttingDown
	}
	return nil
}

// Close makes every later Check fail. It is safe to call more than once and
// from any goroutine.
func (g *Gate) Close() {
	g.closed.Store(true)
}
