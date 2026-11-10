//go:build integration && !windows

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/albievan/cactusfs/internal/adapters/releases/sigstore/sigstoretest"
	"github.com/albievan/cactusfs/internal/app"
	"github.com/albievan/cactusfs/internal/buildinfo"
	"github.com/albievan/cactusfs/internal/domain"
	"github.com/albievan/cactusfs/internal/domain/release"
	"github.com/albievan/cactusfs/test/integration/releasemirror"
)

// updatesVersions are the releases of TestUpdates, reckoned from the
// release this tree is (VERSION): the running one, three patches of it and
// a release two minor versions on.
type updatesVersions struct {
	// current is this tree: what the node runs first.
	current string
	// next is the next patch, built from this tree as that version: the
	// release that is installed.
	next string
	// tampered is a patch whose binary is not the one its SHA256SUMS
	// lists.
	tampered string
	// otherTag is a patch whose lists are signed for the tag of next.
	otherTag string
	// skipped is two minor versions on: never installed from current.
	skipped string
}

// updatesVersionsOf reckons the releases from this tree's.
func updatesVersionsOf(t *testing.T) updatesVersions {
	t.Helper()
	current := buildinfo.Release()
	parts := strings.Split(current, ".")
	if len(parts) != 3 {
		t.Fatalf("VERSION %q is not X.Y.Z", current)
	}
	n := make([]int, 3)
	for i, part := range parts {
		v, err := strconv.Atoi(part)
		if err != nil {
			t.Fatalf("VERSION %q is not X.Y.Z", current)
		}
		n[i] = v
	}
	patch := func(add int) string { return fmt.Sprintf("%d.%d.%d", n[0], n[1], n[2]+add) }
	return updatesVersions{
		current: current, next: patch(1), tampered: patch(2), otherTag: patch(3),
		skipped: fmt.Sprintf("%d.%d.0", n[0], n[1]+2),
	}
}

// buildBinaryAs compiles cmd/cactusfs as a build of release version, as the
// release workflow names it.
func buildBinaryAs(t *testing.T, version string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "cactusfs")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", bin,
		"-ldflags", "-X "+modulePath+"/internal/buildinfo.version="+version, modulePath+"/cmd/cactusfs")
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build as %s: %v\n%s", version, err, out)
	}
	return bin
}

// readFileIn reads a file through its directory, as writeFileIn writes
// one.
func readFileIn(t *testing.T, path string) []byte {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// fileSum is the SHA-256 of a file, in hex.
func fileSum(t *testing.T, path string) string {
	t.Helper()
	sum := sha256.Sum256(readFileIn(t, path))
	return hex.EncodeToString(sum[:])
}

// versionOf runs "<path> version" and returns its first line.
func versionOf(ctx context.Context, t *testing.T, path string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		t.Fatalf("%s version: %v", path, err)
	}
	return strings.SplitN(string(out), "\n", 2)[0]
}

// updatesLogLine is a line of a node's JSON log, as far as these tests read
// it.
type updatesLogLine struct {
	Msg      string `json:"msg"`
	Listener string `json:"listener"`
	Addr     string `json:"addr"`
}

// updatesNode is the binary, running by itself, whose log is read as it
// comes. Its standard output is a pipe of the test's own, not one of
// os/exec, so that the log is read across a re-execution and the process
// is waited for apart from it: exited is closed when the process of that
// PID has ended, which an execution in place never does.
type updatesNode struct {
	cmd    *exec.Cmd
	exe    string
	stderr *lockedBuffer
	root   map[string]string
	s3Key  [2]string

	exited  chan struct{}
	waitErr error

	mu      sync.Mutex
	lines   []updatesLogLine
	notJSON []string
	logEOF  bool
}

// startUpdatesNode starts exe as a node of its own on two drives in dir.
func startUpdatesNode(ctx context.Context, t *testing.T, exe, dir string, env ...string) *updatesNode {
	t.Helper()
	accessKey, secretKey := "AKIDUPDATESROOT00000", strings.Repeat("updates-root-", 3)+"0123456789"
	cmd := exec.CommandContext(ctx, exe, "server",
		"--data", filepath.Join(dir, "drive1"), "--data", filepath.Join(dir, "drive2"), "--kms", "local",
		"--admin-addr", "127.0.0.1:0", "--s3-addr", "127.0.0.1:0")
	cmd.Env = append(cmd.Environ(), append([]string{
		"CACTUSFS_DEV_PLAINTEXT=true", "CACTUSFS_KMS_PASSPHRASE=integration test passphrase",
		"CACTUSFS_ROOT_ACCESS_KEY=" + accessKey, "CACTUSFS_ROOT_SECRET_KEY=" + secretKey,
	}, env...)...)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	n := &updatesNode{
		cmd: cmd, exe: exe, stderr: &lockedBuffer{}, exited: make(chan struct{}),
		root:  map[string]string{"Authorization": "Bearer " + accessKey + ":" + secretKey},
		s3Key: [2]string{accessKey, secretKey},
	}
	cmd.Stdout, cmd.Stderr = writer, n.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the node: %v", err)
	}
	// The node holds the only end that writes now: the log ends when it
	// does.
	_ = writer.Close()
	go func() {
		defer func() { _ = reader.Close() }()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			var line updatesLogLine
			err := json.Unmarshal(scanner.Bytes(), &line)
			n.mu.Lock()
			if err != nil {
				n.notJSON = append(n.notJSON, scanner.Text())
			} else {
				n.lines = append(n.lines, line)
			}
			n.mu.Unlock()
		}
		n.mu.Lock()
		n.logEOF = true
		n.mu.Unlock()
	}()
	go func() {
		n.waitErr = cmd.Wait()
		close(n.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-n.exited:
		default:
			_ = cmd.Process.Kill()
			<-n.exited
		}
	})
	return n
}

// listening waits until the node has said, after line after of its log,
// where both of its listeners are, and returns their addresses and the
// number of lines read then.
func (n *updatesNode) listening(t *testing.T, after int, within time.Duration) (map[string]string, int) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		n.mu.Lock()
		addrs := map[string]string{}
		for _, line := range n.lines[min(after, len(n.lines)):] {
			if line.Msg == "listening" {
				addrs[line.Listener] = line.Addr
			}
		}
		read, ended := len(n.lines), n.logEOF
		n.mu.Unlock()
		if addrs["admin"] != "" && addrs["s3"] != "" {
			return addrs, read
		}
		select {
		case <-n.exited:
			t.Fatalf("the node exited before it listened: %v\n%s", n.waitErr, n.stderr.String())
		default:
		}
		if ended || time.Now().After(deadline) {
			t.Fatalf("the node did not listen within %v (log ended: %v)\n%s", within, ended, n.stderr.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// logged reports how many lines of the node's log are msg.
func (n *updatesNode) logged(msg string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	count := 0
	for _, line := range n.lines {
		if line.Msg == msg {
			count++
		}
	}
	return count
}

// stop sends SIGTERM and fails the test unless the node stopped cleanly.
func (n *updatesNode) stop(t *testing.T) {
	t.Helper()
	if err := n.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case <-n.exited:
	case <-time.After(90 * time.Second):
		t.Fatal("the node did not stop on SIGTERM")
	}
	if n.waitErr != nil {
		t.Fatalf("the node did not exit cleanly on SIGTERM: %v\n%s", n.waitErr, n.stderr.String())
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, line := range n.notJSON {
		t.Errorf("log line is not JSON: %q", line)
	}
}

// s3 is a client of the node's S3 listener at addr, as root.
func (n *updatesNode) s3(addr string) *s3.Client {
	return s3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(n.s3Key[0], n.s3Key[1], ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("http://" + addr)
		o.UsePathStyle = true
	})
}

// updatesConsole is a browser signed in as the console user every new
// deployment has (ADR-0014).
type updatesConsole struct {
	cookie string
	csrf   string
}

func (c updatesConsole) reading() map[string]string { return map[string]string{"Cookie": c.cookie} }

func (c updatesConsole) changing() map[string]string {
	return map[string]string{"Cookie": c.cookie, "X-CSRF-Token": c.csrf}
}

// updatesSignIn signs the default console user in. It fails the test
// without printing the answer, which holds a session.
func updatesSignIn(ctx context.Context, t *testing.T, api string) updatesConsole {
	t.Helper()
	status, header, body := adminCall(ctx, t, http.MethodPost, api+"/session", nil,
		map[string]any{"username": app.DefaultAdminName, "password": app.DefaultAdminPassword})
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	cookie := strings.SplitN(header.Get("Set-Cookie"), ";", 2)[0]
	if err := json.Unmarshal(body, &session); err != nil || status != http.StatusOK || session.CSRFToken == "" ||
		!strings.HasPrefix(cookie, "__Host-cactusfs-session=") {
		t.Fatalf("signing %s in = %d (%v)", app.DefaultAdminName, status, err)
	}
	return updatesConsole{cookie: cookie, csrf: session.CSRFToken}
}

// updatesView is the answer of GET /updates, as far as these tests read it.
type updatesView struct {
	Current struct {
		Version string `json:"version"`
	} `json:"current"`
	SourceConfigured  bool     `json:"source_configured"`
	CheckedAt         *string  `json:"checked_at"`
	LastError         *string  `json:"last_error"`
	NextStep          *string  `json:"next_step"`
	Mode              string   `json:"mode"`
	UpgradeAvailable  bool     `json:"upgrade_available"`
	UnavailableReason *string  `json:"unavailable_reason"`
	Instructions      []string `json:"instructions"`
	Available         []struct {
		Version string  `json:"version"`
		Allowed bool    `json:"allowed"`
		Refusal *string `json:"refusal"`
	} `json:"available"`
}

// offered returns what the view says of each release it lists: "allowed",
// or its refusal.
func (v updatesView) offered() map[string]string {
	out := map[string]string{}
	for _, r := range v.Available {
		switch {
		case r.Allowed:
			out[r.Version] = "allowed"
		case r.Refusal != nil:
			out[r.Version] = *r.Refusal
		default:
			out[r.Version] = "refused without a reason"
		}
	}
	return out
}

// readUpdates asks GET /updates.
func readUpdates(ctx context.Context, t *testing.T, api string, who map[string]string) updatesView {
	t.Helper()
	status, _, body := adminCall(ctx, t, http.MethodGet, api+"/updates", who, nil)
	var view updatesView
	if err := json.Unmarshal(body, &view); err != nil || status != http.StatusOK {
		t.Fatalf("GET /updates = %d %s (%v)", status, body, err)
	}
	return view
}

// checkUpdates asks POST /updates/check, and fails the test unless the
// node found the mirror's releases.
func checkUpdates(ctx context.Context, t *testing.T, api string, c updatesConsole) updatesView {
	t.Helper()
	// No body; like every POST of the API it says it is JSON.
	headers := c.changing()
	headers["Content-Type"] = "application/json"
	status, _, body := adminCall(ctx, t, http.MethodPost, api+"/updates/check", headers, nil)
	var view updatesView
	if err := json.Unmarshal(body, &view); err != nil || status != http.StatusOK {
		t.Fatalf("POST /updates/check = %d %s (%v)", status, body, err)
	}
	if !view.SourceConfigured || view.CheckedAt == nil || view.LastError != nil {
		t.Fatalf("POST /updates/check: the node did not read the mirror: %s", body)
	}
	return view
}

// upgradeProblem asks POST /upgrade and fails the test unless it is
// answered with the problem expected.
func upgradeProblem(ctx context.Context, t *testing.T, api, what string, who map[string]string, version, password string, wantStatus int, wantCode string) {
	t.Helper()
	status, _, body := adminCall(ctx, t, http.MethodPost, api+"/upgrade", who, map[string]any{"version": version, "password": password})
	var problem struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &problem); err != nil || status != wantStatus || problem.Code != wantCode {
		t.Fatalf("%s: POST /upgrade %s = %d %s, want %d %s", what, version, status, body, wantStatus, wantCode)
	}
}

// updatesAuditRecord is a record of the audit trail, as far as these tests
// read it.
type updatesAuditRecord struct {
	Actor   string            `json:"actor"`
	Action  string            `json:"action"`
	Outcome string            `json:"outcome"`
	Details map[string]string `json:"details"`
}

// updatesAudit waits until every one of want has a record of the audit
// trail that matches it, and fails the test naming those that have none.
func updatesAudit(ctx context.Context, t *testing.T, api string, root map[string]string, want map[string]func(updatesAuditRecord) bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		status, _, body := adminCall(ctx, t, http.MethodGet, api+"/audit?limit=1000", root, nil)
		var page struct {
			Records []updatesAuditRecord `json:"records"`
		}
		if err := json.Unmarshal(body, &page); err != nil || status != http.StatusOK {
			t.Fatalf("GET /audit = %d %s (%v)", status, body, err)
		}
		var missing []string
		for what, matches := range want {
			if !slices.ContainsFunc(page.Records, matches) {
				missing = append(missing, what)
			}
		}
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			slices.Sort(missing)
			var seen []string
			for _, r := range page.Records {
				if strings.HasPrefix(r.Action, "admin:UpgradeCluster") || r.Action == "auth:ConfirmPassword" {
					seen = append(seen, fmt.Sprintf("%s %s %s %v", r.Actor, r.Action, r.Outcome, r.Details))
				}
			}
			t.Fatalf("the audit trail has no record of %s; of upgrades it has:\n%s", strings.Join(missing, ", "), strings.Join(seen, "\n"))
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// upgradeRecord matches a record of admin:UpgradeCluster with an outcome
// and the details given.
func upgradeRecord(outcome string, details map[string]string) func(updatesAuditRecord) bool {
	return func(r updatesAuditRecord) bool {
		if r.Action != "admin:UpgradeCluster" || r.Outcome != outcome {
			return false
		}
		for name, value := range details {
			if r.Details[name] != value {
				return false
			}
		}
		return true
	}
}

// TestUpdates: releases checked for and installed from the console, end to
// end (PLAN P.11, ADR-0029). A local TLS mirror answers as GitHub does,
// with releases whose lists are signed by a Sigstore of the test as the
// release workflow at the tag of each release; the nodes verify them with
// the production rule, only the trusted root swapped (update-trusted-root).
//
//   - upgrade-mode binary: the default console user checks, is refused an
//     API key, a wrong password, a release two minor versions on (409), a
//     tampered binary and a list signed for another tag (412) - the
//     executable untouched each time - and then installs the next patch, a
//     build of this tree as that version: the node re-executes in place
//     (same PID), reports the new version, reads an object written before,
//     and the audit trail has every start and refusal.
//   - upgrade-mode off: the node checks, offers the release with the
//     commands to run instead, and answers POST /upgrade 501.
func TestUpdates(t *testing.T) {
	t.Parallel()

	// Built before the Sigstore of the test is made: it signs for a few
	// minutes from then, and a cold build can take longer.
	versions := updatesVersionsOf(t)
	// Each node runs a build in a directory of its own, where an upgrade
	// may replace it.
	binaryExe, offExe, next := buildBinary(t), buildBinary(t), buildBinaryAs(t, versions.next)
	asset := release.BinaryAsset(runtime.GOOS, runtime.GOARCH)
	if asset == "" {
		t.Fatalf("no release is built for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	nextBinary := readFileIn(t, next)

	signer, err := sigstoretest.New()
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := releasemirror.New(signer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mirror.Close)
	fake := func(what string) map[string][]byte { return map[string][]byte{asset: []byte("not a binary: " + what)} }
	for _, r := range []releasemirror.Release{
		{Version: versions.next, Binaries: map[string][]byte{asset: nextBinary}},
		{Version: versions.tampered, Binaries: fake("listed"), Served: fake("served")},
		{Version: versions.otherTag, Binaries: fake("other tag"), SignedAs: versions.next},
		{Version: versions.skipped, Binaries: fake("skipped")},
	} {
		if err := mirror.Publish(r); err != nil {
			t.Fatal(err)
		}
	}
	trustedRoot, err := signer.TrustedRoot()
	if err != nil {
		t.Fatal(err)
	}
	files := t.TempDir()
	writeFileIn(t, filepath.Join(files, "mirror-ca.pem"), mirror.CA())
	writeFileIn(t, filepath.Join(files, "trusted_root.json"), trustedRoot)
	updatesEnv := []string{
		"CACTUSFS_UPDATE_SOURCE=" + mirror.URL(),
		"CACTUSFS_UPDATE_CA=" + filepath.Join(files, "mirror-ca.pem"),
		"CACTUSFS_UPDATE_TRUSTED_ROOT=" + filepath.Join(files, "trusted_root.json"),
		// The mirror is on the loopback address, where nothing is
		// connected to unless it is allowed (ADR-0023).
		"CACTUSFS_EGRESS_ALLOW=127.0.0.1/32",
		// Asked from the console only: one check a minute a node.
		"CACTUSFS_UPDATE_CHECK_INTERVAL=0",
	}

	t.Run("binary", func(t *testing.T) {
		t.Parallel()
		testUpgradeOfTheBinary(t, versions, binaryExe, append(slices.Clone(updatesEnv), "CACTUSFS_UPGRADE_MODE=binary"))
	})
	t.Run("off", func(t *testing.T) {
		t.Parallel()
		testUpgradeModeOff(t, versions, offExe, updatesEnv)
	})
	t.Cleanup(func() {
		// Every request the nodes sent the mirror named the product and
		// carried no credential.
		requests := mirror.Requests()
		if len(requests) == 0 {
			t.Error("the mirror was sent no request")
		}
		for _, r := range requests {
			if r.Credentials || (r.UserAgent != "cactusfs/"+versions.current && r.UserAgent != "cactusfs/"+versions.next) {
				t.Errorf("the mirror was sent %s by %q, with credentials: %v", r.Path, r.UserAgent, r.Credentials)
			}
		}
	})
}

// testUpgradeOfTheBinary is TestUpdates with upgrade-mode binary.
func testUpgradeOfTheBinary(t *testing.T, versions updatesVersions, exe string, env []string) {
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	node := startUpdatesNode(ctx, t, exe, t.TempDir(), env...)
	addrs, read := node.listening(t, 0, 2*time.Minute)
	api := "http://" + addrs["admin"] + "/api/v1"
	pid := node.cmd.Process.Pid

	// An object written before the upgrade.
	const bucketName, key = "before-the-upgrade", "kept.txt"
	content := bytes.Repeat([]byte("written by "+versions.current+"\n"), 1000)
	client := node.s3(addrs["s3"])
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucketName)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucketName), Key: aws.String(key), Body: bytes.NewReader(content)}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	console := updatesSignIn(ctx, t, api)
	if before := readUpdates(ctx, t, api, console.reading()); before.CheckedAt != nil || len(before.Available) != 0 || before.Mode != "binary" {
		t.Fatalf("before a check, GET /updates = checked %v, %d releases, mode %q", before.CheckedAt, len(before.Available), before.Mode)
	}
	checkUpdates(ctx, t, api, console)
	view := readUpdates(ctx, t, api, console.reading())
	wantOffered := map[string]string{
		versions.next: "allowed", versions.tampered: "allowed", versions.otherTag: "allowed",
		versions.skipped: string(release.ReasonSkippedMinor),
	}
	if got := view.offered(); !maps.Equal(got, wantOffered) {
		t.Fatalf("GET /updates offers %v, want %v", got, wantOffered)
	}
	if view.Current.Version != versions.current || !view.UpgradeAvailable || view.Mode != "binary" ||
		view.NextStep == nil || *view.NextStep != versions.otherTag {
		t.Fatalf("GET /updates = current %q, upgrade available %v, mode %q, next step %v",
			view.Current.Version, view.UpgradeAvailable, view.Mode, view.NextStep)
	}

	// Refusals. None of them touches the executable.
	sum := fileSum(t, exe)
	untouched := func(what string) {
		t.Helper()
		if got := fileSum(t, exe); got != sum {
			t.Fatalf("after %s the executable changed", what)
		}
		entries, err := os.ReadDir(filepath.Dir(exe))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Name()
			}
			t.Fatalf("after %s the directory of the executable holds %v", what, names)
		}
	}
	password := app.DefaultAdminPassword

	// An API key of the very user, whose role may install: no password to
	// type, never an upgrade.
	status, _, body := adminCall(ctx, t, http.MethodPost, api+"/users/"+app.DefaultAdminName+"/keys", node.root, map[string]any{"description": "P.11"})
	var created struct {
		Key struct {
			AccessKeyID string `json:"access_key_id"`
		} `json:"key"`
		SecretKey string `json:"secret_key"`
	}
	if err := json.Unmarshal(body, &created); err != nil || status != http.StatusCreated || created.SecretKey == "" {
		t.Fatalf("POST /users/%s/keys = %d (%v)", app.DefaultAdminName, status, err)
	}
	apiKey := map[string]string{"Authorization": "Bearer " + created.Key.AccessKeyID + ":" + created.SecretKey}
	if got := readUpdates(ctx, t, api, apiKey); got.Current.Version != versions.current {
		t.Fatalf("the API key reads GET /updates as %q", got.Current.Version)
	}
	upgradeProblem(ctx, t, api, "an API key", apiKey, versions.next, password, http.StatusForbidden, "AccessDenied")
	untouched("an API key")

	upgradeProblem(ctx, t, api, "a wrong password", console.changing(), versions.next, "not the password of "+app.DefaultAdminName,
		http.StatusForbidden, "AccessDenied")
	untouched("a wrong password")
	// The session stays as it was.
	if status, _, body := adminCall(ctx, t, http.MethodGet, api+"/session", console.reading(), nil); status != http.StatusOK {
		t.Fatalf("after a wrong password GET /session = %d %s", status, body)
	}

	upgradeProblem(ctx, t, api, "two minor versions on", console.changing(), versions.skipped, password, http.StatusConflict, "UpgradeNotAllowed")
	untouched("a release two minor versions on")
	upgradeProblem(ctx, t, api, "a tampered binary", console.changing(), versions.tampered, password, http.StatusPreconditionFailed, "ReleaseNotVerified")
	untouched("a tampered binary")
	upgradeProblem(ctx, t, api, "a bundle of another tag", console.changing(), versions.otherTag, password, http.StatusPreconditionFailed, "ReleaseNotVerified")
	untouched("a bundle of another tag")
	if got := readUpdates(ctx, t, api, console.reading()); got.Current.Version != versions.current {
		t.Fatalf("after the refusals the node runs %q", got.Current.Version)
	}

	// The upgrade.
	stoppedBefore := node.logged("stopped")
	status, header, body := adminCall(ctx, t, http.MethodPost, api+"/upgrade", console.changing(), map[string]any{"version": versions.next, "password": password})
	var progress struct {
		State  string  `json:"state"`
		Target *string `json:"target"`
	}
	if err := json.Unmarshal(body, &progress); err != nil || status != http.StatusAccepted ||
		progress.Target == nil || *progress.Target != versions.next || header.Get("Location") != "/api/v1/upgrade" {
		t.Fatalf("POST /upgrade %s = %d %s (Location %q, %v)", versions.next, status, body, header.Get("Location"), err)
	}

	// The node stops, and the same process - its PID was never waited
	// for - comes back as the new release on the same pipe.
	addrs, _ = node.listening(t, read, 3*time.Minute)
	api = "http://" + addrs["admin"] + "/api/v1"
	select {
	case <-node.exited:
		t.Fatalf("process %d exited: the node was not executed in its place (%v)", pid, node.waitErr)
	default:
	}
	if node.logged("stopped") <= stoppedBefore {
		t.Error("the node restarted without logging a clean stop")
	}
	deadline := time.Now().Add(time.Minute)
	for {
		got := readUpdates(ctx, t, api, console.reading())
		if got.Current.Version == versions.next {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the upgrade GET /updates reports %q, want %q", got.Current.Version, versions.next)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if status, _, body := adminCall(ctx, t, http.MethodGet, api+"/upgrade", console.reading(), nil); status != http.StatusOK ||
		!strings.Contains(string(body), `"state":"idle"`) {
		t.Errorf("after the restart GET /upgrade = %d %s, want idle", status, body)
	}
	if got := versionOf(ctx, t, exe); !strings.HasPrefix(got, "cactusfs "+versions.next+" ") {
		t.Errorf("the executable is now %q, want release %s", got, versions.next)
	}
	if got := versionOf(ctx, t, filepath.Join(filepath.Dir(exe), "cactusfs.previous")); !strings.HasPrefix(got, "cactusfs "+versions.current+" ") {
		t.Errorf("cactusfs.previous is %q, want release %s", got, versions.current)
	}

	// The data is the node's still.
	out, err := node.s3(addrs["s3"]).GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucketName), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("GetObject after the upgrade: %v", err)
	}
	got, err := io.ReadAll(out.Body)
	_ = out.Body.Close()
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("GetObject after the upgrade: %d bytes (%v), want the %d written before", len(got), err, len(content))
	}

	actor := "user/" + app.DefaultAdminName
	updatesAudit(ctx, t, api, node.root, map[string]func(updatesAuditRecord) bool{
		"the check": func(r updatesAuditRecord) bool {
			return r.Action == "admin:CheckForUpdates" && r.Outcome == "success"
		},
		"the API key denied": func(r updatesAuditRecord) bool {
			return upgradeRecord("denied", nil)(r) && strings.Contains(r.Details["reason"], "API key")
		},
		"the wrong password": func(r updatesAuditRecord) bool {
			return r.Action == "auth:ConfirmPassword" && r.Outcome != "success"
		},
		"the skipped minor": upgradeRecord("failed", map[string]string{
			"from": versions.current, "to": versions.skipped, "reason": "not allowed: " + string(release.ReasonSkippedMinor),
		}),
		"the tampered binary": upgradeRecord("failed", map[string]string{
			"from": versions.current, "to": versions.tampered, "reason": domain.ErrReleaseNotVerified.Error(),
		}),
		"the bundle of another tag": upgradeRecord("failed", map[string]string{
			"from": versions.current, "to": versions.otherTag, "reason": domain.ErrReleaseNotVerified.Error(),
		}),
		"the start": func(r updatesAuditRecord) bool {
			return upgradeRecord("success", map[string]string{"from": versions.current, "to": versions.next, "mode": "binary"})(r) &&
				r.Actor == actor && strings.HasPrefix(r.Details["digest"], "sha256:")
		},
		"the end": upgradeRecord("success", map[string]string{"from": versions.current, "to": versions.next, "state": "done"}),
	})
	node.stop(t)
}

// testUpgradeModeOff is TestUpdates with upgrade-mode off, its default.
func testUpgradeModeOff(t *testing.T, versions updatesVersions, exe string, env []string) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	node := startUpdatesNode(ctx, t, exe, t.TempDir(), env...)
	addrs, _ := node.listening(t, 0, 2*time.Minute)
	api := "http://" + addrs["admin"] + "/api/v1"
	sum := fileSum(t, exe)

	console := updatesSignIn(ctx, t, api)
	checkUpdates(ctx, t, api, console)
	view := readUpdates(ctx, t, api, console.reading())
	if got := view.offered(); got[versions.next] != "allowed" {
		t.Fatalf("GET /updates offers %v, want %s allowed", got, versions.next)
	}
	if view.Mode != "off" || view.UpgradeAvailable || view.UnavailableReason == nil || *view.UnavailableReason == "" {
		t.Fatalf("GET /updates = mode %q, upgrade available %v, reason %v", view.Mode, view.UpgradeAvailable, view.UnavailableReason)
	}
	// What to run instead: the binary's own commands, which name it.
	if len(view.Instructions) == 0 || !strings.Contains(strings.Join(view.Instructions, "\n"), release.BinaryAsset(runtime.GOOS, runtime.GOARCH)) {
		t.Fatalf("GET /updates gives the instructions %q", view.Instructions)
	}

	upgradeProblem(ctx, t, api, "upgrade-mode off", console.changing(), versions.next, app.DefaultAdminPassword,
		http.StatusNotImplemented, "UpgradeUnavailable")
	if fileSum(t, exe) != sum {
		t.Fatal("upgrade-mode off changed the executable")
	}
	if got := versionOf(ctx, t, exe); !strings.HasPrefix(got, "cactusfs "+versions.current+" ") {
		t.Fatalf("the executable is %q", got)
	}
	if status, _, body := adminCall(ctx, t, http.MethodGet, api+"/upgrade", console.reading(), nil); status != http.StatusOK ||
		!strings.Contains(string(body), `"state":"idle"`) {
		t.Fatalf("GET /upgrade = %d %s, want idle", status, body)
	}
	updatesAudit(ctx, t, api, node.root, map[string]func(updatesAuditRecord) bool{
		"the refusal": upgradeRecord("failed", map[string]string{
			"from": versions.current, "to": versions.next, "mode": "off", "reason": domain.ErrUpgradeUnavailable.Error(),
		}),
	})
	node.stop(t)
}
