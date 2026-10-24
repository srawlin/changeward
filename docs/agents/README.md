# Coding agents and your own agents

| Agent | Guide | Verified in the real product |
| --- | --- | --- |
| Claude Code | [claude-code.md](claude-code.md) | ✅ Claude Code 2.1 (MCP flow and hook) |
| OpenAI Codex CLI | [codex.md](codex.md) | ✅ codex-cli 0.157 (MCP flow, and hook blocking with self-correction) |
| OpenCode | [opencode.md](opencode.md) | ✅ OpenCode 1.18 (MCP flow and plugin) |
| Gemini CLI | [gemini-cli.md](gemini-cli.md) | ✅ Gemini CLI 0.64 (MCP flow and hook) |
| Cursor | [cursor.md](cursor.md) | ⚠️ hook tested with sample payloads only |
| Claude Desktop, VS Code, Windsurf, Zed, other MCP clients | [mcp-clients.md](mcp-clients.md) | ⚠️ documented configs, not tested end to end |
| LangGraph, OpenAI Agents SDK, Claude Agent SDK, Vercel AI SDK | [sdks.md](sdks.md) | ✅ SDKs tested against a live executor |

Every agent gets the same model:

- The agent **proposes**. Changeward **plans** with an exact dry run, and **policy decides**.
- Changeward **applies** with an **undo** path.
- A hook or plugin stops the agent from going around Changeward with raw `psql`, `kubectl` or `terraform`.
