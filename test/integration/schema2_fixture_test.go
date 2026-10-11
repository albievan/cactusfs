//go:build integration && fixturegen && !windows

package integration_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// TestWriteSchema2Fixture writes testdata/schema-2 with the binary of the
// tree it is run in. It was run once, at commit 098749f:
//
//	rm -rf test/integration/testdata/schema-2
//	go test -tags='integration fixturegen' -run TestWriteSchema2Fixture ./test/integration/
//
// and must not be run again on a later tree: the fixture is what THAT binary
// wrote. It is kept so that it can be seen how the fixture came to be, and
// refuses to write over one that exists.
func TestWriteSchema2Fixture(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("testdata", "schema-2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("%s exists: the fixture is written once, by the binary it is a fixture of", dir)
	}
	bin := buildBinary(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	srv, client := schema2Server(ctx, t, bin, dir)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	put := func(bucket, key, content string, tagging *string) {
		t.Helper()
		_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(content), Tagging: tagging})
		must("PutObject "+key, err)
	}

	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(schema2Bucket)})
	must("CreateBucket", err)
	put(schema2Bucket, "reports/2026/q1.txt", "first quarter", nil)
	put(schema2Bucket, "reports/2026/q2.txt", "second quarter", nil)
	put(schema2Bucket, "reports/summary.txt", "summary", nil)
	put(schema2Bucket, "public/readme.txt", "for everybody", nil)
	put(schema2Bucket, "Tagged.txt", strings.Repeat("t", 70_000), aws.String("Project=Cactus&Stage=Draft"))
	_, err = client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(schema2Bucket), Policy: aws.String(schema2Policy)})
	must("PutBucketPolicy", err)
	_, err = client.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: aws.String(schema2Bucket), CORSConfiguration: &types.CORSConfiguration{
		CORSRules: []types.CORSRule{{AllowedMethods: []string{"GET"}, AllowedOrigins: []string{"https://app.example.test"}, MaxAgeSeconds: aws.Int32(600)}},
	}})
	must("PutBucketCors", err)
	_, err = client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(schema2Bucket),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{{
			ID: aws.String("old-drafts"), Status: types.ExpirationStatusEnabled,
			Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("drafts/")},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(36500)},
		}}},
	})
	must("PutBucketLifecycleConfiguration", err)
	_, err = client.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: aws.String(schema2Bucket), Tagging: &types.Tagging{
		TagSet: []types.Tag{{Key: aws.String("Team"), Value: aws.String("Storage")}, {Key: aws.String("env"), Value: aws.String("Prod")}},
	}})
	must("PutBucketTagging", err)
	created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(schema2Bucket), Key: aws.String("film")})
	must("CreateMultipartUpload", err)
	_, err = client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(schema2Bucket), Key: aws.String("film"), UploadId: created.UploadId, PartNumber: aws.Int32(1),
		Body: bytes.NewReader(bytes.Repeat([]byte("reel one "), fixtureReel)),
	})
	must("UploadPart", err)

	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(schema2Versions)})
	must("CreateBucket", err)
	_, err = client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(schema2Versions), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	})
	must("PutBucketVersioning", err)
	put(schema2Versions, "doc", "first", nil)
	put(schema2Versions, "doc", "second", nil)
	put(schema2Versions, "gone", "soon", nil)
	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(schema2Versions), Key: aws.String("gone")})
	must("DeleteObject", err)
	srv.stop(t)
}
