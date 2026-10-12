package telemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sync"
	"time"
)

// exportErrorInterval is how often, at most, the node says that spans could
// not be exported: a collector that cannot be reached fails every batch, and
// must not fill the log.
const exportErrorInterval = time.Minute

// maxErrorType bounds the name of the type of an error in the log, in bytes.
const maxErrorType = 96

// Classes of an error of the exporter, as the log names them. They are the
// only values of the class attribute.
const (
	exportTimeout     = "timeout"
	exportCanceled    = "canceled"
	exportCertificate = "certificate"
	exportDNS         = "dns"
	exportConnection  = "connection"
	exportRequest     = "request"
	exportOther       = "other"
)

// ExportErrors is where OpenTelemetry reports what it could not do - a batch
// of spans the collector did not take, above all. It implements the
// library's ErrorHandler.
//
// The text of such an error is never logged. It is the library's and the
// HTTP client's, and quotes the URL it was sent to: the endpoint is refused
// when it holds a credential (validateEndpoint), but nothing guarantees what
// else a collector's answer or a proxy's puts into those words. What is
// logged is a fixed message, the class of the error - one of a fixed set -
// and the name of its Go type, at WARN, at most once in exportErrorInterval;
// the line after a silence says how many were left out.
//
// OpenTelemetry has one error handler for the whole process
// (otel.SetErrorHandler), and New sets it when it creates an exporter: left
// alone, the library prints every error to standard error, whole. That
// variable is the library's; this package keeps no state outside the values
// it returns. A process that makes more than one exporting Telemetry - only
// tests do - has the errors of all of them reported to the handler of the
// last, which names no Telemetry and so loses nothing by it.
type ExportErrors struct {
	logger *slog.Logger
	now    func() time.Time

	mu sync.Mutex
	// logged is when the last line was written, said whether one was;
	// suppressed counts the errors since that had no line.
	logged     time.Time
	said       bool
	suppressed uint64
}

// NewExportErrors returns a handler that logs to logger by the clock now.
func NewExportErrors(logger *slog.Logger, now func() time.Time) *ExportErrors {
	return &ExportErrors{logger: logger, now: now}
}

// Handle implements OpenTelemetry's ErrorHandler.
func (e *ExportErrors) Handle(err error) {
	if err == nil {
		return
	}
	e.mu.Lock()
	now := e.now()
	if e.said && now.Sub(e.logged) < exportErrorInterval {
		e.suppressed++
		e.mu.Unlock()
		return
	}
	suppressed := e.suppressed
	e.logged, e.said, e.suppressed = now, true, 0
	e.mu.Unlock()

	kind := fmt.Sprintf("%T", err)
	if len(kind) > maxErrorType {
		kind = kind[:maxErrorType]
	}
	// The library hands over no context: the error belongs to no request.
	e.logger.LogAttrs(context.Background(), slog.LevelWarn, "traces could not be exported",
		slog.String("class", exportErrorClass(err)), slog.String("type", kind),
		slog.Uint64("suppressed", suppressed))
}

// exportErrorClass names an error of the exporter without copying its text.
func exportErrorClass(err error) string {
	var (
		unknownAuthority x509.UnknownAuthorityError
		hostname         x509.HostnameError
		invalid          x509.CertificateInvalidError
		verification     *tls.CertificateVerificationError
		dns              *net.DNSError
		timeout          net.Error
		operation        *net.OpError
		request          *url.Error
	)
	switch {
	case errors.Is(err, context.Canceled):
		return exportCanceled
	case errors.As(err, &unknownAuthority), errors.As(err, &hostname), errors.As(err, &invalid), errors.As(err, &verification):
		return exportCertificate
	case errors.As(err, &dns):
		return exportDNS
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return exportTimeout
	case errors.As(err, &operation):
		return exportConnection
	case errors.As(err, &request):
		return exportRequest
	default:
		return exportOther
	}
}
