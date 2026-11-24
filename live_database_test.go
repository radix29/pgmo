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

// TestLiveDatabases reads three throwaway databases the listing and the
// by-name read must tell apart and describe in full:
//
//   - an ICU template (ICU tailoring rules on 16+) owned by a throwaway role,
//     taking no connections, with a connection limit, a comment, two
//     database-wide settings — one a list holding '=' — and a role-in-database
//     setting that is the role's, not the database's;
//   - two databases whose names differ only by case (builtin provider on 17+,
//     libc before), one closed to PUBLIC.
//
// Size is read as the connected role, as a plain role (refused on the closed
// database: 42501, not ErrNotFound) and as a member of pg_read_all_stats
// (answered without CONNECT), and on the template that takes no connections.
func TestLiveDatabases(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	owner := fmt.Sprintf("pgmo_live_db_owner_%d", stamp)
	plain := fmt.Sprintf("pgmo_live_db_plain_%d", stamp)
	stats := fmt.Sprintf("pgmo_live_db_stats_%d", stamp)
	tpl := fmt.Sprintf("pgmo_live_db_%d_tpl", stamp)
	upper := fmt.Sprintf("pgmo_live_db_%d_Case", stamp)
	lower := fmt.Sprintf("pgmo_live_db_%d_case", stamp)

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, ApplicationName: "pgmo - live databases"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	v := s.Version()

	icuRules, wantRules := "", ""
	if v >= PostgreSQL16 {
		icuRules, wantRules = " ICU_RULES '&a < g'", "&a < g"
	}
	provider, wantProvider, wantLocale := "LOCALE_PROVIDER libc", LocaleProviderLibc, ""
	if v >= PostgreSQL17 {
		provider, wantProvider, wantLocale = "LOCALE_PROVIDER builtin BUILTIN_LOCALE 'C.UTF-8'", LocaleProviderBuiltin, "C.UTF-8"
	}
	q := pgsql.QuoteIdent
	for _, r := range []string{owner, plain, stats} {
		if _, err := admin.Exec(ctx, "CREATE ROLE "+q(r)+" NOLOGIN"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+q(r)); err != nil {
				t.Errorf("drop role %s: %v", r, err)
			}
		})
	}
	for _, d := range []string{tpl, upper, lower} {
		t.Cleanup(func() {
			for _, stmt := range []string{
				"ALTER DATABASE " + q(d) + " IS_TEMPLATE false",
				"DROP DATABASE IF EXISTS " + q(d) + " WITH (FORCE)",
			} {
				if _, err := admin.Exec(context.Background(), stmt); err != nil && !IsUndefinedObject(err) {
					t.Errorf("%s: %v", stmt, err)
				}
			}
		})
	}
	for _, stmt := range []string{
		"CREATE DATABASE " + q(tpl) + " TEMPLATE template0 OWNER " + q(owner) +
			" LOCALE 'C.UTF-8' LOCALE_PROVIDER icu ICU_LOCALE 'und'" + icuRules +
			" CONNECTION LIMIT 3 IS_TEMPLATE true ALLOW_CONNECTIONS false",
		"COMMENT ON DATABASE " + q(tpl) + " IS 'the template'",
		"ALTER DATABASE " + q(tpl) + ` SET search_path = '$user', "Odd=Name", public`,
		"ALTER DATABASE " + q(tpl) + " SET work_mem = '64MB'",
		"ALTER ROLE " + q(owner) + " IN DATABASE " + q(tpl) + " SET work_mem = '1MB'",
		"CREATE DATABASE " + q(upper) + " TEMPLATE template0 LOCALE 'C.UTF-8' " + provider,
		"CREATE DATABASE " + q(lower) + " TEMPLATE template0 LOCALE 'C.UTF-8' " + provider,
		"REVOKE CONNECT ON DATABASE " + q(lower) + " FROM PUBLIC",
		"GRANT pg_read_all_stats TO " + q(stats),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// The listing narrowed to the stamp: the three, in name order (byte
	// order under the catalog's C collation: _Case before _case before _tpl).
	ds, err := s.Databases(ctx, ObjectFilter{Name: []TextCriterion{{TextContains, fmt.Sprint(stamp)}}})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range ds {
		names = append(names, d.Name)
	}
	if want := []string{upper, lower, tpl}; !slices.Equal(names, want) {
		t.Fatalf("Databases = %q, want %q", names, want)
	}
	// A quoted value matches one of the two case-only names alone.
	ds, err = s.Databases(ctx, ObjectFilter{Name: []TextCriterion{{TextEquals, q(upper)}}})
	if err != nil || len(ds) != 1 || ds[0].Name != upper {
		t.Errorf("Databases(equals %s) = %v, %v; want it alone", q(upper), ds, err)
	}

	d, err := s.DatabaseByName(ctx, tpl)
	if err != nil {
		t.Fatal(err)
	}
	got := *d
	got.server, got.OID, got.CollationVersion = nil, 0, ""
	want := Database{
		Name: tpl, Owner: owner, Encoding: "UTF8",
		LocaleProvider: LocaleProviderICU, Collate: "C.UTF-8", CType: "C.UTF-8", Locale: "und", ICURules: wantRules,
		IsTemplate: true, AllowConnections: false, ConnectionLimit: 3, Tablespace: "pg_default",
		Settings: []ParameterSetting{
			{"search_path", `"$user", "Odd=Name", public`},
			{"work_mem", "64MB"},
		},
		Comment: "the template",
	}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("DatabaseByName(%s) =\n%+v\nwant\n%+v", tpl, got, want)
	}
	if d.OID == 0 || d.CollationVersion == "" || d.IsInvalid() || d.Server() != s {
		t.Errorf("OID %d, CollationVersion %q, IsInvalid %v: want an OID and ICU's version, valid", d.OID, d.CollationVersion, d.IsInvalid())
	}

	for _, name := range []string{upper, lower} {
		d, err := s.DatabaseByName(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if d.Name != name || d.LocaleProvider != wantProvider || d.Locale != wantLocale || d.ICURules != "" ||
			d.Settings != nil || d.Comment != "" || d.ConnectionLimit != -1 || !d.AllowConnections || d.IsTemplate {
			t.Errorf("DatabaseByName(%s) = %+v; want a plain %v database", name, d, wantProvider)
		}
	}
	// Exact: a case the catalog does not hold is not found.
	if _, err := s.DatabaseByName(ctx, fmt.Sprintf("pgmo_live_db_%d_CASE", stamp)); !errors.Is(err, ErrNotFound) {
		t.Errorf("DatabaseByName(_CASE): err = %v, want ErrNotFound", err)
	}

	// Size, as each role.
	size := func(role, db string) (int64, error) {
		var n int64
		err := s.InTransaction(ctx, func(ctx context.Context) error {
			if role != "" {
				if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+q(role)); err != nil {
					return err
				}
			}
			var err error
			n, err = s.DatabaseRef(db).Size(ctx)
			return err
		})
		return n, err
	}
	for _, c := range []struct {
		role, db string
		denied   bool
	}{
		{"", tpl, false},      // takes no connections: read from the Server's own database
		{"", lower, false},    // superuser
		{plain, upper, false}, // CONNECT through PUBLIC
		{plain, lower, true},  // no CONNECT
		{stats, lower, false}, // pg_read_all_stats instead of CONNECT
	} {
		n, err := size(c.role, c.db)
		switch {
		case c.denied && (!IsPermissionDenied(err) || errors.Is(err, ErrNotFound)):
			t.Errorf("Size(%s) as %q: %d, %v; want the server's 42501", c.db, c.role, n, err)
		case !c.denied && (err != nil || n <= 0):
			t.Errorf("Size(%s) as %q: %d, %v; want a size", c.db, c.role, n, err)
		}
	}
	if _, err := s.DatabaseRef(fmt.Sprintf("pgmo_live_db_%d_none", stamp)).Size(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("Size of a missing database: err = %v, want ErrNotFound", err)
	}
}
