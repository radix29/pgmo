package pgmo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/radix29/pgmo/pgsql"
)

// A write never goes through the read helpers (gosmo's K1). query, queryRow
// and queryRowScan retry on a transient connection failure, and a connection
// that breaks after the server ran a write is exactly such a failure — so the
// write runs twice. A write that reads a value back uses execScan (CLAUDE.md
// § Conventions).
//
// This scans the SQL handed to every read-helper call: the argument's string
// literals, following an identifier to its const/var or local assignment,
// with the SQL's literals, quoted identifiers and comments blanked first
// (pgsql.Spans), so has_database_privilege(…, 'CREATE') is a read.
// Identifiers resolve through go/types over this package's own declarations;
// every import is an empty stub, and the type errors that leaves are ignored.
func TestNoWriteGoesThroughARetryingRead(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var sources []string
	for _, name := range files {
		if !strings.HasSuffix(name, "_test.go") {
			sources = append(sources, name)
		}
	}
	calls := checkReadHelperSQL(t, sources, func(pos token.Position, helper, write string) {
		t.Errorf("%s: %s is handed SQL containing %q — a retried read runs a write twice; use execScan for a write that reads a value back",
			pos, helper, write)
	})
	if calls == 0 {
		t.Fatal("no read-helper calls found; the helper names are wrong and this test proves nothing")
	}
}

// TestWriteInSQLScan pins writeInSQL: each write form it must catch, and reads
// that only look like one.
func TestWriteInSQLScan(t *testing.T) {
	for _, sql := range []string{
		"INSERT INTO t VALUES (1)", "insert into t select 1",
		"UPDATE t SET a = 1", `UPDATE "T" SET a = 1`, "DELETE FROM t", "MERGE INTO t USING s ON true WHEN MATCHED THEN DELETE",
		"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d",
		"CREATE TABLE t ()", "ALTER ROLE r NOLOGIN", "DROP VIEW v", "TRUNCATE t", "GRANT SELECT ON t TO r", "REVOKE ALL ON t FROM r",
		"COMMENT ON TABLE t IS 'x'", "SECURITY LABEL ON TABLE t IS 'x'", "CALL p()", "VACUUM t", "ANALYZE t", "CLUSTER t", "REINDEX TABLE t",
		"REFRESH MATERIALIZED VIEW v", "LOCK TABLE t", "COPY t FROM STDIN", "CHECKPOINT", "DO $$BEGIN END$$", "NOTIFY c", "SET search_path = x",
		"SELECT pg_catalog.nextval('s')", "SELECT setval('s', 1)", "SELECT pg_catalog.pg_terminate_backend($1)",
		"SELECT pg_cancel_backend($1)", "SELECT pg_reload_conf()", "SELECT pg_catalog.set_config('a', 'b', false)",
		"SELECT pg_stat_reset()", "SELECT pg_create_logical_replication_slot('s', 'p')", "SELECT pg_drop_replication_slot('s')",
		"SELECT pg_advisory_lock(1)", "SELECT pg_notify('c', 'x')", "SELECT lo_unlink(1)", "SELECT pg_switch_wal()",
	} {
		if writeInSQL(sql) == "" {
			t.Errorf("writeInSQL(%q) found no write", sql)
		}
	}
	for _, sql := range []string{
		"SELECT 1", "SELECT * FROM t FOR UPDATE", "SELECT rolcreatedb, rolcreaterole FROM pg_catalog.pg_roles",
		"SELECT pg_catalog.has_database_privilege($1, 'CREATE')", "SELECT last_analyze, last_vacuum FROM pg_catalog.pg_stat_user_tables",
		`SELECT "update", "delete" FROM t`, "SELECT 1 -- DROP TABLE t", "SELECT /* INSERT INTO t */ 1",
		"SELECT $$DELETE FROM t$$", "SELECT locktype FROM pg_catalog.pg_locks", "SELECT pg_catalog.current_setting('server_version_num')",
		"SELECT pg_catalog.pg_get_functiondef(p.oid) FROM pg_catalog.pg_proc p", "SELECT pg_catalog.pg_advisory_unlock_all IS NULL",
	} {
		if w := writeInSQL(sql); w != "" {
			t.Errorf("writeInSQL(%q) = %q, want no write", sql, w)
		}
	}
}

// TestReadHelperGuardSeesEachCallForm is the guard's self-test on a planted
// file: each read helper (scanRows and readByName among them), called with a
// write as a literal, through a const, through a local, and with explicit
// type arguments, is caught; execScan,
// the one helper for a write that reads back, is not.
func TestReadHelperGuardSeesEachCallForm(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "planted.go")
	writeFile(t, src, `package pgmo

const ins = "INSERT INTO t DEFAULT VALUES RETURNING id"

func planted(ctx context.Context, q Querier) {
	query(ctx, q, scanID, "DELETE FROM t RETURNING id")
	query[int64](ctx, q, scanID, ins)
	upd := "UPDATE t SET a = 1 RETURNING id"
	queryRow(ctx, q, scan, upd)
	queryRowScan(ctx, q, "SELECT pg_catalog.nextval('s')", nil, &id)
	execScan(ctx, q, "INSERT INTO t DEFAULT VALUES RETURNING id", nil, &id)
	queryRowScan(ctx, q, "SELECT pg_catalog.has_database_privilege($1, 'CREATE')", nil, &ok)
	scanRows(ctx, q, "x", scanInts, "DROP TABLE t")
	readByName(ctx, q, s, scanThing, "SELECT pg_catalog.setval('s', 1)", nil, nil, "x")
}
`)
	var got []string
	calls := checkReadHelperSQL(t, []string{src}, func(pos token.Position, helper, write string) {
		got = append(got, strconv.Itoa(pos.Line)+" "+helper+" "+write)
	})
	want := []string{"6 query DELETE", "7 query INSERT", "9 queryRow UPDATE", "10 queryRowScan nextval", "13 scanRows DROP", "14 readByName setval"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("flagged:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if calls != 7 {
		t.Errorf("read-helper calls seen = %d, want 7", calls)
	}
}

// readHelperSQLArg is the SQL argument's index for each read helper.
var readHelperSQLArg = map[string]int{"query": 3, "queryRow": 3, "queryRowScan": 2, "scanRows": 4, "readByName": 4}

// checkReadHelperSQL parses files as package pgmo, calls flag for each write
// handed to a read helper, and returns how many read-helper calls it saw.
func checkReadHelperSQL(t *testing.T, files []string, flag func(pos token.Position, helper, write string)) int {
	t.Helper()
	fset := token.NewFileSet()
	var parsed []*ast.File
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed = append(parsed, f)
	}
	if len(parsed) == 0 {
		t.Fatal("no source files checked; the glob is wrong and this test proves nothing")
	}

	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: stubImporter{}, Error: func(error) {}}
	_, _ = conf.Check("github.com/radix29/pgmo", fset, parsed, info)

	// The value each const, var or := local was declared with.
	values := map[types.Object]ast.Expr{}
	for _, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			switch d := n.(type) {
			case *ast.ValueSpec:
				for i, name := range d.Names {
					if o := info.Defs[name]; o != nil && i < len(d.Values) {
						values[o] = d.Values[i]
					}
				}
			case *ast.AssignStmt:
				if d.Tok != token.DEFINE || len(d.Lhs) != len(d.Rhs) {
					return true
				}
				for i, lhs := range d.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						if o := info.Defs[id]; o != nil {
							values[o] = d.Rhs[i]
						}
					}
				}
			}
			return true
		})
	}

	// literals collects the string literals expr is built from.
	var literals func(expr ast.Node, seen map[types.Object]bool) []string
	literals = func(expr ast.Node, seen map[types.Object]bool) []string {
		var out []string
		ast.Inspect(expr, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					if s, err := strconv.Unquote(n.Value); err == nil {
						out = append(out, s)
					}
				}
			case *ast.Ident:
				o := info.Uses[n]
				if o == nil || seen[o] {
					return true
				}
				seen[o] = true
				if v, ok := values[o]; ok {
					out = append(out, literals(v, seen)...)
				}
			}
			return true
		})
		return out
	}

	calls := 0
	for _, f := range parsed {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fun := call.Fun
			switch x := fun.(type) { // query[T](…)
			case *ast.IndexExpr:
				fun = x.X
			case *ast.IndexListExpr:
				fun = x.X
			}
			id, ok := fun.(*ast.Ident)
			if !ok {
				return true
			}
			i, ok := readHelperSQLArg[id.Name]
			if !ok || i >= len(call.Args) {
				return true
			}
			calls++
			for _, lit := range literals(call.Args[i], map[types.Object]bool{}) {
				if w := writeInSQL(lit); w != "" {
					flag(fset.Position(call.Pos()), id.Name, w)
				}
			}
			return true
		})
	}
	return calls
}

// writeStatement matches, in SQL code with literals, quoted identifiers and
// comments blanked, a statement that changes something: DML (a data-modifying
// CTE included), DDL and the utility commands, or a call of a built-in
// function with an effect beyond its result.
var writeStatement = regexp.MustCompile(`(?i)\b(?:` +
	`INSERT\s+INTO|UPDATE\s+\S+\s+SET|DELETE\s+FROM|MERGE\s+INTO|` +
	`CREATE|ALTER|DROP|TRUNCATE|GRANT|REVOKE|COMMENT\s+ON|SECURITY\s+LABEL|CALL|DO|` +
	`VACUUM|ANALYZE|CLUSTER|REINDEX|REFRESH|LOCK|COPY|CHECKPOINT|NOTIFY|LISTEN|SET|RESET|DISCARD` +
	`)\b|\b(?:nextval|setval|set_config|pg_notify|pg_terminate_backend|pg_cancel_backend|pg_reload_conf|` +
	`pg_rotate_logfile|pg_switch_wal|pg_create_restore_point|pg_promote|pg_log_backend_memory_contexts|` +
	`pg_stat_reset\w*|pg_create_\w*replication_slot|pg_drop_replication_slot|pg_replication_slot_advance|` +
	`pg_replication_origin_\w+|pg_advisory_(?:xact_)?lock\w*|lo_\w+|pg_import_system_collations|` +
	`pg_backup_start|pg_backup_stop|pg_wal_replay_\w+)\s*\(`)

// writeInSQL returns the first write writeStatement finds in sql's code, or "".
// A function name is matched bare or after pg_catalog.; a column or table name
// containing a keyword (rolcreatedb, last_analyze, pg_locks) is not a match,
// as \b does not split a word at '_'.
func writeInSQL(sql string) string {
	var code strings.Builder
	for sp := range pgsql.Spans(sql) {
		switch {
		case sp.IsComment():
			code.WriteByte(' ')
		case sp.IsLiteral():
			code.WriteString("''")
		case sp.IsIdent():
			code.WriteString("x")
		default:
			code.WriteString(sql[sp.Start:sp.End])
		}
	}
	m := writeStatement.FindString(code.String())
	if i := strings.IndexFunc(m, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }); i > 0 {
		m = m[:i]
	}
	return strings.TrimRight(m, "(")
}

// stubImporter hands back an empty package for every import: the check needs
// only the package's own declarations resolved.
type stubImporter struct{}

func (stubImporter) Import(path string) (*types.Package, error) {
	p := types.NewPackage(path, filepath.Base(path))
	p.MarkComplete()
	return p, nil
}
