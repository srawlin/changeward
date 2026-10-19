# GitHub Actions

The Changegate Action rehearses the database migrations a pull request adds, on a throwaway Postgres:

1. Replay the base branch's migrations, optionally with synthetic data.
2. Run each new migration forward, and observe the locks, table rewrites and rows touched.
3. Run its down migration.
4. Compare catalog and data checksums with the state before.

It then posts the effect report on the PR, sets a `changegate/rehearsal` commit status, and applies your policy.

**Verified on GitHub** (srawlin/changegate):

- PR #1 is an agent's migration that drops a column. The report flagged it irreversible and asked for code-owner
  review; the status was pending.
- PR #2 was opened by `changegate promote --pr` from an agent session. It was rehearsed as reversible and
  auto-approved.

## Rehearse migrations on every PR

`.github/workflows/changegate.yml`:

```yaml
name: changegate
on:
  pull_request:
  pull_request_review:            # an approval can satisfy "review required"
    types: [submitted, dismissed]
permissions:
  contents: read
  pull-requests: write            # report comment, read approvals
  statuses: write                 # changegate/rehearsal status
jobs:
  rehearse:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }  # needed to diff against the base branch
      - id: changegate
        uses: srawlin/changegate/integrations/github-action@main   # pin a release tag once published
        with:
          mode: report            # start here; switch to gate when you trust it
          synthetic-rows: 1000    # T1 rehearsal; 0 = schema only (T0)
      - run: echo "decision=${{ steps.changegate.outputs.decision }}"
```

Until the first release is published, add `version: source` to build Changegate in the job. That works once
the repository is public; while it's private, vendor the Action and use `version: local`. Changegate's own
self-test builds the checked-out repository with `version: local`.

The Action starts `postgres:<postgres-version>-alpine` in Docker on the runner, unless you pass `database-url`.

## Inputs and outputs

| Input | Default | Meaning |
| --- | --- | --- |
| `mode` | `report` | `report`: never fail. `gate`: fail unless approved, or review-required and satisfied by PR approvals. `deny-only`: fail only when denied or the rehearsal broke. |
| `synthetic-rows` | `1000` | Synthetic rows per table (T1). Generated from types and constraints, never sampled from production. |
| `environment` | `prod` | Environment class the change deploys to, for policy |
| `migrations-dir`, `framework` | detected | Override detection |
| `base-ref` | PR base | Migrations changed since this ref are rehearsed |
| `database-url` | — | Admin DSN of a scratch server (CREATE DATABASE allowed). Use it for T2/T3 on a self-hosted runner next to a masked golden image. |
| `postgres-version` | `18` | When the Action starts Postgres |
| `policy` | `changegate.policy.yaml` if present | Policy file |
| `up-cmd`, `down-cmd` | — | Rails, Django, Alembic: e.g. `bin/rails db:migrate` / `bin/rails db:rollback STEP={n}` |
| `comment` | `true` | Post or update the PR comment (one comment, updated in place) |
| `version` | `latest` | Release tag, `latest`, `source` or `local` |

Outputs:

- `decision`: `approve`, `require` or `deny`.
- `approvers`: comma-separated.
- `reviewed`: `true` when PR approvals satisfied a review-required decision.
- `evidence`: path to the evidence JSON.

**Frameworks detected:**

- Run directly: Flyway, Liquibase (formatted SQL), Atlas, Prisma, golang-migrate, goose and Sqitch.
- Run through `up-cmd`/`down-cmd`: Rails, Django and Alembic.

## What the report shows

```
## ❌ Changegate rehearsal: 1 migration(s) · not proven reversible (irreversible)
Rehearsed on T1 · synthetic data · Postgres 18 · golang-migrate `db/migrations` · 1 base migration(s) replayed
Provenance: agent-authored (claude-code)
Policy (prod): 👀 review required by owner:codeowners: change is destructive, not proven reversible, or failed rehearsal

| | Migration | Reversibility | Locks (existing tables) | Rows touched | Up / down | Findings |
| ❌ | 000002_drop_legacy_ref.up.sql | irreversible | AccessExclusive on public.invoices (500 rows) | 0 | 5ms / 1ms | 1 error, 1 warning |

- error: DROP COLUMN destroys the column's data and breaks code still reading it. Expand/contract: …
- warning: CREATE INDEX without CONCURRENTLY blocks writes to the table for the whole build. …
```

The report contains counts, checksums and lint findings. It never contains row values.

**Provenance:** commits from agents are detected from bot authors, `Co-Authored-By` trailers and branch names
(`changegate detect`), so policy can treat agent PRs more strictly.

## Making it a merge gate

1. Run in `mode: report` for a while and read the reports.
2. Switch to `mode: gate`, or keep `report` and rely on the status.
3. In branch protection (or a ruleset), **require the `changegate/rehearsal` status**.
4. For `owner:codeowners` approvers, add a `CODEOWNERS` entry for your migrations directory and enable
   **Require review from Code Owners**.

Then:

- **Auto-approved** (reversible, rehearsed, non-destructive): the status is green and the PR can merge.
- **Review required:** the status is pending, with the reason. When someone other than the author approves the
  current head commit, the `pull_request_review` run turns it green. Named `user:` approvers must approve
  themselves, and `quorum: 2` needs two people. GitHub enforces *which* people count for `owner:codeowners` and
  `group:` approvers. A new push needs a new approval.
- **Denied** (e.g. a freeze window, or a forbidden operation): the status is red.

## Terraform plans and Kubernetes manifests in the same workflow

```yaml
  terraform:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: hashicorp/setup-terraform@v3
      - run: terraform init -input=false && terraform plan -out tf.plan -input=false && terraform show -json tf.plan > plan.json
      - run: changegate tf review --plan-json plan.json --env prod --github   # 0 ok, 2 review, 3 denied
        env: { GITHUB_TOKEN: "${{ github.token }}" }
```

See [terraform.md](terraform.md) and [kubernetes-gitops.md](kubernetes-gitops.md) for `changegate k8s gate`.

## From an agent session to a PR

When an agent changed a dev database directly, run `changegate promote --pr` (needs `git` and `gh`). It writes
migration files for your framework, with the down generated from the recorded undo data, then opens a PR with
the session timeline. This workflow then rehearses it like any other PR.
