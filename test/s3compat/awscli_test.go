//go:build s3compat && s3tools

package s3compat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The AWS CLI, version 2, against the running server: one test for each
// thing an operator does with it. The CLI is in the builder image, pinned by
// the checksum of its installer (Dockerfile); these tests are built only
// there, with the tag s3tools.

// awsCLI runs the AWS CLI as an operator would have it configured: the
// endpoint, the certificate authority and the credentials in the
// environment, path-style addressing in a configuration file, and nothing
// of the developer's own.
type awsCLI struct {
	t    *testing.T
	home string
}

func newAWSCLI(t *testing.T) *awsCLI {
	t.Helper()
	home := t.TempDir()
	config := "[default]\ns3 =\n    addressing_style = path\n    multipart_threshold = 8MB\n    multipart_chunksize = 5MB\n"
	if err := os.WriteFile(filepath.Join(home, "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return &awsCLI{t: t, home: home}
}

func (a *awsCLI) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "aws", args...)
	cmd.Dir = a.home
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + a.home,
		"AWS_CONFIG_FILE=" + filepath.Join(a.home, "config"),
		"AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(a.home, "no-credentials-file"),
		"AWS_ACCESS_KEY_ID=" + target.accessKey, "AWS_SECRET_ACCESS_KEY=" + target.secretKey,
		"AWS_DEFAULT_REGION=" + region, "AWS_ENDPOINT_URL=" + target.endpoint, "AWS_CA_BUNDLE=" + target.certFile,
		"AWS_EC2_METADATA_DISABLED=true", "AWS_PAGER=", "AWS_MAX_ATTEMPTS=1",
	}
	return cmd
}

// run runs the CLI and returns what it printed; the test fails if the CLI
// does.
func (a *awsCLI) run(args ...string) string {
	a.t.Helper()
	out, err := a.try(args...)
	if err != nil {
		a.t.Fatalf("aws %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (a *awsCLI) try(args ...string) (string, error) {
	out, err := a.command(a.t.Context(), args...).CombinedOutput()
	return string(out), err
}

// file writes a file of the given content under the CLI's directory.
func (a *awsCLI) file(name string, content []byte) string {
	a.t.Helper()
	path := filepath.Join(a.home, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		a.t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		a.t.Fatal(err)
	}
	return path
}

func (a *awsCLI) read(path string) []byte {
	a.t.Helper()
	root, err := os.OpenRoot(a.home)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	name, err := filepath.Rel(a.home, path)
	if err != nil {
		a.t.Fatal(err)
	}
	content, err := root.ReadFile(name)
	if err != nil {
		a.t.Fatal(err)
	}
	return content
}

func TestAWSCLIBuckets(t *testing.T) {
	cli := newAWSCLI(t)
	name := fmt.Sprintf("cli-bucket-%x", randomBytes(t, 4))
	cli.run("s3", "mb", "s3://"+name)
	if out := cli.run("s3", "ls"); !strings.Contains(out, name) {
		t.Errorf("aws s3 ls does not list %s:\n%s", name, out)
	}
	if out, err := cli.try("s3", "mb", "s3://"+name); err == nil {
		t.Logf("making the bucket again: %s", out) // S3 answers 200 in us-east-1; either is in order
	}
	cli.run("s3", "rb", "s3://"+name)
	if out := cli.run("s3", "ls"); strings.Contains(out, name) {
		t.Errorf("aws s3 ls still lists %s after rb", name)
	}
	if out, err := cli.try("s3", "ls", "s3://"+name); err == nil || !strings.Contains(out, "NoSuchBucket") {
		t.Errorf("listing a removed bucket: %v\n%s", err, out)
	}
}

func TestAWSCLICopyUpAndDown(t *testing.T) {
	cli := newAWSCLI(t)
	name := newBucket(t)
	data := randomBytes(t, 300_000)
	local := cli.file("report.bin", data)
	cli.run("s3", "cp", local, "s3://"+name+"/dir/report.bin", "--content-type", "application/x-report", "--metadata", "owner=albie")
	if got, head := get(t, name, "dir/report.bin"); !bytes.Equal(got, data) || head.Metadata["owner"] != "albie" {
		t.Errorf("what the CLI uploaded reads as %d bytes, metadata %v", len(got), head.Metadata)
	}
	if out := cli.run("s3", "ls", "s3://"+name+"/dir/"); !strings.Contains(out, "300000 report.bin") {
		t.Errorf("aws s3 ls of the prefix:\n%s", out)
	}
	back := filepath.Join(cli.home, "back.bin")
	cli.run("s3", "cp", "s3://"+name+"/dir/report.bin", back)
	if !bytes.Equal(cli.read(back), data) {
		t.Error("what the CLI downloaded is not what it uploaded")
	}
	// From and to standard streams.
	up := cli.command(t.Context(), "s3", "cp", "-", "s3://"+name+"/stream")
	up.Stdin = strings.NewReader("streamed in")
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("aws s3 cp - : %v\n%s", err, out)
	}
	if out := cli.run("s3", "cp", "s3://"+name+"/stream", "-"); out != "streamed in" {
		t.Errorf("aws s3 cp to standard output = %q", out)
	}
}

func TestAWSCLIMultipart(t *testing.T) {
	cli := newAWSCLI(t)
	name := newBucket(t)
	// Above the threshold of the configuration: the CLI sends parts.
	data := randomBytes(t, 21<<20)
	local := cli.file("large.bin", data)
	cli.run("s3", "cp", local, "s3://"+name+"/large.bin")
	_, head := get(t, name, "large.bin")
	if etag := strings.Trim(*head.ETag, `"`); !strings.Contains(etag, "-") {
		t.Errorf("a %d-byte upload has the entity tag %s: it was not sent in parts", len(data), etag)
	}
	back := filepath.Join(cli.home, "large.back")
	cli.run("s3", "cp", "s3://"+name+"/large.bin", back)
	if !bytes.Equal(cli.read(back), data) {
		t.Error("a multipart object downloaded by the CLI is not what was uploaded")
	}
	// From bucket to bucket, above the threshold: parts copied on the
	// server.
	cli.run("s3", "cp", "s3://"+name+"/large.bin", "s3://"+name+"/copy/large.bin")
	got, copied := get(t, name, "copy/large.bin")
	if !bytes.Equal(got, data) {
		t.Errorf("a server-side multipart copy by the CLI is %d bytes", len(got))
	}
	if etag := strings.Trim(*copied.ETag, `"`); !strings.Contains(etag, "-") {
		t.Errorf("the copy has the entity tag %s: it was not copied in parts", etag)
	}
	// And a small one, in one request.
	put(t, name, "small", []byte("small"))
	cli.run("s3", "cp", "s3://"+name+"/small", "s3://"+name+"/copy/small")
	if got, _ := get(t, name, "copy/small"); string(got) != "small" {
		t.Errorf("a server-side copy by the CLI = %q", got)
	}
}

func TestAWSCLISync(t *testing.T) {
	cli := newAWSCLI(t)
	name := newBucket(t)
	files := map[string]string{"a.txt": "alpha", "dir/b.txt": "beta", "dir/deep/c.txt": "gamma"}
	for path, content := range files {
		cli.file(filepath.Join("tree", path), []byte(content))
	}
	tree := filepath.Join(cli.home, "tree")
	cli.run("s3", "sync", tree, "s3://"+name+"/site")
	listed := cli.run("s3", "ls", "s3://"+name+"/site/", "--recursive")
	for path := range files {
		if !strings.Contains(listed, "site/"+path) {
			t.Errorf("after sync, %s is not listed:\n%s", path, listed)
		}
	}
	// Nothing changed: nothing is sent.
	if out := cli.run("s3", "sync", tree, "s3://"+name+"/site"); strings.Contains(out, "upload:") {
		t.Errorf("a second sync uploaded again:\n%s", out)
	}
	// One file changed, one removed.
	cli.file("tree/a.txt", []byte("alpha, revised"))
	if err := os.Remove(filepath.Join(tree, "dir", "b.txt")); err != nil {
		t.Fatal(err)
	}
	out := cli.run("s3", "sync", tree, "s3://"+name+"/site", "--delete")
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "delete:") || strings.Contains(out, "c.txt") {
		t.Errorf("sync after one change and one removal:\n%s", out)
	}
	if got, _ := get(t, name, "site/a.txt"); string(got) != "alpha, revised" {
		t.Errorf("the changed file = %q", got)
	}
	down := filepath.Join(cli.home, "down")
	cli.run("s3", "sync", "s3://"+name+"/site", down)
	if string(cli.read(filepath.Join(down, "dir", "deep", "c.txt"))) != "gamma" {
		t.Error("sync down did not bring the tree")
	}
	if _, err := os.Stat(filepath.Join(down, "dir", "b.txt")); err == nil {
		t.Error("sync down brought a file that was deleted")
	}
}

func TestAWSCLIMoveAndRemove(t *testing.T) {
	cli := newAWSCLI(t)
	name := newBucket(t)
	for _, key := range []string{"in/a", "in/b", "keep"} {
		put(t, name, key, []byte(key))
	}
	cli.run("s3", "mv", "s3://"+name+"/in/a", "s3://"+name+"/out/a")
	if got, _ := get(t, name, "out/a"); string(got) != "in/a" {
		t.Errorf("the moved object = %q", got)
	}
	cli.run("s3", "rm", "s3://"+name+"/in/", "--recursive")
	cli.run("s3", "rm", "s3://"+name+"/out/a")
	listed := cli.run("s3", "ls", "s3://"+name+"/", "--recursive")
	if strings.Contains(listed, "in/") || strings.Contains(listed, "out/") || !strings.Contains(listed, "keep") {
		t.Errorf("after mv and rm:\n%s", listed)
	}
	// A bucket with objects is removed with --force.
	cli.run("s3", "rb", "s3://"+name, "--force")
	if out, err := cli.try("s3api", "head-bucket", "--bucket", name); err == nil {
		t.Errorf("the bucket is still there after rb --force:\n%s", out)
	}
}

func TestAWSCLIPresign(t *testing.T) {
	cli := newAWSCLI(t)
	name := newBucket(t)
	put(t, name, "shared.txt", []byte("for whoever has the link"))
	link := strings.TrimSpace(cli.run("s3", "presign", "s3://"+name+"/shared.txt", "--expires-in", "300"))
	if !strings.HasPrefix(link, target.endpoint+"/"+name+"/shared.txt?") || !strings.Contains(link, "X-Amz-Signature=") {
		t.Fatalf("aws s3 presign = %s", link)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, link, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "for whoever has the link" {
		t.Errorf("the presigned link answers %d %q", resp.StatusCode, body)
	}
}

func TestAWSCLIS3API(t *testing.T) {
	cli := newAWSCLI(t)
	name := newBucket(t)
	decode := func(out string, into any) {
		t.Helper()
		if err := json.Unmarshal([]byte(out), into); err != nil {
			t.Fatalf("%v in:\n%s", err, out)
		}
	}
	cli.run("s3api", "put-bucket-versioning", "--bucket", name, "--versioning-configuration", "Status=Enabled")
	var versioning struct{ Status string }
	decode(cli.run("s3api", "get-bucket-versioning", "--bucket", name), &versioning)
	if versioning.Status != "Enabled" {
		t.Errorf("get-bucket-versioning = %+v", versioning)
	}
	var first, second struct {
		VersionID string `json:"VersionId"`
	}
	decode(cli.run("s3api", "put-object", "--bucket", name, "--key", "doc", "--body", cli.file("one", []byte("one")), "--tagging", "stage=draft"), &first)
	decode(cli.run("s3api", "put-object", "--bucket", name, "--key", "doc", "--body", cli.file("two", []byte("two"))), &second)
	if first.VersionID == "" || first.VersionID == second.VersionID {
		t.Fatalf("put-object versions %q and %q", first.VersionID, second.VersionID)
	}
	var versions struct {
		Versions []struct {
			Key       string
			VersionID string `json:"VersionId"`
			IsLatest  bool
		}
	}
	decode(cli.run("s3api", "list-object-versions", "--bucket", name), &versions)
	if len(versions.Versions) != 2 || !versions.Versions[0].IsLatest || versions.Versions[0].VersionID != second.VersionID {
		t.Errorf("list-object-versions = %+v", versions)
	}
	var tags struct{ TagSet []struct{ Key, Value string } }
	decode(cli.run("s3api", "get-object-tagging", "--bucket", name, "--key", "doc", "--version-id", first.VersionID), &tags)
	if len(tags.TagSet) != 1 || tags.TagSet[0].Value != "draft" {
		t.Errorf("get-object-tagging of the first version = %+v", tags)
	}
	cli.run("s3api", "put-object-tagging", "--bucket", name, "--key", "doc", "--tagging", "TagSet=[{Key=stage,Value=final}]")
	decode(cli.run("s3api", "get-object-tagging", "--bucket", name, "--key", "doc"), &tags)
	if len(tags.TagSet) != 1 || tags.TagSet[0].Value != "final" {
		t.Errorf("get-object-tagging after put-object-tagging = %+v", tags)
	}
	var head struct {
		ContentLength        int
		ServerSideEncryption string
		VersionID            string `json:"VersionId"`
	}
	decode(cli.run("s3api", "head-object", "--bucket", name, "--key", "doc"), &head)
	if head.ContentLength != 3 || head.ServerSideEncryption != "AES256" || head.VersionID != second.VersionID {
		t.Errorf("head-object = %+v", head)
	}
	out := filepath.Join(cli.home, "old")
	cli.run("s3api", "get-object", "--bucket", name, "--key", "doc", "--version-id", first.VersionID, out)
	if string(cli.read(out)) != "one" {
		t.Errorf("get-object of the first version = %q", cli.read(out))
	}
	// A lifecycle rule and a bucket policy, as JSON on the command line.
	cli.run("s3api", "put-bucket-lifecycle-configuration", "--bucket", name, "--lifecycle-configuration",
		`{"Rules":[{"ID":"old","Status":"Enabled","Filter":{"Prefix":""},"NoncurrentVersionExpiration":{"NoncurrentDays":30}}]}`)
	var rules struct{ Rules []struct{ ID, Status string } }
	decode(cli.run("s3api", "get-bucket-lifecycle-configuration", "--bucket", name), &rules)
	if len(rules.Rules) != 1 || rules.Rules[0].ID != "old" {
		t.Errorf("get-bucket-lifecycle-configuration = %+v", rules)
	}
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::%s/*"}]}`, name)
	cli.run("s3api", "put-bucket-policy", "--bucket", name, "--policy", policy)
	var stored struct{ Policy string }
	decode(cli.run("s3api", "get-bucket-policy", "--bucket", name), &stored)
	if stored.Policy != policy {
		t.Errorf("get-bucket-policy = %s", stored.Policy)
	}
	cli.run("s3api", "delete-bucket-policy", "--bucket", name)
	// Everything the test made, removed as the CLI removes versions.
	var all struct {
		Versions, DeleteMarkers []struct {
			Key       string
			VersionID string `json:"VersionId"`
		}
	}
	decode(cli.run("s3api", "list-object-versions", "--bucket", name), &all)
	for _, v := range slices.Concat(all.Versions, all.DeleteMarkers) {
		cli.run("s3api", "delete-object", "--bucket", name, "--key", v.Key, "--version-id", v.VersionID)
	}
}

func TestAWSCLICustomerKey(t *testing.T) {
	cli := newAWSCLI(t)
	name := newBucket(t)
	key := cli.file("customer.key", randomBytes(t, 32))
	other := cli.file("other.key", randomBytes(t, 32))
	// Small, and above the multipart threshold: the CLI sends the key
	// with every part and with the completion.
	for object, size := range map[string]int{"small": 1000, "large": 17 << 20} {
		data := randomBytes(t, size)
		local := cli.file(object+".bin", data)
		sent := cli.run("s3", "cp", local, "s3://"+name+"/"+object, "--sse-c", "AES256", "--sse-c-key", "fileb://"+key, "--debug")
		// The large one goes up in parts. (Its entity tag cannot say so:
		// under a customer key it is opaque.)
		if inParts := strings.Contains(sent, "OperationModel(name=UploadPart)"); inParts != (object == "large") {
			t.Errorf("%s: sent in parts: %v", object, inParts)
		}
		back := filepath.Join(cli.home, object+".back")
		cli.run("s3", "cp", "s3://"+name+"/"+object, back, "--sse-c", "AES256", "--sse-c-key", "fileb://"+key)
		if !bytes.Equal(cli.read(back), data) {
			t.Errorf("%s: what came back under the customer key is not what went up", object)
		}
		// Refused by the server, for the reason that it is: not a failure
		// of something else.
		if out, err := cli.try("s3", "cp", "s3://"+name+"/"+object, back+".2"); err == nil || !strings.Contains(out, "(400)") {
			t.Errorf("%s: downloading without the key: %v\n%s", object, err, out)
		}
		if out, err := cli.try("s3", "cp", "s3://"+name+"/"+object, back+".3", "--sse-c", "AES256", "--sse-c-key", "fileb://"+other); err == nil || !strings.Contains(out, "(403)") {
			t.Errorf("%s: downloading with another key: %v\n%s", object, err, out)
		}
	}
}
