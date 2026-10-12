package app_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/audit"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// settingsView is the replicated record of a deployment, as a test gives it.
type settingsView struct {
	nodes       []cluster.Node
	topology    cluster.Topology
	topologyErr error
	nodesErr    error
}

func (v settingsView) Topology(context.Context) (cluster.Topology, error) {
	return v.topology, v.topologyErr
}

func (v settingsView) Nodes(context.Context) ([]cluster.Node, error) { return v.nodes, v.nodesErr }

// settingsOf is what a node of the tests says of itself: its region, its
// build and one drive.
func settingsOf(nodeID, region, drive string) cluster.NodeSettings {
	return cluster.NodeSettings{
		NodeID: nodeID, Roles: []string{"storage"}, KMSKeyID: "local:1", Build: cluster.Build{Version: "1.4.0", Build: "211"},
		Settings: []cluster.Setting{
			cluster.NewSetting("s3-addr", "CACTUSFS_S3_ADDR", "s3-addr", "Listeners", cluster.SettingPlain, cluster.ScopeNode, cluster.SourceDefault, ":9000"),
			cluster.NewSetting("region", "CACTUSFS_REGION", "region", "Placement", cluster.SettingPlain, cluster.ScopeCluster, cluster.SourceFlag, region),
		},
		Drives: []cluster.DriveSettings{{ID: drive, Online: true, Volume: cluster.Volume{Directory: "/data/" + drive}}},
	}
}

// fourNodes is a deployment of four nodes with two drives each, in two
// erasure sets, of which n1 is the node that is asked.
func fourNodes(t *testing.T) settingsView {
	t.Helper()
	view := settingsView{topology: cluster.Topology{DeploymentID: "dep-1", Version: 1}}
	for set := range 2 {
		var slots []cluster.Slot
		for node := 1; node <= 4; node++ {
			slots = append(slots, cluster.Slot{DriveID: fmt.Sprintf("d%d-%d", node, set), NodeID: fmt.Sprintf("n%d", node)})
		}
		view.topology.Sets = append(view.topology.Sets, slots)
	}
	// The record is not kept in the order of the answer.
	for _, node := range []int{3, 1, 4, 2} {
		roles := cluster.Roles{cluster.RoleStorage, cluster.RoleGateway}
		if node <= 3 {
			roles = append(roles, cluster.RoleMeta)
		}
		view.nodes = append(view.nodes, cluster.Node{ID: fmt.Sprintf("n%d", node), Roles: roles, MetadataVersion: 3 + node%2})
	}
	return view
}

// settingsFixture is a SettingsService on node n1 and what it was given.
type settingsFixture struct {
	service *app.SettingsService
	reader  *porttest.NodeSettingsReader
	authz   *porttest.Authorizer
	local   *atomic.Int64
	log     *syncBuffer
}

func newSettingsFixture(t *testing.T, view ports.ClusterView, nodes map[string]porttest.NodeBehaviour, tune func(*app.SettingsServiceConfig)) settingsFixture {
	t.Helper()
	f := settingsFixture{
		reader: porttest.NewNodeSettingsReader(nodes), authz: &porttest.Authorizer{}, local: &atomic.Int64{}, log: &syncBuffer{},
	}
	cfg := app.SettingsServiceConfig{
		View: view, Authz: f.authz, NodeID: "n1", Leader: func() string { return "n2" }, Reader: f.reader,
		// What the metadata store says it writes at: the level of the
		// oldest member of fourNodes.
		Level: func(context.Context) (int, error) { return 3, nil },
		Local: func(context.Context) (cluster.NodeSettings, error) {
			f.local.Add(1)
			return settingsOf("n1", "us-east-1", "d1-0"), nil
		},
		// Long enough that a loaded machine does not make a node that
		// answers at once look slow; a node that is slow in these tests
		// never answers at all.
		NodeTimeout: 10 * time.Second,
		Logger:      slog.New(slog.NewTextHandler(f.log, nil)),
	}
	if tune != nil {
		tune(&cfg)
	}
	service, err := app.NewSettingsService(cfg)
	if err != nil {
		t.Fatalf("NewSettingsService: %v", err)
	}
	f.service = service
	return f
}

var settingsRoot = iam.Principal{AccessKeyID: "ROOT", Root: true}

func answerOf(t *testing.T, view app.SettingsView, nodeID string) cluster.NodeAnswer {
	t.Helper()
	for _, answer := range view.Nodes {
		if answer.NodeID == nodeID {
			return answer
		}
	}
	t.Fatalf("no node %s among %d", nodeID, len(view.Nodes))
	return cluster.NodeAnswer{}
}

// TestSettingsAreRefusedWithoutTheAction: reading the settings needs
// admin:GetSettings on the settings, through the one authorizer; who has it
// not is refused, asked of nobody, and the refusal is audited once. Who has
// it reads, and nothing is audited of that.
func TestSettingsAreRefusedWithoutTheAction(t *testing.T) {
	t.Parallel()
	policy := func(document string) iam.Document {
		parsed, err := iam.ParsePolicy([]byte(document))
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	settings := policy(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"admin:GetSettings","Resource":"arn:cactusfs:admin:::settings"}]}`)
	status := policy(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["admin:GetClusterStatus","s3:*"],"Resource":"*"}]}`)
	builtIn := iam.BuiltInPolicies()

	tests := []struct {
		name      string
		principal iam.Principal
		want      error
	}{
		{"root", settingsRoot, nil},
		{"a user allowed the action", iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{settings}}, nil},
		{"an auditor", iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{builtIn[iam.RoleAuditor]}}, nil},
		{"a cluster admin", iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{builtIn[iam.RoleClusterAdmin]}}, nil},
		{"a user allowed the status of the cluster", iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{status}}, domain.ErrAccessDenied},
		{"a read-only user", iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{builtIn[iam.RoleReadOnly]}}, domain.ErrAccessDenied},
		{"a bucket admin", iam.Principal{AccessKeyID: "K", User: "u", Policies: []iam.Document{builtIn[iam.RoleBucketAdmin]}}, domain.ErrAccessDenied},
		{"a user with no policy", iam.Principal{AccessKeyID: "K", User: "u"}, domain.ErrAccessDenied},
		{"a node", iam.Principal{NodeID: "n2"}, domain.ErrAccessDenied},
		{"nobody", iam.Principal{}, domain.ErrAccessDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// The real authorizer and a trail, so that what is audited is
			// what a deployment would audit.
			clock := porttest.NewClock(testNow)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			trail := porttest.NewMemIAM()
			auditor := app.NewAuditor(trail, clock, logger)
			f := newSettingsFixture(t, fourNodes(t), map[string]porttest.NodeBehaviour{
				"n2": {Settings: settingsOf("n2", "us-east-1", "d2-0")},
			}, func(cfg *app.SettingsServiceConfig) { cfg.Authz = app.NewAuthorizer(logger, clock, auditor) })

			view, err := f.service.Get(t.Context(), tt.principal)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Get = %v, want %v", err, tt.want)
			}
			auditor.Flush(t.Context())
			records, listErr := trail.ListAudit(t.Context(), 0, 100)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if tt.want == nil {
				if len(view.Nodes) != 4 || len(records) != 0 {
					t.Errorf("%d nodes, %d audit records; want 4 and none: reading is not audited", len(view.Nodes), len(records))
				}
				return
			}
			if !reflect.DeepEqual(view, app.SettingsView{}) {
				t.Errorf("a refusal came with a view: %+v", view)
			}
			if f.local.Load() != 0 || f.reader.Calls("n2") != 0 {
				t.Error("a node was asked for a caller who was refused")
			}
			if len(records) != 1 {
				t.Fatalf("%d audit records of one refusal", len(records))
			}
			record := records[0]
			if record.Outcome != audit.Denied || record.Action != string(iam.ActionGetSettings) || record.Resource != "arn:cactusfs:admin:::settings" {
				t.Errorf("audit record = %+v", record)
			}
		})
	}
}

// TestSettingsOfEveryNode: every node of the record is in the answer, by
// ID, with what it said or with why it said nothing; a node that is down,
// slow or old costs the others nothing; this node is never asked over the
// network; drives carry the erasure set the topology puts them in.
func TestSettingsOfEveryNode(t *testing.T) {
	t.Parallel()
	answering := func(nodes ...string) map[string]porttest.NodeBehaviour {
		out := map[string]porttest.NodeBehaviour{}
		for _, node := range nodes {
			out[node] = porttest.NodeBehaviour{Settings: settingsOf(node, "us-east-1", "d"+node[1:]+"-1")}
		}
		return out
	}
	with := func(base map[string]porttest.NodeBehaviour, node string, behaviour porttest.NodeBehaviour) map[string]porttest.NodeBehaviour {
		base[node] = behaviour
		return base
	}
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })

	tests := []struct {
		name    string
		nodes   map[string]porttest.NodeBehaviour
		timeout time.Duration
		// want is the reason of each node that did not answer.
		want map[string]cluster.AnswerReason
		// logged is what the log must say of the failures, and unsaid what
		// the answer must not.
		logged string
	}{
		{name: "all answer", nodes: answering("n2", "n3", "n4")},
		{
			name: "one cannot be reached", nodes: answering("n2", "n4"),
			want: map[string]cluster.AnswerReason{"n3": cluster.ReasonUnreachable}, logged: "node n3 cannot be reached",
		},
		{
			name:    "one does not answer in time",
			nodes:   with(answering("n2", "n3"), "n4", porttest.NodeBehaviour{Silent: true}),
			timeout: 100 * time.Millisecond,
			want:    map[string]cluster.AnswerReason{"n4": cluster.ReasonTimeout}, logged: "context deadline exceeded",
		},
		{
			name:    "one is asked by a reader that does not look at its context",
			nodes:   with(answering("n2", "n3"), "n4", porttest.NodeBehaviour{Stuck: stuck}),
			timeout: 100 * time.Millisecond,
			want:    map[string]cluster.AnswerReason{"n4": cluster.ReasonTimeout}, logged: "gave up waiting",
		},
		{
			name:  "one is of an older release",
			nodes: with(answering("n3", "n4"), "n2", porttest.NodeBehaviour{Err: domain.ErrNotSupported}),
			want:  map[string]cluster.AnswerReason{"n2": cluster.ReasonUnsupported},
		},
		{
			name:  "one was reached and could not say",
			nodes: with(answering("n3", "n4"), "n2", porttest.NodeBehaviour{Err: errors.New("cannot read /secret/place")}),
			want:  map[string]cluster.AnswerReason{"n2": cluster.ReasonFailed}, logged: "cannot read /secret/place",
		},
		{
			name: "one of each",
			nodes: map[string]porttest.NodeBehaviour{
				"n2": {Err: fmt.Errorf("%w: its certificate names another node", domain.ErrNotSupported)},
				"n4": {Silent: true},
			},
			timeout: 100 * time.Millisecond,
			want: map[string]cluster.AnswerReason{
				"n2": cluster.ReasonUnsupported, "n3": cluster.ReasonUnreachable, "n4": cluster.ReasonTimeout,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSettingsFixture(t, fourNodes(t), tt.nodes, func(cfg *app.SettingsServiceConfig) {
				if tt.timeout != 0 {
					cfg.NodeTimeout = tt.timeout
				}
			})
			view, err := f.service.Get(t.Context(), settingsRoot)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}

			var ids []string
			for _, answer := range view.Nodes {
				ids = append(ids, answer.NodeID)
			}
			if !slices.Equal(ids, []string{"n1", "n2", "n3", "n4"}) {
				t.Fatalf("nodes = %v, want every node once, by ID", ids)
			}
			for _, answer := range view.Nodes {
				reason, failed := tt.want[answer.NodeID]
				if answer.Answered == failed || answer.Reason != reason {
					t.Errorf("node %s: answered %v, reason %q; want reason %q", answer.NodeID, answer.Answered, answer.Reason, reason)
				}
				if failed {
					// Nothing but the reason is known of it.
					if !reflect.DeepEqual(answer.Settings, cluster.NodeSettings{}) {
						t.Errorf("node %s did not answer and has settings: %+v", answer.NodeID, answer.Settings)
					}
					continue
				}
				if answer.Settings.NodeID != answer.NodeID || len(answer.Settings.Settings) != 2 || answer.Settings.Settings[0].Name != "region" {
					t.Errorf("node %s: settings = %+v", answer.NodeID, answer.Settings)
				}
			}

			// This node answers for itself, once, and is not asked over
			// the network; every other node is asked exactly once.
			if f.local.Load() != 1 || f.reader.Calls("n1") != 0 {
				t.Errorf("this node: asked itself %d times and the reader %d times; want 1 and 0", f.local.Load(), f.reader.Calls("n1"))
			}
			for _, node := range []string{"n2", "n3", "n4"} {
				if calls := f.reader.Calls(node); calls != 1 {
					t.Errorf("node %s was asked %d times", node, calls)
				}
			}

			// The deployment, from the record.
			wantSet := func(index int) app.ErasureSetSettings {
				return app.ErasureSetSettings{Index: index, Drives: 4, Data: 2, Parity: 2, ReadQuorum: 2, WriteQuorum: 3}
			}
			wantCluster := app.ClusterSettings{
				DeploymentID: "dep-1", MetadataLevel: 3, Leader: "n2", Nodes: 4, Voters: 3,
				ErasureSets: []app.ErasureSetSettings{wantSet(0), wantSet(1)},
			}
			if view.AnsweredBy != "n1" || !reflect.DeepEqual(view.Cluster, wantCluster) {
				t.Errorf("answered by %q, cluster = %+v, want %+v", view.AnsweredBy, view.Cluster, wantCluster)
			}

			// Drives are in the set the topology puts them in: this node's
			// is d1-0, the others' d<n>-1.
			for _, answer := range view.Nodes {
				if !answer.Answered {
					continue
				}
				wantSet := 1
				if answer.NodeID == "n1" {
					wantSet = 0
				}
				if drives := answer.Settings.Drives; len(drives) != 1 || drives[0].ErasureSet != wantSet {
					t.Errorf("node %s: drives = %+v, want one in erasure set %d", answer.NodeID, drives, wantSet)
				}
			}
			if len(view.Mismatches) != 0 {
				t.Errorf("mismatches among nodes that agree: %+v", view.Mismatches)
			}

			// What went wrong is in the log, with the node; the answer has
			// the reason and not a word more.
			log := f.log.String()
			for node := range tt.want {
				if !strings.Contains(log, "level=WARN") || !strings.Contains(log, "node="+node) {
					t.Errorf("the log does not name node %s at WARN: %s", node, log)
				}
			}
			if !strings.Contains(log, tt.logged) {
				t.Errorf("the log lacks %q: %s", tt.logged, log)
			}
			if len(tt.want) == 0 && log != "" {
				t.Errorf("logged although every node answered: %s", log)
			}
			if tt.logged != "" && strings.Contains(fmt.Sprintf("%+v", view), tt.logged) {
				t.Errorf("the answer tells the cause %q: %+v", tt.logged, view)
			}
		})
	}
}

// TestSettingsAreRedactedWhoeverGaveThem: what a reader hands on, and what
// this node says of itself, is redacted again by the use case - a reader
// that is wrong, or a node that is, does not get a secret to the caller.
func TestSettingsAreRedactedWhoeverGaveThem(t *testing.T) {
	t.Parallel()
	const mark = porttest.SettingsMark
	leaking := func(nodeID string) cluster.NodeSettings {
		settings := settingsOf(nodeID, "us-east-1", "d-"+nodeID)
		settings.NodeID = "another-node"
		settings.Settings = append(settings.Settings,
			// Made by hand: nothing has redacted these.
			cluster.Setting{Name: "kms-passphrase", Kind: cluster.SettingSecret, Scope: cluster.ScopeCluster, Source: cluster.SourceEnvironment, Value: mark},
			cluster.Setting{Name: "root-secret-file", Kind: cluster.SettingSecretFile, Scope: cluster.ScopeNode, Value: "/run/" + mark},
			cluster.Setting{Name: "otlp-endpoint", Kind: cluster.SettingURL, Scope: cluster.ScopeCluster, Value: "https://u:" + mark + "@otel:4318/v1?k=" + mark},
			cluster.Setting{Name: "later", Kind: "unheard-of", Scope: cluster.ScopeNode, Value: mark},
			cluster.Setting{Name: "long", Kind: cluster.SettingPlain, Scope: cluster.ScopeNode, Value: strings.Repeat("x", 3*cluster.MaxSettingValueBytes)},
			cluster.Setting{Name: "a-first", Kind: cluster.SettingPlain, Scope: cluster.ScopeNode, Value: "1"},
		)
		return settings
	}
	view := settingsView{nodes: []cluster.Node{{ID: "n1"}, {ID: "n2"}}}
	f := newSettingsFixture(t, view, map[string]porttest.NodeBehaviour{"n2": {Settings: leaking("n2"), Raw: true}},
		func(cfg *app.SettingsServiceConfig) {
			cfg.Local = func(context.Context) (cluster.NodeSettings, error) { return leaking("n1"), nil }
		})
	got, err := f.service.Get(t.Context(), settingsRoot)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if shown := fmt.Sprintf("%+v", got); strings.Contains(shown, mark) {
		t.Fatalf("the view holds what must not be shown: %s", shown)
	}
	for _, answer := range got.Nodes {
		settings := answer.Settings
		if !answer.Answered || settings.NodeID != answer.NodeID {
			t.Errorf("node %s: answered %v as %q", answer.NodeID, answer.Answered, settings.NodeID)
		}
		byName := map[string]cluster.Setting{}
		for i, setting := range settings.Settings {
			byName[setting.Name] = setting
			if i > 0 && settings.Settings[i-1].Name > setting.Name {
				t.Errorf("node %s: settings are not by name", answer.NodeID)
			}
		}
		for _, name := range []string{"kms-passphrase", "root-secret-file", "later"} {
			if setting := byName[name]; setting.Value != "" || !setting.Set {
				t.Errorf("node %s: %s = %+v, want it set and without a value", answer.NodeID, name, setting)
			}
		}
		if url := byName["otlp-endpoint"].Value; url != "https://otel:4318/v1" {
			t.Errorf("node %s: the URL is shown as %q", answer.NodeID, url)
		}
		if long := byName["long"].Value; len(long) > cluster.MaxSettingValueBytes || !strings.HasSuffix(long, cluster.TruncationMarker) {
			t.Errorf("node %s: a value of %d bytes", answer.NodeID, len(long))
		}
		// No topology: the drive is in no set that is known.
		if len(settings.Drives) != 1 || settings.Drives[0].ErasureSet != cluster.ErasureSetUnknown {
			t.Errorf("node %s: drives = %+v", answer.NodeID, settings.Drives)
		}
	}
}

// TestSettingMismatchesAreListed: nodes that differ in what must be the same
// everywhere are named, with what each has; a node that did not answer is
// not part of the comparison.
func TestSettingMismatchesAreListed(t *testing.T) {
	t.Parallel()
	other := settingsOf("n3", "eu-west-1", "d3-1")
	other.KMSKeyID = "vault:other"
	f := newSettingsFixture(t, fourNodes(t), map[string]porttest.NodeBehaviour{
		"n2": {Settings: settingsOf("n2", "us-east-1", "d2-1")},
		"n3": {Settings: other},
	}, nil)
	view, err := f.service.Get(t.Context(), settingsRoot)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := []cluster.Mismatch{
		{Setting: cluster.SettingKMSKeyID, Values: []cluster.MismatchValue{{NodeID: "n1", Value: "local:1"}, {NodeID: "n2", Value: "local:1"}, {NodeID: "n3", Value: "vault:other"}}},
		{Setting: "region", Values: []cluster.MismatchValue{{NodeID: "n1", Value: "us-east-1"}, {NodeID: "n2", Value: "us-east-1"}, {NodeID: "n3", Value: "eu-west-1"}}},
	}
	if !reflect.DeepEqual(view.Mismatches, want) {
		t.Errorf("mismatches = %+v, want %+v", view.Mismatches, want)
	}
	if answerOf(t, view, "n4").Reason != cluster.ReasonUnreachable {
		t.Errorf("n4 = %+v", answerOf(t, view, "n4"))
	}
}

// TestSettingsOfOneNode: a node that stands alone, with nobody to ask and no
// way to, answers for itself - before it has a topology too, and when the
// record names no node, which is how a standalone node runs: it is a cluster
// of one, one node and one voter, at the level its metadata store says. The
// level is the store's wherever it is asked, and a store that cannot say
// fails the call.
func TestSettingsOfOneNode(t *testing.T) {
	t.Parallel()
	topology, err := cluster.NewTopology([]string{"d1-0"})
	if err != nil {
		t.Fatal(err)
	}
	failing := errors.New("the drives cannot be read")
	closed := fmt.Errorf("the store is closed: %w", domain.ErrUnavailable)
	tests := []struct {
		name  string
		view  settingsView
		local error
		// level and levelErr are what the metadata store says; a level of
		// 0 with no error stands for 4, one below 0 for a store that says 0.
		level    int
		levelErr error
		// wantErr is the error of Get; wantSet the erasure set of the drive.
		wantErr     error
		wantSet     int
		wantCluster app.ClusterSettings
	}{
		{
			name: "standalone", view: settingsView{nodes: []cluster.Node{{ID: "n1", Roles: cluster.AllRoles(), MetadataVersion: 4}}, topology: topology},
			wantCluster: app.ClusterSettings{
				DeploymentID: topology.DeploymentID, MetadataLevel: 4, Leader: "n2", Nodes: 1, Voters: 1,
				ErasureSets: []app.ErasureSetSettings{{Drives: 1, Data: 1, ReadQuorum: 1, WriteQuorum: 1}},
			},
		},
		{
			name: "no topology yet", view: settingsView{nodes: []cluster.Node{{ID: "n1", Roles: cluster.AllRoles(), MetadataVersion: 4}}, topologyErr: domain.ErrNoTopology},
			wantSet:     cluster.ErasureSetUnknown,
			wantCluster: app.ClusterSettings{MetadataLevel: 4, Leader: "n2", Nodes: 1, Voters: 1, ErasureSets: []app.ErasureSetSettings{}},
		},
		{
			// How a standalone node runs: it registers nobody.
			name: "no node recorded", view: settingsView{topology: topology},
			wantCluster: app.ClusterSettings{
				DeploymentID: topology.DeploymentID, MetadataLevel: 4, Leader: "n2", Nodes: 1, Voters: 1,
				ErasureSets: []app.ErasureSetSettings{{Drives: 1, Data: 1, ReadQuorum: 1, WriteQuorum: 1}},
			},
		},
		{
			name: "no node recorded, no topology yet", view: settingsView{topologyErr: domain.ErrNoTopology}, wantSet: cluster.ErasureSetUnknown,
			wantCluster: app.ClusterSettings{MetadataLevel: 4, Leader: "n2", Nodes: 1, Voters: 1, ErasureSets: []app.ErasureSetSettings{}},
		},
		{
			name: "no node recorded, a store of the release before", view: settingsView{topologyErr: domain.ErrNoTopology}, level: 3,
			wantSet:     cluster.ErasureSetUnknown,
			wantCluster: app.ClusterSettings{MetadataLevel: 3, Leader: "n2", Nodes: 1, Voters: 1, ErasureSets: []app.ErasureSetSettings{}},
		},
		{
			// The level is what the store writes at, not what the record
			// of a node says the node could apply.
			name: "the store is behind the record", view: settingsView{nodes: []cluster.Node{{ID: "n1", Roles: cluster.AllRoles(), MetadataVersion: 4}}, topologyErr: domain.ErrNoTopology},
			level: 2, wantSet: cluster.ErasureSetUnknown,
			wantCluster: app.ClusterSettings{MetadataLevel: 2, Leader: "n2", Nodes: 1, Voters: 1, ErasureSets: []app.ErasureSetSettings{}},
		},
		{
			// A recorded node is counted as it is recorded: one without
			// the meta role is no voter.
			name: "this node cannot say", view: settingsView{nodes: []cluster.Node{{ID: "n1"}}, topologyErr: domain.ErrNoTopology}, local: failing,
			wantCluster: app.ClusterSettings{MetadataLevel: 4, Leader: "n2", Nodes: 1, ErasureSets: []app.ErasureSetSettings{}},
		},
		{name: "the level cannot be read", view: settingsView{topology: topology}, levelErr: closed, wantErr: domain.ErrUnavailable},
		{name: "the level is none", view: settingsView{topology: topology}, level: -1, wantErr: domain.ErrUnavailable},
		{name: "the record cannot be read", view: settingsView{nodesErr: domain.ErrUnavailable}, wantErr: domain.ErrUnavailable},
		{name: "the topology cannot be read", view: settingsView{topologyErr: domain.ErrUnavailable}, wantErr: domain.ErrUnavailable},
		{
			name:    "a topology no code fits",
			view:    settingsView{topology: cluster.Topology{Sets: [][]cluster.Slot{make([]cluster.Slot, 17)}}},
			wantErr: domain.ErrInvalidArgument,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSettingsFixture(t, tt.view, nil, func(cfg *app.SettingsServiceConfig) {
				cfg.Reader = nil
				cfg.Level = func(context.Context) (int, error) {
					switch {
					case tt.levelErr != nil:
						return 0, tt.levelErr
					case tt.level < 0:
						return 0, nil
					case tt.level > 0:
						return tt.level, nil
					default:
						return 4, nil
					}
				}
				if tt.local != nil {
					cfg.Local = func(context.Context) (cluster.NodeSettings, error) { return cluster.NodeSettings{}, tt.local }
				}
			})
			view, err := f.service.Get(t.Context(), settingsRoot)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Get = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr != nil {
				if !reflect.DeepEqual(view, app.SettingsView{}) {
					t.Errorf("an error came with a view: %+v", view)
				}
				return
			}
			if len(view.Nodes) != 1 || view.Nodes[0].NodeID != "n1" || !reflect.DeepEqual(view.Cluster, tt.wantCluster) || view.Mismatches != nil {
				t.Fatalf("view = %+v", view)
			}
			answer := view.Nodes[0]
			if tt.local != nil {
				if answer.Answered || answer.Reason != cluster.ReasonFailed || strings.Contains(fmt.Sprintf("%+v", view), failing.Error()) {
					t.Errorf("answer = %+v", answer)
				}
				return
			}
			if !answer.Answered || len(answer.Settings.Drives) != 1 || answer.Settings.Drives[0].ErasureSet != tt.wantSet {
				t.Errorf("answer = %+v", answer)
			}
		})
	}

	// Without a way to ask, another node the record names is unreachable.
	f := newSettingsFixture(t, fourNodes(t), nil, func(cfg *app.SettingsServiceConfig) { cfg.Reader = nil })
	view, err := f.service.Get(t.Context(), settingsRoot)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, node := range []string{"n2", "n3", "n4"} {
		if answer := answerOf(t, view, node); answer.Answered || answer.Reason != cluster.ReasonUnreachable {
			t.Errorf("node %s = %+v", node, answer)
		}
	}
}

// TestSettingsReturnWhenTheCallerGoesAway: a caller who cancels is answered
// at once, with the error of its context and no half of a view - also while
// nodes wait for their turn behind others that say nothing.
func TestSettingsReturnWhenTheCallerGoesAway(t *testing.T) {
	t.Parallel()
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	tests := []struct {
		name        string
		behaviour   porttest.NodeBehaviour
		concurrency int
	}{
		{"nodes that never answer", porttest.NodeBehaviour{Silent: true}, 0},
		{"a reader that does not look at its context", porttest.NodeBehaviour{Stuck: stuck}, 0},
		{"nodes waiting for their turn", porttest.NodeBehaviour{Silent: true}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSettingsFixture(t, fourNodes(t), map[string]porttest.NodeBehaviour{"n2": tt.behaviour, "n3": tt.behaviour, "n4": tt.behaviour},
				func(cfg *app.SettingsServiceConfig) { cfg.NodeTimeout, cfg.Concurrency = time.Hour, tt.concurrency })
			ctx, cancel := context.WithCancel(t.Context())
			type result struct {
				view app.SettingsView
				err  error
			}
			returned := make(chan result, 1)
			go func() {
				view, err := f.service.Get(ctx, settingsRoot)
				returned <- result{view, err}
			}()
			// Cancelled before or after the nodes were asked: either way
			// the call must come back.
			cancel()
			select {
			case r := <-returned:
				if !errors.Is(r.err, context.Canceled) || !reflect.DeepEqual(r.view, app.SettingsView{}) {
					t.Errorf("Get = %+v, %v", r.view, r.err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("Get did not return when its caller went away")
			}
			if strings.Contains(f.log.String(), "did not give its settings") {
				t.Errorf("nodes are blamed for a caller who went away: %s", f.log.String())
			}
		})
	}
}

// TestSettingsAskNoMoreNodesAtOnceThanAllowed: with more nodes than may be
// asked at one time, the others wait their turn - and all are asked.
func TestSettingsAskNoMoreNodesAtOnceThanAllowed(t *testing.T) {
	t.Parallel()
	const nodes, atOnce = 12, 3
	view := settingsView{topologyErr: domain.ErrNoTopology}
	for i := 1; i <= nodes; i++ {
		view.nodes = append(view.nodes, cluster.Node{ID: fmt.Sprintf("n%02d", i)})
	}
	reader := &countingReader{}
	f := newSettingsFixture(t, view, nil, func(cfg *app.SettingsServiceConfig) {
		cfg.NodeID, cfg.Reader, cfg.Concurrency = "n01", reader, atOnce
	})
	got, err := f.service.Get(t.Context(), settingsRoot)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Nodes) != nodes || reader.calls.Load() != nodes-1 {
		t.Errorf("%d nodes in the answer, %d asked; want %d and %d", len(got.Nodes), reader.calls.Load(), nodes, nodes-1)
	}
	for _, answer := range got.Nodes {
		if !answer.Answered {
			t.Errorf("node %s = %+v", answer.NodeID, answer)
		}
	}
	if most := reader.most.Load(); most > atOnce || most < 1 {
		t.Errorf("%d nodes were asked at one time, want at most %d", most, atOnce)
	}
}

// countingReader answers for every node and counts how many calls it has in
// hand at one time.
type countingReader struct {
	calls, running, most atomic.Int64
}

func (r *countingReader) NodeSettings(_ context.Context, nodeID string) (cluster.NodeSettings, error) {
	r.calls.Add(1)
	running := r.running.Add(1)
	defer r.running.Add(-1)
	for {
		most := r.most.Load()
		if running <= most || r.most.CompareAndSwap(most, running) {
			break
		}
	}
	// Give the others the time to come in, were they allowed to.
	time.Sleep(5 * time.Millisecond)
	return cluster.NodeSettings{NodeID: nodeID}, nil
}

// TestNewSettingsServiceNeedsItsDependencies: a service that lacks what it
// cannot do without is not made; one without a reader is.
func TestNewSettingsServiceNeedsItsDependencies(t *testing.T) {
	t.Parallel()
	complete := func() app.SettingsServiceConfig {
		return app.SettingsServiceConfig{
			View: settingsView{}, Authz: &porttest.Authorizer{}, NodeID: "n1", Leader: func() string { return "" },
			Level:  func(context.Context) (int, error) { return 4, nil },
			Local:  func(context.Context) (cluster.NodeSettings, error) { return cluster.NodeSettings{}, nil },
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
	}
	tests := []struct {
		name string
		tune func(*app.SettingsServiceConfig)
		ok   bool
	}{
		{"complete, without a reader", func(*app.SettingsServiceConfig) {}, true},
		{"no view", func(cfg *app.SettingsServiceConfig) { cfg.View = nil }, false},
		{"no authorizer", func(cfg *app.SettingsServiceConfig) { cfg.Authz = nil }, false},
		{"no node ID", func(cfg *app.SettingsServiceConfig) { cfg.NodeID = "" }, false},
		{"no leader", func(cfg *app.SettingsServiceConfig) { cfg.Leader = nil }, false},
		{"no metadata level", func(cfg *app.SettingsServiceConfig) { cfg.Level = nil }, false},
		{"no local settings", func(cfg *app.SettingsServiceConfig) { cfg.Local = nil }, false},
		{"no logger", func(cfg *app.SettingsServiceConfig) { cfg.Logger = nil }, false},
		{"a negative timeout", func(cfg *app.SettingsServiceConfig) { cfg.NodeTimeout = -time.Second }, false},
		{"a negative concurrency", func(cfg *app.SettingsServiceConfig) { cfg.Concurrency = -1 }, false},
	}
	for _, tt := range tests {
		cfg := complete()
		tt.tune(&cfg)
		service, err := app.NewSettingsService(cfg)
		if tt.ok != (err == nil) || tt.ok != (service != nil) {
			t.Errorf("%s: NewSettingsService = %v, %v", tt.name, service, err)
		}
		if err != nil && !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: error %v does not say that the configuration is wrong", tt.name, err)
		}
	}
}
