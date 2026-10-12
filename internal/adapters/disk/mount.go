package disk

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// Dir returns the directory the drive was opened at, as this node sees it:
// inside a container, the path in the container.
//
// It is the operator's own configuration and is shown to nobody but a caller
// the authorizer allows to read the settings (admin:GetSettings). It must
// never become the label of a metric or the attribute of a span: those are
// read by whoever may read metrics, and a path can name a tenant, a host or
// a mount.
func (d *Drive) Dir() string {
	return d.dir
}

// Filesystem returns the file system the drive's directory is on
// (FilesystemOf).
func (d *Drive) Filesystem() (cluster.Filesystem, error) {
	return FilesystemOf(d.dir)
}

// FilesystemOf returns the file system dir is on: its type, what is mounted
// and where. On Linux it is read from the table of mounts of this process;
// on every other system, and for a directory no mount of the table holds,
// the answer is the zero value and no error - a node need not know. The
// options of a mount are never part of the answer: for a network file system
// they can hold a credential.
//
// Like Dir, the answer is for the authorized reading of settings only, and
// for no metric and no span.
func FilesystemOf(dir string) (cluster.Filesystem, error) {
	fs, err := filesystemOf(dir)
	if err != nil {
		return cluster.Filesystem{}, fmt.Errorf("disk.FilesystemOf: %w", err)
	}
	return fs, nil
}

// maxMountLine bounds one line of the table of mounts. The options of an
// overlay name every layer and run to kilobytes; nothing comes near this.
const maxMountLine = 1 << 20

// ParseMountInfo reads a table of mounts in the format of
// /proc/self/mountinfo (proc(5)) and returns the mount that path is on: the
// one whose mount point is the longest that holds path, counted in whole
// path elements, and of several at one mount point the last, which lies over
// the others. path must be absolute and clean, with its symbolic links
// resolved. A path that no mount holds, and a path that is not absolute,
// have the zero value.
//
// A line of the table is
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
//
// of which only the mount point (the fifth field), and after the separator
// "-" the type and the source, are used; the options before and after are
// not looked at. Space, tab, newline and backslash in a name are written as
// a backslash and three octal digits. A line that does not have these fields
// is passed over.
//
// A source that could name a credential - a URL, "//user:secret@host/share",
// "user@host:/path" - is returned without it (cluster.RedactDevice).
func ParseMountInfo(r io.Reader, path string) (cluster.Filesystem, error) {
	var found cluster.Filesystem
	if !strings.HasPrefix(path, "/") {
		return found, nil
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxMountLine)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), " ")
		// Six fields, any number of optional ones, the separator, and
		// three more of which the last is not needed.
		const mountPointField, firstOptional = 4, 6
		separator := -1
		for i := firstOptional; i < len(fields); i++ {
			if fields[i] == "-" {
				separator = i
				break
			}
		}
		if separator < 0 || separator+2 >= len(fields) {
			continue
		}
		mountPoint := unescapeMount(fields[mountPointField])
		if !holds(mountPoint, path) || len(mountPoint) < len(found.MountPoint) {
			continue
		}
		found = cluster.Filesystem{
			Type: unescapeMount(fields[separator+1]), Device: cluster.RedactDevice(unescapeMount(fields[separator+2])), MountPoint: mountPoint,
		}
	}
	if err := scanner.Err(); err != nil {
		return cluster.Filesystem{}, fmt.Errorf("reading the table of mounts: %w", err)
	}
	return found, nil
}

// holds reports whether path is the mount point or lies under it, by whole
// path elements: "/data" holds "/data/1" and not "/database".
func holds(mountPoint, path string) bool {
	switch {
	case !strings.HasPrefix(mountPoint, "/"):
		return false
	case mountPoint == "/" || mountPoint == path:
		return true
	}
	return strings.HasPrefix(path, mountPoint) && path[len(mountPoint)] == '/' && !strings.HasSuffix(mountPoint, "/")
}

// unescapeMount undoes the escaping of the table of mounts: a backslash and
// three octal digits are one byte. Anything else stands for itself.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			a, b, c := s[i+1], s[i+2], s[i+3]
			if a >= '0' && a <= '3' && b >= '0' && b <= '7' && c >= '0' && c <= '7' {
				out.WriteByte((a-'0')<<6 | (b-'0')<<3 | (c - '0'))
				i += 3
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return out.String()
}
