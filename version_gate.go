package pgmo

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Version gating: naming a column, view or keyword the connected server does
// not have fails the whole statement, so a read that uses something added in
// a later release asks the server's version first.
//
// Every gate compares server_version_num (major*10000 + minor since
// PostgreSQL 10, e.g. 170002) through atLeast or before — never a bare
// comparison (gosmo's rule: its 2026-10-21 review found bare `major < 13`
// gates that blanked columns on servers that had them). A gate at or below
// MinimumServerVersion is never written: such a server is refused at connect,
// so the gate could only ever be true.
//
// The Server and Database forms (s.atLeast(v)) arrive with those types in P2;
// they read the version once at connect and delegate here.

// ServerVersion is a PostgreSQL server_version_num: major*10000 + minor.
// The named constants are each major's .0 release, which is what a feature
// gate compares against.
type ServerVersion int

const (
	PostgreSQL15 ServerVersion = 150000
	PostgreSQL16 ServerVersion = 160000
	PostgreSQL17 ServerVersion = 170000
	PostgreSQL18 ServerVersion = 180000
)

// MinimumServerVersion is the oldest server pgmo supports: PostgreSQL 15.
// An older server is refused at connect with ErrUnsupportedVersion.
const MinimumServerVersion = PostgreSQL15

// checkServerVersion refuses a server older than MinimumServerVersion. Its
// error carries no prefix: Connect and NewServer add theirs. It lives here,
// not in server.go, because it is the one comparison against the floor
// itself, which TestVersionGatesGoThroughAtLeast refuses everywhere else.
func checkServerVersion(v ServerVersion) error {
	if before(int(v), MinimumServerVersion) {
		return refusalf(ErrUnsupportedVersion, "server is %s; pgmo needs %s or later", v, MinimumServerVersion)
	}
	return nil
}

// Major is the major release number: 17 for 170002.
func (v ServerVersion) Major() int { return int(v) / 10000 }

// Minor is the minor release number: 2 for 170002.
func (v ServerVersion) Minor() int { return int(v) % 10000 }

// String renders v as a refusal reads it: "PostgreSQL 17" for a .0 version,
// "PostgreSQL 17.2" otherwise.
func (v ServerVersion) String() string {
	if v.Minor() == 0 {
		return fmt.Sprintf("PostgreSQL %d", v.Major())
	}
	return fmt.Sprintf("PostgreSQL %d.%d", v.Major(), v.Minor())
}

// atLeast is the one version comparison in pgmo: num is at least v, or 0 —
// the version was never read (a Server built without reading it) — which
// counts as newest, so an unread version degrades nothing; the query is then
// the authority.
func atLeast(num int, v ServerVersion) bool {
	return num == 0 || num >= int(v)
}

// before is !atLeast: num is known and older than v. It is the form a
// refusal ("needs PostgreSQL 17 or later") reads naturally in.
func before(num int, v ServerVersion) bool {
	return !atLeast(num, v)
}

// colSince names col when num is at least since, and otherwise substitutes
// zero. The scan destination list is unchanged either way, so a caller gets a
// zero value on an older server rather than an "undefined column" (42703)
// that kills the read — and a Scan arity error, the failure mode of omitting
// the column instead, cannot happen.
//
// zero must carry col's type: a cast (NULL::bigint, 0::bigint, 'none'::text) or
// a typed literal (false). An uncast literal or NULL resolves to text or
// unknown, and a destination expecting an int64 or a bool fails to scan it.
func colSince(num int, since ServerVersion, col, zero string) string {
	if atLeast(num, since) {
		return col
	}
	return zero
}

// hasColumnSince reports what colSince decides, for the rare gate that cannot
// be expressed as a substituted expression — a GROUP BY or ORDER BY term, a
// join to a view that does not exist yet — and has to be dropped or rewritten
// rather than replaced. It answers exactly as atLeast does; the separate name
// is what lets the gate inventory count such gates.
func hasColumnSince(num int, since ServerVersion) bool {
	return atLeast(num, since)
}

// colUntil is colSince's mirror for a column a later release removed: it
// names col while num is before until, and substitutes gone from until on —
// pg_stat_io.op_bytes (gone in 18) or the checkpoint counters pg_stat_bgwriter
// lost to pg_stat_checkpointer in 17. A rename is a colSince whose zero is the
// old name (colSince(num, PostgreSQL17, "d.datlocale", "d.daticulocale")), not
// a colUntil. gone must carry col's type, as colSince's zero does.
func colUntil(num int, until ServerVersion, col, gone string) string {
	if before(num, until) {
		return col
	}
	return gone
}

// extensionAtLeast is atLeast for an extension's own version, which is not the
// server's: an extension keeps the version it was created at across
// pg_upgrade until ALTER EXTENSION … UPDATE, so pg_stat_statements 1.10 on a
// PostgreSQL 17 server lacks the 1.11 columns that server ships. installed is
// pg_extension.extversion; since is the version that introduced the column
// ("1.11"). An unread installed version ("") counts as newest, as atLeast's 0
// does; so does one that is not dotted digits, which no in-core extension
// has ever used and which therefore says nothing about features.
func extensionAtLeast(installed, since string) bool {
	have, ok := extensionVersionParts(installed)
	if !ok {
		return true
	}
	want, _ := extensionVersionParts(since)
	return slices.Compare(have, want) >= 0
}

// extensionVersionParts splits "1.11" into [1 11]; ok is false for an empty
// or non-numeric version.
func extensionVersionParts(v string) (parts []int, ok bool) {
	if v == "" {
		return nil, false
	}
	for f := range strings.SplitSeq(v, ".") {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return nil, false
		}
		parts = append(parts, n)
	}
	return parts, true
}
