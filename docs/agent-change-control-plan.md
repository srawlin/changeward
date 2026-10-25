# Agent change control: technical plan

---

## 0. Decisions at a glance

| Decision | Choice | Main reason |
| --- | --- | --- |
| **What we build** | One **change engine** with two **first-class** lanes that share policy, evidence, undo and audit:<br>**Lane A, CI/CD:** agent-authored pull/merge requests and deployments.<br>**Lane B, direct:** agents acting on live systems through MCP, hooks or SDKs, from a developer's laptop to production break-glass. | Most *enterprise production* change still flows through Git and CI/CD to separate target systems (Lane A). But most *agent activity*, especially individuals building and experimenting, is already direct (Lane B), and that share will grow as agents operate continuously. Lane B is also where today's tools are weakest. Neither lane is an add-on to the other. |
| **How the lanes connect** | **Promotion and reconciliation:** direct changes made in development can be *promoted* into a PR as code (migration files, manifests, IaC) with their evidence attached. Direct changes made in production (break-glass) automatically open a *reconciliation* PR so Git stays the source of truth. | Lets people work fast in Lane B without creating drift, and turns Lane B experiments into reviewed Lane A changes. |
| **The idea that ties it together** | **Rollback rehearsal:** every agent change must *prove* what it will do and that it can be undone *before* it reaches production. | Turns "trust the agent" into evidence. Lets risk-based routing replace blanket human review, which can't keep up with the volume of agent-written changes. |
| **Core language** | **Go** | Single static binary (air-gapped installs, CI runners, sidecars); fits the ecosystems we integrate with (Kubernetes, Terraform/OpenTofu, OPA, Docker MCP Gateway, Coder, our own Foundry); official MCP Go SDK; strong Postgres tooling (pgx, pg_query_go); a proven plugin model (hashicorp/go-plugin). |
| **Other languages** | **TypeScript and Python** for thin SDKs, agent-framework integrations and `npx`/`uvx` launchers. **No Rust** in v1. | Agent builders live in Python and TypeScript. MCP configs install via `npx`/`uvx`. Rust adds little that Go lacks here and shrinks the contributor pool. |
| **Adapter model** | Out-of-process plugins over **gRPC/protobuf** (Terraform-provider style) plus a published **conformance suite** | Adapters can be written in any language. Commercial adapters ship as separate signed binaries, a clean open-core boundary. Community adapters can be certified. |
| **Policy** | **CEL** built in; optional **OPA/Rego** and **Cedar** bridges | CEL is used by Kubernetes and agentgateway, so it's familiar. Enterprises already standardized on OPA or Cedar can keep them. |
| **Storage** | SQLite (CLI/local) → Postgres (server); append-only change log with a hash chain | Zero-setup start; tamper-evident audit for regulated buyers. |
| **License** | **Apache-2.0** for core, CLI, spec, SDKs and open-source-system adapters. **Commercial** license for the enterprise server features and adapters for proprietary systems. | Maximizes adoption and embedding by gateway, CI and agent vendors (AGPL deters them). Monetizes what enterprises pay for: proprietary systems, governance and compliance. |
| **First thing shipped** | **Postgres**, in both lanes, released together:<br>(A) a GitHub Action / GitLab CI component that rehearses agent-written **migrations** (apply → verify → rollback → compare) and posts an effect report on the PR;<br>(B) a local-first MCP server/proxy and coding-agent hooks that turn live agent SQL into reviewable diffs, with a session timeline, one-command undo and promote-to-PR. | The database is where agent mistakes are most visceral (the July 2025 Replit incident) and where dry-run and undo are technically cleanest. Lane B reaches individual developers immediately via `npx`/`uvx` and MCP registries; Lane A reaches teams via the GitHub/GitLab marketplaces. |

---

## 1. Reframing the problem: two ways change happens

Change reaches systems in two ways, and agents use both:

- **Lane A, the pipeline:** changes are artifacts in Git, promoted through CI/CD to separate target systems (§1.1). This is how most enterprises govern *production*.
- **Lane B, direct:** an agent acts on a live system itself: a developer's local or branch database, a dev cluster, a staging environment, a SaaS tenant, or production during an incident (§1.2). This is how most *agent work* happens day to day, particularly for individuals building things.

Both are first-class in this design: same engine, same policy language, same evidence, same undo and audit. §1.4 compares them side by side.

### 1.1 Lane A: how enterprises govern production today

In a typical enterprise, nobody edits production directly outside emergencies. Changes are **artifacts in Git** that a **pipeline** promotes through environments to **separate target systems**:

```
 Developer or agent                Source control              CI                         CD / deploy                     Target systems
 ────────────────                  ──────────────              ──                         ───────────                     ──────────────
 code, migrations,      PR / MR    GitHub / GitLab    checks   build, test, lint,  merge  GitHub Actions envs,     creds  Databases (prod network)
 IaC, k8s manifests,  ─────────►   protected branch ───────►   terraform plan,    ──────► GitLab environments,   ─────►  Kubernetes clusters
 config, flags                     CODEOWNERS review           migration lint,            Argo CD / Flux (GitOps),        Cloud accounts
                                   required status checks      security scans             Atlantis / HCP TF / Spacelift,  VMs, proxies, SaaS admin
                                                                                          Harness / Spinnaker / Octopus
                                        ▲                                                     │
                                        │                   ITSM: ServiceNow / Jira Service Management
                                        └──── change request, CAB, change windows, freezes ───┘
```

What this means for the design:

- **Credentials live in the deploy path, not with the developer or agent.** Deploy jobs run on runners or controllers inside or next to the production network, using OIDC federation, vault-issued credentials or GitOps controllers that pull.
- **Approval is already layered:**
  - code review (CODEOWNERS, required reviewers)
  - pipeline gates (required checks)
  - deployment gates (GitHub environment protection rules and custom deployment protection rules; GitLab protected environments and deployment approvals)
  - change management (ServiceNow "standard", "normal" and "emergency" changes, CAB meetings, change windows)
- **Each type of change has its own tooling:**

| Change type | Usual artifact | Typical tooling | Built-in preview | Built-in rollback |
| --- | --- | --- | --- | --- |
| Database schema | Migration files | Flyway, Liquibase, Atlas, Sqitch, Prisma/Rails/Django/Alembic migrations, Bytebase; gh-ost/pg-osc for online changes | Partial (Atlas plan/lint, Liquibase `updateSQL`) | Down migrations, often missing or untested |
| Database data | One-off scripts, tickets to DBAs | Bytebase, ad-hoc SQL, admin consoles | Rare | Rare (restore from backup) |
| Infrastructure | Terraform/OpenTofu, Pulumi, CloudFormation | Atlantis, HCP Terraform, Spacelift, env0 | `plan` | Re-apply previous state (fragile for destroys) |
| Kubernetes | Manifests, Helm, Kustomize | Argo CD, Flux, Helm | `kubectl diff`, server-side dry-run, Argo diff | Git revert, Helm rollback, Argo Rollouts abort |
| App releases | Images, packages | CD tools, progressive delivery | Canary analysis | Previous release, feature-flag kill switch |
| Config / proxies | Files, templates | Ansible, Salt, Chef, NixOS, config repos | `--check --diff`, `nginx -t`, NixOS `build`/`dry-activate` | Re-apply previous, NixOS generations |
| SaaS admin | Clicks, scripts, some config-as-code | Vendor consoles, Terraform providers, scripts | Rare (sandboxes) | Rare |

### 1.2 Lane B: how agents (and people) actually work

Most of an agent's working day is direct. Git is not in the loop for each step, and often not at all:

- **Individuals building.** A developer asks a coding agent to "add a column and backfill it", "seed test data", "fix the dev cluster" or "clean up these records". The agent runs `psql`, `kubectl` or a database MCP tool against a local, branch or shared dev system, iterates quickly, and only later (maybe) turns the result into a PR.
- **Operations agents.** Incident response ("scale this deployment", "kill that query"), data fixes ("re-run these failed orders") and routine admin happen on live systems because waiting for a pipeline is too slow or the change isn't code.
- **Systems with no Git representation.** Business data, SaaS tenants (Jira, Salesforce, ServiceNow), identity admin and tickets usually have no config-as-code, so direct is the *only* lane.
- **Continuous agents.** Agents that run all day and act within a budget (a support agent issuing refunds, a FinOps agent resizing resources) make many small direct changes. Routing each through a PR is impractical.

**Why Lane B's share will grow:** agents work faster than PR review cycles, operate continuously, and increasingly touch systems that aren't code. Developer tools are already moving this way, with agents connected directly to databases, clusters and SaaS through MCP. Today's safeguards here are mostly approve/deny prompts and local file checkpoints. Nothing previews effects on live systems, records a timeline of what changed, or undoes it.

**What Lane B needs that Lane A doesn't:**

- **Speed:** plans in about a second, not a CI run. Low-risk changes in personal and dev environments must not block.
- **A session view:** a timeline of every change an agent made in one working session, with undo of any step and "rewind to the start".
- **Graduated strictness by environment:** permissive on a laptop, strict in production, with no reconfiguration in between.
- **A path back to Git:** promote what worked into a reviewed PR (§6.5).

### 1.3 What agents change about this

1. **Volume.** Coding agents (Copilot coding agent, Claude Code and Codex in CI, Cursor background agents, Coder Agents and others) generate far more PRs than humans can review carefully. **The review queue is the bottleneck.** Approving everything by hand doesn't scale, and rubber-stamping is unsafe.
2. **New artifact risk.** Agents write migrations, IaC and manifests that *look* plausible. The dangerous part is the *effect*: a column dropped, a table locked for 20 minutes, a security group opened, a PVC deleted. Reviewers of a diff in Git don't see the effect on production.
3. **Rollbacks are rarely tested.** Down migrations and rollback paths are the least-tested code in most organizations. Agents writing them doesn't help unless someone executes them.
4. **Direct action is mainstream, not marginal.** Developer agents with database, Kubernetes and SaaS MCP servers, incident-response agents and data-fix agents all act on *live systems*, bypassing Git (§1.2). Today's host-level safeguards are mostly **approve/deny prompts**, for example Codex's approval modes driven by MCP tool annotations, or permission prompts in coding agents. Claude Code-style checkpoints undo *local file edits*, not database or infrastructure changes.
5. **Provenance matters.** Enterprises want to know which changes were agent-authored, by which agent, on whose behalf, and apply different policy accordingly.

### 1.4 The two lanes side by side

| | **Lane A: CI/CD** | **Lane B: direct** |
| --- | --- | --- |
| Typical user | Teams shipping to production; coding agents opening PRs | Individuals building with agents; ops, data-fix, SaaS-admin and continuous agents |
| Where the change lands | Separate target systems, via the existing CD tool | The system the agent is connected to: local, branch, dev, staging, SaaS, prod (break-glass) |
| Unit of work | Pull/merge request → deployment | Session → individual actions |
| Latency budget | Minutes (a CI run) | About a second to plan; non-blocking approvals |
| How it's rehearsed | On a separate copy, tiers T0–T3 (§5.3) | Live dry-run in a rolled-back transaction, or a branch/copy above size or risk thresholds (§5.3, §7.1) |
| Approval | Risk-routed: auto-approve, CODEOWNERS/DBA/SRE reviewers, ITSM change request | Risk- and environment-routed: auto in personal/dev, inline prompt, or out-of-band (Slack/web/ITSM) in prod |
| Who applies | The existing CD tool; Changeward gates and verifies | Changeward's executor |
| Credentials | Already in the deploy path (OIDC, vault, GitOps) | Held by the executor, never the agent: local credentials in personal mode, just-in-time credentials in shared/prod |
| Undo | Rehearsed rollback path (down migration, revert, Helm/Argo rollback, flag off) | Per-action undo handle, session rewind, cross-system checkpoints (§7.7) |
| Evidence and audit | PR comment, check run, change record, ITSM ticket | Session timeline, change records, same audit chain |
| Connection to the other lane | Receives promoted and reconciliation PRs from Lane B | Promote to PR; prod break-glass auto-opens a reconciliation PR (§6.5) |
| Distribution | GitHub Marketplace, GitLab CI/CD Catalog | `npx`/`uvx`, MCP registries, coding-agent plugins |

### 1.5 The product's job

> For every agent-originated change, whether it arrives as a PR or a live action, produce a **verified effect plan**, a **reversibility class**, and a **tested undo path**. Route approval by **risk and environment** rather than by default. Gate deployment on that evidence in Lane A; apply through a credential-holding executor in Lane B. Verify the outcome, and roll back automatically when verification fails.

The value to sell an enterprise: **agents can make more changes, in either lane, without more change failures**, measured with DORA's change failure rate and time to restore. In Lane A, security and change-management teams can treat verified low-risk agent changes as pre-approved *standard changes*, reducing CAB load. In Lane B, they can allow agents direct access to more systems, because every action is bounded, recorded and undoable.

The value to an individual developer: **let the agent touch real systems without fear.** Every change is previewed, logged on a timeline, and undoable in one command.

---

## 2. Prior art and the gap

| Area | Existing tools | What they do well | Gap we fill |
| --- | --- | --- | --- |
| Migration linting / review | Atlas (`migrate lint`, CI integrations), Squawk (Postgres migration linter), Bytebase (database DevOps, SQL review, approvals), Liquibase/Flyway enterprise editions | Static rules (dangerous DDL, lock risk), approval workflows for DBAs | **Running** forward + rollback against a production-like copy and comparing state; agent provenance; one effect report across DB + IaC + k8s; deployment-gate integration |
| IaC plan review | Atlantis, HCP Terraform, Spacelift, env0, Infracost, Conftest/OPA | `plan` in the PR, policy as code, cost | Reversibility classification (e.g. "destroys a stateful resource"), rollback rehearsal, cross-artifact view, agent-specific policy |
| Kubernetes GitOps | Argo CD, Flux, Argo Rollouts, Kyverno, Gatekeeper | Diff, sync, progressive delivery, admission policy | Evidence that the agent-authored change was rehearsed, and an undo plan attached to the change record |
| Deployment gates | GitHub custom deployment protection rules (GitHub Apps); GitLab deployment approvals and external status checks (paid tiers; verify current tiers) | Hooks to block deploys pending external checks | Supplying the evidence and risk decision those hooks need |
| Change management | ServiceNow Change, Jira Service Management, PagerDuty | CAB, windows, audit | Machine-generated, evidence-backed change records; automatic standard-change classification |
| Agent runtime safety | Host approval prompts (Codex approval modes, MCP `destructiveHint`), gateway allow/deny policy (agentgateway CEL, Kong, ContextForge), Agent Firewall-style egress control, local file checkpoints | Blocking or allowing | **Previewing effects, staging, verifying and undoing** non-file changes on live systems |
| Database branching, cloning and masking | Neon, Supabase branching, Aurora cloning, Postgres template databases, DBLab Engine (thin clones, CI migration checks), Greenmask / PostgreSQL Anonymizer / Tonic (masking), Dolt | Cheap copies; masked test data | Using them as tiered *rehearsal environments* inside a change workflow (§5.3) |

Nobody combines **agent provenance + effect rehearsal + reversibility + risk-routed approval + deployment gating + undo** across multiple systems. Each of those exists alone, so our job is integration, and the moat is adapter depth and evidence quality.

---

## 3. Core concepts

| Concept | Definition |
| --- | --- |
| **Change** | One intended mutation of one target, such as a migration, a `terraform apply`, an SQL `UPDATE` or a k8s manifest. It carries intent (free text plus a structured operation), target, principal (agent + delegating human), and provenance (PR, commit, session). |
| **Changeset** | An ordered group of Changes applied together, possibly across adapters (migration + app deploy + feature flag). Applied as a **saga**: if step *k* fails, steps *k−1…1* are undone in reverse. |
| **Plan** | An adapter-computed, structured **effect preview**: row diffs, DDL effects and lock estimates, resource create/update/destroy, object diffs. It records **preconditions** (the state the plan assumes) and is content-hashed. |
| **Rehearsal** | Executing the plan, then its undo, in a **production-like copy** (DB branch, ephemeral namespace, emulator, Coder workspace), with verification and a state comparison afterwards. Produces evidence. |
| **Reversibility class** | `reversible` (exact undo tested), `compensatable` (a semantic undo exists, e.g. a refund or recreating a record with a new ID), `checkpoint-only` (undo only by restoring a snapshot), `irreversible` (sent email, destroyed data with no snapshot). Drives policy. |
| **Risk score** | Derived from adapter facts: blast radius (rows, resources), destructiveness, lock or downtime estimate, environment, data sensitivity, reversibility class, rehearsal result, principal trust. |
| **Approval** | A decision bound to the **exact plan hash**. Approving one plan never authorizes a different one, which prevents "approve, then swap". |
| **Receipt** | What actually happened at apply time: what ran, outputs, timing, and an **undo handle** (before-images, previous object versions, snapshot IDs, compensation calls). |
| **Verification** | Post-conditions checked against the *target system*, not the agent's claim: row counts, health checks, rollout status, SLO probes. |
| **Checkpoint (C12)** | A coordinated, cross-layer snapshot (DB branch or template, volume snapshot, git ref, workspace) tied to a Changeset or Session, used for `checkpoint-only` restore and for branching parallel attempts. |
| **Session** (Lane B) | The sequence of Changes made by one agent run or working session, with an optional checkpoint at the start. It's the unit of the timeline view, `rewind`, and promotion. The Lane B equivalent of a PR. |
| **Environment class** | `personal`, `shared-dev`, `staging`, `prod`, `prod-restricted` (e.g. PCI). Set per target connection. Drives default policy, rehearsal tier and approval channel in both lanes. |
| **Promotion** | Converting a Session's changes into code artifacts (migration files, manifests, IaC) and opening a PR with the session's evidence attached. Lane B → Lane A. |
| **Reconciliation** | After a direct change to a Git-managed system (e.g. prod break-glass), automatically opening a PR that brings Git back in line with reality. Also Lane B → Lane A. |

The **Change Record** schema (JSON Schema and protobuf) is published as an open specification. Later it could be proposed as an MCP extension through MCP's proposal process, so hosts and gateways can carry plan, approval and undo semantics in a standard way.

---

## 4. Architecture

### 4.1 Components

```
                          ┌──────────────────────────────────────────────────────────────────┐
                          │                Changeward control plane (server mode)            │
                          │  API (REST + gRPC) · Change/Changeset state machine · Policy (CEL)│
                          │  Approval broker · Risk engine · Evidence store · Audit hash chain│
                          │  Integrations: GitHub App, GitLab app, ServiceNow, Slack/Teams     │
                          └───────▲───────────────────────▲──────────────────────────▲───────┘
                                  │ outbound only          │                          │
             Lane A (CI/CD)       │                        │ Lane B (direct)          │ Rehearsal env providers
   ┌──────────────────────────────┴───┐      ┌─────────────┴───────────────┐   ┌──────┴─────────────────────────┐
   │ CI job / deploy job              │      │ Agent host                  │   │ DB branch (Neon/Supabase/      │
   │  changeward CLI (GitHub Action,  │      │  MCP proxy / MCP server     │   │   pg template), ephemeral k8s  │
   │  GitLab CI component)            │      │  Claude Code / Codex / Cursor│   │   namespace, emulators (Jira,  │
   │  → plan · rehearse · report ·    │      │   hooks · Python/TS SDK     │   │   Maximo), Coder workspaces    │
   │    gate · apply · verify         │      └─────────────┬───────────────┘   └────────────────────────────────┘
   └──────────────┬───────────────────┘                    │
                  │                                        │
          ┌───────▼────────────────────────────────────────▼────────┐
          │ Executor (runs where the credentials and targets are:    │
          │ deploy runner, prod-network sidecar, k8s operator)       │
          │  loads adapter plugins (gRPC): postgres · git · k8s ·    │
          │  terraform · nginx · openapi · [commercial: servicenow,  │
          │  oracle, sqlserver, snowflake, salesforce, jira, maximo]│
          └──────────────────────────────────────────────────────────┘
```

**Design rules:**

1. **The executor goes where the credentials go.** In enterprises that means a CI deploy runner, a sidecar in the production network, or a Kubernetes operator. **The agent never holds target credentials** in the direct lane; the executor does. This alone is a strong security property.
2. **Executors only dial out** to the control plane (the same model as Coder's workspace agent, GitHub self-hosted runners and Argo agent mode). No inbound ports in production.
3. **Everything also runs without a server:** `changeward` in a CI job or on a laptop uses SQLite and local policy files. Server mode adds shared approvals, an organization-wide audit trail and the UI.
4. **It is Git-native where Git is the source of truth.** In Lane A we never apply changes the pipeline would apply anyway. We *produce evidence and gate*, and the existing CD tool still deploys. Integrate, don't replace.
5. **Lane B is not a bypass.** Direct actions go through the same engine, policy, evidence and audit chain as Lane A. When a direct change touches a Git-managed system, reconciliation (§6.5) keeps Git the source of truth.
6. **Local-first.** In personal mode the executor runs on the developer's machine, inside the MCP server or hook process, with the developer's own credentials and a local SQLite store. No server, account or network connection is needed. The same binary scales up to shared executors and server mode without changing how the agent calls it.

### 4.2 The state machine (shared by both lanes)

```
proposed → planned → rehearsed ─┬─► auto-approved ─┐
                                ├─► awaiting-approval ─► approved ─┤
                                └─► rejected                       ▼
                                                     applying → applied → verifying ─┬─► verified (done)
                                                                                     └─► failed ─► undoing → undone
                                                                       (manual: applied/verified → undoing → undone)
```

Every transition is an append-only event: who or what triggered it, plan hash, evidence references, policy decision and reason. That event log *is* the audit product.

The lanes differ only in how the machine is driven. In Lane A, CI events (PR opened, merge, deployment requested) drive it, and `applying` is performed by the existing CD tool. In Lane B, agent tool calls drive it. `awaiting-approval` returns immediately to the agent with a plan ID ("pending approval") rather than blocking, so the agent can continue other work or poll.

### 4.3 The adapter contract (gRPC, sketch)

```proto
service Adapter {
  rpc Describe(DescribeRequest)   returns (AdapterInfo);      // kinds handled, capabilities
  rpc Classify(Operation)         returns (Classification);   // read | write | destructive; target; sensitivity
  rpc Plan(PlanRequest)           returns (Plan);              // effects, preconditions, lock/downtime estimate
  rpc Rehearse(RehearseRequest)   returns (RehearsalResult);   // apply+verify+undo+compare in a provided copy
  rpc Apply(ApplyRequest)         returns (Receipt);           // checks preconditions; returns undo handle
  rpc Verify(VerifyRequest)       returns (Verification);      // post-conditions against the live target
  rpc Undo(UndoRequest)           returns (Receipt);           // exact inverse or compensation
  rpc Checkpoint(CheckpointReq)   returns (CheckpointRef);     // optional (C12)
  rpc Restore(RestoreRequest)     returns (Receipt);           // optional (C12)
  rpc ProvisionCopy(CopyRequest)  returns (CopyRef);           // optional: branch/clone for rehearsal
}
```

- **Capabilities are declared,** not assumed. An adapter may support only `Classify` and `Plan`, for example an approval-only adapter for an unknown SaaS. Policy uses capabilities to decide what can be auto-approved.
- **Plugin transport** uses hashicorp/go-plugin (MPL-2.0, used as a library) or plain gRPC over a Unix socket. Adapters can be written in Go, TypeScript or Python.
- A **conformance suite** (`changeward adapter test`) proves `Apply` then `Undo` restores identical state across a corpus of generated operations. Passing adapters get a "certified" badge, which gives community contributors a reason to submit.

---

## 5. Lane A: CI/CD (GitHub and GitLab)

### 5.1 The flow

```
Agent opens PR/MR ──► changeward detect (provenance: bot account, commit trailers, signed attestations)
        │
        ▼
CI: changeward plan        ── discovers artifacts in the diff: migrations, *.tf, k8s manifests, nginx conf, flag changes
CI: changeward rehearse    ── provisions copies (DB branch from prod snapshot or masked clone, ephemeral namespace,
                              plan-only for terraform) → apply → verify → undo → compare → tear down
CI: changeward report      ── PR comment + check run: effects, risk, reversibility, rehearsal evidence, required approvers
        │
        ▼
Policy decision            ── low risk + reversible + rehearsed → auto-approve check (counts as "standard change")
                              otherwise → required reviewers (CODEOWNERS group, DBA, SRE), or a ServiceNow change request
        │ merge
        ▼
CD (unchanged tool)        ── deploy job asks the changeward deployment gate (GitHub custom deployment protection rule /
                              GitLab deployment approval or external status check) → allowed only if the evidence is
                              current, preconditions still hold (no drift), and the change window / freeze policy allows
        │
        ▼
Post-deploy                ── changeward verify (post-conditions, health, SLO probes)
                              → failure: automatic rollback via the rehearsed undo path (down migration, revert PR,
                                Helm/Argo rollback, flag off), plus an incident note on the change record
```

### 5.2 Deliverables

| Deliverable | Form | Notes |
| --- | --- | --- |
| **GitHub Action** `changeward/rehearse` | Marketplace action wrapping the CLI | The fastest path to users. Works with any GitHub-hosted or self-hosted runner. |
| **GitLab CI/CD component** | Published to the GitLab CI/CD Catalog | Same CLI; MR notes and pipeline status. |
| **GitHub App** (server mode) | Check runs, PR comments, custom deployment protection rule | Needed for deployment gating and organization-wide policy. |
| **GitLab integration** (server mode) | MR notes, external status checks, deployment approvals | Some features need paid GitLab tiers; document this. |
| **Argo CD / Flux hooks** | PreSync hook / health check calling the gate | For GitOps shops where "deploy" is a controller syncing. |
| **Atlantis / OpenTofu wrapper** | Plan annotation and reversibility classification | Complementary to Atlantis, not competing with it. |
| **ServiceNow / Jira Service Management bridge** *(commercial)* | Creates change requests with evidence attached; maps risk to standard, normal or emergency; syncs approvals back | The enterprise wedge. |

### 5.3 Rehearsal environments (shared by both lanes)

A rehearsal is only as good as how closely the copy matches the target. This section is shared infrastructure for both lanes:

- **Lane A** always rehearses on a **separate copy**: a migration must run forward, then backward, then be compared with the starting state, and some steps can't run inside a transaction (`CREATE INDEX CONCURRENTLY`, some type changes).
- **Lane B** usually rehearses on the target itself inside a rolled-back transaction (§7.1). It moves to a separate copy from the same tiers when a change is too large, too risky or not transactional, and it can use a branch as the *working* target for a whole session (§6.3).

**Principle:** Changeward never stores customer data or moves it outside the customer's boundary. It *orchestrates* copies that live inside the customer's environment, and **only results leave**.

#### 5.3.1 Fidelity tiers (databases; Postgres as the example)

| Tier | Where the data comes from | How it's provisioned | What it catches | What it misses |
| --- | --- | --- | --- | --- |
| **T0: schema only** | No data. Schema from `pg_dump --schema-only` of production, or from replaying every migration from scratch. | Seconds; can run on a GitHub-hosted CI runner | Syntax, dependency order, whether the down migration restores the *structure*, dangerous DDL | Data-dependent failures: `NOT NULL` on rows that are null, unique-key violations, failing casts, backfill time, lock duration |
| **T1: synthetic data** | Generated rows that respect types, constraints and foreign keys, scaled to production row counts from catalog statistics (`pg_class.reltuples`). Don't seed from the value samples in `pg_stats`: they contain real production values. | Minutes; CI runner or self-hosted | Rough scale effects, most constraint failures, approximate timing | Real data quirks (bad encodings, legacy nulls, skewed distributions) |
| **T2: masked copy** | A production copy (full, or a subset that keeps references intact) with personal data masked or anonymized: [Greenmask](https://github.com/GreenmaskIO/greenmask) (Apache-2.0), PostgreSQL Anonymizer, or commercial tools such as Tonic. Refreshed on a schedule into a **golden image**. | Golden image built nightly or weekly; each rehearsal clones it in seconds | Nearly all data-dependent failures; realistic timing on a full copy | Changes since the last refresh; behaviour that depends on masked values |
| **T3: full production clone** | **Real production data**, via a copy-on-write branch or clone created and kept **inside the production boundary**: Neon branches, Aurora fast cloning, cloud snapshot restore, ZFS/LVM thin clones of a replica (e.g. [DBLab Engine](https://github.com/postgres-ai/database-lab-engine), Apache-2.0), or `CREATE DATABASE … TEMPLATE` on a replica | Seconds (copy-on-write) to minutes (snapshot restore) | Highest fidelity: exact rows affected, real lock and duration measurements | Concurrent production traffic (a clone is idle); drift after the clone was made |

**Two traps:**

- **Supabase branches don't copy production data by default;** they're seeded from a seed file, so they count as T0/T1 unless the customer adds data.
- **`CREATE DATABASE … TEMPLATE` requires no active connections on the source,** so use it on a replica or clone, never the production primary.

#### 5.3.2 Policy chooses the tier, per environment

```yaml
rehearsal:
  default_tier: T1
  environments:
    prod:      { min_tier: T2, prefer: T3 }               # Lane A: real or masked data before prod deploys
    prod-pci:  { min_tier: T3, executor: "in-boundary" }  # only clones inside the PCI boundary
    staging:   { min_tier: T1 }
  direct:                                                  # Lane B
    personal:  { mode: live-dry-run }                      # rolled-back transaction on the dev DB
    shared-dev:{ mode: live-dry-run, branch_above_rows: 10000 }
    prod:      { mode: live-dry-run, branch_above_rows: 1000, branch_for_ddl: true }
```

Every report states the tier actually used. A T0 "passed" is never presented as if it were a T3 "passed", and policy can refuse auto-approval below a minimum tier.

#### 5.3.3 Where it runs and what leaves the boundary

```
GitHub/GitLab CI job ──► control plane ──► executor INSIDE the prod network/account
 (no prod creds,           (no data)          1. clone golden image / branch prod (T2/T3)
  no prod data)                               2. replay migrations merged since the golden image
                                              3. migrate forward → verify → migrate down
                                              4. compare: catalog diff + per-table checksums
                                              5. measure: duration, locks seen (pg_locks), rows touched
                                              6. destroy the clone
                         ◄── results only: counts, timings, lock modes, catalog diff,
                             checksum match yes/no, hashed keys, never row values by default
```

- **T0/T1 can run on a hosted CI runner. T2/T3 need the executor** (self-hosted runner, sidecar or Kubernetes operator) inside the network where the data lives.
- **The pull-request report is redacted by default.** It's visible to repository readers *and to the agent that wrote the change*. Putting production rows there would leak production data to the agent and open a prompt-injection path. Row values appear only where policy allows and the columns aren't tagged sensitive.

#### 5.3.4 Keeping copies fast and current

- **Golden images, not dumps per run.** Dump-and-restore of a large database can take hours, so rehearsals clone a regularly refreshed golden image (T2) or a copy-on-write branch (T3) in seconds.
- **Replay what's newer than the golden image.** Migrations merged since the image was built are applied to the clone first, so the rehearsal starts from what production will look like.
- **Re-check at deploy time.** The deployment gate confirms production's schema version and key facts (e.g. row counts within tolerance) still match what was rehearsed. If not, it re-rehearses or blocks.
- **Cost:** copy-on-write clones store only changed blocks and are destroyed after each run. A masked golden image costs a full copy per refresh cycle.
- **Prior art and partner:** DBLab Engine already provides thin clones and a migration checker for CI. Changeward supports it as a T3 provider ("use DBLab clones"). Our additions are the forward–rollback–compare cycle, reversibility classes, agent awareness and deployment gating.

#### 5.3.5 Other target types

- **Lock and duration estimates** come from rehearsal timing on a copy of comparable size plus static rules (non-concurrent index builds, table rewrites, `ALTER TYPE`). Report "estimated lock: ACCESS EXCLUSIVE on `orders` (12M rows), about 40s" and block it in production without a maintenance window.
- **Infrastructure:** `terraform plan` is the rehearsal. Full apply/destroy rehearsal is optional, in sandbox accounts, because it's costly.
- **Kubernetes:** server-side dry-run against the real cluster plus an apply in an ephemeral namespace or a kind/k3d cluster.
- **SaaS:** rehearse against emulators such as our Jira and Maximo clones (C11, §6.5–6.7 of the main document). This is a commercial feature.
- **Coder workspaces** (main document §6.8) can host rehearsal environments for teams already running Coder, through a registry module.

---

## 6. Lane B: direct agent actions

Lane B covers every case where an agent acts on a live system itself. It's designed for two audiences at once: **an individual developer** working with a coding agent on their own systems (fast, local, permissive by default), and **an organization** letting agents act directly on shared and production systems (bounded, approved, audited). It's the same product in both cases, with environment class setting the strictness.

### 6.1 The flow

```
Agent session starts ──► changeward session start   (optional checkpoint: DB branch/template, git ref, k8s dump)
        │
        ▼
Agent calls a tool  ──► classify (read → pass through, with result size limits)
   (MCP / hook / SDK)        │ write or destructive
                             ▼
                      plan     ── live dry-run in a rolled-back transaction, or a branch above thresholds (§5.3)
                      policy   ── by environment class, risk, reversibility, principal, time
                             │
             ┌───────────────┼──────────────────────────────┐
             ▼               ▼                              ▼
     auto-approve     inline confirm (personal/dev)   out-of-band approval (prod/restricted)
             │               │                       returns "pending: plan_id" to the agent at once
             └───────┬───────┘                              │ approved
                     ▼                                      ▼
               apply (executor holds the credentials; re-checks preconditions)
                     │
                     ▼
               verify ──► receipt with undo handle ──► appended to the session timeline
                     │
                     ▼
Session ends  ──► summary · undo any step · rewind to checkpoint · promote to PR (§6.5)
                  · reconciliation PR if a Git-managed system was changed
```

### 6.2 Integration surfaces, in priority order

1. **Our own MCP server per adapter** (e.g. `changeward mcp postgres`). It exposes agent-friendly tools: `query` (read-only), `propose_change` (returns a plan ID and a human-readable diff), `apply_change(plan_id)`, `undo_change(receipt_id)`, `checkpoint`, `restore`. Tool annotations are set correctly (`readOnlyHint`, `destructiveHint`, `idempotentHint`), so hosts like Codex gate them appropriately.
2. **MCP proxy mode** (`changeward proxy -- <any MCP server command>`): wraps *any* existing MCP server, over stdio or HTTP.
   - For tools a known adapter understands (e.g. the `execute_sql` tool of a third-party Postgres MCP server), calls are rewritten into plan → approve → apply.
   - For unknown tools, it classifies using annotations and policy. Destructive calls get **approval-only** handling (no preview available), are labelled `irreversible` unless a compensation manifest exists, and are fully logged.
3. **Agent hooks** for coding agents that support pre-tool hooks (e.g. Claude Code's `PreToolUse`; similar hooks where other hosts support them). They intercept shell commands like `psql -c`, `kubectl apply/delete`, `terraform apply`, `helm upgrade` and `nginx -s reload`, and redirect them through the gate. Install with `changeward install claude-code` and similar.
4. **Python and TypeScript SDKs** for custom agents: wrap tool functions with `@guarded(adapter="postgres")`. Integrations for LangGraph interrupts, the OpenAI Agents SDK and the Claude Agent SDK map approvals onto the framework's own human-in-the-loop primitives.
5. **Gateway integrations:** an agentgateway external-policy/processing hook, an IBM ContextForge plugin (Python), Docker MCP Gateway (check its current extension points), and an Envoy `ext_proc` filter. These place Changeward behind the gateways enterprises already run (B2).

### 6.3 Environment modes and credentials

| Environment class | Typical target | Default policy | Approval | Credentials |
| --- | --- | --- | --- | --- |
| `personal` | Local Postgres/Docker, a personal Neon branch, a kind cluster | Auto-approve `reversible` and `checkpoint-only` (with a session checkpoint); confirm `irreversible` | Inline (terminal prompt, MCP elicitation, coding-agent permission prompt) | The developer's own, used by the local executor; never passed into the agent's context |
| `shared-dev` | Team dev database or cluster | Auto-approve small reversible changes; confirm larger or destructive ones | Inline, or Slack for destructive | Shared executor or developer credentials, per team choice |
| `staging` | Pre-production | Auto-approve within limits; destructive needs a second person | Slack/web | Executor-held, just-in-time |
| `prod` | Production, direct (incidents, data fixes, ops agents) | Every write planned; auto-approve only allow-listed, rehearsed, reversible operations within budget; everything else approved out-of-band; break-glass requires an incident or ticket reference | Out-of-band (Slack/web/PagerDuty/ITSM), identity different from the proposer | Executor in the prod boundary, **just-in-time** short-lived credentials (Vault dynamic secrets, cloud IAM) scoped to the session |
| `prod-restricted` | PCI/regulated systems | Read-only by default; writes only through Lane A or explicit break-glass | Quorum *(commercial)* | As `prod`, plus session recording |

- **Branch as the working target.** For personal and shared-dev work, a session can start by creating a database branch (Neon, DBLab, template DB) and pointing the agent at it. The agent then works freely with no approvals on the branch, and the result is promoted or discarded at the end. This is often the best developer experience: real data shape, zero risk, one decision at the end.
- **Environment class comes from the connection, not the agent.** It's set in the connection config (`changeward connect postgres://… --env prod`) or by the organization's server. The agent can't relabel production as `personal`.
- **Preventing bypass.** If the agent also has raw credentials, it can go around the gate. Mitigations: credentials live only in the executor; hooks block raw `psql`/`kubectl` invocations against registered targets; network policy (e.g. Coder Agent Firewall/`boundary`, Kubernetes network policy) limits what the agent's process can reach directly.

### 6.4 Sessions, timeline and undo

The session is to Lane B what the PR is to Lane A: the unit people review and reason about.

```
$ changeward log
SESSION s_7f3a  agent=claude-code  user=steve  env=personal  target=pg://localhost/app   checkpoint=cp_19
  #1  10:02  ALTER TABLE orders ADD COLUMN region text         reversible     applied
  #2  10:03  UPDATE orders SET region = … (48,211 rows)        reversible     applied
  #3  10:05  CREATE INDEX orders_region_idx …                  reversible     applied
  #4  10:09  DELETE FROM orders_archive WHERE … (3,104 rows)   reversible     applied (soft: before-images kept)

$ changeward undo 4            # undo one step (guarded: only if rows still match their after-image)
$ changeward rewind s_7f3a     # undo the whole session in reverse order, or restore cp_19
$ changeward promote s_7f3a    # turn #1–#3 into a migration file + PR (§6.5)
```

- **Dependency-aware undo.** Undoing #1 (the column) requires undoing #2 and #3 first. The CLI shows the dependency chain and asks.
- **Available to the agent as tools too** (`list_session_changes`, `undo_change`), so an agent can clean up after itself, subject to the same policy.
- **Local web view** of the timeline, with diffs per step (open source); an organization-wide view of all sessions *(commercial)*.

### 6.5 Promotion and reconciliation: how Lane B feeds Lane A

**Promotion (dev → PR).** When a session in `personal` or `shared-dev` produced something worth keeping:

1. `changeward promote` groups the session's schema changes and translates them into the project's own migration framework, detected from the repository (Flyway/Liquibase/Atlas SQL files, Prisma/Rails/Django/Alembic migrations). Data changes become seed scripts or one-off data migrations, if the user chooses.
2. It generates the **down** migration from the recorded undo handles, so the rollback path is known to work: it was exercised (or can be exercised) in the session.
3. It opens a PR with the session timeline and evidence attached, and Lane A takes over: rehearsal at the policy-required tier, review, deployment gate.

**Reconciliation (prod direct → Git).** When a `prod` direct change touches a system that Git manages (schema with migrations, Kubernetes under GitOps, Terraform-managed resources):

1. The executor records the change as usual.
2. Changeward opens a **reconciliation PR** expressing the change as code (migration, manifest change, Terraform update or `import` block) and links it to the incident or ticket.
3. Policy can require that reconciliation PRs are merged within a time limit; open ones are reported as drift.

GitOps controllers (Argo CD, Flux) would otherwise revert a direct Kubernetes change on their next sync. The Kubernetes adapter detects GitOps ownership and either pauses auto-sync for that object during the incident (with policy approval) or refuses the direct change and redirects it to Lane A.

### 6.6 Approval in the direct lane (C9)

- **Channels:** terminal prompt (local), MCP elicitation (in-band), the coding agent's own permission prompt (via hooks), Slack/Teams interactive messages, web UI, GitHub/GitLab issue comment, PagerDuty (during incidents), and ServiceNow *(commercial)*.
- **Non-blocking by design.** When approval is needed, the tool call returns immediately with a plan ID and a readable diff. The agent can continue other work, poll `get_change_status`, or end its turn. Approvals can be batched ("approve these 6 reversible changes from session s_7f3a").
- **Security caveat:** an in-band approval (MCP elicitation, or a prompt inside the agent's own UI) is rendered by a client the agent may influence. That's acceptable in `personal` mode, where the human at the keyboard owns the system. Policy requires **out-of-band approval** (Slack, web UI or ServiceNow, by an identity different from the agent's principal) above low risk in shared and production environments. This is the default in server mode.
- **Separation of duties:** in shared and production environments, the principal that proposed a change can never approve it. Multi-approver quorums are available *(commercial)*.
- **Never hold locks while waiting for a human.** Plans are computed in a transaction that is rolled back, and applying re-checks preconditions (§7.1).

### 6.7 Deliverables

| Deliverable | Form | Notes |
| --- | --- | --- |
| **`changeward mcp <adapter>`** | Local MCP server (stdio/HTTP) via `npx`/`uvx`/brew | The fastest path to individual developers; listed in MCP registries |
| **`changeward proxy`** | Wraps any existing MCP server | Adds plan/approve/undo to tools people already use |
| **Coding-agent integrations** | Claude Code plugin and hooks; Codex, Cursor and Gemini CLI configurations | One-command install: `changeward install claude-code` |
| **Session CLI and local web view** | `log`, `undo`, `rewind`, `promote`, `checkpoint` | Open source; the core individual experience |
| **Python/TypeScript SDKs** | `@guarded` wrappers; LangGraph, OpenAI Agents SDK, Claude Agent SDK integrations | For teams building custom and continuous agents |
| **Shared executor** | Container / Helm chart / systemd service near the target | For team dev, staging and prod targets |
| **Slack/Teams approval app** | Interactive approve/reject with diff | Open source basic; org-wide routing *(commercial)* |
| **Gateway plugins** | agentgateway, ContextForge, Docker MCP Gateway, Envoy `ext_proc` | Enterprise placement behind existing gateways |
| **Just-in-time credential broker** *(commercial)* | Vault/cloud IAM integration issuing session-scoped credentials to executors | Required for production direct access at most enterprises |
| **Break-glass workflow** *(commercial)* | Incident/ticket-linked elevation, ServiceNow emergency change, mandatory reconciliation | The enterprise wedge for Lane B |
| **Session recording and replay** *(commercial)* | Full, tamper-evident record of a session's tool calls, plans, approvals and results | Compliance evidence for direct production access |

---

## 7. Adapter designs

### 7.1 Postgres (open source; ships first)

**Statement analysis.** `pg_query_go` (libpg_query, BSD-3) parses statements into an AST. Statements are classified as read, DML or DDL; dangerous forms are flagged (`DROP`, `TRUNCATE`, `DELETE`/`UPDATE` without `WHERE`, `ALTER … DROP COLUMN`, type changes, `GRANT`/`REVOKE`, non-concurrent index builds).

**Lane B, live DML (data changes).** Normally this needs **no copy**: the rehearsal *is* the production database, inside a transaction that is always rolled back, so the diff reflects real data, constraints and triggers.

- **Caveats:**
  - Row locks are held for the moment the dry run takes, hence the short `lock_timeout` and `statement_timeout`.
  - Sequences advance even though the transaction rolls back.
  - Triggers with external side effects (`NOTIFY`, foreign data wrappers, `dblink`) still fire. The adapter detects them from the catalog and either switches to a branch or marks the plan "side effects possible".
- **Switch to a T3 branch (§5.3.1)** when the statement would touch more rows than a configured threshold, when the DDL takes heavy locks, or when policy forbids dry runs on the primary (e.g. PCI).

1. **Plan:** `BEGIN` (REPEATABLE READ, `statement_timeout`, `lock_timeout`).
   - On **Postgres 18+**, rewrite the statement to add `RETURNING OLD.*, NEW.*`. PG18 supports `OLD`/`NEW` in `RETURNING` for `INSERT`/`UPDATE`/`DELETE`/`MERGE`, which yields an exact per-row before/after diff.
   - On older versions, for simple single-table statements, pre-select the target rows by the same predicate; otherwise use an in-transaction capture trigger, which needs privileges.
   - Then `ROLLBACK`. The plan stores primary keys and before/after images (capped, e.g. 10,000 rows; above the cap it stores a summary plus a stricter policy tier) and a hash of the before-images as a **precondition**.
2. **Apply:** a new transaction re-runs the statement with `RETURNING OLD/NEW`. If the affected key set or the before-image hash differs from the plan (**drift**), it aborts with "data changed since plan; re-plan required". Otherwise it commits and stores the before-images as the undo handle.
3. **Undo:** generate inverse statements: `INSERT` deleted rows, `DELETE` inserted rows (by key), `UPDATE` changed rows back, each guarded so it applies only if the row still equals its after-image. Conflicts are reported for human resolution, never overwritten.

**Lane B, live DDL:**

- Validate inside a rolled-back transaction (most Postgres DDL is transactional; flag `CREATE INDEX CONCURRENTLY` and similar).
- **Soft-drop** for destructive DDL: `DROP TABLE t` becomes `ALTER TABLE t RENAME TO _cw_trash_t_<id>` with a retention TTL. `DROP COLUMN` becomes a rename plus `DROP` after retention when policy allows. This keeps the class `reversible` without full backups.
- Otherwise the change is `checkpoint-only` (requires a branch or snapshot, via Neon or cloud APIs) or `irreversible` (blocked in production by default).

**Lane A, migrations:**

- Detect migration frameworks: Flyway, Liquibase, Atlas, Sqitch, Prisma, Rails, Django, Alembic, goose, golang-migrate, Knex.
- Rehearse each migration on a copy: forward → collect effects (catalog diff, row counts, timing, lock observations from `pg_locks` sampling) → run its down/rollback → **compare** the catalog and data checksums with the starting state. A missing down migration or a mismatch makes the class `checkpoint-only` or `irreversible`, and the report says so.
- Suggest an **expand/contract** alternative when a migration is destructive. For example: "add the new column, backfill, switch reads, drop the old column in a later release".

**Known limits, stated up front:** triggers or functions with side effects outside the database (`NOTIFY`, foreign data wrappers, `dblink`), sequence gaps, and very large changes. These lower the reversibility class; they never stay silent.

### 7.2 Git and files (open source)

- Checkpoint and restore of working trees via git refs (`refs/changeward/<cp>`), with Jujutsu operation-log support when present.
- Mostly a *participant* rather than a headline feature, since coding agents already offer local file checkpoints. It provides the git layer of session checkpoints and `rewind` in Lane B, writes the files that promotion and reconciliation PRs contain (§6.5), and supplies provenance in Lane A.

### 7.3 Terraform / OpenTofu (open source)

- Wrap `plan -out` and `show -json`. Classify each resource change as create, update in place, replace or destroy.
- **Stateful-resource rules** (databases, buckets, volumes, KMS keys, DNS zones). Destroying or replacing one is `irreversible` unless a snapshot or backup step precedes it. The plugin can insert such a step for supported providers *(commercial packs for AWS, Azure and GCP)*.
- Undo means re-applying the prior configuration and state, labelled `compensatable`, never claimed to be exact.

### 7.4 Kubernetes / GitOps (open source)

- Server-side apply with `dryRun=All`, plus a diff against live objects with `managedFields` stripped. Store previous live objects as the undo handle.
- Verification: rollout status, readiness, optional Prometheus queries.
- Deleting a PVC, namespace or CRD is blocked or requires a VolumeSnapshot first.
- Argo CD/Flux: a PreSync gate and a post-sync verification hook.

### 7.5 NGINX / Traefik (open source)

Render, validate (`nginx -t` / Traefik dry-run), diff, apply via reload, verify with health checks, and revert on failure. NixOS-managed hosts use generations (main document §6.6).

### 7.6 Generic HTTP / OpenAPI with compensation manifests (open-source framework; commercial packs)

- For any REST API: classify by method and path, and capture request and response.
- **Compensation manifests** (YAML) declare inverse operations. For example `POST /issues → DELETE /issues/{id}`, `PUT /x → PUT /x with before-body`, and "irreversible" for `POST /emails`. Dry-run is used only where the API supports it, or against an emulator.
- The community contributes manifests for open-source and developer APIs. **Commercial packs** cover enterprise SaaS: Salesforce, ServiceNow, Workday, Okta/Entra admin, Jira Cloud, Zendesk, plus Maximo and Jira Data Center, drawing on our successor-product code.

### 7.7 Checkpoints across systems (C12)

- A Changeset-level `checkpoint` asks every participating adapter for a `CheckpointRef` after pausing writers. Examples: Postgres → branch or template database; k8s → VolumeSnapshot plus object dump; files → git ref; Coder → workspace snapshot via template parameters (main document §6.8).
- The checkpoint is recorded as one manifest. `restore` replays restores in reverse dependency order.
- Branching: restoring a checkpoint into N copies supports parallel agent attempts. This is mostly useful together with C11 rehearsal environments.

---

## 8. Policy, risk and identity

**Example policy (CEL):**

```yaml
rules:
  - name: auto-approve-reversible-low-risk
    when: >
      change.reversibility == "reversible" && change.rehearsal.passed &&
      change.blast.rows < 1000 && change.env != "prod-pci"
    decide: approve
  - name: prod-destructive-needs-dba
    when: change.env.startsWith("prod") && change.destructive
    decide: require
    approvers: ["group:dba", "group:sre"]   # quorum: 2 (commercial)
  - name: freeze
    when: calendar.freeze("q4-release")      # change windows / freezes
    decide: deny
  - name: agent-stricter-than-human
    when: principal.kind == "agent" && change.reversibility in ["checkpoint-only","irreversible"]
    decide: require
    approvers: ["owner:codeowners"]
  # Lane B (direct) rules use the same language
  - name: personal-sandbox
    when: change.lane == "direct" && change.env == "personal" && change.reversibility != "irreversible"
    decide: approve
  - name: prod-direct-break-glass
    when: change.lane == "direct" && change.env.startsWith("prod") && !session.incident_ref.exists()
    decide: deny          # direct prod writes need an incident/ticket reference
  - name: continuous-agent-budget
    when: change.lane == "direct" && principal.agent == "refund-agent" && session.spend_usd + change.spend_usd > 500
    decide: require
    approvers: ["group:finance-ops"]
```

The same policy file covers both lanes. A rule can target one lane (`change.lane`), an environment class, or both, so an organization reasons about agent risk in one place.

- **Risk inputs:** adapter facts, rehearsal outcome, environment labels, data-sensitivity tags (from a catalog or manual labels), time and calendar, and principal trust (agent identity, delegating human, the agent's past change failure rate).
- **Identity:** accept OIDC/JWT from CI (GitHub/GitLab OIDC), from agent gateways (agent identity per B1) and from IdPs. Every change record carries both **the agent principal and the delegating human**.
- **Provenance in Lane A:** bot accounts, `Co-Authored-By` trailers, and Sigstore/gitsign signatures or GitHub artifact attestations where available. Unknown provenance is treated as "possibly agent" when policy says so.
- **Provenance in Lane B:** session ID, agent host and version (from MCP client info or the hook environment), the delegating user (OS user locally; IdP identity in shared and prod modes), and agent identity tokens from the gateway when present. Promoted and reconciliation PRs carry the session ID, so provenance survives the move into Lane A.

---

## 9. Security model

| Threat | Mitigation |
| --- | --- |
| Agent tricked (prompt injection) into destructive changes | It can only *propose*. The executor holds credentials, and policy, out-of-band approval and reversibility gates decide. |
| Approve one plan, apply another | Approvals bind to the plan hash; apply re-checks preconditions; plans expire. |
| Agent approves its own change | Separation of duties; approver identity must differ from the proposer; in-band approvals disabled above low risk. |
| Changeward itself compromised | Executors are least-privilege per target; control plane holds no target credentials; plugins are signed and verified; audit log is a hash chain with optional external anchoring. |
| Undo makes things worse (stale undo) | Guarded inverse operations apply only if current state matches the after-image; conflicts go to a human. |
| Data exposure in plans and evidence | Row images are redacted by column tags; evidence retention policies; optional storage of hashes instead of values. |
| Supply chain | Reproducible builds, SBOMs, Sigstore-signed releases, pinned dependencies (practices already used in the Foundry repo). |
| **Lane B: agent bypasses the gate** with raw credentials or direct network access | Credentials only in the executor (just-in-time in shared and prod); hooks block raw CLI invocations against registered targets; network policy limits direct reachability (§6.3) |
| **Lane B: production relabelled as personal** to get permissive policy | Environment class is bound to the connection by config or the organization server, not chosen by the agent; servers can pin classes by host or DSN pattern |
| **Lane B: data leakage through tool results** (large query results flowing into the agent's context and on to the model provider) | Read results capped and column-redacted by sensitivity tags; plans show counts and hashed keys in shared and prod modes |
| **Lane B: drift** between Git and reality after direct changes | Mandatory reconciliation PRs for Git-managed systems; open reconciliations reported as drift (§6.5) |

---

## 10. Language and stack decision

| Criterion | Go | Rust | TypeScript | Python |
| --- | --- | --- | --- | --- |
| Single static binary for air-gapped installs, CI and sidecars | ✅ | ✅ | ⚠️ (bundlers) | ❌ |
| Fit with Kubernetes, Terraform/OpenTofu, OPA, Argo, Docker MCP Gateway, Coder | ✅ native | ⚠️ | ❌ | ❌ |
| MCP SDK maturity | ✅ official Go SDK (~5.2K★) and mcp-go (~9.2K★) | ✅ official rust-sdk | ✅ reference SDK | ✅ reference SDK |
| Postgres depth (driver, real SQL parser) | ✅ pgx, pg_query_go | ✅ | ⚠️ | ✅ |
| Plugin ecosystem precedent | ✅ go-plugin (Terraform, Vault, Packer) | ⚠️ | ⚠️ | ⚠️ |
| Policy libraries | ✅ cel-go, OPA, cedar-go | ✅ Cedar (native), CEL | ⚠️ | ⚠️ |
| Contributor pool among infra/platform engineers | ✅ large | ⚠️ smaller | ✅ (but app-oriented) | ✅ (but data-oriented) |
| Team reuse | ✅ Foundry is Go | — | Kose is TypeScript (but its Jira adapter can be Go over REST) | — |

**Decision:** **Go** for the core, CLI, executor, server and first-party adapters. **TypeScript and Python** for SDKs, framework integrations, the ContextForge plugin and the `npx`/`uvx` launchers. The protocol is defined in protobuf (`buf`) with generated clients. Revisit Rust only if a high-throughput data-plane filter (e.g. an inline agentgateway module) becomes a bottleneck.

**Repository layout (monorepo, Apache-2.0):**

```
changeward/
  proto/                 # Change Record + Adapter service (buf)
  spec/                  # human-readable spec, JSON Schemas
  cmd/changeward/        # CLI: plan, rehearse, report, gate, apply, verify, undo, proxy, mcp, serve, install
  internal/engine/       # state machine, saga, risk, policy (CEL), evidence, audit chain
  internal/store/        # sqlite, postgres
  adapters/{postgres,git,k8s,terraform,nginx,openapi}/
  integrations/{github-action,gitlab-component,claude-code,argo,agentgateway}/
  sdk/{python,typescript}/
  conformance/           # adapter test suite + generated operation corpora
  deploy/{helm,compose,nix}/
```

Commercial code lives in a **separate private repo** (`changeward-enterprise`) that builds signed plugin binaries and server modules against the public proto and SDK. The open-source repo never depends on it.

---

## 11. Packaging and distribution

| Channel | Why it matters |
| --- | --- |
| `npx -y @changeward/cli`, `uvx changeward` (thin launchers that download the verified Go binary) | MCP client configs overwhelmingly use `npx`/`uvx`, so this is zero friction for agent users. |
| Homebrew, `curl \| sh`, apt/rpm, OCI image, Helm chart, Nix flake | Developers, platform teams, air-gapped installs. |
| **GitHub Marketplace Action** and **GitLab CI/CD Catalog component** | The main Lane A distribution, discoverable where CI is configured. |
| **Official MCP Registry**, Docker MCP Catalog, community directories (Glama, PulseMCP, Smithery and others) | The main Lane B discovery path. |
| Claude Code plugin/marketplace entry; config snippets for Codex, Cursor, Gemini CLI | One-command install into coding agents. |
| **Coder registry module** (main document §6.8) | Reaches enterprises running self-hosted agent workspaces. |
| Gateway integrations (agentgateway, ContextForge, Docker MCP Gateway) | Enterprise buyers find us through tools they already run. |

**Telemetry:** off by default and opt-in only, with an anonymous counts schema documented in the repo. Air-gapped customers and open-source communities penalize anything else.

---


## 12. Testing and quality

- **Correctness of undo is the product.** A wrong undo is worse than none. So:
  - **Property-based tests:** for generated operation sequences, `state₀ → apply → undo → state₀′`, then assert a table/catalog checksum match.
  - The **conformance suite** is required for every first-party and certified adapter.
  - **Fuzzing** with SQL corpora (pg_query_go round-trips, real-world migrations from public repos).
- **Matrix testing:** Postgres 13–18 (fallback and `RETURNING OLD/NEW` paths), major migration frameworks, Kubernetes versions, Terraform and OpenTofu versions.
- **Rehearsal fidelity tests:** compare rehearsal predictions (rows, locks, durations) with real applies in a staging fleet, and track the prediction error.
- **Adversarial tests:** prompt-injection scenarios (in issue text, database rows, tool output) driving agents toward destructive actions; measure what gets through.
- **Dogfooding:** use Changeward on its own infrastructure and on the Kose and Foundry migration pipelines.

---


## Appendix: sources and references

- Main analysis: `agent-native-infrastructure.md` (§5 B7, §6.5–6.8, §9.2).
- MCP: [official Go SDK](https://github.com/modelcontextprotocol/go-sdk), [mcp-go](https://github.com/mark3labs/mcp-go), [Python SDK](https://github.com/modelcontextprotocol/python-sdk), [TypeScript SDK](https://github.com/modelcontextprotocol/typescript-sdk), [Rust SDK](https://github.com/modelcontextprotocol/rust-sdk); tool annotations: [StackOne changelog](https://www.stackone.com/changelog/mcp-tool-annotations/), [Codex approval modes using annotations](https://codex.danielvaughan.com/2026/07/24/codex-cli-writes-app-approval-mode-mcp-tool-annotations-read-only-hint-approval-flow/), [Codex readOnlyHint parallelism](https://codex.danielvaughan.com/2026/05/27/codex-cli-mcp-readonlyhint-concurrent-tool-execution-annotations-parallelism/), [OpenAI app submission annotation requirements](https://community.openai.com/t/mcp-submission-still-blocked-annotations-error-despite-correct-manifest-may-2026/1381645).
- Policy and plugins: [cel-go](https://github.com/google/cel-go), [OPA](https://github.com/open-policy-agent/opa), [cedar-go](https://github.com/cedar-policy/cedar-go), [hashicorp/go-plugin](https://github.com/hashicorp/go-plugin).
- Postgres: [pgx](https://github.com/jackc/pgx), [pg_query_go](https://github.com/pganalyze/pg_query_go); PG18 `RETURNING OLD/NEW`: [Crunchy Data](https://www.crunchydata.com/blog/postgres-18-old-and-new-in-the-returning-clause), [Neon](https://neon.com/postgresql/postgresql-18/enhanced-returning), [pgEdge](https://pgedge.com/blog/postgresql-18-returning-enhancements-a-game-changer-for-modern-applications) (confirm against the official PG18 release notes).
- Rehearsal data: [DBLab Engine](https://github.com/postgres-ai/database-lab-engine) (Apache-2.0, thin clones and CI migration checks), [Greenmask](https://github.com/GreenmaskIO/greenmask) (Apache-2.0, anonymization and test data); PostgreSQL Anonymizer, Tonic, Aurora fast cloning, Neon branching (verify current features).
- Incident context: Replit production-database deletion, July 2025: [OECD AI incident record](https://oecd.ai/en/incidents/2025-07-19-1eb1), [eWeek](https://www.eweek.com/news/replit-ai-coding-assistant-failure/), [heise](https://heise.de/-10499597).
- Ecosystem tools referenced (verify current features and tiers before relying on them): GitHub custom deployment protection rules and environments; GitLab protected environments, deployment approvals, external status checks and the CI/CD Catalog; Atlas, Squawk, Bytebase, Flyway, Liquibase; Atlantis, HCP Terraform, Spacelift; Argo CD, Flux, Argo Rollouts; ServiceNow Change Management; Coder (main document §6.8).
