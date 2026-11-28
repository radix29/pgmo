package pgmo

import (
	"slices"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Scripting an ACL: GRANT and REVOKE from the kind's default
// ============================================================
//
// An object starts with its kind's hard-wired ACL (acldefault): the owner
// holds every privilege of the kind, and PUBLIC holds EXECUTE on a routine,
// USAGE on a type, a domain or a language, CONNECT and TEMPORARY on a
// database. A script restates an object's ACL as pg_dump does, as the
// statements turning that default into it: REVOKE ALL from a grantee whose
// default privileges are not all still held, then a GRANT for each grantee
// of what it holds beyond what it was left — WITH GRANT OPTION for the
// grantable ones. An ACL never set (PrivilegesDefault) is the default, and
// writes nothing.
//
// The grantor is not restated: a GRANT records the role running it (the
// owner, when a superuser runs the script), so a privilege granted on by a
// holder of the grant option comes back as granted by the owner
// (OPEN-THREADS.md § Scripter fidelity).

// aclDefault is a kind's hard-wired ACL: what the owner holds and what
// PUBLIC holds, before any GRANT or REVOKE.
type aclDefault struct {
	owner, public []Privilege
}

// aclDefaultOf is kind's default ACL on a server at v: every privilege of
// the kind the release has for the owner, and PUBLIC's.
func aclDefaultOf(kind SecurableKind, v ServerVersion) aclDefault {
	var owner []Privilege
	for _, p := range securableKinds[kind].privileges {
		if p == PrivilegeMaintain && before(int(v), PostgreSQL17) {
			continue
		}
		owner = append(owner, p)
	}
	var public []Privilege
	switch kind {
	case SecurableFunction, SecurableProcedure, SecurableRoutine:
		public = executePrivilege
	case SecurableType, SecurableDomain, SecurableLanguage:
		public = usagePrivilege
	case SecurableDatabase:
		public = []Privilege{PrivilegeConnect, PrivilegeTemporary}
	}
	return aclDefault{owner: owner, public: public}
}

// aclHeld is one grantee's privileges, each with its grant option.
type aclHeld struct {
	grantee string
	privs   []Privilege
	grant   map[Privilege]bool
}

func (h *aclHeld) holds(p Privilege) bool { return slices.Contains(h.privs, p) }

// aclByGrantee groups grants by grantee, in order of first appearance. A
// privilege granted by several grantors is held once, grantable if any
// grant of it is.
func aclByGrantee(grants []PrivilegeGrant) []*aclHeld {
	var out []*aclHeld
	for _, g := range grants {
		i := slices.IndexFunc(out, func(h *aclHeld) bool { return h.grantee == g.Grantee })
		if i < 0 {
			out = append(out, &aclHeld{grantee: g.Grantee, grant: map[Privilege]bool{}})
			i = len(out) - 1
		}
		h := out[i]
		if !h.holds(g.Privilege) {
			h.privs = append(h.privs, g.Privilege)
		}
		h.grant[g.Privilege] = h.grant[g.Privilege] || g.Grantable
	}
	return out
}

// aclStatements are the REVOKE and GRANT statements turning kind's
// default ACL, for owner, into grants, on the object on ("TABLE app.t",
// "FUNCTION app.f(integer)"). isDefault is an ACL never set: nothing.
func aclStatements(on string, kind SecurableKind, owner string, grants []PrivilegeGrant, isDefault bool, v ServerVersion) []string {
	if isDefault {
		return nil
	}
	def := aclDefaultOf(kind, v)
	held := aclByGrantee(grants)
	find := func(grantee string) *aclHeld {
		if i := slices.IndexFunc(held, func(h *aclHeld) bool { return h.grantee == grantee }); i >= 0 {
			return held[i]
		}
		return &aclHeld{grantee: grantee}
	}
	// left is what a grantee still holds of its default after the REVOKEs.
	left := map[string][]Privilege{}
	var stmts []string
	type defaultEntry struct {
		grantee string
		privs   []Privilege
	}
	defaults := []defaultEntry{{"", def.public}}
	if owner != "" { // unknown owner: no default of its to restate
		defaults = append(defaults, defaultEntry{owner, def.owner})
	}
	for _, d := range defaults {
		if len(d.privs) == 0 {
			continue
		}
		h := find(d.grantee)
		if slices.ContainsFunc(d.privs, func(p Privilege) bool { return !h.holds(p) }) {
			stmts = append(stmts, "REVOKE ALL ON "+on+" FROM "+roleSpec(d.grantee))
			continue
		}
		left[d.grantee] = d.privs
	}
	for _, h := range held {
		var plain, grantable []string
		for _, p := range h.privs {
			switch {
			case h.grant[p]:
				grantable = append(grantable, string(p))
			case !slices.Contains(left[h.grantee], p):
				plain = append(plain, string(p))
			}
		}
		if len(plain) > 0 {
			stmts = append(stmts, "GRANT "+strings.Join(plain, ", ")+" ON "+on+" TO "+roleSpec(h.grantee))
		}
		if len(grantable) > 0 {
			stmts = append(stmts, "GRANT "+strings.Join(grantable, ", ")+" ON "+on+" TO "+roleSpec(h.grantee)+" WITH GRANT OPTION")
		}
	}
	return stmts
}

// columnACLStatements are the GRANTs of columns' privileges on the table
// table ("app.t"): a column starts with none, so each is a GRANT.
func columnACLStatements(table string, columns []*Column) []string {
	var stmts []string
	for _, c := range columns {
		col := " (" + pgsql.QuoteIdent(c.Name) + ")"
		for _, h := range aclByGrantee(c.Privileges) {
			var plain, grantable []string
			for _, p := range h.privs {
				if h.grant[p] {
					grantable = append(grantable, string(p)+col)
				} else {
					plain = append(plain, string(p)+col)
				}
			}
			if len(plain) > 0 {
				stmts = append(stmts, "GRANT "+strings.Join(plain, ", ")+" ON TABLE "+table+" TO "+roleSpec(h.grantee))
			}
			if len(grantable) > 0 {
				stmts = append(stmts, "GRANT "+strings.Join(grantable, ", ")+" ON TABLE "+table+" TO "+roleSpec(h.grantee)+" WITH GRANT OPTION")
			}
		}
	}
	return stmts
}

// privileges is aclStatements when o includes privileges.
func (o ScriptOptions) privileges(on string, kind SecurableKind, owner string, grants []PrivilegeGrant, isDefault bool, v ServerVersion) []string {
	if !o.IncludePrivileges {
		return nil
	}
	return aclStatements(on, kind, owner, grants, isDefault, v)
}

// defaultPrivilegeStatements are a schema's default privileges as ALTER
// DEFAULT PRIVILEGES FOR ROLE … IN SCHEMA … GRANT: in a schema an entry
// only adds to the database-wide default, so each is a GRANT.
func defaultPrivilegeStatements(entries []DefaultPrivileges, v ServerVersion) ([]string, error) {
	var stmts []string
	for _, e := range entries {
		scope := DefaultPrivilegeScope{Roles: []string{e.Role}, ObjectType: e.ObjectType}
		if e.Schema != "" {
			scope.Schemas = []string{e.Schema}
		}
		for _, h := range aclByGrantee(e.Grants) {
			for _, grantable := range []bool{false, true} {
				var privs []Privilege
				for _, p := range h.privs {
					if h.grant[p] == grantable {
						privs = append(privs, p)
					}
				}
				if len(privs) == 0 {
					continue
				}
				stmt, err := defaultPrivilegeStatement(scope, PrivilegeChange{Action: ActionGrant, Privileges: privs,
					Grantees: []string{h.grantee}, GrantOption: grantable}, v)
				if err != nil {
					return nil, err
				}
				stmts = append(stmts, stmt)
			}
		}
	}
	return stmts, nil
}
