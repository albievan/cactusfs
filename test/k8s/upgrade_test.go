//go:build k8s

package k8s_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/albievan/cactusfs/internal/adapters/releases/sigstore/sigstoretest"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain"
	domainrelease "github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/test/integration/releasemirror"
)

// The Secrets of the files of the update source (updates.caSecret and
// updates.trustedRootSecret of the chart).
const (
	updateCASecret   = "cactusfs-update-ca"
	updateRootSecret = "cactusfs-update-root"
)

// The annotations the upgrader writes on the StatefulSet of the first node
// by name (internal/adapters/upgrade/kubernetes).
const (
	lockAnnotation     = "cactusfs.io/upgrade"
	resultAnnotation   = "cactusfs.io/upgrade-result"
	previousAnnotation = "cactusfs.io/upgrade-previous"
)

// nodeTimeout is the time the upgrader gives a node (its default; no
// setting changes it).
const nodeTimeout = 10 * time.Minute

// digestOf matches a digest as containerd prints it.
var digestOf = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// upgradeVersions are the releases of TestUpgradeOnK3s, reckoned from the
// release this tree is (VERSION).
type upgradeVersions struct {
	// current is this tree, image A: what the chart installs.
	current string
	// next is image B: this tree built as the next patch, which the
	// cluster has.
	next string
	// unpullable is the patch after it, whose signed digests.txt names an
	// image the cluster does not have.
	unpullable string
}

func upgradeVersionsOf(t *testing.T) upgradeVersions {
	t.Helper()
	current := buildinfo.Release()
	parts := strings.Split(current, ".")
	if len(parts) != 3 {
		t.Fatalf("VERSION %q is not X.Y.Z", current)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		t.Fatalf("VERSION %q is not X.Y.Z", current)
	}
	at := func(add int) string { return fmt.Sprintf("%s.%s.%d", parts[0], parts[1], patch+add) }
	return upgradeVersions{current: current, next: at(1), unpullable: at(2)}
}

// TestUpgradeOnK3s: an upgrade from the console on a real Kubernetes (PLAN
// P.8, P.11, ADR-0029). The chart is installed with image A, this tree, and
// with an update source: a mirror of the test's, on the engine's network,
// whose lists are signed by a Sigstore of the test as the release workflow
// at the tag of each release, with only the trusted root and the CA given
// to the nodes (updates.trustedRootSecret, updates.caSecret).
//
//   - upgrade-mode off, four nodes: each checks, offers the next release
//     with the chart's instructions, and answers POST /upgrade 501;
//   - upgrade.fromConsole turned on with helm upgrade and the pods rolled
//     by hand, as an operator does;
//   - the default console user installs the next release, image B - this
//     tree built as that version, in the cluster's containerd by the digest
//     the signed digests.txt names: the nodes move one at a time, never two
//     away at once, in the order of the run with the node that runs it last;
//     an object is read while each is away; every node then runs B and
//     reports the release; the lock is replaced by the result "done"; the
//     start is audited, and so is the end, once, by the restarted node that
//     replaced the lock, as the user who started it;
//   - a release whose digest names an image the cluster does not have: the
//     first node does not come up, is put back to B and is Ready again, the
//     others are not touched, the run ends failed, its start and end are
//     audited, and the object is read.
//
// Every request the mirror was sent named the product and carried no
// credential.
func TestUpgradeOnK3s(t *testing.T) {
	b := newBench(t)
	v := upgradeVersionsOf(t)
	b.startCluster()
	image := b.loadProductImage()
	next := b.loadNextImage(v.next)
	unpullable := b.absentImage()
	// Signed when published, and so after the builds: the Sigstore of the
	// test signs for a few minutes from when it is made.
	mirror, trustedRoot := b.startMirror(v, next, unpullable)
	b.provision(image)
	b.provisionUpdates(mirror, trustedRoot)
	b.install(b.updateValues(mirror)...)
	b.admittedAsRestricted()
	b.forwardAdmin()

	bucket := "upgrade-" + b.run
	data := b.roundTrip(bucket, "first")
	b.upgradeModeOff(v)
	b.turnUpgradesOn(bucket, "first", data)
	b.upgradeTo(v, next, bucket, "first", data)
	b.putBack(v, next, unpullable, bucket, "first", data)
	b.stillServes(bucket, []string{"first"})

	requests := mirror.Requests()
	if len(requests) == 0 {
		t.Error("the mirror was sent no request")
	}
	for _, r := range requests {
		if r.Credentials || (r.UserAgent != "cactusfs/"+v.current && r.UserAgent != "cactusfs/"+v.next) {
			t.Errorf("the mirror was sent %s by %q, with credentials: %v", r.Path, r.UserAgent, r.Credentials)
		}
	}
}

// byDigest returns the reference the upgrader gives a node's StatefulSet:
// the repository it runs, by digest.
func byDigest(digest string) string { return loadedImage + "@" + digest }

// loadNextImage builds this tree as release version - image B - and puts it
// into the containerd of k3s by the digest of what was imported, which it
// returns: a pod that names <loadedImage>@<digest> runs it.
func (b *bench) loadNextImage(version string) string {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	built := "cactusfs:test-" + version
	if err := b.buildImage(ctx, built, "VERSION="+version); err != nil {
		t.Fatalf("image B: %v", err)
	}
	engine, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatalf("container engine: %v", err)
	}
	defer func() { _ = engine.Close() }()
	name := b.loaded() + "-next"
	// Only the name this run gives the image is removed, never the image.
	t.Cleanup(func() { b.untagName(name) })
	if err := b.importImage(ctx, engine, built, name, true); err != nil {
		t.Fatal(err)
	}
	// The name by digest the import made - the only one of the repository,
	// for image A was imported by its tag alone. Its digest is what the
	// runtime reports as the image's repository digest; the digest that
	// "ctr images ls" prints beside the tag may be another (the archive's
	// own index), which the runtime does not resolve.
	out, err := b.exec(ctx, "ctr", "-n", "k8s.io", "images", "ls", "-q")
	if err != nil {
		t.Fatal(err)
	}
	var digests []string
	for line := range strings.SplitSeq(out, "\n") {
		if digest, ok := strings.CutPrefix(strings.TrimSpace(line), inContainerd(loadedImage)+"@"); ok {
			digests = append(digests, digest)
		}
	}
	if len(digests) != 1 || !digestOf.MatchString(digests[0]) {
		t.Fatalf("containerd names image B by digest %d times, want once:\n%s", len(digests), tail(out, 4000))
	}
	digest := digests[0]
	// The kubelet asks the runtime for the image by the name a pod gives
	// it: so does this. The runtime learns of an import from containerd's
	// events, a moment later.
	found := func(ctx context.Context) (bool, error) {
		_, err := b.exec(ctx, "crictl", "inspecti", inContainerd(byDigest(digest)))
		return err == nil, err
	}
	if err := within(ctx, time.Minute, 2*time.Second, found); err != nil {
		names, _ := b.exec(ctx, "ctr", "-n", "k8s.io", "images", "ls", "-q")
		known, _ := b.exec(ctx, "crictl", "images", "--digests")
		t.Fatalf("image B was imported, but the runtime does not find it as %s: %v\n---- containerd ----\n%s\n---- the runtime ----\n%s",
			byDigest(digest), err, tail(names, 3000), tail(known, 3000))
	}
	return digest
}

// absentImage returns a digest of no image: the runtime does not find
// <loadedImage>@<it>, which is checked - the control of putBack.
func (b *bench) absentImage() string {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	digest := "sha256:" + randomHex(t, 32)
	if out, err := b.exec(ctx, "crictl", "inspecti", inContainerd(byDigest(digest))); err == nil {
		t.Fatalf("the runtime finds an image by a digest made up for the test: %s", tail(out, 2000))
	}
	return digest
}

// startMirror starts the mirror at this machine's address on the network of
// the k3s container - where its pods reach it - and publishes the next and
// the unpullable release, their digests.txt naming the images given. It
// returns the mirror and the trusted root of the Sigstore that signed them.
func (b *bench) startMirror(v upgradeVersions, next, unpullable string) (*releasemirror.Mirror, []byte) {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	k3sIP, err := b.k3s.ContainerIP(ctx)
	if err != nil {
		t.Fatalf("the address of the k3s container: %v", err)
	}
	// The address this machine sends from to the k3s container: a UDP
	// "connection" sends nothing.
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort(k3sIP, "9"))
	if err != nil {
		t.Fatalf("no route to the k3s container at %s: %v", k3sIP, err)
	}
	from, ok := conn.LocalAddr().(*net.UDPAddr)
	_ = conn.Close()
	if !ok {
		t.Fatalf("the address sent from to the k3s container is %s", conn.LocalAddr())
	}
	local := from.IP.String()

	signer, err := sigstoretest.New()
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := releasemirror.NewWith(signer, releasemirror.Options{Listen: net.JoinHostPort(local, "0"), Host: local})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mirror.Close)
	asset := domainrelease.BinaryAsset("linux", "amd64")
	for _, r := range []releasemirror.Release{
		{Version: v.next, ListDigest: next, Binaries: map[string][]byte{asset: []byte("not a binary: " + v.next)}},
		{Version: v.unpullable, ListDigest: unpullable, Binaries: map[string][]byte{asset: []byte("not a binary: " + v.unpullable)}},
	} {
		if err := mirror.Publish(r); err != nil {
			t.Fatal(err)
		}
	}
	root, err := signer.TrustedRoot()
	if err != nil {
		t.Fatal(err)
	}
	return mirror, root
}

// provisionUpdates makes the Secrets of the files of the update source.
func (b *bench) provisionUpdates(mirror *releasemirror.Mirror, trustedRoot []byte) {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, o := range []object{
		sealed(updateCASecret, "Opaque", map[string][]byte{"ca.crt": mirror.CA()}),
		sealed(updateRootSecret, "Opaque", map[string][]byte{"trusted_root.json": trustedRoot}),
	} {
		if err := b.apply(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
}

// updateValues returns the arguments of helm that give the nodes the
// mirror, as the chart's README says an operator does: the source through
// extraEnv, its files from Secrets, a NetworkPolicy rule to its address and
// port. Releases are checked for from the console only (an interval of 0):
// a node checks at most once a minute.
func (b *bench) updateValues(mirror *releasemirror.Mirror) []string {
	t := b.t
	t.Helper()
	source, err := url.Parse(mirror.URL())
	if err != nil {
		t.Fatalf("the mirror's address: %v", err)
	}
	host := source.Hostname()
	values := map[string]any{
		"extraEnv": []any{
			map[string]any{"name": "CACTUSFS_UPDATE_SOURCE", "value": mirror.URL()},
			map[string]any{"name": "CACTUSFS_UPDATE_CHECK_INTERVAL", "value": "0"},
		},
		"updates": map[string]any{"caSecret": updateCASecret, "trustedRootSecret": updateRootSecret},
		"networkPolicy": map[string]any{"egressTo": []any{map[string]any{
			"to":    []any{map[string]any{"ipBlock": map[string]any{"cidr": host + "/32"}}},
			"ports": []any{mirror.Port()},
		}}},
	}
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	// JSON is YAML.
	path := filepath.Join(t.TempDir(), "updates.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"-f", path}
}

// forwardAdmin makes the admin port of every node's pod reachable from
// here, as an operator reaches a console the chart does not publish:
// kubectl port-forward, in the k3s container, at the ports of forwardPort,
// started again whenever it ends - its pod was replaced - and at least
// every minute, so that it never holds on to a pod that is gone.
func (b *bench) forwardAdmin() {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := b.exec(ctx, "timeout", "1", "true"); err != nil {
		t.Fatalf("the k3s container has no timeout: %v", err)
	}
	b.admins = map[string]string{}
	for i, id := range nodeIDs() {
		port, err := b.k3s.MappedPort(ctx, forwardPort(i))
		if err != nil {
			t.Fatalf("the published port of %s: %v", forwardPort(i), err)
		}
		b.admins[id] = net.JoinHostPort(b.host, port.Port())
	}

	forwarding, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	// Registered after the container's removal, so that it runs before it.
	t.Cleanup(func() {
		stop()
		wg.Wait()
	})
	for i, id := range nodeIDs() {
		wg.Go(func() {
			for forwarding.Err() == nil {
				_, _ = b.exec(forwarding, "timeout", "60", "kubectl", "port-forward", "-n", namespace, "pod/"+podOf(id),
					"--address", "0.0.0.0", fmt.Sprintf("%d:9001", adminForwardBase+i))
				select {
				case <-forwarding.Done():
				case <-time.After(time.Second):
				}
			}
		})
	}
}

// adminAnswer is what the admin API answered.
type adminAnswer struct {
	status int
	header http.Header
	body   []byte
}

// admin sends a request to the admin API of a node, over TLS checked
// against the CA of this run, under the name of the listeners'
// certificate. A body that is not nil is sent as JSON.
func (b *bench) admin(ctx context.Context, id, method, path string, headers map[string]string, body any) (adminAnswer, error) {
	addr, ok := b.admins[id]
	if !ok {
		return adminAnswer{}, fmt.Errorf("%w: no admin address of %s", errAbort, id)
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return adminAnswer{}, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+addr+"/api/v1"+path, reader)
	if err != nil {
		return adminAnswer{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	transport := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: b.roots, ServerName: release, MinVersion: tls.VersionTLS13},
		DisableKeepAlives: true,
	}
	resp, err := (&http.Client{Transport: transport, Timeout: time.Minute}).Do(req)
	if err != nil {
		return adminAnswer{}, fmt.Errorf("%s %s on %s: %w", method, path, id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return adminAnswer{}, fmt.Errorf("%s %s on %s: %w", method, path, id, err)
	}
	return adminAnswer{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

// adminSoon is admin, asked again for up to two minutes while the request
// cannot be sent: a port-forward is started again, a pod comes up.
func (b *bench) adminSoon(ctx context.Context, id, method, path string, headers map[string]string, body any) (adminAnswer, error) {
	var answer adminAnswer
	err := within(ctx, 2*time.Minute, 2*time.Second, func(ctx context.Context) (bool, error) {
		var err error
		answer, err = b.admin(ctx, id, method, path, headers, body)
		return err == nil, err
	})
	return answer, err
}

// rootBearer is the root credential as a bearer credential of the admin API.
func (b *bench) rootBearer() map[string]string {
	return map[string]string{"Authorization": "Bearer " + b.accessKey + ":" + b.secretKey}
}

// console is a browser signed in as the console user every new deployment
// has (ADR-0014).
type console struct {
	cookie string
	csrf   string
}

func (c console) reading() map[string]string { return map[string]string{"Cookie": c.cookie} }

func (c console) changing() map[string]string {
	return map[string]string{"Cookie": c.cookie, "X-CSRF-Token": c.csrf}
}

// signIn signs the default console user in at a node. It fails the test
// without printing the answer, which holds a session.
func (b *bench) signIn(ctx context.Context, id string) console {
	b.t.Helper()
	answer, err := b.adminSoon(ctx, id, http.MethodPost, "/session", nil,
		map[string]any{"username": app.DefaultAdminName, "password": app.DefaultAdminPassword})
	if err != nil {
		b.t.Fatal(err)
	}
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	cookie := strings.SplitN(answer.header.Get("Set-Cookie"), ";", 2)[0]
	if err := json.Unmarshal(answer.body, &session); err != nil || answer.status != http.StatusOK || session.CSRFToken == "" ||
		!strings.HasPrefix(cookie, "__Host-cactusfs-session=") {
		b.t.Fatalf("signing %s in at %s = %d (%v)", app.DefaultAdminName, id, answer.status, err)
	}
	return console{cookie: cookie, csrf: session.CSRFToken}
}

// updatesView is the answer of GET /updates, as far as this test reads it.
type updatesView struct {
	Current struct {
		Version string `json:"version"`
	} `json:"current"`
	SourceConfigured  bool     `json:"source_configured"`
	CheckedAt         *string  `json:"checked_at"`
	LastError         *string  `json:"last_error"`
	Mode              string   `json:"mode"`
	UpgradeAvailable  bool     `json:"upgrade_available"`
	UnavailableReason *string  `json:"unavailable_reason"`
	Instructions      []string `json:"instructions"`
	Available         []struct {
		Version string  `json:"version"`
		Allowed bool    `json:"allowed"`
		Refusal *string `json:"refusal"`
	} `json:"available"`
}

// allowed returns the releases the view lists as allowed.
func (v updatesView) allowed() []string {
	var out []string
	for _, r := range v.Available {
		if r.Allowed {
			out = append(out, r.Version)
		}
	}
	slices.Sort(out)
	return out
}

// check asks a node to check for releases, and fails the test unless it
// read the mirror; then it returns GET /updates.
func (b *bench) check(ctx context.Context, id string, c console) updatesView {
	b.t.Helper()
	headers := c.changing()
	headers["Content-Type"] = "application/json"
	answer, err := b.adminSoon(ctx, id, http.MethodPost, "/updates/check", headers, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	var view updatesView
	if err := json.Unmarshal(answer.body, &view); err != nil || answer.status != http.StatusOK {
		b.t.Fatalf("POST /updates/check on %s = %d %s (%v)", id, answer.status, answer.body, err)
	}
	if !view.SourceConfigured || view.CheckedAt == nil || view.LastError != nil {
		b.t.Fatalf("POST /updates/check on %s: the node did not read the mirror: %s", id, answer.body)
	}
	return b.updates(ctx, id, c.reading())
}

// updates asks a node GET /updates.
func (b *bench) updates(ctx context.Context, id string, who map[string]string) updatesView {
	b.t.Helper()
	answer, err := b.adminSoon(ctx, id, http.MethodGet, "/updates", who, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	var view updatesView
	if err := json.Unmarshal(answer.body, &view); err != nil || answer.status != http.StatusOK {
		b.t.Fatalf("GET /updates on %s = %d %s (%v)", id, answer.status, answer.body, err)
	}
	return view
}

// progressView is the answer of GET /upgrade.
type progressView struct {
	State     string `json:"state"`
	Target    string `json:"target"`
	StartedBy string `json:"started_by"`
	Nodes     []struct {
		ID      string `json:"id"`
		Version string `json:"version"`
		State   string `json:"state"`
		Reason  string `json:"reason"`
	} `json:"nodes"`
}

func (p progressView) String() string {
	var nodes []string
	for _, n := range p.Nodes {
		nodes = append(nodes, fmt.Sprintf("%s %s %s %q", n.ID, n.State, n.Version, n.Reason))
	}
	return fmt.Sprintf("%s to %s by %s [%s]", p.State, p.Target, p.StartedBy, strings.Join(nodes, "; "))
}

// progress asks a node GET /upgrade, as root.
func (b *bench) progress(ctx context.Context, id string) (progressView, error) {
	answer, err := b.admin(ctx, id, http.MethodGet, "/upgrade", b.rootBearer(), nil)
	if err != nil {
		return progressView{}, err
	}
	var p progressView
	if answer.status != http.StatusOK {
		return progressView{}, fmt.Errorf("GET /upgrade on %s = %d %s", id, answer.status, answer.body)
	}
	if err := json.Unmarshal(answer.body, &p); err != nil {
		return progressView{}, fmt.Errorf("GET /upgrade on %s: %w", id, err)
	}
	return p, nil
}

// upgrade asks a node POST /upgrade with the console user's password.
func (b *bench) upgrade(ctx context.Context, id string, c console, version string) adminAnswer {
	b.t.Helper()
	answer, err := b.admin(ctx, id, http.MethodPost, "/upgrade", c.changing(),
		map[string]any{"version": version, "password": app.DefaultAdminPassword})
	if err != nil {
		b.t.Fatal(err)
	}
	return answer
}

// auditRecord is a record of the audit trail, as far as this test reads it.
type auditRecord struct {
	Actor   string            `json:"actor"`
	Action  string            `json:"action"`
	Outcome string            `json:"outcome"`
	Details map[string]string `json:"details"`
}

// upgradeRecords returns the records of admin:UpgradeCluster with an
// outcome and the details given.
func (b *bench) upgradeRecords(ctx context.Context, id, outcome string, details map[string]string) ([]auditRecord, error) {
	answer, err := b.admin(ctx, id, http.MethodGet, "/audit?limit=1000", b.rootBearer(), nil)
	if err != nil {
		return nil, err
	}
	var page struct {
		Records []auditRecord `json:"records"`
	}
	if answer.status != http.StatusOK {
		return nil, fmt.Errorf("GET /audit on %s = %d", id, answer.status)
	}
	if err := json.Unmarshal(answer.body, &page); err != nil {
		return nil, fmt.Errorf("GET /audit on %s: %w", id, err)
	}
	var out []auditRecord
	for _, r := range page.Records {
		if r.Action != "admin:UpgradeCluster" || r.Outcome != outcome {
			continue
		}
		matches := true
		for name, value := range details {
			matches = matches && r.Details[name] == value
		}
		if matches {
			out = append(out, r)
		}
	}
	return out, nil
}

// audited waits until the audit trail has at least n records of
// admin:UpgradeCluster with an outcome and the details given.
func (b *bench) audited(ctx context.Context, id, what string, n int, outcome string, details map[string]string) {
	b.t.Helper()
	var found []auditRecord
	err := within(ctx, time.Minute, 2*time.Second, func(ctx context.Context) (bool, error) {
		var err error
		found, err = b.upgradeRecords(ctx, id, outcome, details)
		return len(found) >= n, err
	})
	if err != nil {
		b.t.Fatalf("the audit trail has %d records of %s, want %d (%s %v): %v", len(found), what, n, outcome, details, err)
	}
}

// statefulSet returns the image of a node's StatefulSet, the version label
// of its pod template and its annotations.
func (b *bench) statefulSet(ctx context.Context, id string) (img, version string, annotations map[string]string, err error) {
	out, err := b.kubectl(ctx, "get", "statefulset", "-n", namespace, id, "-o", "json")
	if err != nil {
		return "", "", nil, err
	}
	var sts struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			Template struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
				Spec struct {
					Containers []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	// kubectl may warn before the object.
	start := strings.Index(out, "{")
	if start < 0 {
		return "", "", nil, fmt.Errorf("the StatefulSet %s: kubectl printed no object", id)
	}
	if err := json.Unmarshal([]byte(out[start:]), &sts); err != nil {
		return "", "", nil, fmt.Errorf("the StatefulSet %s: %w", id, err)
	}
	for _, c := range sts.Spec.Template.Spec.Containers {
		if c.Name == "cactusfs" {
			img = c.Image
		}
	}
	return img, sts.Spec.Template.Metadata.Labels["app.kubernetes.io/version"], sts.Metadata.Annotations, nil
}

// podView is what is seen of a node's pod.
type podView struct {
	uid      string
	ready    bool
	deleting bool
	image    string
	// waiting is why its container waits, if it does: ErrImageNeverPull.
	waiting string
}

// away reports whether a node is not serving: no pod, a pod that is not
// ready, or one that is being deleted.
func (p podView) away() bool { return p.uid == "" || !p.ready || p.deleting }

// podViews returns what is seen of every node's pod, by pod name.
func (b *bench) podViews(ctx context.Context) (map[string]podView, error) {
	out, err := b.kubectl(ctx, "get", "pods", "-n", namespace, "-o",
		`jsonpath={range .items[*]}{.metadata.name}{"|"}{.metadata.uid}{"|"}{.status.conditions[?(@.type=="Ready")].status}{"|"}`+
			`{.metadata.deletionTimestamp}{"|"}{.spec.containers[0].image}{"|"}{.status.containerStatuses[0].state.waiting.reason}{"\n"}{end}`)
	if err != nil {
		return nil, err
	}
	views := map[string]podView{}
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "|")
		if len(fields) != 6 || !slices.Contains(pods(), fields[0]) {
			continue
		}
		views[fields[0]] = podView{
			uid: fields[1], ready: fields[2] == "True", deleting: fields[3] != "", image: fields[4], waiting: fields[5],
		}
	}
	return views, nil
}

// watch looks, every second while an upgrade runs, at every node's pod and
// reads the object once.
type watch struct {
	b      *bench
	stop   context.CancelFunc
	done   chan struct{}
	bucket string
	key    string
	data   []byte

	mu sync.Mutex
	// replaced are the pods, in the order a new pod of that name was seen.
	replaced []string
	// together are the times two or more nodes were seen away at once.
	together []string
	reads    int
	failed   int
	// whileAway counts, by pod, the reads that returned the object while
	// that pod was away.
	whileAway map[string]int
	// waiting are the reasons, by pod, its container was seen waiting for.
	waiting map[string][]string
	wrong   error
}

// watching starts a watch; it holds the pods it was started with as the
// first seen.
func (b *bench) watching(bucket, key string, data []byte) *watch {
	b.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &watch{
		b: b, stop: cancel, done: make(chan struct{}), bucket: bucket, key: key, data: data,
		whileAway: map[string]int{}, waiting: map[string][]string{},
	}
	first, err := b.podViews(ctx)
	if err != nil {
		cancel()
		b.t.Fatal(err)
	}
	go w.run(ctx, first)
	// However the test ends, the watch ends before it.
	b.t.Cleanup(func() {
		w.stop()
		<-w.done
	})
	return w
}

func (w *watch) run(ctx context.Context, last map[string]podView) {
	defer close(w.done)
	for ctx.Err() == nil {
		asked, cancel := context.WithTimeout(ctx, 20*time.Second)
		views, err := w.b.podViews(asked)
		cancel()
		if err == nil {
			var away []string
			w.mu.Lock()
			for _, pod := range pods() {
				view := views[pod]
				if before := last[pod].uid; view.uid != "" && before != "" && view.uid != before {
					w.replaced = append(w.replaced, pod)
				}
				if view.uid != "" {
					last[pod] = view
				}
				if view.away() {
					away = append(away, pod)
				}
				if view.waiting != "" && !slices.Contains(w.waiting[pod], view.waiting) {
					w.waiting[pod] = append(w.waiting[pod], view.waiting)
				}
			}
			if len(away) > 1 {
				w.together = append(w.together, time.Now().UTC().Format(time.TimeOnly)+" "+strings.Join(away, ","))
			}
			w.mu.Unlock()

			read, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, err := w.b.reads(read, w.bucket, w.key, w.data)
			cancel()
			w.mu.Lock()
			w.reads++
			switch {
			case errors.Is(err, errAbort):
				w.wrong = err
			case err != nil && ctx.Err() == nil:
				w.failed++
				if w.failed <= 20 {
					w.b.t.Logf("read %d failed with %v away: %v", w.reads, away, err)
				}
			case err == nil:
				for _, pod := range away {
					w.whileAway[pod]++
				}
			}
			w.mu.Unlock()
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// saw waits, for up to a minute, until the watch has seen n pods replaced:
// what was seen by another look may not have been seen by the watch yet.
// What it saw is judged by the caller.
func (w *watch) saw(ctx context.Context, n int) {
	_ = within(ctx, time.Minute, time.Second, func(context.Context) (bool, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		return len(w.replaced) >= n, nil
	})
}

// end stops the watch and fails the test if the object was ever read as
// other bytes, or two nodes were away at once.
func (w *watch) end(what string) {
	w.b.t.Helper()
	w.stop()
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	w.b.t.Logf("%s: %d reads, %d failed; read while away: %v; pods replaced in the order %v; containers waiting: %v",
		what, w.reads, w.failed, w.whileAway, w.replaced, w.waiting)
	if w.wrong != nil {
		w.b.t.Fatalf("%s: %v", what, w.wrong)
	}
	if len(w.together) > 0 {
		w.b.t.Fatalf("%s: two nodes or more were away at once: %v", what, w.together)
	}
}

// upgradeModeOff: with upgrade-mode off - the chart's default - every node
// checks, offers the next release with the chart's instructions, and
// refuses to install it, 501. Nothing of the cluster is changed.
func (b *bench) upgradeModeOff(v upgradeVersions) {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	for _, id := range nodeIDs() {
		c := b.signIn(ctx, id)
		view := b.check(ctx, id, c)
		if got, want := view.allowed(), []string{v.next, v.unpullable}; !slices.Equal(got, want) {
			t.Fatalf("%s offers %v, want %v", id, got, want)
		}
		if view.Current.Version != v.current || view.Mode != "off" || view.UpgradeAvailable ||
			view.UnavailableReason == nil || *view.UnavailableReason == "" {
			t.Fatalf("%s: GET /updates = %s, mode %q, upgrade available %v, reason %v",
				id, view.Current.Version, view.Mode, view.UpgradeAvailable, view.UnavailableReason)
		}
		// The chart's commands, as kubernetes.Instructions gives them.
		if text := strings.Join(view.Instructions, "\n"); !strings.Contains(text, "helm upgrade") ||
			!strings.Contains(text, "upgrade.fromConsole.enabled=true") {
			t.Fatalf("%s gives the instructions %q", id, view.Instructions)
		}
		answer := b.upgrade(ctx, id, c, v.next)
		var problem struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(answer.body, &problem); err != nil || answer.status != http.StatusNotImplemented ||
			problem.Code != "UpgradeUnavailable" {
			t.Fatalf("%s: POST /upgrade with upgrade-mode off = %d %s", id, answer.status, answer.body)
		}
		if p, err := b.progress(ctx, id); err != nil || p.State != "idle" {
			t.Fatalf("%s: GET /upgrade = %v (%v), want idle", id, p, err)
		}
	}
	b.audited(ctx, release+"-meta-0", "the refusals of upgrade-mode off", len(nodeIDs()), "failed", map[string]string{
		"from": v.current, "to": v.next, "mode": "off", "reason": domain.ErrUpgradeUnavailable.Error(),
	})
	for _, id := range nodeIDs() {
		img, _, notes, err := b.statefulSet(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if img != b.loaded() || notes[lockAnnotation] != "" || notes[resultAnnotation] != "" {
			t.Fatalf("with upgrade-mode off the StatefulSet %s changed: image %s, annotations %v", id, img, notes)
		}
	}
}

// turnUpgradesOn turns upgrade.fromConsole on as the chart's README says -
// helm upgrade with the API server's endpoints - and rolls the pods by hand,
// one at a time, those that do not vote first, reading the object after
// each.
func (b *bench) turnUpgradesOn(bucket, key string, data []byte) {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	// "kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes"
	out, err := b.kubectl(ctx, "get", "endpointslices", "-n", "default", "-l", "kubernetes.io/service-name=kubernetes",
		"-o", `jsonpath={range .items[*]}{range .endpoints[*]}{.addresses[0]}{" "}{end}{"|"}{.ports[0].port}{end}`)
	if err != nil {
		t.Fatal(err)
	}
	addresses, port, _ := strings.Cut(lastLine(out), "|")
	var cidrs []string
	for _, address := range strings.Fields(addresses) {
		if net.ParseIP(address) == nil {
			t.Fatalf("an endpoint of the API server is not an address: %q", out)
		}
		cidrs = append(cidrs, address+"/32")
	}
	if len(cidrs) == 0 || port == "" {
		t.Fatalf("the API server has no endpoints: %q", out)
	}
	chart := filepath.Join(b.root, "deploy", "helm", "cactusfs")
	upgrade := exec.CommandContext(ctx, "helm", "upgrade", release, chart,
		"--namespace", namespace, "--kubeconfig", b.kubeconfig, "--reuse-values",
		"--set", "upgrade.fromConsole.enabled=true",
		"--set", "upgrade.fromConsole.apiServer.cidrs={"+strings.Join(cidrs, ",")+"}",
		"--set", "upgrade.fromConsole.apiServer.port="+port)
	if out, err := upgrade.CombinedOutput(); err != nil {
		t.Fatalf("helm upgrade: %v\n%s", err, tail(string(out), 6000))
	}

	// updateStrategy OnDelete: a node takes the change when its pod is
	// deleted, and not before.
	for _, id := range []string{release + "-data-0", release + "-meta-1", release + "-meta-2", release + "-meta-0"} {
		pod := podOf(id)
		before, _, err := b.podState(ctx, namespace, pod)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.kubectl(ctx, "delete", "pod", "-n", namespace, pod, "--wait=false"); err != nil {
			t.Fatal(err)
		}
		replaced := func(ctx context.Context) (bool, error) {
			uid, ready, err := b.podState(ctx, namespace, pod)
			return err == nil && uid != "" && uid != before && ready, err
		}
		if err := within(ctx, 8*time.Minute, 2*time.Second, replaced); err != nil {
			t.Fatalf("pod %s did not come back: %v", pod, err)
		}
		read := func(ctx context.Context) (bool, error) { return b.reads(ctx, bucket, key, data) }
		if err := within(ctx, time.Minute, time.Second, read); err != nil {
			t.Fatalf("with %s back the object is not read: %v", pod, err)
		}
	}
	for _, id := range nodeIDs() {
		out, err := b.kubectl(ctx, "get", "pod", "-n", namespace, podOf(id), "-o",
			`jsonpath={.spec.serviceAccountName}{"|"}{.spec.containers[0].env[?(@.name=="CACTUSFS_UPGRADE_MODE")].value}`)
		if err != nil {
			t.Fatal(err)
		}
		if lastLine(out) != release+"-upgrade|kubernetes" {
			t.Fatalf("pod %s runs as %q after the roll, want %s-upgrade|kubernetes", podOf(id), lastLine(out), release)
		}
	}
}

// upgradeTo installs the next release from the console of the first voter,
// and holds the run to what ADR-0029 says of it.
func (b *bench) upgradeTo(v upgradeVersions, next, bucket, key string, data []byte) {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	console := release + "-meta-0"
	// The order of the run: those that do not vote, then the voters, each
	// by ID; the node that runs it last.
	order := []string{release + "-data-0", release + "-meta-1", release + "-meta-2", console}
	target := byDigest(next)

	c := b.signIn(ctx, console)
	view := b.check(ctx, console, c)
	if view.Mode != "kubernetes" || !view.UpgradeAvailable || !slices.Contains(view.allowed(), v.next) {
		t.Fatalf("GET /updates = mode %q, upgrade available %v (%v), allowed %v",
			view.Mode, view.UpgradeAvailable, view.UnavailableReason, view.allowed())
	}

	w := b.watching(bucket, key, data)
	started := time.Now()
	answer := b.upgrade(ctx, console, c, v.next)
	var begun progressView
	if err := json.Unmarshal(answer.body, &begun); err != nil || answer.status != http.StatusAccepted ||
		begun.Target != v.next || answer.header.Get("Location") != "/api/v1/upgrade" {
		t.Fatalf("POST /upgrade %s = %d %s (Location %q, %v)", v.next, answer.status, answer.body, answer.header.Get("Location"), err)
	}
	final := b.until(ctx, 4*nodeTimeout, "done", "failed")
	w.saw(ctx, 4)
	w.end("the upgrade")
	t.Logf("the upgrade to %s took %s", v.next, time.Since(started).Round(time.Second))
	if final.State != "done" || final.Target != v.next || final.StartedBy != "user/"+app.DefaultAdminName {
		t.Fatalf("the upgrade ended %s", final)
	}

	// The lock of a run whose holder moved itself last is replaced by its
	// result once that node is back.
	var result map[string]any
	settled := func(ctx context.Context) (bool, error) {
		_, _, notes, err := b.statefulSet(ctx, order[0])
		if err != nil {
			return false, err
		}
		result = nil
		if text, ok := notes[resultAnnotation]; ok {
			if err := json.Unmarshal([]byte(text), &result); err != nil {
				return false, err
			}
		}
		return notes[lockAnnotation] == "" && result["state"] == "done", nil
	}
	if err := within(ctx, 2*nodeTimeout, 2*time.Second, settled); err != nil {
		t.Fatalf("the lock of the run was not replaced by its result done: %v (result %v)", err, result)
	}
	if result["target"] != v.next || result["image"] != target || result["holder"] != console {
		t.Fatalf("the result of the run is %v", result)
	}

	w.mu.Lock()
	replaced, whileAway := slices.Clone(w.replaced), maps.Clone(w.whileAway)
	w.mu.Unlock()
	var wantReplaced []string
	for _, id := range order {
		wantReplaced = append(wantReplaced, podOf(id))
	}
	if !slices.Equal(replaced, wantReplaced) {
		t.Fatalf("the pods were replaced in the order %v, want %v: one at a time, the node that ran the upgrade last", replaced, wantReplaced)
	}
	for _, pod := range wantReplaced {
		if whileAway[pod] == 0 {
			t.Errorf("the object was not read while %s was away", pod)
		}
	}

	// Every node runs B and says it runs the release, on every node. The
	// node replaced last has only just come back, and the connection to it
	// may be cut once while the forward to its new pod is made: it is asked
	// again then.
	for _, id := range nodeIDs() {
		var p progressView
		if err := within(ctx, time.Minute, 3*time.Second, func(ctx context.Context) (bool, error) {
			var err error
			p, err = b.progress(ctx, id)
			return err == nil, err
		}); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, n := range p.Nodes {
			got = append(got, n.ID)
			if n.State != "done" || n.Version != v.next {
				t.Errorf("GET /upgrade on %s says %s", id, p)
			}
		}
		if p.State != "done" || !slices.Equal(got, order) {
			t.Errorf("GET /upgrade on %s says %s, want done in the order %v", id, p, order)
		}
		if got := b.updates(ctx, id, b.rootBearer()).Current.Version; got != v.next {
			t.Errorf("%s reports %s, want %s", id, got, v.next)
		}
		img, version, notes, err := b.statefulSet(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if img != target || version != v.next || notes[previousAnnotation] == "" {
			t.Errorf("the StatefulSet %s names %s, version %q, previous %q; want %s, %s and the image it ran",
				id, img, version, notes[previousAnnotation], target, v.next)
		}
	}
	views, err := b.podViews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, pod := range pods() {
		if views[pod].image != target || views[pod].away() {
			t.Errorf("pod %s runs %s (away: %v), want %s", pod, views[pod].image, views[pod].away(), target)
		}
	}
	if _, err := b.reads(ctx, bucket, key, data); err != nil {
		t.Fatalf("after the upgrade the object is not read: %v", err)
	}
	b.audited(ctx, release+"-meta-1", "the start", 1, "success", map[string]string{
		"from": v.current, "to": v.next, "mode": "kubernetes", "digest": next,
	})
	if t.Failed() {
		t.FailNow()
	}
	// The node that ran the upgrade was moved by it, last, before it could
	// report the end: the restarted node, which replaced the lock with the
	// result "done", audits that end - once, as the user who started it.
	end := map[string]string{"from": v.current, "to": v.next, "digest": next, "state": "done"}
	b.audited(ctx, release+"-meta-1", "the end", 1, "success", end)
	// A second record would come from another write over the lock, which
	// is gone: give it the time a record takes, then count.
	time.Sleep(10 * time.Second)
	ends, err := b.upgradeRecords(ctx, release+"-meta-1", "success", map[string]string{"to": v.next, "state": "done"})
	if err != nil {
		t.Fatal(err)
	}
	// The holder, restarted, recorded it, and says the actor was read
	// from the lock.
	maps.Copy(end, map[string]string{"recorded_by": console, "actor_source": "kubernetes lock annotation"})
	if len(ends) != 1 || ends[0].Actor != "user/admin" || !maps.Equal(ends[0].Details, end) {
		t.Fatalf("records of the end of the upgrade to %s in the audit trail: %+v, want one by user/admin with %v", v.next, ends, end)
	}
	t.Logf("the end of the upgrade to %s in the audit trail: actor %s, outcome %s, details %v", v.next, ends[0].Actor, ends[0].Outcome, ends[0].Details)
}

// until asks GET /upgrade of any node that answers, every three seconds,
// until the upgrade is in one of the states, and returns that answer.
func (b *bench) until(ctx context.Context, limit time.Duration, states ...string) progressView {
	b.t.Helper()
	var last progressView
	err := within(ctx, limit, 3*time.Second, func(ctx context.Context) (bool, error) {
		var errs []error
		for _, id := range []string{release + "-meta-1", release + "-meta-2", release + "-data-0", release + "-meta-0"} {
			p, err := b.progress(ctx, id)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if p.String() != last.String() {
				b.t.Logf("GET /upgrade on %s: %s", id, p)
			}
			last = p
			return slices.Contains(states, p.State), nil
		}
		return false, errors.Join(errs...)
	})
	if err != nil {
		b.t.Fatalf("the upgrade did not end %v within %s: %v; last %s", states, limit, err, last)
	}
	return last
}

// putBack starts an upgrade to a release whose image the cluster does not
// have: the first node never comes up on it, is put back to what it ran and
// is Ready again; nothing else is touched; the run ends failed.
func (b *bench) putBack(v upgradeVersions, next, unpullable, bucket, key string, data []byte) {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	console := release + "-meta-0"
	first := release + "-data-0"
	before, err := b.podViews(ctx)
	if err != nil {
		t.Fatal(err)
	}

	c := b.signIn(ctx, console)
	if view := b.check(ctx, console, c); !slices.Contains(view.allowed(), v.unpullable) || view.Current.Version != v.next {
		t.Fatalf("%s runs %s and allows %v, want %s", console, view.Current.Version, view.allowed(), v.unpullable)
	}
	w := b.watching(bucket, key, data)
	started := time.Now()
	if answer := b.upgrade(ctx, console, c, v.unpullable); answer.status != http.StatusAccepted {
		t.Fatalf("POST /upgrade %s = %d %s", v.unpullable, answer.status, answer.body)
	}
	final := b.until(ctx, nodeTimeout+10*time.Minute, "failed", "done")
	t.Logf("the upgrade to %s ended %s after %s", v.unpullable, final.State, time.Since(started).Round(time.Second))
	if final.State != "failed" || len(final.Nodes) != len(nodeIDs()) || final.Nodes[0].ID != first ||
		final.Nodes[0].State != "restored" || !strings.Contains(final.Nodes[0].Reason, "put back") {
		t.Fatalf("the upgrade to %s ended %s; want failed, %s restored", v.unpullable, final, first)
	}
	for _, n := range final.Nodes[1:] {
		if n.State != "pending" {
			t.Errorf("after the failure %s is %s, want pending", n.ID, n.State)
		}
	}

	// The node comes back on B.
	back := func(ctx context.Context) (bool, error) {
		views, err := b.podViews(ctx)
		view := views[podOf(first)]
		return err == nil && view.image == byDigest(next) && !view.away(), err
	}
	if err := within(ctx, 8*time.Minute, 2*time.Second, back); err != nil {
		t.Fatalf("%s did not come back on %s: %v", first, byDigest(next), err)
	}
	w.saw(ctx, 2)
	w.end("the upgrade that is put back")

	w.mu.Lock()
	replaced, waiting, whileAway := slices.Clone(w.replaced), slices.Clone(w.waiting[podOf(first)]), w.whileAway[podOf(first)]
	w.mu.Unlock()
	if !slices.Contains(waiting, "ErrImageNeverPull") {
		t.Errorf("the container of %s was never seen waiting for an image it may not pull: %v", first, waiting)
	}
	if len(replaced) < 2 || slices.ContainsFunc(replaced, func(pod string) bool { return pod != podOf(first) }) {
		t.Errorf("pods replaced %v; want %s, twice, and no other", replaced, podOf(first))
	}
	if whileAway == 0 {
		t.Errorf("the object was not read while %s was away", podOf(first))
	}
	after, err := b.podViews(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range nodeIDs() {
		img, version, notes, err := b.statefulSet(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if img != byDigest(next) || version != v.next {
			t.Errorf("the StatefulSet %s names %s, version %q; want %s, %s", id, img, version, byDigest(next), v.next)
		}
		if id == first {
			if _, ok := notes[previousAnnotation]; ok {
				t.Errorf("the StatefulSet %s, put back, still has %s", id, previousAnnotation)
			}
			continue
		}
		if after[podOf(id)].uid != before[podOf(id)].uid {
			t.Errorf("pod %s was replaced by an upgrade that stopped before it", podOf(id))
		}
	}
	_, _, notes, err := b.statefulSet(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(notes[resultAnnotation]), &result); err != nil || notes[lockAnnotation] != "" ||
		result["state"] != "failed" || result["node"] != first || result["nodeState"] != "restored" ||
		result["target"] != v.unpullable {
		t.Errorf("the result of the run is %v, lock %q (%v)", result, notes[lockAnnotation], err)
	}
	if _, err := b.reads(ctx, bucket, key, data); err != nil {
		t.Fatalf("after the failed upgrade the object is not read: %v", err)
	}
	b.audited(ctx, console, "the start", 1, "success", map[string]string{
		"from": v.next, "to": v.unpullable, "mode": "kubernetes", "digest": unpullable,
	})
	b.audited(ctx, console, "the end", 1, "failed", map[string]string{
		"from": v.next, "to": v.unpullable, "digest": unpullable, "state": "failed",
	})
}
