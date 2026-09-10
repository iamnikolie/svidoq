# svidoq

[![CI](https://github.com/iamnikolie/svidoq/actions/workflows/ci.yml/badge.svg)](https://github.com/iamnikolie/svidoq/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/iamnikolie/svidoq.svg)](https://pkg.go.dev/github.com/iamnikolie/svidoq)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Read-only SQL for agents. A database an agent can question without flooding its
context, leaking your credentials, or reaching the wrong server.

Named for the Ukrainian *свідок* — a witness. A witness sees everything and
changes nothing, and what it gives you is testimony about what was.

Sibling of [`fibery-cli`](https://github.com/iamnikolie/fibery-cli),
[`gitlab-cli`](https://github.com/iamnikolie/gitlab-cli),
[`slack-cli`](https://github.com/iamnikolie/slack-cli) and
[`dziga`](https://github.com/iamnikolie/dziga): same doctrine — plain text on
stdout, nothing costs context until it is called.

```bash
svidoq --config acme --env prod query billing "SELECT id, total FROM invoices WHERE client_id = 7"
```

## Why not just let the agent run `mysql`?

Four reasons, and only one of them is about writes.

**Your credentials stay out of the transcript.** When an agent runs
`mysql -h prod -u app -p'hunter2'`, that password is now in the conversation, in
the shell history, and in every log that conversation touches. Here the agent
names a schema; it never sees a DSN.

**Results are capped and timed.** The daily failure of a database in an agent
loop is not a dropped table, it is forty thousand rows landing in a context
window, or one unbounded scan pinning a server. Every query has a row cap and a
deadline, and a truncated result says so.

**The wrong server is hard to reach.** A profile must be named explicitly —
there is no default — and every query prints the workspace, environment and
database it resolved to.

**Writes cannot get through.** Two independent layers, below.

If you can provision a `SELECT`-only database user, do that too: a grant is
stronger than any client-side check, because it holds for every client. This
tool is for the common case where you cannot — a shared application credential
you do not own — and for the three problems a grant does not solve at all.

## The read-only guarantee

**Layer 1 — the statement is parsed.** Not prefix-matched, parsed, with the
TiDB MySQL parser. Only `SELECT`, set operations, read `WITH`, `SHOW`,
`DESCRIBE` and `EXPLAIN` node types are accepted, and only one statement per
call. That catches what a `^SELECT` check waves through:

```
SELECT * FROM t INTO OUTFILE '/tmp/x'    → writes to the server's filesystem
SELECT 1; DROP TABLE t                   → two statements, one of them fatal
SELECT * FROM t FOR UPDATE               → changes nothing, stalls every writer
```

**Layer 2 — the server refuses anyway.** Everything runs inside
`START TRANSACTION READ ONLY`. If layer 1 ever let something through, MySQL
rejects it with error 1792. The two layers share no code, and the integration
suite verifies layer 2 by deliberately bypassing layer 1.

## Install

**Homebrew:**

```bash
brew trust iamnikolie/tap   # Homebrew 6 refuses untrusted third-party taps
brew tap iamnikolie/tap
brew install iamnikolie/tap/svidoq
```

**Prebuilt binary** — from [Releases](https://github.com/iamnikolie/svidoq/releases):

```bash
tar xzf svidoq_*_darwin_arm64.tar.gz
sudo mv svidoq /usr/local/bin/
```

**With Go** (1.24+):

```bash
go install github.com/iamnikolie/svidoq@latest
```

## Setup

```bash
svidoq config init --config acme     # writes ~/.svidoq/acme/config.yaml
$EDITOR ~/.svidoq/acme/config.yaml
svidoq --config acme check           # does every schema answer?
```

A **profile** is one workspace — a job, a client, a machine. Inside it an
**environment** is a server and a **schema** is a database on that server, so
adding a schema is one line, not one line per environment:

```yaml
default_env: prod

environments:
  prod:
    host: db.internal
    user: readonly
    password_command: "op read op://work/db-prod/password"
    limit: 200        # default rows per query
    max_limit: 5000   # ceiling for --limit
    timeout: 30s
  staging:
    host: db-staging.internal
    user: readonly
    password: ${ACME_STAGING_PASSWORD}

schemas:
  billing:
    description: "invoices, payments"
  analytics:
    description: "event rollups"
    host: analytics-db.internal    # overrides only what differs
```

Nothing about your workspaces lives in this repository, and the password need
not live on disk: `password_command` reads it from a secret manager at query
time, `${VAR}` takes it from the environment, and `SVIDOQ_PASSWORD_<ENV>` is
the last fallback. The file is created 0600.

There is deliberately **no default profile**: `--config` (or `SVIDOQ_CONFIG`)
is required, so a command cannot quietly hit production because a stray DSN was
lying around.

## Use

```bash
svidoq --config acme profiles                        # what workspaces exist
svidoq --config acme envs                            # what environments
svidoq --config acme schemas                         # what databases, and where they point
svidoq --config acme check                           # what actually answers

svidoq --config acme tables billing --filter "invoice%"
svidoq --config acme describe billing invoices
svidoq --config acme query billing "SELECT COUNT(*) FROM invoices"
svidoq --config acme explain billing "SELECT * FROM invoices WHERE client_id = 7"

svidoq --config acme --env staging query billing "SELECT 1"
svidoq --config acme --quiet --format tsv query billing "SELECT id FROM invoices LIMIT 10"
```

Every query prints its resolved target to stderr and its data to stdout:

```
→ acme/prod/billing  readonly@tcp(db.internal:3306)/billing?parseTime=true
id  client  total
--  ------  ------
1   acme    100.50
2   globex  NULL
2 row(s) in 4ms
```

`--json` puts nothing but JSON on stdout — warnings, row counts and truncation
notices always go to stderr, so a pipe into `jq` is never corrupted by a
message.

Run `svidoq skill` for the full agent reference.

## Flags

| Flag | Description |
|---|---|
| `--config <name>` | **Required.** Profile (workspace); or `SVIDOQ_CONFIG` |
| `--env <name>` | Environment inside the profile (default: its `default_env`) |
| `--format table\|json\|csv\|tsv` | Output format (default `table`) |
| `--json` | Alias for `--format json` |
| `--limit N` | Row cap for this query, clamped at the environment's `max_limit` |
| `--timeout 10s` | Per-query deadline |
| `--quiet` | Suppress the target line and row count on stderr |

## Supported databases

MySQL and MariaDB. The driver sits behind an interface (`internal/datasource`),
so another engine is a new package rather than a rewrite — but only what is
actually tested is claimed here.

## Development

```bash
make test          # go test ./... — hermetic, no network, no database
make integration   # spins up MySQL in Docker and runs the live suite
make vet
make fmt
make build
```

The unit suite never touches a database. The integration suite is skipped
unless `SVIDOQ_TEST_DSN` is set, and it is the one that proves the second
layer: it runs writes through the transaction *deliberately bypassing the
validator* and asserts the server refuses them.

## Contributing

Issues and pull requests are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE) © Mykola Klitovchenko
