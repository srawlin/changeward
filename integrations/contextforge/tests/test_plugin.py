"""Tests the plugin against a live executor with stubbed ContextForge classes."""

import asyncio
import hashlib
import json
import os
import socket
import subprocess
import sys
import tempfile
import time
import types
import unittest
import urllib.request

# Stub the ContextForge framework (the real one is provided by the gateway).
fw = types.ModuleType("mcpgateway.plugins.framework")


class _Base:
    def __init__(self, config):
        self.config = config


class _Obj:
    def __init__(self, **kw):
        self.__dict__.update(kw)


fw.Plugin, fw.PluginConfig, fw.PluginContext = _Base, _Obj, _Obj
fw.PluginViolation = fw.ToolPreInvokePayload = fw.ToolPreInvokeResult = _Obj
for name in ("mcpgateway", "mcpgateway.plugins"):
    sys.modules[name] = types.ModuleType(name)
sys.modules["mcpgateway.plugins.framework"] = fw
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from changegate_contextforge.plugin import ChangegatePlugin  # noqa: E402

BIN = os.environ.get("CHANGEGATE_BIN", os.path.join(os.path.dirname(__file__), "..", "..", "..", "bin", "changegate"))
TOKEN = "cgx_contextforge_test"


class PluginTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        d = tempfile.mkdtemp()
        s = socket.socket()
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
        s.close()
        cfg = {"targets": {}, "executor": {"tokens": [{"name": "gateway", "sha256": hashlib.sha256(TOKEN.encode()).hexdigest(), "roles": ["propose"]}]}}
        p = os.path.join(d, "changegate.yaml")
        with open(p, "w") as f:
            json.dump(cfg, f)
        cls.proc = subprocess.Popen([BIN, "executor", "serve", "--addr", f"127.0.0.1:{port}"], env=dict(os.environ, CHANGEGATE_CONFIG=p), stderr=subprocess.DEVNULL)
        cls.url = f"http://127.0.0.1:{port}"
        for _ in range(100):
            try:
                urllib.request.urlopen(cls.url + "/healthz", timeout=1)
                break
            except OSError:
                time.sleep(0.1)
        os.environ["CHANGEGATE_GATEWAY_TOKEN"] = TOKEN

    @classmethod
    def tearDownClass(cls):
        cls.proc.terminate()
        cls.proc.wait(5)

    def run_tool(self, name, args):
        plugin = ChangegatePlugin(_Obj(config={"executor_url": self.url, "env": "prod"}))
        ctx = _Obj(global_context=_Obj(server_id="pg", user="agent-1"))
        return asyncio.run(plugin.tool_pre_invoke(_Obj(name=name, args=args), ctx))

    def test_decisions(self):
        self.assertTrue(self.run_tool("execute_sql", {"sql": "SELECT 1"}).continue_processing)
        r = self.run_tool("execute_sql", {"sql": "DROP TABLE users"})
        self.assertFalse(r.continue_processing)
        self.assertIn("Changegate", r.violation.description)
        self.assertFalse(self.run_tool("send_email", {"to": "ceo"}).continue_processing)


if __name__ == "__main__":
    unittest.main()
