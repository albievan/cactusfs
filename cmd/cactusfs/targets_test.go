package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/albievan/cactusfs/api/openapi"
	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// TestTheSecretsOfATargetAreNowhere is the secret scan of PLAN 8.0.13 on a
// node as the binary wires it - the metadata store of Raft and bbolt, the
// sealed local key file, the bus of events, the telemetry: a target's
// secret and the path of its endpoint, marked, and the signing keys it is
// given, appear in no answer of the management API (but the signing key in
// the one answer that made it), no line of the log, no audit record, no
// event and no span.
func TestTheSecretsOfATargetAreNowhere(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const (
		concealedMark = "MARKED-node-secret-2b7c55"
		concealedPath = "/services/MARKED-node-path-e0a4f1"
	)
	dir := t.TempDir()
	cfg := nodeConfig(dir)
	recorder := tracetest.NewSpanRecorder()
	tel, err := telemetry.New(ctx, telemetry.Config{Version: buildinfo.Get().Tag(), SpanProcessor: recorder})
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{buf: &bytes.Buffer{}}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	var notice bytes.Buffer
	n, err := openNode(ctx, cfg, tel, logger, &notice)
	if err != nil {
		t.Fatalf("openNode: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })

	var (
		mu     sync.Mutex
		events []string
	)
	n.events.Subscribe("secret-scan", func(_ context.Context, e ports.Event) {
		data, err := json.Marshal(e)
		mu.Lock()
		defer mu.Unlock()
		events = append(events, fmt.Sprintf("%s %v", data, err))
	})
	n.startBackground(ctx, cfg)

	handler := tel.TraceAdmin(adminapi.NewHandler(adminapi.Config{
		Buckets: n.buckets, Objects: n.objects, Search: n.search, IAM: n.iam, Sessions: n.sessions, Cluster: n.cluster,
		Settings: n.settings, Targets: n.targets, Idempotency: n.idempotency, Identities: n.identities, Clock: n.clock,
		Logger: logger, SpecYAML: openapi.AdminYAML(), SpecJSON: openapi.AdminJSON(), Build: buildinfo.Get(),
		Observe: tel.ObserveAdmin,
	}))
	credentials := shownCredentials(t, notice.String())
	call := func(method, target, idempotencyKey string, body any) (int, string) {
		t.Helper()
		var content []byte
		if body != nil {
			if content, err = json.Marshal(body); err != nil {
				t.Fatal(err)
			}
		}
		request := httptest.NewRequestWithContext(ctx, method, adminapi.BasePath+target, bytes.NewReader(content))
		request.Header.Set("Authorization", "Bearer "+credentials.AccessKeyID+":"+credentials.SecretKey)
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		if idempotencyKey != "" {
			request.Header.Set("Idempotency-Key", idempotencyKey)
		}
		answer := httptest.NewRecorder()
		handler.ServeHTTP(answer, request)
		return answer.Code, answer.Body.String()
	}
	signingKey := func(body string) string {
		t.Helper()
		var made struct {
			SigningKey string `json:"signing_key"`
		}
		if err := json.Unmarshal([]byte(body), &made); err != nil || made.SigningKey == "" {
			t.Fatalf("no signing key in %s (%v)", body, err)
		}
		return made.SigningKey
	}

	hook := map[string]any{
		"kind": "webhook", "endpoint": "https://hooks.example.com:8443" + concealedPath, "buckets": []string{"*"},
		"auth": map[string]any{"mode": "bearer", "secret": concealedMark},
	}
	status, made := call(http.MethodPut, "/targets/hook", "define-hook", hook)
	if status != http.StatusOK {
		t.Fatalf("PUT /targets/hook = %d %s", status, made)
	}
	keys := []string{signingKey(made)}
	// The answers that may hold no signing key at all.
	var answers []string
	for _, c := range []struct {
		method, target, key string
		body                any
		want                int
	}{
		{http.MethodGet, "/targets/hook", "", nil, http.StatusOK},
		{http.MethodGet, "/targets", "", nil, http.StatusOK},
		{http.MethodPut, "/targets/hook", "", map[string]any{"kind": "webhook", "buckets": []string{"photos"}, "auth": map[string]any{"mode": "bearer"}}, http.StatusOK},
		{http.MethodPut, "/targets/other", "", map[string]any{"kind": "webhook", "endpoint": "http://hooks.example.com" + concealedPath, "buckets": []string{}}, http.StatusBadRequest},
		{http.MethodGet, "/audit", "", nil, http.StatusOK},
	} {
		status, body := call(c.method, c.target, c.key, c.body)
		if status != c.want {
			t.Errorf("%s %s = %d, want %d: %s", c.method, c.target, status, c.want, body)
		}
		answers = append(answers, body)
	}
	status, rotated := call(http.MethodPut, "/targets/hook", "rotate-hook", map[string]any{
		"kind": "webhook", "buckets": []string{"photos"}, "auth": map[string]any{"mode": "bearer"}, "rotate_signing_key": true,
	})
	if status != http.StatusOK {
		t.Fatalf("PUT with rotate_signing_key = %d %s", status, rotated)
	}
	keys = append(keys, signingKey(rotated))
	if status, body := call(http.MethodDelete, "/targets/hook", "delete-hook", nil); status != http.StatusNoContent {
		t.Fatalf("DELETE /targets/hook = %d %s", status, body)
	}

	// The events of the four changes, derived from the entries applied.
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		changed, deleted := 0, 0
		for _, e := range events {
			changed += strings.Count(e, ports.EventTargetChanged)
			deleted += strings.Count(e, ports.EventTargetDeleted)
		}
		mu.Unlock()
		if changed == 3 && deleted == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d target-changed and %d target-deleted events, want 3 and 1", changed, deleted)
		}
		time.Sleep(10 * time.Millisecond)
	}
	n.auditor.Flush(ctx)
	trail, err := n.iam.AuditLog(ctx, iam.Principal{Root: true, AccessKeyID: credentials.AccessKeyID}, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	trailText, err := json.Marshal(trail)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(trailText), string(iam.ActionPutTarget)) || !strings.Contains(string(trailText), string(iam.ActionDeleteTarget)) {
		t.Errorf("the changes of the target are not in the audit trail: %s", trailText)
	}
	var spans []string
	for _, span := range recorder.Ended() {
		spans = append(spans, fmt.Sprintf("%s %v %v %v", span.Name(), span.Attributes(), span.Status(), span.Events()))
	}
	if !strings.Contains(strings.Join(spans, "\n"), "targets.PutTarget") {
		t.Errorf("no span of the store of targets among %d spans", len(spans))
	}
	logs.mu.Lock()
	logText := logs.buf.String()
	logs.mu.Unlock()
	mu.Lock()
	eventText := strings.Join(events, "\n")
	mu.Unlock()

	places := map[string]string{
		"an answer": strings.Join(answers, "\n"), "the log": logText, "the audit trail": string(trailText),
		"an event": eventText, "a span": strings.Join(spans, "\n"),
		// The answers that made a signing key hold it, and nothing else.
		"the answer that made a key": made + rotated,
	}
	for place, text := range places {
		marks := []string{concealedMark, "MARKED"}
		if place != "the answer that made a key" {
			marks = append(marks, keys...)
		}
		for _, mark := range marks {
			if strings.Contains(text, mark) {
				t.Errorf("%s holds %q", place, mark)
			}
		}
	}

	// And on the drives: the state of bbolt, the log of Raft, its
	// snapshots, the stored answers of repeated requests - every file the
	// node wrote, once it has stopped.
	if err := n.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The origin of the endpoint is kept in the clear: finding it shows that
	// the walk reads what the node stored.
	files, origins := 0, 0
	stored := os.DirFS(dir)
	err = fs.WalkDir(stored, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := fs.ReadFile(stored, name)
		if err != nil {
			return err
		}
		files++
		if bytes.Contains(content, []byte("hooks.example.com:8443")) {
			origins++
		}
		for _, mark := range append([]string{concealedMark, "MARKED"}, keys...) {
			if bytes.Contains(content, []byte(mark)) {
				t.Errorf("%s holds %q", name, mark)
			}
		}
		return nil
	})
	if err != nil || files == 0 || origins == 0 {
		t.Fatalf("walking the data of the node: %d files, %d with the origin of the target, %v", files, origins, err)
	}
}
