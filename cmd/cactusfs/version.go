package main

import (
	"runtime"

	"github.com/albievan/cactusfs/internal/buildinfo"
)

// versionString describes the binary: the release, the number of the build,
// the commit when the build knows it, the Go toolchain, the target platform
// and how the binary is packaged (buildinfo.PackagingImage or
// buildinfo.PackagingBinary), so that a binary taken out of an image can be
// told from one of build-all without running anything else.
func versionString(build buildinfo.Info, packaging string) string {
	return "cactusfs " + build.Describe(runtime.Version()+" "+runtime.GOOS+"/"+runtime.GOARCH, packaging)
}
