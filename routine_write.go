package pgmo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Function and procedure writes
// ============================================================
//
// routine_write.go is P4.13.3's functions and procedures: CREATE [OR
// REPLACE] FUNCTION / PROCEDURE from a request, DROP (CASCADE opt-in),
// ALTER … RENAME TO / SET SCHEMA / OWNER TO, COMMENT ON. A routine is named
// by its Signature — the qualified name with its input argument types —
// whatever the session's search_path (ARCHITECTURE.md § Identity).
//
// The body is written dollar-quoted (pgsql.QuoteDollar), as pg_dump writes
// it, so it reads exactly as given; types and defaults are SQL, written as
// given.

// RoutineArgumentSpec is one argument of CreateFunction or CreateProcedure.
type RoutineArgumentSpec struct {
	// Mode is ArgumentIn (or ""), ArgumentOut, ArgumentInOut,
	// ArgumentVariadic, or — a function's alone — ArgumentTable, a column
	// of RETURNS TABLE (…).
	Mode ArgumentMode

	// Name is the argument's name; "" for none (a RETURNS TABLE column
	// needs one).
	Name string

	// Type is the argument's type as SQL ("integer", "app.mood",
	// "text[]"), written as given: qualify one outside pg_catalog.
	Type string

	// Default is an input argument's DEFAULT expression as SQL; "" for
	// none.
	Default string
}

// isInput reports whether the argument is part of the routine's signature.
func (a RoutineArgumentSpec) isInput() bool {
	return a.Mode == "" || a.Mode.IsInput()
}

// definition is the argument as the parameter list writes it.
func (a RoutineArgumentSpec) definition() string {
	var parts []string
	switch a.Mode {
	case ArgumentOut, ArgumentInOut, ArgumentVariadic:
		parts = append(parts, a.Mode.String())
	}
	if a.Name != "" {
		parts = append(parts, pgsql.QuoteIdent(a.Name))
	}
	parts = append(parts, a.Type)
	if a.Default != "" {
		parts = append(parts, "DEFAULT "+a.Default)
	}
	return strings.Join(parts, " ")
}

// routineParameters are args' parameter list, its RETURNS TABLE columns, and
// the input types that make the signature, or the refusal of an argument
// that cannot be written; procedure refuses RETURNS TABLE columns.
func routineParameters(args []RoutineArgumentSpec, procedure bool) (params, table, inputs []string, err error) {
	for i, a := range args {
		if a.Type == "" {
			return nil, nil, nil, invalidf("argument %d: no type", i+1)
		}
		switch a.Mode {
		case "", ArgumentIn, ArgumentOut, ArgumentInOut, ArgumentVariadic:
		case ArgumentTable:
			if procedure {
				return nil, nil, nil, invalidf("argument %d: a procedure returns no table", i+1)
			}
			if a.Name == "" || a.Default != "" {
				return nil, nil, nil, invalidf("argument %d: a RETURNS TABLE column takes a name and no default", i+1)
			}
			table = append(table, pgsql.QuoteIdent(a.Name)+" "+a.Type)
			continue
		default:
			return nil, nil, nil, invalidf("argument %d: unknown mode %q", i+1, string(a.Mode))
		}
		if a.Default != "" && !a.isInput() {
			return nil, nil, nil, invalidf("argument %d: an OUT argument takes no default", i+1)
		}
		params = append(params, a.definition())
		if a.isInput() {
			inputs = append(inputs, a.Type)
		}
	}
	return params, table, inputs, nil
}

// routineSettings are SET clauses, one per setting: SET name = 'v'[, …],
// a list parameter's elements each a literal (ParameterSetting.Values).
func routineSettings(settings []ParameterSetting) ([]string, error) {
	var out []string
	for _, p := range settings {
		if p.Name == "" {
			return nil, invalidf("a setting with no name")
		}
		out = append(out, "SET "+parameterAssignment(p.Name, p.Values()))
	}
	return out, nil
}

// CreateFunctionRequest describes a new function, or — with OrReplace — the
// new definition of an existing one with the same input argument types.
type CreateFunctionRequest struct {
	Schema, Name string

	// OrReplace is OR REPLACE: an existing function with the same input
	// types is redefined in place, keeping its OID, owner and privileges;
	// its result type cannot change. Every attribute left unset here reverts
	// to its default.
	OrReplace bool

	Arguments []RoutineArgumentSpec

	// Returns is RETURNS's type as SQL; ReturnsSet makes it RETURNS SETOF.
	// "" with ArgumentTable columns is RETURNS TABLE (…); "" with OUT
	// arguments lets the server derive it (record, or the one OUT type).
	Returns    string
	ReturnsSet bool

	// Language is LANGUAGE: sql, plpgsql, or another installed one.
	Language string

	// Body is the function's source, written dollar-quoted as given.
	Body string

	// Volatility is IMMUTABLE, STABLE or VOLATILE; "" for the server's
	// (VOLATILE).
	Volatility Volatility

	// Strict is STRICT: NULL in any argument returns NULL without a call.
	Strict bool

	// SecurityDefiner is SECURITY DEFINER: it runs with its owner's
	// privileges. Give it Settings with a search_path of trusted schemas.
	SecurityDefiner bool

	// Leakproof is LEAKPROOF: it reveals nothing about its arguments but
	// its result. Only a superuser declares it.
	Leakproof bool

	// Parallel is PARALLEL SAFE, RESTRICTED or UNSAFE; "" for the server's
	// (UNSAFE).
	Parallel ParallelSafety

	// Cost and Rows are COST (in cpu_operator_cost units) and ROWS (a
	// set-returning function's estimate); nil for the server's.
	Cost *float64
	Rows *float64

	// Settings are SET clauses: parameters set while it runs, as
	// Function.Settings reads them.
	Settings []ParameterSetting
}

// createFunctionStatement is req's CREATE FUNCTION and the input types of
// its signature, or the refusal of one that cannot be written. No prefix:
// the caller names the operation.
func createFunctionStatement(req CreateFunctionRequest) (string, []string, error) {
	if req.Language == "" {
		return "", nil, invalidf("no language")
	}
	params, table, inputs, err := routineParameters(req.Arguments, false)
	if err != nil {
		return "", nil, err
	}
	if len(table) > 0 && (req.Returns != "" || req.ReturnsSet) {
		return "", nil, invalidf("RETURNS TABLE columns and a RETURNS type together")
	}
	if req.ReturnsSet && req.Returns == "" {
		return "", nil, invalidf("RETURNS SETOF with no type")
	}
	settings, err := routineSettings(req.Settings)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	b.WriteString("CREATE ")
	if req.OrReplace {
		b.WriteString("OR REPLACE ")
	}
	b.WriteString("FUNCTION " + pgsql.QualifiedName(req.Schema, req.Name) + "(" + strings.Join(params, ", ") + ")")
	switch {
	case len(table) > 0:
		b.WriteString(" RETURNS TABLE (" + strings.Join(table, ", ") + ")")
	case req.ReturnsSet:
		b.WriteString(" RETURNS SETOF " + req.Returns)
	case req.Returns != "":
		b.WriteString(" RETURNS " + req.Returns)
	}
	b.WriteString(" LANGUAGE " + pgsql.QuoteIdent(req.Language))
	switch req.Volatility {
	case "":
	case VolatilityImmutable, VolatilityStable, VolatilityVolatile:
		b.WriteString(" " + req.Volatility.String())
	default:
		return "", nil, invalidf("unknown volatility %q", string(req.Volatility))
	}
	if req.Strict {
		b.WriteString(" STRICT")
	}
	if req.SecurityDefiner {
		b.WriteString(" SECURITY DEFINER")
	}
	if req.Leakproof {
		b.WriteString(" LEAKPROOF")
	}
	switch req.Parallel {
	case "":
	case ParallelSafe, ParallelRestricted, ParallelUnsafe:
		b.WriteString(" PARALLEL " + req.Parallel.String())
	default:
		return "", nil, invalidf("unknown parallel safety %q", string(req.Parallel))
	}
	if req.Cost != nil {
		b.WriteString(" COST " + strconv.FormatFloat(*req.Cost, 'g', -1, 64))
	}
	if req.Rows != nil {
		b.WriteString(" ROWS " + strconv.FormatFloat(*req.Rows, 'g', -1, 64))
	}
	for _, s := range settings {
		b.WriteString(" " + s)
	}
	b.WriteString(" AS " + pgsql.QuoteDollar(req.Body))
	return b.String(), inputs, nil
}

// CreateFunction creates the function req describes, and returns it read
// back from the catalog — or, under Scripting(ctx), the FunctionRef handle
// with the request's input types. The read-back finds the function by
// schema and name; when overloads share them it cannot tell which is new
// from the types as written ("int" is integer), and returns the handle too,
// whose Signature names the right one.
func (d *Database) CreateFunction(ctx context.Context, req CreateFunctionRequest) (*Function, error) {
	if err := requireQualified("create function", req.Schema, req.Name); err != nil {
		return nil, err
	}
	op := "create function " + pgsql.QualifiedName(req.Schema, req.Name)
	stmt, inputs, err := createFunctionStatement(req)
	if err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	if err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	handle := d.FunctionRef(req.Schema, req.Name, inputs)
	return createdObject(ctx, handle, func() (*Function, error) {
		return unlessAmbiguous(handle)(d.FunctionByName(ctx, req.Schema, req.Name, nil))
	})
}

// unlessAmbiguous turns a read-back's ErrAmbiguous into handle: the create
// succeeded, and only which overload is the new one is unknown.
func unlessAmbiguous[T any](handle T) func(T, error) (T, error) {
	return func(v T, err error) (T, error) {
		if errors.Is(err, ErrAmbiguous) {
			return handle, nil
		}
		return v, err
	}
}

// CreateProcedureRequest describes a new procedure, or — with OrReplace —
// the new definition of an existing one.
type CreateProcedureRequest struct {
	Schema, Name string

	// OrReplace is OR REPLACE, as CreateFunctionRequest's.
	OrReplace bool

	// Arguments take every mode but ArgumentTable.
	Arguments []RoutineArgumentSpec

	Language string
	Body     string // written dollar-quoted as given

	// SecurityDefiner is SECURITY DEFINER: it runs with its owner's
	// privileges.
	SecurityDefiner bool

	// Settings are SET clauses. A procedure with one cannot COMMIT.
	Settings []ParameterSetting
}

// createProcedureStatement is req's CREATE PROCEDURE and the input types of
// its signature, or the refusal of one that cannot be written.
func createProcedureStatement(req CreateProcedureRequest) (string, []string, error) {
	if req.Language == "" {
		return "", nil, invalidf("no language")
	}
	params, _, inputs, err := routineParameters(req.Arguments, true)
	if err != nil {
		return "", nil, err
	}
	settings, err := routineSettings(req.Settings)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	b.WriteString("CREATE ")
	if req.OrReplace {
		b.WriteString("OR REPLACE ")
	}
	b.WriteString("PROCEDURE " + pgsql.QualifiedName(req.Schema, req.Name) + "(" + strings.Join(params, ", ") + ")")
	b.WriteString(" LANGUAGE " + pgsql.QuoteIdent(req.Language))
	if req.SecurityDefiner {
		b.WriteString(" SECURITY DEFINER")
	}
	for _, s := range settings {
		b.WriteString(" " + s)
	}
	b.WriteString(" AS " + pgsql.QuoteDollar(req.Body))
	return b.String(), inputs, nil
}

// CreateProcedure creates the procedure req describes, and returns it read
// back from the catalog — or the ProcedureRef handle, under Scripting(ctx)
// or when overloads share the name (as CreateFunction).
func (d *Database) CreateProcedure(ctx context.Context, req CreateProcedureRequest) (*Procedure, error) {
	if err := requireQualified("create procedure", req.Schema, req.Name); err != nil {
		return nil, err
	}
	op := "create procedure " + pgsql.QualifiedName(req.Schema, req.Name)
	stmt, inputs, err := createProcedureStatement(req)
	if err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	if err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	handle := d.ProcedureRef(req.Schema, req.Name, inputs)
	return createdObject(ctx, handle, func() (*Procedure, error) {
		return unlessAmbiguous(handle)(d.ProcedureByName(ctx, req.Schema, req.Name, nil))
	})
}

// routineDDL is a routine's DDL handle for verb: keyword then its
// Signature, or the refusal of one missing its schema or name.
func routineDDL(db *Database, keyword string, r *RoutineIdentity, verb string) (ddlObject, error) {
	if err := requireQualified(verb+" "+keywordLabel(keyword), r.Schema, r.Name); err != nil {
		return ddlObject{}, err
	}
	return ddlObject{keyword: keyword, name: r.Signature(), run: db.exec}, nil
}

func (f *Function) ddl(verb string) (ddlObject, error) {
	return routineDDL(f.db, "FUNCTION", &f.RoutineIdentity, verb)
}

func (p *Procedure) ddl(verb string) (ddlObject, error) {
	return routineDDL(p.db, "PROCEDURE", &p.RoutineIdentity, verb)
}

// Drop drops the function by its Signature (DROP FUNCTION), with CASCADE
// when opts.Cascade — which drops the triggers, defaults and views using
// it.
func (f *Function) Drop(ctx context.Context, opts DropOptions) error {
	return dropObject(ctx, f.ddl, opts)
}

// Rename renames the function (ALTER FUNCTION … RENAME TO), mirrored onto
// Name (not under WithScript).
func (f *Function) Rename(ctx context.Context, newName string) error {
	return renameObject(ctx, f.ddl, newName, &f.Name)
}

// SetSchema moves the function to schema (ALTER FUNCTION … SET SCHEMA),
// mirrored onto Schema (not under WithScript).
func (f *Function) SetSchema(ctx context.Context, schema string) error {
	return moveObject(ctx, f.ddl, schema, &f.Schema)
}

// SetOwner makes role the function's owner (ALTER FUNCTION … OWNER TO),
// mirrored onto Owner (not under WithScript). A SECURITY DEFINER function
// then runs with the new owner's privileges.
func (f *Function) SetOwner(ctx context.Context, role string) error {
	return ownObject(ctx, f.ddl, role, &f.Owner)
}

// SetComment sets the function's comment (COMMENT ON FUNCTION), ""
// removing it, mirrored onto Comment (not under WithScript).
func (f *Function) SetComment(ctx context.Context, text string) error {
	return commentObject(ctx, f.ddl, text, &f.Comment)
}

// Drop drops the procedure by its Signature (DROP PROCEDURE), with CASCADE
// when opts.Cascade.
func (p *Procedure) Drop(ctx context.Context, opts DropOptions) error {
	return dropObject(ctx, p.ddl, opts)
}

// Rename renames the procedure, mirrored onto Name (not under WithScript).
func (p *Procedure) Rename(ctx context.Context, newName string) error {
	return renameObject(ctx, p.ddl, newName, &p.Name)
}

// SetSchema moves the procedure to schema, mirrored onto Schema (not under
// WithScript).
func (p *Procedure) SetSchema(ctx context.Context, schema string) error {
	return moveObject(ctx, p.ddl, schema, &p.Schema)
}

// SetOwner makes role the procedure's owner, mirrored onto Owner (not under
// WithScript).
func (p *Procedure) SetOwner(ctx context.Context, role string) error {
	return ownObject(ctx, p.ddl, role, &p.Owner)
}

// SetComment sets the procedure's comment, "" removing it, mirrored onto
// Comment (not under WithScript).
func (p *Procedure) SetComment(ctx context.Context, text string) error {
	return commentObject(ctx, p.ddl, text, &p.Comment)
}
