package httpdrain

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// exchange is one request sent over a connection of its own, as a client
// that writes its body itself does.
type exchange struct {
	// head is the request line and the headers, without the blank line
	// that ends them.
	head string
	// send writes the body; nil sends none.
	send func(w io.Writer) error
	// sentFirst waits for the whole body to be written before the answer
	// is read, as a client does that writes its request before it reads.
	sentFirst bool
}

// outcome is what the client of an exchange saw.
type outcome struct {
	resp *http.Response
	body []byte
	// sendErr is what writing the body ended with, when it was waited for.
	sendErr error
	// conn is the connection, with r reading from it, for what comes next.
	conn net.Conn
	r    *bufio.Reader
	sent chan error
}

func run(t *testing.T, addr string, x exchange) *outcome {
	t.Helper()
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// Nothing in a test waits for longer: a hang fails it.
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatalf("deadline: %v", err)
	}
	if _, err := io.WriteString(conn, x.head+"\r\n"); err != nil {
		t.Fatalf("writing the head: %v", err)
	}
	o := &outcome{conn: conn, r: bufio.NewReader(conn), sent: make(chan error, 1)}
	if x.send == nil {
		o.sent <- nil
	} else {
		go func() { o.sent <- x.send(conn) }()
	}
	if x.sentFirst {
		o.sendErr = <-o.sent
		if o.sendErr != nil {
			t.Fatalf("the body was not taken: %v", o.sendErr)
		}
		o.sent <- nil
	}
	resp, err := http.ReadResponse(o.r, nil)
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	o.resp = resp
	o.body, err = io.ReadAll(resp.Body)
	// The body is read to its end; closing it leaves the connection open.
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	return o
}

// kept checks that the connection serves another request.
func (o *outcome) kept(t *testing.T) {
	t.Helper()
	if o.resp.Close {
		t.Fatalf("the answer closes the connection (Connection: %q)", o.resp.Header.Get("Connection"))
	}
	if err := <-o.sent; err != nil {
		t.Fatalf("sending the body: %v", err)
	}
	if _, err := io.WriteString(o.conn, "GET /again HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatalf("a second request: %v", err)
	}
	resp, err := http.ReadResponse(o.r, nil)
	if err != nil {
		t.Fatalf("no answer to a second request on the connection: %v", err)
	}
	_ = resp.Body.Close()
}

// closed checks that the answer closes the connection, and that it is
// closed rather than left hanging.
func (o *outcome) closed(t *testing.T) {
	t.Helper()
	if !o.resp.Close {
		t.Fatalf("the answer keeps the connection (Connection: %q)", o.resp.Header.Get("Connection"))
	}
	if _, err := o.r.ReadByte(); err == nil {
		t.Fatal("the server sent more after an answer that closes")
	} else if errors.Is(err, net.ErrClosed) || isTimeout(err) {
		t.Fatalf("the connection was not closed: %v", err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func put(length int64, extra ...string) string {
	head := "PUT /x HTTP/1.1\r\nHost: test\r\n"
	if length >= 0 {
		head += fmt.Sprintf("Content-Length: %d\r\n", length)
	} else {
		head += "Transfer-Encoding: chunked\r\n"
	}
	for _, h := range extra {
		head += h + "\r\n"
	}
	return head
}

func whole(n int) func(io.Writer) error {
	return func(w io.Writer) error {
		_, err := w.Write(bytes.Repeat([]byte{'a'}, n))
		return err
	}
}

// endless writes until the connection refuses more.
func endless(chunked bool) func(io.Writer) error {
	return func(w io.Writer) error {
		block := bytes.Repeat([]byte{'a'}, 32<<10)
		if chunked {
			block = append(fmt.Appendf(nil, "%x\r\n", len(block)), append(block, "\r\n"...)...)
		}
		for {
			if _, err := w.Write(block); err != nil {
				return err
			}
		}
	}
}

// trickle writes one byte at a time, slowly.
func trickle(w io.Writer) error {
	for range 1 << 20 {
		if _, err := w.Write([]byte{'a'}); err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// TestDrain: an answer written before the body is read reaches a client
// still sending it, within the bounds; past them, or for a client that asked
// to be told before sending, nothing more is read and the connection closes.
func TestDrain(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	cases := []struct {
		name   string
		bounds Bounds
		x      exchange
		// drained is what Drain reports; kept whether the connection
		// serves another request (else it is closed).
		drained, kept bool
		// within is how soon the answer comes, at the latest.
		within time.Duration
	}{
		{
			name: "1 MiB sent whole before reading", x: exchange{head: put(mib), send: whole(mib), sentFirst: true},
			drained: true, kept: true, within: 10 * time.Second,
		},
		{
			name: "chunked, within the bound", x: exchange{head: put(-1), send: func(w io.Writer) error {
				_, err := io.WriteString(w, "5\r\nhello\r\n0\r\n\r\n")
				return err
			}, sentFirst: true},
			drained: true, kept: true, within: 10 * time.Second,
		},
		{
			name: "asks for 100-continue", x: exchange{head: put(mib, "Expect: 100-continue")},
			drained: false, kept: false, within: 5 * time.Second,
		},
		{
			name: "declares more than the bound", bounds: Bounds{Bytes: 64 << 10},
			x:       exchange{head: put(mib), send: endless(false)},
			drained: false, kept: false, within: 5 * time.Second,
		},
		{
			name: "chunked, past the bound", bounds: Bounds{Bytes: 64 << 10},
			x:       exchange{head: put(-1), send: endless(true)},
			drained: false, kept: false, within: 5 * time.Second,
		},
		{
			name: "trickles past the time", bounds: Bounds{Time: 300 * time.Millisecond},
			x:       exchange{head: put(mib), send: trickle},
			drained: false, kept: false, within: 5 * time.Second,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			drained := make(chan bool, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/x" {
					drained <- c.bounds.Drain(w, r, 0)
				}
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, "refused")
			}))
			t.Cleanup(server.Close)
			begun := time.Now()
			o := run(t, server.Listener.Addr().String(), c.x)
			if took := time.Since(begun); took > c.within {
				t.Errorf("answered after %v, want within %v", took, c.within)
			}
			if o.resp.StatusCode != http.StatusForbidden || string(o.body) != "refused" {
				t.Fatalf("answer = %d %q, want 403 \"refused\"", o.resp.StatusCode, o.body)
			}
			if got := <-drained; got != c.drained {
				t.Errorf("Drain = %v, want %v", got, c.drained)
			}
			if c.kept {
				o.kept(t)
			} else {
				o.closed(t)
			}
		})
	}
}

// TestDrainKeepsAReadBody: a body the handler has read is no reason to
// close the connection, and Drain reads nothing more.
func TestDrainKeepsAReadBody(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x" {
			n, _ := io.Copy(io.Discard, r.Body)
			if !(Bounds{}).Drain(w, r, n) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "refused")
	}))
	t.Cleanup(server.Close)
	o := run(t, server.Listener.Addr().String(), exchange{head: put(100), send: whole(100), sentFirst: true})
	if o.resp.StatusCode != http.StatusForbidden {
		t.Fatalf("answer = %d, want 403", o.resp.StatusCode)
	}
	o.kept(t)
}

func TestExpectsContinue(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		values []string
		want   bool
	}{
		{nil, false},
		{[]string{"100-continue"}, true},
		{[]string{"100-Continue"}, true},
		{[]string{"foo, 100-continue"}, true},
		{[]string{"foo", " 100-continue "}, true},
		{[]string{"200-ok"}, false},
		{[]string{"100-continued"}, false},
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/", strings.NewReader(""))
		for _, v := range c.values {
			r.Header.Add("Expect", v)
		}
		if got := expectsContinue(r); got != c.want {
			t.Errorf("expectsContinue(%q) = %v, want %v", c.values, got, c.want)
		}
	}
}
