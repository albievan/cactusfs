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
	"encoding/base64"
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
	"github.com/moby/moby/api/types/container"
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
	rpcPort   = "9002/tcp"

	startTimeout = 3 * time.Minute
)

// Nodes are the nodes of the test cluster. The first three vote in the
// metadata group; all four store shards and serve S3.
var Nodes = []string{"node1", "node2", "node3", "node4"}

// Options adjust the cluster.
type Options struct {
	// Env is added to the environment of every node.
	Env map[string]string
	// NodeEnv is added, by node, to the environment of that node alone, over
	// what Env says: a setting one node has and the others have not, or have
	// otherwise.
	NodeEnv map[string]map[string]string
	// Images names, by node, the image of a node that is to run another
	// release than the one under test: a cluster in the middle of a rolling
	// upgrade. A node that is not named runs the image under test.
	Images map[string]string
	// Prepare, when not nil, runs when the configuration the nodes share
	// has been written and before any node starts. A test that brings its
	// own certificates puts them in place here, with WriteConfig.
	Prepare func(c *Cluster)
	// PublishNodeRPC publishes port 9002 of every node, node RPC, to where
	// the test runs, as the S3 and admin ports always are: on a port of the
	// engine's host that the engine picks. It is off unless a test dials
	// node RPC itself; the nodes reach each other without it. A caller there
	// is served only with a certificate of the cluster's node CA.
	PublishNodeRPC bool
}

// ConfigFile is a file in the configuration directory the nodes share. Its
// name is relative to that directory: "node-ca.pem", "nodes/node1.key".
type ConfigFile struct {
	Name string
	Data []byte
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
	// nodeEnv holds, by node, what that node alone has in its environment.
	nodeEnv map[string]map[string]string
	// images holds, by node, the image of a node that does not run the
	// image under test.
	images map[string]string
	// publishRPC says that node RPC is published to the test.
	publishRPC bool

	// AccessKey and SecretKey are the root credential of the cluster.
	AccessKey string
	SecretKey string
	// KMSPassphrase opens the key file the nodes share. Like the root
	// credential it is made up for the run; a test has it to say that no
	// answer and no log of a node holds it, and prints it nowhere.
	KMSPassphrase string
	roots         *x509.CertPool

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
		containers: map[string]*testcontainers.DockerContainer{}, images: map[string]string{},
		nodeEnv:    map[string]map[string]string{},
		publishRPC: opts.PublishNodeRPC,
		// Credentials and passphrase are made up for this run; none is
		// written anywhere.
		AccessKey: "TEST" + strings.ToUpper(randomHex(t, 8)),
		SecretKey: randomHex(t, 24),
	}
	passphrase := randomHex(t, 24)
	c.KMSPassphrase = passphrase
	for id, name := range opts.Images {
		if name != "" {
			c.images[id] = name
		}
	}
	// Copied: the node is started again from it when it is upgraded.
	for id, env := range opts.NodeEnv {
		c.nodeEnv[id] = map[string]string{}
		for k, v := range env {
			c.nodeEnv[id][k] = v
		}
	}

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

	if opts.Prepare != nil {
		opts.Prepare(c)
	}

	peers := make([]string, len(Nodes))
	for i, id := range Nodes {
		peers[i] = id + "=" + id + ":9002"
	}
	c.env = map[string]string{
		"CACTUSFS_PEERS":           strings.Join(peers, ","),
		"CACTUSFS_DATA":            strings.Join(DriveDirs(), ","),
		"CACTUSFS_META_DIR":        MetaDir(),
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

// DriveDirs returns the directories of the two drives of a node as the node
// sees them, in the order it is given them: inside its container, where a
// volume of its own is mounted at each.
func DriveDirs() []string {
	return []string{dataRoot + "/data1", dataRoot + "/data2"}
}

// MetaDir returns the metadata directory of a node as the node sees it.
func MetaDir() string {
	return dataRoot + "/meta"
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
	// Written before any node started and only read since.
	for k, v := range c.nodeEnv[id] {
		env[k] = v
	}
	// Published on the engine's host, which is not always this machine: a
	// port bound to its loopback address could not be reached from here.
	published := []string{s3Port, adminPort}
	if c.publishRPC {
		published = append(published, rpcPort)
	}
	ctr, err := testcontainers.Run(ctx, c.ImageOf(id),
		testcontainers.WithCmd("server"),
		testcontainers.WithEnv(env),
		testcontainers.WithExposedPorts(published...),
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

// ImageOf returns the image a node runs.
func (c *Cluster) ImageOf(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if name := c.images[id]; name != "" {
		return name
	}
	return c.image
}

// Upgrade replaces the binary of a node, as a rolling upgrade does it for
// one node: the node is stopped gracefully, its container is removed, and a
// new one is started from the image - from the image under test when none is
// named - with the volumes, the name and the environment the node had.
// Upgrade returns when the node says it has started; whether it serves is
// for WaitReady to say.
func (c *Cluster) Upgrade(id, name string) {
	c.t.Helper()
	c.Stop(id)
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	// Without its volumes: they are named, and are the node.
	if _, err := c.docker.ContainerRemove(ctx, c.container(id).GetContainerID(), client.ContainerRemoveOptions{Force: true}); err != nil {
		c.t.Fatalf("remove the container of %s: %v", id, err)
	}
	c.mu.Lock()
	delete(c.containers, id)
	delete(c.images, id)
	if name != "" {
		c.images[id] = name
	}
	c.mu.Unlock()
	if err := c.run(ctx, id); err != nil {
		c.t.Fatalf("start %s from %s: %v", id, c.ImageOf(id), err)
	}
}

// MetaFile returns a file of the metadata directory of a node, which must be
// stopped: the file is read as it lies in the node's volume, by a helper
// container, so that a test can look at the state a member holds and not at
// what some read path makes of it.
func (c *Cluster) MetaFile(id, name string) []byte {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if state, err := c.container(id).State(ctx); err != nil || state.Running {
		c.t.Fatalf("the metadata of %s is read while it is stopped: state %v, %v", id, state, err)
	}
	// The helper only has to be there while the file is copied out of it.
	helper, err := testcontainers.Run(ctx, helperImage,
		testcontainers.WithCmd("sleep", "120"),
		testcontainers.WithMounts(testcontainers.VolumeMount(c.volume(id+"-meta"), "/meta")),
	)
	if helper != nil {
		defer func() { _ = helper.Terminate(context.WithoutCancel(ctx)) }()
	}
	if err != nil {
		c.t.Fatalf("helper on the metadata of %s: %v", id, err)
	}
	reader, err := helper.CopyFileFromContainer(ctx, "/meta/"+name)
	if err != nil {
		c.t.Fatalf("read %s of the metadata of %s: %v", name, id, err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	if err != nil {
		c.t.Fatalf("read %s of the metadata of %s: %v", name, id, err)
	}
	return data
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
	// Asked of the engine every time, not of a cached inspection: the
	// engine publishes a port anew, at another number, when a container is
	// restarted and also when its network is taken away and given back.
	inspected, err := c.docker.ContainerInspect(ctx, ctr.GetContainerID(), client.ContainerInspectOptions{})
	if err != nil {
		return "", err
	}
	if settings := inspected.Container.NetworkSettings; settings != nil {
		for published, bindings := range settings.Ports {
			if published.String() != port {
				continue
			}
			for _, binding := range bindings {
				if binding.HostPort != "" {
					return net.JoinHostPort(host, binding.HostPort), nil
				}
			}
		}
	}
	return "", fmt.Errorf("port %s of node %s is not published", port, id)
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

// HTTP returns an HTTP client for the S3 listener of a node - for a request
// that is no call of the SDK, such as a presigned URL - and Admin one for
// its admin listener, where the management API is. Requests are addressed to
// the node by its name: https://<id>:9000 and https://<id>:9001.
func (c *Cluster) HTTP(id string) *http.Client {
	return &http.Client{Transport: c.transport(id, s3Port), Timeout: 2 * time.Minute}
}

// Admin returns an HTTP client for the admin listener of a node.
func (c *Cluster) Admin(id string) *http.Client {
	return &http.Client{Transport: c.transport(id, adminPort), Timeout: 2 * time.Minute}
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

// Isolate cuts a node off from the other nodes while clients can still
// reach it: every packet the node would send to another node is dropped.
// Connections that are open stall; new ones, in either direction, are never
// completed.
//
// The partition is made inside the node's own network namespace, with routes
// that lead nowhere, by a helper container that shares the namespace. Taking
// the cluster network away from the container would do it too, but makes the
// engine publish the node's ports anew when the network is given back - and
// then, now and then, nothing listens at the published port any more, which
// fails a test for something the node did not do.
func (c *Cluster) Isolate(id string) {
	c.t.Helper()
	c.routes(id, "add")
}

// Rejoin undoes Isolate.
func (c *Cluster) Rejoin(id string) {
	c.t.Helper()
	c.routes(id, "del")
}

// peerAddresses returns the addresses of the other nodes on the cluster
// network.
func (c *Cluster) peerAddresses(ctx context.Context, id string) []string {
	c.t.Helper()
	var addresses []string
	for _, other := range Nodes {
		if other == id {
			continue
		}
		c.mu.Lock()
		ctr := c.containers[other]
		c.mu.Unlock()
		if ctr == nil {
			continue
		}
		inspected, err := c.docker.ContainerInspect(ctx, ctr.GetContainerID(), client.ContainerInspectOptions{})
		if err != nil || inspected.Container.NetworkSettings == nil {
			c.t.Fatalf("inspect %s: %v", other, err)
		}
		for _, endpoint := range inspected.Container.NetworkSettings.Networks {
			if endpoint != nil && endpoint.NetworkID == c.cluster.ID && endpoint.IPAddress.IsValid() {
				addresses = append(addresses, endpoint.IPAddress.String())
			}
		}
	}
	if len(addresses) == 0 {
		c.t.Fatalf("no other node of %s has an address on the cluster network", id)
	}
	return addresses
}

// routes adds or removes, in the network namespace of a node, a route to
// nowhere for every other node.
func (c *Cluster) routes(id, verb string) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var script strings.Builder
	script.WriteString("set -e\n")
	for _, address := range c.peerAddresses(ctx, id) {
		fmt.Fprintf(&script, "ip route %s blackhole %s/32\n", verb, address)
	}
	script.WriteString("ip route\n")
	target := c.container(id).GetContainerID()
	helper, err := testcontainers.Run(ctx, helperImage,
		testcontainers.WithCmd("sh", "-c", script.String()),
		testcontainers.WithHostConfigModifier(func(host *container.HostConfig) {
			host.NetworkMode = container.NetworkMode("container:" + target)
			host.CapAdd = []string{"NET_ADMIN"}
		}),
		testcontainers.WithWaitStrategy(wait.ForExit().WithExitTimeout(time.Minute)),
	)
	if helper != nil {
		defer func() { _ = helper.Terminate(context.WithoutCancel(ctx)) }()
	}
	if err != nil {
		c.t.Fatalf("routes of %s: %v", id, err)
	}
	if state, err := helper.State(ctx); err != nil || state.ExitCode != 0 {
		c.t.Fatalf("routes of %s: %v, %v\n%s", id, state, err, logsOf(ctx, helper))
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

// WriteConfig replaces files in the configuration directory the nodes share,
// in the order given, while the nodes run or before they start. Each file is
// written beside its place and renamed into it, with the owner and mode of
// the file it replaces: a node reads the old content or the new, never half.
// It is how a test replaces a certificate under a running node.
func (c *Cluster) WriteConfig(files ...ConfigFile) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var script strings.Builder
	script.WriteString("set -e\n")
	for _, file := range files {
		if file.Name == "" || strings.ContainsAny(file.Name, "'\\\n") || strings.Contains(file.Name, "..") || strings.HasPrefix(file.Name, "/") {
			c.t.Fatalf("configuration file name %q", file.Name)
		}
		// Base64 has no character a shell reads anything into.
		fmt.Fprintf(&script, "f='/config/%s'; t=\"$f.new\"\n", file.Name)
		script.WriteString("if [ -e \"$f\" ]; then cp -p \"$f\" \"$t\"; fi\n")
		fmt.Fprintf(&script, "echo '%s' | base64 -d > \"$t\"\n", base64.StdEncoding.EncodeToString(file.Data))
		script.WriteString("mv \"$t\" \"$f\"\n")
	}
	helper, err := testcontainers.Run(ctx, helperImage,
		testcontainers.WithCmd("sh", "-c", script.String()),
		testcontainers.WithMounts(testcontainers.VolumeMount(c.volume("config"), "/config")),
		testcontainers.WithWaitStrategy(wait.ForExit().WithExitTimeout(time.Minute)),
	)
	if helper != nil {
		defer func() { _ = helper.Terminate(context.WithoutCancel(ctx)) }()
	}
	if err != nil {
		c.t.Fatalf("write configuration files: %v", err)
	}
	if state, err := helper.State(ctx); err != nil || state.ExitCode != 0 {
		c.t.Fatalf("write configuration files: %v, %v\n%s", state, err, logsOf(ctx, helper))
	}
}

// Address returns where a port of a node - 9000 for S3, 9001 for the admin
// listener, 9002 for node RPC when Options.PublishNodeRPC is set - is reached
// from the test, for a test that
// does the TLS handshake itself to see which certificate it is shown.
func (c *Cluster) Address(ctx context.Context, id string, port int) (string, error) {
	return c.address(ctx, id, fmt.Sprintf("%d/tcp", port))
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
