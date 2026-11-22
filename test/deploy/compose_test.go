//go:build deployscript

package deploy_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// A cluster on a Docker engine (PLAN 10.8, deploy/lib/compose.sh): Docker
// Desktop, Docker CE here or on CE_HOST and Podman, through
// deploy/compose/compose.yaml with a generated override - install,
// upgrade one service at a time, status, uninstall and purge, as the
// commands the fakes record - keeping 10.3's assertions for its copies of
// the secrets: made once, compared, never on a command line, in an
// environment or in any output, refusals exit 2 with nothing changed.

const (
	// clusterEnv is the env file of a cluster on the local key file.
	clusterEnv = "KMS=local\nDEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\nIMAGE_SOURCE=image\nIMAGE=" + nodeImage + "\n"
	// ceHome is the home directory of the ssh user on ceHost (ce_test.go),
	// as the fake ssh says it.
	ceHome = "/home/deploy"
	// ceDir is the deployment's compose directory on ceHost.
	ceDir = ceHome + "/.cactusfs-deploy/ssh-deploy_server.example.test/cactusfs/compose"
)

// composeNodes are compose.yaml's nodes.
var composeNodes = []string{"node1", "node2", "node3", "node4"}

// clusterState is what a fake engine holds of a cluster and answers.
type clusterState struct {
	e dockerEngine
	// ceHost: the engine is the server's, reached over ssh.
	ceHost bool
	// ctrs are the deployment's containers as the listing by label says
	// them ("name|target|service|state|image").
	ctrs []string
	// project are the containers of the compose project of the name
	// ("name|label of the deployment's name").
	project []string
	// vols are the volumes labelled with the name ("name target").
	vols []string
	// byName are the volumes the listing by compose's names finds.
	byName []string
	// running is the listing of running containers ("name|ports|label").
	running []string
	// keys are what the config volume holds (nil: it is not there).
	keys map[string]map[string]string
	// devInit is what dev-init writes.
	devInit map[string]string
	// notReady are the admin ports whose /readyz does not answer 200.
	notReady []string
	// leader is what the admin API names as the leader ("": it does not
	// answer).
	leader string
	// inUse are the URLs at which a probe of a port is answered.
	inUse []string
	// endpoint is what the Docker context's endpoint is.
	endpoint string
	// noCompose: compose does not answer.
	noCompose bool
	// noEngine: compose answers "version" and cannot reach the engine
	// ("ls" fails): Podman's docker-compose provider without
	// podman.socket, or a Docker that is not running.
	noEngine bool
	// composeVersion is what "compose version" prints.
	composeVersion string
	// enforcing: getenforce says Enforcing.
	enforcing bool
	// remoteLeft: the server's directory of the deployment is still there
	// after its rm (CE_HOST).
	remoteLeft bool
	// remoteSecrets are compose's secret files on the server (nil: their
	// directory is not there until the run makes it).
	remoteSecrets map[string]string
	// ceInfoID is the ID the docker compose runs with on the server says
	// (empty: the engine's own).
	ceInfoID string
	// notReadyLater are the admin ports whose /readyz stops answering 200
	// once the leader was asked: a node that fails on its new image.
	notReadyLater []string
	// notReadyUntil are the nodes whose /readyz does not answer 200 until
	// compose has made them again ("up ... <node>"): failed, stopped or
	// without a container.
	notReadyUntil []string
	// upFails is the node whose "up" fails.
	upFails string
	// podmanRemote is the default connection of a Podman that is a remote
	// service (empty: Podman is local).
	podmanRemote string
	// purged: the listings by label change after each deletion.
	purged bool
}

// adminPort is the admin port of a node with the default S3_PORT.
func adminPort(node string) string {
	return map[string]string{"node1": "9001", "node2": "9011", "node3": "9021", "node4": "9031"}[node]
}

// labelOf is the template of a container's label in a listing, by engine.
func labelOf(tool, key string) string {
	if tool == "podman" {
		return `{{index .Labels "` + key + `"}}`
	}
	return `{{.Label "` + key + `"}}`
}

// runningCluster is the state of an installed cluster whose entry is
// state: its containers run image, its volumes are there, and the config
// volume holds what the entry holds.
func runningCluster(t *testing.T, e dockerEngine, state, image string) clusterState {
	t.Helper()
	s := clusterState{e: e, keys: map[string]map[string]string{"config": filesUnder(t, filepath.Join(state, "config"))}, devInit: devInitOutput()}
	s.ctrs = append(s.ctrs, "cactusfs-init-1|"+e.target+"|init|exited|"+image)
	for _, n := range composeNodes {
		s.ctrs = append(s.ctrs, "cactusfs-"+n+"-1|"+e.target+"|"+n+"|running|"+image)
		s.project = append(s.project, "cactusfs-"+n+"-1|cactusfs")
		for _, v := range []string{"meta", "data1", "data2"} {
			s.vols = append(s.vols, "cactusfs_"+n+"-"+v+" "+e.target)
			s.byName = append(s.byName, "cactusfs_"+n+"-"+v)
		}
	}
	s.vols = append(s.vols, "cactusfs-config "+e.target)
	s.running = []string{
		"cactusfs-node1-1|0.0.0.0:9000->9000/tcp, [::]:9000->9000/tcp, 127.0.0.1:9001->9001/tcp|cactusfs",
		"cactusfs-node2-1|0.0.0.0:9010->9000/tcp, 127.0.0.1:9011->9001/tcp|cactusfs",
		"cactusfs-node3-1|0.0.0.0:9020->9000/tcp, 127.0.0.1:9021->9001/tcp|cactusfs",
		"cactusfs-node4-1|0.0.0.0:9030->9000/tcp, 127.0.0.1:9031->9001/tcp|cactusfs",
	}
	s.leader = "node2"
	return s
}

func (s clusterState) rules(t *testing.T) []rule {
	t.Helper()
	tool := s.e.tool
	var rules []rule
	if s.ceHost {
		tool = "docker"
		// The server, over ssh: its home, the directory of the secret
		// files (absent until made), its curl's probes (refused: free)
		// and /readyz, SELinux; "test -e" of the deployment's directory
		// after the purge's rm says it is gone.
		gone := 1
		if s.remoteLeft {
			gone = 0
		}
		infoID := s.ceInfoID
		if infoID == "" {
			infoID = s.e.id
		}
		absent := 1
		if s.remoteSecrets != nil {
			absent = 0
			rules = append(rules, rule{Tool: "ssh", Contains: []string{"tar", "-c", ceDir}, Stdout: tarOf(t, "secrets", s.remoteSecrets)})
		}
		for _, p := range s.notReadyLater {
			rules = append(rules, rule{Tool: "ssh", Contains: []string{"curl", "https://localhost:" + p + "/readyz"}, After: []string{"-H", "@-"}, Exit: 22})
		}
		// Its host key (PLAN 10.7, ce_begin): ssh -G names the server and
		// the rig's known_hosts (made by clusterRun), whose lines for it
		// ssh-keygen -F finds and the install pins.
		rules = append(rules,
			rule{Tool: "ssh", Contains: []string{"-G"}, Stdout: "user deploy\nhostname server.example.test\nport 22\nuserknownhostsfile ~/.ssh/known_hosts\nglobalknownhostsfile none\n"},
			keygenRule("server.example.test", pinLines),
			rule{Tool: "ssh", Contains: []string{"printenv", "HOME"}, Stdout: ceHome + "\n"},
			rule{Tool: "ssh", Contains: []string{"docker", "info"}, Stdout: "Server:\n Containers: 5\n ID: " + infoID + "\n NodeID: x\n"},
			rule{Tool: "ssh", Contains: []string{"test", "-d", ceDir + "/secrets"}, After: []string{"mkdir", "-p", ceDir + "/secrets"}},
			rule{Tool: "ssh", Contains: []string{"test", "-d", ceDir + "/secrets"}, Exit: absent},
			rule{Tool: "ssh", Contains: []string{"test", "-e"}, Exit: gone},
			rule{Tool: "ssh", Contains: []string{"curl", "--max-time", "3"}, Exit: 7},
		)
		for _, p := range s.notReady {
			rules = append(rules, rule{Tool: "ssh", Contains: []string{"curl", "https://localhost:" + p + "/readyz"}, Exit: 22})
		}
		if s.enforcing {
			rules = append(rules, rule{Tool: "ssh", Contains: []string{"getenforce"}, Stdout: "Enforcing\n"})
		} else {
			rules = append(rules, rule{Tool: "ssh", Contains: []string{"getenforce"}, Exit: 127})
		}
		if s.leader != "" {
			rules = append(rules, rule{Tool: "ssh", Contains: []string{"curl", "-H", "@-"}, Stdout: `{"answered_by":"node1","leader":"` + s.leader + `","nodes":[{"id":"node1","leader":false}]}`})
		} else {
			rules = append(rules, rule{Tool: "ssh", Contains: []string{"curl", "-H", "@-"}, Exit: 22})
		}
	}
	if s.enforcing && !s.ceHost {
		rules = append(rules, rule{Tool: "getenforce", Stdout: "Enforcing\n"})
	}
	if s.podmanRemote != "" {
		rules = append(rules,
			rule{Tool: "podman", Contains: []string{"info", "{{.Host.ServiceIsRemote}}"}, Stdout: "true\n"},
			rule{Tool: "podman", Contains: []string{"system", "connection", "list"}, Stdout: "false ssh://core@127.0.0.1:50000/run/podman.sock\ntrue " + s.podmanRemote + "\n"})
	}
	for _, p := range s.notReadyLater {
		rules = append(rules, rule{Tool: "curl", Contains: []string{"https://localhost:" + p + "/readyz"}, After: []string{"-H", "@-"}, Stdout: "503"})
	}
	for _, n := range s.notReadyUntil {
		url := "https://localhost:" + adminPort(n) + "/readyz"
		rules = append(rules,
			rule{Tool: "curl", Contains: []string{url}, Until: []string{"up", n}, Stdout: "503"},
			rule{Tool: "ssh", Contains: []string{"curl", url}, Until: []string{"up", n}, Exit: 22})
	}
	if s.upFails != "" {
		for _, tl := range []string{"docker", "podman", "ssh"} {
			rules = append(rules, rule{Tool: tl, Contains: []string{"up", s.upFails}, Stderr: "compose: cannot make " + s.upFails + "\n", Exit: 1})
		}
	}
	if s.noEngine {
		const down = "Cannot connect to the Docker daemon at unix:///run/podman/podman.sock. Is the docker daemon running?\n"
		rules = append(rules,
			rule{Tool: tool, Contains: []string{"compose", "ls"}, Stderr: down, Exit: 1},
			rule{Tool: "ssh", Contains: []string{"compose", "ls"}, Stderr: down, Exit: 1})
	}
	if s.composeVersion != "" {
		rules = append(rules, rule{Tool: tool, Contains: []string{"compose", "version"}, Stdout: s.composeVersion})
	}
	if s.noCompose {
		rules = append(rules, rule{Tool: tool, Contains: []string{"compose", "version"}, Stderr: "unknown command compose\n", Exit: 1})
		rules = append(rules, rule{Tool: "ssh", Contains: []string{"compose", "version"}, Stderr: "unknown command compose\n", Exit: 1})
	}
	endpoint := s.endpoint
	if endpoint == "" {
		endpoint = "unix:///var/run/docker.sock"
	}
	rules = append(rules, rule{Tool: tool, Contains: []string{"context", "inspect"}, Stdout: endpoint + "\n"})
	// The images given (nodeImage, newImage) are on the engine (PLAN 10.4's
	// image_check asks), as for a standalone node (engineState.rules).
	rules = append(rules, givenHere(tool, newImgFix(imgCommit), "", nodeImage)...)
	rules = append(rules, givenHere(tool, newImgFix(imgCommit), "", newImage)...)
	ctrFmt := "{{.Names}}|" + labelOf(tool, "io.cactusfs.deploy.target") + "|" + labelOf(tool, "com.docker.compose.service") + "|{{.State}}|{{.Image}}"
	statusFmt := labelOf(tool, "com.docker.compose.service") + "|{{.Names}}|{{.State}}|{{.Image}}|" + labelOf(tool, "io.cactusfs.deploy.target") + "|" + labelOf(tool, "org.opencontainers.image.version")
	var ctrs, status string
	for _, c := range s.ctrs {
		ctrs += c + "\n"
		f := strings.Split(c, "|")
		status += f[2] + "|" + f[0] + "|" + f[3] + "|" + f[4] + "|" + f[1] + "|0.7.0-1\n"
	}
	rules = append(rules,
		rule{Tool: tool, Contains: []string{"ps", "-a", byLabel, ctrFmt}, Stdout: ctrs},
		rule{Tool: tool, Contains: []string{"ps", "-a", byLabel, statusFmt}, Stdout: status},
		rule{Tool: tool, Contains: []string{"ps", "-a", "label=com.docker.compose.project=cactusfs"}, Stdout: strings.Join(s.project, "\n")},
		rule{Tool: tool, Contains: []string{"ps", "--format", "{{.Names}}|{{.Ports}}|" + labelOf(tool, "io.cactusfs.deploy.name")}, Stdout: strings.Join(s.running, "\n")},
		rule{Tool: tool, Contains: []string{"network", "ls"}, Stdout: "cactusfs_default\n"},
	)
	names, targets := "", ""
	for _, v := range s.vols {
		names += strings.Fields(v)[0] + "\n"
		targets += v + "\n"
	}
	if s.purged {
		rules = append(rules,
			rule{Tool: tool, Contains: []string{"volume", "ls", byLabel}, After: []string{"volume", "rm", "cactusfs-config"}},
			rule{Tool: tool, Contains: []string{"volume", "ls", byLabel}, After: []string{"volume", "rm", "cactusfs_node1-meta"}, Stdout: "cactusfs-config\n"},
			rule{Tool: tool, Contains: []string{"volume", "inspect", targetFormat}, After: []string{"volume", "rm", "cactusfs_node1-meta"}, Stdout: "cactusfs-config " + s.e.target + "\n"})
	}
	rules = append(rules,
		rule{Tool: tool, Contains: []string{"volume", "ls", byLabel}, Stdout: names},
		rule{Tool: tool, Contains: []string{"volume", "inspect", targetFormat}, Stdout: targets},
		rule{Tool: tool, Contains: []string{"volume", "ls", "name=cactusfs_"}, Stdout: strings.Join(s.byName, "\n")},
	)
	for _, u := range s.inUse {
		rules = append(rules, rule{Tool: "curl", Contains: []string{u}, Stdout: "1"})
	}
	for _, p := range s.notReady {
		rules = append(rules, rule{Tool: "curl", Contains: []string{"https://localhost:" + p + "/readyz"}, Stdout: "503"})
	}
	if s.leader != "" {
		rules = append(rules, rule{Tool: "curl", Contains: []string{"-H", "@-"}, Stdout: `{"answered_by":"node1","leader":"` + s.leader + `","nodes":[{"id":"node1","leader":false}]}`})
	} else {
		rules = append(rules, rule{Tool: "curl", Contains: []string{"-H", "@-"}, Exit: 22})
	}
	rules = append(rules, rule{Tool: "curl", Contains: []string{"--connect-to"}, Stdout: "200"})
	return append(rules, engineRules(t, tool, s.e.id, s.devInit, s.keys)...)
}

// clusterRun runs the script with the env file and the arguments on e's
// engine, under the guard of guardedRun, with CACTUSFS_* variables in the
// script's own environment that must reach no tool.
func clusterRun(r *rig, e dockerEngine, env string, args ...string) result {
	r.t.Helper()
	if strings.Contains(env, "CE_HOST=") {
		// The known_hosts ssh -G names (clusterState.rules).
		put(r.t, filepath.Join(r.home, ".ssh", "known_hosts"), "a file ssh-keygen -F is pointed at\n")
	}
	argv := append([]string{"--env", r.envFile("site.env", env), "--docker", e.target}, args...)
	extra := append(slices.Clone(e.env), shellCactusfs...)
	return r.guardedRun(argv, extra...)
}

// shellCactusfs are variables of the operator's shell that compose.yaml
// would interpolate were they handed to it.
var shellCactusfs = []string{"CACTUSFS_IMAGE=shell.example.test/evil:1", "CACTUSFS_PORT_BASE_1=shell-port-base", "CACTUSFS_LOG_LEVEL=shell-level", "CACTUSFS_KMS_PASSPHRASE=from-the-shell"}

// noShellCactusfs fails the test when a CACTUSFS_* variable reached any
// tool, or any value of shellCactusfs is in an argument.
func noShellCactusfs(t *testing.T, res result) {
	t.Helper()
	if len(res.calls) == 0 {
		t.Fatal("no tool was run, so this proves nothing")
	}
	for _, c := range res.calls {
		for k := range c.Env {
			if strings.HasPrefix(k, "CACTUSFS_") {
				t.Errorf("%s reached %s %q", k, c.Tool, c.Args[:min(len(c.Args), 4)])
			}
		}
		for _, a := range c.Args {
			for _, v := range shellCactusfs {
				if strings.Contains(a, strings.SplitN(v, "=", 2)[1]) {
					t.Errorf("%s of the shell is in an argument of %s: %q", v, c.Tool, a)
				}
			}
		}
	}
}

// clusterInstalled is a rig of the shell of r on which a first install of
// the env file has run on e's engine.
func clusterInstalled(t *testing.T, r *rig, e dockerEngine, env string) *rig {
	t.Helper()
	base := secretsRig(t, r.fresh(t))
	base.rules(clusterState{e: e, devInit: devInitOutput()}.rules(t)...)
	clusterRun(base, e, env, "--install").want(t, 0, "ready: the cluster cactusfs runs")
	return base
}

// composeCalls are the compose commands of a run - their words from
// "compose" on, wherever compose ran.
func composeCalls(res result) [][]string {
	var out [][]string
	for _, c := range res.calls {
		if i := slices.Index(c.Args, "compose"); i >= 0 && (c.Tool == "docker" || c.Tool == "podman" || c.Tool == "ssh") {
			out = append(out, c.Args[i:])
		}
	}
	return out
}

// interpolated is every variable compose.yaml interpolates.
func interpolated(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(string(repoFile(t, "deploy/compose/compose.yaml")), "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		for _, m := range regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)`).FindAllStringSubmatch(line, -1) {
			if !slices.Contains(out, m[1]) {
				out = append(out, m[1])
			}
		}
	}
	if len(out) < 10 {
		t.Fatalf("compose.yaml interpolates %d variables: %q", len(out), out)
	}
	return out
}

// TestComposeInstall: a first install of a cluster on each engine - the
// engine, compose and the ports asked before anything changes; the
// secrets made once by dev-init for node1 to node4, into the config
// volume (labelled, filled before anything starts) and compose's secret
// files (a 0700 directory of the entry, files 0444 equal to the entry's);
// compose.yaml copied unchanged, the override and compose.env written;
// compose run "up --no-build" under env -i from those files, with no
// CACTUSFS_* variable of the shell reaching any tool; then every node
// ready. No secret is on any command line, in any environment or in the
// output, a traced run's included.
func TestComposeInstall(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		for _, e := range dockerEngines {
			t.Run(e.target, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				r.rules(clusterState{e: e, devInit: devInitOutput()}.rules(t)...)
				r.trace = true
				env := clusterEnv + "TLS_HOSTS=s3.example.test,192.0.2.10\nTLS_VALIDITY=720h\nCACTUSFS_LOG_LEVEL=debug\nCACTUSFS_S3_DOMAIN=s3.example.test\n"
				res := clusterRun(r, e, env, "--install")
				r.trace = false
				state := r.stateOf(e.targetID)
				res.want(t, 0, "ready: the cluster cactusfs runs "+nodeImage, "S3 of node4: https://<any address of this machine>:9030",
					"admin API and console: https://127.0.0.1:9001", "the CA to trust: "+state+"/config/s3-ca.pem",
					"the console user admin with the password admin (ADR-0014)")
				noSecretAnywhere(t, res, state)
				noShellCactusfs(t, res)
				// No .curlrc is read: -q first in every curl.
				for _, c := range callsOf(res, "curl") {
					if len(c.Args) == 0 || c.Args[0] != "-q" {
						t.Errorf("curl without -q first: %q", c.Args)
					}
				}

				// The order: every question first, then dev-init, the config
				// volume and its put, compose up, and /readyz of each node.
				var order []string
				for _, c := range res.calls {
					s := sub(c)
					switch {
					case mutating(c) && !slices.Contains(s, "compose") && order == nil:
						t.Errorf("a change before anything was asked: %s %q", c.Tool, c.Args)
					case c.Tool == "curl" && slices.Contains(c.Args, "%{num_connects}"):
						order = append(order, "probe "+c.Args[len(c.Args)-1])
					case len(s) > 1 && s[0] == "volume" && s[1] == "create":
						order = append(order, "volume "+s[len(s)-1])
						if !containsAll(s, []string{"io.cactusfs.deploy.name=cactusfs", "io.cactusfs.deploy.target=" + e.target}) {
							t.Errorf("a volume without the deployment's labels: %q", s)
						}
					case len(s) > 0 && s[0] == "start":
						order = append(order, "dev-init")
						if !containsRun(c.Args, []string{"--nodes", "node1,node2,node3,node4"}) && !callHas(res, "create", "node1,node2,node3,node4") {
							t.Errorf("dev-init is not made for node1 to node4")
						}
					case len(s) > 2 && s[0] == "cp" && s[1] == "-a" && strings.HasPrefix(s[3], "cactusfs-volumes:"):
						order = append(order, "put "+s[3])
					case slices.Contains(s, "compose") && slices.Contains(s, "up"):
						order = append(order, "up")
					case c.Tool == "curl" && slices.Contains(c.Args, "--connect-to"):
						order = append(order, "readyz "+c.Args[len(c.Args)-1])
					}
				}
				before(t, order, "probe http://127.0.0.1:9000/", "probe http://127.0.0.1:9031/", "dev-init",
					"volume cactusfs-config", "put cactusfs-volumes:/var/lib/cactusfs/config", "up",
					"readyz https://s3.example.test:9001/readyz", "readyz https://s3.example.test:9011/readyz",
					"readyz https://s3.example.test:9021/readyz", "readyz https://s3.example.test:9031/readyz")
				if slices.Contains(order, "volume cactusfs-secrets") {
					t.Error("a cluster made the standalone volume cactusfs-secrets")
				}

				// compose: up --no-build of the project, from the entry's files.
				dir := filepath.Join(state, "compose")
				ups := composeCalls(res)
				want := []string{"compose", "--project-name", "cactusfs", "-f", dir + "/compose.yaml", "-f", dir + "/override.yaml", "--env-file", dir + "/compose.env", "up", "-d", "--no-build"}
				if !slices.ContainsFunc(ups, func(a []string) bool { return slices.Equal(a, want) }) {
					t.Errorf("compose was not run as %q: %q", want, ups)
				}
				for _, c := range res.calls {
					if !slices.Contains(c.Args, "compose") {
						continue
					}
					for k := range c.Env {
						if !slices.Contains([]string{"PATH", "HOME", "LC_ALL", "XDG_RUNTIME_DIR"}, k) {
							t.Errorf("compose was given %s", k)
						}
					}
					if e.tool == "podman" && c.Tool != "podman" {
						t.Errorf("compose of %s for podman", c.Tool)
					}
				}

				// The files of the entry.
				if got := fileOf(t, filepath.Join(dir, "compose.yaml")); got != string(repoFile(t, "deploy/compose/compose.yaml")) {
					t.Error("compose.yaml of the entry is not the repository's")
				}
				override := fileOf(t, filepath.Join(dir, "override.yaml"))
				policy := "unless-stopped"
				if e.tool == "podman" {
					policy = "always"
				}
				for _, w := range []string{
					"  init:\n    image: '" + nodeImage + "'\n    pull_policy: never\n",
					"      io.cactusfs.deploy.name: 'cactusfs'\n      io.cactusfs.deploy.target: '" + e.target + "'\n",
					"      - '--validity'\n      - '720h'\n      - '--hosts'\n      - 's3.example.test,192.0.2.10'\n",
					"      - '--nodes'\n      - 'node1,node2,node3,node4'\n",
					"  node4:\n    image: '" + nodeImage + "'\n    pull_policy: never\n    restart: " + policy + "\n",
					"      CACTUSFS_DRIVE_LABELS: 'cactusfs_node3-data1,cactusfs_node3-data2'\n",
					"      CACTUSFS_UPGRADE_MODE: 'off'\n",
					"      CACTUSFS_S3_DOMAIN: 's3.example.test'\n",
					"  config:\n    external: true\n    name: 'cactusfs-config'\n",
					"  node2-data2:\n    labels:\n",
					"networks:\n  default:\n    labels:\n",
				} {
					if !strings.Contains(override, w) {
						t.Errorf("the override does not hold %q:\n%s", w, override)
					}
				}
				if strings.Contains(override, "CACTUSFS_LOG_LEVEL") || strings.Contains(override, "build:") {
					t.Errorf("the override holds what compose.env gives, or a build:\n%s", override)
				}
				envFile := fileOf(t, filepath.Join(dir, "compose.env"))
				vars := interpolated(t)
				for _, v := range vars {
					if !regexp.MustCompile(`(?m)^` + v + `=`).MatchString(envFile) {
						t.Errorf("compose.env does not give %s:\n%s", v, envFile)
					}
				}
				for _, w := range []string{"CACTUSFS_IMAGE=" + nodeImage + "\n", "CACTUSFS_PORT_BASE_1=90\n", "CACTUSFS_PORT_BASE_4=90\n", "CACTUSFS_LOG_LEVEL=debug\n", "CACTUSFS_REGION=\n"} {
					if !strings.Contains(envFile, w) {
						t.Errorf("compose.env does not say %q:\n%s", w, envFile)
					}
				}
				if n := strings.Count(envFile, "\n") - 1; n != len(vars) {
					t.Errorf("compose.env gives %d values, compose.yaml interpolates %d", n, len(vars))
				}

				// compose's secret files: a copy of the entry's secrets, the
				// directory 0700 and the files 0444; the rest of the entry
				// 0700 and 0600; the record says the mode.
				secrets := filepath.Join(dir, "secrets")
				secretFilesAre(t, secrets, filepath.Join(state, "secrets"))
				modesAre(t, filepath.Join(state, "config"))
				for name := range filesUnder(t, filepath.Join(state, "secrets")) {
					if fi, err := os.Stat(filepath.Join(state, "secrets", name)); err != nil || fi.Mode().Perm() != 0o600 {
						t.Errorf("%s/secrets/%s is not 0600: %v", state, name, fi.Mode())
					}
				}
				if got := fileOf(t, filepath.Join(state, "target")); !strings.HasSuffix(got, "kms=local\nmode=cluster\n") {
					t.Errorf("the record does not say the mode: %q", got)
				}
			})
		}
	})
}

// callHas is true when a call of the engine holds every word.
func callHas(res result, words ...string) bool {
	for _, c := range res.calls {
		if containsAll(c.Args, words) {
			return true
		}
	}
	return false
}

// secretFilesAre fails the test unless dir holds exactly the files of
// from, each equal, of mode 0444, in a directory of mode 0700.
func secretFilesAre(t *testing.T, dir, from string) {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("%s has mode %o, want 700", dir, info.Mode().Perm())
	}
	got, want := filesUnder(t, dir), filesUnder(t, from)
	if len(want) < 3 {
		t.Fatalf("%s holds %d files", from, len(want))
	}
	if len(got) != len(want) {
		t.Errorf("%s holds %d files, %s %d", dir, len(got), from, len(want))
	}
	for name, data := range want {
		if got[name] != data {
			t.Errorf("%s/%s is not %s/%s", dir, name, from, name)
		}
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o444 {
			t.Errorf("%s/%s has mode %o, want 444", dir, name, fi.Mode().Perm())
		}
	}
}

// TestComposeMadeOnce: a second install over a running cluster makes
// nothing again - no dev-init, no volume, no put, no secret file written -
// and the copies of the secrets stay as they were.
func TestComposeMadeOnce(t *testing.T) {
	t.Parallel()
	e := dockerEngines[0]
	eachShell(t, func(t *testing.T, r *rig) {
		r = clusterInstalled(t, r, e, clusterEnv)
		state := r.stateOf(e.targetID)
		before := tree(t, filepath.Join(state, "compose", "secrets"))
		beforeState := tree(t, filepath.Join(state, "secrets"))
		r.rules(runningCluster(t, e, state, nodeImage).rules(t)...)
		res := clusterRun(r, e, clusterEnv, "--install")
		res.want(t, 0, "ready: the cluster cactusfs runs")
		noSecretAnywhere(t, res, state)
		for _, c := range res.calls {
			s := sub(c)
			if len(s) > 0 && (s[0] == "start" || (s[0] == "cp" && slices.Contains(s, "-a")) || (s[0] == "volume" && s[1] == "create")) {
				t.Errorf("a second install made or put something: %q", c.Args)
			}
		}
		if after := tree(t, filepath.Join(state, "compose", "secrets")); !equalTrees(before, after) {
			t.Error("a second install changed compose's secret files")
		}
		if after := tree(t, filepath.Join(state, "secrets")); !equalTrees(beforeState, after) {
			t.Error("a second install changed the entry's secrets")
		}
	})
}

// TestComposeCopiesCompared: compose's secret files and the config volume
// are copies of the entry's secrets, compared as 10.3 compares them: one
// that differs is refused (exit 2, nothing changed), naming both; a file
// missing from compose's copy is put back alone; one missing from the
// entry is copied back from compose's, said.
func TestComposeCopiesCompared(t *testing.T) {
	t.Parallel()
	e := dockerEngines[0]
	eachShell(t, func(t *testing.T, base *rig) {
		base = clusterInstalled(t, base, e, clusterEnv)
		for _, c := range []struct {
			name   string
			change func(t *testing.T, state string, s *clusterState)
			code   int
			want   string
			check  func(t *testing.T, state string)
		}{
			{"a secret file that differs", func(t *testing.T, state string, _ *clusterState) {
				p := filepath.Join(state, "compose", "secrets", "root-secret-key")
				remove(t, p)
				put(t, p, "another secret key of somebody else")
			}, 2, "STATE/secrets/root-secret-key and root-secret-key in compose's secret files STATE/compose/secrets differ", nil},
			{"kms.json of the config volume that differs", func(_ *testing.T, _ string, s *clusterState) {
				s.keys["config"]["kms.json"] = "another key file\n"
			}, 2, "STATE/config/kms.json and kms.json in the volume cactusfs-config differ", nil},
			{"a secret file missing", func(t *testing.T, state string, _ *clusterState) {
				remove(t, filepath.Join(state, "compose", "secrets", "kms-passphrase"))
			}, 0, "put into compose's secret files STATE/compose/secrets what it lacked of STATE/secrets: kms-passphrase", func(t *testing.T, state string) {
				secretFilesAre(t, filepath.Join(state, "compose", "secrets"), filepath.Join(state, "secrets"))
			}},
			{"the entry's copy missing", func(t *testing.T, state string, _ *clusterState) {
				remove(t, filepath.Join(state, "secrets", "root-access-key"))
			}, 0, "the local copy STATE/secrets/root-access-key was missing: copied back from compose's secret files STATE/compose/secrets", func(t *testing.T, state string) {
				secretFilesAre(t, filepath.Join(state, "compose", "secrets"), filepath.Join(state, "secrets"))
			}},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				state := r.stateOf(e.targetID)
				s := runningCluster(t, e, state, nodeImage)
				c.change(t, state, &s)
				r.rules(s.rules(t)...)
				res := clusterRun(r, e, clusterEnv, "--install")
				res.stdout = strings.ReplaceAll(res.stdout, state, "STATE")
				res.stderr = strings.ReplaceAll(res.stderr, state, "STATE")
				res.want(t, c.code, c.want)
				if c.code == 2 {
					// The copies were read back through a helper, made and
					// removed - after PLAN 10.4 made the image, whose
					// version line it read (run --rm <ID> version): an image
					// made is not a change of the deployment; nothing was
					// put, made or run.
					for _, call := range res.calls {
						s := sub(call)
						versionLine := len(s) > 2 && s[0] == "run" && s[1] == "--rm" && s[len(s)-1] == "version"
						if mutating(call) && !versionLine && !slices.Contains(s, "cactusfs-volumes") {
							t.Errorf("a refused run changed the engine: %q", call.Args)
						}
					}
					// Only its two questions: "version" and "ls".
					for _, u := range composeCalls(res) {
						if !slices.Equal(u, []string{"compose", "version"}) && !slices.Equal(u, []string{"compose", "ls"}) {
							t.Errorf("compose ran after a refusal: %q", u)
						}
					}
				}
				if c.check != nil {
					c.check(t, state)
				}
			})
		}
	})
}

// TestComposeEngineUnreachable: compose answers "version" but cannot reach
// the engine ("compose ls" fails) - Podman's docker-compose provider
// without podman.socket, Docker not running, here or on CE_HOST: refused
// (exit 2) before anything changes, the socket named for Podman. With
// podman-compose as Podman's provider, which has no "ls", it is not asked.
func TestComposeEngineUnreachable(t *testing.T) {
	t.Parallel()
	ce := dockerEngine{tool: "docker", target: "ce", id: engineID, targetID: "ssh-deploy_server.example.test"}
	cases := []struct {
		name string
		e    dockerEngine
		env  string
		want string
	}{
		{"docker desktop", dockerEngines[0], "", "docker compose cannot reach the engine of the context default (compose ls failed"},
		{"podman", dockerEngines[2], "", "systemctl --user enable --now podman.socket (as root: systemctl enable --now podman.socket"},
		{"ce host", ce, "CE_HOST=" + ceHost + "\n", "docker compose on " + ceHost + " cannot reach its engine (compose ls failed"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				r.rules(clusterState{e: c.e, ceHost: c.env != "", noEngine: true, devInit: devInitOutput()}.rules(t)...)
				res := clusterRun(r, c.e, clusterEnv+c.env, "--install")
				res.want(t, 2, c.want, "nothing was changed")
				noChange(t, res)
				for _, u := range composeCalls(res) {
					if !slices.Equal(u, []string{"compose", "version"}) && !slices.Equal(u, []string{"compose", "ls"}) {
						t.Errorf("compose ran %q", u)
					}
				}
				if len(composeCalls(res)) != 2 {
					t.Errorf("compose was not asked its version and then ls: %q", composeCalls(res))
				}
				if _, err := os.Stat(r.stateDir()); err == nil {
					t.Errorf("a refused run made %s", r.stateDir())
				}
			})
		}
		t.Run("podman-compose", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			e := dockerEngines[2]
			r.rules(clusterState{e: e, noEngine: true, composeVersion: "podman-compose version 1.2.0\npodman version 5.2.2\n", devInit: devInitOutput()}.rules(t)...)
			res := clusterRun(r, e, clusterEnv, "--install")
			res.want(t, 0, "ready: the cluster cactusfs runs")
			if n := len(callsOf(res, "podman", "compose", "ls")); n != 0 {
				t.Errorf("podman-compose was asked ls %d times", n)
			}
		})
	})
}

// TestComposeRefused: what cluster mode refuses before anything changes -
// no container, volume or file is made, no STATE_DIR entry, compose is
// not run.
func TestComposeRefused(t *testing.T) {
	t.Parallel()
	e := dockerEngines[0]
	cases := []struct {
		name   string
		env    string
		change func(s *clusterState)
		want   string
	}{
		{"BIND", "BIND=0.0.0.0\n", nil, "BIND=0.0.0.0: cannot be set in cluster mode"},
		{"ADMIN_BIND", "ADMIN_BIND=0.0.0.0\n", nil, "ADMIN_BIND=0.0.0.0: cannot be set in cluster mode"},
		{"DRIVES", "DRIVES=2\n", nil, "DRIVES=2: cannot be set in cluster mode"},
		{"S3_PORT not of hundreds", "S3_PORT=9050\nADMIN_PORT=9051\n", nil, "S3_PORT=9050: must end in 00 in cluster mode"},
		{"ADMIN_PORT apart", "S3_PORT=19000\nADMIN_PORT=19500\n", nil, "ADMIN_PORT=19500: must be S3_PORT + 1 (19001)"},
		{"node certificates of one's own", "NODE_CERT_DIR=/srv/certs\n", nil, "NODE_CERT_DIR is not put into the config volume of a compose cluster yet"},
		{"a node's zone", "CACTUSFS_TOPOLOGY_ZONE=zone-x\n", nil, "CACTUSFS_TOPOLOGY_ZONE is a setting of each node"},
		{"a node's rack", "CACTUSFS_TOPOLOGY_RACK=r1\n", nil, "CACTUSFS_TOPOLOGY_RACK is a setting of each node"},
		{"an engine elsewhere", "", func(s *clusterState) { s.endpoint = "tcp://engine.example.test:2376" }, "a DOCKER_HOST engine elsewhere is refused for cluster mode"},
		{"no compose", "", func(s *clusterState) { s.noCompose = true }, "docker compose does not answer"},
		{"a container of another target", "", func(s *clusterState) { s.ctrs = []string{"cactusfs-node1-1|ce|node1|running|x"} }, "is the deployment's on the target 'ce'"},
		{"a standalone node of the name", "", func(s *clusterState) { s.ctrs = []string{"cactusfs|dd||running|x"} }, "was not made by compose (a standalone node of cactusfs?)"},
		{"a compose project of the name not ours", "", func(s *clusterState) { s.project = []string{"cactusfs-web-1|"} }, "belongs to a compose project named cactusfs that the script did not make"},
		{"a standalone volume of the name", "", func(s *clusterState) { s.vols = []string{"cactusfs-meta dd"} }, "the volume cactusfs-meta carries the deployment's name and is not one of its compose cluster's"},
		{"a volume of compose's names unlabelled", "", func(s *clusterState) { s.byName = []string{"cactusfs_node2-data1"} }, "a volume named cactusfs_node2-data1 is there without the label"},
		{"data without a record", "", func(s *clusterState) {
			s.vols = []string{"cactusfs_node1-meta dd"}
			s.byName = []string{"cactusfs_node1-meta"}
		}, "say --adopt"},
		{"a port another container publishes", "", func(s *clusterState) { s.running = []string{"web|0.0.0.0:9020->80/tcp|"} }, "port 9020 is published by the container web"},
		{"a port something answers on", "", func(s *clusterState) { s.inUse = []string{"http://127.0.0.1:9031/"} }, "port 9031 is in use"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				s := clusterState{e: e, devInit: devInitOutput()}
				if c.change != nil {
					c.change(&s)
				}
				r.rules(s.rules(t)...)
				res := clusterRun(r, e, clusterEnv+c.env, "--install")
				res.want(t, 2, c.want)
				noChange(t, res)
				for _, u := range composeCalls(res) {
					if !slices.Equal(u, []string{"compose", "version"}) && !slices.Equal(u, []string{"compose", "ls"}) {
						t.Errorf("compose ran: %q", u)
					}
				}
				if _, err := os.Stat(r.stateDir()); err == nil {
					t.Errorf("a refused run made %s", r.stateDir())
				}
			})
		}
	})
}

// TestComposeModeIsKept: a deployment recorded standalone is not installed
// over as a cluster, nor a cluster removed as a standalone one.
func TestComposeModeIsKept(t *testing.T) {
	t.Parallel()
	e := dockerEngines[0]
	eachShell(t, func(t *testing.T, r *rig) {
		cluster := clusterInstalled(t, r, e, clusterEnv)
		cluster.rules(runningCluster(t, e, cluster.stateOf(e.targetID), nodeImage).rules(t)...)
		res := dockerRun(cluster, e, imageEnv, "--install")
		res.want(t, 2, "was made with DEPLOY_MODE=cluster, and the settings now say DEPLOY_MODE=standalone")
		noChange(t, res)
		res = dockerRun(cluster, e, imageEnv, "--uninstall", "--purge", "--purge-confirm=cactusfs")
		res.want(t, 2, "was made with DEPLOY_MODE=cluster")
		noChange(t, res)

		standalone := dockerInstalled(t, r, e, imageEnv)
		standalone.rules(installedState(t, e, standalone.stateOf(e.targetID), nodeImage).rules(t)...)
		res = clusterRun(standalone, e, clusterEnv, "--install")
		res.want(t, 2, "was made with DEPLOY_MODE=standalone, and the settings now say DEPLOY_MODE=cluster")
		noChange(t, res)

		// --status and --uninstall (with --purge too) in the other mode are
		// refused right after the settings are read, in both directions:
		// nothing is asked of the deployment's containers or volumes.
		for _, args := range [][]string{{"--status"}, {"--uninstall"}, {"--uninstall", "--purge", "--purge-confirm=cactusfs"}} {
			for _, c := range []struct {
				name string
				run  func() result
				want string
			}{
				{"standalone settings on a cluster", func() result { return dockerRun(cluster, e, imageEnv, args...) }, "was made with DEPLOY_MODE=cluster, and the settings now say DEPLOY_MODE=standalone"},
				{"cluster settings on a standalone node", func() result { return clusterRun(standalone, e, clusterEnv, args...) }, "was made with DEPLOY_MODE=standalone, and the settings now say DEPLOY_MODE=cluster"},
			} {
				res := c.run()
				res.want(t, 2, c.want)
				noChange(t, res)
				for _, call := range res.calls {
					if s := sub(call); len(s) > 0 && (s[0] == "ps" || s[0] == "volume" || s[0] == "container" || s[0] == "network" || s[0] == "compose") {
						t.Errorf("%s %q: the engine was asked %q", c.name, args, s)
					}
				}
			}
		}
	})
}

// TestComposeUpgrade: an install over a running cluster replaces one
// service at a time - node4, then the voters, the leader the admin API
// names last - each ready before the next, asking the admin API with the
// root credentials on curl's standard input; with no answer the voters go
// in order; the first node not ready within WAIT_TIMEOUT stops the run
// (exit 1) and the nodes after it are not touched.
func TestComposeUpgrade(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, base *rig) {
		for _, e := range dockerEngines {
			t.Run(e.target, func(t *testing.T) {
				t.Parallel()
				base := clusterInstalled(t, base, e, clusterEnv)
				newEnv := strings.Replace(clusterEnv, nodeImage, newImage, 1)
				for _, c := range []struct {
					name          string
					leader        string
					notReady      []string
					notReadyLater []string
					code          int
					order         []string
					want          string
				}{
					{"the leader last", "node2", nil, nil, 0, []string{"node4", "node1", "node3", "node2"}, "the admin API names node2 the leader; it goes last"},
					{"no leader named", "", nil, nil, 0, []string{"node4", "node1", "node2", "node3"}, "the voters go in order"},
					{"a node not ready", "node3", nil, []string{"9001"}, 1, []string{"node4", "node1"}, "upgrade stopped: node1 did not become ready within WAIT_TIMEOUT=1 s"},
					{"two nodes not ready before", "node3", []string{"9011", "9021"}, nil, 2, nil, "more than one node is not ready: node2 (/readyz of 127.0.0.1:9011 answered 503) node3 (/readyz of 127.0.0.1:9021 answered 503)"},
				} {
					t.Run(c.name, func(t *testing.T) {
						t.Parallel()
						r := clone(t, base)
						state := r.stateOf(e.targetID)
						s := runningCluster(t, e, state, nodeImage)
						s.leader = c.leader
						s.notReady = c.notReady
						s.notReadyLater = c.notReadyLater
						r.rules(s.rules(t)...)
						r.keepStdin()
						res := clusterRun(r, e, newEnv+"WAIT_TIMEOUT=1\n", "--install")
						res.want(t, c.code, c.want)
						noSecretAnywhere(t, res, state)
						var got []string
						for _, u := range composeCalls(res) {
							if slices.Contains(u, "up") {
								if !containsRun(u, []string{"up", "-d", "--no-build", "--no-deps"}) {
									t.Errorf("a service is made again without --no-build --no-deps: %q", u)
								}
								got = append(got, u[len(u)-1])
							}
						}
						if !slices.Equal(got, c.order) {
							t.Errorf("the services were made again in the order %q, want %q", got, c.order)
						}
						if c.code == 1 && !strings.Contains(res.stderr, "not touched after it, on their image: node2 node3") {
							t.Errorf("the failure does not name the nodes not touched:\n%s", res.stderr)
						}
						if c.code != 2 && !strings.Contains(fileOf(t, filepath.Join(state, "compose", "override.yaml")), "image: '"+newImage+"'") {
							t.Error("the override does not name the new image")
						}
						// The root credentials reach the admin API on curl's
						// standard input, as a bearer API key.
						for _, call := range callsOf(res, "curl", "-H", "@-") {
							data, err := os.ReadFile(filepath.Join(r.dir, stdinDir, call.Stdin))
							if err != nil {
								t.Fatal(err)
							}
							want := "Authorization: Bearer " + strings.TrimSpace(fileOf(t, filepath.Join(state, "secrets", "root-access-key"))) + ":" +
								strings.TrimSpace(fileOf(t, filepath.Join(state, "secrets", "root-secret-key"))) + "\n"
							if string(data) != want {
								t.Error("the admin API is not asked with the root credentials as its bearer key")
							}
						}
						if c.code == 2 {
							noChange(t, res)
							return
						}
						if len(callsOf(res, "curl", "-H", "@-")) == 0 {
							t.Error("the admin API was not asked for the leader")
						}
					})
				}
			})
		}
	})
}

// TestComposeStatus: --status names every node, its image, version and
// /readyz, and exits 1 when one is not ready; with nothing installed it
// says so and exits 0. It changes nothing.
func TestComposeStatus(t *testing.T) {
	t.Parallel()
	e := dockerEngines[0]
	eachShell(t, func(t *testing.T, base *rig) {
		base = clusterInstalled(t, base, e, clusterEnv)
		state := base.stateOf(e.targetID)
		r := clone(t, base)
		r.rules(runningCluster(t, e, state, nodeImage).rules(t)...)
		res := clusterRun(r, e, clusterEnv, "--status")
		res.want(t, 0, "node1: cactusfs-node1-1, running, image "+nodeImage+", version 0.7.0-1, /readyz 200",
			"node4: cactusfs-node4-1, running", "/readyz: 200 on every node", "admin API and console: https://127.0.0.1:9001")
		noChange(t, res)

		r = clone(t, base)
		s := runningCluster(t, e, state, nodeImage)
		s.notReady = []string{"9021"}
		r.rules(s.rules(t)...)
		res = clusterRun(r, e, clusterEnv, "--status")
		res.want(t, 1, "node3: cactusfs-node3-1, running, image "+nodeImage+", version 0.7.0-1, /readyz 503", "not ready")
		noChange(t, res)

		r = secretsRig(t, base.fresh(t))
		r.rules(clusterState{e: e}.rules(t)...)
		clusterRun(r, e, clusterEnv, "--status").want(t, 0, "cactusfs is not installed on Docker Desktop")
	})
}

// TestComposeUninstall: --uninstall stops and removes the deployment's
// containers and its network, by its label, and keeps every volume, the
// entry and compose's files.
func TestComposeUninstall(t *testing.T) {
	t.Parallel()
	e := dockerEngines[0]
	eachShell(t, func(t *testing.T, base *rig) {
		base = clusterInstalled(t, base, e, clusterEnv)
		state := base.stateOf(e.targetID)
		r := clone(t, base)
		r.rules(runningCluster(t, e, state, nodeImage).rules(t)...)
		before := tree(t, state)
		res := clusterRun(r, e, clusterEnv, "--uninstall")
		res.want(t, 0, "removed the containers", "removed the network cactusfs_default", "kept: the volumes")
		var did []string
		for _, c := range res.calls {
			if s := sub(c); mutating(c) || (len(s) > 1 && s[0] == "network" && s[1] == "rm") {
				did = append(did, strings.Join(s, " "))
			}
		}
		want := []string{
			"stop -t 40 cactusfs-init-1 cactusfs-node1-1 cactusfs-node2-1 cactusfs-node3-1 cactusfs-node4-1",
			"rm cactusfs-init-1 cactusfs-node1-1 cactusfs-node2-1 cactusfs-node3-1 cactusfs-node4-1",
			"network rm cactusfs_default",
		}
		if !slices.Equal(did, want) {
			t.Errorf("uninstall did %q, want %q", did, want)
		}
		if after := tree(t, state); !equalTrees(before, after) {
			t.Error("uninstall changed the entry")
		}
	})
}

// TestComposePurge: --uninstall --purge, confirmed, removes the containers
// and the network, deletes compose's volumes, proves by a listing that none
// is left, deletes the config volume, proves no volume is left - and on
// CE_HOST deletes the server's compose/secrets/ and the rest of the
// deployment's directory there, proving it gone - and last the entry of
// STATE_DIR, a sibling entry kept.
func TestComposePurge(t *testing.T) {
	t.Parallel()
	for _, onCE := range []bool{false, true} {
		name := "here"
		e := dockerEngines[0]
		env := clusterEnv
		if onCE {
			name = "ce-host"
			e = dockerEngine{tool: "docker", target: "ce", id: engineID, targetID: "ssh-deploy_server.example.test"}
			env += "CE_HOST=" + ceHost + "\n"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			eachShell(t, func(t *testing.T, base *rig) {
				base = clusterInstalledOn(t, base, e, env, onCE)
				r := clone(t, base)
				state := r.stateOf(e.targetID)
				sibling := filepath.Join(filepath.Dir(state), "other")
				put(t, filepath.Join(sibling, "target"), "kept\n")
				s := runningCluster(t, e, state, nodeImage)
				s.ceHost = onCE
				s.purged = true
				r.rules(s.rules(t)...)
				res := clusterRun(r, e, env, "--uninstall", "--purge", "--purge-confirm=cactusfs")
				res.want(t, 0, "purged: cactusfs and its secrets are gone")
				var did []string
				for _, c := range res.calls {
					s := sub(c)
					switch {
					case c.Tool == "ssh" && slices.Contains(c.Args, "rm"):
						did = append(did, "ssh rm "+c.Args[len(c.Args)-1])
					case c.Tool == "ssh" && slices.Contains(c.Args, "test") && slices.Contains(c.Args, "-e"):
						did = append(did, "ssh test")
					case len(s) > 1 && s[0] == "volume" && s[1] == "rm" && slices.Contains(s, "cactusfs-config"):
						did = append(did, "volume rm config")
						if len(s) != 3 {
							t.Errorf("the config volume is deleted with others: %q", s)
						}
					case len(s) > 1 && s[0] == "volume" && s[1] == "rm":
						did = append(did, "volume rm data")
						if len(s) != 2+12 {
							t.Errorf("the data volumes deleted: %q", s)
						}
					case len(s) > 1 && s[0] == "volume" && s[1] == "ls" && slices.Contains(s, byLabel):
						did = append(did, "listing")
					case len(s) > 0 && (s[0] == "stop" || s[0] == "rm"):
						did = append(did, s[0])
					}
				}
				// What came before the first stop are the questions.
				did = did[slices.Index(did, "stop"):]
				want := []string{"stop", "rm", "volume rm data", "listing", "volume rm config", "listing"}
				if onCE {
					want = append(want, "ssh rm "+ceDir+"/secrets", "ssh rm "+strings.TrimSuffix(ceDir, "/compose"), "ssh test")
				}
				if !slices.Equal(did, want) {
					t.Errorf("the purge did %q, want %q", did, want)
				}
				if _, err := os.Stat(state); err == nil {
					t.Errorf("the purge left %s", state)
				}
				if fileOf(t, filepath.Join(sibling, "target")) != "kept\n" {
					t.Error("the purge touched a sibling entry")
				}
				if !onCE {
					// A volume of the name that is not the cluster's - a
					// standalone deployment's - refuses the purge before
					// anything is stopped or deleted.
					r = clone(t, base)
					s = runningCluster(t, e, r.stateOf(e.targetID), nodeImage)
					s.vols = append(s.vols, "cactusfs-meta "+e.target)
					r.rules(s.rules(t)...)
					res = clusterRun(r, e, env, "--uninstall", "--purge", "--purge-confirm=cactusfs")
					res.want(t, 2, "the volume cactusfs-meta carries the deployment's name and is not one of its compose cluster's")
					noChange(t, res)
					return
				}
				// The server's directory still there after its rm: the purge
				// fails, and the entry - the keys - is kept.
				r = clone(t, base)
				s.remoteLeft = true
				r.rules(s.rules(t)...)
				res = clusterRun(r, e, env, "--uninstall", "--purge", "--purge-confirm=cactusfs")
				res.want(t, 1, "purge: "+strings.TrimSuffix(ceDir, "/compose")+" is still there on "+ceHost)
				if _, err := os.Stat(filepath.Join(r.stateOf(e.targetID), "target")); err != nil {
					t.Errorf("a purge that failed did not keep the entry: %v", err)
				}
			})
		})
	}
}

// clusterInstalledOn is clusterInstalled, on CE_HOST when onCE.
func clusterInstalledOn(t *testing.T, r *rig, e dockerEngine, env string, onCE bool) *rig {
	t.Helper()
	if !onCE {
		return clusterInstalled(t, r, e, env)
	}
	base := secretsRig(t, r.fresh(t))
	base.rules(clusterState{e: e, ceHost: true, devInit: devInitOutput()}.rules(t)...)
	clusterRun(base, e, env, "--install").want(t, 0, "ready: the cluster cactusfs runs")
	return base
}

// TestComposeOnCEHost: with CE_HOST compose runs on the server, over ssh,
// in CE_STATE_DIR there - under env -i with a PATH and the user's HOME -
// from files sent as tar streams: compose's secret files into a 0700
// directory, made 0444, and the compose files beside them; the engine of
// the containers and volumes is the server's through DOCKER_HOST=ssh://,
// after the host key was checked; compose never runs here. Readiness and
// free ports are asked of the server's curl, the leader with the root
// credentials on standard input.
func TestComposeOnCEHost(t *testing.T) {
	t.Parallel()
	e := dockerEngine{tool: "docker", target: "ce", id: engineID, targetID: "ssh-deploy_server.example.test"}
	env := clusterEnv + "CE_HOST=" + ceHost + "\n"
	eachShell(t, func(t *testing.T, r *rig) {
		r = secretsRig(t, r.fresh(t))
		r.rules(clusterState{e: e, ceHost: true, enforcing: true, devInit: devInitOutput()}.rules(t)...)
		r.keepStdin()
		r.trace = true
		res := clusterRun(r, e, env, "--install")
		r.trace = false
		state := r.stateOf(e.targetID)
		res.want(t, 0, "ready: the cluster cactusfs runs", "ssh -L 9001:127.0.0.1:9001 "+ceHost)
		noSecretAnywhere(t, res, state)
		noShellCactusfs(t, res)
		// At the first contact only the check ("true", against the user's
		// known_hosts) and ssh -G go without the pin; every other ssh -
		// the script's and the Docker CLI's - is given the lines copied
		// from known_hosts alone (PLAN 10.7).
		checked := false
		for _, c := range res.calls {
			if c.Tool != "ssh" || slices.Contains(c.Args, "-G") {
				continue
			}
			if !checked && remote(c) == "true" && !pinned(c) {
				checked = true
				continue
			}
			if !pinned(c) || c.KnownHosts == nil || *c.KnownHosts != pinLines {
				t.Errorf("an ssh of the first install without the pin: %q", c.Args)
			}
		}
		if !checked {
			t.Error("the first install did not check the host key against known_hosts first")
		}

		var ssh [][]string
		for _, c := range res.calls {
			if c.Tool == "ssh" {
				i := slices.Index(c.Args, ceHost)
				ssh = append(ssh, c.Args[i+1:])
				if !containsRun(c.Args, []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes"}) {
					t.Errorf("ssh without its options: %q", c.Args)
				}
			}
			if c.Tool == "docker" && slices.Contains(c.Args, "compose") {
				t.Errorf("compose ran here: %q", c.Args)
			}
			if c.Tool == "docker" && !slices.Contains(c.Args, "--context") && c.Env["DOCKER_HOST"] != "ssh://"+ceHost {
				t.Errorf("the server's engine without DOCKER_HOST=ssh://%s: %q", ceHost, c.Args)
			}
		}
		has := func(words ...string) int {
			for i, a := range ssh {
				if slices.Equal(a, words) {
					return i
				}
			}
			t.Fatalf("ssh did not run %q:\n%q", words, ssh)
			return -1
		}
		rpath := "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
		mk := has("mkdir", "-p", ceDir+"/secrets")
		mode := has("chmod", "0700", ceHome+"/.cactusfs-deploy/ssh-deploy_server.example.test", ceHome+"/.cactusfs-deploy/ssh-deploy_server.example.test/cactusfs", ceDir, ceDir+"/secrets")
		put := has("tar", "-x", "-o", "-f", "-", "-C", ceDir+"/secrets")
		ro := has("chmod", "0444", ceDir+"/secrets/kms-passphrase", ceDir+"/secrets/root-access-key", ceDir+"/secrets/root-secret-key")
		files := has("tar", "-x", "-o", "-f", "-", "-C", ceDir)
		label := has("chcon", "-R", "-t", "container_file_t", ceDir+"/secrets")
		up := has("env", "-i", rpath, "HOME="+ceHome, "docker", "compose", "--project-name", "cactusfs", "-f", ceDir+"/compose.yaml",
			"-f", ceDir+"/override.yaml", "--env-file", ceDir+"/compose.env", "up", "-d", "--no-build")
		if mk >= mode || mode >= put || put >= ro || ro >= files || files >= label || label >= up {
			t.Errorf("the server was given its files out of order: %d %d %d %d %d %d %d", mk, mode, put, ro, files, label, up)
		}
		// What the server was sent: the secrets as the entry holds them,
		// and the compose files with the CA.
		for _, c := range res.calls {
			if c.Tool != "ssh" || !slices.Contains(c.Args, "-x") {
				continue
			}
			names := r.tarNames(c)
			slices.Sort(names)
			if slices.Contains(c.Args, ceDir+"/secrets") {
				if !slices.Equal(names, []string{".", "kms-passphrase", "root-access-key", "root-secret-key"}) {
					t.Errorf("the secret files sent: %q", names)
				}
			} else if !slices.Equal(names, []string{"compose.env", "compose.yaml", "override.yaml", "s3-ca.pem"}) {
				t.Errorf("the compose files sent: %q", names)
			}
		}
		has("env", "-i", rpath, "curl", "-q", "-s", "-f", "-o", "/dev/null", "--max-time", "5", "--noproxy", "localhost", "--cacert", ceDir+"/s3-ca.pem",
			"--resolve", "localhost:9031:127.0.0.1", "https://localhost:9031/readyz")
		has("env", "-i", rpath, "curl", "-q", "-s", "-o", "/dev/null", "--max-time", "3", "--noproxy", "127.0.0.1", "http://127.0.0.1:9020/")
		// No .curlrc of the server is read (-q first), and no proxy is
		// taken: every curl it runs.
		for _, c := range res.calls {
			if i := slices.Index(c.Args, "curl"); c.Tool == "ssh" && i >= 0 {
				if i+1 >= len(c.Args) || c.Args[i+1] != "-q" || !slices.Contains(c.Args, "--noproxy") {
					t.Errorf("the server's curl without -q first or --noproxy: %q", c.Args[i:])
				}
			}
		}
		has("env", "-i", rpath, "HOME="+ceHome, "docker", "info")
		has("chmod", "0700", ceDir+"/secrets")
		if got := fileOf(t, filepath.Join(state, "target")); !strings.HasSuffix(got, "mode=cluster\ncompose-dir="+ceDir+"\n") {
			t.Errorf("the record does not name the server's compose directory: %q", got)
		}
		// A second install, with nothing to put: the server's secret files
		// are set 0444 and their directory 0700 again all the same.
		again := clone(t, r)
		as := runningCluster(t, e, again.stateOf(e.targetID), nodeImage)
		as.ceHost = true
		as.remoteSecrets = filesUnder(t, filepath.Join(state, "secrets"))
		again.rules(as.rules(t)...)
		res = clusterRun(again, e, env, "--install")
		res.want(t, 0, "ready: the cluster cactusfs runs", "host key of "+ceHost+": one of those pinned in")
		// The host keys pinned at the first install (PLAN 10.7, ce.sh) are
		// the only ones every later ssh to the server is given - the
		// script's and the Docker CLI's (through the shim) alike.
		if got := fileOf(t, filepath.Join(state, "ssh-known-hosts")); got != pinLines {
			t.Errorf("the pin of the cluster's server is %q, want %q", got, pinLines)
		}
		for _, c := range res.calls {
			if c.Tool == "ssh" && !slices.Contains(c.Args, "-G") && (!pinned(c) || c.KnownHosts == nil || *c.KnownHosts != pinLines) {
				t.Errorf("an ssh to the server without the pin: %q", c.Args)
			}
		}
		if len(callsOf(res, "ssh", "tar", "-x", "-o", "-f", "-", "-C", ceDir+"/secrets")) != 0 {
			t.Error("a second install put secret files on the server")
		}
		for _, words := range [][]string{
			{"chmod", "0444", ceDir + "/secrets/kms-passphrase", ceDir + "/secrets/root-access-key", ceDir + "/secrets/root-secret-key"},
			{"chmod", "0700", ceDir + "/secrets"},
		} {
			if !slices.ContainsFunc(res.calls, func(c call) bool { return c.Tool == "ssh" && containsRun(c.Args, words) }) {
				t.Errorf("a second install did not run %q on the server", words)
			}
		}
		// Another CE_STATE_DIR is refused: the deployment's files are
		// where the record says.
		moved := clone(t, r)
		ms := runningCluster(t, e, moved.stateOf(e.targetID), nodeImage)
		ms.ceHost = true
		moved.rules(ms.rules(t)...)
		res = clusterRun(moved, e, env+"CE_STATE_DIR=/srv/cactusfs-deploy\n", "--install")
		res.want(t, 2, "its compose files in "+ceDir+" on "+ceHost+", and CE_STATE_DIR now puts them in /srv/cactusfs-deploy/")
		noChange(t, res)
		// The docker compose runs with on the server reaches another
		// engine than DOCKER_HOST=ssh:// does: refused before any change.
		other := secretsRig(t, r.fresh(t))
		other.rules(clusterState{e: e, ceHost: true, ceInfoID: "ENGINE-TWO", devInit: devInitOutput()}.rules(t)...)
		res = clusterRun(other, e, env, "--install")
		res.want(t, 2, "compose on "+ceHost+" reaches the engine 'ENGINE-TWO', and DOCKER_HOST=ssh://"+ceHost+" reaches '"+engineID+"'")
		noChange(t, res)
		if _, err := os.Stat(filepath.Join(other.stateOf(e.targetID), "ssh-known-hosts")); err == nil {
			t.Error("a refused first install pinned the host keys")
		}
		// Another key than the pinned ones: every action is refused before
		// the engine, or compose, is asked anything.
		for _, args := range [][]string{{"--install"}, {"--status"}, {"--uninstall"}} {
			moved := clone(t, r)
			ks := runningCluster(t, e, moved.stateOf(e.targetID), nodeImage)
			ks.ceHost = true
			moved.rules(append([]rule{{Tool: "ssh", Contains: []string{"GlobalKnownHostsFile=none", "true"}, Exit: 255, Stderr: "Host key verification failed.\n"}}, ks.rules(t)...)...)
			res = clusterRun(moved, e, env, args...)
			res.want(t, 2, "its host key is not one of those pinned in "+moved.stateOf(e.targetID)+"/ssh-known-hosts")
			noChange(t, res)
			if n := len(callsOf(res, "docker")); n != 0 {
				t.Errorf("%q with another key: the engine was asked %d times", args, n)
			}
			for _, w := range remotes(res) {
				if w != "true" {
					t.Errorf("%q with another key: the server was asked %q", args, w)
				}
			}
		}
		// No compose secret file is kept here: STATE holds no
		// compose/secrets/ for a server.
		if _, err := os.Stat(filepath.Join(state, "compose", "secrets")); err == nil {
			t.Error("compose's secret files of the server are kept here too")
		}
		err := filepath.WalkDir(filepath.Join(state, "compose"), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), ".") {
				t.Errorf("%s is left over from the run", p)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		// CE_INSTALL_DOCKER=yes on a server without Docker (as root):
		// Docker CE and its compose plugin are installed after every
		// question - node 1's ports among them - and before compose is
		// asked anything there; the install goes on to the cluster.
		installEnv := env + "CE_INSTALL_DOCKER=yes\n"
		inst := secretsRig(t, r.fresh(t))
		inst.keepStdin()
		inst.rules(append(installRules(t, "debian", "0", debKey), clusterState{e: e, ceHost: true, devInit: devInitOutput()}.rules(t)...)...)
		res = clusterRun(inst, e, installEnv, "--install")
		res.want(t, 0, "Docker CE: installing it on "+ceHost+" (Debian GNU/Linux 12 (bookworm)), as root", "Docker CE: installed on "+ceHost, "ready: the cluster cactusfs runs")
		all := remotes(res)
		at := func(prefix string) int {
			return slices.IndexFunc(all, func(w string) bool { return strings.HasPrefix(w, prefix) })
		}
		installed := at("systemctl enable --now docker")
		packages := at("env DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin")
		composeAsked := slices.IndexFunc(all, func(w string) bool { return strings.HasSuffix(w, "docker compose version") })
		firstChange := slices.IndexFunc(all, func(w string) bool { return !question(w) && !strings.HasPrefix(w, "printenv HOME") })
		for _, port := range []string{"9000", "9001"} {
			asked := slices.IndexFunc(all, func(w string) bool { return strings.Contains(w, "http://port.invalid:"+port+"/") })
			if asked < 0 || asked > firstChange {
				t.Errorf("node 1's port %s was not asked before the install of Docker CE: %q", port, all)
			}
		}
		if packages < 0 || installed < packages || composeAsked < installed {
			t.Errorf("Docker CE was not installed (packages %d, enabled %d) before compose was asked (%d): %q", packages, installed, composeAsked, all)
		}
		for i, c := range res.calls {
			if c.Tool == "docker" && c.Env["DOCKER_HOST"] != "" {
				for _, later := range res.calls[i:] {
					if strings.Contains(remote(later), "systemctl enable") {
						t.Fatalf("the server's engine was asked before Docker was installed: %q", c.Args)
					}
				}
			}
		}
		// Refused (Kubernetes is on the server): nothing changed there or
		// on an engine, and no pin was written.
		refused := secretsRig(t, r.fresh(t))
		refused.keepStdin()
		refused.rules(append(append([]rule{{Tool: "ssh", Contains: []string{"ls", "-d"}, Stdout: "/etc/rancher\n/usr/local/bin/k3s\n", Exit: 2}},
			installRules(t, "debian", "0", debKey)...), clusterState{e: e, ceHost: true, devInit: devInitOutput()}.rules(t)...)...)
		res = clusterRun(refused, e, installEnv, "--install")
		res.want(t, 2, "Kubernetes is on "+ceHost+" (/etc/rancher /usr/local/bin/k3s)")
		noChange(t, res)
		if got := changesOn(res); len(got) != 1 || got[0] != "printenv HOME" {
			t.Errorf("a refused install of Docker CE ran on the server %q", got)
		}
		if len(callsOf(res, "docker")) != 0 {
			t.Error("a refused install of Docker CE asked an engine")
		}
		if _, err := os.Stat(filepath.Join(refused.stateOf(e.targetID), "ssh-known-hosts")); err == nil {
			t.Error("a refused install of Docker CE pinned the host keys")
		}
		// No binaries: refused before anything is asked of the server's
		// Docker, let alone installed (PLAN 10.4's image_precheck).
		nobin := secretsRig(t, r.fresh(t))
		nobin.keepStdin()
		remove(t, filepath.Join(nobin.repo, "dist"))
		nobin.rules(append(installRules(t, "debian", "0", debKey), clusterState{e: e, ceHost: true, devInit: devInitOutput()}.rules(t)...)...)
		res = clusterRun(nobin, e, strings.Replace(installEnv, "IMAGE_SOURCE=image\n", "IMAGE_SOURCE=binaries\n", 1), "--install")
		res.want(t, 2, "run make image-binaries")
		noChange(t, res)
		for _, w := range remotes(res) {
			if strings.HasPrefix(w, "docker --version") || !question(w) && w != "printenv HOME" {
				t.Errorf("a run without binaries asked or changed on the server: %q", w)
			}
		}
		// A dry run prints the install and runs none of it.
		dry := secretsRig(t, r.fresh(t))
		dry.keepStdin()
		dry.rules(append(installRules(t, "debian", "0", debKey), clusterState{e: e, ceHost: true, devInit: devInitOutput()}.rules(t)...)...)
		res = clusterRun(dry, e, installEnv, "--install", "--dry-run")
		res.want(t, 0, "dry run: Docker CE is not on "+ceHost+" yet", "run: on_host "+ceHost+" env DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce")
		noChange(t, res)
		if got := changesOn(res); len(got) != 1 || got[0] != "printenv HOME" {
			t.Errorf("a dry run ran on the server %q", got)
		}
		if len(callsOf(res, "docker")) != 0 {
			t.Error("a dry run asked an engine Docker CE was not installed for")
		}
		if _, err := os.Stat(dry.stateDir()); err == nil {
			t.Error("a dry run wrote STATE_DIR")
		}
	})
}

// TestComposeSELinux: under enforcing SELinux here, compose's secret files
// are relabelled container_file_t after they are put and before compose
// runs; not otherwise.
func TestComposeSELinux(t *testing.T) {
	t.Parallel()
	e := dockerEngines[0]
	eachShell(t, func(t *testing.T, r *rig) {
		for _, enforcing := range []bool{true, false} {
			r := secretsRig(t, r.fresh(t))
			r.rules(clusterState{e: e, enforcing: enforcing, devInit: devInitOutput()}.rules(t)...)
			res := clusterRun(r, e, clusterEnv, "--install")
			res.want(t, 0, "ready: the cluster cactusfs runs")
			chcon := callsOf(res, "chcon")
			if !enforcing {
				if len(chcon) != 0 {
					t.Errorf("relabelled without SELinux enforcing: %v", chcon)
				}
				continue
			}
			want := []string{"-R", "-t", "container_file_t", filepath.Join(r.stateOf(e.targetID), "compose", "secrets")}
			if len(chcon) != 1 || !slices.Equal(chcon[0].Args, want) {
				t.Fatalf("relabelled with %v, want %q", chcon, want)
			}
			at := slices.IndexFunc(res.calls, func(c call) bool { return c.Tool == "chcon" })
			up := slices.IndexFunc(res.calls, func(c call) bool { return slices.Contains(c.Args, "up") })
			if at > up {
				t.Error("compose ran before the relabel")
			}
		}
	})
}

// TestComposeVault: with Vault, the nodes are given Vault's settings and
// the token as a compose secret, init only says its version, and the
// passphrase secret of compose.yaml is an empty file no node reads.
func TestComposeVault(t *testing.T) {
	t.Parallel()
	e := dockerEngines[0]
	eachShell(t, func(t *testing.T, r *rig) {
		r = secretsRig(t, r.fresh(t))
		r.rules(clusterState{e: e, devInit: devInitOutput()}.rules(t)...)
		env := withVault(r, strings.Replace(clusterEnv, "ALLOW_SHARED_LOCAL_KMS=yes\n", "", 1)+vaultEnv)
		res := clusterRun(r, e, env, "--install")
		res.want(t, 0, "ready: the cluster cactusfs runs")
		state := r.stateOf(e.targetID)
		noSecretAnywhere(t, res, state)
		override := fileOf(t, filepath.Join(state, "compose", "override.yaml"))
		for _, w := range []string{
			"    command:\n      - version\n",
			"      CACTUSFS_KMS: 'vault'\n      CACTUSFS_KMS_KEY_FILE: ''\n      CACTUSFS_KMS_PASSPHRASE_FILE: ''\n",
			"      CACTUSFS_VAULT_ADDR: 'https://vault.example.test:8200'\n",
			"      CACTUSFS_VAULT_TOKEN_FILE: '/run/secrets/vault-token'\n",
			"    secrets: [vault-token]\n",
			"secrets:\n  kms-passphrase:\n    file: ./none/kms-passphrase\n  vault-token:\n    file: ./secrets/vault-token\n",
		} {
			if !strings.Contains(override, w) {
				t.Errorf("the override does not hold %q:\n%s", w, override)
			}
		}
		secretFilesAre(t, filepath.Join(state, "compose", "secrets"), filepath.Join(state, "secrets"))
		if fileOf(t, filepath.Join(state, "compose", "none", "kms-passphrase")) != "" {
			t.Error("the passphrase file of Vault's cluster is not empty")
		}
	})
}

// TestComposeDryRun: with --dry-run a cluster's install, upgrade, status
// and uninstall ask only read-only questions - compose only its version -
// and write no file.
func TestComposeDryRun(t *testing.T) {
	t.Parallel()
	e := dockerEngines[0]
	eachShell(t, func(t *testing.T, base *rig) {
		base = clusterInstalled(t, base, e, clusterEnv)
		fresh := secretsRig(t, base.fresh(t))
		fresh.rules(clusterState{e: e, devInit: devInitOutput()}.rules(t)...)
		running := clone(t, base)
		running.rules(runningCluster(t, e, running.stateOf(e.targetID), nodeImage).rules(t)...)
		for _, c := range []struct {
			name string
			r    *rig
			args []string
			code int
			want string
		}{
			{"install", fresh, []string{"--install"}, 0, "dry run: would wait up to 600 s for /readyz of each node"},
			{"upgrade", running, []string{"--install"}, 0, "upgrade: one node at a time: node4 node1 node3 node2"},
			{"status", running, []string{"--status"}, 0, "/readyz: 200 on every node"},
			{"uninstall", running, []string{"--uninstall"}, 0, "removed the containers"},
			{"purge", running, []string{"--uninstall", "--purge", "--purge-confirm=cactusfs"}, 0, "purge: deleted"},
		} {
			before := tree(t, c.r.dir)
			res := clusterRun(c.r, e, clusterEnv, append(c.args, "--dry-run")...)
			res.want(t, c.code, "dry run: nothing that changes anything is run", c.want)
			noChange(t, res)
			for _, u := range composeCalls(res) {
				if !slices.Equal(u, []string{"compose", "version"}) && !slices.Equal(u, []string{"compose", "ls"}) {
					t.Errorf("%s: compose ran %q", c.name, u)
				}
			}
			for _, call := range res.calls {
				if call.Tool == "chcon" {
					t.Errorf("%s: relabelled in a dry run", c.name)
				}
			}
			if after := tree(t, c.r.dir); !equalTrees(before, after) {
				t.Errorf("%s: a dry run changed files", c.name)
			}
		}
	})
}

// TestComposeRotation: a Vault token rotated on a running cluster - here
// and on CE_HOST - replaces compose's copy of the file, and since a bind
// mount of a single file keeps the file it was made with, every node is
// made again (--force-recreate), one at a time in the upgrade's order, the
// leader last, each ready before the next.
func TestComposeRotation(t *testing.T) {
	t.Parallel()
	for _, onCE := range []bool{false, true} {
		name := "here"
		e := dockerEngines[0]
		if onCE {
			name = "ce-host"
			e = dockerEngine{tool: "docker", target: "ce", id: engineID, targetID: "ssh-deploy_server.example.test"}
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			eachShell(t, func(t *testing.T, base *rig) {
				base = secretsRig(t, base.fresh(t))
				env := withVault(base, strings.Replace(clusterEnv, "ALLOW_SHARED_LOCAL_KMS=yes\n", "", 1)+vaultEnv)
				if onCE {
					env += "CE_HOST=" + ceHost + "\n"
				}
				base.rules(clusterState{e: e, ceHost: onCE, devInit: devInitOutput()}.rules(t)...)
				clusterRun(base, e, env, "--install").want(t, 0, "ready: the cluster cactusfs runs")

				r := clone(t, base)
				state := r.stateOf(e.targetID)
				s := runningCluster(t, e, state, nodeImage)
				s.ceHost = onCE
				if onCE {
					s.remoteSecrets = filesUnder(t, filepath.Join(state, "secrets"))
				}
				r.rules(s.rules(t)...)
				r.keepStdin()
				token := r.envFile("token2", "rotated-token-"+strings.ReplaceAll(t.Name(), "/", "-"))
				rotated := regexp.MustCompile(`(?m)^VAULT_TOKEN_FILE=.*$`).ReplaceAllString(env, "VAULT_TOKEN_FILE="+token)
				res := clusterRun(r, e, rotated, "--install")
				res.want(t, 0, "rotated, not yet held by every node: secrets/vault-token - every node is made again, one at a time", "ready: the cluster cactusfs runs",
					"removed "+filepath.Join(state, "compose", "recreate-pending"))
				var order []string
				for _, u := range composeCalls(res) {
					if !slices.Contains(u, "up") {
						continue
					}
					if !containsRun(u, []string{"up", "-d", "--no-build", "--no-deps", "--force-recreate"}) {
						t.Errorf("a node is not made again by force: %q", u)
					}
					order = append(order, u[len(u)-1])
				}
				if want := []string{"node4", "node1", "node3", "node2"}; !slices.Equal(order, want) {
					t.Errorf("the nodes were made again in the order %q, want %q", order, want)
				}
				// Each made again only once the one before is ready.
				var seq []string
				for _, c := range res.calls {
					switch {
					case slices.Contains(c.Args, "--force-recreate"):
						seq = append(seq, "up "+c.Args[len(c.Args)-1])
					case len(seq) > 0 && strings.HasSuffix(c.Args[len(c.Args)-1], "/readyz"):
						seq = append(seq, "readyz "+c.Args[len(c.Args)-1])
					}
				}
				before(t, seq, "up node4", "readyz https://localhost:9031/readyz", "up node1", "readyz https://localhost:9001/readyz",
					"up node3", "readyz https://localhost:9021/readyz", "up node2", "readyz https://localhost:9011/readyz")
				if fileOf(t, filepath.Join(state, "secrets", "vault-token")) != fileOf(t, token) {
					t.Error("the entry's token was not rotated")
				}
				if onCE {
					for _, c := range callsOf(res, "ssh", "tar", "-x", "-o", "-f", "-", "-C", ceDir+"/secrets") {
						if names := r.tarNames(c); !slices.Equal(names, []string{"vault-token"}) {
							t.Errorf("the server was sent %q, want the token alone", names)
						}
					}
				} else {
					secretFilesAre(t, filepath.Join(state, "compose", "secrets"), filepath.Join(state, "secrets"))
				}
				noSecretAnywhere(t, res, state, token)
				if _, err := os.Stat(filepath.Join(state, "compose", "recreate-pending")); err == nil {
					t.Error("the marker of a pending rotation is left after every node was made again")
				}

				// A rotation that stops part way - at node1's readiness, or
				// before any node is made again - is finished by the next
				// run with the same files: the marker, written before the
				// copies were replaced, makes every node again by force
				// though the copies are equal by then.
				for _, stop := range []struct {
					name string
					set  func(s *clusterState)
					want string
				}{
					{
						"node1 not ready", func(s *clusterState) { s.notReadyLater = []string{"9001"} },
						"The rotated files (secrets/vault-token) are held by the nodes made again: node4 node1; the others still hold the old ones: node3 node2.",
					},
					{
						"compose fails", func(s *clusterState) { s.upFails = "node4" },
						"The rotated files (secrets/vault-token) are held by the nodes made again: none; the others still hold the old ones: node4 node1 node3 node2.",
					},
				} {
					r := clone(t, base)
					state := r.stateOf(e.targetID)
					s := runningCluster(t, e, state, nodeImage)
					s.ceHost = onCE
					if onCE {
						s.remoteSecrets = filesUnder(t, filepath.Join(state, "secrets"))
					}
					stop.set(&s)
					r.rules(s.rules(t)...)
					token := r.envFile("token2", "rotated-token-"+strings.ReplaceAll(t.Name(), "/", "-")+"-"+strings.ReplaceAll(stop.name, " ", "-"))
					rotated := regexp.MustCompile(`(?m)^VAULT_TOKEN_FILE=.*$`).ReplaceAllString(env, "VAULT_TOKEN_FILE="+token)
					res := clusterRun(r, e, rotated+"WAIT_TIMEOUT=1\n", "--install")
					res.want(t, 1, stop.want, "recreate-pending is kept: the next --install makes every node again while no more than one node is down; otherwise --uninstall, then --install")
					// Two nodes down with the rotation pending: refused, saying
					// the marker and the way out.
					two := clone(t, r)
					ts := runningCluster(t, e, two.stateOf(e.targetID), nodeImage)
					ts.ceHost = onCE
					ts.notReady = []string{"9001", "9011"}
					two.rules(ts.rules(t)...)
					res = clusterRun(two, e, rotated, "--install")
					res.want(t, 2, "more than one node is not ready", "run --uninstall, then --install. A rotation is pending (secrets/vault-token, "+filepath.Join(two.stateOf(e.targetID), "compose", "recreate-pending")+")")
					marker := filepath.Join(state, "compose", "recreate-pending")
					if got := fileOf(t, marker); got != "secrets/vault-token\n" {
						t.Fatalf("%s: the marker says %q", stop.name, got)
					}
					// Again, with every node ready: nothing differs any more,
					// and every node is made again by force all the same.
					s = runningCluster(t, e, state, nodeImage)
					s.ceHost = onCE
					if onCE {
						s.remoteSecrets = filesUnder(t, filepath.Join(state, "secrets"))
					}
					r.rules(s.rules(t)...)
					res = clusterRun(r, e, rotated, "--install")
					res.want(t, 0, "rotated, not yet held by every node: secrets/vault-token", "ready: the cluster cactusfs runs")
					var forced []string
					for _, u := range composeCalls(res) {
						if slices.Contains(u, "up") && slices.Contains(u, "--force-recreate") {
							forced = append(forced, u[len(u)-1])
						}
					}
					if want := []string{"node4", "node1", "node3", "node2"}; !slices.Equal(forced, want) {
						t.Errorf("%s: the run after it made again by force %q, want %q", stop.name, forced, want)
					}
					if _, err := os.Stat(marker); err == nil {
						t.Errorf("%s: the marker is left after every node was made again", stop.name)
					}
				}
			})
		})
	}
}

// TestComposeRecovery: one node that is not ready - failed on a new image,
// stopped, or without its container - does not stop an install: it is made
// first, and each node is ready before the next, which is how a failed
// upgrade is rolled back or tried again; two are refused (Upgrade's cases).
func TestComposeRecovery(t *testing.T) {
	t.Parallel()
	e := dockerEngines[0]
	eachShell(t, func(t *testing.T, base *rig) {
		base = clusterInstalled(t, base, e, clusterEnv)
		// The node made first never becomes ready: the run stops there,
		// and no other node is made again.
		t.Run("the first node never ready", func(t *testing.T) {
			t.Parallel()
			r := clone(t, base)
			s := runningCluster(t, e, r.stateOf(e.targetID), nodeImage)
			s.notReady = []string{"9021"}
			r.rules(s.rules(t)...)
			res := clusterRun(r, e, strings.Replace(clusterEnv, nodeImage, newImage, 1)+"WAIT_TIMEOUT=1\n", "--install")
			res.want(t, 1, "node3 is not ready: (/readyz of 127.0.0.1:9021 answered 503); the upgrade makes it first",
				"upgrade stopped: node3 did not become ready within WAIT_TIMEOUT=1 s")
			var ups []string
			for _, u := range composeCalls(res) {
				if slices.Contains(u, "up") {
					ups = append(ups, u[len(u)-1])
				}
			}
			if !slices.Equal(ups, []string{"node3"}) {
				t.Errorf("compose made %q, want node3 alone", ups)
			}
		})
		for _, c := range []struct {
			name  string
			image string
			set   func(s *clusterState)
			first string
		}{
			{"rollback after a node not ready", nodeImage, func(s *clusterState) {
				for i, n := range []string{"node4", "node1"} {
					s.ctrs[i+1] = "cactusfs-" + n + "-1|dd|" + n + "|running|" + newImage
				}
				s.notReadyUntil = []string{"node1"}
			}, "node1"},
			{"a stopped node started", newImage, func(s *clusterState) {
				s.ctrs[2] = "cactusfs-node2-1|dd|node2|exited|" + nodeImage
				s.running = slices.Delete(s.running, 1, 2)
				s.notReadyUntil = []string{"node2"}
			}, "node2"},
			{"a missing container made again", newImage, func(s *clusterState) {
				s.ctrs = slices.Delete(s.ctrs, 3, 4)
				s.running = slices.Delete(s.running, 2, 3)
				s.notReadyUntil = []string{"node3"}
			}, "node3"},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				s := runningCluster(t, e, r.stateOf(e.targetID), nodeImage)
				c.set(&s)
				r.rules(s.rules(t)...)
				res := clusterRun(r, e, strings.Replace(clusterEnv, nodeImage, c.image, 1), "--install")
				res.want(t, 0, c.first+" is not ready: (/readyz of 127.0.0.1:"+adminPort(c.first)+" answered 503); the upgrade makes it first", "ready: the cluster cactusfs runs "+c.image)
				var order []string
				for _, u := range composeCalls(res) {
					if slices.Contains(u, "up") {
						order = append(order, u[len(u)-1])
					}
				}
				if len(order) != 4 || order[0] != c.first {
					t.Errorf("the nodes were made in the order %q, want %s first", order, c.first)
				}
			})
		}
	})
}

// TestComposePodmanRemote: a Podman that is a remote service is refused in
// cluster mode - on Linux always, on macOS unless its default connection
// is a Podman machine of this machine (127.0.0.1 or localhost).
func TestComposePodmanRemote(t *testing.T) {
	t.Parallel()
	e := dockerEngines[2]
	eachShell(t, func(t *testing.T, r *rig) {
		far := secretsRig(t, r.fresh(t))
		far.rules(clusterState{e: e, podmanRemote: "ssh://core@engine.example.test:22/run/podman/podman.sock", devInit: devInitOutput()}.rules(t)...)
		res := clusterRun(far, e, clusterEnv, "--install")
		res.want(t, 2, "cluster mode bind-mounts files of STATE_DIR, and this Podman is a remote service")
		noChange(t, res)

		near := secretsRig(t, r.fresh(t))
		near.rules(clusterState{e: e, podmanRemote: "ssh://core@127.0.0.1:50123/run/user/501/podman/podman.sock", devInit: devInitOutput()}.rules(t)...)
		res = clusterRun(near, e, clusterEnv, "--install")
		if runtime.GOOS == "darwin" {
			res.want(t, 0, "ready: the cluster cactusfs runs")
		} else {
			res.want(t, 2, "this Podman is a remote service: the files are on this machine, not its")
			noChange(t, res)
		}
	})
}
