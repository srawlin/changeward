---
description: Show this session's database changes (Changegate timeline)
allowed-tools: Bash(changegate log:*), Bash(npx -y @changegate/cli log:*)
---
Run `changegate log` (or `npx -y @changegate/cli log` if `changegate` is not on PATH) and summarize the session's
changes for me: what each change did, how many rows it touched, its reversibility, and its state. Point out anything
that is not reversible.
