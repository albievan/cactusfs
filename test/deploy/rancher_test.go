//go:build deployscript

package deploy_test

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Rancher Desktop as a target (PLAN 10.6, deploy/lib/rancher.sh): its
// engine found - moby through the Docker context rancher-desktop, or
// containerd through nerdctl - and on it the standalone deployment of
// docker.sh, every engine command nerdctl's on containerd; a cluster
// through compose on moby, refused on containerd; its Kubernetes handed to
// the k8s path (10.10) with an image made in nerdctl's namespace k8s.io.
// 10.3's assertions for its copy of the secrets are kept: made once, never
// on a command line, in an environment or in any output, copies compared,
// refusals exit 2 with nothing changed and no STATE_DIR entry.

// containerdID is what the fake containerd says its ID is.
const containerdID = "CONTAINERD-UUID-ONE"

// vmDir is the private directory the fake mktemp of Rancher Desktop's VM
// makes (rdctl shell sudo mktemp -d, nerdctl_put).
const vmDir = "/dev/shm/cactusfs-deploy.Fx7Kq2"

// vmMktemp is the answer of Rancher Desktop's VM to mktemp, through rdctl.
var vmMktemp = rule{Tool: "rdctl", Contains: []string{"shell", "sudo", "mktemp", "-d", "/dev/shm/cactusfs-deploy.XXXXXX"}, Stdout: vmDir + "\n"}

// vmStat is the answer of Rancher Desktop's VM to the file system of the
// private directory: a tmpfs.
var vmStat = rule{Tool: "rdctl", Contains: []string{"shell", "sudo", "stat", "-f", "-c", "%T", vmDir}, Stdout: "tmpfs\n"}

// oldHex is the digest of the target of the image an installed node runs,
// as the fake nerdctl names it (RepoDigests) and as the container records
// it (.Image): a rollback runs it.
const oldHex = "4a5b6c7d8e9f00112233445566778899aabbccddeeff00112233445566778899"

// vmArchive is the answer of Rancher Desktop's VM to the archive of
// <vmDir>/<name> nerdctl_get reads: the stream, as "docker cp
// <container>:<dir> -" writes one.
func vmArchive(name, stream string) rule {
	return rule{Tool: "rdctl", Contains: vmGetArgs(name), Stdout: stream}
}

// vmGetArgs are the words of rdctl's archive of <vmDir>/<name>.
func vmGetArgs(name string) []string {
	return []string{"shell", "sudo", "tar", "-c", "-C", vmDir, "-f", "-", name}
}

// nerdctlRoot is where the fake nerdctl keeps its data in Rancher Desktop's
// VM: its data root and the digest of containerd's address.
const nerdctlRoot = "/var/lib/nerdctl/1935db59"

// mountpoint is the directory of a volume of the namespace default in the
// VM, as "volume inspect" names it.
func mountpoint(volume string) string {
	return nerdctlRoot + "/volumes/default/" + volume + "/_data"
}

// mountRule is the answer of nerdctl to where the volume is.
func mountRule(volume, answer string) rule {
	return rule{Tool: "nerdctl", Contains: []string{"volume", "inspect", "--format", "{{.Mountpoint}}", volume}, Stdout: answer}
}

// volPutArgs are the words of rdctl's extraction into a volume in place
// (nerdctl_volume_put).
func volPutArgs(volume string) []string {
	return []string{"shell", "sudo", "tar", "-x", "--same-owner", "-p", "-C", mountpoint(volume), "-f", "-"}
}

// volGetArgs are the words of rdctl's archive of a volume in place, its
// entries under the unit's name (nerdctl_volume_get).
func volGetArgs(volume, unit string) []string {
	return []string{"shell", "sudo", "tar", "-c", "-C", path.Dir(mountpoint(volume)), "-f", "-", "--transform=s,^_data," + unit + ",", "_data"}
}

// volArchive is the answer of the VM to the archive of a unit's volume.
func volArchive(unit, stream string) rule {
	return rule{Tool: "rdctl", Contains: volGetArgs("cactusfs-"+unit, unit), Stdout: stream}
}

// volumeOf is the volume a call of rdctl extracts into ("-x") or archives
// ("-c") in place, or "".
func volumeOf(c call, op string) string {
	for _, unit := range []string{"secrets", "config"} {
		v := "cactusfs-" + unit
		if (op == "-x" && slices.Equal(c.Args, volPutArgs(v))) || (op == "-c" && slices.Equal(c.Args, volGetArgs(v, unit))) {
			return v
		}
	}
	return ""
}

// rancherEngines are Rancher Desktop with each of its engines: the entry's
// target ID is the target and its Docker context, whichever engine runs.
var rancherEngines = []dockerEngine{
	{tool: "docker", target: "rancher", id: engineID, targetID: "rancher-rancher-desktop"},
	{tool: "nerdctl", target: "rancher", id: containerdID, targetID: "rancher-rancher-desktop"},
}

// rancherIdentity is the engine a run names and records, by the tool of
// the engine.
func rancherIdentity(tool string) string {
	if tool == "nerdctl" {
		return "Rancher Desktop, engine containerd, nerdctl --namespace default"
	}
	return "Rancher Desktop, engine moby, docker --context rancher-desktop"
}

// rancherRules are the answers of Rancher Desktop holding s: on containerd
// the Docker CLI reaches no moby engine (Rancher Desktop serves no Docker
// socket then), so the engine found is nerdctl's.
func rancherRules(t *testing.T, s engineState) []rule {
	t.Helper()
	rules := s.rules(t)
	if s.e.tool == "nerdctl" {
		rules = append([]rule{
			{Tool: "docker", Contains: []string{"--context", "rancher-desktop", "info"}, Stderr: "Cannot connect to the Docker daemon\n", Exit: 1},
			vmMktemp, vmStat,
			mountRule("cactusfs-secrets", mountpoint("cactusfs-secrets")+"\n"), mountRule("cactusfs-config", mountpoint("cactusfs-config")+"\n"),
		}, rules...)
		// An installed node: the digest of its image, by its tag and as it
		// was made from (docker_upgrade_nerdctl).
		if f := strings.Split(s.ctr, "|"); len(f) == 4 {
			rules = append([]rule{
				{Tool: "nerdctl", Contains: []string{"image", "inspect", "--format", "{{range .RepoDigests}}{{.}} {{end}}", f[3]}, Stdout: "cactusfs@sha256:" + oldHex + " \n"},
				{Tool: "nerdctl", Contains: []string{"container", "inspect", "--format", "{{.Image}}", "cactusfs"}, Stdout: "sha256:" + oldHex + "\n"},
			}, rules...)
		}
	}
	return rules
}

// rancherRun runs the script on Rancher Desktop with the env file and the
// arguments, under the guard of guardedRun.
func rancherRun(r *rig, env string, args ...string) result {
	r.t.Helper()
	return r.guardedRun(append([]string{"--env", r.envFile("site.env", env), "--docker", "rancher"}, args...))
}

// tarHeaders is the headers of the tar stream a call read (rig.keepStdin).
func (r *rig) tarHeaders(c call) []*tar.Header {
	r.t.Helper()
	data, err := os.ReadFile(filepath.Join(r.dir, stdinDir, c.Stdin))
	if err != nil {
		r.t.Fatalf("what %q read was not kept: %v", c.Args, err)
	}
	var out []*tar.Header
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			r.t.Fatal(err)
		}
		out = append(out, h)
	}
}

// engineCallsAre fails the test unless every call of an engine is the
// engine of e: on containerd nerdctl in the namespace default (and the
// Docker CLI only asked whether a moby engine answers), on moby the Docker
// CLI with the context rancher-desktop and no nerdctl.
func engineCallsAre(t *testing.T, res result, e dockerEngine) {
	t.Helper()
	n := 0
	for _, c := range res.calls {
		switch {
		case c.Tool == "podman":
			t.Errorf("podman was run for Rancher Desktop: %q", c.Args)
		case e.tool == "nerdctl" && c.Tool == "docker":
			if !slices.Equal(c.Args, []string{"--context", "rancher-desktop", "info", "--format", "{{.ID}}"}) {
				t.Errorf("docker was run for Rancher Desktop's containerd: %q", c.Args)
			}
		case e.tool == "nerdctl" && c.Tool == "nerdctl":
			n++
			if len(c.Args) < 2 || c.Args[0] != "--namespace" || c.Args[1] != "default" {
				t.Errorf("nerdctl was run outside the namespace default: %q", c.Args)
			}
		case e.tool == "docker" && c.Tool == "nerdctl":
			t.Errorf("nerdctl was run for Rancher Desktop's moby: %q", c.Args)
		case e.tool == "docker" && c.Tool == "docker":
			n++
			// Where the context points (PLAN 10.4's image_check, a cluster's
			// files on this machine) is asked of the context by its name.
			if slices.Equal(c.Args, []string{"context", "inspect", "rancher-desktop", "--format", "{{.Endpoints.docker.Host}}"}) {
				continue
			}
			if len(c.Args) < 2 || c.Args[0] != "--context" || c.Args[1] != "rancher-desktop" {
				t.Errorf("docker was run without the context rancher-desktop: %q", c.Args)
			}
		}
	}
	if n == 0 {
		t.Fatalf("%s was never run, so this proves nothing", e.tool)
	}
}

// engineName is the answer of a Docker engine to "info --format
// {{.Name}}|{{.OperatingSystem}}".
func engineName(answer string) rule {
	return rule{Tool: "docker", Contains: []string{"info", "{{.Name}}|{{.OperatingSystem}}"}, Stdout: answer + "\n"}
}

// TestRancherEngine: RANCHER_ENGINE=auto takes moby when the Docker
// context rancher-desktop (or ENGINE_CONTEXT) answers with an ID, else
// containerd when nerdctl does; neither, a tool missing for the engine set,
// a cluster on containerd and an entry recorded on the other engine are
// refused (exit 2) with nothing changed and no STATE_DIR entry made.
func TestRancherEngine(t *testing.T) {
	t.Parallel()
	mobyAnswers := rule{Tool: "docker", Contains: []string{"info", "{{.ID}}"}, Stdout: engineID + "\n"}
	mobySilent := rule{Tool: "docker", Contains: []string{"info", "{{.ID}}"}, Stderr: "Cannot connect to the Docker daemon\n", Exit: 1}
	nerdctlAnswers := rule{Tool: "nerdctl", Contains: []string{"info", "{{.ID}}"}, Stdout: containerdID + "\n"}
	nerdctlSilent := rule{Tool: "nerdctl", Contains: []string{"info", "{{.ID}}"}, Stderr: "cannot access containerd socket\n", Exit: 1}
	cases := []struct {
		name     string
		env      string
		args     []string
		rules    []rule
		without  []string
		recorded string
		code     int
		want     []string
		// asked are the engine questions the run must have asked, in
		// order, by tool and words after the tool.
		asked [][]string
		// notAsked is a tool the run must not have run.
		notAsked string
		// noCalls: the run asked no tool anything.
		noCalls bool
	}{
		{
			name: "moby answers", rules: []rule{mobyAnswers, nerdctlAnswers}, code: statusDone,
			want:     []string{"engine: " + rancherIdentity("docker"), "cactusfs is not installed on Rancher Desktop (engine moby)"},
			asked:    [][]string{{"docker", "--context", "rancher-desktop", "info", "--format", "{{.ID}}"}},
			notAsked: "nerdctl",
		},
		{
			name: "containerd answers", rules: []rule{mobySilent, nerdctlAnswers}, code: statusDone,
			want: []string{"engine: " + rancherIdentity("nerdctl"), "cactusfs is not installed on Rancher Desktop (engine containerd)"},
			asked: [][]string{
				{"docker", "--context", "rancher-desktop", "info", "--format", "{{.ID}}"},
				{"nerdctl", "--namespace", "default", "info", "--format", "{{.ID}}"},
				{"nerdctl", "--namespace", "default", "ps", "-a"},
			},
		},
		{
			name: "an empty answer is no answer", rules: []rule{{Tool: "docker", Contains: []string{"info"}}, nerdctlAnswers}, code: statusDone,
			want: []string{"engine: " + rancherIdentity("nerdctl")},
		},
		{
			name: "no docker here", rules: []rule{nerdctlAnswers}, without: []string{"docker"}, code: statusDone,
			want: []string{"engine: " + rancherIdentity("nerdctl")},
		},
		{
			name: "neither answers", rules: []rule{mobySilent, nerdctlSilent}, code: 2,
			want: []string{"Rancher Desktop does not answer: neither the Docker context rancher-desktop (its moby engine) nor nerdctl (its containerd engine) said who it is"},
		},
		{
			name: "moby set", env: "RANCHER_ENGINE=moby\n", rules: []rule{mobySilent, nerdctlAnswers}, code: statusDone,
			want: []string{"engine: " + rancherIdentity("docker")}, notAsked: "nerdctl",
		},
		{
			name: "containerd set", env: "RANCHER_ENGINE=containerd\n", rules: []rule{mobyAnswers, nerdctlAnswers}, code: statusDone,
			want:  []string{"engine: " + rancherIdentity("nerdctl")},
			asked: [][]string{{"nerdctl", "--namespace", "default", "ps", "-a"}}, notAsked: "docker",
		},
		{
			name: "moby set, no docker", env: "RANCHER_ENGINE=moby\n", without: []string{"docker"}, code: 2,
			want: []string{"RANCHER_ENGINE=moby, and there is no docker here"},
		},
		{
			name: "containerd set, no nerdctl", env: "RANCHER_ENGINE=containerd\n", without: []string{"nerdctl"}, code: 2,
			want: []string{"RANCHER_ENGINE=containerd, and there is no nerdctl here"},
		},
		{
			// A context named in ENGINE_CONTEXT must reach Rancher Desktop:
			// its Lima host, lima-0, or an operating system naming it.
			name: "another context", env: "ENGINE_CONTEXT=rd-two\n", rules: []rule{mobyAnswers, engineName("lima-0|Alpine Linux v3.20")}, code: statusDone,
			want: []string{
				"engine: Rancher Desktop, engine moby, docker --context rd-two",
				"Rancher Desktop's moby through the Docker context rd-two (ENGINE_CONTEXT): the engine lima-0, Alpine Linux v3.20",
			},
			asked: [][]string{
				{"docker", "--context", "rd-two", "info", "--format", "{{.ID}}"},
				{"docker", "--context", "rd-two", "info", "--format", "{{.Name}}|{{.OperatingSystem}}"},
			},
		},
		{
			name: "another context on WSL", env: "ENGINE_CONTEXT=rd-two\nRANCHER_ENGINE=moby\n",
			rules: []rule{engineName("WORKSTATION|Rancher Desktop WSL Distribution")}, code: statusDone,
			want: []string{"Rancher Desktop's moby through the Docker context rd-two (ENGINE_CONTEXT): the engine WORKSTATION, Rancher Desktop WSL Distribution"},
		},
		{
			name: "another context, Docker Desktop's", env: "ENGINE_CONTEXT=desktop-linux\n",
			rules: []rule{mobyAnswers, engineName("docker-desktop|Docker Desktop")}, code: 2,
			want: []string{"ENGINE_CONTEXT=desktop-linux reaches the engine 'docker-desktop' (Docker Desktop), which is not Rancher Desktop's"},
		},
		{
			name: "another context that does not answer", env: "ENGINE_CONTEXT=rd-two\nRANCHER_ENGINE=moby\n",
			rules: []rule{{Tool: "docker", Contains: []string{"info", "{{.Name}}|{{.OperatingSystem}}"}, Stderr: "context not found\n", Exit: 1}}, code: 2,
			want: []string{"the Docker context rd-two (ENGINE_CONTEXT) does not answer 'docker info'"},
		},
		{
			// Set, containerd refuses a cluster before any engine is asked.
			name: "a cluster on containerd set", env: "RANCHER_ENGINE=containerd\nDEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\n", args: []string{"--install"},
			rules: []rule{mobyAnswers, nerdctlAnswers}, code: 2, noCalls: true,
			want: []string{"DEPLOY_MODE=cluster on Rancher Desktop's containerd engine"},
		},
		{
			name: "a cluster on containerd", env: "DEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\n", args: []string{"--install"},
			rules: []rule{mobySilent, nerdctlAnswers}, code: 2,
			want: []string{"DEPLOY_MODE=cluster on Rancher Desktop's containerd engine: its nerdctl compose has no 'up --no-deps'"},
		},
		{
			name: "recorded on moby, containerd now", rules: []rule{mobySilent, nerdctlAnswers},
			recorded: rancherIdentity("docker") + "\nengine-id=" + engineID + "\nkms=local\n", code: 2,
			want: []string{"was made on " + rancherIdentity("docker") + "; the settings now point at " + rancherIdentity("nerdctl")},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				r.rules(c.rules...)
				r.without(c.without...)
				if c.recorded != "" {
					put(t, filepath.Join(r.stateOf("rancher-rancher-desktop"), "target"), c.recorded)
				}
				args := c.args
				if args == nil {
					args = []string{"--status"}
				}
				res := rancherRun(r, "KMS=local\n"+c.env, args...)
				res.want(t, c.code, c.want...)
				noChange(t, res)
				if c.code == 2 && c.recorded == "" {
					if _, err := os.Stat(r.stateDir()); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("a refused run made %s (%v)", r.stateDir(), err)
					}
				}
				at := 0
				for _, w := range c.asked {
					i := slices.IndexFunc(res.calls[at:], func(call call) bool {
						return call.Tool == w[0] && len(call.Args) >= len(w)-1 && slices.Equal(call.Args[:len(w)-1], w[1:])
					})
					if i < 0 {
						t.Fatalf("%q was not asked (in this order) of the calls %v", w, res.calls)
					}
					at += i + 1
				}
				if c.noCalls && len(res.calls) != 0 {
					t.Errorf("the refusal asked %v", res.calls)
				}
				if c.notAsked != "" && len(callsOf(res, c.notAsked)) != 0 {
					t.Errorf("%s was run: %v", c.notAsked, callsOf(res, c.notAsked))
				}
			})
		}
	})
}

// TestRancherInstall: a first install of a standalone deployment on each
// engine of Rancher Desktop - the image built there from the binaries, the
// secrets put into their labelled volumes before the node runs (dev-init,
// the volumes, the puts; 10.3's order), the drives and the metadata volume,
// the node run - every engine command the engine's: on containerd
// nerdctl in the namespace default, with no "cp -a" (nerdctl has none) and
// every tar stream put into a container owned by 65532 by number; the node
// restarted "always" on containerd, "unless-stopped" on moby. The record
// names the engine found. No secret is on any command line, in any
// environment or in the output, a traced run's included.
func TestRancherInstall(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range rancherEngines {
			t.Run(e.tool, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				r.keepStdin()
				r.rules(rancherRules(t, engineState{e: e, devInit: devInitOutput(), ready: true})...)
				r.trace = true
				res := rancherRun(r, "KMS=local\nCACTUSFS_LOG_LEVEL=debug\n", "--install")
				r.trace = false
				state := r.stateOf(e.targetID)
				res.want(t, 0, "engine: "+rancherIdentity(e.tool), "ready: cactusfs runs cactusfs:0.7.0-412-",
					"S3: https://127.0.0.1:9000", "admin API and console: https://127.0.0.1:9001", "the CA to trust: "+state+"/config/s3-ca.pem",
					"the console user admin with the password admin (ADR-0014)")
				noSecretAnywhere(t, res, state)
				engineCallsAre(t, res, e)
				modesAre(t, state)
				record := strings.Split(fileOf(t, filepath.Join(state, "target")), "\n")
				if record[0] != rancherIdentity(e.tool) || !slices.Contains(record, "engine-id="+e.id) || !slices.Contains(record, "kms=local") {
					t.Errorf("the record is %q", record)
				}

				run := runOf(t, res)
				img := run[len(run)-2]
				if !strings.HasPrefix(img, "cactusfs:0.7.0-412-") || run[len(run)-1] != "server" {
					t.Fatalf("the node does not run the image built: %q", run[len(run)-2:])
				}
				before(t, steps(res), "probe http://127.0.0.1:9000/", "probe http://127.0.0.1:9001/", "build", "tag "+img, "version",
					"dev-init", "volume cactusfs-secrets", "volume cactusfs-config",
					"put cactusfs-volumes:/var/lib/cactusfs/secrets", "put cactusfs-volumes:/var/lib/cactusfs/config",
					"volume cactusfs-data1", "volume cactusfs-data4", "volume cactusfs-meta", "run "+img, "readyz")
				for _, c := range res.calls {
					if c.Tool == "curl" && slices.Contains(c.Args, "http://127.0.0.1:9001/") {
						break
					}
					if mutating(c) {
						t.Errorf("a change before the ports were asked: %q", c.Args)
					}
				}
				policy := "unless-stopped"
				if e.tool == "nerdctl" {
					policy = "always"
				}
				holds(t, run,
					[]string{"run", "-d", "--name", "cactusfs"},
					[]string{"--label", "io.cactusfs.deploy.name=cactusfs"},
					[]string{"--label", "io.cactusfs.deploy.target=rancher"},
					[]string{"--user", "65532:65532"},
					[]string{"--read-only"},
					[]string{"--cap-drop", "ALL"},
					[]string{"--security-opt", "no-new-privileges"},
					[]string{"--restart", policy},
					[]string{"--pull", "never"},
					[]string{"--publish", "127.0.0.1:9000:9000"},
					[]string{"--publish", "127.0.0.1:9001:9001"},
					[]string{"--mount", "type=volume,source=cactusfs-secrets,target=/var/lib/cactusfs/secrets,readonly"},
					[]string{"--mount", "type=volume,source=cactusfs-config,target=/var/lib/cactusfs/config,readonly"},
					[]string{"--mount", "type=volume,source=cactusfs-meta,target=/var/lib/cactusfs/meta"},
					[]string{"--env", "CACTUSFS_KMS_KEY_FILE=/var/lib/cactusfs/config/kms.json"},
					[]string{"--env", "CACTUSFS_LOG_LEVEL=debug"},
				)
				for _, c := range res.calls {
					s := sub(c)
					if !isEngine(c.Tool) || len(s) < 2 || s[0] != "volume" || s[1] != "create" {
						continue
					}
					if !containsAll(s, []string{"io.cactusfs.deploy.name=cactusfs", "io.cactusfs.deploy.target=rancher"}) || slices.Contains(s, "--opt") {
						t.Errorf("a volume made with %q", c.Args)
					}
				}
				// Every copy into a container: on containerd a stream owned by
				// 65532 with no names, extracted in a private directory of
				// Rancher Desktop's VM which nerdctl copies in (vmPuts); on
				// moby "cp -a -", whose owner the engine sets, of the stream as
				// tar made it.
				var copies []vmPut
				if e.tool == "nerdctl" {
					var gets []vmPut
					copies, gets = vmCopies(t, res, "default")
					if len(gets) != 1 || gets[0].to != "cactusfs-dev-init:/tmp/out" {
						t.Errorf("the reads out of a container: %v, want dev-init's output alone", gets)
					}
				} else {
					for _, c := range res.calls {
						if s := sub(c); isEngine(c.Tool) && len(s) >= 3 && s[0] == "cp" && mutating(c) {
							copies = append(copies, vmPut{stream: c, to: s[len(s)-1]})
						}
					}
				}
				puts := len(copies)
				for _, p := range copies {
					c := p.stream
					// The secrets unit and dev-init's passphrase are the nodes'
					// user's alone: nothing for group or others, whoever owns
					// them.
					to := p.to
					secretUnit := to == "cactusfs-volumes:/var/lib/cactusfs/secrets" || to == "volume cactusfs-secrets"
					modes := 0
					for _, h := range r.tarHeaders(c) {
						owned := h.Uid == 65532 && h.Gid == 65532 && h.Uname == "" && h.Gname == ""
						if owned != (e.tool == "nerdctl") {
							t.Errorf("%q put %s owned by %d:%d (%q:%q)", c.Args, h.Name, h.Uid, h.Gid, h.Uname, h.Gname)
						}
						if secretUnit || (to == "cactusfs-dev-init:/tmp" && path.Base(h.Name) == "kms-passphrase") {
							modes++
							if h.Mode&0o077 != 0 {
								t.Errorf("%q put %s with mode %o: group or others may read it", c.Args, h.Name, h.Mode)
							}
						}
					}
					if (secretUnit || to == "cactusfs-dev-init:/tmp") && modes == 0 {
						t.Errorf("%q put no entry whose mode was checked", c.Args)
					}
				}
				if puts != 3 {
					t.Errorf("%d copies into a container, want 3: dev-init's input and the two volumes", puts)
				}
			})
		}
	})
}

// vmPut is one tar stream put into a container: the call that read it
// and the container's directory it went to.
type vmPut struct {
	stream call
	to     string
}

// vmArgs are the words of rdctl's calls in nerdctl_put, by step.
var vmArgs = map[string][]string{
	"mktemp":  {"shell", "sudo", "mktemp", "-d", "/dev/shm/cactusfs-deploy.XXXXXX"},
	"stat":    {"shell", "sudo", "stat", "-f", "-c", "%T", vmDir},
	"chown":   {"shell", "sudo", "chown", "65532:65532", vmDir},
	"extract": {"shell", "sudo", "tar", "-x", "--same-owner", "-p", "-C", vmDir, "-f", "-"},
	"remove":  {"shell", "sudo", "rm", "-rf", vmDir},
}

// vmPuts is every put into a container on nerdctl, and fails the test
// unless the run's copies are those of nerdctl_put and nerdctl_get
// (vmCopies).
func vmPuts(t *testing.T, res result, ns string) []vmPut {
	t.Helper()
	puts, _ := vmCopies(t, res, ns)
	return puts
}

// volumeRoot is true for rdctl's chown to 65532:65532 or chmod 0700 of a
// secrets' volume in place (nerdctl_volume_put).
func volumeRoot(c call) bool {
	for _, v := range []string{"cactusfs-secrets", "cactusfs-config"} {
		if slices.Equal(c.Args, []string{"shell", "sudo", "chown", "65532:65532", mountpoint(v)}) || slices.Equal(c.Args, []string{"shell", "sudo", "chmod", "0700", mountpoint(v)}) {
			return true
		}
	}
	return false
}

// vmCopies is every put into, and every read out of, a container or a
// volume on nerdctl, and fails the test unless each is nerdctl_put's,
// nerdctl_get's, nerdctl_volume_put's or nerdctl_volume_get's:
//   - into a container: a private directory made in Rancher Desktop's VM
//     and given to 65532, the stream extracted there by root keeping its
//     owners and modes, "nerdctl cp <dir>/. <container>:<dir>" in the
//     namespace ns, the directory removed;
//   - out of a container: a private directory made, "nerdctl cp
//     <container>:<dir> <dir>/", its archive under the directory's own name
//     to standard output, the directory removed;
//   - into or out of a volume: where the volume is asked of nerdctl, then
//     at once the stream extracted there by root keeping its owners and
//     modes, or archived from there under the unit's name - with no helper
//     container and no cp;
//
// with no "cp -" either way, no "-a", every directory made removed, and
// nothing else run through rdctl. A put is the call that read the stream
// and where it went ("<container>:<dir>", or "volume <name>"); a read is
// the call of rdctl that wrote the stream and where it came from.
func vmCopies(t *testing.T, res result, ns string) (puts, gets []vmPut) {
	t.Helper()
	at := func(i int, step string) bool {
		return i >= 0 && i < len(res.calls) && res.calls[i].Tool == "rdctl" && slices.Equal(res.calls[i].Args, vmArgs[step])
	}
	archive := func(i int) bool {
		a := vmGetArgs("")
		return i >= 0 && i < len(res.calls) && res.calls[i].Tool == "rdctl" && len(res.calls[i].Args) == len(a) &&
			slices.Equal(res.calls[i].Args[:len(a)-1], a[:len(a)-1])
	}
	asked := func(i int, volume string) bool {
		return i >= 0 && res.calls[i].Tool == "nerdctl" && slices.Equal(res.calls[i].Args, []string{"--namespace", ns, "volume", "inspect", "--format", "{{.Mountpoint}}", volume})
	}
	made, removed, inVM := 0, 0, 0
	for i, c := range res.calls {
		switch {
		case at(i, "mktemp"):
			made++
		case at(i, "remove"):
			removed++
		case c.Tool == "rdctl" && volumeOf(c, "-x") != "":
			if v := volumeOf(c, "-x"); asked(i-3, v) && slices.Equal(res.calls[i-2].Args, []string{"shell", "sudo", "chown", "65532:65532", mountpoint(v)}) &&
				slices.Equal(res.calls[i-1].Args, []string{"shell", "sudo", "chmod", "0700", mountpoint(v)}) {
				puts = append(puts, vmPut{stream: c, to: "volume " + v})
			} else {
				t.Errorf("rdctl extracted into the volume %s without asking nerdctl where it is, then making its root 65532 0700, first: %q", v, c.Args)
			}
		case c.Tool == "rdctl" && volumeOf(c, "-c") != "":
			if v := volumeOf(c, "-c"); asked(i-1, v) {
				gets = append(gets, vmPut{stream: c, to: "volume " + v})
			} else {
				t.Errorf("rdctl archived the volume %s without asking nerdctl where it is first: %q", v, c.Args)
			}
		case c.Tool == "rdctl" && !at(i, "chown") && !at(i, "extract") && !at(i, "stat") && !archive(i) && !volumeRoot(c):
			t.Errorf("rdctl was run otherwise than by nerdctl_put, nerdctl_get and the volumes' own: %q", c.Args)
		}
		if c.Tool == "nerdctl" && slices.Contains(c.Args, "cactusfs-volumes") {
			t.Errorf("a helper container of the volumes on nerdctl, whose cp does not see them: %q", c.Args)
		}
		s := sub(c)
		if c.Tool != "nerdctl" || len(s) == 0 || s[0] != "cp" {
			continue
		}
		inVM++
		if mutating(c) {
			if !at(i-4, "mktemp") || !at(i-3, "stat") || !at(i-2, "chown") || !at(i-1, "extract") || !at(i+1, "remove") ||
				c.Args[1] != ns || len(s) != 3 || s[1] != vmDir+"/." {
				t.Errorf("nerdctl copied into a container otherwise than through a private directory of the VM: %q", c.Args)
				continue
			}
			puts = append(puts, vmPut{stream: res.calls[i-1], to: s[2]})
			continue
		}
		if !at(i-2, "mktemp") || !at(i-1, "stat") || !archive(i+1) || !at(i+2, "remove") || c.Args[1] != ns || len(s) != 3 ||
			s[2] != vmDir+"/" || !strings.Contains(s[1], ":") || res.calls[i+1].Args[len(res.calls[i+1].Args)-1] != path.Base(s[1]) {
			t.Errorf("nerdctl copied out of a container otherwise than through a private directory of the VM: %q", c.Args)
			continue
		}
		gets = append(gets, vmPut{stream: res.calls[i+1], to: s[1]})
	}
	if made != removed || made != inVM {
		t.Errorf("%d directories made in the VM, %d removed, %d copies by nerdctl's cp", made, removed, inVM)
	}
	return puts, gets
}

// rancherInstalled is a rig of the shell of r on which a first install has
// run on e's engine of Rancher Desktop, and the image its node runs.
func rancherInstalled(t *testing.T, r *rig, e dockerEngine, env string) (*rig, string) {
	t.Helper()
	base := secretsRig(t, r.fresh(t))
	base.rules(rancherRules(t, engineState{e: e, devInit: devInitOutput(), ready: true})...)
	res := rancherRun(base, env, "--install")
	res.want(t, 0, "ready: cactusfs runs")
	run := runOf(t, res)
	return base, run[len(run)-2]
}

// TestRancherSecrets: 10.3's assertions for Rancher Desktop's copy of the
// secrets, its two volumes, on each engine. A second install makes and
// puts nothing, and upgrades the node; a copy in a volume that differs
// from the entry's is refused (exit 2) before anything is put or run; a
// local copy that is missing is put back from the volume; a volume of the
// deployment's names without its label, and another engine behind the
// same settings, are refused (exit 2) with nothing changed - and with no
// STATE_DIR entry when there was none.
func TestRancherSecrets(t *testing.T) {
	t.Parallel()
	const env = "KMS=local\n"
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range rancherEngines {
			t.Run(e.tool, func(t *testing.T) {
				t.Parallel()
				base, img := rancherInstalled(t, r, e, env)

				t.Run("made once", func(t *testing.T) {
					r := clone(t, base)
					st := r.stateOf(e.targetID)
					was := filesUnder(t, st)
					r.rules(rancherRules(t, installedState(t, e, st, img))...)
					res := rancherRun(r, env, "--install")
					res.want(t, 0, "this run upgrades it", "ready: cactusfs runs")
					noSecretAnywhere(t, res, st)
					for _, c := range res.calls {
						s := sub(c)
						if !isEngine(c.Tool) || len(s) == 0 {
							continue
						}
						if s[0] == "start" || (s[0] == "cp" && mutating(c)) || (s[0] == "volume" && len(s) > 1 && s[1] == "create") {
							t.Errorf("a second install made or put the secrets again: %q", c.Args)
						}
					}
					if e.tool == "nerdctl" {
						puts, gets := vmCopies(t, res, "default")
						var from []string
						for _, g := range gets {
							from = append(from, g.to)
						}
						if len(puts) != 0 || !slices.Equal(from, []string{"volume cactusfs-secrets", "volume cactusfs-config"}) {
							t.Errorf("a second install put %d streams and read %q, want the two volumes read alone", len(puts), from)
						}
					}
					if e.tool == "nerdctl" {
						// nerdctl keeps a stopped container's ports: the old node
						// is removed before the new one runs (TestRancherUpgrade).
						before(t, steps(res), "update --restart no cactusfs", "stop cactusfs", "rm cactusfs", "run "+runOf(t, res)[len(runOf(t, res))-2], "readyz")
					} else {
						before(t, steps(res), "stop cactusfs", "rename cactusfs cactusfs-previous", "run "+runOf(t, res)[len(runOf(t, res))-2], "readyz", "rm cactusfs-previous")
					}
					if now := filesUnder(t, st); !mapsEqual(was, now) {
						t.Errorf("the entry changed:\nbefore %v\nafter  %v", keys(was), keys(now))
					}
				})

				t.Run("copies compared", func(t *testing.T) {
					r := clone(t, base)
					st := r.stateOf(e.targetID)
					s := installedState(t, e, st, img)
					s.keys["secrets"]["root-secret-key"] = "another root secret key of a fixture\n"
					r.rules(rancherRules(t, s)...)
					res := rancherRun(r, env, "--install")
					res.want(t, 2, st+"/secrets/root-secret-key and root-secret-key in the volume cactusfs-secrets differ")
					for _, c := range res.calls {
						s := sub(c)
						if !isEngine(c.Tool) || len(s) == 0 {
							continue
						}
						changed := (s[0] == "cp" && mutating(c)) || s[0] == "stop" || (s[0] == "run" && !isVersionCheck(s)) ||
							(s[0] == "volume" && len(s) > 1 && s[1] != "ls" && s[1] != "inspect")
						if changed {
							t.Errorf("a refused install changed the deployment: %q", c.Args)
						}
					}
					noSecretAnywhere(t, res, st)
				})

				t.Run("a local copy put back", func(t *testing.T) {
					r := clone(t, base)
					st := r.stateOf(e.targetID)
					r.rules(rancherRules(t, installedState(t, e, st, img))...)
					want := fileOf(t, filepath.Join(st, "secrets", "root-access-key"))
					remove(t, filepath.Join(st, "secrets", "root-access-key"))
					res := rancherRun(r, env, "--install")
					res.want(t, 0, "the local copy "+st+"/secrets/root-access-key was missing: copied back from the volume cactusfs-secrets")
					if got := fileOf(t, filepath.Join(st, "secrets", "root-access-key")); got != want {
						t.Error("the copy put back is not the volume's")
					}
					modesAre(t, st)
				})

				t.Run("another engine", func(t *testing.T) {
					r := clone(t, base)
					st := r.stateOf(e.targetID)
					was := filesUnder(t, st)
					other := e
					other.id = "ENGINE-TWO"
					r.rules(rancherRules(t, installedState(t, other, st, img))...)
					res := rancherRun(r, env, "--install")
					res.want(t, 2, "the secrets of one deployment are never given to a second engine")
					noChange(t, res)
					if now := filesUnder(t, st); !mapsEqual(was, now) {
						t.Error("a refused install changed the entry")
					}
				})

				t.Run("a volume without the label", func(t *testing.T) {
					r := secretsRig(t, r.fresh(t))
					s := engineState{e: e, devInit: devInitOutput(), ready: true, keys: map[string]map[string]string{"config": {"ready": "x\n"}}}
					r.rules(append([]rule{{Tool: e.tool, Contains: []string{"volume", "inspect", "{{.Name}} " + volumeLabel(e.tool, "io.cactusfs.deploy.name")}, Stdout: "cactusfs-config \n"}}, rancherRules(t, s)...)...)
					res := rancherRun(r, env, "--install")
					res.want(t, 2, "a volume named cactusfs-config is there without the label io.cactusfs.deploy.name=cactusfs")
					noChange(t, res)
					if _, err := os.Stat(r.stateDir()); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("a refused run made %s (%v)", r.stateDir(), err)
					}
				})
			})
		}
	})
}

// mapsEqual is true when the two maps hold the same keys and values.
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// keys is the keys of a map, sorted.
func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestRancherStatusAndUninstall: --status names the node's state, image and
// the version label of its image (nerdctl's containers hold none of their
// image's labels) and /readyz; --uninstall removes the container through
// the engine and keeps the volumes and the entry; a purge deletes them in
// docker.sh's order, through the same engine.
func TestRancherStatusAndUninstall(t *testing.T) {
	t.Parallel()
	const env = "KMS=local\n"
	versionFormat := `{{index .Config.Labels "org.opencontainers.image.version"}}`
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range rancherEngines {
			t.Run(e.tool, func(t *testing.T) {
				t.Parallel()
				base, img := rancherInstalled(t, r, e, env)

				t.Run("status", func(t *testing.T) {
					r := clone(t, base)
					st := r.stateOf(e.targetID)
					r.rules(append([]rule{{Tool: e.tool, Contains: []string{"image", "inspect", versionFormat, img}, Stdout: "0.7.0-412\n"}},
						rancherRules(t, installedState(t, e, st, img))...)...)
					res := rancherRun(r, env, "--status")
					version := "0.7.0-1"
					if e.tool == "nerdctl" {
						version = "0.7.0-412"
					}
					res.want(t, 0, "node: cactusfs, running, image "+img+", version "+version, "/readyz: 200")
					noChange(t, res)
					engineCallsAre(t, res, e)
					for _, c := range callsOf(res, e.tool, "ps", "-a", byLabel) {
						format := c.Args[len(c.Args)-1]
						if (e.tool == "nerdctl") != strings.Contains(format, "{{.Status}}") || (e.tool == "nerdctl") == strings.Contains(format, "{{.State}}") {
							t.Errorf("the listing of %s asked %q", e.tool, format)
						}
					}
				})

				t.Run("uninstall and purge", func(t *testing.T) {
					r := clone(t, base)
					st := r.stateOf(e.targetID)
					r.rules(rancherRules(t, installedState(t, e, st, img))...)
					res := rancherRun(r, env, "--uninstall")
					res.want(t, 0, "removed the container cactusfs", "kept: the volumes")
					before(t, steps(res), "stop cactusfs", "rm cactusfs")
					engineCallsAre(t, res, e)
					if _, err := os.Stat(st); err != nil {
						t.Fatalf("the entry is gone after an uninstall: %v", err)
					}

					s := installedState(t, e, st, img)
					s.ctr, s.ports, s.purged = "", "", true
					r.rules(rancherRules(t, s)...)
					res = rancherRun(r, env, "--uninstall", "--purge", "--purge-confirm", "cactusfs")
					res.want(t, 0, "purged: cactusfs and its secrets are gone from Rancher Desktop (engine "+map[string]string{"docker": "moby", "nerdctl": "containerd"}[e.tool]+")")
					got := steps(res)
					before(t, got, "volume rm cactusfs-data1 cactusfs-data2 cactusfs-data3 cactusfs-data4 cactusfs-meta", "volume rm cactusfs-config cactusfs-secrets")
					for _, rm := range []string{"volume rm cactusfs-data1 cactusfs-data2 cactusfs-data3 cactusfs-data4 cactusfs-meta", "volume rm cactusfs-config cactusfs-secrets"} {
						if i := index(got, rm); i+1 >= len(got) || got[i+1] != "listing" {
							t.Errorf("no listing proves %q: %q", rm, got)
						}
					}
					engineCallsAre(t, res, e)
					if _, err := os.Stat(st); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("the entry is there after the purge (%v)", err)
					}
				})
			})
		}
	})
}

// TestRancherCluster: a cluster on Rancher Desktop's moby is compose's
// cluster of PLAN 10.8 through "docker --context rancher-desktop compose",
// recorded with its mode; on containerd it is refused (TestRancherEngine).
func TestRancherCluster(t *testing.T) {
	t.Parallel()
	e := rancherEngines[0]
	eachShell(t, func(t *testing.T, r *rig) {
		r = secretsRig(t, r.fresh(t))
		r.rules(clusterState{e: e, devInit: devInitOutput()}.rules(t)...)
		res := clusterRun(r, e, clusterEnv, "--install")
		res.want(t, 0, "engine: "+rancherIdentity("docker"), "ready: the cluster cactusfs runs "+nodeImage)
		state := r.stateOf(e.targetID)
		noSecretAnywhere(t, res, state)
		engineCallsAre(t, res, e)
		ups := 0
		for _, c := range res.calls {
			if i := slices.Index(c.Args, "compose"); i >= 0 && c.Tool == "docker" {
				if i != 2 {
					t.Errorf("compose was run as %q", c.Args)
				}
				if slices.Contains(c.Args, "up") {
					ups++
				}
			}
		}
		if ups == 0 {
			t.Fatal("compose never ran up")
		}
		record := strings.Split(fileOf(t, filepath.Join(state, "target")), "\n")
		if record[0] != rancherIdentity("docker") || !slices.Contains(record, "mode=cluster") {
			t.Errorf("the record is %q", record)
		}
	})
}

// TestRancherKubernetes: with RANCHER_KUBERNETES=yes the image for Rancher
// Desktop's k3s is made on the engine here - on containerd by nerdctl in
// the namespace k8s.io, which its k3s sees, on the route "local"; on moby
// by the Docker CLI of the context rancher-desktop. The deployment's whole
// lifecycle on the Kubernetes path is TestRancherKubernetesLifecycle's.
func TestRancherKubernetes(t *testing.T) {
	t.Parallel()
	const k8s = "DEPLOY_TARGET=rancher\nRANCHER_KUBERNETES=yes\nK8S_S3_ALLOW_FROM=192.0.2.0/24\n"
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range []struct {
			name, env string
			rules     []rule
		}{
			{"containerd set", "RANCHER_ENGINE=containerd\n", nil},
			{"containerd found", "", []rule{
				{Tool: "docker", Contains: []string{"info", "{{.ID}}"}, Exit: 1},
				{Tool: "nerdctl", Contains: []string{"--namespace", "k8s.io", "info", "{{.ID}}"}, Stdout: containerdID + "\n"},
			}},
		} {
			t.Run("image on "+c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				r.imageBinaries(imgCommit, runtime.GOARCH)
				f := newImgFix(imgCommit)
				rules := append(slices.Clone(c.rules), kubeServer("https://127.0.0.1:6443"))
				rules = append(rules, nodeRules("kubectl", runtime.GOARCH)...)
				rules = append(rules, gitRules("")...)
				r.rules(append(rules, madeHere("nerdctl", f)...)...)
				res := r.image("KMS=local\n"+k8s+c.env, resolveAndDeliver, false)
				// nerdctl's .Id, the configuration's digest, is kept: a saved
				// tar would be checked against it.
				res.want(t, 0, "resolved "+f.ref()+" "+runtime.GOARCH, "in the namespace k8s.io of the containerd here (nerdctl)",
					"built "+f.hex+" (manifest; configuration "+f.configHex()+")")
				if len(builds(res)) != 1 {
					t.Fatalf("built %d times: %v", len(builds(res)), builds(res))
				}
				for _, call := range res.calls {
					switch call.Tool {
					case "nerdctl":
						if len(call.Args) < 2 || call.Args[0] != "--namespace" || call.Args[1] != "k8s.io" {
							t.Errorf("nerdctl was run outside the namespace k8s.io: %q", call.Args)
						}
					case "docker", "podman":
						if !slices.Contains(call.Args, "info") {
							t.Errorf("%s was run for an image of containerd: %q", call.Tool, call.Args)
						}
					}
				}
				// The tag is made from the build's name, which was checked to
				// name the image: nerdctl's "image tag" takes no ID.
				tags := callsOf(res, "nerdctl", "image", "tag")
				if len(tags) != 1 || !slices.Equal(sub(tags[0])[2:], []string{workTag(runtime.GOARCH), f.ref()}) {
					t.Errorf("tagged as %v", tags)
				}
			})
		}
	})
}

// TestRancherGivenImage: on nerdctl an IMAGE named <repository>@sha256:<hex>
// is inspected by its bare digest - nerdctl's inspect of a repository with a
// digest takes only an image tagged latest (v2.4.2 pkg/cmd/image/inspect.go)
// - and is taken only when its repository is among the RepoDigests; one
// whose RepoDigests name another repository is pulled, and still refused
// after the pull.
func TestRancherGivenImage(t *testing.T) {
	t.Parallel()
	e := rancherEngines[1]
	hex := strings.TrimPrefix(nodeImage[strings.Index(nodeImage, "@"):], "@sha256:")
	bare := "sha256:" + hex
	f := newImgFix(imgCommit)
	cases := []struct {
		name, repo string
		code       int
		want       string
		pulled     bool
	}{
		{"its repository listed", "registry.example.test/cactusfs", 0, "ready: cactusfs runs " + nodeImage, false},
		{"another repository", "other.example.test/cactusfs", 1, "does not name the repository registry.example.test/cactusfs among its RepoDigests", true},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				digest := c.repo + "@" + bare
				r.rules(append([]rule{
					{Tool: "nerdctl", Contains: []string{"inspect", "{{range .RepoDigests}}{{.}} {{end}}", bare}, Stdout: digest + " \n"},
					{Tool: "nerdctl", Contains: []string{"inspect", bare}, Stdout: fieldsLine(f.configHex(), "linux/"+runtime.GOARCH, imgCommit, "0.7.0-412", f.layers, digest)},
					{Tool: "nerdctl", Contains: []string{"--rm", "version", hex}, Stdout: versionLine(imgCommit, runtime.GOARCH)},
				}, rancherRules(t, engineState{e: e, devInit: devInitOutput(), ready: true})...)...)
				res := rancherRun(r, "KMS=local\nIMAGE_SOURCE=image\nIMAGE="+nodeImage+"\n", "--install")
				res.want(t, c.code, c.want)
				for _, call := range callsOf(res, "nerdctl", "inspect", nodeImage) {
					t.Errorf("nerdctl inspected the image by its repository and digest: %q", call.Args)
				}
				if got := len(callsOf(res, "nerdctl", "pull")) != 0; got != c.pulled {
					t.Errorf("pulled: %v, want %v", got, c.pulled)
				}
				if c.code == 0 {
					run := runOf(t, res)
					if run[len(run)-2] != nodeImage {
						t.Errorf("the node runs %q", run[len(run)-2])
					}
				}
			})
		}
	})
}

// rancherKubeID is the ID a tag made for Rancher Desktop's Kubernetes on
// the tool's engine is recorded with: the cluster's (the UID of its
// kube-system), a slash, and the engine's.
func rancherKubeID(tool string) string {
	if tool == "nerdctl" {
		return clusterUID + "/" + containerdID
	}
	return clusterUID + "/" + engineID
}

// rancherKubeEngine is the answer of the engine whose images Rancher
// Desktop's Kubernetes sees, on the tool, to "info --format {{.ID}}":
// nerdctl in the namespace k8s.io, or the Docker CLI of the context
// rancher-desktop.
func rancherKubeEngine(tool string) rule {
	if tool == "nerdctl" {
		return rule{Tool: "nerdctl", Contains: []string{"--namespace", "k8s.io", "info", "--format", "{{.ID}}"}, Stdout: containerdID + "\n"}
	}
	return rule{Tool: "docker", Contains: []string{"--context", "rancher-desktop", "info", "--format", "{{.ID}}"}, Stdout: engineID + "\n"}
}

// rancherKube is the fake Kubernetes of Rancher Desktop, reached by the
// context rancher-desktop, whose API server is on this machine, on the
// engine of the tool: the image f made there (the engine lacked its tag),
// the chart rendered in the helm image there, dev-init run there. On
// containerd every one of them answers nerdctl's.
func rancherKube(t *testing.T, tool string, k kube) []rule {
	t.Helper()
	extra := []rule{kubeServer("https://127.0.0.1:6443"), tagAbsent(tool, k.f), rancherKubeEngine(tool)}
	extra = append(extra, madeHere(tool, k.f)...)
	if tool == "nerdctl" {
		extra = append(extra,
			rule{Tool: "nerdctl", Contains: []string{"image", "inspect", dockerfileHelm(t)}, Stdout: "[{}]\n"},
			rule{Tool: "nerdctl", Contains: []string{"--tmpfs", "/work"}, Stdout: kubeRender(k.mode), SaveStdin: "render.tar"},
			rule{Tool: "nerdctl", Contains: []string{"cp"}, Starts: []string{"cp", vmDir + "/."}}, vmMktemp, vmStat)
		if k.devInit != nil {
			extra = append(extra, vmArchive("out", tarOf(t, "out", k.devInit)))
		}
	}
	k.extra = append(extra, k.extra...)
	return k.rules(t)
}

// rancherKubeCalls fails the test unless every engine call is the one of
// Rancher Desktop's Kubernetes on the tool: on containerd nerdctl in the
// namespace k8s.io (the image, dev-init) or default (the helm image) and no
// docker; on moby the Docker CLI with the context rancher-desktop and no
// nerdctl; kubectl always with the context rancher-desktop.
func rancherKubeCalls(t *testing.T, res result, tool string) {
	t.Helper()
	for _, c := range res.calls {
		switch c.Tool {
		case "kubectl":
			if len(c.Args) < 2 || c.Args[0] != "--context" || c.Args[1] != "rancher-desktop" {
				t.Errorf("kubectl was run without the context rancher-desktop: %q", c.Args)
			}
		case "podman":
			t.Errorf("podman was run for Rancher Desktop: %q", c.Args)
		case "nerdctl":
			if tool != "nerdctl" {
				t.Errorf("nerdctl was run for Rancher Desktop's moby: %q", c.Args)
				continue
			}
			helm := slices.Contains(c.Args, "--tmpfs") || slices.Contains(c.Args, dockerfileHelm(t))
			switch {
			case len(c.Args) < 2 || c.Args[0] != "--namespace":
				t.Errorf("nerdctl was run without its namespace: %q", c.Args)
			case helm && c.Args[1] != "default":
				t.Errorf("the helm image was run outside the namespace default: %q", c.Args)
			case !helm && c.Args[1] != "k8s.io":
				t.Errorf("nerdctl was run outside the namespace k8s.io: %q", c.Args)
			}
		case "docker":
			if tool == "nerdctl" {
				t.Errorf("docker was run for Rancher Desktop's containerd: %q", c.Args)
				continue
			}
			if slices.Equal(c.Args[:min(len(c.Args), 3)], []string{"context", "inspect", "rancher-desktop"}) {
				continue
			}
			if len(c.Args) < 2 || c.Args[0] != "--context" || c.Args[1] != "rancher-desktop" {
				t.Errorf("docker was run without the context rancher-desktop: %q", c.Args)
			}
		}
	}
}

// TestRancherKubernetesLifecycle: RANCHER_KUBERNETES=yes hands Rancher
// Desktop's own Kubernetes to the Kubernetes path of PLAN 10.10 and 10.11,
// on each of its engines, through the context rancher-desktop: an install
// builds the image on the engine its k3s sees (nerdctl in the namespace
// k8s.io, or the Docker CLI of the context rancher-desktop), runs dev-init
// there, makes the namespace and its Secrets (PLAN 10.9's path, created
// once, never on a command line, in an environment or in a trace), renders
// the chart there with imagePullPolicy Never and the built tag, and records
// that tag; an install of new binaries upgrades the node; --status, an
// uninstall that keeps the claims, and a purge that removes the recorded
// tags no container of that engine runs, by its own CLI, before the entry.
func TestRancherKubernetesLifecycle(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range []struct{ engine, tool string }{{"containerd", "nerdctl"}, {"moby", "docker"}} {
			t.Run(c.engine, func(t *testing.T) {
				t.Parallel()
				env := "DEPLOY_TARGET=rancher\nRANCHER_KUBERNETES=yes\nRANCHER_ENGINE=" + c.engine + "\nKMS=local\nK8S_S3_ALLOW_FROM=192.0.2.0/24\nTLS_HOSTS=s3.example.test\nWAIT_TIMEOUT=3\n"
				r := secretsRig(t, r.fresh(t))
				r.chart()
				state := r.stateOf("rancher-rancher-desktop")
				run := func(args ...string) result {
					t.Helper()
					tmp := t.TempDir()
					res := r.run(append([]string{"--env", r.envFile("site.env", env)}, args...), "TMPDIR="+tmp)
					if left, err := os.ReadDir(tmp); err != nil || len(left) != 0 {
						t.Errorf("the run (exit %d) left %v in TMPDIR (%v)", res.code, left, err)
					}
					return res
				}

				// Install.
				f1 := newImgFix(imgCommit)
				k := kube{tool: "kubectl", mode: "standalone", f: f1, image: f1.ref(), devInit: k8sDevInit("standalone")}
				r.rules(rancherKube(t, c.tool, k)...)
				r.trace = true
				res := run("--install")
				r.trace = false
				res.want(t, 0, "engine: kubectl --context rancher-desktop", "image: the pods run "+f1.ref()+" (imagePullPolicy Never)",
					"ready: cactusfs-node-0 runs "+f1.ref(), "image: recorded "+f1.ref()+" (on the engine whose images Rancher Desktop's Kubernetes sees, engine "+rancherKubeID(c.tool)+")")
				if strings.Contains(res.output(), "not built yet") {
					t.Error("the run says something is not built")
				}
				rancherKubeCalls(t, res, c.tool)
				insideNamespace(t, r, res.calls, "cactusfs")
				noForce(t, res.calls)
				if b := builds(res); len(b) != 1 || b[0].Tool != c.tool {
					t.Fatalf("built %v", b)
				}
				files := tarFiles(t, []byte(read(t, filepath.Join(r.dir, "render.tar"))))
				if !strings.Contains(files["values.yaml"], "  repository: 'cactusfs'\n  tag: '0.7.0-412-"+f1.id12()+"'\n  digest: ''\n  pullPolicy: Never\n") {
					t.Errorf("the values do not run the tag built with imagePullPolicy Never:\n%s", files["values.yaml"])
				}
				if got := read(t, filepath.Join(state, "target")); got != "kubectl --context rancher-desktop\nengine-id="+clusterUID+"\nkms=local\n" {
					t.Errorf("the record is %q", got)
				}
				if got := read(t, filepath.Join(state, "images")); got != "target "+f1.ref()+" "+rancherKubeID(c.tool)+"\n" {
					t.Errorf("the entry records the images %q", got)
				}
				made := entryFiles(t, state)
				for _, key := range standaloneKeys {
					if made[key.file] == "" {
						t.Errorf("%s was not made", key.file)
					}
				}
				checkCreated(t, r, standaloneKeys, made)
				noLeak(t, res, made)
				noSecretAnywhere(t, res, state)
				devInit := callsOf(res, c.tool, "--name", "cactusfs-dev-init")
				if len(devInit) != 1 || !containsRun(devInit[0].Args, []string{"--network", "none"}) {
					t.Errorf("dev-init is not run once on the engine, with no network: %v", devInit)
				}

				// An upgrade to new binaries: the node is replaced, the
				// Secrets are not made again, and once it is Ready the tag
				// it ran is removed - nothing of the cluster names it (its
				// pods and workloads of every namespace listed first) and
				// no container of the engine runs it.
				f2 := newImgFix(imgCommit)
				k = kube{
					tool: "kubectl", mode: "standalone", f: f2, image: f2.ref(), podImage: f1.ref(),
					marker: strings.TrimSpace(read(t, filepath.Join(state, "namespace-uid"))), objects: kubeInstalled("standalone"), secrets: made,
				}
				r.rules(rancherKube(t, c.tool, k)...)
				res = run("--install")
				res.want(t, 0, "this run upgrades it one node at a time", "upgrade: cactusfs-node-0 is Ready on "+f2.ref())
				rancherKubeCalls(t, res, c.tool)
				if got := kubeDeletes(res.calls); !slices.Equal(got, []string{"cactusfs-node-0-0"}) {
					t.Errorf("the pods deleted are %q", got)
				}
				for _, call := range res.calls {
					if slices.Contains(call.Args, "cactusfs-dev-init") {
						t.Errorf("dev-init ran again: %q", call.Args)
					}
				}
				if !maps.Equal(entryFiles(t, state), made) {
					t.Error("the upgrade changed the secrets of the entry")
				}
				res.want(t, 0, "image: removed "+f1.ref()+" on the engine whose images Rancher Desktop's Kubernetes sees, which the script had made")
				if got := imageRemovals(res, c.tool); !slices.Equal(got, []string{f1.ref()}) {
					t.Errorf("the upgrade removed %q, want %s", got, f1.ref())
				}
				ready := lineAt(res, "upgrade: cactusfs-node-0 is Ready on "+f2.ref())
				if removed := lineAt(res, "image: removed "+f1.ref()); removed < ready {
					t.Errorf("the tag the node ran was removed (line %d) before the node was Ready (line %d)", removed, ready)
				}
				for _, kinds := range []string{"pods", "statefulsets,deployments,daemonsets,jobs,cronjobs"} {
					if i, rm := callIndex(res, "kubectl", "get", kinds, "-A"), callIndex(res, c.tool, "image", "rm", f1.ref()); i < 0 || rm < i {
						t.Errorf("the %s of every namespace listed at %d, the tag removed at %d", kinds, i, rm)
					}
				}
				if got := read(t, filepath.Join(state, "images")); got != "target "+f2.ref()+" "+rancherKubeID(c.tool)+"\n" {
					t.Errorf("the entry records the images %q", got)
				}

				// Status.
				k.podImage = f2.ref()
				k.extra = []rule{
					{Tool: "kubectl", Contains: []string{"get", "services", "cactusfs", "cactusfs-admin"}, Stdout: kjsonNoT(kubeList(
						obj{"metadata": obj{"name": "cactusfs"}, "spec": obj{"type": "NodePort", "ports": []any{obj{"port": 9000, "nodePort": 30900}}}},
						obj{"metadata": obj{"name": "cactusfs-admin"}, "spec": obj{"type": "ClusterIP", "ports": []any{obj{"port": 9001}}}}))},
					{Tool: "kubectl", Contains: []string{"get", "pods"}, Stdout: kjson(t, kubeList(podOf("cactusfs-node-0-0", "u1", "r", f2.ref(), true, true)))},
				}
				r.rules(k.rules(t)...)
				res = run("--status")
				res.want(t, 0, "node: cactusfs-node-0, pod cactusfs-node-0-0 on na, ready, image "+f2.ref(),
					"kubectl --context rancher-desktop port-forward -n cactusfs service/cactusfs-admin 9001:9001")
				rancherKubeCalls(t, res, c.tool)
				for _, call := range res.calls {
					if args, ok := kubectlArgs(call); ok && len(args) > 0 && args[0] != "get" && args[0] != "config" && args[0] != "auth" {
						t.Errorf("--status changed %q", args)
					}
				}

				// Uninstall: the claims, the namespace and the entry are kept.
				k.extra = []rule{
					{Tool: "kubectl", Contains: []string{"get", "secrets", "io.cactusfs.deploy.name=cactusfs,app.kubernetes.io/managed-by=cactusfs-deploy"}, Stdout: "secret/cactusfs-root\nsecret/cactusfs-tls\n"},
					{Tool: "kubectl", Contains: []string{"get", "persistentvolumeclaims"}, Stdout: kjsonNoT(kubeList(obj{"metadata": obj{"name": "meta-cactusfs-node-0-0"}}, obj{"metadata": obj{"name": "data1-cactusfs-node-0-0"}}))},
				}
				r.rules(k.rules(t)...)
				res = run("--uninstall")
				res.want(t, 0, "kept: the claims data1-cactusfs-node-0-0 meta-cactusfs-node-0-0, the namespace cactusfs")
				rancherKubeCalls(t, res, c.tool)
				if got := imageRemovals(res, c.tool); len(got) != 0 {
					t.Errorf("an uninstall removed the images %q", got)
				}
				if _, err := os.Stat(filepath.Join(state, "images")); err != nil {
					t.Errorf("an uninstall dropped the record of the images: %v", err)
				}

				// Purge: the second tag still runs in a container of the
				// engine (a pod of another namespace), the first does not
				// (recorded again here, as by an upgrade that could not
				// remove it).
				put(t, filepath.Join(state, "images"), "target "+f1.ref()+" "+rancherKubeID(c.tool)+"\ntarget "+f2.ref()+" "+rancherKubeID(c.tool)+"\n")
				claims := kubeList(obj{"metadata": obj{"name": "meta-cactusfs-node-0-0"}}, obj{"metadata": obj{"name": "data1-cactusfs-node-0-0"}})
				k.extra = []rule{
					rancherKubeEngine(c.tool),
					usedBy(c.tool, "registry.example.test/web:1", asRun(c.tool, f2.ref())),
					{Tool: "kubectl", Contains: []string{"get", "persistentvolumeclaims"}, After: []string{"delete", "persistentvolumeclaim/meta-cactusfs-node-0-0"}, Stdout: kjsonNoT(kubeList())},
					{Tool: "kubectl", Contains: []string{"get", "persistentvolumeclaims"}, Stdout: kjsonNoT(claims)},
					{Tool: "kubectl", Contains: []string{"get", "persistentvolumes"}, Stdout: kjsonNoT(kubeList())},
					{Tool: "kubectl", Contains: []string{"get", "secrets", "io.cactusfs.deploy.name=cactusfs,app.kubernetes.io/managed-by=cactusfs-deploy"}, After: []string{"delete", "secret/cactusfs-root"}, Stdout: ""},
					{Tool: "kubectl", Contains: []string{"get", "secrets", "io.cactusfs.deploy.name=cactusfs,app.kubernetes.io/managed-by=cactusfs-deploy"}, Stdout: "secret/cactusfs-root\nsecret/cactusfs-tls\n"},
					{Tool: "kubectl", Contains: []string{"api-resources", "--namespaced"}, Stdout: "configmaps\npods\nserviceaccounts\nsecrets\nendpoints\nendpointslices.discovery.k8s.io\ndeployments.apps\nevents\nevents.events.k8s.io\n"},
					{Tool: "kubectl", Contains: []string{"get", purgeKinds}, Stdout: "configmap/kube-root-ca.crt\nserviceaccount/default\n"},
					{Tool: "kubectl", Contains: []string{"delete", "--raw"}, SaveStdin: "delete.json"},
				}
				r.rules(k.rules(t)...)
				res = run("--uninstall", "--purge", "--purge-confirm=cactusfs")
				res.want(t, 0, "image: removed "+f1.ref()+" on the engine whose images Rancher Desktop's Kubernetes sees, which the script had made",
					"image: "+f2.ref()+" is kept: a container on the engine whose images Rancher Desktop's Kubernetes sees still runs it", "purged: cactusfs")
				rancherKubeCalls(t, res, c.tool)
				if got := imageRemovals(res, c.tool); !slices.Equal(got, []string{f1.ref()}) {
					t.Errorf("the purge removed %q, want %s alone", got, f1.ref())
				}
				ns := slices.IndexFunc(res.calls, func(c call) bool { return slices.Contains(c.Args, "--raw") })
				rm := callIndex(res, c.tool, "image", "rm", f1.ref())
				if ns < 0 || rm < ns {
					t.Errorf("the namespace was deleted at %d, the image removed at %d", ns, rm)
				}
				if lineAt(res, "image: removed "+f1.ref()) > lineAt(res, "purge: deleted "+state) {
					t.Error("the image was removed after the entry")
				}
				if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the entry is still there: %v", err)
				}
			})
		}
	})
}

// lastCalls fails the test unless the run's last calls are these, in this
// order, each a tool and its whole argv: what the run's EXIT trap removed
// after the failure.
func lastCalls(t *testing.T, res result, want ...call) {
	t.Helper()
	if len(res.calls) < len(want) {
		t.Fatalf("%d calls, want at least %d", len(res.calls), len(want))
	}
	got := res.calls[len(res.calls)-len(want):]
	for i, w := range want {
		if got[i].Tool != w.Tool || !slices.Equal(got[i].Args, w.Args) {
			t.Fatalf("the last calls are not the cleanups %v:\n%v", want, got)
		}
	}
}

// TestRancherPutFails: on containerd a put that fails - the stream not
// extracted in Rancher Desktop's VM, or nerdctl's copy of the directory -
// fails the run (exit 1), and the run's EXIT trap still removes the
// private directory of the VM, then the helper container the put went to,
// in the namespace that made it; dev-init that fails leaves no container
// either. An answer of mktemp that is not a directory it makes fails the
// run and is never removed. No secret is in an argument, an environment or
// the output. Without rdctl an install is refused (exit 2) before anything
// is changed, and no entry is made.
func TestRancherPutFails(t *testing.T) {
	t.Parallel()
	e := rancherEngines[1]
	broke := func(r rule) rule {
		r.Stderr, r.Exit = "it broke\n", 1
		return r
	}
	vmRemove := call{Tool: "rdctl", Args: vmArgs["remove"]}
	removed := func(ctr string) call {
		return call{Tool: "nerdctl", Args: []string{"--namespace", "default", "rm", "-f", ctr}}
	}
	cases := []struct {
		name    string
		fail    rule
		want    string
		cleanup []call
	}{
		{
			"the extraction of dev-init's input fails", broke(rule{Tool: "rdctl", Contains: vmArgs["extract"]}),
			"extracting the stream into " + vmDir + " in Rancher Desktop's VM failed",
			[]call{vmRemove, removed("cactusfs-dev-init")},
		},
		{
			"the copy of dev-init's input fails", broke(rule{Tool: "nerdctl", Contains: []string{"cp", "cactusfs-dev-init:/tmp"}, Starts: []string{"cp", vmDir + "/."}}),
			"copying " + vmDir + " of Rancher Desktop's VM into cactusfs-dev-init:/tmp failed",
			[]call{vmRemove, removed("cactusfs-dev-init")},
		},
		{
			"dev-init fails", broke(rule{Tool: "nerdctl", Contains: []string{"start", "-a", "cactusfs-dev-init"}}),
			"dev-init failed in its container",
			[]call{removed("cactusfs-dev-init")},
		},
		{
			// A volume is filled in place: no directory of the VM, no helper.
			"the extraction into a volume fails",
			broke(rule{Tool: "rdctl", Contains: volPutArgs("cactusfs-secrets")}),
			"extracting the stream into the volume cactusfs-secrets in Rancher Desktop's VM failed",
			[]call{{Tool: "rdctl", Args: volPutArgs("cactusfs-secrets")}},
		},
		{
			"where a volume is cannot be asked",
			broke(mountRule("cactusfs-secrets", "")),
			"could not ask nerdctl where the volume cactusfs-secrets is",
			[]call{{Tool: "nerdctl", Args: []string{"--namespace", "default", "volume", "inspect", "--format", "{{.Mountpoint}}", "cactusfs-secrets"}}},
		},
		{
			"a volume is said to be elsewhere",
			mountRule("cactusfs-secrets", "/etc\n"),
			"internal: nerdctl says the volume cactusfs-secrets is at '/etc', which is not /var/lib/nerdctl/<digest>/volumes/default/cactusfs-secrets/_data",
			[]call{{Tool: "nerdctl", Args: []string{"--namespace", "default", "volume", "inspect", "--format", "{{.Mountpoint}}", "cactusfs-secrets"}}},
		},
		{
			"the copy out of dev-init fails",
			broke(rule{Tool: "nerdctl", Contains: []string{"cp", "cactusfs-dev-init:/tmp/out", vmDir + "/"}}),
			"copying cactusfs-dev-init:/tmp/out into " + vmDir + " of Rancher Desktop's VM failed",
			[]call{vmRemove, removed("cactusfs-dev-init")},
		},
		{
			"the archive of dev-init's output fails",
			broke(vmArchive("out", "")),
			"reading " + vmDir + "/out of Rancher Desktop's VM as a tar stream failed",
			[]call{vmRemove, removed("cactusfs-dev-init")},
		},
		{
			// The second mktemp: the read of dev-init's output.
			"mktemp of the read of dev-init's output fails",
			broke(rule{Tool: "rdctl", Contains: vmArgs["mktemp"], After: vmArgs["remove"]}),
			"making a private directory in Rancher Desktop's VM failed",
			[]call{removed("cactusfs-dev-init")},
		},
		{
			"mktemp answers another path",
			rule{Tool: "rdctl", Contains: vmArgs["mktemp"], Stdout: "/etc\n"},
			"mktemp in Rancher Desktop's VM answered '/etc', which is not a directory of the template /dev/shm/cactusfs-deploy.XXXXXX",
			[]call{removed("cactusfs-dev-init")},
		},
		{
			"the directory is not on a tmpfs",
			rule{Tool: "rdctl", Contains: vmArgs["stat"], Stdout: "ext2/ext3\n"},
			vmDir + " in Rancher Desktop's VM is on 'ext2/ext3', not a tmpfs: a secret would reach the VM's disk, so nothing was written there",
			[]call{vmRemove, removed("cactusfs-dev-init")},
		},
		{
			"mktemp fails", broke(rule{Tool: "rdctl", Contains: vmArgs["mktemp"]}),
			"making a private directory in Rancher Desktop's VM failed",
			[]call{removed("cactusfs-dev-init")},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				r.rules(append([]rule{c.fail}, rancherRules(t, engineState{e: e, devInit: devInitOutput(), ready: true})...)...)
				res := rancherRun(r, "KMS=local\n", "--install")
				res.want(t, 1, c.want)
				lastCalls(t, res, c.cleanup...)
				made, gone := 0, 0
				for _, call := range res.calls {
					if call.Tool == "rdctl" && slices.Equal(call.Args, vmArgs["mktemp"]) {
						made++
					}
					if call.Tool == "rdctl" && slices.Equal(call.Args, vmArgs["remove"]) {
						gone++
					}
					if call.Tool == "rdctl" && len(call.Args) > 3 && call.Args[2] == "rm" && !slices.Equal(call.Args, vmArgs["remove"]) {
						t.Errorf("rdctl removed something other than the directory it made: %q", call.Args)
					}
					if s := sub(call); call.Tool == "nerdctl" && len(s) > 1 && s[0] == "run" && slices.Contains(s, "server") {
						t.Errorf("the node ran after a failed put: %q", call.Args)
					}
				}
				// Every directory made in the VM is removed - but for an
				// answer of mktemp that is not one it makes, and a mktemp
				// that failed.
				if c.fail.Tool == "rdctl" && slices.Equal(c.fail.Contains, vmArgs["mktemp"]) {
					made--
				}
				if made != gone {
					t.Errorf("%d directories made in the VM, %d removed", made, gone)
				}
				noSecretAnywhere(t, res, r.stateOf(e.targetID))
			})
		}
		t.Run("no rdctl", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			r.without("rdctl")
			r.rules(rancherRules(t, engineState{e: e, devInit: devInitOutput(), ready: true})...)
			res := rancherRun(r, "KMS=local\n", "--install")
			res.want(t, 2, "there is no rdctl on PATH or beside nerdctl", "put Rancher Desktop's tools (~/.rd/bin) on PATH")
			noChange(t, res)
			if _, err := os.Stat(r.stateDir()); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused run made %s (%v)", r.stateDir(), err)
			}
		})
	})
}

// TestRancherRotation: on containerd a rotation of the certificate, its key
// and its CA is put into the volume cactusfs-config through a private
// directory of Rancher Desktop's VM - only the three files given, owned by
// the nodes' user, private to it - the volume's copy before the local one;
// a second run with the same files puts nothing.
func TestRancherRotation(t *testing.T) {
	t.Parallel()
	e := rancherEngines[1]
	eachShell(t, func(t *testing.T, r *rig) {
		base, img := rancherInstalled(t, r, e, "KMS=local\n")
		r = clone(t, base)
		r.keepStdin()
		state := r.stateOf(e.targetID)
		given := map[string]string{"s3.pem": "a new certificate " + rand.Text(), "s3.key": "a new key " + rand.Text(), "s3-ca.pem": "a new CA " + rand.Text()}
		env := "KMS=local\n"
		var files []string
		for _, kv := range [][2]string{{"TLS_CERT_FILE", "s3.pem"}, {"TLS_KEY_FILE", "s3.key"}, {"TLS_CA_FILE", "s3-ca.pem"}} {
			path := r.envFile(kv[1], given[kv[1]])
			files = append(files, path)
			env += kv[0] + "=" + path + "\n"
		}
		r.rules(rancherRules(t, installedState(t, e, state, img))...)
		res := rancherRun(r, env, "--install")
		res.want(t, 0, "rotated TLS_CERT_FILE: "+state+"/config/s3.pem replaced, after the volume's copy")
		puts := vmPuts(t, res, "default")
		if len(puts) != 1 || puts[0].to != "volume cactusfs-config" {
			t.Fatalf("the rotation's puts: %v", puts)
		}
		headers := r.tarHeaders(puts[0].stream)
		got := map[string]string{}
		for name, data := range tarFiles(t, []byte(read(t, filepath.Join(r.dir, stdinDir, puts[0].stream.Stdin)))) {
			got[path.Clean(name)] = data
		}
		if !maps.Equal(got, given) || len(headers) != 3 {
			t.Errorf("the rotation put %v (%d entries), want the three files given", slices.Sorted(maps.Keys(got)), len(headers))
		}
		for _, h := range headers {
			if h.Uid != 65532 || h.Gid != 65532 || h.Uname != "" || h.Gname != "" || h.Mode&0o077 != 0 {
				t.Errorf("the rotation put %s owned by %d:%d (%q:%q), mode %o", h.Name, h.Uid, h.Gid, h.Uname, h.Gname, h.Mode)
			}
		}
		for f, data := range given {
			if fileOf(t, filepath.Join(state, "config", f)) != data {
				t.Errorf("config/%s was not rotated in the entry", f)
			}
		}
		volumeLine := strings.Index(res.stdout, "rotated in the volume")
		localLine := strings.Index(res.stdout, "rotated TLS_CERT_FILE:")
		if volumeLine < 0 || volumeLine > localLine {
			t.Errorf("the local copy was replaced before the volume's:\n%s", res.stdout)
		}
		noSecretAnywhere(t, res, state, files...)

		r.rules(rancherRules(t, installedState(t, e, state, img))...)
		res = rancherRun(r, env, "--install")
		res.want(t, 0)
		if len(vmPuts(t, res, "default")) != 0 || strings.Contains(res.output(), "rotated") {
			t.Errorf("a second run rotated again:\n%s", res.output())
		}
	})
}

// TestRancherFetchFails: on containerd the secrets' volumes are read in
// place, where nerdctl says they are in Rancher Desktop's VM. A place that
// cannot be asked, an answer that is not exactly
// /var/lib/nerdctl/<hex>/volumes/default/<volume>/_data (another path,
// another volume or namespace, a digest that is not hex, a "..", two
// lines), and an archive that fails, each fail the run (exit 1) at that
// call: nothing is read from or written to a place not validated, nothing
// is put, the node is not touched, no helper container is made, the entry
// is unchanged, and no secret is in an argument, an environment or the
// output.
func TestRancherFetchFails(t *testing.T) {
	t.Parallel()
	e := rancherEngines[1]
	inspect := call{Tool: "nerdctl", Args: []string{"--namespace", "default", "volume", "inspect", "--format", "{{.Mountpoint}}", "cactusfs-secrets"}}
	said := func(answer string) string {
		return "internal: nerdctl says the volume cactusfs-secrets is at '" + answer + "', which is not /var/lib/nerdctl/<digest>/volumes/default/cactusfs-secrets/_data"
	}
	cases := []struct {
		name string
		fail rule
		want string
		last call
	}{
		{"where it is cannot be asked", rule{Tool: "nerdctl", Contains: inspect.Args[2:], Stderr: "it broke\n", Exit: 1}, "could not ask nerdctl where the volume cactusfs-secrets is", inspect},
		{"another path", mountRule("cactusfs-secrets", "/etc\n"), said("/etc"), inspect},
		{"nothing", mountRule("cactusfs-secrets", ""), said(""), inspect},
		{"another volume", mountRule("cactusfs-secrets", mountpoint("cactusfs-config")+"\n"), said(mountpoint("cactusfs-config")), inspect},
		{"another namespace", mountRule("cactusfs-secrets", nerdctlRoot+"/volumes/k8s.io/cactusfs-secrets/_data\n"), said(nerdctlRoot + "/volumes/k8s.io/cactusfs-secrets/_data"), inspect},
		{"a digest that is not hex", mountRule("cactusfs-secrets", "/var/lib/nerdctl/1935XY59/volumes/default/cactusfs-secrets/_data\n"), said("/var/lib/nerdctl/1935XY59/volumes/default/cactusfs-secrets/_data"), inspect},
		{"a way out", mountRule("cactusfs-secrets", "/var/lib/nerdctl/1935db59/../../../../root/volumes/default/cactusfs-secrets/_data\n"), said("/var/lib/nerdctl/1935db59/../../../../root/volumes/default/cactusfs-secrets/_data"), inspect},
		{"two lines", mountRule("cactusfs-secrets", mountpoint("cactusfs-secrets")+"\n"+mountpoint("cactusfs-secrets")+"\n"), said(mountpoint("cactusfs-secrets") + " " + mountpoint("cactusfs-secrets")), inspect},
		{"the archive fails", rule{Tool: "rdctl", Contains: volGetArgs("cactusfs-secrets", "secrets"), Stderr: "it broke\n", Exit: 1}, "reading the volume cactusfs-secrets in Rancher Desktop's VM as a tar stream failed", call{Tool: "rdctl", Args: volGetArgs("cactusfs-secrets", "secrets")}},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		base, img := rancherInstalled(t, r, e, "KMS=local\n")
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				st := r.stateOf(e.targetID)
				was := filesUnder(t, st)
				r.rules(append([]rule{c.fail}, rancherRules(t, installedState(t, e, st, img))...)...)
				res := rancherRun(r, "KMS=local\n", "--install")
				res.want(t, 1, c.want)
				lastCalls(t, res, c.last)
				for _, call := range res.calls {
					s := sub(call)
					if call.Tool == "rdctl" && !slices.Equal(call.Args, volGetArgs("cactusfs-secrets", "secrets")) {
						t.Errorf("rdctl was run on a place not validated, or to change one: %q", call.Args)
					}
					// The image is made before the secrets are read; nothing
					// of the deployment is changed after.
					touched := len(s) > 0 && (s[0] == "stop" || s[0] == "rename" || s[0] == "create" || s[0] == "start" ||
						(s[0] == "cp" && mutating(call)) || (s[0] == "volume" && len(s) > 1 && s[1] != "ls" && s[1] != "inspect") ||
						(s[0] == "run" && slices.Contains(s, "server")))
					if call.Tool == "nerdctl" && (slices.Contains(call.Args, "cactusfs-volumes") || touched) {
						t.Errorf("a failed read changed the deployment: %q", s)
					}
				}
				if now := filesUnder(t, st); !mapsEqual(was, now) {
					t.Errorf("the entry changed:\nbefore %v\nafter  %v", keys(was), keys(now))
				}
				noSecretAnywhere(t, res, st)
			})
		}
	})
}

// TestRancherUpgrade: a standalone upgrade on each engine of Rancher
// Desktop. On moby it is Docker Desktop's - the old node stopped, renamed
// cactusfs-previous, removed once the new one is ready - with no restart
// policy changed. On containerd, whose nerdctl keeps a stopped container's
// ports until it is removed and whose restart monitor starts again a
// stopped node with the policy "always" (nerdctl v2.2.2): the old node's
// image is pinned by the digest its tag names (RepoDigests), which the
// container must have been made from, its policy is cleared, then it is
// stopped and removed - no rename, no cactusfs-previous - the new node run
// and waited for, and only then the old tag removed. Each step that fails
// before the removal leaves the old node running with its policy given back
// and nothing upgraded. A new node that cannot run is removed and the old
// image run again by its digest on the same volumes ("rolled back"; exit 1);
// a rollback that fails too says the node is DOWN and how to run the old
// image again. A new node that runs and is not ready is left running (it may
// have changed the metadata: Albie's decision of 2026-11-23), with the way
// back. A run stopped by a signal after the removal, also during the
// rollback's run, says what to run; an upgrade an earlier build left half
// way is refused with the way back; an uninstall clears the policy before
// the stop and the removal.
func TestRancherUpgrade(t *testing.T) {
	t.Parallel()
	const env = "KMS=local\n"
	broke := func(words ...string) rule {
		return rule{Tool: "nerdctl", Contains: words, Stderr: "it broke\n", Exit: 1}
	}
	// The new node's run: the first "run -d --name cactusfs", before the
	// rollback's "rm -f cactusfs".
	newRun := []string{"run", "-d", "--name", "cactusfs"}
	rollback := []string{"rm", "-f", "cactusfs"}
	firstRunFails := rule{Tool: "nerdctl", Contains: newRun, Until: rollback, Stderr: "port is already allocated\n", Exit: 1}
	notReady := rule{Tool: "curl", Contains: []string{"--connect-to"}, Stdout: "000"}
	back := "nerdctl --namespace default rm -f cactusfs, then deploy/deploy.sh --install with IMAGE_SOURCE=image IMAGE="
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range rancherEngines {
			t.Run(e.tool, func(t *testing.T) {
				t.Parallel()
				base, img := rancherInstalled(t, r, e, env)
				upgrade := func(t *testing.T, more string, extra ...rule) result {
					t.Helper()
					r := clone(t, base)
					st := r.stateOf(e.targetID)
					r.rules(append(extra, rancherRules(t, installedState(t, e, st, img))...)...)
					return rancherRun(r, env+more, "--install")
				}
				// unchanged fails the test when the old node was stopped,
				// removed or renamed, or a node run - but for the steps
				// allowed.
				unchanged := func(t *testing.T, res result, allowed ...string) {
					t.Helper()
					for _, st := range steps(res) {
						if slices.Contains(allowed, st) {
							continue
						}
						if strings.HasPrefix(st, "stop ") || strings.HasPrefix(st, "rm ") || strings.HasPrefix(st, "rename ") || strings.HasPrefix(st, "run ") {
							t.Errorf("the old node was changed or a node run: %q", st)
						}
					}
				}
				rolledBack := func(t *testing.T, res result) {
					t.Helper()
					got := steps(res)
					rm := index(got, "rm cactusfs")
					run := slices.IndexFunc(got, func(s string) bool { return strings.HasPrefix(s, "run ") && s != "run "+oldHex })
					again := index(got, "run "+oldHex)
					if rm < 0 || run < rm || again < run {
						t.Errorf("not: old node removed, new node tried, old image run again by its digest: %q", got)
					}
					if len(callsOf(res, "nerdctl", "rm", "-f", "cactusfs")) != 1 || again < 0 || slices.Index(got[again:], "readyz") < 0 {
						t.Errorf("the new node was not removed once, or the old one not waited for: %q", got)
					}
					if len(callsOf(res, "nerdctl", "image", "rm", img)) != 0 {
						t.Errorf("the old image was removed by a rollback")
					}
					noVolumeRemoved(t, res)
				}

				t.Run("upgrade", func(t *testing.T) {
					res := upgrade(t, "")
					res.want(t, 0, "ready: cactusfs runs")
					got := steps(res)
					if e.tool == "docker" {
						for _, s := range got {
							if strings.HasPrefix(s, "update") {
								t.Errorf("moby was given %q", s)
							}
						}
						before(t, got, "stop cactusfs", "rename cactusfs cactusfs-previous", "run "+runOf(t, res)[len(runOf(t, res))-2], "readyz", "rm cactusfs-previous")
						return
					}
					newImg := runOf(t, res)[len(runOf(t, res))-2]
					before(t, got, "update --restart no cactusfs", "stop cactusfs", "rm cactusfs", "run "+newImg, "readyz")
					res.want(t, 0, "upgrade: cactusfs runs "+newImg+" in place of "+img+", removed before it ran")
					at := func(words ...string) int {
						return slices.IndexFunc(res.calls, func(c call) bool { return slices.Equal(sub(c), words) })
					}
					digest := at("image", "inspect", "--format", "{{range .RepoDigests}}{{.}} {{end}}", img)
					made := at("container", "inspect", "--format", "{{.Image}}", "cactusfs")
					update := at("update", "--restart", "no", "cactusfs")
					if digest < 0 || made < 0 || digest > update || made > update {
						t.Errorf("the old node's image was not pinned before the upgrade began (%d, %d, %d)", digest, made, update)
					}
					for _, c := range res.calls {
						if slices.Contains(c.Args, "cactusfs-previous") || slices.Contains(sub(c), "rename") {
							t.Errorf("a rename or cactusfs-previous on nerdctl: %q", c.Args)
						}
					}
					if n := len(callsOf(res, "nerdctl", "update")); n != 1 {
						t.Errorf("%d updates, want the one before the stop: %q", n, got)
					}
					if rm := index(got, "readyz"); slices.ContainsFunc(got[:rm], func(s string) bool { return s == "image rm "+img }) {
						t.Error("the old tag was removed before the new node was ready")
					}
					if strings.Contains(res.output(), "the upgrade stopped after") {
						t.Error("a finished upgrade said it was stopped")
					}
					noVolumeRemoved(t, res)
				})
				if e.tool == "docker" {
					return
				}

				for _, c := range []struct {
					name string
					fail rule
					want string
				}{
					{"the old image is not there", broke("image", "inspect", "--format", "{{range .RepoDigests}}{{.}} {{end}}", img), "the image " + img + " the node runs is not on"},
					{"the old image names no digest", rule{Tool: "nerdctl", Contains: []string{"image", "inspect", "--format", "{{range .RepoDigests}}{{.}} {{end}}", img}, Stdout: " \n"}, "nerdctl names no digest of the image " + img},
					{"the tag names another image", rule{Tool: "nerdctl", Contains: []string{"container", "inspect", "--format", "{{.Image}}", "cactusfs"}, Stdout: "sha256:" + strings.Repeat("0", 64) + "\n"}, "and its tag " + img + " now names another (sha256:" + oldHex + ")"},
					{"the node was made from another name", rule{Tool: "nerdctl", Contains: []string{"container", "inspect", "--format", "{{.Image}}", "cactusfs"}, Stdout: "registry.example.test/other:1\n"}, "says it was made from 'registry.example.test/other:1'"},
					{"the update fails", broke("update", "--restart", "no", "cactusfs"), "the restart policy of cactusfs could not be cleared"},
				} {
					t.Run(c.name, func(t *testing.T) {
						res := upgrade(t, "", c.fail)
						res.want(t, 1, c.want, "nothing was stopped or upgraded")
						unchanged(t, res)
					})
				}

				t.Run("a node made from the tag itself", func(t *testing.T) {
					// A nerdctl that records no digest says the name.
					res := upgrade(t, "", rule{Tool: "nerdctl", Contains: []string{"container", "inspect", "--format", "{{.Image}}", "cactusfs"}, Stdout: img + "\n"})
					res.want(t, 0, "ready: cactusfs runs")
				})

				t.Run("the stop fails", func(t *testing.T) {
					res := upgrade(t, "", broke("stop", "-t", "40", "cactusfs"))
					res.want(t, 1, "cactusfs could not be stopped", "nothing was upgraded")
					before(t, steps(res), "update --restart no cactusfs", "stop cactusfs", "update --restart always cactusfs")
					unchanged(t, res, "stop cactusfs")
				})

				t.Run("the removal fails", func(t *testing.T) {
					res := upgrade(t, "", rule{Tool: "nerdctl", Contains: []string{"rm", "cactusfs"}, Starts: []string{"rm", "cactusfs"}, Stderr: "it broke\n", Exit: 1})
					res.want(t, 1, "cactusfs could not be removed", "it was started again and runs "+img+" as before - nothing was upgraded")
					before(t, steps(res), "update --restart no cactusfs", "stop cactusfs", "rm cactusfs", "update --restart always cactusfs")
					update := slices.IndexFunc(res.calls, func(c call) bool { return slices.Equal(sub(c), []string{"update", "--restart", "always", "cactusfs"}) })
					start := slices.IndexFunc(res.calls, func(c call) bool { return slices.Equal(sub(c), []string{"start", "cactusfs"}) })
					if update < 0 || start < update {
						t.Errorf("the policy was not given back before the node was started again (update at %d, start at %d)", update, start)
					}
					unchanged(t, res, "stop cactusfs", "rm cactusfs")
				})

				t.Run("the new node cannot run", func(t *testing.T) {
					res := upgrade(t, "", firstRunFails)
					res.want(t, 1, "could not be run", "rolled back to "+img+", which is ready: nothing was upgraded")
					rolledBack(t, res)
					if strings.Contains(res.output(), "the upgrade stopped after") {
						t.Error("a rollback that ran the old image said the upgrade was stopped")
					}
				})

				t.Run("the new node is not ready", func(t *testing.T) {
					res := upgrade(t, "WAIT_TIMEOUT=1\n", notReady)
					res.want(t, 1, "did not become ready within WAIT_TIMEOUT=1 s", "it is left running, as it may have changed the metadata already",
						"To go back to the image it replaced: "+back+img+" (sha256:"+oldHex+")")
					if len(callsOf(res, "nerdctl", "rm", "-f", "cactusfs")) != 0 || slices.Contains(steps(res), "run "+oldHex) {
						t.Errorf("a node that ran was rolled back: %q", steps(res))
					}
					if strings.Contains(res.output(), "the upgrade stopped after") {
						t.Error("the notice of a stopped upgrade was said too")
					}
					noVolumeRemoved(t, res)
				})

				t.Run("the rollback cannot run", func(t *testing.T) {
					res := upgrade(t, "", broke(newRun...))
					res.want(t, 1, "the node is DOWN", "could not be run again", "To run it again: "+back+img+" (sha256:"+oldHex+")")
					if !slices.Contains(steps(res), "run "+oldHex) {
						t.Errorf("the old image was not run again: %q", steps(res))
					}
					noVolumeRemoved(t, res)
				})

				t.Run("the rollback is not ready", func(t *testing.T) {
					nr := notReady
					res := upgrade(t, "WAIT_TIMEOUT=1\n", firstRunFails, nr)
					res.want(t, 1, "the node is DOWN", "run again, did not become ready within WAIT_TIMEOUT=1 s", "To run it again: "+back+img)
					noVolumeRemoved(t, res)
				})

				for _, c := range []struct {
					name  string
					rules []rule
				}{
					{"stopped by a signal in the new node's run", []rule{{Tool: "nerdctl", Contains: newRun, TermParent: true}}},
					{"stopped by a signal in the rollback's run", []rule{firstRunFails, {Tool: "nerdctl", Contains: newRun, After: rollback, TermParent: true}}},
				} {
					t.Run(c.name, func(t *testing.T) {
						res := upgrade(t, "", c.rules...)
						if res.code != 143 {
							t.Fatalf("exit status %d, want 143 (SIGTERM)\n%s", res.code, res.output())
						}
						res.want(t, 143, "the upgrade stopped after the node cactusfs (image "+img+") was removed and before a node ran again; the volumes are kept",
							back+img)
						noVolumeRemoved(t, res)
					})
				}

				t.Run("an upgrade an earlier build left", func(t *testing.T) {
					res := upgrade(t, "",
						rule{Tool: "nerdctl", Contains: []string{"ps", "-a", "name=cactusfs-previous"}, Stdout: "cactusfs-previous\n"},
						rule{Tool: "nerdctl", Contains: []string{"container", "inspect", "cactusfs-previous"}, Stdout: "cactusfs|rancher|exited|" + img + "\n"})
					res.want(t, 2, "Otherwise put it back: nerdctl --namespace default rm -f cactusfs; nerdctl --namespace default rename cactusfs-previous cactusfs; nerdctl --namespace default start cactusfs; nerdctl --namespace default update --restart always cactusfs")
					noChange(t, res)
				})

				t.Run("uninstall", func(t *testing.T) {
					r := clone(t, base)
					st := r.stateOf(e.targetID)
					r.rules(rancherRules(t, installedState(t, e, st, img))...)
					res := rancherRun(r, env, "--uninstall")
					res.want(t, 0, "removed the container cactusfs")
					before(t, steps(res), "update --restart no cactusfs", "stop cactusfs", "rm cactusfs")

					r.rules(append([]rule{broke("update", "--restart", "no", "cactusfs")}, rancherRules(t, installedState(t, e, st, img))...)...)
					res = rancherRun(r, env, "--uninstall")
					res.want(t, 1, "the restart policy of cactusfs could not be cleared", "nothing was stopped or removed")
					unchanged(t, res)
				})
			})
		}
	})
}

// noVolumeRemoved fails the test when the run removed a volume.
func noVolumeRemoved(t *testing.T, res result) {
	t.Helper()
	for _, c := range res.calls {
		if s := sub(c); isEngine(c.Tool) && len(s) > 1 && s[0] == "volume" && (s[1] == "rm" || s[1] == "prune") {
			t.Errorf("a volume was removed: %q", c.Args)
		}
	}
}

// TestRancherVMDirAnswers: an answer of mktemp in Rancher Desktop's VM that
// is not exactly the template /dev/shm/cactusfs-deploy.XXXXXX with its X's
// made letters and digits - another place, the old /tmp, an empty or short
// suffix, a way out, dots, no dot, a line break, a space - fails the run
// (exit 1), and nothing is written to it or removed from it: no rdctl call
// but mktemp's, and dev-init's container removed.
func TestRancherVMDirAnswers(t *testing.T) {
	t.Parallel()
	e := rancherEngines[1]
	for _, answer := range []string{
		"/etc",
		"/tmp/cactusfs-deploy.Fx7Kq2",
		"/dev/shm/cactusfs-deploy.",
		"/dev/shm/cactusfs-deploy.Fx7Kq",
		"/dev/shm/cactusfs-deploy.a/../..",
		"/dev/shm/cactusfs-deploy...xAbc",
		"/dev/shm/cactusfs-deployFx7Kq2",
		"/dev/shm/cactusfs-deploy.Fx7\nKq2",
		"/dev/shm/cactusfs-deploy.Fx7Kq2\n/dev/shm/cactusfs-deploy.Fx7Kq2",
		"/dev/shm/cactusfs-deploy.Fx Kq2",
		"/dev/shm/cactusfs-deploy.Fx7Kq2/",
	} {
		t.Run(strings.NewReplacer("/", "_", "\n", "NL", " ", "SP").Replace(answer), func(t *testing.T) {
			t.Parallel()
			eachShell(t, func(t *testing.T, r *rig) {
				r = secretsRig(t, r)
				r.rules(append([]rule{{Tool: "rdctl", Contains: vmArgs["mktemp"], Stdout: answer + "\n"}}, rancherRules(t, engineState{e: e, devInit: devInitOutput(), ready: true})...)...)
				res := rancherRun(r, "KMS=local\n", "--install")
				res.want(t, 1, "which is not a directory of the template /dev/shm/cactusfs-deploy.XXXXXX: nothing is written or removed there")
				for _, c := range res.calls {
					if c.Tool == "rdctl" && !slices.Equal(c.Args, vmArgs["mktemp"]) {
						t.Errorf("rdctl was run on an answer not checked: %q", c.Args)
					}
				}
				lastCalls(t, res, call{Tool: "nerdctl", Args: []string{"--namespace", "default", "rm", "-f", "cactusfs-dev-init"}})
			})
		})
	}
}

// TestRancherVolumeRoot: on containerd the root of a secrets' volume is the
// nodes' user's, 0700. A volume that is there and empty - made by a run
// stopped before its put - whose root is nerdctl's root:root is filled, its
// root given to 65532 and made 0700 before the stream goes in; one that
// holds files under such a root is refused (exit 2) with the commands that
// fix it, before anything is put.
func TestRancherVolumeRoot(t *testing.T) {
	t.Parallel()
	e := rancherEngines[1]
	const env = "KMS=local\n"
	eachShell(t, func(t *testing.T, r *rig) {
		base, img := rancherInstalled(t, r, e, env)

		t.Run("empty, owned by root", func(t *testing.T) {
			r := clone(t, base)
			st := r.stateOf(e.targetID)
			s := installedState(t, e, st, img)
			s.keys = map[string]map[string]string{"secrets": {}, "config": {}}
			r.rules(append([]rule{
				volArchive("secrets", tarOwned(t, "secrets", nil, 0)),
				volArchive("config", tarOwned(t, "config", nil, 0)),
			}, rancherRules(t, s)...)...)
			res := rancherRun(r, env, "--install")
			res.want(t, 0, "ready: cactusfs runs")
			for _, unit := range []string{"secrets", "config"} {
				v := "cactusfs-" + unit
				chown := slices.IndexFunc(res.calls, func(c call) bool {
					return c.Tool == "rdctl" && slices.Equal(c.Args, []string{"shell", "sudo", "chown", "65532:65532", mountpoint(v)})
				})
				chmod := slices.IndexFunc(res.calls, func(c call) bool {
					return c.Tool == "rdctl" && slices.Equal(c.Args, []string{"shell", "sudo", "chmod", "0700", mountpoint(v)})
				})
				put := slices.IndexFunc(res.calls, func(c call) bool { return c.Tool == "rdctl" && volumeOf(c, "-x") == v })
				if chown < 0 || chmod < chown || put < chmod {
					t.Errorf("%s: chown at %d, chmod at %d, put at %d: want the root made 65532 0700 before the put", v, chown, chmod, put)
				}
			}
			puts, _ := vmCopies(t, res, "default")
			if len(puts) != 2 {
				t.Errorf("%d puts, want both volumes filled", len(puts))
			}
		})

		t.Run("files under a root owned by root", func(t *testing.T) {
			r := clone(t, base)
			st := r.stateOf(e.targetID)
			s := installedState(t, e, st, img)
			r.rules(append([]rule{volArchive("secrets", tarOwned(t, "secrets", s.keys["secrets"], 0))}, rancherRules(t, s)...)...)
			res := rancherRun(r, env, "--install")
			res.want(t, 2, "the root of the volume cactusfs-secrets is not the nodes' user's with the mode 0700 (owner 0:0, mode 700)",
				"rdctl shell sudo chown 65532:65532 "+mountpoint("cactusfs-secrets")+"; rdctl shell sudo chmod 0700 "+mountpoint("cactusfs-secrets"))
			for _, c := range res.calls {
				if c.Tool == "rdctl" && (volumeOf(c, "-x") != "" || volumeRoot(c)) {
					t.Errorf("a refused run changed a volume: %q", c.Args)
				}
			}
			noSecretAnywhere(t, res, st)
		})
	})
}
