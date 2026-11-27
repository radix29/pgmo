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

// TestLiveExtensions reads the extensions of a throwaway database:
//
//   - plpgsql, initdb's (a system one);
//   - cube and earthdistance, which requires it, both in "E s";
//   - citext at 1.5, below every server's default, commented, holding a
//     table and a function ALTER EXTENSION … ADD gave it.
//
// The listing matches pg_extension row for row, with each default version
// pg_available_extensions gives; earthdistance's members match pg_depend
// and pg_identify_object under any search_path (its array type is a member
// from 17 alone — found on the lab); the available listing matches both
// views, versions in version order. Everything is read again as a plain
// role (identical) and from another database (none installed there). The
// configuration tables are read from the connected database's pg_cron, when
// it has one: pg_extension_config_dump runs only inside an extension's
// script, which no fixture can supply.
func TestLiveExtensions(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	name := func(s string) string { return fmt.Sprintf("pgmo_live_ext_%d_%s", stamp, s) }
	plain, db, other := name("plain"), name("db"), name("other")
	q := pgsql.QuoteIdent

	t.Cleanup(func() {
		for _, d := range []string{db, other} {
			if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+q(d)+" WITH (FORCE)"); err != nil {
				t.Errorf("drop database: %v", err)
			}
		}
		if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+q(plain)); err != nil {
			t.Errorf("drop role: %v", err)
		}
	})
	for _, stmt := range []string{"CREATE DATABASE " + q(db), "CREATE DATABASE " + q(other), "CREATE ROLE " + q(plain)} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	conn := liveScratchConn(t, ctx, db)
	for _, stmt := range []string{
		`CREATE SCHEMA "E s"`,
		`CREATE EXTENSION cube SCHEMA "E s"`,
		`CREATE EXTENSION earthdistance SCHEMA "E s"`,
		`CREATE EXTENSION citext VERSION '1.5' SCHEMA public`,
		`COMMENT ON EXTENSION citext IS 'ci'`,
		`CREATE TABLE public."T x" (id integer)`,
		`CREATE FUNCTION public."F f"(integer, public.citext) RETURNS integer LANGUAGE sql AS 'SELECT 1'`,
		`ALTER EXTENSION citext ADD TABLE public."T x"`,
		`ALTER EXTENSION citext ADD FUNCTION public."F f"(integer, public.citext)`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: db, ApplicationName: "pgmo - live extensions"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	d, err := s.DatabaseByName(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	// What the catalogs say, read independently.
	var owner string
	if err := conn.QueryRow(ctx, "SELECT current_user::text").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	defaults := map[string]string{}
	rows, err := conn.Query(ctx, "SELECT name::text, COALESCE(default_version, '') FROM pg_catalog.pg_available_extensions")
	if err != nil {
		t.Fatal(err)
	}
	var en, ev string
	if _, err := pgx.ForEachRow(rows, []any{&en, &ev}, func() error { defaults[en] = ev; return nil }); err != nil {
		t.Fatal(err)
	}
	oids := map[string]OID{}
	rows, err = conn.Query(ctx, "SELECT extname::text, oid FROM pg_catalog.pg_extension")
	if err != nil {
		t.Fatal(err)
	}
	var eo OID
	if _, err := pgx.ForEachRow(rows, []any{&en, &eo}, func() error { oids[en] = eo; return nil }); err != nil {
		t.Fatal(err)
	}

	var plpgsqlComment string
	if err := conn.QueryRow(ctx, "SELECT pg_catalog.obj_description(oid, 'pg_extension') FROM pg_catalog.pg_extension WHERE extname = 'plpgsql'").
		Scan(&plpgsqlComment); err != nil {
		t.Fatal(err)
	}
	var plpgsqlOwner string
	if err := conn.QueryRow(ctx, "SELECT pg_catalog.pg_get_userbyid(extowner)::text FROM pg_catalog.pg_extension WHERE extname = 'plpgsql'").
		Scan(&plpgsqlOwner); err != nil {
		t.Fatal(err)
	}
	var cubeComment, earthComment, earthVersion string
	if err := conn.QueryRow(ctx, `SELECT pg_catalog.obj_description((SELECT oid FROM pg_catalog.pg_extension WHERE extname = 'cube'), 'pg_extension'),
		pg_catalog.obj_description((SELECT oid FROM pg_catalog.pg_extension WHERE extname = 'earthdistance'), 'pg_extension'),
		(SELECT extversion FROM pg_catalog.pg_extension WHERE extname = 'earthdistance')`).
		Scan(&cubeComment, &earthComment, &earthVersion); err != nil {
		t.Fatal(err)
	}

	want := []Extension{
		{Name: "citext", Owner: owner, Schema: "public", Version: "1.5", Relocatable: true,
			DefaultVersion: defaults["citext"], Comment: "ci"},
		{Name: "cube", Owner: owner, Schema: "E s", Version: "1.5", Relocatable: true,
			DefaultVersion: defaults["cube"], Comment: cubeComment},
		{Name: "earthdistance", Owner: owner, Schema: "E s", Version: earthVersion, Relocatable: true,
			DefaultVersion: defaults["earthdistance"], Requires: []string{"cube"}, Comment: earthComment},
		{Origin: Origin{IsSystem: true}, Name: "plpgsql", Owner: plpgsqlOwner, Schema: "pg_catalog", Version: "1.0",
			DefaultVersion: defaults["plpgsql"], Comment: plpgsqlComment},
	}
	if want[0].DefaultVersion == "1.5" || want[0].DefaultVersion == "" {
		t.Fatalf("citext's default version is %q: the fixture needs one above 1.5", want[0].DefaultVersion)
	}
	dump := func(es []*Extension) string {
		var sb strings.Builder
		for _, e := range es {
			c := *e
			c.db = nil
			fmt.Fprintf(&sb, "%+v\n", c)
		}
		return sb.String()
	}
	all, err := d.Extensions(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(want) || len(oids) != len(all) {
		t.Fatalf("Extensions = %s, want %d (pg_extension holds %d)", dump(all), len(want), len(oids))
	}
	for i, e := range all {
		if e.Database() != d || oids[e.Name] != e.OID {
			t.Errorf("%s: Database %v, OID %d (pg_extension's is %d)", e.Name, e.Database(), e.OID, oids[e.Name])
		}
		g := *e
		g.db, g.OID = nil, 0
		if !reflect.DeepEqual(g, want[i]) {
			t.Errorf("Extensions[%d] =\n%+v\nwant\n%+v", i, g, want[i])
		}
	}

	yes, no := true, false
	for _, c := range []struct {
		f    ObjectFilter
		want []string
	}{
		{ObjectFilter{Name: []TextCriterion{{TextEquals, "CITEXT"}}}, []string{"citext"}},
		{ObjectFilter{Name: []TextCriterion{{TextEquals, `"CITEXT"`}}}, nil},
		{ObjectFilter{Schema: []TextCriterion{{TextEquals, `"E s"`}}}, []string{"cube", "earthdistance"}},
		{ObjectFilter{System: &yes}, []string{"plpgsql"}},
		{ObjectFilter{System: &no, Name: []TextCriterion{{TextContains, "e"}}}, []string{"citext", "cube", "earthdistance"}},
		{ObjectFilter{ExtensionMember: &yes}, nil},
		{ObjectFilter{ExtensionMember: &no}, []string{"citext", "cube", "earthdistance", "plpgsql"}},
	} {
		es, err := d.Extensions(ctx, c.f)
		var got []string
		for _, e := range es {
			got = append(got, e.Name)
		}
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("Extensions(%+v) = %q, %v; want %q", c.f, got, err, c.want)
		}
	}

	byName, err := d.ExtensionByName(ctx, "earthdistance")
	if err != nil || dump([]*Extension{byName}) != dump(all[2:3]) {
		t.Errorf("ExtensionByName(earthdistance) = %+v, %v; want the listing's", byName, err)
	}
	if _, err := d.ExtensionByName(ctx, "Citext"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ExtensionByName(Citext): err = %v, want ErrNotFound (exact names)", err)
	}
	if _, err := d.ExtensionByName(ctx, "hstore"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ExtensionByName(hstore), available but not installed: err = %v, want ErrNotFound", err)
	}

	// Members: pg_depend's rows, identified as pg_identify_object does,
	// whatever the search_path; the array type from 17.
	type member struct {
		catalog, typ, identity string
		oid                    OID
	}
	members := func(e *Extension) []member {
		t.Helper()
		ms, err := e.Members(ctx)
		if err != nil {
			t.Fatalf("%s.Members: %v", e.Name, err)
		}
		out := make([]member, len(ms))
		for i, m := range ms {
			out[i] = member{m.Catalog, m.Type, m.Identity, m.OID}
		}
		return out
	}
	earth := members(byName)
	var direct []member
	rows, err = conn.Query(ctx, `SELECT d.classid::regclass::text, i.type, i.identity, d.objid
		FROM pg_catalog.pg_depend d, pg_catalog.pg_identify_object(d.classid, d.objid, d.objsubid) i
		WHERE d.refobjid = $1 AND d.deptype = 'e' ORDER BY 2, 3`, byName.OID)
	if err != nil {
		t.Fatal(err)
	}
	var m member
	if _, err := pgx.ForEachRow(rows, []any{&m.catalog, &m.typ, &m.identity, &m.oid}, func() error {
		direct = append(direct, m)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(earth, direct) {
		t.Errorf("earthdistance's members =\n%v\nwant (pg_depend)\n%v", earth, direct)
	}
	wantIdentities := []string{
		`"E s".earth()`, `"E s".earth_box("E s".earth,double precision)`, `"E s".<@>(pg_catalog.point,pg_catalog.point)`, `"E s".earth`,
	}
	if s.Version() >= PostgreSQL17 {
		wantIdentities = append(wantIdentities, `"E s".earth[]`)
	}
	for _, w := range wantIdentities {
		if !slices.ContainsFunc(earth, func(m member) bool { return m.identity == w }) {
			t.Errorf("earthdistance has no member %s: %v", w, earth)
		}
	}
	if s.Version() < PostgreSQL17 && slices.ContainsFunc(earth, func(m member) bool { return m.identity == `"E s".earth[]` }) {
		t.Errorf("earthdistance's array type is a member before 17: the lab said otherwise")
	}
	citext := all[0]
	ci := members(citext)
	for _, w := range []member{{"pg_class", "table", `public."T x"`, 0}, {"pg_proc", "function", `public."F f"(integer,public.citext)`, 0},
		{"pg_type", "type", "public.citext", 0}, {"pg_cast", "cast", "(public.citext AS pg_catalog.text)", 0}} {
		if !slices.ContainsFunc(ci, func(m member) bool {
			return m.catalog == w.catalog && m.typ == w.typ && m.identity == w.identity && m.oid != 0
		}) {
			t.Errorf("citext has no member %+v", w)
		}
	}
	var underPath []member
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.on(ctx).Exec(ctx, `SET LOCAL search_path = "E s", public`); err != nil {
			return err
		}
		ms, err := byName.Members(ctx)
		for _, m := range ms {
			underPath = append(underPath, member{m.Catalog, m.Type, m.Identity, m.OID})
		}
		return err
	})
	if err != nil || !reflect.DeepEqual(underPath, earth) {
		t.Errorf("members under a search_path holding \"E s\" = %v, %v; want those read without it", underPath, err)
	}
	if _, err := d.ExtensionRef("citext").Members(ctx); !errors.Is(err, ErrHandleNotLoaded) {
		t.Errorf("Ref.Members: err = %v, want ErrHandleNotLoaded", err)
	}

	// Available: both views, every row, versions in version order.
	avail, err := d.AvailableExtensions(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	type versionRow struct {
		name, version string
		installed     bool
	}
	var viewVersions []versionRow
	rows, err = conn.Query(ctx, `SELECT name::text, version, installed FROM pg_catalog.pg_available_extension_versions`)
	if err != nil {
		t.Fatal(err)
	}
	var vr versionRow
	if _, err := pgx.ForEachRow(rows, []any{&vr.name, &vr.version, &vr.installed}, func() error {
		viewVersions = append(viewVersions, vr)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var listed []versionRow
	var names []string
	for _, a := range avail {
		names = append(names, a.Name)
		if a.Database() != d || a.DefaultVersion != defaults[a.Name] {
			t.Errorf("%s: Database %v, default %q (the view's %q)", a.Name, a.Database(), a.DefaultVersion, defaults[a.Name])
		}
		if !slices.IsSortedFunc(a.Versions, func(x, y AvailableExtensionVersion) int { return compareExtensionVersions(x.Version, y.Version) }) {
			t.Errorf("%s: versions out of order: %+v", a.Name, a.Versions)
		}
		for _, v := range a.Versions {
			listed = append(listed, versionRow{a.Name, v.Version, v.Installed})
		}
	}
	if len(avail) != len(defaults) || !slices.IsSorted(names) {
		t.Errorf("AvailableExtensions = %q, want the view's %d by name", names, len(defaults))
	}
	byKey := func(x, y versionRow) int { return strings.Compare(x.name+"\x00"+x.version, y.name+"\x00"+y.version) }
	slices.SortFunc(listed, byKey)
	slices.SortFunc(viewVersions, byKey)
	if !reflect.DeepEqual(listed, viewVersions) {
		t.Errorf("available versions differ from pg_available_extension_versions:\n%v\nwant\n%v", listed, viewVersions)
	}
	ac, err := d.AvailableExtensionByName(ctx, "citext")
	if err != nil {
		t.Fatal(err)
	}
	var installed []string
	for _, v := range ac.Versions {
		if v.Installed {
			installed = append(installed, v.Version)
		}
		if !v.Trusted || !v.Superuser || !v.Relocatable || v.Schema != "" || v.Requires != nil || v.Comment == "" {
			t.Errorf("citext %s = %+v: want trusted, superuser, relocatable, any schema, no requirement, commented", v.Version, v)
		}
	}
	if ac.InstalledVersion != "1.5" || !slices.Equal(installed, []string{"1.5"}) || ac.Comment == "" ||
		!slices.ContainsFunc(avail, func(a *AvailableExtension) bool { return reflect.DeepEqual(a, ac) }) {
		t.Errorf("AvailableExtensionByName(citext) = %+v: want 1.5 installed, as listed", ac)
	}
	if ae, err := d.AvailableExtensionByName(ctx, "earthdistance"); err != nil || len(ae.Versions) == 0 ||
		!slices.Equal(ae.Versions[0].Requires, []string{"cube"}) || ae.Versions[0].Trusted {
		t.Errorf("AvailableExtensionByName(earthdistance) = %+v, %v; want untrusted versions requiring cube", ae, err)
	}
	if ap, err := d.AvailableExtensionByName(ctx, "plpgsql"); err != nil || len(ap.Versions) != 1 || ap.Versions[0].Schema != "pg_catalog" ||
		ap.Versions[0].Relocatable {
		t.Errorf("AvailableExtensionByName(plpgsql) = %+v, %v; want one version bound to pg_catalog", ap, err)
	}
	if _, err := d.AvailableExtensionByName(ctx, "Citext"); !errors.Is(err, ErrNotFound) {
		t.Errorf("AvailableExtensionByName(Citext): err = %v, want ErrNotFound", err)
	}
	if as, err := d.AvailableExtensions(ctx, ObjectFilter{Name: []TextCriterion{{TextEquals, "citext"}}, System: &yes}); err != nil ||
		len(as) != 1 || as[0].Name != "citext" {
		t.Errorf("AvailableExtensions(citext, System) = %v, %v; want citext alone, System ignored", as, err)
	}

	// The same as a plain role.
	var asPlain []*Extension
	var plainMembers []ExtensionMember
	var plainAvail []*AvailableExtension
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+q(plain)); err != nil {
			return err
		}
		var err error
		if asPlain, err = d.Extensions(ctx, ObjectFilter{}); err != nil {
			return err
		}
		if plainMembers, err = byName.Members(ctx); err != nil {
			return err
		}
		plainAvail, err = d.AvailableExtensions(ctx, ObjectFilter{})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if dump(asPlain) != dump(all) {
		t.Errorf("as a plain role the extensions differ:\n%s\nwant\n%s", dump(asPlain), dump(all))
	}
	if len(plainMembers) != len(earth) {
		t.Errorf("as a plain role earthdistance has %d members, want %d", len(plainMembers), len(earth))
	}
	if !reflect.DeepEqual(plainAvail, avail) {
		t.Errorf("as a plain role the available extensions differ")
	}

	// Another database has plpgsql alone, and nothing installed elsewhere.
	od := s.DatabaseRef(other)
	if es, err := od.Extensions(ctx, ObjectFilter{}); err != nil || len(es) != 1 || es[0].Name != "plpgsql" {
		t.Errorf("Extensions of another database = %s, %v; want plpgsql alone", dump(es), err)
	}
	if oc, err := od.AvailableExtensionByName(ctx, "citext"); err != nil || oc.InstalledVersion != "" ||
		slices.ContainsFunc(oc.Versions, func(v AvailableExtensionVersion) bool { return v.Installed }) || oc.Database() != od {
		t.Errorf("citext available from another database = %+v, %v; want it not installed there", oc, err)
	}

	// A dropped extension has no members left.
	if _, err := conn.Exec(ctx, "DROP EXTENSION earthdistance"); err != nil {
		t.Fatal(err)
	}
	if ms, err := byName.Members(ctx); err != nil || len(ms) != 0 {
		t.Errorf("members of a dropped extension = %v, %v; want none", ms, err)
	}

	// Configuration tables: the connected database's pg_cron, if any.
	var current string
	if err := admin.QueryRow(ctx, "SELECT current_database()::text").Scan(&current); err != nil {
		t.Fatal(err)
	}
	cron, err := s.DatabaseRef(current).ExtensionByName(ctx, "pg_cron")
	if errors.Is(err, ErrNotFound) {
		t.Logf("no pg_cron in %s: configuration tables not checked", current)
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	var cfg []ExtensionConfigTable
	rows, err = admin.Query(ctx, `SELECT n.nspname::text, c.relname::text, COALESCE(u.cond, '')
		FROM pg_catalog.pg_extension x,
			ROWS FROM (pg_catalog.unnest(x.extconfig), pg_catalog.unnest(x.extcondition)) WITH ORDINALITY AS u(reloid, cond, ord)
		JOIN pg_catalog.pg_class c ON c.oid = u.reloid JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE x.extname = 'pg_cron' ORDER BY u.ord`)
	if err != nil {
		t.Fatal(err)
	}
	var ct ExtensionConfigTable
	if _, err := pgx.ForEachRow(rows, []any{&ct.Table.Schema, &ct.Table.Name, &ct.Condition}, func() error {
		cfg = append(cfg, ct)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(cfg) == 0 || !reflect.DeepEqual(cron.ConfigTables, cfg) {
		t.Errorf("pg_cron's ConfigTables = %+v, want %+v", cron.ConfigTables, cfg)
	}
	if !slices.ContainsFunc(cron.ConfigTables, func(c ExtensionConfigTable) bool { return c.Table == (ObjectName{"cron", "job"}) }) {
		t.Errorf("pg_cron's ConfigTables = %+v, want cron.job among them", cron.ConfigTables)
	}
}
