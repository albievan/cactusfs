package s3api

import (
	"encoding/xml"
	"io"
	"net/http"

	"github.com/albievan/cactusfs/internal/domain"
)

// Starting returns what the S3 listener serves while the node is not yet
// part of its deployment: 503 ServiceUnavailable to every request, in the
// form of every S3 error, with nothing read and nobody authenticated. A
// client retries; a load balancer that probes the port learns that the
// process is there.
func Starting() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := toAPIError(domain.ErrUnavailable)
		w.Header().Set("Server", serverName)
		w.Header().Set("Retry-After", "1")
		// The request is not read: the connection is not kept.
		w.Header().Set("Connection", "close")
		if r.Method == http.MethodHead {
			w.WriteHeader(api.status)
			return
		}
		data, err := xml.Marshal(errorResponse{Code: api.code, Message: api.message, Resource: r.URL.Path})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(api.status)
		if _, err := io.WriteString(w, xmlHeader); err == nil {
			_, _ = w.Write(data)
		}
	})
}
