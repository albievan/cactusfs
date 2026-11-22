//go:build deployscript

package deploy_test

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The secrets of a deployment (PLAN 10.3, deploy/lib/secrets.sh): made once,
// kept in the deployment's entry of STATE_DIR and on the target, never on a
// command line, in an environment or in any output.

// engineID is what the fake engines say their ID is.
const engineID = "ENGINE-ONE"

// secretsRig is a rig with what an install needs beside deploy/: the
// Dockerfile (it pins the runtime image) and the binaries of make
// image-binaries for this machine's architecture.
func secretsRig(t *testing.T, r *rig) *rig {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.repo, "Dockerfile"), repoFile(t, "Dockerfile"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.binaries([]byte("a cactusfs binary " + rand.Text()))
	return r
}

// binaries writes dist/image of the rig's repository: the binary of this
// machine's architecture, SHA256SUMS and BUILDINFO.
func (r *rig) binaries(binary []byte) {
	r.t.Helper()
	dir := filepath.Join(r.repo, "dist", "image")
	name := "linux-" + runtime.GOARCH + "/cactusfs"
	if err := os.MkdirAll(filepath.Join(dir, "linux-"+runtime.GOARCH), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), binary, 0o600); err != nil {
		r.t.Fatal(err)
	}
	sum := sha256.Sum256(binary)
	files := map[string]string{
		"SHA256SUMS": hex.EncodeToString(sum[:]) + "  " + name + "\n",
		"BUILDINFO":  "version=0.7.0\nbuild=1\ncommit=abc\npackaging=image\n",
	}
	for f, content := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(content), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
}

// keepStdin makes the fakes keep what each call reads (stdinOf).
func (r *rig) keepStdin() {
	r.t.Helper()
	if err := os.Mkdir(filepath.Join(r.dir, stdinDir), 0o700); err != nil {
		r.t.Fatal(err)
	}
}

// tarNames is the names of the entries of the tar stream a call read, as
// a clean path each ("." for the directory itself), in their order.
func (r *rig) tarNames(c call) []string {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, stdinDir, c.Stdin))
	if err != nil {
		r.t.Fatalf("what %q read was not kept: %v", c.Args, err)
	}
	var names []string
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			r.t.Fatal(err)
		}
		names = append(names, path.Clean(h.Name))
	}
}

// runtimeImage is the RUNTIME_IMAGE the Dockerfile pins.
func runtimeImage(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^ARG RUNTIME_IMAGE=(\S+@sha256:[0-9a-f]{64})$`).FindSubmatch(repoFile(t, "Dockerfile"))
	if m == nil {
		t.Fatal("the Dockerfile pins no RUNTIME_IMAGE")
	}
	return string(m[1])
}

// devInitOutput is what the fake dev-init writes: the files of
// "cactusfs dev-init" for the standalone node "cactusfs", each a value no
// output could hold by chance.
func devInitOutput() map[string]string {
	out := map[string]string{}
	for _, f := range []string{"kms.json", "s3.pem", "s3.key", "s3-ca.pem", "node-ca.pem", "nodes/cactusfs.pem", "nodes/cactusfs.key", "ready"} {
		out[f] = "fixture " + f + " " + rand.Text() + "\n"
	}
	return out
}

// tarOf is a tar stream as "docker cp <container>:<dir> -" writes one: the
// directory under its own name, then what it holds.
func tarOf(t *testing.T, top string, files map[string]string) string {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	dirs := map[string]bool{top: true}
	for name := range files {
		if d := filepath.Dir(name); d != "." {
			dirs[top+"/"+d] = true
		}
	}
	for _, d := range slices.Sorted(maps.Keys(dirs)) {
		if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o700, Uid: 65532, Gid: 65532}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		data := files[name]
		if err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: top + "/" + name, Mode: 0o600, Size: int64(len(data)), Uid: 65532, Gid: 65532}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// tarWithLink is a tar stream of the directory top holding a symbolic link
// named name.
func tarWithLink(t *testing.T, top, name string) string {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, h := range []*tar.Header{
		{Typeflag: tar.TypeDir, Name: top + "/", Mode: 0o700},
		{Typeflag: tar.TypeSymlink, Name: top + "/" + name, Linkname: "/etc/hosts", Mode: 0o777},
	} {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// fileOf is what the file holds, read within its own directory.
func fileOf(t *testing.T, path string) string {
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

// filesUnder is every file under dir, by its path relative to it, with what
// it holds.
func filesUnder(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(root.FS(), rel)
		out[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// engineRules are the answers of an engine (docker or podman) that says its
// ID is id, whose volumes of the deployment hold the units given (none:
// there are no volumes), and whose dev-init writes devInit.
func engineRules(t *testing.T, tool, id string, devInit map[string]string, volumes map[string]map[string]string) []rule {
	t.Helper()
	rules := []rule{
		// Putting a unit into a volume prints nothing; it must not match
		// the rule that reads the volume, below.
		{Tool: tool, Contains: []string{"cp", "-a"}},
		{Tool: tool, Contains: []string{"info"}, Stdout: id + "\n"},
		{Tool: tool, Contains: []string{"cp", "cactusfs-dev-init:/tmp/out", "-"}, Stdout: tarOf(t, "out", devInit)},
	}
	// One listing names every volume there is (none: the fakes print
	// nothing), one inspect gives each its label.
	var listed, labels string
	for _, unit := range []string{"secrets", "config"} {
		files, ok := volumes[unit]
		if !ok {
			continue
		}
		listed += "cactusfs-" + unit + "\n"
		labels += "cactusfs-" + unit + " cactusfs\n"
		rules = append(rules, rule{Tool: tool, Contains: []string{"cp", "cactusfs-volumes:/" + unit, "-"}, Stdout: tarOf(t, unit, files)})
	}
	return append(rules,
		rule{Tool: tool, Contains: []string{"volume", "ls", "name=cactusfs-"}, Stdout: listed},
		rule{Tool: tool, Contains: []string{"volume", "inspect"}, Stdout: labels})
}

// dockerRules are the answers of a Docker engine with the ID engineID.
func dockerRules(t *testing.T, devInit map[string]string, volumes map[string]map[string]string) []rule {
	t.Helper()
	return engineRules(t, "docker", engineID, devInit, volumes)
}

// stateOf is the deployment's entry of STATE_DIR in the rig, for the
// target ID given.
func (r *rig) stateOf(targetID string) string {
	return filepath.Join(r.home, ".local", "state", "cactusfs-deploy", targetID, "cactusfs")
}

// stateDir is the rig's STATE_DIR.
func (r *rig) stateDir() string {
	return filepath.Join(r.home, ".local", "state", "cactusfs-deploy")
}

// withTarget is the volumes of a target that holds what the entry holds.
func withTarget(t *testing.T, state string) map[string]map[string]string {
	t.Helper()
	return map[string]map[string]string{
		"secrets": filesUnder(t, filepath.Join(state, "secrets")),
		"config":  filesUnder(t, filepath.Join(state, "config")),
	}
}

// callsOf is the calls of the tool that hold every word.
func callsOf(res result, tool string, words ...string) []call {
	var out []call
	for _, c := range res.calls {
		if c.Tool == tool && containsAll(c.Args, words) {
			out = append(out, c)
		}
	}
	return out
}

// containsRun is true when the words are in args one after another.
func containsRun(args, words []string) bool {
	for i := 0; i+len(words) <= len(args); i++ {
		if slices.Equal(args[i:i+len(words)], words) {
			return true
		}
	}
	return false
}

// noSecretAnywhere fails the test when a secret - every file of the
// deployment's entry but its record, and every file given - is in an
// argument or the environment of any call, or in the output.
func noSecretAnywhere(t *testing.T, res result, state string, given ...string) {
	t.Helper()
	values := map[string]string{}
	for _, unit := range []string{"secrets", "config"} {
		if _, err := os.Stat(filepath.Join(state, unit)); err != nil {
			continue
		}
		for name, data := range filesUnder(t, filepath.Join(state, unit)) {
			if v := strings.TrimSpace(data); len(v) >= 16 {
				values[unit+"/"+name] = v
			}
		}
	}
	for _, path := range given {
		values[path] = strings.TrimSpace(fileOf(t, path))
	}
	if len(values) == 0 {
		t.Fatal("no secret to look for, so this proves nothing")
	}
	for name, v := range values {
		if strings.Contains(res.output(), v) {
			t.Errorf("the secret of %s is in the output", name)
		}
		for _, c := range res.calls {
			for _, a := range c.Args {
				if strings.Contains(a, v) {
					t.Errorf("the secret of %s is on the command line of %s %q", name, c.Tool, c.Args[:min(len(c.Args), 4)])
				}
			}
			for k, e := range c.Env {
				if strings.Contains(e, v) {
					t.Errorf("the secret of %s is in the environment of %s, %s", name, c.Tool, k)
				}
			}
		}
	}
}

// modesAre fails the test unless every directory under dir, dir included,
// is 0700 and every file 0600, and nothing of a run is left there.
func modesAre(t *testing.T, dir string) {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %o, want %o", path, info.Mode().Perm(), want)
		}
		if strings.HasPrefix(d.Name(), ".work.") || strings.HasPrefix(d.Name(), ".new.") {
			t.Errorf("%s is left over from the run", path)
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 10 {
		t.Fatalf("%d entries under %s; nothing was made", n, dir)
	}
}

// entryPath matches the rig's paths a run may write: the entry of a
// deployment named cactusfs in STATE_DIR (on any target ID), what it
// holds, and the directories above it.
var entryPath = regexp.MustCompile(`^home/\.local(/state(/cactusfs-deploy(/[^/]+(/cactusfs(/.*)?)?)?)?)?$`)

// outsideEntry is every file and directory of the rig but the deployment's
// entry (and the fakes' own files), with a hash of each file: the files
// given, the repository and its binaries, every other directory.
func (r *rig) outsideEntry() map[string]string {
	r.t.Helper()
	all := tree(r.t, r.dir)
	for k := range all {
		if entryPath.MatchString(k) || k == stdinDir || strings.HasPrefix(k, stdinDir+"/") {
			delete(all, k)
		}
	}
	return all
}

// guardedRun runs the script as run does, and fails the test when anything
// outside the deployment's entry changed - a file given, the repository, a
// directory of the rig - whether the run finished or was refused.
func (r *rig) guardedRun(args []string, extraEnv ...string) result {
	r.t.Helper()
	before := r.outsideEntry()
	res := r.run(args, extraEnv...)
	after := r.outsideEntry()
	for k, v := range before {
		if after[k] != v {
			r.t.Errorf("the run (exit %d) changed %s, which is outside the deployment's entry", res.code, k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			r.t.Errorf("the run (exit %d) made %s, which is outside the deployment's entry", res.code, k)
		}
	}
	return res
}

// guarded writes the env file site.env and runs the script with --env and
// the arguments, as withEnv does, under guardedRun.
func (r *rig) guarded(content string, args ...string) result {
	r.t.Helper()
	path := r.envFile("site.env", content)
	return r.guardedRun(append([]string{"--env", path}, args...))
}

// installDD is the arguments of an install on Docker Desktop.
var installDD = []string{"--docker", "dd", "--install"}

// installed is a rig of the shell of r in which a first install with the
// env file has run: the base the cases of a test copy (clone), so that
// each does not make its own.
func installed(t *testing.T, r *rig, env string) *rig {
	t.Helper()
	base := secretsRig(t, r.fresh(t))
	base.rules(dockerRules(t, devInitOutput(), nil)...)
	base.guarded(env, installDD...).want(t, 3, "not built yet (PLAN 10.5)")
	return base
}

// clone is a new rig with a copy of the STATE_DIR of base.
func clone(t *testing.T, base *rig) *rig {
	t.Helper()
	r := secretsRig(t, base.fresh(t))
	from := filepath.Join(base.home, ".local")
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		to := filepath.Join(r.home, ".local", rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o700)
		}
		return os.WriteFile(to, []byte(fileOf(t, p)), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestSecretsAreMadeOnce: a first install makes every secret into the
// entry of STATE_DIR - the root credentials and the passphrase from
// /dev/urandom, the certificates and kms.json by dev-init in a container of
// this machine's architecture - records the engine, and puts them into the
// labelled volumes before anything starts, naming exactly what it puts; a
// second install makes nothing again and puts nothing; no secret is on any
// command line, in any environment or output, a traced run's included; and
// the first installs under the three shells share no generated secret.
func TestSecretsAreMadeOnce(t *testing.T) {
	t.Parallel()
	env := "KMS=local\nTLS_HOSTS=s3.example.test,192.0.2.10\nTLS_VALIDITY=720h\n"
	var mu sync.Mutex
	var made []map[string]string
	t.Cleanup(func() {
		// After every shell's subtest: two deployments never share a
		// generated secret.
		for i := range made {
			for j := i + 1; j < len(made); j++ {
				for name, v := range made[i] {
					if made[j][name] == v {
						t.Errorf("%s is the same in two deployments", name)
					}
				}
			}
		}
	})
	eachShell(t, func(t *testing.T, r *rig) {
		r = secretsRig(t, r)
		r.keepStdin()
		devInit := devInitOutput()
		r.rules(dockerRules(t, devInit, nil)...)
		r.trace = true
		res := r.guarded(env, installDD...)
		r.trace = false
		state := r.stateOf("dd-default")
		mu.Lock()
		made = append(made, filesUnder(t, filepath.Join(state, "secrets")))
		mu.Unlock()
		res.want(t, 3, "not built yet (PLAN 10.5)", "engine ID: "+engineID, "made the root credentials", "made the KMS passphrase", "made "+state+"/config")
		if !strings.Contains(res.stderr, "+ ") {
			t.Fatalf("the run was not traced:\n%s", res.stderr)
		}
		got := filesUnder(t, state)

		// The record: the settings' engine, which 10.1's check reads, the
		// engine's ID and the KMS.
		if want := "docker --context default\nengine-id=" + engineID + "\nkms=local\n"; got["target"] != want {
			t.Errorf("STATE/target = %q, want %q", got["target"], want)
		}
		for _, f := range []struct{ file, form string }{
			{"secrets/root-access-key", `^[A-Z2-7]{20}$`},
			{"secrets/root-secret-key", `^[A-Za-z0-9]{40}$`},
			{"secrets/kms-passphrase", `^[A-Za-z0-9]{48}$`},
		} {
			if !regexp.MustCompile(f.form).MatchString(got[f.file]) {
				t.Errorf("%s is not of the form %s (%d bytes)", f.file, f.form, len(got[f.file]))
			}
		}
		for name, data := range devInit {
			if got["config/"+name] != data {
				t.Errorf("config/%s is not what dev-init wrote", name)
			}
		}
		if len(got) != 4+len(devInit) {
			t.Errorf("the entry holds %d files, want %d: %v", len(got), 4+len(devInit), slices.Sorted(maps.Keys(got)))
		}
		modesAre(t, r.stateDir())
		noSecretAnywhere(t, res, state)

		// dev-init: this machine's architecture, the runtime image the
		// Dockerfile pins and the binary copied in, the nodes' user, no
		// pull, no network, no capability, the settings of the env file;
		// its input and output are tar streams.
		creates := callsOf(res, "docker", "create", "--name", "cactusfs-dev-init")
		if len(creates) != 1 {
			t.Fatalf("dev-init's container was made %d times", len(creates))
		}
		for _, words := range [][]string{
			{"--platform", "linux/" + runtime.GOARCH},
			{"--user", "65532:65532"},
			{"--pull", "never"},
			{"--network", "none"},
			{"--cap-drop", "ALL"},
			{"--security-opt", "no-new-privileges"},
			{runtimeImage(t), "/tmp/cactusfs", "dev-init"},
			{"--nodes", "cactusfs"},
			{"--hosts", "s3.example.test,192.0.2.10"},
			{"--validity", "720h"},
			{"--kms-passphrase-file", "/tmp/kms-passphrase"},
			{"--label", "io.cactusfs.deploy.name=cactusfs"},
		} {
			if !containsRun(creates[0].Args, words) {
				t.Errorf("dev-init's container is made with %q, without %q", creates[0].Args, words)
			}
		}
		helpers := callsOf(res, "docker", "create", "--name", "cactusfs-volumes")
		if len(helpers) != 1 || !containsRun(helpers[0].Args, []string{"--user", "65532:65532", "--pull", "never", "--network", "none"}) {
			t.Errorf("the helper of the volumes: %v", helpers)
		}
		in := callsOf(res, "docker", "cp", "-a", "-", "cactusfs-dev-init:/tmp")
		if len(in) != 1 || !slices.Equal(r.tarNames(in[0]), []string{"cactusfs", "kms-passphrase"}) {
			t.Errorf("dev-init's input was not the binary and the passphrase as a tar stream: %v", in)
		}

		// The order: dev-init, then the volumes made and filled with what
		// the entry holds - by name, nothing else; nothing else is started,
		// no container is run, nothing is mounted from this machine.
		var order []string
		for _, c := range res.calls {
			sub := c.Args[2:] // after --context default
			switch {
			case sub[0] == "run":
				t.Errorf("a container was run: %q", c.Args)
			case sub[0] == "start":
				if !slices.Equal(sub, []string{"start", "-a", "cactusfs-dev-init"}) {
					t.Errorf("something other than dev-init was started: %q", c.Args)
				}
				order = append(order, "dev-init")
			case sub[0] == "volume" && sub[1] == "create":
				if !containsAll(sub, []string{"--label", "io.cactusfs.deploy.name=cactusfs", "io.cactusfs.deploy.target=dd"}) {
					t.Errorf("a volume without the deployment's labels: %q", c.Args)
				}
				order = append(order, "volume "+sub[len(sub)-1])
			case sub[0] == "cp" && sub[1] == "-a" && strings.HasPrefix(sub[3], "cactusfs-volumes:"):
				order = append(order, "put "+sub[3]+" "+strings.Join(r.tarNames(c), ","))
			}
			for i, a := range c.Args {
				if a == "-v" || a == "--volume" || strings.HasPrefix(a, "--volume=") {
					t.Errorf("a host path or volume given with -v: %q", c.Args)
				}
				if a == "--mount" && !strings.HasPrefix(c.Args[i+1], "type=volume,source=cactusfs-") {
					t.Errorf("a mount that is not one of the deployment's volumes: %q", c.Args[i+1])
				}
			}
		}
		want := []string{
			"dev-init", "volume cactusfs-secrets", "volume cactusfs-config",
			"put cactusfs-volumes:/secrets .,kms-passphrase,root-access-key,root-secret-key",
			"put cactusfs-volumes:/config .,kms.json,node-ca.pem,nodes,nodes/cactusfs.key,nodes/cactusfs.pem,ready,s3-ca.pem,s3.key,s3.pem",
		}
		if !slices.Equal(order, want) {
			t.Errorf("the order was\n%q\nwant\n%q", order, want)
		}

		// A second install: both copies are there and equal. Nothing is
		// made, nothing is put, the entry is as it was - under -x too.
		before := tree(t, state)
		r.rules(dockerRules(t, devInitOutput(), withTarget(t, state))...)
		r.trace = true
		res = r.guarded(env, installDD...)
		r.trace = false
		res.want(t, 3, "not built yet (PLAN 10.5)", "hold the same secrets")
		if after := tree(t, state); !equalTrees(before, after) {
			t.Errorf("the second install changed the entry:\nbefore %v\nafter  %v", before, after)
		}
		for _, words := range [][]string{{"create", "--name", "cactusfs-dev-init"}, {"volume", "create"}, {"cp", "-a"}, {"start"}} {
			if c := callsOf(res, "docker", words...); len(c) != 0 {
				t.Errorf("the second install ran %q", c[0].Args)
			}
		}
		if strings.Contains(res.output(), "made ") {
			t.Errorf("the second install says it made something:\n%s", res.output())
		}
		noSecretAnywhere(t, res, state)
	})
}

// copyCase is a second install over an entry and volumes a case changed.
type copyCase struct {
	name string
	// change changes the rig after its first install, and returns the
	// volumes of the target (nil: none), extra lines of the env file and
	// extra arguments.
	change func(t *testing.T, r *rig, state string) (map[string]map[string]string, string, []string)
	// rules come before the engine's own.
	rules []rule
	code  int
	want  []string
	// puts is the tar of each put, "<unit>:<names>", in order; restored:
	// the entry is what the first install made.
	puts     []string
	restored bool
	// replaceEnv: the lines change returns are the whole env file, not
	// lines added to it.
	replaceEnv bool
	// check, when set, looks at the run further: r is the case's rig and
	// content its env file.
	check func(t *testing.T, r *rig, state, content string, res result)
}

// runCopyCases runs the cases, each over a copy of one first install.
func runCopyCases(t *testing.T, env string, cases []copyCase) {
	t.Helper()
	eachShell(t, func(t *testing.T, r *rig) {
		copyCases(t, installed(t, r, env), env, cases)
	})
}

// copyCases runs the cases as parallel subtests of t, each over a copy of
// base, a rig whose first install ran with env.
func copyCases(t *testing.T, base *rig, env string, cases []copyCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := clone(t, base)
			r.keepStdin()
			state := r.stateOf("dd-default")
			first := tree(t, state)
			volumes, extra, args := c.change(t, r, state)
			r.rules(append(slices.Clone(c.rules), dockerRules(t, devInitOutput(), volumes)...)...)
			before := tree(t, state)
			content := env + extra
			if c.replaceEnv {
				content = extra
			}
			res := r.guarded(content, append(slices.Clone(installDD), args...)...)
			want := make([]string, len(c.want))
			for i, w := range c.want {
				want[i] = strings.ReplaceAll(w, "STATE", state)
			}
			res.want(t, c.code, want...)

			// Never made again: no dev-init, no new secret.
			if calls := callsOf(res, "docker", "create", "--name", "cactusfs-dev-init"); len(calls) != 0 {
				t.Errorf("dev-init ran again")
			}
			if strings.Contains(res.output(), "secrets: made ") {
				t.Errorf("a secret was made again:\n%s", res.output())
			}
			var puts []string
			for _, call := range callsOf(res, "docker", "cp", "-a") {
				puts = append(puts, strings.TrimPrefix(call.Args[len(call.Args)-1], "cactusfs-volumes:/")+":"+strings.Join(r.tarNames(call), ","))
			}
			if !slices.Equal(puts, c.puts) {
				t.Errorf("put %q into volumes, want %q", puts, c.puts)
			}
			if c.code != 3 && len(callsOf(res, "docker", "volume", "create")) != 0 {
				t.Error("a run that did not finish made a volume")
			}
			after := tree(t, state)
			switch {
			case c.code != 3 && !equalTrees(before, after):
				t.Errorf("a run that did not finish changed the entry:\nbefore %v\nafter  %v", before, after)
			case c.restored && !equalTrees(first, after):
				t.Errorf("the entry is not what the first install made:\nfirst %v\nafter %v", first, after)
			}
			if c.code == 3 {
				modesAre(t, r.stateDir())
			}
			var given []string
			for _, line := range strings.Split(content, "\n") {
				if key, path, ok := strings.Cut(line, "="); ok && strings.HasSuffix(key, "_FILE") {
					given = append(given, path)
				}
			}
			if _, err := os.Stat(filepath.Join(state, "secrets")); err == nil || len(given) > 0 {
				noSecretAnywhere(t, res, state, given...)
			}
			if c.check != nil {
				c.check(t, r, state, content, res)
			}
		})
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

// keepTarget: the target holds what the entry holds.
func keepTarget(t *testing.T, _ *rig, state string) (map[string]map[string]string, string, []string) {
	return withTarget(t, state), "", nil
}

// TestSecretsTwoCopies: one copy missing is put back from the other, the
// local one with a warning, and only what the target lacks is put there;
// copies that differ or are not whole, an engine that cannot be asked, a
// volume the script did not make, volumes with no record, another engine
// behind the same settings and another KMS stop the run, and nothing is
// made, put or changed.
func TestSecretsTwoCopies(t *testing.T) {
	t.Parallel()
	without := func(files ...string) func(t *testing.T, r *rig, state string) (map[string]map[string]string, string, []string) {
		return func(t *testing.T, _ *rig, state string) (map[string]map[string]string, string, []string) {
			v := withTarget(t, state)
			for _, f := range files {
				remove(t, filepath.Join(state, f))
			}
			return v, "", nil
		}
	}
	local := func(files ...string) func(t *testing.T, r *rig, state string) (map[string]map[string]string, string, []string) {
		return func(t *testing.T, _ *rig, state string) (map[string]map[string]string, string, []string) {
			for _, f := range files {
				remove(t, filepath.Join(state, f))
			}
			return nil, "", nil
		}
	}
	runCopyCases(t, "KMS=local\n", []copyCase{
		{
			name:   "only the local copy",
			change: local(),
			code:   3, want: []string{"put into the volume cactusfs-secrets what it lacked of STATE/secrets: kms-passphrase root-access-key root-secret-key"},
			puts: []string{
				"secrets:.,kms-passphrase,root-access-key,root-secret-key",
				"config:.,kms.json,node-ca.pem,nodes,nodes/cactusfs.key,nodes/cactusfs.pem,ready,s3-ca.pem,s3.key,s3.pem",
			},
			restored: true,
		},
		{
			name: "the local kms.json missing", change: without("config/kms.json"),
			code: 3, want: []string{"the local copy STATE/config/kms.json was missing: copied back from the volume cactusfs-config"}, restored: true,
		},
		{
			name: "the volume's kms.json and a node's files missing",
			change: func(t *testing.T, _ *rig, state string) (map[string]map[string]string, string, []string) {
				v := withTarget(t, state)
				delete(v["config"], "kms.json")
				delete(v["config"], "nodes/cactusfs.pem")
				delete(v["config"], "nodes/cactusfs.key")
				return v, "", nil
			},
			code: 3, puts: []string{"config:kms.json,nodes,nodes/cactusfs.key,nodes/cactusfs.pem"}, restored: true,
		},
		{
			name: "the passphrases differ",
			change: func(t *testing.T, _ *rig, state string) (map[string]map[string]string, string, []string) {
				v := withTarget(t, state)
				v["secrets"]["kms-passphrase"] = "another passphrase " + rand.Text()
				return v, "", nil
			},
			code: 2, want: []string{"STATE/secrets/kms-passphrase and kms-passphrase in the volume cactusfs-secrets differ", "never chooses"},
		},
		{
			name: "the passphrase lost, the key file kept", change: local("secrets/kms-passphrase"),
			code: 2, want: []string{"its passphrase is in neither", "KMS_PASSPHRASE_FILE"},
		},
		{
			name: "half the root credentials", change: local("secrets/root-secret-key"),
			code: 2, want: []string{"one of the root credentials is in", "ROOT_SECRET_KEY_FILE"},
		},
		{
			name: "certificates not complete", change: local("config/ready"),
			code: 2, want: []string{"are not complete (no 'ready'"},
		},
		{
			name: "kms.json lost from both", change: local("config/kms.json"),
			code: 2, want: []string{"kms.json is in neither", "KMS_KEY_FILE"},
		},
		{
			name: "kms.json lost from both, given as a file",
			change: func(t *testing.T, r *rig, state string) (map[string]map[string]string, string, []string) {
				key := r.envFile("kms.json", fileOf(t, filepath.Join(state, "config", "kms.json")))
				phrase := r.envFile("phrase", fileOf(t, filepath.Join(state, "secrets", "kms-passphrase")))
				v := withTarget(t, state)
				delete(v["config"], "kms.json")
				remove(t, filepath.Join(state, "config", "kms.json"))
				return v, "KMS_KEY_FILE=" + key + "\nKMS_PASSPHRASE_FILE=" + phrase + "\n", nil
			},
			code: 3, want: []string{"took KMS_KEY_FILE into STATE/config/kms.json"}, puts: []string{"config:kms.json"}, restored: true,
		},
		{
			name: "a volume of the name the script did not make", change: local(),
			rules: []rule{
				{Tool: "docker", Contains: []string{"volume", "ls", "name=cactusfs-"}, Stdout: "cactusfs-secrets\n"},
				{Tool: "docker", Contains: []string{"volume", "inspect", "cactusfs-secrets"}, Stdout: "cactusfs-secrets \n"},
			},
			code: 2, want: []string{"a volume named cactusfs-secrets is there without the label io.cactusfs.deploy.name=cactusfs", "never takes over"},
		},
		{
			name: "a volume whose name is only part of another", change: local(),
			rules: []rule{{Tool: "docker", Contains: []string{"volume", "ls", "name=cactusfs-"}, Stdout: "cactusfs-secrets-old\nx-cactusfs-secrets\ncactusfs-secretsx\n"}},
			code:  3, puts: []string{
				"secrets:.,kms-passphrase,root-access-key,root-secret-key",
				"config:.,kms.json,node-ca.pem,nodes,nodes/cactusfs.key,nodes/cactusfs.pem,ready,s3-ca.pem,s3.key,s3.pem",
			},
			restored: true,
		},
		{
			name: "a volume holding a link", change: keepTarget,
			rules: []rule{{Tool: "docker", Contains: []string{"cp", "cactusfs-volumes:/secrets", "-"}, Stdout: tarWithLink(t, "secrets", "root-access-key")}},
			code:  2, want: []string{"a volume of the deployment holds something that is neither a file nor a directory"},
		},
		{
			name: "an engine that cannot list its volumes", change: keepTarget,
			rules: []rule{{Tool: "docker", Contains: []string{"volume", "ls"}, Stderr: "Cannot connect to the Docker daemon\n", Exit: 1}},
			code:  1, want: []string{"Cannot connect to the Docker daemon", "could not list the volumes"},
		},
		{
			name: "an engine that cannot inspect a volume it lists", change: keepTarget,
			rules: []rule{{Tool: "docker", Contains: []string{"volume", "inspect"}, Stderr: "Error response from daemon\n", Exit: 1}},
			code:  1, want: []string{"could not inspect the volumes cactusfs-secrets cactusfs-config"},
		},
		{
			name: "volumes and no record", change: without("target", "secrets", "config"),
			code: 2, want: []string{"the deployment's volumes (cactusfs-secrets cactusfs-config) are on Docker Desktop", "holds no record", "--adopt"},
		},
		{
			name: "volumes and no record, adopted",
			change: func(t *testing.T, r *rig, state string) (map[string]map[string]string, string, []string) {
				v, _, _ := without("target", "secrets", "config")(t, r, state)
				return v, "", []string{"--adopt"}
			},
			code: 3, want: []string{"state: recorded", "the local copy STATE/secrets/root-access-key was missing"}, restored: true,
		},
		{
			name: "another engine behind the same settings", change: keepTarget,
			rules: []rule{{Tool: "docker", Contains: []string{"info"}, Stdout: "ENGINE-TWO\n"}},
			code:  2, want: []string{"made on the engine '" + engineID + "'", "now reach another, 'ENGINE-TWO'", "never given to a second engine", "--engine-replaced"},
		},
		{
			// --another-target is for a name on another target ID; it never
			// gives one deployment's secrets to a second engine.
			name: "another engine, said with --another-target",
			change: func(*testing.T, *rig, string) (map[string]map[string]string, string, []string) {
				return nil, "", []string{"--another-target"}
			},
			rules: []rule{{Tool: "docker", Contains: []string{"info"}, Stdout: "ENGINE-TWO\n"}},
			code:  2, want: []string{"now reach another, 'ENGINE-TWO'", "never given to a second engine"},
		},
		{
			name: "the engine replaced",
			change: func(*testing.T, *rig, string) (map[string]map[string]string, string, []string) {
				return nil, "", []string{"--engine-replaced"}
			},
			rules: []rule{{Tool: "docker", Contains: []string{"info"}, Stdout: "ENGINE-TWO\n"}},
			code:  3, want: []string{"the deployment made on the engine '" + engineID + "' is taken as gone", "now belong to the engine 'ENGINE-TWO'", "in place of the one it replaced"},
			puts: []string{
				"secrets:.,kms-passphrase,root-access-key,root-secret-key",
				"config:.,kms.json,node-ca.pem,nodes,nodes/cactusfs.key,nodes/cactusfs.pem,ready,s3-ca.pem,s3.key,s3.pem",
			},
			check: func(t *testing.T, r *rig, state, content string, _ result) {
				if got := fileOf(t, filepath.Join(state, "target")); got != "docker --context default\nengine-id=ENGINE-TWO\nkms=local\n" {
					t.Errorf("the record is %q", got)
				}
				// A later run on the new engine needs no flag, and the old
				// one is refused.
				r.rules(append([]rule{{Tool: "docker", Contains: []string{"info"}, Stdout: "ENGINE-TWO\n"}}, dockerRules(t, devInitOutput(), withTarget(t, state))...)...)
				r.guarded(content, installDD...).want(t, 3, "hold the same secrets")
				r.rules(dockerRules(t, devInitOutput(), withTarget(t, state))...)
				r.guarded(content, installDD...).want(t, 2, "made on the engine 'ENGINE-TWO'")
			},
		},
		{
			name: "the same engine, said replaced",
			change: func(t *testing.T, _ *rig, state string) (map[string]map[string]string, string, []string) {
				return withTarget(t, state), "", []string{"--engine-replaced"}
			},
			code: 3, restored: true,
		},
		{
			name: "another KMS",
			change: func(t *testing.T, r *rig, state string) (map[string]map[string]string, string, []string) {
				token := r.envFile("token", "hvs."+rand.Text())
				return withTarget(t, state), "KMS=vault\nVAULT_ADDR=https://vault.example.test:8200\nVAULT_KEY=cactusfs\nVAULT_TOKEN_FILE=" + token + "\n", nil
			},
			replaceEnv: true,
			code:       2, want: []string{"made with KMS=local, and the settings now say KMS=vault"},
		},
	})
}

// TestSecretsGivenFiles: a root credential, the passphrase or kms.json given
// as a file that differs from a copy - the local one or the volume's - is
// refused; a Vault token or a certificate that differs is a rotation: the
// volume's copy is replaced first, then the local one, and the run says so.
// A rotation stopped half way (the volume's copy replaced) is finished by
// the same files; a local copy that differs from the volume's otherwise is
// refused. A certificate rotated without its CA, where a CA is kept, is
// refused. Every case branches from one first install per shell.
func TestSecretsGivenFiles(t *testing.T) {
	t.Parallel()
	const local = "KMS=local\n"
	// givenEqual is the env lines that give the root credentials, the
	// passphrase and kms.json as files equal to the entry's, but for the
	// key named, which gets a file of its own.
	givenEqual := func(r *rig, state, key string) string {
		files := map[string]string{
			"ROOT_ACCESS_KEY_FILE": "secrets/root-access-key", "ROOT_SECRET_KEY_FILE": "secrets/root-secret-key",
			"KMS_PASSPHRASE_FILE": "secrets/kms-passphrase", "KMS_KEY_FILE": "config/kms.json",
		}
		env := ""
		for k, f := range files {
			content := fileOf(r.t, filepath.Join(state, f))
			if k == key {
				content = "another " + k + " " + rand.Text()
			}
			env += k + "=" + r.envFile(strings.ToLower(k), content) + "\n"
		}
		return env
	}
	var cases []copyCase
	for _, k := range []struct{ key, file string }{
		{"ROOT_ACCESS_KEY_FILE", "secrets/root-access-key"},
		{"ROOT_SECRET_KEY_FILE", "secrets/root-secret-key"},
		{"KMS_PASSPHRASE_FILE", "secrets/kms-passphrase"},
		{"KMS_KEY_FILE", "config/kms.json"},
	} {
		cases = append(cases, copyCase{
			name: k.key + " that differs from the local copy",
			change: func(_ *testing.T, r *rig, state string) (map[string]map[string]string, string, []string) {
				return nil, givenEqual(r, state, k.key), nil
			},
			code: 2, want: []string{k.key + " (", "and STATE/" + k.file + " differ", "never replaces it"},
		})
	}
	// The volume's copy is compared the same way, whatever the key.
	cases = append(cases, copyCase{
		name: "KMS_KEY_FILE that differs from the volume's",
		change: func(t *testing.T, r *rig, state string) (map[string]map[string]string, string, []string) {
			v := withTarget(t, state)
			env := givenEqual(r, state, "KMS_KEY_FILE")
			remove(t, filepath.Join(state, "config", "kms.json"))
			return v, env, nil
		},
		code: 2, want: []string{"KMS_KEY_FILE (", "and kms.json in the volume cactusfs-config differ"},
	})

	// Rotations of the operator's certificate: given files that differ
	// from both copies, from the local copy only (a rotation stopped half
	// way), or from the volume's only (refused: two copies that differ).
	tls := func(r *rig, withCA bool) (string, map[string]string) {
		files := map[string]string{"s3.pem": "a new certificate " + rand.Text(), "s3.key": "a new key " + rand.Text(), "s3-ca.pem": "a new CA " + rand.Text()}
		env := ""
		for _, kv := range [][2]string{{"TLS_CERT_FILE", "s3.pem"}, {"TLS_KEY_FILE", "s3.key"}, {"TLS_CA_FILE", "s3-ca.pem"}} {
			if kv[0] == "TLS_CA_FILE" && !withCA {
				delete(files, kv[1])
				continue
			}
			env += kv[0] + "=" + r.envFile(kv[1], files[kv[1]]) + "\n"
		}
		return env, files
	}
	rotation := func(name string, newLocal, newTarget bool, code int, want []string, puts []string) copyCase {
		return copyCase{
			name: name,
			change: func(t *testing.T, r *rig, state string) (map[string]map[string]string, string, []string) {
				env, files := tls(r, true)
				v := withTarget(t, state)
				for f, data := range files {
					if newTarget {
						v["config"][f] = data
					}
					if newLocal {
						remove(t, filepath.Join(state, "config", f))
						if err := os.WriteFile(filepath.Join(state, "config", f), []byte(data), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				return v, env, nil
			},
			code: code, want: want, puts: puts,
		}
	}
	rotated := []string{"rotated TLS_CERT_FILE: STATE/config/s3.pem replaced", "rotated TLS_KEY_FILE", "rotated TLS_CA_FILE"}
	cases = append(cases,
		rotation("certificates rotated in both copies", false, false, 3, rotated, []string{"config:s3.pem,s3.key,s3-ca.pem"}),
		rotation("certificates rotated where the local copy is old", false, true, 3, rotated, nil),
		rotation("certificates rotated where only the local copy is new", true, false, 2, []string{"STATE/config/s3-ca.pem and s3-ca.pem in the volume cactusfs-config differ"}, nil),
		copyCase{
			name: "a certificate rotated without its CA",
			change: func(t *testing.T, r *rig, state string) (map[string]map[string]string, string, []string) {
				env, _ := tls(r, false)
				return withTarget(t, state), env, nil
			},
			code: 2, want: []string{"s3-ca.pem, the CA of the certificate it replaces, would be kept", "TLS_CA_FILE"},
		})

	// After a rotation of each file both copies hold the file given, the
	// volume's first; a later run with the same files changes nothing.
	type rotationRun struct{ name, file, key string }
	eachShell(t, func(t *testing.T, r *rig) {
		base := installed(t, r, local)
		copyCases(t, base, local, cases)

		vaultEnv := "KMS=vault\nVAULT_ADDR=https://vault.example.test:8200\nVAULT_KEY=cactusfs\nVAULT_TOKEN_FILE=" + r.envFile("token", "hvs.old-"+rand.Text()) + "\n"
		vault := installed(t, r, vaultEnv)
		for _, v := range []rotationRun{
			{"rotation of the certificate, key and CA", "config/s3.pem", "TLS_CERT_FILE"},
			{"rotation of the vault token", "secrets/vault-token", "VAULT_TOKEN_FILE"},
		} {
			t.Run(v.name, func(t *testing.T) {
				t.Parallel()
				from, env := base, local
				if v.key == "VAULT_TOKEN_FILE" {
					from, env = vault, vaultEnv
				}
				r := clone(t, from)
				r.keepStdin()
				state := r.stateOf("dd-default")
				rot := env
				var given map[string]string
				var givenFiles []string
				if v.key == "VAULT_TOKEN_FILE" {
					given = map[string]string{"secrets/vault-token": "hvs.new-" + rand.Text()}
					token := r.envFile("token2", given["secrets/vault-token"])
					givenFiles = append(givenFiles, token)
					// The first line names the new token; the old one is a comment.
					rot = strings.Replace(env, "VAULT_TOKEN_FILE=", "VAULT_TOKEN_FILE="+token+"\n#", 1)
				} else {
					lines, files := tls(r, true)
					rot += lines
					given = map[string]string{}
					for f, data := range files {
						given["config/"+f] = data
						givenFiles = append(givenFiles, filepath.Join(r.dir, f))
					}
				}
				r.rules(dockerRules(t, devInitOutput(), withTarget(t, state))...)
				res := r.guarded(rot, installDD...)
				res.want(t, 3, "rotated "+v.key+": "+state+"/"+v.file+" replaced, after the volume's copy")
				for file, data := range given {
					if got := fileOf(t, filepath.Join(state, file)); got != data {
						t.Errorf("%s was not rotated", file)
					}
				}
				volumeLine := strings.Index(res.stdout, "rotated in the volume")
				localLine := strings.Index(res.stdout, "rotated "+v.key+":")
				if volumeLine < 0 || volumeLine > localLine {
					t.Errorf("the local copy was replaced before the volume's:\n%s", res.stdout)
				}
				noSecretAnywhere(t, res, state, givenFiles...)
				before := tree(t, state)
				r.rules(dockerRules(t, devInitOutput(), withTarget(t, state))...)
				res = r.guarded(rot, installDD...)
				res.want(t, 3)
				if len(callsOf(res, "docker", "cp", "-a")) != 0 || strings.Contains(res.output(), "rotated") {
					t.Errorf("a second run rotated again:\n%s", res.output())
				}
				if after := tree(t, state); !equalTrees(before, after) {
					t.Error("a second run changed the entry")
				}
			})
		}
	})
}

// TestSecretsGivenAsFiles: secrets and certificates given as files are
// copied into the entry as they are and put on the target; with every one
// given, no secret is generated and dev-init does not run; with Vault the
// token is kept and no key file is. No given value reaches a command line,
// an environment or the output.
func TestSecretsGivenAsFiles(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		t.Run("local", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			given := map[string]string{
				"ROOT_ACCESS_KEY_FILE": "secrets/root-access-key", "ROOT_SECRET_KEY_FILE": "secrets/root-secret-key",
				"KMS_PASSPHRASE_FILE": "secrets/kms-passphrase", "KMS_KEY_FILE": "config/kms.json",
				"TLS_CERT_FILE": "config/s3.pem", "TLS_KEY_FILE": "config/s3.key", "TLS_CA_FILE": "config/s3-ca.pem",
			}
			env := "KMS=local\n"
			var paths []string
			for key := range given {
				path := r.envFile(strings.ToLower(key), "given "+key+" "+rand.Text()+"\n")
				paths = append(paths, path)
				env += key + "=" + path + "\n"
			}
			r.rules(dockerRules(t, devInitOutput(), nil)...)
			res := r.guarded(env, installDD...)
			res.want(t, 3, "took ROOT_ACCESS_KEY_FILE into")
			state := r.stateOf("dd-default")
			got := filesUnder(t, state)
			for key, name := range given {
				if got[name] != fileOf(t, filepath.Join(r.dir, strings.ToLower(key))) {
					t.Errorf("%s is not the file %s gives", name, key)
				}
			}
			if _, ok := got["config/ready"]; !ok {
				t.Error("config has no ready")
			}
			if len(callsOf(res, "docker", "create", "--name", "cactusfs-dev-init")) != 0 {
				t.Error("dev-init ran, though every file it makes was given")
			}
			if strings.Contains(res.output(), "secrets: made the") {
				t.Errorf("a secret was generated though it was given:\n%s", res.output())
			}
			modesAre(t, r.stateDir())
			noSecretAnywhere(t, res, state, paths...)
		})
		t.Run("vault", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			token := r.envFile("token", "hvs."+rand.Text()+"\n")
			r.rules(dockerRules(t, devInitOutput(), nil)...)
			res := r.guarded("KMS=vault\nVAULT_ADDR=https://vault.example.test:8200\nVAULT_KEY=cactusfs\nVAULT_TOKEN_FILE="+token+"\n", installDD...)
			res.want(t, 3, "made "+r.stateOf("dd-default")+"/config (the certificates)")
			state := r.stateOf("dd-default")
			got := filesUnder(t, state)
			if got["secrets/vault-token"] != fileOf(t, token) {
				t.Error("secrets/vault-token is not the token file")
			}
			if !strings.HasSuffix(got["target"], "kms=vault\n") {
				t.Errorf("the record does not say KMS=vault: %q", got["target"])
			}
			for _, name := range []string{"secrets/kms-passphrase", "config/kms.json"} {
				if _, ok := got[name]; ok {
					t.Errorf("%s is kept with KMS=vault", name)
				}
			}
			// dev-init was given a passphrase all the same - one thrown away.
			if in := callsOf(res, "docker", "cp", "-a", "-", "cactusfs-dev-init:/tmp"); len(in) != 1 {
				t.Errorf("dev-init's input: %v", in)
			}
			noSecretAnywhere(t, res, state, token)
		})
	})
}

// TestSecretsRefused: what stops a first install before anything is
// written - each with exit 2, no container made, no STATE_DIR.
func TestSecretsRefused(t *testing.T) {
	t.Parallel()
	binary := func(rel string, data string) func(t *testing.T, r *rig) {
		return func(t *testing.T, r *rig) {
			if err := os.WriteFile(filepath.Join(r.repo, "dist", "image", rel), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cases := []struct {
		name  string
		env   string
		setup func(t *testing.T, r *rig)
		rules []rule
		want  string
	}{
		{"no binaries", "", func(t *testing.T, r *rig) { remove(t, filepath.Join(r.repo, "dist")) }, nil, "run make image-binaries"},
		{"a binary that is not the one summed", "", binary("linux-"+runtime.GOARCH+"/cactusfs", "another binary"), nil, "does not match"},
		{"binaries of make build-all", "", binary("BUILDINFO", "packaging=binary\n"), nil, "does not say packaging=image"},
		{"a cluster on one local key file", "DEPLOY_MODE=cluster\n", nil, nil, "KMS=local with DEPLOY_MODE=cluster"},
		{"a certificate that is not there", "TLS_CERT_FILE=/nonexistent/s3.pem\n", nil, nil, "TLS_CERT_FILE=/nonexistent/s3.pem: is not a file that can be read"},
		{"the runtime image absent, offline", "OFFLINE=yes\n", nil, []rule{{Tool: "docker", Contains: []string{"image", "inspect"}, Exit: 1}}, "is not on the engine and OFFLINE=yes"},
		{"a dev-init container the script did not make", "", nil, []rule{
			{Tool: "docker", Contains: []string{"ps", "name=cactusfs-dev-init"}, Stdout: "cactusfs-dev-init\n"},
			{Tool: "docker", Contains: []string{"container", "inspect", "cactusfs-dev-init"}, Stdout: "someone\n"},
		}, "a container named cactusfs-dev-init is there without the label"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				if c.setup != nil {
					c.setup(t, r)
				}
				env := c.env
				if strings.HasPrefix(env, "TLS_CERT_FILE") {
					env += "TLS_KEY_FILE=" + r.envFile("key", "a key") + "\n"
				}
				r.rules(append(slices.Clone(c.rules), dockerRules(t, devInitOutput(), nil)...)...)
				res := r.guarded("KMS=local\n"+env, installDD...)
				res.want(t, 2, c.want)
				for _, words := range [][]string{{"create"}, {"rm"}, {"pull"}, {"volume", "create"}} {
					if calls := callsOf(res, "docker", words...); len(calls) != 0 {
						t.Errorf("a refused install ran %q", calls[0].Args)
					}
				}
				if _, err := os.Stat(r.stateDir()); err == nil {
					t.Error("a refused install wrote STATE_DIR")
				}
			})
		}
	})
}

// TestSecretsOnEveryEngine: Podman runs the same through its own CLI, and
// Docker CE on a server keeps its copy there - its engine over ssh, after
// its host key is checked - while dev-init runs on this machine's engine.
// The runtime image is pulled when it is not there. A dry run asks nothing
// and writes nothing; a target whose copy a later step keeps (Kubernetes)
// makes no secret and records nothing yet.
func TestSecretsOnEveryEngine(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		t.Run("podman", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			r.rules(engineRules(t, "podman", "host:/var/lib/containers", devInitOutput(), nil)...)
			res := r.guardedRun([]string{"--env", r.envFile("site.env", "KMS=local\n"), "--docker", "podman", "--install"}, "XDG_RUNTIME_DIR=/run/user/1000")
			res.want(t, 3, "not built yet (PLAN 10.5)")
			if len(callsOf(res, "docker")) != 0 {
				t.Error("docker was run for podman")
			}
			if len(callsOf(res, "podman", "start", "-a", "cactusfs-dev-init")) != 1 || len(callsOf(res, "podman", "cp", "-a", "-", "cactusfs-volumes:/config")) != 1 {
				t.Errorf("podman did not run dev-init and fill the volumes: %v", res.calls)
			}
			if got := filesUnder(t, r.stateOf("podman-default"))["target"]; got != "podman (local)\nengine-id=host:/var/lib/containers\nkms=local\n" {
				t.Errorf("target = %q", got)
			}
			noSecretAnywhere(t, res, r.stateOf("podman-default"))
		})
		t.Run("ce over ssh", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			r.rules(dockerRules(t, devInitOutput(), nil)...)
			res := r.guarded("KMS=local\nCE_HOST=deploy@server.example.test\n", "--docker", "ce", "--install")
			res.want(t, 3, "not built yet (PLAN 10.7)")
			if len(res.calls) == 0 || res.calls[0].Tool != "ssh" || res.calls[0].Args[len(res.calls[0].Args)-1] != "true" {
				t.Fatalf("the host key was not checked first: %v", res.calls)
			}
			if info := callsOf(res, "docker", "info"); len(info) != 1 || info[0].Env["DOCKER_HOST"] != "ssh://deploy@server.example.test" {
				t.Errorf("the server's engine was not asked its ID: %v", info)
			}
			for _, c := range callsOf(res, "docker") {
				remote := c.Env["DOCKER_HOST"] == "ssh://deploy@server.example.test"
				devInit := false
				for _, a := range c.Args {
					devInit = devInit || strings.HasPrefix(a, "cactusfs-dev-init") || strings.HasPrefix(a, "name=cactusfs-dev-init")
				}
				devInit = devInit || (containsRun(c.Args, []string{"--platform"}) && containsRun(c.Args, []string{"pull"}))
				switch {
				case devInit && remote:
					t.Errorf("dev-init ran on the server: %q", c.Args)
				case !remote && !containsRun(c.Args, []string{"--context", "default"}):
					t.Errorf("this machine's engine without its context: %q", c.Args)
				case !devInit && !remote && !containsRun(c.Args, []string{"image", "inspect"}):
					t.Errorf("the target's engine was asked here, not on the server: %q", c.Args)
				}
			}
			if len(callsOf(res, "docker", "cp", "-a", "-", "cactusfs-volumes:/secrets")) != 1 {
				t.Error("the server's volume was not filled")
			}
			state := r.stateOf("ssh-deploy_server.example.test")
			if got := filesUnder(t, state)["target"]; got != "docker over ssh://deploy@server.example.test\nengine-id="+engineID+"\nkms=local\n" {
				t.Errorf("target = %q", got)
			}
			noSecretAnywhere(t, res, state)
		})
		t.Run("image source image", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			// No Dockerfile: with IMAGE_SOURCE=image the runtime image is not
			// needed - the helpers are made from IMAGE, as dev-init runs in it.
			remove(t, filepath.Join(r.repo, "Dockerfile"))
			const image = "registry.example.test/cactusfs:0.7.0"
			r.rules(dockerRules(t, devInitOutput(), nil)...)
			res := r.guarded("KMS=local\nIMAGE_SOURCE=image\nIMAGE="+image+"\n", installDD...)
			res.want(t, 3, "not built yet (PLAN 10.5)")
			helpers := callsOf(res, "docker", "create", "--name", "cactusfs-volumes")
			if len(helpers) != 1 || !containsRun(helpers[0].Args, []string{image, "true"}) {
				t.Errorf("the helper of the volumes is not made from IMAGE: %v", helpers)
			}
			devInit := callsOf(res, "docker", "create", "--name", "cactusfs-dev-init")
			if len(devInit) != 1 || !containsRun(devInit[0].Args, []string{"--entrypoint", "/cactusfs", image, "dev-init"}) {
				t.Errorf("dev-init does not run in IMAGE: %v", devInit)
			}
			if in := callsOf(res, "docker", "cp", "-a", "-", "cactusfs-dev-init:/tmp"); len(in) != 1 {
				t.Errorf("dev-init's input: %v", in)
			}
			for _, c := range res.calls {
				for _, a := range c.Args {
					if strings.Contains(a, "distroless") {
						t.Errorf("the runtime image is used: %q", c.Args)
					}
				}
			}
			if len(callsOf(res, "docker", "pull")) != 0 {
				t.Error("an image was pulled though IMAGE is there")
			}
		})
		t.Run("the runtime image pulled", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			r.rules(append([]rule{{Tool: "docker", Contains: []string{"image", "inspect"}, Exit: 1}}, dockerRules(t, devInitOutput(), nil)...)...)
			res := r.guarded("KMS=local\n", installDD...)
			res.want(t, 3)
			pulls := callsOf(res, "docker", "pull")
			if len(pulls) != 2 || !containsRun(pulls[1].Args, []string{"--platform", "linux/" + runtime.GOARCH, runtimeImage(t)}) {
				t.Errorf("pulls: %v", pulls)
			}
			for _, c := range callsOf(res, "docker", "create") {
				if c.Args[2] != "create" {
					continue // volume create
				}
				if !containsRun(c.Args, []string{"--pull", "never"}) {
					t.Errorf("a container made without --pull never: %q", c.Args)
				}
			}
		})
		t.Run("dry run", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			r.rules(dockerRules(t, devInitOutput(), nil)...)
			before := tree(t, r.dir)
			res := r.guarded("KMS=local\n", "--docker", "dd", "--install", "--dry-run")
			res.want(t, 3, "secrets: kept in "+r.stateOf("dd-default"), "is made once")
			if len(res.calls) != 0 {
				t.Errorf("a dry run asked %v", res.calls)
			}
			if after := tree(t, r.dir); !equalTrees(before, after) {
				t.Errorf("a dry run wrote files")
			}
		})
		t.Run("kubernetes, a later step", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			res := r.guarded("KMS=local\nK8S_CONTEXT=prod\n", "--docker", "k8s", "--install")
			res.want(t, 3, "not built yet (PLAN 10.10)", "built for this target by 10.9")
			if _, err := os.Stat(r.stateDir()); err == nil {
				t.Error("STATE_DIR was written for a target whose secrets are not built yet")
			}
			if len(res.calls) != 0 {
				t.Errorf("calls: %v", res.calls)
			}
		})
		t.Run("dev-init gives back a link", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			r.rules(append([]rule{{Tool: "docker", Contains: []string{"cp", "cactusfs-dev-init:/tmp/out", "-"}, Stdout: tarWithLink(t, "out", "kms.json")}},
				dockerRules(t, devInitOutput(), nil)...)...)
			res := r.guarded("KMS=local\n", installDD...)
			res.want(t, 2, "what dev-init gave back holds something that is neither a file nor a directory")
			if _, err := os.Stat(filepath.Join(r.stateOf("dd-default"), "config")); err == nil {
				t.Error("config was made of it")
			}
			if len(callsOf(res, "docker", "volume", "create")) != 0 {
				t.Error("a volume was made")
			}
		})
		t.Run("a refused adoption leaves no entry", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			volumes := map[string]map[string]string{"secrets": {"root-access-key": "x"}, "config": {"ready": "x"}}
			r.rules(append([]rule{{Tool: "docker", Contains: []string{"cp", "cactusfs-volumes:/config", "-"}, Stdout: tarWithLink(t, "config", "kms.json")}},
				dockerRules(t, devInitOutput(), volumes)...)...)
			res := r.guarded("KMS=local\n", append(slices.Clone(installDD), "--adopt")...)
			res.want(t, 2, "neither a file nor a directory")
			if _, err := os.Stat(r.stateDir()); err == nil {
				t.Errorf("a refused run left STATE_DIR: %v", tree(t, r.home))
			}
		})
		t.Run("adopt goes with install", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			r.guarded("KMS=local\n", "--docker", "dd", "--status", "--adopt").want(t, 2, "--adopt goes with --install")
		})
	})
}

// TestSecretsLeaveNoHelper: a helper container left by a run that stopped
// half way is removed when it is the deployment's.
func TestSecretsLeaveNoHelper(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		r = secretsRig(t, r)
		r.rules(append([]rule{
			{Tool: "docker", Contains: []string{"ps", "name=cactusfs-dev-init"}, Stdout: "cactusfs-dev-init\n"},
			{Tool: "docker", Contains: []string{"container", "inspect", "cactusfs-dev-init"}, Stdout: "cactusfs\n"},
		}, dockerRules(t, devInitOutput(), nil)...)...)
		res := r.guarded("KMS=local\n", installDD...)
		res.want(t, 3)
		if rm := callsOf(res, "docker", "rm", "-f", "cactusfs-dev-init"); len(rm) != 2 {
			t.Errorf("the left helper was not removed before dev-init ran, and dev-init's after: %d", len(rm))
		}
	})
}
