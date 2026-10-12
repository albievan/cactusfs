package main

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/domain/cluster"
)

// repository is the tree these tests read the committed documents from.
func repository() fs.FS { return os.DirFS("../..") }

func readRepository(t *testing.T, name string) string {
	t.Helper()
	content, err := fs.ReadFile(repository(), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(content)
}

// configOutput runs "cactusfs config" and returns what it printed.
func configOutput(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errs bytes.Buffer
	code = run(t.Context(), append([]string{"config"}, args...), &out, &errs, noEnv)
	return code, out.String(), errs.String()
}

// TestConfigCommand: the command prints the settings in one of two formats
// and refuses anything else as a mistake of the command line.
func TestConfigCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "the environment file by default", wantCode: exitOK, wantStdout: "#" + envS3Addr + "=" + defaultS3Addr + "\n"},
		{name: "the environment file", args: []string{"--format", "env"}, wantCode: exitOK, wantStdout: "#" + envKMSPhrase + "=\n"},
		{name: "the table", args: []string{"--format", "markdown"}, wantCode: exitOK, wantStdout: "| `--s3-addr` | `" + envS3Addr + "` | `" + defaultS3Addr + "` |"},
		{name: "an unknown format", args: []string{"--format", "yaml"}, wantCode: exitUsage, wantStderr: `unknown format "yaml": want env or markdown`},
		{name: "an unknown flag", args: []string{"--nope"}, wantCode: exitUsage, wantStderr: "flag provided but not defined"},
		{name: "a stray argument", args: []string{"env"}, wantCode: exitUsage, wantStderr: `unexpected argument "env"`},
		{name: "help", args: []string{"--help"}, wantCode: exitOK, wantStderr: "cactusfs config [--format env|markdown]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr := configOutput(t, tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d (stderr: %s)", code, tt.wantCode, stderr)
			}
			if !strings.Contains(stdout, tt.wantStdout) || (tt.wantStdout == "" && stdout != "") {
				t.Errorf("stdout = %q, want it to contain %q", stdout, tt.wantStdout)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}

	// The command is in the usage of the binary.
	if !strings.Contains(usage, "\n  config ") {
		t.Error("the usage of cactusfs does not name the config command")
	}
}

// failingWriter is an output that cannot be written to.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestConfigCommandReportsAnOutputThatFails: what could not be printed is an
// error, not a document that is silently short.
func TestConfigCommandReportsAnOutputThatFails(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	if code := runConfig(nil, failingWriter{}, &stderr); code != exitError || !strings.Contains(stderr.String(), "cactusfs config:") {
		t.Errorf("exit code %d, stderr %q", code, stderr.String())
	}
}

// TestSettingsDocumentsInSync: the documents that list the settings are what
// the binary prints. The list is declared once (serverSettings); a document
// that was edited by hand, or not written again after the list changed,
// fails here.
func TestSettingsDocumentsInSync(t *testing.T) {
	t.Parallel()
	const regenerate = "write it again with `make settings-docs` (or, with Go alone, `sh scripts/settings-docs.sh`)"

	code, env, _ := configOutput(t, "--format", formatEnv)
	if code != exitOK {
		t.Fatalf("config --format env = %d", code)
	}
	if got := readRepository(t, "deploy/cactusfs.env.example"); got != env {
		t.Errorf("deploy/cactusfs.env.example is not what `cactusfs config --format env` prints: %s", regenerate)
	}

	code, table, _ := configOutput(t, "--format", formatMarkdown)
	if code != exitOK {
		t.Fatalf("config --format markdown = %d", code)
	}
	for _, document := range []string{"README.md", "docs/ARCHITECTURE.md"} {
		text := readRepository(t, document)
		if strings.Count(text, "\n"+settingsBegin+"\n") != 1 || strings.Count(text, "\n"+settingsEnd+"\n") != 1 {
			t.Errorf("%s: want one line %s and one line %s", document, settingsBegin, settingsEnd)
			continue
		}
		_, rest, _ := strings.Cut(text, "\n"+settingsBegin+"\n")
		block, _, found := strings.Cut(rest, settingsEnd+"\n")
		if !found {
			t.Errorf("%s: %s does not follow %s", document, settingsEnd, settingsBegin)
			continue
		}
		if block != "\n"+table+"\n" {
			t.Errorf("%s: the table between %s and %s is not what `cactusfs config --format markdown` prints: %s",
				document, settingsBegin, settingsEnd, regenerate)
		}
		// No second table kept by hand beside it.
		if strings.Contains(strings.Replace(text, block, "", 1), "| `--s3-addr` |") {
			t.Errorf("%s lists settings outside the block the binary writes", document)
		}
	}

	// Every setting is in both documents, once.
	for _, spec := range serverSettings() {
		if strings.Count(env, "\n#"+spec.Env+"=") != 1 {
			t.Errorf("the environment file has %d lines for %s", strings.Count(env, "\n#"+spec.Env+"="), spec.Env)
		}
		if strings.Count(table, "| `"+spec.Env+"` |") != 1 {
			t.Errorf("the table has %d rows for %s", strings.Count(table, "| `"+spec.Env+"` |"), spec.Env)
		}
	}
}

// assignment is a line of an environment file that sets a variable, also
// when it is commented out.
var assignment = regexp.MustCompile(`(?m)^[ \t]*#?[ \t]*(CACTUSFS_[A-Z0-9_]+)=(.*)$`)

// TestExamplesCarryNoSecret: in the example files every line is commented
// out, and the line of a secret, or of a file that holds one, has no value:
// an example is copied as it is, and committed.
func TestExamplesCarryNoSecret(t *testing.T) {
	t.Parallel()
	secret := map[string]bool{}
	for _, spec := range serverSettings() {
		secret[spec.Env] = spec.holdsSecret()
	}
	for _, example := range []string{"deploy/cactusfs.env.example", "deploy/compose/.env.example"} {
		text := readRepository(t, example)
		found := 0
		for _, line := range strings.Split(text, "\n") {
			if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				t.Errorf("%s: a line that is not commented out: %q", example, line)
			}
		}
		for _, match := range assignment.FindAllStringSubmatch(text, -1) {
			name, value := match[1], strings.TrimSpace(match[2])
			if !secret[name] {
				continue
			}
			found++
			if value != "" {
				t.Errorf("%s: %s is a secret, or names a file that holds one, and has a value", example, name)
			}
		}
		if found == 0 {
			t.Errorf("%s: no line of a secret was found - the test no longer reads the file as it is written", example)
		}
	}
}

// gitIgnores reports whether the lines of a .gitignore at the root of the
// repository ignore file, a path with forward slashes. It reads what such a
// file of this repository is written with: comments, a "!" that takes a
// match back, a pattern without a slash that matches a name at any depth -
// a directory's too, and everything in it - and a pattern with one, which is
// matched from the root. The last pattern that matches decides.
func gitIgnores(lines []string, file string) (bool, error) {
	parts := strings.Split(file, "/")
	ignored := false
	for _, line := range lines {
		pattern, negated := strings.CutPrefix(strings.TrimRight(line, " "), "!")
		if pattern == "" || strings.HasPrefix(pattern, "#") {
			continue
		}
		directory := strings.HasSuffix(pattern, "/")
		pattern = strings.TrimSuffix(pattern, "/")
		matched := false
		// What matches a directory above the file matches the file; the
		// file itself is no directory.
		for depth := 1; depth <= len(parts) && !matched; depth++ {
			if directory && depth == len(parts) {
				break
			}
			var err error
			if strings.Contains(pattern, "/") {
				matched, err = path.Match(strings.TrimPrefix(pattern, "/"), strings.Join(parts[:depth], "/"))
			} else {
				matched, err = path.Match(pattern, parts[depth-1])
			}
			if err != nil {
				return false, fmt.Errorf("pattern %q: %w", line, err)
			}
		}
		if matched {
			ignored = !negated
		}
	}
	return ignored, nil
}

// dockerIgnores is gitIgnores for the lines of a .dockerignore, which are
// read otherwise: every pattern is matched from the root of the context,
// against the file and against each directory above it, "**" stands for any
// number of directories and "*" for no slash.
func dockerIgnores(lines []string, file string) (bool, error) {
	parts := strings.Split(file, "/")
	ignored := false
	for _, line := range lines {
		pattern, negated := strings.CutPrefix(strings.TrimSpace(line), "!")
		if pattern == "" || strings.HasPrefix(pattern, "#") {
			continue
		}
		var expression strings.Builder
		expression.WriteString("^")
		for rest := strings.Trim(pattern, "/"); rest != ""; {
			switch {
			case strings.HasPrefix(rest, "**/"):
				expression.WriteString("(.*/)?")
				rest = rest[len("**/"):]
			case strings.HasPrefix(rest, "**"):
				expression.WriteString(".*")
				rest = rest[len("**"):]
			case rest[0] == '*':
				expression.WriteString("[^/]*")
				rest = rest[1:]
			case rest[0] == '?':
				expression.WriteString("[^/]")
				rest = rest[1:]
			default:
				expression.WriteString(regexp.QuoteMeta(rest[:1]))
				rest = rest[1:]
			}
		}
		expression.WriteString("$")
		compiled, err := regexp.Compile(expression.String())
		if err != nil {
			return false, fmt.Errorf("pattern %q: %w", line, err)
		}
		for depth := 1; depth <= len(parts); depth++ {
			if compiled.MatchString(strings.Join(parts[:depth], "/")) {
				ignored = !negated
				break
			}
		}
	}
	return ignored, nil
}

// TestEnvironmentFilesAreIgnored: a file of variables is committed by nobody
// and enters no build context - `COPY . .` would otherwise put a local
// deploy/compose/.env, or the cactusfs.env the example tells its reader to
// make, into a layer of the builder - while the examples stay part of the
// source.
func TestEnvironmentFilesAreIgnored(t *testing.T) {
	t.Parallel()
	// A copy of an example, wherever it is made and whatever it is called.
	ignored := []string{
		".env", ".env.local", ".env.production.local", "deploy/compose/.env", "deploy/compose/.env.local",
		"cactusfs.env", "deploy/cactusfs.env", "deploy/compose/node1.env", "x/y/z.env", "x/y/.env",
	}
	// The examples, and what merely looks alike.
	kept := []string{
		"deploy/cactusfs.env.example", "deploy/compose/.env.example", ".env.example", "x/.env.local.example",
		"README.md", "cmd/cactusfs/environment.go", "docs/env/notes.md",
	}
	for file, read := range map[string]struct {
		want    []string
		ignores func([]string, string) (bool, error)
	}{
		".gitignore":    {want: []string{".env", ".env.*", "*.env", "!.env*.example"}, ignores: gitIgnores},
		".dockerignore": {want: []string{"**/.env", "**/.env.*", "*.env", "**/*.env", "!**/.env*.example"}, ignores: dockerIgnores},
	} {
		lines := strings.Split(readRepository(t, file), "\n")
		at := -1
		for _, pattern := range read.want {
			index := slices.Index(lines, pattern)
			switch {
			case index < 0:
				t.Errorf("%s has no line %q", file, pattern)
			case index < at:
				// The exception counts only after what it is an
				// exception to: the last pattern that matches decides.
				t.Errorf("%s: %q stands before the patterns it follows", file, pattern)
			}
			at = index
		}
		// Nothing after the exception takes it back.
		for _, line := range lines[at+1:] {
			if strings.Contains(line, ".example") || strings.Contains(line, ".env") {
				t.Errorf("%s: %q follows the exception for the examples", file, line)
			}
		}
		// And the lines do what they are there for.
		for _, name := range ignored {
			if got, err := read.ignores(lines, name); err != nil || !got {
				t.Errorf("%s does not ignore %s (%v)", file, name, err)
			}
		}
		for _, name := range kept {
			if got, err := read.ignores(lines, name); err != nil || got {
				t.Errorf("%s ignores %s (%v)", file, name, err)
			}
		}
	}
	for _, example := range []string{"deploy/cactusfs.env.example", "deploy/compose/.env.example"} {
		if readRepository(t, example) == "" {
			t.Errorf("%s is empty", example)
		}
	}

	// The two readers above read as the tools do.
	rules := []string{"# a comment", "/bin/", "*.out", "web/dist/", ".env", ".env.*", "*.env", "!.env*.example", "!kept.out"}
	for name, want := range map[string]bool{
		"bin/cactusfs": true, "cmd/bin/x": false, "a/b.out": true, "kept.out": false, "web/dist/index.html": true, "x/web/dist/a": false,
		"a/.env/b": true, "a.env.example": false, "bin": false,
	} {
		if got, err := gitIgnores(rules, name); err != nil || got != want {
			t.Errorf("gitIgnores(%q) = %t, %v; want %t", name, got, err, want)
		}
	}
	rules = []string{"# a comment", "bin", "**/*.test", "web/node_modules", "*.env", "**/.env.*", "!**/.env*.example"}
	for name, want := range map[string]bool{
		"bin/cactusfs": true, "cmd/bin/x": false, "a/b/c.test": true, "c.test": true, "web/node_modules/x/y.js": true,
		"a.env": true, "x/a.env": false, "x/.env.local": true, "x/.env.local.example": false,
	} {
		if got, err := dockerIgnores(rules, name); err != nil || got != want {
			t.Errorf("dockerIgnores(%q) = %t, %v; want %t", name, got, err, want)
		}
	}
	if _, err := gitIgnores([]string{"[a"}, "a"); err == nil {
		t.Error("a pattern that cannot be read is no error")
	}
}

// composeOnly are the variables of deploy/compose/compose.yaml that are no
// setting of a node: they are read by the container engine, for the image it
// builds and runs and for the ports it publishes on this machine.
func composeOnly() []string {
	return []string{
		"CACTUSFS_IMAGE", "CACTUSFS_BUILD", "CACTUSFS_COMMIT",
		"CACTUSFS_PORT_BASE_1", "CACTUSFS_PORT_BASE_2", "CACTUSFS_PORT_BASE_3", "CACTUSFS_PORT_BASE_4",
	}
}

// TestDeploymentVariablesAreSettings: the deployments name no variable the
// binary does not read, and the compose cluster's example lists exactly what
// its file can be told.
func TestDeploymentVariablesAreSettings(t *testing.T) {
	t.Parallel()
	variable := regexp.MustCompile(`CACTUSFS_[A-Z0-9]+(?:_[A-Z0-9]+)*`)
	settings := map[string]settingSpec{}
	for _, spec := range serverSettings() {
		settings[spec.Env] = spec
	}

	compose := readRepository(t, "deploy/compose/compose.yaml")
	for _, name := range variable.FindAllString(compose, -1) {
		if _, ok := settings[name]; !ok && !slices.Contains(composeOnly(), name) {
			t.Errorf("compose.yaml names %s, which is no setting of the binary and not one of the engine's (composeOnly)", name)
		}
	}
	for _, name := range composeOnly() {
		if _, ok := settings[name]; ok || !strings.Contains(compose, "${"+name+":") {
			t.Errorf("%s is listed as a variable of the engine: it is a setting, or compose.yaml does not read it", name)
		}
	}

	chart := os.DirFS("../../deploy/helm/cactusfs")
	found := 0
	err := fs.WalkDir(chart, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".tpl")) {
			return err
		}
		content, err := fs.ReadFile(chart, path)
		if err != nil {
			return err
		}
		for _, name := range variable.FindAllString(string(content), -1) {
			found++
			if _, ok := settings[name]; !ok {
				t.Errorf("the chart's %s names %s, which is no setting of the binary", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Error("no variable was found in the chart - the test no longer reads it")
	}

	// What compose.yaml reads from the shell or from .env is what its
	// example lists, no more and no less.
	interpolated := map[string]bool{}
	for _, match := range regexp.MustCompile(`\$\{(CACTUSFS_[A-Z0-9_]+)`).FindAllStringSubmatch(compose, -1) {
		interpolated[match[1]] = true
	}
	listed := map[string]bool{}
	for _, match := range assignment.FindAllStringSubmatch(readRepository(t, "deploy/compose/.env.example"), -1) {
		if listed[match[1]] {
			t.Errorf("deploy/compose/.env.example lists %s twice", match[1])
		}
		listed[match[1]] = true
	}
	if !maps.Equal(interpolated, listed) {
		t.Errorf("compose.yaml reads %v\nits .env.example lists %v", slices.Sorted(maps.Keys(interpolated)), slices.Sorted(maps.Keys(listed)))
	}
	// The values reach a node through its environment, put there by
	// compose - never as a file the engine reads for the container, which
	// would be a second place to look for what a node was told.
	if regexp.MustCompile(`(?m)^\s*env_file\s*:`).MatchString(compose) {
		t.Error("compose.yaml hands a container a file of variables (env_file)")
	}

	// A variable compose.yaml does not find reaches the node empty. That
	// must leave the node its default, for every setting: none may take
	// "set to nothing" for a value.
	empty := map[string]string{}
	for name := range settings {
		empty[name] = ""
	}
	empty[envKMSPhrase] = "unit test passphrase"
	cfg, sources, err := parseServerSettings([]string{"--dev-plaintext", "--data", t.TempDir(), "--kms", "local"}, envMap(empty), io.Discard)
	if err != nil {
		t.Fatalf("a node whose variables are all set to nothing: %v", err)
	}
	for name, spec := range settings {
		if sources[spec.Name] == cluster.SourceEnvironment && name != envKMSPhrase {
			t.Errorf("%s set to nothing counts as set", name)
		}
	}
	if cfg.ScrubRateMiB != defaultScrubRateMiB || cfg.GCGrace != defaultGCGrace || cfg.DriveLabels != nil {
		t.Errorf("a node whose variables are all set to nothing does not have its defaults: %+v", cfg)
	}
}

// TestComposeNamesItsVolumes: every node of the compose cluster is told what
// the engine calls the volume behind each of its drives - the project and
// the volume, in the order of the directories.
func TestComposeNamesItsVolumes(t *testing.T) {
	t.Parallel()
	compose := readRepository(t, "deploy/compose/compose.yaml")
	project := regexp.MustCompile(`(?m)^name: (\S+)$`).FindStringSubmatch(compose)
	if project == nil {
		t.Fatal("compose.yaml does not name its project")
	}
	directories := regexp.MustCompile(`(?m)^  CACTUSFS_DATA: (\S+)$`).FindStringSubmatch(compose)
	if directories == nil {
		t.Fatal("compose.yaml does not say where the drives are")
	}
	nodes := regexp.MustCompile(`(?m)^      CACTUSFS_NODE_ID: (\S+)$`).FindAllStringSubmatch(compose, -1)
	labels := regexp.MustCompile(`(?m)^      CACTUSFS_DRIVE_LABELS: (\S+)$`).FindAllStringSubmatch(compose, -1)
	if len(nodes) != 4 || len(labels) != len(nodes) {
		t.Fatalf("%d nodes, %d of them with labels for their drives", len(nodes), len(labels))
	}
	for i, node := range nodes {
		var want []string
		for _, directory := range strings.Split(directories[1], ",") {
			// /var/lib/cactusfs/data1 is where the volume <node>-data1
			// is mounted.
			volume := node[1] + "-" + directory[strings.LastIndex(directory, "/")+1:]
			if !strings.Contains(compose, "\n      - "+volume+":"+directory+"\n") {
				t.Errorf("%s does not mount the volume %s at %s", node[1], volume, directory)
			}
			if !strings.Contains(compose, "\n  "+volume+":\n") {
				t.Errorf("compose.yaml declares no volume %s", volume)
			}
			if label := project[1] + "_" + volume; validDriveLabel(label) {
				want = append(want, label)
			} else {
				t.Errorf("%q cannot be the label of a drive", label)
			}
		}
		if got := labels[i][1]; got != strings.Join(want, ",") {
			t.Errorf("%s: CACTUSFS_DRIVE_LABELS = %q, want %q", node[1], got, strings.Join(want, ","))
		}
	}
}

// TestChartNamesItsClaims: the chart tells every node the claim behind each
// drive as the StatefulSet names it, in the order of the directories. What
// the render says is asserted by scripts/helm-assert.sh; here the two lines
// of the template are held to each other.
func TestChartNamesItsClaims(t *testing.T) {
	t.Parallel()
	template := readRepository(t, "deploy/helm/cactusfs/templates/statefulset.yaml")
	const each = `{{- range $d := until (int $root.Values.drives.count) }}{{ if $d }},{{ end }}`
	for name, value := range map[string]string{
		envData:        each + `/var/lib/cactusfs/data{{ add $d 1 }}{{- end }}`,
		envDriveLabels: each + `data{{ add $d 1 }}-{{ $id }}-0{{- end }}`,
	} {
		if !strings.Contains(template, "- name: "+name+"\n              value: \""+value+"\"\n") {
			t.Errorf("the StatefulSet does not set %s to %s", name, value)
		}
	}
	for _, line := range []string{"  name: {{ $id }}\n", "        name: data{{ add $d 1 }}\n", "  replicas: 1\n"} {
		if !strings.Contains(template, line) {
			t.Errorf("the StatefulSet no longer has the line %q: a claim is <claim template>-<StatefulSet>-<ordinal>", line)
		}
	}
	assertion := readRepository(t, "scripts/helm-assert.sh")
	if !strings.Contains(assertion, `labels="$labels${labels:+,}data$i-$id-0"`) || !strings.Contains(assertion, `select(.name == \"`+envDriveLabels+`\") | .value" "$labels"`) {
		t.Error("scripts/helm-assert.sh does not assert the labels of the drives")
	}
}
