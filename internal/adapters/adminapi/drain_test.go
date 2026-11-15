package adminapi_test

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/adapters/httpdrain"
	"github.com/albievan/cactusfs/internal/domain/audit"
)

// rawUpload is an upload through the management API sent over a connection
// of its own.
type rawUpload struct {
	path    string
	size    int
	headers map[string]string
	// expect sends "Expect: 100-continue" and no body.
	expect bool
	// meanwhile reads the answer while the body is being written; else the
	// whole body is written before the answer is read.
	meanwhile bool
}

type rawUploadAnswer struct {
	resp *http.Response
	code string
	sent error
	conn net.Conn
	r    *bufio.Reader
}

func (f *fixture) rawUpload(u rawUpload) rawUploadAnswer {
	f.t.Helper()
	transport, ok := f.Client().Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		f.t.Fatal("the fixture's client has no TLS configuration")
	}
	config := transport.TLSClientConfig.Clone()
	// The test server's certificate is for example.com.
	config.ServerName = "example.com"
	dialer := tls.Dialer{Config: config}
	conn, err := dialer.DialContext(f.t.Context(), "tcp", f.Listener.Addr().String())
	if err != nil {
		f.t.Fatalf("dial: %v", err)
	}
	f.t.Cleanup(func() { _ = conn.Close() })
	// Nothing waits for longer: a hang fails the test.
	if err := conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		f.t.Fatal(err)
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+f.root)
	header.Set("Content-Type", "application/octet-stream")
	for name, value := range u.headers {
		header.Set(name, value)
	}
	if u.expect {
		header.Set("Expect", "100-continue")
	}
	var head bytes.Buffer
	fmt.Fprintf(&head, "PUT %s%s HTTP/1.1\r\nHost: example.com\r\nContent-Length: %d\r\n", adminapi.BasePath, u.path, u.size)
	if err := header.Write(&head); err != nil {
		f.t.Fatal(err)
	}
	head.WriteString("\r\n")
	if _, err := conn.Write(head.Bytes()); err != nil {
		f.t.Fatalf("writing the head: %v", err)
	}
	body := bytes.Repeat([]byte{'a'}, u.size)
	a := rawUploadAnswer{conn: conn, r: bufio.NewReader(conn)}
	switch {
	case u.expect:
	case u.meanwhile:
		// What the write ends with is no matter: the server closes.
		go func() { _, _ = conn.Write(body) }()
	default:
		_, a.sent = conn.Write(body)
	}
	resp, err := http.ReadResponse(a.r, nil)
	if err != nil {
		f.t.Fatalf("no answer (body sent: %v): %v", a.sent, err)
	}
	a.resp = resp
	data, err := io.ReadAll(resp.Body)
	// The body is read to its end; closing it leaves the connection open.
	_ = resp.Body.Close()
	if err != nil {
		f.t.Fatalf("reading the answer: %v", err)
	}
	var p struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(data, &p) == nil {
		a.code = p.Code
	}
	return a
}

// TestAnEarlyRefusalOfAnUploadReachesItsClient: an upload through the
// management API answered before its body is read - refused by the quota or
// for a missing bucket, or answered again for its idempotency key - reaches
// a client that writes its whole body before reading as the answer, not as a
// reset connection, and the connection serves the next request. A body past
// the bound is not read and the connection is closed; a client that asked
// "100-continue" is answered without sending anything.
func TestAnEarlyRefusalOfAnUploadReachesItsClient(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	cases := []struct {
		name   string
		upload rawUpload
		// first is sent first, with the same key: the upload is then
		// answered from what was kept.
		first  bool
		status int
		code   string
		kept   bool
	}{
		{name: "quota", upload: rawUpload{path: "/buckets/full/object?key=more", size: mib}, status: http.StatusForbidden, code: "QuotaExceeded", kept: true},
		{name: "no such bucket", upload: rawUpload{path: "/buckets/nowhere/object?key=x", size: mib}, status: http.StatusNotFound, code: "NoSuchBucket", kept: true},
		{
			name: "a repeat answered from what was kept", first: true,
			upload: rawUpload{path: "/buckets/open/object?key=once", size: mib, headers: map[string]string{"Idempotency-Key": "upload-1"}},
			status: http.StatusOK, kept: true,
		},
		{
			name: "asks for 100-continue", upload: rawUpload{path: "/buckets/full/object?key=more", size: mib, expect: true},
			status: http.StatusForbidden, code: "QuotaExceeded",
		},
		{
			name: "past the bound", upload: rawUpload{path: "/buckets/full/object?key=more", size: httpdrain.DefaultBytes + mib, meanwhile: true},
			status: http.StatusForbidden, code: "QuotaExceeded",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			for _, name := range []string{"full", "open"} {
				f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": name}})
			}
			if err := f.meta.SetBucketQuota(t.Context(), "full", 5, audit.Record{}); err != nil {
				t.Fatalf("SetBucketQuota: %v", err)
			}
			if c.first {
				if a := f.rawUpload(c.upload); a.resp.StatusCode != http.StatusOK {
					t.Fatalf("the first upload = %d %s", a.resp.StatusCode, a.code)
				}
			}

			begun := time.Now()
			a := f.rawUpload(c.upload)
			if a.resp.StatusCode != c.status || a.code != c.code {
				t.Fatalf("answer = %d %q, want %d %q", a.resp.StatusCode, a.code, c.status, c.code)
			}
			if c.first && a.resp.Header.Get("Idempotent-Replayed") != "true" {
				t.Fatal("the repeat was carried out again, not answered from what was kept")
			}
			if took := time.Since(begun); took > 10*time.Second {
				t.Errorf("answered after %v", took)
			}
			if c.kept {
				if a.sent != nil {
					t.Errorf("the body was not taken: %v", a.sent)
				}
				if a.resp.Close {
					t.Fatalf("the answer closes the connection (Connection: %q)", a.resp.Header.Get("Connection"))
				}
				if _, err := io.WriteString(a.conn, "GET "+adminapi.BasePath+"/openapi.json HTTP/1.1\r\nHost: example.com\r\n\r\n"); err != nil {
					t.Fatalf("a second request: %v", err)
				}
				next, err := http.ReadResponse(a.r, nil)
				if err != nil {
					t.Fatalf("no answer to a second request on the connection: %v", err)
				}
				_ = next.Body.Close()
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
		})
	}
}
