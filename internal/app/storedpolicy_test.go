package app_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// storedPolicyParser is the parser that takes a bucket policy as it was
// stored before PLAN 9.2a, text after it that is not JSON included.
const storedPolicyParser = "ParseStoredBucketPolicy"

// TestOnlyTheAuthorizerReadsStoredPolicies: iam.ParseStoredBucketPolicy is
// named in the authorizer's read of a stored policy (internal/app/authz.go)
// and nowhere else in internal/ and cmd/ but the package that defines it.
// Used on a document being put or validated, it would accept and store
// text that is not JSON.
func TestOnlyTheAuthorizerReadsStoredPolicies(t *testing.T) {
	t.Parallel()
	repository := filepath.Join("..", "..")
	allowed := filepath.Join("internal", "app", "authz.go")
	defining := filepath.Join("internal", "domain", "iam") + string(filepath.Separator)
	fset := token.NewFileSet()
	var where []string
	inAllowed := 0
	for _, root := range []string{filepath.Join(repository, "internal"), filepath.Join(repository, "cmd")} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(repository, path)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				ident, ok := n.(*ast.Ident)
				if !ok || ident.Name != storedPolicyParser {
					return true
				}
				switch {
				case rel == allowed:
					inAllowed++
				case strings.HasPrefix(rel, defining):
				default:
					where = append(where, fset.Position(ident.Pos()).String())
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	if len(where) > 0 {
		t.Errorf("iam.%s is named outside internal/app/authz.go: %v", storedPolicyParser, where)
	}
	if inAllowed == 0 {
		t.Errorf("internal/app/authz.go no longer names iam.%s: the test looks in the wrong place", storedPolicyParser)
	}
}
