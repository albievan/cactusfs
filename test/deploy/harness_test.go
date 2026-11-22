//go:build deployscript

package deploy_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeTools are the programs the script may run that touch a machine - and
// git, which says which commit an image is built from (PLAN 10.4): each is
// this test binary under that name, and records what it was asked.
var fakeTools = []string{"docker", "podman", "nerdctl", "kubectl", "ssh", "k3s", "curl", "helm", "git"}

// optionalFakes are programs of the machine (hostTools) that a test may
// put a fake in place of, to make it fail as the machine's would not
// (rig.fake).
var optionalFakes = []string{"cmp"}

// hostTools are the programs of this machine the script may use; nothing
// else is on its PATH. A program the script starts using must be added here,
// or the tests fail with "not found" - which is the point. They and the
// shells are taken from systemDirs only, never from the caller's PATH, so
// that what the script is tested with is the system's own.
var hostTools = []string{
	"awk", "sed", "grep", "cat", "tr", "dirname", "sort", "head", "tail", "wc",
	"mkdir", "rm", "chmod", "mktemp", "id", "uname", "env", "ls", "date",
	"pwd", "printenv",
	// The secrets (PLAN 10.3): tar streams, comparing copies, writing a
	// file without writing over one, a checksum of the binary dev-init
	// runs from. The render (PLAN 10.9): copies, base64 of Secret values.
	"tar", "cmp", "ln", "mv", "find", "rmdir", "sha256sum", "shasum",
	"cp", "od", "base64",
	// The Docker targets (PLAN 10.5): the wait for /readyz, and the
	// certificate's expiry --status reads (openssl, where the system has
	// it; the script says so where it has not).
	"sleep", "openssl",
}

// systemDirs are where hostTools and the shells are looked for.
var systemDirs = []string{"/usr/bin", "/bin"}

// systemTool is the path of a program in systemDirs, or "" when none has it.
func systemTool(name string) string {
	for _, dir := range systemDirs {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return path
		}
	}
	return ""
}

// The transcript the fakes write and the rules they answer by are files of
// the rig's directory, the parent of the directory the script runs in -
// which is where the fakes find them: the script does not change directory
// (a fake that cannot find them fails the run).
const (
	transcriptName = "transcript.jsonl"
	rulesName      = "rules.json"
	stdinDir       = "stdin"
)

// call is one command a fake was asked to run, with its whole environment.
type call struct {
	Tool  string            `json:"tool"`
	Args  []string          `json:"args"`
	Stdin string            `json:"stdin_sha256"`
	Env   map[string]string `json:"env,omitempty"`
}

// rule is how a fake answers: the first rule of its tool whose words are
// all among the arguments decides what it prints and its exit status. A
// call that no rule matches prints nothing and succeeds. A rule with
// SaveStdin also keeps what the call read, in that file of the rig's
// directory (the last such call's). A rule with After holds only once an
// earlier call of the run, of the same tool, held all those words: an
// engine whose listing changes after a deletion (PLAN 10.5's purge). A rule with Files writes, for each
// option it names, the content into the path that follows that option among
// the arguments, as a program writes a file it is told to (--iidfile,
// --metadata-file, --digestfile). A rule with TermParent sends SIGTERM to
// the process that started the fake - the shell waiting on it - as a signal
// would reach it mid-command. SleepMs keeps the fake running that long, as
// a command that is slow to end (after what it keeps of its standard input,
// with SaveStdin, which tells a test it has started).
type rule struct {
	Tool       string            `json:"tool"`
	Contains   []string          `json:"contains"`
	After      []string          `json:"after,omitempty"`
	Stdout     string            `json:"stdout"`
	Stderr     string            `json:"stderr,omitempty"`
	Exit       int               `json:"exit"`
	SaveStdin  string            `json:"save_stdin,omitempty"`
	Files      map[string]string `json:"files,omitempty"`
	TermParent bool              `json:"term_parent,omitempty"`
	SleepMs    int               `json:"sleep_ms,omitempty"`
}

// terminate sends SIGTERM to the process.
func terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGTERM)
}

// earlier is true when a call of the transcript before the last - the
// fake's own, written first - is of the tool and holds every word.
func earlier(rig *os.Root, tool string, words []string) (bool, error) {
	data, err := rig.ReadFile(transcriptName)
	if err != nil {
		return false, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, line := range lines[:len(lines)-1] {
		var c call
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return false, err
		}
		if c.Tool == tool && containsAll(c.Args, words) {
			return true, nil
		}
	}
	return false, nil
}

func TestMain(m *testing.M) {
	if name := filepath.Base(os.Args[0]); slices.Contains(fakeTools, name) || slices.Contains(optionalFakes, name) {
		os.Exit(fake(name, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fake records the call and answers it by the rules.
func fake(name string, args []string) int {
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake %s: read stdin: %v\n", name, err)
		return 125
	}
	sum := sha256.Sum256(in)
	c := call{Tool: name, Args: args, Stdin: hex.EncodeToString(sum[:]), Env: map[string]string{}}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		c.Env[k] = v
	}
	line, err := json.Marshal(c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake %s: %v\n", name, err)
		return 125
	}
	rig, err := os.OpenRoot("..")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake %s: the rig's directory: %v\n", name, err)
		return 125
	}
	defer func() { _ = rig.Close() }()
	f, err := rig.OpenFile(transcriptName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake %s: transcript: %v\n", name, err)
		return 125
	}
	_, werr := f.Write(append(line, '\n'))
	if err := errors.Join(werr, f.Close()); err != nil {
		fmt.Fprintf(os.Stderr, "fake %s: transcript: %v\n", name, err)
		return 125
	}
	// A rig with a directory stdinDir keeps what each call read, by its
	// hash, for a test that looks inside (rig.keepStdin).
	if info, err := rig.Stat(stdinDir); err == nil && info.IsDir() && len(in) > 0 {
		if err := rig.WriteFile(stdinDir+"/"+c.Stdin, in, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "fake %s: stdin: %v\n", name, err)
			return 125
		}
	}
	data, err := rig.ReadFile(rulesName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake %s: rules: %v\n", name, err)
		return 125
	}
	var rules []rule
	if err := json.Unmarshal(data, &rules); err != nil {
		fmt.Fprintf(os.Stderr, "fake %s: rules: %v\n", name, err)
		return 125
	}
	for _, r := range rules {
		if r.Tool != name || !containsAll(args, r.Contains) {
			continue
		}
		if len(r.After) > 0 {
			ok, err := earlier(rig, name, r.After)
			if err != nil {
				fmt.Fprintf(os.Stderr, "fake %s: transcript: %v\n", name, err)
				return 125
			}
			if !ok {
				continue
			}
		}
		if r.SaveStdin != "" {
			if err := rig.WriteFile(r.SaveStdin, in, 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "fake %s: keep stdin: %v\n", name, err)
				return 125
			}
		}
		for option, content := range r.Files {
			if i := slices.Index(args, option); i >= 0 && i+1 < len(args) {
				// Written through a root opened on the file's directory, so
				// the name the script gives cannot reach outside it.
				dir, err := os.OpenRoot(filepath.Dir(args[i+1]))
				if err != nil {
					fmt.Fprintf(os.Stderr, "fake %s: write %s: %v\n", name, option, err)
					return 125
				}
				err = dir.WriteFile(filepath.Base(args[i+1]), []byte(content), 0o600)
				_ = dir.Close()
				if err != nil {
					fmt.Fprintf(os.Stderr, "fake %s: write %s: %v\n", name, option, err)
					return 125
				}
			}
		}
		if r.TermParent {
			if err := terminate(os.Getppid()); err != nil {
				fmt.Fprintf(os.Stderr, "fake %s: signal the shell: %v\n", name, err)
				return 125
			}
		}
		time.Sleep(time.Duration(r.SleepMs) * time.Millisecond)
		if _, err := io.WriteString(os.Stdout, r.Stdout); err != nil {
			return 125
		}
		if _, err := io.WriteString(os.Stderr, r.Stderr); err != nil {
			return 125
		}
		return r.Exit
	}
	return 0
}

func containsAll(args, words []string) bool {
	for _, w := range words {
		if !slices.Contains(args, w) {
			return false
		}
	}
	return true
}

// repoRoot is the repository this package is in.
const repoRoot = "../.."

// shell is a shell the script is run under.
type shell struct {
	name string
	argv []string
}

// shells are the shells every test runs the script under: dash (Debian's
// /bin/sh), bash, and bash as macOS's /bin/sh runs it. A missing one fails
// the test: it is never left out.
func shells(t *testing.T) []shell {
	t.Helper()
	var out []shell
	for _, s := range []shell{
		{name: "dash", argv: []string{"dash"}},
		{name: "bash", argv: []string{"bash"}},
		{name: "bash-posix", argv: []string{"bash", "--posix"}},
	} {
		path := systemTool(s.argv[0])
		if path == "" {
			t.Fatalf("%s is not installed in %v: the script is tested under dash and bash, both are needed (the builder image has them)", s.argv[0], systemDirs)
		}
		s.argv[0] = path
		out = append(out, s)
	}
	return out
}

// eachShell runs f as a parallel subtest for every shell, each with a rig
// of its own. A test that runs its cases as parallel subtests gives each
// case a rig of its own too (rig.fresh): two runs never share a directory,
// a transcript or the rules of the fakes.
func eachShell(t *testing.T, f func(t *testing.T, r *rig)) {
	t.Helper()
	for _, sh := range shells(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			f(t, newRig(t, sh))
		})
	}
}

// fresh is a new rig for the subtest t, under the same shell.
func (r *rig) fresh(t *testing.T) *rig {
	t.Helper()
	return newRig(t, r.sh)
}

// rig is one place to run the script: a copy of deploy/ in a repository of
// its own, a PATH of fakes, a home directory and a transcript.
type rig struct {
	t  *testing.T
	sh shell
	// base is the directory the rig was made in as it was named, and dir
	// the same with its symbolic links resolved.
	base       string
	dir        string
	repo       string
	bin        string
	home       string
	work       string
	transcript string
	rulesFile  string
	// trace runs the script with "-x": the shell prints every command it
	// runs, with its words expanded, to standard error.
	trace bool
	// repoAs, when set, is the path of the repository the script is
	// called by instead of repo: the same directory by another name.
	repoAs string
	// input, when set, is what the run reads on its standard input.
	input string
}

func newRig(t *testing.T, sh shell) *rig {
	t.Helper()
	return newRigIn(t, sh, t.TempDir())
}

// newRigIn makes a rig in the directory base, which the test cleans up.
func newRigIn(t *testing.T, sh shell, base string) *rig {
	t.Helper()
	// The script finds itself with "pwd -P": the temporary directory with
	// its symbolic links resolved (on macOS /var is /private/var).
	dir, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{
		base:       base,
		t:          t,
		sh:         sh,
		dir:        dir,
		repo:       filepath.Join(dir, "repo"),
		bin:        filepath.Join(dir, "bin"),
		home:       filepath.Join(dir, "home"),
		work:       filepath.Join(dir, "work"),
		transcript: filepath.Join(dir, transcriptName),
		rulesFile:  filepath.Join(dir, rulesName),
	}
	for _, d := range []string{r.bin, r.home, r.work, filepath.Join(r.repo, "deploy", "lib")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	copyRoot, err := os.OpenRoot(r.repo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = copyRoot.Close() }()
	for _, f := range deployFiles(t) {
		if err := copyRoot.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := copyRoot.WriteFile(f, repoFile(t, f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range fakeTools {
		if err := os.Symlink(self, filepath.Join(r.bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range hostTools {
		path := systemTool(name)
		if path == "" {
			continue // not every system has every one; the script must not need it then
		}
		if err := os.Symlink(path, filepath.Join(r.bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	r.rules()
	return r
}

// repoFiles holds what repoFile has read, by path: the rigs of the
// parallel subtests copy the same files many times.
var repoFiles sync.Map

// repoFile is a file of the repository, by its path relative to it.
func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	if cached, ok := repoFiles.Load(rel); ok {
		if data, ok := cached.([]byte); ok {
			return data
		}
	}
	data := readRepoFile(t, rel)
	repoFiles.Store(rel, data)
	return data
}

func readRepoFile(t *testing.T, rel string) []byte {
	t.Helper()
	root, err := os.OpenRoot(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(rel)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// deployFiles are the files of the repository the script reads, relative
// to it: deploy/ itself, the Dockerfile (which pins the helm and runtime
// images, and builds the image) and internal/buildinfo/VERSION (the release
// an image built from source is). The chart it renders is copied only into
// a rig that renders (rig.chart).
func deployFiles(t *testing.T) []string {
	t.Helper()
	libs, err := filepath.Glob(filepath.Join(repoRoot, "deploy", "lib", "*.sh"))
	if err != nil || len(libs) == 0 {
		t.Fatalf("no deploy/lib/*.sh: %v", err)
	}
	files := []string{"deploy/deploy.sh", "deploy/cactusfs.env.example", "deploy/.env.example", "Dockerfile", "internal/buildinfo/VERSION"}
	for _, l := range libs {
		files = append(files, filepath.ToSlash(filepath.Join("deploy", "lib", filepath.Base(l))))
	}
	return files
}

// chart copies the chart into the rig's repository, for a test that renders.
func (r *rig) chart() {
	r.t.Helper()
	root, err := os.OpenRoot(r.repo)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, f := range chartFiles(r.t) {
		if err := root.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			r.t.Fatal(err)
		}
		if err := root.WriteFile(f, repoFile(r.t, f), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
}

// fake puts the test binary in place of a program of the machine (one of
// optionalFakes): it then records its calls and answers by the rules.
func (r *rig) fake(name string) {
	r.t.Helper()
	if !slices.Contains(optionalFakes, name) {
		r.t.Fatalf("%s is not among the programs a fake may replace", name)
	}
	self, err := os.Executable()
	if err != nil {
		r.t.Fatal(err)
	}
	path := filepath.Join(r.bin, name)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.t.Fatal(err)
	}
	if err := os.Symlink(self, path); err != nil {
		r.t.Fatal(err)
	}
}

// chartFiles are the files of the chart, deploy/helm/cactusfs, relative to
// the repository.
func chartFiles(t *testing.T) []string {
	t.Helper()
	root, err := os.OpenRoot(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	var files []string
	err = fs.WalkDir(root.FS(), "deploy/helm/cactusfs", func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			files = append(files, path)
		}
		return err
	})
	if err != nil || len(files) == 0 {
		t.Fatalf("no chart in deploy/helm/cactusfs: %v", err)
	}
	return files
}

// rules sets how the fakes answer.
func (r *rig) rules(rules ...rule) {
	r.t.Helper()
	if rules == nil {
		rules = []rule{}
	}
	data, err := json.Marshal(rules)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(r.rulesFile, data, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// without takes a fake off the PATH: that program is not installed.
func (r *rig) without(tools ...string) {
	r.t.Helper()
	for _, tool := range tools {
		if err := os.Remove(filepath.Join(r.bin, tool)); err != nil {
			r.t.Fatal(err)
		}
	}
}

// envFile writes an env file and returns its path.
func (r *rig) envFile(name, content string) string {
	r.t.Helper()
	path := filepath.Join(r.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
	return path
}

// result is what one run of the script did.
type result struct {
	code     int
	stdout   string
	stderr   string
	calls    []call
	settings map[string]setting
	node     []string
	// tmp is the TMPDIR the run was given (rig.image), or "".
	tmp string
}

type setting struct {
	value, from string
}

var (
	settingLine = regexp.MustCompile(`^deploy: setting ([A-Z][A-Z0-9_]*)=(.*) \((flag|file|default)\)$`)
	nodeLine    = regexp.MustCompile(`^deploy: node setting (CACTUSFS_[A-Z0-9_]+=.*)$`)
)

// run runs the script with the arguments, the environment of the rig and
// the extra variables given as NAME=value.
func (r *rig) run(args []string, extraEnv ...string) result {
	r.t.Helper()
	if err := os.Remove(r.transcript); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.t.Fatal(err)
	}
	argv := slices.Clone(r.sh.argv[1:])
	if r.trace {
		argv = append(argv, "-x")
	}
	repo := r.repo
	if r.repoAs != "" {
		repo = r.repoAs
	}
	argv = append(argv, filepath.Join(repo, "deploy", "deploy.sh"))
	argv = append(argv, args...)
	cmd := exec.CommandContext(r.t.Context(), r.sh.argv[0], argv...)
	cmd.Dir = r.work
	cmd.Env = append([]string{"PATH=" + r.bin, "HOME=" + r.home}, extraEnv...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	res := result{settings: map[string]setting{}}
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		res.code = exit.ExitCode()
	default:
		r.t.Fatalf("run the script: %v", err)
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	for _, line := range strings.Split(res.stdout, "\n") {
		if m := settingLine.FindStringSubmatch(line); m != nil {
			res.settings[m[1]] = setting{value: m[2], from: m[3]}
		}
		if m := nodeLine.FindStringSubmatch(line); m != nil {
			res.node = append(res.node, m[1])
		}
	}
	res.calls = r.calls()
	return res
}

func (r *rig) calls() []call {
	r.t.Helper()
	f, err := os.Open(r.transcript)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var calls []call
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c call
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			r.t.Fatalf("transcript: %v", err)
		}
		calls = append(calls, c)
	}
	if err := sc.Err(); err != nil {
		r.t.Fatal(err)
	}
	return calls
}

// output is everything the run printed.
func (res result) output() string {
	return res.stdout + res.stderr
}

// want fails the test unless the run ended with the code and printed every
// text given.
func (res result) want(t *testing.T, code int, texts ...string) {
	t.Helper()
	if res.code != code {
		t.Fatalf("exit status %d, want %d\nstdout:\n%s\nstderr:\n%s", res.code, code, res.stdout, res.stderr)
	}
	for _, text := range texts {
		if !strings.Contains(res.output(), text) {
			t.Fatalf("the output does not say %q\nstdout:\n%s\nstderr:\n%s", text, res.stdout, res.stderr)
		}
	}
}

// setting fails the test unless the run resolved the key to the value, from
// that source.
func (res result) setting(t *testing.T, key, value, from string) {
	t.Helper()
	got, ok := res.settings[key]
	if !ok {
		t.Fatalf("no setting %s in the output\nstdout:\n%s\nstderr:\n%s", key, res.stdout, res.stderr)
	}
	if got.value != value || got.from != from {
		t.Fatalf("%s = %q (%s), want %q (%s)", key, got.value, got.from, value, from)
	}
}
