# Changegate GitHub Action

Rehearses the database migrations a pull request adds — forward, observe locks/rewrites/rows, run the down
migration, compare catalog and data checksums — and posts an effect report on the PR. Policy turns the evidence
into a decision: auto-approve (standard change), review by named approvers, or deny.

```yaml
# .github/workflows/changegate.yml
name: changegate
on: pull_request
permissions:
  contents: read
  pull-requests: write
  statuses: write
jobs:
  rehearse:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: srawlin/changegate/integrations/github-action@main   # pin a release tag once published
        with:
          mode: report          # start with report; switch to gate when you trust it
          synthetic-rows: 1000  # T1; set 0 for schema-only T0
```

Rehearsing at T2/T3 (masked golden image or production clone) needs a self-hosted runner inside the network where
the data lives; pass `database-url` pointing at that server. Reports never contain row values.

Detected frameworks: Flyway, Liquibase (formatted SQL), Atlas, Prisma, golang-migrate, goose, Sqitch (run directly);
Rails, Django, Alembic (pass `up-cmd` / `down-cmd`, e.g. `bin/rails db:migrate` / `bin/rails db:rollback STEP={n}`).

Full guide: [docs/ci/github-actions.md](../../docs/ci/github-actions.md).
