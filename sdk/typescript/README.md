# @changegate/sdk (TypeScript)

Route your agent's changes through a Changegate executor: plan (exact preview), policy, approval, apply, undo.
No runtime dependencies (uses `fetch`).

```ts
import { Client, PendingApproval, guarded, proposeAndApplyText } from "@changegate/sdk";

const cg = new Client({ url: "http://127.0.0.1:8443", token: process.env.CHANGEGATE_EXECUTOR_TOKEN });
const session = await cg.startSession({ target: "app", agent: "refund-bot" });

const change = await session.propose("UPDATE orders SET status = 'refunded' WHERE id = 42", { intent: "refund order 42" });
console.log(change.diff, change.reversibility, change.decision);
try { await change.apply(); } catch (e) { if (e instanceof PendingApproval) console.log("waiting for a human"); }

// Tool body for the OpenAI Agents SDK / Vercel AI SDK / Claude Agent SDK:
const result = await proposeAndApplyText(session, sql, "what the user asked for");
```

`applyWithInterrupt(change, interrupt)` integrates with LangGraph.js interrupts.
