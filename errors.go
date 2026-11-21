package pgmo

import (
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgconn"
)

// ============================================================
// Sentinels
// ============================================================
//
// Every refusal pgmo makes, and every absence it reports as an error, wraps
// one of the sentinels below, so a caller tells the kinds apart with
// errors.Is instead of matching on message text. A sentinel never changes a
// message: the text reads on its own and reaches the sentinel through the
// error chain. The server's own errors are *pgconn.PgError and are classified
// by SQLSTATE, not by these.

// ErrNotFound is the sentinel every by-name lookup that reports absence as an
// error wraps, so a caller can tell "this object does not exist" from "the
// lookup itself failed" with errors.Is(err, pgmo.ErrNotFound).
//
// Making that distinction matters: a caller that treats any error as absence
// will go on to create an object it never established was missing, and report
// the creation's failure instead of the permission or connection error that
// actually stopped it.
//
// Every XByName lookup returns an error wrapping ErrNotFound for a name the
// catalog does not hold; none answers (nil, nil). An optional singleton (one
// an object may or may not have) reports "none" the same way.
var ErrNotFound = errors.New("not found")

// ErrAmbiguous is wrapped by a by-name lookup whose name the catalog does not
// keep unique and which matched more than one object, so a caller can tell
// "pick by oid" from "not there" (ErrNotFound) with errors.Is.
//
// Returning one of the matches instead would be wrong: a write keyed by its
// oid then lands on whichever row the server happened to return first. A
// function name is the PostgreSQL case — overloads share a name in one
// schema, told apart only by their argument types; the error names the
// lookup that is exact.
var ErrAmbiguous = errors.New("ambiguous name")

// ErrSchemaRequired is wrapped by every call given a schema-scoped name with
// an empty schema. Nothing defaults it: public, the first schema on the
// role's search_path and the role's own schema are all plausible readings,
// and guessing wrong addresses — or drops — a different object.
var ErrSchemaRequired = errors.New("schema is required")

// ErrHandleNotLoaded is wrapped by every read keyed by a catalog oid the
// receiver does not have — a Table from a …Ref handle, whose OID is zero, or
// any other lookup-free handle whose child reads key on an oid. The read
// would otherwise ask for oid 0 and get back an empty result
// indistinguishable from an object with no children. Read the object with
// its XByName lookup (or the listing) instead.
var ErrHandleNotLoaded = errors.New("handle not loaded: read it by name first")

// ErrUnsupportedVersion reports a call pgmo refused because the connected
// server is older than the feature it names — a refusal decided here, before
// any statement is sent, because the server's own answer would be a syntax
// error or an undefined column naming text the caller never wrote.
//
// Every such refusal wraps it, so a caller (or a sweep across an old cluster)
// can tell "this server is too old for this feature" from "this read is
// broken". Servers below MinimumServerVersion are refused at connect, so a
// gate below it never fires.
var ErrUnsupportedVersion = errors.New("unsupported server version")

// ErrUnsupported reports a request pgmo refuses because it has no faithful
// form for it, decided before any text is produced or statement sent: a
// script of an object it cannot express, or a read with no form for its
// input. A script that recreates something different under the same name is
// worse than none, and under DROP AND CREATE it drops the object and never
// recreates it.
//
// It is not ErrUnsupportedVersion: the server is not too old for anything,
// pgmo simply has no form for the request.
var ErrUnsupported = errors.New("not supported")

// ErrDefinitionHidden reports a script refused because the connected role
// cannot see a definition it needs. PostgreSQL's catalogs are mostly
// readable by every role, but not all of them: pg_subscription.subconninfo
// is readable only by superusers, and pg_user_mappings answers NULL options
// unless the role is the mapped user with USAGE on the server, a member of
// the server's owner, or a superuser.
// Every such refusal also wraps ErrUnsupported, so it is a permission gap a
// caller can name, not a different kind of refusal.
var ErrDefinitionHidden = errors.New("definition not visible to the role")

// ErrInvalidRequest reports a call pgmo refused because of its own
// arguments, before any statement was sent: a privilege name no allowlist
// holds for the object type, a modifier the verb has no form for, an empty
// column list, a value that cannot be written safely. The request is wrong
// as asked; retrying it, or asking another server, gives the same answer.
//
// It is distinct from ErrUnsupported, where the request is well formed but
// pgmo has no faithful form for it, and from ErrSchemaRequired, which a
// missing schema wraps instead.
var ErrInvalidRequest = errors.New("invalid request")

// ============================================================
// Refusals
// ============================================================
//
// An error pgmo decides on its own — one that wraps nothing the server or
// pgx returned — is built by one of the functions below, never by a bare
// fmt.Errorf or errors.New, so it always reaches a sentinel
// (invalid_request_test.go enforces it):
//
//	invalidf      the caller's arguments are wrong   ErrInvalidRequest
//	unsupportedf  pgmo has no faithful form for it    ErrUnsupported
//	notFoundf     the named object does not exist     ErrNotFound
//	ambiguousf    the name matched more than one      ErrAmbiguous
//
// The message is exactly format/args; the sentinel is reached through the
// chain and never changes the text. A %w in format keeps that error
// reachable too, as fmt.Errorf would: a refusal for a definition the role
// cannot see is unsupportedf("…: %w", ErrDefinitionHidden), which satisfies
// both ErrUnsupported and ErrDefinitionHidden.
//
// The prefix rule: an error's text carries "pgmo: " and the attempted
// operation exactly once — `pgmo: drop table "s"."t": …`. The exported
// method the caller called adds it; an unexported helper whose caller wraps
// its error leaves it off, and a caller of an exported pgmo method returns
// its error as is (TestNoWrapOfAnAlreadyPrefixedError).

// refusalError carries its own message and reaches its sentinel — and, when
// its format wrapped one with %w, the cause too — through the chain.
type refusalError struct {
	msg   string
	kind  error // the sentinel: ErrInvalidRequest, ErrUnsupported or ErrNotFound
	cause error // the fmt.Errorf result when it wraps something, else nil
}

func (e *refusalError) Error() string { return e.msg }

func (e *refusalError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.kind}
	}
	return []error{e.kind, e.cause}
}

// refusalf builds a refusal reaching kind whose message is exactly
// format/args.
func refusalf(kind error, format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	e := &refusalError{msg: err.Error(), kind: kind}
	switch err.(type) {
	case interface{ Unwrap() error }, interface{ Unwrap() []error }:
		e.cause = err
	}
	return e
}

// invalidf builds a refusal of the caller's arguments (ErrInvalidRequest).
func invalidf(format string, args ...any) error {
	return refusalf(ErrInvalidRequest, format, args...)
}

// unsupportedf builds a refusal of a request pgmo has no faithful form for
// (ErrUnsupported).
func unsupportedf(format string, args ...any) error {
	return refusalf(ErrUnsupported, format, args...)
}

// notFoundf builds the error of a lookup whose object does not exist
// (ErrNotFound).
func notFoundf(format string, args ...any) error {
	return refusalf(ErrNotFound, format, args...)
}

// ambiguousf builds the error of a by-name lookup that matched more than one
// object (ErrAmbiguous).
func ambiguousf(format string, args ...any) error {
	return refusalf(ErrAmbiguous, format, args...)
}

// ============================================================
// The server's errors
// ============================================================
//
// PostgreSQL reports every error with a five-character SQLSTATE, the same
// in every server language and release; the message follows lc_messages.
// Every helper below is keyed on the SQLSTATE, never the wording. Each code
// was confirmed on pg15 and pg18 from the statement that raises it, not taken
// from the documentation's list.

// AsPgError reports whether err, or any error it wraps, is an error the
// PostgreSQL server sent and, if so, returns it. Its Code is the SQLSTATE;
// Detail, Hint, Where and Position are what psql shows below the message.
//
// An error pgx produced itself (a dropped connection, a scan into the wrong
// type, a cancelled context) is not one, and nor is any pgmo refusal: those
// wrap the sentinels above instead.
func AsPgError(err error) (*pgconn.PgError, bool) {
	return errors.AsType[*pgconn.PgError](err)
}

// hasSQLState reports whether err carries a server error whose SQLSTATE is
// one of codes.
func hasSQLState(err error, codes ...string) bool {
	pe, ok := AsPgError(err)
	return ok && slices.Contains(codes, pe.Code)
}

// IsPermissionDenied reports whether err is the server refusing the role a
// privilege: SQLSTATE 42501 insufficient_privilege, which every "permission
// denied for table …", "must be owner of …" and "must be superuser to …"
// carries.
//
// Unlike SQL Server, PostgreSQL does not blur a missing object into a refused
// one: an object the role cannot see still reports it does not exist
// (IsUndefinedObject) and one it can see but not use reports 42501, so
// neither helper needs to answer for the other.
func IsPermissionDenied(err error) bool {
	return hasSQLState(err, "42501")
}

// undefinedObjectCodes are the SQLSTATEs a statement naming a catalog object
// that does not exist fails with:
//
//	42P01 undefined_table      relation "…" does not exist (tables, views,
//	                           sequences, indexes, materialized views)
//	42704 undefined_object     role, type, extension, … "…" does not exist
//	42883 undefined_function   function …(…) does not exist (and operators)
//	3F000 invalid_schema_name  schema "…" does not exist
//	3D000 invalid_catalog_name database "…" does not exist (also at connect)
//
// 42703 undefined_column is deliberately absent: from one of pgmo's own reads
// it means the read names a column this server lacks — a broken read or a
// missing version gate — and reporting it as absence would hide that.
var undefinedObjectCodes = []string{"42P01", "42704", "42883", "3F000", "3D000"}

// IsUndefinedObject reports whether err is the server saying a named object
// does not exist (see undefinedObjectCodes): a DROP or ALTER of something
// already gone, a connect to a database that was dropped.
func IsUndefinedObject(err error) bool {
	return hasSQLState(err, undefinedObjectCodes...)
}

// alreadyExistsCodes are the SQLSTATEs a CREATE fails with when its name is
// taken:
//
//	42P07 duplicate_table     relation "…" already exists (any relation,
//	                          indexes included: they share pg_class)
//	42710 duplicate_object    role, type, constraint, … "…" already exists
//	42723 duplicate_function  function "…" already exists with same
//	                          argument types
//	42P06 duplicate_schema    schema "…" already exists
//	42P04 duplicate_database  database "…" already exists
//
// 42701 duplicate_column is absent for the same reason as 42703 above.
var alreadyExistsCodes = []string{"42P07", "42710", "42723", "42P06", "42P04"}

// IsAlreadyExists reports whether err is the server refusing a CREATE because
// the name is already taken (see alreadyExistsCodes).
func IsAlreadyExists(err error) bool {
	return hasSQLState(err, alreadyExistsCodes...)
}

// IsQueryCanceled reports whether err is SQLSTATE 57014 query_canceled: the
// statement was stopped by a cancel request (sent when the context ends
// mid-statement on a connection with UseCancelRequests, as every one pgmo
// builds from ConnectionOptions has), by statement_timeout, or by another
// session's pg_cancel_backend. The connection survives; the statement did
// not run to the end. pgmo's own methods join the context's error to it when
// the context caused it.
func IsQueryCanceled(err error) bool {
	return hasSQLState(err, "57014")
}
