# Changegate

**Change control for AI agents.** Every change an agent makes, whether as a pull request or as a live action
against a real system, gets a verified effect plan, a reversibility class and a tested undo path. Approval is
routed by risk and environment instead of by default.

- **Lane B (direct):** agents act on live databases through Changegate's MCP server, MCP proxy or coding-agent
  hooks. Each write is previewed with a dry run that is always rolled back (exact rows, before/after values,
  locks, rewrites). It is decided by policy, applied by an executor that holds the credentials, logged on a session
  timeline, and undoable in one command.
- **Lane A (CI/CD):** migrations in a PR are rehearsed on a throwaway copy: forward, observe, roll back, then
  compare catalog and data checksums. The effect report is posted on the PR, and policy gates the merge or deploy.
- **Promotion** turns a direct-lane session into a migration and a PR, so Git stays the source of truth.

Postgres ships first. Status of the roadmap's M0 and M1 milestones: [docs/m0-m1-status.md](docs/m0-m1-status.md).

## Quick start: let an agent touch your database without fear

```sh
changegate connect app --dsn '${DATABASE_URL}' --env personal   # env class is bound to the connection
changegate install claude-code                                  # MCP server + PreToolUse hook
```

Or add the MCP server to any client:

```json
{ "mcpServers": { "changegate": { "command": "npx", "args": ["-y", "@changegate/cli", "mcp", "postgres", "--target", "app"] } } }
```

The agent gets `query`, `list_tables`, `propose_change`, `apply_change`, `undo_change`, `get_change_status`,
`list_session_changes` and `checkpoint`. From the terminal:

```console
$ changegate exec "UPDATE orders SET region = 'eu' WHERE id % 2 = 0"
plan p_5b1450d3811b · app (personal) · reversible · 500 rows · live-dry-run
#1 UPDATE public.orders — 500 rows
   ~ id=2  region: null → "eu"
   …
change c_0a34ec5d5bdc — verified · 500 rows · UPDATE 500

$ changegate log
SESSION s_179c629fc993  agent=claude-code  user=steve  env=personal  target=app
  #1  10:02  ALTER TABLE orders ADD COLUMN region text                reversible  verified
  #2  10:03  UPDATE orders SET region = 'eu' WHERE id % 2 = 0 (500)   reversible  verified
  #3  10:05  DELETE FROM orders WHERE total > 990 (10 rows)           reversible  verified

$ changegate undo 1 --yes        # dependency-aware: undoes #3 and #2 first
$ changegate rewind --yes        # undo the whole session
$ changegate promote --pr        # schema changes → migration files (up + generated down) → PR
```

How undo stays safe:

- **DML:** on Postgres 18+ statements are rewritten with `RETURNING OLD/NEW` for exact per-row images; older
  versions pre-select target rows. Apply aborts if the rows changed since the plan. Undo touches a row only if it
  still equals its after-image, and refuses with a conflict otherwise.
- **DDL:** `DROP TABLE`, `DROP COLUMN` and `TRUNCATE` are soft-dropped into `_changegate_trash`. Additive DDL is
  inverted from a catalog diff. Anything else is labelled `checkpoint-only` or `irreversible` and handled by policy.
- **Statements with side effects outside the transaction** (`pg_terminate_backend`, `dblink`, `DO` blocks) are
  never executed in a dry run.

Environment classes (`personal`, `shared-dev`, `staging`, `prod`, `prod-restricted`) set the defaults. In
`personal`, reversible changes are auto-approved. In `prod`, direct writes need an incident reference and
out-of-band approval (Slack via `changegate serve`, or `changegate approve` by someone other than the proposer).
Agents can never approve their own changes.

## Quick start: rehearse migrations in pull requests

```yaml
- uses: actions/checkout@v4
  with: { fetch-depth: 0 }
- uses: changegate/rehearse@v0
  with: { mode: report, synthetic-rows: 1000 }
```

Or locally:

```sh
changegate rehearse --base-ref origin/main --dsn postgres://postgres@localhost/postgres --synthetic 1000 --out evidence.json
changegate gate evidence.json          # exit 0 standard change, 2 review required, 3 denied
```

Detected frameworks: Flyway, Liquibase (formatted SQL), Atlas, Prisma, golang-migrate, goose and Sqitch run
directly. Rails, Django and Alembic run through `--up-cmd` / `--down-cmd`. Tiers: T0 schema replay, T1 synthetic
data (optionally scaled from production `reltuples`), and T2/T3 via `--from-database` (golden image or clone) or
`--neon`. Reports never contain row values. GitLab: [`integrations/gitlab-component`](integrations/gitlab-component).

## Policy

One CEL policy file covers both lanes (`changegate policy init` writes the built-in defaults as a starting point):

```yaml
rules:
  - name: prod-destructive-needs-dba
    when: change.env.startsWith("prod") && change.destructive
    decide: require
    approvers: ["group:dba", "group:sre"]
    quorum: 2
  - name: freeze
    when: calendar.freeze("q4-release")
    decide: deny
  - name: agent-stricter-than-human
    when: principal.kind == "agent" && change.reversibility in ["checkpoint-only", "irreversible"]
    decide: require
    approvers: ["owner:codeowners"]
```

Rules combine as deny > require > approve. A rule that fails to evaluate counts as `require`, never as a silent
pass.

## Demo and benchmark

- [`demo/replit`](demo/replit): *What would have saved the Replit database.* An agent deletes production data
  during a code freeze. Changegate shows the exact rows, denies it, and undoes the same mistake on a dev copy in
  one command.
- [`bench`](bench): agent unsafe-action benchmark v0 (scripted actions). Unsafe-action rate 100% → 0%, false-block
  rate 0%, median plan latency ~33 ms, median undo ~9 ms.

## Repository layout

```
proto/changegate/v1/   Change Record + Adapter service (protobuf); gen/ holds generated Go
spec/                  Human-readable spec and JSON Schema
cmd/changegate/        CLI
pkg/adapter/           Adapter SDK: types, plan hashing, go-plugin gRPC transport
conformance/           Adapter conformance suite
internal/engine        State machine;  internal/store  SQLite + hash-chained audit log
internal/policy        CEL policy;     internal/session sessions, dependency-aware undo, rewind, branches
internal/mcpserver     MCP server;     internal/mcpproxy  MCP proxy;  internal/hooks  agent hooks
internal/migrations    Framework detection and writers;  internal/lanea  report, gate, provenance, GitHub/GitLab
internal/approval      Slack approvals
adapters/postgres      Postgres adapter (classifier, lint, dry-run plans, undo, soft-drop, checkpoints, rehearsal)
adapters/toykv         Reference adapter for the conformance suite
integrations/          GitHub Action, GitLab component, Claude Code plugin
packaging/             npm (npx) and PyPI (uvx) launchers, Homebrew formula
demo/, bench/          Replit-style demo, unsafe-action benchmark
```

## Development

```sh
make pg            # start Postgres 18 (:55418) and 16 (:55416) in Docker for integration tests
make test          # go test ./...
make lint          # golangci-lint
make build         # bin/changegate
changegate adapter test postgres --dsn postgres://postgres:pg@localhost:55418/postgres   # conformance
```

`pg_query_go` uses cgo, so release binaries are built natively per platform (see `.github/workflows/release.yml`).

Licensed under Apache-2.0.
