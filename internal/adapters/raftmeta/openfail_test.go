package raftmeta_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
)

// closeCounting is a listener that counts how often it was closed.
type closeCounting struct {
	net.Listener
	closed atomic.Int32
}

func (l *closeCounting) Close() error {
	l.closed.Add(1)
	return l.Listener.Close()
}

// TestOpenClosesTheListenerItFailsWith: a member of a cluster hands Open the
// listener Raft is to accept on. When Open fails before a store owns it,
// the listener, and the goroutine that accepts on it, must not outlive the
// call.
func TestOpenClosesTheListenerItFailsWith(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dir  func(t *testing.T) string
	}{
		{name: "the directory cannot be made", dir: func(t *testing.T) string {
			file := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(file, "meta")
		}},
		{name: "the node file is not one", dir: func(t *testing.T) string {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "node.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			return dir
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			listener := &closeCounting{Listener: inner}
			t.Cleanup(func() { _ = inner.Close() })

			store, err := raftmeta.Open(t.Context(), raftmeta.Config{
				Dir: tc.dir(t), Logger: slog.New(slog.DiscardHandler), NodeID: "meta-1", Timing: fastTiming(),
				Cluster: &raftmeta.Cluster{
					Listener:  listener,
					Dial:      func(context.Context, string) (net.Conn, error) { return nil, errors.New("no peers") },
					Advertise: "meta-1@" + inner.Addr().String(),
				},
			})
			if err == nil {
				_ = store.Close()
				t.Fatal("Open succeeded")
			}
			if listener.closed.Load() == 0 {
				t.Fatal("Open failed and left the Raft listener open")
			}
			if _, err := inner.Accept(); !errors.Is(err, net.ErrClosed) {
				t.Errorf("Accept after a failed Open = %v, want net.ErrClosed", err)
			}
		})
	}
}
