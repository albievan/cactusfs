//go:build k8s

package k8s_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// k3sImage is the Kubernetes the chart is installed on: k3s of the minor
	// version whose schemas "make helm-lint" validates the chart against
	// (K8S_SCHEMAS_VERSION in the Dockerfile), pinned by digest. To move the
	// pin, change the tag AND the digest together:
	//
	//	docker buildx imagetools inspect rancher/k3s:<tag>
	//
	// prints the digest of the index ("Digest: sha256:...").
	k3sImage = "rancher/k3s:v1.36.5-k3s1@sha256:057cfd5a9d51b89f1f0ee139c28d5d5fdd5aa7ddeac2742a0ba90c6dbc0329bb"

	// probeImage has a shell and nc, which the product image has not: the
	// image, and the digest, of the helper of test/clustertest. k3s pulls it
	// itself, as it pulls its own pause and CoreDNS images.
	probeImage = "docker.io/library/alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8"

	// release names the nodes: <release>-<pool>-<index>, with the pools of
	// the chart's values.yaml - three that vote and one that does not.
	release   = "store"
	namespace = "cactusfs"
	// elsewhere is a namespace that is not the release's.
	elsewhere = "elsewhere"

	// machine is the name of the one Kubernetes node, which the volumes of
	// localVolumes are bound to; volumeBase is where they lie on it, in the
	// volume the k3s image keeps its own state in.
	machine    = "k3s"
	volumeBase = "/var/lib/rancher/k3s/cactusfs-test"

	// loadedImage is the name the product image has inside the cluster; its
	// tag is the run.
	loadedImage = "cactusfs-k8s-test"

	apiPort = "6443/tcp"
	// s3NodePort is service.nodePort of ci/single-machine-values.yaml.
	s3NodePort = "30900/tcp"

	// What the pods run as (the chart's securityContext).
	podUser = 65532

	configDir = "/var/lib/cactusfs/config"
)

// errAbort ends a wait at once: what is waited for cannot happen any more.
var errAbort = errors.New("gave up")

// nodeIDs are the nodes the chart makes of the default pools.
func nodeIDs() []string {
	return []string{release + "-meta-0", release + "-meta-1", release + "-meta-2", release + "-data-0"}
}

// podOf returns the pod of a node: the one pod of its StatefulSet.
func podOf(id string) string { return id + "-0" }

// bench is one k3s with the chart on it.
type bench struct {
	t    *testing.T
	run  string
	root string

	k3s        *testcontainers.DockerContainer
	host       string
	kubeconfig string
	s3Address  string

	accessKey string
	secretKey string
	roots     *x509.CertPool

	files int
}

func newBench(t *testing.T) *bench {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatalf("helm is not installed (it is in the builder image: run make test-k8s): %v", err)
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	host, err := engineHost()
	if err != nil {
		t.Fatalf("the address of the container engine: %v", err)
	}
	return &bench{
		t: t, run: randomHex(t, 4), root: root, host: host,
		// Made up for this run; written nowhere but into the cluster's
		// Secrets, which go with the container.
		accessKey: "TEST" + strings.ToUpper(randomHex(t, 8)),
		secretKey: randomHex(t, 24),
	}
}

// engineHost returns the address at which the container engine publishes
// the ports of its containers, as seen from here - from inside the builder
// container too, which is how test/clustertest reaches its nodes.
func engineHost() (string, error) {
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return "", err
	}
	defer func() { _ = provider.Close() }()
	return provider.DaemonHost(context.Background())
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the working directory")
		}
		dir = parent
	}
}

func randomHex(t *testing.T, bytesLen int) string {
	t.Helper()
	raw := make([]byte, bytesLen)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// lastLine returns the last line of output that is not empty: what a command
// printed, after any warning that came before it.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// eventually asks condition again and again until it holds, the context ends
// or the condition says, with errAbort, that it never will.
func eventually(ctx context.Context, every time.Duration, condition func(context.Context) (bool, error)) error {
	var last error
	for {
		ok, err := condition(ctx)
		if ok {
			return nil
		}
		if errors.Is(err, errAbort) {
			return err
		}
		if err != nil {
			last = err
		}
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("%w; the last answer was: %w", ctx.Err(), last)
			}
			return ctx.Err()
		case <-time.After(every):
		}
	}
}

// productImage returns the image under test: $CACTUSFS_TEST_IMAGE when set,
// otherwise it is built from this tree, as test/clustertest builds it and
// under the same name.
func (b *bench) productImage(ctx context.Context) (string, error) {
	if name := os.Getenv("CACTUSFS_TEST_IMAGE"); name != "" {
		return name, nil
	}
	const name = "cactusfs:test"
	build := exec.CommandContext(ctx, "docker", "build", "--quiet", "-t", name, b.root)
	if out, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("docker build: %w\n%s", err, tail(string(out), 4000))
	}
	return name, nil
}

// loaded is the name this run gives the product image: in the engine, to
// save it under, and so in the cluster.
func (b *bench) loaded() string { return loadedImage + ":" + b.run }

// importImage gives the image the name of this run, takes it out of the
// engine as an archive and imports that into the containerd of k3s.
func (b *bench) importImage(ctx context.Context, engine *testcontainers.DockerClient, image string) error {
	if _, err := engine.ImageTag(ctx, client.ImageTagOptions{Source: image, Target: b.loaded()}); err != nil {
		return fmt.Errorf("name %s %s: %w", image, b.loaded(), err)
	}
	saved, err := engine.ImageSave(ctx, []string{b.loaded()})
	if err != nil {
		return fmt.Errorf("save %s: %w", b.loaded(), err)
	}
	defer func() { _ = saved.Close() }()
	// The image is smaller than 60 MB (make image holds it to that).
	archive, err := io.ReadAll(saved)
	if err != nil {
		return fmt.Errorf("save %s: %w", b.loaded(), err)
	}
	const inside = "/tmp/cactusfs-test-product.tar"
	if err := b.k3s.CopyToContainer(ctx, archive, inside, 0o600); err != nil {
		return fmt.Errorf("copy the image into the k3s container: %w", err)
	}
	if _, err := b.exec(ctx, "ctr", "-n", "k8s.io", "images", "import", inside); err != nil {
		return fmt.Errorf("import the image: %w", err)
	}
	if _, err := b.exec(ctx, "rm", "-f", inside); err != nil {
		return err
	}
	// With pullPolicy Never a pod starts only if the kubelet finds the image
	// under the very name the chart gives it.
	names, err := b.exec(ctx, "ctr", "-n", "k8s.io", "images", "ls", "-q")
	if err != nil {
		return err
	}
	if !strings.Contains(names, "/"+b.loaded()) {
		return fmt.Errorf("the image was imported, but not as %s; containerd has:\n%s", b.loaded(), names)
	}
	return nil
}

func logsOf(ctx context.Context, ctr *testcontainers.DockerContainer) string {
	if ctr == nil {
		return ""
	}
	reader, err := ctr.Logs(ctx)
	if err != nil {
		return ""
	}
	defer func() { _ = reader.Close() }()
	data, _ := io.ReadAll(reader)
	return tail(string(data), 6000)
}

func fileOf(ctx context.Context, ctr *testcontainers.DockerContainer, path string) ([]byte, error) {
	reader, err := ctr.CopyFileFromContainer(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}

// runK3s starts the k3s server: one container that is the control plane and
// the one machine of the cluster.
//
// It is privileged, because the kubelet and containerd in it make containers
// of their own. Flannel, its network, enforces no NetworkPolicy; k3s's own
// policy controller (kube-router's) does, and is on unless it is disabled
// with --disable-network-policy, which this does not do. The test does not
// take that on trust: see policyHolds.
//
// Left out is what the chart must not depend on, and does not: an ingress
// controller, a load balancer, a storage driver.
func runK3s(ctx context.Context, name, host string) (*testcontainers.DockerContainer, error) {
	return testcontainers.Run(ctx, k3sImage,
		testcontainers.WithName(name),
		testcontainers.WithCmd("server",
			"--node-name="+machine,
			// The API is reached from outside the container at the engine's
			// address, which its certificate must therefore name.
			"--tls-san="+host,
			"--disable=traefik",
			"--disable=servicelb",
			"--disable=metrics-server",
			"--disable=local-storage",
			// A test machine's disk may be fuller than a kubelet likes; it
			// must not evict the pods under test for that.
			"--kubelet-arg=eviction-hard=imagefs.available<1%,nodefs.available<1%",
			"--kubelet-arg=eviction-minimum-reclaim=imagefs.available=1%,nodefs.available=1%",
		),
		testcontainers.WithExposedPorts(apiPort, s3NodePort),
		testcontainers.WithTmpfs(map[string]string{"/run": "", "/var/run": ""}),
		testcontainers.WithHostConfigModifier(func(hostConfig *container.HostConfig) {
			hostConfig.Privileged = true
			hostConfig.CgroupnsMode = container.CgroupnsModeHost
		}),
	)
}

// devInit runs the product's own "dev-init" in a container of the image and
// returns the files it wrote, by their names in its directory: the two CAs,
// a certificate and key for every node and for the listeners, the sealed KMS
// key file.
func devInit(ctx context.Context, t *testing.T, image, phrase string) (map[string][]byte, error) {
	ctr, err := testcontainers.Run(ctx, image,
		testcontainers.WithCmd("dev-init", "--dir", configDir, "--nodes", strings.Join(nodeIDs(), ","),
			// The name of the S3 Service, which the test's client speaks to.
			"--hosts", release),
		testcontainers.WithEnv(map[string]string{"CACTUSFS_KMS_PASSPHRASE": phrase}),
		testcontainers.WithWaitStrategy(wait.ForExit().WithExitTimeout(time.Minute)),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		return nil, fmt.Errorf("dev-init: %w", err)
	}
	state, err := ctr.State(ctx)
	if err != nil {
		return nil, fmt.Errorf("dev-init: %w", err)
	}
	if state.ExitCode != 0 {
		return nil, fmt.Errorf("dev-init exited with %d\n%s", state.ExitCode, logsOf(ctx, ctr))
	}
	names := []string{"node-ca.pem", "s3-ca.pem", "s3.pem", "s3.key", "kms.json"}
	for _, id := range nodeIDs() {
		names = append(names, "nodes/"+id+".pem", "nodes/"+id+".key")
	}
	files := map[string][]byte{}
	for _, name := range names {
		if files[name], err = fileOf(ctx, ctr, configDir+"/"+name); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// exec runs a command in the k3s container and returns what it printed.
func (b *bench) exec(ctx context.Context, cmd ...string) (string, error) {
	code, reader, err := b.k3s.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		return "", fmt.Errorf("%s: %w", cmd[0], err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("%s: %w", cmd[0], err)
	}
	out := string(raw)
	if code != 0 {
		return out, fmt.Errorf("%s exited with %d:\n%s", strings.Join(cmd, " "), code, tail(out, 4000))
	}
	return out, nil
}

// kubectl runs the kubectl of the k3s container, which is the cluster's
// administrator. The builder image has none.
func (b *bench) kubectl(ctx context.Context, args ...string) (string, error) {
	return b.exec(ctx, append([]string{"kubectl"}, args...)...)
}

// object is a Kubernetes object, as the JSON kubectl applies.
type object map[string]any

func named(kind, ns, name string, labels map[string]string) object {
	meta := map[string]any{"name": name}
	if ns != "" {
		meta["namespace"] = ns
	}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	return object{"apiVersion": "v1", "kind": kind, "metadata": meta}
}

// sealed returns a Secret of the release's namespace. kind is its type.
func sealed(name, kind string, data map[string][]byte) object {
	encoded := map[string]string{}
	for key, value := range data {
		encoded[key] = base64.StdEncoding.EncodeToString(value)
	}
	o := named("Secret", namespace, name, nil)
	o["type"] = kind
	o["data"] = encoded
	return o
}

// apply creates or updates an object. The object goes into the container as
// a file, which is removed again; what kubectl says of a Secret is not
// repeated in the error.
func (b *bench) apply(ctx context.Context, o object) error {
	raw, err := json.Marshal(o)
	if err != nil {
		return err
	}
	b.files++
	path := fmt.Sprintf("/tmp/cactusfs-test-%d.json", b.files)
	if err := b.k3s.CopyToContainer(ctx, raw, path, 0o600); err != nil {
		return fmt.Errorf("copy an object into the k3s container: %w", err)
	}
	_, applyErr := b.kubectl(ctx, "apply", "-f", path)
	if _, err := b.exec(ctx, "rm", "-f", path); err != nil {
		return err
	}
	if applyErr != nil && o["kind"] == "Secret" {
		return fmt.Errorf("kubectl apply of a Secret failed (%v; its output is not shown)", o["metadata"])
	}
	return applyErr
}

// podState returns the UID of a pod and whether it is ready. A pod that does
// not exist has no UID.
func (b *bench) podState(ctx context.Context, ns, pod string) (uid string, ready bool, err error) {
	out, err := b.kubectl(ctx, "get", "pod", "-n", ns, pod, "--ignore-not-found",
		"-o", `jsonpath={.metadata.uid}{" "}{.status.conditions[?(@.type=="Ready")].status}`)
	if err != nil {
		return "", false, err
	}
	fields := strings.Fields(lastLine(out))
	if len(fields) == 0 {
		return "", false, nil
	}
	return fields[0], len(fields) > 1 && fields[1] == "True", nil
}

// podIP returns the address of a pod.
func (b *bench) podIP(ctx context.Context, ns, pod string) (string, error) {
	out, err := b.kubectl(ctx, "get", "pod", "-n", ns, pod, "-o", "jsonpath={.status.podIP}")
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(lastLine(out))
	if ip == nil {
		return "", fmt.Errorf("pod %s/%s has no address: %q", ns, pod, out)
	}
	return ip.String(), nil
}

// waitReady waits until every pod named is ready.
func (b *bench) waitReady(ctx context.Context, ns string, pods ...string) error {
	for _, pod := range pods {
		err := eventually(ctx, 2*time.Second, func(ctx context.Context) (bool, error) {
			_, ready, err := b.podState(ctx, ns, pod)
			return ready, err
		})
		if err != nil {
			return fmt.Errorf("pod %s/%s is not ready: %w", ns, pod, err)
		}
	}
	return nil
}

// Markers of what a probe found, so that a probe that could not be run at all
// is never read as a refusal.
const (
	probeOpen   = "PROBE-OPEN"
	probeClosed = "PROBE-CLOSED"
)

// connects reports whether a TCP connection from a probe pod to an address
// and port is established within three seconds.
func (b *bench) connects(ctx context.Context, ns, pod, ip, port string) (bool, error) {
	out, err := b.kubectl(ctx, "exec", "-n", ns, pod, "--", "sh", "-c",
		`if nc -z -w 3 "$0" "$1"; then echo `+probeOpen+`; else echo `+probeClosed+`; fi`, ip, port)
	if err != nil {
		return false, err
	}
	open, closed := strings.Contains(out, probeOpen), strings.Contains(out, probeClosed)
	if open == closed {
		return false, fmt.Errorf("the probe in %s/%s said neither that %s:%s is open nor that it is closed: %q", ns, pod, ip, port, out)
	}
	return open, nil
}

// probePod is a pod that does nothing, to make connections from. It meets
// the restricted Pod Security Standard, so that it is admitted to the
// release's namespace too.
func probePod(ns, name string, labels map[string]string) object {
	o := named("Pod", ns, name, labels)
	o["spec"] = map[string]any{
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"restartPolicy":                 "Never",
		"terminationGracePeriodSeconds": 0,
		"securityContext": map[string]any{
			"runAsNonRoot": true, "runAsUser": podUser, "runAsGroup": podUser,
			"seccompProfile": map[string]any{"type": "RuntimeDefault"},
		},
		"containers": []any{map[string]any{
			"name": "probe", "image": probeImage, "imagePullPolicy": "IfNotPresent",
			"command": []string{"sleep", "3600"},
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
				"capabilities": map[string]any{"drop": []string{"ALL"}},
			},
		}},
	}
	return o
}

// report returns what tells why pods are not as they should be: the pods,
// their StatefulSets and claims, the events, and the last lines every node
// logged.
func (b *bench) report(ctx context.Context) string {
	var out strings.Builder
	for _, args := range [][]string{
		{"get", "pods,statefulsets,persistentvolumeclaims,networkpolicies", "-n", namespace, "-o", "wide"},
		{"get", "events", "-n", namespace, "--sort-by=.lastTimestamp"},
		{"get", "pods", "-n", elsewhere, "-o", "wide"},
	} {
		text, _ := b.kubectl(ctx, args...)
		fmt.Fprintf(&out, "---- kubectl %s ----\n%s\n", strings.Join(args, " "), tail(text, 8000))
	}
	for _, id := range nodeIDs() {
		text, _ := b.kubectl(ctx, "logs", "-n", namespace, podOf(id), "--tail=40")
		fmt.Fprintf(&out, "---- last log lines of %s ----\n%s\n", podOf(id), tail(text, 6000))
	}
	return out.String()
}

// client returns an S3 client that comes in through the NodePort of the S3
// Service while speaking to it under the Service's name, which is a name in
// the listeners' certificate. It does not retry: a test sees exactly what
// the cluster answered.
func (b *bench) client() *s3.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", b.s3Address)
		},
		TLSClientConfig:       &tls.Config{RootCAs: b.roots, ServerName: release, MinVersion: tls.VersionTLS13},
		ResponseHeaderTimeout: 30 * time.Second,
		// Every request is a new connection, which the Service sends to any
		// of the nodes that are ready.
		DisableKeepAlives: true,
	}
	cfg := aws.Config{
		Region:                     "us-east-1",
		Credentials:                credentials.NewStaticCredentialsProvider(b.accessKey, b.secretKey, ""),
		HTTPClient:                 &http.Client{Transport: transport, Timeout: time.Minute},
		Retryer:                    func() aws.Retryer { return retry.AddWithMaxAttempts(retry.NewStandard(), 1) },
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenSupported,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenSupported,
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("https://" + release + ":9000")
		o.UsePathStyle = true
	})
}

func (b *bench) put(ctx context.Context, bucket, key string, data []byte) error {
	_, err := b.client().PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data)})
	return err
}

func (b *bench) get(ctx context.Context, bucket, key string) ([]byte, error) {
	out, err := b.client().GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()
	return io.ReadAll(out.Body)
}

// reads reports whether an object is read back as the bytes given.
func (b *bench) reads(ctx context.Context, bucket, key string, want []byte) (bool, error) {
	got, err := b.get(ctx, bucket, key)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(got, want) {
		// Never a matter of waiting: what was returned is not the object.
		return false, fmt.Errorf("%w: %s/%s was read as %d bytes that are not the %d written", errAbort, bucket, key, len(got), len(want))
	}
	return true, nil
}
