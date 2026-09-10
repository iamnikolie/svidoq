# Security Policy

## Supported versions

The latest release is the supported one. Fixes land on `main` and go out in the
next tag.

## Reporting a vulnerability

Please do **not** open a public issue for a security problem.

Use GitHub's private vulnerability reporting:
[Security → Report a vulnerability](https://github.com/iamnikolie/svidoq/security/advisories/new).

**A statement that reaches the database and modifies data is the vulnerability
this project cares about most.** If you find a way past either layer — a
statement the parser accepts that writes, or a path to the server that skips
the read-only transaction — that is a report worth making, and a proof-of-
concept statement is enough; no exploit chain needed.

Expect a first response within a week. This is a spare-time project, not a
product with an on-call rotation.

## Scope notes

Known and by design rather than vulnerabilities:

- **The tool is not a substitute for a read-only grant.** Where you can give
  the agent a `SELECT`-only database user, do: a grant is enforced by the
  server for every client, and cannot be bypassed by any bug in this code. Both
  layers here exist for the case where you cannot.
- **Credentials live in the profile.** `~/.svidoq/<profile>/config.yaml` is
  created 0600, and `password_command` keeps the secret out of the file
  entirely, but anyone who can read your home directory can read whatever is
  there and run the same commands you can.
- **`password_command` runs a shell command** from your own config file. It is
  as trusted as the file, which is as trusted as your account.
- **Query results are printed as they arrive.** Content is not sanitized for
  the terminal, and database content is written by other people.
- **The target line and errors go to stderr and may name hosts, schemas and
  users.** Redact before pasting them anywhere public.
