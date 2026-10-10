//go:build s3compat && s3tools

package s3compat_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The S3 tests of Ceph (github.com/ceph/s3-tests) against the running
// server. They are in the builder image at one commit, with the packages
// they import pinned by hash (Dockerfile, tools/requirements.lock).
//
// tools/s3tests-selected.txt is the explicit list of the tests that are run;
// tools/s3tests-baseline.txt lists those that pass. A test of the baseline
// that no longer passes fails this test. "make s3-compat-record" adds tests
// that have begun to pass; nothing is ever taken out of the baseline by a
// tool.
const (
	cephSource   = "/opt/s3tests/src"
	cephPytest   = "/opt/s3tests/venv/bin/pytest"
	cephTools    = "tools"
	cephSelected = "s3tests-selected.txt"
	cephBaseline = "s3tests-baseline.txt"
	// cephRecord, when set to 1, adds newly passing tests to the baseline.
	cephRecord = "CACTUSFS_S3COMPAT_RECORD"
	// cephReport, when set, names a file the JUnit report of pytest is
	// copied to.
	cephReport = "CACTUSFS_S3TESTS_REPORT"
)

// adminCall makes one call to the management API as root.
func adminCall(ctx context.Context, method, path string, body, into any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+target.adminAddr+"/api/v1"+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+target.accessKey+":"+target.secretKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout:   time.Minute,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: target.pool, MinVersion: tls.VersionTLS12}},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		var problem bytes.Buffer
		_, _ = problem.ReadFrom(resp.Body)
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, problem.String())
	}
	if into == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// cephGuests makes the role of the second and third user of the tests: they
// may make buckets and list them, and nothing else. The first user is root.
// So whatever one of them does in a bucket of root's, it does because a
// bucket policy lets it - a test of bucket policies that passes, passes for
// that reason. (A user has no rights over "its own" buckets here: this store
// has no bucket owner, and tests that need one are not in the baseline.)
func cephGuests(ctx context.Context, name string) error {
	policy := map[string]any{"name": name, "document": map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect": "Allow", "Action": []string{"s3:CreateBucket", "s3:ListAllMyBuckets"}, "Resource": "*",
		}},
	}}
	if err := adminCall(ctx, http.MethodPost, "/policies", policy, nil); err != nil {
		return err
	}
	return adminCall(ctx, http.MethodPost, "/roles", map[string]any{"name": name, "policies": []string{name}}, nil)
}

// cephUser makes a user with the role, and a key for it.
func cephUser(ctx context.Context, name, role string) (accessKey, secretKey string, err error) {
	user := map[string]any{"name": name, "roles": []string{role}}
	if err := adminCall(ctx, http.MethodPost, "/users", user, nil); err != nil {
		return "", "", err
	}
	var created struct {
		Key struct {
			AccessKeyID string `json:"access_key_id"`
		} `json:"key"`
		SecretKey string `json:"secret_key"`
	}
	if err := adminCall(ctx, http.MethodPost, "/users/"+name+"/keys", map[string]any{"description": "ceph/s3-tests"}, &created); err != nil {
		return "", "", err
	}
	return created.Key.AccessKeyID, created.SecretKey, nil
}

// junit is the result file of pytest.
type junit struct {
	Suites []struct {
		Cases []struct {
			Class   string    `xml:"classname,attr"`
			Name    string    `xml:"name,attr"`
			Failure *struct{} `xml:"failure"`
			Error   *struct{} `xml:"error"`
			Skipped *struct{} `xml:"skipped"`
		} `xml:"testcase"`
	} `xml:"testsuite"`
}

// lines reads a list file of the tools directory: one entry a line, "#"
// begins a comment.
func lines(t *testing.T, tools *os.Root, name string) []string {
	t.Helper()
	raw, err := tools.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

func TestCephS3Tests(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()
	dir := t.TempDir()
	work, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = work.Close() }()
	toolFiles, err := os.OpenRoot(cephTools)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = toolFiles.Close() }()
	suffix := fmt.Sprintf("%x", randomBytes(t, 3))
	guests := "s3tests-guest-" + suffix
	if err := cephGuests(ctx, guests); err != nil {
		t.Fatalf("the role of the other users of the tests: %v", err)
	}
	altKey, altSecret, err := cephUser(ctx, "s3tests-alt-"+suffix, guests)
	if err != nil {
		t.Fatalf("the second user of the tests: %v", err)
	}
	tenantKey, tenantSecret, err := cephUser(ctx, "s3tests-tenant-"+suffix, guests)
	if err != nil {
		t.Fatalf("the third user of the tests: %v", err)
	}
	host, port, _ := strings.Cut(target.s3Addr, ":")
	account := func(section, id, accessKey, secretKey, extra string) string {
		return fmt.Sprintf("[%s]\ndisplay_name = %s\nuser_id = %s\nemail = %s@example.test\naccess_key = %s\nsecret_key = %s\n%s\n",
			section, id, id, id, accessKey, secretKey, extra)
	}
	conf := fmt.Sprintf("[DEFAULT]\nhost = %s\nport = %s\nis_secure = True\nssl_verify = True\n\n"+
		"[fixtures]\nbucket prefix = ceph-{random}-\niam name prefix = s3-tests-\niam path prefix = /s3-tests/\n\n", host, port) +
		account("s3 main", target.accessKey, target.accessKey, target.secretKey, "api_name = \n") +
		account("s3 alt", altKey, altKey, altSecret, "") +
		account("s3 tenant", tenantKey, tenantKey, tenantSecret, "tenant = testx\n") +
		account("iam", altKey, altKey, altSecret, "") +
		account("iam root", target.accessKey, target.accessKey, target.secretKey, "") +
		account("iam alt root", altKey, altKey, altSecret, "")
	confFile := filepath.Join(dir, "s3tests.conf")
	if err := os.WriteFile(confFile, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}

	selected := lines(t, toolFiles, cephSelected)
	if len(selected) == 0 {
		t.Fatalf("%s selects no test", cephSelected)
	}
	argsFile := filepath.Join(dir, "selected.txt")
	if err := os.WriteFile(argsFile, []byte(strings.Join(selected, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	results := filepath.Join(dir, "results.xml")
	tools, err := filepath.Abs(cephTools)
	if err != nil {
		t.Fatal(err)
	}
	// TLS is verified, against the authority of this run: see the plugin.
	cmd := exec.CommandContext(ctx, cephPytest, "-q", "-p", "no:cacheprovider", "-p", "cactusfs_ca", "--tb=line", "--junitxml="+results, "@"+argsFile)
	cmd.Dir = cephSource
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "S3TEST_CONF=" + confFile, "PYTHONDONTWRITEBYTECODE=1",
		"PYTHONPATH=" + tools, "CACTUSFS_CA_BUNDLE=" + target.certFile,
		"REQUESTS_CA_BUNDLE=" + target.certFile, "AWS_CA_BUNDLE=" + target.certFile, "AWS_EC2_METADATA_DISABLED=true",
	}
	output, runErr := cmd.CombinedOutput()
	raw, err := work.ReadFile(filepath.Base(results))
	if err != nil {
		t.Fatalf("pytest left no results (%v): %v\n%s", runErr, err, tail(output, 4000))
	}
	// For whoever works on what does not pass yet: the whole report.
	if keep := os.Getenv(cephReport); keep != "" {
		where, err := os.OpenRoot(filepath.Dir(keep))
		if err != nil {
			t.Fatal(err)
		}
		err = where.WriteFile(filepath.Base(keep), raw, 0o600)
		_ = where.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	var report junit
	if err := xml.Unmarshal(raw, &report); err != nil {
		t.Fatalf("results of pytest: %v", err)
	}
	passed := map[string]bool{}
	ran := 0
	for _, suite := range report.Suites {
		for _, c := range suite.Cases {
			ran++
			if c.Failure == nil && c.Error == nil && c.Skipped == nil {
				passed[strings.ReplaceAll(c.Class, ".", "/")+".py::"+c.Name] = true
			}
		}
	}
	baseline := lines(t, toolFiles, cephBaseline)
	recording := os.Getenv(cephRecord) == "1"
	if len(baseline) == 0 && !recording {
		t.Fatalf("%s records no test: run make s3-compat-record", cephBaseline)
	}
	t.Logf("ceph/s3-tests: %d selected, %d ran, %d passed (baseline %d)", len(selected), ran, len(passed), len(baseline))
	var regressions []string
	for _, id := range baseline {
		if !passed[id] {
			regressions = append(regressions, id)
		}
	}
	if len(regressions) > 0 {
		t.Errorf("%d tests of the baseline no longer pass:\n%s\n\nthe end of what pytest said:\n%s",
			len(regressions), strings.Join(regressions, "\n"), tail(output, 6000))
	}
	var fresh []string
	for id := range passed {
		if !slices.Contains(baseline, id) {
			fresh = append(fresh, id)
		}
	}
	slices.Sort(fresh)
	switch {
	case len(fresh) == 0:
	case recording:
		file, err := toolFiles.OpenFile(cephBaseline, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = file.Close() }()
		// A file that was edited by hand may not end its last line.
		addition := strings.Join(fresh, "\n") + "\n"
		if had, err := toolFiles.ReadFile(cephBaseline); err != nil {
			t.Fatal(err)
		} else if len(had) > 0 && had[len(had)-1] != '\n' {
			addition = "\n" + addition
		}
		if _, err := file.WriteString(addition); err != nil {
			t.Fatal(err)
		}
		t.Logf("ceph/s3-tests: %d tests added to the baseline", len(fresh))
	default:
		t.Logf("ceph/s3-tests: %d tests pass that the baseline does not record; run make s3-compat-record", len(fresh))
	}
}

func tail(output []byte, n int) string {
	if len(output) > n {
		output = output[len(output)-n:]
	}
	return string(output)
}
