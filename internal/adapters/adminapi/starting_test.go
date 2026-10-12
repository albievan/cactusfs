package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/adminapi/api"
	"github.com/albievan/cactusfs/internal/buildinfo"
)

// TestStarting: until the node has started, the management API, its
// documentation and the console are all answered 503 as a problem.
func TestStarting(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct{ method, target string }{
		"a read":            {http.MethodGet, "/api/v1/buckets"},
		"a change":          {http.MethodPost, "/api/v1/users"},
		"a sign-in":         {http.MethodPost, "/api/v1/session"},
		"the contract":      {http.MethodGet, "/api/v1/openapi.json"},
		"the documentation": {http.MethodGet, "/docs"},
		"the console":       {http.MethodGet, "/"},
		"metrics":           {http.MethodGet, "/metrics"},
		"no body wanted":    {http.MethodHead, "/api/v1/buckets"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			Starting().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tt.method, "http://admin.test"+tt.target, nil))
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Content-Type") != "application/problem+json" ||
				rec.Header().Get("Retry-After") == "" || rec.Header().Get("Set-Cookie") != "" {
				t.Errorf("status %d, headers %v", rec.Code, rec.Header())
			}
			if tt.method == http.MethodHead {
				if rec.Body.Len() != 0 {
					t.Errorf("a body in the answer to HEAD: %q", rec.Body.String())
				}
				return
			}
			var problem api.Problem
			if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil || problem.Status != 503 || problem.Code != "ServiceUnavailable" || *problem.Instance != tt.target {
				t.Errorf("problem %q (%v)", rec.Body.String(), err)
			}
		})
	}
}

// TestServerVersion: the build is said as the contract has it, and a commit
// the build does not know is left out, not sent empty.
func TestServerVersion(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		build buildinfo.Info
		want  string
	}{
		"a numbered build":        {buildinfo.Info{Version: "0.7.0", Build: "412", Commit: "3300281abcde"}, `{"build":"412","commit":"3300281abcde","version":"0.7.0"}`},
		"a build of a dirty tree": {buildinfo.Info{Version: "0.7.0", Build: "412", Commit: "3300281abcde-dirty"}, `{"build":"412","commit":"3300281abcde-dirty","version":"0.7.0"}`},
		"a build without commit":  {buildinfo.Info{Version: "0.7.0", Build: "0"}, `{"build":"0","version":"0.7.0"}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := &operations{h: &Handler{cfg: Config{Build: tt.build}}}
			got, err := json.Marshal(o.server())
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("server = %s, want %s", got, tt.want)
			}
		})
	}
}
