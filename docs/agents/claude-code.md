# Claude Code

Changeward gives Claude Code a safe way to change your database. Claude previews every write with
`propose_change`, policy decides, and `apply_change` runs it with an undo path. A PreToolUse hook stops Claude from
going around Changeward with `psql`, `kubectl apply` or `terraform apply`.

**Verified:** Claude Code 2.1, headless (`claude -p`), with Claude Haiku. Asked to delete a customer, Claude
called `list_tables` → `query` → `propose_change` → `apply_change` → `query`. When forced to run a raw
`psql … DELETE`, the hook blocked it and nothing was deleted.

## Set up

From your project directory:

```sh
changeward connect app --dsn '${DATABASE_URL}' --env personal   # once per target
changeward install claude-code
```

This writes two files, both safe to commit; they contain no secrets.

`.mcp.json`, the MCP server:

```json
{
  "mcpServers": {
    "changeward": {
      "command": "/usr/local/bin/changeward",
      "args": ["mcp", "postgres", "--target", "app"],
      "env": { "CHANGEWARD_AGENT": "claude-code" }
    }
  }
}
```

`.claude/settings.json`, the hook (existing settings are kept):

```json
{
  "env": { "CHANGEWARD_AGENT": "claude-code" },
  "hooks": {
    "PreToolUse": [
      { "matcher": "Bash", "hooks": [{ "type": "command", "command": "/usr/local/bin/changeward hook claude-code" }] }
    ]
  }
}
```

Restart Claude Code. On the first run it asks you to approve the project's `changeward` MCP server; `/mcp`
shows its status. For a MySQL target the server is `mcp mysql`; `install` picks it from the target.

**Plugin alternative** (once the npm package is published): `/plugin marketplace add srawlin/changeward`, then
`/plugin install changeward@changeward`. The plugin adds the hook and slash commands `/changeward-log`,
`/changeward-undo` and `/changeward-promote`. Until the package is published, use `changeward install claude-code`.

## Use it

Ask for changes in plain language: "add a `status` column to customers and archive the ones inactive for two
years". Claude will:

1. Explore with `list_tables` and `query` (read-only, row-capped).
2. Call `propose_change` with the SQL and your intent. You see the plan: affected rows with before/after values,
   locks, reversibility and the policy decision.
3. Call `apply_change`. If policy needs your confirmation (`inline` channel), Claude Code shows an approval
   prompt (MCP elicitation) with the exact plan. If it needs someone else (`out-of-band`), Claude tells you, and
   you or a colleague run `changeward approve PLAN_ID`.
4. Undo on request with `undo_change`, which is dependency-aware.

From your own terminal at any time:

```sh
changeward log              # what Claude did in this session
changeward undo 3 --yes     # undo step 3 (and anything that depends on it)
changeward rewind --yes     # undo the whole session
changeward ui               # web timeline
```

To stop permission prompts for reads, allow the read-only tools in `.claude/settings.json`. Approval of writes
still comes from Changeward's policy, not from Claude Code's permission prompt.

```json
{ "permissions": { "allow": ["mcp__changeward__query", "mcp__changeward__list_tables", "mcp__changeward__get_change_status", "mcp__changeward__list_session_changes"] } }
```

## What the hook blocks

| Command | Result |
| --- | --- |
| `psql … -c "SELECT …"`, `psql -c '\d t'` | allowed |
| `psql` writes or scripts (`-f`, stdin, heredoc) against a registered target | denied, with the equivalent `changeward exec` command |
| an interactive `psql` session against a registered target | Claude Code asks you |
| `kubectl apply/delete/patch/…` when a Kubernetes target is registered | denied, use Changeward; otherwise Claude Code asks you |
| `terraform`/`tofu apply/destroy` when a Terraform target is registered | denied |
| `helm install/upgrade/…` | Claude Code asks you |
| `changeward approve/reject/connect/restore`, `policy` edits, `trash purge` | denied: these are for humans |

Denied attempts are recorded in the audit log (`changeward audit log`, as `hook.deny` events).

## Headless and CI use

```sh
claude -p --mcp-config .mcp.json --allowedTools "mcp__changeward__query" "mcp__changeward__propose_change" "mcp__changeward__apply_change" \
  "archive customers inactive for two years"
```

In `-p` mode there is nobody to answer an inline approval, so anything that isn't auto-approved stays pending
for `changeward approve`.

## Using a shared executor

For shared or production targets, keep credentials off developer machines. Point the MCP server at your team's
executor:

```json
"args": ["mcp", "postgres", "--target", "app", "--executor", "https://changeward.internal:8443"],
"env": { "CHANGEWARD_AGENT": "claude-code", "CHANGEWARD_EXECUTOR_TOKEN": "${CHANGEWARD_EXECUTOR_TOKEN}" }
```

See [executor.md](../executor.md).

## Troubleshooting

- **Claude doesn't see the tools:** run `/mcp`, and approve the project server if asked. Check that
  `changeward targets` lists the target from the project directory.
- **"no current session" from `changeward log`:** the MCP server ends its session when Claude Code exits. Use
  `changeward log --all`.
- **The hook doesn't fire:** check `.claude/settings.json` in the project (not only `~/.claude`). Test it by hand:
  `echo '{"tool_name":"Bash","tool_input":{"command":"psql -d app -c \"DELETE FROM t\""}}' | changeward hook claude-code`.
- **Remove:** delete the `changeward` entry from `.mcp.json` and the hook entry from `.claude/settings.json`.
