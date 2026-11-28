package pgmo_test

import (
	"context"
	"testing"

	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// The statements of P4.13.4 — GRANT and REVOKE on every securable kind,
// ALTER DEFAULT PRIVILEGES, row-level security and policies — whole, with
// quote-hostile names through every parameter that reaches the text. An
// object inside a database is written on that database's own session; a
// database, a tablespace and a parameter on the Server's.
func TestScriptPrivilegeWrites(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	d := s.DatabaseRef(hostileDB)
	grant := func(privs ...pgmo.Privilege) pgmo.PrivilegeChange {
		return pgmo.PrivilegeChange{Action: pgmo.ActionGrant, Privileges: privs, Grantees: []string{`R"r`}}
	}
	revoke := func(privs ...pgmo.Privilege) pgmo.PrivilegeChange {
		return pgmo.PrivilegeChange{Action: pgmo.ActionRevoke, Privileges: privs, Grantees: []string{`R"r`}}
	}
	apply := func(on pgmo.Securable, c pgmo.PrivilegeChange) func(context.Context) error {
		return func(ctx context.Context) error { return d.ApplyPrivileges(ctx, on, c) }
	}
	defaults := func(scope pgmo.DefaultPrivilegeScope, c pgmo.PrivilegeChange) func(context.Context) error {
		return func(ctx context.Context) error { return d.AlterDefaultPrivileges(ctx, scope, c) }
	}
	tbl := pgmo.Securable{Kind: pgmo.SecurableTable, Schema: `My "S"`, Name: "user"}
	tb := d.TableRef(`My "S"`, "user")
	pol := d.PolicyRef(`My "S"`, "user", `P"o`)

	runScriptCasesIn(t, f, hostileDB, []scriptCase{
		// GRANT and REVOKE on each kind in a database
		{"grant table", apply(tbl, grant(pgmo.PrivilegeSelect, pgmo.PrivilegeInsert, pgmo.PrivilegeMaintain)),
			`GRANT SELECT, INSERT, MAINTAIN ON TABLE "My ""S"""."user" TO "R""r"`},
		{"grant every table privilege", apply(tbl, grant(pgmo.PrivilegeSelect, pgmo.PrivilegeInsert, pgmo.PrivilegeUpdate,
			pgmo.PrivilegeDelete, pgmo.PrivilegeTruncate, pgmo.PrivilegeReferences, pgmo.PrivilegeTrigger)),
			`GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER ON TABLE "My ""S"""."user" TO "R""r"`},
		{"grant all, grant option, several grantees", apply(tbl, pgmo.PrivilegeChange{Action: pgmo.ActionGrant, All: true,
			Grantees: []string{"user", "", "current_user"}, GrantOption: true}),
			`GRANT ALL PRIVILEGES ON TABLE "My ""S"""."user" TO "user", PUBLIC, "current_user" WITH GRANT OPTION`},
		{"grant columns", apply(pgmo.Securable{Kind: pgmo.SecurableTable, Schema: "app", Name: "t", Columns: []string{"user", `C"c`}},
			grant(pgmo.PrivilegeSelect, pgmo.PrivilegeUpdate)),
			`GRANT SELECT ("user", "C""c"), UPDATE ("user", "C""c") ON TABLE app.t TO "R""r"`},
		{"grant all columns", apply(pgmo.Securable{Kind: pgmo.SecurableTable, Schema: "app", Name: "t", Columns: []string{"a"}},
			pgmo.PrivilegeChange{Action: pgmo.ActionGrant, All: true, Grantees: []string{""}}),
			`GRANT ALL PRIVILEGES (a) ON TABLE app.t TO PUBLIC`},
		{"revoke", apply(tbl, revoke(pgmo.PrivilegeDelete)), `REVOKE DELETE ON TABLE "My ""S"""."user" FROM "R""r"`},
		{"revoke grant option, cascade", apply(tbl, pgmo.PrivilegeChange{Action: pgmo.ActionRevoke,
			Privileges: []pgmo.Privilege{pgmo.PrivilegeSelect}, Grantees: []string{"", "a"}, GrantOption: true, Cascade: true}),
			`REVOKE GRANT OPTION FOR SELECT ON TABLE "My ""S"""."user" FROM PUBLIC, a CASCADE`},
		{"revoke all columns", apply(pgmo.Securable{Kind: pgmo.SecurableTable, Schema: "app", Name: "t", Columns: []string{"a"}},
			pgmo.PrivilegeChange{Action: pgmo.ActionRevoke, All: true, Grantees: []string{"r"}}),
			`REVOKE ALL PRIVILEGES (a) ON TABLE app.t FROM r`},
		{"all tables in schema", apply(pgmo.Securable{Kind: pgmo.SecurableTable, Schema: `My "S"`, AllInSchema: true}, grant(pgmo.PrivilegeSelect)),
			`GRANT SELECT ON ALL TABLES IN SCHEMA "My ""S""" TO "R""r"`},
		{"all sequences in schema", apply(pgmo.Securable{Kind: pgmo.SecurableSequence, Schema: "app", AllInSchema: true}, grant(pgmo.PrivilegeUsage)),
			`GRANT USAGE ON ALL SEQUENCES IN SCHEMA app TO "R""r"`},
		{"all functions in schema", apply(pgmo.Securable{Kind: pgmo.SecurableFunction, Schema: "app", AllInSchema: true}, revoke(pgmo.PrivilegeExecute)),
			`REVOKE EXECUTE ON ALL FUNCTIONS IN SCHEMA app FROM "R""r"`},
		{"all procedures in schema", apply(pgmo.Securable{Kind: pgmo.SecurableProcedure, Schema: "app", AllInSchema: true}, grant(pgmo.PrivilegeExecute)),
			`GRANT EXECUTE ON ALL PROCEDURES IN SCHEMA app TO "R""r"`},
		{"all routines in schema", apply(pgmo.Securable{Kind: pgmo.SecurableRoutine, Schema: "app", AllInSchema: true}, grant(pgmo.PrivilegeExecute)),
			`GRANT EXECUTE ON ALL ROUTINES IN SCHEMA app TO "R""r"`},
		{"sequence", apply(d.SequenceRef(`My "S"`, "user").Securable(), grant(pgmo.PrivilegeUsage, pgmo.PrivilegeSelect, pgmo.PrivilegeUpdate)),
			`GRANT USAGE, SELECT, UPDATE ON SEQUENCE "My ""S"""."user" TO "R""r"`},
		{"view", apply(d.ViewRef(`My "S"`, "v").Securable(), grant(pgmo.PrivilegeSelect)), `GRANT SELECT ON TABLE "My ""S""".v TO "R""r"`},
		{"materialized view", apply(d.MaterializedViewRef("app", "m").Securable(), grant(pgmo.PrivilegeSelect)), `GRANT SELECT ON TABLE app.m TO "R""r"`},
		{"foreign table", apply(d.ForeignTableRef("app", "ft").Securable(), grant(pgmo.PrivilegeSelect)), `GRANT SELECT ON TABLE app.ft TO "R""r"`},
		{"function", apply(d.FunctionRef(`My "S"`, "user", []string{"integer", `"My S".mood[]`}).Securable(), grant(pgmo.PrivilegeExecute)),
			`GRANT EXECUTE ON FUNCTION "My ""S"""."user"(integer, "My S".mood[]) TO "R""r"`},
		{"function without arguments", apply(d.FunctionRef("app", "f", nil).Securable(), revoke(pgmo.PrivilegeExecute)),
			`REVOKE EXECUTE ON FUNCTION app.f() FROM "R""r"`},
		{"procedure", apply(d.ProcedureRef("app", `P"r`, []string{"text"}).Securable(), grant(pgmo.PrivilegeExecute)),
			`GRANT EXECUTE ON PROCEDURE app."P""r"(text) TO "R""r"`},
		{"aggregate", apply(d.AggregateRef("app", "agg", []string{"double precision", "anyelement"}).Securable(), grant(pgmo.PrivilegeExecute)),
			`GRANT EXECUTE ON FUNCTION app.agg(double precision, anyelement) TO "R""r"`},
		{"routine", apply(pgmo.Securable{Kind: pgmo.SecurableRoutine, Schema: "app", Name: "r", Arguments: []string{}}, grant(pgmo.PrivilegeExecute)),
			`GRANT EXECUTE ON ROUTINE app.r() TO "R""r"`},
		{"schema", apply(d.SchemaRef(`My "S"`).Securable(), grant(pgmo.PrivilegeUsage, pgmo.PrivilegeCreate)),
			`GRANT USAGE, CREATE ON SCHEMA "My ""S""" TO "R""r"`},
		{"type", apply(d.TypeRef(`My "S"`, "user").Securable(), grant(pgmo.PrivilegeUsage)), `GRANT USAGE ON TYPE "My ""S"""."user" TO "R""r"`},
		{"domain", apply(d.DomainRef("app", "d").Securable(), revoke(pgmo.PrivilegeUsage)), `REVOKE USAGE ON DOMAIN app.d FROM "R""r"`},
		{"language", apply(d.LanguageRef(`L"g`).Securable(), grant(pgmo.PrivilegeUsage)), `GRANT USAGE ON LANGUAGE "L""g" TO "R""r"`},
		{"foreign data wrapper", apply(d.ForeignDataWrapperRef(`W"w`).Securable(), grant(pgmo.PrivilegeUsage)),
			`GRANT USAGE ON FOREIGN DATA WRAPPER "W""w" TO "R""r"`},
		{"foreign server", apply(d.ForeignServerRef(`S"s`).Securable(), grant(pgmo.PrivilegeUsage)),
			`GRANT USAGE ON FOREIGN SERVER "S""s" TO "R""r"`},
		{"large object", apply(pgmo.Securable{Kind: pgmo.SecurableLargeObject, Name: "4294967295"}, grant(pgmo.PrivilegeSelect, pgmo.PrivilegeUpdate)),
			`GRANT SELECT, UPDATE ON LARGE OBJECT 4294967295 TO "R""r"`},

		// ALTER DEFAULT PRIVILEGES
		{"defaults database-wide", defaults(pgmo.DefaultPrivilegeScope{ObjectType: pgmo.DefaultPrivilegeFunctions},
			pgmo.PrivilegeChange{Action: pgmo.ActionRevoke, Privileges: []pgmo.Privilege{pgmo.PrivilegeExecute}, Grantees: []string{""}}),
			`ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC`},
		{"defaults for roles in schemas", defaults(pgmo.DefaultPrivilegeScope{Roles: []string{`O"w`, "user"}, Schemas: []string{`My "S"`, "app"},
			ObjectType: pgmo.DefaultPrivilegeTables}, pgmo.PrivilegeChange{Action: pgmo.ActionGrant,
			Privileges: []pgmo.Privilege{pgmo.PrivilegeSelect, pgmo.PrivilegeMaintain}, Grantees: []string{`R"r`, ""}, GrantOption: true}),
			`ALTER DEFAULT PRIVILEGES FOR ROLE "O""w", "user" IN SCHEMA "My ""S""", app GRANT SELECT, MAINTAIN ON TABLES TO "R""r", PUBLIC WITH GRANT OPTION`},
		{"defaults sequences", defaults(pgmo.DefaultPrivilegeScope{ObjectType: pgmo.DefaultPrivilegeSequences}, grant(pgmo.PrivilegeUsage)),
			`ALTER DEFAULT PRIVILEGES GRANT USAGE ON SEQUENCES TO "R""r"`},
		{"defaults types, all", defaults(pgmo.DefaultPrivilegeScope{ObjectType: pgmo.DefaultPrivilegeTypes},
			pgmo.PrivilegeChange{Action: pgmo.ActionRevoke, All: true, Grantees: []string{""}, GrantOption: true, Cascade: true}),
			`ALTER DEFAULT PRIVILEGES REVOKE GRANT OPTION FOR ALL PRIVILEGES ON TYPES FROM PUBLIC CASCADE`},
		{"defaults schemas", defaults(pgmo.DefaultPrivilegeScope{Roles: []string{"o"}, ObjectType: pgmo.DefaultPrivilegeSchemas}, grant(pgmo.PrivilegeCreate)),
			`ALTER DEFAULT PRIVILEGES FOR ROLE o GRANT CREATE ON SCHEMAS TO "R""r"`},
		{"defaults large objects", defaults(pgmo.DefaultPrivilegeScope{ObjectType: pgmo.DefaultPrivilegeLargeObjects}, grant(pgmo.PrivilegeSelect)),
			`ALTER DEFAULT PRIVILEGES GRANT SELECT ON LARGE OBJECTS TO "R""r"`},

		// Row-level security
		{"enable row security", func(ctx context.Context) error { return tb.SetRowSecurity(ctx, true) },
			`ALTER TABLE "My ""S"""."user" ENABLE ROW LEVEL SECURITY`},
		{"disable row security", func(ctx context.Context) error { return tb.SetRowSecurity(ctx, false) },
			`ALTER TABLE "My ""S"""."user" DISABLE ROW LEVEL SECURITY`},
		{"force row security", func(ctx context.Context) error { return tb.SetForceRowSecurity(ctx, true) },
			`ALTER TABLE "My ""S"""."user" FORCE ROW LEVEL SECURITY`},
		{"no force row security", func(ctx context.Context) error { return tb.SetForceRowSecurity(ctx, false) },
			`ALTER TABLE "My ""S"""."user" NO FORCE ROW LEVEL SECURITY`},

		// Policies
		{"policy create", func(ctx context.Context) error {
			_, err := d.CreatePolicy(ctx, pgmo.CreatePolicyRequest{Schema: `My "S"`, Table: "user", Name: `P"o`})
			return err
		}, `CREATE POLICY "P""o" ON "My ""S"""."user"`},
		{"policy create, every option", func(ctx context.Context) error {
			_, err := d.CreatePolicy(ctx, pgmo.CreatePolicyRequest{Schema: "app", Table: "t", Name: "p", Restrictive: true,
				Command: pgmo.PolicyUpdate, Roles: []string{`R"r`, "", "current_user"},
				Using: "owner = current_user AND note <> 'o''brien'", WithCheck: "a > 0"})
			return err
		}, `CREATE POLICY p ON app.t AS RESTRICTIVE FOR UPDATE TO "R""r", PUBLIC, "current_user"` +
			` USING (owner = current_user AND note <> 'o''brien') WITH CHECK (a > 0)`},
		{"policy create, select", func(ctx context.Context) error {
			_, err := d.CreatePolicy(ctx, pgmo.CreatePolicyRequest{Schema: "app", Table: "t", Name: "p", Command: pgmo.PolicySelect, Using: "true"})
			return err
		}, `CREATE POLICY p ON app.t FOR SELECT USING (true)`},
		{"policy create, insert", func(ctx context.Context) error {
			_, err := d.CreatePolicy(ctx, pgmo.CreatePolicyRequest{Schema: "app", Table: "t", Name: "p", Command: pgmo.PolicyInsert, WithCheck: "b"})
			return err
		}, `CREATE POLICY p ON app.t FOR INSERT WITH CHECK (b)`},
		{"policy create, delete", func(ctx context.Context) error {
			_, err := d.CreatePolicy(ctx, pgmo.CreatePolicyRequest{Schema: "app", Table: "t", Name: "p", Command: pgmo.PolicyDelete, Roles: []string{""}})
			return err
		}, `CREATE POLICY p ON app.t FOR DELETE TO PUBLIC`},
		{"policy create, all", func(ctx context.Context) error {
			_, err := d.CreatePolicy(ctx, pgmo.CreatePolicyRequest{Schema: "app", Table: "t", Name: "p", Command: pgmo.PolicyAll})
			return err
		}, `CREATE POLICY p ON app.t FOR ALL`},
		{"policy alter", func(ctx context.Context) error {
			return pol.Alter(ctx, pgmo.PolicyChanges{Roles: []string{"", "user"}, Using: new("a > 'o''b'"), WithCheck: new("true")})
		}, `ALTER POLICY "P""o" ON "My ""S"""."user" TO PUBLIC, "user" USING (a > 'o''b') WITH CHECK (true)`},
		{"policy alter roles", func(ctx context.Context) error { return pol.Alter(ctx, pgmo.PolicyChanges{Roles: []string{`R"r`}}) },
			`ALTER POLICY "P""o" ON "My ""S"""."user" TO "R""r"`},
		{"policy alter using", func(ctx context.Context) error { return pol.Alter(ctx, pgmo.PolicyChanges{Using: new("x")}) },
			`ALTER POLICY "P""o" ON "My ""S"""."user" USING (x)`},
		{"policy rename", func(ctx context.Context) error { return d.PolicyRef(`My "S"`, "user", `P"o`).Rename(ctx, "user") },
			`ALTER POLICY "P""o" ON "My ""S"""."user" RENAME TO "user"`},
		{"policy comment", func(ctx context.Context) error { return pol.SetComment(ctx, "o'brien") },
			`COMMENT ON POLICY "P""o" ON "My ""S"""."user" IS 'o''brien'`},
		{"policy uncomment", func(ctx context.Context) error { return pol.SetComment(ctx, "") },
			`COMMENT ON POLICY "P""o" ON "My ""S"""."user" IS NULL`},
		{"policy drop", func(ctx context.Context) error { return pol.Drop(ctx) }, `DROP POLICY "P""o" ON "My ""S"""."user"`},
	})

	// The shared objects, on the Server's own session.
	cluster := func(on pgmo.Securable, c pgmo.PrivilegeChange) func(context.Context) error {
		return func(ctx context.Context) error { return s.ApplyPrivileges(ctx, on, c) }
	}
	runScriptCases(t, f, []scriptCase{
		{"database", cluster(d.Securable(), grant(pgmo.PrivilegeCreate, pgmo.PrivilegeConnect, pgmo.PrivilegeTemporary)),
			`GRANT CREATE, CONNECT, TEMPORARY ON DATABASE "My ""DB""" TO "R""r"`},
		{"database revoke all", cluster(d.Securable(), pgmo.PrivilegeChange{Action: pgmo.ActionRevoke, All: true, Grantees: []string{""}}),
			`REVOKE ALL PRIVILEGES ON DATABASE "My ""DB""" FROM PUBLIC`},
		{"tablespace", cluster(s.TablespaceRef(`T"s`).Securable(), grant(pgmo.PrivilegeCreate)),
			`GRANT CREATE ON TABLESPACE "T""s" TO "R""r"`},
		{"parameter", cluster(pgmo.Securable{Kind: pgmo.SecurableParameter, Name: "work_mem"}, grant(pgmo.PrivilegeSet, pgmo.PrivilegeAlterSystem)),
			`GRANT SET, ALTER SYSTEM ON PARAMETER work_mem TO "R""r"`},
		{"custom parameter", cluster(pgmo.Securable{Kind: pgmo.SecurableParameter, Name: `plpgsql.extra"x`}, revoke(pgmo.PrivilegeSet)),
			`REVOKE SET ON PARAMETER "plpgsql.extra""x" FROM "R""r"`},
	})
}
