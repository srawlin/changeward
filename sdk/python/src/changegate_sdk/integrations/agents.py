"""OpenAI Agents SDK and Claude Agent SDK helpers.

Both frameworks let a tool return a result string to the model. These helpers
turn a Changegate decision into model-readable text, so the agent learns that
a change is pending approval instead of retrying or bypassing it.
"""

from ..client import Change, Conflict, Denied, PendingApproval, Session


def propose_and_apply_text(session: Session, payload: str, intent: str = "") -> str:
    """Use as the body of a tool (e.g. an `execute_sql` function tool)."""
    change = session.propose(payload, intent=intent)
    try:
        change.apply()
    except PendingApproval:
        return (f"PENDING APPROVAL ({change.id}): this change was not applied. A human must approve plan {change.plan_id}.\n"
                f"{change.diff}\nReasons: {'; '.join(change.reasons)}. Tell the user; do not try another way.")
    except Denied:
        return f"DENIED ({change.id}): {'; '.join(change.reasons)}. Do not retry this change."
    except Conflict as e:
        return f"CONFLICT: the data changed since the plan ({'; '.join(e.conflicts)}). Propose the change again."
    return f"APPLIED {change.id} ({change.reversibility}). Undo with change id {change.id}.\n{change.diff}"


def claude_can_use_tool(session: Session, sql_tools=("execute_sql",)):
    """Claude Agent SDK `can_use_tool` callback: deny raw SQL tools and point Claude at Changegate."""

    async def can_use_tool(tool_name, tool_input, _context):
        if tool_name in sql_tools:
            return {"behavior": "deny", "message": "Use the changegate tools (propose_change, apply_change) for database changes."}
        return {"behavior": "allow", "updatedInput": tool_input}

    return can_use_tool


__all__ = ["propose_and_apply_text", "claude_can_use_tool", "Change"]
