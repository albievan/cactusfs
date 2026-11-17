package web_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/albievan/cactusfs/internal/domain/iam"
)

// The corpus of the console's policy reader and writer (PLAN 9.7, ADR-0032
// Decision 5): documents the API's parsers accept, each of which the reader
// must take and the writer give back as the same document
// (web/src/policy/policy.test.ts). It is written by Go, from the parsers
// themselves, so that the console is held to the grammar the API reads and
// not to a copy of it.
//
// These tests live here, and not in internal/domain/iam, because they read
// and write files: the domain does no I/O (depguard forbids it os). The
// seeds of the parsers' fuzz targets are read from their sources - the
// arguments of each f.Add, and the files of testdata/fuzz - and the parsers
// are asked, through their exported API, which they accept.

const (
	// iamSources is the package whose fuzz seeds begin the corpus.
	iamSources = "../internal/domain/iam"
	// corpusDir and corpusFile are where the corpus is written.
	corpusDir  = "src/policy/testdata"
	corpusFile = "corpus.json"
	// corpusSeed is the seed of the documents the generator adds: fixed, so
	// that the corpus is the same bytes on every run.
	corpusSeed = 20261117
	// generatedPerKind is how many accepted documents of each kind the
	// generator adds.
	generatedPerKind = 100
	// writeCorpusVariable, set to 1, makes TestWriteBuilderCorpus write the
	// corpus instead of only building it: `make policy-corpus`.
	writeCorpusVariable = "CACTUSFS_WRITE_CORPUS"
)

// fuzzTargets are the fuzz targets whose accepted seeds are in the corpus,
// one of each parser.
func fuzzTargets() []string {
	return []string{"FuzzParsePolicy", "FuzzBucketPolicy", "FuzzSubBucketPolicy"}
}

// corpusEntry is one document of the corpus.
type corpusEntry struct {
	// Kind is the kind of document, as the console names it: identity,
	// bucket or sub-bucket.
	Kind string `json:"kind"`
	// Origin says where the document comes from: a seed of a fuzz target,
	// a file of its testdata, or the generator.
	Origin string `json:"origin"`
	// Bucket and SubBucket are the names the document was accepted for.
	Bucket    string `json:"bucket,omitempty"`
	SubBucket string `json:"sub_bucket,omitempty"`
	// Text is the document, as the parser was given it.
	Text string `json:"text"`
}

// builderCorpus is the file.
type builderCorpus struct {
	About   string        `json:"about"`
	Entries []corpusEntry `json:"entries"`
}

// accepts reports whether the parser of the entry's kind accepts it.
func (e corpusEntry) accepts() error {
	var err error
	switch e.Kind {
	case "identity":
		_, err = iam.ParsePolicy([]byte(e.Text))
	case "bucket":
		_, err = iam.ParseBucketPolicy([]byte(e.Text), e.Bucket)
	case "sub-bucket":
		_, err = iam.ParseSubBucketPolicy([]byte(e.Text), e.Bucket, e.SubBucket)
	default:
		err = fmt.Errorf("no kind %q", e.Kind)
	}
	return err
}

// TestWriteBuilderCorpus builds the corpus and holds it to what it must be:
// every entry accepted by its parser, every kind there, every accepted seed
// of the fuzz targets in it, and the same bytes when built again. With
// CACTUSFS_WRITE_CORPUS=1 (`make policy-corpus`) it then writes it to
// web/src/policy/testdata/corpus.json.
func TestWriteBuilderCorpus(t *testing.T) {
	content, entries := buildCorpus(t)
	kinds := map[string]int{}
	for _, entry := range entries {
		if err := entry.accepts(); err != nil {
			t.Errorf("%s (%s) is refused: %v", entry.Origin, entry.Kind, err)
		}
		kinds[entry.Kind]++
	}
	for _, kind := range []string{"identity", "bucket", "sub-bucket"} {
		if kinds[kind] < generatedPerKind {
			t.Errorf("%d documents of the kind %s, want at least %d", kinds[kind], kind, generatedPerKind)
		}
	}
	for _, seed := range fuzzSeeds(t) {
		if seed.accepts() != nil {
			continue
		}
		if !slices.ContainsFunc(entries, func(e corpusEntry) bool { return e.Kind == seed.Kind && e.Text == seed.Text }) {
			t.Errorf("%s is accepted and not in the corpus", seed.Origin)
		}
	}
	if again, _ := buildCorpus(t); !bytes.Equal(again, content) {
		t.Error("the corpus is not the same bytes when built again")
	}
	if os.Getenv(writeCorpusVariable) != "1" {
		return
	}
	root, err := os.OpenRoot(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile(corpusFile, content, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d documents to %s/%s", len(entries), corpusDir, corpusFile)
}

// TestBuilderCorpusIsCurrent: the corpus the console's tests read is the one
// the parsers and their seeds make now. When it is not, `make policy-corpus`
// writes it again.
func TestBuilderCorpusIsCurrent(t *testing.T) {
	want, _ := buildCorpus(t)
	root, err := os.OpenRoot(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	got, err := root.ReadFile(corpusFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s/%s is stale: run `make policy-corpus` (%s=1 go test ./web/ -run '^TestWriteBuilderCorpus$')", corpusDir, corpusFile, writeCorpusVariable)
	}
}

// buildCorpus makes the corpus: the accepted seeds of the fuzz targets, in
// their order, then the accepted documents of the generator; a document of a
// kind is there once. It returns the file's bytes and its entries.
func buildCorpus(t *testing.T) ([]byte, []corpusEntry) {
	t.Helper()
	var entries []corpusEntry
	seen := map[string]bool{}
	add := func(entry corpusEntry) {
		// A text the console holds is Unicode; one that is not valid UTF-8
		// is no text it could be given, nor one JSON could carry as it is.
		if !utf8.ValidString(entry.Text) || seen[entry.Kind+"\x00"+entry.Text] || entry.accepts() != nil {
			return
		}
		seen[entry.Kind+"\x00"+entry.Text] = true
		entries = append(entries, entry)
	}
	for _, seed := range fuzzSeeds(t) {
		add(seed)
	}
	g := &generator{state: corpusSeed}
	for _, kind := range []string{"identity", "bucket", "sub-bucket"} {
		made := 0
		for attempt := 0; made < generatedPerKind && attempt < 100*generatedPerKind; attempt++ {
			before := len(entries)
			entry := g.document(kind)
			entry.Origin = fmt.Sprintf("generated %s %d", kind, made+1)
			add(entry)
			made += len(entries) - before
		}
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	err := encoder.Encode(builderCorpus{
		About: "Policy documents the API's parsers accept, for the console's reader and writer (PLAN 9.7). " +
			"Written by TestWriteBuilderCorpus (web/policycorpus_test.go) with `make policy-corpus`; do not edit.",
		Entries: entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), entries
}

// fuzzSeeds reads the seeds of the fuzz targets: the arguments of each f.Add
// of the target, in order, then the files of its testdata/fuzz directory.
// Each is the document the target's parser is given, with the names it is
// given with.
func fuzzSeeds(t *testing.T) []corpusEntry {
	t.Helper()
	root, err := os.OpenRoot(iamSources)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	sources := root.FS()
	names, err := fs.Glob(sources, "*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		src, err := fs.ReadFile(sources, name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	var seeds []corpusEntry
	for _, target := range fuzzTargets() {
		fuzz, eval, err := findTarget(files, target)
		if err != nil {
			t.Fatal(err)
		}
		add := func(origin string, args []any) {
			entry, err := fuzz.entry(eval, args)
			if err != nil {
				t.Fatalf("%s: %v", origin, err)
			}
			entry.Origin = origin
			seeds = append(seeds, entry)
		}
		seedArgs, err := fuzz.seeds(eval)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		for i, args := range seedArgs {
			add(fmt.Sprintf("%s seed %d", target, i+1), args)
		}
		dir := "testdata/fuzz/" + target
		found, err := fs.ReadDir(sources, dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range found {
			content, err := fs.ReadFile(sources, dir+"/"+file.Name())
			if err != nil {
				t.Fatal(err)
			}
			args, err := readFuzzFile(eval, content)
			if err != nil {
				t.Fatalf("%s/%s: %v", dir, file.Name(), err)
			}
			add(dir+"/"+file.Name(), args)
		}
	}
	return seeds
}

// fuzzTarget is a fuzz target as its source has it.
type fuzzTarget struct {
	name string
	// adds are its f.Add calls, in order.
	adds []*ast.CallExpr
	// params are the names of the parameters of its function after t, which
	// the arguments of f.Add are given to.
	params []string
	// parse is the call of the parser in its function.
	parse *ast.CallExpr
	kind  string
	// locals are the names declared anywhere in its body. An argument of
	// f.Add that names one is refused: it would be read as the package's
	// declaration of that name, which is not what the target is given.
	locals map[string]bool
}

// seeds evaluates the arguments of each f.Add, in order.
func (f fuzzTarget) seeds(eval *evaluator) ([][]any, error) {
	var out [][]any
	for i, call := range f.adds {
		for _, arg := range call.Args {
			if name := localName(arg, f.locals); name != "" {
				return nil, fmt.Errorf("seed %d names %s, which the target declares itself", i+1, name)
			}
		}
		args, err := eval.allAt(call.Args, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("seed %d: %w", i+1, err)
		}
		out = append(out, args)
	}
	return out, nil
}

// localName returns the first name x refers to that is one of locals; ""
// when there is none. The name after a selector's dot is no reference.
func localName(x ast.Expr, locals map[string]bool) string {
	found := ""
	ast.Inspect(x, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		switch n := n.(type) {
		case *ast.SelectorExpr:
			found = localName(n.X, locals)
			return false
		case *ast.Ident:
			if locals[n.Name] {
				found = n.Name
			}
		}
		return true
	})
	return found
}

// declaredNames returns every name declared in body: by :=, by var or
// const, as a key or value of a range, and as a parameter of a function
// literal.
func declaredNames(body *ast.BlockStmt) map[string]bool {
	names := map[string]bool{}
	define := func(xs ...ast.Expr) {
		for _, x := range xs {
			if id, ok := x.(*ast.Ident); ok && id.Name != "_" {
				names[id.Name] = true
			}
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				define(n.Lhs...)
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				define(n.Key, n.Value)
			}
		case *ast.DeclStmt:
			if gen, ok := n.Decl.(*ast.GenDecl); ok {
				for _, spec := range gen.Specs {
					switch spec := spec.(type) {
					case *ast.ValueSpec:
						for _, name := range spec.Names {
							define(name)
						}
					case *ast.TypeSpec:
						define(spec.Name)
					}
				}
			}
		case *ast.FuncLit:
			for _, name := range paramNames(n.Type.Params) {
				names[name] = true
			}
		}
		return true
	})
	return names
}

// entry is the document a seed gives the target's parser, with the names.
func (f fuzzTarget) entry(eval *evaluator, args []any) (corpusEntry, error) {
	if len(args) != len(f.params) {
		return corpusEntry{}, fmt.Errorf("%d arguments for %d parameters", len(args), len(f.params))
	}
	env := map[string]any{}
	for i, name := range f.params {
		env[name] = args[i]
	}
	values, err := eval.allAt(f.parse.Args, env, 0)
	if err != nil {
		return corpusEntry{}, err
	}
	text := make([]string, len(values))
	for i, value := range values {
		s, ok := value.(string)
		if !ok {
			return corpusEntry{}, fmt.Errorf("argument %d of the parser is no string", i)
		}
		text[i] = s
	}
	entry := corpusEntry{Kind: f.kind, Text: text[0]}
	if len(text) > 1 {
		entry.Bucket = text[1]
	}
	if len(text) > 2 {
		entry.SubBucket = text[2]
	}
	return entry, nil
}

// parsers are the kinds of document of the parsers a target may call.
func parsers() map[string]string {
	return map[string]string{"ParsePolicy": "identity", "ParseBucketPolicy": "bucket", "ParseSubBucketPolicy": "sub-bucket"}
}

// findTarget finds the fuzz target name among files, and an evaluator of the
// declarations of its package.
func findTarget(files []*ast.File, name string) (fuzzTarget, *evaluator, error) {
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != name {
				continue
			}
			target := fuzzTarget{name: name, locals: declaredNames(fn.Body)}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch {
				case isSelector(call.Fun, "f", "Add"):
					target.adds = append(target.adds, call)
				case isSelector(call.Fun, "f", "Fuzz") && len(call.Args) == 1:
					if lit, ok := call.Args[0].(*ast.FuncLit); ok && len(paramNames(lit.Type.Params)) > 0 {
						target.params = paramNames(lit.Type.Params)[1:]
						target.parse, target.kind = findParse(lit.Body)
					}
				}
				return true
			})
			if len(target.adds) == 0 || target.parse == nil {
				return fuzzTarget{}, nil, fmt.Errorf("%s: no f.Add, or no call of a parser in its f.Fuzz", name)
			}
			return target, newEvaluator(files, file.Name.Name), nil
		}
	}
	return fuzzTarget{}, nil, fmt.Errorf("no fuzz target %s in %s", name, iamSources)
}

// findParse finds the first call of a parser of package iam in body.
func findParse(body *ast.BlockStmt) (*ast.CallExpr, string) {
	var found *ast.CallExpr
	kind := ""
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || found != nil {
			return found == nil
		}
		for parse, k := range parsers() {
			if isSelector(call.Fun, "iam", parse) {
				found, kind = call, k
			}
		}
		return found == nil
	})
	return found, kind
}

func isSelector(x ast.Expr, pkg, name string) bool {
	sel, ok := x.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

func paramNames(list *ast.FieldList) []string {
	var names []string
	for _, field := range list.List {
		for _, name := range field.Names {
			names = append(names, name.Name)
		}
	}
	return names
}

// readFuzzFile reads a file of a fuzz target's testdata ("go test fuzz v1").
func readFuzzFile(eval *evaluator, content []byte) ([]any, error) {
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) == 0 || lines[0] != "go test fuzz v1" {
		return nil, errors.New("not a file of the fuzz corpus")
	}
	var args []any
	for _, line := range lines[1:] {
		x, err := parser.ParseExpr(line)
		if err != nil {
			return nil, err
		}
		value, err := eval.eval(x, nil)
		if err != nil {
			return nil, err
		}
		args = append(args, value)
	}
	return args, nil
}

// evaluator evaluates the expressions a seed is written with: string
// literals, the conversions []byte(s) and string(s), +, strings.Join, the
// string constants and variables of the package, and calls of its functions
// whose body is one return of such an expression. Anything else is an
// error: a seed this cannot read fails the test, never leaves the corpus.
// Nothing is run: it reads syntax trees and joins strings.
type evaluator struct {
	values map[string]ast.Expr
	funcs  map[string]*ast.FuncDecl
}

func newEvaluator(files []*ast.File, pkg string) *evaluator {
	e := &evaluator{values: map[string]ast.Expr{}, funcs: map[string]*ast.FuncDecl{}}
	for _, file := range files {
		if file.Name.Name != pkg {
			continue
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					e.funcs[d.Name.Name] = d
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok || len(value.Values) != len(value.Names) {
						continue
					}
					for i, name := range value.Names {
						e.values[name.Name] = value.Values[i]
					}
				}
			}
		}
	}
	return e
}

// maxEvalDepth bounds the evaluation, which a declaration that names itself
// would not end.
const maxEvalDepth = 64

// allAt evaluates each of xs at the depth of the expression they are in.
func (e *evaluator) allAt(xs []ast.Expr, env map[string]any, depth int) ([]any, error) {
	out := make([]any, len(xs))
	for i, x := range xs {
		value, err := e.evalAt(x, env, depth)
		if err != nil {
			return nil, err
		}
		out[i] = value
	}
	return out, nil
}

func (e *evaluator) eval(x ast.Expr, env map[string]any) (any, error) {
	return e.evalAt(x, env, 0)
}

func (e *evaluator) str(x ast.Expr, env map[string]any, depth int) (string, error) {
	value, err := e.evalAt(x, env, depth)
	if err != nil {
		return "", err
	}
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%T is no string", x)
	}
	return s, nil
}

func (e *evaluator) evalAt(x ast.Expr, env map[string]any, depth int) (any, error) {
	if depth > maxEvalDepth {
		return nil, errors.New("nested too deeply")
	}
	depth++
	switch x := x.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return nil, fmt.Errorf("a literal %s", x.Value)
		}
		return strconv.Unquote(x.Value)
	case *ast.ParenExpr:
		return e.evalAt(x.X, env, depth)
	case *ast.Ident:
		if value, ok := env[x.Name]; ok {
			return value, nil
		}
		if value, ok := e.values[x.Name]; ok {
			return e.evalAt(value, nil, depth)
		}
		return nil, fmt.Errorf("the name %s", x.Name)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return nil, fmt.Errorf("the operator %s", x.Op)
		}
		left, err := e.str(x.X, env, depth)
		if err != nil {
			return nil, err
		}
		right, err := e.str(x.Y, env, depth)
		return left + right, err
	case *ast.CallExpr:
		return e.call(x, env, depth)
	default:
		return nil, fmt.Errorf("an expression %T", x)
	}
}

func (e *evaluator) call(x *ast.CallExpr, env map[string]any, depth int) (any, error) {
	if x.Ellipsis.IsValid() {
		return nil, errors.New("a call that spreads a list")
	}
	if isConversion(x.Fun) && len(x.Args) == 1 {
		return e.str(x.Args[0], env, depth)
	}
	if isSelector(x.Fun, "strings", "Join") && len(x.Args) == 2 {
		list, err := e.evalAt(x.Args[0], env, depth)
		if err != nil {
			return nil, err
		}
		items, ok := list.([]string)
		if !ok {
			return nil, errors.New("strings.Join of no list")
		}
		sep, err := e.str(x.Args[1], env, depth)
		return strings.Join(items, sep), err
	}
	id, ok := x.Fun.(*ast.Ident)
	if !ok {
		return nil, fmt.Errorf("a call of %T", x.Fun)
	}
	fn, ok := e.funcs[id.Name]
	if !ok || len(fn.Body.List) != 1 {
		return nil, fmt.Errorf("a call of %s, which is not one return", id.Name)
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return nil, fmt.Errorf("a call of %s, which is not one return", id.Name)
	}
	args, err := e.allAt(x.Args, env, depth)
	if err != nil {
		return nil, err
	}
	bound := map[string]any{}
	i := 0
	for _, field := range fn.Type.Params.List {
		_, variadic := field.Type.(*ast.Ellipsis)
		for _, name := range field.Names {
			if variadic {
				var rest []string
				for _, arg := range args[min(i, len(args)):] {
					s, ok := arg.(string)
					if !ok {
						return nil, fmt.Errorf("an argument of %s is no string", id.Name)
					}
					rest = append(rest, s)
				}
				bound[name.Name] = rest
				i = len(args)
				continue
			}
			if i >= len(args) {
				return nil, fmt.Errorf("too few arguments for %s", id.Name)
			}
			bound[name.Name] = args[i]
			i++
		}
	}
	return e.evalAt(ret.Results[0], bound, depth)
}

// isConversion reports whether x is the type of the conversion []byte(s) or
// string(s).
func isConversion(x ast.Expr) bool {
	if id, ok := x.(*ast.Ident); ok {
		return id.Name == "string"
	}
	array, ok := x.(*ast.ArrayType)
	if !ok || array.Len != nil {
		return false
	}
	elt, ok := array.Elt.(*ast.Ident)
	return ok && elt.Name == "byte"
}

// generator makes policy documents of the three kinds, most of them
// accepted, in every form a field may be written and every layout: compact,
// indented, with tabs and CRLF; strings with and without escapes, beyond
// ASCII and beyond the basic plane; Sid absent, empty or null; values of
// aws:username null. Its numbers are a linear congruential generator of its
// own, not math/rand's, so that its documents are the same on every run and
// with every Go.
type generator struct {
	state uint32
}

// n returns a number from 0 up to n. The state is a uint32, so that the
// numbers are the same whatever the size of an int.
func (g *generator) n(n int) int {
	g.state = (g.state*1103515245 + 12345) & 0x7fffffff
	return int(g.state>>8) % n
}

func pick[T any](g *generator, values ...T) T { return values[g.n(len(values))] }

// node is a JSON value to be written: a string, nil for null, a list, or an
// object with its fields in order.
type node any

type field struct {
	name  string
	value node
}

type object []field

// shuffle puts the fields in another order, sometimes.
func (g *generator) shuffle(fields object) object {
	if g.n(3) != 0 {
		return fields
	}
	for i := len(fields) - 1; i > 0; i-- {
		j := g.n(i + 1)
		fields[i], fields[j] = fields[j], fields[i]
	}
	return fields
}

// oneOrMany returns values as one string or as a list.
func (g *generator) oneOrMany(values []node) node {
	if len(values) == 1 && g.n(2) == 0 {
		return values[0]
	}
	return values
}

func (g *generator) some(n int, pool ...string) []node {
	count := 1 + g.n(n)
	var out []node
	for range count {
		out = append(out, pick(g, pool...))
	}
	return out
}

const userAlice, userBob = "arn:aws:iam:::user/alice", "arn:aws:iam:::user/b.o-b_2"

func (g *generator) principal(kind string, effect string) node {
	if kind == "sub-bucket" && effect == "Allow" {
		return object{{"AWS", g.oneOrMany(g.some(2, userAlice, userBob))}}
	}
	switch g.n(4) {
	case 0:
		return "*"
	case 1:
		return object{{"AWS", "*"}}
	case 2:
		return object{{"AWS", g.oneOrMany(g.some(3, "*", userAlice, userBob))}}
	default:
		return object{{"AWS", userAlice}}
	}
}

func (g *generator) actions(kind, effect string) []node {
	switch kind {
	case "identity":
		return g.some(3, "*", "s3:*", "s3:GetObject", "s3:get*", "S3:PutObject", "s3:ListBucket", "admin:ListUsers", "admin:*", "admin:GetPolicyCatalogue", "s3:?etObject")
	case "bucket":
		return g.some(3, "s3:GetObject", "s3:ListBucket", "s3:*", "s3:Get*", "s3:PutObject", "s3:DeleteObject", "s3:GetBucketLocation")
	default:
		if effect == "Deny" {
			return g.some(2, "s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:GetObjectTagging")
		}
		return g.some(3, "s3:GetObject", "s3:PutObject", "s3:ListBucket", "s3:DeleteObject", "s3:GetObjectTagging", "S3:listbucketversions")
	}
}

func (g *generator) resources(kind string) []node {
	switch kind {
	case "identity":
		return g.some(3, "*", "arn:aws:s3:::photos", "arn:aws:s3:::photos/*", "arn:aws:s3:::photos/2026/é ü/*", "arn:cactusfs:admin:::user/*", "arn:cactusfs:admin:::policy/read-only")
	case "bucket":
		return g.some(3, "arn:aws:s3:::site", "arn:aws:s3:::site/*", "arn:aws:s3:::site/public/*", "arn:aws:s3:::site/日本/😀*")
	default:
		return g.some(2, "arn:aws:s3:::site", "arn:aws:s3:::site/reports/*", "arn:aws:s3:::site/reports/2026/\"q\"*")
	}
}

func (g *generator) condition() node {
	tests := []struct {
		operator, key string
		values        []string
		null          bool
	}{
		{"IpAddress", "aws:SourceIp", []string{"10.0.0.0/8", "192.168.1.1", "2001:db8::/32"}, false},
		{"NotIpAddress", "aws:SourceIp", []string{"172.16.0.0/12", "::1"}, false},
		{"Bool", "aws:SecureTransport", []string{"true", "false"}, false},
		{"DateGreaterThan", "aws:CurrentTime", []string{"2026-01-01T00:00:00Z"}, false},
		{"DateLessThan", "aws:CurrentTime", []string{"2027-06-30T12:00:00+02:00"}, false},
		{"StringEquals", "aws:username", []string{"alice", ""}, true},
		{"StringLike", "aws:username", []string{"a*", "?b", "é*"}, true},
		{"StringNotEquals", "aws:username", []string{"bob"}, true},
		{"StringNotLike", "aws:username", []string{"tmp-*"}, false},
	}
	var blocks object
	used := map[int]bool{}
	for range 1 + g.n(2) {
		i := g.n(len(tests))
		if used[i] {
			continue
		}
		used[i] = true
		test := tests[i]
		var values []node
		for range 1 + g.n(2) {
			if test.null && g.n(4) == 0 {
				values = append(values, nil)
			} else {
				values = append(values, pick(g, test.values...))
			}
		}
		blocks = append(blocks, field{test.operator, object{{test.key, g.oneOrMany(values)}}})
	}
	return blocks
}

func (g *generator) statement(kind string) node {
	effect := pick(g, "Allow", "Deny")
	var fields object
	switch g.n(5) {
	case 0:
		fields = append(fields, field{"Sid", ""})
	case 1:
		fields = append(fields, field{"Sid", pick(g, "s1", "Read ‘public’ \"files\" é😀", "a/b\\c\t")})
	case 2:
		fields = append(fields, field{"Sid", nil})
	}
	fields = append(fields, field{"Effect", effect})
	if kind != "identity" {
		fields = append(fields, field{"Principal", g.principal(kind, effect)})
	}
	fields = append(fields,
		field{"Action", g.oneOrMany(g.actions(kind, effect))},
		field{"Resource", g.oneOrMany(g.resources(kind))},
	)
	if g.n(2) == 0 {
		fields = append(fields, field{"Condition", g.condition()})
	}
	return g.shuffle(fields)
}

// document makes the text of one document of a kind.
func (g *generator) document(kind string) corpusEntry {
	var statements []node
	for range 1 + g.n(3) {
		statements = append(statements, g.statement(kind))
	}
	doc := g.shuffle(object{{"Version", iam.PolicyVersion}, {"Statement", statements}})
	layout := layout{g: g, style: g.n(4)}
	var out strings.Builder
	layout.space(&out, 0, true)
	layout.write(&out, doc, 0)
	layout.space(&out, 0, true)
	entry := corpusEntry{Kind: kind, Text: out.String()}
	if kind != "identity" {
		entry.Bucket = "site"
	}
	if kind == "sub-bucket" {
		entry.SubBucket = "reports"
	}
	return entry
}

// layout writes a node in one of four styles: compact, indented with two
// spaces, indented with tabs and CRLF, or white space anywhere.
type layout struct {
	g     *generator
	style int
}

func (l layout) space(out *strings.Builder, depth int, edge bool) {
	switch l.style {
	case 1:
		if !edge {
			out.WriteString("\n" + strings.Repeat("  ", depth))
		}
	case 2:
		if !edge {
			out.WriteString("\r\n" + strings.Repeat("\t", depth))
		}
	case 3:
		out.WriteString(pick(l.g, "", " ", "\t", "\n", "\r\n ", "  "))
	}
}

func (l layout) write(out *strings.Builder, n node, depth int) {
	switch v := n.(type) {
	case nil:
		out.WriteString("null")
	case string:
		l.string(out, v)
	case []node:
		out.WriteString("[")
		for i, item := range v {
			if i > 0 {
				out.WriteString(",")
			}
			l.space(out, depth+1, false)
			l.write(out, item, depth+1)
		}
		if len(v) > 0 {
			l.space(out, depth, false)
		}
		out.WriteString("]")
	case object:
		out.WriteString("{")
		for i, f := range v {
			if i > 0 {
				out.WriteString(",")
			}
			l.space(out, depth+1, false)
			l.string(out, f.name)
			if l.style == 3 {
				l.space(out, depth+1, false)
			}
			out.WriteString(":")
			if l.style != 0 {
				out.WriteString(" ")
			}
			l.write(out, f.value, depth+1)
		}
		if len(v) > 0 {
			l.space(out, depth, false)
		}
		out.WriteString("}")
	}
}

// string writes a JSON string, escaping some of its characters that need no
// escape: as \uXXXX (a character beyond the basic plane as its surrogate
// pair), and a slash as \/.
func (l layout) string(out *strings.Builder, s string) {
	out.WriteString(`"`)
	for _, r := range s {
		escape := l.style == 3 && l.g.n(6) == 0
		switch {
		case r == '"' || r == '\\':
			out.WriteString(`\` + string(r))
		case r == '\t' && !escape:
			out.WriteString(`\t`)
		case r < 0x20:
			out.WriteString(pick(l.g, fmt.Sprintf(`\u%04x`, r), fmt.Sprintf(`\u%04X`, r)))
		case r == '/' && escape:
			out.WriteString(`\/`)
		case escape && r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(out, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		case escape:
			fmt.Fprintf(out, `\u%04x`, r)
		default:
			out.WriteRune(r)
		}
	}
	out.WriteString(`"`)
}

// seedsOfSource reads the seeds of the fuzz target FuzzX of a source.
func seedsOfSource(t *testing.T, src string) ([][]any, error) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "x_test.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	target, eval, err := findTarget([]*ast.File{file}, "FuzzX")
	if err != nil {
		t.Fatal(err)
	}
	return target.seeds(eval)
}

// TestTheSeedReaderRefusesWhatItCannotRead: a helper that calls itself - by
// its return, through the argument of a call, or in a sum - ends in the
// evaluator's error, not in an overflow of the stack; a seed that names
// something the target declares itself is refused, not read as the
// package's declaration of that name; and what it can read, it reads.
func TestTheSeedReaderRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	const head = "package x\n\nconst doc = `{}`\n\n" +
		"func wrap(s string) string { return \"[\" + s + \"]\" }\n\n" +
		"func join(s ...string) string { return strings.Join(s, \",\") }\n\n"
	const fuzz = "\tf.Fuzz(func(t *testing.T, data []byte) { _, _ = iam.ParsePolicy(data) })\n}\n"
	const target = "func FuzzX(f *testing.F) {\n"
	tests := []struct {
		name, src, want, seed string
	}{
		{"read", target + "\tf.Add([]byte(wrap(join(doc, `x`) + doc)))\n" + fuzz, "", "[{},x{}]"},
		{"recursion by the return", "func loop(s string) string { return loop(s) }\n\n" + target + "\tf.Add([]byte(loop(doc)))\n" + fuzz, "nested too deeply", ""},
		{"recursion by an argument", "func loop(s string) string { return wrap(loop(s)) }\n\n" + target + "\tf.Add([]byte(loop(doc)))\n" + fuzz, "nested too deeply", ""},
		{"recursion in a sum", "func loop(s string) string { return loop(s) + \"\" }\n\n" + target + "\tf.Add([]byte(loop(doc)))\n" + fuzz, "nested too deeply", ""},
		{"a name of :=", target + "\tdoc := `[]`\n\tf.Add([]byte(doc))\n" + fuzz, "names doc", ""},
		{"a name of var", target + "\tvar doc = `[]`\n\tf.Add([]byte(wrap(doc)))\n" + fuzz, "names doc", ""},
		{"a value of a range", target + "\tfor _, doc := range []string{`[]`} {\n\t\tf.Add([]byte(doc))\n\t}\n" + fuzz, "names doc", ""},
		{"a key of a range", target + "\tfor doc := range 1 {\n\t\t_ = doc\n\t}\n\tf.Add([]byte(doc))\n" + fuzz, "names doc", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			seeds, err := seedsOfSource(t, head+tt.src)
			if tt.want == "" {
				if err != nil || len(seeds) != 1 || len(seeds[0]) != 1 || seeds[0][0] != tt.seed {
					t.Fatalf("seeds %q, %v; want %q", seeds, err, tt.seed)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v, want one saying %q", err, tt.want)
			}
		})
	}
}
