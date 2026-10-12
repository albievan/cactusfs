//go:build linux

package disk

import (
	"os"
	"path/filepath"

	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// mountInfo is the table of mounts as this process sees it: in a container,
// the mounts of the container.
const mountInfo = "/proc/self/mountinfo"

// filesystemOf looks dir up in the table of mounts, by the name it has once
// its symbolic links are resolved: that is the name the mounts are listed
// under. A directory whose links cannot be resolved is looked up as it is
// named.
func filesystemOf(dir string) (fs cluster.Filesystem, err error) {
	path, err := filepath.Abs(dir)
	if err != nil {
		return cluster.Filesystem{}, err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	table, err := os.Open(mountInfo)
	if err != nil {
		return cluster.Filesystem{}, err
	}
	defer func() {
		if closeErr := table.Close(); err == nil && closeErr != nil {
			fs, err = cluster.Filesystem{}, closeErr
		}
	}()
	return ParseMountInfo(table, filepath.Clean(path))
}
