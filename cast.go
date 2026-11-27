package pgmo

import (
	"context"
	"fmt"
)

// ============================================================
// Casts
// ============================================================
//
// cast.go reads a database's casts (PLAN.md P4.11) from pg_cast, through the
// database's own pool: the conversions CAST(x AS t) and the implicit and
// assignment coercions use. A cast has neither a name nor a schema nor an
// owner: it is the pair of its source and target types — unique in the
// database — and every statement names it (source AS target). Creating one
// takes owning either type; dropping one likewise.
//
// IsSystem is the rule for an object with no schema, an OID below
// FirstNormalObjectId (OPEN-THREADS.md § System objects): initdb's casts,
// some 230 of them. An extension can own one (CREATE CAST in its script).
//
// pg_cast is readable by every role, so the listing is the same for all.

// CastContext is pg_cast.castcontext: where the server applies a cast
// without being asked.
type CastContext string

const (
	CastExplicit   CastContext = "e" // only CAST(x AS t) or x::t
	CastAssignment CastContext = "a" // also on assignment to a column (AS ASSIGNMENT)
	CastImplicit   CastContext = "i" // also in any expression (AS IMPLICIT)
)

// CastMethod is pg_cast.castmethod: how the server converts.
type CastMethod string

const (
	CastByFunction CastMethod = "f" // WITH FUNCTION
	CastByInOut    CastMethod = "i" // WITH INOUT: the target's input of the source's output
	CastBinary     CastMethod = "b" // WITHOUT FUNCTION: binary-coercible, the value unchanged
)

// Cast is one cast of a database. Read it with Database.Casts or
// Database.CastByTypes; Database.CastRef is a handle holding the type names
// alone.
type Cast struct {
	db *Database

	// Origin: IsSystem for initdb's casts; Extension for one an extension
	// owns.
	Origin

	OID OID

	// SourceType and TargetType are the two types' OIDs; Source and Target
	// name them as DDL writes them, a type outside pg_catalog
	// schema-qualified whatever the session's search_path ("integer",
	// "app.mood"): the text Signature writes.
	SourceType, TargetType OID
	Source, Target         string

	// Function is the conversion function (castfunc) for CastByFunction,
	// zero otherwise. It takes the source type, and optionally an integer
	// type modifier and a boolean "explicit" flag.
	Function ObjectName

	Context CastContext
	Method  CastMethod

	// Comment is COMMENT ON CAST's text, empty for none.
	Comment string
}

// Database is the Database the cast was read from.
func (c *Cast) Database() *Database { return c.db }

// Signature is the cast's identity as DROP CAST and COMMENT ON CAST take it:
// "(integer AS app.mood)".
func (c *Cast) Signature() string { return "(" + c.Source + " AS " + c.Target + ")" }

// castSelect is the pg_cast read a *Cast is built from, ending in WHERE
// true without an ORDER BY; scanCast reads its row.
var castSelect = `SELECT ca.oid, ca.oid < 16384, COALESCE(ext.extname::pg_catalog.text, ''),
	ca.castsource, ` + typeName("st") + `,
	ca.casttarget, ` + typeName("tt") + `,
	` + procedureName("cf") + `,
	ca.castcontext::pg_catalog.text, ca.castmethod::pg_catalog.text,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_cast ca
` + typeJoins("JOIN", "ca.castsource", "st") + `
` + typeJoins("JOIN", "ca.casttarget", "tt") + `
` + procedureJoin("ca.castfunc", "cf") + `
` + originJoin(catalogCast, "ca.oid") + `
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = ca.oid
	AND ds.classoid = 'pg_catalog.pg_cast'::pg_catalog.regclass AND ds.objsubid = 0
WHERE true`

// scanCast builds a *Cast of d from one castSelect row.
func scanCast(d *Database, scan func(...any) error) (*Cast, error) {
	c := &Cast{db: d}
	var function []string
	var context, method string
	dest := []any{&c.OID}
	dest = append(dest, scanOrigin(&c.Origin)...)
	dest = append(dest, &c.SourceType, &c.Source, &c.TargetType, &c.Target, &function, &context, &method, &c.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	c.Function = objectNameOf(function)
	c.Context, c.Method = CastContext(context), CastMethod(method)
	return c, nil
}

// castFilterColumns are a cast listing's ObjectFilter columns: a cast has
// neither name nor schema, so only System (by OID) and ExtensionMember
// narrow it.
var castFilterColumns = filterColumns{
	system:    "ca.oid < 16384",
	extension: isExtensionMember(catalogCast, "ca.oid"),
}

// Casts lists the database's casts by source then target type name,
// narrowed by f: System for initdb's, ExtensionMember for those an
// extension owns; Name and Schema criteria are ignored, as a cast has
// neither. They are read through the database's own pool.
func (d *Database) Casts(ctx context.Context, f ObjectFilter) ([]*Cast, error) {
	where, args, err := f.clause(castFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list casts of database %q", d.Name), func(scan func(...any) error) (*Cast, error) {
		return scanCast(d, scan)
	}, castSelect+where+`
ORDER BY 5, 7`, args...)
}

// CastByTypes reads the cast from the type source to the type target, by
// OID. A pair the database holds no cast for is ErrNotFound — whether or
// not the server could convert between them another way (a binary
// coercion it knows without a pg_cast row, an I/O conversion to text). Use
// CastRef instead when only a handle to write through is needed.
func (d *Database) CastByTypes(ctx context.Context, source, target OID) (*Cast, error) {
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanCast, castSelect+`
	AND ca.castsource = $1::pg_catalog.oid AND ca.casttarget = $2::pg_catalog.oid`, []any{source, target},
		notFoundf("pgmo: cast from type %d to type %d not found in database %q", source, target, d.Name),
		fmt.Sprintf("read cast from type %d to type %d of database %q", source, target, d.Name))
}

// CastRef returns a handle for the cast (source AS target) without reading
// the catalog: Source and Target are set and every other field is zero,
// OIDs included. source and target are type names as DDL writes them
// ("integer", "app.mood"), qualified as the caller wants them resolved:
// Signature writes them as given. It is not interchangeable with
// CastByTypes.
func (d *Database) CastRef(source, target string) *Cast {
	return &Cast{db: d, Source: source, Target: target}
}
