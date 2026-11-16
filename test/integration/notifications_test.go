//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/moby/moby/client"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/albievan/cactusfs/internal/adapters/events/webhook"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/test/clustertest"
)

const (
	// natsImage is the NATS server, pinned by digest (the index of
	// nats:2.15.3, every platform). An external service the nodes publish
	// to; nothing of it is linked or shipped.
	natsImage = "nats:2.15.3@sha256:9d85d87039bfba33baf48de4da32a2ee572bcd6c94f6365d05733a672de49253"

	// notifyBucket is the bucket whose changes are told. It keeps
	// versions: a put is a version, ObjectCreated:Put, and a delete a
	// delete marker, ObjectRemoved:DeleteMarkerCreated - each one the
	// bucket lists, so that what must be told is known exactly.
	notifyBucket = "notify"
	// The two targets: a webhook to the receiver, and NATS.
	notifyHook   = "receiver"
	notifyStream = "stream"
	// notifyTrail is the target the audit trail is sent to (PLAN 8.2.8): a
	// webhook to the same receiver, at the same endpoint, with a signing
	// key of its own; it admits no bucket.
	notifyTrail = "trail"
	// notifySubject is the subject the NATS target publishes to, and
	// notifyStreamName the stream the test makes for it (the operator's,
	// ADR-0025: CactusFS makes none).
	notifySubject    = "cactusfs.notify"
	notifyStreamName = "NOTIFICATIONS"
	// The names the two containers have on the cluster's network: the
	// hosts of the endpoints, and the names in their certificates.
	natsHost     = "nats"
	receiverHost = "receiver"
	receiverPort = "8443"
	// receiverRecords is the file in the receiver's container that holds
	// every notification it took, and receiverAuditRecords every record of
	// the audit trail.
	receiverRecords      = "/tmp/records.jsonl"
	receiverAuditRecords = "/tmp/audit.jsonl"

	// notifyPuts objects are put, and every fourth deleted after it:
	// notifyDeletes deletes.
	notifyPuts    = 2000
	notifyDeletes = notifyPuts / 4
	// notifyWorkers send the changes, two through each node at a time.
	notifyWorkers = 8
	// notifyHoldAt is the request the receiver holds unanswered (its line
	// in the receiver's file), about a third of the way through: the
	// leader is killed while it waits for the answer.
	notifyHoldAt = 800
	// notifyWriteLimit is how long a change may take to be acknowledged
	// through some node while the leader is killed and another elected.
	notifyWriteLimit = 2 * time.Minute
	// notifyResentAtMost bounds the IDs a receiver is sent more than once
	// (ADR-0025, "At least once"): one batch of 100 after the change of
	// leader, and once more as much for a request answered while the
	// receiver stopped.
	notifyResentAtMost = 200
	// notifyReceiverDown is how long the receiver is away while writes go
	// on.
	notifyReceiverDown = time.Minute
	// notifyCatchUp is how long what waited may take to arrive once the
	// receiver is back. After a minute of failures the pause before the
	// next attempt is at most 64 s (delivery.Backoff doubles from 1 s);
	// twice that, and the time to send what waited.
	notifyCatchUp = 3 * time.Minute
	// notifySignatureAge is how far a timestamp may be from the receiver's
	// clock (ADR-0025, "How a receiver verifies").
	notifySignatureAge = 5 * time.Minute
	// trailChanges administrative changes are made - a group created each
	// - half of them begun while the receiver holds its request, across the
	// kill of the leader, and half after them.
	trailChanges = 50
	// trailKillAfter of the changes before the kill are answered when the
	// leader is killed; the rest are on their way, and the leader is
	// sending the records of those answered, so the new leader may have to
	// send some again. trailPace is the pause between two changes.
	trailKillAfter = 3
	trailPace      = 200 * time.Millisecond
)

// notifyCA is the certificate authority of the test: it issues the
// certificates of NATS and of the receiver, and the targets trust it alone.
type notifyCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	pool *x509.CertPool
}

func newNotifyCA(t *testing.T) *notifyCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          notifySerial(t),
		Subject:               pkix.Name{CommonName: "cactusfs notification test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	ca := &notifyCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pool: x509.NewCertPool()}
	ca.pool.AddCert(cert)
	return ca
}

func notifySerial(t *testing.T) *big.Int {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		t.Fatal(err)
	}
	return serial
}

// issue returns a server certificate for host, and its key, in PEM.
func (ca *notifyCA) issue(t *testing.T, host string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: notifySerial(t),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

// runtimeImage returns the base image of the product image, as the
// Dockerfile pins it (ARG RUNTIME_IMAGE): the receiver runs in it.
func runtimeImage(t *testing.T) string {
	t.Helper()
	root, err := os.OpenRoot(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	dockerfile, err := root.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(dockerfile)) {
		if image, ok := strings.CutPrefix(strings.TrimSpace(line), "ARG RUNTIME_IMAGE="); ok {
			if !strings.Contains(image, "@sha256:") {
				t.Fatalf("the Dockerfile's RUNTIME_IMAGE %q is not pinned by digest", image)
			}
			return image
		}
	}
	t.Fatal("the Dockerfile names no RUNTIME_IMAGE")
	return ""
}

// buildReceiver compiles test/integration/receiver, static, for the
// engine's Linux, and returns the path of the executable.
func buildReceiver(ctx context.Context, t *testing.T) string {
	t.Helper()
	provider, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer func() { _ = provider.Close() }()
	info, err := provider.Info(ctx, client.InfoOptions{})
	if err != nil {
		t.Fatalf("docker info: %v", err)
	}
	arch := map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[info.Info.Architecture]
	if arch == "" {
		t.Fatalf("the engine runs on %q, for which the receiver is not built", info.Info.Architecture)
	}
	bin := filepath.Join(t.TempDir(), "receiver")
	cmd := exec.CommandContext(ctx, "go", "build", "-tags", "integration", "-trimpath", "-o", bin, modulePath+"/test/integration/receiver")
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build the receiver: %v\n%s", err, out)
	}
	return bin
}

// startNATS runs a NATS server with JetStream on the cluster's network, as
// natsHost, serving TLS with a certificate of ca and taking token alone.
// It is published to the test too, which makes the stream and reads it.
func startNATS(ctx context.Context, t *testing.T, network string, ca *notifyCA, token string) *testcontainers.DockerContainer {
	t.Helper()
	cert, key := ca.issue(t, natsHost)
	// The token is hexadecimal: nothing in it is read by the
	// configuration's syntax.
	config := `listen: "0.0.0.0:4222"
tls {
  cert_file: "/etc/nats/tls.pem"
  key_file: "/etc/nats/tls.key"
  timeout: 5
}
authorization {
  token: "` + token + `"
}
jetstream {
  store_dir: "/tmp/nats"
}
`
	ctr, err := testcontainers.Run(ctx, natsImage,
		testcontainers.WithCmd("--config", "/etc/nats/nats.conf"),
		testcontainers.WithFiles(
			testcontainers.ContainerFile{Reader: strings.NewReader(config), ContainerFilePath: "/etc/nats/nats.conf", FileMode: 0o600},
			testcontainers.ContainerFile{Reader: bytes.NewReader(cert), ContainerFilePath: "/etc/nats/tls.pem", FileMode: 0o644},
			testcontainers.ContainerFile{Reader: bytes.NewReader(key), ContainerFilePath: "/etc/nats/tls.key", FileMode: 0o600},
		),
		testcontainers.WithExposedPorts("4222/tcp"),
		tcnetwork.WithNetworkName([]string{natsHost}, network),
		testcontainers.WithWaitStrategy(wait.ForLog("Server is ready").WithStartupTimeout(2*time.Minute)),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}
	return ctr
}

// startReceiver runs the receiver in the product's base image on the
// cluster's network, as receiverHost, serving TLS with a certificate of ca.
func startReceiver(ctx context.Context, t *testing.T, network, bin string, ca *notifyCA, path, bearer string) *testcontainers.DockerContainer {
	t.Helper()
	cert, key := ca.issue(t, receiverHost)
	ctr, err := testcontainers.Run(ctx, runtimeImage(t),
		testcontainers.WithEntrypoint("/receiver"),
		testcontainers.WithCmd("-addr", ":"+receiverPort, "-cert", "/etc/receiver/tls.pem", "-key", "/etc/receiver/tls.key",
			"-records", receiverRecords, "-audit-records", receiverAuditRecords, "-hold-at", strconv.Itoa(notifyHoldAt)),
		testcontainers.WithEnv(map[string]string{"RECEIVER_PATH": path, "RECEIVER_BEARER": bearer}),
		// The image's user is not root: what it reads is readable by all.
		// The key is a test's, made for this run.
		testcontainers.WithFiles(
			testcontainers.ContainerFile{HostFilePath: bin, ContainerFilePath: "/receiver", FileMode: 0o755},
			testcontainers.ContainerFile{Reader: bytes.NewReader(cert), ContainerFilePath: "/etc/receiver/tls.pem", FileMode: 0o644},
			testcontainers.ContainerFile{Reader: bytes.NewReader(key), ContainerFilePath: "/etc/receiver/tls.key", FileMode: 0o644},
		),
		tcnetwork.WithNetworkName([]string{receiverHost}, network),
		testcontainers.WithWaitStrategy(wait.ForLog("receiver listening").WithStartupTimeout(time.Minute)),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start the receiver: %v", err)
	}
	return ctr
}

// notifyArrival is one message as a receiver or the stream holds it.
type notifyArrival struct {
	// ID and Type are of the headers; Body is the message as it arrived.
	ID, Type string
	Body     []byte
	// Key, Event, VersionID, Sequencer and Size are of the record in Body.
	Key, Event, VersionID, Sequencer string
	Size                             *int64
}

// notifyRecord is what the test reads of the S3 record of a notification.
type notifyRecord struct {
	Records []struct {
		EventVersion string `json:"eventVersion"`
		EventSource  string `json:"eventSource"`
		EventName    string `json:"eventName"`
		S3           struct {
			Bucket struct {
				Name string `json:"name"`
			} `json:"bucket"`
			Object struct {
				Key       string `json:"key"`
				Size      *int64 `json:"size"`
				VersionID string `json:"versionId"`
				Sequencer string `json:"sequencer"`
			} `json:"object"`
		} `json:"s3"`
	} `json:"Records"`
}

// The two kinds of change the test makes in its versioned bucket.
const (
	eventPut    = "ObjectCreated:Put"
	eventMarker = "ObjectRemoved:DeleteMarkerCreated"
)

// Judgements of messages that may yet change; any other fails at once.
var (
	// errNotYet: what has arrived is right, and some change is not told
	// yet.
	errNotYet = errors.New("not every change is told")
	// errUnreachable: what has arrived could not be read this time.
	errUnreachable = errors.New("could not be read")
)

// parseArrival reads the record of a message: one record, of the bucket,
// its key URL-decoded.
func parseArrival(id, kind string, body []byte) (notifyArrival, error) {
	var record notifyRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return notifyArrival{}, fmt.Errorf("message %s: %w: %s", id, err, body)
	}
	if len(record.Records) != 1 {
		return notifyArrival{}, fmt.Errorf("message %s holds %d records: %s", id, len(record.Records), body)
	}
	r := record.Records[0]
	if r.EventVersion != "2.1" || r.EventSource != "cactusfs:s3" || r.S3.Bucket.Name != notifyBucket {
		return notifyArrival{}, fmt.Errorf("message %s: %s", id, body)
	}
	key, err := url.QueryUnescape(r.S3.Object.Key)
	if err != nil {
		return notifyArrival{}, fmt.Errorf("message %s: the key %q: %w", id, r.S3.Object.Key, err)
	}
	return notifyArrival{
		ID: id, Type: kind, Body: body, Key: key, Event: r.EventName, VersionID: r.S3.Object.VersionID,
		Sequencer: r.S3.Object.Sequencer, Size: r.S3.Object.Size,
	}, nil
}

// receiverRecord is a line of the receiver's file (receiver.Record).
type receiverRecord struct {
	At          time.Time `json:"at"`
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Timestamp   string    `json:"timestamp"`
	Signature   string    `json:"signature"`
	ContentType string    `json:"content_type"`
	UserAgent   string    `json:"user_agent"`
	Body        []byte    `json:"body"`
}

// receiverLines returns the lines of the receiver's file of notifications
// written whole, the receiver running or stopped; errUnreachable when it
// cannot be copied out.
func receiverLines(ctx context.Context, receiver *testcontainers.DockerContainer) ([][]byte, error) {
	return receiverFile(ctx, receiver, receiverRecords)
}

// receiverFile returns the lines of a file of the receiver written whole;
// errUnreachable when it cannot be copied out.
func receiverFile(ctx context.Context, receiver *testcontainers.DockerContainer, file string) ([][]byte, error) {
	reader, err := receiver.CopyFileFromContainer(ctx, file)
	if err != nil {
		return nil, fmt.Errorf("the receiver's records %w: %w", errUnreachable, err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("the receiver's records %w: %w", errUnreachable, err)
	}
	// A line not yet written whole is left for the next read.
	lines := bytes.SplitAfter(data, []byte{'\n'})
	if n := len(lines); n > 0 && !bytes.HasSuffix(lines[n-1], []byte{'\n'}) {
		lines = lines[:n-1]
	}
	return lines, nil
}

// receiverArrivals reads what the receiver took, in the order it took it.
// Every request must be signed under signingKey and carry the headers of
// ADR-0025.
func receiverArrivals(ctx context.Context, receiver *testcontainers.DockerContainer, signingKey string) ([]notifyArrival, error) {
	lines, err := receiverLines(ctx, receiver)
	if err != nil {
		return nil, err
	}
	arrivals := make([]notifyArrival, 0, len(lines))
	for _, line := range lines {
		var got receiverRecord
		if err := json.Unmarshal(line, &got); err != nil {
			return nil, fmt.Errorf("a line of the receiver's records: %w", err)
		}
		if !webhook.Verify([]byte(signingKey), got.Timestamp, got.Body, got.Signature) {
			return nil, fmt.Errorf("message %s: the signature %q does not verify", got.ID, got.Signature)
		}
		seconds, err := strconv.ParseInt(got.Timestamp, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("message %s: the timestamp %q: %w", got.ID, got.Timestamp, err)
		}
		if age := got.At.Sub(time.Unix(seconds, 0)).Abs(); age > notifySignatureAge {
			return nil, fmt.Errorf("message %s: signed %s away from when it arrived", got.ID, age)
		}
		if got.ContentType != "application/json" || !strings.HasPrefix(got.UserAgent, "cactusfs/") || got.Type != "cactusfs.notification.object.v1" {
			return nil, fmt.Errorf("message %s: Content-Type %q, User-Agent %q, type %q", got.ID, got.ContentType, got.UserAgent, got.Type)
		}
		arrival, err := parseArrival(got.ID, got.Type, got.Body)
		if err != nil {
			return nil, err
		}
		arrivals = append(arrivals, arrival)
	}
	return arrivals, nil
}

// streamArrivals reads every message of the stream, in its order;
// errUnreachable when the stream cannot be read.
func streamArrivals(ctx context.Context, stream jetstream.Stream) ([]notifyArrival, error) {
	info, err := stream.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("the stream %w: %w", errUnreachable, err)
	}
	var arrivals []notifyArrival
	if info.State.Msgs == 0 {
		return nil, nil
	}
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq; seq++ {
		msg, err := stream.GetMsg(ctx, seq)
		if err != nil {
			return nil, fmt.Errorf("message %d of the stream %w: %w", seq, errUnreachable, err)
		}
		id := msg.Header.Get(jetstream.MsgIDHeader)
		arrival, err := parseArrival(id, msg.Header.Get("Cactusfs-Event-Type"), msg.Data)
		if err != nil {
			return nil, err
		}
		if arrival.Type != "cactusfs.notification.object.v1" {
			return nil, fmt.Errorf("message %s of the stream is of type %q", id, arrival.Type)
		}
		arrivals = append(arrivals, arrival)
	}
	return arrivals, nil
}

// notifyKey is what the test did to one key: how many times a put and a
// delete were sent until one was acknowledged.
type notifyKey struct {
	putTries, deleteTries int
	putAcked, deleteAcked bool
}

// notifyLedger is every change the test made, by key.
type notifyLedger struct {
	mu      sync.Mutex
	keys    map[string]*notifyKey
	retried int
}

func (l *notifyLedger) put(key string, tries int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := l.keys[key]
	if k == nil {
		k = &notifyKey{}
		l.keys[key] = k
	}
	k.putTries, k.putAcked = k.putTries+tries, true
	if tries > 1 {
		l.retried++
	}
}

func (l *notifyLedger) deleted(key string, tries int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := l.keys[key]
	k.deleteTries, k.deleteAcked = k.deleteTries+tries, true
	if tries > 1 {
		l.retried++
	}
}

// changesRetried returns how many changes were sent more than once.
func (l *notifyLedger) changesRetried() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.retried
}

// notifyVersion is a version or a delete marker the bucket holds: what
// exactly one message must tell.
type notifyVersion struct {
	key, event string
	size       int64
}

// bucketVersions lists every version and delete marker of the bucket, by
// version ID, and checks them against the ledger: each acknowledged put is
// at least one version and at most as many as were sent, each
// acknowledged delete likewise a delete marker, and nothing else is there.
func bucketVersions(ctx context.Context, s *s3.Client, ledger *notifyLedger) (map[string]notifyVersion, error) {
	versions := map[string]notifyVersion{}
	type counted struct{ versions, markers int }
	byKey := map[string]*counted{}
	count := func(key string) *counted {
		if byKey[key] == nil {
			byKey[key] = &counted{}
		}
		return byKey[key]
	}
	pages := s3.NewListObjectVersionsPaginator(s, &s3.ListObjectVersionsInput{Bucket: aws.String(notifyBucket)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("ListObjectVersions: %w", err)
		}
		for _, v := range page.Versions {
			versions[aws.ToString(v.VersionId)] = notifyVersion{key: aws.ToString(v.Key), event: eventPut, size: aws.ToInt64(v.Size)}
			count(aws.ToString(v.Key)).versions++
		}
		for _, m := range page.DeleteMarkers {
			versions[aws.ToString(m.VersionId)] = notifyVersion{key: aws.ToString(m.Key), event: eventMarker}
			count(aws.ToString(m.Key)).markers++
		}
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	for key, n := range byKey {
		k := ledger.keys[key]
		switch {
		case k == nil:
			return nil, fmt.Errorf("the bucket holds %q, which the test never wrote", key)
		case n.versions < 1 || n.versions > k.putTries:
			return nil, fmt.Errorf("%q was put %d times and has %d versions", key, k.putTries, n.versions)
		case !k.deleteAcked && n.markers > 0, k.deleteAcked && (n.markers < 1 || n.markers > k.deleteTries):
			return nil, fmt.Errorf("%q was deleted %d times and has %d delete markers", key, k.deleteTries, n.markers)
		}
	}
	for key := range ledger.keys {
		if byKey[key] == nil {
			return nil, fmt.Errorf("the bucket holds no version of %q, which was put", key)
		}
	}
	return versions, nil
}

// judge says what is wrong with the messages one place holds, against the
// versions and delete markers of the bucket: nil when each is told by
// exactly one message ID - of its key, its kind and, for a put, its size -
// and nothing else is told, and per key the first arrival of each ID is in
// ascending sequencer with no put after a delete marker; errNotYet when
// what is there is right and some version is not told yet. It returns the
// IDs held more than once, by how many times.
func judge(arrivals []notifyArrival, versions map[string]notifyVersion) (map[string]int, error) {
	seen := map[string]int{}
	first := map[string]notifyArrival{}
	told := map[string]string{}
	last := map[string]notifyArrival{}
	marked := map[string]bool{}
	for _, a := range arrivals {
		if len(a.ID) != 20 || a.ID != a.Sequencer {
			return nil, fmt.Errorf("message %q has the sequencer %q", a.ID, a.Sequencer)
		}
		seen[a.ID]++
		if before, ok := first[a.ID]; ok {
			if !bytes.Equal(before.Body, a.Body) {
				return nil, fmt.Errorf("message %s arrived again with another body", a.ID)
			}
			continue
		}
		first[a.ID] = a
		want, ok := versions[a.VersionID]
		switch {
		case !ok:
			return nil, fmt.Errorf("message %s tells of the version %q of %q, which the bucket does not hold", a.ID, a.VersionID, a.Key)
		case want.key != a.Key || want.event != a.Event:
			return nil, fmt.Errorf("message %s tells %s of %q for the version %s, which is %s of %q", a.ID, a.Event, a.Key, a.VersionID, want.event, want.key)
		case want.event == eventPut && (a.Size == nil || *a.Size != want.size):
			return nil, fmt.Errorf("message %s tells the size %v of the version %s, which has %d", a.ID, a.Size, a.VersionID, want.size)
		case told[a.VersionID] != "":
			return nil, fmt.Errorf("the version %s is told by two messages, %s and %s", a.VersionID, told[a.VersionID], a.ID)
		}
		told[a.VersionID] = a.ID
		// The changes of a key in the order they were made.
		if prev, ok := last[a.Key]; ok && prev.Sequencer >= a.Sequencer {
			return nil, fmt.Errorf("the messages of %q arrived out of order: %s after %s", a.Key, a.Sequencer, prev.Sequencer)
		}
		last[a.Key] = a
		if a.Event == eventPut && marked[a.Key] {
			return nil, fmt.Errorf("%q was told put after it was told deleted (%s)", a.Key, a.ID)
		}
		marked[a.Key] = marked[a.Key] || a.Event == eventMarker
	}
	if missing := len(versions) - len(told); missing > 0 {
		var some []string
		for id, v := range versions {
			if told[id] == "" && len(some) < 5 {
				some = append(some, fmt.Sprintf("%s of %q (%s)", v.event, v.key, id))
			}
		}
		sort.Strings(some)
		return nil, fmt.Errorf("%w: %d of %d versions, among them %s", errNotYet, missing, len(versions), strings.Join(some, "; "))
	}
	again := map[string]int{}
	for id, n := range seen {
		if n > 1 {
			again[id] = n
		}
	}
	return again, nil
}

// notifyWrite sends a change through the nodes, beginning with the one at
// first, until one acknowledges it, and returns how many times it was sent.
// A refusal (4xx) is not sent again; a failure of the connection or of the
// cluster (5xx) is, through the next node.
func notifyWrite(ctx context.Context, clients []*s3.Client, first int, change func(context.Context, *s3.Client) error) (int, error) {
	deadline := time.Now().Add(notifyWriteLimit)
	for tries := 1; ; tries++ {
		node := (first + tries - 1) % len(clients)
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := change(attempt, clients[node])
		cancel()
		if err == nil {
			return tries, nil
		}
		if status := statusOf(err); status >= 400 && status < 500 {
			return tries, fmt.Errorf("through %s: %w", clustertest.Nodes[node], err)
		}
		if time.Now().After(deadline) {
			return tries, fmt.Errorf("not acknowledged within %s, last through %s: %w", notifyWriteLimit, clustertest.Nodes[node], err)
		}
		select {
		case <-ctx.Done():
			return tries, fmt.Errorf("through %s: %w", clustertest.Nodes[node], ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// receiverTrail reads the records of the audit trail the receiver took, as
// a receiver deduplicates them: by the ID X-Cactusfs-Event-Id, which is
// audit-<the record's sequence>, a record kept at its first arrival - and a
// record that arrives again must be the same bytes. Every request must be
// signed under signingKey and carry the headers of ADR-0025, and its body be
// a line of the trail (audit.ParseLine). It returns the records in the order
// they first arrived, and how many IDs arrived more than once.
func receiverTrail(ctx context.Context, receiver *testcontainers.DockerContainer, signingKey string) ([]audit.Record, int, error) {
	lines, err := receiverFile(ctx, receiver, receiverAuditRecords)
	if err != nil {
		return nil, 0, err
	}
	first := map[string][]byte{}
	again := map[string]bool{}
	var records []audit.Record
	for _, line := range lines {
		var got receiverRecord
		if err := json.Unmarshal(line, &got); err != nil {
			return nil, 0, fmt.Errorf("a line of the receiver's audit records: %w", err)
		}
		if !webhook.Verify([]byte(signingKey), got.Timestamp, got.Body, got.Signature) {
			return nil, 0, fmt.Errorf("record %s: the signature %q does not verify", got.ID, got.Signature)
		}
		if got.ContentType != "application/json" || !strings.HasPrefix(got.UserAgent, "cactusfs/") || got.Type != "cactusfs.audit.exported.v1" {
			return nil, 0, fmt.Errorf("record %s: Content-Type %q, User-Agent %q, type %q", got.ID, got.ContentType, got.UserAgent, got.Type)
		}
		if before, ok := first[got.ID]; ok {
			if !bytes.Equal(before, got.Body) {
				return nil, 0, fmt.Errorf("record %s arrived again with another body", got.ID)
			}
			again[got.ID] = true
			continue
		}
		record, err := audit.ParseLine(got.Body)
		if err != nil {
			return nil, 0, fmt.Errorf("record %s: %w", got.ID, err)
		}
		if got.ID != "audit-"+strconv.FormatUint(record.Sequence, 10) {
			return nil, 0, fmt.Errorf("record %d arrived with the ID %q", record.Sequence, got.ID)
		}
		first[got.ID] = got.Body
		records = append(records, record)
	}
	return records, len(again), nil
}

// trailRecord is a record of the audit trail, as GET /audit gives it.
type trailRecord struct {
	Sequence uint64 `json:"sequence"`
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Hash     string `json:"hash"`
}

// readTrail reads the whole audit trail through a node, a page after
// another; errUnreachable when the node does not answer.
func readTrail(ctx context.Context, c *clustertest.Cluster, id string, root map[string]string) ([]trailRecord, error) {
	var all []trailRecord
	after := uint64(0)
	for {
		status, _, body, err := gridCall(ctx, c, id, root, http.MethodGet, "/audit?limit=1000&after="+strconv.FormatUint(after, 10), nil)
		switch {
		case err != nil:
			return nil, fmt.Errorf("GET /audit through %s %w: %w", id, errUnreachable, err)
		case status != http.StatusOK:
			return nil, fmt.Errorf("GET /audit through %s %w: %d %s", id, errUnreachable, status, body)
		}
		var page struct {
			Records []trailRecord `json:"records"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("GET /audit through %s: %w: %s", id, err, body)
		}
		if len(page.Records) == 0 {
			return all, nil
		}
		all = append(all, page.Records...)
		after = page.Records[len(page.Records)-1].Sequence
	}
}

// judgeTrail says what is wrong with the records of the audit trail the
// receiver holds (receiverTrail), against the trail as a node reads it: nil
// when they are every record from the first, in sequence and without a gap,
// their chain verifies, every record the node read is among them with its
// hash, and so is the creation of every group the test made; errNotYet when
// what is there is right and some is not there yet.
func judgeTrail(held []audit.Record, trail []trailRecord, groups []string) error {
	for i, record := range held {
		if record.Sequence != uint64(i)+1 {
			return fmt.Errorf("the receiver's record %d is %d: the trail arrived out of sequence or with a gap", i+1, record.Sequence)
		}
	}
	if err := audit.Verify(audit.Record{}, held); err != nil {
		return fmt.Errorf("the chain the receiver holds: %w", err)
	}
	for i, record := range trail {
		if i >= len(held) {
			return fmt.Errorf("%w: the receiver holds %d records of the %d of the trail", errNotYet, len(held), len(trail))
		}
		if record.Sequence != held[i].Sequence || record.Hash != held[i].Hash {
			return fmt.Errorf("record %d is not the trail's: hash %s, the trail's %s", held[i].Sequence, held[i].Hash, record.Hash)
		}
	}
	made := map[string]bool{}
	for _, record := range held {
		if record.Action == "admin:PutGroup" && record.Outcome == audit.Succeeded {
			made[record.Resource[strings.LastIndex(record.Resource, "/")+1:]] = true
		}
	}
	for _, group := range groups {
		if !made[group] {
			return fmt.Errorf("%w: the creation of the group %s is not among the %d records the receiver holds", errNotYet, group, len(held))
		}
	}
	return nil
}

// trailChange creates the group of the i-th administrative change through
// the nodes, beginning with the i-th, until one answers 201. Every attempt
// carries the same Idempotency-Key, so an attempt made again after one that
// was taken and not answered is answered as that one was.
func trailChange(ctx context.Context, c *clustertest.Cluster, root map[string]string, i int) (string, error) {
	name := fmt.Sprintf("trail-%02d", i)
	headers := map[string]string{"Idempotency-Key": "trail-change-" + name}
	for k, v := range root {
		headers[k] = v
	}
	deadline := time.Now().Add(notifyWriteLimit)
	for try := 0; ; try++ {
		id := clustertest.Nodes[(i+try)%len(clustertest.Nodes)]
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		status, _, body, err := gridCall(attempt, c, id, headers, http.MethodPost, "/groups", map[string]any{"name": name, "roles": []string{"read-only"}})
		cancel()
		switch {
		case err == nil && status == http.StatusCreated:
			return name, nil
		case err == nil && status == http.StatusConflict && try > 0:
			// An attempt before was committed and its answer lost with
			// the node that gave it: the group is there, and so is its
			// record, which judgeTrail looks for.
			return name, nil
		case err == nil && status >= 400 && status < 500:
			return name, fmt.Errorf("POST /groups %s through %s = %d %s", name, id, status, body)
		case time.Now().After(deadline) && err != nil:
			return name, fmt.Errorf("POST /groups %s not answered within %s, last through %s: %w", name, notifyWriteLimit, id, err)
		case time.Now().After(deadline):
			return name, fmt.Errorf("POST /groups %s not answered within %s, last through %s: %d %s", name, notifyWriteLimit, id, status, body)
		}
		select {
		case <-ctx.Done():
			return name, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// trailChangesFrom makes the administrative changes from first up to, not
// including, last, one after another and trailPace apart, counting in made
// those that were answered, and returns the names of the groups.
func trailChangesFrom(ctx context.Context, c *clustertest.Cluster, root map[string]string, first, last int, made *atomic.Int64) ([]string, error) {
	var names []string
	for i := first; i < last; i++ {
		name, err := trailChange(ctx, c, root, i)
		if err != nil {
			return names, err
		}
		names = append(names, name)
		made.Add(1)
		select {
		case <-ctx.Done():
			return names, ctx.Err()
		case <-time.After(trailPace):
		}
	}
	return names, nil
}

// targetStatus is what the leader knows of the delivery to a target.
type targetStatus struct {
	State          string `json:"state"`
	Pending        int64  `json:"pending"`
	Lost           int64  `json:"lost"`
	Rejected       int64  `json:"rejected"`
	LastErrorClass string `json:"last_error_class"`
}

// targetState asks a node for the status of a target.
func targetState(ctx context.Context, c *clustertest.Cluster, id string, root map[string]string, target string) (targetStatus, error) {
	status, _, body, err := gridCall(ctx, c, id, root, http.MethodGet, "/targets/"+target+"/status", nil)
	if err != nil {
		return targetStatus{}, err
	}
	if status != http.StatusOK {
		return targetStatus{}, fmt.Errorf("the status of %s through %s = %d %s", target, id, status, body)
	}
	var shown targetStatus
	if err := json.Unmarshal(body, &shown); err != nil {
		return targetStatus{}, fmt.Errorf("the status of %s through %s: %w: %s", target, id, err, body)
	}
	return shown, nil
}

// TestBucketNotifications: the notifications of a bucket's changes in the
// product as it is deployed (ADR-0025, PLAN 8.2.6), on four nodes, to a
// webhook and to NATS JetStream, through a change of leader and a receiver
// that is away.
//
// A NATS server (JetStream, TLS, a token) and a receiver - a static program
// of test/integration/receiver in the product's base image, TLS - run on
// the cluster's network, with certificates of a CA made for the test, which
// the two targets trust alone. Root defines both targets through the admin
// API and a bucket that keeps versions, whose rules, set with S3, tell
// them every put and delete.
//
// 2 000 objects are put and every fourth deleted again (a delete marker),
// through all nodes; a change that fails is sent again through another
// node until one acknowledges it. The receiver holds one request unanswered
// - the 800th it takes - and the leader, which waits for its answer, is
// killed then: the new leader must send it again. Then every version and
// delete marker the bucket lists is told by exactly one message ID, at the
// receiver and in the stream, and nothing else is; the first arrival of
// each ID is, per key, in ascending sequencer; the receiver (which keeps
// every request) took at least one ID more than once and at most 200; the
// stream (which deduplicates by Nats-Msg-Id) holds none twice; every
// request is signed under the key the target was given, and carries the
// headers of ADR-0025.
//
// Then the receiver is stopped for a minute while objects are put: every
// write is acknowledged, the receiver's target shows retrying; once it is
// back, it shows ok, and the messages of that minute arrived. At the end
// both targets are ok, nothing pending, lost or rejected, and a message
// published again to the stream with its ID is taken as a duplicate.
//
// The audit trail goes to the receiver too (PLAN 8.2.8), through a third
// target defined with audit: true, at the same endpoint and with a signing
// key of its own; the receiver keeps those records in a file of their own.
// 50 administrative changes are made - a group created each, through the
// nodes in turn - 25 begun while the receiver holds its request, the leader
// killed once three of them are answered and the rest on their way, and 25
// after them. At the end the receiver,
// deduplicating by X-Cactusfs-Event-Id (audit-<sequence>), holds every
// record of the trail from the first, in sequence and without a gap, each
// the trail's (its hash as GET /audit gives it) and the creation of every
// group among them; their hash chain verifies; every request is signed
// under the trail target's key; a record that arrived twice arrived the
// same bytes; and the target is ok.
//
// No --egress-allow: the containers are in the engine's private range,
// which a node connects to without it (ADR-0023).
//
// Not parallel, one cluster (PLAN, Milestone 8).
func TestBucketNotifications(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()
	bin := buildReceiver(ctx, t)
	c := clustertest.Start(t, clustertest.Options{})
	nodes := clustertest.Nodes
	root := gridBearer(c.AccessKey, c.SecretKey)

	ca := newNotifyCA(t)
	token, bearer, path := randomToken(t), randomToken(t), "/hooks/"+randomToken(t)
	natsCtr := startNATS(ctx, t, c.Network(), ca, token)
	receiver := startReceiver(ctx, t, c.Network(), bin, ca, path, bearer)

	// The stream is the operator's: the test makes it, with a window of
	// deduplication longer than the test.
	host, err := natsCtr.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := natsCtr.MappedPort(ctx, "4222/tcp")
	if err != nil {
		t.Fatal(err)
	}
	nc, err := natsgo.Connect("tls://"+net.JoinHostPort(host, port.Port()),
		natsgo.Secure(&tls.Config{RootCAs: ca.pool, ServerName: natsHost, MinVersion: tls.VersionTLS12}),
		natsgo.Token(token), natsgo.Timeout(10*time.Second))
	if err != nil {
		t.Fatalf("connect to NATS: %v", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: notifyStreamName, Subjects: []string{notifySubject}, Storage: jetstream.FileStorage, Duplicates: time.Hour,
	})
	if err != nil {
		t.Fatalf("create the stream: %v", err)
	}

	// The targets, through the admin API (503 until the cluster writes at
	// metadata level 5).
	define := func(name string, definition map[string]any) string {
		t.Helper()
		var made struct {
			SigningKey string `json:"signing_key"`
		}
		c.Eventually("the target "+name+" to be defined", 2*time.Minute, func() bool {
			status, _, body, err := gridCall(ctx, c, "node2", root, http.MethodPut, "/targets/"+name, definition)
			switch {
			case err != nil || status == http.StatusServiceUnavailable:
				return false
			case status != http.StatusOK:
				t.Fatalf("PUT the target %s = %d %s", name, status, body)
			}
			if err := json.Unmarshal(body, &made); err != nil {
				t.Fatalf("PUT the target %s: %v", name, err)
			}
			return true
		})
		return made.SigningKey
	}
	signingKey := define(notifyHook, map[string]any{
		"kind": "webhook", "endpoint": "https://" + receiverHost + ":" + receiverPort + path, "ca_pem": string(ca.pem),
		"buckets": []string{notifyBucket}, "auth": map[string]any{"mode": "bearer", "secret": bearer},
	})
	if signingKey == "" {
		t.Fatal("the webhook was defined without a signing key shown")
	}
	define(notifyStream, map[string]any{
		"kind": "nats", "endpoint": "tls://" + natsHost + ":4222", "subject": notifySubject, "ca_pem": string(ca.pem),
		"buckets": []string{notifyBucket}, "auth": map[string]any{"mode": "token", "secret": token},
	})
	// The audit trail, to the same receiver: no bucket, a key of its own.
	trailKey := define(notifyTrail, map[string]any{
		"kind": "webhook", "endpoint": "https://" + receiverHost + ":" + receiverPort + path, "ca_pem": string(ca.pem),
		"buckets": []string{}, "audit": true, "auth": map[string]any{"mode": "bearer", "secret": bearer},
	})
	if trailKey == "" || trailKey == signingKey {
		t.Fatal("the target of the audit trail was defined without a signing key of its own")
	}

	// The bucket, which keeps versions, and its rules, with S3.
	if _, err := c.S3("node1").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(notifyBucket)}); err != nil {
		t.Fatalf("CreateBucket %s: %v", notifyBucket, err)
	}
	if _, err := c.S3("node2").PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(notifyBucket), VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatalf("PutBucketVersioning %s: %v", notifyBucket, err)
	}
	everything := []types.Event{types.EventS3ObjectCreated, types.EventS3ObjectRemoved}
	if _, err := c.S3("node3").PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{
		Bucket: aws.String(notifyBucket),
		NotificationConfiguration: &types.NotificationConfiguration{
			QueueConfigurations: []types.QueueConfiguration{{QueueArn: aws.String("arn:cactusfs:notify:::" + notifyHook), Events: everything}},
			TopicConfigurations: []types.TopicConfiguration{{TopicArn: aws.String("arn:cactusfs:notify:::" + notifyStream), Events: everything}},
		},
	}); err != nil {
		t.Fatalf("PutBucketNotificationConfiguration: %v", err)
	}

	clients := make([]*s3.Client, len(nodes))
	for i, id := range nodes {
		clients[i] = c.S3(id)
	}
	ledger := &notifyLedger{keys: map[string]*notifyKey{}}
	// What an object holds does not matter here, only its size.
	put := func(ctx context.Context, i int, key string, size int) error {
		data := bytes.Repeat([]byte("n"), size)
		tries, err := notifyWrite(ctx, clients, i, func(ctx context.Context, s *s3.Client) error {
			_, err := s.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(notifyBucket), Key: aws.String(key), Body: bytes.NewReader(data)})
			return err
		})
		if err != nil {
			return fmt.Errorf("PutObject %q: %w", key, err)
		}
		ledger.put(key, tries)
		return nil
	}
	remove := func(ctx context.Context, i int, key string) error {
		tries, err := notifyWrite(ctx, clients, i, func(ctx context.Context, s *s3.Client) error {
			_, err := s.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(notifyBucket), Key: aws.String(key)})
			return err
		})
		if err != nil {
			return fmt.Errorf("DeleteObject %q: %w", key, err)
		}
		ledger.deleted(key, tries)
		return nil
	}

	// 2 000 puts and 500 deletes, through all nodes. Keys hold a space, a
	// plus and a slash, which the record encodes.
	var acked atomic.Int64
	var workers sync.WaitGroup
	var failures []error
	var failuresMu sync.Mutex
	for w := range notifyWorkers {
		workers.Go(func() {
			for i := w; i < notifyPuts; i += notifyWorkers {
				key := fmt.Sprintf("burst/w%d/key %04d+x", w, i)
				err := put(ctx, i, key, 100+i%300)
				if err == nil && i%4 == 0 {
					acked.Add(1)
					err = remove(ctx, i+1, key)
				}
				if err != nil {
					failuresMu.Lock()
					failures = append(failures, err)
					failuresMu.Unlock()
					return
				}
				acked.Add(1)
			}
		})
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()

	// The leader is killed while it waits for the answer to the request the
	// receiver holds: that message was taken and never acknowledged.
	var lines [][]byte
	held := time.Now().Add(notifyWriteLimit)
	for len(lines) < notifyHoldAt {
		select {
		case <-done:
			t.Fatalf("the writes ended (%d acknowledged) before the receiver took %d requests", acked.Load(), notifyHoldAt)
		case <-time.After(250 * time.Millisecond):
		}
		var err error
		if lines, err = receiverLines(ctx, receiver); time.Now().After(held) {
			t.Fatalf("the receiver took %d requests within %s (%v); the leader is killed at the %dth", len(lines), notifyWriteLimit, err, notifyHoldAt)
		}
	}
	// Half of the administrative changes begin now, while the receiver
	// holds its request; the leader is killed once the first few are
	// answered and the rest are on their way - within seconds, far inside
	// the receiver's hold of two minutes. Whether the new leader sends a
	// record of the trail again depends on what the old one had not
	// acknowledged: it is logged, not required.
	var groups []string
	var groupsErr error
	var made atomic.Int64
	beforeKill := make(chan struct{})
	go func() {
		defer close(beforeKill)
		groups, groupsErr = trailChangesFrom(ctx, c, root, 0, trailChanges/2, &made)
	}()
	begun := time.Now()
	for made.Load() < trailKillAfter {
		select {
		case <-beforeKill:
			t.Fatalf("the administrative changes before the kill ended after %d: %v", made.Load(), groupsErr)
		case <-time.After(50 * time.Millisecond):
		}
		if time.Since(begun) > notifyWriteLimit/4 {
			t.Fatalf("%d administrative changes answered within %s", made.Load(), notifyWriteLimit/4)
		}
	}
	victim, err := askLeader(ctx, c, "node4")
	if err != nil {
		t.Fatalf("who leads before the kill: %v", err)
	}
	killed := time.Now()
	c.Kill(victim)
	ackedAtKill := acked.Load()
	madeAtKill := made.Load()
	<-beforeKill
	if groupsErr != nil {
		t.Fatalf("an administrative change before the kill: %v", groupsErr)
	}
	// The other half after them, while the cluster settles.
	afterKill, err := trailChangesFrom(ctx, c, root, trailChanges/2, trailChanges, &made)
	if err != nil {
		t.Fatalf("an administrative change after the kill: %v", err)
	}
	groups = append(groups, afterKill...)
	t.Logf("the leader was killed with %d of the administrative changes answered, the others on their way", madeAtKill)
	<-done
	if err := errors.Join(failures...); err != nil {
		t.Fatalf("a change was not acknowledged: %v", err)
	}
	if got := acked.Load(); got != notifyPuts+notifyDeletes {
		t.Fatalf("%d changes acknowledged; want %d", got, notifyPuts+notifyDeletes)
	}
	t.Logf("the leader %s was killed holding request %d unanswered, after %d changes were acknowledged; the changes went on for %s after it, %d of them sent more than once",
		victim, notifyHoldAt, ackedAtKill, time.Since(killed).Round(time.Second), ledger.changesRetried())
	c.Restart(victim)

	// Every version arrives at both places. What is wrong with what has
	// arrived fails at once; what has not arrived yet, or could not be
	// read, is waited for.
	var atReceiver []notifyArrival
	arrived := func(what string, limit time.Duration) map[string]notifyVersion {
		t.Helper()
		versions, err := bucketVersions(ctx, c.S3("node1"), ledger)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		deadline := time.Now().Add(limit)
		for {
			if atReceiver, err = receiverArrivals(ctx, receiver, signingKey); err == nil {
				if _, err = judge(atReceiver, versions); err != nil {
					err = fmt.Errorf("at the receiver: %w", err)
				}
			}
			if err == nil {
				var inStream []notifyArrival
				if inStream, err = streamArrivals(ctx, stream); err == nil {
					if _, err = judge(inStream, versions); err != nil {
						err = fmt.Errorf("in the stream: %w", err)
					}
				}
			}
			switch {
			case err == nil:
				return versions
			case !errors.Is(err, errNotYet) && !errors.Is(err, errUnreachable):
				t.Fatalf("%s: %v", what, err)
			case time.Now().After(deadline):
				t.Fatalf("%s within %s: %v", what, limit, err)
			}
			time.Sleep(2 * time.Second)
		}
	}
	arrived("the changes of the burst to arrive at the receiver and the stream", notifyCatchUp)

	// The receiver is away for a minute while writes go on.
	grace := 10 * time.Second
	if err := receiver.Stop(ctx, &grace); err != nil {
		t.Fatalf("stop the receiver: %v", err)
	}
	stopped := time.Now()
	var sawRetrying bool
	var states []string
	for i := 0; time.Since(stopped) < notifyReceiverDown; i++ {
		key := fmt.Sprintf("away/key %04d", i)
		data := bytes.Repeat([]byte("n"), 50+i%100)
		through := nodes[i%len(nodes)]
		if _, err := c.S3(through).PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(notifyBucket), Key: aws.String(key), Body: bytes.NewReader(data)}); err != nil {
			t.Fatalf("with the receiver away, PutObject %q through %s: %v", key, through, err)
		}
		ledger.put(key, 1)
		if !sawRetrying {
			status, err := targetState(ctx, c, through, root, notifyHook)
			if err != nil {
				t.Fatalf("with the receiver away: %v", err)
			}
			states = append(states, status.State+"/"+status.LastErrorClass)
			sawRetrying = status.State == "retrying" && status.Pending > 0
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !sawRetrying {
		t.Fatalf("with the receiver away for %s, its target never showed retrying with messages pending: %v", notifyReceiverDown, states)
	}
	if err := receiver.Start(ctx); err != nil {
		t.Fatalf("start the receiver again: %v", err)
	}
	returned := time.Now()
	asked := 0
	c.Eventually("the receiver's target to show ok", notifyCatchUp, func() bool {
		asked++
		status, err := targetState(ctx, c, nodes[asked%len(nodes)], root, notifyHook)
		return err == nil && status.State == "ok" && status.Pending == 0
	})
	t.Logf("the receiver's target was ok again %s after the receiver returned", time.Since(returned).Round(time.Second))
	versions := arrived("the changes made while the receiver was away to arrive", notifyCatchUp)
	for _, target := range []string{notifyHook, notifyStream} {
		status, err := targetState(ctx, c, "node1", root, target)
		if err != nil || status != (targetStatus{State: "ok"}) {
			t.Errorf("at the end the target %s is %+v, %v; want ok, and nothing pending, lost or rejected", target, status, err)
		}
	}

	// The audit trail: every record from the first, in sequence, without a
	// gap, the trail's own, the 50 changes among them, the chain whole.
	var trailHeld []audit.Record
	var trailAgain int
	trailDeadline := time.Now().Add(notifyCatchUp)
	for asked := 0; ; asked++ {
		var trail []trailRecord
		trail, err = readTrail(ctx, c, nodes[asked%len(nodes)], root)
		if err == nil {
			trailHeld, trailAgain, err = receiverTrail(ctx, receiver, trailKey)
		}
		if err == nil {
			err = judgeTrail(trailHeld, trail, groups)
		}
		if err == nil {
			var status targetStatus
			status, err = targetState(ctx, c, nodes[asked%len(nodes)], root, notifyTrail)
			switch {
			case err != nil:
				// A node that does not answer the status now is asked
				// again, as the waits above ask.
				err = fmt.Errorf("the status %w: %w", errUnreachable, err)
			case status != (targetStatus{State: "ok"}):
				err = fmt.Errorf("%w: the target of the audit trail is %+v", errNotYet, status)
			}
		}
		switch {
		case err == nil:
		case !errors.Is(err, errNotYet) && !errors.Is(err, errUnreachable):
			t.Fatalf("the audit trail at the receiver: %v", err)
		case time.Now().After(trailDeadline):
			t.Fatalf("the audit trail at the receiver within %s: %v", notifyCatchUp, err)
		default:
			time.Sleep(2 * time.Second)
			continue
		}
		break
	}
	t.Logf("the audit trail: %d records at the receiver, in sequence, the chain whole, %d of them arrived more than once", len(trailHeld), trailAgain)

	// What arrived more than once: the receiver counts - the message it
	// held at least - and the stream deduplicates.
	again, err := judge(atReceiver, versions)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) < 1 || len(again) > notifyResentAtMost {
		t.Errorf("%d IDs arrived at the receiver more than once; want at least the one it held, at most %d", len(again), notifyResentAtMost)
	}
	inStream, err := streamArrivals(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := judge(inStream, versions)
	if err != nil {
		t.Fatal(err)
	}
	if len(twice) != 0 {
		t.Errorf("%d IDs are in the stream more than once, which deduplicates by Nats-Msg-Id", len(twice))
	}
	// The same messages in both, with the same records.
	bodies := map[string][]byte{}
	for _, a := range atReceiver {
		bodies[a.ID] = a.Body
	}
	for _, a := range inStream {
		body, ok := bodies[a.ID]
		if !ok {
			t.Errorf("message %s is in the stream and did not arrive at the receiver", a.ID)
			continue
		}
		if !bytes.Equal(body, a.Body) {
			t.Errorf("message %s is not the same record in the stream and at the receiver", a.ID)
		}
		delete(bodies, a.ID)
	}
	for id := range bodies {
		t.Errorf("message %s arrived at the receiver and is not in the stream", id)
	}
	// The stream takes a message published again with its ID as a
	// duplicate, as a node's publish sent again is taken.
	if len(inStream) > 0 {
		msg := natsgo.NewMsg(notifySubject)
		msg.Header.Set(jetstream.MsgIDHeader, inStream[0].ID)
		msg.Data = inStream[0].Body
		ack, err := js.PublishMsg(ctx, msg)
		if err != nil || !ack.Duplicate {
			t.Errorf("message %s published again to the stream: %+v, %v; want it taken as a duplicate", inStream[0].ID, ack, err)
		}
		if after, err := streamArrivals(ctx, stream); err != nil || len(after) != len(inStream) {
			t.Errorf("the stream holds %d messages after a duplicate was published (%v); want %d", len(after), err, len(inStream))
		}
	}
	t.Logf("%d versions and delete markers, %d messages: %d arrivals at the receiver, %d IDs more than once; %d in the stream, none twice; %d changes sent more than once",
		len(versions), len(inStream), len(atReceiver), len(again), len(inStream), ledger.changesRetried())
}
