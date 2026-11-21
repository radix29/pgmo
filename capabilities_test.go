package pgmo_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// capabilitiesRow is one answer to the capabilities read, in its SELECT
// order: role, the five attributes, the defined roles, the held roles.
func capabilitiesRow(role string, super, createRole, createDB, repl, bypass bool, defined, held []string) []any {
	return []any{role, super, createRole, createDB, repl, bypass, defined, held}
}

// Every field is filled from its own column, a defined role not held reads
// false, an undefined one has no key, and the probed names are sent as $1.
func TestCapabilitiesFillsEveryField(t *testing.T) {
	f := pgmotest.New(t)
	f.On("pg_has_role", pgmotest.Rows(capabilitiesRow("app", false, true, false, true, false,
		[]string{"pg_checkpoint", "pg_monitor", "pg_signal_backend"},
		[]string{"pg_monitor"})))
	s := f.Server()

	c, err := s.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := pgmo.Capabilities{
		Role: "app", CreateRole: true, Replication: true,
		PredefinedRoles: map[string]bool{"pg_checkpoint": false, "pg_monitor": true, "pg_signal_backend": false},
	}
	if c.Role != want.Role || c.Superuser || !c.CreateRole || c.CreateDB || !c.Replication || c.BypassRLS {
		t.Errorf("attributes = %+v, want %+v", *c, want)
	}
	if len(c.PredefinedRoles) != len(want.PredefinedRoles) {
		t.Errorf("PredefinedRoles = %v, want %v", c.PredefinedRoles, want.PredefinedRoles)
	}
	for k, v := range want.PredefinedRoles {
		if got, ok := c.PredefinedRoles[k]; !ok || got != v {
			t.Errorf("PredefinedRoles[%q] = %v, %v; want %v", k, got, ok, v)
		}
	}
	if !c.Probed() || !c.InRole("pg_monitor") || c.InRole("pg_signal_backend") || c.InRole("pg_maintain") {
		t.Errorf("Probed/InRole wrong for %v", c.PredefinedRoles)
	}
	if !c.Defines("pg_checkpoint") || c.Defines("pg_maintain") {
		t.Errorf("Defines wrong for %v", c.PredefinedRoles)
	}

	var sentArg any
	for _, q := range f.Queries() {
		if strings.Contains(q.Text, "pg_has_role") {
			sentArg = q.Arg(0)
		}
	}
	if got, ok := sentArg.([]string); !ok || !slices.Equal(got, pgmo.ProbedPredefinedRoles) {
		t.Errorf("$1 = %#v, want ProbedPredefinedRoles", sentArg)
	}
}

// Each attribute column lands in its own field: a row with one attribute
// true at a time sets that one alone.
func TestCapabilitiesAttributeColumns(t *testing.T) {
	get := []func(*pgmo.Capabilities) bool{
		func(c *pgmo.Capabilities) bool { return c.Superuser },
		func(c *pgmo.Capabilities) bool { return c.CreateRole },
		func(c *pgmo.Capabilities) bool { return c.CreateDB },
		func(c *pgmo.Capabilities) bool { return c.Replication },
		func(c *pgmo.Capabilities) bool { return c.BypassRLS },
	}
	for i := range get {
		attrs := make([]bool, len(get))
		attrs[i] = true
		f := pgmotest.New(t)
		f.On("pg_has_role", pgmotest.Rows(capabilitiesRow("r", attrs[0], attrs[1], attrs[2], attrs[3], attrs[4], []string{}, []string{})))
		c, err := f.Server().Capabilities(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for j, g := range get {
			if g(c) != (i == j) {
				t.Errorf("column %d true: field %d = %v", i+1, j, g(c))
			}
		}
		if c.IsSuperuser() != (i == 0) {
			t.Errorf("column %d true: IsSuperuser = %v", i+1, c.IsSuperuser())
		}
		if !c.Probed() {
			t.Error("no predefined role defined: Probed = false, want true")
		}
	}
}

// A nil or zero Capabilities knows nothing: not probed, no role, no
// attribute — and no panic.
func TestCapabilitiesNilSafe(t *testing.T) {
	for _, c := range []*pgmo.Capabilities{nil, {}} {
		if c.Probed() || c.IsSuperuser() || c.InRole("pg_monitor") || c.Defines("pg_monitor") {
			t.Errorf("%#v answers as probed", c)
		}
	}
}

// A failed read is reported once, prefixed, with the server's error kept.
func TestCapabilitiesReadFails(t *testing.T) {
	f := pgmotest.New(t)
	f.On("pg_has_role", pgmotest.Fail(&pgconn.PgError{Code: "42501", Message: "permission denied"}))
	_, err := f.Server().Capabilities(t.Context())
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "42501" || !strings.HasPrefix(err.Error(), "pgmo: read capabilities: ") {
		t.Errorf("err = %v, want the 42501 prefixed once", err)
	}
	if n := strings.Count(err.Error(), "pgmo:"); n != 1 {
		t.Errorf("err = %v: %d prefixes", err, n)
	}
}

// Inside InTransaction the read runs on the transaction, where SET LOCAL
// ROLE changes current_user.
func TestCapabilitiesJoinsTheTransaction(t *testing.T) {
	f := pgmotest.New(t)
	f.On("pg_has_role", pgmotest.Rows(capabilitiesRow("r", false, false, false, false, false, []string{}, []string{})))
	s := f.Server()
	err := s.InTransaction(t.Context(), func(ctx context.Context) error {
		_, err := s.Capabilities(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range f.Queries() {
		if strings.Contains(q.Text, "pg_has_role") && q.Tx != 1 {
			t.Errorf("read ran in transaction %d, want 1", q.Tx)
		}
	}
}

// No row (current_user is a role dropped since the session began) is
// pgx.ErrNoRows, not a zero Capabilities that reads as probed.
func TestCapabilitiesNoRow(t *testing.T) {
	f := pgmotest.New(t)
	f.On("pg_has_role", pgmotest.Rows())
	c, err := f.Server().Capabilities(t.Context())
	if !errors.Is(err, pgx.ErrNoRows) || c != nil {
		t.Errorf("Capabilities = %v, %v; want nil, ErrNoRows", c, err)
	}
}

// databasePrivilegesRow is one database's answer, in its SELECT order: name,
// CREATE, CONNECT, TEMPORARY, datallowconn, invalid.
func databasePrivilegesRow(name string, create, connect, temp, allow, invalid bool) []any {
	return []any{name, create, connect, temp, allow, invalid}
}

// Each column lands in its own field, a database per row, keyed by name.
func TestDatabasePrivilegesColumns(t *testing.T) {
	f := pgmotest.New(t)
	f.On("has_database_privilege", pgmotest.Rows(
		databasePrivilegesRow("c", true, false, false, false, false),
		databasePrivilegesRow("n", false, true, false, false, false),
		databasePrivilegesRow("t", false, false, true, false, false),
		databasePrivilegesRow("a", false, false, false, true, false),
		databasePrivilegesRow("i", false, false, false, false, true),
	))
	got, err := f.Server().DatabasePrivileges(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]pgmo.DatabasePrivileges{
		"c": {Create: true},
		"n": {Connect: true},
		"t": {Temporary: true},
		"a": {AllowConnections: true},
		"i": {Invalid: true},
	}
	if !maps.Equal(got, want) {
		t.Errorf("DatabasePrivileges = %+v, want %+v", got, want)
	}
}

// CanConnect needs CONNECT and a database that takes connections.
func TestDatabasePrivilegesCanConnect(t *testing.T) {
	for _, tc := range []struct {
		p    pgmo.DatabasePrivileges
		want bool
	}{
		{pgmo.DatabasePrivileges{Connect: true, AllowConnections: true}, true},
		{pgmo.DatabasePrivileges{Connect: false, AllowConnections: true}, false},
		{pgmo.DatabasePrivileges{Connect: true, AllowConnections: false}, false},
		{pgmo.DatabasePrivileges{Connect: true, AllowConnections: true, Invalid: true}, false},
	} {
		if got := tc.p.CanConnect(); got != tc.want {
			t.Errorf("%+v.CanConnect() = %v, want %v", tc.p, got, tc.want)
		}
	}
}

// No database visible is an empty map, not nil and not an error.
func TestDatabasePrivilegesEmpty(t *testing.T) {
	f := pgmotest.New(t)
	f.On("has_database_privilege", pgmotest.Rows())
	got, err := f.Server().DatabasePrivileges(t.Context())
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("DatabasePrivileges = %#v, %v; want an empty map", got, err)
	}
}

// A failed read is reported once, prefixed, with the server's error kept.
func TestDatabasePrivilegesReadFails(t *testing.T) {
	f := pgmotest.New(t)
	f.On("has_database_privilege", pgmotest.Fail(&pgconn.PgError{Code: "42501", Message: "permission denied"}))
	_, err := f.Server().DatabasePrivileges(t.Context())
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "42501" || !strings.HasPrefix(err.Error(), "pgmo: read database privileges: ") {
		t.Errorf("err = %v, want the 42501 prefixed once", err)
	}
	if n := strings.Count(err.Error(), "pgmo:"); n != 1 {
		t.Errorf("err = %v: %d prefixes", err, n)
	}
}

// Inside InTransaction the read runs on the transaction, where SET LOCAL
// ROLE changes current_user.
func TestDatabasePrivilegesJoinsTheTransaction(t *testing.T) {
	f := pgmotest.New(t)
	f.On("has_database_privilege", pgmotest.Rows())
	s := f.Server()
	err := s.InTransaction(t.Context(), func(ctx context.Context) error {
		_, err := s.DatabasePrivileges(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, q := range f.Queries() {
		if strings.Contains(q.Text, "has_database_privilege") {
			n++
			if q.Tx != 1 {
				t.Errorf("read ran in transaction %d, want 1", q.Tx)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d reads, want 1", n)
	}
}
