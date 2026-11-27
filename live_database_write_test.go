//go:build livedb

package pgmo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/radix29/pgmo/pgsql"
)

// TestLiveDatabaseWrites creates a database with every option the server
// takes, exercises each ALTER and drops it, against the catalog:
//
//   - CreateDatabase reads back what the statement set, and the statement it
//     captures under WithScript, run by hand, makes the same database;
//   - Rename and SetTablespace succeed right after a read through
//     DatabaseQuerier — the Server's own idle pool is closed first, or the
//     server would refuse (55006);
//   - SetParameter stores a list parameter one element per value, and every
//     stored setting written back through Values is stored identically;
//   - Drop is refused while another session is connected, and Force gets
//     past it.
func TestLiveDatabaseWrites(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	owner := fmt.Sprintf("pgmo_live_dbw_owner_%d", stamp)
	owner2 := fmt.Sprintf("pgmo_live_dbw_Owner2_%d", stamp)
	ts := fmt.Sprintf("pgmo_live_dbw_ts_%d", stamp)
	db := fmt.Sprintf(`pgmo_live_dbw_%d "Odd's"`, stamp)
	renamed := fmt.Sprintf(`pgmo_live_dbw_%d Renamed`, stamp)
	scripted := fmt.Sprintf("pgmo_live_dbw_%d_script", stamp)
	q := pgsql.QuoteIdent

	// Databases before the tablespace and the roles they use.
	t.Cleanup(func() {
		for _, d := range []string{db, renamed, scripted} {
			for _, stmt := range []string{
				"ALTER DATABASE " + q(d) + " IS_TEMPLATE false",
				"DROP DATABASE IF EXISTS " + q(d) + " WITH (FORCE)",
			} {
				if _, err := admin.Exec(context.Background(), stmt); err != nil && !IsUndefinedObject(err) {
					t.Errorf("%s: %v", stmt, err)
				}
			}
		}
		for _, stmt := range []string{"DROP TABLESPACE IF EXISTS " + q(ts), "DROP ROLE IF EXISTS " + q(owner), "DROP ROLE IF EXISTS " + q(owner2)} {
			if _, err := admin.Exec(context.Background(), stmt); err != nil {
				t.Errorf("%s: %v", stmt, err)
			}
		}
	})
	conn, err := admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"CREATE ROLE " + q(owner) + " NOLOGIN", "CREATE ROLE " + q(owner2) + " NOLOGIN",
		"SET allow_in_place_tablespaces = on",
		"CREATE TABLESPACE " + q(ts) + " LOCATION ''",
		"RESET allow_in_place_tablespaces",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			conn.Release()
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	conn.Release()

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, ApplicationName: "pgmo - live database writes"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	v := s.Version()

	// Refused before anything is sent: an option the server predates, a
	// create inside a transaction, the Server's own database.
	if v < PostgreSQL16 {
		if _, err := s.CreateDatabase(ctx, CreateDatabaseRequest{Name: scripted, ICURules: "&a < g"}); !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("ICU rules on %s: %v, want ErrUnsupportedVersion", v, err)
		}
	}
	if v < PostgreSQL17 {
		if _, err := s.CreateDatabase(ctx, CreateDatabaseRequest{Name: scripted, LocaleProvider: LocaleProviderBuiltin}); !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("builtin provider on %s: %v, want ErrUnsupportedVersion", v, err)
		}
	}
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		_, err := s.CreateDatabase(ctx, CreateDatabaseRequest{Name: scripted})
		return err
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("create in a transaction: %v, want ErrUnsupported", err)
	}
	if err := s.DatabaseRef(s.CurrentDatabase()).Drop(ctx, DropDatabaseOptions{Force: true}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("drop of the Server's own database: %v, want ErrInvalidRequest", err)
	}
	if _, err := s.DatabaseByName(ctx, scripted); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused create made %q: %v", scripted, err)
	}

	req := CreateDatabaseRequest{
		Name: db, Owner: owner, Template: "template0", Encoding: "UTF8",
		Strategy: DatabaseStrategyFileCopy, LocaleProvider: LocaleProviderICU,
		Locale: "C.UTF-8", ICULocale: "und",
		AllowConnections: new(true), ConnectionLimit: new(5), IsTemplate: new(false),
	}
	wantRules := ""
	if v >= PostgreSQL16 {
		req.ICURules, wantRules = "&a < g", "&a < g"
	}
	got, err := s.CreateDatabase(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if got.OID == 0 || got.Name != db || got.Owner != owner || got.Encoding != "UTF8" ||
		got.LocaleProvider != LocaleProviderICU || got.Locale != "und" || got.ICURules != wantRules ||
		got.Collate != "C.UTF-8" || got.CType != "C.UTF-8" || got.IsTemplate || !got.AllowConnections ||
		got.ConnectionLimit != 5 || got.Tablespace != "pg_default" || got.Settings != nil {
		t.Errorf("created %+v", got)
	}

	// The captured statement, run as it is, makes the same database; on 17+
	// with the builtin provider instead.
	sreq := req
	sreq.Name, sreq.Strategy = scripted, DatabaseStrategyWALLog
	if v >= PostgreSQL17 {
		sreq.LocaleProvider, sreq.ICULocale, sreq.ICURules, sreq.BuiltinLocale = LocaleProviderBuiltin, "", "", "C.UTF-8"
	}
	sctx, script := WithScript(ctx)
	if _, err := s.CreateDatabase(sctx, sreq); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DatabaseByName(ctx, scripted); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a scripted create made %q: %v", scripted, err)
	}
	if _, err := admin.Exec(ctx, script.Entries()[0].SQL); err != nil {
		t.Fatalf("the captured statement %q: %v", script.Entries()[0].SQL, err)
	}
	sgot, err := s.DatabaseByName(ctx, scripted)
	if err != nil {
		t.Fatal(err)
	}
	wantProvider, wantLocale := LocaleProviderICU, "und"
	if v >= PostgreSQL17 {
		wantProvider, wantLocale, wantRules = LocaleProviderBuiltin, "C.UTF-8", ""
	}
	if sgot.Owner != owner || sgot.LocaleProvider != wantProvider || sgot.Locale != wantLocale ||
		sgot.ICURules != wantRules || sgot.ConnectionLimit != 5 || sgot.Collate != "C.UTF-8" {
		t.Errorf("the captured statement made %+v", sgot)
	}
	if err := sgot.Drop(ctx, DropDatabaseOptions{}); err != nil {
		t.Fatal(err)
	}

	// Rename right after a read through the database's own pool.
	if _, err := s.DatabaseQuerier(db).Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if err := got.Rename(ctx, renamed); err != nil {
		t.Fatalf("rename after a read through DatabaseQuerier: %v", err)
	}
	if got.Name != renamed {
		t.Errorf("Name %q after the rename", got.Name)
	}
	if _, err := s.DatabaseByName(ctx, db); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old name still reads: %v", err)
	}

	reread := func() *Database {
		t.Helper()
		d, err := s.DatabaseByName(ctx, renamed)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if err := got.SetOwner(ctx, owner2); err != nil {
		t.Fatal(err)
	}
	if err := got.Alter(ctx, DatabaseChanges{AllowConnections: new(false), ConnectionLimit: new(-1), IsTemplate: new(true)}); err != nil {
		t.Fatal(err)
	}
	if r := reread(); r.Owner != owner2 || r.AllowConnections || r.ConnectionLimit != -1 || !r.IsTemplate {
		t.Errorf("after SetOwner and Alter: %+v", r)
	}
	if got.Owner != owner2 || got.AllowConnections || got.ConnectionLimit != -1 || !got.IsTemplate {
		t.Errorf("not mirrored: %+v", got)
	}
	if err := got.Drop(ctx, DropDatabaseOptions{Force: true}); err == nil {
		t.Fatal("dropped a template")
	}
	if err := got.Alter(ctx, DatabaseChanges{AllowConnections: new(true), IsTemplate: new(false)}); err != nil {
		t.Fatal(err)
	}

	// SetTablespace right after a read through the database's own pool.
	if _, err := s.DatabaseQuerier(renamed).Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if err := got.SetTablespace(ctx, ts); err != nil {
		t.Fatalf("set tablespace after a read through DatabaseQuerier: %v", err)
	}
	if r := reread(); r.Tablespace != ts || got.Tablespace != ts {
		t.Errorf("tablespace %q read, %q mirrored; want %q", r.Tablespace, got.Tablespace, ts)
	}

	// Parameters: each value one element of a list parameter, one value of
	// any other; a stored setting written back through Values is stored as
	// it was.
	params := []struct {
		name   string
		values []string
	}{
		{"search_path", []string{`a "b"`, "$user", "", "o'brien", `back\slash`, "Odd=Name", "public"}},
		{"temp_tablespaces", []string{ts}},
		{"work_mem", []string{"64MB"}},
		{"DateStyle", []string{"ISO, DMY"}},
		{"pgmo.live_opt", []string{`x, "y"`}},
	}
	for _, p := range params {
		if err := got.SetParameter(ctx, p.name, p.values...); err != nil {
			t.Fatalf("set %s: %v", p.name, err)
		}
	}
	stored := reread().Settings
	if len(stored) != len(params) {
		t.Fatalf("stored %q, want %d settings", stored, len(params))
	}
	for _, p := range params {
		i := slices.IndexFunc(stored, func(sp ParameterSetting) bool { return sp.Name == p.name })
		if i < 0 {
			t.Errorf("%s not stored: %q", p.name, stored)
			continue
		}
		if vals := stored[i].Values(); !slices.Equal(vals, p.values) {
			t.Errorf("%s stored as %q, Values %q; want %q", p.name, stored[i].Value, vals, p.values)
		}
	}
	if err := got.ResetAllParameters(ctx); err != nil {
		t.Fatal(err)
	}
	if r := reread(); r.Settings != nil || got.Settings != nil {
		t.Errorf("after RESET ALL: read %q, mirrored %q", r.Settings, got.Settings)
	}
	for _, p := range stored {
		if err := got.SetParameter(ctx, p.Name, p.Values()...); err != nil {
			t.Fatalf("write back %s: %v", p.Name, err)
		}
	}
	if again := reread().Settings; !slices.Equal(again, stored) {
		t.Errorf("written back through Values:\n got %q\nwant %q", again, stored)
	}
	if err := got.ResetParameter(ctx, "WORK_MEM"); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(reread().Settings, func(p ParameterSetting) bool { return p.Name == "work_mem" }) {
		t.Error("work_mem still stored after ResetParameter")
	}
	if err := got.ResetParameter(ctx, "work_mem"); err != nil {
		t.Errorf("resetting a parameter not stored: %v", err)
	}

	// Drop: refused while a session is connected; Force terminates it.
	scratch := liveScratchConn(t, ctx, renamed)
	if err := scratch.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := got.Drop(ctx, DropDatabaseOptions{}); pgErrCode(err) != "55006" {
		t.Errorf("drop with a session connected: %v, want 55006", err)
	}
	if err := got.Drop(ctx, DropDatabaseOptions{Force: true}); err != nil {
		t.Fatalf("drop with force: %v", err)
	}
	if _, err := s.DatabaseByName(ctx, renamed); !errors.Is(err, ErrNotFound) {
		t.Errorf("still there after the drop: %v", err)
	}
	if err := got.Drop(ctx, DropDatabaseOptions{}); !IsUndefinedObject(err) {
		t.Errorf("drop of a database gone: %v, want 3D000", err)
	}
}
