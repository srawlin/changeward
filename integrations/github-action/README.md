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
      - uses: changegate/rehearse@v0
        with:
          mode: report          # start with report; switch to gate when you trust it
          synthetic-rows: 1000  # T1; set 0 for schema-only T0
```

Rehearsing at T2/T3 (masked golden image or production clone) needs a self-hosted runner inside the network where
the data lives; pass `database-url` pointing at that server. Reports never contain row values.

Detected frameworks: Flyway, Liquibase (formatted SQL), Atlas, Prisma, golang-migrate, goose, Sqitch (run directly);
Rails, Django, Alembic (pass `up-cmd` / `down-cmd`, e.g. `bin/rails db:migrate` / `bin/rails db:rollback STEP={n}`).

The Marketplace listing is published from the `changegate/rehearse` repository, which mirrors this directory.
