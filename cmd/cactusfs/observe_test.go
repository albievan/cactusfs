package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/albievan/cactusfs/internal/adapters/health"
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
		if strings.HasPrefix(name, "cactusfs_node_certificate_not_after_seconds") {
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
