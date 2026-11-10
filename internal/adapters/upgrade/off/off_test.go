package off_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/albievan/cactusfs/internal/adapters/upgrade/binary"
	"github.com/albievan/cactusfs/internal/adapters/upgrade/kubernetes"
	"github.com/albievan/cactusfs/internal/adapters/upgrade/off"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/internal/ports/porttest"
)

// TestTheUpgraderOfModeOff keeps the promises of an upgrader that cannot
// upgrade, says why in the words of ADR-0029, and gives the instructions it
// was given, whatever is done to what it hands out.
func TestTheUpgraderOfModeOff(t *testing.T) {
	t.Parallel()
	for name, instructions := range map[string][]string{
		"a binary":            binary.Instructions("/opt/cactusfs/cactusfs", "linux", "amd64"),
		"a binary on Windows": binary.Instructions(`C:\cactusfs\cactusfs.exe`, "windows", "amd64"),
		"the chart":           kubernetes.Instructions(),
		"compose":             off.ImageInstructions(),
		"nothing":             nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			u := off.New(instructions)
			if u.Mode() != release.ModeOff {
				t.Errorf("Mode = %s", u.Mode())
			}
			porttest.UnavailableUpgraderContract(t, u, porttest.FakeVerifiedBinary(t, "0.7.1"))
			ok, reason, got := u.Availability(t.Context())
			if ok || reason != off.Reason || !slices.Equal(got, instructions) {
				t.Errorf("Availability = %v, %q, %q", ok, reason, got)
			}
			if len(got) > 0 {
				got[0] = "changed"
				if _, _, again := u.Availability(t.Context()); !slices.Equal(again, instructions) {
					t.Error("a caller changed the instructions of the upgrader")
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := off.New(nil).Progress(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Progress after the caller went away = %v", err)
	}
}

// TestInstructionsNameTheDeployment: each kind of instructions names what
// its kind of deployment is upgraded with, and verifies the release first.
func TestInstructionsNameTheDeployment(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		lines []string
		want  []string
	}{
		"a binary": {binary.Instructions("/opt/cactusfs/cactusfs", "linux", "arm64"), []string{
			"cactusfs-linux-arm64", "cosign verify-blob", "SHA256SUMS", "'/opt/cactusfs/cactusfs.previous'",
		}},
		"a binary of an unknown path": {binary.Instructions("", "darwin", "arm64"), []string{"<the path of cactusfs>", "shasum -a 256"}},
		"a binary on Windows":         {binary.Instructions(`C:\cactusfs\cactusfs.exe`, "windows", "amd64"), []string{"Get-FileHash", "cactusfs.previous.exe"}},
		"the chart":                   {kubernetes.Instructions(), []string{"helm upgrade", "image.digest"}},
		"compose":                     {off.ImageInstructions(), []string{"digests.txt", "cosign verify-blob", "docker compose up"}},
	} {
		text := strings.Join(tt.lines, "\n")
		for _, want := range tt.want {
			if !strings.Contains(text, want) {
				t.Errorf("%s: the instructions do not name %q:\n%s", name, want, text)
			}
		}
	}
}
