package kubernetes

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// The nodes of the tests, as the chart names them, and the node the
// upgrader runs on.
var fourNodes = []string{"store-meta-0", "store-meta-1", "store-meta-2", "store-data-0"}

const self = "store-meta-1"

// newUpgrader returns an upgrader of the fake's nodes, run on self, with
// short times; change, when not nil, changes its Config first.
func newUpgrader(t *testing.T, f *fakeAPI, nodes []string, change func(*Config)) *Upgrader {
	t.Helper()
	cfg := Config{
		APIBase:     f.server.URL,
		TokenDir:    f.dir,
		Nodes:       func(context.Context) ([]string, error) { return slices.Clone(nodes), nil },
		Self:        self,
		NodeBuild:   f.nodeBuild,
		Ready:       func(context.Context) error { return nil },
		NodeTimeout: 2 * time.Second,
		Poll:        5 * time.Millisecond,
	}
	if change != nil {
		change(&cfg)
	}
	u, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return u
}

// target returns the release the tests install, and the image it is
// installed as; the fake's nodes of that image report its version.
func target(t *testing.T, f *fakeAPI) (release.Verified, string) {
	t.Helper()
	v := porttest.FakeVerifiedImage(t, "0.7.1")
	image := "registry.example:5000/cactusfs@" + v.Digest()
	f.do(func() { f.versions[image] = "0.7.1" })
	return v, image
}

// recorder keeps what a run reports.
type recorder struct {
	mu      sync.Mutex
	reports []release.Progress
	ended   chan struct{}
}

func newRecorder() *recorder { return &recorder{ended: make(chan struct{})} }

func (r *recorder) report(p release.Progress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, p)
	if p.State.Ended() {
		close(r.ended)
	}
}

// last waits for the run to end and returns its last report.
func (r *recorder) last(t *testing.T) release.Progress {
	t.Helper()
	select {
	case <-r.ended:
	case <-time.After(30 * time.Second):
		t.Fatal("the run did not end")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reports[len(r.reports)-1]
}

// evictedPods returns the names of the pods of the eviction requests.
func evictedPods(f *fakeAPI) []string {
	var out []string
	for _, r := range f.requestsOf(http.MethodPost, "/eviction") {
		parts := strings.Split(r.Path, "/")
		out = append(out, parts[len(parts)-2])
	}
	return out
}

// imagePatches returns the strategic merge patches, by StatefulSet.
func imagePatches(f *fakeAPI) []recorded {
	var out []recorded
	for _, r := range f.requestsOf(http.MethodPatch, "") {
		if r.ContentType == strategicPatch {
			out = append(out, r)
		}
	}
	return out
}

// nameOf returns the last element of a path.
func nameOf(path string) string { return path[strings.LastIndexByte(path, '/')+1:] }

func TestNewRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()
	nodes := func(context.Context) ([]string, error) { return nil, nil }
	build := func(context.Context, string) (cluster.Build, error) { return cluster.Build{}, nil }
	ready := func(context.Context) error { return nil }
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"no Nodes", func(c *Config) { c.Nodes = nil }},
		{"no NodeBuild", func(c *Config) { c.NodeBuild = nil }},
		{"no Ready", func(c *Config) { c.Ready = nil }},
		{"no Self", func(c *Config) { c.Self = "" }},
		{"a Self that is no name", func(c *Config) { c.Self = "../pods" }},
		{"a namespace that is no name", func(c *Config) { c.Namespace = "A/B" }},
		{"a negative time", func(c *Config) { c.NodeTimeout = -time.Second }},
		{"an API over http", func(c *Config) { c.APIBase = "http://10.0.0.1:443" }},
		{"an API with a path", func(c *Config) { c.APIBase = "https://10.0.0.1:443/api" }},
		{"an API with a user", func(c *Config) { c.APIBase = "https://u:p@10.0.0.1:443" }},
		{"an API that does not parse", func(c *Config) { c.APIBase = "https://[::1" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{APIBase: "https://10.0.0.1:443", Nodes: nodes, NodeBuild: build, Ready: ready, Self: "store-meta-0"}
			tt.change(&cfg)
			if _, err := New(cfg); !errors.Is(err, domain.ErrInvalidArgument) {
				t.Errorf("New = %v, want ErrInvalidArgument", err)
			}
		})
	}
	cfg := Config{APIBase: "https://10.0.0.1:443/", Nodes: nodes, NodeBuild: build, Ready: ready, Self: "store-meta-0"}
	u, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if u.cfg.TokenDir != DefaultAPIDir || u.cfg.Container != DefaultContainer || u.cfg.NodeTimeout != DefaultNodeTimeout || u.cfg.Poll != DefaultPoll {
		t.Errorf("defaults = %+v", u.cfg)
	}
	if u.Mode() != release.ModeKubernetes {
		t.Errorf("Mode = %s", u.Mode())
	}
}

func TestAPIBaseFromEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"KUBERNETES_SERVICE_HOST": "10.43.0.1", "KUBERNETES_SERVICE_PORT": "443"}, "https://10.43.0.1:443"},
		{map[string]string{"KUBERNETES_SERVICE_HOST": "fd00::1", "KUBERNETES_SERVICE_PORT": "443"}, "https://[fd00::1]:443"},
		{map[string]string{"KUBERNETES_SERVICE_HOST": "10.43.0.1"}, ""},
		{map[string]string{"KUBERNETES_SERVICE_PORT": "443"}, ""},
		{map[string]string{"KUBERNETES_SERVICE_HOST": "", "KUBERNETES_SERVICE_PORT": "443"}, ""},
	}
	for _, tt := range tests {
		lookup := func(k string) (string, bool) { v, ok := tt.env[k]; return v, ok }
		if got := APIBaseFromEnv(lookup); got != tt.want {
			t.Errorf("APIBaseFromEnv(%v) = %q, want %q", tt.env, got, tt.want)
		}
	}
}

func TestAvailability(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*testing.T, *fakeAPI, *Config)
		ok     bool
		reason string
	}{
		{"everything there", nil, true, ""},
		{"no API", func(_ *testing.T, _ *fakeAPI, c *Config) { c.APIBase = "" }, false, "KUBERNETES_SERVICE_HOST"},
		{"no token directory", func(t *testing.T, _ *fakeAPI, c *Config) { c.TokenDir = filepath.Join(t.TempDir(), "none") }, false, "upgrade.fromConsole.enabled is off"},
		{"no namespace", func(_ *testing.T, f *fakeAPI, _ *Config) { f.write(namespaceFile, "Not A Name") }, false, "namespace"},
		{"the namespace given", func(_ *testing.T, f *fakeAPI, c *Config) {
			f.write(namespaceFile, "elsewhere")
			c.Namespace = testNamespace
		}, true, ""},
		{"no nodes", func(_ *testing.T, _ *fakeAPI, c *Config) {
			c.Nodes = func(context.Context) ([]string, error) { return nil, nil }
		}, false, "no node"},
		{"the topology unknown", func(_ *testing.T, _ *fakeAPI, c *Config) {
			c.Nodes = func(context.Context) ([]string, error) { return nil, errors.New("no leader") }
		}, false, "not known"},
		{"a node that is no name", func(_ *testing.T, _ *fakeAPI, c *Config) {
			c.Nodes = func(context.Context) ([]string, error) { return []string{"store-meta-0", "../../secrets/x"}, nil }
		}, false, "cannot name a StatefulSet"},
		{"a StatefulSet it may not read", func(_ *testing.T, f *fakeAPI, _ *Config) {
			f.do(func() { delete(f.sets, "store-data-0") })
		}, false, "did not let this node read the StatefulSet store-data-0: it answered 404 NotFound"},
		{"no such container", func(_ *testing.T, _ *fakeAPI, c *Config) { c.Container = "other" }, false, "has no container other"},
		{"a CA that is not the API's", func(t *testing.T, f *fakeAPI, _ *Config) { f.write(caFile, otherCA(t)) }, false, "could not be reached"},
		{"a CA file that holds none", func(_ *testing.T, f *fakeAPI, _ *Config) { f.write(caFile, "nothing") }, false, "could not be made"},
		{"an empty token", func(_ *testing.T, f *fakeAPI, _ *Config) { f.write(tokenFile, "\n") }, false, "could not be made"},
		{"the API unreachable", func(_ *testing.T, _ *fakeAPI, c *Config) { c.APIBase = "https://127.0.0.1:1" }, false, "could not be reached"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeAPI(t, fourNodes...)
			u := newUpgrader(t, f, fourNodes, func(c *Config) {
				if tt.change != nil {
					tt.change(t, f, c)
				}
			})
			ok, reason, steps := u.Availability(t.Context())
			if ok != tt.ok || !strings.Contains(reason, tt.reason) {
				t.Errorf("Availability = %v, %q; want %v, %q", ok, reason, tt.ok, tt.reason)
			}
			if len(steps) == 0 || !strings.Contains(strings.Join(steps, "\n"), "image.digest") || !strings.Contains(strings.Join(steps, "\n"), "upgrade.fromConsole.enabled=true") {
				t.Errorf("instructions = %q", steps)
			}
			if strings.Contains(reason, firstToken) || strings.Contains(reason, "secret words") {
				t.Errorf("the reason %q holds what it must not", reason)
			}
		})
	}
}

// otherCA returns a CA certificate, in PEM, that signed nothing the fake
// serves.
func otherCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "another CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestUnavailableUpgrader(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, fourNodes...)
	u := newUpgrader(t, f, fourNodes, func(c *Config) { c.APIBase = "" })
	v, _ := target(t, f)
	porttest.UnavailableUpgraderContract(t, u, v)
	if n := len(f.all()); n != 0 {
		t.Errorf("an unavailable upgrader sent %d requests", n)
	}
}

func TestUpgraderContract(t *testing.T) {
	t.Parallel()
	porttest.UpgraderContract(t, func(t *testing.T) (ports.Upgrader, release.Verified, func()) {
		f := newFakeAPI(t, fourNodes...)
		f.setHold(true)
		u := newUpgrader(t, f, fourNodes, func(c *Config) { c.NodeTimeout = time.Minute })
		v, _ := target(t, f)
		return u, v, func() { f.setHold(false) }
	})
}

func TestStartRefusesWhatItDoesNotInstall(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, fourNodes...)
	u := newUpgrader(t, f, fourNodes, nil)
	tests := []struct {
		name   string
		target release.Verified
		want   error
	}{
		{"nothing verified", release.Verified{}, domain.ErrReleaseNotVerified},
		{"a binary", porttest.FakeVerifiedBinary(t, "0.7.1"), domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		if err := u.Start(t.Context(), tt.target, "user/alice", nil); !errors.Is(err, tt.want) {
			t.Errorf("Start of %s = %v, want %v", tt.name, err, tt.want)
		}
	}
	if n := len(f.all()); n != 0 {
		t.Errorf("a refused start sent %d requests", n)
	}
}

func TestAnUpgradeMovesEveryNodeInOrderThisNodeLast(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, fourNodes...)
	u := newUpgrader(t, f, fourNodes, nil)
	v, image := target(t, f)
	rec := newRecorder()
	if err := u.Start(t.Context(), v, "user/alice", rec.report); err != nil {
		t.Fatalf("Start: %v", err)
	}
	last := rec.last(t)
	if last.State != release.StateDone || last.Target != v.Version() || last.StartedBy != "user/alice" {
		t.Fatalf("last report = %+v", last)
	}
	order := []string{"store-meta-0", "store-meta-2", "store-data-0", "store-meta-1"}
	var got []string
	for _, n := range last.Nodes {
		got = append(got, n.ID)
		if n.State != release.NodeDone || n.Version != "0.7.1" || n.Build != "7" {
			t.Errorf("node %+v", n)
		}
	}
	if !slices.Equal(got, order) {
		t.Errorf("nodes = %v, want %v", got, order)
	}

	// Each node patched once, evicted once, in order, this node last.
	var patched []string
	for _, p := range imagePatches(f) {
		patched = append(patched, nameOf(p.Path))
	}
	var pods []string
	for _, n := range order {
		pods = append(pods, n+"-0")
	}
	if !slices.Equal(patched, order) || !slices.Equal(evictedPods(f), pods) {
		t.Errorf("patched %v, evicted %v; want %v and their pods", patched, evictedPods(f), order)
	}

	// The patch of the image, exactly.
	prev := `{\"image\":\"registry.example:5000/cactusfs:0.7.0-12\",\"version\":\"0.7.0-12\"}`
	want := `{"metadata":{"annotations":{"cactusfs.io/upgrade-previous":"` + prev + `"}},"spec":{"template":{"metadata":{"labels":{"app.kubernetes.io/version":"0.7.1"}},"spec":{"containers":[{"image":"` + image + `","name":"cactusfs"}]}}}}`
	if p := imagePatches(f)[0]; p.Body != want || p.Path != "/apis/apps/v1/namespaces/store/statefulsets/store-meta-0" {
		t.Errorf("image patch %s\n%s\nwant\n%s", p.Path, p.Body, want)
	}
	for _, n := range order {
		s := f.set(n)
		if s.images["cactusfs"] != image || s.images["sidecar"] != "registry.example:5000/sidecar:1" || s.labels[VersionLabel] != "0.7.1" || s.labels["cactusfs.io/node"] != n {
			t.Errorf("StatefulSet %s = %+v", n, s)
		}
		if f.podImage(n+"-0") != image {
			t.Errorf("pod of %s runs %s", n, f.podImage(n+"-0"))
		}
	}

	// The lock, taken under the resourceVersion and given back with the
	// result.
	merges := f.requestsOf(http.MethodPatch, "/statefulsets/store-data-0")
	var locks []recorded
	for _, m := range merges {
		if m.ContentType == mergePatch {
			locks = append(locks, m)
		}
	}
	if len(locks) < 2 {
		t.Fatalf("merge patches of the lock: %v", locks)
	}
	var take struct {
		Metadata struct {
			ResourceVersion string            `json:"resourceVersion"`
			Annotations     map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(locks[0].Body), &take); err != nil || take.Metadata.ResourceVersion != "1" || len(take.Metadata.Annotations) != 1 {
		t.Fatalf("lock patch %s: %v", locks[0].Body, err)
	}
	var lock lockRecord
	if err := json.Unmarshal([]byte(take.Metadata.Annotations[LockAnnotation]), &lock); err != nil {
		t.Fatal(err)
	}
	if len(lock.Run) != 32 || lock.Target != "0.7.1" || lock.Image != image || lock.Holder != self || lock.StartedBy != "user/alice" || lock.StartedAt.IsZero() || !lock.Heartbeat.Equal(lock.StartedAt) {
		t.Errorf("lock = %+v", lock)
	}
	end := locks[len(locks)-1].Body
	if !strings.HasPrefix(end, `{"metadata":{"annotations":{"cactusfs.io/upgrade":null,"cactusfs.io/upgrade-result":"`) || !strings.Contains(end, `"resourceVersion":"`) {
		t.Errorf("release patch %s", end)
	}
	s := f.set("store-data-0")
	if _, ok := s.annotations[LockAnnotation]; ok {
		t.Error("the lock is still there")
	}
	result, ok := parseResult(s.annotations)
	if !ok || result.State != release.StateDone || result.Run != lock.Run || result.Node != "" {
		t.Errorf("result = %+v, %v", result, ok)
	}

	// Any node reads the same from the cluster.
	other := newUpgrader(t, f, fourNodes, func(c *Config) { c.Self = "store-data-0" })
	p, err := other.Progress(t.Context())
	if err != nil || p.State != release.StateDone || p.Target != v.Version() || p.StartedBy != "user/alice" || len(p.Nodes) != 4 || p.Nodes[3].ID != self {
		t.Errorf("Progress = %+v, %v", p, err)
	}
	for _, n := range p.Nodes {
		if n.State != release.NodeDone {
			t.Errorf("node %+v", n)
		}
	}

	// Every request carried the token, and the token only.
	for _, r := range f.all() {
		if r.Token != firstToken {
			t.Errorf("%s %s carried %q", r.Method, r.Path, r.Token)
		}
	}
}

func TestTheLockIsTakenUnderItsResourceVersion(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, fourNodes...)
	f.do(func() { f.conflictOnLock = true })
	u := newUpgrader(t, f, fourNodes, nil)
	v, _ := target(t, f)
	if err := u.Start(t.Context(), v, "user/alice", nil); !errors.Is(err, domain.ErrUpgradeRunning) {
		t.Fatalf("Start against a lock taken meanwhile = %v", err)
	}
	if n := len(imagePatches(f)) + len(evictedPods(f)); n != 0 {
		t.Errorf("%d nodes touched", n)
	}
	// Nothing runs here: the next Start may begin.
	rec := newRecorder()
	if err := u.Start(t.Context(), v, "user/alice", rec.report); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p := rec.last(t); p.State != release.StateDone {
		t.Errorf("last = %+v", p)
	}
}

// lockOf writes a lock to the first node's StatefulSet.
func lockOf(f *fakeAPI, lock lockRecord) {
	f.do(func() { f.sets["store-data-0"].annotations[LockAnnotation] = encode(lock) })
}

func TestALockHeldByAnotherNode(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	tests := []struct {
		name    string
		lock    func(image string) lockRecord
		atStart bool // every node at the lock's image before
		running bool // Start is refused
	}{
		{"alive", func(image string) lockRecord {
			return lockRecord{Run: "aaaa", Target: "0.7.1", Image: image, Holder: "store-meta-2", StartedBy: "user/bob", StartedAt: now, Heartbeat: now}
		}, false, true},
		{"stale", func(image string) lockRecord {
			return lockRecord{Run: "aaaa", Target: "0.7.1", Image: image, Holder: "store-meta-2", StartedBy: "user/bob", StartedAt: now.Add(-time.Hour), Heartbeat: now.Add(-5 * time.Second)}
		}, false, false},
		{"of this node, whose process holds no run", func(image string) lockRecord {
			return lockRecord{Run: "aaaa", Target: "0.7.1", Image: image, Holder: self, StartedBy: "user/bob", StartedAt: now, Heartbeat: now}
		}, false, false},
		{"alive, and every node already there", func(image string) lockRecord {
			return lockRecord{Run: "aaaa", Target: "0.7.1", Image: image, Holder: "store-meta-2", StartedBy: "user/bob", StartedAt: now, Heartbeat: now}
		}, true, false},
		{"that does not parse", func(string) lockRecord { return lockRecord{} }, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeAPI(t, fourNodes...)
			u := newUpgrader(t, f, fourNodes, nil)
			v, image := target(t, f)
			lockOf(f, tt.lock(image))
			if tt.atStart {
				f.do(func() {
					for _, n := range fourNodes {
						f.sets[n].images["cactusfs"] = image
						f.pods[n+"-0"].image = image
					}
				})
			}
			rec := newRecorder()
			err := u.Start(t.Context(), v, "user/alice", rec.report)
			if tt.running {
				if !errors.Is(err, domain.ErrUpgradeRunning) {
					t.Fatalf("Start = %v, want ErrUpgradeRunning", err)
				}
				p, err := u.Progress(t.Context())
				if err != nil || p.State != release.StateRunning || p.StartedBy != "user/bob" || p.Nodes[3].ID != "store-meta-2" {
					t.Errorf("Progress = %+v, %v", p, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Start = %v", err)
			}
			if p := rec.last(t); p.State != release.StateDone || p.StartedBy != "user/alice" {
				t.Errorf("last = %+v", p)
			}
			if tt.atStart && len(imagePatches(f))+len(evictedPods(f)) != 0 {
				t.Errorf("nodes at the target were touched: %v", evictedPods(f))
			}
		})
	}
}

func TestAnEvictionTheBudgetRefusesIsAskedAgain(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, fourNodes...)
	f.do(func() { f.evict429["store-meta-2-0"] = 3 })
	u := newUpgrader(t, f, fourNodes, nil)
	v, _ := target(t, f)
	rec := newRecorder()
	if err := u.Start(t.Context(), v, "user/alice", rec.report); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p := rec.last(t); p.State != release.StateDone {
		t.Fatalf("last = %+v", p)
	}
	n := 0
	for _, pod := range evictedPods(f) {
		if pod == "store-meta-2-0" {
			n++
		}
	}
	if n != 4 {
		t.Errorf("store-meta-2-0 was asked to be evicted %d times, want 4", n)
	}
}

func TestANodeThatDoesNotComeUpIsPutBackAndTheRunStops(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(f *fakeAPI, image string)
		ready  func(context.Context) error
		reason string
		state  release.NodeState
	}{
		{"a pod never ready", func(f *fakeAPI, image string) { f.unready[image] = true }, nil, "its pod did not become ready on the new image within 300ms", release.NodeRestored},
		{"a node that reports another version", func(f *fakeAPI, image string) { f.versions[image] = "0.7.0" }, nil, "the node did not report the version 0.7.1", release.NodeRestored},
		{"a cluster that is not ready", nil, func(context.Context) error { return errors.New("no quorum") }, "the cluster did not become ready", release.NodeRestored},
		{"a budget that never allows", func(f *fakeAPI, _ string) { f.evict429["store-meta-0-0"] = 1 << 30 }, nil, "the PodDisruptionBudget did not let its pod be evicted within 300ms", release.NodeFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeAPI(t, fourNodes...)
			u := newUpgrader(t, f, fourNodes, func(c *Config) {
				c.NodeTimeout = 300 * time.Millisecond
				if tt.ready != nil {
					c.Ready = tt.ready
				}
			})
			v, image := target(t, f)
			if tt.change != nil {
				f.do(func() { tt.change(f, image) })
			}
			rec := newRecorder()
			if err := u.Start(t.Context(), v, "user/alice", rec.report); err != nil {
				t.Fatalf("Start: %v", err)
			}
			last := rec.last(t)
			if last.State != release.StateFailed {
				t.Fatalf("last = %+v", last)
			}
			first := last.Nodes[0]
			if first.ID != "store-meta-0" || first.State != tt.state || !strings.Contains(first.Reason, tt.reason) {
				t.Errorf("first node = %+v; want %s, %q", first, tt.state, tt.reason)
			}
			for _, n := range last.Nodes[1:] {
				if n.State != release.NodePending {
					t.Errorf("a node after the failure = %+v", n)
				}
			}
			// Only the first node was touched: patched, then put back.
			patches := imagePatches(f)
			if len(patches) != 2 || nameOf(patches[0].Path) != "store-meta-0" || nameOf(patches[1].Path) != "store-meta-0" {
				t.Fatalf("patches: %v", patches)
			}
			wantBack := `{"metadata":{"annotations":{"cactusfs.io/upgrade-previous":null}},"spec":{"template":{"metadata":{"labels":{"app.kubernetes.io/version":"0.7.0-12"}},"spec":{"containers":[{"image":"registry.example:5000/cactusfs:0.7.0-12","name":"cactusfs"}]}}}}`
			if patches[1].Body != wantBack {
				t.Errorf("rollback patch\n%s\nwant\n%s", patches[1].Body, wantBack)
			}
			s := f.set("store-meta-0")
			if s.images["cactusfs"] != oldImage || s.labels[VersionLabel] != oldVersion {
				t.Errorf("store-meta-0 after the rollback = %+v", s)
			}
			for _, pod := range evictedPods(f) {
				if pod != "store-meta-0-0" {
					t.Errorf("%s was evicted", pod)
				}
			}
			if tt.state == release.NodeRestored && f.podImage("store-meta-0-0") != oldImage {
				t.Errorf("the pod runs %s after the rollback", f.podImage("store-meta-0-0"))
			}
			// The lock is given back with the failure, which any node reads.
			if _, ok := f.set("store-data-0").annotations[LockAnnotation]; ok {
				t.Error("the lock is still there")
			}
			p, err := u.Progress(t.Context())
			if err != nil || p.State != release.StateFailed || p.Nodes[0].State != tt.state || p.Nodes[0].Reason != first.Reason {
				t.Errorf("Progress = %+v, %v", p, err)
			}
		})
	}
}

func TestARunCutOffIsResumedSkippingNodesAtTheTarget(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, fourNodes...)
	u := newUpgrader(t, f, fourNodes, nil)
	v, image := target(t, f)
	prev := encode(previousRecord{Image: oldImage, Version: oldVersion})
	f.do(func() {
		// store-meta-0 was moved and is up; store-meta-2 was patched and
		// its pod evicted, and is coming up.
		for _, n := range []string{"store-meta-0", "store-meta-2"} {
			f.sets[n].images["cactusfs"] = image
			f.sets[n].labels[VersionLabel] = "0.7.1"
			f.sets[n].annotations[PreviousAnnotation] = prev
			f.pods[n+"-0"].image = image
		}
	})
	rec := newRecorder()
	if err := u.Start(t.Context(), v, "user/alice", rec.report); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if p := rec.last(t); p.State != release.StateDone {
		t.Fatalf("last = %+v", p)
	}
	var patched []string
	for _, p := range imagePatches(f) {
		patched = append(patched, nameOf(p.Path))
	}
	if !slices.Equal(patched, []string{"store-data-0", self}) || !slices.Equal(evictedPods(f), []string{"store-data-0-0", self + "-0"}) {
		t.Errorf("patched %v, evicted %v", patched, evictedPods(f))
	}
}

func TestAResumedNodeThatFailsIsPutBackToWhatItRanBefore(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, fourNodes...)
	u := newUpgrader(t, f, fourNodes, func(c *Config) { c.NodeTimeout = 200 * time.Millisecond })
	v, image := target(t, f)
	f.do(func() {
		f.unready[image] = true
		f.sets["store-meta-0"].images["cactusfs"] = image
		f.sets["store-meta-0"].annotations[PreviousAnnotation] = encode(previousRecord{Image: "registry.example:5000/cactusfs:0.6.9-1"})
		f.sets["store-meta-0"].labels[VersionLabel] = "0.7.1"
		f.pods["store-meta-0-0"].image = image
	})
	rec := newRecorder()
	if err := u.Start(t.Context(), v, "user/alice", rec.report); err != nil {
		t.Fatalf("Start: %v", err)
	}
	last := rec.last(t)
	if last.State != release.StateFailed || last.Nodes[0].State != release.NodeRestored {
		t.Fatalf("last = %+v", last)
	}
	s := f.set("store-meta-0")
	if _, has := s.labels[VersionLabel]; has || s.images["cactusfs"] != "registry.example:5000/cactusfs:0.6.9-1" {
		t.Errorf("store-meta-0 = %+v", s)
	}
	if !slices.Equal(evictedPods(f), []string{"store-meta-0-0"}) {
		t.Errorf("evicted %v: a pod already on the new image is not evicted to move it, only to put it back", evictedPods(f))
	}

	// What it ran before not known: left where it is, and said so.
	f2 := newFakeAPI(t, fourNodes...)
	u2 := newUpgrader(t, f2, fourNodes, func(c *Config) { c.NodeTimeout = 200 * time.Millisecond })
	_, image2 := target(t, f2)
	f2.do(func() {
		f2.unready[image2] = true
		f2.sets["store-meta-0"].images["cactusfs"] = image2
		f2.pods["store-meta-0-0"].image = image2
	})
	rec2 := newRecorder()
	if err := u2.Start(t.Context(), v, "user/alice", rec2.report); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if n := rec2.last(t).Nodes[0]; n.State != release.NodeFailed || !strings.Contains(n.Reason, "what it ran before is not known") {
		t.Errorf("node = %+v", n)
	}
}

func TestAPatchThatFailsStopsTheRun(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, fourNodes...)
	f.do(func() { f.failPatch["store-meta-2"] = true })
	u := newUpgrader(t, f, fourNodes, nil)
	v, _ := target(t, f)
	rec := newRecorder()
	if err := u.Start(t.Context(), v, "user/alice", rec.report); err != nil {
		t.Fatalf("Start: %v", err)
	}
	last := rec.last(t)
	if last.State != release.StateFailed || last.Nodes[0].State != release.NodeDone ||
		last.Nodes[1].State != release.NodeFailed || !strings.Contains(last.Nodes[1].Reason, "could not be patched: it answered 500 InternalError") {
		t.Errorf("last = %+v", last)
	}
	if !slices.Equal(evictedPods(f), []string{"store-meta-0-0"}) {
		t.Errorf("evicted %v", evictedPods(f))
	}
}

func TestTheTokenIsReadForEveryRequest(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, fourNodes...)
	f.setHold(true)
	u := newUpgrader(t, f, fourNodes, func(c *Config) { c.NodeTimeout = 20 * time.Second })
	v, _ := target(t, f)
	rec := newRecorder()
	if err := u.Start(t.Context(), v, "user/alice", rec.report); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the first eviction", func() bool { return len(evictedPods(f)) > 0 })
	f.write(tokenFile, secondToken)
	seen := len(f.all())
	f.setHold(false)
	if p := rec.last(t); p.State != release.StateDone {
		t.Fatalf("last = %+v", p)
	}
	all := f.all()
	if all[0].Token != firstToken {
		t.Errorf("the first request carried %q", all[0].Token)
	}
	for _, r := range all[seen+1:] {
		if r.Token != secondToken {
			t.Errorf("%s %s after the token changed carried %q", r.Method, r.Path, r.Token)
		}
	}
}

func TestTheHeartbeatKeepsTheLockAndALostLockStopsTheRun(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, fourNodes...)
	f.setHold(true)
	clock := porttest.NewClock(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC))
	u := newUpgrader(t, f, fourNodes, func(c *Config) {
		c.NodeTimeout = 20 * time.Second
		c.Clock = clock
	})
	v, _ := target(t, f)
	rec := newRecorder()
	if err := u.Start(t.Context(), v, "user/alice", rec.report); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the first eviction", func() bool { return len(evictedPods(f)) > 0 })
	// A heartbeat every quarter of a node's time.
	clock.Advance(6 * time.Second)
	waitFor(t, "a heartbeat", func() bool {
		lock, _, _ := parseLock(f.set("store-data-0").annotations)
		return lock.Heartbeat.Equal(clock.Now())
	})
	if p, err := u.Progress(t.Context()); err != nil || p.State != release.StateRunning {
		t.Errorf("Progress = %+v, %v", p, err)
	}
	// Another node takes the lock over: this run stops and touches nothing.
	lock, _, _ := parseLock(f.set("store-data-0").annotations)
	lock.Run = "bbbb"
	lockOf(f, lock)
	clock.Advance(6 * time.Second)
	last := rec.last(t)
	if last.State != release.StateFailed || last.Nodes[0].State != release.NodeFailed || !strings.Contains(last.Nodes[0].Reason, "another node took it over") {
		t.Errorf("last = %+v", last)
	}
	held, _, _ := parseLock(f.set("store-data-0").annotations)
	if held.Run != "bbbb" {
		t.Errorf("the lock of the other run was touched: %+v", held)
	}
	if len(imagePatches(f)) != 1 {
		t.Errorf("patches after the lock was lost: %v", imagePatches(f))
	}
}

// strategicPatchesLocked counts the strategic merge patches - of an image -
// with the fake's lock held.
func strategicPatchesLocked(f *fakeAPI) int {
	n := 0
	for _, r := range f.requestsOfLocked(http.MethodPatch, "") {
		if r.ContentType == strategicPatch {
			n++
		}
	}
	return n
}

// TestTimeRunningOutAfterTheLockWasTakenOverPutsNothingBack: the budget
// refuses the eviction of the first node until its time runs out, and
// meanwhile another node takes the lock over. The run stops as one whose
// lock was lost - not as one refused by the budget - and puts nothing back:
// no patch to the old image, no second eviction, the other run's lock and
// the node's record of what it ran untouched.
func TestTimeRunningOutAfterTheLockWasTakenOverPutsNothingBack(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, fourNodes...)
	f.do(func() { f.evict429["store-meta-0-0"] = 1 << 30 })
	// The clock stands still: no heartbeat falls due, so only the node's
	// time running out can end the evictions.
	clock := porttest.NewClock(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC))
	u := newUpgrader(t, f, fourNodes, func(c *Config) {
		c.NodeTimeout = 300 * time.Millisecond
		c.Clock = clock
	})
	v, _ := target(t, f)
	var once sync.Once
	var patchesAfter, evictionsAfter int
	var taken bool
	f.onRequest = func(r recorded) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if taken && r.Method == http.MethodPatch {
			patchesAfter++
		}
		if taken && r.Method == http.MethodPost && strategicPatchesLocked(f) > 1 {
			evictionsAfter++
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/eviction") {
			once.Do(func() {
				lock, _, _ := parseLock(f.sets["store-data-0"].annotations)
				lock.Run = "bbbb"
				f.sets["store-data-0"].annotations[LockAnnotation] = encode(lock)
				f.sets["store-data-0"].rv++
				taken = true
			})
		}
	}
	rec := newRecorder()
	if err := u.Start(t.Context(), v, "user/alice", rec.report); err != nil {
		t.Fatalf("Start: %v", err)
	}
	last := rec.last(t)
	if last.State != release.StateFailed || last.Nodes[0].State != release.NodeFailed || !strings.Contains(last.Nodes[0].Reason, "another node took it over") {
		t.Errorf("last = %+v", last)
	}
	if strings.Contains(last.Nodes[0].Reason, "PodDisruptionBudget") {
		t.Errorf("a lost lock was reported as the budget's doing: %q", last.Nodes[0].Reason)
	}
	f.do(func() {
		if patchesAfter != 0 || evictionsAfter != 0 {
			t.Errorf("after the lock was taken over: %d patches, %d evictions after a patch back", patchesAfter, evictionsAfter)
		}
	})
	if n := len(imagePatches(f)); n != 1 {
		t.Errorf("%d image patches, want the one forward", n)
	}
	s := f.set("store-meta-0")
	if s.images["cactusfs"] == oldImage || parsePrevious(s.annotations).Image != oldImage {
		t.Errorf("the first node was put back: %+v", s)
	}
	if held, _, _ := parseLock(f.set("store-data-0").annotations); held.Run != "bbbb" {
		t.Errorf("the lock of the other run was touched: %+v", held)
	}
	if f.podImage("store-meta-0-0") != oldImage {
		t.Errorf("the pod was evicted: it runs %s", f.podImage("store-meta-0-0"))
	}
}

// TestARunSeenDoneByAnotherNodeEndsDone: while the holder waits for the
// cluster to be ready on its last node, another node reads the progress as
// done and writes nothing - the holder is alive. A process of the holder's
// own node (as when it was restarted) replaces the lock with the run's
// result; the run that was still going ends done, puts nothing back and
// writes nothing more.
func TestARunSeenDoneByAnotherNodeEndsDone(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, fourNodes...)
	var ready sync.Mutex
	notReady := false
	clock := porttest.NewClock(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC))
	u := newUpgrader(t, f, fourNodes, func(c *Config) {
		c.NodeTimeout = 20 * time.Second
		c.Clock = clock
		c.Ready = func(context.Context) error {
			ready.Lock()
			defer ready.Unlock()
			if notReady {
				return errors.New("not yet")
			}
			return nil
		}
	})
	v, _ := target(t, f)
	f.onRequest = func(r recorded) {
		// The last node, this one, is evicted: the cluster is not ready
		// from then on, as far as this run sees.
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/"+self+"-0/eviction") {
			ready.Lock()
			notReady = true
			ready.Unlock()
		}
	}
	rec := newRecorder()
	if err := u.Start(t.Context(), v, "user/alice", rec.report); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "this node's eviction", func() bool { return slices.Contains(evictedPods(f), self+"-0") })
	patches := len(f.requestsOf(http.MethodPatch, ""))
	other := newUpgrader(t, f, fourNodes, func(c *Config) {
		c.Self = "store-data-0"
		c.NodeTimeout = 20 * time.Second
		c.Clock = clock
	})
	if p, err := other.Progress(t.Context()); err != nil || p.State != release.StateDone {
		t.Fatalf("Progress of another node = %+v, %v", p, err)
	}
	if n := len(f.requestsOf(http.MethodPatch, "")); n != patches {
		t.Fatalf("another node wrote %d patches while the holder is alive: %+v", n-patches, f.requestsOf(http.MethodPatch, "")[patches:])
	}
	restarted := newUpgrader(t, f, fourNodes, func(c *Config) {
		c.NodeTimeout = 20 * time.Second
		c.Clock = clock
	})
	if p, err := restarted.Progress(t.Context()); err != nil || p.State != release.StateDone {
		t.Fatalf("Progress of the holder's node = %+v, %v", p, err)
	}
	patches = len(f.requestsOf(http.MethodPatch, ""))
	clock.Advance(6 * time.Second)
	last := rec.last(t)
	if last.State != release.StateDone {
		t.Fatalf("last = %+v", last)
	}
	for _, n := range last.Nodes {
		if n.State != release.NodeDone {
			t.Errorf("node %+v", n)
		}
	}
	if n := len(f.requestsOf(http.MethodPatch, "")); n != patches {
		t.Errorf("%d patches after the run was settled", n-patches)
	}
	result, ok := parseResult(f.set("store-data-0").annotations)
	if !ok || result.State != release.StateDone || result.StartedBy != "user/alice" {
		t.Errorf("result = %+v, %v", result, ok)
	}
}

// TestSettleAtStart: a node that starts with a lock whose run has every
// node at its target - the holder, restarted by moving itself last -
// replaces it with the result "done"; with no lock, or none of a run that
// is done, it writes nothing; without the API it asks nothing.
func TestSettleAtStart(t *testing.T) {
	t.Parallel()
	t.Run("a run that is done", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, fourNodes...)
		u := newUpgrader(t, f, fourNodes, nil)
		_, image := target(t, f)
		now := time.Now().UTC()
		lockOf(f, lockRecord{Run: "aaaa", Target: "0.7.1", Image: image, Holder: self, StartedBy: "user/bob", StartedAt: now, Heartbeat: now})
		// The nodes come up a little after the node starts.
		f.setHold(true)
		f.do(func() {
			for _, n := range fourNodes {
				f.sets[n].images["cactusfs"] = image
				f.pods[n+"-0"].image = image
			}
		})
		done := make(chan struct{})
		go func() {
			defer close(done)
			u.Settle(t.Context())
		}()
		time.Sleep(20 * time.Millisecond)
		if _, ok := f.set("store-data-0").annotations[LockAnnotation]; !ok {
			t.Error("the lock was replaced before every node was up")
		}
		f.setHold(false)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Settle did not return")
		}
		s := f.set("store-data-0")
		result, ok := parseResult(s.annotations)
		if _, locked := s.annotations[LockAnnotation]; locked || !ok || result.Run != "aaaa" || result.State != release.StateDone {
			t.Errorf("annotations = %v", s.annotations)
		}
	})
	t.Run("no lock", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, fourNodes...)
		u := newUpgrader(t, f, fourNodes, nil)
		u.Settle(t.Context())
		if n := len(f.requestsOf(http.MethodPatch, "")); n != 0 {
			t.Errorf("%d patches", n)
		}
	})
	t.Run("a run not done gives up after twice a node's time", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, fourNodes...)
		u := newUpgrader(t, f, fourNodes, func(c *Config) { c.NodeTimeout = 50 * time.Millisecond })
		_, image := target(t, f)
		now := time.Now().UTC()
		lockOf(f, lockRecord{Run: "aaaa", Target: "0.7.1", Image: image, Holder: self, StartedAt: now, Heartbeat: now})
		u.Settle(t.Context())
		if _, ok := f.set("store-data-0").annotations[LockAnnotation]; !ok {
			t.Error("the lock of a run not done was removed")
		}
	})
	t.Run("without the API", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, fourNodes...)
		for _, change := range []func(*Config){
			func(c *Config) { c.APIBase = "" },
			func(c *Config) { c.TokenDir = t.TempDir() },
		} {
			newUpgrader(t, f, fourNodes, change).Settle(t.Context())
		}
		if n := len(f.all()); n != 0 {
			t.Errorf("%d requests", n)
		}
	})
}

func TestProgress(t *testing.T) {
	t.Parallel()
	t.Run("idle without the API", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, fourNodes...)
		for _, change := range []func(*Config){
			func(c *Config) { c.APIBase = "" },
			func(c *Config) { c.TokenDir = t.TempDir() },
		} {
			u := newUpgrader(t, f, fourNodes, change)
			if p, err := u.Progress(t.Context()); err != nil || p.State != release.StateIdle {
				t.Errorf("Progress = %+v, %v", p, err)
			}
		}
	})
	t.Run("a caller who went away", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, fourNodes...)
		u := newUpgrader(t, f, fourNodes, nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := u.Progress(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("Progress = %v", err)
		}
	})
	t.Run("an API that refuses", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, fourNodes...)
		u := newUpgrader(t, f, fourNodes, nil)
		f.write(tokenFile, " ")
		if _, err := u.Progress(t.Context()); err == nil {
			t.Error("Progress without a token = nil")
		}
	})
	t.Run("a stale lock is a run that stopped", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, fourNodes...)
		u := newUpgrader(t, f, fourNodes, nil)
		_, image := target(t, f)
		old := time.Now().Add(-time.Hour).UTC()
		lockOf(f, lockRecord{Run: "aaaa", Target: "0.7.1", Image: image, Holder: "store-meta-2", StartedBy: "user/bob", StartedAt: old, Heartbeat: old})
		f.do(func() {
			f.sets["store-meta-0"].images["cactusfs"] = image
			f.pods["store-meta-0-0"].image = image
			f.sets["store-meta-1"].images["cactusfs"] = image
			f.unready[image] = false
		})
		p, err := u.Progress(t.Context())
		if err != nil || p.State != release.StateFailed {
			t.Fatalf("Progress = %+v, %v", p, err)
		}
		want := []release.NodeState{release.NodeDone, release.NodeFailed, release.NodePending, release.NodePending}
		ids := []string{"store-meta-0", "store-meta-1", "store-data-0", "store-meta-2"}
		for i, n := range p.Nodes {
			if n.ID != ids[i] || n.State != want[i] {
				t.Errorf("node %d = %+v, want %s %s", i, n, ids[i], want[i])
			}
		}
		if !strings.Contains(p.Nodes[1].Reason, "start the upgrade again") {
			t.Errorf("reason = %q", p.Nodes[1].Reason)
		}
	})
	t.Run("a lock whose holder moved itself is done once it is up, and the restarted holder replaces it", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, fourNodes...)
		u := newUpgrader(t, f, fourNodes, nil)
		_, image := target(t, f)
		now := time.Now().UTC()
		lockOf(f, lockRecord{Run: "aaaa", Target: "0.7.1", Image: image, Holder: self, StartedBy: "user/bob", StartedAt: now, Heartbeat: now})
		f.do(func() {
			for _, n := range fourNodes {
				f.sets[n].images["cactusfs"] = image
				f.pods[n+"-0"].image = image
			}
		})
		if p, err := u.Progress(t.Context()); err != nil || p.State != release.StateDone {
			t.Errorf("Progress = %+v, %v", p, err)
		}
		// The lock is replaced by the run's result, under the
		// resourceVersion; the next answer reads the result.
		s := f.set("store-data-0")
		if _, ok := s.annotations[LockAnnotation]; ok {
			t.Error("the lock of a run that is done was left")
		}
		result, ok := parseResult(s.annotations)
		if !ok || result.Run != "aaaa" || result.State != release.StateDone || result.StartedBy != "user/bob" || result.Holder != self || result.EndedAt.IsZero() {
			t.Errorf("result = %+v, %v", result, ok)
		}
		if p, err := u.Progress(t.Context()); err != nil || p.State != release.StateDone || p.StartedBy != "user/bob" {
			t.Errorf("Progress after = %+v, %v", p, err)
		}
	})
	t.Run("a holder that is alive is left to end its run: another node writes nothing", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, fourNodes...)
		u := newUpgrader(t, f, fourNodes, nil)
		_, image := target(t, f)
		now := time.Now().UTC()
		lockOf(f, lockRecord{Run: "aaaa", Target: "0.7.1", Image: image, Holder: "store-meta-2", StartedBy: "user/bob", StartedAt: now, Heartbeat: now})
		f.do(func() {
			for _, n := range fourNodes {
				f.sets[n].images["cactusfs"] = image
				f.pods[n+"-0"].image = image
			}
		})
		if p, err := u.Progress(t.Context()); err != nil || p.State != release.StateDone {
			t.Errorf("Progress = %+v, %v", p, err)
		}
		if n := len(f.requestsOf(http.MethodPatch, "")); n != 0 {
			t.Errorf("%d patches while the holder is alive", n)
		}
		if _, ok := f.set("store-data-0").annotations[LockAnnotation]; !ok {
			t.Error("the lock of a live holder was replaced")
		}
	})
	t.Run("a stale lock of a run that is done is replaced by any node", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, fourNodes...)
		u := newUpgrader(t, f, fourNodes, nil)
		_, image := target(t, f)
		old := time.Now().Add(-time.Hour).UTC()
		lockOf(f, lockRecord{Run: "aaaa", Target: "0.7.1", Image: image, Holder: "store-meta-2", StartedBy: "user/bob", StartedAt: old, Heartbeat: old})
		f.do(func() {
			for _, n := range fourNodes {
				f.sets[n].images["cactusfs"] = image
				f.pods[n+"-0"].image = image
			}
		})
		if p, err := u.Progress(t.Context()); err != nil || p.State != release.StateDone {
			t.Errorf("Progress = %+v, %v", p, err)
		}
		if result, ok := parseResult(f.set("store-data-0").annotations); !ok || result.Run != "aaaa" || result.State != release.StateDone {
			t.Errorf("result = %+v, %v", result, ok)
		}
	})
	t.Run("a lock that moved meanwhile is not replaced", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, fourNodes...)
		f.do(func() { f.conflictOnLock = true })
		u := newUpgrader(t, f, fourNodes, nil)
		_, image := target(t, f)
		now := time.Now().UTC()
		lockOf(f, lockRecord{Run: "aaaa", Target: "0.7.1", Image: image, Holder: self, StartedBy: "user/bob", StartedAt: now, Heartbeat: now})
		f.do(func() {
			for _, n := range fourNodes {
				f.sets[n].images["cactusfs"] = image
				f.pods[n+"-0"].image = image
			}
		})
		if p, err := u.Progress(t.Context()); err != nil || p.State != release.StateDone {
			t.Errorf("Progress = %+v, %v", p, err)
		}
		if _, ok := f.set("store-data-0").annotations[LockAnnotation]; !ok {
			t.Error("the lock was removed although its resourceVersion had changed")
		}
	})
	t.Run("a lock of a run not done is left", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, fourNodes...)
		u := newUpgrader(t, f, fourNodes, nil)
		_, image := target(t, f)
		now := time.Now().UTC()
		lockOf(f, lockRecord{Run: "aaaa", Target: "0.7.1", Image: image, Holder: "store-meta-2", StartedAt: now, Heartbeat: now})
		if p, err := u.Progress(t.Context()); err != nil || p.State != release.StateRunning {
			t.Errorf("Progress = %+v, %v", p, err)
		}
		if _, ok := f.set("store-data-0").annotations[LockAnnotation]; !ok {
			t.Error("the lock of a run under way was removed")
		}
		if n := len(f.requestsOf(http.MethodPatch, "")); n != 0 {
			t.Errorf("%d patches while reading a run under way", n)
		}
	})
	t.Run("a StatefulSet that went away", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, fourNodes...)
		u := newUpgrader(t, f, fourNodes, nil)
		_, image := target(t, f)
		now := time.Now().UTC()
		lockOf(f, lockRecord{Run: "aaaa", Target: "0.7.1", Image: image, Holder: "store-meta-2", StartedAt: now, Heartbeat: now})
		f.do(func() { delete(f.sets, "store-meta-1") })
		if _, err := u.Progress(t.Context()); statusOf(err) != http.StatusNotFound {
			t.Errorf("Progress = %v", err)
		}
	})
}

func TestRepositoryOf(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"ghcr.io/albievan/cactusfs:0.7.0-12":       "ghcr.io/albievan/cactusfs",
		"ghcr.io/albievan/cactusfs@sha256:abcd":    "ghcr.io/albievan/cactusfs",
		"ghcr.io/albievan/cactusfs:0.7.0@sha256:a": "ghcr.io/albievan/cactusfs",
		"registry:5000/cactusfs:1":                 "registry:5000/cactusfs",
		"registry:5000/cactusfs":                   "registry:5000/cactusfs",
		"cactusfs:dev":                             "cactusfs",
		"cactusfs":                                 "cactusfs",
	}
	for in, want := range tests {
		if got := repositoryOf(in); got != want {
			t.Errorf("repositoryOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRecords(t *testing.T) {
	t.Parallel()
	if _, ok, err := parseLock(map[string]string{LockAnnotation: strings.Repeat("x", maxRecord+1)}); !ok || err == nil {
		t.Error("a lock too large is read")
	}
	if _, ok, err := parseLock(map[string]string{LockAnnotation: `{"run":"a","image":"i","target":"x"}`}); !ok || err == nil {
		t.Error("a lock of no version is read")
	}
	if _, ok, err := parseLock(map[string]string{LockAnnotation: `{"target":"0.7.1"}`}); !ok || err == nil {
		t.Error("a lock of no run is read")
	}
	for _, text := range []string{"{", `{"state":"running","target":"0.7.1"}`, `{"state":"done","target":"x"}`, strings.Repeat("x", maxRecord+1)} {
		if _, ok := parseResult(map[string]string{ResultAnnotation: text}); ok {
			t.Errorf("parseResult(%.20q) = ok", text)
		}
	}
	if prev := parsePrevious(map[string]string{PreviousAnnotation: "{"}); prev != (previousRecord{}) {
		t.Errorf("parsePrevious = %+v", prev)
	}
	v, err := release.ParseVersion("0.8.0-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	if got := versionLabel(v); got != "0.8.0-rc.1" {
		t.Errorf("versionLabel = %q", got)
	}
	long, err := release.ParseVersion("0.8.0-" + strings.Repeat("a", 60))
	if err != nil {
		t.Fatal(err)
	}
	if got := versionLabel(long); got != "" {
		t.Errorf("versionLabel of a long version = %q", got)
	}
	if got := (&StatusError{Op: "get x", Status: 409}).Error(); got != "kubernetes: get x: the API answered 409" {
		t.Errorf("Error = %q", got)
	}
	if describe(fmt.Errorf("x: %w", domain.ErrUnavailable)) != "it could not be reached" {
		t.Error("describe of an unreachable API")
	}
}
