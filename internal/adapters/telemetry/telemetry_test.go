package telemetry_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/albievan/cactusfs/internal/adapters/s3api"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/object"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

func newTelemetry(t *testing.T, recorder sdktrace.SpanProcessor) *telemetry.Telemetry {
	t.Helper()
	tel, err := telemetry.New(t.Context(), telemetry.Config{Version: "test", SpanProcessor: recorder})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel
}

func scrape(t *testing.T, tel *telemetry.Telemetry) string {
	t.Helper()
	families, err := tel.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var b strings.Builder
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			b.WriteString(family.GetName())
			for _, label := range metric.GetLabel() {
				b.WriteString(" " + label.GetName() + "=" + label.GetValue())
			}
			if histogram := metric.GetHistogram(); histogram != nil {
				b.WriteString(" count=" + strconv.FormatUint(histogram.GetSampleCount(), 10))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func spanNames(recorder *tracetest.SpanRecorder) []string {
	var names []string
	for _, span := range recorder.Ended() {
		names = append(names, span.Name())
	}
	return names
}

// TestDecoratorsPassBehaviourThrough runs the port contracts through the
// decorators: instrumenting a port must not change what it does.
func TestDecoratorsPassBehaviourThrough(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, tracetest.NewSpanRecorder())

	porttest.MetadataContract(t, func(*testing.T) porttest.Metadata {
		return tel.InstrumentMetadata(porttest.NewMemMetadata())
	})
	porttest.ShardStoreContract(t, func(*testing.T) ports.ShardStore {
		return tel.InstrumentShardStore(porttest.NewMemShardStore("mem-drive"))
	})
}

func TestEveryPortCallIsCountedAndTraced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	recorder := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, recorder)

	base, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}
	kms := tel.InstrumentKMS(base)
	meta := tel.InstrumentMetadata(porttest.NewMemMetadata())
	drive := tel.InstrumentShardStore(porttest.NewMemShardStore("drive-a"))

	// One call of every method, and one failing call per port.
	if kms.KeyID() != "test:1" || drive.DriveID() != "drive-a" {
		t.Error("identifiers are not passed through")
	}
	key, err := kms.GenerateDataKey(ctx, []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kms.UnwrapDataKey(ctx, key.Wrapped, []byte("aad")); err != nil {
		t.Fatal(err)
	}
	if _, err := kms.UnwrapDataKey(ctx, []byte("garbage"), nil); !errors.Is(err, domain.ErrKeyUnavailable) {
		t.Fatalf("error not passed through: %v", err)
	}

	if err := meta.CreateBucket(ctx, bucket.Bucket{Name: "bkt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.GetBucket(ctx, "bkt"); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.ListBuckets(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.PutVersion(ctx, porttest.SampleVersion("bkt", "k", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.GetObject(ctx, "bkt", "k"); err != nil {
		t.Fatal(err)
	}
	if err := meta.ScanObjects(ctx, "bkt", "", func(object.Version) ports.ScanControl { return ports.ScanControl{} }); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.DeleteObject(ctx, "bkt", "k", ports.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := meta.DeleteBucket(ctx, "bkt"); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.GetRootCredential(ctx); !errors.Is(err, domain.ErrNotBootstrapped) {
		t.Fatalf("error not passed through: %v", err)
	}
	if err := meta.SetRootCredential(ctx, iam.RootCredential{AccessKeyID: "ROOT"}); err != nil {
		t.Fatal(err)
	}

	id := object.ShardID{DataID: object.ID{1}}
	w, err := drive.Create(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := drive.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if err := drive.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := drive.Open(ctx, id); !errors.Is(err, domain.ErrShardNotFound) {
		t.Fatalf("error not passed through: %v", err)
	}
	if _, err := drive.Create(ctx, object.ShardID{DataID: object.ID{2}}); err != nil {
		t.Fatal(err)
	}

	metrics := scrape(t, tel)
	for _, want := range []string{
		"cactusfs_port_calls_total method=GenerateDataKey port=kms result=ok",
		"cactusfs_port_calls_total method=UnwrapDataKey port=kms result=ok",
		"cactusfs_port_calls_total method=UnwrapDataKey port=kms result=error",
		"cactusfs_port_calls_total method=CreateBucket port=metadata result=ok",
		"cactusfs_port_calls_total method=GetBucket port=metadata result=ok",
		"cactusfs_port_calls_total method=ListBuckets port=metadata result=ok",
		"cactusfs_port_calls_total method=PutVersion port=metadata result=ok",
		"cactusfs_port_calls_total method=GetObject port=metadata result=ok",
		"cactusfs_port_calls_total method=ScanObjects port=metadata result=ok",
		"cactusfs_port_calls_total method=DeleteObject port=metadata result=ok",
		"cactusfs_port_calls_total method=DeleteBucket port=metadata result=ok",
		"cactusfs_port_calls_total method=GetRootCredential port=metadata result=error",
		"cactusfs_port_calls_total method=SetRootCredential port=metadata result=ok",
		"cactusfs_port_calls_total method=Create port=shards result=ok",
		"cactusfs_port_calls_total method=Commit port=shards result=ok",
		"cactusfs_port_calls_total method=Open port=shards result=ok",
		"cactusfs_port_calls_total method=Open port=shards result=error",
		"cactusfs_port_calls_total method=Delete port=shards result=ok",
		"cactusfs_port_call_duration_seconds method=Commit port=shards",
		"cactusfs_build_info goversion=",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}

	names := spanNames(recorder)
	for _, want := range []string{"kms.GenerateDataKey", "kms.UnwrapDataKey", "metadata.PutVersion", "metadata.ScanObjects", "shards.Create", "shards.Commit", "shards.Open", "shards.Delete"} {
		if !contains(names, want) {
			t.Errorf("no span named %q among %v", want, names)
		}
	}
	// A failed call marks its span as an error, by class and without the
	// error's text.
	var failed int
	for _, span := range recorder.Ended() {
		if span.Status().Code.String() == "Error" {
			failed++
			if strings.Contains(span.Status().Description, "garbage") || strings.Contains(span.Status().Description, id.String()) {
				t.Errorf("span %s carries error detail: %q", span.Name(), span.Status().Description)
			}
		}
	}
	if failed != 3 {
		t.Errorf("%d spans are marked as errors, want 3", failed)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func TestS3ObservationsAndSpans(t *testing.T) {
	t.Parallel()
	recorder := tracetest.NewSpanRecorder()
	tel := newTelemetry(t, recorder)

	var inner trace.SpanContext
	handler := tel.TraceS3(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner = trace.SpanContextFromContext(r.Context())
		status := http.StatusOK
		if r.URL.Path == "/fail" {
			status = http.StatusInternalServerError
		}
		tel.ObserveS3(r.Context(), s3api.Observation{
			Operation: "GetObject", Status: status, Duration: 20 * time.Millisecond, BytesIn: 10, BytesOut: 2048,
		})
		w.WriteHeader(status)
	}))
	for _, path := range []string{"/ok", "/fail"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	}

	if !inner.IsValid() {
		t.Error("the handler did not run inside a span")
	}
	spans := recorder.Ended()
	if len(spans) != 2 || spans[0].Name() != "s3.GetObject" || spans[0].SpanKind() != trace.SpanKindServer {
		t.Fatalf("spans = %v", spanNames(recorder))
	}
	if spans[0].Status().Code.String() == "Error" || spans[1].Status().Code.String() != "Error" {
		t.Errorf("span statuses = %v, %v; want only the 500 marked as an error", spans[0].Status(), spans[1].Status())
	}

	metrics := scrape(t, tel)
	for _, want := range []string{
		"cactusfs_s3_requests_total code=200 operation=GetObject",
		"cactusfs_s3_requests_total code=500 operation=GetObject",
		"cactusfs_s3_request_bytes_total direction=in operation=GetObject",
		"cactusfs_s3_request_bytes_total direction=out operation=GetObject",
		"cactusfs_s3_request_duration_seconds operation=GetObject count=2",
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q:\n%s", want, metrics)
		}
	}
}

func TestBackgroundWorkIsCounted(t *testing.T) {
	t.Parallel()
	tel := newTelemetry(t, nil)

	tel.ObserveHeal(app.HealResult{Shards: 6})
	tel.ObserveHeal(app.HealResult{Shards: 6, Damaged: 2, Rebuilt: 2})
	tel.ObserveHeal(app.HealResult{Shards: 6, Damaged: 4, Unrecoverable: true})
	tel.ObserveScrub(app.ScrubStats{Objects: 10, Bytes: 4096})
	tel.ObserveGC(app.GCStats{Shards: 30, Removed: 3})

	families, err := tel.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			name := family.GetName()
			for _, label := range metric.GetLabel() {
				name += "{" + label.GetValue() + "}"
			}
			if counter := metric.GetCounter(); counter != nil {
				got[name] = counter.GetValue()
			}
		}
	}
	want := map[string]float64{
		"cactusfs_heal_objects_total{healthy}": 1, "cactusfs_heal_objects_total{healed}": 1, "cactusfs_heal_objects_total{unrecoverable}": 1,
		"cactusfs_heal_shards_rebuilt_total": 2, "cactusfs_scrub_passes_total": 1, "cactusfs_scrub_objects_total": 10,
		"cactusfs_scrub_bytes_total": 4096, "cactusfs_gc_passes_total": 1, "cactusfs_gc_shards_removed_total": 3,
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %v, want %v", name, got[name], value)
		}
	}
}

func TestNoExporterWithoutAnEndpoint(t *testing.T) {
	t.Parallel()

	tel, err := telemetry.New(t.Context(), telemetry.Config{Version: "test"})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	if tel.Exporting() {
		t.Fatal("traces are exported although no endpoint is configured")
	}
	// Instrumented calls still work, and shutting down has nothing to flush.
	base, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tel.InstrumentKMS(base).GenerateDataKey(t.Context(), nil); err != nil {
		t.Errorf("GenerateDataKey: %v", err)
	}
	if err := tel.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestOTLPEndpointMustBeEncrypted(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{
		"http://collector.internal:4318", "http://192.0.2.10:4318", "http://localhost:4318",
		"collector.internal:4318", "ftp://collector.internal", "://", "https://",
	} {
		if _, err := telemetry.New(t.Context(), telemetry.Config{OTLPEndpoint: endpoint}); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("endpoint %q: error = %v, want %v", endpoint, err, domain.ErrInvalidArgument)
		}
	}
	for _, endpoint := range []string{"https://collector.internal:4318", "http://127.0.0.1:4318", "http://[::1]:4318"} {
		tel, err := telemetry.New(t.Context(), telemetry.Config{OTLPEndpoint: endpoint})
		if err != nil {
			t.Errorf("endpoint %q: %v", endpoint, err)
			continue
		}
		if !tel.Exporting() {
			t.Errorf("endpoint %q: not exporting", endpoint)
		}
		// Nothing was recorded, so there is nothing to send and no
		// connection is attempted.
		if err := tel.Shutdown(t.Context()); err != nil {
			t.Errorf("endpoint %q: Shutdown: %v", endpoint, err)
		}
	}
}

func TestMetricsRequireAuthenticationAndAuthorization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tel := newTelemetry(t, nil)

	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}
	identities := app.NewIdentityService(porttest.NewMemMetadata(), kms, porttest.NewClock(time.Now()), rand.Reader)
	root, err := identities.EnsureRoot(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	authz := &porttest.Authorizer{}
	handler := tel.MetricsHandler(identities, authz, logger)

	// get returns the status, the headers and the body of a scrape.
	get := func(authorization string) (int, http.Header, string) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code, rec.Header(), rec.Body.String()
	}

	refused := map[string]string{
		"no credential":         "",
		"wrong scheme":          "Basic " + root.AccessKeyID + ":" + root.SecretKey,
		"no secret":             "Bearer " + root.AccessKeyID,
		"empty secret":          "Bearer " + root.AccessKeyID + ":",
		"empty access key":      "Bearer :" + root.SecretKey,
		"wrong secret":          "Bearer " + root.AccessKeyID + ":" + strings.Repeat("x", 40),
		"unknown access key":    "Bearer NOSUCHKEY:" + root.SecretKey,
		"secret with a suffix":  "Bearer " + root.AccessKeyID + ":" + root.SecretKey + "x",
		"lowercase bearer only": "bearer " + root.AccessKeyID + ":" + root.SecretKey,
	}
	for name, authorization := range refused {
		status, header, body := get(authorization)
		if status != http.StatusUnauthorized || strings.Contains(body, "cactusfs_") {
			t.Errorf("%s: status %d, body %q; want 401 and no metrics", name, status, body)
		}
		if header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: no WWW-Authenticate header", name)
		}
	}
	if len(authz.Requests()) != 0 {
		t.Errorf("the authorizer was asked about %d unauthenticated requests", len(authz.Requests()))
	}

	status, header, body := get("Bearer " + root.AccessKeyID + ":" + root.SecretKey)
	if status != http.StatusOK || !strings.Contains(body, "cactusfs_build_info") || !strings.Contains(body, "go_goroutines") {
		t.Fatalf("authenticated scrape: status %d, body %.200q", status, body)
	}
	if header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", header.Get("Cache-Control"))
	}
	requests := authz.Requests()
	if len(requests) != 1 || requests[0].Action != iam.ActionGetMetrics || !requests[0].Principal.Root {
		t.Errorf("authorizer saw %+v, want one admin:GetMetrics request by root", requests)
	}

	// An authenticated principal the authorizer refuses gets 403.
	denied := tel.MetricsHandler(notRoot{identities}, authz, logger)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+root.AccessKeyID+":"+root.SecretKey)
	rec := httptest.NewRecorder()
	denied.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "cactusfs_") {
		t.Errorf("non-root scrape: status %d, body %q; want 403 and no metrics", rec.Code, rec.Body.String())
	}

	if strings.Contains(logs.String(), root.SecretKey) {
		t.Error("the log contains the secret key")
	}
	if !strings.Contains(logs.String(), "authentication failed") {
		t.Error("failed authentications are not logged")
	}
}

// notRoot authenticates like the real service but strips the root flag, as a
// non-root key will behave once users exist.
type notRoot struct {
	next *app.IdentityService
}

func (n notRoot) AuthenticationFailed(context.Context, string, iam.Session, string) {}
func (n notRoot) Authenticated(iam.Principal)                                       {}

func (n notRoot) SecretFor(ctx context.Context, accessKeyID string) ([]byte, iam.Principal, error) {
	secret, principal, err := n.next.SecretFor(ctx, accessKeyID)
	principal.Root = false
	return secret, principal, err
}

// TestMetricsCarryNoTenantData: whatever names buckets, keys and access keys
// have, they never become label values.
func TestMetricsCarryNoTenantData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tel := newTelemetry(t, nil)

	meta := tel.InstrumentMetadata(porttest.NewMemMetadata())
	if err := meta.CreateBucket(ctx, bucket.Bucket{Name: "tenant-secret-bucket"}); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.PutVersion(ctx, porttest.SampleVersion("tenant-secret-bucket", "tenant/secret/key.pdf", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.GetObject(ctx, "tenant-secret-bucket", "absent-secret-key"); err == nil {
		t.Fatal("expected an error")
	}
	tel.ObserveS3(ctx, s3api.Observation{Operation: "PutObject", Status: 200})

	families, err := tel.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	allowed := regexp.MustCompile(`^(port|method|result|operation|code|direction|version|goversion|le|quantile)$`)
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "cactusfs_") {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if !allowed.MatchString(label.GetName()) {
					t.Errorf("%s has an unexpected label %q", family.GetName(), label.GetName())
				}
				if strings.Contains(label.GetValue(), "secret") {
					t.Errorf("%s label %s holds tenant data: %q", family.GetName(), label.GetName(), label.GetValue())
				}
			}
		}
	}
}
