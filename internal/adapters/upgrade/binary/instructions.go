package binary

import (
	"strings"

	"github.com/albievan/cactusfs/internal/adapters/releases/sigstore"
	"github.com/albievan/cactusfs/internal/domain/release"
)

// DownloadBase is where the files of the product's releases are published,
// up to the tag: the address the instructions download from.
const DownloadBase = "https://github.com/albievan/cactusfs/releases/download/v"

// instructions returns the commands that install a release by hand on this
// platform (Instructions).
func (u *Upgrader) instructions() []string {
	return manual{executable: u.cfg.Executable, previous: u.previous, goos: u.cfg.GOOS, goarch: u.cfg.GOARCH}.instructions()
}

// Instructions returns the commands that install a release by hand on the
// platform goos/goarch, for the executable at that path: download the
// binary and the signed SHA256SUMS, verify the list with cosign and the
// binary against it, stop the node, keep the old executable as the previous
// one, put the new one in its place, start the node. The version is left
// for the operator to fill in. An empty path is written as a placeholder.
// They are what an upgrader of another mode - upgrade-mode off - shows a
// binary that was shipped as one. The path is goos's, whatever the platform
// that writes them: the previous binary is named beside it with goos's
// separators, never the host's.
func Instructions(executable, goos, goarch string) []string {
	if executable == "" {
		executable = "<the path of cactusfs>"
	}
	ext := ""
	if strings.HasSuffix(strings.ToLower(executable), ".exe") || goos == "windows" {
		ext = ".exe"
	}
	return manual{
		executable: executable, previous: beside(executable, PreviousName+ext, goos), goos: goos, goarch: goarch,
	}.instructions()
}

// beside returns the path of the file name in the directory of path, a path
// of goos: up to its last separator - "/" or, on Windows, "\" too - kept as
// it is written. A path with no separator has name alone.
func beside(path, name, goos string) string {
	separators := "/"
	if goos == "windows" {
		separators = `/\`
	}
	return path[:strings.LastIndexAny(path, separators)+1] + name
}

// manual is what the instructions name.
type manual struct {
	executable, previous, goos, goarch string
}

// instructions are the commands of Instructions.
func (u manual) instructions() []string {
	asset := release.BinaryAsset(u.goos, u.goarch)
	if asset == "" {
		asset = "cactusfs-<os>-<arch>"
	}
	if u.goos == "windows" {
		return u.windowsInstructions(asset)
	}
	sum := "sha256sum --check --ignore-missing " + release.SumsAsset
	if u.goos == "darwin" {
		sum = "shasum -a 256 --check --ignore-missing " + release.SumsAsset
	}
	return []string{
		"# the release to install, without the leading v",
		"VERSION=<version>",
		`BASE="` + DownloadBase + `$VERSION"`,
		`curl -fL -O "$BASE/` + asset + `" -O "$BASE/` + release.SumsAsset + `" -O "$BASE/` + release.SumsBundleAsset + `"`,
		"# the checksums are signed by the release workflow at the tag of the version",
		"cosign verify-blob --new-bundle-format=true --bundle " + release.SumsBundleAsset +
			` --certificate-identity "` + sigstore.DefaultIdentityPrefix + `$VERSION"` +
			" --certificate-oidc-issuer " + sigstore.DefaultIssuer + " " + release.SumsAsset,
		"# the binary is the one they list",
		sum,
		"# stop the node, then keep the old binary and put the new one in its place",
		"cp -p " + shellQuote(u.executable) + " " + shellQuote(u.previous),
		"install -m 0755 " + asset + " " + shellQuote(u.executable),
		"# start the node again",
	}
}

// windowsInstructions are the instructions in PowerShell.
func (u manual) windowsInstructions(asset string) []string {
	return []string{
		"# the release to install, without the leading v",
		`$VERSION = "<version>"`,
		`$BASE = "` + DownloadBase + `$VERSION"`,
		`foreach ($f in "` + asset + `", "` + release.SumsAsset + `", "` + release.SumsBundleAsset + `") { Invoke-WebRequest -Uri "$BASE/$f" -OutFile $f }`,
		"# the checksums are signed by the release workflow at the tag of the version",
		"cosign verify-blob --new-bundle-format=true --bundle " + release.SumsBundleAsset +
			` --certificate-identity "` + sigstore.DefaultIdentityPrefix + `$VERSION"` +
			" --certificate-oidc-issuer " + sigstore.DefaultIssuer + " " + release.SumsAsset,
		"# the binary is the one they list: the two lines must show the same hash",
		`(Get-FileHash -Algorithm SHA256 ` + asset + `).Hash.ToLower()`,
		`Select-String -SimpleMatch "` + asset + `" ` + release.SumsAsset,
		"# stop the service, then keep the old binary and put the new one in its place",
		"Move-Item -Force " + powerShellQuote(u.executable) + " " + powerShellQuote(u.previous),
		"Move-Item " + asset + " " + powerShellQuote(u.executable),
		"# start the service again",
	}
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// powerShellQuote quotes s for PowerShell, as a string taken literally.
func powerShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
