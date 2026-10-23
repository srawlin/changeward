# Changeward for Claude Code

Lets Claude Code act on your database through Changeward instead of raw SQL:

- **MCP server** (`changeward mcp postgres`): `query`, `list_tables`, `propose_change` (exact dry-run preview),
  `apply_change`, `undo_change`, `get_change_status`, `list_session_changes`, `checkpoint`.
- **PreToolUse hook** (`changeward hook claude-code`): blocks `psql` writes against registered targets and points
  Claude at the MCP tools; blocks agent attempts to `changeward approve` its own changes; asks before
  `kubectl`/`terraform`/`helm` changes.
- **Slash commands**: `/changeward-log`, `/changeward-undo`, `/changeward-promote`.

## Install

Plugin (from this repository's marketplace):

```
/plugin marketplace add srawlin/changeward
/plugin install changeward@changeward
```

Or without the plugin system, from your project directory:

```
changeward connect app --dsn '${DATABASE_URL}' --env personal
changeward install claude-code
```

`install` writes `.mcp.json` and a PreToolUse hook into `.claude/settings.json`, pointing at the local binary.

In `personal` targets, reversible changes are auto-approved; anything not reversible asks you inline
(MCP elicitation). In `shared-dev`, `staging` and `prod`, policy decides, and production approvals happen out of band
(`changeward approve`, Slack), never inside the agent's own UI.
