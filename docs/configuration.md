# Configuration

Changegate reads `changegate.yaml` from `$CHANGEGATE_CONFIG`, or from the working directory or the nearest parent
that has one. `changegate connect` creates and updates it. Values support `${VAR}` expansion, so keep secrets in
the environment.

```yaml
targets:
  app:
    adapter: postgres
    env: personal                       # personal | shared-dev | staging | prod | prod-restricted
    dsn: ${APP_DATABASE_URL}
    sensitive: ["users.email", "*.password", "payments.card_last4"]
    options:
      checkpoint_strategy: template
default_target: app

policy: changegate.policy.yaml          # default: changegate.policy.yaml next to this file, else built-ins
store: .changegate/changegate.db        # audit log and change records (SQLite); $CHANGEGATE_STORE
plan_ttl: 15m                           # plans older than this must be re-planned

approvals:
  slack:
    bot_token: ${SLACK_BOT_TOKEN}       # interactive Approve/Reject (needs `changegate serve`)
    channel: "#db-changes"
    signing_secret: ${SLACK_SIGNING_SECRET}
    users: { U012ABC: "user:alice" }    # Slack user → Changegate identity (for separation of duties)
    # webhook_url: ${SLACK_WEBHOOK}     # notifications only

signals:
  jev:
    enabled: false                      # see policy.md: advisory, can only tighten decisions

telemetry:
  otlp_endpoint: http://otel-collector:4318   # or $OTEL_EXPORTER_OTLP_ENDPOINT
  headers: { authorization: "Bearer ${OTEL_TOKEN}" }

executor:                               # only for `changegate executor serve`
  tokens:
    - { name: alice, sha256: "…", roles: [propose] }

adapters:                               # out-of-process adapters (go-plugin gRPC)
  mongo: { path: /usr/local/bin/changegate-adapter-mongo }
```

Use `changegate targets` to list targets, and `changegate connect NAME …` to add or replace one:

```sh
changegate connect NAME --adapter ADAPTER --env CLASS [--dsn DSN] [--option key=value …] [--sensitive t.c,…] [--default]
```

## Target options by adapter

### Postgres (`--adapter postgres`, the default)

| Option | Meaning |
| --- | --- |
| `dsn` | Connection string. Use a role that can read the catalog. Checkpoints need `CREATEDB`. |
| `checkpoint_strategy` | `template` (default; `CREATE DATABASE … TEMPLATE`, needs no other connections), `dump` (`pg_dump`/`pg_restore`), or `neon` |
| `pg_dump`, `pg_restore` | Binary paths; with `pg_dump` set, `template` falls back to a dump |
| `maintenance_db` | Database to connect to for checkpoint and restore (default `postgres`) |
| `allow_terminate` | `true` lets a `personal` target terminate other connections for a template checkpoint |
| `copy_from` | Source for `branch create` copies (e.g. a masked golden image) |
| `neon_project_id`, `neon_branch_id`, `neon_api_key` | Neon branching for checkpoints and branches (`neon_api_key` defaults to `$NEON_API_KEY`) |
| `neon_checkpoint` | `lsn` (default; restore to a point in time) or `branch` |
| `neon_preserve_on_restore` | `false` deletes the branch being replaced on restore |
| `show_values` | `true` shows row values in `shared-dev` plans. By default only `personal` shows values. Shared-dev shows keys only; staging and prod show hashed keys. |

### MySQL (`--adapter mysql`)

`dsn` in the Go MySQL driver's format, e.g. `user:pass@tcp(localhost:3306)/app`. DML is planned in a
rolled-back InnoDB transaction. DDL is analysed statically, and `DROP` is soft-dropped.

### Kubernetes (`--adapter kubernetes`)

| Option | Meaning |
| --- | --- |
| `context`, `kubeconfig` | Which cluster. Inside a pod, the service account is used. |
| `namespace` | Default namespace for manifests without one |
| `kubectl` | Binary path |
| `rollout_timeout` | How long verify waits for rollouts (e.g. `5m`) |

The operation payload is YAML manifests: `changegate exec --target cluster -f deploy.yaml`.

### Terraform / OpenTofu (`--adapter terraform`)

| Option | Meaning |
| --- | --- |
| `dir` | Working directory |
| `binary` | `terraform` (default) or `tofu` |
| `var_files` | Comma-separated `-var-file`s |
| `stateful_types` | Extra resource types whose destruction loses data |

The payload is `apply` (default) or `destroy`.

### Config files (`--adapter configfile`)

| Option | Meaning |
| --- | --- |
| `path` | The managed file |
| `preset` | `nginx`, `traefik` or `haproxy`: default test and reload commands |
| `test_cmd` | Validates the candidate, without reloading (may use `{file}`) |
| `reload_cmd` | Reloads the service |
| `health_urls` | Comma-separated URLs that must answer 2xx/3xx after the reload. They are checked three times in a row; a failure rolls back. |
| `reload_settle` | Wait before health checks (default `700ms`) |

The payload is the new file content.

### HTTP APIs (`--adapter httpapi`)

| Option | Meaning |
| --- | --- |
| `base_url` | API base URL |
| `manifest` | Compensation manifest (e.g. [`manifests/github-issues.yaml`](../manifests/github-issues.yaml)) |
| `header.<Name>` | Request headers, e.g. `header.Authorization: "Bearer ${GITHUB_TOKEN}"` |

The payload is JSON: `{"method": "PATCH", "path": "/repos/o/r/issues/3", "body": {…}}`. Each manifest entry
declares:

- `match`: method and path pattern;
- `before`: the read that captures the prior state;
- `inverse`: the undo call, templated with `{param}`, `{response.x}`, `{body.x}` and `{before.x}`;
- `reversibility`;
- `guard`: fields compared for drift. The default is the fields the call writes or the inverse restores.

Calls with no matching entry are irreversible.

## Environment variables

| Variable | Meaning |
| --- | --- |
| `CHANGEGATE_CONFIG`, `CHANGEGATE_STORE`, `CHANGEGATE_POLICY` | Override file locations |
| `CHANGEGATE_AGENT` | Marks the process as an agent: hooks and the MCP server set it, and approvals are refused when it's set |
| `CHANGEGATE_USER` | Your identity, if not your OS user |
| `CHANGEGATE_SESSION` | The current session |
| `CHANGEGATE_EXECUTOR_URL`, `CHANGEGATE_EXECUTOR_TOKEN` | Use a shared executor |
| `CHANGEGATE_REHEARSAL_DSN` | Scratch server for `rehearse` |
| `CHANGEGATE_GITLAB_TOKEN` | GitLab notes, statuses and approvals in CI |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OpenTelemetry export |
| `TYPESAFE_API_KEY` | Jev signals and `changegate sensitive suggest` |

## Sensitive columns

`sensitive` entries (`table.column` or `*.column`) are redacted in every plan and query result shown to an agent.
`changegate sensitive suggest` proposes entries from table and column names and types only, never values. It
uses Jev and needs `$TYPESAFE_API_KEY`.
