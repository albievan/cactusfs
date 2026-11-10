//go:build !windows

package binary

import (
	"fmt"
	"os"
)

// syncDir makes the names of the directory's entries - a file made, one
// renamed - durable.
func syncDir(dir string) (err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("binary.syncDir: %w", err)
	}
	defer func() { _ = root.Close() }()
	d, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("binary.syncDir: %w", err)
	}
	defer func() {
		if closeErr := d.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("binary.syncDir: %w", closeErr)
		}
	}()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("binary.syncDir: %w", err)
	}
	return nil
}
