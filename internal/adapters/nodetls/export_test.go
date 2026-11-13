package nodetls

import (
	"context"
	"net"
	"time"
)

// ReadWith replaces how a Reloader reads its files, for a test of a reading
// that does not end.
func (r *Reloader) ReadWith(read func(path string) ([]byte, error)) {
	r.read = read
}

// PeerRefusalOf sorts the error of a failed handshake, as the listener does.
func PeerRefusalOf(err error) PeerRefusal {
	return peerRefusalOf(err)
}

// ListenOn serves the connections of inner as Listen serves those of the
// address it listens on, with at most handshakes handshakes at once, from
// one source or from all, and asks allow again about the open ones every
// recheck.
func (i *Identity) ListenOn(
	ctx context.Context, inner net.Listener, allow func(string) bool, handshakes int, recheck time.Duration,
) net.Listener {
	limits := defaultLimits()
	limits.total, limits.perSource, limits.recheck = handshakes, handshakes, recheck
	return i.serve(ctx, inner, allow, limits)
}

// ListenLimited serves the connections of inner with at most total
// handshakes at once, at most perSource of them from one source as source
// names it, and turns away a caller silent for firstByte.
func (i *Identity) ListenLimited(
	ctx context.Context, inner net.Listener, total, perSource int, firstByte time.Duration, source func(net.Addr) string,
) net.Listener {
	limits := defaultLimits()
	limits.total, limits.perSource, limits.firstByte, limits.source = total, perSource, firstByte, source
	return i.serve(ctx, inner, func(string) bool { return true }, limits)
}
