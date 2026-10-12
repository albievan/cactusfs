//go:build integration && !windows

package integration_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	nodev1 "github.com/albievan/cactusfs/internal/adapters/noderpc/nodev1/cactusfs/node/v1"
	"github.com/albievan/cactusfs/test/clustertest"
)

// settingsErasureSet is the coding of one erasure set, as GET /settings gives
// it.
type settingsErasureSet struct {
	Index       int `json:"index"`
	Drives      int `json:"drives"`
	Data        int `json:"data"`
	Parity      int `json:"parity"`
	ReadQuorum  int `json:"read_quorum"`
	WriteQuorum int `json:"write_quorum"`
}

// settingsDrive is one drive of a node.
type settingsDrive struct {
	ID         string `json:"id"`
	Directory  string `json:"directory"`
	Label      string `json:"label"`
	Filesystem *struct {
		Type       string `json:"type"`
		MountPoint string `json:"mount_point"`
	} `json:"filesystem"`
	TotalBytes int64 `json:"total_bytes"`
	FreeBytes  int64 `json:"free_bytes"`
	Online     bool  `json:"online"`
	ErasureSet *int  `json:"erasure_set"`
}

// settingsNode is what one node says of itself, or that it said nothing. Its
// settings are kept as they were sent, member by member: whether a setting
// has a "value" at all is what the test asks of a secret.
type settingsNode struct {
	NodeID   string            `json:"node_id"`
	Answered bool              `json:"answered"`
	Reason   string            `json:"reason"`
	Roles    []string          `json:"roles"`
	Labels   map[string]string `json:"labels"`
	Server   *struct {
		Version string `json:"version"`
		Build   string `json:"build"`
	} `json:"server"`
	StartedAt *time.Time       `json:"started_at"`
	KMSKeyID  string           `json:"kms_key_id"`
	Settings  []map[string]any `json:"settings"`
	Drives    []settingsDrive  `json:"drives"`
	Metadata  *struct {
		Directory  string `json:"directory"`
		TotalBytes int64  `json:"total_bytes"`
	} `json:"metadata"`
	Certificates []struct {
		Use      string    `json:"use"`
		Subject  string    `json:"subject"`
		DNSNames []string  `json:"dns_names"`
		NotAfter time.Time `json:"not_after"`
	} `json:"certificates"`
}

// settingsShown is one setting as a node shows it.
type settingsShown struct {
	kind, source string
	set          bool
	value        string
	// valued says that the answer has a "value" for the setting at all.
	valued bool
}

// shown returns the setting of that name as the node gave it.
func (n settingsNode) shown(name string) (settingsShown, bool) {
	for _, setting := range n.Settings {
		if setting["name"] != name {
			continue
		}
		var out settingsShown
		out.kind, _ = setting["kind"].(string)
		out.source, _ = setting["source"].(string)
		out.set, _ = setting["set"].(bool)
		out.value, out.valued = setting["value"].(string)
		return out, true
	}
	return settingsShown{}, false
}

// settingsMismatch is a setting the nodes that answered differ in.
type settingsMismatch struct {
	Setting string `json:"setting"`
	Values  []struct {
		NodeID string `json:"node_id"`
		Value  string `json:"value"`
	} `json:"values"`
}

// settingsView is the answer of GET /settings, as far as these tests read it.
type settingsView struct {
	AnsweredBy string `json:"answered_by"`
	Cluster    struct {
		DeploymentID  string               `json:"deployment_id"`
		MetadataLevel int                  `json:"metadata_level"`
		Leader        string               `json:"leader"`
		Nodes         int                  `json:"nodes"`
		Voters        int                  `json:"voters"`
		ErasureSets   []settingsErasureSet `json:"erasure_sets"`
	} `json:"cluster"`
	Nodes      []settingsNode     `json:"nodes"`
	Mismatches []settingsMismatch `json:"mismatches"`
}

// whole reports whether the view is of a deployment that has formed and whose
// nodes all answered: so many nodes recorded, a leader, the drives in their
// erasure sets.
func (v settingsView) whole(nodes int) bool {
	if len(v.Nodes) != nodes || v.Cluster.Nodes != nodes || v.Cluster.Leader == "" || len(v.Cluster.ErasureSets) == 0 {
		return false
	}
	for _, node := range v.Nodes {
		if !node.Answered || len(node.Drives) == 0 {
			return false
		}
		for _, drive := range node.Drives {
			if drive.ErasureSet == nil {
				return false
			}
		}
	}
	return true
}

// picture writes who answered on one line, by node: "node1=answered
// node2=unreachable".
func (v settingsView) picture() string {
	parts := make([]string, len(v.Nodes))
	for i, node := range v.Nodes {
		parts[i] = node.NodeID + "=" + node.Reason
		if node.Answered {
			parts[i] = node.NodeID + "=answered"
		}
	}
	return strings.Join(parts, " ")
}

// mismatches writes the mismatches on one line, in their order:
// "gc-grace: node1=24h node2=48h; ".
func (v settingsView) mismatches() string {
	var out strings.Builder
	for _, mismatch := range v.Mismatches {
		out.WriteString(mismatch.Setting + ":")
		for _, value := range mismatch.Values {
			out.WriteString(" " + value.NodeID + "=" + value.Value)
		}
		out.WriteString("; ")
	}
	return out.String()
}

// settingsCall sends one request to the management API of a node, as
// gridCall does, within a minute.
func settingsCall(t *testing.T, c *clustertest.Cluster, id string, who map[string]string, method, path string, body any) (int, http.Header, []byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	return gridCall(ctx, c, id, who, method, path, body)
}

// settingsRead asks a node for the settings of the deployment, as the caller
// the headers name, and returns them beside the answer as it was sent.
func settingsRead(t *testing.T, c *clustertest.Cluster, id string, who map[string]string) (settingsView, []byte, error) {
	t.Helper()
	status, header, body, err := settingsCall(t, c, id, who, http.MethodGet, "/settings", nil)
	if err != nil {
		return settingsView{}, nil, err
	}
	if status != http.StatusOK {
		return settingsView{}, body, fmt.Errorf("the settings through %s = %d %s", id, status, body)
	}
	if kind := header.Get("Content-Type"); !strings.HasPrefix(kind, "application/json") {
		return settingsView{}, body, fmt.Errorf("the settings through %s are sent as %q", id, kind)
	}
	var view settingsView
	if err := json.Unmarshal(body, &view); err != nil {
		return settingsView{}, body, fmt.Errorf("the settings through %s: %w", id, err)
	}
	return view, body, nil
}

// settingsCluster asks a node who leads the metadata group and which format
// of metadata entry each recorded node says it supports.
func settingsCluster(t *testing.T, c *clustertest.Cluster, id string, root map[string]string) (leader string, levels map[string]int, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	return schema4Cluster(ctx, c, id, root)
}

// settingsLowest returns the lowest of the levels, which is the level the
// deployment is at; 0 when there is none.
func settingsLowest(levels map[string]int) int {
	lowest := 0
	for _, level := range levels {
		if lowest == 0 || level < lowest {
			lowest = level
		}
	}
	return lowest
}

// settingsUser creates a user with one built-in role and a key of its own,
// through a node, and returns the header the key authenticates with beside
// the secret of the key.
func settingsUser(t *testing.T, c *clustertest.Cluster, id string, root map[string]string, name, role string) (map[string]string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	accessKeyID, secretKey, err := gridUser(ctx, c, id, root, name, map[string]any{"roles": []string{role}})
	if err != nil {
		return nil, "", fmt.Errorf("a user of %s: %w", role, err)
	}
	return gridBearer(accessKeyID, secretKey), secretKey, nil
}

// settingsAuditRecord is a record of the audit trail, as far as it is read
// here.
type settingsAuditRecord struct {
	Sequence int64  `json:"sequence"`
	Actor    string `json:"actor"`
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Outcome  string `json:"outcome"`
}

// settingsDenials reads the whole audit trail through a node and returns the
// records of a caller who was refused the settings.
func settingsDenials(t *testing.T, c *clustertest.Cluster, id string, root map[string]string) ([]settingsAuditRecord, error) {
	t.Helper()
	var denied []settingsAuditRecord
	after := int64(0)
	for {
		status, _, body, err := settingsCall(t, c, id, root, http.MethodGet, fmt.Sprintf("/audit?after=%d&limit=1000", after), nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("the audit trail through %s = %d %s", id, status, body)
		}
		var page struct {
			Records   []settingsAuditRecord `json:"records"`
			NextAfter *int64                `json:"next_after"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("the audit trail through %s: %w", id, err)
		}
		for _, record := range page.Records {
			if record.Action == "admin:GetSettings" && record.Outcome == "denied" {
				denied = append(denied, record)
			}
		}
		if page.NextAfter == nil || *page.NextAfter <= after {
			return denied, nil
		}
		after = *page.NextAfter
	}
}

// settingsOverRPC asks a node for its settings as another node does: with
// the call of node RPC, on the published port, over mutual TLS. The caller
// shows the certificate the test's node CA issued to the test itself - or
// none, to be refused.
func settingsOverRPC(t *testing.T, c *clustertest.Cluster, pki *rotationPKI, id string, withCertificate bool) (*nodev1.SettingsResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	address, err := c.Address(ctx, id, 9002)
	if err != nil {
		return nil, fmt.Errorf("node RPC of %s: %w", id, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pki.nodeCA.CertPEM) {
		return nil, errors.New("the node CA of the test holds no certificate")
	}
	// The node is reached at its published port and meant by its name, which
	// is the name in its certificate.
	config := &tls.Config{RootCAs: roots, ServerName: id, MinVersion: tls.VersionTLS13}
	if withCertificate {
		config.Certificates = []tls.Certificate{pki.probe}
	}
	conn, err := grpc.NewClient("passthrough:///"+address, grpc.WithTransportCredentials(credentials.NewTLS(config)))
	if err != nil {
		return nil, fmt.Errorf("node RPC of %s: %w", id, err)
	}
	defer func() { _ = conn.Close() }()
	resp, err := nodev1.NewNodeServiceClient(conn).Settings(ctx, &nodev1.SettingsRequest{})
	if err != nil {
		return nil, fmt.Errorf("the settings of %s over node RPC: %w", id, err)
	}
	if reported := resp.GetError(); reported != nil {
		return nil, fmt.Errorf("the settings of %s over node RPC: the node answers %q", id, reported.GetMessage())
	}
	return resp, nil
}

// settingsScan fails for every one of the secrets that text holds. It names
// the secret and never prints it.
func settingsScan(t *testing.T, what, text string, marked map[string]string) {
	t.Helper()
	for name, secret := range marked {
		if strings.Contains(text, secret) {
			t.Errorf("%s holds %s", what, name)
		}
	}
}

// settingsPrintable returns text as a failure may print it: with every one of
// the secrets, should it hold any, replaced by its name.
func settingsPrintable(text string, marked map[string]string) string {
	for name, secret := range marked {
		text = strings.ReplaceAll(text, secret, "["+name+"]")
	}
	return text
}

// TestSettingsOfEveryNode is the acceptance test of PLAN H.10, on the cluster
// of four containers: how every node is configured, asked of any one of them.
//
// Each node is told what the deployment calls its two volumes; one logs at
// another level than the others, which is its own business; one keeps
// unreferenced shards longer than the others, which is a setting the nodes
// should agree on. Every node knows the passphrase of the key file and the
// root credentials, and is given a trace collector - without a credential in
// its URL, which a node refuses - at an address where nothing listens.
//
// Any node answers for all four: each with its two drives, their
// directories, labels and file systems, its certificates and its settings,
// with where each value came from. The one setting the nodes differ in is
// listed, the log level is not. No secret is in any answer, in what a node
// sends another over node RPC, or in any log, and no setting that is a
// secret has a value at all. An auditor may read it; who may only read
// objects is refused, and that is in the audit trail once; nobody is told
// anything without a credential. A node that is stopped is said to be
// unreachable within five seconds, by each of the others, and answers again
// when it is back.
func TestSettingsOfEveryNode(t *testing.T) {
	t.Parallel()
	nodes := clustertest.Nodes
	const (
		// debugNode logs at the level debug: a difference between nodes
		// that is no mismatch, and the log most likely to say too much.
		debugNode = "node2"
		// oddNode keeps unreferenced shards two days, the others the one
		// day that is the default. Nothing is collected in the minutes of
		// this test either way.
		oddNode  = "node4"
		oddGrace = "48h"
		// Nothing listens on the discard port of a node's own loopback
		// address: a connection there is refused at once, and no span
		// leaves the node.
		collectorShown = "http://127.0.0.1:9/v1/traces"
	)
	// A node refuses a collector whose URL carries a credential - userinfo,
	// a query - before it starts (the unit tests hold that, and that such a
	// URL is never printed), so the cluster is given the URL without one.
	// The mark stays among what is looked for: it must be nowhere.
	mark := "se" + "ntinel-" + hex.EncodeToString(randomBytes(t, 6))
	collector := collectorShown
	labels := func(id string) []string {
		return []string{"vol-" + id + "-data1", "vol-" + id + "-data2"}
	}
	nodeEnv := map[string]map[string]string{}
	for _, id := range nodes {
		nodeEnv[id] = map[string]string{"CACTUSFS_DRIVE_LABELS": strings.Join(labels(id), ",")}
	}
	nodeEnv[debugNode]["CACTUSFS_LOG_LEVEL"] = "debug"
	nodeEnv[oddNode]["CACTUSFS_GC_GRACE"] = oddGrace

	// The CAs are the test's own, as in the test of certificate rotation, so
	// that it can show node RPC a certificate: "cactusfs dev-init" keeps no
	// CA key, as it should not.
	pki := newRotationPKI(t)
	c := clustertest.Start(t, clustertest.Options{
		Env:     map[string]string{"CACTUSFS_OTLP_ENDPOINT": collector},
		NodeEnv: nodeEnv,
		Prepare: func(c *clustertest.Cluster) { pki.install(t, c) },
		// The test asks every node over node RPC what it would send another.
		PublishNodeRPC: true,
	})
	root := gridBearer(c.AccessKey, c.SecretKey)
	// What no answer and no log may hold. The harness made the passphrase
	// and the root credentials up for this run.
	marked := map[string]string{
		"the mark in the URL of the collector": mark,
		"the KMS passphrase":                   c.KMSPassphrase,
		"the root secret key":                  c.SecretKey,
	}
	// An answer does not name the root access key either; a log may, where
	// it says who asked.
	answered := map[string]string{"the root access key": c.AccessKey}
	for name, secret := range marked {
		answered[name] = secret
	}
	for name, secret := range answered {
		if len(secret) < 12 {
			t.Fatalf("%s is %d characters long: a search for it would prove nothing", name, len(secret))
		}
	}
	read := func(id string, who map[string]string) (settingsView, []byte) {
		t.Helper()
		view, body, err := settingsRead(t, c, id, who)
		if err != nil {
			t.Fatal(err)
		}
		return view, body
	}

	// check holds an answer against what the nodes were started with. down
	// names the node that is stopped, if one is.
	check := func(asked string, view settingsView, body []byte, down string) {
		t.Helper()
		sent := settingsPrintable(string(body), answered)
		settingsScan(t, "the answer of GET /settings through "+asked, string(body), answered)
		if view.AnsweredBy != asked || len(view.Nodes) != len(nodes) {
			t.Fatalf("asked of %s: answered by %q, with %d nodes: %s", asked, view.AnsweredBy, len(view.Nodes), sent)
		}
		// The nodes as they were sent, member by member: a node that did
		// not answer has its name, that it did not, the reason - and no
		// member besides.
		var members struct {
			Nodes []map[string]json.RawMessage `json:"nodes"`
		}
		if err := json.Unmarshal(body, &members); err != nil || len(members.Nodes) != len(nodes) {
			t.Fatalf("the nodes of the answer through %s: %v: %s", asked, err, sent)
		}

		// The deployment: four nodes, three of which vote; the leader and
		// the level the node itself reports elsewhere; eight drives in one
		// set, of which four may be lost.
		leader, levels, err := settingsCluster(t, c, asked, root)
		if err != nil {
			t.Fatal(err)
		}
		lowest := settingsLowest(levels)
		if d := view.Cluster; d.DeploymentID == "" || d.Nodes != 4 || d.Voters != 3 || d.Leader == "" || d.Leader != leader ||
			!slices.Contains(nodes[:3], d.Leader) || lowest == 0 || d.MetadataLevel != lowest {
			t.Errorf("the deployment through %s = %+v; want four nodes, three voters, the leader %q, the metadata level %d", asked, d, leader, lowest)
		}
		if want := []settingsErasureSet{{Index: 0, Drives: 8, Data: 4, Parity: 4, ReadQuorum: 4, WriteQuorum: 5}}; !slices.Equal(view.Cluster.ErasureSets, want) {
			t.Errorf("the erasure sets through %s = %+v, want %+v", asked, view.Cluster.ErasureSets, want)
		}

		versions, counts, owners := map[string]bool{}, map[int]bool{}, map[string]string{}
		wantMismatch := "gc-grace:"
		for i, node := range view.Nodes {
			id := nodes[i]
			if node.NodeID != id {
				t.Fatalf("node %d of the answer through %s is %q: the nodes are not by ID: %s", i, asked, node.NodeID, sent)
			}
			if id == down {
				if node.Answered || node.Reason != "unreachable" || len(members.Nodes[i]) != 3 {
					t.Errorf("%s is stopped and is shown, through %s, as answered: %t, reason %q, with %d members; want not answered, unreachable and nothing else",
						id, asked, node.Answered, node.Reason, len(members.Nodes[i]))
				}
				continue
			}
			grace, graceSource := "24h", "default"
			if id == oddNode {
				grace, graceSource = oddGrace, "environment"
			}
			wantMismatch += " " + id + "=" + grace
			if _, reasoned := members.Nodes[i]["reason"]; !node.Answered || reasoned {
				t.Errorf("%s, through %s: answered: %t, reason %q; want an answer and no reason", id, asked, node.Answered, node.Reason)
				continue
			}

			// Who it is.
			wantRoles := []string{"console", "gateway", "meta", "storage"}
			if id == "node4" {
				wantRoles = []string{"console", "gateway", "storage"}
			}
			roles := slices.Sorted(slices.Values(node.Roles))
			if !slices.Equal(roles, wantRoles) || len(node.Labels) != 1 || node.Labels["zone"] != "zone-"+id {
				t.Errorf("%s, through %s: roles %v, labels %v", id, asked, node.Roles, node.Labels)
			}
			if node.Server == nil || node.Server.Version == "" || node.Server.Build == "" || node.StartedAt == nil || node.StartedAt.IsZero() ||
				!strings.HasPrefix(node.KMSKeyID, "local:") {
				t.Errorf("%s, through %s: build %+v, started at %v, KMS key %q", id, asked, node.Server, node.StartedAt, node.KMSKeyID)
			}
			if node.Server != nil {
				versions[node.Server.Version+"-"+node.Server.Build] = true
			}

			// Its settings. Nothing that is a secret, or of a kind this
			// test does not know, has a value - not an empty one either.
			counts[len(node.Settings)] = true
			for _, setting := range node.Settings {
				name, _ := setting["name"].(string)
				kind, _ := setting["kind"].(string)
				_, valued := setting["value"]
				if name == "" || (kind != "plain" && kind != "path" && kind != "url" && valued) {
					t.Errorf("%s, through %s: the setting %q of kind %q has a value", id, asked, name, kind)
				}
			}
			logLevel := settingsShown{kind: "plain", source: "default", set: true, value: "info", valued: true}
			if id == debugNode {
				logLevel = settingsShown{kind: "plain", source: "environment", set: true, value: "debug", valued: true}
			}
			for name, want := range map[string]settingsShown{
				"node-id":      {kind: "plain", source: "environment", set: true, value: id, valued: true},
				"data":         {kind: "path", source: "environment", set: true, value: strings.Join(clustertest.DriveDirs(), ","), valued: true},
				"drive-labels": {kind: "plain", source: "environment", set: true, value: strings.Join(labels(id), ","), valued: true},
				"meta-dir":     {kind: "path", source: "environment", set: true, value: clustertest.MetaDir(), valued: true},
				"log-level":    logLevel,
				"gc-grace":     {kind: "plain", source: graceSource, set: true, value: grace, valued: true},
				// Of a URL: where, and nothing a credential travels in.
				"otlp-endpoint": {kind: "url", source: "environment", set: true, value: collectorShown, valued: true},
				// Of a secret, and of the file that holds one: that it is set.
				"kms-passphrase":      {kind: "secret", source: "environment", set: true},
				"root-access-key":     {kind: "secret", source: "environment", set: true},
				"root-secret-key":     {kind: "secret", source: "environment", set: true},
				"tls-key":             {kind: "secret-file", source: "environment", set: true},
				"node-tls-key":        {kind: "secret-file", source: "environment", set: true},
				"kms-key-file":        {kind: "secret-file", source: "environment", set: true},
				"kms-passphrase-file": {kind: "secret-file", source: "default"},
			} {
				if got, listed := node.shown(name); !listed || got != want {
					t.Errorf("%s, through %s: the setting %s = %+v (listed: %t), want %+v", id, asked, name, got, listed, want)
				}
			}
			if got, listed := node.shown("s3-addr"); !listed || !got.set || !got.valued || got.value == "" || got.kind != "plain" {
				t.Errorf("%s, through %s: the setting s3-addr = %+v (listed: %t), want it with its value", id, asked, got, listed)
			}

			// Its two drives, and no other node's: each in the directory
			// its volume is mounted at, under the label of that place, on a
			// file system mounted there, in the one erasure set.
			dirs := clustertest.DriveDirs()
			byDir := map[string]settingsDrive{}
			for _, drive := range node.Drives {
				byDir[drive.Directory] = drive
				if owner, twice := owners[drive.ID]; twice || drive.ID == "" {
					t.Errorf("%s, through %s: the drive %q in %s is also a drive of %q", id, asked, drive.ID, drive.Directory, owner)
				}
				owners[drive.ID] = id
				if !drive.Online || drive.TotalBytes <= 0 || drive.FreeBytes <= 0 || drive.FreeBytes > drive.TotalBytes || drive.ErasureSet == nil || *drive.ErasureSet != 0 {
					t.Errorf("%s, through %s: the drive in %s is online: %t, of %d bytes, %d free, in a set: %t; want it online, with room, in set 0",
						id, asked, drive.Directory, drive.Online, drive.TotalBytes, drive.FreeBytes, drive.ErasureSet != nil)
				}
				if fs := drive.Filesystem; fs == nil || fs.Type == "" || fs.MountPoint != drive.Directory {
					t.Errorf("%s, through %s: the file system of the drive in %s = %+v; want its type, mounted at the directory", id, asked, drive.Directory, fs)
				}
			}
			if len(node.Drives) != len(dirs) || byDir[dirs[0]].Label != labels(id)[0] || byDir[dirs[1]].Label != labels(id)[1] {
				t.Errorf("%s, through %s: %d drives, in %s labelled %q and in %s labelled %q; want the two, labelled %v",
					id, asked, len(node.Drives), dirs[0], byDir[dirs[0]].Label, dirs[1], byDir[dirs[1]].Label, labels(id))
			}
			if node.Metadata == nil || node.Metadata.Directory != clustertest.MetaDir() || node.Metadata.TotalBytes <= 0 {
				t.Errorf("%s, through %s: the metadata volume = %+v, want it in %s", id, asked, node.Metadata, clustertest.MetaDir())
			}

			// Its certificates: the one it shows other nodes, which names
			// it and nothing else, and the one clients are shown.
			uses := map[string]int{}
			for _, certificate := range node.Certificates {
				uses[certificate.Use]++
				named := slices.Contains(certificate.DNSNames, id)
				if certificate.Use == "node" {
					named = certificate.Subject == "CN="+id && slices.Equal(certificate.DNSNames, []string{id})
				}
				if !named || !certificate.NotAfter.After(time.Now()) {
					t.Errorf("%s, through %s: the certificate of use %q is of %q for %v until %s", id, asked,
						certificate.Use, certificate.Subject, certificate.DNSNames, certificate.NotAfter)
				}
			}
			if len(node.Certificates) != 2 || uses["node"] != 1 || uses["s3"] != 1 {
				t.Errorf("%s, through %s: certificates by use %v, want one of use node and one of use s3", id, asked, uses)
			}
		}
		if len(versions) != 1 || len(counts) != 1 || counts[0] {
			t.Errorf("through %s the nodes that answered are of %d builds and list their settings in %d lengths; want one build, and as many settings on each", asked, len(versions), len(counts))
		}
		wantDrives := 2 * len(nodes)
		if down != "" {
			wantDrives -= 2
		}
		if len(owners) != wantDrives {
			t.Errorf("through %s the nodes that answered have %d drives between them, want %d", asked, len(owners), wantDrives)
		}
		// The nodes that answered differ in the one setting they should
		// agree on, and each is listed with what it has. The log level is
		// each node's own, and so is the collector: neither is a mismatch.
		// Nothing is said of the node that is stopped.
		if got := view.mismatches(); got != wantMismatch+"; " {
			t.Errorf("the mismatches through %s = %q, want %q", asked, got, wantMismatch+"; ")
		}
	}

	// A deployment that has just formed: every node registered, the drives
	// in their set.
	c.WaitReady(nodes...)
	c.Eventually("every node to give its settings", 2*time.Minute, func() bool {
		view, _, err := settingsRead(t, c, "node1", root)
		return err == nil && view.whole(len(nodes))
	})

	// Any node answers for all four.
	for _, id := range nodes {
		view, body := read(id, root)
		check(id, view, body, "")
	}

	// What a node sends another when it is asked: no secret is in it, in
	// the bytes that cross the wire or written out, and no setting that is a
	// secret has a value. It is the node's answer: its name, its drives,
	// the collector without what a credential travels in.
	for _, id := range nodes {
		resp, err := settingsOverRPC(t, c, pki, id, true)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := proto.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		written, err := protojson.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		settingsScan(t, "what "+id+" sends over node RPC", string(wire), answered)
		settingsScan(t, "what "+id+" sends over node RPC, written out", string(written), answered)
		if resp.GetNodeId() != id || len(resp.GetDrives()) != 2 || len(resp.GetSettings()) == 0 ||
			!strings.Contains(string(wire), collectorShown) || !strings.Contains(string(wire), clustertest.DriveDirs()[0]) {
			t.Errorf("what %s sends over node RPC is not its answer: it names %q, with %d drives and %d settings", id, resp.GetNodeId(), len(resp.GetDrives()), len(resp.GetSettings()))
		}
		for _, setting := range resp.GetSettings() {
			if kind := setting.GetKind(); kind != "plain" && kind != "path" && kind != "url" && setting.GetValue() != "" {
				t.Errorf("%s sends the setting %s of kind %q with a value over node RPC", id, setting.GetName(), kind)
			}
		}
	}
	// And only a node is told: a caller without a certificate of the node CA
	// gets no answer.
	if resp, err := settingsOverRPC(t, c, pki, "node3", false); err == nil {
		t.Errorf("node3 answered a caller of node RPC that showed no certificate, with %d settings", len(resp.GetSettings()))
	}

	// Who may read it. A user who may look at how the deployment is run
	// reads it, through a node of their choice.
	auditor, auditorSecret, err := settingsUser(t, c, "node4", root, "settings-auditor", "auditor")
	if err != nil {
		t.Fatal(err)
	}
	reader, readerSecret, err := settingsUser(t, c, "node4", root, "settings-reader", "read-only")
	if err != nil {
		t.Fatal(err)
	}
	if denied, err := settingsDenials(t, c, "node1", root); err != nil || len(denied) != 0 {
		t.Fatalf("before anybody was refused, the audit trail holds %d refusals of the settings (%v)", len(denied), err)
	}
	view, body := read("node2", auditor)
	check("node2", view, body, "")
	// A user who may only read objects is refused, with nothing of the
	// settings in the refusal.
	status, header, body, err := settingsCall(t, c, "node3", reader, http.MethodGet, "/settings", nil)
	var problem struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	}
	if err != nil || status != http.StatusForbidden || header.Get("Content-Type") != "application/problem+json" ||
		json.Unmarshal(body, &problem) != nil || problem.Status != http.StatusForbidden || problem.Code != "AccessDenied" {
		t.Errorf("GET /settings as a user of read-only = %d %q %s (%v), want 403 and a problem", status, header.Get("Content-Type"), settingsPrintable(string(body), answered), err)
	}
	settingsScan(t, "the refusal", string(body), answered)
	if strings.Contains(string(body), collectorShown) || strings.Contains(string(body), clustertest.MetaDir()) {
		t.Errorf("the refusal tells of the settings: %s", settingsPrintable(string(body), answered))
	}
	// Nobody is told anything without a credential.
	status, header, body, err = settingsCall(t, c, "node1", nil, http.MethodGet, "/settings", nil)
	if err != nil || status != http.StatusUnauthorized || header.Get("Content-Type") != "application/problem+json" || strings.Contains(string(body), clustertest.MetaDir()) {
		t.Errorf("GET /settings without a credential = %d %q %s (%v), want 401 and a problem", status, header.Get("Content-Type"), settingsPrintable(string(body), answered), err)
	}
	// The refusal is in the audit trail, once, whichever node is asked for
	// the trail; that it was read by who may, and asked for by nobody, is
	// not.
	var denied []settingsAuditRecord
	c.Eventually("the refusal to be in the audit trail", time.Minute, func() bool {
		var err error
		if denied, err = settingsDenials(t, c, "node1", root); err != nil {
			t.Fatal(err)
		}
		return len(denied) > 0
	})
	if len(denied) != 1 || denied[0].Actor != "user/settings-reader" || !strings.Contains(denied[0].Resource, "settings") || denied[0].Sequence <= 0 {
		t.Errorf("the refusals of the settings in the audit trail = %+v, want the one of user/settings-reader", denied)
	}

	// A node is stopped, as an orchestrator stops it - a voter that does not
	// lead, so that the metadata group keeps its leader. It closes its
	// connections as it goes, and its container leaves the cluster network:
	// its name resolves to nothing there and no address is left that could
	// swallow a connection. The others find that out at once, and say it:
	// each of them, within five seconds, with all that it says of the three
	// that run.
	leader, _, err := settingsCluster(t, c, "node4", root)
	if err != nil {
		t.Fatal(err)
	}
	stopped := nodes[0]
	if stopped == leader {
		stopped = nodes[1]
	}
	c.Stop(stopped)
	for _, id := range nodes {
		if id == stopped {
			continue
		}
		began := time.Now()
		view, body := read(id, root)
		if took := time.Since(began); took > 5*time.Second {
			t.Errorf("with %s stopped, the answer through %s took %s; want it within 5s", stopped, id, took)
		}
		check(id, view, body, stopped)
	}

	// It comes back, and is part of every answer again.
	c.Restart(stopped)
	c.Eventually("every node to give its settings again", 2*time.Minute, func() bool {
		for _, id := range nodes {
			if view, _, err := settingsRead(t, c, id, root); err != nil || !view.whole(len(nodes)) {
				return false
			}
		}
		return true
	})
	for _, id := range nodes {
		view, body := read(id, root)
		check(id, view, body, "")
	}

	// Still the one refusal: none of the readings since has added to it.
	if denied, err := settingsDenials(t, c, "node3", root); err != nil || len(denied) != 1 {
		t.Errorf("at the end the audit trail, through node3, holds %d refusals of the settings (%v), want the one", len(denied), err)
	}
	// No secret is in what the nodes logged - the node that logs at debug
	// among them - while they started, answered, refused, lost a peer and
	// found it again.
	marked["the secret key of the auditor"], marked["the secret key of the reader"] = auditorSecret, readerSecret
	for _, id := range nodes {
		log := c.Logs(id)
		if !strings.Contains(log, `"msg":"node started"`) {
			t.Errorf("the log of %s that was searched is not the node's: %d bytes without its start", id, len(log))
		}
		settingsScan(t, "the log of "+id, log, marked)
	}
}
