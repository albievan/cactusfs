//go:build deployscript

package deploy_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
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
		rules = append([]rule{{Tool: "docker", Contains: []string{"--context", "rancher-desktop", "info"}, Stderr: "Cannot connect to the Docker daemon\n", Exit: 1}}, rules...)
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
				// Every copy into a container: on containerd "cp - <container>:<dir>"
				// of a stream owned by 65532 with no names; on moby "cp -a -",
				// whose owner the engine sets, of the stream as tar made it.
				puts := 0
				for _, c := range res.calls {
					s := sub(c)
					if !isEngine(c.Tool) || len(s) < 3 || s[0] != "cp" || !mutating(c) {
						continue
					}
					puts++
					if e.tool == "nerdctl" && slices.Contains(s, "-a") {
						t.Errorf("nerdctl's cp was given -a, which it has not: %q", c.Args)
					}
					// The secrets unit and dev-init's passphrase are the nodes'
					// user's alone: nothing for group or others, whoever owns
					// them.
					to := s[len(s)-1]
					secretUnit := to == "cactusfs-volumes:/var/lib/cactusfs/secrets"
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
					before(t, steps(res), "stop cactusfs", "rename cactusfs cactusfs-previous", "run "+runOf(t, res)[len(runOf(t, res))-2], "readyz", "rm cactusfs-previous")
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

// TestRancherKubernetes: RANCHER_KUBERNETES=yes hands the install to the
// Kubernetes path (PLAN 10.10's stub) without asking the engine, and the
// image for its k3s is made on the engine here - on containerd by nerdctl
// in the namespace k8s.io, which its k3s sees, on the route "local"; on
// moby by the Docker CLI of the context rancher-desktop.
func TestRancherKubernetes(t *testing.T) {
	t.Parallel()
	const k8s = "DEPLOY_TARGET=rancher\nRANCHER_KUBERNETES=yes\nK8S_S3_ALLOW_FROM=192.0.2.0/24\n"
	eachShell(t, func(t *testing.T, r *rig) {
		t.Run("install hands over", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			res := r.withEnv("KMS=local\n"+k8s, "--install")
			res.want(t, 3, "engine: kubectl --context rancher-desktop", "not built yet (PLAN 10.10)")
			if len(res.calls) != 0 {
				t.Errorf("the hand-over ran %v", res.calls)
			}
		})
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
