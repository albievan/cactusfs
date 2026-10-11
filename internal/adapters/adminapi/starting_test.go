package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/adminapi/api"
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
