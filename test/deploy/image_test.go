//go:build deployscript

package deploy_test

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The image of a deployment (PLAN 10.4, deploy/lib/image.sh): made of the
// binaries of make image-binaries, of this tree, or given; for the target's
// platform; looked up by its ID and tagged by it; checked before it is
// shipped; and taken to the target by save and load, an import into k3s, or
// a push to K8S_REGISTRY.

// imgCommit is the commit the fixtures are of.
const imgCommit = "0123456789ab"

// workTag is the tag image_build builds under for the architecture, for
// the deployment cactusfs.
func workTag(arch string) string { return "cactusfs:deploy-cactusfs-build-" + arch }

// registry is the K8S_REGISTRY of the tests.
const registry = "registry.example.test/cactusfs"

// imgConfig is the configuration image_fields reads of an image of the
// Dockerfile: user, entrypoint, command, environment, working directory,
// labels, ports and volumes, as JSON.
const imgConfig = `"65532:65532" ["/cactusfs"] ["server"] ["PATH=/usr/local/bin:/usr/bin"] "" {"org.opencontainers.image.title":"cactusfs"} {"9000/tcp":{},"9001/tcp":{}} null`

// imgFix is an image the fake engines say they built or hold: its ID - in
// Docker's image store and in Podman, the digest of its configuration - the
// commit of its revision label, its layers, and the tar stream "save" writes
// of it.
type imgFix struct {
	hex    string
	commit string
	layers string
	tar    string
}

func randomHex() string {
	sum := sha256.Sum256([]byte(rand.Text()))
	return hex.EncodeToString(sum[:])
}

func newImgFix(commit string) imgFix {
	id := randomHex()
	layers := "sha256:" + randomHex() + " sha256:" + randomHex() + " "
	return imgFix{
		hex:    id,
		commit: commit,
		layers: layers,
		tar:    savedTar("blobs/sha256/"+id, layers),
	}
}

// savedTar is a tar stream as "docker save" writes one: its manifest.json
// names the configuration's file (blobs/sha256/<hex> in an OCI layout,
// <hex>.json in Docker's own), whose rootfs.diff_ids are the layers given
// (separated by spaces).
func savedTar(config, layers string) string {
	return savedTarOf(tarEntry{config, layers})
}

// tarEntry is an image of a saved tar: the name of its configuration's file
// and its layers.
type tarEntry struct{ config, layers string }

// savedTarOf is a saved tar of the images given, as Docker's containerd
// store saves several platforms of one image: one entry each in
// manifest.json (with a nested LayerSources object, as Docker writes), and
// each configuration's file. An entry whose config is "" names none.
func savedTarOf(entries ...tarEntry) string {
	files := map[string]string{}
	var manifest []string
	for _, e := range entries {
		var ids []string
		for _, l := range strings.Fields(e.layers) {
			ids = append(ids, `"`+l+`"`)
		}
		config := ""
		if e.config != "" {
			config = `"Config":"` + e.config + `",`
			files[e.config] = `{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[` + strings.Join(ids, ",") + `]}}`
		}
		manifest = append(manifest, `{`+config+`"RepoTags":["cactusfs:x"],"Layers":["blobs/sha256/layer"],"LayerSources":{"sha256:layer":{"mediaType":"application/vnd.oci.image.layer.v1.tar","size":1,"digest":"sha256:layer"}}}`)
	}
	files["manifest.json"] = "[" + strings.Join(manifest, ",") + "]"
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		data := files[name]
		if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
			panic(err)
		}
		if _, err := w.Write([]byte(data)); err != nil {
			panic(err)
		}
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return b.String()
}

// id12 is the first 12 hex digits of the image's ID.
func (f imgFix) id12() string { return f.hex[:12] }

// ref is the tag the image is given: <version>-<build>-<12 hex of its ID>.
func (f imgFix) ref() string { return "cactusfs:0.7.0-412-" + f.id12() }

// tarSum is the hash of the tar stream, as a fake records what it read.
func (f imgFix) tarSum() string {
	sum := sha256.Sum256([]byte(f.tar))
	return hex.EncodeToString(sum[:])
}

// fields is what "image inspect" answers of the image for linux/<arch>,
// with the registry digests given.
func (f imgFix) fields(arch string, digests ...string) string {
	return fieldsLine(f.hex, "linux/"+arch, f.commit, "0.7.0-412", f.layers, digests...)
}

// fieldsLine is an answer of "image inspect" in the format of image_fields:
// ID, RepoDigests, platform, revision, version, layers and the
// configuration (imgConfig).
func fieldsLine(id, plat, rev, ver, layers string, digests ...string) string {
	return fieldsConfig(id, plat, rev, ver, layers, imgConfig, digests...)
}

// fieldsConfig is fieldsLine with the configuration given.
func fieldsConfig(id, plat, rev, ver, layers, config string, digests ...string) string {
	var d string
	for _, x := range digests {
		d += x + " "
	}
	return "sha256:" + id + "|" + d + "|" + plat + "|" + rev + "|" + ver + "|" + layers + "|" + config + "\n"
}

// versionLine is what the binary of the image prints for "version".
func versionLine(commit, arch string) string {
	c := ""
	if commit != "" {
		c = "commit " + commit + ", "
	}
	return "cactusfs 0.7.0 build 412 (" + c + "go1.27.4 linux/" + arch + ", image)\n"
}

// engineInfo is what an engine of the architecture answers to "info".
func engineInfo(tool, arch string) string {
	if tool == "podman" {
		return "linux/" + arch + "\n"
	}
	if arch == "arm64" {
		return "linux/aarch64\n"
	}
	return "linux/x86_64\n"
}

// built is a build's answer: its ID file (the configuration's digest).
func built(tool, arch string, f imgFix) rule {
	return rule{Tool: tool, Contains: []string{"build", "linux/" + arch}, Files: map[string]string{"--iidfile": "sha256:" + f.hex + "\n"}}
}

// crictlOf is crictl inspecti's answer (-q -o json) of an image whose
// configuration digest is the one given, by the tool that reaches it.
func crictlOf(tool, config string) rule {
	return rule{Tool: tool, Contains: []string{"crictl", "inspecti"}, Stdout: "{\n  \"status\": {\n    \"id\": \"sha256:" + config + "\",\n    \"repoTags\": [\n      \"docker.io/library/cactusfs:x\"\n    ],\n    \"uid\": null\n  }\n}\n"}
}

// imgEngine is the answers of an engine of the architecture engineArch here
// (a local Docker context, a Podman whose service is here) on which the
// image f, for imageArch, is made: its info, the ID file of a build, the
// inspect of the image by its build tag and by its tag, its version line
// and its save - and of a k3s that holds it after an import.
func imgEngine(tool, engineArch, imageArch string, f imgFix) []rule {
	return []rule{
		{Tool: tool, Contains: []string{"context", "inspect"}, Stdout: "unix:///var/run/docker.sock\n"},
		{Tool: tool, Contains: []string{"{{.Host.ServiceIsRemote}}"}, Stdout: "false\n"},
		{Tool: tool, Contains: []string{"info"}, Stdout: engineInfo(tool, engineArch)},
		built(tool, imageArch, f),
		{Tool: tool, Contains: []string{"inspect", workTag(imageArch)}, Stdout: f.fields(imageArch)},
		{Tool: tool, Contains: []string{"inspect", f.hex}, Stdout: f.fields(imageArch)},
		{Tool: tool, Contains: []string{"inspect", f.ref()}, Stdout: f.fields(imageArch)},
		{Tool: tool, Contains: []string{"--rm", "version"}, Stdout: versionLine(f.commit, imageArch)},
		{Tool: tool, Contains: []string{"save"}, Stdout: f.tar},
		crictlOf("ssh", f.hex),
		crictlOf("k3s", f.hex),
	}
}

// engineHere is the answers of the engine here - the Docker CLI through a
// context whose endpoint is a local socket, or a Podman whose service is on
// this machine - of this machine's architecture, as the install of a Docker
// target asks it before anything changes (image_check): its endpoint and
// its platform. Its "info" is asked by the platform's format only, so that
// an engine's ID (engineRules) is still answered by a rule of its own.
func engineHere(tool string) []rule {
	format := "{{.OSType}}/{{.Architecture}}"
	if tool == "podman" {
		format = "{{.Host.OS}}/{{.Host.Arch}}"
	}
	return []rule{
		{Tool: tool, Contains: []string{"context", "inspect"}, Stdout: "unix:///var/run/docker.sock\n"},
		{Tool: tool, Contains: []string{"{{.Host.ServiceIsRemote}}"}, Stdout: "false\n"},
		{Tool: tool, Contains: []string{"info", format}, Stdout: engineInfo(tool, runtime.GOARCH)},
	}
}

// noSuchImage is what the engine says of an image it does not hold.
func noSuchImage(tool, ref string) string {
	if tool == "podman" {
		return "Error: " + ref + ": image not known\n"
	}
	return "Error response from daemon: No such image: " + ref + "\n"
}

// madeHere is engineHere on which the install's image f is built, from the
// binaries or the tree, for this machine's architecture: no build tag left
// by an earlier run, the ID file of the build, the image by its build tag
// (once built), its ID and its tag, and the version line it prints.
func madeHere(tool string, f imgFix) []rule {
	arch := runtime.GOARCH
	return append(engineHere(tool),
		rule{Tool: tool, Contains: []string{"inspect", workTag(arch)}, After: []string{"build", "linux/" + arch}, Stdout: f.fields(arch)},
		rule{Tool: tool, Contains: []string{"inspect", workTag(arch)}, Stderr: noSuchImage(tool, workTag(arch)), Exit: 1},
		built(tool, arch, f),
		rule{Tool: tool, Contains: []string{"inspect", f.hex}, Stdout: f.fields(arch)},
		rule{Tool: tool, Contains: []string{"inspect", f.ref()}, Stdout: f.fields(arch)},
		rule{Tool: tool, Contains: []string{"--rm", "--network", "none", f.hex, "version"}, Stdout: versionLine(f.commit, arch)})
}

// givenHere is engineHere holding the image f, for this machine's
// architecture, under each reference given (IMAGE_SOURCE=image), with the
// registry digest given (none: "").
func givenHere(tool string, f imgFix, digest string, refs ...string) []rule {
	arch := runtime.GOARCH
	var digests []string
	if digest != "" {
		digests = append(digests, digest)
	}
	rules := engineHere(tool)
	for _, ref := range refs {
		rules = append(rules, rule{Tool: tool, Contains: []string{"inspect", ref}, Stdout: f.fields(arch, digests...)})
	}
	return append(rules,
		rule{Tool: tool, Contains: []string{"inspect", f.hex}, Stdout: f.fields(arch, digests...)},
		rule{Tool: tool, Contains: []string{"--rm", "--network", "none", f.hex, "version"}, Stdout: versionLine(f.commit, arch)})
}

// isVersionCheck is true for the run of an image's version line
// (image_version_check): a container removed when it ends, with no
// network, that prints "version" - not a node.
func isVersionCheck(args []string) bool {
	return len(args) > 0 && args[len(args)-1] == "version" &&
		slices.Contains(args, "--rm") && containsRun(args, []string{"--network", "none"})
}

// pushed is the answer of the inspect of the image pushed to the registry:
// the image, with the registry's digest.
func pushed(f imgFix) rule {
	return rule{Tool: "docker", Contains: []string{"inspect", registry + ":0.7.0-412-" + f.id12()}, Stdout: f.fields("amd64", "other.example.test/x@sha256:"+strings.Repeat("0", 64), registry+"@sha256:"+f.hex)}
}

// kubeServer is kubectl's answer of the API server of the context.
func kubeServer(url string) rule {
	return rule{Tool: "kubectl", Contains: []string{"config", "view"}, Stdout: url + "\n"}
}

// nodeRules are the answers of a cluster's kubectl (by the tool given:
// kubectl, k3s, or ssh to a k3s server) with the nodes given, by their
// architecture.
func nodeRules(tool string, arches ...string) []rule {
	var all string
	count := map[string]string{}
	for i, a := range arches {
		name := "node/n" + string(rune('a'+i)) + "\n"
		all += name
		count[a] += name
	}
	var rules []rule
	for _, a := range []string{"amd64", "arm64"} {
		rules = append(rules, rule{Tool: tool, Contains: []string{"nodes", "kubernetes.io/arch=" + a}, Stdout: count[a]})
	}
	return append(rules, rule{Tool: tool, Contains: []string{"get", "nodes"}, Stdout: all})
}

// gitRules are the answers of git for a tree at imgCommit, 412 commits,
// with the status given (empty: no changes).
func gitRules(status string) []rule {
	return []rule{
		{Tool: "git", Contains: []string{"rev-list"}, Stdout: "412\n"},
		{Tool: "git", Contains: []string{"rev-parse"}, Stdout: imgCommit + "\n"},
		{Tool: "git", Contains: []string{"status"}, Stdout: status},
	}
}

// imageBinaries writes the folder of make image-binaries into the rig's
// repository: the binaries of the architectures given - executable, with the
// mode of this test binary, as make image-binaries makes them - their
// SHA256SUMS and BUILDINFO of the commit.
func (r *rig) imageBinaries(commit string, arches ...string) string {
	r.t.Helper()
	self, err := os.Executable()
	if err != nil {
		r.t.Fatal(err)
	}
	exe, err := os.Stat(self)
	if err != nil {
		r.t.Fatal(err)
	}
	dir := filepath.Join(r.repo, "dist", "image")
	var sums string
	for _, a := range arches {
		name := "linux-" + a + "/cactusfs"
		data := []byte("a cactusfs binary for " + a + " " + rand.Text())
		if err := os.MkdirAll(filepath.Join(dir, "linux-"+a), 0o700); err != nil {
			r.t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			r.t.Fatal(err)
		}
		if err := os.Chmod(path, exe.Mode().Perm()); err != nil {
			r.t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		sums += hex.EncodeToString(sum[:]) + "  " + name + "\n"
	}
	put(r.t, filepath.Join(dir, "SHA256SUMS"), sums)
	put(r.t, filepath.Join(dir, "BUILDINFO"), "version=0.7.0\nbuild=412\ncommit="+commit+"\npackaging=image\n")
	return dir
}

// image runs shell code as deploy/deploy.sh would at the point where an
// install acts: deploy/lib/*.sh sourced, the env file parsed, resolved and
// validated, the target resolved and checked, the deployment's STATE_DIR
// entry found. The code then calls what PLAN 10.5 and 10.10 will call. The
// run is given a TMPDIR of its own, which must be empty afterwards (a work
// directory is removed however the run ends), and nothing of the rig may
// change but the fakes' own files.
func (r *rig) image(env, code string, dryRun bool, extraEnv ...string) result {
	r.t.Helper()
	if err := os.Remove(r.transcript); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.t.Fatal(err)
	}
	envPath := r.envFile("site.env", env)
	tmp := r.t.TempDir()
	before := tree(r.t, r.dir)
	dry := "0"
	if dryRun {
		dry = "1"
	}
	argv := slices.Clone(r.sh.argv[1:])
	argv = append(argv, "-c", imagePrelude(dry)+code, "sh", r.repo, envPath)
	cmd := exec.CommandContext(r.t.Context(), r.sh.argv[0], argv...)
	cmd.Dir = r.work
	cmd.Env = append([]string{"PATH=" + r.bin, "HOME=" + r.home, "TMPDIR=" + tmp}, extraEnv...)
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
	res.tmp = tmp
	if left, err := os.ReadDir(tmp); err != nil || len(left) != 0 {
		r.t.Errorf("the run (exit %d) left %v in TMPDIR (%v)", res.code, left, err)
	}
	if after := tree(r.t, r.dir); !maps.Equal(before, after) {
		r.t.Errorf("the run (exit %d) changed the rig:\nbefore %v\nafter  %v", res.code, before, after)
	}
	return res
}

// imagePrelude is the shell code rig.image runs before the test's: the
// libraries sourced, the env file ($2) parsed, resolved and validated, the
// target resolved and checked, the STATE_DIR entry found, with DRY_RUN as
// given ("0" or "1") and the repository in $1.
func imagePrelude(dry string) string {
	return "set -euf; LC_ALL=C; export LC_ALL; DRY_RUN=" + dry + "; ANOTHER_TARGET=0; " +
		"REPO_ROOT=$1; DEPLOY_DIR=$1/deploy; " +
		". \"$DEPLOY_DIR/lib/common.sh\"; . \"$DEPLOY_DIR/lib/env.sh\"; . \"$DEPLOY_DIR/lib/secrets.sh\"; " +
		". \"$DEPLOY_DIR/lib/k8s.sh\"; . \"$DEPLOY_DIR/lib/image.sh\"; " +
		"env_parse \"$2\"; env_resolve; env_validate; resolve_target; target_validate; state_check; "
}

// resolveAndDeliver is the code of an install's image: resolved, delivered,
// and the reference the target runs said.
const resolveAndDeliver = `image_resolve; image_deliver; echo "resolved $RESOLVED_IMAGE $RESOLVED_ARCH"`

// argsAfter is the arguments of a call after the engine's own options (a
// Docker context), from the first of the words given.
func argsAfter(c call, first string) []string {
	if i := slices.Index(c.Args, first); i >= 0 {
		return c.Args[i:]
	}
	return nil
}

// valueOf is the argument after the option in the call, or "".
func valueOf(c call, option string) string {
	if i := slices.Index(c.Args, option); i >= 0 && i+1 < len(c.Args) {
		return c.Args[i+1]
	}
	return ""
}

// noPull fails the test when any call pulls an image: "pull" as a command,
// or a build that is not told not to pull.
func noPull(t *testing.T, res result) {
	t.Helper()
	for _, c := range res.calls {
		if slices.Contains(c.Args, "pull") {
			t.Errorf("%s pulled: %q", c.Tool, c.Args)
		}
		if slices.Contains(c.Args, "build") && !slices.Contains(c.Args, "--pull=false") && !slices.Contains(c.Args, "--pull=never") {
			t.Errorf("%s built without being told not to pull: %q", c.Tool, c.Args)
		}
	}
}

// noPullCommand fails the test when an image was pulled.
func noPullCommand(t *testing.T, res result) {
	t.Helper()
	for _, c := range res.calls {
		if slices.Contains(c.Args, "pull") {
			t.Errorf("%s pulled: %q", c.Tool, c.Args)
		}
	}
}

// builds is the build calls of a run.
func builds(res result) []call {
	var out []call
	for _, c := range res.calls {
		if slices.Contains(c.Args, "build") {
			out = append(out, c)
		}
	}
	return out
}

// callIndex is the position of the first call of the tool that holds the
// words, or -1.
func callIndex(res result, tool string, words ...string) int {
	return slices.IndexFunc(res.calls, func(c call) bool { return c.Tool == tool && containsAll(c.Args, words) })
}

// TestImageFromBinaries: IMAGE_SOURCE=binaries checks the folder, removes a
// stale build tag, builds the from-binaries stage for the target's platform
// through the context's own builder, with the folder as the build context
// "binaries" and the ID written to a file, then checks, tags and runs the
// image by that ID; on the engine that runs it, nothing is moved.
func TestImageFromBinaries(t *testing.T) {
	for _, c := range []struct{ name, env, context string }{
		{"the default context", "", "default"},
		{"a context named", "ENGINE_CONTEXT=desktop-linux\n", "desktop-linux"},
	} {
		t.Run(c.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, r *rig) {
				f := newImgFix(imgCommit)
				rel := r.imageBinaries(imgCommit, "amd64", "arm64")
				r.rules(imgEngine("docker", "amd64", "amd64", f)...)
				res := r.image("KMS=local\nDEPLOY_TARGET=dd\n"+c.env, resolveAndDeliver, false, "DOCKER_HOST=unix:///elsewhere")
				res.want(t, 0, "resolved "+f.ref()+" amd64", "it says 'cactusfs 0.7.0 build 412 (commit 0123456789ab", "is on the target's engine")
				b := builds(res)
				if len(b) != 1 {
					t.Fatalf("%d builds, want 1: %v", len(b), res.calls)
				}
				iid := valueOf(b[0], "--iidfile")
				want := []string{
					"--context", c.context, "buildx", "build", "--builder", c.context, "--load", "--platform", "linux/amd64",
					"--target", "from-binaries", "--build-context", "binaries=" + rel,
					"--build-arg", "VERSION=0.7.0", "--build-arg", "BUILD=412", "--build-arg", "COMMIT=" + imgCommit,
					"-t", workTag("amd64"), "--iidfile", iid, "--metadata-file", valueOf(b[0], "--metadata-file"), r.repo,
				}
				if !slices.Equal(b[0].Args, want) || filepath.Base(iid) != "iid" || !strings.HasPrefix(iid, res.tmp+"/cactusfs-deploy.") {
					t.Errorf("the build is\n %q, want\n %q", b[0].Args, want)
				}
				stale := callIndex(res, "docker", "image", "rm", workTag("amd64"))
				build := callIndex(res, "docker", "build")
				if stale < 0 || stale > build {
					t.Errorf("the stale build tag is not removed before the build: %v", res.calls)
				}
				tag := callsOf(res, "docker", "image", "tag")
				if len(tag) != 1 || !slices.Equal(argsAfter(tag[0], "image"), []string{"image", "tag", f.hex, f.ref()}) {
					t.Errorf("the image is not tagged by its ID: %v", tag)
				}
				if rm := callsOf(res, "docker", "image", "rm", workTag("amd64")); len(rm) != 2 {
					t.Errorf("the build's tag is not removed after the tag: %v", rm)
				}
				run := callsOf(res, "docker", "version")
				if len(run) != 1 || run[0].Args[len(run[0].Args)-2] != f.hex ||
					!containsRun(run[0].Args, []string{"--rm", "--pull", "never", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges"}) {
					t.Errorf("the version line is not read of the image's ID in a container without network and privileges: %v", run)
				}
				for _, call := range res.calls {
					if call.Tool == "docker" && call.Env["DOCKER_HOST"] != "" {
						t.Errorf("docker was given DOCKER_HOST=%s", call.Env["DOCKER_HOST"])
					}
					if slices.Contains(call.Args, "save") || slices.Contains(call.Args, "load") || call.Tool == "ssh" {
						t.Errorf("an image on the target's engine was moved: %q", call.Args)
					}
				}
				noPullCommand(t, res)
			})
		})
	}
}

// TestImageBinariesRefused: a folder that is not what make image-binaries
// writes is refused before anything is built; an image that is not what was
// meant fails the run after its build.
func TestImageBinariesRefused(t *testing.T) {
	type change func(t *testing.T, dir string)
	rewrite := func(file, content string) change {
		return func(t *testing.T, dir string) { put(t, filepath.Join(dir, file), content) }
	}
	refused := []struct {
		name   string
		change change
		want   string
	}{
		{"packaged as a binary", rewrite("BUILDINFO", "version=0.7.0\nbuild=412\ncommit="+imgCommit+"\npackaging=binary\n"), "says packaging 'binary', not image"},
		{"no packaging", rewrite("BUILDINFO", "version=0.7.0\nbuild=412\ncommit="+imgCommit+"\n"), "says packaging '', not image"},
		{"a line more", rewrite("BUILDINFO", "version=0.7.0\nbuild=412\ncommit="+imgCommit+"\npackaging=image\nextra=1\n"), "holds more or other than the four lines"},
		{"a carriage return", rewrite("BUILDINFO", "version=0.7.0\r\nbuild=412\ncommit="+imgCommit+"\npackaging=image\n"), "holds more or other than the four lines"},
		{"a commit that is not one", rewrite("BUILDINFO", "version=0.7.0\nbuild=412\ncommit=$(id)\npackaging=image\n"), "which is not hex digits"},
		{"a version that is not one", rewrite("BUILDINFO", "version=0.7.0;x\nbuild=412\ncommit="+imgCommit+"\npackaging=image\n"), "which is not one"},
		{"a build that is not one", rewrite("BUILDINFO", "version=0.7.0\nbuild=4a\ncommit="+imgCommit+"\npackaging=image\n"), "which is not a number"},
		{"a sum that differs", rewrite("linux-amd64/cactusfs", "another binary"), "does not match"},
		{"no sums", func(t *testing.T, dir string) { remove(t, filepath.Join(dir, "SHA256SUMS")) }, "SHA256SUMS: run make image-binaries"},
		{"no buildinfo", func(t *testing.T, dir string) { remove(t, filepath.Join(dir, "BUILDINFO")) }, "BUILDINFO: run make image-binaries"},
		{"not the target's architecture", func(t *testing.T, dir string) { remove(t, filepath.Join(dir, "linux-amd64", "cactusfs")) }, "linux-amd64/cactusfs: run make image-binaries"},
		{"not executable", func(t *testing.T, dir string) {
			if err := os.Chmod(filepath.Join(dir, "linux-amd64", "cactusfs"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "is not executable"},
	}
	failed := []struct {
		name  string
		rules func(f imgFix) []rule
		want  string
	}{
		{"the build fails", func(imgFix) []rule {
			return []rule{{Tool: "docker", Contains: []string{"build"}, Stderr: "no builder\n", Exit: 1}}
		}, "building the image failed"},
		{"the build writes no ID", func(imgFix) []rule {
			return []rule{{Tool: "docker", Contains: []string{"build"}}}
		}, "the build wrote no image ID"},
		{"another ID", func(f imgFix) []rule {
			return []rule{{Tool: "docker", Contains: []string{"inspect", workTag("amd64")}, Stdout: fieldsLine(randomHex(), "linux/amd64", imgCommit, "0.7.0-412", f.layers)}}
		}, "not the image built"},
		{"another revision", func(f imgFix) []rule {
			return []rule{{Tool: "docker", Contains: []string{"inspect", workTag("amd64")}, Stdout: fieldsLine(f.hex, "linux/amd64", "fedcba987654", "0.7.0-412", f.layers)}}
		}, "the revision label of the image built is 'fedcba987654', not the commit meant, '" + imgCommit + "'"},
		{"another version label", func(f imgFix) []rule {
			return []rule{{Tool: "docker", Contains: []string{"inspect", workTag("amd64")}, Stdout: fieldsLine(f.hex, "linux/amd64", imgCommit, "0.6.0-412", f.layers)}}
		}, "the version label of the image built is '0.6.0-412'"},
		{"another platform", func(f imgFix) []rule {
			return []rule{{Tool: "docker", Contains: []string{"inspect", workTag("amd64")}, Stdout: f.fields("arm64")}}
		}, "the image built is for linux/arm64, not linux/amd64"},
		{"an ID that is not one", func(imgFix) []rule {
			return []rule{{Tool: "docker", Contains: []string{"build"}, Files: map[string]string{"--iidfile": "sha256:xyz"}}}
		}, "is not an image ID"},
		{"another version line", func(imgFix) []rule {
			return []rule{{Tool: "docker", Contains: []string{"--rm", "version"}, Stdout: "cactusfs 0.7.0 build 412 (commit " + imgCommit + ", go1.27.4 linux/amd64, binary)\n"}}
		}, "not the build meant"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range refused {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				c.change(t, r.imageBinaries(imgCommit, "amd64", "arm64"))
				r.rules(imgEngine("docker", "amd64", "amd64", f)...)
				res := r.image("KMS=local\nDEPLOY_TARGET=dd\n", resolveAndDeliver, false)
				res.want(t, 2, c.want)
				if b := builds(res); len(b) != 0 {
					t.Fatalf("built after all: %v", b)
				}
			})
		}
		for _, c := range failed {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				r.imageBinaries(imgCommit, "amd64")
				r.rules(append(c.rules(f), imgEngine("docker", "amd64", "amd64", f)...)...)
				res := r.image("KMS=local\nDEPLOY_TARGET=dd\n", resolveAndDeliver, false)
				res.want(t, 1, c.want)
				if strings.Contains(res.stdout, "resolved ") {
					t.Fatalf("an image was resolved after all:\n%s", res.stdout)
				}
			})
		}
	})
}

// TestImageBinariesRemote: binaries of a changed tree or of no known commit,
// and a Dockerfile that is not the binaries' commit's, are refused for a
// target on another machine - unless ALLOW_DIRTY=yes, which warns - and
// pass for one here.
func TestImageBinariesRemote(t *testing.T) {
	targets := map[string]string{
		"docker ce over ssh": "DEPLOY_TARGET=ce\nCE_HOST=deploy@server.example.test\n",
		"k3s over ssh":       "DEPLOY_TARGET=k3s\nK3S_SSH=admin@k3s.example.test\n",
		"a registry":         "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=" + registry + "\n",
		"a build host":       "DEPLOY_TARGET=dd\nBUILD_HOST=builder@build.example.test\n",
	}
	cases := []struct {
		name    string
		commit  string
		extra   string
		git     []rule
		without bool
		code    int
		want    string
	}{
		{name: "clean", commit: imgCommit, git: gitRules("")},
		{name: "changed", commit: imgCommit + "-dirty", git: gitRules(""), code: 2, want: "the binaries of"},
		{name: "no known commit", commit: "", git: gitRules(""), code: 2, want: "is of no known commit"},
		{name: "changed, allowed", commit: imgCommit + "-dirty", extra: "ALLOW_DIRTY=yes\n", git: gitRules(""), want: "warning: ALLOW_DIRTY=yes: the binaries of"},
		{
			name: "the Dockerfile changed", commit: imgCommit,
			git:  append([]rule{{Tool: "git", Contains: []string{"status", "Dockerfile"}, Stdout: " M Dockerfile\n"}}, gitRules("")...),
			code: 2, want: "the Dockerfile of",
		},
		{
			name: "the Dockerfile of another commit", commit: imgCommit,
			git:  append([]rule{{Tool: "git", Contains: []string{"rev-parse"}, Stdout: "fedcba987654\n"}}, gitRules("")...),
			code: 2, want: "is of the commit fedcba987654, and the binaries of " + imgCommit,
		},
		{name: "no git", commit: imgCommit, without: true, code: 2, want: "the Dockerfile of"},
		{name: "the Dockerfile changed, allowed", commit: imgCommit, extra: "ALLOW_DIRTY=yes\n", git: append([]rule{{Tool: "git", Contains: []string{"status", "Dockerfile"}, Stdout: " M Dockerfile\n"}}, gitRules("")...), want: "warning: ALLOW_DIRTY=yes: the Dockerfile of"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for target, env := range targets {
			for _, c := range cases {
				t.Run(target+", "+c.name, func(t *testing.T) {
					t.Parallel()
					r := r.fresh(t)
					if c.without {
						r.without("git")
					}
					f := newImgFix(c.commit)
					r.imageBinaries(c.commit, "amd64")
					rules := slices.Clone(c.git)
					rules = append(rules, pushed(f))
					for _, tool := range []string{"kubectl", "ssh"} {
						rules = append(rules, nodeRules(tool, "amd64")...)
					}
					r.rules(append(rules, imgEngine("docker", "amd64", "amd64", f)...)...)
					res := r.image("KMS=local\n"+env+c.extra, resolveAndDeliver, false)
					res.want(t, c.code, c.want)
					if b := builds(res); c.code == 2 && len(b) != 0 {
						t.Fatalf("built after all: %v", b)
					}
				})
			}
		}
		// Here: a changed tree and an unknown commit pass, and the
		// Dockerfile is not asked about.
		for _, commit := range []string{imgCommit + "-dirty", ""} {
			t.Run("here, commit "+commit, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(commit)
				r.imageBinaries(commit, "amd64")
				r.rules(append([]rule{{Tool: "docker", Contains: []string{"--rm", "version"}, Stdout: "cactusfs 0.7.0 build 412 (" + map[bool]string{true: "commit " + commit + ", ", false: ""}[commit != ""] + "go1.27.4 linux/amd64, image)\n"}},
					imgEngine("docker", "amd64", "amd64", f)...)...)
				res := r.image("KMS=local\nDEPLOY_TARGET=dd\n", resolveAndDeliver, false)
				res.want(t, 0, "resolved "+f.ref())
				if len(callsOf(res, "git")) != 0 {
					t.Errorf("git was asked for a target here: %v", callsOf(res, "git"))
				}
			})
		}
	})
}

// TestImagePlatformFromTarget: the platform is the target's - its engine's
// info, or the nodes' kubernetes.io/arch - and the image is built for it
// with --platform; its version line is read only on an engine of that
// architecture.
func TestImagePlatformFromTarget(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		rules   func(f imgFix) []rule
		code    int
		want    []string
		tool    string
		build   []string
		version bool
	}{
		{
			name: "docker desktop on arm64", env: "DEPLOY_TARGET=dd\n",
			rules: func(f imgFix) []rule { return imgEngine("docker", "arm64", "arm64", f) },
			want:  []string{"resolved cactusfs:0.7.0-412-", " arm64"}, tool: "docker",
			build: []string{"buildx", "build", "--builder", "default", "--load", "--platform", "linux/arm64"}, version: true,
		},
		{
			name: "podman", env: "DEPLOY_TARGET=podman\n",
			rules: func(f imgFix) []rule { return imgEngine("podman", "amd64", "amd64", f) },
			want:  []string{" amd64"}, tool: "podman",
			build: []string{"build", "--format", "docker", "--platform", "linux/amd64"}, version: true,
		},
		{
			name: "a cluster of arm64 nodes, built on amd64", env: "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=" + registry + "\n",
			rules: func(f imgFix) []rule {
				return append(append(nodeRules("kubectl", "arm64", "arm64", "arm64"),
					rule{Tool: "docker", Contains: []string{"inspect", registry + ":0.7.0-412-" + f.id12()}, Stdout: f.fields("arm64", registry+"@sha256:"+f.hex)}),
					imgEngine("docker", "amd64", "arm64", f)...)
			},
			want: []string{" arm64", "is linux/amd64 and the image linux/arm64: its version line is not read"}, tool: "docker",
			build: []string{"buildx", "build", "--builder", "default", "--load", "--platform", "linux/arm64"},
		},
		{
			name: "nodes of two architectures", env: "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=" + registry + "\n",
			rules: func(f imgFix) []rule {
				return append(nodeRules("kubectl", "amd64", "arm64"), imgEngine("docker", "amd64", "amd64", f)...)
			},
			code: 2, want: []string{"are not all of one architecture (kubernetes.io/arch: 1 amd64, 1 arm64)"},
		},
		{
			name: "an engine of another platform", env: "DEPLOY_TARGET=dd\n",
			rules: func(f imgFix) []rule {
				return append([]rule{{Tool: "docker", Contains: []string{"info"}, Stdout: "windows/x86_64\n"}}, imgEngine("docker", "amd64", "amd64", f)...)
			},
			code: 2, want: []string{"runs 'windows/x86_64': the image of CactusFS is made for linux/amd64 and linux/arm64"},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				r.imageBinaries(imgCommit, "amd64", "arm64")
				r.rules(append(c.rules(f), gitRules("")...)...)
				res := r.image("KMS=local\n"+c.env, resolveAndDeliver, false)
				res.want(t, c.code, c.want...)
				b := builds(res)
				if c.code != 0 {
					if len(b) != 0 {
						t.Fatalf("built after all: %v", b)
					}
					return
				}
				if len(b) != 1 || b[0].Tool != c.tool || !slices.Equal(argsAfter(b[0], c.build[0])[:len(c.build)], c.build) {
					t.Fatalf("the build is %v, want %s %q", b, c.tool, c.build)
				}
				if got := len(callsOf(res, c.tool, "--rm", "version")) == 1; got != c.version {
					t.Errorf("the version line was read: %v, want %v", got, c.version)
				}
			})
		}
	})
}

// TestImageFromSource: --from-source builds this tree's Dockerfile with the
// version, build and commit git gives; a tree with changes is -dirty, and is
// refused for a target on another machine unless ALLOW_DIRTY=yes, as is a
// tree of no known commit - which, here, builds as build 0.
func TestImageFromSource(t *testing.T) {
	const ce = "DEPLOY_TARGET=ce\nCE_HOST=deploy@server.example.test\n"
	unknown := []rule{{Tool: "git", Contains: []string{"rev-parse"}, Stderr: "fatal: not a git repository\n", Exit: 128}, {Tool: "git", Exit: 128}}
	cases := []struct {
		name    string
		env     string
		git     []rule
		without bool
		commit  string
		code    int
		want    []string
	}{
		{name: "here, clean", env: "DEPLOY_TARGET=dd\n", git: gitRules(""), commit: imgCommit, want: []string{"version 0.7.0, build 412, commit " + imgCommit}},
		{name: "here, changed", env: "DEPLOY_TARGET=dd\n", git: gitRules(" M internal/app/x.go\n"), commit: imgCommit + "-dirty", want: []string{"commit " + imgCommit + "-dirty"}},
		{name: "here, a file git does not track", env: "DEPLOY_TARGET=dd\n", git: gitRules("?? notes.txt\n"), commit: imgCommit + "-dirty"},
		{name: "remote, clean", env: ce, git: gitRules(""), commit: imgCommit},
		{name: "remote, changed", env: ce, git: gitRules(" M internal/app/x.go\n"), commit: imgCommit + "-dirty", code: 2, want: []string{"this tree is of a tree with uncommitted changes (commit " + imgCommit + "-dirty), and the target is on another machine (Docker CE on deploy@server.example.test)", "ALLOW_DIRTY=yes"}},
		{name: "remote, changed, allowed", env: ce + "ALLOW_DIRTY=yes\n", git: gitRules(" M internal/app/x.go\n"), commit: imgCommit + "-dirty", want: []string{"warning: ALLOW_DIRTY=yes: this tree is of a tree with uncommitted changes"}},
		{name: "remote, no repository", env: ce, git: unknown, code: 2, want: []string{"is of no known commit"}},
		{name: "remote, no git", env: ce, without: true, code: 2, want: []string{"is of no known commit"}},
		{name: "here, no repository", env: "DEPLOY_TARGET=dd\n", git: unknown, want: []string{"build 0, commit unknown"}},
		{name: "here, no git", env: "DEPLOY_TARGET=dd\n", without: true, want: []string{"build 0, commit unknown"}},
		{name: "remote k3s, changed", env: "DEPLOY_TARGET=k3s\nK3S_SSH=admin@k3s.example.test\n", git: gitRules(" M x\n"), commit: imgCommit + "-dirty", code: 2, want: []string{"(k3s on admin@k3s.example.test)"}},
		{name: "a registry, changed", env: "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=" + registry + "\n", git: gitRules(" M x\n"), commit: imgCommit + "-dirty", code: 2, want: []string{"(pushed to " + registry + ")"}},
		{name: "a build host, changed", env: "DEPLOY_TARGET=dd\nBUILD_HOST=builder@build.example.test\n", git: gitRules(" M x\n"), commit: imgCommit + "-dirty", code: 2, want: []string{"(built on builder@build.example.test)"}},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				if c.without {
					r.without("git")
				}
				f := newImgFix(c.commit)
				rules := append(slices.Clone(c.git), nodeRules("kubectl", "amd64")...)
				rules = append(rules, nodeRules("ssh", "amd64")...)
				rules = append(rules, pushed(f))
				if c.commit == "" {
					rules = append(rules,
						rule{Tool: "docker", Contains: []string{"inspect", workTag("amd64")}, Stdout: fieldsLine(f.hex, "linux/amd64", "", "0.7.0-0", f.layers)},
						rule{Tool: "docker", Contains: []string{"--rm", "version"}, Stdout: "cactusfs 0.7.0 local build (go1.27.4 linux/amd64, image)\n"})
				}
				rules = append(rules, imgEngine("docker", "amd64", "amd64", f)...)
				r.rules(rules...)
				res := r.image("KMS=local\nIMAGE_SOURCE=source\n"+c.env, resolveAndDeliver, false)
				res.want(t, c.code, c.want...)
				b := builds(res)
				if c.code != 0 {
					if len(b) != 0 {
						t.Fatalf("built after all: %v", b)
					}
					return
				}
				if len(b) != 1 {
					t.Fatalf("%d builds: %v", len(b), res.calls)
				}
				build := "412"
				if c.commit == "" {
					build = "0"
				}
				args := argsAfter(b[0], "buildx")
				if slices.Contains(args, "--target") || slices.Contains(args, "--build-context") {
					t.Errorf("a build from source is the Dockerfile's product image: %q", args)
				}
				for _, w := range []string{"VERSION=0.7.0", "BUILD=" + build, "COMMIT=" + c.commit} {
					if !slices.Contains(args, w) {
						t.Errorf("the build lacks the argument %s: %q", w, args)
					}
				}
				if args[len(args)-1] != r.repo {
					t.Errorf("the build's context is %s, want the repository", args[len(args)-1])
				}
				for _, g := range callsOf(res, "git") {
					if !slices.Equal(g.Args[:2], []string{"-C", r.repo}) {
						t.Errorf("git was not pointed at the repository: %q", g.Args)
					}
				}
			})
		}
	})
}

// TestImageRemoteByEngine: whether the target is on another machine is
// asked of the engine or the cluster, not taken from a name: a Docker
// context whose endpoint is not a local socket, a Podman whose service is
// remote, and a desktop's Kubernetes context whose API server is not on
// this machine.
func TestImageRemoteByEngine(t *testing.T) {
	cases := []struct {
		name  string
		env   string
		rules []rule
		code  int
		want  string
	}{
		{
			name: "a docker context over ssh", env: "DEPLOY_TARGET=dd\nENGINE_CONTEXT=farbox\n",
			rules: []rule{{Tool: "docker", Contains: []string{"context", "inspect", "farbox"}, Stdout: "ssh://deploy@far.example.test\n"}},
			code:  2, want: "(the Docker context farbox, whose endpoint is 'ssh://deploy@far.example.test')",
		},
		{
			name: "a docker context over tcp", env: "DEPLOY_TARGET=ce\nENGINE_CONTEXT=farbox\n",
			rules: []rule{{Tool: "docker", Contains: []string{"context", "inspect", "farbox"}, Stdout: "tcp://192.0.2.7:2376\n"}},
			code:  2, want: "whose endpoint is 'tcp://192.0.2.7:2376'",
		},
		{
			name: "a docker context that cannot be read", env: "DEPLOY_TARGET=dd\n",
			rules: []rule{{Tool: "docker", Contains: []string{"context", "inspect"}, Stderr: "context not found\n", Exit: 1}},
			code:  1, want: "could not inspect the Docker context default",
		},
		{name: "a local docker context", env: "DEPLOY_TARGET=dd\n"},
		{
			name: "a podman machine", env: "DEPLOY_TARGET=podman\n",
			rules: []rule{{Tool: "podman", Contains: []string{"{{.Host.ServiceIsRemote}}"}, Stdout: "true\n"}},
			code:  2, want: "(Podman's service is on another machine (podman info: Host.ServiceIsRemote))",
		},
		{
			name: "a podman that does not say", env: "DEPLOY_TARGET=podman\n",
			rules: []rule{{Tool: "podman", Contains: []string{"{{.Host.ServiceIsRemote}}"}, Stdout: "<no value>\n"}},
			code:  1, want: "does not say whether its service is remote",
		},
		{name: "a local podman", env: "DEPLOY_TARGET=podman\n"},
		{
			name: "docker desktop's kubernetes", env: "DEPLOY_TARGET=k8s\nK8S_CONTEXT=docker-desktop\n",
			rules: []rule{kubeServer("https://kubernetes.docker.internal:6443")},
		},
		{
			name: "rancher desktop's kubernetes", env: "DEPLOY_TARGET=rancher\nRANCHER_ENGINE=moby\nRANCHER_KUBERNETES=yes\nK8S_S3_ALLOW_FROM=192.0.2.0/24\n",
			rules: []rule{kubeServer("https://127.0.0.1:6443")},
		},
		{
			name: "a context named docker-desktop elsewhere", env: "DEPLOY_TARGET=k8s\nK8S_CONTEXT=docker-desktop\n",
			rules: []rule{kubeServer("https://192.0.2.9:6443")},
			code:  2, want: "names the API server 'https://192.0.2.9:6443', which is not on this machine",
		},
		{
			name: "a context named rancher-desktop on a look-alike host", env: "DEPLOY_TARGET=k8s\nK8S_CONTEXT=rancher-desktop\n",
			rules: []rule{kubeServer("https://localhost.example.test:6443")},
			code:  2, want: "which is not on this machine",
		},
		{name: "k3s here", env: "DEPLOY_TARGET=k3s\n"},
		{
			name: "k3s through K3S_IMPORT", env: "DEPLOY_TARGET=k3s\nK3S_IMPORT=docker exec -i k3s-bench k3s ctr -n k8s.io images import -\n",
			code: 2, want: "(imported by K3S_IMPORT)",
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit + "-dirty")
				rules := append(slices.Clone(c.rules), gitRules(" M x\n")...)
				rules = append(rules, nodeRules("k3s", "amd64")...)
				rules = append(rules, nodeRules("kubectl", "amd64")...)
				rules = append(rules, imgEngine("docker", "amd64", "amd64", f)...)
				r.rules(append(rules, imgEngine("podman", "amd64", "amd64", f)...)...)
				res := r.image("KMS=local\nIMAGE_SOURCE=source\n"+c.env, resolveAndDeliver, false)
				res.want(t, c.code, c.want)
				if b := builds(res); c.code != 0 && len(b) != 0 {
					t.Fatalf("built after all: %v", b)
				}
			})
		}
	})
}

// TestImageBuildHost: BUILD_HOST builds through DOCKER_HOST=ssh:// with the
// builder "default" of that engine, after the script's own ssh has checked
// its host key; the image is saved there by the tag that is checked to name
// it, loaded on the target's engine - the same bytes - and compared there.
func TestImageBuildHost(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		f := newImgFix(imgCommit)
		r.imageBinaries(imgCommit, "amd64")
		r.rules(append(gitRules(""), imgEngine("docker", "amd64", "amd64", f)...)...)
		res := r.image("KMS=local\nDEPLOY_TARGET=dd\nBUILD_HOST=builder@build.example.test\nENGINE_CONTEXT=desktop-linux\n", resolveAndDeliver, false, "SSH_AUTH_SOCK=/agent.sock")
		res.want(t, 0, "resolved "+f.ref(), "loaded on docker --context desktop-linux",
			"run: DOCKER_HOST=ssh://builder@build.example.test docker save "+f.ref()+" | docker --context desktop-linux load")
		if len(res.calls) == 0 || res.calls[0].Tool != "ssh" || !slices.Equal(res.calls[0].Args[len(res.calls[0].Args)-2:], []string{"builder@build.example.test", "true"}) {
			t.Fatalf("the first call is not the check of the build host's key: %v", res.calls)
		}
		b := builds(res)
		if len(b) != 1 || b[0].Env["DOCKER_HOST"] != "ssh://builder@build.example.test" || b[0].Env["SSH_AUTH_SOCK"] != "/agent.sock" ||
			!slices.Equal(b[0].Args[:4], []string{"buildx", "build", "--builder", "default"}) {
			t.Fatalf("the build is not on the build host's default builder: %v", b)
		}
		save := callsOf(res, "docker", "save", f.ref())
		load := callsOf(res, "docker", "load")
		if len(save) != 1 || save[0].Env["DOCKER_HOST"] != "ssh://builder@build.example.test" {
			t.Fatalf("the save is not on the build host: %v", save)
		}
		if len(load) != 1 || load[0].Env["DOCKER_HOST"] != "" || !slices.Equal(load[0].Args, []string{"--context", "desktop-linux", "load"}) {
			t.Fatalf("the load is not on the target's engine: %v", load)
		}
		if load[0].Stdin != f.tarSum() {
			t.Errorf("the load read something else than the save wrote")
		}
		checkedThere, checkedHere := -1, -1
		for i, c := range res.calls {
			if c.Tool == "docker" && containsAll(c.Args, []string{"inspect", f.ref()}) {
				if c.Env["DOCKER_HOST"] != "" && checkedThere < 0 {
					checkedThere = i
				}
				if c.Env["DOCKER_HOST"] == "" {
					checkedHere = i
				}
			}
		}
		saved := callIndex(res, "docker", "save")
		loaded := callIndex(res, "docker", "load")
		if checkedThere < 0 || checkedThere > saved || checkedHere < loaded {
			t.Errorf("the tag is not checked before the save (%d, save %d), or the copy after the load (%d, load %d)", checkedThere, saved, checkedHere, loaded)
		}
	})
}

// TestImageDelivery: how the image reaches each target, and the routes that
// are refused before anything is built.
func TestImageDelivery(t *testing.T) {
	const k3sImport = "docker exec -i k3s-bench k3s ctr -n k8s.io images import -"
	cases := []struct {
		name    string
		env     string
		without []string
		rules   func(f imgFix) []rule
		code    int
		want    []string
		check   func(t *testing.T, f imgFix, res result)
	}{
		{
			name: "docker ce over ssh: saved here, loaded there",
			env:  "DEPLOY_TARGET=ce\nCE_HOST=deploy@server.example.test\n",
			want: []string{"loaded on DOCKER_HOST=ssh://deploy@server.example.test docker"},
			check: func(t *testing.T, f imgFix, res result) {
				save := callsOf(res, "docker", "save", f.ref())
				load := callsOf(res, "docker", "load")
				if len(save) != 1 || save[0].Env["DOCKER_HOST"] != "" || len(load) != 1 || load[0].Env["DOCKER_HOST"] != "ssh://deploy@server.example.test" {
					t.Fatalf("save %v, load %v", save, load)
				}
				if load[0].Stdin != f.tarSum() {
					t.Errorf("the load read something else than the save wrote")
				}
				if res.calls[0].Tool != "ssh" || !slices.Contains(res.calls[0].Args, "true") {
					t.Errorf("the server's host key is not checked first: %v", res.calls[0])
				}
			},
		},
		{
			name: "docker ce built on the server itself",
			env:  "DEPLOY_TARGET=ce\nCE_HOST=deploy@server.example.test\nBUILD_HOST=deploy@server.example.test\n",
			want: []string{"is on the target's engine"},
			check: func(t *testing.T, _ imgFix, res result) {
				if len(callsOf(res, "docker", "save")) != 0 {
					t.Errorf("an image built on its target was moved")
				}
				if b := builds(res); len(b) != 1 || b[0].Env["DOCKER_HOST"] != "ssh://deploy@server.example.test" {
					t.Errorf("not built on the server: %v", b)
				}
			},
		},
		{
			name: "k3s over ssh: imported with k3s ctr",
			env:  "DEPLOY_TARGET=k3s\nK3S_SSH=admin@k3s.example.test\n",
			want: []string{"imported into the containerd of the cluster's one node"},
			check: func(t *testing.T, f imgFix, res result) {
				imp := callsOf(res, "ssh", "import")
				want := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=20", "-o", "ControlPath=none", "--", "admin@k3s.example.test", "k3s", "ctr", "-n", "k8s.io", "images", "import", "-"}
				if len(imp) != 1 || !slices.Equal(imp[0].Args, want) || imp[0].Stdin != f.tarSum() {
					t.Fatalf("the import is %v, want ssh %q with the image on stdin", imp, want)
				}
			},
		},
		{
			name: "k3s here: imported with k3s ctr",
			env:  "DEPLOY_TARGET=k3s\n",
			check: func(t *testing.T, f imgFix, res result) {
				imp := callsOf(res, "k3s", "import")
				if len(imp) != 1 || !slices.Equal(imp[0].Args, []string{"ctr", "-n", "k8s.io", "images", "import", "-"}) || imp[0].Stdin != f.tarSum() {
					t.Fatalf("the import is %v", imp)
				}
			},
		},
		{
			name: "K3S_IMPORT: its words, the image on stdin",
			env:  "DEPLOY_TARGET=k3s\nK3S_IMPORT=" + k3sImport + "\n",
			check: func(t *testing.T, f imgFix, res result) {
				imp := callsOf(res, "docker", "exec", "import")
				if len(imp) != 1 || !slices.Equal(imp[0].Args, strings.Fields(k3sImport)[1:]) || imp[0].Stdin != f.tarSum() {
					t.Fatalf("the import is %v", imp)
				}
			},
		},
		{
			name:  "an import on two nodes",
			env:   "DEPLOY_TARGET=k3s\nK3S_SSH=admin@k3s.example.test\n",
			rules: func(imgFix) []rule { return nodeRules("ssh", "amd64", "amd64") },
			code:  2, want: []string{"has 2 nodes", "reaches the containerd of one node only", "ErrImageNeverPull", "set K8S_REGISTRY"},
		},
		{
			name:  "K3S_IMPORT on two nodes",
			env:   "DEPLOY_TARGET=k3s\nK3S_IMPORT=" + k3sImport + "\n",
			rules: func(imgFix) []rule { return nodeRules("k3s", "amd64", "amd64") },
			code:  2, want: []string{"has 2 nodes", "set K8S_REGISTRY"},
		},
		{
			name: "pushed to K8S_REGISTRY, run by its digest",
			env:  "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=" + registry + "\n",
			rules: func(f imgFix) []rule {
				return append(nodeRules("kubectl", "amd64", "amd64", "amd64"), pushed(f))
			},
			want: []string{"pushed; the pods run " + registry + "@sha256:"},
			check: func(t *testing.T, f imgFix, res result) {
				tag := callsOf(res, "docker", "tag", registry+":0.7.0-412-"+f.id12())
				push := callsOf(res, "docker", "push")
				if len(tag) != 1 || !slices.Equal(argsAfter(tag[0], "tag"), []string{"tag", f.hex, registry + ":0.7.0-412-" + f.id12()}) {
					t.Fatalf("the registry's tag is not given by the ID: %v", tag)
				}
				if len(push) != 1 || !slices.Equal(argsAfter(push[0], "push"), []string{"push", registry + ":0.7.0-412-" + f.id12()}) {
					t.Fatalf("push %v", push)
				}
				if !strings.Contains(res.stdout, "resolved "+registry+"@sha256:"+f.hex+" amd64") {
					t.Errorf("not run by its digest:\n%s", res.stdout)
				}
			},
		},
		{
			name:    "pushed with podman, its digest from --digestfile",
			env:     "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=" + registry + "\n",
			without: []string{"docker"},
			rules: func(f imgFix) []rule {
				return append(nodeRules("kubectl", "amd64"),
					rule{Tool: "podman", Contains: []string{"push"}, Files: map[string]string{"--digestfile": "sha256:" + f.hex}})
			},
			check: func(t *testing.T, f imgFix, res result) {
				if !strings.Contains(res.stdout, "resolved "+registry+"@sha256:"+f.hex+" amd64") {
					t.Errorf("not run by its digest:\n%s", res.output())
				}
			},
		},
		{
			name: "a push that gives no digest",
			env:  "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=" + registry + "\n",
			rules: func(f imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"inspect", registry + ":0.7.0-412-" + f.id12()}, Stdout: f.fields("amd64", "other.example.test/x@sha256:"+f.hex)}}
			},
			code: 1, want: []string{"gave no digest"},
		},
		{
			name: "a push whose tag names another image",
			env:  "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=" + registry + "\n",
			rules: func(f imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"inspect", registry + ":0.7.0-412-" + f.id12()}, Stdout: fieldsLine(randomHex(), "linux/amd64", imgCommit, "0.7.0-412", f.layers, registry+"@sha256:"+f.hex)}}
			},
			code: 1, want: []string{"not the one built"},
		},
		{
			name:  "docker desktop's kubernetes: the local image",
			env:   "DEPLOY_TARGET=k8s\nK8S_CONTEXT=docker-desktop\n",
			rules: func(imgFix) []rule { return []rule{kubeServer("https://kubernetes.docker.internal:6443")} },
			want:  []string{"is on the Docker engine here, whose images the cluster's one node sees"},
			check: func(t *testing.T, _ imgFix, res result) {
				if len(callsOf(res, "docker", "save")) != 0 || len(callsOf(res, "docker", "push")) != 0 {
					t.Errorf("the local image was moved: %v", res.calls)
				}
			},
		},
		{
			name: "a cluster the image cannot reach",
			env:  "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\n",
			code: 2, want: []string{"the image cannot reach the cluster of kubectl --context prod: set K8S_REGISTRY"},
		},
		{
			name: "a registry and an import",
			env:  "DEPLOY_TARGET=k3s\nK8S_REGISTRY=" + registry + "\nK3S_IMPORT=" + k3sImport + "\n",
			code: 2, want: []string{"K8S_REGISTRY and K3S_IMPORT are both set"},
		},
		{
			name: "a registry with a tag",
			env:  "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=" + registry + ":latest\n",
			code: 2, want: []string{"K8S_REGISTRY=" + registry + ":latest: is the repository the image is pushed to"},
		},
		{
			name: "a registry by host and port",
			env:  "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=registry.example.test:5000\n",
			code: 2, want: []string{"K8S_REGISTRY=registry.example.test:5000: names a registry, not a repository in it"},
		},
		{
			name: "a registry by host, port and path",
			env:  "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=registry.example.test:5000/cactusfs\n",
			rules: func(f imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"inspect", "registry.example.test:5000/cactusfs:0.7.0-412-" + f.id12()}, Stdout: f.fields("amd64", "registry.example.test:5000/cactusfs@sha256:"+f.hex)}}
			},
			want: []string{"pushed; the pods run registry.example.test:5000/cactusfs@sha256:"},
		},
		{
			name: "a save that fails",
			env:  "DEPLOY_TARGET=ce\nCE_HOST=deploy@server.example.test\n",
			rules: func(imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"save"}, Stdout: "half", Stderr: "no space left\n", Exit: 1}}
			},
			code: 1, want: []string{"saving cactusfs:0.7.0-412-", "failed"},
			check: func(t *testing.T, _ imgFix, res result) {
				if len(callsOf(res, "docker", "load")) != 0 {
					t.Errorf("a failed save was loaded")
				}
			},
		},
		{
			name: "a tag moved before the save",
			env:  "DEPLOY_TARGET=ce\nCE_HOST=deploy@server.example.test\n",
			rules: func(f imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"inspect", f.ref()}, Stdout: fieldsLine(randomHex(), "linux/amd64", imgCommit, "0.7.0-412", f.layers)}}
			},
			code: 1, want: []string{"names the image", "not the one checked"},
			check: func(t *testing.T, _ imgFix, res result) {
				if len(callsOf(res, "docker", "save")) != 0 {
					t.Errorf("an image whose tag moved was saved")
				}
			},
		},
		{
			name: "a load that differs on the target",
			env:  "DEPLOY_TARGET=podman\nBUILD_HOST=builder@build.example.test\nALLOW_DIRTY=yes\n",
			rules: func(f imgFix) []rule {
				return []rule{{Tool: "podman", Contains: []string{"inspect", f.ref()}, Stdout: fieldsLine(randomHex(), "linux/amd64", imgCommit, "0.7.0-412", "sha256:"+randomHex()+" ")}}
			},
			code: 1, want: []string{"is not the one sent"},
		},
		{
			name: "a load with another ID and the same content",
			env:  "DEPLOY_TARGET=podman\nBUILD_HOST=builder@build.example.test\nALLOW_DIRTY=yes\n",
			rules: func(f imgFix) []rule {
				return []rule{{Tool: "podman", Contains: []string{"inspect", f.ref()}, Stdout: fieldsLine(randomHex(), "linux/amd64", imgCommit, "0.7.0-412", f.layers)}}
			},
			want: []string{"loaded on podman"},
		},
		{
			name: "rancher on containerd is 10.6's",
			env:  "DEPLOY_TARGET=rancher\nRANCHER_ENGINE=containerd\n",
			code: 3, want: []string{"not built yet (PLAN 10.6)"},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.without(c.without...)
				f := newImgFix(imgCommit)
				r.imageBinaries(imgCommit, "amd64")
				rules := gitRules("")
				if c.rules != nil {
					rules = append(c.rules(f), rules...)
				}
				for _, tool := range []string{"kubectl", "k3s", "ssh"} {
					rules = append(rules, nodeRules(tool, "amd64")...)
				}
				rules = append(rules, imgEngine("docker", "amd64", "amd64", f)...)
				rules = append(rules, imgEngine("podman", "amd64", "amd64", f)...)
				r.rules(rules...)
				res := r.image("KMS=local\n"+c.env, resolveAndDeliver, false)
				res.want(t, c.code, c.want...)
				if c.code == 2 || c.code == 3 {
					if b := builds(res); len(b) != 0 {
						t.Fatalf("built after all: %v", b)
					}
				}
				if c.check != nil {
					c.check(t, f, res)
				}
				noPullCommand(t, res)
			})
		}
	})
}

// TestImageStreamSignal: a TERM while the image is streamed to the target
// ends the run with 143, and the work directory of the tar stream is gone
// (the TMPDIR of the run is empty: rig.image).
func TestImageStreamSignal(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		f := newImgFix(imgCommit)
		r.imageBinaries(imgCommit, "amd64")
		r.rules(append(append([]rule{{Tool: "docker", Contains: []string{"load"}, TermParent: true}}, gitRules("")...),
			imgEngine("docker", "amd64", "amd64", f)...)...)
		res := r.image("KMS=local\nDEPLOY_TARGET=ce\nCE_HOST=deploy@server.example.test\n", resolveAndDeliver, false)
		res.want(t, 143)
		if len(callsOf(res, "docker", "load")) != 1 {
			t.Fatalf("the stream was not reached: %v", res.calls)
		}
		if strings.Contains(res.stdout, "resolved ") {
			t.Fatalf("the run went on after the signal:\n%s", res.stdout)
		}
	})
}

// TestImageGiven: IMAGE_SOURCE=image uses IMAGE as given - pulled for the
// target's platform only when absent, and never with OFFLINE=yes - inspects
// it once, checks it, reads its version line on a native engine, and
// deploys it by its digest, or by a tag of its ID when moved as a tar.
func TestImageGiven(t *testing.T) {
	const repo = "registry.example.test/cactusfs"
	digestOf := func(f imgFix) string { return repo + "@sha256:" + f.hex }
	absent := func(tool, ref string) rule {
		return rule{Tool: tool, Contains: []string{"{{.Id}}", ref}, Stderr: "Error response from daemon: No such image: " + ref + "\n", Exit: 1}
	}
	given := func(tool, ref string, f imgFix, fields string) []rule {
		return []rule{
			{Tool: tool, Contains: []string{"inspect", ref}, Stdout: fields},
			{Tool: tool, Contains: []string{"--rm", "version", f.hex}, Stdout: versionLine(f.commit, "amd64")},
		}
	}
	cases := []struct {
		name  string
		env   string
		image func(f imgFix) string
		rules func(f imgFix, image string) []rule
		code  int
		want  func(f imgFix) []string
		pull  bool
		tool  string
	}{
		{
			name: "by digest, here", env: "DEPLOY_TARGET=dd\n", image: digestOf,
			rules: func(f imgFix, image string) []rule { return given("docker", image, f, f.fields("amd64", digestOf(f))) },
			want: func(f imgFix) []string {
				return []string{"resolved " + digestOf(f) + " amd64", "is on the target's engine", "it says 'cactusfs 0.7.0 build 412"}
			},
		},
		{
			name: "a version line that is not the label's", env: "DEPLOY_TARGET=dd\n", image: digestOf,
			rules: func(f imgFix, image string) []rule {
				return append([]rule{{Tool: "docker", Contains: []string{"--rm", "version", f.hex}, Stdout: "cactusfs 0.6.0 build 9 (go1.27.4 linux/amd64, image)\n"}},
					given("docker", image, f, f.fields("amd64", digestOf(f)))...)
			},
			code: 1, want: func(imgFix) []string { return []string{"not the build meant"} },
		},
		{
			name: "by tag, absent: pulled, then by its digest", env: "DEPLOY_TARGET=dd\n",
			image: func(imgFix) string { return repo + ":0.7.0" },
			rules: func(f imgFix, image string) []rule {
				return append([]rule{absent("docker", image)}, given("docker", image, f, f.fields("amd64", digestOf(f)))...)
			},
			want: func(f imgFix) []string { return []string{"resolved " + digestOf(f) + " amd64"} },
			pull: true,
		},
		{
			name: "podman writes docker.io/library/", env: "DEPLOY_TARGET=podman\n", tool: "podman",
			image: func(imgFix) string { return "cactusfs:0.7.0" },
			rules: func(f imgFix, image string) []rule {
				return given("podman", image, f, f.fields("amd64", "docker.io/library/cactusfs@sha256:"+f.hex))
			},
			want: func(f imgFix) []string {
				return []string{"resolved docker.io/library/cactusfs@sha256:" + f.hex + " amd64"}
			},
		},
		{
			name: "absent and offline", env: "DEPLOY_TARGET=dd\nOFFLINE=yes\n",
			image: func(imgFix) string { return repo + ":0.7.0" },
			rules: func(_ imgFix, image string) []rule { return []rule{absent("docker", image)} },
			code:  2, want: func(imgFix) []string {
				return []string{"OFFLINE=yes, and the image registry.example.test/cactusfs:0.7.0 is not on"}
			},
		},
		{
			// An image of the target's own engine that no registry holds is
			// run by a tag that names its ID: the ID pins the content there.
			name: "no digest, on the engine that runs it", env: "DEPLOY_TARGET=dd\n",
			image: func(imgFix) string { return "cactusfs:mine" },
			rules: func(f imgFix, image string) []rule { return given("docker", image, f, f.fields("amd64")) },
			want:  func(f imgFix) []string { return []string{"resolved cactusfs:", "-" + f.hex[:12] + " amd64"} },
		},
		{
			name: "a digest that is not one", env: "DEPLOY_TARGET=dd\n",
			image: func(imgFix) string { return repo + "@sha256:abc" },
			rules: func(imgFix, string) []rule { return nil },
			code:  2, want: func(imgFix) []string { return []string{"names a digest that is not sha256:<64 hex>"} },
		},
		{
			name: "another platform", env: "DEPLOY_TARGET=dd\n", image: digestOf,
			rules: func(f imgFix, image string) []rule { return given("docker", image, f, f.fields("arm64")) },
			code:  2, want: func(imgFix) []string { return []string{"is for linux/arm64, and the target is linux/amd64"} },
		},
		{
			name: "not an image of the Dockerfile", env: "DEPLOY_TARGET=dd\n", image: digestOf,
			rules: func(f imgFix, image string) []rule {
				return given("docker", image, f, fieldsLine(f.hex, "linux/amd64", "", "", f.layers))
			},
			code: 2, want: func(imgFix) []string { return []string{"has no label org.opencontainers.image.version"} },
		},
		{
			name: "a version label that is not one", env: "DEPLOY_TARGET=dd\n", image: digestOf,
			rules: func(f imgFix, image string) []rule {
				return given("docker", image, f, fieldsLine(f.hex, "linux/amd64", imgCommit, "0.7.0;x-412", f.layers))
			},
			code: 2, want: func(imgFix) []string { return []string{"the labels of the image", "which is not one"} },
		},
		{
			name: "of a changed tree, to a server", env: "DEPLOY_TARGET=ce\nCE_HOST=deploy@server.example.test\n", image: digestOf,
			rules: func(f imgFix, image string) []rule {
				return given("docker", image, f, fieldsLine(f.hex, "linux/amd64", imgCommit+"-dirty", "0.7.0-412", f.layers))
			},
			code: 2, want: func(imgFix) []string { return []string{"uncommitted changes", "ALLOW_DIRTY=yes"} },
		},
		{
			name: "the nodes pull it by its digest, checked here first", env: "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\n", image: digestOf,
			rules: func(f imgFix, image string) []rule {
				return append(nodeRules("kubectl", "amd64", "amd64"), given("docker", image, f, f.fields("amd64", digestOf(f)))...)
			},
			want: func(f imgFix) []string {
				return []string{"resolved " + digestOf(f) + " amd64", "the nodes pull", "it says 'cactusfs 0.7.0"}
			},
		},
		{
			name: "the nodes pull it, absent here: pulled here to be checked", env: "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\n", image: digestOf,
			rules: func(f imgFix, image string) []rule {
				return append(append(nodeRules("kubectl", "amd64"), absent("docker", image)), given("docker", image, f, f.fields("amd64", digestOf(f)))...)
			},
			want: func(f imgFix) []string { return []string{"resolved " + digestOf(f) + " amd64"} },
			pull: true,
		},
		{
			name: "the nodes pull a tag", env: "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\n",
			image: func(imgFix) string { return repo + ":0.7.0" },
			rules: func(imgFix, string) []rule { return nodeRules("kubectl", "amd64", "amd64") },
			code:  2, want: func(imgFix) []string { return []string{"so it is named by its digest"} },
		},
		{
			name: "the nodes pull it, offline", env: "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nOFFLINE=yes\n", image: digestOf,
			rules: func(imgFix, string) []rule { return nodeRules("kubectl", "amd64") },
			code:  2, want: func(imgFix) []string { return []string{"OFFLINE=yes, and the nodes would pull"} },
		},
		{
			name: "the nodes pull it from K8S_REGISTRY, offline", env: "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nOFFLINE=yes\nK8S_REGISTRY=" + repo + "\n", image: digestOf,
			rules: func(f imgFix, image string) []rule {
				return append(nodeRules("kubectl", "amd64"), given("docker", image, f, f.fields("amd64", digestOf(f)))...)
			},
			want: func(f imgFix) []string { return []string{"resolved " + digestOf(f) + " amd64", "the nodes pull"} },
		},
		{
			name: "imported into k3s, tagged by its ID", env: "DEPLOY_TARGET=k3s\nK3S_SSH=admin@k3s.example.test\n", image: digestOf,
			rules: func(f imgFix, image string) []rule {
				return append(append(append(nodeRules("ssh", "amd64"), given("docker", image, f, f.fields("amd64", digestOf(f)))...),
					rule{Tool: "docker", Contains: []string{"inspect", f.ref()}, Stdout: f.fields("amd64")}),
					rule{Tool: "docker", Contains: []string{"save"}, Stdout: f.tar}, crictlOf("ssh", f.hex))
			},
			want: func(f imgFix) []string {
				return []string{"resolved " + f.ref() + " amd64", "imported into the containerd"}
			},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				image := c.image(f)
				tool := c.tool
				if tool == "" {
					tool = "docker"
				}
				rules := append(c.rules(f, image),
					rule{Tool: "docker", Contains: []string{"context", "inspect"}, Stdout: "unix:///var/run/docker.sock\n"},
					rule{Tool: "podman", Contains: []string{"{{.Host.ServiceIsRemote}}"}, Stdout: "false\n"},
					rule{Tool: "docker", Contains: []string{"info"}, Stdout: "linux/x86_64\n"},
					rule{Tool: "podman", Contains: []string{"info"}, Stdout: "linux/amd64\n"})
				r.rules(rules...)
				res := r.image("KMS=local\nIMAGE_SOURCE=image\nIMAGE="+image+"\n"+c.env, resolveAndDeliver, false)
				res.want(t, c.code, c.want(f)...)
				if b := builds(res); len(b) != 0 {
					t.Errorf("a given image was built: %v", b)
				}
				pulls := callsOf(res, tool, "pull")
				if c.pull {
					if len(pulls) != 1 || !slices.Equal(argsAfter(pulls[0], "pull"), []string{"pull", "--platform", "linux/amd64", image}) {
						t.Errorf("the pull is %v", pulls)
					}
				} else if len(pulls) != 0 {
					t.Errorf("pulled: %v", pulls)
				}
				if c.code == 0 {
					var n int
					for _, c := range callsOf(res, tool, "inspect", image) {
						if !slices.Contains(c.Args, "{{.Id}}") {
							n++
						}
					}
					if n != 1 {
						t.Errorf("the image given is inspected %d times (but for whether it is there), want once", n)
					}
					if n := len(callsOf(res, tool, "--rm", "version", f.hex)); n != 1 {
						t.Errorf("the version line of the image given is read %d times, want once, by its ID", n)
					}
				}
				if strings.Contains(c.name, "tagged by its ID") {
					tag := callsOf(res, "docker", "image", "tag")
					imp := callsOf(res, "ssh", "import")
					save := callsOf(res, "docker", "save", f.ref())
					if len(tag) != 1 || !slices.Equal(argsAfter(tag[0], "tag"), []string{"tag", f.hex, f.ref()}) {
						t.Errorf("not tagged by its ID: %v", tag)
					}
					if len(imp) != 1 || len(save) != 1 || imp[0].Stdin != f.tarSum() {
						t.Errorf("save %v, import %v", save, imp)
					}
				}
			})
		}
	})
}

// TestImageOffline: with OFFLINE=yes every image the build needs is asked
// for with "image inspect" first, the build does not pull (the flag of each
// engine) and names its builder, nothing is pulled from a public registry,
// and the push to the operator's own K8S_REGISTRY stays allowed.
func TestImageOffline(t *testing.T) {
	cases := []struct {
		name  string
		env   string
		tool  string
		flag  string
		rules func(f imgFix) []rule
		code  int
		want  []string
	}{
		{name: "docker", env: "DEPLOY_TARGET=dd\n", tool: "docker", flag: "--pull=false"},
		{name: "podman", env: "DEPLOY_TARGET=podman\n", tool: "podman", flag: "--pull=never"},
		{
			name: "a push to the operator's registry", env: "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=" + registry + "\n",
			tool: "docker", flag: "--pull=false",
			rules: func(f imgFix) []rule { return append(nodeRules("kubectl", "amd64"), pushed(f)) },
			want:  []string{"pushed; the pods run " + registry + "@sha256:"},
		},
		{
			name: "the base image absent", env: "DEPLOY_TARGET=dd\n", tool: "docker",
			rules: func(imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"{{.Id}}"}, Stderr: "Error: No such image\n", Exit: 1}}
			},
			code: 2, want: []string{"OFFLINE=yes, and the base image gcr.io/distroless/static-debian13", "is not on docker --context default"},
		},
		{
			name: "the engine cannot be asked", env: "DEPLOY_TARGET=dd\n", tool: "docker",
			rules: func(imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"{{.Id}}"}, Stderr: "Cannot connect to the Docker daemon\n", Exit: 1}}
			},
			code: 1, want: []string{"asking docker --context default for gcr.io/distroless/static-debian13", "Cannot connect"},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		runtime := runtimeImage(t)
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				r.imageBinaries(imgCommit, "amd64")
				rules := gitRules("")
				if c.rules != nil {
					rules = append(c.rules(f), rules...)
				}
				rules = append(rules,
					rule{Tool: "docker", Contains: []string{"inspect", runtime, "{{.Os}}/{{.Architecture}}"}, Stdout: "linux/amd64\n"},
					rule{Tool: "podman", Contains: []string{"inspect", runtime, "{{.Os}}/{{.Architecture}}"}, Stdout: "linux/amd64\n"})
				rules = append(rules, imgEngine("docker", "amd64", "amd64", f)...)
				rules = append(rules, imgEngine("podman", "amd64", "amd64", f)...)
				r.rules(rules...)
				res := r.image("KMS=local\nOFFLINE=yes\n"+c.env, resolveAndDeliver, false)
				res.want(t, c.code, c.want...)
				noPull(t, res)
				b := builds(res)
				if c.code != 0 {
					if len(b) != 0 {
						t.Fatalf("built after all: %v", b)
					}
					return
				}
				first := callIndex(res, c.tool, "inspect", runtime)
				build := callIndex(res, c.tool, "build")
				if first < 0 || first > build {
					t.Errorf("the base image %s is not asked for before the build: %v", runtime, res.calls)
				}
				if len(b) != 1 || !slices.Contains(b[0].Args, c.flag) {
					t.Errorf("the build is %v, want %s", b, c.flag)
				}
				if c.tool == "docker" && valueOf(b[0], "--builder") != "default" {
					t.Errorf("the build names no builder: %q", b[0].Args)
				}
				for _, p := range callsOf(res, "docker", "push") {
					if !strings.HasPrefix(p.Args[len(p.Args)-1], registry+":") {
						t.Errorf("a push elsewhere than K8S_REGISTRY: %q", p.Args)
					}
				}
			})
		}
		// What OFFLINE=yes refuses of the settings: building from source
		// and installing Docker CE - before anything is asked.
		for _, refused := range []struct{ env, want string }{
			{"IMAGE_SOURCE=source\n", "OFFLINE=yes and an image built from source"},
			{"CE_INSTALL_DOCKER=yes\nCE_HOST=deploy@server.example.test\n", "OFFLINE=yes and CE_INSTALL_DOCKER=yes"},
		} {
			res := r.image("KMS=local\nOFFLINE=yes\nDEPLOY_TARGET=ce\n"+refused.env, resolveAndDeliver, false)
			res.want(t, 2, refused.want)
			if len(res.calls) != 0 {
				t.Errorf("%s: asked %v", refused.want, res.calls)
			}
		}
		r.withEnv("KMS=local\nOFFLINE=yes\n", "--docker", "dd", "--install", "--from-source", "--dry-run").want(t, 2, "OFFLINE=yes and an image built from source")
	})
}

// TestImageDryRun: a dry run asks what it reads - the engine's platform and
// endpoint, the nodes, git, whether an image is there - and prints the
// build, the push and the stream of save and load, running none of them.
func TestImageDryRun(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want []string
	}{
		{"here", "DEPLOY_TARGET=dd\n", []string{"run: docker --context default buildx build --builder default --load --platform linux/amd64", "resolved cactusfs:0.7.0-412-dry-run amd64"}},
		{"a server", "DEPLOY_TARGET=ce\nCE_HOST=deploy@server.example.test\n", []string{"run: docker --context default save cactusfs:0.7.0-412-dry-run | DOCKER_HOST=ssh://deploy@server.example.test docker load"}},
		{"k3s over ssh", "DEPLOY_TARGET=k3s\nK3S_SSH=admin@k3s.example.test\n", []string{"| on_host admin@k3s.example.test k3s ctr -n k8s.io images import -"}},
		{"a registry", "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=" + registry + "\n", []string{"run: docker --context default push " + registry + ":0.7.0-412-dry-run", "by the digest the push gives"}},
	}
	readOnly := map[string][][]string{
		"docker":  {{"info"}, {"image", "inspect"}, {"context", "inspect"}},
		"kubectl": {{"get", "nodes"}},
		// The Docker CLI reaches DOCKER_HOST=ssh:// over ssh (the fake
		// docker too): the transport of a docker call checked above.
		"ssh": {{"true"}, {"get", "nodes"}, {"docker", "system", "dial-stdio"}},
		"git": {{"rev-list"}, {"rev-parse"}, {"status"}},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				r.imageBinaries(imgCommit, "amd64")
				rules := append(nodeRules("kubectl", "amd64"), nodeRules("ssh", "amd64")...)
				rules = append(rules, gitRules("")...)
				r.rules(append(rules, imgEngine("docker", "amd64", "amd64", f)...)...)
				res := r.image("KMS=local\n"+c.env, resolveAndDeliver, true)
				res.want(t, 0, c.want...)
				for _, call := range res.calls {
					ok := false
					for _, words := range readOnly[call.Tool] {
						ok = ok || containsAll(call.Args, words)
					}
					if !ok {
						t.Errorf("a dry run ran %s %q", call.Tool, call.Args)
					}
				}
			})
		}
	})
}

// TestImageNative: image_native gives dev-init an image of the engine here
// and of its architecture - the image of the run when it is that one, else
// one built for it on the engine here - and leaves what image_deliver
// needs as it was.
func TestImageNative(t *testing.T) {
	code := `image_resolve; image_native; image_deliver; echo "resolved $RESOLVED_IMAGE $RESOLVED_ARCH native $RESOLVED_NATIVE_IMAGE"`
	eachShell(t, func(t *testing.T, r *rig) {
		t.Run("the image of the run", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			f := newImgFix(imgCommit)
			r.rules(append(gitRules(""), imgEngine("docker", "amd64", "amd64", f)...)...)
			res := r.image("KMS=local\nIMAGE_SOURCE=source\nDEPLOY_TARGET=dd\n", code, false)
			res.want(t, 0, "resolved "+f.ref()+" amd64 native "+f.ref(), "dev-init runs in "+f.ref()+", the image of this run")
			if b := builds(res); len(b) != 1 {
				t.Errorf("%d builds, want 1", len(b))
			}
		})
		t.Run("arm64 nodes, an amd64 engine here", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			arm, amd := newImgFix(imgCommit), newImgFix(imgCommit)
			rules := append(gitRules(""), nodeRules("kubectl", "arm64")...)
			rules = append(rules,
				rule{Tool: "docker", Contains: []string{"build", "linux/arm64"}, Files: map[string]string{"--iidfile": "sha256:" + arm.hex}},
				rule{Tool: "docker", Contains: []string{"build", "linux/amd64"}, Files: map[string]string{"--iidfile": "sha256:" + amd.hex}},
				rule{Tool: "docker", Contains: []string{"inspect", amd.hex}, Stdout: amd.fields("amd64")},
				rule{Tool: "docker", Contains: []string{"inspect", workTag("amd64")}, Stdout: amd.fields("amd64")},
				rule{Tool: "docker", Contains: []string{"inspect", registry + ":0.7.0-412-" + arm.id12()}, Stdout: arm.fields("arm64", registry+"@sha256:"+arm.hex)},
				rule{Tool: "docker", Contains: []string{"--rm", "version"}, Stdout: versionLine(imgCommit, "amd64")})
			rules = append(rules, imgEngine("docker", "amd64", "arm64", arm)...)
			r.rules(rules...)
			res := r.image("KMS=local\nIMAGE_SOURCE=source\nDEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY="+registry+"\n", code, false)
			res.want(t, 0, "resolved "+registry+"@sha256:"+arm.hex+" arm64 native "+amd.ref(), "made for this machine's engine (linux/amd64)")
			b := builds(res)
			if len(b) != 2 || valueOf(b[0], "--platform") != "linux/arm64" || valueOf(b[1], "--platform") != "linux/amd64" {
				t.Fatalf("the builds are %v", b)
			}
			if run := callsOf(res, "docker", "version", amd.hex); len(run) != 1 {
				t.Errorf("the native image's version line is not read: %v", run)
			}
		})
		t.Run("built on BUILD_HOST, dev-init here", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			f := newImgFix(imgCommit)
			r.imageBinaries(imgCommit, "amd64")
			r.rules(append(gitRules(""), imgEngine("docker", "amd64", "amd64", f)...)...)
			res := r.image("KMS=local\nDEPLOY_TARGET=dd\nBUILD_HOST=builder@build.example.test\n", code, false)
			res.want(t, 0, "native "+f.ref(), "made for this machine's engine")
			b := builds(res)
			if len(b) != 2 || b[1].Env["DOCKER_HOST"] != "" || valueOf(b[1], "--builder") != "default" || !slices.Equal(b[1].Args[:2], []string{"--context", "default"}) {
				t.Fatalf("the native build is not on the engine here: %v", b)
			}
			if len(callsOf(res, "docker", "load")) != 1 {
				t.Errorf("the delivery after image_native is not the run's: %v", res.calls)
			}
		})
		t.Run("before image_make", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			r.image("KMS=local\nDEPLOY_TARGET=dd\n", "image_native", false).want(t, 1, "internal: image_native before image_make")
		})
	})
}

// TestK3sImportFirstWord: the first word of K3S_IMPORT is the program: env
// would take one with "=" for a variable to set, and ssh is what K3S_SSH is
// for, with its host key checked.
func TestK3sImportFirstWord(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range []struct{ value, want string }{
			{"PATH=/tmp k3s ctr images import -", "a program's name holds no '='"},
			{"ssh admin@k3s.example.test k3s ctr -n k8s.io images import -", "a k3s server reached over ssh is K3S_SSH"},
			{"/usr/bin/ssh admin@k3s.example.test k3s ctr -n k8s.io images import -", "a k3s server reached over ssh is K3S_SSH"},
			{"sudo ssh admin@k3s.example.test k3s ctr -n k8s.io images import -", "runs 'ssh': a k3s server reached over ssh is K3S_SSH"},
			{"env /usr/bin/ssh admin@k3s.example.test k3s ctr -n k8s.io images import -", "runs '/usr/bin/ssh'"},
		} {
			res := r.withEnv("KMS=local\nK3S_IMPORT="+c.value+"\n", "--docker", "k3s", "--status", "--dry-run")
			res.want(t, 2, "K3S_IMPORT="+c.value+": ", c.want)
			if len(res.calls) != 0 {
				t.Errorf("asked %v", res.calls)
			}
		}
	})
}

// TestImageLoadComparesConfig: after a load the copy on the target must
// have the configuration of the image sent, not only its platform, labels
// and layers: an image FROM it that only sets USER 0, an environment or a
// command has those three the same.
func TestImageLoadComparesConfig(t *testing.T) {
	cases := []struct {
		name, config string
		code         int
	}{
		{"the same", imgConfig, 0},
		{"another user", strings.Replace(imgConfig, `"65532:65532"`, `"0"`, 1), 1},
		{"another environment", strings.Replace(imgConfig, `["PATH=/usr/local/bin:/usr/bin"]`, `["PATH=/usr/local/bin:/usr/bin","LD_PRELOAD=/x.so"]`, 1), 1},
		{"another command", strings.Replace(imgConfig, `["server"]`, `["sh"]`, 1), 1},
		{"another entrypoint", strings.Replace(imgConfig, `["/cactusfs"]`, `["/bin/sh","-c"]`, 1), 1},
		{"another volume", strings.Replace(imgConfig, ` null`, ` {"/":{}}`, 1), 1},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				r.imageBinaries(imgCommit, "amd64")
				rules := []rule{{Tool: "podman", Contains: []string{"inspect", f.ref()}, Stdout: fieldsConfig(randomHex(), "linux/amd64", imgCommit, "0.7.0-412", f.layers, c.config)}}
				rules = append(rules, gitRules("")...)
				rules = append(rules, imgEngine("docker", "amd64", "amd64", f)...)
				r.rules(append(rules, imgEngine("podman", "amd64", "amd64", f)...)...)
				res := r.image("KMS=local\nDEPLOY_TARGET=podman\nBUILD_HOST=builder@build.example.test\n", resolveAndDeliver, false)
				if c.code == 0 {
					res.want(t, 0, "loaded on podman")
				} else {
					res.want(t, 1, "is not the one sent: it differs in its configuration")
					for _, v := range []string{"LD_PRELOAD", `"0"`, `["sh"]`, "/bin/sh", "65532"} {
						if strings.Contains(res.output(), v) {
							t.Errorf("the refusal prints a value of the configuration, %s:\n%s", v, res.stderr)
						}
					}
				}
			})
		}
	})
}

// TestImageOfflineBasePlatform: with OFFLINE=yes the base image of an image
// made of binaries must be on the building engine for the platform built
// for - asked with "image inspect --platform" when the Docker CLI has it -
// or the run is refused before anything is built (BuildKit would fetch the
// other platform from the registry); image_native asks the same for its
// own platform.
func TestImageOfflineBasePlatform(t *testing.T) {
	const arm = "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=" + registry + "\n"
	helpWith := rule{Tool: "docker", Contains: []string{"inspect", "--help"}, Stdout: "Usage: docker image inspect [OPTIONS] IMAGE\n      --platform string   Inspect a specific platform\n"}
	helpWithout := rule{Tool: "docker", Contains: []string{"inspect", "--help"}, Stdout: "Usage: docker image inspect [OPTIONS] IMAGE\n  -f, --format string\n"}
	cases := []struct {
		name   string
		env    string
		code   string
		rules  func(runtime string) []rule
		exit   int
		want   string
		builds int
	}{
		{
			name: "another platform, asked for", env: arm, code: resolveAndDeliver,
			rules: func(runtime string) []rule {
				return []rule{helpWith, {Tool: "docker", Contains: []string{"inspect", "--platform", "linux/arm64", runtime}, Stdout: "linux/arm64\n"}}
			},
			builds: 1,
		},
		{
			name: "another platform, not there", env: arm, code: resolveAndDeliver,
			rules: func(runtime string) []rule {
				return []rule{helpWith, {Tool: "docker", Contains: []string{"inspect", "--platform", "linux/arm64", runtime}, Stderr: "Error: No such image: platform linux/arm64\n", Exit: 1}}
			},
			exit: 2, want: "has no linux/arm64 variant",
		},
		{
			name: "another platform, a CLI that cannot ask", env: arm, code: resolveAndDeliver,
			rules: func(runtime string) []rule {
				return []rule{helpWithout, {Tool: "docker", Contains: []string{"inspect", runtime, "{{.Os}}/{{.Architecture}}"}, Stdout: "linux/amd64\n"}}
			},
			exit: 2, want: "This docker cannot be asked for another platform of an image",
		},
		{
			name: "podman with a base of another platform", env: "DEPLOY_TARGET=podman\n", code: resolveAndDeliver,
			rules: func(runtime string) []rule {
				return append([]rule{{Tool: "podman", Contains: []string{"inspect", runtime, "{{.Os}}/{{.Architecture}}"}, Stdout: "linux/arm64\n"}},
					imgEngine("podman", "amd64", "amd64", newImgFix(imgCommit))...)
			},
			exit: 2, want: "is for linux/arm64, and the image is built for linux/amd64",
		},
		{
			name: "image_native, its platform not there", env: arm, code: `image_resolve; image_native`,
			rules: func(runtime string) []rule {
				return []rule{
					helpWith,
					{Tool: "docker", Contains: []string{"inspect", "--platform", "linux/arm64", runtime}, Stdout: "linux/arm64\n"},
					{Tool: "docker", Contains: []string{"inspect", "--platform", "linux/amd64", runtime}, Stderr: "Error: No such image\n", Exit: 1},
				}
			},
			exit: 2, want: "has no linux/amd64 variant", builds: 1,
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		runtime := runtimeImage(t)
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				r.imageBinaries(imgCommit, "amd64", "arm64")
				rules := append(c.rules(runtime), gitRules("")...)
				rules = append(rules, nodeRules("kubectl", "arm64")...)
				rules = append(rules, rule{Tool: "docker", Contains: []string{"inspect", registry + ":0.7.0-412-" + f.id12()}, Stdout: f.fields("arm64", registry+"@sha256:"+f.hex)})
				r.rules(append(rules, imgEngine("docker", "amd64", "arm64", f)...)...)
				res := r.image("KMS=local\nOFFLINE=yes\n"+c.env, c.code, false)
				res.want(t, c.exit, c.want)
				if b := builds(res); len(b) != c.builds {
					t.Fatalf("%d builds, want %d: %v", len(b), c.builds, b)
				}
				noPull(t, res)
			})
		}
	})
}

// TestImageCheckChangesNothing: image_check decides and refuses, and only
// reads: no build, tag, removal, pull, push, save, load, import or run of a
// container is asked of any tool; image_make before it is refused.
func TestImageCheckChangesNothing(t *testing.T) {
	changes := []string{"build", "buildx", "tag", "rm", "pull", "push", "save", "load", "import", "run", "create", "cp"}
	const repo = "registry.example.test/cactusfs"
	cases := []struct{ name, env string }{
		{"binaries here", "DEPLOY_TARGET=dd\n"},
		{"source to a server", "DEPLOY_TARGET=ce\nCE_HOST=deploy@server.example.test\nIMAGE_SOURCE=source\n"},
		{"binaries on a build host", "DEPLOY_TARGET=dd\nBUILD_HOST=builder@build.example.test\n"},
		{"k3s over ssh", "DEPLOY_TARGET=k3s\nK3S_SSH=admin@k3s.example.test\n"},
		{"a registry, offline", "DEPLOY_TARGET=k8s\nK8S_CONTEXT=prod\nK8S_REGISTRY=" + registry + "\nOFFLINE=yes\n"},
		{"podman", "DEPLOY_TARGET=podman\n"},
		{"a given image that is here", "DEPLOY_TARGET=dd\nIMAGE_SOURCE=image\nIMAGE=" + repo + "@sha256:" + strings.Repeat("a", 64) + "\n"},
		{"a given image that is not here", "DEPLOY_TARGET=dd\nIMAGE_SOURCE=image\nIMAGE=" + repo + ":0.7.0\n"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		runtime := runtimeImage(t)
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				r.imageBinaries(imgCommit, "amd64")
				rules := []rule{
					{Tool: "docker", Contains: []string{"{{.Id}}", repo + ":0.7.0"}, Stderr: "Error: No such image\n", Exit: 1},
					{Tool: "docker", Contains: []string{"inspect", repo + "@sha256:" + strings.Repeat("a", 64)}, Stdout: f.fields("amd64")},
					{Tool: "docker", Contains: []string{"inspect", runtime, "{{.Os}}/{{.Architecture}}"}, Stdout: "linux/amd64\n"},
				}
				rules = append(rules, gitRules("")...)
				for _, tool := range []string{"kubectl", "ssh"} {
					rules = append(rules, nodeRules(tool, "amd64")...)
				}
				rules = append(rules, imgEngine("docker", "amd64", "amd64", f)...)
				r.rules(append(rules, imgEngine("podman", "amd64", "amd64", f)...)...)
				res := r.image("KMS=local\n"+c.env, `image_check; echo "checked $RESOLVED_ARCH"`, false)
				res.want(t, 0, "checked amd64")
				if len(res.calls) == 0 {
					t.Fatal("nothing was asked, so this proves nothing")
				}
				for _, call := range res.calls {
					for _, w := range changes {
						if slices.Contains(call.Args, w) {
							t.Errorf("image_check ran %s %q", call.Tool, call.Args)
						}
					}
				}
			})
		}
		r.image("KMS=local\nDEPLOY_TARGET=dd\n", "image_make", false).want(t, 1, "internal: image_make before image_check")
	})
}

// TestImageBuildIDs: buildx writes the configuration's digest to --iidfile
// and the manifest's to --metadata-file; Docker's containerd store knows
// the image by the manifest's. The ID the engine knows is taken, and the
// build tag must name it; an engine that knows neither, and a build tag
// that names another image, fail the run before anything is tagged.
func TestImageBuildIDs(t *testing.T) {
	metadata := func(config, manifest string) string {
		return "{\n  \"buildx.build.ref\": \"default/default/x\",\n  \"containerimage.config.digest\": \"sha256:" + config + "\",\n  \"containerimage.digest\": \"sha256:" + manifest + "\"\n}"
	}
	absent := func(id string) rule {
		return rule{Tool: "docker", Contains: []string{"{{.Id}}", id}, Stderr: "Error response from daemon: No such image: sha256:" + id + "\n", Exit: 1}
	}
	cases := []struct {
		name  string
		rules func(config, manifest string) []rule
		tagID func(config, manifest string) string
		code  int
		want  string
	}{
		{
			name:  "the image store: the configuration's digest",
			rules: func(string, string) []rule { return nil },
			tagID: func(config, _ string) string { return config },
		},
		{
			name:  "the containerd store: the manifest's digest",
			rules: func(config, _ string) []rule { return []rule{absent(config)} },
			tagID: func(_, manifest string) string { return manifest },
		},
		{
			name:  "neither known, the build tag names the manifest",
			rules: func(config, manifest string) []rule { return []rule{absent(config), absent(manifest)} },
			tagID: func(_, manifest string) string { return manifest },
		},
		{
			name:  "neither known, the build tag names another image",
			rules: func(config, manifest string) []rule { return []rule{absent(config), absent(manifest)} },
			tagID: func(string, string) string { return randomHex() },
			code:  1, want: "the engine knows the image built by neither of its IDs",
		},
		{
			name:  "the build tag names another image",
			rules: func(string, string) []rule { return nil },
			tagID: func(string, string) string { return randomHex() },
			code:  1, want: "not the image built",
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				config, manifest := randomHex(), randomHex()
				f := newImgFix(imgCommit)
				tagged := c.tagID(config, manifest)
				r.imageBinaries(imgCommit, "amd64")
				rules := c.rules(config, manifest)
				rules = append(rules,
					rule{Tool: "docker", Contains: []string{"build"}, Files: map[string]string{"--iidfile": "sha256:" + config, "--metadata-file": metadata(config, manifest)}},
					rule{Tool: "docker", Contains: []string{"inspect", workTag("amd64")}, Stdout: fieldsLine(tagged, "linux/amd64", imgCommit, "0.7.0-412", f.layers)})
				r.rules(append(rules, imgEngine("docker", "amd64", "amd64", f)...)...)
				res := r.image("KMS=local\nDEPLOY_TARGET=dd\n", resolveAndDeliver, false)
				res.want(t, c.code, c.want)
				tags := callsOf(res, "docker", "image", "tag")
				if c.code != 0 {
					if len(tags) != 0 {
						t.Fatalf("tagged after all: %v", tags)
					}
					return
				}
				ref := "cactusfs:0.7.0-412-" + tagged[:12]
				if len(tags) != 1 || !slices.Equal(argsAfter(tags[0], "tag"), []string{"tag", tagged, ref}) {
					t.Fatalf("the tag is %v, want the ID %s", tags, tagged)
				}
				if !strings.Contains(res.stdout, "(configuration "+config+")") {
					t.Errorf("the configuration's digest is not kept:\n%s", res.stdout)
				}
			})
		}
	})
}

// TestImageFieldsWhole: an inspect that answers more than one line, or with
// a control character - a label that holds one - fails the run; no part of
// it is taken.
func TestImageFieldsWhole(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		for _, bad := range []string{"0.7.0-412\nevil", "0.7.0-412\x01", "0.7.0-412\r"} {
			f := newImgFix(imgCommit)
			r.imageBinaries(imgCommit, "amd64")
			r.rules(append([]rule{{Tool: "docker", Contains: []string{"inspect", workTag("amd64")}, Stdout: fieldsLine(f.hex, "linux/amd64", imgCommit, bad, f.layers)}},
				imgEngine("docker", "amd64", "amd64", f)...)...)
			res := r.image("KMS=local\nDEPLOY_TARGET=dd\n", resolveAndDeliver, false)
			res.want(t, 1, "is more than one line, or holds a control character")
			if len(callsOf(res, "docker", "image", "tag")) != 0 {
				t.Fatalf("%q: tagged after all", bad)
			}
		}
	})
}

// TestImageImportVerified: after an import the node's image ID (crictl
// inspecti, its configuration's digest) must be the configuration of the
// tar sent, and that the configuration built; an import through
// K3S_IMPORT's own words is said to be unchecked.
func TestImageImportVerified(t *testing.T) {
	const ssh = "DEPLOY_TARGET=k3s\nK3S_SSH=admin@k3s.example.test\n"
	cases := []struct {
		name  string
		env   string
		rules func(f imgFix) []rule
		code  int
		want  string
		check func(t *testing.T, f imgFix, res result)
	}{
		{
			name: "over ssh", env: ssh, want: "the node holds",
			check: func(t *testing.T, f imgFix, res result) {
				q := callsOf(res, "ssh", "crictl")
				if len(q) != 1 || !slices.Equal(argsAfter(q[0], "k3s"), []string{"k3s", "crictl", "inspecti", "-q", "-o", "json", f.ref()}) {
					t.Fatalf("the node is asked %v", q)
				}
				if callIndex(res, "ssh", "crictl") < callIndex(res, "ssh", "import") {
					t.Errorf("the node is asked before the import")
				}
			},
		},
		{
			name: "here", env: "DEPLOY_TARGET=k3s\n", want: "the node holds",
			check: func(t *testing.T, f imgFix, res result) {
				q := callsOf(res, "k3s", "crictl")
				if len(q) != 1 || !slices.Equal(q[0].Args, []string{"crictl", "inspecti", "-q", "-o", "json", f.ref()}) {
					t.Fatalf("the node is asked %v", q)
				}
			},
		},
		{
			name: "a tar of Docker's own layout", env: ssh, want: "the node holds",
			rules: func(f imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"save"}, Stdout: savedTar(f.hex+".json", f.layers)}}
			},
		},
		{
			name: "the node holds another image", env: ssh, code: 1, want: "not the configuration sent",
			rules: func(imgFix) []rule { return []rule{crictlOf("ssh", randomHex())} },
		},
		{
			name: "the node holds nothing", env: ssh, code: 1, want: "asking the node for",
			rules: func(imgFix) []rule {
				return []rule{{Tool: "ssh", Contains: []string{"crictl"}, Stderr: "no such image\n", Exit: 1}}
			},
		},
		{
			name: "the tar holds another image", env: ssh, code: 1, want: "not the one built, ",
			rules: func(f imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"save"}, Stdout: savedTar("blobs/sha256/"+randomHex(), f.layers)}}
			},
			check: func(t *testing.T, _ imgFix, res result) {
				if len(callsOf(res, "ssh", "import")) != 0 {
					t.Errorf("a tar of another image was imported")
				}
			},
		},
		{
			name: "the tar holds other layers", env: ssh, code: 1, want: "holds other layers than the image checked, in each of its 1 images: nothing was sent",
			rules: func(f imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"save"}, Stdout: savedTar("blobs/sha256/"+f.hex, "sha256:"+randomHex()+" ")}}
			},
			check: func(t *testing.T, _ imgFix, res result) {
				if len(callsOf(res, "ssh", "import")) != 0 {
					t.Errorf("a tar of other layers was imported")
				}
			},
		},
		{
			name: "several platforms, the one of the image taken", env: ssh, want: "the node holds",
			rules: func(f imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"save"}, Stdout: savedTarOf(
					tarEntry{"blobs/sha256/" + randomHex(), "sha256:" + randomHex() + " "},
					tarEntry{"blobs/sha256/" + f.hex, f.layers},
					tarEntry{"blobs/sha256/" + randomHex(), "sha256:" + randomHex() + " sha256:" + randomHex()})}}
			},
		},
		{
			name: "several platforms, none of the image", env: ssh, code: 1,
			want: "holds other layers than the image checked, in each of its 2 images: nothing was sent",
			rules: func(imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"save"}, Stdout: savedTarOf(
					tarEntry{"blobs/sha256/" + randomHex(), "sha256:" + randomHex() + " "},
					tarEntry{"blobs/sha256/" + randomHex(), "sha256:" + randomHex() + " "})}}
			},
			check: func(t *testing.T, _ imgFix, res result) {
				if len(callsOf(res, "ssh", "import")) != 0 {
					t.Errorf("a tar of none of the image's layers was imported")
				}
			},
		},
		{
			name: "a configuration named outside the tar's forms", env: ssh, code: 1,
			want: "names the configuration '../",
			rules: func(f imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"save"}, Stdout: savedTarOf(tarEntry{"../" + f.hex + ".json", f.layers})}}
			},
		},
		{
			name: "a configuration named by a short digest", env: ssh, code: 1,
			want: "names the configuration 'blobs/sha256/abc'",
			rules: func(f imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"save"}, Stdout: savedTarOf(tarEntry{"blobs/sha256/abc", f.layers})}}
			},
		},
		{
			name: "a configuration named by both forms", env: ssh, code: 1,
			want: "names the configuration 'blobs/sha256/",
			rules: func(f imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"save"}, Stdout: savedTarOf(tarEntry{"blobs/sha256/" + f.hex + ".json", f.layers})}}
			},
		},
		{
			name: "no configuration named", env: ssh, code: 1,
			want: "names the configuration ''",
			rules: func(f imgFix) []rule {
				return []rule{{Tool: "docker", Contains: []string{"save"}, Stdout: savedTarOf(tarEntry{"", f.layers})}}
			},
		},
		{
			name: "a tar without a manifest", env: ssh, code: 1, want: "saved without a manifest.json",
			rules: func(imgFix) []rule { return []rule{{Tool: "docker", Contains: []string{"save"}, Stdout: "not a tar"}} },
		},
		{
			name: "K3S_IMPORT", env: "DEPLOY_TARGET=k3s\nK3S_IMPORT=docker exec -i k3s-bench k3s ctr -n k8s.io images import -\n",
			want: "imported by K3S_IMPORT's own command, on a node the script cannot ask",
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				r.imageBinaries(imgCommit, "amd64")
				var rules []rule
				if c.rules != nil {
					rules = c.rules(f)
				}
				rules = append(rules, gitRules("")...)
				for _, tool := range []string{"k3s", "ssh"} {
					rules = append(rules, nodeRules(tool, "amd64")...)
				}
				r.rules(append(rules, imgEngine("docker", "amd64", "amd64", f)...)...)
				res := r.image("KMS=local\nALLOW_DIRTY=no\n"+c.env, resolveAndDeliver, false)
				res.want(t, c.code, c.want)
				if c.check != nil {
					c.check(t, f, res)
				}
			})
		}
	})
}

// TestImageNativeOnlyBuilt: image_native is for IMAGE_SOURCE=binaries and
// source; with IMAGE_SOURCE=image dev-init runs IMAGE itself.
func TestImageNativeOnlyBuilt(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		f := newImgFix(imgCommit)
		image := registry + "@sha256:" + f.hex
		r.rules(append([]rule{{Tool: "docker", Contains: []string{"inspect", image}, Stdout: f.fields("amd64", image)}},
			imgEngine("docker", "amd64", "amd64", f)...)...)
		res := r.image("KMS=local\nDEPLOY_TARGET=dd\nIMAGE_SOURCE=image\nIMAGE="+image+"\n", `image_resolve; image_native`, false)
		res.want(t, 1, "image_native is for IMAGE_SOURCE=binaries and source; with IMAGE_SOURCE=image, dev-init runs IMAGE itself")
	})
}

// TestImageLoopback: the API server of a desktop's context is on this
// machine only by a loopback address in dotted quads, [::1], localhost or
// kubernetes.docker.internal - never by a name that begins like one.
func TestImageLoopback(t *testing.T) {
	cases := []struct {
		server string
		local  bool
	}{
		{"https://127.0.0.1:6443", true},
		{"https://127.0.0.1", true},
		{"https://127.10.20.30:6443/", true},
		{"https://[::1]:6443", true},
		{"https://localhost:6443", true},
		{"https://kubernetes.docker.internal:6443", true},
		{"https://127.example.test:6443", false},
		{"https://127.0.0.1.example.test:6443", false},
		{"https://127.0.0.01:6443", false},
		{"https://127.0.0.256:6443", false},
		{"https://127.0.0:6443", false},
		{"https://localhost.example.test:6443", false},
		{"https://128.0.0.1:6443", false},
		{"http://127.0.0.1:6443", false},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.server, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				r.imageBinaries(imgCommit, "amd64")
				r.rules(append(append([]rule{kubeServer(c.server)}, nodeRules("kubectl", "amd64")...), imgEngine("docker", "amd64", "amd64", f)...)...)
				res := r.image("KMS=local\nDEPLOY_TARGET=k8s\nK8S_CONTEXT=docker-desktop\n", `image_check; echo "checked $RESOLVED_ARCH"`, false)
				if c.local {
					res.want(t, 0, "checked amd64")
				} else {
					res.want(t, 2, "which is not on this machine")
				}
			})
		}
	})
}

// TestImageLoadNamesTheParts: a copy that differs after a load is named by
// the parts that differ - platform, labels, layers - never by their values.
func TestImageLoadNamesTheParts(t *testing.T) {
	cases := []struct {
		name string
		line func(f imgFix) string
		want string
	}{
		{"platform", func(f imgFix) string { return fieldsLine(randomHex(), "linux/arm64", imgCommit, "0.7.0-412", f.layers) }, "differs in its platform"},
		{"revision", func(f imgFix) string {
			return fieldsLine(randomHex(), "linux/amd64", "fedcba987654", "0.7.0-412", f.layers)
		}, "differs in its revision label"},
		{"layers and version", func(imgFix) string {
			return fieldsLine(randomHex(), "linux/amd64", imgCommit, "0.7.0-413", "sha256:"+randomHex()+" ")
		}, "differs in its version label, layers"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				f := newImgFix(imgCommit)
				r.imageBinaries(imgCommit, "amd64")
				rules := append([]rule{{Tool: "podman", Contains: []string{"inspect", f.ref()}, Stdout: c.line(f)}}, gitRules("")...)
				rules = append(rules, imgEngine("docker", "amd64", "amd64", f)...)
				r.rules(append(rules, imgEngine("podman", "amd64", "amd64", f)...)...)
				res := r.image("KMS=local\nDEPLOY_TARGET=podman\nBUILD_HOST=builder@build.example.test\nALLOW_DIRTY=yes\n", resolveAndDeliver, false)
				res.want(t, 1, c.want)
				if strings.Contains(res.stderr, f.layers) || strings.Contains(res.stderr, "fedcba987654") {
					t.Errorf("the refusal prints a value:\n%s", res.stderr)
				}
			})
		}
	})
}

// TestImageGivenAfterPull: a given image that was not here is pulled and
// only then inspected; what is not taken then fails the run (exit 1:
// something has changed), and does not refuse it (exit 2, "nothing
// changed"). A given image on the import route is checked by its layers
// before it is sent (its configuration's digest is not known here).
func TestImageGivenAfterPull(t *testing.T) {
	const repo = "registry.example.test/cactusfs"
	eachShell(t, func(t *testing.T, r *rig) {
		f := newImgFix(imgCommit)
		image := repo + ":0.7.0"
		r.rules(
			rule{Tool: "docker", Contains: []string{"{{.Id}}", image}, Stderr: "Error: No such image\n", Exit: 1},
			rule{Tool: "docker", Contains: []string{"inspect", image}, Stdout: f.fields("arm64", repo+"@sha256:"+f.hex)},
			rule{Tool: "docker", Contains: []string{"context", "inspect"}, Stdout: "unix:///var/run/docker.sock\n"},
			rule{Tool: "docker", Contains: []string{"info"}, Stdout: "linux/x86_64\n"})
		res := r.image("KMS=local\nDEPLOY_TARGET=dd\nIMAGE_SOURCE=image\nIMAGE="+image+"\n", resolveAndDeliver, false)
		res.want(t, 1, "is for linux/arm64, and the target is linux/amd64 (said after the image was pulled)")
		if len(callsOf(res, "docker", "pull")) != 1 {
			t.Fatalf("not pulled: %v", res.calls)
		}

		g := newImgFix(imgCommit)
		digest := repo + "@sha256:" + g.hex
		r.rules(append([]rule{
			{Tool: "docker", Contains: []string{"inspect", digest}, Stdout: g.fields("amd64", digest)},
			{Tool: "docker", Contains: []string{"save"}, Stdout: savedTar("blobs/sha256/"+randomHex(), "sha256:"+randomHex()+" ")},
		}, append(nodeRules("ssh", "amd64"), imgEngine("docker", "amd64", "amd64", g)...)...)...)
		res = r.image("KMS=local\nDEPLOY_TARGET=k3s\nK3S_SSH=admin@k3s.example.test\nIMAGE_SOURCE=image\nIMAGE="+digest+"\n", resolveAndDeliver, false)
		res.want(t, 1, "holds other layers than the image checked, in each of its 1 images: nothing was sent")
		if len(callsOf(res, "ssh", "import")) != 0 {
			t.Errorf("a tar of other layers was imported")
		}
	})
}
