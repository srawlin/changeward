# Gemini CLI

Changegate adds an MCP server and a `BeforeTool` hook on `run_shell_command` to Gemini CLI's project settings.

**Verified:** Gemini CLI 0.64, headless (`gemini -p --yolo`). The MCP tools applied a reversible `UPDATE`.
With every tool call auto-approved by `--yolo`, the hook still blocked a raw `psql DELETE`.

## Set up

```sh
changegate connect app --dsn '${DATABASE_URL}' --env personal
changegate install gemini
```

`.gemini/settings.json` (merged):

```json
{
  "mcpServers": {
    "changegate": {
      "command": "/usr/local/bin/changegate",
      "args": ["mcp", "postgres", "--target", "app"],
      "env": { "CHANGEGATE_AGENT": "gemini-cli" }
    }
  },
  "hooks": {
    "BeforeTool": [
      { "matcher": "run_shell_command", "hooks": [{ "type": "command", "command": "/usr/local/bin/changegate hook gemini" }] }
    ]
  }
}
```

The hook answers `{"decision": "deny", "reason": "Changegate: …"}` with exit code 0, which is the form Gemini CLI
prefers. An exit code of 2 would show up as a "failed" hook.

## Use it

```sh
gemini                                     # interactive; trust the folder when asked
gemini -p "upgrade customer 6 to the pro tier"
```

Gemini CLI runs project settings only in **trusted folders**. Trust the folder interactively, or for automation
set `GEMINI_CLI_TRUST_WORKSPACE=true` (or pass `--skip-trust`). Each time project hooks change, Gemini shows a
warning listing them; that's expected.

## Troubleshooting

- **"Approval mode overridden… folder is not trusted":** see above.
- **The hook didn't fire:** the model may have refused on its own after reading the Changegate server's
  instructions. That's fine. `changegate audit log` shows `hook.deny` events whenever the hook does block.
