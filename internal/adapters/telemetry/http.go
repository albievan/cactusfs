package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/albievan/cactusfs/internal/adapters/s3api"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

const bearerPrefix = "Bearer "

// TraceS3 wraps the S3 handler so that every request runs inside a server
// span. ObserveS3 names the span once the operation is known.
func (t *Telemetry) TraceS3(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := t.tracer.Start(r.Context(), "s3.request", trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ObserveS3 records one finished S3 request. It is the Observe hook of the
// S3 handler.
func (t *Telemetry) ObserveS3(ctx context.Context, o s3api.Observation) {
	code := strconv.Itoa(o.Status)
	t.requests.WithLabelValues(o.Operation, code).Inc()
	t.requestSeconds.WithLabelValues(o.Operation).Observe(o.Duration.Seconds())
	t.requestBytes.WithLabelValues(o.Operation, "in").Add(float64(o.BytesIn))
	t.requestBytes.WithLabelValues(o.Operation, "out").Add(float64(o.BytesOut))

	span := trace.SpanFromContext(ctx)
	span.SetName("s3." + o.Operation)
	span.SetAttributes(attribute.String("s3.operation", o.Operation), attribute.Int("http.response.status_code", o.Status))
	if o.Status >= http.StatusInternalServerError {
		span.SetStatus(codes.Error, code)
	}
}

// Identities resolves an access key to its secret and principal.
// *app.IdentityService implements it.
type Identities interface {
	SecretFor(ctx context.Context, accessKeyID string) ([]byte, iam.Principal, error)
	AuthenticationFailed(ctx context.Context, accessKeyID string, session iam.Session, reason string)
	Authenticated(p iam.Principal)
}

// MetricsHandler serves the metrics in the Prometheus text format to
// authenticated callers that the authorizer allows admin:GetMetrics.
//
// The credential is an API key sent as "Authorization: Bearer <access key
// ID>:<secret>", which a Prometheus scrape configuration can supply. It is
// only ever accepted over the admin listener, which is TLS outside development
// loopback mode.
func (t *Telemetry) MetricsHandler(identities Identities, authorizer ports.Authorizer, logger *slog.Logger) http.Handler {
	metrics := promhttp.HandlerFor(t.registry, promhttp.HandlerOpts{ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn)})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		principal, ok := authenticate(r, identities)
		if !ok {
			logger.WarnContext(r.Context(), "authentication failed", slog.String("endpoint", "/metrics"))
			w.Header().Set("WWW-Authenticate", `Bearer realm="cactusfs"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		request := iam.Request{Principal: principal, Action: iam.ActionGetMetrics, Resource: iam.Resource{Admin: "metrics"}}
		if err := authorizer.Authorize(r.Context(), request); err != nil {
			http.Error(w, "access denied", http.StatusForbidden)
			return
		}
		metrics.ServeHTTP(w, r)
	})
}

// authenticate checks the bearer credential of a request. Every failure
// looks the same to the caller.
func authenticate(r *http.Request, identities Identities) (iam.Principal, bool) {
	credential, ok := strings.CutPrefix(r.Header.Get("Authorization"), bearerPrefix)
	if !ok {
		return iam.Principal{}, false
	}
	accessKeyID, presented, ok := strings.Cut(credential, ":")
	if !ok || accessKeyID == "" || presented == "" {
		return iam.Principal{}, false
	}
	session := iam.Session{SecureTransport: r.TLS != nil}
	if addr, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		session.SourceIP = addr.Addr().Unmap()
	}
	secret, principal, err := identities.SecretFor(r.Context(), accessKeyID)
	if err != nil {
		identities.AuthenticationFailed(r.Context(), accessKeyID, session, "unknown access key")
		return iam.Principal{}, false
	}
	defer clear(secret)
	if !iam.SecretsEqual(secret, []byte(presented)) {
		identities.AuthenticationFailed(r.Context(), accessKeyID, session, "wrong secret")
		return iam.Principal{}, false
	}
	principal.Session = session
	identities.Authenticated(principal)
	return principal, true
}
