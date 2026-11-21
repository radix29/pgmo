package pgmo

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Every name a read resolves goes through pg_catalog
// ============================================================

// TestCatalogReadsAreSchemaQualified holds pgmo's SQL to schema-qualified
// names (P2.2.4): the session's search_path is the caller's, and a function,
// relation or type named bare is whatever that path finds first. A schema
// listed before pg_catalog shadows any of them, and a temporary table
// shadows a catalog relation even under an empty search_path — checked on
// pg15 and pg18 — so pinning search_path at run time would not be enough;
// naming pg_catalog in the text is. Operators are not checked: pg_catalog
// comes first for them unless the path lists it later (OPEN-THREADS.md §
// Name resolution).
//
// It reads every string literal in the package's non-test source that looks
// like SQL, so it covers reads no test reaches. TestUnqualifiedNameScan
// shows what the scan finds.
func TestCatalogReadsAreSchemaQualified(t *testing.T) {
	for _, lit := range sqlLiterals(t) {
		for _, p := range unqualifiedNames(lit.sql) {
			t.Errorf("%s: %s", lit.pos, p)
		}
	}
}

// sqlLiteral is one Go string literal of the package that looks like SQL.
type sqlLiteral struct {
	pos token.Position
	sql string
}

// sqlLiterals returns, in file order, every string literal in the package's
// non-test source that looks like SQL. It fails the test when there is none:
// looksLikeSQL would be wrong, and a guard reading nothing proves nothing.
func sqlLiterals(t *testing.T) []sqlLiteral {
	t.Helper()
	fset, files := parsePackage(t)
	var out []sqlLiteral
	for _, name := range slices.Sorted(maps.Keys(files)) {
		ast.Inspect(files[name], func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && looksLikeSQL(s) {
				out = append(out, sqlLiteral{fset.Position(lit.Pos()), s})
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Fatal("no SQL literal found; looksLikeSQL is wrong and this test proves nothing")
	}
	return out
}

// The scan itself, on SQL holding each shape: what it must flag and what it
// must leave alone.
func TestUnqualifiedNameScan(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
	}{
		// Functions.
		{`SELECT current_setting('server_version_num')`, []string{
			"calls current_setting through search_path — write pg_catalog.current_setting(…)"}},
		{`SELECT "count"(*), count(*) FROM pg_catalog.pg_class`, []string{
			"calls count through search_path — write pg_catalog.count(…)",
			"calls count through search_path — write pg_catalog.count(…)"}},
		{`SELECT x FROM pg_catalog.unnest(ARRAY[1]) AS u(x), generate_series(1, 2) g`, []string{
			"calls generate_series through search_path — write pg_catalog.generate_series(…)"}},
		// Relations: after FROM or JOIN, and any pg_ name in a FROM list.
		{`SELECT 1 FROM pg_class c`, []string{
			"reads pg_class through search_path — qualify it with its schema (pg_catalog.pg_class)"}},
		{`SELECT 1 FROM pg_catalog.pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace`, []string{
			"reads pg_namespace through search_path — qualify it with its schema (pg_catalog.pg_namespace)"}},
		{`SELECT 1 FROM pg_catalog.pg_class c, pg_namespace n`, []string{
			"names pg_namespace through search_path — write pg_catalog.pg_namespace"}},
		{`SELECT "pg_class".relname FROM "pg_class"`, []string{
			"reads pg_class through search_path — qualify it with its schema (pg_catalog.pg_class)"}},
		{`SELECT 1 FROM information_schema.tables t JOIN columns c USING (table_name)`, []string{
			"reads columns through search_path — qualify it with its schema (pg_catalog.columns)"}},
		// Types: casts, CAST(… AS …) and typed literals.
		{`SELECT x::text, y::"int4", CAST(z AS regclass), date '2026-11-21'`, []string{
			"casts to text through search_path — write pg_catalog.text",
			"casts to int4 through search_path — write pg_catalog.int4",
			"casts to regclass through search_path — write pg_catalog.regclass",
			"names type date through search_path — write pg_catalog.date '…'"}},
		// Fine: qualified names, keyword types, grammar constructs, CTEs,
		// aliases, and anything inside a literal or a comment.
		{versionQuery, nil},
		{`SELECT x::integer, y::double precision, z::character varying(10), w::numeric(10, 2),
		         CAST(v AS bigint), x::pg_catalog.text, $1::pg_catalog.regclass, interval '1 day',
		         timestamp with time zone '2026-11-21', NULL::timestamp with time zone`, nil},
		{`SELECT COALESCE(a, b), NULLIF(a, b), GREATEST(a, b), ARRAY(SELECT 1), ROW(1, 2),
		         EXISTS (SELECT), a IN (1, 2), a = ANY ($1), 'a' LIKE 'b', current_user,
		         pg_catalog.count(*) FILTER (WHERE a) OVER (PARTITION BY b ORDER BY c)
		  FROM pg_catalog.pg_class`, nil},
		{`SELECT EXTRACT(epoch FROM t), substring(s FROM 2), trim(both FROM s),
		         a IS DISTINCT FROM b
		  FROM pg_catalog.pg_stat_activity`, nil},
		{`WITH t(a) AS MATERIALIZED (SELECT 1), u AS (SELECT 2)
		  SELECT a FROM t JOIN u ON true JOIN LATERAL pg_catalog.unnest(ARRAY[1]) AS v(x) ON true`, nil},
		{`SELECT 'pg_class', E'current_setting(' -- FROM pg_class
		  /* now() /* nested */ ::text */ FROM pg_catalog.pg_class`, nil},
		{`SELECT 1 FROM pg_catalog.pg_class c WHERE c.relname = $1 LIMIT $2`, nil},
	}
	for _, c := range cases {
		if got := unqualifiedNames(c.sql); !slices.Equal(got, c.want) {
			t.Errorf("unqualifiedNames(%q)\n got  %q\n want %q", c.sql, got, c.want)
		}
	}
}

// The literals the guard reads, and the ones it must not mistake for SQL.
func TestLooksLikeSQL(t *testing.T) {
	for _, s := range []string{versionQuery, "  select 1", "\nWITH t AS (SELECT 1) TABLE t", "NULL::numeric", " FROM pg_catalog.pg_class c"} {
		if !looksLikeSQL(s) {
			t.Errorf("looksLikeSQL(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"pgmo: connection options: select a host", "sslmode", "", "application_name", "from %s to %s"} {
		if looksLikeSQL(s) {
			t.Errorf("looksLikeSQL(%q) = true, want false", s)
		}
	}
}

// sqlStatement matches text that starts a statement; sqlFragment a piece of
// one (a clause or a cast). Lower-case clause words alone are prose.
var (
	sqlStatement = regexp.MustCompile(`(?i)^\s*(SELECT|WITH|INSERT|UPDATE|DELETE|MERGE|SHOW|CALL|CREATE|ALTER|DROP|COMMENT|GRANT|REVOKE|TRUNCATE|VACUUM|ANALYZE|REINDEX|CLUSTER)\s`)
	sqlFragment  = regexp.MustCompile(`\b(SELECT|FROM|JOIN|WHERE|RETURNING)\b|::`)
)

// looksLikeSQL reports whether a Go string literal is SQL, or a piece of it.
func looksLikeSQL(s string) bool {
	return sqlStatement.MatchString(s) || sqlFragment.MatchString(s)
}

// sqlTok is one token of the code in a SQL text.
type sqlTok struct {
	text   string // an identifier's name (a bare one folded to lower case), else the token
	ident  bool
	quoted bool // a "…" identifier: never a keyword
}

// is reports whether t is the bare word or the punctuation w.
func (t sqlTok) is(w string) bool { return !t.quoted && t.text == w }

// literal is the token every string literal becomes; param every $n.
const (
	literalTok = "'…'"
	paramTok   = "$n"
)

// codeToken splits code: a fmt verb (pgmo builds some SQL with Sprintf), an
// identifier, a number, ::, or any other single character.
var codeToken = regexp.MustCompile(`%[-+#0-9.*]*[a-zA-Z%]|[\pL_][\pL\pN_$]*|[0-9]+(\.[0-9]+)?([eE][-+]?[0-9]+)?|::|\S`)

// sqlTokens tokenizes sql's code through pgsql.Spans, so nothing inside a
// literal or a comment is read as a name.
func sqlTokens(sql string) []sqlTok {
	var out []sqlTok
	for sp := range pgsql.Spans(sql) {
		switch {
		case sp.IsComment():
		case sp.IsIdent():
			out = append(out, sqlTok{text: sp.Unescape(sql), ident: true, quoted: true})
		case sp.Kind == pgsql.SpanParam:
			out = append(out, sqlTok{text: paramTok})
		case sp.IsLiteral():
			out = append(out, sqlTok{text: literalTok})
		default:
			for _, w := range codeToken.FindAllString(sql[sp.Start:sp.End], -1) {
				c := w[0]
				ident := c == '_' || c >= 0x80 || ('a' <= c|0x20 && c|0x20 <= 'z')
				if ident {
					w = strings.ToLower(w)
				}
				out = append(out, sqlTok{text: w, ident: ident})
			}
		}
	}
	return out
}

// keywordTypes are the type names the grammar maps to pg_catalog itself
// (SystemTypeName in gram.y), so search_path never sees them: integer is
// always pg_catalog.int4, while int4, bool, text or json are looked up. Checked
// on pg15 and pg18 against a schema of same-named domains ahead of
// pg_catalog.
var keywordTypes = setOf("bigint", "bit", "boolean", "char", "character", "dec", "decimal",
	"double", "float", "int", "integer", "interval", "national", "nchar", "numeric", "real",
	"smallint", "time", "timestamp", "varchar")

// grammarCalls are the words a "(" may follow that are not a function call:
// grammar constructs, clause words, and the keyword types (a type modifier).
var grammarCalls = union(keywordTypes, setOf("all", "and", "any", "array", "as", "between", "by",
	"cast", "check", "coalesce", "default", "distinct", "else", "exists", "extract", "filter",
	"from", "greatest", "group", "groups", "having", "ilike", "in", "include", "into", "is",
	"join", "key", "lateral", "least", "like", "materialized", "normalize", "not", "nullif", "on",
	"only", "or", "over", "overlay", "position", "range", "references", "returning", "row", "rows",
	"select", "setof", "some", "substring", "table", "then", "trim", "unique", "using", "values",
	"varying", "when", "where", "with"))

// beforeLiteral are the words a literal may follow that do not make it a
// typed literal (type 'value'), besides the keyword types.
var beforeLiteral = union(keywordTypes, setOf("all", "and", "any", "as", "between", "by", "case",
	"collate", "comment", "default", "distinct", "else", "escape", "from", "having", "ilike", "in",
	"is", "like", "limit", "not", "offset", "on", "or", "returning", "select", "set", "similar",
	"some", "then", "to", "uescape", "using", "values", "when", "where", "with", "zone"))

// relationWords may follow FROM or JOIN without being a relation.
var relationWords = setOf("lateral", "only", "rows")

// notFromAList are the constructs whose FROM is an argument, not a FROM
// list: EXTRACT(field FROM t), substring(s FROM n) and the like.
var notFromAList = setOf("extract", "overlay", "position", "substring", "trim", "normalize")

// schemaNames are pg_ words that name a schema, not something in one.
var schemaNames = setOf("pg_catalog", "pg_temp", "pg_toast")

// unqualifiedNames lists, in text order, every name sql resolves through
// search_path: a function called bare, a relation read bare after FROM or
// JOIN, any other bare pg_ name, a cast to a type that is not a keyword
// type, and a typed literal of such a type. CTE names and aliases are
// exempt.
func unqualifiedNames(sql string) []string {
	toks := sqlTokens(sql)
	at := func(i int) sqlTok {
		if i < 0 || i >= len(toks) {
			return sqlTok{}
		}
		return toks[i]
	}
	declared := declaredNames(toks)
	var out []string
	var openers []string // per open "(", the bare word before it
	for i, tk := range toks {
		switch {
		case tk.is("("):
			opener := ""
			if p := at(i - 1); p.ident && !p.quoted {
				opener = p.text
			}
			openers = append(openers, opener)
		case tk.is(")") && len(openers) > 0:
			openers = openers[:len(openers)-1]
		}
		if !tk.ident {
			continue
		}
		prev, next := at(i-1), at(i+1)
		if prev.is(".") || next.is(".") {
			continue // part of a qualified name
		}
		inner := ""
		if len(openers) > 0 {
			inner = openers[len(openers)-1]
		}
		bare := !tk.quoted
		name := pgsql.QuoteIdent(tk.text)
		switch {
		case prev.is("::") || (prev.is("as") && inner == "cast"):
			if !bare || !keywordTypes[tk.text] {
				out = append(out, fmt.Sprintf("casts to %s through search_path — write pg_catalog.%s", name, name))
			}
		case next.is("("):
			if !declared[tk.text] && !(bare && grammarCalls[tk.text]) && !prev.is("as") {
				out = append(out, fmt.Sprintf("calls %s through search_path — write pg_catalog.%s(…)", name, name))
			}
		case next.text == literalTok:
			if !bare || !beforeLiteral[tk.text] {
				out = append(out, fmt.Sprintf("names type %s through search_path — write pg_catalog.%s '…'", name, name))
			}
		case (prev.is("from") && !at(i-2).is("distinct") && !notFromAList[inner] || prev.is("join")) &&
			!declared[tk.text] && !(bare && relationWords[tk.text]):
			out = append(out, fmt.Sprintf("reads %s through search_path — qualify it with its schema (pg_catalog.%s)", name, name))
		case strings.HasPrefix(tk.text, "pg_") && !schemaNames[tk.text]:
			out = append(out, fmt.Sprintf("names %s through search_path — write pg_catalog.%s", name, name))
		}
	}
	return out
}

// declaredNames are the names sql's WITH (and WINDOW) clauses declare:
// name AS (…), name AS [NOT] MATERIALIZED (…), name(columns) AS (…).
func declaredNames(toks []sqlTok) map[string]bool {
	at := func(i int) sqlTok {
		if i < 0 || i >= len(toks) {
			return sqlTok{}
		}
		return toks[i]
	}
	declared := map[string]bool{}
	for i, tk := range toks {
		if !tk.ident || at(i-1).is(".") {
			continue
		}
		j := i + 1
		if at(j).is("(") { // a column list
			for depth := 0; j < len(toks); j++ {
				if at(j).is("(") {
					depth++
				} else if at(j).is(")") {
					if depth--; depth == 0 {
						break
					}
				}
			}
			j++
		}
		if !at(j).is("as") {
			continue
		}
		j++
		if at(j).is("not") {
			j++
		}
		if at(j).is("materialized") {
			j++
		}
		if at(j).is("(") {
			declared[tk.text] = true
		}
	}
	return declared
}

func setOf(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

func union(a, b map[string]bool) map[string]bool {
	m := maps.Clone(a)
	maps.Copy(m, b)
	return m
}
