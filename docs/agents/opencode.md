# OpenCode

Changeward adds an MCP server and a small OpenCode plugin. The plugin checks every `bash` tool call and blocks
direct writes to your registered targets.

**Verified:** OpenCode 1.18 (`opencode run`, Claude Haiku 4.5). The agent used `changeward_query` →
`changeward_propose_change` → `changeward_apply_change` without being told to. When forced to run a raw
`psql … DELETE`, the plugin blocked it, and the agent offered to go through Changeward instead.

## Set up

```sh
changeward connect app --dsn '${DATABASE_URL}' --env personal
changeward install opencode
```

`opencode.json` (merged with your existing config):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "changeward": {
      "type": "local",
      "command": ["/usr/local/bin/changeward", "mcp", "postgres", "--target", "app"],
      "environment": { "CHANGEWARD_AGENT": "opencode" },
      "enabled": true
    }
  }
}
```

`.opencode/plugins/changeward.ts` is a `tool.execute.before` handler. It runs
`changeward hook check --json "<command>"` and throws, which blocks the call, on `deny` or `ask`. If the
`changeward` binary is missing, it lets commands through rather than breaking the agent. Set `CHANGEWARD_BIN` to
override the path.

## Use it

```sh
opencode                                  # TUI
opencode run "remove customer 19, they closed their account"
```

Check the server with `opencode mcp list`. The tools appear as `changeward_query`, `changeward_propose_change`
and so on.

## Troubleshooting

- **Plugin not loaded:** OpenCode loads `.opencode/plugins/*.ts` from the project root. Start OpenCode from the
  directory that contains `changeward.yaml`.
- **Test the decision by hand:** `changeward hook check --json "psql -d app -c 'DELETE FROM t'"`.
