package porttest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/ports"
)

// NodeBehaviour is what a node of a NodeSettingsReader does when it is asked
// for its settings. The first of Err, Silent and Stuck that is set decides.
type NodeBehaviour struct {
	// Settings is what the node answers.
	Settings cluster.NodeSettings
	// Raw has the answer handed on as the node gave it, without the
	// redaction a reader owes: a reader that is wrong, for the tests of
	// what stands behind it.
	Raw bool
	// Err, when not nil, is returned instead of an answer.
	Err error
	// Silent is a node that never answers: the call returns when its
	// context ends, with the error of the context.
	Silent bool
	// Stuck, when not nil, is a call that does not look at its context: it
	// returns when the channel is closed, and not before.
	Stuck <-chan struct{}
}

// NodeSettingsReader is a ports.NodeSettingsReader whose nodes do as a test
// says. A node it was told nothing about cannot be reached.
type NodeSettingsReader struct {
	mu    sync.Mutex
	nodes map[string]NodeBehaviour
	calls map[string]int
}

var _ ports.NodeSettingsReader = (*NodeSettingsReader)(nil)

// NewNodeSettingsReader returns a reader of the nodes named.
func NewNodeSettingsReader(nodes map[string]NodeBehaviour) *NodeSettingsReader {
	r := &NodeSettingsReader{nodes: map[string]NodeBehaviour{}, calls: map[string]int{}}
	for id, behaviour := range nodes {
		r.nodes[id] = behaviour
	}
	return r
}

// Calls returns how often the node was asked.
func (r *NodeSettingsReader) Calls(nodeID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[nodeID]
}

// NodeSettings implements ports.NodeSettingsReader.
func (r *NodeSettingsReader) NodeSettings(ctx context.Context, nodeID string) (cluster.NodeSettings, error) {
	r.mu.Lock()
	r.calls[nodeID]++
	behaviour, known := r.nodes[nodeID]
	r.mu.Unlock()
	switch {
	case !known:
		return cluster.NodeSettings{}, fmt.Errorf("%w: node %s cannot be reached", domain.ErrUnavailable, nodeID)
	case behaviour.Err != nil:
		return cluster.NodeSettings{}, fmt.Errorf("node %s: %w", nodeID, behaviour.Err)
	case behaviour.Silent:
		<-ctx.Done()
		return cluster.NodeSettings{}, fmt.Errorf("node %s: %w", nodeID, ctx.Err())
	case behaviour.Stuck != nil:
		<-behaviour.Stuck
		return cluster.NodeSettings{}, fmt.Errorf("node %s: %w", nodeID, domain.ErrUnavailable)
	case behaviour.Raw:
		return behaviour.Settings, nil
	}
	if err := ctx.Err(); err != nil {
		return cluster.NodeSettings{}, fmt.Errorf("node %s: %w", nodeID, err)
	}
	settings := behaviour.Settings.Normalized()
	settings.NodeID = nodeID
	for i := range settings.Drives {
		settings.Drives[i].ErasureSet = cluster.ErasureSetUnknown
	}
	return settings, nil
}

// SettingsMark is in every value of the script of
// NodeSettingsContract that must not be shown. A reader that hands it on has
// failed.
const SettingsMark = "MARK-f3a9-MUST-NOT-BE-SHOWN"

// The nodes of the script of NodeSettingsContract.
const (
	// SettingsNodeAnswering answers with NodeSettingsScript.Settings.
	SettingsNodeAnswering = "node-a"
	// SettingsNodeOld is of a release that cannot be asked.
	SettingsNodeOld = "node-old"
	// SettingsNodeSilent is reached and never answers.
	SettingsNodeSilent = "node-silent"
	// SettingsNodeFailing is reached and answers that it cannot say.
	SettingsNodeFailing = "node-failing"
	// SettingsNodeGone is a node that is not there.
	SettingsNodeGone = "node-gone"
)

// NodeSettingsScript is what the nodes behind a reader under test must do.
type NodeSettingsScript struct {
	// Settings is what SettingsNodeAnswering says of itself: exactly this,
	// as its source gives it - unredacted, since the script is of a node
	// that is wrong.
	Settings cluster.NodeSettings
}

// scriptedSettings returns the answer of the script: a node that says more
// than it may.
func scriptedSettings() cluster.NodeSettings {
	started := time.Date(2026, 10, 12, 8, 30, 0, 0, time.UTC)
	return cluster.NodeSettings{
		NodeID: SettingsNodeAnswering,
		Roles:  []string{"meta", "storage"},
		Labels: map[string]string{"zone": "z1", "rack": "r7"},
		Build:  cluster.Build{Version: "1.4.0", Build: "211", Commit: "abc1234"},
		// Whole seconds: that is what a start and a certificate are told in.
		StartedAt: started,
		KMSKeyID:  "local:1",
		Settings: []cluster.Setting{
			cluster.NewSetting("s3-addr", "CACTUSFS_S3_ADDR", "s3-addr", "Listeners", cluster.SettingPlain, cluster.ScopeNode, cluster.SourceFlag, ":9000"),
			cluster.NewSetting("region", "CACTUSFS_REGION", "region", "Placement", cluster.SettingPlain, cluster.ScopeCluster, cluster.SourceDefault, "us-east-1"),
			// Made by hand, so nothing has redacted them: a passphrase
			// with its value, a URL with a password, the path of a secret,
			// and a kind nobody has heard of.
			{
				Name: "kms-passphrase", Env: "CACTUSFS_KMS_PASSPHRASE", Group: "KMS", Kind: cluster.SettingSecret,
				Scope: cluster.ScopeCluster, Source: cluster.SourceEnvironment, Set: true, Value: "passphrase-" + SettingsMark,
			},
			{
				Name: "otlp-endpoint", Env: "CACTUSFS_OTLP_ENDPOINT", Flag: "otlp-endpoint", Group: "Telemetry", Kind: cluster.SettingURL,
				Scope: cluster.ScopeCluster, Source: cluster.SourceEnvironment, Set: true,
				Value: "https://collector:" + SettingsMark + "@otel.example:4318/v1?token=" + SettingsMark + "#" + SettingsMark,
			},
			{
				Name: "root-secret-file", Env: "CACTUSFS_ROOT_SECRET_FILE", Group: "Bootstrap", Kind: cluster.SettingSecretFile,
				Scope: cluster.ScopeNode, Source: cluster.SourceEnvironment, Set: true, Value: "/run/secrets/" + SettingsMark,
			},
			{
				Name: "of-a-later-release", Kind: "quantum", Scope: cluster.ScopeNode,
				Source: cluster.SourceFlag, Value: "unknown-" + SettingsMark,
			},
		},
		Drives: []cluster.DriveSettings{
			{
				ID: "d2", Online: true, ErasureSet: cluster.ErasureSetUnknown,
				Volume: cluster.Volume{
					Directory: "/data/2", Label: "cactusfs_node1-data2", TotalBytes: 1 << 40, FreeBytes: 1 << 39,
					Filesystem: cluster.Filesystem{Type: "ext4", Device: "/dev/sdb1", MountPoint: "/data/2"},
				},
			},
			{ID: "d1", ErasureSet: cluster.ErasureSetUnknown, Volume: cluster.Volume{Directory: "/data/1"}},
		},
		Metadata: cluster.Volume{
			Directory: "/data/1/.cactusfs/meta", TotalBytes: 1 << 30, FreeBytes: 1 << 29,
			Filesystem: cluster.Filesystem{Type: "xfs", Device: "/dev/sda1", MountPoint: "/data/1"},
		},
		Certificates: []cluster.Certificate{
			{
				Use: cluster.CertificateS3, Subject: "CN=s3.example", Issuer: "CN=Example CA",
				DNSNames: []string{"s3.example", "*.s3.example"}, IPAddresses: []string{"192.0.2.7"},
				NotBefore: started.Add(-time.Hour), NotAfter: started.Add(90 * 24 * time.Hour),
			},
			{
				Use: cluster.CertificateNode, Subject: "CN=node-a", Issuer: "CN=CactusFS node CA",
				DNSNames: []string{"node-a"}, NotBefore: started.Add(-time.Hour), NotAfter: started.Add(24 * time.Hour),
			},
		},
	}
}

// settingsContractWait is how long the contract lets a call take that must
// return when its context ends. It is far more than any reader needs and
// there only so that a reader that hangs fails the test instead of the run.
const settingsContractWait = 30 * time.Second

// NodeSettingsContract verifies that a reader keeps the promises of
// ports.NodeSettingsReader. open returns a reader whose nodes do what the
// script says of the SettingsNode names: one answers with the settings of
// the script, one is of an older release, one never answers, one answers
// that it cannot say, and one is not there.
func NodeSettingsContract(t *testing.T, open func(t *testing.T, script NodeSettingsScript) ports.NodeSettingsReader) {
	t.Helper()
	script := NodeSettingsScript{Settings: scriptedSettings()}

	t.Run("an answer is what the node said, redacted and in order", func(t *testing.T) {
		t.Parallel()
		reader := open(t, script)
		got, err := reader.NodeSettings(t.Context(), SettingsNodeAnswering)
		if err != nil {
			t.Fatalf("NodeSettings: %v", err)
		}
		want := script.Settings.Normalized()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("NodeSettings =\n%+v\nwant\n%+v", got, want)
		}
		if shown := fmt.Sprintf("%+v", got); strings.Contains(shown, SettingsMark) {
			t.Errorf("the answer holds what must not be shown: %s", shown)
		}
		for _, setting := range got.Settings {
			if !setting.Set {
				t.Errorf("setting %s is not set any more", setting.Name)
			}
			if setting.Kind != cluster.SettingPlain && setting.Kind != cluster.SettingURL && setting.Value != "" {
				t.Errorf("setting %s of kind %s has the value %q", setting.Name, setting.Kind, setting.Value)
			}
		}
		for _, drive := range got.Drives {
			if drive.ErasureSet != cluster.ErasureSetUnknown {
				t.Errorf("drive %s is said to be in erasure set %d; a reader does not know", drive.ID, drive.ErasureSet)
			}
		}
		// Asked again, it answers again, and the same.
		again, err := reader.NodeSettings(t.Context(), SettingsNodeAnswering)
		if err != nil || !reflect.DeepEqual(again, got) {
			t.Errorf("asked again: %+v, %v", again, err)
		}
	})

	t.Run("why a node gave no answer", func(t *testing.T) {
		t.Parallel()
		reader := open(t, script)
		// The kinds of failure a reader tells apart, by a name of the test's
		// own; a failure of no kind is none of them.
		const unsupported, unreachable, timeout, failed = "not supported", "unavailable", "deadline exceeded", ""
		kinds := []struct {
			name string
			err  error
		}{
			{unsupported, domain.ErrNotSupported}, {unreachable, domain.ErrUnavailable}, {timeout, context.DeadlineExceeded},
		}
		tests := []struct {
			name    string
			node    string
			timeout time.Duration
			want    string
		}{
			{"a node of an older release", SettingsNodeOld, settingsContractWait, unsupported},
			{"a node that is not there", SettingsNodeGone, settingsContractWait, unreachable},
			{"a node that does not answer in time", SettingsNodeSilent, 200 * time.Millisecond, timeout},
			{"a node that cannot say", SettingsNodeFailing, settingsContractWait, failed},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(t.Context(), tt.timeout)
				defer cancel()
				began := time.Now()
				got, err := reader.NodeSettings(ctx, tt.node)
				if err == nil {
					t.Fatalf("NodeSettings = %+v, want an error", got)
				}
				if took := time.Since(began); took > settingsContractWait {
					t.Errorf("the call took %v", took)
				}
				for _, kind := range kinds {
					if errors.Is(err, kind.err) != (kind.name == tt.want) {
						t.Errorf("error %v: is %q = %v", err, kind.name, errors.Is(err, kind.err))
					}
				}
				if !reflect.DeepEqual(got, cluster.NodeSettings{}) {
					t.Errorf("an error came with settings: %+v", got)
				}
			})
		}
	})

	t.Run("a caller who went away is not kept", func(t *testing.T) {
		t.Parallel()
		reader := open(t, script)
		ctx, cancel := context.WithCancel(t.Context())
		returned := make(chan error, 1)
		go func() {
			_, err := reader.NodeSettings(ctx, SettingsNodeSilent)
			returned <- err
		}()
		cancel()
		select {
		case err := <-returned:
			if err == nil {
				t.Error("a call whose caller went away succeeded")
			}
		case <-time.After(settingsContractWait):
			t.Error("the call did not return when its context ended")
		}
	})
}

// ScriptedNodeSettingsReader returns the reader of this package with nodes
// that do what the script of NodeSettingsContract says.
func ScriptedNodeSettingsReader(script NodeSettingsScript) *NodeSettingsReader {
	return NewNodeSettingsReader(map[string]NodeBehaviour{
		SettingsNodeAnswering: {Settings: script.Settings},
		SettingsNodeOld:       {Err: domain.ErrNotSupported},
		SettingsNodeSilent:    {Silent: true},
		SettingsNodeFailing:   {Err: errors.New("the node could not read its drives")},
	})
}
