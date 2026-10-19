# Changegate for OpenCode

```sh
changegate connect app --dsn "$DATABASE_URL" --env personal
changegate install opencode
```

This writes:

- `opencode.json`: the `changegate` MCP server (`query`, `propose_change`, `apply_change`, `undo_change`, ...).
- `.opencode/plugins/changegate.ts`: a `tool.execute.before` plugin. Before each `bash` call it runs
  `changegate hook check --json`, and throws (blocking the call) when the command would change a registered
  target directly, e.g. a `psql` write, `kubectl apply` or `terraform apply`. The error tells the agent to use the MCP tools.

Verified with OpenCode 1.18 (`opencode run`, Claude Haiku 4.5): the agent used `changegate_query` →
`changegate_propose_change` → `changegate_apply_change` by itself; when forced to run a raw `psql ... DELETE`,
the plugin blocked it and the agent offered to go through Changegate instead.

The plugin source is `cmd/changegate/opencode_plugin.ts` (embedded in the binary). Set `CHANGEGATE_BIN` to
override the binary path.
