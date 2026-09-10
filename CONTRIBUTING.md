# Contributing

Thanks for taking a look. Bug reports and pull requests are welcome, and so is
a plain question in an issue.

## Reporting a bug

Include the output of `svidoq version`, the command you ran, and what you
expected. **Redact hostnames, schema names and query text** — this is a public
tracker and those describe someone's production system.

The most useful single line is usually what `svidoq --config <p> check` prints:
it separates "the query is wrong" from "this database is unreachable".

## The one rule

**A write must never reach a database through this tool.** Both layers stay:
the parser allowlist in `internal/driver/mysql/validator.go` and the read-only
transaction in `runReadOnly`. A change that removes either, or adds a code path
to the server that does not go through `runReadOnly`, will not be merged.

If you add an accepted statement type, add it to `TestValidateAcceptsReads`
*and* argue in the PR why it cannot write. Statement types that look like reads
and are not include `SELECT ... INTO OUTFILE`, locking reads, and anything
followed by a second statement — all three are already covered by tests, and
they are there because each one defeats a naive check.

## Pull requests

```bash
make fmt
make vet
make test
```

CI runs the same three (plus `-race`) on Linux and macOS. If you touched the
driver, run `make integration` too — it spins up MySQL in Docker, seeds it, and
runs the live suite including the deliberate layer-2 bypass.

House rules:

- **One concern per PR.**
- **Tests stay hermetic.** `go test ./...` must pass with no database and no
  network. Anything needing a server goes in `integration_test.go` behind
  `SVIDOQ_TEST_DSN`.
- **Data to stdout, everything else to stderr.** Warnings, row counts and the
  target line must never contaminate a pipe.
- **Nothing workspace-specific in the repository.** No real hostnames, schema
  names or credentials, in code, tests or docs. Examples use `acme`, `billing`,
  `db.internal`.
- **Update `cmd/skill.md` and `README.md` in the same commit** as any change to
  the CLI surface. `skill.md` is embedded in the binary and printed by
  `svidoq skill`.
- **Conventional commit subjects** — `feat:`, `fix:`, `docs:`, `refactor:`,
  `test:`, `chore:`.

## Adding a database engine

Implement `datasource.Datasource` in `internal/driver/<engine>/` and register it
in `internal/registry`. Both layers are per-engine work: the allowlist needs
that dialect's parser, and the transaction needs whatever the engine calls a
read-only transaction (`SET TRANSACTION READ ONLY` in PostgreSQL). An engine
without both is not ready to merge.

## Releases

Maintainer-only:

```bash
git tag -a v1.2.3 -m "v1.2.3"
git push origin v1.2.3
```

GoReleaser builds archives for linux/darwin/windows on amd64 and arm64.
