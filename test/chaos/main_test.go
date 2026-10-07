//go:build chaos && !windows

package chaos_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	modulePath = "github.com/albievan/cactusfs"
	bucket     = "chaos"
	// Six drives: three data and three parity shards per object.
	drives = 6
	parity = 3
	// The shard file layout of ADR-0001: a 64-byte header, then blocks of a
	// 32-byte checksum and up to 128 KiB of data.
	shardHeader   = 64
	blockChecksum = 32
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cactusfs-chaos-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "chaos:", err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "cactusfs")
	build := exec.CommandContext(context.Background(), "go", "build", "-o", binary, modulePath+"/cmd/cactusfs")
	build.Env = append(build.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "chaos: go build: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// node is a running cactusfs process and the directories it stores in.
type node struct {
	t      *testing.T
	dir    string
	access string
	secret string
	// driveCount is the number of drives; extra is appended to the server's
	// arguments on the next start.
	driveCount int
	extra      []string

	cmd    *exec.Cmd
	s3     *s3.Client
	mu     sync.Mutex
	logs   []string
	stderr bytes.Buffer
}

// newNode prepares a deployment directory with six drives. Start runs it.
func newNode(t *testing.T) *node {
	t.Helper()
	raw := make([]byte, 30)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return &node{
		t: t, dir: t.TempDir(), driveCount: drives,
		access: "CHAOS" + strings.ToUpper(hex.EncodeToString(raw[:6])), secret: hex.EncodeToString(raw[6:]),
	}
}

func (n *node) drive(i int) string {
	return filepath.Join(n.dir, fmt.Sprintf("drive%d", i))
}

func (n *node) args() []string {
	args := []string{
		"server", "--dev-plaintext", "--kms", "local",
		// The key file and the metadata live outside the drives, so that a
		// drive can be wiped without taking them along.
		"--meta-dir", filepath.Join(n.dir, "meta"),
		"--admin-addr", "127.0.0.1:0", "--s3-addr", "127.0.0.1:0",
		// Scrub continuously and collect garbage every second, so that the
		// tests see repairs within seconds.
		"--scrub-interval", "1s", "--scrub-rate-mib", "0", "--gc-interval", "1s", "--gc-grace", "1h",
	}
	for i := range n.driveCount {
		args = append(args, "--data", n.drive(i))
	}
	return append(args, n.extra...)
}

// start runs the node and waits until it serves S3. It returns an error
// instead of failing the test when the node refuses to start, which some
// tests expect.
func (n *node) start() error {
	n.t.Helper()
	n.cmd = exec.CommandContext(context.Background(), binary, n.args()...)
	n.cmd.Env = append(n.cmd.Environ(),
		"CACTUSFS_KMS_PASSPHRASE=chaos suite passphrase",
		"CACTUSFS_ROOT_ACCESS_KEY="+n.access, "CACTUSFS_ROOT_SECRET_KEY="+n.secret,
	)
	n.stderr.Reset()
	n.cmd.Stderr = &n.stderr
	stdout, err := n.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := n.cmd.Start(); err != nil {
		return err
	}
	n.t.Cleanup(func() { _ = n.cmd.Process.Kill() })

	listening := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1<<20), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			n.mu.Lock()
			n.logs = append(n.logs, line)
			n.mu.Unlock()
			var entry struct {
				Msg      string `json:"msg"`
				Listener string `json:"listener"`
				Addr     string `json:"addr"`
			}
			if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "listening" && entry.Listener == "s3" {
				listening <- entry.Addr
			}
		}
		close(listening)
	}()

	select {
	case addr, ok := <-listening:
		if !ok {
			_ = n.cmd.Wait()
			return fmt.Errorf("node did not start: %s", strings.TrimSpace(n.stderr.String()))
		}
		n.s3 = s3.NewFromConfig(aws.Config{
			Region:      "us-east-1",
			Credentials: credentials.NewStaticCredentialsProvider(n.access, n.secret, ""),
			HTTPClient:  &http.Client{Timeout: 2 * time.Minute},
		}, func(o *s3.Options) {
			o.BaseEndpoint = aws.String("http://" + addr)
			o.UsePathStyle = true
		})
		return nil
	case <-time.After(60 * time.Second):
		return errors.New("node did not start listening within a minute")
	}
}

func (n *node) mustStart() {
	n.t.Helper()
	if err := n.start(); err != nil {
		n.t.Fatalf("start: %v", err)
	}
}

// stop shuts the node down gracefully.
func (n *node) stop() {
	n.t.Helper()
	if err := n.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		n.t.Fatalf("SIGTERM: %v", err)
	}
	if err := n.cmd.Wait(); err != nil {
		n.t.Fatalf("node did not stop cleanly: %v\n%s", err, n.stderr.String())
	}
}

// kill ends the node without warning, as a power cut or an OOM kill would.
func (n *node) kill() {
	n.t.Helper()
	_ = n.cmd.Process.Kill()
	_ = n.cmd.Wait()
}

// logged reports whether a log line containing text has been written.
func (n *node) logged(text string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, line := range n.logs {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

func (n *node) createBucket() {
	n.t.Helper()
	if _, err := n.s3.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		n.t.Fatalf("CreateBucket: %v", err)
	}
}

func (n *node) put(key string, data []byte) error {
	_, err := n.s3.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data),
	})
	return err
}

func (n *node) get(key string) ([]byte, error) {
	out, err := n.s3.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Body.Close() }()
	return io.ReadAll(out.Body)
}

// populate stores objects of assorted sizes, from empty to several stripes,
// and returns their contents by key.
func (n *node) populate(count int) map[string][]byte {
	n.t.Helper()
	sizes := []int{0, 1, 100, 4096, 65_536, 200_000, 1 << 20, 1<<20 + 1, 3<<20 + 4567}
	objects := map[string][]byte{}
	for i := range count {
		data := make([]byte, sizes[i%len(sizes)])
		if _, err := rand.Read(data); err != nil {
			n.t.Fatal(err)
		}
		key := fmt.Sprintf("dir%d/object-%03d", i%3, i)
		if err := n.put(key, data); err != nil {
			n.t.Fatalf("PutObject(%s): %v", key, err)
		}
		objects[key] = data
	}
	return objects
}

// verify reads every object back and compares it with what was stored.
func (n *node) verify(objects map[string][]byte, when string) {
	n.t.Helper()
	for key, want := range objects {
		got, err := n.get(key)
		if err != nil {
			n.t.Fatalf("%s: GetObject(%s): %v", when, key, err)
		}
		if !bytes.Equal(got, want) {
			n.t.Fatalf("%s: object %s came back different: %d bytes, want %d", when, key, len(got), len(want))
		}
	}
}

// shardFiles returns the shard files on a drive.
func (n *node) shardFiles(driveIndex int) []string {
	n.t.Helper()
	var files []string
	root := filepath.Join(n.drive(driveIndex), "shards")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		n.t.Fatalf("walk %s: %v", root, err)
	}
	return files
}

// snapshot returns the content of every shard file on a drive, by path.
func (n *node) snapshot(driveIndex int) map[string][]byte {
	n.t.Helper()
	out := map[string][]byte{}
	for _, path := range n.shardFiles(driveIndex) {
		out[path] = readFile(n.t, path)
	}
	return out
}

// corrupt flips a byte in the data of every shard file on a drive that has
// any data, and returns how many files it changed.
func (n *node) corrupt(driveIndex int) int {
	n.t.Helper()
	changed := 0
	for _, path := range n.shardFiles(driveIndex) {
		data := readFile(n.t, path)
		if len(data) <= shardHeader+blockChecksum {
			continue // an empty shard has nothing to corrupt but its header
		}
		position := shardHeader + blockChecksum + (len(data)-shardHeader-blockChecksum)/2
		data[position] ^= 0x40
		writeFile(n.t, path, data)
		changed++
	}
	return changed
}

// deleteShards removes every shard file on a drive.
func (n *node) deleteShards(driveIndex int) {
	n.t.Helper()
	for _, path := range n.shardFiles(driveIndex) {
		if err := os.Remove(path); err != nil {
			n.t.Fatalf("remove %s: %v", path, err)
		}
	}
}

// wipe empties a drive completely, as a replaced disk arrives.
func (n *node) wipe(driveIndex int) {
	n.t.Helper()
	if err := os.RemoveAll(n.drive(driveIndex)); err != nil {
		n.t.Fatalf("wipe drive %d: %v", driveIndex, err)
	}
}

// eventually waits until condition holds.
func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// restored reports whether the shard files on a drive are, byte for byte,
// what they were in the snapshot.
func (n *node) restored(want map[string][]byte) bool {
	for path, original := range want {
		data, err := tryReadFile(path)
		if err != nil || !bytes.Equal(data, original) {
			return false
		}
	}
	return true
}

// The helpers below reach files through an os.Root, as the code under test
// does.

func tryReadFile(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return root.ReadFile(filepath.Base(path))
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := tryReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatalf("open %s: %v", filepath.Dir(path), err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(filepath.Base(path), data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
