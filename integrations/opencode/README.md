# Changeward for OpenCode

```sh
changeward connect app --dsn "$DATABASE_URL" --env personal
changeward install opencode
```

This writes:

- `opencode.json`: the `changeward` MCP server (`query`, `propose_change`, `apply_change`, `undo_change`, ...).
- `.opencode/plugins/changeward.ts`: a `tool.execute.before` plugin. Before each `bash` call it runs
  `changeward hook check --json`, and throws (blocking the call) when the command would change a registered
  target directly, e.g. a `psql` write, `kubectl apply` or `terraform apply`. The error tells the agent to use the MCP tools.

Verified with OpenCode 1.18 (`opencode run`, Claude Haiku 4.5): the agent used `changeward_query` →
`changeward_propose_change` → `changeward_apply_change` by itself; when forced to run a raw `psql ... DELETE`,
the plugin blocked it and the agent offered to go through Changeward instead.

The plugin source is `cmd/changeward/opencode_plugin.ts` (embedded in the binary). Set `CHANGEWARD_BIN` to
override the binary path.
