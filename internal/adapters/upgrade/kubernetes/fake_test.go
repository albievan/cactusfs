package kubernetes

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// The namespace and the token of the fake.
const (
	testNamespace = "store"
	firstToken    = "token-one"
	secondToken   = "token-two"
	oldImage      = "registry.example:5000/cactusfs:0.7.0-12"
	oldVersion    = "0.7.0-12"
)

// fakeSet is a StatefulSet of the fake.
type fakeSet struct {
	rv          int
	annotations map[string]string
	labels      map[string]string
	images      map[string]string // container name -> image
}

// fakePod is a pod of the fake.
type fakePod struct {
	image string
}

// recorded is a request the fake was sent.
type recorded struct {
	Method, Path, ContentType, Token, Body string
}

// fakeAPI is an in-cluster API of StatefulSets, pods and evictions, as far
// as the upgrader uses one. An evicted pod is made again at once with the
// image of its StatefulSet; whether it is ready is decided by the test.
type fakeAPI struct {
	t      *testing.T
	server *httptest.Server
	dir    string

	mu       sync.Mutex
	sets     map[string]*fakeSet
	pods     map[string]*fakePod
	requests []recorded
	// evict429 is how many evictions of a pod are answered 429 first.
	evict429 map[string]int
	// unready are images that never become ready (an image that cannot be
	// pulled); hold keeps every pod of a new image unready until let go.
	unready map[string]bool
	hold    bool
	// versions are what a node of a ready pod of the image reports.
	versions map[string]string
	// conflictOnLock answers the next patch with a resourceVersion 409.
	conflictOnLock bool
	// failPatch answers patches of these StatefulSets 500.
	failPatch map[string]bool
	// onRequest, when set, is called with each request, outside the lock.
	onRequest func(recorded)
}

// newFakeAPI starts a fake whose nodes are ids, each at oldImage, and a
// token directory that trusts it.
func newFakeAPI(t *testing.T, ids ...string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{
		t:         t,
		sets:      map[string]*fakeSet{},
		pods:      map[string]*fakePod{},
		evict429:  map[string]int{},
		unready:   map[string]bool{},
		versions:  map[string]string{oldImage: "0.7.0"},
		failPatch: map[string]bool{},
	}
	for _, id := range ids {
		f.sets[id] = &fakeSet{
			rv:          1,
			annotations: map[string]string{},
			labels:      map[string]string{VersionLabel: oldVersion, "cactusfs.io/node": id},
			images:      map[string]string{"cactusfs": oldImage, "sidecar": "registry.example:5000/sidecar:1"},
		}
		f.pods[id+"-0"] = &fakePod{image: oldImage}
	}
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	f.dir = t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw})
	f.write(caFile, string(ca))
	f.write(tokenFile, firstToken+"\n")
	f.write(namespaceFile, testNamespace)
	return f
}

// write writes a file of the token directory.
func (f *fakeAPI) write(name, content string) {
	f.t.Helper()
	// Written beside and renamed, as the kubelet does: a reader sees the
	// old file or the new one.
	tmp := filepath.Join(f.dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(f.dir, name)); err != nil {
		f.t.Fatal(err)
	}
}

// status answers a Status object.
func status(w http.ResponseWriter, code int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "reason": reason, "message": "secret words " + reason})
}

// serve answers a request.
func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec := recorded{
		Method: r.Method, Path: r.URL.Path, ContentType: r.Header.Get("Content-Type"),
		Token: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), Body: string(body),
	}
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	hook := f.onRequest
	f.mu.Unlock()
	if hook != nil {
		hook(rec)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if rec.Token == "" {
		status(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	setPrefix := "/apis/apps/v1/namespaces/" + testNamespace + "/statefulsets/"
	podPrefix := "/api/v1/namespaces/" + testNamespace + "/pods/"
	switch {
	case strings.HasPrefix(r.URL.Path, setPrefix):
		name := strings.TrimPrefix(r.URL.Path, setPrefix)
		set, ok := f.sets[name]
		if !ok {
			status(w, http.StatusNotFound, "NotFound")
			return
		}
		switch r.Method {
		case http.MethodGet:
			f.writeSet(w, name, set)
		case http.MethodPatch:
			f.patch(w, name, set, rec.ContentType, body)
		default:
			status(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
		}
	case strings.HasPrefix(r.URL.Path, podPrefix) && strings.HasSuffix(r.URL.Path, "/eviction"):
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, podPrefix), "/eviction")
		f.evict(w, r.Method, name, body)
	case strings.HasPrefix(r.URL.Path, podPrefix) && r.Method == http.MethodGet:
		name := strings.TrimPrefix(r.URL.Path, podPrefix)
		p, ok := f.pods[name]
		if !ok {
			status(w, http.StatusNotFound, "NotFound")
			return
		}
		ready := f.readyLocked(p)
		cond := "False"
		if ready {
			cond = "True"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{"name": name},
			"spec":     map[string]any{"containers": []any{map[string]any{"name": "cactusfs", "image": p.image}}},
			"status": map[string]any{
				"conditions":        []any{map[string]any{"type": "Initialized", "status": "True"}, map[string]any{"type": "Ready", "status": cond}},
				"containerStatuses": []any{map[string]any{"name": "cactusfs", "ready": ready}},
			},
		})
	default:
		status(w, http.StatusNotFound, "NotFound")
	}
}

// readyLocked reports whether a pod is ready.
func (f *fakeAPI) readyLocked(p *fakePod) bool {
	if f.unready[p.image] {
		return false
	}
	return !f.hold || p.image == oldImage
}

// writeSet answers a StatefulSet.
func (f *fakeAPI) writeSet(w http.ResponseWriter, name string, set *fakeSet) {
	var containers []any
	for _, c := range []string{"sidecar", "cactusfs"} {
		containers = append(containers, map[string]any{"name": c, "image": set.images[c]})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"metadata": map[string]any{"name": name, "resourceVersion": strconv.Itoa(set.rv), "annotations": set.annotations},
		"spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{"labels": set.labels},
			"spec":     map[string]any{"containers": containers},
		}},
	})
}

// patch applies a merge patch or a strategic merge patch, as far as the
// upgrader writes them.
func (f *fakeAPI) patch(w http.ResponseWriter, name string, set *fakeSet, contentType string, body []byte) {
	if f.failPatch[name] {
		status(w, http.StatusInternalServerError, "InternalError")
		return
	}
	var p struct {
		Metadata struct {
			ResourceVersion *string            `json:"resourceVersion"`
			Annotations     map[string]*string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Template struct {
				Metadata struct {
					Labels map[string]*string `json:"labels"`
				} `json:"metadata"`
				Spec struct {
					Containers []container `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		status(w, http.StatusBadRequest, "BadRequest")
		return
	}
	switch contentType {
	case mergePatch:
		if p.Metadata.ResourceVersion != nil && (f.conflictOnLock || *p.Metadata.ResourceVersion != strconv.Itoa(set.rv)) {
			f.conflictOnLock = false
			status(w, http.StatusConflict, "Conflict")
			return
		}
	case strategicPatch:
		for _, c := range p.Spec.Template.Spec.Containers {
			if _, ok := set.images[c.Name]; !ok {
				status(w, http.StatusUnprocessableEntity, "Invalid")
				return
			}
			set.images[c.Name] = c.Image
		}
		for k, v := range p.Spec.Template.Metadata.Labels {
			if v == nil {
				delete(set.labels, k)
			} else {
				set.labels[k] = *v
			}
		}
	default:
		status(w, http.StatusUnsupportedMediaType, "UnsupportedMediaType")
		return
	}
	for k, v := range p.Metadata.Annotations {
		if v == nil {
			delete(set.annotations, k)
		} else {
			set.annotations[k] = *v
		}
	}
	set.rv++
	f.writeSet(w, name, set)
}

// evict evicts a pod: it is made again at once with its StatefulSet's image.
func (f *fakeAPI) evict(w http.ResponseWriter, method, name string, body []byte) {
	want := fmt.Sprintf(`{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":%q,"namespace":%q}}`, name, testNamespace)
	if method != http.MethodPost || string(body) != want {
		f.t.Errorf("eviction of %s: %s %s, want POST %s", name, method, body, want)
		status(w, http.StatusBadRequest, "BadRequest")
		return
	}
	p, ok := f.pods[name]
	if !ok {
		status(w, http.StatusNotFound, "NotFound")
		return
	}
	if f.evict429[name] > 0 {
		f.evict429[name]--
		status(w, http.StatusTooManyRequests, "TooManyRequests")
		return
	}
	set := f.sets[strings.TrimSuffix(name, "-0")]
	p.image = set.images["cactusfs"]
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"kind":"Status","status":"Success"}`))
}

// nodeBuild is the settings RPC of the fake: what the node of a ready pod
// reports.
func (f *fakeAPI) nodeBuild(_ context.Context, node string) (cluster.Build, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pods[node+"-0"]
	if !ok || !f.readyLocked(p) {
		return cluster.Build{}, errors.New("the node does not answer")
	}
	v, ok := f.versions[p.image]
	if !ok {
		return cluster.Build{}, errors.New("the node does not answer")
	}
	return cluster.Build{Version: v, Build: "7"}, nil
}

// setHold keeps the pods of new images unready, or lets them go.
func (f *fakeAPI) setHold(hold bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold = hold
}

// do runs change under the fake's lock.
func (f *fakeAPI) do(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

// requestsOf returns the requests of a method whose path ends so.
func (f *fakeAPI) requestsOf(method, suffix string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requestsOfLocked(method, suffix)
}

// requestsOfLocked is requestsOf with the fake's lock held.
func (f *fakeAPI) requestsOfLocked(method, suffix string) []recorded {
	var out []recorded
	for _, r := range f.requests {
		if r.Method == method && strings.HasSuffix(r.Path, suffix) {
			out = append(out, r)
		}
	}
	return out
}

// all returns every request.
func (f *fakeAPI) all() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

// set returns a copy of what a StatefulSet holds.
func (f *fakeAPI) set(name string) fakeSet {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sets[name]
	out := fakeSet{rv: s.rv, annotations: map[string]string{}, labels: map[string]string{}, images: map[string]string{}}
	for k, v := range s.annotations {
		out.annotations[k] = v
	}
	for k, v := range s.labels {
		out.labels[k] = v
	}
	for k, v := range s.images {
		out.images[k] = v
	}
	return out
}

// podImage returns the image a pod runs.
func (f *fakeAPI) podImage(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pods[name].image
}

// waitFor waits until cond holds, or fails the test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
