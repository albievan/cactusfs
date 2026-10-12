package buildinfo

import (
	"io/fs"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
)

// semver is the grammar of semver.org, version 2.0.0, without a leading "v".
var semver = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
	`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
	`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

func TestEmbeddedReleaseIsASemanticVersion(t *testing.T) {
	t.Parallel()

	if !strings.HasSuffix(embedded, "\n") || strings.Count(embedded, "\n") != 1 {
		t.Errorf("VERSION holds %q, want one line", embedded)
	}
	release := Release()
	if !semver.MatchString(release) {
		t.Errorf("VERSION names %q, which is not a semantic version without a leading v", release)
	}
	// What an image is tagged with and a pod is labelled with must be a
	// valid tag and label value: no build metadata ("+") in the release.
	if strings.Contains(release, "+") {
		t.Errorf("VERSION names %q: build metadata does not go into an image tag", release)
	}
}

// The release is written in two more places that cannot read the file: the
// default of the Dockerfile's VERSION argument, which labels an image built
// with no arguments, and the chart's appVersion. They are held to it here.
func TestReleaseIsTheSameWhereverItIsWritten(t *testing.T) {
	t.Parallel()

	repo := os.DirFS("../..")
	tests := []struct {
		file string
		line *regexp.Regexp
	}{
		{file: "Dockerfile", line: regexp.MustCompile(`(?m)^ARG VERSION=(\S*)$`)},
		{file: "deploy/helm/cactusfs/Chart.yaml", line: regexp.MustCompile(`(?m)^appVersion: "([^"]*)"$`)},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			t.Parallel()
			content, err := fs.ReadFile(repo, tt.file)
			if err != nil {
				t.Fatalf("read %s: %v", tt.file, err)
			}
			found := tt.line.FindAllSubmatch(content, -1)
			if len(found) == 0 {
				t.Fatalf("%s: no line matches %s", tt.file, tt.line)
			}
			for _, match := range found {
				if got := string(match[1]); got != Release() {
					t.Errorf("%s says %q, internal/buildinfo/VERSION says %q", tt.file, got, Release())
				}
			}
		})
	}
}

func TestResolve(t *testing.T) {
	t.Parallel()

	clean := []debug.BuildSetting{
		{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: "330028134523aabbccddeeff00112233445566ff"},
		{Key: "vcs.modified", Value: "false"},
	}
	modified := []debug.BuildSetting{
		{Key: "vcs.revision", Value: "330028134523aabbccddeeff00112233445566ff"},
		{Key: "vcs.modified", Value: "true"},
	}
	tests := []struct {
		name                   string
		version, build, commit string
		release                string
		settings               []debug.BuildSetting
		want                   Info
	}{
		{
			name: "everything injected", version: "1.2.3", build: "412", commit: "abcdef123456", release: "0.7.0\n", settings: modified,
			want: Info{Version: "1.2.3", Build: "412", Commit: "abcdef123456"},
		},
		{
			name: "a dirty commit injected is kept as it is", build: "412", commit: "abcdef123456-dirty", release: "0.7.0\n",
			want: Info{Version: "0.7.0", Build: "412", Commit: "abcdef123456-dirty"},
		},
		{
			name: "nothing injected, built in a checkout", release: "0.7.0\n", settings: clean,
			want: Info{Version: "0.7.0", Build: "0", Commit: "330028134523"},
		},
		{
			name: "nothing injected, built in a checkout that was changed", release: "0.7.0\n", settings: modified,
			want: Info{Version: "0.7.0", Build: "0", Commit: "330028134523-dirty"},
		},
		{
			name: "nothing injected, built outside a repository", release: "0.7.0\n",
			want: Info{Version: "0.7.0", Build: "0"},
		},
		{
			name: "arguments that were given empty count as not given", version: " ", build: "", commit: "\t", release: " 0.7.0 \n", settings: clean,
			want: Info{Version: "0.7.0", Build: "0", Commit: "330028134523"},
		},
		{
			name: "a short revision is not cut", release: "0.7.0", settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}},
			want: Info{Version: "0.7.0", Build: "0", Commit: "abc123"},
		},
		{
			name: "modified without a revision says nothing", release: "0.7.0", settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}},
			want: Info{Version: "0.7.0", Build: "0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolve(tt.version, tt.build, tt.commit, tt.release, tt.settings); got != tt.want {
				t.Errorf("resolve() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestInfoStrings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		info         Info
		details      []string
		wantString   string
		wantDescribe string
		wantTag      string
		wantNumbered bool
		wantDirty    bool
	}{
		{
			name: "a numbered build", info: Info{Version: "0.7.0", Build: "412", Commit: "3300281abcde"}, details: []string{"go1.27.2 linux/amd64"},
			wantString: "0.7.0 build 412 (commit 3300281abcde)", wantDescribe: "0.7.0 build 412 (commit 3300281abcde, go1.27.2 linux/amd64)",
			wantTag: "0.7.0-412", wantNumbered: true,
		},
		{
			name: "a numbered build of a changed tree", info: Info{Version: "0.7.0", Build: "412", Commit: "3300281abcde-dirty"},
			wantString: "0.7.0 build 412 (commit 3300281abcde-dirty)", wantDescribe: "0.7.0 build 412 (commit 3300281abcde-dirty)",
			wantTag: "0.7.0-412", wantNumbered: true, wantDirty: true,
		},
		{
			name: "a numbered build without a commit", info: Info{Version: "0.7.0", Build: "412"}, details: []string{"", "go1.27.2 linux/amd64"},
			wantString: "0.7.0 build 412", wantDescribe: "0.7.0 build 412 (go1.27.2 linux/amd64)",
			wantTag: "0.7.0-412", wantNumbered: true,
		},
		{
			name: "a local build", info: Info{Version: "0.7.0", Build: "0", Commit: "3300281abcde"}, details: []string{"go1.27.2 darwin/arm64"},
			wantString: "0.7.0 local build (commit 3300281abcde)", wantDescribe: "0.7.0 local build (commit 3300281abcde, go1.27.2 darwin/arm64)",
			wantTag: "0.7.0-0",
		},
		{
			name: "the zero build number is a local build too", info: Info{Version: "0.7.0"},
			wantString: "0.7.0 local build", wantDescribe: "0.7.0 local build", wantTag: "0.7.0-0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.info.String(); got != tt.wantString {
				t.Errorf("String() = %q, want %q", got, tt.wantString)
			}
			if got := tt.info.Describe(tt.details...); got != tt.wantDescribe {
				t.Errorf("Describe() = %q, want %q", got, tt.wantDescribe)
			}
			if got := tt.info.Tag(); got != tt.wantTag {
				t.Errorf("Tag() = %q, want %q", got, tt.wantTag)
			}
			if got := tt.info.Numbered(); got != tt.wantNumbered {
				t.Errorf("Numbered() = %v, want %v", got, tt.wantNumbered)
			}
			if got := tt.info.Dirty(); got != tt.wantDirty {
				t.Errorf("Dirty() = %v, want %v", got, tt.wantDirty)
			}
		})
	}
}

// Get is what the binary reports. However the test binary was built, the
// release is never empty, the build is a number and the commit, when there
// is one, is short.
func TestGet(t *testing.T) {
	t.Parallel()

	got := Get()
	if version == "" && got.Version != Release() {
		t.Errorf("Version = %q, want the release %q", got.Version, Release())
	}
	if got.Version == "" {
		t.Error("Version is empty")
	}
	if !regexp.MustCompile(`^\d+$`).MatchString(got.Build) {
		t.Errorf("Build = %q, want a number", got.Build)
	}
	if len(strings.TrimSuffix(got.Commit, dirtySuffix)) > commitLength && commit == "" {
		t.Errorf("Commit = %q, want at most %d characters of the hash", got.Commit, commitLength)
	}
}
