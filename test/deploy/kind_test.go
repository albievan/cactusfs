//go:build deployscript

package deploy_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Docker Desktop's Kubernetes in kind mode (the route kind of
// deploy/lib/image.sh): its one node is a container of the Docker engine,
// desktop-control-plane, with a containerd of its own that does not see
// the engine's images. The image is imported into that containerd through
// "docker exec", checked there, recorded, and removed by a purge.

// kindNode is the node of Docker Desktop's Kubernetes in kind mode.
const kindNode = "desktop-control-plane"

// kindNodes is kubectl's listing of a cluster of one node of the name and
// container runtime given (-o json).
func kindNodes(t *testing.T, name, runtimeVersion string) string {
	t.Helper()
	return kjson(t, kubeList(obj{
		"metadata": obj{"name": name},
		"status": obj{
			"addresses": []any{obj{"type": "InternalIP", "address": "192.0.2.11"}},
			"nodeInfo":  obj{"containerRuntimeVersion": runtimeVersion, "architecture": runtime.GOARCH},
		},
	}))
}

// ctrList is what "ctr images ls" prints of one image: its name, and the
// digest of its target.
func ctrList(name, target string) string {
	return "REF TYPE DIGEST SIZE PLATFORMS LABELS \n" +
		name + " application/vnd.oci.image.manifest.v1+json sha256:" + target + " 30.1 MiB linux/" + runtime.GOARCH + " io.cri-containerd.image=managed \n"
}

// kindExec is the words of a call of the kind node's ctr, in its namespace
// k8s.io, through docker exec.
func kindExec(words ...string) []string {
	return append([]string{"exec", kindNode, "ctr", "--namespace", "k8s.io"}, words...)
}

// kindMode is the answers of Docker Desktop in kind mode: the cluster's one
// node, of a containerd runtime, and the container of that name on the
// Docker engine.
func kindMode(t *testing.T) []rule {
	t.Helper()
	return []rule{
		{Tool: "kubectl", Contains: []string{"get", "nodes", "json"}, Stdout: kindNodes(t, kindNode, "containerd://2.2.0")},
		{Tool: "docker", Contains: []string{"inspect", "--type", "container", kindNode}, Stdout: "/" + kindNode + "\n"},
	}
}

// kindHolds is the answers of the kind node's containerd that holds the
// image of the name once it is imported: nothing of that name before, the
// name after, the digest of its target (a manifest), and that manifest,
// naming the configuration given.
func kindHolds(name, manifest, config string) []rule {
	return []rule{
		{Tool: "docker", Contains: kindExec("images", "ls", "-q", "name=="+name), After: []string{"images", "import"}, Stdout: name + "\n"},
		{Tool: "docker", Contains: kindExec("images", "ls", "-q"), Stdout: ""},
		{Tool: "docker", Contains: kindExec("images", "ls", "name=="+name), Stdout: ctrList(name, manifest)},
		{Tool: "docker", Contains: kindExec("content", "get", "sha256:"+manifest), Stdout: ociManifest(manifest, config)["blobs/sha256/"+manifest]},
	}
}

// kindImageRules is what an image check on Docker Desktop's Kubernetes asks
// before it delivers the image f made of binaries: the API server, the
// nodes by their architecture (this machine's), the engine here and git.
func kindImageRules(f imgFix) []rule {
	arch := runtime.GOARCH
	rules := []rule{kubeServer("https://kubernetes.docker.internal:6443")}
	rules = append(rules, gitRules("")...)
	rules = append(rules, nodeRules("kubectl", arch)...)
	return append(rules, imgEngine("docker", arch, arch, f)...)
}

// kindEnv is the env file of an image check on Docker Desktop's Kubernetes,
// through the Docker context desktop-linux.
const kindEnv = "KMS=local\nDEPLOY_TARGET=k8s\nK8S_CONTEXT=docker-desktop\nENGINE_CONTEXT=desktop-linux\n"

// TestKindDetect: the context docker-desktop takes the route kind only when
// the cluster is one node of a containerd runtime that is a container of
// the Docker engine of the settings' context; Docker Desktop's kubeadm mode
// (the node is the engine's own, docker://), a node of no such container,
// and a cluster that cannot say keep the route local. Nothing is changed.
func TestKindDetect(t *testing.T) {
	cases := []struct {
		name  string
		rules func(t *testing.T) []rule
		route string
		want  []string
		// inspected: the Docker engine was asked for the node's container.
		inspected bool
	}{
		{
			name: "kind mode", rules: kindMode, route: "kind", inspected: true,
			want: []string{
				"image: Docker Desktop's Kubernetes runs in kind mode - its one node, desktop-control-plane, is a container of the Docker engine here with a containerd of its own (containerd://2.2.0)",
				"saved there and imported into the containerd of the cluster's one node, the container desktop-control-plane of the Docker engine here (Docker Desktop's Kubernetes in kind mode)",
			},
		},
		{
			name: "kubeadm mode", route: "local",
			rules: func(t *testing.T) []rule {
				return []rule{{Tool: "kubectl", Contains: []string{"get", "nodes", "json"}, Stdout: kindNodes(t, "docker-desktop", "docker://28.5.1")}}
			},
			want: []string{"on the Docker engine here, whose images the cluster's one node sees"},
		},
		{
			name: "a containerd node that is no container here", route: "local", inspected: true,
			rules: func(t *testing.T) []rule {
				return []rule{
					{Tool: "kubectl", Contains: []string{"get", "nodes", "json"}, Stdout: kindNodes(t, kindNode, "containerd://2.2.0")},
					{Tool: "docker", Contains: []string{"inspect", "--type", "container", kindNode}, Stderr: "Error: No such container: " + kindNode + "\n", Exit: 1},
				}
			},
		},
		{
			name: "a container of another name", route: "local", inspected: true,
			rules: func(t *testing.T) []rule {
				return []rule{
					{Tool: "kubectl", Contains: []string{"get", "nodes", "json"}, Stdout: kindNodes(t, kindNode, "containerd://2.2.0")},
					{Tool: "docker", Contains: []string{"inspect", "--type", "container", kindNode}, Stdout: "/somebody-else\n"},
				}
			},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				r.imageBinaries(imgCommit, runtime.GOARCH)
				r.rules(append(c.rules(t), kindImageRules(f)...)...)
				res := r.image(kindEnv, `image_check; echo "route $_img_route"`, false)
				res.want(t, 0, append([]string{"route " + c.route + "\n"}, c.want...)...)
				if c.route != "kind" && strings.Contains(res.stdout+res.stderr, "kind mode") {
					t.Errorf("the route %s said kind mode:\n%s", c.route, res.stdout+res.stderr)
				}
				inspect := callsOf(res, "docker", "inspect", "--type", "container", kindNode)
				if c.inspected != (len(inspect) == 1) {
					t.Errorf("the node's container was asked for %d times", len(inspect))
				}
				for _, i := range inspect {
					if !slices.Equal(i.Args[:2], []string{"--context", "desktop-linux"}) {
						t.Errorf("the node's container was not asked of the settings' context: %q", i.Args)
					}
				}
				for _, call := range res.calls {
					if slices.Contains(call.Args, "exec") || slices.Contains(call.Args, "save") || slices.Contains(call.Args, "load") {
						t.Errorf("an image check ran %s %q", call.Tool, call.Args)
					}
				}
			})
		}
		t.Run("another context is not asked", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			f := newImgFix(imgCommit)
			r.imageBinaries(imgCommit, runtime.GOARCH)
			r.rules(append(kindMode(t), kindImageRules(f)...)...)
			res := r.image("KMS=local\nDEPLOY_TARGET=k8s\nK8S_CONTEXT=rancher-desktop\n", `image_check; echo "route $_img_route"`, false)
			res.want(t, 0, "route local\n")
			if n := callsOf(res, "kubectl", "get", "nodes", "json"); len(n) != 0 {
				t.Errorf("the nodes were asked for their runtime on another context: %v", n)
			}
		})
	})
}

// TestKindImport: on the route kind the image made on the Docker engine is
// saved there and streamed into the node's containerd by "docker exec -i
// <node> ctr --namespace k8s.io images import -", the bytes the save
// wrote; then the node must list it by name and its target must name the
// configuration sent. Nothing is loaded or pushed. An import that fails, a
// node that does not list the image afterwards and one whose image names
// another configuration fail the run.
func TestKindImport(t *testing.T) {
	name := func(f imgFix) string { return "docker.io/library/" + f.ref() }
	cases := []struct {
		name  string
		rules func(f imgFix, manifest string) []rule
		code  int
		want  []string
	}{
		{
			name:  "imported and checked",
			rules: func(f imgFix, manifest string) []rule { return kindHolds(name(f), manifest, f.hex) },
		},
		{
			name: "an import that fails", code: 1,
			rules: func(f imgFix, manifest string) []rule {
				return append([]rule{{Tool: "docker", Contains: kindExec("images", "import", "-"), Stderr: "ctr: failed to import: unexpected EOF\n", Exit: 1}},
					kindHolds(name(f), manifest, f.hex)...)
			},
			want: []string{"docker --context desktop-linux exec -i desktop-control-plane ctr --namespace k8s.io images import - failed (its message is above)"},
		},
		{
			name: "a node that does not list it after the import", code: 1,
			rules: func(f imgFix, manifest string) []rule {
				return append([]rule{{Tool: "docker", Contains: kindExec("images", "ls", "-q"), Stdout: "docker.io/library/cactusfs:other\n"}},
					kindHolds(name(f), manifest, f.hex)...)
			},
			want: []string{"after the import the node desktop-control-plane does not list " + "docker.io/library/cactusfs:0.7.0-412-", "the pods would not start (ErrImageNeverPull)"},
		},
		{
			name: "a node whose image names another configuration", code: 1,
			rules: func(f imgFix, manifest string) []rule { return kindHolds(name(f), manifest, randomHex()) },
			want:  []string{"names the configuration", "not the one sent, "},
		},
		{
			name: "a listing that cannot be asked", code: 1,
			rules: func(f imgFix, manifest string) []rule {
				return append([]rule{{Tool: "docker", Contains: kindExec("images", "ls", "-q"), After: []string{"images", "import"}, Stderr: "Error response from daemon: container is not running\n", Exit: 1}},
					kindHolds(name(f), manifest, f.hex)...)
			},
			want: []string{"asking the node desktop-control-plane for docker.io/library/cactusfs:0.7.0-412-"},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				manifest := randomHex()
				r.imageBinaries(imgCommit, runtime.GOARCH)
				rules := append(kindMode(t), c.rules(f, manifest)...)
				r.rules(append(rules, kindImageRules(f)...)...)
				res := r.image(kindEnv, resolveAndDeliver, false)
				res.want(t, c.code, append([]string{"run: docker --context desktop-linux save " + f.ref() + " | docker --context desktop-linux exec -i desktop-control-plane ctr --namespace k8s.io images import -"}, c.want...)...)
				imp := callsOf(res, "docker", "images", "import")
				want := []string{"--context", "desktop-linux", "exec", "-i", kindNode, "ctr", "--namespace", "k8s.io", "images", "import", "-"}
				if len(imp) != 1 || !slices.Equal(imp[0].Args, want) || imp[0].Stdin != f.tarSum() {
					t.Fatalf("the image was not imported once, with the saved tar, by %q: %v", want, imp)
				}
				for _, call := range res.calls {
					if call.Tool == "docker" && (slices.Contains(call.Args, "load") || slices.Contains(call.Args, "push")) {
						t.Errorf("the image was moved by %q", call.Args)
					}
					if call.Tool == "docker" && slices.Contains(call.Args, "exec") && !slices.Equal(call.Args[:2], []string{"--context", "desktop-linux"}) {
						t.Errorf("the node was reached through another context: %q", call.Args)
					}
				}
				if c.code != 0 {
					if strings.Contains(res.stdout, "resolved ") || strings.Contains(res.stderr, "the node desktop-control-plane holds") {
						t.Errorf("a failed import was taken:\n%s\n%s", res.stdout, res.stderr)
					}
					if c.name == "an import that fails" && callIndex(res, "docker", "images", "ls") >= 0 {
						t.Error("the node was asked for the image after an import that failed")
					}
					return
				}
				res.want(t, 0, "image: the node desktop-control-plane holds "+f.ref()+", configuration "+f.hex,
					"image: "+f.ref()+" imported into the containerd of the node desktop-control-plane", "resolved "+f.ref()+" ")
				order := []int{
					callIndex(res, "docker", "save", f.ref()),
					callIndex(res, "docker", "images", "import", "-"),
					callIndex(res, "docker", "images", "ls", "-q", "name=="+name(f)),
					callIndex(res, "docker", "content", "get", "sha256:"+manifest),
				}
				if slices.Contains(order, -1) || !slices.IsSorted(order) {
					t.Fatalf("save, import, listing and the manifest's read are at %v", order)
				}
				for _, call := range callsOf(res, "docker", "exec") {
					if i := slices.Index(call.Args, "-i"); i >= 0 && !slices.Contains(call.Args, "import") {
						t.Errorf("a query of the node was given -i: %q", call.Args)
					}
				}
			})
		}
	})
}

// kindInstallEnv is the env file of an install on Docker Desktop's
// Kubernetes of an image given by its digest.
func kindInstallEnv(f imgFix, wait string) string {
	return "DEPLOY_TARGET=k8s\nK8S_CONTEXT=docker-desktop\nKMS=local\nK8S_S3_ALLOW_FROM=192.0.2.0/24\nIMAGE_SOURCE=image\nWAIT_TIMEOUT=" + wait +
		"\nIMAGE=" + registry + "@sha256:" + f.hex + "\n"
}

// waitingPod is the new pod of a node whose container waits with the
// reason given, as kubectl prints it: not Ready.
func waitingPod(name, image, reason, message string) obj {
	id := strings.TrimSuffix(name, "-0")
	return waitingAs(podOf(name, "uid-new-"+id, id+"-new", image, false, true), "containerStatuses", image, reason, message)
}

// waitingAs is the pod p (podOf) whose container of the kind of statuses
// given (containerStatuses, initContainerStatuses) waits with the reason.
func waitingAs(p obj, field, image, reason, message string) obj {
	return withField(p, field, obj{
		"name": "cactusfs", "image": image, "imageID": "", "ready": false, "restartCount": 0,
		"state": obj{"waiting": obj{"reason": reason, "message": message}},
	})
}

// withStatuses is the pod p (podOf) with the status of its containers.
func withStatuses(p obj, statuses ...any) obj {
	return withField(p, "containerStatuses", statuses...)
}

// withField is the pod p (podOf) with the field of its status given.
func withField(p obj, field string, statuses ...any) obj {
	status, ok := p["status"].(obj)
	if !ok {
		status = obj{}
		p["status"] = status
	}
	status[field] = statuses
	return p
}

// TestKindInstall: an install on Docker Desktop's Kubernetes in kind mode
// imports the image into the node, runs it with imagePullPolicy Never and
// records the tag as "kind <name> <cluster>/<node>", for a purge to
// remove; a tag the node held before is not recorded.
func TestKindInstall(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		for _, known := range []bool{false, true} {
			t.Run(map[bool]string{false: "new", true: "there before"}[known], func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.chart()
				f := newImgFix(imgCommit)
				name := "docker.io/library/" + f.ref()
				extra := append([]rule{kubeServer("https://kubernetes.docker.internal:6443")}, kindMode(t)...)
				if known {
					extra = append(extra, rule{Tool: "docker", Contains: kindExec("images", "ls", "-q", "name=="+name), Stdout: name + "\n"})
				}
				extra = append(extra, kindHolds(name, randomHex(), f.hex)...)
				k := kube{tool: "kubectl", mode: "standalone", f: f, image: f.ref(), devInit: k8sDevInit("standalone"), imported: true, extra: extra}
				r.rules(k.rules(t)...)
				res := r.withEnv(kindInstallEnv(f, "3"), "--install")
				res.want(t, 0, "imagePullPolicy Never", "ready: cactusfs-node-0 runs "+f.ref(),
					"image: the node desktop-control-plane holds "+f.ref())
				insideNamespace(t, r, res.calls, "cactusfs")
				files := tarFiles(t, []byte(read(t, filepath.Join(r.dir, "render.tar"))))
				if !strings.Contains(files["values.yaml"], "  repository: 'cactusfs'\n  tag: '0.7.0-412-"+f.id12()+"'\n  digest: ''\n  pullPolicy: Never\n") {
					t.Errorf("the values do not name the tag imported, pulled never:\n%s", files["values.yaml"])
				}
				ls, imp := callIndex(res, "docker", "images", "ls", "-q", "name=="+name), callIndex(res, "docker", "images", "import")
				if ls < 0 || imp < 0 || ls > imp {
					t.Errorf("the node was asked for the tag at %d, the image imported at %d", ls, imp)
				}
				images := filepath.Join(r.state("k8s-docker-desktop", "cactusfs"), "images")
				if known {
					res.want(t, 0, "image: "+name+" was in the containerd of the node desktop-control-plane before the import: it is not recorded, and a purge leaves it")
					if _, err := os.Stat(images); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("a tag that was there before was recorded: %v", err)
					}
					return
				}
				if got := read(t, images); got != "kind "+name+" "+clusterUID+"/"+kindNode+"\n" {
					t.Errorf("the entry records the images %q", got)
				}
				res.want(t, 0, "image: recorded "+name+" (the containerd of the node desktop-control-plane, cluster "+clusterUID+") in "+images+" before the import, for a purge to remove")
			})
		}
	})
}

// kindEntry writes the STATE_DIR entry of a deployment on Docker
// Desktop's Kubernetes whose images file holds the lines given.
func kindEntry(t *testing.T, r *rig, images string) string {
	t.Helper()
	state := r.state("k8s-docker-desktop", "cactusfs")
	put(t, filepath.Join(state, "target"), "kubectl --context docker-desktop\nengine-id="+clusterUID+"\nkms=local\n")
	put(t, filepath.Join(state, "namespace-uid"), kubeMarker+"\n")
	put(t, filepath.Join(state, "namespace"), "cactusfs\n")
	put(t, filepath.Join(state, "images"), images)
	return state
}

// kindPurgeEnv is the env file of a purge on Docker Desktop's Kubernetes.
const kindPurgeEnv = "DEPLOY_TARGET=k8s\nK8S_CONTEXT=docker-desktop\nKMS=local\n"

// TestKindPurge: a purge on Docker Desktop's Kubernetes in kind mode
// removes, after the namespace, each tag recorded on the node reached now
// through "docker exec <node> ctr --namespace k8s.io images rm" - but one a
// pod of another namespace names (by its name, by the digest of its
// configuration as imageID, or by repo@sha256:<its manifest's digest>), one
// recorded on another cluster's node, and one the node cannot be asked
// about, which are said and left. A tag the node no longer holds leaves the
// record. Nodes that cannot be listed keep every tag, with a warning, and
// the purge goes on.
func TestKindPurge(t *testing.T) {
	const (
		gone       = "docker.io/library/cactusfs:0.7.0-412-0123456789ab"
		byName     = "docker.io/library/cactusfs:0.7.0-412-feedfacefeed"
		byImage    = "docker.io/library/cactusfs:0.7.0-412-abcabcabcabc"
		byManifest = "docker.io/library/cactusfs:0.7.0-412-dededededede"
		absent     = "docker.io/library/cactusfs:0.7.0-412-aaaaaaaaaaaa"
		unasked    = "docker.io/library/cactusfs:0.7.0-412-bbbbbbbbbbbb"
		other      = "docker.io/library/cactusfs:0.7.0-412-0000beefcafe"
	)
	on := clusterUID + "/" + kindNode
	lines := ""
	for _, n := range []string{gone, byName, byImage, byManifest, absent, unasked} {
		lines += "kind " + n + " " + on + "\n"
	}
	lines += "kind " + other + " 00000000-gone/" + kindNode + "\n"
	eachShell(t, func(t *testing.T, r *rig) {
		for _, listed := range []bool{true, false} {
			t.Run(map[bool]string{true: "nodes listed", false: "nodes not listed"}[listed], func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				state := kindEntry(t, r, lines)
				manifests := map[string]string{gone: randomHex(), byName: randomHex(), byImage: randomHex(), byManifest: randomHex()}
				running := randomHex()
				// Other namespaces' pods: one runs the image of byImage
				// under a name of its own, by the digest of its
				// configuration; one runs byManifest's by repo@<manifest>;
				// one names byName.
				pods := kubeList(
					withStatuses(podOf("other-0", "u", "r", "registry.example.test/other:1", true, false),
						obj{"name": "c", "image": "registry.example.test/other:1", "imageID": "sha256:" + running}),
					withStatuses(podOf("pinned-0", "u3", "r", "registry.example.test/pinned:1", true, false),
						obj{"name": "c", "image": "registry.example.test/pinned:1", "imageID": "docker.io/library/cactusfs@sha256:" + manifests[byManifest]}),
					podOf("web-0", "u2", "r", "cactusfs:0.7.0-412-feedfacefeed", true, false))
				extra := []rule{
					{Tool: "kubectl", Contains: []string{"get", "persistentvolumes"}, Stdout: kjsonNoT(kubeList())},
					{Tool: "kubectl", Contains: []string{"get", "pods", "-A"}, Stdout: kjsonNoT(pods)},
				}
				if listed {
					extra = append(extra, kindMode(t)...)
				} else {
					extra = append(extra, rule{Tool: "kubectl", Contains: []string{"get", "nodes", "json"}, Stderr: "Error from server (Forbidden): nodes is forbidden\n", Exit: 1})
				}
				extra = append(extra,
					rule{Tool: "docker", Contains: kindExec("images", "ls", "name=="+absent), Stdout: "REF TYPE DIGEST SIZE PLATFORMS LABELS \n"},
					rule{Tool: "docker", Contains: kindExec("images", "ls", "name=="+unasked), Stderr: "Error response from daemon: container is paused\n", Exit: 1})
				for _, n := range []string{gone, byName, byImage, byManifest} {
					config := randomHex()
					if n == byImage {
						config = running
					}
					extra = append(extra,
						rule{Tool: "docker", Contains: kindExec("images", "ls", "name=="+n), Stdout: ctrList(n, manifests[n])},
						rule{Tool: "docker", Contains: kindExec("content", "get", "sha256:"+manifests[n]), Stdout: ociManifest(manifests[n], config)["blobs/sha256/"+manifests[n]]})
				}
				k := purgeBase(installedKube(kube{tool: "kubectl", mode: "standalone", f: f, image: f.ref()}))
				k.extra = append(extra, k.extra...)
				r.rules(k.rules(t)...)
				res := r.withEnv(kindPurgeEnv, "--uninstall", "--purge", "--purge-confirm=cactusfs")
				if strings.Contains(res.stderr, "deploy: FAIL") {
					t.Errorf("a purge that went on said FAIL:\n%s", res.stderr)
				}
				for _, c := range res.calls {
					if c.Tool == "docker" && slices.Contains(c.Args, "exec") && slices.Contains(c.Args, "-i") {
						t.Errorf("a purge gave the node a standard input: %q", c.Args)
					}
				}
				if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the entry is still there: %v", err)
				}
				rm := callsOf(res, "docker", "images", "rm")
				if !listed {
					res.want(t, 0, "the nodes of the cluster of kubectl --context docker-desktop could not be listed (kubectl's message is above): the images imported into Docker Desktop's kind node are kept",
						"purge: the image "+gone+" was imported into the node of Docker Desktop's Kubernetes '"+on+"' (cluster/node), and the node the settings reach now is none", "purged: cactusfs")
					if len(rm) != 0 {
						t.Errorf("an image was removed with nodes that could not be listed: %v", rm)
					}
					return
				}
				res.want(t, 0, "purge: removed the image "+gone+" it imported into the node desktop-control-plane",
					"purge: the image "+byName+" is kept: a pod or a workload's template still names it",
					"purge: the image "+byImage+" is kept: a pod or a workload's template runs its image, sha256:"+running,
					"purge: the image "+byManifest+" is kept: a pod or a workload's template runs its image, sha256:"+manifests[byManifest],
					"purge: the image "+absent+" it imported is no longer in the node desktop-control-plane",
					"the node desktop-control-plane could not be asked for the image "+unasked+" (its message is above): it is kept",
					"purge: the image "+other+" was imported into the node of Docker Desktop's Kubernetes '00000000-gone/desktop-control-plane' (cluster/node), and the node the settings reach now is "+on+": it is left",
					"purged: cactusfs")
				want := []string{"--context", "default", "exec", kindNode, "ctr", "--namespace", "k8s.io", "images", "rm", gone}
				if len(rm) != 1 || !slices.Equal(rm[0].Args, want) {
					t.Errorf("the images removed are %v, want %q alone", rm, want)
				}
				if i, j := slices.IndexFunc(res.calls, func(c call) bool { return slices.Contains(c.Args, "--raw") }), callIndex(res, "docker", "images", "rm"); i < 0 || j < i {
					t.Errorf("the image was removed at %d, the namespace at %d", j, i)
				}
			})
		}
		// The record a purge acts on: a line removed or gone leaves it, a
		// kept one stays. A purge that fails later (claims that stay) keeps
		// the entry, which shows what is left in it.
		t.Run("the record", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			f := newImgFix(imgCommit)
			record := "kind " + gone + " " + on + "\nkind " + absent + " " + on + "\nkind " + byName + " " + on + "\n"
			manifest := randomHex()
			extra := append([]rule{
				{Tool: "kubectl", Contains: []string{"get", "persistentvolumes"}, Stdout: kjsonNoT(kubeList())},
				{Tool: "kubectl", Contains: []string{"get", "pods", "-A"}, Stdout: kjsonNoT(kubeList(podOf("web-0", "u2", "r", "cactusfs:0.7.0-412-feedfacefeed", true, false)))},
				{Tool: "docker", Contains: kindExec("images", "ls", "name=="+absent), Stdout: "REF TYPE DIGEST SIZE PLATFORMS LABELS \n"},
				{Tool: "docker", Contains: kindExec("images", "ls", "name=="+gone), Stdout: ctrList(gone, manifest)},
				{Tool: "docker", Contains: kindExec("content", "get", "sha256:"+manifest), Stdout: ociManifest(manifest, randomHex())["blobs/sha256/"+manifest]},
			}, kindMode(t)...)
			k := purgeBase(installedKube(kube{tool: "kubectl", mode: "standalone", f: f, image: f.ref()}))
			k.extra = append(extra, k.extra...)
			r.rules(k.rules(t)...)
			// The images alone, as the purge acts on them, with the
			// cluster's ID known, on an entry of the run's TMPDIR.
			res := r.image(kindPurgeEnv, `STATE=$TMPDIR/entry; mkdir "$STATE"; printf '%s' `+shq(record)+` >"$STATE/images"; _engine_id=`+clusterUID+
				`; k8s_images_remove; echo "left:"; cat "$STATE/images"; rm -rf "$STATE"`, false)
			res.want(t, 0, "left:\nkind "+byName+" "+on+"\n", "purge: removed the image "+gone, "is no longer in the node", byName+" is kept")
		})
	})
}

// TestKindRecordedFirst: a tag new to the node is recorded before it is
// imported, on the routes kind and import (k3s over ssh): an import that
// succeeds and whose check then fails leaves the line, for a purge to
// remove; an import that fails takes it out again.
func TestKindRecordedFirst(t *testing.T) {
	type route struct {
		name, tool, env, target, line string
		importFails, checkFails       func(f imgFix) rule
		extra                         func(t *testing.T, f imgFix) []rule
	}
	routes := []route{
		{
			name: "kind", tool: "kubectl", target: "k8s-docker-desktop",
			env:  "DEPLOY_TARGET=k8s\nK8S_CONTEXT=docker-desktop\nKMS=local\nK8S_S3_ALLOW_FROM=192.0.2.0/24\nIMAGE_SOURCE=image\nWAIT_TIMEOUT=3\n",
			line: "kind docker.io/library/%s " + clusterUID + "/" + kindNode,
			importFails: func(imgFix) rule {
				return rule{Tool: "docker", Contains: kindExec("images", "import", "-"), Stderr: "ctr: unexpected EOF\n", Exit: 1}
			},
			checkFails: func(imgFix) rule {
				return rule{Tool: "docker", Contains: kindExec("images", "ls", "-q"), Stdout: ""}
			},
			extra: func(t *testing.T, f imgFix) []rule {
				extra := append([]rule{kubeServer("https://kubernetes.docker.internal:6443")}, kindMode(t)...)
				return append(extra, kindHolds("docker.io/library/"+f.ref(), randomHex(), f.hex)...)
			},
		},
		{
			name: "import", tool: "ssh", target: "ssh-admin_k3s.example.test",
			env:  "DEPLOY_TARGET=k3s\nK3S_SSH=admin@k3s.example.test\nKMS=local\nK8S_S3_ALLOW_FROM=192.0.2.0/24\nIMAGE_SOURCE=image\nWAIT_TIMEOUT=3\n",
			line: "docker.io/library/%s",
			importFails: func(imgFix) rule {
				return rule{Tool: "ssh", Contains: []string{"images", "import", "-"}, Stderr: "ctr: unexpected EOF\n", Exit: 1}
			},
			checkFails: func(imgFix) rule { return crictlOf("ssh", randomHex()) },
			extra:      func(*testing.T, imgFix) []rule { return nil },
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, rt := range routes {
			for _, importFails := range []bool{true, false} {
				t.Run(rt.name+map[bool]string{true: ", the import fails", false: ", its check fails"}[importFails], func(t *testing.T) {
					t.Parallel()
					r := r.fresh(t)
					r.chart()
					f := newImgFix(imgCommit)
					fault := rt.checkFails(f)
					if importFails {
						fault = rt.importFails(f)
					}
					k := kube{tool: rt.tool, mode: "standalone", f: f, image: f.ref(), devInit: k8sDevInit("standalone"), imported: true}
					k.extra = append([]rule{fault}, rt.extra(t, f)...)
					r.rules(k.rules(t)...)
					res := r.withEnv(rt.env+"IMAGE="+registry+"@sha256:"+f.hex+"\n", "--install")
					res.want(t, 1, "before the import, for a purge to remove")
					images := filepath.Join(r.state(rt.target, "cactusfs"), "images")
					line := strings.Replace(rt.line, "%s", f.ref(), 1) + "\n"
					if importFails {
						res.want(t, 1, "image: the import failed; '"+strings.TrimSuffix(line, "\n")+"' leaves")
						if _, err := os.Stat(images); !errors.Is(err, os.ErrNotExist) {
							t.Errorf("an import that failed left a record: %v", err)
						}
						return
					}
					if got := read(t, images); got != line {
						t.Errorf("after a check that failed the record is %q, want %q", got, line)
					}
				})
			}
		}
	})
}

// failFast is a case of TestImageNeverPullFailsFast: the route, and what
// "get pod" answers of the node's pod, one answer after another (the last
// for every question after).
type failFast struct {
	name    string
	env     func(f imgFix) string
	route   func(t *testing.T, f imgFix) (image string, extra []rule)
	pods    func(f imgFix, image string) []obj
	upgrade bool
	code    int
	want    func(f imgFix) []string
	// unwanted are texts the run must not say.
	unwanted []string
}

// neverPull is the kubelet's message of ErrImageNeverPull for the image.
func neverPull(image string) string {
	return `Container image "` + image + `" is not present with pull policy of Never`
}

// kindRoute is the route kind of an image given (f.ref() on the node).
func kindRoute(t *testing.T, f imgFix) (string, []rule) {
	extra := append([]rule{kubeServer("https://kubernetes.docker.internal:6443")}, kindMode(t)...)
	return f.ref(), append(extra, kindHolds("docker.io/library/"+f.ref(), randomHex(), f.hex)...)
}

// nodesRoute is the route nodes: IMAGE in K8S_REGISTRY, pulled by its
// digest.
func nodesRoute(_ *testing.T, f imgFix) (string, []rule) { return registry + "@sha256:" + f.hex, nil }

// nodesEnv is kubeEnv with a WAIT_TIMEOUT of a minute.
func nodesEnv(f imgFix) string {
	return strings.Replace(kubeEnv(f), "WAIT_TIMEOUT=3", "WAIT_TIMEOUT=60", 1)
}

// readyPod is the new pod of the standalone node, Ready on the image.
func readyPod(image string) obj {
	return podOf("cactusfs-node-0-0", "uid-new-cactusfs-node-0", "cactusfs-node-0-new", image, true, true)
}

// TestImageNeverPullFailsFast: a new pod of the deployment, on the image of
// the run, whose container (or init container) waits with
// ErrImageNeverPull or InvalidImageName - which cannot clear by itself
// here - fails the wait at once, naming the image, the reason and the
// route, on every route and in an upgrade, rather than waiting out
// WAIT_TIMEOUT. A wait that can clear (ImagePullBackOff, CrashLoopBackOff),
// the old pod of an upgrade, and a pod on another image are waited for.
func TestImageNeverPullFailsFast(t *testing.T) {
	never := func(_ imgFix, image string) []obj {
		return []obj{waitingPod("cactusfs-node-0-0", image, "ErrImageNeverPull", neverPull(image))}
	}
	cases := []failFast{
		{
			name: "the route kind", code: 1, route: kindRoute, pods: never,
			env: func(f imgFix) string { return kindInstallEnv(f, "600") },
			want: func(f imgFix) []string {
				return []string{
					"the pod cactusfs-node-0-0 cannot start: its container waits with ErrImageNeverPull (Container image \"" + f.ref() + "\" is not present with pull policy of Never), which cannot clear by itself here - the image was delivered, and checked where the route allows, before the apply, and the pod's reference does not change - so the run does not wait WAIT_TIMEOUT=600 s for it",
					"The image is " + f.ref() + "; the image went to the cluster by the route kind (saved there and imported into the containerd of the cluster's one node, the container desktop-control-plane",
				}
			},
		},
		{
			name: "an init container alone, the route kind", code: 1, route: kindRoute,
			env: func(f imgFix) string { return kindInstallEnv(f, "600") },
			pods: func(_ imgFix, image string) []obj {
				return []obj{waitingAs(podOf("cactusfs-node-0-0", "uid-new-cactusfs-node-0", "cactusfs-node-0-new", image, false, true), "initContainerStatuses", image, "ErrImageNeverPull", neverPull(image))}
			},
			want: func(imgFix) []string {
				return []string{"the pod cactusfs-node-0-0 cannot start: its container waits with ErrImageNeverPull"}
			},
		},
		{
			name: "the route local", code: 1, pods: never,
			env: func(f imgFix) string { return kindInstallEnv(f, "600") },
			route: func(t *testing.T, f imgFix) (string, []rule) {
				return f.ref(), []rule{
					kubeServer("https://kubernetes.docker.internal:6443"),
					{Tool: "kubectl", Contains: []string{"get", "nodes", "json"}, Stdout: kindNodes(t, "docker-desktop", "docker://28.5.1")},
				}
			},
			want: func(imgFix) []string {
				return []string{
					"its container waits with ErrImageNeverPull", "the route local (on the Docker engine here, whose images the cluster's one node sees)",
					"the pods run it with imagePullPolicy Never and the container runtime of the node it was placed on does not hold it",
				}
			},
		},
		{
			name: "the route nodes, a name the kubelet refuses", code: 1, route: nodesRoute,
			env: func(f imgFix) string { return strings.Replace(kubeEnv(f), "WAIT_TIMEOUT=3", "WAIT_TIMEOUT=600", 1) },
			pods: func(_ imgFix, image string) []obj {
				return []obj{waitingPod("cactusfs-node-0-0", image, "InvalidImageName", `Failed to apply default image tag "`+image+`": couldn't parse image name`)}
			},
			want: func(imgFix) []string {
				return []string{"its container waits with InvalidImageName", "the kubelet does not take the reference", "the route nodes (checked here, then pulled by the nodes by its digest)"}
			},
		},
		{
			name: "an upgrade", code: 1, upgrade: true, route: kindRoute, pods: never,
			env: func(f imgFix) string { return kindInstallEnv(f, "600") },
			want: func(imgFix) []string {
				return []string{
					"upgrade stopped: the pod cactusfs-node-0-0 cannot start: its container waits with ErrImageNeverPull",
					"; the nodes after it were not moved. To go back, install the image it ran: IMAGE_SOURCE=image IMAGE=" + registry + "@sha256:" + strings.Repeat("0", 64),
				}
			},
		},
		{
			name: "pull back-off, then crash loop, then Ready", route: nodesRoute, env: nodesEnv,
			pods: func(_ imgFix, image string) []obj {
				return []obj{
					waitingPod("cactusfs-node-0-0", image, "ImagePullBackOff", `Back-off pulling image "`+image+`"`),
					waitingPod("cactusfs-node-0-0", image, "CrashLoopBackOff", "back-off 10s restarting failed container=cactusfs"),
					readyPod(image),
				}
			},
			want:     func(imgFix) []string { return []string{"ready: cactusfs-node-0 runs "} },
			unwanted: []string{"cannot start"},
		},
		{
			name: "a pod on another image that cannot start, then Ready", route: kindRoute, env: func(f imgFix) string { return kindInstallEnv(f, "60") },
			pods: func(_ imgFix, image string) []obj {
				return []obj{waitingPod("cactusfs-node-0-0", "cactusfs:0.6.0-1-0123456789ab", "ErrImageNeverPull", neverPull("cactusfs:0.6.0-1-0123456789ab")), readyPod(image)}
			},
			want:     func(f imgFix) []string { return []string{"ready: cactusfs-node-0 runs " + f.ref()} },
			unwanted: []string{"cannot start"},
		},
		{
			// The old pod, on its old UID - here already of the new
			// image, so that only its UID tells it apart - waits with
			// ErrImageNeverPull while it goes; the new one is Ready.
			name: "an upgrade's old pod that cannot start", upgrade: true, route: kindRoute, env: func(f imgFix) string { return kindInstallEnv(f, "60") },
			pods: func(_ imgFix, image string) []obj {
				old := waitingAs(podOf("cactusfs-node-0-0", "uid-old-cactusfs-node-0", "cactusfs-node-0-old", image, false, true), "containerStatuses", image, "ErrImageNeverPull", neverPull(image))
				return []obj{old, old, readyPod(image)}
			},
			want:     func(f imgFix) []string { return []string{"upgrade: cactusfs-node-0 is Ready on " + f.ref()} },
			unwanted: []string{"cannot start"},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.chart()
				f := newImgFix(imgCommit)
				image, extra := c.route(t, f)
				// The answers of "get pod", one after another: the n-th
				// once n-1 were asked; the first until then.
				var answers []rule
				pods := c.pods(f, image)
				words := []string{"get", "pod", "cactusfs-node-0-0"}
				for i := len(pods) - 1; i >= 0; i-- {
					answers = append(answers, rule{Tool: "kubectl", Contains: words, Seen: i, Stdout: kjson(t, pods[i])})
				}
				k := kube{tool: "kubectl", mode: "standalone", f: f, image: image, devInit: k8sDevInit("standalone"), imported: image == f.ref()}
				if c.upgrade {
					values := kubeEntry(t, r, "standalone", clusterUID)
					dd := r.state("k8s-docker-desktop", "cactusfs")
					if err := os.MkdirAll(filepath.Dir(dd), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(r.state("k8s-prod", "cactusfs"), dd); err != nil {
						t.Fatal(err)
					}
					put(t, filepath.Join(dd, "target"), "kubectl --context docker-desktop\nengine-id="+clusterUID+"\nkms=local\n")
					k = installedKube(k)
					k.secrets = values
					if len(pods) == 1 {
						// The pod before the upgrade is the old one, Ready;
						// after its deletion the new one cannot start.
						answers[0].After = []string{"delete", "pod", "cactusfs-node-0-0"}
					}
				}
				k.extra = append(answers, extra...)
				r.rules(k.rules(t)...)
				start := time.Now()
				res := r.withEnv(c.env(f), "--install")
				if took := time.Since(start); c.code != 0 && took > 120*time.Second {
					t.Errorf("the run took %v: it waited for a pod that cannot start", took)
				}
				res.want(t, c.code, c.want(f)...)
				for _, u := range c.unwanted {
					if strings.Contains(res.stdout+res.stderr, u) {
						t.Errorf("the run said %q:\n%s", u, res.stderr)
					}
				}
				if strings.Contains(res.stderr, "within WAIT_TIMEOUT") {
					t.Errorf("the run waited out WAIT_TIMEOUT:\n%s", res.stderr)
				}
				asked := 0
				for _, call := range res.calls {
					if args, ok := kubectlArgs(call); ok && len(args) > 2 && slices.Equal(args[:3], words) {
						asked++
					}
				}
				if c.code == 0 && asked < len(pods) {
					t.Errorf("the pod was asked for %d times: its %d states were not all seen", asked, len(pods))
				}
				if c.upgrade && len(kubeDeletes(res.calls)) != 1 {
					t.Errorf("the upgrade deleted %v, want the one pod", kubeDeletes(res.calls))
				}
			})
		}
	})
}

// TestKindDetectRefused: what the detection of kind mode does not take.
// Two nodes of a containerd runtime are Docker Desktop's kind mode with
// several nodes: the route stays local, and the one-node routes are refused
// for K8S_REGISTRY, saying so. Nodes that cannot be listed fail the check.
// A node whose name is not one keeps the route local, and the Docker engine
// is not asked for it.
func TestKindDetectRefused(t *testing.T) {
	twoNodes := func(t *testing.T) string {
		return kjson(t, kubeList(
			obj{"metadata": obj{"name": kindNode}, "status": obj{"nodeInfo": obj{"containerRuntimeVersion": "containerd://2.2.0"}}},
			obj{"metadata": obj{"name": "desktop-worker"}, "status": obj{"nodeInfo": obj{"containerRuntimeVersion": "containerd://2.2.0"}}}))
	}
	cases := []struct {
		name  string
		rules func(t *testing.T) []rule
		nodes int
		code  int
		want  []string
	}{
		{
			name: "two containerd nodes", nodes: 2, code: 2,
			rules: func(t *testing.T) []rule {
				return []rule{{Tool: "kubectl", Contains: []string{"get", "nodes", "json"}, Stdout: twoNodes(t)}}
			},
			want: []string{"is Docker Desktop's Kubernetes in kind mode (its 2 nodes each run a containerd of their own, containerd://...), with more than one node", "set K8S_REGISTRY"},
		},
		{
			name: "nodes that cannot be listed", nodes: 1, code: 1,
			rules: func(*testing.T) []rule {
				return []rule{{Tool: "kubectl", Contains: []string{"get", "nodes", "json"}, Stderr: "Error from server (Forbidden): nodes is forbidden\n", Exit: 1}}
			},
			want: []string{"listing the nodes of the cluster of kubectl --context docker-desktop failed (kubectl's message is above)"},
		},
		{
			name: "a node whose name is not one", nodes: 1,
			rules: func(t *testing.T) []rule {
				return []rule{
					{Tool: "kubectl", Contains: []string{"get", "nodes", "json"}, Stdout: kindNodes(t, "Desktop_Node;x", "containerd://2.2.0")},
					{Tool: "docker", Contains: []string{"inspect", "--type", "container"}, Stdout: "/Desktop_Node;x\n"},
				}
			},
			want: []string{"route local\n"},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				r.imageBinaries(imgCommit, runtime.GOARCH)
				arches := []string{runtime.GOARCH}
				if c.nodes == 2 {
					arches = append(arches, runtime.GOARCH)
				}
				rules := append(c.rules(t), kubeServer("https://kubernetes.docker.internal:6443"))
				rules = append(rules, gitRules("")...)
				rules = append(rules, nodeRules("kubectl", arches...)...)
				r.rules(append(rules, imgEngine("docker", runtime.GOARCH, runtime.GOARCH, f)...)...)
				res := r.image(kindEnv, `image_check; echo "route $_img_route"`, false)
				res.want(t, c.code, c.want...)
				if strings.Contains(res.stdout+res.stderr, "runs in kind mode") {
					t.Errorf("kind mode was taken:\n%s", res.stderr)
				}
				if c.nodes == 2 || strings.Contains(c.name, "name") {
					if n := callsOf(res, "docker", "inspect", "--type", "container"); len(n) != 0 {
						t.Errorf("the Docker engine was asked for a node's container: %v", n)
					}
				}
				if b := builds(res); len(b) != 0 {
					t.Errorf("built: %v", b)
				}
			})
		}
	})
}
