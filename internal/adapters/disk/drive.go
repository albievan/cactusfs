// Package disk is the outbound adapter that stores shards on a local drive.
//
// A drive is a directory. Everything inside it is reached through an os.Root,
// so no operation can leave the directory, and every name is built from
// generated hexadecimal identifiers: bucket names and object keys never reach
// this package. Operating-system differences (directory fsync, file locking)
// are confined to the *_unix.go and *_windows.go files.
package disk

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/fsutil"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
)

const (
	driveFile = "cactusfs-drive.json"
	lockFile  = "lock"
	shardsDir = "shards"
	tmpDir    = "tmp"

	dirMode  = 0o700
	fileMode = 0o600

	driveIDSize = 16
)

// driveInfo is the content of the drive file, which identifies a formatted
// drive. Fields that only a cluster uses are present from version 1 so that
// joining a cluster does not change the format.
type driveInfo struct {
	Magic         string    `json:"magic"`
	FormatVersion int       `json:"format_version"`
	DriveID       string    `json:"drive_id"`
	CreatedAt     time.Time `json:"created_at"`
	ClusterID     *string   `json:"cluster_id"`
	NodeID        *string   `json:"node_id"`
	Set           *int      `json:"set"`
}

// Drive is a ports.ShardStore on one local directory.
type Drive struct {
	root *os.Root
	lock *os.File
	info driveInfo
	// fresh reports that this Open formatted the drive.
	fresh bool

	// commit serialises the existence check and rename that make a shard
	// visible, which keeps shards immutable within this process; the drive
	// lock keeps other processes out.
	commit sync.Mutex
	// seq numbers temporary files.
	seq atomic.Uint64
}

var _ ports.ShardStore = (*Drive)(nil)

// Open opens the drive at dir, formatting it when it is empty, and takes an
// exclusive lock on it. entropy must be a cryptographically secure source; it
// names a new drive. The caller must Close the drive.
func Open(ctx context.Context, dir string, entropy io.Reader, now time.Time) (*Drive, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("disk.Open: %w", err)
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("disk.Open: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("disk.Open: %w", err)
	}
	d := &Drive{root: root}
	if err := d.init(entropy, now); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("disk.Open: %s: %w", dir, err)
	}
	return d, nil
}

func (d *Drive) init(entropy io.Reader, now time.Time) error {
	lock, err := d.root.OpenFile(lockFile, os.O_RDWR|os.O_CREATE, fileMode)
	if err != nil {
		return err
	}
	if err := lockExclusive(lock); err != nil {
		_ = lock.Close()
		return fmt.Errorf("drive is in use by another process: %w", err)
	}
	d.lock = lock

	for _, dir := range []string{shardsDir, tmpDir} {
		if err := d.root.MkdirAll(dir, dirMode); err != nil {
			return err
		}
	}
	// Writes that were in flight when the previous process stopped were
	// never committed. The lock guarantees nobody else is writing.
	if err := d.clearTmp(); err != nil {
		return err
	}

	switch data, err := d.root.ReadFile(driveFile); {
	case err == nil:
		return d.loadInfo(data)
	case errors.Is(err, fs.ErrNotExist):
		return d.format(entropy, now)
	default:
		return err
	}
}

func (d *Drive) loadInfo(data []byte) error {
	var info driveInfo
	if err := json.Unmarshal(data, &info); err != nil || info.Magic != driveFileMagic {
		return fmt.Errorf("%w: %s is not a drive file", domain.ErrIntegrity, driveFile)
	}
	if info.FormatVersion != formatVersion {
		return fmt.Errorf("%w: drive format version %d", domain.ErrUnsupportedFormat, info.FormatVersion)
	}
	if raw, err := hex.DecodeString(info.DriveID); err != nil || len(raw) != driveIDSize {
		return fmt.Errorf("%w: malformed drive ID", domain.ErrIntegrity)
	}
	d.info = info
	return nil
}

// format writes the drive file of a new drive. A directory that already holds
// shards but no drive file is refused: it is a damaged drive, not a new one.
func (d *Drive) format(entropy io.Reader, now time.Time) error {
	entries, err := fs.ReadDir(d.root.FS(), shardsDir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: drive holds shards but %s is missing", domain.ErrIntegrity, driveFile)
	}

	id := make([]byte, driveIDSize)
	if _, err := io.ReadFull(entropy, id); err != nil {
		return fmt.Errorf("generate drive ID: %w", err)
	}
	info := driveInfo{
		Magic:         driveFileMagic,
		FormatVersion: formatVersion,
		DriveID:       hex.EncodeToString(id),
		CreatedAt:     now.UTC(),
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	tmp := tmpDir + "/" + driveFile
	if err := d.writeDurable(tmp, append(data, '\n')); err != nil {
		return err
	}
	if err := d.root.Rename(tmp, driveFile); err != nil {
		return err
	}
	if err := fsutil.SyncDir(d.root, "."); err != nil {
		return err
	}
	d.info = info
	d.fresh = true
	return nil
}

func (d *Drive) writeDurable(name string, data []byte) error {
	f, err := d.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (d *Drive) clearTmp() error {
	entries, err := fs.ReadDir(d.root.FS(), tmpDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := d.root.RemoveAll(tmpDir + "/" + entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

// Close releases the drive lock and the directory handle.
func (d *Drive) Close() error {
	var errs []error
	if d.lock != nil {
		errs = append(errs, unlock(d.lock), d.lock.Close())
		d.lock = nil
	}
	if d.root != nil {
		errs = append(errs, d.root.Close())
		d.root = nil
	}
	return errors.Join(errs...)
}

// Fresh reports whether the drive was empty and has just been formatted. Only
// a fresh drive may take the place of a missing one.
func (d *Drive) Fresh() bool {
	return d.fresh
}

// List implements ports.ShardStore. It reads one directory at a time, so the
// visitor may delete the shard it is shown.
func (d *Drive) List(ctx context.Context, visit func(ports.ShardInfo) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("disk.List: %w", err)
	}
	level1, err := fs.ReadDir(d.root.FS(), shardsDir)
	if err != nil {
		return fmt.Errorf("disk.List: %w", err)
	}
	for _, a := range level1 {
		if !a.IsDir() {
			continue
		}
		level2, err := fs.ReadDir(d.root.FS(), shardsDir+"/"+a.Name())
		if err != nil {
			return fmt.Errorf("disk.List: %w", err)
		}
		for _, b := range level2 {
			if !b.IsDir() {
				continue
			}
			if err := d.listDir(ctx, shardsDir+"/"+a.Name()+"/"+b.Name(), visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Drive) listDir(ctx context.Context, dir string, visit func(ports.ShardInfo) error) error {
	entries, err := fs.ReadDir(d.root.FS(), dir)
	if err != nil {
		return fmt.Errorf("disk.List: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("disk.List: %w", err)
		}
		id, ok := parseShardName(entry.Name())
		if !ok || entry.IsDir() {
			// Not something this adapter wrote; it is left alone.
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("disk.List: %w", err)
		}
		if err := visit(ports.ShardInfo{ID: id, CommittedAt: info.ModTime()}); err != nil {
			return err
		}
	}
	return nil
}

// DriveID implements ports.ShardStore.
func (d *Drive) DriveID() string {
	return d.info.DriveID
}

// Create implements ports.ShardStore.
func (d *Drive) Create(ctx context.Context, id object.ShardID) (ports.ShardWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("disk.Create: %w", err)
	}
	_, path, err := shardPath(id)
	if err != nil {
		return nil, fmt.Errorf("disk.Create: %w", err)
	}
	switch _, err := d.root.Lstat(path); {
	case err == nil:
		return nil, fmt.Errorf("disk.Create: %w: shard %s already exists", domain.ErrInvalidArgument, id)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("disk.Create: %w", err)
	}

	head, err := encodeHeader(header{blockSize: defaultBlock, id: id})
	if err != nil {
		return nil, fmt.Errorf("disk.Create: %w", err)
	}
	index, err := checksumIndex(id)
	if err != nil {
		return nil, fmt.Errorf("disk.Create: %w", err)
	}
	// The sequence number is unique for the life of the process, and tmp is
	// emptied whenever the drive is opened; O_EXCL catches anything else.
	tmp := fmt.Sprintf("%s/%s.%d.%d", tmpDir, id.DataID, id.Index, d.seq.Add(1))
	f, err := d.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return nil, fmt.Errorf("disk.Create: %w", err)
	}
	w := &shardWriter{drive: d, id: id, index: index, file: f, tmp: tmp, block: make([]byte, 0, defaultBlock)}
	if _, err := f.Write(head); err != nil {
		_ = w.Abort()
		return nil, fmt.Errorf("disk.Create: %w", err)
	}
	return w, nil
}

// Open implements ports.ShardStore.
func (d *Drive) Open(ctx context.Context, id object.ShardID) (ports.ShardReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("disk.Open: %w", err)
	}
	_, path, err := shardPath(id)
	if err != nil {
		return nil, fmt.Errorf("disk.Open: %w", err)
	}
	f, err := d.root.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("disk.Open: %w: %s", domain.ErrShardNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("disk.Open: %w", err)
	}
	r, err := newShardReader(f, id)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("disk.Open: shard %s: %w", id, err)
	}
	return r, nil
}

// Delete implements ports.ShardStore.
func (d *Drive) Delete(ctx context.Context, id object.ShardID) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("disk.Delete: %w", err)
	}
	dir, path, err := shardPath(id)
	if err != nil {
		return fmt.Errorf("disk.Delete: %w", err)
	}
	switch err := d.root.Remove(path); {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("disk.Delete: %w", err)
	}
	if err := fsutil.SyncDir(d.root, dir); err != nil {
		return fmt.Errorf("disk.Delete: %w", err)
	}
	return nil
}

// publish moves a finished temporary file to its shard path and makes the
// rename durable.
func (d *Drive) publish(tmp string, id object.ShardID) error {
	dir, path, err := shardPath(id)
	if err != nil {
		return err
	}
	d.commit.Lock()
	defer d.commit.Unlock()

	// Create the two directory levels one at a time so that each new
	// directory entry can be made durable in its parent.
	level1 := dir[:len(shardsDir)+3]
	for _, step := range [][2]string{{level1, shardsDir}, {dir, level1}} {
		switch err := d.root.Mkdir(step[0], dirMode); {
		case err == nil:
			if err := fsutil.SyncDir(d.root, step[1]); err != nil {
				return err
			}
		case !errors.Is(err, fs.ErrExist):
			return err
		}
	}

	switch _, err := d.root.Lstat(path); {
	case err == nil:
		return fmt.Errorf("%w: shard %s already exists", domain.ErrInvalidArgument, id)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := d.root.Rename(tmp, path); err != nil {
		return err
	}
	if err := fsutil.SyncDir(d.root, dir); err != nil {
		// The rename is not known to be durable, so the commit fails; the
		// shard must then not stay visible under its final name.
		_ = d.root.Remove(path)
		return err
	}
	return nil
}
