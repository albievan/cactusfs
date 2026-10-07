// Package ports declares the interfaces through which the use cases in
// internal/app reach the outside world. Implementations live in
// internal/adapters and are wired together only in cmd/cactusfs.
package ports

import "context"

// ReadinessCheck is one condition a node must satisfy before it serves traffic.
type ReadinessCheck interface {
	// Name identifies the check in logs. It must not contain secrets.
	Name() string
	// Check returns nil when the condition holds. It must honour ctx.
	Check(ctx context.Context) error
}
