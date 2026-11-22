//go:build deployscript

package deploy_test

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// k8sBase is the start of the env file of a Kubernetes test: a k8s target
// with its context, the local KMS.
const k8sBase = "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nKMS=local\n"

// k8sReady is k8sBase with S3 published to one network, as k8s_validate
// asks of the default NodePort.
const k8sReady = k8sBase + "K8S_S3_ALLOW_FROM=192.0.2.0/24\n"

// testImage is the image the values are written for: PLAN 10.4 delivers it.
const testImage = "registry.example.test/cactusfs:0.7.0-1-0123456789ab"

// k8s runs shell code as deploy/deploy.sh would at the point where it acts
// on a Kubernetes target: deploy/lib/{common,env,k8s}.sh sourced, the env
// file parsed, resolved and validated, the target resolved and checked, the
// deployment's STATE_DIR entry found (STATE), k8s_validate passed and the
// server's host key checked (k8s_connect). The code then calls what PLAN
// 10.10 and 10.11 will call.
func (r *rig) k8s(env, code string, dryRun bool, extraEnv ...string) result {
	r.t.Helper()
	if err := os.Remove(r.transcript); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.t.Fatal(err)
	}
	envPath := r.envFile("site.env", env)
	given := r.givenPaths(env)
	before := snapshot(r.t, given)
	defer func() {
		if after := snapshot(r.t, given); !maps.Equal(before, after) {
			r.t.Errorf("a file or directory the settings name was removed or changed:\nbefore %v\nafter  %v", before, after)
		}
	}()
	dry := "0"
	if dryRun {
		dry = "1"
	}
	prelude := "set -euf; LC_ALL=C; export LC_ALL; DRY_RUN=" + dry + "; ANOTHER_TARGET=0; " +
		"REPO_ROOT=$1; DEPLOY_DIR=$1/deploy; " +
		". \"$DEPLOY_DIR/lib/common.sh\"; . \"$DEPLOY_DIR/lib/env.sh\"; . \"$DEPLOY_DIR/lib/k8s.sh\"; " +
		"env_parse \"$2\"; env_resolve; env_validate; resolve_target; target_validate; state_check; k8s_validate; k8s_connect; "
	argv := slices.Clone(r.sh.argv[1:])
	if r.trace {
		argv = append(argv, "-x")
	}
	argv = append(argv, "-c", prelude+code, "sh", r.repo, envPath)
	cmd := exec.CommandContext(r.t.Context(), r.sh.argv[0], argv...)
	cmd.Dir = r.work
	cmd.Env = append([]string{"PATH=" + r.bin, "HOME=" + r.home}, extraEnv...)
	if r.input != "" {
		cmd.Stdin = strings.NewReader(r.input)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	var res result
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		res.code = exit.ExitCode()
	default:
		r.t.Fatalf("run the shell: %v", err)
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	res.calls = r.calls()
	return res
}

// givenPaths are the files and directories the env file names by a key that
// ends in _FILE or _DIR - the operator's own, which the script reads and must
// never remove or change - but STATE_DIR (the script's) and the directories of
// another machine or of the repository.
func (r *rig) givenPaths(env string) []string {
	var out []string
	for _, line := range strings.Split(env, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || value == "" || (!strings.HasSuffix(key, "_FILE") && !strings.HasSuffix(key, "_DIR")) {
			continue
		}
		if key == "STATE_DIR" || key == "CE_STATE_DIR" || key == "RELEASE_DIR" {
			continue
		}
		switch {
		case strings.HasPrefix(value, "~/"):
			out = append(out, filepath.Join(r.home, value[2:]))
		case filepath.IsAbs(value):
			out = append(out, value)
		}
	}
	return out
}

// snapshot is every file and directory under the paths, with a hash of each
// file; a path that is not there is recorded as missing.
func snapshot(t *testing.T, paths []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range paths {
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			out[p] = "missing"
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			out[p] = hashFile(t, p)
			continue
		}
		for rel, h := range tree(t, p) {
			out[filepath.Join(p, rel)] = h
		}
		out[p] = "dir"
	}
	return out
}

// hashFile is the SHA-256 of a file, or "link" for a symbolic link.
func hashFile(t *testing.T, path string) string {
	t.Helper()
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return "link"
	}
	sum := sha256.Sum256([]byte(read(t, path)))
	return hex.EncodeToString(sum[:])
}

// tableShells runs f under dash with every case of a table (all), and as a
// smoke test, its first case only, under bash and bash --posix: for the
// tables whose code is not shell-specific. The others run eachShell.
func tableShells(t *testing.T, f func(t *testing.T, r *rig, all bool)) {
	t.Helper()
	for i, sh := range shells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			f(t, newRig(t, sh), i == 0)
		})
	}
}

// state is the deployment's STATE_DIR entry on the target.
func (r *rig) state(target, name string) string {
	return filepath.Join(r.home, ".local", "state", "cactusfs-deploy", target, name)
}

// put writes a file and the directories it is in.
func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// read is the content of a file, read through the directory it is in.
func read(t *testing.T, path string) string {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// kubectlArgs is what kubectl was asked, without how it was reached: the
// words after "--context <ctx>", or after "k3s kubectl" (here or over ssh).
// ok is false for a call that is no kubectl.
func kubectlArgs(c call) (args []string, ok bool) {
	switch c.Tool {
	case "kubectl":
		if len(c.Args) >= 2 && c.Args[0] == "--context" {
			return c.Args[2:], true
		}
		return c.Args, true
	case "k3s", "ssh":
		if i := slices.Index(c.Args, "kubectl"); i >= 0 && (i == 0 || c.Args[i-1] == "k3s") {
			return c.Args[i+1:], true
		}
	}
	return nil, false
}

// insideNamespace fails the test for a kubectl call that reaches outside
// the namespace ns other than as ADR-0033 section 7 allows: listing Nodes,
// StorageClasses and Services of every namespace, getting kube-system or
// the namespace itself, and creating it - from a standard input that is the
// one Namespace document the rig kept in namespace.yaml. Everything else
// names the namespace with -n, and never with -A beside it.
func insideNamespace(t *testing.T, r *rig, calls []call, ns string) {
	t.Helper()
	seen := 0
	for _, c := range calls {
		args, ok := kubectlArgs(c)
		if !ok {
			continue
		}
		seen++
		all := slices.Contains(args, "-A") || slices.Contains(args, "--all-namespaces")
		if i := slices.Index(args, "-n"); i >= 0 && i+1 < len(args) {
			if args[i+1] != ns || all {
				t.Errorf("%s %q: another namespace than %s", c.Tool, args, ns)
			}
			continue
		}
		switch {
		case len(args) >= 2 && args[0] == "get" && slices.Contains([]string{"nodes", "storageclasses", "persistentvolumes"}, args[1]):
		case len(args) >= 3 && args[0] == "get" && args[1] == "services" && all:
		case len(args) >= 3 && args[0] == "get" && args[1] == "namespace" && (args[2] == "kube-system" || args[2] == ns):
		case len(args) == 3 && args[0] == "create" && args[1] == "-f" && args[2] == "-":
			manifest := read(t, filepath.Join(r.dir, "namespace.yaml"))
			sum := sha256.Sum256([]byte(manifest))
			if hex.EncodeToString(sum[:]) != c.Stdin {
				t.Errorf("the create outside a namespace read something else than the namespace kept")
			}
			if strings.Count(manifest, "kind: ") != 1 || !strings.Contains(manifest, "kind: Namespace\n") || strings.Contains(manifest, "---") ||
				!strings.Contains(manifest, "\n  name: "+ns+"\n") {
				t.Errorf("the create outside a namespace is not the one Namespace %s:\n%s", ns, manifest)
			}
		default:
			t.Errorf("%s %q: outside the namespace %s, and not a read ADR-0033 section 7 allows", c.Tool, args, ns)
		}
	}
	if seen == 0 {
		t.Fatal("no kubectl was run: nothing is proven")
	}
}

// kmsLine is KMS=local unless the env file of a case says the KMS itself.
func kmsLine(env string) string {
	if regexp.MustCompile(`(?m)^KMS=`).MatchString(env) {
		return ""
	}
	return "KMS=local\n"
}

// TestK8sValues: the values the chart is rendered with follow from the env
// file and the cluster's nodes (PLAN 10.9).
func TestK8sValues(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		nodes   string
		image   string
		code    int
		want    []string
		notWant []string
	}{
		{
			name: "standalone", env: "K8S_S3_ALLOW_FROM=192.0.2.0/24\n", nodes: "1",
			want: []string{
				"image:\n  repository: 'registry.example.test/cactusfs'\n  tag: '0.7.0-1-0123456789ab'\n  digest: ''\n",
				"pools:\n  - {name: node, replicas: 1, roles: ''}\ndrives: {count: 4, size: '100Gi'}\n",
				"metadata: {size: '10Gi'}\nstorageClassName: ''\n",
				"secrets: {root: 'cactusfs-root', tls: 'cactusfs-tls'}\n",
				"kms:\n  type: local\n  local: {keyFileSecret: 'cactusfs-kms', passphraseSecret: 'cactusfs-kms-passphrase'}\n",
				"service:\n  type: NodePort\n  nodePort: null\n  externalTrafficPolicy: Local\n",
				"adminService:\n  type: ClusterIP\n  nodePort: null\nspread: {whenUnsatisfiable: ScheduleAnyway}\n",
				"  clientsFrom:\n    - ipBlock: {cidr: '192.0.2.0/24'}\n  adminFrom: []\n  egressTo: []\n",
				"upgrade:\n  fromConsole: {enabled: false}\n",
				"extraEnv: []\n",
			},
			notWant: []string{"localVolumes", "dev:", "nodeCA", "logLevel", "s3:"},
		},
		{
			name: "standalone with two drives", env: "K8S_S3_ALLOW_FROM=any\nDRIVES=2\n", nodes: "5",
			want: []string{
				"drives: {count: 2, size: '100Gi'}\n",
				"spread: {whenUnsatisfiable: DoNotSchedule}\n",
				"  clientsFrom:\n    - ipBlock: {cidr: '0.0.0.0/1'}\n    - ipBlock: {cidr: '128.0.0.0/1'}\n    - ipBlock: {cidr: '::/1'}\n    - ipBlock: {cidr: '8000::/1'}\n  adminFrom: []\n",
			},
		},
		{
			name: "cluster", env: "DEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\nK8S_S3_ALLOW_FROM=192.0.2.0/24\n", nodes: "3",
			want: []string{
				"pools:\n  - {name: meta, replicas: 3, roles: ''}\n  - {name: data, replicas: 1, roles: 'gateway,storage,console'}\ndrives: {count: 2, size: '100Gi'}\n",
				"secrets: {root: 'cactusfs-root', tls: 'cactusfs-tls', nodeCA: 'cactusfs-node-ca'}\n",
				"dev: {localKMS: true}\n",
				"spread: {whenUnsatisfiable: DoNotSchedule}\n",
			},
		},
		{
			name: "cluster on one node", env: "DEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\nK8S_S3_ALLOW_FROM=192.0.2.0/24\n", nodes: "1",
			want: []string{"spread: {whenUnsatisfiable: ScheduleAnyway}\n"},
		},
		{
			name: "cluster on two nodes", env: "DEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\nK8S_S3_ALLOW_FROM=192.0.2.0/24\n", nodes: "2",
			code: 2, want: []string{"cluster of 2 nodes: the 3 voters are kept on 3 machines"},
		},
		{
			name: "storage class and sizes", env: "K8S_S3_ALLOW_FROM=192.0.2.0/24\nK8S_STORAGE_CLASS=fast-ssd\nK8S_DRIVE_SIZE=20Gi\nK8S_META_SIZE=2Gi\n", nodes: "1",
			want: []string{"drives: {count: 4, size: '20Gi'}\nmetadata: {size: '2Gi'}\nstorageClassName: 'fast-ssd'\n"},
		},
		{
			name: "hostpath", env: "K8S_S3_ALLOW_FROM=192.0.2.0/24\nK8S_STORAGE=hostpath\nK8S_HOSTPATH=/srv/cactusfs\nK8S_NAMESPACE=store\n", nodes: "1",
			want: []string{
				"storageClassName: ''\nlocalVolumes: {enabled: true, basePath: '/srv/cactusfs/store', nodeName: 'machine-1'}\n",
				"K8S_STORAGE=hostpath makes cluster-scoped objects: the StorageClass store-cactusfs-local and 5 PersistentVolumes",
			},
		},
		{
			name: "hostpath on more nodes", env: "K8S_S3_ALLOW_FROM=192.0.2.0/24\nK8S_STORAGE=hostpath\n", nodes: "3",
			code: 2, want: []string{"K8S_STORAGE=hostpath puts every volume on one machine"},
		},
		{
			name: "S3 inside the cluster", env: "K8S_SERVICE_TYPE=ClusterIP\n", nodes: "1",
			want:    []string{"service:\n  type: ClusterIP\n  nodePort: null\nadminService:", "  clientsFrom: []\n"},
			notWant: []string{"externalTrafficPolicy"},
		},
		{
			name: "admin published", env: "K8S_SERVICE_TYPE=LoadBalancer\nK8S_S3_ALLOW_FROM=198.51.100.7,2001:db8::1\nK8S_NODE_PORT=30900\nK8S_ADMIN_SERVICE_TYPE=NodePort\nK8S_ADMIN_NODE_PORT=30901\nK8S_ADMIN_ALLOW_FROM=192.0.2.0/28\n", nodes: "1",
			want: []string{
				"service:\n  type: LoadBalancer\n  nodePort: 30900\n  externalTrafficPolicy: Local\n",
				"adminService:\n  type: NodePort\n  nodePort: 30901\n  externalTrafficPolicy: Local\n",
				"  clientsFrom:\n    - ipBlock: {cidr: '198.51.100.7/32'}\n    - ipBlock: {cidr: '2001:db8::1/128'}\n  adminFrom:\n    - ipBlock: {cidr: '192.0.2.0/28'}\n",
			},
		},
		{
			name: "settings of the nodes", nodes: "1",
			env: "K8S_S3_ALLOW_FROM=192.0.2.0/24\nCACTUSFS_LOG_LEVEL=debug\nCACTUSFS_REGION=eu-west-1\nCACTUSFS_S3_DOMAIN=s3.example.test\n" +
				"CACTUSFS_SCRUB_INTERVAL=12h\nCACTUSFS_UPDATE_CHANNEL=it's\nCACTUSFS_OTLP_ENDPOINT=https://collector.example.test:4318\n",
			want: []string{
				"logLevel: 'debug'\n", "otlpEndpoint: 'https://collector.example.test:4318'\n",
				"s3:\n  region: 'eu-west-1'\n  domain: 's3.example.test'\n",
				"extraEnv:\n  - {name: CACTUSFS_SCRUB_INTERVAL, value: '12h'}\n  - {name: CACTUSFS_UPDATE_CHANNEL, value: 'it''s'}\nnetworkPolicy:",
			},
			notWant: []string{"name: CACTUSFS_LOG_LEVEL", "name: CACTUSFS_REGION", "name: CACTUSFS_S3_DOMAIN", "name: CACTUSFS_OTLP_ENDPOINT"},
		},
		{
			name: "vault", nodes: "1",
			env: "K8S_S3_ALLOW_FROM=192.0.2.0/24\nKMS=vault\nVAULT_ADDR=https://192.0.2.20:8200\nVAULT_KEY=cactusfs\nVAULT_TOKEN_FILE=~/token\nVAULT_CA_FILE=/vault-ca.pem\nVAULT_NAMESPACE=storage\n",
			want: []string{
				"kms:\n  type: vault\n  vault:\n    addr: 'https://192.0.2.20:8200'\n    transitMount: 'transit'\n    key: 'cactusfs'\n    namespace: 'storage'\n    tokenSecret: 'cactusfs-vault-token'\n    caSecret: 'cactusfs-vault-ca'\n",
				"  egressTo:\n    - to: [{ipBlock: {cidr: '192.0.2.20/32'}}]\n      ports: [8200]\n",
			},
			notWant: []string{"local:", "dev:"},
		},
		{
			name: "image by digest", env: "K8S_S3_ALLOW_FROM=192.0.2.0/24\n", nodes: "1",
			image: "registry.example.test/cactusfs@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			want:  []string{"image:\n  repository: 'registry.example.test/cactusfs'\n  tag: ''\n  digest: 'sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'\n"},
		},
	}
	tableShells(t, func(t *testing.T, r *rig, all bool) {
		for i, c := range cases {
			if !all && i > 0 {
				break
			}
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				put(t, filepath.Join(r.home, "token"), "placeholder")
				values := filepath.Join(r.dir, "values.yaml")
				image := c.image
				if image == "" {
					image = testImage
				}
				res := r.k8s("DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\n"+kmsLine(c.env)+c.env,
					fmt.Sprintf("k8s_values '%s' %s machine-1 '%s'", values, c.nodes, image), false)
				if c.code != 0 {
					res.want(t, c.code, c.want...)
					return
				}
				res.want(t, 0)
				got := read(t, values) + res.output()
				for _, w := range c.want {
					if !strings.Contains(got, w) {
						t.Errorf("the values do not hold %q:\n%s", w, got)
					}
				}
				for _, w := range c.notWant {
					if strings.Contains(got, w) {
						t.Errorf("the values hold %q:\n%s", w, got)
					}
				}
				if len(res.calls) != 0 {
					t.Errorf("writing the values ran %v", res.calls)
				}
			})
		}
	})
}

// TestK8sRefused: what reaching a deployment needs is refused before
// anything is asked of a cluster, when it is missing or wrong - through
// the script's --install (PLAN 10.9; ADR-0033 section 8).
func TestK8sRefused(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("ab", 32)
	cases := []struct{ name, env, want string }{
		{"node port without allow-from", "K8S_SERVICE_TYPE=NodePort", "K8S_S3_ALLOW_FROM must say who may reach it"},
		{"balancer without allow-from", "K8S_SERVICE_TYPE=LoadBalancer", "K8S_SERVICE_TYPE=LoadBalancer publishes S3"},
		{"node port on a ClusterIP", "K8S_SERVICE_TYPE=ClusterIP\nK8S_NODE_PORT=30900", "K8S_NODE_PORT goes with K8S_SERVICE_TYPE=NodePort or LoadBalancer"},
		{"admin published to nobody named", "K8S_S3_ALLOW_FROM=any\nK8S_ADMIN_SERVICE_TYPE=NodePort", "K8S_ADMIN_ALLOW_FROM must say who may reach them"},
		{"admin node port on a ClusterIP", "K8S_S3_ALLOW_FROM=any\nK8S_ADMIN_NODE_PORT=30901", "K8S_ADMIN_NODE_PORT goes with"},
		{"the same node port", "K8S_S3_ALLOW_FROM=any\nK8S_NODE_PORT=30900\nK8S_ADMIN_SERVICE_TYPE=NodePort\nK8S_ADMIN_ALLOW_FROM=any\nK8S_ADMIN_NODE_PORT=30900", "are the same port"},
		{"a prefix too long", "K8S_S3_ALLOW_FROM=192.0.2.0/33", "'192.0.2.0/33' is not an address"},
		{"an octet too large", "K8S_S3_ALLOW_FROM=192.0.2.0/24,300.1.1.1", "'300.1.1.1' is not an address"},
		{"three octets", "K8S_S3_ALLOW_FROM=192.0.2", "'192.0.2' is not an address"},
		{"an IPv6 prefix too long", "K8S_S3_ALLOW_FROM=2001:db8::/129", "'2001:db8::/129' is not an address"},
		{"an IPv6 triple colon", "K8S_S3_ALLOW_FROM=2001:db8:::1", "'2001:db8:::1' is not an address"},
		{"two IPv6 double colons", "K8S_S3_ALLOW_FROM=2001::db8::1", "'2001::db8::1' is not an address"},
		{"nine IPv6 groups", "K8S_S3_ALLOW_FROM=1:2:3:4:5:6:7:8:9", "'1:2:3:4:5:6:7:8:9' is not an address"},
		{"an IPv6 group too long", "K8S_S3_ALLOW_FROM=12345::1", "'12345::1' is not an address"},
		{"an IPv6 address of colons", "K8S_S3_ALLOW_FROM=:::", "':::' is not an address"},
		{"every IPv4 address", "K8S_S3_ALLOW_FROM=192.0.2.0/24,0.0.0.0/0", "'0.0.0.0/0' is every address: write 'any'"},
		{"every IPv6 address", "K8S_S3_ALLOW_FROM=::/0", "'::/0' is every address: write 'any'"},
		{"an octet with a leading zero", "K8S_S3_ALLOW_FROM=192.0.2.010", "'192.0.2.010' is not an address"},
		{"admin range", "K8S_S3_ALLOW_FROM=any\nK8S_ADMIN_SERVICE_TYPE=LoadBalancer\nK8S_ADMIN_ALLOW_FROM=10.0.0.0/8,", "K8S_ADMIN_ALLOW_FROM=10.0.0.0/8,"},
		{"helm image by tag", "K8S_S3_ALLOW_FROM=any\nHELM_IMAGE=registry.example.test/helm:4", "HELM_IMAGE=registry.example.test/helm:4: must name the image by its digest"},
		{"helm image by a short digest", "K8S_S3_ALLOW_FROM=any\nHELM_IMAGE=registry.example.test/helm@sha256:abcd", "must name the image by its digest"},
		{"helm image by a digest not in hex", "K8S_S3_ALLOW_FROM=any\nHELM_IMAGE=registry.example.test/helm@sha256:" + strings.Repeat("zz", 32), "must name the image by its digest"},
		{"helm and helm image", "K8S_S3_ALLOW_FROM=any\nHELM=helm\nHELM_IMAGE=registry.example.test/helm" + digest, "HELM and HELM_IMAGE are both set"},
		{"vault by name", "K8S_S3_ALLOW_FROM=any\nKMS=vault\nVAULT_ADDR=https://vault.example.test:8200\nVAULT_KEY=k\nVAULT_TOKEN_FILE=~/token", "VAULT_ADDR=https://vault.example.test:8200: must name Vault by its IPv4 address"},
		{"host path relative", "K8S_S3_ALLOW_FROM=any\nK8S_HOSTPATH=srv/cactusfs", "K8S_HOSTPATH=srv/cactusfs: must be an absolute directory"},
		{"host path root", "K8S_S3_ALLOW_FROM=any\nK8S_HOSTPATH=/", "K8S_HOSTPATH=/: must be an absolute directory"},
		{"host path up", "K8S_S3_ALLOW_FROM=any\nK8S_HOSTPATH=/srv/../etc", "K8S_HOSTPATH=/srv/../etc: holds a '..'"},
		{"host path space", "K8S_S3_ALLOW_FROM=any\nK8S_HOSTPATH=/srv/cactus fs", "K8S_HOSTPATH=/srv/cactus fs: may hold"},
	}
	allowed := []string{
		"K8S_S3_ALLOW_FROM=192.0.2.0/24,198.51.100.7,2001:db8::/32,::1,1:2:3:4:5:6:7:8,fe80::1:2/64\n",
		"K8S_SERVICE_TYPE=ClusterIP\n",
		"K8S_SERVICE_TYPE=LoadBalancer\nK8S_S3_ALLOW_FROM=any\nK8S_ADMIN_SERVICE_TYPE=NodePort\nK8S_ADMIN_ALLOW_FROM=192.0.2.0/28\nK8S_ADMIN_NODE_PORT=30901\n",
		"K8S_S3_ALLOW_FROM=any\nHELM_IMAGE=registry.example.test/helm" + digest + "\n",
		"K8S_S3_ALLOW_FROM=any\nKMS=vault\nVAULT_ADDR=https://192.0.2.20\nVAULT_KEY=k\nVAULT_TOKEN_FILE=~/token\n",
	}
	tableShells(t, func(t *testing.T, r *rig, all bool) {
		for i, c := range cases {
			if !all && i > 0 {
				break
			}
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				put(t, filepath.Join(r.home, "token"), "placeholder")
				res := r.withEnv(kmsLine(c.env)+c.env+"\n", "--docker", "k3s", "--install")
				res.want(t, 2, c.want)
				if len(res.calls) != 0 {
					t.Fatalf("refused settings still ran %v", res.calls)
				}
			})
		}
		// What is allowed gets as far as the stub of PLAN 10.10.
		put(t, filepath.Join(r.home, "token"), "placeholder")
		for i, env := range allowed {
			if !all && i > 0 {
				break
			}
			r.withEnv(kmsLine(env)+env, "--docker", "k3s", "--install").want(t, 3, "not built yet (PLAN 10.10)")
		}
	})
}

// renderRules are the answers of an engine that has the helm image, and a
// helm that renders one Service.
func renderRules(extra ...rule) []rule {
	return append(extra,
		rule{Tool: "docker", Contains: []string{"image", "inspect"}, Stdout: "[{}]\n"},
		rule{Tool: "podman", Contains: []string{"image", "inspect"}, Stdout: "[{}]\n"},
		rule{Tool: "docker", Contains: []string{"run"}, Stdout: "kind: Service\n", SaveStdin: "render.tar"},
		rule{Tool: "podman", Contains: []string{"run"}, Stdout: "kind: Service\n", SaveStdin: "render.tar"},
		rule{Tool: "helm", Contains: []string{"template"}, Stdout: "kind: Service\n"},
	)
}

// noImage is the answer of an engine that has no such image.
var noImage = rule{Tool: "docker", Contains: []string{"image", "inspect"}, Stderr: "Error: No such image: x\n", Exit: 1}

// dockerfileHelm is the helm image the Dockerfile pins.
func dockerfileHelm(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^ARG HELM_IMAGE=(\S+)$`).FindSubmatch(repoFile(t, "Dockerfile"))
	if m == nil {
		t.Fatal("the Dockerfile pins no HELM_IMAGE")
	}
	return string(m[1])
}

// tarFiles is every regular file of a tar stream, by name.
func tarFiles(t *testing.T, data []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("the render's standard input is not a tar stream: %v", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[strings.TrimPrefix(h.Name, "./")] = string(body)
	}
}

// emptyDir fails the test unless dir holds only .keep.
func emptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("%d entries were left in %s", len(entries)-1, dir)
	}
}

// TestK8sRender: the chart is rendered by helm template in the pinned helm
// image, through the engine here, with no network, nothing mounted and the
// chart and the values as a tar stream on standard input - or by HELM. The
// image is pulled only when the engine says it has none, and never with
// OFFLINE=yes; an engine that cannot be asked fails the run.
func TestK8sRender(t *testing.T) {
	render := func(values string) string {
		return fmt.Sprintf("k8s_values '%s' 1 machine-1 '%s'; k8s_render '%s' v1.31.0", values, testImage, values)
	}
	helmImage := dockerfileHelm(t)
	cases := []struct {
		name  string
		env   string
		rules []rule
		drop  []string
		check func(t *testing.T, r *rig, res result, tmp string)
	}{
		{name: "in the helm image", env: "K8S_NAMESPACE=store\n", check: func(t *testing.T, r *rig, res result, tmp string) {
			res.want(t, 0)
			if res.stdout != "kind: Service\n" {
				t.Errorf("the render is not what helm printed: %q", res.stdout)
			}
			if len(res.calls) != 2 {
				t.Fatalf("want an image inspect and a run, got %v", res.calls)
			}
			inspect, run := res.calls[0], res.calls[1]
			if inspect.Tool != "docker" || !slices.Equal(inspect.Args, []string{"--context", "default", "image", "inspect", helmImage}) {
				t.Errorf("the image is not looked for first: %s %q", inspect.Tool, inspect.Args)
			}
			want := []string{
				"--context", "default", "run", "--rm", "-i", "--pull=never", "--network", "none", "--read-only", "--tmpfs", "/work",
				"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--env", "HOME=/work", "--entrypoint", "sh", helmImage, "-c",
				`set -e; tar -xo -C /work; exec helm template "$@"`, "sh", "cactusfs", "/work/chart", "--namespace", "store", "-f", "/work/values.yaml", "--kube-version", "v1.31.0",
			}
			if !slices.Equal(run.Args, want) {
				t.Errorf("the render's run is\n%q\nwant\n%q", run.Args, want)
			}
			files := tarFiles(t, []byte(read(t, filepath.Join(r.dir, "render.tar"))))
			if files["values.yaml"] != read(t, filepath.Join(r.dir, "values.yaml")) {
				t.Errorf("the tar's values.yaml is not the values written:\n%s", files["values.yaml"])
			}
			for _, f := range chartFiles(t) {
				name := "chart/" + strings.TrimPrefix(f, "deploy/helm/cactusfs/")
				if files[name] != string(repoFile(t, f)) {
					t.Errorf("the tar's %s is not the chart's %s", name, f)
				}
			}
			for name := range files {
				if strings.Contains(name, "._") {
					t.Errorf("the tar holds %s, metadata of this machine", name)
				}
			}
			emptyDir(t, tmp)
		}},
		{name: "pulled when missing", rules: []rule{noImage}, check: func(t *testing.T, _ *rig, res result, _ string) {
			res.want(t, 0, "run: docker --context default pull "+helmImage)
			var words []string
			for _, c := range res.calls {
				words = append(words, c.Args[2])
			}
			if !slices.Equal(words, []string{"image", "pull", "run"}) {
				t.Errorf("want inspect, pull, run; got %v", res.calls)
			}
		}},
		{name: "an engine that cannot be asked", rules: []rule{{Tool: "docker", Contains: []string{"image", "inspect"}, Stderr: "Cannot connect to the Docker daemon\n", Exit: 1}}, check: func(t *testing.T, _ *rig, res result, tmp string) {
			res.want(t, 1, "asking the engine for "+helmImage+" failed: Cannot connect to the Docker daemon")
			if len(res.calls) != 1 {
				t.Errorf("an engine error was taken for a missing image: %v", res.calls)
			}
			emptyDir(t, tmp)
		}},
		{name: "missing and offline", env: "OFFLINE=yes\n", rules: []rule{noImage}, check: func(t *testing.T, _ *rig, res result, tmp string) {
			res.want(t, 2, "OFFLINE=yes, and the helm image "+helmImage+" is not here")
			if len(res.calls) != 1 {
				t.Errorf("more than the inspect was run: %v", res.calls)
			}
			emptyDir(t, tmp)
		}},
		{name: "a helm image of one's own", env: "HELM_IMAGE=registry.example.test/mirror/helm@sha256:" + strings.Repeat("0a", 32) + "\nENGINE_CONTEXT=desktop-linux\n", check: func(t *testing.T, _ *rig, res result, _ string) {
			res.want(t, 0)
			if !slices.Contains(res.calls[1].Args, "registry.example.test/mirror/helm@sha256:"+strings.Repeat("0a", 32)) || res.calls[1].Args[1] != "desktop-linux" {
				t.Errorf("not run as the image of HELM_IMAGE in the engine of ENGINE_CONTEXT: %q", res.calls[1].Args)
			}
		}},
		{name: "podman when there is no docker", drop: []string{"docker"}, check: func(t *testing.T, _ *rig, res result, _ string) {
			res.want(t, 0)
			if len(res.calls) != 2 || res.calls[1].Tool != "podman" || res.calls[1].Args[0] != "run" {
				t.Errorf("not rendered with podman: %v", res.calls)
			}
		}},
		{name: "no engine", drop: []string{"docker", "podman"}, check: func(t *testing.T, _ *rig, res result, _ string) {
			res.want(t, 2, "rendering the chart needs docker or podman here")
		}},
		{name: "a helm program", env: "HELM=helm\n", check: func(t *testing.T, _ *rig, res result, _ string) {
			res.want(t, 0)
			if len(res.calls) != 1 || res.calls[0].Tool != "helm" {
				t.Fatalf("want helm alone, got %v", res.calls)
			}
			args := res.calls[0].Args
			if len(args) != 9 || args[0] != "template" || args[1] != "cactusfs" || !strings.HasSuffix(args[2], "/chart") ||
				args[3] != "--namespace" || args[4] != "cactusfs" || args[5] != "-f" || !strings.HasSuffix(args[6], "/values.yaml") ||
				args[7] != "--kube-version" || args[8] != "v1.31.0" {
				t.Errorf("helm was asked %q", args)
			}
		}},
	}
	tableShells(t, func(t *testing.T, r *rig, all bool) {
		for i, c := range cases {
			if !all && i > 0 {
				break
			}
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.chart()
				r.without(c.drop...)
				r.rules(renderRules(c.rules...)...)
				tmp := filepath.Join(r.dir, "tmp")
				put(t, filepath.Join(tmp, ".keep"), "")
				res := r.k8s(k8sBase+"K8S_S3_ALLOW_FROM=any\n"+c.env, render(filepath.Join(r.dir, "values.yaml")), false, "TMPDIR="+tmp)
				c.check(t, r, res, tmp)
			})
		}
		t.Run("missing in a dry run", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			r.chart()
			r.rules(renderRules(noImage)...)
			res := r.k8s(k8sBase+"K8S_S3_ALLOW_FROM=any\n", render(filepath.Join(r.dir, "values.yaml")), true)
			res.want(t, 0, "run: docker --context default pull", "dry run: the chart is rendered once")
			if len(res.calls) != 1 {
				t.Errorf("a dry run pulled or rendered: %v", res.calls)
			}
		})
	})
}

// nsJSON is the namespace as "kubectl get namespace -o json" prints it,
// with the marker and the name label when given.
func nsJSON(name, marker, label string) string {
	var b strings.Builder
	b.WriteString("{\n    \"apiVersion\": \"v1\",\n    \"kind\": \"Namespace\",\n    \"metadata\": {\n")
	if marker != "" {
		fmt.Fprintf(&b, "        \"annotations\": {\n            \"deploy.cactusfs.io/owner-uid\": %q\n        },\n", marker)
	}
	b.WriteString("        \"labels\": {\n")
	if label != "" {
		fmt.Fprintf(&b, "            \"io.cactusfs.deploy.name\": %q,\n", label)
	}
	fmt.Fprintf(&b, "            \"kubernetes.io/metadata.name\": %q\n        },\n", name)
	fmt.Fprintf(&b, "        \"name\": %q,\n        \"resourceVersion\": \"41\"\n    }\n}\n", name)
	return b.String()
}

// nsCreate keeps the namespace a create reads (insideNamespace checks it).
var nsCreate = []rule{
	{Tool: "kubectl", Contains: []string{"create", "-f", "-"}, SaveStdin: "namespace.yaml"},
	{Tool: "ssh", Contains: []string{"create", "-f", "-"}, SaveStdin: "namespace.yaml"},
}

// TestK8sNamespace: the namespace is made by a create of an object that
// carries the marker and the restricted label, the marker recorded in
// STATE_DIR first (a record that cannot be written stops the run before
// the create); an existing namespace is the deployment's only when its
// marker is the one recorded, and only then is a Helm release in it looked
// for; one marked with no local record is taken only at a terminal, when
// its marker is of the script's form and its label names the deployment.
func TestK8sNamespace(t *testing.T) {
	const uid = "0123456789abcdef0123456789abcdef"
	other := strings.Repeat("f", 32)
	getNS := func(json string) rule {
		return rule{Tool: "kubectl", Contains: []string{"get", "namespace"}, Stdout: json}
	}
	cases := []struct {
		name     string
		recorded string
		rules    []rule
		dryRun   bool
		input    string
		tty      bool
		blocked  bool
		code     int
		want     string
		created  bool
		adopted  string
		noHelm   bool
	}{
		{name: "made", created: true},
		{name: "made with the recorded marker", recorded: uid, created: true},
		{name: "made in a dry run", dryRun: true, want: "dry run: "},
		{name: "a record that cannot be written", blocked: true, code: 1, want: "writing "},
		{name: "ours", recorded: uid, rules: []rule{getNS(nsJSON("cactusfs", uid, "cactusfs"))}},
		{name: "another's", recorded: uid, rules: []rule{getNS(nsJSON("cactusfs", other, "cactusfs"))}, code: 2, want: "is marked " + other + ", and", noHelm: true},
		{name: "unmarked", recorded: uid, rules: []rule{getNS(nsJSON("cactusfs", "", "cactusfs"))}, code: 2, want: "is not the deployment's (it has no deploy.cactusfs.io/owner-uid)", noHelm: true},
		{name: "unmarked, nothing recorded", rules: []rule{getNS(nsJSON("cactusfs", "", ""))}, code: 2, want: "does not take over a namespace it did not make", noHelm: true},
		{name: "marked, record lost, no terminal", rules: []rule{getNS(nsJSON("cactusfs", other, "cactusfs"))}, code: 2, want: "run at a terminal to confirm", noHelm: true},
		{name: "marked, record lost, confirmed at the terminal", rules: []rule{getNS(nsJSON("cactusfs", other, "cactusfs"))}, tty: true, input: "cactusfs\n", want: "type the deployment's name", adopted: other},
		{name: "marked, record lost, not confirmed", rules: []rule{getNS(nsJSON("cactusfs", other, "cactusfs"))}, tty: true, input: "other\n", code: 2, want: "not confirmed"},
		{name: "marked, record lost, another's label", rules: []rule{getNS(nsJSON("cactusfs", other, "billing"))}, tty: true, input: "cactusfs\n", code: 2, want: "labelled io.cactusfs.deploy.name=billing, not cactusfs", noHelm: true},
		{name: "marked, record lost, a short marker", rules: []rule{getNS(nsJSON("cactusfs", "0123abcd", "cactusfs"))}, tty: true, input: "cactusfs\n", code: 2, want: "carries a marker that is not one the script writes", noHelm: true},
		{name: "a marker the script never writes", rules: []rule{getNS(nsJSON("cactusfs", "$(id)", "cactusfs"))}, code: 2, want: "carries a marker that is not one the script writes", noHelm: true},
		{name: "a Helm release", recorded: uid, rules: []rule{
			getNS(nsJSON("cactusfs", uid, "cactusfs")),
			{Tool: "kubectl", Contains: []string{"get", "secrets"}, Stdout: "secret/sh.helm.release.v1.cactusfs.v1\n"},
		}, code: 2, want: "holds a Helm release named cactusfs"},
		{name: "forbidden", rules: []rule{{Tool: "kubectl", Contains: []string{"get", "namespace"}, Stderr: "Error from server (Forbidden): namespaces \"cactusfs\" is forbidden\n", Exit: 1}}, code: 1, want: "the context may not get the namespace cactusfs (get on namespaces, resourceNames [cactusfs]) (Forbidden)"},
		{name: "the read fails", rules: []rule{{Tool: "kubectl", Contains: []string{"get", "namespace"}, Stderr: "Unauthorized\n", Exit: 1}}, code: 1, want: "reading the namespace cactusfs failed"},
		{name: "the create is forbidden", rules: []rule{{Tool: "kubectl", Contains: []string{"create"}, Stderr: "Error from server (Forbidden): namespaces is forbidden\n", Exit: 1, SaveStdin: "namespace.yaml"}}, code: 1, want: "the context may not create the namespace cactusfs (Forbidden): have the cluster's administrator create it from the manifests deploy/deploy.sh --print-namespace prints"},
		{name: "the create fails", rules: []rule{{Tool: "kubectl", Contains: []string{"create"}, Stderr: "AlreadyExists\n", Exit: 1, SaveStdin: "namespace.yaml"}}, code: 1, want: "one of that name made meanwhile is not taken over"},
		{name: "a recorded marker changed by hand", recorded: "abc", code: 2, want: "does not hold a marker"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.trace = true
				r.input = c.input
				r.rules(append(c.rules, nsCreate...)...)
				state := r.state("k8s-prod", "cactusfs")
				switch {
				case c.recorded != "":
					put(t, filepath.Join(state, "namespace-uid"), c.recorded+"\n")
				case c.blocked:
					put(t, state, "a file where the entry would be")
				}
				code := "k8s_namespace_ensure"
				if c.tty {
					code = "k8s_tty() { return 0; }; " + code
				}
				res := r.k8s(k8sReady, code, c.dryRun)
				res.want(t, c.code, c.want)
				insideNamespace(t, r, res.calls, "cactusfs")
				var creates []call
				for _, call := range res.calls {
					args, _ := kubectlArgs(call)
					if slices.Contains(args, "apply") {
						t.Errorf("the namespace was applied: %q", args)
					}
					if slices.Contains(args, "create") {
						creates = append(creates, call)
					}
					if c.noHelm && slices.Contains(args, "secrets") {
						t.Errorf("the Helm release was looked for in a namespace that is not the deployment's: %q", args)
					}
				}
				if c.adopted != "" {
					if got := strings.TrimSpace(read(t, filepath.Join(state, "namespace-uid"))); got != c.adopted {
						t.Errorf("the marker recorded is %q, want %q", got, c.adopted)
					}
				}
				if !c.created {
					if len(creates) != 0 && c.want != "one of that name made meanwhile is not taken over" && !strings.Contains(c.name, "create is forbidden") {
						t.Fatalf("a namespace was created: %v", creates)
					}
					if c.recorded == "" && c.adopted == "" && !c.blocked && c.want != "one of that name made meanwhile is not taken over" && !strings.Contains(c.name, "create is forbidden") {
						if _, err := os.Stat(filepath.Join(state, "namespace-uid")); !errors.Is(err, os.ErrNotExist) {
							t.Errorf("a marker was recorded: %v", err)
						}
					}
					return
				}
				if len(creates) != 1 || !slices.Equal(creates[0].Args, []string{"--context", "prod", "create", "-f", "-"}) {
					t.Fatalf("want one create of standard input, got %v", creates)
				}
				recorded := strings.TrimSpace(read(t, filepath.Join(state, "namespace-uid")))
				if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(recorded) || (c.recorded != "" && recorded != c.recorded) {
					t.Fatalf("the recorded marker is %q", recorded)
				}
				for path, mode := range map[string]os.FileMode{state: 0o700, filepath.Join(state, "namespace-uid"): 0o600} {
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					if info.Mode().Perm() != mode {
						t.Errorf("%s has mode %v, want %v", path, info.Mode().Perm(), mode)
					}
				}
				manifest := read(t, filepath.Join(r.dir, "namespace.yaml"))
				for _, w := range []string{
					"kind: Namespace\n", "  name: cactusfs\n",
					"    io.cactusfs.deploy.name: 'cactusfs'\n", "    io.cactusfs.deploy.target: 'k8s'\n",
					"    pod-security.kubernetes.io/enforce: restricted\n",
					"  annotations:\n    deploy.cactusfs.io/owner-uid: '" + recorded + "'\n",
				} {
					if !strings.Contains(manifest, w) {
						t.Errorf("the namespace created does not hold %q:\n%s", w, manifest)
					}
				}
			})
		}
		// Over ssh to a k3s server: the same, as plain words, after the
		// host key is checked.
		t.Run("over ssh", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			r.rules(nsCreate...)
			res := r.k8s("DEPLOY_TARGET=k3s\nK3S_SSH=deploy@k3s.example.test\nKMS=local\nK8S_NAMESPACE=store\nK8S_SERVICE_TYPE=ClusterIP\n", "k8s_namespace_ensure", false)
			res.want(t, 0)
			insideNamespace(t, r, res.calls, "store")
			if len(res.calls) != 3 || !slices.Equal(res.calls[0].Args[len(res.calls[0].Args)-1:], []string{"true"}) {
				t.Fatalf("want the host key check, the get and the create: %v", res.calls)
			}
			want := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=20", "--", "deploy@k3s.example.test", "k3s", "kubectl", "create", "-f", "-"}
			if !slices.Equal(res.calls[2].Args, want) {
				t.Errorf("the create over ssh is %q", res.calls[2].Args)
			}
		})
	})
}

// secretKey is one key of a Secret of the deployment and the file of the
// STATE_DIR entry it is read from (PLAN 10.3's layout).
type secretKey struct{ secret, kind, key, file string }

// standaloneKeys and clusterKeys are the Secrets of a deployment on the
// local KMS.
var (
	standaloneKeys = []secretKey{
		{"cactusfs-root", "Opaque", "access-key", "secrets/root-access-key"},
		{"cactusfs-root", "Opaque", "secret-key", "secrets/root-secret-key"},
		{"cactusfs-tls", "kubernetes.io/tls", "tls.crt", "config/s3.pem"},
		{"cactusfs-tls", "kubernetes.io/tls", "tls.key", "config/s3.key"},
		{"cactusfs-kms", "Opaque", "kms.json", "config/kms.json"},
		{"cactusfs-kms-passphrase", "Opaque", "passphrase", "secrets/kms-passphrase"},
	}
	clusterKeys = func() []secretKey {
		keys := slices.Clone(standaloneKeys)
		keys = append(keys, secretKey{"cactusfs-node-ca", "Opaque", "ca.crt", "config/node-ca.pem"})
		for _, id := range []string{"cactusfs-meta-0", "cactusfs-meta-1", "cactusfs-meta-2", "cactusfs-data-0"} {
			keys = append(keys,
				secretKey{id + "-node-tls", "kubernetes.io/tls", "tls.crt", "config/nodes/" + id + ".pem"},
				secretKey{id + "-node-tls", "kubernetes.io/tls", "tls.key", "config/nodes/" + id + ".key"})
		}
		return keys
	}()
	vaultKeys = []secretKey{
		{"cactusfs-root", "Opaque", "access-key", "secrets/root-access-key"},
		{"cactusfs-root", "Opaque", "secret-key", "secrets/root-secret-key"},
		{"cactusfs-tls", "kubernetes.io/tls", "tls.crt", "config/s3.pem"},
		{"cactusfs-tls", "kubernetes.io/tls", "tls.key", "config/s3.key"},
	}
)

// secretFixtures writes a value made for the run into each file of the
// deployment's STATE_DIR entry the keys are read from, and returns them by
// their path within the entry.
func secretFixtures(t *testing.T, state string, keys []secretKey) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, k := range keys {
		out[k.file] = "fixture-" + rand.Text()
		put(t, filepath.Join(state, k.file), out[k.file])
	}
	return out
}

// secretJSON is a Secret as "kubectl get secret -o json" prints it: its
// type, the values of its keys (encoded), labelled with the deployment's
// name unless label is empty, and a label of another key that holds the
// name, which must not be taken for it.
func secretJSON(name, kind, label string, data map[string]string) string {
	var b strings.Builder
	b.WriteString("{\n    \"apiVersion\": \"v1\",\n    \"data\": {\n")
	keys := slices.Sorted(maps.Keys(data))
	for i, k := range keys {
		sep := ","
		if i == len(keys)-1 {
			sep = ""
		}
		fmt.Fprintf(&b, "        %q: %q%s\n", k, base64.StdEncoding.EncodeToString([]byte(data[k])), sep)
	}
	b.WriteString("    },\n    \"kind\": \"Secret\",\n    \"metadata\": {\n")
	b.WriteString("        \"annotations\": {\n            \"io.cactusfs.deploy.name\": \"cactusfs\"\n        },\n")
	b.WriteString("        \"labels\": {\n")
	if label != "" {
		fmt.Fprintf(&b, "            \"io.cactusfs.deploy.name\": %q,\n", label)
	}
	b.WriteString("            \"other\": \"x\"\n        },\n")
	fmt.Fprintf(&b, "        \"name\": %q,\n        \"namespace\": \"cactusfs\",\n        \"resourceVersion\": \"7%d\"\n    },\n", name, len(name))
	fmt.Fprintf(&b, "    \"type\": %q\n}\n", kind)
	return b.String()
}

// clusterHas are the rules of a cluster whose namespace holds the Secrets
// of the keys with the values given (by file), labelled as the deployment's.
func clusterHas(keys []secretKey, values map[string]string) []rule {
	data := map[string]map[string]string{}
	kinds := map[string]string{}
	var order []string
	for _, k := range keys {
		if data[k.secret] == nil {
			data[k.secret] = map[string]string{}
			order = append(order, k.secret)
		}
		data[k.secret][k.key] = values[k.file]
		kinds[k.secret] = k.kind
	}
	var rules []rule
	for _, name := range order {
		rules = append(rules, rule{Tool: "kubectl", Contains: []string{"get", "secret", name}, Stdout: secretJSON(name, kinds[name], "cactusfs", data[name])})
	}
	return rules
}

// secretDocs splits the Secrets written into their documents, by name.
func secretDocs(t *testing.T, stream string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, doc := range strings.Split(stream, "---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		m := regexp.MustCompile(`(?m)^  name: (\S+)$`).FindStringSubmatch(doc)
		if m == nil {
			t.Fatalf("a document without a name:\n%s", doc)
		}
		out[m[1]] = doc
	}
	return out
}

// writes are the calls that change something on the cluster.
func writes(calls []call) []call {
	var out []call
	for _, c := range calls {
		if args, ok := kubectlArgs(c); ok && len(args) > 0 && slices.Contains([]string{"create", "apply", "replace", "patch", "delete"}, args[0]) {
			out = append(out, c)
		}
	}
	return out
}

// noLeak fails the test when a value, or its base64, is in the output of
// the run (a trace included) or on a command line.
func noLeak(t *testing.T, res result, values map[string]string) {
	t.Helper()
	if !strings.Contains(res.stderr, "+ ") {
		t.Fatal("the run was not traced: nothing is proven")
	}
	for file, value := range values {
		encoded := base64.StdEncoding.EncodeToString([]byte(value))
		if strings.Contains(res.output(), value) || strings.Contains(res.output(), encoded) {
			t.Errorf("the secret of %s is in the output or the trace", file)
		}
		for _, call := range res.calls {
			for _, a := range call.Args {
				if strings.Contains(a, value) || strings.Contains(a, encoded) {
					t.Errorf("the secret of %s is on the command line of %s", file, call.Tool)
				}
			}
		}
	}
}

// noWorkDir fails the test when a work directory is left in the entry.
func noWorkDir(t *testing.T, state string) {
	t.Helper()
	entries, err := os.ReadDir(state)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".k8s") {
			t.Errorf("the comparison left %s in the entry", e.Name())
		}
	}
}

// TestK8sSecrets: the Secrets of a deployment are read back before anything
// is written; the cluster's copy of a secret the data depends on is never
// replaced - a value that differs is refused, one the cluster alone has is
// copied back, one it lacks is created (never applied); a certificate or
// token given by its *_FILE setting is rotated with kubectl replace at the
// version read back; an empty or unreadable local copy is refused; no
// secret is on a command line, in the output or in a trace (ADR-0033
// section 3).
func TestK8sSecrets(t *testing.T) {
	createArgs := []string{"--context", "prod", "create", "--field-manager=cactusfs-deploy", "-n", "cactusfs", "-f", "-"}
	replaceArgs := []string{"--context", "prod", "replace", "--field-manager=cactusfs-deploy", "-n", "cactusfs", "-f", "-"}
	saving := []rule{
		{Tool: "kubectl", Contains: []string{"create", "--field-manager=cactusfs-deploy"}, SaveStdin: "created.yaml"},
		{Tool: "kubectl", Contains: []string{"replace"}, SaveStdin: "replaced.yaml"},
	}
	without := func(keys []secretKey, secrets ...string) []secretKey {
		var out []secretKey
		for _, k := range keys {
			if !slices.Contains(secrets, k.secret) {
				out = append(out, k)
			}
		}
		return out
	}
	cases := []secretCase{
		{
			name: "created, standalone", keys: standaloneKeys, want: []string{"are created in the namespace cactusfs"},
			check: func(t *testing.T, s secretSetup, res result) {
				w := writes(res.calls)
				if len(w) != 1 || !slices.Equal(w[0].Args, createArgs) {
					t.Fatalf("want one create of standard input, got %v", w)
				}
				checkCreated(t, s.r, standaloneKeys, s.values)
				gets := 0
				for _, call := range res.calls {
					if args, _ := kubectlArgs(call); len(args) > 2 && args[0] == "get" && args[1] == "secret" {
						gets++
						if !slices.Contains(args, "--ignore-not-found") {
							t.Errorf("a read of %q", args)
						}
					}
				}
				if gets != 4 {
					t.Errorf("%d Secrets read back, want 4", gets)
				}
			},
		},
		{
			name: "created, cluster", env: "DEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\n", keys: clusterKeys, want: []string{"are created"},
			check: func(t *testing.T, s secretSetup, _ result) { checkCreated(t, s.r, clusterKeys, s.values) },
		},
		{
			name: "a second run changes nothing", keys: standaloneKeys,
			prepare: func(_ *testing.T, s secretSetup) []rule { return clusterHas(standaloneKeys, s.values) },
			check: func(t *testing.T, _ secretSetup, res result) {
				if w := writes(res.calls); len(w) != 0 {
					t.Errorf("an unchanged deployment wrote %v", w)
				}
			},
		},
		{
			name: "the cluster lacks one", keys: standaloneKeys,
			prepare: func(_ *testing.T, s secretSetup) []rule {
				return clusterHas(without(standaloneKeys, "cactusfs-tls"), s.values)
			},
			check: func(t *testing.T, s secretSetup, _ result) {
				docs := secretDocs(t, read(t, filepath.Join(s.r.dir, "created.yaml")))
				if len(docs) != 1 || docs["cactusfs-tls"] == "" {
					t.Errorf("want cactusfs-tls created alone, got %v", docs)
				}
			},
		},
		{
			name: "a read that fails", keys: standaloneKeys, code: 1, want: []string{"reading the Secret cactusfs-kms of the namespace cactusfs failed; nothing was changed"},
			prepare: func(_ *testing.T, _ secretSetup) []rule {
				return []rule{{Tool: "kubectl", Contains: []string{"get", "secret", "cactusfs-kms"}, Stderr: "Unauthorized\n", Exit: 1}}
			},
		},
		{
			name: "a read that is forbidden", keys: standaloneKeys, code: 1, want: []string{"the context may not get the Secret cactusfs-root in the namespace cactusfs (Forbidden)"},
			prepare: func(_ *testing.T, _ secretSetup) []rule {
				return []rule{{Tool: "kubectl", Contains: []string{"get", "secret"}, Stderr: "Error from server (Forbidden): secrets is forbidden\n", Exit: 1}}
			},
		},
		differs("a key file that differs", standaloneKeys, "", "cactusfs-kms", "config/kms.json"),
		differs("a passphrase that differs", standaloneKeys, "", "cactusfs-kms-passphrase", "secrets/kms-passphrase"),
		differs("root credentials that differ", standaloneKeys, "", "cactusfs-root", "secrets/root-secret-key"),
		differs("a development certificate that differs", standaloneKeys, "", "cactusfs-tls", "config/s3.key"),
		differs("a node CA that differs", clusterKeys, "DEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\n", "cactusfs-node-ca", "config/node-ca.pem"),
		differs("a node key that differs", clusterKeys, "DEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\n", "cactusfs-meta-1-node-tls", "config/nodes/cactusfs-meta-1.key"),
		{
			name: "a differing key file in a dry run", keys: standaloneKeys, dryRun: true, code: 2, want: []string{"hold different values of kms.json"},
			prepare: func(_ *testing.T, s secretSetup) []rule {
				theirs := maps.Clone(s.values)
				theirs["config/kms.json"] = "fixture-" + rand.Text()
				return clusterHas(standaloneKeys, theirs)
			},
		},
		{
			name: "an empty passphrase", keys: standaloneKeys, code: 2, want: []string{"secrets/kms-passphrase, the local copy of passphrase of the Secret cactusfs-kms-passphrase, is empty, unreadable or not a file"},
			prepare: func(t *testing.T, s secretSetup) []rule {
				put(t, filepath.Join(s.state, "secrets", "kms-passphrase"), "")
				return nil
			},
		},
		{
			name: "an unreadable key file", keys: standaloneKeys, code: 2, want: []string{"config/kms.json, the local copy of kms.json of the Secret cactusfs-kms, is empty, unreadable or not a file"},
			prepare: func(t *testing.T, s secretSetup) []rule {
				path := filepath.Join(s.state, "config", "kms.json")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				// A link that leads nowhere: unreadable for root as well.
				if err := os.Symlink(filepath.Join(s.state, "nowhere"), path); err != nil {
					t.Fatal(err)
				}
				return clusterHas(standaloneKeys, s.values)
			},
		},
		{
			name: "a key file that is a directory", keys: standaloneKeys, code: 2, want: []string{"is empty, unreadable or not a file"},
			prepare: func(t *testing.T, s secretSetup) []rule {
				path := filepath.Join(s.state, "config", "kms.json")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return nil
			},
		},
		{
			name: "a comparison that fails", keys: standaloneKeys, code: 1, want: []string{"comparing ", "failed"},
			prepare: func(_ *testing.T, s secretSetup) []rule {
				s.r.fake("cmp")
				return append(clusterHas(standaloneKeys, s.values), rule{Tool: "cmp", Stderr: "cmp: I/O error\n", Exit: 2})
			},
		},
		{
			name: "the cluster's copy only", keys: standaloneKeys, want: []string{"config/kms.json was missing here: copied back from the Secret cactusfs-kms"},
			prepare: func(t *testing.T, s secretSetup) []rule {
				for _, f := range []string{"config/kms.json", "secrets/kms-passphrase"} {
					if err := os.Remove(filepath.Join(s.state, f)); err != nil {
						t.Fatal(err)
					}
				}
				return clusterHas(standaloneKeys, s.values)
			},
			check: func(t *testing.T, s secretSetup, res result) {
				if w := writes(res.calls); len(w) != 0 {
					t.Errorf("wrote to the cluster: %v", w)
				}
				for _, f := range []string{"config/kms.json", "secrets/kms-passphrase"} {
					path := filepath.Join(s.state, f)
					if read(t, path) != s.values[f] {
						t.Errorf("%s was not copied back as the cluster has it", f)
					}
					if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
						t.Errorf("%s: %v %v", f, info, err)
					}
				}
			},
		},
		{
			name: "the cluster's copy only, in a dry run", keys: standaloneKeys, dryRun: true, want: []string{"config/kms.json is not written"},
			prepare: func(t *testing.T, s secretSetup) []rule {
				if err := os.Remove(filepath.Join(s.state, "config", "kms.json")); err != nil {
					t.Fatal(err)
				}
				return clusterHas(standaloneKeys, s.values)
			},
			check: func(t *testing.T, s secretSetup, _ result) {
				if _, err := os.Stat(filepath.Join(s.state, "config", "kms.json")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("a dry run copied back: %v", err)
				}
			},
		},
		{
			name: "copied back before the hook", keys: standaloneKeys, want: []string{"hook saw kms.json", "are created"},
			prepare: func(t *testing.T, s secretSetup) []rule {
				// The cluster has the key file and its passphrase; the root
				// credentials are in neither copy.
				for _, f := range []string{"config/kms.json", "secrets/kms-passphrase", "secrets/root-access-key", "secrets/root-secret-key"} {
					if err := os.Remove(filepath.Join(s.state, f)); err != nil {
						t.Fatal(err)
					}
				}
				return clusterHas(without(standaloneKeys, "cactusfs-root", "cactusfs-tls"), s.values)
			},
			check: func(t *testing.T, s secretSetup, _ result) {
				if got := read(t, filepath.Join(s.r.dir, "hook.list")); strings.Contains(got, "kms") || !strings.Contains(got, "secrets/root-access-key") {
					t.Errorf("the hook was handed %q: only what is missing from both copies", got)
				}
				if read(t, filepath.Join(s.state, "config", "kms.json")) != s.values["config/kms.json"] {
					t.Error("the key file is not the cluster's")
				}
				docs := secretDocs(t, read(t, filepath.Join(s.r.dir, "created.yaml")))
				if _, ok := docs["cactusfs-kms"]; ok || docs["cactusfs-root"] == "" {
					t.Errorf("want the root Secret created and not the key file's: %v", slices.Sorted(maps.Keys(docs)))
				}
			},
		},
		{
			name: "an empty value in the cluster's copy", keys: standaloneKeys, code: 2, want: []string{"the Secret cactusfs-kms-passphrase of the namespace cactusfs holds an empty value: not copied back"},
			prepare: func(t *testing.T, s secretSetup) []rule {
				if err := os.Remove(filepath.Join(s.state, "secrets", "kms-passphrase")); err != nil {
					t.Fatal(err)
				}
				theirs := maps.Clone(s.values)
				theirs["secrets/kms-passphrase"] = ""
				return clusterHas(standaloneKeys, theirs)
			},
			check: func(t *testing.T, s secretSetup, _ result) {
				if _, err := os.Stat(filepath.Join(s.state, "secrets", "kms-passphrase")); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("an empty passphrase was copied back: %v", err)
				}
			},
		},
		{
			name: "not the deployment's", keys: standaloneKeys, code: 2, want: []string{"has a Secret cactusfs-root without the label io.cactusfs.deploy.name=cactusfs"},
			prepare: func(_ *testing.T, s secretSetup) []rule {
				return []rule{{Tool: "kubectl", Contains: []string{"get", "secret", "cactusfs-root"}, Stdout: secretJSON("cactusfs-root", "Opaque", "", map[string]string{"access-key": s.values["secrets/root-access-key"], "secret-key": s.values["secrets/root-secret-key"]})}}
			},
		},
		{
			name: "another type", keys: standaloneKeys, code: 2, want: []string{"is of the type Opaque, not kubernetes.io/tls"},
			prepare: func(_ *testing.T, s secretSetup) []rule {
				return []rule{{Tool: "kubectl", Contains: []string{"get", "secret", "cactusfs-tls"}, Stdout: secretJSON("cactusfs-tls", "Opaque", "cactusfs", map[string]string{"tls.crt": s.values["config/s3.pem"], "tls.key": s.values["config/s3.key"]})}}
			},
		},
		{
			name: "a key the cluster's Secret lacks", keys: standaloneKeys, code: 2, want: []string{"has no key secret-key"},
			prepare: func(_ *testing.T, s secretSetup) []rule {
				return []rule{{Tool: "kubectl", Contains: []string{"get", "secret", "cactusfs-root"}, Stdout: secretJSON("cactusfs-root", "Opaque", "cactusfs", map[string]string{"access-key": s.values["secrets/root-access-key"]})}}
			},
		},
		{
			name: "a certificate of one's own is rotated", keys: standaloneKeys, want: []string{"rotated: the Secrets cactusfs-tls in the namespace cactusfs are replaced"},
			env: "TLS_CERT_FILE=~/own.crt\nTLS_KEY_FILE=~/own.key\n",
			prepare: func(t *testing.T, s secretSetup) []rule {
				put(t, filepath.Join(s.r.home, "own.crt"), "fixture-own-crt-"+s.values["config/s3.pem"])
				put(t, filepath.Join(s.r.home, "own.key"), "fixture-own-key-"+s.values["config/s3.key"])
				return clusterHas(standaloneKeys, s.values)
			},
			check: func(t *testing.T, s secretSetup, res result) {
				w := writes(res.calls)
				if len(w) != 1 || !slices.Equal(w[0].Args, replaceArgs) {
					t.Fatalf("want one replace, got %v", w)
				}
				docs := secretDocs(t, read(t, filepath.Join(s.r.dir, "replaced.yaml")))
				doc := docs["cactusfs-tls"]
				if len(docs) != 1 || !strings.Contains(doc, "  tls.crt: "+base64.StdEncoding.EncodeToString([]byte("fixture-own-crt-"+s.values["config/s3.pem"]))+"\n") ||
					!strings.Contains(doc, "  resourceVersion: '712'\n") {
					t.Errorf("want cactusfs-tls alone, from the files given, at the version read back:\n%v", docs)
				}
			},
		},
		{
			name: "a Vault token of one's own is rotated", keys: vaultKeys, want: []string{"rotated: the Secrets cactusfs-vault-token in the namespace"},
			env: "KMS=vault\nVAULT_ADDR=https://192.0.2.20:8200\nVAULT_KEY=k\nVAULT_TOKEN_FILE=~/token\n",
			prepare: func(t *testing.T, s secretSetup) []rule {
				put(t, filepath.Join(s.r.home, "token"), "fixture-new-"+rand.Text())
				return append(clusterHas(vaultKeys, s.values), rule{
					Tool: "kubectl", Contains: []string{"get", "secret", "cactusfs-vault-token"},
					Stdout: secretJSON("cactusfs-vault-token", "Opaque", "cactusfs", map[string]string{"token": "fixture-old-" + rand.Text()}),
				})
			},
			check: func(t *testing.T, s secretSetup, res result) {
				w := writes(res.calls)
				if len(w) != 1 || !slices.Equal(w[0].Args, replaceArgs) {
					t.Fatalf("want one replace, got %v", w)
				}
				doc := secretDocs(t, read(t, filepath.Join(s.r.dir, "replaced.yaml")))["cactusfs-vault-token"]
				if !strings.Contains(doc, "  token: "+base64.StdEncoding.EncodeToString([]byte(read(t, filepath.Join(s.r.home, "token"))))+"\n") {
					t.Errorf("the token is not the file's:\n%s", doc)
				}
			},
		},
		{name: "neither copy", keys: nil, code: 3, want: []string{"not built yet (PLAN 10.3 for Kubernetes)"}},
		{
			name: "files the settings name", keys: nil,
			env: "ROOT_ACCESS_KEY_FILE=~/a\nROOT_SECRET_KEY_FILE=~/b\nTLS_CERT_FILE=~/c\nTLS_KEY_FILE=~/d\nKMS_PASSPHRASE_FILE=~/e\nKMS_KEY_FILE=~/f\n",
			prepare: func(t *testing.T, s secretSetup) []rule {
				for _, f := range []string{"a", "b", "c", "d", "e", "f"} {
					s.values[f] = "fixture-" + rand.Text()
					put(t, filepath.Join(s.r.home, f), s.values[f])
				}
				return nil
			},
			check: func(t *testing.T, s secretSetup, _ result) {
				stream := read(t, filepath.Join(s.r.dir, "created.yaml"))
				for f, value := range s.values {
					if !strings.Contains(stream, base64.StdEncoding.EncodeToString([]byte(value))) {
						t.Errorf("the file %s was not used", f)
					}
				}
			},
		},
		{
			name: "a dry run", keys: standaloneKeys, dryRun: true, want: []string{"run: kubectl --context prod create --field-manager=cactusfs-deploy -n cactusfs -f -"},
			check: func(t *testing.T, _ secretSetup, res result) {
				if w := writes(res.calls); len(w) != 0 {
					t.Errorf("a dry run wrote: %v", w)
				}
			},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.trace = true
				s := secretSetup{r: r, state: r.state("k8s-prod", "cactusfs")}
				s.values = secretFixtures(t, s.state, c.keys)
				var rules []rule
				if c.prepare != nil {
					rules = c.prepare(t, s)
				}
				r.rules(append(rules, saving...)...)
				code := "k8s_secrets_ensure"
				if c.name == "copied back before the hook" {
					code = "secrets_ensure_k8s() { cat \"$1\" >'" + filepath.Join(r.dir, "hook.list") + "'; [ -s \"$STATE/config/kms.json\" ] && echo hook saw kms.json; " +
						"printf made >\"$STATE/secrets/root-access-key\"; printf made >\"$STATE/secrets/root-secret-key\"; }; " + code
				}
				env := k8sReady + c.env
				if strings.Contains(c.env, "KMS=vault") {
					env = "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_S3_ALLOW_FROM=192.0.2.0/24\n" + c.env
				}
				res := r.k8s(env, code, c.dryRun)
				res.want(t, c.code, c.want...)
				insideNamespace(t, r, res.calls, "cactusfs")
				if c.code != 0 {
					if w := writes(res.calls); len(w) != 0 {
						t.Errorf("a refused or failed run wrote %v", w)
					}
				}
				if c.check != nil {
					c.check(t, s, res)
				}
				noLeak(t, res, s.values)
				noWorkDir(t, s.state)
			})
		}
	})
}

// secretSetup is what a case of TestK8sSecrets prepares and checks: its
// rig, the deployment's STATE_DIR entry and the values written there.
type secretSetup struct {
	r      *rig
	state  string
	values map[string]string
}

// secretCase is a case of TestK8sSecrets: the env file's lines, the keys
// whose files the entry holds, what the case changes and how the cluster
// answers, and what the run must end with.
type secretCase struct {
	name    string
	env     string
	keys    []secretKey
	prepare func(t *testing.T, s secretSetup) []rule
	dryRun  bool
	code    int
	want    []string
	check   func(t *testing.T, s secretSetup, res result)
}

// differs is a case of TestK8sSecrets: the cluster's copy of one key holds
// another value than the entry's, which is refused and changes nothing.
func differs(name string, keys []secretKey, env, secret, file string) secretCase {
	return secretCase{
		name: name, env: env, keys: keys, code: 2,
		want: []string{"the Secret " + secret + " of the namespace cactusfs and ", file + " hold different values", "nothing was changed"},
		prepare: func(_ *testing.T, s secretSetup) []rule {
			theirs := maps.Clone(s.values)
			theirs[file] = "fixture-" + rand.Text()
			s.values["theirs"] = theirs[file]
			return clusterHas(keys, theirs)
		},
		check: func(t *testing.T, s secretSetup, _ result) {
			if read(t, filepath.Join(s.state, file)) == s.values["theirs"] {
				t.Error("the local copy was changed")
			}
		},
	}
}

// checkCreated fails the test unless the Secrets created hold every key's
// value, with the deployment's labels, in the namespace.
func checkCreated(t *testing.T, r *rig, keys []secretKey, values map[string]string) {
	t.Helper()
	docs := secretDocs(t, read(t, filepath.Join(r.dir, "created.yaml")))
	names := map[string]bool{}
	for _, k := range keys {
		names[k.secret] = true
		doc := docs[k.secret]
		want := "  " + k.key + ": " + base64.StdEncoding.EncodeToString([]byte(values[k.file])) + "\n"
		for _, s := range []string{
			"kind: Secret\n", "  namespace: cactusfs\n", "type: " + k.kind + "\n", want,
			"    io.cactusfs.deploy.name: 'cactusfs'\n", "    app.kubernetes.io/managed-by: cactusfs-deploy\n",
		} {
			if !strings.Contains(doc, s) {
				t.Errorf("the Secret %s does not hold %q:\n%s", k.secret, s, doc)
			}
		}
		if strings.Contains(doc, "resourceVersion") {
			t.Errorf("a created Secret names a resourceVersion:\n%s", doc)
		}
	}
	if len(docs) != len(names) {
		t.Errorf("%d Secrets created, want %d", len(docs), len(names))
	}
}

// TestK8sSecretsYAMLOnlyFromWholeFiles: the YAML of the Secrets is written
// only when every file was encoded - an empty or missing file fails it, so
// a stream cut short never reaches kubectl.
func TestK8sSecretsYAMLOnlyFromWholeFiles(t *testing.T) {
	tableShells(t, func(t *testing.T, r *rig, _ bool) {
		state := r.state("k8s-prod", "cactusfs")
		secretFixtures(t, state, standaloneKeys)
		put(t, filepath.Join(state, "secrets", "kms-passphrase"), "")
		res := r.k8s(k8sReady, "d=$(mktemp -d); if k8s_secrets_yaml \"$d\" >\"$d/out\"; then echo whole; else echo refused; fi", false)
		res.want(t, 0, "refused")
		if strings.Contains(res.stdout, "whole") {
			t.Error("the YAML was written from an empty file")
		}
	})
}

// TestK8sStaysInItsNamespace: the steps of an install that PLAN 10.9 builds,
// one after another, write only in the namespace and read outside it only
// what ADR-0033 section 7 allows - here and over ssh.
func TestK8sStaysInItsNamespace(t *testing.T) {
	const sequence = "n=$(k8s_nodes) || exit; set -- $n; k8s_storage_check; k8s_namespace_ensure; k8s_secrets_ensure; k8s_values \"$STATE/values.yaml\" \"$1\" \"$2\" '" + testImage + "'"
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range []struct{ name, tool, env, target string }{
			{"context", "kubectl", k8sBase, "k8s-prod"},
			{"ssh", "ssh", "DEPLOY_TARGET=k3s\nK3S_SSH=deploy@k3s.example.test\nKMS=local\n", "ssh-deploy_k3s.example.test"},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.rules(append([]rule{
					{Tool: c.tool, Contains: []string{"get", "nodes"}, Stdout: "node/machine-1\n"},
					{Tool: c.tool, Contains: []string{"get", "storageclasses"}, Stdout: "{\n    \"metadata\": {\n        \"annotations\": {\n            \"storageclass.kubernetes.io/is-default-class\": \"true\"\n"},
					{Tool: c.tool, Contains: []string{"create", "--field-manager=cactusfs-deploy"}},
				}, nsCreate...)...)
				secretFixtures(t, r.state(c.target, "cactusfs"), standaloneKeys)
				res := r.k8s(c.env+"K8S_S3_ALLOW_FROM=192.0.2.0/24\n", sequence, false)
				res.want(t, 0)
				insideNamespace(t, r, res.calls, "cactusfs")
				if !strings.Contains(read(t, filepath.Join(r.state(c.target, "cactusfs"), "values.yaml")), "spread: {whenUnsatisfiable: ScheduleAnyway}") {
					t.Error("one node, and the spread is not relaxed")
				}
			})
		}
	})
	tableShells(t, func(t *testing.T, r *rig, all bool) {
		cases := []struct {
			name, env, code string
			rules           []rule
			exit            int
			want            string
		}{
			{"no default storage class", "", "k8s_storage_check", []rule{{Tool: "kubectl", Contains: []string{"get", "storageclasses"}, Stdout: "{}\n"}}, 2, "the cluster has no default StorageClass"},
			{"a beta default storage class", "", "k8s_storage_check", []rule{{Tool: "kubectl", Contains: []string{"get", "storageclasses"}, Stdout: "            \"storageclass.beta.kubernetes.io/is-default-class\": \"true\"\n"}}, 0, ""},
			{"a storage class that is not there", "K8S_STORAGE_CLASS=fast\n", "k8s_storage_check", []rule{{Tool: "kubectl", Contains: []string{"get", "storageclasses"}, Stdout: "storageclass.storage.k8s.io/local-path\n"}}, 2, "K8S_STORAGE_CLASS=fast: the cluster has no StorageClass of that name"},
			{"a storage class that is there", "K8S_STORAGE_CLASS=local-path\n", "k8s_storage_check", []rule{{Tool: "kubectl", Contains: []string{"get", "storageclasses"}, Stdout: "storageclass.storage.k8s.io/local-path\n"}}, 0, ""},
			{"no node", "", "n=$(k8s_nodes) || exit; echo not reached", []rule{{Tool: "kubectl", Contains: []string{"get", "nodes"}, Stdout: ""}}, 1, "the cluster lists no node"},
			{"a node name that is no name", "K8S_STORAGE=hostpath\n", "n=$(k8s_nodes) || exit; set -- $n; k8s_values \"$STATE.values\" \"$1\" \"$2\" '" + testImage + "'", []rule{{Tool: "kubectl", Contains: []string{"get", "nodes"}, Stdout: "node/Machine;1\n"}}, 1, "is not a node name"},
		}
		for i, c := range cases {
			if !all && i > 0 {
				break
			}
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.rules(c.rules...)
				res := r.k8s(k8sBase+"K8S_S3_ALLOW_FROM=any\n"+c.env, c.code, false)
				res.want(t, c.exit, c.want)
				if strings.Contains(res.stdout, "not reached") {
					t.Error("a failure in a command substitution was lost")
				}
			})
		}
	})
}

// TestPrintNamespace: --print-namespace writes the manifests of the
// namespace - marker and restricted label included - and of the Role and
// ClusterRole a namespaced context needs, on standard output and nothing
// else there, and says on standard error how to bind them; the marker is
// recorded in STATE_DIR, the same at every run; nothing is asked of the
// cluster.
func TestPrintNamespace(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		args := []string{"--docker", "k8s", "--print-namespace"}
		env := "KMS=local\nK8S_CONTEXT=team-a\nK8S_NAMESPACE=store\n"
		res := r.withEnv(env, args...)
		res.want(t, 0, "kubectl create rolebinding cactusfs-deploy -n store --role=cactusfs-deploy --user=<identity>",
			"kubectl create clusterrolebinding cactusfs-deploy-store --clusterrole=cactusfs-deploy-store --user=<identity>")
		if len(res.calls) != 0 {
			t.Fatalf("--print-namespace ran %v", res.calls)
		}
		state := r.state("k8s-team-a", "cactusfs")
		uid := strings.TrimSpace(read(t, filepath.Join(state, "namespace-uid")))
		want := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: store\n  labels:\n" +
			"    io.cactusfs.deploy.name: 'cactusfs'\n    io.cactusfs.deploy.target: 'k8s'\n" +
			"    pod-security.kubernetes.io/enforce: restricted\n  annotations:\n" +
			"    deploy.cactusfs.io/owner-uid: '" + uid + "'\n---\n"
		if !strings.HasPrefix(res.stdout, want) || strings.Contains(res.stdout, "deploy:") {
			t.Fatalf("standard output is not the manifests alone:\n%s\nwant first:\n%s", res.stdout, want)
		}
		for _, w := range []string{
			"kind: Role\nmetadata:\n  name: cactusfs-deploy\n  namespace: store\n",
			"kind: ClusterRole\nmetadata:\n  name: cactusfs-deploy-store\n",
			"  - apiGroups: [\"\"]\n    resources: [namespaces]\n    resourceNames: [store, kube-system]\n    verbs: [get]\n",
			"  - apiGroups: [\"\"]\n    resources: [nodes, services, persistentvolumes]\n    verbs: [list]\n",
			"  - apiGroups: [storage.k8s.io]\n    resources: [storageclasses]\n    verbs: [list]\n",
		} {
			if !strings.Contains(res.stdout, w) {
				t.Errorf("the manifests do not hold %q:\n%s", w, res.stdout)
			}
		}
		if info, err := os.Stat(filepath.Join(state, "namespace-uid")); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("the marker's file: %v %v", info, err)
		}
		again := r.withEnv(env, args...)
		again.want(t, 0)
		if again.stdout != res.stdout {
			t.Errorf("a second run printed another marker:\n%s", again.stdout)
		}
		// A dry run records nothing and says so.
		r2 := r.fresh(t)
		dry := r2.withEnv(env, append(args, "--dry-run")...)
		dry.want(t, 0, "the marker", "is not recorded")
		if _, err := os.Stat(filepath.Join(r2.state("k8s-team-a", "cactusfs"), "namespace-uid")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a dry run recorded the marker: %v", err)
		}
		// A record that cannot be written ends the run without a manifest.
		r3 := r.fresh(t)
		put(t, r3.state("k8s-team-a", "cactusfs"), "a file where the entry would be")
		blocked := r3.withEnv(env, args...)
		blocked.want(t, 1, "writing ")
		if strings.Contains(blocked.stdout, "kind: Namespace") {
			t.Error("a manifest was printed whose marker was not recorded")
		}
	})
}

// profiles are the env files of test/deploy/testdata/k8s, by name, and the
// number of nodes each is rendered for (its "# nodes: N" line).
func profiles(t *testing.T) map[string]struct {
	env   string
	nodes int
} {
	t.Helper()
	root, err := os.OpenRoot(filepath.Join("testdata", "k8s"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	paths, err := fs.Glob(root.FS(), "*.settings")
	if err != nil || len(paths) < 4 {
		t.Fatalf("test/deploy/testdata/k8s holds %d profiles: %v", len(paths), err)
	}
	out := map[string]struct {
		env   string
		nodes int
	}{}
	for _, p := range paths {
		data, err := root.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(`(?m)^# nodes: ([0-9]+)$`).FindSubmatch(data)
		if m == nil {
			t.Fatalf("%s does not say how many nodes it is rendered for (# nodes: N)", p)
		}
		n, err := strconv.Atoi(string(m[1]))
		if err != nil {
			t.Fatal(err)
		}
		out[strings.TrimSuffix(filepath.Base(p), ".settings")] = struct {
			env   string
			nodes int
		}{string(data), n}
	}
	return out
}

// profileFiles writes, under the rig's home, a placeholder for every file a
// profile names with "~/": no profile holds or names a secret of its own.
func profileFiles(t *testing.T, r *rig, env string) {
	t.Helper()
	for _, m := range regexp.MustCompile(`(?m)^[A-Z_]+_FILE=~/(\S+)$`).FindAllStringSubmatch(env, -1) {
		put(t, filepath.Join(r.home, m[1]), "placeholder")
	}
}

// TestProfiles: every profile of test/deploy/testdata/k8s is an env file the
// script takes and writes values for; that the chart renders each and the
// render is valid is TestProfilesRender (tag deployrender, make
// deploy-check). The profiles are files git keeps.
func TestProfiles(t *testing.T) {
	for name := range profiles(t) {
		path := "test/deploy/testdata/k8s/" + name + ".settings"
		cmd := exec.CommandContext(t.Context(), "git", "check-ignore", "-q", "--no-index", path)
		cmd.Dir = repoRoot
		err := cmd.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Errorf("git ignores %s, or cannot say (%v): a fresh clone would not have it", path, err)
		}
	}
	names := slices.Sorted(maps.Keys(profiles(t)))
	tableShells(t, func(t *testing.T, r *rig, all bool) {
		for i, name := range names {
			if !all && i > 0 {
				break
			}
			p := profiles(t)[name]
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.chart()
				r.rules(renderRules()...)
				profileFiles(t, r, p.env)
				values := filepath.Join(r.dir, "values.yaml")
				res := r.k8s(p.env, fmt.Sprintf("k8s_values '%s' %d machine-1 '%s'; k8s_render '%s'", values, p.nodes, testImage, values), false)
				res.want(t, 0, "kind: Service")
			})
		}
	})
}

// TestK8sWorkDirQuote: a work directory whose path holds a single quote -
// which the trap that removes it holds between single quotes - is refused
// and removed, never used.
func TestK8sWorkDirQuote(t *testing.T) {
	tableShells(t, func(t *testing.T, r *rig, _ bool) {
		state := filepath.Join(r.dir, "it's-state")
		res := r.k8s(k8sReady+"STATE_DIR="+state+"\n", "k8s_secrets_ensure", false)
		res.want(t, 1, "holds a single quote")
		if len(writes(res.calls)) != 0 {
			t.Errorf("wrote: %v", res.calls)
		}
		noWorkDir(t, filepath.Join(state, "k8s-prod", "cactusfs"))
	})
}

// TestK8sNodeCertDirSurvives: the directory NODE_CERT_DIR names, and every
// certificate and key in it, are there unchanged after a run that creates
// the Secrets, one that is refused and one that stops as not built yet -
// the work directories of the script are removed and nothing else (the
// review of 10.9: a trap expanded late removed the directory a callee had
// put in the same variable).
func TestK8sNodeCertDirSurvives(t *testing.T) {
	ids := []string{"cactusfs-meta-0", "cactusfs-meta-1", "cactusfs-meta-2", "cactusfs-data-0"}
	cluster := "DEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\nNODE_CERT_DIR=~/certs\n"
	// The keys not read from NODE_CERT_DIR.
	var own []secretKey
	for _, k := range clusterKeys {
		if !strings.HasPrefix(k.file, "config/nodes/") {
			own = append(own, k)
		}
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range []struct {
			name    string
			prepare func(t *testing.T, r *rig, values map[string]string) []rule
			code    int
			want    string
		}{
			{"a run that creates", func(*testing.T, *rig, map[string]string) []rule { return nil }, 0, "are created"},
			{"a refused run", func(_ *testing.T, _ *rig, values map[string]string) []rule {
				theirs := maps.Clone(values)
				theirs["config/node-ca.pem"] = "fixture-" + rand.Text()
				return clusterHas(own, theirs)
			}, 2, "hold different values of ca.crt"},
			{"a run not built yet", func(t *testing.T, r *rig, _ map[string]string) []rule {
				if err := os.Remove(filepath.Join(r.state("k8s-prod", "cactusfs"), "secrets", "kms-passphrase")); err != nil {
					t.Fatal(err)
				}
				return nil
			}, 3, "not built yet (PLAN 10.3 for Kubernetes)"},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.trace = true
				certs := filepath.Join(r.home, "certs")
				for _, id := range ids {
					put(t, filepath.Join(certs, id+".crt"), "fixture-crt-"+rand.Text())
					put(t, filepath.Join(certs, id+".key"), "fixture-key-"+rand.Text())
				}
				before := snapshot(t, []string{certs})
				values := secretFixtures(t, r.state("k8s-prod", "cactusfs"), own)
				r.rules(append(c.prepare(t, r, values), rule{Tool: "kubectl", Contains: []string{"create", "--field-manager=cactusfs-deploy"}, SaveStdin: "created.yaml"})...)
				res := r.k8s(k8sReady+cluster, "k8s_secrets_ensure", false)
				res.want(t, c.code, c.want)
				after := snapshot(t, []string{certs})
				if len(after) != 9 || !maps.Equal(before, after) {
					t.Fatalf("NODE_CERT_DIR changed:\nbefore %v\nafter  %v", before, after)
				}
				noWorkDir(t, r.state("k8s-prod", "cactusfs"))
			})
		}
	})
}
