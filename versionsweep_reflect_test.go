// The reflective half of the live version sweep (live_versionsweep_test.go)
// and the guards that keep the sweep complete, kept out of the livedb build so
// they run in go test ./...: the write guard reflectSweep runs under
// (TestReflectSweepRefusesWrites), and the checks that every function sending
// a read, and every exported context-first method, is either swept or says
// why not (TestSweepCoversEveryRead, TestSweepReachesEveryContextMethod).
package pgmo

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/radix29/pgmo/pgsql"
)

// sweptTypes are the receivers the sweep walks, by the name its labels and
// keys use. A family type (Database, Table, Function…) joins here when it
// lands, and its listing is then swept reflectively.
var sweptTypes = map[string]reflect.Type{
	"Server":   reflect.TypeFor[*Server](),
	"Database": reflect.TypeFor[*Database](),
	"Role":     reflect.TypeFor[*Role](),
	"Schema":   reflect.TypeFor[*Schema](),
	"Table":    reflect.TypeFor[*Table](),

	"View":             reflect.TypeFor[*View](),
	"MaterializedView": reflect.TypeFor[*MaterializedView](),

	"Sequence": reflect.TypeFor[*Sequence](),

	"Type":   reflect.TypeFor[*Type](),
	"Domain": reflect.TypeFor[*Domain](),

	"Trigger":      reflect.TypeFor[*Trigger](),
	"EventTrigger": reflect.TypeFor[*EventTrigger](),
	"Policy":       reflect.TypeFor[*Policy](),

	"Extension": reflect.TypeFor[*Extension](),

	"Tablespace":              reflect.TypeFor[*Tablespace](),
	"Collation":               reflect.TypeFor[*Collation](),
	"TextSearchParser":        reflect.TypeFor[*TextSearchParser](),
	"TextSearchTemplate":      reflect.TypeFor[*TextSearchTemplate](),
	"TextSearchDictionary":    reflect.TypeFor[*TextSearchDictionary](),
	"TextSearchConfiguration": reflect.TypeFor[*TextSearchConfiguration](),
	"Language":                reflect.TypeFor[*Language](),
	"Cast":                    reflect.TypeFor[*Cast](),
	"Operator":                reflect.TypeFor[*Operator](),

	"ForeignDataWrapper": reflect.TypeFor[*ForeignDataWrapper](),
	"ForeignServer":      reflect.TypeFor[*ForeignServer](),
	"UserMapping":        reflect.TypeFor[*UserMapping](),
	"ForeignTable":       reflect.TypeFor[*ForeignTable](),

	"Function":  reflect.TypeFor[*Function](),
	"Procedure": reflect.TypeFor[*Procedure](),
	"Aggregate": reflect.TypeFor[*Aggregate](),
}

// sweepSkip names the context-only methods the sweep must not call because
// they write, "<type>.<method>" → what the write does. A method classified wrongly has to be
// visible at the point it is skipped, so the list lives here, in the test;
// checkSweepKeys fails on an entry naming no method, so it cannot rot into
// names that skip nothing.
var sweepSkip = map[string]string{
	"Database.ResetAllParameters": "ALTER DATABASE … RESET ALL",
	"Role.Drop":                   "DROP ROLE",
	"Role.ClearPassword":          "ALTER ROLE … PASSWORD NULL",
	"Role.ResetAllParameters":     "ALTER ROLE … RESET ALL",
	"Tablespace.Drop":             "DROP TABLESPACE",
}

// sweepNotReads names the exported context-first methods reflectSweep cannot
// call that are not reads either, with why. Every other such method is a read
// the live sweep drives by hand, and so is in sweepMustCall
// (TestSweepReachesEveryContextMethod).
var sweepNotReads = map[string]string{
	"Server.InTransaction": "runs the caller's function in a transaction; sends nothing of its own but BEGIN/COMMIT",
	"Server.OpenSession":   "dials a session for the caller; sends no statement",

	// Database writes (database_write.go): CreateDatabase's read-back is
	// DatabaseByName, swept on its own.
	"Server.CreateDatabase":   "CREATE DATABASE, read back by DatabaseByName",
	"Database.Drop":           "DROP DATABASE",
	"Database.Rename":         "ALTER DATABASE … RENAME TO",
	"Database.SetOwner":       "ALTER DATABASE … OWNER TO",
	"Database.SetTablespace":  "ALTER DATABASE … SET TABLESPACE",
	"Database.Alter":          "ALTER DATABASE … WITH",
	"Database.SetParameter":   "ALTER DATABASE … SET",
	"Database.ResetParameter": "ALTER DATABASE … RESET",

	// Role writes (role_write.go): CreateRole's read-back is RoleByName,
	// and a password's scram_iterations read passwordVerifier, both swept
	// on their own.
	"Server.CreateRole":               "CREATE ROLE, read back by RoleByName",
	"Role.Rename":                     "ALTER ROLE … RENAME TO",
	"Role.Alter":                      "ALTER ROLE … WITH",
	"Role.SetPassword":                "ALTER ROLE … PASSWORD",
	"Role.SetParameter":               "ALTER ROLE … SET",
	"Role.SetDatabaseParameter":       "ALTER ROLE … IN DATABASE … SET",
	"Role.ResetParameter":             "ALTER ROLE … RESET",
	"Role.ResetDatabaseParameter":     "ALTER ROLE … IN DATABASE … RESET",
	"Role.ResetAllDatabaseParameters": "ALTER ROLE … IN DATABASE … RESET ALL",
	"Role.AddMember":                  "GRANT role TO member",
	"Role.RemoveMember":               "REVOKE role FROM member",

	// Objects owned (role_write.go, P4.14).
	"Database.ReassignOwned": "REASSIGN OWNED BY … TO …",
	"Database.DropOwned":     "DROP OWNED BY …",

	// P4.13.3 (object_write.go and the *_write.go files): every CreateX's
	// read-back is its family's XByName, swept on its own.
	"Database.CreateCompositeType":     "CREATE TYPE … AS (…), read back by TypeByName",
	"Database.CreateDomain":            "CREATE DOMAIN, read back by DomainByName",
	"Database.CreateEnumType":          "CREATE TYPE … AS ENUM, read back by TypeByName",
	"Database.CreateExtension":         "CREATE EXTENSION, read back by ExtensionByName",
	"Database.CreateFunction":          "CREATE FUNCTION, read back by FunctionByName",
	"Database.CreateMaterializedView":  "CREATE MATERIALIZED VIEW, read back by MaterializedViewByName",
	"Database.CreateProcedure":         "CREATE PROCEDURE, read back by ProcedureByName",
	"Database.CreateRangeType":         "CREATE TYPE … AS RANGE, read back by TypeByName",
	"Database.CreateSchema":            "CREATE SCHEMA, read back by SchemaByName",
	"Database.CreateSequence":          "CREATE SEQUENCE, read back by SequenceByName",
	"Database.CreateTable":             "CREATE TABLE, read back by TableByName",
	"Database.CreateTrigger":           "CREATE TRIGGER, read back by TriggerByName",
	"Database.CreateView":              "CREATE VIEW, read back by ViewByName",
	"Database.SetComment":              "COMMENT ON DATABASE",
	"Domain.AddConstraint":             "ALTER DOMAIN … ADD",
	"Domain.Drop":                      "DROP DOMAIN",
	"Domain.DropConstraint":            "ALTER DOMAIN … DROP CONSTRAINT",
	"Domain.Rename":                    "ALTER DOMAIN … RENAME TO",
	"Domain.SetComment":                "COMMENT ON DOMAIN",
	"Domain.SetDefault":                "ALTER DOMAIN … SET/DROP DEFAULT",
	"Domain.SetNotNull":                "ALTER DOMAIN … SET/DROP NOT NULL",
	"Domain.SetOwner":                  "ALTER DOMAIN … OWNER TO",
	"Domain.SetSchema":                 "ALTER DOMAIN … SET SCHEMA",
	"Domain.ValidateConstraint":        "ALTER DOMAIN … VALIDATE CONSTRAINT",
	"Database.CreateEventTrigger":      "CREATE EVENT TRIGGER, read back by EventTriggerByName",
	"EventTrigger.Drop":                "DROP EVENT TRIGGER",
	"EventTrigger.Rename":              "ALTER EVENT TRIGGER … RENAME TO",
	"EventTrigger.SetComment":          "COMMENT ON EVENT TRIGGER",
	"EventTrigger.SetEnabled":          "ALTER EVENT TRIGGER … ENABLE/DISABLE",
	"EventTrigger.SetOwner":            "ALTER EVENT TRIGGER … OWNER TO",
	"Extension.Drop":                   "DROP EXTENSION",
	"Extension.SetComment":             "COMMENT ON EXTENSION",
	"Extension.SetSchema":              "ALTER EXTENSION … SET SCHEMA",
	"Extension.Update":                 "ALTER EXTENSION … UPDATE",
	"Function.Drop":                    "DROP FUNCTION",
	"Function.Rename":                  "ALTER FUNCTION … RENAME TO",
	"Function.SetComment":              "COMMENT ON FUNCTION",
	"Function.SetOwner":                "ALTER FUNCTION … OWNER TO",
	"Function.SetSchema":               "ALTER FUNCTION … SET SCHEMA",
	"MaterializedView.CreateIndex":     "CREATE INDEX",
	"MaterializedView.Drop":            "DROP MATERIALIZED VIEW",
	"MaterializedView.DropIndex":       "DROP INDEX",
	"MaterializedView.Refresh":         "REFRESH MATERIALIZED VIEW",
	"MaterializedView.Rename":          "ALTER MATERIALIZED VIEW … RENAME TO",
	"MaterializedView.RenameIndex":     "ALTER INDEX … RENAME TO",
	"MaterializedView.SetComment":      "COMMENT ON MATERIALIZED VIEW",
	"MaterializedView.SetIndexComment": "COMMENT ON INDEX",
	"MaterializedView.SetOwner":        "ALTER MATERIALIZED VIEW … OWNER TO",
	"MaterializedView.SetSchema":       "ALTER MATERIALIZED VIEW … SET SCHEMA",
	"Procedure.Drop":                   "DROP PROCEDURE",
	"Procedure.Rename":                 "ALTER PROCEDURE … RENAME TO",
	"Procedure.SetComment":             "COMMENT ON PROCEDURE",
	"Procedure.SetOwner":               "ALTER PROCEDURE … OWNER TO",
	"Procedure.SetSchema":              "ALTER PROCEDURE … SET SCHEMA",
	"Role.SetComment":                  "COMMENT ON ROLE",
	"Schema.Drop":                      "DROP SCHEMA",
	"Schema.Rename":                    "ALTER SCHEMA … RENAME TO",
	"Schema.SetComment":                "COMMENT ON SCHEMA",
	"Schema.SetOwner":                  "ALTER SCHEMA … OWNER TO",
	"Sequence.Alter":                   "ALTER SEQUENCE",
	"Sequence.Drop":                    "DROP SEQUENCE",
	"Sequence.Rename":                  "ALTER SEQUENCE … RENAME TO",
	"Sequence.SetComment":              "COMMENT ON SEQUENCE",
	"Sequence.SetOwner":                "ALTER SEQUENCE … OWNER TO",
	"Sequence.SetSchema":               "ALTER SEQUENCE … SET SCHEMA",
	"Server.CreateTablespace":          "CREATE TABLESPACE, read back by TablespaceByName",
	"Table.AddColumn":                  "ALTER TABLE … ADD COLUMN",
	"Table.AddConstraint":              "ALTER TABLE … ADD",
	"Table.AlterColumn":                "ALTER TABLE … ALTER COLUMN",
	"Table.CreateIndex":                "CREATE INDEX",
	"Table.Drop":                       "DROP TABLE",
	"Table.DropColumn":                 "ALTER TABLE … DROP COLUMN",
	"Table.DropConstraint":             "ALTER TABLE … DROP CONSTRAINT",
	"Table.DropIndex":                  "DROP INDEX",
	"Table.Rename":                     "ALTER TABLE … RENAME TO",
	"Table.RenameColumn":               "ALTER TABLE … RENAME COLUMN",
	"Table.RenameConstraint":           "ALTER TABLE … RENAME CONSTRAINT",
	"Table.RenameIndex":                "ALTER INDEX … RENAME TO",
	"Table.SetColumnComment":           "COMMENT ON COLUMN",
	"Table.SetComment":                 "COMMENT ON TABLE",
	"Table.SetConstraintComment":       "COMMENT ON CONSTRAINT",
	"Table.SetIndexComment":            "COMMENT ON INDEX",
	"Table.SetOwner":                   "ALTER TABLE … OWNER TO",
	"Table.SetSchema":                  "ALTER TABLE … SET SCHEMA",
	"Tablespace.Rename":                "ALTER TABLESPACE … RENAME TO",
	"Tablespace.ResetOptions":          "ALTER TABLESPACE … RESET (…)",
	"Tablespace.SetComment":            "COMMENT ON TABLESPACE",
	"Tablespace.SetOptions":            "ALTER TABLESPACE … SET (…)",
	"Tablespace.SetOwner":              "ALTER TABLESPACE … OWNER TO",
	"Table.ValidateConstraint":         "ALTER TABLE … VALIDATE CONSTRAINT",
	"Trigger.Drop":                     "DROP TRIGGER",
	// P4.13.4 (privilege_write.go, policy_write.go): CreatePolicy's
	// read-back is PolicyByName, swept on its own.
	"Database.ApplyPrivileges":        "GRANT/REVOKE … ON an object in the database",
	"Server.ApplyPrivileges":          "GRANT/REVOKE … ON DATABASE/TABLESPACE/PARAMETER",
	"Database.AlterDefaultPrivileges": "ALTER DEFAULT PRIVILEGES",
	"Database.CreatePolicy":           "CREATE POLICY, read back by PolicyByName",
	"Table.SetRowSecurity":            "ALTER TABLE … ENABLE/DISABLE ROW LEVEL SECURITY",
	"Table.SetForceRowSecurity":       "ALTER TABLE … [NO] FORCE ROW LEVEL SECURITY",
	"Policy.Alter":                    "ALTER POLICY … ON",
	"Policy.Rename":                   "ALTER POLICY … ON … RENAME TO",
	"Policy.SetComment":               "COMMENT ON POLICY",
	"Trigger.Rename":                  "ALTER TRIGGER … RENAME TO",
	"Trigger.SetComment":              "COMMENT ON TRIGGER",
	"Trigger.SetEnabled":              "ALTER TABLE … ENABLE/DISABLE TRIGGER",
	"Type.AddEnumValue":               "ALTER TYPE … ADD VALUE",
	"Type.Drop":                       "DROP TYPE",
	"Type.Rename":                     "ALTER TYPE … RENAME TO",
	"Type.RenameEnumValue":            "ALTER TYPE … RENAME VALUE",
	"Type.SetComment":                 "COMMENT ON TYPE",
	"Type.SetOwner":                   "ALTER TYPE … OWNER TO",
	"Type.SetSchema":                  "ALTER TYPE … SET SCHEMA",
	"View.Drop":                       "DROP VIEW",
	"View.Rename":                     "ALTER VIEW … RENAME TO",
	"View.SetComment":                 "COMMENT ON VIEW",
	"View.SetOwner":                   "ALTER VIEW … OWNER TO",
	"View.SetSchema":                  "ALTER VIEW … SET SCHEMA",
}

// sweepReaders maps every function that sends a read — calls one of
// readHelpers — to the sweep label that drives it, "<type>.<method>" for a
// method. TestSweepCoversEveryRead finds them in the source and fails on one
// missing here, so a new read cannot go unswept; each label must be in
// sweepMustCall, which the live sweep checks it called.
var sweepReaders = map[string]string{
	"newServer":                  "NewServer",
	"Server.Info":                "Server.Info",
	"Server.Capabilities":        "Server.Capabilities",
	"Server.DatabasePrivileges":  "Server.DatabasePrivileges",
	"Server.passwordVerifier":    "Server.passwordVerifier",
	"routineByName":              "routineByName",
	"Server.Databases":           "Server.Databases",
	"Server.DatabaseByName":      "Server.DatabaseByName",
	"Database.Size":              "Database.Size",
	"Server.Roles":               "Server.Roles",
	"Server.RoleByName":          "Server.RoleByName",
	"Database.Schemas":           "Database.Schemas",
	"Database.SchemaByName":      "Database.SchemaByName",
	"Database.DefaultPrivileges": "Database.DefaultPrivileges",
	"Database.Tables":            "Database.Tables",
	"Database.TableByName":       "Database.TableByName",
	"Table.Columns":              "Table.Columns",
	"Table.Constraints":          "Table.Constraints",
	"Table.Indexes":              "Table.Indexes",
	"relationTriggers":           "Table.Triggers",
	"Table.Policies":             "Table.Policies",
	"Database.PolicyByName":      "Database.PolicyByName",
	"Table.Children":             "Table.Children",
	"Table.Size":                 "Table.Size",

	"Database.Views":                  "Database.Views",
	"Database.ViewByName":             "Database.ViewByName",
	"View.Columns":                    "View.Columns",
	"Database.MaterializedViews":      "Database.MaterializedViews",
	"Database.MaterializedViewByName": "Database.MaterializedViewByName",
	"MaterializedView.Columns":        "MaterializedView.Columns",
	"MaterializedView.Indexes":        "MaterializedView.Indexes",
	"MaterializedView.Size":           "MaterializedView.Size",

	"Database.Sequences":      "Database.Sequences",
	"Database.SequenceByName": "Database.SequenceByName",
	"Sequence.LastValue":      "Sequence.LastValue",

	"Database.Types":        "Database.Types",
	"Database.TypeByName":   "Database.TypeByName",
	"Database.Domains":      "Database.Domains",
	"Database.DomainByName": "Database.DomainByName",

	"Database.Triggers":           "Database.Triggers",
	"Database.TriggerByName":      "Database.TriggerByName",
	"Database.EventTriggers":      "Database.EventTriggers",
	"Database.EventTriggerByName": "Database.EventTriggerByName",

	"Database.Extensions":               "Database.Extensions",
	"Database.ExtensionByName":          "Database.ExtensionByName",
	"Extension.Members":                 "Extension.Members",
	"Database.AvailableExtensions":      "Database.AvailableExtensions",
	"Database.AvailableExtensionByName": "Database.AvailableExtensionByName",

	"Server.Tablespaces":       "Server.Tablespaces",
	"Server.TablespaceByName":  "Server.TablespaceByName",
	"Tablespace.Size":          "Tablespace.Size",
	"Database.Collations":      "Database.Collations",
	"Database.CollationByName": "Database.CollationByName",
	"Database.Languages":       "Database.Languages",
	"Database.LanguageByName":  "Database.LanguageByName",
	"Database.Casts":           "Database.Casts",
	"Database.CastByTypes":     "Database.CastByTypes",
	"Database.Operators":       "Database.Operators",
	"Database.OperatorByName":  "Database.OperatorByName",

	"Database.ForeignDataWrappers":      "Database.ForeignDataWrappers",
	"Database.ForeignDataWrapperByName": "Database.ForeignDataWrapperByName",
	"ForeignDataWrapper.Servers":        "ForeignDataWrapper.Servers",
	"Database.ForeignServers":           "Database.ForeignServers",
	"Database.ForeignServerByName":      "Database.ForeignServerByName",
	"ForeignServer.UserMappings":        "ForeignServer.UserMappings",
	"Database.UserMappings":             "Database.UserMappings",
	"Database.UserMappingByName":        "Database.UserMappingByName",
	"Database.ForeignTables":            "Database.ForeignTables",
	"Database.ForeignTableByName":       "Database.ForeignTableByName",
	"ForeignTable.Columns":              "ForeignTable.Columns",
	"ForeignTable.Constraints":          "ForeignTable.Constraints",

	// The text search families' shared readers, each driven through every
	// family (sweepMustCall lists all four).
	"listTextSearch":   "Database.TextSearchConfigurations",
	"textSearchByName": "Database.TextSearchConfigurationByName",

	// The routine families' shared readers, each driven through every
	// family (sweepMustCall lists all three).
	"listRoutines":        "Database.Functions",
	"routineFamilyByName": "Database.FunctionByName",
	"readBody":            "Function.Body",

	// Dependencies and search (P4.14): the four dependency reads share one
	// reader, driven through each (sweepMustCall lists all four).
	"readDependencies":           "Database.Dependencies",
	"Server.DependentDatabases":  "Server.DependentDatabases",
	"Database.Search":            "Database.Search",
	"Database.SearchDefinitions": "Database.SearchDefinitions",
}

// sweepMustCall names every read the live sweep must reach, by hand or
// reflectively. Listed by hand because what it guards against is precisely
// the read that quietly stops being swept: the reflective half covers a
// method only while it keeps a shape sweepArgs accepts, and one renamed,
// given an argument or moved leaves no trace when it drops out. A sweep that
// reports 0 failures because it made the call and one that reports it
// because it never did read the same otherwise (checkCoverage).
var sweepMustCall = []string{
	// The reads sweepReaders maps: the connect-time version read; the three
	// Server reads (reflective, listed in case that stops); the SCRAM
	// iteration count a password write reads first; the any-kind routine
	// lookup under Database.RoutineByName.
	"NewServer",
	"Server.Info",
	"Server.Capabilities",
	"Server.DatabasePrivileges",
	"Server.passwordVerifier",
	"routineByName",

	// The database family (P4.1): the listing (reflective, and by hand with
	// a filter), the by-name read, and the size (reflective on the scratch
	// database, and by hand on template0, which takes no connections).
	"Server.Databases",
	"Server.DatabaseByName",
	"Database.Size",

	// The role family (P4.2): the listing (reflective, and by hand with a
	// filter) and the by-name read.
	"Server.Roles",
	"Server.RoleByName",

	// The schema family (P4.3): the listing (reflective on the scratch
	// database, and by hand with a filter), the by-name read, and the
	// database's default privileges (reflective, and by hand).
	"Database.Schemas",
	"Database.SchemaByName",
	"Database.DefaultPrivileges",

	// The table family (P4.4): the listing (reflective on the scratch
	// database, and by hand with a filter), the by-name read, and each read
	// of a table's parts (reflective on app.child, and by hand on the
	// fixture's tables, asserting their rows).
	"Database.Tables",
	"Database.TableByName",
	"Table.Columns",
	"Table.Constraints",
	"Table.Indexes",
	"Table.Triggers",
	"Table.Policies",
	"Database.PolicyByName",
	"Table.Children",
	"Table.Size",

	// The view family (P4.5): both listings (reflective on the scratch
	// database, and by hand with a filter), both by-name reads, and each
	// part read (reflective on the fixture's view and materialized view,
	// and by hand asserting their rows).
	"Database.Views",
	"Database.ViewByName",
	"View.Columns",
	"Database.MaterializedViews",
	"Database.MaterializedViewByName",
	"MaterializedView.Columns",
	"MaterializedView.Indexes",
	"MaterializedView.Size",

	// The routine families (P4.6): the three listings (reflective on the
	// scratch database, and by hand with a filter), the three by-name reads
	// (the family's shared readers, so each family's select is sent), the
	// two bodies (reflective and by hand), and the any-kind identity read.
	"Database.Functions",
	"Database.FunctionByName",
	"Function.Body",
	"Database.Procedures",
	"Database.ProcedureByName",
	"Procedure.Body",
	"Database.Aggregates",
	"Database.AggregateByName",
	"Database.RoutineByName",

	// The sequence family (P4.7): the listing (reflective on the scratch
	// database, and by hand with a filter), the by-name read, and the last
	// value (reflective on the fixture's sequence, and by hand: a value as
	// the connected role, refused as the plain one).
	"Database.Sequences",
	"Database.SequenceByName",
	"Sequence.LastValue",

	// The type and domain families (P4.8): the listings (reflective on the
	// scratch database, and by hand with a filter) and the by-name reads of
	// each kind.
	"Database.Types",
	"Database.TypeByName",
	"Database.Domains",
	"Database.DomainByName",

	// The trigger families (P4.9): the database's triggers and event
	// triggers (reflective on the scratch database, and by hand with a
	// filter), both by-name reads, and a view's triggers (reflective on the
	// fixture's view, and by hand) — the read Table.Triggers shares.
	"Database.Triggers",
	"Database.TriggerByName",
	"View.Triggers",
	"Database.EventTriggers",
	"Database.EventTriggerByName",

	// The extension family (P4.10): the installed listing and the
	// available one (reflective on the scratch database, and by hand with a
	// filter), both by-name reads, and an extension's members (reflective
	// on the fixture's citext, and by hand).
	"Database.Extensions",
	"Database.ExtensionByName",
	"Extension.Members",
	"Database.AvailableExtensions",
	"Database.AvailableExtensionByName",

	// The P4.11 families: tablespaces (reflective, and by hand: the
	// listing with a filter, by name, and the size of pg_default — any
	// role's, its session's database living there — and of pg_global,
	// refused to the plain role); collations, languages, casts, operators
	// and the four text search families (reflective on the scratch
	// database, and by hand with a filter, each by-name read on a fixture
	// object).
	"Server.Tablespaces",
	"Server.TablespaceByName",
	"Tablespace.Size",
	"Database.Collations",
	"Database.CollationByName",
	"Database.TextSearchParsers",
	"Database.TextSearchParserByName",
	"Database.TextSearchTemplates",
	"Database.TextSearchTemplateByName",
	"Database.TextSearchDictionaries",
	"Database.TextSearchDictionaryByName",
	"Database.TextSearchConfigurations",
	"Database.TextSearchConfigurationByName",
	"Database.Languages",
	"Database.LanguageByName",
	"Database.Casts",
	"Database.CastByTypes",
	"Database.Operators",
	"Database.OperatorByName",

	// The P4.12 families: wrappers, servers, user mappings and foreign
	// tables (reflective on the scratch database and the fixture's
	// objects, and by hand: each listing with a filter, each by-name read,
	// a wrapper's servers, a server's user mappings — the options of each
	// shown or withheld by role — and a foreign table's parts, its
	// triggers through the read Table.Triggers shares).
	"Database.ForeignDataWrappers",
	"Database.ForeignDataWrapperByName",
	"ForeignDataWrapper.Servers",
	"Database.ForeignServers",
	"Database.ForeignServerByName",
	"ForeignServer.UserMappings",
	"Database.UserMappings",
	"Database.UserMappingByName",
	"Database.ForeignTables",
	"Database.ForeignTableByName",
	"ForeignTable.Columns",
	"ForeignTable.Constraints",
	"ForeignTable.Triggers",

	// The query fragments the P4 families will build their listings from.
	// No read sends them yet, so the sweep runs each in a query of its own,
	// over every catalog it is written for.
	"originColumns",
	"ObjectFilter.clause",

	// Dependencies and search (P4.14), by hand on the fixture: each
	// direction inside the database and between shared objects, the
	// databases a role has dependents in, a search by name and by
	// definition.
	"Database.Dependencies",
	"Database.Dependents",
	"Server.Dependencies",
	"Server.Dependents",
	"Server.DependentDatabases",
	"Database.Search",
	"Database.SearchDefinitions",
}

// readHelpers are the read paths of statement.go and rows.go: every read
// pgmo sends goes through one (TestRowIterationGoesThroughTheHelpers).
var readHelpers = []string{"query", "queryRow", "queryRowScan", "scanRows", "readByName"}

// sweep records what ran and what failed, so the result is one report rather
// than a stream of failures in call order. With role set, each call runs in
// its own transaction under SET LOCAL ROLE role, so a read the role's
// visibility breaks is found, and a failed read does not abort the next.
type sweep struct {
	t       testing.TB
	ctx     context.Context
	srv     *Server
	role    string          // "" for the connected role
	calls   int             // labels reached
	failed  []string        // reads the server (or pgmo) rejected
	refused []string        // pgmo's own version refusals; expected, not failures
	called  map[string]bool // every label reached, for checkCoverage
}

// call runs one read and records its outcome. A panic is recorded like an
// error: the sweep's value is in reaching the end of the list.
func (sw *sweep) call(label string, fn func(ctx context.Context) error) {
	sw.calls++
	if sw.called == nil {
		sw.called = map[string]bool{}
	}
	sw.called[label] = true
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		if sw.role == "" {
			return fn(sw.ctx)
		}
		return sw.srv.InTransaction(sw.ctx, func(ctx context.Context) error {
			if _, err := sw.srv.on(ctx).Exec(ctx, "SET LOCAL ROLE "+pgsql.QuoteIdent(sw.role)); err != nil {
				return fmt.Errorf("SET LOCAL ROLE: %w", err)
			}
			return fn(ctx)
		})
	}()
	switch {
	case err == nil:
	case errors.Is(err, ErrUnsupportedVersion):
		// pgmo declining a feature the server is too old for is the gate
		// working, not a defect — the point of sweeping the oldest major.
		sw.refused = append(sw.refused, fmt.Sprintf("%s: %v", label, err))
	default:
		sw.failed = append(sw.failed, fmt.Sprintf("%s: %v", label, err))
	}
}

// sweepZeroQueries are the listing arguments whose zero value means "all",
// so a listing taking one is as reachable by reflection as a context-only one.
var sweepZeroQueries = []reflect.Type{
	reflect.TypeFor[ObjectFilter](),
}

// sweepEmptyVariadics are the variadic filters whose empty form means "all".
// None yet; not every variadic qualifies.
var sweepEmptyVariadics = []reflect.Type{}

// sweepArgs reports whether reflectSweep can call a method of type ft (its
// receiver first) and the arguments it passes after the context: none, a
// sweepZeroQueries zero value, or an empty sweepEmptyVariadics. It must end
// in an error.
func sweepArgs(ft reflect.Type) ([]reflect.Value, bool) {
	if ft.NumIn() < 2 || ft.In(1) != reflect.TypeFor[context.Context]() {
		return nil, false
	}
	if ft.NumOut() == 0 || ft.Out(ft.NumOut()-1) != reflect.TypeFor[error]() {
		return nil, false
	}
	switch {
	case ft.NumIn() == 2:
		return nil, true
	case ft.NumIn() == 3 && ft.IsVariadic() && slices.Contains(sweepEmptyVariadics, ft.In(2)):
		return nil, true
	case ft.NumIn() == 3 && slices.Contains(sweepZeroQueries, ft.In(2)):
		return []reflect.Value{reflect.Zero(ft.In(2))}, true
	}
	return nil, false
}

// reflectSweep calls every exported method on recv sweepArgs accepts and
// sweepSkip does not name. Methods taking other arguments are driven by hand
// in TestLiveVersionSweep; this half keeps a newly added listing covered
// without anyone remembering to add it.
//
// Each call runs under its own WithScript context. Reads still reach the
// server, so the sweep measures what it always did; a write is only
// collected, and fails the sweep as one that wrote. That makes sweepSkip a
// detector rather than the only guard: a new context-only write nobody listed
// would otherwise run the first time the sweep met a real server.
func (sw *sweep) reflectSweep(typeName, instance string, recv any) {
	rv := reflect.ValueOf(recv)
	for m := range rv.Type().Methods() {
		extra, ok := sweepArgs(m.Type)
		if !ok {
			continue
		}
		if _, skip := sweepSkip[typeName+"."+m.Name]; skip {
			continue
		}
		sw.call(fmt.Sprintf("%s.%s [%s]", typeName, m.Name, instance), func(ctx context.Context) error {
			wctx, capture := WithScript(ctx)
			out := m.Func.Call(append([]reflect.Value{rv, reflect.ValueOf(wctx)}, extra...))
			if capture.Len() > 0 {
				// Ahead of the method's own error: a write that also failed
				// is still a write.
				return fmt.Errorf("wrote under the sweep — add it to sweepSkip: %.200q",
					strings.Join(strings.Fields(capture.String()), " "))
			}
			if e := out[len(out)-1].Interface(); e != nil {
				return e.(error)
			}
			return nil
		})
	}
}

// checkCoverage fails on any sweepMustCall entry no label matched: a sweep
// that reports 0 failures because it never made a call reads the same as one
// that made it. Labels carry two decorations, " [<instance>]" from the
// reflective half and "(<arg>)" from the hand-driven one.
func (sw *sweep) checkCoverage() {
	sw.t.Helper()
	for _, want := range sweepMustCall {
		if !slices.ContainsFunc(slices.Collect(maps.Keys(sw.called)), func(label string) bool {
			return label == want || strings.HasPrefix(label, want+" [") || strings.HasPrefix(label, want+"(")
		}) {
			sw.t.Errorf("sweep never called %s — it is in sweepMustCall but no label matched, "+
				"so this server's answer for it is unknown, not clean", want)
		}
	}
}

// checkSweepKeys fails on a sweepSkip or sweepNotReads key naming no method
// of a swept type, or one reflectSweep would not call anyway.
func checkSweepKeys(t *testing.T) {
	t.Helper()
	for list, keys := range map[string]map[string]string{"sweepSkip": sweepSkip, "sweepNotReads": sweepNotReads} {
		for key := range keys {
			typeName, method, _ := strings.Cut(key, ".")
			rt := sweptTypes[typeName]
			if rt == nil {
				t.Errorf("%s key %q names no swept type", list, key)
				continue
			}
			m, found := rt.MethodByName(method)
			if !found {
				t.Errorf("%s names %s, which %s does not have — stale entry", list, key, rt)
				continue
			}
			_, reflectable := sweepArgs(m.Type)
			if list == "sweepSkip" && !reflectable {
				t.Errorf("sweepSkip names %s, which reflectSweep never calls — stale entry", key)
			}
			if list == "sweepNotReads" && reflectable {
				t.Errorf("sweepNotReads names %s, which reflectSweep calls — skip a write in sweepSkip", key)
			}
		}
	}
}

// TestSweepSkipNamesRealMethods runs the sweep's stale-entry check offline,
// so a rename that orphans an entry fails go test ./... rather than waiting
// for the next live run.
func TestSweepSkipNamesRealMethods(t *testing.T) {
	checkSweepKeys(t)
}

// TestSweepCoversEveryRead: every function sending a read is in sweepReaders,
// every entry there still sends one, and each label is one the live sweep
// must call.
func TestSweepCoversEveryRead(t *testing.T) {
	_, files := parsePackage(t)
	readers := sweepReadersIn(files)
	for _, fn := range slices.Sorted(maps.Keys(readers)) {
		if _, ok := sweepReaders[fn]; !ok {
			t.Errorf("%s: %s sends a read the version sweep does not drive — add it to sweepReaders "+
				"and call it in live_versionsweep_test.go", readers[fn], fn)
		}
	}
	for _, fn := range slices.Sorted(maps.Keys(sweepReaders)) {
		if _, ok := readers[fn]; !ok {
			t.Errorf("sweepReaders names %s, which sends no read — stale entry", fn)
		}
		if label := sweepReaders[fn]; !slices.Contains(sweepMustCall, label) {
			t.Errorf("sweepReaders drives %s by %q, which is not in sweepMustCall", fn, label)
		}
	}
}

// TestSweepReachesEveryContextMethod: every exported context-first method a
// swept type declares is called reflectively, skipped as a write, named as
// no read, or driven by hand (in sweepMustCall).
func TestSweepReachesEveryContextMethod(t *testing.T) {
	_, files := parsePackage(t)
	for _, key := range contextMethodsIn(files) {
		typeName, method, _ := strings.Cut(key, ".")
		rt, ok := sweptTypes[typeName]
		if !ok {
			continue
		}
		m, found := rt.MethodByName(method)
		if !found {
			t.Errorf("%s: declared in the source, not found by reflection on %s", key, rt)
			continue
		}
		_, reflectable := sweepArgs(m.Type)
		_, notRead := sweepNotReads[key]
		if !reflectable && !notRead && !slices.Contains(sweepMustCall, key) {
			t.Errorf("%s takes arguments reflectSweep has no value for: drive it by hand in "+
				"live_versionsweep_test.go and list it in sweepMustCall, or say in sweepNotReads why it is no read", key)
		}
	}
}

// sweepReadersIn maps each function in files that calls a read helper — in
// its body or in a function literal there — to the position of the first
// call, keyed "<type>.<method>" for a method. The helpers themselves are not
// readers.
func sweepReadersIn(files map[string]*ast.File) map[string]string {
	out := map[string]string{}
	for name, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			key := funcKey(fd)
			if fd.Recv == nil && slices.Contains(readHelpers, fd.Name.Name) {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := unindex(call.Fun).(*ast.Ident); ok && slices.Contains(readHelpers, id.Name) {
					if _, seen := out[key]; !seen {
						out[key] = name
					}
				}
				return true
			})
		}
	}
	return out
}

// contextMethodsIn is every exported method in files whose first parameter is
// a context.Context, keyed "<type>.<method>", sorted.
func contextMethodsIn(files map[string]*ast.File) []string {
	var out []string
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || !fd.Name.IsExported() {
				continue
			}
			params := fd.Type.Params.List
			if len(params) == 0 || selectorName(params[0].Type) != "context.Context" {
				continue
			}
			out = append(out, funcKey(fd))
		}
	}
	slices.Sort(out)
	return out
}

// TestSweepReadersScan is the reader finder's self-test on planted code: a
// method and a function calling a helper (with type arguments, and from a
// function literal) are readers; a helper calling another, a function calling
// none, and a method of the same name on another type are told apart.
func TestSweepReadersScan(t *testing.T) {
	src := `package pgmo
func (s *Server) Listing(ctx context.Context) error { _, err := scanRows[int](ctx, s.on(ctx), "x", nil, "SELECT 1"); return err }
func byName(ctx context.Context, q Querier) error { return queryRowScan(ctx, q, "SELECT 1", nil) }
func lit(ctx context.Context, q Querier) { go func() { _ = queryRow(ctx, q, nil, "SELECT 1") }() }
func (d Database[T]) Listing(ctx context.Context) error { return readByName(ctx) }
func scanRows(ctx context.Context) error { return query(ctx) }
func quiet(ctx context.Context) error { return nil }
func (s *Server) Exec(ctx context.Context) error { return s.exec(ctx, "x") }
`
	f, err := parser.ParseFile(token.NewFileSet(), "planted.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*ast.File{"planted.go": f}
	got := slices.Sorted(maps.Keys(sweepReadersIn(files)))
	want := []string{"Database.Listing", "Server.Listing", "byName", "lit"}
	if !slices.Equal(got, want) {
		t.Errorf("readers = %q, want %q", got, want)
	}
	if got, want := contextMethodsIn(files), []string{"Database.Listing", "Server.Exec", "Server.Listing"}; !slices.Equal(got, want) {
		t.Errorf("context methods = %q, want %q", got, want)
	}
}

// sweepWriter has the shape of a write added later without a sweepSkip
// entry: an exported method taking only ctx, returning an error — beside a
// read of the same shape, which must still be swept as one, and a listing
// taking an ObjectFilter, reached with its zero value.
type sweepWriter struct{ s *Server }

func (w sweepWriter) Checkpoint(ctx context.Context) error {
	_, err := w.s.exec(ctx, "CHECKPOINT")
	return err
}

func (w sweepWriter) Answer(ctx context.Context) (int, error) {
	var n int
	return n, queryRowScan(ctx, w.s.on(ctx), "SELECT 42", nil, &n, new(string))
}

func (w sweepWriter) Listing(ctx context.Context, f ObjectFilter) ([]string, error) {
	if !f.Empty() {
		return nil, invalidf("swept with a filter: %+v", f)
	}
	return nil, nil
}

func (w sweepWriter) Named(ctx context.Context, name string) error { return nil }

// TestReflectSweepRefusesWrites pins the guard reflectSweep runs under: a
// context-only write sweepSkip does not name is collected, not executed, and
// fails the sweep, while a read beside it still reaches the server and passes;
// a method taking an argument the sweep has no value for is not called; and
// one sweepSkip names is not called at all.
func TestReflectSweepRefusesWrites(t *testing.T) {
	f := startFakeServer(t, 150020)
	s, err := Connect(testContext(t), f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	sw := &sweep{t: t, ctx: testContext(t), srv: s}
	sw.reflectSweep("sweepWriter", "fake", sweepWriter{s})
	if got := slices.Sorted(maps.Keys(sw.called)); !slices.Equal(got, []string{
		"sweepWriter.Answer [fake]", "sweepWriter.Checkpoint [fake]", "sweepWriter.Listing [fake]",
	}) {
		t.Fatalf("swept %q, want Answer, Checkpoint and Listing", got)
	}
	if len(sw.failed) != 1 || !strings.HasPrefix(sw.failed[0], "sweepWriter.Checkpoint ") ||
		!strings.Contains(sw.failed[0], "wrote under the sweep") || !strings.Contains(sw.failed[0], "CHECKPOINT") {
		t.Errorf("failures = %q, want one naming the Checkpoint write", sw.failed)
	}
	queries, _, _ := f.seen(t, 1)
	if slices.ContainsFunc(queries, func(q string) bool { return strings.Contains(q, "CHECKPOINT") }) {
		t.Errorf("the write reached the server: %q", queries)
	}
	if !slices.Contains(queries, "SELECT 42") {
		t.Errorf("the read did not reach the server: %q", queries)
	}

	sweepSkip["sweepWriter.Checkpoint"] = "planted"
	t.Cleanup(func() { delete(sweepSkip, "sweepWriter.Checkpoint") })
	sw = &sweep{t: t, ctx: testContext(t), srv: s}
	sw.reflectSweep("sweepWriter", "fake", sweepWriter{s})
	if sw.called["sweepWriter.Checkpoint [fake]"] || len(sw.failed) != 0 {
		t.Errorf("a sweepSkip entry was still called: %v, failures %q", sw.called, sw.failed)
	}
}

// TestSweepCallUnderARole: with role set each call is its own transaction
// opened by SET LOCAL ROLE, so one failing read does not abort the next; a
// panic is a failure, and a version refusal is counted apart.
func TestSweepCallUnderARole(t *testing.T) {
	f := startFakeServer(t, 150020)
	s, err := Connect(testContext(t), f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	sw := &sweep{t: t, ctx: testContext(t), srv: s, role: "Plain Role"}
	var sawTx bool
	sw.call("fails", func(ctx context.Context) error { return errors.New("boom") })
	sw.call("panics", func(ctx context.Context) error { panic("bang") })
	sw.call("too new", func(ctx context.Context) error { return refusalf(ErrUnsupportedVersion, "needs 16") })
	sw.call("reads", func(ctx context.Context) error {
		_, sawTx = sw.srv.on(ctx).(pgx.Tx)
		return nil
	})
	if sw.calls != 4 || len(sw.failed) != 2 || len(sw.refused) != 1 ||
		!strings.HasPrefix(sw.failed[0], "fails: ") || !strings.Contains(sw.failed[1], "panic: bang") {
		t.Errorf("calls %d, failed %q, refused %q", sw.calls, sw.failed, sw.refused)
	}
	if !sawTx {
		t.Error("a call under a role did not run in a transaction")
	}
	queries, _, _ := f.seen(t, 1)
	sets := 0
	for _, q := range queries {
		if q == `SET LOCAL ROLE "Plain Role"` {
			sets++
		}
	}
	if sets != 4 {
		t.Errorf("SET LOCAL ROLE sent %d times, want once per call: %q", sets, queries)
	}
}

// errorRecorder is a testing.TB whose Errorf is recorded, not reported.
type errorRecorder struct {
	testing.TB
	errs []string
}

func (r *errorRecorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// TestCheckCoverage: an entry is covered by its label bare, with the
// reflective " [instance]" or the hand-driven "(arg)" — and not by a label it
// is only a prefix of; each one left uncalled is reported by name.
func TestCheckCoverage(t *testing.T) {
	rec := &errorRecorder{TB: t}
	sw := &sweep{t: rec, called: map[string]bool{}}
	for i, want := range sweepMustCall {
		switch i % 3 {
		case 0:
			sw.called[want] = true
		case 1:
			sw.called[want+" [db]"] = true
		case 2:
			sw.called[want+"(arg)"] = true
		}
	}
	sw.checkCoverage()
	if len(rec.errs) != 0 {
		t.Errorf("every entry called, still reported: %q", rec.errs)
	}

	missing := sweepMustCall[len(sweepMustCall)-1]
	for label := range sw.called {
		if strings.HasPrefix(label, missing) {
			delete(sw.called, label)
		}
	}
	sw.called[missing+"Extra"] = true
	sw.checkCoverage()
	if len(rec.errs) != 1 || !strings.Contains(rec.errs[0], "never called "+missing+" ") {
		t.Errorf("one entry uncalled: reported %q, want it alone", rec.errs)
	}
}
