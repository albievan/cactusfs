//go:build !unix

package nodetls

import (
	"fmt"
	"os"

	"github.com/albievan/cactusfs/internal/domain"
)

// openForRead opens a file that is a regular file by its name. There is no
// way here to open without waiting, so what is not a regular file is not
// opened at all; the caller asks the open file again.
func openForRead(from opener, name string) (*os.File, error) {
	info, err := from.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", domain.ErrInvalidArgument, name)
	}
	return from.OpenFile(name, os.O_RDONLY, 0)
}
