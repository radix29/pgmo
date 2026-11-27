package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Triggers
// ============================================================
//
// trigger.go reads triggers from pg_trigger (PLAN.md P4.4, P4.9): ordinary
// and constraint triggers on a table, a partitioned table, a view (INSTEAD
// OF) or a foreign table, with the function each runs and the arguments it
// is passed. They are read per relation — Table.Triggers, View.Triggers —
// or for the whole database, Database.Triggers, the folder SSMS has no
// counterpart of but a tool searching "which triggers call this function"
// needs. The triggers the server creates itself to enforce foreign keys
// (tgisinternal) are not read — they are the foreign key, on
// Table.Constraints. A partition's copy of its parent's trigger is read,
// marked Inherited.
//
// A trigger has no schema of its own: its name is unique on its relation,
// not in a schema, and DDL names it NAME ON schema.relation. So it is
// addressed by three names (TriggerByName), and its Origin is its
// relation's — a trigger is created, dumped and dropped with it, and cannot
// be an extension member on its own (ALTER EXTENSION … ADD takes no
// TRIGGER).
//
// Event triggers, which belong to the database and fire on DDL, are
// event_trigger.go's.

// TriggerTiming is when a trigger fires relative to its event.
type TriggerTiming string

const (
	TriggerBefore    TriggerTiming = "BEFORE"
	TriggerAfter     TriggerTiming = "AFTER"
	TriggerInsteadOf TriggerTiming = "INSTEAD OF"
)

// TriggerEvent is a statement kind a trigger fires on, as CREATE TRIGGER
// spells it.
type TriggerEvent string

const (
	TriggerInsert   TriggerEvent = "INSERT"
	TriggerDelete   TriggerEvent = "DELETE"
	TriggerUpdate   TriggerEvent = "UPDATE"
	TriggerTruncate TriggerEvent = "TRUNCATE"
)

// TriggerEnabled is pg_trigger.tgenabled: when a trigger fires, as ALTER
// TABLE … ENABLE/DISABLE TRIGGER sets it against
// session_replication_role.
type TriggerEnabled string

const (
	TriggerEnabledOrigin  TriggerEnabled = "O" // ENABLE: fires unless the session is a replica
	TriggerEnabledReplica TriggerEnabled = "R" // ENABLE REPLICA: fires only in a replica session
	TriggerEnabledAlways  TriggerEnabled = "A" // ENABLE ALWAYS: fires in every session
	TriggerDisabled       TriggerEnabled = "D" // DISABLE
)

// String names the state as ALTER TABLE sets it: ENABLE, ENABLE REPLICA,
// ENABLE ALWAYS, DISABLE; an unknown code reads TriggerEnabled("x").
func (e TriggerEnabled) String() string {
	switch e {
	case TriggerEnabledOrigin:
		return "ENABLE"
	case TriggerEnabledReplica:
		return "ENABLE REPLICA"
	case TriggerEnabledAlways:
		return "ENABLE ALWAYS"
	case TriggerDisabled:
		return "DISABLE"
	}
	return fmt.Sprintf("TriggerEnabled(%q)", string(e))
}

// Trigger is one trigger on a relation. Read it with Table.Triggers,
// View.Triggers, Database.Triggers or Database.TriggerByName;
// Database.TriggerRef is a handle holding the names alone.
type Trigger struct {
	db *Database

	// Origin is the relation's: IsSystem for one in a system schema,
	// Extension for one an extension owns.
	Origin

	OID OID

	// Schema and Table name the relation the trigger is on, and TableKind
	// says what it is: a table, a partitioned table, a view or a foreign
	// table.
	Schema    string
	Table     string
	TableKind RelationKind

	Name string

	// Definition is the CREATE TRIGGER statement as pg_get_triggerdef
	// renders it: the table schema-qualified, the function and the WHEN
	// condition as the session's search_path sees them, the arguments'
	// literals as its standard_conforming_strings reads them (a backslash
	// doubled when it is off).
	Definition string

	Timing TriggerTiming

	// Events are the statement kinds it fires on, in the server's order
	// (INSERT, DELETE, UPDATE, TRUNCATE).
	Events []TriggerEvent

	// UpdateColumns are UPDATE OF's columns, nil when any UPDATE fires it.
	UpdateColumns []string

	// ForEachRow is FOR EACH ROW; false for FOR EACH STATEMENT.
	ForEachRow bool

	// Function is the trigger function it executes. A trigger function
	// takes no declared arguments, so it is the Function of this name with
	// none: Database.FunctionByName(ctx, schema, name, []OID{}).
	Function ObjectName

	// Arguments are EXECUTE FUNCTION's arguments, the function's TG_ARGV:
	// string literals as given, in order, nil for none. An empty string is
	// an argument.
	Arguments []string

	Enabled TriggerEnabled

	// IsConstraint reports a CONSTRAINT TRIGGER, which Deferrable and
	// InitiallyDeferred apply to.
	IsConstraint      bool
	Deferrable        bool
	InitiallyDeferred bool

	// Inherited reports a partition's copy of a trigger on its parent
	// (tgparentid): it is created and dropped with the parent's.
	Inherited bool

	// OldTable and NewTable are REFERENCING OLD TABLE AS / NEW TABLE AS's
	// transition relation names, empty for none.
	OldTable string
	NewTable string

	// HasCondition reports a WHEN condition. Its text is in Definition
	// alone: the server renders it only there (pg_get_expr refuses an
	// expression over both OLD and NEW).
	HasCondition bool

	// Comment is COMMENT ON TRIGGER's text, empty for none.
	Comment string
}

// Database is the Database the trigger was read from.
func (t *Trigger) Database() *Database { return t.db }

// QualifiedTable is the trigger's relation quoted for a statement:
// schema.table, as DROP TRIGGER name ON … takes it.
func (t *Trigger) QualifiedTable() string { return pgsql.QualifiedName(t.Schema, t.Table) }

// The pg_trigger.tgtype bits (the server's pg_trigger.h).
const (
	triggerTypeRow      = 1 << 0
	triggerTypeBefore   = 1 << 1
	triggerTypeInsert   = 1 << 2
	triggerTypeDelete   = 1 << 3
	triggerTypeUpdate   = 1 << 4
	triggerTypeTruncate = 1 << 5
	triggerTypeInstead  = 1 << 6
)

// decodeTriggerType sets t's Timing, Events and ForEachRow from tgtype.
func (t *Trigger) decodeTriggerType(tgtype int) {
	switch {
	case tgtype&triggerTypeInstead != 0:
		t.Timing = TriggerInsteadOf
	case tgtype&triggerTypeBefore != 0:
		t.Timing = TriggerBefore
	default:
		t.Timing = TriggerAfter
	}
	t.Events = nil
	for _, e := range []struct {
		bit   int
		event TriggerEvent
	}{{triggerTypeInsert, TriggerInsert}, {triggerTypeDelete, TriggerDelete},
		{triggerTypeUpdate, TriggerUpdate}, {triggerTypeTruncate, TriggerTruncate}} {
		if tgtype&e.bit != 0 {
			t.Events = append(t.Events, e.event)
		}
	}
	t.ForEachRow = tgtype&triggerTypeRow != 0
}

// triggerSelect is the pg_trigger read a *Trigger is built from, ending in
// its WHERE leaving out the triggers the server made itself, without an
// ORDER BY; scanTrigger reads its row. The relation's extension, UPDATE
// OF's column names, the arguments and the comment are joins, not a
// subquery per row (CLAUDE.md § Conventions). Both grouped reads are keyed
// by (oid, tgrelid), so a read of one relation's triggers narrows them to it
// too.
//
// tgargs is each argument's bytes in the database's encoding, each followed
// by a NUL — no encoding has a NUL inside a character, and the server
// refuses one in a literal — so the arguments are the spans between NULs.
// They are split as text: encode(…, 'escape') writes a NUL as \000, a
// backslash as \\ and a byte over 127 as \ooo; with every \\ rewritten as
// \134 (the same byte), every backslash left starts a three-digit escape,
// so \000 is found only where a NUL was. Each piece is decoded back to its
// bytes and converted from the database's encoding as the server converts
// any text; the piece after the last NUL is past tgnargs and dropped. Not
// generate_series over the bytes: the planner prices each call at 15, and a
// listing of 6,000 triggers would cross jit_above_cost. The literals are
// escape strings, read the same whatever standard_conforming_strings is.
var triggerSelect = `SELECT tg.oid, n.nspname::pg_catalog.text, c.relname::pg_catalog.text, c.relkind::pg_catalog.text,
	` + originJoinColumns("n.nspname") + `,
	tg.tgname::pg_catalog.text, pg_catalog.pg_get_triggerdef(tg.oid),
	tg.tgtype::integer, uc.cols,
	pn.nspname::pg_catalog.text, p.proname::pg_catalog.text, ta.args,
	tg.tgenabled::pg_catalog.text, tg.tgconstraint <> 0, tg.tgdeferrable, tg.tginitdeferred,
	tg.tgparentid <> 0,
	COALESCE(tg.tgoldtable::pg_catalog.text, ''), COALESCE(tg.tgnewtable::pg_catalog.text, ''),
	tg.tgqual IS NOT NULL,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_trigger tg
JOIN pg_catalog.pg_class c ON c.oid = tg.tgrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_proc p ON p.oid = tg.tgfoid
JOIN pg_catalog.pg_namespace pn ON pn.oid = p.pronamespace
` + originJoin(catalogClass, "c.oid") + `
LEFT JOIN (SELECT k.oid, k.tgrelid, pg_catalog.array_agg(a.attname::pg_catalog.text ORDER BY k.n) AS cols
	FROM (SELECT t1.oid, t1.tgrelid, u.num, u.n
		FROM pg_catalog.pg_trigger t1, pg_catalog.unnest(t1.tgattr::pg_catalog.int2[]) WITH ORDINALITY AS u(num, n)) AS k
	JOIN pg_catalog.pg_attribute a ON a.attrelid = k.tgrelid AND a.attnum = k.num
	GROUP BY k.oid, k.tgrelid) AS uc ON uc.oid = tg.oid AND uc.tgrelid = tg.tgrelid
LEFT JOIN (SELECT t2.oid, t2.tgrelid,
		pg_catalog.array_agg(pg_catalog.convert_from(pg_catalog.decode(x.arg, 'escape'), pg_catalog.getdatabaseencoding())
			ORDER BY x.n) AS args
	FROM pg_catalog.pg_trigger t2,
		pg_catalog.unnest(pg_catalog.string_to_array(pg_catalog.replace(pg_catalog.encode(t2.tgargs, 'escape'),
			E'\\\\', E'\\134'), E'\\000')) WITH ORDINALITY AS x(arg, n)
	WHERE x.n <= t2.tgnargs
	GROUP BY t2.oid, t2.tgrelid) AS ta ON ta.oid = tg.oid AND ta.tgrelid = tg.tgrelid
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = tg.oid
	AND ds.classoid = 'pg_catalog.pg_trigger'::pg_catalog.regclass AND ds.objsubid = 0
WHERE NOT tg.tgisinternal`

// relationTriggersQuery reads the triggers of the relation $1, by name.
var relationTriggersQuery = triggerSelect + `
	AND tg.tgrelid = $1
ORDER BY tg.tgname`

// scanTrigger builds a *Trigger of d from one triggerSelect row.
func scanTrigger(d *Database, scan func(...any) error) (*Trigger, error) {
	t := &Trigger{db: d}
	var kind, enabled string
	var tgtype int
	dest := []any{&t.OID, &t.Schema, &t.Table, &kind}
	dest = append(dest, scanOrigin(&t.Origin)...)
	dest = append(dest, &t.Name, &t.Definition, &tgtype, &t.UpdateColumns,
		&t.Function.Schema, &t.Function.Name, &t.Arguments,
		&enabled, &t.IsConstraint, &t.Deferrable, &t.InitiallyDeferred,
		&t.Inherited, &t.OldTable, &t.NewTable, &t.HasCondition, &t.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	t.TableKind = RelationKind(kind)
	t.decodeTriggerType(tgtype)
	t.Enabled = TriggerEnabled(enabled)
	if len(t.UpdateColumns) == 0 {
		t.UpdateColumns = nil
	}
	if len(t.Arguments) == 0 {
		t.Arguments = nil
	}
	return t, nil
}

// relationTriggers reads the triggers of the relation oid in d, by name;
// what names the read.
func relationTriggers(ctx context.Context, d *Database, oid OID, what string) ([]*Trigger, error) {
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, what, func(scan func(...any) error) (*Trigger, error) {
		return scanTrigger(d, scan)
	}, relationTriggersQuery, oid)
}

// Triggers reads the table's triggers by name: ordinary and constraint
// triggers, a partition's inherited ones included, but not those the
// server creates to enforce a foreign key. A TableRef is refused
// (ErrHandleNotLoaded).
func (t *Table) Triggers(ctx context.Context) ([]*Trigger, error) {
	if err := t.requireLoaded("list triggers of table"); err != nil {
		return nil, err
	}
	return relationTriggers(ctx, t.db, t.OID, "list triggers of table "+t.QualifiedName())
}

// Triggers reads the view's triggers by name — INSTEAD OF triggers on its
// rows, and statement-level BEFORE/AFTER ones. A ViewRef is refused
// (ErrHandleNotLoaded).
func (v *View) Triggers(ctx context.Context) ([]*Trigger, error) {
	if err := v.requireLoaded("list triggers of view"); err != nil {
		return nil, err
	}
	return relationTriggers(ctx, v.db, v.OID, "list triggers of view "+v.QualifiedName())
}

// triggerFilterColumns are a trigger listing's ObjectFilter columns: Name on
// the trigger's name; Schema, System and ExtensionMember on its relation's.
var triggerFilterColumns = filterColumns{
	name:      "tg.tgname",
	schema:    "n.nspname",
	system:    systemSchema("n.nspname"),
	extension: isExtensionMember(catalogClass, "c.oid"),
}

// Triggers lists every trigger in the database — on tables, partitioned
// tables, views and foreign tables, partitions' inherited ones included,
// but not those the server creates to enforce a foreign key — by schema,
// relation, then name, narrowed by f: Name on the trigger's name, Schema on
// its relation's schema, System for those on a relation in a system schema,
// ExtensionMember for those on a relation an extension owns. They are read
// through the database's own pool.
func (d *Database) Triggers(ctx context.Context, f ObjectFilter) ([]*Trigger, error) {
	where, args, err := f.clause(triggerFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list triggers of database %q", d.Name), func(scan func(...any) error) (*Trigger, error) {
		return scanTrigger(d, scan)
	}, triggerSelect+where+`
ORDER BY n.nspname, c.relname, tg.tgname`, args...)
}

// TriggerByName reads the trigger name on the relation schema.table —
// stored names, matched exactly (fold typed input with pgsql.FoldIdent
// first). schema is required (ErrSchemaRequired). A trigger the relation
// does not have, one the server made for a foreign key, or a relation the
// database does not hold, is ErrNotFound. Use TriggerRef instead when only
// a handle to write through is needed.
func (d *Database) TriggerByName(ctx context.Context, schema, table, name string) (*Trigger, error) {
	desc := fmt.Sprintf("%s on %s", pgsql.QuoteIdent(name), pgsql.QualifiedName(schema, table))
	if schema == "" {
		return nil, fmt.Errorf("pgmo: read trigger %s: %w", desc, ErrSchemaRequired)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanTrigger, triggerSelect+`
	AND n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text AND tg.tgname = $3::pg_catalog.text`,
		[]any{schema, table, name},
		notFoundf("pgmo: trigger %s not found in database %q", desc, d.Name),
		fmt.Sprintf("read trigger %s of database %q", desc, d.Name))
}

// TriggerRef returns a handle for the trigger name on the relation
// schema.table without reading the catalog: Schema, Table and Name are set
// and every other field is zero, OID included. It is not interchangeable
// with TriggerByName.
func (d *Database) TriggerRef(schema, table, name string) *Trigger {
	return &Trigger{db: d, Schema: schema, Table: table, Name: name}
}
