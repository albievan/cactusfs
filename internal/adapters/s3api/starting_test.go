package s3api

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStarting: until the node has started, every request is answered 503
// as S3 answers errors, and none is read or authenticated.
func TestStarting(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		method, target string
		headers        map[string]string
	}{
		"list buckets":            {http.MethodGet, "/", nil},
		"an upload":               {http.MethodPut, "/bucket/key", nil},
		"a signed request":        {http.MethodGet, "/bucket/key", map[string]string{"Authorization": "AWS4-HMAC-SHA256 Credential=whoever"}},
		"a request with no body":  {http.MethodHead, "/bucket/key", nil},
		"a preflight":             {http.MethodOptions, "/bucket/key", map[string]string{"Origin": "https://example.test"}},
		"what is not S3 at all":   {http.MethodPost, "/../../etc", nil},
		"a presigned-looking URL": {http.MethodGet, "/bucket/key?X-Amz-Signature=abc", nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := &unread{Reader: strings.NewReader("an upload that must not be read")}
			req := httptest.NewRequestWithContext(t.Context(), tt.method, "http://s3.test"+tt.target, body)
			for key, value := range tt.headers {
				req.Header.Set(key, value)
			}
			rec := httptest.NewRecorder()
			Starting().ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" || rec.Header().Get("Connection") != "close" {
				t.Errorf("status %d, headers %v", rec.Code, rec.Header())
			}
			if body.read {
				t.Error("the request body was read")
			}
			if rec.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Error("a cross-origin answer from a node that knows no bucket")
			}
			if tt.method == http.MethodHead {
				if rec.Body.Len() != 0 {
					t.Errorf("a body in the answer to HEAD: %q", rec.Body.String())
				}
				return
			}
			var answer errorResponse
			if err := xml.Unmarshal(rec.Body.Bytes(), &answer); err != nil || answer.Code != "ServiceUnavailable" {
				t.Errorf("answer %q (%v)", rec.Body.String(), err)
			}
		})
	}
}

type unread struct {
	*strings.Reader
	read bool
}

func (u *unread) Read(p []byte) (int, error) {
	u.read = true
	return u.Reader.Read(p)
}
