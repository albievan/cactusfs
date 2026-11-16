package webhook_test

import (
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/events/webhook"
)

// TestValidVersion: the versions a User-Agent can name, as New judges them.
func TestValidVersion(t *testing.T) {
	t.Parallel()
	for version, want := range map[string]bool{
		"1.2.3":                   true,
		"1.2.3-rc.1+build_7":      true,
		"":                        false,
		"1.0 (evil)":              false,
		"1.0\r\nX-Injected: 1":    false,
		strings.Repeat("1", 64):   true,
		strings.Repeat("1", 65):   false,
		"v1.2.3/../../etc/passwd": false,
	} {
		if got := webhook.ValidVersion(version); got != want {
			t.Errorf("ValidVersion(%q) = %v, want %v", version, got, want)
		}
	}
}
