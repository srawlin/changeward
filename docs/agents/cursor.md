# Cursor

Changeward adds an MCP server and a `beforeShellExecution` hook to Cursor's project configuration.

**Status:** the hook was tested with Cursor's documented payloads, but **not yet inside the Cursor app** (we had
no Cursor account to test with). Please report what you see.

## Set up

```sh
changeward connect app --dsn '${DATABASE_URL}' --env personal
changeward install cursor
```

`.cursor/mcp.json`:

```json
{ "mcpServers": { "changeward": { "command": "/usr/local/bin/changeward", "args": ["mcp", "postgres", "--target", "app"],
                                  "env": { "CHANGEWARD_AGENT": "cursor" } } } }
```

`.cursor/hooks.json`:

```json
{ "version": 1, "hooks": { "beforeShellExecution": [{ "command": "/usr/local/bin/changeward hook cursor" }] } }
```

The hook reads `{"command": …, "cwd": …}` and answers `{"permission": "allow" | "deny" | "ask", "userMessage": …,
"agentMessage": …}`. Denied commands tell the agent to use the Changeward tools.

## Use it

Open the project in Cursor, enable the `changeward` server under *Settings → MCP*, and ask the agent for a
change. It should call `propose_change`, then `apply_change`. `changeward log --all` shows the sessions.
