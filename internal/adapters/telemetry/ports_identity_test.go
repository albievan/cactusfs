package telemetry_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// instrumentedIAM is the three stores of identities as the composition root
// hands them out: each through its own decorator.
type instrumentedIAM struct {
	ports.IAMStore
	ports.AuditLog
	ports.SessionStore
}

// methodsOf returns the names of the methods of a port.
func methodsOf[T any]() []string {
	port := reflect.TypeFor[T]()
	names := make([]string, port.NumMethod())
	for i := range names {
		names[i] = port.Method(i).Name
	}
	return names
}

// wantEveryMethod fails for every method of a port that the metrics do not
// count under the port's name: the method label is the name of the Go
// method, and no method goes around the decorator.
func wantEveryMethod(t *testing.T, metrics, port string, methods []string) {
	t.Helper()
	if len(methods) == 0 {
		t.Fatalf("port %s has no method", port)
	}
	for _, method := range methods {
		if !strings.Contains(metrics, "cactusfs_port_calls_total method="+method+" port="+port+" result=") {
			t.Errorf("port %s: no call of %s is counted", port, method)
		}
		if !strings.Contains(metrics, "cactusfs_port_call_duration_seconds method="+method+" port="+port+" count=") {
			t.Errorf("port %s: no call of %s is timed", port, method)
		}
	}
}

// TestIdentityStoresAreCountedAndTraced runs the contracts of the stores of
// identities, sessions, the audit trail and repeated requests through their
// decorators: instrumenting them changes nothing of what they do, every one
// of their methods is counted under its own name, and nothing the contracts
// name - users, keys, tokens - is in a metric or a span.
func TestIdentityStoresAreCountedAndTraced(t *testing.T) {
	t.Parallel()
	recorder := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, recorder)

	// The contracts run their cases side by side; the group is done when
	// they all are.
	t.Run("contracts", func(t *testing.T) {
		porttest.IAMContract(t, func(*testing.T) porttest.IAM {
			mem := porttest.NewMemIAM()
			return instrumentedIAM{
				IAMStore: tel.InstrumentIAM(mem), AuditLog: tel.InstrumentAuditLog(mem), SessionStore: tel.InstrumentSessions(mem),
			}
		})
		porttest.IdempotencyContract(t, func(*testing.T) ports.IdempotencyStore {
			return tel.InstrumentIdempotency(porttest.NewMemIAM())
		})
	})

	metrics := scrape(t, tel)
	wantEveryMethod(t, metrics, "iam", methodsOf[ports.IAMStore]())
	wantEveryMethod(t, metrics, "audit", methodsOf[ports.AuditLog]())
	wantEveryMethod(t, metrics, "sessions", methodsOf[ports.SessionStore]())
	wantEveryMethod(t, metrics, "idempotency", methodsOf[ports.IdempotencyStore]())
	// A user that does not exist is an error of the port, counted as every
	// other port counts what it returns.
	for _, want := range []string{
		"cactusfs_port_calls_total method=GetUser port=iam result=ok",
		"cactusfs_port_calls_total method=GetUser port=iam result=error",
		"cactusfs_port_calls_total method=GetSession port=sessions result=error",
		"cactusfs_port_calls_total method=GetIdempotent port=idempotency result=error",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	for _, want := range []string{"iam.PutUser", "audit.AppendAudit", "sessions.PutSession", "idempotency.BeginIdempotent"} {
		if !contains(spanNames(recorder), want) {
			t.Errorf("no span named %q", want)
		}
	}
	// What the contracts name: a user, a policy, a group.
	wantNothingOf(t, tel, recorder, "alice", "p-read", "viewers", "team")
}

// fixedView is a view of a cluster of one node, or of none.
type fixedView struct{ err error }

func (v fixedView) Topology(context.Context) (cluster.Topology, error) {
	return cluster.Topology{DeploymentID: "dep-sentinel"}, v.err
}

func (v fixedView) Nodes(context.Context) ([]cluster.Node, error) {
	return []cluster.Node{{ID: "node-sentinel"}}, v.err
}

// fixedProvider accepts what one user presents, and nothing else.
type fixedProvider struct{ user, presents string }

func (p fixedProvider) Authenticate(_ context.Context, username, password string) (string, error) {
	if username == p.user && password == p.presents {
		return username, nil
	}
	return "", domain.ErrAccessDenied
}

// fixedHasher hashes by a rule a test can read.
type fixedHasher struct{}

func (fixedHasher) Hash(password string) (string, error) {
	if len(password) == 0 {
		return "", domain.ErrInvalidArgument
	}
	return "hash-of-" + password, nil
}

func (fixedHasher) Verify(encoded, password string) (bool, error) {
	if !strings.HasPrefix(encoded, "hash-of-") {
		return false, domain.ErrUnsupportedFormat
	}
	return encoded == "hash-of-"+password, nil
}

// collecting keeps what was published; refusing delivers nothing.
type collecting struct{ events []ports.Event }

func (c *collecting) Publish(_ context.Context, e ports.Event) error {
	c.events = append(c.events, e)
	return nil
}

type refusing struct{}

func (refusing) Publish(context.Context, ports.Event) error { return domain.ErrUnavailable }

// TestRemainingPortsAreCountedAndTraced: the event publisher, the identity
// provider, the password hasher and the view of the cluster are reached
// through decorators that pass everything through, count a call by the name
// of its method and an error as an error, and record nothing of what was
// passed.
func TestRemainingPortsAreCountedAndTraced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	recorder := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, recorder)

	sink := &collecting{}
	event := ports.Event{
		ID: "event-sentinel", Type: ports.EventObjectCreated, Time: time.Unix(1, 0),
		Data: ports.ObjectEvent{Bucket: "bucket-sentinel", Key: "key-sentinel"},
	}
	if err := tel.InstrumentEventPublisher(sink).Publish(ctx, event); err != nil || len(sink.events) != 1 || sink.events[0].ID != "event-sentinel" {
		t.Fatalf("Publish: %v, delivered %+v", err, sink.events)
	}
	if err := tel.InstrumentEventPublisher(refusing{}).Publish(ctx, event); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("error not passed through: %v", err)
	}

	provider := tel.InstrumentIdentityProvider(fixedProvider{user: "user-sentinel", presents: "password-sentinel"})
	if name, err := provider.Authenticate(ctx, "user-sentinel", "password-sentinel"); err != nil || name != "user-sentinel" {
		t.Fatalf("Authenticate = %q, %v", name, err)
	}
	if _, err := provider.Authenticate(ctx, "user-sentinel", "wrong-sentinel"); !errors.Is(err, domain.ErrAccessDenied) {
		t.Fatalf("error not passed through: %v", err)
	}

	hasher := tel.InstrumentPasswords(fixedHasher{})
	encoded, err := hasher.Hash("password-sentinel")
	if err != nil || encoded != "hash-of-password-sentinel" {
		t.Fatalf("Hash = %q, %v", encoded, err)
	}
	if _, err := hasher.Hash(""); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("error not passed through: %v", err)
	}
	if ok, err := hasher.Verify(encoded, "password-sentinel"); err != nil || !ok {
		t.Fatalf("Verify = %v, %v", ok, err)
	}
	// A wrong password is an answer; a hash that cannot be read, an error.
	if ok, err := hasher.Verify(encoded, "wrong-sentinel"); err != nil || ok {
		t.Fatalf("Verify of a wrong password = %v, %v", ok, err)
	}
	if _, err := hasher.Verify("garbage", "password-sentinel"); !errors.Is(err, domain.ErrUnsupportedFormat) {
		t.Fatalf("error not passed through: %v", err)
	}

	mem := porttest.NewMemMetadata()
	if err := mem.CreateBucket(ctx, bucket.Bucket{Name: "bucket-sentinel"}); err != nil {
		t.Fatal(err)
	}
	if err := mem.SetBucketConfig(ctx, "bucket-sentinel", "cors", []byte("rules-sentinel")); err != nil {
		t.Fatal(err)
	}
	local := tel.InstrumentLocalConfig(mem)
	if document, err := local.GetBucketConfig(ctx, "bucket-sentinel", "cors"); err != nil || string(document) != "rules-sentinel" {
		t.Fatalf("GetBucketConfig = %q, %v", document, err)
	}
	if _, err := local.GetBucketConfig(ctx, "absent-sentinel", "cors"); !errors.Is(err, domain.ErrBucketNotFound) {
		t.Fatalf("error not passed through: %v", err)
	}

	view := tel.InstrumentClusterView(fixedView{})
	if topology, err := view.Topology(ctx); err != nil || topology.DeploymentID != "dep-sentinel" {
		t.Fatalf("Topology = %+v, %v", topology, err)
	}
	if nodes, err := view.Nodes(ctx); err != nil || len(nodes) != 1 {
		t.Fatalf("Nodes = %+v, %v", nodes, err)
	}
	missing := tel.InstrumentClusterView(fixedView{err: domain.ErrNoTopology})
	if _, err := missing.Topology(ctx); !errors.Is(err, domain.ErrNoTopology) {
		t.Fatalf("error not passed through: %v", err)
	}
	if _, err := missing.Nodes(ctx); !errors.Is(err, domain.ErrNoTopology) {
		t.Fatalf("error not passed through: %v", err)
	}

	metrics := scrape(t, tel)
	wantEveryMethod(t, metrics, "events", methodsOf[ports.EventPublisher]())
	wantEveryMethod(t, metrics, "identity_provider", methodsOf[ports.IdentityProvider]())
	wantEveryMethod(t, metrics, "passwords", methodsOf[ports.PasswordHasher]())
	wantEveryMethod(t, metrics, "cluster_view", methodsOf[ports.ClusterView]())
	for _, want := range []string{
		"cactusfs_port_calls_total method=Publish port=events result=ok",
		"cactusfs_port_calls_total method=Publish port=events result=error",
		"cactusfs_port_calls_total method=Authenticate port=identity_provider result=ok",
		"cactusfs_port_calls_total method=Authenticate port=identity_provider result=error",
		"cactusfs_port_calls_total method=Hash port=passwords result=error",
		"cactusfs_port_calls_total method=Verify port=passwords result=ok",
		"cactusfs_port_calls_total method=Verify port=passwords result=error",
		"cactusfs_port_calls_total method=Topology port=cluster_view result=error",
		"cactusfs_port_calls_total method=GetBucketConfig port=local_config result=ok",
		"cactusfs_port_calls_total method=GetBucketConfig port=local_config result=error",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	names := spanNames(recorder)
	for _, want := range []string{"events.Publish", "identity_provider.Authenticate", "cluster_view.Topology", "cluster_view.Nodes", "local_config.GetBucketConfig"} {
		if !contains(names, want) {
			t.Errorf("no span named %q among %v", want, names)
		}
	}
	// The hasher takes no context: its calls are counted and are no spans.
	for _, name := range names {
		if strings.HasPrefix(name, "passwords.") {
			t.Errorf("a call of the password hasher is the span %q, which belongs to no trace", name)
		}
	}
	wantNothingOf(t, tel, recorder, "sentinel", "hash-of-", "garbage")
}

// TestEveryDecoratorIsMarked: the composition root's test knows a decorated
// port by telemetry.Instrumented; every decorator must be one, the writer of
// a decorated shard store included.
func TestEveryDecoratorIsMarked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tel := newTelemetry(t, nil)
	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}
	mem, identities := porttest.NewMemMetadata(), porttest.NewMemIAM()
	shards := tel.InstrumentShardStore(porttest.NewMemShardStore("drive"))
	writer, err := shards.Create(ctx, object.ShardID{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Abort() }()
	decorated := map[string]any{
		"KMS": tel.InstrumentKMS(kms), "ShardStore": shards, "ShardWriter": writer, "Metadata": tel.InstrumentMetadata(mem),
		"BucketPolicies": tel.InstrumentBucketPolicies(mem), "ObjectAccess": tel.InstrumentObjectAccess(mem),
		"BucketActivity": tel.InstrumentBucketActivity(mem), "IAM": tel.InstrumentIAM(identities),
		"Sessions": tel.InstrumentSessions(identities), "Idempotency": tel.InstrumentIdempotency(identities),
		"AuditLog": tel.InstrumentAuditLog(identities), "EventPublisher": tel.InstrumentEventPublisher(refusing{}),
		"IdentityProvider": tel.InstrumentIdentityProvider(fixedProvider{}), "Passwords": tel.InstrumentPasswords(fixedHasher{}),
		"ClusterView": tel.InstrumentClusterView(fixedView{}), "LocalConfig": tel.InstrumentLocalConfig(mem),
	}
	for name, value := range decorated {
		if _, ok := value.(telemetry.Instrumented); !ok {
			t.Errorf("the decorator of %s is not telemetry.Instrumented", name)
		}
	}
	// What is not decorated is not marked.
	for name, value := range map[string]any{"KMS": kms, "Metadata": mem, "IAM": identities} {
		if _, ok := value.(telemetry.Instrumented); ok {
			t.Errorf("an undecorated %s is telemetry.Instrumented", name)
		}
	}
}
