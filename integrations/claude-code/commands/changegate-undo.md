---
description: Undo a database change made in this session
argument-hint: <step number or change id>
---
Use the `undo_change` tool of the changegate MCP server to undo change $ARGUMENTS. If it reports that later changes
depend on it, show me the list and ask before calling again with `with_dependents: true`. If the undo is refused
because the rows changed since, explain the conflict; do not try to force it.
