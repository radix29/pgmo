package pgmo_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// routineRows answers the routine read with rows in its SELECT order, the
// columns typed as the server sends them.
func routineRows(rows ...[]any) pgmotest.Responder {
	return func(pgmotest.Query) pgmotest.Result {
		return pgmotest.Result{
			Types: []uint32{pgtype.OIDOID, pgtype.NameOID, pgtype.NameOID, pgtype.TextOID,
				pgtype.OIDArrayOID, pgtype.TextArrayOID, pgtype.TextOID},
			Rows: rows,
		}
	}
}

// Two overloads of idt.f and an ordered-set aggregate, as pg18 answered for
// the live test's fixture.
var (
	rowFInt = []any{uint32(17271), "idt", "f", "f", []uint32{23}, []string{"integer"}, "x integer"}
	rowFTy  = []any{uint32(17272), "idt", "f", "f", []uint32{17268, 1009}, []string{"idt.ty", "text[]"},
		"x ty, VARIADIC y text[], OUT z integer"}
	rowOS = []any{uint32(17276), "idt", "os", "a", []uint32{701, 2283}, []string{"double precision", "anyelement"},
		"double precision ORDER BY anyelement"}
)

// Every field is filled from its own column, and the schema and name are
// bound as $1 and $2, never written into the text.
func TestRoutineByNameFillsEveryField(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_proc", routineRows(rowOS))
	s := f.Server()

	got, err := pgmo.RoutineByName(t.Context(), s.Querier(), "idt", "os", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := &pgmo.RoutineIdentity{
		OID: 17276, Schema: "idt", Name: "os", Kind: pgmo.RoutineAggregate,
		ArgumentTypes:     []pgmo.OID{701, 2283},
		ArgumentTypeNames: []string{"double precision", "anyelement"},
		IdentityArguments: "double precision ORDER BY anyelement",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RoutineByName =\n%+v\nwant\n%+v", got, want)
	}
	if n := reflect.TypeFor[pgmo.RoutineIdentity]().NumField(); n != 7 {
		t.Errorf("RoutineIdentity has %d fields; this test checks 7 — add the new one", n)
	}
	q := f.Queries()
	last := q[len(q)-1]
	if last.Arg(0) != "idt" || last.Arg(1) != "os" {
		t.Errorf("args = %v, %v; want idt, os", last.Arg(0), last.Arg(1))
	}
	if strings.Contains(last.Text, "'idt'") || strings.Contains(last.Text, "'os'") {
		t.Errorf("the name is written into the SQL:\n%s", last.Text)
	}
}

// An overloaded name with no argument types is ErrAmbiguous, naming each
// overload's signature; with argument types, the one overload matching them.
func TestRoutineByNameOverloads(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_proc", routineRows(rowFInt, rowFTy))
	s := f.Server()
	ctx := t.Context()

	_, err := pgmo.RoutineByName(ctx, s.Querier(), "idt", "f", nil)
	if !errors.Is(err, pgmo.ErrAmbiguous) || errors.Is(err, pgmo.ErrNotFound) {
		t.Fatalf("no argument types: err = %v, want ErrAmbiguous only", err)
	}
	for _, sig := range []string{"idt.f(integer)", "idt.f(idt.ty, text[])"} {
		if !strings.Contains(err.Error(), sig) {
			t.Errorf("error %q does not name overload %s", err, sig)
		}
	}

	got, err := pgmo.RoutineByName(ctx, s.Querier(), "idt", "f", []pgmo.OID{17268, 1009})
	if err != nil {
		t.Fatal(err)
	}
	if got.OID != 17272 {
		t.Errorf("argument types (17268, 1009): got OID %d, want 17272", got.OID)
	}
	got, err = pgmo.RoutineByName(ctx, s.Querier(), "idt", "f", []pgmo.OID{23})
	if err != nil || got.OID != 17271 {
		t.Errorf("argument types (23): got %+v, %v; want OID 17271", got, err)
	}

	// A prefix of the arguments, or the same types in another order, is
	// another routine.
	for _, args := range [][]pgmo.OID{{17268}, {1009, 17268}, {}} {
		if _, err := pgmo.RoutineByName(ctx, s.Querier(), "idt", "f", args); !errors.Is(err, pgmo.ErrNotFound) {
			t.Errorf("argument types %v: err = %v, want ErrNotFound", args, err)
		}
	}
}

// No routine of that name is ErrNotFound; a failed read is the server's
// error, not absence; an empty schema is refused before anything is sent.
func TestRoutineByNameErrors(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_proc", routineRows())
	s := f.Server()
	ctx := t.Context()

	if _, err := pgmo.RoutineByName(ctx, s.Querier(), "idt", "nope", nil); !errors.Is(err, pgmo.ErrNotFound) {
		t.Errorf("no such routine: err = %v, want ErrNotFound", err)
	}

	before := len(f.Queries())
	if _, err := pgmo.RoutineByName(ctx, s.Querier(), "", "f", nil); !errors.Is(err, pgmo.ErrSchemaRequired) {
		t.Errorf("empty schema: err = %v, want ErrSchemaRequired", err)
	}
	if n := len(f.Queries()); n != before {
		t.Errorf("empty schema sent %d statements", n-before)
	}

	g := pgmotest.New(t)
	g.On("FROM pg_catalog.pg_proc", pgmotest.Fail(&pgconn.PgError{Code: "42501"}))
	_, err := pgmo.RoutineByName(ctx, g.Server().Querier(), "idt", "f", nil)
	if !pgmo.IsPermissionDenied(err) || errors.Is(err, pgmo.ErrNotFound) {
		t.Errorf("failed read: err = %v, want the server's 42501 and not ErrNotFound", err)
	}
}

// Signature writes the qualified name, quoted, and the argument type names
// as read; no arguments is "()", which ROUTINE accepts for an aggregate too.
func TestRoutineSignature(t *testing.T) {
	cases := []struct {
		r          pgmo.RoutineIdentity
		qualified  string
		sig, kword string
	}{
		{pgmo.RoutineIdentity{Schema: "idt", Name: "f", Kind: pgmo.RoutineFunction,
			ArgumentTypeNames: []string{"idt.ty", "text[]"}}, "idt.f", "idt.f(idt.ty, text[])", "FUNCTION"},
		{pgmo.RoutineIdentity{Schema: "My Schema", Name: "Select", Kind: pgmo.RoutineProcedure,
			ArgumentTypeNames: []string{`idt."Odd T"[]`}}, `"My Schema"."Select"`, `"My Schema"."Select"(idt."Odd T"[])`, "PROCEDURE"},
		{pgmo.RoutineIdentity{Schema: "idt", Name: "ag", Kind: pgmo.RoutineAggregate}, "idt.ag", "idt.ag()", "AGGREGATE"},
		{pgmo.RoutineIdentity{Schema: "public", Name: "rank_x", Kind: pgmo.RoutineWindow,
			ArgumentTypeNames: []string{"integer"}}, "public.rank_x", "public.rank_x(integer)", "FUNCTION"},
		{pgmo.RoutineIdentity{Schema: "public", Name: "u", Kind: "?"}, "public.u", "public.u()", "ROUTINE"},
	}
	for _, tc := range cases {
		if got := tc.r.QualifiedName(); got != tc.qualified {
			t.Errorf("QualifiedName = %s, want %s", got, tc.qualified)
		}
		if got := tc.r.Signature(); got != tc.sig {
			t.Errorf("Signature = %s, want %s", got, tc.sig)
		}
		if got := tc.r.Kind.Keyword(); got != tc.kword {
			t.Errorf("%q.Keyword() = %s, want %s", tc.r.Kind, got, tc.kword)
		}
	}
}
