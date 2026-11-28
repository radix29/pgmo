package pgmo

import (
	"context"
	"fmt"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Scripter (mirrors gosmo's Scripter, SMO's Scripter)
// ============================================================
//
// A Scripter writes the DDL of one object at a time — CREATE, DROP, both, or
// the CREATE OR REPLACE form PostgreSQL has for a few kinds — from what pgmo
// reads of it. It only reads: the script is returned as text, never run.
//
// Where the server renders a part itself (pg_get_viewdef, pg_get_functiondef,
// pg_get_indexdef, pg_get_constraintdef, pg_get_triggerdef, pg_get_expr) the
// script uses that text; the rest — a table's columns, a type, a role, a
// grant — is built from the catalog fields (PLAN.md P5.2).
//
// That text follows the session: a name the session's search_path finds is
// left unqualified, a string literal is written for its
// standard_conforming_strings, a date for its DateStyle. So every Script
// method reads its object in a transaction of its own (render) that pins
// those settings first, as pg_dump does: search_path is "pg_catalog,
// pg_temp", so a rendered name outside pg_catalog is always
// schema-qualified, and the script means the same in any session. With
// SchemaQualify off it is the object's schema first, then those two: names
// in the object's own schema are then written bare, as the object's own name
// is, and the script means the same when run under that search_path.

// ScriptVerb selects which statements a Scripter writes.
type ScriptVerb int

const (
	// ScriptCreate writes the object's CREATE statement and what restates
	// the rest of it — owner, comments, privileges, a table's indexes and
	// triggers (the zero value).
	ScriptCreate ScriptVerb = iota
	// ScriptDrop writes its DROP statement alone.
	ScriptDrop
	// ScriptDropAndCreate writes the DROP and then the CREATE: the
	// re-runnable form.
	ScriptDropAndCreate
	// ScriptAlter writes CREATE OR REPLACE for the kinds PostgreSQL restates
	// whole that way — views, functions, procedures, aggregates, triggers
	// and procedural languages — and falls back to ScriptCreate for every
	// other kind, which has no statement restating the whole object.
	ScriptAlter
)

// String is the verb as SSMS's Script As menu names it.
func (v ScriptVerb) String() string {
	switch v {
	case ScriptCreate:
		return "CREATE"
	case ScriptDrop:
		return "DROP"
	case ScriptDropAndCreate:
		return "DROP and CREATE"
	case ScriptAlter:
		return "ALTER"
	}
	return fmt.Sprintf("ScriptVerb(%d)", int(v))
}

// ScriptOptions controls what a Scripter writes. The zero value writes the
// CREATE statement alone, under the object's bare name; DefaultScriptOptions
// is what a "Script As" wants.
type ScriptOptions struct {
	// Verb selects CREATE (the default), DROP, DROP and CREATE, or ALTER.
	Verb ScriptVerb

	// SchemaQualify writes the object's own name, and its parts', with its
	// schema (app.t); off, bare (t), and the server renders its definitions
	// under a search_path starting with the object's schema, so names in it
	// are bare too. Names pgmo builds from the catalog — a column's type, a
	// foreign key's table — stay qualified either way.
	SchemaQualify bool

	// IncludeOwner writes ALTER … OWNER TO after the CREATE: the script then
	// gives the object the owner it has, not the role running the script.
	IncludeOwner bool

	// IncludeComments writes COMMENT ON for the object and its parts
	// (columns, constraints, indexes, triggers, policies) that have one.
	IncludeComments bool

	// IncludePrivileges writes the GRANT and REVOKE statements turning the
	// kind's default access into the object's ACL, as pg_dump does; nothing
	// when the ACL was never set. A schema's default privileges
	// (ALTER DEFAULT PRIVILEGES … IN SCHEMA) are written with it.
	IncludePrivileges bool

	// IfExists writes DROP … IF EXISTS, so the DROP of an object that is
	// not there succeeds.
	IfExists bool

	// IfNotExists writes CREATE … IF NOT EXISTS where PostgreSQL's CREATE
	// has it — a schema, table, foreign table, sequence, index, materialized
	// view, extension, collation, foreign server, user mapping. Other kinds'
	// CREATE is written unguarded: PostgreSQL has no such form for them.
	// What follows the CREATE (owner, comments, grants) runs either way and
	// restates the object as it was read.
	IfNotExists bool

	// Cascade writes DROP … CASCADE: what depends on the object is dropped
	// with it. A database, role, tablespace and user mapping have no
	// CASCADE and are dropped without it.
	Cascade bool
}

// DefaultScriptOptions returns the options a "Script As" wants: CREATE,
// schema-qualified, with owner, comments and privileges.
func DefaultScriptOptions() ScriptOptions {
	return ScriptOptions{
		SchemaQualify:     true,
		IncludeOwner:      true,
		IncludeComments:   true,
		IncludePrivileges: true,
	}
}

// Scripter writes DDL for the objects of one database. Build it with
// NewScripter; its methods are safe for concurrent use, each reading in a
// transaction of its own.
type Scripter struct {
	db   *Database
	opts ScriptOptions
}

// NewScripter returns a Scripter for the objects of database db (a handle
// from DatabaseByName or DatabaseRef), writing with opts.
func NewScripter(db *Database, opts ScriptOptions) *Scripter {
	return &Scripter{db: db, opts: opts}
}

// Database is the database the Scripter scripts.
func (sc *Scripter) Database() *Database { return sc.db }

// Options are the options the Scripter writes with.
func (sc *Scripter) Options() ScriptOptions { return sc.opts }

// ServerScripter writes DDL for the cluster-wide objects of a server:
// roles and tablespaces. Databases are Scripter.ScriptDatabase's.
type ServerScripter struct {
	server *Server
	opts   ScriptOptions
}

// NewServerScripter returns a ServerScripter for s, writing with opts.
func NewServerScripter(s *Server, opts ScriptOptions) *ServerScripter {
	return &ServerScripter{server: s, opts: opts}
}

// Server is the server the ServerScripter scripts.
func (sc *ServerScripter) Server() *Server { return sc.server }

// Options are the options the ServerScripter writes with.
func (sc *ServerScripter) Options() ScriptOptions { return sc.opts }

// ------------------------------------------------------------
// The script text
// ------------------------------------------------------------

// scriptText is a script being written: statements, each terminated once
// when rendered, and notes, written as -- comments and never terminated
// (a semicolon after a -- comment would be swallowed by it).
type scriptText struct {
	items []scriptItem
}

type scriptItem struct {
	text string
	note bool
}

// add appends statements, skipping empty ones.
func (s *scriptText) add(stmts ...string) {
	for _, stmt := range stmts {
		if stmt != "" {
			s.items = append(s.items, scriptItem{text: stmt})
		}
	}
}

// notef appends a -- note; every name in it is made comment-safe.
func (s *scriptText) notef(format string, args ...any) {
	s.items = append(s.items, scriptItem{text: "-- " + pgsql.CommentSafe(fmt.Sprintf(format, args...)), note: true})
}

// String renders the script as ScriptCollector.String does: each statement
// terminated, a blank line between items.
func (s *scriptText) String() string {
	var b strings.Builder
	for i, it := range s.items {
		if i > 0 {
			b.WriteString("\n")
		}
		if it.note {
			b.WriteString(it.text)
		} else {
			b.WriteString(terminated(it.text))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ------------------------------------------------------------
// Verbs
// ------------------------------------------------------------

// script writes one object under the verb: drop is its DROP statement,
// create writes its CREATE and what follows it. Under DROP AND CREATE an
// error from create discards the DROP too: a script that drops the object
// and cannot put it back is never returned.
func (o ScriptOptions) script(op, drop string, create func(s *scriptText) error) (string, error) {
	var s scriptText
	switch o.Verb {
	case ScriptCreate, ScriptAlter:
	case ScriptDrop:
		s.add(drop)
		return s.String(), nil
	case ScriptDropAndCreate:
		s.add(drop)
	default:
		return "", invalidf("pgmo: %s: unknown verb %d", op, int(o.Verb))
	}
	if err := create(&s); err != nil {
		return "", err
	}
	return s.String(), nil
}

// member is script for an extension's member: its DDL is CREATE
// EXTENSION's, and DROP on it alone is refused (2BP01), so the script is a
// note saying so whatever the verb (OPEN-THREADS.md § System objects).
func (o ScriptOptions) member(label string, origin Origin) (string, bool) {
	if !origin.IsExtensionMember() {
		return "", false
	}
	var s scriptText
	s.notef("%s is a member of extension %s: CREATE EXTENSION creates it and DROP EXTENSION drops it", label, pgsql.QuoteIdent(origin.Extension))
	return s.String(), true
}

// drop is DROP keyword name, with IF EXISTS and CASCADE as o asks.
func (o ScriptOptions) drop(keyword, name string) string {
	return o.dropOn(keyword, name, "")
}

// dropOn is drop for an object named on a table (a trigger, a policy):
// DROP keyword name ON table.
func (o ScriptOptions) dropOn(keyword, name, table string) string {
	stmt := "DROP " + keyword
	if o.IfExists {
		stmt += " IF EXISTS"
	}
	stmt += " " + name
	if table != "" {
		stmt += " ON " + table
	}
	if o.Cascade {
		stmt += " CASCADE"
	}
	return stmt
}

// dropPlain is drop for a kind whose DROP takes no CASCADE (a role, a
// database, a tablespace, a user mapping).
func (o ScriptOptions) dropPlain(keyword, name string) string {
	stmt := "DROP " + keyword
	if o.IfExists {
		stmt += " IF EXISTS"
	}
	return stmt + " " + name
}

// ifNotExists is " IF NOT EXISTS" when o asks for it, for the kinds whose
// CREATE has it.
func (o ScriptOptions) ifNotExists() string {
	if o.IfNotExists {
		return " IF NOT EXISTS"
	}
	return ""
}

// orReplace is "OR REPLACE " under ScriptAlter, for the kinds that have it.
func (o ScriptOptions) orReplace() string {
	if o.Verb == ScriptAlter {
		return "OR REPLACE "
	}
	return ""
}

// name is a schema-scoped object's name as the script writes it: qualified,
// or bare with SchemaQualify off.
func (o ScriptOptions) name(schema, name string) string {
	if o.SchemaQualify || schema == "" {
		return pgsql.QualifiedName(schema, name)
	}
	return pgsql.QuoteIdent(name)
}

// owner is ALTER keyword name OWNER TO role, when o includes owners and the
// owner is known.
func (o ScriptOptions) owner(keyword, name, role string) string {
	if !o.IncludeOwner || role == "" {
		return ""
	}
	return "ALTER " + keyword + " " + name + " OWNER TO " + pgsql.QuoteIdent(role)
}

// comment is COMMENT ON object IS 'text', when o includes comments and
// there is one.
func (o ScriptOptions) comment(object, text string) string {
	if !o.IncludeComments || text == "" {
		return ""
	}
	return commentStatement(object, text)
}

// ------------------------------------------------------------
// Rendering under fixed settings
// ------------------------------------------------------------

// renderSettings are the session settings a script is read under, as
// pg_dump sets them: names qualified by search_path, string literals in
// the standard form, dates in ISO, intervals in the postgres style, floats
// with every digit. search_path is the caller's (renderPath).
const renderSettings = `SELECT pg_catalog.set_config('search_path', $1, true),
	pg_catalog.set_config('standard_conforming_strings', 'on', true),
	pg_catalog.set_config('DateStyle', 'ISO', true),
	pg_catalog.set_config('IntervalStyle', 'postgres', true),
	pg_catalog.set_config('extra_float_digits', '3', true)`

// renderPath is the search_path an object of schema is rendered under:
// "pg_catalog, pg_temp" when qualifying, so every name outside pg_catalog is
// written with its schema; with the object's schema first otherwise.
func (o ScriptOptions) renderPath(schema string) string {
	if o.SchemaQualify || schema == "" {
		return "pg_catalog, pg_temp"
	}
	return pgsql.QuoteIdent(schema) + ", pg_catalog, pg_temp"
}

// renderIn runs fn with a context whose reads of database run in one
// transaction of their own, under renderSettings with search_path path,
// and rolls it back after. Inside the caller's InTransaction on that
// database it is a savepoint of it, so the script sees the transaction's
// uncommitted changes and leaves its settings as they were; another
// database there is refused, as any read of it. Reads of the server's own
// catalog (roles) from fn's context go to the Server's own Querier when
// database is another one.
func (s *Server) renderIn(ctx context.Context, database, path string, fn func(ctx context.Context) error) error {
	q, err := s.onDatabase(ctx, database)
	if err != nil {
		return err
	}
	bctx, release := s.bound(ctx)
	defer release()
	tx, err := q.Begin(bctx)
	if err != nil {
		return fmt.Errorf("pgmo: database %q: begin script read: %w", database, cancelCause(bctx, err))
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(bctx), txRollbackTimeout)
		_ = tx.Rollback(rctx)
		cancel()
	}()
	if _, err := s.execOn(bctx, tx, renderSettings, []any{path}); err != nil {
		return fmt.Errorf("pgmo: database %q: set script read settings: %w", database, err)
	}
	return fn(context.WithValue(bctx, txCtxKey{}, &serverTx{server: s, tx: tx, database: database}))
}

// render is renderIn on the Scripter's database, under the path an object
// of schema is rendered under.
func (sc *Scripter) render(ctx context.Context, schema string, fn func(ctx context.Context) error) error {
	return sc.db.server.renderIn(ctx, sc.db.Name, sc.opts.renderPath(schema), fn)
}

// render is renderIn on the Server's own database.
func (sc *ServerScripter) render(ctx context.Context, fn func(ctx context.Context) error) error {
	return sc.server.renderIn(ctx, sc.server.database, sc.opts.renderPath(""), fn)
}

// scriptOf reads with read under render, then writes the script with
// write: the shape of every Script method.
func scriptOf[T any](ctx context.Context, render func(context.Context, func(context.Context) error) error,
	read func(ctx context.Context) (T, error), write func(T) (string, error)) (string, error) {
	var v T
	err := render(ctx, func(ctx context.Context) error {
		var err error
		v, err = read(ctx)
		return err
	})
	if err != nil {
		return "", err
	}
	return write(v)
}

// in is render bound to schema, for scriptOf.
func (sc *Scripter) in(schema string) func(context.Context, func(context.Context) error) error {
	return func(ctx context.Context, fn func(context.Context) error) error { return sc.render(ctx, schema, fn) }
}
