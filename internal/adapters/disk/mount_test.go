package disk_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/disk"
	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// mountMark is in every part of the table of the tests that must not be
// shown: options, and the credentials of a source.
const mountMark = "MARK-5e2d-MUST-NOT-BE-SHOWN"

// mountTableText is a table of mounts as a container on a busy host has it, with
// what a table can hold that a careless reader trips over.
const mountTableText = `1418 1205 0:267 / / rw,relatime master:412 - overlay overlay rw,lowerdir=/var/lib/docker/overlay2/l/A:/var/lib/docker/overlay2/l/B,upperdir=/var/lib/docker/overlay2/x/diff,workdir=/var/lib/docker/overlay2/x/work
1419 1418 0:270 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
1420 1418 0:271 / /dev rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755
1425 1418 8:17 / /data rw,noatime shared:7 master:9 propagate_from:3 - ext4 /dev/sdb1 rw,errors=remount-ro
1426 1425 8:33 / /data/1 rw,noatime - xfs /dev/sdc1 rw,attr2,inode64
1427 1425 8:49 /volumes/cactusfs_node1-data2/_data /data/2 rw,relatime master:1 - ext4 /dev/sdd1 rw
1428 1418 0:300 / /mnt/with\040space\011tab\012line\134slash rw - ext4 /dev/disk/by-label/my\040disk rw
1429 1418 0:301 / /mnt/share rw,username=backup,password=MARK-5e2d-MUST-NOT-BE-SHOWN - cifs //backup<credential>@files.example/share rw,password=MARK-5e2d-MUST-NOT-BE-SHOWN
1430 1418 0:302 / /mnt/ssh rw - fuse.sshfs deploy@host.example:/srv/data rw,IdentityFile=MARK-5e2d-MUST-NOT-BE-SHOWN
1431 1418 0:303 / /mnt/dav rw - davfs https://user<credential>@dav.example/files?token=MARK-5e2d-MUST-NOT-BE-SHOWN rw
1432 1418 0:304 / /mnt/nfs rw - nfs4 filer.example:/export/data rw,sec=krb5,addr=192.0.2.9
1433 1418 0:305 / /mnt/over rw - ext4 /dev/sde1 rw
1434 1418 0:306 / /mnt/over rw - xfs /dev/sdf1 rw
1435 1418 0:307 / /mnt/optional rw unbindable shared:1 master:2 - btrfs /dev/sdg1 rw,subvol=/@data
this line is not of the table
1436 1418 0:308 / /mnt/short rw -
1437 1418 0:309 / /mnt/noseparator rw ext4 /dev/sdh1 rw
1438 1418 0:310 / relative rw - ext4 /dev/sdi1 rw
`

// mountTable is the table with the password of each source written in: the
// text above marks its place, so that no line of this file reads as a URL
// with a password in it.
func mountTable() string {
	return strings.ReplaceAll(mountTableText, "<credential>", ":"+mountMark)
}

func TestParseMountInfo(t *testing.T) {
	t.Parallel()
	overlay := cluster.Filesystem{Type: "overlay", Device: "overlay", MountPoint: "/"}
	tests := []struct {
		name string
		path string
		want cluster.Filesystem
	}{
		{"the root mount", "/", overlay},
		{"a path under the root mount only", "/var/lib/cactusfs", overlay},
		{"a mount point itself", "/data", cluster.Filesystem{Type: "ext4", Device: "/dev/sdb1", MountPoint: "/data"}},
		{"a path under a mount", "/data/meta/raft", cluster.Filesystem{Type: "ext4", Device: "/dev/sdb1", MountPoint: "/data"}},
		{"a mount nested in another", "/data/1", cluster.Filesystem{Type: "xfs", Device: "/dev/sdc1", MountPoint: "/data/1"}},
		{"a path under the nested mount", "/data/1/shards/ab", cluster.Filesystem{Type: "xfs", Device: "/dev/sdc1", MountPoint: "/data/1"}},
		{"a bind mount of a directory", "/data/2/shards", cluster.Filesystem{Type: "ext4", Device: "/dev/sdd1", MountPoint: "/data/2"}},
		{"a name that only begins like a mount point", "/data/10", cluster.Filesystem{Type: "ext4", Device: "/dev/sdb1", MountPoint: "/data"}},
		{"a name that only begins like a mount point, at the root", "/database", overlay},
		{
			"escaped space, tab, newline and backslash", "/mnt/with space\ttab\nline\\slash/drive",
			cluster.Filesystem{Type: "ext4", Device: "/dev/disk/by-label/my disk", MountPoint: "/mnt/with space\ttab\nline\\slash"},
		},
		{"a share with credentials", "/mnt/share/d", cluster.Filesystem{Type: "cifs", Device: "//files.example/share", MountPoint: "/mnt/share"}},
		{"a source that names a user", "/mnt/ssh", cluster.Filesystem{Type: "fuse.sshfs", Device: "host.example:/srv/data", MountPoint: "/mnt/ssh"}},
		{"a URL with userinfo and a query", "/mnt/dav", cluster.Filesystem{Type: "davfs", Device: "https://dav.example/files", MountPoint: "/mnt/dav"}},
		{"an export", "/mnt/nfs/x", cluster.Filesystem{Type: "nfs4", Device: "filer.example:/export/data", MountPoint: "/mnt/nfs"}},
		{"one mount over another", "/mnt/over/x", cluster.Filesystem{Type: "xfs", Device: "/dev/sdf1", MountPoint: "/mnt/over"}},
		{"several optional fields", "/mnt/optional", cluster.Filesystem{Type: "btrfs", Device: "/dev/sdg1", MountPoint: "/mnt/optional"}},
		{"a line that ends at the separator", "/mnt/short/x", overlay},
		{"a line without a separator", "/mnt/noseparator", overlay},
		{"a path that is not absolute", "relative/x", cluster.Filesystem{}},
		{"no path", "", cluster.Filesystem{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := disk.ParseMountInfo(strings.NewReader(mountTable()), tt.path)
			if err != nil || got != tt.want {
				t.Errorf("ParseMountInfo(%q) = %+v, %v; want %+v", tt.path, got, err, tt.want)
			}
			if shown := got.Type + got.Device + got.MountPoint; strings.Contains(shown, mountMark) || strings.Contains(shown, "rw") {
				t.Errorf("ParseMountInfo(%q) = %+v, which holds an option or a credential", tt.path, got)
			}
		})
	}

	t.Run("a path no mount holds", func(t *testing.T) {
		t.Parallel()
		table := "1 0 8:1 / /data rw - ext4 /dev/sda1 rw\n2 0 8:2 / /var rw - ext4 /dev/sda2 rw\n"
		for _, path := range []string{"/srv/drive", "/dat", "/data2", "/"} {
			if got, err := disk.ParseMountInfo(strings.NewReader(table), path); err != nil || got != (cluster.Filesystem{}) {
				t.Errorf("ParseMountInfo(%q) = %+v, %v; want nothing", path, got, err)
			}
		}
		if got, err := disk.ParseMountInfo(strings.NewReader(""), "/data"); err != nil || got != (cluster.Filesystem{}) {
			t.Errorf("an empty table = %+v, %v", got, err)
		}
	})

	t.Run("a table without a last newline, and with lines of Windows", func(t *testing.T) {
		t.Parallel()
		got, err := disk.ParseMountInfo(strings.NewReader("1 0 8:1 / /data rw - ext4 /dev/sda1 rw\r\n2 0 8:2 / /data/1 rw - xfs /dev/sdb1 rw"), "/data/1/x")
		if want := (cluster.Filesystem{Type: "xfs", Device: "/dev/sdb1", MountPoint: "/data/1"}); err != nil || got != want {
			t.Errorf("ParseMountInfo = %+v, %v; want %+v", got, err, want)
		}
	})

	t.Run("a table that cannot be read", func(t *testing.T) {
		t.Parallel()
		broken := errors.New("the table went away")
		if got, err := disk.ParseMountInfo(failingReader{broken}, "/data"); !errors.Is(err, broken) || got != (cluster.Filesystem{}) {
			t.Errorf("ParseMountInfo = %+v, %v; want %v", got, err, broken)
		}
		// A line longer than any table has is an error, not a guess.
		long := "1 0 8:1 / /data rw - ext4 /dev/sda1 rw," + strings.Repeat("x", 2<<20) + "\n"
		if got, err := disk.ParseMountInfo(strings.NewReader(long), "/data"); err == nil || got != (cluster.Filesystem{}) {
			t.Errorf("a line of %d bytes = %+v, %v; want an error", len(long), got, err)
		}
	})
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// TestMountSourcesLoseTheirCredentials: whatever stands in the source of a
// mount, what is shown of it names no user and carries no query.
func TestMountSourcesLoseTheirCredentials(t *testing.T) {
	t.Parallel()
	tests := []struct{ source, want string }{
		{"/dev/sda1", "/dev/sda1"},
		{"/dev/mapper/vg0-data", "/dev/mapper/vg0-data"},
		{"overlay", "overlay"},
		{"tmpfs", "tmpfs"},
		{"UUID=0a3407de-014b-458b-b5c1-848e92a327a3", "UUID=0a3407de-014b-458b-b5c1-848e92a327a3"},
		{"/dev/disk/by-label/a#b?c", "/dev/disk/by-label/a#b?c"},
		{"filer:/export", "filer:/export"},
		{"[2001:db8::7]:/export", "[2001:db8::7]:/export"},
		{"//files/share", "//files/share"},
		{"//user:" + mountMark + "@files/share", "//files/share"},
		{"//files/share?password=" + mountMark, "//files/share"},
		{"user@host:/path", "host:/path"},
		{"user:" + mountMark + "@host:/path", "host:/path"},
		{"https://user:" + mountMark + "@host/dav", "https://host/dav"},
		{"https://host/dav?token=" + mountMark, "https://host/dav"},
		{"https://host/dav#" + mountMark, "https://host/dav"},
		{"s3fs://bucket?key=" + mountMark + "@x", "s3fs://"},
		{"/dev/odd@" + mountMark + "@name", "name"},
	}
	for _, tt := range tests {
		table := "1 0 8:1 / /data rw - ext4 " + tt.source + " rw\n"
		got, err := disk.ParseMountInfo(strings.NewReader(table), "/data")
		if err != nil || got.Device != tt.want {
			t.Errorf("source %q is shown as %q, %v; want %q", tt.source, got.Device, err, tt.want)
		}
		if strings.Contains(got.Device, mountMark) {
			t.Errorf("source %q is shown with its credential: %q", tt.source, got.Device)
		}
	}
}

// underMount reports, by whole path elements and without the code under
// test, whether path is at or under mountPoint.
func underMount(mountPoint, path string) bool {
	if !strings.HasPrefix(mountPoint, "/") || !strings.HasPrefix(path, "/") {
		return false
	}
	want, have := strings.Split(strings.TrimSuffix(mountPoint, "/"), "/"), strings.Split(path, "/")
	if mountPoint == "/" {
		return true
	}
	if len(want) > len(have) {
		return false
	}
	for i := range want {
		if want[i] != have[i] {
			return false
		}
	}
	return true
}

// FuzzMountInfo: whatever the table holds, reading it does not panic, and
// what it answers is a mount the path is really on, with a source that names
// no user.
func FuzzMountInfo(f *testing.F) {
	f.Add(mountTable(), "/data/1/shards")
	f.Add(mountTable(), "/mnt/with space\ttab\nline\\slash/x")
	f.Add("1 0 8:1 / / rw - ext4 /dev/sda1 rw\n", "/")
	f.Add("1 0 8:1 / /a\\040b rw - ext4 //u:p@h/s rw", "/a b/c")
	f.Add("1 0 8:1 / /a\\ rw - \\04 \\777 rw\n- - - - - - - - -\n      -   \n", "/a\\")
	f.Add("1 0 8:1 / /data/ rw - ext4 x rw\n", "/data/1")
	f.Add("", "")
	f.Fuzz(func(t *testing.T, table, path string) {
		got, err := disk.ParseMountInfo(strings.NewReader(table), path)
		if err != nil {
			if got != (cluster.Filesystem{}) {
				t.Fatalf("an error came with %+v", got)
			}
			return
		}
		if got == (cluster.Filesystem{}) {
			return
		}
		if !underMount(got.MountPoint, path) {
			t.Fatalf("path %q is said to be on the mount at %q", path, got.MountPoint)
		}
		if strings.Contains(got.Device, "@") {
			t.Fatalf("the source is shown as %q", got.Device)
		}
		// The same table and path give the same answer.
		if again, err := disk.ParseMountInfo(strings.NewReader(table), path); err != nil || again != got {
			t.Fatalf("read again: %+v, %v; first %+v", again, err, got)
		}
	})
}

// TestDriveSaysWhereItIs: a drive names the directory it was opened at, and
// the file system behind it where this system tells - which is checked by
// the test of each system.
func TestDriveSaysWhereItIs(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "drive")
	drive := openDrive(t, dir)
	if drive.Dir() != dir {
		t.Errorf("Dir = %q, want %q", drive.Dir(), dir)
	}
	fromDrive, err := drive.Filesystem()
	if err != nil {
		t.Fatalf("Filesystem: %v", err)
	}
	fromDir, err := disk.FilesystemOf(dir)
	if err != nil || fromDir != fromDrive {
		t.Errorf("FilesystemOf = %+v, %v; the drive says %+v", fromDir, err, fromDrive)
	}
	checkFilesystem(t, dir, fromDrive)

	// It is asked of the directory by name, so a drive that is closed still
	// says where it was.
	if err := drive.Close(); err != nil {
		t.Fatal(err)
	}
	if after, err := drive.Filesystem(); err != nil || after != fromDrive || drive.Dir() != dir {
		t.Errorf("after Close: %+v, %v", after, err)
	}
}
