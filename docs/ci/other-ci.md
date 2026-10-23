# Other CI systems (Jenkins, CircleCI, Buildkite, Azure Pipelines, …)

Every CI integration is a few CLI commands with documented exit codes, so any runner that can start Docker or
reach a scratch Postgres can use them.

## Migrations

```sh
# A scratch Postgres the job may CREATE DATABASE on:
docker run -d --name cw-pg -e POSTGRES_PASSWORD=pg -p 55432:5432 postgres:18-alpine
export CHANGEWARD_REHEARSAL_DSN="postgres://postgres:pg@localhost:55432/postgres?sslmode=disable"

changeward rehearse --base-ref origin/main --synthetic 1000 --env prod --out evidence.json > report.md
changeward gate evidence.json        # 0 approved · 2 review required · 3 denied · 1 rehearsal failed
```

- `report.md` is the Markdown report. Post it with your CI's PR-comment step.
- `changeward gate --json` prints the decision, approvers and reasons as JSON.
- `changeward report evidence.json` re-renders the report from the evidence.

Useful `rehearse` flags:

| Flag | Use |
| --- | --- |
| `--migrations-dir`, `--framework` | Override detection |
| `--up-cmd`, `--down-cmd` | Code-based frameworks (Rails, Django, Alembic) |
| `--scale-from DSN` | Size synthetic data from production row estimates (`reltuples` only, no values) |
| `--from-database NAME` | T2/T3: clone a masked golden image or a clone instead of replaying migrations |
| `--neon` | Rehearse on a Neon branch (`NEON_API_KEY`, `NEON_PROJECT_ID`, `NEON_BRANCH_ID`) |
| `--schema dump.sql`, `--seed seed.sql` | Start from a schema dump, or add seed data |

## Terraform and Kubernetes

```sh
changeward tf review --plan-json plan.json --env prod                    # 0 / 2 / 3
changeward k8s gate --manifests rendered/ --target cluster --executor URL # 0 only when approved
```

## Jenkins example

```groovy
stage('Changeward') {
  steps {
    sh '''
      docker run -d --rm --name cw-pg-$BUILD_ID -e POSTGRES_PASSWORD=pg -p 55432:5432 postgres:18-alpine
      export CHANGEWARD_REHEARSAL_DSN="postgres://postgres:pg@localhost:55432/postgres?sslmode=disable"
      changeward rehearse --base-ref origin/$CHANGE_TARGET --synthetic 1000 --out evidence.json > report.md
      changeward gate evidence.json --report-only
    '''
    archiveArtifacts artifacts: 'report.md,evidence.json'
  }
  post { always { sh 'docker rm -f cw-pg-$BUILD_ID || true' } }
}
```

Start with `--report-only` (always exit 0). Drop it when you want the stage to fail on "review required" or
"denied".
