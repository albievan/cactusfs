package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/albievan/cactusfs/api/openapi"
	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/adapters/httpsession"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/adapters/upgrade/off"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
)

// updatesMark is in the path of the update source and in the password of
// the tests below. It is put together here so that it is written nowhere
// whole.
func updatesMark() string {
	return strings.Join([]string{"MARK", "9c41", "UPDATES"}, "-")
}

// withUpdates is a node's configuration that checks a source - at an
// address nothing answers - and replaces its own binary, a file of the
// test's.
func withUpdates(t *testing.T, dir string) serverConfig {
	t.Helper()
	cfg := nodeConfig(dir)
	cfg.UpdateSource = "https://127.0.0.1:9/" + updatesMark() + "/releases"
	cfg.UpdateChannel, cfg.UpgradeMode = "stable", "binary"
	exe := filepath.Join(t.TempDir(), "cactusfs")
	writeTestFile(t, exe, "not a binary")
	cfg.executable = exe
	return cfg
}

// TestUpdatesAreWiredThroughTheirDecorators: the source, the verifier and
// the upgrader of a node are each reached through its decorator, whatever
// upgrade-mode is; and the upgrader is the one of the mode.
func TestUpdatesAreWiredThroughTheirDecorators(t *testing.T) {
	t.Parallel()
	rootOf := func(s started) iam.Principal {
		return iam.Principal{Root: true, AccessKeyID: shownCredentials(t, s.notice).AccessKeyID}
	}
	first := mustStart(t, withUpdates(t, t.TempDir()))
	standalone, root := first.node, rootOf(first)
	wantWired(t, standalone, "ports.ReleaseSource", "ports.ReleaseVerifier", "ports.Upgrader")
	view, err := standalone.updates.Get(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	// A node on its own may replace its binary: it is one node.
	if !view.SourceConfigured || view.Mode != release.ModeBinary || !view.UpgradeAvailable || view.Current.String() != buildinfo.Get().Version {
		t.Errorf("a node on its own in upgrade-mode binary: %+v", view)
	}

	second := mustStart(t, nodeConfig(t.TempDir()))
	plain, root := second.node, rootOf(second)
	wantWired(t, plain, "ports.ReleaseVerifier", "ports.Upgrader")
	view, err = plain.updates.Get(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if view.SourceConfigured || view.Mode != release.ModeOff || view.UpgradeAvailable || view.UnavailableReason != off.Reason ||
		view.Channel != release.ChannelStable {
		t.Errorf("a node told nothing of updates: %+v", view)
	}
	if _, err := plain.updates.Check(t.Context(), root); !errors.Is(err, domain.ErrUpdatesOff) {
		t.Errorf("Check without a source = %v", err)
	}
}

// TestUpdateSettingsThatCannotBeHonouredStopTheNode: what is checked when
// the node opens - the files of update-ca and update-trusted-root, the token
// of upgrade-mode kubernetes - stops it when it is wrong, and names no part
// of the update source.
func TestUpdateSettingsThatCannotBeHonouredStopTheNode(t *testing.T) {
	t.Parallel()
	files := t.TempDir()
	notPEM, notRoot := filepath.Join(files, "ca.pem"), filepath.Join(files, "root.json")
	writeTestFile(t, notPEM, "no certificate here")
	writeTestFile(t, notRoot, `{"mediaType": "not a trusted root"}`)
	tokens := t.TempDir()
	for name, tt := range map[string]struct {
		change func(*serverConfig)
		want   string
	}{
		"kubernetes without the token": {
			change: func(c *serverConfig) {
				c.UpgradeMode, c.kubernetesAPIDir = "kubernetes", filepath.Join(files, "no-such-directory")
			},
			want: "upgrade.fromConsole.enabled",
		},
		"a CA file that holds no certificate": {change: func(c *serverConfig) { c.UpdateCA = notPEM }, want: "update-ca"},
		"a CA file that is not there":         {change: func(c *serverConfig) { c.UpdateCA = filepath.Join(files, "absent.pem") }, want: "update-ca"},
		"a trusted root that does not parse":  {change: func(c *serverConfig) { c.UpdateTrustedRoot = notRoot }, want: "update-trusted-root"},
		"a binary of no known path":           {change: func(c *serverConfig) { c.executable = "" }, want: "upgrade-mode binary"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := withUpdates(t, t.TempDir())
			tt.change(&cfg)
			_, err := startNode(t, cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("openNode = %v, want an error naming %q", err, tt.want)
			}
			if strings.Contains(err.Error(), updatesMark()) {
				t.Errorf("the error names the update source: %v", err)
			}
		})
	}

	// With the token's directory there, the node starts in the mode; what
	// else it lacks, its upgrader says when asked.
	cfg := nodeConfig(t.TempDir())
	cfg.UpgradeMode, cfg.kubernetesAPIDir = "kubernetes", tokens
	s := mustStart(t, cfg)
	view, err := s.node.updates.Get(t.Context(), iam.Principal{Root: true, AccessKeyID: shownCredentials(t, s.notice).AccessKeyID})
	if err != nil || view.Mode != release.ModeKubernetes || view.UpgradeAvailable || view.UnavailableReason == "" {
		t.Errorf("a node of upgrade-mode kubernetes: %+v, %v", view, err)
	}
}

// fixedNodes is a view of the cluster that has the nodes it was given.
type fixedNodes struct {
	nodes []cluster.Node
	err   error
}

func (f fixedNodes) Topology(context.Context) (cluster.Topology, error) {
	return cluster.Topology{}, nil
}

func (f fixedNodes) Nodes(context.Context) ([]cluster.Node, error) {
	return slices.Clone(f.nodes), f.err
}

// TestUpgradeOrder: the nodes are moved those that hold no vote first, then
// the voters, and the node that runs the upgrade last.
func TestUpgradeOrder(t *testing.T) {
	t.Parallel()
	voter, storage := cluster.Roles{cluster.RoleMeta, cluster.RoleGateway}, cluster.Roles{cluster.RoleStorage}
	view := fixedNodes{nodes: []cluster.Node{
		{ID: "n1", Roles: voter}, {ID: "n2", Roles: storage}, {ID: "n3", Roles: voter}, {ID: "n4", Roles: storage}, {ID: "n5", Roles: voter},
	}}
	for self, want := range map[string][]string{
		"n1": {"n2", "n4", "n3", "n5", "n1"},
		"n4": {"n2", "n1", "n3", "n5", "n4"},
	} {
		got, err := upgradeOrder(t.Context(), view, self)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("from %s: %v, %v; want %v", self, got, err, want)
		}
	}
	if _, err := upgradeOrder(t.Context(), fixedNodes{err: domain.ErrUnavailable}, "n1"); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("a view that cannot be read: %v", err)
	}
}

// TestWhatAnUpgradeAsksOfTheNode: the build of the node itself is the one
// it says of itself, another's is asked over node RPC - which a node on its
// own has not - and the node is ready only once the server has given it its
// readiness, and then as /readyz says.
func TestWhatAnUpgradeAsksOfTheNode(t *testing.T) {
	t.Parallel()
	n := mustStart(t, nodeConfig(t.TempDir())).node
	build, err := n.nodeBuild(t.Context(), n.id, nil)
	if err != nil || build.Version != buildinfo.Get().Version {
		t.Errorf("the build of the node itself = %+v, %v", build, err)
	}
	if _, err := n.nodeBuild(t.Context(), "another", nil); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("the build of another node, on a node on its own = %v", err)
	}
	if err := n.ready(t.Context()); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("ready before the server gave the readiness = %v", err)
	}
	n.readiness.Store(app.NewHealthService(failingCheck{}))
	if err := n.ready(t.Context()); !errors.Is(err, domain.ErrUnavailable) || !strings.Contains(err.Error(), "metadata") {
		t.Errorf("ready while a check fails = %v", err)
	}
	n.readiness.Store(app.NewHealthService(n.meta))
	if err := n.ready(t.Context()); err != nil {
		t.Errorf("ready = %v", err)
	}
}

// failingCheck is a readiness check that fails.
type failingCheck struct{}

func (failingCheck) Name() string                { return "metadata" }
func (failingCheck) Check(context.Context) error { return errors.New("no leader") }

var _ ports.ReadinessCheck = failingCheck{}

// TestThePasswordOfAnUpgradeAndTheSourceAreNowhere is the secret scan of
// PLAN P.9 on a node as the binary wires it: the password typed with an
// upgrade, and the path of the update source, both marked, are in no answer
// of the operations of releases, no line of the log, no audit record and no
// span - through a check the source does not answer, an upgrade by an API
// key, and one by a console session with a wrong password.
func TestThePasswordOfAnUpgradeAndTheSourceAreNowhere(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	mark := updatesMark()
	password := mark + "-the-password"
	cfg := withUpdates(t, t.TempDir())
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
	n.startBackground(ctx, cfg)
	handler := tel.TraceAdmin(adminapi.NewHandler(adminapi.Config{
		Buckets: n.buckets, Objects: n.objects, Search: n.search, IAM: n.iam, Sessions: n.sessions, Cluster: n.cluster,
		Settings: n.settings, Targets: n.targets, Updates: n.updates, Idempotency: n.idempotency, Identities: n.identities,
		Clock: n.clock, Logger: logger, SpecYAML: openapi.AdminYAML(), SpecJSON: openapi.AdminJSON(), Build: buildinfo.Get(),
		Observe: tel.ObserveAdmin,
	}))
	credentials := shownCredentials(t, notice.String())
	call := func(method, target string, headers map[string]string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var content []byte
		if body != nil {
			if content, err = json.Marshal(body); err != nil {
				t.Fatal(err)
			}
		}
		request := httptest.NewRequestWithContext(ctx, method, adminapi.BasePath+target, bytes.NewReader(content))
		request.Header.Set("Content-Type", "application/json")
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		answer := httptest.NewRecorder()
		handler.ServeHTTP(answer, request)
		return answer
	}
	rootKey := map[string]string{"Authorization": "Bearer " + credentials.AccessKeyID + ":" + credentials.SecretKey}

	// The console user of a new deployment signs in (ADR-0014).
	signed := call(http.MethodPost, "/session", nil, map[string]any{"username": app.DefaultAdminName, "password": app.DefaultAdminName})
	if signed.Code != http.StatusOK {
		t.Fatalf("sign-in = %d %s", signed.Code, signed.Body)
	}
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(signed.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	console := map[string]string{
		"Cookie": strings.SplitN(signed.Header().Get("Set-Cookie"), ";", 2)[0], httpsession.CSRFHeader: session.CSRFToken,
	}

	var answers []string
	for _, c := range []struct {
		method, target string
		headers        map[string]string
		body           any
		want           int
	}{
		{http.MethodGet, "/updates", rootKey, nil, http.StatusOK},
		// Nothing answers at the source's address: the check is made,
		// and fails by its class.
		{http.MethodPost, "/updates/check", rootKey, nil, http.StatusOK},
		{http.MethodGet, "/updates", console, nil, http.StatusOK},
		{http.MethodPost, "/upgrade", rootKey, map[string]any{"version": "0.7.1", "password": password}, http.StatusForbidden},
		{http.MethodPost, "/upgrade", console, map[string]any{"version": "0.7.1", "password": password}, http.StatusForbidden},
		{http.MethodGet, "/upgrade", console, nil, http.StatusOK},
		{http.MethodGet, "/audit?limit=1000", rootKey, nil, http.StatusOK},
	} {
		answer := call(c.method, c.target, c.headers, c.body)
		if answer.Code != c.want {
			t.Errorf("%s %s = %d, want %d: %s", c.method, c.target, answer.Code, c.want, answer.Body)
		}
		answers = append(answers, answer.Header().Get("Location")+answer.Body.String())
	}
	var checked struct {
		LastError string `json:"last_error"`
	}
	if err := json.Unmarshal([]byte(answers[1]), &checked); err != nil || checked.LastError == "" {
		t.Errorf("the check of a source nothing answers at: %s", answers[1])
	}

	// The auditor of the node writes in the background too: a record it
	// has taken from its queue is not one Flush finds there, so the trail
	// is read until it holds what the requests did.
	var trailText []byte
	for deadline := time.Now().Add(10 * time.Second); ; {
		n.auditor.Flush(ctx)
		trail, err := n.iam.AuditLog(ctx, iam.Principal{Root: true, AccessKeyID: credentials.AccessKeyID}, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if trailText, err = json.Marshal(trail); err != nil {
			t.Fatal(err)
		}
		missing := slices.DeleteFunc([]string{string(iam.ActionCheckForUpdates), string(iam.ActionUpgradeCluster), string(app.ActionConfirmPassword)},
			func(action string) bool { return strings.Contains(string(trailText), action) })
		if len(missing) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the audit trail has no record of %v", missing)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var spans []string
	for _, span := range recorder.Ended() {
		spans = append(spans, fmt.Sprintf("%s %v %v %v", span.Name(), span.Attributes(), span.Status(), span.Events()))
	}
	if !strings.Contains(strings.Join(spans, "\n"), "release_source.Releases") {
		t.Errorf("no span of the update source among %d spans", len(spans))
	}
	logs.mu.Lock()
	logText := logs.buf.String()
	logs.mu.Unlock()
	if !strings.Contains(logText, "the update source could not be read") {
		t.Error("the log does not say that the source could not be read")
	}
	for place, text := range map[string]string{
		"an answer": strings.Join(answers, "\n"), "the log": logText, "the audit trail": string(trailText), "a span": strings.Join(spans, "\n"),
	} {
		if strings.Contains(text, mark) {
			t.Errorf("%s holds the mark of the password or of the update source", place)
		}
	}
	if _, err := os.Stat(cfg.executable); err != nil {
		t.Errorf("the executable: %v", err)
	}
}
