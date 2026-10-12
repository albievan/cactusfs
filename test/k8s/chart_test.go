//go:build k8s

package k8s_test

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
)

// TestChartOnK3s installs the chart, with the value set of a development
// cluster on one machine, on a k3s that enforces NetworkPolicies, in a
// namespace that enforces the restricted Pod Security Standard, and holds the
// running cluster to what the chart's README says of it.
func TestChartOnK3s(t *testing.T) {
	b := newBench(t)
	b.startCluster()
	image := b.loadProductImage()
	b.provision(image)
	b.install()

	b.admittedAsRestricted()
	bucket := "k8s-" + b.run
	data := b.roundTrip(bucket, "first")
	// A node that votes is away, then a pod of the node that does not vote
	// is deleted: both kinds of node, and never two at a time.
	b.readWhileAway(release+"-meta-1", bucket, "first", data)
	b.deletedAndBack(release+"-data-0", bucket, "first", data)
	b.policyHolds()
	// The policy was in force all along: the nodes still reach each other.
	b.stillServes(bucket, []string{"first", "while-away", "after-return"})
}

// startCluster starts k3s and waits until it can run pods that find each
// other by name.
func (b *bench) startCluster() {
	t := b.t
	t.Helper()
	if strings.Contains(k3sImage, "REPLACE_WITH_DIGEST") {
		t.Fatalf("the k3s image is not pinned yet: %s\nPut its digest in test/k8s/bench_test.go; "+
			"\"docker buildx imagetools inspect\" of the tag prints it.", k3sImage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	var err error
	b.k3s, err = runK3s(ctx, "cactusfs-k8s-"+b.run, b.host)
	// Removed with its volumes however the test ends.
	testcontainers.CleanupContainer(t, b.k3s)
	if err != nil {
		t.Fatalf("start k3s - it needs a privileged container, which a rootless engine does not give; "+
			"this test cannot run without one: %v\n%s", err, logsOf(ctx, b.k3s))
	}
	// Registered after the container's removal, so that it runs before it.
	t.Cleanup(b.reportIfFailed)

	running := func(ctx context.Context) error {
		state, err := b.k3s.State(ctx)
		if err != nil {
			return fmt.Errorf("%w: the state of the k3s container: %w", errAbort, err)
		}
		if !state.Running {
			return fmt.Errorf("%w: the k3s container stopped with exit code %d - it needs a privileged container", errAbort, state.ExitCode)
		}
		return nil
	}
	steps := []struct {
		what string
		args []string
		good func(string) bool
	}{
		{"the Kubernetes API", []string{"get", "--raw", "/readyz"}, func(out string) bool { return lastLine(out) == "ok" }},
		{
			"the machine " + machine,
			[]string{"get", "node", machine, "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`},
			func(out string) bool { return lastLine(out) == "True" },
		},
		// The nodes of the store find each other by name.
		{
			"the cluster's DNS",
			[]string{"get", "deployment", "-n", "kube-system", "coredns", "-o", "jsonpath={.status.availableReplicas}"},
			func(out string) bool { return lastLine(out) != "" && lastLine(out) != "0" },
		},
	}
	for _, step := range steps {
		err := eventually(ctx, 2*time.Second, func(ctx context.Context) (bool, error) {
			if err := running(ctx); err != nil {
				return false, err
			}
			out, err := b.kubectl(ctx, step.args...)
			return err == nil && step.good(out), err
		})
		if err != nil {
			t.Fatalf("k3s: %s is not ready: %v\n---- last log lines of k3s ----\n%s", step.what, err, logsOf(ctx, b.k3s))
		}
	}

	// The administrator's kubeconfig, for helm: with the address the API is
	// reached at from here instead of the container's own loopback.
	api, err := b.k3s.MappedPort(ctx, apiPort)
	if err != nil {
		t.Fatalf("the published port of the Kubernetes API: %v", err)
	}
	raw, err := fileOf(ctx, b.k3s, "/etc/rancher/k3s/k3s.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const inside = "https://127.0.0.1:6443"
	if strings.Count(string(raw), inside) != 1 {
		t.Fatalf("the kubeconfig of k3s does not name its API as %s exactly once", inside)
	}
	config := strings.Replace(string(raw), inside, "https://"+net.JoinHostPort(b.host, api.Port()), 1)
	b.kubeconfig = filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(b.kubeconfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	published, err := b.k3s.MappedPort(ctx, s3NodePort)
	if err != nil {
		t.Fatalf("the published NodePort of S3: %v", err)
	}
	b.s3Address = net.JoinHostPort(b.host, published.Port())
}

// reportIfFailed logs the state of the cluster when the test has failed.
func (b *bench) reportIfFailed() {
	if !b.t.Failed() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b.t.Log(b.report(ctx))
}

// loadProductImage puts the image under test into the containerd of k3s,
// without a registry, under a name of this run, and returns the image.
func (b *bench) loadProductImage() string {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	image, err := b.productImage(ctx)
	if err != nil {
		t.Fatalf("product image: %v", err)
	}
	engine, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatalf("container engine: %v", err)
	}
	defer func() { _ = engine.Close() }()
	// Only the name this run gives the image is removed, never the image.
	t.Cleanup(b.untag)
	if err := b.importImage(ctx, engine, image); err != nil {
		t.Fatal(err)
	}
	return image
}

// untag removes the name of this run from the product image. Nothing is
// forced and nothing is pruned: the image keeps the name it came with.
func (b *bench) untag() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	engine, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		return
	}
	defer func() { _ = engine.Close() }()
	_, _ = engine.ImageRemove(ctx, b.loaded(), client.ImageRemoveOptions{})
}

// provision makes what the chart does not: the namespaces, the Secrets the
// chart is told to use, and the directories of the volumes.
func (b *bench) provision(image string) {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// The certificates and the key file are the product's own work, as for
	// the compose cluster; they exist for this run only.
	phrase := randomHex(t, 24)
	files, err := devInit(ctx, t, image, phrase)
	if err != nil {
		t.Fatal(err)
	}
	b.roots = x509.NewCertPool()
	if !b.roots.AppendCertsFromPEM(files["s3-ca.pem"]) {
		t.Fatal("the S3 CA file holds no certificate")
	}

	// Admission is the judge of the pods: this namespace takes none that
	// does not meet the restricted standard.
	objects := []object{
		named("Namespace", "", namespace, map[string]string{
			"pod-security.kubernetes.io/enforce":         "restricted",
			"pod-security.kubernetes.io/enforce-version": "latest",
		}),
		named("Namespace", "", elsewhere, nil),
		// The names are those of ci/single-machine-values.yaml; the keys
		// are those the chart's README lists.
		sealed("cactusfs-root", "Opaque", map[string][]byte{
			"access-key": []byte(b.accessKey), "secret-key": []byte(b.secretKey),
		}),
		sealed("cactusfs-tls", "kubernetes.io/tls", map[string][]byte{"tls.crt": files["s3.pem"], "tls.key": files["s3.key"]}),
		sealed("cactusfs-node-ca", "Opaque", map[string][]byte{"ca.crt": files["node-ca.pem"]}),
		sealed("cactusfs-kms", "Opaque", map[string][]byte{"kms.json": files["kms.json"]}),
		sealed("cactusfs-kms-passphrase", "Opaque", map[string][]byte{"passphrase": []byte(phrase)}),
	}
	for _, id := range nodeIDs() {
		// A Secret for every node, with its key and nobody else's.
		objects = append(objects, sealed(id+"-node-tls", "kubernetes.io/tls", map[string][]byte{
			"tls.crt": files["nodes/"+id+".pem"], "tls.key": files["nodes/"+id+".key"],
		}))
	}
	for _, o := range objects {
		if err := b.apply(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	// localVolumes: <basePath>/<node ID>/<volume>, belonging to the user the
	// pods run as. Two drives, as the value set has it.
	mkdir := []string{"mkdir", "-p"}
	for _, id := range nodeIDs() {
		for _, volume := range []string{"meta", "data1", "data2"} {
			mkdir = append(mkdir, volumeBase+"/"+id+"/"+volume)
		}
	}
	if _, err := b.exec(ctx, mkdir...); err != nil {
		t.Fatal(err)
	}
	owner := fmt.Sprintf("%d:%d", podUser, podUser)
	if _, err := b.exec(ctx, "chown", "-R", owner, volumeBase); err != nil {
		t.Fatal(err)
	}
}

// install installs the chart with helm, from here, and waits until every
// node's pod is ready.
func (b *bench) install() {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	chart := filepath.Join(b.root, "deploy", "helm", "cactusfs")
	install := exec.CommandContext(ctx, "helm", "install", release, chart,
		"--namespace", namespace, "--kubeconfig", b.kubeconfig,
		"-f", filepath.Join(chart, "ci", "single-machine-values.yaml"),
		"-f", filepath.Join(b.root, "test", "k8s", "testdata", "values.yaml"),
		"--set-string", "image.repository="+loadedImage,
		"--set-string", "image.tag="+b.run,
		"--set-string", "localVolumes.nodeName="+machine,
		"--set-string", "localVolumes.basePath="+volumeBase,
		"--wait", "--timeout", "8m")
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("helm install: %v\n%s", err, tail(string(out), 6000))
	}
	// helm's own wait is not relied on: it may call a StatefulSet whose
	// update strategy is OnDelete done before its pod is ready.
	if err := b.waitReady(ctx, namespace, pods()...); err != nil {
		t.Fatalf("the cluster did not form: %v", err)
	}
}

func pods() []string {
	names := make([]string, 0, len(nodeIDs()))
	for _, id := range nodeIDs() {
		names = append(names, podOf(id))
	}
	return names
}

// admittedAsRestricted checks that the pods were admitted by a namespace
// that enforces the restricted standard - and that the namespace does
// enforce it, by offering it a pod that does not meet it.
func (b *bench) admittedAsRestricted() {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	level, err := b.kubectl(ctx, "get", "namespace", namespace, "-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}`)
	if err != nil || lastLine(level) != "restricted" {
		t.Fatalf("the namespace enforces %q, not restricted (%v)", level, err)
	}
	// A pod that is refused is an event of its StatefulSet.
	events, err := b.kubectl(ctx, "get", "events", "-n", namespace, "-o", `jsonpath={range .items[*]}{.reason}{": "}{.message}{"\n"}{end}`)
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(events, "\n") {
		if strings.Contains(line, "PodSecurity") {
			t.Fatalf("a pod of the chart was refused: %s", line)
		}
	}
	for _, pod := range pods() {
		if _, ready, err := b.podState(ctx, namespace, pod); err != nil || !ready {
			t.Fatalf("pod %s is not ready (%v)", pod, err)
		}
	}
	// The control: a pod that says nothing of how it runs is not admitted.
	// Without it, pods that are admitted would prove nothing.
	plain := named("Pod", namespace, "not-restricted", nil)
	plain["spec"] = map[string]any{"containers": []any{map[string]any{"name": "main", "image": probeImage}}}
	err = b.apply(ctx, plain)
	if err == nil {
		t.Fatal("the namespace admitted a pod that does not meet the restricted standard: it enforces nothing")
	}
	if !strings.Contains(err.Error(), "violates PodSecurity") {
		t.Fatalf("a pod that does not meet the restricted standard was refused, but not for that: %v", err)
	}
}

// roundTrip makes a bucket, writes an object through the S3 Service - over
// TLS, checked against the CA of this run, with the root credentials - and
// reads it back. It returns the object.
func (b *bench) roundTrip(bucket, key string) []byte {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// More than one segment of the cipher, and not a whole number of them.
	data := make([]byte, 3<<20+17)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if _, err := b.client().CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create the bucket through %s: %v", b.s3Address, err)
	}
	if err := b.put(ctx, bucket, key, data); err != nil {
		t.Fatalf("PUT: %v", err)
	}
	if _, err := b.reads(ctx, bucket, key, data); err != nil {
		t.Fatalf("GET: %v", err)
	}
	return data
}

// readWhileAway takes a node away - its StatefulSet is scaled to no pod, so
// that the node stays away for as long as the test looks - reads the object
// and writes another while it is gone, and brings the node back.
//
// Deleting the pod alone would not do for this: the StatefulSet makes a new
// one at once, and the time in which the node is truly gone is too short to
// be sure a read fell into it. deletedAndBack does that.
func (b *bench) readWhileAway(id, bucket, key string, data []byte) {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	pod := podOf(id)

	if _, err := b.kubectl(ctx, "scale", "statefulset", "-n", namespace, id, "--replicas=0"); err != nil {
		t.Fatal(err)
	}
	gone := func(ctx context.Context) (bool, error) {
		uid, _, err := b.podState(ctx, namespace, pod)
		return err == nil && uid == "", err
	}
	// The pod drains before it stops (the chart's termination grace period).
	if err := within(ctx, 4*time.Minute, time.Second, gone); err != nil {
		t.Fatalf("pod %s did not go away: %v", pod, err)
	}

	// The others may need a moment to agree on who leads, when the node
	// that left led the metadata group.
	read := func(ctx context.Context) (bool, error) { return b.reads(ctx, bucket, key, data) }
	if err := within(ctx, time.Minute, time.Second, read); err != nil {
		t.Fatalf("with %s away the object is not read: %v", id, err)
	}
	// After that every read is good.
	for i := range 5 {
		if _, err := b.reads(ctx, bucket, key, data); err != nil {
			t.Fatalf("with %s away, read %d of the object failed: %v", id, i+1, err)
		}
	}
	// And a write is accepted: one node of four may be away.
	written := []byte("written while " + id + " was away")
	write := func(ctx context.Context) (bool, error) {
		err := b.put(ctx, bucket, "while-away", written)
		return err == nil, err
	}
	if err := within(ctx, time.Minute, time.Second, write); err != nil {
		t.Fatalf("with %s away a write is not accepted: %v", id, err)
	}
	if _, err := b.reads(ctx, bucket, "while-away", written); err != nil {
		t.Fatalf("with %s away, what was written is not read: %v", id, err)
	}
	// All of that happened without the node: it is still gone.
	if away, err := gone(ctx); err != nil || !away {
		t.Fatalf("pod %s came back by itself; the reads prove nothing (%v)", pod, err)
	}

	if _, err := b.kubectl(ctx, "scale", "statefulset", "-n", namespace, id, "--replicas=1"); err != nil {
		t.Fatal(err)
	}
	back, stop := context.WithTimeout(ctx, 6*time.Minute)
	defer stop()
	if err := b.waitReady(back, namespace, pod); err != nil {
		t.Fatalf("%s did not come back: %v", id, err)
	}
	if _, err := b.reads(ctx, bucket, key, data); err != nil {
		t.Fatalf("with %s back the object is not read: %v", id, err)
	}
	if _, err := b.reads(ctx, bucket, "while-away", written); err != nil {
		t.Fatalf("with %s back, what was written without it is not read: %v", id, err)
	}
}

// deletedAndBack deletes the pod of a node, as an operator does to roll a
// change out (updateStrategy OnDelete), and checks that the StatefulSet
// makes a new pod, that the new pod becomes ready, and that the cluster then
// reads what it had and takes a write.
func (b *bench) deletedAndBack(id, bucket, key string, data []byte) {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pod := podOf(id)

	before, ready, err := b.podState(ctx, namespace, pod)
	if err != nil || before == "" || !ready {
		t.Fatalf("pod %s is not ready before it is deleted (%v)", pod, err)
	}
	if _, err := b.kubectl(ctx, "delete", "pod", "-n", namespace, pod, "--wait=false"); err != nil {
		t.Fatal(err)
	}
	// While the pod drains, stops and is made anew, the object goes on
	// being read. What is returned must be the object, every time. A read
	// that fails in that window is counted and told, not held against the
	// chart: how long the node is truly gone is not in the test's hands
	// here - readWhileAway is where a read without the node must succeed.
	asked, failed := 0, 0
	replaced := func(ctx context.Context) (bool, error) {
		asked++
		if _, err := b.reads(ctx, bucket, key, data); errors.Is(err, errAbort) {
			return false, err
		} else if err != nil {
			failed++
			t.Logf("while pod %s was replaced, read %d failed: %v", pod, asked, err)
		}
		uid, ready, err := b.podState(ctx, namespace, pod)
		return err == nil && uid != "" && uid != before && ready, err
	}
	if err := within(ctx, 8*time.Minute, time.Second, replaced); err != nil {
		t.Fatalf("pod %s was deleted and no new pod of that name became ready: %v", pod, err)
	}
	t.Logf("while pod %s was replaced: %d reads, %d failed", pod, asked, failed)
	if _, err := b.reads(ctx, bucket, key, data); err != nil {
		t.Fatalf("with the new pod %s the object is not read: %v", pod, err)
	}
	written := []byte("written after " + id + " returned")
	if err := b.put(ctx, bucket, "after-return", written); err != nil {
		t.Fatalf("with the new pod %s a write is not accepted: %v", pod, err)
	}
	if _, err := b.reads(ctx, bucket, "after-return", written); err != nil {
		t.Fatalf("with the new pod %s, what was written is not read: %v", pod, err)
	}
}

// within is eventually, with a limit of its own.
func within(ctx context.Context, limit, every time.Duration, condition func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	return eventually(ctx, every, condition)
}

// policyHolds checks the NetworkPolicy from two probe pods.
//
// One is outside the release, in another namespace. It must reach port 9000
// of every node, which the test's values open to everybody, and must not
// reach 9001, 9002 or 9003. The connection to 9000 is the control: it is
// made from the same pod, to the same address, by the same command as the
// ones that must fail. If it fails, the test fails - nothing was learned of
// the policy. If the others succeed, the cluster's network enforces no
// policy, or the chart's does not close the ports: the test fails for that.
//
// The other probe is in the release's namespace and carries the labels the
// chart selects its pods by. It must reach 9002 and 9003: the node ports
// answer, and are closed to the first probe for what it is, not because
// nothing listens.
func (b *bench) policyHolds() {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	const outsider, insider = "outsider", "insider"
	probes := []object{
		probePod(elsewhere, outsider, nil),
		// The labels of "cactusfs.selector" in the chart's _helpers.tpl.
		probePod(namespace, insider, map[string]string{
			"app.kubernetes.io/name": "cactusfs", "app.kubernetes.io/instance": release,
		}),
	}
	for _, probe := range probes {
		if err := b.apply(ctx, probe); err != nil {
			t.Fatal(err)
		}
	}
	started, stop := context.WithTimeout(ctx, 5*time.Minute)
	defer stop()
	if err := b.waitReady(started, elsewhere, outsider); err != nil {
		t.Fatalf("the probe did not start (k3s pulls its image, %s): %v", probeImage, err)
	}
	if err := b.waitReady(started, namespace, insider); err != nil {
		t.Fatalf("the probe did not start: %v", err)
	}

	reaches := func(ns, pod, ip, port string) func(context.Context) (bool, error) {
		return func(ctx context.Context) (bool, error) { return b.connects(ctx, ns, pod, ip, port) }
	}
	for _, id := range nodeIDs() {
		ip, err := b.podIP(ctx, namespace, podOf(id))
		if err != nil {
			t.Fatal(err)
		}
		// The control comes first.
		if err := within(ctx, time.Minute, 2*time.Second, reaches(elsewhere, outsider, ip, "9000")); err != nil {
			t.Fatalf("control: a pod outside the release cannot reach %s (%s) on 9000, which the policy opens to it; "+
				"so that it cannot reach the other ports says nothing: %v", id, ip, err)
		}
		for _, port := range []string{"9001", "9002", "9003"} {
			open, err := b.connects(ctx, elsewhere, outsider, ip, port)
			if err != nil {
				t.Fatal(err)
			}
			if open {
				t.Fatalf("a pod outside the release reached %s (%s) on %s: the policy does not close the port, "+
					"or the cluster's network enforces no NetworkPolicy", id, ip, port)
			}
		}
		// A policy controller may take a moment to learn of a new pod.
		for _, port := range []string{"9002", "9003"} {
			if err := within(ctx, time.Minute, 2*time.Second, reaches(namespace, insider, ip, port)); err != nil {
				t.Fatalf("control: a pod with the release's labels cannot reach %s (%s) on %s: %v", id, ip, port, err)
			}
		}
	}
	// The refusals hold once the controller has seen both probes as well.
	for _, id := range nodeIDs() {
		ip, err := b.podIP(ctx, namespace, podOf(id))
		if err != nil {
			t.Fatal(err)
		}
		for _, port := range []string{"9002", "9003"} {
			if open, err := b.connects(ctx, elsewhere, outsider, ip, port); err != nil || open {
				t.Fatalf("a pod outside the release reached %s (%s) on %s (%v)", id, ip, port, err)
			}
		}
	}
	// The insider carries the release's labels: it goes before the release
	// is looked at again.
	if _, err := b.kubectl(ctx, "delete", "pod", "-n", namespace, insider, "--wait=true"); err != nil {
		t.Fatal(err)
	}
}

// stillServes checks, last of all, that every node is ready and that the
// cluster answers for everything the test wrote and takes one more write: the
// nodes reach each other under the policy that kept the probe out.
func (b *bench) stillServes(bucket string, keys []string) {
	t := b.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	for _, pod := range pods() {
		if _, ready, err := b.podState(ctx, namespace, pod); err != nil || !ready {
			t.Fatalf("at the end pod %s is not ready (%v)", pod, err)
		}
	}
	for _, key := range keys {
		if _, err := b.get(ctx, bucket, key); err != nil {
			t.Fatalf("at the end %s is not read: %v", key, err)
		}
	}
	last := []byte("the last write")
	if err := b.put(ctx, bucket, "last", last); err != nil {
		t.Fatalf("at the end a write is not accepted: %v", err)
	}
	if _, err := b.reads(ctx, bucket, "last", last); err != nil {
		t.Fatalf("at the end, what was written is not read: %v", err)
	}
}
