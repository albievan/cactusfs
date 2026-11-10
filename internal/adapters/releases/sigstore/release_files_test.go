package sigstore_test

import (
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// repository is the tree the release script and workflow are read from.
func repository() fs.FS { return os.DirFS("../../../..") }

func readRepository(t *testing.T, name string) string {
	t.Helper()
	content, err := fs.ReadFile(repository(), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(content)
}

// uploads returns the files `gh release create` of the release workflow
// hands to the GitHub Release: every argument under dist/ of the command,
// which goes on over lines that end with a backslash.
func uploads(t *testing.T, workflow string) []string {
	t.Helper()
	lines := strings.Split(workflow, "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "gh release create") })
	if start < 0 {
		t.Fatal(".github/workflows/release.yml runs no gh release create")
	}
	var files []string
	for _, line := range lines[start:] {
		for field := range strings.FieldsSeq(line) {
			if strings.HasPrefix(field, "dist/") {
				files = append(files, field)
			}
		}
		if !strings.HasSuffix(strings.TrimSpace(line), `\`) {
			break
		}
	}
	return files
}

// TestReleaseSignsWhatTheVerifierReads: the files a node verifies an
// upgrade with (PLAN P.4, ADR-0029) - SHA256SUMS of the binaries and
// digests.txt of the image, each with its Sigstore bundle - are written and
// signed by scripts/release.sh --publish, the signature is verified there,
// and the release workflow uploads each of them to the GitHub Release.
// digests.txt is signed only once it is complete.
func TestReleaseSignsWhatTheVerifierReads(t *testing.T) {
	t.Parallel()
	script := readRepository(t, "scripts/release.sh")
	workflow := readRepository(t, ".github/workflows/release.yml")
	uploaded := uploads(t, workflow)

	for _, signed := range []string{"SHA256SUMS", "digests.txt"} {
		bundle := signed + ".sigstore.json"
		args := `"$out/` + signed + `" "$out/` + bundle + `"`
		if !strings.Contains(script, "sc_sign_blob "+args) {
			t.Errorf("scripts/release.sh does not sign %s into %s with sc_sign_blob", signed, bundle)
		}
		if !strings.Contains(script, "sc_verify_blob "+args) {
			t.Errorf("scripts/release.sh does not verify %s with sc_verify_blob", bundle)
		}
		for _, file := range []string{signed, bundle} {
			if !slices.Contains(uploaded, "dist/release/"+file) {
				t.Errorf("the release workflow does not upload dist/release/%s (it uploads %v)", file, uploaded)
			}
		}
		if !strings.Contains(script, "--bundle "+bundle) {
			t.Errorf("NOTES.md of scripts/release.sh does not say how %s is verified with %s", signed, bundle)
		}
	}

	// Every line of digests.txt is written before it is signed.
	appended := regexp.MustCompile(`>>\s*"\$out/digests\.txt"`).FindAllStringIndex(script, -1)
	signedAt := strings.Index(script, `sc_sign_blob "$out/digests.txt"`)
	if len(appended) == 0 || signedAt < 0 {
		t.Fatal("scripts/release.sh neither appends to nor signs digests.txt")
	}
	if last := appended[len(appended)-1][0]; last > signedAt {
		t.Error("scripts/release.sh appends to digests.txt after it is signed")
	}
	// Only a release signs files: the rehearsal (--dry-run, `make
	// sign-verify`) has ended before the first sc_sign_blob.
	dryRunEnds := strings.Index(script, `echo "release (dry run): PASS"`)
	firstSign := strings.Index(script, "sc_sign_blob ")
	if dryRunEnds < 0 || firstSign < dryRunEnds {
		t.Error("scripts/release.sh signs a file before the rehearsal has ended")
	}

	// NOTES.md no longer says digests.txt is unsigned.
	if strings.Contains(script, "a convenience and is not signed") {
		t.Error("NOTES.md of scripts/release.sh still says a file of the release is not signed")
	}

	// What is uploaded is what the release writes: every file under
	// dist/release/ is written by the script.
	for _, file := range uploaded {
		name, ok := strings.CutPrefix(file, "dist/release/")
		if !ok || strings.Contains(name, "*") {
			continue
		}
		if !strings.Contains(script, `"$out/`+name+`"`) {
			t.Errorf("the release workflow uploads %s, which scripts/release.sh does not write", file)
		}
	}
}
