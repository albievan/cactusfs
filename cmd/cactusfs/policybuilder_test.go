package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/albievan/cactusfs/internal/adapters/s3api"
)

// TestTheServedAdminAPIHasThePolicyBuilder holds server.go and node.go to
// the wiring of PLAN 9.6: the management API of a node started as the
// binary starts it is given the use cases of the policy builder (ADR-0032)
// - the catalogue, the three validations and the explanation answer, not
// the 501 of a node without them - and the explanation appends its record
// to the node's own trail before it answers.
func TestTheServedAdminAPIHasThePolicyBuilder(t *testing.T) {
	t.Parallel()
	call, root, stop := servedAdmin(t)
	defer stop()

	if status, body := call(http.MethodPost, "/api/v1/users", root, `{"name":"dora","roles":["read-only"]}`); status != http.StatusCreated {
		t.Fatalf("POST /users = %d %s", status, body)
	}
	document := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/*"}]}`
	tests := []struct {
		method, path, body string
		want               string
	}{
		{http.MethodGet, "/api/v1/policy-catalogue", "", `"admin:ExplainAccess"`},
		{http.MethodPost, "/api/v1/policy-validation", `{"document":{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:Get*","Resource":"*"}]}}`, `"valid":true`},
		{http.MethodPost, "/api/v1/buckets/pub/policy/validation", `{"document":` + document + `}`, `"open_to_anyone":true`},
		{http.MethodPost, "/api/v1/buckets/pub/subbuckets/reports/policy/validation", `{"policy":` + document + `}`, `"valid":false`},
		{http.MethodGet, "/api/v1/access/explain?user=dora&action=s3:GetObject&bucket=pub&key=k", "", `"authenticates":true`},
	}
	for _, tt := range tests {
		status, body := call(tt.method, tt.path, root, tt.body)
		if status != http.StatusOK || !strings.Contains(body, tt.want) {
			t.Errorf("%s %s on the served node = %d %s, want 200 with %s", tt.method, tt.path, status, body, tt.want)
		}
	}
	status, body := call(http.MethodGet, "/api/v1/audit", root, "")
	if status != http.StatusOK || !strings.Contains(body, `"action":"admin:ExplainAccess"`) || !strings.Contains(body, `"answer":"decision"`) {
		t.Errorf("the trail of the served node = %d %s, want the record of the explanation", status, body)
	}
}

// TestBucketPolicySameThroughS3AndAdmin (ADR-0032, Decision 2): a bucket's
// policy is one document whichever API sets it - the bytes PUT through S3's
// PutBucketPolicy are what the management API reads, and the bytes put
// through the management API are what S3's GetBucketPolicy reads, keys in
// the order they were given. The management API answers JSON, which
// carries a document without the spaces it was sent with; S3 answers the
// bytes as they were set.
func TestBucketPolicySameThroughS3AndAdmin(t *testing.T) {
	t.Parallel()
	call, s3Addr, root, stop := servedNode(t)
	defer stop()
	keyID, secret, _ := strings.Cut(root, ":")
	s3 := func(method, path string, body []byte) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, "http://"+s3Addr+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		payload := hex.EncodeToString(sum[:])
		req.Header.Set("X-Amz-Content-Sha256", payload)
		credentials := aws.Credentials{AccessKeyID: keyID, SecretAccessKey: secret}
		if err := v4.NewSigner().SignHTTP(t.Context(), credentials, req, payload, "s3", s3api.DefaultRegion, time.Now()); err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		answer, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, answer
	}
	adminPolicy := func() string {
		t.Helper()
		status, body := call(http.MethodGet, "/api/v1/buckets/shared/policy", root, "")
		if status != http.StatusOK {
			t.Fatalf("GET the policy through the management API = %d %s", status, body)
		}
		var got struct {
			Document json.RawMessage `json:"document"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		return string(got.Document)
	}
	if status, body := call(http.MethodPost, "/api/v1/buckets", root, `{"name":"shared"}`); status != http.StatusCreated {
		t.Fatalf("POST /buckets = %d %s", status, body)
	}

	// Through S3, read through the management API: the same bytes.
	throughS3 := `{"Statement":[{"Resource":"arn:aws:s3:::shared/*","Sid":"s3","Principal":{"AWS":"arn:aws:iam:::user/dora"},` +
		`"Action":["s3:GetObject"],"Effect":"Allow"}],"Version":"2012-10-17"}`
	if status, body := s3(http.MethodPut, "/shared?policy", []byte(throughS3)); status != http.StatusNoContent {
		t.Fatalf("PutBucketPolicy through S3 = %d %s", status, body)
	}
	if got := adminPolicy(); got != throughS3 {
		t.Errorf("the management API reads %s, S3 set %s", got, throughS3)
	}

	// Through the management API, read through S3: the same bytes, the
	// spaces they were sent with included.
	for _, throughAdmin := range []string{
		`{"Version":"2012-10-17","Statement":[{"Sid":"admin","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::shared/*"}]}`,
		"{\n  \"Version\": \"2012-10-17\",\n  \"Statement\": [ { \"Effect\": \"Deny\", \"Principal\": \"*\", \"Action\": \"s3:GetObject\", \"Resource\": \"arn:aws:s3:::shared/x\" } ]\n}",
	} {
		if status, body := call(http.MethodPut, "/api/v1/buckets/shared/policy", root, `{"document":`+throughAdmin+`}`); status != http.StatusOK {
			t.Fatalf("PUT the policy through the management API = %d %s", status, body)
		}
		status, got := s3(http.MethodGet, "/shared?policy", nil)
		if status != http.StatusOK || string(got) != throughAdmin {
			t.Errorf("GetBucketPolicy through S3 = %d %q, the management API set %q", status, got, throughAdmin)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(throughAdmin)); err != nil {
			t.Fatal(err)
		}
		if got := adminPolicy(); got != compact.String() {
			t.Errorf("the management API reads %s, it set %s", got, throughAdmin)
		}
	}
}
