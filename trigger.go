package pgmo

import (
	"context"
	"fmt"
)

// ============================================================
// Table triggers
// ============================================================
//
// trigger.go reads a table's triggers (PLAN.md P4.4) from pg_trigger:
// ordinary and constraint triggers, with the function each runs. The
// triggers the server creates itself to enforce foreign keys
// (tgisinternal) are not read — they are the foreign key, on
// Table.Constraints. A partition's copy of its parent's trigger is read,
// marked Inherited. Event triggers, which belong to the database, are
// P4.9's.

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

// Trigger is one trigger of a table.
type Trigger struct {
	OID  OID
	Name string

	// Definition is the CREATE TRIGGER statement as pg_get_triggerdef
	// renders it: the table schema-qualified, the function and the WHEN
	// condition as the session's search_path sees them.
	Definition string

	Timing TriggerTiming

	// Events are the statement kinds it fires on, in the server's order
	// (INSERT, DELETE, UPDATE, TRUNCATE).
	Events []TriggerEvent

	// UpdateColumns are UPDATE OF's columns, nil when any UPDATE fires it.
	UpdateColumns []string

	// ForEachRow is FOR EACH ROW; false for FOR EACH STATEMENT.
	ForEachRow bool

	// Function is the trigger function it executes.
	Function ObjectName

	Enabled TriggerEnabled

	// IsConstraint reports a CONSTRAINT TRIGGER, which Deferrable and
	// InitiallyDeferred apply to.
	IsConstraint      bool
	Deferrable        bool
	InitiallyDeferred bool

	// Inherited reports a partition's copy of a trigger on its parent
	// (tgparentid): it is created and dropped with the parent's.
	Inherited bool

	// Comment is COMMENT ON TRIGGER's text, empty for none.
	Comment string
}

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

// triggersQuery reads the triggers of the table $1 the server did not
// create itself, by name.
var triggersQuery = `SELECT tg.oid, tg.tgname::pg_catalog.text, pg_catalog.pg_get_triggerdef(tg.oid),
	tg.tgtype::integer,
	` + constraintColumns("tg.tgattr::pg_catalog.int2[]", "tg.tgrelid") + `,
	pn.nspname::pg_catalog.text, p.proname::pg_catalog.text,
	tg.tgenabled::pg_catalog.text, tg.tgconstraint <> 0, tg.tgdeferrable, tg.tginitdeferred,
	tg.tgparentid <> 0,
	COALESCE(pg_catalog.obj_description(tg.oid, 'pg_trigger'), '')
FROM pg_catalog.pg_trigger tg
JOIN pg_catalog.pg_proc p ON p.oid = tg.tgfoid
JOIN pg_catalog.pg_namespace pn ON pn.oid = p.pronamespace
WHERE tg.tgrelid = $1 AND NOT tg.tgisinternal
ORDER BY tg.tgname`

// scanTrigger builds a *Trigger from one triggersQuery row.
func scanTrigger(scan func(...any) error) (*Trigger, error) {
	t := &Trigger{}
	var tgtype int
	var enabled string
	if err := scan(&t.OID, &t.Name, &t.Definition, &tgtype, &t.UpdateColumns,
		&t.Function.Schema, &t.Function.Name, &enabled, &t.IsConstraint, &t.Deferrable, &t.InitiallyDeferred,
		&t.Inherited, &t.Comment); err != nil {
		return nil, err
	}
	t.decodeTriggerType(tgtype)
	t.Enabled = TriggerEnabled(enabled)
	if len(t.UpdateColumns) == 0 {
		t.UpdateColumns = nil
	}
	return t, nil
}

// Triggers reads the table's triggers by name: ordinary and constraint
// triggers, a partition's inherited ones included, but not those the
// server creates to enforce a foreign key. A TableRef is refused
// (ErrHandleNotLoaded).
func (t *Table) Triggers(ctx context.Context) ([]*Trigger, error) {
	if err := t.requireLoaded("list triggers of table"); err != nil {
		return nil, err
	}
	q, err := t.on(ctx)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, "list triggers of table "+t.QualifiedName(), scanTrigger, triggersQuery, t.OID)
}
