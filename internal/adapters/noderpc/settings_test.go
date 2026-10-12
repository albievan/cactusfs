package noderpc_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"

	"github.com/albievan/cactusfs/internal/adapters/noderpc"
	nodev1 "github.com/albievan/cactusfs/internal/adapters/noderpc/nodev1/cactusfs/node/v1"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/cluster"
	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

const settingsMethod = "/cactusfs.node.v1.NodeService/Settings"

// serveRaw is a gRPC server on loopback with the node certificate of nodeID
// and the services the test registers: a node that is not this release, or
// not honest.
func serveRaw(t *testing.T, p *pki, nodeID string, register func(*grpc.Server)) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(p.identity(nodeID).Server())))
	register(server)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

// serveOldRelease is a node of a release from before the Settings call: its
// NodeService is the one of the contract without that method, so that what
// answers the call is gRPC itself, as on a real node of that release.
func serveOldRelease(t *testing.T, p *pki, nodeID string) string {
	t.Helper()
	return serveRaw(t, p, nodeID, func(server *grpc.Server) {
		old := nodev1.NodeService_ServiceDesc
		old.Methods = slices.DeleteFunc(slices.Clone(old.Methods), func(m grpc.MethodDesc) bool { return m.MethodName == "Settings" })
		if len(old.Methods) != len(nodev1.NodeService_ServiceDesc.Methods)-1 {
			t.Fatal("the contract has no Settings method to leave out")
		}
		server.RegisterService(&old, nodev1.UnimplementedNodeServiceServer{})
	})
}

// rawSettings asks a node for its settings as gRPC delivers them: the
// message the node encoded, before any client of this package has looked at
// it.
func rawSettings(t *testing.T, p *pki, nodeID, address string) *nodev1.SettingsResponse {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///"+address,
		grpc.WithTransportCredentials(credentials.NewTLS(p.identity("gateway-1").Client(nodeID))),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	resp, err := nodev1.NewNodeServiceClient(conn).Settings(ctx, &nodev1.SettingsRequest{})
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	return resp
}

func withSettings(source ports.LocalSettings) func(*noderpc.ServerConfig) {
	return func(cfg *noderpc.ServerConfig) { cfg.Settings = source }
}

// TestNodeSettingsHonourTheContract: the settings of a node on the other
// side of mutual TLS are read as the port promises - with real nodes behind
// the names of the script: one that answers, one of a release without the
// call, one that never answers, one that cannot say and one that is not
// there.
func TestNodeSettingsHonourTheContract(t *testing.T) {
	t.Parallel()
	porttest.NodeSettingsContract(t, func(t *testing.T, script porttest.NodeSettingsScript) ports.NodeSettingsReader {
		t.Helper()
		p := newPKI(t)
		answering := serveWith(t, p, porttest.SettingsNodeAnswering, withSettings(func(context.Context) (cluster.NodeSettings, error) {
			return script.Settings, nil
		}))
		silent := serveWith(t, p, porttest.SettingsNodeSilent, withSettings(func(ctx context.Context) (cluster.NodeSettings, error) {
			<-ctx.Done()
			return cluster.NodeSettings{}, ctx.Err()
		}))
		failing := serveWith(t, p, porttest.SettingsNodeFailing, withSettings(func(context.Context) (cluster.NodeSettings, error) {
			return cluster.NodeSettings{}, errors.New("the drives cannot be read")
		}))
		return clientFor(t, p.identity("gateway-1"), map[string]string{
			porttest.SettingsNodeAnswering: answering.address,
			porttest.SettingsNodeOld:       serveOldRelease(t, p, porttest.SettingsNodeOld),
			porttest.SettingsNodeSilent:    silent.address,
			porttest.SettingsNodeFailing:   failing.address,
		})
	})
}

// TestSettingsAreServedToNodesOnly: the call is one of node RPC - in the set
// of methods that are counted, behind the handshake and behind the
// authorizer like every other - and a node that was given nothing to say
// says so instead of pretending to be an older release.
func TestSettingsAreServedToNodesOnly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	if !slices.Contains(noderpc.Methods(), settingsMethod) {
		t.Fatalf("%s is not among the methods of node RPC: %v", settingsMethod, noderpc.Methods())
	}

	p, foreign := newPKI(t), newPKI(t)
	authz := &switchAuthorizer{}
	var asked atomic.Int64
	const cause = "open /run/secrets/passphrase: permission denied"
	var fail atomic.Bool
	logs := &lockedLog{}
	node := serveWith(t, p, "storage-1", func(cfg *noderpc.ServerConfig) {
		cfg.Authz, cfg.Logger = authz, slog.New(slog.NewTextHandler(logs, nil))
		cfg.Settings = func(context.Context) (cluster.NodeSettings, error) {
			asked.Add(1)
			if fail.Load() {
				return cluster.NodeSettings{}, fmt.Errorf("%w: %s", domain.ErrKeyUnavailable, cause)
			}
			return cluster.NodeSettings{NodeID: "storage-1", KMSKeyID: "local:1"}, nil
		}
	})
	addresses := map[string]string{"storage-1": node.address, "storage-2": node.address}

	// A caller without a certificate of the node CA gets no connection.
	stranger := clientFor(t, foreign.identity("gateway-1"), addresses)
	if got, err := stranger.NodeSettings(ctx, "storage-1"); !errors.Is(err, domain.ErrUnavailable) || !reflect.DeepEqual(got, cluster.NodeSettings{}) {
		t.Errorf("NodeSettings with a certificate of another CA = %+v, %v; want the node to be unreachable", got, err)
	}
	if asked.Load() != 0 || len(authz.inner.Requests()) != 0 {
		t.Fatal("a stranger's call reached the node")
	}

	// A member is served, as the node its certificate names.
	member := clientFor(t, p.identity("gateway-7"), addresses)
	got, err := member.NodeSettings(ctx, "storage-1")
	if err != nil || got.NodeID != "storage-1" || got.KMSKeyID != "local:1" {
		t.Fatalf("NodeSettings = %+v, %v", got, err)
	}
	requests := authz.inner.Requests()
	if len(requests) != 1 || requests[0].Principal.NodeID != "gateway-7" || requests[0].Action != iam.ActionNodeRPC {
		t.Errorf("authorization requests = %+v, want one of gateway-7 for node RPC", requests)
	}
	// A node that answers as another than the one that was asked is not
	// believed.
	if _, err := member.NodeSettings(ctx, "storage-2"); err == nil {
		t.Error("the settings of storage-2 were taken from a node that is storage-1")
	}

	// What the authorizer refuses is not served. The node was reached, and
	// is of this release: the refusal is neither of the other reasons.
	authz.deny.Store(true)
	before := asked.Load()
	_, err = member.NodeSettings(ctx, "storage-1")
	if err == nil || errors.Is(err, domain.ErrNotSupported) || errors.Is(err, domain.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("NodeSettings although the authorizer refused = %v", err)
	}
	if asked.Load() != before {
		t.Error("a refused call reached the node's settings")
	}
	authz.deny.Store(false)

	// A node that cannot read its settings says that, and keeps why to its
	// own log.
	fail.Store(true)
	_, err = member.NodeSettings(ctx, "storage-1")
	if err == nil || strings.Contains(err.Error(), "/run/secrets") {
		t.Errorf("NodeSettings of a node that cannot say = %v", err)
	}
	if errors.Is(err, domain.ErrNotSupported) || errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("a node that cannot say is taken for another kind of failure: %v", err)
	}
	if !strings.Contains(logs.String(), cause) {
		t.Errorf("the node did not log why: %s", logs.String())
	}

	// A node that was given no settings to tell answers so. It is of this
	// release, and must not be taken for an older one.
	bare := serve(t, p, "storage-9")
	_, err = clientFor(t, p.identity("gateway-7"), map[string]string{"storage-9": bare.address}).NodeSettings(ctx, "storage-9")
	if err == nil || errors.Is(err, domain.ErrNotSupported) || errors.Is(err, domain.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("NodeSettings of a node without a source = %v, want a failure of no other kind", err)
	}
}

// lockedLog is a log that several goroutines write.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// TestSettingsCallsAreCounted: the interceptors count the call on both
// sides, by its method - which they know from the contract.
func TestSettingsCallsAreCounted(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	gateway, storage := observe(t, false), observe(t, false)
	served := storage.tel.NodeRPC(noderpc.Methods())
	node := serveWith(t, p, "storage-sentinel", func(cfg *noderpc.ServerConfig) {
		cfg.UnaryInterceptor, cfg.StreamInterceptor = served.UnaryServer(), served.StreamServer()
		cfg.Settings = func(context.Context) (cluster.NodeSettings, error) {
			return cluster.NodeSettings{NodeID: "storage-sentinel", Drives: []cluster.DriveSettings{{ID: "d", Volume: cluster.Volume{Directory: "/directory-sentinel"}}}}, nil
		}
	})
	calling := gateway.tel.NodeRPC(noderpc.Methods())
	client := clientFor(t, p.identity("gateway-sentinel"), map[string]string{"storage-sentinel": node.address}).
		WithInterceptors(calling.UnaryClient(), calling.StreamClient())
	reader := gateway.tel.InstrumentNodeSettings(client)
	if _, err := reader.NodeSettings(t.Context(), "storage-sentinel"); err != nil {
		t.Fatalf("NodeSettings: %v", err)
	}
	gateway.eventually(t, "cactusfs_noderpc_requests_total code=OK method=Settings side=client")
	storage.eventually(t, "cactusfs_noderpc_requests_total code=OK method=Settings side=server")
	gateway.eventually(t, "cactusfs_port_calls_total method=NodeSettings port=node_settings result=ok")
	// Neither the node nor what it said is a label.
	for _, observed := range []observedNode{gateway, storage} {
		for _, line := range observed.series(t) {
			if strings.Contains(line, "sentinel") {
				t.Errorf("a series names a node or a directory: %s", line)
			}
		}
	}
}

// settingsWireMark is in everything the node of the next two tests has that
// must not leave it.
const settingsWireMark = "MARK-9b41-MUST-NOT-CROSS-THE-WIRE"

// overfullSettings is what a node says that was configured by somebody who
// knew no bounds, through a source that redacted nothing: secrets with their
// values, far too much of everything, names and values far too long, and a
// directory that is not text.
func overfullSettings(nodeID string) cluster.NodeSettings {
	long := strings.Repeat("n", 10000) + settingsWireMark
	n := cluster.NodeSettings{
		NodeID: nodeID, KMSKeyID: long, Build: cluster.Build{Version: long, Build: long, Commit: long},
		StartedAt: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC),
		Labels:    map[string]string{},
		Metadata: cluster.Volume{
			Directory: "/meta/" + long, Label: long,
			Filesystem: cluster.Filesystem{Type: long, Device: long, MountPoint: long},
		},
		Settings: []cluster.Setting{
			// Made by hand: nothing has redacted these.
			{Name: "a-passphrase", Kind: cluster.SettingSecret, Scope: cluster.ScopeCluster, Source: cluster.SourceEnvironment, Value: settingsWireMark},
			{Name: "a-secret-file", Kind: cluster.SettingSecretFile, Scope: cluster.ScopeNode, Value: "/run/secrets/" + settingsWireMark},
			{Name: "a-url", Kind: cluster.SettingURL, Scope: cluster.ScopeCluster, Value: "https://user:" + settingsWireMark + "@otel:4318/v1?token=" + settingsWireMark + "#" + settingsWireMark},
			{Name: "a-kind-of-later", Kind: "later", Value: settingsWireMark},
			{Name: "a-kind-too-long", Kind: cluster.SettingKind("plain" + long), Value: settingsWireMark},
			{Name: "a-long-value", Kind: cluster.SettingPlain, Value: strings.Repeat("v", 3*cluster.MaxSettingValueBytes) + settingsWireMark},
			{Name: "a-name-" + long, Env: long, Flag: long, Group: long, Kind: cluster.SettingPlain, Scope: cluster.SettingScope(long), Source: cluster.SettingSource(long), Value: "1"},
			{Name: "a-value-that-is-not-text", Kind: cluster.SettingPath, Value: "/data/\xff\xfe/1"},
		},
	}
	for i := range 10000 {
		n.Settings = append(n.Settings, cluster.NewSetting(fmt.Sprintf("setting-%05d", i), "", "", "", cluster.SettingPlain, cluster.ScopeNode, cluster.SourceDefault, "v"))
	}
	for i := range 2000 {
		n.Drives = append(n.Drives, cluster.DriveSettings{ID: fmt.Sprintf("drive-%04d-%s", i, long), Volume: cluster.Volume{Directory: "/data/\xff/" + long}})
	}
	var names []string
	for i := range 500 {
		names = append(names, fmt.Sprintf("name-%03d-%s", i, long))
		n.Roles = append(n.Roles, fmt.Sprintf("role-%03d-%s", i, long))
		n.Labels[fmt.Sprintf("label-%03d-%s", i, long)] = long
	}
	for i := range 200 {
		n.Certificates = append(n.Certificates, cluster.Certificate{
			Use: long, Subject: fmt.Sprintf("CN=%03d-%s", i, strings.Repeat("s", 2*cluster.MaxSettingValueBytes)+settingsWireMark), Issuer: long,
			DNSNames: names, IPAddresses: names,
		})
	}
	return n
}

// withinBounds fails for everything of a node's settings that exceeds the
// bounds of the wire, or is not text.
func withinBounds(t *testing.T, what string, n cluster.NodeSettings) {
	t.Helper()
	count := func(of string, got, limit int) {
		t.Helper()
		if got > limit {
			t.Errorf("%s: %d %s, want at most %d", what, got, of, limit)
		}
	}
	text := func(of, s string, limit int) {
		t.Helper()
		if len(s) > limit || !utf8.ValidString(s) {
			t.Errorf("%s: %s of %d bytes, want text of at most %d", what, of, len(s), limit)
		}
	}
	short := func(of, s string) { t.Helper(); text(of, s, noderpc.MaxSettingsNameBytes) }
	value := func(of, s string) { t.Helper(); text(of, s, cluster.MaxSettingValueBytes) }
	volume := func(v cluster.Volume) {
		t.Helper()
		value("directory", v.Directory)
		short("label", v.Label)
		short("file system type", v.Filesystem.Type)
		value("device", v.Filesystem.Device)
		value("mount point", v.Filesystem.MountPoint)
	}

	count("settings", len(n.Settings), noderpc.MaxSettings)
	count("drives", len(n.Drives), noderpc.MaxSettingsDrives)
	count("certificates", len(n.Certificates), noderpc.MaxSettingsCertificates)
	count("labels", len(n.Labels), noderpc.MaxSettingsLabels)
	count("roles", len(n.Roles), noderpc.MaxSettingsLabels)
	short("node ID", n.NodeID)
	short("KMS key", n.KMSKeyID)
	short("version", n.Build.Version)
	short("build", n.Build.Build)
	short("commit", n.Build.Commit)
	volume(n.Metadata)
	for _, role := range n.Roles {
		short("role", role)
	}
	for label, v := range n.Labels {
		short("label", label)
		short("value of a label", v)
	}
	for _, s := range n.Settings {
		short("name", s.Name)
		short("variable", s.Env)
		short("flag", s.Flag)
		short("group", s.Group)
		short("kind", string(s.Kind))
		short("scope", string(s.Scope))
		short("source", string(s.Source))
		value("value", s.Value)
		if s.Kind != cluster.SettingPlain && s.Kind != cluster.SettingPath && s.Kind != cluster.SettingURL && s.Value != "" {
			t.Errorf("%s: setting %s of kind %.20s has a value", what, s.Name, s.Kind)
		}
	}
	for _, drive := range n.Drives {
		short("drive ID", drive.ID)
		volume(drive.Volume)
	}
	for _, c := range n.Certificates {
		short("use", c.Use)
		value("subject", c.Subject)
		value("issuer", c.Issuer)
		count("DNS names", len(c.DNSNames), noderpc.MaxCertificateNames)
		count("addresses", len(c.IPAddresses), noderpc.MaxCertificateNames)
		for _, name := range append(slices.Clone(c.DNSNames), c.IPAddresses...) {
			short("name of a certificate", name)
		}
	}
}

// decoded reads a response as a client of this package would, without what
// that client does to it afterwards: the message, field by field.
func decoded(resp *nodev1.SettingsResponse) cluster.NodeSettings {
	n := cluster.NodeSettings{
		NodeID: resp.GetNodeId(), KMSKeyID: resp.GetKmsKeyId(), Roles: resp.GetRoles(), Labels: resp.GetLabels(),
		Build: cluster.Build{Version: resp.GetBuild().GetVersion(), Build: resp.GetBuild().GetBuild(), Commit: resp.GetBuild().GetCommit()},
	}
	volume := func(v *nodev1.Volume) cluster.Volume {
		return cluster.Volume{
			Directory: v.GetDirectory(), Label: v.GetLabel(),
			Filesystem: cluster.Filesystem{
				Type: v.GetFilesystem().GetType(), Device: v.GetFilesystem().GetDevice(), MountPoint: v.GetFilesystem().GetMountPoint(),
			},
		}
	}
	n.Metadata = volume(resp.GetMetadata())
	for _, s := range resp.GetSettings() {
		n.Settings = append(n.Settings, cluster.Setting{
			Name: s.GetName(), Env: s.GetEnv(), Flag: s.GetFlag(), Group: s.GetGroup(), Kind: cluster.SettingKind(s.GetKind()),
			Scope: cluster.SettingScope(s.GetScope()), Source: cluster.SettingSource(s.GetSource()), Set: s.GetSet(), Value: s.GetValue(),
		})
	}
	for _, drive := range resp.GetDrives() {
		n.Drives = append(n.Drives, cluster.DriveSettings{ID: drive.GetId(), Volume: volume(drive.GetVolume())})
	}
	for _, c := range resp.GetCertificates() {
		n.Certificates = append(n.Certificates, cluster.Certificate{
			Use: c.GetUse(), Subject: c.GetSubject(), Issuer: c.GetIssuer(), DNSNames: c.GetDnsNames(), IPAddresses: c.GetIpAddresses(),
		})
	}
	return n
}

// TestSecretsDoNotLeaveTheNode: whatever the source of a node's settings
// hands the server - secrets with their values, a URL with a password, more
// of everything than there can be - the bytes the node puts on the wire hold
// none of the secrets and stay within the bounds. That is checked on the
// encoded answer, before any client has had a hand in it.
func TestSecretsDoNotLeaveTheNode(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	logs := &lockedLog{}
	node := serveWith(t, p, "storage-1", func(cfg *noderpc.ServerConfig) {
		cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
		cfg.Settings = func(context.Context) (cluster.NodeSettings, error) { return overfullSettings("storage-1"), nil }
	})
	resp := rawSettings(t, p, "storage-1", node.address)
	if resp.GetError() != nil {
		t.Fatalf("the node answered %v", resp.GetError())
	}
	encoded, err := proto.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(settingsWireMark)) {
		at := bytes.Index(encoded, []byte(settingsWireMark))
		t.Fatalf("the encoded answer holds what must not leave the node, after %q", encoded[max(0, at-80):at])
	}
	if bytes.Contains(encoded, []byte("user:")) || bytes.Contains(encoded, []byte("token=")) {
		t.Error("the encoded answer holds the userinfo or the query of a URL")
	}
	// Well within what a client of this package takes in one message.
	if len(encoded) > 2<<20 {
		t.Errorf("the encoded answer has %d bytes", len(encoded))
	}

	got := decoded(resp)
	withinBounds(t, "the encoded answer", got)
	if len(got.Settings) != noderpc.MaxSettings || len(got.Drives) != noderpc.MaxSettingsDrives || len(got.Certificates) != noderpc.MaxSettingsCertificates {
		t.Errorf("%d settings, %d drives, %d certificates: less was sent than there was room for",
			len(got.Settings), len(got.Drives), len(got.Certificates))
	}
	// What is kept is the first by name, and it is redacted, not dropped.
	byName := map[string]cluster.Setting{}
	for _, s := range got.Settings {
		byName[s.Name] = s
	}
	for _, secret := range []string{"a-passphrase", "a-secret-file", "a-kind-of-later", "a-kind-too-long"} {
		if s, ok := byName[secret]; !ok || !s.Set || s.Value != "" {
			t.Errorf("%s = %+v (sent: %v), want it sent as set and without a value", secret, s, ok)
		}
	}
	if s := byName["a-url"]; s.Value != "https://otel:4318/v1" {
		t.Errorf("the URL is sent as %q", s.Value)
	}
	if s := byName["a-long-value"]; !strings.HasSuffix(s.Value, cluster.TruncationMarker) {
		t.Errorf("a value of %d bytes does not say that it was cut", len(s.Value))
	}
	if s := byName["a-value-that-is-not-text"]; !strings.HasPrefix(s.Value, "/data/") || !strings.HasSuffix(s.Value, "/1") {
		t.Errorf("a value that is not text is sent as %q", s.Value)
	}
	if !strings.Contains(logs.String(), "dropped=") {
		t.Errorf("the node did not log that it dropped what exceeds the bounds: %s", logs.String())
	}

	// And through a client of this package: the same, in order.
	client := clientFor(t, p.identity("gateway-1"), map[string]string{"storage-1": node.address})
	read, err := client.NodeSettings(t.Context(), "storage-1")
	if err != nil {
		t.Fatalf("NodeSettings: %v", err)
	}
	withinBounds(t, "what the client returns", read)
	if strings.Contains(fmt.Sprintf("%+v", read), settingsWireMark) {
		t.Error("what the client returns holds what must not be shown")
	}
	if !reflect.DeepEqual(read, read.Normalized()) {
		t.Error("what the client returns is not in the order of Normalized")
	}
}

// lyingNode answers the Settings call with whatever it was given: a node
// that is not this release's server and keeps none of its rules.
type lyingNode struct {
	nodev1.UnimplementedNodeServiceServer
	answer *nodev1.SettingsResponse
}

func (n lyingNode) Settings(context.Context, *nodev1.SettingsRequest) (*nodev1.SettingsResponse, error) {
	return n.answer, nil
}

// TestSettingsOfANodeThatLies: a peer that holds a certificate of the node
// CA and sends what no node of this release sends - secret values, endless
// lists, names without end - gets none of it past the client, which redacts
// and bounds what it receives and logs that it dropped something.
func TestSettingsOfANodeThatLies(t *testing.T) {
	t.Parallel()
	p := newPKI(t)
	long := strings.Repeat("n", 3000) + settingsWireMark
	answer := &nodev1.SettingsResponse{
		NodeId: "storage-1", KmsKeyId: long, Build: &nodev1.Build{Version: long, Build: long, Commit: long},
		Labels: map[string]string{},
		Metadata: &nodev1.Volume{
			Directory: strings.Repeat("d", 3*cluster.MaxSettingValueBytes) + settingsWireMark, Label: long,
			Filesystem: &nodev1.Filesystem{Type: long, Device: "//user:" + settingsWireMark + "@files/share", MountPoint: "/m"},
		},
		Settings: []*nodev1.Setting{
			{Name: "a-passphrase", Kind: "secret", Scope: "cluster", Value: settingsWireMark},
			{Name: "a-secret-file", Kind: "secret-file", Value: "/run/" + settingsWireMark},
			{Name: "a-url", Kind: "url", Value: "https://user:" + settingsWireMark + "@otel:4318/v1?token=" + settingsWireMark},
			{Name: "a-kind-of-later", Kind: "later", Value: settingsWireMark},
			{Name: "a-kind-too-long", Kind: "plain" + long, Value: settingsWireMark},
			{Name: "a-long-value", Kind: "plain", Value: strings.Repeat("v", 3*cluster.MaxSettingValueBytes) + settingsWireMark},
			{Name: "a-name-" + long, Env: long, Flag: long, Group: long, Kind: "plain", Scope: long, Source: long, Value: "1"},
		},
	}
	for i := range 10000 {
		answer.Settings = append(answer.Settings, &nodev1.Setting{Name: fmt.Sprintf("setting-%05d", i), Kind: "plain", Value: "v"})
	}
	for i := range 5000 {
		answer.Drives = append(answer.Drives, &nodev1.DriveSettings{Id: fmt.Sprintf("drive-%04d", i)})
	}
	for i := range 100 {
		answer.Roles = append(answer.Roles, fmt.Sprintf("role-%03d", i))
		answer.Labels[fmt.Sprintf("label-%03d", i)] = "x"
		certificate := &nodev1.Certificate{Use: "node", Subject: fmt.Sprintf("CN=%03d", i)}
		for j := range 100 {
			certificate.DnsNames = append(certificate.DnsNames, fmt.Sprintf("name-%03d", j))
			certificate.IpAddresses = append(certificate.IpAddresses, fmt.Sprintf("192.0.2.%d", j))
		}
		answer.Certificates = append(answer.Certificates, certificate)
	}
	if size := proto.Size(answer); size > 2<<20 {
		t.Fatalf("the lie has %d bytes, more than a client takes: nothing would be tested", size)
	}
	address := serveRaw(t, p, "storage-1", func(server *grpc.Server) {
		nodev1.RegisterNodeServiceServer(server, lyingNode{answer: answer})
	})
	logs := &lockedLog{}
	client := clientFor(t, p.identity("gateway-1"), map[string]string{"storage-1": address}).
		WithLogger(slog.New(slog.NewTextHandler(logs, nil)))

	got, err := client.NodeSettings(t.Context(), "storage-1")
	if err != nil {
		t.Fatalf("NodeSettings: %v", err)
	}
	if shown := fmt.Sprintf("%+v", got); strings.Contains(shown, settingsWireMark) {
		at := strings.Index(shown, settingsWireMark)
		t.Fatalf("what the client returns holds what must not be shown, after %q", shown[max(0, at-80):at])
	}
	withinBounds(t, "what the client returns", got)
	if len(got.Settings) != noderpc.MaxSettings || len(got.Drives) != noderpc.MaxSettingsDrives ||
		len(got.Certificates) != noderpc.MaxSettingsCertificates || len(got.Labels) != noderpc.MaxSettingsLabels {
		t.Errorf("%d settings, %d drives, %d certificates, %d labels", len(got.Settings), len(got.Drives), len(got.Certificates), len(got.Labels))
	}
	for _, drive := range got.Drives {
		if drive.ErasureSet != cluster.ErasureSetUnknown {
			t.Fatalf("drive %s is said to be in erasure set %d", drive.ID, drive.ErasureSet)
		}
	}
	if !reflect.DeepEqual(got, got.Normalized()) {
		t.Error("what the client returns is not in the order of Normalized")
	}
	if log := logs.String(); !strings.Contains(log, "node=storage-1") || !strings.Contains(log, "dropped=") {
		t.Errorf("the client did not log what it dropped: %s", log)
	}
}
