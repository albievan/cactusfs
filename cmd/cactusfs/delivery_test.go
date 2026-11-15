package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// hookTarget is a webhook that nothing is sent to: no bucket names it.
func hookTarget(name string) app.TargetInput {
	return app.TargetInput{
		Name: name, Kind: delivery.KindWebhook, Endpoint: "https://hooks.example.com/services/never-sent", Buckets: []string{"photos"},
	}
}

// statusWhenTaken asks a node for the status of a target until the node
// that leads has taken it up, at most 30 s.
func statusWhenTaken(ctx context.Context, t *testing.T, n *node, admin iam.Principal, name string) ports.TargetStatus {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		status, err := n.targets.Status(ctx, admin, name)
		if err == nil {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("the status of %s through %s: %v", name, n.id, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestANodeWiresDelivery: a node on its own leads, delivers and answers how
// a target stands from its own worker, and its gauges of delivery say so;
// before the worker runs it answers unavailable, not "nothing waits".
func TestANodeWiresDelivery(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cfg := nodeConfig(t.TempDir())
	n := mustStart(t, cfg).node
	root := iam.Principal{AccessKeyID: "ROOT", Root: true}
	if _, err := n.targets.Put(ctx, root, hookTarget("hook")); err != nil {
		t.Fatal(err)
	}
	if _, err := n.targets.Status(ctx, root, "hook"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Status before the worker runs = %v, want %v", err, domain.ErrUnavailable)
	}
	if _, ok := samples(t, n.telemetry)["cactusfs_outbox_rows{}"]; ok {
		t.Error("the gauges of delivery are exposed before the worker runs")
	}

	n.startBackground(ctx, cfg)
	status := statusWhenTaken(ctx, t, n, root, "hook")
	if status.State != ports.DeliveryOK || status.Kind != delivery.KindWebhook || status.Pending != 0 {
		t.Errorf("the status of a target nothing is sent to = %+v", status)
	}
	if _, err := n.targets.Status(ctx, root, "missing"); !errors.Is(err, domain.ErrEntityNotFound) {
		t.Errorf("Status of no target = %v, want %v", err, domain.ErrEntityNotFound)
	}
	metrics := samples(t, n.telemetry)
	for name, want := range map[string]float64{
		"cactusfs_delivery_targets{kind=webhook,state=ok}":       1,
		"cactusfs_delivery_targets{kind=nats,state=ok}":          0,
		"cactusfs_delivery_oldest_pending_seconds{kind=webhook}": 0,
		"cactusfs_outbox_rows{}":                                 0,
		"cactusfs_outbox_dropped_total{}":                        0,
		"cactusfs_egress_refusals_total{class=egress_refused}":   0,
	} {
		if got, ok := metrics[name]; !ok || got != want {
			t.Errorf("%s = %v (exposed: %v), want %v", name, got, ok, want)
		}
	}
	if got := deliveryAttemptTimeout(cfg.DeliveryTimeout); got <= cfg.DeliveryTimeout {
		t.Errorf("an attempt may take %s, no longer than the delivery timeout %s", got, cfg.DeliveryTimeout)
	}
}

// TestTargetStatusFromAnyNode: GET /targets/{name}/status answers from any
// node of a cluster - the one that leads from its worker, the others by
// asking it over node RPC - and only the node that leads exposes the gauges
// of delivery.
func TestTargetStatusFromAnyNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newTestCluster(t, fourNodes())
	c.start(c.all()...)
	stored, err := c.node("n1").meta.GetRootCredential(ctx)
	if err != nil {
		t.Fatal(err)
	}
	admin := iam.Principal{AccessKeyID: stored.AccessKeyID, Root: true}
	if _, err := c.node("n2").targets.Put(ctx, admin, hookTarget("hook")); err != nil {
		t.Fatalf("Put through n2: %v", err)
	}
	// The worker of every node runs, as the background of a node does; it
	// delivers only where its node leads.
	for _, id := range c.all() {
		workerCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
		done := make(chan struct{})
		worker := c.node(id).delivery
		go func() {
			defer close(done)
			worker.Run(workerCtx)
		}()
		t.Cleanup(func() {
			stop()
			<-done
		})
	}
	leaders := 0
	for _, id := range c.all() {
		n := c.node(id)
		status := statusWhenTaken(ctx, t, n, admin, "hook")
		if status.Name != "hook" || status.State != ports.DeliveryOK || status.Kind != delivery.KindWebhook {
			t.Errorf("the status through %s = %+v", id, status)
		}
		_, exposed := samples(t, c.telemetry[id])["cactusfs_delivery_targets{kind=webhook,state=ok}"]
		if n.meta.IsLeader() {
			leaders++
			if !exposed {
				t.Errorf("the node that leads, %s, does not expose the gauges of delivery", id)
			}
		} else if exposed {
			t.Errorf("%s, which does not lead, exposes the gauges of delivery", id)
		}
	}
	if leaders != 1 {
		t.Errorf("%d nodes lead", leaders)
	}
}
