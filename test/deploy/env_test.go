//go:build deployscript

package deploy_test

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// withEnv writes the env file site.env and runs the script with --env and
// the arguments.
func (r *rig) withEnv(content string, args ...string) result {
	r.t.Helper()
	path := r.envFile("site.env", content)
	return r.run(append([]string{"--env", path}, args...))
}

// statusDD is the arguments of a run that gets as far as its action: a dry
// run of --status on Docker Desktop. Built since PLAN 10.5, it asks the
// engine by the deployment's label only, and with nothing there - what the
// fakes answer unless told otherwise - says so and exits statusDone.
var statusDD = []string{"--docker", "dd", "--status", "--dry-run"}

// statusDone is the exit status of --status when the deployment is not
// installed (or is ready).
const statusDone = 0

// TestHelpListsEveryFlag: --help names every flag the script parses, and
// every flag of PLAN row 10.1.
func TestHelpListsEveryFlag(t *testing.T) {
	src := repoFile(t, "deploy/deploy.sh")
	arm := regexp.MustCompile(`(?m)^\t(--[a-z-]+)(?:=\*)?(?: \| -[a-z])?\)`)
	parsed := map[string]bool{}
	for _, m := range arm.FindAllStringSubmatch(string(src), -1) {
		parsed[m[1]] = true
	}
	for _, flag := range []string{
		"--docker", "--env", "--install", "--status", "--uninstall", "--purge",
		"--purge-confirm", "--from-source", "--dry-run", "--help",
		"--another-target", "--print-namespace",
	} {
		if !parsed[flag] {
			t.Errorf("deploy.sh does not parse %s", flag)
		}
	}
	eachShell(t, func(t *testing.T, r *rig) {
		res := r.run([]string{"--help"})
		res.want(t, 0)
		for flag := range parsed {
			if !regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(flag) + `\b`).MatchString(res.stdout) {
				t.Errorf("--help does not list %s:\n%s", flag, res.stdout)
			}
		}
		if len(res.calls) != 0 {
			t.Errorf("--help ran %v", res.calls)
		}
	})
}

// TestValuesAreTakenLiterally: a value that a shell would run is a value -
// passed to the nodes as written, or refused, never repeating it when it is
// a setting of the nodes - and nothing runs. A setting of the nodes with a
// $, a backtick or a control character is refused: none takes one.
func TestValuesAreTakenLiterally(t *testing.T) {
	type value struct {
		name, text string
		// nodeRefused: as a setting of the nodes the value is refused.
		nodeRefused bool
	}
	values := func(mark string) []value {
		return []value{
			{"substitution", "$(touch " + mark + ")", true},
			{"backtick", "`touch " + mark + "`", true},
			{"semicolon", "x; touch " + mark, false},
			{"and and pipe", "x && touch " + mark + " | cat > " + mark, false},
			{"quotes and expansions", "'quoted' \"double\" \\ $HOME ${HOME} *", true},
			{"quotes", "'quoted' \"double\" \\ * ? [a]", false},
			{"escape character", "x\x1b[31m", true},
			{"tab", "a\tb", true},
		}
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, v := range values("MARK") {
			t.Run(v.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				mark := filepath.Join(r.dir, "ran")
				text := strings.ReplaceAll(v.text, "MARK", mark)
				res := r.withEnv("KMS=local\nCACTUSFS_S3_DOMAIN="+text+"\n", statusDD...)
				if v.nodeRefused {
					res.want(t, 2, "site.env:2: the value of CACTUSFS_S3_DOMAIN holds a $, a backtick or a control character")
					if strings.Contains(res.output(), text) {
						t.Fatalf("the refusal repeats the value:\n%s", res.output())
					}
				} else {
					res.want(t, statusDone, "is not installed")
					if want := []string{"CACTUSFS_S3_DOMAIN=" + text}; !slices.Equal(res.node, want) {
						t.Fatalf("passed to the nodes %q, want %q", res.node, want)
					}
				}
				if !strings.ContainsAny(text, "\t\x1b") {
					res = r.withEnv("KMS=local\nDEPLOY_NAME="+text+"\n", statusDD...)
					res.want(t, 2, "DEPLOY_NAME="+text+": must be lower-case")
					res = r.withEnv("KMS=local\nTLS_CERT_FILE=~/"+text+"\nTLS_KEY_FILE=/k\n", statusDD...)
					// The key file does not exist, so the run is refused - after
					// the path was read as written.
					res.want(t, 2, "TLS_KEY_FILE=/k: is not a file that can be read")
				}
				if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("the value %q ran: %s exists", text, mark)
				}
				if entries, err := os.ReadDir(r.work); err != nil || len(entries) != 0 {
					t.Fatalf("the working directory changed: %v %v", entries, err)
				}
			})
		}
		t.Run("a path", func(t *testing.T) {
			t.Parallel()
			r := r.fresh(t)
			mark := filepath.Join(r.dir, "ran")
			key := filepath.Join(r.dir, "key")
			if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
				t.Fatal(err)
			}
			res := r.withEnv("KMS=local\nTLS_CERT_FILE=~/$(touch "+mark+")\nTLS_KEY_FILE="+key+"\n", statusDD...)
			res.want(t, statusDone)
			res.setting(t, "TLS_CERT_FILE", r.home+"/$(touch "+mark+")", "file")
			if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the path ran: %s exists", mark)
			}
		})
	})
}

// TestRefusedLines: a line that is not KEY=value with a known key is
// refused with its line number, and the value is never repeated.
func TestRefusedLines(t *testing.T) {
	const secret = "s3cr3t-value-never-printed"
	cases := []struct {
		name, line, want string
	}{
		{"lower-case key", "deploy_target=dd", "the text before the first '=' (13 characters) is not upper case"},
		{"mixed-case key", "Deploy_Name=x", "(11 characters) is not upper case"},
		{"export", "export DEPLOY_NAME=x", "is not upper case"},
		{"pasted secret with an equals sign", "wJalrXUtnFEMI/K7MDENG+" + secret + "==", "(48 characters) is not upper case"},
		{"pasted upper-case secret", "AKIA/" + strings.ToUpper(secret) + "=", "(31 characters) is not a key"},
		{"unknown key", "DEPLOY_TARGT=dd", "unknown key DEPLOY_TARGT"},
		{"no equals", "DEPLOY_TARGET dd " + secret, "no '=' on the line"},
		{"bare word", secret, "no '=' on the line"},
		{"space before equals", "DEPLOY_NAME =x", "(12 characters) is not a key"},
		{"leading space", " DEPLOY_NAME=x", "(12 characters) is not a key"},
		{"digit first", "1KEY=x", "(4 characters) is not a key"},
		{"empty key", "=" + secret, "(0 characters) is not a key"},
		{"carriage return", "DEPLOY_NAME=x\r", "carriage return"},
		{"twice", "DEPLOY_NAME=a\nDEPLOY_NAME=b", "DEPLOY_NAME is set twice"},
		{"purge confirmation", "PURGE_CONFIRM=cactusfs", "PURGE_CONFIRM is not a setting"},
		{"root access key inline", "ROOT_ACCESS_KEY=" + secret, "write ROOT_ACCESS_KEY_FILE="},
		{"root secret key inline", "ROOT_SECRET_KEY=" + secret, "write ROOT_SECRET_KEY_FILE="},
		{"passphrase inline", "KMS_PASSPHRASE=" + secret, "write KMS_PASSPHRASE_FILE="},
		{"vault token inline", "VAULT_TOKEN=" + secret, "write VAULT_TOKEN_FILE="},
		{"key file inline", "KMS_KEY=" + secret, "write KMS_KEY_FILE="},
		{"a password", "ADMIN_PASSWORD=" + secret, "ADMIN_PASSWORD looks like a secret"},
		{"a token", "REGISTRY_TOKEN=" + secret, "REGISTRY_TOKEN looks like a secret"},
		{"a secret", "S3_SECRET=" + secret, "S3_SECRET looks like a secret"},
		{"a passphrase", "MY_PASSPHRASE=" + secret, "MY_PASSPHRASE looks like a secret"},
		{"a credential", "REGISTRY_CREDENTIALS=" + secret, "REGISTRY_CREDENTIALS looks like a secret"},
		{"node secret", "CACTUSFS_KMS_PASSPHRASE=" + secret, "CACTUSFS_KMS_PASSPHRASE looks like a secret"},
		{"node root secret", "CACTUSFS_ROOT_SECRET_KEY=" + secret, "CACTUSFS_ROOT_SECRET_KEY looks like a secret"},
		{"node secret by its name", "CACTUSFS_REGISTRY_TOKEN=" + secret, "CACTUSFS_REGISTRY_TOKEN looks like a secret"},
		{"node password by its name", "CACTUSFS_SMTP_PASSWORD_FILE=/p", "CACTUSFS_SMTP_PASSWORD_FILE looks like a secret"},
		{"node credential by its name", "CACTUSFS_CLOUD_CREDENTIALS=" + secret, "CACTUSFS_CLOUD_CREDENTIALS looks like a secret"},
		{"node value with a substitution", "CACTUSFS_LOG_LEVEL=$(" + secret + ")", "the value of CACTUSFS_LOG_LEVEL holds a $"},
		{"node value with a backtick", "CACTUSFS_LOG_LEVEL=`" + secret + "`", "the value of CACTUSFS_LOG_LEVEL holds a $, a backtick"},
		{"node secret file", "CACTUSFS_TLS_KEY=/k", "CACTUSFS_TLS_KEY is a secret"},
		{"node managed", "CACTUSFS_DATA=/d", "CACTUSFS_DATA is set by the script itself"},
		{"node peers", "CACTUSFS_PEERS=a=b:1", "CACTUSFS_PEERS is set by the script itself"},
		{"node kms", "CACTUSFS_KMS=local", "CACTUSFS_KMS is set by the script itself"},
		{"node unknown", "CACTUSFS_NOT_A_SETTING=x", "CACTUSFS_NOT_A_SETTING is not a setting of cactusfs server"},
		{"node lower case", "CACTUSFS_log_level=x", "is not upper case"},
		{"node twice", "CACTUSFS_LOG_LEVEL=info\nCACTUSFS_LOG_LEVEL=debug", "CACTUSFS_LOG_LEVEL is set twice"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				// Line 1 a comment, line 2 blank, line 3 KMS: the line under
				// test is line 4 (and the one after it line 5).
				res := r.withEnv("# a site\n\nKMS=local\n"+c.line+"\n", statusDD...)
				line := 4
				if strings.Contains(c.line, "\n") {
					line = 5
				}
				res.want(t, 2, fmt.Sprintf("site.env:%d: ", line), c.want)
				if strings.Contains(res.output(), secret) || strings.Contains(res.output(), strings.ToUpper(secret)) {
					t.Fatalf("the refusal repeats the value:\n%s", res.output())
				}
				if len(res.calls) != 0 {
					t.Fatalf("a refused file still ran %v", res.calls)
				}
			})
		}
	})
}

// TestFileForm: blank, blank-looking and comment lines - indented ones too -
// are skipped, a last line without a newline is read, the value is
// everything after the first "=", and an empty value is the default.
func TestFileForm(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		res := r.withEnv("\n  \t \n# DEPLOY_NAME=commented\n#DEPLOY_NAME=commented\n  # DEPLOY_NAME=indented\n\t#DEPLOY_MODE=tab\n \t # lower=case\nKMS=local\nCACTUSFS_EGRESS_ALLOW=a=b==c \nDEPLOY_MODE=\nDEPLOY_NAME=last", statusDD...)
		res.want(t, statusDone)
		res.setting(t, "DEPLOY_NAME", "last", "file")
		res.setting(t, "DEPLOY_MODE", "standalone", "default")
		if want := []string{"CACTUSFS_EGRESS_ALLOW=a=b==c "}; !slices.Equal(res.node, want) {
			t.Fatalf("passed to the nodes %q, want %q", res.node, want)
		}
	})
}

// TestPrecedence: a flag, then the env file, then the default; never the
// environment of the process.
func TestPrecedence(t *testing.T) {
	cases := []struct {
		name  string
		env   string
		args  []string
		vars  []string
		code  int
		key   string
		value string
		from  string
	}{
		{name: "default", env: "KMS=local\n", args: statusDD, code: statusDone, key: "DEPLOY_NAME", value: "cactusfs", from: "default"},
		{name: "file over default", env: "KMS=local\nDEPLOY_NAME=site\n", args: statusDD, code: statusDone, key: "DEPLOY_NAME", value: "site", from: "file"},
		{name: "flag over file", env: "KMS=local\nDEPLOY_TARGET=podman\n", args: statusDD, code: statusDone, key: "DEPLOY_TARGET", value: "dd", from: "flag"},
		{
			name: "from-source over file", env: "KMS=local\nIMAGE_SOURCE=image\nIMAGE=registry.example.test/cactusfs:1\n",
			args: []string{"--docker", "dd", "--install", "--from-source", "--dry-run"}, code: 3,
			key: "IMAGE_SOURCE", value: "source", from: "flag",
		},
		{
			name: "not the environment", env: "KMS=local\n", args: statusDD, code: statusDone,
			vars: []string{"DEPLOY_NAME=fromenv", "DEPLOY_MODE=cluster", "IMAGE_SOURCE=image", "SETTINGS=DEPLOY_NAME=x", "FILE_SETTINGS=DEPLOY_NAME=y", "FLAG_SETTINGS=DEPLOY_NAME=z", "PASSED=CACTUSFS_DATA=/x"},
			key:  "DEPLOY_NAME", value: "cactusfs", from: "default",
		},
		{
			name: "not the environment: the target", env: "KMS=local\n", args: []string{"--status", "--dry-run"}, code: 2,
			vars: []string{"DEPLOY_TARGET=dd", "TARGET=dd"},
		},
		{
			name: "not the environment: KMS", env: "DEPLOY_NAME=x\n", args: statusDD, code: 2,
			vars: []string{"KMS=local"},
		},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				path := r.envFile("site.env", c.env)
				res := r.run(append([]string{"--env", path}, c.args...), c.vars...)
				res.want(t, c.code)
				if c.key != "" {
					res.setting(t, c.key, c.value, c.from)
				}
				if c.name == "not the environment" && len(res.node) != 0 {
					t.Fatalf("passed to the nodes from the environment: %q", res.node)
				}
			})
		}
	})
}

// TestEnvFileChoice: --env, else deploy/.env, else none - and then KMS is
// not set, which is refused.
func TestEnvFileChoice(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		r.run(statusDD).want(t, 2, "KMS is not set")
		local := filepath.Join(r.repo, "deploy", ".env")
		if err := os.WriteFile(local, []byte("KMS=local\nDEPLOY_NAME=local\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		res := r.run(statusDD)
		res.want(t, statusDone)
		res.setting(t, "DEPLOY_NAME", "local", "file")
		// --env wins, and deploy/.env is not read at all: its line would
		// be refused.
		if err := os.WriteFile(local, []byte("not a line\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		res = r.withEnv("KMS=local\nDEPLOY_NAME=chosen\n", statusDD...)
		res.want(t, statusDone)
		res.setting(t, "DEPLOY_NAME", "chosen", "file")
		r.run(append([]string{"--env", filepath.Join(r.dir, "missing.env")}, statusDD...)).want(t, 2, "no file")
	})
}

// TestKMSMustBeStated: KMS is local or vault, never unset or empty, and
// what each needs.
func TestKMSMustBeStated(t *testing.T) {
	cases := []struct{ name, env, want string }{
		{"unset", "DEPLOY_NAME=x\n", "KMS is not set"},
		{"empty", "KMS=\n", "KMS is not set"},
		{"other", "KMS=none\n", "KMS=none: must be one of: local vault"},
		{"vault without its settings", "KMS=vault\n", "KMS=vault needs VAULT_ADDR"},
		{"vault over http", "KMS=vault\nVAULT_ADDR=http://vault.example.test:8200\n", "must be https://host:port"},
		{"local key file shared", "KMS=local\nDEPLOY_MODE=cluster\n", "ALLOW_SHARED_LOCAL_KMS=yes"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r.fresh(t).withEnv(c.env, statusDD...).want(t, 2, c.want)
			})
		}
		res := r.withEnv("KMS=local\nDEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\n", statusDD...)
		res.want(t, 3, "not built yet (PLAN 10.8)")
		token := r.envFile("token", "t")
		res = r.withEnv("KMS=vault\nVAULT_ADDR=https://vault.example.test:8200\nVAULT_KEY=cactusfs\nVAULT_TOKEN_FILE="+token+"\n", statusDD...)
		res.want(t, statusDone)
		res.setting(t, "KMS", "vault", "file")
	})
}

// TestPaths: "~/" is expanded in *_DIR and *_FILE only, a relative path is
// the repository's, STATE_DIR is never inside the repository and its
// default follows XDG_STATE_HOME.
func TestPaths(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		res := r.withEnv("KMS=local\nRELEASE_DIR=~/rel\nNODE_CA_FILE=certs/ca.crt\nCACTUSFS_S3_DOMAIN=~/x\n", statusDD...)
		res.want(t, statusDone)
		res.setting(t, "RELEASE_DIR", r.home+"/rel", "file")
		res.setting(t, "NODE_CA_FILE", r.repo+"/certs/ca.crt", "file")
		res.setting(t, "STATE_DIR", r.home+"/.local/state/cactusfs-deploy", "default")
		res.setting(t, "CE_STATE_DIR", ".cactusfs-deploy", "default")
		if want := []string{"CACTUSFS_S3_DOMAIN=~/x"}; !slices.Equal(res.node, want) {
			t.Fatalf("passed to the nodes %q, want %q", res.node, want)
		}
		res = r.run(append([]string{"--env", r.envFile("site.env", "KMS=local\n")}, statusDD...), "XDG_STATE_HOME="+r.dir+"/xdg")
		res.want(t, statusDone)
		res.setting(t, "STATE_DIR", r.dir+"/xdg/cactusfs-deploy", "default")

		for _, c := range []struct{ name, env, want string }{
			{"state in the repository", "STATE_DIR=state", "is inside the repository"},
			{"state is the repository", "STATE_DIR=" + r.repo, "is inside the repository"},
			{"another user's home", "STATE_DIR=~root/state", "only a leading '~/' is expanded"},
			{"server directory with a tilde", "CE_STATE_DIR=~/x", "is a directory on the server"},
			{"climbs into the repository", "STATE_DIR=" + r.dir + "/outside/../repo/state", "holds a '..'"},
			{"climbs out of the repository", "STATE_DIR=" + r.repo + "/../outside", "holds a '..'"},
			{"climbs at the end", "STATE_DIR=" + r.repo + "/deploy/..", "holds a '..'"},
			{"relative climb", "STATE_DIR=../state", "holds a '..'"},
		} {
			r.withEnv("KMS=local\n"+c.env+"\n", statusDD...).want(t, 2, c.want)
		}
		// A link into the repository is followed, however deep the part of
		// the path that does not exist yet; a link elsewhere is accepted.
		for _, link := range []struct{ name, to string }{{"repo-link", r.repo}, {"deploy-link", filepath.Join(r.repo, "deploy")}, {"out-link", r.home}} {
			if err := os.Symlink(link.to, filepath.Join(r.dir, link.name)); err != nil {
				t.Fatal(err)
			}
		}
		for _, dir := range []string{r.dir + "/repo-link", r.dir + "/repo-link/state", r.dir + "/deploy-link/a/b/c", r.dir + "/repo-link/deploy/lib/"} {
			r.withEnv("KMS=local\nSTATE_DIR="+dir+"\n", statusDD...).want(t, 2, "STATE_DIR="+dir+": is inside the repository ("+r.repo)
		}
		for _, dir := range []string{r.dir + "/out-link/state", r.dir + "/outside/a/b", r.dir + "/repository-sibling/state", r.dir + "/rep"} {
			res := r.withEnv("KMS=local\nSTATE_DIR="+dir+"\n", statusDD...)
			res.want(t, statusDone)
			res.setting(t, "STATE_DIR", dir, "file")
		}
		// The directory the rig was made in, as it was named: on macOS the
		// temporary directory is under /var, a link to /private/var.
		res = r.withEnv("KMS=local\nSTATE_DIR="+filepath.Join(r.base, "repo", "state")+"\n", statusDD...)
		res.want(t, 2, "is inside the repository")

		// The repository's path in another case: the same directory on a
		// case-insensitive filesystem (macOS by default), and refused there;
		// another, which does not exist, on a case-sensitive one.
		upper := strings.ToUpper(r.repo)
		res = r.withEnv("KMS=local\nSTATE_DIR="+upper+"/state\n", statusDD...)
		if caseInsensitive(t, r.repo, upper) {
			res.want(t, 2, "STATE_DIR="+upper+"/state: is inside the repository ("+r.repo+"/state")
			// And the script called by the path in another case: the
			// repository is still the one the filesystem stores.
			r.repoAs = upper
			res = r.withEnv("KMS=local\nSTATE_DIR="+r.repo+"/state\n", statusDD...)
			r.repoAs = ""
			res.want(t, 2, "STATE_DIR="+r.repo+"/state: is inside the repository")
		} else {
			res.want(t, statusDone)
			res.setting(t, "STATE_DIR", upper+"/state", "file")
		}

		// A link on the part of the path that does not exist yet - one that
		// points nowhere - is refused, wherever it would point.
		for _, link := range []struct{ name, to string }{{"dangling-in", filepath.Join(r.repo, "not-yet")}, {"dangling-out", filepath.Join(r.dir, "nowhere")}} {
			if err := os.Symlink(link.to, filepath.Join(r.dir, link.name)); err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{r.dir + "/" + link.name, r.dir + "/" + link.name + "/state", r.dir + "/" + link.name + "/a/b"} {
				r.withEnv("KMS=local\nSTATE_DIR="+dir+"\n", statusDD...).want(t, 2, "STATE_DIR="+dir+": has a symbolic link that points nowhere")
			}
		}
		for _, dir := range []string{"~/Documents/cactusfs", "~/Desktop/s", "~/Library/Mobile Documents/x", "/data/Dropbox/s", "/x/OneDrive - Example/s"} {
			res := r.withEnv("KMS=local\nSTATE_DIR="+dir+"\n", statusDD...)
			res.want(t, statusDone, "a sync service may copy")
		}
		res = r.withEnv("KMS=local\nSTATE_DIR=/srv/cactusfs-state\n", statusDD...)
		res.want(t, statusDone)
		if strings.Contains(res.output(), "sync service") {
			t.Fatalf("warned of a directory no sync service copies:\n%s", res.output())
		}
	})
}

// caseInsensitive: whether the filesystem of path takes other, the same
// path in another case, for the same directory.
func caseInsensitive(t *testing.T, path, other string) bool {
	t.Helper()
	if path == other {
		t.Fatalf("%s has no letter to change the case of", path)
	}
	a, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(other)
	if err != nil {
		return false
	}
	return os.SameFile(a, b)
}

// TestStateDirUnderTmp: a repository under /tmp - on macOS a link to
// /private/tmp - is the same repository by either name, and a STATE_DIR
// outside it is accepted by either name. The rigs are made under /tmp
// itself, not the test's temporary directory, so that the link is the one
// of the system.
func TestStateDirUnderTmp(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		base, err := os.MkdirTemp("/tmp", "cactusfs-deploy-test-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.RemoveAll(base); err != nil {
				t.Error(err)
			}
		})
		r = newRigIn(t, r.sh, base)
		names := []string{base}
		if r.dir != base {
			names = append(names, r.dir) // /private/tmp/... on macOS
		}
		for _, name := range names {
			for _, dir := range []string{name + "/repo", name + "/repo/state", name + "/repo/deploy/x/y"} {
				r.withEnv("KMS=local\nSTATE_DIR="+dir+"\n", statusDD...).want(t, 2, "STATE_DIR="+dir+": is inside the repository ("+r.repo)
			}
			res := r.withEnv("KMS=local\nSTATE_DIR="+name+"/state\n", statusDD...)
			res.want(t, statusDone)
			res.setting(t, "STATE_DIR", name+"/state", "file")
		}
	})
}

// TestValidation: a value of the wrong form is refused with its key.
func TestValidation(t *testing.T) {
	cases := []struct{ name, env, want string }{
		{"drives", "DRIVES=5", "DRIVES=5: must be a whole number from 1 to 4"},
		{"validity in days", "TLS_VALIDITY=365d", "TLS_VALIDITY=365d: must be a number of hours"},
		{"validity too long", "TLS_VALIDITY=100000h", "TLS_VALIDITY=100000h: must be from 24h to 87600h"},
		{"validity too short", "TLS_VALIDITY=1h", "TLS_VALIDITY=1h: must be from 24h to 87600h"},
		{"no drives", "DRIVES=0", "DRIVES=0"},
		{"privileged port", "S3_PORT=80", "S3_PORT=80"},
		{"same ports", "S3_PORT=9001", "the same port"},
		{"name", "DEPLOY_NAME=Prod", "DEPLOY_NAME=Prod"},
		{"rancher engine", "RANCHER_ENGINE=docker", "RANCHER_ENGINE=docker: must be one of: auto moby containerd"},
		{"k3s import begins with an option", "K3S_IMPORT=-n k3s ctr", "K3S_IMPORT=-n k3s ctr: begins with '-n'"},
		{"k3s import of spaces only", "K3S_IMPORT=   ", "K3S_IMPORT=   : holds no word"},
		{"kubectl with an equals sign", "KUBECTL=A=b", "KUBECTL=A=b: names a program, and a program's name holds no '='"},
		{"helm with an equals sign", "HELM=helm=3", "HELM=helm=3: names a program"},
		{"pass a lower-case name", "TOOL_ENV_PASS=https_proxy", "TOOL_ENV_PASS=https_proxy: must be names of variables"},
		{"pass a value", "TOOL_ENV_PASS=HTTPS_PROXY=http://x", "TOOL_ENV_PASS=HTTPS_PROXY=http://x: must be names"},
		{"pass with a space", "TOOL_ENV_PASS=A, B", "must be names"},
		{"pass an empty name", "TOOL_ENV_PASS=A,,B", "must be names"},
		{"pass a trailing comma", "TOOL_ENV_PASS=A,", "must be names"},
		{"pass a digit first", "TOOL_ENV_PASS=A,1B", "1B is not a name"},
		{"pass docker", "TOOL_ENV_PASS=NO_PROXY,DOCKER_HOST", "DOCKER_HOST would give a tool what the script keeps from it"},
		{"pass docker config", "TOOL_ENV_PASS=DOCKER_CONFIG", "DOCKER_CONFIG would give"},
		{"pass containers", "TOOL_ENV_PASS=CONTAINERS_CONF", "CONTAINERS_CONF would give"},
		{"pass containerd", "TOOL_ENV_PASS=CONTAINERD_ADDRESS", "CONTAINERD_ADDRESS would give"},
		{"pass container host", "TOOL_ENV_PASS=CONTAINER_HOST", "CONTAINER_HOST would give"},
		{"pass kubeconfig", "TOOL_ENV_PASS=KUBECONFIG", "KUBECONFIG would give"},
		{"pass nerdctl", "TOOL_ENV_PASS=NERDCTL_TOML", "NERDCTL_TOML would give"},
		{"pass a node setting", "TOOL_ENV_PASS=CACTUSFS_LOG_LEVEL", "CACTUSFS_LOG_LEVEL would give"},
		{"pass path", "TOOL_ENV_PASS=PATH", "PATH would give"},
		{"pass home", "TOOL_ENV_PASS=HOME", "HOME would give"},
		{"pass a locale", "TOOL_ENV_PASS=LC_ALL", "LC_ALL would give"},
		{"pass ssh", "TOOL_ENV_PASS=SSH_ASKPASS", "SSH_ASKPASS would give"},
		{"pass the loader", "TOOL_ENV_PASS=LD_PRELOAD", "LD_PRELOAD would give"},
		{"pass the macOS loader", "TOOL_ENV_PASS=DYLD_INSERT_LIBRARIES", "DYLD_INSERT_LIBRARIES would give"},
		{"pass bash startup", "TOOL_ENV_PASS=BASH_ENV", "BASH_ENV would give"},
		{"pass xdg", "TOOL_ENV_PASS=XDG_CONFIG_HOME", "XDG_CONFIG_HOME would give"},
		{"pass a secret", "TOOL_ENV_PASS=AWS_SECRET_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY looks like a secret"},
		{"pass a token", "TOOL_ENV_PASS=AWS_SESSION_TOKEN", "AWS_SESSION_TOKEN looks like a secret"},
		{"pass a password", "TOOL_ENV_PASS=REGISTRY_PASSWORD", "REGISTRY_PASSWORD looks like a secret"},
		{"pass a variable of the script", "TOOL_ENV_PASS=SETTINGS", "SETTINGS is a variable or setting of the script itself"},
		{"pass a setting", "TOOL_ENV_PASS=DEPLOY_NAME", "DEPLOY_NAME is a variable or setting"},
		{"bind in cluster mode", "DEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\nBIND=0.0.0.0", "BIND=0.0.0.0: cannot be set in cluster mode"},
		{"key file without passphrase", "KMS_KEY_FILE=/etc/hosts", "KMS_KEY_FILE needs KMS_PASSPHRASE_FILE"},
		{"mode", "DEPLOY_MODE=ha", "DEPLOY_MODE=ha"},
		{"ssh option as host", "K3S_SSH=-oProxyCommand=touch", "K3S_SSH=-oProxyCommand=touch: must be user@host"},
		{"ssh code as host", "CE_HOST=admin@server.example.test;id", "CE_HOST="},
		{"ssh space", "BUILD_HOST=build host.example.test", "BUILD_HOST="},
		{"ssh substitution", "BUILD_HOST=$(id)@host", "BUILD_HOST="},
		{"node port", "K8S_NODE_PORT=8080", "K8S_NODE_PORT=8080"},
		{"size", "K8S_DRIVE_SIZE=10GB", "K8S_DRIVE_SIZE=10GB"},
		{"wait", "WAIT_TIMEOUT=0", "WAIT_TIMEOUT=0"},
		{"image needed", "IMAGE_SOURCE=image", "IMAGE_SOURCE=image needs IMAGE"},
		{"image form", "IMAGE=-x", "IMAGE=-x"},
		{"cert alone", "TLS_CERT_FILE=/c", "TLS_CERT_FILE and TLS_KEY_FILE are given together"},
		{"secret file missing", "ROOT_ACCESS_KEY_FILE=/nonexistent/a", "ROOT_ACCESS_KEY_FILE=/nonexistent/a: is not a file"},
		{"allow from", "K8S_S3_ALLOW_FROM=everyone", "K8S_S3_ALLOW_FROM=everyone"},
		{"yes or no", "ALLOW_DIRTY=true", "ALLOW_DIRTY=true: must be one of: yes no"},
		{"engine context", "ENGINE_CONTEXT=a b", "ENGINE_CONTEXT=a b"},
		{"offline install of docker", "OFFLINE=yes\nCE_INSTALL_DOCKER=yes", "OFFLINE=yes and CE_INSTALL_DOCKER=yes"},
		{"offline from source", "OFFLINE=yes\nIMAGE_SOURCE=source", "OFFLINE=yes and an image built from source"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				res := r.fresh(t).withEnv("KMS=local\n"+c.env+"\n", statusDD...)
				res.want(t, 2, c.want)
				if len(res.calls) != 0 {
					t.Fatalf("refused settings still ran %v", res.calls)
				}
			})
		}
		// --from-source with OFFLINE is refused too. On Kubernetes OFFLINE
		// is not refused: HELM_IMAGE (or HELM) must then be present, which
		// is asked where it is used (ADR-0033 section 12, PLAN 10.9).
		r.withEnv("KMS=local\nOFFLINE=yes\n", "--docker", "dd", "--install", "--from-source", "--dry-run").want(t, 2, "OFFLINE=yes and an image built from source")
		r.withEnv("KMS=local\nOFFLINE=yes\n", "--docker", "k3s", "--status", "--dry-run").want(t, 3)
		// The operator's own registry is not a public pull.
		r.withEnv("KMS=local\nOFFLINE=yes\nK8S_REGISTRY=registry.example.test/cactusfs\n", "--docker", "k3s", "--status", "--dry-run").want(t, 3)
		// A key file of one's own, with its passphrase; never with Vault.
		kms := r.envFile("kms.json", "{}")
		pass := r.envFile("pass", "p")
		res := r.withEnv("KMS=local\nKMS_KEY_FILE="+kms+"\nKMS_PASSPHRASE_FILE="+pass+"\n", statusDD...)
		res.want(t, statusDone)
		res.setting(t, "KMS_KEY_FILE", kms, "file")
		r.withEnv("KMS=vault\nVAULT_ADDR=https://vault.example.test:8200\nVAULT_KEY=k\nVAULT_TOKEN_FILE="+pass+"\nKMS_KEY_FILE="+kms+"\nKMS_PASSPHRASE_FILE="+pass+"\n", statusDD...).want(t, 2, "KMS_KEY_FILE is the key file of KMS=local")
		// BIND stays at its default in cluster mode.
		r.withEnv("KMS=local\nDEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\nBIND=127.0.0.1\n", statusDD...).want(t, 3)
		// A target that the file names wrongly is refused (the flag of the
		// other cases would win over it).
		r.withEnv("KMS=local\nDEPLOY_TARGET=swarm\n", "--status", "--dry-run").want(t, 2, "DEPLOY_TARGET=swarm: must be one of")
	})
}

// TestK3sImportIsWordsNotShell: the one command among the settings is split
// at spaces and never given to a shell; what a shell would read is refused.
func TestK3sImportIsWordsNotShell(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		mark := filepath.Join(r.dir, "ran")
		res := r.withEnv("KMS=local\nK3S_IMPORT=docker exec -i k3s-bench  k3s ctr -n k8s.io images import -\n", "--docker", "k3s", "--status", "--dry-run")
		res.want(t, 3, "K3S_IMPORT runs: [docker] [exec] [-i] [k3s-bench] [k3s] [ctr] [-n] [k8s.io] [images] [import] [-]")
		for _, value := range []string{
			"k3s ctr images import -; touch " + mark,
			"k3s $(touch " + mark + ")",
			"k3s `touch " + mark + "`",
			"k3s ctr | tee " + mark,
			"k3s ctr > " + mark,
			"k3s ctr & touch " + mark,
			"k3s 'ctr'",
			"k3s *",
		} {
			res := r.withEnv("KMS=local\nK3S_IMPORT="+value+"\n", "--docker", "k3s", "--status", "--dry-run")
			res.want(t, 2, "K3S_IMPORT="+value+": is a command split at spaces and run without a shell")
			if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("K3S_IMPORT=%q ran", value)
			}
		}
	})
}

// TestCactusfsSettings: a setting of the nodes passes when the example
// names it and it is neither a secret nor managed; every managed one the
// script names is a setting the example names, and is refused.
func TestCactusfsSettings(t *testing.T) {
	managed := heredoc(t, "env.sh", "MANAGED")
	example := cactusfsExample(t)
	for _, name := range managed {
		if _, ok := example[name]; !ok {
			t.Errorf("deploy/lib/env.sh manages %s, which deploy/cactusfs.env.example does not name", name)
		}
	}
	eachShell(t, func(t *testing.T, r *rig) {
		res := r.withEnv("KMS=local\nCACTUSFS_LOG_LEVEL=debug\nCACTUSFS_REGION=eu-test-1\nCACTUSFS_SCRUB_RATE_MIB=16\n", statusDD...)
		res.want(t, statusDone)
		if want := []string{"CACTUSFS_LOG_LEVEL=debug", "CACTUSFS_REGION=eu-test-1", "CACTUSFS_SCRUB_RATE_MIB=16"}; !slices.Equal(res.node, want) {
			t.Fatalf("passed to the nodes %q, want %q", res.node, want)
		}
		// A managed setting is refused as managed, or - when the example
		// or its name says it is a secret - as a secret.
		for _, name := range managed {
			res := r.withEnv("KMS=local\n"+name+"=x\n", statusDD...)
			res.want(t, 2, "site.env:2: "+name+" ")
			if !strings.Contains(res.stderr, name+" is set by the script itself") && !strings.Contains(res.stderr, name+" is a secret") && !strings.Contains(res.stderr, name+" looks like a secret") {
				t.Errorf("%s refused, but not as managed:\n%s", name, res.stderr)
			}
		}
		for name, secret := range example {
			if secret {
				res := r.withEnv("KMS=local\n"+name+"=x\n", statusDD...)
				res.want(t, 2, "site.env:2: "+name+" ")
				if !strings.Contains(res.stderr, name+" is a secret") && !strings.Contains(res.stderr, name+" looks like a secret") {
					t.Errorf("%s refused, but not as a secret:\n%s", name, res.stderr)
				}
			}
		}
	})
}

// TestTarget: --docker, else DEPLOY_TARGET, else what runs under the name;
// nothing, or more than one, is refused.
func TestTarget(t *testing.T) {
	ps := func(tool, out string) rule {
		return rule{Tool: tool, Contains: []string{"ps"}, Stdout: out}
	}
	// ns answers the question for a namespace of the deployment on that
	// target, and only that one.
	ns := func(tool, target string) rule {
		return rule{Tool: tool, Contains: []string{"namespaces", "io.cactusfs.deploy.name=cactusfs,io.cactusfs.deploy.target=" + target, "name"}, Stdout: "namespace/cactusfs\n"}
	}
	cases := []struct {
		name    string
		env     string
		args    []string
		rules   []rule
		without []string
		code    int
		want    []string
	}{
		{name: "flag", args: []string{"--docker", "podman"}, code: statusDone, want: []string{"target podman (from --docker)", "engine: podman (local)"}},
		{name: "flag form with equals", args: []string{"--docker=ce"}, code: statusDone, want: []string{"target ce (from --docker)"}},
		{name: "file", env: "DEPLOY_TARGET=k3s\n", code: 3, want: []string{"target k3s (from DEPLOY_TARGET)"}},
		{name: "flag over file", env: "DEPLOY_TARGET=k3s\n", args: []string{"--docker", "dd"}, code: statusDone, want: []string{"target dd (from --docker)"}},
		{name: "bad flag", args: []string{"--docker", "swarm"}, code: 2, want: []string{"DEPLOY_TARGET=swarm: must be one of"}},
		{name: "no value", args: []string{"--docker"}, code: 2, want: []string{"--docker needs a target"}},
		{name: "empty value", args: []string{"--docker", ""}, code: 2, want: []string{"--docker needs a target"}},
		{name: "empty value with equals", args: []string{"--docker="}, code: 2, want: []string{"--docker needs a target"}},
		{name: "empty in the file probes", env: "DEPLOY_TARGET=\n", code: 2, want: []string{"no target: nothing named cactusfs runs here"}},
		{name: "nothing runs", code: 2, want: []string{"no target: nothing named cactusfs runs here"}},
		{name: "docker desktop runs", rules: []rule{ps("docker", "dd\ndd\n")}, code: statusDone, want: []string{"target dd (from what runs)", "engine: docker --context default"}},
		{name: "rancher with moby runs", rules: []rule{ps("docker", "rancher\n")}, code: 3, want: []string{"target rancher (from what runs)"}},
		{name: "podman runs", rules: []rule{ps("podman", "podman\n")}, code: statusDone, want: []string{"target podman (from what runs)"}},
		{name: "nerdctl runs", rules: []rule{ps("nerdctl", "0123abcd\n")}, code: 3, want: []string{"target rancher (from what runs)"}},
		{
			name: "a namespace of the context", env: "K8S_CONTEXT=prod\n",
			rules: []rule{ns("kubectl", "k8s")}, code: 3,
			want: []string{"target k8s (from what runs)", "engine: kubectl --context prod"},
		},
		{
			name: "rancher's kubernetes in the context", env: "K8S_CONTEXT=rancher-desktop\n",
			rules: []rule{ns("kubectl", "rancher")}, code: 3,
			want: []string{"target rancher (from what runs)"},
		},
		{
			name: "no context, no kubectl asked", rules: []rule{ns("kubectl", "k8s")},
			code: 2, want: []string{"no target"},
		},
		{
			// A namespace whose target label is missing or another is not
			// taken for one: kubectl answers nothing to every question.
			name: "a namespace without the target label", env: "K8S_CONTEXT=prod\n",
			rules: []rule{{Tool: "kubectl", Contains: []string{"io.cactusfs.deploy.name=cactusfs"}, Stdout: ""}}, code: 2,
			want: []string{"no target"},
		},
		{
			name: "k3s over ssh", env: "K3S_SSH=admin@k3s.example.test\n",
			rules: []rule{ns("ssh", "k3s")}, code: 3,
			want: []string{"target k3s (from what runs)", "engine: k3s kubectl over ssh admin@k3s.example.test"},
		},
		{
			name: "two targets", env: "K8S_CONTEXT=prod\n",
			rules: []rule{ps("docker", "dd\n"), ns("kubectl", "k8s")}, code: 2,
			want: []string{"runs on more than one target (dd k8s)"},
		},
		{
			name: "a label with control characters", rules: []rule{ps("docker", "s\x1b[2Jwarm\n")}, code: 2,
			want: []string{"carries the target 's[2Jwarm'", "no target"},
		},
		{name: "unknown label", rules: []rule{ps("docker", "swarm\n")}, code: 2, want: []string{"carries the target 'swarm'", "no target"}},
		{name: "engine does not answer", rules: []rule{{Tool: "docker", Contains: []string{"ps"}, Stdout: "dd\n", Exit: 1}}, code: 2, want: []string{"no target"}},
		{name: "no engine installed", without: []string{"docker", "podman", "nerdctl", "kubectl"}, code: 2, want: []string{"no target"}},
		{name: "k8s needs its context", args: []string{"--docker", "k8s"}, code: 2, want: []string{"--docker k8s needs K8S_CONTEXT"}},
		{name: "k8s with its context", env: "K8S_CONTEXT=prod\n", args: []string{"--docker", "k8s"}, code: 3, want: []string{"engine: kubectl --context prod", "not built yet (PLAN 10.11)"}},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				rr := r.fresh(t)
				rr.rules(c.rules...)
				rr.without(c.without...)
				res := rr.withEnv("KMS=local\n"+c.env, append(append([]string{}, c.args...), "--status", "--dry-run")...)
				res.want(t, c.code, c.want...)
				if strings.Contains(res.output(), "\x1b") {
					t.Errorf("an engine's answer was printed with a control character:\n%q", res.output())
				}
				for _, call := range res.calls {
					switch call.Tool {
					case "docker", "podman", "nerdctl":
						if !slices.Contains(call.Args, "label=io.cactusfs.deploy.name=cactusfs") {
							t.Errorf("%s was not asked for the deployment's label: %q", call.Tool, call.Args)
						}
					case "kubectl":
						if !slices.Contains(call.Args, "--context") {
							t.Errorf("kubectl was run without a context: %q", call.Args)
						}
					}
				}
			})
		}
	})
}

// TestToolsSeeOnlyTheSettings: every tool runs under "env -i": it sees
// PATH, HOME and LC_ALL, and only what it needs besides (SSH_AUTH_SOCK for
// ssh, XDG_RUNTIME_DIR for podman) - never DOCKER_HOST, DOCKER_CONFIG,
// CONTAINERS_CONF, CONTAINERD_ADDRESS, KUBECONFIG, CACTUSFS_* or anything
// else of the caller's environment.
func TestToolsSeeOnlyTheSettings(t *testing.T) {
	allowed := map[string][]string{
		"":       {"PATH", "HOME", "LC_ALL"},
		"ssh":    {"SSH_AUTH_SOCK"},
		"podman": {"XDG_RUNTIME_DIR"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		vars := []string{
			"DOCKER_HOST=ssh://x", "DOCKER_CONTEXT=elsewhere", "DOCKER_TLS_VERIFY=1", "DOCKER_CERT_PATH=/x",
			"CONTAINER_HOST=ssh://x", "CONTAINER_CONNECTION=elsewhere", "CONTAINER_SSHKEY=/x", "KUBECONFIG=/x/config",
			"DOCKER_CONFIG=/x", "CONTAINERS_CONF=/x", "CONTAINERS_CONF_OVERRIDE=/x", "CONTAINERS_REGISTRIES_CONF=/x",
			"CONTAINERD_ADDRESS=/x", "CONTAINERD_NAMESPACE=x", "NERDCTL_TOML=/x", "CACTUSFS_LOG_LEVEL=debug",
			"CACTUSFS_IMAGE=registry.example.test/x", "HTTPS_PROXY=http://proxy.example.test", "SSH_AUTH_SOCK=/agent.sock",
			"XDG_RUNTIME_DIR=/run/user/1000", "XDG_STATE_HOME=" + r.dir + "/xdg", "LC_ALL=fr_FR.UTF-8", "LANG=fr_FR.UTF-8",
		}
		path := r.envFile("site.env", "KMS=local\nK8S_CONTEXT=prod\nK3S_SSH=admin@k3s.example.test\n")
		res := r.run([]string{"--env", path, "--status", "--dry-run"}, vars...)
		res.want(t, 2, "no target")
		tools := map[string]bool{}
		for _, c := range res.calls {
			tools[c.Tool] = true
			want := map[string]string{"PATH": r.bin, "HOME": r.home, "LC_ALL": "C"}
			switch c.Tool {
			case "ssh":
				want["SSH_AUTH_SOCK"] = "/agent.sock"
			case "podman":
				want["XDG_RUNTIME_DIR"] = "/run/user/1000"
			}
			for k, v := range c.Env {
				if !slices.Contains(allowed[""], k) && !slices.Contains(allowed[c.Tool], k) {
					t.Errorf("%s %q was run with %s=%s", c.Tool, c.Args, k, v)
				}
			}
			for k, v := range want {
				if c.Env[k] != v {
					t.Errorf("%s was run with %s=%q, want %q", c.Tool, k, c.Env[k], v)
				}
			}
		}
		for _, tool := range []string{"docker", "podman", "nerdctl", "kubectl", "ssh"} {
			if !tools[tool] {
				t.Errorf("%s was not asked, so this proves nothing of it", tool)
			}
		}
		// Without SSH_AUTH_SOCK and XDG_RUNTIME_DIR in the caller's
		// environment, none is made up.
		res = r.run([]string{"--env", path, "--status", "--dry-run"})
		res.want(t, 2, "no target")
		for _, c := range res.calls {
			for k := range c.Env {
				if !slices.Contains(allowed[""], k) {
					t.Errorf("%s was run with %s", c.Tool, k)
				}
			}
		}
		res = r.withEnv("KMS=local\nENGINE_CONTEXT=colima\n", "--docker", "dd", "--status", "--dry-run")
		res.want(t, statusDone, "engine: docker --context colima")

		// TOOL_ENV_PASS: every tool is given the variables it names that
		// the caller's environment holds, with their values as they are,
		// and nothing it does not name.
		pass := r.envFile("pass.env", "KMS=local\nK8S_CONTEXT=prod\nK3S_SSH=admin@k3s.example.test\nTOOL_ENV_PASS=HTTPS_PROXY,NO_PROXY,AWS_PROFILE,CLOUDSDK_CONFIG\n")
		passed := map[string]string{"HTTPS_PROXY": "http://proxy.example.test:3128", "NO_PROXY": ".example.test,10.0.0.0/8", "AWS_PROFILE": "deploy $(x) `y`"}
		extra := append(slices.Clone(vars), "HTTPS_PROXY="+passed["HTTPS_PROXY"], "NO_PROXY="+passed["NO_PROXY"], "AWS_PROFILE="+passed["AWS_PROFILE"])
		res = r.run([]string{"--env", pass, "--status", "--dry-run"}, extra...)
		res.want(t, 2, "no target")
		if len(res.calls) == 0 {
			t.Fatal("no tool was run, so this proves nothing")
		}
		for _, c := range res.calls {
			for k, v := range passed {
				if c.Env[k] != v {
					t.Errorf("%s was run with %s=%q, want %q", c.Tool, k, c.Env[k], v)
				}
			}
			for k := range c.Env {
				if _, ok := passed[k]; !ok && !slices.Contains(allowed[""], k) && !slices.Contains(allowed[c.Tool], k) {
					t.Errorf("%s was run with %s, which TOOL_ENV_PASS does not name", c.Tool, k)
				}
			}
			if _, ok := c.Env["CLOUDSDK_CONFIG"]; ok {
				t.Errorf("%s was given CLOUDSDK_CONFIG, which the caller's environment does not hold", c.Tool)
			}
		}
	})
}

// TestScriptVariablesAreListed: TOOL_ENV_PASS may not name a variable of the
// script, so the list of them in deploy/lib/env.sh holds every upper-case
// name the script assigns, and nothing else.
func TestScriptVariablesAreListed(t *testing.T) {
	listed := heredoc(t, "env.sh", "VARS")
	assign := regexp.MustCompile(`(?m)^[\t ]*(?:export )?([A-Z_][A-Z0-9_]*)=.*$`)
	// A pattern of a case arm, such as DOCKER_HOST=ssh://*), assigns nothing.
	casePattern := regexp.MustCompile(`\*\)`)
	var assigned []string
	for _, m := range assign.FindAllStringSubmatch(scriptCode(t), -1) {
		if casePattern.MatchString(m[0]) {
			continue
		}
		if !slices.Contains(assigned, m[1]) {
			assigned = append(assigned, m[1])
		}
	}
	sort.Strings(assigned)
	if len(assigned) < 10 {
		t.Fatalf("found %d assignments in the script; it was not read", len(assigned))
	}
	if !slices.Equal(assigned, listed) {
		t.Errorf("the script assigns %v\ndeploy/lib/env.sh lists %v", assigned, listed)
	}
}

// source runs the shell code with deploy/lib/common.sh sourced first, in
// the rig's environment.
func (r *rig) source(code string, extraEnv ...string) result {
	r.t.Helper()
	if err := os.Remove(r.transcript); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.t.Fatal(err)
	}
	argv := slices.Clone(r.sh.argv[1:])
	argv = append(argv, "-c", "set -euf; DRY_RUN=0; . \"$1\"; "+code, "sh", filepath.Join(r.repo, "deploy", "lib", "common.sh"))
	cmd := exec.CommandContext(r.t.Context(), r.sh.argv[0], argv...)
	cmd.Dir = r.work
	cmd.Env = append([]string{"PATH=" + r.bin, "HOME=" + r.home}, extraEnv...)
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

// TestToolsAndRemoteWords: what tool, on_host and host_key_check of
// deploy/lib/common.sh let through, called as later steps will call them.
func TestToolsAndRemoteWords(t *testing.T) {
	cases := []struct {
		name  string
		code  string
		exit  int
		want  string
		calls int
	}{
		{"an engine over ssh needs its host key checked", "tool DOCKER_HOST=ssh://a@b.example.test docker ps", 1, "the host key of a@b.example.test was not checked", 0},
		{"an engine over ssh after the check", "host_key_check a@b.example.test; tool DOCKER_HOST=ssh://a@b.example.test docker ps", 0, "", 2},
		{"a podman over ssh after the check", "host_key_check a@b.example.test; tool CONTAINER_HOST=ssh://a@b.example.test podman ps", 0, "", 2},
		{"another variable", "tool KUBECONFIG=/x kubectl get pods", 1, "not KUBECONFIG", 0},
		{"a command with an equals sign", "tool A=b", 1, "only DOCKER_HOST or CONTAINER_HOST", 0},
		{"on_host needs the check", "on_host a@b.example.test true", 1, "was not checked before it was used", 0},
		{"on_host an empty word", "host_key_check a@b; on_host a@b k3s ''", 2, "an empty word is not handed to ssh", 1},
		{"on_host an assignment first", "host_key_check a@b; on_host a@b PATH=/tmp k3s", 2, "'PATH=/tmp' would be read as an assignment", 1},
		{"on_host an option first", "host_key_check a@b; on_host a@b -v", 2, "is not a command this script hands to ssh", 1},
		{"on_host a metacharacter", "host_key_check a@b; on_host a@b k3s 'a;b'", 2, "'a;b' is not a word", 1},
		{"on_host an equals sign later", "host_key_check a@b; on_host a@b k3s kubectl -l a=b -", 0, "", 2},
		{"the check is made once", "host_key_check a@b; host_key_check a@b; on_host a@b true", 0, "", 2},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				r := r.fresh(t)
				res := r.source(c.code, "SSH_AUTH_SOCK=/agent.sock", "DOCKER_HOST=unix:///elsewhere")
				res.want(t, c.exit, c.want)
				if len(res.calls) != c.calls {
					t.Fatalf("%d calls, want %d: %v", len(res.calls), c.calls, res.calls)
				}
				for _, call := range res.calls {
					switch call.Tool {
					case "ssh":
						if call.Env["SSH_AUTH_SOCK"] != "/agent.sock" {
							t.Errorf("ssh without the agent: %v", call.Env)
						}
					case "docker", "podman":
						v := call.Env["DOCKER_HOST"] + call.Env["CONTAINER_HOST"]
						if v != "ssh://a@b.example.test" || call.Env["SSH_AUTH_SOCK"] != "/agent.sock" {
							t.Errorf("%s with %v", call.Tool, call.Env)
						}
					}
				}
			})
		}
	})
}

// TestSSH: every ssh is non-interactive, refuses unknown host keys, gives up
// on a host that does not answer, reads nothing of the terminal for a
// question, and is handed only plain words.
func TestSSH(t *testing.T) {
	want := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=20", "--", "admin@k3s.example.test"}
	empty := sha256.Sum256(nil)
	eachShell(t, func(t *testing.T, r *rig) {
		res := r.withEnv("KMS=local\nK3S_SSH=admin@k3s.example.test\n", "--status", "--dry-run")
		res.want(t, 2, "no target")
		var sshCalls []call
		for _, c := range res.calls {
			if c.Tool == "ssh" {
				sshCalls = append(sshCalls, c)
			}
		}
		if len(sshCalls) < 2 {
			t.Fatalf("ssh was run %d times, want the host key check and the questions", len(sshCalls))
		}
		if got := sshCalls[0].Args[len(want):]; !slices.Equal(got, []string{"true"}) {
			t.Errorf("the first ssh runs %q, want the host key check, true", got)
		}
		for _, c := range sshCalls {
			if len(c.Args) < len(want) || !slices.Equal(c.Args[:len(want)], want) {
				t.Errorf("ssh %q, want it to begin %q", c.Args, want)
			}
			if c.Stdin != hex.EncodeToString(empty[:]) {
				t.Errorf("ssh %q read something from its standard input", c.Args)
			}
			for _, w := range c.Args[len(want):] {
				if regexp.MustCompile(`[^A-Za-z0-9_./=:@+,-]`).MatchString(w) {
					t.Errorf("ssh was handed the word %q, which is not plain", w)
				}
			}
		}

		// A host key that ssh does not know: refused, with ssh's own
		// message, and nothing else is run there - nor anywhere after.
		r.rules(rule{Tool: "ssh", Contains: []string{"true"}, Stderr: "Host key verification failed.\n", Exit: 255})
		res = r.withEnv("KMS=local\nK3S_SSH=admin@k3s.example.test\n", "--status", "--dry-run")
		res.want(t, 2, "Host key verification failed.", "ssh to admin@k3s.example.test failed (exit 255", "known_hosts", "README.md")
		last := res.calls[len(res.calls)-1]
		if last.Tool != "ssh" || !slices.Equal(last.Args[len(want):], []string{"true"}) {
			t.Fatalf("a call after the refused host key: %v", res.calls)
		}
		var n int
		for _, c := range res.calls {
			if c.Tool == "ssh" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("ssh was run %d times after its host key was refused", n)
		}
	})
}

// TestSecretsNeverInTheTrace: with every secret given as a file, a run
// traced with -x prints none of them: no secret is read into the script.
func TestSecretsNeverInTheTrace(t *testing.T) {
	// Fixture secrets, made for the run: each is a value no output could
	// hold by chance.
	secrets := map[string]string{}
	for _, key := range []string{"ROOT_ACCESS_KEY_FILE", "ROOT_SECRET_KEY_FILE", "KMS_KEY_FILE", "KMS_PASSPHRASE_FILE", "VAULT_TOKEN_FILE", "TLS_KEY_FILE"} {
		secrets[key] = "fixture-" + rand.Text()
	}
	eachShell(t, func(t *testing.T, r *rig) {
		files := ""
		for key, value := range secrets {
			files += key + "=" + r.envFile(strings.ToLower(key), value) + "\n"
		}
		local := "KMS=local\nTLS_CERT_FILE=/c.crt\n" + strings.ReplaceAll(files, "VAULT_TOKEN_FILE=", "#VAULT_TOKEN_FILE=")
		vault := "KMS=vault\nVAULT_ADDR=https://vault.example.test:8200\nVAULT_KEY=cactusfs\nTLS_CERT_FILE=/c.crt\n" +
			strings.ReplaceAll(strings.ReplaceAll(files, "KMS_KEY_FILE=", "#KMS_KEY_FILE="), "KMS_PASSPHRASE_FILE=", "#KMS_PASSPHRASE_FILE=")
		r.trace = true
		for _, run := range []struct {
			env  string
			args []string
		}{
			{local, []string{"--docker", "dd", "--install", "--dry-run"}},
			{local, []string{"--docker", "dd", "--install"}},
			{vault, []string{"--docker", "k3s", "--status"}},
			{vault, []string{"--uninstall", "--purge", "--purge-confirm", "cactusfs", "--dry-run"}},
		} {
			args := run.args
			res := r.withEnv(run.env, args...)
			if res.code != 3 && res.code != 2 {
				t.Fatalf("%v: exit status %d\n%s", args, res.code, res.output())
			}
			if !strings.Contains(res.stderr, "+ ") {
				t.Fatalf("the run was not traced:\n%s", res.stderr)
			}
			for key, value := range secrets {
				if strings.Contains(res.output(), value) {
					t.Errorf("%v: the secret of %s is in the output", args, key)
				}
			}
			for _, c := range res.calls {
				for _, a := range c.Args {
					for key, value := range secrets {
						if strings.Contains(a, value) {
							t.Errorf("the secret of %s is on the command line of %s", key, c.Tool)
						}
					}
				}
			}
		}
	})
}

// TestActions: one action a run, the flags that go with it, and each stub
// names the step that builds it. The actions built for a standalone
// deployment on Docker (PLAN 10.5) are TestDockerActions'.
func TestActions(t *testing.T) {
	cases := []struct {
		name string
		env  string
		args []string
		code int
		want string
	}{
		{"no action", "", []string{"--docker", "dd"}, 2, "say what to do"},
		{"two actions", "", []string{"--install", "--status"}, 2, "one action a run"},
		{"unknown flag", "", []string{"--status", "--force"}, 2, "unknown argument '--force'"},
		{"purge alone", "", []string{"--install", "--purge"}, 2, "--purge goes with --uninstall"},
		{"purge confirm alone", "", []string{"--uninstall", "--purge-confirm", "cactusfs"}, 2, "--purge-confirm goes with --purge"},
		{"purge confirm wrong name", "", []string{"--docker", "dd", "--uninstall", "--purge", "--purge-confirm", "other"}, 2, "is not the name of the deployment"},
		{"from source on status", "", []string{"--status", "--from-source"}, 2, "--from-source goes with --install"},
		{"ce over ssh", "CE_HOST=deploy@server.example.test\n", []string{"--docker", "ce", "--install", "--dry-run"}, 3, "not built yet (PLAN 10.7)"},
		{"rancher", "", []string{"--docker", "rancher", "--install"}, 3, "not built yet (PLAN 10.6)"},
		{"rancher kubernetes", "RANCHER_KUBERNETES=yes\nK8S_S3_ALLOW_FROM=192.0.2.0/24\n", []string{"--docker", "rancher", "--install"}, 3, "not built yet (PLAN 10.10)"},
		{"cluster on docker", "DEPLOY_MODE=cluster\nALLOW_SHARED_LOCAL_KMS=yes\n", []string{"--docker", "dd", "--install"}, 3, "not built yet (PLAN 10.8)"},
		{"k3s install", "K8S_S3_ALLOW_FROM=192.0.2.0/24\n", []string{"--docker", "k3s", "--install"}, 3, "not built yet (PLAN 10.10)"},
		{"k3s install, S3 published to nobody named", "", []string{"--docker", "k3s", "--install"}, 2, "K8S_S3_ALLOW_FROM must say who may reach it"},
		{"k3s purge", "", []string{"--docker", "k3s", "--uninstall", "--purge", "--purge-confirm", "cactusfs"}, 3, "not built yet (PLAN 10.11)"},
		{"purge confirm without its name", "", []string{"--docker", "dd", "--uninstall", "--purge", "--purge-confirm", "--dry-run"}, 2, "--purge-confirm needs the deployment's name, not '--dry-run'"},
		{"env without its file", "", []string{"--status", "--env"}, 2, "--env needs a file"},
		{"print namespace", "K8S_CONTEXT=prod\n", []string{"--docker", "k8s", "--print-namespace"}, 0, "deploy.cactusfs.io/owner-uid: '"},
		{"print namespace, docker", "", []string{"--docker", "dd", "--print-namespace"}, 2, "--print-namespace is for a Kubernetes target"},
		{"print namespace and install", "", []string{"--print-namespace", "--install"}, 2, "one action a run"},
		{"docker empty", "", []string{"--docker", "", "--status"}, 2, "--docker needs a target"},
		{"docker empty with equals", "", []string{"--docker=", "--status"}, 2, "--docker needs a target"},
		{"env empty", "", []string{"--env", "", "--status"}, 2, "--env needs a file"},
		{"env empty with equals", "", []string{"--env=", "--status"}, 2, "--env needs a file"},
		{"purge confirm empty", "", []string{"--docker", "dd", "--uninstall", "--purge", "--purge-confirm", ""}, 2, "--purge-confirm needs the deployment's name"},
		{"purge confirm empty with equals", "", []string{"--docker", "dd", "--uninstall", "--purge", "--purge-confirm="}, 2, "--purge-confirm needs the deployment's name"},
		{"docker with a line", "", []string{"--docker", "dd\nKMS=vault", "--status"}, 2, "its value has a line break"},
		{"docker with a line and equals", "", []string{"--docker=dd\nIMAGE_SOURCE=image", "--status"}, 2, "its value has a line break"},
		{"docker with a carriage return", "", []string{"--docker=dd\r", "--status"}, 2, "its value has a line break"},
		{"env with a line", "", []string{"--env=a\nb", "--status"}, 2, "its value has a line break"},
		{"purge confirm with a line", "", []string{"--docker", "dd", "--uninstall", "--purge", "--purge-confirm=cactusfs\nx"}, 2, "its value has a line break"},
	}
	eachShell(t, func(t *testing.T, r *rig) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				res := r.fresh(t).withEnv("KMS=local\n"+c.env, c.args...)
				res.want(t, c.code, c.want)
				if len(res.calls) != 0 {
					t.Fatalf("a stub ran %v", res.calls)
				}
			})
		}
	})
}

// TestDryRunChangesNothing: with --dry-run nothing but read-only questions
// is asked of any tool, and no file is written - not the state, not the
// repository, not the home directory.
func TestDryRunChangesNothing(t *testing.T) {
	// The read-only questions, by tool: each is the words of one. An
	// engine's subcommand is matched where it stands - its first words
	// after the Docker CLI's "--context X" (sub, docker_test.go) - so a word
	// that only appears among the arguments of another command does not
	// pass. curl only probes a port or /readyz (PLAN 10.5): it sends nothing.
	engines := map[string][][]string{
		"docker":  {{"ps"}, {"volume", "ls"}, {"volume", "inspect"}, {"container", "inspect"}, {"info"}, {"image", "inspect"}},
		"podman":  {{"ps"}},
		"nerdctl": {{"ps"}},
	}
	others := map[string][]string{
		"kubectl": {"get"},
		"ssh":     {"get"},
		"curl":    {"-o", "/dev/null"},
	}
	asked := func(c call) bool {
		if words, ok := others[c.Tool]; ok {
			return containsAll(c.Args, words) &&
				!slices.Contains(c.Args, "-X") && !slices.Contains(c.Args, "-T") && !slices.Contains(c.Args, "-d")
		}
		s := sub(c)
		for _, words := range engines[c.Tool] {
			if len(s) >= len(words) && slices.Equal(s[:len(words)], words) {
				return true
			}
		}
		return false
	}
	eachShell(t, func(t *testing.T, r *rig) {
		r.rules(rule{Tool: "docker", Contains: []string{"ps"}, Stdout: "dd\n"})
		path := r.envFile("site.env", "KMS=local\nSTATE_DIR="+filepath.Join(r.dir, "state")+"\n")
		before := tree(t, r.dir)
		for _, run := range []struct {
			args []string
			code int
		}{
			// An install stops where the image is resolved (PLAN 10.4).
			{[]string{"--install", "--dry-run"}, 3},
			{[]string{"--install", "--from-source", "--dry-run"}, 3},
			{[]string{"--status", "--dry-run"}, statusDone},
			{[]string{"--uninstall", "--dry-run"}, 0},
			// A purge with no record of the deployment is refused (PLAN 10.5).
			{[]string{"--uninstall", "--purge", "--purge-confirm", "cactusfs", "--dry-run"}, 2},
		} {
			args := run.args
			res := r.run(append([]string{"--env", path}, args...))
			res.want(t, run.code, "dry run: nothing that changes anything is run", "target dd (from what runs)")
			if len(res.calls) == 0 {
				t.Fatal("no tool was asked what runs, so this proves nothing")
			}
			for _, c := range res.calls {
				if !asked(c) {
					t.Errorf("%v: %s %q is not a read-only question", args, c.Tool, c.Args)
				}
			}
			if after := tree(t, r.dir); !equalTrees(before, after) {
				t.Fatalf("%v changed files: before %v, after %v", args, before, after)
			}
		}
	})
}

// tree is every file and directory under dir, with a hash of each file;
// the transcript and the rules of the fakes are left out.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()
	err = fs.WalkDir(fsys, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case rel == transcriptName || rel == rulesName || rel == "site.env":
			return nil
		case d.IsDir():
			out[rel] = "dir"
		case d.Type()&fs.ModeSymlink != 0:
			out[rel] = "link"
		default:
			data, err := fs.ReadFile(fsys, rel)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			out[rel] = hex.EncodeToString(sum[:])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func equalTrees(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestStateRecordsTheEngine: a deployment's state says which engine or
// cluster it was made on, and a run whose settings point elsewhere is
// refused.
func TestStateRecordsTheEngine(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		state := filepath.Join(r.home, ".local", "state", "cactusfs-deploy", "dd-default", "cactusfs")
		res := r.withEnv("KMS=local\n", statusDD...)
		res.want(t, statusDone, "state: "+state)
		if err := os.MkdirAll(state, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "target"), []byte("docker --context default\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		r.withEnv("KMS=local\n", statusDD...).want(t, statusDone)
		if err := os.WriteFile(filepath.Join(state, "target"), []byte("docker --context $(touch x)\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		r.withEnv("KMS=local\n", statusDD...).want(t, 2, "was made on docker --context $(touch x); the settings now point at docker --context default")
		if _, err := os.Stat(filepath.Join(r.work, "x")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the recorded identity ran")
		}

		// The name recorded on another target, and not on this one, is
		// refused unless --another-target says so.
		if err := os.RemoveAll(state); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(r.home, ".local", "state", "cactusfs-deploy", "k8s-prod", "cactusfs")
		if err := os.MkdirAll(other, 0o700); err != nil {
			t.Fatal(err)
		}
		r.withEnv("KMS=local\n", statusDD...).want(t, 2, "recorded on another target (k8s-prod", "--another-target")
		r.withEnv("KMS=local\n", append([]string{"--another-target"}, statusDD...)...).want(t, statusDone)
		r.withEnv("KMS=local\nDEPLOY_NAME=second\n", statusDD...).want(t, statusDone)
		// A server over ssh is its own target: the destination is its ID.
		res = r.withEnv("KMS=local\nCE_HOST=deploy@server.example.test\n", "--docker", "ce", "--status", "--dry-run", "--another-target")
		res.want(t, 3, "state: "+filepath.Join(r.home, ".local", "state", "cactusfs-deploy", "ssh-deploy_server.example.test", "cactusfs"))
	})
}

// TestEnvFileStaysHere: deploy/.env is ignored by git and left out of the
// image's build context; the example is neither.
func TestEnvFileStaysHere(t *testing.T) {
	ignored := func(path string) bool {
		cmd := exec.CommandContext(t.Context(), "git", "check-ignore", "-q", "--no-index", path)
		cmd.Dir = repoRoot
		err := cmd.Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
			return true
		case errors.As(err, &exit) && exit.ExitCode() == 1:
			return false
		default:
			t.Fatalf("git check-ignore %s: %v", path, err)
			return false
		}
	}
	data := repoFile(t, ".dockerignore")
	var patterns []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			patterns = append(patterns, line)
		}
	}
	for _, c := range []struct {
		path string
		kept bool
	}{
		{"deploy/.env", false},
		{"deploy/site.env", false},
		{"deploy/.env.local", false},
		{"deploy/.env.example", true},
		{"deploy/deploy.sh", true},
	} {
		if got := ignored(c.path); got == c.kept {
			t.Errorf("git ignores %s: %v, want %v", c.path, got, !c.kept)
		}
		if got := dockerIgnored(patterns, c.path); got == c.kept {
			t.Errorf(".dockerignore leaves %s out: %v, want %v", c.path, got, !c.kept)
		}
	}
}

// dockerIgnored applies .dockerignore's patterns as the engines do: the
// last pattern that matches the path, or a directory above it, decides;
// "!" keeps; "**" is any number of directories.
func dockerIgnored(patterns []string, path string) bool {
	out := false
	for _, p := range patterns {
		keep := strings.HasPrefix(p, "!")
		p = strings.TrimPrefix(strings.TrimPrefix(p, "!"), "/")
		re := regexp.MustCompile("^" + globRegexp(p) + "(/.*)?$")
		if re.MatchString(path) {
			out = !keep
		}
	}
	return out
}

func globRegexp(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch {
		case strings.HasPrefix(p[i:], "**/"):
			b.WriteString("(.*/)?")
			i += 2
		case strings.HasPrefix(p[i:], "**"):
			b.WriteString(".*")
			i++
		case p[i] == '*':
			b.WriteString("[^/]*")
		case p[i] == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(p[i])))
		}
	}
	return b.String()
}

// TestEveryKeyIsReadAndExplained: the keys of deploy/.env.example and the
// script's table are the same, with the same defaults; every key of the
// table is read by the script, and every key the script reads is in it.
func TestEveryKeyIsReadAndExplained(t *testing.T) {
	table := map[string]string{}
	var order []string
	for _, line := range heredoc(t, "env.sh", "KEYS") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("a line of the table is not KEY=value: %q", line)
		}
		table[key] = value
		order = append(order, key)
	}

	data := repoFile(t, "deploy/.env.example")
	keyLine := regexp.MustCompile(`^(#?)([A-Z][A-Z0-9_]*)=(.*)$`)
	example := map[string]string{}
	var exampleOrder []string
	for i, line := range strings.Split(string(data), "\n") {
		m := keyLine.FindStringSubmatch(line)
		if m == nil {
			if strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "# ") && line != "#" {
				t.Errorf(".env.example:%d: a comment that is neither a key nor prose (\"# \"): %q", i+1, line)
			}
			continue
		}
		if _, dup := example[m[2]]; dup {
			t.Errorf(".env.example names %s twice", m[2])
		}
		example[m[2]] = m[3]
		exampleOrder = append(exampleOrder, m[2])
		switch {
		case m[2] == "KMS":
			if m[1] != "" || m[3] != "local" {
				t.Errorf(".env.example:%d: KMS must be written out as KMS=local, the explicit opt-in", i+1)
			}
		case m[1] == "":
			t.Errorf(".env.example:%d: %s is set; every line but KMS is commented out", i+1, m[2])
		}
	}
	if !slices.Equal(order, exampleOrder) {
		t.Errorf("the keys differ or are in another order:\n table   %v\n example %v", order, exampleOrder)
	}
	for key, def := range table {
		got, ok := example[key]
		switch {
		case !ok:
			t.Errorf("%s is in the table and not in .env.example", key)
		case key == "KMS":
			if def != "" {
				t.Errorf("KMS has the default %q; it must be stated", def)
			}
		case got != def:
			t.Errorf("%s: the default is %q, .env.example says %q", key, def, got)
		}
	}

	// What the script reads: every key word used in its code, outside the
	// table and comments.
	code := scriptCode(t)
	for key := range table {
		if !regexp.MustCompile(`\b` + key + `\b`).MatchString(code) {
			t.Errorf("the script never reads %s", key)
		}
	}
	reads := regexp.MustCompile(`\b(?:setting|one_of|yes_no|number|chars|dns_label|ssh_host|word|image_ref|size|allow_from|path|secret_file|bad|together|env_flag) "?([A-Z][A-Z0-9_]*)\b"?(?: ([A-Z][A-Z0-9_]*)\b)?`)
	var unknown []string
	for _, m := range reads.FindAllStringSubmatch(code, -1) {
		for _, key := range m[1:] {
			if key == "" {
				continue
			}
			if _, ok := table[key]; !ok && !slices.Contains(unknown, key) {
				unknown = append(unknown, key)
			}
		}
	}
	sort.Strings(unknown)
	if len(unknown) != 0 {
		t.Errorf("the script reads keys that are not in the table nor .env.example: %v", unknown)
	}

	// And the example itself is an env file the script takes.
	eachShell(t, func(t *testing.T, r *rig) {
		res := r.run([]string{"--env", filepath.Join(r.repo, "deploy", ".env.example"), "--docker", "dd", "--status", "--dry-run"})
		res.want(t, statusDone)
		for key := range table {
			if _, ok := res.settings[key]; !ok {
				t.Errorf("--dry-run does not show %s", key)
			}
		}
	})
}

// heredoc is the lines of the here-document of deploy/lib/<file> that ends
// with the word.
func heredoc(t *testing.T, file, word string) []string {
	t.Helper()
	data := repoFile(t, "deploy/lib/"+file)
	_, rest, ok := strings.Cut(string(data), "<<'"+word+"'\n")
	if !ok {
		t.Fatalf("deploy/lib/%s has no here-document %s", file, word)
	}
	body, _, ok := strings.Cut(rest, "\n"+word+"\n")
	if !ok {
		t.Fatalf("the here-document %s of deploy/lib/%s does not end", word, file)
	}
	return strings.Split(body, "\n")
}

// scriptCode is the code of the script - deploy.sh and deploy/lib/*.sh -
// without comments and without the table of keys.
func scriptCode(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, f := range deployFiles(t) {
		if !strings.HasSuffix(f, ".sh") {
			continue
		}
		text := string(repoFile(t, f))
		if before, rest, ok := strings.Cut(text, "<<'KEYS'\n"); ok {
			_, after, _ := strings.Cut(rest, "\nKEYS\n")
			text = before + after
		}
		for _, line := range strings.Split(text, "\n") {
			if trimmed := strings.TrimSpace(line); !strings.HasPrefix(trimmed, "#") {
				b.WriteString(line + "\n")
			}
		}
	}
	return b.String()
}

// cactusfsExample is every setting deploy/cactusfs.env.example names, and
// whether its explanation calls it a secret.
func cactusfsExample(t *testing.T) map[string]bool {
	t.Helper()
	data := repoFile(t, "deploy/cactusfs.env.example")
	out := map[string]bool{}
	var text string
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "#CACTUSFS_"):
			name, _, _ := strings.Cut(line[1:], "=")
			out[name] = strings.Contains(text, "A secret,") || strings.Contains(text, "holds a secret")
			text = ""
		case strings.HasPrefix(line, "# "):
			text += " " + line[2:]
		default:
			text = ""
		}
	}
	if len(out) < 40 {
		t.Fatalf("deploy/cactusfs.env.example names %d settings; it was not read", len(out))
	}
	return out
}
