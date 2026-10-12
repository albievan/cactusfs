package telemetry

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcmetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	rpcSpanPrefix = "cactusfs.noderpc/"
	sideClient    = "client"
	sideServer    = "server"
	// methodOther is the method of a call that is none of the node's own.
	methodOther = "other"
)

// NodeRPC traces and counts the calls between nodes, on the node that makes
// them and on the node that answers. A call is one span on each side, named
// "cactusfs.noderpc/<Method>"; the caller's trace travels in the metadata of
// the call as a W3C traceparent - with the tracestate that came with it, if
// any, which is passed on as it is and goes to the collector as the trace
// state of the span - so that the two are one trace. Nothing else is read
// from the metadata or written to it: no baggage.
//
// Nothing of a call is recorded but its method and how it ended: no request,
// no answer, no name of a node, and never the text of an error, which may
// name a bucket or a key.
type NodeRPC struct {
	t *Telemetry
	// methods maps the full name of a method, "/package.Service/Method", to
	// the name it is counted under.
	methods map[string]string
}

// NodeRPC returns the instrumentation of node RPC. fullMethods are the
// methods the node has, as gRPC names them ("/package.Service/Method"); they
// are the only values the method label ever takes, with "other" for a call
// that is none of them.
func (t *Telemetry) NodeRPC(fullMethods []string) *NodeRPC {
	r := &NodeRPC{t: t, methods: make(map[string]string, len(fullMethods))}
	for _, full := range fullMethods {
		r.methods[full] = full[strings.LastIndexByte(full, '/')+1:]
	}
	return r
}

// carrier is the metadata of a call as a propagator reads and writes it.
type carrier grpcmetadata.MD

// Get implements propagation.TextMapCarrier.
func (c carrier) Get(key string) string {
	if values := grpcmetadata.MD(c).Get(key); len(values) > 0 {
		return values[0]
	}
	return ""
}

// Set implements propagation.TextMapCarrier.
func (c carrier) Set(key, value string) { grpcmetadata.MD(c).Set(key, value) }

// Keys implements propagation.TextMapCarrier.
func (c carrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for key := range c {
		keys = append(keys, key)
	}
	return keys
}

// rpcCode is how a call ended, as one of the codes gRPC defines. A code a
// peer made up is counted as Unknown: the label takes no value a caller
// chooses.
func rpcCode(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	code := status.FromContextError(err).Code()
	if s, ok := status.FromError(err); ok {
		code = s.Code()
	}
	if code > codes.Unauthenticated {
		return codes.Unknown
	}
	return code
}

// begin starts the span of one call and returns the function that ends it
// and counts the call. Without a collector the span is one that records
// nothing, and nothing is read from or written to the metadata of the call.
func (r *NodeRPC) begin(ctx context.Context, side, fullMethod string) (context.Context, func(error)) {
	method, ok := r.methods[fullMethod]
	if !ok {
		method = methodOther
	}
	started := time.Now()
	kind := trace.SpanKindClient
	if side == sideServer {
		kind = trace.SpanKindServer
		if r.t.tracing {
			if md, ok := grpcmetadata.FromIncomingContext(ctx); ok {
				ctx = propagation.TraceContext{}.Extract(ctx, carrier(md))
			}
		}
	}
	ctx, span := r.t.tracer.Start(ctx, rpcSpanPrefix+method, trace.WithSpanKind(kind))
	if side == sideClient && span.SpanContext().IsValid() {
		md, _ := grpcmetadata.FromOutgoingContext(ctx)
		md = md.Copy()
		propagation.TraceContext{}.Inject(ctx, carrier(md))
		ctx = grpcmetadata.NewOutgoingContext(ctx, md)
	}
	return ctx, func(err error) {
		code := rpcCode(err).String()
		if span.IsRecording() {
			span.SetAttributes(
				attribute.String("rpc.system", "grpc"), attribute.String("rpc.method", method),
				attribute.String("rpc.grpc.status_code", code),
			)
			if err != nil {
				span.SetStatus(otelcodes.Error, code)
			}
		}
		r.t.rpcRequests.WithLabelValues(side, method, code).Inc()
		r.t.rpcSeconds.WithLabelValues(side, method).Observe(time.Since(started).Seconds())
		span.End()
	}
}

// UnaryClient returns the interceptor of the calls this node makes that have
// one request and one answer.
func (r *NodeRPC) UnaryClient() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context, method string, req, reply any, conn *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption,
	) error {
		ctx, done := r.begin(ctx, sideClient, method)
		err := invoker(ctx, method, req, reply, conn, opts...)
		done(err)
		return err
	}
}

// StreamClient returns the interceptor of the streams this node opens. The
// span of a stream ends, and the call is counted, when gRPC says the stream
// has ended.
//
// A stream that gRPC reports as Canceled, and on which no send and no
// receive had failed, is counted as OK on this side - whoever cancelled it.
// That is how a shard writer ends its stream once the other node has
// confirmed the commit, and it is the reason for the rule; but a stream that
// ended because the context of its request was cancelled, before any message
// of it failed, looks the same from here and is counted as OK as well. This
// side cannot tell the two apart. The node that served the stream can: it
// says whether the stream was completed or abandoned.
func (r *NodeRPC) StreamClient() grpc.StreamClientInterceptor {
	return func(
		ctx context.Context, desc *grpc.StreamDesc, conn *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		ctx, done := r.begin(ctx, sideClient, method)
		traced := &clientStream{}
		finish := func(err error) {
			if status.Code(err) == codes.Canceled && !traced.failed.Load() {
				err = nil
			}
			traced.once.Do(func() { done(err) })
		}
		stream, err := streamer(ctx, desc, conn, method, append(opts, grpc.OnFinish(finish))...)
		if err != nil {
			traced.failed.Store(true)
			finish(err)
			return nil, err
		}
		traced.ClientStream = stream
		return traced, nil
	}
}

// clientStream notes whether a stream ever failed under its caller.
type clientStream struct {
	grpc.ClientStream
	failed atomic.Bool
	once   sync.Once
}

// SendMsg implements grpc.ClientStream.
func (s *clientStream) SendMsg(m any) error {
	err := s.ClientStream.SendMsg(m)
	if err != nil {
		s.failed.Store(true)
	}
	return err
}

// RecvMsg implements grpc.ClientStream. The end of a stream is not a
// failure of it.
func (s *clientStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err != nil && !errors.Is(err, io.EOF) {
		s.failed.Store(true)
	}
	return err
}

// UnaryServer returns the interceptor of the calls this node answers that
// have one request and one answer. It belongs before every other
// interceptor, so that a call that is refused is counted too.
func (r *NodeRPC) UnaryServer() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, done := r.begin(ctx, sideServer, info.FullMethod)
		resp, err := handler(ctx, req)
		done(err)
		return resp, err
	}
}

// StreamServer returns the interceptor of the streams this node serves. It
// belongs before every other interceptor.
func (r *NodeRPC) StreamServer() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, done := r.begin(stream.Context(), sideServer, info.FullMethod)
		err := handler(srv, serverStream{ServerStream: stream, ctx: ctx})
		done(err)
		return err
	}
}

// serverStream is a stream whose context carries the span of the call.
type serverStream struct {
	grpc.ServerStream
	ctx context.Context
}

// Context implements grpc.ServerStream.
func (s serverStream) Context() context.Context { return s.ctx }
