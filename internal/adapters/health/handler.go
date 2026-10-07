// Package health is the inbound adapter that serves the liveness and readiness
// probes, GET /healthz and GET /readyz.
//
// The probes are unauthenticated because orchestrators cannot hold
// credentials. For that reason a response never says more than "ok" or
// "unavailable"; the reason a node is not ready is written to the log only.
package health

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/albievan/cactusfs/internal/domain/cluster"
)

const (
	bodyOK          = `{"status":"ok"}` + "\n"
	bodyUnavailable = `{"status":"unavailable"}` + "\n"
)

// Reporter is the use case the handler depends on. *app.HealthService
// implements it.
type Reporter interface {
	// Readiness evaluates every readiness check of this node.
	Readiness(ctx context.Context) cluster.Readiness
}

// Handler serves the probe endpoints.
type Handler struct {
	reporter Reporter
	logger   *slog.Logger
	timeout  time.Duration
}

// NewHandler returns a Handler. timeout bounds one readiness evaluation.
func NewHandler(reporter Reporter, logger *slog.Logger, timeout time.Duration) *Handler {
	return &Handler{reporter: reporter, logger: logger, timeout: timeout}
}

// Register adds the probe routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", h.live)
	mux.HandleFunc("GET /readyz", h.ready)
}

// live reports that the process is up and able to answer HTTP.
func (h *Handler) live(w http.ResponseWriter, r *http.Request) {
	h.write(w, r, http.StatusOK, bodyOK)
}

// ready reports whether the node should receive traffic.
func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	readiness := h.reporter.Readiness(ctx)
	if readiness.Ready() {
		h.write(w, r, http.StatusOK, bodyOK)
		return
	}

	for _, check := range readiness.Failed() {
		h.logger.WarnContext(ctx, "readiness check failed",
			slog.String("check", check.Name),
			slog.Any("error", check.Err),
		)
	}
	h.write(w, r, http.StatusServiceUnavailable, bodyUnavailable)
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.WriteString(w, body); err != nil {
		h.logger.DebugContext(r.Context(), "probe response not delivered", slog.Any("error", err))
	}
}
