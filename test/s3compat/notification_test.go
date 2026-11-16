//go:build s3compat

package s3compat_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// adminAt is quotaAdmin with a context of its own: a clean-up runs once
// the test's context is done.
func adminAt(ctx context.Context, t *testing.T, method, path string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, "https://"+target.adminAddr+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+target.accessKey+":"+target.secretKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c := &http.Client{
		Timeout:   time.Minute,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: target.pool, MinVersion: tls.VersionTLS12}},
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Errorf("%s %s: %v", method, path, err)
		return 0, nil
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("%s %s: %v", method, path, err)
	}
	return resp.StatusCode, answer
}

// notifyARN is the ARN by which a rule names a target of the deployment.
func notifyARN(target string) *string {
	return aws.String("arn:cactusfs:notify:::" + target)
}

// keyFilter is the filter of a rule, of the rules given as name and value.
func keyFilter(rules ...string) *types.NotificationConfigurationFilter {
	filter := &types.NotificationConfigurationFilter{Key: &types.S3KeyFilter{}}
	for i := 0; i+1 < len(rules); i += 2 {
		filter.Key.FilterRules = append(filter.Key.FilterRules, types.FilterRule{Name: types.FilterRuleName(rules[i]), Value: aws.String(rules[i+1])})
	}
	return filter
}

// queueRule is one queue rule as the SDK writes it.
type queueRule struct {
	id, arn string
	events  []types.Event
	filter  []string
}

// queuesOf and topicsOf read the rules as the SDK read them: ID, ARN,
// events and filter rules, comparable.
func queuesOf(in []types.QueueConfiguration) []queueRule {
	out := []queueRule{}
	for _, q := range in {
		out = append(out, queueRule{aws.ToString(q.Id), aws.ToString(q.QueueArn), q.Events, filterOf(q.Filter)})
	}
	return out
}

func topicsOf(in []types.TopicConfiguration) []queueRule {
	out := []queueRule{}
	for _, q := range in {
		out = append(out, queueRule{aws.ToString(q.Id), aws.ToString(q.TopicArn), q.Events, filterOf(q.Filter)})
	}
	return out
}

func filterOf(f *types.NotificationConfigurationFilter) []string {
	var out []string
	if f == nil || f.Key == nil {
		return out
	}
	for _, rule := range f.Key.FilterRules {
		out = append(out, string(rule.Name), aws.ToString(rule.Value))
	}
	return out
}

// TestBucketNotificationConfiguration: the notification rules of a bucket
// as the SDK sets and reads them (ADR-0025). A rule names a target the
// administrator defined and opened to the bucket, by its ARN; GET gives
// back what was put, each kind in its order; what S3 has and is not done
// here is refused as not implemented, what S3 does not have as an invalid
// argument; an empty configuration removes the rules.
func TestBucketNotificationConfiguration(t *testing.T) {
	name := newBucket(t)
	// A target of the bucket's own, of a name no other run has.
	hook := "notify-" + strings.TrimPrefix(name, "compat-")
	definition, err := json.Marshal(map[string]any{"kind": "webhook", "endpoint": "https://hooks.example.com", "buckets": []string{name}})
	if err != nil {
		t.Fatal(err)
	}
	if status, body := quotaAdmin(t, http.MethodPut, "/api/v1/targets/"+hook, definition); status != http.StatusOK {
		t.Fatalf("PUT the target %s = %d %s", hook, status, body)
	}
	c := client()
	// Registered after the bucket's own clean-up, so run before it: a
	// target that rules name is not deleted.
	t.Cleanup(func() {
		_, _ = c.PutBucketNotificationConfiguration(context.Background(), &s3.PutBucketNotificationConfigurationInput{
			Bucket: aws.String(name), NotificationConfiguration: &types.NotificationConfiguration{},
		})
		if status, body := adminAt(context.Background(), t, http.MethodDelete, "/api/v1/targets/"+hook, nil); status != http.StatusNoContent {
			t.Errorf("DELETE the target %s = %d %s", hook, status, body)
		}
	})

	get := func() *s3.GetBucketNotificationConfigurationOutput {
		t.Helper()
		out, err := c.GetBucketNotificationConfiguration(t.Context(), &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String(name)})
		if err != nil {
			t.Fatalf("GetBucketNotificationConfiguration: %v", err)
		}
		return out
	}
	set := func(config types.NotificationConfiguration) error {
		_, err := c.PutBucketNotificationConfiguration(t.Context(), &s3.PutBucketNotificationConfigurationInput{
			Bucket: aws.String(name), NotificationConfiguration: &config,
		})
		return err
	}

	// None yet: an empty configuration, not an error.
	if out := get(); len(out.QueueConfigurations)+len(out.TopicConfigurations)+len(out.LambdaFunctionConfigurations) != 0 || out.EventBridgeConfiguration != nil {
		t.Fatalf("a bucket without rules has %+v", out)
	}

	config := types.NotificationConfiguration{
		QueueConfigurations: []types.QueueConfiguration{
			{
				Id: aws.String("uploads"), QueueArn: notifyARN(hook), Events: []types.Event{types.EventS3ObjectCreated},
				Filter: keyFilter("prefix", "logs/", "suffix", ".gz"),
			},
			{QueueArn: notifyARN(hook), Events: []types.Event{types.EventS3ObjectCreatedCompleteMultipartUpload, types.EventS3ObjectRemovedDelete}},
		},
		TopicConfigurations: []types.TopicConfiguration{
			{Id: aws.String("removals"), TopicArn: notifyARN(hook), Events: []types.Event{types.EventS3ObjectRemoved}, Filter: keyFilter("suffix", ".tmp")},
		},
	}
	if err := set(config); err != nil {
		t.Fatalf("PutBucketNotificationConfiguration: %v", err)
	}
	arn := aws.ToString(notifyARN(hook))
	wantQueues := []queueRule{
		{"uploads", arn, []types.Event{types.EventS3ObjectCreated}, []string{"prefix", "logs/", "suffix", ".gz"}},
		// The ID the server gave the rule that had none.
		{"rule-1", arn, []types.Event{types.EventS3ObjectCreatedCompleteMultipartUpload, types.EventS3ObjectRemovedDelete}, nil},
	}
	wantTopics := []queueRule{{"removals", arn, []types.Event{types.EventS3ObjectRemoved}, []string{"suffix", ".tmp"}}}
	out := get()
	if got := queuesOf(out.QueueConfigurations); !reflect.DeepEqual(got, wantQueues) {
		t.Errorf("queues read back\n%+v\nwant\n%+v", got, wantQueues)
	}
	if got := topicsOf(out.TopicConfigurations); !reflect.DeepEqual(got, wantTopics) {
		t.Errorf("topics read back\n%+v\nwant\n%+v", got, wantTopics)
	}

	one := func(arn *string, events ...types.Event) []types.QueueConfiguration {
		return []types.QueueConfiguration{{QueueArn: arn, Events: events}}
	}
	for refusal, tt := range map[string]struct {
		config types.NotificationConfiguration
		status int
		code   string
	}{
		"a Lambda function": {types.NotificationConfiguration{LambdaFunctionConfigurations: []types.LambdaFunctionConfiguration{
			{LambdaFunctionArn: aws.String("arn:aws:lambda:us-east-1:123456789012:function:f"), Events: []types.Event{types.EventS3ObjectCreated}},
		}}, http.StatusNotImplemented, "NotImplemented"},
		"EventBridge": {types.NotificationConfiguration{EventBridgeConfiguration: &types.EventBridgeConfiguration{}}, http.StatusNotImplemented, "NotImplemented"},
		"a copy":      {types.NotificationConfiguration{QueueConfigurations: one(notifyARN(hook), types.EventS3ObjectCreatedCopy)}, http.StatusNotImplemented, "NotImplemented"},
		"tagging":     {types.NotificationConfiguration{QueueConfigurations: one(notifyARN(hook), types.EventS3ObjectTagging)}, http.StatusNotImplemented, "NotImplemented"},
		"an event S3 does not have": {
			types.NotificationConfiguration{QueueConfigurations: one(notifyARN(hook), types.Event("s3:ObjectCreated:Rename"))}, http.StatusBadRequest, "InvalidArgument",
		},
		"a filter S3 does not have": {types.NotificationConfiguration{QueueConfigurations: []types.QueueConfiguration{{
			QueueArn: notifyARN(hook), Events: []types.Event{types.EventS3ObjectCreatedPut}, Filter: keyFilter("tag", "a"),
		}}}, http.StatusBadRequest, "InvalidArgument"},
		"an ARN of AWS": {
			types.NotificationConfiguration{QueueConfigurations: one(aws.String("arn:aws:sqs:us-east-1:123456789012:q"), types.EventS3ObjectCreatedPut)}, http.StatusBadRequest, "InvalidArgument",
		},
		"a target that is not there": {
			types.NotificationConfiguration{QueueConfigurations: one(notifyARN("no-such-target"), types.EventS3ObjectCreatedPut)}, http.StatusBadRequest, "InvalidArgument",
		},
	} {
		err := set(tt.config)
		var failed interface{ HTTPStatusCode() int }
		if !errors.As(err, &failed) || failed.HTTPStatusCode() != tt.status || errorCode(err) != tt.code {
			t.Errorf("PutBucketNotificationConfiguration with %s: %v; want %d %s", refusal, err, tt.status, tt.code)
		}
	}
	// None of them changed what was set.
	again := get()
	if !reflect.DeepEqual(queuesOf(again.QueueConfigurations), wantQueues) || !reflect.DeepEqual(topicsOf(again.TopicConfigurations), wantTopics) {
		t.Errorf("a refused configuration changed the rules: %+v", again)
	}

	// An empty configuration removes them.
	if err := set(types.NotificationConfiguration{}); err != nil {
		t.Fatalf("PutBucketNotificationConfiguration of none: %v", err)
	}
	if out := get(); len(out.QueueConfigurations)+len(out.TopicConfigurations) != 0 {
		t.Errorf("after an empty configuration the bucket has %+v", out)
	}
}
