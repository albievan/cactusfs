//go:build deployscript && deployrender

package deploy_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The render test needs helm and kubeconform, and the Kubernetes schemas
// kubeconform reads instead of the network: the builder image has all three
// (the Dockerfile's helm and helmtools stages), and "make deploy-check" runs
// this file there, with the tag deployrender. A missing one fails the test.

// builderTool is the path of a program of the builder image.
func builderTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("%s is not installed: it is in the builder image - run make deploy-check (%v)", name, err)
	}
	return path
}

// schemaLayout is how kubeconform finds a schema in the builder's directory
// (as scripts/helm-lint.sh gives it).
const schemaLayout = "{{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json"

// schemas is the builder's directory of Kubernetes schemas and the one
// version it holds (v1.x.y).
func schemas(t *testing.T) (dir, version string) {
	t.Helper()
	dir = os.Getenv("KUBERNETES_SCHEMAS")
	if dir == "" {
		dir = "/opt/kubernetes-schemas"
	}
	found, err := filepath.Glob(filepath.Join(dir, "v*-standalone-strict"))
	if err != nil || len(found) != 1 {
		t.Fatalf("%s must hold the schemas of one Kubernetes version (the builder image has them): found %v, %v", dir, found, err)
	}
	return dir, strings.TrimSuffix(filepath.Base(found[0]), "-standalone-strict")
}

// namespacedKinds are the kinds a render may hold on any cluster; the
// hostpath kinds only with K8S_STORAGE=hostpath (ADR-0033 section 7).
var (
	namespacedKinds = []string{"Service", "StatefulSet", "NetworkPolicy", "PodDisruptionBudget", "Secret", "ConfigMap", "ServiceAccount", "Role", "RoleBinding"}
	hostpathKinds   = []string{"StorageClass", "PersistentVolume"}
)

// TestProfilesRender: every profile of test/deploy/testdata/k8s renders
// through the script with the builder's helm, and the render passes
// kubeconform -strict against the builder's schemas; it holds no
// cluster-scoped kind but under K8S_STORAGE=hostpath, and a published
// Service keeps the client's address. The Namespace, the Role and
// ClusterRole of --print-namespace and the Secrets the script writes for the
// profile pass kubeconform -strict as well.
func TestProfilesRender(t *testing.T) {
	helm := builderTool(t, "helm")
	kubeconform := builderTool(t, "kubeconform")
	dir, version := schemas(t)
	// One shell: the other tests run the code that is shell-specific
	// under all three; this one judges what the chart makes of the values.
	sh := shells(t)[0]
	kind := regexp.MustCompile(`(?m)^kind: (\S+)$`)
	for name, p := range profiles(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, sh)
			r.chart()
			r.without("helm")
			if err := os.Symlink(helm, filepath.Join(r.bin, "helm")); err != nil {
				t.Fatal(err)
			}
			profileFiles(t, r, p.env)
			values := filepath.Join(r.dir, "values.yaml")
			render := filepath.Join(r.dir, "render.yaml")
			made := filepath.Join(r.dir, "made.yaml")
			// Placeholders for every file a Secret is read from, so that
			// the Secrets can be written and judged too.
			code := fmt.Sprintf(`k8s_secret_list | while IFS="$(printf '\t')" read -r _s _t _k _c _f; do mkdir -p "$(dirname "$_f")"; printf placeholder >"$_f"; done
k8s_values '%[1]s' %[2]d machine-1 '%[3]s'
k8s_render '%[1]s' '%[4]s' >'%[5]s'
{ k8s_namespace_yaml 0123456789abcdef0123456789abcdef; k8s_rbac_yaml; d=$(mktemp -d); k8s_secrets_yaml "$d"; } >'%[6]s'`,
				values, p.nodes, testImage, version, render, made)
			res := r.k8s(p.env+"HELM=helm\n", code, false)
			res.want(t, 0)
			// The render, and the Namespace and Secrets the script writes
			// itself (their schemas are in the Dockerfile's helmtools stage).
			for _, f := range []string{render, made} {
				cmd := exec.CommandContext(t.Context(), kubeconform, "-strict", "-summary",
					"-kubernetes-version", strings.TrimPrefix(version, "v"),
					"-schema-location", filepath.Join(dir, schemaLayout), f)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("kubeconform -strict on %s: %v\n%s", filepath.Base(f), err, out)
				}
			}
			text := read(t, render)
			hostpath := strings.Contains(p.env, "\nK8S_STORAGE=hostpath\n")
			var kinds []string
			for _, m := range kind.FindAllStringSubmatch(text, -1) {
				kinds = append(kinds, m[1])
				switch {
				case slices.Contains(namespacedKinds, m[1]):
				case hostpath && slices.Contains(hostpathKinds, m[1]):
				default:
					t.Errorf("the render holds a %s: no cluster-scoped kind but under K8S_STORAGE=hostpath", m[1])
				}
			}
			if hostpath && (!slices.Contains(kinds, "StorageClass") || !slices.Contains(kinds, "PersistentVolume")) {
				t.Errorf("K8S_STORAGE=hostpath and no StorageClass or PersistentVolume: %v", kinds)
			}
			published := regexp.MustCompile(`(?m)^  type: (NodePort|LoadBalancer)$`).FindAllString(text, -1)
			local := strings.Count(text, "\n  externalTrafficPolicy: Local\n")
			if len(published) != local {
				t.Errorf("%d Services are published and %d keep the client's address (externalTrafficPolicy: Local)", len(published), local)
			}
			if !strings.Contains(read(t, made), "kind: Namespace") {
				t.Error("no namespace was written")
			}
		})
	}
}
