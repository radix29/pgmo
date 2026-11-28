package pgmo

import (
	"context"
	"strconv"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Scripting routines: functions, procedures, aggregates
// ============================================================
//
// A function's and a procedure's CREATE is the server's own
// (pg_get_functiondef, Body.Definition), which always reads CREATE OR
// REPLACE: the script keeps it under ScriptAlter and writes a plain CREATE
// otherwise. An aggregate has no such function; its CREATE AGGREGATE is
// built from pg_aggregate's fields.

// signature is a routine's name with its argument types as the script
// writes it: qualified or bare by SchemaQualify.
func (o ScriptOptions) signature(r *RoutineIdentity) string {
	return o.name(r.Schema, r.Name) + "(" + strings.Join(r.ArgumentTypeNames, ", ") + ")"
}

// aggregateSignature is signature as AGGREGATE takes it: "ag(*)" for one
// taking no arguments.
func (o ScriptOptions) aggregateSignature(r *RoutineIdentity) string {
	if len(r.ArgumentTypeNames) == 0 {
		return o.name(r.Schema, r.Name) + "(*)"
	}
	return o.signature(r)
}

// routineCreate is pg_get_functiondef's text for keyword (FUNCTION,
// PROCEDURE) as the verb writes it: OR REPLACE only under ScriptAlter, and
// the routine's own name bare with SchemaQualify off. The server always
// qualifies the name; a header not in the expected form is left as the
// server wrote it.
func (o ScriptOptions) routineCreate(def, keyword string, r *RoutineIdentity) string {
	head := "CREATE OR REPLACE " + keyword + " " + pgsql.QualifiedName(r.Schema, r.Name) + "("
	rest, ok := strings.CutPrefix(def, head)
	if !ok {
		return strings.TrimRight(def, "\n")
	}
	return strings.TrimRight("CREATE "+o.orReplace()+keyword+" "+o.name(r.Schema, r.Name)+"("+rest, "\n")
}

type functionParts struct {
	function *Function
	body     RoutineBody
}

// ScriptFunction scripts the function (or window function) schema.name
// taking the input argument types args (nil when the name is not
// overloaded): the server's CREATE FUNCTION (OR REPLACE under ScriptAlter),
// owner, comment and privileges.
func (sc *Scripter) ScriptFunction(ctx context.Context, schema, name string, args []OID) (string, error) {
	op := "script function " + pgsql.QualifiedName(schema, name)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*functionParts, error) {
		f, err := sc.db.FunctionByName(ctx, schema, name, args)
		if err != nil {
			return nil, err
		}
		body, err := f.Body(ctx)
		if err != nil {
			return nil, err
		}
		return &functionParts{function: f, body: body}, nil
	}, func(p *functionParts) (string, error) {
		o, f := sc.opts, p.function
		sig := o.signature(&f.RoutineIdentity)
		if s, ok := o.member("function "+sig, f.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("FUNCTION", sig), func(s *scriptText) error {
			s.add(o.routineCreate(p.body.Definition, "FUNCTION", &f.RoutineIdentity))
			s.add(o.owner("FUNCTION", sig, f.Owner))
			s.add(o.comment("FUNCTION "+sig, f.Comment))
			s.add(o.privileges("FUNCTION "+sig, SecurableFunction, f.Owner, f.Privileges, f.PrivilegesDefault, sc.db.server.version)...)
			return nil
		})
	})
}

type procedureParts struct {
	procedure *Procedure
	body      RoutineBody
}

// ScriptProcedure scripts the procedure schema.name taking the input
// argument types args (nil when the name is not overloaded): the server's
// CREATE PROCEDURE (OR REPLACE under ScriptAlter), owner, comment and
// privileges.
func (sc *Scripter) ScriptProcedure(ctx context.Context, schema, name string, args []OID) (string, error) {
	op := "script procedure " + pgsql.QualifiedName(schema, name)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*procedureParts, error) {
		p, err := sc.db.ProcedureByName(ctx, schema, name, args)
		if err != nil {
			return nil, err
		}
		body, err := p.Body(ctx)
		if err != nil {
			return nil, err
		}
		return &procedureParts{procedure: p, body: body}, nil
	}, func(pp *procedureParts) (string, error) {
		o, p := sc.opts, pp.procedure
		sig := o.signature(&p.RoutineIdentity)
		if s, ok := o.member("procedure "+sig, p.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("PROCEDURE", sig), func(s *scriptText) error {
			s.add(o.routineCreate(pp.body.Definition, "PROCEDURE", &p.RoutineIdentity))
			s.add(o.owner("PROCEDURE", sig, p.Owner))
			s.add(o.comment("PROCEDURE "+sig, p.Comment))
			s.add(o.privileges("PROCEDURE "+sig, SecurableProcedure, p.Owner, p.Privileges, p.PrivilegesDefault, sc.db.server.version)...)
			return nil
		})
	})
}

// ScriptAggregate scripts the aggregate schema.name taking the argument
// types args (nil when the name is not overloaded): CREATE AGGREGATE (OR
// REPLACE under ScriptAlter) built from its fields, owner, comment and
// privileges.
func (sc *Scripter) ScriptAggregate(ctx context.Context, schema, name string, args []OID) (string, error) {
	op := "script aggregate " + pgsql.QualifiedName(schema, name)
	return scriptOf(ctx, sc.in(schema), func(ctx context.Context) (*Aggregate, error) {
		return sc.db.AggregateByName(ctx, schema, name, args)
	}, func(a *Aggregate) (string, error) {
		o := sc.opts
		sig := o.aggregateSignature(&a.RoutineIdentity)
		if s, ok := o.member("aggregate "+sig, a.Origin); ok {
			return s, nil
		}
		return o.script(op, o.drop("AGGREGATE", sig), func(s *scriptText) error {
			s.add(o.aggregateCreate(a))
			s.add(o.owner("AGGREGATE", sig, a.Owner))
			s.add(o.comment("AGGREGATE "+sig, a.Comment))
			// GRANT takes an aggregate under FUNCTION, whose form of a
			// zero-argument one is ag(), not ag(*).
			s.add(o.privileges("FUNCTION "+o.signature(&a.RoutineIdentity), SecurableFunction, a.Owner, a.Privileges, a.PrivilegesDefault, sc.db.server.version)...)
			return nil
		})
	})
}

// aggregateArgument is one argument as CREATE AGGREGATE's list writes it:
// [mode] [name] type, the mode only when VARIADIC (an aggregate's
// arguments are inputs).
func aggregateArgument(a RoutineArgument) string {
	var parts []string
	if a.Mode == ArgumentVariadic {
		parts = append(parts, a.Mode.String())
	}
	if a.Name != "" {
		parts = append(parts, pgsql.QuoteIdent(a.Name))
	}
	return strings.Join(append(parts, a.Type), " ")
}

// aggregateCreate is the aggregate's CREATE AGGREGATE, its options in the
// order the documentation lists them, each written only when it is not the
// default.
func (o ScriptOptions) aggregateCreate(a *Aggregate) string {
	args := make([]string, len(a.Arguments))
	for i, arg := range a.Arguments {
		args[i] = aggregateArgument(arg)
	}
	var list string
	switch {
	case a.AggregateKind != AggregateNormal && a.DirectArguments <= len(args):
		list = strings.Join(args[:a.DirectArguments], ", ")
		if list != "" {
			list += " "
		}
		list += "ORDER BY " + strings.Join(args[a.DirectArguments:], ", ")
	case len(args) == 0:
		list = "*"
	default:
		list = strings.Join(args, ", ")
	}
	fn := func(n ObjectName) string { return n.QualifiedName() }
	opts := []string{"SFUNC = " + fn(a.TransitionFunction), "STYPE = " + a.StateType}
	if a.StateSpace != 0 {
		opts = append(opts, "SSPACE = "+strconv.Itoa(a.StateSpace))
	}
	defaultModify := FinalReadOnly
	if a.AggregateKind != AggregateNormal {
		defaultModify = FinalReadWrite
	}
	if a.FinalFunction.Name != "" {
		opts = append(opts, "FINALFUNC = "+fn(a.FinalFunction))
		if a.FinalExtra {
			opts = append(opts, "FINALFUNC_EXTRA")
		}
	}
	if a.FinalModify != "" && a.FinalModify != defaultModify {
		opts = append(opts, "FINALFUNC_MODIFY = "+a.FinalModify.String())
	}
	if a.CombineFunction.Name != "" {
		opts = append(opts, "COMBINEFUNC = "+fn(a.CombineFunction))
	}
	if a.SerialFunction.Name != "" {
		opts = append(opts, "SERIALFUNC = "+fn(a.SerialFunction))
	}
	if a.DeserialFunction.Name != "" {
		opts = append(opts, "DESERIALFUNC = "+fn(a.DeserialFunction))
	}
	if a.HasInitialCondition {
		opts = append(opts, "INITCOND = "+pgsql.QuoteLiteral(a.InitialCondition))
	}
	if a.MovingTransitionFunction.Name != "" {
		opts = append(opts, "MSFUNC = "+fn(a.MovingTransitionFunction),
			"MINVFUNC = "+fn(a.MovingInverseFunction), "MSTYPE = "+a.MovingStateType)
		if a.MovingStateSpace != 0 {
			opts = append(opts, "MSSPACE = "+strconv.Itoa(a.MovingStateSpace))
		}
		if a.MovingFinalFunction.Name != "" {
			opts = append(opts, "MFINALFUNC = "+fn(a.MovingFinalFunction))
			if a.MovingFinalExtra {
				opts = append(opts, "MFINALFUNC_EXTRA")
			}
		}
		if a.MovingFinalModify != "" && a.MovingFinalModify != defaultModify {
			opts = append(opts, "MFINALFUNC_MODIFY = "+a.MovingFinalModify.String())
		}
		if a.HasMovingInitialCondition {
			opts = append(opts, "MINITCOND = "+pgsql.QuoteLiteral(a.MovingInitialCondition))
		}
	}
	if a.SortOperator != "" {
		opts = append(opts, "SORTOP = "+a.SortOperator)
	}
	switch a.Parallel {
	case ParallelSafe:
		opts = append(opts, "PARALLEL = SAFE")
	case ParallelRestricted:
		opts = append(opts, "PARALLEL = RESTRICTED")
	}
	if a.AggregateKind == AggregateHypothetical {
		opts = append(opts, "HYPOTHETICAL")
	}
	return "CREATE " + o.orReplace() + "AGGREGATE " + o.name(a.Schema, a.Name) + " (" + list + ") (\n    " +
		strings.Join(opts, ",\n    ") + "\n)"
}
