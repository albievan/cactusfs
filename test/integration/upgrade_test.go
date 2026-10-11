//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// The fixture in testdata/previous-release is the data of a standalone
// deployment - four drives, the metadata directory and the sealed key file -
// as the binary of the release before object versions wrote it (commit
// 6e5c94e, entry schema 1, state schema 1). It holds one bucket with:
//
//	dir/small.txt   "written by the previous release"
//	dir/big.bin     300000 times the letter m
//	overwritten     written twice; "the second write" is current
//	deleted         written and deleted
//
// It is test data and is never used for anything else; its passphrase and
// its root credentials are the ones below.
const (
	fixturePassphrase = "upgrade fixture passphrase"
	fixtureAccessKey  = "AKIDUPGRADEFIXTURE00"
	fixtureSecretKey  = "upgrade-fixture-secret-key-0123456789abcd"
	fixtureBucket     = "from-the-previous-release"
)

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	if err := os.CopyFS(to, os.DirFS(from)); err != nil {
		t.Fatalf("copy the fixture: %v", err)
	}
}

func apiCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

// TestUpgradeFromThePreviousRelease: a deployment written by the release
// before object versions is opened by this one. Nothing is rewritten and
// nothing is lost: every object reads back, each is the null version of its
// key, and when the bucket begins to keep versions the old objects are the
// oldest versions of their keys. The state stays readable by the previous
// release until something only this one can express is written.
func TestUpgradeFromThePreviousRelease(t *testing.T) {
	t.Parallel()
	// Built before the clock of the test starts: on a cold cache the build
	// alone can take longer than the test may.
	bin, dir := buildBinary(t), t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	copyTree(t, filepath.Join("testdata", "previous-release"), dir)

	start := func() (*server, *s3.Client) {
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
	read := func(client *s3.Client, key string, version *string) (string, *s3.GetObjectOutput, error) {
		out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(fixtureBucket), Key: aws.String(key), VersionId: version})
		if err != nil {
			return "", nil, err
		}
		defer func() { _ = out.Body.Close() }()
		data, err := io.ReadAll(out.Body)
		return string(data), out, err
	}

	srv, client := start()
	want := map[string]string{
		"dir/small.txt": "written by the previous release",
		"dir/big.bin":   strings.Repeat("m", 300000),
		"overwritten":   "the second write",
	}
	for key, content := range want {
		got, out, err := read(client, key, nil)
		if err != nil || got != content {
			t.Fatalf("object %s written by the previous release: %d bytes, %v", key, len(got), err)
		}
		if out.VersionId != nil {
			t.Errorf("object %s in a bucket that never kept versions has version %q", key, aws.ToString(out.VersionId))
		}
		// Each is the null version of its key.
		if got, _, err := read(client, key, aws.String("null")); err != nil || got != content {
			t.Errorf("object %s by the name of its null version: %v", key, err)
		}
	}
	if _, _, err := read(client, "deleted", nil); apiCode(err) != "NoSuchKey" {
		t.Errorf("the object the previous release deleted: %v", err)
	}
	state, err := client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(fixtureBucket)})
	if err != nil || state.Status != "" {
		t.Fatalf("versioning of a bucket of the previous release = %q, %v", state.Status, err)
	}
	listed, err := client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(fixtureBucket)})
	if err != nil || len(listed.Versions) != 3 || len(listed.DeleteMarkers) != 0 {
		t.Fatalf("versions of the old bucket: %d and %d markers, %v; want 3 and none", len(listed.Versions), len(listed.DeleteMarkers), err)
	}
	for _, v := range listed.Versions {
		if aws.ToString(v.VersionId) != "null" || !aws.ToBool(v.IsLatest) {
			t.Errorf("old object %s is version %q, latest %v", aws.ToString(v.Key), aws.ToString(v.VersionId), aws.ToBool(v.IsLatest))
		}
	}

	// The bucket begins to keep versions. The old objects stay, as the
	// oldest version of their keys.
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(fixtureBucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatalf("PutBucketVersioning: %v", err)
	}
	put, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(fixtureBucket), Key: aws.String("dir/small.txt"), Body: strings.NewReader("written by this release")})
	if err != nil || aws.ToString(put.VersionId) == "" {
		t.Fatalf("PutObject into the now versioned bucket: %v, version %q", err, aws.ToString(put.VersionId))
	}
	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(fixtureBucket), Key: aws.String("dir/big.bin")}); err != nil {
		t.Fatal(err)
	}
	srv.stop(t)

	// After a restart everything is as it was left: the new version, the
	// marker, and under them what the previous release wrote.
	srv, client = start()
	if got, _, err := read(client, "dir/small.txt", nil); err != nil || got != "written by this release" {
		t.Errorf("the current version after a restart: %q, %v", got, err)
	}
	if got, _, err := read(client, "dir/small.txt", aws.String("null")); err != nil || got != want["dir/small.txt"] {
		t.Errorf("the previous release's version under a new one: %q, %v", got, err)
	}
	if _, _, err := read(client, "dir/big.bin", nil); apiCode(err) != "NoSuchKey" {
		t.Errorf("the deleted object: %v", err)
	}
	if got, _, err := read(client, "dir/big.bin", aws.String("null")); err != nil || got != want["dir/big.bin"] {
		t.Errorf("the previous release's version under a delete marker: %d bytes, %v", len(got), err)
	}
	listed, err = client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(fixtureBucket), Prefix: aws.String("dir/")})
	if err != nil || len(listed.Versions) != 3 || len(listed.DeleteMarkers) != 1 {
		t.Errorf("versions under dir/: %d and %d markers, %v; want 3 and 1", len(listed.Versions), len(listed.DeleteMarkers), err)
	}
	// What this release adds works on the deployment the previous one
	// made: a multipart upload, beside what was there.
	created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(fixtureBucket), Key: aws.String("multipart/film")})
	if err != nil {
		t.Fatalf("CreateMultipartUpload on the upgraded deployment: %v", err)
	}
	film := [][]byte{bytes.Repeat([]byte("reel one "), 600_000), []byte("the end")}
	var parts []types.CompletedPart
	for i, data := range film {
		number := aws.Int32(1)
		if i == 1 {
			number = aws.Int32(2)
		}
		sent, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(fixtureBucket), Key: aws.String("multipart/film"), UploadId: created.UploadId, PartNumber: number, Body: bytes.NewReader(data),
		})
		if err != nil {
			t.Fatalf("UploadPart %d: %v", i+1, err)
		}
		parts = append(parts, types.CompletedPart{PartNumber: number, ETag: sent.ETag, ChecksumCRC32: sent.ChecksumCRC32})
	}
	if _, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(fixtureBucket), Key: aws.String("multipart/film"), UploadId: created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	srv.stop(t)
	srv, client = start()
	if got, _, err := read(client, "multipart/film", nil); err != nil || got != string(film[0])+string(film[1]) {
		t.Errorf("the multipart object after a restart: %d bytes, %v", len(got), err)
	}
	if got, _, err := read(client, "dir/small.txt", aws.String("null")); err != nil || got != want["dir/small.txt"] {
		t.Errorf("the previous release's version, beside a multipart object: %q, %v", got, err)
	}
	srv.stop(t)
	if out := srv.stderr.String(); bytes.Contains([]byte(out), []byte(fixtureSecretKey)) {
		t.Fatal("a secret was written to standard error")
	}
}
