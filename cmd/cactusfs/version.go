package main

import (
	"runtime"

	"github.com/albievan/cactusfs/internal/buildinfo"
)

// versionString describes the binary: the release, the number of the build,
// the commit when the build knows it, the Go toolchain and the target
// platform.
func versionString(build buildinfo.Info) string {
	return "cactusfs " + build.Describe(runtime.Version()+" "+runtime.GOOS+"/"+runtime.GOARCH)
}
