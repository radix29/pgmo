package pgmo

import (
	"fmt"
	"strings"
)

// ============================================================
// System objects and extension members
// ============================================================
//
// system_objects.go says where a catalog object comes from, in the two ways a
// tool cares about: whether the server owns it (IsSystem) and whether an
// extension does (Extension). They are separate questions with separate
// answers, and pgmo never folds one into the other:
//
//   - IsSystem is a property of the object's schema: pg_catalog,
//     information_schema, pg_toast, and every session's pg_temp_N and
//     pg_toast_temp_N. The server reserves the pg_ prefix for schemas
//     (CREATE SCHEMA pg_x is 42939), so the rule is information_schema or a
//     name starting pg_ — the same one ObjectFilter.System applies
//     (systemSchema), so a listing's flag and its filter never disagree. A
//     schema's own IsSystem is its name's.
//
//   - Extension is the name of the extension the object is a member of —
//     pg_depend's deptype 'e' row from the object to a pg_extension — or
//     empty. An object belongs to one extension at most (ALTER EXTENSION ADD
//     refuses a second). Only membership counts: deptype 'x' (ALTER … DEPENDS
//     ON EXTENSION) makes an object depend on an extension without being
//     part of it, and it is dropped with it but scripted by pg_dump, so it
//     stays a user object.
//
// An extension member is not a system object. It lives where the extension
// was installed — usually a user schema (pg_stat_statements in public,
// pg_cron's own cron schema) — the user chose to install it, psql's \d lists
// it without S, and dropping or upgrading it is the user's business. What
// sets it apart is that its DDL is the extension's: pg_dump writes CREATE
// EXTENSION rather than the members, and a member's own DROP is refused
// (2BP01) — so a tool hides or marks members, and scripts the extension in
// their place. An extension installed into pg_catalog (plpgsql, pg_cron's
// functions) has members that are system objects as well; the two flags are
// then both set.

// Origin is where a catalog object comes from. The families embed it, so a
// Table answers t.IsSystem and t.Extension directly.
type Origin struct {
	// IsSystem reports an object in a system schema: information_schema or
	// any schema named pg_* (pg_catalog, pg_toast, pg_temp_N,
	// pg_toast_temp_N) — the session's own temporary objects included. For a
	// schema, the schema itself is one.
	IsSystem bool

	// Extension is the name of the extension the object is a member of
	// (pg_depend deptype 'e'), empty for none.
	Extension string
}

// IsExtensionMember reports whether an extension owns the object: its DDL is
// CREATE EXTENSION's, and DROP on it alone is refused.
func (o Origin) IsExtensionMember() bool { return o.Extension != "" }

// IsSystemSchema reports whether the schema named name is a system one, by
// the rule IsSystem and ObjectFilter.System apply: information_schema, or a
// name starting pg_. name is a stored name, matched exactly — "PG_X" is a
// user schema.
func IsSystemSchema(name string) bool {
	return strings.HasPrefix(name, "pg_") || name == "information_schema"
}

// catalogTable is a system catalog an object's row lives in, as pg_depend's
// classid names it. It is written into SQL as a literal, so only the
// constants below are values of it; a family whose objects live in another
// catalog adds one.
type catalogTable string

// The catalogs whose objects pgmo reads an Origin for.
const (
	catalogClass     catalogTable = "pg_catalog.pg_class"     // relations, indexes, sequences
	catalogProc      catalogTable = "pg_catalog.pg_proc"      // functions, procedures, aggregates
	catalogType      catalogTable = "pg_catalog.pg_type"      // types, domains
	catalogNamespace catalogTable = "pg_catalog.pg_namespace" // schemas

	catalogEventTrigger catalogTable = "pg_catalog.pg_event_trigger" // event triggers
)

// extensionMembership is the condition on pg_depend d that it is the row
// making the object whose OID is the expression oid, in catalog, a member of
// the extension d.refobjid. objsubid 0: a member is a whole object, never a
// column.
func extensionMembership(catalog catalogTable, oid string) string {
	return fmt.Sprintf(`d.classid = '%s'::pg_catalog.regclass AND d.objid = %s AND d.objsubid = 0
			AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.deptype = 'e'`, catalog, oid)
}

// extensionOf is a text expression naming the extension that the object oid
// in catalog is a member of, ” for none (Origin.Extension).
func extensionOf(catalog catalogTable, oid string) string {
	return `COALESCE((SELECT e.extname::pg_catalog.text FROM pg_catalog.pg_depend d
		JOIN pg_catalog.pg_extension e ON e.oid = d.refobjid
		WHERE ` + extensionMembership(catalog, oid) + `), '')`
}

// isExtensionMember is a boolean expression true when the object oid in
// catalog is a member of an extension (ObjectFilter.ExtensionMember).
func isExtensionMember(catalog catalogTable, oid string) string {
	return `EXISTS (SELECT FROM pg_catalog.pg_depend d WHERE ` + extensionMembership(catalog, oid) + `)`
}

// originColumns reads an Origin, in scanOrigin's order, for an object whose
// schema name is the expression nspname and whose OID in catalog is oid.
func originColumns(nspname string, catalog catalogTable, oid string) string {
	return systemSchema(nspname) + ", " + extensionOf(catalog, oid)
}

// originJoinColumns read an Origin as originColumns do, in scanOrigin's
// order, with the extension taken from originJoin's join rather than a
// subquery per row.
func originJoinColumns(nspname string) string {
	return systemSchema(nspname) + ", COALESCE(ext.extname::pg_catalog.text, '')"
}

// originJoin LEFT JOINs the membership row (pg_depend d) and the extension
// (pg_extension ext) of the object oid in catalog, for originJoinColumns:
// the planner prices a subquery per row at an index scan each, so a listing
// as long as pg_catalog's functions is estimated over jit_above_cost and
// compiled for seconds, where a join is estimated as the hash join it is.
// An object is a member of one extension at most, so it adds no row.
func originJoin(catalog catalogTable, oid string) string {
	return `LEFT JOIN pg_catalog.pg_depend d ON ` + extensionMembership(catalog, oid) + `
LEFT JOIN pg_catalog.pg_extension ext ON ext.oid = d.refobjid`
}

// scanOrigin is originColumns' destinations in o.
func scanOrigin(o *Origin) []any { return []any{&o.IsSystem, &o.Extension} }
