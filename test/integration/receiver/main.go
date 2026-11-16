//go:build integration

// Command receiver is the webhook receiver of the notification test of
// test/integration (PLAN 8.2.6, 8.2.8): a static program that
// TestBucketNotifications builds and runs in the distroless base image of the
// product, beside a four-node cluster.
//
// It serves HTTPS with a certificate the test made, takes a POST at one path
// with one bearer secret, and keeps every request it took - the headers of
// ADR-0025 and the body as it arrived, before any parsing - as a line of JSON
// appended to a file, before it answers 200. Nothing is deduplicated and
// nothing is verified here: the test counts what arrived more than once and
// verifies every signature, with the signing key the cluster showed it once.
//
// The file is in the container's own file system, so it outlives a stop and
// a start of the container: the test reads it with the engine's copy, and a
// request the receiver answered is in it, whenever the test looks. With
// -audit-records the records of the audit trail (X-Cactusfs-Event-Type
// cactusfs.audit.exported.v1) are kept in a file of their own, the same way,
// so that the lines of the first are the notifications alone. A request
// at another path, with another method or with another bearer secret is
// answered 404, 405 or 401, and kept nowhere.
//
// With -hold-at N the request that becomes line N of the file is written
// and then never answered: it is held until its client goes away, or for
// holdFor, and its connection is closed under it. A sender that is killed
// while it waits has not seen it taken, and must send it again. Lines are
// counted from the start of the file, so a receiver started again over its
// file does not hold another; the file of the audit trail is not counted.
// Only HTTP/1.1 is served, so that a held request's connection can be taken
// and closed.
//
//	receiver -addr :8443 -cert tls.pem -key tls.key -records /tmp/records.jsonl -audit-records /tmp/audit.jsonl -hold-at 800
//
// RECEIVER_PATH and RECEIVER_BEARER name the path and the secret. The secret
// is made up for the run, and is printed nowhere.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// maxBody bounds the body of a request: a notification is one record.
const maxBody = 1 << 20

// auditType is the type of a record of the audit trail (ADR-0025).
const auditType = "cactusfs.audit.exported.v1"

// holdFor bounds how long the held request is held.
const holdFor = 2 * time.Minute

// Record is a request the receiver took, as a line of its file.
type Record struct {
	// At is when it arrived, by the receiver's clock.
	At time.Time `json:"at"`
	// ID, Type, Timestamp and Signature are the headers X-Cactusfs-Event-Id,
	// -Event-Type, -Timestamp and -Signature.
	ID        string `json:"id"`
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Signature string `json:"signature"`
	// ContentType and UserAgent are the headers of those names.
	ContentType string `json:"content_type"`
	UserAgent   string `json:"user_agent"`
	// Body is the body, byte for byte.
	Body []byte `json:"body"`
}

// receiver takes notifications and appends them to its file.
type receiver struct {
	path   string
	bearer []byte
	// holdAt is the line whose request is held; 0 holds none.
	holdAt int
	mu     sync.Mutex
	out    *os.File
	// lines is how many lines the file holds.
	lines int
	// audit, when not nil, is the file the records of the audit trail are
	// appended to.
	audit *os.File
}

// ServeHTTP implements http.Handler.
func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != rc.path {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), rc.bearer) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "body", http.StatusRequestEntityTooLarge)
		return
	}
	line, err := json.Marshal(Record{
		At: time.Now().UTC(), ID: r.Header.Get("X-Cactusfs-Event-Id"), Type: r.Header.Get("X-Cactusfs-Event-Type"),
		Timestamp: r.Header.Get("X-Cactusfs-Timestamp"), Signature: r.Header.Get("X-Cactusfs-Signature"),
		ContentType: r.Header.Get("Content-Type"), UserAgent: r.Header.Get("User-Agent"), Body: body,
	})
	if err != nil {
		http.Error(w, "record", http.StatusInternalServerError)
		return
	}
	// One line at a time, written whole before the answer: what was
	// answered 200 is in the file.
	rc.mu.Lock()
	held := false
	if rc.audit != nil && r.Header.Get("X-Cactusfs-Event-Type") == auditType {
		_, err = rc.audit.Write(append(line, '\n'))
	} else {
		_, err = rc.out.Write(append(line, '\n'))
		if err == nil {
			rc.lines++
		}
		held = err == nil && rc.lines == rc.holdAt
	}
	rc.mu.Unlock()
	if err != nil {
		http.Error(w, "record", http.StatusInternalServerError)
		return
	}
	if held {
		rc.hold(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// hold keeps a request unanswered until its client goes away or holdFor
// has passed, then closes its connection without an answer.
func (rc *receiver) hold(w http.ResponseWriter, r *http.Request) {
	slog.Info("receiver holding a request", "line", rc.holdAt)
	timer := time.NewTimer(holdFor)
	defer timer.Stop()
	select {
	case <-r.Context().Done():
	case <-timer.C:
	}
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		slog.Error("receiver could not drop the held request", "error", err.Error())
		return
	}
	_ = conn.Close()
	slog.Info("receiver dropped the held request", "line", rc.holdAt)
}

// countLines returns how many lines a file holds.
func countLines(f *os.File) (int, error) {
	data, err := io.ReadAll(f)
	if err != nil {
		return 0, err
	}
	return bytes.Count(data, []byte{'\n'}), nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	if err := run(logger, os.Args[1:]); err != nil {
		logger.Error("receiver stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger, args []string) error {
	flags := flag.NewFlagSet("receiver", flag.ContinueOnError)
	addr := flags.String("addr", ":8443", "where to listen")
	certFile := flags.String("cert", "", "the certificate, in PEM")
	keyFile := flags.String("key", "", "its key, in PEM")
	records := flags.String("records", "/tmp/records.jsonl", "the file requests are appended to")
	auditRecords := flags.String("audit-records", "", "the file records of the audit trail are appended to; empty for the file of -records")
	holdAt := flags.Int("hold-at", 0, "the line of the file whose request is held unanswered; 0 for none")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("arguments: %w", err)
	}
	path, bearer := os.Getenv("RECEIVER_PATH"), os.Getenv("RECEIVER_BEARER")
	if path == "" || bearer == "" {
		return errors.New("RECEIVER_PATH and RECEIVER_BEARER are required")
	}
	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		return fmt.Errorf("certificate: %w", err)
	}
	root, err := os.OpenRoot(filepath.Dir(*records))
	if err != nil {
		return fmt.Errorf("records: %w", err)
	}
	defer func() { _ = root.Close() }()
	out, err := root.OpenFile(filepath.Base(*records), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("records: %w", err)
	}
	defer func() { _ = out.Close() }()
	lines, err := countLines(out)
	if err != nil {
		return fmt.Errorf("records: %w", err)
	}
	var audit *os.File
	if *auditRecords != "" {
		if filepath.Dir(*auditRecords) != filepath.Dir(*records) {
			return errors.New("audit-records: in the directory of records")
		}
		if audit, err = root.OpenFile(filepath.Base(*auditRecords), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err != nil {
			return fmt.Errorf("audit-records: %w", err)
		}
		defer func() { _ = audit.Close() }()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	server := &http.Server{
		Handler: &receiver{path: path, bearer: []byte("Bearer " + bearer), holdAt: *holdAt, out: out, lines: lines, audit: audit},
		// HTTP/1.1 alone: a held request's connection is taken and closed.
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}},
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	served := make(chan error, 1)
	go func() { served <- server.ServeTLS(listener, "", "") }()
	logger.Info("receiver listening", "addr", listener.Addr().String())

	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	// A stop lets what is being answered finish: a request that was
	// answered is in the file, and one that was not is sent again.
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Info("receiver stopped")
	return nil
}
