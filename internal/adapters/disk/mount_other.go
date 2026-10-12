//go:build !linux

package disk

import "github.com/albievan/cactusfs/internal/domain/cluster"

// filesystemOf answers nothing: only Linux has a table of mounts that is
// read here. What is shown of a drive elsewhere is its directory and its
// room.
func filesystemOf(string) (cluster.Filesystem, error) {
	return cluster.Filesystem{}, nil
}
