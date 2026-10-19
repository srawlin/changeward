# M2 status

Status of the roadmap's M2 milestone ("Infrastructure breadth", [`agent-change-control-plan.md`](agent-change-control-plan.md) §14)
as of 2026-10-19. "Tested" means automated tests against real systems, run in CI on GitHub (`.github/workflows/ci.yml`):
Postgres 16/18, MySQL 8.4, a kind cluster, Terraform, and NGINX in Docker.

## Both lanes

| Scope item | Status | Where / notes |
| --- | --- | --- |
| Kubernetes: dry-run diff, undo handles | Done, tested (kind) | `adapters/kubernetes`. Server-side dry run diffed against live objects (managed fields and status stripped). Previous objects are the undo handle; create/update/delete/scale are restored exactly. Apply and undo are guarded by a desired-state hash, so controller status updates are not mistaken for drift. Rollout verification. Deleting a PVC, PV, Namespace or CRD is irreversible. Field-manager takeovers are warned about. |
| Kubernetes: GitOps-ownership detection | Done, tested | Argo CD tracking annotation and label; Flux kustomize/helm labels. Flagged in plans (`gitops_owner` fact, `gitops-owned` flag). |
| Argo CD / Flux hooks | Done (gate tested on kind; PreSync Job manifest provided) | `changegate k8s gate` runs locally or on the executor (`POST /v1/gitops/gate`). Approvals bind to the exact plan of desired vs. live state, so a retried sync passes once approved. `integrations/argocd/presync-gate.yaml`; Flux guidance in `integrations/flux`. **The PreSync Job was not run inside a real Argo CD.** |
| Terraform/OpenTofu classification | Done, tested (Terraform 1.16) | `adapters/terraform` (direct lane: plan file, exact-plan apply, stale-plan and edited-config conflicts, verify by empty re-plan, undo re-applies the last applied or git HEAD configuration, labelled compensatable). `changegate tf review --plan-json` in CI. Stateful destroy/replace is irreversible. |
| NGINX/Traefik | Done, tested (NGINX in Docker) | `adapters/configfile` presets nginx/traefik/haproxy. The candidate is validated with `nginx -t` without reloading. Atomic write, test, reload; a failing config is never reloaded. Health-checked verification is robust to asynchronous reloads (settle delay plus three consecutive healthy answers). Guarded undo. |
| OpenAPI + compensation manifests | Done, tested (fake API) | `adapters/httpapi`: manifests declare a `before` read and an `inverse` call per operation, with drift and stale-undo guards; unmatched calls are irreversible. Starter `manifests/github-issues.yaml` (**not exercised against the real GitHub API**). |
| Changesets (saga) | Done, tested | Engine `ProposeChangeset` / `ApplyChangeset` (compensation in reverse) / `UndoChangeset`; `changegate changeset plan -f`, `apply`, `undo`; verified across Postgres plus a config file. |
| OTel export | Done, tested | `internal/telemetry`: OTLP/HTTP JSON traces (one trace per change, a span per transition, a root span); opt-in via `telemetry.otlp_endpoint` or `$OTEL_EXPORTER_OTLP_ENDPOINT`; attributes only, never plan contents. **Not tested against a real collector.** |
| MySQL adapter | Done, tested (MySQL 8.4) | `adapters/mysql`: TiDB parser; DML captured in a rolled-back InnoDB transaction; lossless hex images and SQL row hashes guard apply and undo; static DDL with soft-drop/soft-truncate into `_changegate_trash` and exact DROP COLUMN restore; passes the conformance suite; `changegate mcp mysql`. |

## Lane B

| Scope item | Status | Notes |
| --- | --- | --- |
| Shared executor (Helm) | Done, tested; Helm chart deployed to kind | `changegate executor serve`: hashed bearer tokens with roles (propose, approve, admin), session ownership, inline vs. out-of-band approval, separation of duties. `internal/backend` lets the MCP server run against it (`changegate mcp postgres --executor URL`). Helm chart (non-root, read-only rootfs, PVC) verified end to end in kind with the Python SDK. systemd unit. Single replica until the Postgres store (M3). |
| Codex / Cursor / Gemini CLI integrations | Done (hooks tested with sample payloads) | `changegate hook cursor` (beforeShellExecution JSON) and `hook gemini` (BeforeTool, exit 2 blocks); `install cursor|gemini` write MCP and hook config; `install codex` prints the MCP config; `install agents-md` adds instructions. **Not verified inside the real Cursor, Gemini CLI or Codex applications.** |
| Python/TS SDKs with framework integrations | Done, tested against a live executor | `sdk/python` (stdlib only) and `sdk/typescript` (fetch; strict `tsc`). Sessions, propose, apply, confirm, undo, `guarded` wrappers. LangGraph interrupt helper (tested with a stand-in `interrupt`), agent tool-body helpers (OpenAI Agents SDK, Claude Agent SDK, Vercel AI SDK), Claude Agent SDK `can_use_tool`. **Not run inside the real frameworks.** Not published. |
| Local web timeline | Done, tested | `changegate ui` (localhost only): sessions, diffs, decisions, signals, audit events, approve/reject/undo with a CSRF token and a strict CSP. |
| Promotion for Kubernetes manifests | Done, tested locally | `changegate promote --k8s-dir` writes desired-state manifests (deleted objects' files removed). |
| Reconciliation PRs | Done, tested locally | `changegate reconcile` (incident-linked PR and trailers); `changegate drift --max-age` exits 3 when direct staging/prod changes stay unreconciled. |

## Lane A

| Scope item | Status | Notes |
| --- | --- | --- |
| agentgateway and ContextForge integrations | Done, tested against a live executor | Executor `POST /v1/authz/mcp`: agentgateway external authz, Envoy `ext_authz` HTTP (token in `x-changegate-token`; appended path accepted). `POST /v1/authz/tool` serves the ContextForge plugin (`integrations/contextforge`, tested with stubbed framework classes). Reads pass, SQL writes to DB tools are blocked in favour of Changegate's tools, other calls are policy-decided and audited. **Not run inside real agentgateway, Envoy or ContextForge;** the agentgateway field names in the example need checking against its documentation. |

## Exit criteria

- **Lane A:** "a design partner runs it on real agent PRs for 4 weeks with a measured auto-approval rate and zero
  unsafe merges." This needs a design partner. The tooling is in place: the Action reports and gates; the
  auto-approval rate and decisions are in the audit log and OTel export.
- **Lane B:** "a design partner's team uses shared-dev/staging direct mode for 4 weeks; median plan latency under
  1s; promote-to-PR used." This needs a design partner. Measured plan latency is about 30–100 ms on local Postgres
  (benchmark); promote-to-PR was verified on GitHub (PR #2).

## Also done during M2

- Finished M1 verification on GitHub: CI on GitHub with all services; the Action on real PRs (comment, status, gate);
  `promote --pr` opened a PR that the Action rehearsed and auto-approved.
- Neon live: LSN-based checkpoints, restore, branch copies, `rehearse --neon` (T3).
- Module path is now `github.com/srawlin/changegate`.

## Known gaps

- **GitLab:** the provided SSH key (`SHA256:cvH5hC…acvGI`) is rejected by gitlab.com. Add `~/.ssh/id_ed25519.pub`
  under GitLab → Preferences → SSH Keys. Posting MR notes from the component also needs a project access token
  (`api` scope) as `CHANGEGATE_GITLAB_TOKEN`.
- **Executor store:** SQLite on one replica. HA needs the Postgres store (M3).
- **Kubernetes:** status is excluded from guards, but so are some controller-defaulted fields that appear in the
  dry run; diffs can show defaulted fields on first apply.
- **MySQL:** DDL is static (it cannot be dry-run); INSERT … SELECT, upserts and REPLACE are `checkpoint-only`;
  auto-increment capture assumes the consecutive ids MySQL gives simple multi-row inserts.
- **Gateway authz:** it can only block or allow; it cannot preview or undo, which is why agents should be routed to
  Changegate's MCP server.
