package telemetry_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/adapters/s3api"
	"github.com/albievan/cactusfs/internal/adapters/telemetry"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// The observability pack of deploy/observability - alerting rules, their
// tests and dashboards - and the copy of the rules the Helm chart carries.
// These tests hold the pack to the metrics the binary has: a rule or a panel
// on a metric that does not exist never fires and never draws, and says
// nothing is wrong. Run by `go test` and by `make observability-check`.
const (
	packRules      = "deploy/observability/rules/cactusfs.rules.yaml"
	packRuleTests  = "deploy/observability/rules/cactusfs.rules.test.yaml"
	packRulesGlob  = "deploy/observability/rules/*.yaml"
	packDashboards = "deploy/observability/dashboards/*.json"
	chartRules     = "deploy/helm/cactusfs/files/cactusfs.rules.yaml"
	packDatasource = "${DS_PROMETHEUS}"
)

var (
	// A metric of the binary, wherever it is written: in an expression, in
	// a series of a rule test. The registry's Go collector is checked as
	// well. Names of recording rules have colons (cactusfs:...) and are not
	// matched.
	packMetric = regexp.MustCompile(`\b(?:cactusfs|go)_[a-z0-9_]+`)
	descName   = regexp.MustCompile(`fqName: "([^"]+)"`)
	alertName  = regexp.MustCompile(`(?m)^\s*- alert: (\S+)\s*$`)
	severity   = regexp.MustCompile(`(?m)^\s*severity: (critical|warning|info)\s*$`)
	// The panels of Grafana itself; anything else is a plugin to install.
	corePanels = map[string]bool{"row": true, "timeseries": true, "stat": true, "table": true, "text": true}
)

// repository is the tree this package is in. The pack is read through it, so
// that no path is made of anything but constants.
func repository() fs.FS {
	return os.DirFS("../../..")
}

// registeredSeries is every series name the registry of a node exposes: the
// name of a counter or a gauge, and for a histogram or a summary its
// _bucket, _sum and _count. A collector with labels shows nothing until it
// is observed, so every observation the binary makes is made once here; a
// metric that is registered and does not show fails the test, which is how a
// new metric is added to the list.
func registeredSeries(t *testing.T) map[string]bool {
	t.Helper()
	return seriesOf(t, observedTelemetry(t))
}

// observedTelemetry is the telemetry of a node on which every observation
// the binary makes has been made once.
func observedTelemetry(t *testing.T) *telemetry.Telemetry {
	t.Helper()
	ctx := t.Context()
	tel := newTelemetry(t, nil)

	tel.ObserveS3(ctx, s3api.Observation{Operation: "GetObject", Status: 200, Duration: time.Millisecond, BytesIn: 1, BytesOut: 1})
	tel.ObserveAdmin(ctx, adminapi.Observation{Operation: "GET /api/v1/buckets", Status: 200, Duration: time.Millisecond})
	tel.ObserveHeal(app.HealResult{Shards: 6})
	tel.ObserveScrub(app.ScrubStats{Objects: 1, Bytes: 1})
	tel.ObserveGC(app.GCStats{Shards: 1, Removed: 1})
	tel.ObserveLifecycle(app.LifecycleStats{Expired: 1})
	tel.ObserveAccessDropped(1)
	tel.ObserveEvent(ctx, ports.Event{Type: "cactusfs.bucket.created.v1"})
	kms, err := porttest.NewKMS("test:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tel.InstrumentKMS(kms).GenerateDataKey(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// A call between nodes, as the node that makes it and as the node that
	// answers it.
	rpc := tel.NodeRPC([]string{methodRead})
	if err := rpc.UnaryClient()(ctx, methodRead, nil, nil, nil, (&answering{}).invoke); err != nil {
		t.Fatal(err)
	}
	tel.ObserveHandshakeRefused(telemetry.HandshakeCertificate)
	// How the node is: read from its sources when it is scraped.
	tel.ObserveRaft(fixedRaft{known: true, state: telemetry.RaftState{
		Leader: true, Term: 1, AppliedIndex: 1, CommitIndex: 1, Voters: 1, SchemaLevel: 1, SchemaSupported: 1, StateSchema: 1,
	}})
	tel.ObserveDrives(fixedDrives{{ID: "drive-a", Online: true, FreeBytes: 1, TotalBytes: 2}})
	tel.ObserveNodeCertificate(fixedCertificate{notAfter: time.Unix(1, 0), present: true})
	tel.ObserveReadiness(readyWhen(true))
	return tel
}

func seriesOf(t *testing.T, tel *telemetry.Telemetry) map[string]bool {
	t.Helper()
	families, err := tel.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	gathered, series := map[string]bool{}, map[string]bool{}
	for _, family := range families {
		name := family.GetName()
		gathered[name] = true
		switch family.GetType().String() {
		case "HISTOGRAM":
			series[name+"_bucket"], series[name+"_sum"], series[name+"_count"] = true, true, true
		case "SUMMARY":
			series[name], series[name+"_sum"], series[name+"_count"] = true, true, true
		default:
			series[name] = true
		}
	}

	// What is registered, whether or not it has been observed. The process
	// and Go collectors describe what a platform may not have; what they
	// have here is in the gathering.
	collector, ok := tel.Gatherer().(prometheus.Collector)
	if !ok {
		t.Fatal("the registry does not describe its metrics")
	}
	descs := make(chan *prometheus.Desc, 1024)
	collector.Describe(descs)
	close(descs)
	described := 0
	for desc := range descs {
		match := descName.FindStringSubmatch(desc.String())
		if match == nil {
			t.Fatalf("no name in the description %s", desc)
		}
		name := match[1]
		if !strings.HasPrefix(name, "cactusfs_") || strings.HasPrefix(name, "cactusfs_process_") {
			continue
		}
		described++
		if !gathered[name] {
			t.Errorf("%s is registered and this test does not observe it: add the observation to registeredSeries", name)
		}
	}
	if described == 0 {
		t.Fatal("the registry describes no cactusfs_ metric")
	}
	return series
}

func readPack(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(repository(), name)
	if err != nil {
		t.Fatalf("the observability pack is part of the tree: %v", err)
	}
	return string(data)
}

func globPack(t *testing.T, pattern string) []string {
	t.Helper()
	names, err := fs.Glob(repository(), pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatalf("no file matches %s", pattern)
	}
	return names
}

// withoutComments drops the lines of a YAML file that are comments: they may
// name metrics that are still to come.
func withoutComments(text string) string {
	var kept []string
	for line := range strings.SplitSeq(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// TestObservabilityPackNamesOnlyRegisteredMetrics reads every rule file,
// rule test and dashboard and fails on a metric the registry does not have.
// The repository has no YAML parser among its direct dependencies and gains
// none for this: the name of a metric is recognisable in the raw text.
func TestObservabilityPackNamesOnlyRegisteredMetrics(t *testing.T) {
	t.Parallel()
	series := registeredSeries(t)

	files := append(globPack(t, packRulesGlob), globPack(t, packDashboards)...)
	files = append(files, chartRules)
	for _, name := range files {
		text := readPack(t, name)
		if path.Ext(name) == ".yaml" {
			text = withoutComments(text)
		}
		names := packMetric.FindAllString(text, -1)
		if len(names) == 0 {
			t.Errorf("%s names no metric of the binary", name)
		}
		unknown := map[string]bool{}
		for _, metric := range names {
			if !series[metric] {
				unknown[metric] = true
			}
		}
		for _, metric := range sortedKeys(unknown) {
			t.Errorf("%s names %s, which the registry does not have", name, metric)
		}
	}
}

// TestRegisteredSeriesKnowsHistograms holds the check itself to account: it
// must know a histogram by its series and refuse a name that is close.
func TestRegisteredSeriesKnowsHistograms(t *testing.T) {
	t.Parallel()
	series := registeredSeries(t)
	tests := []struct {
		name string
		want bool
	}{
		{"cactusfs_build_info", true},
		{"cactusfs_s3_requests_total", true},
		{"cactusfs_s3_request_duration_seconds_bucket", true},
		{"cactusfs_s3_request_duration_seconds_sum", true},
		{"cactusfs_s3_request_duration_seconds_count", true},
		{"cactusfs_port_call_duration_seconds_bucket", true},
		{"cactusfs_object_access_dropped_total", true},
		{"cactusfs_process_start_time_seconds", true},
		{"go_goroutines", true},
		// A histogram has no series of its own name.
		{"cactusfs_s3_request_duration_seconds", false},
		{"cactusfs_s3_requests", false},
		{"cactusfs_noderpc_requests_total", true},
		{"cactusfs_noderpc_request_duration_seconds_bucket", true},
		{"cactusfs_raft_is_leader", true},
		{"cactusfs_drive_online", true},
		{"cactusfs_ready", true},
		{"cactusfs_noderpc_request_duration_seconds", false},
		{"cactusfs_raft_leader", false},
		{"cactusfs_drive_offline", false},
	}
	for _, tt := range tests {
		if series[tt.name] != tt.want {
			t.Errorf("series %s: registered is %v, want %v", tt.name, series[tt.name], tt.want)
		}
	}
}

// TestObservabilityPackAlertsAreCompleteAndTested: every alert says how long
// it waits, how severe it is and what it means, and the rule tests name it
// at least twice - once firing, once not.
func TestObservabilityPackAlertsAreCompleteAndTested(t *testing.T) {
	t.Parallel()
	rules := withoutComments(readPack(t, packRules))
	ruleTests := readPack(t, packRuleTests)

	blocks := strings.Split(rules, "- alert: ")
	if len(blocks) < 2 {
		t.Fatalf("%s has no alert", packRules)
	}
	if got := len(alertName.FindAllString(rules, -1)); got != len(blocks)-1 {
		t.Fatalf("%s: %d alerts are named on a line of their own, of %d", packRules, got, len(blocks)-1)
	}
	seen := map[string]bool{}
	for _, block := range blocks[1:] {
		name, _, _ := strings.Cut(block, "\n")
		name = strings.TrimSpace(name)
		if seen[name] {
			t.Errorf("alert %s is defined twice", name)
		}
		seen[name] = true
		if !strings.HasPrefix(name, "CactusFS") {
			t.Errorf("alert %s: the name of an alert begins with CactusFS", name)
		}
		for _, field := range []string{"\n        expr: ", "\n        for: ", "\n          summary: ", "\n          description: "} {
			if !strings.Contains(block, field) {
				t.Errorf("alert %s has no %q", name, strings.TrimSpace(field))
			}
		}
		if !severity.MatchString(block) {
			t.Errorf("alert %s has no severity of critical, warning or info", name)
		}
		if got := strings.Count(ruleTests, "alertname: "+name+"\n"); got < 2 {
			t.Errorf("alert %s is tested %d times in %s: once firing and once not is the least", name, got, packRuleTests)
		}
	}
}

// TestChartCarriesTheSameRules: the chart can only read files of its own
// directory, so it has a copy of the rules; the copy is the same file.
func TestChartCarriesTheSameRules(t *testing.T) {
	t.Parallel()
	if !bytes.Equal([]byte(readPack(t, packRules)), []byte(readPack(t, chartRules))) {
		t.Errorf("%s and %s differ: the second is a copy of the first", packRules, chartRules)
	}
}

// TestObservabilityPackDashboards: a dashboard is JSON that Grafana can load
// with nothing installed and nothing fetched - panels of Grafana itself, the
// data source a variable, no address of anything.
func TestObservabilityPackDashboards(t *testing.T) {
	t.Parallel()
	uids := map[string]string{}
	for _, name := range globPack(t, packDashboards) {
		text := readPack(t, name)
		for _, remote := range []string{"http:", "https:", "//"} {
			if strings.Contains(strings.ToLower(text), remote) {
				t.Errorf("%s contains %q: a dashboard refers to nothing outside itself", name, remote)
			}
		}
		var dashboard struct {
			UID           string          `json:"uid"`
			Title         string          `json:"title"`
			SchemaVersion int             `json:"schemaVersion"`
			Panels        []dashboardPane `json:"panels"`
		}
		decoder := json.NewDecoder(strings.NewReader(text))
		if err := decoder.Decode(&dashboard); err != nil {
			t.Errorf("%s is not JSON: %v", name, err)
			continue
		}
		if dashboard.UID == "" || dashboard.Title == "" || dashboard.SchemaVersion == 0 {
			t.Errorf("%s: a dashboard has a uid, a title and a schemaVersion", name)
		}
		if other, ok := uids[dashboard.UID]; ok {
			t.Errorf("%s and %s have the uid %s", name, other, dashboard.UID)
		}
		uids[dashboard.UID] = name

		queries := 0
		var check func(panels []dashboardPane)
		check = func(panels []dashboardPane) {
			for _, panel := range panels {
				if !corePanels[panel.Type] {
					t.Errorf("%s: panel %q is of type %q, which is not a panel of Grafana itself", name, panel.Title, panel.Type)
				}
				check(panel.Panels)
				if panel.Type == "row" || panel.Type == "text" {
					continue
				}
				if len(panel.Targets) == 0 {
					t.Errorf("%s: panel %q queries nothing", name, panel.Title)
				}
				if panel.Datasource.UID != packDatasource {
					t.Errorf("%s: panel %q reads the data source %q, not %s", name, panel.Title, panel.Datasource.UID, packDatasource)
				}
				for _, target := range panel.Targets {
					queries++
					if strings.TrimSpace(target.Expr) == "" {
						t.Errorf("%s: panel %q has a query with no expression", name, panel.Title)
					}
					if target.Datasource.UID != packDatasource {
						t.Errorf("%s: a query of panel %q reads the data source %q, not %s", name, panel.Title, target.Datasource.UID, packDatasource)
					}
				}
			}
		}
		check(dashboard.Panels)
		if queries == 0 {
			t.Errorf("%s has no query", name)
		}
	}
}

type dashboardSource struct {
	UID string `json:"uid"`
}

type dashboardPane struct {
	Type       string          `json:"type"`
	Title      string          `json:"title"`
	Datasource dashboardSource `json:"datasource"`
	Panels     []dashboardPane `json:"panels"`
	Targets    []struct {
		Expr       string          `json:"expr"`
		Datasource dashboardSource `json:"datasource"`
	} `json:"targets"`
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// The list of metrics in the architecture document, between two markers:
// one row of a table a metric.
const (
	architecture = "docs/ARCHITECTURE.md"
	metricsBegin = "<!-- metrics:begin -->"
	metricsEnd   = "<!-- metrics:end -->"
)

var (
	documentedRow   = regexp.MustCompile("^\\| `(cactusfs_[a-z0-9_]+)` \\| (gauge|counter|histogram) \\| ([^|]+) \\| [^|]+ \\|$")
	documentedLabel = regexp.MustCompile("`([a-z_]+)`")
)

// TestArchitectureListsTheMetrics holds the list of metrics in
// docs/ARCHITECTURE.md and the registry to each other. A metric that is
// registered and not listed fails it; so does one that is listed and not
// registered, or listed with another type or other labels. That is how a
// new metric gets into the document, and a removed one out of it.
func TestArchitectureListsTheMetrics(t *testing.T) {
	t.Parallel()
	text := readPack(t, architecture)
	_, rest, found := strings.Cut(text, metricsBegin)
	table, _, closed := strings.Cut(rest, metricsEnd)
	if !found || !closed {
		t.Fatalf("%s has no list of metrics between %s and %s", architecture, metricsBegin, metricsEnd)
	}
	documented := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(table), "\n") {
		if strings.HasPrefix(line, "| Metric ") || strings.HasPrefix(line, "|---") {
			continue
		}
		row := documentedRow.FindStringSubmatch(line)
		if row == nil {
			t.Errorf("%s: this row of the list of metrics cannot be read: %s", architecture, line)
			continue
		}
		var labels []string
		for _, label := range documentedLabel.FindAllStringSubmatch(row[3], -1) {
			labels = append(labels, label[1])
		}
		if len(labels) == 0 && strings.TrimSpace(row[3]) != "-" {
			t.Errorf("%s: %s: the labels are %q; a metric without labels has a dash", architecture, row[1], row[3])
		}
		sort.Strings(labels)
		if _, twice := documented[row[1]]; twice {
			t.Errorf("%s lists %s twice", architecture, row[1])
		}
		documented[row[1]] = row[2] + " " + strings.Join(labels, ",")
	}

	families, err := observedTelemetry(t).Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	registered := map[string]string{}
	for _, family := range families {
		name := family.GetName()
		if !strings.HasPrefix(name, "cactusfs_") || strings.HasPrefix(name, "cactusfs_process_") {
			continue
		}
		labels := map[string]bool{}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = true
			}
		}
		registered[name] = strings.ToLower(family.GetType().String()) + " " + strings.Join(sortedKeys(labels), ",")
	}
	if len(registered) < 30 {
		t.Fatalf("the registry has %d metrics of the binary; the test does not see them all", len(registered))
	}
	for name, want := range registered {
		switch got, ok := documented[name]; {
		case !ok:
			t.Errorf("%s is registered and %s does not list it", name, architecture)
		case got != want:
			t.Errorf("%s: %s lists it as %q, the registry has %q", name, architecture, got, want)
		}
	}
	for name := range documented {
		if _, ok := registered[name]; !ok {
			t.Errorf("%s lists %s, which the registry does not have", architecture, name)
		}
	}
}
