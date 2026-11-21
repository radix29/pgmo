//go:build livedb

package pgmo

import (
	"context"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/radix29/pgmo/pgsql"
)

// TestLiveCapabilities reads Capabilities as the connected superuser and,
// through SET LOCAL ROLE inside InTransaction, as two throwaway roles: one
// with every attribute and pg_monitor (whose members hold
// pg_read_all_settings and pg_read_all_stats by nesting), one NOINHERIT
// member of pg_signal_backend, which is a member without its privileges.
// pg_maintain is defined from 17 on.
func TestLiveCapabilities(t *testing.T) {
	admin, ctx, done := liveDB(t)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	full := fmt.Sprintf("pgmo_live_cap_full_%d", stamp)
	noinherit := fmt.Sprintf("pgmo_live_cap_noinh_%d", stamp)
	for _, stmt := range []string{
		"CREATE ROLE " + pgsql.QuoteIdent(full) + " NOLOGIN INHERIT CREATEDB CREATEROLE REPLICATION BYPASSRLS",
		"GRANT pg_monitor TO " + pgsql.QuoteIdent(full),
		"CREATE ROLE " + pgsql.QuoteIdent(noinherit) + " NOLOGIN NOINHERIT",
		"GRANT pg_signal_backend TO " + pgsql.QuoteIdent(noinherit),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		for _, r := range []string{full, noinherit} {
			if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgsql.QuoteIdent(r)); err != nil {
				t.Errorf("drop role %s: %v", r, err)
			}
		}
	})

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, ApplicationName: "pgmo - live capabilities"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	defined := map[string]bool{}
	for _, name := range ProbedPredefinedRoles {
		defined[name] = name != "pg_maintain" || s.Version() >= 170000
	}
	roles := func(held ...string) map[string]bool {
		m := map[string]bool{}
		for name, ok := range defined {
			if ok {
				m[name] = false
			}
		}
		for _, name := range held {
			m[name] = true
		}
		return m
	}

	// as reads Capabilities as role ("" for the connected one).
	as := func(t *testing.T, role string) *Capabilities {
		t.Helper()
		var c *Capabilities
		err := s.InTransaction(ctx, func(ctx context.Context) error {
			if role != "" {
				if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+pgsql.QuoteIdent(role)); err != nil {
					return err
				}
			}
			var err error
			c, err = s.Capabilities(ctx)
			return err
		})
		if err != nil {
			t.Fatalf("Capabilities as %q: %v", role, err)
		}
		return c
	}

	t.Run("superuser", func(t *testing.T) {
		c, err := s.Capabilities(ctx) // outside a transaction too
		if err != nil {
			t.Fatal(err)
		}
		if !c.Probed() || !c.IsSuperuser() {
			t.Errorf("superuser: %+v", c)
		}
		all := roles()
		for name := range all {
			all[name] = true
		}
		if !maps.Equal(c.PredefinedRoles, all) {
			t.Errorf("PredefinedRoles = %v, want %v", c.PredefinedRoles, all)
		}
	})

	t.Run("every attribute, pg_monitor", func(t *testing.T) {
		c := as(t, full)
		if c.Role != full || c.Superuser || !c.CreateRole || !c.CreateDB || !c.Replication || !c.BypassRLS {
			t.Errorf("attributes: %+v", c)
		}
		want := roles("pg_monitor", "pg_read_all_settings", "pg_read_all_stats")
		if !maps.Equal(c.PredefinedRoles, want) {
			t.Errorf("PredefinedRoles = %v, want %v", c.PredefinedRoles, want)
		}
	})

	t.Run("noinherit member", func(t *testing.T) {
		c := as(t, noinherit)
		if c.Role != noinherit || c.Superuser || c.CreateRole || c.CreateDB || c.Replication || c.BypassRLS {
			t.Errorf("attributes: %+v", c)
		}
		if want := roles(); !maps.Equal(c.PredefinedRoles, want) {
			t.Errorf("PredefinedRoles = %v, want %v", c.PredefinedRoles, want)
		}
		if !c.Defines("pg_signal_backend") || c.InRole("pg_signal_backend") {
			t.Errorf("pg_signal_backend: defined %v, held %v; want defined, not held", c.Defines("pg_signal_backend"), c.InRole("pg_signal_backend"))
		}
		if c.Defines("pg_maintain") != (s.Version() >= 170000) {
			t.Errorf("pg_maintain defined = %v on %d", c.Defines("pg_maintain"), s.Version())
		}
	})
}

// TestLiveDatabasePrivileges reads DatabasePrivileges as the connected
// superuser and, through SET LOCAL ROLE, as two throwaway roles over three
// throwaway databases: one with CONNECT and TEMPORARY revoked from PUBLIC and
// CREATE granted to a group role (held by an inheriting member, not by a
// NOINHERIT one), one closed with ALLOW_CONNECTIONS false, and one marked
// invalid as an interrupted DROP DATABASE leaves it (datconnlimit = -2). The
// two the catalog says take no connections are checked to refuse one.
func TestLiveDatabasePrivileges(t *testing.T) {
	admin, ctx, done := liveDB(t)
	t.Cleanup(done) // registered first, so it runs after the drops
	stamp := time.Now().UnixNano()
	group := fmt.Sprintf("pgmo_live_dbp_group_%d", stamp)
	member := fmt.Sprintf("pgmo_live_dbp_member_%d", stamp)
	noinherit := fmt.Sprintf("pgmo_live_dbp_noinh_%d", stamp)
	granted := fmt.Sprintf("pgmo_live_dbp_granted_%d", stamp)
	closed := fmt.Sprintf("pgmo_live_dbp_closed_%d", stamp)
	invalid := fmt.Sprintf("pgmo_live_dbp_invalid_%d", stamp)

	t.Cleanup(func() { // registered before the databases' drops, so it runs after them: their ACLs name the group
		for _, r := range []string{member, noinherit, group} {
			if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgsql.QuoteIdent(r)); err != nil {
				t.Errorf("drop role %s: %v", r, err)
			}
		}
	})
	for _, db := range []string{granted, closed, invalid} {
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgsql.QuoteIdent(db)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgsql.QuoteIdent(db)+" WITH (FORCE)"); err != nil {
				t.Errorf("drop database %s: %v", db, err)
			}
		})
	}
	for _, stmt := range []string{
		"CREATE ROLE " + pgsql.QuoteIdent(group) + " NOLOGIN",
		"CREATE ROLE " + pgsql.QuoteIdent(member) + " NOLOGIN INHERIT IN ROLE " + pgsql.QuoteIdent(group),
		"CREATE ROLE " + pgsql.QuoteIdent(noinherit) + " NOLOGIN NOINHERIT IN ROLE " + pgsql.QuoteIdent(group),
		"REVOKE CONNECT, TEMPORARY ON DATABASE " + pgsql.QuoteIdent(granted) + " FROM PUBLIC",
		"GRANT CREATE ON DATABASE " + pgsql.QuoteIdent(granted) + " TO " + pgsql.QuoteIdent(group),
		"ALTER DATABASE " + pgsql.QuoteIdent(closed) + " ALLOW_CONNECTIONS false",
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// What an interrupted DROP DATABASE leaves behind; DROP DATABASE clears it.
	if _, err := admin.Exec(ctx, "UPDATE pg_catalog.pg_database SET datconnlimit = -2 WHERE datname = $1", invalid); err != nil {
		t.Fatal(err)
	}

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, ApplicationName: "pgmo - live database privileges"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	// as reads DatabasePrivileges as role ("" for the connected one).
	as := func(t *testing.T, role string) map[string]DatabasePrivileges {
		t.Helper()
		var m map[string]DatabasePrivileges
		err := s.InTransaction(ctx, func(ctx context.Context) error {
			if role != "" {
				if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+pgsql.QuoteIdent(role)); err != nil {
					return err
				}
			}
			var err error
			m, err = s.DatabasePrivileges(ctx)
			return err
		})
		if err != nil {
			t.Fatalf("DatabasePrivileges as %q: %v", role, err)
		}
		return m
	}
	check := func(t *testing.T, m map[string]DatabasePrivileges, db string, want DatabasePrivileges) {
		t.Helper()
		if got, ok := m[db]; !ok || got != want {
			t.Errorf("%s = %+v, %v; want %+v", db, got, ok, want)
		}
	}
	all := DatabasePrivileges{Create: true, Connect: true, Temporary: true, AllowConnections: true}

	t.Run("superuser", func(t *testing.T) {
		m, err := s.DatabasePrivileges(ctx) // outside a transaction too
		if err != nil {
			t.Fatal(err)
		}
		check(t, m, granted, all)
		check(t, m, s.CurrentDatabase(), all)
		closedWant, invalidWant := all, all
		closedWant.AllowConnections = false
		invalidWant.Invalid = true
		check(t, m, closed, closedWant)
		check(t, m, invalid, invalidWant)
		if m["template0"].AllowConnections || m["template0"].CanConnect() {
			t.Errorf("template0 = %+v, want closed", m["template0"])
		}
		for _, db := range []string{closed, invalid} {
			if m[db].CanConnect() {
				t.Errorf("%s: CanConnect = true", db)
			}
			if _, err := s.DatabaseQuerier(db).Exec(ctx, "SELECT 1"); err == nil {
				t.Errorf("%s: connected, want refused", db)
			}
		}
	})

	t.Run("inheriting member", func(t *testing.T) {
		m := as(t, member)
		check(t, m, granted, DatabasePrivileges{Create: true, AllowConnections: true})
		// PUBLIC's default CONNECT and TEMPORARY, nothing more.
		check(t, m, closed, DatabasePrivileges{Connect: true, Temporary: true})
		if m[granted].CanConnect() {
			t.Errorf("%s: CanConnect without CONNECT", granted)
		}
	})

	t.Run("noinherit member", func(t *testing.T) {
		check(t, as(t, noinherit), granted, DatabasePrivileges{AllowConnections: true})
	})
}
