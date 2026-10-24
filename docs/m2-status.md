# M2 status

Status of the roadmap's M2 milestone ("Infrastructure breadth", [`agent-change-control-plan.md`](agent-change-control-plan.md) §14)
as of 2026-10-19. "Tested" means automated tests against real systems, run in CI on GitHub (`.github/workflows/ci.yml`):
Postgres 16/18, MySQL 8.4, a kind cluster, Terraform, and NGINX in Docker.

## Both lanes

| Scope item | Status | Where / notes |
| --- | --- | --- |
| Kubernetes: dry-run diff, undo handles | Done, tested (kind) | `adapters/kubernetes`. Server-side dry run diffed against live objects (managed fields and status stripped). Previous objects are the undo handle; create/update/delete/scale are restored exactly. Apply and undo are guarded by a desired-state hash, so controller status updates are not mistaken for drift. Rollout verification. Deleting a PVC, PV, Namespace or CRD is irreversible. Field-manager takeovers are warned about. |
| Kubernetes: GitOps-ownership detection | Done, tested | Argo CD tracking annotation and label; Flux kustomize/helm labels. Flagged in plans (`gitops_owner` fact, `gitops-owned` flag). |
| Argo CD / Flux hooks | Done, verified in Argo CD v3.5.5 (kind) | `changeward k8s gate` runs locally or on the executor (`POST /v1/gitops/gate`). Approvals bind to the exact plan of desired vs. live state, so a retried sync passes once approved. Hook resources (Argo CD/Helm) are left out of the plan. Verified: PreSync gate blocked a prod sync until an SRE approved it with `changeward approve --executor`; Argo's retry then passed (`integrations/argocd/README.md`). Flux guidance only (`integrations/flux`). |
| Terraform/OpenTofu classification | Done, tested (Terraform 1.16) | `adapters/terraform` (direct lane: plan file, exact-plan apply, stale-plan and edited-config conflicts, verify by empty re-plan, undo re-applies the last applied or git HEAD configuration, labelled compensatable). `changeward tf review --plan-json` in CI. Stateful destroy/replace is irreversible. |
| NGINX/Traefik | Done, tested (NGINX in Docker) | `adapters/configfile` presets nginx/traefik/haproxy. The candidate is validated with `nginx -t` without reloading. Atomic write, test, reload; a failing config is never reloaded. Health-checked verification is robust to asynchronous reloads (settle delay plus three consecutive healthy answers). Guarded undo. |
| OpenAPI + compensation manifests | Done, verified against the real GitHub API | `adapters/httpapi`: manifests declare a `before` read and an `inverse` call per operation. Drift guards cover only the fields the change writes or restores (`guard:` overrides). JSON numbers are kept exact. Unmatched calls are irreversible. `manifests/github-issues.yaml` was verified on srawlin/changeward: issue create, edit and comment, then undo of all three. |
| Changesets (saga) | Done, tested | Engine `ProposeChangeset` / `ApplyChangeset` (compensation in reverse) / `UndoChangeset`; `changeward changeset plan -f`, `apply`, `undo`; verified across Postgres plus a config file. |
| OTel export | Done, verified with OpenTelemetry Collector 0.163 | `internal/telemetry`: OTLP/HTTP JSON traces, one trace per change (an undo joins it), a span per transition, a root span. Opt-in via `telemetry.otlp_endpoint` or `$OTEL_EXPORTER_OTLP_ENDPOINT`. Attributes only, never plan contents. The root span is re-sent with a later end time after an undo. |
| MySQL adapter | Done, tested (MySQL 8.4) | `adapters/mysql`: TiDB parser; DML captured in a rolled-back InnoDB transaction; lossless hex images and SQL row hashes guard apply and undo; static DDL with soft-drop/soft-truncate into `_changeward_trash` and exact DROP COLUMN restore; passes the conformance suite; `changeward mcp mysql`. |

## Lane B

| Scope item | Status | Notes |
| --- | --- | --- |
| Shared executor (Helm) | Done, tested; Helm chart deployed to kind | `changeward executor serve`: hashed bearer tokens with roles (propose, approve, admin), session ownership, inline vs. out-of-band approval, separation of duties. `internal/backend` lets the MCP server run against it (`changeward mcp postgres --executor URL`). Helm chart (non-root, read-only rootfs, PVC) verified end to end in kind with the Python SDK. systemd unit. Single replica until the Postgres store (M3). |
| Coding-agent integrations | Codex, OpenCode and Gemini CLI verified in the real CLIs; Cursor not run | `changeward install codex` (`.codex/config.toml`: MCP server and PreToolUse hook `hook codex`), `install opencode` (`opencode.json` and a `tool.execute.before` plugin), `install gemini` (BeforeTool hook, JSON deny), `install cursor`, `install agents-md`. Verified: codex-cli 0.157, OpenCode 1.18 and Gemini CLI 0.64 used `propose_change` → `apply_change` themselves. In each CLI a raw `psql DELETE` was blocked by the hook or plugin (Codex then rerouted through Changeward). Cursor needs a Cursor account (not available). |
| Python/TS SDKs with framework integrations | Done, tested against a live executor | `sdk/python` (stdlib only) and `sdk/typescript` (fetch; strict `tsc`). Sessions, propose, apply, confirm, undo, `guarded` wrappers. LangGraph interrupt helper (tested with a stand-in `interrupt`), agent tool-body helpers (OpenAI Agents SDK, Claude Agent SDK, Vercel AI SDK), Claude Agent SDK `can_use_tool`. **Not run inside the real frameworks.** Not published. |
| Local web timeline | Done, tested | `changeward ui` (localhost only): sessions, diffs, decisions, signals, audit events, approve/reject/undo with a CSRF token and a strict CSP. |
| Promotion for Kubernetes manifests | Done, tested locally | `changeward promote --k8s-dir` writes desired-state manifests (deleted objects' files removed). |
| Reconciliation PRs | Done, tested locally | `changeward reconcile` (incident-linked PR and trailers); `changeward drift --max-age` exits 3 when direct staging/prod changes stay unreconciled. |

## Lane A

| Scope item | Status | Notes |
| --- | --- | --- |
| Gateways: Envoy, agentgateway, ContextForge | Done, verified in Envoy 1.36, agentgateway v1.6.0 and ContextForge 1.0.11 | Executor `POST /v1/authz/mcp` (any method; denials are JSON-RPC errors carrying the reason) and `POST /v1/authz/tool` (ContextForge plugin, `cpex` framework). Reads pass; SQL writes to DB tools are blocked; tools declared read-only (`x-changeward-read-only-tools`) pass and are audited; others are policy-decided and audited. Each gateway was verified in front of the reference "everything" MCP server. Envoy and agentgateway were driven by the official Python MCP client. |

## Exit criteria

- **Lane A:** "a design partner runs it on real agent PRs for 4 weeks with a measured auto-approval rate and zero
  unsafe merges." This needs a design partner. The tooling is in place: the Action reports and gates; the
  auto-approval rate and decisions are in the audit log and OTel export.
- **Lane B:** "a design partner's team uses shared-dev/staging direct mode for 4 weeks; median plan latency under
  1s; promote-to-PR used." This needs a design partner. Measured plan latency is about 30–100 ms on local Postgres
  (benchmark); promote-to-PR was verified on GitHub (PR #2).

## Jev in M2

`changeward sensitive suggest [--apply]` asks Jev, per column, whether the table/column name and type suggest personal
or secret data. Only schema names and types are sent, never row values. Suggestions can only add redaction. On a
test schema it flagged exactly the 7 sensitive columns (email, full_name, password_hash, last_login_ip, dob,
shipping_address, api_token; scores 0.96–0.99) and none of the 7 others. Two uses were considered and rejected
because they would let a model loosen a decision: classifying unannotated MCP tools, and choosing reversibility.

## Also done during M2

- Finished M1 verification on GitHub: CI on GitHub with all services; the Action on real PRs (comment, status, gate);
  `promote --pr` opened a PR that the Action rehearsed and auto-approved.
- Neon live: LSN-based checkpoints, restore, branch copies, `rehearse --neon` (T3).
- Module path is now `github.com/srawlin/changeward`.

## Known gaps

- **GitLab component:** verified on gitlab.com: MR notes, commit statuses, and report and gate modes. Until there are
  GitHub releases, the job downloads the binary from the project's generic package registry (`binary-url`). The
  `~/.ssh/id_ed25519` key is passphrase-protected, so access is over HTTPS with the project token.
- **Cursor:** not verified (needs a Cursor account). **Flux:** guidance only, not run in a real Flux.
- **ContextForge image:** `ghcr.io/ibm/mcp-context-forge` needs an x86-64-v3 CPU; it was verified from PyPI instead.
- **Executor store:** SQLite on one replica. HA needs the Postgres store (M3).
- **Kubernetes:** status is excluded from guards, but so are some controller-defaulted fields that appear in the
  dry run; diffs can show defaulted fields on first apply.
- **MySQL:** DDL is static (it cannot be dry-run); INSERT … SELECT, upserts and REPLACE are `checkpoint-only`;
  auto-increment capture assumes the consecutive ids MySQL gives simple multi-row inserts.
- **Gateway authz:** it can only block or allow; it cannot preview or undo, which is why agents should be routed to
  Changeward's MCP server.
