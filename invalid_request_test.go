package pgmo

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The refusal builders (errors.go § Refusals): each reaches its own sentinel
// and no other, keeps its message exactly, and keeps a %w cause reachable.
func TestRefusalBuilders(t *testing.T) {
	sentinels := []error{ErrInvalidRequest, ErrUnsupported, ErrNotFound, ErrAmbiguous}
	cases := []struct {
		name  string
		build func(string, ...any) error
		kind  error
	}{
		{"invalidf", invalidf, ErrInvalidRequest},
		{"unsupportedf", unsupportedf, ErrUnsupported},
		{"notFoundf", notFoundf, ErrNotFound},
		{"ambiguousf", ambiguousf, ErrAmbiguous},
	}
	for _, tc := range cases {
		err := tc.build("pgmo: drop table %s: %d columns", `"s"."t"`, 0)
		if got, want := err.Error(), `pgmo: drop table "s"."t": 0 columns`; got != want {
			t.Errorf("%s: message %q, want %q", tc.name, got, want)
		}
		for _, s := range sentinels {
			if got, want := errors.Is(err, s), s == tc.kind; got != want {
				t.Errorf("%s: errors.Is(err, %v) = %v, want %v", tc.name, s, got, want)
			}
		}
		if errors.Unwrap(err) != nil {
			t.Errorf("%s: a single-error Unwrap found something; the chain is Unwrap() []error", tc.name)
		}
		if n := len(err.(*refusalError).Unwrap()); n != 1 {
			t.Errorf("%s: no %%w, yet %d errors in the chain, want 1", tc.name, n)
		}

		// %w keeps the cause: a sentinel, and a server error under it.
		wrapped := tc.build("pgmo: script %s: %w", "x", ErrDefinitionHidden)
		if !errors.Is(wrapped, tc.kind) || !errors.Is(wrapped, ErrDefinitionHidden) {
			t.Errorf("%s: %%w of ErrDefinitionHidden: %v reaches kind %v, hidden %v", tc.name,
				wrapped, errors.Is(wrapped, tc.kind), errors.Is(wrapped, ErrDefinitionHidden))
		}
		if got, want := wrapped.Error(), "pgmo: script x: "+ErrDefinitionHidden.Error(); got != want {
			t.Errorf("%s: wrapped message %q, want %q", tc.name, got, want)
		}
		server := tc.build("pgmo: read %s: %w", "x", pgErr("42501"))
		if pe, ok := AsPgError(server); !ok || pe.Code != "42501" {
			t.Errorf("%s: %%w of a server error: AsPgError = %v, %v", tc.name, pe, ok)
		}
		if !IsPermissionDenied(server) {
			t.Errorf("%s: %%w of 42501 no longer reads as permission denied", tc.name)
		}
	}
}

// ============================================================
// Every refusal reaches a sentinel
// ============================================================

// bareRefusalsAllowed lists, per file, the start of each format string a
// bare fmt.Errorf (no %w) or errors.New inside a function may use: a refusal
// about the server's state rather than the caller's arguments (invalidf),
// pgmo having no form for the request (unsupportedf) or something absent
// (notFoundf). An entry nothing matches any more fails, so the list only
// shrinks with the code.
var bareRefusalsAllowed = map[string][]string{
	"errors.go": {""}, // refusalf itself: its format is a parameter
	// A role row whose parallel arrays do not line up: the server's answer
	// is malformed, nothing the caller asked for.
	"role.go": {"membership arrays of different lengths", "database settings arrays of different lengths"},
	// An ACL's or default-privilege read's parallel arrays that do not line
	// up: likewise the server's answer, malformed.
	"privileges.go": {"privilege arrays of different lengths"},
	// A table row's parent schema and name arrays that do not line up.
	"table.go": {"name arrays of different lengths"},
	// A routine row whose argument arrays do not line up, or whose input
	// arguments are not its proargtypes.
	"routine.go": {"argument arrays of different lengths", "argument types "},
	// A composite type's attribute arrays, or a domain's constraint arrays,
	// that do not line up.
	"type.go":   {"attribute arrays of different lengths"},
	"domain.go": {"constraint arrays of different lengths"},
	// An extension row's configuration table arrays that do not line up.
	"extension.go": {"configuration arrays of different lengths"},
	// A text search configuration row's mapping arrays that do not line up.
	"text_search.go": {"mapping arrays of different lengths"},
}

// A call refused for its own arguments wraps ErrInvalidRequest, so a caller
// can tell "fix the request" from "the server said no" with errors.Is rather
// than by matching text (gosmo's G11, carried over). Every fmt.Errorf whose
// format has no %w, and every errors.New inside a function — an error that
// wraps nothing, so one pgmo decided on its own — must use a refusal builder
// or be on bareRefusalsAllowed. A package-level var ErrX = errors.New(...) is
// a sentinel, not a refusal, and is not scanned.
func TestEveryArgumentRefusalWrapsErrInvalidRequest(t *testing.T) {
	fset, files := parsePackage(t)
	seen := 0
	used := map[string]bool{} // file + "\x00" + allowed prefix
	for name, f := range files {
		for _, b := range bareRefusals(f) {
			seen++
			ok := false
			for _, prefix := range bareRefusalsAllowed[name] {
				if strings.HasPrefix(b.format, prefix) {
					used[name+"\x00"+prefix] = true
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("%s: %s(%q) wraps nothing: use invalidf, unsupportedf, notFoundf or ambiguousf, or add it to bareRefusalsAllowed if it is about the server's state",
					fset.Position(b.pos), b.fn, b.format)
			}
		}
	}
	if seen == 0 {
		t.Fatal("found no bare fmt.Errorf at all, not even refusalf's; the scan is broken")
	}
	for name, prefixes := range bareRefusalsAllowed {
		for _, prefix := range prefixes {
			if !used[name+"\x00"+prefix] {
				t.Errorf("%s: allowed %q matches no error any more; drop it from the list", name, prefix)
			}
		}
	}
}

// The scan itself, on a source holding each shape: what it must flag and
// what it must leave alone.
func TestBareRefusalScan(t *testing.T) {
	const src = `package p

var ErrX = errors.New("sentinel")

func f(err error) error {
	if err != nil {
		return fmt.Errorf("pgmo: read: %w", err)
	}
	_ = errors.New("bare " + "new")
	_ = invalidf("pgmo: built")
	return fmt.Errorf("pgmo: bare %d", 1)
}
`
	f := parseSource(t, src)
	var got []string
	for _, b := range bareRefusals(f) {
		got = append(got, b.fn+"("+b.format+")")
	}
	want := []string{"errors.New(bare new)", "fmt.Errorf(pgmo: bare %d)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("bareRefusals = %q, want %q", got, want)
	}
}

// bareRefusal is one error construction that wraps nothing.
type bareRefusal struct {
	pos    token.Pos
	fn     string // "fmt.Errorf" or "errors.New"
	format string // literalText of its first argument
}

// bareRefusals lists f's fmt.Errorf calls without %w and errors.New calls
// that are not a package-level var's value, in source order.
func bareRefusals(f *ast.File) []bareRefusal {
	sentinel := map[*ast.CallExpr]bool{}
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.VAR {
			for _, spec := range g.Specs {
				for _, v := range spec.(*ast.ValueSpec).Values {
					if call, ok := v.(*ast.CallExpr); ok {
						sentinel[call] = true
					}
				}
			}
		}
	}
	var out []bareRefusal
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 || sentinel[call] {
			return true
		}
		fn := selectorName(call.Fun)
		if fn != "fmt.Errorf" && fn != "errors.New" {
			return true
		}
		format := literalText(call.Args[0])
		if fn == "fmt.Errorf" && strings.Contains(format, "%w") {
			return true
		}
		out = append(out, bareRefusal{call.Pos(), fn, format})
		return true
	})
	return out
}

// ============================================================
// One prefix
// ============================================================

// TestNoWrapOfAnAlreadyPrefixedError enforces the prefix rule (errors.go §
// Refusals) by reading the source, so it covers refusals no test triggers.
// A function whose own return statements build a "pgmo: ..." error is
// prefixed; a call to one whose error is then wrapped in
// fmt.Errorf("pgmo: ...: %w") prints the prefix twice. The fix is on the
// callee's side when it is unexported (drop its prefix: the caller names the
// operation) and on the caller's when it is exported (return the error as it
// is).
//
// Callees are resolved without type-checking: a package function by name, a
// method through its receiver when that is the enclosing function's receiver
// or parameter, whose declared type is known. A call on a local variable is
// not resolved, so this finds the common shape, not every one.
func TestNoWrapOfAnAlreadyPrefixedError(t *testing.T) {
	fset, files := parsePackage(t)
	var parsed []*ast.File
	for _, f := range files {
		parsed = append(parsed, f)
	}
	for _, d := range doublePrefixes(parsed) {
		t.Errorf("%s: wraps %s's error in another \"pgmo:\" prefix", fset.Position(d.pos), d.callee)
	}
}

// The scan itself, on a source holding each shape. pgmo has no exported
// refusals yet, so the package alone cannot show the scan finds anything.
func TestDoublePrefixScan(t *testing.T) {
	const src = `package p

type Table struct{}

func (t *Table) Drop(ctx context.Context) error {
	return invalidf("pgmo: drop table: no name")
}

func lookup(name string) error {
	return refusalf(ErrNotFound, "pgmo: lookup %q: not there", name)
}

func helper() error {
	return invalidf("no name")
}

func (t *Table) Rename(ctx context.Context, to string) error {
	if err := t.Drop(ctx); err != nil {
		return fmt.Errorf("pgmo: rename table: %w", err)
	}
	err := lookup(to)
	if err != nil {
		return fmt.Errorf("pgmo: rename table: %w", err)
	}
	if err := helper(); err != nil {
		return fmt.Errorf("pgmo: rename table: %w", err)
	}
	if err := t.Drop(ctx); err != nil {
		return err
	}
	return nil
}
`
	f := parseSource(t, src)
	var got []string
	for _, d := range doublePrefixes([]*ast.File{f}) {
		got = append(got, d.callee)
	}
	want := []string{"Table.Drop", "lookup"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("doublePrefixes = %q, want %q", got, want)
	}
}

// doublePrefix is a call whose already-prefixed error is wrapped again.
type doublePrefix struct {
	pos    token.Pos
	callee string
}

// doublePrefixes finds, across files, every if-statement that checks the
// error of a call to a prefixed function and wraps it with another prefix.
func doublePrefixes(files []*ast.File) []doublePrefix {
	prefixed := map[string]bool{} // funcKey of every function returning a "pgmo:" error itself
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if _, ok := n.(*ast.FuncLit); ok {
					return false
				}
				if r, ok := n.(*ast.ReturnStmt); ok {
					for _, e := range r.Results {
						if call, ok := e.(*ast.CallExpr); ok && buildsPrefixedError(call) {
							prefixed[funcKey(fd)] = true
						}
					}
				}
				return true
			})
		}
	}

	var out []doublePrefix
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			types := identTypes(fd)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				b, ok := n.(*ast.BlockStmt)
				if !ok {
					return true
				}
				for i, st := range b.List {
					ifs, ok := st.(*ast.IfStmt)
					if !ok {
						continue
					}
					assign := ifs.Init
					if assign == nil && i > 0 {
						assign = b.List[i-1]
					}
					callee := assignedCallee(assign, types)
					if prefixed[callee] && wrapsWithPrefix(ifs.Body) {
						out = append(out, doublePrefix{ifs.Pos(), callee})
					}
				}
				return true
			})
		}
	}
	return out
}

// funcKey names a declaration as "Recv.Name" for a method, "Name" otherwise.
func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	return declTypeName(fd.Recv.List[0].Type) + "." + fd.Name.Name
}

// declTypeName is a named type's name through any pointer or type arguments;
// "" for anything else.
func declTypeName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return declTypeName(e.X)
	case *ast.IndexExpr:
		return declTypeName(e.X)
	case *ast.IndexListExpr:
		return declTypeName(e.X)
	case *ast.Ident:
		return e.Name
	}
	return ""
}

// identTypes maps a function's receiver and parameter names to their types.
func identTypes(fd *ast.FuncDecl) map[string]string {
	m := map[string]string{}
	for _, fl := range []*ast.FieldList{fd.Recv, fd.Type.Params} {
		if fl == nil {
			continue
		}
		for _, field := range fl.List {
			for _, n := range field.Names {
				m[n.Name] = declTypeName(field.Type)
			}
		}
	}
	return m
}

// assignedCallee is the funcKey of the call st assigns from, "" when st is no
// such assignment or the callee cannot be resolved.
func assignedCallee(st ast.Stmt, types map[string]string) string {
	a, ok := st.(*ast.AssignStmt)
	if !ok || len(a.Rhs) != 1 {
		return ""
	}
	call, ok := a.Rhs[0].(*ast.CallExpr)
	if !ok {
		return ""
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.IndexExpr:
		return declTypeName(fn.X)
	case *ast.SelectorExpr:
		if x, ok := fn.X.(*ast.Ident); ok && types[x.Name] != "" {
			return types[x.Name] + "." + fn.Sel.Name
		}
	}
	return ""
}

// buildsPrefixedError reports whether call builds an error whose text starts
// "pgmo:".
func buildsPrefixedError(call *ast.CallExpr) bool {
	format := 0
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		switch fn.Name {
		case "invalidf", "unsupportedf", "notFoundf", "ambiguousf":
		case "refusalf":
			format = 1
		default:
			return false
		}
	case *ast.SelectorExpr:
		if n := selectorName(fn); n != "fmt.Errorf" && n != "errors.New" {
			return false
		}
	default:
		return false
	}
	return len(call.Args) > format && strings.HasPrefix(literalText(call.Args[format]), "pgmo:")
}

// wrapsWithPrefix reports whether body holds fmt.Errorf("pgmo: ...%w...").
func wrapsWithPrefix(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 || selectorName(call.Fun) != "fmt.Errorf" {
			return true
		}
		format := literalText(call.Args[0])
		if strings.HasPrefix(format, "pgmo:") && strings.Contains(format, "%w") {
			found = true
		}
		return true
	})
	return found
}

// ============================================================
// Source helpers
// ============================================================

// parsePackage parses the package's non-test files, keyed by file name.
func parsePackage(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatal("no source files found")
	}
	return fset, files
}

// parseSource parses a test's inline source.
func parseSource(t *testing.T, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "src.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// selectorName is "pkg.Name" for a pkg.Name expression, "" otherwise.
func selectorName(e ast.Expr) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return x.Name + "." + sel.Sel.Name
}

// literalText is a format argument's text: a string literal, or a
// concatenation of them. Anything else reads as "", which only the errors.go
// entry matches.
func literalText(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.BasicLit:
		s, err := strconv.Unquote(e.Value)
		if err == nil {
			return s
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return literalText(e.X) + literalText(e.Y)
		}
	}
	return ""
}
