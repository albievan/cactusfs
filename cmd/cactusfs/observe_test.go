package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/albievan/cactusfs/internal/adapters/health"
	"github.com/albievan/cactusfs/internal/adapters/nodetls"
	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/adapters/s3api"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// samples returns every sample of a node's metrics, as
// "name{label=value,...}" to its value; of a histogram, its count.
func samples(t *testing.T, tel *telemetry.Telemetry) map[string]float64 {
	t.Helper()
	families, err := tel.Gatherer().Gather()
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			var labels []string
			for _, label := range metric.GetLabel() {
				labels = append(labels, label.GetName()+"="+label.GetValue())
			}
			name := family.GetName() + "{" + strings.Join(labels, ",") + "}"
			switch {
			case metric.GetGauge() != nil:
				all[name] = metric.GetGauge().GetValue()
			case metric.GetCounter() != nil:
				all[name] = metric.GetCounter().GetValue()
			case metric.GetHistogram() != nil:
				all[name] = float64(metric.GetHistogram().GetSampleCount())
			}
		}
	}
	return all
}

// TestANodeSaysHowItIs: a node on its own leads its metadata group of one,
// writes the newest schema, and has its drives online - by their generated
// IDs, with the room on them, and never by where they are. When the node
// has stopped its gauges say nothing any more.
func TestANodeSaysHowItIs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	started := mustStart(t, nodeConfig(dir))
	n := started.node
	tel := n.telemetry

	got := samples(t, tel)
	for name, want := range map[string]float64{
		"cactusfs_raft_is_leader{}":            1,
		"cactusfs_raft_peers{}":                1,
		"cactusfs_metadata_schema_level{}":     raftmeta.MetadataVersion,
		"cactusfs_metadata_schema_supported{}": raftmeta.MetadataVersion,
	} {
		if value, ok := got[name]; !ok || value != want {
			t.Errorf("%s = %v (exposed: %v), want %v", name, value, ok, want)
		}
	}
	for _, name := range []string{"cactusfs_raft_term{}", "cactusfs_raft_applied_index{}", "cactusfs_raft_commit_index{}", "cactusfs_metadata_state_schema{}"} {
		if got[name] < 1 {
			t.Errorf("%s = %v, want at least 1", name, got[name])
		}
	}
	if got["cactusfs_raft_applied_index{}"] > got["cactusfs_raft_commit_index{}"] {
		t.Errorf("applied index %v is beyond the commit index %v", got["cactusfs_raft_applied_index{}"], got["cactusfs_raft_commit_index{}"])
	}
	if len(n.drives) != 2 {
		t.Fatalf("%d drives", len(n.drives))
	}
	for _, drive := range n.drives {
		id := drive.DriveID()
		total, free := got["cactusfs_drive_total_bytes{drive="+id+"}"], got["cactusfs_drive_free_bytes{drive="+id+"}"]
		if got["cactusfs_drive_online{drive="+id+"}"] != 1 || total <= 0 || free < 0 || free > total {
			t.Errorf("drive %s: online %v, %v bytes free of %v", id, got["cactusfs_drive_online{drive="+id+"}"], free, total)
		}
	}
	// A node on its own has no certificate for other nodes; and nothing
	// names a directory: the drive label is one of the generated IDs, whole,
	// and no sample holds the path of the node's data.
	ids := map[string]bool{}
	for _, drive := range n.drives {
		ids[drive.DriveID()] = true
	}
	driveSeries := 0
	for name := range got {
		if strings.HasPrefix(name, "cactusfs_node_certificate_not_after_seconds") || strings.HasPrefix(name, "cactusfs_tls_") {
			t.Errorf("%s is exposed by a node that has no node certificate", name)
		}
		if strings.Contains(name, dir) {
			t.Errorf("%s names where a drive is", name)
		}
		if !strings.HasPrefix(name, "cactusfs_drive_") {
			continue
		}
		driveSeries++
		_, labels, _ := strings.Cut(strings.TrimSuffix(name, "}"), "{")
		label, id, _ := strings.Cut(labels, "=")
		if label != "drive" || !ids[id] || strings.ContainsAny(id, `/\`) {
			t.Errorf("%s: its label is not the generated ID of one of the node's drives", name)
		}
	}
	// Online, free and total, of two drives.
	if driveSeries != 6 {
		t.Errorf("%d series of drives, want 6", driveSeries)
	}

	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	for name := range samples(t, tel) {
		if strings.HasPrefix(name, "cactusfs_raft_") || strings.HasPrefix(name, "cactusfs_drive_") || strings.HasPrefix(name, "cactusfs_metadata_") {
			t.Errorf("%s is exposed by a node that has stopped", name)
		}
	}
}

// tracedCluster is a cluster of four nodes, each recording its spans.
func tracedCluster(t *testing.T) (*testCluster, map[string]*tracetest.SpanRecorder) {
	t.Helper()
	c := newTestCluster(t, fourNodes())
	recorders := map[string]*tracetest.SpanRecorder{}
	for _, id := range c.all() {
		recorders[id] = tracetest.NewSpanRecorder()
		tel, err := telemetry.New(t.Context(), telemetry.Config{Version: buildinfo.Get().Tag(), SpanProcessor: recorders[id]})
		if err != nil {
			t.Fatal(err)
		}
		c.telemetry[id] = tel
	}
	c.start(c.all()...)
	return c, recorders
}

// spansOf returns the spans a node ended in a trace, by name.
func spansOf(recorder *tracetest.SpanRecorder, trace oteltrace.TraceID) map[string][]sdktrace.ReadOnlySpan {
	byName := map[string][]sdktrace.ReadOnlySpan{}
	for _, span := range recorder.Ended() {
		if span.SpanContext().TraceID() == trace {
			byName[span.Name()] = append(byName[span.Name()], span)
		}
	}
	return byName
}

// TestAPutThroughTheClusterIsOneTrace: an object written through the
// gateway of a cluster of four is one trace. Every other node holds shards
// of it, and each one's span of the write is a child of the gateway's span
// of the call that asked for it; so is the leader's span of the commit. The
// nodes count the calls on both sides. Nothing any node recorded - no
// attribute of a span, no label of a metric - names the bucket, the key,
// the caller or what was written.
func TestAPutThroughTheClusterIsOneTrace(t *testing.T) {
	t.Parallel()
	c, recorders := tracedCluster(t)
	gateway := c.node("n4")
	caller := iam.Principal{AccessKeyID: "SENTINELCALLERKEY", Root: true}
	const bucketName, key = "bucket-sentinel", "folder-sentinel/key-sentinel.txt"
	body := bytes.Repeat([]byte("content-sentinel "), 4096)

	if err := gateway.buckets.Create(t.Context(), caller, bucketName); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The PUT, inside the span of the S3 request as the S3 handler runs it.
	put := gateway.telemetry.TraceS3(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, err := gateway.objects.Put(r.Context(), caller, app.PutInput{
			Bucket: bucketName, Key: key, Body: bytes.NewReader(body), Size: int64(len(body)),
			Verify: func() (string, error) { return "etag", nil },
		})
		if err != nil {
			t.Errorf("Put: %v", err)
		}
		gateway.telemetry.ObserveS3(r.Context(), s3api.Observation{Operation: "PutObject", Status: http.StatusOK})
	}))
	put.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/"+bucketName+"/"+key, nil))

	var request sdktrace.ReadOnlySpan
	for _, span := range recorders["n4"].Ended() {
		if span.Name() == "s3.PutObject" {
			request = span
		}
	}
	if request == nil {
		t.Fatal("the gateway recorded no span of the request")
	}
	trace := request.SpanContext().TraceID()

	// The gateway's spans of its calls, by their IDs.
	calls := map[oteltrace.SpanID]string{}
	for name, spans := range spansOf(recorders["n4"], trace) {
		if strings.HasPrefix(name, "cactusfs.noderpc/") {
			for _, span := range spans {
				if span.SpanKind() == oteltrace.SpanKindClient {
					calls[span.SpanContext().SpanID()] = name
				}
			}
		}
	}
	if len(calls) == 0 {
		t.Fatal("the gateway recorded no call to another node in the trace of the request")
	}

	// Two shards on every node: the three others each served two writes.
	committed := false
	for _, id := range []string{"n1", "n2", "n3"} {
		deadline := time.Now().Add(10 * time.Second)
		var spans map[string][]sdktrace.ReadOnlySpan
		for {
			spans = spansOf(recorders[id], trace)
			if len(spans["cactusfs.noderpc/WriteShard"]) >= 2 || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		writes := spans["cactusfs.noderpc/WriteShard"]
		if len(writes) != 2 {
			t.Errorf("node %s served %d shard writes in the trace of the request, want 2", id, len(writes))
		}
		served := map[oteltrace.SpanID]bool{}
		for _, span := range writes {
			served[span.SpanContext().SpanID()] = true
			if span.SpanKind() != oteltrace.SpanKindServer || calls[span.Parent().SpanID()] != "cactusfs.noderpc/WriteShard" || !span.Parent().IsRemote() {
				t.Errorf("node %s: the parent of its span of a shard write is not the gateway's span of that call", id)
			}
		}
		// What the node did with its drive belongs to the call it served.
		if len(spans["shards.Commit"]) != 2 {
			t.Errorf("node %s committed %d shards in the trace, want 2", id, len(spans["shards.Commit"]))
		}
		for _, span := range spans["shards.Commit"] {
			if !served[span.Parent().SpanID()] {
				t.Errorf("node %s: a commit of a shard is not a child of the call that asked for it", id)
			}
		}
		// The version is committed through the leader, in the same trace.
		for _, span := range spans["cactusfs.noderpc/Propose"] {
			committed = committed || calls[span.Parent().SpanID()] == "cactusfs.noderpc/Propose"
		}

		got := samples(t, c.telemetry[id])
		if got["cactusfs_noderpc_requests_total{code=OK,method=WriteShard,side=server}"] < 2 {
			t.Errorf("node %s counts %v shard writes it served", id, got["cactusfs_noderpc_requests_total{code=OK,method=WriteShard,side=server}"])
		}
	}
	if !committed {
		t.Error("no node's span of the commit of the version is a child of the gateway's call")
	}
	// The gateway learns that a stream has ended a moment after it ended it.
	got := samples(t, gateway.telemetry)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) &&
		got["cactusfs_noderpc_request_duration_seconds{method=WriteShard,side=client}"] < 6; got = samples(t, gateway.telemetry) {
		time.Sleep(10 * time.Millisecond)
	}
	if got["cactusfs_noderpc_requests_total{code=OK,method=WriteShard,side=client}"] < 6 || got["cactusfs_noderpc_request_duration_seconds{method=WriteShard,side=client}"] < 6 {
		t.Errorf("the gateway counts %v shard writes on other nodes, timed %v",
			got["cactusfs_noderpc_requests_total{code=OK,method=WriteShard,side=client}"],
			got["cactusfs_noderpc_request_duration_seconds{method=WriteShard,side=client}"])
	}

	// The state of the cluster, as its nodes tell it: one leader, three
	// voters, a certificate that ends in the future, drives online.
	leaders := 0.0
	for _, id := range c.all() {
		got := samples(t, c.telemetry[id])
		leaders += got["cactusfs_raft_is_leader{}"]
		if got["cactusfs_raft_peers{}"] != 3 {
			t.Errorf("node %s knows %v voters, want 3", id, got["cactusfs_raft_peers{}"])
		}
		if end := got["cactusfs_node_certificate_not_after_seconds{}"]; end <= float64(time.Now().Unix()) {
			t.Errorf("node %s: its certificate ends at %v, which is not in the future", id, end)
		}
		if got["cactusfs_metadata_schema_level{}"] != raftmeta.MetadataVersion {
			t.Errorf("node %s: the cluster writes schema %v", id, got["cactusfs_metadata_schema_level{}"])
		}
		for _, drive := range c.node(id).drives {
			if got["cactusfs_drive_online{drive="+drive.DriveID()+"}"] != 1 {
				t.Errorf("node %s: drive %s is not online", id, drive.DriveID())
			}
		}
		// No handshake was refused for its certificate among members.
		if got["cactusfs_node_handshakes_refused_total{reason=certificate}"] != 0 {
			t.Errorf("node %s refused %v handshakes of members", id, got["cactusfs_node_handshakes_refused_total{reason=certificate}"])
		}
	}
	if leaders != 1 {
		t.Errorf("%v nodes say they lead the metadata group", leaders)
	}
	// With drives on other nodes in use, the gateway holds no port that is
	// not decorated.
	wantWired(t, gateway, wiredKinds...)

	// Nothing recorded on any node names what was written, where, or by
	// whom - nor the directory of a drive.
	checked := 0
	for _, id := range c.all() {
		var all []string
		for name := range samples(t, c.telemetry[id]) {
			all = append(all, name)
		}
		for _, span := range recorders[id].Ended() {
			all = append(all, span.Name(), span.Status().Description)
			for _, attr := range span.Attributes() {
				all = append(all, string(attr.Key), attr.Value.String())
			}
			for _, event := range span.Events() {
				all = append(all, event.Name)
				for _, attr := range event.Attributes {
					all = append(all, string(attr.Key), attr.Value.String())
				}
			}
		}
		checked += len(all)
		for _, text := range all {
			for _, word := range []string{"sentinel", "SENTINEL", c.dir} {
				if strings.Contains(text, word) {
					t.Errorf("node %s recorded %q, which contains %q", id, text, word)
				}
			}
		}
	}
	if checked < 100 {
		t.Fatalf("only %d recorded things were checked", checked)
	}
}

// TestReadinessGauge: the gauge reads what /readyz reads.
func TestReadinessGauge(t *testing.T) {
	t.Parallel()
	gate := health.NewGate()
	metadata := &lateCheck{name: "metadata"}
	tel := newTelemetry(t)
	tel.ObserveReadiness(readyGauge{health: app.NewHealthService(gate, metadata)})

	// Starting: the metadata store is not there yet.
	if got, ok := samples(t, tel)["cactusfs_ready{}"]; !ok || got != 0 {
		t.Errorf("cactusfs_ready = %v (exposed: %v) while the node starts, want 0", got, ok)
	}
	metadata.set(gate)
	if got := samples(t, tel)["cactusfs_ready{}"]; got != 1 {
		t.Errorf("cactusfs_ready = %v once the node has started, want 1", got)
	}
	// Draining.
	gate.Close()
	if got, ok := samples(t, tel)["cactusfs_ready{}"]; !ok || got != 0 {
		t.Errorf("cactusfs_ready = %v (exposed: %v) while the node drains, want 0", got, ok)
	}
}

// endingCA is a CA of a test that issues certificates that end when the
// test says: the development CA of nodetls ends all of its own with itself.
type endingCA struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pem         []byte
	serial      int64
}

func newEndingCA(t *testing.T) *endingCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &endingCA{certificate: certificate, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), serial: 1}
}

// issue writes a certificate that names name, may be used as usages say and
// ends at notAfter, and its key, to the two files.
func (ca *endingCA) issue(t *testing.T, certFile, keyFile, name string, notAfter time.Time, usages ...x509.ExtKeyUsage) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca.serial++
	template := &x509.Certificate{
		SerialNumber: big.NewInt(ca.serial), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	// The key first, as a replacement is written: the pair is whole when
	// the certificate is.
	writeTestFile(t, keyFile, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})))
	writeTestFile(t, certFile, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
}

// TestCertificateGaugesFollowARotation: the gauges of the node's certificate
// and of the one clients are shown say when the certificate in use ends -
// after a replacement, the new one. A pair that is refused changes no end,
// is pending for as long as it is on disk and is counted once, by its
// reason; a good pair after it is pending no more, and the count stays.
func TestCertificateGaugesFollowARotation(t *testing.T) {
	t.Parallel()
	ca, dir := newEndingCA(t), t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	writeTestFile(t, caFile, string(ca.pem))
	nodeCert, nodeKey := filepath.Join(dir, "node.pem"), filepath.Join(dir, "node.key")
	servedCert, servedKey := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	node, server := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	// Whole seconds: the gauges are in seconds.
	now := time.Now().Truncate(time.Second)
	ends := map[string]time.Time{"node": now.Add(2 * time.Hour), "s3": now.Add(3 * time.Hour)}
	ca.issue(t, nodeCert, nodeKey, "n1", ends["node"], node...)
	ca.issue(t, servedCert, servedKey, "s3.test", ends["s3"], server...)

	identity, err := nodetls.Load(nodeCert, nodeKey, caFile)
	if err != nil {
		t.Fatalf("nodetls.Load: %v", err)
	}
	// Never read by a loop: the test says when the files are looked at.
	ofNode, err := identity.Reloader(nodetls.ReloadConfig{CertFile: nodeCert, KeyFile: nodeKey, Clock: systemClock{}})
	if err != nil {
		t.Fatalf("Reloader: %v", err)
	}
	_, ofServer, err := loadTLSConfig(t.Context(), serverConfig{TLSCert: servedCert, TLSKey: servedKey}, nil)
	if err != nil {
		t.Fatalf("loadTLSConfig: %v", err)
	}
	// Attached as openCluster and runServer attach them.
	tel := newTelemetry(t)
	tel.ObserveNodeCertificate(certificateGauge{reloader: ofNode})
	tel.ObserveTLS(telemetry.TLSUseNode, tlsGauges{reloader: ofNode})
	tel.ObserveTLS(telemetry.TLSUseS3, tlsGauges{reloader: ofServer})

	pending := map[string]float64{"node": 0, "s3": 0}
	refused := map[string]float64{}
	check := func(when string) {
		t.Helper()
		got := samples(t, tel)
		want := map[string]float64{"cactusfs_node_certificate_not_after_seconds{}": float64(ends["node"].Unix())}
		for _, use := range []string{"node", "s3"} {
			want["cactusfs_tls_certificate_not_after_seconds{use="+use+"}"] = float64(ends[use].Unix())
			want["cactusfs_tls_reload_pending{use="+use+"}"] = pending[use]
			for _, reason := range nodetls.ReloadRefusals() {
				name := "cactusfs_tls_reload_refused_total{reason=" + string(reason) + ",use=" + use + "}"
				want[name] = refused[name]
			}
		}
		for name, value := range want {
			if sample, ok := got[name]; !ok || sample != value {
				t.Errorf("%s: %s = %v (exposed: %v), want %v", when, name, sample, ok, value)
			}
		}
	}
	check("at the start")

	steps := []struct {
		name string
		// use is the certificate whose files change, and write changes them.
		use   string
		write func()
		// replaced says that the pair is put to use; ends is then its end.
		replaced bool
		ends     time.Time
		// reason is why it is refused when it is not.
		reason nodetls.ReloadRefusal
	}{
		{
			name: "the node's certificate is renewed", use: "node", replaced: true, ends: now.Add(5 * time.Hour),
			write: func() { ca.issue(t, nodeCert, nodeKey, "n1", now.Add(5*time.Hour), node...) },
		},
		{
			name: "the certificate for clients is renewed", use: "s3", replaced: true, ends: now.Add(7 * time.Hour),
			write: func() { ca.issue(t, servedCert, servedKey, "s3.test", now.Add(7*time.Hour), server...) },
		},
		{
			name: "a certificate that names another node", use: "node", reason: nodetls.RefusedIdentity,
			write: func() { ca.issue(t, nodeCert, nodeKey, "n2", now.Add(9*time.Hour), node...) },
		},
		{name: "the same files, looked at again", use: "node", reason: nodetls.RefusedIdentity, write: func() {}},
		{
			name: "a certificate no server may show", use: "s3", reason: nodetls.RefusedUsage,
			write: func() {
				ca.issue(t, servedCert, servedKey, "s3.test", now.Add(9*time.Hour), x509.ExtKeyUsageClientAuth)
			},
		},
		{
			name: "the node's certificate is put right", use: "node", replaced: true, ends: now.Add(6 * time.Hour),
			write: func() { ca.issue(t, nodeCert, nodeKey, "n1", now.Add(6*time.Hour), node...) },
		},
		{
			name: "a renewal that ends sooner is the one in use all the same", use: "s3", replaced: true, ends: now.Add(4 * time.Hour),
			write: func() { ca.issue(t, servedCert, servedKey, "s3.test", now.Add(4*time.Hour), server...) },
		},
	}
	counted := map[string]bool{}
	for _, step := range steps {
		step.write()
		reloader := ofNode
		if step.use == "s3" {
			reloader = ofServer
		}
		if got := reloader.Poll(t.Context()); got != step.replaced {
			t.Fatalf("%s: Poll = %t, want %t", step.name, got, step.replaced)
		}
		if step.replaced {
			ends[step.use], pending[step.use] = step.ends, 0
		} else {
			pending[step.use] = 1
			// Counted once for one content of the files.
			name := "cactusfs_tls_reload_refused_total{reason=" + string(step.reason) + ",use=" + step.use + "}"
			if !counted[name] {
				counted[name] = true
				refused[name]++
			}
		}
		check(step.name)
	}
	if len(counted) != 2 {
		t.Fatalf("%d kinds of refusal were tried, want 2", len(counted))
	}

	// Detached, as a node that stops detaches them.
	tel.ObserveNodeCertificate(nil)
	tel.ObserveTLS(telemetry.TLSUseNode, nil)
	for name := range samples(t, tel) {
		if strings.HasPrefix(name, "cactusfs_node_certificate_") || strings.Contains(name, "use=node") {
			t.Errorf("%s is exposed when the node has stopped", name)
		}
	}
}

// TestReasonsOfTheMetricsAreThoseOfNodeTLS: the reason labels of the metrics
// take the values the telemetry adapter defines, and nodetls names the
// reasons; the two adapters do not know each other, so this holds them to
// each other. Every reason a certificate on disk is refused for is a label
// value, and no label value is without its reason; every reason a handshake
// is refused for is counted under one of the four the metric has.
func TestReasonsOfTheMetricsAreThoseOfNodeTLS(t *testing.T) {
	t.Parallel()
	var reload []string
	for _, reason := range nodetls.ReloadRefusals() {
		reload = append(reload, string(reason))
	}
	labelled := telemetry.TLSReloadReasons()
	slices.Sort(reload)
	slices.Sort(labelled)
	if !slices.Equal(reload, labelled) {
		t.Errorf("nodetls refuses a certificate on disk for %v; the metric has the reasons %v", reload, labelled)
	}

	tests := []struct {
		refusal nodetls.PeerRefusal
		want    string
	}{
		{refusal: nodetls.PeerRefusedNoCertificate, want: telemetry.HandshakeCertificate},
		{refusal: nodetls.PeerRefusedUntrusted, want: telemetry.HandshakeCertificate},
		{refusal: nodetls.PeerRefusedIdentity, want: telemetry.HandshakeCertificate},
		{refusal: nodetls.PeerRefusedNotAdmitted, want: telemetry.HandshakeNotAdmitted},
		{refusal: nodetls.PeerRefusedTimeout, want: telemetry.HandshakeTimeout},
		{refusal: nodetls.PeerRefusedHandshake, want: telemetry.HandshakeOther},
		{refusal: "a reason nodetls does not have yet", want: telemetry.HandshakeOther},
	}
	sorted := map[nodetls.PeerRefusal]bool{}
	for _, tc := range tests {
		t.Run(string(tc.refusal), func(t *testing.T) {
			t.Parallel()
			if got := handshakeReason(tc.refusal); got != tc.want {
				t.Errorf("handshakeReason(%q) = %q, want %q", tc.refusal, got, tc.want)
			}
		})
		sorted[tc.refusal] = true
	}
	for _, refusal := range nodetls.PeerRefusals() {
		if !sorted[refusal] {
			t.Errorf("this test does not say what %q is counted as", refusal)
		}
	}
}
