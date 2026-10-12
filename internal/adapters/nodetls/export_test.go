package nodetls

// ReadWith replaces how a Reloader reads its files, for a test of a reading
// that does not end.
func (r *Reloader) ReadWith(read func(path string) ([]byte, error)) {
	r.read = read
}

// PeerRefusalOf sorts the error of a failed handshake, as the listener does.
func PeerRefusalOf(err error) PeerRefusal {
	return peerRefusalOf(err)
}
