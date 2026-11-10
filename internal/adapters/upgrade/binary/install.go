package binary

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/release"
)

// The steps of a run, as the hook of a test is told them.
const (
	// stageWritten: the new binary is whole in its temporary file, synced,
	// and not yet run.
	stageWritten = "written"
	// stageChecked: the new binary ran and named the target.
	stageChecked = "checked"
	// stageKept: the old executable is kept as the previous one, and the
	// new one is not in its place yet.
	stageKept = "kept"
)

// maxVersionOutput is the most of what "<new> version" prints that is read.
const maxVersionOutput = 4 << 10

// install fetches the target's binary beside the executable, holds it to the
// verified SHA-256, runs it once and moves it into place. running is called
// once the binary is verified, before it is run; swapping just before it is
// moved into place, and nothing is moved when it says false. On failure the
// temporary file is removed and the executable left as it was.
func (u *Upgrader) install(ctx context.Context, target release.Verified, running func(), swapping func() bool) *failure {
	releases, err := u.cfg.Source.Releases(ctx)
	if err != nil {
		return &failure{reason: "the update source could not be asked for the release", err: err}
	}
	asset, found := findAsset(releases, target)
	if !found {
		return &failure{
			reason: "the update source lists no " + target.Asset() + " in " + target.Version().String(),
			err:    fmt.Errorf("binary.install: %w: no %s in %s", domain.ErrReleaseNotVerified, target.Asset(), target.Version()),
		}
	}
	info, err := os.Stat(u.cfg.Executable)
	if err != nil {
		return &failure{reason: "the executable could not be read", err: fmt.Errorf("binary.install: %w", err)}
	}

	temp, err := os.CreateTemp(u.dir, tempPrefix+"*"+u.ext)
	if err != nil {
		return &failure{reason: "the directory of the executable cannot be written to", err: fmt.Errorf("binary.install: %w", err)}
	}
	name := temp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = temp.Close()
			_ = os.Remove(name)
		}
	}()

	hash := sha256.New()
	if err := u.cfg.Source.Fetch(ctx, asset, MaxBinary, io.MultiWriter(temp, hash)); err != nil {
		return &failure{reason: "the binary could not be fetched", err: err}
	}
	want := target.SHA256()
	if got := hash.Sum(nil); subtle.ConstantTimeCompare(got, want[:]) != 1 {
		return &failure{
			reason: "the binary fetched is not the one the signed " + release.SumsAsset + " lists",
			err:    fmt.Errorf("binary.install: %w: sha256:%x fetched, %s listed", domain.ErrReleaseNotVerified, got, target.Digest()),
		}
	}
	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		return &failure{reason: "the binary could not be made executable", err: fmt.Errorf("binary.install: %w", err)}
	}
	if err := temp.Sync(); err != nil {
		return &failure{reason: "the binary could not be written to disk", err: fmt.Errorf("binary.install: %w", err)}
	}
	if err := temp.Close(); err != nil {
		return &failure{reason: "the binary could not be written to disk", err: fmt.Errorf("binary.install: %w", err)}
	}
	if err := syncDir(u.dir); err != nil {
		return &failure{reason: "the binary could not be written to disk", err: fmt.Errorf("binary.install: %w", err)}
	}
	if err := u.at(stageWritten); err != nil {
		return &failure{reason: "the upgrade was stopped", err: err}
	}

	running()
	if err := u.check(ctx, name, target.Version()); err != nil {
		return &failure{reason: "the new binary did not run, or did not name " + target.Version().String(), err: err}
	}
	if err := u.at(stageChecked); err != nil {
		return &failure{reason: "the upgrade was stopped", err: err}
	}

	if !swapping() {
		cause := context.Cause(ctx)
		if cause == nil {
			cause = errors.New("the node is stopping")
		}
		return &failure{
			reason: "the node stopped before the new binary was moved into place",
			err:    fmt.Errorf("binary.install: %w", cause),
		}
	}
	if err := u.replace(u.cfg.Executable, u.previous, name, u.at); err != nil {
		return &failure{reason: "the new binary could not be moved into place", err: err}
	}
	keep = true
	u.markReplaced(target.Version())
	if err := syncDir(u.dir); err != nil {
		// The binary is in place; whether the rename outlives a power
		// failure the file system will tell. The node goes on.
		u.cfg.Logger.WarnContext(ctx, "the directory of the executable could not be synced after the upgrade")
	}
	return nil
}

// findAsset returns the target's binary as the source lists it.
func findAsset(releases []release.Release, target release.Verified) (release.Asset, bool) {
	for _, r := range releases {
		if r.Version == target.Version() {
			return r.Asset(target.Asset())
		}
	}
	return release.Asset{}, false
}

// check runs "<path> version", with no environment and for at most the
// check timeout, and returns nil when its first line names the target as
// cmd/cactusfs prints it: "cactusfs <version> ...".
func (u *Upgrader) check(ctx context.Context, path string, target release.Version) error {
	ctx, cancel := context.WithTimeout(ctx, u.checkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	// Nothing of the node's environment - which may hold its credentials -
	// is given to a binary that has not run here before.
	cmd.Env = []string{}
	cmd.Dir = u.dir
	out := &limitedBuffer{limit: maxVersionOutput}
	cmd.Stdout = out
	cmd.WaitDelay = u.checkTimeout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("binary.check: %w", err)
	}
	first, _, _ := strings.Cut(out.String(), "\n")
	fields := strings.Fields(first)
	if len(fields) < 2 || fields[0] != "cactusfs" || fields[1] != target.String() {
		return fmt.Errorf("binary.check: the new binary says %q, not %s", truncate(first, 120), target)
	}
	return nil
}

// limitedBuffer keeps at most limit bytes of what is written to it, and
// takes the rest without keeping it.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

// Write implements io.Writer.
func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// String returns what was kept.
func (b *limitedBuffer) String() string { return b.buf.String() }

// truncate returns at most n bytes of s.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// replaceByLink keeps the executable as previous by a hard link - a copy
// where the file system has none - and renames temp over the executable:
// the path names the old binary or the new one, whole, at every moment.
func replaceByLink(exe, previous, temp string, stage func(string) error) error {
	if err := os.Remove(previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("binary.replace: %w", err)
	}
	if err := os.Link(exe, previous); err != nil {
		if err := copyFile(filepath.Dir(exe), filepath.Base(exe), filepath.Base(previous)); err != nil {
			return fmt.Errorf("binary.replace: %w", err)
		}
	}
	if err := stage(stageKept); err != nil {
		return err
	}
	if err := os.Rename(temp, exe); err != nil {
		return fmt.Errorf("binary.replace: %w", err)
	}
	return nil
}

// replaceAside renames the executable to previous - which Windows allows of
// a running executable, where it refuses to replace it - and temp into its
// place; if that fails, the executable is renamed back. Between the two
// renames the executable's path names nothing, and previous the old binary.
func replaceAside(exe, previous, temp string, stage func(string) error) error {
	if err := os.Remove(previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("binary.replace: the previous binary cannot be removed (is it still running?): %w", err)
	}
	if err := os.Rename(exe, previous); err != nil {
		return fmt.Errorf("binary.replace: %w", err)
	}
	if err := stage(stageKept); err != nil {
		return errors.Join(err, restore(previous, exe))
	}
	if err := os.Rename(temp, exe); err != nil {
		return errors.Join(fmt.Errorf("binary.replace: %w", err), restore(previous, exe))
	}
	return nil
}

// restore renames the kept executable back into place.
func restore(previous, exe string) error {
	if err := os.Rename(previous, exe); err != nil {
		return fmt.Errorf("binary.restore: %w", err)
	}
	return nil
}

// copyFile copies the file src of dir to dst, a new file of dir of src's
// mode, synced. Both are opened through the directory, never by a path that
// could lead out of it.
func copyFile(dir, src, dst string) (err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("binary.copy: %w", err)
	}
	defer func() { _ = root.Close() }()
	in, err := root.Open(src)
	if err != nil {
		return fmt.Errorf("binary.copy: %w", err)
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return fmt.Errorf("binary.copy: %w", err)
	}
	out, err := root.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("binary.copy: %w", err)
	}
	defer func() {
		if closeErr := out.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("binary.copy: %w", closeErr)
		}
		if err != nil {
			_ = root.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("binary.copy: %w", err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("binary.copy: %w", err)
	}
	return nil
}
