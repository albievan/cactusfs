package adminapi_test

import (
	"net/http"
	"testing"

	"github.com/albievan/cactusfs/internal/domain/audit"
)

// TestQuotaExceededIsAProblem: an upload through the management API that
// would take a bucket past its quota is answered 403 problem+json with the
// code QuotaExceeded (ADR-0024), and stores nothing.
func TestQuotaExceededIsAProblem(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.must(http.StatusCreated, call{method: "POST", path: "/buckets", body: map[string]any{"name": "docs"}})
	if err := f.meta.SetBucketQuota(t.Context(), "docs", 5, audit.Record{}); err != nil {
		t.Fatalf("SetBucketQuota: %v", err)
	}
	p := f.must(http.StatusForbidden, call{method: "PUT", path: "/buckets/docs/object?key=big", raw: []byte("123456")}).problem(t)
	if p["code"] != "QuotaExceeded" || p["title"] != "The bucket quota has been reached" || p["type"] != "about:blank" {
		t.Errorf("the refusal of the quota = %v", p)
	}
	f.must(http.StatusNotFound, call{method: "GET", path: "/buckets/docs/object?key=big"})
	f.must(http.StatusOK, call{method: "PUT", path: "/buckets/docs/object?key=small", raw: []byte("12345")})
}
