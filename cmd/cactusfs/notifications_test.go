package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/adapters/events/webhook"
	"github.com/albievan/cactusfs/internal/adapters/sigv4"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain/bucket"
	"github.com/albievan/cactusfs/internal/domain/delivery"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

// received is a request a receiver of a test was sent.
type received struct {
	header http.Header
	path   string
	body   []byte
}

// hookReceiver is a webhook receiver over TLS on loopback that hands every
// request it is sent to the test and answers 204.
type hookReceiver struct {
	server *httptest.Server
	got    chan received
}

func newHookReceiver(t *testing.T) *hookReceiver {
	t.Helper()
	r := &hookReceiver{got: make(chan received, 64)}
	r.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case r.got <- received{header: req.Header.Clone(), path: req.URL.Path, body: body}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	r.server.StartTLS()
	t.Cleanup(r.server.Close)
	return r
}

// caPEM is the certificate of the receiver, which is its own CA, in PEM.
func (r *hookReceiver) caPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.server.Certificate().Raw})
}

// unreachable returns a loopback address nothing listens on: one freeAddr
// found free, of the range no system hands out by itself.
func unreachable(t *testing.T) string {
	t.Helper()
	return freeAddr(t)
}

// notifyConfig is the configuration of a standalone node of the binary
// test, in a directory of its own.
func notifyConfig(t *testing.T) serverConfig {
	t.Helper()
	cfg := nodeConfig(t.TempDir())
	cfg.AdminAddr, cfg.S3Addr = freeAddr(t), freeAddr(t)
	cfg.DevPlaintext = true
	cfg.Region = "us-east-1"
	cfg.DrainDelay, cfg.ShutdownTimeout = 0, 5*time.Second
	cfg.DeliveryTimeout = 5 * time.Second
	// The receiver, and what cannot be reached, are on loopback.
	cfg.EgressAllow = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	return cfg
}

// served is the binary of a test, serving.
type served struct {
	admin, s3 string
	// ready is how long it took from the start to /readyz answering 200.
	ready time.Duration
	logs  *lockedBuffer
	stop  func(t *testing.T)
}

// logText returns what the binary logged so far.
func (s served) logText() string {
	s.logs.mu.Lock()
	defer s.logs.mu.Unlock()
	return s.logs.buf.String()
}

// serveUntilReady starts the binary as runServer does and waits, at most
// 30 s, for it to be ready.
func serveUntilReady(ctx context.Context, t *testing.T, cfg serverConfig) served {
	t.Helper()
	serveCtx, cancel := context.WithCancel(ctx)
	s := served{admin: "http://" + cfg.AdminAddr, s3: "http://" + cfg.S3Addr, logs: &lockedBuffer{buf: &bytes.Buffer{}}}
	done := make(chan error, 1)
	began := time.Now()
	go func() { done <- runServer(serveCtx, cfg, s.logs, io.Discard) }()
	stopped := false
	s.stop = func(t *testing.T) {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("runServer = %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("the node did not stop")
		}
	}
	t.Cleanup(func() { s.stop(t) })
	for statusOf(ctx, t, s.admin+"/readyz") != http.StatusOK {
		if time.Since(began) > 30*time.Second {
			t.Fatalf("the node did not become ready within 30 s; log:\n%s", s.logText())
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.ready = time.Since(began)
	return s
}

// putObject puts an object through the S3 API, signed with SigV4, and
// returns how long the answer took.
func putObject(ctx context.Context, t *testing.T, s3, region string, credentials app.Credentials, key string) time.Duration {
	t.Helper()
	body := []byte("the contents of " + key)
	sum := sha256.Sum256(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s3+"/photos/"+key, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := sigv4.SignRequest(req, credentials.AccessKeyID, []byte(credentials.SecretKey), region, hex.EncodeToString(sum[:]), time.Now()); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	took := time.Since(began)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT %s = %d %s", key, resp.StatusCode, answer)
	}
	return took
}

// newDeployment opens a node of cfg once, makes the bucket "photos", lets
// define does what else the deployment needs, and closes it: what a binary
// started on cfg finds.
func newDeployment(t *testing.T, cfg serverConfig, define func(n *node, root iam.Principal)) app.Credentials {
	t.Helper()
	ctx := t.Context()
	var notice bytes.Buffer
	n, err := openNode(ctx, cfg, newTelemetry(t), slog.New(slog.DiscardHandler), &notice)
	if err != nil {
		t.Fatal(err)
	}
	credentials := shownCredentials(t, notice.String())
	root := iam.Principal{AccessKeyID: credentials.AccessKeyID, Root: true}
	if err := n.buckets.Create(ctx, root, "photos"); err != nil {
		t.Fatal(err)
	}
	if define != nil {
		define(n, root)
	}
	n.auditor.Flush(ctx)
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	return credentials
}

// TestANodeNotifiesThroughItsTargets is the binary test of PLAN 8.2.5. Two
// standalone nodes are started as the binary starts, one after the other:
// one without targets, and one whose bucket notifies a webhook that
// answers, a webhook that cannot be reached and a NATS server that cannot
// be reached. The second becomes ready no later than the first plus half a
// --delivery-timeout, and its puts answer no later than the first's plus
// half a --delivery-timeout - nothing waits for a target. The webhook that
// answers receives a signed notification of each put, at its sealed path
// and with its bearer secret; the two that cannot be reached are retried
// after failures of class connect; no secret of a target is in the log.
func TestANodeNotifiesThroughItsTargets(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	keys := []string{"cactus one.jpg", "dir/two.jpg", "three+four.jpg"}

	// The node without targets: how long starting and writing take here.
	plain := notifyConfig(t)
	plainCredentials := newDeployment(t, plain, nil)
	baseline := serveUntilReady(ctx, t, plain)
	var baselinePut time.Duration
	for _, key := range keys {
		baselinePut = max(baselinePut, putObject(ctx, t, baseline.s3, plain.Region, plainCredentials, key))
	}
	baseline.stop(t)

	// The node with targets.
	receiver := newHookReceiver(t)
	cfg := notifyConfig(t)
	const (
		bearer         = "MARKED-bearer-1c9e"
		deadStreamMark = "MARKED-stream-77a0"
	)
	var signingKey string
	deadHook, deadStream := unreachable(t), unreachable(t)
	credentials := newDeployment(t, cfg, func(n *node, root iam.Principal) {
		made, err := n.targets.Put(ctx, root, app.TargetInput{
			Name: "hook", Kind: delivery.KindWebhook, Endpoint: receiver.server.URL + "/hooks/in", CAPEM: receiver.caPEM(),
			Buckets: []string{"photos"}, Auth: delivery.Auth{Mode: delivery.AuthBearer}, Secret: bearer,
		})
		if err != nil {
			t.Fatal(err)
		}
		signingKey = made.SigningKey
		for _, in := range []app.TargetInput{
			{Name: "dead-hook", Kind: delivery.KindWebhook, Endpoint: "https://" + deadHook + "/hooks/in", Buckets: []string{"photos"}},
			{
				Name: "dead-stream", Kind: delivery.KindNATS, Endpoint: "tls://" + deadStream, Subject: "cactusfs.events", Buckets: []string{"photos"},
				Auth: delivery.Auth{Mode: delivery.AuthToken}, Secret: deadStreamMark,
			},
		} {
			if _, err := n.targets.Put(ctx, root, in); err != nil {
				t.Fatalf("Put %s: %v", in.Name, err)
			}
		}
		notifications := app.NewNotificationService(app.NotificationServiceConfig{
			Buckets: n.meta, Targets: n.meta, Authz: n.authorizer, Auditor: n.auditor,
		})
		var rules []bucket.NotificationRule
		for _, name := range []string{"hook", "dead-hook", "dead-stream"} {
			rules = append(rules, bucket.NotificationRule{Target: name, On: []delivery.Event{delivery.EventPut}})
		}
		if _, err := notifications.Set(ctx, root, "photos", bucket.NotificationConfig{Rules: rules}); err != nil {
			t.Fatal(err)
		}
	})
	notifying := serveUntilReady(ctx, t, cfg)
	// Well under one delivery timeout: nothing at the start waits for a
	// target.
	slack := cfg.DeliveryTimeout / 2
	if notifying.ready > baseline.ready+slack {
		t.Errorf("ready after %s with targets that cannot be reached, %s without targets: more than %s apart", notifying.ready, baseline.ready, slack)
	}
	for _, key := range keys {
		if took := putObject(ctx, t, notifying.s3, cfg.Region, credentials, key); took > baselinePut+slack {
			t.Errorf("PUT %s took %s with targets, at most %s without: more than %s apart", key, took, baselinePut, slack)
		}
	}

	// Each put is told to the webhook that answers, signed.
	seen := map[string]bool{}
	deadline := time.After(30 * time.Second)
	for len(seen) < len(keys) {
		select {
		case <-deadline:
			t.Fatalf("the receiver was told of %d of %d puts within 30 s; log:\n%s", len(seen), len(keys), notifying.logText())
		case got := <-receiver.got:
			key := wantSignedNotification(t, got, signingKey, bearer)
			seen[key] = true
		}
	}
	for _, key := range keys {
		if !seen[key] {
			t.Errorf("no notification of %q", key)
		}
	}
	// The targets that cannot be reached were tried, and are retried:
	// their publishers were made, and failed to connect.
	for _, name := range []string{"dead-hook", "dead-stream"} {
		url := notifying.admin + "/api/v1/targets/" + name + "/status"
		state := targetState(ctx, t, url, credentials)
		for began := time.Now(); state.LastErrorClass != string(delivery.ClassConnect); state = targetState(ctx, t, url, credentials) {
			if time.Since(began) > 30*time.Second {
				t.Fatalf("%s within 30 s: %+v, want a target retried after a failure of class connect", name, state)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if state.State != ports.DeliveryRetrying || state.Pending == 0 {
			t.Errorf("%s: %+v, want %s with items pending", name, state, ports.DeliveryRetrying)
		}
	}

	notifying.stop(t)
	// Neither the secrets of the targets nor the signing key are in the
	// log.
	text := notifying.logText()
	for _, secret := range []string{bearer, deadStreamMark, signingKey, "/hooks/in"} {
		if strings.Contains(text, secret) {
			t.Errorf("the log holds a secret of a target: %q", secret)
		}
	}
}

// wantSignedNotification checks one request a receiver got - the path, the
// bearer secret, the signature under the signing key, the type and the S3
// record of a put - and returns the key it tells of.
func wantSignedNotification(t *testing.T, got received, signingKey, bearer string) string {
	t.Helper()
	if got.path != "/hooks/in" {
		t.Errorf("a notification to the path %q", got.path)
	}
	if got.header.Get("Authorization") != "Bearer "+bearer {
		t.Error("a notification without the target's bearer secret")
	}
	timestamp, signature := got.header.Get(webhook.HeaderTimestamp), got.header.Get(webhook.HeaderSignature)
	if !webhook.Verify([]byte(signingKey), timestamp, got.body, signature) {
		t.Errorf("the signature %q of a notification does not verify", signature)
	}
	if typ := got.header.Get(webhook.HeaderEventType); typ != ports.MessageNotificationObject {
		t.Errorf("a notification of type %q", typ)
	}
	if got.header.Get(webhook.HeaderEventID) == "" {
		t.Error("a notification without an ID")
	}
	var record struct {
		Records []struct {
			EventName string `json:"eventName"`
			S3        struct {
				Bucket struct {
					Name string `json:"name"`
				} `json:"bucket"`
				Object struct {
					Key string `json:"key"`
				} `json:"object"`
			} `json:"s3"`
		} `json:"Records"`
	}
	if err := json.Unmarshal(got.body, &record); err != nil || len(record.Records) != 1 {
		t.Fatalf("a notification that is not one S3 record: %s (%v)", got.body, err)
	}
	r := record.Records[0]
	if put, _ := app.NotificationEventName(delivery.EventPut); r.EventName != put || r.S3.Bucket.Name != "photos" {
		t.Errorf("a notification of %s in %s", r.EventName, r.S3.Bucket.Name)
	}
	key, err := url.QueryUnescape(r.S3.Object.Key)
	if err != nil {
		t.Errorf("the key %q of a notification: %v", r.S3.Object.Key, err)
	}
	return key
}

// shownTargetState is what GET /targets/{name}/status says of a target.
type shownTargetState struct {
	State          string `json:"state"`
	Pending        uint64 `json:"pending"`
	LastErrorClass string `json:"last_error_class"`
}

// targetState asks the management API how a target stands, with the API
// key as the bearer credential.
func targetState(ctx context.Context, t *testing.T, url string, credentials app.Credentials) shownTargetState {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credentials.AccessKeyID+":"+credentials.SecretKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var state shownTargetState
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

// statusOf returns the status of GET url, or 0 when there is no answer.
func statusOf(ctx context.Context, t *testing.T, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}
