# Changegate Change Record specification, v0

Status: draft (v0). Wire format: [`proto/changegate/v1/change.proto`](../proto/changegate/v1/change.proto) and
[`adapter.proto`](../proto/changegate/v1/adapter.proto). JSON form: [`schemas/change-record.schema.json`](schemas/change-record.schema.json).

The Change Record describes one agent-originated change, whether it arrives as a pull request (Lane A, CI/CD) or as
a live action through MCP, a hook or an SDK (Lane B, direct), together with its evidence, approvals and undo
path. The goal is that hosts, gateways and CI systems can carry plan, approval and undo semantics in one
standard way; a later version may be proposed as an MCP extension.

## Concepts

| Term | Meaning |
| --- | --- |
| **Change** | One intended mutation of one target: intent, operation, target, principal, provenance. |
| **Plan** | An adapter-computed effect preview: effects, preconditions, reversibility, facts, a display diff. Content-hashed (`hash`). |
| **Rehearsal** | Executing the plan (and its undo) on a production-like copy, or in a rolled-back transaction on the target (`live-dry-run`). The tier is always reported: `static`, `live-dry-run`, `T0`–`T3`. |
| **Reversibility** | `reversible` (exact undo tested or testable), `compensatable` (semantic undo), `checkpoint-only` (restore a snapshot), `irreversible`. |
| **Decision** | Policy outcome: `approve`, `require` (with approvers, quorum and channel `inline` / `out-of-band`), `deny`. |
| **Approval** | A human decision bound to the exact plan hash. |
| **Receipt** | What happened at apply time, with an opaque adapter **undo handle**. |
| **Verification** | Post-conditions checked against the target, not the agent's claim. |
| **Session** | The ordered changes of one agent run in the direct lane: the unit of timeline, rewind and promotion. |
| **Event** | An append-only, hash-chained audit record of every transition. |

## Lifecycle

```
proposed → planned → rehearsed ─┬─► auto-approved ─────────────────┐
                                ├─► awaiting-approval ─► approved ─┤
                                └─► rejected                       ▼
                                          applying → applied → verifying ─┬─► verified
                                                                          └─► failed ─► undoing → undone
                                (manual: applied/verified → undoing → undone; a refused undo returns to the prior state)
```

`planned → auto-approved | awaiting-approval | rejected` is allowed directly when a plan is `static` (not rehearsed).

## Rules every implementation must follow

1. **Approvals bind to the plan hash.** An approval for plan hash *h* never authorizes a plan with a different hash.
   The hash covers adapter, target, environment, operation, effects, reversibility, destructiveness, blast radius,
   precondition hash and adapter data; it excludes IDs, timestamps and the display diff.
2. **Apply re-checks preconditions.** If the target no longer matches what the plan assumed, apply fails with a
   conflict and nothing is changed ("re-plan required").
3. **Undo is guarded.** An inverse touches state only if it still equals its after-image; otherwise the undo is
   refused with a conflict and nothing is changed. Conflicts go to a human; they are never overwritten.
4. **Environment class comes from the connection,** never from the agent.
5. **Separation of duties.** An agent can never approve its own change. When the decision's channel is
   `out-of-band`, the approver must differ from the delegating human, and in-band channels (MCP elicitation, the
   agent's own UI) are not accepted.
6. **Never hold locks while waiting for a human.** Plans are computed in transactions that are rolled back.
7. **Evidence states its tier.** A `T0` pass is never presented as a `T3` pass.
8. **Row values do not leave the copy** in Lane A reports, and are redacted by sensitivity tags (and, outside
   `personal`, replaced by counts and hashed keys) in Lane B plans.

## Audit chain

Each event carries `prev_hash` and `hash = sha256(prev_hash || canonical_json(event without hash))`. Verifying the
chain detects modified or deleted events (the last event can additionally be anchored externally). The SQLite
store also refuses `UPDATE` and `DELETE` on events.

## Adapter contract

`Describe`, `Classify`, `Plan` are required; `Apply`, `Verify`, `Undo`, `Rehearse`, `Checkpoint`, `Restore`,
`ProvisionCopy` are optional and declared in `AdapterInfo.capabilities`. Adapters run out of process over gRPC
(hashicorp/go-plugin handshake `CHANGEGATE_ADAPTER_PLUGIN`). The conformance suite (`changegate adapter test`)
proves that Plan is side-effect free and hashed, that Apply → Undo restores identical state across a generated
corpus (per operation and as a whole-sequence rewind), that stale plans and stale undos are refused, and that
checkpoints restore. Out-of-process adapters take part through two Plan conventions: operation kind
`changegate.fingerprint` returns `facts.fingerprint`, and `changegate.corpus` (params `seed`, `n`) returns
`facts.corpus`, a JSON list of operations.
