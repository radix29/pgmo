package pgmo

import (
	"context"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Scripting databases, roles and tablespaces
// ============================================================

// ScriptDatabase scripts the Scripter's database: CREATE DATABASE from
// template0 with its encoding, locale provider and locales, tablespace and
// connection settings, its stored parameter defaults (ALTER DATABASE … SET),
// owner and comment. It is read through the Server's own database, so a
// database that takes no connections is scripted too. CREATE DATABASE runs outside a transaction block, on a
// session of another database: the script says so in a note. The objects in
// it are scripted on their own; its own ACL is not read yet
// (OPEN-THREADS.md § Privilege writes).
func (sc *Scripter) ScriptDatabase(ctx context.Context) (string, error) {
	d := sc.db
	op := "script database " + pgsql.QuoteIdent(d.Name)
	// Read on the Server's own database: pg_database is shared, and the
	// database itself may refuse connections (ALLOW_CONNECTIONS false).
	render := func(ctx context.Context, fn func(context.Context) error) error {
		return d.server.renderIn(ctx, d.server.database, sc.opts.renderPath(""), fn)
	}
	return scriptOf(ctx, render, func(ctx context.Context) (*Database, error) {
		return d.server.DatabaseByName(ctx, d.Name)
	}, func(db *Database) (string, error) {
		o := sc.opts
		dname := pgsql.QuoteIdent(db.Name)
		return o.script(op, o.dropPlain("DATABASE", dname), func(s *scriptText) error {
			req := CreateDatabaseRequest{Name: db.Name, Template: "template0", Encoding: db.Encoding,
				LocaleProvider: db.LocaleProvider, Collate: db.Collate, CType: db.CType, ICURules: db.ICURules,
				Tablespace: db.Tablespace}
			switch db.LocaleProvider {
			case LocaleProviderICU:
				req.ICULocale = db.Locale
			case LocaleProviderBuiltin:
				req.BuiltinLocale = db.Locale
			}
			if db.Tablespace == "pg_default" {
				req.Tablespace = ""
			}
			if !db.AllowConnections {
				req.AllowConnections = &db.AllowConnections
			}
			if db.ConnectionLimit != -1 {
				req.ConnectionLimit = &db.ConnectionLimit
			}
			if db.IsTemplate {
				req.IsTemplate = &db.IsTemplate
			}
			create, err := createDatabaseStatement(int(d.server.version), req)
			if err != nil {
				return wrapScript(op, err)
			}
			s.notef("CREATE DATABASE cannot run inside a transaction block; run the script connected to another database")
			s.add(create)
			for _, p := range db.Settings {
				s.add("ALTER DATABASE " + dname + " SET " + parameterAssignment(p.Name, p.Values()))
			}
			s.add(o.owner("DATABASE", dname, db.Owner))
			s.add(o.comment("DATABASE "+dname, db.Comment))
			return nil
		})
	})
}

// ScriptRole scripts the role name: CREATE ROLE with every attribute
// stated, its parameter defaults (ALTER ROLE … SET, and … IN DATABASE …
// SET), the roles it is a member of and its own members (GRANT, with the
// grant's options), and its comment. Its password is never readable, so
// never scripted: a note says so for a role that can log in.
func (sc *ServerScripter) ScriptRole(ctx context.Context, name string) (string, error) {
	s := sc.server
	op := "script role " + pgsql.QuoteIdent(name)
	return scriptOf(ctx, sc.render, func(ctx context.Context) (*Role, error) {
		return s.RoleByName(ctx, name)
	}, func(r *Role) (string, error) {
		o := sc.opts
		rname := pgsql.QuoteIdent(r.Name)
		return o.script(op, o.dropPlain("ROLE", rname), func(t *scriptText) error {
			attrs := RoleAttributes{Superuser: &r.Superuser, Inherit: &r.Inherit, CreateRole: &r.CreateRole,
				CreateDB: &r.CreateDB, CanLogin: &r.CanLogin, Replication: &r.Replication, BypassRLS: &r.BypassRLS,
				ConnectionLimit: &r.ConnectionLimit}
			if !r.ValidUntil.IsZero() {
				attrs.ValidUntil = &r.ValidUntil
			}
			opts, err := attrs.options()
			if err != nil {
				return wrapScript(op, err)
			}
			t.add("CREATE ROLE " + rname + " WITH " + strings.Join(opts, " "))
			if r.CanLogin {
				t.notef("the role's password is not readable and is not scripted: set it with ALTER ROLE … PASSWORD")
			}
			for _, p := range r.Settings {
				t.add("ALTER ROLE " + rname + " SET " + parameterAssignment(p.Name, p.Values()))
			}
			for _, ds := range r.DatabaseSettings {
				for _, p := range ds.Settings {
					t.add("ALTER ROLE " + rname + " IN DATABASE " + pgsql.QuoteIdent(ds.Database) + " SET " + parameterAssignment(p.Name, p.Values()))
				}
			}
			for _, m := range append(append([]RoleMembership(nil), r.MemberOf...), r.Members...) {
				stmt, err := grantMembershipStatement(int(s.version), m.Role, m.Member, membershipOptions(m, s.version))
				if err != nil {
					return wrapScript(op, err)
				}
				t.add(stmt)
			}
			t.add(o.comment("ROLE "+rname, r.Comment))
			return nil
		})
	})
}

// membershipOptions are a membership's options as its GRANT restates them:
// all three from 16, where each is stored; ADMIN alone, when held, on 15.
func membershipOptions(m RoleMembership, v ServerVersion) MembershipOptions {
	if before(int(v), PostgreSQL16) {
		if m.Admin {
			return MembershipOptions{Admin: &m.Admin}
		}
		return MembershipOptions{}
	}
	return MembershipOptions{Admin: &m.Admin, Inherit: &m.Inherit, Set: &m.Set}
}

// ScriptTablespace scripts the tablespace name: CREATE TABLESPACE with its
// location and options, owner, comment and privileges. An in-place
// tablespace (allow_in_place_tablespaces) is written LOCATION ”. pg_default
// and pg_global, which initdb creates in the data directory, have no
// CREATE: ErrInvalidRequest.
func (sc *ServerScripter) ScriptTablespace(ctx context.Context, name string) (string, error) {
	s := sc.server
	op := "script tablespace " + pgsql.QuoteIdent(name)
	return scriptOf(ctx, sc.render, func(ctx context.Context) (*Tablespace, error) {
		return s.TablespaceByName(ctx, name)
	}, func(ts *Tablespace) (string, error) {
		o := sc.opts
		if ts.Location == "" {
			return "", invalidf("pgmo: %s: a tablespace in the data directory has no CREATE TABLESPACE", op)
		}
		tname := pgsql.QuoteIdent(ts.Name)
		return o.script(op, o.dropPlain("TABLESPACE", tname), func(t *scriptText) error {
			location := ts.Location
			if !strings.HasPrefix(location, "/") && !isWindowsAbsolute(location) {
				location = "" // in place: pg_tblspc/<oid>, relative to the data directory
			}
			stmt := "CREATE TABLESPACE " + tname + " LOCATION " + pgsql.QuoteLiteral(location)
			opts, err := storageParameters(ts.Options)
			if err != nil {
				return wrapScript(op, err)
			}
			if opts != "" {
				stmt += " WITH " + opts
			}
			t.notef("CREATE TABLESPACE cannot run inside a transaction block")
			t.add(stmt)
			t.add(o.owner("TABLESPACE", tname, ts.Owner))
			t.add(o.comment("TABLESPACE "+tname, ts.Comment))
			t.add(o.privileges("TABLESPACE "+tname, SecurableTablespace, ts.Owner, ts.Privileges, ts.PrivilegesDefault, s.version)...)
			return nil
		})
	})
}

// isWindowsAbsolute reports a Windows absolute path, C:\… or C:/….
func isWindowsAbsolute(p string) bool {
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') &&
		(p[0] >= 'A' && p[0] <= 'Z' || p[0] >= 'a' && p[0] <= 'z')
}
