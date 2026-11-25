package pgmo

import (
	"context"
	"fmt"
)

// ============================================================
// Constraints
// ============================================================
//
// table_constraint.go reads a table's constraints (PLAN.md P4.4) from
// pg_constraint: primary keys, unique, foreign key, check and exclusion
// constraints and, from 18, NOT NULL ones — 18 records each NOT NULL column
// as a named constraint of its own (contype 'n'); before 18 it is
// Column.NotNull alone. A constraint trigger (contype 't') is a trigger, and
// is read with Table.Triggers.

// ConstraintKind is pg_constraint.contype.
type ConstraintKind string

const (
	ConstraintPrimaryKey ConstraintKind = "p"
	ConstraintUnique     ConstraintKind = "u"
	ConstraintForeignKey ConstraintKind = "f"
	ConstraintCheck      ConstraintKind = "c"
	ConstraintExclusion  ConstraintKind = "x"
	ConstraintNotNull    ConstraintKind = "n" // 18+
)

// String names the kind as a constraint definition spells it: PRIMARY KEY,
// UNIQUE, FOREIGN KEY, CHECK, EXCLUDE, NOT NULL; an unknown code reads
// ConstraintKind("x").
func (k ConstraintKind) String() string {
	switch k {
	case ConstraintPrimaryKey:
		return "PRIMARY KEY"
	case ConstraintUnique:
		return "UNIQUE"
	case ConstraintForeignKey:
		return "FOREIGN KEY"
	case ConstraintCheck:
		return "CHECK"
	case ConstraintExclusion:
		return "EXCLUDE"
	case ConstraintNotNull:
		return "NOT NULL"
	}
	return fmt.Sprintf("ConstraintKind(%q)", string(k))
}

// ForeignKeyAction is pg_constraint.confupdtype/confdeltype: what a foreign
// key does when the referenced row is updated or deleted.
type ForeignKeyAction string

const (
	ForeignKeyNoAction   ForeignKeyAction = "a"
	ForeignKeyRestrict   ForeignKeyAction = "r"
	ForeignKeyCascade    ForeignKeyAction = "c"
	ForeignKeySetNull    ForeignKeyAction = "n"
	ForeignKeySetDefault ForeignKeyAction = "d"
)

// String names the action as ON UPDATE/ON DELETE spells it: NO ACTION,
// RESTRICT, CASCADE, SET NULL, SET DEFAULT; an unknown code reads
// ForeignKeyAction("x").
func (a ForeignKeyAction) String() string {
	switch a {
	case ForeignKeyNoAction:
		return "NO ACTION"
	case ForeignKeyRestrict:
		return "RESTRICT"
	case ForeignKeyCascade:
		return "CASCADE"
	case ForeignKeySetNull:
		return "SET NULL"
	case ForeignKeySetDefault:
		return "SET DEFAULT"
	}
	return fmt.Sprintf("ForeignKeyAction(%q)", string(a))
}

// ForeignKeyMatch is pg_constraint.confmatchtype: how a multi-column
// foreign key treats a NULL in some of its columns.
type ForeignKeyMatch string

const (
	ForeignKeyMatchSimple  ForeignKeyMatch = "s"
	ForeignKeyMatchFull    ForeignKeyMatch = "f"
	ForeignKeyMatchPartial ForeignKeyMatch = "p" // accepted by the catalog, not implemented by the server
)

// String names the match type as MATCH spells it: SIMPLE, FULL, PARTIAL; an
// unknown code reads ForeignKeyMatch("x").
func (m ForeignKeyMatch) String() string {
	switch m {
	case ForeignKeyMatchSimple:
		return "SIMPLE"
	case ForeignKeyMatchFull:
		return "FULL"
	case ForeignKeyMatchPartial:
		return "PARTIAL"
	}
	return fmt.Sprintf("ForeignKeyMatch(%q)", string(m))
}

// Constraint is one constraint of a table.
type Constraint struct {
	OID  OID
	Name string
	Kind ConstraintKind

	// Definition is the constraint as pg_get_constraintdef renders it, what
	// follows ADD CONSTRAINT name: "PRIMARY KEY (id)", "FOREIGN KEY (pid)
	// REFERENCES app.parent(id) ON DELETE CASCADE", "CHECK ((x > 0)) NOT
	// VALID".
	Definition string

	// Columns are the constrained columns (conkey) in the constraint's
	// order: a key's columns, those a check references, a NOT NULL's one.
	// nil for a check on no column.
	Columns []string

	// Deferrable and InitiallyDeferred: DEFERRABLE, INITIALLY DEFERRED.
	Deferrable        bool
	InitiallyDeferred bool

	// Validated is false for a constraint added NOT VALID and not yet
	// validated: the server enforces it on new rows only.
	Validated bool

	// Enforced is false for a constraint declared NOT ENFORCED (18+, check
	// and foreign key constraints); true on every constraint before 18.
	Enforced bool

	// IsLocal reports a constraint defined on the table itself;
	// InheritCount the number of parents it is inherited from (a
	// partition's copy of its parent's check or NOT NULL constraint). A
	// partition's copies of its parent's keys are not inherited, but
	// attached (pg_constraint.conparentid): they are local.
	IsLocal      bool
	InheritCount int

	// NoInherit is NO INHERIT: children do not get the constraint.
	NoInherit bool

	// Index is the index enforcing a primary key, unique or exclusion
	// constraint, in the table's schema; empty for other kinds.
	Index string

	// ReferencedTable and ReferencedColumns are a foreign key's referenced
	// table and its columns, in the key's order; zero for other kinds.
	ReferencedTable   ObjectName
	ReferencedColumns []string

	// OnUpdate, OnDelete and Match are a foreign key's actions and match
	// type; empty for other kinds.
	OnUpdate ForeignKeyAction
	OnDelete ForeignKeyAction
	Match    ForeignKeyMatch

	// Comment is COMMENT ON CONSTRAINT's text, empty for none.
	Comment string
}

// constraintColumns is the names of the attnums in the int2[] expression
// keys, columns of the relation rel, in the array's order.
func constraintColumns(keys, rel string) string {
	return `ARRAY(SELECT a.attname::pg_catalog.text FROM pg_catalog.unnest(` + keys + `) WITH ORDINALITY AS k(num, n)
		JOIN pg_catalog.pg_attribute a ON a.attrelid = ` + rel + ` AND a.attnum = k.num ORDER BY k.n)`
}

// constraintsQuery reads the constraints of the table $1 but its
// constraint triggers, by name, for a server at num: conenforced is 18+,
// and every constraint before is enforced.
func constraintsQuery(num int) string {
	return `SELECT co.oid, co.conname::pg_catalog.text, co.contype::pg_catalog.text,
	pg_catalog.pg_get_constraintdef(co.oid),
	` + constraintColumns("co.conkey", "co.conrelid") + `,
	co.condeferrable, co.condeferred, co.convalidated,
	` + colSince(num, PostgreSQL18, "co.conenforced", "true") + `,
	co.conislocal, co.coninhcount::integer, co.connoinherit,
	COALESCE(ic.relname::pg_catalog.text, ''),
	COALESCE(fn.nspname::pg_catalog.text, ''), COALESCE(fc.relname::pg_catalog.text, ''),
	` + constraintColumns("co.confkey", "co.confrelid") + `,
	CASE WHEN co.contype = 'f' THEN co.confupdtype::pg_catalog.text ELSE '' END,
	CASE WHEN co.contype = 'f' THEN co.confdeltype::pg_catalog.text ELSE '' END,
	CASE WHEN co.contype = 'f' THEN co.confmatchtype::pg_catalog.text ELSE '' END,
	COALESCE(pg_catalog.obj_description(co.oid, 'pg_constraint'), '')
FROM pg_catalog.pg_constraint co
LEFT JOIN pg_catalog.pg_class ic ON ic.oid = co.conindid AND co.contype IN ('p', 'u', 'x')
LEFT JOIN pg_catalog.pg_class fc ON fc.oid = co.confrelid
LEFT JOIN pg_catalog.pg_namespace fn ON fn.oid = fc.relnamespace
WHERE co.conrelid = $1 AND co.contype <> 't'
ORDER BY co.conname`
}

// scanConstraint builds a *Constraint from one constraintsQuery row.
func scanConstraint(scan func(...any) error) (*Constraint, error) {
	c := &Constraint{}
	var kind, onUpdate, onDelete, match string
	if err := scan(&c.OID, &c.Name, &kind, &c.Definition, &c.Columns,
		&c.Deferrable, &c.InitiallyDeferred, &c.Validated, &c.Enforced,
		&c.IsLocal, &c.InheritCount, &c.NoInherit, &c.Index,
		&c.ReferencedTable.Schema, &c.ReferencedTable.Name, &c.ReferencedColumns,
		&onUpdate, &onDelete, &match, &c.Comment); err != nil {
		return nil, err
	}
	c.Kind = ConstraintKind(kind)
	c.OnUpdate, c.OnDelete, c.Match = ForeignKeyAction(onUpdate), ForeignKeyAction(onDelete), ForeignKeyMatch(match)
	if len(c.Columns) == 0 {
		c.Columns = nil
	}
	if len(c.ReferencedColumns) == 0 {
		c.ReferencedColumns = nil
	}
	return c, nil
}

// Constraints reads the table's constraints by name: keys, checks,
// exclusions and, from 18, NOT NULL constraints. Constraint triggers are on
// Table.Triggers. A TableRef is refused (ErrHandleNotLoaded).
func (t *Table) Constraints(ctx context.Context) ([]*Constraint, error) {
	if err := t.requireLoaded("list constraints of table"); err != nil {
		return nil, err
	}
	q, err := t.on(ctx)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, "list constraints of table "+t.QualifiedName(), scanConstraint,
		constraintsQuery(int(t.db.server.version)), t.OID)
}
