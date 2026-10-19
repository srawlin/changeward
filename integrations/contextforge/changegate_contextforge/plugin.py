"""ContextForge plugin: ask the Changegate executor before every tool call.

Reads pass; SQL writes to database tools are blocked (use Changegate's own tools for a preview and undo);
other tool calls are decided by Changegate policy as approval-only changes and audited.
"""

import json
import os
import urllib.request

from mcpgateway.plugins.framework import (  # provided by ContextForge
    Plugin,
    PluginConfig,
    PluginContext,
    PluginViolation,
    ToolPreInvokePayload,
    ToolPreInvokeResult,
)


def decide(executor_url, token, server, env, agent, tool, arguments, timeout=5.0):
    body = json.dumps({"server": server, "env": env, "agent": agent, "tool": tool, "arguments": arguments or {}}).encode()
    req = urllib.request.Request(executor_url.rstrip("/") + "/v1/authz/tool", data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310 (configured executor URL)
        return json.loads(resp.read())


class ChangegatePlugin(Plugin):
    def __init__(self, config: PluginConfig):
        super().__init__(config)
        c = config.config or {}
        self.executor_url = c.get("executor_url", "http://127.0.0.1:8443")
        self.token = os.environ.get(c.get("token_env", "CHANGEGATE_GATEWAY_TOKEN"), "")
        self.env = c.get("env", "prod")
        self.fail_closed = c.get("fail_closed", True)

    async def tool_pre_invoke(self, payload: ToolPreInvokePayload, context: PluginContext) -> ToolPreInvokeResult:
        server = getattr(getattr(context, "global_context", None), "server_id", None) or "mcp"
        agent = getattr(getattr(context, "global_context", None), "user", None) or "via-contextforge"
        try:
            d = decide(self.executor_url, self.token, str(server), self.env, str(agent), payload.name, payload.args)
        except Exception as e:  # noqa: BLE001
            if not self.fail_closed:
                return ToolPreInvokeResult(continue_processing=True)
            d = {"allow": False, "reason": f"Changegate unavailable: {e}"}
        if d.get("allow"):
            return ToolPreInvokeResult(continue_processing=True)
        return ToolPreInvokeResult(
            continue_processing=False,
            violation=PluginViolation(reason="Blocked by Changegate", description=d.get("reason", ""), code="CHANGEGATE_POLICY",
                                      details={"tool": payload.name, "change_id": d.get("change_id", "")}),
        )
