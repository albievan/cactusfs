// Package domain is the root of the CactusFS domain model. It holds the typed
// sentinel errors shared by every aggregate. Inbound adapters, and only they,
// map these errors to S3 XML error codes or problem+json responses.
//
// Nothing under internal/domain imports anything outside the standard library.
package domain

import "errors"

// ErrShuttingDown reports that the node has begun a graceful shutdown and no
// longer accepts new work.
var ErrShuttingDown = errors.New("node is shutting down")
