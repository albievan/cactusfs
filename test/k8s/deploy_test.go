//go:build k8s

package k8s_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// TestDeployScriptOnK3s: deploy/deploy.sh on the k3s of the bench (PLAN
// 10.10, 10.11). The script reaches the cluster through KUBECTL - the k3s
// container's own kubectl, behind a wrapper that drops --context - and
// imports the image with K3S_IMPORT, the container's ctr; it renders with
// the builder's helm (HELM). The volumes are the chart's localVolumes
// (K8S_STORAGE=hostpath: the bench has no storage driver), whose
// directories the test makes, as the script does not.
//
// Standalone: installed by the script, an S3 round trip with the
// credentials and the CA of its STATE_DIR, the cluster's identity (the UID
// of kube-system) in its record, a namespace and an object made beforehand
// outside unchanged (resourceVersion), uninstall keeps the claims, a
// reinstall reads the object, a certificate of the operator's rotates the
// Secret (kubectl replace at the resourceVersion read back), and a purge
// leaves nothing of the release but the Released volumes it names.
//
// Cluster mode: installed, the object written, upgraded to a second image
// with the pods' start times proving one node at a time - the one that does
// not vote first - and the object read during it; uninstall keeps the
// claims, a reinstall reads the object, and the purge removes the entry of
// STATE_DIR only after the claims are gone.
func TestDeployScriptOnK3s(t *testing.T) {
	b := newBench(t)
	v := upgradeVersionsOf(t)
	b.startCluster()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	image, err := b.productImage(ctx)
	if err != nil {
		t.Fatalf("product image: %v", err)
	}
	next := "cactusfs:test-" + v.next
	if err := b.buildImage(ctx, next, "VERSION="+v.next); err != nil {
		t.Fatalf("the second image: %v", err)
	}
	t.Cleanup(func() { b.untagName(next) })
	// The script tags each image it moves by its ID
	// (cactusfs:<version>-<build>-<12 hex>) on the engine here; those of
	// this run's images go when the test ends.
	t.Cleanup(func() { b.untagIDTags(image, next) })

	outside := b.outsideObjects()
	clusterBefore := b.clusterScoped()

	// A NodePort another namespace's Service holds is refused before
	// anything is made.
	clash := b.deployer("clash", "standalone", image)
	clash.env = strings.Replace(clash.env, "K8S_NODE_PORT=30900", "K8S_NODE_PORT="+outsideNodePort, 1)
	out := clash.run(2, "--install")
	clash.says(out, "the NodePort "+outsideNodePort+" is held by the Service "+outside["namespace"]+"/web")
	clash.gone()

	solo := b.deployer("solo", "standalone", image)
	solo.volumes()
	out = solo.run(0, "--install")
	solo.says(out, "ready: solo-node-0 runs ")
	solo.recordsTheCluster()
	solo.trust()
	solo.reachS3()
	bucket := "deploy-" + b.run
	data := b.roundTrip(bucket, "first")
	b.outsideUnchanged(outside)

	out = solo.run(0, "--uninstall")
	solo.says(out, "kept: the claims ")
	solo.claimsKept()
	solo.run(0, "--install")
	solo.reads(bucket, "first", data)
	solo.rotate()
	out = solo.run(0, "--uninstall", "--purge", "--purge-confirm=solo")
	solo.says(out, "purge: no claim of solo is left", "left, as their StorageClass's reclaimPolicy Retain keeps them", "purged: solo")
	solo.gone()
	b.outsideUnchanged(outside)

	quad := b.deployer("quad", "cluster", image)
	quad.volumes()
	quad.run(0, "--install")
	quad.trust()
	quad.reachS3()
	bucket = "deploy-cluster-" + b.run
	data = b.roundTrip(bucket, "first")
	quad.lockRefuses()
	// The upgrade to the second image is made after a console-style
	// patch of the image: --install stops at the conflict, and
	// --install --take-image takes the fields and rolls the pods.
	quad.consolePatch()
	quad.image = next
	out = quad.run(1, "--install")
	quad.says(out, "the apply met a conflict", "--take-image")
	rendered := quad.upgrade(next, bucket, "first", data, "--take-image=quad")
	quad.ownsTheImage(rendered)
	quad.run(0, "--uninstall")
	quad.claimsKept()
	quad.run(0, "--install")
	quad.reads(bucket, "first", data)
	out = quad.run(0, "--uninstall", "--purge", "--purge-confirm=quad")
	quad.says(out, "purged: quad")
	// The bench imports with K3S_IMPORT, whose node the script cannot ask:
	// it records no tag, and the purge leaves the tags (the harness proves
	// the removal over K3S_SSH, TestK8sPurge).
	if quad.importedTag() != "" {
		t.Error("a tag imported by K3S_IMPORT was recorded")
	}
	if !b.hasTag("docker.io/library/" + rendered) {
		t.Errorf("the purge removed %s, which K3S_IMPORT imported", rendered)
	}
	if strings.Index(out, "purge: no claim of quad is left") > strings.Index(out, "purge: deleted "+quad.entry()) {
		t.Errorf("the entry of STATE_DIR was deleted before the claims were proved gone:\n%s", tail(out, 4000))
	}
	quad.gone()
	b.outsideUnchanged(outside)
	b.clusterScopedUnchanged(clusterBefore, clash.ns, solo.ns, quad.ns)
}

// deployer is one deployment the script makes on the bench: its name and
// mode, its own namespace, and the files of its run - the env file, the
// wrappers that reach the k3s container, STATE_DIR.
type deployer struct {
	b     *bench
	name  string
	mode  string
	ns    string
	dir   string
	env   string
	state string
	image string
	// extra are lines added to the env file of later runs.
	extra string
}

// hostPath is K8S_HOSTPATH: in the volume the k3s image keeps its state in.
const hostPath = "/var/lib/rancher/k3s/cactusfs-deploy"

func (b *bench) deployer(name, mode, image string) *deployer {
	t := b.t
	t.Helper()
	dir := t.TempDir()
	d := &deployer{b: b, name: name, mode: mode, ns: name + "-" + b.run, dir: dir, state: filepath.Join(dir, "state"), image: image}
	// Registered after the k3s container's removal, so that it runs first:
	// a test that failed shows the deployment's own namespace too (the
	// bench's report shows the chart's).
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		d.dumpS3(ctx)
	})
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	container := b.k3s.GetContainerID()
	// The context is dropped: the k3s container's kubectl is the cluster's
	// administrator. Nothing of the caller's environment is needed: docker
	// reaches the engine by its default socket, as in the builder.
	wrappers := map[string]string{
		"kubectl":    "#!/bin/sh\nif [ \"$1\" = --context ]; then shift 2; fi\nexec docker exec -i " + container + " kubectl \"$@\"\n",
		"k3s-import": "#!/bin/sh\nexec docker exec -i " + container + " ctr -n k8s.io images import -\n",
	}
	root, err := os.OpenRoot(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for name, script := range wrappers {
		// Programs the script runs: executable by their owner alone.
		if err := root.WriteFile(name, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	lines := []string{
		"DEPLOY_TARGET=k3s",
		"DEPLOY_NAME=" + name,
		"DEPLOY_MODE=" + mode,
		"K8S_NAMESPACE=" + d.ns,
		"K8S_CONTEXT=bench",
		"KUBECTL=" + filepath.Join(bin, "kubectl"),
		"K3S_IMPORT=" + filepath.Join(bin, "k3s-import"),
		"KMS=local",
		"IMAGE_SOURCE=image",
		// The image of this tree, which may have changes: it goes to
		// another machine only in name (the bench's k3s).
		"ALLOW_DIRTY=yes",
		"HELM=helm",
		"K8S_STORAGE=hostpath",
		"K8S_HOSTPATH=" + hostPath,
		"K8S_NODE_PORT=30900",
		"K8S_S3_ALLOW_FROM=any",
		// The name the test's client gives the S3 Service.
		"TLS_HOSTS=" + release,
		"STATE_DIR=" + d.state,
		"WAIT_TIMEOUT=600",
	}
	if mode == "cluster" {
		lines = append(lines, "ALLOW_SHARED_LOCAL_KMS=yes")
	}
	d.env = strings.Join(lines, "\n") + "\n"
	return d
}

// nodes are the node IDs of the deployment.
func (d *deployer) nodes() []string {
	if d.mode == "cluster" {
		return []string{d.name + "-meta-0", d.name + "-meta-1", d.name + "-meta-2", d.name + "-data-0"}
	}
	return []string{d.name + "-node-0"}
}

// entry is the deployment's entry of STATE_DIR (the target ID of a k3s
// reached by a context: k3s-<context>).
func (d *deployer) entry() string { return filepath.Join(d.state, "k3s-bench", d.name) }

// volumes makes the directories of the chart's localVolumes, as the
// operator of K8S_STORAGE=hostpath does: <K8S_HOSTPATH>/<namespace>/<node
// ID>/<volume>, belonging to the user the pods run as.
func (d *deployer) volumes() {
	t := d.b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	drives := 4
	if d.mode == "cluster" {
		drives = 2
	}
	mkdir := []string{"mkdir", "-p"}
	for _, id := range d.nodes() {
		mkdir = append(mkdir, hostPath+"/"+d.ns+"/"+id+"/meta")
		for i := 1; i <= drives; i++ {
			mkdir = append(mkdir, fmt.Sprintf("%s/%s/%s/data%d", hostPath, d.ns, id, i))
		}
	}
	if _, err := d.b.exec(ctx, mkdir...); err != nil {
		t.Fatal(err)
	}
	if _, err := d.b.exec(ctx, "chown", "-R", fmt.Sprintf("%d:%d", podUser, podUser), hostPath+"/"+d.ns); err != nil {
		t.Fatal(err)
	}
}

// run runs deploy/deploy.sh with the deployment's env file and the
// arguments, under a PATH that finds the wrappers first, and returns what
// it printed; an exit status other than want fails the test.
func (d *deployer) run(want int, args ...string) string {
	t := d.b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	envFile := filepath.Join(d.dir, "site.env")
	if err := os.WriteFile(envFile, []byte(d.env+"IMAGE="+d.image+"\n"+d.extra), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(d.dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "sh", append([]string{filepath.Join(d.b.root, "deploy", "deploy.sh"), "--env", envFile}, args...)...)
	cmd.Env = []string{"PATH=" + filepath.Join(d.dir, "bin") + ":" + os.Getenv("PATH"), "HOME=" + home}
	out, err := cmd.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatalf("deploy.sh %v: %v", args, err)
	}
	if code != want {
		t.Fatalf("deploy.sh %v exited %d, want %d:\n%s", args, code, want, tail(string(out), 8000))
	}
	return string(out)
}

// says fails the test unless the output says each text.
func (d *deployer) says(out string, texts ...string) {
	d.b.t.Helper()
	for _, text := range texts {
		if !strings.Contains(out, text) {
			d.b.t.Fatalf("deploy.sh did not say %q:\n%s", text, tail(out, 8000))
		}
	}
}

// file is a file of the deployment's entry of STATE_DIR.
func (d *deployer) file(rel string) []byte {
	d.b.t.Helper()
	root, err := os.OpenRoot(d.entry())
	if err != nil {
		d.b.t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(rel)
	if err != nil {
		d.b.t.Fatal(err)
	}
	return data
}

// recordsTheCluster: the entry's record holds the UID of kube-system, the
// cluster's identity.
func (d *deployer) recordsTheCluster() {
	t := d.b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	uid, err := d.b.kubectl(ctx, "get", "namespace", "kube-system", "-o", "jsonpath={.metadata.uid}")
	if err != nil {
		t.Fatal(err)
	}
	if record := string(d.file("target")); !strings.Contains(record, "engine-id="+lastLine(uid)+"\n") {
		t.Errorf("the record does not hold the UID of kube-system, %s:\n%s", lastLine(uid), record)
	}
}

// trust gives the bench's client the CA and the root credentials of the
// deployment's STATE_DIR.
func (d *deployer) trust() {
	t := d.b.t
	t.Helper()
	d.b.roots = x509.NewCertPool()
	if !d.b.roots.AppendCertsFromPEM(d.file("config/s3-ca.pem")) {
		t.Fatal("the CA of STATE_DIR holds no certificate")
	}
	d.b.accessKey = strings.TrimSpace(string(d.file("secrets/root-access-key")))
	d.b.secretKey = strings.TrimSpace(string(d.file("secrets/root-secret-key")))
}

// reads fails the test unless the object is read back as data, waiting for
// the deployment to answer.
func (d *deployer) reads(bucket, key string, data []byte) {
	t := d.b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	err := within(ctx, 2*time.Minute, 2*time.Second, func(ctx context.Context) (bool, error) {
		return d.b.reads(ctx, bucket, key, data)
	})
	if err != nil {
		t.Fatalf("the object %s/%s was not read back: %v", bucket, key, err)
	}
}

// claims are the deployment's claims, by name.
func (d *deployer) claims() []string {
	t := d.b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := d.b.kubectl(ctx, "get", "persistentvolumeclaims", "-n", d.ns, "-o", "name")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(out)
}

// claimsKept: after an uninstall the claims are there, every node's, and
// nothing else of the workload is.
func (d *deployer) claimsKept() {
	t := d.b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	claims := d.claims()
	for _, id := range d.nodes() {
		want := "persistentvolumeclaim/meta-" + id + "-0"
		if !strings.Contains(strings.Join(claims, " "), want) {
			t.Errorf("the uninstall did not keep %s: %v", want, claims)
		}
	}
	left, err := d.b.kubectl(ctx, "get", "statefulsets,services,networkpolicies,poddisruptionbudgets", "-n", d.ns,
		"-l", "app.kubernetes.io/instance="+d.name, "-o", "name")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(left) != "" {
		t.Errorf("the uninstall left %s", left)
	}
	secrets, err := d.b.kubectl(ctx, "get", "secrets", "-n", d.ns, "-l", "io.cactusfs.deploy.name="+d.name, "-o", "name")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(secrets) != "" {
		t.Errorf("the uninstall left the Secrets %s", secrets)
	}
}

// gone: after a purge the deployment's namespace is gone, so its claims
// and Secrets, and so is its entry of STATE_DIR; the volumes the hostpath
// StorageClass keeps (reclaimPolicy Retain) are Released, and the script
// deleted none.
func (d *deployer) gone() {
	t := d.b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	err := within(ctx, 2*time.Minute, 2*time.Second, func(ctx context.Context) (bool, error) {
		out, err := d.b.kubectl(ctx, "get", "namespace", d.ns, "--ignore-not-found", "-o", "name")
		return strings.TrimSpace(out) == "", err
	})
	if err != nil {
		t.Errorf("the namespace %s is still there: %v", d.ns, err)
	}
	if _, err := os.Stat(d.entry()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the entry of STATE_DIR is still there: %v", err)
	}
	out, err := d.b.kubectl(ctx, "get", "persistentvolumes", "-o", `jsonpath={range .items[*]}{.metadata.name}{" "}{.status.phase}{"\n"}{end}`)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, d.ns+"-") && !strings.HasSuffix(line, " Released") {
			t.Errorf("a volume of the deployment is %q after the purge, not Released", line)
		}
	}
}

// rotate gives the deployment a certificate of the operator's own: the
// install replaces the Secret <name>-tls (kubectl replace, at the
// resourceVersion read back) and STATE_DIR's copy, and says it rotated it.
func (d *deployer) rotate() {
	t := d.b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	before, err := d.b.kubectl(ctx, "get", "secret", d.name+"-tls", "-n", d.ns, "-o", "jsonpath={.metadata.resourceVersion}")
	if err != nil {
		t.Fatal(err)
	}
	cert, key, ca := certificateOf(t, release, "localhost")
	files := map[string][]byte{"own.crt": cert, "own.key": key, "own-ca.pem": ca}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(d.dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d.extra = "TLS_CERT_FILE=" + filepath.Join(d.dir, "own.crt") + "\nTLS_KEY_FILE=" + filepath.Join(d.dir, "own.key") +
		"\nTLS_CA_FILE=" + filepath.Join(d.dir, "own-ca.pem") + "\n"
	out := d.run(0, "--install")
	d.says(out, "rotated: the Secrets "+d.name+"-tls", "secrets: rotated TLS_CERT_FILE")
	got, err := d.b.kubectl(ctx, "get", "secret", d.name+"-tls", "-n", d.ns, "-o", `jsonpath={.data.tls\.crt}`)
	if err != nil {
		t.Fatal(err)
	}
	if lastLine(got) != base64.StdEncoding.EncodeToString(cert) {
		t.Error("the Secret does not hold the certificate given")
	}
	after, err := d.b.kubectl(ctx, "get", "secret", d.name+"-tls", "-n", d.ns, "-o", "jsonpath={.metadata.resourceVersion}")
	if err != nil {
		t.Fatal(err)
	}
	if lastLine(after) == lastLine(before) {
		t.Error("the Secret was not replaced")
	}
	managers, err := d.b.kubectl(ctx, "get", "secret", d.name+"-tls", "-n", d.ns, "-o", "jsonpath={.metadata.managedFields[*].manager}")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range strings.Fields(lastLine(managers)) {
		if m != "cactusfs-deploy" {
			t.Errorf("the Secret's field managers are %q, want cactusfs-deploy alone", managers)
		}
	}
	if !bytes.Equal(d.file("config/s3.pem"), cert) {
		t.Error("STATE_DIR's copy of the certificate was not replaced")
	}
}

// certificateOf is a certificate for the names, its key and the CA that
// issued it, made for the test.
func certificateOf(t *testing.T, names ...string) (cert, key, ca []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "deploy test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

// podTimes are when each node's pod was started and when it last became
// Ready, by node ID.
func (d *deployer) podTimes(ctx context.Context) (started, ready map[string]time.Time, err error) {
	started, ready = map[string]time.Time{}, map[string]time.Time{}
	for _, id := range d.nodes() {
		out, err := d.b.kubectl(ctx, "get", "pod", podOf(id), "-n", d.ns, "-o",
			`jsonpath={.status.startTime}{" "}{.status.conditions[?(@.type=="Ready")].lastTransitionTime}`)
		if err != nil {
			return nil, nil, err
		}
		fields := strings.Fields(lastLine(out))
		if len(fields) != 2 {
			return nil, nil, fmt.Errorf("the pod %s says %q", podOf(id), out)
		}
		if started[id], err = time.Parse(time.RFC3339, fields[0]); err != nil {
			return nil, nil, err
		}
		if ready[id], err = time.Parse(time.RFC3339, fields[1]); err != nil {
			return nil, nil, err
		}
	}
	return started, ready, nil
}

// upgrade installs the second image over the deployment while the object
// is read again and again, and holds the run to one node at a time: the
// node that does not vote first, then the voters, each new pod started
// after the one before it was Ready again; no read returns other bytes, and
// reads succeed while it runs.
func (d *deployer) upgrade(next, bucket, key string, data []byte, extra ...string) string {
	t := d.b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	startedBefore, _, err := d.podTimes(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var good, wrong int
	readCtx, stopReading := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for readCtx.Err() == nil {
			got, err := d.b.get(readCtx, bucket, key)
			mu.Lock()
			switch {
			case err == nil && bytes.Equal(got, data):
				good++
			case err == nil:
				wrong++
			}
			mu.Unlock()
			select {
			case <-readCtx.Done():
			case <-time.After(time.Second):
			}
		}
	}()
	d.image = next
	out := d.run(0, append([]string{"--install"}, extra...)...)
	stopReading()
	<-done
	d.says(out, "upgrade: "+d.name+"-meta-2 is Ready on ", "was imported by K3S_IMPORT's command, on a node the script cannot ask: it is not recorded, and a purge leaves it")
	// The image the pods run, as the install names it (a tag of the
	// image's ID the import gave it).
	m := regexp.MustCompile(`image: the pods run (\S+) \(imagePullPolicy Never\)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the install does not say the image the pods run:\n%s", tail(out, 4000))
	}
	rendered := m[1]

	started, ready, err := d.podTimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	previous := ""
	for _, id := range []string{d.name + "-data-0", d.name + "-meta-0", d.name + "-meta-1", d.name + "-meta-2"} {
		if !started[id].After(startedBefore[id]) {
			t.Errorf("the pod of %s was not replaced (started %v, before %v)", id, started[id], startedBefore[id])
		}
		if previous != "" && started[id].Before(ready[previous]) {
			t.Errorf("the pod of %s started at %v, before %s was Ready again at %v: not one at a time", id, started[id], previous, ready[previous])
		}
		previous = id
	}
	mu.Lock()
	defer mu.Unlock()
	if wrong != 0 {
		t.Errorf("%d reads during the upgrade returned other bytes", wrong)
	}
	if good == 0 {
		t.Error("the object was never read during the upgrade")
	}
	return rendered
}

// outsideObjects makes, outside the deployments, a namespace and an object
// in it, and returns their resourceVersions, which no run of the script may
// change.
func (b *bench) outsideObjects() map[string]string {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ns := "outside-" + b.run
	if err := b.apply(ctx, named("Namespace", "", ns, nil)); err != nil {
		t.Fatal(err)
	}
	sentinel := named("ConfigMap", ns, "sentinel", nil)
	sentinel["data"] = map[string]string{"made": "before the script ran"}
	// A Service of NodePort outsideNodePort, which no deployment may take.
	web := named("Service", ns, "web", nil)
	web["spec"] = map[string]any{"type": "NodePort", "ports": []any{map[string]any{"port": 80, "nodePort": 30950}}}
	// A sibling release: objects of another namespace with the labels of
	// the deployments' release names, which no run may touch.
	sibling := named("ConfigMap", ns, "sibling", map[string]string{"app.kubernetes.io/instance": "solo", "app.kubernetes.io/name": "cactusfs"})
	siblingSvc := named("Service", ns, "quad", map[string]string{"app.kubernetes.io/instance": "quad", "app.kubernetes.io/name": "cactusfs"})
	siblingSvc["spec"] = map[string]any{"ports": []any{map[string]any{"port": 9000}}}
	for _, o := range []object{sentinel, web, sibling, siblingSvc} {
		if err := b.apply(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	return b.versionsOf(ctx, ns)
}

// outsideNodePort is the NodePort of the Service web outside the
// deployments.
const outsideNodePort = "30950"

// versionsOf is the resourceVersions of the namespace and its sentinel.
func (b *bench) versionsOf(ctx context.Context, ns string) map[string]string {
	t := b.t
	t.Helper()
	out := map[string]string{"namespace": ns}
	for what, args := range map[string][]string{
		"namespace/" + ns:      {"get", "namespace", ns, "-o", "jsonpath={.metadata.resourceVersion}"},
		"configmap/" + ns:      {"get", "configmap", "sentinel", "-n", ns, "-o", "jsonpath={.metadata.resourceVersion}"},
		"service/web":          {"get", "service", "web", "-n", ns, "-o", "jsonpath={.metadata.resourceVersion}"},
		"configmap/sibling":    {"get", "configmap", "sibling", "-n", ns, "-o", "jsonpath={.metadata.resourceVersion}"},
		"service/sibling-quad": {"get", "service", "quad", "-n", ns, "-o", "jsonpath={.metadata.resourceVersion}"},
	} {
		v, err := b.kubectl(ctx, args...)
		if err != nil {
			t.Fatal(err)
		}
		out[what] = lastLine(v)
	}
	return out
}

// outsideUnchanged fails the test when the namespace or the object made
// outside changed.
func (b *bench) outsideUnchanged(before map[string]string) {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	after := b.versionsOf(ctx, before["namespace"])
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed: resourceVersion %s, then %s", k, v, after[k])
		}
	}
}

// clusterScoped is the resourceVersion of every cluster-scoped object of
// the kinds a deployment could touch, by kind and name.
func (b *bench) clusterScoped() map[string]string {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := b.kubectl(ctx, "get",
		"namespaces,storageclasses,persistentvolumes,customresourcedefinitions,clusterroles,clusterrolebindings,validatingwebhookconfigurations,mutatingwebhookconfigurations",
		"-o", `jsonpath={range .items[*]}{.kind}/{.metadata.name} {.metadata.resourceVersion}{"\n"}{end}`)
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if name, version, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			versions[name] = version
		}
	}
	return versions
}

// clusterScopedUnchanged fails the test when a cluster-scoped object changed,
// came or went, but the deployments' namespaces and what hostpath made for
// them (StorageClass and PersistentVolumes named after the namespace).
func (b *bench) clusterScopedUnchanged(before map[string]string, namespaces ...string) {
	t := b.t
	t.Helper()
	after := b.clusterScoped()
	ours := func(name string) bool {
		for _, ns := range namespaces {
			if strings.HasSuffix(name, "/"+ns) || strings.Contains(name, "/"+ns+"-") {
				return true
			}
		}
		return false
	}
	for name, v := range before {
		if after[name] != v && !ours(name) {
			t.Errorf("the cluster-scoped %s changed: resourceVersion %s, then %q", name, v, after[name])
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok && !ours(name) {
			t.Errorf("the cluster-scoped %s was made", name)
		}
	}
}

// importedTag is the image tag the deployment's entry records as imported
// by the script (the last), or "".
func (d *deployer) importedTag() string {
	data, err := os.ReadFile(filepath.Join(d.entry(), "images"))
	if err != nil {
		return ""
	}
	lines := strings.Fields(string(data))
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// hasTag reports whether the containerd of k3s holds an image of the name.
func (b *bench) hasTag(name string) bool {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := b.exec(ctx, "ctr", "-n", "k8s.io", "images", "ls", "-q", "name=="+name)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out) == name
}

// lockRefuses: a console upgrade's lock with a fresh heartbeat on the first
// node by name, <name>-data-0, refuses an install before anything changes.
func (d *deployer) lockRefuses() {
	t := d.b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	first := d.name + "-data-0"
	lock := `{"run":"bench","target":"0.8.0","image":"x@sha256:` + strings.Repeat("e", 64) + `","holder":"` + d.name + `-meta-1","startedBy":"admin","startedAt":"` +
		time.Now().UTC().Format(time.RFC3339) + `","heartbeat":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
	if _, err := d.b.kubectl(ctx, "annotate", "statefulset", first, "-n", d.ns, "cactusfs.io/upgrade="+lock); err != nil {
		t.Fatal(err)
	}
	out := d.run(2, "--install")
	d.says(out, "holds the lock cactusfs.io/upgrade on the StatefulSet "+first)
	if _, err := d.b.kubectl(ctx, "annotate", "statefulset", first, "-n", d.ns, "cactusfs.io/upgrade-"); err != nil {
		t.Fatal(err)
	}
}

// consolePatch changes the image and the version label of every node's pod
// template as an upgrade from the console does: a strategic merge patch by
// a field manager of its own. No pod restarts (OnDelete).
func (d *deployer) consolePatch() {
	t := d.b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	patch := `{"spec":{"template":{"metadata":{"labels":{"app.kubernetes.io/version":"console"}},"spec":{"containers":[{"name":"cactusfs","image":"cactusfs:console"}]}}}}`
	for _, id := range d.nodes() {
		if _, err := d.b.kubectl(ctx, "patch", "statefulset", id, "-n", d.ns, "--type", "strategic", "--field-manager=cactusfs-upgrade", "-p", patch); err != nil {
			t.Fatal(err)
		}
	}
}

// ownsTheImage: after --take-image every node's template runs the render's
// image (given: the name the import gave it), cactusfs-deploy owns the
// image field by its apply, and the console's field manager owns neither
// the image nor the version label.
func (d *deployer) ownsTheImage(image string) {
	t := d.b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, id := range d.nodes() {
		raw, err := d.b.kubectl(ctx, "get", "statefulset", id, "-n", d.ns, "--show-managed-fields", "-o", "json")
		if err != nil {
			t.Fatal(err)
		}
		var sts struct {
			Metadata struct {
				ManagedFields []struct {
					Manager   string          `json:"manager"`
					Operation string          `json:"operation"`
					FieldsV1  json.RawMessage `json:"fieldsV1"`
				} `json:"managedFields"`
			} `json:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := json.Unmarshal([]byte(raw[strings.Index(raw, "{"):]), &sts); err != nil {
			t.Fatal(err)
		}
		if len(sts.Spec.Template.Spec.Containers) == 0 || sts.Spec.Template.Spec.Containers[0].Image != image {
			t.Errorf("%s does not run the render's image: %v", id, sts.Spec.Template.Spec.Containers)
		}
		deployOwns := false
		for _, m := range sts.Metadata.ManagedFields {
			fields := string(m.FieldsV1)
			switch {
			case m.Manager == "cactusfs-upgrade" && (strings.Contains(fields, `"f:image"`) || strings.Contains(fields, `"f:app.kubernetes.io/version"`)):
				t.Errorf("%s: the console's manager still owns the image or the version label: %s", id, fields)
			case m.Manager == "cactusfs-deploy" && m.Operation == "Apply" && strings.Contains(fields, `"f:image"`):
				deployOwns = true
			}
		}
		if !deployOwns {
			t.Errorf("%s: cactusfs-deploy does not own the image", id)
		}
	}
}

// reachS3 points the bench's client at the S3 NodePort the script
// published (30900, externalTrafficPolicy Local, the NetworkPolicy's
// clientsFrom "any") through the engine's port mapping, and waits - up to
// three minutes - until it completes a TLS handshake under the name the
// certificate holds, and then until a first S3 request is answered (a pod
// is Ready a moment before its endpoint is programmed on the node). When
// either never happens it logs the deployment's Service, EndpointSlices,
// NetworkPolicy and pods, its events and node logs, and the k3s container's
// rules naming the port, and fails.
func (d *deployer) reachS3() {
	b := d.b
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	published, err := b.k3s.MappedPort(ctx, s3NodePort)
	if err != nil {
		t.Fatalf("the published NodePort of S3: %v", err)
	}
	b.s3Address = net.JoinHostPort(b.host, published.Port())
	handshake := func(ctx context.Context) (bool, error) {
		dialer := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: 5 * time.Second},
			Config:    &tls.Config{RootCAs: b.roots, ServerName: release, MinVersion: tls.VersionTLS13},
		}
		conn, err := dialer.DialContext(ctx, "tcp", b.s3Address)
		if err != nil {
			return false, err
		}
		_ = conn.Close()
		return true, nil
	}
	if err := within(ctx, 3*time.Minute, 2*time.Second, handshake); err != nil {
		d.dumpS3(ctx)
		t.Fatalf("the NodePort of %s does not answer TLS at %s: %v", d.name, b.s3Address, err)
	}
	t.Logf("the NodePort of %s answers TLS at %s", d.name, b.s3Address)
	// A first request answered at all - with any status - says the path
	// carries S3; only a request that was not answered is sent again.
	answered := func(ctx context.Context) (bool, error) {
		_, err := b.client().ListBuckets(ctx, &s3.ListBucketsInput{})
		var response *smithyhttp.ResponseError
		if err == nil || errors.As(err, &response) {
			return true, nil
		}
		return false, err
	}
	if err := within(ctx, 2*time.Minute, 2*time.Second, answered); err != nil {
		d.dumpS3(ctx)
		t.Fatalf("S3 of %s through %s answered no request within 2 minutes: %v", d.name, b.s3Address, err)
	}
}

// dumpS3 logs what tells why S3 of the deployment is not reached.
func (d *deployer) dumpS3(ctx context.Context) {
	b := d.b
	for _, args := range [][]string{
		{"kubectl", "get", "service,endpointslices,networkpolicies,pods", "-n", d.ns, "-o", "wide"},
		{"kubectl", "get", "service", d.name, "-n", d.ns, "-o", "yaml"},
		{"kubectl", "get", "networkpolicy", d.name, "-n", d.ns, "-o", "yaml"},
		{"kubectl", "get", "events", "-n", d.ns, "--sort-by=.lastTimestamp"},
		{"kubectl", "logs", "-n", d.ns, "-l", "app.kubernetes.io/instance=" + d.name, "--tail=40", "--prefix"},
		{"sh", "-c", "iptables-save 2>/dev/null | grep -n -e 30900 -e KUBE-SVL -e KUBE-EXT -e KUBE-NWPLCY | head -120"},
		{"sh", "-c", "nft list ruleset 2>/dev/null | grep -n -e 30900 -e 9000 | head -60"},
		{"sh", "-c", "ipset list 2>/dev/null | head -120"},
	} {
		out, err := b.exec(ctx, args...)
		b.t.Logf("---- %s ----\n%s%v", strings.Join(args, " "), tail(out, 6000), err)
	}
}

// idTag is a tag the script gives an image it moves as a tar stream:
// cactusfs:<version>-<build>-<12 hex of its ID> (deploy/lib/image.sh).
var idTag = regexp.MustCompile(`^cactusfs:[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)*-[0-9a-f]{12}$`)

// untagIDTags removes, from the engine here, the tags of the script's form
// that name one of the images given - by their IDs - and nothing else: no
// image is removed, only those names.
func (b *bench) untagIDTags(images ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ids := map[string]bool{}
	for _, image := range images {
		out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image).Output()
		if err == nil {
			ids[strings.TrimSpace(string(out))] = true
		}
	}
	out, err := exec.CommandContext(ctx, "docker", "image", "ls", "--no-trunc", "--format", "{{.Repository}}:{{.Tag}} {{.ID}}", "cactusfs").Output()
	if err != nil {
		b.t.Logf("listing the engine's cactusfs images to untag the script's: %v", err)
		return
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		name, id, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && ids[id] && idTag.MatchString(name) {
			b.untagName(name)
		}
	}
}
