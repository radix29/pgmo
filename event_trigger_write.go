package pgmo

import (
	"context"
	"fmt"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Event trigger writes
// ============================================================
//
// event_trigger_write.go is P4.13.3's event triggers: CREATE EVENT TRIGGER,
// ALTER EVENT TRIGGER … ENABLE/DISABLE / RENAME TO / OWNER TO, DROP EVENT
// TRIGGER (CASCADE opt-in), COMMENT ON EVENT TRIGGER. An event trigger
// belongs to one database and has no schema; creating, altering and
// dropping one takes a superuser, and its owner must be one — both left to
// the server (42501).

// CreateEventTriggerRequest describes a new event trigger.
type CreateEventTriggerRequest struct {
	Name string

	// Event is what it fires on; EventTriggerLogin is 17+.
	Event EventTriggerEvent

	// Tags are WHEN TAG IN's command tags ("CREATE TABLE", "DROP
	// FUNCTION"); nil fires for every command the event covers.
	Tags []string

	// Function is the function it executes: one returning event_trigger
	// and taking no arguments.
	Function ObjectName
}

// CreateEventTrigger creates the event trigger req describes, and returns it
// read back from the catalog — or, under Scripting(ctx), the
// EventTriggerRef handle. The login event before 17 is refused with
// ErrUnsupportedVersion before anything is sent.
func (d *Database) CreateEventTrigger(ctx context.Context, req CreateEventTriggerRequest) (*EventTrigger, error) {
	if req.Name == "" {
		return nil, invalidf("pgmo: create event trigger: no name")
	}
	op := "create event trigger " + pgsql.QuoteIdent(req.Name)
	switch req.Event {
	case EventTriggerDDLCommandStart, EventTriggerDDLCommandEnd, EventTriggerSQLDrop, EventTriggerTableRewrite:
	case EventTriggerLogin:
		if num := int(d.server.version); before(num, PostgreSQL17) {
			return nil, refusalf(ErrUnsupportedVersion, "pgmo: %s: the login event needs %s or later; the server is %s", op, PostgreSQL17, ServerVersion(num))
		}
	default:
		return nil, invalidf("pgmo: %s: unknown event %q", op, string(req.Event))
	}
	if err := checkQualified("function", req.Function.Schema, req.Function.Name); err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	stmt := "CREATE EVENT TRIGGER " + pgsql.QuoteIdent(req.Name) + " ON " + string(req.Event)
	if len(req.Tags) > 0 {
		tags := make([]string, len(req.Tags))
		for i, tag := range req.Tags {
			if tag == "" {
				return nil, invalidf("pgmo: %s: an empty tag", op)
			}
			tags[i] = pgsql.QuoteLiteral(tag)
		}
		stmt += " WHEN TAG IN (" + strings.Join(tags, ", ") + ")"
	}
	stmt += " EXECUTE FUNCTION " + req.Function.QualifiedName() + "()"
	if err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return createdObject(ctx, d.EventTriggerRef(req.Name), func() (*EventTrigger, error) {
		return d.EventTriggerByName(ctx, req.Name)
	})
}

func (e *EventTrigger) ddl(verb string) (ddlObject, error) {
	if e.Name == "" {
		return ddlObject{}, invalidf("pgmo: %s event trigger: no name", verb)
	}
	return ddlObject{keyword: "EVENT TRIGGER", name: pgsql.QuoteIdent(e.Name), run: e.db.exec}, nil
}

// SetEnabled sets when the event trigger fires — ALTER EVENT TRIGGER …
// ENABLE (TriggerEnabledOrigin), ENABLE REPLICA, ENABLE ALWAYS or DISABLE —
// mirrored onto Enabled (not under WithScript).
func (e *EventTrigger) SetEnabled(ctx context.Context, state TriggerEnabled) error {
	o, err := e.ddl("enable")
	if err != nil {
		return err
	}
	var action string
	switch state {
	case TriggerEnabledOrigin:
		action = "ENABLE"
	case TriggerEnabledReplica:
		action = "ENABLE REPLICA"
	case TriggerEnabledAlways:
		action = "ENABLE ALWAYS"
	case TriggerDisabled:
		action = "DISABLE"
	default:
		return invalidf("pgmo: %s: unknown state %q", o.op("enable"), string(state))
	}
	if err := o.do(ctx, "enable", "ALTER EVENT TRIGGER "+o.name+" "+action); err != nil {
		return err
	}
	setIfApplied(ctx, &e.Enabled, state)
	return nil
}

// Drop drops the event trigger (DROP EVENT TRIGGER), with CASCADE when
// opts.Cascade.
func (e *EventTrigger) Drop(ctx context.Context, opts DropOptions) error {
	return dropObject(ctx, e.ddl, opts)
}

// Rename renames the event trigger, mirrored onto Name (not under
// WithScript).
func (e *EventTrigger) Rename(ctx context.Context, newName string) error {
	return renameObject(ctx, e.ddl, newName, &e.Name)
}

// SetOwner makes role — a superuser, or the server refuses (42501) — the
// event trigger's owner, mirrored onto Owner (not under WithScript).
func (e *EventTrigger) SetOwner(ctx context.Context, role string) error {
	return ownObject(ctx, e.ddl, role, &e.Owner)
}

// SetComment sets the event trigger's comment, "" removing it, mirrored onto
// Comment (not under WithScript).
func (e *EventTrigger) SetComment(ctx context.Context, text string) error {
	return commentObject(ctx, e.ddl, text, &e.Comment)
}
