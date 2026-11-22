//go:build deployscript

package deploy_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Docker CE on a server over SSH (PLAN 10.7, deploy/lib/ce.sh): the
// standalone deployment of PLAN 10.5 on the engine of CE_HOST. Its host
// keys are pinned by ssh itself - the lines of the user's known_hosts that
// accepted it at the first install, kept in the entry - and every ssh to it
// afterwards, the Docker CLI's through a shim first on its PATH, accepts it
// with them alone. The ports and /readyz are asked on the server, and
// Docker CE is installed there when asked.

const (
	ceHost     = "deploy@server.example.test"
	ceTargetID = "ssh-deploy_server.example.test"
	ceEnv      = imageEnv + "CE_HOST=" + ceHost + "\n"
	// pinLines are the lines of known_hosts ssh-keygen -F finds for the
	// server: a hashed one and a host certificate authority's.
	pinLines = "|1|Zml4dHVyZS1zYWx0LTAwMDA=|Zml4dHVyZS1oYXNoLTAwMDA= ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixtureHostKeyOfTheServer00000000000000000\n" +
		"@cert-authority *.example.test ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixtureHostCertificateAuthority00000000000\n"
	// The fingerprints of Docker's signing keys (ce.sh, ce_fingerprint).
	debKey = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"
	rpmKey = "060A61C51B558A7F742B77AAC52FEB6B621E9F35"
	// readyzOnServer is what the server runs for /readyz of a deployment
	// with dev-init's CA, which it reads on standard input.
	readyzOnServer = "curl -q -s -o /dev/null -D - --max-time 5 --noproxy localhost --resolve localhost:9001:127.0.0.1 --cacert /dev/stdin https://localhost:9001/readyz"
	// dialStdio is what the Docker CLI runs over ssh (the fake docker too).
	dialStdio = "docker system dial-stdio"
	// keyDir is the directory the fake server's mktemp -d makes.
	keyDir = "/tmp/tmp.Fx7Kq2Lm9P"
)

// ceEngine is the server's Docker engine as the fakes play it.
var ceEngine = dockerEngine{tool: "docker", target: "ce", id: engineID, targetID: ceTargetID}

// sshPrefix is how every ssh the script runs begins - its own and the
// shim's.
var sshPrefix = []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=20", "-o", "ControlPath=none"}

// plainWord is a word ssh may be handed (plain, deploy/lib/common.sh).
var plainWord = regexp.MustCompile(`^[A-Za-z0-9_./=:@+,-]+$`)

// sshConfig is what ssh -G says of the server: the host it connects to, its
// port and further lines (a ProxyJump, a HostKeyAlias); the known_hosts
// files are the rig's.
func sshConfig(r *rig, host, port, more string) rule {
	return rule{Tool: "ssh", Contains: []string{"-G"}, Stdout: "user deploy\nhostname " + host + "\nport " + port + "\n" + more +
		"userknownhostsfile ~/.ssh/known_hosts " + r.home + "/.ssh/known_hosts2\nglobalknownhostsfile " + r.home + "/global_known_hosts\n"}
}

// keygenRule is what ssh-keygen -F finds for the name in known_hosts.
func keygenRule(name, lines string) rule {
	return rule{Tool: "ssh-keygen", Contains: []string{"-F", name}, Stdout: "# Host " + name + " found: line 3\n" + lines}
}

// ceRules are the answers of the server: the extra rules first, then its
// ssh configuration, the lines known_hosts holds for it, a check whose ssh
// writes a forged "Server host key" line (which must change nothing), its
// ports free, its /readyz as s says, and its engine as s holds it. The
// rig's known_hosts file is made: ssh -G names it.
func ceRules(t *testing.T, r *rig, s engineState, extra ...rule) []rule {
	t.Helper()
	put(t, filepath.Join(r.home, ".ssh", "known_hosts"), "a file ssh-keygen -F is pointed at\n")
	status := "HTTP/2 503 \r\ncontent-length: 0\r\n\r\n"
	if s.ready {
		status = "HTTP/2 200 \r\ncontent-type: text/plain\r\n\r\n"
	}
	rules := append(slices.Clone(extra),
		sshConfig(r, "server.example.test", "22", ""),
		keygenRule("server.example.test", pinLines),
		rule{Tool: "ssh", Contains: []string{"true"}, Stderr: "debug1: Server host key: ssh-ed25519 SHA256:Forged0By0The0Server\n"},
		rule{Tool: "ssh", Contains: []string{"curl", "http://port.invalid:9000/"}, Exit: 7},
		rule{Tool: "ssh", Contains: []string{"curl", "http://port.invalid:9001/"}, Exit: 7},
		rule{Tool: "ssh", Contains: []string{"curl", "https://localhost:9001/readyz"}, Stdout: status})
	return append(rules, s.rules(t)...)
}

// ceRun runs the script with the env file and the arguments on --docker
// ce, with TMPDIR a directory of the rig, under the guard of guardedRun -
// which so proves the run's own directory is removed.
func ceRun(r *rig, env string, args ...string) result {
	r.t.Helper()
	tmp := filepath.Join(r.dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		r.t.Fatal(err)
	}
	return r.guardedRun(append([]string{"--env", r.envFile("site.env", env), "--docker", "ce"}, args...), "TMPDIR="+tmp)
}

// ceInstalled is a rig of the shell of r on which a first install on the
// server has run: the base the cases of a test clone.
func ceInstalled(t *testing.T, r *rig) *rig {
	t.Helper()
	base := secretsRig(t, r.fresh(t))
	base.rules(ceRules(t, base, engineState{e: ceEngine, devInit: devInitOutput(), ready: true})...)
	ceRun(base, ceEnv, "--install").want(t, 0, "ready: cactusfs runs")
	return base
}

// remote is what an ssh call runs on the server: its words after the
// destination ("" for a call that is not ssh, or runs nothing there).
func remote(c call) string {
	if c.Tool != "ssh" {
		return ""
	}
	i := slices.Index(c.Args, "--")
	if i < 0 || i+2 > len(c.Args) {
		return ""
	}
	return strings.Join(c.Args[i+2:], " ")
}

// remotes is what every ssh of the run ran on the server, in order.
func remotes(res result) []string {
	var out []string
	for _, c := range res.calls {
		if c.Tool == "ssh" && remote(c) != "" {
			out = append(out, remote(c))
		}
	}
	return out
}

// pinned is true when ssh was given the pin as its only known_hosts.
// pinnedPrefix is how every pinned ssh begins - on_host's and the shim's -
// before any argument of its caller (Docker's -o ConnectTimeout=30 among
// them, which would otherwise come first): the pin as its only known_hosts
// ("UserKnownHostsFile=" and any file), and nothing else that could accept
// a key - DNS, a KnownHostsCommand, keys the server offers, its address.
var pinnedPrefix = append(slices.Clone(sshPrefix),
	"-o", "UserKnownHostsFile=", "-o", "GlobalKnownHostsFile=none",
	"-o", "VerifyHostKeyDNS=no", "-o", "UpdateHostKeys=no", "-o", "CheckHostIP=no",
	"-o", "IgnoreUnknown=KnownHostsCommand", "-o", "KnownHostsCommand=none")

// pinned is true when ssh's arguments begin with pinnedPrefix.
func pinned(c call) bool {
	if len(c.Args) < len(pinnedPrefix) {
		return false
	}
	for i, w := range pinnedPrefix {
		if w == "UserKnownHostsFile=" {
			if !strings.HasPrefix(c.Args[i], w) || len(c.Args[i]) == len(w) {
				return false
			}
		} else if c.Args[i] != w {
			return false
		}
	}
	return true
}

// sshChecked fails the test unless every ssh of the run is the script's own
// or the shim's: non-interactive, a host key it does not know refused,
// ConnectTimeout set, no multiplexing, the destination after "--", only
// plain words handed to the server, nothing read from standard input but
// by the calls stdin allows (the command run there, mapped to the hash of
// what it may read); every one but the first contact's check and ssh -G
// given the pin as its only known_hosts, holding pin; and every question
// of the server's engine reached through an ssh that is pinned - the
// shim's, first on the Docker CLI's PATH.
func sshChecked(t *testing.T, res result, pin string, stdin map[string]string) {
	t.Helper()
	empty := sha256.Sum256(nil)
	first := true
	for i, c := range res.calls {
		if c.Tool == "docker" && c.Env["DOCKER_HOST"] == "ssh://"+ceHost {
			if i+1 >= len(res.calls) || remote(res.calls[i+1]) != dialStdio || !pinned(res.calls[i+1]) {
				t.Errorf("the Docker CLI reached the server without the pinned ssh: %q", c.Args)
			}
		}
		if c.Tool != "ssh" {
			continue
		}
		if len(c.Args) < len(sshPrefix) || !slices.Equal(c.Args[:len(sshPrefix)], sshPrefix) {
			t.Errorf("ssh %q does not begin %q", c.Args, sshPrefix)
		}
		j := slices.Index(c.Args, "--")
		if j < 0 || j+1 >= len(c.Args) || c.Args[j+1] != ceHost {
			t.Errorf("ssh %q does not name %s after --", c.Args, ceHost)
			continue
		}
		for _, w := range c.Args[j+1:] {
			if !plainWord.MatchString(w) {
				t.Errorf("ssh was handed the word %q, which is not plain", w)
			}
		}
		want, ok := stdin[remote(c)]
		if !ok {
			want = hex.EncodeToString(empty[:])
		}
		if c.Stdin != want {
			t.Errorf("ssh %q read %s from its standard input, want %s", c.Args[j+2:], c.Stdin, want)
		}
		switch {
		case slices.Contains(c.Args, "-G"):
		case first && remote(c) == "true" && !pinned(c):
			first = false
		case !pinned(c):
			t.Errorf("ssh %q without the pin", c.Args)
		case c.KnownHosts == nil || *c.KnownHosts != pin:
			t.Errorf("ssh %q was given the known_hosts %v, want %q", c.Args[j+1:], c.KnownHosts, pin)
		}
	}
}

// sum is the hex SHA-256 of what the file holds.
func sum(t *testing.T, path string) string {
	t.Helper()
	s := sha256.Sum256([]byte(fileOf(t, path)))
	return hex.EncodeToString(s[:])
}

// TestCEInstall: an install on CE_HOST - the host key checked by the
// script's own ssh against known_hosts before anything else, the lines
// that accepted it (a hashed one, a host certificate authority's) copied by
// ssh-keygen -F under the name ssh -G gives, checked to accept it alone,
// and pinned in the entry; a forged "Server host key" line on ssh's
// standard error changes nothing; every ssh after the first is pinned, the
// Docker CLI's through the shim; dev-init on this machine's engine; the
// ports probed on the server before anything changes and /readyz asked
// there over TLS with the CA on ssh's standard input; the ssh -L command
// said. No secret is on any command line, in any environment or in the
// output of a traced run.
func TestCEInstall(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		r = secretsRig(t, r)
		r.rules(ceRules(t, r, engineState{e: ceEngine, devInit: devInitOutput(), ready: true})...)
		r.trace = true
		res := ceRun(r, ceEnv, "--install")
		r.trace = false
		state := r.stateOf(ceTargetID)
		res.want(t, 0, "ready: cactusfs runs "+nodeImage,
			"host key of "+ceHost+": accepted by known_hosts",
			"pinned the host keys of "+ceHost+" (server.example.test) in "+state+"/ssh-known-hosts",
			"ssh -N -L 9001:127.0.0.1:9001 -- "+ceHost, "then https://localhost:9001",
			"S3: https://127.0.0.1:9000 on server.example.test only",
			"anywhere but the server server.example.test")
		noSecretAnywhere(t, res, state)
		modesAre(t, state)
		if got := fileOf(t, filepath.Join(state, "ssh-known-hosts")); got != pinLines {
			t.Errorf("ssh-known-hosts = %q, want %q", got, pinLines)
		}

		// The host key first, against known_hosts; then ssh -G and
		// ssh-keygen -F of the file ~ named (one that is not there is not
		// read); then the check with the pin alone.
		if len(res.calls) < 4 || remote(res.calls[0]) != "true" || pinned(res.calls[0]) ||
			!slices.Contains(res.calls[1].Args, "-G") || res.calls[2].Tool != "ssh-keygen" ||
			remote(res.calls[3]) != "true" || !pinned(res.calls[3]) {
			t.Fatalf("the run does not begin with the checks of the host key: %v", res.calls[:min(4, len(res.calls))])
		}
		keygen := callsOf(res, "ssh-keygen")
		if len(keygen) != 1 || !slices.Equal(keygen[0].Args, []string{"-F", "server.example.test", "-f", filepath.Join(r.home, ".ssh", "known_hosts")}) {
			t.Errorf("ssh-keygen ran %v", keygen)
		}
		ca := filepath.Join(state, "config", "s3-ca.pem")
		sshChecked(t, res, pinLines, map[string]string{readyzOnServer: sum(t, ca)})
		if !slices.Contains(remotes(res), readyzOnServer) {
			t.Errorf("/readyz was not asked on the server: %q", remotes(res))
		}
		if n := len(callsOf(res, "ssh", "-T", "--", ceHost, "docker", "system", "dial-stdio")); n == 0 {
			t.Error("the Docker CLI never reached the server")
		}

		// No curl of this machine: the ports and /readyz are the server's.
		if n := len(callsOf(res, "curl")); n != 0 {
			t.Errorf("curl ran %d times on this machine", n)
		}
		for _, w := range remotes(res) {
			if strings.HasPrefix(w, "curl") && !strings.HasPrefix(w, "curl -q ") {
				t.Errorf("curl on the server without -q first: %q", w)
			}
		}
		// Every question of the server's engine goes through DOCKER_HOST;
		// this machine's engine runs dev-init only (and its image).
		for _, c := range callsOf(res, "docker") {
			remoteEngine := c.Env["DOCKER_HOST"] == "ssh://"+ceHost
			devInit := slices.Contains(c.Args, "--platform") || slices.ContainsFunc(c.Args, func(a string) bool {
				return strings.HasPrefix(a, "cactusfs-dev-init") || strings.HasPrefix(a, "name=cactusfs-dev-init")
			})
			switch {
			case remoteEngine && devInit:
				t.Errorf("dev-init on the server: %q", c.Args)
			case !remoteEngine && !devInit && !containsRun(c.Args, []string{"image", "inspect"}):
				t.Errorf("this machine's engine was asked what the server's should be: %q", c.Args)
			case !remoteEngine && !containsRun(c.Args, []string{"--context", "default"}):
				t.Errorf("this machine's engine without its context: %q", c.Args)
			case remoteEngine && slices.Contains(c.Args, "--context"):
				t.Errorf("the server's engine with a context: %q", c.Args)
			case remoteEngine && !regexp.MustCompile(`^`+regexp.QuoteMeta(filepath.Join(r.dir, "tmp"))+`/cactusfs-ce\.[^/:]+/bin:`).MatchString(c.Env["PATH"]):
				t.Errorf("the Docker CLI's PATH does not begin with the run's shim: %q", c.Env["PATH"])
			case !remoteEngine && strings.Contains(c.Env["PATH"], "cactusfs-ce."):
				t.Errorf("this machine's engine was given the shim: %q", c.Env["PATH"])
			}
		}
		// The ports are probed on the server before anything changes.
		probed := 0
		for _, c := range res.calls {
			if strings.Contains(remote(c), "http://port.invalid:") {
				probed++
				if !strings.Contains(remote(c), "--resolve port.invalid:") || !strings.Contains(remote(c), "--noproxy port.invalid") {
					t.Errorf("a port probed with %q", remote(c))
				}
				continue
			}
			if probed < 2 && mutating(c) {
				t.Errorf("a change before the ports were probed: %q", c.Args)
			}
		}
		if probed != 2 {
			t.Errorf("%d ports probed on the server, want 2", probed)
		}
		run := runOf(t, res)
		holds(t, run, []string{"--publish", "127.0.0.1:9000:9000"}, []string{"--publish", "127.0.0.1:9001:9001"},
			[]string{"--restart", "unless-stopped"})
		if got := fileOf(t, filepath.Join(state, "target")); got != "docker over ssh://"+ceHost+"\nengine-id="+engineID+"\nkms=local\n" {
			t.Errorf("target = %q", got)
		}
	})
}

// TestCEHostKey: a host key known_hosts does not hold is refused before
// anything else runs; known_hosts lines that ssh-keygen -F does not find,
// or that do not accept the server alone, stop the run; a key the pin does
// not hold refuses every action, with nothing run but the check, even when
// known_hosts accepts it; an entry without a pin (an install made before
// PLAN 10.7) is pinned by the next install. The name looked up is ssh -G's:
// a HostKeyAlias, or a port other than 22 in brackets - and with a
// ProxyJump, the target's, whose HostName is where S3 is said to be.
func TestCEHostKey(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		t.Run("unknown", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			r.rules(ceRules(t, r, engineState{e: ceEngine}, rule{
				Tool: "ssh", Contains: []string{"true"}, Exit: 255,
				Stderr: "No ED25519 host key is known for server.example.test and you have requested strict checking.\nHost key verification failed.\n",
			})...)
			for _, args := range [][]string{{"--install"}, {"--status"}, {"--uninstall"}, {"--install", "--dry-run"}} {
				res := ceRun(r, ceEnv, args...)
				res.want(t, 2, "Host key verification failed.", "ssh to "+ceHost+" failed (exit 255", "known_hosts", "README.md")
				if len(res.calls) != 1 {
					t.Errorf("%v: %d calls after the host key was refused: %v", args, len(res.calls), res.calls)
				}
				if _, err := os.Stat(r.stateDir()); err == nil {
					t.Errorf("%v: STATE_DIR was made", args)
				}
			}
		})
		for _, c := range []struct {
			name  string
			rules []rule
			code  int
			want  string
		}{
			{"no line found", []rule{{Tool: "ssh-keygen", Contains: []string{"-F"}, Exit: 1}}, 1, "ssh-keygen -F found no line for server.example.test"},
			{
				"lines that do not accept it",
				[]rule{{Tool: "ssh", Contains: []string{"GlobalKnownHostsFile=none", "true"}, Exit: 255, Stderr: "Host key verification failed.\n"}},
				2, "known_hosts accepted its key, and the lines copied from it for server.example.test do not",
			},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				r.rules(ceRules(t, r, engineState{e: ceEngine}, c.rules...)...)
				res := ceRun(r, ceEnv, "--install")
				res.want(t, c.code, c.want)
				noChange(t, res)
				if len(callsOf(res, "docker")) != 0 {
					t.Error("an engine was asked")
				}
			})
		}
		for _, c := range []struct {
			name, port, more, keyname, s3 string
		}{
			{"port", "2222", "", "[server.example.test]:2222", "https://server.example.test:9000"},
			{"alias", "2222", "hostkeyalias server-key.example.test\n", "server-key.example.test", "https://server.example.test:9000"},
			{"proxy jump", "22", "proxyjump bastion.example.test\n", "node7.internal.example.test", "https://node7.internal.example.test:9000"},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				host := "server.example.test"
				if c.name == "proxy jump" {
					host = "node7.internal.example.test"
				}
				r.rules(ceRules(t, r, engineState{e: ceEngine, devInit: devInitOutput(), ready: true},
					sshConfig(r, host, c.port, c.more), keygenRule(c.keyname, pinLines))...)
				res := ceRun(r, ceEnv+"BIND=0.0.0.0\n", "--install")
				res.want(t, 0, "ready: cactusfs runs", "S3: "+c.s3, "("+c.keyname+")")
				if k := callsOf(res, "ssh-keygen"); len(k) != 1 || k[0].Args[1] != c.keyname {
					t.Errorf("ssh-keygen ran %v, want it to look up %s", k, c.keyname)
				}
				if got := fileOf(t, filepath.Join(r.stateOf(ceTargetID), "ssh-known-hosts")); got != pinLines {
					t.Errorf("ssh-known-hosts = %q", got)
				}
				sshChecked(t, res, pinLines, map[string]string{readyzOnServer: sum(t, filepath.Join(r.stateOf(ceTargetID), "config", "s3-ca.pem"))})
			})
		}

		base := ceInstalled(t, r)
		state := base.stateOf(ceTargetID)
		t.Run("another key", func(t *testing.T) {
			t.Parallel()
			for _, args := range [][]string{{"--install"}, {"--status"}, {"--uninstall"}, {"--uninstall", "--purge", "--purge-confirm", "cactusfs"}} {
				r := clone(t, base)
				// known_hosts now accepts the server's new key: the pin
				// still refuses it, and ssh is given the pin alone.
				r.rules(ceRules(t, r, installedState(t, ceEngine, state, nodeImage),
					rule{Tool: "ssh", Contains: []string{"GlobalKnownHostsFile=none", "true"}, Exit: 255, Stderr: "Host key verification failed.\n"})...)
				res := ceRun(r, ceEnv, args...)
				res.want(t, 2, "its host key is not one of those pinned in "+r.stateOf(ceTargetID)+"/ssh-known-hosts")
				// ssh -G and ssh-keygen -F (for @revoked lines) are of this
				// machine; the one ssh to the server is the pinned check,
				// last.
				last := res.calls[len(res.calls)-1]
				if len(remotes(res)) != 1 || remote(last) != "true" || !pinned(last) || last.KnownHosts == nil || *last.KnownHosts != pinLines {
					t.Errorf("%v: the calls after another key: %v", args, res.calls)
				}
				if len(callsOf(res, "docker")) != 0 {
					t.Errorf("%v: an engine was asked after another key", args)
				}
				if got := fileOf(t, filepath.Join(r.stateOf(ceTargetID), "ssh-known-hosts")); got != pinLines {
					t.Errorf("%v: the pin became %q", args, got)
				}
			}
		})
		t.Run("a colon in TMPDIR", func(t *testing.T) {
			t.Parallel()
			// The run's directory begins the Docker CLI's PATH, where a ':'
			// would split it.
			r := secretsRig(t, r.fresh(t))
			tmp := filepath.Join(r.dir, "a:b")
			if err := os.MkdirAll(tmp, 0o700); err != nil {
				t.Fatal(err)
			}
			res := r.guardedRun([]string{"--env", r.envFile("site.env", ceEnv), "--docker", "ce", "--status"}, "TMPDIR="+tmp)
			res.want(t, 2, "PATH can begin with")
			if len(res.calls) != 0 {
				t.Errorf("the run asked %v", res.calls)
			}
		})
		t.Run("revoked since", func(t *testing.T) {
			t.Parallel()
			// known_hosts now revokes a key and accepts another: the run
			// takes the @revoked line into its copy of the pin, never the
			// new key, and the pin in STATE does not change.
			r := clone(t, base)
			const revoked = "@revoked * ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixtureRevokedKey000000000000000000000000000\n"
			const accepted = "server.example.test ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFixtureNewKeyNotPinned0000000000000000000\n"
			r.rules(ceRules(t, r, installedState(t, ceEngine, state, nodeImage),
				keygenRule("server.example.test", pinLines+revoked+accepted))...)
			res := ceRun(r, ceEnv, "--status")
			res.want(t, 0, "known_hosts revokes 1 key(s) for server.example.test", "one of those pinned in")
			sshChecked(t, res, pinLines+revoked, map[string]string{readyzOnServer: sum(t, filepath.Join(state, "config", "s3-ca.pem"))})
			if got := fileOf(t, filepath.Join(r.stateOf(ceTargetID), "ssh-known-hosts")); got != pinLines {
				t.Errorf("the pin in STATE became %q", got)
			}
		})
		t.Run("pinned by the next install", func(t *testing.T) {
			t.Parallel()
			r := clone(t, base)
			pin := filepath.Join(r.stateOf(ceTargetID), "ssh-known-hosts")
			remove(t, pin)
			r.rules(ceRules(t, r, installedState(t, ceEngine, state, nodeImage))...)
			res := ceRun(r, strings.Replace(ceEnv, nodeImage, newImage, 1), "--install")
			res.want(t, 0, "ready: cactusfs runs "+newImage, "pinned the host keys of "+ceHost)
			if got := fileOf(t, pin); got != pinLines {
				t.Errorf("ssh-known-hosts = %q", got)
			}
			// The install after that reads the pin, and asks known_hosts
			// nothing.
			res = ceRun(r, ceEnv, "--install")
			res.want(t, 0, "one of those pinned in "+pin)
			if strings.Contains(res.stdout, "pinned the host keys") || fileOf(t, pin) != pinLines {
				t.Error("the pin was made again")
			}
			sshChecked(t, res, pinLines, map[string]string{readyzOnServer: sum(t, filepath.Join(r.stateOf(ceTargetID), "config", "s3-ca.pem"))})
		})
	})
}

// TestCEStatus: --status asks the server's engine by the deployment's label
// and /readyz on the server, with the pinned ssh, changes nothing, and
// prints the ssh -L command that brings the admin port here; an ssh that
// fails on /readyz reads as not ready.
func TestCEStatus(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		base := ceInstalled(t, r)
		state := base.stateOf(ceTargetID)
		for _, c := range []struct {
			name  string
			ready bool
			extra []rule
			code  int
			want  []string
		}{
			{"ready", true, nil, 0, []string{"/readyz: 200"}},
			{"not ready", false, nil, 1, []string{"/readyz: 503", "not ready: its log: DOCKER_HOST=ssh://" + ceHost + " docker logs cactusfs"}},
			{"ssh fails", true, []rule{{Tool: "ssh", Contains: []string{"curl", "https://localhost:9001/readyz"}, Exit: 255}}, 1, []string{"/readyz: 000"}},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := clone(t, base)
				s := installedState(t, ceEngine, state, nodeImage)
				s.ready = c.ready
				r.rules(ceRules(t, r, s, c.extra...)...)
				res := ceRun(r, ceEnv, "--status")
				res.want(t, c.code, append(c.want, "one of those pinned in", "node: cactusfs, running, image "+nodeImage,
					"admin API and console: https://127.0.0.1:9001 on server.example.test; from this machine: ssh -N -L 9001:127.0.0.1:9001 -- "+ceHost)...)
				noChange(t, res)
				sshChecked(t, res, pinLines, map[string]string{readyzOnServer: sum(t, filepath.Join(state, "config", "s3-ca.pem"))})
				for _, call := range callsOf(res, "docker") {
					if call.Env["DOCKER_HOST"] != "ssh://"+ceHost {
						t.Errorf("--status asked an engine other than the server's: %q", call.Args)
					}
				}
			})
		}
	})
}

// TestCEPorts: a port something answers on the server is refused before
// anything changes, naming it; a server without curl is refused; on the
// server, a bind to every address is dialled at its loopback address, and
// an IPv6 one is given to curl without brackets (which are not plain).
func TestCEPorts(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range []struct {
			name, env string
			extra     rule
			want      string
		}{
			{
				"S3 in use", "",
				rule{Tool: "ssh", Contains: []string{"curl", "http://port.invalid:9000/"}, Exit: 52},
				"port 9000 on 127.0.0.1 of " + ceHost + " is in use",
			},
			{
				"admin in use", "",
				rule{Tool: "ssh", Contains: []string{"curl", "http://port.invalid:9001/"}},
				"port 9001 on 127.0.0.1 of " + ceHost + " is in use",
			},
			{
				"no curl there", "",
				rule{Tool: "ssh", Contains: []string{"curl", "http://port.invalid:9000/"}, Exit: 127},
				"curl is needed on " + ceHost,
			},
			{
				"every address", "BIND=0.0.0.0\n",
				rule{Tool: "ssh", Contains: []string{"curl", "--resolve", "port.invalid:9000:127.0.0.1"}},
				"port 9000 on 0.0.0.0 of " + ceHost + " is in use",
			},
			{
				"IPv6", "ADMIN_BIND=::1\n",
				rule{Tool: "ssh", Contains: []string{"curl", "--resolve", "port.invalid:9001:::1"}},
				"port 9001 on ::1 of " + ceHost + " is in use",
			},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				r.rules(ceRules(t, r, engineState{e: ceEngine, devInit: devInitOutput(), ready: true}, c.extra)...)
				res := ceRun(r, ceEnv+c.env, "--install")
				res.want(t, 2, c.want)
				noChange(t, res)
				sshChecked(t, res, pinLines, nil)
				if _, err := os.Stat(r.stateDir()); err == nil {
					t.Error("STATE_DIR was made")
				}
			})
		}
	})
}

// TestCEPurge: an uninstall and a purge on the server go through its
// engine, after the host key is checked against the pin, and a purge ends
// by deleting the entry - the pin with it.
func TestCEPurge(t *testing.T) {
	t.Parallel()
	eachShell(t, func(t *testing.T, r *rig) {
		base := ceInstalled(t, r)
		state := base.stateOf(ceTargetID)
		r = clone(t, base)
		s := installedState(t, ceEngine, state, nodeImage)
		s.purged = true
		r.rules(ceRules(t, r, s)...)
		res := ceRun(r, ceEnv, "--uninstall", "--purge", "--purge-confirm", "cactusfs")
		res.want(t, 0, "one of those pinned in", "purged: cactusfs and its secrets are gone from Docker CE on "+ceHost+" over SSH")
		if _, err := os.Stat(r.stateOf(ceTargetID)); err == nil {
			t.Error("the entry is still there")
		}
		if len(callsOf(res, "docker", "volume", "rm", "cactusfs-data1")) != 1 {
			t.Error("the data volumes were not deleted")
		}
		for _, c := range callsOf(res, "docker") {
			if c.Env["DOCKER_HOST"] != "ssh://"+ceHost {
				t.Errorf("the purge asked an engine other than the server's: %q", c.Args)
			}
		}
		sshChecked(t, res, pinLines, nil)
	})
}

// installRules are the answers of a server without Docker that runs the
// distribution of the os-release file of testdata, as the user given ("0":
// root), on amd64, with nothing of Kubernetes or podman-docker, and a gpg
// that reads the key of the fingerprint given.
func installRules(t *testing.T, osRelease, uid, fingerprint string) []rule {
	t.Helper()
	return []rule{
		{Tool: "ssh", Contains: []string{"docker", "--version"}, Stderr: "bash: line 1: docker: command not found\n", Exit: 127},
		{Tool: "ssh", Contains: []string{"cat", "/etc/os-release"}, Stdout: string(repoFile(t, "test/deploy/testdata/os-release/"+osRelease))},
		{Tool: "ssh", Contains: []string{"id", "-u"}, Stdout: uid + "\n"},
		{Tool: "ssh", Contains: []string{"id", "-un"}, Stdout: "deploy\n"},
		{Tool: "ssh", Contains: []string{"dpkg", "--print-architecture"}, Stdout: "amd64\n"},
		{Tool: "ssh", Contains: []string{"ls", "-d"}, Stderr: "ls: cannot access '/etc/rancher': No such file or directory\n", Exit: 2},
		{Tool: "ssh", Contains: []string{"ps", "-e", "-o", "comm="}, Stdout: "systemd\n  sshd\ncontainerd-shim\nbash\n"},
		{Tool: "ssh", Contains: []string{"dpkg-query", "-W", "podman-docker"}, Stderr: "dpkg-query: no packages found matching podman-docker\n", Exit: 1},
		{Tool: "ssh", Contains: []string{"rpm", "-q", "podman-docker"}, Stdout: "package podman-docker is not installed\n", Exit: 1},
		{Tool: "ssh", Contains: []string{"mktemp", "-d"}, Stdout: keyDir + "\n"},
		{Tool: "ssh", Contains: []string{"gpg", "--show-keys"}, Stdout: "pub:-:4096:1:" + fingerprint[24:] + ":1487788586:::-:::scESC::::::23::0:\n" +
			"fpr:::::::::" + fingerprint + ":\nuid:-::::1487788586::0000000000000000000000000000000000000000::Docker Release (CE) <docker@docker.com>::::::::::0:\n" +
			"sub:-:4096:1:7EA0A9C3F273FCD8:1487789221:::::s::::::23:\nfpr:::::::::D3306A018370199E527AE7997EA0A9C3F273FCD8:\n"},
		{Tool: "ssh", Contains: []string{"dd", "status=none"}, SaveStdin: stdinDir + "/repository"},
	}
}

// installQuestions are the questions an install of Docker CE asks before
// it changes anything, by how each begins.
var installQuestions = []string{
	"docker --version", "cat /etc/os-release", "id -u", "id -un", "sudo -n true", "dpkg --print-architecture",
	"ls -d ", "ps -e -o comm=", "dpkg-query -W podman-docker", "rpm -q podman-docker", "curl -q -s -o /dev/null --max-time 3 ",
}

// question is true for what the server is asked, not told to change.
func question(w string) bool {
	if w == "true" || w == dialStdio || strings.HasPrefix(w, "gpg ") || strings.HasPrefix(w, "curl -q -s ") {
		return true
	}
	for _, q := range installQuestions {
		if strings.HasPrefix(w, q) {
			return true
		}
	}
	return false
}

// changesOn is what the run changed on the server: every ssh but the
// questions, in order.
func changesOn(res result) []string {
	var out []string
	for _, w := range remotes(res) {
		if !question(w) {
			out = append(out, w)
		}
	}
	return out
}

// TestCEInstallDocker: CE_INSTALL_DOCKER=yes on a server without Docker
// installs Docker CE from Docker's repository for each distribution of
// testdata/os-release - every question first, the server's ports among
// them; then the key fetched over HTTPS into a directory of its own, its
// fingerprint checked there, and only then installed where the repository
// (written on dd's standard input, as Docker's pages write it) trusts it;
// the packages; as root, or through sudo -n with the user added to the
// group docker when CE_DOCKER_GROUP=yes - and the install goes on to the
// node.
func TestCEInstallDocker(t *testing.T) {
	t.Parallel()
	const debList, rpmList = "/etc/apt/sources.list.d/docker.sources", "/etc/yum.repos.d/docker-ce.repo"
	fetch := func(repo string) []string {
		return []string{
			"mktemp -d",
			"curl -q -fsSL --proto =https --tlsv1.2 -o " + keyDir + "/docker.asc https://download.docker.com/linux/" + repo + "/gpg",
		}
	}
	deb := func(repo string) []string {
		out := []string{
			"sudo -n env DEBIAN_FRONTEND=noninteractive apt-get update",
			"sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl gnupg",
			"sudo -n install -m 0755 -d /etc/apt/keyrings",
		}
		out = append(out, fetch(repo)...)
		return append(out,
			"sudo -n install -m 0644 "+keyDir+"/docker.asc /etc/apt/keyrings/docker.asc",
			"rm -rf -- "+keyDir,
			"sudo -n dd of="+debList+" status=none",
			"sudo -n env DEBIAN_FRONTEND=noninteractive apt-get update",
			"sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin",
			"sudo -n systemctl enable --now docker",
			"sudo -n usermod -aG docker deploy",
		)
	}
	rpm := func(repo string) []string {
		return append(fetch(repo),
			"sudo -n install -m 0644 "+keyDir+"/docker.asc /etc/pki/rpm-gpg/docker-ce.asc",
			"rm -rf -- "+keyDir,
			"sudo -n rpm --import /etc/pki/rpm-gpg/docker-ce.asc",
			"sudo -n dd of="+rpmList+" status=none",
			"sudo -n dnf -y install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin",
			"sudo -n systemctl enable --now docker",
			"sudo -n usermod -aG docker deploy",
		)
	}
	// As Docker's pages write them: the deb822 source, and the
	// docker-ce-stable repository of docker-ce.repo with the key a file.
	debRepo := func(repo, suite string) string {
		return "Types: deb\nURIs: https://download.docker.com/linux/" + repo + "\nSuites: " + suite +
			"\nComponents: stable\nArchitectures: amd64\nSigned-By: /etc/apt/keyrings/docker.asc\n"
	}
	rpmRepo := func(repo string) string {
		return "[docker-ce-stable]\nname=Docker CE Stable - $basearch\nbaseurl=https://download.docker.com/linux/" + repo +
			"/$releasever/$basearch/stable\nenabled=1\ngpgcheck=1\ngpgkey=file:///etc/pki/rpm-gpg/docker-ce.asc\n"
	}
	cases := []struct {
		osRelease, fingerprint, name string
		changes                      []string
		list, repository             string
	}{
		{"debian", debKey, "Debian GNU/Linux 12 (bookworm)", deb("debian"), debList, debRepo("debian", "bookworm")},
		{"ubuntu", debKey, "Ubuntu 24.04.1 LTS", deb("ubuntu"), debList, debRepo("ubuntu", "noble")},
		{"rocky", rpmKey, "Rocky Linux 9.4 (Blue Onyx)", rpm("centos"), rpmList, rpmRepo("centos")},
		{"rhel", rpmKey, "Red Hat Enterprise Linux 9.4 (Plow)", rpm("rhel"), rpmList, rpmRepo("rhel")},
	}
	const installEnv = ceEnv + "CE_INSTALL_DOCKER=yes\nCE_DOCKER_GROUP=yes\n"
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.osRelease, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				r.keepStdin()
				r.rules(ceRules(t, r, engineState{e: ceEngine, devInit: devInitOutput(), ready: true},
					installRules(t, c.osRelease, "1000", c.fingerprint)...)...)
				res := ceRun(r, installEnv, "--install")
				res.want(t, 0, "Docker CE: installing it on "+ceHost+" ("+c.name+"), as deploy through sudo",
					"the key fetched is Docker's, "+c.fingerprint, "Docker CE: installed on "+ceHost, "ready: cactusfs runs")
				got := changesOn(res)
				if len(got) < len(c.changes) || !slices.Equal(got[:len(c.changes)], c.changes) {
					t.Fatalf("the changes on the server:\n%q\nwant them to begin:\n%q", got, c.changes)
				}
				if repo := fileOf(t, filepath.Join(r.dir, stdinDir, "repository")); repo != c.repository {
					t.Errorf("the repository written:\n%s\nwant:\n%s", repo, c.repository)
				}
				// Every question before the first change; the questions of
				// the install never after it (the ports are asked again,
				// of the engine's node, once it is there).
				all := remotes(res)
				first := slices.Index(all, c.changes[0])
				asked := map[string]bool{}
				for i, w := range all {
					for _, q := range installQuestions {
						if !strings.HasPrefix(w, q) {
							continue
						}
						if i < first {
							asked[q] = true
						} else if !strings.HasPrefix(q, "curl") {
							t.Errorf("the question %q after the first change", w)
						}
					}
				}
				for _, q := range installQuestions {
					if c.fingerprint == debKey && q == "rpm -q podman-docker" || c.fingerprint == rpmKey && strings.HasPrefix(q, "dpkg") {
						continue
					}
					if !asked[q] {
						t.Errorf("%q was not asked before the first change", q)
					}
				}
				// gpg read the key where it was fetched, before it was put
				// where the package manager trusts it.
				gpg := slices.IndexFunc(all, func(w string) bool { return strings.HasPrefix(w, "gpg ") })
				placed := slices.IndexFunc(all, func(w string) bool { return strings.Contains(w, "install -m 0644") })
				if gpg < 0 || placed < gpg || all[gpg] != "gpg --homedir "+keyDir+" --batch --with-colons --show-keys "+keyDir+"/docker.asc" {
					t.Errorf("the key was not checked where it was fetched before it was placed: %q", all)
				}
				for i, call := range res.calls {
					if call.Tool == "docker" && call.Env["DOCKER_HOST"] != "" {
						for _, later := range res.calls[i:] {
							if strings.Contains(remote(later), "systemctl enable") {
								t.Fatalf("the server's engine was asked before Docker was installed: %q", call.Args)
							}
						}
						break
					}
				}
				sshChecked(t, res, pinLines, map[string]string{
					"sudo -n dd of=" + c.list + " status=none": sum(t, filepath.Join(r.dir, stdinDir, "repository")),
					readyzOnServer: sum(t, filepath.Join(r.stateOf(ceTargetID), "config", "s3-ca.pem")),
				})
				for _, w := range all {
					if strings.Contains(w, " -k") || strings.Contains(w, "--insecure") {
						t.Errorf("the server ran %q", w)
					}
				}
			})
		}
		t.Run("as root", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			r.keepStdin()
			r.rules(ceRules(t, r, engineState{e: ceEngine, devInit: devInitOutput(), ready: true}, installRules(t, "debian", "0", debKey)...)...)
			res := ceRun(r, strings.Replace(ceEnv, ceHost, "root@server.example.test", 1)+"CE_INSTALL_DOCKER=yes\n", "--install")
			res.want(t, 0, "as root, from https://download.docker.com/linux/debian")
			for _, w := range changesOn(res) {
				if strings.HasPrefix(w, "sudo") || strings.HasPrefix(w, "usermod") {
					t.Errorf("as root the server ran %q", w)
				}
			}
			if !slices.Contains(changesOn(res), "env DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin") {
				t.Errorf("Docker CE was not installed: %q", changesOn(res))
			}
			if slices.Contains(remotes(res), "id -un") || slices.Contains(remotes(res), "sudo -n true") {
				t.Error("root was asked for sudo")
			}
		})
		t.Run("docker there", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			r.rules(ceRules(t, r, engineState{e: ceEngine, devInit: devInitOutput(), ready: true},
				rule{Tool: "ssh", Contains: []string{"docker", "--version"}, Stdout: "Docker version 27.3.1, build ce12230\n"})...)
			res := ceRun(r, ceEnv+"CE_INSTALL_DOCKER=yes\n", "--install")
			res.want(t, 0, "has Docker already (Docker version 27.3.1, build ce12230): CE_INSTALL_DOCKER=yes installs nothing", "ready: cactusfs runs")
			if got := changesOn(res); len(got) != 0 {
				t.Errorf("the server was changed over ssh: %q", got)
			}
		})
		t.Run("no curl yet", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			r.keepStdin()
			// Debian's install brings curl: the ports are asked again once
			// it is there.
			installed := []string{"install", "-y", "docker-ce"}
			r.rules(ceRules(t, r, engineState{e: ceEngine, devInit: devInitOutput(), ready: true}, append([]rule{
				{Tool: "ssh", Contains: []string{"curl", "-s", "--max-time", "3"}, After: installed, Exit: 7},
				{Tool: "ssh", Contains: []string{"curl", "-s", "--max-time", "3"}, Exit: 127},
			}, installRules(t, "debian", "1000", debKey)...)...)...)
			res := ceRun(r, installEnv, "--install")
			res.want(t, 0, "has no curl yet; its ports are asked once the install has put it there", "ready: cactusfs runs")
		})
		t.Run("dry run", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			r.rules(ceRules(t, r, engineState{e: ceEngine}, installRules(t, "ubuntu", "1000", debKey)...)...)
			res := ceRun(r, installEnv, "--install", "--dry-run")
			res.want(t, 0, "run: on_host "+ceHost+" sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce",
				"Suites: noble", "would check with gpg", "dry run: Docker CE is not on "+ceHost+" yet")
			if got := changesOn(res); len(got) != 0 {
				t.Errorf("a dry run changed the server: %q", got)
			}
			if len(callsOf(res, "docker")) != 0 {
				t.Error("a dry run asked an engine the server does not have")
			}
			if _, err := os.Stat(r.stateDir()); err == nil {
				t.Error("STATE_DIR was made")
			}
		})
		t.Run("not Docker's key", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			r.rules(ceRules(t, r, engineState{e: ceEngine, devInit: devInitOutput(), ready: true},
				installRules(t, "debian", "1000", "0123456789ABCDEF0123456789ABCDEF01234567")...)...)
			res := ceRun(r, installEnv, "--install")
			res.want(t, 1, "is not Docker's key", "fingerprint '0123456789ABCDEF0123456789ABCDEF01234567'; Docker's is "+debKey,
				"it was never put at /etc/apt/keyrings/docker.asc")
			got := changesOn(res)
			if !slices.Contains(got, "rm -rf -- "+keyDir) {
				t.Errorf("the key's directory was not removed: %q", got)
			}
			for _, w := range got {
				if strings.Contains(w, "install -m 0644") || strings.Contains(w, "docker-ce") || strings.Contains(w, " dd ") {
					t.Errorf("the server ran %q after a key that is not Docker's", w)
				}
			}
			if len(callsOf(res, "docker")) != 0 {
				t.Error("an engine was asked after the key was refused")
			}
		})
	})
}

// TestCEInstallDockerRefused: an unknown distribution (whatever ID_LIKE
// says), a Debian without its release name, a user without passwordless
// sudo, a user other than root without CE_DOCKER_GROUP=yes, a server where
// k3s, k0s, MicroK8s or Kubernetes is installed or runs, one with
// podman-docker (the package, or its docker command), a port in use, and
// no curl on a server whose install does not bring it are refused before
// anything changes there or anywhere; CE_INSTALL_DOCKER_FORCE=yes installs
// on a Kubernetes or podman-docker server with a warning, and does not
// change the rest; OFFLINE=yes refuses the fetch before anything is asked.
func TestCEInstallDockerRefused(t *testing.T) {
	t.Parallel()
	files := func(out string) rule {
		return rule{Tool: "ssh", Contains: []string{"ls", "-d"}, Stdout: out, Exit: 2}
	}
	processes := func(out string) rule {
		return rule{Tool: "ssh", Contains: []string{"ps", "-e", "-o", "comm="}, Stdout: out}
	}
	podmanPackage := rule{Tool: "ssh", Contains: []string{"dpkg-query", "-W", "podman-docker"}, Stdout: "podman-docker\t4.3.1+ds1-8+deb12u1\n"}
	podmanShim := rule{Tool: "ssh", Contains: []string{"docker", "--version"}, Stdout: "podman version 4.3.1\n", Stderr: "Emulate Docker CLI using podman.\n"}
	noSudo := rule{Tool: "ssh", Contains: []string{"sudo", "-n", "true"}, Stderr: "sudo: a password is required\n", Exit: 1}
	const group = "CE_DOCKER_GROUP=yes\n"
	cases := []struct {
		name, osRelease, env string
		extra                []rule
		code                 int
		want                 string
	}{
		{"alpine", "alpine", group, nil, 2, "runs Alpine Linux v3.20 (ID=alpine), which is not a distribution the script installs Docker CE on"},
		{"alpine forced", "alpine", group + "CE_INSTALL_DOCKER_FORCE=yes\n", nil, 2, "CE_INSTALL_DOCKER_FORCE does not change this"},
		{"linuxmint", "linuxmint", group, nil, 2, "(ID=linuxmint, ID_LIKE=ubuntu debian), which is not a distribution"},
		{"no codename", "debian-no-codename", group, nil, 2, "names no release (VERSION_CODENAME)"},
		{"no sudo", "debian", group, []rule{noSudo}, 2, "the user deploy of " + ceHost + " has no sudo without a password"},
		{"group docker", "debian", "", nil, 2, "say CE_DOCKER_GROUP=yes to have the install add it (sudo -n usermod -aG docker deploy)"},
		{"group docker forced", "debian", "CE_INSTALL_DOCKER_FORCE=yes\n", nil, 2, "say CE_DOCKER_GROUP=yes"},
		{"k3s", "ubuntu", group, []rule{files("/etc/rancher\n/usr/local/bin/k3s\n")}, 2, "Kubernetes is on " + ceHost + " (/etc/rancher /usr/local/bin/k3s)"},
		{"microk8s", "ubuntu", group, []rule{files("/snap/microk8s\n/var/snap/microk8s\n")}, 2, "(/snap/microk8s /var/snap/microk8s)"},
		{"k0s", "debian", group, []rule{files("/var/lib/k0s\n/usr/local/bin/k0s\n/etc/k0s\n")}, 2, "(/var/lib/k0s /usr/local/bin/k0s /etc/k0s)"},
		{"kubelet running", "rocky", group, []rule{processes("systemd\n  kubelet\nsshd\n")}, 2, "(process kubelet)"},
		{"k3s running", "debian", group, []rule{processes("systemd\nk3s-server\ncontainerd\n")}, 2, "(process k3s-server)"},
		{"k0s running", "debian", group, []rule{processes("k0scontroller\nkubelite\n")}, 2, "(process k0scontroller process kubelite)"},
		{"podman-docker", "debian", group, []rule{podmanPackage}, 2, "the package podman-docker is installed, which Docker CE conflicts with"},
		{"podman shim", "rocky", group, []rule{podmanShim}, 2, "docker there is not Docker: it says 'podman version 4.3.1' (podman-docker)"},
		{"port in use", "debian", group, []rule{{Tool: "ssh", Contains: []string{"curl", "http://port.invalid:9000/"}}}, 2, "port 9000 on 127.0.0.1 of " + ceHost + " is in use"},
		{"no curl", "rhel", group, []rule{{Tool: "ssh", Contains: []string{"curl", "http://port.invalid:9000/"}, Exit: 127}}, 2, "curl is needed on " + ceHost},
		{
			"k3s forced", "ubuntu", group + "CE_INSTALL_DOCKER_FORCE=yes\n",
			[]rule{files("/etc/rancher\n/usr/local/bin/k3s\n")},
			0,
			"CE_INSTALL_DOCKER_FORCE=yes: installing Docker CE on " + ceHost + ", where k3s, k0s, MicroK8s or Kubernetes is",
		},
		{"podman-docker forced", "debian", group + "CE_INSTALL_DOCKER_FORCE=yes\n", []rule{podmanPackage}, 0, "though the package podman-docker is installed"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				r.keepStdin()
				fingerprint := debKey
				if c.osRelease == "rocky" || c.osRelease == "rhel" {
					fingerprint = rpmKey
				}
				r.rules(ceRules(t, r, engineState{e: ceEngine, devInit: devInitOutput(), ready: true},
					append(c.extra, installRules(t, c.osRelease, "1000", fingerprint)...)...)...)
				res := ceRun(r, ceEnv+"CE_INSTALL_DOCKER=yes\n"+c.env, "--install")
				res.want(t, c.code, c.want)
				if c.code == 0 {
					if !slices.ContainsFunc(changesOn(res), func(w string) bool { return strings.Contains(w, "install -y docker-ce") }) {
						t.Errorf("forced, Docker CE was not installed: %q", changesOn(res))
					}
					return
				}
				res.want(t, 2, "nothing was changed")
				if got := changesOn(res); len(got) != 0 {
					t.Errorf("a refused run changed the server: %q", got)
				}
				if len(callsOf(res, "docker")) != 0 {
					t.Error("a refused run asked an engine")
				}
				if _, err := os.Stat(r.stateDir()); err == nil {
					t.Error("STATE_DIR was made")
				}
			})
		}
		t.Run("offline", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			res := ceRun(r, ceEnv+"CE_INSTALL_DOCKER=yes\nOFFLINE=yes\n", "--install")
			res.want(t, 2, "OFFLINE=yes and CE_INSTALL_DOCKER=yes")
			if len(res.calls) != 0 {
				t.Errorf("an offline run asked %v", res.calls)
			}
		})
		t.Run("a port in CE_HOST", func(t *testing.T) {
			t.Parallel()
			r := secretsRig(t, r.fresh(t))
			res := ceRun(r, strings.Replace(ceEnv, ceHost, ceHost+":2222", 1), "--status")
			res.want(t, 2, "CE_HOST="+ceHost+":2222: names no port or IPv6 address")
			if len(res.calls) != 0 {
				t.Errorf("the run asked %v", res.calls)
			}
		})
	})
}

// TestCEInstallDockerPrecheck: the image's refusals that need no engine
// (PLAN 10.4's image_precheck) come before anything is done to the server:
// CE_INSTALL_DOCKER=yes without binaries, or with binaries of a tree that
// has changes, is refused (exit 2) with Docker not even asked for there,
// and nothing changed or pinned.
func TestCEInstallDockerPrecheck(t *testing.T) {
	t.Parallel()
	const binEnv = "KMS=local\nCE_HOST=" + ceHost + "\nCE_INSTALL_DOCKER=yes\nCE_DOCKER_GROUP=yes\n"
	cases := []struct {
		name  string
		setup func(t *testing.T, r *rig)
		rules []rule
		want  string
	}{
		{"no binaries", func(t *testing.T, r *rig) { remove(t, filepath.Join(r.repo, "dist")) }, gitRules(""), "run make image-binaries"},
		{"a tree with changes", nil, gitRules(" M deploy/deploy.sh\n"), "the target is on another machine (Docker CE on " + ceHost + ")"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := secretsRig(t, r.fresh(t))
				r.keepStdin()
				if c.setup != nil {
					c.setup(t, r)
				}
				r.rules(ceRules(t, r, engineState{e: ceEngine, devInit: devInitOutput(), ready: true},
					append(c.rules, installRules(t, "debian", "0", debKey)...)...)...)
				res := ceRun(r, binEnv, "--install")
				res.want(t, 2, c.want)
				noChange(t, res)
				if got := remotes(res); len(got) != 0 {
					t.Errorf("the server was asked %q before the image's refusals", got)
				}
				if len(callsOf(res, "docker")) != 0 {
					t.Error("an engine was asked")
				}
				if _, err := os.Stat(r.stateDir()); err == nil {
					t.Error("STATE_DIR was written")
				}
			})
		}
	})
}
