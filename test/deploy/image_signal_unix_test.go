//go:build deployscript && unix

package deploy_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestImageInterrupt: a Ctrl-C - an INT to the script's whole process
// group, as a terminal sends it - during a slow step (a build) ends the run
// at once: the shell, the subshell of the work directory and the tool it
// runs all receive it (none of them ignores it), the run ends within two
// seconds, nothing after the step runs, and the work directory is gone. The
// script is started in a process group of its own, as a terminal's job is.
func TestImageInterrupt(t *testing.T) {
	eachShell(t, func(t *testing.T, r *rig) {
		f := newImgFix(imgCommit)
		r.imageBinaries(imgCommit, "amd64")
		r.rules(append(append([]rule{{Tool: "docker", Contains: []string{"buildx", "build"}, SaveStdin: "started", SleepMs: 30000}}, gitRules("")...),
			imgEngine("docker", "amd64", "amd64", f)...)...)
		envPath := r.envFile("site.env", "KMS=local\nDEPLOY_TARGET=dd\n")
		tmp := t.TempDir()
		argv := slices.Clone(r.sh.argv[1:])
		argv = append(argv, "-c", imagePrelude("0")+resolveAndDeliver+`; echo "went on"`, "sh", r.repo, envPath)
		cmd := exec.CommandContext(t.Context(), r.sh.argv[0], argv...)
		cmd.Dir = r.work
		cmd.Env = []string{"PATH=" + r.bin, "HOME=" + r.home, "TMPDIR=" + tmp}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		started := filepath.Join(r.dir, "started")
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := os.Stat(started); err == nil {
				break
			}
			if time.Now().After(deadline) {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				_ = cmd.Wait()
				t.Fatalf("the build was not reached:\n%s\n%s", stdout.String(), stderr.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		sent := time.Now()
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
		err := cmd.Wait()
		took := time.Since(sent)
		if took > 2*time.Second {
			t.Errorf("the run ended %v after the INT, want at once: something in it ignored the signal", took)
		}
		var exit *exec.ExitError
		if err == nil || !errors.As(err, &exit) {
			t.Errorf("the run ended with %v after an INT, want a failure", err)
		}
		if strings.Contains(stdout.String(), "went on") || strings.Contains(stdout.String(), "resolved ") {
			t.Errorf("the run went on after the INT:\n%s", stdout.String())
		}
		if left, err := os.ReadDir(tmp); err != nil || len(left) != 0 {
			t.Errorf("the run left %v in TMPDIR (%v)", left, err)
		}
		for _, c := range r.calls() {
			if slices.Contains(c.Args, "tag") {
				t.Errorf("a step after the build ran: %s %q", c.Tool, c.Args)
			}
		}
	})
}
