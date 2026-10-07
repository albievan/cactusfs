// Package telemetry is the observability adapter: Prometheus metrics named
// cactusfs_*, served on an authenticated /metrics endpoint, and OpenTelemetry
// traces.
//
// Nothing here is global. Metrics live in a registry owned by a Telemetry
// value, and traces go to a tracer provider owned by it; neither the default
// Prometheus registry nor the global OpenTelemetry provider is used. Without a
// configured OTLP endpoint no exporter exists and no connection is ever made,
// which keeps the node air-gap safe.
//
// Metrics carry no tenant data: no label holds a bucket name, an object key or
// an access key ID.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/albievan/cactusfs/internal/domain"
)

const (
	tracerName      = "github.com/albievan/cactusfs"
	resultOK        = "ok"
	resultError     = "error"
	shutdownTimeout = 5 * time.Second
)

// Config configures Telemetry.
type Config struct {
	// Version is the release version, reported in cactusfs_build_info.
	Version string
	// OTLPEndpoint is the URL of an OTLP/HTTP trace collector, for example
	// "https://collector.internal:4318". Empty disables trace export.
	OTLPEndpoint string
	// SpanProcessor, when not nil, receives every span instead of an OTLP
	// exporter. Tests use it to record spans.
	SpanProcessor sdktrace.SpanProcessor
}

// Telemetry owns the node's metrics and traces.
type Telemetry struct {
	registry *prometheus.Registry
	tracer   trace.Tracer
	shutdown func(context.Context) error
	// exporting reports whether spans leave the process.
	exporting bool

	requests       *prometheus.CounterVec
	requestSeconds *prometheus.HistogramVec
	requestBytes   *prometheus.CounterVec
	portCalls      *prometheus.CounterVec
	portSeconds    *prometheus.HistogramVec
}

// New returns the node's Telemetry. The caller must call Shutdown.
func New(ctx context.Context, cfg Config) (*Telemetry, error) {
	t := &Telemetry{registry: prometheus.NewRegistry(), shutdown: func(context.Context) error { return nil }}
	if err := t.initTracing(ctx, cfg); err != nil {
		return nil, fmt.Errorf("telemetry.New: %w", err)
	}

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cactusfs_build_info", Help: "Build information; the value is always 1.",
	}, []string{"version", "goversion"})
	buildInfo.WithLabelValues(cfg.Version, runtime.Version()).Set(1)

	t.requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cactusfs_s3_requests_total", Help: "S3 API requests by operation and HTTP status code.",
	}, []string{"operation", "code"})
	t.requestSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "cactusfs_s3_request_duration_seconds", Help: "Duration of S3 API requests by operation.",
		Buckets: prometheus.ExponentialBuckets(0.001, 4, 9),
	}, []string{"operation"})
	t.requestBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cactusfs_s3_request_bytes_total", Help: "Bytes received from and sent to S3 clients by operation.",
	}, []string{"operation", "direction"})
	t.portCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cactusfs_port_calls_total", Help: "Calls across an outbound adapter boundary by port, method and result.",
	}, []string{"port", "method", "result"})
	t.portSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "cactusfs_port_call_duration_seconds", Help: "Duration of calls across an outbound adapter boundary.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 9),
	}, []string{"port", "method"})

	for _, collector := range []prometheus.Collector{
		buildInfo, t.requests, t.requestSeconds, t.requestBytes, t.portCalls, t.portSeconds,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{Namespace: "cactusfs"}),
	} {
		if err := t.registry.Register(collector); err != nil {
			return nil, fmt.Errorf("telemetry.New: %w", err)
		}
	}
	return t, nil
}

func (t *Telemetry) initTracing(ctx context.Context, cfg Config) error {
	var processor sdktrace.SpanProcessor
	switch {
	case cfg.SpanProcessor != nil:
		processor = cfg.SpanProcessor
	case cfg.OTLPEndpoint != "":
		if err := validateEndpoint(cfg.OTLPEndpoint); err != nil {
			return err
		}
		exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.OTLPEndpoint))
		if err != nil {
			return fmt.Errorf("OTLP exporter: %w", err)
		}
		processor = sdktrace.NewBatchSpanProcessor(exporter)
		t.exporting = true
	default:
		// No endpoint: spans are created against a provider that drops
		// them, and nothing leaves the process.
		t.tracer = noop.NewTracerProvider().Tracer(tracerName)
		return nil
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(processor),
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", "cactusfs"),
			attribute.String("service.version", cfg.Version),
		)),
	)
	t.tracer = provider.Tracer(tracerName)
	t.shutdown = provider.Shutdown
	return nil
}

// validateEndpoint requires TLS to the collector, except on loopback: span
// names and timings describe what a deployment is doing.
func validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: OTLP endpoint %q is not a URL", domain.ErrInvalidArgument, endpoint)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("%w: OTLP endpoint %q must use https unless it is a loopback IP address",
			domain.ErrInvalidArgument, endpoint)
	default:
		return fmt.Errorf("%w: OTLP endpoint %q must be an https URL", domain.ErrInvalidArgument, endpoint)
	}
}

// Exporting reports whether traces are exported.
func (t *Telemetry) Exporting() bool {
	return t.exporting
}

// Shutdown flushes pending spans and stops the exporter.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := t.shutdown(ctx); err != nil {
		return fmt.Errorf("telemetry.Shutdown: %w", err)
	}
	return nil
}

// Gatherer returns the metrics registry, for the /metrics handler.
func (t *Telemetry) Gatherer() prometheus.Gatherer {
	return t.registry
}

// call measures one call across an outbound port: it starts a span and
// returns the function that ends it and records the metrics.
func (t *Telemetry) call(ctx context.Context, port, method string) (context.Context, func(error)) {
	started := time.Now()
	ctx, span := t.tracer.Start(ctx, port+"."+method, trace.WithSpanKind(trace.SpanKindInternal))
	return ctx, func(err error) {
		result := resultOK
		if err != nil {
			result = resultError
			span.SetStatus(codes.Error, errorClass(err))
		}
		span.End()
		t.portCalls.WithLabelValues(port, method, result).Inc()
		t.portSeconds.WithLabelValues(port, method).Observe(time.Since(started).Seconds())
	}
}

// errorClass names an error for a span without copying its text, which may
// name buckets or keys.
func errorClass(err error) string {
	for _, known := range []error{
		domain.ErrBucketNotFound, domain.ErrObjectNotFound, domain.ErrBucketAlreadyExists, domain.ErrBucketNotEmpty,
		domain.ErrIntegrity, domain.ErrShardNotFound, domain.ErrKeyUnavailable, domain.ErrUnsupportedFormat,
		domain.ErrNotLeader, domain.ErrShuttingDown, domain.ErrInvalidArgument, domain.ErrAccessDenied,
		domain.ErrNotBootstrapped, domain.ErrAlreadyBootstrapped, context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "error"
}
