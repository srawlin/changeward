# Your own agents: Python and TypeScript SDKs

For agents you build yourself (support bots, ops agents, LangGraph graphs), route changes through a
[shared executor](../executor.md). The agent proposes; the executor plans, decides, applies and keeps the undo
data. The agent never holds the database credentials.

```sh
changegate executor token refund-bot --roles propose     # prints the token once, plus the config entry
changegate executor serve --addr 127.0.0.1:8443          # or deploy with the Helm chart
```

Both SDKs read `CHANGEGATE_EXECUTOR_URL` and `CHANGEGATE_EXECUTOR_TOKEN` when no URL or token is passed.

## Python (`sdk/python`, standard library only)

```python
from changegate_sdk import Client, PendingApproval, Denied, Conflict

cg = Client("http://127.0.0.1:8443", token="cgx_...")
session = cg.start_session(target="app", agent="refund-bot", incident_ref="TICKET-881")

change = session.propose("UPDATE orders SET status = 'refunded' WHERE id = 42", intent="refund order 42")
print(change.diff, change.reversibility, change.decision)   # exact preview before anything happens

try:
    change.apply()                       # applies if policy approved
except PendingApproval:
    print("a human must approve", change.plan_id)   # changegate approve <plan>, Slack, or the web view
except Denied as e:
    print("denied:", change.reasons)

change.undo()                            # dependency-aware; refuses on conflicting edits
session.end()
```

| Integration | Helper |
| --- | --- |
| **LangGraph** | `changegate_sdk.integrations.langgraph.apply_with_interrupt(change, interrupt)` pauses the graph with the plan as the interrupt payload. When you resume with approval, the change is confirmed and applied. |
| **OpenAI Agents SDK**, **Claude Agent SDK** tool bodies | `changegate_sdk.integrations.agents.propose_and_apply_text(session, sql, intent)` returns model-readable text: `APPLIED …`, `PENDING APPROVAL … do not try another way`, `DENIED … do not retry`, or `CONFLICT … propose again`. |
| **Claude Agent SDK** permissions | `claude_can_use_tool(session, sql_tools=("execute_sql",))` is a `can_use_tool` callback that denies raw SQL tools and points Claude at Changegate. |
| Any function | `@guarded(lambda: session, intent="refund")` on a function that returns SQL: the SQL is proposed and applied through Changegate. |

Example tool for the OpenAI Agents SDK:

```python
from agents import function_tool
from changegate_sdk.integrations.agents import propose_and_apply_text

@function_tool
def execute_sql(sql: str, why: str) -> str:
    """Change the orders database. Every change is previewed, policy-checked and undoable."""
    return propose_and_apply_text(session, sql, intent=why)
```

## TypeScript (`sdk/typescript`, no dependencies, uses `fetch`)

```ts
import { Client, PendingApproval, proposeAndApplyText, applyWithInterrupt } from "@changegate/sdk";

const cg = new Client({ url: "http://127.0.0.1:8443", token: process.env.CHANGEGATE_EXECUTOR_TOKEN });
const session = await cg.startSession({ target: "app", agent: "refund-bot" });

const change = await session.propose("UPDATE orders SET status = 'refunded' WHERE id = 42", { intent: "refund order 42" });
console.log(change.diff, change.reversibility, change.decision);
try { await change.apply(); } catch (e) { if (e instanceof PendingApproval) console.log("waiting for a human"); }
```

- **Vercel AI SDK, OpenAI Agents SDK (JS), Claude Agent SDK (TS):** use
  `proposeAndApplyText(session, sql, intent)` as the tool's `execute`.
- **LangGraph.js:** `applyWithInterrupt(change, interrupt)`.

## Choosing the identity

The token's name is the delegating human or service, recorded as `user:<name>`. `agent` is the agent's
identity. Policy can tell them apart (`principal.kind == "agent"`), and an approval must come from a token
other than the proposer's, with the `approve` role.
