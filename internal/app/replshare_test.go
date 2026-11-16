package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
)

// TestReplicationDestinationIsOneBucketsOnly (PLAN 8.1.4 review): a bucket
// of another deployment takes the versions of one bucket here. A
// configuration that names a target and a bucket there that another
// bucket's configuration names already is refused, with nothing stored and
// nothing audited; the bucket that names it may put it again; once that
// configuration is removed or points elsewhere, another bucket may name
// it.
func TestReplicationDestinationIsOneBucketsOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newReplConfigFixture(t)
	if err := f.meta.CreateBucket(ctx, bucket.Bucket{Name: "albums"}); err != nil {
		t.Fatal(err)
	}
	if err := f.meta.SetBucketVersioning(ctx, "albums", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if _, err := f.targets.Put(ctx, f.root, siteTarget("site-b", "photos", "albums", "suspended", "plain", "copies")); err != nil {
		t.Fatal(err)
	}
	to := func(destination string) bucket.ReplicationConfig {
		config := replConfig("site-b", replRule("all", 0, ""))
		config.Bucket = destination
		return config
	}
	steps := []struct {
		name        string
		bucket      string
		destination string
		remove      bool
		refused     bool
	}{
		{name: "the first bucket names it", bucket: "photos", destination: "copy"},
		{name: "another bucket names it too", bucket: "albums", destination: "copy", refused: true},
		{name: "another bucket names another", bucket: "albums", destination: "copy-2"},
		{name: "the first puts it again", bucket: "photos", destination: "copy"},
		{name: "the first moves to the second's", bucket: "photos", destination: "copy-2", refused: true},
		{name: "the second's is removed", bucket: "albums", remove: true},
		{name: "the first moves to it now", bucket: "photos", destination: "copy-2"},
		{name: "the second takes the first's old one", bucket: "albums", destination: "copy"},
	}
	for _, step := range steps {
		if step.remove {
			if err := f.replication.DeleteConfiguration(ctx, f.root, step.bucket); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
			continue
		}
		before, err := f.meta.GetBucketConfig(ctx, step.bucket, bucket.ConfigReplication)
		if err != nil {
			t.Fatal(err)
		}
		records := len(f.trail(t))
		_, err = f.replication.SetConfiguration(ctx, f.root, step.bucket, to(step.destination))
		switch {
		case step.refused && !errors.Is(err, domain.ErrInvalidArgument):
			t.Errorf("%s: %v, want a refusal", step.name, err)
		case step.refused && (!strings.Contains(err.Error(), "target site-b is not another deployment that is open to bucket "+step.bucket) ||
			strings.Contains(err.Error(), step.destination) || strings.Contains(err.Error(), "photos") && step.bucket != "photos"):
			// The one sentence of every refusal of a target: neither the
			// bucket there nor the other bucket here is named.
			t.Errorf("%s: the refusal says %q", step.name, err)
		case !step.refused && err != nil:
			t.Errorf("%s: %v", step.name, err)
		}
		if !step.refused {
			continue
		}
		after, err := f.meta.GetBucketConfig(ctx, step.bucket, bucket.ConfigReplication)
		if err != nil || string(after) != string(before) {
			t.Errorf("%s: the configuration changed on a refusal", step.name)
		}
		if len(f.trail(t)) != records {
			t.Errorf("%s: a refusal was audited as a change", step.name)
		}
	}
}
