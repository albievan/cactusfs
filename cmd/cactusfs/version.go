package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// version is the release version. It is a build-time constant, set with
//
//	-ldflags "-X main.version=<version>"
//
// and never modified at run time.
var version = "dev"

// versionString describes the binary: release version, Go toolchain, target
// platform and, when the build embedded it, the source revision.
func versionString() string {
	s := fmt.Sprintf("cactusfs %s (%s %s/%s)", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if revision := vcsRevision(); revision != "" {
		s += " commit " + revision
	}
	return s
}

// vcsRevision returns the revision recorded by the Go toolchain, or "" when
// the binary was built outside a repository checkout.
func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if revision != "" && modified == "true" {
		revision += "-dirty"
	}
	return revision
}
