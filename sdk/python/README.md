# changeward-sdk (Python)

Route your agent's changes through a Changeward executor: plan (exact preview), policy, approval, apply, undo.
Standard library only.

```python
from changeward_sdk import Client, PendingApproval, guarded

cw = Client("http://127.0.0.1:8443", token="cwx_...")      # or CHANGEWARD_EXECUTOR_URL / _TOKEN
session = cw.start_session(target="app", agent="refund-bot")

change = session.propose("UPDATE orders SET status = 'refunded' WHERE id = 42", intent="refund order 42")
print(change.diff, change.reversibility, change.decision)
try:
    change.apply()
except PendingApproval as p:
    print("waiting for a human:", p.change.plan_id)

@guarded(lambda: session, intent="refund")
def refund(order_id: int) -> str:
    return f"UPDATE orders SET status = 'refunded' WHERE id = {int(order_id)}"
```

Integrations: `changeward_sdk.integrations.langgraph.apply_with_interrupt` (LangGraph interrupts),
`changeward_sdk.integrations.agents.propose_and_apply_text` (OpenAI Agents SDK / Claude Agent SDK tool bodies),
`claude_can_use_tool` (Claude Agent SDK permission callback).

Run an executor locally with `changeward executor serve` (see `changeward executor token`).
