// Changeward plugin for OpenCode (written by `changeward install opencode`).
// Shell commands that would change a registered target directly (psql writes,
// kubectl apply, terraform apply, ...) are blocked with instructions to use
// the changeward MCP tools, which preview, policy-check and record an undo.
import { spawnSync } from "node:child_process"

const CHANGEWARD = process.env.CHANGEWARD_BIN || "__CHANGEWARD_BIN__"

export const Changeward = async ({ directory }) => {
  return {
    "tool.execute.before": async (input, output) => {
      if (input.tool !== "bash") return
      const command = output?.args?.command
      if (typeof command !== "string" || command === "") return
      const r = spawnSync(CHANGEWARD, ["hook", "check", "--json", command], {
        cwd: directory,
        encoding: "utf8",
        env: { ...process.env, CHANGEWARD_AGENT: "opencode" },
        timeout: 30000,
      })
      if (r.error) return // never break the agent if changeward is missing
      let d = { permission: "allow", reason: "" }
      try {
        d = JSON.parse(r.stdout)
      } catch {
        return
      }
      if (d.permission === "deny") throw new Error("Changeward: " + d.reason)
      if (d.permission === "ask") throw new Error("Changeward: " + d.reason + " Ask the user to run it themselves.")
    },
  }
}
