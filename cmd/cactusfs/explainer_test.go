package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The explainer (ADR-0032, Decision 3; PLAN 9.4). A ports.AccessExplainer
// is the authorizer's decision and why, with no record of it: never the
// gate of an operation. The separation is structural: *app.Authorizer has
// no method Explain, so neither it nor anything that holds it as a
// ports.Authorizer can be asserted to an explainer, and the one
// implementation is made by app.NewExplainer (internal/app/explainer.go)
// over the authorizer's unexported decide.
//
// What the tests below hold, over the source of the non-test files of
// internal/app, internal/adapters/... and cmd/cactusfs, type-checked with
// go/types: nothing there names app.NewExplainer, the type
// ports.AccessExplainer or the authorizer's decide, or selects a method
// Explain of a type that has the port's method set - a call, a method
// value or a method expression - but
//
//   - internal/app/explainer.go, and internal/app/authz.go for decide;
//   - in internal/app, the explanation use case of PLAN 9.5: the
//     declarations of ExplainService and ExplainServiceConfig, the function
//     NewExplainService and the methods of ExplainService;
//   - in cmd/cactusfs, one place: an argument of a call of
//     app.NewExplainService, or a field of an app.ExplainServiceConfig
//     literal.
//
// It holds today, when there is no ExplainService and nothing wires an
// explainer, and it holds unchanged once 9.5 adds them. Tests are not held
// to it. Files the default build of this platform leaves out - another
// operating system's, another build tag's - cannot be type-checked here;
// they are read with go/parser and must not name NewExplainer,
// AccessExplainer or decide, nor select a field or method called Explain.
// Today no file that only a build tag brings in exists outside tests in
// those directories, so that reading covers the files of the other
// operating systems.
const (
	explainUseCase     = "ExplainService"
	explainConstructor = "NewExplainService"
	explainConfig      = "ExplainServiceConfig"
	newExplainer       = "NewExplainer"
	explainerPort      = "AccessExplainer"
	explainerFile      = "explainer.go"
	authorizerFile     = "authz.go"
	mainPackage        = "github.com/albievan/cactusfs/cmd/cactusfs"
)

// listedPackage is what `go list` says of a package.
type listedPackage struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Export     string
}

// listPackages lists this package, the use cases, the adapters and
// everything they import, as the go command builds them for this platform:
// the files of each, and the export data a type checker imports them from.
func listPackages(t *testing.T) map[string]listedPackage {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", "list", "-export", "-deps", "-json=ImportPath,Dir,GoFiles,Export",
		".", "github.com/albievan/cactusfs/internal/app", "github.com/albievan/cactusfs/internal/adapters/...").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("go list: %v: %s", err, exit.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}
	listed := map[string]listedPackage{}
	decoder := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p listedPackage
		if err := decoder.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("the output of go list: %v", err)
		}
		listed[p.ImportPath] = p
	}
	return listed
}

// exportImporter imports packages from the export data go list made.
func exportImporter(fset *token.FileSet, listed map[string]listedPackage) types.Importer {
	return importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		p, ok := listed[path]
		if !ok || p.Export == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(filepath.Clean(p.Export))
	})
}

// withPackages is an importer that knows some packages it was given.
type withPackages struct {
	types.Importer
	given map[string]*types.Package
}

func (w withPackages) Import(path string) (*types.Package, error) {
	if p, ok := w.given[path]; ok {
		return p, nil
	}
	return w.Importer.Import(path)
}

// checked is a package type-checked from its source.
type checked struct {
	pkg   *types.Package
	info  *types.Info
	files []*ast.File
}

func typeCheck(fset *token.FileSet, imp types.Importer, path string, files []*ast.File) (checked, error) {
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	pkg, err := (&types.Config{Importer: imp}).Check(path, fset, files, info)
	if err != nil {
		return checked{}, fmt.Errorf("type-checking %s: %w", path, err)
	}
	return checked{pkg: pkg, info: info, files: files}, nil
}

// checkListed type-checks a listed package from its source.
func checkListed(t *testing.T, fset *token.FileSet, imp types.Importer, p listedPackage) checked {
	t.Helper()
	var files []*ast.File
	for _, name := range p.GoFiles {
		file, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	c, err := typeCheck(fset, imp, p.ImportPath, files)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// finding is a place that breaks the rule.
type finding struct {
	at   token.Position
	what string
}

func (f finding) String() string { return f.at.String() + ": " + f.what }

// explainerRule finds who names the explainer.
type explainerRule struct {
	fset      *token.FileSet
	explainer *types.Interface
	// appPath is the package of the use cases, wiringPath the composition
	// root.
	appPath, wiringPath string
	// ownFile, when set, lets the explainer's own file and the
	// authorizer's be read as any other.
	ownFile bool

	findings []finding
	// wired counts the places of the composition root that wire one.
	wired int
	// idents counts the identifiers looked at.
	idents int
}

func (r *explainerRule) found(pos token.Pos, format string, args ...any) {
	r.findings = append(r.findings, finding{at: r.fset.Position(pos), what: fmt.Sprintf(format, args...)})
}

// ofTheUseCase reports whether a name is one of the explanation use case.
func ofTheUseCase(name string) bool {
	return name == explainUseCase || name == explainConstructor || name == explainConfig
}

// receiverName is the name of the type a method is declared on.
func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if index, ok := t.(*ast.IndexExpr); ok {
		t = index.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// scan looks at one type-checked package.
func (r *explainerRule) scan(c checked) {
	isApp, isWiring := c.pkg.Path() == r.appPath, c.pkg.Path() == r.wiringPath
	for _, file := range c.files {
		base := filepath.Base(r.fset.Position(file.Pos()).Filename)
		if isApp && base == explainerFile && !r.ownFile {
			continue
		}
		decideAllowed := isApp && base == authorizerFile && !r.ownFile
		var wiring [][2]token.Pos
		if isWiring {
			wiring = r.wiringSites(c, file)
		}
		for _, decl := range file.Decls {
			if isApp && r.ofTheUseCaseDecl(decl) {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.Ident:
					r.ident(c, n, decideAllowed, wiring)
				case *ast.SelectorExpr:
					r.selector(c, n, wiring)
				}
				return true
			})
		}
	}
}

// ofTheUseCaseDecl reports whether a declaration of the package of the use
// cases is the explanation use case's: its types, its constructor, its
// methods.
func (r *explainerRule) ofTheUseCaseDecl(decl ast.Decl) bool {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil {
			return receiverName(d) == explainUseCase
		}
		return d.Name.Name == explainConstructor
	case *ast.GenDecl:
		if d.Tok != token.TYPE || len(d.Specs) == 0 {
			return false
		}
		for _, spec := range d.Specs {
			if ts, ok := spec.(*ast.TypeSpec); !ok || !ofTheUseCase(ts.Name.Name) {
				return false
			}
		}
		return true
	}
	return false
}

// wiringSites returns the stretches of a file of the composition root
// that hand something to the explanation use case: the arguments of a call
// of NewExplainService, the fields of an ExplainServiceConfig literal.
func (r *explainerRule) wiringSites(c checked, file *ast.File) [][2]token.Pos {
	var sites [][2]token.Pos
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			var callee types.Object
			switch fun := ast.Unparen(n.Fun).(type) {
			case *ast.Ident:
				callee = c.info.Uses[fun]
			case *ast.SelectorExpr:
				callee = c.info.Uses[fun.Sel]
			}
			if r.ofApp(callee, explainConstructor) {
				for _, arg := range n.Args {
					sites = append(sites, [2]token.Pos{arg.Pos(), arg.End()})
				}
			}
		case *ast.CompositeLit:
			named, ok := types.Unalias(c.info.TypeOf(n)).(*types.Named)
			if ok && r.ofApp(named.Obj(), explainConfig) {
				for _, elt := range n.Elts {
					sites = append(sites, [2]token.Pos{elt.Pos(), elt.End()})
				}
			}
		}
		return true
	})
	return sites
}

// ofApp reports whether obj is the object of that name of the package of
// the use cases.
func (r *explainerRule) ofApp(obj types.Object, name string) bool {
	return obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == r.appPath && obj.Name() == name
}

// inWiring reports whether pos lies in a wiring site.
func inWiring(sites [][2]token.Pos, pos token.Pos) bool {
	return slices.ContainsFunc(sites, func(s [2]token.Pos) bool { return s[0] <= pos && pos < s[1] })
}

func (r *explainerRule) ident(c checked, id *ast.Ident, decideAllowed bool, wiring [][2]token.Pos) {
	obj := c.info.Uses[id]
	if obj == nil || obj.Pkg() == nil {
		return
	}
	r.idents++
	switch {
	case r.ofApp(obj, newExplainer) && isFunc(obj, false):
		if inWiring(wiring, id.Pos()) {
			r.wired++
			if r.wired == 1 {
				return
			}
			r.found(id.Pos(), "app.%s is wired a second time", newExplainer)
			return
		}
		r.found(id.Pos(), "names app.%s", newExplainer)
	case obj.Pkg().Path() == portsPackage && obj.Name() == explainerPort && isTypeName(obj):
		if !inWiring(wiring, id.Pos()) {
			r.found(id.Pos(), "names ports.%s", explainerPort)
		}
	case r.ofApp(obj, "decide") && isFunc(obj, true) && !decideAllowed:
		r.found(id.Pos(), "names the authorizer's decide")
	}
}

func isFunc(obj types.Object, method bool) bool {
	fn, ok := obj.(*types.Func)
	return ok && (fn.Signature().Recv() != nil) == method
}

func isTypeName(obj types.Object) bool {
	_, ok := obj.(*types.TypeName)
	return ok
}

// selector finds a method Explain selected on something with the port's
// method set: a call, a method value, a method expression.
func (r *explainerRule) selector(c checked, sel *ast.SelectorExpr, wiring [][2]token.Pos) {
	selection := c.info.Selections[sel]
	if selection == nil || selection.Kind() == types.FieldVal || selection.Obj().Name() != "Explain" {
		return
	}
	recv := selection.Recv()
	if !types.Implements(recv, r.explainer) && (types.IsInterface(recv) || !types.Implements(types.NewPointer(recv), r.explainer)) {
		return
	}
	if !inWiring(wiring, sel.Pos()) {
		r.found(sel.Sel.Pos(), "selects Explain of %s", recv)
	}
}

// unchecked reads, with go/parser, the non-test files of the given
// directories that the type-checked build left out: they must not name
// what the rule is about at all.
func (r *explainerRule) unchecked(t *testing.T, checkedFiles map[string]bool, dirs ...string) (read int) {
	t.Helper()
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			abs, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			if checkedFiles[abs] {
				return nil
			}
			read++
			file, err := parser.ParseFile(r.fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.Ident:
					if n.Name == newExplainer || n.Name == explainerPort || n.Name == "decide" {
						r.found(n.Pos(), "names %s in a file left out of the build that was type-checked", n.Name)
					}
				case *ast.SelectorExpr:
					if n.Sel.Name == "Explain" {
						r.found(n.Sel.Pos(), "selects Explain in a file left out of the build that was type-checked")
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	return read
}

// accessExplainer finds the port in the package of the ports.
func accessExplainer(t *testing.T, imp types.Importer) *types.Interface {
	t.Helper()
	ports, err := imp.Import(portsPackage)
	if err != nil {
		t.Fatal(err)
	}
	obj := ports.Scope().Lookup(explainerPort)
	if obj == nil {
		t.Fatalf("there is no ports.%s", explainerPort)
	}
	iface, ok := obj.Type().Underlying().(*types.Interface)
	if !ok {
		t.Fatalf("ports.%s is no interface", explainerPort)
	}
	return iface
}

// TestOnlyTheExplainUseCaseHoldsAnExplainer: in the source of internal/app,
// internal/adapters and cmd/cactusfs, nothing but the explanation use case
// and its one wiring names the explainer, as the comment above says; the
// authorizer is no explainer, and neither is ports.Authorizer.
func TestOnlyTheExplainUseCaseHoldsAnExplainer(t *testing.T) {
	t.Parallel()
	listed := listPackages(t)
	fset := token.NewFileSet()
	imp := exportImporter(fset, listed)
	explainer := accessExplainer(t, imp)
	rule := &explainerRule{fset: fset, explainer: explainer, appPath: appPackage, wiringPath: mainPackage}

	checkedFiles := map[string]bool{}
	var useCases checked
	scanned := 0
	for path, p := range listed {
		if path != appPackage && path != mainPackage && !strings.HasPrefix(path, adaptersPackage) {
			continue
		}
		c := checkListed(t, fset, imp, p)
		rule.scan(c)
		scanned++
		for _, name := range p.GoFiles {
			abs, err := filepath.Abs(filepath.Join(p.Dir, name))
			if err != nil {
				t.Fatal(err)
			}
			checkedFiles[abs] = true
		}
		if path == appPackage {
			useCases = c
		}
	}
	repository := filepath.Join("..", "..")
	read := rule.unchecked(t, checkedFiles,
		filepath.Join(repository, "internal", "app"), filepath.Join(repository, "internal", "adapters"), filepath.Join(repository, "cmd", "cactusfs"))
	for _, f := range rule.findings {
		t.Error(f)
	}

	// What the rule rests on: the authorizer is no explainer, nor is what
	// every other use case holds; NewExplainer makes one.
	if useCases.pkg == nil {
		t.Fatal("internal/app was not listed")
	}
	authz := useCases.pkg.Scope().Lookup("Authorizer")
	ports, err := imp.Import(portsPackage)
	if err != nil {
		t.Fatal(err)
	}
	if authz == nil || types.Implements(types.NewPointer(authz.Type()), explainer) || types.Implements(authz.Type(), explainer) {
		t.Error("*app.Authorizer is a ports.AccessExplainer: a type assertion of it finds the explainer")
	}
	if types.Implements(ports.Scope().Lookup("Authorizer").Type(), explainer) {
		t.Error("ports.Authorizer has the method Explain")
	}
	if fn, ok := useCases.pkg.Scope().Lookup(newExplainer).(*types.Func); !ok || !types.Identical(fn.Signature().Results().At(0).Type(), ports.Scope().Lookup(explainerPort).Type()) {
		t.Errorf("app.%s does not return a ports.%s", newExplainer, explainerPort)
	}
	// And that it looked: at the packages, and at the explainer's own
	// file, where it finds the names it is about.
	if scanned < 20 || rule.idents < 10000 || read == 0 {
		t.Errorf("the rule type-checked %d packages, looked at %d identifiers and read %d other files", scanned, rule.idents, read)
	}
	own := &explainerRule{fset: fset, explainer: explainer, appPath: appPackage, wiringPath: mainPackage, ownFile: true}
	own.scan(useCases)
	var inOwnFiles []string
	for _, f := range own.findings {
		inOwnFiles = append(inOwnFiles, filepath.Base(f.at.Filename)+" "+f.what)
	}
	for _, want := range []string{explainerFile + " names ports.AccessExplainer", explainerFile + " names the authorizer's decide", authorizerFile + " names the authorizer's decide"} {
		if !slices.ContainsFunc(inOwnFiles, func(s string) bool { return strings.HasPrefix(s, want) }) {
			t.Errorf("read as any other, the explainer's files do not show %q: %v", want, inOwnFiles)
		}
	}
}

// The source TestTheExplainerRuleSeesAHolder plants: a line marked
// "// want" must be found, as often as the number after it says (once
// when none), and no other line.
const (
	plantedAuthz = `package app

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/iam"
)

type Authorizer struct{}

func NewAuthorizer() *Authorizer { return &Authorizer{} }

func (a *Authorizer) decide(_ context.Context, _ iam.Request, _ *iam.Explanation) error { return nil }

func (a *Authorizer) Authorize(ctx context.Context, req iam.Request) error { return a.decide(ctx, req, nil) }

func (a *Authorizer) Allowed(ctx context.Context, req iam.Request) (bool, error) {
	return a.decide(ctx, req, nil) == nil, nil
}
`
	plantedExplainer = `package app

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

func NewExplainer(a *Authorizer) ports.AccessExplainer { return explainer{a} }

type explainer struct{ a *Authorizer }

func (e explainer) Explain(ctx context.Context, req iam.Request) (iam.Explanation, error) {
	var x iam.Explanation
	return x, e.a.decide(ctx, req, &x)
}
`
	plantedUseCase = `package app

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

type ExplainService struct{ explainer ports.AccessExplainer }

type ExplainServiceConfig struct{ Explainer ports.AccessExplainer }

func NewExplainService(e ports.AccessExplainer) *ExplainService {
	var kept ports.AccessExplainer = e
	return &ExplainService{explainer: kept}
}

func (s *ExplainService) Answer(ctx context.Context, req iam.Request) bool {
	x, err := s.explainer.Explain(ctx, req)
	return err == nil && x.Allowed
}
`
	plantedHolders = `package app

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

type PlantSource interface{ Get() ports.AccessExplainer } // want

type PlantGetter func() ports.AccessExplainer // want

type Wide interface {
	ports.Authorizer
	Explain(context.Context, iam.Request) (iam.Explanation, error)
}

type PlantUseCase struct {
	src   PlantSource
	get   PlantGetter
	wide  Wide
	authz ports.Authorizer
}

func NewPlantUseCase(src PlantSource, get PlantGetter, wide Wide, a ports.Authorizer) *PlantUseCase {
	return &PlantUseCase{src: src, get: get, wide: wide, authz: a}
}

func (p *PlantUseCase) Do(ctx context.Context, req iam.Request) bool {
	a, _ := NewExplainer(NewAuthorizer()).Explain(ctx, req) // want 2
	b, _ := p.src.Get().Explain(ctx, req) // want
	c, _ := p.get().Explain(ctx, req) // want
	d, _ := p.wide.Explain(ctx, req) // want
	if e, ok := p.authz.(ports.AccessExplainer); ok { // want
		_, _ = e.Explain(ctx, req) // want
	}
	later := p.wide.Explain // want
	_ = later
	var x iam.Explanation
	_ = NewAuthorizer().decide(ctx, req, &x) // want
	return a.Allowed && b.Allowed && c.Allowed && d.Allowed
}
`
	plantedAdapter = `package noderpc

import (
	"context"

	"github.com/albievan/cactusfs/internal/domain/iam"
	"github.com/albievan/cactusfs/internal/ports"
)

type Server struct{ authz ports.Authorizer }

func (s *Server) Peek(ctx context.Context, req iam.Request) bool {
	if e, ok := s.authz.(ports.AccessExplainer); ok { // want
		x, _ := e.Explain(ctx, req) // want
		return x.Allowed
	}
	return false
}
`
	plantedWiring = `package main

import (
	app "example.com/fake/app"
	"example.com/fake/adapters/noderpc"
)

func wire(a *app.Authorizer) {
	_ = app.NewExplainService(app.NewExplainer(a))
	_ = app.ExplainServiceConfig{Explainer: app.NewExplainer(a)} // want
	e := app.NewExplainer(a) // want
	_ = e
	_ = &noderpc.Server{}
}

func main() {}
`
)

// TestTheExplainerRuleSeesAHolder holds the test above to account: use
// cases that come by an explainer - through a named interface or function
// type, by making one, by a type assertion, through an interface that
// repeats the method, by the authorizer's decide - an adapter that asserts
// its authorizer to one, and a second wiring are found out; the explainer's
// file, the explanation use case and its one wiring are not.
func TestTheExplainerRuleSeesAHolder(t *testing.T) {
	t.Parallel()
	const fakeApp, fakeAdapter, fakeMain = "example.com/fake/app", "example.com/fake/adapters/noderpc", "example.com/fake/cmd"
	listed := listPackages(t)
	fset := token.NewFileSet()
	imp := exportImporter(fset, listed)
	explainer := accessExplainer(t, imp)
	sources := map[string]string{
		"authz.go": plantedAuthz, "explainer.go": plantedExplainer, "explain.go": plantedUseCase, "plant.go": plantedHolders,
		"server.go": plantedAdapter, "main.go": plantedWiring,
	}
	parse := func(names ...string) []*ast.File {
		var files []*ast.File
		for _, name := range names {
			file, err := parser.ParseFile(fset, name, sources[name], parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, file)
		}
		return files
	}
	useCases, err := typeCheck(fset, imp, fakeApp, parse("authz.go", "explainer.go", "explain.go", "plant.go"))
	if err != nil {
		t.Fatal(err)
	}
	given := withPackages{Importer: imp, given: map[string]*types.Package{fakeApp: useCases.pkg}}
	adapter, err := typeCheck(fset, given, fakeAdapter, parse("server.go"))
	if err != nil {
		t.Fatal(err)
	}
	given.given[fakeAdapter] = adapter.pkg
	wiring, err := typeCheck(fset, given, fakeMain, parse("main.go"))
	if err != nil {
		t.Fatal(err)
	}
	rule := &explainerRule{fset: fset, explainer: explainer, appPath: fakeApp, wiringPath: fakeMain}
	for _, c := range []checked{useCases, adapter, wiring} {
		rule.scan(c)
	}

	want := map[string]int{}
	for name, src := range sources {
		for i, line := range strings.Split(src, "\n") {
			_, mark, ok := strings.Cut(line, "// want")
			if !ok {
				continue
			}
			n := 1
			if mark = strings.TrimSpace(mark); mark != "" {
				if n, err = strconv.Atoi(mark); err != nil {
					t.Fatal(err)
				}
			}
			want[fmt.Sprintf("%s:%d", name, i+1)] = n
		}
	}
	got := map[string]int{}
	for _, f := range rule.findings {
		got[fmt.Sprintf("%s:%d", f.at.Filename, f.at.Line)]++
	}
	for at, n := range want {
		if got[at] != n {
			t.Errorf("%s: found %d times, want %d", at, got[at], n)
		}
	}
	for at, n := range got {
		if want[at] == 0 {
			t.Errorf("%s: found %d times where nothing is to be found", at, n)
		}
	}
	if t.Failed() {
		for _, f := range rule.findings {
			t.Log(f)
		}
	}
	if rule.wired != 2 {
		t.Errorf("the rule counted %d wiring sites, want 2: the one allowed and a second", rule.wired)
	}
}
