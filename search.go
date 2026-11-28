package pgmo

import (
	"context"
	"fmt"
)

// ============================================================
// Object search
// ============================================================
//
// search.go is gosmo's Database.Search and SearchDefinitions: find the
// objects of one database by name, or by the text of their definitions,
// across families in one statement — SSMS's Object Explorer Details search
// box. A name is matched as every listing's ObjectFilter matches it (typed
// text folded with pgsql.FoldIdent, compared exactly by strpos), so a
// search finds what the same text names in a statement, and gosmo's LOWER
// on both sides does not carry over (names.go).
//
// The families searched are the schema-scoped ones with a database listing
// of their own: tables (partitioned too), views, materialized views,
// sequences, foreign tables, functions, procedures, aggregates, types and
// domains, and triggers (named by the table's schema). A table's
// constraints, indexes and policies are its parts and are not searched;
// neither are the families without a schema of their own.

// SearchResult is one object a search found.
type SearchResult struct {
	// Origin: IsSystem for an object in a system schema; Extension for
	// one an extension owns — a trigger, its table's extension.
	Origin

	// Catalog and OID address the object (Address()).
	Catalog string
	OID     OID

	// Type is the object's kind: table, view, materialized view,
	// sequence, foreign table, function, procedure, aggregate, type,
	// domain or trigger — pg_identify_object's names, but for a domain,
	// which it calls a type.
	Type string

	// Schema and Name are the stored names; for a trigger, Schema is its
	// table's.
	Schema string
	Name   string

	// Table is a trigger's table, in Schema; empty for every other kind.
	Table string

	// Identity is pg_identify_object's identity, as Dependency.Identity:
	// schema-qualified and quoted whatever the session's search_path, a
	// routine with its argument types — what tells overloads apart.
	Identity string
}

// Address is the object's address, to read its dependencies.
func (r SearchResult) Address() ObjectAddress { return ObjectAddress{Catalog: r.Catalog, OID: r.OID} }

// searchObjects is every searched object as one relation o(cls, catalog,
// oid, type, schema, name, tbl, member): member is the OID of the object
// whose extension membership is the result's (a trigger's table). Types
// leave out, as Database.Types does, the implicit array types and the row
// types of relations other than composite types.
const searchObjects = `(
	SELECT 'pg_catalog.pg_class'::pg_catalog.regclass::pg_catalog.oid, 'pg_class', c.oid,
		CASE c.relkind WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' WHEN 'S' THEN 'sequence'
			WHEN 'f' THEN 'foreign table' ELSE 'table' END,
		n.nspname::pg_catalog.text, c.relname::pg_catalog.text, '', c.oid
	FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
	UNION ALL
	SELECT 'pg_catalog.pg_proc'::pg_catalog.regclass::pg_catalog.oid, 'pg_proc', p.oid,
		CASE p.prokind WHEN 'p' THEN 'procedure' WHEN 'a' THEN 'aggregate' ELSE 'function' END,
		n.nspname::pg_catalog.text, p.proname::pg_catalog.text, '', p.oid
	FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
	UNION ALL
	SELECT 'pg_catalog.pg_type'::pg_catalog.regclass::pg_catalog.oid, 'pg_type', t.oid,
		CASE t.typtype WHEN 'd' THEN 'domain' ELSE 'type' END,
		n.nspname::pg_catalog.text, t.typname::pg_catalog.text, '', t.oid
	FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
	LEFT JOIN pg_catalog.pg_class r ON r.oid = t.typrelid
	LEFT JOIN pg_catalog.pg_type te ON te.oid = t.typelem
	WHERE (t.typrelid = 0 OR r.relkind = 'c') AND (te.oid IS NULL OR te.typarray <> t.oid)
	UNION ALL
	SELECT 'pg_catalog.pg_trigger'::pg_catalog.regclass::pg_catalog.oid, 'pg_trigger', tg.oid, 'trigger',
		n.nspname::pg_catalog.text, tg.tgname::pg_catalog.text, c.relname::pg_catalog.text, c.oid
	FROM pg_catalog.pg_trigger tg JOIN pg_catalog.pg_class c ON c.oid = tg.tgrelid
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	WHERE NOT tg.tgisinternal
) AS o(cls, catalog, oid, type, schema, name, tbl, member)`

// searchSelect reads a SearchResult from o, joined to the extension owning
// o.member (a trigger's table, the object itself otherwise) — a join, not
// a subquery per row, since a search can span every catalog row. joins are
// the caller's own, and its WHERE terms follow.
func searchSelect(joins string) string {
	return `SELECT o.catalog, o.oid, o.type, o.schema, o.name, o.tbl,
	COALESCE(i.identity, ''), ` + systemSchema("o.schema") + `, COALESCE(ext.extname::pg_catalog.text, '')
FROM ` + searchObjects + `
LEFT JOIN pg_catalog.pg_depend d ON d.classid = CASE o.catalog WHEN 'pg_trigger' THEN 'pg_catalog.pg_class'::pg_catalog.regclass::pg_catalog.oid ELSE o.cls END
	AND d.objid = o.member AND d.objsubid = 0
	AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.deptype = 'e'
LEFT JOIN pg_catalog.pg_extension ext ON ext.oid = d.refobjid` + joins + `
CROSS JOIN LATERAL pg_catalog.pg_identify_object(o.cls, o.oid, 0) AS i
WHERE true`
}

var (
	searchQuery = searchSelect("")

	// definitionSearchQuery joins each routine's row and language for
	// searchDefinition.
	definitionSearchQuery = searchSelect(`
LEFT JOIN pg_catalog.pg_proc p ON o.catalog = 'pg_proc' AND p.oid = o.oid
LEFT JOIN pg_catalog.pg_language l ON l.oid = p.prolang`) + `
	AND o.type IN ('view', 'materialized view', 'function', 'procedure')
	AND pg_catalog.strpos(` + searchDefinition + `, $1::pg_catalog.text) > 0`
)

// searchFilterColumns are the search's own: name and schema are o's, a
// trigger's schema its table's.
var searchFilterColumns = filterColumns{
	name:      "o.name",
	schema:    "o.schema",
	system:    systemSchema("o.schema"),
	extension: "ext.oid IS NOT NULL",
}

// searchOrder is both searches' order: by kind, then schema, name and
// table, then identity to keep overloads in a stable order — byte by byte
// (COLLATE "C"), as a name column sorts, whatever the database's
// collation: the text casts and pg_identify_object take the default one.
const searchOrder = `
ORDER BY o.type COLLATE pg_catalog."C", o.schema COLLATE pg_catalog."C", o.name COLLATE pg_catalog."C", o.tbl COLLATE pg_catalog."C", i.identity COLLATE pg_catalog."C"`

// scanSearchResult builds a SearchResult from one searchSelect row.
func scanSearchResult(scan func(...any) error) (SearchResult, error) {
	var r SearchResult
	dest := []any{&r.Catalog, &r.OID, &r.Type, &r.Schema, &r.Name, &r.Table, &r.Identity}
	return r, scan(append(dest, scanOrigin(&r.Origin)...)...)
}

// Search finds the objects of the database whose name, and schema, f's
// criteria match — gosmo's Search, which matched a name containing a
// pattern, is Search(ctx, ObjectFilter{Name: []TextCriterion{{TextContains,
// pattern}}}). f is applied as every listing applies it (object_filter.go):
// typed text folded with pgsql.FoldIdent and compared exactly, System and
// ExtensionMember as for each family; a zero f finds every object
// searched, pg_catalog's thousands of functions included — pass
// System: new(false) to leave the system schemas out. The families searched
// are in the search.go header; results are ordered by Type, Schema, Name,
// Table.
func (d *Database) Search(ctx context.Context, f ObjectFilter) ([]SearchResult, error) {
	where, args, err := f.clause(searchFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("search objects of database %q", d.Name), scanSearchResult,
		searchQuery+where+searchOrder, args...)
}

// searchDefinition is the definition text of o: a view's or materialized
// view's query (pg_get_viewdef), a routine's body — the SQL-standard
// body's text (pg_get_function_sqlbody) or the source as written (prosrc)
// for any language but C and internal, whose prosrc is a symbol — and NULL
// for every other kind. p and l are the routine's row and language,
// definitionSearchQuery's joins.
const searchDefinition = `CASE
	WHEN o.type IN ('view', 'materialized view') THEN pg_catalog.pg_get_viewdef(o.oid)
	WHEN p.prosqlbody IS NOT NULL THEN pg_catalog.pg_get_function_sqlbody(p.oid)
	WHEN l.lanname NOT IN ('c', 'internal') THEN p.prosrc
	END`

// SearchDefinitions finds the views, materialized views, functions and
// procedures of the database whose definition contains text — gosmo's
// SearchDefinitions over sql_modules: a view's query as
// pg_get_viewdef renders it, a routine's body as written (a SQL-standard
// BEGIN ATOMIC or RETURN body as the server renders it). C and internal
// functions have no body to search, aggregates and the other kinds none
// at all. f narrows which objects are searched, as for Search; a zero f
// searches the system schemas too.
//
// text is not a name and is not folded: it is matched as written, case
// and all, anywhere in the definition (strpos, so % and _ are literal),
// and an empty text is refused (ErrInvalidRequest). A rendered definition
// names objects as the reading session's search_path would, as every
// rendered definition does (Function.Body): a table on the path reads
// unqualified.
func (d *Database) SearchDefinitions(ctx context.Context, text string, f ObjectFilter) ([]SearchResult, error) {
	what := fmt.Sprintf("search definitions of database %q", d.Name)
	if text == "" {
		return nil, invalidf("pgmo: %s: no text to search for", what)
	}
	where, args, err := f.clause(searchFilterColumns, []any{text})
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, what, scanSearchResult, definitionSearchQuery+where+searchOrder, args...)
}
