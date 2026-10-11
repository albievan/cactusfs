package adminapi

import (
	"encoding/json"
	"net/http"

	"github.com/albievan/cactusfs/internal/adapters/adminapi/api"
)

// Starting returns what the admin listener serves, besides the probes, while
// the node is not yet part of its deployment: 503 as problem+json to every
// request, with nothing read and nobody authenticated.
func Starting() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		instance := r.URL.Path
		detail := "The node is starting."
		body := api.Problem{
			Type: "about:blank", Title: "Temporarily unavailable", Status: http.StatusServiceUnavailable,
			Code: "ServiceUnavailable", Detail: &detail, Instance: &instance,
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method != http.MethodHead {
			_ = json.NewEncoder(w).Encode(body)
		}
	})
}
