package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestListenersOpenBeforeTheNodeHasJoined: a member of a cluster whose
// peers cannot be reached is found alive and not ready by whatever watches
// it, and serves nothing else - not a stale answer, not an empty one, and
// nothing to anybody without the node behind it. (PLAN 7.0.)
func TestListenersOpenBeforeTheNodeHasJoined(t *testing.T) {
	t.Parallel()
	c := newTestCluster(t, fourNodes())
	cfg := c.configs["n4"] // a gateway; the other three never start
	cfg.AdminAddr, cfg.S3Addr = freeAddr(t), freeAddr(t)
	cfg.DevPlaintext = true
	cfg.DrainDelay, cfg.ShutdownTimeout = time.Minute, 5*time.Second

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logs := &lockedBuffer{buf: &bytes.Buffer{}}
	logText := func() string {
		logs.mu.Lock()
		defer logs.mu.Unlock()
		return logs.buf.String()
	}
	done := make(chan error, 1)
	began := time.Now()
	go func() { done <- runServer(ctx, cfg, logs, io.Discard) }()

	call := func(method, url string) (int, http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, nil
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, body
	}
	admin, s3 := "http://"+cfg.AdminAddr, "http://"+cfg.S3Addr
	for {
		if status, _, _ := call(http.MethodGet, admin+"/healthz"); status == http.StatusOK {
			break
		}
		if time.Since(began) > 5*time.Second {
			t.Fatalf("no /healthz within 5 s of the start; log:\n%s", logText())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if status, _, body := call(http.MethodGet, admin+"/readyz"); status != http.StatusServiceUnavailable {
		t.Errorf("/readyz of a node that has not joined = %d %s, want 503", status, body)
	}
	for _, path := range []string{"/api/v1/buckets", "/api/v1/openapi.json", "/metrics", "/docs", "/"} {
		status, header, body := call(http.MethodGet, admin+path)
		var problem struct {
			Status int    `json:"status"`
			Code   string `json:"code"`
		}
		if status != http.StatusServiceUnavailable || header.Get("Content-Type") != "application/problem+json" ||
			json.Unmarshal(body, &problem) != nil || problem.Status != 503 || problem.Code != "ServiceUnavailable" || header.Get("Retry-After") == "" {
			t.Errorf("admin %s of a node that has not joined = %d %q %s", path, status, header.Get("Content-Type"), body)
		}
	}
	for _, tt := range []struct{ method, path string }{
		{http.MethodGet, "/"}, {http.MethodGet, "/bucket/key"}, {http.MethodPut, "/bucket/key"}, {http.MethodHead, "/bucket"},
	} {
		status, header, body := call(tt.method, s3+tt.path)
		var answer struct {
			XMLName xml.Name `xml:"Error"`
			Code    string
		}
		wellFormed := tt.method == http.MethodHead || (xml.Unmarshal(body, &answer) == nil && answer.Code == "ServiceUnavailable")
		if status != http.StatusServiceUnavailable || !wellFormed || header.Get("Retry-After") == "" {
			t.Errorf("S3 %s %s of a node that has not joined = %d %s", tt.method, tt.path, status, body)
		}
	}
	// "listening" is what says that the node is served; it has not been
	// said of either. (The node's own listeners, for its peers, are another
	// matter.)
	if text := logText(); !strings.Contains(text, "listener open; the node is starting") ||
		strings.Contains(text, `"msg":"listening","listener":"admin"`) || strings.Contains(text, `"msg":"listening","listener":"s3"`) {
		t.Errorf("the log of a node that has not joined:\n%s", text)
	}

	// Told to stop while it waits, it stops at once - there is nobody to
	// drain, whatever the drain delay - and as a node that was told to
	// stop does: without an error.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a node told to stop while it was starting: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a node that never joined did not stop within 20 s (drain delay one minute)")
	}
}

// TestNodeServesOnceItHasStarted: the other half. The listeners that
// refused begin to serve the node, in an order nobody can be misled by, and
// a node that has started drains when it is told to stop.
func TestNodeServesOnceItHasStarted(t *testing.T) {
	t.Parallel()
	cfg := nodeConfig(t.TempDir())
	cfg.AdminAddr, cfg.S3Addr = freeAddr(t), freeAddr(t)
	cfg.DevPlaintext = true
	cfg.DrainDelay, cfg.ShutdownTimeout = 2*time.Second, 5*time.Second

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logs := &lockedBuffer{buf: &bytes.Buffer{}}
	logText := func() string {
		logs.mu.Lock()
		defer logs.mu.Unlock()
		return logs.buf.String()
	}
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, cfg, logs, io.Discard) }()

	status := func(url string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	admin, s3 := "http://"+cfg.AdminAddr, "http://"+cfg.S3Addr
	deadline := time.Now().Add(30 * time.Second)
	for status(admin+"/readyz") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatalf("the node did not become ready; log:\n%s", logText())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Served, and so answered as the node answers somebody it does not
	// know - not refused as a node that is starting refuses everybody.
	for name, url := range map[string]string{"admin API": admin + "/api/v1/buckets", "metrics": admin + "/metrics", "S3": s3 + "/"} {
		if got := status(url); got != http.StatusUnauthorized && got != http.StatusForbidden {
			t.Errorf("%s of a node that has started = %d, want 401 or 403", name, got)
		}
	}
	text := logText()
	open, started, listening := strings.Index(text, "listener open; the node is starting"), strings.Index(text, `"msg":"node started"`), strings.Index(text, `"msg":"listening","listener":"admin"`)
	if open < 0 || started < open || listening < started {
		t.Errorf("the log does not say, in this order, listener open, node started, listening:\n%s", text)
	}

	// Told to stop, it drains: not ready, still alive, for the drain delay.
	cancel()
	drained := false
	for began := time.Now(); time.Since(began) < 1500*time.Millisecond; time.Sleep(20 * time.Millisecond) {
		if status(admin+"/readyz") == http.StatusServiceUnavailable && status(admin+"/healthz") == http.StatusOK {
			drained = true
			break
		}
	}
	if !drained {
		t.Error("a node that had started did not keep serving, not ready, while it drained")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runServer = %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the node did not stop")
	}
}
