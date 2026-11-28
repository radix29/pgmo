//go:build livedb

package pgmo

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/radix29/pgmo/pgsql"
)

// TestLiveDependencies drives the P4.14 reads and writes against a throwaway
// database (liveObjectWrites' fixture) holding every kind of edge: a view
// over a table's columns through its rule, a foreign key through its
// constraint and the referenced key's index, a default using a sequence, a
// sequence owned by a column and an identity's, a trigger's function, a
// SQL-standard body reading the view, a policy naming a role, a grant, a
// partitioned table in a tablespace of the test's own and its partition's
// index, inheritance, a domain, an extension's members. Each object's
// Dependencies and Dependents are checked edge for edge against pg_depend
// and pg_shdepend folded independently, in Go; the shared reads, both
// searches, REASSIGN OWNED and DROP OWNED against the catalog.
func TestLiveDependencies(t *testing.T) {
	l := newLiveObjectWrites(t)
	q := pgsql.QuoteIdent
	sc := q(liveObjSchema)
	tsName := fmt.Sprintf("pgmo_live_deps_%d T's", l.stamp)
	inPlace, err := Connect(l.ctx, ConnectionOptions{ConnString: *liveConnString, Database: "postgres",
		ApplicationName: "pgmo - live dependencies (in place)", Params: map[string]string{"allow_in_place_tablespaces": "on"}})
	l.ok("connect in place", err)
	t.Cleanup(func() {
		// The partitioned table names the tablespace: the database goes
		// first (its own cleanup, later, finds it gone).
		for _, stmt := range []string{"DROP DATABASE IF EXISTS " + q(l.db) + " WITH (FORCE)", "DROP TABLESPACE IF EXISTS " + q(tsName)} {
			if _, err := inPlace.exec(context.Background(), stmt); err != nil {
				t.Errorf("%s: %v", stmt, err)
			}
		}
		inPlace.Close()
	})
	ts, err := inPlace.CreateTablespace(l.ctx, CreateTablespaceRequest{Name: tsName})
	l.ok("create tablespace", err)
	tt := sc + `."T t"`
	for _, stmt := range []string{
		"CREATE SCHEMA " + sc,
		"CREATE TYPE " + sc + ".mood AS ENUM ('a', 'b')",
		"CREATE TABLE " + tt + " (id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY, m " + sc + ".mood, v text DEFAULT 'x' CHECK (v <> ''))",
		"CREATE SEQUENCE " + sc + ".owned OWNED BY " + tt + ".v",
		"CREATE TABLE " + sc + ".c (id integer REFERENCES " + tt + ", n serial)",
		"CREATE INDEX c_id ON " + sc + ".c (id)",
		"CREATE VIEW " + sc + ".v AS SELECT id, m FROM " + tt,
		"CREATE FUNCTION " + sc + ".f(integer) RETURNS bigint LANGUAGE sql BEGIN ATOMIC SELECT pg_catalog.count(*) FROM " + sc + ".v WHERE id = $1; END",
		"CREATE FUNCTION " + sc + ".trg() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'",
		"CREATE TRIGGER tr BEFORE INSERT ON " + tt + " FOR EACH ROW EXECUTE FUNCTION " + sc + ".trg()",
		"CREATE POLICY p ON " + tt + " TO " + q(l.own) + " USING (true)",
		"GRANT SELECT ON " + tt + " TO " + q(l.own2),
		"ALTER FUNCTION " + sc + ".trg() OWNER TO " + q(l.own),
		"CREATE TABLE " + sc + ".pt (a integer, b integer) PARTITION BY RANGE (a) TABLESPACE " + q(tsName),
		"CREATE TABLE " + sc + ".pt1 PARTITION OF " + sc + ".pt FOR VALUES FROM (0) TO (10)",
		"CREATE INDEX pt_b ON " + sc + ".pt (b)",
		"CREATE TABLE " + sc + ".par (x integer)",
		"CREATE TABLE " + sc + ".chi () INHERITS (" + sc + ".par)",
		"CREATE DOMAIN " + sc + ".pos AS integer CHECK (VALUE > 0)",
		"CREATE TABLE " + sc + ".dt (p " + sc + ".pos)",
		"CREATE EXTENSION citext SCHEMA " + sc,
		"ALTER DATABASE " + q(l.db) + " OWNER TO " + q(l.own),
	} {
		l.exec(stmt)
	}
	d, err := l.s.DatabaseByName(l.ctx, l.db)
	l.ok("read the database", err)
	l.d = d

	l.dependencyCrossCheck(ts)
	l.dependencyCases(ts)
	l.searches()
	l.ownedWrites()
}

// depEdge is one raw catalog edge: the dependent (cls, obj, sub), the
// referenced one, the letter, whether pg_shdepend's, and the dependent's
// relkind when it is a relation.
type depEdge struct {
	cls, obj         OID
	sub              int32
	refCls, refObj   OID
	refSub           int32
	deptype, relkind string
	shared           bool
}

// depKey is what the cross-check compares of a Dependency: the other
// object's address and the kind.
type depKey struct {
	addr ObjectAddress
	kind DependencyKind
}

// dependencyCrossCheck reads pg_depend and this database's pg_shdepend
// whole, folds them in Go by the rule dependency.go states — an internal
// edge from anything but a sequence, an automatic one from a default,
// constraint, rule, trigger, policy or index, never an edge to itself —
// and checks every user object's (OID ≥ 16384) Dependencies and
// Dependents, and the dependents of both roles and the tablespace, edge
// for edge.
func (l *liveObjectWrites) dependencyCrossCheck(ts *Tablespace) {
	t, ctx := l.t, l.ctx
	cat := map[OID]string{}
	rows, err := l.conn.Query(ctx, `SELECT c.oid, c.relname::pg_catalog.text FROM pg_catalog.pg_class c
		WHERE c.relnamespace = 'pg_catalog'::pg_catalog.regnamespace AND c.relkind = 'r'`)
	l.ok("read catalogs", err)
	for rows.Next() {
		var oid OID
		var name string
		l.ok("scan catalog", rows.Scan(&oid, &name))
		cat[oid] = name
	}
	l.ok("catalogs", rows.Err())
	byName := map[string]OID{}
	for oid, name := range cat {
		byName[name] = oid
	}
	var edges []depEdge
	rows, err = l.conn.Query(ctx, `SELECT d.classid, d.objid, d.objsubid, d.refclassid, d.refobjid, d.refobjsubid,
			d.deptype::pg_catalog.text, COALESCE(r.relkind::pg_catalog.text, ''), false
		FROM pg_catalog.pg_depend d
		LEFT JOIN pg_catalog.pg_class r ON d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND r.oid = d.objid
		UNION ALL
		SELECT s.classid, s.objid, s.objsubid, s.refclassid, s.refobjid, 0, s.deptype::pg_catalog.text, '', true
		FROM pg_catalog.pg_shdepend s
		WHERE s.dbid = (SELECT oid FROM pg_catalog.pg_database WHERE datname = pg_catalog.current_database())`)
	l.ok("read edges", err)
	for rows.Next() {
		var e depEdge
		l.ok("scan edge", rows.Scan(&e.cls, &e.obj, &e.sub, &e.refCls, &e.refObj, &e.refSub, &e.deptype, &e.relkind, &e.shared))
		edges = append(edges, e)
	}
	l.ok("edges", rows.Err())

	partCatalogs := []OID{byName["pg_attrdef"], byName["pg_constraint"], byName["pg_rewrite"], byName["pg_trigger"], byName["pg_policy"]}
	isFold := func(e depEdge) bool {
		if e.shared || (e.cls == e.refCls && e.obj == e.refObj) {
			return false
		}
		return (e.deptype == "i" && e.relkind != "S") ||
			(e.deptype == "a" && (e.relkind == "i" || e.relkind == "I" || slices.Contains(partCatalogs, e.cls)))
	}
	owner := map[[2]OID][2]OID{}
	for _, e := range edges {
		if isFold(e) {
			owner[[2]OID{e.cls, e.obj}] = [2]OID{e.refCls, e.refObj}
		}
	}
	root := func(cls, obj OID) [2]OID {
		at := [2]OID{cls, obj}
		for range 20 {
			next, ok := owner[at]
			if !ok {
				return at
			}
			at = next
		}
		t.Fatalf("no root for %v in 20 steps", [2]OID{cls, obj})
		return at
	}
	addr := func(r [2]OID) ObjectAddress { return ObjectAddress{Catalog: cat[r[0]], OID: r[1]} }

	wantDeps, wantDents := map[ObjectAddress]map[depKey]bool{}, map[ObjectAddress]map[depKey]bool{}
	add := func(m map[ObjectAddress]map[depKey]bool, of ObjectAddress, k depKey) {
		if m[of] == nil {
			m[of] = map[depKey]bool{}
		}
		m[of][k] = true
	}
	for _, e := range edges {
		from := root(e.cls, e.obj)
		to := [2]OID{e.refCls, e.refObj}
		if !e.shared {
			to = root(e.refCls, e.refObj)
		}
		if from == to || (!e.shared && isFold(e)) {
			continue
		}
		kind := dependencyKind(e.deptype, e.shared)
		add(wantDeps, addr(from), depKey{addr(to), kind})
		add(wantDents, addr(to), depKey{addr(from), kind})
	}

	got := func(deps []Dependency) map[depKey]bool {
		out := map[depKey]bool{}
		for _, d := range deps {
			if d.Type == "" || d.Identity == "" || d.Shared != (d.Kind == dependencyKind("o", true) || d.Kind == DependencyPrivilege ||
				d.Kind == DependencyPolicyRole || d.Kind == DependencyInitialPrivilege || d.Kind == DependencyTablespace) {
				t.Errorf("%+v: unnamed, or Shared not the kind's", d)
			}
			out[depKey{d.Address(), d.Kind}] = true
		}
		return out
	}
	objects := map[ObjectAddress]bool{}
	for _, e := range edges {
		for _, r := range [][2]OID{root(e.cls, e.obj), root(e.refCls, e.refObj)} {
			if r[1] >= 16384 && !e.shared {
				objects[addr(r)] = true
			}
		}
	}
	role := func(name string) ObjectAddress {
		var oid OID
		l.ok("role oid", l.conn.QueryRow(ctx, "SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1", name).Scan(&oid))
		return ObjectAddress{Catalog: "pg_authid", OID: oid}
	}
	if len(objects) < 40 {
		t.Fatalf("%d user objects in the fixture's edges, want every one of them", len(objects))
	}
	for _, a := range slices.SortedFunc(maps.Keys(objects), func(a, b ObjectAddress) int { return int(a.OID) - int(b.OID) }) {
		deps, err := l.d.Dependencies(ctx, a)
		l.ok(fmt.Sprintf("Dependencies(%+v)", a), err)
		if g, w := got(deps), wantDeps[a]; !maps.Equal(g, w) && !(len(g) == 0 && len(w) == 0) {
			t.Errorf("Dependencies(%+v) = %v\nwant %v", a, g, w)
		}
		dents, err := l.d.Dependents(ctx, a)
		l.ok(fmt.Sprintf("Dependents(%+v)", a), err)
		if g, w := got(dents), wantDents[a]; !maps.Equal(g, w) && !(len(g) == 0 && len(w) == 0) {
			t.Errorf("Dependents(%+v) = %v\nwant %v", a, g, w)
		}
	}
	for _, a := range []ObjectAddress{role(l.own), role(l.own2), ts.Address()} {
		dents, err := l.d.Dependents(ctx, a)
		l.ok(fmt.Sprintf("Dependents(%+v)", a), err)
		if g, w := got(dents), wantDents[a]; len(w) == 0 || !maps.Equal(g, w) {
			t.Errorf("Dependents(%+v) = %v\nwant %v", a, g, w)
		}
	}
	t.Logf("cross-checked %d objects over %d edges", len(objects), len(edges))
}

// dependencyCases pins what the cross-check cannot name: the parts each
// edge is recorded on, the identities, the shared reads, and the refusals.
func (l *liveObjectWrites) dependencyCases(ts *Tablespace) {
	t, d, s, ctx := l.t, l.d, l.s, l.ctx
	q := pgsql.QuoteIdent
	sc := q(liveObjSchema)
	tt := sc + `."T t"`
	line := func(x Dependency) string {
		out := string(x.Kind) + " " + x.Type + " " + x.Identity
		if x.Part != "" {
			out += " via " + x.PartType + " " + x.Part
		}
		if x.OwnPart != "" {
			out += " from " + x.OwnPartType + " " + x.OwnPart
		}
		return out
	}
	// all is every edge's line; lines, the edges inside the database
	// alone, for the checks below whose shared edges depend on the role
	// running the test (a bootstrap superuser owns with no edge).
	all := func(deps []Dependency) []string {
		var out []string
		for _, x := range deps {
			out = append(out, line(x))
		}
		return out
	}
	lines := func(deps []Dependency, err error) []string {
		t.Helper()
		l.ok("read", err)
		return all(slices.DeleteFunc(deps, func(x Dependency) bool { return x.Shared }))
	}
	shared := func(deps []Dependency, err error) []string {
		t.Helper()
		l.ok("read", err)
		return all(deps)
	}
	check := func(what string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", what, got, want)
		}
	}
	view, err := d.ViewByName(ctx, liveObjSchema, "v")
	l.ok("read view", err)
	check("Dependencies(v)", lines(d.Dependencies(ctx, view.Address())), []string{
		"normal schema " + sc,
		"normal table " + tt + " via table column " + tt + ".id from rule \"_RETURN\" on " + sc + ".v",
		"normal table " + tt + " via table column " + tt + ".m from rule \"_RETURN\" on " + sc + ".v",
		"normal type " + sc + ".mood from view column " + sc + ".v.m",
	})
	table, err := d.TableByName(ctx, liveObjSchema, "T t")
	l.ok("read table", err)
	check("Dependents(T t)", lines(d.Dependents(ctx, table.Address())), []string{
		"internal sequence " + sc + `."T t_id_seq" from table column ` + tt + ".id",
		"auto sequence " + sc + ".owned from table column " + tt + ".v",
		"normal table " + sc + ".c via table constraint c_id_fkey on " + sc + ".c from table column " + tt + ".id",
		"normal table " + sc + ".c via table constraint c_id_fkey on " + sc + ".c from index " + sc + `."T t_pkey"`,
		"normal view " + sc + ".v via rule \"_RETURN\" on " + sc + ".v from table column " + tt + ".id",
		"normal view " + sc + ".v via rule \"_RETURN\" on " + sc + ".v from table column " + tt + ".m",
	})
	deps, err := d.Dependencies(ctx, table.Address())
	l.ok("Dependencies(T t)", err)
	for _, w := range []string{
		"normal function " + sc + ".trg() from trigger tr on " + tt,
		"policy role role " + q(l.own) + " from policy p on " + tt,
		"privilege role " + q(l.own2),
		"normal schema " + sc,
		"normal type " + sc + ".mood from table column " + tt + ".m",
	} {
		if !slices.Contains(all(deps), w) {
			t.Errorf("Dependencies(T t): no %q in %q", w, all(deps))
		}
	}
	pt, err := d.TableByName(ctx, liveObjSchema, "pt")
	l.ok("read pt", err)
	check("Dependents(pt)", lines(d.Dependents(ctx, pt.Address())), []string{
		"auto table " + sc + ".pt1",
		"partition primary table " + sc + ".pt1 via index " + sc + ".pt1_b_idx from index " + sc + ".pt_b",
	})
	deps, err = d.Dependencies(ctx, pt.Address())
	if w := "tablespace tablespace " + q(ts.Name); err != nil || !slices.Contains(all(deps), w) {
		t.Errorf("Dependencies(pt): no %q in %q (%v)", w, all(deps), err)
	}
	trg, err := d.FunctionByName(ctx, liveObjSchema, "trg", nil)
	l.ok("read trg", err)
	check("Dependents(trg)", lines(d.Dependents(ctx, trg.Address())), []string{
		"normal table " + tt + " via trigger tr on " + tt,
	})
	f, err := d.FunctionByName(ctx, liveObjSchema, "f", nil)
	l.ok("read f", err)
	check("Dependencies(f)", lines(d.Dependencies(ctx, f.Address())), []string{
		"normal schema " + sc,
		"normal view " + sc + ".v via view column " + sc + ".v.id",
	})
	cit, err := d.ExtensionByName(ctx, "citext")
	l.ok("read citext", err)
	members, err := cit.Members(ctx)
	l.ok("citext members", err)
	dents, err := d.Dependents(ctx, cit.Address())
	l.ok("Dependents(citext)", err)
	if len(dents) != len(members) {
		t.Errorf("Dependents(citext): %d, want its %d members", len(dents), len(members))
	}
	for _, x := range dents {
		if x.Kind != DependencyExtension || !slices.ContainsFunc(members, func(m ExtensionMember) bool { return m.Address() == x.Address() }) {
			t.Errorf("Dependents(citext): %+v is no member", x)
		}
	}

	// The shared reads: the database's owner, the role's database, the
	// databases holding the role's dependents.
	check("Server.Dependencies(database)", shared(s.Dependencies(ctx, d.Address())), []string{"owner role " + q(l.own)})
	own, err := s.RoleByName(ctx, l.own)
	l.ok("read role", err)
	check("Server.Dependents(own)", shared(s.Dependents(ctx, own.Address())), []string{"owner database " + q(l.db)})
	if got, err := s.DependentDatabases(ctx, own.Address()); err != nil || !slices.Equal(got, []string{l.db}) {
		t.Errorf("DependentDatabases(own) = %q, %v; want the test's database", got, err)
	}

	// Refusals and absences.
	if _, err := d.Dependencies(ctx, ObjectAddress{Catalog: "pg_class", OID: 1}); !errors.Is(err, ErrNotFound) {
		t.Errorf("no such relation: %v, want ErrNotFound", err)
	}
	if _, err := d.Dependents(ctx, ObjectAddress{Catalog: "no_catalog", OID: table.OID}); !errors.Is(err, ErrNotFound) {
		t.Errorf("no such catalog: %v, want ErrNotFound", err)
	}
	if _, err := s.Dependents(ctx, ObjectAddress{Catalog: "pg_authid", OID: 1}); !errors.Is(err, ErrNotFound) {
		t.Errorf("no such role: %v, want ErrNotFound", err)
	}
	if got, err := d.Dependents(ctx, d.SchemaRef("pg_catalog").Address()); !errors.Is(err, ErrHandleNotLoaded) {
		t.Errorf("a Ref: %+v, %v", got, err)
	}
	pgc, err := d.SchemaByName(ctx, "pg_catalog")
	l.ok("read pg_catalog", err)
	if got, err := d.Dependents(ctx, pgc.Address()); err != nil || len(got) != 0 {
		t.Errorf("pg_catalog's dependents: %d, %v; want none recorded — initdb's objects are pinned", len(got), err)
	}
}

// searches checks both searches against the fixture: names folded as a
// filter folds them, a quoted name exact; triggers by their table's schema;
// extension members told apart; definitions matched as written.
func (l *liveObjectWrites) searches() {
	t, d, ctx := l.t, l.d, l.ctx
	q := pgsql.QuoteIdent
	sc := q(liveObjSchema)
	inSchema := []TextCriterion{{TextEquals, sc}}
	names := func(rs []SearchResult, err error) []string {
		t.Helper()
		l.ok("search", err)
		var out []string
		for _, r := range rs {
			out = append(out, r.Type+" "+r.Identity)
		}
		return out
	}
	check := func(what string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got %q\nwant %q", what, got, want)
		}
	}
	check(`Search("T")`, names(d.Search(ctx, ObjectFilter{Name: []TextCriterion{{TextContains, `"T"`}}, Schema: inSchema})),
		[]string{"sequence " + sc + `."T t_id_seq"`, "table " + sc + `."T t"`})
	user := ObjectFilter{Schema: inSchema, ExtensionMember: new(false)}
	check("Search(user objects)", names(d.Search(ctx, user)), []string{
		"domain " + sc + ".pos",
		"function " + sc + ".f(integer)", "function " + sc + ".trg()",
		"sequence " + sc + `."T t_id_seq"`, "sequence " + sc + ".c_n_seq", "sequence " + sc + ".owned",
		"table " + sc + `."T t"`, "table " + sc + ".c", "table " + sc + ".chi", "table " + sc + ".dt",
		"table " + sc + ".par", "table " + sc + ".pt", "table " + sc + ".pt1",
		"trigger tr on " + sc + `."T t"`,
		"type " + sc + ".mood",
		"view " + sc + ".v",
	})
	rs, err := d.Search(ctx, ObjectFilter{Name: []TextCriterion{{TextEquals, "tr"}}, Schema: inSchema})
	if err != nil || len(rs) != 1 || rs[0].Table != "T t" || rs[0].Schema != liveObjSchema || rs[0].Catalog != "pg_trigger" || rs[0].IsSystem {
		t.Errorf("Search(tr) = %+v, %v; want the trigger on T t", rs, err)
	}
	members, err := d.Search(ctx, ObjectFilter{Schema: inSchema, ExtensionMember: new(true)})
	l.ok("search members", err)
	if len(members) < 10 {
		t.Errorf("%d citext members found, want its types and functions", len(members))
	}
	for _, r := range members {
		if r.Extension != "citext" {
			t.Errorf("%+v: not citext's", r)
		}
	}
	if rs, err := d.Search(ctx, ObjectFilter{Name: []TextCriterion{{TextEquals, "pg_class"}}}); err != nil || len(rs) != 1 || !rs[0].IsSystem {
		t.Errorf("Search(pg_class) = %+v, %v; want the system table", rs, err)
	}

	check(`SearchDefinitions("RETURN NEW")`, names(d.SearchDefinitions(ctx, "RETURN NEW", user)), []string{"function " + sc + ".trg()"})
	check(`SearchDefinitions("return new")`, names(d.SearchDefinitions(ctx, "return new", user)), nil)
	check(`SearchDefinitions("count(*)")`, names(d.SearchDefinitions(ctx, "count(*)", user)), []string{"function " + sc + ".f(integer)"})
	check(`SearchDefinitions("T t")`, names(d.SearchDefinitions(ctx, `"T t"`, user)), []string{"view " + sc + ".v"})
	if rs, err := d.SearchDefinitions(ctx, "int4pl", ObjectFilter{}); err != nil ||
		slices.ContainsFunc(rs, func(r SearchResult) bool { return r.Name == "int4pl" }) {
		t.Errorf("SearchDefinitions(int4pl) = %+v, %v; an internal function's symbol is no body", rs, err)
	}
}

// ownedWrites reassigns and drops what the roles own, read back through
// the catalog and the dependency reads.
func (l *liveObjectWrites) ownedWrites() {
	t, d, s, ctx := l.t, l.d, l.s, l.ctx
	q := pgsql.QuoteIdent
	own, err := s.RoleByName(ctx, l.own)
	l.ok("read own", err)
	own2, err := s.RoleByName(ctx, l.own2)
	l.ok("read own2", err)
	kinds := func(r *Role) []string {
		t.Helper()
		deps, err := d.Dependents(ctx, r.Address())
		l.ok("dependents", err)
		var out []string
		for _, x := range deps {
			out = append(out, string(x.Kind)+" "+x.Type)
		}
		return out
	}

	// The captured statement, run by hand, does what the call does: here
	// it is run, and the call is checked after it.
	stmt := l.scripted(func(ctx context.Context) error { return d.ReassignOwned(ctx, []string{l.own}, l.own2) })
	if want := "REASSIGN OWNED BY " + q(l.own) + " TO " + q(l.own2); stmt != want {
		t.Errorf("captured %q, want %q", stmt, want)
	}
	l.ok("reassign", d.ReassignOwned(ctx, []string{l.own}, l.own2))
	trg, err := d.FunctionByName(ctx, liveObjSchema, "trg", nil)
	if err != nil || trg.Owner != l.own2 {
		t.Errorf("trg after REASSIGN OWNED: %+v, %v", trg, err)
	}
	if db, err := s.DatabaseByName(ctx, l.db); err != nil || db.Owner != l.own2 {
		t.Errorf("the database after REASSIGN OWNED: %+v, %v — a shared object is reassigned too", db, err)
	}
	if got := kinds(own); !slices.Equal(got, []string{"policy role table"}) {
		t.Errorf("own's dependents after REASSIGN OWNED: %q, want its policy alone", got)
	}
	l.ok("drop owned", d.DropOwned(ctx, []string{l.own}, DropOptions{}))
	if got := kinds(own); got != nil {
		t.Errorf("own's dependents after DROP OWNED: %q", got)
	}
	if n := l.scalar("SELECT pg_catalog.count(*)::pg_catalog.text FROM pg_catalog.pg_policy WHERE polname = 'p'"); n != "0" {
		t.Errorf("%s policies left; the one naming own alone goes with it", n)
	}
	if dbs, err := s.DependentDatabases(ctx, own.Address()); err != nil || dbs != nil {
		t.Errorf("own's databases after DROP OWNED: %q, %v", dbs, err)
	}

	// own2 now owns the trigger's function: RESTRICT is refused while the
	// trigger uses it, CASCADE takes the trigger too and revokes the grant.
	l.wantCode("drop owned, restrict", d.DropOwned(ctx, []string{l.own2}, DropOptions{}), "2BP01")
	l.ok("drop owned, cascade", d.DropOwned(ctx, []string{l.own2}, DropOptions{Cascade: true}))
	if got := kinds(own2); got != nil {
		t.Errorf("own2's dependents after DROP OWNED CASCADE: %q", got)
	}
	if n := l.scalar("SELECT pg_catalog.count(*)::pg_catalog.text FROM pg_catalog.pg_trigger WHERE tgname = 'tr'"); n != "0" {
		t.Errorf("%s triggers left; CASCADE drops the one on the dropped function", n)
	}
	if got, err := s.Dependents(ctx, own2.Address()); err != nil || len(got) != 1 || got[0].Kind != DependencyOwner {
		t.Errorf("own2's shared dependents: %+v, %v; the database it owns stays", got, err)
	}
}
