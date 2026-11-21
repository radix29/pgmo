package pgmo

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/radix29/pgmo/pgsql"
)

// The golden files: testdata/version_gates/<name>.sql holds each gated read
// rendered on every supported release, so a gate can only change through a
// diff a human reads, and the SQL pgmo sends a release the lab does not run
// is still on record (gosmo's version_gate_golden_test.go).
//
// pgmo has no gated reads yet, so goldenQueries is empty and the directory
// holds only .gitkeep; each check below runs on a fixture as well, so the
// machinery is proven before the first read relies on it. A read that calls
// colSince, colUntil or hasColumnSince adds its builder here in the same
// change (TestEveryGatedCallSiteHasAGoldenQuery holds it to that), then runs
//
//	go test -run TestGatedQueriesMatchTheirGoldenFiles -updategolden
//
// and reads the diff — regenerating is not review.

// updateGolden rewrites the golden files instead of comparing against them.
var updateGolden = flag.Bool("updategolden", false, "rewrite the version-gate golden files")

// goldenVersions are the releases each query is rendered at: the floor to
// the newest pgmo knows.
var goldenVersions = []ServerVersion{PostgreSQL15, PostgreSQL16, PostgreSQL17, PostgreSQL18}

// goldenQuery is one gated statement builder, rendered per release.
//
// relations lists the catalog relations the query reads that carry gated
// columns, so the rendering is checked against the hand-written catalogGates
// rather than only against itself: a golden file regenerated from a builder
// whose `since` is wrong would otherwise pin the bug forever.
type goldenQuery struct {
	name      string
	relations []string

	// columns narrows which gated columns of those relations the query is
	// expected to name, for a relation read by more than one query with
	// different column lists. Empty means every gated column of the
	// relations listed.
	columns []string

	render func(num int) string
}

// goldenQueries are pgmo's gated reads. None exist yet (P2 brings the
// first); see the file comment.
var goldenQueries = []goldenQuery{}

// covers reports whether g is one of the column gates q is expected to
// render. A relation gate (g.column == "") is a FROM clause, not a column,
// and is held by the inventory's statement and live checks instead.
func (q goldenQuery) covers(g catalogGate) bool {
	if g.column == "" || (g.since == 0 && g.until == 0) || !slices.Contains(q.relations, g.relation) {
		return false
	}
	return len(q.columns) == 0 || slices.Contains(q.columns, g.column)
}

// renderGolden is a golden file's whole text: every release, in order,
// under a header naming it.
func renderGolden(q goldenQuery) string {
	var sb strings.Builder
	for _, v := range goldenVersions {
		fmt.Fprintf(&sb, "-- %s\n%s\n", v, strings.TrimRight(q.render(int(v)), "\n"))
	}
	return sb.String()
}

func goldenPath(dir, name string) string {
	return filepath.Join(dir, name+".sql")
}

const goldenDir = "testdata/version_gates"

// The fixture every self-test below uses: a pg_database read with a rename,
// a column added in 16 and one added in 17, and a pg_stat_io read with a
// column removed in 18.
var fixtureGoldenQuery = goldenQuery{
	name:      "fixture_database",
	relations: []string{"pg_database"},
	columns:   []string{"daticulocale", "datlocale", "daticurules", "dathasloginevt"},
	render: func(num int) string {
		return `SELECT d.oid, d.datname,
	` + colSince(num, PostgreSQL17, "d.datlocale", "d.daticulocale") + ` AS locale,
	` + colSince(num, PostgreSQL16, "d.daticurules", "NULL::text") + `,
	` + colSince(num, PostgreSQL17, "d.dathasloginevt", "false") + `,
	pg_catalog.format('%s, %s', d.datcollate, d.datctype)
FROM pg_catalog.pg_database d
ORDER BY d.datname`
	},
}

// ============================================================
// The files match the builders
// ============================================================

func TestGatedQueriesMatchTheirGoldenFiles(t *testing.T) {
	for _, q := range goldenQueries {
		t.Run(q.name, func(t *testing.T) {
			if *updateGolden {
				path := goldenPath(goldenDir, q.name)
				if err := os.WriteFile(path, []byte(renderGolden(q)), 0o644); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
				t.Logf("rewrote %s — read the diff before committing it", path)
				return
			}
			if msg := goldenMismatch(goldenDir, q); msg != "" {
				t.Error(msg)
			}
		})
	}
}

// A golden file no query renders any more is a check of nothing.
func TestEveryGoldenFileHasAQuery(t *testing.T) {
	for _, msg := range staleGoldenFiles(t, goldenDir, goldenQueries) {
		t.Error(msg)
	}
}

func TestGoldenFileMachinery(t *testing.T) {
	dir := t.TempDir()
	q := fixtureGoldenQuery
	if msg := goldenMismatch(dir, q); !strings.Contains(msg, "no such file") {
		t.Errorf("missing file: %q, want a read error", msg)
	}
	if err := os.WriteFile(goldenPath(dir, q.name), []byte(renderGolden(q)), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := goldenMismatch(dir, q); msg != "" {
		t.Errorf("fresh file: %s", msg)
	}
	crlf := strings.ReplaceAll(renderGolden(q), "\n", "\r\n")
	if err := os.WriteFile(goldenPath(dir, q.name), []byte(crlf), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg := goldenMismatch(dir, q); msg != "" {
		t.Errorf("CRLF checkout: %s", msg)
	}

	// A gate moved from 17 to 16 changes the 16 rendering, and only that.
	moved := q
	moved.render = func(num int) string {
		return strings.Replace(q.render(num), "d.daticulocale", colSince(num, PostgreSQL16, "d.datlocale", "d.daticulocale"), 1)
	}
	if msg := goldenMismatch(dir, moved); msg == "" {
		t.Error("moved gate: the golden file still matches")
	}
	for _, v := range goldenVersions {
		if differs := moved.render(int(v)) != q.render(int(v)); differs != (v == PostgreSQL16) {
			t.Errorf("moved gate: rendering on %s differs = %v", v, differs)
		}
	}

	if err := os.WriteFile(goldenPath(dir, "gone"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := staleGoldenFiles(t, dir, []goldenQuery{q})
	if len(got) != 1 || !strings.Contains(got[0], "gone.sql") {
		t.Errorf("stale files = %q, want gone.sql alone", got)
	}

	want := "-- PostgreSQL 15\nSELECT d.oid, d.datname,\n\td.daticulocale AS locale,\n\tNULL::text,\n\tfalse,"
	if r := renderGolden(q); !strings.HasPrefix(r, want) {
		t.Errorf("renderGolden starts\n%s\nwant\n%s", r[:min(len(r), len(want))], want)
	}
}

// goldenMismatch compares q's rendering with its file in dir; "" when they
// agree. A CRLF checkout reads the same as LF.
func goldenMismatch(dir string, q goldenQuery) string {
	path := goldenPath(dir, q.name)
	want, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("read %s: %v (run go test -run TestGatedQueriesMatchTheirGoldenFiles -updategolden to create it)", path, err)
	}
	got := renderGolden(q)
	if got == strings.ReplaceAll(string(want), "\r\n", "\n") {
		return ""
	}
	return fmt.Sprintf("rendered SQL differs from %s.\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
}

// staleGoldenFiles lists the .sql files in dir no query renders.
func staleGoldenFiles(t *testing.T, dir string, queries []goldenQuery) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".sql")
		if !slices.ContainsFunc(queries, func(q goldenQuery) bool { return q.name == name }) {
			out = append(out, p+": no golden query renders it — delete it, or restore its query")
		}
	}
	return out
}

// ============================================================
// The renderings agree with the inventory
// ============================================================

// The golden files are generated from the builders, so on their own they pin
// whatever the builders do — including a wrong `since`. This checks each
// rendering against catalogGates instead, which is written from the lab
// catalogs and never from the code.
func TestGoldenRenderingsAgreeWithTheGateInventory(t *testing.T) {
	for _, q := range goldenQueries {
		for _, msg := range inventoryDisagreements(q) {
			t.Error(msg)
		}
	}
}

// Every gated call site must be rendered by some golden query, or a gate
// exists that no golden file covers. The inventory runs ahead of the reads
// (P1.4.2 listed every gate before any read used one), so the converse —
// every inventory entry has a query — does not hold yet.
func TestEveryGatedCallSiteHasAGoldenQuery(t *testing.T) {
	fset, files := parsePackage(t)
	for name, f := range files {
		if name == "version_gate.go" {
			continue
		}
		for _, s := range gateCallSites(f) {
			if msg := uncoveredCallSite(s, goldenQueries); msg != "" {
				t.Errorf("%s: %s", fset.Position(s.pos), msg)
			}
		}
	}
}

func TestGoldenInventoryCheckOnTheFixture(t *testing.T) {
	if got := inventoryDisagreements(fixtureGoldenQuery); len(got) != 0 {
		t.Errorf("the fixture disagrees with the inventory:\n%s", strings.Join(got, "\n"))
	}

	// A wrong since: daticurules gated on 17 instead of 16.
	wrong := fixtureGoldenQuery
	wrong.render = func(num int) string {
		return strings.Replace(fixtureGoldenQuery.render(num), "d.daticurules", colSince(num, PostgreSQL17, "d.daticurules", "NULL::text"), 1)
	}
	got := inventoryDisagreements(wrong)
	if len(got) != 1 || !strings.Contains(got[0], "on PostgreSQL 16 names pg_database.daticurules = false, want true") {
		t.Errorf("wrong since: %q", got)
	}

	// No inventory entry at all.
	none := goldenQuery{name: "none", relations: []string{"pg_namespace"}, render: func(int) string { return "SELECT 1" }}
	if got := inventoryDisagreements(none); len(got) != 1 || !strings.Contains(got[0], "checks nothing but itself") {
		t.Errorf("no entries: %q", got)
	}

	// Call-site coverage: the fixture's gates are covered, others are not.
	f := parseSource(t, `package p
func f(n int) {
	_ = colSince(n, PostgreSQL17, "d.datlocale", "d.daticulocale")
	_ = colUntil(n, PostgreSQL18, "op_bytes", "NULL::bigint")
	_ = hasColumnSince(n, PostgreSQL16)
	_ = hasColumnSince(n, PostgreSQL18)
	_ = atLeast(n, PostgreSQL18)
}
`)
	var msgs []string
	for _, s := range gateCallSites(f) {
		if msg := uncoveredCallSite(s, []goldenQuery{fixtureGoldenQuery}); msg != "" {
			msgs = append(msgs, msg)
		}
	}
	want := []string{
		`colUntil(PostgreSQL18, "op_bytes"): no golden query renders op_bytes — add the read's builder to goldenQueries`,
		`hasColumnSince(PostgreSQL18): no golden query changes at PostgreSQL 18 — add the read's builder to goldenQueries`,
	}
	if strings.Join(msgs, "\n") != strings.Join(want, "\n") {
		t.Errorf("uncovered call sites\n%s\nwant\n%s", strings.Join(msgs, "\n"), strings.Join(want, "\n"))
	}
}

// inventoryDisagreements lists, for each gated column q covers and each
// release, where q names the column and the inventory says the release has
// it not — or the reverse.
func inventoryDisagreements(q goldenQuery) []string {
	var out []string
	gates := 0
	for _, g := range catalogGates {
		if !q.covers(g) {
			continue
		}
		gates++
		for _, v := range goldenVersions {
			named := namesColumn(q.render(int(v)), g.column)
			if want := catalogGateExpected(g, int(v)); named != want {
				out = append(out, fmt.Sprintf("%s on %s names %s.%s = %v, want %v (inventory: since %s, until %s)",
					q.name, v, g.relation, g.column, named, want, g.since, g.until))
			}
		}
	}
	if gates == 0 {
		out = append(out, fmt.Sprintf("%s has no inventory entry for any column of %v — the golden file checks nothing but itself", q.name, q.relations))
	}
	return out
}

// namesColumn reports whether sql names column outside its literals and
// comments, as an identifier rather than part of a longer one.
func namesColumn(sql, column string) bool {
	word := regexp.MustCompile(`(^|[^A-Za-z0-9_$])` + regexp.QuoteMeta(column) + `($|[^A-Za-z0-9_$])`)
	for sp := range pgsql.Spans(sql) {
		text := sql[sp.Start:sp.End]
		switch {
		case sp.IsIdent():
			if sp.Unescape(sql) == column {
				return true
			}
		case !sp.IsQuoted() && !sp.IsComment():
			if word.MatchString(text) {
				return true
			}
		}
	}
	return false
}

// uncoveredCallSite is why no query in queries renders s; "" when one does.
func uncoveredCallSite(s gateCallSite, queries []goldenQuery) string {
	switch s.fn {
	case "colSince", "colUntil":
		col := bareColumn(s.col)
		for _, q := range queries {
			for _, g := range catalogGates {
				if g.column == col && q.covers(g) {
					return ""
				}
			}
		}
		return fmt.Sprintf("%s(%s, %q): no golden query renders %s — add the read's builder to goldenQueries", s.fn, versionConst(s.version), s.col, col)
	case "hasColumnSince":
		// No column to look for: some query must render differently either
		// side of the release.
		for _, q := range queries {
			if q.render(int(s.version)-1) != q.render(int(s.version)) {
				return ""
			}
		}
		return fmt.Sprintf("hasColumnSince(%s): no golden query changes at %s — add the read's builder to goldenQueries", versionConst(s.version), s.version)
	}
	return "" // atLeast/before also gate statements and refusals, not only reads
}

// ============================================================
// Arity
// ============================================================

// A gate that drops a column instead of substituting one leaves the scan
// destination list one short, and nothing says so until a live run on that
// release (gosmo's A4). colSince prevents it by construction; this is the
// blanket rule for hasColumnSince and hand-built lists.
func TestGatedQueriesHaveTheSameArityAtEveryRelease(t *testing.T) {
	for _, q := range goldenQueries {
		if msg := arityProblem(q); msg != "" {
			t.Error(msg)
		}
	}
}

func TestArityCheckOnTheFixture(t *testing.T) {
	if msg := arityProblem(fixtureGoldenQuery); msg != "" {
		t.Errorf("fixture: %s", msg)
	}
	dropped := fixtureGoldenQuery
	dropped.render = func(num int) string {
		s := fixtureGoldenQuery.render(num)
		if !hasColumnSince(num, PostgreSQL16) {
			s = strings.Replace(s, "\n\tNULL::text,", "", 1)
		}
		return s
	}
	if msg := arityProblem(dropped); !strings.Contains(msg, "selects 5 expressions on PostgreSQL 15, 6 on PostgreSQL 16") {
		t.Errorf("dropped column: %q", msg)
	}
	empty := fixtureGoldenQuery
	empty.render = func(num int) string {
		return strings.Replace(fixtureGoldenQuery.render(num), "NULL::text", colSince(num, PostgreSQL16, "d.daticurules", ""), 1)
	}
	if msg := arityProblem(empty); !strings.Contains(msg, "expression 4 is empty") {
		t.Errorf("empty substitute: %q", msg)
	}
}

func TestSelectExprs(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
	}{
		{"SELECT a, b FROM t", []string{"a", "b"}},
		{"select f(a, b), 'x, from' AS \"from, y\" from t", []string{"f(a, b)", "'x, from' AS \"from, y\""}},
		{"WITH c AS (SELECT x, y FROM t) SELECT c.x -- , z\nFROM c", []string{"c.x -- , z"}},
		{"SELECT $q$a, FROM$q$, (SELECT 1 FROM u), b.selected, from_x FROM t", []string{"$q$a, FROM$q$", "(SELECT 1 FROM u)", "b.selected", "from_x"}},
		{"SELECT 1, 2", []string{"1", "2"}},
	}
	for _, c := range cases {
		got, err := selectExprs(c.sql)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("selectExprs(%q) = %q, %v; want %q", c.sql, got, err, c.want)
		}
	}
	for _, bad := range []string{"VALUES (1)", "SELECT (a, b FROM t", "SELECT a,, b FROM t"} {
		if _, err := selectExprs(bad); err == nil {
			t.Errorf("selectExprs(%q): no error", bad)
		}
	}
}

// arityProblem is why q's select list differs in length across releases; ""
// when it does not.
func arityProblem(q goldenQuery) string {
	want := -1
	for _, v := range goldenVersions {
		exprs, err := selectExprs(q.render(int(v)))
		if err != nil {
			return fmt.Sprintf("%s on %s: %v", q.name, v, err)
		}
		if want == -1 {
			want = len(exprs)
			continue
		}
		if len(exprs) != want {
			return fmt.Sprintf("%s selects %d expressions on %s, %d on %s — a Scan arity error on one of them",
				q.name, want, goldenVersions[0], len(exprs), v)
		}
	}
	return ""
}

// selectExprs splits sql's top-level select list — between its first
// top-level SELECT and the FROM that follows, or the end — at top-level
// commas. Literals, quoted identifiers and comments are skipped through
// pgsql.Spans, so a comma or keyword inside them does not count. An empty
// expression is an error: it is what a gate whose substitute is "" leaves.
func selectExprs(sql string) ([]string, error) {
	start, end, depth := -1, len(sql), 0
	var commas []int
	for sp := range pgsql.Spans(sql) {
		if sp.IsQuoted() || sp.IsComment() {
			continue
		}
		for i := sp.Start; i < sp.End; i++ {
			switch c := sql[i]; {
			case c == '(':
				depth++
			case c == ')':
				depth--
			case depth != 0:
			case c == ',' && start >= 0:
				commas = append(commas, i)
			case start < 0 && keywordAt(sql, i, "select"):
				start = i + len("select")
			case start >= 0 && keywordAt(sql, i, "from"):
				end = i
			}
			if end != len(sql) {
				break
			}
		}
		if end != len(sql) {
			break
		}
	}
	if depth != 0 && end == len(sql) {
		return nil, fmt.Errorf("unbalanced parentheses in %q", sql)
	}
	if start < 0 {
		return nil, fmt.Errorf("no top-level SELECT in %q", sql)
	}
	var out []string
	from := start
	for _, c := range append(commas, end) {
		expr := strings.TrimSpace(sql[from:c])
		if expr == "" {
			return nil, fmt.Errorf("expression %d is empty — a gate substituted nothing: %q", len(out)+1, sql)
		}
		out = append(out, expr)
		from = c + 1
	}
	return out, nil
}

// keywordAt reports whether the word kw (lower case) starts at sql[i],
// ignoring case, as a whole word.
func keywordAt(sql string, i int, kw string) bool {
	isWord := func(c byte) bool {
		return c == '_' || c == '$' || '0' <= c && c <= '9' || 'a' <= c|0x20 && c|0x20 <= 'z'
	}
	if i > 0 && (isWord(sql[i-1]) || sql[i-1] == '.') {
		return false
	}
	if j := i + len(kw); j > len(sql) || !strings.EqualFold(sql[i:j], kw) || j < len(sql) && isWord(sql[j]) {
		return false
	}
	return true
}
