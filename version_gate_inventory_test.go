package pgmo

import (
	"fmt"
	"go/ast"
	"go/token"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/radix29/pgmo/pgsql"
)

// The version-gate inventory: every catalog column, catalog relation,
// statement form and extension column pgmo knows to differ across the
// supported releases (PLAN.md P1.4.2), written down before the reads that
// will use them.
//
// It is written by hand from the server's own catalog, never generated from
// call sites — a wrong `since` copied out of the code would be pinned by a
// test that agrees with it forever (gosmo's rule). Every entry was checked on
// the lab clusters on 2026-11-21 (pg15 150020, pg16 160016, pg17 170012, pg18
// 180007): the catalog entries by diffing pg_attribute across the four, the
// statement entries by running each probe, the pg_stat_statements entries
// against each server's installed extension and the extension's own update
// scripts. TestLiveVersionGatesMatchTheServer re-asks the connected server.
//
// The other half (P1.4.3): TestEveryVersionGateIsInTheInventory holds each
// gate call site to these tables, and version_gate_golden_test.go pins the
// text each gated read renders on every release.

// catalogGate is a pg_catalog relation or column whose presence depends on
// the server's release.
type catalogGate struct {
	relation string        // pg_catalog relation, unqualified
	column   string        // "" gates the relation itself
	since    ServerVersion // first release that has it; 0: at or below the floor, ungated
	until    ServerVersion // first release that no longer has it; 0: still present
	note     string        // what replaces it, or why it is listed at all
}

var catalogGates = []catalogGate{
	// pg_stat_io: the view is 16; 18 replaces op_bytes with per-operation
	// byte counts.
	{"pg_stat_io", "", PostgreSQL16, 0, ""},
	{"pg_stat_io", "op_bytes", PostgreSQL16, PostgreSQL18, "read_bytes/write_bytes/extend_bytes from 18"},
	{"pg_stat_io", "read_bytes", PostgreSQL18, 0, ""},
	{"pg_stat_io", "write_bytes", PostgreSQL18, 0, ""},
	{"pg_stat_io", "extend_bytes", PostgreSQL18, 0, ""},

	// pg_stat_checkpointer split from pg_stat_bgwriter in 17: the view
	// appears, the checkpoint counters leave pg_stat_bgwriter (renamed on the
	// way), and buffers_backend/_fsync go for good (pg_stat_io has them).
	{"pg_stat_checkpointer", "", PostgreSQL17, 0, ""},
	{"pg_stat_checkpointer", "num_done", PostgreSQL18, 0, ""},
	{"pg_stat_checkpointer", "slru_written", PostgreSQL18, 0, ""},
	{"pg_stat_bgwriter", "checkpoints_timed", 0, PostgreSQL17, "pg_stat_checkpointer.num_timed"},
	{"pg_stat_bgwriter", "checkpoints_req", 0, PostgreSQL17, "pg_stat_checkpointer.num_requested"},
	{"pg_stat_bgwriter", "checkpoint_write_time", 0, PostgreSQL17, "pg_stat_checkpointer.write_time"},
	{"pg_stat_bgwriter", "checkpoint_sync_time", 0, PostgreSQL17, "pg_stat_checkpointer.sync_time"},
	{"pg_stat_bgwriter", "buffers_checkpoint", 0, PostgreSQL17, "pg_stat_checkpointer.buffers_written"},
	{"pg_stat_bgwriter", "buffers_backend", 0, PostgreSQL17, "pg_stat_io"},
	{"pg_stat_bgwriter", "buffers_backend_fsync", 0, PostgreSQL17, "pg_stat_io"},

	// pg_stat_wal: 18 moves the write/sync counters to pg_stat_io.
	{"pg_stat_wal", "wal_write", 0, PostgreSQL18, "pg_stat_io (object 'wal')"},
	{"pg_stat_wal", "wal_sync", 0, PostgreSQL18, "pg_stat_io (object 'wal')"},
	{"pg_stat_wal", "wal_write_time", 0, PostgreSQL18, "pg_stat_io (object 'wal')"},
	{"pg_stat_wal", "wal_sync_time", 0, PostgreSQL18, "pg_stat_io (object 'wal')"},

	// pg_stat_progress_copy is 14, below the floor: listed so nobody gates it.
	{"pg_stat_progress_copy", "", 0, 0, "PostgreSQL 14: ungated"},
	{"pg_stat_progress_copy", "tuples_skipped", PostgreSQL17, 0, ""},

	// pg_database: datlocprovider is 15 (at the floor, ungated); 17 renames
	// daticulocale to datlocale, which also carries the builtin provider's
	// locale.
	{"pg_database", "datlocprovider", 0, 0, "PostgreSQL 15: ungated"},
	{"pg_database", "daticulocale", 0, PostgreSQL17, "renamed datlocale"},
	{"pg_database", "datlocale", PostgreSQL17, 0, ""},
	{"pg_database", "daticurules", PostgreSQL16, 0, ""},
	{"pg_database", "dathasloginevt", PostgreSQL17, 0, ""},

	// pg_collation: the same 17 rename as pg_database.
	{"pg_collation", "colliculocale", 0, PostgreSQL17, "renamed colllocale"},
	{"pg_collation", "colllocale", PostgreSQL17, 0, ""},
	{"pg_collation", "collicurules", PostgreSQL16, 0, ""},

	// pg_auth_members: 16 gives memberships an OID and the INHERIT/SET options.
	{"pg_auth_members", "oid", PostgreSQL16, 0, ""},
	{"pg_auth_members", "inherit_option", PostgreSQL16, 0, ""},
	{"pg_auth_members", "set_option", PostgreSQL16, 0, ""},

	// pg_attribute: attgenerated is ungated; its 'v' value (virtual) is 18 —
	// see the statement gate.
	{"pg_attribute", "attgenerated", 0, 0, "PostgreSQL 12: ungated; 'v' is 18"},

	// pg_class, pg_constraint.
	{"pg_class", "relallfrozen", PostgreSQL18, 0, ""},
	{"pg_constraint", "conenforced", PostgreSQL18, 0, ""},
	{"pg_constraint", "conperiod", PostgreSQL18, 0, ""},

	// pg_stat_all_tables (and the _user_/_sys_ views built on it).
	{"pg_stat_all_tables", "last_seq_scan", PostgreSQL16, 0, ""},
	{"pg_stat_all_tables", "last_idx_scan", PostgreSQL16, 0, ""},
	{"pg_stat_all_tables", "n_tup_newpage_upd", PostgreSQL16, 0, ""},
	{"pg_stat_all_tables", "total_vacuum_time", PostgreSQL18, 0, ""},
	{"pg_stat_all_tables", "total_autovacuum_time", PostgreSQL18, 0, ""},
	{"pg_stat_all_tables", "total_analyze_time", PostgreSQL18, 0, ""},
	{"pg_stat_all_tables", "total_autoanalyze_time", PostgreSQL18, 0, ""},

	// pg_stat_database.
	{"pg_stat_database", "parallel_workers_to_launch", PostgreSQL18, 0, ""},
	{"pg_stat_database", "parallel_workers_launched", PostgreSQL18, 0, ""},

	// Replication slots and logical replication.
	{"pg_replication_slots", "conflicting", PostgreSQL16, 0, ""},
	{"pg_replication_slots", "inactive_since", PostgreSQL17, 0, ""},
	{"pg_replication_slots", "invalidation_reason", PostgreSQL17, 0, ""},
	{"pg_replication_slots", "failover", PostgreSQL17, 0, ""},
	{"pg_replication_slots", "synced", PostgreSQL17, 0, ""},
	{"pg_replication_slots", "two_phase_at", PostgreSQL18, 0, ""},
	{"pg_subscription", "subpasswordrequired", PostgreSQL16, 0, ""},
	{"pg_subscription", "subrunasowner", PostgreSQL16, 0, ""},
	{"pg_subscription", "suborigin", PostgreSQL16, 0, ""},
	{"pg_subscription", "subfailover", PostgreSQL17, 0, ""},
	{"pg_stat_subscription", "leader_pid", PostgreSQL16, 0, ""},
	{"pg_stat_subscription", "worker_type", PostgreSQL17, 0, ""},
	{"pg_stat_subscription_stats", "confl_insert_exists", PostgreSQL18, 0, ""},
	{"pg_stat_subscription_stats", "confl_update_origin_differs", PostgreSQL18, 0, ""},
	{"pg_stat_subscription_stats", "confl_update_exists", PostgreSQL18, 0, ""},
	{"pg_stat_subscription_stats", "confl_update_missing", PostgreSQL18, 0, ""},
	{"pg_stat_subscription_stats", "confl_delete_origin_differs", PostgreSQL18, 0, ""},
	{"pg_stat_subscription_stats", "confl_delete_missing", PostgreSQL18, 0, ""},
	{"pg_stat_subscription_stats", "confl_multiple_unique_conflicts", PostgreSQL18, 0, ""},
}

// stableColumns are relations the plan expected to drift and that did not:
// pg_stat_activity and pg_stat_replication have the same columns, in the same
// order, on 15, 16, 17 and 18. They need no gate; the live test holds every
// supported server to the list, so a release that changes one turns up as a
// failure rather than as a read that dies on that release. (Their *values*
// do drift — 17 adds the 'slotsync worker' backend_type, 18 'io worker'; a
// read must not switch on an exhaustive list of them.)
var stableColumns = map[string][]string{
	"pg_stat_activity": {
		"datid", "datname", "pid", "leader_pid", "usesysid", "usename",
		"application_name", "client_addr", "client_hostname", "client_port",
		"backend_start", "xact_start", "query_start", "state_change",
		"wait_event_type", "wait_event", "state", "backend_xid", "backend_xmin",
		"query_id", "query", "backend_type",
	},
	"pg_stat_replication": {
		"pid", "usesysid", "usename", "application_name", "client_addr",
		"client_hostname", "client_port", "backend_start", "backend_xmin",
		"state", "sent_lsn", "write_lsn", "flush_lsn", "replay_lsn",
		"write_lag", "flush_lag", "replay_lag", "sync_priority", "sync_state",
		"reply_time",
	},
}

// statementGate is syntax, an option or a predefined object a release added:
// probe (after setup, in one transaction that is always rolled back) runs on
// since and later and fails with sqlstate before it.
type statementGate struct {
	name     string
	since    ServerVersion
	setup    []string
	probe    string
	sqlstate string // what a server older than since raises
}

var statementGates = []statementGate{
	{"MERGE … RETURNING", PostgreSQL17,
		[]string{`CREATE TEMP TABLE pgmo_gate_merge (a int)`},
		`MERGE INTO pgmo_gate_merge t USING (VALUES (1)) v (x) ON t.a = v.x
		 WHEN NOT MATCHED THEN INSERT VALUES (v.x) RETURNING merge_action(), t.a`,
		"42601"},
	{"virtual generated column", PostgreSQL18, nil,
		`CREATE TEMP TABLE pgmo_gate_virtual (a int, b int GENERATED ALWAYS AS (a * 2) VIRTUAL)`,
		"42601"},
	// ConstraintSpec.NotEnforced: a check or foreign key declared and never
	// checked (CreateTable, AddConstraint).
	// CreateEventTrigger's EventTriggerLogin.
	{"login event trigger", PostgreSQL17,
		[]string{`CREATE FUNCTION pg_temp.pgmo_gate_evt() RETURNS event_trigger LANGUAGE plpgsql AS $$BEGIN END$$`},
		`CREATE EVENT TRIGGER pgmo_gate_login ON login EXECUTE FUNCTION pg_temp.pgmo_gate_evt()`,
		"42601"},
	{"NOT ENFORCED constraint", PostgreSQL18, nil,
		`CREATE TEMP TABLE pgmo_gate_not_enforced (a int, CONSTRAINT c CHECK (a > 0) NOT ENFORCED)`,
		"42601"},
	// PrivilegeMaintain in ApplyPrivileges and AlterDefaultPrivileges.
	{"MAINTAIN privilege", PostgreSQL17,
		[]string{`CREATE TEMP TABLE pgmo_gate_maintain (a int)`},
		`GRANT MAINTAIN ON pg_temp.pgmo_gate_maintain TO PUBLIC`,
		"42601"},
	// AlterDefaultPrivileges' DefaultPrivilegeLargeObjects.
	{"default privileges on large objects", PostgreSQL18, nil,
		`ALTER DEFAULT PRIVILEGES FOR ROLE CURRENT_USER GRANT SELECT ON LARGE OBJECTS TO PUBLIC`,
		"42601"},
	// ICU tailoring rules came to collations and databases together;
	// CREATE DATABASE … ICU_RULES cannot be probed in a transaction, so the
	// collation's form stands for both (CreateDatabase's gate).
	{"ICU tailoring rules", PostgreSQL16, nil,
		`CREATE COLLATION pg_temp.pgmo_gate_rules (provider = icu, locale = 'und', rules = '&a < b')`,
		"42601"},
	// The builtin provider came to collations and databases together
	// (CreateDatabase's LOCALE_PROVIDER builtin and BUILTIN_LOCALE gate).
	{"builtin collation provider", PostgreSQL17, nil,
		`CREATE COLLATION pg_temp.pgmo_gate_builtin (provider = builtin, locale = 'C')`,
		"42P17"},
	{"EXPLAIN (GENERIC_PLAN)", PostgreSQL16, nil,
		`EXPLAIN (GENERIC_PLAN) SELECT $1::int`,
		"42P02"},
	{"EXPLAIN (SERIALIZE, MEMORY)", PostgreSQL17, nil,
		`EXPLAIN (ANALYZE, SERIALIZE, MEMORY) SELECT 1`,
		"42601"},
	{"GRANT role WITH INHERIT/SET", PostgreSQL16,
		[]string{`CREATE ROLE pgmo_gate_member`},
		`GRANT pg_monitor TO pgmo_gate_member WITH INHERIT FALSE, SET FALSE`,
		"42601"},
	{"role pg_create_subscription", PostgreSQL16, nil, `SELECT 'pg_create_subscription'::regrole`, "42704"},
	{"role pg_use_reserved_connections", PostgreSQL16, nil, `SELECT 'pg_use_reserved_connections'::regrole`, "42704"},
	{"role pg_maintain", PostgreSQL17, nil, `SELECT 'pg_maintain'::regrole`, "42704"},
	{"role pg_signal_autovacuum_worker", PostgreSQL18, nil, `SELECT 'pg_signal_autovacuum_worker'::regrole`, "42704"},
}

// extensionGate is a column of an extension's view whose presence follows
// the extension's installed version (pg_extension.extversion), not the
// server's — see extensionAtLeast. Versions are the extension's own.
type extensionGate struct {
	extension string
	relation  string // in the extension's schema, which is wherever it was created
	column    string
	since     string // "": present in every version a supported server ships
	until     string // "": still present
	note      string
}

// pg_stat_statements ships 1.4 plus update scripts to 1.10 with 15 and 16,
// 1.11 with 17, 1.12 with 18 — and an installation keeps whatever version it
// was created at, so a PostgreSQL 15 server may hold anything from 1.4 up.
// The versions below come from the extension's update scripts.
var extensionGates = []extensionGate{
	{"pg_stat_statements", "pg_stat_statements", "total_exec_time", "1.8", "", "was total_time"},
	{"pg_stat_statements", "pg_stat_statements", "mean_exec_time", "1.8", "", "was mean_time"},
	{"pg_stat_statements", "pg_stat_statements", "plans", "1.8", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "total_plan_time", "1.8", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "wal_bytes", "1.8", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "toplevel", "1.9", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "temp_blk_read_time", "1.10", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "jit_functions", "1.10", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "jit_generation_time", "1.10", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "jit_emission_time", "1.10", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "blk_read_time", "", "1.11", "renamed shared_blk_read_time"},
	{"pg_stat_statements", "pg_stat_statements", "blk_write_time", "", "1.11", "renamed shared_blk_write_time"},
	{"pg_stat_statements", "pg_stat_statements", "shared_blk_read_time", "1.11", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "shared_blk_write_time", "1.11", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "local_blk_read_time", "1.11", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "local_blk_write_time", "1.11", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "jit_deform_count", "1.11", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "jit_deform_time", "1.11", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "stats_since", "1.11", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "minmax_stats_since", "1.11", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "wal_buffers_full", "1.12", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "parallel_workers_to_launch", "1.12", "", ""},
	{"pg_stat_statements", "pg_stat_statements", "parallel_workers_launched", "1.12", "", ""},
}

// supportedVersions are the releases a gate may name: above the floor (a gate
// at or below it could only ever be true) and no newer than the newest
// release pgmo knows.
var supportedVersions = []ServerVersion{PostgreSQL16, PostgreSQL17, PostgreSQL18}

// catalogGateExpected is what the inventory says about g on num: whether a
// read may name it there.
func catalogGateExpected(g catalogGate, num int) bool {
	return (g.since == 0 || atLeast(num, g.since)) && (g.until == 0 || before(num, g.until))
}

// extensionGateExpected is catalogGateExpected for an extension column.
func extensionGateExpected(g extensionGate, installed string) bool {
	return (g.since == "" || extensionAtLeast(installed, g.since)) &&
		(g.until == "" || !extensionAtLeast(installed, g.until))
}

func TestCatalogGatesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, g := range catalogGates {
		key := g.relation + "." + g.column
		if seen[key] {
			t.Errorf("%s: listed twice", key)
		}
		seen[key] = true
		for _, v := range []ServerVersion{g.since, g.until} {
			if v != 0 && !slices.Contains(supportedVersions, v) {
				t.Errorf("%s: gate on %s — gates are .0 releases above the floor (%v)", key, v, supportedVersions)
			}
		}
		if g.since != 0 && g.until != 0 && g.until <= g.since {
			t.Errorf("%s: until %s is not after since %s", key, g.until, g.since)
		}
		if g.since == 0 && g.until == 0 && g.note == "" {
			t.Errorf("%s: neither gated nor removed — say why it is listed", key)
		}
		if g.column != "" && g.since != 0 {
			// A column newer than its relation is fine; one older than it is
			// a typo in one of the two.
			for _, r := range catalogGates {
				if r.relation == g.relation && r.column == "" && r.since > g.since {
					t.Errorf("%s: since %s, before its relation (%s)", key, g.since, r.since)
				}
			}
		}
	}
	for rel := range stableColumns {
		for _, g := range catalogGates {
			if g.relation == rel {
				t.Errorf("%s is listed as stable and gated (%s)", rel, g.column)
			}
		}
	}
}

func TestStatementGatesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, g := range statementGates {
		if seen[g.name] {
			t.Errorf("%s: listed twice", g.name)
		}
		seen[g.name] = true
		if !slices.Contains(supportedVersions, g.since) {
			t.Errorf("%s: gate on %s — gates are .0 releases above the floor", g.name, g.since)
		}
		if len(g.sqlstate) != 5 {
			t.Errorf("%s: sqlstate %q is not a SQLSTATE", g.name, g.sqlstate)
		}
	}
}

func TestExtensionGatesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, g := range extensionGates {
		key := fmt.Sprintf("%s:%s.%s", g.extension, g.relation, g.column)
		if seen[key] {
			t.Errorf("%s: listed twice", key)
		}
		seen[key] = true
		for _, v := range []string{g.since, g.until} {
			if _, ok := extensionVersionParts(v); v != "" && !ok {
				t.Errorf("%s: %q is not a dotted numeric version", key, v)
			}
		}
		if g.since == "" && g.until == "" {
			t.Errorf("%s: neither gated nor removed", key)
		}
		if g.since != "" && g.until != "" && extensionAtLeast(g.since, g.until) {
			t.Errorf("%s: until %s is not after since %s", key, g.until, g.since)
		}
	}
}

// TestRenamesAreContiguous checks every rename the inventory records: the old
// name's until is the new name's since, so exactly one of the two exists on
// any release — which is what colSince(num, v, new, old) assumes.
func TestRenamesAreContiguous(t *testing.T) {
	renames := []struct{ relation, old, new string }{
		{"pg_database", "daticulocale", "datlocale"},
		{"pg_collation", "colliculocale", "colllocale"},
	}
	find := func(rel, col string) catalogGate {
		for _, g := range catalogGates {
			if g.relation == rel && g.column == col {
				return g
			}
		}
		t.Fatalf("%s.%s is not in the inventory", rel, col)
		return catalogGate{}
	}
	for _, r := range renames {
		o, n := find(r.relation, r.old), find(r.relation, r.new)
		if o.until == 0 || o.until != n.since {
			t.Errorf("%s: %s until %s, %s since %s — a release would have both or neither",
				r.relation, r.old, o.until, r.new, n.since)
		}
		for _, num := range []int{150020, 160016, 170012, 180007, 0} {
			if catalogGateExpected(o, num) == catalogGateExpected(n, num) {
				t.Errorf("%s on %d: %s and %s are both %v", r.relation, num, r.old, r.new, catalogGateExpected(o, num))
			}
		}
	}
	for _, r := range []struct{ old, new string }{{"blk_read_time", "shared_blk_read_time"}, {"blk_write_time", "shared_blk_write_time"}} {
		var o, n extensionGate
		for _, g := range extensionGates {
			switch g.column {
			case r.old:
				o = g
			case r.new:
				n = g
			}
		}
		if o.until == "" || o.until != n.since {
			t.Errorf("pg_stat_statements: %s until %q, %s since %q", r.old, o.until, r.new, n.since)
		}
	}
}

// TestExtensionGatesOnTheLabVersions pins what the inventory decides for the
// three pg_stat_statements versions the lab runs, as observed there on
// 2026-11-21: 1.10 (pg15, pg16), 1.11 (pg17), 1.12 (pg18).
func TestExtensionGatesOnTheLabVersions(t *testing.T) {
	cases := []struct {
		version string
		column  string
		want    bool
	}{
		{"1.10", "jit_functions", true},
		{"1.10", "blk_read_time", true},
		{"1.10", "shared_blk_read_time", false},
		{"1.10", "stats_since", false},
		{"1.11", "blk_read_time", false},
		{"1.11", "shared_blk_read_time", true},
		{"1.11", "jit_deform_count", true},
		{"1.11", "wal_buffers_full", false},
		{"1.12", "wal_buffers_full", true},
		{"1.12", "parallel_workers_launched", true},
		{"1.7", "total_exec_time", false},
	}
	for _, c := range cases {
		var g *extensionGate
		for i := range extensionGates {
			if extensionGates[i].column == c.column {
				g = &extensionGates[i]
			}
		}
		if g == nil {
			t.Fatalf("%s is not in the inventory", c.column)
		}
		if got := extensionGateExpected(*g, c.version); got != c.want {
			t.Errorf("pg_stat_statements %s, %s: %v, want %v", c.version, c.column, got, c.want)
		}
	}
}

// ============================================================
// Every call site is in the inventory
// ============================================================

// TestEveryVersionGateIsInTheInventory holds each gate call site to the
// inventory above, so a `since` typed at the call site is checked against
// the catalog the inventory was read from, not against itself (gosmo's rule).
// pgmo has no gated reads yet; TestGateCallSiteScan shows what it finds.
func TestEveryVersionGateIsInTheInventory(t *testing.T) {
	fset, files := parsePackage(t)
	for name, f := range files {
		if name == "version_gate.go" {
			continue // the declarations, not call sites
		}
		for _, s := range gateCallSites(f) {
			if msg := s.inventoryProblem(); msg != "" {
				t.Errorf("%s: %s", fset.Position(s.pos), msg)
			}
		}
	}
}

// The scan and the check, on a source holding each shape.
func TestGateCallSiteScan(t *testing.T) {
	const src = `package p

func f(n int, ext string) {
	_ = colSince(n, PostgreSQL17, "d.datlocale", "d.daticulocale")         // a rename: fine
	_ = colSince(n, PostgreSQL16, "s.last_seq_scan", "NULL::timestamptz")  // fine
	_ = colSince(n, PostgreSQL16, "m.set_option", "true")                  // fine: a boolean literal
	_ = colUntil(n, PostgreSQL18, "op_bytes", "NULL::bigint")              // fine
	_ = hasColumnSince(n, PostgreSQL17)                                    // fine
	_ = atLeast(n, PostgreSQL16)                                           // fine: pg_stat_io
	_ = extensionAtLeast(ext, "1.11")                                      // fine

	_ = colSince(n, PostgreSQL17, "s.last_seq_scan", "NULL::timestamptz")  // wrong release
	_ = colSince(n, PostgreSQL18, "x.no_such_column", "0::int")            // not in the inventory
	_ = colUntil(n, PostgreSQL17, "op_bytes", "NULL::bigint")              // wrong release
	_ = colSince(n, PostgreSQL16, "d.daticurules", "d.datcollate")         // zero is no removed column
	_ = colSince(n, PostgreSQL16, col, "NULL::text")                       // column not a literal
	_ = extensionAtLeast(ext, "1.13")                                      // no such extension gate
	_ = extensionAtLeast(ext, v)                                           // not a literal
}
`
	var got []string
	for _, s := range gateCallSites(parseSource(t, src)) {
		if msg := s.inventoryProblem(); msg != "" {
			got = append(got, msg)
		}
	}
	want := []string{
		`colSince(PostgreSQL17, "s.last_seq_scan"): the inventory has no column last_seq_scan introduced in PostgreSQL 17`,
		`colSince(PostgreSQL18, "x.no_such_column"): the inventory has no column no_such_column introduced in PostgreSQL 18`,
		`colUntil(PostgreSQL17, "op_bytes"): the inventory has no column op_bytes removed in PostgreSQL 17`,
		`colSince(PostgreSQL16, "d.daticurules"): its substitute d.datcollate reads as a column, but the inventory has no column datcollate removed in PostgreSQL 16 — cast a literal, or record the rename`,
		`colSince: the column is not a string literal, so the inventory cannot be checked against it`,
		`extensionAtLeast("1.13"): no extension gate starts or ends at 1.13`,
		`extensionAtLeast: the version is not a string literal, so the inventory cannot be checked against it`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("gate call-site problems\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// gateCallSite is one call to a gate function, as the source spells it.
type gateCallSite struct {
	pos     token.Pos
	fn      string
	version ServerVersion // the release constant named; 0 for extensionAtLeast, or none
	col     string        // colSince/colUntil: the column, unquoted; extensionAtLeast: the version
	zero    string        // colSince/colUntil: the substitute's source text
	literal bool          // col was a string literal
}

// gateCallSites lists f's calls to the gate functions, in source order. A
// call whose version is not a supported constant is left to
// TestVersionGatesGoThroughAtLeast.
func gateCallSites(f *ast.File) []gateCallSite {
	byName := map[string]ServerVersion{}
	for _, v := range supportedVersions {
		byName[versionConst(v)] = v
	}
	var out []gateCallSite
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := calleeName(c)
		s := gateCallSite{pos: c.Pos(), fn: fn}
		switch {
		case fn == "extensionAtLeast" && len(c.Args) == 2:
			s.col, s.literal = stringLit(c.Args[1])
		case slices.Contains(serverGateFuncs, fn):
			i := versionConstArg(c)
			if i < 0 || byName[constName(c.Args[i])] == 0 {
				return true
			}
			s.version = byName[constName(c.Args[i])]
			if (fn == "colSince" || fn == "colUntil") && len(c.Args) >= i+3 {
				s.col, s.literal = stringLit(c.Args[i+1])
				s.zero, _ = stringLit(c.Args[i+2])
			}
		default:
			return true
		}
		out = append(out, s)
		return true
	})
	return out
}

// stringLit is e's value when it is a string literal.
func stringLit(e ast.Expr) (string, bool) {
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		v, err := strconv.Unquote(lit.Value)
		return v, err == nil
	}
	return exprText(e), false
}

// boolLiteral are the keywords columnRef would read as a column: true and
// false are typed literals (boolean), as colSince's doc offers them.
var boolLiteral = map[string]bool{"true": true, "false": true}

// columnRef matches a substitute that is a column reference rather than a
// typed literal: col, alias.col, or either quoted.
var columnRef = regexp.MustCompile(`^(?:(?:[a-z_][a-z0-9_]*|"[^"]+")\.)?(?:[a-z_][a-z0-9_]*|"[^"]+")$`)

// bareColumn is a column reference's column, unqualified and unquoted:
// datlocale for d.datlocale, Mixed for d."Mixed".
func bareColumn(ref string) string {
	if strings.HasSuffix(ref, `"`) {
		if i := strings.LastIndex(ref[:len(ref)-1], `"`); i >= 0 {
			return pgsql.UnquoteIdent(ref[i:])
		}
	}
	if i := strings.LastIndex(ref, "."); i >= 0 {
		ref = ref[i+1:]
	}
	return pgsql.UnquoteIdent(ref)
}

// inventoryProblem is what the inventory says is wrong with s; "" when
// nothing is.
func (s gateCallSite) inventoryProblem() string {
	if (s.fn == "colSince" || s.fn == "colUntil" || s.fn == "extensionAtLeast") && !s.literal && s.col != "" {
		what := "column"
		if s.fn == "extensionAtLeast" {
			what = "version"
		}
		return fmt.Sprintf("%s: the %s is not a string literal, so the inventory cannot be checked against it", s.fn, what)
	}
	hasCatalog := func(column string, since, until ServerVersion) bool {
		for _, g := range catalogGates {
			if g.column == column && (since == 0 || g.since == since) && (until == 0 || g.until == until) {
				return true
			}
		}
		return false
	}
	switch s.fn {
	case "extensionAtLeast":
		for _, g := range extensionGates {
			if g.since == s.col || g.until == s.col {
				return ""
			}
		}
		return fmt.Sprintf("extensionAtLeast(%q): no extension gate starts or ends at %s", s.col, s.col)
	case "colSince":
		col := bareColumn(s.col)
		if !hasCatalog(col, s.version, 0) {
			return fmt.Sprintf("colSince(%s, %q): the inventory has no column %s introduced in %s", versionConst(s.version), s.col, col, s.version)
		}
		if columnRef.MatchString(s.zero) && !boolLiteral[s.zero] && !hasCatalog(bareColumn(s.zero), 0, s.version) {
			return fmt.Sprintf("colSince(%s, %q): its substitute %s reads as a column, but the inventory has no column %s removed in %s — cast a literal, or record the rename",
				versionConst(s.version), s.col, s.zero, bareColumn(s.zero), s.version)
		}
	case "colUntil":
		col := bareColumn(s.col)
		if !hasCatalog(col, 0, s.version) {
			return fmt.Sprintf("colUntil(%s, %q): the inventory has no column %s removed in %s", versionConst(s.version), s.col, col, s.version)
		}
	default: // atLeast, before, hasColumnSince: the release must be one something changed in
		for _, g := range catalogGates {
			if g.since == s.version || g.until == s.version {
				return ""
			}
		}
		for _, g := range statementGates {
			if g.since == s.version {
				return ""
			}
		}
		return fmt.Sprintf("%s(%s): nothing in the inventory changes in %s", s.fn, versionConst(s.version), s.version)
	}
	return ""
}
