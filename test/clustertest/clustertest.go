//go:build integration || chaos

// Package clustertest runs a real CactusFS cluster for the integration and
// chaos suites: four containers of the product image, formed into a cluster
// over mutual TLS exactly as deploy/compose/compose.yaml does it, and reached
// through the S3 API with an ordinary S3 client.
//
// Each node is on two networks. "cluster" carries the node-to-node traffic
// under the node names; a test cuts a node off by disconnecting it from that
// network. "edge" carries the published S3 port, so that a node that is cut
// off from its peers can still be asked.
package clustertest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	// helperImage is a small public image with a shell, pinned by digest. It
	// is used to look at and tamper with the files in a node's volumes: the
	// product image has no shell.
	helperImage = "alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8"

	configDir = "/var/lib/cactusfs/config"
	dataRoot  = "/var/lib/cactusfs"
	s3Port    = "9000/tcp"
	adminPort = "9001/tcp"

	startTimeout = 3 * time.Minute
)

// Nodes are the nodes of the test cluster. The first three vote in the
// metadata group; all four store shards and serve S3.
var Nodes = []string{"node1", "node2", "node3", "node4"}

// Options adjust the cluster.
type Options struct {
	// Env is added to the environment of every node.
	Env map[string]string
}

// Cluster is a running test cluster.
type Cluster struct {
	t       *testing.T
	image   string
	docker  *client.Client
	edge    *testcontainers.DockerNetwork
	cluster *testcontainers.DockerNetwork
	prefix  string
	env     map[string]string

	// AccessKey and SecretKey are the root credential of the cluster.
	AccessKey string
	SecretKey string
	roots     *x509.CertPool

	mu         sync.Mutex
	containers map[string]*testcontainers.DockerContainer
}

var (
	imageOnce sync.Once
	imageName string
	errImage  error
)

// image returns the product image under test: $CACTUSFS_TEST_IMAGE when set,
// otherwise it is built from this repository, once per test binary.
func image(ctx context.Context) (string, error) {
	imageOnce.Do(func() {
		if name := os.Getenv("CACTUSFS_TEST_IMAGE"); name != "" {
			imageName = name
			return
		}
		root, err := repoRoot()
		if err != nil {
			errImage = err
			return
		}
		imageName = "cactusfs:test"
		build := exec.CommandContext(ctx, "docker", "build", "--quiet", "-t", imageName, root)
		if out, err := build.CombinedOutput(); err != nil {
			errImage = fmt.Errorf("docker build: %w\n%s", err, out)
		}
	})
	return imageName, errImage
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

// Start forms a new cluster and waits until every node serves.
func Start(t *testing.T, opts Options) *Cluster {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	img, err := image(ctx)
	if err != nil {
		t.Fatalf("product image: %v", err)
	}
	provider, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	docker := provider.Client

	c := &Cluster{
		t: t, image: img, docker: docker, prefix: "cactusfs-test-" + randomHex(t, 4),
		containers: map[string]*testcontainers.DockerContainer{},
		// Credentials and passphrase are made up for this run; none is
		// written anywhere.
		AccessKey: "TEST" + strings.ToUpper(randomHex(t, 8)),
		SecretKey: randomHex(t, 24),
	}
	passphrase := randomHex(t, 24)

	if c.edge, err = tcnetwork.New(ctx); err != nil {
		t.Fatalf("edge network: %v", err)
	}
	testcontainers.CleanupNetwork(t, c.edge)
	if c.cluster, err = tcnetwork.New(ctx); err != nil {
		t.Fatalf("cluster network: %v", err)
	}
	testcontainers.CleanupNetwork(t, c.cluster)
	t.Cleanup(c.removeVolumes)
	// What the nodes share, written once by the product's own command into
	// a volume, as the compose file does.
	initCtr, err := testcontainers.Run(ctx, img,
		testcontainers.WithCmd("dev-init", "--dir", configDir, "--nodes", strings.Join(Nodes, ",")),
		testcontainers.WithEnv(map[string]string{"CACTUSFS_KMS_PASSPHRASE": passphrase}),
		testcontainers.WithMounts(testcontainers.VolumeMount(c.volume("config"), configDir)),
		testcontainers.WithWaitStrategy(wait.ForExit().WithExitTimeout(time.Minute)),
	)
	testcontainers.CleanupContainer(t, initCtr)
	if err != nil {
		t.Fatalf("dev-init: %v", err)
	}
	if state, err := initCtr.State(ctx); err != nil || state.ExitCode != 0 {
		t.Fatalf("dev-init exited with %v, %v\n%s", state, err, logsOf(ctx, initCtr))
	}

	peers := make([]string, len(Nodes))
	for i, id := range Nodes {
		peers[i] = id + "=" + id + ":9002"
	}
	c.env = map[string]string{
		"CACTUSFS_PEERS":           strings.Join(peers, ","),
		"CACTUSFS_DATA":            dataRoot + "/data1," + dataRoot + "/data2",
		"CACTUSFS_META_DIR":        dataRoot + "/meta",
		"CACTUSFS_KMS":             "local",
		"CACTUSFS_KMS_KEY_FILE":    configDir + "/kms.json",
		"CACTUSFS_KMS_PASSPHRASE":  passphrase,
		"CACTUSFS_ROOT_ACCESS_KEY": c.AccessKey,
		"CACTUSFS_ROOT_SECRET_KEY": c.SecretKey,
		"CACTUSFS_NODE_TLS_CA":     configDir + "/node-ca.pem",
		"CACTUSFS_TLS_CERT":        configDir + "/s3.pem",
		"CACTUSFS_TLS_KEY":         configDir + "/s3.key",
	}
	for k, v := range opts.Env {
		c.env[k] = v
	}

	// A new cluster forms when all its nodes are up: start them together.
	errs := make(chan error, len(Nodes))
	for _, id := range Nodes {
		go func() { errs <- c.run(ctx, id) }()
	}
	for range Nodes {
		if err := <-errs; err != nil {
			t.Fatalf("start the cluster: %v", err)
		}
	}

	// The CA clients trust for the S3 listeners.
	reader, err := c.container("node1").CopyFileFromContainer(ctx, configDir+"/s3-ca.pem")
	if err != nil {
		t.Fatalf("read the S3 CA: %v", err)
	}
	defer func() { _ = reader.Close() }()
	pem, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read the S3 CA: %v", err)
	}
	c.roots = x509.NewCertPool()
	if !c.roots.AppendCertsFromPEM(pem) {
		t.Fatal("the S3 CA file holds no certificate")
	}
	// When a test fails, what the nodes logged is the evidence. Registered
	// last, so that it runs before the containers are removed.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, id := range Nodes {
			c.mu.Lock()
			_, started := c.containers[id]
			c.mu.Unlock()
			if started {
				t.Logf("---- last log lines of %s ----\n%s", id, tail(warnings(c.Logs(id)), 6000))
			}
		}
	})

	return c
}

func (c *Cluster) volume(name string) string {
	return c.prefix + "-" + name
}

// run creates and starts the container of a node.
func (c *Cluster) run(ctx context.Context, id string) error {
	env := map[string]string{
		"CACTUSFS_NODE_ID":       id,
		"CACTUSFS_NODE_TLS_CERT": configDir + "/nodes/" + id + ".pem",
		"CACTUSFS_NODE_TLS_KEY":  configDir + "/nodes/" + id + ".key",
		"CACTUSFS_TOPOLOGY_ZONE": "zone-" + id,
	}
	for k, v := range c.env {
		env[k] = v
	}
	if id == "node4" {
		env["CACTUSFS_ROLES"] = "gateway,storage,console"
	}
	ctr, err := testcontainers.Run(ctx, c.image,
		testcontainers.WithCmd("server"),
		testcontainers.WithEnv(env),
		testcontainers.WithExposedPorts(s3Port, adminPort),
		tcnetwork.WithNetwork(nil, c.edge),
		tcnetwork.WithNetwork([]string{id}, c.cluster),
		testcontainers.WithMounts(
			testcontainers.VolumeMount(c.volume("config"), configDir),
			testcontainers.VolumeMount(c.volume(id+"-meta"), dataRoot+"/meta"),
			testcontainers.VolumeMount(c.volume(id+"-data1"), dataRoot+"/data1"),
			testcontainers.VolumeMount(c.volume(id+"-data2"), dataRoot+"/data2"),
		),
		testcontainers.WithWaitStrategyAndDeadline(startTimeout, wait.ForLog(`"msg":"node started"`)),
	)
	if ctr != nil {
		c.mu.Lock()
		c.containers[id] = ctr
		c.mu.Unlock()
		testcontainers.CleanupContainer(c.t, ctr)
	}
	if err != nil {
		return fmt.Errorf("node %s: %w\n%s", id, err, logsOf(ctx, ctr))
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
	data, _ := io.ReadAll(io.LimitReader(reader, 64<<10))
	return string(data)
}

func (c *Cluster) removeVolumes() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	names := []string{c.volume("config")}
	for _, id := range Nodes {
		names = append(names, c.volume(id+"-meta"), c.volume(id+"-data1"), c.volume(id+"-data2"))
	}
	for _, name := range names {
		_, _ = c.docker.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true})
	}
}

func (c *Cluster) container(id string) *testcontainers.DockerContainer {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctr := c.containers[id]
	if ctr == nil {
		c.t.Fatalf("no container for node %s", id)
	}
	return ctr
}

// Logs returns the log of a node so far.
func (c *Cluster) Logs(id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reader, err := c.container(id).Logs(ctx)
	if err != nil {
		return ""
	}
	defer func() { _ = reader.Close() }()
	data, _ := io.ReadAll(reader)
	return string(data)
}

// address returns where a published port of a node is reached from the test.
func (c *Cluster) address(ctx context.Context, id, port string) (string, error) {
	ctr := c.container(id)
	host, err := ctr.Host(ctx)
	if err != nil {
		return "", err
	}
	mapped, err := ctr.MappedPort(ctx, port)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, mapped.Port()), nil
}

// transport returns an HTTP transport that reaches a node at its published
// port while speaking to it under its own name, which is the name in its
// certificate and in the signature of every request.
func (c *Cluster) transport(id, port string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// The published port changes when a container is restarted.
			address, err := c.address(ctx, id, port)
			if err != nil {
				return nil, err
			}
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
		},
		TLSClientConfig:       &tls.Config{RootCAs: c.roots, ServerName: id, MinVersion: tls.VersionTLS13},
		ResponseHeaderTimeout: 30 * time.Second,
		DisableKeepAlives:     true,
	}
}

// S3 returns an S3 client that talks to one node. It does not retry: a test
// sees exactly what the node answered.
func (c *Cluster) S3(id string) *s3.Client {
	cfg := aws.Config{
		Region:                     "us-east-1",
		Credentials:                credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, ""),
		HTTPClient:                 &http.Client{Transport: c.transport(id, s3Port), Timeout: 2 * time.Minute},
		Retryer:                    func() aws.Retryer { return retry.AddWithMaxAttempts(retry.NewStandard(), 1) },
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenSupported,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenSupported,
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("https://" + id + ":9000")
		o.UsePathStyle = true
	})
}

// Ready reports whether a node answers its readiness probe with 200.
func (c *Cluster) Ready(id string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+id+":9001/readyz", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Transport: c.transport(id, adminPort)}).Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// Put stores an object through a node.
func (c *Cluster) Put(ctx context.Context, id, bucket, key string, data []byte) error {
	_, err := c.S3(id).PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data)})
	return err
}

// Get reads an object through a node.
func (c *Cluster) Get(ctx context.Context, id, bucket, key string) ([]byte, error) {
	out, err := c.S3(id).GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()
	return io.ReadAll(out.Body)
}

// Kill stops a node the hard way: SIGKILL, no warning.
func (c *Cluster) Kill(id string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := c.docker.ContainerKill(ctx, c.container(id).GetContainerID(), client.ContainerKillOptions{Signal: "SIGKILL"}); err != nil {
		c.t.Fatalf("kill %s: %v", id, err)
	}
	c.waitStopped(ctx, id)
}

// Stop stops a node gracefully, as an orchestrator does.
func (c *Cluster) Stop(id string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	grace := time.Minute
	if err := c.container(id).Stop(ctx, &grace); err != nil {
		c.t.Fatalf("stop %s: %v", id, err)
	}
	c.waitStopped(ctx, id)
}

func (c *Cluster) waitStopped(ctx context.Context, id string) {
	c.t.Helper()
	for {
		state, err := c.container(id).State(ctx)
		if err != nil {
			c.t.Fatalf("state of %s: %v", id, err)
		}
		if !state.Running {
			return
		}
		select {
		case <-ctx.Done():
			c.t.Fatalf("node %s did not stop", id)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Restart starts a stopped node again, with its volumes as they are, and
// waits until it serves.
func (c *Cluster) Restart(id string) {
	c.t.Helper()
	c.StartNode(id)
	c.WaitReady(id)
}

// StartNode starts a stopped node and returns at once.
func (c *Cluster) StartNode(id string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := c.docker.ContainerStart(ctx, c.container(id).GetContainerID(), client.ContainerStartOptions{}); err != nil {
		c.t.Fatalf("start %s: %v", id, err)
	}
}

// WaitReady waits until a node answers its readiness probe.
func (c *Cluster) WaitReady(ids ...string) {
	c.t.Helper()
	deadline := time.Now().Add(startTimeout)
	for _, id := range ids {
		for !c.Ready(id) {
			if time.Now().After(deadline) {
				c.t.Fatalf("node %s did not become ready\n%s", id, tail(c.Logs(id), 4000))
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// Isolate cuts a node off from the other nodes. Its S3 port stays reachable.
func (c *Cluster) Isolate(id string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := c.docker.NetworkDisconnect(ctx, c.cluster.ID, client.NetworkDisconnectOptions{
		Container: c.container(id).GetContainerID(), Force: true,
	})
	if err != nil {
		c.t.Fatalf("isolate %s: %v", id, err)
	}
}

// Rejoin undoes Isolate.
func (c *Cluster) Rejoin(id string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := c.docker.NetworkConnect(ctx, c.cluster.ID, client.NetworkConnectOptions{
		Container: c.container(id).GetContainerID(), EndpointConfig: &network.EndpointSettings{Aliases: []string{id}},
	})
	if err != nil {
		c.t.Fatalf("rejoin %s: %v", id, err)
	}
}

// Shell runs a shell script in a helper container that has the two drives of
// a node mounted at /data1 and /data2, and returns its output. It is how a
// test looks at, damages or deletes shard files.
func (c *Cluster) Shell(id, script string) string {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	helper, err := testcontainers.Run(ctx, helperImage,
		testcontainers.WithCmd("sh", "-c", script),
		testcontainers.WithMounts(
			testcontainers.VolumeMount(c.volume(id+"-data1"), "/data1"),
			testcontainers.VolumeMount(c.volume(id+"-data2"), "/data2"),
		),
		testcontainers.WithWaitStrategy(wait.ForExit().WithExitTimeout(time.Minute)),
	)
	if helper != nil {
		defer func() { _ = helper.Terminate(context.WithoutCancel(ctx)) }()
	}
	if err != nil {
		c.t.Fatalf("helper on %s: %v", id, err)
	}
	out := logsOf(ctx, helper)
	if state, err := helper.State(ctx); err != nil || state.ExitCode != 0 {
		c.t.Fatalf("helper on %s exited with %v, %v\n%s", id, state, err, out)
	}
	return out
}

// Eventually waits until condition holds, and fails the test when it does not
// within limit.
func (c *Cluster) Eventually(what string, limit time.Duration, condition func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(limit)
	for !condition() {
		if time.Now().After(deadline) {
			c.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// warnings keeps the log lines that say something went wrong, and the few
// that mark the life of the node.
func warnings(log string) string {
	var kept []string
	for line := range strings.SplitSeq(log, "\n") {
		if strings.Contains(line, `"level":"WARN"`) || strings.Contains(line, `"level":"ERROR"`) ||
			strings.Contains(line, "node started") || strings.Contains(line, "joined the cluster") ||
			strings.Contains(line, "cactusfs server:") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}
