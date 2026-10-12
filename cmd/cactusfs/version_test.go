package main

import (
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/buildinfo"
)

func TestVersionString(t *testing.T) {
	t.Parallel()

	platform := runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH
	tests := []struct {
		name  string
		build buildinfo.Info
		want  string
	}{
		{
			name: "a numbered build", build: buildinfo.Info{Version: "0.7.0", Build: "412", Commit: "3300281abcde"},
			want: "cactusfs 0.7.0 build 412 (commit 3300281abcde, " + platform + ")",
		},
		{
			name: "a numbered build of a changed tree", build: buildinfo.Info{Version: "0.7.0", Build: "412", Commit: "3300281abcde-dirty"},
			want: "cactusfs 0.7.0 build 412 (commit 3300281abcde-dirty, " + platform + ")",
		},
		{
			name: "a plain go build in a checkout", build: buildinfo.Info{Version: "0.7.0", Build: "0", Commit: "3300281abcde"},
			want: "cactusfs 0.7.0 local build (commit 3300281abcde, " + platform + ")",
		},
		{
			name: "a plain go build of an export", build: buildinfo.Info{Version: "0.7.0", Build: "0"},
			want: "cactusfs 0.7.0 local build (" + platform + ")",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := versionString(tt.build); got != tt.want {
				t.Errorf("versionString() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestProbesDoNotNameTheBuild: the probes answer anybody, so they say nothing
// of which build answers - not in a body, not in a header.
func TestProbesDoNotNameTheBuild(t *testing.T) {
	t.Parallel()

	build := buildinfo.Info{Version: "97.98.99-fixture", Build: "424242-of-the-fixture", Commit: "c0ffee15c0de"}
	srv := startServer(t, serveOptions{build: build})
	client := &http.Client{Timeout: 5 * time.Second}

	for _, path := range []string{"/healthz", "/readyz"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+srv.addr+path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("GET %s: read body: %v", path, err)
		}
		said := string(body)
		for name, values := range resp.Header {
			said += "\n" + name + ": " + strings.Join(values, ", ")
		}
		for _, part := range []string{build.Version, build.Build, build.Commit, buildinfo.Release()} {
			if strings.Contains(said, part) {
				t.Errorf("GET %s names the build (%s):\n%s", path, part, said)
			}
		}
		for _, name := range []string{"Server", "X-Powered-By"} {
			if resp.Header.Get(name) != "" {
				t.Errorf("GET %s has a %s header: %q", path, name, resp.Header.Get(name))
			}
		}
	}

	srv.cancel()
	if err := srv.wait(t); err != nil {
		t.Fatalf("serve returned %v, want nil after a graceful shutdown", err)
	}
}
