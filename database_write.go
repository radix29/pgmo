package pgmo

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Database writes
// ============================================================
//
// database_write.go is P4.13.1: CREATE DATABASE, DROP DATABASE (WITH (FORCE)
// opt-in), and the ALTER DATABASE forms — RENAME TO, OWNER TO, SET
// TABLESPACE, WITH <options>, SET/RESET <parameter>. Every statement runs on
// the Server's own database (s.exec): a database is a cluster-wide object,
// written from any session but its own.
//
// Three of them need the database free of sessions — DROP, RENAME and SET
// TABLESPACE — and so cannot touch the database the Server is connected to
// (refused here, before anything is sent: the server's own answers are 55006
// and 0A000), and close the Server's own idle pool on it first
// (releaseDatabase), which would otherwise be the session in the way. CREATE
// DATABASE, DROP DATABASE and SET TABLESPACE cannot run in a transaction
// block, and are refused under InTransaction (refuseInTx).

// DatabaseStrategy is CREATE DATABASE's STRATEGY: how the template is copied.
type DatabaseStrategy string

const (
	// DatabaseStrategyWALLog copies block by block through the WAL — the
	// server's default: no checkpoint, efficient for a small template.
	DatabaseStrategyWALLog DatabaseStrategy = "WAL_LOG"
	// DatabaseStrategyFileCopy copies the files, with a checkpoint before
	// and after — faster for a large template, at the cost of the checkpoints.
	DatabaseStrategyFileCopy DatabaseStrategy = "FILE_COPY"
)

// CreateDatabaseRequest describes a new database. Only Name is required; every
// other field left zero is left out of the statement, so the server's default
// applies — the template's encoding and locale (template1 by default), the
// connected role as owner, pg_default unless the template has another.
//
// Each field is one CREATE DATABASE option, written as given: pgmo does not
// derive one from another. LOCALE sets LC_COLLATE and LC_CTYPE, and from 16
// the ICU locale too under LOCALE_PROVIDER icu (from 17 the builtin locale
// under builtin); LC_COLLATE, LC_CTYPE, ICU_LOCALE and BUILTIN_LOCALE name
// one each. A locale or encoding other than the template's needs template0 —
// the server refuses the rest (22023).
type CreateDatabaseRequest struct {
	Name string

	Owner    string // OWNER: a role name; "" is the connected role
	Template string // TEMPLATE: the database to copy; "" is template1
	Encoding string // ENCODING: UTF8, LATIN1, SQL_ASCII…

	Strategy DatabaseStrategy // STRATEGY; "" is the server's (WAL_LOG)

	// LocaleProvider is LOCALE_PROVIDER: libc, icu, or builtin (17+).
	// LocaleProviderDefault is a collation's alone and is refused.
	LocaleProvider LocaleProvider
	Locale         string // LOCALE
	Collate        string // LC_COLLATE
	CType          string // LC_CTYPE
	ICULocale      string // ICU_LOCALE
	ICURules       string // ICU_RULES, 16+
	BuiltinLocale  string // BUILTIN_LOCALE, 17+

	Tablespace string // TABLESPACE: the default tablespace

	// AllowConnections, ConnectionLimit and IsTemplate are written when set:
	// ALLOW_CONNECTIONS, CONNECTION LIMIT (-1 for none), IS_TEMPLATE.
	AllowConnections *bool
	ConnectionLimit  *int
	IsTemplate       *bool
}

// createDatabaseStatement is req's CREATE DATABASE on a server at num, or the
// refusal of a field it cannot write: ErrInvalidRequest for a value no server
// takes, ErrUnsupportedVersion for an option num predates. No prefix: the
// caller names the operation.
func createDatabaseStatement(num int, req CreateDatabaseRequest) (string, error) {
	if req.Strategy != "" && req.Strategy != DatabaseStrategyWALLog && req.Strategy != DatabaseStrategyFileCopy {
		return "", invalidf("unknown strategy %q; it is %s or %s", string(req.Strategy), DatabaseStrategyWALLog, DatabaseStrategyFileCopy)
	}
	switch req.LocaleProvider {
	case "", LocaleProviderLibc, LocaleProviderICU:
	case LocaleProviderBuiltin:
		if before(num, PostgreSQL17) {
			return "", refusalf(ErrUnsupportedVersion, "the builtin locale provider needs %s or later; the server is %s", PostgreSQL17, ServerVersion(num))
		}
	default:
		return "", invalidf("%s is not a database's locale provider; it is libc, icu or builtin", req.LocaleProvider)
	}
	if req.ICURules != "" && before(num, PostgreSQL16) {
		return "", refusalf(ErrUnsupportedVersion, "ICU_RULES needs %s or later; the server is %s", PostgreSQL16, ServerVersion(num))
	}
	if req.BuiltinLocale != "" && before(num, PostgreSQL17) {
		return "", refusalf(ErrUnsupportedVersion, "BUILTIN_LOCALE needs %s or later; the server is %s", PostgreSQL17, ServerVersion(num))
	}
	if req.ConnectionLimit != nil && *req.ConnectionLimit < -1 {
		return "", invalidf("connection limit %d; it is -1 (none) or more", *req.ConnectionLimit)
	}

	var opts []string
	ident := func(option, v string) {
		if v != "" {
			opts = append(opts, option+" = "+pgsql.QuoteIdent(v))
		}
	}
	literal := func(option, v string) {
		if v != "" {
			opts = append(opts, option+" = "+pgsql.QuoteLiteral(v))
		}
	}
	ident("OWNER", req.Owner)
	ident("TEMPLATE", req.Template)
	literal("ENCODING", req.Encoding)
	if req.Strategy != "" {
		opts = append(opts, "STRATEGY = "+string(req.Strategy))
	}
	if req.LocaleProvider != "" {
		opts = append(opts, "LOCALE_PROVIDER = "+req.LocaleProvider.String())
	}
	literal("LOCALE", req.Locale)
	literal("LC_COLLATE", req.Collate)
	literal("LC_CTYPE", req.CType)
	literal("ICU_LOCALE", req.ICULocale)
	literal("ICU_RULES", req.ICURules)
	literal("BUILTIN_LOCALE", req.BuiltinLocale)
	ident("TABLESPACE", req.Tablespace)
	opts = append(opts, connectionOptions(req.AllowConnections, req.ConnectionLimit, req.IsTemplate)...)

	stmt := "CREATE DATABASE " + pgsql.QuoteIdent(req.Name)
	if len(opts) > 0 {
		stmt += " WITH " + strings.Join(opts, " ")
	}
	return stmt, nil
}

// connectionOptions are the ALLOW_CONNECTIONS, CONNECTION LIMIT and
// IS_TEMPLATE options CREATE DATABASE and ALTER DATABASE … WITH share, for
// the ones set.
func connectionOptions(allow *bool, limit *int, template *bool) []string {
	var opts []string
	if allow != nil {
		opts = append(opts, "ALLOW_CONNECTIONS = "+strconv.FormatBool(*allow))
	}
	if limit != nil {
		opts = append(opts, "CONNECTION LIMIT = "+strconv.Itoa(*limit))
	}
	if template != nil {
		opts = append(opts, "IS_TEMPLATE = "+strconv.FormatBool(*template))
	}
	return opts
}

// CreateDatabase creates the database req describes, and returns it read back
// from the catalog — or, under Scripting(ctx), the DatabaseRef handle, since
// nothing ran.
//
// CREATE DATABASE cannot run in a transaction block: inside InTransaction it
// is refused with ErrUnsupported before anything is sent. An option the
// server is too old for (ICU_RULES before 16, the builtin provider and
// BUILTIN_LOCALE before 17) is refused with ErrUnsupportedVersion. The role
// needs CREATEDB (or superuser), and membership in Owner when it names
// another role.
func (s *Server) CreateDatabase(ctx context.Context, req CreateDatabaseRequest) (*Database, error) {
	if req.Name == "" {
		return nil, invalidf("pgmo: create database: no name")
	}
	op := fmt.Sprintf("create database %q", req.Name)
	if err := refuseInTx(ctx, s, op); err != nil {
		return nil, err
	}
	stmt, err := createDatabaseStatement(int(s.version), req)
	if err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	if _, err := s.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return createdObject(ctx, s.DatabaseRef(req.Name), func() (*Database, error) {
		return s.DatabaseByName(ctx, req.Name)
	})
}

// DropDatabaseOptions are Database.Drop's choices.
type DropDatabaseOptions struct {
	// Force is DROP DATABASE … WITH (FORCE): the server terminates every
	// other session connected to the database first (it signals them as
	// pg_terminate_backend would, and needs the same right to), instead of
	// refusing while one is (55006). It does not get past a prepared
	// transaction, an active logical replication slot or a subscription in
	// the database.
	Force bool
}

// Drop drops the database: DROP DATABASE, with WITH (FORCE) when opts.Force.
// The server refuses one with another session connected — unless Force —
// and a template (IsTemplate: clear it first with Alter).
//
// The database the Server is connected to cannot be dropped from its own
// session, and is refused with ErrInvalidRequest before anything is sent;
// connect the Server to another (postgres). DROP DATABASE cannot run in a
// transaction block: inside InTransaction it is refused with ErrUnsupported.
// The Server's own pool on the database, opened by a read through
// DatabaseQuerier, is closed first when idle, so it is not the session in
// the way (see releaseDatabase); one in use is left, and is that session.
func (d *Database) Drop(ctx context.Context, opts DropDatabaseOptions) error {
	s := d.server
	if d.Name == "" {
		return invalidf("pgmo: drop database: no name")
	}
	op := fmt.Sprintf("drop database %q", d.Name)
	if err := d.refuseOwnSession(op); err != nil {
		return err
	}
	if err := refuseInTx(ctx, s, op); err != nil {
		return err
	}
	stmt := "DROP DATABASE " + pgsql.QuoteIdent(d.Name)
	if opts.Force {
		stmt += " WITH (FORCE)"
	}
	s.releaseDatabase(ctx, d.Name)
	if _, err := s.exec(ctx, stmt); err != nil {
		return fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return nil
}

// Rename renames the database: ALTER DATABASE … RENAME TO, mirrored onto Name
// (not under WithScript). The server refuses it while any other session is
// connected to the database (55006) — there is no FORCE for it — so the
// Server's own idle pool on it is closed first, as for Drop. The database
// the Server is connected to cannot be renamed, and is refused with
// ErrInvalidRequest. It may run inside InTransaction.
func (d *Database) Rename(ctx context.Context, newName string) error {
	if d.Name == "" || newName == "" {
		return invalidf("pgmo: rename database: both names are required")
	}
	op := fmt.Sprintf("rename database %q to %q", d.Name, newName)
	if err := d.refuseOwnSession(op); err != nil {
		return err
	}
	d.server.releaseDatabase(ctx, d.Name)
	if _, err := d.server.exec(ctx, "ALTER DATABASE "+pgsql.QuoteIdent(d.Name)+" RENAME TO "+pgsql.QuoteIdent(newName)); err != nil {
		return fmt.Errorf("pgmo: %s: %w", op, err)
	}
	setIfApplied(ctx, &d.Name, newName)
	return nil
}

// SetOwner makes role the database's owner: ALTER DATABASE … OWNER TO,
// mirrored onto Owner (not under WithScript). The role needs to own the
// database (or be superuser), be a member of role, and hold CREATEDB.
func (d *Database) SetOwner(ctx context.Context, role string) error {
	if d.Name == "" || role == "" {
		return invalidf("pgmo: set database owner: both the database and the role are required")
	}
	if _, err := d.server.exec(ctx, "ALTER DATABASE "+pgsql.QuoteIdent(d.Name)+" OWNER TO "+pgsql.QuoteIdent(role)); err != nil {
		return fmt.Errorf("pgmo: set database %q owner to %q: %w", d.Name, role, err)
	}
	setIfApplied(ctx, &d.Owner, role)
	return nil
}

// SetTablespace moves the database to tablespace — its catalogs and every
// object in its old default tablespace: ALTER DATABASE … SET TABLESPACE,
// mirrored onto Tablespace (not under WithScript). The server copies the
// files, and refuses while any other session is connected to the database
// (55006), so the Server's own idle pool on it is closed first, as for
// Drop. The database the Server is connected to cannot be moved
// (ErrInvalidRequest), and the statement cannot run in a transaction block
// (ErrUnsupported inside InTransaction).
func (d *Database) SetTablespace(ctx context.Context, tablespace string) error {
	if d.Name == "" || tablespace == "" {
		return invalidf("pgmo: set database tablespace: both the database and the tablespace are required")
	}
	op := fmt.Sprintf("set database %q tablespace to %q", d.Name, tablespace)
	if err := d.refuseOwnSession(op); err != nil {
		return err
	}
	if err := refuseInTx(ctx, d.server, op); err != nil {
		return err
	}
	d.server.releaseDatabase(ctx, d.Name)
	if _, err := d.server.exec(ctx, "ALTER DATABASE "+pgsql.QuoteIdent(d.Name)+" SET TABLESPACE "+pgsql.QuoteIdent(tablespace)); err != nil {
		return fmt.Errorf("pgmo: %s: %w", op, err)
	}
	setIfApplied(ctx, &d.Tablespace, tablespace)
	return nil
}

// refuseOwnSession refuses op on the database the Server is connected to,
// which a statement needing the database free of sessions cannot reach from
// its own (the server answers 55006, or 0A000 for a rename).
func (d *Database) refuseOwnSession(op string) error {
	if d.Name != d.server.database {
		return nil
	}
	return invalidf("pgmo: %s: the Server is connected to it; connect it to another database (postgres) to do this", op)
}

// DatabaseChanges is a partial ALTER DATABASE … WITH: each non-nil field is
// written, the rest left as they are.
type DatabaseChanges struct {
	// AllowConnections false closes the database to every new connection,
	// superusers' included; sessions already connected stay.
	AllowConnections *bool
	// ConnectionLimit is the most concurrent connections, -1 for none.
	ConnectionLimit *int
	// IsTemplate lets any role with CREATEDB clone the database; a template
	// cannot be dropped until it is cleared.
	IsTemplate *bool
}

// Alter applies every field set on c in one ALTER DATABASE … WITH, mirrored
// onto the matching fields (not under WithScript). An empty c sends nothing.
// A connection limit below -1 is refused with ErrInvalidRequest.
func (d *Database) Alter(ctx context.Context, c DatabaseChanges) error {
	opts := connectionOptions(c.AllowConnections, c.ConnectionLimit, c.IsTemplate)
	if len(opts) == 0 {
		return nil
	}
	if d.Name == "" {
		return invalidf("pgmo: alter database: no name")
	}
	if c.ConnectionLimit != nil && *c.ConnectionLimit < -1 {
		return invalidf("pgmo: alter database %q: connection limit %d; it is -1 (none) or more", d.Name, *c.ConnectionLimit)
	}
	if _, err := d.server.exec(ctx, "ALTER DATABASE "+pgsql.QuoteIdent(d.Name)+" WITH "+strings.Join(opts, " ")); err != nil {
		return fmt.Errorf("pgmo: alter database %q: %w", d.Name, err)
	}
	setPtrIfApplied(ctx, &d.AllowConnections, c.AllowConnections)
	setPtrIfApplied(ctx, &d.ConnectionLimit, c.ConnectionLimit)
	setPtrIfApplied(ctx, &d.IsTemplate, c.IsTemplate)
	return nil
}

// ------------------------------------------------------------
// Parameter settings: ALTER DATABASE … SET / RESET
// ------------------------------------------------------------

// SetParameter stores a default for configuration parameter name, applied to
// every session that starts in the database: ALTER DATABASE … SET name =
// 'value'[, …]. Each value is written as a string literal, which the server
// parses as the parameter's type ('64MB', 'on'); several values set a list
// parameter one element each (search_path: "$user", public), as
// ParameterSetting.Values splits a stored one. A setting the server stored is
// written back with SetParameter(ctx, p.Name, p.Values()...).
//
// Settings is not mirrored: the server stores the parameter's canonical name
// and flattened value, so read the database again to see it. The role needs
// to own the database, and superuser for a superuser-only parameter.
func (d *Database) SetParameter(ctx context.Context, name string, values ...string) error {
	if d.Name == "" || name == "" {
		return invalidf("pgmo: set database parameter: both the database and the parameter are required")
	}
	if len(values) == 0 {
		return invalidf("pgmo: set database %q parameter %q: no value; ResetParameter removes one", d.Name, name)
	}
	stmt := "ALTER DATABASE " + pgsql.QuoteIdent(d.Name) + " SET " + parameterAssignment(name, values)
	if _, err := d.server.exec(ctx, stmt); err != nil {
		return fmt.Errorf("pgmo: set database %q parameter %q: %w", d.Name, name, err)
	}
	return nil
}

// parameterAssignment is SET's "name = 'value'[, …]" for a database's or a
// role's stored default: the name an identifier, each value a literal.
func parameterAssignment(name string, values []string) string {
	lits := make([]string, len(values))
	for i, v := range values {
		lits[i] = pgsql.QuoteLiteral(v)
	}
	return pgsql.QuoteIdent(name) + " = " + strings.Join(lits, ", ")
}

// ResetParameter removes the database's stored default for parameter name:
// ALTER DATABASE … RESET name. Removing one that is not stored succeeds.
// Settings is not mirrored, as for SetParameter.
func (d *Database) ResetParameter(ctx context.Context, name string) error {
	if d.Name == "" || name == "" {
		return invalidf("pgmo: reset database parameter: both the database and the parameter are required")
	}
	if _, err := d.server.exec(ctx, "ALTER DATABASE "+pgsql.QuoteIdent(d.Name)+" RESET "+pgsql.QuoteIdent(name)); err != nil {
		return fmt.Errorf("pgmo: reset database %q parameter %q: %w", d.Name, name, err)
	}
	return nil
}

// ResetAllParameters removes every default stored for the database (ALTER
// DATABASE … RESET ALL), mirrored onto Settings (not under WithScript). The
// defaults stored for a role in the database (ALTER ROLE … IN DATABASE) are
// the role's, and stay.
func (d *Database) ResetAllParameters(ctx context.Context) error {
	if d.Name == "" {
		return invalidf("pgmo: reset database parameters: no name")
	}
	if _, err := d.server.exec(ctx, "ALTER DATABASE "+pgsql.QuoteIdent(d.Name)+" RESET ALL"); err != nil {
		return fmt.Errorf("pgmo: reset database %q parameters: %w", d.Name, err)
	}
	setIfApplied(ctx, &d.Settings, nil)
	return nil
}

// listQuoteParameters are the parameters the server stores as a list of
// quoted elements (GUC_LIST_QUOTE): every built-in one, as pg_dump's
// variable_is_guc_list_quote lists them. The flag is not exposed through
// SQL, so an extension's list-quoted parameter cannot be told and reads as
// one value — pg_dump has the same limit.
var listQuoteParameters = map[string]bool{
	"local_preload_libraries":   true,
	"search_path":               true,
	"session_preload_libraries": true,
	"shared_preload_libraries":  true,
	"temp_tablespaces":          true,
	"unix_socket_directories":   true,
}

// Values is the setting as SetParameter takes it back. A list-quoted
// parameter (search_path, temp_tablespaces, the preload libraries) is stored
// with each element double-quoted where it needs it — "$user", public — and
// is split into its elements, unquoted, as pg_dump splits it (SplitGUCList).
// Any other parameter is its one Value: the server parses a list such as
// DateStyle's 'ISO, MDY' from the one literal. A list-quoted value that does
// not split — malformed, or empty — is its one Value too.
func (p ParameterSetting) Values() []string {
	if listQuoteParameters[p.Name] {
		if elems, ok := splitGUCList(p.Value); ok && len(elems) > 0 {
			return elems
		}
	}
	return []string{p.Value}
}

// splitGUCList splits a list-quoted parameter's stored value at its commas:
// an element is double-quoted, with "" for a quote inside, or runs to the
// next comma or whitespace; whitespace around elements is skipped. It is
// fe_utils' SplitGUCList, which pg_dump uses on the same text; ok is false
// where that returns false (an unterminated quote, an empty unquoted
// element, text after an element). Whitespace is the server's scanner's:
// space, tab, newline, carriage return, vertical tab, form feed.
func splitGUCList(s string) (elems []string, ok bool) {
	isSpace := func(c byte) bool { return strings.IndexByte(" \t\n\r\v\f", c) >= 0 }
	i := 0
	skip := func() {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
	}
	skip()
	if i == len(s) {
		return nil, true
	}
	for {
		if i == len(s) {
			return nil, false // a comma ending the list
		}
		var elem strings.Builder
		if s[i] == '"' {
			i++
			for {
				j := strings.IndexByte(s[i:], '"')
				if j < 0 {
					return nil, false
				}
				elem.WriteString(s[i : i+j])
				i += j + 1
				if i < len(s) && s[i] == '"' {
					elem.WriteByte('"')
					i++
					continue
				}
				break
			}
		} else {
			start := i
			for i < len(s) && s[i] != ',' && !isSpace(s[i]) {
				i++
			}
			if i == start {
				return nil, false
			}
			elem.WriteString(s[start:i])
		}
		elems = append(elems, elem.String())
		skip()
		switch {
		case i == len(s):
			return elems, true
		case s[i] == ',':
			i++
			skip()
		default:
			return nil, false
		}
	}
}
