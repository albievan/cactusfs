//go:build deployscript

package deploy_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// A standalone deployment on a Docker engine (PLAN 10.5,
// deploy/lib/docker.sh): Docker Desktop, Docker CE on this machine and
// rootless Podman - install, upgrade, status, uninstall and purge, as the
// commands the fakes record.

const (
	// nodeImage and newImage are the images an install and an upgrade
	// deploy (IMAGE_SOURCE=image), named by their digest: image_check finds
	// each on the engine (givenHere) and the node runs it as given. PLAN
	// 10.4 builds the others (madeHere).
	nodeImage = "registry.example.test/cactusfs@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	newImage  = "registry.example.test/cactusfs@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	imageEnv  = "KMS=local\nIMAGE_SOURCE=image\nIMAGE=" + nodeImage + "\n"
	// imgDigest is a digest a push names an image by.
	imgDigest = "3333333333333333333333333333333333333333333333333333333333333333"
	// The format docker.sh inspects the data volumes with, and the one it
	// reads their target with before a purge.
	labelsFormat = `{{.Name}} {{index .Labels "io.cactusfs.deploy.name"}} {{index .Labels "io.cactusfs.deploy.target"}}`
	targetFormat = `{{.Name}} {{index .Labels "io.cactusfs.deploy.target"}}`
	byLabel      = "label=io.cactusfs.deploy.name=cactusfs"
	readyzURL    = "https://localhost:9001/readyz"
)

// volumeLabel is the template of a volume's label in "volume inspect", by
// engine: nerdctl's Labels is a pointer to a map, nil without labels, read
// through "with" (deploy/lib/rancher.sh, PLAN 10.6).
func volumeLabel(tool, key string) string {
	if tool == "nerdctl" {
		return `{{with .Labels}}{{index . "` + key + `"}}{{end}}`
	}
	return `{{index .Labels "` + key + `"}}`
}

// labelsFormatOf and targetFormatOf are labelsFormat and targetFormat of an
// engine.
func labelsFormatOf(tool string) string {
	return "{{.Name}} " + volumeLabel(tool, "io.cactusfs.deploy.name") + " " + volumeLabel(tool, "io.cactusfs.deploy.target")
}

func targetFormatOf(tool string) string {
	return "{{.Name}} " + volumeLabel(tool, "io.cactusfs.deploy.target")
}

// vaultEnv is the lines of an env file for Vault; withVault makes an env
// file of imageEnv and them: KMS=vault in place of KMS=local, and the token
// a file of the rig.
const vaultEnv = "KMS=vault\nVAULT_ADDR=https://vault.example.test:8200\nVAULT_KEY=cactusfs\nVAULT_TOKEN_FILE=@TOKEN@\n"

func withVault(r *rig, env string) string {
	r.t.Helper()
	if !strings.Contains(env, "KMS=vault") {
		return env
	}
	env = strings.Replace(env, "KMS=local\n", "", 1)
	return strings.Replace(env, "@TOKEN@", r.envFile("token", "fixture-token-"+strings.ReplaceAll(r.t.Name(), "/", "-")), 1)
}

// dockerEngine is a target the tests run on: its engine's program, the
// target's name, the ID the engine says, the entry's target ID and what
// the run's environment adds.
type dockerEngine struct {
	tool, target, id, targetID string
	env                        []string
}

var dockerEngines = []dockerEngine{
	{tool: "docker", target: "dd", id: engineID, targetID: "dd-default"},
	{tool: "docker", target: "ce", id: engineID, targetID: "ce-default"},
	{tool: "podman", target: "podman", id: "host:/var/lib/containers", targetID: "podman-default", env: []string{"XDG_RUNTIME_DIR=/run/user/1000"}},
}

// engineState is what a fake engine holds and answers.
type engineState struct {
	e dockerEngine
	// ctr is the node's container as inspect answers ("owner|target|state|
	// image"); empty: there is none.
	ctr string
	// ports is what the running node publishes as ps shows it - Docker groups
	// consecutive ports into a range (docker/cli formatter.DisplayablePorts),
	// so the script reads its own from "port"; empty: not running.
	ports string
	// others are other running containers, "name|ports" a line.
	others string
	// data are the data and metadata volumes, "name owner target" each.
	data []string
	// keys are what the secrets' volumes hold (nil: they are not there).
	keys map[string]map[string]string
	// devInit is what dev-init writes.
	devInit map[string]string
	// inUse are the URLs at which the probe of a port is answered.
	inUse []string
	// ready: /readyz answers 200.
	ready bool
	// status is the line of the node in the listing by label (--status).
	status string
	// labelled are the volumes the listing by label names, with their
	// target ("name target"); purged lists them as they are once the data
	// volumes, then the secrets' volumes, are deleted.
	labelled []string
	purged   bool
}

// installedState is the engine of an installed deployment whose entry is
// state: the node runs image, with its drives, its metadata volume and the
// secrets' volumes of the entry.
func installedState(t *testing.T, e dockerEngine, state, image string) engineState {
	t.Helper()
	s := engineState{
		e: e, ctr: "cactusfs|" + e.target + "|running|" + image,
		ports: "127.0.0.1:9000-9001->9000-9001/tcp",
		keys:  withTarget(t, state), devInit: devInitOutput(), ready: true,
		status: "cactusfs|running|" + image + "|" + e.target + "|0.7.0-1",
	}
	for _, v := range []string{"data1", "data2", "data3", "data4", "meta"} {
		s.data = append(s.data, "cactusfs-"+v+" cactusfs "+e.target)
	}
	for _, v := range []string{"config", "data1", "data2", "data3", "data4", "meta", "secrets"} {
		s.labelled = append(s.labelled, "cactusfs-"+v+" "+e.target)
	}
	return s
}

func (s engineState) rules(t *testing.T) []rule {
	t.Helper()
	tool := s.e.tool
	// The images given (nodeImage, newImage) are on the engine; one built
	// is answered by engineRules (madeHere).
	rules := append(givenHere(tool, newImgFix(imgCommit), "", nodeImage), givenHere(tool, newImgFix(imgCommit), "", newImage)...)
	if s.ctr != "" {
		rules = append(rules,
			rule{Tool: tool, Contains: []string{"ps", "-a", "name=cactusfs"}, Stdout: "cactusfs\n"},
			rule{Tool: tool, Contains: []string{"container", "inspect", "cactusfs"}, Stdout: s.ctr + "\n"})
	}
	running := s.others
	if s.ports != "" {
		running = "cactusfs|" + s.ports + "\n" + running
		// What "port cactusfs" says of a node published on the defaults:
		// one line a port.
		rules = append(rules, rule{Tool: tool, Contains: []string{"port", "cactusfs"}, Stdout: "9000/tcp -> 127.0.0.1:9000\n9001/tcp -> 127.0.0.1:9001\n"})
	}
	rules = append(rules, rule{Tool: tool, Contains: []string{"ps", "{{.Names}}|{{.Ports}}"}, Stdout: running})
	if s.status != "" {
		rules = append(rules, rule{Tool: tool, Contains: []string{"ps", "-a", byLabel}, Stdout: s.status + "\n"})
	}
	// The listing by name: the secrets' volumes and the data volumes.
	listed := ""
	for _, unit := range []string{"secrets", "config"} {
		if _, ok := s.keys[unit]; ok {
			listed += "cactusfs-" + unit + "\n"
		}
	}
	labels := ""
	for _, d := range s.data {
		listed += strings.Fields(d)[0] + "\n"
		labels += d + "\n"
	}
	rules = append(rules,
		rule{Tool: tool, Contains: []string{"volume", "ls", "name=cactusfs-"}, Stdout: listed},
		rule{Tool: tool, Contains: []string{"volume", "inspect", labelsFormatOf(tool)}, Stdout: labels})
	// The listing by label, and the target of each.
	names, targets := "", ""
	for _, l := range s.labelled {
		names += strings.Fields(l)[0] + "\n"
		targets += l + "\n"
	}
	if s.purged {
		rules = append(rules,
			rule{Tool: tool, Contains: []string{"volume", "ls", byLabel}, After: []string{"volume", "rm", "cactusfs-secrets"}},
			rule{Tool: tool, Contains: []string{"volume", "ls", byLabel}, After: []string{"volume", "rm", "cactusfs-data1"}, Stdout: "cactusfs-config\ncactusfs-secrets\n"})
	}
	rules = append(rules,
		rule{Tool: tool, Contains: []string{"volume", "ls", byLabel}, Stdout: names},
		rule{Tool: tool, Contains: []string{"volume", "inspect", targetFormatOf(tool)}, Stdout: targets})
	for _, u := range s.inUse {
		rules = append(rules, rule{Tool: "curl", Contains: []string{u}, Stdout: "1"})
	}
	ready := "000"
	if s.ready {
		ready = "200"
	}
	rules = append(rules, rule{Tool: "curl", Contains: []string{"--connect-to"}, Stdout: ready})
	return append(rules, engineRules(t, tool, s.e.id, s.devInit, s.keys)...)
}

// sub is the words of an engine's call after the Docker CLI's context, or
// nerdctl's namespace (PLAN 10.6).
func sub(c call) []string {
	if c.Tool == "docker" && len(c.Args) >= 2 && c.Args[0] == "--context" {
		return c.Args[2:]
	}
	if c.Tool == "nerdctl" && len(c.Args) >= 2 && c.Args[0] == "--namespace" {
		return c.Args[2:]
	}
	return c.Args
}

// isEngine is true for a program of a container engine: docker, podman,
// nerdctl.
func isEngine(tool string) bool {
	return tool == "docker" || tool == "podman" || tool == "nerdctl"
}

// mutating is true for a call that changes the engine: a container, a
// volume, or an image - built, loaded, pulled, tagged or removed. A copy
// into a container ("cp -a -", or nerdctl's "cp <directory of Rancher
// Desktop's VM>/. <container>:<dir>") is a change; a copy out of one - to
// standard output, or on nerdctl into a directory of the VM - is not.
// Every call of rdctl changes Rancher Desktop's VM (a private directory
// made, filled or removed: nerdctl_put, nerdctl_get) but its archive of
// such a directory to standard output (nerdctl_get).
func mutating(c call) bool {
	if c.Tool == "rdctl" {
		return !containsRun(c.Args, []string{"tar", "-c"})
	}
	if !isEngine(c.Tool) {
		return false
	}
	s := sub(c)
	if len(s) == 0 {
		return false
	}
	switch s[0] {
	case "create", "run", "start", "stop", "rm", "rename", "update", "pull", "tag", "kill", "restart", "build", "load":
		return true
	case "buildx":
		return len(s) > 1 && s[1] == "build"
	case "image":
		return len(s) > 1 && (s[1] == "tag" || s[1] == "rm" || s[1] == "load" || s[1] == "pull")
	case "cp":
		return len(s) > 2 && strings.Contains(s[len(s)-1], ":")
	case "volume":
		return len(s) > 1 && (s[1] == "create" || s[1] == "rm" || s[1] == "prune")
	}
	return false
}

// noChange fails the test when the run changed the engine, or a server it
// reached over ssh.
func noChange(t *testing.T, res result) {
	t.Helper()
	for _, c := range res.calls {
		if mutating(c) {
			t.Errorf("the run changed the engine: %s %q", c.Tool, c.Args)
		}
		// What a run changes on a server over ssh (CE_HOST): a directory or
		// file made, filled, given a mode or a label, or removed.
		w := strings.TrimPrefix(remote(c), "sudo -n ")
		for _, change := range []string{"mkdir ", "tar -x ", "chmod ", "chcon ", "rm "} {
			if strings.HasPrefix(w, change) {
				t.Errorf("the run changed the server: %q", w)
			}
		}
	}
}

// steps is what the run did to the engine, in order, by a short name each:
// the probes of a port and of /readyz, the image's build, tag, load and
// version line, the volumes made, the puts, dev-init, the node's container
// stopped, removed and run.
func steps(res result) []string {
	var out []string
	for _, c := range res.calls {
		s := sub(c)
		switch {
		case c.Tool == "curl" && slices.Contains(c.Args, "%{num_connects}"):
			out = append(out, "probe "+c.Args[len(c.Args)-1])
		case c.Tool == "curl":
			out = append(out, "readyz")
		case !isEngine(c.Tool) && c.Tool != "rdctl":
		case len(s) == 0:
		case s[0] == "build" || (s[0] == "buildx" && len(s) > 1 && s[1] == "build"):
			out = append(out, "build")
		case s[0] == "image" && len(s) > 3 && s[1] == "tag":
			out = append(out, "tag "+s[3])
		case s[0] == "load":
			out = append(out, "load")
		case s[0] == "run" && isVersionCheck(s):
			out = append(out, "version")
		case s[0] == "volume" && s[1] == "create":
			out = append(out, "volume "+s[len(s)-1])
		case s[0] == "volume" && s[1] == "rm":
			out = append(out, "volume rm "+strings.Join(s[2:], " "))
		case s[0] == "volume" && s[1] == "ls" && slices.Contains(s, byLabel):
			out = append(out, "listing")
		case s[0] == "cp" && mutating(c) && strings.HasPrefix(s[len(s)-1], "cactusfs-volumes:"):
			out = append(out, "put "+s[len(s)-1])
		case c.Tool == "rdctl" && volumeOf(c, "-x") != "":
			// On nerdctl a volume is filled in place (nerdctl_volume_put):
			// the step is named by the unit's place on Docker.
			out = append(out, "put cactusfs-volumes:/var/lib/cactusfs/"+strings.TrimPrefix(volumeOf(c, "-x"), "cactusfs-"))
		case s[0] == "start":
			out = append(out, "dev-init")
		case s[0] == "run":
			out = append(out, "run "+s[len(s)-2])
		case s[0] == "rename":
			out = append(out, "rename "+s[1]+" "+s[2])
		case s[0] == "update":
			out = append(out, "update "+strings.Join(s[1:], " "))
		case s[0] == "stop" || s[0] == "rm":
			out = append(out, s[0]+" "+s[len(s)-1])
		}
	}
	return out
}

// index is where a step is among the steps, or -1.
func index(steps []string, step string) int {
	return slices.Index(steps, step)
}

// before fails the test unless each step is there, in that order.
func before(t *testing.T, got []string, want ...string) {
	t.Helper()
	at := -1
	for _, w := range want {
		i := index(got, w)
		if i < 0 || i < at {
			t.Fatalf("the steps %q do not hold %q in this order", got, want)
		}
		at = i
	}
}

// runOf is the node's container's run, the one there must be (the run of
// the image's version line is not a node).
func runOf(t *testing.T, res result) []string {
	t.Helper()
	var runs [][]string
	for _, c := range res.calls {
		if s := sub(c); isEngine(c.Tool) && len(s) > 0 && s[0] == "run" && !isVersionCheck(s) {
			runs = append(runs, s)
		}
	}
	if len(runs) != 1 {
		t.Fatalf("the node was run %d times: %q", len(runs), runs)
	}
	return runs[0]
}

// holds fails the test unless the run's words hold each group of words, one
// after another.
func holds(t *testing.T, args []string, groups ...[]string) {
	t.Helper()
	for _, g := range groups {
		if !containsRun(args, g) {
			t.Errorf("the node is run without %q:\n%q", g, args)
		}
	}
}

// dockerRun runs the script with the env file and the arguments, on the
// engine of e, under the guard of guardedRun.
func dockerRun(r *rig, e dockerEngine, env string, args ...string) result {
	r.t.Helper()
	argv := append([]string{"--env", r.envFile("site.env", env), "--docker", e.target}, args...)
	return r.guardedRun(argv, e.env...)
}

// dockerInstalled is a rig of the shell of r on which a first install of
// the env file has run on e's engine: the base the cases of a test clone.
func dockerInstalled(t *testing.T, r *rig, e dockerEngine, env string) *rig {
	t.Helper()
	base := secretsRig(t, r.fresh(t))
	base.rules(engineState{e: e, devInit: devInitOutput(), ready: true}.rules(t)...)
	dockerRun(base, e, env, "--install").want(t, 0, "ready: cactusfs runs")
	return base
}

// TestDockerInstall: a first install on each engine - the ports asked
// before anything changes, the secrets put before the node is run (dev-init,
// the two volumes and their puts, PLAN 10.3's order, with the node added),
// the drives and the metadata volume made with the deployment's labels, and
// the node run as 65532 from the image, read-only, every capability dropped,
// no new privileges, restarted unless stopped, S3 on BIND and the admin port
// on ADMIN_BIND, the volumes mounted by name and the secrets read-only, given
// every secret as a file and the key file of the config volume - then ready.
// No secret is on any command line, in any environment or in the output, a
// traced run's included.
func TestDockerInstall(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range dockerEngines {
			t.Run(e.target, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				r.rules(engineState{e: e, devInit: devInitOutput(), ready: true}.rules(t)...)
				r.trace = true
				res := dockerRun(r, e, imageEnv+"CACTUSFS_LOG_LEVEL=debug\n", "--install")
				r.trace = false
				res.want(t, 0, "ready: cactusfs runs "+nodeImage, "S3: https://127.0.0.1:9000",
					"admin API and console: https://127.0.0.1:9001", "the CA to trust: "+r.stateOf(e.targetID)+"/config/s3-ca.pem")
				noSecretAnywhere(t, res, r.stateOf(e.targetID))
				if e.tool == "podman" && len(callsOf(res, "docker")) != 0 {
					t.Error("docker was run for podman")
				}
				// ADR-0014: every install says admin/admin must go before the
				// admin port is opened; on the loopback address it warns of
				// nothing more. Podman says how its containers come back
				// after a reboot.
				res.want(t, 0, "the console user admin with the password admin (ADR-0014): change its password or delete the user")
				if strings.Contains(res.stderr, "publishes the admin API and console beyond this machine") {
					t.Errorf("a warning of a published admin port on the loopback address:\n%s", res.stderr)
				}
				if got := strings.Contains(res.stdout, "podman-restart.service"); got != (e.tool == "podman") {
					t.Errorf("the podman-restart advice said: %v, on %s", got, e.target)
				}

				got := steps(res)
				before(t, got, "probe http://127.0.0.1:9000/", "probe http://127.0.0.1:9001/", "dev-init",
					"volume cactusfs-secrets", "volume cactusfs-config",
					"put cactusfs-volumes:/var/lib/cactusfs/secrets", "put cactusfs-volumes:/var/lib/cactusfs/config",
					"volume cactusfs-data1", "volume cactusfs-data2", "volume cactusfs-data3", "volume cactusfs-data4",
					"volume cactusfs-meta", "run "+nodeImage, "readyz")
				for _, c := range res.calls {
					if c.Tool == "curl" && slices.Contains(c.Args, "http://127.0.0.1:9001/") {
						break
					}
					if mutating(c) {
						t.Errorf("a change before the ports were asked: %q", c.Args)
					}
				}
				for _, c := range res.calls {
					if s := sub(c); mutating(c) && s[0] == "volume" && s[1] == "create" &&
						!containsAll(s, []string{"io.cactusfs.deploy.name=cactusfs", "io.cactusfs.deploy.target=" + e.target}) {
						t.Errorf("a volume without the deployment's labels: %q", c.Args)
					}
				}
				// On Podman the secrets' volumes are owned by the nodes' user
				// from their creation: the helper's copy does not set their
				// root's owner there. Docker's are made plainly.
				for _, unit := range []string{"cactusfs-secrets", "cactusfs-config"} {
					made := callsOf(res, e.tool, "volume", "create", unit)
					if len(made) != 1 {
						t.Fatalf("the volume %s was made %d times", unit, len(made))
					}
					owned := containsRun(sub(made[0]), []string{"--opt", "o=uid=65532,gid=65532"})
					if owned != (e.tool == "podman") || (e.tool != "podman" && slices.Contains(made[0].Args, "--opt")) {
						t.Errorf("the volume %s is made with %q on %s", unit, made[0].Args, e.target)
					}
				}

				run := runOf(t, res)
				holds(t, run,
					[]string{"run", "-d", "--name", "cactusfs"},
					[]string{"--label", "io.cactusfs.deploy.name=cactusfs"},
					[]string{"--label", "io.cactusfs.deploy.target=" + e.target},
					[]string{"--user", "65532:65532"},
					[]string{"--read-only"},
					[]string{"--cap-drop", "ALL"},
					[]string{"--security-opt", "no-new-privileges"},
					[]string{"--pull", "never"},
					[]string{"--publish", "127.0.0.1:9000:9000"},
					[]string{"--publish", "127.0.0.1:9001:9001"},
					[]string{"--mount", "type=volume,source=cactusfs-secrets,target=/var/lib/cactusfs/secrets,readonly"},
					[]string{"--mount", "type=volume,source=cactusfs-config,target=/var/lib/cactusfs/config,readonly"},
					[]string{"--mount", "type=volume,source=cactusfs-meta,target=/var/lib/cactusfs/meta"},
					[]string{"--mount", "type=volume,source=cactusfs-data1,target=/var/lib/cactusfs/data1"},
					[]string{"--mount", "type=volume,source=cactusfs-data4,target=/var/lib/cactusfs/data4"},
					[]string{"--env", "CACTUSFS_KMS=local"},
					[]string{"--env", "CACTUSFS_KMS_KEY_FILE=/var/lib/cactusfs/config/kms.json"},
					[]string{"--env", "CACTUSFS_KMS_PASSPHRASE_FILE=/var/lib/cactusfs/secrets/kms-passphrase"},
					[]string{"--env", "CACTUSFS_ROOT_ACCESS_KEY_FILE=/var/lib/cactusfs/secrets/root-access-key"},
					[]string{"--env", "CACTUSFS_ROOT_SECRET_KEY_FILE=/var/lib/cactusfs/secrets/root-secret-key"},
					[]string{"--env", "CACTUSFS_TLS_CERT=/var/lib/cactusfs/config/s3.pem"},
					[]string{"--env", "CACTUSFS_TLS_KEY=/var/lib/cactusfs/config/s3.key"},
					[]string{"--env", "CACTUSFS_META_DIR=/var/lib/cactusfs/meta"},
					[]string{"--env", "CACTUSFS_DATA=/var/lib/cactusfs/data1,/var/lib/cactusfs/data2,/var/lib/cactusfs/data3,/var/lib/cactusfs/data4"},
					[]string{"--env", "CACTUSFS_DRIVE_LABELS=cactusfs-data1,cactusfs-data2,cactusfs-data3,cactusfs-data4"},
					[]string{"--env", "CACTUSFS_UPGRADE_MODE=off"},
					[]string{"--env", "CACTUSFS_S3_ADDR=:9000"},
					[]string{"--env", "CACTUSFS_ADMIN_ADDR=:9001"},
					[]string{"--env", "CACTUSFS_LOG_LEVEL=debug"},
				)
				// The restart policy: on Podman "always", the one policy its
				// podman-restart.service starts after a reboot (start --all
				// --filter restart-policy=always); on Docker "unless-stopped".
				policy := "unless-stopped"
				if e.tool == "podman" {
					policy = "always"
				}
				holds(t, run, []string{"--restart", policy})
				if !slices.Equal(run[len(run)-2:], []string{nodeImage, "server"}) {
					t.Errorf("the node does not run %s server: %q", nodeImage, run[len(run)-2:])
				}
				for i, a := range run {
					switch {
					case a == "-v" || a == "--volume" || strings.HasPrefix(a, "--volume=") || a == "-e" || a == "--env-file":
						t.Errorf("the node is run with %q", a)
					case a == "--mount" && !strings.HasPrefix(run[i+1], "type=volume,source=cactusfs-"):
						t.Errorf("a mount that is not one of the deployment's volumes: %q", run[i+1])
					case a == "--env" && !strings.HasPrefix(run[i+1], "CACTUSFS_"):
						t.Errorf("the node is given %q", run[i+1])
					}
				}
				// curl reads no .curlrc: -q first, on every call.
				for _, c := range callsOf(res, "curl") {
					if len(c.Args) == 0 || c.Args[0] != "-q" {
						t.Errorf("curl without -q first: %q", c.Args)
					}
				}
				// /readyz over TLS with the deployment's CA, never through a
				// proxy, never without verifying.
				for _, c := range callsOf(res, "curl", "--connect-to") {
					if !containsRun(c.Args, []string{"--cacert", r.stateOf(e.targetID) + "/config/s3-ca.pem"}) ||
						!containsRun(c.Args, []string{"--noproxy", "*"}) || slices.Contains(c.Args, "-k") || slices.Contains(c.Args, "--insecure") {
						t.Errorf("/readyz asked with %q", c.Args)
					}
				}
			})
		}
	})
}

// TestDockerSettings: the settings that shape the node - the number of
// drives, the addresses and ports, Vault - on Docker Desktop.
func TestDockerSettings(t *testing.T) {
	t.Parallel()
	e := dockerEngines[0]
	cases := []struct {
		name, env string
		ready     string
		want      [][]string
		not       []string
		// warned is what the run warns of on standard error.
		warned string
	}{
		{
			name: "two drives", env: "DRIVES=2\n", ready: readyzURL,
			want: [][]string{
				{"--env", "CACTUSFS_DATA=/var/lib/cactusfs/data1,/var/lib/cactusfs/data2"},
				{"--env", "CACTUSFS_DRIVE_LABELS=cactusfs-data1,cactusfs-data2"},
				{"--mount", "type=volume,source=cactusfs-data2,target=/var/lib/cactusfs/data2"},
			},
			not: []string{"type=volume,source=cactusfs-data3,target=/var/lib/cactusfs/data3"},
		},
		{
			name: "addresses and ports", env: "BIND=0.0.0.0\nADMIN_BIND=::1\nS3_PORT=19000\nADMIN_PORT=19001\n",
			ready: "https://localhost:19001/readyz",
			want: [][]string{
				{"--publish", "0.0.0.0:19000:19000"},
				{"--env", "CACTUSFS_S3_ADDR=:19000"},
				{"--publish", "[::1]:19001:19001"},
				{"--env", "CACTUSFS_ADMIN_ADDR=:19001"},
			},
		},
		{
			// The admin port published beyond this machine: admin/admin
			// (ADR-0014) is warned of.
			name: "admin published", env: "ADMIN_BIND=192.0.2.10\n",
			ready:  readyzURL,
			want:   [][]string{{"--publish", "192.0.2.10:9001:9001"}},
			warned: "ADMIN_BIND=192.0.2.10 publishes the admin API and console beyond this machine: change the password of the console user admin, or delete it, now (ADR-0014)",
		},
		{
			name:  "vault",
			env:   vaultEnv + "VAULT_NAMESPACE=team\n",
			ready: readyzURL,
			want: [][]string{
				{"--env", "CACTUSFS_KMS=vault"},
				{"--env", "CACTUSFS_VAULT_ADDR=https://vault.example.test:8200"},
				{"--env", "CACTUSFS_VAULT_TRANSIT_MOUNT=transit"},
				{"--env", "CACTUSFS_VAULT_KEY=cactusfs"},
				{"--env", "CACTUSFS_VAULT_NAMESPACE=team"},
				{"--env", "CACTUSFS_VAULT_TOKEN_FILE=/var/lib/cactusfs/secrets/vault-token"},
			},
			not: []string{"CACTUSFS_KMS_KEY_FILE=/var/lib/cactusfs/config/kms.json", "CACTUSFS_KMS=local"},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				rules := engineState{e: e, devInit: devInitOutput(), ready: true}.rules(t)
				r.rules(append([]rule{{Tool: "curl", Contains: []string{c.ready}, Stdout: "200"}}, rules...)...)
				res := dockerRun(r, e, withVault(r, imageEnv+c.env), "--install")
				res.want(t, 0, "ready: cactusfs runs")
				if c.warned != "" && !strings.Contains(res.stderr, c.warned) {
					t.Errorf("no warning %q:\n%s", c.warned, res.stderr)
				}
				run := runOf(t, res)
				holds(t, run, c.want...)
				for _, n := range c.not {
					if slices.Contains(run, n) {
						t.Errorf("the node is run with %q", n)
					}
				}
			})
		}
	})
}

// TestDockerRefused: what refuses an install does so before anything
// changes - no container, volume or secret is made, no STATE_DIR entry
// written: a port in use (answered, or published by another container), a
// container or volume of the deployment's names without its labels or of
// another target, drives that do not match DRIVES, data volumes with no
// record or no keys, a Vault CA that would not reach the node, no curl.
func TestDockerRefused(t *testing.T) {
	t.Parallel()
	e := dockerEngines[0]
	labelled := func(n int, meta bool) []string {
		var out []string
		for i := 1; i <= n; i++ {
			out = append(out, "cactusfs-data"+string(rune('0'+i))+" cactusfs dd")
		}
		if meta {
			out = append(out, "cactusfs-meta cactusfs dd")
		}
		return out
	}
	cases := []struct {
		name, env string
		args      []string
		change    func(s *engineState)
		without   string
		want      string
	}{
		{name: "the S3 port answers", change: func(s *engineState) { s.inUse = []string{"http://127.0.0.1:9000/"} }, want: "port 9000 on 127.0.0.1 is in use"},
		{name: "the admin port answers", env: "ADMIN_PORT=19001\n", change: func(s *engineState) { s.inUse = []string{"http://127.0.0.1:19001/"} }, want: "port 19001 on 127.0.0.1 is in use"},
		{name: "another container publishes the port", change: func(s *engineState) { s.others = "web|0.0.0.0:9000->80/tcp\n" }, want: "port 9000 is published by the container web"},
		// Docker groups consecutive ports into a range: the port inside one
		// is published too.
		{name: "another container publishes a range", change: func(s *engineState) { s.others = "web|9002/tcp, 0.0.0.0:8999-9001->80-82/tcp\n" }, want: "port 9000 is published by the container web"},
		{name: "an unlabelled container", change: func(s *engineState) { s.ctr = "||running|nginx" }, want: "a container named cactusfs is there without the label io.cactusfs.deploy.name=cactusfs"},
		{name: "another target's container", change: func(s *engineState) { s.ctr = "cactusfs|podman|running|" + nodeImage }, want: "is the deployment's on the target 'podman', not dd"},
		{name: "an unlabelled volume", change: func(s *engineState) { s.data = []string{"cactusfs-data1  "} }, want: "a volume named cactusfs-data1 is there without the label"},
		{name: "another target's volume", change: func(s *engineState) { s.data = []string{"cactusfs-meta cactusfs ce"} }, want: "the volume cactusfs-meta is the deployment's on the target 'ce'"},
		{name: "a drive beyond DRIVES", env: "DRIVES=2\n", change: func(s *engineState) { s.data = labelled(3, false) }, want: "DRIVES=2: a drive would be left out"},
		{name: "drives changed after a run", change: func(s *engineState) { s.data = labelled(2, true) }, want: "has run with 2 drive volumes and DRIVES=4"},
		{name: "data with no record", change: func(s *engineState) { s.data = labelled(4, true) }, want: "holds no record of them"},
		{name: "data without its keys", args: []string{"--adopt"}, change: func(s *engineState) { s.data = labelled(4, true) }, want: "neither the volume cactusfs-config nor"},
		{name: "a Vault CA", env: vaultEnv + "VAULT_CA_FILE=/etc/vault-ca.pem\n", want: "VAULT_CA_FILE is not put into the config volume"},
		{name: "no curl", without: "curl", want: "curl is needed"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				s := engineState{e: e, devInit: devInitOutput(), ready: true}
				if c.change != nil {
					c.change(&s)
				}
				r.rules(s.rules(t)...)
				if c.without != "" {
					r.without(c.without)
				}
				res := dockerRun(r, e, withVault(r, imageEnv+c.env), append([]string{"--install"}, c.args...)...)
				res.want(t, 2, c.want)
				noChange(t, res)
				if _, err := os.Stat(r.stateDir()); err == nil {
					t.Errorf("a refused install wrote STATE_DIR: %v", tree(t, r.home))
				}
			})
		}
	})
}

// TestDockerPortsOfOthers: another container holds S3_PORT or ADMIN_PORT
// only by a TCP binding on every address, or on the bind address checked
// (any address when the bind address is every address): UDP and SCTP, and
// another address of the machine, leave the port free.
func TestDockerPortsOfOthers(t *testing.T) {
	t.Parallel()
	e := dockerEngines[0]
	cases := []struct {
		name, env, others string
		refused           bool
	}{
		{name: "udp", others: "web|0.0.0.0:9000->9000/udp"},
		{name: "sctp", others: "web|0.0.0.0:9000->9000/sctp"},
		{name: "another address", others: "web|192.0.2.10:9000->80/tcp"},
		{name: "a range on another address", others: "web|192.0.2.10:8999-9001->80-82/tcp"},
		{name: "the bind address", others: "web|127.0.0.1:9000->80/tcp", refused: true},
		{name: "every address", others: "web|0.0.0.0:9000->80/tcp", refused: true},
		{name: "every IPv6 address", others: "web|[::]:9000->80/tcp", refused: true},
		{name: "no address", others: "web|9000->80/tcp", refused: true},
		{name: "udp beside tcp", others: "web|0.0.0.0:9000->9000/udp, 127.0.0.1:9000->9000/tcp", refused: true},
		{name: "another address, BIND every address", env: "BIND=0.0.0.0\n", others: "web|192.0.2.10:9000->80/tcp", refused: true},
		{name: "ADMIN_BIND's address", env: "ADMIN_BIND=192.0.2.10\n", others: "web|192.0.2.10:9001->80/tcp", refused: true},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				r.rules(engineState{e: e, devInit: devInitOutput(), ready: true, others: c.others + "\n"}.rules(t)...)
				res := dockerRun(r, e, imageEnv+c.env, "--install")
				if c.refused {
					res.want(t, 2, "is published by the container web")
					noChange(t, res)
					return
				}
				res.want(t, 0, "ready: cactusfs runs")
			})
		}
	})
}

// TestDockerOwnPortsByState: the node's own ports are read with "port"
// while it holds them - running, paused or restarting - and not taken for
// another's; an exited node holds none, and is not asked.
func TestDockerOwnPortsByState(t *testing.T) {
	t.Parallel()
	upgrade := strings.Replace(imageEnv, nodeImage, newImage, 1)
	eachShell(t, func(t *testing.T, r *rig) {
		e := dockerEngines[0]
		base := dockerInstalled(t, r, e, imageEnv)
		state := base.stateOf(e.targetID)
		for _, st := range []string{"running", "paused", "restarting", "exited"} {
			t.Run(st, func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				s := installedState(t, e, state, nodeImage)
				s.ctr = "cactusfs|dd|" + st + "|" + nodeImage
				if st == "exited" {
					s.ports = ""
				} else {
					// The node's own ports answer a probe: only reading
					// them with "port" lets the upgrade go on.
					s.inUse = []string{"http://127.0.0.1:9000/", "http://127.0.0.1:9001/"}
				}
				r.rules(s.rules(t)...)
				res := dockerRun(r, e, upgrade, "--install")
				res.want(t, 0, "ready: cactusfs runs "+newImage)
				if asked := len(callsOf(res, e.tool, "port", "cactusfs")); asked != map[bool]int{true: 0, false: 1}[st == "exited"] {
					t.Errorf("port was asked %d times for a node %s", asked, st)
				}
			})
		}
	})
}

// TestDockerUpgrade: --install on an installed deployment stops the node,
// keeps it as cactusfs-previous, runs the new one from the new image with
// the same volumes - after the secrets are compared, nothing made or put -
// and removes the old one only once the new one is ready; the ports the
// node holds are not taken for another's. A new node that cannot be run or
// is not ready within WAIT_TIMEOUT fails the run, saying the node is down,
// that the old one is kept, and how to go back to it. A cactusfs-previous
// already there (an upgrade that did not finish) is refused before
// anything changes.
func TestDockerUpgrade(t *testing.T) {
	t.Parallel()
	upgrade := strings.Replace(imageEnv, nodeImage, newImage, 1)
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range []dockerEngine{dockerEngines[0], dockerEngines[2]} {
			base := dockerInstalled(t, r, e, imageEnv)
			state := base.stateOf(e.targetID)
			t.Run(e.target, func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				s := installedState(t, e, state, nodeImage)
				// The node's own ports answer: they are not refused.
				s.inUse = []string{"http://127.0.0.1:9000/", "http://127.0.0.1:9001/"}
				r.rules(s.rules(t)...)
				entry := tree(t, r.stateOf(e.targetID))
				res := dockerRun(r, e, upgrade, "--install")
				res.want(t, 0, "this run upgrades it, keeping its volumes", "upgrade: stopping cactusfs (image "+nodeImage+") and keeping it as cactusfs-previous",
					"upgrade: removed cactusfs-previous", "ready: cactusfs runs "+newImage)
				got := steps(res)
				before(t, got, "stop cactusfs", "rename cactusfs cactusfs-previous", "run "+newImage, "readyz", "rm cactusfs-previous")
				// On Podman the old node's policy is cleared before the new one
				// runs: podman-restart.service starts every container whose
				// policy is "always" at a reboot. Docker needs nothing.
				if e.tool == "podman" {
					before(t, got, "rename cactusfs cactusfs-previous", "update --restart no cactusfs-previous", "run "+newImage)
				} else if i := slices.IndexFunc(got, func(s string) bool { return strings.HasPrefix(s, "update ") }); i >= 0 {
					t.Errorf("docker was asked %q", got[i])
				}
				for _, st := range got {
					if strings.HasPrefix(st, "volume ") || strings.HasPrefix(st, "put ") || strings.HasPrefix(st, "probe ") || st == "dev-init" || st == "rm cactusfs" {
						t.Errorf("the upgrade did %q", st)
					}
				}
				if c := callsOf(res, e.tool, "stop", "-t", "40", "cactusfs"); len(c) != 1 {
					t.Errorf("the node was not stopped with time to drain: %v", c)
				}
				holds(t, runOf(t, res),
					[]string{"--mount", "type=volume,source=cactusfs-data1,target=/var/lib/cactusfs/data1"},
					[]string{"--mount", "type=volume,source=cactusfs-meta,target=/var/lib/cactusfs/meta"},
					[]string{"--mount", "type=volume,source=cactusfs-config,target=/var/lib/cactusfs/config,readonly"})
				if after := tree(t, r.stateOf(e.targetID)); !equalTrees(entry, after) {
					t.Error("the upgrade changed the entry of STATE_DIR")
				}
			})
			for _, c := range []struct {
				name string
				env  string
				fail func(s *engineState) []rule
				want string
			}{
				{"not ready", "WAIT_TIMEOUT=3\n", func(s *engineState) []rule { s.ready = false; return nil }, "did not become ready within WAIT_TIMEOUT=3"},
				{"not run", "", func(*engineState) []rule {
					return []rule{{Tool: e.tool, Contains: []string{"run", "-d"}, Stderr: "port is already allocated\n", Exit: 125}}
				}, "could not be run"},
			} {
				t.Run(e.target+" "+c.name, func(t *testing.T) {
					t.Parallel()
					r := clone(t, base)
					s := installedState(t, e, state, nodeImage)
					extra := c.fail(&s)
					r.rules(append(extra, s.rules(t)...)...)
					res := dockerRun(r, e, upgrade+c.env, "--install")
					res.want(t, 1, "the node is DOWN", c.want, "kept, stopped, as cactusfs-previous (image "+nodeImage+")",
						"rename cactusfs-previous cactusfs", "IMAGE="+nodeImage)
					if index(steps(res), "rm cactusfs-previous") >= 0 {
						t.Error("the node the new one replaced was removed")
					}
					if c.name == "not ready" {
						if n := len(callsOf(res, "curl", readyzURL)); n < 2 {
							t.Errorf("/readyz was asked %d times", n)
						}
					}
					if e.tool == "podman" {
						res.want(t, 1, "podman update --restart always cactusfs")
						if strings.Contains(res.output(), "keeps the restart policy always") {
							t.Error("the old node's policy was cleared, and the run says it was not")
						}
					}
				})
			}
			if e.tool == "podman" {
				// A Podman before 5.1 has no "update --restart": the run warns
				// and goes on - the old node is removed once the new one is
				// ready; when it is not, the run says a reboot would start it.
				t.Run(e.target+" old podman", func(t *testing.T) {
					t.Parallel()
					r := clone(t, base)
					s := installedState(t, e, state, nodeImage)
					old := rule{Tool: "podman", Contains: []string{"update", "--restart", "no"}, Stderr: "Error: unknown flag: --restart\n", Exit: 125}
					r.rules(append([]rule{old}, s.rules(t)...)...)
					res := dockerRun(r, e, upgrade, "--install")
					res.want(t, 0, "cannot clear the restart policy of cactusfs-previous", "upgrade: removed cactusfs-previous")
					s.ready = false
					r.rules(append([]rule{old}, s.rules(t)...)...)
					res = dockerRun(r, e, upgrade+"WAIT_TIMEOUT=1\n", "--install")
					res.want(t, 1, "the node is DOWN", "cactusfs-previous keeps the restart policy always, so podman-restart.service would start it at a reboot")
				})
			}
			// A rename that fails after the stop: the node is started again,
			// and the run says whether it runs or is down, and how to start
			// it; nothing new is run.
			for _, c := range []struct {
				name  string
				extra []rule
				want  string
			}{
				{
					"not renamed",
					[]rule{{Tool: e.tool, Contains: []string{"rename"}, Stderr: "Error: conflict\n", Exit: 1}},
					"it was started again and runs " + nodeImage + " as before - nothing was upgraded",
				},
				{"not renamed nor started", []rule{
					{Tool: e.tool, Contains: []string{"rename"}, Stderr: "Error: conflict\n", Exit: 1},
					{Tool: e.tool, Contains: []string{"start", "cactusfs"}, Stderr: "Error: engine\n", Exit: 1},
				}, "the node is DOWN. Start it with: " + map[string]string{"docker": "docker --context default", "podman": "podman"}[e.tool] + " start cactusfs"},
			} {
				t.Run(e.target+" "+c.name, func(t *testing.T) {
					t.Parallel()
					r := clone(t, base)
					r.rules(append(c.extra, installedState(t, e, state, nodeImage).rules(t)...)...)
					res := dockerRun(r, e, upgrade, "--install")
					res.want(t, 1, "could not be renamed cactusfs-previous", c.want)
					got := steps(res)
					before(t, got, "stop cactusfs", "rename cactusfs cactusfs-previous")
					if len(callsOf(res, e.tool, "start", "cactusfs")) != 1 {
						t.Errorf("the node was not started again: %q", got)
					}
					if slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "run ") }) {
						t.Errorf("a new node was run: %q", got)
					}
				})
			}
			t.Run(e.target+" unfinished", func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				s := installedState(t, e, state, nodeImage)
				r.rules(append([]rule{
					{Tool: e.tool, Contains: []string{"ps", "-a", "name=cactusfs-previous"}, Stdout: "cactusfs-previous\n"},
					{Tool: e.tool, Contains: []string{"container", "inspect", "cactusfs-previous"}, Stdout: "cactusfs|" + e.target + "|exited|" + nodeImage + "\n"},
				}, s.rules(t)...)...)
				res := dockerRun(r, e, upgrade, "--install")
				res.want(t, 2, "an upgrade did not finish: cactusfs-previous holds the node it replaced", "rename cactusfs-previous cactusfs")
				noChange(t, res)
			})
		}
	})
}

// TestDockerStatus: --status says the node's state, image and version, the
// volumes, where it is reached and what /readyz answers - 0 when ready or
// not installed, 1 when installed and not ready - and asks the engine only
// by the deployment's label, changing nothing.
func TestDockerStatus(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range []dockerEngine{dockerEngines[0], dockerEngines[2]} {
			base := dockerInstalled(t, r, e, imageEnv)
			state := base.stateOf(e.targetID)
			for _, c := range []struct {
				name  string
				state func() engineState
				code  int
				want  []string
			}{
				{"not installed", func() engineState { return engineState{e: e} }, 0, []string{"cactusfs is not installed on"}},
				{"uninstalled", func() engineState {
					s := installedState(t, e, state, nodeImage)
					s.ctr, s.ports, s.status = "", "", ""
					return s
				}, 0, []string{"cactusfs is not installed on", "its volumes are kept: cactusfs-config cactusfs-data1"}},
				{"ready", func() engineState { return installedState(t, e, state, nodeImage) }, 0, []string{
					"node: cactusfs, running, image " + nodeImage + ", version 0.7.0-1", "volumes: cactusfs-config cactusfs-data1",
					"S3: https://127.0.0.1:9000", "admin API and console: https://127.0.0.1:9001", "/readyz: 200",
					"the CA to trust: ", "/cactusfs-deploy/" + e.targetID + "/cactusfs/config/s3-ca.pem",
				}},
				{"not ready", func() engineState {
					s := installedState(t, e, state, nodeImage)
					s.ready = false
					return s
				}, 1, []string{"/readyz: 000", "not ready"}},
			} {
				t.Run(e.target+" "+c.name, func(t *testing.T) {
					t.Parallel()
					r := clone(t, base)
					r.rules(c.state().rules(t)...)
					// A real certificate in the entry: --status says when it
					// expires (ADR-0033 section 3), read by openssl.
					cert := filepath.Join(r.stateOf(e.targetID), "config", "s3.pem")
					put(t, cert, certExpiring(t, time.Date(2031, time.March, 12, 3, 4, 5, 0, time.UTC)))
					res := dockerRun(r, e, imageEnv, "--status")
					res.want(t, c.code, c.want...)
					if c.name == "ready" {
						if systemTool("openssl") != "" {
							res.want(t, 0, "certificate: "+cert+" expires Mar 12 03:04:05 2031 GMT")
						} else {
							res.want(t, 0, "certificate: "+cert+" (its expiry is shown when openssl is installed)")
						}
					}
					noChange(t, res)
					for _, call := range res.calls {
						if (call.Tool == "docker" || call.Tool == "podman") && !slices.Contains(call.Args, byLabel) {
							t.Errorf("%s was not asked by the deployment's label: %q", call.Tool, call.Args)
						}
					}
				})
			}
		}
	})
}

// certExpiring is a self-signed certificate, as PEM, that expires at the
// time given.
func certExpiring(t *testing.T, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    notAfter.AddDate(-1, 0, 0),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestDockerUninstall: --uninstall stops and removes the node and keeps
// every volume and the entry of STATE_DIR; an unlabelled container of the
// name is refused and left.
func TestDockerUninstall(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		e := dockerEngines[0]
		base := dockerInstalled(t, r, e, imageEnv)
		state := base.stateOf(e.targetID)
		t.Run("installed", func(t *testing.T) {
			t.Parallel()
			r := clone(t, base)
			r.rules(installedState(t, e, state, nodeImage).rules(t)...)
			entry := tree(t, r.stateOf(e.targetID))
			res := dockerRun(r, e, imageEnv, "--uninstall")
			res.want(t, 0, "removed the container cactusfs", "kept: the volumes cactusfs-config cactusfs-data1 cactusfs-data2 cactusfs-data3 cactusfs-data4 cactusfs-meta cactusfs-secrets")
			if !slices.Equal(steps(res), []string{"stop cactusfs", "rm cactusfs", "listing"}) {
				t.Errorf("the steps were %q", steps(res))
			}
			if after := tree(t, r.stateOf(e.targetID)); !equalTrees(entry, after) {
				t.Error("--uninstall changed the entry of STATE_DIR")
			}
		})
		t.Run("nothing there", func(t *testing.T) {
			t.Parallel()
			r := clone(t, base)
			r.rules(engineState{e: e}.rules(t)...)
			res := dockerRun(r, e, imageEnv, "--uninstall")
			res.want(t, 0, "no container cactusfs on Docker Desktop")
			noChange(t, res)
		})
		t.Run("an unlabelled container", func(t *testing.T) {
			t.Parallel()
			r := clone(t, base)
			s := installedState(t, e, state, nodeImage)
			s.ctr = "||running|nginx"
			r.rules(s.rules(t)...)
			res := dockerRun(r, e, imageEnv, "--uninstall")
			res.want(t, 2, "without the label io.cactusfs.deploy.name=cactusfs")
			noChange(t, res)
		})
		// The node an unfinished upgrade kept: checked by its name and target
		// labels before anything is removed, and removed after the node.
		previous := func(labels string) []rule {
			return []rule{
				{Tool: e.tool, Contains: []string{"ps", "-a", "name=cactusfs-previous"}, Stdout: "cactusfs-previous\n"},
				{Tool: e.tool, Contains: []string{"container", "inspect", "cactusfs-previous"}, Stdout: labels + "|exited|" + nodeImage + "\n"},
			}
		}
		t.Run("a previous node", func(t *testing.T) {
			t.Parallel()
			r := clone(t, base)
			r.rules(append(previous("cactusfs|dd"), installedState(t, e, state, nodeImage).rules(t)...)...)
			res := dockerRun(r, e, imageEnv, "--uninstall")
			res.want(t, 0, "removed cactusfs-previous, the node an unfinished upgrade kept")
			if got := steps(res); !slices.Equal(got, []string{"stop cactusfs", "rm cactusfs", "rm cactusfs-previous", "listing"}) {
				t.Errorf("the steps were %q", got)
			}
		})
		for _, labels := range []string{"|", "cactusfs|podman"} {
			t.Run("a previous node labelled "+labels, func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				r.rules(append(previous(labels), installedState(t, e, state, nodeImage).rules(t)...)...)
				res := dockerRun(r, e, imageEnv, "--uninstall")
				res.want(t, 2, "cactusfs-previous")
				noChange(t, res)
			})
		}
		// The helpers a run left behind: found and checked by both labels
		// before the node is stopped, removed after it; one of another
		// target refuses the run before anything is removed.
		helper := func(name, labels string) []rule {
			return []rule{
				{Tool: e.tool, Contains: []string{"ps", "-a", "name=" + name}, Stdout: name + "\n"},
				{Tool: e.tool, Contains: []string{"container", "inspect", name}, Stdout: labels + "|created|" + nodeImage + "\n"},
			}
		}
		t.Run("helpers left", func(t *testing.T) {
			t.Parallel()
			r := clone(t, base)
			rules := append(helper("cactusfs-volumes", "cactusfs|dd"), helper("cactusfs-dev-init", "cactusfs|dd")...)
			r.rules(append(rules, installedState(t, e, state, nodeImage).rules(t)...)...)
			res := dockerRun(r, e, imageEnv, "--uninstall")
			res.want(t, 0, "removed cactusfs-volumes, a helper a run left behind", "removed cactusfs-dev-init, a helper a run left behind")
			if got := steps(res); !slices.Equal(got, []string{"stop cactusfs", "rm cactusfs", "rm cactusfs-volumes", "rm cactusfs-dev-init", "listing"}) {
				t.Errorf("the steps were %q", got)
			}
		})
		for _, name := range []string{"cactusfs-volumes", "cactusfs-dev-init"} {
			t.Run("a helper of another target, "+name, func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				r.rules(append(helper(name, "cactusfs|podman"), installedState(t, e, state, nodeImage).rules(t)...)...)
				res := dockerRun(r, e, imageEnv, "--uninstall")
				res.want(t, 2, "the container "+name+" is the deployment's on the target 'podman', not dd")
				noChange(t, res)
			})
		}
		t.Run("another engine", func(t *testing.T) {
			t.Parallel()
			r := clone(t, base)
			s := installedState(t, e, state, nodeImage)
			r.rules(append([]rule{{Tool: e.tool, Contains: []string{"info"}, Stdout: "ENGINE-TWO\n"}}, s.rules(t)...)...)
			res := dockerRun(r, e, imageEnv, "--uninstall")
			res.want(t, 2, "now reach another, 'ENGINE-TWO': nothing of the deployment is removed through it")
			noChange(t, res)
		})
	})
}

// TestDockerPurge: --uninstall --purge goes on only with the deployment's
// name (a terminal's answer or --purge-confirm), on the engine recorded for
// it - or, with no record, only with --adopt, which records this engine
// first - and in the order of ADR-0033 section 3: the node, the data
// volumes, a listing that proves none is left, the secrets' volumes, a
// listing that proves no volume of the deployment is left, and last the
// entry of STATE_DIR alone - a sibling entry stays. A failure before the
// last step keeps the entry: a volume still there, a listing or an inspect
// that fails, a volume of another target that appears - none is taken for
// "nothing left". A labelled volume of another target refuses the purge
// before anything is deleted.
func TestDockerPurge(t *testing.T) {
	t.Parallel()
	confirm := []string{"--uninstall", "--purge", "--purge-confirm=cactusfs"}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range []dockerEngine{dockerEngines[0], dockerEngines[2]} {
			base := dockerInstalled(t, r, e, imageEnv)
			state := base.stateOf(e.targetID)
			// sibling is another deployment's entry on the same target.
			sibling := func(r *rig) string {
				dir := filepath.Join(filepath.Dir(r.stateOf(e.targetID)), "other")
				put(t, filepath.Join(dir, "target"), "kept\n")
				return dir
			}
			// kept fails the test unless the entry, its keys, is still there
			// and the secrets' volumes were not deleted.
			kept := func(t *testing.T, r *rig, res result) {
				t.Helper()
				if _, err := os.Stat(filepath.Join(r.stateOf(e.targetID), "config", "kms.json")); err != nil {
					t.Error("the entry of STATE_DIR is gone")
				}
				if len(callsOf(res, e.tool, "volume", "rm", "cactusfs-secrets")) != 0 {
					t.Error("the secrets' volumes were deleted")
				}
			}
			afterData := []string{"volume", "rm", "cactusfs-data1"}
			t.Run(e.target, func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				other := sibling(r)
				s := installedState(t, e, state, nodeImage)
				s.purged = true
				r.rules(s.rules(t)...)
				res := dockerRun(r, e, imageEnv, confirm...)
				res.want(t, 0, "purge: no data volume of cactusfs is left", "purge: no volume of cactusfs is left", "purge: deleted "+r.stateOf(e.targetID))
				want := []string{
					"listing", "stop cactusfs", "rm cactusfs",
					"volume rm cactusfs-data1 cactusfs-data2 cactusfs-data3 cactusfs-data4 cactusfs-meta", "listing",
					"volume rm cactusfs-config cactusfs-secrets", "listing",
				}
				if got := steps(res); !slices.Equal(got, want) {
					t.Errorf("the steps were\n%q\nwant\n%q", got, want)
				}
				if _, err := os.Stat(r.stateOf(e.targetID)); err == nil {
					t.Error("the entry of STATE_DIR is still there")
				}
				if fileOf(t, filepath.Join(other, "target")) != "kept\n" {
					t.Error("the sibling entry was touched")
				}
			})
			t.Run(e.target+" without its name", func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				r.rules(installedState(t, e, state, nodeImage).rules(t)...)
				res := dockerRun(r, e, imageEnv, "--uninstall", "--purge")
				res.want(t, 2, "type its name when asked at a terminal, or say --purge-confirm=cactusfs")
				noChange(t, res)
				kept(t, r, res)
			})
			for _, c := range []struct {
				name  string
				extra []rule
				code  int
				want  string
			}{
				{"a data volume left", nil, 1, "is still there after it was deleted; its secrets and "},
				{"a listing that fails after the data", []rule{
					{Tool: e.tool, Contains: []string{"volume", "ls", byLabel}, After: afterData, Stderr: "Cannot connect to the engine\n", Exit: 1},
				}, 1, "could not list the volumes of"},
				{"an inspect that fails after the data", []rule{
					{Tool: e.tool, Contains: []string{"volume", "inspect", targetFormat}, After: afterData, Stderr: "Cannot connect to the engine\n", Exit: 1},
				}, 1, "could not inspect the volumes of the deployment"},
				{"another target's volume after the data", []rule{
					{Tool: e.tool, Contains: []string{"volume", "ls", byLabel}, After: afterData, Stdout: "cactusfs-config\ncactusfs-secrets\ncactusfs-x\n"},
					{Tool: e.tool, Contains: []string{"volume", "inspect", targetFormat}, After: afterData, Stdout: "cactusfs-config " + e.target + "\ncactusfs-secrets " + e.target + "\ncactusfs-x rancher\n"},
				}, 1, "purge: the volume cactusfs-x carries the deployment's name and the target 'rancher'"},
			} {
				t.Run(e.target+" "+c.name, func(t *testing.T) {
					t.Parallel()
					r := clone(t, base)
					s := installedState(t, e, state, nodeImage)
					r.rules(append(c.extra, s.rules(t)...)...)
					res := dockerRun(r, e, imageEnv, confirm...)
					res.want(t, c.code, c.want)
					if len(callsOf(res, e.tool, afterData...)) != 1 {
						t.Fatalf("the data volumes were not deleted first: %q", steps(res))
					}
					kept(t, r, res)
				})
			}
			// The last proof, after the secrets' volumes: a listing that
			// fails, or a volume of another target in it, keeps the entry -
			// the keys of whatever may be left.
			afterKeys := []string{"volume", "rm", "cactusfs-secrets"}
			for _, c := range []struct {
				name  string
				extra []rule
				code  int
				want  string
			}{
				{"a listing that fails after the secrets", []rule{
					{Tool: e.tool, Contains: []string{"volume", "ls", byLabel}, After: afterKeys, Stderr: "Cannot connect to the engine\n", Exit: 1},
				}, 1, "could not list the volumes of"},
				{"another target's volume after the secrets", []rule{
					{Tool: e.tool, Contains: []string{"volume", "ls", byLabel}, After: afterKeys, Stdout: "cactusfs-x\n"},
					{Tool: e.tool, Contains: []string{"volume", "inspect", targetFormat}, After: afterKeys, Stdout: "cactusfs-x rancher\n"},
				}, 1, "purge: the volume cactusfs-x carries the deployment's name and the target 'rancher'"},
			} {
				t.Run(e.target+" "+c.name, func(t *testing.T) {
					t.Parallel()
					r := clone(t, base)
					s := installedState(t, e, state, nodeImage)
					s.purged = true
					r.rules(append(c.extra, s.rules(t)...)...)
					res := dockerRun(r, e, imageEnv, confirm...)
					res.want(t, c.code, c.want)
					if len(callsOf(res, e.tool, afterKeys...)) != 1 {
						t.Fatalf("the secrets' volumes were not deleted: %q", steps(res))
					}
					if _, err := os.Stat(filepath.Join(r.stateOf(e.targetID), "config", "kms.json")); err != nil {
						t.Error("the entry of STATE_DIR is gone")
					}
				})
			}
			t.Run(e.target+" another engine", func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				s := installedState(t, e, state, nodeImage)
				r.rules(append([]rule{{Tool: e.tool, Contains: []string{"info"}, Stdout: "ENGINE-TWO\n"}}, s.rules(t)...)...)
				res := dockerRun(r, e, imageEnv, confirm...)
				res.want(t, 2, "now reach another, 'ENGINE-TWO': nothing of the deployment is removed through it")
				noChange(t, res)
				kept(t, r, res)
			})
			t.Run(e.target+" another target's volume", func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				s := installedState(t, e, state, nodeImage)
				s.labelled[1] = "cactusfs-data1 rancher"
				r.rules(s.rules(t)...)
				res := dockerRun(r, e, imageEnv, confirm...)
				res.want(t, 2, "the volume cactusfs-data1 carries the deployment's name and the target 'rancher'")
				noChange(t, res)
				kept(t, r, res)
			})
			t.Run(e.target+" no record", func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				remove(t, filepath.Join(r.stateOf(e.targetID), "target"))
				s := installedState(t, e, state, nodeImage)
				s.purged = true
				r.rules(s.rules(t)...)
				res := dockerRun(r, e, imageEnv, confirm...)
				res.want(t, 2, "holds no record of cactusfs", "say --adopt")
				noChange(t, res)
				kept(t, r, res)
				// --adopt records this engine once what is labelled has been
				// found and checked - after the listing, before the first
				// stop - then purges.
				res = dockerRun(r, e, imageEnv, append(slices.Clone(confirm), "--adopt")...)
				res.want(t, 0, "state: --adopt: recorded", "engine "+e.id, "purged: cactusfs")
				out := res.stdout
				if a, b := strings.Index(out, "state: --adopt: recorded"), strings.Index(out, "removed the container cactusfs"); a < 0 || b < 0 || a > b {
					t.Errorf("the record was not written before the node was removed:\n%s", out)
				}
				if got := steps(res); len(got) == 0 || got[0] != "listing" {
					t.Errorf("the steps were %q, want the listing first", got)
				}
				if _, err := os.Stat(r.stateOf(e.targetID)); err == nil {
					t.Error("the entry of STATE_DIR is still there")
				}
			})
			// --adopt is refused, and writes no record, when what it would
			// adopt is not this deployment's to delete, or when the engine
			// holds nothing labelled with the name: the settings may reach
			// another engine.
			for _, c := range []struct {
				name  string
				state func() engineState
				want  string
			}{
				{"adopt, another target's volume", func() engineState {
					s := installedState(t, e, state, nodeImage)
					s.labelled[1] = "cactusfs-data1 rancher"
					return s
				}, "the volume cactusfs-data1 carries the deployment's name and the target 'rancher'"},
				{"adopt, nothing labelled", func() engineState { return engineState{e: e} }, "holds no container or volume labelled io.cactusfs.deploy.name=cactusfs; the settings may reach another engine"},
			} {
				t.Run(e.target+" "+c.name, func(t *testing.T) {
					t.Parallel()
					r := clone(t, base)
					remove(t, filepath.Join(r.stateOf(e.targetID), "target"))
					r.rules(c.state().rules(t)...)
					res := dockerRun(r, e, imageEnv, append(slices.Clone(confirm), "--adopt")...)
					res.want(t, 2, c.want)
					noChange(t, res)
					kept(t, r, res)
					if _, err := os.Stat(filepath.Join(r.stateOf(e.targetID), "target")); err == nil {
						t.Error("a refused --adopt wrote the record")
					}
				})
			}
			t.Run(e.target+" dry run", func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				r.rules(installedState(t, e, state, nodeImage).rules(t)...)
				entry := tree(t, r.stateOf(e.targetID))
				res := dockerRun(r, e, imageEnv, "--uninstall", "--purge", "--dry-run")
				res.want(t, 0, "a purge asks for the name of the deployment", "run: rm -rf --")
				noChange(t, res)
				if after := tree(t, r.stateOf(e.targetID)); !equalTrees(entry, after) {
					t.Error("a dry run changed the entry of STATE_DIR")
				}
			})
		}
	})
}

// TestDockerActions: the actions of a standalone deployment on a Docker
// engine are built - each asks the engine and changes nothing when there is
// nothing to change (the fakes hold nothing); a dry run of an install from
// binaries checks the image (PLAN 10.4) and says what it would build and
// run, changing nothing; a purge of a deployment STATE_DIR holds no record
// of is refused before the engine is asked.
func TestDockerActions(t *testing.T) {
	cases := []struct {
		name string
		args []string
		code int
		want string
		// binaries: the rig has the binaries of make image-binaries, and its
		// engine answers what image_check asks.
		binaries bool
	}{
		{"dd install", []string{"--docker", "dd", "--install", "--dry-run"}, 0, "dry run: once built, the image is checked and tagged cactusfs:0.7.0-412-<12 hex of its ID>", true},
		{"podman status", []string{"--docker", "podman", "--status"}, 0, "cactusfs is not installed on Podman", false},
		{"ce here", []string{"--docker", "ce", "--uninstall"}, 0, "no container cactusfs on Docker CE", false},
		{"another target", []string{"--docker", "dd", "--status", "--another-target"}, 0, "cactusfs is not installed on Docker Desktop", false},
		{"purge confirm with equals, no record", []string{"--docker", "dd", "--uninstall", "--purge", "--purge-confirm=cactusfs"}, 2, "holds no record of cactusfs", false},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				if c.binaries {
					r = secretsRig(t, r)
					r.rules(madeHere("docker", newImgFix(imgCommit))...)
				}
				res := r.withEnv("KMS=local\n", c.args...)
				res.want(t, c.code, c.want)
				if len(res.calls) == 0 && c.code != 2 {
					t.Fatal("the engine was not asked")
				}
				noChange(t, res)
			})
		}
	})
}

// TestDockerImageRef: the image of the node comes from docker_image_ref
// alone, called plainly (never in a command substitution) and leaving the
// reference in _dk_image: when PLAN 10.4's image_deliver is there it runs
// in the script's own shell - what it sets is seen - and RESOLVED_IMAGE,
// which image_make left and image_deliver may name anew (a push), is the
// image whatever IMAGE_SOURCE says; without it, IMAGE with
// IMAGE_SOURCE=image; else the run stops as not built yet. A reference
// that is empty or not one fails the run. An install from binaries runs the
// image it built, by its tag, never pulled.
func TestDockerImageRef(t *testing.T) {
	t.Parallel()
	const lib = `L=${1%/*}; . "$L/env.sh"; . "$L/docker.sh"; engine() { echo Docker Desktop; }; `
	const deliver = `RESOLVED_IMAGE=registry.example.test/c:0.7.0-1-0123456789ab; ` +
		`image_deliver() { echo "delivering $RESOLVED_IMAGE" >&2; RESOLVED_IMAGE=registry.example.test/c@sha256:` + imgDigest + `; }; `
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range []struct {
			name, code string
			exit       int
			stdout     string
			stderr     string
		}{
			{"image", "SETTINGS='IMAGE_SOURCE=image\nIMAGE=registry.example.test/c:1\n'; docker_image_ref; echo \"ref=[$_dk_image]\"", 0, "ref=[registry.example.test/c:1]\n", ""},
			{
				"image_deliver", "SETTINGS='IMAGE_SOURCE=image\nIMAGE=registry.example.test/c:1\n'; " + deliver + "docker_image_ref; echo \"ref=[$_dk_image]\"", 0,
				"ref=[registry.example.test/c@sha256:" + imgDigest + "]\n", "delivering registry.example.test/c:0.7.0-1-0123456789ab",
			},
			{"image_deliver, nothing resolved", "SETTINGS='IMAGE_SOURCE=binaries\nIMAGE=\n'; RESOLVED_IMAGE=; image_deliver() { :; }; docker_image_ref; echo reached", 1, "", "the image reference for the node is not one: ''"},
			{"not built", "SETTINGS='IMAGE_SOURCE=binaries\nIMAGE=\n'; docker_image_ref; echo reached", 3, "deploy: the image, binaries: built and put on Docker Desktop", "not built yet (PLAN 10.4)"},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				res := r.source(lib + c.code)
				if res.code != c.exit || !strings.Contains(res.stdout, c.stdout) || !strings.Contains(res.stderr, c.stderr) || strings.Contains(res.stdout, "reached") {
					t.Fatalf("exit %d, stdout %q, stderr %q", res.code, res.stdout, res.stderr)
				}
			})
		}
		t.Run("install", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			f := newImgFix(imgCommit)
			r.rules(append(madeHere("docker", f), engineState{e: dockerEngines[0], devInit: devInitOutput(), ready: true}.rules(t)...)...)
			res := dockerRun(r, dockerEngines[0], "KMS=local\n", "--install")
			res.want(t, 0, "image: built "+f.hex, "image: "+f.ref()+"\n", "ready: cactusfs runs "+f.ref())
			run := runOf(t, res)
			holds(t, run, []string{"--pull", "never"})
			if !slices.Equal(run[len(run)-2:], []string{f.ref(), "server"}) {
				t.Errorf("the node does not run %s server: %q", f.ref(), run[len(run)-2:])
			}
			noPullCommand(t, res)
		})
	})
}

// imageCheckReads are the questions image_check asks of an engine here, by
// the words of each: its endpoint (a Docker context's), where its service
// is (Podman's) and its platform.
var imageCheckReads = [][]string{{"context", "inspect"}, {"{{.Host.ServiceIsRemote}}"}, {"info", "{{.OSType}}/{{.Architecture}}"}, {"info", "{{.Host.OS}}/{{.Host.Arch}}"}}

// lineAt is where the first line of the output that holds the text is,
// among the lines of standard output, or -1.
func lineAt(res result, text string) int {
	return slices.IndexFunc(strings.Split(res.stdout, "\n"), func(l string) bool { return strings.Contains(l, text) })
}

// TestDockerInstallOrder: an install wires PLAN 10.4 into PLAN 10.5's order.
// image_check only reads, and does so after the ports are asked and before
// anything changes; image_make builds, tags and reads the version line of
// the image before dev-init and before any secret is put; image_deliver
// gets it to the target's engine after the secrets and before the volumes
// of the node and the node - on the target's own engine there is nothing
// to move, and from BUILD_HOST it is saved there and loaded on the target;
// the node runs RESOLVED_IMAGE, the image's tag, with --pull never.
func TestDockerInstallOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, env string
		e         dockerEngine
		// remote: the image is built on BUILD_HOST and loaded on the target.
		remote bool
	}{
		{name: "docker desktop", e: dockerEngines[0]},
		{name: "podman", e: dockerEngines[2]},
		{name: "built on BUILD_HOST", e: dockerEngines[0], env: "BUILD_HOST=builder@build.example.test\n", remote: true},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				f := newImgFix(imgCommit)
				rules := append(gitRules(""), madeHere(c.e.tool, f)...)
				rules = append(rules, rule{Tool: c.e.tool, Contains: []string{"save", f.ref()}, Stdout: f.tar})
				r.rules(append(rules, engineState{e: c.e, devInit: devInitOutput(), ready: true}.rules(t)...)...)
				res := dockerRun(r, c.e, "KMS=local\n"+c.env, "--install")
				res.want(t, 0, "ready: cactusfs runs "+f.ref())

				// image_check: what it asks - the engine's endpoint or where
				// its service is, and the target's platform - is asked after
				// the ports and before the build and the first change.
				ports := slices.IndexFunc(res.calls, func(c call) bool {
					return c.Tool == "curl" && slices.Contains(c.Args, "http://127.0.0.1:9001/")
				})
				build := slices.IndexFunc(res.calls, func(c call) bool { return slices.Contains(c.Args, "build") })
				firstChange := slices.IndexFunc(res.calls, mutating)
				if ports < 0 || build < 0 || firstChange < build {
					t.Fatalf("the ports at %d, the build at %d, the first change at %d: %v", ports, build, firstChange, res.calls)
				}
				// Built on BUILD_HOST: its host key is checked before ssh
				// reaches it, and the image is remote whatever the context.
				where := []string{"context", "inspect"}
				tool := c.e.tool
				switch {
				case c.remote:
					where, tool = []string{"builder@build.example.test", "true"}, "ssh"
					overSSH := slices.IndexFunc(res.calls, func(c call) bool { return c.Env["DOCKER_HOST"] != "" })
					if i := callIndex(res, tool, where...); i < 0 || i > overSSH {
						t.Errorf("the build host's key was checked at %d, and ssh first used at %d", i, overSSH)
					}
				case c.e.tool == "podman":
					where = []string{"{{.Host.ServiceIsRemote}}"}
				}
				for _, asked := range []struct {
					tool  string
					words []string
				}{{tool, where}, {c.e.tool, imageCheckReads[2]}, {c.e.tool, imageCheckReads[3]}} {
					i := callIndex(res, asked.tool, asked.words...)
					if asked.words[0] == "info" && i < 0 {
						continue // the other engine's format
					}
					if i < ports || i > build {
						t.Errorf("image_check asked %q at %d: the ports at %d, the build at %d", asked.words, i, ports, build)
					}
				}
				if callIndex(res, c.e.tool, imageCheckReads[2]...) < 0 && callIndex(res, c.e.tool, imageCheckReads[3]...) < 0 {
					t.Errorf("the target's platform was not asked: %v", res.calls)
				}

				// image_make before the secrets, image_deliver after them and
				// before the node's volumes and the node.
				got := steps(res)
				order := []string{"build", "tag " + f.ref(), "version", "dev-init", "put cactusfs-volumes:/var/lib/cactusfs/secrets", "put cactusfs-volumes:/var/lib/cactusfs/config"}
				if c.remote {
					order = append(order, "load")
				}
				order = append(order, "volume cactusfs-data1", "volume cactusfs-meta", "run "+f.ref(), "readyz")
				before(t, got, order...)
				if n := len(builds(res)); n != 1 {
					t.Fatalf("%d builds, want 1", n)
				}
				if b := builds(res)[0]; (b.Env["DOCKER_HOST"] != "") != c.remote {
					t.Errorf("the build ran with DOCKER_HOST=%q", b.Env["DOCKER_HOST"])
				}
				delivered := "image: " + f.ref() + " is on the target's engine"
				if c.remote {
					delivered = "image: " + f.ref() + " loaded on docker --context default"
				}
				if at, secrets := lineAt(res, delivered), lineAt(res, "secrets: put into the volume cactusfs-config"); at < 0 || secrets < 0 || at < secrets || at > lineAt(res, "ready: ") {
					t.Errorf("image_deliver said %q at line %d, the secrets were put at %d:\n%s", delivered, at, secrets, res.stdout)
				}

				// The node: RESOLVED_IMAGE, never pulled.
				run := runOf(t, res)
				holds(t, run, []string{"--pull", "never"})
				if !slices.Equal(run[len(run)-2:], []string{f.ref(), "server"}) {
					t.Errorf("the node does not run %s server: %q", f.ref(), run[len(run)-2:])
				}
				noPullCommand(t, res)
			})
		}
	})
}

// TestDockerFromSourceDevInit: with --from-source, dev-init runs in
// RESOLVED_NATIVE_IMAGE - the image of this tree image_make built, on the
// engine here and of this machine's architecture, so built once - and the
// node runs the same image; the binaries of make image-binaries are not
// there, and not needed.
func TestDockerFromSourceDevInit(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		r = secretsRig(t, r)
		remove(t, filepath.Join(r.repo, "dist"))
		f := newImgFix(imgCommit)
		r.rules(append(append(gitRules(""), madeHere("docker", f)...), engineState{e: dockerEngines[0], devInit: devInitOutput(), ready: true}.rules(t)...)...)
		res := dockerRun(r, dockerEngines[0], "KMS=local\n", "--install", "--from-source")
		res.want(t, 0, "image: built from "+r.repo, "dev-init runs in "+f.ref()+", the image of this run", "ready: cactusfs runs "+f.ref())
		if b := builds(res); len(b) != 1 || slices.Contains(b[0].Args, "from-binaries") || valueOf(b[0], "--platform") != "linux/"+runtime.GOARCH {
			t.Fatalf("the builds are %v", b)
		}
		creates := callsOf(res, "docker", "create", "--name", "cactusfs-dev-init")
		if len(creates) != 1 || !containsRun(creates[0].Args, []string{"--entrypoint", "/cactusfs", f.ref(), "dev-init"}) ||
			!containsRun(creates[0].Args, []string{"--platform", "linux/" + runtime.GOARCH}) {
			t.Fatalf("dev-init does not run in %s: %v", f.ref(), creates)
		}
		in := callsOf(res, "docker", "cp", "-a", "-", "cactusfs-dev-init:/tmp")
		if len(in) != 1 {
			t.Fatalf("dev-init's input: %v", in)
		}
		before(t, steps(res), "build", "tag "+f.ref(), "version", "dev-init", "put cactusfs-volumes:/var/lib/cactusfs/config", "run "+f.ref())
		if run := runOf(t, res); run[len(run)-2] != f.ref() {
			t.Errorf("the node does not run %s: %q", f.ref(), run)
		}
	})
}

// TestPodmanVolumeRoot: on Podman a secrets' volume whose root is not owned
// by the nodes' user - made by an earlier run, whose helper's copy left it
// root:root 0700 - is refused with the way back, and nothing is put into it
// or run; on Docker the root is not judged (cp -a's "." entry, or the
// copy-up, gives it its owner).
func TestPodmanVolumeRoot(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range []dockerEngine{dockerEngines[0], dockerEngines[2]} {
			base := dockerInstalled(t, r, e, imageEnv)
			state := base.stateOf(e.targetID)
			t.Run(e.target, func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				s := installedState(t, e, state, nodeImage)
				keys := withTarget(t, state)
				r.rules(append([]rule{{Tool: e.tool, Contains: []string{"cp", "cactusfs-volumes:/var/lib/cactusfs/config", "-"}, Stdout: tarOwned(t, "config", keys["config"], 0)}}, s.rules(t)...)...)
				res := dockerRun(r, e, imageEnv, "--install")
				if e.tool != "podman" {
					res.want(t, 0, "ready: cactusfs runs "+nodeImage)
					return
				}
				res.want(t, 2, "the root of the volume cactusfs-config is owned by the user 0, not by the nodes' user 65532",
					"deploy/deploy.sh --uninstall, then podman volume rm cactusfs-secrets cactusfs-config, then deploy/deploy.sh --install")
				for _, words := range [][]string{{"cp", "-a"}, {"volume", "create"}, {"stop"}, {"rename"}} {
					if c := callsOf(res, e.tool, words...); len(c) != 0 {
						t.Errorf("a refused install ran %q", c[0].Args)
					}
				}
				if runs := slices.DeleteFunc(callsOf(res, e.tool, "run"), func(c call) bool { return isVersionCheck(sub(c)) }); len(runs) != 0 {
					t.Errorf("a refused install ran %q", runs[0].Args)
				}
			})
		}
	})
}
