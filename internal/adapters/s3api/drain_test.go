package s3api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/httpdrain"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/audit"
)

// rawPut is a signed PUT sent over a connection of its own, the way an SDK
// sends a body it does not ask "100-continue" for: the whole body is
// written, and only then is the answer read.
type rawPut struct {
	path    string
	size    int
	headers map[string]string
	// keyID and key sign the request; the root's when empty.
	keyID string
	key   []byte
	// expect sends "Expect: 100-continue"; the body follows at once unless
	// waits or withhold says otherwise.
	expect bool
	// waits reads "100 Continue" before the body is written, as a client
	// does that asked for it and waits.
	waits bool
	// withhold sends no body: the client waits to be told and never is.
	withhold bool
	// meanwhile reads the answer while the body is being written, as a
	// client must whose body is refused past the bound.
	meanwhile bool
}

// rawAnswer is what the client of a rawPut saw.
type rawAnswer struct {
	resp *http.Response
	code string
	// sent is what writing the body ended with.
	sent error
	// continued says that "100 Continue" came before the answer.
	continued bool
	conn      net.Conn
	r         *bufio.Reader
}

func (s *testServer) rawPut(ctx context.Context, p rawPut) rawAnswer {
	s.t.Helper()
	body := bytes.Repeat([]byte{'a'}, p.size)
	r, err := http.NewRequestWithContext(ctx, http.MethodPut, s.URL+p.path, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	r.Host = r.URL.Host
	for name, value := range p.headers {
		r.Header.Set(name, value)
	}
	keyID, key := s.keyID, s.key
	if p.keyID != "" {
		keyID, key = p.keyID, p.key
	}
	sum := sha256.Sum256(body)
	signRequest(r, keyID, key, serverNow, hex.EncodeToString(sum[:]))
	if p.expect {
		r.Header.Set("Expect", "100-continue")
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", s.Listener.Addr().String())
	if err != nil {
		s.t.Fatalf("dial: %v", err)
	}
	if s.TLS != nil {
		transport, ok := s.Client().Transport.(*http.Transport)
		if !ok || transport.TLSClientConfig == nil {
			s.t.Fatal("the test server's client has no TLS configuration")
		}
		// The test server's certificate is for example.com.
		config := transport.TLSClientConfig.Clone()
		config.ServerName = "example.com"
		conn = tls.Client(conn, config)
	}
	s.t.Cleanup(func() { _ = conn.Close() })
	// Nothing waits for longer: a hang fails the test.
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		s.t.Fatal(err)
	}
	var head strings.Builder
	fmt.Fprintf(&head, "PUT %s HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n", r.URL.RequestURI(), r.Host, p.size)
	if err := r.Header.Write(&head); err != nil {
		s.t.Fatal(err)
	}
	head.WriteString("\r\n")
	if _, err := io.WriteString(conn, head.String()); err != nil {
		s.t.Fatalf("writing the head: %v", err)
	}
	a := rawAnswer{conn: conn, r: bufio.NewReader(conn)}
	if p.waits {
		// Nothing is sent until the server says so.
		resp, err := http.ReadResponse(a.r, nil)
		if err != nil {
			s.t.Fatalf("no answer to the head: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusContinue {
			s.t.Fatalf("the head was answered %d, want 100 Continue", resp.StatusCode)
		}
		a.continued = true
	}
	switch {
	case p.withhold:
	case p.meanwhile:
		// What the write ends with is no matter: the server closes.
		go func() { _, _ = conn.Write(body) }()
	default:
		_, a.sent = conn.Write(body)
	}
	var resp *http.Response
	for {
		resp, err = http.ReadResponse(a.r, nil)
		if err != nil {
			s.t.Fatalf("no answer (body sent: %v): %v", a.sent, err)
		}
		if resp.StatusCode != http.StatusContinue {
			break
		}
		_ = resp.Body.Close()
		a.continued = true
	}
	a.resp = resp
	data, err := io.ReadAll(resp.Body)
	// The body is read to its end; closing it leaves the connection open.
	_ = resp.Body.Close()
	if err != nil {
		s.t.Fatalf("reading the answer: %v", err)
	}
	var e errorResponse
	if xml.Unmarshal(data, &e) == nil {
		a.code = e.Code
	}
	return a
}

// TestAnEarlyRefusalReachesAClientStillSending: a PUT refused before its
// body is read - by the quota, the authorizer, a missing bucket, a header -
// is answered with its S3 error, not with a reset connection, when the
// client writes its whole body before reading: the body is taken off the
// wire first, and the connection serves the next request. A client that
// asked "100-continue" is told to send and is answered the same, whether it
// waited or sent at once. A body past the bound is not read and the
// connection is closed; a client that asked "100-continue" for one is
// answered without being told to send.
func TestAnEarlyRefusalReachesAClientStillSending(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	cases := []struct {
		name   string
		secure bool
		put    rawPut
		// user signs with the key of a user who may do nothing.
		user   bool
		status int
		code   string
		// kept says the connection serves another request; else it is
		// closed.
		kept bool
		// continued is whether "100 Continue" came before the answer.
		continued bool
	}{
		{name: "quota", put: rawPut{path: "/full/one-more", size: mib}, status: http.StatusForbidden, code: "QuotaExceeded", kept: true},
		{name: "quota over TLS", secure: true, put: rawPut{path: "/full/one-more", size: mib}, status: http.StatusForbidden, code: "QuotaExceeded", kept: true},
		{name: "access denied", user: true, put: rawPut{path: "/full/one-more", size: mib}, status: http.StatusForbidden, code: "AccessDenied", kept: true},
		{name: "no such bucket", put: rawPut{path: "/nowhere/x", size: mib}, status: http.StatusNotFound, code: "NoSuchBucket", kept: true},
		{
			name: "a header not implemented", put: rawPut{path: "/full/x", size: mib, headers: map[string]string{"X-Amz-Unknown": "1"}},
			status: http.StatusNotImplemented, code: "NotImplemented", kept: true,
		},
		{
			name: "an invalid tag", put: rawPut{path: "/full/x", size: mib, headers: map[string]string{"X-Amz-Tagging": "a=" + strings.Repeat("v", 300)}},
			status: http.StatusBadRequest, code: "InvalidTag", kept: true,
		},
		{
			// As aws-sdk-go-v2 does from 2 MiB.
			name: "asks for 100-continue and waits", put: rawPut{path: "/full/one-more", size: 5 * mib, expect: true, waits: true},
			status: http.StatusForbidden, code: "QuotaExceeded", kept: true, continued: true,
		},
		{
			// As the SDK does once its second of waiting is out.
			name: "asks for 100-continue and sends at once", put: rawPut{path: "/full/one-more", size: 5 * mib, expect: true},
			status: http.StatusForbidden, code: "QuotaExceeded", kept: true, continued: true,
		},
		{
			name: "asks for 100-continue over TLS and waits", secure: true, put: rawPut{path: "/full/one-more", size: 5 * mib, expect: true, waits: true},
			status: http.StatusForbidden, code: "QuotaExceeded", kept: true, continued: true,
		},
		{
			name: "asks for 100-continue past the bound", put: rawPut{path: "/full/one-more", size: httpdrain.DefaultBytes + mib, expect: true, withhold: true},
			status: http.StatusForbidden, code: "QuotaExceeded",
		},
		{
			name: "past the bound", put: rawPut{path: "/full/one-more", size: httpdrain.DefaultBytes + mib, meanwhile: true},
			status: http.StatusForbidden, code: "QuotaExceeded",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			s := startTestServer(t, "", c.secure)
			s.createBucket("full")
			if err := s.meta.SetBucketQuota(ctx, "full", 5, audit.Record{}); err != nil {
				t.Fatalf("SetBucketQuota: %v", err)
			}
			if c.user {
				if _, err := s.iam.CreateUser(ctx, s.principal(), app.UserInput{Name: "nobody"}); err != nil {
					t.Fatal(err)
				}
				creds, _, err := s.iam.CreateAPIKey(ctx, s.principal(), app.APIKeyInput{Owner: "nobody"})
				if err != nil {
					t.Fatal(err)
				}
				c.put.keyID, c.put.key = creds.AccessKeyID, []byte(creds.SecretKey)
			}

			begun := time.Now()
			a := s.rawPut(ctx, c.put)
			if a.resp.StatusCode != c.status || a.code != c.code {
				t.Fatalf("answer = %d %s, want %d %s", a.resp.StatusCode, a.code, c.status, c.code)
			}
			if took := time.Since(begun); took > 10*time.Second {
				t.Errorf("answered after %v", took)
			}
			if a.continued != c.continued {
				t.Errorf("100 Continue sent = %v, want %v", a.continued, c.continued)
			}
			if c.kept {
				if a.sent != nil {
					t.Errorf("the body was not taken: %v", a.sent)
				}
				if a.resp.Close {
					t.Fatalf("the answer closes the connection (Connection: %q)", a.resp.Header.Get("Connection"))
				}
				// The connection serves the next request.
				if _, err := io.WriteString(a.conn, "HEAD /full HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
					t.Fatalf("a second request: %v", err)
				}
				next, err := http.ReadResponse(a.r, &http.Request{Method: http.MethodHead})
				if err != nil {
					t.Fatalf("no answer to a second request on the connection: %v", err)
				}
				_ = next.Body.Close()
				// The refused PUT was handled to its end before the
				// second request was read.
				if got := s.seen.lastOf("PutObject").BytesIn; c.put.expect && got != int64(c.put.size) {
					t.Errorf("%d bytes of the body were read, want all %d", got, c.put.size)
				}
				return
			}
			if !a.resp.Close {
				t.Fatalf("the answer keeps the connection (Connection: %q)", a.resp.Header.Get("Connection"))
			}
			_, err := a.r.ReadByte()
			var ne net.Error
			switch {
			case err == nil:
				t.Fatal("more was sent after an answer that closes")
			case errors.As(err, &ne) && ne.Timeout():
				t.Fatalf("the connection was left open: %v", err)
			}
			if c.put.withhold {
				if got := s.seen.last().BytesIn; got != 0 {
					t.Errorf("%d bytes of the body were read; the client was never told to send it", got)
				}
			}
		})
	}
}

// lastOf is the last observation of the operation, or none.
func (o *observations) lastOf(operation string) Observation {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := len(o.list) - 1; i >= 0; i-- {
		if o.list[i].Operation == operation {
			return o.list[i]
		}
	}
	return Observation{}
}
