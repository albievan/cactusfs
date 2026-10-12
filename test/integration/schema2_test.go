//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The fixture in testdata/schema-2 is the data of a standalone deployment -
// four drives, the metadata directory and the sealed key file - as the
// binary of commit 098749f wrote it: entry schema 2, state schema 2, the last
// before object access records, sub-buckets and tag rules (ADR-0016). It was
// written by TestWriteSchema2Fixture (schema2_fixture_test.go) and is never
// written again: a later binary cannot write what an earlier one would have.
//
// It holds, in the bucket "schema-two":
//
//	a bucket policy (anonymous read under public/), CORS rules, a lifecycle
//	rule, and bucket tags of mixed case (Team=Storage, env=Prod)
//	reports/2026/q1.txt, reports/2026/q2.txt, reports/summary.txt
//	public/readme.txt
//	Tagged.txt        with object tags of mixed case (Project=Cactus, Stage=Draft)
//	an upload in progress of "film" with one part
//
// and in the bucket "schema-two-versions", which keeps versions:
//
//	doc               two versions, "first" and "second"
//	gone              one version, under a delete marker
//
// Its passphrase and root credentials are the ones of the other fixture:
// test data, used for nothing else.
const (
	schema2Bucket   = "schema-two"
	schema2Versions = "schema-two-versions"
	schema2Policy   = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::schema-two/public/*"}]}`
)

// schema2Server starts this tree's binary on a copy of a deployment.
func schema2Server(ctx context.Context, t *testing.T, bin, dir string) (*server, *s3.Client) {
	t.Helper()
	srv := startServerWith(ctx, t, bin, []string{
		"server",
		"--data", filepath.Join(dir, "drive1"), "--data", filepath.Join(dir, "drive2"),
		"--data", filepath.Join(dir, "drive3"), "--data", filepath.Join(dir, "drive4"),
		"--meta-dir", filepath.Join(dir, "meta"), "--kms", "local", "--kms-key-file", filepath.Join(dir, "kms.json"),
		"--admin-addr", "127.0.0.1:0", "--s3-addr", "127.0.0.1:0",
	}, "CACTUSFS_DEV_PLAINTEXT=true", "CACTUSFS_KMS_PASSPHRASE="+fixturePassphrase,
		"CACTUSFS_ROOT_ACCESS_KEY="+fixtureAccessKey, "CACTUSFS_ROOT_SECRET_KEY="+fixtureSecretKey)
	addrs := waitForListening(t, srv.logs)
	client := s3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(fixtureAccessKey, fixtureSecretKey, ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("http://" + addrs["s3"])
		o.UsePathStyle = true
	})
	return srv, client
}

func tagsOf(set []types.Tag) map[string]string {
	out := map[string]string{}
	for _, tag := range set {
		out[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}
	return out
}

// TestUpgradeFromSchema2: a deployment written with entry schema 2 is opened
// by this binary. Nothing is rewritten and nothing is lost: objects, versions,
// markers, the bucket's policy, CORS, lifecycle and tags - in the case they
// were written in - and an upload that was in progress, which can be
// finished.
func TestUpgradeFromSchema2(t *testing.T) {
	// Not beside the others: this test starts one node as a process and is
	// done in seconds when it has the machine, and it ran out of its three
	// minutes when four-node clusters of containers were starting around it.
	bin, dir := buildBinary(t), t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	copyTree(t, filepath.Join("testdata", "schema-2"), dir)

	srv, client := schema2Server(ctx, t, bin, dir)
	read := func(bucket, key string, version *string) (string, error) {
		out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: version})
		if err != nil {
			return "", err
		}
		defer func() { _ = out.Body.Close() }()
		data, err := io.ReadAll(out.Body)
		return string(data), err
	}
	for key, want := range map[string]string{
		"reports/2026/q1.txt": "first quarter", "reports/2026/q2.txt": "second quarter", "reports/summary.txt": "summary",
		"public/readme.txt": "for everybody", "Tagged.txt": strings.Repeat("t", 70_000),
	} {
		if got, err := read(schema2Bucket, key, nil); err != nil || got != want {
			t.Errorf("object %s of the schema-2 deployment: %d bytes, %v", key, len(got), err)
		}
	}
	listed, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(schema2Bucket), Prefix: aws.String("reports/"), Delimiter: aws.String("/")})
	if err != nil || len(listed.Contents) != 1 || len(listed.CommonPrefixes) != 1 || aws.ToString(listed.CommonPrefixes[0].Prefix) != "reports/2026/" {
		t.Errorf("listing under reports/: %d keys, %d prefixes, %v", len(listed.Contents), len(listed.CommonPrefixes), err)
	}

	// The bucket's configuration, as it was written.
	policy, err := client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(schema2Bucket)})
	if err != nil || aws.ToString(policy.Policy) != schema2Policy {
		t.Errorf("bucket policy = %s, %v", aws.ToString(policy.Policy), err)
	}
	cors, err := client.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String(schema2Bucket)})
	if err != nil || len(cors.CORSRules) != 1 || cors.CORSRules[0].AllowedOrigins[0] != "https://app.example.test" {
		t.Errorf("CORS rules = %+v, %v", cors, err)
	}
	lifecycle, err := client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(schema2Bucket)})
	if err != nil || len(lifecycle.Rules) != 1 || aws.ToString(lifecycle.Rules[0].ID) != "old-drafts" {
		t.Errorf("lifecycle rules = %+v, %v", lifecycle, err)
	}
	// Tags keep the case they were given in: nothing is folded on reading.
	bucketTags, err := client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(schema2Bucket)})
	if err != nil || tagsOf(bucketTags.TagSet)["Team"] != "Storage" || tagsOf(bucketTags.TagSet)["env"] != "Prod" {
		t.Errorf("bucket tags = %v, %v", bucketTags, err)
	}
	objectTags, err := client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(schema2Bucket), Key: aws.String("Tagged.txt")})
	if err != nil || tagsOf(objectTags.TagSet)["Project"] != "Cactus" || tagsOf(objectTags.TagSet)["Stage"] != "Draft" {
		t.Errorf("object tags = %v, %v", objectTags, err)
	}

	// Versions and markers.
	versions, err := client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(schema2Versions)})
	if err != nil || len(versions.Versions) != 3 || len(versions.DeleteMarkers) != 1 {
		t.Fatalf("versions: %d and %d markers, %v; want 3 and 1", len(versions.Versions), len(versions.DeleteMarkers), err)
	}
	if got, err := read(schema2Versions, "doc", nil); err != nil || got != "second" {
		t.Errorf("the current version of doc = %q, %v", got, err)
	}
	for _, v := range versions.Versions {
		if aws.ToString(v.Key) == "doc" && !aws.ToBool(v.IsLatest) {
			if got, err := read(schema2Versions, "doc", v.VersionId); err != nil || got != "first" {
				t.Errorf("the older version of doc = %q, %v", got, err)
			}
		}
	}
	if _, err := read(schema2Versions, "gone", nil); apiCode(err) != "NoSuchKey" {
		t.Errorf("the object under a delete marker: %v", err)
	}

	// The upload that was in progress is still there, with its part, and
	// is finished by this binary.
	uploads, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(schema2Bucket)})
	if err != nil || len(uploads.Uploads) != 1 || aws.ToString(uploads.Uploads[0].Key) != "film" {
		t.Fatalf("uploads in progress = %+v, %v", uploads, err)
	}
	id := uploads.Uploads[0].UploadId
	parts, err := client.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(schema2Bucket), Key: aws.String("film"), UploadId: id})
	if err != nil || len(parts.Parts) != 1 || aws.ToInt64(parts.Parts[0].Size) != int64(len("reel one ")*fixtureReel) {
		t.Fatalf("parts of the upload in progress = %+v, %v", parts, err)
	}
	if _, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(schema2Bucket), Key: aws.String("film"), UploadId: id,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: parts.Parts[0].ETag}}},
	}); err != nil {
		t.Fatalf("CompleteMultipartUpload of the old upload: %v", err)
	}
	srv.stop(t)

	// After a restart, the same - and the film.
	srv, client = schema2Server(ctx, t, bin, dir)
	film, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(schema2Bucket), Key: aws.String("film")})
	if err != nil {
		t.Fatalf("the finished film: %v", err)
	}
	data, _ := io.ReadAll(film.Body)
	_ = film.Body.Close()
	if want := bytes.Repeat([]byte("reel one "), fixtureReel); !bytes.Equal(data, want) {
		t.Errorf("the film is %d bytes, want %d", len(data), len(want))
	}
	srv.stop(t)
}

// fixtureReel is how often the one part of the fixture's upload repeats its
// words. The part is small: it is the last, and a last part may be.
const fixtureReel = 2_000
