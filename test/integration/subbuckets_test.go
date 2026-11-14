//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/albievan/cactusfs/test/clustertest"
)

// errWrongAnswer marks an answer that is wrong rather than missing: a
// request that must be refused was let in or told something else than 403
// AccessDenied, or one that is allowed was answered with other content. The
// test fails on it at once; it is never waited out, not even during an
// election. An allowed request that fails is waited out: refusing too much
// lets nobody in, and is caught when the wait runs out.
var errWrongAnswer = errors.New("wrong answer")

// sdkStatus is the status of the answer to a call of the SDK: 200 when it
// succeeded, 0 when no answer came.
func sdkStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	return statusOf(err)
}

// xmlCode is the code of an S3 error document, or "" for any other body.
func xmlCode(body []byte) string {
	var problem struct {
		Code string `xml:"Code"`
	}
	if xml.Unmarshal(body, &problem) != nil {
		return ""
	}
	return problem.Code
}

// mustRefuse judges the answer to a request that must be refused: nil for
// 403 AccessDenied; a plain error when the server gave no answer or one of
// 5xx, which an election may cause; errWrongAnswer for anything else - a 2xx
// lets the caller in, and another 4xx (404, say) is not the refusal either.
func mustRefuse(what string, status int, code string, err error) error {
	switch {
	case status == http.StatusForbidden && code == "AccessDenied":
		return nil
	case status == 0 || status >= http.StatusInternalServerError:
		return errors.Join(fmt.Errorf("%s: not answered yet (status %d)", what, status), err)
	default:
		return errors.Join(fmt.Errorf("%w: %s = %d %q, want 403 AccessDenied", errWrongAnswer, what, status, code), err)
	}
}

// subBucketAnswers is what a plain user and an anonymous client are told
// through one node about the keys of the test's buckets; nil when every
// answer is the one expected. Every request is made, whatever came before,
// so that a wrong answer is seen even in a round where another request was
// not answered; the error wraps errWrongAnswer when any answer was wrong.
func subBucketAnswers(ctx context.Context, c *clustertest.Cluster, id string, user *s3.Client, content map[string][]byte) error {
	var errs []error
	// The user's own policies grant nothing of the bucket; the sub-bucket's
	// lets them read its keys and list it.
	switch got, err := gridGet(ctx, user, "parent", "sub/k"); {
	case err != nil:
		errs = append(errs, fmt.Errorf("GET parent/sub/k as plain through %s: %w", id, err))
	case !bytes.Equal(got, content["parent/sub/k"]):
		errs = append(errs, fmt.Errorf("%w: GET parent/sub/k as plain through %s: the content differs", errWrongAnswer, id))
	}
	// Outside the sub-bucket: at the top of the bucket, and under a key
	// that begins with the sub-bucket's name but not its name and a slash.
	for _, key := range []string{"k", "subway/k"} {
		_, err := gridGet(ctx, user, "parent", key)
		errs = append(errs, mustRefuse("GET parent/"+key+" as plain through "+id, sdkStatus(err), apiCode(err), err))
	}
	listed, err := user.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("parent"), Prefix: aws.String("sub/")})
	if err != nil {
		errs = append(errs, fmt.Errorf("ListObjectsV2 of parent with the prefix sub/ as plain through %s: %w", id, err))
	} else {
		keys := make([]string, 0, len(listed.Contents))
		for _, object := range listed.Contents {
			keys = append(keys, aws.ToString(object.Key))
		}
		if want := []string{"sub/j", "sub/k"}; !slices.Equal(keys, want) {
			errs = append(errs, fmt.Errorf("%w: ListObjectsV2 of parent with the prefix sub/ as plain through %s = %q, want %q", errWrongAnswer, id, keys, want))
		}
	}
	// Only a prefix that begins with the sub-bucket's name and a slash is in
	// the sub-bucket: without one, or with "sub" alone, the bucket is listed,
	// which nothing grants the user.
	for _, prefix := range []*string{nil, aws.String("sub")} {
		_, err := user.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("parent"), Prefix: prefix})
		what := fmt.Sprintf("ListObjectsV2 of parent with the prefix %q as plain through %s", aws.ToString(prefix), id)
		errs = append(errs, mustRefuse(what, sdkStatus(err), apiCode(err), err))
	}

	// Anonymous: the public bucket is read by anyone; its private
	// sub-bucket by nobody without credentials; and a sub-bucket's Allow
	// never admits an anonymous caller.
	switch status, body, err := anonymousGet(ctx, c, id, "public", "open"); {
	case err != nil || status != http.StatusOK:
		errs = append(errs, errors.Join(fmt.Errorf("anonymous GET public/open through %s = %d", id, status), err))
	case !bytes.Equal(body, content["public/open"]):
		errs = append(errs, fmt.Errorf("%w: anonymous GET public/open through %s: the content differs", errWrongAnswer, id))
	}
	for _, object := range []struct{ bucket, key string }{{"public", "private/secret"}, {"parent", "sub/k"}} {
		status, body, err := anonymousGet(ctx, c, id, object.bucket, object.key)
		errs = append(errs, mustRefuse("anonymous GET "+object.bucket+"/"+object.key+" through "+id, status, xmlCode(body), err))
	}
	return errors.Join(errs...)
}

// anonymousGet reads an object path-style with no credentials at all.
func anonymousGet(ctx context.Context, c *clustertest.Cluster, id, bucketName, key string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+id+":9000/"+bucketName+"/"+key, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.HTTP(id).Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("anonymous GET %s/%s through %s: %w", bucketName, key, id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("anonymous GET %s/%s through %s: %w", bucketName, key, id, err)
	}
	return resp.StatusCode, body, nil
}

// TestSubBucketsAcrossTheCluster: sub-buckets in the product as it is
// deployed (ADR-0017, PLAN C.7). Through the management API root makes a
// bucket and a sub-bucket whose policy lets a plain user - an API key and no
// policy of their own that grants the bucket - read and list the
// sub-bucket's keys, and a public bucket with a private sub-bucket. With
// aws-sdk-go-v2 the user reads and lists the sub-bucket and nothing else of
// the bucket, and an anonymous client reads the public bucket but not its
// private sub-bucket. The answers are the same through another node once the
// leader of the metadata group has been stopped and another elected.
func TestSubBucketsAcrossTheCluster(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	c := clustertest.Start(t, clustertest.Options{})
	root := gridBearer(c.AccessKey, c.SecretKey)

	mustAdmin := func(id string, want int, method, path string, body any) {
		t.Helper()
		status, _, answer, err := gridCall(ctx, c, id, root, method, path, body)
		if err != nil {
			t.Fatal(err)
		}
		if status != want {
			t.Fatalf("%s %s through %s = %d %s, want %d", method, path, id, status, answer, want)
		}
	}
	mustAdmin("node1", http.StatusCreated, http.MethodPost, "/buckets", map[string]any{"name": "parent"})
	mustAdmin("node1", http.StatusOK, http.MethodPut, "/buckets/parent/subbuckets/sub", map[string]any{"policy": map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{
			map[string]any{
				"Effect": "Allow", "Principal": map[string]any{"AWS": "arn:aws:iam:::user/plain"},
				"Action": "s3:GetObject", "Resource": "arn:aws:s3:::parent/sub/*",
			},
			map[string]any{
				"Effect": "Allow", "Principal": map[string]any{"AWS": "arn:aws:iam:::user/plain"},
				"Action": "s3:ListBucket", "Resource": "arn:aws:s3:::parent",
			},
		},
	}})
	mustAdmin("node1", http.StatusCreated, http.MethodPost, "/buckets", map[string]any{"name": "public"})
	mustAdmin("node1", http.StatusOK, http.MethodPut, "/buckets/public/policy", map[string]any{"document": map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::public/*",
		}},
	}})
	mustAdmin("node1", http.StatusOK, http.MethodPut, "/buckets/public/subbuckets/private", map[string]any{"policy": map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{map[string]any{
			"Effect": "Deny", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::public/private/*",
		}},
	}})

	// A user with no role, no group and no policy: what they may do comes
	// from the sub-bucket's policy alone.
	accessKeyID, secretKey, err := gridUser(ctx, c, "node1", root, "plain", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}

	content := map[string][]byte{}
	for _, object := range []struct{ bucket, key string }{
		{"parent", "sub/k"},
		{"parent", "sub/j"},
		{"parent", "k"},
		{"parent", "subway/k"},
		{"public", "open"},
		{"public", "private/secret"},
	} {
		data := randomBytes(t, 4096)
		if err := c.Put(ctx, "node1", object.bucket, object.key, data); err != nil {
			t.Fatalf("Put %s/%s through node1: %v", object.bucket, object.key, err)
		}
		content[object.bucket+"/"+object.key] = data
	}

	// Through a node other than the one that took the changes.
	if err := subBucketAnswers(ctx, c, "node2", gridS3(c, "node2", accessKeyID, secretKey), content); err != nil {
		t.Fatal(err)
	}

	// The leader is stopped, as an orchestrator stops it; another is
	// elected, and a node that is neither answers as before.
	leader, err := askLeader(ctx, c, "node2")
	if err != nil {
		t.Fatal(err)
	}
	other := "node3"
	if leader == other {
		other = "node4"
	}
	c.Stop(leader)
	c.Eventually("a leader other than "+leader, 2*time.Minute, func() bool {
		now, err := askLeader(ctx, c, other)
		return err == nil && now != leader
	})
	user := gridS3(c, other, accessKeyID, secretKey)
	// Each missing answer is told once, so that a wait that times out says
	// what was missing.
	// An answer that is wrong - above all one that lets in what must be
	// refused - fails the test at once: only no answer is waited out.
	told := ""
	c.Eventually("the sub-bucket's answers through "+other+" without "+leader, 2*time.Minute, func() bool {
		err := subBucketAnswers(ctx, c, other, user, content)
		if errors.Is(err, errWrongAnswer) {
			t.Fatalf("through %s without %s: %v", other, leader, err)
		}
		if err != nil && err.Error() != told {
			told = err.Error()
			t.Logf("not yet: %v", err)
		}
		return err == nil
	})
}
