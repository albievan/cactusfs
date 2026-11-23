//go:build deployscript

package deploy_test

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
	defer r.givenGuard(env)()
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

// givenGuard snapshots the files and directories the env file names (its
// *_FILE and *_DIR settings, givenPaths) and returns the check to defer: the
// test fails when the run removed or changed any of them - or anything a
// symbolic link among them leads to (the deferred minor of the review of
// 10.9). Every run of the k8s tests goes through it, those through the
// script (withEnv) as well as those through r.k8s.
func (r *rig) givenGuard(env string) func() {
	r.t.Helper()
	given := r.givenPaths(env)
	before := snapshot(r.t, given)
	return func() {
		if after := snapshot(r.t, given); !maps.Equal(before, after) {
			r.t.Errorf("a file or directory the settings name was removed or changed:\nbefore %v\nafter  %v", before, after)
		}
	}
}

// snapshot is every file and directory under the paths, with a hash of each
// file; a path that is not there is recorded as missing. A symbolic link is
// followed - recorded by where it points and what it leads to (a file's
// hash, a directory's tree) - so that a run that changes what a link leads
// to is seen; one that leads nowhere is recorded as such.
func snapshot(t *testing.T, paths []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	seen := map[string]bool{}
	var walk func(p string)
	walk = func(p string) {
		if seen[p] {
			return
		}
		seen[p] = true
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			out[p] = "missing"
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				t.Fatal(err)
			}
			out[p] = "link to " + target
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(p), target)
			}
			walk(target)
			return
		}
		if !info.IsDir() {
			out[p] = hashFile(t, p)
			return
		}
		for rel, h := range tree(t, p) {
			full := filepath.Join(p, rel)
			out[full] = h
			if h == "link" {
				delete(out, full)
				walk(full)
			}
		}
		out[p] = "dir"
	}
	for _, p := range paths {
		walk(p)
	}
	return out
}

// hashFile is the SHA-256 of a file.
func hashFile(t *testing.T, path string) string {
	t.Helper()
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
		// The kubeconfig here, not the cluster: the API server of the context.
		case len(args) >= 2 && args[0] == "config" && args[1] == "view":
		// Discovery: the kinds the API offers (the purge's check that the
		// namespace is empty).
		case len(args) >= 1 && args[0] == "api-resources":
		// The pods of every namespace, read at a purge: whether one still
		// runs an image tag it would remove.
		case len(args) >= 3 && args[0] == "get" && (args[1] == "pods" || args[1] == "statefulsets,deployments,daemonsets,jobs,cronjobs") && all:
		// The namespace's delete, with its preconditions on standard input
		// (PLAN 10.11): the deployment's own namespace and no other.
		case len(args) == 5 && args[0] == "delete" && args[1] == "--raw" && args[2] == "/api/v1/namespaces/"+ns && args[3] == "-f" && args[4] == "-":
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
		// What is allowed gets as far as the cluster (PLAN 10.10): its
		// first question is the UID of kube-system, which the fakes do not
		// answer.
		put(t, filepath.Join(r.home, "token"), "placeholder")
		for i, env := range allowed {
			if !all && i > 0 {
				break
			}
			res := r.withEnv(kmsLine(env)+env, "--docker", "k3s", "--install")
			res.want(t, 1, "did not say the UID of its namespace kube-system")
			if len(res.calls) == 0 || !slices.Equal(res.calls[0].Args, []string{"kubectl", "get", "namespace", "kube-system", "-o", "json"}) {
				t.Errorf("the first question is not the cluster's identity: %v", res.calls)
			}
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
			want := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=20", "-o", "ControlPath=none", "--", "deploy@k3s.example.test", "k3s", "kubectl", "create", "-f", "-"}
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

// --- PLAN 10.10 and 10.11: install, image delivery, upgrade, status,
// uninstall and purge through deploy/deploy.sh ---------------------------------

// clusterUID is the UID of kube-system the fake cluster says: its identity.
const clusterUID = "6b2f0c1e-4d3a-4f5b-9c8d-1e2f3a4b5c6d"

// nsUID is the UID the fake cluster gives the deployment's namespace.
const nsUID = "0f0e0d0c-0b0a-4908-8706-050403020100"

// kubeMarker is the namespace marker of an installed deployment.
const kubeMarker = "0123456789abcdef0123456789abcdef"

// obj is an object of the Kubernetes API, as kubectl prints it.
type obj = map[string]any

// kjson is the object as "kubectl get -o json" prints it: indented by four
// spaces, a key or an element a line, the keys sorted.
func kjson(t *testing.T, v any) string {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

// kubeList is a list of objects, as kubectl prints one.
func kubeList(items ...any) obj {
	if items == nil {
		items = []any{}
	}
	return obj{"apiVersion": "v1", "kind": "List", "items": items}
}

// kubeNodes are the node IDs of a deployment named cactusfs.
func kubeNodes(mode string) []string {
	if mode == "cluster" {
		return []string{"cactusfs-meta-0", "cactusfs-meta-1", "cactusfs-meta-2", "cactusfs-data-0"}
	}
	return []string{"cactusfs-node-0"}
}

// kubeRender is what the fake helm renders for the mode: the chart's
// objects of a deployment named cactusfs, by kind and name.
func kubeRender(mode string) string {
	objects := []string{"Service/cactusfs-nodes", "Service/cactusfs", "Service/cactusfs-admin", "NetworkPolicy/cactusfs"}
	if mode == "cluster" {
		objects = append(objects, "PodDisruptionBudget/cactusfs")
	}
	for _, id := range kubeNodes(mode) {
		objects = append(objects, "StatefulSet/"+id)
	}
	var b strings.Builder
	for _, o := range objects {
		kind, name, _ := strings.Cut(o, "/")
		fmt.Fprintf(&b, "---\n# Source: cactusfs/templates/x.yaml\napiVersion: v1\nkind: %s\nmetadata:\n  name: %s\n  labels:\n    app.kubernetes.io/instance: cactusfs\n    app.kubernetes.io/version: \"0.7.0-412\"\nspec:\n  name: not-the-name\n", kind, name)
		if kind == "StatefulSet" {
			b.WriteString("  template:\n    metadata:\n      labels:\n        app.kubernetes.io/version: \"0.7.0-412\"\n")
		}
	}
	return b.String()
}

// kubeInstalled is what kubectl lists of an installed deployment's objects.
func kubeInstalled(mode string) []string {
	out := []string{"networkpolicy.networking.k8s.io/cactusfs", "service/cactusfs", "service/cactusfs-admin", "service/cactusfs-nodes"}
	if mode == "cluster" {
		out = append(out, "poddisruptionbudget.policy/cactusfs")
	}
	for _, id := range kubeNodes(mode) {
		out = append(out, "statefulset.apps/"+id)
	}
	return out
}

// k8sDevInit is what the fake dev-init writes for the nodes of the mode.
func k8sDevInit(mode string) map[string]string {
	out := map[string]string{}
	files := []string{"kms.json", "s3.pem", "s3.key", "s3-ca.pem", "node-ca.pem", "ready"}
	for _, id := range kubeNodes(mode) {
		files = append(files, "nodes/"+id+".pem", "nodes/"+id+".key")
	}
	for _, f := range files {
		out[f] = "fixture " + f + " " + rand.Text() + "\n"
	}
	return out
}

// lockOf is the value of the console upgrade's lock (ADR-0029) with the
// heartbeat given.
func lockOf(heartbeat string) string {
	return `{"run":"r1","target":"0.8.0","image":"registry.example.test/cactusfs@sha256:` + strings.Repeat("e", 64) +
		`","holder":"cactusfs-meta-1","startedBy":"admin","startedAt":"2026-11-22T09:00:00Z","heartbeat":"` + heartbeat + `"}`
}

// kube is a fake cluster of the k8s tests, reached by the context prod (or
// by "k3s kubectl" over ssh, tool "ssh"), with one machine, na, of this
// machine's architecture, and the engine here that holds the image given
// (IMAGE_SOURCE=image) and renders the chart.
type kube struct {
	tool string
	mode string
	f    imgFix
	// image is what the pods are to run; podImage what the pods there run
	// (none: there are no pods).
	image, podImage string
	// marker is the marker of the namespace there; none: no namespace.
	marker string
	// objects are the release's objects there (kubectl's names).
	objects []string
	// secrets are the values of the cluster's Secrets by the entry's file
	// they are compared with (none: the cluster has no Secret).
	secrets map[string]string
	// lock is the lock annotation of the first node's StatefulSet.
	lock string
	// never is a node whose new pod is never ready.
	never string
	// devInit is what dev-init writes.
	devInit map[string]string
	// imported: the image goes to k3s over ssh (save, import, crictl).
	imported bool
	// foreign are objects among objects that another field manager
	// applied (not cactusfs-deploy with the operation Apply).
	foreign []string
	// notReady are the nodes whose pod the listing of the release's pods
	// says is not Ready.
	notReady []string
	extra    []rule
}

// kindOf is the kind of an object kubectl names <resource>/<name>.
func kindOf(name string) string {
	switch {
	case strings.HasPrefix(name, "statefulset.apps/"):
		return "StatefulSet"
	case strings.HasPrefix(name, "service/"):
		return "Service"
	case strings.HasPrefix(name, "networkpolicy.networking.k8s.io/"):
		return "NetworkPolicy"
	case strings.HasPrefix(name, "poddisruptionbudget.policy/"):
		return "PodDisruptionBudget"
	}
	return "Unknown"
}

// objectsJSON is the listing of the release's objects with their field
// managers (--show-managed-fields -o json): cactusfs-deploy's Apply, or for
// those of foreign another manager's Update.
func objectsJSON(t *testing.T, objects, foreign []string) string {
	t.Helper()
	var items []any
	for _, o := range objects {
		manager := obj{"manager": "cactusfs-deploy", "operation": "Apply"}
		if slices.Contains(foreign, o) {
			manager = obj{"manager": "kubectl-client-side-apply", "operation": "Update"}
		}
		_, name, _ := strings.Cut(o, "/")
		items = append(items, obj{"kind": kindOf(o), "metadata": obj{"name": name, "managedFields": []any{obj{"manager": "kube-controller-manager", "operation": "Update"}, manager}}})
	}
	return kjson(t, kubeList(items...))
}

// podsJSON is the listing of the release's pods: one a node, Ready but
// those of notReady.
func podsJSON(t *testing.T, mode, image string, notReady ...string) string {
	t.Helper()
	var items []any
	for _, id := range kubeNodes(mode) {
		items = append(items, podOf(id+"-0", "uid-"+id, id+"-new", image, !slices.Contains(notReady, id), true))
	}
	return kjson(t, kubeList(items...))
}

// keys are the Secrets of the mode on the local KMS.
func (k kube) keys() []secretKey {
	if k.mode == "cluster" {
		return clusterKeys
	}
	return standaloneKeys
}

func (k kube) rules(t *testing.T) []rule {
	t.Helper()
	tool := k.tool
	if tool == "" {
		tool = "kubectl"
	}
	arch := runtime.GOARCH
	rs := slices.Clone(k.extra)
	rs = append(rs,
		rule{Tool: tool, Contains: []string{"get", "namespace", "kube-system"}, Stdout: kjson(t, obj{"apiVersion": "v1", "kind": "Namespace", "metadata": obj{"name": "kube-system", "uid": clusterUID}})},
		rule{Tool: "kubectl", Contains: []string{"config", "view"}, Stdout: "https://192.0.2.1:6443\n"},
		rule{Tool: tool, Contains: []string{"get", "nodes", "json"}, Stdout: kjson(t, kubeList(obj{
			"metadata": obj{"name": "na"},
			"status":   obj{"addresses": []any{obj{"type": "Hostname", "address": "na"}, obj{"type": "InternalIP", "address": "192.0.2.11"}}},
		}))},
		rule{Tool: tool, Contains: []string{"get", "storageclasses"}, Stdout: "{\n    \"metadata\": {\n        \"annotations\": {\n            \"storageclass.kubernetes.io/is-default-class\": \"true\"\n"},
		rule{Tool: tool, Contains: []string{"get", "statefulsets,services,networkpolicies,poddisruptionbudgets"}, Stdout: objectsJSON(t, k.objects, k.foreign)},
		rule{Tool: tool, Contains: []string{"get", "pods", "-l"}, Stdout: podsJSON(t, k.mode, k.image, k.notReady...)},
	)
	if k.marker != "" {
		rs = append(rs, rule{Tool: tool, Contains: []string{"get", "namespace", "cactusfs"}, Stdout: kjson(t, obj{
			"apiVersion": "v1", "kind": "Namespace",
			"metadata": obj{
				"name": "cactusfs", "uid": nsUID, "resourceVersion": "41",
				"annotations": obj{"deploy.cactusfs.io/owner-uid": k.marker},
				"labels":      obj{"io.cactusfs.deploy.name": "cactusfs", "kubernetes.io/metadata.name": "cactusfs"},
			},
		})})
	}
	for _, id := range kubeNodes(k.mode) {
		meta := obj{"name": id, "namespace": "cactusfs", "generation": 2}
		// The lock is on the first node by name, as the nodes choose it.
		if id == slices.Min(kubeNodes(k.mode)) && k.lock != "" {
			meta["annotations"] = obj{"cactusfs.io/upgrade": k.lock}
		}
		if len(k.objects) > 0 {
			rs = append(rs, rule{Tool: tool, Contains: []string{"get", "statefulset", id}, Stdout: kjson(t, obj{
				"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": meta,
				"spec": obj{"template": obj{
					"metadata": obj{"labels": obj{"app.kubernetes.io/version": "0.6.0-1"}},
					"spec":     obj{"containers": []any{obj{"name": "cactusfs", "image": k.podImage}}},
				}},
				"status": obj{"observedGeneration": 2, "updateRevision": id + "-new"},
			})})
		}
		pod := id + "-0"
		gateway := k.mode == "standalone" || id == "cactusfs-data-0" || strings.Contains(id, "-meta-")
		fresh := podOf(pod, "uid-new-"+id, id+"-new", k.image, id != k.never, gateway)
		rs = append(rs, rule{Tool: tool, Contains: []string{"get", "pod", pod}, After: []string{"delete", "pod", pod}, Stdout: kjson(t, fresh)})
		if k.podImage != "" {
			rev := id + "-old"
			if k.podImage == k.image {
				rev = id + "-new"
			}
			rs = append(rs, rule{Tool: tool, Contains: []string{"get", "pod", pod}, Stdout: kjson(t, podOf(pod, "uid-old-"+id, rev, k.podImage, true, gateway))})
		} else {
			rs = append(rs, rule{Tool: tool, Contains: []string{"get", "pod", pod}, Stdout: kjson(t, fresh)})
		}
	}
	if k.secrets != nil {
		for _, r := range clusterHas(k.keys(), k.secrets) {
			r.Tool = tool
			rs = append(rs, r)
		}
	}
	rs = append(rs,
		rule{Tool: tool, Contains: []string{"create", "--field-manager=cactusfs-deploy"}, SaveStdin: "created.yaml"},
		rule{Tool: tool, Contains: []string{"create", "-f", "-"}, SaveStdin: "namespace.yaml"},
		rule{Tool: tool, Contains: []string{"apply", "--field-manager=cactusfs-deploy"}, SaveStdin: "applied.yaml"},
	)
	rs = append(rs, nodeRules(tool, arch)...)
	if k.imported {
		rs = append(rs,
			rule{Tool: "docker", Contains: []string{"inspect", k.f.ref()}, Stdout: k.f.fields(arch)},
			rule{Tool: "docker", Contains: []string{"save"}, Stdout: k.f.tar},
			crictlOf(tool, k.f.hex))
	}
	given := registry + "@sha256:" + k.f.hex
	rs = append(rs, givenHere("docker", k.f, given, given)...)
	rs = append(rs,
		rule{Tool: "docker", Contains: []string{"image", "inspect", dockerfileHelm(t)}, Stdout: "[{}]\n"},
		rule{Tool: "docker", Contains: []string{"--tmpfs", "/work"}, Stdout: kubeRender(k.mode), SaveStdin: "render.tar"},
	)
	if k.devInit != nil {
		rs = append(rs, rule{Tool: "docker", Contains: []string{"cp", "cactusfs-dev-init:/tmp/out", "-"}, Stdout: tarOf(t, "out", k.devInit)})
	}
	return rs
}

// podOf is a pod of a node, as kubectl prints it.
func podOf(name, uid, rev, image string, ready, gateway bool) obj {
	labels := obj{"app.kubernetes.io/instance": "cactusfs", "app.kubernetes.io/name": "cactusfs", "app.kubernetes.io/version": "0.7.0-412", "controller-revision-hash": rev}
	if gateway {
		labels["cactusfs.io/gateway"] = "true"
	}
	status := "False"
	if ready {
		status = "True"
	}
	return obj{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": obj{"name": name, "namespace": "cactusfs", "uid": uid, "labels": labels},
		"spec":     obj{"nodeName": "na", "containers": []any{obj{"name": "cactusfs", "image": image}}},
		"status":   obj{"conditions": []any{obj{"type": "Initialized", "status": "True"}, obj{"type": "Ready", "status": status}}},
	}
}

// kubeEnv is the env file of a deployment on the context prod whose image
// is given by its digest in K8S_REGISTRY: the nodes pull it (PLAN 10.4's
// route "nodes").
func kubeEnv(f imgFix) string {
	return "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nKMS=local\nK8S_S3_ALLOW_FROM=192.0.2.0/24\nIMAGE_SOURCE=image\nK8S_REGISTRY=" + registry +
		"\nIMAGE=" + registry + "@sha256:" + f.hex + "\nWAIT_TIMEOUT=3\n"
}

// kubeRun runs the script with the env file, as withEnv does, with a
// TMPDIR of its own that must be empty afterwards: every work directory is
// removed, however the run ends (the run's one EXIT trap).
func (r *rig) kubeRun(env string, args ...string) result {
	r.t.Helper()
	path := r.envFile("site.env", env)
	defer r.givenGuard(env)()
	tmp := r.t.TempDir()
	res := r.run(append([]string{"--env", path}, args...), "TMPDIR="+tmp)
	if left, err := os.ReadDir(tmp); err != nil || len(left) != 0 {
		r.t.Errorf("the run (exit %d) left %v in TMPDIR (%v)", res.code, left, err)
	}
	if entries, err := os.ReadDir(r.state("k8s-prod", "cactusfs")); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				r.t.Errorf("the run (exit %d) left %s in the entry", res.code, e.Name())
			}
		}
	}
	return res
}

// kubeEntry writes the STATE_DIR entry of an installed deployment of the
// mode on the context prod: its record (the cluster's ID), its namespace
// marker, and a value made for the run for each of its secrets, which it
// returns by file.
func kubeEntry(t *testing.T, r *rig, mode, engineID string) map[string]string {
	t.Helper()
	state := r.state("k8s-prod", "cactusfs")
	keys := standaloneKeys
	if mode == "cluster" {
		keys = clusterKeys
	}
	values := secretFixtures(t, state, keys)
	put(t, filepath.Join(state, "config", "ready"), "2026-11-22T10:00:00Z\n")
	put(t, filepath.Join(state, "config", "s3-ca.pem"), "a CA\n")
	put(t, filepath.Join(state, "namespace-uid"), kubeMarker+"\n")
	put(t, filepath.Join(state, "namespace"), "cactusfs\n")
	record := "kubectl --context prod\nengine-id=" + engineID + "\nkms=local\n"
	if mode == "cluster" {
		record += "mode=cluster\n"
	}
	put(t, filepath.Join(state, "target"), record)
	return values
}

// kubeIndex is the index of the first call of the tool whose kubectl words
// (kubectlArgs) begin with the words given, at or after from; -1 when none.
func kubeIndex(calls []call, from int, words ...string) int {
	for i := from; i < len(calls); i++ {
		if args, ok := kubectlArgs(calls[i]); ok && len(args) >= len(words) && slices.Equal(args[:len(words)], words) {
			return i
		}
	}
	return -1
}

// kubeDeletes is the pods the run deleted, in order.
func kubeDeletes(calls []call) []string {
	var out []string
	for _, c := range calls {
		if args, ok := kubectlArgs(c); ok && len(args) > 2 && args[0] == "delete" && args[1] == "pod" {
			out = append(out, args[2])
		}
	}
	return out
}

// noForce fails the test when any apply forced its way - but, with
// takeImage, the one apply of the render by cactusfs-deploy.
func noForce(t *testing.T, calls []call, takeImage ...bool) {
	t.Helper()
	forced := 0
	for _, c := range calls {
		args, ok := kubectlArgs(c)
		if !ok || !slices.Contains(args, "--force-conflicts") {
			continue
		}
		forced++
		if len(takeImage) == 0 || !takeImage[0] || strings.Join(args, " ") != forceApply || forced > 1 {
			t.Errorf("an apply was forced: %q", args)
		}
	}
}

// entryFiles is the secrets of the entry by file: every file of
// secrets/ and config/.
func entryFiles(t *testing.T, state string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, unit := range []string{"secrets", "config"} {
		for f, v := range filesUnder(t, filepath.Join(state, unit)) {
			out[unit+"/"+f] = v
		}
	}
	return out
}

// TestK8sInstall: a first install on Kubernetes asks everything first and
// changes nothing until it has (the cluster's identity, the nodes, the
// storage, the namespace, the image), then makes the namespace, makes the
// secrets once - the root credentials and the passphrase from /dev/urandom,
// kms.json and the certificates by dev-init on this machine - into
// STATE_DIR and the namespace's Secrets (create, never apply), records the
// cluster's identity (the UID of kube-system), applies the render
// server-side as cactusfs-deploy, never forced, and waits for the pod. A
// second install makes nothing, creates nothing and deletes no pod that
// runs the image already. No secret is on a command line, in an
// environment, in the output or in a trace (PLAN 10.3's assertions, kept on
// Kubernetes).
func TestK8sInstall(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		for _, mode := range []string{"standalone", "cluster"} {
			t.Run(mode, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.chart()
				r.trace = true
				f := newImgFix(imgCommit)
				image := registry + "@sha256:" + f.hex
				env := kubeEnv(f)
				if mode == "cluster" {
					env += "DEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\n"
				}
				k := kube{mode: mode, f: f, image: image, devInit: k8sDevInit(mode)}
				r.rules(k.rules(t)...)
				res := r.kubeRun(env, "--install")
				last := kubeNodes(mode)[len(kubeNodes(mode))-1]
				res.want(t, 0, "ready: "+last+" runs "+image, "cluster ID (the UID of its namespace kube-system): "+clusterUID,
					"API server of the context prod: https://192.0.2.1:6443", "imagePullPolicy IfNotPresent", "the console user admin with the password admin (ADR-0014)")
				insideNamespace(t, r, res.calls, "cactusfs")
				noForce(t, res.calls)

				// Every question before the first change.
				first := kubeIndex(res.calls, 0, "create", "-f", "-")
				if first < 0 {
					t.Fatal("the namespace was not created")
				}
				for _, c := range res.calls[:first] {
					args, isKube := kubectlArgs(c)
					if isKube && args[0] != "get" && args[0] != "config" {
						t.Errorf("%q before the namespace was made", args)
					}
					if c.Tool == "docker" && (slices.Contains(c.Args, "create") || slices.Contains(c.Args, "cp") || slices.Contains(c.Args, "--tmpfs")) {
						t.Errorf("%q before the namespace was made", c.Args)
					}
				}
				var order []string
				for _, w := range writes(res.calls) {
					args, _ := kubectlArgs(w)
					order = append(order, strings.Join(args, " "))
				}
				want := []string{"create -f -", "create --field-manager=cactusfs-deploy -n cactusfs -f -", "apply --server-side --field-manager=cactusfs-deploy -n cactusfs -f -"}
				if !slices.Equal(order, want) {
					t.Fatalf("the changes are\n%q\nwant\n%q", order, want)
				}
				if read(t, filepath.Join(r.dir, "applied.yaml")) != kubeRender(mode) {
					t.Error("what was applied is not the render")
				}
				files := tarFiles(t, []byte(read(t, filepath.Join(r.dir, "render.tar"))))
				if !strings.Contains(files["values.yaml"], "  digest: 'sha256:"+f.hex+"'\n  pullPolicy: IfNotPresent\n") {
					t.Errorf("the values do not name the image by its digest for the nodes to pull:\n%s", files["values.yaml"])
				}
				// The record: the settings' cluster, its ID and the KMS.
				state := r.state("k8s-prod", "cactusfs")
				record := "kubectl --context prod\nengine-id=" + clusterUID + "\nkms=local\n"
				if mode == "cluster" {
					// The mode is recorded too (state_mode_check, PLAN 10.8).
					record += "mode=cluster\n"
				}
				if got := read(t, filepath.Join(state, "target")); got != record {
					t.Errorf("the record is %q", got)
				}
				made := entryFiles(t, state)
				for _, k := range k.keys() {
					if made[k.file] == "" {
						t.Errorf("%s was not made", k.file)
					}
				}
				checkCreated(t, r, k.keys(), made)
				for f := range k.devInit {
					if _, ok := made["config/"+f]; !ok {
						t.Errorf("dev-init's %s is not in the entry", f)
					}
				}
				if got := made["secrets/root-access-key"]; !regexp.MustCompile(`^[A-Z2-7]{20}$`).MatchString(got) {
					t.Errorf("the access key is %d characters of another alphabet", len(got))
				}
				noLeak(t, res, made)
				for _, c := range res.calls {
					for name, value := range c.Env {
						for file, secret := range made {
							if strings.Contains(value, secret) {
								t.Errorf("the secret of %s is in the environment of %s (%s)", file, c.Tool, name)
							}
						}
					}
				}
				var devInit []call
				for _, c := range res.calls {
					if c.Tool == "docker" && slices.Contains(c.Args, "--name") && slices.Contains(c.Args, "cactusfs-dev-init") {
						devInit = append(devInit, c)
					}
				}
				if len(devInit) != 1 || !containsRun(devInit[0].Args, []string{"--nodes", strings.Join(kubeNodes(mode), ",")}) ||
					!containsRun(devInit[0].Args, []string{"--network", "none"}) || !containsRun(devInit[0].Args, []string{"--user", "65532:65532"}) {
					t.Errorf("dev-init is not run once, for the chart's nodes, with no network, as 65532: %v", devInit)
				}

				// A second install: the cluster has the namespace, the
				// Secrets and the pods on this image.
				k.marker = strings.TrimSpace(read(t, filepath.Join(state, "namespace-uid")))
				k.objects = kubeInstalled(mode)
				k.secrets = made
				k.podImage = image
				k.devInit = nil
				r.rules(k.rules(t)...)
				again := r.kubeRun(env, "--install")
				again.want(t, 0, "runs "+image+" already, and is ready")
				order = nil
				for _, w := range writes(again.calls) {
					args, _ := kubectlArgs(w)
					order = append(order, args[0])
				}
				if !slices.Equal(order, []string{"apply"}) {
					t.Errorf("a second install changed %q, want the apply alone", order)
				}
				if !maps.Equal(entryFiles(t, state), made) {
					t.Error("a second install changed the secrets of the entry")
				}
				for _, c := range again.calls {
					if c.Tool == "docker" && slices.Contains(c.Args, "cactusfs-dev-init") {
						t.Errorf("dev-init ran again: %q", c.Args)
					}
				}
				noLeak(t, again, made)
			})
		}
	})
}

// TestK8sInstallValues: the values of an install carry the image as the
// route delivered it - by its digest for the nodes to pull, imported into
// k3s over K3S_SSH by a tag of its ID with imagePullPolicy Never, the tag
// recorded in the entry so that a purge removes it and no other.
func TestK8sInstallValues(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range []struct {
			name, env, tool, pull, target string
			imported, known, words        bool
		}{
			{name: "pulled by the nodes", tool: "kubectl", pull: "IfNotPresent", target: "k8s-prod"},
			{
				name: "imported over ssh", tool: "ssh", pull: "Never", target: "ssh-admin_k3s.example.test", imported: true,
				env: "DEPLOY_TARGET=k3s\nK3S_SSH=admin@k3s.example.test\nKMS=local\nK8S_S3_ALLOW_FROM=192.0.2.0/24\nIMAGE_SOURCE=image\nWAIT_TIMEOUT=3\n",
			},
			{
				// The bench's route (test/k8s): a context, and K3S_IMPORT's
				// words, whose node the script cannot ask whether it held
				// the tag before - so it records none, says so, and the
				// purge leaves it.
				name: "imported by K3S_IMPORT's words", tool: "kubectl", pull: "Never", target: "k3s-prod", imported: true, words: true,
				env: "DEPLOY_TARGET=k3s\nK8S_CONTEXT=prod\nK3S_IMPORT=docker exec -i k3s-bench ctr -n k8s.io images import -\nALLOW_DIRTY=yes\nKMS=local\nK8S_S3_ALLOW_FROM=192.0.2.0/24\nIMAGE_SOURCE=image\nWAIT_TIMEOUT=3\n",
			},
			{
				name: "imported over ssh, the tag there before", tool: "ssh", pull: "Never", target: "ssh-admin_k3s.example.test", imported: true, known: true,
				env: "DEPLOY_TARGET=k3s\nK3S_SSH=admin@k3s.example.test\nKMS=local\nK8S_S3_ALLOW_FROM=192.0.2.0/24\nIMAGE_SOURCE=image\nWAIT_TIMEOUT=3\n",
			},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.chart()
				f := newImgFix(imgCommit)
				image := registry + "@sha256:" + f.hex
				podImage := image
				if c.imported {
					podImage = f.ref()
				}
				k := kube{tool: c.tool, mode: "standalone", f: f, image: podImage, devInit: k8sDevInit("standalone"), imported: c.imported}
				if c.known {
					k.extra = []rule{{Tool: "ssh", Contains: []string{"images", "ls", "-q", "name==docker.io/library/" + f.ref()}, Stdout: "docker.io/library/" + f.ref() + "\n"}}
				}
				r.rules(k.rules(t)...)
				env := kubeEnv(f)
				if c.env != "" {
					env = c.env + "IMAGE=" + image + "\n"
				}
				path := r.envFile("site.env", env)
				res := r.run([]string{"--env", path, "--install"})
				res.want(t, 0, "imagePullPolicy "+c.pull, "ready: cactusfs-node-0 runs "+podImage)
				insideNamespace(t, r, res.calls, "cactusfs")
				files := tarFiles(t, []byte(read(t, filepath.Join(r.dir, "render.tar"))))
				if !strings.Contains(files["values.yaml"], "  pullPolicy: "+c.pull+"\n") {
					t.Errorf("the values do not say pullPolicy %s:\n%s", c.pull, files["values.yaml"])
				}
				images := filepath.Join(r.state(c.target, "cactusfs"), "images")
				if c.words {
					res.want(t, 0, "image: docker.io/library/"+f.ref()+" was imported by K3S_IMPORT's command, on a node the script cannot ask: it is not recorded, and a purge leaves it")
					if _, err := os.Stat(images); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("a tag K3S_IMPORT imported was recorded: %v", err)
					}
					for _, call := range res.calls {
						if containsRun(call.Args, []string{"images", "ls"}) {
							t.Errorf("containerd was asked through %s %q, which K3S_IMPORT does not reach", call.Tool, call.Args)
						}
					}
					imp := callsOf(res, "docker", "exec", "-i", "k3s-bench", "ctr", "-n", "k8s.io", "images", "import", "-")
					if len(imp) != 1 || imp[0].Stdin != f.tarSum() {
						t.Errorf("the image was not imported once by K3S_IMPORT's words: %v", imp)
					}
				} else if c.imported {
					// The question comes before the import.
					ls, imp := -1, -1
					for i, call := range res.calls {
						if containsRun(call.Args, []string{"k3s", "ctr", "-n", "k8s.io", "images", "ls", "-q", "name==docker.io/library/" + f.ref()}) && ls < 0 {
							ls = i
						}
						if containsRun(call.Args, []string{"images", "import", "-"}) && imp < 0 {
							imp = i
						}
					}
					if ls < 0 || imp < 0 || ls > imp {
						t.Errorf("containerd was asked for the tag at %d, the image imported at %d", ls, imp)
					}
				}
				if c.known {
					res.want(t, 0, "was in the containerd of k3s before the import")
					if _, err := os.Stat(images); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("a tag that was there before was recorded: %v", err)
					}
				} else if c.imported && !c.words {
					if got := read(t, images); got != "docker.io/library/"+f.ref()+"\n" {
						t.Errorf("the entry records the images %q", got)
					}
					if !strings.Contains(files["values.yaml"], "  repository: 'cactusfs'\n  tag: '0.7.0-412-"+f.id12()+"'\n  digest: ''\n") {
						t.Errorf("the values do not name the tag imported:\n%s", files["values.yaml"])
					}
				} else if !c.imported {
					if _, err := os.Stat(images); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("an image pulled by the nodes was recorded as imported: %v", err)
					}
					if !strings.Contains(files["values.yaml"], "  tag: ''\n  digest: 'sha256:"+f.hex+"'\n") {
						t.Errorf("the values do not name the digest:\n%s", files["values.yaml"])
					}
				}
			})
		}
	})
}

// TestK8sInstallRefused: what an install refuses, it refuses before any
// change - nothing written to the cluster, no secret made, no dev-init run,
// no image built or pulled - with exit 2.
func TestK8sInstallRefused(t *testing.T) {
	fresh := func(ts time.Time) string { return lockOf(ts.UTC().Format(time.RFC3339Nano)) }
	cases := []struct {
		name  string
		env   string
		entry string // the engine ID of an entry of an installed deployment, or none
		k     func(k kube) kube
		want  string
		tty   bool
		input string
		mode  string
	}{
		{name: "another cluster", entry: "another-uid-0000", k: installedKube, want: "now reach another, '" + clusterUID + "'"},
		{name: "a console upgrade on the first node by name, cluster mode", mode: "cluster", entry: clusterUID, k: func(k kube) kube {
			k = installedKube(k)
			k.lock = fresh(time.Now())
			return k
		}, want: "holds the lock cactusfs.io/upgrade on the StatefulSet cactusfs-data-0"},
		{
			name: "another namespace than the one recorded", entry: clusterUID, env: "K8S_NAMESPACE=elsewhere\n", k: installedKube,
			want: "records the namespace cactusfs, and the settings name elsewhere",
		},
		{name: "a node port another Service holds", env: "K8S_NODE_PORT=30900\n", k: func(k kube) kube {
			k.extra = append(k.extra, rule{Tool: "kubectl", Contains: []string{"get", "services", "-A"}, Stdout: "{\n    \"items\": [\n        {\n            \"metadata\": {\n                \"name\": \"web\",\n                \"namespace\": \"shop\"\n            },\n            \"spec\": {\n                \"ports\": [\n                    {\n                        \"nodePort\": 30900,\n                        \"port\": 80\n                    }\n                ]\n            }\n        }\n    ]\n}\n"})
			return k
		}, want: "the NodePort 30900 is held by the Service shop/web"},
		{name: "a console upgrade under way", entry: clusterUID, k: func(k kube) kube {
			k = installedKube(k)
			k.lock = fresh(time.Now().Add(-time.Minute))
			return k
		}, want: "an upgrade from the console (ADR-0029) holds the lock cactusfs.io/upgrade on the StatefulSet cactusfs-node-0"},
		{name: "a console upgrade's heartbeat with an offset", entry: clusterUID, k: func(k kube) kube {
			k = installedKube(k)
			k.lock = lockOf(time.Now().In(time.FixedZone("x", 2*3600)).Format(time.RFC3339))
			return k
		}, want: "holds the lock cactusfs.io/upgrade"},
		{name: "a lock that cannot be read", entry: clusterUID, k: func(k kube) kube {
			k = installedKube(k)
			k.lock = `{"run":"r1"}`
			return k
		}, want: "its heartbeat cannot be read"},
		{name: "a node the settings have not", entry: clusterUID, k: func(k kube) kube {
			k = installedKube(k)
			k.objects = append(k.objects, "statefulset.apps/cactusfs-meta-0")
			return k
		}, want: "the StatefulSet cactusfs-meta-0 of cactusfs in the namespace cactusfs is not a node of DEPLOY_MODE=standalone"},
		{name: "take-image on nothing installed", env: "", k: nil, want: "--take-image: cactusfs is not installed"},
		{name: "take-image with a stale lock", entry: clusterUID, k: func(k kube) kube {
			k = installedKube(k)
			k.lock = fresh(time.Now().Add(-2 * time.Hour))
			return k
		}, want: "--take-image is refused while the StatefulSet cactusfs-node-0 holds the console upgrade's lock"},
		{name: "take-image not confirmed", entry: clusterUID, k: installedKube, want: "type the deployment's name when asked at a terminal, or say --take-image=cactusfs"},
		{name: "take-image, another name typed", entry: clusterUID, k: installedKube, tty: true, input: "other\n", want: "that is not the name of the deployment"},
		{name: "a given root secret that differs", entry: clusterUID, env: "ROOT_SECRET_KEY_FILE=~/rsk\n", k: installedKube, want: "ROOT_SECRET_KEY_FILE"},
		{name: "a Secret that differs", entry: clusterUID, k: func(k kube) kube {
			k = installedKube(k)
			k.secrets = maps.Clone(k.secrets)
			k.secrets["config/kms.json"] = "fixture-other-" + rand.Text()
			return k
		}, want: "hold different values of kms.json"},
		{name: "half a root pair", entry: clusterUID, k: func(k kube) kube {
			k = installedKube(k)
			k.secrets = nil
			return k
		}, want: "one of the root credentials is in"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.chart()
				r.trace = true
				r.input = c.input
				f := newImgFix(imgCommit)
				image := registry + "@sha256:" + f.hex
				mode := c.mode
				if mode == "" {
					mode = "standalone"
				}
				k := kube{mode: mode, f: f, image: image, devInit: k8sDevInit(mode)}
				var values map[string]string
				if c.entry != "" {
					values = kubeEntry(t, r, mode, c.entry)
					k.secrets = values
				}
				if c.k != nil {
					k = c.k(k)
				}
				if c.name == "half a root pair" {
					remove(t, filepath.Join(r.state("k8s-prod", "cactusfs"), "secrets", "root-secret-key"))
				}
				if c.name == "a given root secret that differs" {
					put(t, filepath.Join(r.home, "rsk"), "fixture-given-"+rand.Text())
				}
				r.rules(k.rules(t)...)
				args := []string{"--install"}
				if strings.HasPrefix(c.name, "take-image") {
					args = append(args, "--take-image")
				}
				state := r.state("k8s-prod", "cactusfs")
				before := map[string]string{}
				if c.entry != "" {
					before = entryFiles(t, state)
				}
				env := kubeEnv(f) + c.env
				if mode == "cluster" {
					env += "DEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\n"
				}
				var res result
				if c.tty {
					path := r.envFile("site.env", env)
					res = r.runTTY([]string{"--env", path, "--install", "--take-image"})
				} else {
					res = r.kubeRun(env, args...)
				}
				res.want(t, 2, c.want)
				if w := writes(res.calls); len(w) != 0 {
					t.Errorf("a refused install changed the cluster: %v", w)
				}
				for _, call := range res.calls {
					if call.Tool == "docker" && (slices.Contains(call.Args, "cactusfs-dev-init") || slices.Contains(call.Args, "pull") || slices.Contains(call.Args, "--tmpfs")) {
						t.Errorf("a refused install ran %q", call.Args)
					}
				}
				if c.entry != "" && !maps.Equal(entryFiles(t, state), before) {
					t.Error("a refused install changed the secrets of the entry")
				}
				if c.entry == "" {
					if _, err := os.Stat(filepath.Join(state, "secrets")); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("a refused install made secrets: %v", err)
					}
				}
				if values != nil {
					noLeak(t, res, values)
				}
			})
		}
	})
}

// The applies of --take-image, by their words.
const (
	dryApply   = "apply --server-side --dry-run=server --field-manager=cactusfs-deploy -n cactusfs -f -"
	forceApply = "apply --server-side --force-conflicts --field-manager=cactusfs-deploy -n cactusfs -f -"
	mainApply  = "apply --server-side --field-manager=cactusfs-deploy -n cactusfs -f -"
)

// takeFields are the fields an upgrade from the console patches.
var takeFields = []string{".spec.template.spec.containers[name=\"cactusfs\"].image", ".spec.template.metadata.labels.app.kubernetes.io/version"}

// kubectlAdvice is what kubectl adds to a conflict of a server-side apply
// (kubectl v0.37.0, pkg/cmd/apply/apply.go).
const kubectlAdvice = `Please review the fields above--they currently have other managers. Here
are the ways you can resolve this warning:
* If you intend to manage all of these fields, please re-run the apply
  command with the ` + "`--force-conflicts`" + ` flag.
* If you do not intend to manage all of the fields, please edit your
  manifest to remove references to the fields that should keep their
  current managers.
* You may co-own fields by updating your manifest to match the existing
  value; in this case, you'll become the manager if the other manager(s)
  stop managing the field (remove it from their configuration).
See https://kubernetes.io/docs/reference/using-api/server-side-apply/#conflicts
`

// conflictOf is one object's conflict as kubectl prints it: the message of
// apimachinery's getConflictMessage (one conflict, or several under the
// manager's line) and kubectl's advice.
func conflictOf(fields ...string) string {
	if len(fields) == 1 {
		return "Apply failed with 1 conflict: conflict with \"cactusfs-upgrade\" using apps/v1: " + fields[0] + "\n" + kubectlAdvice
	}
	msg := fmt.Sprintf("Apply failed with %d conflicts: conflicts with \"cactusfs-upgrade\" using apps/v1:\n", len(fields))
	for _, f := range fields {
		msg += "- " + f + "\n"
	}
	return msg + kubectlAdvice
}

// dryConflict is the answer of the server's dry run of the apply when two
// StatefulSets conflict with the console's field manager on the fields
// given: two errors, which kubectl prints one after the other without the
// "error: " it gives a single one (cmd/util/helpers.go MultipleErrors).
func dryConflict(fields ...string) rule {
	return dryAnswer(conflictOf(fields...) + "\n" + conflictOf(fields...) + "\n")
}

// dryAnswer is the dry run's answer with the standard error given.
func dryAnswer(stderr string) rule {
	return rule{Tool: "kubectl", Contains: []string{"apply", "--dry-run=server"}, Stderr: stderr, Exit: 1}
}

// checkTake fails the test unless the applies of the run are those given.
func checkTake(t *testing.T, res result, want []string) {
	t.Helper()
	var applies []string
	for _, c := range res.calls {
		if args, ok := kubectlArgs(c); ok && args[0] == "apply" {
			applies = append(applies, strings.Join(args, " "))
		}
	}
	if !slices.Equal(applies, want) {
		t.Errorf("the applies are\n%q\nwant\n%q", applies, want)
	}
}

// podsJSONNoT is podsJSON for a value that always encodes.
func podsJSONNoT(mode, image string, notReady ...string) string {
	var items []any
	for _, id := range kubeNodes(mode) {
		items = append(items, podOf(id+"-0", "uid-"+id, id+"-new", image, !slices.Contains(notReady, id), true))
	}
	return kjsonNoT(kubeList(items...))
}

// installedKube is a cluster on which the deployment is installed: its
// namespace with the marker of kubeEntry, its objects, its Secrets (the
// values the entry holds) and its pods on an older image.
func installedKube(k kube) kube {
	k.marker = kubeMarker
	k.objects = kubeInstalled(k.mode)
	k.podImage = registry + "@sha256:" + strings.Repeat("0", 64)
	k.devInit = nil
	return k
}

// runTTY runs the script as run does, as if at a terminal: k8s_tty, the
// script's one test of a terminal, says yes. Standard input is r.input.
func (r *rig) runTTY(args []string) result {
	r.t.Helper()
	// deploy.sh is copied with k8s_tty answering yes.
	lib := filepath.Join(r.repo, "deploy", "lib", "k8s.sh")
	data := strings.Replace(read(r.t, lib), "k8s_tty() {\n\t[ -t 0 ]\n}", "k8s_tty() {\n\treturn 0\n}", 1)
	if err := os.WriteFile(lib, []byte(data), 0o600); err != nil {
		r.t.Fatal(err)
	}
	return r.runIn(args)
}

// runIn runs the script with r.input on its standard input.
func (r *rig) runIn(args []string) result {
	r.t.Helper()
	if err := os.Remove(r.transcript); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.t.Fatal(err)
	}
	argv := slices.Clone(r.sh.argv[1:])
	if r.trace {
		argv = append(argv, "-x")
	}
	argv = append(argv, filepath.Join(r.repo, "deploy", "deploy.sh"))
	argv = append(argv, args...)
	cmd := exec.CommandContext(r.t.Context(), r.sh.argv[0], argv...)
	cmd.Dir = r.work
	cmd.Env = []string{"PATH=" + r.bin, "HOME=" + r.home}
	cmd.Stdin = strings.NewReader(r.input)
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
		r.t.Fatalf("run the script: %v", err)
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	res.calls = r.calls()
	return res
}

// TestK8sUpgrade: an install over an installed deployment applies the
// render and then deletes the pods one at a time - the nodes that do not
// vote, then the voters - each Ready on the new revision and image before
// the next, reading the console upgrade's lock before each; it stops at
// the first that is not ready, at a lock that appears, and at a conflict of
// the apply (a console upgrade moved the image), which it never forces;
// --take-image, after a dry run on the server shows conflicts on the image
// and the version label alone (read by allowlist, line by line, as kubectl
// writes them), applies the render once with --force-conflicts by
// cactusfs-deploy, and refuses anything else; what the render no longer
// holds is pruned.
func TestK8sUpgrade(t *testing.T) {
	conflict := rule{
		Tool: "kubectl", Contains: []string{"apply", "--field-manager=cactusfs-deploy"},
		Stderr: "error: Apply failed with 1 conflict: conflict with \"cactusfs-upgrade\" using apps/v1: .spec.template.spec.containers[name=\"cactusfs\"].image\n", Exit: 1,
	}
	cases := []struct {
		name    string
		mode    string
		env     string
		args    []string
		k       func(k kube) kube
		code    int
		want    []string
		deletes []string
		check   func(t *testing.T, r *rig, res result)
	}{
		{
			name: "standalone", mode: "standalone",
			deletes: []string{"cactusfs-node-0-0"},
			want:    []string{"upgrade: cactusfs-node-0 is Ready on "},
		},
		{
			name: "cluster: the node that does not vote first, then the voters", mode: "cluster",
			deletes: []string{"cactusfs-data-0-0", "cactusfs-meta-0-0", "cactusfs-meta-1-0", "cactusfs-meta-2-0"},
			want:    []string{"upgrade: cactusfs-meta-2 is Ready on "},
			check: func(t *testing.T, _ *rig, res result) {
				// Before each node the lock is read, and each pod is
				// asked for after its deletion and before the next.
				at := kubeIndex(res.calls, 0, "apply")
				for _, id := range []string{"cactusfs-data-0", "cactusfs-meta-0", "cactusfs-meta-1", "cactusfs-meta-2"} {
					// The lock is on the first node by name, cactusfs-data-0.
					lock := kubeIndex(res.calls, at, "get", "statefulset", "cactusfs-data-0")
					del := kubeIndex(res.calls, at, "delete", "pod", id+"-0")
					got := kubeIndex(res.calls, del, "get", "pod", id+"-0")
					if lock < 0 || del < 0 || got < 0 || lock > del {
						t.Fatalf("%s: the lock read at %d, the delete at %d, the pod read at %d", id, lock, del, got)
					}
					at = got
				}
			},
		},
		{
			name: "a node that is not ready stops it", mode: "cluster",
			k:       func(k kube) kube { k.never = "cactusfs-meta-0"; return k },
			code:    1,
			deletes: []string{"cactusfs-data-0-0", "cactusfs-meta-0-0"},
			want:    []string{"upgrade stopped: the node cactusfs-meta-0 is not Ready on ", "the nodes after it were not moved", "IMAGE_SOURCE=image IMAGE=" + registry + "@sha256:" + strings.Repeat("0", 64)},
		},
		{
			name: "a console upgrade that starts during it stops it", mode: "cluster",
			k: func(k kube) kube {
				// The lock appears on cactusfs-data-0, the first node by name,
				// once its pod was replaced.
				k.extra = append(k.extra, rule{
					Tool: "kubectl", Contains: []string{"get", "statefulset", "cactusfs-data-0"}, After: []string{"delete", "pod", "cactusfs-data-0-0"},
					Stdout: kjsonNoT(obj{
						"apiVersion": "apps/v1", "kind": "StatefulSet",
						"metadata": obj{"name": "cactusfs-data-0", "generation": 2, "annotations": obj{"cactusfs.io/upgrade": lockOf(time.Now().UTC().Format(time.RFC3339Nano))}},
						"status":   obj{"observedGeneration": 2, "updateRevision": "cactusfs-data-0-new"},
					}),
				})
				return k
			},
			code:    1,
			deletes: []string{"cactusfs-data-0-0"},
			want:    []string{"upgrade stopped: an upgrade from the console (ADR-0029) holds the lock"},
		},
		{
			name: "a stale lock is left to the nodes", mode: "standalone",
			k: func(k kube) kube {
				k.lock = lockOf(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))
				return k
			},
			deletes: []string{"cactusfs-node-0-0"},
			want:    []string{"is older than 25 minutes: its holder is gone, and the nodes settle it"},
		},
		{
			name: "a conflict stops it", mode: "standalone",
			k:    func(k kube) kube { k.extra = append(k.extra, conflict); return k },
			code: 1,
			want: []string{"the apply met a conflict", "error: Apply failed with 1 conflict", "run --install --take-image", "no node was moved"},
		},
		{
			name: "--take-image forces the render once", mode: "cluster", args: []string{"--take-image=cactusfs"},
			k:       func(k kube) kube { k.extra = append(k.extra, dryConflict(takeFields...)); return k },
			deletes: []string{"cactusfs-data-0-0", "cactusfs-meta-0-0", "cactusfs-meta-1-0", "cactusfs-meta-2-0"},
			want:    []string{"--take-image: the render is applied once with --force-conflicts by cactusfs-deploy"},
			check: func(t *testing.T, _ *rig, res result) {
				checkTake(t, res, []string{dryApply, forceApply})
				if kubeIndex(res.calls, 0, "apply", "--server-side", "--force-conflicts") > kubeIndex(res.calls, 0, "delete", "pod") {
					t.Error("a pod was deleted before the render was applied")
				}
			},
		},
		{
			name: "--take-image refuses another field's conflict", mode: "standalone", args: []string{"--take-image=cactusfs"},
			k: func(k kube) kube {
				k.extra = append(k.extra, dryConflict(append(slices.Clone(takeFields), ".spec.template.spec.containers[name=\"cactusfs\"].env")...))
				return k
			},
			code: 2,
			want: []string{"--take-image takes the image and the version label alone, and the apply would also conflict on .spec.template.spec.containers[name=\"cactusfs\"].env"},
			check: func(t *testing.T, _ *rig, res result) {
				checkTake(t, res, []string{dryApply})
			},
		},
		{
			name: "--take-image refuses an error that is no conflict", mode: "standalone", args: []string{"--take-image=cactusfs"},
			k: func(k kube) kube {
				k.extra = append(k.extra, rule{Tool: "kubectl", Contains: []string{"apply", "--dry-run=server"}, Stderr: "error: unable to recognize \"STDIN\": no matches for kind\n", Exit: 1})
				return k
			},
			code: 2,
			want: []string{"--take-image: the dry run of the apply failed with something that is not a conflict"},
			check: func(t *testing.T, _ *rig, res result) {
				checkTake(t, res, []string{dryApply})
			},
		},
		{
			name: "--take-image refuses a conflict with another error beside it", mode: "standalone", args: []string{"--take-image=cactusfs"},
			k: func(k kube) kube {
				c := dryConflict(takeFields...)
				c.Stderr += "Error from server (Invalid): StatefulSet.apps \"cactusfs-node-0\" is invalid\n"
				k.extra = append(k.extra, c)
				return k
			},
			code: 2,
			want: []string{"something that is not a conflict", "Error from server (Invalid)"},
			check: func(t *testing.T, _ *rig, res result) {
				checkTake(t, res, []string{dryApply})
			},
		},
		{
			name: "--take-image refuses a conflict on a field outside the template", mode: "standalone", args: []string{"--take-image=cactusfs"},
			k: func(k kube) kube {
				k.extra = append(k.extra, dryAnswer("error: Apply failed with 1 conflict: conflict with \"admin\" using storage.k8s.io/v1 at 2026-11-22T10:00:00Z: .reclaimPolicy\n"+kubectlAdvice))
				return k
			},
			code:  2,
			want:  []string{"would also conflict on .reclaimPolicy"},
			check: func(t *testing.T, _ *rig, res result) { checkTake(t, res, []string{dryApply}) },
		},
		{
			name: "--take-image refuses a connection error beside a conflict", mode: "standalone", args: []string{"--take-image=cactusfs"},
			k: func(k kube) kube {
				k.extra = append(k.extra, dryAnswer(conflictOf(takeFields...)+"\nThe connection to the server 192.0.2.1:6443 was refused - did you specify the right host or port?\n"))
				return k
			},
			code:  2,
			want:  []string{"something that is not a conflict", "'The connection to the server 192.0.2.1:6443 was refused"},
			check: func(t *testing.T, _ *rig, res result) { checkTake(t, res, []string{dryApply}) },
		},
		{
			name: "--take-image refuses a count that does not match", mode: "standalone", args: []string{"--take-image=cactusfs"},
			k: func(k kube) kube {
				k.extra = append(k.extra, dryAnswer(strings.Replace(conflictOf(takeFields...), "Apply failed with 2 conflicts", "Apply failed with 3 conflicts", 1)))
				return k
			},
			code:  2,
			want:  []string{"counts 3 conflicts and names 2 fields"},
			check: func(t *testing.T, _ *rig, res result) { checkTake(t, res, []string{dryApply}) },
		},
		{
			name: "--take-image forces a single conflict with the error prefix", mode: "standalone", args: []string{"--take-image=cactusfs"},
			k: func(k kube) kube {
				k.extra = append(k.extra, dryAnswer("error: "+conflictOf(takeFields[0])+"Warning: a warning of the server\n"))
				return k
			},
			deletes: []string{"cactusfs-node-0-0"},
			want:    []string{"--take-image: the render is applied once with --force-conflicts"},
			check:   func(t *testing.T, _ *rig, res result) { checkTake(t, res, []string{dryApply, forceApply}) },
		},
		{
			name: "--take-image with nothing to force", mode: "standalone", args: []string{"--take-image=cactusfs"},
			deletes: []string{"cactusfs-node-0-0"},
			want:    []string{"--take-image: the render applies without a conflict; nothing is forced"},
			check: func(t *testing.T, _ *rig, res result) {
				checkTake(t, res, []string{dryApply, mainApply})
			},
		},
		{
			name: "two nodes away refuse it", mode: "cluster",
			k:    func(k kube) kube { k.notReady = []string{"cactusfs-meta-1", "cactusfs-meta-2"}; return k },
			code: 2,
			want: []string{"the nodes cactusfs-meta-1 cactusfs-meta-2 of cactusfs are not Ready"},
		},
		{
			name: "a node away is replaced first", mode: "cluster",
			k: func(k kube) kube {
				k.notReady = []string{"cactusfs-meta-1"}
				k.extra = append(k.extra, rule{Tool: "kubectl", Contains: []string{"get", "pods", "-l"}, After: []string{"delete", "pod", "cactusfs-meta-1-0"}, Stdout: podsJSONNoT(k.mode, k.image)})
				return k
			},
			deletes: []string{"cactusfs-meta-1-0", "cactusfs-data-0-0", "cactusfs-meta-0-0", "cactusfs-meta-2-0"},
			want:    []string{"upgrade: the node cactusfs-meta-1 is not Ready; it is replaced first"},
		},
		{
			name: "a second node away stops it", mode: "cluster",
			k: func(k kube) kube {
				k.extra = append(k.extra, rule{Tool: "kubectl", Contains: []string{"get", "pods", "-l"}, After: []string{"delete", "pod", "cactusfs-data-0-0"}, Stdout: podsJSONNoT(k.mode, k.image, "cactusfs-meta-2")})
				return k
			},
			code:    1,
			deletes: []string{"cactusfs-data-0-0"},
			want:    []string{"upgrade stopped before the node cactusfs-meta-0: the nodes cactusfs-meta-2 are not Ready"},
		},
		{
			name: "what the render no longer holds is pruned", mode: "standalone",
			k: func(k kube) kube {
				k.objects = append(slices.Clone(k.objects), "poddisruptionbudget.policy/cactusfs")
				return k
			},
			deletes: []string{"cactusfs-node-0-0"},
			want:    []string{"pruned what the render no longer holds: poddisruptionbudget.policy/cactusfs"},
			check: func(t *testing.T, _ *rig, res result) {
				prune := kubeIndex(res.calls, 0, "delete", "poddisruptionbudget.policy/cactusfs", "-n", "cactusfs")
				if prune < kubeIndex(res.calls, 0, "apply") || prune > kubeIndex(res.calls, 0, "delete", "pod") {
					t.Errorf("the prune is at %d, not between the apply and the first pod", prune)
				}
			},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.chart()
				r.keepStdin()
				f := newImgFix(imgCommit)
				image := registry + "@sha256:" + f.hex
				values := kubeEntry(t, r, c.mode, clusterUID)
				k := installedKube(kube{mode: c.mode, f: f, image: image, secrets: values})
				if c.k != nil {
					k = c.k(k)
				}
				r.rules(k.rules(t)...)
				env := kubeEnv(f) + c.env
				if c.mode == "cluster" {
					env += "DEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\n"
				}
				res := r.kubeRun(env, append([]string{"--install"}, c.args...)...)
				res.want(t, c.code, c.want...)
				insideNamespace(t, r, res.calls, "cactusfs")
				noForce(t, res.calls, slices.Contains(c.args, "--take-image=cactusfs"))
				if got := kubeDeletes(res.calls); !slices.Equal(got, c.deletes) {
					t.Errorf("the pods deleted are %q, want %q", got, c.deletes)
				}
				for _, w := range writes(res.calls) {
					if args, _ := kubectlArgs(w); args[0] == "create" {
						t.Errorf("an upgrade created %q", args)
					}
				}
				if c.check != nil {
					c.check(t, r, res)
				}
			})
		}
	})
}

// TestK8sStatus: --status lists the pods, every address - a NodePort on the
// machines that run a ready pod of a gateway - probes /healthz and /readyz
// of a published admin port with the CA, and prints the port-forward
// commands when no admin port answered (with K3S_SSH, the pair ssh -L and
// k3s kubectl port-forward); it changes nothing, and exits 0 when every
// node is ready or nothing is installed, 1 when a node is not ready.
func TestK8sStatus(t *testing.T) {
	services := func(admin string) rule {
		adminSvc := obj{"metadata": obj{"name": "cactusfs-admin"}, "spec": obj{"type": "ClusterIP", "ports": []any{obj{"port": 9001}}}}
		if admin != "" {
			adminSvc = obj{"metadata": obj{"name": "cactusfs-admin"}, "spec": obj{"type": "NodePort", "ports": []any{obj{"port": 9001, "nodePort": 30901}}}}
		}
		return rule{Tool: "kubectl", Contains: []string{"get", "services", "cactusfs", "cactusfs-admin"}, Stdout: kjsonNoT(kubeList(
			obj{"metadata": obj{"name": "cactusfs"}, "spec": obj{"type": "NodePort", "ports": []any{obj{"port": 9000, "nodePort": 30900}}}},
			adminSvc))}
	}
	pods := func(t *testing.T, image string, ready bool) rule {
		return rule{Tool: "kubectl", Contains: []string{"get", "pods"}, Stdout: kjson(t, kubeList(podOf("cactusfs-node-0-0", "u1", "r", image, ready, true)))}
	}
	cases := []struct {
		name  string
		env   string
		k     func(t *testing.T, k kube) kube
		code  int
		want  []string
		never []string
	}{
		{
			name: "ready, the admin port inside the cluster",
			k: func(t *testing.T, k kube) kube {
				k.extra = append(k.extra, services(""), pods(t, k.image, true))
				return k
			},
			want: []string{
				"node: cactusfs-node-0, pod cactusfs-node-0-0 on na, ready, image " + registry,
				"S3: https://192.0.2.11:30900 (NodePort, on the machine na, which runs a ready pod)",
				"admin API and console: inside the cluster only, https://cactusfs-admin.cactusfs.svc:9001 (ClusterIP)",
				"the admin API from this machine: kubectl --context prod port-forward -n cactusfs service/cactusfs-admin 9001:9001",
			},
		},
		{
			name: "the admin port published and answering", env: "K8S_ADMIN_SERVICE_TYPE=NodePort\nK8S_ADMIN_ALLOW_FROM=192.0.2.0/24\n",
			k: func(t *testing.T, k kube) kube {
				k.extra = append(k.extra, services("NodePort"), pods(t, k.image, true), rule{Tool: "curl", Contains: []string{"--connect-to"}, Stdout: "200"})
				return k
			},
			want:  []string{"admin API and console: https://192.0.2.11:30901", "/healthz 200, /readyz 200"},
			never: []string{"port-forward"},
		},
		{
			name: "a node not ready", code: 1,
			k: func(t *testing.T, k kube) kube {
				k.extra = append(k.extra, services(""), pods(t, k.image, false))
				return k
			},
			want:  []string{"NOT ready", "the NodePort 30900, on no machine yet"},
			never: []string{"https://192.0.2.11:30900"},
		},
		{
			name: "a console upgrade under way",
			k: func(t *testing.T, k kube) kube {
				k.lock = lockOf(time.Now().UTC().Format(time.RFC3339Nano))
				k.extra = append(k.extra, services(""), pods(t, k.image, true))
				return k
			},
			want: []string{"an upgrade from the console holds the lock cactusfs.io/upgrade on cactusfs-node-0"},
		},
		{
			name: "not installed", code: 0,
			k:    func(_ *testing.T, k kube) kube { k.marker = ""; return k },
			want: []string{"cactusfs is not installed: kubectl --context prod has no namespace cactusfs"},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				image := registry + "@sha256:" + f.hex
				kubeEntry(t, r, "standalone", clusterUID)
				k := c.k(t, installedKube(kube{mode: "standalone", f: f, image: image}))
				r.rules(k.rules(t)...)
				res := r.kubeRun(kubeEnv(f)+c.env, "--status")
				res.want(t, c.code, c.want...)
				for _, n := range c.never {
					if strings.Contains(res.output(), n) {
						t.Errorf("the output says %q:\n%s", n, res.output())
					}
				}
				if w := writes(res.calls); len(w) != 0 {
					t.Errorf("--status changed %v", w)
				}
				insideNamespace(t, r, res.calls, "cactusfs")
			})
		}
		t.Run("over ssh", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			r.rules(rule{Tool: "ssh", Contains: []string{"get", "namespace", "cactusfs"}, Stdout: kjson(t, obj{"metadata": obj{"name": "cactusfs", "annotations": obj{"deploy.cactusfs.io/owner-uid": kubeMarker}}})},
				rule{Tool: "ssh", Contains: []string{"get", "statefulsets,services,networkpolicies,poddisruptionbudgets"}, Stdout: objectsJSON(t, []string{"statefulset.apps/cactusfs-node-0"}, nil)})
			put(t, filepath.Join(r.state("ssh-admin_k3s.example.test", "cactusfs"), "namespace-uid"), kubeMarker+"\n")
			res := r.withEnv("DEPLOY_TARGET=k3s\nK3S_SSH=admin@k3s.example.test\nKMS=local\n", "--status")
			res.want(t, 1, "node: cactusfs-node-0 has no pod",
				"ssh -L 9001:127.0.0.1:9001 -- admin@k3s.example.test, and in that session k3s kubectl port-forward -n cactusfs service/cactusfs-admin 9001:9001")
		})
	})
}

// kjsonNoT is kjson for a value that always encodes.
func kjsonNoT(v any) string {
	data, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		return "{}"
	}
	return string(data) + "\n"
}

// TestK8sUninstall: --uninstall deletes the release's objects and its
// Secrets and keeps its claims, the namespace and STATE_DIR's entry; it is
// refused on another cluster than the recorded one, in a namespace that is
// not the deployment's, and while a console upgrade is under way.
func TestK8sUninstall(t *testing.T) {
	deleteObjects := "delete networkpolicy.networking.k8s.io/cactusfs service/cactusfs service/cactusfs-admin service/cactusfs-nodes statefulset.apps/cactusfs-node-0 -n cactusfs"
	deleteSecrets := "delete secret/cactusfs-root secret/cactusfs-tls -n cactusfs"
	cases := []struct {
		name    string
		entry   string
		env     string
		prepare func(t *testing.T, r *rig, state string, k kube) kube
		code    int
		want    string
		writes  []string
	}{
		{name: "removed, the claims kept", entry: clusterUID, want: "kept: the claims data1-cactusfs-node-0-0 meta-cactusfs-node-0-0, the namespace cactusfs", writes: []string{deleteObjects, deleteSecrets}},
		{name: "another cluster", entry: "another-uid-0000", code: 2, want: "now reach another"},
		{name: "a namespace that is not the deployment's", entry: clusterUID, code: 2, want: "is not this deployment's", prepare: func(_ *testing.T, _ *rig, _ string, k kube) kube {
			k.marker = strings.Repeat("f", 32)
			return k
		}},
		{name: "another namespace than the one recorded", entry: clusterUID, env: "K8S_NAMESPACE=other\n", code: 2, want: "records the namespace cactusfs, and the settings name other"},
		{name: "a console upgrade under way", entry: clusterUID, code: 2, want: "holds the lock cactusfs.io/upgrade", prepare: func(_ *testing.T, _ *rig, _ string, k kube) kube {
			k.lock = lockOf(time.Now().UTC().Format(time.RFC3339Nano))
			return k
		}},
		// The review of 10.10: an uninstall deleted the Secrets without
		// knowing STATE_DIR still held their values.
		{name: "a key file the cluster alone holds", entry: clusterUID, code: 2, want: "config/kms.json is missing from", prepare: func(t *testing.T, _ *rig, state string, k kube) kube {
			remove(t, filepath.Join(state, "config", "kms.json"))
			return k
		}},
		{name: "a passphrase that differs", entry: clusterUID, code: 2, want: "hold different values of passphrase", prepare: func(_ *testing.T, _ *rig, _ string, k kube) kube {
			k.secrets = maps.Clone(k.secrets)
			k.secrets["secrets/kms-passphrase"] = "fixture-other-" + rand.Text()
			return k
		}},
		{
			name: "a certificate given that differs", entry: clusterUID, env: "TLS_CERT_FILE=~/own.crt\nTLS_KEY_FILE=~/own.key\nTLS_CA_FILE=~/own-ca.pem\n", code: 2,
			want: "hold other values than the files the settings name", prepare: func(t *testing.T, r *rig, state string, k kube) kube {
				put(t, filepath.Join(r.home, "own.crt"), "fixture-own-"+rand.Text())
				put(t, filepath.Join(r.home, "own.key"), read(t, filepath.Join(state, "config", "s3.key")))
				put(t, filepath.Join(r.home, "own-ca.pem"), "a CA\n")
				return k
			},
		},
		{name: "a Secret the settings do not name", entry: clusterUID, code: 2, want: "holds the Secret cactusfs-vault-token of cactusfs, which is not one the settings name (it was made under other settings: DEPLOY_MODE, KMS or VAULT_CA_FILE)", prepare: func(_ *testing.T, _ *rig, _ string, k kube) kube {
			k.extra = append([]rule{{Tool: "kubectl", Contains: []string{"get", "secrets", "io.cactusfs.deploy.name=cactusfs,app.kubernetes.io/managed-by=cactusfs-deploy"}, Stdout: "secret/cactusfs-root\nsecret/cactusfs-vault-token\n"}}, k.extra...)
			return k
		}},
		{name: "a StatefulSet another manager applied keeps the Secrets", entry: clusterUID, want: "kept: the Secrets secret/cactusfs-root secret/cactusfs-tls, as a StatefulSet carrying the release's labels that the script did not apply is left", prepare: func(_ *testing.T, _ *rig, _ string, k kube) kube {
			k.foreign = []string{"statefulset.apps/cactusfs-node-0"}
			return k
		}, writes: []string{"delete networkpolicy.networking.k8s.io/cactusfs service/cactusfs service/cactusfs-admin service/cactusfs-nodes -n cactusfs"}},
		{name: "an object another manager applied is left", entry: clusterUID, want: "left: service/cactusfs-admin carries the release's labels and was not applied by the script", prepare: func(_ *testing.T, _ *rig, _ string, k kube) kube {
			k.foreign = []string{"service/cactusfs-admin"}
			return k
		}, writes: []string{"delete networkpolicy.networking.k8s.io/cactusfs service/cactusfs service/cactusfs-nodes statefulset.apps/cactusfs-node-0 -n cactusfs", deleteSecrets}},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				values := kubeEntry(t, r, "standalone", c.entry)
				state := r.state("k8s-prod", "cactusfs")
				k := installedKube(kube{mode: "standalone", f: f, image: registry + "@sha256:" + f.hex, secrets: values})
				k.extra = []rule{
					{Tool: "kubectl", Contains: []string{"get", "secrets", "io.cactusfs.deploy.name=cactusfs,app.kubernetes.io/managed-by=cactusfs-deploy"}, Stdout: "secret/cactusfs-root\nsecret/cactusfs-tls\n"},
					{Tool: "kubectl", Contains: []string{"get", "persistentvolumeclaims"}, Stdout: kjsonNoT(kubeList(obj{"metadata": obj{"name": "meta-cactusfs-node-0-0"}}, obj{"metadata": obj{"name": "data1-cactusfs-node-0-0"}}))},
				}
				if c.prepare != nil {
					k = c.prepare(t, r, state, k)
				}
				r.rules(k.rules(t)...)
				before := tree(t, state)
				res := r.kubeRun(kubeEnv(f)+c.env, "--uninstall")
				res.want(t, c.code, c.want)
				if len(res.calls) > 0 {
					insideNamespace(t, r, res.calls, "cactusfs")
				}
				var got []string
				for _, w := range writes(res.calls) {
					args, _ := kubectlArgs(w)
					got = append(got, strings.Join(args, " "))
				}
				if !slices.Equal(got, c.writes) {
					t.Errorf("the uninstall changed\n%q\nwant\n%q", got, c.writes)
				}
				if !equalTrees(before, tree(t, state)) {
					t.Error("the uninstall changed the entry of STATE_DIR")
				}
			})
		}
	})
}

// TestK8sPurge: --uninstall --purge, confirmed, deletes in the order of
// ADR-0033 section 3 - the workload, the release's claims, a listing that
// proves none and no PersistentVolume bound to one is left (a Released one
// kept by reclaimPolicy Retain is named and left), the Secrets and a
// listing that proves them gone, the namespace (marked, empty when read
// again, deleted by "delete --raw" with preconditions on its uid and
// resourceVersion), the image tags it imported - and last STATE_DIR's
// entry, a sibling entry kept; a step that fails keeps the entry.
// purgeKinds is the kinds the fake API offers to list in a namespace, as
// the purge lists them (events left out).
const purgeKinds = "configmaps,pods,serviceaccounts,secrets,endpoints,endpointslices.discovery.k8s.io,deployments.apps"

func TestK8sPurge(t *testing.T) {
	claims := kubeList(obj{"metadata": obj{"name": "meta-cactusfs-node-0-0"}}, obj{"metadata": obj{"name": "data1-cactusfs-node-0-0"}})
	pv := func(name, claim, phase, policy string) obj {
		return obj{"metadata": obj{"name": name}, "spec": obj{"claimRef": obj{"namespace": "cactusfs", "name": claim}, "persistentVolumeReclaimPolicy": policy}, "status": obj{"phase": phase}}
	}
	base := func(k kube) kube {
		k.extra = append(k.extra,
			rule{Tool: k.tool, Contains: []string{"get", "persistentvolumeclaims"}, After: []string{"delete", "persistentvolumeclaim/meta-cactusfs-node-0-0"}, Stdout: kjsonNoT(kubeList())},
			rule{Tool: k.tool, Contains: []string{"get", "persistentvolumeclaims"}, Stdout: kjsonNoT(claims)},
			rule{Tool: k.tool, Contains: []string{"get", "persistentvolumes"}, Stdout: kjsonNoT(kubeList(
				pv("pvc-1", "meta-cactusfs-node-0-0", "Released", "Retain"),
				pv("pvc-other", "meta-cactusfs-node-0-0", "Bound", "Delete")))},
			rule{Tool: k.tool, Contains: []string{"get", "secrets", "io.cactusfs.deploy.name=cactusfs,app.kubernetes.io/managed-by=cactusfs-deploy"}, After: []string{"delete", "secret/cactusfs-root"}, Stdout: ""},
			rule{Tool: k.tool, Contains: []string{"get", "secrets", "io.cactusfs.deploy.name=cactusfs,app.kubernetes.io/managed-by=cactusfs-deploy"}, Stdout: "secret/cactusfs-root\nsecret/cactusfs-tls\n"},
			rule{Tool: k.tool, Contains: []string{"api-resources", "--namespaced"}, Stdout: "configmaps\npods\nserviceaccounts\nsecrets\nendpoints\nendpointslices.discovery.k8s.io\ndeployments.apps\nevents\nevents.events.k8s.io\n"},
			// What is left: what every namespace holds, and the
			// deployment's own objects on their way out.
			rule{Tool: k.tool, Contains: []string{"get", purgeKinds}, Stdout: "configmap/kube-root-ca.crt\nserviceaccount/default\npod/cactusfs-node-0-0\nendpoints/cactusfs\nendpointslice.discovery.k8s.io/cactusfs-x7k2p\n"},
			rule{Tool: k.tool, Contains: []string{"get", "pods", "-A"}, Stdout: kjsonNoT(kubeList(podOf("web-0", "u", "r", "registry.example.test/web:1", true, false)))},
			rule{Tool: k.tool, Contains: []string{"get", "statefulsets,deployments,daemonsets,jobs,cronjobs", "-A"}, Stdout: kjsonNoT(kubeList())},
			rule{Tool: k.tool, Contains: []string{"delete", "--raw"}, SaveStdin: "delete.json"},
		)
		return k
	}
	// The PV of another namespace's claim of the same name is not ours.
	otherNS := func(k kube) kube {
		k.extra = append([]rule{{Tool: "kubectl", Contains: []string{"get", "persistentvolumes"}, Stdout: kjsonNoT(kubeList(
			obj{"metadata": obj{"name": "pvc-x"}, "spec": obj{"claimRef": obj{"namespace": "shop", "name": "meta-cactusfs-node-0-0"}, "persistentVolumeReclaimPolicy": "Delete"}, "status": obj{"phase": "Bound"}},
			pv("pvc-1", "meta-cactusfs-node-0-0", "Released", "Retain")))}}, k.extra...)
		return k
	}
	cases := []struct {
		name   string
		env    string
		args   []string
		k      func(k kube) kube
		code   int
		want   []string
		purged bool
		noName bool
		check  func(t *testing.T, r *rig, res result)
	}{
		{
			name: "purged", k: otherNS, purged: true,
			want: []string{
				"purge: no claim of cactusfs is left", "left, as their StorageClass's reclaimPolicy Retain keeps them: pvc-1",
				"purge: deleted the namespace cactusfs (uid " + nsUID + ", resourceVersion 41: the preconditions guard the Namespace object itself, not what it holds", "purged: cactusfs",
			},
			check: func(t *testing.T, r *rig, res result) {
				steps := []int{
					kubeIndex(res.calls, 0, "delete", "networkpolicy.networking.k8s.io/cactusfs"),
					kubeIndex(res.calls, 0, "delete", "persistentvolumeclaim/data1-cactusfs-node-0-0"),
				}
				steps = append(steps, kubeIndex(res.calls, steps[1], "get", "persistentvolumeclaims"))
				steps = append(steps, kubeIndex(res.calls, steps[2], "get", "persistentvolumes"))
				steps = append(steps, kubeIndex(res.calls, steps[3], "delete", "secret/cactusfs-root"))
				steps = append(steps, kubeIndex(res.calls, steps[4], "get", "secrets"))
				steps = append(steps, kubeIndex(res.calls, steps[5], "get", "namespace", "cactusfs"))
				steps = append(steps, kubeIndex(res.calls, steps[6], "get", purgeKinds))
				steps = append(steps, kubeIndex(res.calls, steps[7], "delete", "--raw", "/api/v1/namespaces/cactusfs", "-f", "-"))
				for i, s := range steps {
					if s < 0 || (i > 0 && s <= steps[i-1]) {
						t.Fatalf("the purge's steps are at %v: step %d is out of order", steps, i)
					}
				}
				if got := read(t, filepath.Join(r.dir, "delete.json")); got != `{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":"`+nsUID+`","resourceVersion":"41"}}`+"\n" {
					t.Errorf("the namespace's delete sent %q", got)
				}
				for _, c := range res.calls {
					if args, ok := kubectlArgs(c); ok && args[0] == "delete" && slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "persistentvolume/") || a == "persistentvolumes" }) {
						t.Errorf("a PersistentVolume was deleted: %q", args)
					}
				}
			},
		},
		{
			name: "claims that stay keep the entry", code: 1,
			k: func(k kube) kube {
				k.extra = append([]rule{{Tool: "kubectl", Contains: []string{"get", "persistentvolumeclaims"}, Stdout: kjsonNoT(claims)}}, k.extra...)
				return k
			},
			want: []string{"purge: the claims data1-cactusfs-node-0-0 meta-cactusfs-node-0-0 of cactusfs are still there after WAIT_TIMEOUT=3 s"},
			check: func(t *testing.T, _ *rig, res result) {
				if kubeIndex(res.calls, 0, "delete", "secret/cactusfs-root") >= 0 || kubeIndex(res.calls, 0, "delete", "--raw") >= 0 {
					t.Error("the Secrets or the namespace were deleted while claims are left")
				}
			},
		},
		{
			name: "a volume still bound keeps the entry", code: 1,
			want: []string{"purge: the PersistentVolumes pvc-other (Bound) of the claims of cactusfs are still there"},
		},
		{
			name: "a namespace that holds more is left", purged: true,
			k: func(k kube) kube {
				k = otherNS(k)
				k.extra = append([]rule{{Tool: "kubectl", Contains: []string{"get", purgeKinds}, Stdout: "configmap/kube-root-ca.crt\nendpoints/cactusfs\ndeployment.apps/web\n"}}, k.extra...)
				return k
			},
			want: []string{"purge: the namespace cactusfs is left: it holds deployment.apps/web, which is not the deployment's", "purged: cactusfs"},
			check: func(t *testing.T, _ *rig, res result) {
				if kubeIndex(res.calls, 0, "delete", "--raw") >= 0 {
					t.Error("a namespace that holds more was deleted")
				}
			},
		},
		{
			name: "a namespace the context may not delete is left", purged: true,
			k: func(k kube) kube {
				k = otherNS(k)
				k.extra = append([]rule{{Tool: "kubectl", Contains: []string{"delete", "--raw"}, Stderr: "Error from server (Forbidden): namespaces \"cactusfs\" is forbidden\n", Exit: 1}}, k.extra...)
				return k
			},
			want: []string{"the context may not delete the namespace cactusfs (Forbidden): it is left, empty, to the cluster's administrator", "purged: cactusfs"},
		},
		{
			name: "a precondition that fails keeps the entry", code: 1,
			k: func(k kube) kube {
				k = otherNS(k)
				k.extra = append([]rule{{Tool: "kubectl", Contains: []string{"delete", "--raw"}, Stderr: "Error from server (Conflict): Precondition failed: UID in precondition\n", Exit: 1}}, k.extra...)
				return k
			},
			want: []string{"purge: deleting the namespace cactusfs failed"},
		},
		{
			name: "a key file the cluster alone holds is not deleted", code: 2,
			k: func(k kube) kube {
				k.extra = append(k.extra, rule{
					Tool: "kubectl", Contains: []string{"get", "secret", "cactusfs-kms"},
					Stdout: secretJSON("cactusfs-kms", "Opaque", "cactusfs", map[string]string{"kms.json": "fixture-only-here-" + rand.Text()}),
				})
				return k
			},
			want: []string{"hold different values of kms.json"},
		},
		{
			name: "a Secret the settings do not name goes with the purge", purged: true, k: func(k kube) kube {
				k = otherNS(k)
				sel := "io.cactusfs.deploy.name=cactusfs,app.kubernetes.io/managed-by=cactusfs-deploy"
				k.extra = append([]rule{
					{Tool: "kubectl", Contains: []string{"get", "secrets", sel}, After: []string{"delete", "secret/cactusfs-root"}, Stdout: ""},
					{Tool: "kubectl", Contains: []string{"get", "secrets", sel}, Stdout: "secret/cactusfs-root\nsecret/cactusfs-vault-token\n"},
				}, k.extra...)
				return k
			},
			want: []string{"the Secret cactusfs-vault-token of cactusfs is not one the settings name (they changed since it was made); the purge deletes it with the rest", "purge: deleted the Secrets secret/cactusfs-root secret/cactusfs-vault-token", "purged: cactusfs"},
		},
		{
			name: "no record of the namespace's name", code: 2, noName: true,
			want: []string{"holds no record of cactusfs (its target, its namespace's name and marker)"},
		},
		{
			name: "the namespace is gone already", purged: true,
			k:    func(k kube) kube { k.marker = ""; return k },
			want: []string{"has no namespace cactusfs", "purged: cactusfs"},
		},
		{
			name: "what hostpath made is named and left", env: "K8S_STORAGE=hostpath\n", k: otherNS, purged: true,
			want: []string{"purge: left, cluster-scoped and never deleted by the script: the StorageClass cactusfs-cactusfs-local and the PersistentVolumes cactusfs-<node ID>-<volume>, and the directories /var/lib/cactusfs-deploy/cactusfs on the node"},
		},
		{
			name: "not confirmed", args: []string{"--uninstall", "--purge"}, code: 2,
			want: []string{"type its name when asked at a terminal, or say --purge-confirm=cactusfs; nothing was deleted"},
		},
		{
			name: "a dry run", args: []string{"--uninstall", "--purge", "--purge-confirm=cactusfs", "--dry-run"}, k: otherNS,
			want: []string{"run: kubectl --context prod delete persistentvolumeclaim/data1-cactusfs-node-0-0", "run: rm -rf --"},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				values := kubeEntry(t, r, "standalone", clusterUID)
				if c.noName {
					remove(t, filepath.Join(r.state("k8s-prod", "cactusfs"), "namespace"))
				}
				sibling := r.state("k8s-prod", "other")
				put(t, filepath.Join(sibling, "target"), "kubectl --context prod\n")
				k := base(installedKube(kube{tool: "kubectl", mode: "standalone", f: f, image: registry + "@sha256:" + f.hex, secrets: values}))
				if c.k != nil {
					k = c.k(k)
				}
				r.rules(k.rules(t)...)
				args := c.args
				if args == nil {
					args = []string{"--uninstall", "--purge", "--purge-confirm=cactusfs"}
				}
				res := r.kubeRun(kubeEnv(f)+c.env, args...)
				res.want(t, c.code, c.want...)
				if len(res.calls) > 0 {
					insideNamespace(t, r, res.calls, "cactusfs")
				}
				state := r.state("k8s-prod", "cactusfs")
				_, err := os.Stat(state)
				if c.purged != errors.Is(err, os.ErrNotExist) {
					t.Errorf("purged %v, and the entry: %v", c.purged, err)
				}
				if _, err := os.Stat(filepath.Join(sibling, "target")); err != nil {
					t.Errorf("a sibling entry was touched: %v", err)
				}
				if c.code == 2 || slices.Contains(args, "--dry-run") {
					if w := writes(res.calls); len(w) != 0 {
						t.Errorf("a refused or dry run changed %v", w)
					}
				}
				if c.check != nil {
					c.check(t, r, res)
				}
			})
		}
		t.Run("the images it imported, over ssh", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			f := newImgFix(imgCommit)
			state := r.state("ssh-admin_k3s.example.test", "cactusfs")
			put(t, filepath.Join(state, "target"), "k3s kubectl over ssh admin@k3s.example.test\nengine-id="+clusterUID+"\nkms=local\n")
			put(t, filepath.Join(state, "namespace-uid"), kubeMarker+"\n")
			put(t, filepath.Join(state, "namespace"), "cactusfs\n")
			put(t, filepath.Join(state, "images"), "docker.io/library/cactusfs:0.7.0-412-0123456789ab\ndocker.io/library/cactusfs:0.7.0-412-feedfacefeed\ndocker.io/library/cactusfs:0.7.0-412-0000beefcafe\n")
			k := base(installedKube(kube{tool: "ssh", mode: "standalone", f: f, image: f.ref()}))
			// A pod of another namespace runs the second tag, by the name
			// its spec gives it: that tag is kept.
			k.extra = append([]rule{
				{Tool: "ssh", Contains: []string{"get", "persistentvolumes"}, Stdout: kjsonNoT(kubeList())},
				{Tool: "ssh", Contains: []string{"get", "pods", "-A"}, Stdout: kjsonNoT(kubeList(podOf("other-0", "u", "r", "cactusfs:0.7.0-412-feedfacefeed", true, false)))},
				// A Deployment scaled to none names the third in its template.
				{Tool: "ssh", Contains: []string{"get", "statefulsets,deployments,daemonsets,jobs,cronjobs", "-A"}, Stdout: kjsonNoT(kubeList(obj{
					"kind": "Deployment", "metadata": obj{"name": "later", "namespace": "shop"},
					"spec": obj{"template": obj{"spec": obj{"containers": []any{obj{"name": "c", "image": "cactusfs:0.7.0-412-0000beefcafe"}}}}},
				}))},
			}, k.extra...)
			r.rules(k.rules(t)...)
			res := r.withEnv("DEPLOY_TARGET=k3s\nK3S_SSH=admin@k3s.example.test\nKMS=local\n", "--uninstall", "--purge", "--purge-confirm=cactusfs")
			res.want(t, 0, "purge: removed the image docker.io/library/cactusfs:0.7.0-412-0123456789ab it imported",
				"purge: the image docker.io/library/cactusfs:0.7.0-412-feedfacefeed is kept: a pod or a workload's template still names it",
				"purge: the image docker.io/library/cactusfs:0.7.0-412-0000beefcafe is kept: a pod or a workload's template still names it", "purged: cactusfs")
			rm := callsOf(res, "ssh", "images", "rm")
			want := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=20", "-o", "ControlPath=none", "--", "admin@k3s.example.test", "k3s", "ctr", "-n", "k8s.io", "images", "rm", "docker.io/library/cactusfs:0.7.0-412-0123456789ab"}
			if len(rm) != 1 || !slices.Equal(rm[0].Args, want) {
				t.Errorf("the image removed is %v", rm)
			}
			if i, j := slices.IndexFunc(res.calls, func(c call) bool { return slices.Contains(c.Args, "--raw") }), slices.IndexFunc(res.calls, func(c call) bool { return slices.Contains(c.Args, "rm") }); i < 0 || j < i {
				t.Errorf("the image was removed at %d, the namespace at %d", j, i)
			}
			if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the entry is still there: %v", err)
			}
		})
	})
}

// TestK8sEngineReplaced: a cluster made again (another UID of kube-system
// behind the same settings) is refused, and taken with --engine-replaced,
// which says so and records its ID.
func TestK8sEngineReplaced(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		r.chart()
		f := newImgFix(imgCommit)
		values := kubeEntry(t, r, "standalone", "gone-0000")
		k := installedKube(kube{mode: "standalone", f: f, image: registry + "@sha256:" + f.hex, secrets: values})
		k.podImage = k.image
		r.rules(k.rules(t)...)
		res := r.kubeRun(kubeEnv(f), "--install", "--engine-replaced")
		res.want(t, 0, "--engine-replaced: the deployment made on the cluster 'gone-0000' is taken as gone", "state: recorded the engine "+clusterUID)
		if got := read(t, filepath.Join(r.state("k8s-prod", "cactusfs"), "target")); got != "kubectl --context prod\nengine-id="+clusterUID+"\nkms=local\n" {
			t.Errorf("the record is %q", got)
		}
	})
}

// shq is the word as a shell reads it back, in single quotes.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// TestOneExitTrap: the cleanups of a run are one EXIT trap (PLAN 10.10,
// common.sh): each command is written into it when it is added, its words
// quoted - a path with spaces, quotes and "$(...)" is removed as it is, and
// a variable assigned afterwards changes nothing - and they run last added
// first, on an exit, a refusal, a failure and a TERM; one dropped is not
// run; a subshell that adds its own (after cleanup_none) runs only those
// when it ends.
func TestOneExitTrap(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range []struct {
			name, end string
			code      int
		}{
			{"an exit", "exit 0", 0},
			{"a refusal", "refuse no", 2},
			{"a failure", "fail no", 1},
			{"a TERM", "kill -TERM $$; sleep 5", 143},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				odd := filepath.Join(r.dir, "a dir with 'quotes' and $(id)")
				outer := filepath.Join(r.dir, "outer")
				inner := filepath.Join(outer, "inner")
				dropped := filepath.Join(r.dir, "dropped")
				sub := filepath.Join(r.dir, "sub")
				for _, d := range []string{odd, inner, dropped, sub} {
					if err := os.MkdirAll(d, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				code := "d=" + shq(odd) + "; cleanup_add rm -rf -- \"$d\"; d=/nonexistent; " +
					// Last added, first run: inner goes before its parent.
					"cleanup_add rmdir -- " + shq(outer) + "; cleanup_add rm -rf -- " + shq(inner) + "; " +
					"cleanup_add rm -rf -- " + shq(dropped) + "; cleanup_drop rm -rf -- " + shq(dropped) + "; " +
					"(cleanup_none; cleanup_add rm -rf -- " + shq(sub) + "; exit 0); " +
					"[ -d " + shq(odd) + " ] && [ ! -d " + shq(sub) + " ] && echo the-subshell-ran-its-own; " + c.end
				res := r.source(code)
				res.want(t, c.code, "the-subshell-ran-its-own")
				for _, d := range []string{odd, outer} {
					if _, err := os.Stat(d); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("%s was not removed: %v", d, err)
					}
				}
				if _, err := os.Stat(dropped); err != nil {
					t.Errorf("a cleanup that was dropped ran: %v", err)
				}
			})
		}
	})
}

// TestWorkdirInSubstitution: image_in_workdir called in a command
// substitution, as image_build and image_podman_push call it, keeps the
// run's own cleanups for the run - started with cleanup_none, the
// substitution's trap removes its work directory and nothing else (the
// review of the 10.10 merge: without it, the substitution's EXIT trap ran
// every cleanup of the run, CE_HOST's pinned known_hosts among them, when
// it ended) - and the run's run when the run ends.
func TestWorkdirInSubstitution(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		r = r.fresh(t)
		run := filepath.Join(r.dir, "the run's")
		if err := os.MkdirAll(run, 0o700); err != nil {
			t.Fatal(err)
		}
		code := ". \"$DEPLOY_DIR/lib/image.sh\"; " +
			"cleanup_add rm -rf -- " + shq(run) + "; " +
			"in_dir() { [ -d \"$1\" ] && echo \"$1\"; }; " +
			"w=$(cleanup_none; image_in_workdir in_dir); " +
			"[ -n \"$w\" ] && [ ! -d \"$w\" ] && echo the-workdir-went; " +
			"[ -d " + shq(run) + " ] && echo the-run-kept-its-own; exit 0"
		res := r.source(code, "DEPLOY_DIR="+filepath.Join(r.repo, "deploy"), "TMPDIR="+r.dir)
		res.want(t, 0, "the-workdir-went", "the-run-kept-its-own")
		if _, err := os.Stat(run); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the run's cleanup did not run when it ended: %v", err)
		}
	})
}

// TestWorkdirSubstitutionsStartClean: every command substitution of the
// scripts that calls a function adding a cleanup starts with cleanup_none
// (TestWorkdirInSubstitution says why).
func TestWorkdirSubstitutionsStartClean(t *testing.T) {
	t.Parallel()
	adders := regexp.MustCompile(`\$\(\s*(?:cleanup_none\s*[;\n]\s*)?(image_in_workdir|k8s_in_workdir|ce_work_begin|secrets_begin|k8s_secrets_ensure|k8s_secrets_verify|k8s_namespace_check)\b`)
	dir := filepath.Join("..", "..", "deploy")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	files, err := fs.Glob(root.FS(), "lib/*.sh")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "deploy.sh")
	seen := 0
	for _, f := range files {
		body, err := root.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		// Comments out, lines kept: shfmt puts a substitution's commands on
		// lines of their own.
		lines := strings.Split(string(body), "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				lines[i] = ""
			}
		}
		text := strings.Join(lines, "\n")
		for _, at := range adders.FindAllStringIndex(text, -1) {
			seen++
			if m := text[at[0]:at[1]]; !strings.Contains(m, "cleanup_none") {
				t.Errorf("%s:%d: %q does not start with cleanup_none", f, strings.Count(text[:at[0]], "\n")+1, m)
			}
		}
	}
	if seen == 0 {
		t.Error("no command substitution of image_in_workdir was found: the scripts were not read")
	}
}

// TestK8sGenerationFails: a step of making the secrets that fails - here,
// reading dev-init's directory out of its container - ends the install
// with exit 1 before any Secret is created (the review of 10.10: on the
// left of "||" a failed step was passed over).
func TestK8sGenerationFails(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		r.chart()
		f := newImgFix(imgCommit)
		k := kube{mode: "standalone", f: f, image: registry + "@sha256:" + f.hex, devInit: k8sDevInit("standalone")}
		k.extra = []rule{{Tool: "docker", Contains: []string{"cp", "cactusfs-dev-init:/tmp/out", "-"}, Stderr: "Error: no such container\n", Exit: 1}}
		r.rules(k.rules(t)...)
		res := r.kubeRun(kubeEnv(f), "--install")
		res.want(t, 1, "copying what dev-init wrote out of cactusfs-dev-init failed")
		for _, c := range res.calls {
			if args, ok := kubectlArgs(c); ok && len(args) > 1 && args[0] == "create" && args[1] == "--field-manager=cactusfs-deploy" {
				t.Errorf("a Secret was created after the generation failed: %q", args)
			}
			if args, ok := kubectlArgs(c); ok && len(args) > 0 && args[0] == "apply" {
				t.Errorf("the render was applied after the generation failed: %q", args)
			}
		}
		if _, err := os.Stat(filepath.Join(r.state("k8s-prod", "cactusfs"), "config")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a configuration was kept from a dev-init that failed: %v", err)
		}
	})
}

// TestK8sHostpathSaidFirst: with K8S_STORAGE=hostpath the install says which
// cluster-scoped objects it will make before it changes anything, and once.
func TestK8sHostpathSaidFirst(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		r.chart()
		f := newImgFix(imgCommit)
		k := kube{mode: "standalone", f: f, image: registry + "@sha256:" + f.hex, devInit: k8sDevInit("standalone")}
		r.rules(k.rules(t)...)
		res := r.kubeRun(kubeEnv(f)+"K8S_STORAGE=hostpath\n", "--install")
		said := "K8S_STORAGE=hostpath makes cluster-scoped objects: the StorageClass cactusfs-cactusfs-local and 5 PersistentVolumes"
		res.want(t, 0, said)
		if strings.Count(res.stdout, said) != 1 || strings.Index(res.stdout, said) > strings.Index(res.stdout, "run: kubectl --context prod create -f -") {
			t.Errorf("the cluster-scoped objects are not said once, before the first change:\n%s", res.stdout)
		}
	})
}

// TestPrintNamespaceImport: the ClusterRole lists pods and the workloads
// cluster-wide only when the image is imported into k3s (a purge's check
// of the tags still in use), never otherwise.
func TestPrintNamespaceImport(t *testing.T) {
	tableShells(t, func(t *testing.T, r *rig, _ bool) {
		res := r.withEnv("KMS=local\nK8S_CONTEXT=team-a\nK3S_IMPORT=docker exec -i k3s-bench ctr -n k8s.io images import -\n", "--docker", "k3s", "--print-namespace")
		res.want(t, 0)
		for _, w := range []string{
			"  - apiGroups: [\"\"]\n    resources: [nodes, services, persistentvolumes, pods]\n    verbs: [list]\n",
			"  - apiGroups: [apps]\n    resources: [statefulsets, deployments, daemonsets]\n    verbs: [list]\n",
			"  - apiGroups: [batch]\n    resources: [jobs, cronjobs]\n    verbs: [list]\n",
		} {
			if !strings.Contains(res.stdout, w) {
				t.Errorf("the ClusterRole of an import does not hold %q:\n%s", w, res.stdout)
			}
		}
		other := r.fresh(t).withEnv("KMS=local\nK8S_CONTEXT=team-a\n", "--docker", "k8s", "--print-namespace")
		if strings.Contains(other.stdout, "persistentvolumes, pods") || strings.Contains(other.stdout, "daemonsets") {
			t.Errorf("the ClusterRole lists pods or workloads without an import:\n%s", other.stdout)
		}
	})
}
