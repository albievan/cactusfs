//go:build integration || chaos

package clustertest

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// standaloneDrives is how many drives a standalone node is given: enough
// for it to erasure-code (two data and two parity shards), as a deployment
// of one with four drives does.
const standaloneDrives = 4

// Standalone is a deployment of one node - every role in one process, a
// metadata group of one - run from the product image under test, as
// `cactusfs server --data <dir>...` runs it. It is a site of its own: its
// own certificate authority for its listeners, its own sealed KMS key file
// and its own root credential, none of them shared with a Cluster.
type Standalone struct {
	t      *testing.T
	docker *client.Client
	ctr    *testcontainers.DockerContainer
	prefix string
	roots  *x509.CertPool

	// Name is the node's name on the network it was started on, and the
	// name its S3 and admin listeners' certificate is issued for: another
	// deployment reaches it at https://<Name>:9000.
	Name string
	// AccessKey and SecretKey are its root credential, and KMSPassphrase
	// opens its key file. All three are made up for the run.
	AccessKey     string
	SecretKey     string
	KMSPassphrase string
	// CAPEM is the certificate, in PEM, of the CA that issued the
	// certificate of its listeners: what a client - another deployment's
	// target among them - must trust to reach it.
	CAPEM []byte

	mu     sync.Mutex
	paused bool
}

// StandaloneDriveDirs returns the directories of the drives of a standalone
// node as it sees them, in the order it is given them.
func StandaloneDriveDirs() []string {
	dirs := make([]string, standaloneDrives)
	for i := range dirs {
		dirs[i] = fmt.Sprintf("%s/data%d", dataRoot, i+1)
	}
	return dirs
}

// StartStandalone starts a standalone node on the network named network -
// a Cluster's Network(), for a test of two sites - under the alias name,
// and waits until it says it has started. Its configuration (a CA and a
// certificate for name, and a KMS key file) is written by the product's own
// dev-init into a volume of its own. Only the S3 and admin ports are
// published to the test. The container and its volumes are removed when the
// test ends; a node that is paused then is resumed first.
func StartStandalone(t *testing.T, network, name string) *Standalone {
	t.Helper()
	img, err := image(t.Context())
	if err != nil {
		t.Fatalf("product image: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	provider, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })

	s := &Standalone{
		t: t, docker: provider.Client, prefix: "cactusfs-test-" + randomHex(t, 4), Name: name,
		AccessKey: "TEST" + strings.ToUpper(randomHex(t, 8)), SecretKey: randomHex(t, 24), KMSPassphrase: randomHex(t, 24),
	}
	t.Cleanup(s.removeVolumes)

	initCtr, err := testcontainers.Run(ctx, img,
		testcontainers.WithCmd("dev-init", "--dir", configDir, "--nodes", name),
		testcontainers.WithEnv(map[string]string{"CACTUSFS_KMS_PASSPHRASE": s.KMSPassphrase}),
		testcontainers.WithMounts(testcontainers.VolumeMount(s.volume("config"), configDir)),
		testcontainers.WithWaitStrategy(wait.ForExit().WithExitTimeout(time.Minute)),
	)
	testcontainers.CleanupContainer(t, initCtr)
	if err != nil {
		t.Fatalf("dev-init of %s: %v", name, err)
	}
	if state, err := initCtr.State(ctx); err != nil || state.ExitCode != 0 {
		t.Fatalf("dev-init of %s exited with %v, %v\n%s", name, state, err, logsOf(ctx, initCtr))
	}

	mounts := []testcontainers.ContainerMount{
		testcontainers.VolumeMount(s.volume("config"), configDir),
		testcontainers.VolumeMount(s.volume("meta"), testcontainers.ContainerMountTarget(MetaDir())),
	}
	for i, dir := range StandaloneDriveDirs() {
		mounts = append(mounts, testcontainers.VolumeMount(s.volume(fmt.Sprintf("data%d", i+1)), testcontainers.ContainerMountTarget(dir)))
	}
	ctr, err := testcontainers.Run(ctx, img,
		testcontainers.WithCmd("server"),
		testcontainers.WithEnv(map[string]string{
			"CACTUSFS_DATA":            strings.Join(StandaloneDriveDirs(), ","),
			"CACTUSFS_META_DIR":        MetaDir(),
			"CACTUSFS_KMS":             "local",
			"CACTUSFS_KMS_KEY_FILE":    configDir + "/kms.json",
			"CACTUSFS_KMS_PASSPHRASE":  s.KMSPassphrase,
			"CACTUSFS_ROOT_ACCESS_KEY": s.AccessKey,
			"CACTUSFS_ROOT_SECRET_KEY": s.SecretKey,
			"CACTUSFS_TLS_CERT":        configDir + "/s3.pem",
			"CACTUSFS_TLS_KEY":         configDir + "/s3.key",
		}),
		testcontainers.WithExposedPorts(s3Port, adminPort),
		tcnetwork.WithNetworkName([]string{name}, network),
		testcontainers.WithMounts(mounts...),
		testcontainers.WithWaitStrategyAndDeadline(startTimeout, wait.ForLog(`"msg":"node started"`)),
	)
	if ctr != nil {
		s.ctr = ctr
		testcontainers.CleanupContainer(t, ctr)
		// Registered after the container's removal, so it runs before it:
		// a paused container is resumed before it is stopped.
		t.Cleanup(func() {
			s.mu.Lock()
			paused := s.paused
			s.mu.Unlock()
			if paused {
				s.Unpause()
			}
		})
	}
	if err != nil {
		t.Fatalf("standalone node %s: %v\n%s", name, err, logsOf(ctx, ctr))
	}

	reader, err := ctr.CopyFileFromContainer(ctx, configDir+"/s3-ca.pem")
	if err != nil {
		t.Fatalf("read the S3 CA of %s: %v", name, err)
	}
	defer func() { _ = reader.Close() }()
	if s.CAPEM, err = io.ReadAll(reader); err != nil {
		t.Fatalf("read the S3 CA of %s: %v", name, err)
	}
	s.roots = x509.NewCertPool()
	if !s.roots.AppendCertsFromPEM(s.CAPEM) {
		t.Fatalf("the S3 CA file of %s holds no certificate", name)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("---- last log lines of %s ----\n%s", name, tail(warnings(s.Logs()), 6000))
		}
	})
	return s
}

func (s *Standalone) volume(name string) string {
	return s.prefix + "-" + name
}

func (s *Standalone) removeVolumes() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	names := []string{s.volume("config"), s.volume("meta")}
	for i := range standaloneDrives {
		names = append(names, s.volume(fmt.Sprintf("data%d", i+1)))
	}
	for _, name := range names {
		_, _ = s.docker.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true})
	}
}

func (s *Standalone) transport(port string) *http.Transport {
	return transportTo(s.roots, s.Name, func(ctx context.Context) (string, error) {
		return publishedAddress(ctx, s.docker, s.ctr, s.Name, port)
	})
}

// S3 returns an S3 client of the node with its root credential, and S3As
// one with another key. Neither retries.
func (s *Standalone) S3() *s3.Client {
	return s.S3As(s.AccessKey, s.SecretKey)
}

// S3As returns an S3 client of the node that signs with the key given.
func (s *Standalone) S3As(accessKey, secretKey string) *s3.Client {
	return newS3(s.transport(s3Port), s.Name, accessKey, secretKey)
}

// HTTP returns an HTTP client for the S3 listener of the node, reached as
// https://<Name>:9000, for a request that is no call of the SDK.
func (s *Standalone) HTTP() *http.Client {
	return &http.Client{Transport: s.transport(s3Port), Timeout: 2 * time.Minute}
}

// Admin returns an HTTP client for the admin listener of the node,
// reached as https://<Name>:9001.
func (s *Standalone) Admin() *http.Client {
	return &http.Client{Transport: s.transport(adminPort), Timeout: 2 * time.Minute}
}

// Pause freezes every process of the node, as `docker pause` does: its
// address stays, connections to it are taken by the kernel and then get no
// answer. Unpause resumes it.
func (s *Standalone) Pause() {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := s.docker.ContainerPause(ctx, s.ctr.GetContainerID(), client.ContainerPauseOptions{}); err != nil {
		s.t.Fatalf("pause %s: %v", s.Name, err)
	}
	s.mu.Lock()
	s.paused = true
	s.mu.Unlock()
}

// Unpause resumes a node Pause froze.
func (s *Standalone) Unpause() {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := s.docker.ContainerUnpause(ctx, s.ctr.GetContainerID(), client.ContainerUnpauseOptions{}); err != nil {
		s.t.Fatalf("unpause %s: %v", s.Name, err)
	}
	s.mu.Lock()
	s.paused = false
	s.mu.Unlock()
}

// Logs returns the log of the node so far.
func (s *Standalone) Logs() string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return logsOf(ctx, s.ctr)
}
