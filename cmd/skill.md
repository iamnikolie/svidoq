# svidoq — read-only SQL (agent reference)

`svidoq` runs a single read-only statement against a configured database and
prints the result. A write cannot reach the database through it.

## Shape of an invocation

```
svidoq --config <profile> [--env <env>] <command> <schema> [args] [flags]
```

- **profile** — one workspace (a job, a client). Required; there is no default,
  so a command cannot silently reach the wrong workspace. `SVIDOQ_CONFIG` also
  sets it.
- **env** — an environment inside the profile (a server: `prod`, `staging`).
  Defaults to the profile's `default_env`.
- **schema** — a database on that server.

The resolved target is printed to stderr before every query
(`→ acme/prod/billing  readonly@tcp(...)/billing`), so there is never doubt
about where a result came from. `--quiet` suppresses it.

## Commands

| Command | Purpose |
|---|---|
| `profiles` | list configured profiles (no profile needed) |
| `envs` | list environments in the profile |
| `schemas` | list schemas reachable in the environment |
| `check` | connect to every schema and report what answers |
| `tables <schema> [--filter LIKE]` | list tables |
| `describe <schema> <table>` | column name, type, nullability, key, default |
| `query <schema> <sql>` | run one read-only statement |
| `explain <schema> <sql>` | query plan for one read-only statement |
| `config init` | write a starter profile |
| `config show` | resolved profile, passwords masked |
| `skill` | this document |
| `version` | version, to stdout |

## Flags

| Flag | Meaning |
|---|---|
| `--config <name>` | profile; or `SVIDOQ_CONFIG` |
| `--env <name>` | environment within the profile |
| `--json` / `--format table\|json\|csv\|tsv` | output format; default `table` |
| `--limit N` | row cap for this query; clamped at the environment's `max_limit` |
| `--timeout 10s` | per-query deadline |
| `--quiet` | no target line, no row count on stderr |

## What is accepted

A **single** statement, one of: `SELECT`, `UNION`/`INTERSECT`/`EXCEPT`, read
`WITH`, `SHOW`, `DESCRIBE`, `EXPLAIN`.

Rejected, with the reason named: every write and DDL, `GRANT`/`REVOKE`, `SET`,
`USE`, `CALL`, `LOAD DATA`, `FLUSH`, `KILL`, transaction control, multiple
statements separated by `;`, `SELECT ... INTO OUTFILE/DUMPFILE` (writes to the
server's filesystem), and locking reads `FOR UPDATE` / `FOR SHARE` /
`LOCK IN SHARE MODE` (they change nothing but stall every writer).

The check is a real SQL parse, not a prefix match, so `SELECT`-shaped
statements that are not reads are caught. Everything then runs inside
`START TRANSACTION READ ONLY`, so the server refuses a write independently.

## Working with results

- Output is capped. A capped result says so on stderr: `truncated at N rows`.
  Narrow the query rather than raising the cap — that is usually the real fix.
- `--json` puts nothing but JSON on stdout: `{schema, env, columns, rows,
  row_count, truncated, elapsed_ms}`. Warnings go to stderr.
- In `table` output a single row is printed as key/value lines, NULL is printed
  as `NULL` (distinct from an empty string), and newlines inside a value are
  escaped so the alignment survives. CSV/TSV keep real newlines, quoted.

## Recipes

```bash
# orient in an unfamiliar database
svidoq --config acme schemas
svidoq --config acme tables billing --filter "invoice%"
svidoq --config acme describe billing invoices

# read
svidoq --config acme query billing "SELECT id, total FROM invoices WHERE client_id = 7"
svidoq --config acme --env staging query billing "SELECT COUNT(*) FROM invoices"

# feed a script
ids=$(svidoq --config acme --quiet --format tsv query billing "SELECT id FROM invoices LIMIT 10")

# why is it slow
svidoq --config acme explain billing "SELECT * FROM invoices WHERE client_id = 7"

# is it me or the database
svidoq --config acme check
```

## Failure modes worth recognising

| Message | Meaning |
|---|---|
| `statement is not read-only: ...` | layer 1 refused it; the reason names the statement kind |
| `write blocked by the read-only transaction` | layer 1 let something through and the server caught it — report it as a bug |
| `unknown schema ... (configured: ...)` | typo, or the schema is only defined in another environment |
| `incomplete configuration: no host for ...` | the environment has no `host` and the schema does not override one |
| `password_command failed: ...` | the secret manager refused; its stderr is included, usually "not signed in" |
| `connection failed` | network or credentials — run `check` to see which schemas are reachable |

## Configuration

`~/.svidoq/<profile>/config.yaml` (override the base with `SVIDOQ_HOME`), mode
0600. An environment describes a server; a schema describes a database on it.

```yaml
default_env: prod

environments:
  prod:
    host: db.internal
    user: readonly
    password_command: "op read op://work/db-prod/password"
    limit: 200
    max_limit: 5000
    timeout: 30s

schemas:
  billing:
    description: "invoices, payments"
  analytics:
    host: analytics-db.internal    # overrides only what differs
```

Passwords resolve in this order: the schema's, then the environment's, then
`SVIDOQ_PASSWORD_<ENV>`. At each level: `password` (a literal, used exactly as
written), `password_env` (the name of an environment variable), or
`password_command` (its stdout). Nothing is expanded, so a `$` in a password is
a `$`. A schema may set `url:` for a full DSN, bypassing composition entirely.
