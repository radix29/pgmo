package pgmo_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// The column types of the sequence read and of the last-value read, in
// SELECT order, as the server sends them.
var (
	sequenceTypes = append([]uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
		pgtype.TextOID, pgtype.TextOID, pgtype.Int8OID, pgtype.Int8OID, pgtype.Int8OID, pgtype.Int8OID, pgtype.Int8OID, pgtype.BoolOID,
		pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.BoolOID},
		append(aclTypes, pgtype.TextOID)...)
	lastValueTypes = []uint32{pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.Int8OID}
)

// A sequence with every column set to something its neighbours are not.
var rowSequence = []any{uint32(41001), "app", "ids", "own", true, "citext",
	"u", "smallint", int64(-1), int64(-2), int64(-300), int64(-1), int64(5), true,
	"app", "orders", "id", true, false,
	[]string{"B b"}, []string{"own"}, []string{"USAGE"}, []bool{true},
	"the ids"}

// sequenceMatch and lastValueMatch tell the two reads apart in a fake.
const (
	sequenceMatch  = "WHERE c.relkind = 'S'"
	lastValueMatch = "pg_catalog.pg_sequence_last_value(r.oid)"
)

// loadedSequence reads rowSequence by name from database appdb of a fresh
// fake, which answers the last-value read with r when it is not nil.
func loadedSequence(t *testing.T, r pgmotest.Responder) (*pgmotest.Fake, *pgmotest.Fake, *pgmo.Sequence) {
	t.Helper()
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(sequenceMatch, rowsOf(sequenceTypes, rowSequence))
	if r != nil {
		other.On(lastValueMatch, r)
	}
	q, err := f.Server().DatabaseRef("appdb").SequenceByName(t.Context(), "app", "ids")
	if err != nil {
		t.Fatal(err)
	}
	return f, other, q
}

// Every field is filled from its own column, the names are bound as $1 and
// $2, and the read goes to the sequence's own database.
func TestSequenceByNameFillsEveryField(t *testing.T) {
	f, other, q := loadedSequence(t, nil)
	if q.Database() == nil || q.Database().Name != "appdb" {
		t.Errorf("Database() = %v", q.Database())
	}
	want := pgmo.Sequence{
		Origin: pgmo.Origin{IsSystem: true, Extension: "citext"}, OID: 41001, Schema: "app", Name: "ids", Owner: "own",
		Persistence: pgmo.PersistenceUnlogged, DataType: "smallint", Start: -1, Increment: -2, MinValue: -300, MaxValue: -1,
		Cache: 5, Cycle: true, OwnedBy: pgmo.ObjectName{Schema: "app", Name: "orders"}, OwnedByColumn: "id", Identity: true,
		Privileges: []pgmo.PrivilegeGrant{{Grantee: "B b", Grantor: "own", Privilege: pgmo.PrivilegeUsage, Grantable: true}},
		Comment:    "the ids",
	}
	got := *q
	got.SetDatabase(nil)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SequenceByName =\n%+v\nwant\n%+v", got, want)
	}
	if n := reflect.TypeFor[pgmo.Sequence]().NumField(); n != 20 {
		t.Errorf("Sequence has %d fields; this test checks 20 — add the new one", n)
	}
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the sequence read went to appdb", n)
	}
	last := lastQuery(other)
	if last.Arg(0) != "app" || last.Arg(1) != "ids" || strings.Contains(last.Text, "'ids'") {
		t.Errorf("names not bound as $1, $2: args %v, text\n%s", last.Args, last.Text)
	}
	for _, w := range []string{sequenceMatch,
		"JOIN pg_catalog.pg_sequence s ON s.seqrelid = c.oid",
		"c.relpersistence::pg_catalog.text,\n\tpg_catalog.format_type(s.seqtypid, NULL),\n\ts.seqstart, s.seqincrement, s.seqmin, s.seqmax, s.seqcache, s.seqcycle,",
		"COALESCE(otn.nspname::pg_catalog.text, ''), COALESCE(ot.relname::pg_catalog.text, ''),\n\tCOALESCE(oa.attname::pg_catalog.text, ''), COALESCE(od.deptype = 'i', false),",
		"LEFT JOIN pg_catalog.pg_depend od ON od.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND od.objid = c.oid\n\tAND od.objsubid = 0 AND od.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass AND od.deptype IN ('a', 'i')",
		"LEFT JOIN pg_catalog.pg_class ot ON ot.oid = od.refobjid",
		"LEFT JOIN pg_catalog.pg_namespace otn ON otn.oid = ot.relnamespace",
		"LEFT JOIN pg_catalog.pg_attribute oa ON oa.attrelid = od.refobjid AND oa.attnum = od.refobjsubid",
		"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = c.oid\n\tAND ds.classoid = 'pg_catalog.pg_class'::pg_catalog.regclass AND ds.objsubid = 0",
		"COALESCE(c.relacl, pg_catalog.acldefault('s', c.relowner))", "c.relacl IS NULL",
		"LEFT JOIN pg_catalog.pg_extension ext ON ext.oid = d.refobjid",
		"AND n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text",
	} {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
	if q.QualifiedName() != "app.ids" || !q.IsOwned() {
		t.Errorf("QualifiedName %s, IsOwned %v", q.QualifiedName(), q.IsOwned())
	}
	if (&pgmo.Sequence{}).IsOwned() {
		t.Error("a sequence no column owns reads as owned")
	}
}

// The listing pushes its filter into the query and orders by schema then
// name; empty arrays and a NULL ACL read as none.
func TestSequencesFiltered(t *testing.T) {
	yes := true
	f := pgmotest.New(t)
	f.On(sequenceMatch, rowsOf(sequenceTypes, []any{uint32(41002), "app", "Ids", "own", false, "",
		"p", "bigint", int64(1), int64(1), int64(1), int64(9223372036854775807), int64(1), false,
		"", "", "", false, true, nil, nil, nil, nil, ""}))
	d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	qs, err := d.Sequences(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "IDS"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &yes,
		ExtensionMember: &yes,
	})
	if err != nil || len(qs) != 1 || qs[0].Privileges != nil || !qs[0].PrivilegesDefault || qs[0].IsOwned() ||
		qs[0].OwnedBy != (pgmo.ObjectName{}) || qs[0].Persistence != pgmo.PersistencePermanent || qs[0].MaxValue != 1<<63-1 {
		t.Fatalf("Sequences = %+v, %v", qs, err)
	}
	last := lastQuery(f)
	tail := sequenceMatch + " AND pg_catalog.strpos(c.relname, $1) > 0 AND n.nspname = $2::pg_catalog.text" +
		" AND (pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema') AND EXISTS ("
	if !strings.Contains(last.Text, tail) || !strings.HasSuffix(last.Text, "\nORDER BY n.nspname, c.relname") ||
		len(last.Args) != 2 || last.Arg(0) != "ids" || last.Arg(1) != "app" {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}
	n := len(f.Queries())
	if _, err := d.Sequences(t.Context(), pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}); !errors.Is(err, pgmo.ErrInvalidRequest) {
		t.Errorf("unknown operator: err = %v, want ErrInvalidRequest", err)
	}
	if len(f.Queries()) != n {
		t.Errorf("a refused filter was sent")
	}
}

// No schema is refused before anything is sent; no row is ErrNotFound; a
// server error and a malformed ACL are wrapped once, naming the read.
func TestSequenceByNameErrors(t *testing.T) {
	f := pgmotest.New(t)
	f.On(sequenceMatch, rowsOf(sequenceTypes))
	d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	n := len(f.Queries())
	if _, err := d.SequenceByName(t.Context(), "", "s"); !errors.Is(err, pgmo.ErrSchemaRequired) ||
		!strings.HasPrefix(err.Error(), `pgmo: read sequence "s"`) {
		t.Errorf("no schema: err = %v, want ErrSchemaRequired", err)
	}
	if len(f.Queries()) != n {
		t.Errorf("a schema-less read was sent")
	}
	_, err := d.SequenceByName(t.Context(), "app", "gone")
	if !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), `sequence app.gone not found in database "`+pgmotest.DefaultDatabase+`"`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}

	f = pgmotest.New(t)
	f.On(sequenceMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	_, err = f.Server().DatabaseRef(pgmotest.DefaultDatabase).SequenceByName(t.Context(), "app", "s")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read sequence app.s of database ") ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}

	// The last ACL array, before the comment, cut short.
	row := append([]any(nil), rowSequence...)
	row[len(row)-2] = row[len(row)-2].([]bool)[:0]
	f = pgmotest.New(t)
	f.On(sequenceMatch, rowsOf(sequenceTypes, row))
	_, err = f.Server().DatabaseRef(pgmotest.DefaultDatabase).Sequences(t.Context(), pgmo.ObjectFilter{})
	if err == nil || !strings.HasPrefix(err.Error(), "pgmo: list sequences of database ") || !strings.Contains(err.Error(), "different lengths") {
		t.Errorf("ACL short: err = %v, want the mismatch wrapped naming the read", err)
	}
}

// A Ref reads nothing, holds the names alone, and LastValue refuses it
// before sending anything.
func TestSequenceRef(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef("appdb")
	n := len(f.Queries())
	q := d.SequenceRef("app", "ids")
	got := *q
	got.SetDatabase(nil)
	if q.Database() != d || !reflect.DeepEqual(got, pgmo.Sequence{Schema: "app", Name: "ids"}) {
		t.Errorf("SequenceRef = %+v", q)
	}
	if _, _, err := q.LastValue(t.Context()); !errors.Is(err, pgmo.ErrHandleNotLoaded) || !strings.Contains(err.Error(), "app.ids") ||
		!strings.HasPrefix(err.Error(), "pgmo: read last value of sequence app.ids: ") {
		t.Errorf("LastValue on a Ref: err = %v, want ErrHandleNotLoaded naming app.ids", err)
	}
	if len(f.Queries()) != n || f.Database("appdb").Opens() != 0 {
		t.Errorf("a Ref read sent a statement")
	}
}

// LastValue goes to the sequence's database keyed by its OID as $1, reads
// the value only past every guard, and answers each case of its row.
func TestSequenceLastValue(t *testing.T) {
	for _, c := range []struct {
		name      string
		row       []any // nil: no row
		value     int64
		called    bool
		wantErrIs []error
		wantText  string
	}{
		{"called", []any{true, false, false, int64(-9)}, -9, true, nil, ""},
		{"never called", []any{true, false, false, nil}, 0, false, nil, ""},
		{"no privilege", []any{false, false, false, nil}, 0, false, []error{pgmo.ErrDefinitionHidden, pgmo.ErrUnsupported},
			"neither SELECT nor USAGE"},
		{"another session's temporary", []any{true, true, false, nil}, 0, false, []error{pgmo.ErrUnsupported},
			"another session's temporary sequence"},
		{"unlogged on a standby", []any{true, false, true, nil}, 0, false, []error{pgmo.ErrUnsupported},
			"unlogged sequence holds no value on a standby"},
		{"dropped", nil, 0, false, []error{pgmo.ErrNotFound}, `sequence app.ids not found in database "appdb"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := rowsOf(lastValueTypes)
			if c.row != nil {
				r = rowsOf(lastValueTypes, c.row)
			}
			f, other, q := loadedSequence(t, r)
			v, called, err := q.LastValue(t.Context())
			if v != c.value || called != c.called {
				t.Errorf("LastValue = %d, %v; want %d, %v", v, called, c.value, c.called)
			}
			for _, is := range c.wantErrIs {
				if !errors.Is(err, is) {
					t.Errorf("err = %v, want %v", err, is)
				}
			}
			if c.wantErrIs == nil && err != nil {
				t.Errorf("err = %v", err)
			}
			if c.wantText != "" && (err == nil || !strings.Contains(err.Error(), c.wantText) || strings.Count(err.Error(), "pgmo: ") != 1) {
				t.Errorf("err = %v, want %q once prefixed", err, c.wantText)
			}
			if n := len(f.Queries()); n != 1 {
				t.Errorf("%d statements on the Server's own database, want 1", n)
			}
			last := lastQuery(other)
			if len(last.Args) != 1 || last.Arg(0) != pgmo.OID(41001) && last.Arg(0) != uint32(41001) {
				t.Errorf("not keyed by the OID as $1: args %v (%T)", last.Args, last.Arg(0))
			}
			for _, w := range []string{
				"CASE WHEN r.readable AND NOT r.other_temp AND NOT r.unlogged_standby\n\t\tTHEN pg_catalog.pg_sequence_last_value(r.oid) END",
				"pg_catalog.has_sequence_privilege(c.oid, 'SELECT,USAGE') AS readable",
				"pg_catalog.pg_is_other_temp_schema(c.relnamespace) AS other_temp",
				"c.relpersistence = 'u' AND pg_catalog.pg_is_in_recovery() AS unlogged_standby",
				"WHERE c.oid = $1 AND c.relkind = 'S'",
			} {
				if !strings.Contains(last.Text, w) {
					t.Errorf("no %q in\n%s", w, last.Text)
				}
			}
		})
	}

	_, _, q := loadedSequence(t, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	if _, _, err := q.LastValue(t.Context()); err == nil || errors.Is(err, pgmo.ErrNotFound) ||
		!strings.HasPrefix(err.Error(), "pgmo: read last value of sequence app.ids: ") || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
}
