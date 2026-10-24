"""HTTP client for the Changeward executor API (stdlib only)."""

from __future__ import annotations

import functools
import json
import os
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Callable, Optional


class ChangewardError(Exception):
    """An error returned by the executor."""

    def __init__(self, message: str, status: int = 0, body: Optional[dict] = None):
        super().__init__(message)
        self.status = status
        self.body = body or {}


class Conflict(ChangewardError):
    """The target changed since the plan (or since apply, for undo). Nothing was changed."""

    @property
    def conflicts(self) -> list:
        return self.body.get("conflicts") or []


class PendingApproval(ChangewardError):
    """Policy requires a human decision. The change was not applied; poll or retry later."""

    def __init__(self, change: "Change"):
        super().__init__(f"change {change.id} needs approval: {'; '.join(change.reasons)}")
        self.change = change


class Denied(ChangewardError):
    """Policy denied the change."""

    def __init__(self, change: "Change"):
        super().__init__(f"change {change.id} was denied: {'; '.join(change.reasons)}")
        self.change = change


class Client:
    """Talks to `changeward executor serve`."""

    def __init__(self, url: Optional[str] = None, token: Optional[str] = None, timeout: float = 120.0):
        self.url = (url or os.environ.get("CHANGEWARD_EXECUTOR_URL", "http://127.0.0.1:8443")).rstrip("/")
        self.token = token or os.environ.get("CHANGEWARD_EXECUTOR_TOKEN", "")
        self.timeout = timeout

    def _call(self, method: str, path: str, body: Any = None) -> Any:
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.url + path, data=data, method=method)
        req.add_header("Authorization", "Bearer " + self.token)
        req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:  # noqa: S310 (configured executor URL)
                raw = resp.read()
                return json.loads(raw) if raw else None
        except urllib.error.HTTPError as e:
            raw = e.read()
            e.close()
            try:
                payload = json.loads(raw)
            except ValueError:
                payload = {"error": raw.decode(errors="replace")}
            cls = Conflict if e.code == 409 else ChangewardError
            raise cls(payload.get("error") or payload.get("reason") or str(e), e.code, payload) from None

    def start_session(self, target: str, agent: str = "sdk", incident_ref: str = "", checkpoint: bool = False) -> "Session":
        s = self._call("POST", "/v1/sessions", {"target": target, "agent": agent, "incident_ref": incident_ref, "checkpoint": checkpoint})
        return Session(self, s)

    def session(self, session_id: str) -> "Session":
        return Session(self, self._call("GET", "/v1/sessions/" + urllib.parse.quote(session_id)))

    def change(self, ref: str) -> "Change":
        return Change(self, self._call("GET", "/v1/changes/" + urllib.parse.quote(ref)))


class Session:
    """A sequence of changes by one agent run (the unit of timeline and undo)."""

    def __init__(self, client: Client, data: dict):
        self.client = client
        self.data = data
        self.id = data["id"]

    def propose(self, payload: str, intent: str = "", kind: str = "") -> "Change":
        """Plan a change (dry run; nothing is modified) and get the policy decision."""
        c = self.client._call("POST", "/v1/changes", {"session_id": self.id, "payload": payload, "intent": intent, "kind": kind, "host": "sdk:python"})
        return Change(self.client, c)

    def query(self, sql: str, max_rows: int = 100) -> dict:
        return self.client._call("POST", f"/v1/sessions/{urllib.parse.quote(self.id)}/query", {"query": sql, "max_rows": max_rows})

    def changes(self) -> list:
        return [Change(self.client, c) for c in self.client._call("GET", f"/v1/sessions/{urllib.parse.quote(self.id)}/changes") or []]

    def checkpoint(self, label: str = "") -> str:
        return self.client._call("POST", f"/v1/sessions/{urllib.parse.quote(self.id)}/checkpoint", {"label": label})["checkpoint_id"]

    def end(self) -> None:
        self.client._call("POST", f"/v1/sessions/{urllib.parse.quote(self.id)}/end")


class Change:
    """A Change Record: plan, decision, state, receipt."""

    def __init__(self, client: Client, data: dict):
        self.client = client
        self.data = data

    @property
    def id(self) -> str:
        return self.data["id"]

    @property
    def state(self) -> str:
        return self.data["state"]

    @property
    def plan_id(self) -> str:
        return (self.data.get("plan") or {}).get("id", "")

    @property
    def diff(self) -> str:
        return (self.data.get("plan") or {}).get("diff", "")

    @property
    def reversibility(self) -> str:
        return (self.data.get("plan") or {}).get("reversibility", "")

    @property
    def decision(self) -> str:
        return (self.data.get("decision") or {}).get("decision", "")

    @property
    def reasons(self) -> list:
        return (self.data.get("decision") or {}).get("reasons") or []

    @property
    def needs_approval(self) -> bool:
        return self.state == "awaiting-approval"

    @property
    def inline(self) -> bool:
        """True when the delegating human may confirm in the agent's own UI."""
        return (self.data.get("decision") or {}).get("channel") == "inline"

    def refresh(self) -> "Change":
        self.data = self.client._call("GET", "/v1/changes/" + urllib.parse.quote(self.id))
        return self

    def apply(self) -> "Change":
        """Apply if approved. Raises PendingApproval, Denied or Conflict otherwise."""
        self.refresh()
        if self.state == "rejected":
            raise Denied(self)
        if self.state == "awaiting-approval":
            raise PendingApproval(self)
        self.data = self.client._call("POST", f"/v1/changes/{urllib.parse.quote(self.id)}/apply")
        return self

    def confirm(self, approved: bool = True, channel: str = "sdk") -> "Change":
        """Record the delegating human's inline confirmation (only where policy allows inline)."""
        self.data = self.client._call("POST", f"/v1/changes/{urllib.parse.quote(self.id)}/approve-inline",
                                      {"plan_hash": self.data["plan"]["hash"], "approved": approved, "channel": channel})
        return self

    def undo(self, with_dependents: bool = False) -> list:
        res = self.client._call("POST", f"/v1/changes/{urllib.parse.quote(self.id)}/undo", {"with_dependents": with_dependents})
        if res and res.get("dependents"):
            raise ChangewardError("later changes depend on this one: " + ", ".join(d["id"] for d in res["dependents"]), 0, res)
        return [Change(self.client, c) for c in (res or {}).get("undone") or []]


def guarded(session_getter: Callable[[], Session], intent: str = "", apply: bool = True):
    """Decorator: the wrapped function returns the operation (e.g. SQL); Changeward plans it
    and, when policy auto-approves, applies it. Otherwise PendingApproval/Denied is raised and
    nothing ran. The decorated function returns the Change.

        @guarded(lambda: session, intent="refund")
        def refund(order_id):
            return f"UPDATE orders SET status = 'refunded' WHERE id = {int(order_id)}"
    """

    def wrap(fn):
        @functools.wraps(fn)
        def inner(*args, **kwargs):
            payload = fn(*args, **kwargs)
            change = session_getter().propose(payload, intent=intent or fn.__name__)
            if apply:
                change.apply()
            return change

        return inner

    return wrap
