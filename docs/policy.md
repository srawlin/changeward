# Policy

One policy file covers every system and both lanes. Rules are [CEL](https://cel.dev) expressions over the plan.
Start from the built-in defaults:

```sh
changegate policy init          # writes changegate.policy.yaml with the built-in rules
changegate policy check         # compile and report errors
```

## Rules

```yaml
version: 1
rules:
  - name: prod-destructive-needs-dba
    when: change.env == "prod" && change.destructive
    decide: require                 # approve | require | deny
    channel: out-of-band            # inline (the agent's user may confirm) | out-of-band (someone else)
    approvers: ["group:dba", "group:sre"]
    quorum: 2
    reason: destructive production change

  - name: release-freeze
    when: change.env == "prod" && calendar.freeze("q4-release")
    decide: deny
    reason: release freeze

  - name: agents-stricter-than-humans
    when: principal.kind == "agent" && change.reversibility in ["checkpoint-only", "irreversible"]
    decide: require
    approvers: ["owner:codeowners"]

  - name: big-updates-in-staging
    when: change.env == "staging" && change.blast.rows > 50000
    decide: require

freezes:
  - { name: q4-release, start: "2026-12-15T00:00:00Z", end: "2027-01-05T00:00:00Z" }
```

- **Combining:** deny > require > approve. Every matching rule counts. Their approvers are merged, and the
  highest quorum wins.
- **Fallback:** your rules come first. If none match, the built-in environment defaults apply
  ([concepts.md](concepts.md#environment-classes)).
- **Errors fail closed:** a rule that fails to evaluate counts as `require`, never as a silent pass.
- **Inline vs out-of-band:** `inline` lets the delegating human confirm in the agent's UI. `out-of-band` requires
  `changegate approve`, Slack or the web view, from someone other than the proposer.

## Variables

| Variable | Fields |
| --- | --- |
| `change` | `lane` (`direct`/`ci`), `env`, `target`, `adapter`, `kind`, `access` (`read`/`write`/`destructive`), `reversibility`, `destructive`, `blast.rows`, `rehearsal.passed`, `rehearsal.tier`, `statements` (e.g. `["UPDATE"]`), `flags` (e.g. `gitops-owned`), `objects`, `facts` (adapter-specific, e.g. `gitops_owner`), `warnings` (count), `signals` |
| `principal` | `kind` (`agent`/`human`), `agent`, `human` |
| `session` | `incident_ref`, `has_checkpoint` |
| `now` | timestamp |
| `calendar.freeze(name)` | `true` inside the named window |

**Approver forms:**

- `user:alice`, checked against identities.
- `group:sre`, enforced by your approval channel.
- `owner:codeowners`, enforced by CODEOWNERS on GitHub, or approval rules on GitLab.
- `owner`, the delegating human.

In CI, PR or MR approvals satisfy `require` when every named `user:` has approved and the quorum is met by
people other than the author ([GitHub](ci/github-actions.md#making-it-a-merge-gate),
[GitLab](ci/gitlab.md#making-it-a-merge-gate)).

## Testing policy

```sh
changegate plan --target app "DELETE FROM orders WHERE created_at < now() - interval '2 years'"   # plan + decision, nothing applied
changegate gate evidence.json --json                                                             # decision for CI evidence
```

## Slack approvals

Configure `approvals.slack` in `changegate.yaml`, and run `changegate serve` where Slack can reach it (an
ingress or a tunnel). Its interactivity endpoint is `POST /slack/interactions`. Requests are verified with the
signing secret.

Changes that need out-of-band approval post a message with the plan and **Approve / Reject** buttons. `users`
maps Slack user IDs to `user:` identities, so separation of duties compares like with like.

## Jev signals (optional)

```yaml
signals:
  jev:
    enabled: true          # key from $TYPESAFE_API_KEY
```

Before auto-approving a change, Changegate asks TypeSafe's Jev two questions:

- `intent_mismatch`: does the effect go beyond what the agent said it was doing?
- `injection`: does the stated intent look injected?

A confident score turns an auto-approval into "confirm with a human". Signals can only tighten a decision, never
approve, and they appear as `change.signals` for your own rules. Only the stated intent and the redacted plan
diff are sent.
