# Other MCP clients

Any MCP client that can launch a stdio server can use Changegate. You get the tools and the preview → policy →
apply → undo flow. Clients without hooks can still run raw commands, so pair this with database credentials the
agent doesn't have (see [executor.md](../executor.md)), or wrap their existing database server with
`changegate proxy`.

## The server

```sh
changegate mcp postgres --target app      # or: mcp mysql
```

Flags:

- `--executor URL`: use a shared executor, with the token in `$CHANGEGATE_EXECUTOR_TOKEN`.
- `--checkpoint`: checkpoint the target when the session starts.
- `--branch`: work on a private copy of the target.
- `--session ID`: attach to an existing session.

The server must run in a directory that contains `changegate.yaml` (or a child of it), or set
`CHANGEGATE_CONFIG=/path/to/changegate.yaml` in its environment.

**Tools:**

| Tool | Does |
| --- | --- |
| `query` | Read-only SQL, row-capped; sensitive columns redacted |
| `list_tables` | Tables with row estimates |
| `propose_change` | Plan a write: exact preview, reversibility and policy decision. Nothing changes. |
| `apply_change` | Apply a planned change. It asks for inline confirmation if policy allows that, or says who must approve. |
| `get_change_status` | State of a change (e.g. after an out-of-band approval) |
| `undo_change` | Undo, dependency-aware |
| `list_session_changes` | This session's timeline |
| `checkpoint` | Snapshot the target |

## Client configuration

These snippets follow each client's documented MCP format. They have not been tested end to end like the
coding agents in this folder.

**Claude Desktop** (`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS):

```json
{ "mcpServers": { "changegate": { "command": "/usr/local/bin/changegate",
  "args": ["mcp", "postgres", "--target", "app"],
  "env": { "CHANGEGATE_CONFIG": "/Users/me/projects/shop/changegate.yaml", "CHANGEGATE_AGENT": "claude-desktop" } } } }
```

**VS Code** (GitHub Copilot agent mode, `.vscode/mcp.json`):

```json
{ "servers": { "changegate": { "type": "stdio", "command": "changegate", "args": ["mcp", "postgres", "--target", "app"],
  "env": { "CHANGEGATE_AGENT": "vscode" } } } }
```

**Windsurf** (`~/.codeium/windsurf/mcp_config.json`) and other clients that use the `mcpServers` shape: the same
as Claude Desktop.

**Zed** (`settings.json`):

```json
{ "context_servers": { "changegate": { "command": "changegate", "args": ["mcp", "postgres", "--target", "app"] } } }
```

Clients that support MCP elicitation, like Claude Code, show inline approval prompts. Others report that
approval is needed, and you approve with `changegate approve`.

## Agents without MCP: AGENTS.md and the generic hook

`changegate install agents-md` appends a section to `AGENTS.md`. It tells any agent that reads it to use the
Changegate tools or `changegate exec`, and never `changegate approve`.

For agents with a pre-command hook mechanism of their own, call the generic check. It exits with 2 when the
command must not run:

```sh
changegate hook check "psql -d app -c 'DELETE FROM orders'"          # exit 2, reason on stderr
changegate hook check --json "kubectl apply -f deploy.yaml"           # {"permission": "...", "reason": "..."}
```

## Wrapping an existing database MCP server

If your agent already uses a database MCP server, put Changegate in front of it:

```sh
changegate proxy --target app -- npx -y @modelcontextprotocol/server-postgres "$DATABASE_URL"
```

Reads pass through to the upstream server. SQL writes (`execute_sql`, `query` and the like) are planned, decided
and applied by Changegate. Other tools that aren't read-only need approval under `--env` policy.
