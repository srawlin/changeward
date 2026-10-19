# Renaming checklist

*Changegate* is a working name. The code uses exactly three case forms, with no `ChangeGate` or `changeGate`
anywhere, so a rename is mostly three replacements. These counts exclude generated code in `gen/`:

| Form | Hits | Files | Examples |
| --- | ---: | ---: | --- |
| `changegate` | 924 | 168 | binary, Go module path, packages, config file names, MCP server name, HTTP headers |
| `Changegate` | 260 | 95 | prose, type names (`ChangegateError`, `ChangegatePlugin`) |
| `CHANGEGATE` | 109 | 50 | environment variables (`CHANGEGATE_CONFIG`, …) |

Then rename paths: `cmd/changegate/`, `deploy/helm/changegate-executor/`, `proto/changegate/v1/`,
`sdk/python/src/changegate_sdk/`, `integrations/contextforge/changegate_contextforge/`, the toykv plugin binary
and the self-test workflow. List them with `git ls-files | grep -i changegate`.

## Things that need more than search and replace

| Item | Why it matters |
| --- | --- |
| Go module path `github.com/srawlin/changegate` | Rename the repository first, then update the module path and imports |
| Protobuf package `changegate.v1` | Regenerate `gen/` with `make proto` after renaming `proto/changegate/` |
| `_changegate_trash` schema | Databases that already hold soft-dropped objects need a migration (`ALTER SCHEMA … RENAME`) or a fallback lookup |
| Config file names `changegate.yaml`, `changegate.policy.yaml`, `.changegate/` | Read the old names as a fallback for one release |
| Environment variables `CHANGEGATE_*` | Accept the old names as a fallback for one release |
| PR/MR comment markers (`<!-- changegate-report -->`, `<!-- changegate-terraform -->`) | Old comments won't be found and updated, so a second comment appears once per open PR |
| Commit status context `changegate/rehearsal` | Required-status settings in GitHub branch protection and GitLab must be updated |
| MCP server name `changegate` | Agents' tool names change (`mcp__changegate__propose_change`), and so do Claude Code permission allow-lists |
| Hook and installer markers (`# >>> changegate` in `.codex/config.toml`, hook detection by binary name) | `install` should recognise and replace old entries |
| Gateway headers `x-changegate-*` | Gateway configs in the field |
| Token prefix `cgx_` | Optional; existing tokens stay valid because only their hashes are stored |
| Distribution names | npm `@changegate/cli` and `@changegate/sdk`, PyPI `changegate` and `changegate-sdk`, Homebrew `srawlin/tap/changegate`, `ghcr.io/srawlin/changegate`, Helm chart `changegate-executor` |
| Claude Code plugin marketplace (`.claude-plugin/marketplace.json`) | Plugin and marketplace names |
| External test fixtures | GitLab test project files, the Argo CD app in kind, GitHub test PRs |

A suggested order:

1. Rename the repository.
2. Run a scripted replace of the three forms.
3. Rename the paths.
4. Run `make proto`.
5. Add fallbacks for the old config file names and environment variables.
6. Run `make test lint`, and `scripts/gen-cli-docs.sh` to regenerate `docs/cli.md`.
