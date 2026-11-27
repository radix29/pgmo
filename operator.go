package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Operators
// ============================================================
//
// operator.go reads a database's operators (PLAN.md P4.11) from
// pg_operator, through the database's own pool. An operator is a schema's
// object named by symbols (+, @>, ===) and, like a function, overloaded:
// its identity is its schema, name and operand types — the left one none
// for a prefix operator (postfix ones went in 14) — and every statement
// names it schema.name (left, right), the operator name unquoted (it is no
// identifier) and NONE for an absent operand.
//
// pg_operator is readable by every role, and an operator has no ACL of its
// own (using one takes EXECUTE on its function), so the listing is the
// same for every role. pg_catalog holds some 800.

// OperatorKind is pg_operator.oprkind.
type OperatorKind string

const (
	OperatorInfix  OperatorKind = "b" // left and right operands
	OperatorPrefix OperatorKind = "l" // right operand only
)

// Operator is one operator of a database. Read it with Database.Operators
// or Database.OperatorByName; Database.OperatorRef is a handle holding the
// name and operand type names alone.
type Operator struct {
	db *Database

	// Origin: IsSystem for an operator in a system schema (pg_catalog's);
	// Extension for one an extension owns.
	Origin

	OID    OID
	Schema string
	Name   string

	// Owner is the owning role's name (oprowner).
	Owner string

	Kind OperatorKind

	// LeftType, RightType and ResultType are the operand and result types'
	// OIDs, LeftType 0 for a prefix operator; Left, Right and Result name
	// them as DDL writes them, a type outside pg_catalog schema-qualified
	// whatever the session's search_path — Left empty for none. Left and
	// Right are what Signature writes.
	LeftType, RightType, ResultType OID
	Left, Right, Result             string

	// Function is the function implementing it (oprcode, FUNCTION); zero
	// for a shell operator, one a COMMUTATOR or NEGATOR clause named before
	// it was defined.
	Function ObjectName

	// Commutator and Negator are the operators COMMUTATOR and NEGATOR
	// named, zero for none. A commutator's operand types are this one's
	// swapped; a negator's are this one's.
	Commutator, Negator ObjectName

	// Restrict and Join are the selectivity estimators (RESTRICT, JOIN),
	// zero for none.
	Restrict, Join ObjectName

	// Merges and Hashes report MERGES and HASHES (oprcanmerge,
	// oprcanhash): the operator can take part in a merge or hash join.
	Merges, Hashes bool

	// Comment is COMMENT ON OPERATOR's text, empty for none.
	Comment string
}

// Database is the Database the operator was read from.
func (o *Operator) Database() *Database { return o.db }

// QualifiedName is the operator's name for a statement: the schema quoted,
// the name as it is (an operator name is no identifier) — "app.===".
func (o *Operator) QualifiedName() string { return pgsql.QuoteIdent(o.Schema) + "." + o.Name }

// Signature is the operator's identity as DROP OPERATOR, ALTER OPERATOR and
// COMMENT ON OPERATOR take it: "app.=== (integer, app.mood)", NONE for an
// absent left operand.
func (o *Operator) Signature() string {
	left := o.Left
	if left == "" {
		left = "NONE"
	}
	return o.QualifiedName() + " (" + left + ", " + o.Right + ")"
}

// operatorName is a text[] expression naming the operator joined as alias
// with its schema as alias+"n", {schema, name}, '{}' for none —
// objectNameOf reads it.
func operatorName(alias string) string {
	return `CASE WHEN ` + alias + `.oid IS NULL THEN '{}'
		ELSE ARRAY[` + alias + `n.nspname::pg_catalog.text, ` + alias + `.oprname::pg_catalog.text] END`
}

// operatorSelect is the pg_operator read an *Operator is built from, ending
// in WHERE true without an ORDER BY; scanOperator reads its row. Every
// referenced type, function and operator is a join (see originJoin): the
// listing of pg_catalog's 800 stays under jit_above_cost.
var operatorSelect = `SELECT o.oid, n.nspname::pg_catalog.text, o.oprname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(o.oprowner)::pg_catalog.text,
	` + originJoinColumns("n.nspname") + `,
	o.oprkind::pg_catalog.text,
	o.oprleft, COALESCE(` + typeName("lt") + `, ''),
	o.oprright, COALESCE(` + typeName("rt") + `, ''),
	o.oprresult, COALESCE(` + typeName("ot") + `, ''),
	` + procedureName("fc") + `,
	` + operatorName("oc") + `, ` + operatorName("og") + `,
	` + procedureName("fr") + `, ` + procedureName("fj") + `,
	o.oprcanmerge, o.oprcanhash,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_operator o
JOIN pg_catalog.pg_namespace n ON n.oid = o.oprnamespace
` + typeJoins("LEFT JOIN", "o.oprleft", "lt") + `
` + typeJoins("LEFT JOIN", "o.oprright", "rt") + `
` + typeJoins("LEFT JOIN", "o.oprresult", "ot") + `
` + procedureJoin("o.oprcode", "fc") + `
LEFT JOIN pg_catalog.pg_operator oc ON oc.oid = o.oprcom
LEFT JOIN pg_catalog.pg_namespace ocn ON ocn.oid = oc.oprnamespace
LEFT JOIN pg_catalog.pg_operator og ON og.oid = o.oprnegate
LEFT JOIN pg_catalog.pg_namespace ogn ON ogn.oid = og.oprnamespace
` + procedureJoin("o.oprrest", "fr") + `
` + procedureJoin("o.oprjoin", "fj") + `
` + originJoin(catalogOperator, "o.oid") + `
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = o.oid
	AND ds.classoid = 'pg_catalog.pg_operator'::pg_catalog.regclass AND ds.objsubid = 0
WHERE true`

// scanOperator builds an *Operator of d from one operatorSelect row.
func scanOperator(d *Database, scan func(...any) error) (*Operator, error) {
	o := &Operator{db: d}
	var kind string
	var function, commutator, negator, restrict, join []string
	dest := []any{&o.OID, &o.Schema, &o.Name, &o.Owner}
	dest = append(dest, scanOrigin(&o.Origin)...)
	dest = append(dest, &kind, &o.LeftType, &o.Left, &o.RightType, &o.Right, &o.ResultType, &o.Result,
		&function, &commutator, &negator, &restrict, &join, &o.Merges, &o.Hashes, &o.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	o.Kind = OperatorKind(kind)
	o.Function, o.Commutator, o.Negator = objectNameOf(function), objectNameOf(commutator), objectNameOf(negator)
	o.Restrict, o.Join = objectNameOf(restrict), objectNameOf(join)
	return o, nil
}

// operatorFilterColumns are an operator listing's ObjectFilter columns.
var operatorFilterColumns = filterColumns{
	name:      "o.oprname",
	schema:    "n.nspname",
	system:    systemSchema("n.nspname"),
	extension: isExtensionMember(catalogOperator, "o.oid"),
}

// Operators lists the database's operators by schema, name, then operand
// type OIDs, narrowed by f: Name on the operator's name (no folding changes
// one: an operator name has no letters), Schema on its schema's, System
// for those in a system schema, ExtensionMember for those an extension
// owns. They are read through the database's own pool.
func (d *Database) Operators(ctx context.Context, f ObjectFilter) ([]*Operator, error) {
	where, args, err := f.clause(operatorFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list operators of database %q", d.Name), func(scan func(...any) error) (*Operator, error) {
		return scanOperator(d, scan)
	}, operatorSelect+where+`
ORDER BY n.nspname, o.oprname, o.oprleft, o.oprright`, args...)
}

// OperatorByName reads the operator schema.name taking a left operand of
// the type left (0 for a prefix operator) and a right one of the type
// right — the catalog's unique key. schema is required
// (ErrSchemaRequired); none is ErrNotFound. Use OperatorRef instead when
// only a handle to write through is needed.
func (d *Database) OperatorByName(ctx context.Context, schema, name string, left, right OID) (*Operator, error) {
	what := pgsql.QuoteIdent(schema) + "." + name + fmt.Sprintf(" (%d, %d)", left, right)
	if schema == "" {
		return nil, fmt.Errorf("pgmo: read operator %q: %w", name, ErrSchemaRequired)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanOperator, operatorSelect+`
	AND n.nspname = $1::pg_catalog.text AND o.oprname = $2::pg_catalog.text
	AND o.oprleft = $3::pg_catalog.oid AND o.oprright = $4::pg_catalog.oid`, []any{schema, name, left, right},
		notFoundf("pgmo: operator %s not found in database %q", what, d.Name),
		fmt.Sprintf("read operator %s of database %q", what, d.Name))
}

// OperatorRef returns a handle for the operator schema.name (left, right)
// without reading the catalog: Schema, Name, Left and Right are set and
// every other field is zero, OIDs included. left and right are type names
// as DDL writes them ("integer", "app.mood"), left empty for a prefix
// operator, qualified as the caller wants them resolved: Signature writes
// them as given. It is not interchangeable with OperatorByName.
func (d *Database) OperatorRef(schema, name, left, right string) *Operator {
	return &Operator{db: d, Schema: schema, Name: name, Left: left, Right: right}
}
