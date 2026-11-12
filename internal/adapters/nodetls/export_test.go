package nodetls

import (
	"context"
	"net"
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
// address it listens on, with at most handshakes handshakes at once.
func (i *Identity) ListenOn(ctx context.Context, inner net.Listener, allow func(string) bool, handshakes int) net.Listener {
	return i.serve(ctx, inner, allow, handshakes)
}
