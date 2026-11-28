package pgmo

import (
	"context"
	"fmt"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Index writes
// ============================================================
//
// index_write.go is P4.13.3's indexes: CREATE INDEX (CONCURRENTLY opt-in),
// DROP INDEX (CONCURRENTLY, CASCADE), ALTER INDEX … RENAME TO, COMMENT ON
// INDEX — on a table's or a materialized view's, through the relation's
// methods. An index is a part of its relation, in the relation's schema,
// as Table.Indexes reads it, so it has no handle of its own.
//
// CONCURRENTLY builds or drops without the lock that blocks writes, in
// several transactions of the server's own, and so cannot run in a
// transaction block: inside InTransaction it is refused with ErrUnsupported
// before anything is sent.

// IndexColumn is one key column of CreateIndexRequest: a column by name, or
// an expression.
type IndexColumn struct {
	// Column is the column's name; Expression an expression as SQL
	// ("lower(email)"), written in parentheses. Exactly one is set.
	Column     string
	Expression string

	// Collation is COLLATE; zero for the column's.
	Collation ObjectName

	// OperatorClass is the operator class; zero for the type's default
	// for the method. Schema-qualify one outside pg_catalog.
	OperatorClass ObjectName

	// Descending is DESC; NullsFirst is NULLS FIRST (true) or NULLS LAST
	// (false), nil for the order's default (last ascending, first
	// descending).
	Descending bool
	NullsFirst *bool
}

// CreateIndexRequest describes a new index on a table or materialized view.
type CreateIndexRequest struct {
	// Name is the index's name, created in the relation's schema; "" lets
	// the server pick one (table_column_idx).
	Name string

	// Unique is UNIQUE; NullsNotDistinct its NULLS NOT DISTINCT.
	Unique           bool
	NullsNotDistinct bool

	// Method is USING's access method: btree (""), hash, gist, spgist, gin,
	// brin, or an extension's.
	Method string

	Columns []IndexColumn

	// Include are INCLUDE's non-key columns.
	Include []string

	// Predicate makes a partial index: WHERE's condition as SQL.
	Predicate string

	// Tablespace is TABLESPACE; "" is the database's default.
	Tablespace string

	// Options are the storage parameters (WITH (fillfactor = '90', …)).
	Options []ParameterSetting

	// Concurrently is CONCURRENTLY: built without blocking writes, in
	// transactions of its own — refused inside InTransaction. A failed one
	// leaves an invalid index (Index.Valid false) to drop.
	Concurrently bool

	// Only is ON ONLY: on a partitioned table, the index of the parent
	// alone, left invalid until each partition's is attached.
	Only bool
}

// createIndexStatement is req's CREATE INDEX on the relation rel (its
// qualified name), or the refusal of one that cannot be written. No prefix:
// the caller names the operation.
func createIndexStatement(rel string, req CreateIndexRequest) (string, error) {
	if len(req.Columns) == 0 {
		return "", invalidf("an index with no columns")
	}
	if req.NullsNotDistinct && !req.Unique {
		return "", invalidf("NULLS NOT DISTINCT without UNIQUE")
	}
	if err := requireNames("INCLUDE", req.Include); err != nil {
		return "", err
	}
	cols := make([]string, len(req.Columns))
	for i, c := range req.Columns {
		switch {
		case c.Column != "" && c.Expression != "":
			return "", invalidf("index column %d: both a column and an expression", i+1)
		case c.Column != "":
			cols[i] = pgsql.QuoteIdent(c.Column)
		case c.Expression != "":
			cols[i] = "(" + c.Expression + ")"
		default:
			return "", invalidf("index column %d: neither a column nor an expression", i+1)
		}
		if c.Collation.Name != "" {
			cols[i] += " COLLATE " + c.Collation.QualifiedName()
		}
		if c.OperatorClass.Name != "" {
			cols[i] += " " + c.OperatorClass.QualifiedName()
		}
		if c.Descending {
			cols[i] += " DESC"
		}
		if c.NullsFirst != nil {
			if *c.NullsFirst {
				cols[i] += " NULLS FIRST"
			} else {
				cols[i] += " NULLS LAST"
			}
		}
	}
	opts, err := storageParameters(req.Options)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("CREATE ")
	if req.Unique {
		b.WriteString("UNIQUE ")
	}
	b.WriteString("INDEX ")
	if req.Concurrently {
		b.WriteString("CONCURRENTLY ")
	}
	if req.Name != "" {
		b.WriteString(pgsql.QuoteIdent(req.Name) + " ")
	}
	b.WriteString("ON ")
	if req.Only {
		b.WriteString("ONLY ")
	}
	b.WriteString(rel)
	if req.Method != "" {
		b.WriteString(" USING " + pgsql.QuoteIdent(req.Method))
	}
	b.WriteString(" (" + strings.Join(cols, ", ") + ")")
	if len(req.Include) > 0 {
		b.WriteString(" INCLUDE (" + identList(req.Include) + ")")
	}
	if req.NullsNotDistinct {
		b.WriteString(" NULLS NOT DISTINCT")
	}
	if opts != "" {
		b.WriteString(" WITH " + opts)
	}
	if req.Tablespace != "" {
		b.WriteString(" TABLESPACE " + pgsql.QuoteIdent(req.Tablespace))
	}
	if req.Predicate != "" {
		b.WriteString(" WHERE " + req.Predicate)
	}
	return b.String(), nil
}

// DropIndexOptions are the choices of DropIndex.
type DropIndexOptions struct {
	// Concurrently is CONCURRENTLY: dropped without blocking the
	// relation's reads and writes — refused inside InTransaction, and with
	// Cascade (the server takes only one).
	Concurrently bool
	// Cascade is CASCADE: the objects depending on the index go with it.
	Cascade bool
}

// relationIndexes are the index writes of the relation schema.name in db —
// a table's or a materialized view's; keyword names the relation's kind for
// errors.
type relationIndexes struct {
	db                    *Database
	keyword, schema, name string
}

// op is the relation's operation as an error names it, after refusing a
// relation missing its schema or name.
func (r relationIndexes) op(verb string) (string, error) {
	kind := keywordLabel(r.keyword)
	if err := requireQualified(verb+" "+kind, r.schema, r.name); err != nil {
		return "", err
	}
	return verb + " " + kind + " " + pgsql.QualifiedName(r.schema, r.name), nil
}

func (r relationIndexes) create(ctx context.Context, req CreateIndexRequest) error {
	op, err := r.op("create index on")
	if err != nil {
		return err
	}
	if req.Concurrently {
		if err := refuseInTx(ctx, r.db.server, op+" concurrently"); err != nil {
			return err
		}
	}
	stmt, err := createIndexStatement(pgsql.QualifiedName(r.schema, r.name), req)
	if err != nil {
		return fmt.Errorf("pgmo: %s: %w", op, err)
	}
	if err := r.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return nil
}

// index is the index name's qualified name — in the relation's schema — and
// the operation verb on it as an error names it.
func (r relationIndexes) index(verb, name string) (qualified, op string, err error) {
	if name == "" {
		return "", "", invalidf("pgmo: %s index: no name", verb)
	}
	rop, err := r.op(verb + " index " + pgsql.QuoteIdent(name) + " of")
	if err != nil {
		return "", "", err
	}
	return pgsql.QualifiedName(r.schema, name), rop, nil
}

func (r relationIndexes) drop(ctx context.Context, name string, opts DropIndexOptions) error {
	idx, op, err := r.index("drop", name)
	if err != nil {
		return err
	}
	stmt := "DROP INDEX "
	if opts.Concurrently {
		if opts.Cascade {
			return invalidf("pgmo: %s: CONCURRENTLY and CASCADE together; the server takes only one", op)
		}
		if err := refuseInTx(ctx, r.db.server, op+" concurrently"); err != nil {
			return err
		}
		stmt += "CONCURRENTLY "
	}
	stmt += idx + DropOptions{Cascade: opts.Cascade}.clause()
	if err := r.db.exec(ctx, stmt); err != nil {
		return fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return nil
}

func (r relationIndexes) rename(ctx context.Context, name, newName string) error {
	idx, op, err := r.index("rename", name)
	if err != nil {
		return err
	}
	if newName == "" {
		return invalidf("pgmo: %s: no new name", op)
	}
	if err := r.db.exec(ctx, "ALTER INDEX "+idx+" RENAME TO "+pgsql.QuoteIdent(newName)); err != nil {
		return fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return nil
}

func (r relationIndexes) comment(ctx context.Context, name, text string) error {
	idx, op, err := r.index("comment on", name)
	if err != nil {
		return err
	}
	if err := r.db.exec(ctx, commentStatement("INDEX "+idx, text)); err != nil {
		return fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return nil
}

func (t *Table) indexes() relationIndexes {
	return relationIndexes{db: t.db, keyword: "TABLE", schema: t.Schema, name: t.Name}
}

func (m *MaterializedView) indexes() relationIndexes {
	return relationIndexes{db: m.db, keyword: "MATERIALIZED VIEW", schema: m.Schema, name: m.Name}
}

// CreateIndex creates the index req describes on the table (CREATE INDEX).
// With Concurrently it is refused inside InTransaction. Nothing is read
// back: read Indexes.
func (t *Table) CreateIndex(ctx context.Context, req CreateIndexRequest) error {
	return t.indexes().create(ctx, req)
}

// DropIndex drops the table's index name (DROP INDEX, in the table's
// schema). An index backing a constraint is dropped with the constraint
// (DropConstraint), and the server refuses it here (2BP01).
func (t *Table) DropIndex(ctx context.Context, name string, opts DropIndexOptions) error {
	return t.indexes().drop(ctx, name, opts)
}

// RenameIndex renames the table's index name (ALTER INDEX … RENAME TO).
func (t *Table) RenameIndex(ctx context.Context, name, newName string) error {
	return t.indexes().rename(ctx, name, newName)
}

// SetIndexComment sets the comment of the table's index name (COMMENT ON
// INDEX), "" removing it.
func (t *Table) SetIndexComment(ctx context.Context, name, text string) error {
	return t.indexes().comment(ctx, name, text)
}

// CreateIndex creates the index req describes on the materialized view, as
// Table.CreateIndex does. A unique index on plain columns, no predicate, is
// what RefreshMaterializedViewOptions.Concurrently needs.
func (m *MaterializedView) CreateIndex(ctx context.Context, req CreateIndexRequest) error {
	return m.indexes().create(ctx, req)
}

// DropIndex drops the materialized view's index name, as Table.DropIndex
// does.
func (m *MaterializedView) DropIndex(ctx context.Context, name string, opts DropIndexOptions) error {
	return m.indexes().drop(ctx, name, opts)
}

// RenameIndex renames the materialized view's index name.
func (m *MaterializedView) RenameIndex(ctx context.Context, name, newName string) error {
	return m.indexes().rename(ctx, name, newName)
}

// SetIndexComment sets the comment of the materialized view's index name,
// "" removing it.
func (m *MaterializedView) SetIndexComment(ctx context.Context, name, text string) error {
	return m.indexes().comment(ctx, name, text)
}
