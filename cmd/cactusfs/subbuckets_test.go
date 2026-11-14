package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/s3api"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// TestTheNodeAuthorizerReadsSubBucketPolicies holds the composition root to
// the wiring of ADR-0017 that no use case can see from inside, and the
// wiring walk does not either: the authorizer of a node, as the binary
// starts it, decides a request in a sub-bucket from the sub-bucket's policy
// too. Under a bucket whose policy lets anybody read it, a sub-bucket whose
// policy denies everybody its keys is refused to an anonymous client of the
// S3 API, served as server.go serves it; a key outside it is still read. An
// authorizer given the bucket's policies alone, or nothing, would let the
// GET in the sub-bucket through.
func TestTheNodeAuthorizerReadsSubBucketPolicies(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	n := mustStart(t, nodeConfig(t.TempDir())).node
	root := iam.Principal{AccessKeyID: "ROOT", Root: true}
	if err := n.buckets.Create(ctx, root, "pub"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	public := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/*"}]}`
	if err := n.buckets.SetPolicy(ctx, root, "pub", []byte(public)); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	private := `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/private/*"}]}`
	if _, err := n.subBuckets.Put(ctx, root, "pub", "private", []byte(private)); err != nil {
		t.Fatalf("Put of the sub-bucket: %v", err)
	}
	for _, key := range []string{"open.txt", "private/salaries.csv"} {
		content := []byte("content of " + key)
		if _, err := n.objects.Put(ctx, root, app.PutInput{Bucket: "pub", Key: key, Body: bytes.NewReader(content), Size: int64(len(content))}); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}
	handler := s3api.NewHandler(s3api.Config{
		Buckets: n.buckets, Objects: n.objects, Identities: n.identities, Clock: n.clock, Logger: slog.New(slog.DiscardHandler),
	})

	tests := map[string]struct {
		path string
		want int
	}{
		"outside the sub-bucket, the bucket's Allow lets anybody in": {path: "/pub/open.txt", want: http.StatusOK},
		"in the sub-bucket, its Deny holds for anybody":              {path: "/pub/private/salaries.csv", want: http.StatusForbidden},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			body, _ := io.ReadAll(rec.Result().Body)
			if rec.Code != tt.want {
				t.Fatalf("anonymous GET %s = %d, want %d: %s", tt.path, rec.Code, tt.want, body)
			}
			if tt.want == http.StatusForbidden && bytes.Contains(body, []byte("content of")) {
				t.Errorf("a refusal carries the object: %s", body)
			}
		})
	}
}

// servedAdmin starts a node as the binary starts it, with the management
// API on a plaintext loopback listener, and waits until it is ready. It
// returns a caller of that API, the bearer credential of root, and a stop
// that ends the node and fails the test if it did not end well.
func servedAdmin(t *testing.T) (call func(method, path, bearer, body string) (int, string), root string, stop func()) {
	t.Helper()
	cfg := nodeConfig(t.TempDir())
	cfg.AdminAddr, cfg.S3Addr = freeAddr(t), freeAddr(t)
	cfg.DevPlaintext = true
	cfg.DrainDelay, cfg.ShutdownTimeout = 0, 5*time.Second

	ctx, cancel := context.WithCancel(t.Context())
	logs := &lockedBuffer{buf: &bytes.Buffer{}}
	notice := &lockedBuffer{buf: &bytes.Buffer{}}
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, cfg, logs, notice) }()

	admin := "http://" + cfg.AdminAddr
	call = func(method, path, bearer, body string) (int, string) {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, admin+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer func() { _ = resp.Body.Close() }()
		answer, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(answer)
	}
	stop = func() {
		t.Helper()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runServer: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the node did not stop within 20 s")
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if status, _ := call(http.MethodGet, "/readyz", "", ""); status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			stop()
			logs.mu.Lock()
			t.Fatalf("the node did not become ready; log:\n%s", logs.buf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	notice.mu.Lock()
	creds := shownCredentials(t, notice.buf.String())
	notice.mu.Unlock()
	return call, creds.AccessKeyID + ":" + creds.SecretKey, stop
}

// TestTheServedAdminAPICreatesSubBuckets holds server.go to the wiring of
// PLAN C.6: the management API of a node started as the binary starts it is
// given the use case of sub-buckets, so a PUT of one answers 200 and the
// list names it - not the 501 of a node without the use case.
func TestTheServedAdminAPICreatesSubBuckets(t *testing.T) {
	t.Parallel()
	call, root, stop := servedAdmin(t)
	defer stop()

	if status, body := call(http.MethodPost, "/api/v1/buckets", root, `{"name":"parent"}`); status != http.StatusCreated {
		t.Fatalf("POST /buckets = %d %s", status, body)
	}
	if status, body := call(http.MethodPut, "/api/v1/buckets/parent/subbuckets/reports", root, `{}`); status != http.StatusOK {
		t.Fatalf("PUT a sub-bucket on the served node = %d %s, want 200", status, body)
	}
	if status, body := call(http.MethodGet, "/api/v1/buckets/parent/subbuckets", root, ""); status != http.StatusOK || !strings.Contains(body, `"name":"reports"`) {
		t.Errorf("the list of sub-buckets = %d %s", status, body)
	}
}

// TestTheServedAdminAPIKeepsTheCatalogue holds server.go and node.go to the
// wiring of PLAN D.4: the management API of a node started as the binary
// starts it is given the use case of the catalogue of tags - a definition
// is made and listed, not answered 501 - and the object service the same
// catalogue, so a bucket that takes only its tags takes a tag it defines. A
// bucket made through the API lower-cases its tags.
func TestTheServedAdminAPIKeepsTheCatalogue(t *testing.T) {
	t.Parallel()
	call, root, stop := servedAdmin(t)
	defer stop()

	if status, body := call(http.MethodPost, "/api/v1/tag-definitions", root, `{"key":"stage","values":["draft","final"]}`); status != http.StatusCreated {
		t.Fatalf("POST /tag-definitions on the served node = %d %s, want 201", status, body)
	}
	if status, body := call(http.MethodGet, "/api/v1/tag-definitions", root, ""); status != http.StatusOK || !strings.Contains(body, `"key":"stage"`) {
		t.Errorf("the catalogue = %d %s", status, body)
	}
	if status, body := call(http.MethodPost, "/api/v1/buckets", root, `{"name":"documents"}`); status != http.StatusCreated {
		t.Fatalf("POST /buckets = %d %s", status, body)
	}
	if status, body := call(http.MethodGet, "/api/v1/buckets/documents/tag-rules", root, ""); status != http.StatusOK || !strings.Contains(body, `"lowercase":true`) {
		t.Errorf("the rules of a bucket made through the API = %d %s", status, body)
	}
	rules := `{"lowercase":true,"catalogue_only":true,"required":[]}`
	if status, body := call(http.MethodPut, "/api/v1/buckets/documents/tag-rules", root, rules); status != http.StatusOK {
		t.Fatalf("PUT the rules = %d %s", status, body)
	}
	if status, body := call(http.MethodPut, "/api/v1/buckets/documents/object?key=report", root, "content"); status != http.StatusOK {
		t.Fatalf("an upload = %d %s", status, body)
	}
	tags := func(pairs string) string { return `{"tags":[` + pairs + `]}` }
	if status, body := call(http.MethodPut, "/api/v1/buckets/documents/object/tags?key=report", root, tags(`{"key":"Stage","value":"Draft"}`)); status != http.StatusOK ||
		!strings.Contains(body, `{"key":"stage","value":"draft"}`) {
		t.Errorf("a tag of the catalogue = %d %s", status, body)
	}
	if status, body := call(http.MethodPut, "/api/v1/buckets/documents/object/tags?key=report", root, tags(`{"key":"colour","value":"red"}`)); status != http.StatusBadRequest ||
		!strings.Contains(body, `"TagRuleViolated"`) {
		t.Errorf("a tag outside the catalogue = %d %s", status, body)
	}
}
