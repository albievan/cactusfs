package telemetry_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/domain"
)

// collectorMark is in every credential the tests below put into the URL of a
// collector. It is put together here so that it is written nowhere whole.
func collectorMark() string {
	return strings.Join([]string{"MARK", "c41b", "NEVER", "SHOWN"}, "-")
}

// TestOTLPEndpointHoldsNoCredential: an endpoint with userinfo, a query or a
// fragment is refused, whatever else is right or wrong with it, and the
// refusal says why without repeating what it refuses.
func TestOTLPEndpointHoldsNoCredential(t *testing.T) {
	t.Parallel()
	mark := collectorMark()
	tests := []struct {
		name, endpoint string
		// shown is the endpoint as the error names it.
		shown string
	}{
		{name: "userinfo", endpoint: "https://tracer:" + mark + "@collector.test:4318/v1/traces", shown: `"https://collector.test:4318/v1/traces"`},
		{name: "a user alone", endpoint: "https://" + mark + "@collector.test:4318", shown: `"https://collector.test:4318"`},
		{name: "a query", endpoint: "https://collector.test:4318/?token=" + mark, shown: `"https://collector.test:4318/"`},
		{name: "a fragment", endpoint: "https://collector.test:4318/#" + mark, shown: `"https://collector.test:4318/"`},
		{name: "an empty query", endpoint: "https://collector.test:4318/?", shown: `"https://collector.test:4318/"`},
		{name: "all three", endpoint: "https://u:" + mark + "@collector.test:4318/?token=" + mark + "#" + mark, shown: `"https://collector.test:4318/"`},
		{name: "all three, without TLS", endpoint: "http://u:" + mark + "@example.com:4318/?token=" + mark, shown: `"http://example.com:4318/"`},
		{name: "on loopback", endpoint: "http://u:" + mark + "@127.0.0.1:4318", shown: `"http://127.0.0.1:4318"`},
		{name: "what is no URL", endpoint: "u:" + mark + "@collector.test:4318", shown: `"collector.test:4318"`},
		{name: "what cannot be parsed", endpoint: "https://u:" + mark + "@collector .test/%zz?" + mark, shown: `"https://collector .test/%zz"`},
		{name: "a password with the characters of a URL", endpoint: "https://u:" + mark + "/?#@collector.test:4318", shown: `"https://"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := telemetry.New(t.Context(), telemetry.Config{OTLPEndpoint: tt.endpoint})
			if !errors.Is(err, domain.ErrInvalidArgument) {
				t.Fatalf("error = %v, want %v", err, domain.ErrInvalidArgument)
			}
			text := err.Error()
			if strings.Contains(text, mark) {
				t.Errorf("the refusal repeats the credential: %s", text)
			}
			if !strings.Contains(text, "credentials do not belong in the endpoint URL") || !strings.Contains(text, "OTLP endpoint "+tt.shown) {
				t.Errorf("the refusal does not say why, or of which endpoint (%s): %s", tt.shown, text)
			}
		})
	}

	// What is refused for another reason is named without its credential
	// too - there is none in it, and the words of the parser are not used.
	for _, endpoint := range []string{"http://collector.test:4318", "ftp://collector.test", "https://", "https://collector.test:port"} {
		_, err := telemetry.New(t.Context(), telemetry.Config{OTLPEndpoint: endpoint})
		if !errors.Is(err, domain.ErrInvalidArgument) || strings.Contains(err.Error(), "credentials") || strings.Contains(err.Error(), "parse") {
			t.Errorf("endpoint %q: error = %v", endpoint, err)
		}
	}
}

// exportLog is the lines a handler wrote, as the node's log has them.
type exportLog struct {
	buf bytes.Buffer
}

func (l *exportLog) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&l.buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (l *exportLog) lines(t *testing.T) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a line of the log is not JSON: %q", line)
		}
		lines = append(lines, record)
	}
	return lines
}

// TestExportErrorsAreLoggedWithoutTheirText: what OpenTelemetry could not
// export is one line at WARN with a fixed message, the class of the error
// and its type; never its text, which quotes the URL of the collector. And
// it is one line a minute at most, however often the collector fails.
func TestExportErrorsAreLoggedWithoutTheirText(t *testing.T) {
	t.Parallel()
	mark := collectorMark()
	marked := "https://tracer:" + mark + "@collector.test:4318/v1/traces?token=" + mark + "#" + mark
	refused := &url.Error{Op: "Post", URL: marked, Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused by " + mark)}}

	var (
		log   exportLog
		mu    sync.Mutex
		clock = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	)
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		clock = clock.Add(d)
	}
	handler := telemetry.NewExportErrors(log.logger(), now)

	// Nothing to report is no line.
	handler.Handle(nil)
	if got := log.lines(t); len(got) != 0 {
		t.Fatalf("no error, and the log has %v", got)
	}

	handler.Handle(fmt.Errorf("traces export: %w", refused))
	// A collector that cannot be reached fails every batch, from every
	// goroutine that exports.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 25 {
				handler.Handle(errors.New(`Post "` + marked + `": context deadline exceeded`))
			}
		})
	}
	wg.Wait()
	advance(59 * time.Second)
	handler.Handle(refused)
	lines := log.lines(t)
	if len(lines) != 1 {
		t.Fatalf("%d lines within a minute, want 1:\n%s", len(lines), log.buf.String())
	}
	if first := lines[0]; first["level"] != "WARN" || first["msg"] != "traces could not be exported" || first["class"] != "connection" ||
		first["type"] != "*fmt.wrapError" || first["suppressed"] != float64(0) {
		t.Errorf("the line of the first error: %v", first)
	}

	// A minute after the line, the next error is said, with how many were
	// not.
	advance(time.Second)
	handler.Handle(context.DeadlineExceeded)
	handler.Handle(refused)
	lines = log.lines(t)
	if len(lines) != 2 {
		t.Fatalf("%d lines after a minute, want 2:\n%s", len(lines), log.buf.String())
	}
	if second := lines[1]; second["class"] != "timeout" || second["suppressed"] != float64(201) || second["level"] != "WARN" {
		t.Errorf("the line after a minute: %v", second)
	}
	advance(time.Minute)
	handler.Handle(refused)
	if lines = log.lines(t); len(lines) != 3 || lines[2]["suppressed"] != float64(1) || lines[2]["type"] != "*url.Error" {
		t.Errorf("the third line: %v", lines)
	}

	if text := log.buf.String(); strings.Contains(text, mark) || strings.Contains(text, "collector.test") || strings.Contains(text, "token") {
		t.Errorf("the log holds the URL of the collector, or what was in it:\n%s", text)
	}
}

// longNamed is an error whose type has a long name.
type longNamed[T any] struct{ error }

// TestExportErrorClasses: an error is named by one of a fixed set of words,
// and its type by a name of bounded length.
func TestExportErrorClasses(t *testing.T) {
	t.Parallel()
	timeout := &net.OpError{Op: "read", Err: context.DeadlineExceeded}
	tests := []struct {
		name  string
		err   error
		class string
	}{
		{name: "a deadline", err: fmt.Errorf("export: %w", context.DeadlineExceeded), class: "timeout"},
		{name: "a read that timed out", err: &url.Error{Op: "Post", URL: "https://collector.test", Err: timeout}, class: "timeout"},
		{name: "a caller that went away", err: &url.Error{Op: "Post", URL: "https://collector.test", Err: context.Canceled}, class: "canceled"},
		{name: "an unknown CA", err: &url.Error{Op: "Post", URL: "https://collector.test", Err: x509.UnknownAuthorityError{}}, class: "certificate"},
		{name: "another name", err: x509.HostnameError{Host: "collector.test"}, class: "certificate"},
		{name: "a certificate that ended", err: x509.CertificateInvalidError{Reason: x509.Expired}, class: "certificate"},
		{name: "a name that does not resolve", err: &net.OpError{Op: "dial", Err: &net.DNSError{Name: "collector.test", IsNotFound: true}}, class: "dns"},
		{name: "a connection refused", err: &net.OpError{Op: "dial", Err: errors.New("refused")}, class: "connection"},
		{name: "an answer that was no answer", err: &url.Error{Op: "Post", URL: "https://collector.test", Err: errors.New("EOF")}, class: "request"},
		{name: "anything else", err: errors.New("failed to upload traces; HTTP status code: 401"), class: "other"},
		{name: "a type with a long name", err: longNamed[map[string]map[string]map[string]map[string]map[string]map[string][]context.Context]{errors.New("x")}, class: "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var log exportLog
			telemetry.NewExportErrors(log.logger(), time.Now).Handle(tt.err)
			lines := log.lines(t)
			if len(lines) != 1 {
				t.Fatalf("%d lines", len(lines))
			}
			kind, isText := lines[0]["type"].(string)
			if lines[0]["class"] != tt.class || !isText || kind == "" || len(kind) > 96 {
				t.Errorf("class %v, type %v; want class %q and a type of 96 bytes at most", lines[0]["class"], lines[0]["type"], tt.class)
			}
			if strings.Contains(log.buf.String(), "collector.test") {
				t.Errorf("the line names the collector: %s", log.buf.String())
			}
		})
	}
}

// TestAnExporterReportsToTheNode: a Telemetry that exports takes over where
// OpenTelemetry reports its errors, which is standard error otherwise - and
// several in one process, as tests make them, do so without harm.
func TestAnExporterReportsToTheNode(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			var log exportLog
			tel, err := telemetry.New(t.Context(), telemetry.Config{OTLPEndpoint: "https://collector.test:4318", Logger: log.logger()})
			if err != nil {
				t.Errorf("telemetry.New: %v", err)
				return
			}
			if _, ours := otel.GetErrorHandler().(*telemetry.ExportErrors); !ours {
				t.Errorf("the error handler of the process is a %T", otel.GetErrorHandler())
			}
			// Nothing was recorded: nothing is sent, and nothing fails.
			if err := tel.Shutdown(t.Context()); err != nil {
				t.Errorf("Shutdown: %v", err)
			}
			if log.buf.Len() != 0 {
				t.Errorf("an exporter with nothing to send logged: %s", log.buf.String())
			}
		})
	}
	wg.Wait()
}
