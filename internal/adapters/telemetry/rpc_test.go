package telemetry_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/ports"
)

// The methods a test node has, as gRPC names them.
const (
	methodRead  = "/cactusfs.node.v1.StorageService/ReadShard"
	methodWrite = "/cactusfs.node.v1.StorageService/WriteShard"
)

// recorded is everything a Telemetry holds that could carry a name: every
// label of every metric, and of every span its name, its attributes, the
// description of its status and its events.
func recorded(t *testing.T, tel *telemetry.Telemetry, recorder *tracetest.SpanRecorder) []string {
	t.Helper()
	families, err := tel.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var all []string
	for _, family := range families {
		all = append(all, family.GetName())
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				all = append(all, label.GetName(), label.GetValue())
			}
		}
	}
	if recorder == nil {
		return all
	}
	for _, span := range recorder.Ended() {
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

// wantNothingOf fails when anything recorded contains one of the words: the
// name of a bucket, a key, a user, an access key.
func wantNothingOf(t *testing.T, tel *telemetry.Telemetry, recorder *tracetest.SpanRecorder, words ...string) {
	t.Helper()
	all := recorded(t, tel, recorder)
	if len(all) == 0 {
		t.Fatal("nothing was recorded, so nothing was checked")
	}
	for _, text := range all {
		for _, word := range words {
			if strings.Contains(text, word) {
				t.Errorf("%q is recorded, and contains %q", text, word)
			}
		}
	}
}

// answering is the far side of a call in these tests: it keeps the metadata
// the call arrived with and answers with err.
type answering struct {
	md  metadata.MD
	err error
}

func (a *answering) invoke(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
	a.md, _ = metadata.FromOutgoingContext(ctx)
	return a.err
}

// TestNodeRPCCallsAreOneTrace: the span of the node that answers is a child
// of the span of the node that calls, in the caller's trace - carried by a
// traceparent in the metadata of the call and by nothing else. Both sides
// count the call by its method and by how it ended.
func TestNodeRPCCallsAreOneTrace(t *testing.T) {
	t.Parallel()
	callerSpans, answererSpans := tracetest.NewSpanRecorder(), tracetest.NewSpanRecorder()
	caller, answerer := newTelemetry(t, callerSpans), newTelemetry(t, answererSpans)
	methods := []string{methodRead, methodWrite}

	// The caller keeps the metadata it had; the trace is added to it.
	far := &answering{}
	outgoing := metadata.AppendToOutgoingContext(context.Background(), "x-kept", "yes")
	if err := caller.NodeRPC(methods).UnaryClient()(outgoing, methodRead, nil, nil, nil, far.invoke); err != nil {
		t.Fatal(err)
	}
	if got := far.md.Get("x-kept"); len(got) != 1 || got[0] != "yes" {
		t.Errorf("the caller's own metadata did not travel: %v", far.md)
	}
	if len(far.md.Get("traceparent")) != 1 || len(far.md) != 2 {
		t.Fatalf("the call carries %v, want the caller's metadata and one traceparent", far.md)
	}

	// The other node: the call arrives with that metadata.
	incoming := metadata.NewIncomingContext(context.Background(), far.md)
	handled := false
	_, err := answerer.NodeRPC(methods).UnaryServer()(incoming, nil, &grpc.UnaryServerInfo{FullMethod: methodRead},
		func(context.Context, any) (any, error) {
			handled = true
			return nil, status.Error(codes.PermissionDenied, "node gateway-sentinel may not read bucket-sentinel/key-sentinel")
		})
	if !handled || status.Code(err) != codes.PermissionDenied {
		t.Fatalf("handled %v, error %v", handled, err)
	}

	sent, served := callerSpans.Ended(), answererSpans.Ended()
	if len(sent) != 1 || len(served) != 1 {
		t.Fatalf("%d spans of the caller and %d of the answerer, want one each", len(sent), len(served))
	}
	client, server := sent[0], served[0]
	if client.Name() != "cactusfs.noderpc/ReadShard" || server.Name() != "cactusfs.noderpc/ReadShard" {
		t.Errorf("spans are named %q and %q", client.Name(), server.Name())
	}
	if client.SpanKind().String() != "client" || server.SpanKind().String() != "server" {
		t.Errorf("span kinds are %s and %s", client.SpanKind(), server.SpanKind())
	}
	if !client.SpanContext().TraceID().IsValid() || server.SpanContext().TraceID() != client.SpanContext().TraceID() {
		t.Errorf("the two spans are in the traces %s and %s", client.SpanContext().TraceID(), server.SpanContext().TraceID())
	}
	if server.Parent().SpanID() != client.SpanContext().SpanID() || !server.Parent().IsRemote() {
		t.Errorf("the parent of the answerer's span is %s (remote: %v), want the caller's span %s",
			server.Parent().SpanID(), server.Parent().IsRemote(), client.SpanContext().SpanID())
	}

	for tel, want := range map[*telemetry.Telemetry]string{
		caller:   "cactusfs_noderpc_requests_total code=OK method=ReadShard side=client",
		answerer: "cactusfs_noderpc_requests_total code=PermissionDenied method=ReadShard side=server",
	} {
		if metrics := scrape(t, tel); !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q:\n%s", want, metrics)
		}
	}
	if metrics := scrape(t, answerer); !strings.Contains(metrics, "cactusfs_noderpc_request_duration_seconds method=ReadShard side=server count=1") {
		t.Errorf("the call was not timed:\n%s", metrics)
	}
	// The text of an error names what was asked for. It is in no span.
	wantNothingOf(t, answerer, answererSpans, "sentinel")
	wantNothingOf(t, caller, callerSpans, "sentinel")
}

// TestNodeRPCLabelsAreAFixedSet: the method label is one of the node's own
// methods or "other", the code one of gRPC's - whatever a peer sends.
func TestNodeRPCLabelsAreAFixedSet(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil)
	rpc := tel.NodeRPC([]string{methodRead})
	tests := []struct {
		name   string
		method string
		err    error
		want   string
	}{
		{"a method of the node", methodRead, nil, "code=OK method=ReadShard"},
		{"a method that is not the node's", "/attacker.Service/key-sentinel", nil, "code=OK method=other"},
		{"no method at all", "bucket-sentinel", nil, "code=OK method=other"},
		{"a status", methodRead, status.Error(codes.Unavailable, "down"), "code=Unavailable method=ReadShard"},
		{"a wrapped status", methodRead, errors.Join(errors.New("x"), status.Error(codes.NotFound, "x")), "code=NotFound method=ReadShard"},
		{"a code a peer made up", methodRead, status.Error(codes.Code(4711), "x"), "code=Unknown method=ReadShard"},
		{"a deadline", methodRead, context.DeadlineExceeded, "code=DeadlineExceeded method=ReadShard"},
		{"a cancellation", methodRead, context.Canceled, "code=Canceled method=ReadShard"},
		{"any other error", methodRead, io.ErrUnexpectedEOF, "code=Unknown method=ReadShard"},
	}
	for _, tt := range tests {
		far := &answering{err: tt.err}
		if err := rpc.UnaryClient()(context.Background(), tt.method, nil, nil, nil, far.invoke); !errors.Is(err, tt.err) {
			t.Errorf("%s: error %v, want %v", tt.name, err, tt.err)
		}
		_, _ = rpc.UnaryServer()(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: tt.method},
			func(context.Context, any) (any, error) { return nil, tt.err })
		metrics := scrape(t, tel)
		for _, side := range []string{"client", "server"} {
			if want := "cactusfs_noderpc_requests_total " + tt.want + " side=" + side; !strings.Contains(metrics, want) {
				t.Errorf("%s: metrics lack %q", tt.name, want)
			}
		}
	}
	wantNothingOf(t, tel, nil, "sentinel", "attacker", "4711")
}

// TestNodeRPCWithoutACollector: with no collector configured a call is
// counted and timed all the same, nothing is added to it, and what a caller
// claims about a trace is not read.
func TestNodeRPCWithoutACollector(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil)
	rpc := tel.NodeRPC([]string{methodRead})

	far := &answering{}
	if err := rpc.UnaryClient()(context.Background(), methodRead, nil, nil, nil, far.invoke); err != nil {
		t.Fatal(err)
	}
	if len(far.md) != 0 {
		t.Errorf("the call carries %v; without tracing it carries nothing of it", far.md)
	}
	incoming := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"))
	_, err := rpc.UnaryServer()(incoming, nil, &grpc.UnaryServerInfo{FullMethod: methodRead},
		func(ctx context.Context, _ any) (any, error) {
			// A call this node would make from here carries no trace.
			return nil, rpc.UnaryClient()(ctx, methodRead, nil, nil, nil, far.invoke)
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(far.md.Get("traceparent")) != 0 {
		t.Errorf("a trace was passed on by a node that does not trace: %v", far.md)
	}
	metrics := scrape(t, tel)
	for _, want := range []string{
		"cactusfs_noderpc_requests_total code=OK method=ReadShard side=client",
		"cactusfs_noderpc_requests_total code=OK method=ReadShard side=server",
		"cactusfs_noderpc_request_duration_seconds method=ReadShard side=client count=2",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q:\n%s", want, metrics)
		}
	}
}

// fakeStream is a stream of the far side that hands a test its context.
type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s fakeStream) Context() context.Context { return s.ctx }

// TestNodeRPCStreamsAreTracedOnTheAnsweringSide: the handler of a stream
// runs inside the span of the call, in the caller's trace.
func TestNodeRPCStreamsAreTracedOnTheAnsweringSide(t *testing.T) {
	t.Parallel()
	callerSpans, answererSpans := tracetest.NewSpanRecorder(), tracetest.NewSpanRecorder()
	caller, answerer := newTelemetry(t, callerSpans), newTelemetry(t, answererSpans)
	far := &answering{}
	if err := caller.NodeRPC([]string{methodWrite}).UnaryClient()(context.Background(), methodWrite, nil, nil, nil, far.invoke); err != nil {
		t.Fatal(err)
	}
	incoming := fakeStream{ctx: metadata.NewIncomingContext(context.Background(), far.md)}
	var inside context.Context
	err := answerer.NodeRPC([]string{methodWrite}).StreamServer()(nil, incoming, &grpc.StreamServerInfo{FullMethod: methodWrite},
		func(_ any, stream grpc.ServerStream) error {
			inside = stream.Context()
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	// The stores the handler calls are decorated: their spans are children.
	if _, err := answerer.InstrumentPasswords(fixedHasher{}).Hash("x"); err != nil {
		t.Fatal(err)
	}
	if err := answerer.InstrumentEventPublisher(&collecting{}).Publish(inside, ports.Event{}); err != nil {
		t.Fatal(err)
	}
	served := answererSpans.Ended()
	if len(served) != 2 || len(callerSpans.Ended()) != 1 {
		t.Fatalf("%d spans of the answerer, %d of the caller", len(served), len(callerSpans.Ended()))
	}
	server, child, client := served[0], served[1], callerSpans.Ended()[0]
	if server.Name() != "cactusfs.noderpc/WriteShard" || server.Parent().SpanID() != client.SpanContext().SpanID() {
		t.Errorf("the stream's span %q has the parent %s, want %s", server.Name(), server.Parent().SpanID(), client.SpanContext().SpanID())
	}
	if child.Parent().SpanID() != server.SpanContext().SpanID() || child.SpanContext().TraceID() != client.SpanContext().TraceID() {
		t.Errorf("what the handler did is not part of the caller's trace")
	}
	if metrics := scrape(t, answerer); !strings.Contains(metrics, "cactusfs_noderpc_requests_total code=OK method=WriteShard side=server") {
		t.Errorf("the stream was not counted:\n%s", metrics)
	}
}
