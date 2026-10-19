# CLI reference

Generated from `changegate --help` by `scripts/gen-cli-docs.sh`. Every command also accepts `--json`
for machine-readable output.

## `changegate adapter`

```text
Adapter tools

Usage:
  changegate adapter [command]

Available Commands:
  test        Run the adapter conformance suite

Flags:
  -h, --help   help for adapter

```

### `changegate adapter test`

```text
Run the conformance suite against a built-in adapter or a plugin binary.

  changegate adapter test postgres --dsn postgres://admin@localhost/postgres
  changegate adapter test toykv
  changegate adapter test --plugin ./changegate-adapter-foo --config path=/tmp/x

The Postgres driver creates and drops scratch databases on the server; the
DSN must be able to CREATE DATABASE. Plugins take part through the
changegate.fingerprint and changegate.corpus Plan conventions.

Usage:
  changegate adapter test [postgres|toykv] [flags]

Flags:
      --config stringArray   plugin target config key=value ({tmp} expands to a scratch path)
      --dsn string           admin DSN for the postgres driver
  -h, --help                 help for test
      --n int                corpus size (default 30)
      --plugin string        adapter plugin binary
      --seed int             corpus seed (default 1)

```

## `changegate apply`

```text
Apply an approved change (REF: plan ID, change ID, or #seq)

Usage:
  changegate apply REF [flags]

Flags:
  -h, --help   help for apply
  -y, --yes    approve inline-approvable changes without prompting

```

## `changegate approve`

```text
Approve a pending change (binds to its exact plan hash)

Usage:
  changegate approve REF [flags]

Flags:
      --as string         approver identity (local mode; server mode uses SSO)
      --channel string    approval channel (default "terminal")
      --executor string   approve on a shared executor (default $CHANGEGATE_EXECUTOR_URL; token from $CHANGEGATE_EXECUTOR_TOKEN, approve role)
  -h, --help              help for approve
      --reason string     reason

```

## `changegate audit`

```text
Inspect and verify the tamper-evident audit log

Usage:
  changegate audit [command]

Available Commands:
  log         Print audit events
  verify      Verify the event hash chain

Flags:
  -h, --help   help for audit

```

### `changegate audit log`

```text
Print audit events

Usage:
  changegate audit log [flags]

Flags:
      --change string    filter by change ID
  -h, --help             help for log
      --limit int        show the last N events (0 = all) (default 50)
      --session string   filter by session ID

```

### `changegate audit verify`

```text
Verify the event hash chain

Usage:
  changegate audit verify [flags]

Flags:
  -h, --help   help for verify

```

## `changegate branch`

```text
Work on a private branch of a target (branch-as-working-target)

Usage:
  changegate branch [command]

Available Commands:
  create      Create a branch of the session's target and point the session at it
  discard     Drop the session's branch

Flags:
  -h, --help   help for branch

```

### `changegate branch create`

```text
Create a branch of the session's target and point the session at it

Usage:
  changegate branch create [SESSION] [flags]

Flags:
  -h, --help          help for create
      --name string   branch name

```

### `changegate branch discard`

```text
Drop the session's branch

Usage:
  changegate branch discard [SESSION] [flags]

Flags:
  -h, --help   help for discard

```

## `changegate changeset`

```text
Plan and apply ordered changes across systems as a saga (failed step → earlier steps undone)

Usage:
  changegate changeset [command]

Available Commands:
  apply       Apply every step in order; on failure undo the steps already applied
  plan        Plan every step and decide policy for the group
  undo        Undo an applied changeset, last step first

Flags:
  -h, --help   help for changeset

```

### `changegate changeset apply`

```text
Apply every step in order; on failure undo the steps already applied

Usage:
  changegate changeset apply ID [flags]

Flags:
  -h, --help   help for apply

```

### `changegate changeset plan`

```text
Plan a changeset. Example:

  name: add-region
  steps:
    - target: app            # postgres
      sql: ALTER TABLE orders ADD COLUMN region text
    - target: cluster        # kubernetes
      file: k8s/web.yaml
    - target: edge           # nginx (configfile)
      file: nginx/default.conf

Every step is planned against current state, so steps that depend on each
other's effects on the same target belong in one multi-statement step.

Usage:
  changegate changeset plan -f CHANGESET.yaml [flags]

Flags:
  -f, --file string   changeset file
  -h, --help          help for plan

```

### `changegate changeset undo`

```text
Undo an applied changeset, last step first

Usage:
  changegate changeset undo ID [flags]

Flags:
  -h, --help   help for undo

```

## `changegate checkpoint`

```text
Checkpoint the current session's target

Usage:
  changegate checkpoint [flags]

Flags:
  -h, --help             help for checkpoint
      --label string     label
      --session string   session (default: current)

```

## `changegate connect`

```text
Register a target in changegate.yaml. The environment class is bound to the
connection, never chosen by the agent: personal, shared-dev, staging, prod, prod-restricted.

Use ${VAR} in --dsn to keep secrets out of the file, e.g. --dsn '${DATABASE_URL}'.

Usage:
  changegate connect NAME [flags]

Flags:
      --adapter string       adapter (default "postgres")
      --default              make this the default target
      --dsn string           connection string (supports ${VAR})
      --env string           environment class (default "personal")
  -h, --help                 help for connect
      --option stringArray   adapter option key=value (repeatable)
      --sensitive strings    sensitive columns to redact (table.column, *.column)

```

## `changegate detect`

```text
Detect agent provenance of the commits in a change (bots, trailers, branches)

Usage:
  changegate detect [flags]

Flags:
      --base-ref string   base ref (commits in base..HEAD are inspected)
  -h, --help              help for detect

```

## `changegate drift`

```text
List sessions in the given environment classes that applied direct changes
to Postgres or Kubernetes targets without a promotion or reconciliation PR
(changegate promote/reconcile). Run it from cron or CI to enforce that
break-glass changes reach Git within a time limit.

Usage:
  changegate drift [flags]

Flags:
      --env string         environment classes to check (default "staging,prod,prod-restricted")
  -h, --help               help for drift
      --max-age duration   fail when a direct change has been unreconciled for longer than this (e.g. 24h)

```

## `changegate exec`

```text
Plan and, if policy allows, apply a change in one step

Usage:
  changegate exec [SQL] [flags]

Flags:
  -f, --file string      read SQL from FILE ('-' for stdin)
  -h, --help             help for exec
      --intent string    what the change is for
      --no-session       do not attach to the current session
      --session string   session (default: current)
  -t, --target string    target name
  -y, --yes              approve inline-approvable changes without prompting

```

## `changegate executor`

```text
Run or configure a shared executor (holds target credentials; enforces policy next to the target)

Usage:
  changegate executor [command]

Available Commands:
  serve       Serve the executor API
  token       Create an API token (prints the token once, and the config entry to add)

Flags:
  -h, --help   help for executor

```

### `changegate executor serve`

```text
Serve the executor API for MCP servers, CLIs and SDKs (changegate mcp postgres --executor URL).
Tokens are configured under executor.tokens in changegate.yaml; the token's name is the delegating human.
Serve TLS directly (--tls-cert/--tls-key) or behind a TLS-terminating proxy.

Usage:
  changegate executor serve [flags]

Flags:
      --addr string       listen address (default "127.0.0.1:8443")
  -h, --help              help for serve
      --tls-cert string   TLS certificate
      --tls-key string    TLS key

```

### `changegate executor token`

```text
Create an API token (prints the token once, and the config entry to add)

Usage:
  changegate executor token NAME [flags]

Flags:
  -h, --help           help for token
      --roles string   comma-separated roles: propose, approve, admin (default "propose")

```

## `changegate gate`

```text
Apply policy to rehearsal evidence. Exit codes: 0 approved (standard change),
2 review required (approvers printed), 3 denied, 1 rehearsal failed.
With --report-only the decision is printed but the exit code is always 0
(the recommended way to start: report, don't block).
With --github or --gitlab, approvals on the pull/merge request that satisfy
"review required" (named user: approvers, quorum, never the author) pass the gate.

Usage:
  changegate gate EVIDENCE.json [flags]

Flags:
      --env string    override the environment class (e.g. the deployment environment)
      --github        count GitHub pull request approvals toward "review required" (GitHub Actions env, GITHUB_TOKEN)
      --gitlab        count GitLab merge request approvals toward "review required" (CHANGEGATE_GITLAB_TOKEN)
  -h, --help          help for gate
      --report-only   never fail; just print the decision

```

## `changegate hook`

```text
Coding-agent hooks (called by the agent, not by people)

Usage:
  changegate hook [command]

Available Commands:
  check       Evaluate a shell command for any agent; exit 2 means blocked
  claude-code Claude Code PreToolUse hook: route direct database changes through Changegate
  codex       Codex PreToolUse hook (same wire format as Claude Code)
  cursor      Cursor beforeShellExecution hook
  gemini      Gemini CLI BeforeTool hook (shell commands); prints {decision: deny}

Flags:
  -h, --help   help for hook

```

### `changegate hook check`

```text
Evaluate a shell command for any agent; exit 2 means blocked

Usage:
  changegate hook check COMMAND [flags]

Flags:
  -h, --help   help for check

```

### `changegate hook claude-code`

```text
Claude Code PreToolUse hook: route direct database changes through Changegate

Usage:
  changegate hook claude-code [flags]

Flags:
  -h, --help   help for claude-code

```

### `changegate hook codex`

```text
Codex PreToolUse hook (same wire format as Claude Code)

Usage:
  changegate hook codex [flags]

Flags:
  -h, --help   help for codex

```

### `changegate hook cursor`

```text
Cursor beforeShellExecution hook

Usage:
  changegate hook cursor [flags]

Flags:
  -h, --help   help for cursor

```

### `changegate hook gemini`

```text
Gemini CLI BeforeTool hook (shell commands); prints {decision: deny}

Usage:
  changegate hook gemini [flags]

Flags:
  -h, --help   help for gemini

```

## `changegate install`

```text
Install Changegate into a coding agent (claude-code, codex, cursor, gemini, opencode, agents-md)

Usage:
  changegate install AGENT [flags]

Flags:
  -h, --help            help for install
  -t, --target string   target the MCP server guards (default: the default target)

```

## `changegate k8s`

```text
Kubernetes and GitOps (Argo CD, Flux) integration

Usage:
  changegate k8s [command]

Available Commands:
  gate        GitOps gate: plan desired manifests against the live cluster; pass only if policy approves this exact plan

Flags:
  -h, --help   help for k8s

```

### `changegate k8s gate`

```text
Plan the manifests a sync would apply (server-side dry run against live objects) and apply
policy in the CI lane. Exit 0 when auto-approved or when this exact plan hash was approved
(e.g. with changegate approve, Slack or the web UI); exit 2 while approval is pending; exit 3
when denied. Run it in CI on GitOps-repository pull requests, or as an Argo CD PreSync hook
Job (see integrations/argocd). Changegate never applies here: the GitOps controller does.

Usage:
  changegate k8s gate --manifests DIR [flags]

Flags:
      --executor string    run the gate on a shared executor ($CHANGEGATE_EXECUTOR_URL; token in $CHANGEGATE_EXECUTOR_TOKEN)
  -h, --help               help for gate
      --manifests string   manifest file or directory (rendered YAML)
      --out string         write the plan diff here (for PR comments)
  -t, --target string      kubernetes target

```

## `changegate log`

```text
Show a session's timeline (default: current session)

Usage:
  changegate log [SESSION] [flags]

Flags:
      --all    show recent sessions
  -h, --help   help for log

```

## `changegate mcp`

```text
Run a local MCP server that gives an agent plan → approve → apply → undo tools
for a target instead of raw execution. Credentials stay in this process.

Add to an MCP client, e.g. Claude Code's .mcp.json:
  {"mcpServers": {"changegate": {"command": "changegate", "args": ["mcp", "postgres", "--target", "app"]}}}

Usage:
  changegate mcp ADAPTER [flags]

Flags:
      --branch            work on a private branch of the target
      --checkpoint        checkpoint the target when the session starts
      --executor string   use a shared executor at this URL ($CHANGEGATE_EXECUTOR_URL); token from $CHANGEGATE_EXECUTOR_TOKEN
  -h, --help              help for mcp
      --session string    attach to an existing session instead of starting one
  -t, --target string     target (default: the default target)

```

## `changegate migrations`

```text
Detect migration frameworks and list migrations

Usage:
  changegate migrations [flags]

Flags:
      --dir string   repository root (default: git top level)
  -h, --help         help for migrations

```

## `changegate plan`

```text
Without a target (or with --static), plan analyzes SQL statically: statement
classification and risk findings. With a target, it runs a live dry run in a
rolled-back transaction, records the proposal, and applies policy.

Exit codes: 0 auto-approved (or static), 2 approval required, 3 denied.

Usage:
  changegate plan [SQL] [flags]

Flags:
  -f, --file string      read SQL from FILE ('-' for stdin)
  -h, --help             help for plan
      --intent string    what the change is for
      --no-session       do not attach to the current session
      --session string   session (default: current)
      --static           static analysis only (no database)
  -t, --target string    target name

```

## `changegate policy`

```text
Manage CEL change-control policy

Usage:
  changegate policy [command]

Available Commands:
  check       Compile a policy file and report errors
  init        Write changegate.policy.yaml with the built-in defaults as a starting point

Flags:
  -h, --help   help for policy

```

### `changegate policy check`

```text
Compile a policy file and report errors

Usage:
  changegate policy check [FILE] [flags]

Flags:
  -h, --help   help for check

```

### `changegate policy init`

```text
Write changegate.policy.yaml with the built-in defaults as a starting point

Usage:
  changegate policy init [flags]

Flags:
  -h, --help   help for init

```

## `changegate promote`

```text
Promote converts the applied changes of a direct-lane session into a migration
for the project's framework (detected from the repository; override with
--framework/--dir). Up is the SQL as written; down is generated from the
recorded undo handles, so the rollback path is one that was exercised.
With --pr it commits to a new branch and opens a pull request carrying the
session timeline as evidence; Lane A then rehearses and gates it.

Writable frameworks: golang-migrate, goose, flyway, liquibase, atlas, prisma, sqitch, rails, django, alembic

Usage:
  changegate promote [SESSION] [flags]

Flags:
      --base string        base branch for the PR (default: the repository default)
      --dir string         migrations directory, relative to the repository root
      --dry-run            print the migration instead of writing it
      --framework string   migration framework (default: detected)
  -h, --help               help for promote
      --include-data       include data changes (DML) in the migration
      --k8s-dir string     manifests directory for Kubernetes changes, relative to the repository root
      --name string        migration name (default: the first change's intent)
      --pr                 commit on a new branch and open a pull request (needs git and gh)

```

## `changegate proxy`

```text
Wrap an existing stdio MCP server. Known SQL tools (execute_sql, query, ...)
are rewritten when --target names a Postgres target: reads pass through to the
upstream server, writes are planned, approved and applied by Changegate.
Other non-read-only tools get approval-only handling under --env policy.

  changegate proxy --target app -- npx -y @modelcontextprotocol/server-postgres $DATABASE_URL

Usage:
  changegate proxy [flags] -- COMMAND [ARGS...]

Flags:
      --env string         environment class for other tools (default "personal")
  -h, --help               help for proxy
      --name string        upstream name used in change records
      --sql-tool strings   tool names whose argument is SQL (default: common names)
  -t, --target string      Postgres target for SQL tools (enables rewriting)

```

## `changegate reconcile`

```text
Promote converts the applied changes of a direct-lane session into a migration
for the project's framework (detected from the repository; override with
--framework/--dir). Up is the SQL as written; down is generated from the
recorded undo handles, so the rollback path is one that was exercised.
With --pr it commits to a new branch and opens a pull request carrying the
session timeline as evidence; Lane A then rehearses and gates it.

Writable frameworks: golang-migrate, goose, flyway, liquibase, atlas, prisma, sqitch, rails, django, alembic

Usage:
  changegate reconcile [SESSION] [flags]

Flags:
      --base string        base branch for the PR (default: the repository default)
      --dir string         migrations directory, relative to the repository root
      --dry-run            print the migration instead of writing it
      --framework string   migration framework (default: detected)
  -h, --help               help for reconcile
      --include-data       include data changes (DML) in the migration
      --k8s-dir string     manifests directory for Kubernetes changes, relative to the repository root
      --name string        migration name (default: the first change's intent)
      --pr                 commit on a new branch and open a pull request (needs git and gh)

```

## `changegate rehearse`

```text
Rehearse the migrations a change adds (Lane A). Changegate builds a throwaway
copy, replays base migrations (T0) or clones a database (--from-database,
--neon), optionally generates synthetic data (T1), then applies each new
migration, observing locks, table rewrites and rows touched; runs the down
migrations in reverse; and compares catalog and data checksums with the state
before each migration. Policy is applied to the evidence for --env.

  changegate rehearse --base-ref origin/main --dsn postgres://postgres@localhost/postgres --out evidence.json

Exit codes: 0 rehearsal ran (see the report and gate), 1 a migration failed.

Usage:
  changegate rehearse [flags]

Flags:
      --all                     treat every migration as new (rehearse the whole history)
      --base-ref string         git ref of the base branch; migrations changed since it are new
      --dir string              repository root (default: git top level)
      --down-cmd string         command that rolls back {n} migrations
      --dsn string              admin DSN of a scratch server (default: $CHANGEGATE_REHEARSAL_DSN)
      --env string              environment class the change will deploy to (for policy) (default "prod")
      --framework string        migration framework (default: detected)
      --from-database string    clone this database (golden image or clone) instead of replaying base migrations
  -h, --help                    help for rehearse
      --keep                    keep the rehearsal database
      --max-rows int            cap synthetic rows per table (default 100000)
      --migrations-dir string   migrations directory relative to the root (default: detected)
      --neon                    rehearse on a Neon branch ($NEON_API_KEY, $NEON_PROJECT_ID, $NEON_BRANCH_ID, --dsn = branch DSN)
      --new strings             treat these migration paths as new (instead of --base-ref)
      --out report              write evidence JSON here (input to report and `gate`)
      --scale-from string       read per-table row counts (reltuples only, no values) from this DSN for synthetic data
      --schema string           load this schema dump instead of replaying base migrations
      --seed string             SQL file to load after the base state
      --synthetic int           generate N synthetic rows per table (T1)
      --tier string             fidelity tier of the copy (T0-T3); inferred when not set
      --up-cmd string           command that migrates to head (code-based frameworks; DATABASE_URL is set)

```

## `changegate reject`

```text
Reject a pending change

Usage:
  changegate reject REF [flags]

Flags:
      --as string         approver identity (local mode; server mode uses SSO)
      --channel string    approval channel (default "terminal")
      --executor string   approve on a shared executor (default $CHANGEGATE_EXECUTOR_URL; token from $CHANGEGATE_EXECUTOR_TOKEN, approve role)
  -h, --help              help for reject
      --reason string     reason

```

## `changegate report`

```text
Render rehearsal evidence as a PR/MR comment (and post it)

Usage:
  changegate report EVIDENCE.json [flags]

Flags:
      --github   post to the GitHub pull request (GitHub Actions env)
      --gitlab   post to the GitLab merge request (GitLab CI env)
  -h, --help     help for report
      --status   also set a commit status

```

## `changegate restore`

```text
Restore a checkpoint (discards everything after it)

Usage:
  changegate restore CHECKPOINT [flags]

Flags:
  -h, --help             help for restore
      --session string   session
  -y, --yes              confirm

```

## `changegate rewind`

```text
Undo every change in a session, newest first (or restore its start checkpoint)

Usage:
  changegate rewind [SESSION] [flags]

Flags:
      --checkpoint   restore the session checkpoint if an undo fails
  -h, --help         help for rewind
  -y, --yes          do not ask

```

## `changegate sensitive`

```text
Manage sensitive-column redaction

Usage:
  changegate sensitive [command]

Available Commands:
  suggest     Suggest columns to redact, using Jev on table/column names and types only (never values)

Flags:
  -h, --help   help for sensitive

```

### `changegate sensitive suggest`

```text
Ask TypeSafe's Jev, for every column of the target, whether its name and type suggest
personal or secret data. Only schema names and types are sent; row values are never read.
Suggestions can only add redaction. With --apply they are merged into the target's
sensitive list in changegate.yaml. Requires $TYPESAFE_API_KEY.

Usage:
  changegate sensitive suggest [flags]

Flags:
      --apply             add the suggestions to the target's sensitive list
  -h, --help              help for suggest
  -t, --target string     SQL target
      --threshold float   minimum score to suggest a column (default 0.7)

```

## `changegate serve`

```text
Serve HTTP endpoints for out-of-band approvals:

  POST /slack/interactions   Slack interactivity request URL (signed requests only)
  GET  /changes/{ref}        change record (JSON, plan config redacted)
  GET  /healthz

Configure approvals.slack in changegate.yaml (bot_token, channel, signing_secret,
users). Slack must reach this address, e.g. through your ingress or a tunnel.

Usage:
  changegate serve [flags]

Flags:
      --addr string   listen address (default "127.0.0.1:8484")
  -h, --help          help for serve

```

## `changegate session`

```text
Manage direct-lane sessions

Usage:
  changegate session [command]

Available Commands:
  end         End a session
  list        List recent sessions
  start       Start a session (optionally with a checkpoint, or on a private branch)
  use         Make SESSION the current session

Flags:
  -h, --help   help for session

```

### `changegate session end`

```text
End a session

Usage:
  changegate session end [SESSION] [flags]

Flags:
  -h, --help   help for end

```

### `changegate session list`

```text
List recent sessions

Usage:
  changegate session list [flags]

Flags:
  -h, --help   help for list

```

### `changegate session start`

```text
Start a session (optionally with a checkpoint, or on a private branch)

Usage:
  changegate session start [flags]

Flags:
      --agent string      agent name (e.g. claude-code)
      --branch            work on a private branch of the target
      --checkpoint        take a checkpoint at session start
  -h, --help              help for start
      --incident string   incident or ticket reference (required for direct prod writes)
  -t, --target string     target

```

### `changegate session use`

```text
Make SESSION the current session

Usage:
  changegate session use SESSION [flags]

Flags:
  -h, --help   help for use

```

## `changegate show`

```text
Show a change record

Usage:
  changegate show REF [flags]

Flags:
  -h, --help   help for show

```

## `changegate status`

```text
Print a change's state

Usage:
  changegate status REF [flags]

Flags:
  -h, --help   help for status

```

## `changegate targets`

```text
List configured targets

Usage:
  changegate targets [flags]

Flags:
  -h, --help   help for targets

```

## `changegate tf`

```text
Terraform/OpenTofu in CI (Lane A)

Usage:
  changegate tf [command]

Available Commands:
  review      Classify a plan (terraform show -json) for review and gate it with policy

Flags:
  -h, --help   help for tf

```

### `changegate tf review`

```text
Review a plan produced by your pipeline (terraform plan -out p && terraform show -json p > plan.json).
Every resource change is classified; destroying or replacing a stateful resource
(databases, buckets, volumes, keys, DNS zones) is irreversible. Policy decides in the
CI lane. Exit codes: 0 approved, 2 review required, 3 denied.

Usage:
  changegate tf review --plan-json FILE [flags]

Flags:
      --env string              environment class the plan applies to (default "prod")
      --github                  post the review as a PR comment
  -h, --help                    help for review
      --plan-json string        output of terraform show -json PLANFILE
      --stateful-types string   extra resource types to treat as stateful

```

## `changegate trash`

```text
List or purge soft-dropped Postgres objects

Usage:
  changegate trash [command]

Available Commands:
  list        List soft-dropped tables and columns
  purge       Permanently drop soft-dropped objects (they can no longer be undone)

Flags:
  -h, --help   help for trash

```

### `changegate trash list`

```text
List soft-dropped tables and columns

Usage:
  changegate trash list [flags]

Flags:
  -h, --help            help for list
  -t, --target string   target

```

### `changegate trash purge`

```text
Permanently drop soft-dropped objects (they can no longer be undone)

Usage:
  changegate trash purge [flags]

Flags:
  -h, --help            help for purge
  -t, --target string   target
  -y, --yes             confirm

```

## `changegate ui`

```text
Open the local web view of sessions, changes, diffs and audit events

Usage:
  changegate ui [flags]

Flags:
      --addr string   listen address (localhost only) (default "127.0.0.1:8485")
  -h, --help          help for ui
      --open          open a browser

```

## `changegate undo`

```text
Undo a change (STEP is its number in the session; dependents are undone first)

Usage:
  changegate undo STEP|REF [flags]

Flags:
  -h, --help             help for undo
      --only             undo only this change (guards still apply)
      --session string   session for STEP numbers (default: current)
  -y, --yes              undo dependent changes without asking

```

## `changegate version`

```text
Print the version

Usage:
  changegate version [flags]

Flags:
  -h, --help   help for version

```

