import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { createServer } from "node:net";
import { Client, Conflict, Denied, PendingApproval, guarded, proposeAndApplyText, applyWithInterrupt } from "../src/index.ts";

const here = dirname(fileURLToPath(import.meta.url));
const BIN = process.env.CHANGEGATE_BIN ?? join(here, "..", "..", "..", "bin", "changegate");
const DEV = "cgx_ts_dev_token", OPS = "cgx_ts_ops_token";
const sha = (t: string) => createHash("sha256").update(t).digest("hex");
let proc: ChildProcess;
let url = "";

before(async () => {
  const dir = mkdtempSync(join(tmpdir(), "cg-ts-"));
  const port: number = await new Promise((r) => { const s = createServer(); s.listen(0, "127.0.0.1", () => { const p = (s.address() as { port: number }).port; s.close(() => r(p)); }); });
  const cfg = {
    targets: {
      dev: { adapter: "toykv", env: "personal", options: { path: join(dir, "dev.json") } },
      shared: { adapter: "toykv", env: "shared-dev", options: { path: join(dir, "shared.json") } },
      prod: { adapter: "toykv", env: "prod", options: { path: join(dir, "prod.json") } },
    },
    executor: { tokens: [{ name: "dev", sha256: sha(DEV), roles: ["propose"] }, { name: "ops", sha256: sha(OPS), roles: ["propose", "approve"] }] },
  };
  writeFileSync(join(dir, "changegate.yaml"), JSON.stringify(cfg));
  proc = spawn(BIN, ["executor", "serve", "--addr", `127.0.0.1:${port}`], { env: { ...process.env, CHANGEGATE_CONFIG: join(dir, "changegate.yaml") }, stdio: "ignore" });
  url = `http://127.0.0.1:${port}`;
  for (let i = 0; i < 100; i++) {
    try { if ((await fetch(url + "/healthz")).ok) break; } catch { /* starting */ }
    await new Promise((r) => setTimeout(r, 100));
  }
});
after(() => { proc.kill(); });

test("lifecycle", async () => {
  const s = await new Client({ url, token: DEV }).startSession({ target: "dev", agent: "ts-test" });
  const c = await s.propose("set greeting hello", { intent: "say hello" });
  assert.equal(c.decision, "approve");
  await c.apply();
  assert.equal(c.state, "verified");
  const undone = await c.undo();
  assert.deepEqual(undone.map((u) => u.state), ["undone"]);
});

test("pending, denied, out-of-band approval", async () => {
  const dev = new Client({ url, token: DEV }), ops = new Client({ url, token: OPS });
  const s = await dev.startSession({ target: "prod", agent: "ts-test", incidentRef: "INC-1" });
  const c = await s.propose("set flag on");
  await assert.rejects(c.apply(), PendingApproval);
  await ops.call("POST", `/v1/changes/${c.id}/approve`, { plan_hash: c.record.plan!.hash, approved: true });
  assert.equal((await c.apply()).state, "verified");
  const noIncident = await dev.startSession({ target: "prod", agent: "ts-test" });
  await assert.rejects((await noIncident.propose("set x 1")).apply(), Denied);
});

test("guarded, conflict, helpers", async () => {
  const dev = new Client({ url, token: DEV });
  const s = await dev.startSession({ target: "dev", agent: "ts-test" });
  const put = guarded(() => s, (k: string, v: string) => `set ${k} ${v}`, { intent: "set key" });
  assert.equal((await put("k1", "v1")).state, "verified");
  const stale = await s.propose("set k1 v2");
  await put("k1", "v3");
  await assert.rejects(stale.apply(), Conflict);
  assert.match(await proposeAndApplyText(s, "set t 1"), /^APPLIED/);
  const sh = await dev.startSession({ target: "shared", agent: "ts-test" });
  const big = await sh.propose("del missing");
  assert.ok(big.needsApproval && big.inline);
  const seen: unknown[] = [];
  await applyWithInterrupt(big, (v) => { seen.push(v); return true; });
  assert.equal(seen.length, 1);
  assert.equal(big.state, "verified");
});
