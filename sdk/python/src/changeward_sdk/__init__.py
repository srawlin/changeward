"""Changeward SDK.

Route an agent's changes through Changeward: every change is planned (an exact
preview), decided by policy, applied by an executor that holds the
credentials, logged, and undoable.

    from changeward_sdk import Client

    cw = Client("http://127.0.0.1:8443", token=os.environ["CHANGEWARD_EXECUTOR_TOKEN"])
    session = cw.start_session(target="app", agent="support-bot")
    change = session.propose("UPDATE orders SET status = 'refunded' WHERE id = 42", intent="refund order 42")
    change.apply()          # raises PendingApproval if policy needs a human
    change.undo()
"""

from .client import (
    Change,
    ChangewardError,
    Client,
    Conflict,
    Denied,
    PendingApproval,
    Session,
    guarded,
)

__all__ = ["Client", "Session", "Change", "ChangewardError", "Conflict", "Denied", "PendingApproval", "guarded"]
__version__ = "0.1.0"
