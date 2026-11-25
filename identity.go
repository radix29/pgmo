package pgmo

// identity.go is how pgmo names a catalog object so that the name picks out
// that object and no other: its OID, its schema-qualified name and, for a
// function, procedure or aggregate, the types of its arguments.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Identity
// ============================================================
//
// Every catalog object pgmo reads carries its OID and, if it lives in a
// schema, its Schema and Name: the OID keys every later read of the object's
// parts, and the qualified name is what a statement about it writes, since
// DDL takes names, not OIDs. The OID alone does not survive a dump and
// restore, and the name alone is not unique:
//
//   - names are unique per schema and per catalog only — a table and a
//     function may share a name, and so may two constraints on two tables;
//   - a function, procedure or aggregate is unique by schema, name and
//     input argument types: overloads share a name in one schema.
//
// So a routine also carries its argument types (RoutineIdentity), and a
// by-name lookup of a routine that names no arguments refuses an overloaded
// name with ErrAmbiguous instead of returning whichever overload the server
// sent first.

// OID is a PostgreSQL object identifier: the oid column of the catalog the
// object is in. It is unique within that catalog (pg_class, pg_proc, …), not
// across catalogs, and for objects inside a database only within that
// database. A zero OID is no object — the value a lookup-free …Ref handle
// carries (ErrHandleNotLoaded).
type OID uint32

// RoutineKind is pg_proc.prokind: what kind of routine a pg_proc row is.
// Each takes its own keyword in DDL (DROP FUNCTION, DROP PROCEDURE, DROP
// AGGREGATE); every one of them is also a ROUTINE.
type RoutineKind string

// The kinds of routine.
const (
	RoutineFunction  RoutineKind = "f" // a function
	RoutineProcedure RoutineKind = "p" // a procedure (CALL)
	RoutineAggregate RoutineKind = "a" // an aggregate, plain or ordered-set
	RoutineWindow    RoutineKind = "w" // a window function (written in C only)
)

// Keyword is the kind's DDL keyword: FUNCTION, PROCEDURE or AGGREGATE (a
// window function is a FUNCTION). An unknown kind answers ROUTINE, which
// every statement about one accepts.
func (k RoutineKind) Keyword() string {
	switch k {
	case RoutineFunction, RoutineWindow:
		return "FUNCTION"
	case RoutineProcedure:
		return "PROCEDURE"
	case RoutineAggregate:
		return "AGGREGATE"
	}
	return "ROUTINE"
}

// RoutineIdentity is what tells a function, procedure or aggregate apart
// from its overloads: the schema, the name and the input argument types.
// The families of P4 embed it.
type RoutineIdentity struct {
	// OID is pg_proc.oid.
	OID OID

	// Schema and Name are the routine's schema and name, as stored.
	Schema, Name string

	// Kind is pg_proc.prokind.
	Kind RoutineKind

	// ArgumentTypes is pg_proc.proargtypes: the OIDs of the input argument
	// types (IN, INOUT and VARIADIC; for an aggregate, the direct then the
	// aggregated ones), in order. With Schema and Name it is the catalog's
	// unique key (pg_proc_proname_args_nsp_index), so it is what matches one
	// overload exactly (SameArguments).
	ArgumentTypes []OID

	// ArgumentTypeNames is ArgumentTypes rendered as DDL writes them, each
	// type outside pg_catalog schema-qualified whatever the session's
	// search_path: "idt.ty", "idt.\"Odd T\"[]", "text[]", "double precision".
	// It is what Signature writes.
	ArgumentTypeNames []string

	// IdentityArguments is pg_get_function_identity_arguments: the arguments
	// with their modes and names, "x idt.ty, VARIADIC y text[], OUT z
	// integer", "double precision ORDER BY anyelement" — fit for a label.
	// Its type names follow the reading session's search_path (a type on it
	// is unqualified), so it is not a key and pgmo never writes it into DDL.
	IdentityArguments string
}

// QualifiedName is the routine's schema-qualified name, quoted as DDL
// writes it.
func (r *RoutineIdentity) QualifiedName() string { return pgsql.QualifiedName(r.Schema, r.Name) }

// Signature is the qualified name with the argument types,
// "idt.f(idt.ty, text[])": the form every statement naming a routine takes
// after ROUTINE (DROP ROUTINE, COMMENT ON ROUTINE, ALTER ROUTINE, GRANT … ON
// ROUTINE), for every kind, and after the kind's own Keyword for every
// routine but an aggregate with no arguments, which AGGREGATE writes
// "ag(*)". An ordered-set aggregate's direct and aggregated types are one
// flat list here; AGGREGATE takes that as well as its ORDER BY form. Its
// text does not depend on the session's search_path.
func (r *RoutineIdentity) Signature() string {
	return r.QualifiedName() + "(" + strings.Join(r.ArgumentTypeNames, ", ") + ")"
}

// SameArguments reports whether the routine takes exactly the input
// argument types types, in order.
func (r *RoutineIdentity) SameArguments(types []OID) bool {
	return slices.Equal(r.ArgumentTypes, types)
}

// typeName is a text expression naming, as DDL writes it, the type joined
// as alias by typeJoins — built here rather than taken from format_type,
// pg_get_function_identity_arguments or regprocedure, which leave a type
// unqualified when the session's search_path finds it, so a name read under
// one path names another type, or none, under the next. A type in
// pg_catalog is format_type's name ("double precision", "\"char\"",
// "text[]"; qualified by format_type itself in the odd session whose path
// shadows it); any other is its schema and name quoted, and an array of one
// (its element type's typarray, as format_type tests) is the element's name
// with "[]" — the array's own name ("_ty") would resolve as well, but nobody
// writes it. No type modifier is written: the OIDs it is used on (argument,
// result and state types) carry none. NULL when a typeJoins LEFT JOIN found
// no type (OID 0).
func typeName(alias string) string {
	t, tn, e, en := alias, alias+"n", alias+"e", alias+"en"
	return `CASE
			WHEN ` + tn + `.nspname = 'pg_catalog' THEN pg_catalog.format_type(` + t + `.oid, NULL)
			WHEN ` + e + `.oid IS NOT NULL THEN pg_catalog.quote_ident(` + en + `.nspname) || '.' || pg_catalog.quote_ident(` + e + `.typname) || '[]'
			ELSE pg_catalog.quote_ident(` + tn + `.nspname) || '.' || pg_catalog.quote_ident(` + t + `.typname)
		END`
}

// typeJoins joins the type whose OID is the expression oid as alias, with
// what typeName reads: its schema (alias+"n"), and the element type an
// array is of (alias+"e") with its schema (alias+"en"). join is "JOIN" for
// an OID always set, "LEFT JOIN" for one that may be 0. Joins rather than a
// scalar subquery per type: the planner prices a subquery per unnested
// element high enough that a listing of pg_catalog's functions crossed
// jit_above_cost and spent seconds compiling (found on pg15 and pg18).
func typeJoins(join, oid, alias string) string {
	t, tn, e, en := alias, alias+"n", alias+"e", alias+"en"
	return join + ` pg_catalog.pg_type ` + t + ` ON ` + t + `.oid = ` + oid + `
		` + join + ` pg_catalog.pg_namespace ` + tn + ` ON ` + tn + `.oid = ` + t + `.typnamespace
		LEFT JOIN pg_catalog.pg_type ` + e + ` ON ` + e + `.oid = ` + t + `.typelem AND ` + e + `.typarray = ` + t + `.oid
		LEFT JOIN pg_catalog.pg_namespace ` + en + ` ON ` + en + `.oid = ` + e + `.typnamespace`
}

// routineIdentityColumns reads a RoutineIdentity from pg_proc p joined to
// pg_namespace n on p.pronamespace, in scanRoutineIdentity's order. The
// argument type names are typeName's. proargtypes carries no typmods, so
// none is lost.
var routineIdentityColumns = `p.oid, n.nspname, p.proname, p.prokind::pg_catalog.text,
	p.proargtypes::pg_catalog.oid[],
	ARRAY(SELECT ` + typeName("t") + `
		FROM pg_catalog.unnest(p.proargtypes::pg_catalog.oid[]) WITH ORDINALITY AS a(oid, n)
		` + typeJoins("JOIN", "a.oid", "t") + `
		ORDER BY a.n),
	pg_catalog.pg_get_function_identity_arguments(p.oid)`

// scanRoutineIdentity is routineIdentityColumns' destinations in r.
func scanRoutineIdentity(r *RoutineIdentity) []any {
	return []any{&r.OID, &r.Schema, &r.Name, &r.Kind, &r.ArgumentTypes, &r.ArgumentTypeNames, &r.IdentityArguments}
}

// routinesByNameQuery reads every routine named $2 in schema $1, by OID so
// the order is stable.
var routinesByNameQuery = `SELECT ` + routineIdentityColumns + `
	FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
	WHERE n.nspname = $1 AND p.proname = $2
	ORDER BY p.oid`

// routineByName reads the routine schema.name through q. With args nil the
// name must be unique in the schema: none is ErrNotFound and more than one
// is ErrAmbiguous, naming each overload's Signature. With args non-nil
// (empty for no arguments) the routine whose ArgumentTypes equal args is
// returned, and none is ErrNotFound. Names match exactly, as stored. The
// error has no "pgmo: " prefix; the exported caller adds it.
func routineByName(ctx context.Context, q Querier, schema, name string, args []OID) (*RoutineIdentity, error) {
	if schema == "" {
		return nil, fmt.Errorf("routine %q: %w", name, ErrSchemaRequired)
	}
	qualified := pgsql.QualifiedName(schema, name)
	found, err := scanRows(ctx, q, "", func(scan func(...any) error) (*RoutineIdentity, error) {
		var r RoutineIdentity
		return &r, scan(scanRoutineIdentity(&r)...)
	}, routinesByNameQuery, schema, name)
	if err != nil {
		return nil, fmt.Errorf("read routine %s: %w", qualified, err)
	}
	r, err := pickOverload(found, func(r *RoutineIdentity) *RoutineIdentity { return r }, args, "routine")
	if err != nil {
		return nil, fmt.Errorf("read routine %s: %w", qualified, err)
	}
	return r, nil
}

// pickOverload picks from found — the routines of one schema and name, of
// the family kind names ("function", "routine") — the one args asks for.
// With args nil the name must be unique: none is ErrNotFound and more than
// one is ErrAmbiguous, naming each overload's Signature. With args non-nil
// (empty for no arguments) it is the one whose ArgumentTypes equal args,
// ErrNotFound if none. The error has no "pgmo: " prefix and no name; the
// caller wraps it with both.
func pickOverload[T any](found []T, id func(T) *RoutineIdentity, args []OID, kind string) (T, error) {
	var zero T
	if args != nil {
		found = slices.DeleteFunc(found, func(r T) bool { return !id(r).SameArguments(args) })
	}
	switch len(found) {
	case 0:
		if args != nil {
			return zero, notFoundf("no %s with argument types %v", kind, args)
		}
		return zero, notFoundf("no such %s", kind)
	case 1:
		return found[0], nil
	}
	sigs := make([]string, len(found))
	for i, r := range found {
		sigs[i] = id(r).Signature()
	}
	return zero, ambiguousf("%d overloads (%s); name the argument types", len(found), strings.Join(sigs, "; "))
}
