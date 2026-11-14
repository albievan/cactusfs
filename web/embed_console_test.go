//go:build console

package web_test

import (
	"io/fs"
	"regexp"
	"testing"

	"github.com/albievan/cactusfs/web"
)

// TestTheEmbeddedConsoleCarriesItsNotices: a binary built with the console
// carries the notices file its build wrote (PLAN X.5), naming the packages
// the console is built with and the notices of pdf.js's decoders shipped
// beside the bundle. Run by `make licences`, after the console is built.
func TestTheEmbeddedConsoleCarriesItsNotices(t *testing.T) {
	t.Parallel()
	dist, built := web.Dist()
	if !built {
		t.Fatal("Dist() says the console was not built, with the console tag")
	}
	notices, err := fs.ReadFile(dist, "notices.txt")
	if err != nil {
		t.Fatalf("the embedded console has no notices.txt: %v", err)
	}
	for _, name := range []string{"react", "react-dom", "bootstrap", "bootstrap-icons", "openapi-fetch", "pdfjs-dist", "fflate"} {
		if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` \S+\nLicence: \S+\n`).Match(notices) {
			t.Errorf("notices.txt does not name the package %s with its version and licence", name)
		}
	}
	for _, file := range []string{"LICENSE_JBIG2", "LICENSE_PDFJS_JBIG2", "LICENSE_OPENJPEG", "LICENSE_PDFJS_OPENJPEG"} {
		if !regexp.MustCompile(`(?m)^wasm/` + file + `:$`).Match(notices) {
			t.Errorf("notices.txt does not carry pdfjs-dist's %s", file)
		}
	}
}
