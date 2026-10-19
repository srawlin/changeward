"""Changegate SDK.

Route an agent's changes through Changegate: every change is planned (an exact
preview), decided by policy, applied by an executor that holds the
credentials, logged, and undoable.

    from changegate_sdk import Client

    cg = Client("http://127.0.0.1:8443", token=os.environ["CHANGEGATE_EXECUTOR_TOKEN"])
    session = cg.start_session(target="app", agent="support-bot")
    change = session.propose("UPDATE orders SET status = 'refunded' WHERE id = 42", intent="refund order 42")
    change.apply()          # raises PendingApproval if policy needs a human
    change.undo()
"""

from .client import (
    Change,
    ChangegateError,
    Client,
    Conflict,
    Denied,
    PendingApproval,
    Session,
    guarded,
)

__all__ = ["Client", "Session", "Change", "ChangegateError", "Conflict", "Denied", "PendingApproval", "guarded"]
__version__ = "0.1.0"
