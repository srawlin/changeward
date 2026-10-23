---
description: Show this session's database changes (Changeward timeline)
allowed-tools: Bash(changeward log:*), Bash(npx -y @changeward/cli log:*)
---
Run `changeward log` (or `npx -y @changeward/cli log` if `changeward` is not on PATH) and summarize the session's
changes for me: what each change did, how many rows it touched, its reversibility, and its state. Point out anything
that is not reversible.
