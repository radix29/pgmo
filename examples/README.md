# Examples

Runnable programs, mirroring gosmo's `examples/`. Planned; none exists yet.
Each will create its own throwaway database and drop it afterwards; nothing
already on the server is modified.

Connection: a libpq connection string or service name in `PGMO_DSN`
(e.g. `PGMO_DSN='service=pg18'`); the password comes from `~/.pgpass` or the
service file, never from the environment or the command line.

| Program | Will cover |
| --- | --- |
| `go run ./examples` | Guided tour of the whole library (`internal/demo`) |
| `go run ./examples/copy` | `COPY FROM`/`TO` from a slice, a generator and a streaming CSV |
| `go run ./examples/diagnostic` | `AsPgError`, SQLSTATE classes, EXPLAIN, dependencies, `pg_stat_*` reads |
| `go run ./examples/jobs` | pg_cron jobs and run history (where the extension exists) |
| `go run ./examples/maintain` | VACUUM, ANALYZE, REINDEX, sizes, bloat, `pg_stat_statements` |
| `go run ./examples/scripting` | The `Scripter`, and `WithScript`'s collect-instead-of-execute mode |
| `go run ./examples/security` | Roles, memberships, privileges, default privileges, RLS policies |
