# OpenAI Codex CLI

Codex gets the Changegate MCP server for previewed, policy-checked, undoable changes. A PreToolUse hook blocks
raw `psql` writes. Codex hooks use the same wire format as Claude Code's.

**Verified:** codex-cli 0.157 (`codex exec`). Through the MCP server, Codex used `query` → `propose_change`
→ `apply_change` → `query`. With only the installed project config, it first tried a raw `psql DELETE`. The hook
blocked it, and Codex rerouted through `propose_change` / `apply_change` on its own.

## Set up

```sh
changegate connect app --dsn '${DATABASE_URL}' --env personal
changegate install codex
```

This writes a managed block into `.codex/config.toml`. Running `install` again replaces only that block.

```toml
# >>> changegate (managed by `changegate install codex`)
[mcp_servers.changegate]
command = "/usr/local/bin/changegate"
args = ["mcp","postgres","--target","app"]
env = { CHANGEGATE_AGENT = "codex" }

[[hooks.PreToolUse]]
matcher = "^Bash$"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "/usr/local/bin/changegate hook codex"
timeout = 30
# <<< changegate
```

Two one-time steps in Codex:

1. **Trust the project.** Codex loads `.codex/config.toml` only for trusted projects; it asks the first time you
   open the directory.
2. **Review the hook.** Run `/hooks` and trust the Changegate hook. Codex skips new or changed hooks until they
   are reviewed.

To use it in every project instead, put the same block in `~/.codex/config.toml`, with `--target` naming a target
in that project's `changegate.yaml`.

## Use it

```sh
codex "upgrade customers 1 to 5 to the pro tier"
codex exec "customer 20 closed their account; remove them"     # non-interactive
```

Codex calls `propose_change`, shows the plan, then calls `apply_change`. For automation, `codex exec` needs
`--dangerously-bypass-hook-trust` (or a managed hook in `requirements.toml`), because it can't open `/hooks`.

Codex's sandbox and Changegate do different jobs. In `workspace-write` mode, shell commands have no network, so a
raw `psql` fails anyway, but only by accident. The Changegate MCP server runs outside the sandbox and is the
supported path. With network access turned on (`sandbox_workspace_write.network_access = true`), the hook is
what stops direct writes.

Sessions appear in `changegate log --all` as `agent=codex-mcp-client/<version>`.

## Without install

The MCP server alone, one time:

```sh
codex exec -c 'mcp_servers.changegate.command="changegate"' \
  -c 'mcp_servers.changegate.args=["mcp","postgres","--target","app"]' "…"
```

Or `codex mcp add changegate -- changegate mcp postgres --target app`.

## Troubleshooting

- **Hook never runs:** the project isn't trusted, or the hook hasn't been reviewed in `/hooks`. Hooks passed with
  `-c` also need review, or `--dangerously-bypass-hook-trust`.
- **The model isn't supported with your login:** pass `-m` with a model available to your account.
- **`codex exec` waits for input:** redirect stdin (`< /dev/null`) in scripts.
