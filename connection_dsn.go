package pgmo

import (
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultApplicationName is application_name when no field, connection
// string, service or PGAPPNAME names one.
const defaultApplicationName = "pgmo"

// fieldKeywords maps each libpq keyword a ConnectionOptions field controls to
// the field, for Params' refusal. "database" is pgx's spelling of dbname,
// which pgx accepts as a keyword too.
var fieldKeywords = map[string]string{
	"host":                 "Host",
	"port":                 "Port",
	"dbname":               "Database",
	"database":             "Database",
	"user":                 "User",
	"password":             "Password",
	"service":              "Service",
	"sslmode":              "SSLMode",
	"sslrootcert":          "SSLRootCert",
	"sslcert":              "SSLCert",
	"sslkey":               "SSLKey",
	"application_name":     "ApplicationName",
	"connect_timeout":      "ConnectTimeout",
	"target_session_attrs": "TargetSessionAttrs",
}

// connectionKeywords are the connection-string keywords no field controls
// that are not server runtime parameters: pgx (or pgxpool) reads them
// itself, or, for "options", "replication" and "ssl", sends or aliases them
// as something other than one setting. Params refuses them — written there,
// they would change how the connection is made, not a setting of the
// session — and they go in ConnString instead.
// TestConnectionKeywordsAreNotRuntimeParams holds the list to pgx.
var connectionKeywords = []string{
	// pgconn
	"passfile", "servicefile", "sslcertmode", "sslnegotiation", "sslpassword",
	"sslsni", "krbspn", "krbsrvname", "min_protocol_version",
	"max_protocol_version", "channel_binding", "require_auth",
	// pgx
	"statement_cache_capacity", "description_cache_capacity",
	"default_query_exec_mode",
	// pgxpool
	"pool_max_conns", "pool_min_conns", "pool_min_idle_conns",
	"pool_max_conn_lifetime", "pool_max_conn_lifetime_jitter",
	"pool_max_conn_idle_time", "pool_health_check_period", "pool_ping_timeout",
	// sent as startup parameters, but not settings: libpq's options (the
	// command-line switches Params replaces), a walsender request, and the
	// URI alias of sslmode=require.
	"options", "replication", "ssl",
}

// poolConfig resolves o into the pgxpool.Config Connect opens: o's fields are
// checked first, each refusal naming the field, then written over
// o.ConnString, so pgx's ParseConfig resolves the rest as libpq does — field
// → ConnString → service file → PG* environment → password file.
//
// Password alone is not written into the string: an explicit Password wins
// over every other source, so it is set on the parsed config instead, and a
// parse failure can never quote it. The pgx error of a failed parse is not
// kept in the chain either, since its ConnString field holds the caller's
// connection string, password and all; its (redacted) text and its cause are.
//
// The returned error is prefixed "pgmo: connection options: " and wraps
// ErrInvalidRequest.
func (o ConnectionOptions) poolConfig() (*pgxpool.Config, error) {
	connString, err := o.connString()
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, parseConfigError(err)
	}
	if o.Password != "" {
		cfg.ConnConfig.Password = o.Password
	}
	if cfg.ConnConfig.RuntimeParams["application_name"] == "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = defaultApplicationName
	}
	return cfg, nil
}

// parseConfigError is poolConfig's error for a ParseConfig failure: pgx's
// text, which redacts the passwords it recognises, and its cause (a missing
// service, an unreadable certificate), but not the *pgconn.ParseConfigError
// itself, whose ConnString is unredacted.
func parseConfigError(err error) error {
	pce, ok := errors.AsType[*pgconn.ParseConfigError](err)
	if !ok {
		return invalidf("pgmo: connection options: %w", err)
	}
	return &refusalError{
		msg:   "pgmo: connection options: " + pce.Error(),
		kind:  ErrInvalidRequest,
		cause: pce.Unwrap(),
	}
}

// connString checks o's fields and returns o.ConnString with each set field
// but Password written after it, in ConnString's own form (keyword/value or
// URI), so that the field wins: pgx keeps the last value of a repeated
// keyword in both forms, and in a URI a query parameter over the host, port,
// user and database the URI's authority names.
func (o ConnectionOptions) connString() (string, error) {
	if err := o.validate(); err != nil {
		return "", err
	}
	kv := o.keywords()
	uri := isURI(o.ConnString)
	var b strings.Builder
	b.WriteString(o.ConnString)
	if !uri && oddTrailingBackslashes(o.ConnString) {
		// A backslash ending keyword/value text escapes the end, and pgx
		// drops it; followed by a space, it would escape that instead.
		b.Reset()
		b.WriteString(o.ConnString[:len(o.ConnString)-1])
	}
	sep := " "
	if uri {
		sep = "&"
		if !uriHasQuery(o.ConnString) {
			sep = "?"
		}
	}
	for _, k := range slices.Sorted(maps.Keys(kv)) {
		if b.Len() > 0 {
			b.WriteString(sep)
		}
		if uri {
			sep = "&"
			b.WriteString(uriEscape(k) + "=" + uriEscape(kv[k]))
		} else {
			b.WriteString(k + "=" + quoteKeywordValue(kv[k]))
		}
	}
	return b.String(), nil
}

// keywords is the connection-string keywords o's fields and Params set:
// each non-zero field but Password under its libpq keyword, and each Params
// entry as written. validate has run, so no Params key collides with a field.
func (o ConnectionOptions) keywords() map[string]string {
	kv := make(map[string]string, len(o.Params)+12)
	maps.Copy(kv, o.Params)
	set := func(k, v string) {
		if v != "" {
			kv[k] = v
		}
	}
	set("host", o.Host)
	if o.Port != 0 {
		kv["port"] = strconv.Itoa(o.Port)
	}
	set("dbname", o.Database)
	set("user", o.User)
	set("service", o.Service)
	set("sslmode", string(o.SSLMode))
	set("sslrootcert", o.SSLRootCert)
	set("sslcert", o.SSLCert)
	set("sslkey", o.SSLKey)
	set("application_name", o.ApplicationName)
	if o.ConnectTimeout > 0 {
		kv["connect_timeout"] = strconv.FormatInt(int64((o.ConnectTimeout+time.Second-1)/time.Second), 10)
	}
	set("target_session_attrs", string(o.TargetSessionAttrs))
	return kv
}

// validate refuses, naming the field, what would otherwise fail inside pgx
// worded in keywords the caller never wrote, or silently mean something else.
func (o ConnectionOptions) validate() error {
	for _, f := range []struct{ name, v string }{
		{"Host", o.Host}, {"Database", o.Database}, {"User", o.User},
		{"Password", o.Password}, {"Service", o.Service},
		{"SSLRootCert", o.SSLRootCert}, {"SSLCert", o.SSLCert}, {"SSLKey", o.SSLKey},
		{"ApplicationName", o.ApplicationName}, {"ConnString", o.ConnString},
	} {
		if strings.IndexByte(f.v, 0) >= 0 {
			return invalidf("pgmo: connection options: ConnectionOptions.%s holds a NUL byte", f.name)
		}
	}
	if o.Port < 0 || o.Port > 65535 {
		return invalidf("pgmo: connection options: ConnectionOptions.Port %d is outside 1-65535", o.Port)
	}
	if o.ConnectTimeout < 0 {
		return invalidf("pgmo: connection options: ConnectionOptions.ConnectTimeout %v is negative", o.ConnectTimeout)
	}
	if o.SSLMode != "" && !slices.Contains(sslModes, o.SSLMode) {
		return invalidf("pgmo: connection options: ConnectionOptions.SSLMode %q is not one of %s", o.SSLMode, joinQuoted(sslModes))
	}
	if o.TargetSessionAttrs != "" && !slices.Contains(targetSessionAttrs, o.TargetSessionAttrs) {
		return invalidf("pgmo: connection options: ConnectionOptions.TargetSessionAttrs %q is not one of %s", o.TargetSessionAttrs, joinQuoted(targetSessionAttrs))
	}
	if (o.SSLCert == "") != (o.SSLKey == "") {
		return invalidf("pgmo: connection options: ConnectionOptions.SSLCert and SSLKey are set together or not at all")
	}
	return o.validateParams()
}

// validateParams refuses a Params key that is not a runtime parameter's name
// (letters, digits, '_' and '.', as a setting's name is), that a field
// controls, that is a connection keyword rather than a setting, or that is
// given twice in different case (setting names are case-insensitive).
// Matching is case-insensitive throughout, so "Host" is refused as "host".
func (o ConnectionOptions) validateParams() error {
	seen := make(map[string]string, len(o.Params))
	for _, k := range slices.Sorted(maps.Keys(o.Params)) {
		lk := strings.ToLower(k)
		switch {
		case !isSettingName(k):
			return invalidf("pgmo: connection options: ConnectionOptions.Params key %q is not a runtime parameter name", k)
		case fieldKeywords[lk] != "":
			return invalidf("pgmo: connection options: ConnectionOptions.Params key %q is set through ConnectionOptions.%s, not Params", k, fieldKeywords[lk])
		case slices.Contains(connectionKeywords, lk):
			return invalidf("pgmo: connection options: ConnectionOptions.Params key %q is a connection parameter, not a runtime parameter; write it in ConnString", k)
		case seen[lk] != "":
			return invalidf("pgmo: connection options: ConnectionOptions.Params keys %q and %q name the same parameter", seen[lk], k)
		case strings.IndexByte(o.Params[k], 0) >= 0:
			return invalidf("pgmo: connection options: ConnectionOptions.Params[%q] holds a NUL byte", k)
		}
		seen[lk] = k
	}
	return nil
}

// sslModes and targetSessionAttrs are every value of their type, for
// validate.
var (
	sslModes = []SSLMode{
		SSLModeDisable, SSLModeAllow, SSLModePrefer,
		SSLModeRequire, SSLModeVerifyCA, SSLModeVerifyFull,
	}
	targetSessionAttrs = []TargetSessionAttrs{
		TargetSessionAny, TargetSessionReadWrite, TargetSessionReadOnly,
		TargetSessionPrimary, TargetSessionStandby, TargetSessionPreferStandby,
	}
)

// joinQuoted is vs as "a", "b", "c", for a refusal listing the values a field
// takes.
func joinQuoted[S ~string](vs []S) string {
	q := make([]string, len(vs))
	for i, v := range vs {
		q[i] = strconv.Quote(string(v))
	}
	return strings.Join(q, ", ")
}

// isSettingName reports whether k has the shape of a server setting's name,
// an extension's ("pg_stat_statements.track") included.
func isSettingName(k string) bool {
	if k == "" {
		return false
	}
	for _, c := range k {
		if !(c == '_' || c == '.' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z') {
			return false
		}
	}
	return true
}

// isURI reports whether s is a connection URI rather than keyword/value, by
// pgx's test.
func isURI(s string) bool {
	return strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://")
}

// oddTrailingBackslashes reports whether s ends in an odd run of
// backslashes: its last one escapes nothing but the end of the text.
func oddTrailingBackslashes(s string) bool {
	n := len(s) - len(strings.TrimRight(s, `\`))
	return n%2 == 1
}

// uriHasQuery reports whether the URI s has a query part, as pgx's parser
// finds it: the first '?' after the user information, which may hold one
// unescaped.
func uriHasQuery(s string) bool {
	_, p, _ := strings.Cut(s, "://")
	if i := strings.IndexAny(p, "@/"); i >= 0 && p[i] == '@' {
		p = p[i+1:]
	}
	return strings.IndexByte(p, '?') >= 0
}

// uriEscape percent-encodes every byte of s but RFC 3986's unreserved ones.
// pgx's URI decoder takes no '+' for a space and ends a value at a raw one,
// so url.QueryEscape will not do.
func uriEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if c == '-' || c == '.' || c == '_' || c == '~' ||
			'0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

// quoteKeywordValue is v as a quoted keyword/value value: in single quotes,
// with a backslash before each quote and backslash, as libpq reads it.
func quoteKeywordValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(v) + "'"
}
