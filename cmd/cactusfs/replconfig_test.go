package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/albievan/cactusfs/internal/adapters/s3api"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// TestANodeWiresReplicationConfiguration holds the composition root to the
// use case of replication configuration (ADR-0026, PLAN 8.1.3): a node has
// one, over the metadata store of Raft and bbolt and its targets - a
// configuration that names another deployment open to the bucket is kept
// and read back, one of a bucket that keeps no versions is refused, one of
// a target that is not open to it too; every change is told as
// cactusfs.bucket.configuration-changed.v1 from the applied entry; and a
// removal removes it. Who sends the versions is PLAN 8.1.4's.
func TestANodeWiresReplicationConfiguration(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cfg := nodeConfig(t.TempDir())
	n := mustStart(t, cfg).node
	root := iam.Principal{AccessKeyID: "ROOT", Root: true}
	if n.replication == nil {
		t.Fatal("the node has no use case of replication")
	}
	var (
		mu      sync.Mutex
		changed []string
	)
	n.events.Subscribe("replication-config-test", func(_ context.Context, e ports.Event) {
		if data, ok := e.Data.(ports.BucketEvent); ok && e.Type == ports.EventBucketConfigChanged {
			mu.Lock()
			defer mu.Unlock()
			changed = append(changed, data.Bucket)
		}
	})
	// The bus delivers what is applied once the node runs.
	n.startBackground(ctx, cfg)
	for _, name := range []string{"photos", "plain"} {
		if err := n.buckets.Create(ctx, root, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.buckets.SetVersioning(ctx, root, "photos", bucket.VersioningEnabled); err != nil {
		t.Fatal(err)
	}
	for name, buckets := range map[string][]string{"site-b": {"photos", "plain"}, "site-c": {"logs"}} {
		if _, err := n.targets.Put(ctx, root, app.TargetInput{
			Name: name, Kind: delivery.KindS3, Endpoint: "https://s3." + name + ".example", Region: "eu-west-1", Buckets: buckets,
			Auth: delivery.Auth{Mode: delivery.AuthSigV4, ID: "AKIDSITEBEXAMPLE0001"}, Secret: "not-a-secret-of-anybody",
		}); err != nil {
			t.Fatalf("Put of target %s: %v", name, err)
		}
	}
	config := bucket.ReplicationConfig{Target: "site-b", Bucket: "copy", Rules: []bucket.ReplicationRule{
		{ID: "logs", Enabled: true, Priority: 2, Prefix: "logs/", Markers: true},
		{ID: "all", Enabled: true},
	}}
	if _, err := n.replication.SetConfiguration(ctx, root, "photos", config); err != nil {
		t.Fatalf("SetConfiguration = %v", err)
	}
	got, err := n.replication.Configuration(ctx, root, "photos")
	if err != nil || !reflect.DeepEqual(got, config) {
		t.Fatalf("Configuration = %+v, %v; want %+v", got, err, config)
	}
	if _, err := n.replication.SetConfiguration(ctx, root, "plain", config); !errors.Is(err, domain.ErrVersioningRequired) {
		t.Errorf("a configuration of a bucket that keeps no versions = %v", err)
	}
	closed := config
	closed.Target = "site-c"
	if _, err := n.replication.SetConfiguration(ctx, root, "photos", closed); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("a configuration that names a target closed to the bucket = %v", err)
	}
	// While it is there, the bucket keeps versions.
	if err := n.buckets.SetVersioning(ctx, root, "photos", bucket.VersioningSuspended); !errors.Is(err, domain.ErrInvalidBucketState) {
		t.Errorf("suspending the versions of a replicated bucket = %v", err)
	}
	if err := n.replication.DeleteConfiguration(ctx, root, "photos"); err != nil {
		t.Fatalf("DeleteConfiguration = %v", err)
	}
	if got, err := n.replication.Configuration(ctx, root, "photos"); err != nil || !got.Empty() {
		t.Errorf("Configuration after the removal = %+v, %v", got, err)
	}
	// The set and the removal, each told once from the entry applied.
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		told := append([]string(nil), changed...)
		mu.Unlock()
		if reflect.DeepEqual(told, []string{"photos", "photos"}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the changes were told as %v, want the two of photos", told)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTheServedAPIsReadReplicationConfiguration holds server.go to the
// wiring of PLAN 8.1.3: both APIs of a node started as the binary starts it
// are given the use case, so GET ?replication and GET
// /buckets/{b}/replication of a bucket without one answer 404, not the 501
// of a node without the use case.
func TestTheServedAPIsReadReplicationConfiguration(t *testing.T) {
	t.Parallel()
	call, s3Addr, root, stop := servedNode(t)
	defer stop()

	if status, body := call(http.MethodPost, "/api/v1/buckets", root, `{"name":"photos"}`); status != http.StatusCreated {
		t.Fatalf("POST /buckets = %d %s", status, body)
	}
	if status, body := call(http.MethodGet, "/api/v1/buckets/photos/replication", root, ""); status != http.StatusNotFound || !strings.Contains(body, "NoSuchEntity") {
		t.Errorf("GET /buckets/photos/replication on the served node = %d %q, want 404 NoSuchEntity", status, body)
	}

	keyID, secret, _ := strings.Cut(root, ":")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+s3Addr+"/photos?replication", nil)
	if err != nil {
		t.Fatal(err)
	}
	const emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	req.Header.Set("X-Amz-Content-Sha256", emptyPayload)
	credentials := aws.Credentials{AccessKeyID: keyID, SecretAccessKey: secret}
	if err := v4.NewSigner().SignHTTP(t.Context(), credentials, req, emptyPayload, "s3", s3api.DefaultRegion, time.Now()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "<Code>ReplicationConfigurationNotFoundError</Code>") {
		t.Errorf("GET /photos?replication on the served node = %d %s, want 404 ReplicationConfigurationNotFoundError", resp.StatusCode, body)
	}
}
