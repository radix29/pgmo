//go:build livedb

package pgmo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/radix29/pgmo/pgsql"
)

// TestLivePrivilegeWrites drives every P4.13.4 write against a throwaway
// database (liveObjectWrites' fixture): GRANT and REVOKE on every securable
// kind, read back through the family's own Privileges; the captured
// statement run by hand gives the same ACL; a grant option passed on and
// revoked with CASCADE; default privileges read back and given to a table
// created afterwards; row-level security and policies read back and
// filtering rows for a plain role; the server's refusals; a rolled-back
// transaction leaving nothing.
func TestLivePrivilegeWrites(t *testing.T) {
	l := newLiveObjectWrites(t)
	q := pgsql.QuoteIdent
	sc := q(liveObjSchema)
	for _, stmt := range []string{
		"CREATE SCHEMA " + sc,
		"CREATE TABLE " + sc + `."T t" (a int, "B b" text)`,
		"CREATE SEQUENCE " + sc + ".seq",
		"CREATE VIEW " + sc + ".v AS SELECT 1 AS x",
		"CREATE MATERIALIZED VIEW " + sc + ".mv AS SELECT 1 AS x",
		"CREATE FUNCTION " + sc + ".f(integer, text[]) RETURNS int LANGUAGE sql AS 'SELECT 1'",
		"CREATE PROCEDURE " + sc + ".p() LANGUAGE sql AS 'SELECT 1'",
		"CREATE AGGREGATE " + sc + ".agg(int) (SFUNC = int4pl, STYPE = int)",
		"CREATE TYPE " + sc + ".mood AS ENUM ('a')",
		"CREATE DOMAIN " + sc + ".pos AS int CHECK (VALUE > 0)",
		"CREATE FOREIGN DATA WRAPPER " + q(`W"w`),
		"CREATE SERVER " + q(`S"s`) + " FOREIGN DATA WRAPPER " + q(`W"w`),
		"CREATE FOREIGN TABLE " + sc + ".ft (a int) SERVER " + q(`S"s`),
	} {
		l.exec(stmt)
	}
	l.grants()
	l.grantOptions()
	l.sharedObjects()
	l.defaultPrivileges()
	l.rowSecurity()
	l.privilegeRefusals()
	l.privilegeTransaction()
}

// grantSet is a family's Privileges for one grantee, "PRIV" or "PRIV*" for
// one held WITH GRANT OPTION, sorted.
func grantSet(grants []PrivilegeGrant, grantee string) []string {
	var out []string
	for _, g := range grants {
		if g.Grantee != grantee {
			continue
		}
		p := string(g.Privilege)
		if g.Grantable {
			p += "*"
		}
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// grants grants on every kind in the database, to a role, its quote-hostile
// twin and PUBLIC, and revokes again, each read back by the family's read.
func (l *liveObjectWrites) grants() {
	t, d, ctx := l.t, l.d, l.ctx
	read := map[string]func() ([]PrivilegeGrant, error){}
	secs := map[string]Securable{}
	add := func(name string, sec Securable, r func() ([]PrivilegeGrant, error)) {
		secs[name], read[name] = sec, r
	}
	tb, err := d.TableByName(ctx, liveObjSchema, "T t")
	l.ok("read table", err)
	add("table", tb.Securable(), func() ([]PrivilegeGrant, error) {
		x, err := d.TableByName(ctx, liveObjSchema, "T t")
		return privilegesOf(x, err)
	})
	add("sequence", d.SequenceRef(liveObjSchema, "seq").Securable(), func() ([]PrivilegeGrant, error) {
		x, err := d.SequenceByName(ctx, liveObjSchema, "seq")
		return privilegesOf(x, err)
	})
	add("view", d.ViewRef(liveObjSchema, "v").Securable(), func() ([]PrivilegeGrant, error) {
		x, err := d.ViewByName(ctx, liveObjSchema, "v")
		return privilegesOf(x, err)
	})
	add("matview", d.MaterializedViewRef(liveObjSchema, "mv").Securable(), func() ([]PrivilegeGrant, error) {
		x, err := d.MaterializedViewByName(ctx, liveObjSchema, "mv")
		return privilegesOf(x, err)
	})
	add("foreign table", d.ForeignTableRef(liveObjSchema, "ft").Securable(), func() ([]PrivilegeGrant, error) {
		x, err := d.ForeignTableByName(ctx, liveObjSchema, "ft")
		return privilegesOf(x, err)
	})
	fn, err := d.FunctionByName(ctx, liveObjSchema, "f", nil)
	l.ok("read function", err)
	add("function", fn.Securable(), func() ([]PrivilegeGrant, error) {
		x, err := d.FunctionByName(ctx, liveObjSchema, "f", nil)
		return privilegesOf(x, err)
	})
	pr, err := d.ProcedureByName(ctx, liveObjSchema, "p", nil)
	l.ok("read procedure", err)
	add("procedure", pr.Securable(), func() ([]PrivilegeGrant, error) {
		x, err := d.ProcedureByName(ctx, liveObjSchema, "p", nil)
		return privilegesOf(x, err)
	})
	ag, err := d.AggregateByName(ctx, liveObjSchema, "agg", nil)
	l.ok("read aggregate", err)
	add("aggregate", ag.Securable(), func() ([]PrivilegeGrant, error) {
		x, err := d.AggregateByName(ctx, liveObjSchema, "agg", nil)
		return privilegesOf(x, err)
	})
	add("type", d.TypeRef(liveObjSchema, "mood").Securable(), func() ([]PrivilegeGrant, error) {
		x, err := d.TypeByName(ctx, liveObjSchema, "mood")
		return privilegesOf(x, err)
	})
	add("domain", d.DomainRef(liveObjSchema, "pos").Securable(), func() ([]PrivilegeGrant, error) {
		x, err := d.DomainByName(ctx, liveObjSchema, "pos")
		return privilegesOf(x, err)
	})
	add("schema", d.SchemaRef(liveObjSchema).Securable(), func() ([]PrivilegeGrant, error) {
		x, err := d.SchemaByName(ctx, liveObjSchema)
		return privilegesOf(x, err)
	})
	add("language", d.LanguageRef("plpgsql").Securable(), func() ([]PrivilegeGrant, error) {
		x, err := d.LanguageByName(ctx, "plpgsql")
		return privilegesOf(x, err)
	})
	add("wrapper", d.ForeignDataWrapperRef(`W"w`).Securable(), func() ([]PrivilegeGrant, error) {
		x, err := d.ForeignDataWrapperByName(ctx, `W"w`)
		return privilegesOf(x, err)
	})
	add("server", d.ForeignServerRef(`S"s`).Securable(), func() ([]PrivilegeGrant, error) {
		x, err := d.ForeignServerByName(ctx, `S"s`)
		return privilegesOf(x, err)
	})
	// Every privilege each kind takes, MAINTAIN only where the server has it.
	privs := map[string][]Privilege{
		"sequence": sequencePrivileges, "function": executePrivilege, "procedure": executePrivilege, "aggregate": executePrivilege,
		"type": usagePrivilege, "domain": usagePrivilege, "schema": {PrivilegeUsage, PrivilegeCreate},
		"language": usagePrivilege, "wrapper": usagePrivilege, "server": usagePrivilege,
	}
	relation := slices.Clone(tablePrivileges)
	if before(l.num, PostgreSQL17) {
		relation = slices.DeleteFunc(relation, func(p Privilege) bool { return p == PrivilegeMaintain })
	}
	for _, k := range []string{"table", "view", "matview", "foreign table"} {
		privs[k] = relation
	}
	names := func(ps []Privilege, star string) []string {
		out := make([]string, len(ps))
		for i, p := range ps {
			out[i] = string(p) + star
		}
		slices.Sort(out)
		return out
	}
	for name, sec := range secs {
		ps := privs[name]
		l.ok("grant on "+name, d.ApplyPrivileges(ctx, sec, PrivilegeChange{Action: ActionGrant, Privileges: ps,
			Grantees: []string{l.own, l.own2}, GrantOption: true}))
		l.ok("grant all on "+name+" to PUBLIC", d.ApplyPrivileges(ctx, sec, PrivilegeChange{Action: ActionGrant, All: true, Grantees: []string{""}}))
		got, err := read[name]()
		l.ok("read "+name, err)
		for _, who := range []string{l.own, l.own2} {
			if g := grantSet(got, who); !reflect.DeepEqual(g, names(ps, "*")) {
				t.Errorf("%s: %s holds %v, want %v", name, who, g, names(ps, "*"))
			}
		}
		if g := grantSet(got, ""); !reflect.DeepEqual(g, names(ps, "")) {
			t.Errorf("%s: PUBLIC holds %v after ALL PRIVILEGES, want %v", name, g, names(ps, ""))
		}
		// Revoke: the grant option alone, then everything.
		l.ok("revoke grant option on "+name, d.ApplyPrivileges(ctx, sec, PrivilegeChange{Action: ActionRevoke, Privileges: ps[:1],
			Grantees: []string{l.own}, GrantOption: true}))
		l.ok("revoke all on "+name, d.ApplyPrivileges(ctx, sec, PrivilegeChange{Action: ActionRevoke, All: true, Grantees: []string{l.own2, ""}}))
		got, err = read[name]()
		l.ok("read "+name, err)
		want := names(ps, "*")
		want[slices.Index(want, string(ps[0])+"*")] = string(ps[0])
		slices.Sort(want)
		if g := grantSet(got, l.own); !reflect.DeepEqual(g, want) {
			t.Errorf("%s: after revoking the grant option of %s, %s holds %v, want %v", name, ps[0], l.own, g, want)
		}
		if g, p := grantSet(got, l.own2), grantSet(got, ""); g != nil || p != nil {
			t.Errorf("%s: after REVOKE ALL, %s holds %v and PUBLIC %v", name, l.own2, g, p)
		}
	}

	// Columns, and ALL … IN SCHEMA.
	cols := tb.Securable()
	cols.Columns = []string{"a", "B b"}
	l.ok("grant on columns", d.ApplyPrivileges(ctx, cols, PrivilegeChange{Action: ActionGrant,
		Privileges: []Privilege{PrivilegeSelect, PrivilegeUpdate}, Grantees: []string{l.own2}}))
	columns, err := tb.Columns(ctx)
	l.ok("read columns", err)
	for _, c := range columns {
		if g := grantSet(c.Privileges, l.own2); !reflect.DeepEqual(g, []string{"SELECT", "UPDATE"}) {
			t.Errorf("column %s: %s holds %v", c.Name, l.own2, g)
		}
	}
	l.ok("grant on all sequences", d.ApplyPrivileges(ctx, Securable{Kind: SecurableSequence, Schema: liveObjSchema, AllInSchema: true},
		PrivilegeChange{Action: ActionGrant, Privileges: []Privilege{PrivilegeUsage}, Grantees: []string{l.own2}}))
	l.ok("grant on all routines", d.ApplyPrivileges(ctx, Securable{Kind: SecurableRoutine, Schema: liveObjSchema, AllInSchema: true},
		PrivilegeChange{Action: ActionGrant, Privileges: []Privilege{PrivilegeExecute}, Grantees: []string{l.own2}}))
	if g, err := read["sequence"](); err != nil || !slices.Contains(grantSet(g, l.own2), "USAGE") {
		t.Errorf("ALL SEQUENCES IN SCHEMA: %v, %v", g, err)
	}
	for _, k := range []string{"function", "procedure", "aggregate"} {
		if g, err := read[k](); err != nil || !slices.Contains(grantSet(g, l.own2), "EXECUTE") {
			t.Errorf("ALL ROUTINES IN SCHEMA, %s: %v, %v", k, g, err)
		}
	}

	// The statement captured under WithScript, run by hand on one new view,
	// gives the same ACL as ApplyPrivileges on its twin.
	l.exec("CREATE VIEW " + pgsql.QualifiedName(liveObjSchema, "v2") + " AS SELECT 1 AS x")
	l.exec("CREATE VIEW " + pgsql.QualifiedName(liveObjSchema, "v3") + " AS SELECT 1 AS x")
	change := PrivilegeChange{Action: ActionGrant, Privileges: []Privilege{PrivilegeSelect, PrivilegeTrigger},
		Grantees: []string{l.own2, l.own}, GrantOption: true}
	stmt := l.scripted(func(ctx context.Context) error {
		return d.ApplyPrivileges(ctx, d.ViewRef(liveObjSchema, "v2").Securable(), change)
	})
	l.exec(stmt)
	l.ok("grant on view", d.ApplyPrivileges(ctx, d.ViewRef(liveObjSchema, "v3").Securable(), change))
	v2, err := d.ViewByName(ctx, liveObjSchema, "v2")
	l.ok("read view 2", err)
	v3, err := d.ViewByName(ctx, liveObjSchema, "v3")
	l.ok("read view 3", err)
	if !reflect.DeepEqual(v2.Privileges, v3.Privileges) || grantSet(v2.Privileges, l.own) == nil {
		t.Errorf("the captured GRANT run by hand gave %v, ApplyPrivileges %v", v2.Privileges, v3.Privileges)
	}
}

// privilegesOf is a family read's Privileges, through the field every
// family names alike.
func privilegesOf(x any, err error) ([]PrivilegeGrant, error) {
	if err != nil {
		return nil, err
	}
	return reflect.ValueOf(x).Elem().FieldByName("Privileges").Interface().([]PrivilegeGrant), nil
}

// grantOptions: a grantee passes a privilege on with its grant option; the
// grant option cannot be revoked without CASCADE while it has (2BP01), and
// CASCADE takes the onward grant too.
func (l *liveObjectWrites) grantOptions() {
	t, d, ctx := l.t, l.d, l.ctx
	sec := d.SequenceRef(liveObjSchema, "seq").Securable()
	l.ok("grant with grant option", d.ApplyPrivileges(ctx, sec, PrivilegeChange{Action: ActionGrant,
		Privileges: []Privilege{PrivilegeSelect}, Grantees: []string{l.own}, GrantOption: true}))
	l.exec("SET ROLE " + pgsql.QuoteIdent(l.own))
	l.exec("GRANT SELECT ON SEQUENCE " + pgsql.QualifiedName(liveObjSchema, "seq") + " TO " + pgsql.QuoteIdent(l.own2))
	l.exec("RESET ROLE")
	held := func() bool {
		return l.scalar("SELECT pg_catalog.has_sequence_privilege($1, $2::pg_catalog.regclass, 'SELECT')::text",
			l.own2, pgsql.QualifiedName(liveObjSchema, "seq")) == "true"
	}
	if !held() {
		t.Fatal("the onward grant did not take")
	}
	revoke := PrivilegeChange{Action: ActionRevoke, Privileges: []Privilege{PrivilegeSelect}, Grantees: []string{l.own}, GrantOption: true}
	l.wantCode("revoke a used grant option without CASCADE", d.ApplyPrivileges(ctx, sec, revoke), "2BP01")
	revoke.Cascade = true
	l.ok("revoke grant option CASCADE", d.ApplyPrivileges(ctx, sec, revoke))
	if held() {
		t.Error("CASCADE left the onward grant")
	}
	if l.scalar("SELECT pg_catalog.has_sequence_privilege($1, $2::pg_catalog.regclass, 'SELECT WITH GRANT OPTION')::text",
		l.own, pgsql.QualifiedName(liveObjSchema, "seq")) != "false" {
		t.Error("GRANT OPTION FOR left the grant option")
	}
}

// sharedObjects: the database, a tablespace and a parameter, through the
// Server — read back by hand (pgmo reads none of their ACLs yet).
func (l *liveObjectWrites) sharedObjects() {
	t, s, ctx := l.t, l.s, l.ctx
	q := pgsql.QuoteIdent
	param := fmt.Sprintf("pgmo_live_%d.x", l.stamp)
	l.t.Cleanup(func() {
		stmt := "REVOKE ALL ON PARAMETER " + q(param) + " FROM " + q(l.own2)
		if _, err := l.conn.Exec(context.Background(), stmt); err != nil {
			t.Errorf("%s: %v", stmt, err)
		}
	})
	acl := func(query string, args ...any) string {
		items := strings.Split(l.scalar(`SELECT COALESCE(pg_catalog.string_agg(CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(a.grantee)::text END
			|| ' ' || a.privilege_type || CASE WHEN a.is_grantable THEN '*' ELSE '' END, ','), '') FROM `+query, args...), ",")
		slices.Sort(items)
		return strings.Join(items, ",")
	}
	dbACL := func() string {
		return acl(`pg_catalog.pg_database d, pg_catalog.aclexplode(d.datacl) a WHERE d.datname = $1 AND a.grantee <> d.datdba`, l.db)
	}
	l.ok("revoke all on the database from PUBLIC", s.ApplyPrivileges(ctx, l.d.Securable(),
		PrivilegeChange{Action: ActionRevoke, All: true, Grantees: []string{""}}))
	l.ok("grant on the database", s.ApplyPrivileges(ctx, l.d.Securable(), PrivilegeChange{Action: ActionGrant,
		Privileges: []Privilege{PrivilegeConnect, PrivilegeTemporary}, Grantees: []string{l.own2}, GrantOption: true}))
	if got, want := dbACL(), l.own2+" CONNECT*,"+l.own2+" TEMPORARY*"; got != want {
		t.Errorf("database ACL %q, want %q", got, want)
	}
	// A tablespace of the test's own (in place), never a pre-existing one:
	// a revoke leaves an explicit ACL where there was none.
	tsName := fmt.Sprintf("pgmo_live_privw_%d T's", l.stamp)
	inPlace, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: "postgres",
		ApplicationName: "pgmo - live privilege writes (in place)", Params: map[string]string{"allow_in_place_tablespaces": "on"}})
	l.ok("connect in place", err)
	t.Cleanup(func() {
		_, _ = inPlace.exec(context.Background(), "DROP TABLESPACE IF EXISTS "+q(tsName))
		inPlace.Close()
	})
	tsp, err := inPlace.CreateTablespace(ctx, CreateTablespaceRequest{Name: tsName})
	l.ok("create tablespace", err)
	l.ok("grant on the tablespace", s.ApplyPrivileges(ctx, tsp.Securable(),
		PrivilegeChange{Action: ActionGrant, Privileges: []Privilege{PrivilegeCreate}, Grantees: []string{l.own2}}))
	ts, err := s.TablespaceByName(ctx, tsName)
	l.ok("read tablespace", err)
	if g := grantSet(ts.Privileges, l.own2); !reflect.DeepEqual(g, []string{"CREATE"}) {
		t.Errorf("tablespace: %s holds %v", l.own2, g)
	}
	l.ok("revoke on the tablespace", s.ApplyPrivileges(ctx, tsp.Securable(),
		PrivilegeChange{Action: ActionRevoke, All: true, Grantees: []string{l.own2}}))
	if ts, err = s.TablespaceByName(ctx, tsName); err != nil || grantSet(ts.Privileges, l.own2) != nil {
		t.Errorf("tablespace after the revoke: %+v, %v", ts, err)
	}
	l.ok("drop tablespace", tsp.Drop(ctx))
	paramACL := func() string {
		return acl(`pg_catalog.pg_parameter_acl p, pg_catalog.aclexplode(p.paracl) a WHERE p.parname = $1 AND a.grantee = $2::pg_catalog.regrole`,
			param, q(l.own2))
	}
	l.ok("grant on the parameter", s.ApplyPrivileges(ctx, Securable{Kind: SecurableParameter, Name: param},
		PrivilegeChange{Action: ActionGrant, Privileges: []Privilege{PrivilegeSet, PrivilegeAlterSystem}, Grantees: []string{l.own2}}))
	if got, want := paramACL(), l.own2+" ALTER SYSTEM,"+l.own2+" SET"; got != want {
		t.Errorf("parameter ACL %q, want %q", got, want)
	}
	l.ok("revoke on the parameter", s.ApplyPrivileges(ctx, Securable{Kind: SecurableParameter, Name: param},
		PrivilegeChange{Action: ActionRevoke, All: true, Grantees: []string{l.own2}}))
	// Back to the default ACL, the server drops the catalog row.
	if n := l.scalar("SELECT pg_catalog.count(*)::text FROM pg_catalog.pg_parameter_acl WHERE parname = $1", param); n != "0" {
		t.Errorf("%s rows of pg_parameter_acl left for %s", n, param)
	}

	// A large object, granted in its database.
	oid := l.scalar("SELECT pg_catalog.lo_create(0)::text")
	l.ok("grant on the large object", l.d.ApplyPrivileges(ctx, Securable{Kind: SecurableLargeObject, Name: oid},
		PrivilegeChange{Action: ActionGrant, Privileges: []Privilege{PrivilegeSelect, PrivilegeUpdate}, Grantees: []string{l.own2}}))
	if got, want := acl(`pg_catalog.pg_largeobject_metadata m, pg_catalog.aclexplode(m.lomacl) a WHERE m.oid = $1::pg_catalog.oid AND a.grantee <> m.lomowner`, oid),
		l.own2+" SELECT,"+l.own2+" UPDATE"; got != want {
		t.Errorf("large object ACL %q, want %q", got, want)
	}
}

// defaultPrivileges: entries read back by Database.DefaultPrivileges, and
// given to a table the role creates afterwards.
func (l *liveObjectWrites) defaultPrivileges() {
	t, d, ctx := l.t, l.d, l.ctx
	l.ok("grant create on the schema", d.ApplyPrivileges(ctx, d.SchemaRef(liveObjSchema).Securable(),
		PrivilegeChange{Action: ActionGrant, Privileges: []Privilege{PrivilegeCreate, PrivilegeUsage}, Grantees: []string{l.own}}))
	tables := PrivilegeChange{Action: ActionGrant, Privileges: []Privilege{PrivilegeSelect, PrivilegeInsert}, Grantees: []string{l.own2}, GrantOption: true}
	l.ok("default privileges on tables", d.AlterDefaultPrivileges(ctx,
		DefaultPrivilegeScope{Roles: []string{l.own}, Schemas: []string{liveObjSchema}, ObjectType: DefaultPrivilegeTables}, tables))
	l.ok("revoke execute on functions from PUBLIC", d.AlterDefaultPrivileges(ctx,
		DefaultPrivilegeScope{Roles: []string{l.own}, ObjectType: DefaultPrivilegeFunctions},
		PrivilegeChange{Action: ActionRevoke, Privileges: []Privilege{PrivilegeExecute}, Grantees: []string{""}}))
	loErr := d.AlterDefaultPrivileges(ctx, DefaultPrivilegeScope{Roles: []string{l.own}, ObjectType: DefaultPrivilegeLargeObjects},
		PrivilegeChange{Action: ActionGrant, Privileges: []Privilege{PrivilegeSelect}, Grantees: []string{l.own2}})
	if atLeast(l.num, PostgreSQL18) {
		l.ok("default privileges on large objects", loErr)
	} else if !errors.Is(loErr, ErrUnsupportedVersion) {
		t.Errorf("default privileges on large objects before 18: %v", loErr)
	}
	entries, err := d.DefaultPrivileges(ctx)
	l.ok("read default privileges", err)
	var sawTables, sawFunctions, sawLO bool
	for _, e := range entries {
		switch {
		case e.Role == l.own && e.Schema == liveObjSchema && e.ObjectType == DefaultPrivilegeTables:
			sawTables = reflect.DeepEqual(grantSet(e.Grants, l.own2), []string{"INSERT*", "SELECT*"})
		case e.Role == l.own && e.Schema == "" && e.ObjectType == DefaultPrivilegeFunctions:
			sawFunctions = grantSet(e.Grants, "") == nil && reflect.DeepEqual(grantSet(e.Grants, l.own), []string{"EXECUTE"})
		case e.Role == l.own && e.ObjectType == DefaultPrivilegeLargeObjects:
			sawLO = reflect.DeepEqual(grantSet(e.Grants, l.own2), []string{"SELECT"})
		}
	}
	if !sawTables || !sawFunctions || sawLO != atLeast(l.num, PostgreSQL18) {
		t.Errorf("default privileges read back %+v: tables %v, functions %v, large objects %v", entries, sawTables, sawFunctions, sawLO)
	}
	// A table own creates now starts with the entry's grants.
	l.exec("SET ROLE " + pgsql.QuoteIdent(l.own))
	l.exec("CREATE TABLE " + pgsql.QualifiedName(liveObjSchema, "later") + " (a int)")
	l.exec("RESET ROLE")
	later, err := d.TableByName(ctx, liveObjSchema, "later")
	l.ok("read later", err)
	if g := grantSet(later.Privileges, l.own2); !reflect.DeepEqual(g, []string{"INSERT*", "SELECT*"}) {
		t.Errorf("a table created after ALTER DEFAULT PRIVILEGES: %s holds %v", l.own2, g)
	}
	// Revoked again, the entry goes.
	tables.Action, tables.GrantOption = ActionRevoke, false
	l.ok("revoke default privileges", d.AlterDefaultPrivileges(ctx,
		DefaultPrivilegeScope{Roles: []string{l.own}, Schemas: []string{liveObjSchema}, ObjectType: DefaultPrivilegeTables}, tables))
	sch, err := d.SchemaByName(ctx, liveObjSchema)
	l.ok("read schema", err)
	if len(sch.DefaultPrivileges) != 0 {
		t.Errorf("the schema's default privileges after the revoke: %+v", sch.DefaultPrivileges)
	}
}

// rowSecurity: enable, force, policies created, altered, renamed, commented
// and dropped, each read back, and each one's effect on what a plain role
// sees.
func (l *liveObjectWrites) rowSecurity() {
	t, d, ctx := l.t, l.d, l.ctx
	q := pgsql.QuoteIdent
	table := pgsql.QualifiedName(liveObjSchema, "rls")
	l.exec("CREATE TABLE " + table + " (a int)")
	l.exec("INSERT INTO " + table + " SELECT pg_catalog.generate_series(1, 4)")
	l.exec("ALTER TABLE " + table + " OWNER TO " + q(l.own))
	tb, err := d.TableByName(ctx, liveObjSchema, "rls")
	l.ok("read rls", err)
	l.ok("grant usage on the schema", d.ApplyPrivileges(ctx, d.SchemaRef(liveObjSchema).Securable(), PrivilegeChange{Action: ActionGrant,
		Privileges: []Privilege{PrivilegeUsage}, Grantees: []string{l.own2}}))
	l.ok("grant select", d.ApplyPrivileges(ctx, tb.Securable(), PrivilegeChange{Action: ActionGrant,
		Privileges: []Privilege{PrivilegeSelect}, Grantees: []string{l.own2}}))
	seen := func(role string) int {
		t.Helper()
		l.exec("SET ROLE " + q(role))
		defer l.exec("RESET ROLE")
		n, err := strconv.Atoi(l.scalar("SELECT pg_catalog.count(*)::text FROM " + table))
		l.ok("count", err)
		return n
	}
	readBack := func() *Table {
		t.Helper()
		x, err := d.TableByName(ctx, liveObjSchema, "rls")
		l.ok("read rls", err)
		return x
	}
	l.ok("enable row security", tb.SetRowSecurity(ctx, true))
	if x := readBack(); !x.RowSecurity || !tb.RowSecurity || x.ForceRowSecurity {
		t.Errorf("after ENABLE: read %v/%v, mirrored %v", x.RowSecurity, x.ForceRowSecurity, tb.RowSecurity)
	}
	if n := seen(l.own2); n != 0 {
		t.Errorf("no policy: %s sees %d rows, want 0 (default deny)", l.own2, n)
	}

	req := CreatePolicyRequest{Schema: liveObjSchema, Table: "rls", Name: `P"o l`, Command: PolicySelect,
		Roles: []string{l.own2, l.own}, Using: "a > 2"}
	p, err := d.CreatePolicy(ctx, req)
	l.ok("create policy", err)
	if p.OID == 0 || p.Schema != liveObjSchema || p.Table != "rls" || p.Name != req.Name || p.Command != PolicySelect ||
		!p.Permissive || !reflect.DeepEqual(p.Roles, []string{l.own2, l.own}) || p.Using != "(a > 2)" || p.WithCheck != "" ||
		p.Database() != d {
		t.Errorf("policy read back %+v", p)
	}
	if n := seen(l.own2); n != 2 {
		t.Errorf("USING (a > 2): %s sees %d rows, want 2", l.own2, n)
	}
	// The captured CREATE POLICY run by hand makes the same policy.
	stmt := l.scripted(func(ctx context.Context) error {
		_, err := d.CreatePolicy(ctx, CreatePolicyRequest{Schema: liveObjSchema, Table: "rls", Name: "twin", Restrictive: true,
			Command: PolicyUpdate, Roles: []string{""}, Using: "a > 0", WithCheck: "a < 10"})
		return err
	})
	l.exec(stmt)
	twin, err := d.PolicyByName(ctx, liveObjSchema, "rls", "twin")
	l.ok("read twin", err)
	if twin.Permissive || twin.Command != PolicyUpdate || !reflect.DeepEqual(twin.Roles, []string{""}) ||
		twin.Using != "(a > 0)" || twin.WithCheck != "(a < 10)" {
		t.Errorf("the captured CREATE POLICY run by hand made %+v", twin)
	}
	l.ok("drop twin", twin.Drop(ctx))

	l.ok("alter policy", p.Alter(ctx, PolicyChanges{Roles: []string{""}, Using: new("a > 3")}))
	got, err := d.PolicyByName(ctx, liveObjSchema, "rls", p.Name)
	l.ok("read altered policy", err)
	if !reflect.DeepEqual(got.Roles, []string{""}) || got.Using != "(a > 3)" {
		t.Errorf("policy after ALTER %+v", got)
	}
	if n := seen(l.own2); n != 1 {
		t.Errorf("USING (a > 3) TO PUBLIC: %s sees %d rows, want 1", l.own2, n)
	}
	// A restrictive policy narrows the permissive one.
	_, err = d.CreatePolicy(ctx, CreatePolicyRequest{Schema: liveObjSchema, Table: "rls", Name: "only small", Restrictive: true, Using: "a < 4"})
	l.ok("create restrictive policy", err)
	if n := seen(l.own2); n != 0 {
		t.Errorf("with a restrictive policy: %s sees %d rows, want 0", l.own2, n)
	}
	l.ok("drop restrictive policy", d.PolicyRef(liveObjSchema, "rls", "only small").Drop(ctx))

	// FORCE applies the policies to the owner.
	if n := seen(l.own); n != 4 {
		t.Errorf("not forced: the owner sees %d rows, want 4", n)
	}
	l.ok("force row security", tb.SetForceRowSecurity(ctx, true))
	if x := readBack(); !x.ForceRowSecurity || !tb.ForceRowSecurity {
		t.Errorf("after FORCE: read %v, mirrored %v", x.ForceRowSecurity, tb.ForceRowSecurity)
	}
	if n := seen(l.own); n != 1 {
		t.Errorf("forced: the owner sees %d rows, want 1", n)
	}
	l.ok("no force", tb.SetForceRowSecurity(ctx, false))

	l.ok("rename policy", p.Rename(ctx, "renamed"))
	l.ok("comment policy", p.SetComment(ctx, "it's \\ here"))
	got, err = d.PolicyByName(ctx, liveObjSchema, "rls", "renamed")
	l.ok("read renamed policy", err)
	if got.Name != p.Name || got.Comment != p.Comment || got.OID != p.OID {
		t.Errorf("policy read back %+v, mirrored %+v", got, p)
	}
	l.ok("uncomment policy", p.SetComment(ctx, ""))
	ps, err := tb.Policies(ctx)
	l.ok("list policies", err)
	if len(ps) != 1 || ps[0].Comment != "" || ps[0].Table != "rls" || ps[0].Database() != d {
		t.Errorf("policies %+v", ps)
	}
	l.ok("drop policy", p.Drop(ctx))
	if _, err := d.PolicyByName(ctx, liveObjSchema, "rls", "renamed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("dropped policy: %v", err)
	}
	l.ok("disable row security", tb.SetRowSecurity(ctx, false))
	if x := readBack(); x.RowSecurity || tb.RowSecurity {
		t.Errorf("after DISABLE: read %v, mirrored %v", x.RowSecurity, tb.RowSecurity)
	}
	if n := seen(l.own2); n != 4 {
		t.Errorf("row security off: %s sees %d rows, want 4", l.own2, n)
	}
}

// privilegeRefusals: what pgmo leaves to the server is refused by it.
func (l *liveObjectWrites) privilegeRefusals() {
	d, ctx := l.d, l.ctx
	grant := PrivilegeChange{Action: ActionGrant, Privileges: []Privilege{PrivilegeSelect}, Grantees: []string{"pgmo no such role"}}
	l.wantCode("grant to a role that does not exist", d.ApplyPrivileges(ctx, d.TableRef(liveObjSchema, "T t").Securable(), grant), "42704")
	l.wantCode("a grant option to PUBLIC", d.ApplyPrivileges(ctx, d.TableRef(liveObjSchema, "T t").Securable(),
		PrivilegeChange{Action: ActionGrant, Privileges: []Privilege{PrivilegeSelect}, Grantees: []string{""}, GrantOption: true}), "0LP01")
	grant.Grantees = []string{l.own2}
	l.wantCode("a sequence named as a table's kind", d.ApplyPrivileges(ctx,
		Securable{Kind: SecurableSequence, Schema: liveObjSchema, Name: "T t"}, grant), "42809")
	l.wantCode("a function's wrong argument types", d.ApplyPrivileges(ctx,
		Securable{Kind: SecurableFunction, Schema: liveObjSchema, Name: "f", Arguments: []string{"text"}},
		PrivilegeChange{Action: ActionGrant, Privileges: []Privilege{PrivilegeExecute}, Grantees: []string{l.own2}}), "42883")
	l.wantCode("IN SCHEMA on default privileges for schemas", d.AlterDefaultPrivileges(ctx,
		DefaultPrivilegeScope{Schemas: []string{liveObjSchema}, ObjectType: DefaultPrivilegeSchemas},
		PrivilegeChange{Action: ActionGrant, Privileges: []Privilege{PrivilegeUsage}, Grantees: []string{l.own2}}), "0LP01")
	_, err := d.CreatePolicy(ctx, CreatePolicyRequest{Schema: liveObjSchema, Table: "v", Name: "p"})
	l.wantCode("a policy on a view", err, "42809")
	_, err = d.CreatePolicy(ctx, CreatePolicyRequest{Schema: liveObjSchema, Table: "rls", Name: "p", Command: PolicyInsert, Using: "true"})
	l.wantCode("USING on an INSERT policy", err, "42601")
	l.wantCode("row security on a view's name", d.TableRef(liveObjSchema, "v").SetRowSecurity(ctx, true), "42809")
}

// privilegeTransaction: writes inside a rolled-back InTransaction, on a
// Server connected to the throwaway database, leave nothing.
func (l *liveObjectWrites) privilegeTransaction() {
	t, ctx := l.t, l.ctx
	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: l.db, ApplicationName: "pgmo - live privilege writes (tx)"})
	l.ok("connect to the database", err)
	defer s.Close()
	d := s.DatabaseRef(l.db)
	sentinel := errors.New("roll back")
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		tb := d.TableRef(liveObjSchema, "T t")
		if err := errors.Join(
			d.ApplyPrivileges(ctx, tb.Securable(), PrivilegeChange{Action: ActionGrant, All: true, Grantees: []string{""}}),
			d.AlterDefaultPrivileges(ctx, DefaultPrivilegeScope{ObjectType: DefaultPrivilegeTypes},
				PrivilegeChange{Action: ActionRevoke, All: true, Grantees: []string{""}}),
			tb.SetRowSecurity(ctx, true),
		); err != nil {
			return err
		}
		_, err := d.CreatePolicy(ctx, CreatePolicyRequest{Schema: liveObjSchema, Table: "T t", Name: "in tx"})
		if err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTransaction: %v", err)
	}
	tb, err := d.TableByName(ctx, liveObjSchema, "T t")
	l.ok("read table", err)
	if grantSet(tb.Privileges, "") != nil || tb.RowSecurity {
		t.Errorf("after the rollback: PUBLIC holds %v, row security %v", grantSet(tb.Privileges, ""), tb.RowSecurity)
	}
	if _, err := d.PolicyByName(ctx, liveObjSchema, "T t", "in tx"); !errors.Is(err, ErrNotFound) {
		t.Errorf("policy after the rollback: %v", err)
	}
	entries, err := d.DefaultPrivileges(ctx)
	l.ok("read default privileges", err)
	for _, e := range entries {
		if e.ObjectType == DefaultPrivilegeTypes {
			t.Errorf("default privileges after the rollback: %+v", e)
		}
	}
}
