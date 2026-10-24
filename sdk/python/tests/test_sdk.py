"""Integration tests: run against a real `changeward executor serve` with toy targets."""

import hashlib
import json
import os
import socket
import subprocess
import tempfile
import time
import unittest
import urllib.request

from changeward_sdk import Client, Conflict, Denied, PendingApproval, guarded
from changeward_sdk.integrations.agents import propose_and_apply_text
from changeward_sdk.integrations.langgraph import apply_with_interrupt

BIN = os.environ.get("CHANGEWARD_BIN", os.path.join(os.path.dirname(__file__), "..", "..", "..", "bin", "changeward"))
DEV_TOKEN, OPS_TOKEN = "cwx_dev_token_for_tests", "cwx_ops_token_for_tests"


def sha(t):
    return hashlib.sha256(t.encode()).hexdigest()


class ExecutorTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.dir = tempfile.mkdtemp()
        s = socket.socket()
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
        s.close()
        cfg = {
            "targets": {
                "dev": {"adapter": "toykv", "env": "personal", "options": {"path": os.path.join(cls.dir, "dev.json")}},
                "shared": {"adapter": "toykv", "env": "shared-dev", "options": {"path": os.path.join(cls.dir, "shared.json")}},
                "prod": {"adapter": "toykv", "env": "prod", "options": {"path": os.path.join(cls.dir, "prod.json")}},
            },
            "executor": {"tokens": [{"name": "dev", "sha256": sha(DEV_TOKEN), "roles": ["propose"]},
                                    {"name": "ops", "sha256": sha(OPS_TOKEN), "roles": ["propose", "approve"]}]},
        }
        path = os.path.join(cls.dir, "changeward.yaml")
        with open(path, "w") as f:
            json.dump(cfg, f)  # JSON is valid YAML
        env = dict(os.environ, CHANGEWARD_CONFIG=path)
        cls.proc = subprocess.Popen([BIN, "executor", "serve", "--addr", f"127.0.0.1:{port}"], env=env, stderr=subprocess.DEVNULL)
        cls.url = f"http://127.0.0.1:{port}"
        for _ in range(100):
            try:
                urllib.request.urlopen(cls.url + "/healthz", timeout=1)
                break
            except OSError:
                time.sleep(0.1)
        cls.dev = Client(cls.url, DEV_TOKEN)
        cls.ops = Client(cls.url, OPS_TOKEN)

    @classmethod
    def tearDownClass(cls):
        cls.proc.terminate()
        cls.proc.wait(5)

    def test_lifecycle(self):
        s = self.dev.start_session("dev", agent="py-test")
        c = s.propose("set greeting hello", intent="say hello")
        self.assertEqual(c.decision, "approve")
        self.assertEqual(c.reversibility, "reversible")
        c.apply()
        self.assertEqual(c.state, "verified")
        self.assertEqual(len(s.changes()), 1)
        undone = c.undo()
        self.assertEqual([u.state for u in undone], ["undone"])

    def test_pending_and_out_of_band(self):
        s = self.dev.start_session("prod", agent="py-test", incident_ref="INC-1")
        c = s.propose("set flag on")
        self.assertTrue(c.needs_approval)
        self.assertFalse(c.inline)
        with self.assertRaises(PendingApproval):
            c.apply()
        # The proposer cannot confirm inline; someone else approves out of band.
        with self.assertRaises(Exception):
            c.confirm(True)
        self.ops._call("POST", f"/v1/changes/{c.id}/approve", {"plan_hash": c.data["plan"]["hash"], "approved": True})
        self.assertEqual(c.apply().state, "verified")

    def test_denied(self):
        s = self.dev.start_session("prod", agent="py-test")  # no incident reference
        with self.assertRaises(Denied):
            s.propose("set x 1").apply()

    def test_guarded_and_conflict(self):
        s = self.dev.start_session("dev", agent="py-test")

        @guarded(lambda: s, intent="set a key")
        def put(k, v):
            return f"set {k} {v}"

        c = put("k1", "v1")
        self.assertEqual(c.state, "verified")
        stale = s.propose("set k1 v2")
        put("k1", "v3")
        with self.assertRaises(Conflict):
            stale.apply()

    def test_integrations(self):
        s = self.dev.start_session("dev", agent="py-test")
        self.assertIn("APPLIED", propose_and_apply_text(s, "set t 1"))
        ps = self.dev.start_session("prod", agent="py-test", incident_ref="INC-2")
        self.assertIn("PENDING APPROVAL", propose_and_apply_text(ps, "set t 1"))
        # LangGraph: an inline decision (irreversible in personal) is confirmed through interrupt().
        sh = self.dev.start_session("shared", agent="py-test")
        big = sh.propose("del missing")  # destructive in shared-dev → confirmation required
        seen = []

        def fake_interrupt(payload):
            seen.append(payload)
            return True

        self.assertTrue(big.needs_approval, big.data.get("decision"))
        apply_with_interrupt(big, interrupt=fake_interrupt)
        self.assertEqual(len(seen), 1)
        self.assertTrue(seen[0]["changeward"]["inline"])
        self.assertEqual(big.state, "verified")


if __name__ == "__main__":
    unittest.main()
