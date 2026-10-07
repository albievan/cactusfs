package disk_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/disk"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

const (
	headerSize = 64
	blockSize  = 128 << 10
	storedSize = 32 + blockSize
)

func openDrive(t *testing.T, dir string) *disk.Drive {
	t.Helper()
	d, err := disk.Open(context.Background(), dir, rand.Reader, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("disk.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func shardID(fill byte, index int) object.ShardID {
	return object.ShardID{DataID: object.ID(bytes.Repeat([]byte{fill}, object.IDSize)), Index: index}
}

func writeShard(t *testing.T, d *disk.Drive, id object.ShardID, data []byte) {
	t.Helper()
	w, err := d.Create(context.Background(), id)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func readShard(d *disk.Drive, id object.ShardID) ([]byte, error) {
	r, err := d.Open(context.Background(), id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
}

func shardFile(dir string, id object.ShardID) string {
	name := id.DataID.String()
	return filepath.Join(dir, "shards", name[:2], name[2:4], name+"."+strconv.Itoa(id.Index))
}

func randomData(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return data
}

func TestDriveContract(t *testing.T) {
	t.Parallel()
	porttest.ShardStoreContract(t, func(t *testing.T) ports.ShardStore { return openDrive(t, t.TempDir()) })
}

func TestDriveIdentityAndPersistence(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := openDrive(t, dir)
	id := first.DriveID()
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("DriveID() = %q, want 32 hexadecimal characters", id)
	}
	data := randomData(t, 3*blockSize+17)
	writeShard(t, first, shardID(1, 0), data)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := openDrive(t, dir)
	if second.DriveID() != id {
		t.Errorf("DriveID changed across reopen: %q then %q", id, second.DriveID())
	}
	got, err := readShard(second, shardID(1, 0))
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("shard did not survive a reopen: %v", err)
	}

	other := openDrive(t, t.TempDir())
	if other.DriveID() == id {
		t.Error("two drives were given the same ID")
	}
}

func TestDriveIsLockedAgainstASecondOpen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := openDrive(t, dir)

	if second, err := disk.Open(context.Background(), dir, rand.Reader, time.Now()); err == nil {
		_ = second.Close()
		t.Fatal("a second Open of a drive in use succeeded")
	} else if !strings.Contains(err.Error(), "in use") {
		t.Errorf("error = %v, want it to say the drive is in use", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	openDrive(t, dir)
}

func TestOpenRefusesBadDrives(t *testing.T) {
	t.Parallel()

	t.Run("cancelled context", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := disk.Open(ctx, t.TempDir(), rand.Reader, time.Now()); !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want %v", err, context.Canceled)
		}
	})
	t.Run("no entropy", func(t *testing.T) {
		t.Parallel()
		if _, err := disk.Open(context.Background(), t.TempDir(), bytes.NewReader(nil), time.Now()); err == nil {
			t.Error("formatting a drive without entropy succeeded")
		}
	})
	t.Run("drive file is not ours", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "cactusfs-drive.json"), []byte(`{"magic":"something-else"}`))
		if _, err := disk.Open(context.Background(), dir, rand.Reader, time.Now()); !errors.Is(err, domain.ErrIntegrity) {
			t.Errorf("error = %v, want %v", err, domain.ErrIntegrity)
		}
	})
	t.Run("newer drive format", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		content := `{"magic":"cactusfs-drive","format_version":2,"drive_id":"00112233445566778899aabbccddeeff"}`
		writeFile(t, filepath.Join(dir, "cactusfs-drive.json"), []byte(content))
		if _, err := disk.Open(context.Background(), dir, rand.Reader, time.Now()); !errors.Is(err, domain.ErrUnsupportedFormat) {
			t.Errorf("error = %v, want %v", err, domain.ErrUnsupportedFormat)
		}
	})
	t.Run("malformed drive ID", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		content := `{"magic":"cactusfs-drive","format_version":1,"drive_id":"../../etc"}`
		writeFile(t, filepath.Join(dir, "cactusfs-drive.json"), []byte(content))
		if _, err := disk.Open(context.Background(), dir, rand.Reader, time.Now()); !errors.Is(err, domain.ErrIntegrity) {
			t.Errorf("error = %v, want %v", err, domain.ErrIntegrity)
		}
	})
	t.Run("shards without a drive file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		d := openDrive(t, dir)
		writeShard(t, d, shardID(1, 0), []byte("data"))
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, "cactusfs-drive.json")); err != nil {
			t.Fatal(err)
		}
		// Re-formatting would give the drive a new identity and orphan its shards.
		if _, err := disk.Open(context.Background(), dir, rand.Reader, time.Now()); !errors.Is(err, domain.ErrIntegrity) {
			t.Errorf("error = %v, want %v", err, domain.ErrIntegrity)
		}
	})
}

func TestOnDiskLayoutUsesGeneratedNamesOnly(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	d := openDrive(t, dir)
	writeShard(t, d, shardID(0xAB, 0), randomData(t, 1000))
	writeShard(t, d, shardID(0xAB, 3), randomData(t, 1000))
	writeShard(t, d, shardID(0x01, 1), nil)

	allowed := regexp.MustCompile(`^(cactusfs-drive\.json|lock|tmp|shards(/[0-9a-f]{2}(/[0-9a-f]{2}(/[0-9a-f]{32}\.[0-9]{1,3})?)?)?)$`)
	var files int
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, dir+string(filepath.Separator)))
		if !allowed.MatchString(rel) {
			t.Errorf("unexpected path on the drive: %s", rel)
		}
		if !entry.IsDir() && strings.HasPrefix(rel, "shards/") {
			files++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if files != 3 {
		t.Errorf("found %d shard files, want 3", files)
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "tmp")); err != nil || len(entries) != 0 {
		t.Errorf("tmp holds %d entries after commits (%v), want 0", len(entries), err)
	}
}

func TestInvalidShardIndexIsRejected(t *testing.T) {
	t.Parallel()

	d := openDrive(t, t.TempDir())
	for _, index := range []int{-1, 256, 1 << 20} {
		id := shardID(1, index)
		if _, err := d.Create(context.Background(), id); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("Create(index %d) error = %v, want %v", index, err, domain.ErrInvalidArgument)
		}
		if _, err := d.Open(context.Background(), id); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("Open(index %d) error = %v, want %v", index, err, domain.ErrInvalidArgument)
		}
		if err := d.Delete(context.Background(), id); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("Delete(index %d) error = %v, want %v", index, err, domain.ErrInvalidArgument)
		}
	}
}

func TestCancelledContext(t *testing.T) {
	t.Parallel()

	d := openDrive(t, t.TempDir())
	writeShard(t, d, shardID(1, 0), []byte("data"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := d.Create(ctx, shardID(2, 0)); !errors.Is(err, context.Canceled) {
		t.Errorf("Create error = %v", err)
	}
	if _, err := d.Open(ctx, shardID(1, 0)); !errors.Is(err, context.Canceled) {
		t.Errorf("Open error = %v", err)
	}
	if err := d.Delete(ctx, shardID(1, 0)); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete error = %v", err)
	}

	w, err := d.Create(context.Background(), shardID(3, 0))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte("never visible")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Commit(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Commit error = %v", err)
	}
	if _, err := d.Open(context.Background(), shardID(3, 0)); !errors.Is(err, domain.ErrShardNotFound) {
		t.Errorf("a shard whose commit failed is visible: %v", err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("Write after a failed Commit error = %v", err)
	}
	if err := w.Commit(context.Background()); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("second Commit error = %v", err)
	}
}

func TestInterruptedWritesAreDiscardedOnOpen(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	d := openDrive(t, dir)
	w, err := d.Create(context.Background(), shardID(1, 0))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write(randomData(t, 2*blockSize)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// The process "crashes": neither Commit nor Abort runs.
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "tmp")); len(entries) != 1 {
		t.Fatalf("tmp holds %d entries before reopen, want the 1 interrupted write", len(entries))
	}

	reopened := openDrive(t, dir)
	if entries, _ := os.ReadDir(filepath.Join(dir, "tmp")); len(entries) != 0 {
		t.Errorf("tmp holds %d entries after reopen, want 0", len(entries))
	}
	if _, err := reopened.Open(context.Background(), shardID(1, 0)); !errors.Is(err, domain.ErrShardNotFound) {
		t.Errorf("an uncommitted shard is visible after reopen: %v", err)
	}
}

// TestDamageIsDetected is the integrity property of the format: whatever
// happens to a shard file, a read returns an error and never damaged bytes.
func TestDamageIsDetected(t *testing.T) {
	t.Parallel()

	data := randomData(t, 3*blockSize+1000)

	tests := []struct {
		name   string
		damage func(t *testing.T, path string, file []byte) []byte
		// wantErr is the error Open or the read must report.
		wantErr error
		// verified is how many leading bytes may still be returned: those of
		// blocks that precede the damage and passed verification.
		verified int
	}{
		{
			name: "bit flip in the first block", wantErr: domain.ErrIntegrity, verified: 0,
			damage: func(_ *testing.T, _ string, f []byte) []byte { f[headerSize+32+5] ^= 1; return f },
		},
		{
			name: "bit flip in a later block", wantErr: domain.ErrIntegrity, verified: 2 * blockSize,
			damage: func(_ *testing.T, _ string, f []byte) []byte { f[headerSize+2*storedSize+32+9] ^= 1; return f },
		},
		{
			name: "bit flip in a block checksum", wantErr: domain.ErrIntegrity, verified: blockSize,
			damage: func(_ *testing.T, _ string, f []byte) []byte { f[headerSize+storedSize+3] ^= 1; return f },
		},
		{
			name: "bit flip in the last byte", wantErr: domain.ErrIntegrity, verified: 3 * blockSize,
			damage: func(_ *testing.T, _ string, f []byte) []byte { f[len(f)-1] ^= 1; return f },
		},
		{
			name: "two blocks swapped", wantErr: domain.ErrIntegrity, verified: 0,
			damage: func(_ *testing.T, _ string, f []byte) []byte {
				a := bytes.Clone(f[headerSize : headerSize+storedSize])
				copy(f[headerSize:], f[headerSize+storedSize:headerSize+2*storedSize])
				copy(f[headerSize+storedSize:], a)
				return f
			},
		},
		{
			name: "cut inside a block", wantErr: domain.ErrIntegrity, verified: blockSize,
			damage: func(_ *testing.T, _ string, f []byte) []byte { return f[:headerSize+storedSize+500] },
		},
		{
			name: "cut inside a checksum", wantErr: domain.ErrIntegrity, verified: -1,
			damage: func(_ *testing.T, _ string, f []byte) []byte { return f[:headerSize+storedSize+10] },
		},
		{
			name: "cut inside the header", wantErr: domain.ErrIntegrity, verified: -1,
			damage: func(_ *testing.T, _ string, f []byte) []byte { return f[:20] },
		},
		{
			name: "empty file", wantErr: domain.ErrIntegrity, verified: -1,
			damage: func(_ *testing.T, _ string, _ []byte) []byte { return nil },
		},
		{
			name: "bit flip in the header", wantErr: domain.ErrIntegrity, verified: -1,
			damage: func(_ *testing.T, _ string, f []byte) []byte { f[13] ^= 1; return f },
		},
		{
			name: "wrong magic", wantErr: domain.ErrIntegrity, verified: -1,
			damage: func(_ *testing.T, _ string, f []byte) []byte { f[0] = 'X'; return f },
		},
		{
			name: "garbage", wantErr: domain.ErrIntegrity, verified: -1,
			damage: func(t *testing.T, _ string, f []byte) []byte { return randomData(t, len(f)) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			d := openDrive(t, dir)
			id := shardID(7, 0)
			writeShard(t, d, id, data)

			path := shardFile(dir, id)
			writeFile(t, path, tt.damage(t, path, readFile(t, path)))

			got, err := readShard(d, id)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.verified < 0 && got != nil {
				t.Fatalf("Open should have failed, but %d bytes were read", len(got))
			}
			if len(got) > max(tt.verified, 0) {
				t.Errorf("read %d bytes, but only %d precede the damage", len(got), tt.verified)
			}
			if !bytes.HasPrefix(data, got) {
				t.Error("returned bytes that are not the original data")
			}
		})
	}
}

func TestFutureShardFormatIsRefusedNotMisread(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	d := openDrive(t, dir)
	id := shardID(7, 0)
	writeShard(t, d, id, []byte("data"))

	// Rewrite the header as version 2 with a valid checksum, as a newer
	// binary would have written it.
	path := shardFile(dir, id)
	file := readFile(t, path)
	binary.BigEndian.PutUint16(file[8:], 2)
	copy(file[48:64], headerChecksum(file[:48]))
	writeFile(t, path, file)

	if _, err := readShard(d, id); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Errorf("error = %v, want %v", err, domain.ErrUnsupportedFormat)
	}
}

func TestShardFilesCannotBeSwapped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	d := openDrive(t, dir)
	a, b := shardID(1, 0), shardID(2, 0)
	writeShard(t, d, a, []byte("contents of a"))
	writeShard(t, d, b, []byte("contents of b"))

	// An attacker with drive access puts b's file where a's belongs.
	writeFile(t, shardFile(dir, a), readFile(t, shardFile(dir, b)))

	if got, err := readShard(d, a); !errors.Is(err, domain.ErrIntegrity) || got != nil {
		t.Errorf("reading a swapped shard = %q, %v; want nil, %v", got, err, domain.ErrIntegrity)
	}
	if got, err := readShard(d, b); err != nil || string(got) != "contents of b" {
		t.Errorf("the untouched shard = %q, %v", got, err)
	}
}

func TestTruncationAtABlockBoundaryShowsInSize(t *testing.T) {
	t.Parallel()

	// A file cut exactly between blocks is a valid, shorter shard. The disk
	// layer cannot know: the layout's recorded shard size and the stream
	// authentication catch it. This test pins the part the disk layer owns.
	dir := t.TempDir()
	d := openDrive(t, dir)
	id := shardID(7, 0)
	data := randomData(t, 2*blockSize)
	writeShard(t, d, id, data)

	if err := os.Truncate(shardFile(dir, id), headerSize+storedSize); err != nil {
		t.Fatal(err)
	}
	r, err := d.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()
	if r.Size() != blockSize {
		t.Errorf("Size() = %d, want %d: the caller compares it with the recorded shard size", r.Size(), blockSize)
	}
}

func TestAbortIsIdempotent(t *testing.T) {
	t.Parallel()

	d := openDrive(t, t.TempDir())
	w, err := d.Create(context.Background(), shardID(1, 0))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for range 2 {
		if err := w.Abort(); err != nil {
			t.Errorf("Abort: %v", err)
		}
	}
}
