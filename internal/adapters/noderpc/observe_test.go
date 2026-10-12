package noderpc_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/albievan/cactusfs/internal/adapters/noderpc"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// observedNode is the telemetry of one node of a test: its metrics and the
// spans it recorded. A nil recorder is a node that exports no traces.
type observedNode struct {
	tel   *telemetry.Telemetry
	spans *tracetest.SpanRecorder
}

func observe(t *testing.T, tracing bool) observedNode {
	t.Helper()
	node := observedNode{}
	cfg := telemetry.Config{Version: "test"}
	if tracing {
		node.spans = tracetest.NewSpanRecorder()
		cfg.SpanProcessor = node.spans
	}
	tel, err := telemetry.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	node.tel = tel
	return node
}

// series returns the metrics of a node, one series a line, as
// "name label=value ...", with the count of a counter or a histogram.
func (n observedNode) series(t *testing.T) []string {
	t.Helper()
	families, err := n.tel.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			line := family.GetName()
			for _, label := range metric.GetLabel() {
				line += " " + label.GetName() + "=" + label.GetValue()
			}
			if counter := metric.GetCounter(); counter != nil && counter.GetValue() == 0 {
				continue
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// eventually waits until the node has a series that begins with want: the
// end of a stream is noticed a moment after the call that ended it.
func (n observedNode) eventually(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		lines := n.series(t)
		if slices.ContainsFunc(lines, func(line string) bool { return strings.HasPrefix(line, want) }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no series %q among:\n%s", want, strings.Join(lines, "\n"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// named returns the ended spans of a node with a name, waiting until there
// are count of them.
func (n observedNode) named(t *testing.T, name string, count int) []sdktrace.ReadOnlySpan {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var found []sdktrace.ReadOnlySpan
		for _, span := range n.spans.Ended() {
			if span.Name() == name {
				found = append(found, span)
			}
		}
		if len(found) == count {
			return found
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d spans named %q, want %d", len(found), name, count)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// everything is all a node recorded that could carry a name: the labels of
// its metrics and, of every span, the name, the attributes, the description
// of the status and the events.
func (n observedNode) everything(t *testing.T) []string {
	t.Helper()
	all := n.series(t)
	if n.spans == nil {
		return all
	}
	for _, span := range n.spans.Ended() {
		all = append(all, span.Name(), span.Status().Description)
		for _, attr := range span.Attributes() {
			all = append(all, string(attr.Key), attr.Value.String())
		}
		for _, event := range span.Events() {
			all = append(all, event.Name)
			for _, attr := range event.Attributes {
				all = append(all, string(attr.Key), attr.Value.String())
			}
		}
	}
	return all
}

// observedPair is a storage node and a gateway that reaches a drive of it
// over mutual TLS, each with its own telemetry wired as the composition
// root wires it. The names of the two nodes and of the drive are marked, so
// that a test can look for them where they must not be.
func observedPair(t *testing.T, tracing bool) (gateway, storage observedNode, drive ports.ShardStore, local *porttest.MemShardStore) {
	t.Helper()
	p := newPKI(t)
	gateway, storage = observe(t, tracing), observe(t, tracing)
	local = porttest.NewMemShardStore("drive-sentinel")
	served := storage.tel.NodeRPC(noderpc.Methods())
	node := serveWith(t, p, "storage-sentinel", func(cfg *noderpc.ServerConfig) {
		cfg.UnaryInterceptor, cfg.StreamInterceptor = served.UnaryServer(), served.StreamServer()
		cfg.OnHandshakeRefused = storage.tel.ObserveHandshakeRefused
		// The drive is decorated on the node that has it, as on a real one.
		decorated := storage.tel.InstrumentShardStore(local)
		cfg.Drive = func(id string) (ports.ShardStore, bool) { return decorated, id == local.DriveID() }
	})
	calling := gateway.tel.NodeRPC(noderpc.Methods())
	client := clientFor(t, p.identity("gateway-sentinel"), map[string]string{"storage-sentinel": node.address}).
		WithInterceptors(calling.UnaryClient(), calling.StreamClient())
	return gateway, storage, gateway.tel.InstrumentShardStore(client.Drive("storage-sentinel", local.DriveID())), local
}

// inRequest runs fn inside the span of an S3 request of the gateway, as a
// handler of the S3 API runs.
func inRequest(t *testing.T, gateway observedNode, fn func(r *http.Request)) {
	t.Helper()
	handler := gateway.tel.TraceS3(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { fn(r) }))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/bucket-sentinel/key-sentinel", nil))
}

// TestAWriteThroughTwoNodesIsOneTrace: a shard written by a gateway on a
// drive of another node is one trace. The span of the storage node is a
// child of the gateway's span of the call, and what the storage node did
// with its drive is a child of that. Both nodes count the call. Nothing
// either of them recorded names a node, a drive, a bucket or a key.
func TestAWriteThroughTwoNodesIsOneTrace(t *testing.T) {
	t.Parallel()
	gateway, storage, drive, local := observedPair(t, true)

	data := []byte("the content of an object, which is nobody's business: content-sentinel")
	inRequest(t, gateway, func(r *http.Request) {
		ctx := r.Context()
		writer, err := drive.Create(ctx, shard(7, 0))
		if err != nil {
			t.Errorf("Create: %v", err)
			return
		}
		if _, err := writer.Write(data); err != nil {
			t.Errorf("Write: %v", err)
		}
		if err := writer.Commit(ctx); err != nil {
			t.Errorf("Commit: %v", err)
		}
		reader, err := drive.Open(ctx, shard(7, 0))
		if err != nil {
			t.Errorf("Open: %v", err)
			return
		}
		got := make([]byte, len(data))
		if _, err := reader.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("ReadAt: %v", err)
		}
		if string(got) != string(data) {
			t.Error("the shard did not come back as it was written")
		}
		// A shard that is not there: an error of the drive, told in the
		// answer of a call that succeeded.
		if _, err := drive.Open(ctx, shard(8, 0)); !errors.Is(err, domain.ErrShardNotFound) {
			t.Errorf("Open of a missing shard = %v", err)
		}
	})
	if stored, ok := local.Bytes(shard(7, 0)); !ok || string(stored) != string(data) {
		t.Fatal("the shard is not on the drive of the storage node")
	}

	// One trace: the request of the gateway.
	request := gateway.named(t, "s3.request", 1)[0]
	trace := request.SpanContext().TraceID()
	sent := gateway.named(t, "cactusfs.noderpc/WriteShard", 1)[0]
	served := storage.named(t, "cactusfs.noderpc/WriteShard", 1)[0]
	if sent.SpanContext().TraceID() != trace || served.SpanContext().TraceID() != trace {
		t.Errorf("the request is the trace %s, the gateway's call %s, the storage node's %s",
			trace, sent.SpanContext().TraceID(), served.SpanContext().TraceID())
	}
	if served.Parent().SpanID() != sent.SpanContext().SpanID() || !served.Parent().IsRemote() {
		t.Errorf("the parent of the storage node's span is %s, want the gateway's call %s", served.Parent().SpanID(), sent.SpanContext().SpanID())
	}
	// On the gateway: request > shards.Create > the call.
	created := gateway.named(t, "shards.Create", 1)[0]
	if created.Parent().SpanID() != request.SpanContext().SpanID() || sent.Parent().SpanID() != created.SpanContext().SpanID() {
		t.Errorf("on the gateway the call is not a child of shards.Create, or that not of the request")
	}
	// On the storage node: the call > shards.Create and shards.Commit.
	for _, name := range []string{"shards.Create", "shards.Commit"} {
		local := storage.named(t, name, 1)[0]
		if local.Parent().SpanID() != served.SpanContext().SpanID() || local.SpanContext().TraceID() != trace {
			t.Errorf("%s on the storage node is not a child of the call it served", name)
		}
	}
	// Every span either node recorded belongs to that one trace, and every
	// call the storage node served has the gateway's span of it as parent.
	calls := map[string]bool{}
	for _, span := range gateway.spans.Ended() {
		if span.SpanContext().TraceID() != trace {
			t.Errorf("the gateway's span %q is in another trace", span.Name())
		}
		calls[span.SpanContext().SpanID().String()] = strings.HasPrefix(span.Name(), "cactusfs.noderpc/")
	}
	rpcs := 0
	for _, span := range storage.spans.Ended() {
		if span.SpanContext().TraceID() != trace {
			t.Errorf("the storage node's span %q is in another trace", span.Name())
		}
		if strings.HasPrefix(span.Name(), "cactusfs.noderpc/") {
			rpcs++
			if !calls[span.Parent().SpanID().String()] {
				t.Errorf("the storage node's span %q is not a child of a call of the gateway", span.Name())
			}
		}
	}
	// WriteShard, then StatShard and ReadShard for the shard that is there,
	// and StatShard for the one that is not.
	if rpcs < 4 {
		t.Errorf("the storage node recorded %d calls, want at least 4", rpcs)
	}

	for _, method := range []string{"WriteShard", "StatShard", "ReadShard"} {
		gateway.eventually(t, "cactusfs_noderpc_requests_total code=OK method="+method+" side=client")
		storage.eventually(t, "cactusfs_noderpc_requests_total code=OK method="+method+" side=server")
		gateway.eventually(t, "cactusfs_noderpc_request_duration_seconds method="+method+" side=client")
		storage.eventually(t, "cactusfs_noderpc_request_duration_seconds method="+method+" side=server")
	}
	// Neither node counts as the other.
	for _, line := range gateway.series(t) {
		if strings.HasPrefix(line, "cactusfs_noderpc_") && strings.Contains(line, "side=server") {
			t.Errorf("the gateway counts a call it did not serve: %s", line)
		}
	}

	for _, node := range []observedNode{gateway, storage} {
		all := node.everything(t)
		if len(all) < 10 {
			t.Fatalf("a node recorded %d things; too few to have checked anything", len(all))
		}
		for _, text := range all {
			if strings.Contains(text, "sentinel") {
				t.Errorf("%q is recorded: a node, a drive, a bucket, a key or content is named", text)
			}
		}
	}
}

// TestCallsAreCountedWithoutACollector: with tracing off the same calls are
// counted and timed on both sides, and the shard arrives all the same.
func TestCallsAreCountedWithoutACollector(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	gateway, storage, drive, local := observedPair(t, false)

	writer, err := drive.Create(ctx, shard(3, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := local.Bytes(shard(3, 1)); !ok {
		t.Fatal("the shard did not arrive")
	}
	// A listing is a stream of the other direction.
	listed := 0
	if err := drive.List(ctx, func(ports.ShardInfo) error { listed++; return nil }); err != nil || listed != 1 {
		t.Fatalf("List: %d shards, %v", listed, err)
	}
	if err := drive.Delete(ctx, shard(3, 1)); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"WriteShard", "ListShards", "DeleteShard"} {
		gateway.eventually(t, "cactusfs_noderpc_requests_total code=OK method="+method+" side=client")
		storage.eventually(t, "cactusfs_noderpc_requests_total code=OK method="+method+" side=server")
	}

	// A writer that is given up: the node that served the stream says so.
	// The gateway ended it itself, having met no error, and counts no
	// failure of its own.
	abandoned, err := drive.Create(ctx, shard(4, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := abandoned.Abort(); err != nil {
		t.Fatal(err)
	}
	storage.eventually(t, "cactusfs_noderpc_requests_total code=Canceled method=WriteShard side=server")
	for _, line := range gateway.series(t) {
		if strings.HasPrefix(line, "cactusfs_noderpc_requests_total") && !strings.Contains(line, "code=OK") {
			t.Errorf("the gateway counts a failed call: %s", line)
		}
	}
}

// TestRefusedHandshakesAreCounted: a caller whose certificate is not of the
// cluster's node CA gets no connection, and the node counts the refusal.
func TestRefusedHandshakesAreCounted(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	p, foreign := newPKI(t), newPKI(t)
	storage := observe(t, false)
	node := serveWith(t, p, "storage-1", func(cfg *noderpc.ServerConfig) {
		cfg.OnHandshakeRefused = storage.tel.ObserveHandshakeRefused
	}, porttest.NewMemShardStore("d1"))
	addresses := map[string]string{"storage-1": node.address}

	// A member first: nothing is refused.
	if _, err := clientFor(t, p.identity("gateway-1"), addresses).Info(ctx, "storage-1", node.address); err != nil {
		t.Fatalf("Info: %v", err)
	}
	for _, line := range storage.series(t) {
		if strings.HasPrefix(line, "cactusfs_node_handshakes_refused_total") {
			t.Fatalf("a handshake is counted as refused though none was: %s", line)
		}
	}
	stranger := clientFor(t, foreign.identity("gateway-1"), addresses)
	if _, err := stranger.Info(ctx, "storage-1", node.address); !errors.Is(err, domain.ErrUnavailable) {
		t.Fatalf("Info with a certificate of another CA = %v", err)
	}
	storage.eventually(t, "cactusfs_node_handshakes_refused_total reason=certificate")
}
