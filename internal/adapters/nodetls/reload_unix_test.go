//go:build unix

package nodetls_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// secretVolume lays files out as the kubelet does for a Secret volume: the
// names the node reads are links into "..data", which is itself a link to a
// directory with the files of one revision.
type secretVolume struct {
	dir string
}

func newSecretVolume(t *testing.T, p pair) secretVolume {
	t.Helper()
	v := secretVolume{dir: t.TempDir()}
	v.update(t, "..2026_10_12_10_00_00.0000000001", p)
	for _, name := range []string{"tls.crt", "tls.key"} {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(v.dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

// update writes a revision into a new directory and switches "..data" to it
// in one step, by renaming a new link over the old one. No file that the
// node reads is ever written to.
func (v secretVolume) update(t *testing.T, revision string, p pair) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(v.dir, revision), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(v.dir, revision, "tls.crt"), p.cert)
	writeFile(t, filepath.Join(v.dir, revision, "tls.key"), p.key)
	if err := os.Symlink(revision, filepath.Join(v.dir, "..data_tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(v.dir, "..data_tmp"), filepath.Join(v.dir, "..data")); err != nil {
		t.Fatal(err)
	}
}

// TestReloadPicksUpASecretVolumeUpdate: Kubernetes replaces a Secret's files
// by switching a link, under names that do not change. The new revision is
// found by its content.
func TestReloadPicksUpASecretVolumeUpdate(t *testing.T) {
	t.Parallel()
	ca := authority(t)
	first, second := serverPair(t, ca, "localhost"), serverPair(t, ca, "localhost")
	volume := newSecretVolume(t, first)
	f := files{filepath.Join(volume.dir, "tls.crt"), filepath.Join(volume.dir, "tls.key")}
	log := &logSink{}
	r := serverReloader(t, f, nodetls.ReloadConfig{Interval: time.Minute, Logger: log.logger()})
	address := serveServer(t, r)

	if r.Poll(t.Context()) || shown(t, address, ca) != serialOf(t, first.cert) {
		t.Fatal("the first revision was replaced by itself")
	}
	volume.update(t, "..2026_10_12_10_05_00.0000000002", second)
	if !r.Poll(t.Context()) {
		t.Fatalf("the new revision of the Secret was not put to use:\n%s", log)
	}
	if got := shown(t, address, ca); got != serialOf(t, second.cert) {
		t.Errorf("after the update a handshake was shown %s, want %s", got, serialOf(t, second.cert))
	}
	if r.Refused(nodetls.RefusedPair) != 0 || r.Refused(nodetls.RefusedUnreadable) != 0 {
		t.Errorf("a switch of the link was seen as half a pair:\n%s", log)
	}
}

// linked puts a pair where a Reloader does not look - "archive" - and gives
// it, in "live", the names a Reloader reads, as links that leave "live":
// relative ones, as certbot makes them, or absolute ones.
type linked struct {
	archive, live string
	absolute      bool
}

func newLinked(t *testing.T, absolute bool) linked {
	t.Helper()
	dir := t.TempDir()
	l := linked{archive: filepath.Join(dir, "archive", "host"), live: filepath.Join(dir, "live", "host"), absolute: absolute}
	for _, made := range []string{l.archive, l.live} {
		if err := os.MkdirAll(made, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func (l linked) files() files {
	return files{filepath.Join(l.live, "tls.crt"), filepath.Join(l.live, "tls.key")}
}

// point writes a pair into "archive" under names of that revision and
// switches the links in "live" to it, each in one step.
func (l linked) point(t *testing.T, revision string, p pair) {
	t.Helper()
	for name, data := range map[string][]byte{"tls.crt": p.cert, "tls.key": p.key} {
		writeFile(t, filepath.Join(l.archive, revision+"-"+name), data)
		target := filepath.Join("..", "..", "archive", "host", revision+"-"+name)
		if l.absolute {
			target = filepath.Join(l.archive, revision+"-"+name)
		}
		fresh := filepath.Join(l.live, name+".new")
		if err := os.Symlink(target, fresh); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(fresh, filepath.Join(l.live, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReloadFollowsALinkOutOfTheDirectoryForTheServerPair: the certificate
// clients are shown is often a link into another directory. The node starts
// with one, as it did when crypto/tls read the files, and takes a replacement
// through one.
func TestReloadFollowsALinkOutOfTheDirectoryForTheServerPair(t *testing.T) {
	t.Parallel()
	for name, absolute := range map[string]bool{"a relative link": false, "an absolute link": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ca := authority(t)
			first, second := serverPair(t, ca, "localhost"), serverPair(t, ca, "localhost")
			links := newLinked(t, absolute)
			links.point(t, "1", first)
			log := &logSink{}
			r := serverReloader(t, links.files(), nodetls.ReloadConfig{Interval: time.Minute, Logger: log.logger()})
			address := serveServer(t, r)
			if r.Poll(t.Context()) || shown(t, address, ca) != serialOf(t, first.cert) {
				t.Fatalf("the pair behind the links is not the one served:\n%s", log)
			}

			links.point(t, "2", second)
			if !r.Poll(t.Context()) {
				t.Fatalf("the pair the links now lead to was not put to use:\n%s", log)
			}
			if got := shown(t, address, ca); got != serialOf(t, second.cert) {
				t.Errorf("after the links were switched a handshake was shown %s, want %s", got, serialOf(t, second.cert))
			}
			if r.Refused(nodetls.RefusedUnreadable) != 0 || len(log.lines(`"level":"ERROR"`)) != 0 {
				t.Errorf("a link that leaves its directory was held against the pair:\n%s", log)
			}
		})
	}
}

// TestReloadKeepsTheNodePairWithinItsDirectory: the node's own certificate
// and key are read within their directory, as they always were. A link that
// leaves it is not followed - not at start, and not for a replacement - and
// one that stays inside is.
func TestReloadKeepsTheNodePairWithinItsDirectory(t *testing.T) {
	t.Parallel()
	const self = "node-1"
	ca := authority(t)
	first, second := nodePair(t, ca, self), nodePair(t, ca, self)
	caFile := writeCA(t, ca)
	for name, absolute := range map[string]bool{"a relative link": false, "an absolute link": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			links := newLinked(t, absolute)
			links.point(t, "1", first)
			if _, err := nodetls.Load(links.files().cert, links.files().key, caFile); err == nil {
				t.Fatal("a node started with a certificate behind a link that leaves its directory")
			}

			// Files of its own at start; the replacement comes behind links.
			f := newFiles(t, first)
			id, err := nodetls.Load(f.cert, f.key, caFile)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			log := &logSink{}
			r, err := id.Reloader(nodetls.ReloadConfig{
				CertFile: f.cert, KeyFile: f.key, Interval: time.Minute, Clock: porttest.NewClock(time.Now()), Logger: log.logger(),
			})
			if err != nil {
				t.Fatal(err)
			}
			links.point(t, "2", second)
			for _, path := range []string{f.cert, f.key} {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(links.live, filepath.Base(path)), path); err != nil {
					t.Fatal(err)
				}
			}
			if r.Poll(t.Context()) || r.Pending() != nodetls.RefusedUnreadable || r.Refused(nodetls.RefusedUnreadable) != 1 {
				t.Errorf("a node certificate behind a link that leaves its directory: Pending = %q, Replaced = %d\n%s",
					r.Pending(), r.Replaced(), log)
			}
			if got := id.NotAfter(); !got.Equal(ca.NotAfter()) || r.Replaced() != 0 {
				t.Errorf("the certificate in use changed: Replaced = %d", r.Replaced())
			}
		})
	}

	// Links that stay inside the directory are followed: a Secret volume.
	volume := newSecretVolume(t, first)
	if _, err := nodetls.Load(filepath.Join(volume.dir, "tls.crt"), filepath.Join(volume.dir, "tls.key"), caFile); err != nil {
		t.Errorf("a node certificate in a Secret volume: %v", err)
	}
}

// within runs what and fails the test when it has not returned in time: what
// is tested here is that nothing waits for a file.
func within(t *testing.T, what string, do func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		do()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return", what)
	}
}

// TestReloadRefusesAFifo: a pipe where the certificate or the key should be
// is not read - reading one that nobody writes to never ends - at start and
// afterwards, for the node's pair and for the one clients are shown. The
// certificate in use stays, and the loop ends with its context.
func TestReloadRefusesAFifo(t *testing.T) {
	t.Parallel()
	const self = "node-1"
	ca := authority(t)
	inUse := map[string]pair{"node": nodePair(t, ca, self), "server": serverPair(t, ca, "localhost")}
	caFile := writeCA(t, ca)
	for _, kind := range []string{"node", "server"} {
		for _, which := range []string{"certificate", "key"} {
			t.Run(kind+": a pipe for the "+which, func(t *testing.T) {
				t.Parallel()
				f := newFiles(t, inUse[kind])
				log, steps := &logSink{}, newStepper()
				cfg := nodetls.ReloadConfig{
					CertFile: f.cert, KeyFile: f.key, Interval: nodetls.MinReloadInterval,
					Clock: porttest.NewClock(time.Now()), Logger: log.logger(), Tick: steps.tick,
				}
				var r *nodetls.Reloader
				var err error
				if kind == "node" {
					var id *nodetls.Identity
					if id, err = nodetls.Load(f.cert, f.key, caFile); err != nil {
						t.Fatalf("Load: %v", err)
					}
					r, err = id.Reloader(cfg)
				} else {
					r, err = nodetls.LoadServer(t.Context(), cfg)
				}
				if err != nil {
					t.Fatal(err)
				}

				path := f.cert
				if which == "key" {
					path = f.key
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
				// Nobody ever opens the pipe for writing.
				replaced := true
				within(t, "Poll with a pipe for the "+which, func() { replaced = r.Poll(t.Context()) })
				if replaced || r.Pending() != nodetls.RefusedUnreadable || r.Refused(nodetls.RefusedUnreadable) != 1 || r.Replaced() != 0 {
					t.Errorf("Poll = %t, Pending = %q, Refused(unreadable) = %d", replaced, r.Pending(), r.Refused(nodetls.RefusedUnreadable))
				}
				if said := log.lines(`"level":"WARN"`, `"reason":"unreadable"`, "is not a regular file"); len(said) != 1 {
					t.Errorf("want one warning that the file is not a regular file; the log:\n%s", log)
				}
				if certificate, err := r.GetCertificate(nil); err != nil || certificate.Leaf.SerialNumber.String() != serialOf(t, inUse[kind].cert) {
					t.Errorf("the certificate in use is not the one from before: %v", err)
				}

				// The loop looks at the pipe, and ends when it is told to.
				loopCtx, stop := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() {
					defer close(done)
					r.Run(loopCtx)
				}()
				steps.await(t)
				steps.step(t)
				stop()
				within(t, "Run, after its context was cancelled", func() { <-done })

				// A node does not start with a pipe either, and does not wait
				// for one.
				within(t, "the start with a pipe for the "+which, func() {
					if kind == "node" {
						_, err = nodetls.Load(f.cert, f.key, caFile)
					} else {
						_, err = nodetls.LoadServer(t.Context(), cfg)
					}
				})
				if err == nil {
					t.Error("a node started with a pipe for the " + which)
				}
			})
		}
	}
}
