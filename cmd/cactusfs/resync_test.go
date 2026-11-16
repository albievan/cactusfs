package main

import (
	"bytes"
	"encoding/pem"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
)

// TestANodeResyncsWhatWasThereBefore holds the composition root to the
// resync (PLAN 8.1.6): versions put before the bucket had a replication
// configuration are not sent by the outbox; a resync asked for through the
// use case is walked by the node, which leads, over the metadata store of
// Raft - the cursor stored through it, the statuses acknowledged by version -
// and every version arrives at the other deployment, COMPLETED here, the
// resync done.
func TestANodeResyncsWhatWasThereBefore(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	cfgB := nodeConfig(t.TempDir())
	cfgB.Region = siteRegion
	b := mustStart(t, cfgB)
	keyB := shownCredentials(t, b.notice)
	rootB := iam.Principal{AccessKeyID: keyB.AccessKeyID, Root: true}
	siteB := s3Of(t, b.node, true)

	cfgA := nodeConfig(t.TempDir())
	cfgA.Region = siteRegion
	cfgA.EgressAllow = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	cfgA.DeliveryTimeout = 5 * time.Second
	a := mustStart(t, cfgA)
	keyA := shownCredentials(t, a.notice)
	rootA := iam.Principal{AccessKeyID: keyA.AccessKeyID, Root: true}

	if err := b.node.buckets.Create(ctx, rootB, "copies"); err != nil {
		t.Fatal(err)
	}
	if err := b.node.buckets.SetVersioning(ctx, rootB, "copies", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	if _, err := b.node.replication.SetReplicaMode(ctx, rootB, "copies", a.node.publishers.source); err != nil {
		t.Fatalf("SetReplicaMode: %v", err)
	}
	if err := a.node.buckets.Create(ctx, rootA, "photos"); err != nil {
		t.Fatal(err)
	}
	if err := a.node.buckets.SetVersioning(ctx, rootA, "photos", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	// Written before any configuration, through the S3 API: no row, no
	// status.
	writer := s3Caller{t: t, base: s3Of(t, a.node, false).URL, client: http.DefaultClient, creds: aws.Credentials{
		AccessKeyID: keyA.AccessKeyID, SecretAccessKey: keyA.SecretKey,
	}}
	written := map[string][]byte{}
	var versions []object.Version
	for i := range 4 {
		key := "old/" + strconv.Itoa(i)
		data := []byte("before the configuration " + strconv.Itoa(i))
		header, _ := writer.do(http.MethodPut, "/photos/"+key, data)
		ref, err := object.ParseVersionRef(header.Get("X-Amz-Version-Id"))
		if err != nil {
			t.Fatalf("the version ID of %s: %v", key, err)
		}
		v, err := a.node.objects.HeadVersion(ctx, rootA, "photos", key, ref)
		if err != nil || v.Replication != "" {
			t.Fatalf("%s before the configuration: %+v, %v", key, v, err)
		}
		written[key] = data
		versions = append(versions, v)
	}
	if _, err := a.node.targets.Put(ctx, rootA, app.TargetInput{
		Name: "site-b", Kind: delivery.KindS3, Endpoint: siteB.URL, Region: siteRegion, Buckets: []string{"photos"},
		CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: siteB.Certificate().Raw}),
		Auth:  delivery.Auth{Mode: delivery.AuthSigV4, ID: keyB.AccessKeyID}, Secret: keyB.SecretKey,
	}); err != nil {
		t.Fatalf("Put of the target: %v", err)
	}
	if _, err := a.node.replication.SetConfiguration(ctx, rootA, "photos", bucket.ReplicationConfig{
		Target: "site-b", Bucket: "copies", Rules: []bucket.ReplicationRule{{ID: "all", Enabled: true}},
	}); err != nil {
		t.Fatalf("SetConfiguration: %v", err)
	}
	started, err := a.node.replication.Resync(ctx, rootA, "photos")
	if err != nil || started.State != bucket.ResyncRunning {
		t.Fatalf("Resync = %+v, %v", started, err)
	}
	a.node.startBackground(ctx, cfgA)

	deadline := time.Now().Add(60 * time.Second)
	for {
		record, err := a.node.meta.GetResync(ctx, "photos")
		if err != nil {
			t.Fatal(err)
		}
		if record.State == bucket.ResyncDone {
			if record.Scanned != 4 || record.Sent != 4 {
				t.Errorf("the resync is done with %+v, want 4 looked at and sent", record)
			}
			break
		}
		if time.Now().After(deadline) {
			progress, ok := a.node.resync.Progress("photos")
			t.Fatalf("the resync is %+v, not done; the leader shows %+v (%v)", record, progress, ok)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, v := range versions {
		got, err := a.node.objects.HeadVersion(ctx, rootA, "photos", v.Key, v.Ref())
		if err != nil || got.Replication != object.ReplicationCompleted {
			t.Errorf("%s at A is %q (%v), want COMPLETED", v.Key, got.Replication, err)
		}
		ref := v.Ref()
		out, err := b.node.objects.Get(ctx, rootB, app.GetInput{Bucket: "copies", Key: v.Key, Version: &ref})
		if err != nil {
			t.Fatalf("the copy of %s at B: %v", v.Key, err)
		}
		data, err := io.ReadAll(out.Body)
		_ = out.Body.Close()
		if err != nil || !bytes.Equal(data, written[v.Key]) || out.Version.Replication != object.ReplicationReplica {
			t.Errorf("the copy of %s at B: %q, %q (%v)", v.Key, data, out.Version.Replication, err)
		}
	}
}
