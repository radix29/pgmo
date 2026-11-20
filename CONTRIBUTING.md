# Contributing to pgmo

Thanks for your interest in pgmo.

## Before you start

Open an issue to discuss a change before sending a pull request, especially a
new exported API. Read `CLAUDE.md` and `ARCHITECTURE.md` for the conventions
the code follows.

## Checks

```sh
go build ./... && go vet ./... && gofmt -l .
go test ./...
go vet -tags livedb ./...   # compiles the live-database tests too
staticcheck ./...
```

`gofmt -l .` must print nothing. Tests tagged `livedb` need a PostgreSQL
server you own and are run with `-livedb '<libpq connection string or
service=name>'`; never point them at a server you do not own, create only
disposable objects, and never put a password in the argument — use a service
file or `~/.pgpass`. A change to SQL behaviour should be exercised against
PostgreSQL 15 and the newest release, not only the unit tests.

## Pull requests

- One logical change per PR, with tests for new behaviour.
- Explain what changed and why; link the issue.
- Do not edit `CHANGELOG.md` or `RELEASE.md`; the maintainer does at release.

## Issues

Bug reports, questions and feature requests are welcome. For a bug, please
include:

- the pgmo version (tag or commit) and Go version;
- the PostgreSQL version (`select version()`) and whether it is self-hosted or
  a managed service;
- a minimal snippet that reproduces it, and the error (with SQLSTATE) or wrong
  output.

Never paste connection strings, passwords or tokens.

## Security

Do not report vulnerabilities in a public issue. Use GitHub's private
vulnerability reporting on the repository's Security tab.

## Licence

pgmo is MIT licensed (see `LICENSE`).
