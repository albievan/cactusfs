package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/albievan/cactusfs/api/openapi"
	"github.com/albievan/cactusfs/internal/adapters/adminapi"
	"github.com/albievan/cactusfs/internal/adapters/kms/local"
	"github.com/albievan/cactusfs/internal/adapters/passwords"
	"github.com/albievan/cactusfs/internal/adapters/raftmeta"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
)

// settingsMark is in every secret, and in the name of every directory that
// holds one, that the tests below start a node with. It is put together
// here so that it is written nowhere whole.
func settingsMark() string {
	return strings.Join([]string{"MARK", "e7a2", "NEVER", "SHOWN"}, "-")
}

// wantNoneOf fails for every one of the secrets that text holds.
func wantNoneOf(t *testing.T, what, text string, marked map[string]string) {
	t.Helper()
	for name, secret := range marked {
		if secret != "" && strings.Contains(text, secret) {
			t.Errorf("%s holds %s", what, name)
		}
	}
}

// settingsAnswer is what GET /settings answers, as far as these tests read
// it.
type settingsAnswer struct {
	AnsweredBy string `json:"answered_by"`
	Cluster    struct {
		MetadataLevel int    `json:"metadata_level"`
		Leader        string `json:"leader"`
		Nodes         int    `json:"nodes"`
		Voters        int    `json:"voters"`
	} `json:"cluster"`
	Nodes []struct {
		NodeID   string            `json:"node_id"`
		Answered bool              `json:"answered"`
		Reason   string            `json:"reason"`
		Roles    []string          `json:"roles"`
		Labels   map[string]string `json:"labels"`
		KMSKeyID string            `json:"kms_key_id"`
		Server   struct {
			Version string `json:"version"`
		} `json:"server"`
		Settings []struct {
			Name   string `json:"name"`
			Kind   string `json:"kind"`
			Source string `json:"source"`
			Set    bool   `json:"set"`
			Value  string `json:"value"`
		} `json:"settings"`
		Drives []struct {
			ID         string `json:"id"`
			Directory  string `json:"directory"`
			Label      string `json:"label"`
			Online     bool   `json:"online"`
			TotalBytes int64  `json:"total_bytes"`
			ErasureSet *int   `json:"erasure_set"`
		} `json:"drives"`
		Metadata struct {
			Directory  string `json:"directory"`
			TotalBytes int64  `json:"total_bytes"`
		} `json:"metadata"`
		Certificates []struct {
			Use         string   `json:"use"`
			Subject     string   `json:"subject"`
			IPAddresses []string `json:"ip_addresses"`
		} `json:"certificates"`
	} `json:"nodes"`
}

// TestANodeShowsItsSettingsAndNoSecret: a node on its own, started as the
// binary starts one - its settings parsed from a command line and an
// environment, its listeners over TLS - answers GET /api/v1/settings with
// what it was told and where each value came from, to a caller who may read
// it and to nobody else. Every secret it was started with is marked, and so
// is every directory that holds one: no mark is in the answer, and none in
// the node's log. (PLAN H.9.)
func TestANodeShowsItsSettingsAndNoSecret(t *testing.T) {
	t.Parallel()
	base, mark := t.TempDir(), settingsMark()
	// The directory of the secret files is marked by its name.
	secretDir := filepath.Join(base, "holds-"+mark)
	if err := os.Mkdir(secretDir, 0o700); err != nil {
		t.Fatal(err)
	}
	certFile, madeKey, pool := writeTestCertificate(t)
	keyPEM, err := fs.ReadFile(os.DirFS(filepath.Dir(madeKey)), filepath.Base(madeKey))
	if err != nil {
		t.Fatal(err)
	}
	tlsKey, kmsKey := filepath.Join(secretDir, "tls.key"), filepath.Join(secretDir, "kms.json")
	writeTestFile(t, tlsKey, string(keyPEM))
	passphrase := mark + "-the-passphrase"
	if err := local.Create(kmsKey, []byte(passphrase), local.Params{Time: 1, MemoryKiB: 8 << 10, Threads: 1}, rand.Reader); err != nil {
		t.Fatal(err)
	}
	rootID, err := iam.GenerateAccessKeyID(strings.NewReader(strings.Repeat("q", 64)))
	if err != nil {
		t.Fatal(err)
	}
	rootSecret, err := iam.GenerateSecret(strings.NewReader(strings.Repeat("z", 64)))
	if err != nil {
		t.Fatal(err)
	}
	// Nothing listens there: no span leaves the process. A collector's URL
	// with a credential in it is refused before the node starts
	// (TestARefusedCollectorIsNamedWithoutItsCredential), and what is shown
	// of a URL that holds one all the same is in
	// TestEverySettingThatHoldsASecretIsHidden.
	collector := "https://127.0.0.1:9/v1/traces"
	marked := map[string]string{
		"the mark": mark, "the KMS passphrase": passphrase, "the root secret key": rootSecret,
		"the path of the TLS key": tlsKey, "the path of the KMS key file": kmsKey,
	}

	drives := []string{filepath.Join(base, "d1"), filepath.Join(base, "d2")}
	adminAddr, s3Addr := freeAddr(t), freeAddr(t)
	cfg, sources, err := parseServerSettings([]string{
		"--tls-cert", certFile, "--tls-key", tlsKey, "--data", strings.Join(drives, ","), "--drive-labels", "vol-one,vol-two",
		"--kms", "local", "--kms-key-file", kmsKey, "--otlp-endpoint", collector, "--admin-addr", adminAddr,
		"--topology-zone", "zone-a", "--shutdown-timeout", "5s",
	}, envMap(map[string]string{
		envKMSPhrase: passphrase, envRootKeyID: rootID, envRootKeyValue: rootSecret, envS3Addr: s3Addr, envScrubInterval: "36h",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// As the command does; and what has no flag, because only tests change
	// it.
	cfg.sources = sources
	cfg.passwordParams = passwords.Params{Time: 1, MemoryKiB: 8 << 10, Threads: 1}
	cfg.raftTiming = raftmeta.Timing{
		Heartbeat: 50 * time.Millisecond, Election: 50 * time.Millisecond, LeaderLease: 50 * time.Millisecond, Commit: 5 * time.Millisecond,
	}

	logs, notice := &lockedBuffer{buf: &bytes.Buffer{}}, &lockedBuffer{buf: &bytes.Buffer{}}
	text := func(l *lockedBuffer) string {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.buf.String()
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
		defer cancel()
		go func() {
			<-stop
			cancel()
		}()
		done <- runServer(ctx, cfg, logs, notice)
	}()
	stopped := false
	halt := func() error {
		if stopped {
			return nil
		}
		stopped = true
		close(stop)
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			t.Error("the node did not stop within 30 s")
			return nil
		}
	}
	t.Cleanup(func() { _ = halt() })

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}},
	}
	t.Cleanup(client.CloseIdleConnections)
	type answer struct {
		status      int
		contentType string
		body        []byte
		shown       *x509.Certificate
	}
	call := func(method, path, bearer string, body any) answer {
		t.Helper()
		var payload io.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			payload = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, "https://"+adminAddr+path, payload)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := client.Do(req)
		if err != nil {
			return answer{}
		}
		defer func() { _ = resp.Body.Close() }()
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		out := answer{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: got}
		if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
			out.shown = resp.TLS.PeerCertificates[0]
		}
		return out
	}
	for began := time.Now(); call(http.MethodGet, "/readyz", "", nil).status != http.StatusOK; {
		if time.Since(began) > 30*time.Second {
			t.Fatalf("the node was not ready within 30 s; log:\n%s", text(logs))
		}
		time.Sleep(20 * time.Millisecond)
	}
	rootBearer := rootID + ":" + rootSecret

	// Nobody is told anything without a credential.
	if got := call(http.MethodGet, "/api/v1/settings", "", nil); got.status != http.StatusUnauthorized || got.contentType != "application/problem+json" {
		t.Errorf("GET /settings without a credential = %d %q %s", got.status, got.contentType, got.body)
	}
	// Nor with one that does not allow it.
	if got := call(http.MethodPost, "/api/v1/users", rootBearer, map[string]any{"name": "reader", "roles": []string{iam.RoleReadOnly}}); got.status != http.StatusCreated {
		t.Fatalf("create a user = %d %s", got.status, got.body)
	}
	var created struct {
		Key struct {
			AccessKeyID string `json:"access_key_id"`
		} `json:"key"`
		SecretKey string `json:"secret_key"`
	}
	if got := call(http.MethodPost, "/api/v1/users/reader/keys", rootBearer, map[string]any{}); got.status != http.StatusCreated || json.Unmarshal(got.body, &created) != nil {
		t.Fatalf("create a key = %d %s", got.status, got.body)
	}
	refused := call(http.MethodGet, "/api/v1/settings", created.Key.AccessKeyID+":"+created.SecretKey, nil)
	var problem struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	}
	if refused.status != http.StatusForbidden || refused.contentType != "application/problem+json" ||
		json.Unmarshal(refused.body, &problem) != nil || problem.Status != http.StatusForbidden || problem.Code != "AccessDenied" {
		t.Errorf("GET /settings as a read-only user = %d %q %s", refused.status, refused.contentType, refused.body)
	}
	wantNoneOf(t, "the refusal", string(refused.body), marked)

	// The root credentials may.
	got := call(http.MethodGet, "/api/v1/settings", rootBearer, nil)
	if got.status != http.StatusOK {
		t.Fatalf("GET /settings = %d %s", got.status, got.body)
	}
	wantNoneOf(t, "the answer of GET /settings", string(got.body), marked)
	if strings.Contains(string(got.body), rootID) || strings.Contains(string(got.body), secretPlaceholder) {
		t.Errorf("the answer of GET /settings names the root access key, or what stands for a secret inside the node:\n%s", got.body)
	}
	var view settingsAnswer
	if err := json.Unmarshal(got.body, &view); err != nil {
		t.Fatalf("GET /settings: %v\n%s", err, got.body)
	}
	if len(view.Nodes) != 1 || !view.Nodes[0].Answered || view.Nodes[0].NodeID == "" || view.AnsweredBy != view.Nodes[0].NodeID {
		t.Fatalf("the answer does not hold this node, answered: %s", got.body)
	}
	// A node on its own is a cluster of one, not a deployment of nothing:
	// one node, which is the one voter of its metadata group and leads it,
	// writing at the level its metadata store writes at - though it
	// registers no node, itself included.
	if d := view.Cluster; d.Nodes != 1 || d.Voters != 1 || d.MetadataLevel != raftmeta.MetadataVersion || d.Leader != view.AnsweredBy {
		t.Errorf("the deployment of a node on its own = %+v; want one node, one voter, the level %d, led by %s",
			d, raftmeta.MetadataVersion, view.AnsweredBy)
	}
	node := view.Nodes[0]
	if !slices.Equal(node.Roles, []string{"gateway", "meta", "storage", "console"}) || node.Labels["zone"] != "zone-a" || len(node.Labels) != 1 {
		t.Errorf("roles %v, labels %v", node.Roles, node.Labels)
	}
	if node.Server.Version != buildinfo.Get().Version || !strings.HasPrefix(node.KMSKeyID, "local:") {
		t.Errorf("version %q, KMS key %q", node.Server.Version, node.KMSKeyID)
	}

	// What is no secret is there, with where it came from.
	type shown struct {
		source string
		set    bool
		value  string
	}
	settings := map[string]shown{}
	for _, setting := range node.Settings {
		settings[setting.Name] = shown{source: setting.Source, set: setting.Set, value: setting.Value}
	}
	if len(settings) != len(serverSettings()) {
		t.Errorf("%d settings in the answer, %d declared", len(settings), len(serverSettings()))
	}
	for name, want := range map[string]shown{
		"data":           {source: "flag", set: true, value: strings.Join(drives, ",")},
		"drive-labels":   {source: "flag", set: true, value: "vol-one,vol-two"},
		"admin-addr":     {source: "flag", set: true, value: adminAddr},
		"s3-addr":        {source: "environment", set: true, value: s3Addr},
		"scrub-interval": {source: "environment", set: true, value: "36h"},
		"gc-grace":       {source: "default", set: true, value: "24h"},
		"meta-dir":       {source: "derived", set: true, value: filepath.Join(drives[0], metaDirName)},
		"tls-cert":       {source: "flag", set: true, value: certFile},
		"topology-zone":  {source: "flag", set: true, value: "zone-a"},
		"otlp-endpoint":  {source: "flag", set: true, value: collector},
		// Of a secret, and of the file that holds one: that it is set.
		"kms-passphrase":      {source: "environment", set: true},
		"root-access-key":     {source: "environment", set: true},
		"root-secret-key":     {source: "environment", set: true},
		"tls-key":             {source: "flag", set: true},
		"kms-key-file":        {source: "flag", set: true},
		"kms-passphrase-file": {source: "default"},
		"vault-token-file":    {source: "default"},
	} {
		if settings[name] != want {
			t.Errorf("%s = %+v, want %+v", name, settings[name], want)
		}
	}

	// The drives, each with the label of its place in --data.
	if len(node.Drives) != 2 {
		t.Fatalf("drives: %s", got.body)
	}
	labels := map[string]string{}
	for _, drive := range node.Drives {
		labels[drive.Directory] = drive.Label
		if drive.ID == "" || !drive.Online || drive.TotalBytes <= 0 || drive.ErasureSet == nil || *drive.ErasureSet != 0 {
			t.Errorf("drive %+v", drive)
		}
	}
	if labels[drives[0]] != "vol-one" || labels[drives[1]] != "vol-two" {
		t.Errorf("labels by directory: %v", labels)
	}
	if node.Metadata.Directory != filepath.Join(drives[0], metaDirName) || node.Metadata.TotalBytes <= 0 {
		t.Errorf("metadata volume %+v", node.Metadata)
	}

	// The certificate is the one a client is shown.
	if got.shown == nil || len(node.Certificates) != 1 {
		t.Fatalf("certificates in the answer: %+v; shown to the client: %v", node.Certificates, got.shown)
	}
	if served := node.Certificates[0]; served.Use != cluster.CertificateS3 || served.Subject != got.shown.Subject.String() ||
		!slices.Equal(served.IPAddresses, []string{"127.0.0.1"}) {
		t.Errorf("certificate in the answer %+v, the client was shown %q", served, got.shown.Subject.String())
	}

	if err := halt(); err != nil {
		wantNoneOf(t, "the error the node stopped with", err.Error(), marked)
		t.Errorf("the node stopped with %v", err)
	}
	wantNoneOf(t, "the log of the node", text(logs), marked)
	wantNoneOf(t, "what the node told its operator", text(notice), marked)
	if !strings.Contains(text(logs), `"msg":"node started"`) {
		t.Errorf("the log that was searched is not the node's:\n%s", text(logs))
	}
}

// TestSecretFilesOfANodeAreNotShown: the same for what the node above could
// not be started with at the same time - a passphrase in a file, and Vault
// with its token file. Neither the path of such a file, in a directory whose
// name is marked, nor what a URL carries beside the address is in what the
// node says of itself.
func TestSecretFilesOfANodeAreNotShown(t *testing.T) {
	t.Parallel()
	mark := settingsMark()

	t.Run("a passphrase file", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		secretDir := filepath.Join(base, "holds-"+mark)
		if err := os.Mkdir(secretDir, 0o700); err != nil {
			t.Fatal(err)
		}
		passphraseFile, passphrase := filepath.Join(secretDir, "passphrase"), mark+"-in-a-file"
		writeTestFile(t, passphraseFile, passphrase+"\n")
		parsed, sources, err := parseServerSettings([]string{
			"--dev-plaintext", "--data", filepath.Join(base, "d1"), "--kms", "local", "--kms-passphrase-file", passphraseFile,
			"--kms-key-file", filepath.Join(secretDir, "kms.json"),
		}, noEnv, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		cfg := nodeConfig(base)
		cfg.Data, cfg.MetaDir, cfg.KMSKeyFile, cfg.KMSPassphraseFile = parsed.Data, parsed.MetaDir, parsed.KMSKeyFile, parsed.KMSPassphraseFile
		cfg.secrets, cfg.sources = secrets{}, sources
		if err := local.Create(cfg.KMSKeyFile, []byte(passphrase), local.Params{Time: 1, MemoryKiB: 8 << 10, Threads: 1}, rand.Reader); err != nil {
			t.Fatal(err)
		}
		started := mustStart(t, cfg)

		said, err := started.node.local.Local(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(said)
		if err != nil {
			t.Fatal(err)
		}
		marked := map[string]string{"the mark": mark, "the path of the passphrase file": passphraseFile}
		wantNoneOf(t, "what the node says of itself", string(encoded), marked)
		// The key file and its passphrase were there before the node:
		// nothing of either is in its log.
		wantNoneOf(t, "the log of the node", started.logs, marked)
		for _, setting := range said.Settings {
			if setting.Name == "kms-passphrase-file" && (!setting.Set || setting.Value != "" || setting.Source != cluster.SourceFlag) {
				t.Errorf("the passphrase file is shown as %+v", setting)
			}
			if setting.Name == "kms-passphrase" && setting.Set {
				t.Errorf("a passphrase that was not given is shown as %+v", setting)
			}
		}
		if len(said.Certificates) != 0 || said.NodeID != started.node.id || said.NodeID == "" {
			t.Errorf("a node on its own without TLS: certificates %v, node %q", said.Certificates, said.NodeID)
		}
	})

	t.Run("Vault", func(t *testing.T) {
		t.Parallel()
		tokenFile := filepath.Join(t.TempDir(), "holds-"+mark, "token")
		cfg, sources, err := parseServerSettings([]string{
			"--dev-plaintext", "--data", t.TempDir(), "--kms", "vault", "--vault-key", "cactusfs",
			"--vault-addr", "https://operator:" + mark + "@vault.test:8200/?wrap=" + mark, "--vault-token-file", tokenFile,
		}, noEnv, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		cfg.sources = sources
		shown := shownSettings(cfg)
		encoded, err := json.Marshal(shown)
		if err != nil {
			t.Fatal(err)
		}
		wantNoneOf(t, "the settings of a node with Vault", string(encoded), map[string]string{"the mark": mark, "the path of the token file": tokenFile})
		for _, setting := range shown {
			switch setting.Name {
			case "vault-addr":
				if setting.Value != "https://vault.test:8200/" {
					t.Errorf("the address of Vault is shown as %q", setting.Value)
				}
			case "vault-token-file":
				if !setting.Set || setting.Value != "" {
					t.Errorf("the token file is shown as %+v", setting)
				}
			}
		}
	})
}

// TestARefusedCollectorIsNamedWithoutItsCredential: a node told to send its
// traces to a URL with userinfo, a query or a fragment does not start, and
// what it tells its operator - on standard error, and in its log - repeats
// none of them, whatever else is wrong with the URL.
func TestARefusedCollectorIsNamedWithoutItsCredential(t *testing.T) {
	t.Parallel()
	mark := settingsMark()
	for name, endpoint := range map[string]string{
		"userinfo":                     "https://tracer:" + mark + "@collector.test:4318/v1/traces",
		"a query":                      "https://collector.test:4318/v1/traces?token=" + mark,
		"a fragment":                   "https://collector.test:4318/v1/traces#" + mark,
		"all of them, and no TLS":      "http://u:" + mark + "@example.com:4318/?token=" + mark + "#" + mark,
		"all of them, and no URL":      "u:" + mark + "@collector .test/%zz?" + mark,
		"a variable, not a flag":       "https://tracer:" + mark + "@collector.test:4318",
		"a password that holds a mark": "https://u:" + mark + "/?#@collector.test:4318",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			args, env := storeArgs(t, "--dev-plaintext", "--admin-addr", "127.0.0.1:0"), map[string]string{envKMSPhrase: "unit test passphrase"}
			if name == "a variable, not a flag" {
				env[envOTLPEndpoint] = endpoint
			} else {
				args = append(args, "--otlp-endpoint", endpoint)
			}
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), append([]string{"server"}, args...), &stdout, &stderr, envMap(env))
			if code != exitError || !strings.Contains(stderr.String(), "credentials do not belong in the endpoint URL") {
				t.Errorf("exit code %d, standard error %q", code, stderr.String())
			}
			marked := map[string]string{"the mark": mark}
			wantNoneOf(t, "standard error", stderr.String(), marked)
			wantNoneOf(t, "standard output, where the log goes", stdout.String(), marked)
		})
	}
}

// TestEverySettingThatHoldsASecretIsHidden goes by the list of settings and
// not by a list of its own: every setting declared a secret, or the path of
// a file that holds one, is given a marked value - a secret through its
// variable, which is the only way one arrives - and every setting declared a
// URL one with marked userinfo, query and fragment. The node then says its
// settings as it does when asked (newLocalSettings, Local), the management
// API answers with them, and no mark is in either. A setting that is added
// with one of these kinds is covered by being declared; one that holds a
// secret under another kind is what TestSettingsClassification is for.
func TestEverySettingThatHoldsASecretIsHidden(t *testing.T) {
	t.Parallel()
	mark := settingsMark()
	const shownURL = "https://collector.test:4318/v1/traces"
	markedURL := "https://user-" + mark + ":pass-" + mark + "@collector.test:4318/v1/traces?token=" + mark + "#frag-" + mark

	// The configuration is filled below what refuses a combination of
	// settings, or a URL with a credential: what is tested here is that
	// nothing is shown, whatever was let through.
	var cfg serverConfig
	cfg.sources = settingSources{}
	targets := settingTargets{cfg: &cfg}
	flags := flag.NewFlagSet("cactusfs server", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	marked := map[string]string{"the mark": mark}
	secrets, urls := map[string]bool{}, map[string]bool{}
	for _, spec := range serverSettings() {
		value := ""
		switch spec.Kind {
		case cluster.SettingSecret:
			value = mark + "-value-of-" + spec.Name
		case cluster.SettingSecretFile:
			value = filepath.Join(string(filepath.Separator), "holds-"+mark, spec.Name)
		case cluster.SettingURL:
			value = markedURL
		case cluster.SettingPlain, cluster.SettingPath:
		}
		source, err := spec.declare(flags, &targets, envMap(map[string]string{spec.Env: value}))
		if err != nil {
			t.Fatalf("%s: %v", spec.Name, err)
		}
		cfg.sources[spec.Name] = source
		if value == "" {
			continue
		}
		kept, isText := spec.bind(&targets).(*string)
		if !isText {
			t.Fatalf("%s is of kind %q and is kept in a %T", spec.Name, spec.Kind, spec.bind(&targets))
		}
		if *kept != value || source != cluster.SourceEnvironment {
			t.Fatalf("%s was given a marked value through its variable and holds %q from %q", spec.Name, *kept, source)
		}
		if spec.Flag {
			// And through its flag, as it is given on a command line.
			*kept = ""
			if err := flags.Set(spec.Name, value); err != nil || *kept != value {
				t.Fatalf("--%s: %v; it holds %q", spec.Name, err, *kept)
			}
			cfg.sources[spec.Name] = cluster.SourceFlag
		}
		marked["the value of "+spec.Name] = value
		if spec.Kind == cluster.SettingURL {
			urls[spec.Name] = true
		} else {
			secrets[spec.Name] = true
		}
	}
	// The list is what is tested: an empty one, or one that lost a kind,
	// would pass everything above.
	for _, name := range []string{
		"kms-passphrase", "root-access-key", "root-secret-key", "tls-key", "node-tls-key", "kms-key-file", "kms-passphrase-file", "vault-token-file",
	} {
		if !secrets[name] {
			t.Errorf("%s was not given a marked value: it is no longer declared a secret, or a file that holds one", name)
		}
	}
	for _, name := range []string{"otlp-endpoint", "vault-addr"} {
		if !urls[name] {
			t.Errorf("%s was not given a marked URL: it is no longer declared a URL", name)
		}
	}
	if cfg.secrets.kmsPassphrase == "" || cfg.OTLPEndpoint != markedURL || !strings.Contains(cfg.TLSKey, mark) {
		t.Fatal("the configuration does not hold what it was given")
	}

	// What the node says of itself, as it is made when a node starts.
	started := mustStart(t, nodeConfig(t.TempDir()))
	said, err := newLocalSettings(cfg, started.node.drives, "local:test", time.Now()).Local(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(said)
	if err != nil {
		t.Fatal(err)
	}
	wantNoneOf(t, "what the node says of itself", string(encoded), marked)
	if strings.Contains(string(encoded), secretPlaceholder) {
		t.Errorf("what the node says of itself holds what stands for a secret inside it:\n%s", encoded)
	}

	// And what the management API answers when a node says that: the
	// handler the server mounts, over the node that was started.
	n := started.node
	n.local.settings = said.Settings
	handler := adminapi.NewHandler(adminapi.Config{
		Buckets: n.buckets, Objects: n.objects, Search: n.search, IAM: n.iam, Sessions: n.sessions, Cluster: n.cluster,
		Settings: n.settings, Idempotency: n.idempotency, Identities: n.identities, Clock: n.clock,
		Logger: slog.New(slog.DiscardHandler), SpecYAML: openapi.AdminYAML(), SpecJSON: openapi.AdminJSON(), Build: buildinfo.Get(),
	})
	credentials := shownCredentials(t, started.notice)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, adminapi.BasePath+"/settings", nil)
	request.Header.Set("Authorization", "Bearer "+credentials.AccessKeyID+":"+credentials.SecretKey)
	answer := httptest.NewRecorder()
	handler.ServeHTTP(answer, request)
	body := answer.Body.String()
	if answer.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d %s", answer.Code, body)
	}
	wantNoneOf(t, "the answer of GET /settings", body, marked)
	if strings.Contains(body, secretPlaceholder) {
		t.Errorf("the answer of GET /settings holds what stands for a secret inside the node:\n%s", body)
	}
	var view settingsAnswer
	if err := json.Unmarshal([]byte(body), &view); err != nil || len(view.Nodes) != 1 || !view.Nodes[0].Answered {
		t.Fatalf("GET /settings: %v\n%s", err, body)
	}
	seen := 0
	for _, setting := range view.Nodes[0].Settings {
		switch {
		case secrets[setting.Name]:
			seen++
			if !setting.Set || setting.Value != "" {
				t.Errorf("%s, a secret that is set, is shown as %+v", setting.Name, setting)
			}
		case urls[setting.Name]:
			seen++
			if !setting.Set || setting.Value != shownURL {
				t.Errorf("%s is shown as %+v, want %s", setting.Name, setting, shownURL)
			}
		}
	}
	if seen != len(secrets)+len(urls) || len(view.Nodes[0].Settings) != len(serverSettings()) {
		t.Errorf("%d of the %d marked settings are in the answer, which has %d settings of %d declared",
			seen, len(secrets)+len(urls), len(view.Nodes[0].Settings), len(serverSettings()))
	}
}

// TestSettingsOfFourNodes: any node of a cluster answers for all of them -
// each with its own drives, directories and labels and the certificate it
// shows its peers - by asking the others over node RPC, through the decorated
// port; nothing secret is in what one node sends another; and a node that is
// down is said to be unreachable without holding up the answer. (PLAN H.9.)
func TestSettingsOfFourNodes(t *testing.T) {
	t.Parallel()
	c := newTestCluster(t, fourNodes())
	for id, cfg := range c.configs {
		cfg.DriveLabels = []string{"claim1-" + id, "claim2-" + id}
		c.configs[id] = cfg
	}
	c.start(c.all()...)

	// The port the others are asked through is decorated, like every other.
	wantWired(t, c.node("n4"), append([]string{"ports.NodeSettingsReader"}, wiredKinds...)...)

	check := func(view app.SettingsView, asked string, down ...string) {
		t.Helper()
		if view.AnsweredBy != asked || len(view.Nodes) != 4 || view.Cluster.Nodes != 4 || view.Cluster.Voters != 3 ||
			view.Cluster.MetadataLevel != raftmeta.MetadataVersion {
			t.Fatalf("asked of %s: answered by %q, %d nodes: %+v", asked, view.AnsweredBy, len(view.Nodes), view.Cluster)
		}
		for i, answer := range view.Nodes {
			id := []string{"n1", "n2", "n3", "n4"}[i]
			cfg := c.configs[id]
			if answer.NodeID != id {
				t.Fatalf("node %d of the answer is %q", i, answer.NodeID)
			}
			if slices.Contains(down, id) {
				if answer.Answered || answer.Reason != cluster.ReasonUnreachable || len(answer.Settings.Settings) != 0 {
					t.Errorf("%s is down and is shown as answered %t, reason %q", id, answer.Answered, answer.Reason)
				}
				continue
			}
			if !answer.Answered || answer.Reason != cluster.ReasonNone {
				t.Errorf("%s: answered %t, reason %q", id, answer.Answered, answer.Reason)
				continue
			}
			said := answer.Settings
			if said.NodeID != id || !slices.Equal(said.Roles, strings.Split(cfg.Roles.String(), ",")) || said.Labels["zone"] != cfg.Labels.Zone ||
				said.Build.Version != buildinfo.Get().Version || !strings.HasPrefix(said.KMSKeyID, "local:") || said.StartedAt.IsZero() {
				t.Errorf("%s says of itself: %+v", id, said)
			}
			if len(said.Settings) != len(serverSettings()) || said.Metadata.Directory != cfg.MetaDir {
				t.Errorf("%s: %d settings, metadata in %q", id, len(said.Settings), said.Metadata.Directory)
			}
			// Its own drives: by their directories and labels, each in a
			// set of the topology.
			labels, ids := map[string]string{}, map[string]bool{}
			for _, drive := range said.Drives {
				labels[drive.Directory] = drive.Label
				ids[drive.ID] = true
				if !drive.Online || drive.ErasureSet != 0 {
					t.Errorf("%s: drive %+v", id, drive)
				}
			}
			if len(said.Drives) != 2 || labels[cfg.Data[0]] != "claim1-"+id || labels[cfg.Data[1]] != "claim2-"+id {
				t.Errorf("%s: drives %+v", id, said.Drives)
			}
			if own := c.node(id); own != nil {
				for _, drive := range own.drives {
					if !ids[drive.DriveID()] {
						t.Errorf("%s: its drive %s is not in what it says", id, drive.DriveID())
					}
				}
			}
			// The certificate it shows its peers, which names it.
			if len(said.Certificates) != 1 || said.Certificates[0].Use != cluster.CertificateNode ||
				!slices.Contains(said.Certificates[0].DNSNames, id) || said.Certificates[0].NotAfter.IsZero() {
				t.Errorf("%s: certificates %+v", id, said.Certificates)
			}
		}
	}

	view, err := c.node("n4").settings.Get(t.Context(), root())
	if err != nil {
		t.Fatal(err)
	}
	check(view, "n4")
	if len(view.Mismatches) != 0 {
		t.Errorf("four nodes configured alike differ in %+v", view.Mismatches)
	}
	if len(view.Cluster.ErasureSets) != 1 || view.Cluster.ErasureSets[0].Drives != 8 || view.Cluster.Leader == "" {
		t.Errorf("the deployment: %+v", view.Cluster)
	}

	// What one node sends another holds no secret and no path of a file
	// that holds one, whoever asks.
	for _, id := range []string{"n1", "n2", "n3"} {
		cfg := c.configs[id]
		sent, err := c.node("n4").rpc.NodeSettings(t.Context(), id)
		if err != nil {
			t.Fatalf("ask %s: %v", id, err)
		}
		encoded, err := json.Marshal(sent)
		if err != nil {
			t.Fatal(err)
		}
		wantNoneOf(t, "what "+id+" sends another node", string(encoded), map[string]string{
			"the KMS passphrase": nodePassphrase, "the path of its key": cfg.NodeTLSKey, "the path of the KMS key file": cfg.KMSKeyFile,
			"what stands for a secret inside the node": secretPlaceholder,
		})
		if !strings.Contains(string(encoded), cfg.Data[0]) || !strings.Contains(string(encoded), cfg.NodeTLSCert) {
			t.Errorf("what %s sends another node lacks its drive or its certificate file:\n%s", id, encoded)
		}
	}

	// A node that is down costs its own time and is said to be down.
	c.stop("n2")
	began := time.Now()
	view, err = c.node("n1").settings.Get(t.Context(), root())
	if err != nil {
		t.Fatal(err)
	}
	if took, limit := time.Since(began), app.DefaultSettingsNodeTimeout+2*time.Second; took > limit {
		t.Errorf("the answer with a node down took %s, want it within %s", took, limit)
	}
	check(view, "n1", "n2")
}

// TestAVolumeThatHangsDoesNotHoldUpTheSettings: the node says what it knows
// of a volume that does not answer - where it is, not how much room it has -
// within the one deadline all its volumes share, and does not ask that
// volume again while the first question is out: whoever asks meanwhile waits
// for the one answer, no longer than the same deadline.
func TestAVolumeThatHangsDoesNotHoldUpTheSettings(t *testing.T) {
	t.Parallel()
	started := mustStart(t, nodeConfig(t.TempDir()))
	local := started.node.local

	whole, err := local.Local(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if whole.Metadata.TotalBytes == 0 || len(whole.Drives) != 2 || !whole.Drives[0].Online || whole.Drives[0].TotalBytes == 0 {
		t.Fatalf("a node whose volumes answer: %+v", whole)
	}

	// The second drive hangs, as a mount that has gone away does.
	release, asked := make(chan struct{}), make(chan struct{}, 8)
	answers := local.drives[1].probe.ask
	local.drives[1].probe.ask = func() volumeFacts {
		asked <- struct{}{}
		<-release
		return answers()
	}
	began := time.Now()
	partial, err := local.Local(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took < volumeProbeTimeout || took > volumeProbeTimeout+2*time.Second {
		t.Errorf("the answer with a volume that hangs took %s, want about %s", took, volumeProbeTimeout)
	}
	if hung := partial.Drives[1]; hung.Online || hung.TotalBytes != 0 || hung.ID != whole.Drives[1].ID || hung.Directory != whole.Drives[1].Directory {
		t.Errorf("the drive that hangs is shown as %+v", hung)
	}
	if !partial.Drives[0].Online || partial.Metadata.TotalBytes == 0 {
		t.Errorf("the volumes that answer are not shown: %+v", partial)
	}

	// Asked again while the question is out, the node waits for that one
	// answer against a deadline of its own, and puts no second question.
	began = time.Now()
	if again, err := local.Local(t.Context()); err != nil || again.Drives[1].Online || again.Drives[1].TotalBytes != 0 || !again.Drives[0].Online {
		t.Errorf("asked again: %+v, %v", again.Drives, err)
	}
	if took := time.Since(began); took < volumeProbeTimeout || took > volumeProbeTimeout+2*time.Second {
		t.Errorf("asked again, the answer took %s, want about %s", took, volumeProbeTimeout)
	}
	if len(asked) != 1 {
		t.Errorf("the drive that hangs was asked %d times", len(asked))
	}

	// A caller that has gone is not answered.
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := local.Local(gone); err == nil {
		t.Error("a caller that had gone away was answered")
	}

	// The drive answers again, and is shown again.
	close(release)
	for began := time.Now(); ; time.Sleep(10 * time.Millisecond) {
		back, err := local.Local(t.Context())
		if err == nil && back.Drives[1].Online {
			break
		}
		if time.Since(began) > 10*time.Second {
			t.Fatalf("the drive that answers again is still shown as %+v (%v)", back.Drives[1], err)
		}
	}
}

// localAnswers asks the node for its settings from several goroutines at
// once, each so many times, and returns every answer.
func localAnswers(t *testing.T, local *localSettings, callers, times int) []cluster.NodeSettings {
	t.Helper()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		said []cluster.NodeSettings
	)
	for range callers {
		wg.Go(func() {
			for range times {
				settings, err := local.Local(t.Context())
				if err != nil {
					t.Errorf("settings.Local: %v", err)
					return
				}
				mu.Lock()
				said = append(said, settings)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return said
}

// TestConcurrentCallersSeeTheSameVolumes: two consoles, or two gateways that
// ask the same node at the same moment, are told the same of its volumes. A
// caller that comes while a question is out waits for that answer - it is
// not told that the drive could not be reached - and a drive that hangs is
// asked once, whoever waits for it.
func TestConcurrentCallersSeeTheSameVolumes(t *testing.T) {
	t.Parallel()
	started := mustStart(t, nodeConfig(t.TempDir()))
	local := started.node.local
	const callers = 8

	answers := localAnswers(t, local, callers, 200)
	if len(answers) != callers*200 {
		t.Fatalf("%d answers", len(answers))
	}
	for _, said := range answers {
		if len(said.Drives) != 2 || said.Metadata.TotalBytes == 0 {
			t.Fatalf("a node whose volumes answer: %+v", said)
		}
		for _, drive := range said.Drives {
			if !drive.Online || drive.TotalBytes == 0 {
				t.Fatalf("a drive that answers is shown as %+v", drive)
			}
		}
	}

	// The second drive hangs: it alone is shown as not reached, to every
	// caller, and one question is out to it.
	var asked atomic.Int64
	release, answer := make(chan struct{}), local.drives[1].probe.ask
	local.drives[1].probe.ask = func() volumeFacts {
		asked.Add(1)
		<-release
		return answer()
	}
	began := time.Now()
	answers = localAnswers(t, local, callers, 1)
	if took := time.Since(began); took < volumeProbeTimeout || took > volumeProbeTimeout+2*time.Second {
		t.Errorf("the answers with a volume that hangs took %s, want about %s", took, volumeProbeTimeout)
	}
	if len(answers) != callers {
		t.Fatalf("%d answers", len(answers))
	}
	for _, said := range answers {
		if hung := said.Drives[1]; hung.Online || hung.TotalBytes != 0 || hung.FreeBytes != 0 || hung.Directory == "" {
			t.Errorf("the drive that hangs is shown as %+v", hung)
		}
		if !said.Drives[0].Online || said.Drives[0].TotalBytes == 0 || said.Metadata.TotalBytes == 0 {
			t.Errorf("the volumes that answer are not shown: %+v", said)
		}
	}
	if got := asked.Load(); got != 1 {
		t.Errorf("the drive that hangs was asked %d times, want once", got)
	}

	// Callers that wait when the drive answers are all given that answer.
	waiting := make(chan []cluster.NodeSettings, 1)
	go func() { waiting <- localAnswers(t, local, callers, 1) }()
	time.Sleep(volumeProbeTimeout / 10)
	close(release)
	for _, said := range <-waiting {
		if !said.Drives[1].Online || said.Drives[1].TotalBytes == 0 {
			t.Errorf("the drive that answered while its callers waited is shown as %+v", said.Drives[1])
		}
	}
	if got := asked.Load(); got != 1 {
		t.Errorf("the drive was asked %d times for the one answer", got)
	}
	// And it is asked again from then on.
	for _, said := range localAnswers(t, local, callers, 20) {
		if !said.Drives[1].Online {
			t.Fatalf("the drive that answers again is shown as %+v", said.Drives[1])
		}
	}
	if got := asked.Load(); got < 2 {
		t.Errorf("the drive that answers again was not asked again: %d questions", got)
	}
}

// TestBytesOf: the room the operating system reports, as whole bytes.
func TestBytesOf(t *testing.T) {
	t.Parallel()
	for room, want := range map[float64]uint64{0: 0, -1: 0, 1.9: 1, 1 << 40: 1 << 40, 1e30: 1<<64 - 1} {
		if got := bytesOf(room); got != want {
			t.Errorf("bytesOf(%v) = %d, want %d", room, got, want)
		}
	}
	if got := bytesOf(math.NaN()); got != 0 {
		t.Errorf("bytesOf(NaN) = %d", got)
	}
}

// TestTheMetadataLevelOfTheSettingsIsTheStores: what the settings service is
// given to learn the metadata level with answers what the store writes at
// while the store is open, and that it cannot say - never a level of its own
// making - once the store is closed; a caller who went away is not kept.
func TestTheMetadataLevelOfTheSettingsIsTheStores(t *testing.T) {
	t.Parallel()
	n := mustStart(t, nodeConfig(t.TempDir())).node
	level := metadataLevel(n.meta)
	if got, err := level(t.Context()); err != nil || got != raftmeta.MetadataVersion {
		t.Errorf("the level of a node on its own = %d, %v; want %d", got, err, raftmeta.MetadataVersion)
	}
	view, err := n.settings.Get(t.Context(), root())
	if err != nil {
		t.Fatal(err)
	}
	if d := view.Cluster; d.Nodes != 1 || d.Voters != 1 || d.MetadataLevel != raftmeta.MetadataVersion || len(view.Nodes) != 1 {
		t.Errorf("the deployment of a node on its own = %+v, with %d nodes listed", d, len(view.Nodes))
	}

	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := level(t.Context()); !errors.Is(err, domain.ErrUnavailable) || got != 0 {
		t.Errorf("the level of a closed store = %d, %v; want none, and %v", got, err, domain.ErrUnavailable)
	}
}
