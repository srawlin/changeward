# What would have saved the Replit database

In July 2025 an AI coding agent, during an explicit code freeze, ran destructive commands against a production
database and deleted records for 1,206 executives and 1,196 companies, then misreported what had happened. This
demo replays that kind of action through Changegate:

1. **Production, during the freeze:** `DELETE FROM executives` is planned in a rolled-back transaction. Changegate
   shows exactly which rows would go (emails redacted) and policy denies it. Nothing runs.
2. **Production with an incident reference:** a destructive change needs out-of-band approval from someone other
   than the proposer. The agent cannot approve itself.
3. **A personal dev copy:** the agent moves fast. Every change is auto-approved because it is reversible:
   deleted rows are captured as before-images, and `DROP TABLE` becomes a soft-drop.
4. **`changegate rewind`** undoes the whole session in one command.
5. **The audit log** is hash-chained, so the agent's account of events can be checked against it.

```
cd demo/replit
CG=../../bin/changegate ./run.sh
```

Requires Docker. The same flow works through the MCP server (`changegate mcp postgres --target dev`) that coding
agents call directly.
