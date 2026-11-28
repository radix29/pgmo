package pgmo

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Privilege writes: GRANT, REVOKE, ALTER DEFAULT PRIVILEGES
// ============================================================
//
// privilege_write.go is P4.13.4's privileges, gosmo's ApplyPermission for
// PostgreSQL: one GRANT or REVOKE of a set of privileges on one securable to
// a set of roles (ApplyPrivileges), and ALTER DEFAULT PRIVILEGES
// (AlterDefaultPrivileges). PostgreSQL has no DENY: a privilege is held or
// not, and REVOKE takes away only what the same grantor granted.
//
// A privilege is a keyword, never quoted, so each statement takes only the
// privileges its object kind accepts, from an allowlist (securableKinds) —
// what keeps a caller's Privilege text out of the SQL. Everything else the
// server refuses — a role that does not exist (42704), an object of another
// kind (42809), REVOKE … RESTRICT with dependent grants (2BP01), IN SCHEMA on
// default privileges for schemas (0LP01) — is left to it (docs/decisions.md).
//
// GRANTED BY is not offered: PostgreSQL 15–18 accept only the current user
// there (0A000 otherwise), which is what the statement records anyway.
// Nothing is mirrored: the server merges a grant into the ACL (one item per
// grantee and grantor) and records a superuser's as the owner's — read the
// object's Privileges again.

// PrivilegeAction is the statement a PrivilegeChange issues.
type PrivilegeAction string

const (
	ActionGrant  PrivilegeAction = "GRANT"
	ActionRevoke PrivilegeAction = "REVOKE"
)

// PrivilegeChange is one GRANT or REVOKE of privileges to (from) roles,
// whatever it is on: ApplyPrivileges' securable or AlterDefaultPrivileges'
// object type.
type PrivilegeChange struct {
	Action PrivilegeAction

	// Privileges are the privileges granted or revoked, each one the object
	// kind accepts. All is ALL PRIVILEGES instead: every privilege the kind
	// has on the server's release. Exactly one of the two is set.
	Privileges []Privilege
	All        bool

	// Grantees are the roles granted to (revoked from); "" is PUBLIC, as on
	// PrivilegeGrant.Grantee. At least one.
	Grantees []string

	// GrantOption is WITH GRANT OPTION on a GRANT — the grantees may grant
	// the privileges on — and GRANT OPTION FOR on a REVOKE: the grantees
	// lose only that right and keep the privileges.
	GrantOption bool

	// Cascade is a REVOKE's CASCADE: what the grantees granted on from the
	// grant option is revoked too. Without it the server refuses to revoke
	// a grant option someone has used (2BP01). REVOKE only; never a default.
	Cascade bool
}

// SecurableKind is the kind of object a privilege is on: the keyword GRANT's
// ON clause names it by.
type SecurableKind string

const (
	// SecurableTable is a table, partitioned table, view, materialized view
	// or foreign table (GRANT … ON TABLE takes them all); the one kind with
	// column privileges (Securable.Columns).
	SecurableTable              SecurableKind = "TABLE"
	SecurableSequence           SecurableKind = "SEQUENCE"
	SecurableDatabase           SecurableKind = "DATABASE" // Server.ApplyPrivileges
	SecurableDomain             SecurableKind = "DOMAIN"
	SecurableForeignDataWrapper SecurableKind = "FOREIGN DATA WRAPPER"
	SecurableForeignServer      SecurableKind = "FOREIGN SERVER"
	// SecurableFunction is a function or an aggregate; SecurableRoutine any
	// of the three. Each is named by its argument types
	// (Securable.Arguments).
	SecurableFunction  SecurableKind = "FUNCTION"
	SecurableProcedure SecurableKind = "PROCEDURE"
	SecurableRoutine   SecurableKind = "ROUTINE"
	SecurableLanguage  SecurableKind = "LANGUAGE"
	// SecurableLargeObject is named by its OID, in decimal (Securable.Name).
	SecurableLargeObject SecurableKind = "LARGE OBJECT"
	// SecurableParameter is a configuration parameter (pg_parameter_acl):
	// SET lets a role set a superuser-only parameter in its session, ALTER
	// SYSTEM write it to postgresql.auto.conf. Server.ApplyPrivileges.
	SecurableParameter  SecurableKind = "PARAMETER"
	SecurableSchema     SecurableKind = "SCHEMA"
	SecurableTablespace SecurableKind = "TABLESPACE" // Server.ApplyPrivileges
	SecurableType       SecurableKind = "TYPE"
)

// securableKind is what a SecurableKind's statement accepts.
type securableKind struct {
	privileges []Privilege // what the kind accepts, in the server's order
	schema     bool        // schema-scoped: Schema required, written schema.name
	cluster    bool        // a shared object: Server.ApplyPrivileges
	routine    bool        // named by its argument types
	allIn      bool        // has an ALL … IN SCHEMA form
}

var tablePrivileges = []Privilege{PrivilegeSelect, PrivilegeInsert, PrivilegeUpdate, PrivilegeDelete,
	PrivilegeTruncate, PrivilegeReferences, PrivilegeTrigger, PrivilegeMaintain}

// columnPrivileges are the privileges a column takes (GRANT SELECT (a) …).
var columnPrivileges = []Privilege{PrivilegeSelect, PrivilegeInsert, PrivilegeUpdate, PrivilegeReferences}

var sequencePrivileges = []Privilege{PrivilegeUsage, PrivilegeSelect, PrivilegeUpdate}

var usagePrivilege = []Privilege{PrivilegeUsage}

var executePrivilege = []Privilege{PrivilegeExecute}

var largeObjectPrivileges = []Privilege{PrivilegeSelect, PrivilegeUpdate}

var securableKinds = map[SecurableKind]securableKind{
	SecurableTable:              {privileges: tablePrivileges, schema: true, allIn: true},
	SecurableSequence:           {privileges: sequencePrivileges, schema: true, allIn: true},
	SecurableDatabase:           {privileges: []Privilege{PrivilegeCreate, PrivilegeConnect, PrivilegeTemporary}, cluster: true},
	SecurableDomain:             {privileges: usagePrivilege, schema: true},
	SecurableForeignDataWrapper: {privileges: usagePrivilege},
	SecurableForeignServer:      {privileges: usagePrivilege},
	SecurableFunction:           {privileges: executePrivilege, schema: true, routine: true, allIn: true},
	SecurableProcedure:          {privileges: executePrivilege, schema: true, routine: true, allIn: true},
	SecurableRoutine:            {privileges: executePrivilege, schema: true, routine: true, allIn: true},
	SecurableLanguage:           {privileges: usagePrivilege},
	SecurableLargeObject:        {privileges: largeObjectPrivileges},
	SecurableParameter:          {privileges: []Privilege{PrivilegeSet, PrivilegeAlterSystem}, cluster: true},
	SecurableSchema:             {privileges: []Privilege{PrivilegeUsage, PrivilegeCreate}},
	SecurableTablespace:         {privileges: []Privilege{PrivilegeCreate}, cluster: true},
	SecurableType:               {privileges: usagePrivilege, schema: true},
}

// Securable is the object ApplyPrivileges grants on. A family's handle gives
// its own (Table.Securable, Function.Securable…).
type Securable struct {
	Kind SecurableKind

	// Schema is the object's schema, required for a schema-scoped kind
	// (tables, sequences, routines, types, domains) and empty for the
	// others.
	Schema string

	// Name is the object's name; a large object's OID in decimal; a
	// parameter's name as SHOW takes it ("work_mem", "plpgsql.extra_errors").
	Name string

	// Arguments are a routine's input argument types as SQL, written as
	// given — RoutineIdentity.ArgumentTypeNames. Required (empty for none)
	// for the routine kinds, which are always named by their signature, and
	// refused on the others.
	Arguments []string

	// Columns narrows a SecurableTable grant to these columns (GRANT SELECT
	// (a, b) ON …), which take SELECT, INSERT, UPDATE and REFERENCES. Nil
	// is the whole table.
	Columns []string

	// AllInSchema is ON ALL TABLES (SEQUENCES, FUNCTIONS, PROCEDURES,
	// ROUTINES) IN SCHEMA Schema: every such object the schema holds now,
	// not those created later (AlterDefaultPrivileges). Name is empty.
	AllInSchema bool
}

// object is the securable as GRANT's ON clause names it, after "ON ": the
// kind's keyword and the name, or the refusal of an incomplete one.
func (s Securable) object(k securableKind) (string, error) {
	if s.Columns != nil {
		switch {
		case s.Kind != SecurableTable || s.AllInSchema:
			return "", invalidf("column privileges are on one table")
		case len(s.Columns) == 0:
			// Nil means the whole table; widening an empty list to it would
			// be the wrong guess.
			return "", invalidf("no columns named")
		}
		if err := requireNames("the column list", s.Columns); err != nil {
			return "", err
		}
	}
	if s.Arguments != nil && !k.routine {
		return "", invalidf("argument types on a %s", keywordLabel(string(s.Kind)))
	}
	if s.AllInSchema {
		switch {
		case !k.allIn:
			return "", invalidf("no ALL … IN SCHEMA form for a %s", keywordLabel(string(s.Kind)))
		case s.Schema == "":
			return "", ErrSchemaRequired
		case s.Name != "" || s.Arguments != nil:
			return "", invalidf("ALL … IN SCHEMA names no object")
		}
		return "ALL " + string(s.Kind) + "S IN SCHEMA " + pgsql.QuoteIdent(s.Schema), nil
	}
	if !k.schema && s.Schema != "" {
		return "", invalidf("a %s is not in a schema", keywordLabel(string(s.Kind)))
	}
	if k.schema {
		if err := checkQualified(keywordLabel(string(s.Kind)), s.Schema, s.Name); err != nil {
			return "", err
		}
	} else if s.Name == "" {
		return "", invalidf("%s: no name", keywordLabel(string(s.Kind)))
	}
	switch {
	case k.routine:
		if s.Arguments == nil {
			return "", invalidf("%s %s: no argument types (empty for none)", keywordLabel(string(s.Kind)), pgsql.QualifiedName(s.Schema, s.Name))
		}
		for _, a := range s.Arguments {
			if a == "" {
				return "", invalidf("an empty argument type")
			}
		}
		return string(s.Kind) + " " + pgsql.QualifiedName(s.Schema, s.Name) + "(" + strings.Join(s.Arguments, ", ") + ")", nil
	case k.schema:
		return string(s.Kind) + " " + pgsql.QualifiedName(s.Schema, s.Name), nil
	case s.Kind == SecurableLargeObject:
		if oid, err := strconv.ParseUint(s.Name, 10, 32); err != nil || oid == 0 || strconv.FormatUint(oid, 10) != s.Name {
			return "", invalidf("large object %q: not an OID", s.Name)
		}
		return string(s.Kind) + " " + s.Name, nil
	}
	return string(s.Kind) + " " + pgsql.QuoteIdent(s.Name), nil
}

// label is the securable as an error message names it: `table app."T"`,
// `all tables in schema app`, `function app.f(integer)`.
func (s Securable) label() string {
	kind := keywordLabel(string(s.Kind))
	switch {
	case s.AllInSchema:
		return "all " + kind + "s in schema " + pgsql.QuoteIdent(s.Schema)
	case s.Arguments != nil:
		return kind + " " + pgsql.QualifiedName(s.Schema, s.Name) + "(" + strings.Join(s.Arguments, ", ") + ")"
	case s.Schema != "":
		return kind + " " + pgsql.QualifiedName(s.Schema, s.Name)
	case s.Kind == SecurableLargeObject:
		return kind + " " + s.Name
	}
	return kind + " " + pgsql.QuoteIdent(s.Name)
}

// roleSpec is a grantee as GRANT, REVOKE and CREATE POLICY's TO take it:
// PUBLIC for "", else the quoted name — which the server reads as a role
// even when it spells a keyword ("current_user").
func roleSpec(name string) string {
	if name == "" {
		return "PUBLIC"
	}
	return pgsql.QuoteIdent(name)
}

// roleSpecs is roleSpec over names, comma-separated.
func roleSpecs(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = roleSpec(n)
	}
	return strings.Join(q, ", ")
}

// verb is the change's action as an error names it: "grant", "revoke".
func (c PrivilegeChange) verb() string { return keywordLabel(string(c.Action)) }

// statement is c on the object clause on ("TABLE app.t", "TABLES"), with
// columns after each privilege, the privileges checked against allowed —
// what kind, the object kind's keyword, takes (an error names it); the
// prefix (ALTER DEFAULT PRIVILEGES …) is the caller's. v is the server's
// release, which MAINTAIN and other privileges a release added need.
func (c PrivilegeChange) statement(on, kind string, columns []string, allowed []Privilege, v ServerVersion) (string, error) {
	switch c.Action {
	case ActionGrant:
		if c.Cascade {
			return "", invalidf("CASCADE on a GRANT")
		}
	case ActionRevoke:
	default:
		return "", invalidf("unknown action %q", string(c.Action))
	}
	if len(c.Grantees) == 0 {
		return "", invalidf("no grantees")
	}
	cols := ""
	if columns != nil {
		cols = " (" + identList(columns) + ")"
	}
	var privs string
	switch {
	case c.All && c.Privileges != nil:
		return "", invalidf("both ALL PRIVILEGES and a privilege list")
	case c.All:
		privs = "ALL PRIVILEGES" + cols
	case len(c.Privileges) == 0:
		return "", invalidf("no privileges")
	default:
		parts := make([]string, len(c.Privileges))
		for i, p := range c.Privileges {
			if !slices.Contains(allowed, p) {
				return "", invalidf("%q is not a privilege of %s", string(p), keywordLabel(kind))
			}
			if p == PrivilegeMaintain && before(int(v), PostgreSQL17) {
				return "", refusalf(ErrUnsupportedVersion, "MAINTAIN needs %s or later; the server is %s", PostgreSQL17, v)
			}
			parts[i] = string(p) + cols
		}
		privs = strings.Join(parts, ", ")
	}
	if c.Action == ActionGrant {
		stmt := "GRANT " + privs + " ON " + on + " TO " + roleSpecs(c.Grantees)
		if c.GrantOption {
			stmt += " WITH GRANT OPTION"
		}
		return stmt, nil
	}
	stmt := "REVOKE "
	if c.GrantOption {
		stmt += "GRANT OPTION FOR "
	}
	stmt += privs + " ON " + on + " FROM " + roleSpecs(c.Grantees)
	if c.Cascade {
		stmt += " CASCADE"
	}
	return stmt, nil
}

// privilegeStatement is c on on, or the refusal of one that cannot be
// written; cluster says which receiver is asking. The error is prefixed,
// naming the operation.
func privilegeStatement(on Securable, c PrivilegeChange, cluster bool, v ServerVersion) (string, error) {
	k, ok := securableKinds[on.Kind]
	if !ok {
		return "", invalidf("pgmo: %s privileges: unknown securable kind %q", c.verb(), string(on.Kind))
	}
	op := c.verb() + " privileges on " + on.label()
	switch {
	case k.cluster && !cluster:
		return "", invalidf("pgmo: %s: a %s is the server's; use Server.ApplyPrivileges", op, keywordLabel(string(on.Kind)))
	case !k.cluster && cluster:
		return "", invalidf("pgmo: %s: a %s is in a database; use Database.ApplyPrivileges", op, keywordLabel(string(on.Kind)))
	}
	object, err := on.object(k)
	if err != nil {
		return "", fmt.Errorf("pgmo: %s: %w", op, err)
	}
	allowed, kind := k.privileges, string(on.Kind)
	if on.Columns != nil {
		allowed, kind = columnPrivileges, "COLUMNS"
	}
	stmt, err := c.statement(object, kind, on.Columns, allowed, v)
	if err != nil {
		return "", fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return stmt, nil
}

// ApplyPrivileges issues one GRANT or REVOKE of c on the object on, in the
// database: every kind but a database, a tablespace and a parameter, which
// are the server's (Server.ApplyPrivileges). It runs on the database's own
// session, inside InTransaction too.
//
// A privilege the kind does not take, an incomplete securable, and a
// modifier the action has no form for are refused with ErrInvalidRequest,
// MAINTAIN before 17 with ErrUnsupportedVersion, before anything is sent.
// The server checks the rest: the role must own the object, or hold the
// privileges WITH GRANT OPTION (a superuser's grant is recorded as the
// owner's).
func (d *Database) ApplyPrivileges(ctx context.Context, on Securable, c PrivilegeChange) error {
	stmt, err := privilegeStatement(on, c, false, d.server.version)
	if err != nil {
		return err
	}
	if err := d.exec(ctx, stmt); err != nil {
		return fmt.Errorf("pgmo: %s privileges on %s in database %q: %w", c.verb(), on.label(), d.Name, err)
	}
	return nil
}

// ApplyPrivileges issues one GRANT or REVOKE of c on a database, a
// tablespace or a configuration parameter — the shared objects — on the
// Server's own session. See Database.ApplyPrivileges for the refusals.
func (s *Server) ApplyPrivileges(ctx context.Context, on Securable, c PrivilegeChange) error {
	stmt, err := privilegeStatement(on, c, true, s.version)
	if err != nil {
		return err
	}
	if _, err := s.exec(ctx, stmt); err != nil {
		return fmt.Errorf("pgmo: %s privileges on %s: %w", c.verb(), on.label(), err)
	}
	return nil
}

// ------------------------------------------------------------
// ALTER DEFAULT PRIVILEGES
// ------------------------------------------------------------

// DefaultPrivilegeScope is which future objects an ALTER DEFAULT PRIVILEGES
// is for: those of ObjectType that Roles create, in Schemas.
type DefaultPrivilegeScope struct {
	// Roles are FOR ROLE: the creating roles; nil is the connected role.
	// The connected role must be a member of each.
	Roles []string

	// Schemas are IN SCHEMA; nil is database-wide (DefaultPrivileges
	// explains the difference: in a schema the entry can only add).
	// Schemas take no IN SCHEMA (0LP01, the server's).
	Schemas []string

	ObjectType DefaultPrivilegeObjectType
}

// defaultPrivilegeKinds are the privileges each default-privilege object
// type takes, as the matching securable kind's.
var defaultPrivilegeKinds = map[DefaultPrivilegeObjectType][]Privilege{
	DefaultPrivilegeTables:       tablePrivileges,
	DefaultPrivilegeSequences:    sequencePrivileges,
	DefaultPrivilegeFunctions:    executePrivilege,
	DefaultPrivilegeTypes:        usagePrivilege,
	DefaultPrivilegeSchemas:      securableKinds[SecurableSchema].privileges,
	DefaultPrivilegeLargeObjects: largeObjectPrivileges,
}

// defaultPrivilegeStatement is c in scope as ALTER DEFAULT PRIVILEGES, or the
// refusal of one that cannot be written. No prefix: the caller names the
// operation.
func defaultPrivilegeStatement(scope DefaultPrivilegeScope, c PrivilegeChange, v ServerVersion) (string, error) {
	allowed, ok := defaultPrivilegeKinds[scope.ObjectType]
	if !ok {
		return "", invalidf("unknown object type %q", string(scope.ObjectType))
	}
	if scope.ObjectType == DefaultPrivilegeLargeObjects && before(int(v), PostgreSQL18) {
		return "", refusalf(ErrUnsupportedVersion, "default privileges on large objects need %s or later; the server is %s", PostgreSQL18, v)
	}
	if err := requireNames("FOR ROLE", scope.Roles); err != nil {
		return "", err
	}
	if err := requireNames("IN SCHEMA", scope.Schemas); err != nil {
		return "", err
	}
	if scope.Roles != nil && len(scope.Roles) == 0 {
		return "", invalidf("an empty FOR ROLE list (nil is the connected role)")
	}
	if scope.Schemas != nil && len(scope.Schemas) == 0 {
		return "", invalidf("an empty IN SCHEMA list (nil is database-wide)")
	}
	stmt, err := c.statement(scope.ObjectType.String(), scope.ObjectType.String(), nil, allowed, v)
	if err != nil {
		return "", err
	}
	prefix := "ALTER DEFAULT PRIVILEGES "
	if scope.Roles != nil {
		prefix += "FOR ROLE " + identList(scope.Roles) + " "
	}
	if scope.Schemas != nil {
		prefix += "IN SCHEMA " + identList(scope.Schemas) + " "
	}
	return prefix + stmt, nil
}

// AlterDefaultPrivileges issues one ALTER DEFAULT PRIVILEGES: c applied to
// the ACL objects of scope.ObjectType will be created with, by scope.Roles,
// in scope.Schemas — not to any object that exists. Read the result back
// with Database.DefaultPrivileges.
//
// A REVOKE database-wide takes a privilege out of the hard-wired default
// (REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC); in a schema it can only remove
// what a GRANT in that schema added. Refusals as ApplyPrivileges'; large
// objects before 18 are ErrUnsupportedVersion.
func (d *Database) AlterDefaultPrivileges(ctx context.Context, scope DefaultPrivilegeScope, c PrivilegeChange) error {
	op := fmt.Sprintf("alter default privileges on %s in database %q", keywordLabel(scope.ObjectType.String()), d.Name)
	stmt, err := defaultPrivilegeStatement(scope, c, d.server.version)
	if err != nil {
		return fmt.Errorf("pgmo: %s: %w", op, err)
	}
	if err := d.exec(ctx, stmt); err != nil {
		return fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return nil
}

// ------------------------------------------------------------
// The families' securables
// ------------------------------------------------------------

func (d *Database) Securable() Securable { return Securable{Kind: SecurableDatabase, Name: d.Name} }

func (t *Tablespace) Securable() Securable {
	return Securable{Kind: SecurableTablespace, Name: t.Name}
}

func (s *Schema) Securable() Securable { return Securable{Kind: SecurableSchema, Name: s.Name} }

func (t *Table) Securable() Securable {
	return Securable{Kind: SecurableTable, Schema: t.Schema, Name: t.Name}
}

// Securable is the view's: GRANT takes a view as a table.
func (v *View) Securable() Securable {
	return Securable{Kind: SecurableTable, Schema: v.Schema, Name: v.Name}
}

// Securable is the materialized view's: GRANT takes one as a table.
func (m *MaterializedView) Securable() Securable {
	return Securable{Kind: SecurableTable, Schema: m.Schema, Name: m.Name}
}

// Securable is the foreign table's: GRANT takes one as a table.
func (f *ForeignTable) Securable() Securable {
	return Securable{Kind: SecurableTable, Schema: f.Schema, Name: f.Name}
}

func (q *Sequence) Securable() Securable {
	return Securable{Kind: SecurableSequence, Schema: q.Schema, Name: q.Name}
}

// routineSecurable is a routine's Securable by its Signature's parts; the
// argument list is never nil, so a routine without arguments is named f().
func routineSecurable(kind SecurableKind, id RoutineIdentity) Securable {
	args := id.ArgumentTypeNames
	if args == nil {
		args = []string{}
	}
	return Securable{Kind: kind, Schema: id.Schema, Name: id.Name, Arguments: args}
}

func (f *Function) Securable() Securable {
	return routineSecurable(SecurableFunction, f.RoutineIdentity)
}

func (p *Procedure) Securable() Securable {
	return routineSecurable(SecurableProcedure, p.RoutineIdentity)
}

// Securable is the aggregate's: GRANT … ON FUNCTION takes an aggregate.
func (a *Aggregate) Securable() Securable {
	return routineSecurable(SecurableFunction, a.RoutineIdentity)
}

func (t *Type) Securable() Securable {
	return Securable{Kind: SecurableType, Schema: t.Schema, Name: t.Name}
}

func (m *Domain) Securable() Securable {
	return Securable{Kind: SecurableDomain, Schema: m.Schema, Name: m.Name}
}

func (l *Language) Securable() Securable { return Securable{Kind: SecurableLanguage, Name: l.Name} }

func (w *ForeignDataWrapper) Securable() Securable {
	return Securable{Kind: SecurableForeignDataWrapper, Name: w.Name}
}

func (s *ForeignServer) Securable() Securable {
	return Securable{Kind: SecurableForeignServer, Name: s.Name}
}
