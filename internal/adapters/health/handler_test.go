package health_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/health"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/ports"
)

type failingCheck struct{ err error }

func (failingCheck) Name() string                  { return "disk" }
func (f failingCheck) Check(context.Context) error { return f.err }

type deadlineReporter struct{ hasDeadline bool }

func (d *deadlineReporter) Readiness(ctx context.Context) cluster.Readiness {
	_, d.hasDeadline = ctx.Deadline()
	return cluster.Readiness{}
}

func newMux(t *testing.T, logs *bytes.Buffer, checks ...ports.ReadinessCheck) *http.ServeMux {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	mux := http.NewServeMux()
	health.NewHandler(app.NewHealthService(checks...), logger, time.Second).Register(mux)
	return mux
}

func TestHandler(t *testing.T) {
	t.Parallel()

	const failureReason = "open /mnt/drive7: input/output error"

	tests := []struct {
		name       string
		method     string
		path       string
		checks     []ports.ReadinessCheck
		wantStatus int
		wantBody   string
		wantLog    string
	}{
		{name: "liveness", method: http.MethodGet, path: "/healthz", wantStatus: http.StatusOK, wantBody: `{"status":"ok"}` + "\n"},
		{
			name: "liveness ignores failing readiness", method: http.MethodGet, path: "/healthz",
			checks:     []ports.ReadinessCheck{failingCheck{err: errors.New(failureReason)}},
			wantStatus: http.StatusOK, wantBody: `{"status":"ok"}` + "\n",
		},
		{name: "liveness HEAD has no body", method: http.MethodHead, path: "/healthz", wantStatus: http.StatusOK},
		{name: "ready", method: http.MethodGet, path: "/readyz", wantStatus: http.StatusOK, wantBody: `{"status":"ok"}` + "\n"},
		{
			name: "not ready hides the reason from the client and logs it", method: http.MethodGet, path: "/readyz",
			checks:     []ports.ReadinessCheck{failingCheck{err: errors.New(failureReason)}},
			wantStatus: http.StatusServiceUnavailable, wantBody: `{"status":"unavailable"}` + "\n",
			wantLog: failureReason,
		},
		{name: "POST is rejected", method: http.MethodPost, path: "/readyz", wantStatus: http.StatusMethodNotAllowed},
		{name: "nothing else is served", method: http.MethodGet, path: "/", wantStatus: http.StatusNotFound},
		{name: "no metrics yet", method: http.MethodGet, path: "/metrics", wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var logs bytes.Buffer
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)

			newMux(t, &logs, tt.checks...).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusOK || tt.wantStatus == http.StatusServiceUnavailable {
				if got := rec.Body.String(); got != tt.wantBody {
					t.Errorf("body = %q, want %q", got, tt.wantBody)
				}
				if got := rec.Header().Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", got)
				}
				if got := rec.Header().Get("Cache-Control"); got != "no-store" {
					t.Errorf("Cache-Control = %q, want no-store", got)
				}
			}
			if strings.Contains(rec.Body.String(), failureReason) {
				t.Errorf("response leaks the failure reason: %q", rec.Body.String())
			}
			if tt.wantLog != "" && !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("log %q does not contain %q", logs.String(), tt.wantLog)
			}
		})
	}
}

func TestHandlerBoundsReadinessWithTimeout(t *testing.T) {
	t.Parallel()

	reporter := &deadlineReporter{}
	mux := http.NewServeMux()
	health.NewHandler(reporter, slog.New(slog.DiscardHandler), time.Second).Register(mux)

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))

	if !reporter.hasDeadline {
		t.Error("readiness was evaluated without a deadline")
	}
}

func TestGate(t *testing.T) {
	t.Parallel()

	gate := health.NewGate()
	if gate.Name() == "" {
		t.Error("Name() is empty")
	}
	if err := gate.Check(t.Context()); err != nil {
		t.Fatalf("open gate: Check() = %v, want nil", err)
	}

	gate.Close()
	gate.Close()

	if err := gate.Check(t.Context()); !errors.Is(err, domain.ErrShuttingDown) {
		t.Fatalf("closed gate: Check() = %v, want %v", err, domain.ErrShuttingDown)
	}
}

func TestReadyzTurnsUnavailableWhenGateCloses(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	gate := health.NewGate()
	mux := newMux(t, &logs, gate)

	get := func() int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
		return rec.Code
	}

	if got := get(); got != http.StatusOK {
		t.Fatalf("before close: status = %d, want 200", got)
	}
	gate.Close()
	if got := get(); got != http.StatusServiceUnavailable {
		t.Fatalf("after close: status = %d, want 503", got)
	}
}
