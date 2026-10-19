# agentgateway

agentgateway can call an external authorization service for each MCP request. Point it at the Changegate
executor's `/v1/authz/mcp` endpoint. The request body is the MCP JSON-RPC message; respond 200 to allow, 403 to deny:

- `tools/list` and other non-call methods pass through.
- `tools/call` of a known SQL tool (`execute_sql`, `query`, …): read-only SQL passes. Writes are denied with
  instructions to use Changegate's MCP tools (`propose_change` → `apply_change`), so the write gets a preview,
  a policy check and an undo path instead of running directly.
- Other tool calls are decided by Changegate policy as approval-only changes (`irreversible`, `mcp-tool` kind).
  The decision is recorded in the audit log (`gateway.*` events).

Send Changegate's token in `x-changegate-token`, plus `x-changegate-server` (upstream name), `x-changegate-env`
(environment class) and optionally `x-changegate-agent`. A sketch of the policy (check the field names against
your agentgateway version's external-authorization documentation):

```yaml
policies:
  extAuthz:
    host: changegate-executor.changegate.svc:8443
    protocol: http
    path: /v1/authz/mcp
    includeRequestBody: true
    failOpen: false
    headers:
      x-changegate-token: ${CHANGEGATE_GATEWAY_TOKEN}
      x-changegate-server: postgres-mcp
      x-changegate-env: prod
```

Better still, route agents to Changegate's own MCP server (`changegate mcp postgres --executor ...`) behind the
gateway, and keep the authz hook as the backstop for direct database tools.
