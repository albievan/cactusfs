//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// adminCall sends one request to the admin listener of a running server.
func adminCall(ctx context.Context, t *testing.T, method, url string, headers map[string]string, body any) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	if raw, ok := body.([]byte); ok {
		reader = bytes.NewReader(raw)
	} else if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp.StatusCode, resp.Header, data
}

// TestManagementAPIOfTheBinary: the binary a user builds with Go alone serves
// the management API, its contract and its documentation from what is inside
// it, and says that it has no console.
func TestManagementAPIOfTheBinary(t *testing.T) {
	t.Parallel()
	// Built before the clock of the test starts: on a cold cache the build
	// alone can take longer than the test may.
	bin, dir := buildBinary(t), t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()
	const accessKey, secretKey = "AKIDINTEGRATIONROOT0", "integration-root-secret-key-0123456789ab"
	srv := startServer(ctx, t, bin, dir, "CACTUSFS_ROOT_ACCESS_KEY="+accessKey, "CACTUSFS_ROOT_SECRET_KEY="+secretKey)
	base := "http://" + waitForListening(t, srv.logs)["admin"]
	root := map[string]string{"Authorization": "Bearer " + accessKey + ":" + secretKey}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range root {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	// The documentation and everything it loads come from the binary, and
	// the browser is told to load nothing from anywhere else.
	status, header, page := adminCall(ctx, t, http.MethodGet, base+"/docs", nil, nil)
	if status != http.StatusOK || !strings.Contains(string(page), "/docs/swagger-ui-bundle.js") || strings.Contains(string(page), "//") && strings.Contains(string(page), "http") {
		t.Fatalf("GET /docs = %d: %s", status, page)
	}
	csp := header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") || strings.Contains(csp, "http") {
		t.Errorf("Content-Security-Policy of /docs = %q", csp)
	}
	for path, contentType := range map[string]string{
		"/docs/swagger-ui-bundle.js": "text/javascript", "/docs/swagger-ui.css": "text/css", "/docs/initializer.js": "text/javascript",
		"/api/v1/openapi.json": "application/json", "/api/v1/openapi.yaml": "application/yaml",
	} {
		status, header, body := adminCall(ctx, t, http.MethodGet, base+path, nil, nil)
		if status != http.StatusOK || !strings.HasPrefix(header.Get("Content-Type"), contentType) || len(body) < 200 {
			t.Errorf("GET %s = %d, %q, %d bytes", path, status, header.Get("Content-Type"), len(body))
		}
	}
	status, header, page = adminCall(ctx, t, http.MethodGet, base+"/", nil, nil)
	if status != http.StatusOK || !strings.Contains(string(page), "console was not built") || header.Get("Content-Security-Policy") == "" {
		t.Errorf("GET / of a binary without the console = %d: %s", status, page)
	}

	// The API: nothing without a credential, everything for root.
	if status, _, _ := adminCall(ctx, t, http.MethodGet, base+"/api/v1/buckets", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/buckets without a credential = %d", status)
	}
	if status, _, body := adminCall(ctx, t, http.MethodPost, base+"/api/v1/buckets", with(map[string]string{"Idempotency-Key": "b-1"}), map[string]any{"name": "from-the-api"}); status != http.StatusCreated {
		t.Fatalf("create bucket = %d %s", status, body)
	}
	status, header, _ = adminCall(ctx, t, http.MethodPost, base+"/api/v1/buckets", with(map[string]string{"Idempotency-Key": "b-1"}), map[string]any{"name": "from-the-api"})
	if status != http.StatusCreated || header.Get("Idempotent-Replayed") != "true" {
		t.Errorf("the repeat of a bucket creation = %d, replayed %q", status, header.Get("Idempotent-Replayed"))
	}
	content := bytes.Repeat([]byte("cactus "), 50_000)
	status, _, body := adminCall(ctx, t, http.MethodPut, base+"/api/v1/buckets/from-the-api/object?key=dir/file.bin",
		with(map[string]string{"Content-Type": "application/octet-stream"}), content)
	if status != http.StatusOK {
		t.Fatalf("upload = %d %s", status, body)
	}
	status, header, got := adminCall(ctx, t, http.MethodGet, base+"/api/v1/buckets/from-the-api/object?key=dir/file.bin", root, nil)
	if status != http.StatusOK || !bytes.Equal(got, content) || !strings.HasPrefix(header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download = %d, %d bytes, disposition %q", status, len(got), header.Get("Content-Disposition"))
	}

	// The console user every new deployment begins with (ADR-0014): signs
	// in over HTTP with the password everybody knows, which can then never
	// be chosen again.
	if status, _, body := adminCall(ctx, t, http.MethodPost, base+"/api/v1/session", nil, map[string]any{"username": "admin", "password": "admin"}); status != http.StatusOK {
		t.Errorf("sign-in as the default console user = %d %s", status, body)
	}
	if status, _, body := adminCall(ctx, t, http.MethodPut, base+"/api/v1/users/admin", root, map[string]any{"roles": []string{"cluster-admin"}, "password": "admin"}); status != http.StatusBadRequest {
		t.Errorf("setting the default password again = %d %s, want 400", status, body)
	}
	if status, _, body := adminCall(ctx, t, http.MethodDelete, base+"/api/v1/users/admin", root, nil); status != http.StatusNoContent {
		t.Errorf("deleting the default console user = %d %s", status, body)
	}
	if status, _, _ := adminCall(ctx, t, http.MethodPost, base+"/api/v1/session", nil, map[string]any{"username": "admin", "password": "admin"}); status != http.StatusUnauthorized {
		t.Errorf("sign-in as the deleted default user = %d, want 401", status)
	}

	// A console user: signs in, reads with the cookie, changes with the
	// cookie and the CSRF token.
	const password = "a password for the console"
	if status, _, body := adminCall(ctx, t, http.MethodPost, base+"/api/v1/users", root, map[string]any{
		"name": "operator", "roles": []string{"bucket-admin"}, "password": password,
	}); status != http.StatusCreated {
		t.Fatalf("create user = %d %s", status, body)
	}
	status, header, body = adminCall(ctx, t, http.MethodPost, base+"/api/v1/session", nil, map[string]any{"username": "operator", "password": password})
	if status != http.StatusOK {
		t.Fatalf("sign-in = %d %s", status, body)
	}
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		t.Fatal(err)
	}
	cookie := strings.SplitN(header.Get("Set-Cookie"), ";", 2)[0]
	if !strings.HasPrefix(cookie, "__Host-cactusfs-session=") || !strings.Contains(header.Get("Set-Cookie"), "HttpOnly") {
		t.Fatalf("Set-Cookie = %q", header.Get("Set-Cookie"))
	}
	if status, _, _ := adminCall(ctx, t, http.MethodPost, base+"/api/v1/buckets", map[string]string{"Cookie": cookie}, map[string]any{"name": "no-csrf"}); status != http.StatusUnauthorized {
		t.Errorf("a change with the cookie alone = %d, want 401", status)
	}
	if status, _, body := adminCall(ctx, t, http.MethodPost, base+"/api/v1/buckets",
		map[string]string{"Cookie": cookie, "X-CSRF-Token": session.CSRFToken}, map[string]any{"name": "operators"}); status != http.StatusCreated {
		t.Errorf("a change with cookie and CSRF token = %d %s", status, body)
	}
	if status, _, _ := adminCall(ctx, t, http.MethodGet, base+"/api/v1/users", map[string]string{"Cookie": cookie}, nil); status != http.StatusForbidden {
		t.Errorf("a bucket administrator listing users = %d, want 403", status)
	}

	// The cluster of one, and the trail of all of the above.
	status, _, body = adminCall(ctx, t, http.MethodGet, base+"/api/v1/cluster", root, nil)
	var cluster struct {
		Leader      string `json:"leader"`
		DrivesTotal int    `json:"drives_total"`
		Online      int    `json:"drives_online"`
	}
	if err := json.Unmarshal(body, &cluster); err != nil || status != http.StatusOK || cluster.Leader == "" || cluster.DrivesTotal != 2 || cluster.Online != 2 {
		t.Errorf("cluster status = %d %s (%v)", status, body, err)
	}
	status, _, body = adminCall(ctx, t, http.MethodGet, base+"/api/v1/audit?limit=1000", root, nil)
	trail := string(body)
	for _, want := range []string{"admin:PutUser", "s3:CreateBucket", "auth:SignIn", "system:BootstrapRoot"} {
		if !strings.Contains(trail, want) {
			t.Errorf("the audit trail (status %d) has no %s", status, want)
		}
	}
	if strings.Contains(trail, password) || strings.Contains(trail, secretKey) || strings.Contains(trail, session.CSRFToken) {
		t.Fatal("a secret is in the audit trail")
	}
	// Metrics count the API by route, never by name.
	status, _, metrics := adminCall(ctx, t, http.MethodGet, base+"/metrics", root, nil)
	if status != http.StatusOK || !strings.Contains(string(metrics), `cactusfs_admin_requests_total{code="201",operation="POST /api/v1/buckets"}`) ||
		strings.Contains(string(metrics), "from-the-api") {
		t.Errorf("metrics (status %d) do not count the management API by route", status)
	}
	srv.stop(t)
	if out := srv.stderr.String(); strings.Contains(out, password) || strings.Contains(out, secretKey) {
		t.Fatal("a secret was written to standard error")
	}
}
