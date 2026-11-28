package pgmo

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Sequence writes
// ============================================================
//
// sequence_write.go is P4.13.3's sequences: CREATE SEQUENCE, ALTER
// SEQUENCE's options, RENAME TO, SET SCHEMA, OWNER TO, DROP SEQUENCE (CASCADE
// opt-in), COMMENT ON SEQUENCE. An identity column's sequence is the
// column's, and the server refuses most of these on it (0A000/2BP01).

// SequenceOwner is OWNED BY: the column a sequence belongs to, dropped with
// it. The zero SequenceOwner is OWNED BY NONE.
type SequenceOwner struct {
	Table  ObjectName
	Column string
}

// SequenceOptions are the options CREATE SEQUENCE and ALTER SEQUENCE share:
// each non-nil (or non-empty) field is written, the rest left to the server
// — its default on a create, unchanged on an alter.
type SequenceOptions struct {
	// DataType is AS: smallint, integer or bigint (the default), which
	// bounds MinValue and MaxValue.
	DataType string

	Increment *int64 // INCREMENT BY; negative for a descending sequence
	MinValue  *int64 // MINVALUE
	MaxValue  *int64 // MAXVALUE
	Start     *int64 // START WITH
	Cache     *int64 // CACHE: values preallocated per session, 1 for none
	Cycle     *bool  // CYCLE / NO CYCLE: wrap around at the limit

	// OwnedBy is OWNED BY table.column, or OWNED BY NONE for the zero
	// SequenceOwner; nil leaves it.
	OwnedBy *SequenceOwner
}

// options are o's clauses, or the refusal of one that cannot be written.
// No prefix: the caller names the operation.
func (o SequenceOptions) options() ([]string, error) {
	var opts []string
	if o.DataType != "" {
		opts = append(opts, "AS "+o.DataType)
	}
	num := func(clause string, v *int64) {
		if v != nil {
			opts = append(opts, clause+" "+strconv.FormatInt(*v, 10))
		}
	}
	num("INCREMENT BY", o.Increment)
	num("MINVALUE", o.MinValue)
	num("MAXVALUE", o.MaxValue)
	num("START WITH", o.Start)
	num("CACHE", o.Cache)
	if o.Cycle != nil {
		if *o.Cycle {
			opts = append(opts, "CYCLE")
		} else {
			opts = append(opts, "NO CYCLE")
		}
	}
	if o.OwnedBy != nil {
		switch ob := *o.OwnedBy; {
		case ob == SequenceOwner{}:
			opts = append(opts, "OWNED BY NONE")
		case ob.Column == "":
			return nil, invalidf("OWNED BY a table with no column")
		default:
			if err := checkQualified("owned by", ob.Table.Schema, ob.Table.Name); err != nil {
				return nil, err
			}
			opts = append(opts, "OWNED BY "+ob.Table.QualifiedName()+"."+pgsql.QuoteIdent(ob.Column))
		}
	}
	return opts, nil
}

// CreateSequenceRequest describes a new sequence.
type CreateSequenceRequest struct {
	Schema, Name string

	// Unlogged is UNLOGGED: not WAL-logged, reset by crash recovery.
	Unlogged bool

	SequenceOptions
}

// CreateSequence creates the sequence req describes, and returns it read
// back from the catalog — or, under Scripting(ctx), the SequenceRef handle.
func (d *Database) CreateSequence(ctx context.Context, req CreateSequenceRequest) (*Sequence, error) {
	if err := requireQualified("create sequence", req.Schema, req.Name); err != nil {
		return nil, err
	}
	op := "create sequence " + pgsql.QualifiedName(req.Schema, req.Name)
	opts, err := req.options()
	if err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	stmt := "CREATE "
	if req.Unlogged {
		stmt += "UNLOGGED "
	}
	stmt += "SEQUENCE " + pgsql.QualifiedName(req.Schema, req.Name)
	if len(opts) > 0 {
		stmt += " " + strings.Join(opts, " ")
	}
	if err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return createdObject(ctx, d.SequenceRef(req.Schema, req.Name), func() (*Sequence, error) {
		return d.SequenceByName(ctx, req.Schema, req.Name)
	})
}

func (q *Sequence) ddl(verb string) (ddlObject, error) {
	return q.db.qualified(verb, "SEQUENCE", q.Schema, q.Name)
}

// SequenceChanges is a partial ALTER SEQUENCE: the options set, and Restart.
type SequenceChanges struct {
	SequenceOptions

	// Restart is RESTART WITH: the value the next nextval returns. It does
	// not change Start.
	Restart *int64
}

// Alter applies every change set on c in one ALTER SEQUENCE, mirrored onto
// the matching fields (not under WithScript). An empty c sends nothing.
func (q *Sequence) Alter(ctx context.Context, c SequenceChanges) error {
	o, err := q.ddl("alter")
	if err != nil {
		return err
	}
	opts, err := c.options()
	if err != nil {
		return fmt.Errorf("pgmo: %s: %w", o.op("alter"), err)
	}
	if c.Restart != nil {
		opts = append(opts, "RESTART WITH "+strconv.FormatInt(*c.Restart, 10))
	}
	if len(opts) == 0 {
		return nil
	}
	if err := o.do(ctx, "alter", "ALTER SEQUENCE "+o.name+" "+strings.Join(opts, " ")); err != nil {
		return err
	}
	if c.DataType != "" {
		setIfApplied(ctx, &q.DataType, c.DataType)
	}
	setPtrIfApplied(ctx, &q.Increment, c.Increment)
	setPtrIfApplied(ctx, &q.MinValue, c.MinValue)
	setPtrIfApplied(ctx, &q.MaxValue, c.MaxValue)
	setPtrIfApplied(ctx, &q.Start, c.Start)
	setPtrIfApplied(ctx, &q.Cache, c.Cache)
	setPtrIfApplied(ctx, &q.Cycle, c.Cycle)
	if c.OwnedBy != nil {
		setIfApplied(ctx, &q.OwnedBy, c.OwnedBy.Table)
		setIfApplied(ctx, &q.OwnedByColumn, c.OwnedBy.Column)
	}
	return nil
}

// Drop drops the sequence (DROP SEQUENCE), with CASCADE when opts.Cascade —
// which drops the column defaults calling nextval on it.
func (q *Sequence) Drop(ctx context.Context, opts DropOptions) error {
	return dropObject(ctx, q.ddl, opts)
}

// Rename renames the sequence (ALTER SEQUENCE … RENAME TO), mirrored onto
// Name (not under WithScript). A default calling nextval on it follows it,
// since the call holds its OID.
func (q *Sequence) Rename(ctx context.Context, newName string) error {
	return renameObject(ctx, q.ddl, newName, &q.Name)
}

// SetSchema moves the sequence to schema, mirrored onto Schema (not under
// WithScript).
func (q *Sequence) SetSchema(ctx context.Context, schema string) error {
	return moveObject(ctx, q.ddl, schema, &q.Schema)
}

// SetOwner makes role the sequence's owner, mirrored onto Owner (not under
// WithScript). One owned by a column has its table's owner, and the server
// refuses another (55000).
func (q *Sequence) SetOwner(ctx context.Context, role string) error {
	return ownObject(ctx, q.ddl, role, &q.Owner)
}

// SetComment sets the sequence's comment, "" removing it, mirrored onto
// Comment (not under WithScript).
func (q *Sequence) SetComment(ctx context.Context, text string) error {
	return commentObject(ctx, q.ddl, text, &q.Comment)
}
