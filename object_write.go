package pgmo

import (
	"context"
	"fmt"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Writes shared by the database-scoped families
// ============================================================
//
// object_write.go is the part of P4.13.3 every family shares: DROP with an
// opt-in CASCADE, the ALTER … RENAME TO / SET SCHEMA / OWNER TO forms, and
// COMMENT ON. A schema-scoped object is written on its database's own
// session (Database.exec → execIn), since PostgreSQL has no USE; a
// cluster-wide one (a tablespace, a role, a database) on the Server's.
//
// What a statement embeds is written in one of three ways: a name through
// QuoteIdent (QualifiedName for schema.name, Signature for a routine); a
// value through QuoteLiteral; and SQL the caller wrote — a type, a default or
// check expression, a view's query, a function's body — as given, since it
// is SQL by definition and no quoting could make it anything else. A field
// holding SQL says so in its doc.

// DropOptions are the choices of DROP for a database-scoped object.
type DropOptions struct {
	// Cascade is CASCADE: the server drops every object that depends on
	// this one too — views over a table, a column's default using a
	// sequence, a foreign key referencing it — and then those depending on
	// them. Without it the server refuses while anything depends on the
	// object (2BP01). Never a default: a caller offering it says so.
	Cascade bool
}

// clause is the tail opts adds to a DROP: " CASCADE", or nothing (the
// server's RESTRICT).
func (o DropOptions) clause() string {
	if o.Cascade {
		return " CASCADE"
	}
	return ""
}

// exec runs stmt on the database's own session, as every write to an
// object inside it must: s.execIn, which inside InTransaction is the
// transaction when it is bound to this database and a refusal otherwise.
// The error is unwrapped: the caller names the operation.
func (d *Database) exec(ctx context.Context, stmt string) error {
	_, err := d.server.execIn(ctx, d.Name, stmt)
	return err
}

// commentStatement is COMMENT ON object IS 'text' — or IS NULL for "",
// which removes the comment: PostgreSQL stores no empty comment (COMMENT …
// IS ” removes it as well), and Comment reads "" for none.
func commentStatement(object, text string) string {
	lit := "NULL"
	if text != "" {
		lit = pgsql.QuoteLiteral(text)
	}
	return "COMMENT ON " + object + " IS " + lit
}

// ddlObject is one object as the shared ALTER/DROP/COMMENT forms name it:
// keyword is the statement's object type ("TABLE", "MATERIALIZED VIEW",
// "FUNCTION"), name its DDL form (quoted, schema-qualified, a routine's
// signature), and run the session it is written on.
type ddlObject struct {
	keyword string
	name    string
	run     func(ctx context.Context, stmt string) error
}

// keywordLabel is a statement's object-type keyword ("MATERIALIZED VIEW")
// as an error message names the kind ("materialized view"). It is SQL
// grammar, never a name.
func keywordLabel(keyword string) string { return strings.ToLower(keyword) }

// op is the operation verb applied to the object, as an error names it:
// `drop table app."T"`.
func (o ddlObject) op(verb string) string {
	return verb + " " + keywordLabel(o.keyword) + " " + o.name
}

// do runs stmt as the operation verb, wrapping a failure once.
func (o ddlObject) do(ctx context.Context, verb, stmt string) error {
	if err := o.run(ctx, stmt); err != nil {
		return fmt.Errorf("pgmo: %s: %w", o.op(verb), err)
	}
	return nil
}

func (o ddlObject) drop(ctx context.Context, opts DropOptions) error {
	return o.do(ctx, "drop", "DROP "+o.keyword+" "+o.name+opts.clause())
}

func (o ddlObject) rename(ctx context.Context, newName string) error {
	if newName == "" {
		return invalidf("pgmo: %s: no new name", o.op("rename"))
	}
	return o.do(ctx, "rename", "ALTER "+o.keyword+" "+o.name+" RENAME TO "+pgsql.QuoteIdent(newName))
}

func (o ddlObject) setSchema(ctx context.Context, schema string) error {
	if schema == "" {
		return fmt.Errorf("pgmo: %s: %w", o.op("move"), ErrSchemaRequired)
	}
	return o.do(ctx, "move", "ALTER "+o.keyword+" "+o.name+" SET SCHEMA "+pgsql.QuoteIdent(schema))
}

func (o ddlObject) setOwner(ctx context.Context, role string) error {
	if role == "" {
		return invalidf("pgmo: %s: no role", o.op("set owner of"))
	}
	return o.do(ctx, "set owner of", "ALTER "+o.keyword+" "+o.name+" OWNER TO "+pgsql.QuoteIdent(role))
}

func (o ddlObject) setComment(ctx context.Context, text string) error {
	return o.do(ctx, "comment on", commentStatement(o.keyword+" "+o.name, text))
}

// requireQualified refuses an object without a name or without a schema,
// before anything is sent: what names it in DDL would otherwise be
// incomplete — and an unqualified name resolves through the session's
// search_path, to whatever it finds first. op is the operation as the
// error names it ("drop table").
func requireQualified(op, schema, name string) error {
	if err := checkQualified(op, schema, name); err != nil {
		return fmt.Errorf("pgmo: %w", err)
	}
	return nil
}

// checkQualified is requireQualified without the "pgmo: " prefix, for a
// name inside a request whose caller names the operation ("references").
func checkQualified(what, schema, name string) error {
	if name == "" {
		return invalidf("%s: no name", what)
	}
	if schema == "" {
		return fmt.Errorf("%s %s: %w", what, pgsql.QuoteIdent(name), ErrSchemaRequired)
	}
	return nil
}

// qualified is a schema-scoped object's DDL handle on database d for the
// operation verb, or the refusal of one missing its schema or name.
func (d *Database) qualified(verb, keyword, schema, name string) (ddlObject, error) {
	if err := requireQualified(verb+" "+keywordLabel(keyword), schema, name); err != nil {
		return ddlObject{}, err
	}
	return ddlObject{keyword: keyword, name: pgsql.QualifiedName(schema, name), run: d.exec}, nil
}

// ============================================================
// Storage parameters: WITH (name = 'value', …)
// ============================================================

// storageParameters is a WITH (…) list of storage parameters — a table's,
// an index's, a materialized view's, a tablespace's — as pg_dump writes
// them: each name an identifier (a "toast." prefix kept as a qualifier, as
// reloptions stores it), each value a literal, which the server parses as
// the parameter's type ('70', 'on'). "" for none.
func storageParameters(params []ParameterSetting) (string, error) {
	if len(params) == 0 {
		return "", nil
	}
	parts := make([]string, len(params))
	for i, p := range params {
		name, err := storageParameterName(p.Name)
		if err != nil {
			return "", err
		}
		parts[i] = name + " = " + pgsql.QuoteLiteral(p.Value)
	}
	return "(" + strings.Join(parts, ", ") + ")", nil
}

// storageParameterName is a storage parameter's name as WITH (…) and RESET
// (…) take it: one identifier, or a namespace and an identifier
// ("toast.autovacuum_enabled").
func storageParameterName(name string) (string, error) {
	ns, rest, dotted := strings.Cut(name, ".")
	switch {
	case name == "":
		return "", invalidf("a storage parameter with no name")
	case !dotted:
		return pgsql.QuoteIdent(name), nil
	case ns == "" || rest == "" || strings.Contains(rest, "."):
		return "", invalidf("storage parameter %q: a name is one word, or a namespace and one word", name)
	}
	return pgsql.QuoteIdent(ns) + "." + pgsql.QuoteIdent(rest), nil
}

// identList is names quoted and comma-separated: a column list's body.
func identList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = pgsql.QuoteIdent(n)
	}
	return strings.Join(q, ", ")
}

// requireNames refuses a list with an empty name in it.
func requireNames(what string, names []string) error {
	for _, n := range names {
		if n == "" {
			return invalidf("an empty name in %s", what)
		}
	}
	return nil
}

// ------------------------------------------------------------
// The shared forms, for a handle's exported methods
// ------------------------------------------------------------
//
// Each takes the handle's ddl method — its DDL form for a verb, or the
// refusal of a handle missing a name — and mirrors a change onto the field
// dst points at (not under WithScript).

type ddlOf func(verb string) (ddlObject, error)

func dropObject(ctx context.Context, ddl ddlOf, opts DropOptions) error {
	o, err := ddl("drop")
	if err != nil {
		return err
	}
	return o.drop(ctx, opts)
}

func renameObject(ctx context.Context, ddl ddlOf, newName string, dst *string) error {
	o, err := ddl("rename")
	if err != nil {
		return err
	}
	if err := o.rename(ctx, newName); err != nil {
		return err
	}
	setIfApplied(ctx, dst, newName)
	return nil
}

func moveObject(ctx context.Context, ddl ddlOf, schema string, dst *string) error {
	o, err := ddl("move")
	if err != nil {
		return err
	}
	if err := o.setSchema(ctx, schema); err != nil {
		return err
	}
	setIfApplied(ctx, dst, schema)
	return nil
}

func ownObject(ctx context.Context, ddl ddlOf, role string, dst *string) error {
	o, err := ddl("set owner of")
	if err != nil {
		return err
	}
	if err := o.setOwner(ctx, role); err != nil {
		return err
	}
	setIfApplied(ctx, dst, role)
	return nil
}

func commentObject(ctx context.Context, ddl ddlOf, text string, dst *string) error {
	o, err := ddl("comment on")
	if err != nil {
		return err
	}
	if err := o.setComment(ctx, text); err != nil {
		return err
	}
	setIfApplied(ctx, dst, text)
	return nil
}
