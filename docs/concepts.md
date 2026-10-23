# Concepts

## The change lifecycle

Every change, whatever the system, follows the same state machine:

```
proposed → planned → rehearsed → (auto-approved | awaiting-approval → approved | rejected)
        → applying → applied → verifying → verified → (undoing → undone)
```

- **Plan:** the adapter works out the exact effect without changing anything.
  - Postgres and MySQL: the statement runs in a transaction that is always rolled back. That captures the rows,
    before/after values, locks taken and tables rewritten.
  - Kubernetes: a server-side dry run against the live objects.
  - Terraform: a plan file.
  - Config files: validated (e.g. `nginx -t`) without a reload.
  - REST APIs: the manifest's `before` read.
- **Decide:** policy turns the plan into `approve`, `require` (approval needed, with a channel and approvers)
  or `deny`.
- **Apply:** only the planned operation is executed, and only if the target still matches the plan. Postgres
  checks that the rows haven't changed; Kubernetes, the desired-state hash; Terraform, the exact plan file.
  Otherwise the apply stops with a *conflict* and you plan again.
- **Verify:** an adapter-specific check that the effect matches the plan: the same rows, an empty Terraform
  re-plan, a healthy rollout, passing health checks.
- **Undo:** the recorded inverse is applied, guarded the same way. It touches only what is still as the change
  left it.

Each change has a **plan ID** (`p_…`) and a **change ID** (`c_…`), and a step number (`#3`) in its session.
Commands accept any of them.

## Two lanes

| | Direct lane | CI lane |
| --- | --- | --- |
| Who | An agent (or a person) acting on a live system | A pull request |
| Entry points | MCP server, `changeward exec`, hooks, SDKs, executor, gateways | `rehearse` + `gate`, `tf review`, `k8s gate`, GitHub Action, GitLab component |
| Evidence | A live dry run against the target | A rehearsal on a throwaway copy, or a dry run against the cluster |
| Result | Applied and undoable, on a session timeline | A PR comment, a commit status, a merge or deploy gate |

The same policy file governs both; rules can check `change.lane`.

### Promotion

Direct changes in dev shouldn't stay only in dev. `changeward promote` turns a session's applied changes into
migration files for your framework. The *up* is the SQL as run. The *down* is generated from the recorded undo
data, so the rollback is one that was actually exercised.

With `--pr`, it opens a pull request carrying the session timeline, and the CI lane then rehearses it.
Kubernetes changes are promoted to manifests (`--k8s-dir`). For changes made directly to Git-managed systems
during an incident, `changeward reconcile` opens the matching PR. `changeward drift` reports any that are still
unreconciled.

## Environment classes

A target's class is set when you `connect` it, and an agent can never change it:

| Class | Built-in default for direct writes |
| --- | --- |
| `personal` | Reversible changes, or checkpoint-only changes with a checkpoint taken, are auto-approved. Anything else asks you inline, in the agent's UI. |
| `shared-dev` | Auto-approved if reversible, non-destructive and under 1,000 rows; otherwise you confirm. |
| `staging` | Auto-approved if reversible, non-destructive and under 10,000 rows; otherwise a second person approves, out of band. |
| `prod` | Needs an incident or ticket reference, plus out-of-band approval by someone other than the proposer. |
| `prod-restricted` | Read-only in the direct lane. |

In CI: reversible, rehearsed and non-destructive changes are auto-approved as standard changes; anything else
needs review by code owners. Your own rules take precedence ([policy.md](policy.md)).

## Reversibility classes

| Class | Meaning | Examples |
| --- | --- | --- |
| `reversible` | Exact undo from recorded state | Postgres DML (row images); additive DDL; soft-dropped `DROP TABLE`/`DROP COLUMN`/`TRUNCATE`; Kubernetes object changes; a config-file change |
| `compensatable` | An inverse operation exists, but it isn't byte-for-byte | Closing a GitHub issue to undo creating it; re-creating a deleted resource under a new id |
| `checkpoint-only` | Undo needs a checkpoint of the whole target | `ALTER TYPE`, column type changes, `INSERT … SELECT` on MySQL |
| `irreversible` | Nothing can bring it back | Sending email, `terraform destroy` of a database, an unmatched API call |

Policy sees the class, so "irreversible in prod" can be denied or sent to a DBA.

## Sessions

A session groups an agent's work on one target, with who (agent and delegating human), when, and why
(`--incident`). The MCP server starts one automatically. On the command line, use `changeward session start`.

- `changeward log`: the timeline.
- `changeward undo STEP`: undo a step. Later steps that touched the same objects are undone first, or listed if
  you don't pass `--yes`.
- `changeward rewind`: undo the whole session, newest first.

## Checkpoints

Checkpoints make `checkpoint-only` changes safe. They are taken with `session start --checkpoint`,
`changeward checkpoint`, or the agent's `checkpoint` tool, and restored with `changeward restore`.

| Postgres strategy | How |
| --- | --- |
| `template` (local) | `CREATE DATABASE … TEMPLATE` |
| `dump` | `pg_dump` / `pg_restore` |
| Neon | A branch at the current LSN (`neon_project_id`, `neon_api_key`); restore keeps an ancestor branch |

`changeward branch create` points a session at a private copy of the target (a Neon branch, or a template copy)
so an agent can experiment freely. `changeward branch discard` throws the copy away.

## Approvals

- **Inline:** confirmed by the delegating human in the agent's own UI, through MCP elicitation. Allowed only
  where policy says `channel: inline` (personal and shared-dev defaults).
- **Out of band:** `changeward approve PLAN_ID`, with `--executor` on a shared executor, Slack buttons through
  `changeward serve`, or the web view. The approver must be someone other than the proposer.
- An approval binds to the **plan hash**. If the data or the change moves after approval, the change must be
  planned again and approved again.
- Agents can't approve. The hooks block `changeward approve` from agent shells, and the executor's roles keep
  `propose` and `approve` separate.

## Changesets

`changeward changeset plan -f changes.yaml` plans ordered steps across systems, for example a migration and a
Kubernetes rollout. `changeset apply` runs them as a saga: if a step fails, the earlier steps are undone in
reverse order. `changeset undo` reverts the whole set.

## Audit log

Every event goes to an append-only, hash-chained log: proposals, plans, decisions, approvals, applies, undos,
blocked hook attempts and gateway decisions. `changeward audit verify` checks the chain.

With `telemetry.otlp_endpoint` set, each change is also exported as an OpenTelemetry trace, with a span per
state. Attributes only, never row values.

## What Changeward never does

- It never sends row values anywhere. Plans shown to agents redact `sensitive` columns, and production plans hide
  values. CI reports contain counts and checksums only.
- It never runs a statement with outside effects while planning.
- It never lets an approval carry over to a different plan.
