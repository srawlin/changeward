# agentgateway

agentgateway can call an external authorization service for each MCP request. Point it at the Changegate
executor's `/v1/authz/mcp` endpoint. The request body is the MCP JSON-RPC message. The executor answers 200 to
allow, or 403 to deny. A 403 body is a JSON-RPC error for that call, so the agent sees why it was blocked:

- `initialize`, `tools/list`, other non-call methods, and `GET`/`DELETE` session requests pass through.
- `tools/call` of a known SQL tool (`execute_sql`, `query`, …): read-only SQL passes. Writes are denied with
  instructions to use Changegate's MCP tools (`propose_change` → `apply_change`), so the write gets a preview,
  a policy check and an undo path instead of running directly.
- Tools listed in `x-changegate-read-only-tools` (comma-separated globs) pass and are audited (`gateway.read`).
- Other tool calls are decided by Changegate policy as approval-only changes (`irreversible`, `mcp-tool` kind).
  Every decision is recorded in the audit log (`gateway.*` events).

Verified with agentgateway v1.6.0 (`ghcr.io/agentgateway/agentgateway:v1.6.0`), using the reference "everything"
MCP server as the upstream and the official Python MCP client. `echo`, declared read-only, passed. `get-sum` on a
`prod` upstream was denied, and the client received Changegate's reason.

```yaml
# yaml-language-server: $schema=https://agentgateway.dev/schema/config
mcp:
  port: 3000
  policies:
    extAuthz:
      host: changegate-executor.changegate.svc:8443
      failureMode: deny
      includeRequestBody:
        maxRequestBytes: 1048576
      protocol:
        http:
          path: '"/v1/authz/mcp"'             # CEL expressions: string literals are quoted
          addRequestHeaders:
            x-changegate-token: '"cgx_..."'     # a `changegate executor token gateway --roles propose` token
            x-changegate-server: '"postgres-mcp"'
            x-changegate-env: '"prod"'
            x-changegate-read-only-tools: '"list_*,get_*,describe_*"'
  targets:
  - name: postgres-mcp
    mcp:
      host: http://postgres-mcp.internal:3001/mcp
```

By default, agentgateway's HTTP external authorization forwards only the client's `Authorization` header. A
client therefore cannot set the `x-changegate-*` headers itself; they come from `addRequestHeaders`.

Better still, route agents to Changegate's own MCP server (`changegate mcp postgres --executor ...`) behind the
gateway, and keep the authz hook as the backstop for direct database tools.
