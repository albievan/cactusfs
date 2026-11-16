//go:build s3compat

package s3compat_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// replicationRole is the role ARN by which a configuration names a target
// of the deployment.
func replicationRole(target string) *string {
	return aws.String("arn:cactusfs:replication:::" + target)
}

// toBucket is the destination of a rule: a bucket of the other deployment.
func toBucket(name string) *types.Destination {
	return &types.Destination{Bucket: aws.String("arn:aws:s3:::" + name)}
}

// noMarkers is the delete markers of a rule, not copied.
func noMarkers() *types.DeleteMarkerReplication {
	return &types.DeleteMarkerReplication{Status: types.DeleteMarkerReplicationStatusDisabled}
}

// replicationRuleOf is one rule as the SDK reads it: ID, status, priority,
// prefix, delete markers and destination, comparable.
type replicationRuleOf struct {
	id, status    string
	priority      int32
	prefix        string
	markers       string
	destination   string
	filterWritten bool
}

func replicationRulesOf(in []types.ReplicationRule) []replicationRuleOf {
	out := []replicationRuleOf{}
	for _, r := range in {
		rule := replicationRuleOf{id: aws.ToString(r.ID), status: string(r.Status), priority: aws.ToInt32(r.Priority)}
		if r.Filter != nil {
			rule.filterWritten, rule.prefix = true, aws.ToString(r.Filter.Prefix)
		}
		if r.DeleteMarkerReplication != nil {
			rule.markers = string(r.DeleteMarkerReplication.Status)
		}
		if r.Destination != nil {
			rule.destination = aws.ToString(r.Destination.Bucket)
		}
		out = append(out, rule)
	}
	return out
}

// TestBucketReplicationConfiguration: the replication configuration of a
// bucket as the SDK sets, reads and removes it (ADR-0026, PLAN 8.1.3). The
// role ARN names a target of kind s3 the administrator defined and opened
// to the bucket; GET gives back what was put, every rule with its filter,
// priority and delete markers said; what S3 has and is not done here is
// refused as not implemented; a bucket whose versioning is not enabled is
// 400 InvalidRequest; a bucket without a configuration is 404
// ReplicationConfigurationNotFoundError; DELETE removes it.
func TestBucketReplicationConfiguration(t *testing.T) {
	name := newBucket(t)
	c := client()
	// Another deployment of the bucket's own, of a name no other run has.
	site := "site-" + strings.TrimPrefix(name, "compat-")
	// Its key is made for the run: nobody has it, and nothing is sent.
	keyID, secret, err := randomCredentials()
	if err != nil {
		t.Fatal(err)
	}
	definition, err := json.Marshal(map[string]any{
		"kind": "s3", "endpoint": "https://s3.site-b.example", "region": "us-east-1", "buckets": []string{name},
		"auth": map[string]any{"mode": "sigv4", "id": keyID, "secret": secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, body := quotaAdmin(t, http.MethodPut, "/api/v1/targets/"+site, definition); status != http.StatusOK {
		t.Fatalf("PUT the target %s = %d %s", site, status, body)
	}
	// Registered after the bucket's own clean-up, so run before it: a
	// target that a configuration names is not deleted.
	t.Cleanup(func() {
		_, _ = c.DeleteBucketReplication(context.Background(), &s3.DeleteBucketReplicationInput{Bucket: aws.String(name)})
		if status, body := adminAt(context.Background(), t, http.MethodDelete, "/api/v1/targets/"+site, nil); status != http.StatusNoContent {
			t.Errorf("DELETE the target %s = %d %s", site, status, body)
		}
	})

	set := func(config types.ReplicationConfiguration) error {
		_, err := c.PutBucketReplication(t.Context(), &s3.PutBucketReplicationInput{Bucket: aws.String(name), ReplicationConfiguration: &config})
		return err
	}
	failsWith := func(err error, status int, code string) bool {
		var failed interface{ HTTPStatusCode() int }
		return errors.As(err, &failed) && failed.HTTPStatusCode() == status && errorCode(err) == code
	}
	config := types.ReplicationConfiguration{
		Role: replicationRole(site),
		Rules: []types.ReplicationRule{
			{
				ID: aws.String("logs"), Status: types.ReplicationRuleStatusEnabled, Priority: aws.Int32(2),
				Filter:                  &types.ReplicationRuleFilter{Prefix: aws.String("logs/")},
				DeleteMarkerReplication: &types.DeleteMarkerReplication{Status: types.DeleteMarkerReplicationStatusEnabled},
				Destination:             toBucket("copy"),
			},
			{
				Status: types.ReplicationRuleStatusDisabled, Priority: aws.Int32(1),
				Filter:                  &types.ReplicationRuleFilter{Prefix: aws.String("tmp/")},
				DeleteMarkerReplication: &types.DeleteMarkerReplication{Status: types.DeleteMarkerReplicationStatusDisabled},
				Destination:             toBucket("copy"),
			},
		},
	}

	// Versioning not enabled: refused as S3 refuses it.
	if err := set(config); !failsWith(err, http.StatusBadRequest, "InvalidRequest") {
		t.Errorf("PutBucketReplication of a bucket that keeps no versions: %v; want 400 InvalidRequest", err)
	}
	if _, err := c.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{
		Bucket: aws.String(name), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatalf("PutBucketVersioning: %v", err)
	}

	// None yet.
	_, err = c.GetBucketReplication(t.Context(), &s3.GetBucketReplicationInput{Bucket: aws.String(name)})
	if !failsWith(err, http.StatusNotFound, "ReplicationConfigurationNotFoundError") {
		t.Fatalf("GetBucketReplication of a bucket without one: %v", err)
	}

	if err := set(config); err != nil {
		t.Fatalf("PutBucketReplication: %v", err)
	}
	destination := "arn:aws:s3:::copy"
	want := []replicationRuleOf{
		{"logs", "Enabled", 2, "logs/", "Enabled", destination, true},
		// The ID the server gave the rule that had none.
		{"rule-1", "Disabled", 1, "tmp/", "Disabled", destination, true},
	}
	get := func() *s3.GetBucketReplicationOutput {
		t.Helper()
		out, err := c.GetBucketReplication(t.Context(), &s3.GetBucketReplicationInput{Bucket: aws.String(name)})
		if err != nil {
			t.Fatalf("GetBucketReplication: %v", err)
		}
		return out
	}
	out := get()
	if out.ReplicationConfiguration == nil || aws.ToString(out.ReplicationConfiguration.Role) != aws.ToString(replicationRole(site)) {
		t.Fatalf("GetBucketReplication = %+v", out.ReplicationConfiguration)
	}
	if got := replicationRulesOf(out.ReplicationConfiguration.Rules); !reflect.DeepEqual(got, want) {
		t.Errorf("rules read back\n%+v\nwant\n%+v", got, want)
	}

	// While it is there, the bucket keeps versions.
	_, err = c.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{
		Bucket: aws.String(name), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusSuspended},
	})
	if !failsWith(err, http.StatusConflict, "InvalidBucketState") {
		t.Errorf("suspending the versions of a replicated bucket: %v; want 409 InvalidBucketState", err)
	}

	one := func(change func(*types.ReplicationRule)) types.ReplicationConfiguration {
		rule := types.ReplicationRule{
			Status: types.ReplicationRuleStatusEnabled, Priority: aws.Int32(1), Filter: &types.ReplicationRuleFilter{Prefix: aws.String("")},
			DeleteMarkerReplication: &types.DeleteMarkerReplication{Status: types.DeleteMarkerReplicationStatusDisabled}, Destination: toBucket("copy"),
		}
		change(&rule)
		return types.ReplicationConfiguration{Role: replicationRole(site), Rules: []types.ReplicationRule{rule}}
	}
	for refusal, tt := range map[string]struct {
		config types.ReplicationConfiguration
		status int
		code   string
	}{
		"a filter on a tag": {one(func(r *types.ReplicationRule) {
			r.Filter = &types.ReplicationRuleFilter{Tag: &types.Tag{Key: aws.String("a"), Value: aws.String("b")}}
		}), http.StatusNotImplemented, "NotImplemented"},
		"the objects already there": {one(func(r *types.ReplicationRule) {
			r.ExistingObjectReplication = &types.ExistingObjectReplication{Status: types.ExistingObjectReplicationStatusEnabled}
		}), http.StatusNotImplemented, "NotImplemented"},
		"a storage class": {one(func(r *types.ReplicationRule) {
			r.Destination.StorageClass = types.StorageClassStandardIa
		}), http.StatusNotImplemented, "NotImplemented"},
		"two destination buckets": {types.ReplicationConfiguration{Role: replicationRole(site), Rules: []types.ReplicationRule{
			{ID: aws.String("a"), Status: types.ReplicationRuleStatusEnabled, Priority: aws.Int32(1), Filter: &types.ReplicationRuleFilter{Prefix: aws.String("a/")}, DeleteMarkerReplication: noMarkers(), Destination: toBucket("copy")},
			{ID: aws.String("b"), Status: types.ReplicationRuleStatusEnabled, Priority: aws.Int32(2), Filter: &types.ReplicationRuleFilter{Prefix: aws.String("b/")}, DeleteMarkerReplication: noMarkers(), Destination: toBucket("other")},
		}}, http.StatusNotImplemented, "NotImplemented"},
		// A rule of the later form says what becomes of delete markers,
		// as S3 requires; a rule says which keys it is for.
		"the later form without delete markers": {one(func(r *types.ReplicationRule) {
			r.DeleteMarkerReplication = nil
		}), http.StatusBadRequest, "MalformedXML"},
		"a rule for no keys said": {one(func(r *types.ReplicationRule) {
			r.Filter = nil
		}), http.StatusBadRequest, "MalformedXML"},
		"two rules of one priority that overlap": {types.ReplicationConfiguration{Role: replicationRole(site), Rules: []types.ReplicationRule{
			{ID: aws.String("a"), Status: types.ReplicationRuleStatusEnabled, Priority: aws.Int32(1), Filter: &types.ReplicationRuleFilter{Prefix: aws.String("logs/")}, DeleteMarkerReplication: noMarkers(), Destination: toBucket("copy")},
			{ID: aws.String("b"), Status: types.ReplicationRuleStatusEnabled, Priority: aws.Int32(1), Filter: &types.ReplicationRuleFilter{Prefix: aws.String("logs/2026/")}, DeleteMarkerReplication: noMarkers(), Destination: toBucket("copy")},
		}}, http.StatusBadRequest, "InvalidArgument"},
		"a role of AWS": {types.ReplicationConfiguration{
			Role: aws.String("arn:aws:iam::123456789012:role/replication"), Rules: one(func(*types.ReplicationRule) {}).Rules,
		}, http.StatusBadRequest, "InvalidArgument"},
		"a target that is not there": {types.ReplicationConfiguration{
			Role: replicationRole("no-such-target"), Rules: one(func(*types.ReplicationRule) {}).Rules,
		}, http.StatusBadRequest, "InvalidArgument"},
	} {
		if err := set(tt.config); !failsWith(err, tt.status, tt.code) {
			t.Errorf("PutBucketReplication with %s: %v; want %d %s", refusal, err, tt.status, tt.code)
		}
	}
	// None of them changed what was set.
	if got := replicationRulesOf(get().ReplicationConfiguration.Rules); !reflect.DeepEqual(got, want) {
		t.Errorf("a refused configuration changed the rules: %+v", got)
	}

	// DELETE removes it.
	if _, err := c.DeleteBucketReplication(t.Context(), &s3.DeleteBucketReplicationInput{Bucket: aws.String(name)}); err != nil {
		t.Fatalf("DeleteBucketReplication: %v", err)
	}
	_, err = c.GetBucketReplication(t.Context(), &s3.GetBucketReplicationInput{Bucket: aws.String(name)})
	if !failsWith(err, http.StatusNotFound, "ReplicationConfigurationNotFoundError") {
		t.Errorf("GetBucketReplication after DeleteBucketReplication: %v", err)
	}
}
