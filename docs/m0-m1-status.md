# M0 and M1 status

Status of the roadmap in [`agent-change-control-plan.md`](agent-change-control-plan.md) §14 as of 2026-10-18.
"Tested" means covered by automated tests that run against real Postgres 16 and 18 in Docker (`make pg && make
test`), unless stated otherwise.

## M0: Foundations — done

| Scope item | Status | Where |
| --- | --- | --- |
| Proto and spec v0 | Done | `proto/changegate/v1`, `spec/README.md`, `spec/schemas/change-record.schema.json` (engine output validated against it in tests) |
| Engine state machine | Done, tested | `internal/engine`: lifecycle, plan-hash binding, quorum, separation of duties, expiry, drift → failed, guarded undo, auto-rollback on failed verification |
| SQLite store | Done, tested | `internal/store`: WAL, optimistic state transitions, append-only events (DB triggers) with a SHA-256 hash chain; `changegate audit verify` |
| CEL policy | Done, tested | `internal/policy`: user rules plus built-in environment defaults, freezes (`calendar.freeze`), quorum, channels; evaluation errors fail safe |
| CLI skeleton | Done | `cmd/changegate` (grew into the full M1 CLI) |
| Conformance harness | Done, tested | `conformance/`: plan purity and hashing, per-operation and sequence apply/undo round trips, stale-plan and stale-undo guards, checkpoint/restore; runs in process and over the gRPC plugin transport |
| Postgres statement classifier | Done, tested | `adapters/postgres/classify.go`, `lint.go` (pg_query_go; Squawk-style findings) |

**Exit criteria:** `changegate plan -f file.sql` works on SQL files (static analysis without a database). Conformance
runs on the toy adapter, in process and as a plugin binary (`changegate adapter test toykv`). Both met.

## M1: Postgres, both lanes

### Lane B (direct)

| Scope item | Status | Notes |
| --- | --- | --- |
| Live DML plan/apply/undo, PG18 `RETURNING OLD/NEW` plus fallback | Done, tested | PG18: exact images for INSERT/UPDATE/DELETE/MERGE/upsert/multi-table. PG<18: pre-select for simple single-table statements with a primary key; otherwise `checkpoint-only`. Drift detection; guarded set-based undo. Cascading FKs, triggers and foreign tables lower the class. Conformance passes on generated corpora (3 seeds × 2 versions). |
| Soft-drop DDL | Done, tested | DROP TABLE/VIEW/SEQUENCE (indexes and owned sequences renamed), DROP COLUMN (renamed, made nullable), TRUNCATE (rows copied). Plus inverses for additive DDL (catalog diff), renames, defaults, NOT NULL, type changes (`compensatable`), constraints, indexes, functions, triggers, GRANT/REVOKE. `changegate trash list/purge`. |
| Local MCP server | Done, tested | `changegate mcp postgres`; correct tool annotations; tested in memory and over real stdio |
| MCP proxy | Done, tested | `changegate proxy -- <server>`; tested against a fake upstream (reads pass through, SQL writes applied by Changegate, unknown write tools gated and logged) |
| Claude Code plugin and hooks | Done; hook tested | `changegate hook claude-code`, `changegate install claude-code`, `integrations/claude-code` plugin plus marketplace manifest. **Not verified by loading the plugin in Claude Code.** |
| Sessions: `log` / `undo` / `rewind` | Done, tested | Dependency-aware undo; rewind with checkpoint fallback |
| Branch-as-working-target (template DB, Neon) | Template: done, tested. Neon: implemented, **untested** | No Neon API key was available. Template branching needs no other connections on the source (`allow_terminate` for personal targets). |
| `promote` to migration files for 3 frameworks | Done for 10 | golang-migrate, goose, Flyway, Liquibase SQL, Atlas, Prisma, Sqitch, Rails, Django, Alembic. All writers are tested by writing and then re-detecting the result. End to end (session → promote → Lane A rehearsal) was run manually for golang-migrate. `--pr` uses git and gh but was **not run against GitHub**. |
| Approvals: terminal, MCP elicitation, Slack | Done, tested | Elicitation uses MCP multi-round-trip input requests (protocol 2026-07-28; the SDK falls back for older clients). Slack: signed interactions and user mapping, tested against a fake Slack API. **Not tested against real Slack.** |

### Lane A (CI/CD)

| Scope item | Status | Notes |
| --- | --- | --- |
| Migration detection: Flyway, Liquibase, Atlas, Prisma, Rails, Django, Alembic, golang-migrate | Done, tested | Also goose and Sqitch. Liquibase XML/YAML changelogs are detected, but rehearsal needs the Liquibase CLI (command mode). |
| Rehearsal on template DB / Neon branch | Template and T0/T1: done, tested. Neon: implemented, **untested** | Per-migration forward → locks/rewrites/rows → down → catalog and data checksum compare; re-apply check; command mode for code frameworks (tested with psql standing in). Synthetic data generator (T1) respects types, NOT NULL and FKs, and scales from production `reltuples`. |
| PR report | Done | Markdown with no row values; agent provenance (bots, trailers, branches) |
| GitHub Action | Written, **not run on GitHub** | `integrations/github-action/action.yml` (composite). Comment and status posting is implemented against the REST API but untested against github.com. Needs a published release to download. |
| GitLab component | Written, **not run on GitLab** | `integrations/gitlab-component/templates/rehearse.yml` |

### Packaging, demo, benchmark

| Scope item | Status | Notes |
| --- | --- | --- |
| npx / uvx launchers | Done, tested locally | Download from the GitHub release, verify SHA-256, cache, exec. Tested against a local "release", including a tampered checksum. **Not published** (needs npm/PyPI accounts; names unclaimed). |
| Homebrew | Formula template | Filled in by the release workflow |
| OCI image | Done | `Dockerfile` (Alpine, cgo/musl) builds and runs locally |
| Release workflow | Written, **not run** | Native per-platform builds (cgo), checksums, cosign keyless signing, npm, PyPI, tap, GHCR |
| Replit-style demo | Done, run | `demo/replit/run.sh` |
| Benchmark v0 | Done, run | `bench/`: scripted actions, not real agents (v1). Results are in `bench/README.md`. |

### Exit criteria (not engineering deliverables)

Launch, external issues/PRs, weekly active sessions, repositories running the Action, and design partners all
need a public release and outreach. None of that has happened. Before launch, the plan's trademark and namespace
check for the name "Changegate" still needs doing. The GitHub org, npm scope `@changegate`, PyPI name and Homebrew
tap used here are placeholders.

## Known gaps and follow-ups

- **Lane A deploy-time re-check (§5.3.4):** the gate does not yet confirm that production's schema version still
  matches the rehearsed base. Server-mode deployment protection rules are M3.
- **Lane B `branch_above_rows` (§5.3.2):** large or risky statements are not yet automatically moved to a branch.
  Large DML above the image cap is reported as `checkpoint-only`.
- **Command-mode rehearsal** (Rails/Django/Alembic) compares schema only, not data. A down command that changes
  nothing is reported as unconfirmed (`checkpoint-only`).
- **Sequences** advance during dry runs, and undo does not reset them (a documented caveat).
- **`json` (not `jsonb`) columns** are restored semantically; whitespace may be normalized.
- **Hooks** treat `kubectl`, `terraform`, `helm` and nginx changes as "ask". Their adapters are M2.
- **Postgres store for server mode** is M3. The `Store` interface is ready for it.
