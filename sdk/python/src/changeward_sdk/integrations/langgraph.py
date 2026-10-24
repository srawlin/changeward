"""LangGraph: turn Changeward approvals into graph interrupts.

    from changeward_sdk.integrations.langgraph import apply_with_interrupt

    def refund_node(state):
        change = session.propose(sql, intent="refund order")
        apply_with_interrupt(change)     # pauses the graph for a human when policy requires it
"""

from ..client import Change, Denied


def apply_with_interrupt(change: Change, interrupt=None) -> Change:
    """Apply, or pause the graph with `interrupt()` when approval is needed.

    For inline decisions the resume value is the human's answer (True/False) and is recorded as
    their confirmation. For out-of-band decisions the graph resumes once approval was granted
    elsewhere (Slack, web, `changeward approve`); the change is then applied.
    """
    if interrupt is None:
        from langgraph.types import interrupt  # noqa: PLC0415 (optional dependency)
    change.refresh()
    if change.state == "rejected":
        raise Denied(change)
    if change.needs_approval:
        answer = interrupt({"changeward": {"change_id": change.id, "plan_id": change.plan_id, "diff": change.diff,
                                          "reasons": change.reasons, "inline": change.inline}})
        if change.inline:
            change.confirm(bool(answer), channel="langgraph-interrupt")
            if not answer:
                raise Denied(change)
    return change.apply()
