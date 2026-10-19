# Getting started

This guide takes about ten minutes:

1. Install Changegate.
2. Connect a sample Postgres database.
3. Make, inspect and undo changes yourself.
4. Hand the database to a coding agent.

## Install

Changegate is a single binary, `changegate`.

**From source** (until the first release). You need Go 1.27+ and a C compiler; the Postgres parser uses cgo.

```sh
git clone https://github.com/srawlin/changegate
cd changegate
make build                       # → bin/changegate
export PATH="$PWD/bin:$PATH"
changegate version
```

**Docker:** `docker build -t changegate .` builds an Alpine image with `changegate`, `git` and `kubectl`.

**Packages** are built by the release workflow (`.github/workflows/release.yml`) once a version is tagged:

| Channel | Command |
| --- | --- |
| Homebrew | `brew install srawlin/tap/changegate` |
| npm (no install) | `npx -y @changegate/cli …` |
| PyPI (no install) | `uvx changegate …` |
| Container | `ghcr.io/srawlin/changegate` |

## 1. A sample database

```sh
docker run -d --name shop-db -e POSTGRES_PASSWORD=pg -p 5433:5432 postgres:18-alpine
sleep 3
docker exec shop-db psql -U postgres -c "CREATE DATABASE shop"
docker exec shop-db psql -U postgres -d shop -c "
  CREATE TABLE customers(id bigserial PRIMARY KEY, email text NOT NULL UNIQUE,
                         plan text NOT NULL DEFAULT 'free', last_seen date);
  INSERT INTO customers(email, plan, last_seen)
  SELECT 'user'||g||'@example.com', CASE WHEN g % 10 = 0 THEN 'pro' ELSE 'free' END, DATE '2026-10-01' - g * 7
  FROM generate_series(1, 200) g"
```

## 2. Connect it

```sh
mkdir shop && cd shop
changegate connect shop --dsn 'postgres://postgres:pg@localhost:5433/shop?sslmode=disable' --env personal
```

This writes `changegate.yaml` in the current directory. The audit store goes in `.changegate/`. `--env` binds
the connection to an **environment class**: `personal`, `shared-dev`, `staging`, `prod` or `prod-restricted`. The
class sets the default policy, and an agent can never change it. Keep secrets out of the file with
`--dsn '${SHOP_DATABASE_URL}'`.

## 3. Make some changes

Start a session, so the changes are grouped and can be undone together:

```console
$ changegate session start --agent demo
session s_30f9a360ec02 started on shop (personal)

$ changegate exec --intent "add a status column" \
    "ALTER TABLE customers ADD COLUMN status text NOT NULL DEFAULT 'active'"
plan p_cc3e0861f0ad · shop (personal) · reversible · 0 rows · live-dry-run
#1 ALTER TABLE customers ADD COLUMN status text NOT NULL DEFAULT 'active'
   ALTER TABLE on public.customers (200 rows)
locks: public.customers=AccessExclusiveLock
change c_cef3901905f6 (#1) — auto-approved
policy: auto-approved (builtin/personal-auto)
change c_cef3901905f6 — verified · 0 rows · ALTER TABLE

$ changegate exec --intent "archive customers inactive for two years" \
    "UPDATE customers SET status = 'archived' WHERE last_seen < DATE '2024-10-19'"
plan p_805e48ce4b33 · shop (personal) · reversible · 99 rows · live-dry-run
#1 UPDATE public.customers — 99 rows
   ~ id=102  status: "active" → "archived"
   ~ id=103  status: "active" → "archived"
   …
change c_0abd3564f023 (#2) — verified · 99 rows · UPDATE 99

$ changegate exec "DELETE FROM customers WHERE plan = 'free' AND status = 'archived'"
plan p_f0d05ccc3d5d · shop (personal) · reversible · 89 rows · live-dry-run
#1 DELETE public.customers — 89 rows
   - id=121  {email="user121@example.com" id=121 last_seen="2024-06-06" plan="free" status="archived"}
   …
change c_7587bd7fb6bd (#3) — verified · 89 rows · DELETE 89
```

Each change was **planned first**. The statement ran in a transaction that was always rolled back, which
captured the exact rows and before/after values. Policy then decided. In `personal`, reversible changes are
auto-approved. Changegate applied the change, checked that only the planned rows moved, and kept the undo data.

## 4. Look and undo

```console
$ changegate log
SESSION s_30f9a360ec02  agent=demo  user=you  env=personal  target=shop
  #1  20:47  ALTER TABLE customers ADD COLUMN status text NOT NULL DEF...   reversible  verified
  #2  20:47  UPDATE customers SET status = 'archived' WHERE last_seen ... (99 rows) reversible  verified
  #3  20:47  DELETE FROM customers WHERE plan = 'free' AND status = 'a... (89 rows) reversible  verified

$ changegate undo 2 --yes
undoing c_0abd3564f023 first requires undoing 1 later change(s) that touch the same objects:
  c_7587bd7fb6bd  DELETE FROM customers WHERE plan = 'free' AND status = 'a...
undone c_7587bd7fb6bd  DELETE FROM customers WHERE plan = 'free' AND status = 'a...
undone c_0abd3564f023  UPDATE customers SET status = 'archived' WHERE last_seen ...

$ changegate rewind --yes        # undo everything left in the session
undone c_cef3901905f6  ALTER TABLE customers ADD COLUMN status text NOT NULL DEF...
```

Undo is dependency-aware. Undoing step 2 first undid step 3, which deleted rows that step 2 had changed.

Undo is guarded too. A row is restored only if it still matches what the change left behind. If someone edited
it since, the undo stops with a conflict instead of overwriting their work.

`changegate ui` shows the same timeline in a local web view, with diffs, decisions and the audit log.

## 5. See production policy

Connect the same database as `prod`, and try a direct write:

```console
$ changegate connect shop-prod --dsn 'postgres://postgres:pg@localhost:5433/shop?sslmode=disable' --env prod
$ changegate exec --target shop-prod "UPDATE customers SET plan = 'pro' WHERE id = 1"
plan p_3985405c7f7f · shop-prod (prod) · reversible · 1 rows · live-dry-run
#1 UPDATE public.customers — 1 rows
   ~ id=h:6b86b273  plan
change c_15571c41ae7a (#4) — rejected
policy: denied: direct production writes need an incident or ticket reference (changegate session start --incident ...); production direct changes need out-of-band approval by someone other than the proposer
```

In production, row values are hidden from the plan, and keys are hashed. A direct write needs:

- an incident reference: `changegate session start --target shop-prod --incident INC-123`;
- approval from someone other than the proposer: `changegate approve PLAN_ID` from another identity, or Slack.

Write your own rules in `changegate.policy.yaml`; see [policy.md](policy.md).

## 6. Give it to your agent

```sh
changegate install claude-code      # or codex, opencode, gemini, cursor
```

Restart the agent and ask for a change in plain language. It will call `propose_change`, show you the plan, and
`apply_change` once policy allows it. If it reaches for `psql` instead, the hook blocks the command and points it
back to Changegate. Agent-specific details: [agents/](agents/).

## Next steps

- [Rehearse migrations in pull requests](ci/github-actions.md) (or [GitLab](ci/gitlab.md)).
- [Promote a session to a migration and a PR](concepts.md#promotion): `changegate promote --pr`.
- [Run a shared executor](executor.md) so agents never hold production credentials.
- [Checkpoints](concepts.md#checkpoints): `changegate session start --checkpoint` makes even irreversible
  changes restorable.
