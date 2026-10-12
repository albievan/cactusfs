//go:build integration && !windows

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/test/clustertest"
)

const (
	// rotationInterval is how often the nodes of this test read their
	// certificate files again: the shortest there is.
	rotationInterval = "5s"
	// rotationWait is that interval and a margin for a busy machine.
	rotationWait = 45 * time.Second
	// rotationProbe names the test itself when it dials node RPC. It is a
	// certificate of the cluster's node CA, as any caller there must show.
	rotationProbe = "probe-1"
)

// rotationPKI is the two CAs of the test cluster, kept by the test so that it
// can issue the certificates it replaces the first ones with. "cactusfs
// dev-init" keeps no CA key, as it should not.
type rotationPKI struct {
	nodeCA *nodetls.Authority
	s3CA   *nodetls.Authority
	// probe is what the test shows when it dials node RPC.
	probe tls.Certificate
}

func newRotationPKI(t *testing.T) *rotationPKI {
	t.Helper()
	nodeCA, err := nodetls.NewAuthority(rand.Reader, time.Now(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s3CA, err := nodetls.NewNamedAuthority(rand.Reader, time.Now(), 24*time.Hour, "CactusFS S3 CA (rotation test)")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := nodeCA.Issue(rotationProbe, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	probe, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return &rotationPKI{nodeCA: nodeCA, s3CA: s3CA, probe: probe}
}

func rotationSerial(t *testing.T, certPEM []byte) string {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("no PEM block in the certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate.SerialNumber.String()
}

// rotationNodeFiles issues a node certificate from the given CA for the given name
// and returns it as the files of the node with the given ID - key first, so
// that a node that reads between the two finds a pair that does not match
// and waits, not a certificate without a key. With it comes the serial
// number.
func rotationNodeFiles(t *testing.T, ca *nodetls.Authority, id, named string) ([]clustertest.ConfigFile, string) {
	t.Helper()
	certPEM, keyPEM, err := ca.Issue(named, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return []clustertest.ConfigFile{
		{Name: "nodes/" + id + ".key", Data: keyPEM}, {Name: "nodes/" + id + ".pem", Data: certPEM},
	}, rotationSerial(t, certPEM)
}

// s3Files issues the certificate all nodes show on their S3 and admin
// listeners.
func (p *rotationPKI) s3Files(t *testing.T) ([]clustertest.ConfigFile, string) {
	t.Helper()
	names := append([]string{"localhost"}, clustertest.Nodes...)
	certPEM, keyPEM, err := p.s3CA.IssueServer(names, []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return []clustertest.ConfigFile{{Name: "s3.key", Data: keyPEM}, {Name: "s3.pem", Data: certPEM}}, rotationSerial(t, certPEM)
}

// install replaces what dev-init wrote by certificates of the test's CAs,
// before any node has started.
func (p *rotationPKI) install(t *testing.T, c *clustertest.Cluster) {
	t.Helper()
	files := []clustertest.ConfigFile{{Name: "node-ca.pem", Data: p.nodeCA.CertPEM}, {Name: "s3-ca.pem", Data: p.s3CA.CertPEM}}
	for _, id := range clustertest.Nodes {
		pair, _ := rotationNodeFiles(t, p.nodeCA, id, id)
		files = append(files, pair...)
	}
	pair, _ := p.s3Files(t)
	c.WriteConfig(append(files, pair...)...)
}

// shown completes a handshake with a listener of a node and returns the
// serial number of the certificate it was shown. The certificate is verified
// as a client of that listener verifies it: under the S3 CA for the S3 and
// admin listeners, and under the node CA, as the node that was meant, with a
// node certificate shown in return, for node RPC.
func (p *rotationPKI) shown(t *testing.T, c *clustertest.Cluster, id string, port int) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	address, err := c.Address(ctx, id, port)
	if err != nil {
		return "", err
	}
	roots := x509.NewCertPool()
	config := &tls.Config{RootCAs: roots, ServerName: id, MinVersion: tls.VersionTLS12}
	if port == 9002 {
		roots.AppendCertsFromPEM(p.nodeCA.CertPEM)
		config.MinVersion = tls.VersionTLS13
		config.Certificates = []tls.Certificate{p.probe}
		config.NextProtos = []string{"h2"}
	} else {
		roots.AppendCertsFromPEM(p.s3CA.CertPEM)
	}
	conn, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: config}).DialContext(ctx, "tcp", address)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	secured, ok := conn.(*tls.Conn)
	if !ok {
		return "", errors.New("not a TLS connection")
	}
	return secured.ConnectionState().PeerCertificates[0].SerialNumber.String(), nil
}

// mustShow fails the test unless a new handshake with the listener is shown
// the certificate with the given serial number.
func (p *rotationPKI) mustShow(t *testing.T, c *clustertest.Cluster, id string, port int, serial, when string) {
	t.Helper()
	if got, err := p.shown(t, c, id, port); err != nil || got != serial {
		t.Errorf("%s: port %d of %s shows certificate %s, %v; want %s", when, port, id, got, err, serial)
	}
}

// rotationLeader asks a node who leads the metadata group.
func rotationLeader(t *testing.T, c *clustertest.Cluster, id string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	return askLeader(ctx, c, id)
}

func askLeader(ctx context.Context, c *clustertest.Cluster, id string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+id+":9001/api/v1/cluster", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.AccessKey+":"+c.SecretKey)
	resp, err := c.Admin(id).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /api/v1/cluster through %s: %d", id, resp.StatusCode)
	}
	var status struct {
		Leader string `json:"leader"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		return "", err
	}
	if status.Leader == "" {
		return "", fmt.Errorf("node %s knows of no leader", id)
	}
	return status.Leader, nil
}

// The two lines hashicorp/raft logs when a node stands for election and when
// it wins one. They are that library's wording, not a contract of it.
const (
	raftStoodForElection = "entering candidate state"
	raftWonElection      = "election won"
)

// rotationElections counts, over the logs of all nodes, how often a node
// stood for election and how often one won.
//
// This is an inference, and a weak one: the term of the metadata group is
// not shown by any API, so that it has not changed is concluded from two
// strings of a library's log not having appeared again. It is the second
// line of evidence only. The first is the test in cmd/cactusfs,
// TestClusterRotatesNodeCertificatesAndKeepsItsLeader, which reads the term
// from Raft itself, on the same transport. Should the library word its lines
// otherwise, the counts here stay at zero for ever - which is why the test
// demands, before it replaces anything, that each line was seen while the
// cluster formed.
func rotationElections(c *clustertest.Cluster) (stood, won int) {
	for _, id := range clustertest.Nodes {
		log := c.Logs(id)
		stood += strings.Count(log, raftStoodForElection)
		won += strings.Count(log, raftWonElection)
	}
	return stood, won
}

// rotationLoad writes and reads objects through every node and asks every
// node's management API, over and over, each request on a connection of its
// own: every one of them is a new handshake.
type rotationLoad struct {
	stop   context.CancelFunc
	done   sync.WaitGroup
	rounds map[string]*atomic.Int64

	mu       sync.Mutex
	failures []string
}

func startRotationLoad(t *testing.T, c *clustertest.Cluster, bucket string) *rotationLoad {
	t.Helper()
	ctx, stop := context.WithCancel(t.Context())
	load := &rotationLoad{stop: stop, rounds: map[string]*atomic.Int64{}}
	for _, id := range clustertest.Nodes {
		rounds := &atomic.Int64{}
		load.rounds[id] = rounds
		data := randomBytes(t, 32<<10)
		load.done.Go(func() {
			for round := 0; ctx.Err() == nil; round++ {
				if err := load.round(ctx, c, id, bucket, fmt.Sprintf("%s/object-%04d", id, round), data); err != nil {
					// A request cut off because the test is over is no failure.
					if ctx.Err() != nil {
						return
					}
					load.fail(fmt.Sprintf("%s round %d through %s: %v", time.Now().Format(time.RFC3339Nano), round, id, err))
				}
				rounds.Add(1)
			}
		})
	}
	return load
}

func (l *rotationLoad) round(ctx context.Context, c *clustertest.Cluster, id, bucket, key string, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := c.Put(ctx, id, bucket, key, data); err != nil {
		return fmt.Errorf("PUT: %w", err)
	}
	got, err := c.Get(ctx, id, bucket, key)
	if err != nil {
		return fmt.Errorf("GET: %w", err)
	}
	if !bytes.Equal(got, data) {
		return errors.New("GET: the object is not what was written")
	}
	if _, err := askLeader(ctx, c, id); err != nil {
		return fmt.Errorf("management API: %w", err)
	}
	return nil
}

func (l *rotationLoad) fail(what string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.failures) < 20 {
		l.failures = append(l.failures, what)
	}
}

func (l *rotationLoad) failed() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.failures...)
}

// more waits until every node has served the given number of rounds more
// than it has now.
func (l *rotationLoad) more(c *clustertest.Cluster, rounds int64) {
	want := map[string]int64{}
	for id, done := range l.rounds {
		want[id] = done.Load() + rounds
	}
	c.Eventually(fmt.Sprintf("%d more rounds of requests through every node", rounds), 3*time.Minute, func() bool {
		for id, done := range l.rounds {
			if done.Load() < want[id] {
				return false
			}
		}
		return true
	})
}

func (l *rotationLoad) finish() {
	l.stop()
	l.done.Wait()
}

// TestCertificateRotation is the acceptance test of PLAN 7.5, on the cluster
// of four containers: while requests go through every node without pause,
// the node certificate of every node and the certificate of the S3 and admin
// listeners are replaced on disk. Within the interval, new handshakes are
// shown the new certificates on the S3, admin and node RPC ports; no request
// fails; the metadata group keeps its leader and holds no election. A node
// certificate that names another node, and one of another CA, are refused
// and logged, and the one in use stays.
func TestCertificateRotation(t *testing.T) {
	t.Parallel()
	pki := newRotationPKI(t)
	c := clustertest.Start(t, clustertest.Options{
		Env:     map[string]string{"CACTUSFS_TLS_RELOAD_INTERVAL": rotationInterval},
		Prepare: func(c *clustertest.Cluster) { pki.install(t, c) },
		// The test dials node RPC to see which certificate it is shown.
		PublishNodeRPC: true,
	})
	nodes := clustertest.Nodes
	const bucket = "rotation"
	createRotationBucket(t, c, bucket)

	leader, err := rotationLeader(t, c, "node1")
	if err != nil {
		t.Fatalf("the leader before the rotation: %v", err)
	}
	// What every listener shows before anything is replaced.
	before := map[string]string{}
	for _, id := range nodes {
		for _, port := range []int{9000, 9001, 9002} {
			serial, err := pki.shown(t, c, id, port)
			if err != nil {
				t.Fatalf("handshake with port %d of %s before the rotation: %v", port, id, err)
			}
			before[fmt.Sprintf("%s:%d", id, port)] = serial
		}
	}

	load := startRotationLoad(t, c, bucket)
	defer load.finish()
	load.more(c, 2)
	// A cluster that formed held an election, and somebody won it. If the
	// logs do not say so in the words that are counted, the counts below
	// would prove nothing: the test fails here, before anything is replaced.
	stood, won := rotationElections(c)
	if stood == 0 || won == 0 {
		t.Fatalf("the logs of the cluster that formed hold %q %d times and %q %d times, want each at least once: "+
			"hashicorp/raft words its log otherwise, and no election could be counted", raftStoodForElection, stood, raftWonElection, won)
	}

	// The node certificates, one node after the other.
	inUse := map[string]string{}
	for _, id := range nodes {
		files, serial := rotationNodeFiles(t, pki.nodeCA, id, id)
		c.WriteConfig(files...)
		c.Eventually("node RPC of "+id+" to show its new certificate", rotationWait, func() bool {
			got, err := pki.shown(t, c, id, 9002)
			return err == nil && got == serial
		})
		if serial == before[id+":9002"] {
			t.Fatalf("%s was issued the certificate it had", id)
		}
		inUse[id] = serial
		// The cluster goes on through it: requests are served by every
		// node, and the leader is who it was.
		load.more(c, 2)
		if now, err := rotationLeader(t, c, id); err != nil || now != leader {
			t.Fatalf("after the certificate of %s was replaced the leader is %q, %v; it was %s", id, now, err, leader)
		}
	}

	// The certificate of the S3 and admin listeners, which the nodes share.
	files, s3Serial := pki.s3Files(t)
	c.WriteConfig(files...)
	for _, id := range nodes {
		for _, port := range []int{9000, 9001} {
			c.Eventually(fmt.Sprintf("port %d of %s to show the new certificate", port, id), rotationWait, func() bool {
				got, err := pki.shown(t, c, id, port)
				return err == nil && got == s3Serial
			})
		}
	}
	load.more(c, 3)
	load.finish()

	if failures := load.failed(); len(failures) != 0 {
		t.Errorf("%d requests failed while certificates were replaced:\n%s", len(failures), strings.Join(failures, "\n"))
	}
	if now, err := rotationLeader(t, c, "node4"); err != nil || now != leader {
		t.Errorf("after the rotation the leader is %q, %v; it was %s", now, err, leader)
	}
	if nowStood, nowWon := rotationElections(c); nowStood != stood || nowWon != won {
		t.Errorf("the metadata group held an election during the rotation: candidates %d before, %d after; elections won %d before, %d after",
			stood, nowStood, won, nowWon)
	}
	for _, id := range nodes {
		if !c.Ready(id) {
			t.Errorf("node %s is not ready after the rotation", id)
		}
		log := c.Logs(id)
		for _, kind := range []string{"node", "server"} {
			if !rotationLogged(log, "certificate replaced", `"certificate":"`+kind+`"`) {
				t.Errorf("the log of %s does not say that its %s certificate was replaced", id, kind)
			}
		}
	}

	// What must not be loaded is refused and logged, and the certificate in
	// use stays.
	foreign, err := nodetls.NewAuthority(rand.Reader, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	refusals := []struct {
		what   string
		ca     *nodetls.Authority
		named  string
		reason string
	}{
		{"a certificate that names another node", pki.nodeCA, "node2", "identity"},
		{"a certificate of another CA", foreign, "node1", "untrusted"},
	}
	for _, refusal := range refusals {
		files, refused := rotationNodeFiles(t, refusal.ca, "node1", refusal.named)
		c.WriteConfig(files...)
		c.Eventually("node1 to refuse "+refusal.what, rotationWait, func() bool {
			return rotationLogged(c.Logs("node1"), `"level":"ERROR"`, `"reason":"`+refusal.reason+`"`, `"certificate":"node"`, refused)
		})
		pki.mustShow(t, c, "node1", 9002, inUse["node1"], "with "+refusal.what+" on disk")
	}
	// Half a pair for the listeners clients reach: a warning, and the one
	// in use stays.
	half, _ := pki.s3Files(t)
	c.WriteConfig(half[1])
	c.Eventually("the nodes to see half a pair", rotationWait, func() bool {
		return rotationLogged(c.Logs("node3"), `"level":"WARN"`, `"reason":"pair"`, `"certificate":"server"`)
	})
	for _, id := range nodes {
		pki.mustShow(t, c, id, 9000, s3Serial, "with half a pair on disk")
		pki.mustShow(t, c, id, 9001, s3Serial, "with half a pair on disk")
	}

	// The cluster is what it was: it serves, node1 too, and has its leader.
	verifyRotationRoundTrip(t, c, bucket)
	if now, err := rotationLeader(t, c, "node1"); err != nil || now != leader {
		t.Errorf("at the end the leader is %q, %v; it was %s", now, err, leader)
	}
	if nowStood, nowWon := rotationElections(c); nowStood != stood || nowWon != won {
		t.Errorf("the metadata group held an election after a certificate was refused: candidates %d before, %d after; elections won %d before, %d after",
			stood, nowStood, won, nowWon)
	}
	// No key in any log.
	for _, id := range nodes {
		if strings.Contains(c.Logs(id), "PRIVATE KEY") {
			t.Errorf("the log of %s holds a private key", id)
		}
	}
}

// rotationLogged reports whether one line of a log contains all the parts.
func rotationLogged(log string, parts ...string) bool {
	for line := range strings.SplitSeq(log, "\n") {
		all := true
		for _, part := range parts {
			all = all && strings.Contains(line, part)
		}
		if all {
			return true
		}
	}
	return false
}

func createRotationBucket(t *testing.T, c *clustertest.Cluster, bucket string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, err := c.S3("node1").CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
}

// verifyRotationRoundTrip writes through node1 and reads through node2.
func verifyRotationRoundTrip(t *testing.T, c *clustertest.Cluster, bucket string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	data := randomBytes(t, 8<<10)
	if err := c.Put(ctx, "node1", bucket, "at-the-end", data); err != nil {
		t.Fatalf("PUT through node1 at the end: %v", err)
	}
	if got, err := c.Get(ctx, "node2", bucket, "at-the-end"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("GET through node2 at the end: %d bytes, %v", len(got), err)
	}
}
