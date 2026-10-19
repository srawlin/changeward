# Changegate

**Let AI agents change real systems, safely.** Changegate sits between your agents and your databases,
clusters and infrastructure. Every change an agent wants to make is previewed exactly, checked against your
policy, applied by Changegate, and kept undoable. That covers coding agents in your terminal, agents in your
product, and pull requests in CI.

```console
$ changegate exec "DELETE FROM orders WHERE total > 990"
plan p_5b14 · app (personal) · reversible · 10 rows · live-dry-run
#1 DELETE public.orders — 10 rows
   - id=17 total=999.00 customer_id=4 …
change c_0a34 — verified · 10 rows · DELETE 10

$ changegate undo 1 --yes
undone c_0a34  DELETE FROM orders WHERE total > 990
```

> *Changegate* is a working name. See [docs/renaming.md](docs/renaming.md).

## Why Changegate

Agents are good at writing changes, but nobody can see the effects until after they run. A `DELETE` without a
`WHERE`, a migration that rewrites a 50 GB table, a `kubectl apply` over a hand-fixed production object, a
`terraform apply` that replaces a database: all of these look fine in a chat window.

| Without Changegate | With Changegate |
| --- | --- |
| The agent runs `psql`, `kubectl` or `terraform` directly with your credentials. | The agent calls `propose_change`. Changegate holds the credentials and applies only what was approved. |
| You see what happened afterwards, if you look. | You see the **exact effect first**: rows and before/after values, locks, table rewrites, objects created, changed or destroyed. |
| "Undo" means restoring a backup. | Every change gets a **tested undo path**: row images, soft-dropped tables, previous manifests, the last-applied Terraform config. The undo refuses if someone changed the data since. |
| Approval is all or nothing: the agent asks for every command, or for none. | **Policy routes by risk and environment.** Reversible changes in dev are auto-approved. Production writes need an incident reference and a second person. The agent can never approve its own change. |
| Migrations in PRs are reviewed by reading SQL. | Migrations are **rehearsed on a throwaway copy**: forward, observe, roll back, compare. The PR gets an effect report and a merge gate. |
| The audit trail is scattered shell history. | Every session, plan, decision, approval and undo is in a **hash-chained audit log**, and can be exported to OpenTelemetry. |

## How it works

```
 agent ──▶ propose_change ──▶ plan (dry run, always rolled back) ──▶ policy ──▶ approve? ──▶ apply ──▶ verify
                                    │                                   │                       │
                                    ▼                                   ▼                       ▼
                         exact effects + undo path         auto / you / a 2nd person       undo any time
```

Two lanes share one engine and one policy:

- **Direct lane:** agents act on live systems through Changegate's MCP server, coding-agent hooks, SDKs or a
  gateway. Each write becomes plan → decision → apply → verify, on a session timeline you can undo step by step.
- **CI lane:** migrations, Kubernetes manifests and Terraform plans in a PR are rehearsed or dry-run. The report
  is posted on the PR, and policy gates the merge or deploy.

`changegate promote --pr` turns what an agent did directly in dev into migration files and a pull request, so
Git stays the source of truth.

**Supported systems:**

| System | What you get |
| --- | --- |
| **Postgres** (16 to 18, Neon) | Exact dry runs and row-level undo. Soft-drop for `DROP`/`TRUNCATE`. Checkpoints: template DB, dump or Neon branch. |
| **MySQL** 8 | The same guarantees for data changes; table-level checks for schema changes. |
| **Kubernetes** | Server-side dry-run diffs and exact undo; Argo CD and Flux ownership flagged. |
| **Terraform / OpenTofu** | Only the exact reviewed plan is applied. Destroying stateful resources is irreversible. |
| **NGINX, Traefik, HAProxy** config | Validated before reload, health-checked after, rolled back on failure. |
| **REST APIs** | Undo calls declared per operation in a YAML manifest (e.g. GitHub issues). |

## Quick start (5 minutes)

**1. Install** (pre-release, from source; needs Go 1.27+ and a C compiler):

```sh
git clone https://github.com/srawlin/changegate && cd changegate && make build   # → bin/changegate
export PATH="$PWD/bin:$PATH"
```

Homebrew, `npx @changegate/cli` and `uvx changegate` packages come with the first release
([details](docs/getting-started.md#install)).

**2. Connect a database.** Its environment class decides the default policy:

```sh
changegate connect app --dsn 'postgres://localhost/myapp_dev' --env personal
```

**3. Plug it into your coding agent:**

```sh
changegate install claude-code     # or: codex, opencode, gemini, cursor, agents-md
```

This adds the Changegate MCP server: `query`, `list_tables`, `propose_change`, `apply_change`,
`undo_change`, `get_change_status`, `list_session_changes` and `checkpoint`. It also adds a hook that blocks the
agent's direct writes (`psql`, `kubectl apply`, `terraform apply`, …) and tells it to use those tools.

**4. Ask your agent for a change,** e.g. "archive customers inactive for two years", then look at what happened:

```sh
changegate log                 # the session timeline: every change, its reversibility and state
changegate undo 2 --yes        # undo step 2 (later steps that depend on it are undone first)
changegate rewind --yes        # undo the whole session
changegate ui                  # the same in a local web view
```

The [getting-started guide](docs/getting-started.md) walks through this with a sample database in Docker.

## Integrations

### Coding agents

| Agent | Setup | What is installed | Guide |
| --- | --- | --- | --- |
| **Claude Code** | `changegate install claude-code` | `.mcp.json` and a PreToolUse hook in `.claude/settings.json` | [docs/agents/claude-code.md](docs/agents/claude-code.md) |
| **OpenAI Codex CLI** | `changegate install codex` | `.codex/config.toml`: MCP server and PreToolUse hook | [docs/agents/codex.md](docs/agents/codex.md) |
| **OpenCode** | `changegate install opencode` | `opencode.json` and the `.opencode/plugins/changegate.ts` plugin | [docs/agents/opencode.md](docs/agents/opencode.md) |
| **Gemini CLI** | `changegate install gemini` | `.gemini/settings.json`: MCP server and BeforeTool hook | [docs/agents/gemini-cli.md](docs/agents/gemini-cli.md) |
| **Cursor** | `changegate install cursor` | `.cursor/mcp.json` and a `beforeShellExecution` hook | [docs/agents/cursor.md](docs/agents/cursor.md) |
| **Any MCP client** (Claude Desktop, VS Code, Windsurf, Zed, …) | one JSON snippet | MCP server only | [docs/agents/mcp-clients.md](docs/agents/mcp-clients.md) |
| **Your own agents** (LangGraph, OpenAI Agents SDK, Claude Agent SDK, Vercel AI SDK) | Python / TypeScript SDK | client for a shared executor | [docs/agents/sdks.md](docs/agents/sdks.md) |

Claude Code, Codex, OpenCode and Gemini CLI were tested end to end with each real CLI. The agent used
`propose_change` → `apply_change` by itself, and a forced raw `psql DELETE` was blocked by the hook.

### CI/CD and GitOps

| Pipeline | What it does | Guide |
| --- | --- | --- |
| **GitHub Actions** | Rehearses migrations in PRs; posts a report comment and a `changegate/rehearsal` status; report or gate mode | [docs/ci/github-actions.md](docs/ci/github-actions.md) |
| **GitLab CI/CD** | The same, as a CI/CD component, with MR notes and commit statuses | [docs/ci/gitlab.md](docs/ci/gitlab.md) |
| **Terraform / OpenTofu** in any CI | `changegate tf review` classifies the plan and gates on policy | [docs/ci/terraform.md](docs/ci/terraform.md) |
| **Kubernetes, Argo CD, Flux** | `changegate k8s gate` as a PR check or an Argo CD PreSync hook: a sync waits until its exact plan is approved | [docs/ci/kubernetes-gitops.md](docs/ci/kubernetes-gitops.md) |
| **Any other CI** (Jenkins, CircleCI, Buildkite, …) | The `rehearse`, `gate`, `tf review` and `k8s gate` CLI commands, with exit codes | [docs/ci/other-ci.md](docs/ci/other-ci.md) |

### Shared executor and gateways

For teams, run `changegate executor serve` next to your systems (Helm chart or systemd). It holds the
credentials, enforces policy and keeps the audit log. Agents, SDKs and gates call it with per-person tokens.
MCP gateways (**Envoy**, **agentgateway**, **IBM ContextForge**) can ask it to authorize every tool call. See
[docs/executor.md](docs/executor.md) and [docs/gateways.md](docs/gateways.md).

## Safety model in brief

- **Environment classes** (`personal`, `shared-dev`, `staging`, `prod`, `prod-restricted`) are bound to the
  connection, never chosen by the agent.
- **Reversibility classes:**
  - `reversible`: exact undo.
  - `compensatable`: an inverse call exists.
  - `checkpoint-only`: needs a checkpoint to restore.
  - `irreversible`: the policy sees this.
- **Approvals bind to the plan hash.** If the data or the change moves after approval, it must be planned
  again.
- **Separation of duties:** out-of-band approvals must come from someone other than the proposer. Agents can't
  approve.
- **Dry runs never leak side effects.** Data changes run in a transaction that is always rolled back.
  Statements with effects outside it (`dblink`, `pg_terminate_backend`, `DO` blocks) are never executed while
  planning.
- **Fail closed.** A policy rule that errors counts as "require approval", and gateways deny when Changegate is
  unreachable.

The details are in [docs/concepts.md](docs/concepts.md) and [docs/policy.md](docs/policy.md).

## Documentation

| | |
| --- | --- |
| [Getting started](docs/getting-started.md) | Install, first target, first agent session, undo |
| [Concepts](docs/concepts.md) | Lanes, plans, environment and reversibility classes, sessions, checkpoints, approvals, audit |
| [Configuration](docs/configuration.md) | `changegate.yaml` and target options for every adapter |
| [Policy](docs/policy.md) | CEL rules, built-in defaults, approvals, Slack, Jev signals |
| [Coding agents](docs/agents/) | Claude Code, Codex, OpenCode, Gemini CLI, Cursor, other MCP clients, SDKs |
| [CI/CD](docs/ci/) | GitHub Actions, GitLab, Terraform, Kubernetes/GitOps, other CI |
| [Shared executor](docs/executor.md) and [gateways](docs/gateways.md) | Team deployment, tokens, Helm, Envoy/agentgateway/ContextForge |
| [CLI reference](docs/cli.md) | Every command and flag |
| [Status](docs/m2-status.md) | What is done and verified, and the known gaps |

## Demo and benchmark

- [`demo/replit`](demo/replit), *what would have saved the Replit database*: an agent deletes production data
  during a code freeze. Changegate shows the exact rows, denies the change, and undoes the same mistake on a dev
  copy in one command.
- [`bench`](bench), agent unsafe-action benchmark v0: the unsafe-action rate drops from 100% to 0% with no false
  blocks. Median plan latency is about 33 ms; median undo is about 9 ms.

## Development

```sh
make pg     # Postgres 18 (:55418) and 16 (:55416) in Docker for integration tests
make test   # go test ./...
make lint   # golangci-lint
make build  # bin/changegate
```

The code map is in [docs/development.md](docs/development.md). Licensed under Apache-2.0.
