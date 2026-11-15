// Package httpdrain takes what is left of a request body off the wire before
// an answer that does not need it, so that a client still sending its upload
// hears the answer instead of a reset connection.
//
// net/http reads at most 256 KiB of a body the handler left unread; past that
// it closes the connection, and a client that is still writing - an S3 SDK
// sends bodies under 2 MiB without "Expect: 100-continue" - sees the reset
// and never the 403 that refused it. Drain reads the rest of such a body
// within a bound of bytes and of time, and otherwise closes the connection
// as before: a refusal costs at most Bytes bytes and Time of a connection,
// whatever the client does.
//
// A client that asks "Expect: 100-continue" is drained as well: it waits for
// "100 Continue" only a while (aws-sdk-go-v2's transport a second, as RFC
// 9110 allows) and then sends anyway, so an answer slower than that would
// reach it as a reset. Reading its body makes net/http send "100 Continue"
// first, so a client that waits is invited to send what fits the bounds -
// the same bounds as any other - and hears the answer on a connection that
// stays open.
package httpdrain

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultBytes is the most of a body read before an early answer. It
	// is four times the 2 MiB from which aws-sdk-go-v2 asks for
	// "100-continue", so that every body an SDK sends without asking fits,
	// with room for the framing of aws-chunked content. A body that asked
	// and is declared longer is not invited to be sent.
	DefaultBytes = 8 << 20
	// DefaultTime is the longest a body is read before an early answer:
	// 8 MiB at 13 Mbit/s, 2 MiB at 3.4 Mbit/s. A client that trickles is
	// not waited for longer.
	DefaultTime = 5 * time.Second
)

// Bounds is how much of a body, and for how long, is read before an answer
// that does not need it.
type Bounds struct {
	// Bytes is the most that is read; DefaultBytes when not above 0.
	Bytes int64
	// Time is the longest that is read for; DefaultTime when not above 0.
	Time time.Duration
}

func (b Bounds) bytes() int64 {
	if b.Bytes > 0 {
		return b.Bytes
	}
	return DefaultBytes
}

func (b Bounds) time() time.Duration {
	if b.Time > 0 {
		return b.Time
	}
	return DefaultTime
}

// Drain reads and discards what is left of r's body, at most b's bytes for
// at most b's time, and is called before an answer is written that does not
// need the body. consumed is how much of the body the handler has read
// already, or 0 when it does not know. w must reach the server's own
// ResponseWriter through Unwrap, which sets the read deadline.
//
// A request that asked "Expect: 100-continue" is read like any other, with
// a known length or chunked: the first read makes net/http send "100
// Continue" (Drain must run before the answer is written, which stops it),
// so a client that waits sends its body and one that did not wait has it
// read; either hears the answer on a connection that stays open. One whose
// declared length is over the bound is not invited: nothing is read, and
// net/http answers it without "100 Continue" and closes. Nothing is read
// over HTTP/2, which ends one stream and not the connection, nor for an
// HTTP/1.0 request asking "100-continue", to which net/http sends no "100
// Continue". Where the rest of the body does not fit the bounds, or the
// deadline cannot be set, nothing more is read and the answer carries
// "Connection: close", as net/http would have done. It reports whether the
// body was read to its end.
func (b Bounds) Drain(w http.ResponseWriter, r *http.Request, consumed int64) bool {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 || r.ProtoMajor != 1 {
		return false
	}
	if expectsContinue(r) && !r.ProtoAtLeast(1, 1) {
		// A client that waits would be waited for until the deadline.
		return false
	}
	limit := b.bytes()
	if r.ContentLength > 0 && r.ContentLength-consumed > limit {
		w.Header().Set("Connection", "close")
		return false
	}
	transfer := http.NewResponseController(w)
	// A network deadline is wall-clock time, whatever clock the handler
	// keeps.
	if err := transfer.SetReadDeadline(time.Now().Add(b.time())); err != nil {
		w.Header().Set("Connection", "close")
		return false
	}
	// The body is never kept: what is read is thrown away.
	n, err := io.CopyN(io.Discard, r.Body, limit+1)
	if !errors.Is(err, io.EOF) || n > limit {
		// Too long, too slow, or cut short: the connection goes. Its
		// read deadline stays where it is, so that nothing more is read
		// from it.
		w.Header().Set("Connection", "close")
		return false
	}
	// The whole body is read; the server sets the deadlines of the next
	// request itself.
	_ = transfer.SetReadDeadline(time.Time{})
	return true
}

// expectsContinue reports whether r asks to be told before sending its body.
func expectsContinue(r *http.Request) bool {
	for _, value := range r.Header.Values("Expect") {
		for token := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "100-continue") {
				return true
			}
		}
	}
	return false
}
