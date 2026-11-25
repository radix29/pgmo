package pgmo_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// The column types of the routine reads, in SELECT order, as the server
// sends them: the shared head (identity, origin, owner, arguments), each
// family's own columns, then the ACL and the comment.
var (
	routineHeadTypes = []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.OIDArrayOID, pgtype.TextOID,
		pgtype.BoolOID, pgtype.TextOID, pgtype.TextOID,
		pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.OIDArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID}
	routineTailTypes = append(append([]uint32{pgtype.BoolOID}, aclTypes...), pgtype.TextOID)

	functionTypes = concatTypes(routineHeadTypes, []uint32{pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
		pgtype.TextOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.TextOID, pgtype.Float8OID, pgtype.Float8OID,
		pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextOID, pgtype.BoolOID}, routineTailTypes)
	procedureTypes = concatTypes(routineHeadTypes, []uint32{pgtype.TextOID, pgtype.BoolOID, pgtype.TextArrayOID,
		pgtype.TextOID, pgtype.BoolOID}, routineTailTypes)
	aggregateTypes = concatTypes(routineHeadTypes, []uint32{pgtype.TextOID, pgtype.TextOID, pgtype.TextOID,
		pgtype.TextOID, pgtype.Int4OID,
		pgtype.TextArrayOID, pgtype.TextOID, pgtype.Int4OID,
		pgtype.TextArrayOID, pgtype.BoolOID, pgtype.TextOID,
		pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID,
		pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextOID, pgtype.Int4OID,
		pgtype.TextArrayOID, pgtype.BoolOID, pgtype.TextOID,
		pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID, pgtype.BoolOID,
		pgtype.TextOID}, routineTailTypes)

	routineBodyTypes = []uint32{pgtype.TextOID, pgtype.TextOID}
)

// concatTypes joins column type lists.
func concatTypes(parts ...[]uint32) []uint32 {
	var out []uint32
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// A function, a procedure and an aggregate with every column set to
// something its neighbours are not.
var (
	rowFunction = []any{uint32(41001), "app", "f", "f", []uint32{23, 41000}, "a integer, VARIADIC b mood[]",
		false, "myext", "own",
		[]string{"i", "v", "o"}, []string{"a", "b", ""}, []uint32{23, 41000, 25},
		[]string{"integer", "app.mood[]", "text"}, []string{"", "'{}'::mood[]", ""},
		"plpgsql", "record", true, "SETOF record", "s", true, true, false, "r", float64(7.5), float64(30),
		[]string{"search_path=app, pg_temp", "work_mem=8MB"}, []string{"app", "f_support"}, "", true,
		false, []string{"own", ""}, []string{"own", "own"}, []string{"EXECUTE", "EXECUTE"}, []bool{true, false},
		"the f"}
	rowProcedure = []any{uint32(42001), "app", "p", "p", []uint32{23}, "INOUT a integer, OUT b text",
		true, "", "own",
		[]string{"b", "o"}, []string{"a", "b"}, []uint32{23, 25}, []string{"integer", "text"}, []string{"", ""},
		"c", true, []string{"work_mem=4MB"}, "$libdir/mylib", false,
		true, []string{"own"}, []string{"own"}, []string{"EXECUTE"}, []bool{false},
		"the p"}
	rowAggregate = []any{uint32(43001), "app", "ag", "a", []uint32{701, 2283}, "double precision ORDER BY anyelement",
		false, "", "own",
		[]string{"i", "i"}, []string{"", ""}, []uint32{701, 2283}, []string{"double precision", "anyelement"}, []string{"", ""},
		"anyelement", "anyelement", "s",
		"o", int32(1),
		[]string{"pg_catalog", "ordered_set_transition"}, "internal", int32(48),
		[]string{"app", "ag_final"}, true, "w",
		[]string{"app", "ag_combine"}, []string{"app", "ag_ser"}, []string{"app", "ag_deser"},
		[]string{"app", "ag_mtrans"}, []string{"app", "ag_minv"}, "app.st", int32(16),
		[]string{"app", "ag_mfinal"}, false, "s",
		"", true, "{0}", true,
		"OPERATOR(pg_catalog.<)",
		false, []string{"B b"}, []string{"own"}, []string{"EXECUTE"}, []bool{false},
		"the ag"}
)

// The three reads, told apart in a fake.
const (
	functionMatch  = "WHERE p.prokind IN ('f', 'w')"
	procedureMatch = "WHERE p.prokind = 'p'"
	aggregateMatch = "WHERE p.prokind = 'a'"
	bodyMatch      = "pg_catalog.pg_get_functiondef(p.oid)"
)

// loadedRoutines reads rowFunction, rowProcedure and rowAggregate by name
// from database appdb of a fresh fake, which answers each other read with
// the responders given.
func loadedRoutines(t *testing.T, parts map[string]pgmotest.Responder) (*pgmotest.Fake, *pgmotest.Fake,
	*pgmo.Function, *pgmo.Procedure, *pgmo.Aggregate) {
	t.Helper()
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(functionMatch, rowsOf(functionTypes, rowFunction))
	other.On(procedureMatch, rowsOf(procedureTypes, rowProcedure))
	other.On(aggregateMatch, rowsOf(aggregateTypes, rowAggregate))
	for match, r := range parts {
		other.On(match, r)
	}
	d := f.Server().DatabaseRef("appdb")
	fn, err := d.FunctionByName(t.Context(), "app", "f", nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := d.ProcedureByName(t.Context(), "app", "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := d.AggregateByName(t.Context(), "app", "ag", nil)
	if err != nil {
		t.Fatal(err)
	}
	return f, other, fn, p, a
}

// Every field of the three types is filled from its own column, the
// identity's type names are the input arguments', the names are bound as
// $1 and $2 — the arguments read for $2 alone — and the reads go to the
// routine's own database.
func TestRoutineFamiliesByNameFillEveryField(t *testing.T) {
	f, other, fn, p, a := loadedRoutines(t, nil)
	for what, d := range map[string]*pgmo.Database{"function": fn.Database(), "procedure": p.Database(), "aggregate": a.Database()} {
		if d == nil || d.Name != "appdb" {
			t.Errorf("%s Database() = %v", what, d)
		}
	}
	wantFunction := pgmo.Function{
		Origin: pgmo.Origin{Extension: "myext"},
		RoutineIdentity: pgmo.RoutineIdentity{OID: 41001, Schema: "app", Name: "f", Kind: pgmo.RoutineFunction,
			ArgumentTypes: []pgmo.OID{23, 41000}, ArgumentTypeNames: []string{"integer", "app.mood[]"},
			IdentityArguments: "a integer, VARIADIC b mood[]"},
		Owner: "own", Language: "plpgsql",
		Arguments: []pgmo.RoutineArgument{
			{Mode: pgmo.ArgumentIn, Name: "a", TypeOID: 23, Type: "integer"},
			{Mode: pgmo.ArgumentVariadic, Name: "b", TypeOID: 41000, Type: "app.mood[]", Default: "'{}'::mood[]"},
			{Mode: pgmo.ArgumentOut, TypeOID: 25, Type: "text"},
		},
		ReturnType: "record", ReturnsSet: true, Result: "SETOF record",
		Volatility: pgmo.VolatilityStable, Strict: true, SecurityDefiner: true, Parallel: pgmo.ParallelRestricted,
		Cost: 7.5, Rows: 30,
		Settings:        []pgmo.ParameterSetting{{Name: "search_path", Value: "app, pg_temp"}, {Name: "work_mem", Value: "8MB"}},
		SupportFunction: pgmo.ObjectName{Schema: "app", Name: "f_support"},
		SQLStandardBody: true,
		Privileges: []pgmo.PrivilegeGrant{{Grantee: "own", Grantor: "own", Privilege: pgmo.PrivilegeExecute, Grantable: true},
			{Grantee: "", Grantor: "own", Privilege: pgmo.PrivilegeExecute}},
		Comment: "the f",
	}
	gf := *fn
	gf.SetDatabase(nil)
	if !reflect.DeepEqual(gf, wantFunction) {
		t.Errorf("FunctionByName =\n%+v\nwant\n%+v", gf, wantFunction)
	}
	wantProcedure := pgmo.Procedure{
		Origin: pgmo.Origin{IsSystem: true},
		RoutineIdentity: pgmo.RoutineIdentity{OID: 42001, Schema: "app", Name: "p", Kind: pgmo.RoutineProcedure,
			ArgumentTypes: []pgmo.OID{23}, ArgumentTypeNames: []string{"integer"}, IdentityArguments: "INOUT a integer, OUT b text"},
		Owner: "own", Language: "c",
		Arguments: []pgmo.RoutineArgument{{Mode: pgmo.ArgumentInOut, Name: "a", TypeOID: 23, Type: "integer"},
			{Mode: pgmo.ArgumentOut, Name: "b", TypeOID: 25, Type: "text"}},
		SecurityDefiner: true, Settings: []pgmo.ParameterSetting{{Name: "work_mem", Value: "4MB"}},
		ObjectFile:        "$libdir/mylib",
		Privileges:        []pgmo.PrivilegeGrant{{Grantee: "own", Grantor: "own", Privilege: pgmo.PrivilegeExecute}},
		PrivilegesDefault: true, Comment: "the p",
	}
	gp := *p
	gp.SetDatabase(nil)
	if !reflect.DeepEqual(gp, wantProcedure) {
		t.Errorf("ProcedureByName =\n%+v\nwant\n%+v", gp, wantProcedure)
	}
	wantAggregate := pgmo.Aggregate{
		RoutineIdentity: pgmo.RoutineIdentity{OID: 43001, Schema: "app", Name: "ag", Kind: pgmo.RoutineAggregate,
			ArgumentTypes: []pgmo.OID{701, 2283}, ArgumentTypeNames: []string{"double precision", "anyelement"},
			IdentityArguments: "double precision ORDER BY anyelement"},
		Owner: "own",
		Arguments: []pgmo.RoutineArgument{{Mode: pgmo.ArgumentIn, TypeOID: 701, Type: "double precision"},
			{Mode: pgmo.ArgumentIn, TypeOID: 2283, Type: "anyelement"}},
		ReturnType: "anyelement", Result: "anyelement", Parallel: pgmo.ParallelSafe,
		AggregateKind: pgmo.AggregateOrderedSet, DirectArguments: 1,
		TransitionFunction: pgmo.ObjectName{Schema: "pg_catalog", Name: "ordered_set_transition"},
		StateType:          "internal", StateSpace: 48,
		FinalFunction: pgmo.ObjectName{Schema: "app", Name: "ag_final"}, FinalExtra: true, FinalModify: pgmo.FinalReadWrite,
		CombineFunction:          pgmo.ObjectName{Schema: "app", Name: "ag_combine"},
		SerialFunction:           pgmo.ObjectName{Schema: "app", Name: "ag_ser"},
		DeserialFunction:         pgmo.ObjectName{Schema: "app", Name: "ag_deser"},
		MovingTransitionFunction: pgmo.ObjectName{Schema: "app", Name: "ag_mtrans"},
		MovingInverseFunction:    pgmo.ObjectName{Schema: "app", Name: "ag_minv"},
		MovingStateType:          "app.st", MovingStateSpace: 16,
		MovingFinalFunction: pgmo.ObjectName{Schema: "app", Name: "ag_mfinal"}, MovingFinalModify: pgmo.FinalShareable,
		HasInitialCondition: true, MovingInitialCondition: "{0}", HasMovingInitialCondition: true,
		SortOperator: "OPERATOR(pg_catalog.<)",
		Privileges:   []pgmo.PrivilegeGrant{{Grantee: "B b", Grantor: "own", Privilege: pgmo.PrivilegeExecute}},
		Comment:      "the ag",
	}
	ga := *a
	ga.SetDatabase(nil)
	if !reflect.DeepEqual(ga, wantAggregate) {
		t.Errorf("AggregateByName =\n%+v\nwant\n%+v", ga, wantAggregate)
	}
	for typ, want := range map[reflect.Type]int{
		reflect.TypeFor[pgmo.Function]():        23,
		reflect.TypeFor[pgmo.Procedure]():       13,
		reflect.TypeFor[pgmo.Aggregate]():       34,
		reflect.TypeFor[pgmo.RoutineArgument](): 5,
	} {
		if n := typ.NumField(); n != want {
			t.Errorf("%s has %d fields; this test checks %d — add the new one", typ.Name(), n, want)
		}
	}

	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the routine reads went to appdb", n)
	}
	qs := other.Queries()
	if len(qs) != 3 {
		t.Fatalf("%d statements on appdb, want 3", len(qs))
	}
	for i, c := range []struct {
		name  string
		wants []string
	}{
		{"f", []string{functionMatch, "WHERE q.prokind IN ('f', 'w') AND q.proname = $2::pg_catalog.text",
			"JOIN pg_catalog.pg_language l ON l.oid = p.prolang", "p.procost::double precision, p.prorows::double precision", "p.provolatile::pg_catalog.text, p.proisstrict, p.prosecdef, p.proleakproof,",
			"LEFT JOIN pg_catalog.pg_type rt ON rt.oid = p.prorettype", "pg_catalog.pg_get_function_result(p.oid)",
			"LEFT JOIN pg_catalog.pg_proc sp ON sp.oid = p.prosupport::pg_catalog.oid", "p.prosqlbody IS NOT NULL"}},
		{"p", []string{procedureMatch, "WHERE q.prokind IN ('p') AND q.proname = $2::pg_catalog.text",
			"JOIN pg_catalog.pg_language l ON l.oid = p.prolang"}},
		{"ag", []string{aggregateMatch, "WHERE q.prokind IN ('a') AND q.proname = $2::pg_catalog.text",
			"JOIN pg_catalog.pg_aggregate g ON g.aggfnoid = p.oid", "LEFT JOIN pg_catalog.pg_proc tf ON tf.oid = g.aggtransfn::pg_catalog.oid",
			"LEFT JOIN pg_catalog.pg_type mt ON mt.oid = g.aggmtranstype", "LEFT JOIN pg_catalog.pg_operator so ON so.oid = g.aggsortop",
			"g.agginitval IS NOT NULL"}},
	} {
		q := qs[i]
		// The names are one letter, as prokind's literals are: the schema
		// tells a name written into the text.
		if q.Arg(0) != "app" || q.Arg(1) != c.name || strings.Contains(q.Text, "'app'") {
			t.Errorf("%s: names not bound as $1, $2: args %v, text\n%s", c.name, q.Args, q.Text)
		}
		for _, w := range append(c.wants,
			"AND n.nspname = $1::pg_catalog.text AND p.proname = $2::pg_catalog.text\nORDER BY p.oid",
			"pg_catalog.unnest(COALESCE(q.proallargtypes, q.proargtypes::pg_catalog.oid[])) WITH ORDINALITY AS u(oid, n)",
			"COALESCE(x.modes[x.n::integer]::pg_catalog.text, 'i')",
			"COALESCE(pg_catalog.pg_get_function_arg_default(x.proc, x.n::integer), '')",
			"GROUP BY x.proc) AS ar(proc, modes, names, types, typenames, defaults) ON ar.proc = p.oid",
			"COALESCE(p.proacl, pg_catalog.acldefault('f', p.proowner))", "p.proacl IS NULL",
			"d.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND d.objid = p.oid",
			"ds.classoid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND ds.objsubid = 0",
			"pg_catalog.pg_get_function_identity_arguments(p.oid)") {
			if !strings.Contains(q.Text, w) {
				t.Errorf("%s: no %q in\n%s", c.name, w, q.Text)
			}
		}
	}
	for what, sig := range map[string]string{
		"function": fn.Signature(), "procedure": p.Signature(), "aggregate": a.Signature(),
	} {
		want := map[string]string{"function": "app.f(integer, app.mood[])", "procedure": "app.p(integer)",
			"aggregate": "app.ag(double precision, anyelement)"}[what]
		if sig != want {
			t.Errorf("%s Signature() = %q, want %q", what, sig, want)
		}
	}
}

// The listings push their filter into the query, read every routine's
// arguments of the family's kinds, and order by schema, name, then OID;
// empty arrays and a NULL ACL read as none.
func TestRoutinesFiltered(t *testing.T) {
	yes := true
	filter := pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "AG"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &yes,
		ExtensionMember: &yes,
	}
	empty := func(row []any) []any {
		r := append([]any(nil), row...)
		r[4], r[5] = []uint32{}, ""
		for i := 9; i < 14; i++ { // no arguments: the LEFT JOIN found none
			r[i] = nil
		}
		for i := len(r) - 5; i < len(r)-1; i++ { // an empty ACL
			r[i] = nil
		}
		return r
	}
	for _, c := range []struct {
		kind, match, kinds string
		types              []uint32
		row                []any
		list               func(context.Context, *pgmo.Database) (bool, error)
	}{
		{"function", functionMatch, "'f', 'w'", functionTypes, empty(rowFunction),
			func(ctx context.Context, d *pgmo.Database) (bool, error) {
				fs, err := d.Functions(ctx, filter)
				return err == nil && len(fs) == 1 && fs[0].Arguments == nil && fs[0].ArgumentTypeNames != nil &&
					len(fs[0].ArgumentTypeNames) == 0 && fs[0].Privileges == nil, err
			}},
		{"procedure", procedureMatch, "'p'", procedureTypes, empty(rowProcedure),
			func(ctx context.Context, d *pgmo.Database) (bool, error) {
				ps, err := d.Procedures(ctx, filter)
				return err == nil && len(ps) == 1 && ps[0].Arguments == nil && ps[0].Privileges == nil, err
			}},
		{"aggregate", aggregateMatch, "'a'", aggregateTypes, empty(rowAggregate),
			func(ctx context.Context, d *pgmo.Database) (bool, error) {
				as, err := d.Aggregates(ctx, filter)
				return err == nil && len(as) == 1 && as[0].Arguments == nil && as[0].Privileges == nil, err
			}},
	} {
		f := pgmotest.New(t)
		f.On(c.match, rowsOf(c.types, c.row))
		d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
		if ok, err := c.list(t.Context(), d); !ok {
			t.Fatalf("%s: listing wrong: %v", c.kind, err)
		}
		last := lastQuery(f)
		tail := c.match + " AND pg_catalog.strpos(p.proname, $1) > 0 AND n.nspname = $2::pg_catalog.text" +
			" AND (pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema') AND EXISTS ("
		if !strings.Contains(last.Text, tail) || !strings.HasSuffix(last.Text, "\nORDER BY n.nspname, p.proname, p.oid") ||
			!strings.Contains(last.Text, "WHERE d.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND d.objid = p.oid") ||
			!strings.Contains(last.Text, "WHERE q.prokind IN ("+c.kinds+")\n") || strings.Contains(last.Text, "q.proname") ||
			len(last.Args) != 2 || last.Arg(0) != "ag" || last.Arg(1) != "app" {
			t.Errorf("%s: filter not pushed down, then ordered: args %v\n%s", c.kind, last.Args, last.Text)
		}
		n := len(f.Queries())
		bad := pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}
		if _, err := d.Functions(t.Context(), bad); !errors.Is(err, pgmo.ErrInvalidRequest) {
			t.Errorf("Functions, unknown operator: err = %v, want ErrInvalidRequest", err)
		}
		if _, err := d.Procedures(t.Context(), bad); !errors.Is(err, pgmo.ErrInvalidRequest) {
			t.Errorf("Procedures, unknown operator: err = %v, want ErrInvalidRequest", err)
		}
		if _, err := d.Aggregates(t.Context(), bad); !errors.Is(err, pgmo.ErrInvalidRequest) {
			t.Errorf("Aggregates, unknown operator: err = %v, want ErrInvalidRequest", err)
		}
		if len(f.Queries()) != n {
			t.Errorf("a refused filter was sent")
		}
	}
}

// byNameRead is one family's by-name read, its error alone.
type byNameRead func(context.Context, *pgmo.Database, string, string, []pgmo.OID) error

// The three by-name reads, by family name.
var byNameReads = []struct {
	what, match string
	types       []uint32
	row         []any
	read        byNameRead
	list        func(context.Context, *pgmo.Database) error
}{
	{"function", functionMatch, functionTypes, rowFunction,
		func(ctx context.Context, d *pgmo.Database, s, n string, args []pgmo.OID) error {
			_, err := d.FunctionByName(ctx, s, n, args)
			return err
		},
		func(ctx context.Context, d *pgmo.Database) error {
			_, err := d.Functions(ctx, pgmo.ObjectFilter{})
			return err
		}},
	{"procedure", procedureMatch, procedureTypes, rowProcedure,
		func(ctx context.Context, d *pgmo.Database, s, n string, args []pgmo.OID) error {
			_, err := d.ProcedureByName(ctx, s, n, args)
			return err
		},
		func(ctx context.Context, d *pgmo.Database) error {
			_, err := d.Procedures(ctx, pgmo.ObjectFilter{})
			return err
		}},
	{"aggregate", aggregateMatch, aggregateTypes, rowAggregate,
		func(ctx context.Context, d *pgmo.Database, s, n string, args []pgmo.OID) error {
			_, err := d.AggregateByName(ctx, s, n, args)
			return err
		},
		func(ctx context.Context, d *pgmo.Database) error {
			_, err := d.Aggregates(ctx, pgmo.ObjectFilter{})
			return err
		}},
}

// overload is row as another overload: a new OID, and its first input
// argument's type changed to text.
func overload(row []any) []any {
	r := append([]any(nil), row...)
	r[0] = r[0].(uint32) + 1
	types := append([]uint32(nil), r[4].([]uint32)...)
	types[0] = 25
	r[4] = types
	all := append([]uint32(nil), r[11].([]uint32)...)
	names := append([]string(nil), r[12].([]string)...)
	all[0], names[0] = 25, "text"
	r[11], r[12] = all, names
	return r
}

// With no argument types an overloaded name is ErrAmbiguous naming each
// Signature; argument types pick one or find none; no schema is refused
// before anything is sent; no row is ErrNotFound; a server error is
// wrapped once, naming the read.
func TestRoutineFamilyOverloads(t *testing.T) {
	for _, c := range byNameReads {
		f := pgmotest.New(t)
		f.On(c.match, rowsOf(c.types, c.row, overload(c.row)))
		d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
		name := c.row[2].(string)
		err := c.read(t.Context(), d, "app", name, nil)
		if !errors.Is(err, pgmo.ErrAmbiguous) || !strings.HasPrefix(err.Error(), "pgmo: read "+c.what+" app."+name+" of database ") ||
			!strings.Contains(err.Error(), "2 overloads (app."+name+"(") || !strings.Contains(err.Error(), "; app."+name+"(text") ||
			strings.Count(err.Error(), "pgmo: ") != 1 {
			t.Errorf("%s, overloaded: err = %v, want ErrAmbiguous naming both", c.what, err)
		}
		ids := c.row[4].([]uint32)
		args := make([]pgmo.OID, len(ids))
		for i, id := range ids {
			args[i] = pgmo.OID(id)
		}
		if err := c.read(t.Context(), d, "app", name, args); err != nil {
			t.Errorf("%s, exact argument types: %v", c.what, err)
		}
		err = c.read(t.Context(), d, "app", name, []pgmo.OID{})
		if !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), "no "+c.what+" with argument types []") {
			t.Errorf("%s, no such overload: err = %v, want ErrNotFound", c.what, err)
		}

		f = pgmotest.New(t)
		f.On(c.match, rowsOf(c.types))
		d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
		n := len(f.Queries())
		if err := c.read(t.Context(), d, "", "r", nil); !errors.Is(err, pgmo.ErrSchemaRequired) ||
			!strings.HasPrefix(err.Error(), "pgmo: read "+c.what+` "r"`) {
			t.Errorf("%s, no schema: err = %v, want ErrSchemaRequired", c.what, err)
		}
		if len(f.Queries()) != n {
			t.Errorf("%s: a schema-less read was sent", c.what)
		}
		err = c.read(t.Context(), d, "app", "gone", nil)
		if !errors.Is(err, pgmo.ErrNotFound) || err.Error() != "pgmo: read "+c.what+` app.gone of database "`+pgmotest.DefaultDatabase+`": no such `+c.what {
			t.Errorf("%s missing: err = %v, want ErrNotFound naming it", c.what, err)
		}

		f = pgmotest.New(t)
		f.On(c.match, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
		err = c.read(t.Context(), f.Server().DatabaseRef(pgmotest.DefaultDatabase), "app", "r", nil)
		if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read "+c.what+" app.r of database ") ||
			strings.Count(err.Error(), "pgmo: ") != 1 {
			t.Errorf("%s, server error: err = %v, want it wrapped once naming the read", c.what, err)
		}
	}
}

// A malformed row — ACL or argument arrays of different lengths, or input
// arguments that are not proargtypes — fails the read naming it.
func TestRoutineRowsMalformed(t *testing.T) {
	for _, c := range byNameReads {
		for name, change := range map[string]func([]any){
			"ACL short":       func(r []any) { r[len(r)-2] = r[len(r)-2].([]bool)[:0] },
			"defaults short":  func(r []any) { r[13] = r[13].([]string)[:1] },
			"inputs not keys": func(r []any) { r[4] = []uint32{99} },
		} {
			row := append([]any(nil), c.row...)
			change(row)
			f := pgmotest.New(t)
			f.On(c.match, rowsOf(c.types, row))
			err := c.list(t.Context(), f.Server().DatabaseRef(pgmotest.DefaultDatabase))
			want := map[string]string{"ACL short": "privilege arrays of different lengths",
				"defaults short": "argument arrays of different lengths", "inputs not keys": "not proargtypes' [99]"}[name]
			if err == nil || !strings.HasPrefix(err.Error(), "pgmo: list "+c.what+"s of database ") || !strings.Contains(err.Error(), want) {
				t.Errorf("%s, %s: err = %v, want %q wrapped naming the read", c.what, name, err, want)
			}
		}
	}
}

// A Ref reads nothing and holds the names, kind and argument type names
// alone; Body refuses it before sending anything.
func TestRoutineRefs(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef("appdb")
	n := len(f.Queries())
	fn := d.FunctionRef("app", "f", []string{"integer", "app.mood[]"})
	p := d.ProcedureRef("app", "p", []string{"integer"})
	a := d.AggregateRef("app", "ag", nil)
	gf, gp, ga := *fn, *p, *a
	gf.SetDatabase(nil)
	gp.SetDatabase(nil)
	ga.SetDatabase(nil)
	if fn.Database() != d || !reflect.DeepEqual(gf, pgmo.Function{RoutineIdentity: pgmo.RoutineIdentity{Schema: "app", Name: "f",
		Kind: pgmo.RoutineFunction, ArgumentTypeNames: []string{"integer", "app.mood[]"}}}) {
		t.Errorf("FunctionRef = %+v", fn)
	}
	if p.Database() != d || !reflect.DeepEqual(gp, pgmo.Procedure{RoutineIdentity: pgmo.RoutineIdentity{Schema: "app", Name: "p",
		Kind: pgmo.RoutineProcedure, ArgumentTypeNames: []string{"integer"}}}) {
		t.Errorf("ProcedureRef = %+v", p)
	}
	if a.Database() != d || !reflect.DeepEqual(ga, pgmo.Aggregate{RoutineIdentity: pgmo.RoutineIdentity{Schema: "app", Name: "ag",
		Kind: pgmo.RoutineAggregate}}) {
		t.Errorf("AggregateRef = %+v", a)
	}
	if fn.Signature() != "app.f(integer, app.mood[])" || a.Signature() != "app.ag()" {
		t.Errorf("Signature %q, %q", fn.Signature(), a.Signature())
	}
	for name, read := range map[string]func() error{
		"Function.Body":  func() error { _, err := fn.Body(t.Context()); return err },
		"Procedure.Body": func() error { _, err := p.Body(t.Context()); return err },
	} {
		want := map[string]string{"Function.Body": "pgmo: read body of function app.f(integer, app.mood[]): ",
			"Procedure.Body": "pgmo: read body of procedure app.p(integer): "}[name]
		if err := read(); !errors.Is(err, pgmo.ErrHandleNotLoaded) || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s on a Ref: err = %v, want ErrHandleNotLoaded as %q", name, err, want)
		}
	}
	if len(f.Queries()) != n || f.Database("appdb").Opens() != 0 {
		t.Errorf("a Ref read sent a statement")
	}
}

// Body reads the source and definition by the OID, bound as $1, and the
// family's kinds, bound as $2, from the routine's database; a routine
// dropped since is ErrNotFound; a server error is wrapped once.
func TestRoutineBody(t *testing.T) {
	_, other, fn, p, _ := loadedRoutines(t, map[string]pgmotest.Responder{
		bodyMatch: rowsOf(routineBodyTypes, []any{"cron_alter_job",
			"CREATE OR REPLACE FUNCTION app.f()\n LANGUAGE c\nAS '$libdir/pg_cron', $function$cron_alter_job$function$\n"}),
	})
	for what, c := range map[string]struct {
		body  func() (pgmo.RoutineBody, error)
		oid   pgmo.OID
		kinds []string
	}{
		"function":  {func() (pgmo.RoutineBody, error) { return fn.Body(t.Context()) }, 41001, []string{"f", "w"}},
		"procedure": {func() (pgmo.RoutineBody, error) { return p.Body(t.Context()) }, 42001, []string{"p"}},
	} {
		b, err := c.body()
		if err != nil || b.Source != "cron_alter_job" || !strings.Contains(b.Definition, "AS '$libdir/pg_cron', $function$cron_alter_job") {
			t.Errorf("%s Body = %+v, %v", what, b, err)
		}
		last := lastQuery(other)
		if !strings.Contains(last.Text, bodyMatch) || len(last.Args) != 2 || last.Arg(0) != c.oid ||
			!reflect.DeepEqual(last.Arg(1), c.kinds) {
			t.Errorf("%s: not keyed by OID $1 and kinds $2: args %v (%T, %T)\n%s", what, last.Args, last.Arg(0), last.Arg(1), last.Text)
		}
	}

	_, _, fn, p, _ = loadedRoutines(t, map[string]pgmotest.Responder{bodyMatch: rowsOf(routineBodyTypes)})
	if _, err := fn.Body(t.Context()); !errors.Is(err, pgmo.ErrNotFound) ||
		err.Error() != `pgmo: function app.f(integer, app.mood[]) not found in database "appdb"` {
		t.Errorf("function dropped: err = %v, want ErrNotFound", err)
	}
	if _, err := p.Body(t.Context()); !errors.Is(err, pgmo.ErrNotFound) {
		t.Errorf("procedure dropped: err = %v, want ErrNotFound", err)
	}

	_, _, fn, _, _ = loadedRoutines(t, map[string]pgmotest.Responder{
		bodyMatch: pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"})})
	if _, err := fn.Body(t.Context()); err == nil || errors.Is(err, pgmo.ErrNotFound) ||
		!strings.HasPrefix(err.Error(), "pgmo: read body of function app.f(integer, app.mood[]): ") || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
}

// Database.RoutineByName reads through the database's own pool and
// prefixes its error once.
func TestDatabaseRoutineByName(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On("FROM pg_catalog.pg_proc", routineRows(rowFInt, rowFTy))
	d := f.Server().DatabaseRef("appdb")
	r, err := d.RoutineByName(t.Context(), "idt", "f", []pgmo.OID{23})
	if err != nil || r.OID != 17271 || r.Signature() != "idt.f(integer)" {
		t.Errorf("RoutineByName = %+v, %v", r, err)
	}
	if n := len(f.Queries()); n != 1 || len(other.Queries()) != 1 {
		t.Errorf("%d statements on the Server's own database, %d on appdb; want the read on appdb", n, len(other.Queries()))
	}
	_, err = d.RoutineByName(t.Context(), "idt", "f", nil)
	if !errors.Is(err, pgmo.ErrAmbiguous) || !strings.HasPrefix(err.Error(), "pgmo: read routine idt.f: 2 overloads (") ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("overloaded: err = %v, want ErrAmbiguous prefixed once", err)
	}
	if _, err := d.RoutineByName(t.Context(), "", "f", nil); !errors.Is(err, pgmo.ErrSchemaRequired) ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("no schema: err = %v, want ErrSchemaRequired prefixed once", err)
	}
}

// String spells each code as the DDL does; an unknown one is shown as
// itself. IsInput holds for the modes a call passes.
func TestRoutineCodeStrings(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{pgmo.ArgumentIn.String(), "IN"}, {pgmo.ArgumentOut.String(), "OUT"}, {pgmo.ArgumentInOut.String(), "INOUT"},
		{pgmo.ArgumentVariadic.String(), "VARIADIC"}, {pgmo.ArgumentTable.String(), "TABLE"},
		{pgmo.ArgumentMode("x").String(), `ArgumentMode("x")`},
		{pgmo.VolatilityImmutable.String(), "IMMUTABLE"}, {pgmo.VolatilityStable.String(), "STABLE"},
		{pgmo.VolatilityVolatile.String(), "VOLATILE"}, {pgmo.Volatility("x").String(), `Volatility("x")`},
		{pgmo.ParallelSafe.String(), "SAFE"}, {pgmo.ParallelRestricted.String(), "RESTRICTED"},
		{pgmo.ParallelUnsafe.String(), "UNSAFE"}, {pgmo.ParallelSafety("x").String(), `ParallelSafety("x")`},
		{pgmo.AggregateNormal.String(), "normal"}, {pgmo.AggregateOrderedSet.String(), "ordered-set"},
		{pgmo.AggregateHypothetical.String(), "hypothetical-set"}, {pgmo.AggregateKind("x").String(), `AggregateKind("x")`},
		{pgmo.FinalReadOnly.String(), "READ_ONLY"}, {pgmo.FinalShareable.String(), "SHAREABLE"},
		{pgmo.FinalReadWrite.String(), "READ_WRITE"}, {pgmo.FinalModify("x").String(), `FinalModify("x")`},
	} {
		if c.got != c.want {
			t.Errorf("String() = %q, want %q", c.got, c.want)
		}
	}
	for m, want := range map[pgmo.ArgumentMode]bool{pgmo.ArgumentIn: true, pgmo.ArgumentInOut: true, pgmo.ArgumentVariadic: true,
		pgmo.ArgumentOut: false, pgmo.ArgumentTable: false, pgmo.ArgumentMode("x"): false} {
		if m.IsInput() != want {
			t.Errorf("%v.IsInput() = %v, want %v", m, !want, want)
		}
	}
}
