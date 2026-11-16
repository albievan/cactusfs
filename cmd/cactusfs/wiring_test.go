package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// Every outbound port is reached through a decorator of the telemetry
// adapter (CLAUDE.md, section 5: traces and metrics on every adapter
// boundary). These tests hold the composition root to that, by looking at
// what it built rather than at how it is written: they walk a node that has
// started - the node, its use cases and everything those hold - and look at
// every field that is a port. Such a field must hold a decorator, known by
// telemetry.Instrumented, or be of a kind listed in undecorated with the
// reason why.
//
// A field is a port when its type is an interface of internal/ports, or
// when it is any interface and what it holds comes from internal/adapters:
// a use case that names its needs with an interface of its own is held to
// the same rule.
//
// What the walk does not see. It looks at fields that are interfaces, in
// this package and in internal/app, and at nothing else:
//
//   - a function handed to a use case is not a field it looks at. Today
//     three such functions are methods of the metadata adapter, taken as
//     they are in node.go, and so cross a boundary without a decorator:
//     Leader: n.meta.Leader (the cluster and the settings service), Ready:
//     n.meta.Check (the access recorder and the collector) and Active:
//     n.meta.IsLeader (the scrubber, the lifecycle worker, the usage
//     counter, the delivery worker and the publishers of targets). All
//     three answer from memory - Check after a look at the
//     state machine, under its lock - and none has a context that a span
//     could belong to but Check; what Check says is exposed as
//     cactusfs_ready. A fourth is made of one: Level: metadataLevel(n.meta)
//     (the settings and the quota services), over Store.Observe, which
//     reads as Check does and is what the gauge
//     cactusfs_metadata_schema_level shows. A function that did I/O would
//     have to be a port, with a decorator, to be seen here;
//   - what an adapter holds is the adapter's business (see ours): an
//     adapter that was handed another adapter is not found out. The egress
//     client and the dial function a publisher of a target is given are
//     such; TestThePublishersOfTargetsAreDecorated sees them by what they
//     count;
//   - a port that is held only while a request runs, or by the server and
//     not by the node, is not there when the walk comes by: shard readers
//     and the readiness checks, which are on the list below for that reason.
const (
	portsPackage    = "github.com/albievan/cactusfs/internal/ports"
	appPackage      = "github.com/albievan/cactusfs/internal/app"
	adaptersPackage = "github.com/albievan/cactusfs/internal/adapters/"
)

// undecorated are the kinds of port that are wired without a decorator, on
// purpose. A kind is named as its interface is: "ports.Clock".
var undecorated = map[string]string{
	// Not adapters: the time, and use cases and compositions of this
	// program that are themselves made of decorated ports.
	"ports.Clock":       "telling the time crosses no boundary and cannot fail",
	"ports.Authorizer":  "the authorizer is a use case (app.Authorizer); the policies it reads come through a decorated port",
	"ports.DriveSource": "composed in this package of decorated shard stores and a decorated view of the cluster; walked below",
	// Pure computation, per stripe of every object: it has no context for
	// a span to belong to, and its time is part of the request that asked.
	"ports.ErasureCodec": "computes parity in memory; no I/O, no context, called once a stripe",
	// Opened through a decorated shard store, which counts the opening;
	// reading at an offset is the body of a request and is measured there.
	"ports.ShardReader": "obtained from a decorated ShardStore.Open; reads are the body of the request",
	// The readiness checks are the gauge cactusfs_ready.
	"ports.ReadinessCheck": "what the checks say is exposed as cactusfs_ready",
}

// wiring walks what a node holds.
type wiring struct {
	seen map[uintptr]map[reflect.Type]bool
	// decorated and allowed count the ports found, by kind; violations are
	// the ports that are neither.
	decorated, allowed map[string]int
	violations         []string
}

func walkWiring(root any) *wiring {
	w := &wiring{seen: map[uintptr]map[reflect.Type]bool{}, decorated: map[string]int{}, allowed: map[string]int{}}
	w.value("node", reflect.ValueOf(root))
	sort.Strings(w.violations)
	return w
}

// ours reports whether a type is this program's composition root or one of
// its use cases: the walk goes into those and into nothing else. What an
// adapter holds is the adapter's business.
func ours(t reflect.Type) bool {
	pkg := t.PkgPath()
	return pkg == appPackage || pkg == reflect.TypeFor[node]().PkgPath()
}

func kindOf(t reflect.Type) string {
	return t.String()
}

// field looks at one field, element or entry whose declared type is static.
func (w *wiring) field(path string, static reflect.Type, v reflect.Value) {
	if static.Kind() != reflect.Interface {
		w.value(path, v)
		return
	}
	if v.IsNil() {
		return
	}
	held := v.Elem().Type()
	base := held
	for base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	isPort := static.PkgPath() == portsPackage || strings.HasPrefix(base.PkgPath(), adaptersPackage)
	switch {
	case !isPort:
	case held.Implements(reflect.TypeFor[telemetry.Instrumented]()):
		w.decorated[kindOf(static)]++
		// Behind the decorator is the adapter itself.
		return
	case undecorated[kindOf(static)] != "":
		w.allowed[kindOf(static)]++
	default:
		w.violations = append(w.violations, fmt.Sprintf("%s: a %s is wired as a %s, without a decorator of the telemetry adapter", path, kindOf(static), held))
	}
	w.value(path, v.Elem())
}

func (w *wiring) value(path string, v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() || !ours(v.Type().Elem()) {
			return
		}
		types := w.seen[v.Pointer()]
		if types == nil {
			types = map[reflect.Type]bool{}
			w.seen[v.Pointer()] = types
		}
		if types[v.Type()] {
			return
		}
		types[v.Type()] = true
		w.value(path, v.Elem())
	case reflect.Struct:
		if !ours(v.Type()) {
			return
		}
		for i := range v.NumField() {
			w.field(path+"."+v.Type().Field(i).Name, v.Type().Field(i).Type, v.Field(i))
		}
	case reflect.Interface:
		if !v.IsNil() {
			w.value(path, v.Elem())
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			w.field(fmt.Sprintf("%s[%d]", path, i), v.Type().Elem(), v.Index(i))
		}
	case reflect.Map:
		for entries := v.MapRange(); entries.Next(); {
			w.field(path+"[]", v.Type().Elem(), entries.Value())
		}
	default:
	}
}

// wantWired fails for every port of a node that is wired without its
// decorator, and when the walk did not find the ports it is known to hold -
// which would mean that it no longer looks where they are.
func wantWired(t *testing.T, n *node, kinds ...string) {
	t.Helper()
	w := walkWiring(n)
	for _, violation := range w.violations {
		t.Error(violation)
	}
	for _, kind := range kinds {
		if w.decorated[kind] == 0 {
			t.Errorf("the walk found no decorated %s: it does not reach where the node keeps one", kind)
		}
	}
	for kind := range w.allowed {
		if w.decorated[kind] > 0 {
			t.Errorf("%s is listed as wired without a decorator and is decorated in %d places: take it off the list", kind, w.decorated[kind])
		}
	}
}

// The kinds of port every node holds.
var wiredKinds = []string{
	"ports.BucketStore", "ports.ObjectIndex", "ports.UploadStore", "ports.RootCredentialStore", "ports.BucketPolicies",
	"ports.BucketActivityStore", "ports.ObjectAccessStore", "ports.ShardStore", "ports.KMS", "ports.IAMStore",
	"ports.AuditLog", "ports.SessionStore", "ports.IdempotencyStore", "ports.PasswordHasher", "ports.IdentityProvider",
	"ports.EventPublisher", "ports.ClusterView", "app.BucketPolicySource", "ports.TargetStore", "ports.SubBucketLister", "ports.SubBucketStore",
	"ports.ReleaseVerifier", "ports.Upgrader", "ports.BucketUsageStore", "ports.Outbox", "ports.DeliveryStatusReader",
	"ports.ReplicationStore",
}

// TestEveryPortIsWiredThroughItsDecorator: a node on its own, started as
// the binary starts it, holds no port that is not decorated.
func TestEveryPortIsWiredThroughItsDecorator(t *testing.T) {
	t.Parallel()
	started := mustStart(t, nodeConfig(t.TempDir()))
	wantWired(t, started.node, wiredKinds...)

	// The list of exceptions holds nothing that is not met.
	w := walkWiring(started.node)
	for kind := range undecorated {
		if kind == "ports.ShardReader" || kind == "ports.ReadinessCheck" {
			// Held only while a request reads; held by the server, not
			// by the node.
			continue
		}
		if w.allowed[kind] == 0 {
			t.Errorf("%s is listed as wired without a decorator, and the node holds none: take it off the list", kind)
		}
	}
}

// TestThePublishersOfTargetsAreDecorated: the publishers a node makes for
// its targets are decorated - which the walk sees once they are made - and
// they reach the network through an egress client observed by the
// telemetry and a dial function behind its decorator, which the walk cannot
// see: a webhook, a NATS server and another deployment (the client of
// replication, PLAN 8.1.4) at an address the node may not connect to are
// each a failed connection counted in cactusfs_egress_refusals_total and a
// span of the egress boundary; the client of another deployment is
// decorated too (span site.Check).
func TestThePublishersOfTargetsAreDecorated(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	recorder := tracetest.NewSpanRecorder()
	tel, err := telemetry.New(ctx, telemetry.Config{Version: buildinfo.Get().Tag(), SpanProcessor: recorder})
	if err != nil {
		t.Fatal(err)
	}
	var notice bytes.Buffer
	// No --egress-allow: loopback is an address the node does not
	// connect to.
	n, err := openNode(ctx, nodeConfig(t.TempDir()), tel, slog.New(slog.DiscardHandler), &notice)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })
	root := iam.Principal{AccessKeyID: "ROOT", Root: true}
	port := closedPort(t)
	for _, in := range []app.TargetInput{
		{Name: "hook", Kind: delivery.KindWebhook, Endpoint: "https://127.0.0.1:" + port + "/hooks/in", Buckets: []string{"photos"}},
		{Name: "stream", Kind: delivery.KindNATS, Endpoint: "tls://127.0.0.1:" + port, Subject: "cactusfs.events", Buckets: []string{"photos"}},
	} {
		if _, err := n.targets.Put(ctx, root, in); err != nil {
			t.Fatalf("Put %s: %v", in.Name, err)
		}
		target, err := n.meta.GetTarget(ctx, in.Name)
		if err != nil {
			t.Fatal(err)
		}
		publisher, err := n.publishers.Publisher(ctx, target)
		if err != nil {
			t.Fatalf("the publisher of %s: %v", in.Name, err)
		}
		if _, ok := publisher.(telemetry.Instrumented); !ok {
			t.Errorf("the publisher of %s is a %T, without a decorator of the telemetry adapter", in.Name, publisher)
		}
		if err := publisher.Publish(ctx, testMessage()); delivery.ClassOf(err) != delivery.ClassEgressRefused {
			t.Errorf("a message to %s at loopback = %v, want a failure of class %s", in.Name, err, delivery.ClassEgressRefused)
		}
	}
	// And the client of another deployment (PLAN 8.1.4), through the same
	// egress client: its probe of a bucket at loopback is refused.
	if _, err := n.targets.Put(ctx, root, app.TargetInput{
		Name: "site", Kind: delivery.KindS3, Endpoint: "https://127.0.0.1:" + port, Region: "eu-1", Buckets: []string{"photos"},
		Auth: delivery.Auth{Mode: delivery.AuthSigV4, ID: "AKIDSITEBEXAMPLE0001"}, Secret: "not-a-secret-of-anybody",
	}); err != nil {
		t.Fatalf("Put site: %v", err)
	}
	target, err := n.meta.GetTarget(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	site, err := n.publishers.Site(ctx, target)
	if err != nil {
		t.Fatalf("the client of site: %v", err)
	}
	if _, ok := site.(telemetry.Instrumented); !ok {
		t.Errorf("the client of site is a %T, without a decorator of the telemetry adapter", site)
	}
	if _, err := site.Check(ctx, "photos"); delivery.ClassOf(err) != delivery.ClassEgressRefused {
		t.Errorf("a probe of site at loopback = %v, want a failure of class %s", err, delivery.ClassEgressRefused)
	}
	if got := samples(t, tel)["cactusfs_egress_refusals_total{class=egress_refused}"]; got != 3 {
		t.Errorf("cactusfs_egress_refusals_total{class=egress_refused} = %v, want 3: the egress client or its dial function is not decorated", got)
	}
	spans := map[string]int{}
	for _, span := range recorder.Ended() {
		spans[span.Name()]++
	}
	if spans["egress.Do"] != 2 || spans["egress.Dial"] != 1 || spans["events.Publish"] < 2 || spans["site.Check"] != 1 {
		t.Errorf("spans of the egress boundary, of the publishers and of the client of another deployment: %v", spans)
	}
	w := walkWiring(n)
	for _, violation := range w.violations {
		t.Error(violation)
	}
	// The bus, and the two publishers of targets.
	if w.decorated["ports.EventPublisher"] < 3 {
		t.Errorf("the walk found %d decorated publishers, want the bus and the two of the targets", w.decorated["ports.EventPublisher"])
	}
}

// TestTheWiringTestSeesAnUndecoratedPort holds the test above to account: a
// node whose use cases were handed stores as they are is found out, port by
// port, and a decorated one is not.
func TestTheWiringTestSeesAnUndecoratedPort(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t)
	meta, identities := porttest.NewMemMetadata(), porttest.NewMemIAM()
	clock := systemClock{}
	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}

	bare := &node{
		clock:       clock,
		publisher:   discardEvents{},
		buckets:     app.NewBucketService(meta, nil, clock),
		idempotency: app.NewIdempotencyService(identities, kms, clock, porttest.Entropy()),
		iam:         app.NewIAMService(app.IAMServiceConfig{Store: identities, Trail: identities, KMS: kms, Clock: clock}),
	}
	got := walkWiring(bare).violations
	for _, want := range []string{
		"node.publisher: a ports.EventPublisher is wired as a main.discardEvents",
		"node.buckets.buckets: a ports.BucketStore is wired as a *porttest.MemMetadata",
		"node.idempotency.store: a ports.IdempotencyStore is wired as a *porttest.MemIAM",
		"node.idempotency.kms: a ports.KMS is wired as a *porttest.KMS",
		"node.iam.store: a ports.IAMStore is wired as a *porttest.MemIAM",
		"node.iam.trail: a ports.AuditLog is wired as a *porttest.MemIAM",
	} {
		if !slices.ContainsFunc(got, func(violation string) bool { return strings.HasPrefix(violation, want) }) {
			t.Errorf("the walk did not report %q; it reported:\n%s", want, strings.Join(got, "\n"))
		}
	}
	// The clock is on the list of exceptions and is no violation.
	for _, violation := range got {
		if strings.Contains(violation, "ports.Clock") {
			t.Errorf("an exception is reported: %s", violation)
		}
	}

	wired := &node{
		clock:       clock,
		publisher:   tel.InstrumentEventPublisher(discardEvents{}),
		buckets:     app.NewBucketService(tel.InstrumentMetadata(meta), nil, clock),
		idempotency: app.NewIdempotencyService(tel.InstrumentIdempotency(identities), tel.InstrumentKMS(kms), clock, porttest.Entropy()),
		iam: app.NewIAMService(app.IAMServiceConfig{
			Store: tel.InstrumentIAM(identities), Trail: tel.InstrumentAuditLog(identities), KMS: tel.InstrumentKMS(kms), Clock: clock,
		}),
	}
	if w := walkWiring(wired); len(w.violations) != 0 || w.decorated["ports.IAMStore"] != 1 || w.allowed["ports.Clock"] == 0 {
		t.Errorf("a node whose ports are decorated: violations %v, decorated %v, allowed %v", w.violations, w.decorated, w.allowed)
	}
}

// discardEvents is a publisher that delivers to nobody.
type discardEvents struct{}

// Publish implements ports.EventPublisher.
func (discardEvents) Publish(context.Context, ports.Event) error { return nil }

// TestThePreviewCapIsWired: the cap of previews a node is given is the one
// its object service applies (ADR-0028), and a configuration that names
// none - one a test built by hand - has the default.
func TestThePreviewCapIsWired(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		mib  int
		want int64
	}{
		{mib: 5, want: 5 << 20},
		{mib: 0, want: app.DefaultPreviewMaxSize},
	} {
		cfg := nodeConfig(t.TempDir())
		cfg.PreviewMaxSizeMiB = tt.mib
		if got := mustStart(t, cfg).node.objects.PreviewMaxSize(); got != tt.want {
			t.Errorf("preview-max-size %d: the object service previews up to %d bytes, want %d", tt.mib, got, tt.want)
		}
	}
}
