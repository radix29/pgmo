//go:build livedb

package pgmo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/radix29/pgmo/pgsql"
)

// TestLiveRoutines reads the functions, procedures and aggregates of a
// throwaway database the listings, the by-name reads and the bodies must
// describe in full:
//
//   - app.f, three overloads: f(a integer, b text DEFAULT, VARIADIC c
//     app.mood[] DEFAULT, OUT d app."Odd T"), STABLE STRICT LEAKPROOF;
//     f(x integer), granted WITH GRANT OPTION and revoked from PUBLIC, with
//     a comment; app."F"(integer), differing by case only;
//   - app.g: RETURNS TABLE over app.mood, a SQL-standard body, IMMUTABLE
//     SECURITY DEFINER PARALLEL SAFE COST ROWS, SET search_path and
//     work_mem; app.h: SETOF, LEAKPROOF but not STRICT, with a planner
//     SUPPORT function;
//   - app.citext_eq: a C function of the citext extension;
//   - app.p(INOUT, OUT) in PL/pgSQL, SECURITY DEFINER, SET; app.p2() with
//     a BEGIN ATOMIC body;
//   - app.ag(*); app.msum(integer): final, combine and moving-aggregate
//     functions, initial conditions, a final-function mode; app.mymax with
//     a sort operator; app.os, ordered-set; pg_catalog.rank, hypothetical.
//
// Each by-name read refuses the other families and picks overloads by
// argument types. The structured type names hold under a search_path that
// finds the types, where the rendered ones do not. Every routine of every
// listing has the type names identity.go's own rule gives. The listings and
// bodies are read again as a plain role: identical.
func TestLiveRoutines(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	name := func(s string) string { return fmt.Sprintf("pgmo_live_routine_%d_%s", stamp, s) }
	b, plain := name("B b"), name("plain")
	db := name("db")
	q := pgsql.QuoteIdent

	var connected string
	if err := admin.QueryRow(ctx, "SELECT current_user::text").Scan(&connected); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+q(db)+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
		for _, r := range []string{b, plain} {
			if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+q(r)); err != nil {
				t.Errorf("drop role %s: %v", r, err)
			}
		}
	})
	for _, stmt := range []string{"CREATE DATABASE " + q(db), "CREATE ROLE " + q(b), "CREATE ROLE " + q(plain)} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cfg, err := pgx.ParseConfig(*liveConnString)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	for _, stmt := range []string{
		"CREATE SCHEMA app",
		"CREATE EXTENSION citext SCHEMA app",
		"CREATE TYPE app.mood AS ENUM ('a', 'b')",
		`CREATE TYPE app."Odd T" AS (x integer)`,
		`CREATE FUNCTION app.f(a integer, b text DEFAULT 'x', VARIADIC c app.mood[] DEFAULT '{a}', OUT d app."Odd T")
			LANGUAGE sql STABLE STRICT LEAKPROOF AS 'SELECT ROW(a)::app."Odd T"'`,
		"CREATE FUNCTION app.f(x integer) RETURNS integer LANGUAGE sql AS 'SELECT x'",
		"REVOKE EXECUTE ON FUNCTION app.f(integer) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION app.f(integer) TO " + q(b) + " WITH GRANT OPTION",
		"COMMENT ON FUNCTION app.f(integer) IS 'one'",
		`CREATE FUNCTION app."F"(integer) RETURNS integer LANGUAGE sql AS 'SELECT 2'`,
		`CREATE FUNCTION app.g(x integer) RETURNS TABLE (p integer, q app.mood) LANGUAGE sql
			IMMUTABLE SECURITY DEFINER PARALLEL SAFE COST 7 ROWS 3
			SET search_path = app, pg_temp SET work_mem = '8MB'
			BEGIN ATOMIC SELECT x, 'a'::app.mood; END`,
		"CREATE FUNCTION app.h(n integer) RETURNS SETOF integer LANGUAGE sql LEAKPROOF ROWS 40 " +
			"SUPPORT pg_catalog.generate_series_int4_support AS 'SELECT pg_catalog.generate_series(1, n)'",
		`CREATE PROCEDURE app.p(INOUT a integer, OUT b text) LANGUAGE plpgsql SECURITY DEFINER SET work_mem = '4MB'
			AS $$BEGIN b := 'x'; END$$`,
		"CREATE PROCEDURE app.p2() LANGUAGE sql BEGIN ATOMIC SELECT 1; END",
		"COMMENT ON PROCEDURE app.p2() IS 'atomic'",
		"CREATE FUNCTION app.add(bigint, integer) RETURNS bigint LANGUAGE sql IMMUTABLE AS 'SELECT $1 + $2'",
		"CREATE FUNCTION app.sub(bigint, integer) RETURNS bigint LANGUAGE sql IMMUTABLE AS 'SELECT $1 - $2'",
		"CREATE FUNCTION app.fin(bigint) RETURNS numeric LANGUAGE sql IMMUTABLE AS 'SELECT $1::numeric'",
		"CREATE AGGREGATE app.ag(*) (sfunc = pg_catalog.int8inc, stype = bigint, initcond = '0')",
		`CREATE AGGREGATE app.msum(integer) (sfunc = app.add, stype = bigint, initcond = '0',
			finalfunc = app.fin, finalfunc_modify = shareable, combinefunc = pg_catalog.int8pl, parallel = safe,
			msfunc = app.add, minvfunc = app.sub, mstype = bigint, msspace = 16, minitcond = '0', mfinalfunc = app.fin)`,
		"CREATE AGGREGATE app.mymax(integer) (sfunc = pg_catalog.int4larger, stype = integer, sortop = >)",
		"CREATE AGGREGATE app.os(double precision ORDER BY anyelement) (sfunc = pg_catalog.ordered_set_transition, " +
			"stype = internal, finalfunc = pg_catalog.percentile_disc_final, finalfunc_extra)",
		"GRANT EXECUTE ON FUNCTION app.msum(integer) TO " + q(b),
		"COMMENT ON AGGREGATE app.msum(integer) IS 'moving'",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	var moodOID, oddOID, moodArrayOID OID
	if err := conn.QueryRow(ctx, `SELECT 'app.mood'::regtype::oid, 'app."Odd T"'::regtype::oid, 'app.mood[]'::regtype::oid`).
		Scan(&moodOID, &oddOID, &moodArrayOID); err != nil {
		t.Fatal(err)
	}

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: db, ApplicationName: "pgmo - live routines"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	d, err := s.DatabaseByName(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	app := ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}}
	no, yes := false, true

	// The user functions no extension owns, by name (byte order) then OID.
	fs, err := d.Functions(ctx, ObjectFilter{Schema: app.Schema, ExtensionMember: &no})
	if err != nil {
		t.Fatal(err)
	}
	var sigs []string
	for _, f := range fs {
		sigs = append(sigs, f.Signature())
	}
	if want := []string{`app."F"(integer)`, "app.add(bigint, integer)", "app.f(integer, text, app.mood[])", "app.f(integer)",
		"app.fin(bigint)", "app.g(integer)", "app.h(integer)", "app.sub(bigint, integer)"}; !slices.Equal(sigs, want) {
		t.Fatalf("Functions(app, no extension) = %q, want %q", sigs, want)
	}
	function := func(n string, args ...OID) *Function {
		t.Helper()
		f, err := d.FunctionByName(ctx, "app", n, args)
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.IndexFunc(fs, func(l *Function) bool { return l.OID == f.OID }); i >= 0 && !reflect.DeepEqual(fs[i], f) {
			t.Errorf("listing's %s =\n%+v\nby name\n%+v", f.Signature(), fs[i], f)
		}
		return f
	}

	f3 := function("f", 23, 25, moodArrayOID)
	if f3.Kind != RoutineFunction || f3.Owner != connected || f3.Language != "sql" || f3.ReturnType != `app."Odd T"` ||
		f3.ReturnsSet || f3.Result != `app."Odd T"` || f3.Volatility != VolatilityStable || !f3.Strict || !f3.Leakproof ||
		f3.SecurityDefiner || f3.Parallel != ParallelUnsafe || f3.Cost != 100 || f3.Rows != 0 || f3.Settings != nil ||
		f3.SupportFunction != (ObjectName{}) || f3.ObjectFile != "" || f3.SQLStandardBody || !f3.PrivilegesDefault ||
		f3.Comment != "" || f3.Origin != (Origin{}) {
		t.Errorf("app.f(integer, text, app.mood[]) = %+v", f3)
	}
	if want := []RoutineArgument{
		{ArgumentIn, "a", 23, "integer", ""},
		{ArgumentIn, "b", 25, "text", "'x'::text"},
		{ArgumentVariadic, "c", moodArrayOID, "app.mood[]", "'{a}'::app.mood[]"},
		{ArgumentOut, "d", oddOID, `app."Odd T"`, ""},
	}; !reflect.DeepEqual(f3.Arguments, want) {
		t.Errorf("app.f arguments =\n%+v\nwant\n%+v", f3.Arguments, want)
	}
	if !slices.ContainsFunc(f3.Privileges, func(g PrivilegeGrant) bool { return g.Grantee == "" && g.Privilege == PrivilegeExecute }) {
		t.Errorf("app.f(…) privileges %+v: want the default EXECUTE to PUBLIC", f3.Privileges)
	}
	f1 := function("f", 23)
	if f1.Comment != "one" || f1.PrivilegesDefault || f1.Volatility != VolatilityVolatile ||
		!reflect.DeepEqual(f1.Arguments, []RoutineArgument{{ArgumentIn, "x", 23, "integer", ""}}) ||
		slices.ContainsFunc(f1.Privileges, func(g PrivilegeGrant) bool { return g.Grantee == "" }) ||
		!slices.Contains(f1.Privileges, PrivilegeGrant{Grantee: b, Grantor: connected, Privilege: PrivilegeExecute, Grantable: true}) {
		t.Errorf("app.f(integer) = %+v: want commented, granted to %s WITH GRANT OPTION, not to PUBLIC", f1, b)
	}
	if upper := function("F"); upper.OID == f1.OID || upper.Name != "F" {
		t.Errorf(`app."F" = %+v: want its own function`, upper)
	}
	g := function("g")
	if !g.ReturnsSet || g.ReturnType != "record" || g.Result != "TABLE(p integer, q app.mood)" || g.Volatility != VolatilityImmutable ||
		!g.SecurityDefiner || g.Parallel != ParallelSafe || g.Cost != 7 || g.Rows != 3 || !g.SQLStandardBody ||
		!reflect.DeepEqual(g.Settings, []ParameterSetting{{"search_path", "app, pg_temp"}, {"work_mem", "8MB"}}) ||
		!reflect.DeepEqual(g.Arguments, []RoutineArgument{{ArgumentIn, "x", 23, "integer", ""},
			{ArgumentTable, "p", 23, "integer", ""}, {ArgumentTable, "q", moodOID, "app.mood", ""}}) {
		t.Errorf("app.g = %+v", g)
	}
	h := function("h")
	if !h.ReturnsSet || h.ReturnType != "integer" || h.Rows != 40 || !h.Leakproof || h.Strict ||
		h.SupportFunction != (ObjectName{"pg_catalog", "generate_series_int4_support"}) {
		t.Errorf("app.h = %+v: want SETOF integer, LEAKPROOF not STRICT, ROWS 40, its SUPPORT function", h)
	}

	// The extension's C function, and a window function of pg_catalog.
	eq, err := d.FunctionByName(ctx, "app", "citext_eq", nil)
	if err != nil {
		t.Fatal(err)
	}
	if eq.Language != "c" || eq.ObjectFile != "$libdir/citext" || eq.Origin != (Origin{Extension: "citext"}) ||
		!slices.Equal(eq.ArgumentTypeNames, []string{"app.citext", "app.citext"}) {
		t.Errorf("app.citext_eq = %+v: want citext's C function", eq)
	}
	members, err := d.Functions(ctx, ObjectFilter{Schema: app.Schema, ExtensionMember: &yes})
	if err != nil || !slices.ContainsFunc(members, func(f *Function) bool { return f.OID == eq.OID }) ||
		slices.ContainsFunc(members, func(f *Function) bool { return f.Extension != "citext" }) {
		t.Errorf("Functions(app, extension members) = %d, %v; want citext's alone, citext_eq among them", len(members), err)
	}
	rn, err := d.FunctionByName(ctx, "pg_catalog", "row_number", nil)
	if err != nil || rn.Kind != RoutineWindow || !rn.IsSystem || rn.Language != "internal" || rn.ReturnType != "bigint" {
		t.Errorf("pg_catalog.row_number = %+v, %v; want a system window function", rn, err)
	}

	// Bodies: as written, C's link symbol, a SQL-standard one in the
	// definition alone.
	for _, c := range []struct {
		f             *Function
		source, inDef string
	}{
		{f1, "SELECT x", "AS $function$SELECT x$function$"},
		{g, "", "BEGIN ATOMIC"},
		{eq, "citext_eq", "AS '$libdir/citext', $function$citext_eq$function$"},
	} {
		body, err := c.f.Body(ctx)
		if err != nil || body.Source != c.source || !strings.HasPrefix(body.Definition, "CREATE OR REPLACE FUNCTION ") ||
			!strings.Contains(body.Definition, c.inDef) {
			t.Errorf("%s body = %+v, %v; want source %q, %q in the definition", c.f.Signature(), body, err, c.source, c.inDef)
		}
	}

	// Procedures.
	ps, err := d.Procedures(ctx, app)
	if err != nil || len(ps) != 2 || ps[0].Name != "p" || ps[1].Name != "p2" {
		t.Fatalf("Procedures(app) = %+v, %v; want p, p2", ps, err)
	}
	p, err := d.ProcedureByName(ctx, "app", "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, ps[0]) || p.Kind != RoutineProcedure || p.Language != "plpgsql" || !p.SecurityDefiner ||
		p.Signature() != "app.p(integer)" || !reflect.DeepEqual(p.Settings, []ParameterSetting{{"work_mem", "4MB"}}) ||
		!reflect.DeepEqual(p.Arguments, []RoutineArgument{{ArgumentInOut, "a", 23, "integer", ""}, {ArgumentOut, "b", 25, "text", ""}}) {
		t.Errorf("app.p = %+v", p)
	}
	p2 := ps[1]
	if !p2.SQLStandardBody || p2.Arguments != nil || p2.Comment != "atomic" || p2.Signature() != "app.p2()" {
		t.Errorf("app.p2 = %+v", p2)
	}
	if body, err := p.Body(ctx); err != nil || body.Source != "BEGIN b := 'x'; END" ||
		!strings.HasPrefix(body.Definition, "CREATE OR REPLACE PROCEDURE app.p(INOUT a integer, OUT b text)") {
		t.Errorf("app.p body = %+v, %v", body, err)
	}

	// Aggregates.
	as, err := d.Aggregates(ctx, ObjectFilter{Schema: app.Schema, ExtensionMember: &no})
	if err != nil {
		t.Fatal(err)
	}
	var aggs []string
	for _, a := range as {
		aggs = append(aggs, a.Signature())
	}
	if want := []string{"app.ag()", "app.msum(integer)", "app.mymax(integer)", "app.os(double precision, anyelement)"}; !slices.Equal(aggs, want) {
		t.Fatalf("Aggregates(app, no extension) = %q, want %q", aggs, want)
	}
	aggregate := func(schema, n string, args []OID) *Aggregate {
		t.Helper()
		a, err := d.AggregateByName(ctx, schema, n, args)
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.IndexFunc(as, func(l *Aggregate) bool { return l.OID == a.OID }); i >= 0 && !reflect.DeepEqual(as[i], a) {
			t.Errorf("listing's %s =\n%+v\nby name\n%+v", a.Signature(), as[i], a)
		}
		return a
	}
	if ag := aggregate("app", "ag", []OID{}); ag.Arguments != nil || ag.TransitionFunction != (ObjectName{"pg_catalog", "int8inc"}) ||
		ag.StateType != "bigint" || ag.InitialCondition != "0" || !ag.HasInitialCondition || ag.AggregateKind != AggregateNormal ||
		ag.ReturnType != "bigint" || ag.FinalFunction != (ObjectName{}) || ag.HasMovingInitialCondition || ag.SortOperator != "" {
		t.Errorf("app.ag(*) = %+v", ag)
	}
	ms := aggregate("app", "msum", nil)
	if ms.TransitionFunction != (ObjectName{"app", "add"}) || ms.FinalFunction != (ObjectName{"app", "fin"}) ||
		ms.FinalModify != FinalShareable || ms.CombineFunction != (ObjectName{"pg_catalog", "int8pl"}) || ms.Parallel != ParallelSafe ||
		ms.MovingTransitionFunction != (ObjectName{"app", "add"}) || ms.MovingInverseFunction != (ObjectName{"app", "sub"}) ||
		ms.MovingStateType != "bigint" || ms.MovingStateSpace != 16 || ms.MovingFinalFunction != (ObjectName{"app", "fin"}) ||
		ms.MovingInitialCondition != "0" || !ms.HasMovingInitialCondition || ms.ReturnType != "numeric" || ms.Comment != "moving" ||
		ms.PrivilegesDefault || !slices.ContainsFunc(ms.Privileges, func(g PrivilegeGrant) bool { return g.Grantee == b }) {
		t.Errorf("app.msum = %+v", ms)
	}
	if mx := aggregate("app", "mymax", nil); mx.SortOperator != "OPERATOR(pg_catalog.>)" || mx.MovingTransitionFunction != (ObjectName{}) ||
		mx.MovingStateType != "" || mx.HasInitialCondition {
		t.Errorf("app.mymax = %+v: want SORTOP >, no moving implementation, no initial condition", mx)
	}
	if os := aggregate("app", "os", nil); os.AggregateKind != AggregateOrderedSet || os.DirectArguments != 1 || !os.FinalExtra ||
		os.StateType != "internal" || os.FinalFunction != (ObjectName{"pg_catalog", "percentile_disc_final"}) {
		t.Errorf("app.os = %+v: want ordered-set with one direct argument", os)
	}
	if rank := aggregate("pg_catalog", "rank", nil); rank.AggregateKind != AggregateHypothetical || !rank.IsSystem {
		t.Errorf("pg_catalog.rank = %+v: want the hypothetical-set aggregate (the window function is not an aggregate)", rank)
	}

	// Each by-name read finds its own family alone, and picks overloads.
	for _, c := range []struct {
		what string
		read func() error
		want error
	}{
		{"FunctionByName(f)", func() error { _, err := d.FunctionByName(ctx, "app", "f", nil); return err }, ErrAmbiguous},
		{"FunctionByName(f, text)", func() error { _, err := d.FunctionByName(ctx, "app", "f", []OID{25}); return err }, ErrNotFound},
		{"FunctionByName(a procedure)", func() error { _, err := d.FunctionByName(ctx, "app", "p", nil); return err }, ErrNotFound},
		{"FunctionByName(an aggregate)", func() error { _, err := d.FunctionByName(ctx, "app", "ag", nil); return err }, ErrNotFound},
		{"ProcedureByName(a function)", func() error { _, err := d.ProcedureByName(ctx, "app", "g", nil); return err }, ErrNotFound},
		{"AggregateByName(a function)", func() error { _, err := d.AggregateByName(ctx, "app", "g", nil); return err }, ErrNotFound},
		{"FunctionByName(g, folded schema)", func() error { _, err := d.FunctionByName(ctx, "APP", "g", nil); return err }, ErrNotFound},
	} {
		if err := c.read(); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.what, err, c.want)
		}
	}
	for n, kind := range map[string]RoutineKind{"g": RoutineFunction, "p": RoutineProcedure, "msum": RoutineAggregate} {
		if r, err := d.RoutineByName(ctx, "app", n, nil); err != nil || r.Kind != kind {
			t.Errorf("RoutineByName(app.%s) = %+v, %v; want kind %v", n, r, err, kind)
		}
	}
	fref, pref := d.FunctionRef("app", "g", []string{"integer"}), d.ProcedureRef("app", "p", []string{"integer"})
	if _, err := fref.Body(ctx); !errors.Is(err, ErrHandleNotLoaded) {
		t.Errorf("FunctionRef.Body: err = %v, want ErrHandleNotLoaded", err)
	}
	if _, err := pref.Body(ctx); !errors.Is(err, ErrHandleNotLoaded) {
		t.Errorf("ProcedureRef.Body: err = %v, want ErrHandleNotLoaded", err)
	}

	// Under a search_path finding app's types, the structured names stay
	// qualified and the rendered ones do not.
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.on(ctx).Exec(ctx, "SET LOCAL search_path = app, pg_catalog"); err != nil {
			return err
		}
		f, err := d.FunctionByName(ctx, "app", "f", []OID{23, 25, moodArrayOID})
		if err != nil {
			return err
		}
		var types, wantTypes []string
		for i := range f.Arguments {
			types, wantTypes = append(types, f.Arguments[i].Type), append(wantTypes, f3.Arguments[i].Type)
		}
		if !slices.Equal(types, wantTypes) || f.ReturnType != f3.ReturnType || !slices.Equal(f.ArgumentTypeNames, f3.ArgumentTypeNames) {
			return fmt.Errorf("on app's path %+v\nwant the structured names of\n%+v", f, f3)
		}
		// Rendered text — the RETURNS clause, the identity arguments, a
		// default's expression — is the server's, unqualified on the path.
		if f.Result != `"Odd T"` || !strings.Contains(f.IdentityArguments, "VARIADIC c mood[]") ||
			f.Arguments[2].Default != "'{a}'::mood[]" {
			return fmt.Errorf("on app's path Result %q, IdentityArguments %q, default %q: want them unqualified, as the server renders",
				f.Result, f.IdentityArguments, f.Arguments[2].Default)
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
	// A Signature replayed: DDL lands on the routine read.
	if _, err := conn.Exec(ctx, "COMMENT ON FUNCTION "+f3.Signature()+" IS 'replayed'"); err != nil {
		t.Errorf("COMMENT ON FUNCTION %s: %v", f3.Signature(), err)
	} else if again, err := d.FunctionByName(ctx, "app", "f", []OID{23, 25, moodArrayOID}); err != nil || again.Comment != "replayed" {
		t.Errorf("COMMENT ON FUNCTION %s landed elsewhere: %+v", f3.Signature(), again)
	}

	// Every routine of the database — pg_catalog's thousands — has the
	// type names identity.go's own rule gives, and its OIDs.
	idents, err := scanRows(ctx, s.on(ctx), "identities", func(scan func(...any) error) (*RoutineIdentity, error) {
		var r RoutineIdentity
		return &r, scan(scanRoutineIdentity(&r)...)
	}, `SELECT `+routineIdentityColumns+` FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[OID]*RoutineIdentity{}
	for _, r := range idents {
		want[r.OID] = r
	}
	var got []*RoutineIdentity
	start := time.Now()
	allF, err := d.Functions(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Functions: %d in %v", len(allF), time.Since(start))
	for _, f := range allF {
		got = append(got, &f.RoutineIdentity)
	}
	allP, err := d.Procedures(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range allP {
		got = append(got, &p.RoutineIdentity)
	}
	start = time.Now()
	allA, err := d.Aggregates(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Aggregates: %d in %v", len(allA), time.Since(start))
	for _, a := range allA {
		got = append(got, &a.RoutineIdentity)
	}
	if len(got) != len(idents) {
		t.Errorf("the listings hold %d routines, pg_proc %d", len(got), len(idents))
	}
	for _, r := range got {
		if w := want[r.OID]; w == nil || !reflect.DeepEqual(r, w) {
			t.Errorf("%s: listed as\n%+v\nidentity reads\n%+v", r.Signature(), r, w)
		}
	}

	// As a plain role: the same listings and bodies.
	dump := func(ctx context.Context) (string, error) {
		var sb strings.Builder
		fs, err := d.Functions(ctx, app)
		if err != nil {
			return "", err
		}
		for _, f := range fs {
			c := *f
			c.db = nil
			body, err := f.Body(ctx)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&sb, "%+v\n%+v\n", c, body)
		}
		ps, err := d.Procedures(ctx, app)
		if err != nil {
			return "", err
		}
		for _, p := range ps {
			c := *p
			c.db = nil
			body, err := p.Body(ctx)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&sb, "%+v\n%+v\n", c, body)
		}
		as, err := d.Aggregates(ctx, app)
		if err != nil {
			return "", err
		}
		for _, a := range as {
			c := *a
			c.db = nil
			fmt.Fprintf(&sb, "%+v\n", c)
		}
		return sb.String(), nil
	}
	all, err := dump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var asPlain string
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+q(plain)); err != nil {
			return err
		}
		var err error
		asPlain, err = dump(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if asPlain != all {
		t.Errorf("as a plain role the routines differ:\n%s\nwant\n%s", asPlain, all)
	}
}
