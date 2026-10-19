// Changegate plugin for OpenCode (written by `changegate install opencode`).
// Shell commands that would change a registered target directly (psql writes,
// kubectl apply, terraform apply, ...) are blocked with instructions to use
// the changegate MCP tools, which preview, policy-check and record an undo.
import { spawnSync } from "node:child_process"

const CHANGEGATE = process.env.CHANGEGATE_BIN || "__CHANGEGATE_BIN__"

export const Changegate = async ({ directory }) => {
  return {
    "tool.execute.before": async (input, output) => {
      if (input.tool !== "bash") return
      const command = output?.args?.command
      if (typeof command !== "string" || command === "") return
      const r = spawnSync(CHANGEGATE, ["hook", "check", "--json", command], {
        cwd: directory,
        encoding: "utf8",
        env: { ...process.env, CHANGEGATE_AGENT: "opencode" },
        timeout: 30000,
      })
      if (r.error) return // never break the agent if changegate is missing
      let d = { permission: "allow", reason: "" }
      try {
        d = JSON.parse(r.stdout)
      } catch {
        return
      }
      if (d.permission === "deny") throw new Error("Changegate: " + d.reason)
      if (d.permission === "ask") throw new Error("Changegate: " + d.reason + " Ask the user to run it themselves.")
    },
  }
}
