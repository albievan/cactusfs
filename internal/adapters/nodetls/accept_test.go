package nodetls_test

import (
	"context"
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
	listener := server.ListenOn(ctx, inner, func(string) (bool, error) { return true, nil }, 4, time.Hour)
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
	listener := server.ListenOn(ctx, tcpListener(t), func(string) (bool, error) { return true, nil }, 1, time.Hour)
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
	allow := func(nodeID string) (bool, error) { return nodeID != "meta-2" || !removed.Load(), nil }
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

// TestOneSourceCannotTakeEveryHandshake: one host that can reach the Raft
// port and opens connections that say nothing holds at most the handshakes
// of one source; a node calling from another source still gets through
// (review of 2026-11-13: a single global bound let one host refuse every
// peer and stall elections). The source of a connection is injected: every
// connection here comes from 127.0.0.1, and 127.0.0.2 is not on every host.
func TestOneSourceCannotTakeEveryHandshake(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ca := authority(t)
	server, client := identity(t, ca, "meta-1"), identity(t, ca, "meta-2")
	var fromPeer atomic.Bool
	source := func(net.Addr) string {
		if fromPeer.Load() {
			return "peer"
		}
		return "attacker"
	}
	const perSource, extra = 4, 3
	// The silent callers are not turned away for their silence while the
	// node calls: only the bound of their source holds them back.
	listener := server.ListenLimited(ctx, tcpListener(t), 64, perSource, time.Minute, source)
	t.Cleanup(func() { _ = listener.Close() })
	address := listener.Addr().String()

	for range perSource + extra {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
	}
	// Every silent caller has been counted once the extra ones are refused.
	deadline := time.Now().Add(10 * time.Second)
	for server.PeersRefused(nodetls.PeerRefusedHandshake) < extra {
		if time.Now().After(deadline) {
			t.Fatalf("handshakes refused = %d, want %d: callers past their source's bound were not turned away",
				server.PeersRefused(nodetls.PeerRefusedHandshake), extra)
		}
		time.Sleep(5 * time.Millisecond)
	}

	fromPeer.Store(true)
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := client.Dial(dialCtx, nodetls.Address("meta-1", address))
	if err != nil {
		t.Fatalf("a node from another source was refused while one source held its handshakes: %v", err)
	}
	defer func() { _ = conn.Close() }()
	select {
	case held := <-accepted:
		_ = held.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("the node was not handed out")
	}
	if got := server.PeersRefused(nodetls.PeerRefusedHandshake); got != extra {
		t.Errorf("handshakes refused = %d, want %d", got, extra)
	}
}

// TestASilentCallerIsTurnedAwayAtTheFirstByteDeadline: a caller that sends
// no ClientHello is closed after the short wait for its first byte, not
// after the whole handshake's ten seconds; a node that speaks at once is
// not hurried by it.
func TestASilentCallerIsTurnedAwayAtTheFirstByteDeadline(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ca := authority(t)
	server, client := identity(t, ca, "meta-1"), identity(t, ca, "meta-2")
	listener := server.ListenLimited(ctx, tcpListener(t), 64, 64, 200*time.Millisecond, nodetls.SourceOf)
	t.Cleanup(func() { _ = listener.Close() })
	address := listener.Addr().String()

	silent, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()
	start := time.Now()
	_ = silent.SetReadDeadline(time.Now().Add(9 * time.Second))
	if _, err := silent.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a silent caller: read = %v, want it closed", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("a silent caller was held %v; the first-byte wait is 200ms", waited)
	}
	if got := server.PeersRefused(nodetls.PeerRefusedTimeout); got != 1 {
		t.Errorf("timeouts = %d, want 1", got)
	}

	// A node's handshake is unaffected, and the connection it gets carries
	// no deadline of the handshake.
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	conn, err := client.Dial(ctx, nodetls.Address("meta-1", address))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var held net.Conn
	select {
	case held = <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the node was not handed out")
	}
	defer func() { _ = held.Close() }()
	time.Sleep(400 * time.Millisecond) // past the first-byte wait
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = held.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := held.Read(make([]byte, 1)); err != nil {
		t.Errorf("a node's connection after the first-byte wait: read = %v", err)
	}
}

// TestSourceOf: IPv4 is counted by address, IPv6 by /64.
func TestSourceOf(t *testing.T) {
	t.Parallel()
	tcp := func(ip string) net.Addr { return &net.TCPAddr{IP: net.ParseIP(ip), Port: 1234} }
	for _, tc := range []struct {
		name string
		a, b net.Addr
		same bool
	}{
		{"one IPv4 address, two ports", tcp("192.0.2.1"), &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 9}, true},
		{"two IPv4 addresses", tcp("192.0.2.1"), tcp("192.0.2.2"), false},
		{"IPv4 and the same mapped into IPv6", tcp("192.0.2.1"), tcp("::ffff:192.0.2.1"), true},
		{"one IPv6 /64", tcp("2001:db8:1:2::1"), tcp("2001:db8:1:2:ffff:ffff:ffff:ffff"), true},
		{"two IPv6 /64s", tcp("2001:db8:1:2::1"), tcp("2001:db8:1:3::1"), false},
	} {
		if got := nodetls.SourceOf(tc.a) == nodetls.SourceOf(tc.b); got != tc.same {
			t.Errorf("%s: %s and %s are one source = %v, want %v",
				tc.name, nodetls.SourceOf(tc.a), nodetls.SourceOf(tc.b), got, tc.same)
		}
	}
}

// TestAnErrorOfAllowClosesNoOpenConnection: a store that cannot tell for a
// moment whether a node is admitted must not cut every node off from the
// others (review of 2026-11-13). An open connection is kept while allow
// fails and closed when it says no; a new caller allow cannot judge is
// turned away.
func TestAnErrorOfAllowClosesNoOpenConnection(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ca := authority(t)
	server, client := identity(t, ca, "meta-1"), identity(t, ca, "meta-2")
	const (
		admitted = iota
		failing
		refused
	)
	var state atomic.Int32
	var asked atomic.Int32
	allow := func(string) (bool, error) {
		asked.Add(1)
		switch state.Load() {
		case failing:
			return false, errors.New("the store cannot tell for now")
		case refused:
			return false, nil
		}
		return true, nil
	}
	listener := server.ListenOn(ctx, tcpListener(t), allow, 4, 20*time.Millisecond)
	t.Cleanup(func() { _ = listener.Close() })
	address := listener.Addr().String()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	conn, err := client.Dial(ctx, nodetls.Address("meta-1", address))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	select {
	case held := <-accepted:
		defer func() { _ = held.Close() }()
	case <-time.After(10 * time.Second):
		t.Fatal("the node was not handed out")
	}

	// allow fails for several rechecks: the connection stays open.
	state.Store(failing)
	before := asked.Load()
	for asked.Load() < before+5 {
		time.Sleep(5 * time.Millisecond)
	}
	_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("an open connection while allow fails: read = %v, want it open", err)
	}

	// A new caller is not admitted while allow cannot judge it. (TLS 1.3
	// completes the client's side before the server judges; the server
	// closes the connection then.)
	refusedBefore := server.PeersRefused(nodetls.PeerRefusedNotAdmitted)
	if other, err := identity(t, ca, "meta-3").Dial(ctx, nodetls.Address("meta-1", address)); err == nil {
		_ = other.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := other.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("a caller allow could not judge: read = %v, want it closed", err)
		}
		_ = other.Close()
	}
	if got := server.PeersRefused(nodetls.PeerRefusedNotAdmitted) - refusedBefore; got != 1 {
		t.Errorf("not admitted = %d, want 1", got)
	}

	// A definite no closes it.
	state.Store(refused)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the connection of a node no longer admitted: read = %v, want it closed", err)
	}
}
