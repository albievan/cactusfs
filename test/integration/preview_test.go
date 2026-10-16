//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/albievan/cactusfs/internal/domain/preview/previewtest"
	"github.com/albievan/cactusfs/test/clustertest"
)

// previewProblem is the code of a problem+json answer, or "" when the answer
// is none.
func previewProblem(header http.Header, body []byte) string {
	if !strings.HasPrefix(header.Get("Content-Type"), "application/problem+json") {
		return ""
	}
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &problem); err != nil {
		return ""
	}
	return problem.Code
}

// previewAuditRecord is a record of the audit trail, as GET /audit gives it.
type previewAuditRecord struct {
	Sequence    int64  `json:"sequence"`
	Actor       string `json:"actor"`
	AccessKeyID string `json:"access_key_id"`
	Action      string `json:"action"`
	Resource    string `json:"resource"`
	Outcome     string `json:"outcome"`
}

// previewAudit reads the whole audit trail through a node, a page after
// another.
func previewAudit(ctx context.Context, t *testing.T, c *clustertest.Cluster, id string, root map[string]string) []previewAuditRecord {
	t.Helper()
	var all []previewAuditRecord
	after := int64(0)
	for {
		status, _, body, err := gridCall(ctx, c, id, root, http.MethodGet, "/audit?limit=1000&after="+strconv.FormatInt(after, 10), nil)
		var page struct {
			Records []previewAuditRecord `json:"records"`
		}
		if err != nil || status != http.StatusOK || json.Unmarshal(body, &page) != nil {
			t.Fatalf("GET /audit through %s = %d %s (%v)", id, status, body, err)
		}
		if len(page.Records) == 0 {
			return all
		}
		all = append(all, page.Records...)
		after = page.Records[len(page.Records)-1].Sequence
	}
}

// TestPreviewAcrossTheCluster: who may preview an object, in the product as
// it is deployed (ADR-0028, PLAN O.12). Four nodes; every request is sent
// to a node other than the one before, so that what is decided is decided
// on replicated state.
//
//   - alice, whose role allows s3:PutObject and s3:GetObject and nothing
//     else - no admin:PreviewObject - uploads a PNG through node1 with a key
//     of hers and previews it through node2 with that key and through node3
//     with a console session: she owns the version, and owning it is all
//     that lets her.
//   - bob, with the same download and no upload, is the control: he is
//     refused the preview - 403, and a record in the audit trail - and
//     downloads the object, through the management API and through S3.
//   - carol (built-in read-only) previews alice's object: the role assigns
//     the preview.
//   - a key of alice's whose inline policy allows s3:GetObject alone is
//     refused the preview, and downloads.
//   - the allowed previews, and none of the refused ones, are counted as
//     downloads in the object's access record once the nodes have handed
//     their counts over: a download by a sentinel user on each node that
//     refused a preview, made after the refusal, is waited for, so that a
//     refusal counted by mistake would be in the record by then.
//   - a Deny of admin:PreviewObject given to alice refuses her the preview
//     of her own object, by key and by session, and is audited; she still
//     downloads it. No allowed preview is ever audited.
func TestPreviewAcrossTheCluster(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	// The shortest interval there is: the test waits for it.
	c := clustertest.Start(t, clustertest.Options{Env: map[string]string{"CACTUSFS_ACCESS_FLUSH_INTERVAL": "5s"}})
	root := gridBearer(c.AccessKey, c.SecretKey)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	const (
		bucketName = "previews"
		key        = "photos/cactus.png"
	)
	resource := "arn:aws:s3:::" + bucketName + "/" + key
	previewPath := "/buckets/" + bucketName + "/object/preview?key=" + url.QueryEscape(key)
	downloadPath := "/buckets/" + bucketName + "/object?key=" + url.QueryEscape(key)
	accessPath := "/buckets/" + bucketName + "/object/access?key=" + url.QueryEscape(key)

	if _, err := c.S3("node1").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketName)}); err != nil {
		t.Fatalf("CreateBucket through node1: %v", err)
	}
	// What bob may do: download, and nothing else; what alice may do: that
	// and upload. Neither role names admin:PreviewObject.
	manage := func(path string, body map[string]any) {
		t.Helper()
		status, _, answer, err := gridCall(ctx, c, "node4", root, http.MethodPost, path, body)
		if err != nil || status != http.StatusCreated {
			t.Fatalf("POST %s = %d %s (%v)", path, status, answer, err)
		}
	}
	manage("/policies", map[string]any{"name": "download-previews", "document": json.RawMessage(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::previews/*"}]}`)})
	manage("/roles", map[string]any{"name": "downloader", "policies": []string{"download-previews"}})
	manage("/policies", map[string]any{"name": "upload-previews", "document": json.RawMessage(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:PutObject","s3:GetObject"],"Resource":"arn:aws:s3:::previews/*"}]}`)})
	manage("/roles", map[string]any{"name": "uploader", "policies": []string{"upload-previews"}})
	password := "console-" + c.SecretKey[:24]
	aliceID, aliceSecret, err := gridUser(ctx, c, "node4", root, "alice", map[string]any{"roles": []string{"uploader"}, "password": password})
	must("the user who uploads", err)
	bobID, bobSecret, err := gridUser(ctx, c, "node4", root, "bob", map[string]any{"roles": []string{"downloader"}})
	must("the user who may only download", err)
	carolID, carolSecret, err := gridUser(ctx, c, "node4", root, "carol", map[string]any{"roles": []string{"read-only"}})
	must("the user who reads", err)
	sentinelID, sentinelSecret, err := gridUser(ctx, c, "node4", root, "sentinel", map[string]any{"roles": []string{"downloader"}})
	must("the user whose downloads mark the counts as handed over", err)
	sentinel := gridBearer(sentinelID, sentinelSecret)
	alice, bob, carol := gridBearer(aliceID, aliceSecret), gridBearer(bobID, bobSecret), gridBearer(carolID, carolSecret)

	// alice uploads, through node1, with her key: the version is hers.
	content := previewtest.PNGOf(640, 480, 200_000)
	if _, err := gridS3(c, "node1", aliceID, aliceSecret).PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucketName), Key: aws.String(key), Body: bytes.NewReader(content), ContentType: aws.String("image/png"),
	}); err != nil {
		t.Fatalf("PutObject through node1 with alice's key: %v", err)
	}

	// previewed asks for the preview and says what the answer is not that it
	// should be: "" when it is the PNG, as a preview is sent.
	previewed := func(id string, who map[string]string) string {
		t.Helper()
		status, header, body, err := gridCall(ctx, c, id, who, http.MethodGet, previewPath, nil)
		must("preview through "+id, err)
		switch {
		case status != http.StatusOK:
			return fmt.Sprintf("%d %s %s", status, previewProblem(header, body), body)
		case !bytes.Equal(body, content):
			return fmt.Sprintf("200 with %d bytes, not the object's %d", len(body), len(content))
		}
		for name, want := range map[string]string{
			"Content-Type": "application/octet-stream", "X-Cactusfs-Preview-Kind": "png", "X-Cactusfs-Preview-Truncated": "false",
			"Content-Length": strconv.Itoa(len(content)), "X-Content-Type-Options": "nosniff", "Cache-Control": "no-store",
			"Content-Security-Policy": "sandbox; default-src 'none'", "Cross-Origin-Resource-Policy": "same-origin",
		} {
			if got := header.Get(name); got != want {
				return fmt.Sprintf("200 with %s %q, want %q", name, got, want)
			}
		}
		if !strings.HasPrefix(header.Get("Content-Disposition"), "attachment") || header.Get("ETag") == "" {
			return fmt.Sprintf("200 with Content-Disposition %q and ETag %q", header.Get("Content-Disposition"), header.Get("ETag"))
		}
		return ""
	}
	// refused says what the answer is when it is not a refusal of the
	// preview: 403, AccessDenied.
	refused := func(id string, who map[string]string) string {
		t.Helper()
		status, header, body, err := gridCall(ctx, c, id, who, http.MethodGet, previewPath, nil)
		must("preview through "+id, err)
		if status != http.StatusForbidden || previewProblem(header, body) != "AccessDenied" || bytes.Contains(body, content[:64]) {
			return fmt.Sprintf("%d %s, %d bytes", status, previewProblem(header, body), len(body))
		}
		return ""
	}
	downloaded := func(id string, who map[string]string) {
		t.Helper()
		status, _, body, err := gridCall(ctx, c, id, who, http.MethodGet, downloadPath, nil)
		must("download through "+id, err)
		if status != http.StatusOK || !bytes.Equal(body, content) {
			t.Fatalf("download through the management API of %s = %d, %d bytes", id, status, len(body))
		}
	}

	// The owner, through two nodes that did not take the upload: with her
	// key, and signed in to the console.
	if got := previewed("node2", alice); got != "" {
		t.Fatalf("alice's preview of her own object through node2 = %s", got)
	}
	status, header, body, err := gridCall(ctx, c, "node3", nil, http.MethodPost, "/session", map[string]any{"username": "alice", "password": password})
	must("signing in on node3", err)
	cookie := strings.SplitN(header.Get("Set-Cookie"), ";", 2)[0]
	if status != http.StatusOK || !strings.HasPrefix(cookie, "__Host-cactusfs-session=") {
		t.Fatalf("signing in on node3 = %d %s, a cookie of %d bytes", status, previewProblem(header, body), len(cookie))
	}
	session := map[string]string{"Cookie": cookie}
	if got := previewed("node3", session); got != "" {
		t.Fatalf("alice's preview through her session on node3 = %s", got)
	}

	// Download alone is not preview: bob is refused, and the refusal is in
	// the trail. He downloads.
	if got := refused("node3", bob); got != "" {
		t.Errorf("bob's preview = %s, want 403 AccessDenied", got)
	}
	downloaded("node3", bob)
	out, err := gridS3(c, "node1", bobID, bobSecret).GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucketName), Key: aws.String(key)})
	must("bob's GetObject through node1", err)
	got := new(bytes.Buffer)
	_, err = got.ReadFrom(out.Body)
	_ = out.Body.Close()
	if err != nil || !bytes.Equal(got.Bytes(), content) {
		t.Fatalf("bob's GetObject through node1: %d bytes, %v", got.Len(), err)
	}

	// Preview assigned by the built-in read-only role.
	if got := previewed("node4", carol); got != "" {
		t.Errorf("carol's preview of alice's object through node4 = %s", got)
	}

	// A key of alice's that is narrowed to downloads: refused the preview of
	// her own object, and downloads it.
	status, _, body, err = gridCall(ctx, c, "node4", root, http.MethodPost, "/users/alice/keys", map[string]any{"inline_policy": json.RawMessage(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::previews/*"}]}`)})
	var narrowKey struct {
		Key struct {
			AccessKeyID string `json:"access_key_id"`
		} `json:"key"`
		SecretKey string `json:"secret_key"`
	}
	if err != nil || status != http.StatusCreated || json.Unmarshal(body, &narrowKey) != nil || narrowKey.SecretKey == "" {
		t.Fatalf("a key of alice's with an inline policy = %d (%v)", status, err)
	}
	narrow := gridBearer(narrowKey.Key.AccessKeyID, narrowKey.SecretKey)
	if got := refused("node1", narrow); got != "" {
		t.Errorf("the preview with alice's key without the action = %s, want 403 AccessDenied", got)
	}
	downloaded("node2", narrow)

	// Both refusals are in the trail - written in the background, and read
	// through a node that served neither.
	c.Eventually("the refusals of the preview to be audited", time.Minute, func() bool {
		var bobs, narrowed bool
		for _, r := range previewAudit(ctx, t, c, "node2", root) {
			if r.Action != "admin:PreviewObject" || r.Outcome != "denied" || r.Resource != resource {
				continue
			}
			bobs = bobs || r.Actor == "user/bob" && r.AccessKeyID == bobID
			narrowed = narrowed || r.Actor == "user/alice" && r.AccessKeyID == narrowKey.Key.AccessKeyID
		}
		return bobs && narrowed
	})
	// An allowed preview is not audited, as a download is not.
	noAllowedPreviewAudited := func() {
		t.Helper()
		for _, r := range previewAudit(ctx, t, c, "node2", root) {
			if r.Action == "admin:PreviewObject" && r.Outcome != "denied" {
				t.Errorf("an allowed preview is in the audit trail: %+v", r)
			}
		}
	}
	noAllowedPreviewAudited()

	// Counted: alice's two previews and her narrowed key's download,
	// carol's preview, bob's two downloads - and no refused preview. The
	// refusals were served by node3 (bob) and node1 (the narrowed key): a
	// download there by the sentinel, after them, is handed over with or
	// after anything those nodes counted before it.
	downloaded("node1", sentinel)
	downloaded("node3", sentinel)
	want := map[string]int64{"user/alice": 3, "user/bob": 2, "user/carol": 1, "user/sentinel": 2}
	var access objectAccess
	readers := func(a objectAccess) map[string]int64 {
		counted := map[string]int64{}
		for _, r := range a.Readers {
			if r.Presigned || r.Copies != 0 {
				t.Errorf("reader %+v", r)
			}
			counted[r.Who] += r.Downloads
		}
		return counted
	}
	c.Eventually("the sentinel's downloads on node1 and node3 to be counted", time.Minute, func() bool {
		status, _, body, err := gridCall(ctx, c, "node2", root, http.MethodGet, accessPath, nil)
		access = objectAccess{}
		if err != nil || status != http.StatusOK || json.Unmarshal(body, &access) != nil {
			t.Fatalf("access = %d %s (%v)", status, body, err)
		}
		return readers(access)["user/sentinel"] >= 2
	})
	// The other nodes hand theirs over in the same interval; what any node
	// still holds is waited for once more.
	c.Eventually("every download to be counted", time.Minute, func() bool {
		status, _, body, err := gridCall(ctx, c, "node2", root, http.MethodGet, accessPath, nil)
		access = objectAccess{}
		if err != nil || status != http.StatusOK || json.Unmarshal(body, &access) != nil {
			t.Fatalf("access = %d %s (%v)", status, body, err)
		}
		return access.DownloadCount >= 8
	})
	if counted := readers(access); access.UploadedBy != "user/alice" || access.DownloadCount != 8 || access.OtherReaders != 0 || fmt.Sprint(counted) != fmt.Sprint(want) {
		t.Errorf("access = %+v; downloads by reader %v, want %v", access, counted, want)
	}

	// A Deny of the preview refuses the owner: by key, and by the session
	// she signed in with before it was given (a user changed without a new
	// password keeps her sessions). She still downloads.
	manage("/policies", map[string]any{"name": "no-previews", "document": json.RawMessage(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":"admin:PreviewObject","Resource":"arn:aws:s3:::previews/*"}]}`)})
	manage("/roles", map[string]any{"name": "preview-denied", "policies": []string{"no-previews"}})
	status, _, body, err = gridCall(ctx, c, "node4", root, http.MethodPut, "/users/alice",
		map[string]any{"groups": []string{}, "roles": []string{"uploader", "preview-denied"}, "disabled": false})
	if err != nil || status != http.StatusOK {
		t.Fatalf("give alice the Deny = %d %s (%v)", status, body, err)
	}
	c.Eventually("the Deny to refuse alice's key through node2", time.Minute, func() bool {
		return refused("node2", alice) == ""
	})
	c.Eventually("the Deny to refuse alice's session through node3", time.Minute, func() bool {
		return refused("node3", session) == ""
	})
	// Both refusals are audited: each node has its own record of refusals,
	// so neither is folded into the narrowed key's refusal on node1.
	c.Eventually("the refusals by the Deny to be audited", time.Minute, func() bool {
		var byKey, bySession bool
		for _, r := range previewAudit(ctx, t, c, "node4", root) {
			if r.Action != "admin:PreviewObject" || r.Outcome != "denied" || r.Resource != resource || r.Actor != "user/alice" {
				continue
			}
			byKey = byKey || r.AccessKeyID == aliceID
			bySession = bySession || r.AccessKeyID == ""
		}
		return byKey && bySession
	})
	downloaded("node1", alice)
	// Nobody else is touched by it.
	if got := previewed("node1", carol); got != "" {
		t.Errorf("carol's preview after alice's Deny = %s", got)
	}
	noAllowedPreviewAudited()
}

// TestPreviewOfAVersionWithoutAMaker: a version written before versions
// recorded who made them (entry schema 2, before ADR-0016) has no owner, and
// is previewed only by whoever is assigned the preview (ADR-0028).
//
// The schema-2 fixture is the one deployment there is with such versions:
// the fixtures of schema 3 and later record a maker, root, for every
// version. Its objects were stored without a type a preview has, so an
// assigned preview gets past both questions of who may - and is then 415
// NotPreviewable; what is not assigned is refused with 403 before anything
// about the object is said. That the assigned preview of such a version
// gives its bytes when it is of a kind a preview has is shown at the level
// of the rule (internal/domain/iam, "assigned, creator unknown") and of the
// use case (internal/app).
func TestPreviewOfAVersionWithoutAMaker(t *testing.T) {
	// Not beside the others, as the other tests of the fixtures are not: one
	// node as a process, done in seconds when it has the machine.
	bin, dir := buildBinary(t), t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	copyTree(t, filepath.Join("testdata", "schema-2"), dir)
	srv := startServerWith(ctx, t, bin, []string{
		"server",
		"--data", filepath.Join(dir, "drive1"), "--data", filepath.Join(dir, "drive2"),
		"--data", filepath.Join(dir, "drive3"), "--data", filepath.Join(dir, "drive4"),
		"--meta-dir", filepath.Join(dir, "meta"), "--kms", "local", "--kms-key-file", filepath.Join(dir, "kms.json"),
		"--admin-addr", "127.0.0.1:0", "--s3-addr", "127.0.0.1:0",
	}, "CACTUSFS_DEV_PLAINTEXT=true", "CACTUSFS_KMS_PASSPHRASE="+fixturePassphrase,
		"CACTUSFS_ROOT_ACCESS_KEY="+fixtureAccessKey, "CACTUSFS_ROOT_SECRET_KEY="+fixtureSecretKey)
	api := "http://" + waitForListening(t, srv.logs)["admin"] + "/api/v1"
	root := schema3RootBearer()
	const key = "reports/summary.txt"
	previewURL := api + "/buckets/" + schema2Bucket + "/object/preview?key=" + url.QueryEscape(key)

	// The version records nobody.
	status, _, body := adminCall(ctx, t, http.MethodGet, api+"/buckets/"+schema2Bucket+"/object/access?key="+url.QueryEscape(key), root, nil)
	var access objectAccess
	if err := json.Unmarshal(body, &access); err != nil || status != http.StatusOK || access.UploadedBy != "" {
		t.Fatalf("access of %s = %d %s (%v); want no uploader", key, status, body, err)
	}

	// A user who may download it and is assigned nothing, and one whose
	// built-in role assigns the preview.
	userWithKey := func(name string, roles ...string) map[string]string {
		t.Helper()
		if status, _, body := adminCall(ctx, t, http.MethodPost, api+"/users", root, map[string]any{"name": name, "roles": roles}); status != http.StatusCreated {
			t.Fatalf("create user %s = %d %s", name, status, body)
		}
		status, _, body := adminCall(ctx, t, http.MethodPost, api+"/users/"+name+"/keys", root, map[string]any{})
		var created struct {
			Key struct {
				AccessKeyID string `json:"access_key_id"`
			} `json:"key"`
			SecretKey string `json:"secret_key"`
		}
		if err := json.Unmarshal(body, &created); err != nil || status != http.StatusCreated || created.SecretKey == "" {
			t.Fatalf("create a key of %s = %d (%v)", name, status, err)
		}
		return gridBearer(created.Key.AccessKeyID, created.SecretKey)
	}
	if status, _, body := adminCall(ctx, t, http.MethodPost, api+"/policies", root, map[string]any{"name": "download-schema-two", "document": json.RawMessage(
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::schema-two/*"}]}`)}); status != http.StatusCreated {
		t.Fatalf("create the policy = %d %s", status, body)
	}
	if status, _, body := adminCall(ctx, t, http.MethodPost, api+"/roles", root, map[string]any{"name": "downloader", "policies": []string{"download-schema-two"}}); status != http.StatusCreated {
		t.Fatalf("create the role = %d %s", status, body)
	}
	keeper := userWithKey("keeper", "downloader")
	viewer := userWithKey("viewer", "read-only")

	status, header, body := adminCall(ctx, t, http.MethodGet, previewURL, keeper, nil)
	if status != http.StatusForbidden || previewProblem(header, body) != "AccessDenied" {
		t.Errorf("the preview of a version without a maker, not assigned = %d %s, want 403 AccessDenied", status, body)
	}
	status, _, body = adminCall(ctx, t, http.MethodGet, api+"/buckets/"+schema2Bucket+"/object?key="+url.QueryEscape(key), keeper, nil)
	if status != http.StatusOK || string(body) != "summary" {
		t.Errorf("the download of %s = %d, %d bytes", key, status, len(body))
	}
	status, header, body = adminCall(ctx, t, http.MethodGet, previewURL, viewer, nil)
	if status != http.StatusUnsupportedMediaType || previewProblem(header, body) != "NotPreviewable" {
		t.Errorf("the preview of a version without a maker, assigned = %d %s, want 415 NotPreviewable", status, body)
	}
	srv.stop(t)
}
