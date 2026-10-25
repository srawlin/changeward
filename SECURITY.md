# Security policy

Changeward sits between AI agents and production systems, and holds the credentials to them. A flaw that lets a
change reach a system without the plan, policy and approval it should have had is the most serious kind of bug
this project can have. Thank you for helping find them.

## Reporting a vulnerability

**Please do not open a public issue, pull request or discussion for a security problem.**

Report it privately through GitHub: **[Report a vulnerability](https://github.com/srawlin/changeward/security/advisories/new)**
(the repository's *Security* tab → *Report a vulnerability*). Only the maintainers can see the report.

Please include:

- what an attacker can do, and what they need first (an agent session, a `propose` token, network access to
  the executor, a malicious row or issue the agent reads, …);
- the Changeward version or commit, the adapter (Postgres, MySQL, Kubernetes, Terraform, …) and how it is
  deployed (local CLI, MCP server, shared executor, CI);
- steps to reproduce, ideally the smallest `changeward.yaml`, policy and statement or manifest that show it;
- whether you have shared the finding with anyone else.

## What to expect

- We acknowledge the report within **3 business days**.
- We send an initial assessment (confirmed or not, severity, likely fix) within **10 business days**.
- We aim to release a fix within **90 days** of the report, sooner for critical issues, and agree a disclosure
  date with you. We publish a GitHub security advisory, and request a CVE where one applies.
- We credit you in the advisory, unless you would rather stay anonymous.

If you don't hear back within the times above, please send a short follow-up through the same report.

## Supported versions

Changeward is pre-release. Security fixes go to the `main` branch, and to the latest release once releases are
published. Older versions are not patched; please upgrade.

## Scope

These are the guarantees Changeward makes (see [Concepts](https://changeward.com/docs/concepts) and
[Policy](https://changeward.com/docs/policy)). Breaking any of them is in scope:

- **No unplanned or unapproved change.** A change is applied only as planned, and only after policy approved it
  or the approvals it requires were given. Applying something different from the reviewed plan, or applying
  without the required approval, is a vulnerability.
- **Approvals bind to the plan hash.** Reusing an approval after the change, the plan or the target data moved.
- **Separation of duties.** An agent approving its own change, or a proposer satisfying an out-of-band approval
  that must come from someone else.
- **The environment class is bound to the connection.** An agent or request choosing or changing it.
- **Dry runs have no side effects.** Any statement or call during planning that changes state outside the
  rolled-back transaction (for example through `dblink`, `pg_terminate_backend` or `DO` blocks), or a Kubernetes
  or Terraform plan that changes the live system.
- **Fail closed.** A policy error, timeout or unreachable executor that results in a change being allowed.
- **Undo is guarded.** Undo overwriting data that someone changed after the original change.
- **Production redaction.** Row values or unhashed keys from a `prod` target appearing in plans, tool results,
  reports or logs that the agent or a PR can see; production values in CI rehearsal data.
- **The audit log is tamper-evident.** Changing, removing or reordering entries without the hash chain showing it.
- **Executor authentication and roles.** Acting without a valid token, or beyond the token's roles; reading the
  credentials or secrets the executor holds.
- **Gateway authorization.** A tool call that the Envoy, agentgateway or ContextForge integration should have
  denied getting through.
- **Prompt injection that defeats the controls above.** Content an agent reads (an issue, a row, tool output)
  causing a change to bypass planning, policy or approval. Injection that only makes the agent *propose* a
  harmful change, which policy then handles as designed, is not a vulnerability.

Also in scope: the SDKs, the CI integrations (GitHub Action, GitLab component), the Helm chart and systemd unit,
and the install and release artifacts.

## Out of scope

- **Getting around a coding-agent hook while the agent's machine holds the credentials.** The hooks installed by
  `changeward install` steer a cooperative agent to Changeward's tools; they are not a security boundary against
  a process that already has direct database or cluster credentials. The boundary is the
  [shared executor](https://changeward.com/docs/executor), which holds the credentials so the agent has none.
  A hook bypass is still worth reporting as a regular issue.
- Attacks that need control of the executor host, its configuration or its secrets, or of a policy file you
  are allowed to edit.
- Decisions that follow the policy as written, including the built-in defaults, even if you think the policy
  should be stricter. Suggestions are welcome as issues.
- Vulnerabilities in dependencies without a way to exploit them through Changeward (please report those
  upstream), and findings from automated scanners without a demonstrated impact.
- Denial of service through very large or slow plans, unless it causes a change to be allowed or the audit log
  to be lost.
- The changeward.com website, unless it serves malicious content or install instructions.

## Safe harbor

We will not pursue or support legal action against anyone who researches and reports in good faith under this
policy: testing only against systems and data you own or are allowed to test, not accessing or changing other
people's data, not degrading anyone's service, and giving us reasonable time to fix the problem before
disclosing it.

## Running Changeward securely

- Run the **shared executor** for anything beyond a personal environment, so credentials live next to your
  systems and not on laptops or in agents' environments.
- Give each person and service its own executor token with the narrowest roles (`propose`, `approve`, …), and
  serve the executor over TLS (`--tls-cert`, `--tls-key`).
- Connect each target with the **least-privileged** database role or Kubernetes service account that can make
  the changes you allow, and classify production as `prod` or `prod-restricted`.
- Keep secrets out of `changeward.yaml`: reference them as `${VAR}`.
- Export the audit log to OpenTelemetry or other storage that agents can't write to.
