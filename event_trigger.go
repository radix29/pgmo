package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Event triggers
// ============================================================
//
// event_trigger.go reads a database's event triggers (PLAN.md P4.9) from
// pg_event_trigger, through the database's own pool: the counterpart of SQL
// Server's database-scope DDL triggers (gosmo's DatabaseTrigger), which
// PostgreSQL fires on DDL — before and after a command, on the objects it
// dropped, on a table rewrite — and, from 17, on a login. An event trigger
// is a database's object with no schema: its name is unique in the
// database, and it runs a function returning event_trigger.
//
// The catalog has no built-in rows, so IsSystem — an OID below
// FirstNormalObjectId, the rule for an object with no schema
// (OPEN-THREADS.md § System objects) — is false for every event trigger a
// server holds today; it is read so the flag and ObjectFilter.System keep
// one meaning across families. An extension can own one (CREATE EVENT
// TRIGGER in its script, or ALTER EXTENSION … ADD EVENT TRIGGER).
//
// pg_event_trigger is readable by every role, so the listing is the same
// for all; creating, altering or dropping one takes a superuser.

// EventTriggerEvent is the event an event trigger fires on, as CREATE EVENT
// TRIGGER … ON spells it.
type EventTriggerEvent string

const (
	EventTriggerDDLCommandStart EventTriggerEvent = "ddl_command_start"
	EventTriggerDDLCommandEnd   EventTriggerEvent = "ddl_command_end"
	EventTriggerSQLDrop         EventTriggerEvent = "sql_drop"
	EventTriggerTableRewrite    EventTriggerEvent = "table_rewrite"
	EventTriggerLogin           EventTriggerEvent = "login" // PostgreSQL 17+
)

// EventTrigger is one event trigger of a database. Read it with
// Database.EventTriggers or Database.EventTriggerByName;
// Database.EventTriggerRef is a handle holding the name alone.
type EventTrigger struct {
	db *Database

	// Origin: IsSystem for a built-in event trigger (none exists);
	// Extension for one an extension owns.
	Origin

	OID  OID
	Name string

	// Owner is the owning role's name (evtowner).
	Owner string

	Event EventTriggerEvent

	// Function is the function it executes, one returning event_trigger
	// and taking no arguments: Database.FunctionByName(ctx, schema, name,
	// []OID{}).
	Function ObjectName

	// Enabled is when it fires, as ALTER EVENT TRIGGER … ENABLE [REPLICA |
	// ALWAYS] / DISABLE sets it against session_replication_role.
	Enabled TriggerEnabled

	// Tags are WHEN TAG IN's command tags, as the server stores them
	// (upper case: CREATE TABLE), in the order given; nil when it fires for
	// every command the event covers.
	Tags []string

	// Comment is COMMENT ON EVENT TRIGGER's text, empty for none.
	Comment string
}

// Database is the Database the event trigger was read from.
func (e *EventTrigger) Database() *Database { return e.db }

// QualifiedName is the event trigger's name quoted for a statement. It has
// no schema.
func (e *EventTrigger) QualifiedName() string { return pgsql.QuoteIdent(e.Name) }

// eventTriggerSelect is the pg_event_trigger read an *EventTrigger is built
// from, ending in WHERE true without an ORDER BY; scanEventTrigger reads its
// row. The extension and the comment are joins (originJoin).
var eventTriggerSelect = `SELECT et.oid, et.evtname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(et.evtowner)::pg_catalog.text,
	et.oid < 16384, COALESCE(ext.extname::pg_catalog.text, ''),
	et.evtevent::pg_catalog.text, pn.nspname::pg_catalog.text, p.proname::pg_catalog.text,
	et.evtenabled::pg_catalog.text, et.evttags,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_event_trigger et
JOIN pg_catalog.pg_proc p ON p.oid = et.evtfoid
JOIN pg_catalog.pg_namespace pn ON pn.oid = p.pronamespace
` + originJoin(catalogEventTrigger, "et.oid") + `
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = et.oid
	AND ds.classoid = 'pg_catalog.pg_event_trigger'::pg_catalog.regclass AND ds.objsubid = 0
WHERE true`

// scanEventTrigger builds an *EventTrigger of d from one eventTriggerSelect
// row.
func scanEventTrigger(d *Database, scan func(...any) error) (*EventTrigger, error) {
	e := &EventTrigger{db: d}
	var event, enabled string
	dest := []any{&e.OID, &e.Name, &e.Owner}
	dest = append(dest, scanOrigin(&e.Origin)...)
	dest = append(dest, &event, &e.Function.Schema, &e.Function.Name, &enabled, &e.Tags, &e.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	e.Event = EventTriggerEvent(event)
	e.Enabled = TriggerEnabled(enabled)
	if len(e.Tags) == 0 {
		e.Tags = nil
	}
	return e, nil
}

// eventTriggerFilterColumns are an event trigger listing's ObjectFilter
// columns: no schema; System on the OID (none is built in).
var eventTriggerFilterColumns = filterColumns{
	name:      "et.evtname",
	system:    "et.oid < 16384",
	extension: isExtensionMember(catalogEventTrigger, "et.oid"),
}

// EventTriggers lists the database's event triggers by name, narrowed by
// f: Name on the trigger's name, System for built-in ones (none exists),
// ExtensionMember for those an extension owns; a Schema criterion is
// ignored, as an event trigger has none. They are read through the
// database's own pool.
func (d *Database) EventTriggers(ctx context.Context, f ObjectFilter) ([]*EventTrigger, error) {
	where, args, err := f.clause(eventTriggerFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list event triggers of database %q", d.Name), func(scan func(...any) error) (*EventTrigger, error) {
		return scanEventTrigger(d, scan)
	}, eventTriggerSelect+where+`
ORDER BY et.evtname`, args...)
}

// EventTriggerByName reads the event trigger name — a stored name, matched
// exactly (fold typed input with pgsql.FoldIdent first). One the database
// does not hold is ErrNotFound. Use EventTriggerRef instead when only a
// handle to write through is needed.
func (d *Database) EventTriggerByName(ctx context.Context, name string) (*EventTrigger, error) {
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanEventTrigger, eventTriggerSelect+`
	AND et.evtname = $1::pg_catalog.text`, []any{name},
		notFoundf("pgmo: event trigger %s not found in database %q", pgsql.QuoteIdent(name), d.Name),
		fmt.Sprintf("read event trigger %s of database %q", pgsql.QuoteIdent(name), d.Name))
}

// EventTriggerRef returns a handle for the event trigger name without
// reading the catalog: Name is set and every other field is zero, OID
// included. It is not interchangeable with EventTriggerByName.
func (d *Database) EventTriggerRef(name string) *EventTrigger {
	return &EventTrigger{db: d, Name: name}
}
