package pgmo

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/radix29/pgmo/pgsql"
)

// matchName matches exactly: a name differing only by case is another
// object, never a fallback; more than one exact match is ErrAmbiguous.
func TestMatchName(t *testing.T) {
	type obj struct{ name string }
	key := func(o *obj) string { return o.name }
	lower, upper, umlaut, empty := &obj{"orders"}, &obj{"Orders"}, &obj{"Äbc"}, &obj{""}
	for _, tc := range []struct {
		items []*obj
		name  string
		want  *obj
		err   error
	}{
		{[]*obj{lower, upper}, "orders", lower, nil},
		{[]*obj{lower, upper}, "Orders", upper, nil},
		{[]*obj{lower, upper}, "ORDERS", nil, ErrNotFound},
		{[]*obj{upper}, "orders", nil, ErrNotFound}, // gosmo's fallback would answer upper
		{[]*obj{lower}, "Orders", nil, ErrNotFound},
		{[]*obj{umlaut}, "äbc", nil, ErrNotFound},
		{[]*obj{umlaut}, "Äbc", umlaut, nil},
		{[]*obj{lower, upper, {"orders"}}, "orders", nil, ErrAmbiguous},
		{nil, "orders", nil, ErrNotFound},
		{[]*obj{lower, empty}, "", empty, nil},
	} {
		got, err := matchName(tc.items, tc.name, key)
		if got != tc.want || err != tc.err {
			t.Errorf("matchName(%q) = %v, %v; want %v, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
}

// ============================================================
// The guard: no case folding of names
// ============================================================

// caseFoldingAllowed names, by "file:function", the code that folds case on
// purpose, each with why the text it folds is not an object name.
var caseFoldingAllowed = map[string]string{
	"connection_dsn.go:validateParams": "runtime parameter (GUC) and connection keyword names are case-insensitive to the server; they are not catalog names",
	"object_write.go:keywordLabel":     "a statement's object-type keyword (TABLE, MATERIALIZED VIEW) lowered for an error message; SQL grammar, not a name",
}

// caseFoldingDirs are the directories whose non-test Go files and SQL the
// guard reads: pgmo's own reads and its exported fake. internal/scram is
// left out — SASLprep maps a password, never a name.
var caseFoldingDirs = []string{".", "pgsql", "pgmotest"}

// TestNoCaseFoldingOfNames refuses case folding in pgmo: in Go
// (strings.EqualFold, strings.ToLower and the like) and in a SQL literal's
// code (lower(), upper(), ILIKE, ~*). PostgreSQL names are case-sensitive
// once stored (names.go), so a folded comparison of one is a wrong answer
// waiting for two names differing only by case. Code folding something that
// is not a name goes in caseFoldingAllowed with the reason.
func TestNoCaseFoldingOfNames(t *testing.T) {
	used := map[string]bool{}
	for _, dir := range caseFoldingDirs {
		names, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			for _, c := range goCaseFoldings(fset, f) {
				site := filepath.ToSlash(name) + ":" + c.fn
				if caseFoldingAllowed[site] != "" {
					used[site] = true
					continue
				}
				t.Errorf("%s: %s in %s folds case — names are matched exactly (names.go); fold typed input with pgsql.FoldIdent, or add %q to caseFoldingAllowed with why it is not a name", c.pos, c.call, c.fn, site)
			}
		}
	}
	for _, lit := range sqlLiterals(t) {
		for _, f := range sqlCaseFoldings(lit.sql) {
			t.Errorf("%s: SQL %s folds case — compare names with = (names.go)", lit.pos, f)
		}
	}
	for site, why := range caseFoldingAllowed {
		if why == "" {
			t.Errorf("caseFoldingAllowed[%q] has no reason", site)
		}
		if !used[site] {
			t.Errorf("caseFoldingAllowed[%q] matches no case folding; remove it", site)
		}
	}
}

// caseFolding is one Go call that folds case: where, in which function
// ("" at package level), and which.
type caseFolding struct {
	pos  token.Position
	fn   string
	call string
}

// foldingCalls are the standard library's case-folding functions, by
// package name as imported.
var foldingCalls = map[string][]string{
	"strings": {"EqualFold", "ToLower", "ToUpper", "ToTitle", "ToLowerSpecial", "ToUpperSpecial", "ToTitleSpecial", "Title"},
	"bytes":   {"EqualFold", "ToLower", "ToUpper", "ToTitle", "ToLowerSpecial", "ToUpperSpecial", "ToTitleSpecial", "Title"},
	"unicode": {"ToLower", "ToUpper", "ToTitle", "To", "SimpleFold"},
	"cases":   {"Fold", "Lower", "Upper", "Title"}, // golang.org/x/text/cases
}

// goCaseFoldings lists every call in f of a foldingCalls function, named
// after the function declaration holding it.
func goCaseFoldings(fset *token.FileSet, f *ast.File) []caseFolding {
	var out []caseFolding
	for _, decl := range f.Decls {
		fn := ""
		if d, ok := decl.(*ast.FuncDecl); ok {
			fn = d.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && slices.Contains(foldingCalls[pkg.Name], sel.Sel.Name) {
				out = append(out, caseFolding{fset.Position(sel.Pos()), fn, pkg.Name + "." + sel.Sel.Name})
			}
			return true
		})
	}
	return out
}

// sqlFolding matches, in code, the SQL that folds case: a call of lower,
// upper, initcap or casefold (bare or qualified), ILIKE, and the
// case-insensitive regex and LIKE operators.
var sqlFolding = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_$])(?:(lower|upper|initcap|casefold)\s*\(|(ilike)(?:$|[^a-z0-9_$]))|(!?~~?\*)`)

// sqlCaseFoldings lists what sqlFolding finds in sql's code, skipping
// literals, quoted identifiers and comments.
func sqlCaseFoldings(sql string) []string {
	var out []string
	for sp := range pgsql.Spans(sql) {
		if sp.Kind != pgsql.SpanCode {
			continue
		}
		for _, m := range sqlFolding.FindAllStringSubmatch(sql[sp.Start:sp.End], -1) {
			switch {
			case m[1] != "":
				out = append(out, strings.ToLower(m[1])+"()")
			case m[2] != "":
				out = append(out, "ILIKE")
			default:
				out = append(out, m[3])
			}
		}
	}
	return out
}

// The SQL scan: what it must flag and what it must leave alone.
func TestSQLCaseFoldingScan(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		want []string
	}{
		{`SELECT 1 FROM pg_catalog.pg_class c WHERE lower(c.relname) = $1`, []string{"lower()"}},
		{`WHERE pg_catalog.UPPER (n.nspname) = $1 OR initcap(x) = y`, []string{"upper()", "initcap()"}},
		{`WHERE c.relname ILIKE $1 OR c.relname NOT ilike $2`, []string{"ILIKE", "ILIKE"}},
		{`WHERE a ~* $1 OR a !~* $1 OR a ~~* $1 OR a !~~* $1`, []string{"~*", "!~*", "~~*", "!~~*"}},
		{`WHERE pg_catalog.casefold(x) = $1`, []string{"casefold()"}},
		// Fine: exact comparison, LIKE and ~, look-alike words, and anything
		// in a literal, a quoted identifier or a comment.
		{`WHERE c.relname = $1 AND c.relname LIKE $2 AND c.relname ~ $3`, nil},
		{`SELECT lowercase(x), x_lower(y), upper_bound, ilike_x, "lower"(z)`, nil},
		{`SELECT 'lower(x) ILIKE ~*', $$upper(y)$$ -- lower(z)
		  /* a ILIKE b */`, nil},
	} {
		if got := sqlCaseFoldings(tc.sql); !slices.Equal(got, tc.want) {
			t.Errorf("sqlCaseFoldings(%q) = %q, want %q", tc.sql, got, tc.want)
		}
	}
}

// The Go walk finds each folding call, in a function or at package level,
// and names the function; a planted EqualFold proves it is not blind.
func TestGoCaseFoldingScan(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "src.go", `package p
import ("bytes"; "strings"; "unicode")
var folded = strings.ToLower("X")
func byName(a, b string) bool { return strings.EqualFold(a, b) }
func other(b []byte, r rune) { _ = bytes.ToUpper(b); _ = unicode.SimpleFold(r); _ = strings.TrimSpace("x") }
`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range goCaseFoldings(fset, f) {
		got = append(got, fmt.Sprintf("%d %s %s", c.pos.Line, c.fn, c.call))
	}
	want := []string{"3  strings.ToLower", "4 byName strings.EqualFold", "5 other bytes.ToUpper", "5 other unicode.SimpleFold"}
	if !slices.Equal(got, want) {
		t.Errorf("goCaseFoldings = %q, want %q", got, want)
	}
}
