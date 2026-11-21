package pgmo

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// ============================================================
// scanRows, readByName, foundRow
// ============================================================

// scanInts is a scanRows scan of one int4 column.
func scanInts(scan func(...any) error) (int32, error) {
	var v int32
	return v, scan(&v)
}

// A list read's three failures — the query's, a scan's, the rows' Err after
// rows were delivered — carry the same message, and the cause stays
// reachable. An empty what returns the cause bare.
func TestScanRowsWrapsEveryFailureAlike(t *testing.T) {
	missing := pgErr("42P01")
	scanFailed := errors.New("cannot scan")
	for _, c := range []struct {
		name    string
		attempt attempt
		scan    func(func(...any) error) (int32, error)
		cause   error
	}{
		{"query", attempt{err: missing}, scanInts, missing},
		{"rows.Err after a row", attempt{rows: []int32{1, 2}, late: missing}, scanInts, missing},
		{"scan", attempt{rows: []int32{1}}, func(func(...any) error) (int32, error) { return 0, scanFailed }, scanFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &flakyQuerier{attempts: []attempt{c.attempt}}
			got, err := scanRows(t.Context(), f, "read things", c.scan, "SELECT 1")
			if got != nil || !errors.Is(err, c.cause) {
				t.Fatalf("scanRows = %v, %v; want nil and the cause %v", got, err, c.cause)
			}
			if want := "pgmo: read things: " + c.cause.Error(); err.Error() != want {
				t.Errorf("message %q, want %q", err.Error(), want)
			}

			f = &flakyQuerier{attempts: []attempt{c.attempt}}
			_, err = scanRows(t.Context(), f, "", c.scan, "SELECT 1")
			if !errors.Is(err, c.cause) || err.Error() != c.cause.Error() {
				t.Errorf("empty what: err = %v, want the cause bare", err)
			}
		})
	}
}

// scanRows collects every row in order, an empty result is a nil slice, and
// the whole read is retried as query's is: rows the failed attempt delivered
// are not kept.
func TestScanRowsCollects(t *testing.T) {
	f := &flakyQuerier{attempts: []attempt{{rows: []int32{1}, late: pgErr("40P01")}, {rows: []int32{1, 2, 3}}}}
	got, err := scanRows(t.Context(), f, "read things", scanInts, "SELECT 1")
	if err != nil || !slices.Equal(got, []int32{1, 2, 3}) || f.calls != 2 {
		t.Errorf("scanRows = %v, %v after %d statements; want [1 2 3], nil after 2", got, err, f.calls)
	}

	f = &flakyQuerier{attempts: []attempt{{}}}
	got, err = scanRows(t.Context(), f, "read things", scanInts, "SELECT 1")
	if err != nil || got != nil {
		t.Errorf("empty: scanRows = %#v, %v; want nil, nil", got, err)
	}
}

// readByName hands the owner to the family's scan, turns no row into the
// caller's notFound (not retried), and wraps any other failure.
func TestReadByName(t *testing.T) {
	type owner struct{ name string }
	type thing struct {
		owner *owner
		v     int32
	}
	scanThing := func(o *owner, scan func(...any) error) (*thing, error) {
		th := &thing{owner: o}
		return th, scan(&th.v)
	}
	o := &owner{name: "db"}
	notFound := notFoundf("pgmo: thing %q: no such thing", "x")
	read := func(f *flakyQuerier) (*thing, error) {
		return readByName(t.Context(), f, o, scanThing, "SELECT 1", nil, notFound, `read thing "x"`)
	}

	f := &flakyQuerier{attempts: []attempt{{err: pgErr("40P01")}, {rows: []int32{7}}}}
	got, err := read(f)
	if err != nil || got.v != 7 || got.owner != o || f.calls != 2 {
		t.Errorf("found: readByName = %+v, %v after %d statements; want v 7, owner set, after 2", got, err, f.calls)
	}

	f = &flakyQuerier{attempts: []attempt{{}, {rows: []int32{7}}}}
	got, err = read(f)
	if err != notFound || got != nil || f.calls != 1 {
		t.Errorf("no row: readByName = %v, %v after %d statements; want nil, notFound after 1", got, err, f.calls)
	}

	missing := pgErr("42501")
	f = &flakyQuerier{attempts: []attempt{{err: missing}}}
	got, err = read(f)
	if got != nil || !errors.Is(err, missing) || errors.Is(err, ErrNotFound) {
		t.Fatalf("failed read: readByName = %v, %v; want nil, the 42501 and not ErrNotFound", got, err)
	}
	if want := `pgmo: read thing "x": ` + missing.Error(); err.Error() != want {
		t.Errorf("failed read: message %q, want %q", err.Error(), want)
	}
}

// foundRow and rowErr: a value passes through, pgx.ErrNoRows — wrapped or
// not — is notFound, anything else is wrapped once, and nil stays nil.
func TestFoundRow(t *testing.T) {
	notFound := notFoundf("pgmo: thing: no such thing")
	if v, err := foundRow(3, nil, notFound, "read thing"); v != 3 || err != nil {
		t.Errorf("found: foundRow = %v, %v", v, err)
	}
	for _, noRow := range []error{pgx.ErrNoRows, fmt.Errorf("scan: %w", pgx.ErrNoRows)} {
		if v, err := foundRow(3, noRow, notFound, "read thing"); v != 0 || err != notFound {
			t.Errorf("no row (%v): foundRow = %v, %v; want 0, notFound", noRow, v, err)
		}
	}
	missing := pgErr("42P01")
	if err := rowErr(missing, notFound, "read thing"); !errors.Is(err, missing) || err.Error() != "pgmo: read thing: "+missing.Error() {
		t.Errorf("rowErr = %v, want the 42P01 wrapped once", err)
	}
	if err := rowErr(missing, notFound, ""); err != missing {
		t.Errorf("rowErr with empty what = %v, want the error bare", err)
	}
	if err := rowErr(nil, notFound, "read thing"); err != nil {
		t.Errorf("rowErr(nil) = %v", err)
	}
}

// ============================================================
// Guards
// ============================================================

// rawRowAccess lists, per file, the row access it may do itself: the
// helpers' own bodies, and databaseQuerier, a Querier forwarding to the
// pool it leases. Everything else reads through rows.go's helpers.
var rawRowAccess = map[string][]string{
	"statement.go":      {".Query", ".QueryRow", "pgx.CollectRows", "queryRow"},
	"rows.go":           {"query", "queryRow"},
	"database_pools.go": {".Query", ".QueryRow"},
}

// Every read goes through the helpers (CLAUDE.md § Conventions, Row
// iteration): no hand-rolled rows.Next() loop, whose Close or Err is one
// omission away, and no list read on query directly, which would leave its
// error for each caller to wrap — or not.
func TestRowIterationGoesThroughTheHelpers(t *testing.T) {
	fset, files := parsePackage(t)
	for _, v := range rawRowAccesses(fset, files, rawRowAccess) {
		t.Error(v)
	}
}

// TestRawRowAccessScan is the guard's self-test on planted code: each raw
// form is caught, in a file not allowed it, and only there.
func TestRawRowAccessScan(t *testing.T) {
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for name, src := range map[string]string{
		"planted.go": `package pgmo
func planted(ctx context.Context, q Querier) {
	rows, _ := q.Query(ctx, "SELECT 1")
	for rows.Next() {
	}
	_ = q.QueryRow(ctx, "SELECT 1").Scan(&v)
	_, _ = pgx.CollectRows(rows, pgx.RowTo[int])
	_, _ = pgx.CollectOneRow(rows, pgx.RowTo[int])
	_ = pgx.ForEachRow(rows, nil, nil)
	_, _ = query(ctx, q, scanInt, "SELECT 1")
	_, _ = query[int32](ctx, q, scanInt, "SELECT 1")
	_ = queryRow(ctx, q, nil, "SELECT 1")
	_ = queryRowScan(ctx, q, "SELECT 1", nil, &v)
	_, _ = scanRows(ctx, q, "x", scanInts, "SELECT 1")
	for sp := range spans { sp.Next() }
}`,
		"statement.go": `package pgmo
func allowed(ctx context.Context, q Querier) {
	rows, _ := q.Query(ctx, "SELECT 1")
	_, _ = pgx.CollectRows(rows, nil)
	for rows.Next() {
	}
}`,
	} {
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = f
	}
	got := rawRowAccesses(fset, files, rawRowAccess)
	want := []string{
		"planted.go:3: .Query", "planted.go:4: .Next", "planted.go:6: .QueryRow",
		"planted.go:7: pgx.CollectRows", "planted.go:8: pgx.CollectOneRow", "planted.go:9: pgx.ForEachRow",
		"planted.go:10: query", "planted.go:11: query", "planted.go:12: queryRow",
		"statement.go:5: .Next",
	}
	for i := range got {
		got[i] = got[i][:strings.Index(got[i], " — ")]
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("flagged:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// rawRowAccesses reports each raw row access in files a file's allowed
// entry does not cover, sorted by position: a call of a Querier's Query or
// QueryRow (any method so named), a zero-argument Next() on anything but a
// loop's range value, a pgx row collector, or the query/queryRow helpers.
func rawRowAccesses(fset *token.FileSet, files map[string]*ast.File, allowed map[string][]string) []string {
	type hit struct {
		pos  token.Position
		form string
	}
	var hits []hit
	for name, f := range files {
		// A range-over-func loop's value is no rows; its Next is its own.
		rangeVars := map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if r, ok := n.(*ast.RangeStmt); ok {
				for _, e := range []ast.Expr{r.Key, r.Value} {
					if id, ok := e.(*ast.Ident); ok {
						rangeVars[id.Name] = true
					}
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			form := ""
			switch fun := unindex(call.Fun).(type) {
			case *ast.Ident:
				if fun.Name == "query" || fun.Name == "queryRow" {
					form = fun.Name
				}
			case *ast.SelectorExpr:
				switch name := selectorName(fun); {
				case name == "pgx.CollectRows", name == "pgx.CollectOneRow", name == "pgx.CollectExactlyOneRow", name == "pgx.ForEachRow":
					form = name
				case fun.Sel.Name == "Query", fun.Sel.Name == "QueryRow":
					form = "." + fun.Sel.Name
				case fun.Sel.Name == "Next" && len(call.Args) == 0:
					if x, ok := fun.X.(*ast.Ident); !ok || !rangeVars[x.Name] {
						form = ".Next"
					}
				}
			}
			if form != "" && !slices.Contains(allowed[name], form) {
				hits = append(hits, hit{fset.Position(call.Pos()), form})
			}
			return true
		})
	}
	sort.Slice(hits, func(i, j int) bool {
		a, b := hits[i].pos, hits[j].pos
		return a.Filename < b.Filename || a.Filename == b.Filename && a.Line < b.Line
	})
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = filepath.Base(h.pos.Filename) + ":" + strconv.Itoa(h.pos.Line) + ": " + h.form +
			" — read through scanRows, readByName or queryRowScan (rows.go); a list read's query, scan and rows.Err share one wrap there"
	}
	return out
}

// unindex is fun without explicit type arguments: query for query[T].
func unindex(fun ast.Expr) ast.Expr {
	switch x := fun.(type) {
	case *ast.IndexExpr:
		return x.X
	case *ast.IndexListExpr:
		return x.X
	}
	return fun
}

// rowScanArg is the scan argument's index for each helper whose scan runs
// while the rows hold their connection.
var rowScanArg = map[string]int{"scanRows": 3, "readByName": 3, "query": 2, "queryRow": 2}

// Never query inside a row loop (CLAUDE.md § Conventions): the scan a read
// helper is handed runs while the rows hold their connection, so a query
// there holds one connection per row while acquiring another — and runs
// again on a retry. This reads every scan passed to scanRows, readByName,
// query and queryRow — a func literal, or a package function by name — and
// each package function it calls, for a statement.
func TestNoQueryInsideARowScan(t *testing.T) {
	fset, files := parsePackage(t)
	found, scans := queriesInRowScans(fset, files)
	for _, v := range found {
		t.Error(v)
	}
	if scans == 0 {
		t.Fatal("no row scans found; the helper names are wrong and this test proves nothing")
	}
}

// TestQueryInRowScanScan is the guard's self-test on planted code: a scan
// that queries directly, through a package function, through the Server's
// routing, or with the read's ctx is caught; one that only scans is not.
func TestQueryInRowScanScan(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "planted.go", `package pgmo
func planted(ctx context.Context, s *Server, q Querier) {
	scanRows(ctx, q, "x", func(scan func(...any) error) (int, error) {
		var v int
		_ = queryRowScan(ctx, q, "SELECT 1", nil, &v)
		return v, scan(&v)
	}, "SELECT 1")
	readByName(ctx, q, s, scanChild, "SELECT 1", nil, nil, "x")
	query[int](ctx, q, func(row pgx.CollectableRow) (int, error) { return lookup(ctx, row) }, "SELECT 1")
	scanRows(ctx, q, "x", scanDeep, "SELECT 1")
	scanRows(ctx, q, "x", func(scan func(...any) error) (int, error) { var v int; return v, scan(fill(&v)...) }, "SELECT 1")
	queryRow(ctx, q, func(row pgx.Row) error { return row.Scan(&v) }, "SELECT 1")
}
func scanChild(s *Server, scan func(...any) error) (int, error) {
	_, err := s.on(context.Background()).Exec(context.Background(), "SELECT 1")
	return 0, err
}
func scanDeep(scan func(...any) error) (int, error) { return 0, helper(scan) }
func helper(scan func(...any) error) error {
	_, err := scanRows(context.Background(), nil, "", scanInts, "SELECT 1")
	return err
}
func fill(v *int) []any { return []any{v} }
`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	got, scans := queriesInRowScans(fset, map[string]*ast.File{"planted.go": f})
	for i := range got {
		got[i] = got[i][:strings.Index(got[i], " — ")]
	}
	want := []string{
		"planted.go:5: scanRows scan calls queryRowScan",
		"planted.go:9: query scan calls lookup(ctx, …)",
		"planted.go:15: readByName scan calls .Exec",
		"planted.go:15: readByName scan calls on",
		"planted.go:20: scanRows scan calls scanRows",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("flagged:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The six scans in planted, and helper's own scanRows of scanInts,
	// which is not in the planted file and so not followed.
	if scans != 7 {
		t.Errorf("row scans seen = %d, want 7", scans)
	}
}

// statementCalls are the calls that send a statement or reach something
// that does: pgmo's read and write helpers and routing, and a Querier's
// methods.
var statementCalls = map[string]bool{
	"query": true, "queryRow": true, "queryRowScan": true, "execScan": true,
	"scanRows": true, "readByName": true, "exec": true, "execIn": true, "execSecret": true,
	"on": true, "onDatabase": true, "DatabaseQuerier": true, "Querier": true,
	"Query": true, "QueryRow": true, "Exec": true, "CopyFrom": true, "Begin": true, "SendBatch": true,
}

// queriesInRowScans reports, sorted by position, each call inside a row
// scan that sends a statement: a statementCalls name, or any call handed
// the read's ctx. It follows package functions the scan calls by name. It
// returns how many row scans it saw.
func queriesInRowScans(fset *token.FileSet, files map[string]*ast.File) ([]string, int) {
	funcs := map[string]*ast.FuncDecl{}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Body != nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	found := map[string]bool{}
	scans := 0
	var check func(helper string, body ast.Node, seen map[string]bool)
	check = func(helper string, body ast.Node, seen map[string]bool) {
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fun := unindex(call.Fun).(type) {
			case *ast.Ident:
				name = fun.Name
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			}
			what := ""
			switch {
			case statementCalls[name]:
				what = name
				if _, ok := unindex(call.Fun).(*ast.SelectorExpr); ok && name[0] >= 'A' && name[0] <= 'Z' {
					what = "." + name
				}
			case slices.ContainsFunc(call.Args, func(a ast.Expr) bool { id, ok := a.(*ast.Ident); return ok && id.Name == "ctx" }):
				what = name + "(ctx, …)"
			}
			if what != "" {
				found[fset.Position(call.Pos()).String()+"\x00"+helper+" scan calls "+what] = true
			}
			if fd := funcs[name]; fd != nil && !seen[name] {
				if _, ok := call.Fun.(*ast.Ident); ok {
					seen[name] = true
					check(helper, fd.Body, seen)
				}
			}
			return true
		})
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := unindex(call.Fun).(*ast.Ident)
			if !ok {
				return true
			}
			i, ok := rowScanArg[id.Name]
			if !ok || i >= len(call.Args) {
				return true
			}
			switch scan := call.Args[i].(type) {
			case *ast.FuncLit:
				scans++
				check(id.Name, scan.Body, map[string]bool{})
			case *ast.Ident:
				scans++
				if fd := funcs[scan.Name]; fd != nil {
					check(id.Name, fd.Body, map[string]bool{scan.Name: true})
				}
			}
			return true
		})
	}
	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return positionLess(keys[i], keys[j]) })
	out := make([]string, len(keys))
	for i, k := range keys {
		pos, rest, _ := strings.Cut(k, "\x00")
		file, line, _ := strings.Cut(pos, ":")
		line, _, _ = strings.Cut(line, ":")
		out[i] = filepath.Base(file) + ":" + line + ": " + rest +
			" — a scan runs while the rows hold their connection and again on a retry; fetch children in one query and group in Go"
	}
	return out, scans
}

// positionLess orders "file:line:col\x00…" keys by file, line, column, then
// text.
func positionLess(a, b string) bool {
	pa, ra, _ := strings.Cut(a, "\x00")
	pb, rb, _ := strings.Cut(b, "\x00")
	fa := strings.Split(pa, ":")
	fb := strings.Split(pb, ":")
	if fa[0] != fb[0] {
		return fa[0] < fb[0]
	}
	for i := 1; i < 3 && i < len(fa) && i < len(fb); i++ {
		x, _ := strconv.Atoi(fa[i])
		y, _ := strconv.Atoi(fb[i])
		if x != y {
			return x < y
		}
	}
	return ra < rb
}
