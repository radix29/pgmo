package pgmo

import "context"

// ============================================================
// Indexes
// ============================================================
//
// index.go reads a table's indexes (PLAN.md P4.4) from pg_index, with the
// index's pg_class row, access method, the constraint it backs, tablespace
// and comment, in one statement. Where SQL Server describes an index by
// flags (clustered, filtered, columnstore), PostgreSQL's is a
// CREATE INDEX statement the server renders (pg_get_indexdef) plus what a
// list wants at a glance: its method, uniqueness, columns, predicate and
// whether it is usable.

// Index is one index of a table.
type Index struct {
	OID  OID
	Name string

	// Definition is the index's CREATE INDEX statement as pg_get_indexdef
	// renders it — the table always schema-qualified, expressions as the
	// session's search_path sees them. A partitioned table's own index reads
	// "ON ONLY".
	Definition string

	// Method is the access method's name: btree, hash, gist, gin, spgist,
	// brin, or an extension's.
	Method string

	Unique    bool
	Primary   bool
	Exclusion bool

	// NullsNotDistinct is UNIQUE NULLS NOT DISTINCT: NULLs count as equal.
	NullsNotDistinct bool

	// Clustered marks the index CLUSTER last used (indisclustered).
	Clustered bool

	// Valid is false for an index the server must not use for queries: a
	// CREATE INDEX CONCURRENTLY that failed (drop or REINDEX it), or a
	// partitioned table's index not yet attached on every partition.
	Valid bool

	// KeyColumns are the key's columns or expressions, in order, as
	// pg_get_indexdef renders each ("name", "lower((b)::text)"), without
	// ordering or operator class — those are in Definition.
	KeyColumns []string

	// IncludedColumns are the INCLUDE columns, nil for none.
	IncludedColumns []string

	// Predicate is a partial index's WHERE condition, empty for none.
	Predicate string

	// Constraint is the primary key, unique or exclusion constraint the
	// index enforces, empty for a plain index.
	Constraint string

	// Tablespace is the index's own tablespace, empty for the database's
	// default.
	Tablespace string

	// Options are the index's storage parameters (fillfactor…), nil for
	// none.
	Options []ParameterSetting

	// Comment is COMMENT ON INDEX's text, empty for none.
	Comment string

	// Parent is the partitioned table's index this one is attached to — a
	// partition's own index for its parent's (pg_inherits), created and
	// dropped with it; zero for none.
	Parent ObjectName
}

// indexColumns is pg_get_indexdef's rendering of the index i's columns
// numbered from to to, in order.
func indexColumns(from, to string) string {
	return `ARRAY(SELECT pg_catalog.pg_get_indexdef(i.indexrelid, k.n, true)
		FROM pg_catalog.generate_series(` + from + `, ` + to + `) AS k(n) ORDER BY k.n)`
}

// indexesQuery reads the indexes of the table $1 by name.
var indexesQuery = `SELECT i.indexrelid, ic.relname::pg_catalog.text, pg_catalog.pg_get_indexdef(i.indexrelid),
	am.amname::pg_catalog.text,
	i.indisunique, i.indisprimary, i.indisexclusion, i.indnullsnotdistinct, i.indisclustered, i.indisvalid,
	` + indexColumns("1", "i.indnkeyatts") + `,
	` + indexColumns("i.indnkeyatts + 1", "i.indnatts") + `,
	COALESCE(pg_catalog.pg_get_expr(i.indpred, i.indrelid, true), ''),
	COALESCE(co.conname::pg_catalog.text, ''),
	COALESCE(ts.spcname::pg_catalog.text, ''),
	ic.reloptions,
	COALESCE(pg_catalog.obj_description(i.indexrelid, 'pg_class'), ''),
	COALESCE(pn.nspname::pg_catalog.text, ''), COALESCE(pc.relname::pg_catalog.text, '')
FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
JOIN pg_catalog.pg_am am ON am.oid = ic.relam
LEFT JOIN pg_catalog.pg_constraint co ON co.conindid = i.indexrelid AND co.conrelid = i.indrelid
	AND co.contype IN ('p', 'u', 'x')
LEFT JOIN pg_catalog.pg_tablespace ts ON ts.oid = ic.reltablespace
LEFT JOIN pg_catalog.pg_inherits ih ON ih.inhrelid = i.indexrelid
LEFT JOIN pg_catalog.pg_class pc ON pc.oid = ih.inhparent
LEFT JOIN pg_catalog.pg_namespace pn ON pn.oid = pc.relnamespace
WHERE i.indrelid = $1
ORDER BY ic.relname`

// scanIndex builds an *Index from one indexesQuery row.
func scanIndex(scan func(...any) error) (*Index, error) {
	x := &Index{}
	var options []string
	if err := scan(&x.OID, &x.Name, &x.Definition, &x.Method,
		&x.Unique, &x.Primary, &x.Exclusion, &x.NullsNotDistinct, &x.Clustered, &x.Valid,
		&x.KeyColumns, &x.IncludedColumns, &x.Predicate, &x.Constraint, &x.Tablespace,
		&options, &x.Comment, &x.Parent.Schema, &x.Parent.Name); err != nil {
		return nil, err
	}
	if len(x.IncludedColumns) == 0 {
		x.IncludedColumns = nil
	}
	x.Options = parseSettings(options)
	return x, nil
}

// Indexes reads the table's indexes by name, those backing its constraints
// included. A TableRef is refused (ErrHandleNotLoaded).
func (t *Table) Indexes(ctx context.Context) ([]*Index, error) {
	if err := t.requireLoaded("list indexes of table"); err != nil {
		return nil, err
	}
	q, err := t.on(ctx)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, "list indexes of table "+t.QualifiedName(), scanIndex, indexesQuery, t.OID)
}
