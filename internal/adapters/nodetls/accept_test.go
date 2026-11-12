package nodetls_test

import (
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/nodetls"
)

// failing fails its first Accepts as a node out of file descriptors would,
// then accepts as the listener it wraps.
type failing struct {
	net.Listener
	failures atomic.Int32
}

func (f *failing) Accept() (net.Conn, error) {
	if f.failures.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.EMFILE)}
	}
	return f.Listener.Accept()
}

func tcpListener(t *testing.T) net.Listener {
	t.Helper()
	inner, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return inner
}

// TestListenerOutlivesFailedAccepts: a failure to accept that passes - out of
// file descriptors - must not leave the node deaf until it restarts.
func TestListenerOutlivesFailedAccepts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ca := authority(t)
	server, client := identity(t, ca, "meta-1"), identity(t, ca, "meta-2")
	inner := &failing{Listener: tcpListener(t)}
	inner.failures.Store(8) // 5 ms doubling: some 1.3 s of failures
	listener := server.ListenOn(ctx, inner, func(string) bool { return true }, 4, time.Hour)
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	conn, err := client.Dial(ctx, nodetls.Address("meta-1", listener.Addr().String()))
	if err != nil {
		t.Fatalf("a node could not connect after the listener failed to accept: %v", err)
	}
	defer func() { _ = conn.Close() }()
	select {
	case held := <-accepted:
		_ = held.Close()
	case <-time.After(30 * time.Second):
		t.Fatal("the listener stopped accepting after Accept failed")
	}
	if left := inner.failures.Load(); left >= 0 {
		t.Errorf("%d failures were never met", left+1)
	}

	// Closing it still ends it.
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Accept after Close = %v", err)
	}
}

// TestListenerBoundsHandshakes: callers that connect and say nothing hold a
// handshake each until it times out. Past the bound, a new caller is turned
// away at once rather than given a goroutine and a socket too; once a
// handshake ends, a node can connect again.
func TestListenerBoundsHandshakes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ca := authority(t)
	server, client := identity(t, ca, "meta-1"), identity(t, ca, "meta-2")
	listener := server.ListenOn(ctx, tcpListener(t), func(string) bool { return true }, 1, time.Hour)
	t.Cleanup(func() { _ = listener.Close() })
	address := listener.Addr().String()

	silent := func() net.Conn {
		t.Helper()
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	// Two callers that say nothing: one holds the only handshake, the other
	// is closed at once. Which is which is the order the listener took them.
	first, second := silent(), silent()
	closed := make(chan net.Conn, 2)
	for _, conn := range []net.Conn{first, second} {
		go func() {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := conn.Read(make([]byte, 1)); errors.Is(err, io.EOF) {
				closed <- conn
			}
		}()
	}
	var holder net.Conn
	select {
	case dropped := <-closed:
		holder = first
		if dropped == first {
			holder = second
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a caller past the bound of handshakes was not turned away")
	}
	if got := server.PeersRefused(nodetls.PeerRefusedHandshake); got != 1 {
		t.Errorf("handshakes refused = %d, want 1", got)
	}

	// The holder gives up; its handshake ends and its token is given back.
	_ = holder.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := client.Dial(ctx, nodetls.Address("meta-1", address))
		if err == nil {
			defer func() { _ = conn.Close() }()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a node could not connect once the handshake was free: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case held := <-accepted:
		_ = held.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("the node was not handed out")
	}
}

// TestListenerClosesTheConnectionsOfANodeNoLongerAdmitted: a voter taken
// out of the group, or a node that was revoked (ADR-0030), keeps no
// connection it opened while it was admitted; the others keep theirs.
func TestListenerClosesTheConnectionsOfANodeNoLongerAdmitted(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ca := authority(t)
	server := identity(t, ca, "meta-1")
	var removed atomic.Bool
	allow := func(nodeID string) bool { return nodeID != "meta-2" || !removed.Load() }
	listener := server.ListenOn(ctx, tcpListener(t), allow, 4, 20*time.Millisecond)
	t.Cleanup(func() { _ = listener.Close() })
	address := listener.Addr().String()

	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()
	dial := func(nodeID string) net.Conn {
		t.Helper()
		conn, err := identity(t, ca, nodeID).Dial(ctx, nodetls.Address("meta-1", address))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		select {
		case <-accepted:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s was not handed out", nodeID)
		}
		return conn
	}
	leaving, staying := dial("meta-2"), dial("meta-3")

	removed.Store(true)
	_ = leaving.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := leaving.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the connection of a node no longer admitted: read = %v, want it closed", err)
	}
	_ = staying.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := staying.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the connection of an admitted node: read = %v, want it open", err)
	}
}
