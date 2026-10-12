// Package buildinfo says which build of CactusFS this is: the release, the
// number of the build and the commit it was made from.
//
// The release is named in one place, the file VERSION beside this one. It is
// embedded, so a plain `go build ./cmd/cactusfs` reports it, and the Makefile,
// the Dockerfile and the Helm chart are held to it. The build and the commit
// are known only to whoever builds - they are told with
//
//	-ldflags "-X github.com/albievan/cactusfs/internal/buildinfo.build=<n>
//	          -X github.com/albievan/cactusfs/internal/buildinfo.commit=<hash>"
//
// and when they are not, the commit is the one the Go toolchain recorded and
// the build is "0": a build that nobody numbered.
//
// The package has no state: the variables below are constants of the build,
// never modified at run time. internal/domain does not import it.
package buildinfo

import (
	_ "embed" // the release is embedded from VERSION
	"runtime/debug"
	"strings"
)

// Unnumbered is the build of a binary that was built without a number: with
// plain `go build`, or by an image build that was given none.
const Unnumbered = "0"

// commitLength is how much of a commit hash is shown.
const commitLength = 12

// dirtySuffix marks a commit whose tree had uncommitted changes to tracked
// files when the binary was built.
const dirtySuffix = "-dirty"

// embedded is the content of VERSION: the release, a semantic version
// without a leading "v".
//
//go:embed VERSION
var embedded string

// version, build and commit are build-time constants, set with -ldflags -X
// (see the package comment) and never modified at run time. Each is empty
// when the build did not set it.
var (
	version string
	build   string
	commit  string
)

// Info identifies a build.
type Info struct {
	// Version is the release: a semantic version without a leading "v".
	Version string
	// Build numbers the build: the count of commits it was made from, or
	// Unnumbered.
	Build string
	// Commit is the short hash of the commit, with "-dirty" when the tree
	// had uncommitted changes; empty when the build does not know it.
	Commit string
}

// Get returns the identity of the running binary.
func Get() Info {
	var settings []debug.BuildSetting
	if info, ok := debug.ReadBuildInfo(); ok {
		settings = info.Settings
	}
	return resolve(version, build, commit, embedded, settings)
}

// Release returns the release named in the embedded VERSION file.
func Release() string {
	return strings.TrimSpace(embedded)
}

// resolve composes an identity from what the build injected, with what the
// binary carries anyway where it injected nothing.
func resolve(injectedVersion, injectedBuild, injectedCommit, release string, settings []debug.BuildSetting) Info {
	info := Info{
		Version: strings.TrimSpace(injectedVersion),
		Build:   strings.TrimSpace(injectedBuild),
		Commit:  strings.TrimSpace(injectedCommit),
	}
	if info.Version == "" {
		info.Version = strings.TrimSpace(release)
	}
	if info.Build == "" {
		info.Build = Unnumbered
	}
	if info.Commit == "" {
		info.Commit = vcsCommit(settings)
	}
	return info
}

// vcsCommit returns the commit the Go toolchain recorded, or "" when the
// binary was built outside a repository checkout.
func vcsCommit(settings []debug.BuildSetting) string {
	var revision, modified string
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	if len(revision) > commitLength {
		revision = revision[:commitLength]
	}
	if revision != "" && modified == "true" {
		revision += dirtySuffix
	}
	return revision
}

// Numbered reports whether the build was given a number.
func (i Info) Numbered() bool {
	return i.Build != "" && i.Build != Unnumbered
}

// Dirty reports whether the binary was built from a tree with uncommitted
// changes.
func (i Info) Dirty() bool {
	return strings.HasSuffix(i.Commit, dirtySuffix)
}

// Tag returns "<version>-<build>": what an image of this build is tagged
// with and what telemetry reports.
func (i Info) Tag() string {
	build := i.Build
	if build == "" {
		build = Unnumbered
	}
	return i.Version + "-" + build
}

// String describes the build in words: "0.7.0 build 412 (commit
// 3300281abcde)", or "0.7.0 local build" for one without a number or a
// known commit.
func (i Info) String() string {
	return i.Describe()
}

// Describe is String with more inside the brackets after the commit: the
// Go toolchain and the platform, say.
func (i Info) Describe(details ...string) string {
	s := i.Version
	if i.Numbered() {
		s += " build " + i.Build
	} else {
		s += " local build"
	}
	var parts []string
	if i.Commit != "" {
		parts = append(parts, "commit "+i.Commit)
	}
	for _, detail := range details {
		if detail != "" {
			parts = append(parts, detail)
		}
	}
	if len(parts) == 0 {
		return s
	}
	return s + " (" + strings.Join(parts, ", ") + ")"
}
