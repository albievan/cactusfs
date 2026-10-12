package disk

import (
	"fmt"
)

// Usage is the space of the file system a drive is on, in bytes. The numbers
// are for a gauge: they are what the operating system says at one moment.
type Usage struct {
	// FreeBytes is what this process may still write.
	FreeBytes float64
	// TotalBytes is the size of the file system.
	TotalBytes float64
}

// Usage asks the operating system how much room the drive's file system has,
// after making sure that the drive is still there: the file that identifies
// it can be found. Both are asked through the handle the drive was opened
// with, not through its name: what answers is the directory the drive writes
// to, also when another has been put where it was. (On Windows the room is
// asked by name; see fileSystemUsage there.)
//
// It reads no shard and lists nothing - a few system calls - and is safe to
// call at any time, also while the drive is being closed. An error means
// that the drive cannot be reached; a drive that is closed is not.
func (d *Drive) Usage() (Usage, error) {
	if _, err := d.opened.Lstat(driveFile); err != nil {
		return Usage{}, fmt.Errorf("disk.Usage: %w", err)
	}
	usage, err := fileSystemUsage(d.opened)
	if err != nil {
		return Usage{}, fmt.Errorf("disk.Usage: %w", err)
	}
	return usage, nil
}
