//go:build livedb

package pgmo

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/radix29/pgmo/pgsql"
)

// TestLiveRoutineIdentity reads routines of every kind — overloads, user
// and odd-named array types, a domain, VARIADIC, INOUT/OUT, a zero-argument
// and an ordered-set aggregate, a name differing only by case — through a
// session whose search_path finds their types, and replays each Signature
// on a session whose search_path finds look-alike types of the same names
// instead. Each must resolve to its own OID there, and DDL written with it
// must land on it.
func TestLiveRoutineIdentity(t *testing.T) {
	admin, ctx, done := liveDB(t)
	t.Cleanup(done) // registered first, so it runs after the drop
	stamp := time.Now().UnixNano()
	schema := fmt.Sprintf("pgmo_live_ident_%d", stamp)
	shadow := schema + "_shadow"
	S, SH := pgsql.QuoteIdent(schema), pgsql.QuoteIdent(shadow)
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+S+", "+SH+" CASCADE"); err != nil {
			t.Errorf("drop schemas: %v", err)
		}
	})
	for _, stmt := range []string{
		"CREATE SCHEMA " + S,
		"CREATE SCHEMA " + SH,
		"CREATE TYPE " + S + ".ty AS (a integer)",
		"CREATE TYPE " + S + `."Odd T" AS ENUM ('a')`,
		"CREATE DOMAIN " + S + ".dom AS integer",
		// The look-alikes a session searching SH first finds instead.
		"CREATE TYPE " + SH + ".ty AS (b pg_catalog.text)",
		"CREATE TYPE " + SH + `."Odd T" AS ENUM ('z')`,
		"CREATE DOMAIN " + SH + ".dom AS pg_catalog.text",
		"CREATE FUNCTION " + S + ".f(x integer) RETURNS integer LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION " + S + ".f(x " + S + ".ty, VARIADIC y pg_catalog.text[], OUT z integer) LANGUAGE sql AS 'SELECT 1'",
		"CREATE FUNCTION " + S + ".f(" + S + `.dom, "Weird""T" integer DEFAULT 3) RETURNS integer LANGUAGE sql AS 'SELECT 1'`,
		"CREATE FUNCTION " + S + `."F"(` + S + `."Odd T"[], character varying, timestamp with time zone, "char", anyelement) ` +
			"RETURNS integer LANGUAGE sql AS 'SELECT 1'",
		"CREATE PROCEDURE " + S + ".p(INOUT a integer, OUT b integer) LANGUAGE sql AS 'SELECT 1, 2'",
		"CREATE AGGREGATE " + S + ".ag(*) (sfunc = pg_catalog.int8inc, stype = bigint, initcond = '0')",
		"CREATE AGGREGATE " + S + ".os(double precision ORDER BY anyelement) (sfunc = pg_catalog.ordered_set_transition, " +
			"stype = internal, finalfunc = pg_catalog.percentile_disc_final, finalfunc_extra)",
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// reader finds the fixture's own types on its path; replay finds the
	// look-alikes.
	reader, err := Connect(ctx, ConnectionOptions{
		ConnString: *liveConnString, ApplicationName: "pgmo - live identity",
		Params: map[string]string{"search_path": S + ", pg_catalog"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reader.Close)
	replay, err := Connect(ctx, ConnectionOptions{
		ConnString: *liveConnString, ApplicationName: "pgmo - live identity replay",
		Params: map[string]string{"search_path": SH + ", pg_catalog"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replay.Close)

	// want is the catalog's answer, read on reader's path without pgmo.
	type wantRow struct {
		oid                   OID
		name, kind, identArgs string
		argTypes              []OID
		regprocedure          string
	}
	rows, err := reader.Pool().Query(ctx, `SELECT p.oid, p.proname, p.prokind::text, pg_get_function_identity_arguments(p.oid),
		p.proargtypes::oid[], p.oid::regprocedure::text
		FROM pg_proc p WHERE p.pronamespace = $1::regnamespace ORDER BY p.oid`, S)
	if err != nil {
		t.Fatal(err)
	}
	want, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (wantRow, error) {
		var w wantRow
		return w, r.Scan(&w.oid, &w.name, &w.kind, &w.identArgs, &w.argTypes, &w.regprocedure)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 7 {
		t.Fatalf("fixture has %d routines, want 7", len(want))
	}

	pathDependent := 0
	for _, w := range want {
		got, err := routineByName(ctx, reader.Querier(), schema, w.name, w.argTypes)
		if err != nil {
			t.Errorf("%s(%v): %v", w.name, w.argTypes, err)
			continue
		}
		if got.OID != w.oid || got.Schema != schema || got.Name != w.name || string(got.Kind) != w.kind ||
			!got.SameArguments(w.argTypes) || got.IdentityArguments != w.identArgs || len(got.ArgumentTypeNames) != len(w.argTypes) {
			t.Errorf("%s: got %+v; want OID %d kind %s args %v identity %q", w.regprocedure, got, w.oid, w.kind, w.argTypes, w.identArgs)
			continue
		}
		sig := got.Signature()

		// The signature resolves to the routine under replay's path, where
		// the server's own regprocedure text may not.
		var resolved *OID
		if err := replay.Pool().QueryRow(ctx, "SELECT pg_catalog.to_regprocedure($1)::pg_catalog.oid", sig).Scan(&resolved); err != nil {
			t.Fatalf("to_regprocedure(%s): %v", sig, err)
		}
		if resolved == nil || *resolved != w.oid {
			t.Errorf("%s: replayed resolves to %v, want %d", sig, resolved, w.oid)
		}
		var server *OID
		if err := replay.Pool().QueryRow(ctx, "SELECT pg_catalog.to_regprocedure($1)::pg_catalog.oid", w.regprocedure).Scan(&server); err != nil {
			t.Fatalf("to_regprocedure(%s): %v", w.regprocedure, err)
		}
		if server == nil || *server != w.oid {
			pathDependent++
		}

		// DDL written with it lands on the routine: after ROUTINE for every
		// kind, after the kind's keyword for all but a zero-argument
		// aggregate.
		for _, kw := range []string{"ROUTINE", got.Kind.Keyword()} {
			if kw == "AGGREGATE" && len(got.ArgumentTypes) == 0 {
				continue
			}
			comment := fmt.Sprintf("%s %d", kw, w.oid)
			if _, err := replay.Pool().Exec(ctx, "COMMENT ON "+kw+" "+sig+" IS "+pgsql.QuoteLiteral(comment)); err != nil {
				t.Errorf("COMMENT ON %s %s: %v", kw, sig, err)
				continue
			}
			var c *string
			if err := admin.QueryRow(ctx, "SELECT pg_catalog.obj_description($1, 'pg_proc')", w.oid).Scan(&c); err != nil {
				t.Fatal(err)
			}
			if c == nil || *c != comment {
				t.Errorf("COMMENT ON %s %s landed elsewhere: the routine's comment is %v", kw, sig, c)
			}
		}
		t.Logf("%-50s %s", sig, got.IdentityArguments)
	}
	if pathDependent == 0 {
		t.Error("every regprocedure text resolved under replay's path too; the look-alikes are not in the way and the test proves nothing")
	}

	// By name alone: the overloaded f is ambiguous, "F" is not one of its
	// overloads, a zero-argument aggregate is found with no or empty args.
	if _, err := routineByName(ctx, reader.Querier(), schema, "f", nil); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("f with no argument types: err = %v, want ErrAmbiguous", err)
	}
	r, err := routineByName(ctx, reader.Querier(), schema, "F", nil)
	if err != nil || len(r.ArgumentTypes) != 5 {
		t.Fatalf(`"F": got %+v, %v; want the one five-argument routine`, r, err)
	}
	// An array of a user type is written as its element's name with [].
	if got, want := r.ArgumentTypeNames[0], S+`."Odd T"[]`; got != want {
		t.Errorf(`"F"'s first argument type is written %s, want %s`, got, want)
	}
	for _, args := range [][]OID{nil, {}} {
		if r, err := routineByName(ctx, reader.Querier(), schema, "ag", args); err != nil || r.Signature() != S+".ag()" {
			t.Errorf("ag with args %#v: got %+v, %v", args, r, err)
		}
	}
	if _, err := routineByName(ctx, reader.Querier(), schema, "nope", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("nope: err = %v, want ErrNotFound", err)
	}
}
