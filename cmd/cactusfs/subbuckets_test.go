package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

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
