package pgmo

import (
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestMinimumServerVersionIsPostgreSQL15(t *testing.T) {
	if MinimumServerVersion != 150000 {
		t.Errorf("MinimumServerVersion = %d, want 150000", MinimumServerVersion)
	}
}

func TestServerVersionParts(t *testing.T) {
	cases := []struct {
		v            ServerVersion
		major, minor int
		str          string
	}{
		{PostgreSQL17, 17, 0, "PostgreSQL 17"},
		{170002, 17, 2, "PostgreSQL 17.2"},
		{150013, 15, 13, "PostgreSQL 15.13"},
	}
	for _, c := range cases {
		if c.v.Major() != c.major || c.v.Minor() != c.minor || c.v.String() != c.str {
			t.Errorf("%d: got %d, %d, %q; want %d, %d, %q",
				int(c.v), c.v.Major(), c.v.Minor(), c.v.String(), c.major, c.minor, c.str)
		}
	}
}

func TestAtLeastAndBefore(t *testing.T) {
	cases := []struct {
		num  int
		want bool // atLeast(num, PostgreSQL17)
	}{
		{150013, false},
		{160009, false},
		{170000, true}, // the introducing version itself
		{170002, true},
		{180001, true},
		{0, true}, // never read: treat as newest
	}
	for _, c := range cases {
		if got := atLeast(c.num, PostgreSQL17); got != c.want {
			t.Errorf("atLeast(%d, 17) = %v, want %v", c.num, got, c.want)
		}
		if got := before(c.num, PostgreSQL17); got == c.want {
			t.Errorf("before(%d, 17) = %v, want %v", c.num, got, !c.want)
		}
		if got := hasColumnSince(c.num, PostgreSQL17); got != c.want {
			t.Errorf("hasColumnSince(%d, 17) = %v, want %v", c.num, got, c.want)
		}
	}
}

func TestColSinceNamesTheColumnOnlyFromItsVersion(t *testing.T) {
	const col, zero = "io.read_bytes", "NULL::numeric"
	for _, c := range []struct {
		num  int
		want string
	}{
		{150013, zero}, {170006, zero}, {180000, col}, {180001, col}, {0, col},
	} {
		if got := colSince(c.num, PostgreSQL18, col, zero); got != c.want {
			t.Errorf("colSince(%d, 18) = %q, want %q", c.num, got, c.want)
		}
	}
}

func TestColUntilNamesTheColumnOnlyBeforeItWasRemoved(t *testing.T) {
	const col, gone = "io.op_bytes", "NULL::bigint"
	for _, c := range []struct {
		num  int
		want string
	}{
		{150013, col}, {170006, col}, {180000, gone}, {180001, gone}, {0, gone},
	} {
		if got := colUntil(c.num, PostgreSQL18, col, gone); got != c.want {
			t.Errorf("colUntil(%d, 18) = %q, want %q", c.num, got, c.want)
		}
	}
}

func TestExtensionAtLeast(t *testing.T) {
	cases := []struct {
		installed, since string
		want             bool
	}{
		{"1.10", "1.11", false},
		{"1.11", "1.11", true},
		{"1.12", "1.11", true},
		{"1.9", "1.10", false}, // numeric, not lexical: "1.9" > "1.10" as text
		{"1.10", "1.9", true},
		{"2.0", "1.12", true},
		{"1", "1.0", false}, // a shorter prefix sorts first
		{"", "1.12", true},  // never read: treat as newest
		{"1.x", "1.12", true},
	}
	for _, c := range cases {
		if got := extensionAtLeast(c.installed, c.since); got != c.want {
			t.Errorf("extensionAtLeast(%q, %q) = %v, want %v", c.installed, c.since, got, c.want)
		}
	}
}

// ============================================================
// Every gate goes through atLeast
// ============================================================

// versionConstName matches the identifiers that name a server version.
var versionConstName = regexp.MustCompile(`^(PostgreSQL\d+|MinimumServerVersion)$`)

// versionVarName matches a variable, field or accessor that holds a version:
// serverVersion, versionNum, s.version, extversion. A bare num is not
// matched — it is what version_gate.go calls its parameter, and elsewhere it
// is as likely a count.
var versionVarName = regexp.MustCompile(`(?i)version`)

// TestVersionGatesGoThroughAtLeast keeps every version comparison in
// version_gate.go (gosmo's C1: its gates were spelled five ways, and the one
// that forgot "0 is newest" blanked columns on servers that had them). It
// reads the source, so it covers gates no test reaches. pgmo has no gated
// reads yet; TestHandVersionComparisonScan shows what the scan finds.
func TestVersionGatesGoThroughAtLeast(t *testing.T) {
	fset, files := parsePackage(t)
	for name, f := range files {
		if name == "version_gate.go" {
			continue
		}
		for _, p := range handVersionComparisons(f) {
			t.Errorf("%s: %s", fset.Position(p.pos), p.what)
		}
	}
}

// The scan itself, on a source holding each shape: what it must flag and
// what it must leave alone.
func TestHandVersionComparisonScan(t *testing.T) {
	const src = `package p

func f(s *Server, v ServerVersion, n int, extversion string) {
	_ = s.versionNum >= int(PostgreSQL17)       // ordering on a version field
	_ = int(PostgreSQL16) <= n                  // ordering against a constant
	_ = v.Major() < 17                          // ordering on Major
	_ = s.versionNum == 0                       // the unread case atLeast owns
	_ = v == PostgreSQL17                       // exact release: misses every minor
	_ = extversion > "1.10"                     // extension versions sort as text
	_ = atLeast(n, PostgreSQL15)                // a gate at the floor
	_ = colSince(n, MinimumServerVersion, "c", "NULL::int")
	_ = colSince(n, v, "c", "NULL::int")        // version not a named constant
	_ = s.atLeast(PostgreSQL14)                 // below the floor, method form

	_ = atLeast(s.versionNum, PostgreSQL17)     // fine
	_ = s.before(PostgreSQL18)                  // fine
	_ = n >= 3                                  // not a version
	_ = v.Major() == 17                         // a major, not a gate
	_ = extversion == ""                        // "is it installed"
	_ = extensionAtLeast(extversion, "1.11")    // fine
}
`
	var got []string
	for _, p := range handVersionComparisons(parseSource(t, src)) {
		got = append(got, p.what)
	}
	want := []string{
		`compares a version by hand (s.versionNum >= int(PostgreSQL17)) — use atLeast/before, colSince/colUntil or extensionAtLeast`,
		`compares a version by hand (int(PostgreSQL16) <= n) — use atLeast/before, colSince/colUntil or extensionAtLeast`,
		`compares a version by hand (v.Major() < 17) — use atLeast/before, colSince/colUntil or extensionAtLeast`,
		`compares a version by hand (s.versionNum == 0) — use atLeast/before, colSince/colUntil or extensionAtLeast`,
		`compares a version by hand (v == PostgreSQL17) — use atLeast/before, colSince/colUntil or extensionAtLeast`,
		`compares a version by hand (extversion > "1.10") — use atLeast/before, colSince/colUntil or extensionAtLeast`,
		`atLeast gates on PostgreSQL15, at or below the floor: it could only ever be true`,
		`colSince gates on MinimumServerVersion, at or below the floor: it could only ever be true`,
		`colSince's version is not a named PostgreSQLnn constant, so no inventory check can read it`,
		`atLeast gates on PostgreSQL14, at or below the floor: it could only ever be true`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("handVersionComparisons found\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// versionProblem is one version comparison made outside version_gate.go.
type versionProblem struct {
	pos  token.Pos
	what string
}

// serverGateFuncs are the gate functions that take a ServerVersion constant,
// in function (atLeast(num, v)) or method (s.atLeast(v)) form.
var serverGateFuncs = []string{"atLeast", "before", "colSince", "colUntil", "hasColumnSince"}

// handVersionComparisons lists, in source order, f's comparisons that should
// be a gate function — an ordering on a version, an equality with 0 (the
// unread case) or with a release constant — and its gate calls whose version
// is not a supported named constant.
func handVersionComparisons(f *ast.File) []versionProblem {
	var out []versionProblem
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BinaryExpr:
			if handComparison(n) {
				out = append(out, versionProblem{n.Pos(), fmt.Sprintf(
					"compares a version by hand (%s) — use atLeast/before, colSince/colUntil or extensionAtLeast", exprText(n))})
			}
		case *ast.CallExpr:
			fn := calleeName(n)
			if !slices.Contains(serverGateFuncs, fn) {
				return true
			}
			c := versionConstArg(n)
			switch {
			case c < 0:
				out = append(out, versionProblem{n.Pos(), fn + "'s version is not a named PostgreSQLnn constant, so no inventory check can read it"})
			case !slices.Contains(supportedVersionNames(), constName(n.Args[c])):
				out = append(out, versionProblem{n.Pos(), fmt.Sprintf(
					"%s gates on %s, at or below the floor: it could only ever be true", fn, constName(n.Args[c]))})
			}
		}
		return true
	})
	return out
}

// handComparison reports whether b compares a version without a gate
// function.
func handComparison(b *ast.BinaryExpr) bool {
	x, y := versionish(b.X), versionish(b.Y)
	switch b.Op {
	case token.LSS, token.LEQ, token.GTR, token.GEQ:
		return x || y
	case token.EQL, token.NEQ:
		return isVersionConst(b.X) || isVersionConst(b.Y) || (x && isZero(b.Y)) || (y && isZero(b.X))
	}
	return false
}

// versionish reports whether e holds a server or extension version: a
// release constant, a name matching versionVarName, a Major/Minor call, or a
// conversion of one of those.
func versionish(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.ParenExpr:
		return versionish(e.X)
	case *ast.Ident:
		return versionConstName.MatchString(e.Name) || versionVarName.MatchString(e.Name)
	case *ast.SelectorExpr:
		return versionConstName.MatchString(e.Sel.Name) || versionVarName.MatchString(e.Sel.Name)
	case *ast.CallExpr:
		switch fn := e.Fun.(type) {
		case *ast.Ident: // a conversion: int(v), ServerVersion(n)
			return len(e.Args) == 1 && versionish(e.Args[0])
		case *ast.SelectorExpr:
			return fn.Sel.Name == "Major" || fn.Sel.Name == "Minor" || versionVarName.MatchString(fn.Sel.Name)
		}
	}
	return false
}

// isVersionConst reports whether e is a release constant, through any
// conversion.
func isVersionConst(e ast.Expr) bool {
	if c, ok := e.(*ast.CallExpr); ok && len(c.Args) == 1 {
		return isVersionConst(c.Args[0])
	}
	return versionConstName.MatchString(constName(e))
}

func isZero(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "0"
}

// calleeName is the called function's or method's own name: "atLeast" for
// both atLeast(n, v) and s.atLeast(v).
func calleeName(c *ast.CallExpr) string {
	switch fn := c.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// constName is the name e spells, "" for anything but an identifier or a
// selector's last part.
func constName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

// versionConstArg is the index of c's first argument naming a release
// constant, -1 when none does.
func versionConstArg(c *ast.CallExpr) int {
	for i, a := range c.Args {
		if versionConstName.MatchString(constName(a)) {
			return i
		}
	}
	return -1
}

// supportedVersionNames are the constants a gate may name, as the source
// spells them: "PostgreSQL16" for PostgreSQL16.
func supportedVersionNames() []string {
	var names []string
	for _, v := range supportedVersions {
		names = append(names, versionConst(v))
	}
	return names
}

// versionConst spells v as its constant: "PostgreSQL17".
func versionConst(v ServerVersion) string {
	return fmt.Sprintf("PostgreSQL%d", v.Major())
}

// exprText renders e back to source for a message.
func exprText(e ast.Expr) string {
	var sb strings.Builder
	if err := format.Node(&sb, token.NewFileSet(), e); err != nil {
		return fmt.Sprintf("%T", e)
	}
	return sb.String()
}
