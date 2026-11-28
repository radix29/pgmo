package pgmo

import (
	"context"
	"fmt"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Trigger writes
// ============================================================
//
// trigger_write.go is P4.13.3's triggers: CREATE [OR REPLACE] [CONSTRAINT]
// TRIGGER from a request, DROP TRIGGER (CASCADE opt-in), ALTER TRIGGER …
// RENAME TO, ALTER TABLE … ENABLE/DISABLE TRIGGER, COMMENT ON TRIGGER. A
// trigger is named by its name ON its table, and lives in its table's
// schema.

// CreateTriggerRequest describes a new trigger on the table (or view, or
// foreign table) Schema.Table.
type CreateTriggerRequest struct {
	Schema, Table, Name string

	// OrReplace is OR REPLACE: an existing trigger of the name on the table
	// is redefined. Not for a constraint trigger.
	OrReplace bool

	// Constraint is CONSTRAINT TRIGGER: an AFTER … FOR EACH ROW trigger
	// whose firing can be deferred to the end of the transaction
	// (Deferrable, InitiallyDeferred).
	Constraint        bool
	Deferrable        bool
	InitiallyDeferred bool

	// Timing is BEFORE, AFTER or INSTEAD OF (a view's row triggers).
	Timing TriggerTiming

	// Events are what fires it, joined by OR; UpdateColumns restrict
	// UPDATE to an update of those columns (UPDATE OF …).
	Events        []TriggerEvent
	UpdateColumns []string

	// OldTable and NewTable are REFERENCING OLD TABLE AS / NEW TABLE AS:
	// the transition tables an AFTER trigger sees; "" for none.
	OldTable, NewTable string

	// ForEachRow is FOR EACH ROW; false is FOR EACH STATEMENT.
	ForEachRow bool

	// When is WHEN's condition as SQL (OLD and NEW in a row trigger); ""
	// for none.
	When string

	// Function is the trigger function EXECUTE FUNCTION calls, and
	// Arguments the string arguments it receives in TG_ARGV.
	Function  ObjectName
	Arguments []string
}

// createTriggerStatement is req's CREATE TRIGGER, or the refusal of one that
// cannot be written. No prefix: the caller names the operation.
func createTriggerStatement(req CreateTriggerRequest) (string, error) {
	switch req.Timing {
	case TriggerBefore, TriggerAfter, TriggerInsteadOf:
	default:
		return "", invalidf("unknown timing %q", string(req.Timing))
	}
	if len(req.Events) == 0 {
		return "", invalidf("no events")
	}
	if (req.Deferrable || req.InitiallyDeferred) && !req.Constraint {
		return "", invalidf("DEFERRABLE on a trigger that is not a constraint trigger")
	}
	if err := checkQualified("function", req.Function.Schema, req.Function.Name); err != nil {
		return "", err
	}
	if err := requireNames("UPDATE OF", req.UpdateColumns); err != nil {
		return "", err
	}
	var events []string
	update := false
	for _, e := range req.Events {
		switch e {
		case TriggerInsert, TriggerDelete, TriggerTruncate:
			events = append(events, string(e))
		case TriggerUpdate:
			update = true
			if len(req.UpdateColumns) > 0 {
				events = append(events, "UPDATE OF "+identList(req.UpdateColumns))
			} else {
				events = append(events, "UPDATE")
			}
		default:
			return "", invalidf("unknown event %q", string(e))
		}
	}
	if len(req.UpdateColumns) > 0 && !update {
		return "", invalidf("UPDATE OF columns with no UPDATE event")
	}

	var b strings.Builder
	b.WriteString("CREATE ")
	if req.OrReplace {
		b.WriteString("OR REPLACE ")
	}
	if req.Constraint {
		b.WriteString("CONSTRAINT ")
	}
	b.WriteString("TRIGGER " + pgsql.QuoteIdent(req.Name) + " " + string(req.Timing) + " " + strings.Join(events, " OR ") +
		" ON " + pgsql.QualifiedName(req.Schema, req.Table))
	if req.Deferrable {
		b.WriteString(" DEFERRABLE")
	}
	if req.InitiallyDeferred {
		b.WriteString(" INITIALLY DEFERRED")
	}
	if req.OldTable != "" || req.NewTable != "" {
		b.WriteString(" REFERENCING")
		if req.OldTable != "" {
			b.WriteString(" OLD TABLE AS " + pgsql.QuoteIdent(req.OldTable))
		}
		if req.NewTable != "" {
			b.WriteString(" NEW TABLE AS " + pgsql.QuoteIdent(req.NewTable))
		}
	}
	if req.ForEachRow {
		b.WriteString(" FOR EACH ROW")
	} else {
		b.WriteString(" FOR EACH STATEMENT")
	}
	if req.When != "" {
		b.WriteString(" WHEN (" + req.When + ")")
	}
	args := make([]string, len(req.Arguments))
	for i, a := range req.Arguments {
		args[i] = pgsql.QuoteLiteral(a)
	}
	b.WriteString(" EXECUTE FUNCTION " + req.Function.QualifiedName() + "(" + strings.Join(args, ", ") + ")")
	return b.String(), nil
}

// requireTrigger refuses a trigger without a name, a table or a schema.
func requireTrigger(op, schema, table, name string) error {
	if name == "" {
		return invalidf("pgmo: %s: no name", op)
	}
	return requireQualified(op+" "+pgsql.QuoteIdent(name)+" on table", schema, table)
}

// CreateTrigger creates the trigger req describes, and returns it read back
// from the catalog — or, under Scripting(ctx), the TriggerRef handle. The
// role needs TRIGGER on the table and EXECUTE on the function.
func (d *Database) CreateTrigger(ctx context.Context, req CreateTriggerRequest) (*Trigger, error) {
	if err := requireTrigger("create trigger", req.Schema, req.Table, req.Name); err != nil {
		return nil, err
	}
	op := "create trigger " + pgsql.QuoteIdent(req.Name) + " on " + pgsql.QualifiedName(req.Schema, req.Table)
	stmt, err := createTriggerStatement(req)
	if err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	if err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return createdObject(ctx, d.TriggerRef(req.Schema, req.Table, req.Name), func() (*Trigger, error) {
		return d.TriggerByName(ctx, req.Schema, req.Table, req.Name)
	})
}

// ddl is the trigger as DROP, ALTER and COMMENT ON name it: name ON table.
func (t *Trigger) ddl(verb string) (ddlObject, error) {
	if err := requireTrigger(verb+" trigger", t.Schema, t.Table, t.Name); err != nil {
		return ddlObject{}, err
	}
	return ddlObject{keyword: "TRIGGER", name: pgsql.QuoteIdent(t.Name) + " ON " + t.QualifiedTable(), run: t.db.exec}, nil
}

// Drop drops the trigger (DROP TRIGGER … ON), with CASCADE when
// opts.Cascade. A trigger a partitioned table's cloned onto a partition
// (Inherited) is dropped with its parent's, and the server refuses it alone
// (2BP01).
func (t *Trigger) Drop(ctx context.Context, opts DropOptions) error {
	return dropObject(ctx, t.ddl, opts)
}

// Rename renames the trigger (ALTER TRIGGER … ON … RENAME TO), mirrored onto
// Name (not under WithScript).
func (t *Trigger) Rename(ctx context.Context, newName string) error {
	return renameObject(ctx, t.ddl, newName, &t.Name)
}

// SetComment sets the trigger's comment (COMMENT ON TRIGGER … ON), ""
// removing it, mirrored onto Comment (not under WithScript).
func (t *Trigger) SetComment(ctx context.Context, text string) error {
	return commentObject(ctx, t.ddl, text, &t.Comment)
}

// SetEnabled sets when the trigger fires — ALTER TABLE … ENABLE TRIGGER
// (TriggerEnabledOrigin), ENABLE REPLICA, ENABLE ALWAYS or DISABLE —
// mirrored onto Enabled (not under WithScript). The server takes it on a
// table's trigger, not a view's (42809); disabling a constraint trigger
// (a foreign key's) needs superuser.
func (t *Trigger) SetEnabled(ctx context.Context, e TriggerEnabled) error {
	o, err := t.ddl("enable")
	if err != nil {
		return err
	}
	var action string
	switch e {
	case TriggerEnabledOrigin:
		action = "ENABLE"
	case TriggerEnabledReplica:
		action = "ENABLE REPLICA"
	case TriggerEnabledAlways:
		action = "ENABLE ALWAYS"
	case TriggerDisabled:
		action = "DISABLE"
	default:
		return invalidf("pgmo: %s: unknown state %q", o.op("enable"), string(e))
	}
	if err := o.do(ctx, "enable", "ALTER TABLE "+t.QualifiedTable()+" "+action+" TRIGGER "+pgsql.QuoteIdent(t.Name)); err != nil {
		return err
	}
	setIfApplied(ctx, &t.Enabled, e)
	return nil
}
