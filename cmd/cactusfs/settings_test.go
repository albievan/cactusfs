package main

import (
	"bytes"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// TestSettingsRegistry: the list of settings is whole and says of each what
// the rest of the program relies on.
func TestSettingsRegistry(t *testing.T) {
	t.Parallel()
	specs := serverSettings()
	groups := map[string]bool{}
	for _, group := range settingGroups() {
		if groups[group.Name] || group.Title == "" {
			t.Errorf("group %q is listed twice or has no title", group.Name)
		}
		groups[group.Name] = true
	}
	names, envs := map[string]bool{}, map[string]bool{}
	name := regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	used := map[string]bool{}
	for _, spec := range specs {
		t.Run(spec.Name, func(t *testing.T) {
			if !name.MatchString(spec.Name) {
				t.Errorf("name %q: want lowercase words joined by hyphens", spec.Name)
			}
			if want := "CACTUSFS_" + strings.ToUpper(strings.ReplaceAll(spec.Name, "-", "_")); spec.Env != want {
				t.Errorf("variable %q, want %q", spec.Env, want)
			}
			if names[spec.Name] || envs[spec.Env] {
				t.Errorf("%s (%s) is declared twice", spec.Name, spec.Env)
			}
			names[spec.Name], envs[spec.Env] = true, true
			if !spec.Kind.Valid() || !spec.Scope.Valid() {
				t.Errorf("kind %q, scope %q", spec.Kind, spec.Scope)
			}
			if !groups[spec.Group] {
				t.Errorf("group %q is not one of settingGroups", spec.Group)
			}
			used[spec.Group] = true
			if spec.Usage == "" || strings.Contains(spec.Usage, spec.Env) {
				t.Errorf("usage %q: want what the setting is for, without its own variable", spec.Usage)
			}
			// A command line is visible to every user of the machine.
			if spec.Kind == cluster.SettingSecret && spec.Flag {
				t.Error("a secret has a flag")
			}
			if spec.Kind != cluster.SettingSecret && !spec.Flag {
				t.Error("a setting that is no secret has no flag")
			}
			if spec.holdsSecret() && spec.Default != "" {
				t.Errorf("a secret has the default %q", spec.Default)
			}
			if spec.bind == nil {
				t.Fatal("nothing to keep the value in")
			}
			var cfg serverConfig
			switch spec.bind(&settingTargets{cfg: &cfg}).(type) {
			case *string, *bool, *time.Duration, *int, *stringList:
			default:
				t.Errorf("the value is kept in a %T", spec.bind(&settingTargets{cfg: &cfg}))
			}
		})
	}
	for group := range groups {
		if !used[group] {
			t.Errorf("group %q has no setting", group)
		}
	}
}

// TestSettingsClassification: what must never be shown is declared so, and
// what every node must have the same is too. A setting added without a
// thought for either shows up here.
func TestSettingsClassification(t *testing.T) {
	t.Parallel()
	kinds := map[cluster.SettingKind][]string{
		cluster.SettingSecret:     {"kms-passphrase", "root-access-key", "root-secret-key"},
		cluster.SettingSecretFile: {"kms-key-file", "kms-passphrase-file", "node-tls-key", "tls-key", "vault-token-file"},
		cluster.SettingURL:        {"otlp-endpoint", "vault-addr"},
		cluster.SettingPath:       {"data", "meta-dir", "node-tls-ca", "node-tls-cert", "tls-cert", "vault-ca-cert"},
	}
	sameEverywhere := []string{
		"gc-grace", "kms", "lifecycle-interval", "peers", "region", "s3-domain", "scrub-interval", "scrub-rate-mib",
		"vault-addr", "vault-key", "vault-namespace", "vault-transit-mount",
	}
	got := map[cluster.SettingKind][]string{}
	var everywhere []string
	for _, spec := range serverSettings() {
		if spec.Kind != cluster.SettingPlain {
			got[spec.Kind] = append(got[spec.Kind], spec.Name)
		}
		if spec.Scope == cluster.ScopeCluster {
			everywhere = append(everywhere, spec.Name)
		}
		// Whatever is called a key, a token or a passphrase is a secret
		// or a file that holds one - but for the name of the Transit key.
		if strings.Contains(spec.Name, "key") || strings.Contains(spec.Name, "token") || strings.Contains(spec.Name, "passphrase") {
			if !spec.holdsSecret() && spec.Name != "vault-key" {
				t.Errorf("%s is of kind %q", spec.Name, spec.Kind)
			}
		}
	}
	for kind, want := range kinds {
		slices.Sort(got[kind])
		if !slices.Equal(got[kind], want) {
			t.Errorf("settings of kind %q = %v, want %v", kind, got[kind], want)
		}
	}
	slices.Sort(everywhere)
	if !slices.Equal(everywhere, sameEverywhere) {
		t.Errorf("settings of scope cluster = %v, want %v", everywhere, sameEverywhere)
	}
}

// TestEveryFlagIsASetting: the command line and the list are the same set,
// and --help says of each flag what the list says.
func TestEveryFlagIsASetting(t *testing.T) {
	t.Parallel()
	var cfg serverConfig
	targets := settingTargets{cfg: &cfg}
	flags := flag.NewFlagSet("cactusfs server", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	specs := map[string]settingSpec{}
	for _, spec := range serverSettings() {
		source, err := spec.declare(flags, &targets, noEnv)
		if err != nil {
			t.Fatalf("%s: the default %q is not a value of the setting: %v", spec.Name, spec.Default, err)
		}
		if source != cluster.SourceDefault {
			t.Errorf("%s: source %q with nothing given", spec.Name, source)
		}
		specs[spec.Name] = spec
	}
	declared := map[string]bool{}
	flags.VisitAll(func(f *flag.Flag) {
		declared[f.Name] = true
		spec, ok := specs[f.Name]
		switch {
		case !ok || !spec.Flag:
			t.Errorf("--%s is a flag and no setting with a flag", f.Name)
		case f.Usage != spec.Usage+" ($"+spec.Env+")":
			t.Errorf("--%s: usage %q", f.Name, f.Usage)
		}
	})
	for name, spec := range specs {
		if spec.Flag != declared[name] {
			t.Errorf("%s: declared with a flag %t, on the command line %t", name, spec.Flag, declared[name])
		}
	}
}

// TestServerHelpIsTheGolden: --help is, byte for byte, the text in
// testdata/server-help.golden. That file was written by the binary before
// the flags were declared from one list (PLAN H.1) and is its output since;
// a flag that is added, or a usage that changes, changes it on purpose:
//
//	go run ./cmd/cactusfs server --help 2> cmd/cactusfs/testdata/server-help.golden
func TestServerHelpIsTheGolden(t *testing.T) {
	t.Parallel()
	want, err := fs.ReadFile(os.DirFS("testdata"), "server-help.golden")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"server", "--help"}, &stdout, &stderr, noEnv); code != exitOK {
		t.Fatalf("server --help = %d", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("server --help wrote to standard output: %q", stdout.String())
	}
	if got := stderr.String(); got != string(want) {
		t.Errorf("server --help differs from testdata/server-help.golden; if that is meant, write the file again:\n"+
			"  go run ./cmd/cactusfs server --help 2> cmd/cactusfs/testdata/server-help.golden\ngot:\n%s", got)
	}
}

// TestSettingSources: where each value came from is recorded for every way a
// value arrives.
func TestSettingSources(t *testing.T) {
	t.Parallel()
	dataDir, otherDir := t.TempDir(), t.TempDir()
	const (
		byDefault   = cluster.SourceDefault
		environment = cluster.SourceEnvironment
		byFlag      = cluster.SourceFlag
		derived     = cluster.SourceDerived
	)
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want map[string]cluster.SettingSource
	}{
		{
			name: "defaults, and what the node works out from them",
			args: []string{"--dev-plaintext", "--data", dataDir, "--kms", "local"},
			env:  map[string]string{envKMSPhrase: "unit test passphrase"},
			want: map[string]cluster.SettingSource{
				"dev-plaintext": byFlag, "data": byFlag, "kms": byFlag, "kms-passphrase": environment,
				"log-level": byDefault, "scrub-interval": byDefault, "scrub-rate-mib": byDefault, "audit-export": byDefault,
				"rpc-addr": byDefault, "roles": byDefault, "region": byDefault, "peers": byDefault, "drive-labels": byDefault,
				"root-access-key": byDefault, "root-secret-key": byDefault, "tls-key": byDefault, "otlp-endpoint": byDefault,
				// Filled in by the node: the directory inside the first
				// drive, the key file inside that, loopback for a
				// development listener.
				"meta-dir": derived, "kms-key-file": derived, "s3-addr": derived, "admin-addr": derived,
			},
		},
		{
			name: "the environment, of every type of value",
			env: map[string]string{
				envKMSPhrase: "unit test passphrase", envDevPlaintext: "true", envData: dataDir + "," + otherDir, envKMS: "local",
				envDriveLabels: "vol-a,vol-b", envMetaDir: filepath.Join(dataDir, "m"), envScrubInterval: "6h", envScrubRate: "8",
				envLogLevel: "debug", envS3Addr: "127.0.0.1:19000", envRootKeyID: "ROOTKEY", envRootKeyValue: "root-secret-value",
				envKMSKeyFile: filepath.Join(dataDir, "k.json"), envAuditExport: "true",
				// Set to nothing is not set.
				envRegion: "", envGCGrace: "",
			},
			want: map[string]cluster.SettingSource{
				"dev-plaintext": environment, "data": environment, "kms": environment, "drive-labels": environment,
				"meta-dir": environment, "scrub-interval": environment, "scrub-rate-mib": environment, "log-level": environment,
				"s3-addr": environment, "root-access-key": environment, "root-secret-key": environment,
				"kms-key-file": environment, "kms-passphrase": environment, "audit-export": environment,
				"region": byDefault, "gc-grace": byDefault, "admin-addr": derived,
			},
		},
		{
			name: "a flag over the environment",
			args: []string{
				"--dev-plaintext=true", "--data", dataDir, "--kms", "local", "--drive-labels", "vol-a", "--scrub-interval", "1h",
				"--scrub-rate-mib", "4", "--log-level", "error", "--admin-addr", "127.0.0.1:2", "--region", "eu-1",
			},
			env: map[string]string{
				envKMSPhrase: "unit test passphrase", envDevPlaintext: "false", envData: otherDir, envDriveLabels: "vol-x",
				envScrubInterval: "6h", envScrubRate: "8", envLogLevel: "debug", envAdminAddr: "127.0.0.1:1", envRegion: "us-9",
				envGCInterval: "30m",
			},
			want: map[string]cluster.SettingSource{
				"dev-plaintext": byFlag, "data": byFlag, "kms": byFlag, "drive-labels": byFlag, "scrub-interval": byFlag,
				"scrub-rate-mib": byFlag, "log-level": byFlag, "admin-addr": byFlag, "region": byFlag,
				"gc-interval": environment, "s3-addr": derived,
			},
		},
		{
			name: "a list whose flag names nothing is read from the environment",
			args: []string{"--dev-plaintext", "--data", "", "--kms", "local"},
			env:  map[string]string{envKMSPhrase: "unit test passphrase", envData: dataDir},
			want: map[string]cluster.SettingSource{"data": environment, "drive-labels": byDefault},
		},
		{
			name: "a member of a cluster: the addresses it advertises",
			args: []string{
				"--tls-cert", "c.pem", "--tls-key", "k.pem", "--data", dataDir, "--kms", "local", "--kms-key-file", "k.json",
				"--node-id", "b", "--peers", "a=h1:9002,b=h2:9002,c=h3:9002,d=h4:9002",
				"--node-tls-cert", "n.pem", "--node-tls-key", "n.key", "--node-tls-ca", "ca.pem",
			},
			env: map[string]string{envKMSPhrase: "unit test passphrase"},
			want: map[string]cluster.SettingSource{
				"advertise-rpc": derived, "advertise-raft": derived, "meta-dir": derived, "kms-key-file": byFlag,
				"peers": byFlag, "node-id": byFlag, "s3-addr": byDefault, "admin-addr": byDefault, "raft-addr": byDefault,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, sources, err := parseServerSettings(tt.args, envMap(tt.env), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			for _, spec := range serverSettings() {
				if !sources[spec.Name].Valid() {
					t.Errorf("%s: source %q", spec.Name, sources[spec.Name])
				}
			}
			for name, want := range tt.want {
				if sources[name] != want {
					t.Errorf("%s: source %q, want %q", name, sources[name], want)
				}
			}
			// The configuration itself carries no record: whoever runs
			// the node hands it over.
			if cfg.sources != nil {
				t.Error("parsing recorded the sources in the configuration")
			}
		})
	}

	// A configuration that is refused has no record.
	if _, sources, err := parseServerSettings([]string{"--dev-plaintext"}, noEnv, io.Discard); err == nil || sources != nil {
		t.Errorf("a refused configuration: sources %v, error %v", sources, err)
	}
}

// TestDriveLabels: one label for each drive directory or none, each a name.
func TestDriveLabels(t *testing.T) {
	t.Parallel()
	one, two := t.TempDir(), t.TempDir()
	long := strings.Repeat("v", maxDriveLabel)
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    []string
		wantErr string
	}{
		{name: "none"},
		{name: "one for each drive", args: []string{"--drive-labels", "cactusfs_node1-data1,cactusfs_node1-data2"}, want: []string{"cactusfs_node1-data1", "cactusfs_node1-data2"}},
		{name: "the flag repeated", args: []string{"--drive-labels", "data1-n1-0", "--drive-labels", "data2-n1-0"}, want: []string{"data1-n1-0", "data2-n1-0"}},
		{name: "from the environment", env: map[string]string{envDriveLabels: "a.b/c, D_9"}, want: []string{"a.b/c", "D_9"}},
		{name: "the flag over the environment", args: []string{"--drive-labels", "x,y"}, env: map[string]string{envDriveLabels: "p,q"}, want: []string{"x", "y"}},
		{name: "the longest label", args: []string{"--drive-labels", long + ",b"}, want: []string{long, "b"}},
		{name: "set to nothing", env: map[string]string{envDriveLabels: ""}},
		{name: "too few", args: []string{"--drive-labels", "only"}, wantErr: "names 1 volumes for 2 data directories"},
		{name: "too many", args: []string{"--drive-labels", "a,b,c"}, wantErr: "names 3 volumes for 2 data directories"},
		{name: "an empty one counts as none given", args: []string{"--drive-labels", "a,,"}, wantErr: "names 1 volumes for 2 data directories"},
		{name: "a label too long", args: []string{"--drive-labels", long + "v,b"}, wantErr: "drive label 1"},
		{name: "a space", args: []string{"--drive-labels", "a b,c"}, wantErr: `drive label 1 ("a b")`},
		{name: "a colon", args: []string{"--drive-labels", "a,host:/export"}, wantErr: "drive label 2"},
		{name: "a line break", args: []string{"--drive-labels", "a,b\nc"}, wantErr: "drive label 2"},
		{name: "what is not ASCII", args: []string{"--drive-labels", "a,bé"}, wantErr: "drive label 2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{envKMSPhrase: "unit test passphrase"}
			for name, value := range tt.env {
				env[name] = value
			}
			args := append([]string{"--dev-plaintext", "--data", one + "," + two, "--kms", "local"}, tt.args...)
			cfg, err := parseServerConfig(args, envMap(env), io.Discard)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cfg.DriveLabels, tt.want) {
				t.Errorf("labels = %q, want %q", cfg.DriveLabels, tt.want)
			}
		})
	}
}

// TestShownSettings: the settings of a configuration as they may be shown.
// The value of a secret is not only removed by the domain: it is never
// handed to it.
func TestShownSettings(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	mark := strings.Join([]string{"MARK", "5d1e", "NEVER", "SHOWN"}, "-")
	cfg, sources, err := parseServerSettings([]string{
		"--dev-plaintext", "--data", dataDir, "--kms", "local", "--drive-labels", "vol-1", "--scrub-interval", "36h",
		"--otlp-endpoint", "https://user:" + mark + "@collector.test:4318/v1/traces?token=" + mark + "#" + mark,
		"--region", "eu-1",
	}, envMap(map[string]string{
		envKMSPhrase: mark + "-passphrase", envRootKeyID: mark + "-ID", envRootKeyValue: mark + "-secret", envLogLevel: "warn",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg.sources = sources

	shown := map[string]cluster.Setting{}
	for _, setting := range shownSettings(cfg) {
		if strings.Contains(setting.Value, mark) || strings.Contains(setting.Value, secretPlaceholder) {
			t.Errorf("%s is shown as %q", setting.Name, setting.Value)
		}
		if setting != setting.Redacted() {
			t.Errorf("%s is not as NewSetting makes it: %+v", setting.Name, setting)
		}
		shown[setting.Name] = setting
	}
	if len(shown) != len(serverSettings()) {
		t.Errorf("%d settings shown, %d declared", len(shown), len(serverSettings()))
	}
	want := []cluster.Setting{
		{Name: "data", Env: envData, Flag: "data", Group: groupStorage, Kind: cluster.SettingPath, Scope: cluster.ScopeNode, Source: cluster.SourceFlag, Set: true, Value: dataDir},
		{Name: "drive-labels", Env: envDriveLabels, Flag: "drive-labels", Group: groupStorage, Kind: cluster.SettingPlain, Scope: cluster.ScopeNode, Source: cluster.SourceFlag, Set: true, Value: "vol-1"},
		{Name: "meta-dir", Env: envMetaDir, Flag: "meta-dir", Group: groupStorage, Kind: cluster.SettingPath, Scope: cluster.ScopeNode, Source: cluster.SourceDerived, Set: true, Value: filepath.Join(dataDir, "meta")},
		{Name: "scrub-interval", Env: envScrubInterval, Flag: "scrub-interval", Group: groupMaintenance, Kind: cluster.SettingPlain, Scope: cluster.ScopeCluster, Source: cluster.SourceFlag, Set: true, Value: "36h"},
		{Name: "gc-grace", Env: envGCGrace, Flag: "gc-grace", Group: groupMaintenance, Kind: cluster.SettingPlain, Scope: cluster.ScopeCluster, Source: cluster.SourceDefault, Set: true, Value: "24h"},
		{Name: "log-level", Env: envLogLevel, Flag: "log-level", Group: groupObservability, Kind: cluster.SettingPlain, Scope: cluster.ScopeNode, Source: cluster.SourceEnvironment, Set: true, Value: "warn"},
		{Name: "roles", Env: envRoles, Flag: "roles", Group: groupCluster, Kind: cluster.SettingPlain, Scope: cluster.ScopeNode, Source: cluster.SourceDefault, Set: true, Value: "gateway,meta,storage,console"},
		{Name: "s3-addr", Env: envS3Addr, Flag: "s3-addr", Group: groupListeners, Kind: cluster.SettingPlain, Scope: cluster.ScopeNode, Source: cluster.SourceDerived, Set: true, Value: "127.0.0.1:9000"},
		{Name: "otlp-endpoint", Env: envOTLPEndpoint, Flag: "otlp-endpoint", Group: groupObservability, Kind: cluster.SettingURL, Scope: cluster.ScopeNode, Source: cluster.SourceFlag, Set: true, Value: "https://collector.test:4318/v1/traces"},
		{Name: "kms-passphrase", Env: envKMSPhrase, Group: groupKMS, Kind: cluster.SettingSecret, Scope: cluster.ScopeNode, Source: cluster.SourceEnvironment, Set: true},
		{Name: "root-secret-key", Env: envRootKeyValue, Group: groupBootstrap, Kind: cluster.SettingSecret, Scope: cluster.ScopeNode, Source: cluster.SourceEnvironment, Set: true},
		{Name: "kms-key-file", Env: envKMSKeyFile, Flag: "kms-key-file", Group: groupKMS, Kind: cluster.SettingSecretFile, Scope: cluster.ScopeNode, Source: cluster.SourceDerived, Set: true},
		{Name: "kms-passphrase-file", Env: envKMSPhraseFile, Flag: "kms-passphrase-file", Group: groupKMS, Kind: cluster.SettingSecretFile, Scope: cluster.ScopeNode, Source: cluster.SourceDefault},
		{Name: "peers", Env: envPeers, Flag: "peers", Group: groupCluster, Kind: cluster.SettingPlain, Scope: cluster.ScopeCluster, Source: cluster.SourceDefault},
	}
	for _, setting := range want {
		if shown[setting.Name] != setting {
			t.Errorf("%s =\n%+v\nwant\n%+v", setting.Name, shown[setting.Name], setting)
		}
	}

	// A configuration that was not parsed has no record of its sources;
	// its settings are shown all the same, as defaults.
	for _, setting := range shownSettings(nodeConfig(dataDir)) {
		if setting.Source != cluster.SourceDefault {
			t.Errorf("%s of a configuration built by hand: source %q", setting.Name, setting.Source)
		}
	}
	if got := peersText(map[string]string{"b": "h2:9002", "a": "h1:9002"}); got != "a=h1:9002,b=h2:9002" {
		t.Errorf("peers = %q", got)
	}
}

// TestDurationText: a duration is written as it is typed.
func TestDurationText(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		0: "0s", 30 * time.Second: "30s", time.Minute: "1m", 90 * time.Second: "1m30s", time.Hour: "1h",
		24 * time.Hour: "24h", 90 * time.Minute: "1h30m", time.Hour + time.Second: "1h0m1s", 1500 * time.Millisecond: "1.5s",
	} {
		got := durationText(d)
		if got != want {
			t.Errorf("durationText(%v) = %q, want %q", d, got, want)
		}
		if back, err := time.ParseDuration(got); err != nil || back != d {
			t.Errorf("durationText(%v) = %q, which reads back as %v (%v)", d, got, back, err)
		}
	}
}
