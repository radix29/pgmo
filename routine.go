package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Functions, procedures and aggregates
// ============================================================
//
// routine.go reads a database's routines (PLAN.md P4.6): pg_proc rows,
// through the database's own pool. They are three types, as CREATE
// FUNCTION, CREATE PROCEDURE and CREATE AGGREGATE are three statements
// taking different clauses: a Function (prokind f, and w — a window
// function, written in C) has a return type, volatility, strictness,
// parallel safety, cost and rows; a Procedure (p) has none of these, being
// CALLed; an Aggregate (a) is its pg_aggregate row — transition, final,
// combine and moving-aggregate functions, state types, initial conditions,
// a sort operator — and has no body of its own. Each embeds the
// RoutineIdentity (identity.go): a routine's name is unique only with its
// argument types, so a by-name read with no argument types refuses an
// overloaded name (ErrAmbiguous) rather than pick one.
//
// A routine's body — prosrc and pg_get_functiondef's CREATE OR REPLACE text
// — is read on demand (Body), not by the listings: one listing of pg15's
// own 3,106 functions carries 577 KB of definitions, and a database of
// PL/pgSQL code many times that. The catalog attributes are read eagerly.
//
// What PostgreSQL adds over SQL Server: overloading; procedural languages
// (SQL, PL/pgSQL, C, any an extension installs); per-routine SET of
// configuration parameters (search_path above all, for a SECURITY DEFINER
// routine); user-defined aggregates; parallel-safety and planner cost
// annotations. What it lacks: encrypted definitions and schema binding.
//
// Rendered text — Result, a default's expression, the definition — names
// types and objects as the session's search_path sees them, as every
// expression the server renders does (table.go); the structured fields
// (Argument.Type, ReturnType, an aggregate's functions and types) are
// qualified whatever it is.

// ArgumentMode is pg_proc.proargmodes' entry for one argument: how it
// passes a value.
type ArgumentMode string

// The argument modes.
const (
	ArgumentIn       ArgumentMode = "i" // IN: an input
	ArgumentOut      ArgumentMode = "o" // OUT: a result column
	ArgumentInOut    ArgumentMode = "b" // INOUT: both
	ArgumentVariadic ArgumentMode = "v" // VARIADIC: the last input, an array taking any number of values
	ArgumentTable    ArgumentMode = "t" // a column of RETURNS TABLE (…)
)

// String names the mode as CREATE FUNCTION writes it: "IN", "OUT",
// "INOUT", "VARIADIC"; a RETURNS TABLE column reads "TABLE". An unknown
// value reads ArgumentMode("x").
func (m ArgumentMode) String() string {
	switch m {
	case ArgumentIn:
		return "IN"
	case ArgumentOut:
		return "OUT"
	case ArgumentInOut:
		return "INOUT"
	case ArgumentVariadic:
		return "VARIADIC"
	case ArgumentTable:
		return "TABLE"
	}
	return fmt.Sprintf("ArgumentMode(%q)", string(m))
}

// IsInput reports a mode whose argument is passed in a call — IN, INOUT
// or VARIADIC: the arguments RoutineIdentity.ArgumentTypes lists.
func (m ArgumentMode) IsInput() bool {
	return m == ArgumentIn || m == ArgumentInOut || m == ArgumentVariadic
}

// RoutineArgument is one argument of a routine, input or output, in
// declaration order.
type RoutineArgument struct {
	// Mode is the argument's mode.
	Mode ArgumentMode

	// Name is the argument's name, empty for an unnamed one.
	Name string

	// TypeOID is the argument's type.
	TypeOID OID

	// Type is the type as DDL writes it, qualified outside pg_catalog
	// whatever the session's search_path ("app.mood", "text[]"), as
	// RoutineIdentity.ArgumentTypeNames. An argument carries no type
	// modifier.
	Type string

	// Default is the DEFAULT expression as pg_get_function_arg_default
	// renders it ("'x'::text"), empty for none. Only trailing inputs have
	// one.
	Default string
}

// Volatility is pg_proc.provolatile: what a function's result may depend
// on, so how freely the planner may fold or reuse it.
type Volatility string

// The volatility classes.
const (
	VolatilityImmutable Volatility = "i" // the same arguments always give the same result
	VolatilityStable    Volatility = "s" // …within one statement
	VolatilityVolatile  Volatility = "v" // anything may change it; the default
)

// String names the class as CREATE FUNCTION writes it: "IMMUTABLE",
// "STABLE", "VOLATILE"; an unknown value reads Volatility("x").
func (v Volatility) String() string {
	switch v {
	case VolatilityImmutable:
		return "IMMUTABLE"
	case VolatilityStable:
		return "STABLE"
	case VolatilityVolatile:
		return "VOLATILE"
	}
	return fmt.Sprintf("Volatility(%q)", string(v))
}

// ParallelSafety is pg_proc.proparallel: whether a routine may run in a
// parallel worker.
type ParallelSafety string

// The parallel safety classes.
const (
	ParallelSafe       ParallelSafety = "s" // anywhere, workers included
	ParallelRestricted ParallelSafety = "r" // only in the leader of a parallel plan
	ParallelUnsafe     ParallelSafety = "u" // never in a parallel plan; the default
)

// String names the class as CREATE FUNCTION writes it after PARALLEL:
// "SAFE", "RESTRICTED", "UNSAFE"; an unknown value reads
// ParallelSafety("x").
func (p ParallelSafety) String() string {
	switch p {
	case ParallelSafe:
		return "SAFE"
	case ParallelRestricted:
		return "RESTRICTED"
	case ParallelUnsafe:
		return "UNSAFE"
	}
	return fmt.Sprintf("ParallelSafety(%q)", string(p))
}

// RoutineBody is a function's or procedure's body, read on demand
// (Function.Body, Procedure.Body).
type RoutineBody struct {
	// Source is pg_proc.prosrc: the body as written for an interpreted
	// language (SQL, PL/pgSQL), the link symbol for C, the internal
	// function's name for internal; empty for a SQL-standard body (BEGIN
	// ATOMIC … END or RETURN …), which is stored parsed and only
	// Definition shows.
	Source string

	// Definition is pg_get_functiondef's CREATE OR REPLACE statement — a C
	// function's ends AS 'objectfile', 'symbol'.
	Definition string
}

// Function is one function of a database: a plain or window function. Read
// it with Database.Functions or Database.FunctionByName;
// Database.FunctionRef is a handle holding the name alone.
type Function struct {
	db *Database

	// Origin: IsSystem for a function in a system schema (pg_catalog's);
	// Extension for one an extension owns.
	Origin

	// RoutineIdentity: the OID, schema, name, kind (RoutineFunction or
	// RoutineWindow) and input argument types.
	RoutineIdentity

	// Owner is the owning role's name (proowner).
	Owner string

	// Language is the language's name (sql, plpgsql, c, internal…).
	Language string

	// Arguments are every argument, inputs and outputs (OUT, and the
	// columns of RETURNS TABLE), in declaration order; nil for none.
	Arguments []RoutineArgument

	// ReturnType is the declared result type (prorettype) as DDL writes
	// it, qualified outside pg_catalog: "record" for a function with OUT
	// arguments or RETURNS TABLE, "trigger", "void"…
	ReturnType string

	// ReturnsSet is RETURNS SETOF (or TABLE): the function returns rows.
	ReturnsSet bool

	// Result is pg_get_function_result: the RETURNS clause's text,
	// "SETOF integer", "TABLE(p integer, q app.ty)" — fit for a label.
	Result string

	Volatility Volatility

	// Strict is STRICT (RETURNS NULL ON NULL INPUT): a NULL argument
	// returns NULL without calling the function.
	Strict bool

	// SecurityDefiner is SECURITY DEFINER: the function runs with its
	// owner's privileges.
	SecurityDefiner bool

	// Leakproof is LEAKPROOF: the function reveals nothing about its
	// arguments but its result, so it may run before a security barrier's
	// conditions.
	Leakproof bool

	Parallel ParallelSafety

	// Cost is the planner's cost estimate (COST), in cpu_operator_cost
	// units, per row for a set-returning function.
	Cost float64

	// Rows is the planner's estimate of the rows a set-returning function
	// returns (ROWS); 0 for one that returns no set.
	Rows float64

	// Settings are the SET clauses (proconfig) as stored, search_path among
	// them; nil for none.
	Settings []ParameterSetting

	// SupportFunction is the planner support function (SUPPORT), zero for
	// none.
	SupportFunction ObjectName

	// ObjectFile is a C function's shared library (probin), as written —
	// "$libdir/pg_cron"; empty for any other language.
	ObjectFile string

	// SQLStandardBody reports a body written BEGIN ATOMIC … END or RETURN
	// … (prosqlbody): stored parsed, its dependencies tracked, and no
	// RoutineBody.Source.
	SQLStandardBody bool

	// Privileges are the function's ACL, per grantee, in stored order. One
	// never set reads as the hard-wired default, EXECUTE to PUBLIC and the
	// owner (PrivilegesDefault).
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (proacl NULL).
	PrivilegesDefault bool

	// Comment is COMMENT ON FUNCTION's text, empty for none.
	Comment string
}

// Procedure is one procedure of a database. Read it with
// Database.Procedures or Database.ProcedureByName; Database.ProcedureRef is
// a handle holding the name alone.
type Procedure struct {
	db *Database

	// Origin: IsSystem for one in a system schema; Extension for one an
	// extension owns.
	Origin

	// RoutineIdentity: the OID, schema, name, kind (RoutineProcedure) and
	// input argument types (IN, INOUT, VARIADIC — not OUT, which CALL
	// passes but a signature does not name).
	RoutineIdentity

	// Owner is the owning role's name (proowner).
	Owner string

	// Language is the language's name.
	Language string

	// Arguments are every argument, OUT ones included, in declaration
	// order; nil for none.
	Arguments []RoutineArgument

	// SecurityDefiner is SECURITY DEFINER.
	SecurityDefiner bool

	// Settings are the SET clauses (proconfig) as stored; nil for none.
	Settings []ParameterSetting

	// ObjectFile is a C procedure's shared library (probin); empty for any
	// other language.
	ObjectFile string

	// SQLStandardBody reports a BEGIN ATOMIC … END body (prosqlbody).
	SQLStandardBody bool

	// Privileges are the procedure's ACL, per grantee, in stored order; one
	// never set reads as EXECUTE to PUBLIC and the owner
	// (PrivilegesDefault).
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (proacl NULL).
	PrivilegesDefault bool

	// Comment is COMMENT ON PROCEDURE's text, empty for none.
	Comment string
}

// AggregateKind is pg_aggregate.aggkind.
type AggregateKind string

// The aggregate kinds.
const (
	AggregateNormal       AggregateKind = "n" // sum(x)
	AggregateOrderedSet   AggregateKind = "o" // percentile_cont(0.5) WITHIN GROUP (ORDER BY x)
	AggregateHypothetical AggregateKind = "h" // rank(3) WITHIN GROUP (ORDER BY x): an ordered-set one, HYPOTHETICAL
)

// String names the kind: "normal", "ordered-set", "hypothetical-set"; an
// unknown value reads AggregateKind("x").
func (k AggregateKind) String() string {
	switch k {
	case AggregateNormal:
		return "normal"
	case AggregateOrderedSet:
		return "ordered-set"
	case AggregateHypothetical:
		return "hypothetical-set"
	}
	return fmt.Sprintf("AggregateKind(%q)", string(k))
}

// FinalModify is pg_aggregate.aggfinalmodify (and aggmfinalmodify): what
// the final function does to the transition state.
type FinalModify string

// The final-function modes.
const (
	FinalReadOnly  FinalModify = "r" // READ_ONLY: leaves it as it was
	FinalShareable FinalModify = "s" // SHAREABLE: changes it, but in a way later calls may share
	FinalReadWrite FinalModify = "w" // READ_WRITE: changes it; no more transitions after
)

// String names the mode as CREATE AGGREGATE writes it after FINALFUNC_MODIFY:
// "READ_ONLY", "SHAREABLE", "READ_WRITE"; an unknown value reads
// FinalModify("x").
func (m FinalModify) String() string {
	switch m {
	case FinalReadOnly:
		return "READ_ONLY"
	case FinalShareable:
		return "SHAREABLE"
	case FinalReadWrite:
		return "READ_WRITE"
	}
	return fmt.Sprintf("FinalModify(%q)", string(m))
}

// Aggregate is one aggregate function of a database, its pg_proc row with
// its pg_aggregate row. Read it with Database.Aggregates or
// Database.AggregateByName; Database.AggregateRef is a handle holding the
// name alone.
//
// Function names are qualified whatever the session's search_path
// (CREATE AGGREGATE takes a name and finds the function by the argument
// types it must have); a zero ObjectName is none. Types are written as
// Argument.Type is; empty is none.
type Aggregate struct {
	db *Database

	// Origin: IsSystem for one in a system schema (pg_catalog's); Extension
	// for one an extension owns.
	Origin

	// RoutineIdentity: the OID, schema, name, kind (RoutineAggregate) and
	// argument types — for an ordered-set aggregate the direct ones, then
	// the aggregated ones. One taking * has none (identity.go: Signature
	// is then the ROUTINE form).
	RoutineIdentity

	// Owner is the owning role's name (proowner).
	Owner string

	// Arguments are the arguments in declaration order, nil for none (*).
	Arguments []RoutineArgument

	// ReturnType is the result type as DDL writes it.
	ReturnType string

	// Result is pg_get_function_result's text, as Function.Result.
	Result string

	// Parallel is PARALLEL.
	Parallel ParallelSafety

	// AggregateKind is normal, ordered-set or hypothetical-set.
	AggregateKind AggregateKind

	// DirectArguments is how many of the arguments are direct ones
	// (aggnumdirectargs), before ORDER BY in an ordered-set aggregate's
	// call; 0 for a normal aggregate.
	DirectArguments int

	// TransitionFunction is SFUNC; StateType is STYPE; StateSpace is
	// SSPACE, the state's average size in bytes, 0 for the default
	// estimate.
	TransitionFunction ObjectName
	StateType          string
	StateSpace         int

	// FinalFunction is FINALFUNC; FinalExtra is FINALFUNC_EXTRA (it takes
	// the arguments' types as extra dummy arguments); FinalModify is
	// FINALFUNC_MODIFY.
	FinalFunction ObjectName
	FinalExtra    bool
	FinalModify   FinalModify

	// CombineFunction is COMBINEFUNC (partial aggregation); SerialFunction
	// and DeserialFunction are SERIALFUNC and DESERIALFUNC (an internal
	// state crossing a parallel worker's boundary).
	CombineFunction  ObjectName
	SerialFunction   ObjectName
	DeserialFunction ObjectName

	// The moving-aggregate implementation, for a window frame whose start
	// moves: MSFUNC, MINVFUNC, MSTYPE, MSSPACE, MFINALFUNC,
	// MFINALFUNC_EXTRA, MFINALFUNC_MODIFY. All zero for none — though
	// the catalog stores READ_ONLY as MovingFinalModify even then.
	MovingTransitionFunction ObjectName
	MovingInverseFunction    ObjectName
	MovingStateType          string
	MovingStateSpace         int
	MovingFinalFunction      ObjectName
	MovingFinalExtra         bool
	MovingFinalModify        FinalModify

	// InitialCondition is INITCOND, the state's starting value as a
	// literal's text; HasInitialCondition tells an empty one from none
	// (the state starts NULL). Likewise MINITCOND.
	InitialCondition          string
	HasInitialCondition       bool
	MovingInitialCondition    string
	HasMovingInitialCondition bool

	// SortOperator is SORTOP as CREATE AGGREGATE takes it whatever the
	// search_path, "OPERATOR(pg_catalog.<)" — the operator a min or max
	// aggregate's result is the first by, so an index can answer it; empty
	// for none.
	SortOperator string

	// Privileges are the aggregate's ACL, per grantee, in stored order;
	// one never set reads as EXECUTE to PUBLIC and the owner
	// (PrivilegesDefault).
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (proacl NULL).
	PrivilegesDefault bool

	// Comment is COMMENT ON AGGREGATE's text, empty for none.
	Comment string
}

// Database is the Database the function was read from.
func (f *Function) Database() *Database { return f.db }

// Database is the Database the procedure was read from.
func (p *Procedure) Database() *Database { return p.db }

// Database is the Database the aggregate was read from.
func (a *Aggregate) Database() *Database { return a.db }

// ============================================================
// Queries
// ============================================================

// procedureName is a text[] expression naming the routine joined by
// procedureJoin as alias, {schema, name}, '{}' for none; objectNameOf
// reads it.
func procedureName(alias string) string {
	return `CASE WHEN ` + alias + `.oid IS NULL THEN '{}'
		ELSE ARRAY[` + alias + `n.nspname::pg_catalog.text, ` + alias + `.proname::pg_catalog.text] END`
}

// procedureJoin LEFT JOINs the routine whose OID is the regproc expression
// oid as alias, its schema as alias+"n" — none for 0. A join, not a
// subquery per row: see originJoin.
func procedureJoin(oid, alias string) string {
	return `LEFT JOIN pg_catalog.pg_proc ` + alias + ` ON ` + alias + `.oid = ` + oid + `::pg_catalog.oid
LEFT JOIN pg_catalog.pg_namespace ` + alias + `n ON ` + alias + `n.oid = ` + alias + `.pronamespace`
}

// objectNameOf is a procedureName array as an ObjectName, zero for none.
func objectNameOf(a []string) ObjectName {
	if len(a) != 2 {
		return ObjectName{}
	}
	return ObjectName{Schema: a[0], Name: a[1]}
}

// routineArguments is the LEFT JOIN reading every argument of the routine
// p as five parallel arrays, under alias ar: modes, names, type OIDs, type
// names and defaults, in declaration order; NULLs for none. proallargtypes
// lists every argument and is NULL when all are inputs, when proargtypes
// does (proargmodes is then NULL too: all IN). proargnames is indexed as
// the list it goes with. pg_get_function_arg_default numbers every argument
// from 1 and is NULL for one with no default.
//
// It is one grouped read of every routine's arguments, joined to p by OID,
// rather than a LATERAL per routine: the planner estimates ten rows per
// unnest and the types' joins per routine at a seq scan of pg_type each,
// so a LATERAL listing of pg_catalog's 3,100 functions ran in 5 s on pg15
// and pg18 — 4 of them compiling it, its estimate over jit_above_cost —
// and this one in under 0.4 s. It unnests the routines (pg_proc q) of the
// kinds kinds, an SQL list ("'f', 'w'"), and with byName only those named
// $2, the by-name reads' name.
func routineArguments(kinds string, byName bool) string {
	where := `
		WHERE q.prokind IN (` + kinds + `)`
	if byName {
		where += ` AND q.proname = $2::pg_catalog.text`
	}
	return `LEFT JOIN (SELECT x.proc,
		pg_catalog.array_agg(COALESCE(x.modes[x.n::integer]::pg_catalog.text, 'i') ORDER BY x.n),
		pg_catalog.array_agg(COALESCE(x.names[x.n::integer], '') ORDER BY x.n),
		pg_catalog.array_agg(x.type ORDER BY x.n),
		pg_catalog.array_agg(` + typeName("xt") + ` ORDER BY x.n),
		pg_catalog.array_agg(COALESCE(pg_catalog.pg_get_function_arg_default(x.proc, x.n::integer), '') ORDER BY x.n)
	FROM (SELECT q.oid, q.proargmodes, q.proargnames, u.oid, u.n
		FROM pg_catalog.pg_proc q,
		pg_catalog.unnest(COALESCE(q.proallargtypes, q.proargtypes::pg_catalog.oid[])) WITH ORDINALITY AS u(oid, n)` + where + `
		) AS x(proc, modes, names, type, n)
	` + typeJoins("JOIN", "x.type", "xt") + `
	GROUP BY x.proc) AS ar(proc, modes, names, types, typenames, defaults) ON ar.proc = p.oid`
}

// routineArgumentColumns are routineArguments' columns, in
// routineArgumentArrays' order.
const routineArgumentColumns = `ar.modes, ar.names, ar.types, ar.typenames, ar.defaults`

// routineArgumentArrays are one routine's argument arrays as scanned.
type routineArgumentArrays struct {
	modes, names []string
	types        []OID
	typeNames    []string
	defaults     []string
}

// dest is the arrays' scan destinations, in routineArgumentColumns order.
func (a *routineArgumentArrays) dest() []any {
	return []any{&a.modes, &a.names, &a.types, &a.typeNames, &a.defaults}
}

// arguments zips the arrays; nil for none.
func (a routineArgumentArrays) arguments() ([]RoutineArgument, error) {
	n := len(a.modes)
	if len(a.names) != n || len(a.types) != n || len(a.typeNames) != n || len(a.defaults) != n {
		return nil, fmt.Errorf("argument arrays of different lengths: %d modes, %d names, %d types, %d type names, %d defaults",
			n, len(a.names), len(a.types), len(a.typeNames), len(a.defaults))
	}
	if n == 0 {
		return nil, nil
	}
	out := make([]RoutineArgument, n)
	for i := range out {
		out[i] = RoutineArgument{Mode: ArgumentMode(a.modes[i]), Name: a.names[i], TypeOID: a.types[i],
			Type: a.typeNames[i], Default: a.defaults[i]}
	}
	return out, nil
}

// identity sets r's ArgumentTypeNames from args — the input arguments'
// Type, in order — after checking that their OIDs are r's ArgumentTypes,
// so the names cannot drift from the key they spell.
func (a routineArgumentArrays) identity(r *RoutineIdentity, args []RoutineArgument) error {
	var inputs []OID
	names := make([]string, 0, len(r.ArgumentTypes))
	for _, arg := range args {
		if arg.Mode.IsInput() {
			inputs = append(inputs, arg.TypeOID)
			names = append(names, arg.Type)
		}
	}
	if !r.SameArguments(inputs) {
		return fmt.Errorf("argument types %v of the inputs, not proargtypes' %v", inputs, r.ArgumentTypes)
	}
	r.ArgumentTypeNames = names
	return nil
}

// routineKeyColumns read a RoutineIdentity but its ArgumentTypeNames from
// pg_proc p joined to pg_namespace n, in scanRoutineKey's order; the names
// are the input arguments' (routineArgumentArrays.identity), read once with
// every argument.
const routineKeyColumns = `p.oid, n.nspname, p.proname, p.prokind::pg_catalog.text,
	p.proargtypes::pg_catalog.oid[],
	pg_catalog.pg_get_function_identity_arguments(p.oid)`

// routineHead is the start of every routine read's select list: identity,
// origin, owner, arguments.
var routineHead = `SELECT ` + routineKeyColumns + `,
	` + originJoinColumns("n.nspname") + `,
	pg_catalog.pg_get_userbyid(p.proowner)::pg_catalog.text,
	` + routineArgumentColumns + `,`

// routineTail is the end of every routine read's select list — ACL and
// comment — and its FROM up to the joins each adds.
const routineTail = `
	p.proacl IS NULL,
	pa.grantees, pa.grantors, pa.privileges, pa.grantables,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_proc p
JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace`

// routineJoins are the joins every routine read ends its FROM with: the
// extension, the comment (pg_description, as obj_description reads it —
// a join, not obj_description's subquery per row: see originJoin), the
// arguments (routineArguments'), and the ACL.
func routineJoins(kinds string, byName bool) string {
	return originJoin(catalogProc, "p.oid") + `
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = p.oid
	AND ds.classoid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND ds.objsubid = 0
` + routineArguments(kinds, byName) + `
` + aclLateral("COALESCE(p.proacl, pg_catalog.acldefault('f', p.proowner))", "pa")
}

// functionKinds are the prokinds a Function is: plain and window.
const functionKinds = `'f', 'w'`

// functionSelect is the pg_proc read a *Function is built from, ending in
// its WHERE on the kind — byName for a by-name read, whose arguments are
// then read for the name $2 alone; scanFunction reads its row.
func functionSelect(byName bool) string {
	return routineHead + `
	l.lanname::pg_catalog.text,
	COALESCE(` + typeName("rt") + `, ''), p.proretset,
	COALESCE(pg_catalog.pg_get_function_result(p.oid), ''),
	p.provolatile::pg_catalog.text, p.proisstrict, p.prosecdef, p.proleakproof,
	p.proparallel::pg_catalog.text, p.procost::double precision, p.prorows::double precision,
	COALESCE(p.proconfig, '{}'),
	` + procedureName("sp") + `,
	COALESCE(p.probin, ''), p.prosqlbody IS NOT NULL,` + routineTail + `
JOIN pg_catalog.pg_language l ON l.oid = p.prolang
` + typeJoins("LEFT JOIN", "p.prorettype", "rt") + `
` + procedureJoin("p.prosupport", "sp") + `
` + routineJoins(functionKinds, byName) + `
WHERE p.prokind IN (` + functionKinds + `)`
}

// procedureSelect is the pg_proc read a *Procedure is built from, as
// functionSelect; scanProcedure reads its row.
func procedureSelect(byName bool) string {
	return routineHead + `
	l.lanname::pg_catalog.text,
	p.prosecdef,
	COALESCE(p.proconfig, '{}'),
	COALESCE(p.probin, ''), p.prosqlbody IS NOT NULL,` + routineTail + `
JOIN pg_catalog.pg_language l ON l.oid = p.prolang
` + routineJoins(`'p'`, byName) + `
WHERE p.prokind = 'p'`
}

// aggregateSelect is the pg_proc and pg_aggregate read an *Aggregate is
// built from, as functionSelect; scanAggregate reads its row.
func aggregateSelect(byName bool) string {
	return routineHead + `
	COALESCE(` + typeName("rt") + `, ''),
	COALESCE(pg_catalog.pg_get_function_result(p.oid), ''),
	p.proparallel::pg_catalog.text,
	g.aggkind::pg_catalog.text, g.aggnumdirectargs::integer,
	` + procedureName("tf") + `, COALESCE(` + typeName("st") + `, ''), g.aggtransspace,
	` + procedureName("ff") + `, g.aggfinalextra, g.aggfinalmodify::pg_catalog.text,
	` + procedureName("cf") + `,
	` + procedureName("sf") + `,
	` + procedureName("df") + `,
	` + procedureName("mtf") + `,
	` + procedureName("mif") + `,
	COALESCE(` + typeName("mt") + `, ''), g.aggmtransspace,
	` + procedureName("mff") + `, g.aggmfinalextra, g.aggmfinalmodify::pg_catalog.text,
	COALESCE(g.agginitval, ''), g.agginitval IS NOT NULL,
	COALESCE(g.aggminitval, ''), g.aggminitval IS NOT NULL,
	COALESCE('OPERATOR(' || pg_catalog.quote_ident(son.nspname) || '.' || so.oprname::pg_catalog.text || ')', ''),` + routineTail + `
JOIN pg_catalog.pg_aggregate g ON g.aggfnoid = p.oid
` + typeJoins("LEFT JOIN", "p.prorettype", "rt") + `
` + typeJoins("LEFT JOIN", "g.aggtranstype", "st") + `
` + typeJoins("LEFT JOIN", "g.aggmtranstype", "mt") + `
` + procedureJoin("g.aggtransfn", "tf") + `
` + procedureJoin("g.aggfinalfn", "ff") + `
` + procedureJoin("g.aggcombinefn", "cf") + `
` + procedureJoin("g.aggserialfn", "sf") + `
` + procedureJoin("g.aggdeserialfn", "df") + `
` + procedureJoin("g.aggmtransfn", "mtf") + `
` + procedureJoin("g.aggminvtransfn", "mif") + `
` + procedureJoin("g.aggmfinalfn", "mff") + `
LEFT JOIN pg_catalog.pg_operator so ON so.oid = g.aggsortop
LEFT JOIN pg_catalog.pg_namespace son ON son.oid = so.oprnamespace
` + routineJoins(`'a'`, byName) + `
WHERE p.prokind = 'a'`
}

// The routine reads: each family's listing and by-name select.
var (
	functionList, functionByName   = functionSelect(false), functionSelect(true)
	procedureList, procedureByName = procedureSelect(false), procedureSelect(true)
	aggregateList, aggregateByName = aggregateSelect(false), aggregateSelect(true)
)

// routineFilterColumns are a routine listing's ObjectFilter columns.
var routineFilterColumns = filterColumns{
	name:      "p.proname",
	schema:    "n.nspname",
	system:    systemSchema("n.nspname"),
	extension: isExtensionMember(catalogProc, "p.oid"),
}

// routineByNameWhere narrows a routine read to the schema $1 and name $2,
// by OID so overloads come in a stable order.
const routineByNameWhere = `
	AND n.nspname = $1::pg_catalog.text AND p.proname = $2::pg_catalog.text
ORDER BY p.oid`

// routineListOrder orders a routine listing: schema, name, then the
// overloads by OID, the order they were created in.
const routineListOrder = `
ORDER BY n.nspname, p.proname, p.oid`

// routineScan is the start of every routine row's destinations, in
// routineHead's order; the rest is the type's own, then routineTailDest.
func routineScan(id *RoutineIdentity, o *Origin, owner *string, args *routineArgumentArrays) []any {
	dest := []any{&id.OID, &id.Schema, &id.Name, &id.Kind, &id.ArgumentTypes, &id.IdentityArguments}
	dest = append(dest, scanOrigin(o)...)
	dest = append(dest, owner)
	return append(dest, args.dest()...)
}

// routineTailDest is routineTail's destinations.
func routineTailDest(aclDefault *bool, acl *privilegeArrays, comment *string) []any {
	dest := []any{aclDefault}
	dest = append(dest, acl.dest()...)
	return append(dest, comment)
}

// scanFunction builds a *Function of d from one functionSelect row.
func scanFunction(d *Database, scan func(...any) error) (*Function, error) {
	f := &Function{db: d}
	var args routineArgumentArrays
	var volatility, parallel string
	var config, support []string
	var acl privilegeArrays
	dest := routineScan(&f.RoutineIdentity, &f.Origin, &f.Owner, &args)
	dest = append(dest, &f.Language, &f.ReturnType, &f.ReturnsSet, &f.Result, &volatility, &f.Strict,
		&f.SecurityDefiner, &f.Leakproof, &parallel, &f.Cost, &f.Rows, &config, &support, &f.ObjectFile, &f.SQLStandardBody)
	dest = append(dest, routineTailDest(&f.PrivilegesDefault, &acl, &f.Comment)...)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	f.Volatility, f.Parallel = Volatility(volatility), ParallelSafety(parallel)
	f.Settings = parseSettings(config)
	f.SupportFunction = objectNameOf(support)
	var err error
	if f.Arguments, err = args.arguments(); err != nil {
		return nil, err
	}
	if err = args.identity(&f.RoutineIdentity, f.Arguments); err != nil {
		return nil, err
	}
	f.Privileges, err = acl.all()
	return f, err
}

// scanProcedure builds a *Procedure of d from one procedureSelect row.
func scanProcedure(d *Database, scan func(...any) error) (*Procedure, error) {
	p := &Procedure{db: d}
	var args routineArgumentArrays
	var config []string
	var acl privilegeArrays
	dest := routineScan(&p.RoutineIdentity, &p.Origin, &p.Owner, &args)
	dest = append(dest, &p.Language, &p.SecurityDefiner, &config, &p.ObjectFile, &p.SQLStandardBody)
	dest = append(dest, routineTailDest(&p.PrivilegesDefault, &acl, &p.Comment)...)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	p.Settings = parseSettings(config)
	var err error
	if p.Arguments, err = args.arguments(); err != nil {
		return nil, err
	}
	if err = args.identity(&p.RoutineIdentity, p.Arguments); err != nil {
		return nil, err
	}
	p.Privileges, err = acl.all()
	return p, err
}

// scanAggregate builds an *Aggregate of d from one aggregateSelect row.
func scanAggregate(d *Database, scan func(...any) error) (*Aggregate, error) {
	a := &Aggregate{db: d}
	var args routineArgumentArrays
	var parallel, kind, finalModify, mFinalModify string
	var trans, final, combine, serial, deserial, mTrans, mInv, mFinal []string
	var acl privilegeArrays
	dest := routineScan(&a.RoutineIdentity, &a.Origin, &a.Owner, &args)
	dest = append(dest, &a.ReturnType, &a.Result, &parallel, &kind, &a.DirectArguments,
		&trans, &a.StateType, &a.StateSpace,
		&final, &a.FinalExtra, &finalModify,
		&combine, &serial, &deserial,
		&mTrans, &mInv, &a.MovingStateType, &a.MovingStateSpace,
		&mFinal, &a.MovingFinalExtra, &mFinalModify,
		&a.InitialCondition, &a.HasInitialCondition, &a.MovingInitialCondition, &a.HasMovingInitialCondition,
		&a.SortOperator)
	dest = append(dest, routineTailDest(&a.PrivilegesDefault, &acl, &a.Comment)...)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	a.Parallel, a.AggregateKind = ParallelSafety(parallel), AggregateKind(kind)
	a.FinalModify, a.MovingFinalModify = FinalModify(finalModify), FinalModify(mFinalModify)
	a.TransitionFunction, a.FinalFunction = objectNameOf(trans), objectNameOf(final)
	a.CombineFunction, a.SerialFunction, a.DeserialFunction = objectNameOf(combine), objectNameOf(serial), objectNameOf(deserial)
	a.MovingTransitionFunction, a.MovingInverseFunction = objectNameOf(mTrans), objectNameOf(mInv)
	a.MovingFinalFunction = objectNameOf(mFinal)
	var err error
	if a.Arguments, err = args.arguments(); err != nil {
		return nil, err
	}
	if err = args.identity(&a.RoutineIdentity, a.Arguments); err != nil {
		return nil, err
	}
	a.Privileges, err = acl.all()
	return a, err
}

// ============================================================
// Reads
// ============================================================

// listRoutines lists d's routines of one family: sel narrowed by f, in
// routineListOrder.
func listRoutines[T any](ctx context.Context, d *Database, what, sel string, f ObjectFilter,
	scanT func(*Database, func(...any) error) (T, error)) ([]T, error) {
	where, args, err := f.clause(routineFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list %s of database %q", what, d.Name), func(scan func(...any) error) (T, error) {
		return scanT(d, scan)
	}, sel+where+routineListOrder, args...)
}

// routineFamilyByName reads d's routine of one family named schema.name,
// picking the overload by args as pickOverload does; kind names the family
// in errors ("function").
func routineFamilyByName[T any](ctx context.Context, d *Database, kind, sel, schema, name string, args []OID,
	scanT func(*Database, func(...any) error) (T, error), id func(T) *RoutineIdentity) (T, error) {
	var zero T
	qualified := pgsql.QualifiedName(schema, name)
	if schema == "" {
		return zero, fmt.Errorf("pgmo: read %s %q: %w", kind, name, ErrSchemaRequired)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return zero, err
	}
	what := fmt.Sprintf("read %s %s of database %q", kind, qualified, d.Name)
	found, err := scanRows(ctx, q, what, func(scan func(...any) error) (T, error) {
		return scanT(d, scan)
	}, sel+routineByNameWhere, schema, name)
	if err != nil {
		return zero, err
	}
	r, err := pickOverload(found, id, args, kind)
	if err != nil {
		return zero, fmt.Errorf("pgmo: %s: %w", what, err)
	}
	return r, nil
}

// Functions lists the database's functions — plain and window — by schema,
// name, then argument types, narrowed by f: Name on the function's name,
// Schema on its schema's, System for those in a system schema (pg_catalog's
// thousands), ExtensionMember for those an extension owns. Every function
// is listed whatever the role's privileges on it. Bodies are not read
// (Function.Body). They are read through the database's own pool.
func (d *Database) Functions(ctx context.Context, f ObjectFilter) ([]*Function, error) {
	return listRoutines(ctx, d, "functions", functionList, f, scanFunction)
}

// FunctionByName reads the function schema.name — stored names, matched
// exactly (fold typed input with pgsql.FoldIdent first). schema is required
// (ErrSchemaRequired). With args nil the name must belong to one function:
// none is ErrNotFound, overloads are ErrAmbiguous naming each Signature.
// With args non-nil (empty for none) it reads the function whose input
// argument types are exactly args, ErrNotFound if none. A procedure or
// aggregate of the name is not a function. Use FunctionRef instead when
// only a handle to write through is needed.
func (d *Database) FunctionByName(ctx context.Context, schema, name string, args []OID) (*Function, error) {
	return routineFamilyByName(ctx, d, "function", functionByName, schema, name, args, scanFunction,
		func(f *Function) *RoutineIdentity { return &f.RoutineIdentity })
}

// FunctionRef returns a handle for the function schema.name taking
// arguments of the types argumentTypes without reading the catalog: Schema,
// Name, Kind (RoutineFunction) and ArgumentTypeNames are set and every other
// field is zero, OID included — so Body refuses it (ErrHandleNotLoaded).
// argumentTypes are type names as DDL writes them ("integer",
// "app.mood[]"), qualified as the caller wants them resolved: Signature
// writes them as given. It is not interchangeable with FunctionByName.
func (d *Database) FunctionRef(schema, name string, argumentTypes []string) *Function {
	return &Function{db: d, RoutineIdentity: RoutineIdentity{Schema: schema, Name: name, Kind: RoutineFunction,
		ArgumentTypeNames: argumentTypes}}
}

// Procedures lists the database's procedures by schema, name, then
// argument types, narrowed by f as Functions is. Bodies are not read
// (Procedure.Body).
func (d *Database) Procedures(ctx context.Context, f ObjectFilter) ([]*Procedure, error) {
	return listRoutines(ctx, d, "procedures", procedureList, f, scanProcedure)
}

// ProcedureByName reads the procedure schema.name, picking an overload by
// args as FunctionByName does. A function or aggregate of the name is not a
// procedure.
func (d *Database) ProcedureByName(ctx context.Context, schema, name string, args []OID) (*Procedure, error) {
	return routineFamilyByName(ctx, d, "procedure", procedureByName, schema, name, args, scanProcedure,
		func(p *Procedure) *RoutineIdentity { return &p.RoutineIdentity })
}

// ProcedureRef returns a handle for the procedure schema.name without
// reading the catalog, as FunctionRef does (Kind RoutineProcedure). It is
// not interchangeable with ProcedureByName.
func (d *Database) ProcedureRef(schema, name string, argumentTypes []string) *Procedure {
	return &Procedure{db: d, RoutineIdentity: RoutineIdentity{Schema: schema, Name: name, Kind: RoutineProcedure,
		ArgumentTypeNames: argumentTypes}}
}

// Aggregates lists the database's aggregates by schema, name, then
// argument types, narrowed by f as Functions is.
func (d *Database) Aggregates(ctx context.Context, f ObjectFilter) ([]*Aggregate, error) {
	return listRoutines(ctx, d, "aggregates", aggregateList, f, scanAggregate)
}

// AggregateByName reads the aggregate schema.name, picking an overload by
// args as FunctionByName does — one taking * has no argument types (args
// empty). A function or procedure of the name is not an aggregate.
func (d *Database) AggregateByName(ctx context.Context, schema, name string, args []OID) (*Aggregate, error) {
	return routineFamilyByName(ctx, d, "aggregate", aggregateByName, schema, name, args, scanAggregate,
		func(a *Aggregate) *RoutineIdentity { return &a.RoutineIdentity })
}

// AggregateRef returns a handle for the aggregate schema.name without
// reading the catalog, as FunctionRef does (Kind RoutineAggregate; no
// argument types for one taking *). It is not interchangeable with
// AggregateByName.
func (d *Database) AggregateRef(schema, name string, argumentTypes []string) *Aggregate {
	return &Aggregate{db: d, RoutineIdentity: RoutineIdentity{Schema: schema, Name: name, Kind: RoutineAggregate,
		ArgumentTypeNames: argumentTypes}}
}

// RoutineByName reads the identity of the routine schema.name of any kind —
// function, procedure or aggregate — picking an overload by args as
// FunctionByName does, so a caller holding a name learns which family's
// read gives the rest (RoutineIdentity.Kind).
func (d *Database) RoutineByName(ctx context.Context, schema, name string, args []OID) (*RoutineIdentity, error) {
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	r, err := routineByName(ctx, q, schema, name, args)
	if err != nil {
		return nil, fmt.Errorf("pgmo: %w", err)
	}
	return r, nil
}

// routineBodyQuery reads the body of the routine $1 of the kinds $2.
const routineBodyQuery = `SELECT p.prosrc, pg_catalog.pg_get_functiondef(p.oid)
	FROM pg_catalog.pg_proc p
	WHERE p.oid = $1 AND p.prokind::pg_catalog.text = ANY ($2::pg_catalog.text[])`

// readBody reads the body of the routine id of kinds through d's pool;
// family names it in errors.
func readBody(ctx context.Context, d *Database, id *RoutineIdentity, family string, kinds []string) (RoutineBody, error) {
	var b RoutineBody
	if id.OID == 0 {
		return b, fmt.Errorf("pgmo: read body of %s %s: %w", family, id.Signature(), ErrHandleNotLoaded)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return b, err
	}
	err = queryRowScan(ctx, q, routineBodyQuery, []any{id.OID, kinds}, &b.Source, &b.Definition)
	return foundRow(b, err, notFoundf("pgmo: %s %s not found in database %q", family, id.Signature(), d.Name),
		fmt.Sprintf("read body of %s %s", family, id.Signature()))
}

// Body reads the function's body: its source and its CREATE OR REPLACE
// FUNCTION statement. A FunctionRef is refused (ErrHandleNotLoaded); a
// function dropped since it was read is ErrNotFound.
func (f *Function) Body(ctx context.Context) (RoutineBody, error) {
	return readBody(ctx, f.db, &f.RoutineIdentity, "function", []string{string(RoutineFunction), string(RoutineWindow)})
}

// Body reads the procedure's body: its source and its CREATE OR REPLACE
// PROCEDURE statement. A ProcedureRef is refused (ErrHandleNotLoaded); a
// procedure dropped since it was read is ErrNotFound.
func (p *Procedure) Body(ctx context.Context) (RoutineBody, error) {
	return readBody(ctx, p.db, &p.RoutineIdentity, "procedure", []string{string(RoutineProcedure)})
}
