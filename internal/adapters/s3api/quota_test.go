package s3api

import (
	"net/http"
	"testing"

	"github.com/albievan/cactusfs/internal/domain/audit"
)

// TestQuotaExceededOverHTTP: a PutObject that would take a bucket past its
// quota is answered 403 QuotaExceeded with the message of ADR-0024, and
// stores nothing; one that fits is taken.
func TestQuotaExceededOverHTTP(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, "")
	s.createBucket("bkt")
	if err := s.meta.SetBucketQuota(t.Context(), "bkt", 5, audit.Record{}); err != nil {
		t.Fatalf("SetBucketQuota: %v", err)
	}
	refused := s.do(call{method: http.MethodPut, path: "/bkt/big", body: []byte("123456")})
	if status, code := answer(refused); status != http.StatusForbidden || code != "QuotaExceeded" {
		t.Errorf("PUT past the quota = %d %s, want 403 QuotaExceeded", status, code)
	}
	if status, code := answer(s.do(call{method: http.MethodGet, path: "/bkt/big"})); status != http.StatusNotFound {
		t.Errorf("GET of the refused object = %d %s, want 404", status, code)
	}
	if status, code := answer(s.do(call{method: http.MethodPut, path: "/bkt/small", body: []byte("12345")})); status != http.StatusOK {
		t.Errorf("PUT to the quota = %d %s, want 200", status, code)
	}
}
