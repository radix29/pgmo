package pgmo_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// roleRows answers the role read with rows in its SELECT order, the columns
// typed as the server sends them.
func roleRows(rows ...[]any) pgmotest.Responder {
	return func(pgmotest.Query) pgmotest.Result {
		return pgmotest.Result{
			Types: []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID,
				pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.Int4OID,
				pgtype.TimestamptzOID, pgtype.TextArrayOID,
				pgtype.TextArrayOID, pgtype.TextArrayOID,
				pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.BoolArrayOID, pgtype.BoolArrayOID, pgtype.BoolArrayOID,
				pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.BoolArrayOID, pgtype.BoolArrayOID, pgtype.BoolArrayOID,
				pgtype.TextOID},
			Rows: rows,
		}
	}
}

var until = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

// Two roles as pg18 answered for the live test's fixture: a login role with
// every attribute set apart from SUPERUSER, settings in every database and in
// two databases, two grants of one group by different grantors, and a member
// of its own; and pg_monitor, predefined, with no settings (NULLs).
var (
	rowRoleApp = []any{uint32(16400), "app", false, false, true, true, true, true, true, 4,
		pgtype.Timestamptz{Time: until, Valid: true},
		[]string{`search_path="$user", "Odd=Name"`, "work_mem=2MB"},
		[]string{"db1", "db1", "db2"}, []string{"work_mem=3MB", "statement_timeout=5s", "work_mem=4MB"},
		[]string{"grp", "grp"}, []string{"postgres", "granter"}, []bool{true, false}, []bool{true, false}, []bool{true, false},
		[]string{"sub"}, []string{""}, []bool{false}, []bool{true}, []bool{true},
		"the app"}
	rowRoleMonitor = []any{uint32(3373), "pg_monitor", false, true, false, false, false, false, false, -1,
		nil, nil, nil, nil,
		[]string{"pg_read_all_settings"}, []string{"postgres"}, []bool{false}, []bool{true}, []bool{true},
		nil, nil, nil, nil, nil,
		""}
)

// Every field is filled from its own column — memberships zipped from their
// arrays in both directions, database settings grouped by database — and the
// name is bound as $1, never written into the text.
func TestRoleByNameFillsEveryField(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_roles r", roleRows(rowRoleApp))
	s := f.Server()

	got, err := s.RoleByName(t.Context(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if got.Server() != s {
		t.Errorf("Server() = %p, want %p", got.Server(), s)
	}
	want := pgmo.Role{
		OID: 16400, Name: "app",
		Superuser: false, Inherit: false, CreateRole: true, CreateDB: true, CanLogin: true, Replication: true, BypassRLS: true,
		ConnectionLimit: 4, ValidUntil: until,
		Settings: []pgmo.ParameterSetting{{Name: "search_path", Value: `"$user", "Odd=Name"`}, {Name: "work_mem", Value: "2MB"}},
		DatabaseSettings: []pgmo.DatabaseSettings{
			{Database: "db1", Settings: []pgmo.ParameterSetting{{Name: "work_mem", Value: "3MB"}, {Name: "statement_timeout", Value: "5s"}}},
			{Database: "db2", Settings: []pgmo.ParameterSetting{{Name: "work_mem", Value: "4MB"}}},
		},
		MemberOf: []pgmo.RoleMembership{
			{Role: "grp", Member: "app", Grantor: "postgres", Admin: true, Inherit: true, Set: true},
			{Role: "grp", Member: "app", Grantor: "granter"},
		},
		Members: []pgmo.RoleMembership{{Role: "app", Member: "sub", Inherit: true, Set: true}},
		Comment: "the app",
	}
	g := *got
	g.SetServer(nil)
	g.ValidUntil = g.ValidUntil.UTC()
	if !reflect.DeepEqual(g, want) {
		t.Errorf("RoleByName =\n%+v\nwant\n%+v", g, want)
	}
	if n := reflect.TypeFor[pgmo.Role]().NumField(); n != 17 {
		t.Errorf("Role has %d fields; this test checks 17 — add the new one", n)
	}
	if got.IsPredefined() || got.IsBootstrapSuperuser() {
		t.Errorf("IsPredefined %v, IsBootstrapSuperuser %v: want neither", got.IsPredefined(), got.IsBootstrapSuperuser())
	}
	q := f.Queries()
	last := q[len(q)-1]
	if last.Arg(0) != "app" || strings.Contains(last.Text, "'app'") {
		t.Errorf("name not bound as $1: args %v, text\n%s", last.Args, last.Text)
	}
	if !strings.Contains(last.Text, "r.rolname = $1::pg_catalog.text") {
		t.Errorf("name not matched exactly:\n%s", last.Text)
	}
}

// The listing scans every row (NULL arrays read as none) and pushes a name
// filter and System — the predefined roles — into its query; Schema and
// ExtensionMember narrow nothing.
func TestRolesFiltered(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_roles r", roleRows(rowRoleApp, rowRoleMonitor))
	s := f.Server()

	yes := true
	rs, err := s.Roles(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "MON"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "ignored"}},
		System:          &yes,
		ExtensionMember: &yes,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := rs[1]
	if len(rs) != 2 || m.Name != "pg_monitor" || !m.IsPredefined() || m.Settings != nil || m.DatabaseSettings != nil ||
		m.Members != nil || !m.ValidUntil.IsZero() || len(m.MemberOf) != 1 ||
		m.MemberOf[0] != (pgmo.RoleMembership{Role: "pg_read_all_settings", Member: "pg_monitor", Grantor: "postgres", Inherit: true, Set: true}) {
		t.Fatalf("Roles = %+v", rs)
	}
	q := f.Queries()
	last := q[len(q)-1]
	// Exactly the name and the predefined condition, nothing for Schema or
	// ExtensionMember, then the order.
	const tail = "WHERE true AND pg_catalog.strpos(r.rolname, $1) > 0 AND (r.oid < 16384 AND r.oid <> 10)\nORDER BY r.rolname"
	if !strings.HasSuffix(last.Text, tail) || strings.Count(last.Text, "WHERE true") != 1 || len(last.Args) != 1 || last.Arg(0) != "mon" {
		t.Errorf("filter not pushed down as strpos(r.rolname, $1) and the predefined condition, then ordered: args %v\n%s", last.Args, last.Text)
	}
	if t.Failed() {
		return
	}
	// System false is the predefined condition negated.
	no := false
	if _, err := s.Roles(t.Context(), pgmo.ObjectFilter{System: &no}); err != nil {
		t.Fatal(err)
	}
	q = f.Queries()
	if last := q[len(q)-1]; !strings.HasSuffix(last.Text, "WHERE true AND NOT (r.oid < 16384 AND r.oid <> 10)\nORDER BY r.rolname") {
		t.Errorf("System false: not the negated condition:\n%s", last.Text)
	}

	// An unknown operator is refused before anything is sent.
	n := len(f.Queries())
	if _, err := s.Roles(t.Context(), pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}); !errors.Is(err, pgmo.ErrInvalidRequest) {
		t.Errorf("unknown operator: err = %v, want ErrInvalidRequest", err)
	}
	if len(f.Queries()) != n {
		t.Errorf("a refused filter was sent")
	}
}

// Before 16 a grant has no INHERIT or SET option: the member's INHERIT
// attribute and true stand in; from 16 the grant's own columns are read.
func TestRoleMembershipOptionsByRelease(t *testing.T) {
	for _, c := range []struct {
		version         pgmo.ServerVersion
		want, forbidden []string
	}{
		{pgmo.PostgreSQL15, []string{"array_agg(r.rolinherit ORDER BY g.rolname", "array_agg(u.rolinherit ORDER BY u.rolname", "array_agg(true ORDER BY"},
			[]string{"inherit_option", "set_option"}},
		{pgmo.PostgreSQL16, []string{"array_agg(m.inherit_option ORDER BY g.rolname", "array_agg(m.inherit_option ORDER BY u.rolname", "array_agg(m.set_option ORDER BY"},
			[]string{"r.rolinherit ORDER", "u.rolinherit ORDER", "array_agg(true"}},
	} {
		f := pgmotest.New(t)
		f.Version = c.version
		f.On("FROM pg_catalog.pg_roles r", roleRows())
		if _, err := f.Server().Roles(t.Context(), pgmo.ObjectFilter{}); err != nil {
			t.Fatal(err)
		}
		q := f.Queries()
		text := q[len(q)-1].Text
		for _, w := range c.want {
			if !strings.Contains(text, w) {
				t.Errorf("%d: no %q in\n%s", c.version, w, text)
			}
		}
		for _, w := range c.forbidden {
			if strings.Contains(text, w) {
				t.Errorf("%d: %q in\n%s", c.version, w, text)
			}
		}
	}
}

// VALID UNTIL: NULL and 'infinity' are no expiry, '-infinity' always expired.
func TestRoleValidUntil(t *testing.T) {
	for _, c := range []struct {
		name    string
		ts      any
		want    time.Time
		expired bool
	}{
		{"null", nil, time.Time{}, false},
		{"infinity", pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}, time.Time{}, false},
		{"-infinity", pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}, pgmo.ValidUntilNegativeInfinity, true},
		{"past", pgtype.Timestamptz{Time: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{"future", pgtype.Timestamptz{Time: until, Valid: true}, until, false},
	} {
		row := append([]any(nil), rowRoleMonitor...)
		row[10] = c.ts
		f := pgmotest.New(t)
		f.On("FROM pg_catalog.pg_roles r", roleRows(row))
		r, err := f.Server().RoleByName(t.Context(), "pg_monitor")
		if err != nil {
			t.Fatal(err)
		}
		if !r.ValidUntil.Equal(c.want) || r.PasswordExpired(time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)) != c.expired {
			t.Errorf("%s: ValidUntil %v, expired %v; want %v, %v", c.name, r.ValidUntil, r.PasswordExpired(time.Now()), c.want, c.expired)
		}
	}
	if pgmo.ValidUntilNegativeInfinity.After(time.Time{}) {
		t.Errorf("ValidUntilNegativeInfinity %v is after the zero Time", pgmo.ValidUntilNegativeInfinity)
	}
}

// No row is ErrNotFound naming the role; a server error and a malformed row
// are wrapped once, naming the read.
func TestRoleByNameErrors(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_roles r", roleRows())
	_, err := f.Server().RoleByName(t.Context(), "gone")
	if !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), `role "gone" not found`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}

	f = pgmotest.New(t)
	f.On("FROM pg_catalog.pg_roles r", pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	_, err = f.Server().RoleByName(t.Context(), "app")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), `pgmo: read role "app": `) ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}

	// One array of each parallel set cut short: database settings (13), the
	// grantors and the options of MemberOf (15, 16, 18), and of Members
	// (20, 23). rowRoleApp's Members arrays hold one element: they are
	// emptied instead.
	for _, col := range []int{13, 15, 16, 18, 20, 23} {
		row := append([]any(nil), rowRoleApp...)
		switch v := row[col].(type) {
		case []string:
			row[col] = v[:len(v)-1]
		case []bool:
			row[col] = v[:len(v)-1]
		}
		f = pgmotest.New(t)
		f.On("FROM pg_catalog.pg_roles r", roleRows(row))
		_, err = f.Server().Roles(t.Context(), pgmo.ObjectFilter{})
		if err == nil || !strings.HasPrefix(err.Error(), "pgmo: list roles: ") || !strings.Contains(err.Error(), "different lengths") {
			t.Errorf("column %d short: err = %v, want the mismatch wrapped naming the read", col, err)
		}
	}
}

// RoleRef reads nothing and holds the name alone.
func TestRoleRef(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	n := len(f.Queries())
	r := s.RoleRef("app")
	if len(f.Queries()) != n {
		t.Errorf("RoleRef sent a statement")
	}
	g := *r
	g.SetServer(nil)
	if r.Server() != s || !reflect.DeepEqual(g, pgmo.Role{Name: "app"}) || r.IsPredefined() || r.IsBootstrapSuperuser() ||
		r.PasswordExpired(time.Now()) {
		t.Errorf("RoleRef = %+v", r)
	}
}

// IsPredefined is an OID initdb assigned other than the bootstrap
// superuser's; IsBootstrapSuperuser is that one.
func TestRolePredefined(t *testing.T) {
	for oid, want := range map[pgmo.OID][2]bool{
		0: {false, false}, 10: {false, true}, 3373: {true, false}, 6337: {true, false},
		16383: {true, false}, 16384: {false, false}, 20000: {false, false},
	} {
		r := &pgmo.Role{OID: oid}
		if got := [2]bool{r.IsPredefined(), r.IsBootstrapSuperuser()}; got != want {
			t.Errorf("OID %d: IsPredefined, IsBootstrapSuperuser = %v, want %v", oid, got, want)
		}
	}
}
