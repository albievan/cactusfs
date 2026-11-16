package main

import (
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
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
)

// TestANodeWiresNotificationRules holds the composition root to the use
// case of notification rules (ADR-0025, PLAN 8.2.2): a node has one, over
// the metadata store of Raft and bbolt and its targets - rules that name a
// target open to the bucket are kept and read back in their order, a
// target that is not open to it is refused, and an empty set removes them.
// Who delivers them is PLAN 8.2.5's.
func TestANodeWiresNotificationRules(t *testing.T) {
	t.Parallel()
	n := mustStart(t, nodeConfig(t.TempDir())).node
	ctx := t.Context()
	root := iam.Principal{AccessKeyID: "ROOT", Root: true}
	if n.notifications == nil {
		t.Fatal("the node has no use case of notification rules")
	}
	if err := n.buckets.Create(ctx, root, "photos"); err != nil {
		t.Fatal(err)
	}
	for name, buckets := range map[string][]string{"hook": {"photos"}, "elsewhere": {"logs"}} {
		if _, err := n.targets.Put(ctx, root, app.TargetInput{Name: name, Kind: delivery.KindWebhook, Endpoint: "https://hooks.example.com", Buckets: buckets}); err != nil {
			t.Fatalf("Put of target %s: %v", name, err)
		}
	}
	rules := bucket.NotificationConfig{Rules: []bucket.NotificationRule{
		{ID: "b", As: bucket.NotifyAsTopic, Target: "hook", On: []delivery.Event{delivery.EventDelete}},
		{ID: "a", As: bucket.NotifyAsQueue, Target: "hook", On: []delivery.Event{delivery.EventPut}, Prefix: "in/"},
	}}
	if _, err := n.notifications.Set(ctx, root, "photos", rules); err != nil {
		t.Fatalf("Set = %v", err)
	}
	got, err := n.notifications.Get(ctx, root, "photos")
	if err != nil || !reflect.DeepEqual(got, rules) {
		t.Fatalf("Get = %+v, %v; want %+v", got, err, rules)
	}
	closed := bucket.NotificationConfig{Rules: []bucket.NotificationRule{{ID: "c", As: bucket.NotifyAsQueue, Target: "elsewhere", On: []delivery.Event{delivery.EventPut}}}}
	if _, err := n.notifications.Set(ctx, root, "photos", closed); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Errorf("rules that name a target closed to the bucket = %v", err)
	}
	if _, err := n.notifications.Set(ctx, root, "photos", bucket.NotificationConfig{}); err != nil {
		t.Fatalf("Set of no rules = %v", err)
	}
	if got, err := n.notifications.Get(ctx, root, "photos"); err != nil || !got.Empty() {
		t.Errorf("Get after the rules were removed = %+v, %v", got, err)
	}
}

// TestTheServedAPIsReadNotificationRules holds server.go to the wiring of
// PLAN 8.2.2: both APIs of a node started as the binary starts it are given
// the use case, so GET ?notification and GET /buckets/{b}/notifications
// answer 200, not the 501 of a node without it.
func TestTheServedAPIsReadNotificationRules(t *testing.T) {
	t.Parallel()
	call, s3Addr, root, stop := servedNode(t)
	defer stop()

	if status, body := call(http.MethodPost, "/api/v1/buckets", root, `{"name":"photos"}`); status != http.StatusCreated {
		t.Fatalf("POST /buckets = %d %s", status, body)
	}
	if status, body := call(http.MethodGet, "/api/v1/buckets/photos/notifications", root, ""); status != http.StatusOK || body != `{"rules":[]}`+"\n" {
		t.Errorf("GET /buckets/photos/notifications on the served node = %d %q, want 200 and no rules", status, body)
	}

	keyID, secret, _ := strings.Cut(root, ":")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+s3Addr+"/photos?notification", nil)
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
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<NotificationConfiguration") {
		t.Errorf("GET /photos?notification on the served node = %d %s, want 200 and a configuration", resp.StatusCode, body)
	}
}
