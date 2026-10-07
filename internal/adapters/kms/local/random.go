package local

import (
	"crypto/rand"
	"io"
)

// randomSource returns the operating system's cryptographically secure random
// source. Data keys are always drawn from it; it is not injectable, so that no
// configuration can make keys predictable.
func randomSource() io.Reader {
	return rand.Reader
}
